package hooks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/send"
	"github.com/IZZY-Agency/claude-tg/internal/send/sendtest"
	"github.com/IZZY-Agency/claude-tg/internal/session"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/term"
)

const sid = "6449d606-55c1-4e6d-a915-4b14c777f57f"

type harness struct {
	rec   *sendtest.Recorder
	store *state.Store
	deps  Deps
	away  bool
	now   time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "st"))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{rec: &sendtest.Recorder{}, store: st, away: true, now: time.Date(2026, 10, 22, 15, 0, 0, 0, time.UTC)}
	sender := &send.Sender{API: h.rec, Store: st, ChatID: 1, Sleep: func(time.Duration) {}}
	h.deps = Deps{
		Sender: sender,
		Store:  st,
		Resolve: func(ctx context.Context, env session.Env) (*state.Session, error) {
			s, _ := st.LoadSession(env.SessionID)
			if s.Name == "" {
				s.Name = "acme-shop-92"
			}
			s.Cwd = env.Cwd
			_ = st.SaveSession(s)
			return s, nil
		},
		Away: func(context.Context) bool { return h.away },
		Now:  func() time.Time { return h.now },
		Log:  func(string, ...any) {},
	}
	return h
}

func load(t *testing.T, name string) *Input {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	in, err := ParseInput(f)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func (h *harness) follow(t *testing.T) {
	t.Helper()
	s, _ := h.store.LoadSession(sid)
	s.Follow = true
	if err := h.store.SaveSession(s); err != nil {
		t.Fatal(err)
	}
}

func TestStopSendsOnlyWhenFollowed(t *testing.T) {
	h := newHarness(t)
	in := load(t, "stop.json")
	if err := Handle(context.Background(), "stop", in, h.deps); err != nil || len(h.rec.Calls) != 0 {
		t.Fatalf("unfollowed session sent: calls=%d err=%v", len(h.rec.Calls), err)
	}
	h.follow(t)
	if err := Handle(context.Background(), "stop", in, h.deps); err != nil || len(h.rec.Calls) != 1 {
		t.Fatalf("followed session: calls=%d err=%v", len(h.rec.Calls), err)
	}
	c := h.rec.Calls[0]
	if !strings.HasPrefix(c.Text, "💬 <b>acme-shop-92</b>\n") || !strings.Contains(c.Text, "<b>42</b> products") || !c.Silent {
		t.Fatalf("feed text=%q silent=%v", c.Text, c.Silent)
	}
}

func TestStopSkipsWhenStopHookActiveOrEmpty(t *testing.T) {
	h := newHarness(t)
	h.follow(t)
	in := load(t, "stop.json")
	in.StopHookActive = true
	_ = Handle(context.Background(), "stop", in, h.deps)
	in.StopHookActive = false
	in.LastAssistantMessage = ""
	_ = Handle(context.Background(), "stop", in, h.deps)
	if len(h.rec.Calls) != 0 {
		t.Fatalf("calls=%d", len(h.rec.Calls))
	}
}

func TestPermissionAlertNamesTool(t *testing.T) {
	h := newHarness(t)
	if err := Handle(context.Background(), "permission-request", load(t, "permission_request_bash.json"), h.deps); err != nil {
		t.Fatal(err)
	}
	if len(h.rec.Calls) != 0 {
		t.Fatal("permission-request must not send")
	}
	h.now = h.now.Add(6 * time.Second)
	if err := Handle(context.Background(), "notification", load(t, "notification_permission.json"), h.deps); err != nil {
		t.Fatal(err)
	}
	if len(h.rec.Calls) != 1 {
		t.Fatalf("calls=%d", len(h.rec.Calls))
	}
	c := h.rec.Calls[0]
	if c.Text != "🔐 <b>acme-shop-92</b>\nneeds permission: <code>Bash: rm -rf node_modules</code>" || c.Silent {
		t.Fatalf("text=%q silent=%v", c.Text, c.Silent)
	}
}

func TestPermissionSummaryForEdit(t *testing.T) {
	h := newHarness(t)
	_ = Handle(context.Background(), "permission-request", load(t, "permission_request_edit.json"), h.deps)
	s, _ := h.store.LoadSession(sid)
	if s.PendingPermission == nil || s.PendingPermission.Summary != "/Users/alex/code/acme-shop/config/settings.php" {
		t.Fatalf("pending=%+v", s.PendingPermission)
	}
}

func TestPermissionAlertGenericWhenStale(t *testing.T) {
	h := newHarness(t)
	_ = Handle(context.Background(), "permission-request", load(t, "permission_request_bash.json"), h.deps)
	h.now = h.now.Add(61 * time.Second)
	_ = Handle(context.Background(), "notification", load(t, "notification_permission.json"), h.deps)
	if len(h.rec.Calls) != 1 || !strings.HasSuffix(h.rec.Calls[0].Text, "\nneeds your permission") {
		t.Fatalf("calls=%+v", h.rec.Calls)
	}
}

func TestAgentNeedsInput(t *testing.T) {
	h := newHarness(t)
	_ = Handle(context.Background(), "notification", load(t, "notification_agent.json"), h.deps)
	if len(h.rec.Calls) != 1 || h.rec.Calls[0].Text != "✋ <b>acme-shop-92</b>\nneeds input: Background agent url-revert needs your input" {
		t.Fatalf("calls=%+v", h.rec.Calls)
	}
}

func TestAlertsRespectAwayGate(t *testing.T) {
	h := newHarness(t)
	h.away = false
	_ = Handle(context.Background(), "notification", load(t, "notification_permission.json"), h.deps)
	_ = Handle(context.Background(), "stop-failure", load(t, "stop_failure.json"), h.deps)
	if len(h.rec.Calls) != 0 {
		t.Fatalf("alerts sent while at the keyboard: %+v", h.rec.Calls)
	}
	// The feed is never gated.
	h.follow(t)
	_ = Handle(context.Background(), "stop", load(t, "stop.json"), h.deps)
	if len(h.rec.Calls) != 1 {
		t.Fatalf("feed gated: calls=%d", len(h.rec.Calls))
	}
}

// phone fixes whether the turn counts as started from the phone.
func (h *harness) phone(fromPhone bool) {
	h.deps.FromPhone = func(*Input, *state.Session) bool { return fromPhone }
}

func TestAlertPhoneStartedAtKeyboard(t *testing.T) {
	h := newHarness(t)
	h.away = false
	var logs []string
	h.deps.Log = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	h.phone(false)
	_ = Handle(context.Background(), "notification", load(t, "notification_permission.json"), h.deps)
	if len(h.rec.Calls) != 0 || len(logs) != 1 || !strings.HasPrefix(logs[0], "alert suppressed (user at keyboard)") {
		t.Fatalf("calls=%d logs=%q", len(h.rec.Calls), logs)
	}
	h.phone(true)
	_ = Handle(context.Background(), "notification", load(t, "notification_permission.json"), h.deps)
	if len(h.rec.Calls) != 1 {
		t.Fatalf("phone-started alert not sent: calls=%d", len(h.rec.Calls))
	}
}

func TestStopFailurePhoneStartedAtKeyboard(t *testing.T) {
	h := newHarness(t)
	h.away = false
	h.phone(true)
	_ = Handle(context.Background(), "stop-failure", load(t, "stop_failure.json"), h.deps)
	if len(h.rec.Calls) != 1 {
		t.Fatalf("calls=%d", len(h.rec.Calls))
	}
}

func TestAlertPhoneStartedDedupe(t *testing.T) {
	h := newHarness(t)
	h.away = false
	h.phone(true)
	in := load(t, "notification_permission.json")
	_ = Handle(context.Background(), "notification", in, h.deps)
	h.now = h.now.Add(5 * time.Minute)
	_ = Handle(context.Background(), "notification", in, h.deps)
	if len(h.rec.Calls) != 1 {
		t.Fatalf("duplicate alert sent: calls=%d", len(h.rec.Calls))
	}
}

func TestAlertsDedupe(t *testing.T) {
	h := newHarness(t)
	in := load(t, "notification_permission.json")
	_ = Handle(context.Background(), "notification", in, h.deps)
	h.now = h.now.Add(5 * time.Minute)
	_ = Handle(context.Background(), "notification", in, h.deps)
	if len(h.rec.Calls) != 1 {
		t.Fatalf("duplicate alert sent: calls=%d", len(h.rec.Calls))
	}
	h.now = h.now.Add(6 * time.Minute)
	_ = Handle(context.Background(), "notification", in, h.deps)
	if len(h.rec.Calls) != 2 {
		t.Fatalf("alert after the window not sent: calls=%d", len(h.rec.Calls))
	}
}

func TestStopFailureText(t *testing.T) {
	h := newHarness(t)
	_ = Handle(context.Background(), "stop-failure", load(t, "stop_failure.json"), h.deps)
	if len(h.rec.Calls) != 1 || h.rec.Calls[0].Text != "💥 <b>acme-shop-92</b>\nstopped: <code>rate_limit</code>, 429 Too Many Requests" {
		t.Fatalf("calls=%+v", h.rec.Calls)
	}
}

// A session end turns follow off and forgets the session's voice marks in
// the same write: a session id resumed within the hour must not speak a
// later answer for a note the ended session never answered.
func TestSessionEndClearsFollow(t *testing.T) {
	h := newHarness(t)
	h.follow(t)
	if _, err := h.store.UpdateSession(sid, func(s *state.Session) {
		s.MarkVoice("", "", h.now)
		s.MarkVoice("0123456789abcdef", "", h.now)
	}); err != nil {
		t.Fatal(err)
	}
	if err := Handle(context.Background(), "session-end", load(t, "session_end.json"), h.deps); err != nil {
		t.Fatal(err)
	}
	s, _ := h.store.LoadSession(sid)
	if s.Follow || s.EndedAt.IsZero() || len(s.VoiceMarks) != 0 || len(h.rec.Calls) != 0 {
		t.Fatalf("session=%+v calls=%d", s, len(h.rec.Calls))
	}
}

func TestParseInputGarbage(t *testing.T) {
	if _, err := ParseInput(strings.NewReader("not json")); err == nil {
		t.Fatal("want error for non-JSON")
	}
	if _, err := ParseInput(strings.NewReader("")); err == nil {
		t.Fatal("want error for empty input")
	}
	if _, err := ParseInput(strings.NewReader(`{"hook_event_name":"Stop"}`)); err == nil {
		t.Fatal("want error when session_id is missing")
	}
}

func TestUnknownKindIsError(t *testing.T) {
	h := newHarness(t)
	if err := Handle(context.Background(), "bogus", load(t, "stop.json"), h.deps); err == nil {
		t.Fatal("want error for unknown kind")
	}
}

// Recording a permission request must not wait on name resolution.
func TestPermissionRequestDoesNotResolve(t *testing.T) {
	h := newHarness(t)
	calls := 0
	h.deps.Resolve = func(ctx context.Context, env session.Env) (*state.Session, error) {
		calls++
		s, _ := h.store.LoadSession(env.SessionID)
		return s, nil
	}
	if err := Handle(context.Background(), "permission-request", load(t, "permission_request_bash.json"), h.deps); err != nil {
		t.Fatal(err)
	}
	s, _ := h.store.LoadSession(sid)
	if calls != 0 || s.PendingPermission == nil || s.PendingPermission.Summary != "rm -rf node_modules" {
		t.Fatalf("resolve calls=%d pending=%+v", calls, s.PendingPermission)
	}
}

// Secrets in tool input must not reach state, dedupe or Telegram.
func TestPermissionAlertRedactsSecrets(t *testing.T) {
	h := newHarness(t)
	in := load(t, "permission_request_bash.json")
	in.ToolInput = []byte(`{"command":"curl -H 'Authorization: token=supersecretvalue123' https://x"}`)
	_ = Handle(context.Background(), "permission-request", in, h.deps)
	s, _ := h.store.LoadSession(sid)
	if strings.Contains(s.PendingPermission.Summary, "supersecretvalue123") {
		t.Fatalf("raw secret persisted: %q", s.PendingPermission.Summary)
	}
	h.now = h.now.Add(6 * time.Second)
	_ = Handle(context.Background(), "notification", load(t, "notification_permission.json"), h.deps)
	if len(h.rec.Calls) != 1 || strings.Contains(h.rec.Calls[0].Text, "supersecretvalue123") || !strings.Contains(h.rec.Calls[0].Text, "[redacted]") {
		t.Fatalf("calls=%+v", h.rec.Calls)
	}
	b, _ := os.ReadFile(filepath.Join(h.store.Dir, "dedupe.json"))
	if strings.Contains(string(b), "redacted") || strings.Contains(string(b), "rm ") || strings.Contains(string(b), "needs permission") {
		t.Fatalf("dedupe file stores raw alert text: %s", b)
	}
}

// v2: a session answered from the phone reports its next answers, one per
// delivered input, then stops.
func TestStopSendsPendingAnswersThenStops(t *testing.T) {
	h := newHarness(t)
	_, _ = h.store.UpdateSession(sid, func(s *state.Session) { s.PendingAnswers = 2 })
	in := load(t, "stop.json")
	_ = Handle(context.Background(), "stop", in, h.deps)
	_ = Handle(context.Background(), "stop", in, h.deps)
	_ = Handle(context.Background(), "stop", in, h.deps)
	if len(h.rec.Calls) != 2 {
		t.Fatalf("calls=%d, want exactly the two pending answers", len(h.rec.Calls))
	}
	s, _ := h.store.LoadSession(sid)
	if s.PendingAnswers != 0 || s.Follow {
		t.Fatalf("counter not cleared: %+v", s)
	}
}

// A waiting hook owns the dialog: the 🔐 alert would duplicate its buttons.
func TestNotificationSkippedWhileAskOpen(t *testing.T) {
	h := newHarness(t)
	if err := Handle(context.Background(), "permission-request", load(t, "permission_request_bash.json"), h.deps); err != nil {
		t.Fatal(err)
	}
	pane := termScreen(t, "permission-screen.txt")
	h.deps.Screen = func(context.Context) (string, error) { return pane, nil }
	if err := h.store.PutAsk(&state.Ask{ID: state.NewAskID(), SessionID: sid, Tool: "Bash", HookPID: 4242, Created: h.now, Signature: state.Fingerprint(term.PermissionSignature(pane))}); err != nil {
		t.Fatal(err)
	}
	h.deps.Alive = func(pid int) bool { return true }
	if err := Handle(context.Background(), "notification", load(t, "notification_permission.json"), h.deps); err != nil {
		t.Fatal(err)
	}
	if len(h.rec.Calls) != 0 {
		t.Fatalf("alert sent while a waiting hook handles the dialog: %+v", h.rec.Calls)
	}
}

// The alert is skipped only for the dialog on the pane: a hook's claim on a
// dialog since answered at the Mac, which it has not yet seen close, owns
// nothing there. Without a pane read nothing is known to be
// claimed, and the alert goes.
func TestNotificationChecksTheDialogOnThePane(t *testing.T) {
	pane := termScreen(t, "permission-screen.txt")
	for name, c := range map[string]struct {
		claimed string // the dialog whose signature the live ask claimed
		screen  func() (string, error)
		want    int
	}{
		"the dialog on the pane":         {pane, func() (string, error) { return pane, nil }, 0},
		"a dialog since answered":        {termScreen(t, "mcp-dialog.txt"), func() (string, error) { return pane, nil }, 1},
		"a pane that cannot be read":     {pane, func() (string, error) { return "", errors.New("no pane") }, 1},
		"no pane reader":                 {pane, nil, 1},
		"no dialog on the pane any more": {pane, func() (string, error) { return termScreen(t, "idle-screen.txt"), nil }, 1},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.deps.Alive = func(pid int) bool { return true }
			if c.screen != nil {
				h.deps.Screen = func(context.Context) (string, error) { return c.screen() }
			}
			if err := Handle(context.Background(), "permission-request", load(t, "permission_request_bash.json"), h.deps); err != nil {
				t.Fatal(err)
			}
			_ = h.store.PutAsk(&state.Ask{ID: state.NewAskID(), SessionID: sid, Tool: "Bash", HookPID: 4242, Created: h.now,
				Signature: state.Fingerprint(term.PermissionSignature(c.claimed))})
			if err := Handle(context.Background(), "notification", load(t, "notification_permission.json"), h.deps); err != nil {
				t.Fatal(err)
			}
			if len(h.rec.Calls) != c.want {
				t.Fatalf("calls=%+v, want %d", h.rec.Calls, c.want)
			}
		})
	}
}

