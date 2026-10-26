package term

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var (
	optionRe      = regexp.MustCompile(`^\s*❯?\s*[1-9]\. `)
	firstOptionRe = regexp.MustCompile(`^\s*❯?\s*1\. `)
	inputRe       = regexp.MustCompile(`^❯( .*)?$`) // the input prompt sits at column 0
	// The options of a picker's review screen, with the cursor on either.
	reviewSubmitRe = regexp.MustCompile(`^\s*❯?\s*1\. Submit answers\s*$`)
	reviewCancelRe = regexp.MustCompile(`^\s*❯?\s*2\. Cancel\s*$`)
)

// dialogSpan returns the line range of the permission dialog at the bottom
// of the screen, or (-1, -1). The dialog must have its question, a numbered
// option and the Esc hint, and nothing after it may be the input prompt:
// quoted dialog text in an answer always sits above the prompt line.
func dialogSpan(lines []string) (int, int) {
	q := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], "Do you want to proceed?") {
			q = i
			break
		}
	}
	if q < 0 {
		return -1, -1
	}
	option, esc := -1, -1
	for i := q; i < len(lines); i++ {
		if option < 0 && optionRe.MatchString(lines[i]) {
			option = i
		}
		if esc < 0 && strings.Contains(lines[i], "Esc to cancel") {
			esc = i
		}
	}
	if option < 0 || esc < 0 {
		return -1, -1
	}
	end := esc
	if option > end {
		end = option
	}
	for i := end + 1; i < len(lines); i++ {
		if inputRe.MatchString(strings.TrimRight(lines[i], " \t")) {
			return -1, -1
		}
	}
	return q, end
}

// PromptVisible reports whether the screen ends in a Claude Code permission dialog.
func PromptVisible(screen string) bool {
	q, _ := dialogSpan(strings.Split(screen, "\n"))
	return q >= 0
}

// DialogSignature identifies the dialog on screen (its question and the
// lines above it that name the command) so two reads can be compared.
func DialogSignature(screen string) string {
	lines := strings.Split(screen, "\n")
	q, end := dialogSpan(lines)
	if q < 0 {
		return ""
	}
	start := q - 8
	if start < 0 {
		start = 0
	}
	return strings.Join(lines[start:end+1], "\n")
}

// PermissionSignature identifies any permission dialog at the bottom of the
// screen: a "Do you want to …" question with a numbered option under it and
// no input prompt after it. Bash and MCP tools ask "Do you want to
// proceed?", Edit "Do you want to make this edit to F?", Write "Do you want
// to create F?", WebFetch "Do you want to allow Claude to fetch this
// content?" with no Esc line under its options. The signature is the dialog
// from below its top border down to its first option (ruling R30): nothing
// of the transcript above it (the pending call's ⏺ blinks), and not the
// options, so moving the cursor or typing into an option does not change
// it. It is compact (see compact), so a terminal that re-wraps or resizes
// the dialog does not change it either. "" when no dialog is on screen, and
// when the plan approval is: a plan line may read "Do you want to …".
func PermissionSignature(screen string) string {
	sig, _ := PermissionDialog(screen)
	return sig
}

// PermissionDialog is PermissionSignature, and whether the dialog's top
// border is in the read (whole). A dialog taller than the read is not
// whole: its signature starts at the question, so a line more or less
// under the dialog does not shift it, and holds only the question.
// Neither the plan approval nor a question picker is a permission dialog,
// whatever the plan says or the question asks (rulings R32, R38).
//
// Parallel calls wait on one dialog: it shows the first call not yet
// answered, with a queue counter on its title line (see queue). The
// signature leaves the counter out: a call joining the queue, or one
// behind the dialog answered, moves it while the dialog stays.
func PermissionDialog(screen string) (sig string, whole bool) {
	return permissionSig(screen, true)
}

// permissionSig is PermissionDialog, with the queue counter left out of the
// signature or kept in it.
func permissionSig(screen string, dropQueue bool) (sig string, whole bool) {
	lines, from, opt, whole := permissionDialog(screen)
	if lines == nil {
		return "", false
	}
	head := slices.Clone(lines[from:opt])
	if whole && dropQueue {
		head[0], _, _ = queue(head[0])
	}
	return compact(head), whole
}

