package tmux

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/IZZY-Agency/claude-tg/internal/term"
)

var _ term.Terminal = (*CLI)(nil)

func TestCommandsSplitsTextAndKeys(t *testing.T) {
	cases := []struct {
		in   string
		want [][]string
	}{
		{" hello world", [][]string{{"-l", "--", " hello world"}}},
		{"\n", [][]string{{"Enter"}}},
		{"1\n", [][]string{{"-l", "--", "1"}, {"Enter"}}},
		{"\x1b", [][]string{{"Escape"}}},
		{"\x7f\x7f\x7f", [][]string{{"-N", "3", "BSpace"}}},
		{"\x7f\n", [][]string{{"BSpace"}, {"Enter"}}},
		// tmux's parser eats an unescaped trailing ";" of an argument, so
		// trailing semicolons go out as hex keys.
		{"hello;", [][]string{{"-l", "--", "hello"}, {"-H", "3b"}}},
		{"a;;", [][]string{{"-l", "--", "a"}, {"-N", "2", "-H", "3b"}}},
		{";", [][]string{{"-H", "3b"}}},
		{"x;y;", [][]string{{"-l", "--", "x;y"}, {"-H", "3b"}}},
		{"end\\;", [][]string{{"-l", "--", "end\\"}, {"-H", "3b"}}},
		{"a;\n", [][]string{{"-l", "--", "a"}, {"-H", "3b"}, {"Enter"}}},
		{" a\x7fb\n", [][]string{{"-l", "--", " a"}, {"BSpace"}, {"-l", "--", "b"}, {"Enter"}}},
		{" -n looks like a flag", [][]string{{"-l", "--", " -n looks like a flag"}}},
		{"", nil},
	}
	for _, c := range cases {
		if got := Commands(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Commands(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestTailKeepsLastLinesWithoutTrailingBlanks(t *testing.T) {
	out := "a\nb\nc\nd\n\n\n"
	if got := Tail(out, 2); got != "c\nd" {
		t.Fatalf("got %q", got)
	}
	if got := Tail(out, 10); got != "a\nb\nc\nd" {
		t.Fatalf("got %q", got)
	}
	if got := Tail("", 5); got != "" {
		t.Fatalf("got %q", got)
	}
	// Lines that are only spaces count as blank at the end, inner ones stay.
	if got := Tail("x\n   \ny\n   \n", 10); got != "x\n   \ny" {
		t.Fatalf("got %q", got)
	}
}

func TestArgsUseSocketWhenKnown(t *testing.T) {
	c := &CLI{}
	if got := c.args(term.Pane{Backend: Backend, Target: "%5", Socket: "/tmp/tmux-501/default"}, "send-keys", "-t", "%5", "Enter"); !reflect.DeepEqual(got, []string{"-S", "/tmp/tmux-501/default", "send-keys", "-t", "%5", "Enter"}) {
		t.Fatalf("got %v", got)
	}
	if got := c.args(term.Pane{Backend: Backend, Target: "%5"}, "display-message"); !reflect.DeepEqual(got, []string{"display-message"}) {
		t.Fatalf("got %v", got)
	}
}

func TestCommandsChunksLongLiteralRuns(t *testing.T) {
	text := strings.Repeat("é", 5000) // 10000 bytes
	var back strings.Builder
	for _, c := range Commands(text) {
		if len(c) != 3 || c[0] != "-l" {
			t.Fatalf("unexpected command %v", c)
		}
		if len(c[2]) > maxLiteralBytes || !utf8.ValidString(c[2]) {
			t.Fatalf("chunk of %d bytes, valid=%v", len(c[2]), utf8.ValidString(c[2]))
		}
		back.WriteString(c[2])
	}
	if back.String() != text {
		t.Fatal("chunks do not add up to the text")
	}
}

func TestRefusesPaneWithoutSocket(t *testing.T) {
	c := &CLI{Path: "/nonexistent/tmux"}
	p := term.Pane{Backend: Backend, Target: "%0"}
	ctx := context.Background()
	if c.Exists(ctx, p) {
		t.Fatal("a pane without a socket must not exist")
	}
	if err := c.Type(ctx, p, "x"); err == nil || !strings.Contains(err.Error(), "socket") {
		t.Fatalf("Type: %v", err)
	}
	if _, err := c.Screen(ctx, p, 10); err == nil || !strings.Contains(err.Error(), "socket") {
		t.Fatalf("Screen: %v", err)
	}
}