// Calls made at once share one dialog (ruling P25): the hook of the call
// queued behind the one shown waits with its ask open and nothing claimed.
// The hook that claimed the dialog shown silences the alert; with none (a
// first call tg cannot tie to its dialog), the alert goes.
func TestNotificationWhileACallIsQueued(t *testing.T) {
	for name, c := range map[string]struct {
		claimed bool
		want    int
	}{
		"the call shown claimed by its hook": {true, 0},
		"the call shown claimed by none":     {false, 1},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.deps.Alive = func(pid int) bool { return true }
			pane := termScreen(t, "mcp-queue-1of2.txt")
			h.deps.Screen = func(context.Context) (string, error) { return pane, nil }
			h.now = h.now.Add(-time.Second)
			if c.claimed {
				_ = h.store.PutAsk(&state.Ask{ID: state.NewAskID(), SessionID: sid, Tool: "Bash", HookPID: 4241, Created: h.now, Signature: state.Fingerprint(term.PermissionSignature(pane))})
			}
			if err := Handle(context.Background(), "permission-request", load(t, "permission_request_bash.json"), h.deps); err != nil {
				t.Fatal(err)
			}
			h.now = h.now.Add(time.Second)
			_ = h.store.PutAsk(&state.Ask{ID: state.NewAskID(), SessionID: sid, Tool: "Bash", HookPID: 4242, Created: h.now})
			if err := Handle(context.Background(), "notification", load(t, "notification_permission.json"), h.deps); err != nil {
				t.Fatal(err)
			}
			if len(h.rec.Calls) != c.want {
				t.Fatalf("calls=%+v, want %d", h.rec.Calls, c.want)
			}
		})
	}
}

