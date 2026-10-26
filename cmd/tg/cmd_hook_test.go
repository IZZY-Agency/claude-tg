package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/classify"
	"github.com/IZZY-Agency/claude-tg/internal/config"
	"github.com/IZZY-Agency/claude-tg/internal/hooks"
	"github.com/IZZY-Agency/claude-tg/internal/idle"
	"github.com/IZZY-Agency/claude-tg/internal/session"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
	"github.com/IZZY-Agency/claude-tg/internal/term"
)

const waitPayload = `{"session_id":"6449d606-55c1-4e6d-a915-4b14c777f57f","transcript_path":"/nonexistent/t.jsonl","cwd":"/tmp","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"ls"},"permission_suggestions":[]}`

func TestAttended(t *testing.T) {
	const sid = "s1"
	agents := func(kind string) func() ([]session.Agent, error) {
		return func() ([]session.Agent, error) {
			return []session.Agent{{SessionID: "other", Kind: "background"}, {SessionID: sid, Kind: kind}}, nil
		}
	}
	notListed := func() ([]session.Agent, error) {
		return []session.Agent{{SessionID: "other", Kind: "background"}}, nil
	}
	unavailable := func() ([]session.Agent, error) { return nil, errors.New("claude: not found") }
	listed := false
	notCalled := func() ([]session.Agent, error) { listed = true; return nil, nil }
	cases := []struct {
		name string
		env  string
		list func() ([]session.Agent, error)
		want bool
	}{
		{"claude -p", "0", notCalled, false},
		{"interactive", "1", agents("interactive"), true},
		{"background agent", "1", agents("background"), false},
		{"background agent without the marker", "", agents("background"), false},
		{"not listed", "", notListed, true},
		{"agents unavailable, attended marker", "1", unavailable, true},
		{"agents unavailable, no marker", "", unavailable, false},
	}
	for _, tc := range cases {
		if got := attended(tc.env, tc.list, sid); got != tc.want {
			t.Errorf("%s: attended=%v, want %v", tc.name, got, tc.want)
		}
	}
	if listed {
		t.Fatal("ran `claude agents` for a claude -p run")
	}
}

func TestProcessAlive(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if !processAlive(os.Getpid()) || !processAlive(0) || !processAlive(-1) {
		t.Fatal("own pid, unknown pid 0 and a negative pid count as alive")
	}
	if processAlive(cmd.Process.Pid) {
		t.Fatalf("reaped child %d reported alive", cmd.Process.Pid)
	}
}

// The decision goes to stdout under the long, signal-aware context.
func TestHookPermissionWaitWritesDecision(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	orig := waitPermissionFn
	t.Cleanup(func() { waitPermissionFn = orig })
	var deadline time.Time
	var out bytes.Buffer
	var atCommit []string
	waitPermissionFn = func(ctx context.Context, in *hooks.Input, d hooks.Deps) ([]byte, func()) {
		deadline, _ = ctx.Deadline()
		if in.TranscriptPath != "/nonexistent/t.jsonl" || d.Edit == nil || d.RelayUp == nil || d.Unattended == nil || d.Attended == nil || d.Alive == nil || d.AwayIn == nil {
			t.Errorf("input=%+v or deps not wired", in)
		}
		return []byte(`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`),
			func() { atCommit = append(atCommit, out.String()) }
	}
	if code := run([]string{"hook", "permission-wait"}, strings.NewReader(waitPayload), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	const decision = `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}` + "\n"
	if out.String() != decision {
		t.Fatalf("stdout=%q", out.String())
	}
	// The answer is collected (and the ask removed) only once stdout has it.
	if len(atCommit) != 1 || atCommit[0] != decision {
		t.Fatalf("commits=%q, want one, after the decision was written", atCommit)
	}
	if left := time.Until(deadline); left < 12*time.Hour || left > 12*time.Hour+2*time.Minute {
		t.Fatalf("hook context ends in %s, want 12h2m", left)
	}
}

// failingWriter is a stdout Claude Code no longer reads.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write |1: broken pipe") }

// A decision that cannot be written never counts as collected: the ask is
// left for the relay's sweep, which reports it as not delivered.
func TestHookPermissionWaitUndeliveredDecisionNotCommitted(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	orig := waitPermissionFn
	t.Cleanup(func() { waitPermissionFn = orig })
	committed := false
	waitPermissionFn = func(context.Context, *hooks.Input, hooks.Deps) ([]byte, func()) {
		return []byte(`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`), func() { committed = true }
	}
	if code := run([]string{"hook", "permission-wait"}, strings.NewReader(waitPayload), failingWriter{}, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if committed {
		t.Fatal("committed an answer stdout never took")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "state", "tg.log"))
	if !strings.Contains(string(b), "broken pipe") {
		t.Fatalf("log lacks the failed write:\n%s", b)
	}
}

// With no relay running nobody can receive a tap: the hook records the
// request and leaves at once, silently.
func TestHookPermissionWaitWithoutRelay(t *testing.T) {
	dir := isolate(t)
	noPane(t)
	writeConfig(t, dir)
	var out bytes.Buffer
	start := time.Now()
	if code := run([]string{"hook", "permission-wait"}, strings.NewReader(waitPayload), &out, io.Discard); code != 0 || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%q", code, out.String())
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("waited without a relay")
	}
	assertRecordedNoAsk(t, dir)
}

