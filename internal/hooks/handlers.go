package hooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/IZZY-Agency/claude-tg/internal/classify"
	"github.com/IZZY-Agency/claude-tg/internal/render"
	"github.com/IZZY-Agency/claude-tg/internal/send"
	"github.com/IZZY-Agency/claude-tg/internal/session"
	"github.com/IZZY-Agency/claude-tg/internal/speech"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/term"
	"github.com/IZZY-Agency/claude-tg/internal/transcript"
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
	Speaker Speaker     // speaks a short answer to a voice note (with a Classifier, to the phone) before its text; nil = spoken answers off
	// Classifier judges which answers to questions typed on the phone are
	// also spoken (voiceCheck); nil = no TypeSafe key: only an answer to a
	// question asked by voice is, by speech.Choose alone, as it is with a
	// key.
	Classifier Classifier
	// ReadPrompt reads a transcript's last prompt for the phone-started
	// check of alerts and buttons (phoneCheck); nil = transcript.LastPrompt.
	ReadPrompt promptReader
	// ReadPromptBefore reads the prompt of the turn that gave an answer
	// for the Stop hook's phone check (phonePrompt); nil =
	// transcript.PromptBefore.
	ReadPromptBefore func(path, answer string) (transcript.Prompt, bool, error)
	// TextReserve is the part of the context's time that TypeSafe's
	// judgment, speech and the voice upload leave to the text that
	// follows; 0 = none kept.
	TextReserve time.Duration
	// SpeechWindow is how long TypeSafe's judgment, speech and the voice
	// upload may take together, from the end of the spool flush before
	// them; 0 = maxSpeechWait.
	SpeechWindow time.Duration

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
	FromPhone  func(in *Input, sess *state.Session) bool                       // the turn started from the phone, so alerts and buttons skip the away gate; nil = phoneCheck

	Tick, AwayTick, Deadline time.Duration // defaults 1s, 5s, 12h
}

// Classifier judges an answer for voiceCheck: TypeSafe's Jev
// (classify.Client). question is the phone prompt it answers, "" when there
// is none to send.
type Classifier interface {
	Judge(ctx context.Context, question, answer string) (classify.Judgment, error)
}

