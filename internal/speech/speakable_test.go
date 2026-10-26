package speech

import (
	"strings"
	"testing"
)

func TestPrepareAndSpeakable(t *testing.T) {
	tests := []struct {
		name string
		in   string
		text string
		ok   bool
	}{
		{"bold numbers", "Готово. Тесты прошли: **840** из **840**.", "Готово. Тесты прошли: 840 из 840.", true},
		{"heading and bullets", "## Итог\n\n- PR смержен\n- релиз ждёт тебя", "Итог. PR смержен. релиз ждёт тебя.", true},
		{"link and bare url", "Details: [the docs](https://example.com/docs), or https://example.com/x for the raw file.", "Details: the docs, or for the raw file.", true},
		{"short inline code", "Run `tg relay install` to reload.", "Run tg relay install to reload.", true},
		{"identifier in code", "Set `voice_command` first.", "Set voice_command first.", true},
		{"mostly code", "Run `go test ./...` and check `/Users/alex/proj/main.go`.", "Run and check.", false},
		{"fenced block only", "Fixed:\n\n```go\nfunc a() {}\n```", "Fixed:", false},
		{"small block in long prose", "The relay restarted and the voice note went through; the reaction cleared as expected.\n\n```\nok\n```", "The relay restarted and the voice note went through; the reaction cleared as expected.", true},
		{"table", "| a | b |\n|---|---|\n| 1 | 2 |\nAll green.", "All green.", false},
		{"pictographs", "Готово ✅ → релиз", "Готово релиз.", true},
		{"paths", "Edited internal/relay/router.go and README.md.", "Edited and README.md.", false},
		{"empty", "", "", false},
		{"code only", "```\ncode only\n```", "", false},

		{"unterminated fence runs to the end", "Intro line\n```\nnever closed\nmore", "Intro line.", false},
		{"tilde fence", "Everything is done.\n~~~\nx := 1\n~~~\nBye", "Everything is done. Bye.", true},
		{"horizontal rules", "One\n---\nTwo\n***\nThree\n___", "One. Two. Three.", true},
		{"html tags", "Press <b>go</b> now<br/>please", "Press go now please.", true},
		{"blockquote", "> quoted line\n> second", "quoted line. second.", true},
		{"star bullets and plus", "* one\n+ two", "one. two.", true},
		{"emphasis variants", "This is *very* __bold__ and _soft_.", "This is very bold and soft.", true},
		{"intraword underscore stays", "See snake_case_name here", "See snake_case_name here.", true},
		{"emphasis around number", "**840**.", "840.", true},
		{"code span keeps underscores", "Use `_private_` now", "Use _private_ now.", true},
		{"code span with flag dropped", "Run `ls --all` today", "Run today.", true},
		{"code span with equals dropped", "Set `a=b` today", "Set today.", true},
		{"code span of four words dropped", "Please try this once more `one two three four` again", "Please try this once more again.", true},
		{"code span with slash dropped", "Open `a/b` now", "Open now.", true},
		{"double backtick span", "Run ``tg relay`` now", "Run tg relay now.", true},
		{"home path dropped", "See ~/notes.txt and ./run.sh and ../up", "See and and.", false},
		{"two slashes dropped, one kept", "Use and/or here, not a/b/c", "Use and/or here, not.", true},
		{"punctuation around url kept", "(see https://example.com/a).", "(see ).", false},
		{"punctuation ends in line", "Wait!", "Wait!", true},
		{"ellipsis and semicolon", "Hmm…\nNext;", "Hmm… Next;", true},
		{"no space before punctuation", "Hello ,  world !", "Hello, world!", true},
		{"image alt kept", "![the logo](https://example.com/a.png) shown", "the logo shown.", true},
		{"crlf", "A\r\nB\r\n", "A. B.", true},
		{"dotfile keeps its space", "edit .gitignore and .env and .NET and .5", "edit .gitignore and .env and .NET and .5.", true},
		{"space before comma glued to word", "Hello ,world", "Hello,world.", true},
		{"dropped path before dot", "check /a/b/c .", "check.", true},
		{"degree and legal signs stay", "It is 20°C, © 2026, ® and ™ and № 5 ✅", "It is 20°C, © 2026, ® and ™ and № 5.", true},
		{"trailing comma becomes dot", "Hello,", "Hello.", true},
		{"closing quote after dot", "He said \"go.\"", "He said \"go.\"", true},
		{"curly quote after dot", "He said “go.”", "He said “go.”", true},
		{"bracket after bang", "(done!)", "(done!)", true},
		{"guillemet after question", "«ok?»", "«ok?»", true},
		{"paren after dot", "(one.)", "(one.)", true},
		{"quote after word gets a dot", "He said \"go\"", "He said \"go\".", true},
		{"punctuation-only line skipped", "A\n...\nB", "A. B.", true},

		{"quoted fence dropped", "> ```sh\n> echo hello\n> ```\n> Done.", "Done.", false},
		{"nested quoted fence dropped", "> > ```\n> > x := 1\n>> ```\n> > Fine.", "Fine.", false},
		{"quoted table dropped", "> | a | b |\n> |---|---|\n> | 1 | 2 |\n> All green.", "All green.", false},
		{"strikethrough keeps the words", "~~old~~ new", "old new.", true},
		{"task boxes after bullets", "- [x] tests pass\n* [ ] docs\n+ [X] release", "tests pass. docs. release.", true},
		{"one-slash file paths dropped", "I fixed the bug in src/main.go today, and cmd/tg.go too.", "I fixed the bug in today, and too.", true},
		{"one-slash dotfile dropped", "See src/.env now", "See now.", true},
		{"one-slash words kept", "Use and/or, TCP/IP at 60 km/h, 24/7.", "Use and/or, TCP/IP at 60 km/h, 24/7.", true},

		{"table without outer pipes", "City | High\n--- | ---\nParis | 17 °C\nAll green.", "All green.", false},
		{"aligned delimiter row, one outer pipe", "Paris is warmer than Lyon today, by about three degrees and more.\nCity | High |\n:--- | ---: |\nParis | 17 °C |", "Paris is warmer than Lyon today, by about three degrees and more.", true},
		{"rows end at a blank line", "a | b\n--- | ---\n1 | 2\n\nso x | y holds", "so x | y holds.", false},
		{"a pipe in prose stays", "Run ls | wc now.", "Run ls | wc now.", true},
		{"a rule under a line with a pipe is no delimiter row", "Use a | b here.\n---\nDone.", "Use a | b here. Done.", true},
		{"one-dash delimiter cells", "City | High\n- | -\nParis | 17 °C\nAll green.", "All green.", false},
		{"two-dash delimiter cells", "City | High\n-- | --\nParis | 17 °C\nAll green.", "All green.", false},
		{"short aligned delimiter cells", "City | High\n:- | -:\nParis | 17 °C\nAll green.", "All green.", false},
		{"colons alone are no delimiter cell", "Use a | b here.\n: | :\nDone.", "Use a | b here. Done.", true},
		{"dash cells without a pipe header are no table", "Use a b here.\n- | -\nDone.", "Use a b here. Done.", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text, ok := Speakable(tc.in)
			if text != tc.text || ok != tc.ok {
				t.Errorf("Speakable(%q) = (%q, %v), want (%q, %v)", tc.in, text, ok, tc.text, tc.ok)
			}
			prepared, kept, _ := Prepare(tc.in)
			if prepared != text {
				t.Errorf("Prepare text %q differs from Speakable text %q", prepared, text)
			}
			if want := len([]rune(prepared)); kept != want {
				t.Errorf("kept = %d, want %d", kept, want)
			}
		})
	}
}

