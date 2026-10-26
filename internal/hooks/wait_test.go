package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/ask"
	"github.com/IZZY-Agency/claude-tg/internal/render"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
	"github.com/IZZY-Agency/claude-tg/internal/term"
)

type editCall struct {
	id     int64
	html   string
	ctxErr error
}

// pollFunc is a fake transcript watch.
type pollFunc func() (bool, error)

func (f pollFunc) Poll() (bool, error) { return f() }

// waitHarness drives WaitPermission on a virtual clock: every Sleep advances
// it and then runs onSleep, so a test acts between two ticks.
type waitHarness struct {
	*harness
	edits     []editCall
	mac       bool // the fake transcript reports the call resolved
	pollErr   error
	watchErr  error // the transcript cannot be stat'ed
	polls     int
	shown     string // the session's pane
	screenErr error
	reads     int
	alive     func(pid int) bool
	awayCalls int
	sleeps    int
	onSleep   func()
	askID     string // set by a test that needs the ask after it is gone
}

func newWaitHarness(t *testing.T) *waitHarness {
	t.Helper()
	w := &waitHarness{harness: newHarness(t)}
	w.deps.Edit = func(ctx context.Context, id int64, html string) error {
		w.edits = append(w.edits, editCall{id, html, ctx.Err()})
		return nil
	}
	w.deps.RelayUp = func() bool { return true }
	w.deps.Attended = func(context.Context, string) bool { return true }
	w.deps.Alive = func(pid int) bool {
		if w.alive != nil {
			return w.alive(pid)
		}
		return true
	}
	w.deps.Watch = func(path, tool string, input json.RawMessage) (Watcher, error) {
		if w.watchErr != nil {
			return nil, w.watchErr
		}
		return pollFunc(func() (bool, error) { w.polls++; return w.mac, w.pollErr }), nil
	}
	w.deps.Sleep = func(ctx context.Context, d time.Duration) bool {
		w.now = w.now.Add(d)
		w.sleeps++
		if w.onSleep != nil {
			w.onSleep()
		}
		return ctx.Err() == nil
	}
	w.deps.Away = func(context.Context) bool {
		w.awayCalls++
		return w.away
	}
	w.shown = dialogFor(t, "rm -rf node_modules") // permission_request_bash.json's call
	w.deps.Screen = func(context.Context) (string, error) {
		w.reads++
		return w.shown, w.screenErr
	}
	return w
}

// waitPermission is WaitPermission with the decision delivered: its commit
// runs as the hook command runs it once stdout took the decision.
func waitPermission(ctx context.Context, in *Input, d Deps) []byte {
	out, commit := WaitPermission(ctx, in, d)
	if commit != nil {
		commit()
	}
	return out
}

// termScreen is a real Claude Code screen from the term package's tests.
func termScreen(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "term", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The call shown in the real Bash dialog, and the questions in the real
// pickers, of the term package's testdata.
const (
	screenCommand  = "command -v tg && tg follow on"
	screenQuestion = "Which colours do you like for the probe?"      // picker-multi-q1, -q1-ticked, -review
	singleQuestion = "Which fruit do you want for the picker probe?" // picker-single, -cursor, -typing
)

// dialogFor is the real Bash dialog showing command instead of its own.
func dialogFor(t *testing.T, command string) string {
	t.Helper()
	return strings.ReplaceAll(termScreen(t, "permission-screen.txt"), screenCommand, command)
}

// pickerFor is a real picker screen asking question instead of its own
// first question.
func pickerFor(t *testing.T, name, question string) string {
	t.Helper()
	s := termScreen(t, name)
	return strings.ReplaceAll(strings.ReplaceAll(s, screenQuestion, question), singleQuestion, question)
}

// tabBarRe is the recorded pickers' tab bar, ticked or not.
var tabBarRe = regexp.MustCompile(`(?m)^←  .* ✔ Submit  →$`)

// withHeaders is a picker screen of several questions whose tab bar names
// headers instead of the recorded "Colours" and "Pet".
func withHeaders(s string, headers ...string) string {
	bar := "← "
	for _, h := range headers {
		bar += " ☐ " + h + " "
	}
	return tabBarRe.ReplaceAllLiteralString(s, bar+" ✔ Submit  →")
}

// question returns a question request and shows its picker on the pane:
// its first question, and its headers in the tab bar.
func (w *waitHarness) question(t *testing.T, toolInput string) *Input {
	t.Helper()
	in := questionInput(t, toolInput)
	if qs, err := ask.ParseQuestions(in.ToolInput); err == nil {
		w.shown = pickerFor(t, "picker-multi-q1.txt", qs[0].Question)
		if len(qs) > 1 {
			var headers []string
			for _, q := range qs {
				headers = append(headers, q.Header)
			}
			w.shown = withHeaders(w.shown, headers...)
		}
	}
	return in
}

// openAsk returns the only ask in the store.
func (w *waitHarness) openAsk(t *testing.T) *state.Ask {
	t.Helper()
	asks, _ := w.store.ListAsks()
	if len(asks) != 1 {
		t.Fatalf("asks=%d, want 1", len(asks))
	}
	return asks[0]
}

func (w *waitHarness) asksLeft() int {
	asks, _ := w.store.ListAsks()
	return len(asks)
}

const twoQuestions = `{"questions":[` +
	`{"question":"Which fruit?","header":"Fruit","options":[{"label":"Banana","description":"yellow"},{"label":"Apple","description":"red or green"}],"multiSelect":false},` +
	`{"question":"Which color?","header":"Color","options":[{"label":"Red","description":""},{"label":"Blue","description":""}],"multiSelect":false}]}`

func questionInput(t *testing.T, toolInput string) *Input {
	t.Helper()
	in := load(t, "permission_request_bash.json")
	in.ToolName = "AskUserQuestion"
	in.ToolInput = json.RawMessage(toolInput)
	return in
}

// The cheap skips: plan approval, a subagent's prompt, no relay to take a
// tap, a claude -p run, a transcript that cannot be watched. They come
// before the first scan of the transcript (up to 8 MiB).
func TestWaitCheapSkips(t *testing.T) {
	cases := map[string]func(w *waitHarness, in *Input){
		"ExitPlanMode": func(_ *waitHarness, in *Input) {
			in.ToolName, in.ToolInput = "ExitPlanMode", json.RawMessage(`{"plan":"do it"}`)
		},
		"subagent":      func(_ *waitHarness, in *Input) { in.AgentID = "a3f9c2e17b4d" },
		"relay down":    func(w *waitHarness, _ *Input) { w.deps.RelayUp = func() bool { return false } },
		"claude -p":     func(w *waitHarness, _ *Input) { w.deps.Unattended = func() bool { return true } },
		"no transcript": func(w *waitHarness, _ *Input) { w.watchErr = os.ErrNotExist },
		// Ruling R30: no text of the call to find in a dialog, so no
		// dialog can be tied to it (P21: numbers and booleans alone tie
		// no call to a dialog).
		"a tool without a key": func(_ *waitHarness, in *Input) {
			in.ToolName, in.ToolInput = "mcp__linear__save_issue", json.RawMessage(`{"n":3,"b":true}`)
		},
		"a tool with floats and nulls only": func(_ *waitHarness, in *Input) {
			in.ToolName, in.ToolInput = "mcp__linear__save_issue", json.RawMessage(`{"n":3.5,"b":null}`)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWaitHarness(t)
			in := load(t, "permission_request_bash.json")
			setup(w, in)
			if out := waitPermission(context.Background(), in, w.deps); out != nil {
				t.Fatalf("out=%s", out)
			}
			assertSkipped(t, w)
			if w.polls != 0 || w.reads != 0 {
				t.Fatalf("scanned the transcript %d time(s), read the pane %d time(s) before skipping", w.polls, w.reads)
			}
		})
	}
}

// Without Deps.RelayUp the hook asks the store: the running relay must be
// one that routes ask taps.
func TestWaitNeedsARelayThatServesAsks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		features []string
		buttons  bool
	}{{"older relay", nil, false}, {"buttons relay", []string{state.FeatureAsks}, true}} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWaitHarness(t)
			w.deps.RelayUp = nil
			release, err := w.store.LockRelay()
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if err := w.store.SaveRelay(state.RelayState{PID: 1, Features: tc.features}); err != nil {
				t.Fatal(err)
			}
			w.onSleep = func() { w.mac = true }
			waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
			if !tc.buttons {
				assertSkipped(t, w)
				return
			}
			if len(w.rec.Calls) != 1 {
				t.Fatalf("calls=%d, want the button message", len(w.rec.Calls))
			}
		})
	}
}

func TestParseInputAgentID(t *testing.T) {
	in, err := ParseInput(strings.NewReader(`{"session_id":"s1","hook_event_name":"PermissionRequest","agent_id":"a3f9c2e17b4d","agent_type":"Explore"}`))
	if err != nil || in.AgentID != "a3f9c2e17b4d" {
		t.Fatalf("in=%+v err=%v", in, err)
	}
}

func TestWaitSkipsUnattended(t *testing.T) {
	t.Run("claude -p", func(t *testing.T) {
		w := newWaitHarness(t)
		w.deps.Unattended = func() bool { return true }
		w.deps.Attended = func(context.Context, string) bool { t.Error("ran `claude agents` for a claude -p run"); return true }
		if out := waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps); out != nil {
			t.Fatalf("out=%s", out)
		}
		assertSkipped(t, w)
	})
	t.Run("background agent", func(t *testing.T) {
		w := newWaitHarness(t)
		var asked string
		w.deps.Attended = func(_ context.Context, id string) bool {
			asked = id
			if w.asksLeft() != 1 {
				t.Error("`claude agents` ran before the ask was put")
			}
			return false
		}
		if out := waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps); out != nil {
			t.Fatalf("out=%s", out)
		}
		assertSkipped(t, w)
		if asked != sid {
			t.Fatalf("Attended asked for %q", asked)
		}
	})
}

// assertSkipped: nothing sent, no ask left, no wait, but the request is
// recorded for /approve and the 🔐 fallback.
func assertSkipped(t *testing.T, w *waitHarness) {
	t.Helper()
	if len(w.rec.Calls) != 0 || w.asksLeft() != 0 || w.sleeps != 0 || len(w.edits) != 0 {
		t.Fatalf("calls=%d asks=%d sleeps=%d edits=%d", len(w.rec.Calls), w.asksLeft(), w.sleeps, len(w.edits))
	}
	s, _ := w.store.LoadSession(sid)
	if s.PendingPermission == nil || s.PendingPermission.Tool == "" {
		t.Fatalf("request not recorded: %+v", s.PendingPermission)
	}
}

func TestWaitAnsweredAtMacBeforeAway(t *testing.T) {
	w := newWaitHarness(t)
	w.away = false
	w.onSleep = func() {
		if w.sleeps == 3 {
			w.mac = true
		}
	}
	if out := waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps); out != nil {
		t.Fatalf("out=%s", out)
	}
	if len(w.rec.Calls) != 0 || len(w.edits) != 0 || w.asksLeft() != 0 {
		t.Fatalf("calls=%d edits=%d asks=%d", len(w.rec.Calls), len(w.edits), w.asksLeft())
	}
}

// The ask exists before any waiting, so the 6-second notification hook
// already sees it, and it carries what the relay needs.
func TestWaitPutsAskBeforeWaiting(t *testing.T) {
	w := newWaitHarness(t)
	w.away = false
	start := w.now
	w.onSleep = func() {
		if w.sleeps == 1 {
			a := w.openAsk(t)
			if a.SessionID != sid || a.Tool != "Bash" || a.Summary != "rm -rf node_modules" || a.HookPID != os.Getpid() ||
				!a.Created.Equal(start) || !a.Deadline.Equal(start.Add(12*time.Hour)) || len(a.ToolInput) != 0 || !a.Open() ||
				a.Signature == "" || a.Signature != state.Fingerprint(term.PermissionSignature(w.shown)) {
				t.Errorf("ask=%+v", a)
			}
			w.mac = true
		}
	}
	waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
	if w.sleeps != 1 {
		t.Fatalf("sleeps=%d", w.sleeps)
	}
}

// A turn started from the phone sends the buttons at once, though the
// owner counts as at the Mac for another hour; one started at the Mac
// waits as TestWaitAnsweredAtMacBeforeAway shows.
func TestWaitPhoneStartedSendsAtOnce(t *testing.T) {
	w := newWaitHarness(t)
	w.away = false
	w.deps.AwayIn = func(context.Context) time.Duration { return time.Hour }
	w.deps.FromPhone = func(*Input, *state.Session) bool { return true }
	var logs []string
	w.deps.Log = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	start := w.now
	w.onSleep = func() {
		if len(w.rec.Calls) == 1 {
			w.mac = true
		}
	}
	waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
	if len(w.rec.Calls) != 1 || w.now.Sub(start) > 2*time.Second {
		t.Fatalf("calls=%d after %s", len(w.rec.Calls), w.now.Sub(start))
	}
	want := "permission-wait (" + sid + "): phone-started turn: sending now"
	if !slices.Contains(logs, want) {
		t.Fatalf("log line missing: %q", logs)
	}

	w = newWaitHarness(t)
	w.away = false
	w.deps.AwayIn = func(context.Context) time.Duration { return time.Hour }
	w.deps.FromPhone = func(*Input, *state.Session) bool { return false }
	logs = nil
	w.deps.Log = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	w.onSleep = func() {
		if w.sleeps == 3 {
			w.mac = true
		}
	}
	waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
	if len(w.rec.Calls) != 0 {
		t.Fatalf("a Mac-started turn sent: calls=%d", len(w.rec.Calls))
	}
	if slices.Contains(logs, want) {
		t.Fatalf("phone-started line logged for a Mac-started turn: %q", logs)
	}
}

// A negative phone check is made again every 5 s during the first 30 s of
// the wait (ruling P14): the relay records its line only once its Enter
// returns, which may be after the hook first looked. A turn found
// phone-started by a recheck sends at once; one never found so waits for
// the idle time as before; nothing is rechecked after 30 s. Why the turn
// does not count is logged once per hook call.
func TestWaitRechecksPhone(t *testing.T) {
	for _, c := range []struct {
		name      string
		phoneFrom int           // the call from which FromPhone says yes; 0 = never
		sentAt    time.Duration // -1 = not sent
	}{
		{"phone-started at the second check", 2, 5 * time.Second},
		{"phone-started at the last check within 30 s", 6, 25 * time.Second},
		{"never phone-started", 0, -1},
		{"phone-started only after 30 s", 7, -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWaitHarness(t)
			w.away = false
			idleReads := 0
			w.deps.AwayIn = func(context.Context) time.Duration { idleReads++; return time.Hour }
			start := w.now
			var checks []time.Duration
			w.deps.FromPhone = func(*Input, *state.Session) bool {
				checks = append(checks, w.now.Sub(start))
				return c.phoneFrom > 0 && len(checks) >= c.phoneFrom
			}
			var logs []string
			w.deps.Log = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
			sentAt := time.Duration(-1)
			w.onSleep = func() {
				if len(w.rec.Calls) == 1 && sentAt < 0 {
					sentAt = w.now.Sub(start)
					w.mac = true
				}
				if w.now.Sub(start) >= 2*time.Minute {
					w.mac = true
				}
			}
			waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
			// A message sent at s is first seen by the sleep ending a
			// second later.
			if want := c.sentAt; (want < 0 && sentAt >= 0) || (want >= 0 && sentAt != want+time.Second) {
				t.Fatalf("sent at %s (checks at %v), want %s", sentAt, checks, want)
			}
			wantChecks := []time.Duration{0, 5 * time.Second, 10 * time.Second, 15 * time.Second, 20 * time.Second, 25 * time.Second}
			if c.phoneFrom > 0 && c.phoneFrom < len(wantChecks) {
				wantChecks = wantChecks[:c.phoneFrom]
			}
			if !slices.Equal(checks, wantChecks) {
				t.Fatalf("checks at %v, want %v", checks, wantChecks)
			}
			phoneLine := slices.Contains(logs, "permission-wait ("+sid+"): phone-started turn: sending now")
			if phoneLine != (c.sentAt >= 0) {
				t.Fatalf("logs = %q", logs)
			}
			// The rechecks add no idle reads: the hour's wait is read once.
			if idleReads != 1 {
				t.Fatalf("idle time read %d times, want once", idleReads)
			}
		})
	}
}

// The relay's record lands 3 s after the hook first looked (its Enter
// was slow to return): the real check finds it at the 5-second recheck,
// and the buttons go out then.
func TestWaitRecheckFindsALateRecord(t *testing.T) {
	w := newWaitHarness(t)
	w.away = false
	w.deps.AwayIn = func(context.Context) time.Duration { return time.Hour }
	start := w.now
	in := load(t, "permission_request_bash.json")
	in.TranscriptPath = writePrompts(t, promptLine{start, " Привет"})
	w.deps.Watch = func(string, string, json.RawMessage) (Watcher, error) {
		return pollFunc(func() (bool, error) { return w.mac, nil }), nil
	}
	sentAt := time.Duration(-1)
	w.onSleep = func() {
		if w.now.Sub(start) == 3*time.Second {
			if _, err := w.store.UpdateSession(sid, func(s *state.Session) { s.RecordPhoneLine("Привет", w.now, false) }); err != nil {
				t.Error(err)
			}
		}
		if len(w.rec.Calls) == 1 && sentAt < 0 {
			sentAt = w.now.Sub(start)
			w.mac = true
		}
		if w.now.Sub(start) >= time.Minute {
			w.mac = true
		}
	}
	waitPermission(context.Background(), in, w.deps)
	if sentAt != 6*time.Second {
		t.Fatalf("buttons first seen at %s, want 6s (sent at the 5-second recheck)", sentAt)
	}
}

// Why a turn of a session holding phone lines is not phone-started is
// logged once per waiting hook, however often it is rechecked.
func TestWaitLogsPhoneCheckOnce(t *testing.T) {
	w := newWaitHarness(t)
	w.away = false
	w.deps.AwayIn = func(context.Context) time.Duration { return time.Hour }
	in := load(t, "permission_request_bash.json")
	in.TranscriptPath = writePrompts(t, promptLine{w.now, "typed at the Mac"})
	if _, err := w.store.UpdateSession(sid, func(s *state.Session) { s.RecordPhoneLine("Привет", w.now, false) }); err != nil {
		t.Fatal(err)
	}
	w.deps.Watch = func(string, string, json.RawMessage) (Watcher, error) {
		return pollFunc(func() (bool, error) { return w.mac, nil }), nil
	}
	start := w.now
	w.onSleep = func() {
		if w.now.Sub(start) >= time.Minute {
			w.mac = true
		}
	}
	var logs []string
	w.deps.Log = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	waitPermission(context.Background(), in, w.deps)
	n := 0
	for _, l := range logs {
		if l == "phone check ("+sid+"): last prompt was not sent from the phone" {
			n++
		}
	}
	if n != 1 || len(w.rec.Calls) != 0 {
		t.Fatalf("logged the reason %d time(s), calls=%d; logs %q", n, len(w.rec.Calls), logs)
	}
}

// With FromPhone nil the real check runs: the transcript's last prompt is
// the line the relay typed and the stored session holds it, so the
// buttons and the alerts go out at once.
func TestPhoneStartedRealRecordReachesTheHooks(t *testing.T) {
	at := time.Date(2026, 10, 22, 15, 0, 0, 0, time.UTC)
	prep := func(h *harness, in *Input) {
		in.TranscriptPath = writePrompts(t, promptLine{at, " Привет"})
		s, _ := h.store.LoadSession(sid)
		s.RecordPhoneLine("Привет", at, false)
		if err := h.store.SaveSession(s); err != nil {
			t.Fatal(err)
		}
	}

	w := newWaitHarness(t)
	w.away = false
	w.deps.AwayIn = func(context.Context) time.Duration { return time.Hour }
	var logs []string
	w.deps.Log = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	w.deps.Watch = func(string, string, json.RawMessage) (Watcher, error) {
		return pollFunc(func() (bool, error) { return w.mac, nil }), nil
	}
	in := load(t, "permission_request_bash.json")
	prep(w.harness, in)
	w.onSleep = func() {
		if len(w.rec.Calls) == 1 {
			w.mac = true
		}
	}
	waitPermission(context.Background(), in, w.deps)
	if len(w.rec.Calls) != 1 || !slices.Contains(logs, "permission-wait ("+sid+"): phone-started turn: sending now") {
		t.Fatalf("calls=%d logs=%q", len(w.rec.Calls), logs)
	}

	h := newHarness(t)
	h.away = false
	in = load(t, "notification_permission.json")
	prep(h, in)
	_ = Handle(context.Background(), "notification", in, h.deps)
	if len(h.rec.Calls) != 1 {
		t.Fatalf("alert calls=%d", len(h.rec.Calls))
	}
}

