package render

import (
	"html"
	"regexp"
	"strings"
)

var tokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bghp_[A-Za-z0-9_]{20,}`), // "_" too: a token right after this one is masked with it
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`\bxox[bp]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`\b(?:AKIA[A-Z0-9]{16})+`), // a run of keys back to back in one match
}

var kvPattern = regexp.MustCompile(`(?i)\b(token|key|secret|password)=(\S{8,})`)

// Redact replaces obvious credentials with [redacted]. It is a safety net,
// not a guarantee; callers must still avoid putting secrets in messages.
// It is for raw text (plain messages, button texts, logs, stored
// summaries): a value runs to the next space, so on HTML it would swallow a
// closing tag. Use RedactHTML for HTML.
//
// It runs until nothing changes: keys written back to back come to light
// one by one as their neighbours are masked, and cleaning a cleaned text
// must change nothing. Each pass that changes s masks at least one value,
// so len(s) passes bound it.
func Redact(s string) string {
	for range len(s) + 1 {
		next := redactOnce(s)
		if next == s {
			break
		}
		s = next
	}
	return s
}

// redactOnce is one pass of Redact.
func redactOnce(s string) string {
	for _, re := range tokenPatterns {
		s = re.ReplaceAllString(s, "[redacted]")
	}
	return kvPattern.ReplaceAllString(s, "$1=[redacted]")
}

var htmlTag = regexp.MustCompile(`<[^>]*>`)

// RedactHTML runs Redact over the text between tags only, so a masked value
// never swallows a tag and the HTML stays valid. It is for tg's own HTML,
// where text is escaped (every "<" starts a tag) and tags carry no
// attributes worth masking.
func RedactHTML(s string) string {
	var b strings.Builder
	last := 0
	for _, loc := range htmlTag.FindAllStringIndex(s, -1) {
		b.WriteString(Redact(s[last:loc[0]]))
		b.WriteString(s[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(Redact(s[last:]))
	return b.String()
}

// StripHTML turns tg's HTML into the text Telegram would show: tags dropped,
// entities decoded. It is the plain-text form of a message whose HTML
// Telegram refused.
func StripHTML(s string) string {
	return html.UnescapeString(htmlTag.ReplaceAllString(s, ""))
}

// AsShown is plain text as tg's HTML shows it once redacted: credentials
// redacted, then escaped and redacted again as HTML (escaping can lengthen
// a value past the redaction threshold), read back as text. It is stable:
// AsShown(AsShown(s)) == AsShown(s). Session names are stored and matched
// in this form, so that a name copied from any message addresses the
// session it names.
func AsShown(s string) string {
	for range len(s) + 1 {
		next := html.UnescapeString(RedactHTML(EscapeHTML(Redact(s))))
		if next == s {
			break
		}
		s = next
	}
	return s
}

// MaxName is the most runes of a session name a message shows (send clips
// labels to it).
const MaxName = 64

// ShownName is a session name as every message shows it, final: AsShown,
// each redacted value shown as "•••" (short enough that no later cleaning
// redacts it again, and a cut cannot leave half of it to expand), every
// run of whitespace one space, and cut to MaxName runes with "…", until
// none of these changes it (a cut's "…" can lengthen a value past the
// redaction threshold). Session names are stored and matched in this
// form, so that a header, the session list and the record read alike.
func ShownName(s string) string { return nameForm(s, true) }

// FullName is ShownName without the cut: a name matched whole however long
// (Claude Code's, which addresses its session as typed).
func FullName(s string) string { return nameForm(s, false) }

// nameForm is ShownName, cut to MaxName runes or not.
func nameForm(s string, cut bool) string {
	for range len(s) + 2 {
		next := strings.ReplaceAll(AsShown(s), "[redacted]", "•••")
		next = strings.Join(strings.Fields(next), " ")
		if r := []rune(next); cut && len(r) > MaxName {
			next = string(r[:MaxName-1]) + "…"
		}
		if next == s {
			break
		}
		s = next
	}
	return s
}