func TestPrepareCounts(t *testing.T) {
	tests := []struct {
		name          string
		in            string
		kept, dropped int
	}{
		{"fence with its fences", "ab\n```\nxyz\n```", 3, 3 + 3 + 3},
		{"table line", "| a |\nok", 3, 5},
		{"url", "see https://e.co/x now", 8, 14},
		{"link url counts in neither", "[ab](https://example.com/very/long)", 3, 0},
		{"dropped code span with backticks", "a `x/y` b", 4, 5},
		{"kept code span without backticks", "a `xy` b", 7, 0},
		{"markers and pictographs count in neither", "**a** ✅", 2, 0},
		{"quoted fence with its fences", "> ab\n> ```\n> xyz\n> ```", 3, 3 + 3 + 3},
		{"quoted table line", "> | a |\n> ok", 3, 5},
		{"strikethrough markers count in neither", "~~ab~~", 3, 0},
		{"one-slash file path", "a src/x.go b", 4, 8},
		{"table without outer pipes", "a | b\n--- | ---\n1 | 2\nok", 3, 5 + 9 + 5},
		{"quoted table without outer pipes", "> a | b\n> --- | ---\n> 1 | 2\n> ok", 3, 5 + 9 + 5},
		{"one-dash delimiter row", "a | b\n- | -\n1 | 2\nok", 3, 5 + 5 + 5},
		{"two-dash delimiter row", "a | b\n--|--\n1 | 2\nok", 3, 5 + 5 + 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, kept, dropped := Prepare(tc.in)
			if kept != tc.kept || dropped != tc.dropped {
				t.Errorf("Prepare(%q) kept, dropped = %d, %d; want %d, %d", tc.in, kept, dropped, tc.kept, tc.dropped)
			}
		})
	}
}

