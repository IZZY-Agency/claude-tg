package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/state"
)

func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TG_CONFIG", filepath.Join(dir, "config.json"))
	t.Setenv("TG_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	// Never let a test reach the real launchd/systemd service or its folder.
	t.Setenv("TG_LAUNCH_AGENTS_DIR", filepath.Join(dir, "LaunchAgents"))
	t.Setenv("TG_SYSTEMD_USER_DIR", filepath.Join(dir, "systemd-user"))
	t.Setenv("TG_NO_SERVICE_MANAGER", "1")
	return dir
}

func writeConfig(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"bot_token":"1:x","chat_id":1,"away_after_seconds":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Every message tg sends from the command line scrubs the configured
// secrets: the bot token and the TypeSafe key.
func TestNewAppSenderSecrets(t *testing.T) {
	dir := isolate(t)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"bot_token":"1:x","chat_id":1,"typesafe_api_key":"k-secret-123"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a, code := newApp(&cli{stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard, env: os.Getenv})
	if code != exitOK {
		t.Fatalf("code=%d", code)
	}
	if want := []string{"1:x", "k-secret-123"}; !slices.Equal(a.sender.Secrets, want) {
		t.Fatalf("sender secrets: %d of them, want the bot token and the key", len(a.sender.Secrets))
	}
}

func TestSendWithoutConfigExits2(t *testing.T) {
	isolate(t)
	var errb bytes.Buffer
	code := run([]string{"send", "hello"}, strings.NewReader(""), io.Discard, &errb)
	if code != 2 || !strings.Contains(errb.String(), "tg setup") {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
}

func TestFollowWithoutSessionExits2(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	var errb bytes.Buffer
	code := run([]string{"follow", "status"}, strings.NewReader(""), io.Discard, &errb)
	if code != 2 || !strings.Contains(errb.String(), "session") {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
}

func TestFollowStatusWithSessionFlag(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	var out bytes.Buffer
	code := run([]string{"follow", "status", "--session", "6449d606-55c1-4e6d-a915-4b14c777f57f"}, strings.NewReader(""), &out, io.Discard)
	if code != 0 || !strings.Contains(out.String(), "off") {
		t.Fatalf("code=%d out=%q", code, out.String())
	}
}

func TestHookNeverFailsOnGarbage(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	for _, in := range []string{"x", "", `{"hook_event_name":"Stop"}`} {
		var out bytes.Buffer
		code := run([]string{"hook", "stop"}, strings.NewReader(in), &out, io.Discard)
		if code != 0 || out.Len() != 0 {
			t.Fatalf("input %q: code=%d stdout=%q", in, code, out.String())
		}
	}
	// Unconfigured is also silent and successful.
	isolate(t)
	var out bytes.Buffer
	if code := run([]string{"hook", "stop"}, strings.NewReader(`{"session_id":"s"}`), &out, io.Discard); code != 0 || out.Len() != 0 {
		t.Fatalf("unconfigured hook: code=%d stdout=%q", code, out.String())
	}
}

func TestSendEmptyTextIsUsage(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	if code := run([]string{"send"}, strings.NewReader(""), io.Discard, io.Discard); code != 1 {
		t.Fatalf("code=%d", code)
	}
	if code := run([]string{"send", "-"}, strings.NewReader("   \n"), io.Discard, io.Discard); code != 1 {
		t.Fatalf("stdin whitespace: code=%d", code)
	}
}

func TestSessionsListsFollowState(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	sid := "6449d606-55c1-4e6d-a915-4b14c777f57f"
	t.Setenv("CLAUDE_CODE_SESSION_ID", sid)
	// follow on would try to send a confirmation; use the store directly via status first,
	// then assert the listing shows the session with follow off.
	_ = run([]string{"follow", "status"}, strings.NewReader(""), io.Discard, io.Discard)
	var out bytes.Buffer
	if code := run([]string{"sessions", "--json"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out.String(), sid) || !strings.Contains(out.String(), `"follow": false`) {
		t.Fatalf("out=%s", out.String())
	}
}

func TestLogPrintsTail(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state", "tg.log"), []byte("l1\nl2\nl3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := run([]string{"log", "-n", "2"}, strings.NewReader(""), &out, io.Discard); code != 0 || out.String() != "l2\nl3\n" {
		t.Fatalf("code=%d out=%q", code, out.String())
	}
}

// tg follow off forgets the session's voice marks in the same write: a
// mark made while it was followed must not make the answer to a later
// typed swipe-reply spoken. tg follow on keeps them.
func TestFollowOffClearsVoiceMarks(t *testing.T) {
	const sid = "6449d606-55c1-4e6d-a915-4b14c777f57f"
	calls := speakSetup(t, "")
	store, err := state.Open(os.Getenv("TG_STATE_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	// A freshly resolved name: tg follow never runs `claude agents`.
	s := &state.Session{SessionID: sid, Name: "acme-shop-92", NameResolvedAt: time.Now()}
	s.MarkVoice("", "", time.Now())
	if err := store.SaveSession(s); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		action string
		marks  int
	}{{"on", 1}, {"off", 0}} {
		var out, errb bytes.Buffer
		if code := run([]string{"follow", step.action, "--session", sid}, strings.NewReader(""), &out, &errb); code != exitOK {
			t.Fatalf("follow %s: code=%d err=%q", step.action, code, errb.String())
		}
		after, err := store.LoadSession(sid)
		if err != nil {
			t.Fatal(err)
		}
		if after.Follow != (step.action == "on") || len(after.VoiceMarks) != step.marks {
			t.Fatalf("follow %s: follow=%v marks=%+v, want %d marks", step.action, after.Follow, after.VoiceMarks, step.marks)
		}
	}
	if got := calls(); len(got) != 2 || got[0].method != "sendMessage" {
		t.Fatalf("confirmations = %+v", got)
	}
}

// Leftover operands must be rejected, not silently reordered.
func TestFollowRejectsLeftoverArgs(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	if code := run([]string{"follow", "on", "typo", "--session", "6449d606-55c1-4e6d-a915-4b14c777f57f"}, strings.NewReader(""), io.Discard, io.Discard); code != 1 {
		t.Fatalf("code=%d, want usage error", code)
	}
}

// A negative line count is a usage error, not a panic.
func TestLogNegativeCountIsUsage(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "state", "tg.log"), []byte("l1\n"), 0o600)
	if code := run([]string{"log", "-n", "-1"}, strings.NewReader(""), io.Discard, io.Discard); code != 1 {
		t.Fatalf("code=%d", code)
	}
}
