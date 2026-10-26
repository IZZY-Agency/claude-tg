package relaytest

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/IZZY-Agency/claude-tg/internal/term"
)

// The fake types into the input prompt line only: the "❯" line between the
// input box's borders. A picker's cursor line "❯ 1. …" at column 0, first
// on a screen with no prompt line above it, is left alone and the picker
// swallows the keystrokes, as Claude Code does.
func TestRenderReplacesOnlyTheInputPromptLine(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile("../../term/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	idle, ticked := read("idle-screen.txt"), read("picker-multi-q1-ticked.txt")
	tm := &Term{Screens: map[string]string{"I": idle, "P": ticked}}
	ctx := context.Background()
	for _, id := range []string{"I", "P"} {
		if err := tm.Type(ctx, term.Pane{Backend: "agterm", Target: id}, "hi"); err != nil {
			t.Fatal(err)
		}
	}
	shown, _ := tm.Screen(ctx, term.Pane{Backend: "agterm", Target: "I"}, 40)
	if !strings.Contains(shown, "\n❯ hi\n") || strings.Count(shown, "\n❯") != 1 {
		t.Fatalf("idle screen after typing:\n%s", shown)
	}
	shown, _ = tm.Screen(ctx, term.Pane{Backend: "agterm", Target: "P"}, 40)
	if shown != ticked || tm.Input("P") != "" {
		t.Fatalf("the picker's cursor line was taken for the input prompt: input=%q\n%s", tm.Input("P"), shown)
	}
}

// A reaction on a done ctx fails and is not recorded, as a request would.
func TestReactionOnDoneContextFails(t *testing.T) {
	a := &API{}
	ctx, cancel := context.WithCancel(context.Background())
	if err := a.SetMessageReaction(ctx, 1, 500, "👀"); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := a.SetMessageReaction(ctx, 1, 500, ""); err == nil {
		t.Fatal("a reaction on a cancelled ctx succeeded")
	}
	if got := a.Reacted(); len(got) != 1 || got[0] != (Reaction{500, "👀"}) {
		t.Fatalf("reactions = %+v", got)
	}
}
