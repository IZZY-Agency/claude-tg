package speech

import "testing"

func TestLanguage(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Готово, тесты прошли.", "ru"},
		{"Готово, тести пройшли, реліз зібрано.", "uk"},
		{"Готово, тести пройшли, їжак ы.", "ru"},
		{"PR смержен в main", "ru"},
		{"Done, the tests pass.", "en"},
		{"C'est prêt, les tests sont passés.", "fr"},
		{"Les tests sont ok pour la version", "fr"},
		{"The tests are ok pour", "en"},
		{"12345 ...", ""},
		{"Да.", "ru"},
		{"Ні.", "uk"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := Language(tc.in); got != tc.want {
			t.Errorf("Language(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// An answer with fewer than three letters takes the language of what it
// answers, when that is known; otherwise, or with more letters, its own.
func TestLanguageOr(t *testing.T) {
	for _, tc := range []struct{ text, fallback, want string }{
		{"2 + 2 = 4.", "ru", "ru"},
		{"13 °C", "uk", "uk"},
		{"Да.", "uk", "uk"},
		{"Да.", "", "ru"},
		{"2 + 2 = 4.", "", ""},
		{"Yes.", "ru", "en"},
		{"Done, the tests pass.", "ru", "en"},
	} {
		if got := LanguageOr(tc.text, tc.fallback); got != tc.want {
			t.Errorf("LanguageOr(%q, %q) = %q, want %q", tc.text, tc.fallback, got, tc.want)
		}
	}
}