// permissionDialog finds the permission dialog at the bottom of the screen
// (PermissionDialog): the screen's lines, the dialog's first line (its
// title, under its top border, when whole; else its question) and its
// first option. nil lines when there is none.
func permissionDialog(screen string) (lines []string, from, opt int, whole bool) {
	if PlanApproval(screen) || PickerSignature(screen) != "" {
		return nil, 0, 0, false
	}
	lines = strings.Split(screen, "\n")
	q := lastQuestion(lines)
	if q < 0 || !below(lines[q+1:], optionRe) {
		return nil, 0, 0, false
	}
	opt = q + 1 // the question may wrap onto the lines before the first option
	for !optionRe.MatchString(lines[opt]) {
		opt++
	}
	from = topBorder(lines, q) + 1
	if from == 0 {
		return lines, q, opt, false
	}
	return lines, from, opt, true
}

// queueRe is the counter Claude Code 2.1.310 right-aligns on a permission
// dialog's title line while parallel calls wait on one dialog ("Tool use
// … 1 of 2", mcp-queue-*.txt): the calls answered, plus one, of all of
// them. It counts progress, not the call shown: a call behind the dialog
// answered by its hook moves it while the dialog stays.
var queueRe = regexp.MustCompile(`^(.*\S)\s+([1-9][0-9]{0,3}) of ([1-9][0-9]{0,3})\s*$`)

// queue reads the counter off a dialog's title line: the line without it,
// and how many calls are still to answer behind the one shown (0 for the
// last of them, or a line without a counter).
func queue(title string) (rest string, behind int, ok bool) {
	g := queueRe.FindStringSubmatch(title)
	if g == nil {
		return title, 0, false
	}
	k, _ := strconv.Atoi(g[2])
	m, _ := strconv.Atoi(g[3])
	if k > m || m < 2 {
		return title, 0, false
	}
	return g[1], m - k, true
}

// DialogQueued reports whether the permission dialog at the bottom of the
// screen, read whole, has calls queued behind it, still to answer: its
// queue counter's first number is below its second. A queued call's own
// dialog comes up only once the ones before it are answered.
func DialogQueued(screen string) bool {
	lines, from, _, whole := permissionDialog(screen)
	if lines == nil || !whole {
		return false
	}
	_, behind, _ := queue(lines[from])
	return behind > 0
}

// lastQuestion is the index of the last "Do you want to …" line; -1 when
// there is none.
func lastQuestion(lines []string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "Do you want to ") {
			return i
		}
	}
	return -1
}

// planQuestion is the plan approval's question, compact: "Claude has
// written up a plan and is ready to execute. Would you like to proceed?",
// which a terminal of 80 columns wraps before "proceed?".
const planQuestion = "Wouldyouliketoproceed?"

// planMarkers are the plan approval's words, compact: its question and
// the labels of its options as Claude Code 2.1.310 draws them ("Yes, and
// use auto mode", "Yes, manually approve edits", "Tell Claude what to
// change") and as its binary also spells them ("Yes, clear context … and
// use auto mode / and auto-accept edits / and bypass permissions", "No,
// keep planning", "No, refine with Ultraplan in a cloud session"). The
// Bash dialog's "Yes, and switch to auto mode" and the Edit dialog's "Yes,
// and switch to accept edits (auto-approve …)" match none of them.
var planMarkers = []string{planQuestion, "anduseautomode", "andauto-acceptedits", "andbypasspermissions", "manuallyapproveedits", "TellClaudewhattochange", "keepplanning", "refinewithUltraplan"}

// PlanApproval reports whether the screen ends in the plan approval dialog
// ("Ready to code?" … "Would you like to proceed?" over numbered options,
// no input prompt after them). The question is looked for, compact, in the
// lines between the last solid rule and the first option of the dialog at
// the bottom, so a terminal that wraps it (" Would you like to" / "
// proceed?") still shows it. A digit picks one of its options, so nothing
// may be typed into it, and it is never a permission dialog, whatever the
// plan says. A permission dialog whose command quotes the plan's question
// stays one: its own "Do you want to …" question comes after the quote.
func PlanApproval(screen string) bool {
	lines := strings.Split(screen, "\n")
	first := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if firstOptionRe.MatchString(lines[i]) {
			first = i
			break
		}
	}
	if first < 0 || !below(lines[first+1:], nil) {
		return false
	}
	head := compact(lines[topBorder(lines, first)+1 : first])
	i := strings.LastIndex(head, planQuestion)
	return i >= 0 && !strings.Contains(head[i:], "Doyouwantto")
}