// A claude -p run has no dialog to wait beside.
func TestHookPermissionWaitSkipsUnattended(t *testing.T) {
	dir := isolate(t)
	noPane(t)
	writeConfig(t, dir)
	t.Setenv("CLAUDE_CODE_SESSION_ATTENDED", "0")
	store, err := state.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	release, err := store.LockRelay() // a relay that routes ask taps is up
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := store.SaveRelay(state.RelayState{PID: 1, Features: []string{state.FeatureAsks}}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := run([]string{"hook", "permission-wait"}, strings.NewReader(waitPayload), &out, io.Discard); code != 0 || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%q", code, out.String())
	}
	assertRecordedNoAsk(t, dir)
}

func assertRecordedNoAsk(t *testing.T, dir string) {
	t.Helper()
	store, err := state.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := store.LoadSession("6449d606-55c1-4e6d-a915-4b14c777f57f")
	if s.PendingPermission == nil || s.PendingPermission.Tool != "Bash" {
		t.Fatalf("request not recorded: %+v", s.PendingPermission)
	}
	if asks, _ := store.ListAsks(); len(asks) != 0 {
		t.Fatalf("asks left: %d", len(asks))
	}
}

// Every other kind keeps its short context and its silent stdout.
func TestHookOtherKindsUnchanged(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	orig := waitPermissionFn
	t.Cleanup(func() { waitPermissionFn = orig })
	waitPermissionFn = func(context.Context, *hooks.Input, hooks.Deps) ([]byte, func()) {
		t.Error("permission-request ran the waiting hook")
		return []byte("{}"), func() {}
	}
	var out bytes.Buffer
	if code := run([]string{"hook", "permission-request"}, strings.NewReader(waitPayload), &out, io.Discard); code != 0 || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%q", code, out.String())
	}
	assertRecordedNoAsk(t, dir)
}

// The hook's relay check wants a relay that routes ask taps, not just one
// holding the lock: an older relay would leave the buttons dead.
func TestHookRelayUpNeedsAsksFeature(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	store, err := state.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	orig := waitPermissionFn
	t.Cleanup(func() { waitPermissionFn = orig })
	var got []bool
	waitPermissionFn = func(_ context.Context, _ *hooks.Input, d hooks.Deps) ([]byte, func()) {
		got = append(got, d.RelayUp())
		return nil, nil
	}
	hook := func() {
		if code := run([]string{"hook", "permission-wait"}, strings.NewReader(waitPayload), io.Discard, io.Discard); code != 0 {
			t.Fatalf("code=%d", code)
		}
	}
	release, err := store.LockRelay()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	hook() // an older relay: its state has no features
	if err := store.SaveRelay(state.RelayState{PID: 1, Features: []string{state.FeatureAsks}}); err != nil {
		t.Fatal(err)
	}
	hook()
	if len(got) != 2 || got[0] || !got[1] {
		t.Fatalf("RelayUp: %v, want [false true]", got)
	}
}

// noPane keeps a test's hook away from the terminal it runs in.
func noPane(t *testing.T) {
	t.Helper()
	for _, k := range []string{"AGTERM_SESSION_ID", "AGTERM_WINDOW_ID", "TMUX", "TMUX_PANE"} {
		t.Setenv(k, "")
	}
}

