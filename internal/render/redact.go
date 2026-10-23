package render

import "regexp"

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
func Redact(s string) string {
	for _, re := range tokenPatterns {
		s = re.ReplaceAllString(s, "[redacted]")
	}
	return kvPattern.ReplaceAllString(s, "$1=[redacted]")
}
