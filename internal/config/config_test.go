package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

func TestVoiceCommandRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude-tg", "config.json")
	want := &Config{BotToken: "123:abc", ChatID: 1, VoiceCommand: `whisper-cli "$TG_VOICE_FILE"`}
	if err := want.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.VoiceCommand != want.VoiceCommand {
		t.Fatalf("VoiceCommand=%q, want %q", got.VoiceCommand, want.VoiceCommand)
	}
	bare := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(bare, []byte(`{"bot_token":"1:x","chat_id":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = Load(bare)
	if err != nil {
		t.Fatal(err)
	}
	if got.VoiceCommand != "" {
		t.Fatalf("absent key loaded as %q", got.VoiceCommand)
	}
}

func TestSpeechCommandRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude-tg", "config.json")
	want := &Config{BotToken: "123:abc", ChatID: 1, SpeechCommand: `say-it "$TG_SPEECH_FILE"`}
	if err := want.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.SpeechCommand != want.SpeechCommand {
		t.Fatalf("SpeechCommand=%q, want %q", got.SpeechCommand, want.SpeechCommand)
	}
	empty := filepath.Join(t.TempDir(), "config.json")
	if err := (&Config{BotToken: "1:x", ChatID: 1}).Save(empty); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(empty)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "speech_command") {
		t.Fatalf("empty speech_command written: %s", b)
	}
}

func TestConfigTypesafeKeyRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude-tg", "config.json")
	want := &Config{BotToken: "123:abc", ChatID: 1, TypesafeAPIKey: "k-test"}
	if err := want.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.TypesafeAPIKey != want.TypesafeAPIKey {
		t.Fatalf("TypesafeAPIKey=%q, want %q", got.TypesafeAPIKey, want.TypesafeAPIKey)
	}
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), `"typesafe_api_key"`) {
		t.Fatalf("file lacks typesafe_api_key: %s, %v", b, err)
	}
	empty := filepath.Join(t.TempDir(), "config.json")
	if err := (&Config{BotToken: "1:x", ChatID: 1}).Save(empty); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(empty)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "typesafe_api_key") {
		t.Fatalf("empty typesafe_api_key written: %s", b)
	}
}

// Secrets are the configured values no message may show: the bot token
// and the TypeSafe key, those that are set.
func TestConfigSecrets(t *testing.T) {
	for _, tc := range []struct {
		cfg  Config
		want []string
	}{
		{Config{BotToken: "123:abc", TypesafeAPIKey: "k-test"}, []string{"123:abc", "k-test"}},
		{Config{BotToken: "123:abc"}, []string{"123:abc"}},
		{Config{TypesafeAPIKey: "k-test"}, []string{"k-test"}},
		{Config{}, nil},
	} {
		if got := tc.cfg.Secrets(); !slices.Equal(got, tc.want) {
			t.Errorf("Secrets() of %+v = %q, want %q", tc.cfg, got, tc.want)
		}
	}
}