// Once a dialog of a phone-started turn is answered at the Mac, the
// owner is there: the turn's next alert waits for the idle time again,
// and the log says why (ruling P16).
func TestPhoneStartedEndsWithAMacAnswer(t *testing.T) {
	w := newWaitHarness(t)
	at := w.now
	in := load(t, "permission_request_bash.json")
	in.TranscriptPath = writePrompts(t, promptLine{at, " Привет"})
	if _, err := w.store.UpdateSession(sid, func(s *state.Session) { s.RecordPhoneLine("Привет", at, false) }); err != nil {
		t.Fatal(err)
	}
	w.away = false
	w.deps.AwayIn = func(context.Context) time.Duration { return time.Hour }
	w.deps.Watch = func(string, string, json.RawMessage) (Watcher, error) {
		return pollFunc(func() (bool, error) { return w.mac, nil }), nil
	}
	w.onSleep = func() {
		if len(w.rec.Calls) == 1 {
			w.mac = true // answered at the Mac, the buttons out
		}
	}
	waitPermission(context.Background(), in, w.deps)
	if len(w.rec.Calls) != 1 {
		t.Fatalf("calls=%d, want the phone-started turn's buttons", len(w.rec.Calls))
	}

	// The turn's next alert, from the same store.
	w.now = w.now.Add(time.Second)
	var logs []string
	w.deps.Log = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	alert := load(t, "notification_permission.json")
	alert.TranscriptPath = in.TranscriptPath
	_ = Handle(context.Background(), "notification", alert, w.deps)
	if len(w.rec.Calls) != 1 || !slices.Contains(logs, "phone check ("+sid+"): answered at the Mac since") {
		t.Fatalf("calls=%d logs=%q, want the alert held for the Mac answer", len(w.rec.Calls), logs)
	}
}

func TestWaitSendsAfterIdle(t *testing.T) {
	w := newWaitHarness(t)
	w.away = false
	start := w.now
	var sentAt time.Time
	var sentAfter int
	w.onSleep = func() {
		if w.awayCalls == 2 {
			w.away = true
		}
		if len(w.rec.Calls) == 1 && sentAt.IsZero() {
			sentAt, sentAfter = w.now, w.awayCalls
			a := w.openAsk(t)
			if len(a.MessageIDs) != 1 || a.MessageIDs[0] != 1 || len(a.Texts) != 1 || a.Texts[0] != w.rec.Calls[0].Text || a.SessionName != "acme-shop-92" {
				t.Errorf("ask after send=%+v", a)
			}
			w.mac = true
		}
	}
	if out := waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps); out != nil {
		t.Fatalf("out=%s", out)
	}
	if len(w.rec.Calls) != 1 || sentAfter != 3 {
		t.Fatalf("calls=%d, sent after away check %d, want one message after the third", len(w.rec.Calls), sentAfter)
	}
	// Away checks at 0 s, 5 s and 10 s: the message goes out at 10 s and is
	// seen by the sleep that ends at 11 s.
	if got := sentAt.Sub(start); got != 11*time.Second {
		t.Fatalf("message first seen after %s, want 11s", got)
	}
	c := w.rec.Calls[0]
	if c.Text != "🔐 <b>acme-shop-92</b>\nneeds permission: <code>Bash: rm -rf node_modules</code>" || c.ParseMode != "HTML" || c.Silent {
		t.Fatalf("text=%q parse=%q silent=%v", c.Text, c.ParseMode, c.Silent)
	}
	if len(c.InlineKeyboard) != 1 || c.InlineKeyboard[0][0].Text != "✅ Yes" || !strings.HasSuffix(c.InlineKeyboard[0][0].Data, ":0:y") || c.InlineKeyboard[0][1].Text != "❌ No" {
		t.Fatalf("keyboard=%+v", c.InlineKeyboard)
	}
}

// While the owner is at the Mac, the idle time is read again only when it
// could first reach the threshold, not every 5 s.
func TestWaitRechecksIdleWhenThresholdCouldBeCrossed(t *testing.T) {
	const threshold = 120 * time.Second
	cases := []struct {
		name   string
		inputs []time.Duration // when the owner touched the Mac, after the request
		checks []time.Duration // when the idle time is read
	}{
		{"leaves at once", nil, []time.Duration{0, 120 * time.Second}},
		{"types again at 60 s", []time.Duration{60 * time.Second}, []time.Duration{0, 120 * time.Second, 180 * time.Second}},
		// The threshold is a few hundred ms away: read again a second later.
		{"almost away", []time.Duration{-119*time.Second - 700*time.Millisecond}, []time.Duration{0, time.Second}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWaitHarness(t)
			start := w.now
			last := start
			for _, at := range c.inputs {
				if at < 0 {
					last = start.Add(at)
				}
			}
			var checks []time.Duration
			w.deps.AwayIn = func(context.Context) time.Duration {
				checks = append(checks, w.now.Sub(start))
				for _, at := range c.inputs {
					if at >= 0 && !w.now.Before(start.Add(at)) {
						last = start.Add(at)
					}
				}
				if idle := w.now.Sub(last); idle < threshold {
					return threshold - idle
				}
				return 0
			}
			var sentAt time.Duration
			w.onSleep = func() {
				if len(w.rec.Calls) == 1 && sentAt == 0 {
					sentAt = w.now.Sub(start)
					w.mac = true
				}
			}
			waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
			if fmt.Sprint(checks) != fmt.Sprint(c.checks) || w.awayCalls != 0 {
				t.Fatalf("idle read at %v (Away called %d times), want %v", checks, w.awayCalls, c.checks)
			}
			if want := c.checks[len(c.checks)-1] + time.Second; sentAt != want {
				t.Fatalf("message first seen at %s, want %s", sentAt, want)
			}
		})
	}
}

func TestWaitRetriesFailedSend(t *testing.T) {
	w := newWaitHarness(t)
	w.rec.Script = []error{errors.New("connection reset by peer")}
	w.onSleep = func() {
		if len(w.rec.Calls) == 2 {
			w.mac = true
		}
	}
	waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
	if len(w.rec.Calls) != 2 || w.awayCalls != 2 {
		t.Fatalf("calls=%d awayCalls=%d, want the retry on the second away check", len(w.rec.Calls), w.awayCalls)
	}
	if len(w.edits) != 1 || w.edits[0].id != 1 {
		t.Fatalf("edits=%+v", w.edits)
	}
}

// Telegram refuses the message for good (400 even as plain text, 403 bot
// blocked): no retries; the owner still hears of the prompt through the
// plain 🔐 alert, and the dialog stays the Mac's.
func TestWaitPermanentSendErrorSendsPlainAlert(t *testing.T) {
	const alert = "🔐 <b>acme-shop-92</b>\nneeds permission: <code>Bash: rm -rf node_modules</code>"
	cases := []struct {
		name   string
		script []error
	}{
		{"403", []error{&telegram.APIError{Code: 403, Description: "Forbidden: bot was blocked by the user"}}},
		{"400 twice", []error{
			&telegram.APIError{Code: 400, Description: "Bad Request: can't parse entities"},
			&telegram.APIError{Code: 400, Description: "Bad Request: BUTTON_DATA_INVALID"},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWaitHarness(t)
			w.rec.Script = c.script
			if out := waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps); out != nil {
				t.Fatalf("out=%s", out)
			}
			n := len(c.script)
			if len(w.rec.Calls) != n+1 || w.awayCalls != 1 || w.sleeps != 0 || w.asksLeft() != 0 || len(w.edits) != 0 {
				t.Fatalf("calls=%d awayCalls=%d sleeps=%d asks=%d edits=%d", len(w.rec.Calls), w.awayCalls, w.sleeps, w.asksLeft(), len(w.edits))
			}
			if a := w.rec.Calls[n]; a.Text != alert || a.ParseMode != "HTML" || len(a.InlineKeyboard) != 0 || a.Silent {
				t.Fatalf("alert=%+v", a)
			}
		})
	}
	t.Run("second question refused", func(t *testing.T) {
		w := newWaitHarness(t)
		w.rec.Script = []error{nil, &telegram.APIError{Code: 403, Description: "Forbidden: bot was blocked by the user"}}
		if out := waitPermission(context.Background(), w.question(t, twoQuestions), w.deps); out != nil {
			t.Fatalf("out=%s", out)
		}
		if len(w.rec.Calls) != 3 || len(w.rec.Calls[2].InlineKeyboard) != 0 || !strings.HasPrefix(w.rec.Calls[2].Text, "🔐 <b>acme-shop-92</b>\nneeds permission: <code>AskUserQuestion: ") {
			t.Fatalf("calls=%+v", w.rec.Calls)
		}
		// The question already out can no longer be answered from the phone.
		if len(w.edits) != 1 || w.edits[0].id != 1 || !strings.HasSuffix(w.edits[0].html, "<i>expired: answer at the Mac</i>") || w.asksLeft() != 0 {
			t.Fatalf("edits=%+v asks=%d", w.edits, w.asksLeft())
		}
	})
}

func TestWaitReturnsPhoneAnswer(t *testing.T) {
	w := newWaitHarness(t)
	sent := make(chan string)
	applied := make(chan error)
	go func() {
		id := <-sent
		_, err := w.store.UpdateAsk(id, func(a *state.Ask) error {
			a.Decision = &state.Decision{Behavior: "allow", Suggestion: -1}
			a.Done = true
			a.Settled = true // the relay showed the answer
			return nil
		})
		applied <- err
	}()
	fired := false
	w.onSleep = func() {
		if len(w.rec.Calls) == 1 && !fired {
			fired = true
			sent <- w.openAsk(t).ID
			if err := <-applied; err != nil {
				t.Errorf("UpdateAsk: %v", err)
			}
		}
	}
	out := waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
	if want := `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`; string(out) != want {
		t.Fatalf("out=%s\nwant %s", out, want)
	}
	if w.asksLeft() != 0 || len(w.edits) != 0 {
		t.Fatalf("asks=%d edits=%+v (the relay edits a phone answer)", w.asksLeft(), w.edits)
	}
}

func TestWaitAnsweredAtMacEditsMessage(t *testing.T) {
	w := newWaitHarness(t)
	w.onSleep = func() {
		if len(w.rec.Calls) == 1 {
			w.mac = true
		}
	}
	if out := waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps); out != nil {
		t.Fatalf("out=%s", out)
	}
	if len(w.edits) != 1 || w.edits[0].id != 1 || w.edits[0].html != w.rec.Calls[0].Text+"\n\n<i>answered at the Mac</i>" {
		t.Fatalf("edits=%+v", w.edits)
	}
	if w.asksLeft() != 0 {
		t.Fatal("ask left behind")
	}
}

// A dialog the waiting hook sees closed at the Mac is recorded in the
// session (MacAnswerAt), at the time it decides so, whether the call's
// result showed (the transcript) or the dialog went (the screen guard):
// a turn the phone started before it no longer counts as one (ruling
// P16). A close within 30 s after the relay typed the phone's /approve or
// /deny fallback came from the phone and records nothing (P20); one 31 s
// after does. A wait that expires or ends records nothing.
func TestWaitMacCloseRecordsMacAnswer(t *testing.T) {
	for _, c := range []struct {
		name   string
		act    func(w *waitHarness)
		record bool
	}{
		{"the call's result", func(w *waitHarness) { w.mac = true }, true},
		{"the dialog gone", func(w *waitHarness) { w.shown = termScreen(t, "idle-screen.txt") }, true},
		// The relay typed the phone's /approve fallback 10 s before: the
		// dialog closed from the phone (ruling P20).
		{"the phone's fallback 10 s before", func(w *waitHarness) {
			w.mac = true
			_, _ = w.store.UpdateSession(sid, func(s *state.Session) { s.PhoneAnswerAt = w.now.Add(-10 * time.Second) })
		}, false},
		{"the phone's fallback 31 s before", func(w *waitHarness) {
			w.mac = true
			_, _ = w.store.UpdateSession(sid, func(s *state.Session) { s.PhoneAnswerAt = w.now.Add(-31 * time.Second) })
		}, true},
		{"expired", func(*waitHarness) {}, false}, // the deadline, 30 s
		{"session ended", func(w *waitHarness) {
			_, _ = w.store.UpdateSession(sid, func(s *state.Session) { s.EndedAt = w.now })
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWaitHarness(t)
			w.deps.Deadline = 30 * time.Second
			var acted time.Time
			w.onSleep = func() {
				if len(w.rec.Calls) == 1 && acted.IsZero() {
					acted = w.now
					c.act(w)
				}
			}
			var logs []string
			w.deps.Log = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
			waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
			s, _ := w.store.LoadSession(sid)
			if strings.HasPrefix(c.name, "the phone's fallback") && !c.record {
				if !s.MacAnswerAt.IsZero() || !slices.Contains(logs, "permission-wait ("+sid+"): closed by the phone's /approve or /deny: no answer at the Mac recorded") {
					t.Fatalf("MacAnswerAt = %v; logs %q", s.MacAnswerAt, logs)
				}
				return
			}
			if !c.record {
				if !s.MacAnswerAt.IsZero() || slices.Contains(logs, "permission-wait ("+sid+"): closed: mac") {
					t.Fatalf("recorded a Mac answer at %v; logs %q", s.MacAnswerAt, logs)
				}
				if len(w.edits) != 1 || strings.Contains(w.edits[0].html, "answered at the Mac") {
					t.Fatalf("edits = %+v, want the message closed as %s", w.edits, c.name)
				}
				return
			}
			if !slices.Contains(logs, "permission-wait ("+sid+"): closed: mac") {
				t.Fatalf("logs = %q, want the Mac close", logs)
			}
			if s.MacAnswerAt.IsZero() || s.MacAnswerAt.Before(acted) || s.MacAnswerAt.After(w.now) {
				t.Fatalf("MacAnswerAt = %v, want the close time, between %v and %v", s.MacAnswerAt, acted, w.now)
			}
		})
	}
}

// A transcript that cannot be read means "unknown": keep waiting.
func TestWaitPollErrorKeepsWaiting(t *testing.T) {
	w := newWaitHarness(t)
	w.pollErr = os.ErrNotExist
	w.deps.Deadline = time.Minute
	waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
	if w.sleeps != 60 || len(w.edits) != 1 || !strings.HasSuffix(w.edits[0].html, "<i>expired: answer at the Mac</i>") {
		t.Fatalf("sleeps=%d edits=%+v", w.sleeps, w.edits)
	}
}

func TestWaitDeadlineExpires(t *testing.T) {
	w := newWaitHarness(t)
	w.deps.Deadline = 30 * time.Second
	start := w.now
	if out := waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps); out != nil {
		t.Fatalf("out=%s", out)
	}
	if got := w.now.Sub(start); got != 30*time.Second {
		t.Fatalf("closed after %s, want 30s", got)
	}
	if len(w.edits) != 1 || !strings.HasSuffix(w.edits[0].html, "\n\n<i>expired: answer at the Mac</i>") || w.asksLeft() != 0 {
		t.Fatalf("edits=%+v asks=%d", w.edits, w.asksLeft())
	}
}

func TestWaitSessionEnded(t *testing.T) {
	ended := func(t *testing.T, w *waitHarness) {
		t.Helper()
		if out := waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps); out != nil {
			t.Fatalf("out=%s", out)
		}
		if len(w.edits) != 1 || !strings.HasSuffix(w.edits[0].html, "\n\n<i>session ended</i>") || w.asksLeft() != 0 {
			t.Fatalf("edits=%+v asks=%d", w.edits, w.asksLeft())
		}
	}
	t.Run("session-end hook", func(t *testing.T) {
		w := newWaitHarness(t)
		// An end recorded before the request belongs to an earlier run.
		_, _ = w.store.UpdateSession(sid, func(s *state.Session) { s.EndedAt = w.now.Add(-time.Hour) })
		w.onSleep = func() {
			if w.sleeps == 3 {
				_, _ = w.store.UpdateSession(sid, func(s *state.Session) { s.EndedAt = w.now })
			}
		}
		ended(t, w)
		if w.sleeps != 3 {
			t.Fatalf("sleeps=%d", w.sleeps)
		}
	})
	t.Run("process gone", func(t *testing.T) {
		w := newWaitHarness(t)
		_, _ = w.store.UpdateSession(sid, func(s *state.Session) { s.PID = 4242 })
		w.alive = func(pid int) bool { return pid != 4242 || w.sleeps < 2 }
		ended(t, w)
	})
	t.Run("the hook's own pid wins over a stale record", func(t *testing.T) {
		w := newWaitHarness(t)
		_, _ = w.store.UpdateSession(sid, func(s *state.Session) { s.PID = 999 }) // an earlier process of a resumed session
		w.deps.Env.PID = 4242
		w.alive = func(pid int) bool { return pid != 999 && (pid != 4242 || w.sleeps < 2) }
		ended(t, w)
	})
}

func TestWaitQuestionOneMessagePerQuestion(t *testing.T) {
	w := newWaitHarness(t)
	w.onSleep = func() {
		if w.sleeps == 1 {
			a := w.openAsk(t)
			if len(a.MessageIDs) != 2 || len(a.Texts) != 2 || len(a.Answers) != 2 || len(a.Ticked) != 2 {
				t.Errorf("ask=%+v", a)
			}
			// A question's input is kept: the answer echoes its questions.
			if qs, err := ask.ParseQuestions(a.ToolInput); err != nil || len(qs) != 2 {
				t.Errorf("tool input %s: %v", a.ToolInput, err)
			}
			// Question 1 answered from the phone, then the Mac answers.
			_, err := w.store.UpdateAsk(a.ID, func(a *state.Ask) error {
				ask.ApplyTap(a, ask.Tap{AskID: a.ID, Q: 0, Action: 'o', N: 0})
				return nil
			})
			if err != nil {
				t.Error(err)
			}
			w.mac = true
		}
	}
	if out := waitPermission(context.Background(), w.question(t, twoQuestions), w.deps); out != nil {
		t.Fatalf("out=%s", out)
	}
	if len(w.rec.Calls) != 2 {
		t.Fatalf("calls=%d", len(w.rec.Calls))
	}
	if c := w.rec.Calls[0]; !strings.HasPrefix(c.Text, "✋ <b>acme-shop-92</b> · <b>Fruit</b> <i>(1/2)</i>\nWhich fruit?") ||
		len(c.InlineKeyboard) != 3 || c.InlineKeyboard[0][0].Text != "Banana" || !strings.HasSuffix(c.InlineKeyboard[0][0].Data, ":0:o0") {
		t.Fatalf("first=%q kb=%+v", c.Text, c.InlineKeyboard)
	}
	if c := w.rec.Calls[1]; !strings.HasPrefix(c.Text, "✋ <b>acme-shop-92</b> · <b>Color</b> <i>(2/2)</i>\nWhich color?") ||
		!strings.HasSuffix(c.InlineKeyboard[0][0].Data, ":1:o0") {
		t.Fatalf("second=%q kb=%+v", c.Text, c.InlineKeyboard)
	}
	// The Mac decided the call: the phone's answer to question 1 was not
	// sent, so both messages say so.
	if len(w.edits) != 2 || w.edits[0].id != 1 || w.edits[1].id != 2 ||
		!strings.HasSuffix(w.edits[0].html, "<i>answered at the Mac</i>") || !strings.HasSuffix(w.edits[1].html, "<i>answered at the Mac</i>") {
		t.Fatalf("edits=%+v", w.edits)
	}
}

