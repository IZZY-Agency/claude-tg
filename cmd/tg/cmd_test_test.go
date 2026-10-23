package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
