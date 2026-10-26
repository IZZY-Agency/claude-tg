package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

// The entries Claude Code writes for a session's title: one it generates,
// and one the owner sets with /rename.
func aiTitle(s string) string {
	return `{"type":"ai-title","aiTitle":` + quote(s) + `,"sessionId":"s"}` + "\n"
}

func customTitle(s string) string {
	return `{"type":"custom-title","customTitle":` + quote(s) + `,"sessionId":"s"}` + "\n"
}

func title(t *testing.T, content string) (string, bool) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Title(path)
	if err != nil {
		t.Fatal(err)
	}
	return got, ok
}

// A session's title is the newest one the owner set with /rename, else the
// newest one Claude Code generated; none without either. A last line still
// being written leaves it unknown, as the other reads do.
func TestTitle(t *testing.T) {
	turn := prompt(" What is the weather?", "2026-10-25T17:16:31.123Z") + others
	for _, c := range []struct {
		name, content, want string
		ok                  bool
	}{
		{"the newest generated title", aiTitle("Ready check") + turn + aiTitle("Login token claim failure on prod") + turn, "Login token claim failure on prod", true},
		{"a title set by /rename wins over newer generated ones", aiTitle("Ready check") + customTitle("tg-dup") + turn + aiTitle("Telegram buttons") + turn, "tg-dup", true},
		{"the newest title set by /rename", customTitle("first") + turn + customTitle("second") + aiTitle("generated"), "second", true},
		{"no title", turn, "", false},
		{"a blank title is none", aiTitle("  ") + turn, "", false},
		{"a title spread over lines is one line", aiTitle("Telegram\nbuttons"), "Telegram buttons", true},
		{"a last line still being written", aiTitle("Ready check") + turn[:len(turn)/2], "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, ok := title(t, c.content); got != c.want || ok != c.ok {
				t.Fatalf("Title = %q, %v; want %q, %v", got, ok, c.want, c.ok)
			}
		})
	}
}

// A transcript that cannot be read is an error.
func TestTitleMissingFile(t *testing.T) {
	if _, ok, err := Title(filepath.Join(t.TempDir(), "gone.jsonl")); err == nil || ok {
		t.Fatalf("ok %v err %v, want an error", ok, err)
	}
}
