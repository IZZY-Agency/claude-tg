package term

import (
	"regexp"
	"strings"
)

var (
	optionRe = regexp.MustCompile(`^\s*❯?\s*[1-9]\. `)
	inputRe  = regexp.MustCompile(`^❯( .*)?$`) // the input prompt sits at column 0
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

// OneLine collapses whitespace so the text submits as a single prompt.
func OneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