func TestNotificationStillSentWithoutAsk(t *testing.T) {
	cases := map[string]func(h *harness){
		"no ask": func(h *harness) {},
		"another session's ask": func(h *harness) {
			_ = h.store.PutAsk(&state.Ask{ID: state.NewAskID(), SessionID: "other", Tool: "Bash", Created: h.now})
		},
		"closed ask": func(h *harness) {
			_ = h.store.PutAsk(&state.Ask{ID: state.NewAskID(), SessionID: sid, Tool: "Bash", Created: h.now, Closed: "mac"})
		},
		// A waiting hook stuck on an earlier dialog must not silence a newer one.
		"ask older than the pending request": func(h *harness) {
			_ = h.store.PutAsk(&state.Ask{ID: state.NewAskID(), SessionID: sid, Tool: "Bash", Created: h.now.Add(-time.Minute)})
			_ = Handle(context.Background(), "permission-request", load(t, "permission_request_bash.json"), h.deps)
		},
		"ask of a dead hook": func(h *harness) {
			_ = h.store.PutAsk(&state.Ask{ID: state.NewAskID(), SessionID: sid, Tool: "Bash", HookPID: 4242, Created: h.now})
			h.deps.Alive = func(pid int) bool { return pid != 4242 }
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			setup(h)
			if err := Handle(context.Background(), "notification", load(t, "notification_permission.json"), h.deps); err != nil {
				t.Fatal(err)
			}
			if len(h.rec.Calls) != 1 || !strings.HasPrefix(h.rec.Calls[0].Text, "🔐 <b>acme-shop-92</b>") {
				t.Fatalf("calls=%+v", h.rec.Calls)
			}
		})
	}
}

