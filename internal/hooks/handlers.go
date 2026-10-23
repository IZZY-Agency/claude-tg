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
		_, _ = d.Store.UpdateSession(sess.SessionID, func(s *state.Session) {
			if s.PendingAnswers > 0 {
				s.PendingAnswers--
				owed = true
			}
		})
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
		sess, err := d.resolve(ctx, in)
		if err != nil {
			return err
		}
		text, markdown := "needs your permission", false
		if p := sess.PendingPermission; p != nil && d.now().Sub(p.At) <= pendingTTL && p.Tool != "" {
			text = "needs permission: `" + p.Tool
			if p.Summary != "" {
				text += ": " + p.Summary
			}
			text += "`"
			markdown = true
		}
		return alert(ctx, in, d, text, send.KindPermission, markdown)
	case "agent_needs_input":
		return alert(ctx, in, d, "needs input: "+strings.TrimSpace(in.Message), send.KindInput, false)
	}
	return nil
}

func handleStopFailure(ctx context.Context, in *Input, d Deps) error {
	text := "stopped: `" + strings.ReplaceAll(in.Error, "`", "'") + "`"
	if in.ErrorDetails != "" {
		text += ", " + strings.ReplaceAll(in.ErrorDetails, "`", "'")
	}
	return alert(ctx, in, d, text, send.KindFailure, true)
}

// alert sends a hook-generated alert through the away gate and the dedupe.
// The text is redacted before it reaches the log, the dedupe file or Telegram,
// and the dedupe key is a hash so no alert text is persisted there.
func alert(ctx context.Context, in *Input, d Deps, text string, kind send.Kind, markdown bool) error {
	text = render.Redact(text)
	sess, err := d.resolve(ctx, in)
	if err != nil {
		return err
	}
	if d.Away != nil && !d.Away(ctx) {
		d.logf("alert suppressed (user at keyboard): %s", text)
		return nil
	}
	sum := sha256.Sum256([]byte(sess.SessionID + "|" + text))
	ok, derr := d.Store.Dedupe(hex.EncodeToString(sum[:]), dedupeWindow, d.now())
	if derr != nil {
		d.logf("dedupe: %v", derr)
	}
	if !ok {
		d.logf("alert deduped: %s", text)
		return nil
	}
	_, err = d.Sender.Send(ctx, send.Message{SessionID: sess.SessionID, Label: sess.Name, Body: text, Markdown: markdown, Kind: kind})
	return err
}

func handleSessionEnd(_ context.Context, in *Input, d Deps) error {
	_, err := d.Store.UpdateSession(in.SessionID, func(sess *state.Session) {
		sess.Follow = false
		sess.EndedAt = d.now()
	})
	return err
}