// The relay may answer question 1 before question 2's message is out; the
// hook's bookkeeping must keep that answer.
func TestWaitKeepsRelayAnswerWhileSending(t *testing.T) {
	w := newWaitHarness(t)
	w.rec.Script = []error{nil, errors.New("connection reset by peer")}
	tap := func(q int) {
		a := w.openAsk(t)
		if _, err := w.store.UpdateAsk(a.ID, func(a *state.Ask) error {
			ask.ApplyTap(a, ask.Tap{AskID: a.ID, Q: q, Action: 'o', N: 0})
			a.Settled = a.Done // the relay showed the answer
			return nil
		}); err != nil {
			t.Error(err)
		}
	}
	w.onSleep = func() {
		switch {
		case w.sleeps == 1 && len(w.rec.Calls) == 2:
			tap(0)
		case len(w.rec.Calls) == 3 && w.openAsk(t).Answers[1] == nil:
			a := w.openAsk(t)
			if len(a.Answers) != 2 || string(a.Answers[0]) != `"Banana"` || len(a.MessageIDs) != 2 {
				t.Errorf("relay answer lost: answers=%q ids=%v", a.Answers, a.MessageIDs)
			}
			tap(1)
		}
	}
	out := waitPermission(context.Background(), w.question(t, twoQuestions), w.deps)
	var got struct {
		HookSpecificOutput struct {
			Decision struct {
				Behavior     string `json:"behavior"`
				UpdatedInput struct {
					Answers map[string]string `json:"answers"`
				} `json:"updatedInput"`
			} `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("out=%s: %v", out, err)
	}
	d := got.HookSpecificOutput.Decision
	if d.Behavior != "allow" || d.UpdatedInput.Answers["Which fruit?"] != "Banana" || d.UpdatedInput.Answers["Which color?"] != "Red" {
		t.Fatalf("out=%s", out)
	}
	if w.asksLeft() != 0 {
		t.Fatal("ask left behind")
	}
}

// What tg stores and sends is redacted; the answer handed to Claude is not.
func TestWaitStoresRedactedTexts(t *testing.T) {
	w := newWaitHarness(t)
	const secret = "token=supersecretvalue123"
	in := w.question(t, `{"questions":[{"question":"Use `+secret+`?","header":"Auth","options":[{"label":"`+secret+`","description":""},{"label":"No","description":""}],"multiSelect":false}]}`)
	w.onSleep = func() {
		if w.sleeps == 1 {
			a := w.openAsk(t)
			if strings.Contains(a.Texts[0], "supersecretvalue123") || !strings.Contains(a.Texts[0], "[redacted]") {
				t.Errorf("stored text=%q", a.Texts[0])
			}
			if _, err := w.store.UpdateAsk(a.ID, func(a *state.Ask) error {
				ask.ApplyTap(a, ask.Tap{AskID: a.ID, Q: 0, Action: 'o', N: 0})
				return nil
			}); err != nil {
				t.Error(err)
			}
		}
	}
	out := waitPermission(context.Background(), in, w.deps)
	if len(w.rec.Calls) != 1 {
		t.Fatalf("calls=%d", len(w.rec.Calls))
	}
	c := w.rec.Calls[0]
	if strings.Contains(c.Text, "supersecretvalue123") || strings.Contains(c.InlineKeyboard[0][0].Text, "supersecretvalue123") {
		t.Fatalf("sent text=%q kb=%+v", c.Text, c.InlineKeyboard)
	}
	if !strings.Contains(string(out), `"Use `+secret+`?":"`+secret+`"`) {
		t.Fatalf("answer not exact: %s", out)
	}
}

// Claude Code sends SIGTERM about 0.1 s after an Esc at the Mac, before the
// call's result is written, and as the session quits; it never means
// "expired" (the hook's own deadline comes first). Ruling R27: only the
// transcript decides, watched for half a second: a result is the Mac's
// answer, anything else the session ending. The pane is not read: quitting
// Claude Code in a pane that stays open also takes the dialog away.
func TestWaitCancelled(t *testing.T) {
	cases := []struct {
		name  string
		after func(w *waitHarness) // each 100 ms after the signal
		want  string
		took  time.Duration
	}{
		{"esc at the Mac", func(w *waitHarness) { w.mac = true }, "answered at the Mac", 100 * time.Millisecond},
		{"quit, the pane still open", func(w *waitHarness) { w.shown = termScreen(t, "idle-screen.txt") }, "session ended", 500 * time.Millisecond},
		{"session quits", func(*waitHarness) {}, "session ended", 500 * time.Millisecond},
		{"tab closed", func(w *waitHarness) { w.screenErr = errors.New("agtermctl session text: no such session") }, "session ended", 500 * time.Millisecond},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWaitHarness(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var at time.Time
			reads := 0
			w.onSleep = func() {
				switch {
				case !at.IsZero():
					c.after(w)
				case len(w.rec.Calls) == 1:
					at, reads = w.now, w.reads
					cancel() // SIGTERM, SIGHUP or SIGINT
				}
			}
			if out := waitPermission(ctx, load(t, "permission_request_bash.json"), w.deps); out != nil {
				t.Fatalf("out=%s", out)
			}
			if len(w.edits) != 1 || w.edits[0].ctxErr != nil || !strings.HasSuffix(w.edits[0].html, "\n\n<i>"+c.want+"</i>") {
				t.Fatalf("edits=%+v, want %q", w.edits, c.want)
			}
			if got := w.now.Sub(at); got != c.took {
				t.Fatalf("closed %s after the signal, want %s", got, c.took)
			}
			if w.reads != reads {
				t.Fatalf("read the pane %d time(s) after the signal", w.reads-reads)
			}
			if w.asksLeft() != 0 {
				t.Fatal("ask left behind")
			}
		})
	}
}

func TestWaitStoreFailureReturnsNil(t *testing.T) {
	w := newWaitHarness(t)
	asks := filepath.Join(w.store.Dir, "asks")
	if err := os.RemoveAll(asks); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(asks, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps); out != nil {
		t.Fatalf("out=%s", out)
	}
	if len(w.rec.Calls) != 0 || w.sleeps != 0 {
		t.Fatalf("calls=%d sleeps=%d", len(w.rec.Calls), w.sleeps)
	}
}

func TestWaitPanicReturnsNil(t *testing.T) {
	w := newWaitHarness(t)
	var logged string
	w.deps.Log = func(format string, args ...any) {
		if strings.Contains(format, "panic") {
			logged = format
		}
	}
	w.onSleep = func() {
		if len(w.rec.Calls) == 1 {
			panic("boom")
		}
	}
	if out := waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps); out != nil {
		t.Fatalf("out=%s", out)
	}
	if logged == "" || w.asksLeft() != 0 || len(w.edits) != 1 || !strings.HasSuffix(w.edits[0].html, "<i>expired: answer at the Mac</i>") {
		t.Fatalf("logged=%q asks=%d edits=%+v", logged, w.asksLeft(), w.edits)
	}
}

func TestWaitBadQuestionInputReturnsNil(t *testing.T) {
	w := newWaitHarness(t)
	if out := waitPermission(context.Background(), w.question(t, `{"questions":[]}`), w.deps); out != nil {
		t.Fatalf("out=%s", out)
	}
	if len(w.rec.Calls) != 0 || w.asksLeft() != 0 || w.sleeps != 0 {
		t.Fatalf("calls=%d asks=%d sleeps=%d", len(w.rec.Calls), w.asksLeft(), w.sleeps)
	}
}

// Claude Code keys the answers by question text: two questions with one
// text cannot both be answered from the phone. No buttons and no ask, so
// the six-second notification sends the plain alert; the request is
// recorded for it.
func TestWaitSkipsDuplicateQuestionTexts(t *testing.T) {
	w := newWaitHarness(t)
	const dup = `{"questions":[` +
		`{"question":"Which fruit?","header":"Fruit","options":[{"label":"Banana","description":""}],"multiSelect":false},` +
		`{"question":"Which fruit?","header":"Again","options":[{"label":"Apple","description":""}],"multiSelect":false}]}`
	if out := waitPermission(context.Background(), w.question(t, dup), w.deps); out != nil {
		t.Fatalf("out=%s", out)
	}
	if len(w.rec.Calls) != 0 || w.asksLeft() != 0 || w.sleeps != 0 {
		t.Fatalf("calls=%d asks=%d sleeps=%d", len(w.rec.Calls), w.asksLeft(), w.sleeps)
	}
	if sess, _ := w.store.LoadSession(sid); sess.PendingPermission == nil || sess.PendingPermission.Tool != "AskUserQuestion" {
		t.Fatalf("request not recorded: %+v", sess.PendingPermission)
	}
}

// The watch (the transcript's size) is taken first, then the checks run
// cheapest first: relay, the environment marker, the first transcript scan,
// the ask, and `claude agents` last.
func TestWaitWatchesFromTheFirstInstant(t *testing.T) {
	w := newWaitHarness(t)
	var events []string
	w.deps.Watch = func(path, tool string, input json.RawMessage) (Watcher, error) {
		events = append(events, "watch")
		return pollFunc(func() (bool, error) { events = append(events, "poll"); return w.mac, nil }), nil
	}
	w.deps.RelayUp = func() bool { events = append(events, "relay"); return true }
	w.deps.Unattended = func() bool { events = append(events, "marker"); return false }
	w.deps.Screen = func(context.Context) (string, error) { events = append(events, "screen"); return w.shown, nil }
	w.deps.Attended = func(context.Context, string) bool {
		events = append(events, "agents")
		w.mac = true
		return true
	}
	waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
	if got := strings.Join(events, " "); got != "watch relay marker poll screen agents poll" {
		t.Fatalf("events: %s", got)
	}
}

// Live fact (2026-10-24): the call's tool_use line can be written only once
// the dialog is answered, together with its result, after the hook began.
func TestWaitMacAnswerBeforeToolUseLine(t *testing.T) {
	w := newWaitHarness(t)
	w.deps.Watch = nil // the real transcript watch
	in := load(t, "permission_request_bash.json")
	in.TranscriptPath = filepath.Join(t.TempDir(), "t.jsonl")
	writeLines(t, in.TranscriptPath, false, `{"type":"system","subtype":"turn_duration","durationMs":5}`)
	w.deps.Attended = func(context.Context, string) bool {
		writeLines(t, in.TranscriptPath, true,
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_Esc","name":"Bash","input":`+string(in.ToolInput)+`}]}}`,
			`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_Esc","content":"The user doesn't want to proceed with this tool use.","is_error":true}]}}`)
		return true
	}
	if out := waitPermission(context.Background(), in, w.deps); out != nil {
		t.Fatalf("out=%s", out)
	}
	if len(w.rec.Calls) != 0 || len(w.edits) != 0 || w.asksLeft() != 0 || w.sleeps != 0 {
		t.Fatalf("calls=%d edits=%d asks=%d sleeps=%d", len(w.rec.Calls), len(w.edits), w.asksLeft(), w.sleeps)
	}
}