// testKey stands in for a configured TypeSafe key: no credential pattern
// of render.Redact matches it, so only the sender's secrets scrub it.
const testKey = "tsk_live_9QzXv7LmPq2Rw8Yb"

// The hooks' own log lines that carry an alert's text scrub the configured
// secrets as a message is (send.Sender.Clean): the TypeSafe key in an
// alert logs [redacted], whether the alert is held at the keyboard or
// deduped.
func TestAlertLogScrubsSecrets(t *testing.T) {
	h := newHarness(t)
	h.deps.Sender.Secrets = []string{"123456:ABCDEFsecretbot", testKey}
	var logs []string
	h.deps.Log = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	in := load(t, "notification_agent.json")
	in.Message = "Background agent url-revert found the key " + testKey
	h.away = false
	h.phone(false)
	_ = Handle(context.Background(), "notification", in, h.deps)
	h.away = true
	_ = Handle(context.Background(), "notification", in, h.deps)
	_ = Handle(context.Background(), "notification", in, h.deps)
	const text = "needs input: Background agent url-revert found the key [redacted]"
	want := []string{"alert suppressed (user at keyboard): " + text, "alert deduped: " + text}
	if len(logs) != 2 || logs[0] != want[0] || logs[1] != want[1] {
		t.Fatalf("logs=%q, want %q", logs, want)
	}
	if len(h.rec.Calls) != 1 || strings.Contains(h.rec.Calls[0].Text, testKey) {
		t.Fatalf("calls=%+v", h.rec.Calls)
	}
}

