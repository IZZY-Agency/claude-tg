package speech

import (
	"regexp"
	"strings"
)

// clockTime is a time of day written with a colon ("19:02", "7:30").
var clockTime = regexp.MustCompile(`\b([01]?\d|2[0-3]):([0-5]\d)\b`)

// unitPair is a slash between two short words ("км/год", "km/h",
// "and/or"), alone in its token once the punctuation around it is trimmed
// (around).
var unitPair = regexp.MustCompile(`^\p{L}{1,4}/\p{L}{1,4}$`)

// around is the punctuation trimmed off a token before unitPair is tried:
// sentence marks, brackets and quotes, nothing that a path or a name holds.
const around = `.,;:!?…()[]{}"'“”„‘’«»`

// token is a run of non-space characters.
var token = regexp.MustCompile(`\S+`)

// guillemets turns «» into “”, which no voice reads out.
var guillemets = strings.NewReplacer("«", "“", "»", "”")

// Voiced rewrites what speech engines read out as symbols in text spoken
// in lang (Language), as measured with the Piper voices (2026-10-26): «»
// become “”, which the Ukrainian voice does not say as "відкриті лапки";
// a slash between two short words ("км/год", "and/or") becomes a space,
// except in French, whose voice reads "km/h" well, while a slash in a
// path or an address ("src/config") is still read; and in Ukrainian the
// colon of a time of day ("19:02", read "дві крапки") becomes a space,
// unless it is part of a longer run of colons ("1:2:30"). A ratio written
// like a time ("3:10") is read like one.
func Voiced(text, lang string) string {
	text = guillemets.Replace(text)
	if lang != "fr" {
		text = spaceUnitSlashes(text)
	}
	if lang == "uk" {
		text = spaceClockColons(text)
	}
	return text
}

// spaceUnitSlashes replaces the slash of each token that is a unitPair by
// a space.
func spaceUnitSlashes(text string) string {
	if !strings.Contains(text, "/") {
		return text
	}
	return token.ReplaceAllStringFunc(text, func(t string) string {
		core := strings.Trim(t, around)
		if !unitPair.MatchString(core) {
			return t
		}
		return strings.Replace(t, "/", " ", 1)
	})
}

// spaceClockColons replaces the colon of each clockTime by a space, unless
// a colon touches the time on either side ("1:2:30", "12:34:56").
func spaceClockColons(text string) string {
	var b strings.Builder
	last := 0
	for _, m := range clockTime.FindAllStringSubmatchIndex(text, -1) {
		start, end := m[0], m[1]
		if (start > 0 && text[start-1] == ':') || (end < len(text) && text[end] == ':') {
			continue
		}
		b.WriteString(text[last:start])
		b.WriteString(text[m[2]:m[3]] + " " + text[m[4]:m[5]])
		last = end
	}
	b.WriteString(text[last:])
	return b.String()
}