func writeLines(t *testing.T, path string, appendTo bool, lines ...string) {
	t.Helper()
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendTo {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// The phone answered in the same instant the wait ended another way. A
// result at the Mac means the Mac decided the call: every message says so
// and the phone's answer is dropped. Any other end still hands the phone's
// answer to Claude, and the relay's edit stands.
func TestWaitPhoneAnswerWinsOverClose(t *testing.T) {
	for _, how := range []string{"mac", "cancel", "expired", "ended"} {
		t.Run(how, func(t *testing.T) {
			w := newWaitHarness(t)
			w.deps.Deadline = time.Second
			if how != "expired" {
				w.deps.Deadline = time.Minute
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w.onSleep = func() {
				if len(w.rec.Calls) == 1 && w.sleeps == 1 {
					a := w.openAsk(t)
					if _, err := w.store.UpdateAsk(a.ID, func(a *state.Ask) error {
						a.Decision = &state.Decision{Behavior: "allow", Suggestion: -1}
						a.Done = true
						a.Settled = true // the relay showed the answer
						return nil
					}); err != nil {
						t.Error(err)
					}
					switch how {
					case "mac":
						w.mac = true
					case "cancel":
						cancel()
					case "ended":
						_, _ = w.store.UpdateSession(sid, func(s *state.Session) { s.EndedAt = w.now })
					}
				}
			}
			out := waitPermission(ctx, load(t, "permission_request_bash.json"), w.deps)
			if w.asksLeft() != 0 {
				t.Fatal("ask left behind")
			}
			if how == "mac" {
				if out != nil || len(w.edits) != 1 || !strings.HasSuffix(w.edits[0].html, "<i>answered at the Mac</i>") {
					t.Fatalf("out=%s edits=%+v", out, w.edits)
				}
				return
			}
			if want := `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`; string(out) != want {
				t.Fatalf("out=%s", out)
			}
			if len(w.edits) != 0 {
				t.Fatalf("edits=%+v", w.edits)
			}
		})
	}
}

// Question 1 answered from the phone, question 2 not: the answer never
// reached Claude, so its message says so instead of keeping "→ Banana".
func TestWaitPartialAnswerNotSent(t *testing.T) {
	cases := []struct{ how, answered, open string }{
		{"expired", "not sent: answer at the Mac", "expired: answer at the Mac"},
		{"ended", "not sent: session ended", "session ended"},
		{"mac", "answered at the Mac", "answered at the Mac"},
	}
	for _, c := range cases {
		t.Run(c.how, func(t *testing.T) {
			w := newWaitHarness(t)
			w.deps.Deadline = 2 * time.Second
			if c.how != "expired" {
				w.deps.Deadline = time.Minute
			}
			w.onSleep = func() {
				if w.sleeps != 1 {
					return
				}
				a := w.openAsk(t)
				if _, err := w.store.UpdateAsk(a.ID, func(a *state.Ask) error {
					ask.ApplyTap(a, ask.Tap{AskID: a.ID, Q: 0, Action: 'o', N: 0})
					return nil
				}); err != nil {
					t.Error(err)
				}
				switch c.how {
				case "ended":
					_, _ = w.store.UpdateSession(sid, func(s *state.Session) { s.EndedAt = w.now })
				case "mac":
					w.mac = true
				}
			}
			if out := waitPermission(context.Background(), w.question(t, twoQuestions), w.deps); out != nil {
				t.Fatalf("out=%s", out)
			}
			if len(w.edits) != 2 || w.edits[0].id != 1 || w.edits[1].id != 2 {
				t.Fatalf("edits=%+v", w.edits)
			}
			if !strings.HasSuffix(w.edits[0].html, "?\n• <b>Banana</b> — yellow\n• <b>Apple</b> — red or green\n\n<i>"+c.answered+"</i>") {
				t.Errorf("answered message: %q, want it to end in %q", w.edits[0].html, c.answered)
			}
			if !strings.HasSuffix(w.edits[1].html, "\n\n<i>"+c.open+"</i>") {
				t.Errorf("open message: %q, want it to end in %q", w.edits[1].html, c.open)
			}
		})
	}
}

// Stored texts and closing edits are exactly what was sent: redacted and
// scrubbed of the bot token.
func TestWaitScrubsBotToken(t *testing.T) {
	w := newWaitHarness(t)
	const bot = "123456:ABCDEFsecretbot"
	w.deps.Sender.Secrets = []string{bot}
	in := load(t, "permission_request_bash.json")
	in.ToolInput = json.RawMessage(`{"command":"curl https://api.telegram.org/bot` + bot + `/getMe"}`)
	w.shown = dialogFor(t, "curl https://api.telegram.org/bot"+bot+"/getMe")
	w.onSleep = func() {
		if w.sleeps == 1 {
			if a := w.openAsk(t); strings.Contains(a.Texts[0], bot) || a.Texts[0] != w.rec.Calls[0].Text {
				t.Errorf("stored=%q sent=%q", a.Texts[0], w.rec.Calls[0].Text)
			}
			w.mac = true
		}
	}
	waitPermission(context.Background(), in, w.deps)
	if len(w.rec.Calls) != 1 || strings.Contains(w.rec.Calls[0].Text, bot) {
		t.Fatalf("calls=%+v", w.rec.Calls)
	}
	if len(w.edits) != 1 || strings.Contains(w.edits[0].html, bot) || !strings.Contains(w.edits[0].html, "bot[redacted]/getMe") {
		t.Fatalf("edits=%+v", w.edits)
	}
}

// The waiting hook's summary is cleaned before it is cut, as the
// permission request's: a configured secret crossing the cut at
// summaryMax runes leaves no prefix of itself in the session record, the
// ask or the messages.
func TestWaitSummaryCleanedBeforeCut(t *testing.T) {
	w := newWaitHarness(t)
	w.deps.Sender.Secrets = []string{testKey}
	cmd := "echo " + strings.Repeat("a", summaryMax-10) + " " + testKey + " done"
	in := load(t, "permission_request_bash.json")
	in.ToolInput = json.RawMessage(`{"command":"` + cmd + `"}`)
	w.shown = dialogFor(t, cmd)
	leaks := func(s string) bool { return strings.Contains(s, testKey[:4]) }
	asked := false
	w.onSleep = func() {
		if w.sleeps == 1 {
			a := w.openAsk(t)
			asked = true
			if leaks(a.Summary) || leaks(strings.Join(a.Texts, "")) {
				t.Errorf("ask summary=%q texts=%q", a.Summary, a.Texts)
			}
			w.mac = true
		}
	}
	waitPermission(context.Background(), in, w.deps)
	if !asked || len(w.rec.Calls) != 1 || leaks(w.rec.Calls[0].Text) {
		t.Fatalf("asked=%v calls=%+v", asked, w.rec.Calls)
	}
	for _, e := range w.edits {
		if leaks(e.html) {
			t.Fatalf("edits=%+v", w.edits)
		}
	}
	if s, _ := w.store.LoadSession(sid); s.PendingPermission == nil || leaks(s.PendingPermission.Summary) {
		t.Fatalf("pending=%+v", s.PendingPermission)
	}
}

// An option label holding the bot token is masked in the question's
// message, its buttons and the stored text the relay edits from.
func TestWaitScrubsBotTokenInAQuestion(t *testing.T) {
	w := newWaitHarness(t)
	const bot = "123456:ABCDEFsecretbot"
	w.deps.Sender.Secrets = []string{bot}
	in := w.question(t, `{"questions":[{"question":"Which bot?","header":"Bot","options":[{"label":"bot`+bot+`","description":"the `+bot+` one"},{"label":"none","description":""}],"multiSelect":false}]}`)
	w.onSleep = func() {
		if len(w.rec.Calls) > 0 {
			if a := w.openAsk(t); strings.Contains(strings.Join(a.Texts, ""), bot) {
				t.Errorf("stored texts hold the token: %q", a.Texts)
			}
			w.mac = true
		}
	}
	waitPermission(context.Background(), in, w.deps)
	if len(w.rec.Calls) != 1 {
		t.Fatalf("calls=%d", len(w.rec.Calls))
	}
	c := w.rec.Calls[0]
	kb := fmt.Sprint(c.InlineKeyboard)
	if strings.Contains(c.Text, bot) || strings.Contains(kb, bot) || !strings.Contains(kb, "bot[redacted]") {
		t.Fatalf("text=%q keyboard=%s", c.Text, kb)
	}
	if len(w.edits) != 1 || strings.Contains(w.edits[0].html, bot) {
		t.Fatalf("edits=%+v", w.edits)
	}
}

// Claude Code kills the hook about 1.5 s after its SIGTERM. While the relay
// holds the ask's lock (an edit stuck in the network), the hook waits for
// it until its closing deadline (1.4 s after the signal), tries once more,
// then writes its closure without the lock: the ask says "mac" and the answer the relay counted as owed to the
// phone is taken back. The removal is left to the relay's sweep.
func TestWaitCancelledClosesUnderABusyLock(t *testing.T) {
	w := newWaitHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	free, done := make(chan struct{}), make(chan struct{})
	var signalled time.Time
	w.onSleep = func() {
		if w.sleeps != 1 {
			return
		}
		a := w.openAsk(t)
		w.askID = a.ID
		_, _ = w.store.UpdateAsk(a.ID, func(a *state.Ask) error {
			a.Decision, a.Done, a.Armed = &state.Decision{Behavior: "allow", Suggestion: -1}, true, true
			return nil
		})
		_, _ = w.store.UpdateSession(sid, func(s *state.Session) { s.ArmAsk(a.ID) }) // as the relay arms it
		held := make(chan struct{})
		go func() {
			defer close(done)
			_ = w.store.WithAsk(a.ID, func(*state.Ask) error {
				close(held)
				select {
				case <-free:
				case <-time.After(15 * time.Second):
				}
				return nil
			})
		}()
		<-held
		w.mac = true // Esc at the Mac: its result is in the transcript
		signalled = time.Now()
		cancel()
	}
	out := waitPermission(ctx, load(t, "permission_request_bash.json"), w.deps)
	took := time.Since(signalled)
	close(free)
	<-done
	if out != nil || took > 1500*time.Millisecond {
		t.Fatalf("out=%s, closed %s after the signal, want no decision within the 1.5s grace", out, took)
	}
	a, err := w.store.LoadAsk(w.id(t))
	if err != nil || a.Closed != "mac" || !a.TakeBack {
		t.Fatalf("ask %+v, %v; want it closed as mac, its answer owed back", a, err)
	}
	if s, _ := w.store.LoadSession(sid); s.Owed() != 0 {
		t.Fatalf("owed %d, want 0", s.Owed())
	}
	if len(w.edits) != 1 || !strings.HasSuffix(w.edits[0].html, "<i>answered at the Mac</i>") {
		t.Fatalf("edits=%+v", w.edits)
	}
}

// The session's lock is held (another hook recording, the relay arming)
// when the signalled hook closes: the closure goes on record first, then
// the answer owed back waits for the session's lock only until the closing
// deadline. Not done in time, it stays owed on the ask (TakeBack), which is
// left for the relay's sweep.
func TestWaitCancelledUnderABusySessionLock(t *testing.T) {
	w := newWaitHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	free, done := make(chan struct{}), make(chan struct{})
	var signalled time.Time
	w.onSleep = func() {
		if w.sleeps != 1 {
			return
		}
		a := w.openAsk(t)
		w.askID = a.ID
		_, _ = w.store.UpdateAsk(a.ID, func(a *state.Ask) error {
			a.Decision, a.Done, a.Armed = &state.Decision{Behavior: "allow", Suggestion: -1}, true, true
			return nil
		})
		_, _ = w.store.UpdateSession(sid, func(s *state.Session) { s.ArmAsk(a.ID) }) // as the relay arms it
		held := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = w.store.UpdateSession(sid, func(*state.Session) {
				close(held)
				select {
				case <-free:
				case <-time.After(15 * time.Second):
				}
			})
		}()
		<-held
		w.mac = true
		signalled = time.Now()
		cancel()
	}
	out := waitPermission(ctx, load(t, "permission_request_bash.json"), w.deps)
	took := time.Since(signalled)
	close(free)
	<-done
	if out != nil || took > 1500*time.Millisecond {
		t.Fatalf("out=%s, closed %s after the signal, want no decision within the 1.5s grace", out, took)
	}
	a, err := w.store.LoadAsk(w.id(t))
	if err != nil || a.Closed != "mac" || !a.TakeBack {
		t.Fatalf("ask %+v, %v; want it kept, closed as mac, its answer owed back", a, err)
	}
	if s, _ := w.store.LoadSession(sid); s.Owed() != 1 {
		t.Fatalf("owed %d, want 1 until the sweep", s.Owed())
	}
	if len(w.edits) != 1 || !strings.HasSuffix(w.edits[0].html, "<i>answered at the Mac</i>") {
		t.Fatalf("edits=%+v", w.edits)
	}
}

// The relay holds the ask's lock (a stuck edit) and the session's lock is
// busy too when Claude Code signals the hook, which it kills about 1.5 s
// later. The hook's closing runs under one deadline, 1.4 s from the
// signal, for both locks and its closing edits: waiting for one lock never
// renews the time for the next, and the edits never run past it. What it
// could not finish (the ask's removal, the answer owed back) is left to
// the relay's sweep.
func TestWaitCancelledUnderBothLocksKeepsOneDeadline(t *testing.T) {
	w := newWaitHarness(t)
	var deadlines []time.Time
	w.deps.Edit = func(ctx context.Context, id int64, html string) error {
		dl, _ := ctx.Deadline()
		deadlines = append(deadlines, dl)
		w.edits = append(w.edits, editCall{id, html, ctx.Err()})
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	free := make(chan struct{})
	var holders sync.WaitGroup
	hold := func(take func(hold func())) {
		held := make(chan struct{})
		holders.Add(1)
		go func() {
			defer holders.Done()
			take(func() {
				close(held)
				select {
				case <-free:
				case <-time.After(15 * time.Second):
				}
			})
		}()
		<-held
	}
	var signalled time.Time
	w.onSleep = func() {
		if w.sleeps != 1 {
			return
		}
		a := w.openAsk(t)
		w.askID = a.ID
		_, _ = w.store.UpdateAsk(a.ID, func(a *state.Ask) error {
			a.Decision, a.Done, a.Armed = &state.Decision{Behavior: "allow", Suggestion: -1}, true, true
			return nil
		})
		_, _ = w.store.UpdateSession(sid, func(s *state.Session) { s.ArmAsk(a.ID) })
		hold(func(wait func()) { _ = w.store.WithAsk(a.ID, func(*state.Ask) error { wait(); return nil }) })
		hold(func(wait func()) { _, _ = w.store.UpdateSession(sid, func(*state.Session) { wait() }) })
		w.mac = true
		signalled = time.Now()
		cancel()
	}
	out := waitPermission(ctx, load(t, "permission_request_bash.json"), w.deps)
	took := time.Since(signalled)
	close(free)
	holders.Wait()
	if out != nil || took > 1500*time.Millisecond {
		t.Fatalf("out=%s, closed %s after the signal, want within 1.5s", out, took)
	}
	limit := signalled.Add(1450 * time.Millisecond)
	if len(deadlines) != 1 || deadlines[0].IsZero() || deadlines[0].After(limit) {
		t.Fatalf("closing edits' deadline %v, want by %v (1.4s after the signal)", deadlines, limit)
	}
	a, err := w.store.LoadAsk(w.id(t))
	if err != nil || a.Closed != "mac" || !a.TakeBack {
		t.Fatalf("ask %+v, %v; want it closed as mac, its answer owed back", a, err)
	}
	if s, _ := w.store.LoadSession(sid); s.Owed() != 1 {
		t.Fatalf("owed %d, want 1 until the sweep", s.Owed())
	}
}

// Claude Code stops the hook as the session quits: say so, not "expired".
func TestWaitCancelledAsSessionEndsSaysEnded(t *testing.T) {
	w := newWaitHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.onSleep = func() {
		if len(w.rec.Calls) == 1 {
			_, _ = w.store.UpdateSession(sid, func(s *state.Session) { s.EndedAt = w.now })
			cancel()
		}
	}
	waitPermission(ctx, load(t, "permission_request_bash.json"), w.deps)
	if len(w.edits) != 1 || !strings.HasSuffix(w.edits[0].html, "\n\n<i>session ended</i>") {
		t.Fatalf("edits=%+v", w.edits)
	}
}

// A message that went out but could not be recorded cannot be answered:
// it is closed at once.
func TestWaitRecordFailureClosesExpired(t *testing.T) {
	w := newWaitHarness(t)
	w.deps.Away = func(context.Context) bool {
		asks, _ := w.store.ListAsks()
		for _, a := range asks {
			_ = w.store.DeleteAsk(a.ID)
		}
		return true
	}
	if out := waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps); out != nil {
		t.Fatalf("out=%s", out)
	}
	if len(w.rec.Calls) != 1 || w.sleeps != 0 || len(w.edits) != 1 || !strings.HasSuffix(w.edits[0].html, "<i>expired: answer at the Mac</i>") {
		t.Fatalf("calls=%d sleeps=%d edits=%+v", len(w.rec.Calls), w.sleeps, w.edits)
	}
}

// The relay's sweep closed the ask (and edited the message): just leave.
func TestWaitAskClosedElsewhere(t *testing.T) {
	w := newWaitHarness(t)
	w.onSleep = func() {
		if w.sleeps == 1 {
			a := w.openAsk(t)
			_, _ = w.store.UpdateAsk(a.ID, func(a *state.Ask) error { a.Closed = "expired"; return nil })
		}
	}
	if out := waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps); out != nil {
		t.Fatalf("out=%s", out)
	}
	if len(w.edits) != 0 || w.asksLeft() != 0 || w.sleeps != 1 {
		t.Fatalf("edits=%+v asks=%d sleeps=%d", w.edits, w.asksLeft(), w.sleeps)
	}
}

func TestWaitAskGoneClosesExpired(t *testing.T) {
	w := newWaitHarness(t)
	w.onSleep = func() {
		if w.sleeps == 1 {
			_ = w.store.DeleteAsk(w.openAsk(t).ID)
		}
	}
	if out := waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps); out != nil {
		t.Fatalf("out=%s", out)
	}
	if len(w.edits) != 1 || !strings.HasSuffix(w.edits[0].html, "<i>expired: answer at the Mac</i>") || w.sleeps != 1 {
		t.Fatalf("edits=%+v sleeps=%d", w.edits, w.sleeps)
	}
}

// A phone answer that cannot be turned into a decision sends the owner
// back to the Mac instead of leaving "approved" on the phone.
func TestWaitOutputErrorSendsOwnerToMac(t *testing.T) {
	w := newWaitHarness(t)
	w.onSleep = func() {
		if w.sleeps == 1 {
			a := w.openAsk(t)
			_, _ = w.store.UpdateAsk(a.ID, func(a *state.Ask) error { a.Done = true; return nil }) // no Decision
		}
	}
	if out := waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps); out != nil {
		t.Fatalf("out=%s", out)
	}
	if len(w.edits) != 1 || !strings.HasSuffix(w.edits[0].html, "<i>expired: answer at the Mac</i>") || w.asksLeft() != 0 {
		t.Fatalf("edits=%+v asks=%d", w.edits, w.asksLeft())
	}
}

// The relay counts a phone answer in the session's PendingAnswers before it
// saves the ask done (Armed). When the Mac answers anyway, the phone's
// answer never reaches Claude and nothing is owed to the phone: the hook
// takes the count back, once. A followed session was never counted.
func TestWaitMacWinTakesBackTheArmedAnswer(t *testing.T) {
	for _, tc := range []struct {
		name        string
		armed       bool
		typed, want int // answers owed for typed replies; owed in all after the close
	}{
		{"armed", true, 0, 0},
		{"armed, with typed replies owed too", true, 2, 2},
		{"not armed (a followed session)", false, 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWaitHarness(t)
			if _, err := w.store.UpdateSession(sid, func(s *state.Session) { s.PendingAnswers = tc.typed }); err != nil {
				t.Fatal(err)
			}
			w.onSleep = func() {
				if w.sleeps == 1 {
					a := w.openAsk(t)
					_, _ = w.store.UpdateAsk(a.ID, func(a *state.Ask) error {
						a.Decision, a.Done, a.Armed = &state.Decision{Behavior: "allow", Suggestion: -1}, true, tc.armed
						return nil
					})
					if tc.armed { // as the relay arms it
						_, _ = w.store.UpdateSession(sid, func(s *state.Session) { s.ArmAsk(a.ID) })
					}
					w.mac = true // the transcript has the call's result: the Mac answered first
				}
			}
			if out := waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps); out != nil {
				t.Fatalf("out=%s", out)
			}
			if s, _ := w.store.LoadSession(sid); s.Owed() != tc.want || s.PendingAnswers != tc.typed {
				t.Fatalf("owed %d (typed %d), want %d (typed %d)", s.Owed(), s.PendingAnswers, tc.want, tc.typed)
			}
			if len(w.edits) != 1 || !strings.HasSuffix(w.edits[0].html, "<i>answered at the Mac</i>") {
				t.Fatalf("edits=%+v", w.edits)
			}
		})
	}
	// An answer that cannot become a decision is not delivered either.
	w := newWaitHarness(t)
	w.onSleep = func() {
		if w.sleeps == 1 {
			a := w.openAsk(t)
			_, _ = w.store.UpdateAsk(a.ID, func(a *state.Ask) error { a.Done, a.Armed = true, true; return nil }) // no Decision
			_, _ = w.store.UpdateSession(sid, func(s *state.Session) { s.ArmAsk(a.ID) })
		}
	}
	if out := waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps); out != nil {
		t.Fatalf("out=%s", out)
	}
	if s, _ := w.store.LoadSession(sid); s.Owed() != 0 {
		t.Fatalf("output error: owed %d, want 0", s.Owed())
	}
}

// A failure that repeats every check or tick is logged once, not 43 200 times.
func TestWaitLogsPersistentFailuresOnce(t *testing.T) {
	w := newWaitHarness(t)
	var logs []string
	w.deps.Log = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	w.rec.Script = []error{errors.New("connection reset"), errors.New("connection reset"), errors.New("connection reset")}
	corruptAt := 0
	w.onSleep = func() {
		switch {
		case len(w.rec.Calls) == 4 && corruptAt == 0:
			corruptAt = w.sleeps
			a := w.openAsk(t)
			if err := os.WriteFile(filepath.Join(w.store.Dir, "asks", a.ID+".json"), []byte("{"), 0o600); err != nil {
				t.Error(err)
			}
		case corruptAt > 0 && w.sleeps == corruptAt+5:
			w.mac = true
		}
	}
	waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
	count := func(sub string) int {
		n := 0
		for _, l := range logs {
			if strings.Contains(l, sub) {
				n++
			}
		}
		return n
	}
	if len(w.rec.Calls) != 4 || count("send:") != 1 || count("load ask") != 1 {
		t.Fatalf("calls=%d logs:\n%s", len(w.rec.Calls), strings.Join(logs, "\n"))
	}
	if w.asksLeft() != 0 || len(w.edits) != 1 {
		t.Fatalf("asks=%d edits=%+v", w.asksLeft(), w.edits)
	}
}

// No pane to read: the hook cannot tell when the dialog closes at the Mac,
// so it leaves the prompt to the plain 🔐 alert.
func TestWaitNoPaneNoButtons(t *testing.T) {
	w := newWaitHarness(t)
	w.deps.Screen = nil
	if out := waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps); out != nil {
		t.Fatalf("out=%s", out)
	}
	assertSkipped(t, w)
	if waitingHookOwnsDialog(context.Background(), load(t, "notification_permission.json"), w.deps) {
		t.Fatal("the 6-second alert would still be silenced")
	}
}

// The dialog must show on the pane within 3 s, read every half second.
func TestWaitDialogNotOnScreenNoButtons(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(w *waitHarness)
	}{
		{"idle prompt", func(w *waitHarness) { w.shown = termScreen(t, "idle-screen.txt") }},
		{"unreadable pane", func(w *waitHarness) { w.screenErr = errors.New("agtermctl session text: no such session") }},
		{"a picker, not a dialog", func(w *waitHarness) { w.shown = termScreen(t, "picker-single.txt") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWaitHarness(t)
			tc.setup(w)
			start := w.now
			if out := waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps); out != nil {
				t.Fatalf("out=%s", out)
			}
			if len(w.rec.Calls) != 0 || w.asksLeft() != 0 || len(w.edits) != 0 {
				t.Fatalf("calls=%d asks=%d edits=%d", len(w.rec.Calls), w.asksLeft(), len(w.edits))
			}
			if got := w.now.Sub(start); got != 3*time.Second || w.reads != 7 {
				t.Fatalf("gave up after %s and %d reads, want 3s and 7", got, w.reads)
			}
		})
	}
}

func TestWaitDialogAppearsLate(t *testing.T) {
	w := newWaitHarness(t)
	dialog := w.shown
	w.shown = termScreen(t, "idle-screen.txt")
	w.onSleep = func() {
		switch {
		case w.sleeps == 4: // 2 s
			w.shown = dialog
		case len(w.rec.Calls) == 1:
			w.mac = true
		}
	}
	waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
	if len(w.rec.Calls) != 1 {
		t.Fatalf("calls=%d, want the button message", len(w.rec.Calls))
	}
}

// Approved at the Mac while the owner was there, the tool still running (no
// result yet): when the owner leaves, the dialog is gone and nothing is sent.
func TestWaitScreenGuardBeforeSending(t *testing.T) {
	w := newWaitHarness(t)
	w.away = false
	w.onSleep = func() {
		switch w.sleeps {
		case 3:
			w.shown = termScreen(t, "idle-screen.txt") // "1" at the Mac
		case 8:
			w.away = true
		}
	}
	if out := waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps); out != nil {
		t.Fatalf("out=%s", out)
	}
	if len(w.rec.Calls) != 0 || len(w.edits) != 0 || w.asksLeft() != 0 {
		t.Fatalf("calls=%d edits=%d asks=%d", len(w.rec.Calls), len(w.edits), w.asksLeft())
	}
}

// After sending, the pane is read every 5 s; two reads 1 s apart without
// the dialog mean it was answered at the Mac.
func TestWaitScreenGuardAfterSending(t *testing.T) {
	w := newWaitHarness(t)
	var goneAt time.Time
	w.onSleep = func() {
		if len(w.rec.Calls) == 1 && goneAt.IsZero() && w.sleeps == 7 {
			goneAt = w.now
			w.shown = termScreen(t, "idle-screen.txt")
		}
	}
	if out := waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps); out != nil {
		t.Fatalf("out=%s", out)
	}
	if got := w.now.Sub(goneAt); goneAt.IsZero() || got > 6*time.Second {
		t.Fatalf("closed %s after the dialog went", got)
	}
	if len(w.edits) != 1 || !strings.HasSuffix(w.edits[0].html, "<i>answered at the Mac</i>") || w.asksLeft() != 0 {
		t.Fatalf("edits=%+v asks=%d", w.edits, w.asksLeft())
	}
}

func TestWaitScreenGuardTellsDialogsApart(t *testing.T) {
	cases := []struct {
		name, start, then string
		question          bool
		closes            bool
	}{
		{"another dialog", "permission-screen.txt", "edit-dialog.txt", false, true},
		// A picker of several questions stays one picker across its tabs
		// and its review screen: moving there at the Mac is no answer.
		{"the next question", "picker-multi-q1.txt", "picker-multi-q2.txt", true, false},
		{"the next question, nothing answered", "picker-multi-q1.txt", "picker-multi-q2-untouched.txt", true, false},
		{"the review screen", "picker-multi-q1.txt", "picker-multi-review.txt", true, false},
		{"the review screen, nothing answered", "picker-multi-q1.txt", "picker-multi-review-none.txt", true, false},
		{"the review screen, all answered", "picker-multi-q1.txt", "picker-multi-review-all.txt", true, false},
		{"another picker", "picker-multi-q1.txt", "picker-single.txt", true, true},
		{"a tick at the Mac", "picker-multi-q1.txt", "picker-multi-q1-ticked.txt", true, false},
		{"the cursor moves", "picker-single.txt", "picker-single-cursor.txt", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWaitHarness(t)
			w.deps.Deadline = time.Minute
			in := load(t, "permission_request_bash.json")
			switch {
			case tc.question && strings.HasPrefix(tc.start, "picker-single"):
				in = questionInput(t, `{"questions":[{"question":"Which fruit?","header":"Fruit","options":[{"label":"Apple","description":""}],"multiSelect":false}]}`)
			case tc.question:
				in = questionInput(t, twoQuestions)
			}
			screen := func(name string) string {
				if tc.question {
					return withHeaders(pickerFor(t, name, "Which fruit?"), "Fruit", "Color") // twoQuestions'
				}
				return strings.ReplaceAll(termScreen(t, name), screenCommand, "rm -rf node_modules")
			}
			w.shown = screen(tc.start)
			w.onSleep = func() {
				if w.sleeps == 2 {
					w.shown = screen(tc.then)
				}
			}
			waitPermission(context.Background(), in, w.deps)
			want := "<i>expired: answer at the Mac</i>"
			if tc.closes {
				want = "<i>answered at the Mac</i>"
			}
			if len(w.edits) == 0 || !strings.HasSuffix(w.edits[0].html, want) {
				t.Fatalf("edits=%+v, want %q", w.edits, want)
			}
		})
	}
}

