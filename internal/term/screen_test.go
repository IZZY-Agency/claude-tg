package term

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The picker-*.txt, *-dialog.txt, bash-gutter-*.txt and plan-approval.txt
// screens were read with `agtermctl session text --lines 60` from throwaway
// Claude Code 2.1.310 sessions on 2026-10-24: everything above the last
// prompt removed, local paths replaced with /Users/alex/…. The two
// bash-gutter screens are one dialog, its tty set to 100 then 86 columns.
// picker-multi-q2-untouched, -review-none and -review-all are one picker of
// two questions, read at 22:15 that day: Tab to the second question with
// nothing answered, Tab to the review screen, then back, Red ticked, "2"
// for Dog, which lands on the review screen again. The review screen has no
// "Enter to select" footer.
func screen(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPickerSignature(t *testing.T) {
	single := PickerSignature(screen(t, "picker-single.txt"))
	if !SignatureHas(single, "☐ Fruit") || !SignatureHas(single, "Which fruit do you want for the picker probe?") {
		t.Fatalf("single: signature %q lacks the header or the question", single)
	}
	if strings.Contains(single, "Apple") || strings.Contains(single, "Esc to cancel") || strings.Contains(single, "Use the AskUserQuestion") {
		t.Fatalf("single: signature %q reaches past the question block", single)
	}
	// Moving the cursor or typing into "Type something" at the Mac does not
	// answer the question: the picker is the same.
	for _, name := range []string{"picker-single-cursor.txt", "picker-single-typing.txt"} {
		if got := PickerSignature(screen(t, name)); got != single {
			t.Errorf("%s: signature %q, want the untouched picker's %q", name, got, single)
		}
	}

	// A picker of several questions is one picker: its tab bar names them
	// all, and every question's tab and the review screen show it alike, so
	// moving between them at the Mac answers nothing.
	q1 := PickerSignature(screen(t, "picker-multi-q1.txt"))
	if !SignatureHas(q1, "☐ Colours ☐ Pet ✔ Submit") || SignatureHas(q1, "Which colours") {
		t.Fatalf("multi q1: signature %q, want the tab bar", q1)
	}
	for _, name := range []string{"picker-multi-q1-ticked.txt", "picker-multi-q2.txt", "picker-multi-q2-untouched.txt",
		"picker-multi-review.txt", "picker-multi-review-none.txt", "picker-multi-review-all.txt"} {
		if got := PickerSignature(screen(t, name)); got != q1 {
			t.Errorf("%s: signature %q, want the picker's %q", name, got, q1)
		}
	}
	// PickerView is what the picker shows now: the question on screen.
	if v := PickerView(screen(t, "picker-multi-q1.txt")); !SignatureHas(v, "Which colours do you like for the probe?") || SignatureHas(v, "Which pet") {
		t.Errorf("multi q1: view %q", v)
	}
	if v := PickerView(screen(t, "picker-multi-q2-untouched.txt")); !SignatureHas(v, "Which pet suits the probe?") || SignatureHas(v, "Which colours") {
		t.Errorf("multi q2: view %q", v)
	}
	if v := PickerView(screen(t, "picker-multi-review-none.txt")); !SignatureHas(v, "Review your answers") {
		t.Errorf("review: view %q", v)
	}
	if v := PickerView(screen(t, "picker-single.txt")); v != single {
		t.Errorf("single: view %q, want the signature %q", v, single)
	}

	if p := PickerSignature(screen(t, "picker-preview.txt")); !SignatureHas(p, "Which layout should the probe use?") || strings.Contains(p, "┌") {
		t.Errorf("preview picker: signature %q", p)
	}

	for _, name := range []string{"picker-declined.txt", "permission-screen.txt", "edit-dialog.txt", "idle-screen.txt"} {
		if got := PickerSignature(screen(t, name)); got != "" {
			t.Errorf("%s: no picker on screen, got signature %q", name, got)
		}
	}
}

// The review screen of a picker of several questions has no footer: it is
// the picker's tab bar over "Review your answers" and the options "1.
// Submit answers" and "2. Cancel" at the bottom. It is a picker (a digit
// submits or cancels it), never a permission dialog. Without the tab bar,
// with something under the options, or quoted above the input prompt, it
// is none.
func TestPickerReviewScreen(t *testing.T) {
	for _, name := range []string{"picker-multi-review.txt", "picker-multi-review-none.txt", "picker-multi-review-all.txt"} {
		s := screen(t, name)
		if PickerSignature(s) == "" {
			t.Errorf("%s: the review screen is not recognised", name)
		}
		if sig, _ := PermissionDialog(s); sig != "" || BashDialog(s) != "" || PlanApproval(s) {
			t.Errorf("%s: reads as another dialog", name)
		}
		cursor := strings.Replace(strings.Replace(s, "❯ 1. Submit answers", "  1. Submit answers", 1), "  2. Cancel", "❯ 2. Cancel", 1)
		if PickerSignature(cursor) != PickerSignature(s) {
			t.Errorf("%s: the cursor on Cancel changed the signature", name)
		}
	}
	s := screen(t, "picker-multi-review-none.txt")
	noTabs := strings.Replace(s, "←  ☐ Colours  ☐ Pet  ✔ Submit  →", " ☐ Colours", 1)
	more := s + "\n  3. Something else\n"
	quoted := s + "\n────────\n❯ \n────────\n  ⏸ manual mode on\n"
	for name, x := range map[string]string{"no tab bar": noTabs, "another option under it": more, "quoted": quoted} {
		if x == s {
			t.Fatalf("%s: screen not changed", name)
		}
		if got := PickerSignature(x); got != "" {
			t.Errorf("%s: signature %q, want none", name, got)
		}
	}
}

// Picker text quoted in an answer sits above the input prompt.
func TestPickerSignatureIgnoresQuotedText(t *testing.T) {
	quoted := "⏺ The picker said:\n ☐ Fruit\n\nWhich fruit?\n\n❯ 1. Apple\n\nEnter to select · ↑/↓ to navigate · Esc to cancel\n────────\n❯ \n────────\n  ⏸ manual mode on\n"
	if got := PickerSignature(quoted); got != "" {
		t.Fatalf("quoted picker text counted: %q", got)
	}
}

func TestPermissionSignature(t *testing.T) {
	cases := []struct{ file, question, above string }{
		{"permission-screen.txt", "Do you want to proceed?", "command -v tg && tg follow on"},
		{"edit-dialog.txt", "Do you want to make this edit to notes.txt?", "+there"},
		{"write-dialog.txt", "Do you want to create fresh.txt?", "1 probe"},
		{"write-overwrite-dialog.txt", "Do you want to overwrite notes.txt?", "1 +probe"},
		// No "Esc to cancel" line under a fetch dialog.
		{"webfetch-dialog.txt", "Do you want to allow Claude to fetch this content?", "url: https://example.com/"},
	}
	for _, c := range cases {
		sig := PermissionSignature(screen(t, c.file))
		if !SignatureHas(sig, c.question) || !SignatureHas(sig, c.above) {
			t.Errorf("%s: signature %q lacks %q or %q", c.file, sig, c.question, c.above)
		}
		if SignatureHas(sig, "1. Yes") {
			t.Errorf("%s: signature %q includes the options", c.file, sig)
		}
	}
	// The cursor moving to another option is not an answer.
	edit := screen(t, "edit-dialog.txt")
	moved := strings.Replace(strings.Replace(edit, " ❯ 1. Yes\n", "   1. Yes\n", 1), "   3. No", " ❯ 3. No", 1)
	if moved == edit {
		t.Fatal("test screen not changed")
	}
	if PermissionSignature(moved) != PermissionSignature(edit) {
		t.Error("moving the cursor changed the signature")
	}
	// A narrower terminal redraws the rules shorter.
	var narrow []string
	for _, l := range strings.Split(edit, "\n") {
		if strings.Trim(strings.TrimSpace(l), "─╌") == "" && strings.TrimSpace(l) != "" {
			r := []rune(l)
			l = string(r[:len(r)/2])
		}
		narrow = append(narrow, l)
	}
	if got := PermissionSignature(strings.Join(narrow, "\n")); got != PermissionSignature(edit) {
		t.Errorf("a narrower rule changed the signature: %q", got)
	}
	quoted := "⏺ When Claude asks \"Do you want to proceed?\" you can press Esc to cancel · Tab to amend.\n  ❯ 1. Yes is the default.\n────────\n❯ \n────────\n  [Fable 5.1] │ x\n"
	for name, s := range map[string]string{"quoted": quoted, "idle": screen(t, "idle-screen.txt"), "picker": screen(t, "picker-single.txt"), "declined": screen(t, "picker-declined.txt")} {
		if got := PermissionSignature(s); got != "" {
			t.Errorf("%s: no dialog, got signature %q", name, got)
		}
	}
}

// Ruling R30: a signature starts below the dialog's top border. The lines
// above it belong to the transcript: the pending call's ⏺ blinks, and the
// line above a short dialog is whatever the session printed last.
func TestSignaturesStopAtTheTopBorder(t *testing.T) {
	cases := []struct {
		file, above string
		sig         func(string) string
	}{
		{"permission-screen.txt", "Running 1 shell command", PermissionSignature},
		{"write-dialog.txt", "Write(fresh.txt)", PermissionSignature},
		{"webfetch-dialog.txt", "Fetch(https://example.com)", PermissionSignature},
		{"edit-dialog.txt", "Update(notes.txt)", PermissionSignature},
		{"picker-single.txt", "Do nothing else.", PickerSignature},
	}
	for _, c := range cases {
		s := screen(t, c.file)
		sig := c.sig(s)
		if sig == "" || SignatureHas(sig, c.above) {
			t.Errorf("%s: signature %q reaches above the dialog (%q)", c.file, sig, c.above)
		}
		// The blinking ⏺ of the pending call, and any other change above
		// the border, leaves the signature alone.
		var blink []string
		for _, l := range strings.Split(s, "\n") {
			if strings.HasPrefix(l, "⏺") {
				l = " " + strings.TrimPrefix(l, "⏺")
			}
			if strings.Contains(l, c.above) {
				l = "something else entirely"
			}
			blink = append(blink, l)
		}
		if got := c.sig(strings.Join(blink, "\n")); got != sig {
			t.Errorf("%s: a change above the border changed the signature: %q, want %q", c.file, got, sig)
		}
	}
}

// Ruling R30: a terminal that re-wraps the dialog's text, between words or
// inside a long word, gives the same signature.
func TestSignaturesSurviveRewrap(t *testing.T) {
	cases := []struct {
		file, line string
		wrapped    []string
		sig        func(string) string
	}{
		{"permission-screen.txt", " Tip: auto mode handles these prompts for you — choose \"switch to auto mode\" below",
			[]string{" Tip: auto mode handles these prompts for you —", " choose \"switch to auto mode\" below"}, PermissionSignature},
		{"permission-screen.txt", " command -v tg && tg follow on", []string{" command -v tg && tg fo", " llow on"}, PermissionSignature},
		{"webfetch-dialog.txt", " Do you want to allow Claude to fetch this content?", []string{" Do you want to allow Claude to fetch", " this content?"}, PermissionSignature},
		{"picker-single.txt", "Which fruit do you want for the picker probe?", []string{"Which fruit do you want for the", "picker probe?"}, PickerSignature},
	}
	for _, c := range cases {
		s := screen(t, c.file)
		wrapped := strings.Replace(s, "\n"+c.line+"\n", "\n"+strings.Join(c.wrapped, "\n")+"\n", 1)
		if wrapped == s {
			t.Fatalf("%s: line %q not found", c.file, c.line)
		}
		if got, want := c.sig(wrapped), c.sig(s); got != want || want == "" {
			t.Errorf("%s: re-wrapping %q changed the signature: %q, want %q", c.file, c.line, got, want)
		}
	}
}

// SignatureHas matches a call's key string against a signature, whitespace
// and wrapping aside; an empty key matches nothing.
func TestSignatureHas(t *testing.T) {
	sig := PermissionSignature(screen(t, "permission-screen.txt"))
	for key, want := range map[string]bool{
		"command -v tg && tg follow on":    true,
		"command -v tg &&\n  tg follow on": true,
		"rm -rf node_modules":              false,
		"":                                 false,
		" \n ":                             false,
	} {
		if got := SignatureHas(sig, key); got != want {
			t.Errorf("SignatureHas(sig, %q) = %v, want %v", key, got, want)
		}
	}
}

// Ruling R30a: Claude Code 2.1.310 draws a Bash command wider than 80
// columns, or with a newline, in a box with only a left border: "│ " on
// every row (so does a long WebFetch url row). A resize re-wraps the rows;
// the signature and the call's key ignore the gutter. Real screens: the same
// dialog at 100 and at 86 columns, a heredoc commit, two long urls.
func TestSignaturesIgnoreTheGutter(t *testing.T) {
	wide, narrow := PermissionSignature(screen(t, "bash-gutter-100.txt")), PermissionSignature(screen(t, "bash-gutter-86.txt"))
	if wide == "" || narrow != wide {
		t.Fatalf("the re-wrapped gutter changed the signature: %q at 100 columns, %q at 86", wide, narrow)
	}
	const touch = "touch sigcheck-gutter-alpha.txt sigcheck-gutter-bravo.txt sigcheck-gutter-charlie.txt sigcheck-gutter-delta-e.txt"
	if !SignatureHas(wide, touch) {
		t.Errorf("signature %q lacks the command", wide)
	}
	// The command exactly as in the transcript's tool_use input.
	const commit = "git commit -m \"$(cat <<'EOF'\nsigcheck heredoc probe\n\nSigned-off-by: Alex Doe <alex@example.com>\nEOF\n)\""
	if sig := PermissionSignature(screen(t, "bash-heredoc-dialog.txt")); !SignatureHas(sig, commit) {
		t.Errorf("heredoc: signature %q lacks the command", sig)
	}
	for file, url := range map[string]string{
		"webfetch-long-dialog.txt":    "https://example.com/sigcheck/Some-Long-Path/for-the-probe?alpha=1&beta=two&gamma=three-four-five-six-seven-eight-nine-ten-eleven",
		"webfetch-wrapped-dialog.txt": "https://example.org/sigcheck/segment01-segment02-segment03-segment04-segment05-segment06-segment07-segment08-segment09-segment10-segment11-segment12-segment13-segment14-segment15-segment16-segment17-segment18-segment19-segment20-segment21-segment22-segment23-segment24-segment25?q=end",
	} {
		if sig := PermissionSignature(screen(t, file)); !SignatureHas(sig, url) {
			t.Errorf("%s: signature %q lacks the url", file, sig)
		}
	}
}

// A dialog taller than the read (a long diff) has no top border on screen:
// its signature starts at the question, so a line more or less under the
// dialog (typing, Tab to amend, a redraw) does not shift it.
func TestTallDialogSignatureStartsAtTheQuestion(t *testing.T) {
	var lines []string
	for _, l := range strings.Split(screen(t, "edit-dialog.txt"), "\n") {
		lines = append(lines, l)
		if strings.HasPrefix(l, " 2 +there") {
			for i := 3; i < 80; i++ {
				lines = append(lines, fmt.Sprintf(" %d +line %d", i, i))
			}
		}
	}
	read := func(lines []string) string { return strings.Join(lines[len(lines)-60:], "\n") }
	sig := PermissionSignature(read(lines))
	amended := PermissionSignature(read(append(lines, "  and also this")))
	if sig == "" || amended != sig || !SignatureHas(sig, "Do you want to make this edit to notes.txt?") {
		t.Fatalf("tall dialog: signature %q, one line more under it %q", sig, amended)
	}
	// The same for a picker whose top border is above the read.
	picker := strings.Split(screen(t, "picker-single.txt"), "\n")
	for i, l := range picker {
		if strings.HasPrefix(l, "Which fruit") {
			picker = picker[i-1:] // from the blank line above the question
			break
		}
	}
	p, shifted := PickerSignature(strings.Join(picker, "\n")), PickerSignature(strings.Join(picker[1:], "\n"))
	if p == "" || shifted != p || !SignatureHas(p, "Which fruit do you want for the picker probe?") {
		t.Fatalf("picker without its border: signature %q, shifted %q", p, shifted)
	}
}

// Plan approval ("Ready to code?" … "Would you like to proceed?") is a
// dialog too: a digit picks one of its options. Real screen, plan mode.
func TestPlanApproval(t *testing.T) {
	if !PlanApproval(screen(t, "plan-approval.txt")) {
		t.Fatal("plan approval dialog not recognised")
	}
	quoted := "⏺ Claude asked \"Would you like to proceed?\" and listed:\n  ❯ 1. Yes, and use auto mode\n────────\n❯ \n────────\n  ⏸ plan mode on\n"
	for name, s := range map[string]string{"quoted": quoted, "idle": screen(t, "idle-screen.txt"), "permission": screen(t, "permission-screen.txt"), "picker": screen(t, "picker-single.txt")} {
		if PlanApproval(s) {
			t.Errorf("%s: no plan approval on screen", name)
		}
	}
}

// planWithQuestion is the real plan approval screen with a plan line that
// reads like a permission dialog's question.
func planWithQuestion(t *testing.T) string {
	t.Helper()
	s := screen(t, "plan-approval.txt")
	out := strings.Replace(s, "\n Steps\n", "\n Do you want to keep the old flag? Open question.\n\n Steps\n", 1)
	if out == s {
		t.Fatal("plan screen not changed")
	}
	return out
}

// A plan line starting "Do you want to …" over the plan's numbered options
// must not read as a permission dialog: /approve would pick "Yes, and use
// auto mode". The plan approval at the bottom wins; a permission dialog at
// the bottom whose command quotes the plan's question stays one.
func TestPlanApprovalIsNeverAPermissionDialog(t *testing.T) {
	plan := planWithQuestion(t)
	if !PlanApproval(plan) {
		t.Fatal("plan approval with a question line not recognised")
	}
	if sig, _ := PermissionDialog(plan); sig != "" || PermissionSignature(plan) != "" {
		t.Fatalf("plan approval read as a permission dialog: %q", sig)
	}
	bash := strings.ReplaceAll(screen(t, "permission-screen.txt"), "command -v tg && tg follow on", `echo "Would you like to proceed?"`)
	if PlanApproval(bash) || PermissionSignature(bash) == "" {
		t.Fatalf("a Bash dialog quoting the plan's question: plan=%v sig=%q", PlanApproval(bash), PermissionSignature(bash))
	}
}

// PermissionDialog says whether the dialog's top border is in the read: a
// dialog taller than the read is cut and its signature holds only the
// question.
func TestPermissionDialogWhole(t *testing.T) {
	if sig, whole := PermissionDialog(screen(t, "edit-subdir-dialog.txt")); sig == "" || !whole {
		t.Fatalf("whole dialog: %q %v", sig, whole)
	}
	tall := tallRead(t, "edit-subdir-dialog.txt", " 2 +there")
	sig, whole := PermissionDialog(tall)
	if sig == "" || whole || !SignatureHas(sig, "Do you want to make this edit to notes.txt?") {
		t.Fatalf("tall dialog: %q whole=%v", sig, whole)
	}
}

// tallRead is the screen with 77 diff lines added after the line starting
// with after, read as its last 60 lines: the top border is above the read.
func tallRead(t *testing.T, name, after string) string {
	t.Helper()
	var lines []string
	for _, l := range strings.Split(screen(t, name), "\n") {
		lines = append(lines, l)
		if strings.HasPrefix(l, after) {
			for i := 3; i < 80; i++ {
				lines = append(lines, fmt.Sprintf(" %d +line %d", i, i))
			}
		}
	}
	if len(lines) < 80 {
		t.Fatalf("%s: no line %q", name, after)
	}
	return strings.Join(lines[len(lines)-60:], "\n")
}

// A picker whose top border is above the read, and whose question wraps:
// the signature holds the whole question, so its key is still found.
func TestPickerWithoutBorderKeepsTheWholeQuestion(t *testing.T) {
	s := strings.Replace(screen(t, "picker-single.txt"), "\nWhich fruit do you want for the picker probe?\n", "\nWhich fruit do you want\nfor the picker probe?\n", 1)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "Which fruit") {
			lines = lines[i:] // the read starts at the question's first line
			break
		}
	}
	sig := PickerSignature(strings.Join(lines, "\n"))
	if !SignatureHas(sig, "Which fruit do you want for the picker probe?") {
		t.Fatalf("signature %q lacks the wrapped question", sig)
	}
}

