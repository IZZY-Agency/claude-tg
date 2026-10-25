package render

import (
	"html"
	"regexp"
	"strings"
)

var tokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bghp_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`\bxox[bp]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`\bAKIA[A-Z0-9]{16}`),
}

var kvPattern = regexp.MustCompile(`(?i)\b(token|key|secret|password)=(\S{8,})`)

// Redact replaces obvious credentials with [redacted]. It is a safety net,
// not a guarantee; callers must still avoid putting secrets in messages.
// It is for raw text (plain messages, button texts, logs, stored
// summaries): a value runs to the next space, so on HTML it would swallow a
// closing tag. Use RedactHTML for HTML.
func Redact(s string) string {
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
