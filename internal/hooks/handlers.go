package hooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/render"
	"github.com/IZZY-Agency/claude-tg/internal/send"
	"github.com/IZZY-Agency/claude-tg/internal/session"
	"github.com/IZZY-Agency/claude-tg/internal/state"
)

// Deps is everything a handler needs, injectable for tests.
type Deps struct {
	Sender  *send.Sender
	Store   *state.Store
	Resolve func(context.Context, session.Env) (*state.Session, error)
	Away    func(context.Context) bool
	Now     func() time.Time
	Log     func(string, ...any)
	Env     session.Env // agterm ids and pid from the hook's environment; SessionID/Cwd come from the payload

	// The waiting hook's dependencies (permission-wait); all optional.
	Edit       func(ctx context.Context, messageID int64, html string) error   // EditMessage with nil keyboard; nil = no edits
	RelayUp    func() bool                                                     // a relay that routes ask taps runs; nil = Store.RelayServes(state.FeatureAsks)
	Unattended func() bool                                                     // the environment marks a non-interactive run (claude -p); nil = no
	Attended   func(ctx context.Context, sessionID string) bool                // the `claude agents` check, run after the ask is put; nil = attended
	Alive      func(pid int) bool                                              // pid 0 → true; nil = alive
	Watch      func(path, tool string, input json.RawMessage) (Watcher, error) // nil = transcript.NewWatch
	Sleep      func(ctx context.Context, d time.Duration) bool                 // false when ctx ended
	Screen     func(ctx context.Context) (string, error)                       // the session's own terminal pane; nil = none readable: no buttons
	AwayIn     func(ctx context.Context) time.Duration                         // how long until the owner counts as away, 0 = away now; nil = Away, re-checked every AwayTick

	Tick, AwayTick, Deadline time.Duration // defaults 1s, 5s, 12h
}

const (
	pendingTTL   = 60 * time.Second
	dedupeWindow = 10 * time.Minute
	summaryMax   = 120
)

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) logf(format string, args ...any) {
	if d.Log != nil {
		d.Log(format, args...)
	}
}

// alive reports whether pid runs; pid 0 (unknown) counts as alive.
func (d Deps) alive(pid int) bool {
	return pid == 0 || d.Alive == nil || d.Alive(pid)
}

func (d Deps) resolve(ctx context.Context, in *Input) (*state.Session, error) {
	env := d.Env
	env.SessionID = in.SessionID
	env.Cwd = in.Cwd
	env.Own = true // hooks run inside the session's own process environment
	return d.Resolve(ctx, env)
}

// Handle runs one hook kind. Errors are for the log only; the CLI exits 0 regardless.
func Handle(ctx context.Context, kind string, in *Input, d Deps) error {
	switch kind {
	case "stop":
		return handleStop(ctx, in, d)
	case "permission-request":
		return handlePermissionRequest(ctx, in, d)
	case "notification":
		return handleNotification(ctx, in, d)
	case "stop-failure":
		return handleStopFailure(ctx, in, d)
	case "session-end":
		return handleSessionEnd(ctx, in, d)
	}
	return fmt.Errorf("hook: unknown kind %q", kind)
}

func handleStop(ctx context.Context, in *Input, d Deps) error {
	sess, err := d.resolve(ctx, in)
	if err != nil {
		return err
	}
	if in.StopHookActive || strings.TrimSpace(in.LastAssistantMessage) == "" {
		return nil
	}
	if !sess.Follow {
		owed := false
		_, _ = d.Store.UpdateSession(sess.SessionID, func(s *state.Session) { owed = s.TakeAnswer() })
		if !owed {
			return nil
		}
	}
	_, err = d.Sender.Send(ctx, send.Message{SessionID: sess.SessionID, Label: sess.Name, Body: in.LastAssistantMessage, Silent: true, Markdown: true, Kind: send.KindFeed})
	return err
}

// Summary reduces a tool input to one line a phone can show.
func Summary(toolName string, toolInput json.RawMessage) string {
	var m map[string]any
	_ = json.Unmarshal(toolInput, &m)
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	var s string
	switch toolName {
	case "Bash":
		s = pick("command")
	case "Edit", "Write", "Read", "MultiEdit", "NotebookEdit":
		s = pick("file_path", "notebook_path")
	default:
		s = strings.TrimSpace(string(toolInput))
	}
	s = strings.Join(strings.Fields(s), " ")
	s = strings.ReplaceAll(s, "`", "'") // keeps the alert's code span intact
	s = render.Redact(s)                // never persist a credential from tool input
	if r := []rune(s); len(r) > summaryMax {
		s = string(r[:summaryMax]) + "…"
	}
	return s
}