// rewrap is the screen redrawn at width columns, as a narrower terminal
// draws it: a rule is cut to the width, a longer text line breaks between
// words and continues with one space of indent, as Claude Code 2.1.310
// wraps the plan approval's question (" Claude has written up a plan and
// is ready to execute. Would you like to" / " proceed?").
func rewrap(s string, width int) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimRight(l, " ")
		r := []rune(l)
		switch {
		case len(r) <= width:
			out = append(out, l)
		case strings.Trim(strings.TrimSpace(l), "─╌") == "":
			out = append(out, string(r[:width]))
		default:
			line := ""
			for _, w := range strings.Fields(l) {
				if line != "" && len([]rune(line))+1+len([]rune(w)) > width {
					out = append(out, line)
					line = " " + w
					continue
				}
				if line == "" {
					line = " " + w
				} else {
					line += " " + w
				}
			}
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// Ruling R38: the plan approval read at 80 columns, where its question
// wraps, is still the plan approval, never a permission dialog, and a plan
// marker is on the read; with a plan line that reads like a dialog's
// question too.
func TestPlanApprovalSurvivesAWrap(t *testing.T) {
	for name, s := range map[string]string{"plan": screen(t, "plan-approval.txt"), "plan with a question line": planWithQuestion(t)} {
		w := rewrap(s, 80)
		if !strings.Contains(w, "Would you like to\n proceed?\n") {
			t.Fatalf("%s: the question did not wrap at 80 columns:\n%s", name, w)
		}
		if !PlanApproval(w) {
			t.Errorf("%s: the re-wrapped plan approval is not recognised", name)
		}
		if sig := PermissionSignature(w); sig != "" {
			t.Errorf("%s: the re-wrapped plan approval reads as a permission dialog: %q", name, sig)
		}
		if !PlanMarker(w) {
			t.Errorf("%s: no plan marker found on the re-wrapped read", name)
		}
		if BashDialog(w) != "" {
			t.Errorf("%s: the re-wrapped plan approval reads as the Bash dialog", name)
		}
	}
}

// pickerAskingToProceed is the real single-choice picker asking "Do you
// want to proceed?", with those words in the transcript above it as well.
func pickerAskingToProceed(t *testing.T) string {
	t.Helper()
	s := screen(t, "picker-single.txt")
	out := strings.Replace(s, "\nWhich fruit do you want for the picker probe?\n", "\nDo you want to proceed?\n", 1)
	out = strings.Replace(out, "\n──", "\n⏺ I will ask with a picker: Do you want to proceed? Apple, Banana or Cherry.\n──", 1)
	if strings.Count(out, "Do you want to proceed?") != 2 {
		t.Fatal("picker screen not changed")
	}
	return out
}

// Ruling R38: a picker whose question is "Do you want to proceed?" is a
// picker, not a permission dialog: a "1" would pick its first option. With
// its cursor line at column 0 (as recorded) and indented like a dialog's
// option, so that no line reads as the input prompt.
func TestPickerIsNeverAPermissionDialog(t *testing.T) {
	s := pickerAskingToProceed(t)
	indented := strings.Replace(s, "\n❯ 1. Apple\n", "\n ❯ 1. Apple\n", 1)
	if indented == s {
		t.Fatal("picker screen not changed")
	}
	for name, p := range map[string]string{"cursor at column 0": s, "cursor indented": indented} {
		if PickerSignature(p) == "" {
			t.Errorf("%s: the picker is not recognised", name)
		}
		if sig, whole := PermissionDialog(p); sig != "" || whole {
			t.Errorf("%s: the picker reads as a permission dialog: %q", name, sig)
		}
		if BashDialog(p) != "" {
			t.Errorf("%s: the picker reads as the Bash dialog", name)
		}
	}
}

// Ruling R38: the one dialog the relay may type into is the strict v0.2
// Bash dialog, "Do you want to proceed?" over a numbered option with the
// "Esc to cancel · Tab to amend" footer, with no picker on screen and no
// plan marker anywhere in the read. Its signature is PermissionSignature's.
func TestBashDialog(t *testing.T) {
	for _, name := range []string{"permission-screen.txt", "bash-gutter-100.txt", "bash-gutter-86.txt", "bash-heredoc-dialog.txt", "read-outside-dialog.txt"} {
		s := screen(t, name)
		if sig := BashDialog(s); sig == "" || sig != PermissionSignature(s) {
			t.Errorf("%s: BashDialog = %q, want PermissionSignature %q", name, sig, PermissionSignature(s))
		}
	}
	bash := screen(t, "permission-screen.txt")
	noFooter := strings.Replace(bash, "\n Esc to cancel · Tab to amend", "\n Esc to cancel", 1)
	planAbove := strings.Replace(bash, "\n⏺ Skill(telegram-notify)\n", "\n   2. Yes, manually approve edits\n", 1)
	planQuoted := strings.ReplaceAll(bash, "command -v tg && tg follow on", `echo "Would you like to proceed?"`)
	// An Edit of this very fixture: its diff shows the Bash question and
	// footer, but the dialog's own question is the Edit's.
	edit := strings.Replace(screen(t, "edit-dialog.txt"), "\n 2 +there", "\n 2 +Do you want to proceed?\n 3 + Esc to cancel · Tab to amend", 1)
	for name, s := range map[string]string{
		"edit": screen(t, "edit-dialog.txt"), "write": screen(t, "write-dialog.txt"), "webfetch": screen(t, "webfetch-dialog.txt"),
		"plan": screen(t, "plan-approval.txt"), "picker": screen(t, "picker-single.txt"), "idle": screen(t, "idle-screen.txt"),
		"picker asking to proceed": pickerAskingToProceed(t), "no footer": noFooter, "plan option in the scrollback": planAbove,
		"plan question in the command": planQuoted, "edit showing the Bash lines": edit,
	} {
		if noFooter == bash || planAbove == bash || planQuoted == bash || edit == screen(t, "edit-dialog.txt") {
			t.Fatal("test screen not changed")
		}
		if sig := BashDialog(s); sig != "" {
			t.Errorf("%s: BashDialog = %q, want none", name, sig)
		}
	}
}

// PlanMarker finds the plan approval's words anywhere in the read, wrapped
// or not: the question, or one of its option labels.
func TestPlanMarker(t *testing.T) {
	bash := screen(t, "permission-screen.txt")
	for name, c := range map[string]struct {
		s    string
		want bool
	}{
		"plan":                  {screen(t, "plan-approval.txt"), true},
		"plan at 80 columns":    {rewrap(screen(t, "plan-approval.txt"), 80), true},
		"only the plan options": {"   2. Yes, manually approve edits\n   3. Tell Claude what to change\n", true},
		"keep planning":         {"   3. No, keep planning\n", true},
		"quoted in a command":   {strings.ReplaceAll(bash, "command -v tg && tg follow on", `echo "Would you like to proceed?"`), true},
		"bash":                  {bash, false},
		"edit":                  {screen(t, "edit-dialog.txt"), false},
		"picker":                {screen(t, "picker-single.txt"), false},
		"idle":                  {screen(t, "idle-screen.txt"), false},
	} {
		if got := PlanMarker(c.s); got != c.want {
			t.Errorf("%s: PlanMarker = %v, want %v", name, got, c.want)
		}
	}
}

// lastNonBlank is the last line of s that is not blank.
func lastNonBlank(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}

// Round 5: a pane narrower than the picker's footer (49 columns for the
// single picker, 60 multi, 66 preview, 74 typing) wraps it over two lines.
// The picker is still recognised, with the signature it has at full width,
// and is neither a permission dialog nor the Bash dialog.
func TestPickerSignatureSurvivesAWrap(t *testing.T) {
	for name, widths := range map[string][]int{
		"picker-single.txt": {40, 46}, "picker-multi-q1.txt": {50, 56}, "picker-preview.txt": {46, 50},
		"picker-single-typing.txt": {60, 70}, "picker-multi-q1-ticked.txt": {50, 56},
	} {
		s := screen(t, name)
		want := PickerSignature(s)
		if want == "" {
			t.Fatalf("%s: no signature at full width", name)
		}
		for _, w := range widths {
			narrow := rewrap(s, w)
			if strings.Contains(lastNonBlank(narrow), "Enter to select") {
				t.Fatalf("%s at %d columns: the footer did not wrap:\n%s", name, w, narrow)
			}
			if got := PickerSignature(narrow); got != want {
				t.Errorf("%s at %d columns: signature %q, want %q", name, w, got, want)
			}
			if sig, _ := PermissionDialog(narrow); sig != "" || BashDialog(narrow) != "" {
				t.Errorf("%s at %d columns: reads as a dialog: %q / %q", name, w, sig, BashDialog(narrow))
			}
		}
	}
}

// bashQuotingPicker is the single-choice picker whose option descriptions
// quote the Bash dialog's question and footer.
func bashQuotingPicker(t *testing.T) string {
	t.Helper()
	s := strings.Replace(screen(t, "picker-single.txt"), "     A crisp red apple", "     Do you want to proceed?", 1)
	s = strings.Replace(s, "     A ripe yellow banana", "     Esc to cancel · Tab to amend", 1)
	if strings.Count(s, "Do you want to proceed?") != 1 || strings.Count(s, "Esc to cancel · Tab to amend") != 1 {
		t.Fatal("picker screen not changed")
	}
	return s
}

// editWithADirectoryNameInjection is the Edit dialog of a file outside the
// cwd whose directory name holds a newline, the Bash question and the Bash
// footer; the "always allow access to <dir>" option shows them under the
// Edit's own question.
func editWithADirectoryNameInjection(t *testing.T) string {
	t.Helper()
	s := strings.Replace(screen(t, "edit-outside-dialog.txt"), "      /Users/alex/scratch/fix2 for this session (shift+tab)",
		"      /Users/alex/scratch/fix2\nDo you want to proceed?\n Esc to cancel · Tab to amend\n      for this session (shift+tab)", 1)
	if !strings.Contains(s, "\nDo you want to proceed?\n") {
		t.Fatal("edit screen not changed")
	}
	return s
}

// Round 5: BashDialog is anchored. The line after the question must be
// option 1 and the read's last non-blank line must be the footer, so a
// picker narrower than its footer whose descriptions quote the Bash
// question and footer, an Edit dialog whose directory name holds them,
// and a dialog with a line drawn under its footer are never the Bash
// dialog; the Bash and Read dialogs are, at every width they fit in.
func TestBashDialogIsAnchored(t *testing.T) {
	picker := bashQuotingPicker(t)
	multi := strings.Replace(screen(t, "picker-multi-q1.txt"), "         Warm", "         Do you want to proceed?", 1)
	multi = strings.Replace(multi, "         Natural", "         Esc to cancel · Tab to amend", 1)
	bash := screen(t, "permission-screen.txt")
	for name, s := range map[string]string{
		"the quoting picker at 46 columns": rewrap(picker, 46), "the quoting picker at 40 columns": rewrap(picker, 40),
		"the quoting multi picker at 56 columns":         rewrap(multi, 56),
		"an Edit with the strings in its directory name": editWithADirectoryNameInjection(t),
		"a line under the footer":                        bash + "\n  ⏸ manual mode on\n",
	} {
		if strings.Count(s, "Do you want to proceed?") == 0 {
			t.Fatalf("%s: screen not as intended", name)
		}
		if sig := BashDialog(s); sig != "" {
			t.Errorf("%s: BashDialog = %q, want none", name, sig)
		}
	}
	for _, name := range []string{"permission-screen.txt", "bash-gutter-100.txt", "bash-gutter-86.txt", "bash-heredoc-dialog.txt", "read-outside-dialog.txt", "read-home-dialog.txt"} {
		s := screen(t, name)
		for w := 30; w <= 200; w++ {
			narrow := rewrap(s, w)
			if sig := BashDialog(narrow); sig == "" || sig != PermissionSignature(narrow) {
				t.Errorf("%s at %d columns: BashDialog = %q, PermissionSignature = %q", name, w, sig, PermissionSignature(narrow))
				break
			}
		}
	}
}

// Reads of a picker of several questions that do not hold it whole, built
// from the recorded screens: the tab bar clipped at the pane's edge, the
// tab bar scrolled to the active tab (a header left out), the picker
// taller than the read (its top border and tab bar above it), and the
// review screen cut to its last lines.
func clippedTabs(t *testing.T) string {
	t.Helper()
	return replaced(t, screen(t, "picker-multi-q1.txt"), "←  ☐ Colours  ☐ Pet  ✔ Submit  →", "←  ☐ Colours  ☐ Pet  ✔ Sub")
}

func scrolledTabs(t *testing.T) string {
	t.Helper()
	return replaced(t, screen(t, "picker-multi-q2-untouched.txt"), "←  ☐ Colours  ☐ Pet  ✔ Submit  →", "←  ☐ Pet  ✔ Submit  →")
}

func tallPicker(t *testing.T) string {
	t.Helper()
	return lastLines(screen(t, "picker-multi-q2-untouched.txt"), 11) // from "Which pet suits the probe?"
}

func cutReview(t *testing.T) string {
	t.Helper()
	return lastLines(screen(t, "picker-multi-review-none.txt"), 5) // "Ready to submit your answers?" and its options
}

func replaced(t *testing.T, s, old, new string) string {
	t.Helper()
	if !strings.Contains(s, old) {
		t.Fatalf("%q not on the screen", old)
	}
	return strings.Replace(s, old, new, 1)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return strings.Join(lines[len(lines)-n:], "\n") + "\n"
}

// ReadPicker says how much of the picker a read holds: its tab bar (a
// picker of several questions, read "← … ✔ Submit →"), whether its top
// border is in the read, and its review screen.
func TestReadPicker(t *testing.T) {
	const tabs = "←☐Colours☐Pet✔Submit→"
	for _, tc := range []struct {
		name, screen, tabs string
		whole, review, ok  bool
	}{
		{"q1", screen(t, "picker-multi-q1.txt"), tabs, true, false, true},
		{"q2", screen(t, "picker-multi-q2-untouched.txt"), tabs, true, false, true},
		{"review", screen(t, "picker-multi-review-all.txt"), tabs, true, true, true},
		{"single", screen(t, "picker-single.txt"), "", true, false, true},
		{"clipped tab bar", clippedTabs(t), "", true, false, true},
		{"scrolled tab bar", scrolledTabs(t), "←☐Pet✔Submit→", true, false, true},
		{"taller than the read", tallPicker(t), "", false, false, true},
		// Cropped above the question, with no top border in the read: from
		// the tab bar, from the blank line under it, from a blank line over
		// it. The tab bar alone is no proof the picker is read whole.
		{"cropped at the tab bar", lastLines(screen(t, "picker-multi-q2-untouched.txt"), 13), "", false, false, true},
		{"cropped under the tab bar", lastLines(screen(t, "picker-multi-q2-untouched.txt"), 12), "", false, false, true},
		{"cropped over the tab bar", "\n" + lastLines(screen(t, "picker-multi-q2-untouched.txt"), 13), "", false, false, true},
		{"review cut", cutReview(t), "", false, false, false},
		{"idle", screen(t, "idle-screen.txt"), "", false, false, false},
	} {
		p, ok := ReadPicker(tc.screen)
		if ok != tc.ok || p.Tabs != tc.tabs || p.Whole != tc.whole || p.Review != tc.review {
			t.Errorf("%s: %+v, %v; want tabs %q whole %v review %v ok %v", tc.name, p, ok, tc.tabs, tc.whole, tc.review, tc.ok)
		}
	}
	if p, _ := ReadPicker(tallPicker(t)); p.View != "Whichpetsuitstheprobe?" {
		t.Errorf("taller than the read: view %q, want its question", p.View)
	}
	// The tab bar's labels, exactly and in order, repeats included; a bar a
	// narrow pane wraps reads the same.
	bar := "←  ☐ Colours  ☐ Pet  ✔ Submit  →"
	for name, c := range map[string]struct {
		screen string
		labels []string
	}{
		"recorded":       {screen(t, "picker-multi-q1-ticked.txt"), []string{"Colours", "Pet"}},
		"wrapped":        {replaced(t, screen(t, "picker-multi-q1.txt"), bar, "←  ☐ Colours  ☐ Pet\n ✔ Submit  →"), []string{"Colours", "Pet"}},
		"spaces, repeat": {replaced(t, screen(t, "picker-multi-q1.txt"), bar, "←  ☐ Due date  ☒ Choice  ☐ Choice  ✔ Submit  →"), []string{"Due date", "Choice", "Choice"}},
		"single":         {screen(t, "picker-single.txt"), nil},
		"clipped":        {clippedTabs(t), nil},
	} {
		p, _ := ReadPicker(c.screen)
		if !reflect.DeepEqual(p.TabLabels, c.labels) {
			t.Errorf("%s: labels %q, want %q", name, p.TabLabels, c.labels)
		}
	}
}

// PickerMarkers finds the picker's own marks at the bottom of a read that
// does not hold the picker whole, so nothing is typed into it and no hook
// takes it for gone. The idle input box (with text that names the marks,
// wrapped or not), a declined picker in the history, and the permission
// dialogs show none.
func TestPickerMarkers(t *testing.T) {
	for name, s := range map[string]string{
		"q1": screen(t, "picker-multi-q1.txt"), "review": screen(t, "picker-multi-review-none.txt"), "single": screen(t, "picker-single.txt"),
		"clipped tab bar": clippedTabs(t), "scrolled tab bar": scrolledTabs(t), "taller than the read": tallPicker(t), "review cut": cutReview(t),
		"footer cut": replaced(t, screen(t, "picker-single.txt"), "Enter to select · ↑/↓ to navigate · Esc to cancel", "Enter to sel"),
	} {
		if !PickerMarkers(s) {
			t.Errorf("%s: no picker marks found", name)
		}
	}
	idle := screen(t, "idle-screen.txt")
	typed := replaced(t, idle, "\n❯ \n", "\n❯ Type something like ☐ Submit answers\n")
	wrapped := replaced(t, idle, "\n❯ \n", "\n❯ please Type something, then\n  Enter to select · ✔ Submit\n")
	for name, s := range map[string]string{
		"idle": idle, "typed": typed, "wrapped": wrapped, "declined": screen(t, "picker-declined.txt"),
		"permission": screen(t, "permission-screen.txt"), "edit": screen(t, "edit-dialog.txt"), "plan": screen(t, "plan-approval.txt"),
	} {
		if PickerMarkers(s) {
			t.Errorf("%s: picker marks found", name)
		}
	}
}
