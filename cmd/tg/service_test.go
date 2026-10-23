package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/IZZY-Agency/claude-tg/internal/relay"
)

// serviceFileIn is where `tg relay install` writes the service file under
// an isolate()d directory on this OS.
func serviceFileIn(dir string) string {
	if runtime.GOOS == "linux" {
		return filepath.Join(dir, "systemd-user", relay.UnitName)
	}
	return filepath.Join(dir, "LaunchAgents", relay.Label+".plist")
}

// serviceFileCommandMarkers are strings that prove the printed service file
// runs `<binary> relay` under this OS's manager.
func serviceFileCommandMarkers() []string {
	if runtime.GOOS == "linux" {
		return []string{"Description=claude-tg Telegram relay", "ExecStart=", " relay\n"}
	}
	return []string{"<string>" + relay.Label + "</string>", "<string>relay</string>"}
}

// serviceFileKeepAliveMarker is the restart policy line of this OS's file.
func serviceFileKeepAliveMarker() string {
	if runtime.GOOS == "linux" {
		return "Restart=always"
	}
	return "KeepAlive"
}

// serviceFileEnvMarkers are the TG_CONFIG and TG_STATE_DIR entries the
// service must carry so it reads the CLI's config and state.
func serviceFileEnvMarkers(dir string) []string {
	cfg, st := filepath.Join(dir, "config.json"), filepath.Join(dir, "state")
	if runtime.GOOS == "linux" {
		return []string{"Environment=TG_CONFIG=" + cfg + "\n", "Environment=TG_STATE_DIR=" + st + "\n"}
	}
	return []string{"<key>TG_CONFIG</key>", "<string>" + cfg + "</string>", "<key>TG_STATE_DIR</key>", "<string>" + st + "</string>"}
}

func TestServiceManagerDisabledHonoursBothNames(t *testing.T) {
	env := func(vals map[string]string) *cli {
		return &cli{env: func(k string) string { return vals[k] }}
	}
	if serviceManagerDisabled(env(nil)) {
		t.Fatal("enabled by default")
	}
	if !serviceManagerDisabled(env(map[string]string{"TG_NO_SERVICE_MANAGER": "1"})) {
		t.Fatal("TG_NO_SERVICE_MANAGER must disable")
	}
	if !serviceManagerDisabled(env(map[string]string{"TG_NO_LAUNCHCTL": "1"})) {
		t.Fatal("TG_NO_LAUNCHCTL must keep disabling (alias)")
	}
}

func TestSummarizeLaunchctl(t *testing.T) {
	tests := map[string]string{
		"\tstate = running\n\tpid = 4242\n": "running, pid 4242",
		"state = not running\n":             "not running",
		"pid = abc\nstate = waiting":        "waiting",
		"":                                  "state unknown",
	}
	for in, want := range tests {
		if got := summarizeLaunchctl(in); got != want {
			t.Fatalf("summarizeLaunchctl(%q)=%q want %q", in, got, want)
		}
	}
}

func TestSystemdStatus(t *testing.T) {
	tests := []struct {
		state string
		err   error
		pid   string
		want  string
	}{
		{"active\n", nil, "4242\n", "active (pid 4242)"},
		{"active", nil, "0", "active"},
		{"activating", errors.New("exit status 3"), "", "activating"},
		{"refreshing", nil, "7", "refreshing (pid 7)"},
		{"inactive", errors.New("exit status 3"), "", notLoadedStatus},
		{"failed", errors.New("exit status 3"), "", "failed (run `tg relay install`)"},
		{"Failed to connect to bus: No medium found", errors.New("exit status 1"), "", "unknown (systemctl --user is-active: Failed to connect to bus: No medium found)"},
		{"", errors.New(`exec: "systemctl": executable file not found in $PATH`), "", `unknown (systemctl --user is-active: exec: "systemctl": executable file not found in $PATH)`},
	}
	for _, tc := range tests {
		if got := systemdStatus(tc.state, tc.err, tc.pid); got != tc.want {
			t.Fatalf("systemdStatus(%q, %v, %q)=%q want %q", tc.state, tc.err, tc.pid, got, tc.want)
		}
	}
}

