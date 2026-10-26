package agterm

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/IZZY-Agency/claude-tg/internal/term"
)

var _ term.Terminal = (*CLI)(nil)

func TestCLIExistsLive(t *testing.T) {
	id := os.Getenv("AGTERM_SESSION_ID")
	if _, err := exec.LookPath("agtermctl"); err != nil || id == "" {
		t.Skip("needs agtermctl and a session id")
	}
	c := &CLI{}
	if !c.Exists(context.Background(), term.Pane{Backend: Backend, Target: id}) {
		t.Fatalf("this session %s should exist", id)
	}
	if c.Exists(context.Background(), term.Pane{Backend: Backend, Target: "00000000-0000-0000-0000-000000000000"}) {
		t.Fatal("a made-up id must not exist")
	}
}

// An agterm tree, as `agtermctl tree --json` prints it (trimmed): one tab
// the owner named, one carrying Claude Code's terminal title.
const treeJSON = `{"ok":true,"result":{"tree":{"workspaces":[{"id":"W1","name":"work","sessions":[` +
	`{"id":"37E65173-A408-4C8A-B459-A198E80C798E","name":"Nova","title":"✳ NOV-132","cwd":"/Users/alex/work/nova"},` +
	`{"id":"F0B23286-0202-4D2D-9A15-37A3E8BA907F","name":"✳ Login token claim failure on prod","title":"✳ Login token claim failure on prod"}]},` +
	`{"id":"W2","name":"izzy","sessions":[{"id":"E053ADA7-9CFE-4D2F-91CA-C396CC919FE2","name":"Ultracoins","title":"⠂ Ultracoins loyalty rewards"}]}]}}}`

// A tab is found by its session id, ignoring case, in any workspace.
func TestFindTab(t *testing.T) {
	for _, c := range []struct {
		id   string
		want Tab
		ok   bool
	}{
		{"37e65173-a408-4c8a-b459-a198e80c798e", Tab{Name: "Nova", Title: "✳ NOV-132"}, true},
		{"E053ADA7-9CFE-4D2F-91CA-C396CC919FE2", Tab{Name: "Ultracoins", Title: "⠂ Ultracoins loyalty rewards"}, true},
		{"00000000-0000-0000-0000-000000000000", Tab{}, false},
		{"", Tab{}, false},
	} {
		if got, ok := findTab([]byte(treeJSON), c.id); got != c.want || ok != c.ok {
			t.Errorf("findTab(%q) = %+v, %v; want %+v, %v", c.id, got, ok, c.want, c.ok)
		}
	}
	if _, ok := findTab([]byte("not json"), "37E65173-A408-4C8A-B459-A198E80C798E"); ok {
		t.Error("found a tab in a tree that is not JSON")
	}
}