const (
	pendingTTL   = 60 * time.Second
	dedupeWindow = 10 * time.Minute
	summaryMax   = 120
	// questionMax caps, in runes, the phone's prompt sent to TypeSafe as
	// question (capRunes): what was asked fits in it, and a pasted page
	// adds nothing to the judgment of a short answer.
	questionMax = 1000
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

// clean is what the sender does to every plain text it sends
// (send.Sender.Clean: the configured secrets, the bot token and the
// TypeSafe key, scrubbed, then credentials redacted); render.Redact alone
// without a sender. A text of a call or an alert that a hook logs or
// stores goes through it, as what it sends does.
func (d Deps) clean(text string) string {
	if d.Sender == nil {
		return render.Redact(text)
	}
	return d.Sender.Clean(text)
}

// alive reports whether pid runs; pid 0 (unknown) counts as alive.
func (d Deps) alive(pid int) bool {
	return pid == 0 || d.Alive == nil || d.Alive(pid)
}

func (d Deps) resolve(ctx context.Context, in *Input) (*state.Session, error) {
	env := d.Env
	env.SessionID = in.SessionID
	env.Cwd = in.Cwd
	env.TranscriptPath = in.TranscriptPath // for the session's title (session.Label)
	env.Own = true                         // hooks run inside the session's own process environment
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

// handleStop posts Claude's answer when the phone is owed it: every answer
// of a followed session, else one per answer owed (TakeAnswer). An answer
// to a voice note (TakeVoice: the oldest live mark, spent with the answer
// it makes spoken) is first sent as a voice message when a Speaker is set
// and speech.Choose finds something to say: the whole answer, or a long
// one's opening. The text follows exactly as it would without.
//
// With a Classifier as well, the answer to a turn the phone started is a
// candidate too: the prompt of the answer's own turn is read from the
// transcript, before the session's write (phonePrompt with
// Deps.ReadPromptBefore: the prompt before the answer, not the newest
// one, as a follow-up the phone queued to the busy session may already be
// written after it; read again for up to 2 s while the answer is not on
// disk yet: stopPrompt), and when it is a phone line the write spends that
// line (TakePhoneLine), so that one phone line makes only the first
// answer after it a candidate. Such a candidate is spoken only as voiceCheck
// decides, given the phone's prompt (cleaned, and cut to questionMax
// runes); an answer to a question asked by voice is spoken on the hard
// rules alone and never judged (owner's ruling, 2026-10-26), and a turn
// started at the Mac with no voice mark is never spoken. When a
// session holding phone lines is not phone-started, the reason is logged
// for an answer the phone gets (logPhone), never with the prompt's text,
// and only while the session's newest phone line is within phoneTurnTTL
// (recentPhoneLine): a session that once got a phone line does not log it
// on every Mac turn after.
//
// A followed session owes nothing, so its record is written only to spend
// a mark or a phone line it holds. Only a mark or line whose spending was
// saved makes the answer a candidate: when the write fails it is still on
// disk, and the answer is text only. An unfollowed session's answer is
// owed only as the write's callback found: a write that fails before its
// callback runs (the lock or the record unreadable) finds nothing owed,
// and nothing is sent.
func handleStop(ctx context.Context, in *Input, d Deps) error {
	sess, err := d.resolve(ctx, in)
	if err != nil {
		return err
	}
	if in.StopHookActive || strings.TrimSpace(in.LastAssistantMessage) == "" {
		return nil
	}
	judge := d.Speaker != nil && d.Classifier != nil
	var prompt transcript.Prompt
	var fromPhone bool
	var why string
	if judge {
		read := func(path string) (transcript.Prompt, bool, error) {
			return d.stopPrompt(ctx, sess, path, in.LastAssistantMessage)
		}
		prompt, fromPhone, why = phonePrompt(in, sess, d.now(), read)
	}
	owed, voice, phone := sess.Follow, false, false // a followed session posts every answer
	var mark state.VoiceMark
	spend := func(s *state.Session) {
		mark, voice = s.TakeVoice(d.now())
		phone = fromPhone && s.TakePhoneLine(prompt.Text, prompt.At)
	}
	var werr error
	switch {
	case !sess.Follow:
		_, werr = d.Store.UpdateSession(sess.SessionID, func(s *state.Session) {
			if owed = s.TakeAnswer(); owed {
				spend(s)
			}
		})
	case len(sess.VoiceMarks) > 0 || fromPhone:
		_, werr = d.Store.UpdateSession(sess.SessionID, spend)
	}
	if werr != nil {
		d.logf("stop: save session: %v", werr)
		voice, phone = false, false
	}
	if !owed {
		return nil
	}
	if recentPhoneLine(sess, d.now()) {
		d.logPhone(sess.SessionID, why) // why a session holding phone lines is not judged as the phone's
	}
	switch {
	case voice && d.Speaker != nil: // asked by voice: the hard rules alone decide
		speakAnswer(ctx, d, sess, in.LastAssistantMessage, "", mark.Lang, false)
	case judge && phone:
		question := capRunes(d.Sender.Clean(strings.TrimSpace(prompt.Text)), questionMax)
		speakAnswer(ctx, d, sess, in.LastAssistantMessage, question, speech.Language(prompt.Text), true)
	}
	_, err = d.Sender.Send(ctx, send.Message{SessionID: sess.SessionID, Label: sess.Name, Body: in.LastAssistantMessage, Silent: true, Markdown: true, Kind: send.KindFeed})
	return err
}

// capRunes returns s cut to at most n runes on a rune boundary, its last
// rune "…" when it was cut.
func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// summary reduces a tool input to one line a phone can show. It is cleaned
// as a message is (clean) before it is cut to summaryMax runes, so a
// secret the cut would split leaves no prefix of itself: the session
// record and the ask store it as it is sent.
func (d Deps) summary(toolName string, toolInput json.RawMessage) string {
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
	s = d.clean(s)                      // never persist a secret or credential from tool input
	if r := []rune(s); len(r) > summaryMax {
		s = string(r[:summaryMax]) + "…"
	}
	return s
}

// handlePermissionRequest records the pending call without resolving the
// session name: it must return well within the prompt's six-second window.
func handlePermissionRequest(_ context.Context, in *Input, d Deps) error {
	pending := &state.Pending{Tool: in.ToolName, Summary: d.summary(in.ToolName, in.ToolInput), At: d.now()}
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
		if waitingHookOwnsDialog(ctx, in, d) {
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
// ask for the session's latest permission request, and a live waiting hook
// of the session claimed the dialog on its pane. An ask older than that
// request belongs to an earlier dialog and must not silence this one. A
// hook still looking for its dialog (a call queued behind another, ruling
// P25) claimed nothing: the dialog shown may be one no hook can match, and
// its alert must go.
func waitingHookOwnsDialog(ctx context.Context, in *Input, d Deps) bool {
	a, ok := d.Store.OpenAskForSession(in.SessionID)
	if !ok || !d.alive(a.HookPID) || !d.claimedDialog(ctx, in.SessionID) {
		return false
	}
	sess, err := d.Store.LoadSession(in.SessionID)
	if err != nil || sess.PendingPermission == nil {
		return true
	}
	return !a.Created.Before(sess.PendingPermission.At)
}

// claimedDialog reports whether an open ask of the session whose hook is
// alive holds the signature of the dialog on the session's pane
// (state.Store.ClaimSignature): its permission dialog's, or a picker's tab
// bar or question. A claim on a dialog since answered at the Mac, which its
// hook has not yet seen close, owns nothing there. Without a pane
// read, nothing is known to be claimed, and the alert goes: at worst it
// repeats the buttons of a prompt already on the phone.
func (d Deps) claimedDialog(ctx context.Context, sessionID string) bool {
	if d.Screen == nil {
		return false
	}
	screen, err := d.Screen(ctx)
	if err != nil {
		d.logf("permission alert (%s): screen: %v", sessionID, err)
		return false
	}
	shown := []string{term.PermissionSignature(screen)}
	if p, ok := term.ReadPicker(screen); ok {
		shown = append(shown, p.Tabs, p.View)
	}
	asks, err := d.Store.ListAsks()
	if err != nil {
		d.logf("list asks: %v", err)
		return false
	}
	for _, a := range asks {
		if a.SessionID != sessionID || !a.Open() || a.Signature == "" || !d.alive(a.HookPID) {
			continue
		}
		if slices.ContainsFunc(shown, func(sig string) bool { return sig != "" && state.Fingerprint(sig) == a.Signature }) {
			return true
		}
	}
	return false
}

// handleStopFailure alerts that the session's turn failed. That turn will
// answer no voice note, so the session's voice marks go first, whether or
// not the alert passes the away gate: a later answer must not be spoken
// for it.
func handleStopFailure(ctx context.Context, in *Input, d Deps) error {
	if s, err := d.Store.LoadSession(in.SessionID); err == nil && len(s.VoiceMarks) > 0 {
		if _, err := d.Store.UpdateSession(in.SessionID, func(s *state.Session) { s.ClearVoice() }); err != nil {
			d.logf("voice marks: %v", err)
		}
	}
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

// alert sends a hook-generated alert through the away gate (skipped for a
// turn started from the phone) and the dedupe.
func alert(ctx context.Context, in *Input, d Deps, text string, kind send.Kind, markdown bool) error {
	sess, err := d.resolve(ctx, in)
	if err != nil {
		return err
	}
	// A turn started from the phone has its owner away from the Mac
	// whatever the idle clock says: its alerts go at once. Asked only
	// while the owner counts as at the Mac, so the log says why a turn of
	// a session holding phone lines waits.
	if d.Away != nil && !d.Away(ctx) {
		phone, why := d.phoneStarted(in, sess)
		if !phone {
			d.logPhone(sess.SessionID, why)
			d.logf("alert suppressed (user at keyboard): %s", d.clean(text))
			return nil
		}
	}
	return deliverAlert(ctx, d, sess.SessionID, sess.Name, text, kind, markdown)
}

// deliverAlert sends an alert unless the same one went out within the
// dedupe window. The text is cleaned as a message is (clean: the
// configured secrets scrubbed, credentials redacted) before it reaches
// the log, the dedupe file or Telegram, and the dedupe key is a hash so no
// alert text is persisted there.
func deliverAlert(ctx context.Context, d Deps, sessionID, name, text string, kind send.Kind, markdown bool) error {
	text = d.clean(text)
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

// handleSessionEnd records that the session ended and turns its follow
// feed off, which forgets its voice marks in the same write
// (state.Session.SetFollow): a session id resumed within the hour must not
// speak a later answer for a note the ended session never answered.
func handleSessionEnd(_ context.Context, in *Input, d Deps) error {
	_, err := d.Store.UpdateSession(in.SessionID, func(sess *state.Session) {
		sess.SetFollow(false)
		sess.EndedAt = d.now()
	})
	return err
}
