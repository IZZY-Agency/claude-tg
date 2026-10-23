package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "claude-tg")
	path := filepath.Join(dir, "config.json")
	want := &Config{
		BotToken: "123:abc", ChatID: 123456789, UserID: 123456789, BotUsername: "acme_notify_bot",
		ClaudePath: "/Users/x/.local/bin/claude", BinaryPath: "/Users/x/.local/bin/tg",
		AwayAfterSeconds: 90, InstalledAt: time.Date(2026, 10, 22, 15, 0, 0, 0, time.UTC),
	}
	if err := want.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if *got != *want {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file perm %o, want 600", fi.Mode().Perm())
	}
	di, _ := os.Stat(dir)
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir perm %o, want 700", di.Mode().Perm())
	}
}

func TestLoadMissingIsErrNotConfigured(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

func TestDefaultAwayAfterSeconds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"bot_token":"t","chat_id":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.AwayAfterSeconds != 120 {
		t.Fatalf("AwayAfterSeconds=%d, want 120", got.AwayAfterSeconds)
	}
}

func TestDefaultPathsHonourXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xc")
	t.Setenv("XDG_STATE_HOME", "/tmp/xs")
	if DefaultPath() != "/tmp/xc/claude-tg/config.json" {
		t.Fatalf("DefaultPath=%q", DefaultPath())
	}
	if StateDir() != "/tmp/xs/claude-tg" {
		t.Fatalf("StateDir=%q", StateDir())
	}
}