func TestSpeakableLength(t *testing.T) {
	if _, ok := Speakable(strings.Repeat("а", MaxRunes-1) + "."); !ok {
		t.Error("500 runes should be speakable")
	}
	if _, ok := Speakable(strings.Repeat("а", MaxRunes) + "."); ok {
		t.Error("501 runes should not be speakable")
	}
}

// parisLead and parisFigures make the shape of a real weather answer: a
// lead sentence, a blank line, then bulleted figures, together well over
// MaxRunes once made speakable.
const (
	parisLead    = "It's raining in Paris this morning, and the rain should stop around midday, leaving the rest of the day cloudy."
	parisFigures = "- **Temperature:** 14 °C now, rising to 17 °C this afternoon and falling to 11 °C overnight.\n" +
		"- **Rain:** about 4 mm before noon, then dry; the chance of a shower after three is under ten percent.\n" +
		"- **Wind:** from the south-west at 15 km/h, gusting to 30 km/h near the river this morning.\n" +
		"- **Humidity:** 88 percent this morning, dropping to 65 percent by the evening as the clouds thin.\n" +
		"- **Sunrise and sunset:** 8:21 and 18:37; the sun may break through briefly around five.\n" +
		"- **Tomorrow:** dry and brighter, with sunny spells, a high of 19 °C and a light breeze from the west."
	parisAnswer = parisLead + "\n\n" + parisFigures

	marseilleLead   = "It is sunny and warm in Marseille today, with a light sea breeze and no rain until the weekend."
	marseilleAnswer = marseilleLead + "\n\n" + parisFigures + "\n\n" +
		"| | Marseille | Paris |\n|---|---|---|\n| High | 24 °C | 17 °C |\n| Rain | none | 4 mm |"

	// ukAnswer is a session's real answer to "А якщо українську?" (live
	// test, 2026-10-26): a short lead-in, a prose paragraph, then a list.
	ukAnswer = "Звісно, ось українською.\n\nСтатусу поки немає: у цій сесії ще не було жодного завдання. До цього ви лише попросили відповісти «ready».\n\nЯ перевірив, чи не лишилося чогось від попередньої роботи. Нічого немає:\n- У робочій теці `voicetest` лежить лише службовий файл `.logstart` (4 байти).\n- Пам'ять для цього проєкту порожня, записів про минулі завдання немає.\n- Фонових процесів чи агентів я в цій сесії не запускав.\n\nЯкщо ви питаєте про завдання з іншої сесії чи іншого репозиторію, напишіть, яке саме (або дайте посилання на PR чи тікет). Я перевірю й розповім, що там."
	ukLead   = "Звісно, ось українською.\n\nСтатусу поки немає: у цій сесії ще не було жодного завдання. До цього ви лише попросили відповісти «ready»."

	// lesyaAnswer is a session's real answer to the voice note "А якщо
	// українською?" (rc8 live test, 2026-10-26): a colon lead of ten
	// words, a prose paragraph, then a list.
	lesyaAnswer = "Я озвучив прогноз голосом Lesya. Ось той самий текст українською:\n\nУ Парижі зараз 14:04, хмарно й прохолодно (дані AccuWeather для району Нотр-Дам).\n\n- **Температура:** 13 °C, через вітер відчувається як 8 °C.\n- **Вітер:** південний, 21 км/год, пориви до 38 км/год.\n- **Опади:** іде невеликий дощ, 0,6 мм за останню годину. За добу випало 5,8 мм.\n- **Вологість:** 79 %, тиск 1017 мбар і падає.\n- **Видимість:** 24 км, хмарність 100 %.\n- **Повітря:** якість середня, для більшості людей прийнятна.\n\nПопереджень про негоду немає. Якщо виходите, візьміть куртку й парасольку."
	lesyaLead   = "Я озвучив прогноз голосом Lesya. Ось той самий текст українською:\n\nУ Парижі зараз 14:04, хмарно й прохолодно (дані AccuWeather для району Нотр-Дам)."

	warmerLead = "Paris is warmer than Lyon today, by about three degrees."
	cityTable  = "| city | high |\n|---|---|\n| Paris | 17 °C |\n| Lyon | 14 °C |"
	// bareTable is cityTable without outer pipes, as GFM allows.
	bareTable = "City | High\n--- | ---\nParis | 17 °C\nLyon | 14 °C"
)

