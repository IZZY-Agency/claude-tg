package term

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

type fake struct {
	exists bool
	typed  []string
	screen string
}

func (f *fake) Exists(context.Context, Pane) bool { return f.exists }
func (f *fake) Type(_ context.Context, _ Pane, text string) error {
	f.typed = append(f.typed, text)
	return nil
}
func (f *fake) Screen(context.Context, Pane, int) (string, error) { return f.screen, nil }

func TestPaneString(t *testing.T) {
	cases := map[Pane]string{
		{}:                                     "-",
		{Backend: "agterm", Target: "abc-123"}: "agterm:abc-123",
		{Backend: "tmux", Target: "%5", Socket: "/s"}: "tmux:%5",
	}
	for p, want := range cases {
		if got := p.String(); got != want {
			t.Errorf("%+v: got %q want %q", p, got, want)
		}
	}
	if !(Pane{}).IsZero() || (Pane{Backend: "tmux", Target: "%1"}).IsZero() {
		t.Fatal("IsZero wrong")
	}
}

func TestMuxDispatchesOnBackend(t *testing.T) {
	ag, tm := &fake{exists: true, screen: "agterm screen"}, &fake{exists: true, screen: "tmux screen"}
	m := Mux{Backends: map[string]Terminal{"agterm": ag, "tmux": tm}}
	ctx := context.Background()
	p := Pane{Backend: "tmux", Target: "%5"}
	if !m.Exists(ctx, p) {
		t.Fatal("tmux pane should exist")
	}
	if err := m.Type(ctx, p, "hi"); err != nil || len(tm.typed) != 1 || len(ag.typed) != 0 {
		t.Fatalf("Type went to the wrong backend: %v %v %v", err, tm.typed, ag.typed)
	}
	if s, _ := m.Screen(ctx, Pane{Backend: "agterm", Target: "x"}, 10); s != "agterm screen" {
		t.Fatalf("Screen: got %q", s)
	}
}

func TestMuxRefusesUnknownBackend(t *testing.T) {
	m := Mux{Backends: map[string]Terminal{"agterm": &fake{exists: true}}}
	ctx := context.Background()
	for _, p := range []Pane{{}, {Backend: "screen", Target: "1"}} {
		if m.Exists(ctx, p) {
			t.Errorf("%+v must not exist", p)
		}
		if err := m.Type(ctx, p, "x"); err == nil {
			t.Errorf("%+v: Type must fail", p)
		}
		if _, err := m.Screen(ctx, p, 10); err == nil {
			t.Errorf("%+v: Screen must fail", p)
		}
	}
	var nb *NoBackendError
	if err := m.Type(ctx, Pane{Backend: "screen"}, "x"); !errors.As(err, &nb) || nb.Backend != "screen" {
		t.Fatalf("want NoBackendError for screen, got %v", err)
	}
}

func TestDetect(t *testing.T) {
	look := func(found ...string) func(string) (string, error) {
		return func(name string) (string, error) {
			for _, f := range found {
				if f == name {
					return "/usr/bin/" + name, nil
				}
			}
			return "", errors.New("not found")
		}
	}
	if got := Detect(look()); len(got) != 0 {
		t.Fatalf("none: got %v", got)
	}
	if got := Detect(look("tmux")); len(got) != 1 || got[0] != "tmux" {
		t.Fatalf("tmux only: got %v", got)
	}
	if got := Detect(look("tmux", "agtermctl")); len(got) != 2 || got[0] != "agterm" || got[1] != "tmux" {
		t.Fatalf("both, agterm first: got %v", got)
	}
}

func TestPromptVisible(t *testing.T) {
	perm, _ := os.ReadFile("testdata/permission-screen.txt")
	idle, _ := os.ReadFile("testdata/idle-screen.txt")
	if !PromptVisible(string(perm)) {
		t.Fatal("permission dialog not detected")
	}
	if PromptVisible(string(idle)) {
		t.Fatal("idle prompt misdetected as a permission dialog")
	}
	if DialogSignature(string(perm)) == "" || DialogSignature(string(idle)) != "" {
		t.Fatal("DialogSignature disagrees with PromptVisible")
	}
}

func TestOneLine(t *testing.T) {
	if got := OneLine("  first line\nsecond\tline \r\n\n third  "); got != "first line second line third" {
		t.Fatalf("got %q", got)
	}
}

func TestLookPathInAndDetectIn(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/tmux", []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/agtermctl", []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := "/nonexistent/bin:" + dir
	if p, err := LookPathIn(path, "tmux"); err != nil || p != dir+"/tmux" {
		t.Fatalf("tmux: %q %v", p, err)
	}
	if _, err := LookPathIn(path, "agtermctl"); err == nil {
		t.Fatal("a non-executable file must not count")
	}
	if got := DetectIn(path); len(got) != 1 || got[0] != "tmux" {
		t.Fatalf("DetectIn: %v", got)
	}
}

// Quoted dialog text in an answer must not count.
func TestPromptVisibleIgnoresQuotedText(t *testing.T) {
	quoted := "⏺ When Claude asks \"Do you want to proceed?\" you can press Esc to cancel · Tab to amend.\n  ❯ 1. Yes is the default.\n────────\n❯ \n────────\n  [Fable 5.1] │ x\n"
	if PromptVisible(quoted) {
		t.Fatal("dialog text above the input prompt must not be treated as a dialog")
	}
	perm, _ := os.ReadFile("testdata/permission-screen.txt")
	if sig := DialogSignature(string(perm)); !strings.Contains(sig, "command -v tg && tg follow on") {
		t.Fatalf("signature %q should carry the command", sig)
	}
	if DialogSignature(quoted) != "" {
		t.Fatal("no dialog, no signature")
	}
}