// handlePermissionRequest records the pending call without resolving the
// session name: it must return well within the prompt's six-second window.
func handlePermissionRequest(_ context.Context, in *Input, d Deps) error {
	pending := &state.Pending{Tool: in.ToolName, Summary: Summary(in.ToolName, in.ToolInput), At: d.now()}
	_, err := d.Store.UpdateSession(in.SessionID, func(sess *state.Session) {
		sess.PendingPermission = pending
		if in.Cwd != "" {
			sess.Cwd = in.Cwd
		}
	})
	return err
}

func handleNotification(ctx context.Context, in *Input, d Deps) error {
	switch in.NotificationType {
	case "permission_prompt":
		if waitingHookOwnsDialog(in, d) {
			d.logf("permission alert skipped: a waiting hook handles the dialog (%s)", in.SessionID)
			return nil
		}
		sess, err := d.resolve(ctx, in)
		if err != nil {
			return err
		}
		text, markdown := "needs your permission", false
		if p := sess.PendingPermission; p != nil && d.now().Sub(p.At) <= pendingTTL && p.Tool != "" {
			text, markdown = permissionText(p.Tool, p.Summary), true
		}
		return alert(ctx, in, d, text, send.KindPermission, markdown)
	case "agent_needs_input":
		return alert(ctx, in, d, "needs input: "+strings.TrimSpace(in.Message), send.KindInput, false)
	}
	return nil
}

// waitingHookOwnsDialog reports whether a live waiting hook holds an open
// ask for the session's latest permission request. An ask older than that
// request belongs to an earlier dialog and must not silence this one.
func waitingHookOwnsDialog(in *Input, d Deps) bool {
	a, ok := d.Store.OpenAskForSession(in.SessionID)
	if !ok || !d.alive(a.HookPID) {
		return false
	}
	sess, err := d.Store.LoadSession(in.SessionID)
	if err != nil || sess.PendingPermission == nil {
		return true
	}
	return !a.Created.Before(sess.PendingPermission.At)
}

func handleStopFailure(ctx context.Context, in *Input, d Deps) error {
	text := "stopped: `" + strings.ReplaceAll(in.Error, "`", "'") + "`"
	if in.ErrorDetails != "" {
		text += ", " + strings.ReplaceAll(in.ErrorDetails, "`", "'")
	}
	return alert(ctx, in, d, text, send.KindFailure, true)
}

// permissionText is the 🔐 alert's markdown body for a pending call.
func permissionText(tool, summary string) string {
	text := "needs permission: `" + tool
	if summary != "" {
		text += ": " + summary
	}
	return text + "`"
}

// alert sends a hook-generated alert through the away gate and the dedupe.
func alert(ctx context.Context, in *Input, d Deps, text string, kind send.Kind, markdown bool) error {
	sess, err := d.resolve(ctx, in)
	if err != nil {
		return err
	}
	if d.Away != nil && !d.Away(ctx) {
		d.logf("alert suppressed (user at keyboard): %s", render.Redact(text))
		return nil
	}
	return deliverAlert(ctx, d, sess.SessionID, sess.Name, text, kind, markdown)
}

// deliverAlert sends an alert unless the same one went out within the
// dedupe window. The text is redacted before it reaches the log, the dedupe
// file or Telegram, and the dedupe key is a hash so no alert text is
// persisted there.
func deliverAlert(ctx context.Context, d Deps, sessionID, name, text string, kind send.Kind, markdown bool) error {
	text = render.Redact(text)
	sum := sha256.Sum256([]byte(sessionID + "|" + text))
	ok, derr := d.Store.Dedupe(hex.EncodeToString(sum[:]), dedupeWindow, d.now())
	if derr != nil {
		d.logf("dedupe: %v", derr)
	}
	if !ok {
		d.logf("alert deduped: %s", text)
		return nil
	}
	_, err := d.Sender.Send(ctx, send.Message{SessionID: sessionID, Label: name, Body: text, Markdown: markdown, Kind: kind})
	return err
}

func handleSessionEnd(_ context.Context, in *Input, d Deps) error {
	_, err := d.Store.UpdateSession(in.SessionID, func(sess *state.Session) {
		sess.Follow = false
		sess.EndedAt = d.now()
	})
	return err
}