// PlanMarker reports whether the plan approval's words (planMarkers) appear
// anywhere in the read, wrapped or not: the plan approval at the bottom, or
// one scrolled up above another dialog. The relay then types nothing into
// the pane, whatever is at the bottom (ruling R38).
func PlanMarker(screen string) bool {
	all := compact(strings.Split(screen, "\n"))
	for _, m := range planMarkers {
		if strings.Contains(all, m) {
			return true
		}
	}
	return false
}

// bashFooter is the last line of the v0.2 Bash dialog.
const bashFooter = "Esc to cancel · Tab to amend"

// BashDialog identifies the one dialog the relay's /approve and /deny may
// type into (ruling R38), the strict v0.2 Bash dialog, anchored at both
// ends: the last "Do you want to …" line is exactly "Do you want to
// proceed?", the line right after it is option 1, the read's last non-blank
// line is exactly the "Esc to cancel · Tab to amend" footer, no input
// prompt lies between, no question picker is on screen and no plan marker
// is anywhere in the read. The anchors keep out text that merely quotes
// the question and the footer: a picker's option descriptions (in a pane
// too narrow for PickerSignature's footer), or a directory name with a
// newline in an Edit dialog's "always allow access to" option. Read and
// MCP tool dialogs share the layout. The signature is
// PermissionSignature's with the queue counter kept (ruling P26): the
// relay's two reads must see the same call at the same place in a queue,
// so an answer at the Mac between them that brings up an identical call
// stops the keystroke. A dialog whose top border is above the read gets
// none: its signature is only its question, alike for any two such
// dialogs. "" for any other screen.
func BashDialog(screen string) string {
	lines := strings.Split(screen, "\n")
	q := lastQuestion(lines)
	if q < 0 || q+1 >= len(lines) || strings.TrimSpace(lines[q]) != "Do you want to proceed?" || !firstOptionRe.MatchString(lines[q+1]) || !below(lines[q+1:], nil) {
		return ""
	}
	last := len(lines) - 1
	for last > q && strings.TrimSpace(lines[last]) == "" {
		last--
	}
	if strings.TrimSpace(lines[last]) != bashFooter || PickerSignature(screen) != "" || PlanMarker(screen) {
		return ""
	}
	sig, whole := permissionSig(screen, false)
	if !whole {
		return ""
	}
	return sig
}

// PickerSignature identifies the AskUserQuestion picker at the bottom of the
// screen, one signature for all of its questions. A picker is numbered
// options over an "Enter to select · … · Esc to cancel" footer (one line,
// or several in a pane narrower than it), or the review screen of a picker
// of several questions (see picker); no input prompt follows it. A picker
// of several questions is headed by a tab bar ("←  ☐ Colours  ☐ Pet  ✔
// Submit  →"), which every question's tab and the review screen show
// alike: its signature is that bar, compact, tick marks evened out, so
// moving between the tabs or to the review screen at the Mac answers
// nothing. A single question's signature is its header and its question
// (PickerView). Moving the cursor, ticking an option, typing into "Type
// something" or a re-wrap leaves the signature unchanged; another picker
// or no picker changes it. "" when no picker is on screen.
func PickerSignature(screen string) string {
	p, ok := ReadPicker(screen)
	switch {
	case !ok:
		return ""
	case p.Tabs != "":
		return p.Tabs
	}
	return p.View
}

// PickerView is what the picker at the bottom of the screen shows above its
// options: its header or tab bar and the question on screen, or its review
// screen's text, compact, tick marks evened out; the lines between its top
// border and its first option (from its question when its top is above
// the read). It shows which question is up. "" when no picker is on screen.
func PickerView(screen string) string {
	p, _ := ReadPicker(screen)
	return p.View
}

// PickerRead is what one read shows of the picker at its bottom.
type PickerRead struct {
	View      string   // PickerView
	Tabs      string   // its tab bar, compact, tick marks evened out, read whole: "← … ✔Submit →"; "" for a single question's header, a bar clipped at the pane's edge, or none in the read
	TabLabels []string // the labels of Tabs' question tabs, in order, repeats included ("Submit" aside)
	Whole     bool     // its top border is in the read
	Review    bool     // it is the review screen of a picker of several questions
}

