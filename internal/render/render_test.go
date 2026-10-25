package render

import (
	"strings"
	"testing"
)

func TestMarkdownLiteBoldCodeFence(t *testing.T) {
	in := "**ok** `x<y` \n```\na&b\n```"
	want := "<b>ok</b> <code>x&lt;y</code> \n<pre>a&amp;b</pre>"
	if got := MarkdownLite(in); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestHeadingBecomesBold(t *testing.T) {
	if got := MarkdownLite("## Title <1>\nbody"); got != "<b>Title &lt;1&gt;</b>\nbody" {
		t.Fatalf("got %q", got)
	}
}

func TestUnclosedFenceIsClosed(t *testing.T) {
	got := MarkdownLite("```\nx := 1")
	if got != "<pre>x := 1</pre>" {
		t.Fatalf("got %q", got)
	}
}

func TestChunkSplitsOnBlankLines(t *testing.T) {
	p := strings.Repeat("a", 2000)
	chunks := Chunk(p+"\n\n"+p, 3500)
	if len(chunks) != 2 || chunks[0] != p || chunks[1] != p {
		t.Fatalf("got %d chunks, lens %v", len(chunks), lens(chunks))
	}
}

func TestChunkHardSplitsLongLine(t *testing.T) {
	chunks := Chunk(strings.Repeat("b", 8000), 3500)
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks", len(chunks))
	}
	for _, c := range chunks {
		if len([]rune(c)) > 3500 {
			t.Fatalf("chunk too long: %d", len([]rune(c)))
		}
	}
	if strings.Join(chunks, "") != strings.Repeat("b", 8000) {
		t.Fatal("hard split lost or duplicated characters")
	}
}

func TestChunkReopensFence(t *testing.T) {
	body := strings.Repeat("line of code\n", 400) // ~5200 chars
	chunks := Chunk("intro\n\n```\n"+body+"```\nafter", 3500)
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks", len(chunks))
	}
	if !strings.HasSuffix(strings.TrimRight(chunks[0], "\n"), "```") {
		t.Fatalf("first chunk does not close the fence: %q", tail(chunks[0]))
	}
	if !strings.HasPrefix(chunks[1], "```") {
		t.Fatalf("second chunk does not reopen the fence: %q", head(chunks[1]))
	}
}

