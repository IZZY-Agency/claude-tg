package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IZZY-Agency/claude-tg/internal/assets"
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
	if code != 1 || !strings.Contains(errb.String(), "--chat-id") {
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
