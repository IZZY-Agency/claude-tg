package term

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// The named-* screens are Claude Code 2.1.327 in fullscreen mode, in a
// session renamed with /rename acme-desk, read whole (87 rows) with
// agtermctl session text on 2026-11-18. A named session draws its name in
// a rule on the pane's last row ("──── acme-desk ─"). While a dialog or a
// picker is up, that rule is the read's last line: right under the dialog
// once the conversation fills the pane (named-picker-multi-*, 90 lines
// printed first), 55 to 57 blank rows below it in a short one
// (named-picker-short, named-bash-short). named-idle is the input box, the
// rule with the name above it, read 60 lines deep.

// tallFake returns the last lines asked of its screen, as agtermctl session
// text --lines does, and records how many each call asked for.
type tallFake struct {
	fake
	asked []int
}

func (f *tallFake) Screen(_ context.Context, _ Pane, lines int) (string, error) {
	f.asked = append(f.asked, lines)
	body, nl := strings.CutSuffix(f.screen, "\n")
	all := strings.Split(body, "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	s := strings.Join(all, "\n")
	if nl {
		s += "\n"
	}
	return s, nil
}

// read is the screen a hook gets through Mux, 60 lines asked as the hooks
// and the relay ask them.
func read(t *testing.T, name string) (string, *tallFake) {
	t.Helper()
	f := &tallFake{fake: fake{exists: true, screen: screen(t, name)}}
	m := Mux{Backends: map[string]Terminal{"agterm": f}}
	s, err := m.Screen(context.Background(), Pane{Backend: "agterm", Target: "x"}, 60)
	if err != nil {
		t.Fatal(err)
	}
	return s, f
}

func TestNamedSessionPicker(t *testing.T) {
	for _, name := range []string{"named-picker-short.txt", "named-picker-multi-q1.txt"} {
		s, _ := read(t, name)
		p, ok := ReadPicker(s)
		if !ok || !p.Whole {
			t.Fatalf("%s: picker not read whole (ok=%v): %q", name, ok, s)
		}
		if PickerSignature(s) == "" {
			t.Errorf("%s: no picker signature", name)
		}
	}
	s, _ := read(t, "named-picker-short.txt")
	if v := PickerView(s); !SignatureHas(v, "☐ Fruit") || !SignatureHas(v, "Which fruit do you want for the picker probe?") {
		t.Errorf("short: view %q", v)
	}
	s, _ = read(t, "named-picker-multi-q1.txt")
	if p, _ := ReadPicker(s); p.Tabs == "" || len(p.TabLabels) != 2 || p.TabLabels[0] != "Colour" || p.TabLabels[1] != "Pet" {
		t.Errorf("multi: tabs %q labels %q", p.Tabs, p.TabLabels)
	}
}

func TestNamedSessionPickerReview(t *testing.T) {
	s, _ := read(t, "named-picker-multi-review.txt")
	p, ok := ReadPicker(s)
	if !ok || !p.Review || p.Tabs == "" {
		t.Fatalf("review screen not read: ok=%v %+v", ok, p)
	}
}

func TestNamedSessionPermissionDialog(t *testing.T) {
	s, _ := read(t, "named-bash-short.txt")
	if !PromptVisible(s) {
		t.Fatal("permission dialog not visible")
	}
	if sig, whole := PermissionDialog(s); sig == "" || !whole || !SignatureHas(sig, "echo tg-probe-named > out.txt") {
		t.Fatalf("permission dialog: sig %q whole %v", sig, whole)
	}
}

// The read reaches past the blank rows of a short session, and what comes
// back is still the bottom lines asked for.
func TestMuxScreenReadsPastTheNameRule(t *testing.T) {
	s, f := read(t, "named-picker-short.txt")
	if len(f.asked) != 1 || f.asked[0] <= 60 {
		t.Fatalf("asked %v lines, want more than 60", f.asked)
	}
	if n := len(strings.Split(strings.TrimRight(s, "\n"), "\n")); n > 60 {
		t.Fatalf("got %d lines, want at most 60", n)
	}
	if strings.Contains(s, "─ acme-desk ─") {
		t.Fatalf("the name rule is still in the read: %q", s)
	}
	if last := strings.TrimRight(s, "\n"); !strings.HasSuffix(last, "Esc to cancel") {
		t.Fatalf("the read does not end at the picker's footer: %q", last[max(0, len(last)-120):])
	}
}

// Only a rule with a name as the read's last line goes, with the blank
// rows above it: the input box's rule (named-idle), a plain rule and a
// screen without one keep their lines, trailing blank rows aside.
func TestMuxScreenKeepsOtherRules(t *testing.T) {
	idle := trimmed(screen(t, "named-idle.txt"))
	if s, _ := read(t, "named-idle.txt"); s != idle {
		t.Fatalf("idle screen changed:\n%q\nwant\n%q", s, idle)
	}
	for _, name := range []string{"picker-single.txt", "permission-screen.txt", "idle-screen.txt", "picker-multi-review.txt", "edit-dialog.txt", "write-dialog.txt", "plan-approval.txt", "trust-dialog.txt", "webfetch-dialog.txt", "mcp-dialog.txt"} {
		if s, _ := read(t, name); s != trimmed(screen(t, name)) {
			t.Errorf("%s changed", name)
		}
	}
	plain := "text\n\n" + strings.Repeat("─", 40) + "\n"
	f := &fake{exists: true, screen: plain}
	m := Mux{Backends: map[string]Terminal{"agterm": f}}
	if s, _ := m.Screen(context.Background(), Pane{Backend: "agterm", Target: "x"}, 60); s != plain {
		t.Errorf("a plain rule went: %q", s)
	}
}

// A short conversation leaves blank rows under the dialog down to the
// pane's last row: they go, named session or not, so a dialog's top stays
// in the read.
func TestMuxScreenDropsTrailingBlankRows(t *testing.T) {
	for _, name := range []string{"named-picker-short.txt", "named-bash-short.txt"} {
		unnamed := regexp.MustCompile(`(?m)^.*─ acme-desk ─.*$`).ReplaceAllString(screen(t, name), "")
		if strings.Contains(unnamed, "─ acme-desk ─") {
			t.Fatalf("%s: the name rule is still in", name)
		}
		f := &tallFake{fake: fake{exists: true, screen: unnamed}}
		m := Mux{Backends: map[string]Terminal{"agterm": f}}
		s, err := m.Screen(context.Background(), Pane{Backend: "agterm", Target: "x"}, 60)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := ReadPicker(s); !ok && !PromptVisible(s) {
			t.Errorf("%s without its name rule: no picker or dialog in %q", name, s)
		}
		if p, ok := ReadPicker(s); ok && !p.Whole {
			t.Errorf("%s without its name rule: picker not whole", name)
		}
		if _, ok := ReadPicker(s); !ok {
			if sig, whole := PermissionDialog(s); sig == "" || !whole {
				t.Errorf("%s without its name rule: dialog not read whole (sig %q)", name, sig)
			}
		}
	}
}

// A pane taller than the first read, a short conversation in it: the
// read is all blank rows (and the name rule), so Screen reads again,
// deeper, and finds the picker.
func TestMuxScreenReadsDeeperOnATallPane(t *testing.T) {
	picker := strings.TrimRight(screen(t, "picker-single.txt"), "\n")
	for name, tail := range map[string]string{"named": "\n" + strings.Repeat("─", 40) + " acme-desk ─\n", "unnamed": "\n"} {
		tall := picker + strings.Repeat("\n", 400) + tail
		f := &tallFake{fake: fake{exists: true, screen: tall}}
		m := Mux{Backends: map[string]Terminal{"agterm": f}}
		s, err := m.Screen(context.Background(), Pane{Backend: "agterm", Target: "x"}, 60)
		if err != nil {
			t.Fatal(err)
		}
		if p, ok := ReadPicker(s); !ok || !p.Whole {
			t.Errorf("%s: picker not read whole after %v", name, f.asked)
		}
		if len(f.asked) != 2 || f.asked[1] <= f.asked[0] {
			t.Errorf("%s: asked %v, want a second, deeper read", name, f.asked)
		}
	}
	// A read that comes back full and keeps all the lines asked for (long
	// scrollback outside fullscreen mode) needs no second one.
	var long []string
	for i := range 400 {
		long = append(long, fmt.Sprintf("line %d", i))
	}
	f := &tallFake{fake: fake{exists: true, screen: strings.Join(long, "\n") + "\n"}}
	m := Mux{Backends: map[string]Terminal{"agterm": f}}
	s, err := m.Screen(context.Background(), Pane{Backend: "agterm", Target: "x"}, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.asked) != 1 || !strings.HasSuffix(s, "line 399\n") || strings.Count(s, "\n") != 60 {
		t.Errorf("long scrollback: asked %v, got %d lines ending %q", f.asked, strings.Count(s, "\n"), s[max(0, len(s)-20):])
	}
}

// trimmed is screen without its trailing blank rows, a final line break kept.
func trimmed(screen string) string {
	body, nl := strings.CutSuffix(screen, "\n")
	lines := strings.Split(body, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	out := strings.Join(lines, "\n")
	if nl {
		out += "\n"
	}
	return out
}