// ReadPicker finds the AskUserQuestion picker at the bottom of the screen:
// the options over the footer, or the review screen of a picker of several
// questions, which Claude Code 2.1.310 draws with no footer: the tab bar,
// "Review your answers", the answers so far, "Ready to submit your
// answers?" and the options "1. Submit answers" and "2. Cancel" as the
// read's last lines (a digit submits or cancels it). The review screen
// counts only with its tab bar and top border in the read. A read may hold
// less than the whole picker: its top above the read, or its tab bar
// clipped or scrolled to the active tab (not recorded; a bar leaving out
// a question's header reads as another one). Whether that is enough to
// tell the picker is the caller's call.
func ReadPicker(screen string) (PickerRead, bool) {
	lines := strings.Split(screen, "\n")
	first, review := -1, false
	if foot := pickerFooter(lines); foot >= 0 && below(lines[foot+1:], nil) {
		for i := foot - 1; i >= 0; i-- {
			if firstOptionRe.MatchString(lines[i]) {
				first = i
				break
			}
		}
	} else if first = reviewOptions(lines); first >= 0 {
		review = true
	}
	if first < 0 {
		return PickerRead{}, false
	}
	border := topBorder(lines, first)
	from := border + 1
	if border < 0 {
		if review {
			return PickerRead{}, false
		}
		// The picker's top is above the read: start at its question, the
		// block of text right above the options, however it wraps. The
		// picker is not read whole, whatever is above that block (its tab
		// bar, a blank line): only its top border proves its top is in.
		from = first
		for from > 0 && strings.TrimSpace(lines[from-1]) == "" {
			from--
		}
		for from > 0 && strings.TrimSpace(lines[from-1]) != "" {
			from--
		}
	}
	p := PickerRead{View: ticks.Replace(compact(lines[from:first])), Review: review, Whole: border >= 0}
	if p.Whole {
		p.Tabs, p.TabLabels = tabBar(lines[from:first])
	}
	if review && (p.Tabs == "" || !strings.Contains(p.View, "Reviewyouranswers")) {
		return PickerRead{}, false
	}
	return p, true
}

// pickerMarks are drawn by the AskUserQuestion picker only: the marks of
// its tab bar or header, its review screen's first option, its free-text
// option, its footer's first words and its last option.
var pickerMarks = []string{"☐", "☒", "☑", "✔ Submit", "Submit answers", "Type something", "Enter to select", "Chat about this"}

// PickerMarkers reports whether a read shows the picker's own marks
// (pickerMarks) below the input box, or anywhere when the read has none (a
// dialog or picker hides it): a picker the read may not hold whole (its
// tab bar clipped or scrolled, its top above the read, its review screen
// cut, its footer clipped). Nothing may be typed into such a screen, and a
// waiting hook must not take it for its picker gone. The input box is a
// "❯" line at column 0 right under a rule, with what it holds (wrapped
// onto more lines) up to the next rule; only the lines after that count.
func PickerMarkers(screen string) bool {
	lines := strings.Split(screen, "\n")
	from := 0
	for i := len(lines) - 1; i > 0; i-- {
		if strings.HasPrefix(lines[i], "❯") && strings.HasPrefix(strings.TrimSpace(lines[i-1]), "─") {
			from = i + 1
			for from < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[from]), "─") {
				from++
			}
			break
		}
	}
	for _, l := range lines[min(from, len(lines)):] {
		for _, m := range pickerMarks {
			if strings.Contains(l, m) {
				return true
			}
		}
	}
	return false
}

// reviewOptions is the index of the first option of a picker's review
// screen: the read's last two non-blank lines are "1. Submit answers" and
// "2. Cancel", the cursor on either. -1 when they are not.
func reviewOptions(lines []string) int {
	last := len(lines) - 1
	for last >= 0 && strings.TrimSpace(lines[last]) == "" {
		last--
	}
	if last < 1 || !reviewCancelRe.MatchString(lines[last]) || !reviewSubmitRe.MatchString(lines[last-1]) {
		return -1
	}
	return last - 1
}

// tabMarkRe is a tab's mark in the tab bar: unanswered, answered, Submit.
var tabMarkRe = regexp.MustCompile(`[☐☒☑✔]`)