// The owner moves through a picker of two questions at the Mac: the second
// question, the review screen, back to the first. The phone's messages stay
// open; once the picker is gone (submitted), they say "answered at the Mac"
// within two screen reads. Real screens of one picker throughout.
func TestWaitMultiQuestionPickerClosesOnlyWhenGone(t *testing.T) {
	w := newWaitHarness(t)
	const probe = `{"questions":[` +
		`{"question":"Which colours do you like for the probe?","header":"Colours","options":[{"label":"Red","description":"Warm"},{"label":"Green","description":"Fresh"},{"label":"Blue","description":"Calm"}],"multiSelect":true},` +
		`{"question":"Which pet suits the probe?","header":"Pet","options":[{"label":"Cat","description":"Independent"},{"label":"Dog","description":"Loyal"}],"multiSelect":false}]}`
	in := questionInput(t, probe)
	w.shown = termScreen(t, "picker-multi-q1.txt")
	var goneAt time.Time
	w.onSleep = func() {
		switch w.sleeps {
		case 3:
			w.shown = termScreen(t, "picker-multi-q2-untouched.txt")
		case 10:
			w.shown = termScreen(t, "picker-multi-review-none.txt")
		case 20:
			w.shown = termScreen(t, "picker-multi-q1-ticked.txt")
		case 30:
			w.shown = termScreen(t, "picker-multi-review-all.txt")
		case 40:
			goneAt, w.shown = w.now, termScreen(t, "idle-screen.txt")
		}
		if goneAt.IsZero() && len(w.edits) > 0 {
			t.Errorf("closed at sleep %d, before the picker went: %+v", w.sleeps, w.edits)
		}
	}
	if out := waitPermission(context.Background(), in, w.deps); out != nil {
		t.Fatalf("out=%s", out)
	}
	if len(w.rec.Calls) != 2 {
		t.Fatalf("calls=%d, want one message per question", len(w.rec.Calls))
	}
	if goneAt.IsZero() || w.now.Sub(goneAt) > 6*time.Second || len(w.edits) != 2 ||
		!strings.HasSuffix(w.edits[0].html, "<i>answered at the Mac</i>") || !strings.HasSuffix(w.edits[1].html, "<i>answered at the Mac</i>") {
		t.Fatalf("closed %s after the picker went, edits=%+v", w.now.Sub(goneAt), w.edits)
	}
}

// The commit after stdout took the decision waits at most 1 s for the
// ask's lock (ruling R44), then gives up: Claude Code has the answer, and
// the relay or its sweep finishes the ask.
func TestWaitCommitWaitsAtMostASecond(t *testing.T) {
	w := newWaitHarness(t)
	var logs []string
	w.deps.Log = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	w.onSleep = func() {
		if w.sleeps == 1 {
			a := w.openAsk(t)
			w.askID = a.ID
			_, _ = w.store.UpdateAsk(a.ID, func(a *state.Ask) error {
				a.Decision, a.Done = &state.Decision{Behavior: "allow", Suggestion: -1}, true
				return nil
			})
		}
	}
	out, commit := WaitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
	if out == nil || commit == nil {
		t.Fatal("no decision")
	}
	held, free, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_ = w.store.WithAsk(w.id(t), func(*state.Ask) error {
			close(held)
			select {
			case <-free:
			case <-time.After(15 * time.Second):
			}
			return nil
		})
	}()
	<-held
	start := time.Now()
	commit()
	took := time.Since(start)
	close(free)
	<-done
	if took < time.Second || took > 1300*time.Millisecond {
		t.Fatalf("the commit took %s, want about 1s", took)
	}
	if a, err := w.store.LoadAsk(w.id(t)); err != nil || a.Collected {
		t.Fatalf("ask %+v, %v; want it left, not collected", a, err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "mark collected") {
		t.Fatalf("the give-up is not logged: %q", logs)
	}
}

// probeQuestions is the two-question call of the recorded multi-question
// pickers, its first question replaced by first.
func probeQuestions(first string) string {
	return `{"questions":[` +
		`{"question":"` + first + `","header":"Colours","options":[{"label":"Red","description":"Warm"},{"label":"Green","description":"Fresh"},{"label":"Blue","description":"Calm"}],"multiSelect":true},` +
		`{"question":"Which pet suits the probe?","header":"Pet","options":[{"label":"Cat","description":"Independent"},{"label":"Dog","description":"Loyal"}],"multiSelect":false}]}`
}

// Two calls whose first questions agree in their first 40 characters (the
// key) both match one picker on the pane. The claim is the screen's alone,
// the same whichever hook reads it: the first hook holds it, the second
// finds it held and leaves its prompt to the plain alert.
func TestWaitTwoCallsAlikeClaimOnePicker(t *testing.T) {
	const qa, qb = "Which colours do you like for the probe? (first call)", "Which colours do you like for the probe? (second call)"
	screen := pickerFor(t, "picker-multi-q1.txt", qa)
	a := newWaitHarness(t)
	a.shown = screen
	b := newWaitHarness(t)
	b.store, b.deps.Store, b.shown = a.store, a.store, screen
	var bOut []byte
	a.onSleep = func() {
		if a.sleeps == 1 { // a holds the picker and waits; b's call comes now
			bOut = waitPermission(context.Background(), questionInput(t, probeQuestions(qb)), b.deps)
			a.mac = true
		}
	}
	waitPermission(context.Background(), questionInput(t, probeQuestions(qa)), a.deps)
	if len(a.rec.Calls) != 2 {
		t.Fatalf("first call: %d messages, want 2", len(a.rec.Calls))
	}
	if bOut != nil || len(b.rec.Calls) != 0 {
		t.Fatalf("second call claimed the first one's picker: %d messages", len(b.rec.Calls))
	}
}

// Reads of the recorded two-question picker that do not hold it whole: its
// tab bar clipped at the pane's edge, scrolled to the active tab (a header
// left out), its top above the read, its review screen cut.
func partialPickers(t *testing.T) map[string]string {
	t.Helper()
	swap := func(s, old, new string) string {
		if !strings.Contains(s, old) {
			t.Fatalf("%q not on the screen", old)
		}
		return strings.Replace(s, old, new, 1)
	}
	tail := func(s string, n int) string {
		lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
		return strings.Join(lines[len(lines)-n:], "\n") + "\n"
	}
	const bar = "←  ☐ Colours  ☐ Pet  ✔ Submit  →"
	return map[string]string{
		"clipped tab bar":      swap(termScreen(t, "picker-multi-q1.txt"), bar, "←  ☐ Colours  ☐ Pet  ✔ Sub"),
		"scrolled tab bar":     swap(termScreen(t, "picker-multi-q2-untouched.txt"), bar, "←  ☐ Pet  ✔ Submit  →"),
		"taller than the read": tail(termScreen(t, "picker-multi-q2-untouched.txt"), 11),
		"review cut":           tail(termScreen(t, "picker-multi-review-none.txt"), 5),
		// Cropped above the question, no top border in the read.
		"cropped at the tab bar":    tail(termScreen(t, "picker-multi-q2-untouched.txt"), 13),
		"cropped under the tab bar": tail(termScreen(t, "picker-multi-q2-untouched.txt"), 12),
		"cropped over the tab bar":  "\n" + tail(termScreen(t, "picker-multi-q2-untouched.txt"), 13),
	}
}

// While the phone waits, a read that does not hold the picker whole is no
// answer: it says nothing (unknown), the hook keeps waiting, and the
// messages close only once the picker is gone.
func TestWaitIncompletePickerReadsKeepWaiting(t *testing.T) {
	for name, partial := range partialPickers(t) {
		t.Run(name, func(t *testing.T) {
			w := newWaitHarness(t)
			w.shown = termScreen(t, "picker-multi-q1.txt")
			var goneAt time.Time
			w.onSleep = func() {
				switch w.sleeps {
				case 3:
					w.shown = partial
				case 30:
					goneAt, w.shown = w.now, termScreen(t, "idle-screen.txt")
				}
				if goneAt.IsZero() && len(w.edits) > 0 {
					t.Errorf("closed at sleep %d on a partial read: %+v", w.sleeps, w.edits)
				}
			}
			waitPermission(context.Background(), questionInput(t, probeQuestions("Which colours do you like for the probe?")), w.deps)
			if len(w.rec.Calls) != 2 || goneAt.IsZero() || w.now.Sub(goneAt) > 6*time.Second || len(w.edits) != 2 ||
				!strings.HasSuffix(w.edits[0].html, "<i>answered at the Mac</i>") {
				t.Fatalf("calls=%d, closed %s after the picker went, edits=%+v", len(w.rec.Calls), w.now.Sub(goneAt), w.edits)
			}
		})
	}
}

// A picker the hook cannot read whole is never adopted: without its whole
// set signature (every question's header in the tab bar, under the top
// border) the hook could not tell it apart later. The prompt keeps the
// plain alert. A single question's picker taller than the read neither.
func TestWaitIncompletePickerNotAdopted(t *testing.T) {
	cases := partialPickers(t)
	delete(cases, "review cut") // a picker is first drawn on its first question
	tail := func(s string, n int) string {
		lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
		return strings.Join(lines[len(lines)-n:], "\n") + "\n"
	}
	cases["first question cropped at the tab bar"] = tail(termScreen(t, "picker-multi-q1.txt"), 16)
	cases["first question cropped under the tab bar"] = tail(termScreen(t, "picker-multi-q1.txt"), 15)
	single := termScreen(t, "picker-single.txt")
	lines := strings.Split(single, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "Which fruit") {
			cases["a single question taller than the read"] = strings.Join(lines[i:], "\n")
		}
	}
	for name, screen := range cases {
		t.Run(name, func(t *testing.T) {
			w := newWaitHarness(t)
			w.shown = screen
			in := questionInput(t, probeQuestions("Which colours do you like for the probe?"))
			if strings.Contains(name, "single") {
				in = questionInput(t, `{"questions":[{"question":"`+singleQuestion+`","header":"Fruit","options":[{"label":"Apple","description":""}],"multiSelect":false}]}`)
			}
			if strings.Contains(name, "taller") && !strings.Contains(name, "single") {
				// its top shows the second question; give the call that one first
				in = questionInput(t, `{"questions":[{"question":"Which pet suits the probe?","header":"Pet","options":[{"label":"Cat","description":""}],"multiSelect":false},`+
					`{"question":"Which colours do you like for the probe?","header":"Colours","options":[{"label":"Red","description":""}],"multiSelect":true}]}`)
			}
			if out := waitPermission(context.Background(), in, w.deps); out != nil {
				t.Fatalf("out=%s", out)
			}
			if len(w.rec.Calls) != 0 || w.asksLeft() != 0 {
				t.Fatalf("calls=%d asks=%d, want no buttons", len(w.rec.Calls), w.asksLeft())
			}
		})
	}
}

// A complete set names every question's header in the tab bar, exactly and
// in order, repeats included: headers that are substrings of one another
// (Mode, Mode2) or repeated (Choice, Choice) are not proven by a bar that
// shows fewer tabs (one scrolled to the active tab). Such a bar is never
// adopted and, while the phone waits, is no sign the picker went.
func TestWaitTabBarMustNameEveryHeaderExactly(t *testing.T) {
	call := func(h1, h2 string) string {
		return `{"questions":[` +
			`{"question":"Which colours do you like for the probe?","header":"` + h1 + `","options":[{"label":"Red","description":""}],"multiSelect":true},` +
			`{"question":"Which pet suits the probe?","header":"` + h2 + `","options":[{"label":"Cat","description":""}],"multiSelect":false}]}`
	}
	q1 := termScreen(t, "picker-multi-q1.txt")
	bar := func(b string) string { return tabBarRe.ReplaceAllLiteralString(q1, b) }
	for _, tc := range []struct {
		name, h1, h2, bar string
		adopted           bool
	}{
		{"Mode and Mode2, one tab shown", "Mode", "Mode2", "←  ☐ Mode2  ✔ Submit  →", false},
		{"Choice twice, one tab shown", "Choice", "Choice", "←  ☐ Choice  ✔ Submit  →", false},
		{"Choice twice, both shown", "Choice", "Choice", "←  ☐ Choice  ☐ Choice  ✔ Submit  →", true},
		{"Mode and Mode2, both shown", "Mode", "Mode2", "←  ☐ Mode  ☐ Mode2  ✔ Submit  →", true},
		{"in another order", "Mode", "Mode2", "←  ☐ Mode2  ☐ Mode  ✔ Submit  →", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWaitHarness(t)
			w.shown = bar(tc.bar)
			w.onSleep = func() {
				if len(w.rec.Calls) > 0 {
					w.mac = true
				}
			}
			waitPermission(context.Background(), questionInput(t, call(tc.h1, tc.h2)), w.deps)
			if got := len(w.rec.Calls) > 0; got != tc.adopted {
				t.Fatalf("adopted = %v, want %v", got, tc.adopted)
			}
		})
	}
	// Adopted with the whole bar, then read scrolled to one tab: unknown.
	w := newWaitHarness(t)
	w.shown = bar("←  ☐ Mode  ☐ Mode2  ✔ Submit  →")
	w.deps.Deadline = time.Minute
	w.onSleep = func() {
		if w.sleeps == 3 {
			w.shown = bar("←  ☐ Mode2  ✔ Submit  →")
		}
	}
	waitPermission(context.Background(), questionInput(t, call("Mode", "Mode2")), w.deps)
	if len(w.edits) == 0 || !strings.HasSuffix(w.edits[0].html, "<i>expired: answer at the Mac</i>") {
		t.Fatalf("edits=%+v, want the wait to run to its deadline", w.edits)
	}
}

// One read without the dialog (a redraw) is not an answer, and an
// unreadable pane is unknown: keep waiting, log it once.
func TestWaitScreenGlitchesKeepWaiting(t *testing.T) {
	w := newWaitHarness(t)
	w.deps.Deadline = time.Minute
	var logs []string
	w.deps.Log = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	dialog := w.shown
	w.onSleep = func() {
		switch w.sleeps {
		case 5:
			w.shown = termScreen(t, "idle-screen.txt")
		case 6:
			w.shown = dialog
		case 20:
			w.screenErr = errors.New("agtermctl session text: timeout")
		}
	}
	waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
	if len(w.edits) != 1 || !strings.HasSuffix(w.edits[0].html, "<i>expired: answer at the Mac</i>") {
		t.Fatalf("edits=%+v", w.edits)
	}
	n := 0
	for _, l := range logs {
		if strings.Contains(l, "screen:") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("screen errors logged %d times:\n%s", n, strings.Join(logs, "\n"))
	}
}

// A secret right before a closing tag: the hook's HTML is redacted between
// its tags, so the masked value never swallows "</code>" in what is sent,
// stored as the base of later edits, or edited in.
func TestWaitRedactionKeepsTags(t *testing.T) {
	w := newWaitHarness(t)
	in := load(t, "permission_request_bash.json")
	in.ToolInput = json.RawMessage(`{"command":"mysql -u root --password=s3cr3tvalue"}`)
	w.shown = dialogFor(t, "mysql -u root --password=s3cr3tvalue")
	const want = "🔐 <b>acme-shop-92</b>\nneeds permission: <code>Bash: mysql -u root --password=[redacted]</code>"
	w.onSleep = func() {
		if w.sleeps == 1 {
			if a := w.openAsk(t); len(a.Texts) != 1 || a.Texts[0] != want {
				t.Errorf("stored=%q", a.Texts)
			}
			w.mac = true
		}
	}
	waitPermission(context.Background(), in, w.deps)
	if len(w.rec.Calls) != 1 || w.rec.Calls[0].Text != want {
		t.Fatalf("calls=%+v", w.rec.Calls)
	}
	if len(w.edits) != 1 || w.edits[0].html != want+"\n\n<i>answered at the Mac</i>" {
		t.Fatalf("edits=%+v", w.edits)
	}
}

// Each offered suggestion's button shows only its number: the message lists
// what each number grants, from the same suggestions as the buttons.
func TestWaitListsSuggestionsInMessage(t *testing.T) {
	w := newWaitHarness(t)
	in := load(t, "permission_request_bash.json")
	in.PermissionSuggestions = json.RawMessage(`[` +
		`{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"rm -rf node_modules"}],"behavior":"allow","destination":"localSettings"},` +
		`{"type":"setMode","mode":"acceptEdits","destination":"session"},` +
		`{"type":"addDirectories","directories":["/tmp/x"],"destination":"session"}]`)
	w.onSleep = func() { w.mac = true }
	waitPermission(context.Background(), in, w.deps)
	if len(w.rec.Calls) != 1 {
		t.Fatalf("calls=%d", len(w.rec.Calls))
	}
	c := w.rec.Calls[0]
	const want = "🔐 <b>acme-shop-92</b>\nneeds permission: <code>Bash: rm -rf node_modules</code>\n" +
		"① Yes, always allow Bash(rm -rf node_modules) (this project, this Mac)\n" +
		"② Yes, allow access to /tmp/x for this session"
	if c.Text != want {
		t.Fatalf("text=%q\nwant %q", c.Text, want)
	}
	kb := c.InlineKeyboard
	if len(kb) != 3 || kb[1][0].Text != "①  Yes, always" || !strings.HasSuffix(kb[1][0].Data, ":0:s0") ||
		kb[2][0].Text != "②  Yes, this session" || !strings.HasSuffix(kb[2][0].Data, ":0:s2") {
		t.Fatalf("keyboard=%+v", kb)
	}
}

// The ask file never holds the dialog's screen text, only its fingerprint:
// a command line can carry a password the summary masks.
func TestWaitStoresOnlyTheSignaturesFingerprint(t *testing.T) {
	w := newWaitHarness(t)
	const cmd = "mysql -u root --password=hunter2secret appdb"
	in := load(t, "permission_request_bash.json")
	in.ToolInput = json.RawMessage(`{"command":"` + cmd + `"}`)
	w.shown = dialogFor(t, cmd)
	var file string
	w.onSleep = func() {
		if w.sleeps == 1 {
			a := w.openAsk(t)
			b, err := os.ReadFile(filepath.Join(w.store.Dir, "asks", a.ID+".json"))
			if err != nil {
				t.Fatal(err)
			}
			file = string(b)
			if a.Signature != state.Fingerprint(term.PermissionSignature(w.shown)) {
				t.Errorf("signature %q", a.Signature)
			}
		}
		if len(w.rec.Calls) > 0 {
			w.mac = true
		}
	}
	waitPermission(context.Background(), in, w.deps)
	if file == "" || strings.Contains(file, "hunter2secret") {
		t.Fatalf("the ask file holds the password:\n%s", file)
	}
	if len(w.rec.Calls) != 1 || len(w.edits) != 1 {
		t.Fatalf("calls=%d edits=%d: the screen guard must still recognise the dialog", len(w.rec.Calls), len(w.edits))
	}
}

