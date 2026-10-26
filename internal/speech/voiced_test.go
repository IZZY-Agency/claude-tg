package speech

import "testing"

// Voiced rewrites what speech engines read out as symbols, measured with
// the Piper voices (espeak-ng phonemes, 2026-10-26): the Ukrainian voice
// says «» ("відкриті лапки"), a colon in a time ("дві крапки") and a
// slash ("коса риска"); the Russian and English ones say a slash too.
// The French voice reads "km/h" and "19:02" well, so both stay.
func TestVoiced(t *testing.T) {
	for _, tc := range []struct{ text, lang, want string }{
		{"точку «Ліон» пошук не знайшов", "uk", "точку “Ліон” пошук не знайшов"},
		{"бо «так»", "ru", "бо “так”"},
		{"Вітер 6 км/год, о 19:02.", "uk", "Вітер 6 км год, о 19 02."},
		{"Ветер 6 км/ч, в 19:02.", "ru", "Ветер 6 км ч, в 19:02."},
		{"Wind 6 km/h at 9:05, and/or rain.", "en", "Wind 6 km h at 9:05, and or rain."},
		{"Vent 6 km/h à 19:02, «Lyon».", "fr", "Vent 6 km/h à 19:02, “Lyon”."},
		{"(км/год), м/с.", "uk", "(км год), м с."},
		{"'km/h' and \"m/s\"", "en", "'km h' and \"m s\""},
		// A slash in a path or address is read, as it means something.
		{"a/b/c", "en", "a/b/c"},
		{"Open src/config or example.com/help", "en", "Open src/config or example.com/help"},
		{"input/output", "en", "input/output"},
		{"src/log2, src/log_ and $a/b", "en", "src/log2, src/log_ and $a/b"},
		{"24/7, 1/2, x/2", "uk", "24/7, 1/2, x/2"},
		{"Пам'ять і пам’ять", "uk", "Пам'ять і пам’ять"},
		// Only a time of day, alone: not a longer run of colons, not 25:00.
		{"о 7:30 і о 123:45", "uk", "о 7 30 і о 123:45"},
		{"Тривалість 1:2:30, о 12:34:56", "uk", "Тривалість 1:2:30, о 12:34:56"},
		{"о 25:00 чи 23:59", "uk", "о 25:00 чи 23 59"},
		// A ratio written like a time reads like one.
		{"Співвідношення 3:10", "uk", "Співвідношення 3 10"},
		{"без мови", "", "без мови"},
	} {
		if got := Voiced(tc.text, tc.lang); got != tc.want {
			t.Errorf("Voiced(%q, %q) = %q, want %q", tc.text, tc.lang, got, tc.want)
		}
	}
}