// relay status names the manager of this OS in its first line.
func TestRelayStatusNamesTheManager(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	kind := "launchd agent"
	if runtime.GOOS == "linux" {
		kind = "systemd user service"
	}
	var out bytes.Buffer
	if code := run([]string{"relay", "status"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.HasPrefix(out.String(), kind+": not installed ("+serviceFileIn(dir)+")\n") {
		t.Fatalf("out=%s", out.String())
	}
	if code := run([]string{"relay", "install", "--no-load"}, strings.NewReader(""), io.Discard, io.Discard); code != 0 {
		t.Fatal("install")
	}
	out.Reset()
	if code := run([]string{"relay", "status"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	// with the manager disabled the file exists but nothing runs it
	if !strings.HasPrefix(out.String(), kind+": ") || strings.Contains(out.String(), "not installed") {
		t.Fatalf("out=%s", out.String())
	}
}

// setup --check reports the service file for this OS.
func TestSetupCheckReportsServiceFile(t *testing.T) {
	dir := isolate(t)
	claudeHome(t)
	writeConfig(t, dir)
	kind := "launchd agent"
	if runtime.GOOS == "linux" {
		kind = "systemd user service"
	}
	var out bytes.Buffer
	if code := run([]string{"setup", "--check"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "relay: not installed (tg relay install)\n") {
		t.Fatalf("out=%s", out.String())
	}
	if code := run([]string{"relay", "install", "--no-load"}, strings.NewReader(""), io.Discard, io.Discard); code != 0 {
		t.Fatal("install")
	}
	out.Reset()
	if code := run([]string{"setup", "--check"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "relay: "+kind+" installed ("+serviceFileIn(dir)+")\n") {
		t.Fatalf("out=%s", out.String())
	}
	if _, err := os.Stat(serviceFileIn(dir)); err != nil {
		t.Fatal("service file missing after install --no-load")
	}
}

func TestServiceStateString(t *testing.T) {
	for s, want := range map[serviceState]string{serviceUnknown: "unknown", serviceInactive: "inactive", serviceActive: "active"} {
		if s.String() != want {
			t.Fatalf("%d.String()=%q want %q", int(s), s.String(), want)
		}
	}
	var zero serviceState
	if zero != serviceUnknown {
		t.Fatal("the zero state must commit to nothing")
	}
}

// is-active prints the unit's active state and exits non-zero for every
// state but active, reloading and refreshing, so the word decides; a word
// that is not a unit state (bus error, no systemctl) is unknown, with the
// error carrying systemctl's answer.
func TestSystemdState(t *testing.T) {
	exit1, exit3, exit4 := errors.New("exit status 1"), errors.New("exit status 3"), errors.New("exit status 4")
	tests := []struct {
		out     string
		err     error
		want    serviceState
		wantErr string
	}{
		{"active", nil, serviceActive, ""},
		{"active\n", nil, serviceActive, ""},
		{"reloading", nil, serviceActive, ""},
		{"refreshing", nil, serviceActive, ""},
		{"activating", exit3, serviceActive, ""},
		{"deactivating", exit3, serviceActive, ""},
		{"inactive", exit3, serviceInactive, ""},
		{"inactive", exit4, serviceInactive, ""}, // unit file not found: nothing runs it
		{"failed", exit3, serviceInactive, ""},
		{"maintenance", exit3, serviceUnknown, "systemctl --user is-active: maintenance"},
		{"Failed to connect to bus: No medium found", exit1, serviceUnknown, "systemctl --user is-active: Failed to connect to bus: No medium found"},
		{"", errors.New(`exec: "systemctl": executable file not found in $PATH`), serviceUnknown, `systemctl --user is-active: exec: "systemctl": executable file not found in $PATH`},
		{"", nil, serviceUnknown, "systemctl --user is-active: no answer"},
	}
	for _, tc := range tests {
		got, err := systemdState(tc.out, tc.err)
		if got != tc.want {
			t.Fatalf("systemdState(%q, %v)=%v want %v", tc.out, tc.err, got, tc.want)
		}
		gotErr := ""
		if err != nil {
			gotErr = err.Error()
		}
		if gotErr != tc.wantErr {
			t.Fatalf("systemdState(%q, %v) err=%q want %q", tc.out, tc.err, gotErr, tc.wantErr)
		}
	}
}