// Choose picks what of an answer may be spoken: the whole answer when it
// is short prose with no table, list or heading, else its opening
// paragraph when that is prose and short, else the whole answer when it is
// short and holds no table (Jev then decides whether it listens well); a
// fenced block anywhere rules out everything, a table the whole answer.
func TestChoose(t *testing.T) {
	const short = "It is 14 degrees and cloudy in Paris today, with light rain this evening."
	long := strings.TrimSpace(strings.Repeat("It is raining in Paris. ", 25)) // one paragraph of 600 runes
	for _, f := range []string{parisAnswer, marseilleAnswer, long} {
		if _, ok := Speakable(f); ok {
			t.Fatalf("fixture %.40q... is speakable as a whole; the cases below need it long", f)
		}
	}
	whole := func(s string) Choice { return Choice{Text: s, Markdown: s} }
	opening := func(s string) Choice { return Choice{Text: s, Markdown: s, Opening: true} }
	code := Choice{Reason: "code block or table"}
	tooLong := Choice{Reason: "long or mostly code"}
	for _, tc := range []struct {
		name string
		in   string
		want Choice
	}{
		{"short prose", short, whole(short)},
		{"short prose made speakable", "It is **14 °C** in Paris ✅", Choice{Text: "It is 14 °C in Paris.", Markdown: "It is **14 °C** in Paris ✅"}},
		{"Paris: a lead sentence, then figures", parisAnswer, opening(parisLead)},
		{"Marseille: a lead, figures and a trailing table", marseilleAnswer, opening(marseilleLead)},
		{"leading blank lines", "\n \n" + parisAnswer, opening(parisLead)},
		{"CRLF line ends", strings.ReplaceAll(parisAnswer, "\n", "\r\n"), opening(parisLead)},
		{"a two-line lead", "It's raining in Paris this morning,\nand it should stop around midday.\n\n" + parisFigures,
			Choice{Text: "It's raining in Paris this morning. and it should stop around midday.", Markdown: "It's raining in Paris this morning,\nand it should stop around midday.", Opening: true}},
		{"a bold lead is prose", "**Paris:** rain until midday, then cloudy.\n\n" + parisFigures,
			Choice{Text: "Paris: rain until midday, then cloudy.", Markdown: "**Paris:** rain until midday, then cloudy.", Opening: true}},
		{"a lead then a list, no blank line", parisLead + "\n" + parisFigures, opening(parisLead)},
		{"a lead then a table, no blank line", warmerLead + "\n" + cityTable, opening(warmerLead)},
		{"a lead then a heading, no blank line", parisLead + "\n## Details\n" + parisFigures, opening(parisLead)},
		{"a colon lead before bullets", "Here's the current weather in Paris:\n\n" + parisFigures, tooLong},
		{"a colon lead right before bullets", "Here's the current weather in Paris:\n" + parisFigures, tooLong},
		{"a colon lead before a table", "Here are today's highs:\n\n" + cityTable, code},
		{"a setext heading with =", "Paris today\n===========\n\n" + parisFigures, tooLong},
		{"a setext heading with -", "Paris today\n-----------\n\n" + parisFigures, tooLong},
		{"a two-line setext heading", "Paris today,\nand tomorrow\n---\n\n" + parisFigures, tooLong},
		{"a long answer led by a heading", "## Paris today\n\n" + parisAnswer, tooLong},
		{"led by a bullet", parisFigures + "\n\n" + parisLead, tooLong},
		{"led by a star bullet", "* " + parisLead + "\n\n" + parisFigures, tooLong},
		{"led by a plus bullet", "+ " + parisLead + "\n\n" + parisFigures, tooLong},
		{"led by a numbered item", "1. " + parisLead + "\n\n" + parisFigures, tooLong},
		{"led by a numbered item with a parenthesis", "1) " + parisLead + "\n\n" + parisFigures, tooLong},
		{"led by a horizontal rule", "---\n" + parisLead + "\n\n" + parisFigures, tooLong},
		{"led by a quote", "> " + parisLead + "\n\n" + parisFigures, tooLong},
		{"a lead too long itself", long + "\n\n" + parisFigures, tooLong},
		{"one paragraph of 600 runes", long, tooLong},
		{"a fenced block after a short lead", parisLead + "\n\n```sh\ncurl wttr.in/Paris\n```", code},
		{"a fenced block after a long answer", parisAnswer + "\n\n```sh\ncurl wttr.in/Paris\n```", code},
		{"a quoted fence", parisLead + "\n\n> ~~~\n> x\n> ~~~", code},
		{"a short answer with a table and a prose lead", warmerLead + "\n\n" + cityTable, opening(warmerLead)},
		{"a short answer that is only a table", cityTable, code},
		{"a table led by a heading", "## Highs\n\n" + cityTable, code},
		{"nothing to say", "", tooLong},

		{"a short bulleted answer with a prose lead", "Both test runs passed on the new server.\n\n- unit: 840 of 840\n- e2e: 12 of 12", opening("Both test runs passed on the new server.")},
		{"a short bulleted answer with a prose lead, no blank line", "Both test runs passed on the new server.\n- unit: 840 of 840\n- e2e: 12 of 12", opening("Both test runs passed on the new server.")},
		{"a short numbered answer with a prose lead", "Both test runs passed on the new server.\n\n1. unit\n2. e2e", opening("Both test runs passed on the new server.")},
		{"a short answer with a heading after a prose lead", "Both test runs passed on the new server.\n\n## Details\n\nAll green.", opening("Both test runs passed on the new server.")},
		{"a short all-bullets answer", "- unit tests passed\n- the shop is live again", Choice{Text: "unit tests passed. the shop is live again.", Markdown: "- unit tests passed\n- the shop is live again"}},
		{"a short answer led by a heading", "## Done\n\nAll tests passed.", Choice{Text: "Done. All tests passed.", Markdown: "## Done\n\nAll tests passed."}},
		{"a short colon lead before bullets", "Here's the weather in Paris:\n- 14 °C\n- light rain", Choice{Text: "Here's the weather in Paris: 14 °C. light rain.", Markdown: "Here's the weather in Paris:\n- 14 °C\n- light rain"}},

		{"a bold line is a heading", "**Summary**\n\n" + parisAnswer, tooLong},
		{"an underscored bold line is a heading", "__Summary__\n\n" + parisAnswer, tooLong},
		{"an italic line is a heading", "*Summary*\n\n" + parisAnswer, tooLong},
		{"an underscored italic line is a heading", "_Summary_\n" + parisAnswer, tooLong},
		{"a bold-italic line is a heading", "***Summary***\n\n" + parisAnswer, tooLong},
		{"a lead then a bold heading, no blank line", parisLead + "\n**Details**\n" + parisFigures, opening(parisLead)},
		{"a line bold only at both ends is prose", "**Paris:** rain until midday, then **cloudy**.\n\n" + parisFigures,
			Choice{Text: "Paris: rain until midday, then cloudy.", Markdown: "**Paris:** rain until midday, then **cloudy**.", Opening: true}},
		{"a short answer under a bold heading", "**Summary**\n\nAll tests passed.", Choice{Text: "Summary. All tests passed.", Markdown: "**Summary**\n\nAll tests passed."}},

		{"a table without outer pipes", bareTable, code},
		{"a table without outer pipes after a prose lead", warmerLead + "\n\n" + bareTable, opening(warmerLead)},
		{"a table without outer pipes right under a lead", warmerLead + "\nCity | High\n:--- | ---:\nParis | 17 °C", opening(warmerLead)},
		{"a pipe in prose is no table", "Run ls | wc to count the files, then report.", whole("Run ls | wc to count the files, then report.")},
		{"a table with one-dash cells", "City | High\n- | -\nParis | 17 °C", code},
		{"a table with two-dash cells", "City | High\n-- | --\nParis | 17 °C", code},
		{"a table with short aligned cells after a lead", warmerLead + "\n\nCity | High\n:-- | --:\nParis | 17 °C", opening(warmerLead)},
		{"a table with one-dash cells right under a lead", warmerLead + "\nCity | High\n- | -\nParis | 17 °C", opening(warmerLead)},

		{"a short lead-in takes the next paragraph with it", ukAnswer, Choice{Text: "Звісно, ось українською. Статусу поки немає: у цій сесії ще не було жодного завдання. До цього ви лише попросили відповісти «ready».", Markdown: ukLead, Opening: true}},
		{"an English lead-in", "Sure, here it is in Ukrainian.\n\nThere is no status yet: this session has had no task so far.\n\n" + parisFigures,
			Choice{Text: "Sure, here it is in Ukrainian. There is no status yet: this session has had no task so far.", Markdown: "Sure, here it is in Ukrainian.\n\nThere is no status yet: this session has had no task so far.", Opening: true}},
		{"a short factual opening is kept with the next paragraph", "It is 14 °C in Paris.\n\nRain should stop around midday.\n\n" + parisFigures,
			Choice{Text: "It is 14 °C in Paris. Rain should stop around midday.", Markdown: "It is 14 °C in Paris.\n\nRain should stop around midday.", Opening: true}},
		{"a first paragraph over eight words is no lead-in", parisLead + "\n\nTomorrow looks drier.\n\n" + parisFigures, opening(parisLead)},
		{"a lead-in before a colon paragraph stands alone", "Sure, here it is.\n\nHere are the figures:\n\n" + parisFigures, opening("Sure, here it is.")},
		{"a lead-in before a paragraph too long stands alone", "Sure, here it is.\n\n" + long + "\n\n" + parisFigures, opening("Sure, here it is.")},
		{"a lead-in before a table stands alone", "Sure, here it is.\n\n" + cityTable, opening("Sure, here it is.")},
		{"a lead-in before a setext heading stands alone", "Sure, here it is.\n\nParis today\n---\n\n" + parisFigures, opening("Sure, here it is.")},
		{"a lead-in before a quote stands alone", "Sure, here it is.\n\n> There is no status yet.\n\n" + parisFigures, opening("Sure, here it is.")},
		{"a lead-in with CRLF line ends", "Sure, here it is.\r\n\r\nThere is no status yet.\r\n\r\n" + parisFigures,
			Choice{Text: "Sure, here it is. There is no status yet.", Markdown: "Sure, here it is.\n\nThere is no status yet.", Opening: true}},
		{"a lead-in after leading blank lines", "\n \nSure, here it is.\n\n\nThere is no status yet.\n\n" + parisFigures,
			Choice{Text: "Sure, here it is. There is no status yet.", Markdown: "Sure, here it is.\n\nThere is no status yet.", Opening: true}},
		{"a lead-in whose next paragraph runs into a list", "Sure, here it is.\n\nThere is no status yet.\n" + parisFigures,
			Choice{Text: "Sure, here it is. There is no status yet.", Markdown: "Sure, here it is.\n\nThere is no status yet.", Opening: true}},
		{"eight words are a lead-in", "Of course, here is the answer in Ukrainian.\n\nThere is no status yet.\n\n" + parisFigures,
			Choice{Text: "Of course, here is the answer in Ukrainian. There is no status yet.", Markdown: "Of course, here is the answer in Ukrainian.\n\nThere is no status yet.", Opening: true}},
		{"nine words are not", "Of course, here is the full answer in Ukrainian.\n\nThere is no status yet.\n\n" + parisFigures, opening("Of course, here is the full answer in Ukrainian.")},
		{"a colon lead takes the prose paragraph after it", lesyaAnswer, Choice{Text: "Я озвучив прогноз голосом Lesya. Ось той самий текст українською: У Парижі зараз 14:04, хмарно й прохолодно (дані AccuWeather для району Нотр-Дам).", Markdown: lesyaLead, Opening: true}},
		{"a colon lead before a colon paragraph", "Here is what I found in the logs today:\n\nThe relay restarted twice, at these times:\n\n" + parisFigures, tooLong},
		{"a lead-in ended by a list has no next paragraph", "Sure, here it is.\n" + parisFigures + "\n\nThere is no status yet.", opening("Sure, here it is.")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Choose(tc.in); got != tc.want {
				t.Errorf("Choose(%q)\n got %+v\nwant %+v", tc.in, got, tc.want)
			}
		})
	}
}