// The screen guard reads the hook's own pane, as the relay would type into
// it; without one there is no screen reader and so no buttons.
func TestHookWiresScreenFromOwnPane(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	orig := waitPermissionFn
	t.Cleanup(func() { waitPermissionFn = orig })
	var wired []bool
	waitPermissionFn = func(_ context.Context, _ *hooks.Input, d hooks.Deps) ([]byte, func()) {
		wired = append(wired, d.Screen != nil) // never called: it would read a real pane
		return nil, nil
	}
	hook := func() {
		if code := run([]string{"hook", "permission-wait"}, strings.NewReader(waitPayload), io.Discard, io.Discard); code != 0 {
			t.Fatalf("code=%d", code)
		}
	}
	noPane(t)
	hook()
	t.Setenv("AGTERM_SESSION_ID", "29D99BC0-8C1C-42EB-99C8-99AFA61A9B2B")
	hook()
	if len(wired) != 2 || wired[0] || !wired[1] {
		t.Fatalf("Screen wired: %v, want [false true]", wired)
	}
}

func TestOwnPane(t *testing.T) {
	cases := []struct {
		env  session.Env
		want term.Pane
	}{
		{session.Env{}, term.Pane{}},
		{session.Env{AgtermSessionID: "A1"}, term.Pane{Backend: "agterm", Target: "A1"}},
		// tmux names the exact pane, even inside an agterm tab.
		{session.Env{AgtermSessionID: "A1", TmuxPane: "%5", TmuxSocket: "/tmp/tmux-501/default"}, term.Pane{Backend: "tmux", Target: "%5", Socket: "/tmp/tmux-501/default"}},
	}
	for _, c := range cases {
		if got := ownPane(c.env); got != c.want {
			t.Errorf("ownPane(%+v) = %+v, want %+v", c.env, got, c.want)
		}
	}
}

// awayIn agrees with idle.Away: 0 exactly when the owner counts as away.
func TestAwayIn(t *testing.T) {
	idleFor := func(n int, err error) idle.Probe {
		return func(context.Context) (int, error) { return n, err }
	}
	cases := []struct {
		name      string
		threshold int
		probe     idle.Probe
		want      time.Duration
	}{
		{"gate off", 0, idleFor(0, nil), 0},
		{"idle long enough", 120, idleFor(130, nil), 0},
		{"no idle reading", 120, idleFor(0, errors.New("ioreg: not found")), 0},
		{"at the keyboard", 120, idleFor(0, nil), 120 * time.Second},
		{"idle a while", 120, idleFor(45, nil), 75 * time.Second},
	}
	for _, c := range cases {
		got := awayIn(context.Background(), c.threshold, c.probe)
		if got != c.want {
			t.Errorf("%s: awayIn=%s, want %s", c.name, got, c.want)
		}
		if away := idle.Away(context.Background(), c.threshold, c.probe); away != (got == 0) {
			t.Errorf("%s: idle.Away=%v but awayIn=%s", c.name, away, got)
		}
	}
}

