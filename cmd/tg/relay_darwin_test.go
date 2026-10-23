package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/relay"
)

// Uninstall must not report success while the agent is still loaded.
func TestRelayUninstallFailsWhileStillLoaded(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	if code := run([]string{"relay", "install", "--no-load"}, strings.NewReader(""), io.Discard, io.Discard); code != 0 {
		t.Fatalf("install code=%d", code)
	}
	t.Setenv("TG_NO_SERVICE_MANAGER", "") // use the stub below instead of the hard guard
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
	var errb bytes.Buffer
	if code := run([]string{"relay", "uninstall"}, strings.NewReader(""), io.Discard, &errb); code == 0 {
		t.Fatal("uninstall must fail while the agent is still loaded")
	}
	if !strings.Contains(errb.String(), "launchd agent is still loaded") {
		t.Fatalf("stderr=%s", errb.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "LaunchAgents", relay.Label+".plist")); err != nil {
		t.Fatal("plist must be kept while the agent is still loaded")
	}
	if code := run([]string{"remove", "--purge"}, strings.NewReader(""), io.Discard, io.Discard); code == 0 {
		t.Fatal("purge must abort while the agent is still loaded")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatal("config must survive an aborted purge")
	}
}

// Reinstalling races launchd: the old job may still be unloading when the new
// one is bootstrapped. Install must wait for the unload and retry bootstrap.
func TestRelayInstallWaitsForBootoutAndRetriesBootstrap(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	t.Setenv("TG_NO_SERVICE_MANAGER", "")
	oldFn, oldPoll := launchctlFn, launchdPoll
	launchdPoll = 5 * time.Millisecond
	prints, bootstraps := 0, 0
	var order []string
	launchctlFn = func(args ...string) (string, error) {
		order = append(order, args[0])
		switch args[0] {
		case "bootout":
			return "", nil
		case "print":
			prints++
			if prints <= 2 {
				return "state = running\npid = 1", nil // still unloading
			}
			if bootstraps >= 2 {
				return "state = running\npid = 2", nil // the new job
			}
			return "Could not find service", errors.New("exit 113")
		case "bootstrap":
			bootstraps++
			if bootstraps == 1 {
				return "Bootstrap failed: 5: Input/output error", errors.New("exit 5")
			}
			return "", nil
		}
		return "", nil
	}
	t.Cleanup(func() { launchctlFn, launchdPoll = oldFn, oldPoll })
	var out bytes.Buffer
	if code := run([]string{"relay", "install"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d out=%s", code, out.String())
	}
	if bootstraps != 2 {
		t.Fatalf("bootstrap attempts=%d want 2", bootstraps)
	}
	// print (waiting for the unload) must come before the first bootstrap
	firstPrint, firstBootstrap := -1, -1
	for i, c := range order {
		if c == "print" && firstPrint < 0 {
			firstPrint = i
		}
		if c == "bootstrap" && firstBootstrap < 0 {
			firstBootstrap = i
		}
	}
	if firstPrint < 0 || firstPrint > firstBootstrap {
		t.Fatalf("install did not wait for the unload: %v", order)
	}
	if !strings.Contains(out.String(), "launchd agent: loaded, running, pid 2") {
		t.Fatalf("status after install: %s", out.String())
	}
}

// The older guard name must keep working for existing docs and scripts.
func TestLegacyNoLaunchctlGuardStillDisablesLaunchctl(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	t.Setenv("TG_NO_SERVICE_MANAGER", "")
	t.Setenv("TG_NO_LAUNCHCTL", "1")
	old := launchctlFn
	launchctlFn = func(args ...string) (string, error) {
		t.Fatalf("launchctl %v reached despite TG_NO_LAUNCHCTL", args)
		return "", nil
	}
	t.Cleanup(func() { launchctlFn = old })
	var out bytes.Buffer
	if code := run([]string{"relay", "status"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.HasPrefix(out.String(), "launchd agent: not installed (") {
		t.Fatalf("out=%s", out.String())
	}
}
