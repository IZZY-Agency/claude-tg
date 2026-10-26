package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/assets"
	"github.com/IZZY-Agency/claude-tg/internal/config"
	"github.com/IZZY-Agency/claude-tg/internal/settings"
)

func claudeHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "claude")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("../../internal/settings/testdata/settings.before.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "settings.json"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "CLAUDE.md"), []byte("- my rule\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TG_CLAUDE_HOME", home)
	return home
}

func TestTokenSourceOrder(t *testing.T) {
	dir := t.TempDir()
	channel := filepath.Join(dir, "channel.env")
	if err := os.WriteFile(channel, []byte("TELEGRAM_BOT_TOKEN=4:channel\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		flag, env, cfg, want, source string
	}{
		{"1:flag", "2:env", "3:cfg", "1:flag", "flag"},
		{"", "2:env", "3:cfg", "2:env", "TELEGRAM_BOT_TOKEN"},
		{"", "", "3:cfg", "3:cfg", "existing config"},
		{"", "", "", "4:channel", channel},
	}
	for _, tc := range cases {
		got, src := resolveToken(tc.flag, tc.env, tc.cfg, channel)
		if got != tc.want || src != tc.source {
			t.Fatalf("flag=%q env=%q cfg=%q: got %q from %q, want %q from %q", tc.flag, tc.env, tc.cfg, got, src, tc.want, tc.source)
		}
	}
	got, _ := resolveToken("", "", "", filepath.Join(dir, "missing.env"))
	if got != "" {
		t.Fatalf("missing channel file: got %q", got)
	}
}

func TestSetupCheckChangesNothing(t *testing.T) {
	isolate(t)
	home := claudeHome(t)
	before, _ := os.ReadFile(filepath.Join(home, "settings.json"))
	var out bytes.Buffer
	code := run([]string{"setup", "--check"}, strings.NewReader(""), &out, io.Discard)
	if code != 0 {
		t.Fatalf("code=%d out=%s", code, out.String())
	}
	after, _ := os.ReadFile(filepath.Join(home, "settings.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("--check modified settings.json")
	}
	if _, err := os.Stat(filepath.Join(home, "skills", assets.SkillName)); err == nil {
		t.Fatal("--check installed the skill")
	}
	for _, want := range []string{"config: missing", "hooks: 0/6", "allow rules: 0/4", "skill: missing", "CLAUDE.md: missing"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestSetupNoTTYNoChatIDExits1(t *testing.T) {
	isolate(t)
	claudeHome(t)
	var errb bytes.Buffer
	code := run([]string{"setup", "--token", "1:x", "--yes"}, strings.NewReader(""), io.Discard, &errb)
	if code != exitUsage || !strings.Contains(errb.String(), "--chat-id") {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
}

func TestRemoveRestoresSettings(t *testing.T) {
	dir := isolate(t)
	home := claudeHome(t)
	writeConfig(t, dir)
	original, _ := os.ReadFile(filepath.Join(home, "settings.json"))
	bin := "/Users/alex/.local/bin/tg"
	installed, _, err := settings.Install(original, bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "settings.json"), installed, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := assets.InstallSkill(filepath.Join(home, "skills"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := assets.InstallClaudeMD(filepath.Join(home, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	// Point the config's binary path at the installed hooks so remove knows the prefix.
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"bot_token":"1:x","chat_id":1,"binary_path":"`+bin+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := run([]string{"remove"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d out=%s", code, out.String())
	}
	got, _ := os.ReadFile(filepath.Join(home, "settings.json"))
	o, _ := settings.Parse(original)
	want, _ := settings.Marshal(o, settings.DetectIndent(original), bytes.HasSuffix(original, []byte("\n")))
	if !bytes.Equal(got, want) {
		t.Fatalf("settings not restored:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(home, "skills", assets.SkillName)); err == nil {
		t.Fatal("skill still installed")
	}
	if b, _ := os.ReadFile(filepath.Join(home, "CLAUDE.md")); string(b) != "- my rule\n" {
		t.Fatalf("CLAUDE.md not restored: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatal("remove without --purge must keep the config")
	}
	if code := run([]string{"remove", "--purge"}, strings.NewReader(""), io.Discard, io.Discard); code != 0 {
		t.Fatalf("purge code=%d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
		t.Fatal("--purge must delete the config")
	}
}

func TestSetupNonInteractiveInstallsEverythingButSkipsNetworkWithCheck(t *testing.T) {
	// A full non-interactive setup needs the real bot (GetMe); covered manually in Task 14.
	t.Skip("full setup exercised live in Task 14")
}

func TestFindClaudeKeepsLauncherPath(t *testing.T) {
	// ~/.local/bin/claude is a symlink into a versioned directory that changes
	// on every update; the config must keep the stable launcher path.
	dir := t.TempDir()
	real := filepath.Join(dir, "versions", "2.1.308")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(dir, "claude")
	if err := os.Symlink(real, launcher); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if got := findClaude(); got != launcher {
		t.Fatalf("findClaude()=%q, want the launcher %q", got, launcher)
	}
}

// --check must not create state directories.
func TestSetupCheckCreatesNoStateDir(t *testing.T) {
	isolate(t)
	claudeHome(t)
	stateDir := os.Getenv("TG_STATE_DIR")
	if code := run([]string{"setup", "--check"}, strings.NewReader(""), io.Discard, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if _, err := os.Stat(stateDir); err == nil {
		t.Fatal("--check created the state directory")
	}
}

// settings rewrites are atomic and backups keep the original mode.
func TestRemoveBackupKeepsOriginalMode(t *testing.T) {
	dir := isolate(t)
	home := claudeHome(t)
	writeConfig(t, dir)
	settingsPath := filepath.Join(home, "settings.json")
	original, _ := os.ReadFile(settingsPath)
	installed, _, _ := settings.Install(original, "/Users/alex/.local/bin/tg")
	if err := os.WriteFile(settingsPath, installed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(settingsPath, 0o600); err != nil { // WriteFile keeps an existing file's mode
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"bot_token":"1:x","chat_id":1,"binary_path":"/Users/alex/.local/bin/tg"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"remove"}, strings.NewReader(""), io.Discard, io.Discard); code != 0 {
		t.Fatalf("code=%d", code)
	}
	backups, _ := filepath.Glob(settingsPath + ".bak-*")
	if len(backups) != 1 {
		t.Fatalf("backups=%v", backups)
	}
	if fi, _ := os.Stat(backups[0]); fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %o, want 600", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(settingsPath); fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode %o after rewrite, want 600", fi.Mode().Perm())
	}
	if tmps, _ := filepath.Glob(filepath.Join(home, "*.tmp*")); len(tmps) != 0 {
		t.Fatalf("temp files left behind: %v", tmps)
	}
}

func TestSetupCheckReportsOutdatedPermissionHook(t *testing.T) {
	dir := isolate(t)
	home := claudeHome(t)
	bin := "/Users/alex/.local/bin/tg"
	cfg := `{"bot_token":"1:x","chat_id":1,"binary_path":"` + bin + `"}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	old := `{"hooks":{"PermissionRequest":[{"hooks":[{"type":"command","command":"` + bin + ` hook permission-request","async":true}]}]}}`
	if err := os.WriteFile(filepath.Join(home, "settings.json"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := run([]string{"setup", "--check"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("code=%d out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "outdated: PermissionRequest") || strings.Contains(out.String(), "missing: PermissionRequest") {
		t.Fatalf("output lacks the outdated line:\n%s", out.String())
	}
}

func TestSetupCheckOutdatedLineHasNoDoubleSpace(t *testing.T) {
	dir := isolate(t)
	home := claudeHome(t)
	bin := "/Users/alex/.local/bin/tg"
	cfg := `{"bot_token":"1:x","chat_id":1,"binary_path":"` + bin + `"}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	old := `{"hooks":{"PermissionRequest":[{"hooks":[{"type":"command","command":"` + bin + ` hook permission-request","async":true}]}]}}`
	if err := os.WriteFile(filepath.Join(home, "settings.json"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	run([]string{"setup", "--check"}, strings.NewReader(""), &out, io.Discard)
	if !strings.Contains(out.String(), "  outdated: PermissionRequest (run `tg setup` to update it)") {
		t.Fatalf("outdated line malformed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "  missing: Stop\n") {
		t.Fatalf("missing line malformed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "hooks: 0/6 installed") {
		t.Fatalf("count wrong:\n%s", out.String())
	}
}

// A re-run of `tg setup` rebuilds the config; the owner's voice_command must
// survive it.
func TestSetupKeepsVoiceCommand(t *testing.T) {
	existing := &config.Config{AwayAfterSeconds: 30, VoiceCommand: "transcribe-it", SpeechCommand: "speak-it"}
	cfg := &config.Config{AwayAfterSeconds: config.DefaultAwayAfterSeconds, InstalledAt: time.Now()}
	carryOver(cfg, existing)
	if cfg.VoiceCommand != "transcribe-it" || cfg.SpeechCommand != "speak-it" || cfg.AwayAfterSeconds != 30 {
		t.Fatalf("carryOver lost fields: %+v", cfg)
	}
	before := *cfg
	carryOver(cfg, nil)
	if *cfg != before {
		t.Fatalf("carryOver(cfg, nil) changed the config: %+v", cfg)
	}
}

func TestSetupCheckVoiceLine(t *testing.T) {
	for _, tc := range []struct{ cfg, want string }{
		{`{"bot_token":"1:x","chat_id":1,"voice_command":"cat x"}`, "voice: configured\n"},
		{`{"bot_token":"1:x","chat_id":1,"speech_command":"say x"}`, "voice: not configured (see the README)\nspeech: configured\n"},
		{`{"bot_token":"1:x","chat_id":1}`, "voice: not configured (see the README)\nspeech: not configured (see the README)\n"},
	} {
		dir := isolate(t)
		claudeHome(t)
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(tc.cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if code := run([]string{"setup", "--check"}, strings.NewReader(""), &out, io.Discard); code != 0 {
			t.Fatalf("code=%d out=%s", code, out.String())
		}
		o := out.String()
		i, j := strings.Index(o, "relay:"), strings.Index(o, tc.want)
		if i < 0 || j < i {
			t.Fatalf("want %q after the relay line:\n%s", tc.want, o)
		}
	}
}

// fakeTelegramForSetup answers getMe and sendMessage so a full non-interactive
// setup runs without the network.
func fakeTelegramForSetup(t *testing.T) {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true,"result":{"id":7,"username":"fakebot","message_id":1,"date":1,"chat":{"id":1}}}`)
	}))
	t.Cleanup(s.Close)
	old := setupTelegramBaseURL
	setupTelegramBaseURL = s.URL
	t.Cleanup(func() { setupTelegramBaseURL = old })
}

func runSetupKey(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(append([]string{"setup", "--token", "1:x", "--chat-id", "1", "--yes", "--no-claude-md"}, args...), strings.NewReader(""), &out, &errb)
	return out.String(), errb.String(), code
}

func savedKey(t *testing.T, dir string) string {
	t.Helper()
	cfg, err := config.Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.TypesafeAPIKey
}

func modelsServer(t *testing.T, status int) {
	t.Helper()
	fakeTypesafe(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		w.WriteHeader(status)
		io.WriteString(w, `{"data":[]}`)
	})
}

func TestSetupTypesafeKeyFromFlag(t *testing.T) {
	dir := isolate(t)
	claudeHome(t)
	fakeTelegramForSetup(t)
	modelsServer(t, 200)
	out, errb, code := runSetupKey(t, "--typesafe-key", "k-1")
	if code != 0 || !strings.Contains(out, "TypeSafe: key accepted\n") {
		t.Fatalf("code=%d out=%s err=%s", code, out, errb)
	}
	if savedKey(t, dir) != "k-1" {
		t.Fatalf("saved %q", savedKey(t, dir))
	}
	if strings.Contains(out+errb, "k-1") {
		t.Fatalf("the key was printed:\n%s%s", out, errb)
	}
}

func TestSetupTypesafeKeyRejected(t *testing.T) {
	dir := isolate(t)
	claudeHome(t)
	fakeTelegramForSetup(t)
	modelsServer(t, 401)
	out, _, code := runSetupKey(t, "--typesafe-key", "k-bad")
	if code != 0 || !strings.Contains(out, "TypeSafe: key rejected; not saved\n") || savedKey(t, dir) != "" {
		t.Fatalf("code=%d out=%s saved=%q", code, out, savedKey(t, dir))
	}
}

func TestSetupTypesafeKeyUnchecked(t *testing.T) {
	dir := isolate(t)
	claudeHome(t)
	fakeTelegramForSetup(t)
	s := httptest.NewServer(http.NotFoundHandler())
	url := s.URL
	s.Close() // nothing listens any more
	old := classifyBaseURL
	classifyBaseURL = url
	t.Cleanup(func() { classifyBaseURL = old })
	out, errb, code := runSetupKey(t, "--typesafe-key", "k-4")
	if code != 0 || !strings.Contains(out, "TypeSafe: could not check the key (") || !strings.Contains(out, "); saved anyway\n") {
		t.Fatalf("code=%d out=%s err=%s", code, out, errb)
	}
	if savedKey(t, dir) != "k-4" || strings.Contains(out+errb, "k-4") {
		t.Fatalf("saved=%q out=%s", savedKey(t, dir), out)
	}
}

func TestSetupTypesafeKeyFromEnv(t *testing.T) {
	dir := isolate(t)
	claudeHome(t)
	fakeTelegramForSetup(t)
	modelsServer(t, 200)
	t.Setenv("TYPESAFE_API_KEY", "key-env-2")
	out, _, code := runSetupKey(t)
	if code != 0 || savedKey(t, dir) != "key-env-2" || strings.Contains(out, "key-env-2") {
		t.Fatalf("code=%d saved=%q out=%s", code, savedKey(t, dir), out)
	}
}

func TestSetupYesKeepsTypesafeKey(t *testing.T) {
	dir := isolate(t)
	claudeHome(t)
	fakeTelegramForSetup(t)
	modelsServer(t, 200)
	t.Setenv("TYPESAFE_API_KEY", "")
	writeKeyConfig(t, dir, "k-3")
	out, _, code := runSetupKey(t)
	if code != 0 || savedKey(t, dir) != "k-3" || strings.Contains(out, "TypeSafe API key for") {
		t.Fatalf("code=%d saved=%q out=%s", code, savedKey(t, dir), out)
	}
}

func TestSetupWithoutKeySavesNone(t *testing.T) {
	dir := isolate(t)
	claudeHome(t)
	fakeTelegramForSetup(t)
	t.Setenv("TYPESAFE_API_KEY", "")
	out, _, code := runSetupKey(t)
	if code != 0 || savedKey(t, dir) != "" || strings.Contains(out, "TypeSafe") {
		t.Fatalf("code=%d out=%s", code, out)
	}
}

func TestSetupCheckClassifierLine(t *testing.T) {
	for _, tc := range []struct{ cfg, want string }{
		{`{"bot_token":"1:x","chat_id":1,"typesafe_api_key":"k"}`, "speech: not configured (see the README)\nclassifier: configured (TypeSafe), unused until speech_command is set\n"},
		{`{"bot_token":"1:x","chat_id":1,"typesafe_api_key":"k","speech_command":"say x"}`, "speech: configured\nclassifier: configured (TypeSafe)\n"},
		{`{"bot_token":"1:x","chat_id":1}`, "speech: not configured (see the README)\nclassifier: not configured (typed answers are never spoken)\n"},
	} {
		dir := isolate(t)
		claudeHome(t)
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(tc.cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if code := run([]string{"setup", "--check"}, strings.NewReader(""), &out, io.Discard); code != 0 || !strings.Contains(out.String(), tc.want) {
			t.Fatalf("code=%d want %q in:\n%s", code, tc.want, out.String())
		}
	}
}

func TestChooseTypesafeKey(t *testing.T) {
	asked, prompted := 0, 0
	for _, tc := range []struct {
		name                 string
		flag, env, cfg       string
		tty, yes, reuse      bool
		typed, want          string
		wantAsked, wantPromp int
	}{
		{"flag wins", "f", "e", "c", true, false, true, "", "f", 0, 0},
		{"env over config", "", "e", "c", true, false, true, "", "e", 0, 0},
		{"tty reuse yes keeps", "", "", "c", true, false, true, "typed", "c", 1, 0},
		{"tty reuse no prompts", "", "", "c", true, false, false, "new", "new", 1, 1},
		{"tty reuse no, Enter keeps none", "", "", "c", true, false, false, "", "", 1, 1},
		{"tty no key prompts", "", "", "", true, false, false, "new", "new", 0, 1},
		{"tty no key, Enter skips", "", "", "", true, false, false, "", "", 0, 1},
		{"yes keeps existing, no prompt", "", "", "c", true, true, false, "typed", "c", 0, 0},
		{"yes with no key never prompts", "", "", "", true, true, false, "typed", "", 0, 0},
		{"no tty keeps existing", "", "", "c", false, false, false, "typed", "c", 0, 0},
		{"no tty no key", "", "", "", false, false, false, "typed", "", 0, 0},
	} {
		asked, prompted = 0, 0
		got := chooseTypesafeKey(tc.flag, tc.env, tc.cfg, tc.tty, tc.yes,
			func() bool { asked++; return tc.reuse },
			func() string { prompted++; return tc.typed })
		if got != tc.want || asked != tc.wantAsked || prompted != tc.wantPromp {
			t.Errorf("%s: got %q asked=%d prompted=%d, want %q %d %d", tc.name, got, asked, prompted, tc.want, tc.wantAsked, tc.wantPromp)
		}
	}
}

func TestSetupRejectedExistingKeyIsDropped(t *testing.T) {
	dir := isolate(t)
	claudeHome(t)
	fakeTelegramForSetup(t)
	modelsServer(t, 401)
	t.Setenv("TYPESAFE_API_KEY", "")
	writeKeyConfig(t, dir, "k-old-7")
	out, _, code := runSetupKey(t)
	if code != 0 || !strings.Contains(out, "TypeSafe: key rejected; not saved\n") || savedKey(t, dir) != "" {
		t.Fatalf("code=%d saved=%q out=%s", code, savedKey(t, dir), out)
	}
}

func TestSetupKeyWithoutSpeechCommandSaysSo(t *testing.T) {
	const note = "TypeSafe: used once speech_command is set (see the README)\n"
	dir := isolate(t)
	claudeHome(t)
	fakeTelegramForSetup(t)
	modelsServer(t, 200)
	out, _, code := runSetupKey(t, "--typesafe-key", "k-9")
	if code != 0 || !strings.Contains(out, "TypeSafe: key accepted\n"+note) {
		t.Fatalf("code=%d out=%s", code, out)
	}
	// With a speech command already configured the note is not printed.
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"bot_token":"1:x","chat_id":1,"speech_command":"say x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, code = runSetupKey(t, "--typesafe-key", "k-9")
	if code != 0 || !strings.Contains(out, "key accepted") || strings.Contains(out, "used once speech_command") {
		t.Fatalf("code=%d out=%s", code, out)
	}
}

func TestSetupTrimsKeyFromFlagAndEnv(t *testing.T) {
	dir := isolate(t)
	claudeHome(t)
	fakeTelegramForSetup(t)
	modelsServer(t, 200)
	if _, _, code := runSetupKey(t, "--typesafe-key", "k-flag \n"); code != 0 || savedKey(t, dir) != "k-flag" {
		t.Fatalf("code=%d saved=%q", code, savedKey(t, dir))
	}
	t.Setenv("TYPESAFE_API_KEY", " k-env\n")
	if _, _, code := runSetupKey(t); code != 0 || savedKey(t, dir) != "k-env" {
		t.Fatalf("code=%d saved=%q", code, savedKey(t, dir))
	}
}

func TestSetupUncheckedLineNamesTypesafeOnce(t *testing.T) {
	isolate(t)
	claudeHome(t)
	fakeTelegramForSetup(t)
	s := httptest.NewServer(http.NotFoundHandler())
	url := s.URL
	s.Close()
	old := classifyBaseURL
	classifyBaseURL = url
	t.Cleanup(func() { classifyBaseURL = old })
	out, _, _ := runSetupKey(t, "--typesafe-key", "k-5")
	if !strings.Contains(out, "TypeSafe: could not check the key (") || strings.Contains(out, "typesafe: ") {
		t.Fatalf("out=%s", out)
	}
}