// The waiting hook's closing edits go through send.EditHTML: redacted
// between the tags, and retried once as plain text without the keyboard
// when Telegram refuses the HTML.
func TestHookEditFallsBackToPlainText(t *testing.T) {
	dir := isolate(t)
	noPane(t)
	writeConfig(t, dir)
	var calls []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		body["method"] = path.Base(r.URL.Path)
		calls = append(calls, body)
		if body["parse_mode"] == "HTML" {
			_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer srv.Close()
	orig, origClient := waitPermissionFn, hookClient
	t.Cleanup(func() { waitPermissionFn, hookClient = orig, origClient })
	hookClient = func(token string) *telegram.Client {
		return &telegram.Client{Token: token, HTTP: srv.Client(), BaseURL: srv.URL}
	}
	var editErr error
	waitPermissionFn = func(ctx context.Context, _ *hooks.Input, d hooks.Deps) ([]byte, func()) {
		editErr = d.Edit(ctx, 7, "🔐 <b>acme</b>\nneeds permission: <code>Bash: mysql --password=s3cr3tvalue</code>\n\n<i>answered at the Mac</i>")
		return nil, nil
	}
	if code := run([]string{"hook", "permission-wait"}, strings.NewReader(waitPayload), io.Discard, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if editErr != nil || len(calls) != 2 {
		t.Fatalf("err=%v calls=%+v", editErr, calls)
	}
	html, plain := calls[0], calls[1]
	if html["method"] != "editMessageText" || html["text"] != "🔐 <b>acme</b>\nneeds permission: <code>Bash: mysql --password=[redacted]</code>\n\n<i>answered at the Mac</i>" {
		t.Fatalf("html edit=%+v", html)
	}
	if _, ok := plain["parse_mode"]; ok || plain["text"] != "🔐 acme\nneeds permission: Bash: mysql --password=[redacted]\n\nanswered at the Mac" || plain["reply_markup"] != nil || plain["message_id"] != float64(7) {
		t.Fatalf("plain edit=%+v", plain)
	}
}

// The stop hook's budget covers the speech command's 30 s, the voice
// upload and the text's reserve (hookTimeout) when speech_command is set;
// without it the budget is the one every hook had.
func TestStopBudget(t *testing.T) {
	if got := stopBudget(&config.Config{}); got != 15*time.Second {
		t.Fatalf("without speech_command: %s, want 15s", got)
	}
	got := stopBudget(&config.Config{SpeechCommand: copyFixture})
	if got != 60*time.Second {
		t.Fatalf("with speech_command: %s, want 1m0s", got)
	}
	speechTimeout := speechRunner(&cli{env: os.Getenv}, copyFixture).Timeout
	if got-stopTextReserve <= speechTimeout || stopTextReserve != hookTimeout {
		t.Fatalf("budget %s, text reserve %s: no room for the speech command's %s", got, stopTextReserve, speechTimeout)
	}
}

const stopPayload = `{"session_id":"6449d606-55c1-4e6d-a915-4b14c777f57f","cwd":"/Users/alex/code/acme-shop","hook_event_name":"Stop","stop_hook_active":false,"last_assistant_message":"Готово, тесты прошли."}`

// The stop hook asks TypeSafe only with both a key and a speech command:
// without speech there is nothing to choose, and without a key the 0.4.0
// rule stands.
func TestHookWiresClassifierOnlyWithKeyAndSpeech(t *testing.T) {
	for _, tc := range []struct {
		name, key, speech string
		wired             bool
	}{
		{"key and speech_command", "k-secret-123", copyFixture, true},
		{"key only", "k-secret-123", "", false},
		{"speech_command only", "", copyFixture, false},
		{"neither", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := isolate(t)
			cfg := map[string]any{"bot_token": "1:x", "chat_id": 1}
			if tc.key != "" {
				cfg["typesafe_api_key"] = tc.key
			}
			if tc.speech != "" {
				cfg["speech_command"] = tc.speech
			}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			orig := handleFn
			t.Cleanup(func() { handleFn = orig })
			var got []hooks.Classifier
			handleFn = func(_ context.Context, kind string, _ *hooks.Input, d hooks.Deps) error {
				if kind == "stop" {
					got = append(got, d.Classifier)
				}
				return nil
			}
			if code := run([]string{"hook", "stop"}, strings.NewReader(stopPayload), io.Discard, io.Discard); code != 0 {
				t.Fatalf("code=%d", code)
			}
			if len(got) != 1 {
				t.Fatalf("stop hook ran %d times, want once", len(got))
			}
			c, ok := got[0].(*classify.Client)
			switch {
			case tc.wired && (!ok || c == nil || c.Key != tc.key || c.BaseURL != ""):
				t.Fatalf("Classifier=%T (key matches: %v), want a *classify.Client with the configured key and the default URL", got[0], ok && c != nil && c.Key == tc.key)
			case !tc.wired && got[0] != nil:
				t.Fatalf("Classifier=%T, want nil", got[0])
			}
		})
	}
}

// A hook's 💬 message never shows the TypeSafe key, not even in the JSON
// form of the config, which credential redaction alone misses: the key is
// one of the sender's secrets, as the bot token is.
func TestHookStopRedactsTypesafeKey(t *testing.T) {
	const sid = "6449d606-55c1-4e6d-a915-4b14c777f57f"
	const key = "tsk-7Hq2mVx9LpR4wZ"
	dir := isolate(t)
	noPane(t)
	raw, err := json.Marshal(map[string]any{"bot_token": "1:x", "chat_id": 1, "typesafe_api_key": key})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var texts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		texts = append(texts, body.Text)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":42}}`))
	}))
	defer srv.Close()
	orig := hookClient
	t.Cleanup(func() { hookClient = orig })
	hookClient = func(token string) *telegram.Client {
		return &telegram.Client{Token: token, HTTP: srv.Client(), BaseURL: srv.URL}
	}
	store, err := state.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	// Followed, with a freshly resolved name: the hook never runs `claude agents`.
	if err := store.SaveSession(&state.Session{SessionID: sid, Name: "acme-shop-92", NameResolvedAt: time.Now(), Follow: true}); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"session_id": sid, "cwd": "/Users/alex/code/acme-shop", "hook_event_name": "Stop",
		"last_assistant_message": "The config holds:\n\n\"typesafe_api_key\": \"" + key + "\"",
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"hook", "stop"}, bytes.NewReader(payload), io.Discard, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if len(texts) != 1 || !strings.HasPrefix(texts[0], "💬 <b>acme-shop-92</b>") || !strings.Contains(texts[0], "[redacted]") || strings.Contains(texts[0], key) {
		t.Fatalf("sent %q, want one 💬 message with the key redacted", texts)
	}
}

