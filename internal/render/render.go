// Package render turns Claude's markdown-flavoured text into the HTML subset
// Telegram accepts, splits it into messages, and redacts secrets.
package render

import (
	"regexp"
	"strings"
	"unicode"
)

// EscapeHTML escapes the three characters Telegram's HTML mode reserves.
func EscapeHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

var (
	linkRe    = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^\s)]+)\)`)
	headingRe = regexp.MustCompile(`^#{1,6}\s+(.*)$`)
	taskRe    = regexp.MustCompile(`^(\s*)[-*+]\s+\[( |x|X)\]\s+(.*)$`)
	bulletRe  = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	ruleRe    = regexp.MustCompile(`^(-{3,}|\*{3,}|_{3,})\s*$`)
	quoteRe   = regexp.MustCompile(`^>\s?(.*)$`)
)

// MarkdownLite renders a light markdown subset to Telegram HTML: **bold**,
// *italic*, `code`, fenced blocks (with language), # headings, bullet and
// task lists, [links](url), > quotes, tables (monospace) and --- rules.
// Everything else is escaped plain text. An unclosed fence is closed.
func MarkdownLite(src string) string {
	var out []string
	var fence, quote, table []string
	inFence, fenceLang := false, ""

	flushQuote := func() {
		if len(quote) > 0 {
			out = append(out, "<blockquote>"+strings.Join(quote, "\n")+"</blockquote>")
			quote = nil
		}
	}
	flushTable := func() {
		if len(table) > 0 {
			out = append(out, "<pre>"+EscapeHTML(strings.Join(table, "\n"))+"</pre>")
			table = nil
		}
	}
	closeFence := func() {
		body := EscapeHTML(strings.Join(fence, "\n"))
		if fenceLang != "" {
			out = append(out, `<pre><code class="language-`+EscapeHTML(fenceLang)+`">`+body+"</code></pre>")
		} else {
			out = append(out, "<pre>"+body+"</pre>")
		}
		fence, inFence, fenceLang = nil, false, ""
	}

	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(line, "```") {
			if inFence {
				closeFence()
			} else {
				flushQuote()
				flushTable()
				inFence = true
				fenceLang = strings.TrimSpace(strings.TrimPrefix(line, "```"))
			}
			continue
		}
		if inFence {
			fence = append(fence, line)
			continue
		}
		if m := quoteRe.FindStringSubmatch(line); m != nil {
			flushTable()
			quote = append(quote, inline(m[1]))
			continue
		}
		flushQuote()
		if strings.HasPrefix(strings.TrimSpace(line), "|") {
			table = append(table, line)
			continue
		}
		flushTable()
		switch {
		case ruleRe.MatchString(line):
			out = append(out, "───") // same length as the source rule
		case headingRe.MatchString(line):
			out = append(out, "<b>"+inline(headingRe.FindStringSubmatch(line)[1])+"</b>")
		case taskRe.MatchString(line):
			m := taskRe.FindStringSubmatch(line)
			box := "☐"
			if m[2] != " " {
				box = "☑"
			}
			out = append(out, m[1]+box+" "+inline(m[3]))
		case bulletRe.MatchString(line):
			m := bulletRe.FindStringSubmatch(line)
			dot := "•"
			if len(m[1]) >= 2 {
				dot = "◦"
			}
			out = append(out, m[1]+dot+" "+inline(m[2]))
		default:
			out = append(out, inline(line))
		}
	}
	if inFence {
		closeFence()
	}
	flushQuote()
	flushTable()
	return strings.Join(out, "\n")
}

// inline handles `code`, links and emphasis inside one line.
func inline(line string) string {
	parts := strings.Split(line, "`")
	if len(parts)%2 == 0 {
		return textWithLinks(line) // unmatched backtick: plain text
	}
	var b strings.Builder
	for i, p := range parts {
		if i%2 == 1 {
			b.WriteString("<code>" + EscapeHTML(p) + "</code>")
		} else {
			b.WriteString(textWithLinks(p))
		}
	}
	return b.String()
}

