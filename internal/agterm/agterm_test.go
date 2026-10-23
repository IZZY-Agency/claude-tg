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
