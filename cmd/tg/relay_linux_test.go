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

// Uninstall must not report success while systemd still runs the unit.
func TestRelayUninstallFailsWhileStillActive(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	if code := run([]string{"relay", "install", "--no-load"}, strings.NewReader(""), io.Discard, io.Discard); code != 0 {
		t.Fatalf("install code=%d", code)
	}
	t.Setenv("TG_NO_SERVICE_MANAGER", "") // use the stub below instead of the hard guard
	old := systemctlFn
	systemctlFn = func(args ...string) (string, error) {
		switch args[0] {
		case "disable":
			return "Failed to stop claude-tg-relay.service: Connection timed out", errors.New("exit status 1")
		case "is-active":
			return "active", nil
		}
		return "", nil
	}
	t.Cleanup(func() { systemctlFn = old })
	var errb bytes.Buffer
	if code := run([]string{"relay", "uninstall"}, strings.NewReader(""), io.Discard, &errb); code == 0 {
		t.Fatal("uninstall must fail while the unit is still active")
	}
	if !strings.Contains(errb.String(), "systemd user service is still active") {
		t.Fatalf("stderr=%s", errb.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "systemd-user", relay.UnitName)); err != nil {
		t.Fatal("unit must be kept while the service is still active")
	}
	if code := run([]string{"remove", "--purge"}, strings.NewReader(""), io.Discard, io.Discard); code == 0 {
		t.Fatal("purge must abort while the service is still active")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatal("config must survive an aborted purge")
	}
}