// The summary is cleaned before it is cut at summaryMax runes: a
// configured secret crossing the cut leaves no prefix of itself in the
// session record or the 🔐 alert.
func TestPermissionSummaryCleanedBeforeCut(t *testing.T) {
	h := newHarness(t)
	h.deps.Sender.Secrets = []string{testKey}
	in := load(t, "permission_request_bash.json")
	// The key starts at rune 116 of the command: its first four runes fall
	// before the cut.
	in.ToolInput = []byte(`{"command":"echo ` + strings.Repeat("a", summaryMax-10) + ` ` + testKey + ` done"}`)
	if err := Handle(context.Background(), "permission-request", in, h.deps); err != nil {
		t.Fatal(err)
	}
	s, _ := h.store.LoadSession(sid)
	if want := "echo " + strings.Repeat("a", summaryMax-10) + " [red…"; s.PendingPermission == nil || s.PendingPermission.Summary != want {
		t.Fatalf("pending=%+v, want the summary %q", s.PendingPermission, want)
	}
	h.now = h.now.Add(6 * time.Second)
	_ = Handle(context.Background(), "notification", load(t, "notification_permission.json"), h.deps)
	if len(h.rec.Calls) != 1 || strings.Contains(h.rec.Calls[0].Text, testKey[:4]) {
		t.Fatalf("calls=%+v", h.rec.Calls)
	}
}
