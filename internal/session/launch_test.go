package session

import (
	"strings"
	"testing"
)

const launchID = "6449d606-55c1-4e6d-a915-4b14c777f57f"

// One program, in the two forms a pane can need: a line for a shell that is
// already there, and a process for a pane that has none.
func TestLaunchForms(t *testing.T) {
	got, err := Launch("/opt/homebrew/bin/fish", "/Users/alex/.local/bin/claude", launchID, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := "/Users/alex/.local/bin/claude --session-id " + launchID; got.Line != want {
		t.Fatalf("line = %q, want %q", got.Line, want)
	}
	if want := "/opt/homebrew/bin/fish -lc '/Users/alex/.local/bin/claude --session-id " + launchID + "'"; got.Process != want {
		t.Fatalf("process = %q, want %q", got.Process, want)
	}
}

// Left unset, no flag at all: the session starts in the owner's own default
// mode, so a stricter one they chose stays. Forcing auto would loosen it.
func TestLaunchAddsNoPermissionFlag(t *testing.T) {
	got, err := Launch("/bin/zsh", "/usr/local/bin/claude", launchID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"permission", "dangerously", "auto"} {
		if strings.Contains(got.Line+got.Process, flag) {
			t.Fatalf("launch %+v has %q", got, flag)
		}
	}
}

// Nothing in it is quoted, so what could need quoting is refused.
func TestLaunchRefusesWhatCouldBreakOut(t *testing.T) {
	for _, c := range []struct{ shell, claude, id string }{
		{"/bin/zsh", "/Users/alex/my tools/claude", launchID},
		{"/bin/zsh", "/tmp/x';touch /tmp/y;'", launchID},
		{"/bin/zsh", "$(id)", launchID},
		{"/bin/zsh;id", "/usr/bin/claude", launchID},
		{"", "/usr/bin/claude", launchID},
		{"/bin/zsh", "", launchID},
		{"/bin/zsh", "/usr/bin/claude", "not a uuid"},
	} {
		if l, err := Launch(c.shell, c.claude, c.id, ""); err == nil {
			t.Errorf("Launch(%q, %q, %q) = %+v, want an error", c.shell, c.claude, c.id, l)
		}
	}
}

// A mode the owner chose reaches both forms.
func TestLaunchUsesTheModeAsked(t *testing.T) {
	got, err := Launch("/bin/zsh", "/usr/local/bin/claude", launchID, "bypassPermissions")
	if err != nil {
		t.Fatal(err)
	}
	if want := "/usr/local/bin/claude --permission-mode bypassPermissions --session-id " + launchID; got.Line != want {
		t.Fatalf("line = %q, want %q", got.Line, want)
	}
	if want := "/bin/zsh -lc '/usr/local/bin/claude --permission-mode bypassPermissions --session-id " + launchID + "'"; got.Process != want {
		t.Fatalf("process = %q, want %q", got.Process, want)
	}
}

// Every mode Claude Code takes is accepted, and nothing else: the value goes
// into a command line.
func TestLaunchTakesOnlyKnownModes(t *testing.T) {
	for _, mode := range []string{"acceptEdits", "auto", "bypassPermissions", "manual", "dontAsk", "plan"} {
		if _, err := Launch("/bin/zsh", "/usr/bin/claude", launchID, mode); err != nil {
			t.Errorf("mode %q: %v", mode, err)
		}
	}
	for _, mode := range []string{"bypass", "AUTO", "auto; id", "auto --dangerously-skip-permissions", "--dangerously-skip-permissions", " auto"} {
		if l, err := Launch("/bin/zsh", "/usr/bin/claude", launchID, mode); err == nil {
			t.Errorf("mode %q gave %+v", mode, l)
		}
	}
}

func TestParseDsclShell(t *testing.T) {
	if got := parseDscl("UserShell: /opt/homebrew/bin/fish\n"); got != "/opt/homebrew/bin/fish" {
		t.Fatalf("got %q", got)
	}
	if got := parseDscl("No such key: UserShell\n"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestParsePasswdShell(t *testing.T) {
	if got := parsePasswd("alex:x:1000:1000:Alex:/home/alex:/usr/bin/fish\n"); got != "/usr/bin/fish" {
		t.Fatalf("got %q", got)
	}
	if got := parsePasswd("broken\n"); got != "" {
		t.Fatalf("got %q", got)
	}
}

// The relay has no $SHELL under launchd, so the user database comes next,
// and a shell that always exists comes last.
func TestLoginShellOrder(t *testing.T) {
	lookup := func(string) string { return "/from/database" }
	none := func(string) string { return "" }
	if got := loginShell("/from/env", lookup, "alex"); got != "/from/env" {
		t.Fatalf("got %q", got)
	}
	if got := loginShell("", lookup, "alex"); got != "/from/database" {
		t.Fatalf("got %q", got)
	}
	if got := loginShell("", none, "alex"); got != "/bin/sh" {
		t.Fatalf("got %q", got)
	}
}
