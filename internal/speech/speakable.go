// Package speech turns a Claude answer into text a speech engine may read,
// guesses the language of that text and reads the duration of the OGG/Opus
// file the engine wrote.
package speech

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxRunes is the longest speakable text, in runes. At speech rates that is
// about 30 to 35 seconds of audio.
const MaxRunes = 500

// placeholderBase is the first rune of a private-use run that stands in for
// a kept inline-code span while the rest of its line is cleaned, so that
// emphasis removal and path detection never touch the span's content.
const placeholderBase = 0xF0000

var (
	horizontalRule = regexp.MustCompile(`^(?:-{3,}|\*{3,}|_{3,})$`)
	headingMark    = regexp.MustCompile(`^#{1,6}\s+`)
	bulletMark     = regexp.MustCompile(`^[-*+]\s+(?:\[[ xX]\](?:\s+|$))?`) // a task list's box goes with its bullet
	htmlTag        = regexp.MustCompile(`<!--.*?-->|</?[A-Za-z][^>]*>`)
	markdownLink   = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	fileExtension  = regexp.MustCompile(`\.[A-Za-z0-9]{1,5}$`)
	listItem       = regexp.MustCompile(`^(?:[-*+]|[0-9]{1,9}[.)])(?:\s|$)`)
	setextLine     = regexp.MustCompile(`^(?:=+|-+)$`)
	delimiterCell  = regexp.MustCompile(`^:?-+:?$`)
)

// Prepare turns Markdown into text a speech engine may read. kept is the
// rune count of text; dropped is the rune count of content that was removed
// because it does not read aloud: fenced blocks with their fences, table
// lines (tableLines), HTML tags, URLs, paths and inline-code spans that
// were dropped. Markup markers and pictographs count in neither. Blockquote
// marks go first, so a fence or table inside a quote is dropped like any
// other.
func Prepare(markdown string) (text string, kept, dropped int) {
	var lines []string
	var fence string // the opening fence of the block being skipped
	all := plainLines(markdown)
	table := tableLines(all)
	for i, line := range all {
		if fence != "" {
			dropped += utf8.RuneCountInString(line)
			if closesFence(line, fence) {
				fence = ""
			}
			continue
		}
		if f := openingFence(line); f != "" {
			fence = f
			dropped += utf8.RuneCountInString(line)
			continue
		}
		if table[i] {
			dropped += utf8.RuneCountInString(line)
			continue
		}
		if horizontalRule.MatchString(line) {
			continue
		}
		line, d := cleanLine(line)
		dropped += d
		if line == "" || !strings.ContainsFunc(line, isLetterOrDigit) {
			continue
		}
		line = endSentence(line)
		lines = append(lines, line)
	}
	text = strings.Join(lines, " ")
	text = strings.Join(strings.Fields(text), " ")
	text = closeSpaceBeforePunct(text)
	return text, utf8.RuneCountInString(text), dropped
}

// Speakable runs Prepare and reports whether the result should be spoken:
// there is something to say, it fits MaxRunes, and the answer is not mostly
// content that was dropped.
func Speakable(markdown string) (string, bool) {
	text, kept, dropped := Prepare(markdown)
	return text, kept > 0 && kept <= MaxRunes && dropped <= kept
}

// The reasons Choose gives for an answer of which nothing may be spoken.
const (
	reasonCode = "code block or table"
	reasonLong = "long or mostly code"
)

// Choice is what Choose picked of an answer to speak.
type Choice struct {
	Text     string // the speakable text to say: Markdown made speakable
	Markdown string // the part a classifier judges: the whole answer or its opening (Opening)
	Opening  bool   // Markdown is only the opening paragraph, or a lead-in and the paragraph after it
	Reason   string // "" when something may be spoken; else "code block or table" or "long or mostly code"
}