// An MCP ask stores its keys (ruling P27) cleaned like every stored text:
// an argument holding a credential is masked, the other keys are kept.
func TestWaitStoresMCPKeysMasked(t *testing.T) {
	const tool = "mcp__claude_ai_AccuWeather__widgets-search-claude"
	// A bot token is longer than a key keeps of a value (keyLen runes): the
	// shortened secret would no longer match what cleaning masks.
	const token = "123456789:AAHfakeTokenForTheTestOnlyQwErTyUiOpAsDfGh"
	for name, secret := range map[string]string{"a credential": "password=hunter2secret", "the bot token": token} {
		t.Run(name, func(t *testing.T) {
			w := newWaitHarness(t)
			w.deps.Sender.Secrets = []string{token}
			w.shown = strings.Replace(termScreen(t, "mcp-dialog.txt"), `query: "Paris, France" }`, `query: "Paris, France", auth: "`+secret+`" }`, 1)
			var file string
			var keys []string
			w.onSleep = func() {
				if w.sleeps == 1 {
					a := w.openAsk(t)
					b, err := os.ReadFile(filepath.Join(w.store.Dir, "asks", a.ID+".json"))
					if err != nil {
						t.Fatal(err)
					}
					file, keys = string(b), a.Keys
				}
				if len(w.rec.Calls) > 0 {
					w.mac = true
				}
			}
			waitPermission(context.Background(), mcpInput(t, tool, `{"queryParams":{"query":"Paris, France","auth":"`+secret+`"}}`), w.deps)
			if file == "" || strings.Contains(file, secret[:20]) {
				t.Fatalf("the ask file holds the secret:\n%s", file)
			}
			if keys != nil {
				t.Fatalf("keys=%q, want none for an input holding a secret", keys)
			}
			if s, _ := w.store.LoadSession(sid); s.PendingPermission == nil || strings.Contains(s.PendingPermission.Summary, secret[:20]) {
				t.Fatalf("the session record holds the secret: %+v", s.PendingPermission)
			}
			if len(w.rec.Calls) != 1 {
				t.Fatalf("calls=%d: the hook must still find its dialog", len(w.rec.Calls))
			}
		})
	}
	// An input without a secret keeps its keys.
	w := newWaitHarness(t)
	in := mcpInput(t, tool, `{"queryParams":{"query":"Paris, France"}}`)
	if keys := askKeys(callKey(tool, in.ToolInput, ""), in.ToolInput, w.deps.Sender.Clean); !slices.Contains(keys, `query:"Paris, France"`) || !slices.Contains(keys, "(MCP)") {
		t.Fatalf("keys=%q, want the plain ones kept", keys)
	}
}

// The phone's answer is collected only once Claude Code has it: the hook
// returns the decision with a commit that the hook command runs after
// writing it to stdout. Until then the ask stays, answered and not
// collected; when the write fails the commit never runs, and the relay's
// sweep reports the answer as not delivered once the hook is gone.
func TestWaitCollectsOnlyOnCommit(t *testing.T) {
	for _, delivered := range []bool{true, false} {
		w := newWaitHarness(t)
		w.onSleep = func() {
			if w.sleeps == 1 {
				a := w.openAsk(t)
				w.askID = a.ID
				if _, err := w.store.UpdateAsk(a.ID, func(a *state.Ask) error {
					a.Decision = &state.Decision{Behavior: "allow", Suggestion: -1}
					a.Done = true
					return nil
				}); err != nil {
					t.Error(err)
				}
			}
		}
		out, commit := WaitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
		if out == nil || commit == nil {
			t.Fatalf("out=%s commit=%v, want both", out, commit != nil)
		}
		a, err := w.store.LoadAsk(w.id(t))
		if err != nil || !a.Done || a.Collected {
			t.Fatalf("before the commit: ask %+v, %v; want it answered and not collected", a, err)
		}
		if !delivered {
			continue // the write failed: no commit
		}
		commit()
		// The relay has not shown the answer on the phone yet (Settled):
		// the ask stays, collected, until it has.
		if a, err := w.store.LoadAsk(w.id(t)); err != nil || !a.Collected {
			t.Fatalf("after the commit: ask %+v, %v; want it collected and kept for the relay", a, err)
		}
	}
	// The relay showed the answer first: the commit removes the ask.
	w := newWaitHarness(t)
	w.onSleep = func() {
		if w.sleeps == 1 {
			_, _ = w.store.UpdateAsk(w.openAsk(t).ID, func(a *state.Ask) error {
				a.Decision, a.Done, a.Settled = &state.Decision{Behavior: "allow", Suggestion: -1}, true, true
				return nil
			})
		}
	}
	if out := waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps); out == nil || w.asksLeft() != 0 {
		t.Fatalf("out=%s asks=%d", out, w.asksLeft())
	}
	// No decision, no commit.
	w = newWaitHarness(t)
	w.onSleep = func() { w.mac = true }
	if out, commit := WaitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps); out != nil || commit != nil {
		t.Fatalf("out=%s commit=%v", out, commit != nil)
	}
}

func (w *waitHarness) id(t *testing.T) string {
	t.Helper()
	if w.askID == "" {
		t.Fatal("no ask yet")
	}
	return w.askID
}

// Ruling R30: the hook adopts only a dialog or picker that shows its own
// call: the Bash command, the file's name, the fetched host, the first
// question. Another call's dialog (queued, or another session's in a
// shared pane) never gets buttons; with none of its own within 3 s the
// prompt gets the plain alert. Real screens throughout.
func TestWaitAdoptsOnlyItsOwnDialog(t *testing.T) {
	cases := []struct {
		name, tool, input, screen string
		own                       bool
		cwd                       string
	}{
		{"its Bash command", "Bash", `{"command":"command -v tg && tg follow on"}`, "permission-screen.txt", true, ""},
		{"another Bash command", "Bash", `{"command":"rm -rf node_modules"}`, "permission-screen.txt", false, ""},
		{"its Edit", "Edit", `{"file_path":"/Users/alex/probe/notes.txt","old_string":"world","new_string":"there"}`, "edit-dialog.txt", true, "/Users/alex/probe"},
		{"another file's Edit", "Edit", `{"file_path":"/Users/alex/probe/other.txt","old_string":"a","new_string":"b"}`, "edit-dialog.txt", false, "/Users/alex/probe"},
		{"its Write", "Write", `{"file_path":"/Users/alex/probe/fresh.txt","content":"probe\n"}`, "write-dialog.txt", true, "/Users/alex/probe"},
		{"an Edit beside a Write dialog", "Edit", `{"file_path":"/Users/alex/probe/notes.txt","old_string":"a","new_string":"b"}`, "write-dialog.txt", false, "/Users/alex/probe"},
		{"its fetch", "WebFetch", `{"url":"https://example.com","prompt":"What is the title of this page?"}`, "webfetch-dialog.txt", true, ""},
		{"another host's fetch", "WebFetch", `{"url":"https://example.org/x","prompt":"title?"}`, "webfetch-dialog.txt", false, ""},
		{"its question", questionTool, `{"questions":[{"question":"` + singleQuestion + `","header":"Fruit","options":[{"label":"Apple","description":""},{"label":"Banana","description":""}],"multiSelect":false}]}`, "picker-single.txt", true, ""},
		{"another question", questionTool, `{"questions":[{"question":"Which pet suits the probe?","header":"Pet","options":[{"label":"Cat","description":""},{"label":"Dog","description":""}],"multiSelect":false}]}`, "picker-multi-q1.txt", false, ""},
		// Ruling R30a: a command wider than 80 columns or with a newline
		// sits in a "│" gutter.
		{"its long Bash command", "Bash", `{"command":"touch sigcheck-gutter-alpha.txt sigcheck-gutter-bravo.txt sigcheck-gutter-charlie.txt sigcheck-gutter-delta-e.txt"}`, "bash-gutter-86.txt", true, ""},
		{"its heredoc commit", "Bash", `{"command":"git commit -m \"$(cat <<'EOF'\nsigcheck heredoc probe\n\nSigned-off-by: Alex Doe <alex@example.com>\nEOF\n)\""}`, "bash-heredoc-dialog.txt", true, ""},
		// Ruling R30b: the path and the url as the dialog shows them.
		{"its Edit in a subdirectory", "Edit", `{"file_path":"/Users/alex/probe/sub/notes.txt","old_string":"world","new_string":"there"}`, "edit-subdir-dialog.txt", true, "/Users/alex/probe"},
		{"another directory's notes.txt", "Edit", `{"file_path":"/Users/alex/probe/other/notes.txt","old_string":"world","new_string":"there"}`, "edit-subdir-dialog.txt", false, "/Users/alex/probe"},
		{"its Write in a subdirectory", "Write", `{"file_path":"/Users/alex/probe/sub/fresh.txt","content":"probe\n"}`, "write-subdir-dialog.txt", true, "/Users/alex/probe"},
		// Round 4: a Write over an existing file shows "Overwrite file" and
		// asks "Do you want to overwrite F?" (recorded).
		{"its Write over an existing file", "Write", `{"file_path":"/Users/alex/probe/notes.txt","content":"probe\n"}`, "write-overwrite-dialog.txt", true, "/Users/alex/probe"},
		{"another file's Write beside an overwrite dialog", "Write", `{"file_path":"/Users/alex/probe/fresh.txt","content":"probe\n"}`, "write-overwrite-dialog.txt", false, "/Users/alex/probe"},
		{"an Edit beside an overwrite dialog", "Edit", `{"file_path":"/Users/alex/probe/notes.txt","old_string":"a","new_string":"b"}`, "write-overwrite-dialog.txt", false, "/Users/alex/probe"},
		{"its Edit outside the project", "Edit", `{"file_path":"/Users/alex/scratch/fix2/outside.txt","old_string":"world","new_string":"there"}`, "edit-outside-dialog.txt", true, "/Users/alex/scratch/sigcheck-proj"},
		{"its Read outside the project", "Read", `{"file_path":"/etc/hosts"}`, "read-outside-dialog.txt", true, "/Users/alex/probe"},
		{"its Read of a file in the home folder", "Read", `{"file_path":"/Users/alex/code/acme-shop/README.md","limit":5}`, "read-home-dialog.txt", true, "/Users/alex/probe"},
		{"a Read of another file in /etc", "Read", `{"file_path":"/etc/hosts.equiv"}`, "read-outside-dialog.txt", false, "/Users/alex/probe"},
		{"its fetch, host in capitals", "WebFetch", `{"url":"https://Example.COM/sigcheck/Some-Long-Path/for-the-probe?alpha=1&beta=two&gamma=three-four-five-six-seven-eight-nine-ten-eleven","prompt":"What is the title of this page?"}`, "webfetch-long-dialog.txt", true, ""},
		{"another page of that host", "WebFetch", `{"url":"https://example.com/sigcheck/other","prompt":"title?"}`, "webfetch-long-dialog.txt", false, ""},
		// Ruling R30b, item 5: a key never matches a longer text that
		// merely starts or ends with it.
		{"a shorter command beside a boxed one", "Bash", `{"command":"touch sigcheck-gutter-alpha.txt"}`, "bash-gutter-86.txt", false, ""},
		{"the top notes.txt beside sub/notes.txt", "Edit", `{"file_path":"/Users/alex/probe/notes.txt","old_string":"world","new_string":"there"}`, "edit-subdir-dialog.txt", false, "/Users/alex/probe"},
		{"a url the dialog's url starts with", "WebFetch", `{"url":"https://example.com/sigcheck/Some-Long-Path/for-the-probe?alpha=1","prompt":"title?"}`, "webfetch-long-dialog.txt", false, ""},
		{"its fetch of a long url", "WebFetch", `{"url":"https://example.org/sigcheck/segment01-segment02-segment03-segment04-segment05-segment06-segment07-segment08-segment09-segment10-segment11-segment12-segment13-segment14-segment15-segment16-segment17-segment18-segment19-segment20-segment21-segment22-segment23-segment24-segment25?q=end","prompt":"What is the page title?"}`, "webfetch-wrapped-dialog.txt", true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWaitHarness(t)
			in := load(t, "permission_request_bash.json")
			in.ToolName, in.ToolInput = c.tool, json.RawMessage(c.input)
			if c.cwd != "" {
				in.Cwd = c.cwd
			}
			w.shown = termScreen(t, c.screen)
			w.onSleep = func() {
				if len(w.rec.Calls) > 0 {
					w.mac = true
				}
			}
			start := w.now
			waitPermission(context.Background(), in, w.deps)
			if c.own {
				if len(w.rec.Calls) != 1 || len(w.rec.Calls[0].InlineKeyboard) == 0 {
					t.Fatalf("calls=%+v, want the button message", w.rec.Calls)
				}
				return
			}
			if len(w.rec.Calls) != 0 || w.asksLeft() != 0 || len(w.edits) != 0 {
				t.Fatalf("calls=%d asks=%d edits=%d", len(w.rec.Calls), w.asksLeft(), len(w.edits))
			}
			if got := w.now.Sub(start); got != 3*time.Second || w.reads != 7 {
				t.Fatalf("gave up after %s and %d reads, want 3s and 7", got, w.reads)
			}
		})
	}
}

// Another call's dialog shows first (a queued prompt); the hook's own one
// comes up within the 3 s window and is the one adopted: nothing is sent
// beside the other one.
func TestWaitAdoptsItsOwnDialogAfterAnother(t *testing.T) {
	w := newWaitHarness(t)
	own := w.shown
	w.shown = termScreen(t, "permission-screen.txt") // command -v tg && tg follow on
	start := w.now
	sentAt := time.Duration(-1)
	w.onSleep = func() {
		if w.sleeps == 3 { // 1.5 s: that one answered, this call's dialog shows
			w.shown = own
		}
		if len(w.rec.Calls) == 1 && sentAt < 0 {
			sentAt = w.now.Sub(start)
			w.mac = true
		}
	}
	waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
	// Adopted at 1.5 s and sent then; first seen by the sleep ending at 2.5 s.
	if len(w.rec.Calls) != 1 || sentAt != 2500*time.Millisecond {
		t.Fatalf("calls=%d, message first seen at %s, want 2.5s", len(w.rec.Calls), sentAt)
	}
}

func TestCallKey(t *testing.T) {
	long := strings.Repeat("x", 30) + " && " + strings.Repeat("y", 30)
	const cwd = "/Users/alex/probe"
	keys := func(whole []string, tall ...string) callKeys { return callKeys{whole: whole, tall: tall} }
	one := func(k ...string) []string { return k }
	cases := []struct {
		tool, input string
		want        callKeys
	}{
		// Between the dialog's ╌ rules ("─" in a signature): the whole
		// command, or its first 40 characters.
		{"Bash", `{"command":"rm -rf node_modules"}`, keys(one("─rm -rf node_modules─"))},
		{"Bash", `{"command":"` + long + `"}`, keys(one("─" + strings.Repeat("x", 30) + " && yyyyyy"))},
		{"Bash", `{"command":"cat <<EOF\n  a\tb\nEOF"}`, keys(one("─cat <<EOF a b EOF─"))},
		// "Edit file" / "Create file", the path as shown, a rule; a dialog
		// taller than the read shows only its question.
		{"Edit", `{"file_path":"/Users/alex/probe/notes.txt"}`, keys(one("Edit file notes.txt ─"), "Do you want to make this edit to notes.txt?")},
		{"Edit", `{"file_path":"/Users/alex/probe/sub/notes.txt"}`, keys(one("Edit file sub/notes.txt ─"), "Do you want to make this edit to notes.txt?")},
		{"Edit", `{"file_path":"/Users/alex/scratch/outside.txt"}`, keys(one("/outside.txt ─"), "Do you want to make this edit to outside.txt?")},
		// Write: a new file, an existing one, a network path (Claude Code
		// 2.1.310: "Create file"/"create", "Overwrite file"/"overwrite",
		// "Write file"/"write to").
		{"Write", `{"file_path":"/Users/alex/probe/sub/fresh.txt"}`, keys(one("Create file sub/fresh.txt ─", "Overwrite file sub/fresh.txt ─", "Write file sub/fresh.txt ─"),
			"Do you want to create fresh.txt?", "Do you want to overwrite fresh.txt?", "Do you want to write to fresh.txt?")},
		// "Read(", the absolute path, then ")" or " · lines …".
		{"Read", `{"file_path":"/etc/hosts"}`, keys(one("Read(/etc/hosts)"))},
		{"Read", `{"file_path":"/Users/alex/code/acme-shop/README.md","limit":5}`, keys(one("Read(/Users/alex/code/acme-shop/README.md ·"))},
		// No recorded dialog: the base name, undelimited.
		{"Read", `{"file_path":"/Users/alex/probe/sub/notes.txt"}`, keys(one("notes.txt"))},
		{"NotebookEdit", `{"notebook_path":"/Users/alex/probe/nb/analysis.ipynb"}`, keys(one("analysis.ipynb"))},
		// "url: " the url as the dialog spells it, then "prompt:"; else
		// "fetch content from" the lower-cased host and a rule.
		{"WebFetch", `{"url":"https://Example.COM/sigcheck/Some-Long-Path/for-the-probe?alpha=1&beta=two"}`, keys(one("url: https://example.com/sigcheck/Some-Long-Path/for-the-probe?alpha=1&beta=two prompt:"))},
		{"WebFetch", `{"url":"https://example.com"}`, keys(one("url: https://example.com/ prompt:"))},
		{"WebFetch", `{"url":"https://example.com:443/a"}`, keys(one("url: https://example.com/a prompt:"))},
		{"WebFetch", `{"url":"https://example.com:8443/a?b=c"}`, keys(one("url: https://example.com:8443/a?b=c prompt:"))},
		// A "'" in the query: the WHATWG form spells it %27; either matches.
		{"WebFetch", `{"url":"https://example.com/a?q=it's"}`, keys(one("url: https://example.com/a?q=it's prompt:", "url: https://example.com/a?q=it%27s prompt:"))},
		{"WebFetch", `{"url":"http://Example.com/a"}`, keys(one("fetch content from example.com ─"))},
		{"WebFetch", `{"url":"https://user:pw@example.com/a"}`, keys(one("fetch content from example.com ─"))},
		{"WebFetch", `{"url":"https://example.com/a b"}`, keys(one("fetch content from example.com ─"))},
		{"WebFetch", `{"url":"https://example.com/caf\u00e9"}`, keys(one("fetch content from example.com ─"))},
		{"WebFetch", `{"url":"https://B\u00fccher.de/x"}`, keys(one("fetch content from bücher.de ─"))},
		{questionTool, `{"questions":[{"question":"Which address should get the alerts on the production site?"},{"question":"Second?"}]}`, keys(one("Which address should get the alerts on t"))},
		{"Bash", `{"command":""}`, callKeys{}},
		{"Edit", `{}`, callKeys{}},
		{"WebFetch", `{"url":"not a url"}`, callKeys{}},
		{questionTool, `{"questions":[]}`, callKeys{}},
		// An MCP tool (ruling P12): "(MCP)" and every leaf of the input,
		// nested ones too: a string in quotes (from 40 characters on, an
		// opening quote and its first 40), a whole number or true/false
		// as written; under a plain identifier, its name and a colon in
		// front; an array item or a value under another name, alone.
		// Numbers and booleans never tie a call to a dialog alone: no
		// string, no keys (P21).
		{"mcp__linear__save_issue", `{"title":"Fix the login"}`, callKeys{every: one("(MCP)", `title:"Fix the login"`)}},
		{"mcp__claude_ai_AccuWeather__widgets-search-claude", `{"queryParams":{"query":"Paris, France"}}`, callKeys{every: one("(MCP)", `query:"Paris, France"`)}},
		{"mcp__x__y", `{"a":{"b":["one",{"c":"two"}]},"z":"last"}`, callKeys{every: one("(MCP)", `"one"`, `c:"two"`, `z:"last"`)}},
		{"mcp__x__y", `{"q":"` + strings.Repeat("a", 50) + `"}`, callKeys{every: one("(MCP)", `q:"`+strings.Repeat("a", 40))}},
		{"mcp__x__y", `{"q":"` + strings.Repeat("é", 39) + `"}`, callKeys{every: one("(MCP)", `q:"`+strings.Repeat("é", 39)+`"`)}},
		{"mcp__x__y", `{"days":5}`, callKeys{}},
		{"mcp__x__y", `{"force":true,"dry_run":false,"$top":-12}`, callKeys{}},
		{"mcp__x__y", `{"q":"x","days":5}`, callKeys{every: one("(MCP)", "days:5", `q:"x"`)}},
		{"mcp__x__y", `{"q":"x","force":true,"dry_run":false,"$top":-12}`, callKeys{every: one("(MCP)", "$top:-12", "dry_run:false", "force:true", `q:"x"`)}},
		{"mcp__x__y", `{"ids":[5,7],"on":true}`, callKeys{}},
		{"mcp__x__y", `{"ids":[5,true,"x",1.5,null]}`, callKeys{every: one("(MCP)", "5", "true", `"x"`)}},
		{"mcp__x__y", `{"my-key":"v","2nd":7,"ok":"w"}`, callKeys{every: one("(MCP)", "7", `"v"`, `ok:"w"`)}},
		// A value with a quote, a backslash or a line break is left out,
		// and so is a float, a null, an empty object or array.
		{"mcp__x__y", `{"a":"say \"hi\"","b":"c:\\d","c":"one\ntwo","d":"cr\rx","e":"kept"}`, callKeys{every: one("(MCP)", `e:"kept"`)}},
		{"mcp__x__y", `{"a":"say \"hi\"","b":"one\ntwo"}`, callKeys{}},
		{"mcp__x__y", `{"f":1.5,"g":1e3,"h":-0.0,"i":2E1,"n":null,"o":{},"l":[]}`, callKeys{}},
		{"mcp__x__y", `{"empty":"  ","null":null}`, callKeys{}},
		{"mcp__x__y", `{}`, callKeys{}},
		{"mcp__x__y", `not json`, callKeys{}},
		{"mcp__x__y", `{"days":5} {"days":6}`, callKeys{}},
		{"Bash", `not json`, callKeys{}},
	}
	for _, c := range cases {
		if got := callKey(c.tool, json.RawMessage(c.input), cwd); fmt.Sprintf("%q", got) != fmt.Sprintf("%q", c.want) {
			t.Errorf("callKey(%s, %s) = %q, want %q", c.tool, c.input, got, c.want)
		}
	}
	// Without a cwd a path cannot be made relative: the base name, as for
	// a file outside it.
	if got := callKey("Edit", json.RawMessage(`{"file_path":"/Users/alex/probe/sub/notes.txt"}`), ""); fmt.Sprint(got.whole) != "[/notes.txt ─]" {
		t.Errorf("no cwd: %q", got)
	}
}

