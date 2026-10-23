package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestRelayWithoutConfigExits2(t *testing.T) {
	isolate(t)
	if code := run([]string{"relay", "--once"}, strings.NewReader(""), io.Discard, io.Discard); code != 2 {
		t.Fatalf("code=%d", code)
	}
}

func TestRelayInstallPrintShowsPlist(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	var out bytes.Buffer
	if code := run([]string{"relay", "install", "--print"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	for _, want := range serviceFileCommandMarkers() {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("out lacks %q:\n%s", want, out.String())
		}
	}
}

func TestRelayInstallWritesPlistToOverrideDir(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	path := serviceFileIn(dir)
	var out bytes.Buffer
	if code := run([]string{"relay", "install", "--no-load"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d out=%s", code, out.String())
	}
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), serviceFileKeepAliveMarker()) {
		t.Fatalf("service file: err=%v content=%s", err, b)
	}
	if code := run([]string{"relay", "uninstall", "--no-load"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("uninstall code=%d", code)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("service file still present after uninstall")
	}
}

func TestRelayRejectsUnknownSubcommand(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	if code := run([]string{"relay", "bogus"}, strings.NewReader(""), io.Discard, io.Discard); code != 1 {
		t.Fatalf("code=%d", code)
	}
}

// v2: remove must take the relay agent down before anything else.
func TestRemoveUninstallsRelayAgent(t *testing.T) {
	dir := isolate(t)
	claudeHome(t)
	writeConfig(t, dir)
	if code := run([]string{"relay", "install", "--no-load"}, strings.NewReader(""), io.Discard, io.Discard); code != 0 {
		t.Fatalf("install code=%d", code)
	}
	var out bytes.Buffer
	if code := run([]string{"remove", "--purge"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("remove code=%d out=%s", code, out.String())
	}
	if _, err := os.Stat(serviceFileIn(dir)); err == nil {
		t.Fatal("relay service file still present after remove")
	}
	if !strings.Contains(out.String(), "Relay agent") {
		t.Fatalf("out=%s", out.String())
	}
}

// The agent must see the same config and state paths.
func TestRelayPlistCarriesConfigAndStatePaths(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	var out bytes.Buffer
	if code := run([]string{"relay", "install", "--print"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	for _, want := range serviceFileEnvMarkers(dir) {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("service file lacks %q:\n%s", want, out.String())
		}
	}
}