// Choose picks what of an answer may be spoken, for a classifier to judge
// (spec 2026-10-26-voice-classifier-design.md, "Hard rules" and "Long
// answers: the opening paragraph"). In order: a fenced code block anywhere
// rules out everything (code is always text); an answer with no table
// line, list item or heading line (structured) that Speakable accepts is
// spoken whole; else its opening paragraph (openingParagraphs) when
// Speakable accepts it and its speakable text does not end in a colon,
// which announces what follows ("Here is the weather in Paris:"). An
// opening paragraph of at most leadInWords words, or one that ends in a
// colon, followed by another prose paragraph is tried with that paragraph
// first, by the same test: a lead-in ("Sure, here it is in Ukrainian.")
// says too little alone, a colon lead announces the paragraph, and a short
// opening that does say something is kept, not dropped; else,
// when no opening paragraph qualifies, a whole answer with no table line
// that Speakable accepts, lists and headings and all: the classifier then
// judges whether it listens well. Otherwise nothing is, for a table when
// the answer holds one, else for its length or code. Fences, quotes and
// table lines are found as Prepare finds them.
func Choose(markdown string) Choice {
	lines := plainLines(markdown)
	table := false
	for _, t := range tableLines(lines) {
		table = table || t
	}
	structured := false
	for i, line := range lines {
		if openingFence(line) != "" {
			return Choice{Reason: reasonCode}
		}
		structured = structured || listItem.MatchString(line) || heading(line) ||
			(i > 0 && lines[i-1] != "" && setextLine.MatchString(line))
	}
	var whole Choice
	if !table {
		if text, ok := Speakable(markdown); ok {
			whole = Choice{Text: text, Markdown: markdown}
		}
	}
	if whole.Text != "" && !structured {
		return whole
	}
	if first, next := openingParagraphs(markdown); first != "" {
		tries := []string{first}
		if next != "" && (len(strings.Fields(first)) <= leadInWords || colonLead(first)) {
			tries = []string{first + "\n\n" + next, first}
		}
		for _, p := range tries {
			if text, ok := Speakable(p); ok && !strings.HasSuffix(text, ":") {
				return Choice{Text: text, Markdown: p, Opening: true}
			}
		}
	}
	switch {
	case whole.Text != "":
		return whole
	case table:
		return Choice{Reason: reasonCode}
	}
	return Choice{Reason: reasonLong}
}

// leadInWords is the most words an opening paragraph has for Choose to
// speak the next prose paragraph with it (owner's ruling, 2026-10-26).
const leadInWords = 8

// colonLead reports whether a paragraph's speakable text ends in a colon:
// it announces what follows.
func colonLead(paragraph string) bool {
	text, _ := Speakable(paragraph)
	return strings.HasSuffix(text, ":")
}

// openingParagraphs returns the prose lines that open markdown, after
// leading blank lines, up to the first blank line or the first line that
// is not prose (a list, a table or a heading right after the lead), joined
// by "\n" without their line ends; first is "" when the first line is not
// prose, or when the lines are a setext heading: a line of '=' or '-'
// under them. next is the paragraph after first by the same rules, when
// only blank lines part them; "" when first ended at a line that is not
// prose, or when no prose paragraph follows.
func openingParagraphs(markdown string) (first, next string) {
	table := tableLines(plainLines(markdown))
	lines := strings.Split(markdown, "\n")
	i := 0
	// paragraph reads the paragraph from line i on, and reports whether a
	// blank line or the end, not a line that is not prose, ended it.
	paragraph := func() (string, bool) {
		var para []string
		for ; i < len(lines); i++ {
			raw := strings.TrimSuffix(lines[i], "\r")
			line := strings.TrimSpace(raw)
			switch {
			case line == "" && len(para) == 0:
				continue
			case len(para) > 0 && setextLine.MatchString(line):
				return "", false
			case line == "":
				return strings.Join(para, "\n"), true
			case table[i] || !prose(line):
				return strings.Join(para, "\n"), false
			}
			para = append(para, raw)
		}
		return strings.Join(para, "\n"), true
	}
	first, blank := paragraph()
	if first == "" || !blank {
		return first, ""
	}
	next, _ = paragraph()
	return first, next
}

// prose reports whether a trimmed line that is not a table line
// (tableLines) reads as a paragraph's prose: it is not a heading, a list
// item, a quote, a fence or a horizontal rule.
func prose(line string) bool {
	return !heading(line) && !listItem.MatchString(line) && !strings.HasPrefix(line, ">") &&
		openingFence(line) == "" && !horizontalRule.MatchString(line)
}

// heading reports whether a trimmed line is a heading: an ATX heading
// ('#'), or a line wholly wrapped in one emphasis (emphasisLine), which an
// answer uses as one ("**Summary**").
func heading(line string) bool {
	return strings.HasPrefix(line, "#") || emphasisLine(line)
}

// emphasisLine reports whether a trimmed line is wholly one emphasis span:
// **Summary**, __Summary__, *Summary* or _Summary_ (***Summary*** too).
// A line that only starts and ends with emphasis ("**Paris:** rain, then
// **cloud**") is not one: its inner text holds the marker again.
func emphasisLine(line string) bool {
	for _, mark := range []string{"**", "__", "*", "_"} {
		inner, ok := strings.CutPrefix(line, mark)
		if !ok {
			continue
		}
		if inner, ok = strings.CutSuffix(inner, mark); ok && inner != "" &&
			strings.TrimSpace(inner) == inner && !strings.Contains(inner, mark) {
			return true
		}
	}
	return false
}

// unquote strips the blockquote marks ('>' with or without a space after,
// nested or not) from the start of a trimmed line.
func unquote(line string) string {
	for strings.HasPrefix(line, ">") {
		line = strings.TrimSpace(line[1:])
	}
	return line
}