// A pane that cannot be read just before sending says nothing about the
// dialog: nothing is sent blind. The send waits for the next away check
// (5 s later, no re-reads in between) and goes once a read shows the
// dialog again; the error is logged once.
func TestWaitScreenErrorHoldsTheSend(t *testing.T) {
	w := newWaitHarness(t)
	start := w.now
	var logs []string
	w.deps.Log = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	var reads []time.Duration
	w.deps.Screen = func(context.Context) (string, error) {
		at := w.now.Sub(start)
		reads = append(reads, at)
		if len(reads) > 1 && at < 12*time.Second { // the dialog was seen, then the pane went unreadable
			return "", errors.New("agtermctl session text: timeout")
		}
		return w.shown, nil
	}
	sentAt := time.Duration(-1)
	w.onSleep = func() {
		if len(w.rec.Calls) == 1 && sentAt < 0 {
			sentAt = w.now.Sub(start)
			w.mac = true
		}
	}
	waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
	if len(w.rec.Calls) != 1 || sentAt != 16*time.Second {
		t.Fatalf("calls=%d, message first seen at %s, want it sent at the 15 s away check", len(w.rec.Calls), sentAt)
	}
	if want := "[0s 0s 5s 10s 15s]"; fmt.Sprint(reads[:5]) != want || len(reads) > 5 && reads[5] < 16*time.Second {
		t.Fatalf("pane read at %v, want %s before the send", reads, want)
	}
	n := 0
	for _, l := range logs {
		if strings.Contains(l, "screen:") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("screen errors logged %d times:\n%s", n, strings.Join(logs, "\n"))
	}
}

// Once the message is out, a read error is unknown and keeps the wait; the
// pane is read again 5 s later, not every tick.
func TestWaitScreenErrorAfterSendingReadsEvery5s(t *testing.T) {
	w := newWaitHarness(t)
	w.deps.Deadline = time.Minute
	start := w.now
	var errReads []time.Duration
	w.deps.Screen = func(context.Context) (string, error) {
		if w.screenErr != nil {
			errReads = append(errReads, w.now.Sub(start))
		}
		return w.shown, w.screenErr
	}
	w.onSleep = func() {
		if w.sleeps == 3 {
			w.screenErr = errors.New("agtermctl session text: timeout")
		}
	}
	waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
	if len(w.edits) != 1 || !strings.HasSuffix(w.edits[0].html, "<i>expired: answer at the Mac</i>") {
		t.Fatalf("edits=%+v", w.edits)
	}
	if len(errReads) < 2 {
		t.Fatalf("errored reads at %v", errReads)
	}
	for i := 1; i < len(errReads); i++ {
		if gap := errReads[i] - errReads[i-1]; gap != 5*time.Second {
			t.Fatalf("errored reads at %v, want 5 s apart", errReads)
		}
	}
}

// otherAsk puts an ask of another call of the same session (or of session)
// in the store, made ago before now, holding sig ("" = still looking).
func (w *waitHarness) otherAsk(t *testing.T, session string, ago time.Duration, sig string) *state.Ask {
	t.Helper()
	a := &state.Ask{ID: state.NewAskID(), SessionID: session, Tool: "Bash", HookPID: os.Getpid(),
		Created: w.now.Add(-ago), Deadline: w.now.Add(12 * time.Hour)}
	if sig != "" {
		a.Signature = state.Fingerprint(sig)
	}
	if err := w.store.PutAsk(a); err != nil {
		t.Fatal(err)
	}
	return a
}

// Ruling R30b: keys can collide between concurrent calls (two Reads of one
// file, two fetches of one url, two commands alike in their first 40
// characters). A dialog another unclosed ask of the session holds is never
// adopted: the hook keeps looking for 3 s, then leaves the prompt to the
// plain alert.
func TestWaitRefusesADialogAnotherAskHolds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(w *waitHarness, sig string)
		buttons bool
	}{
		{"held by an older ask", func(w *waitHarness, sig string) { w.otherAsk(t, sid, 10*time.Second, sig) }, false},
		{"held by an answered, uncollected ask", func(w *waitHarness, sig string) {
			a := w.otherAsk(t, sid, 10*time.Second, sig)
			_, _ = w.store.UpdateAsk(a.ID, func(a *state.Ask) error { a.Done = true; return nil })
		}, false},
		{"held by another session's ask", func(w *waitHarness, sig string) { w.otherAsk(t, "another-session", 10*time.Second, sig) }, true},
		{"held by a closed ask", func(w *waitHarness, sig string) {
			a := w.otherAsk(t, sid, 10*time.Second, sig)
			_, _ = w.store.UpdateAsk(a.ID, func(a *state.Ask) error { a.Closed = "mac"; return nil })
		}, true},
		{"another dialog held", func(w *waitHarness, _ string) {
			w.otherAsk(t, sid, 10*time.Second, term.PermissionSignature(termScreen(t, "edit-dialog.txt")))
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWaitHarness(t)
			tc.setup(w, term.PermissionSignature(w.shown))
			w.onSleep = func() {
				if len(w.rec.Calls) > 0 {
					w.mac = true
				}
			}
			start := w.now
			waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
			if tc.buttons {
				if len(w.rec.Calls) != 1 {
					t.Fatalf("calls=%d, want the button message", len(w.rec.Calls))
				}
				return
			}
			if len(w.rec.Calls) != 0 || w.asksLeft() != 1 || w.now.Sub(start) != 3*time.Second || w.reads != 7 {
				t.Fatalf("calls=%d asks=%d after %s and %d reads, want no message, only the other ask, 3s and 7 reads",
					len(w.rec.Calls), w.asksLeft(), w.now.Sub(start), w.reads)
			}
		})
	}
}

// Dialogs show in the order of their calls: while an older ask of the
// session still looks for its dialog (no signature yet), the dialog on the
// pane is the older call's and is left to it.
func TestWaitLeavesTheDialogToAnOlderAskStillLooking(t *testing.T) {
	t.Run("the older one claims it", func(t *testing.T) {
		w := newWaitHarness(t)
		older := w.otherAsk(t, sid, time.Second, "")
		sig := term.PermissionSignature(w.shown)
		w.onSleep = func() {
			if w.sleeps == 2 { // 1 s later: the older hook adopts the dialog
				_, _ = w.store.UpdateAsk(older.ID, func(a *state.Ask) error { a.Signature = state.Fingerprint(sig); return nil })
			}
		}
		waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
		if len(w.rec.Calls) != 0 || w.asksLeft() != 1 || w.reads != 7 {
			t.Fatalf("calls=%d asks=%d reads=%d", len(w.rec.Calls), w.asksLeft(), w.reads)
		}
	})
	t.Run("the older one gives up", func(t *testing.T) {
		w := newWaitHarness(t)
		older := w.otherAsk(t, sid, time.Second, "")
		start := w.now
		sentAt := time.Duration(-1)
		w.onSleep = func() {
			if w.sleeps == 2 { // no dialog of its own on the pane: it removes its ask
				_ = w.store.DeleteAsk(older.ID)
			}
			if len(w.rec.Calls) == 1 && sentAt < 0 {
				sentAt = w.now.Sub(start)
				w.mac = true
			}
		}
		waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
		// Adopted at the read right after the older ask went (1 s); first
		// seen by the sleep that ends at 2 s.
		if len(w.rec.Calls) != 1 || sentAt != 2*time.Second {
			t.Fatalf("calls=%d, message first seen at %s, want 2s", len(w.rec.Calls), sentAt)
		}
	})
	t.Run("an older ask long past its window", func(t *testing.T) {
		w := newWaitHarness(t)
		w.otherAsk(t, sid, time.Minute, "") // a hook that died looking
		w.onSleep = func() { w.mac = true }
		waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
		if len(w.rec.Calls) != 1 {
			t.Fatalf("calls=%d, want the button message", len(w.rec.Calls))
		}
	})
}

// A panic after the phone answered must not drop the answer: the closing
// hands it to Claude Code, as any other close does.
func TestWaitPanicKeepsPhoneAnswer(t *testing.T) {
	w := newWaitHarness(t)
	w.onSleep = func() {
		if w.sleeps == 1 && len(w.rec.Calls) == 1 {
			a := w.openAsk(t)
			if _, err := w.store.UpdateAsk(a.ID, func(a *state.Ask) error {
				a.Decision = &state.Decision{Behavior: "allow", Suggestion: -1}
				a.Done = true
				a.Settled = true // the relay showed the answer
				return nil
			}); err != nil {
				t.Error(err)
			}
			panic("boom")
		}
	}
	out := waitPermission(context.Background(), load(t, "permission_request_bash.json"), w.deps)
	if want := `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`; string(out) != want {
		t.Fatalf("out=%s, want the phone's answer", out)
	}
	if w.asksLeft() != 0 || len(w.edits) != 0 {
		t.Fatalf("asks=%d edits=%+v", w.asksLeft(), w.edits)
	}
}

// Item 5: Read's dialog shows "Read(<path>)", so /etc/hosts never takes
// the dialog of /etc/hosts.equiv (the real Read screen with that path).
func TestWaitReadKeyIsDelimited(t *testing.T) {
	for _, tc := range []struct {
		path    string
		buttons bool
	}{{"/etc/hosts", false}, {"/etc/hosts.equiv", true}} {
		w := newWaitHarness(t)
		in := load(t, "permission_request_bash.json")
		in.ToolName, in.ToolInput, in.Cwd = "Read", json.RawMessage(`{"file_path":"`+tc.path+`"}`), "/Users/alex/probe"
		w.shown = strings.ReplaceAll(termScreen(t, "read-outside-dialog.txt"), "/etc/hosts", "/etc/hosts.equiv")
		w.onSleep = func() { w.mac = true }
		waitPermission(context.Background(), in, w.deps)
		if got := len(w.rec.Calls) == 1; got != tc.buttons {
			t.Errorf("%s: buttons=%v, want %v", tc.path, got, tc.buttons)
		}
	}
}

// Item 4: a "'" in the query is "%27" in the WHATWG form; the key matches
// a dialog showing either.
func TestWaitURLWithQuoteMatchesEitherForm(t *testing.T) {
	const want = "https://example.com/sigcheck/Some-Long-Path/for-the-probe?alpha=1&beta=two&gamma=three-four-five-six-seven-eight-nine-ten-eleven"
	for _, shown := range []string{"it's", "it%27s"} {
		w := newWaitHarness(t)
		in := load(t, "permission_request_bash.json")
		in.ToolName, in.ToolInput = "WebFetch", json.RawMessage(`{"url":"https://example.com/sigcheck/Some-Long-Path/for-the-probe?alpha=1&beta=it's","prompt":"title?"}`)
		w.shown = strings.Replace(termScreen(t, "webfetch-long-dialog.txt"), want, "https://example.com/sigcheck/Some-Long-Path/for-the-probe?alpha=1&beta="+shown, 1)
		w.onSleep = func() { w.mac = true }
		waitPermission(context.Background(), in, w.deps)
		if len(w.rec.Calls) != 1 {
			t.Errorf("dialog showing %q: calls=%d, want the button message", shown, len(w.rec.Calls))
		}
	}
}

// tallRead is a real dialog with 77 diff lines added after the line
// starting with after, read as its last 60 lines: its top border is above
// the read.
func tallRead(t *testing.T, name, after string) string {
	t.Helper()
	out := tallReadOf(termScreen(t, name), after)
	if out == "" {
		t.Fatalf("%s: no line %q", name, after)
	}
	return out
}

// tallReadOf is tallRead of a screen; "" when it has no line starting with
// after.
func tallReadOf(screen, after string) string {
	var lines []string
	for _, l := range strings.Split(screen, "\n") {
		lines = append(lines, l)
		if strings.HasPrefix(l, after) {
			for i := 3; i < 80; i++ {
				lines = append(lines, fmt.Sprintf(" %d +line %d", i, i))
			}
		}
	}
	if len(lines) < 80 {
		return ""
	}
	return strings.Join(lines[len(lines)-60:], "\n")
}

// Item 3: an Edit or Write dialog taller than the read shows only its
// question, which names the file by its base name: a file in a
// subdirectory still gets its buttons.
func TestWaitTallDialogOfAFileInASubdirectory(t *testing.T) {
	for _, tc := range []struct{ tool, input, screen, after string }{
		{"Edit", `{"file_path":"/Users/alex/probe/sub/notes.txt","old_string":"world","new_string":"there"}`, "edit-subdir-dialog.txt", " 2 +there"},
		{"Write", `{"file_path":"/Users/alex/probe/sub/fresh.txt","content":"probe\n"}`, "write-subdir-dialog.txt", "  1 probe"},
		{"Write", `{"file_path":"/Users/alex/probe/notes.txt","content":"probe\n"}`, "write-overwrite-dialog.txt", "  1 +probe"}, // over an existing file
	} {
		w := newWaitHarness(t)
		in := load(t, "permission_request_bash.json")
		in.ToolName, in.ToolInput, in.Cwd = tc.tool, json.RawMessage(tc.input), "/Users/alex/probe"
		w.shown = tallRead(t, tc.screen, tc.after)
		w.onSleep = func() { w.mac = true }
		waitPermission(context.Background(), in, w.deps)
		if len(w.rec.Calls) != 1 {
			t.Errorf("%s: calls=%d, want the button message", tc.tool, len(w.rec.Calls))
		}
	}
}

// Item 1: the plan approval is never adopted, even when a plan line reads
// like an Edit dialog's question and the plan's top is above the read.
func TestWaitNeverAdoptsAPlanApproval(t *testing.T) {
	w := newWaitHarness(t)
	plan := strings.Replace(termScreen(t, "plan-approval.txt"), "\n Steps\n", "\n Do you want to make this edit to notes.txt? Open question.\n\n Steps\n", 1)
	w.shown = strings.Join(strings.Split(plan, "\n")[8:], "\n") // the top border cut off
	in := load(t, "permission_request_bash.json")
	in.ToolName, in.ToolInput, in.Cwd = "Edit", json.RawMessage(`{"file_path":"/Users/alex/probe/notes.txt","old_string":"a","new_string":"b"}`), "/Users/alex/probe"
	waitPermission(context.Background(), in, w.deps)
	if len(w.rec.Calls) != 0 || w.reads != 7 {
		t.Fatalf("calls=%d reads=%d, want no message after 7 reads", len(w.rec.Calls), w.reads)
	}
}

// Round 4: a Write to a network path shows "Write file" and asks "Do you
// want to write to F?" (Claude Code 2.1.310's own strings; no such dialog
// was recorded, so this is the overwrite screen with those words put in).
func TestWaitWriteToANetworkPathGetsButtons(t *testing.T) {
	s := strings.Replace(termScreen(t, "write-overwrite-dialog.txt"), "\n Overwrite file\n", "\n Write file\n", 1)
	s = strings.Replace(s, "Do you want to overwrite notes.txt?", "Do you want to write to notes.txt?", 1)
	if strings.Contains(s, "Overwrite file") || strings.Contains(s, "want to overwrite") || !strings.Contains(s, "Do you want to write to notes.txt?") {
		t.Fatal("screen not changed")
	}
	for name, shown := range map[string]string{"whole": s, "tall": tallReadOf(s, "  1 +probe")} {
		w := newWaitHarness(t)
		in := load(t, "permission_request_bash.json")
		in.ToolName, in.ToolInput, in.Cwd = "Write", json.RawMessage(`{"file_path":"/Users/alex/probe/notes.txt","content":"probe\n"}`), "/Users/alex/probe"
		w.shown = shown
		w.onSleep = func() { w.mac = true }
		waitPermission(context.Background(), in, w.deps)
		if len(w.rec.Calls) != 1 || len(w.rec.Calls[0].InlineKeyboard) == 0 {
			t.Errorf("%s: calls=%d, want the button message", name, len(w.rec.Calls))
		}
	}
}