// Install reloads the unit files, enables the unit for every login and
// restarts it so a reinstall picks up the new binary; status then names the
// manager and the main pid.
func TestRelayInstallReloadsEnablesAndRestarts(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	t.Setenv("TG_NO_SERVICE_MANAGER", "")
	old := systemctlFn
	var order []string
	running := false
	systemctlFn = func(args ...string) (string, error) {
		order = append(order, strings.Join(args, " "))
		switch args[0] {
		case "daemon-reload":
			return "", nil
		case "enable":
			return "Created symlink /home/alex/.config/systemd/user/default.target.wants/claude-tg-relay.service → /home/alex/.config/systemd/user/claude-tg-relay.service.", nil
		case "restart":
			running = true
			return "", nil
		case "is-active":
			if running {
				return "active", nil
			}
			return "inactive", errors.New("exit status 3")
		case "show":
			return "4242", nil
		}
		return "", errors.New("unexpected systemctl " + args[0])
	}
	t.Cleanup(func() { systemctlFn = old })
	var out bytes.Buffer
	if code := run([]string{"relay", "install"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d out=%s", code, out.String())
	}
	want := []string{"daemon-reload", "enable " + relay.UnitName, "restart " + relay.UnitName, "is-active " + relay.UnitName, "show -p MainPID --value " + relay.UnitName}
	if strings.Join(order, "\n") != strings.Join(want, "\n") {
		t.Fatalf("systemctl calls:\n%s\nwant:\n%s", strings.Join(order, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(out.String(), "systemd user service: active (pid 4242)") {
		t.Fatalf("status after install: %s", out.String())
	}
}

// A failing systemctl step is reported with its name and output.
func TestRelayInstallReportsFailingSystemctlStep(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	t.Setenv("TG_NO_SERVICE_MANAGER", "")
	old := systemctlFn
	systemctlFn = func(args ...string) (string, error) {
		if args[0] == "daemon-reload" {
			return "Failed to connect to bus: No medium found", errors.New("exit status 1")
		}
		return "", nil
	}
	t.Cleanup(func() { systemctlFn = old })
	var errb bytes.Buffer
	if code := run([]string{"relay", "install"}, strings.NewReader(""), io.Discard, &errb); code == 0 {
		t.Fatal("install must fail when systemctl fails")
	}
	if !strings.Contains(errb.String(), "systemctl --user daemon-reload") || !strings.Contains(errb.String(), "No medium found") {
		t.Fatalf("stderr=%s", errb.String())
	}
}

// Status with an inactive unit points at `tg relay install`, like launchd.
func TestRelayStatusInactiveUnit(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	if code := run([]string{"relay", "install", "--no-load"}, strings.NewReader(""), io.Discard, io.Discard); code != 0 {
		t.Fatal("install")
	}
	t.Setenv("TG_NO_SERVICE_MANAGER", "")
	old := systemctlFn
	systemctlFn = func(args ...string) (string, error) {
		if args[0] == "is-active" {
			return "inactive", errors.New("exit status 3")
		}
		t.Fatalf("unexpected systemctl %v", args)
		return "", nil
	}
	t.Cleanup(func() { systemctlFn = old })
	var out bytes.Buffer
	if code := run([]string{"relay", "status"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.HasPrefix(out.String(), "systemd user service: "+notLoadedStatus+"\n") {
		t.Fatalf("out=%s", out.String())
	}
}

// The older guard name must keep working for existing docs and scripts.
func TestLegacyNoLaunchctlGuardStillDisablesSystemctl(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	t.Setenv("TG_NO_SERVICE_MANAGER", "")
	t.Setenv("TG_NO_LAUNCHCTL", "1")
	old := systemctlFn
	systemctlFn = func(args ...string) (string, error) {
		t.Fatalf("systemctl %v reached despite TG_NO_LAUNCHCTL", args)
		return "", nil
	}
	t.Cleanup(func() { systemctlFn = old })
	var out bytes.Buffer
	if code := run([]string{"relay", "status"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.HasPrefix(out.String(), "systemd user service: not installed (") {
		t.Fatalf("out=%s", out.String())
	}
}

// When systemd cannot be asked (no user bus over a bare ssh login, say),
// `disable --now` and `is-active` both fail and the relay may well still be
// running. remove must then delete nothing and say that the state is
// unknown, under its own name.
func TestRemovePurgeRefusesWhenServiceStateUnknown(t *testing.T) {
	dir := isolate(t)
	home := claudeHome(t)
	writeConfig(t, dir)
	if code := run([]string{"relay", "install", "--no-load"}, strings.NewReader(""), io.Discard, io.Discard); code != 0 {
		t.Fatalf("install code=%d", code)
	}
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	settingsBefore, err := os.ReadFile(filepath.Join(home, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TG_NO_SERVICE_MANAGER", "")
	old := systemctlFn
	systemctlFn = func(args ...string) (string, error) {
		return "Failed to connect to bus: No medium found", errors.New("exit status 1")
	}
	t.Cleanup(func() { systemctlFn = old })
	var out, errb bytes.Buffer
	if code := run([]string{"remove", "--purge"}, strings.NewReader(""), &out, &errb); code == 0 {
		t.Fatal("remove --purge must fail while the service state is unknown")
	}
	for _, want := range []string{
		"tg remove: the systemd user service's state is unknown",
		"the relay may still be running",
		"systemctl --user is-active: Failed to connect to bus: No medium found",
		"nothing removed",
	} {
		if !strings.Contains(errb.String(), want) {
			t.Fatalf("stderr lacks %q:\n%s", want, errb.String())
		}
	}
	if out.Len() != 0 {
		t.Fatalf("stdout must stay empty, got %q", out.String())
	}
	for _, p := range []string{
		filepath.Join(dir, "systemd-user", relay.UnitName),
		filepath.Join(dir, "config.json"),
		filepath.Join(dir, "state"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s must survive a refused remove: %v", p, err)
		}
	}
	settingsAfter, err := os.ReadFile(filepath.Join(home, "settings.json"))
	if err != nil || !bytes.Equal(settingsBefore, settingsAfter) {
		t.Fatalf("settings.json changed by a refused remove (err=%v)", err)
	}
}

// Once systemd confirms the unit stopped, remove --purge takes everything
// down: unit file, hooks, config and state.
func TestRemovePurgeRemovesEverythingOnceTheServiceStopped(t *testing.T) {
	dir := isolate(t)
	claudeHome(t)
	writeConfig(t, dir)
	if code := run([]string{"relay", "install", "--no-load"}, strings.NewReader(""), io.Discard, io.Discard); code != 0 {
		t.Fatalf("install code=%d", code)
	}
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TG_NO_SERVICE_MANAGER", "")
	old := systemctlFn
	var calls []string
	stopped := false
	systemctlFn = func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "disable":
			stopped = true
			return `Removed "/home/alex/.config/systemd/user/default.target.wants/claude-tg-relay.service".`, nil
		case "is-active":
			if stopped {
				return "inactive", errors.New("exit status 3")
			}
			return "active", nil
		}
		t.Fatalf("unexpected systemctl %v", args)
		return "", nil
	}
	t.Cleanup(func() { systemctlFn = old })
	var out, errb bytes.Buffer
	if code := run([]string{"remove", "--purge"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("remove code=%d stderr=%s", code, errb.String())
	}
	want := []string{"disable --now " + relay.UnitName, "is-active " + relay.UnitName}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("systemctl calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	for _, p := range []string{
		filepath.Join(dir, "systemd-user", relay.UnitName),
		filepath.Join(dir, "config.json"),
		filepath.Join(dir, "state"),
	} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s still present after remove --purge", p)
		}
	}
	for _, want := range []string{"Relay agent removed.\n", "Config and state: deleted\n"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("stdout lacks %q:\n%s", want, out.String())
		}
	}
}

// A control character in a value would corrupt the unit (a newline starts
// a new directive), so install refuses it before printing or writing.
func TestRelayInstallRefusesControlCharacterInBinaryPath(t *testing.T) {
	dir := isolate(t)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"bot_token":"1:x","chat_id":1,"away_after_seconds":0,"binary_path":"/opt/x\n/tg"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "tg relay install: binary path \"/opt/x\\n/tg\" contains control character '\\n'\n"
	for _, args := range [][]string{{"relay", "install", "--print"}, {"relay", "install", "--no-load"}} {
		var out, errb bytes.Buffer
		if code := run(args, strings.NewReader(""), &out, &errb); code == 0 {
			t.Fatalf("%v must fail", args)
		}
		if errb.String() != want {
			t.Fatalf("%v stderr=%q want %q", args, errb.String(), want)
		}
		if out.Len() != 0 {
			t.Fatalf("%v stdout=%q, want nothing", args, out.String())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "systemd-user", relay.UnitName)); err == nil {
		t.Fatal("unit must not be written")
	}
}