// textWithLinks escapes text, turns [text](url) into anchors and applies
// emphasis to everything except the link destinations, which stay opaque.
func textWithLinks(s string) string {
	var b strings.Builder
	var hrefs []string
	last := 0
	for _, m := range linkRe.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(EscapeHTML(s[last:m[0]]))
		text, url := s[m[2]:m[3]], s[m[4]:m[5]]
		hrefs = append(hrefs, strings.ReplaceAll(EscapeHTML(url), `"`, "&quot;"))
		b.WriteString(`<a href="` + hrefSlot + `">` + EscapeHTML(text) + "</a>")
		last = m[1]
	}
	b.WriteString(EscapeHTML(s[last:]))
	out := emphasis(b.String())
	for _, h := range hrefs {
		out = strings.Replace(out, hrefSlot, h, 1)
	}
	return out
}

// hrefSlot stands in for a link destination while emphasis runs; it contains
// no delimiter characters, so emphasis never touches it.
const hrefSlot = "\x00href\x00"

// emphasis applies *italic*, **bold**, ***both*** and _italic_ to escaped
// text with a small delimiter-run matcher, so nested emphasis closes in the
// right order instead of producing crossed tags.
func emphasis(s string) string {
	type run struct {
		pos, n            int
		ch                rune
		canOpen, canClose bool
		open, close       []string // tags decided while pairing
		rem               int      // unpaired delimiters
	}
	rs := []rune(s)
	var runs []*run
	for i := 0; i < len(rs); {
		c := rs[i]
		if c != '*' && c != '_' {
			i++
			continue
		}
		j := i
		for j < len(rs) && rs[j] == c {
			j++
		}
		prev, next := ' ', ' '
		if i > 0 {
			prev = rs[i-1]
		}
		if j < len(rs) {
			next = rs[j]
		}
		left := !unicode.IsSpace(next) && (!isPunct(next) || unicode.IsSpace(prev) || isPunct(prev))
		right := !unicode.IsSpace(prev) && (!isPunct(prev) || unicode.IsSpace(next) || isPunct(next))
		r := &run{pos: i, n: j - i, ch: c, canOpen: left, canClose: right, rem: j - i}
		if c == '_' { // underscores must not open or close inside a word
			r.canOpen = left && (!right || isPunct(prev))
			r.canClose = right && (!left || isPunct(next))
		}
		runs = append(runs, r)
		i = j
	}
	var stack []*run
	for _, r := range runs {
		if r.canClose {
			for r.rem > 0 && len(stack) > 0 {
				top := stack[len(stack)-1]
				if top.ch != r.ch {
					break
				}
				tag := "i"
				use := 1
				if r.rem >= 2 && top.rem >= 2 {
					tag, use = "b", 2
				}
				top.open = append(top.open, "<"+tag+">")
				r.close = append(r.close, "</"+tag+">")
				top.rem -= use
				r.rem -= use
				if top.rem == 0 {
					stack = stack[:len(stack)-1]
				}
			}
		}
		if r.rem > 0 && r.canOpen {
			stack = append(stack, r)
		}
	}
	var b strings.Builder
	last := 0
	for _, r := range runs {
		b.WriteString(string(rs[last:r.pos]))
		b.WriteString(strings.Join(r.close, ""))
		if r.rem > 0 && !(r.canOpen && len(r.open) > 0) {
			b.WriteString(strings.Repeat(string(r.ch), r.rem))
		}
		for i := len(r.open) - 1; i >= 0; i-- { // outermost first
			b.WriteString(r.open[i])
		}
		if r.rem > 0 && r.canOpen && len(r.open) > 0 {
			b.WriteString(strings.Repeat(string(r.ch), r.rem))
		}
		last = r.pos + r.n
	}
	b.WriteString(string(rs[last:]))
	return b.String()
}

func isPunct(r rune) bool { return unicode.IsPunct(r) || unicode.IsSymbol(r) }
