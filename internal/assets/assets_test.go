package assets

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallSkillCreatesMarkerAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	changed, err := InstallSkill(dir, false)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	for _, f := range []string{"SKILL.md", MarkerFile} {
		if _, err := os.Stat(filepath.Join(dir, SkillName, f)); err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
	}
	changed, err = InstallSkill(dir, false)
	if err != nil || changed {
		t.Fatalf("second install changed=%v err=%v", changed, err)
	}
}

func TestInstallSkillRefusesUnmanagedDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, SkillName), 0o755); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(dir, SkillName, "SKILL.md")
	if err := os.WriteFile(own, []byte("user's own skill"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallSkill(dir, false); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("want ErrUnmanaged, got %v", err)
	}
	if b, _ := os.ReadFile(own); string(b) != "user's own skill" {
		t.Fatal("refused install must not touch the file")
	}
	if _, err := InstallSkill(dir, true); err != nil {
		t.Fatalf("force: %v", err)
	}
}

func TestRemoveSkillOnlyManaged(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, SkillName), 0o755); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveSkill(dir)
	if err != nil || removed {
		t.Fatalf("unmanaged dir removed=%v err=%v", removed, err)
	}
	if _, err := InstallSkill(dir, true); err != nil {
		t.Fatal(err)
	}
	removed, err = RemoveSkill(dir)
	if err != nil || !removed {
		t.Fatalf("managed dir removed=%v err=%v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, SkillName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("skill dir still exists")
	}
}

func TestClaudeMDBlockIdempotentAndRemovable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "CLAUDE.md")
	before := "- existing rule one\n- existing rule two\n"
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := InstallClaudeMD(path)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), StartMarker) || !strings.Contains(string(b), "telegram-notify") || !strings.HasPrefix(string(b), before) {
		t.Fatalf("content after install:\n%s", b)
	}
	changed, err = InstallClaudeMD(path)
	if err != nil || changed {
		t.Fatalf("second install changed=%v err=%v", changed, err)
	}
	changed, err = RemoveClaudeMD(path)
	if err != nil || !changed {
		t.Fatalf("remove changed=%v err=%v", changed, err)
	}
	if b, _ := os.ReadFile(path); string(b) != before {
		t.Fatalf("remove did not restore:\n%q\nwant\n%q", b, before)
	}
}

func TestInstallClaudeMDCreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "CLAUDE.md")
	if _, err := InstallClaudeMD(path); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), EndMarker) {
		t.Fatalf("content: %q", b)
	}
}

func TestSkillFrontmatterName(t *testing.T) {
	b, err := FS.ReadFile("skills/telegram-notify/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.HasPrefix(s, "---\n") || !strings.Contains(s, "\nname: telegram-notify\n") {
		t.Fatalf("frontmatter: %q", s[:min(len(s), 200)])
	}
	desc := s[strings.Index(s, "description:"):]
	desc = desc[:strings.Index(desc, "\n")]
	for _, trigger := range []string{"Telegram", "notify", "monitor", "follow", "critical"} {
		if !strings.Contains(strings.ToLower(desc), strings.ToLower(trigger)) {
			t.Fatalf("description lacks trigger %q: %s", trigger, desc)
		}
	}
	for _, section := range []string{"tg send", "tg follow on", "tg sessions", "--level info", "on its own line"} {
		if !strings.Contains(s, section) {
			t.Fatalf("skill body lacks %q", section)
		}
	}
	// A chained command falls outside the narrow allow rules and prompts.
	for _, bad := range []string{"command -v tg &&", "&& tg "} {
		if strings.Contains(s, bad) {
			t.Fatalf("skill must not teach chained tg commands: %q", bad)
		}
	}
}

// An end marker at EOF without a newline must not panic.
func TestClaudeMDEndMarkerAtEOF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "CLAUDE.md")
	content := "- rule\n\n" + StartMarker + "\nstale line\n" + EndMarker
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := InstallClaudeMD(path)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if _, err := RemoveClaudeMD(path); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "- rule\n" {
		t.Fatalf("after remove: %q", b)
	}
}
