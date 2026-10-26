package render

import (
	"strings"
	"testing"
)

// AsShown is text as tg's HTML shows it once redacted: a value that
// escaping lengthens past the redaction threshold is redacted too, and
// text with nothing to redact reads back unchanged.
func TestAsShown(t *testing.T) {
	for in, want := range map[string]string{
		"Ops password=ab&cd":       "Ops password=[redacted]",
		"Ops password=abcdefghij":  "Ops password=[redacted]",
		"Ops password=[redacted]":  "Ops password=[redacted]",
		"a & b <c>":                "a & b <c>",
		"nova · Login token claim": "nova · Login token claim",
	} {
		if got := AsShown(in); got != want {
			t.Errorf("AsShown(%q) = %q, want %q", in, got, want)
		}
		if got := AsShown(AsShown(in)); got != AsShown(in) {
			t.Errorf("AsShown is not stable on %q: %q", in, got)
		}
	}
}

// Redaction is stable: keys written back to back are all masked at once,
// so cleaning a cleaned text changes nothing.
func TestRedactStable(t *testing.T) {
	k := "AKIA" + strings.Repeat("A", 16)
	for _, in := range []string{k + k + k, strings.Repeat(k, 70), "x " + k + k + " y", "password=" + k} {
		once := Redact(in)
		if strings.Contains(once, "AKIA") || Redact(once) != once || AsShown(AsShown(in)) != AsShown(in) {
			t.Errorf("Redact(%q) = %q, again %q", in, once, Redact(once))
		}
	}
}

// Back-to-back tokens are masked whole in one pass: a run of AWS keys, and
// GitHub tokens whose body the next one's underscore used to cut, leaving
// it in clear.
func TestRedactRunsInOnePass(t *testing.T) {
	k := "AKIA" + strings.Repeat("B", 16)
	g1, g2 := "ghp_"+strings.Repeat("a", 36), "ghp_"+strings.Repeat("b", 36)
	for _, in := range []string{strings.Repeat(k, 70), g1 + g2, "x " + g1 + g2 + " y"} {
		once := redactOnce(in)
		if strings.Contains(once, "AKIA") || strings.Contains(once, "aaaa") || strings.Contains(once, "bbbb") {
			t.Errorf("redactOnce(%.30q) = %.60q", in, once)
		}
	}
}

// ShownName is a session name as every message shows it, final: redacted
// as HTML shows it, a redacted value as "•••", single spaces, at most 64
// runes; cleaning, cutting or shaping it again changes nothing.
func TestShownName(t *testing.T) {
	for _, in := range []string{
		strings.Repeat("x", 51) + " key=abcdefg suffix",
		strings.Repeat("x", 46) + " password=abcdefghij",
		"Ops\tstaging\nnow",
		"Ops password=ab&cd",
		strings.Repeat("AKIA"+strings.Repeat("C", 16), 5),
		"nova · Login token claim failure on…",
	} {
		got := ShownName(in)
		if ShownName(got) != got || Redact(got) != got || AsShown(got) != got || strings.ContainsAny(got, "\t\n") || len([]rune(got)) > MaxName {
			t.Errorf("ShownName(%.40q) = %q, not final", in, got)
		}
	}
	if got := ShownName("Ops\tstaging"); got != "Ops staging" {
		t.Errorf("ShownName = %q", got)
	}
}
