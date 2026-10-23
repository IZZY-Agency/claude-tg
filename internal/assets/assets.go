// Package assets embeds the skill and CLAUDE.md snippet tg installs and
// knows how to put them in place and take them out again.
package assets

import (
	"bytes"
	"embed"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

//go:embed skills claude-md-snippet.md
var FS embed.FS

const (
	// SkillName is the directory under ~/.claude/skills.
	SkillName = "telegram-notify"
	// MarkerFile marks a skill directory as owned by tg.
	MarkerFile = ".tg-managed"
	// StartMarker and EndMarker delimit the block tg owns in CLAUDE.md.
	StartMarker = "<!-- claude-tg:start -->"
	EndMarker   = "<!-- claude-tg:end -->"
)

// ErrUnmanaged is returned when the skill directory exists but tg did not create it.
var ErrUnmanaged = errors.New("skill directory exists and is not managed by tg (use --force to overwrite)")

func skillFiles() (map[string][]byte, error) {
	entries, err := FS.ReadDir("skills/" + SkillName)
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := FS.ReadFile("skills/" + SkillName + "/" + e.Name())
		if err != nil {
			return nil, err
		}
		out[e.Name()] = b
	}
	return out, nil
}

// InstallSkill writes the embedded skill into skillsDir/<SkillName>. It
// refuses to overwrite a directory without the marker unless force is set.
func InstallSkill(skillsDir string, force bool) (bool, error) {
	dir := filepath.Join(skillsDir, SkillName)
	marker := filepath.Join(dir, MarkerFile)
	if _, err := os.Stat(dir); err == nil {
		if _, err := os.Stat(marker); err != nil && !force {
			return false, ErrUnmanaged
		}
	}
	files, err := skillFiles()
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	changed := false
	for name, content := range files {
		path := filepath.Join(dir, name)
		if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, content) {
			continue
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			return changed, err
		}
		changed = true
	}
	if _, err := os.Stat(marker); err != nil {
		if err := os.WriteFile(marker, []byte("managed by tg; do not edit files here by hand\n"), 0o644); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// RemoveSkill deletes the skill directory when tg owns it.
func RemoveSkill(skillsDir string) (bool, error) {
	dir := filepath.Join(skillsDir, SkillName)
	if _, err := os.Stat(filepath.Join(dir, MarkerFile)); err != nil {
		return false, nil
	}
	return true, os.RemoveAll(dir)
}

func snippet() string {
	b, _ := FS.ReadFile("claude-md-snippet.md")
	return strings.TrimRight(string(b), "\n")
}

func block() string {
	return StartMarker + "\n" + snippet() + "\n" + EndMarker + "\n"
}

// InstallClaudeMD appends or refreshes the marked block in the file at path.
func InstallClaudeMD(path string) (bool, error) {
	cur, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	s := string(cur)
	want := block()
	if i := strings.Index(s, StartMarker); i >= 0 {
		j := strings.Index(s[i:], EndMarker)
		if j < 0 {
			return false, errors.New("CLAUDE.md has a start marker without an end marker")
		}
		end := i + j + len(EndMarker)
		if end < len(s) && s[end] == '\n' {
			end++
		}
		if s[i:end] == want {
			return false, nil
		}
		s = s[:i] + want + s[end:]
		return true, os.WriteFile(path, []byte(s), 0o644)
	}
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	if s != "" {
		s += "\n"
	}
	s += want
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, []byte(s), 0o644)
}

// RemoveClaudeMD deletes the marked block and the blank line before it.
func RemoveClaudeMD(path string) (bool, error) {
	cur, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	s := string(cur)
	i := strings.Index(s, StartMarker)
	if i < 0 {
		return false, nil
	}
	j := strings.Index(s[i:], EndMarker)
	if j < 0 {
		return false, errors.New("CLAUDE.md has a start marker without an end marker")
	}
	end := i + j + len(EndMarker)
	if end < len(s) && s[end] == '\n' {
		end++
	}
	before := s[:i]
	before = strings.TrimSuffix(before, "\n") // the separating blank line
	if before != "" && !strings.HasSuffix(before, "\n") {
		before += "\n"
	}
	return true, os.WriteFile(path, []byte(before+s[end:]), 0o644)
}