// tg hook stop speaks a short answer owed to a voice note with the
// configured speech_command, then sends the text; without speech_command
// the answer is text only and the mark is spent all the same.
func TestHookStopSpeaksWithSpeechCommand(t *testing.T) {
	const sid = "6449d606-55c1-4e6d-a915-4b14c777f57f"
	for _, tc := range []struct {
		name, command string
		want          []string
	}{
		{"speech_command", copyFixture, []string{"sendVoice", "sendMessage"}},
		{"no speech_command", "", []string{"sendMessage"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := speakSetup(t, tc.command)
			noPane(t)
			dir := os.Getenv("TG_STATE_DIR")
			store, err := state.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			// A freshly resolved name: the hook never runs `claude agents`.
			s := &state.Session{SessionID: sid, Name: "acme-shop-92", NameResolvedAt: time.Now(), PendingAnswers: 1}
			s.MarkVoice("", "", time.Now())
			if err := store.SaveSession(s); err != nil {
				t.Fatal(err)
			}
			if code := run([]string{"hook", "stop"}, strings.NewReader(stopPayload), io.Discard, io.Discard); code != 0 {
				t.Fatalf("code=%d", code)
			}
			got := calls()
			var methods []string
			for _, c := range got {
				methods = append(methods, c.method)
			}
			if strings.Join(methods, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("requests=%v, want %v", methods, tc.want)
			}
			log, _ := os.ReadFile(filepath.Join(dir, "tg.log"))
			if tc.command != "" {
				v := got[0]
				if v.fields["caption"] != "🔊 acme-shop-92" || v.fields["disable_notification"] != "true" || v.audio == 0 {
					t.Fatalf("voice=%+v", v)
				}
				if !strings.Contains(string(log), "spoke 21 characters (") {
					t.Fatalf("log lacks the spoken line:\n%s", log)
				}
			}
			after, err := store.LoadSession(sid)
			if err != nil {
				t.Fatal(err)
			}
			if len(after.VoiceMarks) != 0 || after.PendingAnswers != 0 {
				t.Fatalf("mark or answer not spent: %+v", after)
			}
			left, _ := filepath.Glob(filepath.Join(dir, "voice", "speech-*"))
			if len(left) != 0 {
				t.Fatalf("speech dirs left behind: %v", left)
			}
		})
	}
}

// A signal to the stop hook while it speaks (the terminal closing, say)
// ends the speech: the command's process group, which the signal does not
// reach, is killed rather than orphaned, and the text still goes out. The
// signal goes to this test process, so this test must stay sequential
// (see TestSpeakInterrupted).
func TestHookStopSignalEndsSpeech(t *testing.T) {
	const sid = "6449d606-55c1-4e6d-a915-4b14c777f57f"
	calls := speakSetup(t, `echo $$ > "$TG_TEST_PIDFILE"; sleep 33.2719`)
	noPane(t)
	pidFile := filepath.Join(t.TempDir(), "speech.pid")
	t.Setenv("TG_TEST_PIDFILE", pidFile)
	dir := os.Getenv("TG_STATE_DIR")
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &state.Session{SessionID: sid, Name: "acme-shop-92", NameResolvedAt: time.Now(), PendingAnswers: 1}
	s.MarkVoice("", "", time.Now())
	if err := store.SaveSession(s); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- run([]string{"hook", "stop"}, strings.NewReader(stopPayload), io.Discard, io.Discard) }()
	var pid int
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if b, _ := os.ReadFile(pidFile); bytes.HasSuffix(b, []byte("\n")) {
			if pid, err = strconv.Atoi(strings.TrimSpace(string(b))); err != nil || pid <= 0 {
				t.Fatalf("pid file %q: %v", b, err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the speech command never started")
		}
	}
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code=%d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stop hook did not end its speech on the signal")
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatalf("the speech command %d outlived the signal", pid)
	}
	var methods []string
	for _, c := range calls() {
		methods = append(methods, c.method)
	}
	if strings.Join(methods, " ") != "sendMessage" {
		t.Fatalf("requests=%v, want the text only", methods)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "tg.log"))
	if !strings.Contains(string(log), "speech: context canceled") {
		t.Fatalf("log lacks the ended speech:\n%s", log)
	}
}