// An MCP call's dialog (Claude Code 2.1.310, the AccuWeather search for
// "Paris, France") gets buttons when the screen shows "(MCP)" and every
// string of the call; the same screen against another call's arguments
// gets none.
func TestWaitMCPButtons(t *testing.T) {
	const tool = "mcp__claude_ai_AccuWeather__widgets-search-claude"
	for _, c := range []struct {
		name, input string
		own         bool
	}{
		{"its search", `{"queryParams":{"query":"Paris, France"}}`, true},
		{"another search", `{"queryParams":{"query":"Lyon"}}`, false},
		{"one of its strings only", `{"queryParams":{"query":"Paris, France","lang":"fr"}}`, false},
		// A short value is closed by its quote: "Lyon" is not "Lyon, France".
		{"a value the dialog's value starts", `{"queryParams":{"query":"Paris"}}`, false},
		// The argument's name is part of the key (ruling P12).
		{"the same string under another name", `{"queryParams":{"q":"Paris, France"}}`, false},
		{"the same string under a name the dialog's ends with", `{"queryParams":{"ry":"Paris, France"}}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWaitHarness(t)
			in := load(t, "permission_request_bash.json")
			in.ToolName, in.ToolInput = tool, json.RawMessage(c.input)
			in.PermissionSuggestions = json.RawMessage(`[{"type":"addRules","rules":[{"toolName":"` + tool + `"}],"behavior":"allow","destination":"localSettings"}]`)
			w.shown = termScreen(t, "mcp-dialog.txt")
			w.onSleep = func() {
				if len(w.rec.Calls) > 0 {
					w.mac = true
				}
			}
			var logs []string
			w.deps.Log = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
			waitPermission(context.Background(), in, w.deps)
			if !c.own {
				if len(w.rec.Calls) != 0 || w.asksLeft() != 0 {
					t.Fatalf("calls=%d asks=%d, want none", len(w.rec.Calls), w.asksLeft())
				}
				return
			}
			if len(w.rec.Calls) != 1 || len(w.rec.Calls[0].InlineKeyboard) != 2 {
				t.Fatalf("calls=%+v, want the button message with the always option", w.rec.Calls)
			}
			row := w.rec.Calls[0].InlineKeyboard[0]
			if len(row) != 2 || !strings.HasPrefix(row[0].Text, "✅") || !strings.HasPrefix(row[1].Text, "❌") {
				t.Fatalf("first row=%+v, want ✅ and ❌", row)
			}
			if kb := w.rec.Calls[0].InlineKeyboard[1]; !strings.Contains(kb[0].Text, "always") {
				t.Fatalf("second row=%+v, want the always option", kb)
			}
		})
	}
}

// Two MCP calls differing only in a number or a boolean: each hook adopts
// only the dialog that shows its own value, whole: "limit: 5" is not
// "limit: 50", nor "force: true" "force: false".
func TestWaitMCPNumbersAndBooleans(t *testing.T) {
	const tool = "mcp__claude_ai_AccuWeather__widgets-search-claude"
	fixture := termScreen(t, "mcp-dialog.txt")
	const shown = `queryParams: { query: "Paris, France" }`
	if !strings.Contains(fixture, shown) {
		t.Fatal("fixture lacks the arguments to replace")
	}
	for _, c := range []struct {
		name, dialog, input string
		own                 bool
	}{
		{"its limit", `queryParams: { query: "Paris, France", limit: 50 }`, `{"queryParams":{"query":"Paris, France","limit":50}}`, true},
		{"a limit the dialog's starts", `queryParams: { query: "Paris, France", limit: 50 }`, `{"queryParams":{"query":"Paris, France","limit":5}}`, false},
		{"a limit starting the dialog's", `queryParams: { query: "Paris, France", limit: 5 }`, `{"queryParams":{"query":"Paris, France","limit":50}}`, false},
		{"a negative limit", `queryParams: { query: "Paris, France", limit: -5 }`, `{"queryParams":{"query":"Paris, France","limit":5}}`, false},
		{"a float limit", `queryParams: { query: "Paris, France", limit: 5.5 }`, `{"queryParams":{"query":"Paris, France","limit":5}}`, false},
		{"its top-level days", `queryParams: { query: "Paris, France" }, days: 5`, `{"queryParams":{"query":"Paris, France"},"days":5}`, true},
		{"its force", `queryParams: { query: "Paris, France" }, force: true`, `{"queryParams":{"query":"Paris, France"},"force":true}`, true},
		{"another force", `queryParams: { query: "Paris, France" }, force: false`, `{"queryParams":{"query":"Paris, France"},"force":true}`, false},
		{"its id in an array", `queryParams: { query: "Paris, France", ids: [ 7, 12 ] }`, `{"queryParams":{"query":"Paris, France","ids":[7,12]}}`, true},
		{"an id the dialog's starts", `queryParams: { query: "Paris, France", ids: [ 70, 12 ] }`, `{"queryParams":{"query":"Paris, France","ids":[7,12]}}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWaitHarness(t)
			in := load(t, "permission_request_bash.json")
			in.ToolName, in.ToolInput = tool, json.RawMessage(c.input)
			w.shown = strings.Replace(fixture, shown, c.dialog, 1)
			w.onSleep = func() {
				if len(w.rec.Calls) > 0 {
					w.mac = true
				}
			}
			waitPermission(context.Background(), in, w.deps)
			if got := len(w.rec.Calls) == 1; got != c.own {
				t.Fatalf("sent=%v, want %v (keys %q)", got, c.own, callKey(tool, in.ToolInput, ""))
			}
		})
	}
}

// An MCP key is found only standing alone where it starts or ends with a
// word character: a name is not the end of a longer one, a number or a
// boolean not the start of a longer one. A string's key ends at its
// quote; a long one's start is a prefix by design.
func TestHasArg(t *testing.T) {
	for _, c := range []struct {
		sig, key string
		want     bool
	}{
		{`─queryParams:{query:"Paris,France"}─`, `query:"Paris, France"`, true},
		{`─queryParams:{subquery:"Paris,France"}─`, `query:"Paris, France"`, false},
		{`─queryParams:{my-query:"Paris,France"}─`, `query:"Paris, France"`, false},
		{`─a:{limit:50}─`, "limit:5", false},
		{`─a:{limit:5,b:1}─`, "limit:5", true},
		{`─a:{limit:5}─`, "limit:5", true},
		{`─a:{limit:5.5}─`, "limit:5", false},
		{`─a:[5,7]─`, "5", true},
		{`─a:[15,7]─`, "5", false},
		{`─a:[-5,7]─`, "5", false},
		{`─Returnsupto5items─`, "5", false},
		{`─force:true─`, "force:true", true},
		{`─force:trueish─`, "force:true", false},
		{`─q:"aaaa"─`, `"aaa`, true},
		{`─q:"aaaab"─`, `q:"aaaa`, true},
		{`─xq:"aaaab"─`, `q:"aaaa`, false},
		{`─Tool:(MCP)─`, "(MCP)", true},
		{`─a:5─b:limit:5─`, "limit:5", true}, // a later spelling stands alone
		{`─a─`, "", false},
	} {
		if got := hasArg(c.sig, c.key); got != c.want {
			t.Errorf("hasArg(%q, %q) = %v, want %v", c.sig, c.key, got, c.want)
		}
	}
}

// The second capture (Claude Code 2.1.310, the AccuWeather daily
// forecast) prints several arguments inline, comma-separated: each key
// stands alone there, so its own call is adopted; a call differing only in
// one argument is not. A call whose keys are a subset of the dialog's (the
// location key alone) is adopted, as every one of its keys shows: the
// residual the spec names for calls with the same strings.
func TestWaitMCPInlineArguments(t *testing.T) {
	const tool = "mcp__claude_ai_AccuWeather__widgets-daily-claude"
	for _, c := range []struct {
		name, input string
		own         bool
	}{
		{"its forecast", `{"queryParams":{"locationKey":"2608456","unit":"metric","lang":"en"}}`, true},
		{"another unit", `{"queryParams":{"locationKey":"2608456","unit":"imperial","lang":"en"}}`, false},
		{"another location", `{"queryParams":{"locationKey":"2608457","unit":"metric","lang":"en"}}`, false},
		{"the location key alone", `{"queryParams":{"locationKey":"2608456"}}`, true},
		{"its forecast and a day count", `{"queryParams":{"locationKey":"2608456","unit":"metric","lang":"en","days":5}}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWaitHarness(t)
			in := load(t, "permission_request_bash.json")
			in.ToolName, in.ToolInput = tool, json.RawMessage(c.input)
			w.shown = termScreen(t, "mcp-multiarg-dialog.txt")
			w.onSleep = func() {
				if len(w.rec.Calls) > 0 {
					w.mac = true
				}
			}
			waitPermission(context.Background(), in, w.deps)
			if got := len(w.rec.Calls) == 1; got != c.own {
				t.Fatalf("sent=%v, want %v (keys %q)", got, c.own, callKey(tool, in.ToolInput, ""))
			}
		})
	}
}

// Two queued MCP prompts whose values start alike: the hook for "Lyon"
// never adopts the dialog of "Lyon, France"; the hook for "Lyon, France"
// does.
func TestWaitMCPPrefixValues(t *testing.T) {
	const tool = "mcp__claude_ai_AccuWeather__widgets-search-claude"
	screen := strings.Replace(termScreen(t, "mcp-dialog.txt"), `query: "Paris, France"`, `query: "Lyon, France"`, 1)
	if !strings.Contains(screen, "Lyon, France") {
		t.Fatal("fixture lacks the value to replace")
	}
	for _, c := range []struct {
		query string
		own   bool
	}{{"Lyon", false}, {"Lyon, France", true}} {
		w := newWaitHarness(t)
		in := load(t, "permission_request_bash.json")
		in.ToolName, in.ToolInput = tool, json.RawMessage(`{"queryParams":{"query":"`+c.query+`"}}`)
		w.shown = screen
		w.onSleep = func() {
			if len(w.rec.Calls) > 0 {
				w.mac = true
			}
		}
		waitPermission(context.Background(), in, w.deps)
		if got := len(w.rec.Calls) == 1; got != c.own {
			t.Errorf("query %q: sent=%v, want %v", c.query, got, c.own)
		}
	}
}

// The AccuWeather calls of the live test of 2026-10-26: two made at once
// wait on one dialog, the current weather shown first (mcp-queue-1of2.txt),
// the forecast behind it (mcp-queue-2of2.txt once the first is answered).
const (
	currentTool     = "mcp__claude_ai_AccuWeather__widgets-current-claude"
	currentInput    = `{"queryParams":{"locationKey":"2608456","unit":"metric","lang":"en"}}`
	forecastTool    = "mcp__claude_ai_AccuWeather__widgets-todayTonightForecast-claude"
	forecastInput   = `{"queryParams":{"locationKey":"2608456","unit":"metric","lang":"en","days":1}}`
	queueAnsweredIn = time.Minute // the dialog before the call's is answered
)

func mcpInput(t *testing.T, tool, input string) *Input {
	t.Helper()
	in := load(t, "permission_request_bash.json")
	in.ToolName, in.ToolInput = tool, json.RawMessage(input)
	in.PermissionSuggestions = json.RawMessage(`[{"type":"addRules","rules":[{"toolName":"` + tool + `"}],"behavior":"allow","destination":"localSettings"}]`)
	return in
}

// A call queued behind the dialog shown keeps looking past dialogWait
// while the pane shows calls queued, and its buttons go out once its own
// dialog comes up. In the live test the hook gave up after three seconds.
func TestWaitQueuedBehindAnotherCall(t *testing.T) {
	w := newWaitHarness(t)
	w.away = true
	w.shown = termScreen(t, "mcp-queue-1of2.txt")
	start, sentAt := w.now, time.Time{}
	w.onSleep = func() {
		if w.now.Sub(start) >= queueAnsweredIn {
			w.shown = termScreen(t, "mcp-queue-2of2.txt")
		}
		if len(w.rec.Calls) > 0 && sentAt.IsZero() {
			sentAt, w.mac = w.now, true
		}
	}
	var logs []string
	w.deps.Log = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	waitPermission(context.Background(), mcpInput(t, forecastTool, forecastInput), w.deps)
	if len(w.rec.Calls) != 1 || len(w.rec.Calls[0].InlineKeyboard) == 0 || !strings.Contains(w.rec.Calls[0].Text, "todayTonightForecast") {
		t.Fatalf("calls=%+v, want the forecast's button message", w.rec.Calls)
	}
	if got := sentAt.Sub(start); got < queueAnsweredIn || got > queueAnsweredIn+2*time.Second {
		t.Errorf("sent %v after the request, want once its dialog came up at %v", got, queueAnsweredIn)
	}
	if s := strings.Join(logs, "\n"); strings.Contains(s, "no buttons") {
		t.Errorf("logs:\n%s", s)
	}
}

// A queued call whose dialog never comes up gives up dialogWait after the
// pane last showed calls queued (all answered or cancelled at the Mac), and
// one answered at the Mac while queued stops looking at once. Neither sends
// anything, and the ask goes.
func TestWaitQueuedCallNeverShown(t *testing.T) {
	for name, gone := range map[string]func(w *waitHarness){
		"queue gone":          func(w *waitHarness) { w.shown = termScreen(t, "idle-screen.txt") },
		"answered at the Mac": func(w *waitHarness) { w.mac = true },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWaitHarness(t)
			w.away = true
			w.shown = termScreen(t, "mcp-queue-1of2.txt")
			start := w.now
			w.onSleep = func() {
				if w.now.Sub(start) >= queueAnsweredIn {
					gone(w)
				}
			}
			var logs []string
			w.deps.Log = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
			waitPermission(context.Background(), mcpInput(t, forecastTool, forecastInput), w.deps)
			if len(w.rec.Calls) != 0 || w.asksLeft() != 0 {
				t.Fatalf("calls=%d asks=%d, want none", len(w.rec.Calls), w.asksLeft())
			}
			if got := w.now.Sub(start); got < queueAnsweredIn || got > queueAnsweredIn+dialogWait+2*time.Second {
				t.Errorf("gave up %v after the request, want by %v", got, queueAnsweredIn+dialogWait)
			}
			if s := strings.Join(logs, "\n"); !strings.Contains(s, "no buttons") {
				t.Errorf("logs:\n%s", s)
			}
		})
	}
}

// The dialog a hook adopted gains the queue counter when a second call
// joins, and moves it when a call behind it is answered: the dialog is
// still there, and the phone's answer still reaches Claude. In the live
// test the hook closed as answered at the Mac and the tap went nowhere.
func TestWaitDialogJoinedByAQueue(t *testing.T) {
	queued := termScreen(t, "mcp-queue-1of2.txt")
	w := newWaitHarness(t)
	w.away = true
	w.shown = strings.Replace(queued, "1 of 2", "      ", 1)
	var answerAt time.Time
	w.onSleep = func() {
		switch {
		case len(w.rec.Calls) == 0:
		case answerAt.IsZero():
			w.shown, answerAt = queued, w.now.Add(time.Minute)
		case w.now.Before(answerAt):
		default:
			w.shown = strings.Replace(queued, "1 of 2", "2 of 3", 1)
			a := w.openAsk(t)
			if _, err := w.store.UpdateAsk(a.ID, func(a *state.Ask) error {
				a.Decision = &state.Decision{Behavior: "allow", Suggestion: -1}
				a.Done = true
				return nil
			}); err != nil {
				t.Error(err)
			}
		}
	}
	out := waitPermission(context.Background(), mcpInput(t, currentTool, currentInput), w.deps)
	if want := `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`; string(out) != want {
		t.Fatalf("out=%s edits=%+v\nwant %s", out, w.edits, want)
	}
}

// Ruling P27: the current weather's keys are all in the forecast's dialog,
// which shows a day count too. While the forecast's hook still looks for its
// dialog, the current weather's hook leaves it alone and waits for its own;
// a forecast hook that is gone leaves the old rule: the keys are shown.
func TestWaitYieldsToTheCallWithMoreKeys(t *testing.T) {
	forecastFirst := strings.Replace(termScreen(t, "mcp-queue-2of2.txt"), "2 of 2", "1 of 2", 1)
	currentNext := strings.Replace(termScreen(t, "mcp-queue-1of2.txt"), "1 of 2", "2 of 2", 1)
	for name, c := range map[string]struct {
		forecastAlive bool
		wantAt        time.Duration // when the current weather's buttons go out
	}{
		"the forecast's hook looking": {true, queueAnsweredIn},
		"the forecast's hook gone":    {false, 0},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWaitHarness(t)
			w.away = true
			w.alive = func(pid int) bool { return pid != 4243 || c.forecastAlive }
			if err := w.store.PutAsk(&state.Ask{ID: state.NewAskID(), SessionID: sid, Tool: forecastTool, HookPID: 4243, Created: w.now,
				Keys: callKey(forecastTool, json.RawMessage(forecastInput), "").every}); err != nil {
				t.Fatal(err)
			}
			w.shown = forecastFirst
			start, sentAt := w.now, time.Time{}
			w.onSleep = func() {
				if w.now.Sub(start) >= queueAnsweredIn {
					w.shown = currentNext
				}
				if len(w.rec.Calls) > 0 && sentAt.IsZero() {
					sentAt, w.mac = w.now, true
				}
			}
			waitPermission(context.Background(), mcpInput(t, currentTool, currentInput), w.deps)
			if len(w.rec.Calls) != 1 || !strings.Contains(w.rec.Calls[0].Text, "widgets-current") {
				t.Fatalf("calls=%+v, want the current weather's button message", w.rec.Calls)
			}
			if got := sentAt.Sub(start); got < c.wantAt || got > c.wantAt+2*time.Second {
				t.Errorf("sent %v after the request, want at %v", got, c.wantAt)
			}
		})
	}
}

// A queued call stops looking once its session ends: a crash that leaves
// the queue on the pane must not keep the hook polling until its deadline.
func TestWaitQueuedSessionEnds(t *testing.T) {
	w := newWaitHarness(t)
	w.away = true
	w.shown = termScreen(t, "mcp-queue-1of2.txt")
	_, _ = w.store.UpdateSession(sid, func(s *state.Session) { s.PID = 4242 })
	start := w.now
	w.alive = func(pid int) bool { return pid != 4242 || w.now.Sub(start) < queueAnsweredIn }
	var logs []string
	w.deps.Log = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	waitPermission(context.Background(), mcpInput(t, forecastTool, forecastInput), w.deps)
	if len(w.rec.Calls) != 0 || w.asksLeft() != 0 {
		t.Fatalf("calls=%d asks=%d, want none", len(w.rec.Calls), w.asksLeft())
	}
	if got := w.now.Sub(start); got < queueAnsweredIn || got > queueAnsweredIn+2*time.Second {
		t.Errorf("gave up %v after the request, want at %v", got, queueAnsweredIn)
	}
	if s := strings.Join(logs, "\n"); !strings.Contains(s, "session ended while queued") {
		t.Errorf("logs:\n%s", s)
	}
}

// Keys compare as sets: a call repeating a value has fewer keys
// than one with the same value and another, whatever the counts. The call
// repeating "Paris" leaves the dialog showing "Paris" and "Lyon" to the
// call it belongs to, and gets its own once that is answered.
func TestWaitYieldsComparesKeySets(t *testing.T) {
	const tool = "mcp__claude_ai_Example__lookup"
	queued := termScreen(t, "mcp-queue-1of2.txt")
	const shown = `queryParams: { locationKey: "2608456", unit: "metric", lang: "en" }`
	if !strings.Contains(queued, shown) {
		t.Fatal("fixture lacks the arguments to replace")
	}
	for name, c := range map[string]struct {
		ours, theirs             string
		oursDialog, theirsDialog string
	}{
		"a repeated array item": {`{"items":["Paris","Paris"]}`, `{"items":["Paris","Lyon"]}`,
			`items: [ "Paris", "Paris" ]`, `items: [ "Paris", "Lyon" ]`},
		"a repeated nested leaf": {`{"a":{"q":"Paris"},"b":{"q":"Paris"}}`, `{"a":{"q":"Paris"},"b":{"q":"Lyon"}}`,
			`a: { q: "Paris" }, b: { q: "Paris" }`, `a: { q: "Paris" }, b: { q: "Lyon" }`},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWaitHarness(t)
			w.away = true
			w.alive = func(int) bool { return true }
			theirs := mcpInput(t, tool, c.theirs)
			if err := w.store.PutAsk(&state.Ask{ID: state.NewAskID(), SessionID: sid, Tool: tool, HookPID: 4243, Created: w.now,
				Keys: askKeys(callKey(tool, theirs.ToolInput, ""), theirs.ToolInput, render.Redact)}); err != nil {
				t.Fatal(err)
			}
			w.shown = strings.Replace(queued, shown, c.theirsDialog, 1)
			start, sentAt := w.now, time.Time{}
			w.onSleep = func() {
				if w.now.Sub(start) >= queueAnsweredIn {
					w.shown = strings.Replace(strings.Replace(queued, shown, c.oursDialog, 1), "1 of 2", "2 of 2", 1)
				}
				if len(w.rec.Calls) > 0 && sentAt.IsZero() {
					sentAt, w.mac = w.now, true
				}
			}
			waitPermission(context.Background(), mcpInput(t, tool, c.ours), w.deps)
			if got := sentAt.Sub(start); len(w.rec.Calls) != 1 || got < queueAnsweredIn || got > queueAnsweredIn+2*time.Second {
				t.Fatalf("calls=%d sent %v after the request, want one at %v", len(w.rec.Calls), got, queueAnsweredIn)
			}
		})
	}
}

// Each MCP key comes once, in the order it first comes: a repeated value or
// leaf proves nothing more, and keys compare as sets (ruling P27).
func TestMCPKeysOnceEach(t *testing.T) {
	for input, want := range map[string][]string{
		`{"items":["Paris","Paris"]}`:                            {"(MCP)", `"Paris"`},
		`{"a":{"q":"Paris"},"b":{"q":"Paris"}}`:                  {"(MCP)", `q:"Paris"`},
		`{"a":{"q":"Paris"},"b":{"q":"Lyon"},"c":{"q":"Paris"}}`: {"(MCP)", `q:"Paris"`, `q:"Lyon"`},
	} {
		if got := callKey("mcp__claude_ai_Example__lookup", json.RawMessage(input), "").every; !slices.Equal(got, want) {
			t.Errorf("%s: keys=%q, want %q", input, got, want)
		}
	}
}
