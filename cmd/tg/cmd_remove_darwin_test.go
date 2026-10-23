package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IZZY-Agency/claude-tg/internal/relay"
)

// remove takes the relay down through the same service manager as
// `relay uninstall` and, when launchd still has the agent, says so under
// its own name and deletes nothing.
func TestRemoveNamesTheLoadedLaunchdAgent(t *testing.T) {
	dir := isolate(t)
	claudeHome(t)
	writeConfig(t, dir)
	if code := run([]string{"relay", "install", "--no-load"}, strings.NewReader(""), io.Discard, io.Discard); code != 0 {
		t.Fatalf("install code=%d", code)
	}
	t.Setenv("TG_NO_SERVICE_MANAGER", "")
	old := launchctlFn
	launchctlFn = func(args ...string) (string, error) {
		switch args[0] {
		case "bootout":
			return "Boot-out failed: 5: Input/output error", errors.New("exit 5")
		case "print":
			return "state = running", nil
		}
		return "", nil
	}
	t.Cleanup(func() { launchctlFn = old })
	var out, errb bytes.Buffer
	if code := run([]string{"remove", "--purge"}, strings.NewReader(""), &out, &errb); code == 0 {
		t.Fatal("remove --purge must fail while the agent is still loaded")
	}
	for _, want := range []string{"tg remove: the launchd agent is still loaded", "nothing removed"} {
		if !strings.Contains(errb.String(), want) {
			t.Fatalf("stderr lacks %q:\n%s", want, errb.String())
		}
	}
	if out.Len() != 0 {
		t.Fatalf("stdout must stay empty, got %q", out.String())
	}
	for _, p := range []string{filepath.Join(dir, "LaunchAgents", relay.Label+".plist"), filepath.Join(dir, "config.json")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s must survive a refused remove: %v", p, err)
		}
	}
}

// launchd keeps its two answers: `launchctl print` succeeding means the
// agent is loaded, any failure means it is not; it never says unknown.
func TestLaunchdStateFollowsLaunchctlPrint(t *testing.T) {
	old := launchctlFn
	t.Cleanup(func() { launchctlFn = old })
	var answer error
	launchctlFn = func(args ...string) (string, error) {
		if args[0] != "print" {
			t.Fatalf("unexpected launchctl %v", args)
		}
		return "state = running", answer
	}
	m := &launchdManager{}
	if s, err := m.State(); s != serviceActive || err != nil {
		t.Fatalf("print ok: state=%v err=%v", s, err)
	}
	answer = errors.New("exit 113")
	if s, err := m.State(); s != serviceInactive || err != nil {
		t.Fatalf("print failed: state=%v err=%v", s, err)
	}
}
