package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

// recordConflict writes a relay state that says another program has
// been reading the bot's updates for 20 minutes.
func recordConflict(t *testing.T, dir string) {
	t.Helper()
	st, err := state.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveRelay(state.RelayState{PollConflictSince: time.Now().Add(-20 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
}

const conflictLine = "poll conflict: another program has been reading this bot's updates since 20m ago"

func TestRelayStatusShowsPollConflict(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	recordConflict(t, dir)
	var out bytes.Buffer
	if code := run([]string{"relay", "status"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out.String(), conflictLine) {
		t.Fatalf("out=%q", out.String())
	}
}

func TestSetupCheckShowsPollConflict(t *testing.T) {
	dir := isolate(t)
	claudeHome(t)
	writeConfig(t, dir)
	recordConflict(t, dir)
	var out bytes.Buffer
	if code := run([]string{"setup", "--check"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out.String(), conflictLine) {
		t.Fatalf("out=%q", out.String())
	}
}

func TestStatusQuietWithoutPollConflict(t *testing.T) {
	dir := isolate(t)
	claudeHome(t)
	writeConfig(t, dir)
	for _, args := range [][]string{{"relay", "status"}, {"setup", "--check"}} {
		var out bytes.Buffer
		run(args, strings.NewReader(""), &out, io.Discard)
		if strings.Contains(out.String(), "poll conflict") {
			t.Fatalf("%v: %q", args, out.String())
		}
	}
}

// enablePlugin turns the Telegram channel plugin on in the test's
// ~/.claude/settings.json and gives it token in its .env.
func enablePlugin(t *testing.T, home string, enabled bool, token string) {
	t.Helper()
	p := filepath.Join(home, "settings.json")
	src, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(src, &s); err != nil {
		t.Fatal(err)
	}
	plugins, _ := s["enabledPlugins"].(map[string]any)
	if plugins == nil {
		plugins = map[string]any{}
	}
	plugins["telegram@claude-plugins-official"] = enabled
	s["enabledPlugins"] = plugins
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(home, "channels", "telegram", ".env")
	if err := os.MkdirAll(filepath.Dir(env), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env, []byte("TELEGRAM_BOT_TOKEN="+token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

const wantPluginWarning = "telegram plugin: enabled with this bot's token"

func TestSetupCheckWarnsAboutTelegramPlugin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		token   string
		warn    bool
	}{
		{"enabled, same bot", true, "1:x", true},
		{"disabled", false, "1:x", false},
		{"enabled, another bot", true, "2:y", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := isolate(t)
			home := claudeHome(t)
			writeConfig(t, dir) // bot_token 1:x
			enablePlugin(t, home, tc.enabled, tc.token)
			var out bytes.Buffer
			if code := run([]string{"setup", "--check"}, strings.NewReader(""), &out, io.Discard); code != 0 {
				t.Fatalf("code=%d out=%s", code, out.String())
			}
			if got := strings.Contains(out.String(), wantPluginWarning); got != tc.warn {
				t.Fatalf("warning=%v, want %v:\n%s", got, tc.warn, out.String())
			}
			if tc.warn && !strings.Contains(out.String(), "claude plugin disable telegram@claude-plugins-official") {
				t.Fatalf("the warning does not say how to turn it off:\n%s", out.String())
			}
		})
	}
}

func TestSetupWarnsAboutTelegramPlugin(t *testing.T) {
	dir := isolate(t)
	home := claudeHome(t)
	enablePlugin(t, home, true, "1:x")
	fakeTelegramForSetup(t)
	out, errb, code := runSetupKey(t)
	if code != 0 {
		t.Fatalf("code=%d err=%s", code, errb)
	}
	if !strings.Contains(out+errb, wantPluginWarning) {
		t.Fatalf("setup with the plugin's own token did not warn:\nout=%s\nerr=%s", out, errb)
	}
	_ = dir
}

func TestDiscoverChatTellsConflictsApart(t *testing.T) {
	for _, tc := range []struct{ desc, want string }{
		{"Conflict: terminated by other getUpdates request; make sure that only one bot instance is running", "another program is reading this bot's updates"},
		{"Conflict: can't use getUpdates method while webhook is active; use deleteWebhook to delete the webhook first", "a webhook is active for this bot"},
	} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `{"ok":false,"error_code":409,"description":"`+tc.desc+`"}`)
		}))
		var errb bytes.Buffer
		c := &cli{stdin: strings.NewReader(""), stdout: io.Discard, stderr: &errb, env: os.Getenv}
		client := &telegram.Client{Token: "1:x", HTTP: s.Client(), BaseURL: s.URL}
		_, _, code := discoverChat(c, context.Background(), client, "fakebot", 0, true)
		s.Close()
		if code == exitOK || !strings.Contains(errb.String(), tc.want) {
			t.Errorf("%q: code=%d err=%q, want it to say %q", tc.desc, code, errb.String(), tc.want)
		}
	}
}