// openingFence returns the fence run (three or more backticks or tildes) a
// line opens, or "" when the line is not a fence.
func openingFence(line string) string {
	if line == "" || (line[0] != '`' && line[0] != '~') {
		return ""
	}
	n := len(line) - len(strings.TrimLeft(line, line[:1]))
	if n < 3 {
		return ""
	}
	return line[:n]
}

// closesFence reports whether line closes a block opened by fence: a run of
// the same character at least as long, and nothing else.
func closesFence(line, fence string) bool {
	run := strings.TrimLeft(line, fence[:1])
	return run == "" && len(line) >= len(fence)
}

// plainLines splits markdown into its lines, each trimmed and with its
// blockquote marks stripped (unquote), as Prepare and Choose read them.
func plainLines(markdown string) []string {
	lines := strings.Split(markdown, "\n")
	for i, raw := range lines {
		lines[i] = unquote(strings.TrimSpace(raw))
	}
	return lines
}

// tableLines marks which of lines (plainLines) are table lines, as Prepare
// drops and Choose finds them: every line starting with '|', and each GFM
// table written without outer pipes: a delimiter row (delimiterRow) right
// under a header line holding a '|', that header, and the non-blank lines
// holding a '|' right below the delimiter row.
func tableLines(lines []string) []bool {
	table := make([]bool, len(lines))
	for i := 0; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "|") {
			table[i] = true
		}
		if i == 0 || !strings.Contains(lines[i-1], "|") || !delimiterRow(lines[i]) {
			continue
		}
		table[i-1], table[i] = true, true
		for i+1 < len(lines) && lines[i+1] != "" && strings.Contains(lines[i+1], "|") {
			i++
			table[i] = true
		}
	}
	return table
}

// delimiterRow reports whether a plain line is a GFM table's delimiter
// row: cells of one or more '-', each with an optional ':' at either end,
// separated by '|', with or without outer pipes (Claude Code's renderer
// takes "- | -" too). A line with no '|' is a horizontal rule, a setext
// underline or a bullet, never one.
func delimiterRow(line string) bool {
	if !strings.Contains(line, "|") {
		return false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(line, "|"), "|")
	for _, cell := range strings.Split(inner, "|") {
		if !delimiterCell.MatchString(strings.TrimSpace(cell)) {
			return false
		}
	}
	return true
}

// cleanLine strips Markdown from one trimmed prose line and returns the
// speakable remainder with the rune count of what it dropped.
func cleanLine(line string) (string, int) {
	line = strings.Map(func(r rune) rune {
		if isPictograph(r) {
			return -1
		}
		return r
	}, line)
	line = strings.TrimSpace(line)

	line = headingMark.ReplaceAllString(line, "")
	line = bulletMark.ReplaceAllString(line, "")

	dropped := 0
	line, spans, d := takeCodeSpans(line)
	dropped += d
	line = strings.ReplaceAll(line, "~~", "") // strikethrough: the words stay

	for _, m := range htmlTag.FindAllString(line, -1) {
		dropped += utf8.RuneCountInString(m)
	}
	line = htmlTag.ReplaceAllString(line, " ")
	line = markdownLink.ReplaceAllString(line, "$1")

	tokens := strings.Fields(line)
	for i, tok := range tokens {
		lead, core, trail := splitToken(tok)
		if isURL(core) || isPath(core) {
			dropped += utf8.RuneCountInString(core)
			core = ""
		}
		tokens[i] = lead + core + trail
	}
	line = removeEmphasis(strings.Join(tokens, " "))

	var b strings.Builder
	for _, r := range line {
		if i := int(r) - placeholderBase; i >= 0 && i < len(spans) {
			b.WriteString(spans[i])
		} else {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String()), dropped
}

// takeCodeSpans replaces every inline-code span of line: a span that reads as
// a word or two becomes a placeholder rune (its content is returned in
// spans), any other span becomes a space and its runes, backticks included,
// are counted as dropped. An unmatched backtick run is removed.
func takeCodeSpans(line string) (out string, spans []string, dropped int) {
	var b strings.Builder
	for len(line) > 0 {
		start := strings.IndexByte(line, '`')
		if start < 0 {
			b.WriteString(line)
			break
		}
		b.WriteString(line[:start])
		line = line[start:]
		n := len(line) - len(strings.TrimLeft(line, "`"))
		end := indexRun(line[n:], n)
		if end < 0 {
			line = line[n:] // stray backticks are markup, not speech
			continue
		}
		content := strings.TrimSpace(line[n : n+end])
		if readsAsWords(content) {
			b.WriteRune(rune(placeholderBase + len(spans)))
			spans = append(spans, content)
		} else {
			b.WriteByte(' ')
			dropped += utf8.RuneCountInString(line[:n+end+n])
		}
		line = line[n+end+n:]
	}
	return b.String(), spans, dropped
}

// indexRun returns the index in s of the first run of exactly n backticks,
// or -1.
func indexRun(s string, n int) int {
	for i := 0; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		run := len(s[i:]) - len(strings.TrimLeft(s[i:], "`"))
		if run == n {
			return i
		}
		i += run
	}
	return -1
}