// tabBar is the tab bar of a picker of several questions, compact, tick
// marks evened out, and its question tabs' labels in order: the first
// lines of the block under the picker's top border, up to a blank line,
// reading "← ☐ A ☐ B ✔ Submit →" (a pane narrower than the bar wraps it).
// "" and no labels when the block starts otherwise (a single question's
// header) or the bar is not read whole.
func tabBar(block []string) (string, []string) {
	start := 0
	for start < len(block) && strings.TrimSpace(block[start]) == "" {
		start++
	}
	end := start
	for end < len(block) && strings.TrimSpace(block[end]) != "" {
		end++
	}
	bar := ticks.Replace(compact(block[start:end]))
	if !strings.HasPrefix(bar, "←") || !strings.HasSuffix(bar, "→") || !strings.Contains(bar, "✔Submit") {
		return "", nil
	}
	text := strings.Join(strings.Fields(strings.Join(block[start:end], " ")), " ")
	inner := strings.TrimSuffix(strings.TrimPrefix(text, "←"), "→")
	marks := tabMarkRe.FindAllStringIndex(inner, -1)
	if len(marks) < 2 || strings.TrimSpace(inner[:marks[0][0]]) != "" {
		return "", nil
	}
	var labels []string
	for i, m := range marks {
		end := len(inner)
		if i+1 < len(marks) {
			end = marks[i+1][0]
		}
		label := strings.TrimSpace(inner[m[1]:end])
		last := i == len(marks)-1
		switch {
		case last && (inner[m[0]:m[1]] != "✔" || label != "Submit"):
			return "", nil
		case last:
		case label == "" || inner[m[0]:m[1]] == "✔":
			return "", nil
		default:
			labels = append(labels, label)
		}
	}
	return bar, labels
}

// pickerFooter is the index of the first line of the picker's footer,
// "Enter to select · … · Esc to cancel", which a pane narrower than it
// (49 columns for a single-choice picker, up to 74 with "ctrl+g to edit in
// Vim") wraps over several lines: the trailing run of non-blank lines is
// read from the bottom up, compact, until it holds both words. -1 when it
// never does, or a blank line comes first; the picker always draws one
// above its footer.
func pickerFooter(lines []string) int {
	acc := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			if acc == "" {
				continue
			}
			return -1
		}
		acc = compact(lines[i:i+1]) + acc
		if strings.Contains(acc, "Entertoselect") && strings.Contains(acc, "Esctocancel") {
			return i
		}
	}
	return -1
}

// SignatureHas reports whether a dialog's or picker's signature shows key, a
// text of the call it is for (ruling R30): whitespace and line breaks
// aside, as in the signature. An empty key matches nothing.
func SignatureHas(sig, key string) bool {
	k := strings.Join(strings.Fields(key), "")
	return k != "" && strings.Contains(sig, k)
}

// ticks evens out the tab bar's per-question marks, which a tick at the Mac
// flips before the question is answered.
var ticks = strings.NewReplacer("☒", "☐", "☑", "☐")

// below reports whether lines (what follows a dialog's question or a
// picker's footer) hold no input prompt and, when want is set, a line it
// matches. Quoted dialog text in an answer always sits above the prompt.
func below(lines []string, want *regexp.Regexp) bool {
	found := want == nil
	for _, l := range lines {
		if inputRe.MatchString(strings.TrimRight(l, " \t")) {
			return false
		}
		if want != nil && want.MatchString(l) {
			found = true
		}
	}
	return found
}

// topBorder is the index of the rule closest above line i, the top border of
// the dialog or picker i belongs to; -1 when it is above the screen read (a
// dialog taller than the read).
func topBorder(lines []string, i int) int {
	for j := i - 1; j >= 0; j-- {
		if rule(lines[j]) {
			return j
		}
	}
	return -1
}

// rule reports whether l is a horizontal rule of box-drawing dashes.
func rule(l string) bool {
	l = strings.TrimSpace(l)
	return len(l) > 0 && strings.Trim(l, "─") == ""
}

// compact joins lines into a signature without what a redraw at another
// width changes: all whitespace goes (a re-wrap moves line breaks between
// words or, for a long word, inside it), a rule of any length is one "─",
// and the "│" gutter that starts every row of a boxed text goes too:
// Claude Code 2.1.310 draws a Bash command wider than 80 columns or with a
// newline (and a long WebFetch url) in a box with only a left border, and
// a re-wrap moves those rows.
func compact(lines []string) string {
	var b strings.Builder
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t != "" && strings.Trim(t, "─╌") == "" {
			b.WriteString("─")
			continue
		}
		for _, f := range strings.Fields(strings.TrimPrefix(t, "│")) {
			b.WriteString(f)
		}
	}
	return b.String()
}

// OneLine collapses whitespace so the text submits as a single prompt.
func OneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
