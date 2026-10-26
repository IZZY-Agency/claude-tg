package speech

import (
	"strings"
	"unicode"
)

// minLetters is the fewest letters LanguageOr trusts text's own language
// from.
const minLetters = 3

// LanguageOr is Language, except that text with fewer than minLetters
// letters ("2 + 2 = 4.", "13 °C", "Да.") gets fallback, the language of
// what it answers, when fallback is set: so few letters say little of
// their language.
func LanguageOr(text, fallback string) string {
	if fallback != "" && letters(text) < minLetters {
		return fallback
	}
	return Language(text)
}

// letters counts text's Cyrillic and Latin letters.
func letters(text string) int {
	n := 0
	for _, r := range text {
		if unicode.Is(unicode.Cyrillic, r) || unicode.Is(unicode.Latin, r) {
			n++
		}
	}
	return n
}

// frenchWords are common French words that English text does not use.
var frenchWords = map[string]bool{
	"le": true, "la": true, "les": true, "des": true, "est": true, "une": true,
	"pour": true, "avec": true, "sont": true, "dans": true, "pas": true,
	"que": true, "qui": true, "sur": true,
}

// Language guesses the language of text from its letters: "ru", "uk", "en",
// "fr", or "" when there are no letters of either script. Cyrillic text with
// Ukrainian-only letters and none of the Russian-only ones is "uk"; Latin
// text with French accents or at least two distinct common French words is
// "fr".
func Language(text string) string {
	var cyrillic, latin int
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Cyrillic, r):
			cyrillic++
		case unicode.Is(unicode.Latin, r):
			latin++
		}
	}
	lower := strings.ToLower(text)
	switch {
	case cyrillic > latin:
		if strings.ContainsAny(lower, "іїєґ") && !strings.ContainsAny(lower, "ыэъё") {
			return "uk"
		}
		return "ru"
	case latin > cyrillic:
		if strings.ContainsAny(lower, "éèêàçùûôîœëï") {
			return "fr"
		}
		seen := map[string]bool{}
		for _, w := range strings.FieldsFunc(lower, func(r rune) bool { return !unicode.IsLetter(r) }) {
			if frenchWords[w] {
				seen[w] = true
			}
		}
		if len(seen) >= 2 {
			return "fr"
		}
		return "en"
	}
	return ""
}