// readsAsWords reports whether the content of an inline-code span reads as
// speech: at most three space-separated words made of letters, digits, '-',
// '_' and '.', none starting with '-'.
func readsAsWords(content string) bool {
	words := strings.Fields(content)
	if len(words) == 0 || len(words) > 3 {
		return false
	}
	for _, w := range words {
		if strings.HasPrefix(w, "-") {
			return false
		}
		for _, r := range w {
			if !isLetterOrDigit(r) && r != '-' && r != '_' && r != '.' {
				return false
			}
		}
	}
	return true
}

const (
	leadingPunct  = "([{\"'«<*_"
	trailingPunct = ")]}\"'»>.,;:!?…*_"
)

// splitToken separates the punctuation around a whitespace-delimited token
// from its core.
func splitToken(tok string) (lead, core, trail string) {
	core = strings.TrimLeft(tok, leadingPunct)
	lead = tok[:len(tok)-len(core)]
	trimmed := strings.TrimRight(core, trailingPunct)
	trail = core[len(trimmed):]
	return lead, trimmed, trail
}

// isURL reports whether a token is an http or https URL.
func isURL(tok string) bool {
	l := strings.ToLower(tok)
	return strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://")
}

// isPath reports whether a token looks like a file path: it starts like
// one, has two or more slashes, or has one slash and ends in a file
// extension (src/main.go). and/or, TCP/IP, km/h and 24/7 are words.
func isPath(tok string) bool {
	for _, p := range []string{"/", "~/", "./", "../"} {
		if strings.HasPrefix(tok, p) {
			return true
		}
	}
	switch strings.Count(tok, "/") {
	case 0:
		return false
	case 1:
		return fileExtension.MatchString(tok[strings.IndexByte(tok, '/')+1:])
	}
	return true
}

// removeEmphasis deletes Markdown emphasis markers: every '*', and each run
// of '_' unless it sits between two letters or digits, so snake_case stays.
func removeEmphasis(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i := 0; i < len(rs); {
		switch rs[i] {
		case '*':
			i++
		case '_':
			j := i
			for j < len(rs) && rs[j] == '_' {
				j++
			}
			if i > 0 && j < len(rs) && isLetterOrDigit(rs[i-1]) && isLetterOrDigit(rs[j]) {
				b.WriteString(string(rs[i:j]))
			}
			i = j
		default:
			b.WriteRune(rs[i])
			i++
		}
	}
	return b.String()
}

// isPictograph reports whether r is a symbol a voice should not read: emoji
// and other symbols, arrows, variation selector 16 and the zero-width joiner.
// The degree, copyright, registered, trade mark and numero signs are symbols
// too, but they read as words, so they stay.
func isPictograph(r rune) bool {
	switch r {
	case '°', '©', '®', '™', '№':
		return false
	}
	return unicode.Is(unicode.So, r) || (r >= 0x2190 && r <= 0x21FF) || r == 0xFE0F || r == 0x200D
}

// closingMarks may follow the punctuation that ends a sentence.
const closingMarks = "\"'”’»)]}"

// endSentence makes sure a line ends in sentence punctuation so the voice
// pauses: a trailing comma becomes a dot, and a line that already ends in
// . ! ? … : ; (possibly followed by closing quotes or brackets) is left
// alone.
func endSentence(line string) string {
	if strings.HasSuffix(line, ",") {
		return line[:len(line)-1] + "."
	}
	core := strings.TrimRight(line, closingMarks)
	if r, _ := utf8.DecodeLastRuneInString(core); strings.ContainsRune(".!?…:;", r) {
		return line
	}
	return line + "."
}

// closeSpaceBeforePunct removes a space that sits before , ! ? : ; and before
// a '.' that is followed by a space, the end of the text or another such
// mark. A space before ".gitignore" stays, since that dot starts a name.
func closeSpaceBeforePunct(s string) string {
	const marks = ",.!?:;"
	rs := []rune(s)
	var b strings.Builder
	for i, r := range rs {
		if r == ' ' && i+1 < len(rs) && strings.ContainsRune(marks, rs[i+1]) {
			j := i + 1
			if rs[j] != '.' || j+1 == len(rs) || rs[j+1] == ' ' || strings.ContainsRune(marks, rs[j+1]) {
				continue
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isLetterOrDigit(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}