func TestTruncateAppendsLine(t *testing.T) {
	chunks := []string{"1", "2", "3", "4", "55555", "666"}
	got := Truncate(chunks, 4)
	if len(got) != 4 {
		t.Fatalf("got %d chunks", len(got))
	}
	if !strings.HasSuffix(got[3], "… truncated, 8 more characters") {
		t.Fatalf("last chunk %q", got[3])
	}
}

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"ghp_abcdefghijklmnopqrstuvwxyz0123": "[redacted]",
		"glpat-abcdefghijklmnopqrst":         "[redacted]",
		"sk-abcdefghijklmnopqrstuvwxyz":      "[redacted]",
		"xoxb-1234567890-abcdefghij":         "[redacted]",
		"AKIAABCDEFGHIJKLMNOP":               "[redacted]",
		"token=supersecretvalue123":          "token=[redacted]",
		"PASSWORD=hunter2hunter2":            "PASSWORD=[redacted]",
		"token=abc":                          "token=abc",
		// Raw text: the whole value goes, "<" included (Ruling R23).
		"mysql --password=s3cr3t<dump.sql":           "mysql --password=[redacted]",
		"deploy ok, see https://git.example.com/x/1": "deploy ok, see https://git.example.com/x/1",
	}
	for in, want := range cases {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderedChunkUnder4096(t *testing.T) {
	// Telegram's limit is on visible text after entity parsing, so the
	// source chunk length is what must stay under 4096; escaping may grow
	// the wire payload but must leave no raw reserved characters.
	src := strings.Repeat("&<>", 1200) + "\n\n" + strings.Repeat("x", 20000)
	for _, c := range Chunk(src, SourceChunk) {
		if n := len([]rune(c)); n > 4096 {
			t.Fatalf("visible chunk %d > 4096", n)
		}
		html := MarkdownLite(c)
		if strings.ContainsAny(strings.NewReplacer("&amp;", "", "&lt;", "", "&gt;", "").Replace(html), "&<>") {
			t.Fatalf("unescaped reserved character in %q", head(html))
		}
	}
}

func lens(ss []string) []int {
	out := make([]int, len(ss))
	for i, s := range ss {
		out[i] = len(s)
	}
	return out
}
func tail(s string) string {
	if len(s) > 20 {
		return s[len(s)-20:]
	}
	return s
}
func head(s string) string {
	if len(s) > 20 {
		return s[:20]
	}
	return s
}

func TestItalicAndBoldItalic(t *testing.T) {
	cases := map[string]string{
		"a *word* here":         "a <i>word</i> here",
		"a _word_ here.":        "a <i>word</i> here.",
		"snake_case_name stays": "snake_case_name stays",
		"2 * 3 * 4":             "2 * 3 * 4",
		"**bold** and *it*":     "<b>bold</b> and <i>it</i>",
		"path/to/file_name.txt": "path/to/file_name.txt",
	}
	for in, want := range cases {
		if got := MarkdownLite(in); got != want {
			t.Errorf("MarkdownLite(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestListsAndTasks(t *testing.T) {
	in := "- first\n* second <x>\n  - nested\n- [ ] todo\n- [x] done\n1. numbered"
	want := "• first\n• second &lt;x&gt;\n  ◦ nested\n☐ todo\n☑ done\n1. numbered"
	if got := MarkdownLite(in); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestLinks(t *testing.T) {
	in := "see [the MR](https://git.example.com/g/p/-/merge_requests/1?x=1&y=2) and **[bold link](https://x.y)**"
	want := `see <a href="https://git.example.com/g/p/-/merge_requests/1?x=1&amp;y=2">the MR</a> and <b><a href="https://x.y">bold link</a></b>`
	if got := MarkdownLite(in); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestBlockquoteAndRule(t *testing.T) {
	in := "> quoted & more\n> second line\n\n---\nafter"
	want := "<blockquote>quoted &amp; more\nsecond line</blockquote>\n\n───\nafter"
	if got := MarkdownLite(in); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestFenceWithLanguage(t *testing.T) {
	if got := MarkdownLite("```go\nx := 1\n```"); got != `<pre><code class="language-go">x := 1</code></pre>` {
		t.Fatalf("got %q", got)
	}
}

func TestTableBecomesPre(t *testing.T) {
	in := "intro\n| a | b |\n|---|---|\n| 1 | <2> |\nafter"
	want := "intro\n<pre>| a | b |\n|---|---|\n| 1 | &lt;2&gt; |</pre>\nafter"
	if got := MarkdownLite(in); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

// Attribute values need their quotes escaped.
func TestLinkHrefEscapesQuotes(t *testing.T) {
	got := MarkdownLite(`[x](https://a.b/?q="y")`)
	if got != `<a href="https://a.b/?q=&quot;y&quot;">x</a>` {
		t.Fatalf("got %q", got)
	}
}

// Fence state must carry across more than two chunks.
func TestChunkFenceAcrossThreeChunks(t *testing.T) {
	body := strings.Repeat("0123456789abcdef\n", 500) // ~8500 chars
	chunks := Chunk("```\n"+body+"```", 3500)
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks", len(chunks))
	}
	for i, c := range chunks {
		if i > 0 && !strings.HasPrefix(c, "```") {
			t.Fatalf("chunk %d does not reopen the fence: %q", i, head(c))
		}
		if !strings.HasSuffix(strings.TrimRight(c, "\n"), "```") {
			t.Fatalf("chunk %d does not close the fence: %q", i, tail(c))
		}
		if strings.Count(c, "```")%2 != 0 {
			t.Fatalf("chunk %d has unbalanced fences", i)
		}
	}
}

// Rendering must not grow the visible text beyond the source.
func TestRuleKeepsLength(t *testing.T) {
	if got := MarkdownLite("---"); len([]rune(got)) != 3 {
		t.Fatalf("rule rendered as %q (%d runes)", got, len([]rune(got)))
	}
}

// Minor from review: emphasis must nest instead of crossing tags.
func TestEmphasisNesting(t *testing.T) {
	cases := map[string]string{
		"**bold *italic***":    "<b>bold <i>italic</i></b>",
		"***a**b*":             "<i><b>a</b>b</i>",
		"***both***":           "<i><b>both</b></i>",
		"*a **b** c*":          "<i>a <b>b</b> c</i>",
		"**unclosed":           "**unclosed",
		"a * b * c":            "a * b * c",
		"**x** and **y**":      "<b>x</b> and <b>y</b>",
		"file_name and _it_":   "file_name and <i>it</i>",
		"**[l](https://x.y)**": `<b><a href="https://x.y">l</a></b>`,
	}
	for in, want := range cases {
		if got := MarkdownLite(in); got != want {
			t.Errorf("MarkdownLite(%q) = %q, want %q", in, got, want)
		}
	}
}

// Emphasis must not touch link destinations.
func TestEmphasisLeavesLinkHrefAlone(t *testing.T) {
	got := MarkdownLite("[doc](https://example.com/_index_) and *it*")
	want := `<a href="https://example.com/_index_">doc</a> and <i>it</i>`
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

// RedactHTML runs on finished HTML: it masks the text between tags and
// never swallows a tag.
func TestRedactHTMLKeepsTags(t *testing.T) {
	cases := map[string]string{
		"<b>key=abcdefgh123</b>": "<b>key=[redacted]</b>",
		"🔐 <b>key=abcdefgh12</b>\n\n<i>→ password=hunter2hunter2</i> sk-0123456789abcdefXYZ": "🔐 <b>key=[redacted]</b>\n\n<i>→ password=[redacted]</i> [redacted]",
		"<code>Bash: mysql --password=s3cr3t&lt;dump.sql</code>":                             "<code>Bash: mysql --password=[redacted]</code>",
		"no tags token=supersecretvalue123":                                                  "no tags token=[redacted]",
		"<pre>ok</pre>":                                                                      "<pre>ok</pre>",
	}
	for in, want := range cases {
		if got := RedactHTML(in); got != want {
			t.Errorf("RedactHTML(%q) = %q, want %q", in, got, want)
		}
	}
	// Plain Redact on the same HTML would eat the closing tag.
	if got := Redact("<b>key=abcdefgh123</b>"); got != "<b>key=[redacted]" {
		t.Errorf("Redact on HTML = %q", got)
	}
}

// StripHTML is the plain-text form of tg's HTML for a fallback edit.
func TestStripHTML(t *testing.T) {
	in := "🔐 <b>a&lt;b&gt;&amp;</b>\nneeds permission: <code>Bash: echo '&lt;x&gt;'</code>\n\n<i>→ ok</i>"
	if got, want := StripHTML(in), "🔐 a<b>&\nneeds permission: Bash: echo '<x>'\n\n→ ok"; got != want {
		t.Fatalf("StripHTML = %q, want %q", got, want)
	}
}
