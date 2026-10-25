package ask

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/IZZY-Agency/claude-tg/internal/render"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

func TestPermissionMessage(t *testing.T) {
	html, plain := PermissionMessage("acme-shop-92", "Bash", "rm -rf node_modules")
	if want := "🔐 <b>acme-shop-92</b>\nneeds permission: <code>Bash: rm -rf node_modules</code>"; html != want {
		t.Errorf("html = %q, want %q", html, want)
	}
	if want := "🔐 acme-shop-92\nneeds permission: Bash: rm -rf node_modules"; plain != want {
		t.Errorf("plain = %q, want %q", plain, want)
	}

	html, plain = PermissionMessage("a<b>&", "Bash", "echo '<x>' && true")
	if want := "🔐 <b>a&lt;b&gt;&amp;</b>\nneeds permission: <code>Bash: echo '&lt;x&gt;' &amp;&amp; true</code>"; html != want {
		t.Errorf("escaped html = %q, want %q", html, want)
	}
	if want := "🔐 a<b>&\nneeds permission: Bash: echo '<x>' && true"; plain != want {
		t.Errorf("escaped plain = %q, want %q", plain, want)
	}

	if html, _ := PermissionMessage("acme-shop-92", "ExitPlanMode", ""); html != "🔐 <b>acme-shop-92</b>\nneeds permission: <code>ExitPlanMode</code>" {
		t.Errorf("no summary: html = %q", html)
	}
}

// Ruling R22: each offered suggestion is listed in full and numbered under
// the needs-permission line; its button carries only the number.
func TestPermissionMessageListsSuggestions(t *testing.T) {
	long := "Yes, always allow Read, Bash(npm run test:unit -- --watch=false), Bash(git log:*) (this project, shared)"
	sugg := []Suggestion{
		{Index: 1, Label: long},
		{Index: 3, Label: "Yes, allow access to …/a<b>&c for this session", Session: true},
	}
	html, plain := PermissionMessage("acme-shop-92", "Bash", "npm test", sugg...)
	wantHTML := "🔐 <b>acme-shop-92</b>\nneeds permission: <code>Bash: npm test</code>\n" +
		"① " + render.EscapeHTML(long) + "\n" +
		"② Yes, allow access to …/a&lt;b&gt;&amp;c for this session"
	if html != wantHTML {
		t.Errorf("html = %q\nwant %q", html, wantHTML)
	}
	wantPlain := "🔐 acme-shop-92\nneeds permission: Bash: npm test\n" +
		"① " + long + "\n" +
		"② Yes, allow access to …/a<b>&c for this session"
	if plain != wantPlain {
		t.Errorf("plain = %q\nwant %q", plain, wantPlain)
	}
}

// Labels are clipped only when the list would not fit in one message.
func TestPermissionMessageFitsWithHugeSuggestions(t *testing.T) {
	var sugg []Suggestion
	for i := 0; i < 3; i++ {
		sugg = append(sugg, Suggestion{Index: i, Label: "Yes, always allow Bash(" + strings.Repeat("&", 3000) + ") (all projects)"})
	}
	html, plain := PermissionMessage("n", "Bash", "x", sugg...)
	if n := utf8.RuneCountInString(plain); n > 4000 {
		t.Fatalf("visible length %d > 4000", n)
	}
	if n := utf8.RuneCountInString(render.StripHTML(html)); n > 4000 {
		t.Fatalf("html visible length %d > 4000", n)
	}
	checkHTML(t, strings.NewReplacer("<code>", "", "</code>", "").Replace(html))
	for i, mark := range []string{"①", "②", "③"} {
		if !strings.Contains(plain, "\n"+mark+" Yes, always allow Bash(&&&") || !strings.Contains(plain, "…") {
			t.Fatalf("suggestion %d missing or unclipped: %q", i, plain)
		}
	}
}

