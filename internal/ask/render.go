package ask

import (
	"fmt"
	"unicode/utf8"

	"github.com/IZZY-Agency/claude-tg/internal/render"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

const (
	maxText        = 4000 // tg's cap on a message's visible text
	maxButton      = 60   // runes of button text
	maxDescription = 200  // runes of an option description, when the message must shrink
	maxQuestion    = 1000 // runes of the question, when the message must shrink
)

// header is "<icon> <b>NAME</b>" and its plain form.
func header(icon, name string) (html, plain string) {
	if name == "" {
		name = "mac"
	}
	return icon + " <b>" + render.EscapeHTML(name) + "</b>", icon + " " + name
}

// PermissionMessage renders a permission prompt: the needs-permission line,
// then each offered suggestion numbered and in full (Ruling R22), since its
// button shows only the number. Pass the suggestions PermissionKeyboard
// gets. The summary is already redacted by the caller. Labels are clipped
// only when the whole would not fit in one message.
func PermissionMessage(name, tool, summary string, sugg ...Suggestion) (html, plain string) {
	for _, max := range []int{0, maxDescription, maxButton} {
		html, plain = permissionMessage(name, tool, summary, sugg, max)
		if utf8.RuneCountInString(plain) <= maxText {
			break
		}
	}
	return html, plain
}

func permissionMessage(name, tool, summary string, sugg []Suggestion, max int) (html, plain string) {
	html, plain = header("🔐", name)
	code := tool
	if summary != "" {
		code += ": " + summary
	}
	html += "\nneeds permission: <code>" + render.EscapeHTML(code) + "</code>"
	plain += "\nneeds permission: " + code
	for i, s := range sugg {
		label := clip(s.Label, max)
		html += "\n" + circled(i+1) + " " + render.EscapeHTML(label)
		plain += "\n" + circled(i+1) + " " + label
	}
	return html, plain
}

// PermissionKeyboard is Yes and No, then one row per suggestion: its number
// in the message and how long it lasts ("①  Yes, always", "②  Yes, this
// session").
func PermissionKeyboard(id string, sugg []Suggestion) [][]telegram.Button {
	kb := [][]telegram.Button{{
		{Text: "✅ Yes", Data: TapData(id, 0, 'y', 0)},
		{Text: "❌ No", Data: TapData(id, 0, 'n', 0)},
	}}
	for i, s := range sugg {
		text := circled(i+1) + "  Yes, always"
		if s.Session {
			text = circled(i+1) + "  Yes, this session"
		}
		kb = append(kb, []telegram.Button{{Text: text, Data: TapData(id, 0, 's', s.Index)}})
	}
	return kb
}

// circled is n as a circled digit (① to ⑳), or "(n)" past twenty.
func circled(n int) string {
	if n >= 1 && n <= 20 {
		return string(rune('①' + n - 1))
	}
	return fmt.Sprintf("(%d)", n)
}

// shrink is one step of fitting a question into maxText: whether option
// previews show, and rune limits (0 = no clip).
type shrink struct {
	previews                     bool
	description, question, label int
}

// shrinks are tried in order until the message fits: drop previews, clip
// descriptions, clip the question. The last step, for input outside
// AskUserQuestion's schema only, also clips labels and the header.
var shrinks = []shrink{
	{previews: true},
	{},
	{description: maxDescription},
	{description: maxDescription, question: maxQuestion},
	{description: maxDescription, question: maxQuestion, label: maxButton},
}

// QuestionMessage renders question q, number i (1-based) of n; the count
// shows only when n > 1. For input within AskUserQuestion's schema (up to
// four options) the visible text stays within 4000 characters.
func QuestionMessage(name string, q Question, i, n int) (html, plain string) {
	for _, s := range shrinks {
		html, plain = questionMessage(name, q, i, n, s)
		if utf8.RuneCountInString(plain) <= maxText {
			break
		}
	}
	return html, plain
}

func questionMessage(name string, q Question, i, n int, s shrink) (html, plain string) {
	html, plain = header("✋", name)
	if h := clip(q.Header, s.label); h != "" {
		html += " · <b>" + render.EscapeHTML(h) + "</b>"
		plain += " · " + h
	}
	if n > 1 {
		html += fmt.Sprintf(" <i>(%d/%d)</i>", i, n)
		plain += fmt.Sprintf(" (%d/%d)", i, n)
	}
	text := clip(q.Question, s.question)
	html += "\n" + render.EscapeHTML(text)
	plain += "\n" + text
	for _, o := range q.Options {
		label := clip(o.Label, s.label)
		html += "\n• <b>" + render.EscapeHTML(label) + "</b>"
		plain += "\n• " + label
		if d := clip(o.Description, s.description); d != "" {
			html += " — " + render.EscapeHTML(d)
			plain += " — " + d
		}
		if s.previews && o.Preview != "" {
			html += "\n<pre>" + render.EscapeHTML(o.Preview) + "</pre>"
			plain += "\n" + o.Preview
		}
	}
	return html, plain
}

// QuestionKeyboard has one row per option ("✓ " when ticked), Done for a
// multi-select question, and ✍️ Other.
func QuestionKeyboard(id string, qi int, q Question, ticked []bool) [][]telegram.Button {
	var kb [][]telegram.Button
	for i, o := range q.Options {
		text := o.Label
		if i < len(ticked) && ticked[i] {
			text = "✓ " + text
		}
		kb = append(kb, []telegram.Button{{Text: clip(text, maxButton), Data: TapData(id, qi, 'o', i)}})
	}
	if q.MultiSelect {
		kb = append(kb, []telegram.Button{{Text: "Done", Data: TapData(id, qi, 'd', 0)}})
	}
	return append(kb, []telegram.Button{{Text: "✍️ Other", Data: TapData(id, qi, 'x', 0)}})
}

// WithOutcome appends an italic outcome line to a sent message's HTML.
func WithOutcome(html, outcome string) string {
	return html + "\n\n<i>" + render.EscapeHTML(outcome) + "</i>"
}

// clip shortens s to max runes, ending in "…"; max 0 leaves s whole.
func clip(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
