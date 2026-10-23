package main

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IZZY-Agency/claude-tg/internal/state"
)

// withTools makes lookPathFn find only the named executables.
func withTools(t *testing.T, found ...string) {
	t.Helper()
	old := lookPathFn
	lookPathFn = func(name string) (string, error) {
		for _, f := range found {
			if f == name {
				return "/opt/homebrew/bin/" + name, nil
			}
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { lookPathFn = old })
}

func TestRelayInstallReportsTerminalBackends(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	withTools(t, "tmux")
	var out, errb bytes.Buffer
	if code := run([]string{"relay", "install", "--no-load"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("code=%d out=%s err=%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "terminal backends: tmux\n") || strings.Contains(errb.String(), "warning") {
		t.Fatalf("out=%q err=%q", out.String(), errb.String())
	}
}

func TestRelayInstallWarnsWithoutTerminalBackend(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	withTools(t)
	var out, errb bytes.Buffer
	if code := run([]string{"relay", "install", "--no-load"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("install must still succeed: code=%d err=%s", code, errb.String())
	}
	want := "warning: neither agtermctl nor tmux is on PATH; replies and /approve will be refused, /sessions, /follow and /status still work"
	if !strings.Contains(errb.String(), want) || !strings.Contains(out.String(), "terminal backends: none\n") {
		t.Fatalf("out=%q err=%q", out.String(), errb.String())
	}
}

func TestRelayStatusReportsTerminalBackends(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	withTools(t, "agtermctl", "tmux")
	var out bytes.Buffer
	if code := run([]string{"relay", "status"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out.String(), "terminal backends: agterm, tmux\n") {
		t.Fatalf("out=%q", out.String())
	}
}

func TestSetupCheckReportsTerminalBackends(t *testing.T) {
	isolate(t)
	claudeHome(t)
	withTools(t)
	var out bytes.Buffer
	if code := run([]string{"setup", "--check"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out.String(), "terminal backends: none (replies unavailable)\n") {
		t.Fatalf("out=%q", out.String())
	}
}

func TestSessionsShowsTerminalColumn(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	st, err := state.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SaveSession(&state.Session{SessionID: "11111111-0000-4000-8000-000000000001", Name: "in-tmux", TmuxPane: "%5", TmuxSocket: "/private/tmp/tmux-501/default", AgtermSessionID: "A1"})
	_ = st.SaveSession(&state.Session{SessionID: "22222222-0000-4000-8000-000000000002", Name: "in-agterm", AgtermSessionID: "A2"})
	_ = st.SaveSession(&state.Session{SessionID: "33333333-0000-4000-8000-000000000003", Name: "nowhere"})
	var out bytes.Buffer
	if code := run([]string{"sessions"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	lines := strings.Split(out.String(), "\n")
	if !strings.Contains(lines[0], "TERMINAL") {
		t.Fatalf("header: %q", lines[0])
	}
	for _, want := range []struct{ name, cell string }{{"in-tmux", "tmux %5"}, {"in-agterm", "agterm"}, {"nowhere", "-"}} {
		found := false
		for _, l := range lines[1:] {
			if strings.HasPrefix(l, want.name) && strings.Contains(l, want.cell) {
				found = true
			}
		}
		if !found {
			t.Fatalf("row for %s lacks %q:\n%s", want.name, want.cell, out.String())
		}
	}
	out.Reset()
	if code := run([]string{"sessions", "--json"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("json code=%d", code)
	}
	if !strings.Contains(out.String(), `"terminal": "tmux:%5"`) || !strings.Contains(out.String(), `"terminal": "-"`) {
		t.Fatalf("json=%s", out.String())
	}
}

// `tg follow status --session A` run from
// another session's pane must not rebind A to the caller's terminal.
func TestSessionFlagDoesNotRebindTargetSession(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	st, err := state.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	target := "aaaaaaaa-0000-4000-8000-000000000001"
	_ = st.SaveSession(&state.Session{SessionID: target, Name: "target-01", AgtermSessionID: "A1", Cwd: "/a"})
	t.Setenv("CLAUDE_CODE_SESSION_ID", "bbbbbbbb-0000-4000-8000-000000000002")
	t.Setenv("TMUX_PANE", "%9")
	t.Setenv("TMUX", "/private/tmp/tmux-501/default,1,0")
	t.Setenv("AGTERM_SESSION_ID", "")
	if code := run([]string{"follow", "status", "--session", target}, strings.NewReader(""), io.Discard, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	got, _ := st.LoadSession(target)
	if got.TmuxPane != "" || got.AgtermSessionID != "A1" || got.Cwd != "/a" {
		t.Fatalf("target was rebound to the caller's terminal: %+v", got)
	}
}