func TestPermissionKeyboard(t *testing.T) {
	sugg := []Suggestion{
		{Index: 0, Label: "Yes, allow access to …/back for this session", Session: true},
		{Index: 3, Label: "Yes, always allow " + strings.Repeat("x", 100) + " (all projects)"},
	}
	kb := PermissionKeyboard("0123456789abcdef", sugg)
	want := [][]telegram.Button{
		{{Text: "✅ Yes", Data: "a:0123456789abcdef:0:y"}, {Text: "❌ No", Data: "a:0123456789abcdef:0:n"}},
		{{Text: "①  Yes, this session", Data: "a:0123456789abcdef:0:s0"}},
		{{Text: "②  Yes, always", Data: "a:0123456789abcdef:0:s3"}},
	}
	if !reflect.DeepEqual(kb, want) {
		t.Fatalf("PermissionKeyboard = %+v\nwant %+v", kb, want)
	}
}

func TestSuggestionNumbers(t *testing.T) {
	for n, want := range map[int]string{1: "①", 2: "②", 10: "⑩", 20: "⑳", 21: "(21)"} {
		if got := circled(n); got != want {
			t.Errorf("circled(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestQuestionKeyboard(t *testing.T) {
	q := Question{Question: "Which colours?", MultiSelect: true, Options: []Option{{Label: "Red"}, {Label: "Green"}, {Label: strings.Repeat("é", 70)}}}
	kb := QuestionKeyboard("0123456789abcdef", 1, q, []bool{true, false, true})
	want := [][]telegram.Button{
		{{Text: "✓ Red", Data: "a:0123456789abcdef:1:o0"}},
		{{Text: "Green", Data: "a:0123456789abcdef:1:o1"}},
		{{Text: "✓ " + strings.Repeat("é", 57) + "…", Data: "a:0123456789abcdef:1:o2"}},
		{{Text: "Done", Data: "a:0123456789abcdef:1:d"}},
		{{Text: "✍️ Other", Data: "a:0123456789abcdef:1:x"}},
	}
	if !reflect.DeepEqual(kb, want) {
		t.Fatalf("multi-select keyboard = %+v\nwant %+v", kb, want)
	}

	q.MultiSelect = false
	kb = QuestionKeyboard("0123456789abcdef", 0, q, nil)
	want = [][]telegram.Button{
		{{Text: "Red", Data: "a:0123456789abcdef:0:o0"}},
		{{Text: "Green", Data: "a:0123456789abcdef:0:o1"}},
		{{Text: strings.Repeat("é", 59) + "…", Data: "a:0123456789abcdef:0:o2"}},
		{{Text: "✍️ Other", Data: "a:0123456789abcdef:0:x"}},
	}
	if !reflect.DeepEqual(kb, want) {
		t.Fatalf("single-choice keyboard = %+v\nwant %+v", kb, want)
	}
}

func TestQuestionMessageHeaderCounts(t *testing.T) {
	q := Question{Question: "Which address?", Header: "Email", Options: []Option{
		{Label: "support@example.com", Description: "already the support address"},
		{Label: "sbozhko@izzy.agency", Preview: "To: you"},
	}}
	html, plain := QuestionMessage("shop-92", q, 2, 3)
	wantHTML := "✋ <b>shop-92</b> · <b>Email</b> <i>(2/3)</i>\n" +
		"Which address?\n" +
		"• <b>support@example.com</b> — already the support address\n" +
		"• <b>sbozhko@izzy.agency</b>\n<pre>To: you</pre>"
	if html != wantHTML {
		t.Errorf("html = %q\nwant %q", html, wantHTML)
	}
	wantPlain := "✋ shop-92 · Email (2/3)\n" +
		"Which address?\n" +
		"• support@example.com — already the support address\n" +
		"• sbozhko@izzy.agency\nTo: you"
	if plain != wantPlain {
		t.Errorf("plain = %q\nwant %q", plain, wantPlain)
	}

	html, plain = QuestionMessage("shop-92", q, 1, 1)
	if first := strings.SplitN(html, "\n", 2)[0]; first != "✋ <b>shop-92</b> · <b>Email</b>" {
		t.Errorf("single question header = %q", first)
	}
	if first := strings.SplitN(plain, "\n", 2)[0]; first != "✋ shop-92 · Email" {
		t.Errorf("single question plain header = %q", first)
	}

	q.Header = ""
	if html, _ := QuestionMessage("shop-92", q, 1, 2); !strings.HasPrefix(html, "✋ <b>shop-92</b> <i>(1/2)</i>\n") {
		t.Errorf("no header: %q", html)
	}
}

// checkHTML fails when html has a reserved character outside the tags and
// entities tg emits.
func checkHTML(t *testing.T, html string) {
	t.Helper()
	bare := strings.NewReplacer("<b>", "", "</b>", "", "<i>", "", "</i>", "", "<pre>", "", "</pre>", "",
		"&amp;", "", "&lt;", "", "&gt;", "").Replace(html)
	if strings.ContainsAny(bare, "<>&") {
		t.Fatalf("unescaped reserved character in %q", html)
	}
}

func TestQuestionMessageEscapesAndFits(t *testing.T) {
	const s = "<b>&"
	// Question and descriptions longer than their clip limits, but the
	// message fits once the previews are gone.
	question, description := s+" which one? "+s+strings.Repeat("q", 1500), s+" description"+strings.Repeat("d", 300)
	q := Question{Question: question, Header: s}
	for i := 0; i < 4; i++ {
		q.Options = append(q.Options, Option{Label: s + " label", Description: description, Preview: s + strings.Repeat("p", 3000)})
	}
	html, plain := QuestionMessage(s, q, 1, 2)
	if n := utf8.RuneCountInString(plain); n > 4000 {
		t.Fatalf("visible length %d > 4000", n)
	}
	if n := utf8.RuneCountInString(html); n > 4000 {
		t.Fatalf("html length %d > 4000", n)
	}
	// No raw "<b>&" from the input: every reserved character outside tg's
	// own tags is an entity.
	checkHTML(t, html)
	// Previews go first; descriptions and the question stay whole.
	if strings.Contains(html, "<pre>") || strings.Contains(plain, "ppp") {
		t.Fatalf("previews kept: %q", html)
	}
	if !strings.Contains(html, "\n"+render.EscapeHTML(question)+"\n") || strings.Count(html, "— "+render.EscapeHTML(description)+"\n") != 3 ||
		!strings.HasSuffix(html, "— "+render.EscapeHTML(description)) || strings.Contains(html, "…") {
		t.Fatalf("question or descriptions clipped while dropping previews was enough: %q", html)
	}
	if want := "✋ <b>&lt;b&gt;&amp;</b> · <b>&lt;b&gt;&amp;</b> <i>(1/2)</i>\n"; !strings.HasPrefix(html, want) {
		t.Fatalf("header = %q, want prefix %q", html, want)
	}

	// Long descriptions are clipped to 200 runes next; the question stays.
	q.Question = s + " which one? " + s
	for i := range q.Options {
		q.Options[i].Description = s + strings.Repeat("d", 3000)
	}
	html, plain = QuestionMessage("n", q, 1, 2)
	if n := utf8.RuneCountInString(plain); n > 4000 {
		t.Fatalf("visible length %d > 4000 with long descriptions", n)
	}
	checkHTML(t, html)
	wantDesc := "— " + s + strings.Repeat("d", 200-len(s)-1) + "…"
	if strings.Count(plain, wantDesc) != 4 || !strings.Contains(plain, s+" which one? "+s+"\n") {
		t.Fatalf("descriptions not clipped to 200 runes (question kept): %q", plain)
	}

	// A long question is clipped to 1000 runes last.
	q.Question = strings.Repeat("q", 5000)
	html, plain = QuestionMessage("n", q, 1, 2)
	if n := utf8.RuneCountInString(plain); n > 4000 {
		t.Fatalf("visible length %d > 4000 with a long question", n)
	}
	checkHTML(t, html)
	if !strings.Contains(plain, "\n"+strings.Repeat("q", 999)+"…\n") {
		t.Fatalf("question not clipped to 1000 runes: %q", plain[:200])
	}
}

func TestWithOutcome(t *testing.T) {
	if got, want := WithOutcome("🔐 <b>x</b>", "❌ denied from the phone: a<b>"), "🔐 <b>x</b>\n\n<i>❌ denied from the phone: a&lt;b&gt;</i>"; got != want {
		t.Fatalf("WithOutcome = %q, want %q", got, want)
	}
}
