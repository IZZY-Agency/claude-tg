package ask

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

// NoMessage is what Claude reads when the owner taps No.
const NoMessage = "The user denied this from their phone."

// ReplyPrefix precedes the owner's text when a swipe-reply denies a
// permission prompt.
const ReplyPrefix = "The user denied this from their phone and replied: "

// questionTool is the tool whose asks are questions; every other ask is a
// permission prompt.
const questionTool = "AskUserQuestion"

const (
	toastSent     = "sent"
	toastApproved = "approved"
	toastDenied   = "denied"
	toastAnswered = "already answered"
	toastGone     = "this request is gone"
	toastPick     = "pick at least one"
	toastReply    = "reply to this message with your answer"
)

// The outcome lines of a permission prompt the phone decided.
const (
	outcomeApproved = "✅ approved from the phone"
	outcomeDenied   = "❌ denied from the phone"
)

// minEcho is how much of the owner's text or a label an edit always keeps;
// past it, the echo is clipped so the edit stays within maxText.
const minEcho = 64

// Tap is a decoded button press. Action: 'o' option N, 'd' done, 'x' other,
// 'y' yes, 'n' no, 's' suggestion N (an index into the original
// permission_suggestions).
type Tap struct {
	AskID  string
	Q      int
	Action byte
	N      int
}

// TapData encodes a button as "a:<id>:<q>:<action><n>"; n is left out for
// d, x, y and n. With a 16-hex-character id it fits callback_data's 64 bytes.
func TapData(id string, q int, action byte, n int) string {
	d := "a:" + id + ":" + strconv.Itoa(q) + ":" + string(action)
	if action == 'o' || action == 's' {
		d += strconv.Itoa(n)
	}
	return d
}

// ParseTap decodes what TapData encodes and rejects anything else.
func ParseTap(data string) (Tap, bool) {
	rest, ok := strings.CutPrefix(data, "a:")
	if !ok {
		return Tap{}, false
	}
	parts := strings.Split(rest, ":")
	if len(parts) != 3 || !isAskID(parts[0]) || parts[2] == "" {
		return Tap{}, false
	}
	q, ok := number(parts[1])
	if !ok {
		return Tap{}, false
	}
	t := Tap{AskID: parts[0], Q: q, Action: parts[2][0]}
	arg := parts[2][1:]
	switch t.Action {
	case 'o', 's':
		if t.N, ok = number(arg); !ok {
			return Tap{}, false
		}
	case 'd', 'x', 'y', 'n':
		if arg != "" {
			return Tap{}, false
		}
	default:
		return Tap{}, false
	}
	return t, true
}

// isAskID reports whether s looks like state.NewAskID's output.
func isAskID(s string) bool {
	if len(s) != 16 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// number parses a non-negative decimal of at most nine digits.
func number(s string) (int, bool) {
	if s == "" || len(s) > 9 {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

// Result tells the caller how to answer a tap or reply and what to edit.
type Result struct {
	Toast        string
	Index        int                 // which message to edit
	Edit         string              // new full HTML, "" = no text edit
	Keyboard     [][]telegram.Button // with KeyboardOnly: the new keyboard
	KeyboardOnly bool
}

// ApplyTap applies a button press to the ask. It mutates a; the caller runs
// it inside Store.UpdateAsk. A tap on a closed or answered ask, or one that
// matches nothing in it, changes nothing.
func ApplyTap(a *state.Ask, t Tap) Result {
	if r, ok := settled(a, t.Q); ok {
		return r
	}
	if t.AskID != a.ID {
		return gone(t.Q)
	}
	if a.Tool == questionTool {
		return tapQuestion(a, t)
	}
	return tapPermission(a, t)
}

// ApplyReply applies a swipe-reply to message index of the ask: it answers
// a question with the text, or denies a permission prompt with it.
func ApplyReply(a *state.Ask, index int, text string) Result {
	if r, ok := settled(a, index); ok {
		return r
	}
	if a.Tool != questionTool {
		if index != 0 {
			return gone(index)
		}
		if strings.TrimSpace(text) == "" {
			return Result{Toast: toastReply, Index: index}
		}
		return decide(a, state.Decision{Behavior: "deny", Suggestion: -1, Message: ReplyPrefix + text},
			toastDenied, outcomeDenied+": ", text)
	}
	qs, err := ParseQuestions(a.ToolInput)
	if err != nil || index < 0 || index >= len(qs) {
		return gone(index)
	}
	if answered(a, index) {
		return Result{Toast: toastAnswered, Index: index}
	}
	if strings.TrimSpace(text) == "" {
		return Result{Toast: toastReply, Index: index}
	}
	if index < len(a.Ticked) {
		a.Ticked[index] = nil // free text replaces any ticks
	}
	return answer(a, len(qs), index, text, text)
}

// settled answers for an ask that no longer takes answers.
func settled(a *state.Ask, index int) (Result, bool) {
	switch {
	case a.Closed != "":
		return Result{Toast: ClosedToast(a.Closed), Index: index}, true
	case a.Done:
		return Result{Toast: toastAnswered, Index: index}, true
	}
	return Result{}, false
}

// ClosedToast is the toast for a tap on an ask closed for reason (an
// ask's Closed value).
func ClosedToast(reason string) string {
	switch reason {
	case "mac":
		return "already answered at the Mac"
	case "ended":
		return "session ended"
	}
	return "expired"
}

func gone(index int) Result { return Result{Toast: toastGone, Index: index} }

func tapQuestion(a *state.Ask, t Tap) Result {
	qs, err := ParseQuestions(a.ToolInput)
	if err != nil || t.Q < 0 || t.Q >= len(qs) {
		return gone(t.Q)
	}
	q := qs[t.Q]
	switch t.Action {
	case 'o':
		if t.N < 0 || t.N >= len(q.Options) {
			return gone(t.Q)
		}
	case 'd':
		if !q.MultiSelect {
			return gone(t.Q)
		}
	case 'x':
	default:
		return gone(t.Q)
	}
	if answered(a, t.Q) {
		return Result{Toast: toastAnswered, Index: t.Q}
	}
	switch t.Action {
	case 'x':
		return Result{Toast: toastReply, Index: t.Q}
	case 'o':
		label := q.Options[t.N].Label
		if !q.MultiSelect {
			return answer(a, len(qs), t.Q, label, label)
		}
		ticked := ticks(a, t.Q, len(q.Options))
		ticked[t.N] = !ticked[t.N]
		return Result{Index: t.Q, Keyboard: QuestionKeyboard(a.ID, t.Q, q, ticked), KeyboardOnly: true}
	}
	var labels []string // Done: the ticked labels in option order
	for i, o := range q.Options {
		if t.Q < len(a.Ticked) && i < len(a.Ticked[t.Q]) && a.Ticked[t.Q][i] {
			labels = append(labels, o.Label)
		}
	}
	if len(labels) == 0 {
		return Result{Toast: toastPick, Index: t.Q}
	}
	return answer(a, len(qs), t.Q, labels, strings.Join(labels, ", "))
}

func tapPermission(a *state.Ask, t Tap) Result {
	if t.Q != 0 {
		return gone(t.Q)
	}
	switch t.Action {
	case 'y':
		return decide(a, state.Decision{Behavior: "allow", Suggestion: -1}, toastApproved, outcomeApproved, "")
	case 'n':
		return decide(a, state.Decision{Behavior: "deny", Suggestion: -1, Message: NoMessage, Interrupt: true},
			toastDenied, outcomeDenied, "")
	case 's':
		for _, s := range ParseSuggestions(a.Suggestions) {
			if s.Index == t.N {
				return decide(a, state.Decision{Behavior: "allow", Suggestion: s.Index},
					toastApproved, outcomeApproved+": ", s.Label)
			}
		}
	}
	return gone(0)
}

// answered reports whether question q already has an answer.
func answered(a *state.Ask, q int) bool { return q < len(a.Answers) && a.Answers[q] != nil }

// ticks returns question q's tick slice, growing the ask's state to fit:
// the hook appends empty slots as it sends.
func ticks(a *state.Ask, q, options int) []bool {
	for len(a.Ticked) <= q {
		a.Ticked = append(a.Ticked, nil)
	}
	for len(a.Ticked[q]) < options {
		a.Ticked[q] = append(a.Ticked[q], false)
	}
	return a.Ticked[q]
}

// answer records v as question q's answer (of n questions) and marks the
// ask done once every question has one.
func answer(a *state.Ask, n, q int, v any, shown string) Result {
	b, _ := encode(v) // a string or a []string always encodes
	for len(a.Answers) < n {
		a.Answers = append(a.Answers, nil)
	}
	a.Answers[q] = b
	a.Done = true
	for _, r := range a.Answers[:n] {
		if r == nil {
			a.Done = false
		}
	}
	return Result{Toast: toastSent, Index: q, Edit: edited(a, q, "→ ", shown)}
}

// decide records a permission decision; any decision completes the ask.
func decide(a *state.Ask, d state.Decision, toast, prefix, text string) Result {
	a.Decision, a.Done = &d, true
	return Result{Toast: toast, Index: 0, Edit: edited(a, 0, prefix, text)}
}

// edited is message i's HTML with the outcome prefix+text appended. The
// text is clipped so the visible message stays within maxText, keeping at
// least minEcho runes (the edit then still fits Telegram's 4096).
func edited(a *state.Ask, i int, prefix, text string) string {
	base := ""
	if i < len(a.Texts) {
		base = a.Texts[i]
	}
	room := maxText - utf8.RuneCountInString(base) - utf8.RuneCountInString("\n\n"+prefix)
	return WithOutcome(base, prefix+clip(text, max(room, minEcho)))
}

// Answered is message i of a as the phone's answer left it: its sent text
// and the outcome line ApplyTap or ApplyReply put under it. ok is false
// when message i has no answer from the phone.
func Answered(a *state.Ask, i int) (html string, ok bool) {
	if a.Tool != questionTool {
		d := a.Decision
		if i != 0 || d == nil {
			return "", false
		}
		switch {
		case d.Behavior == "allow" && d.Suggestion >= 0:
			for _, s := range ParseSuggestions(a.Suggestions) {
				if s.Index == d.Suggestion {
					return edited(a, 0, outcomeApproved+": ", s.Label), true
				}
			}
			return edited(a, 0, outcomeApproved, ""), true
		case d.Behavior == "allow":
			return edited(a, 0, outcomeApproved, ""), true
		case strings.HasPrefix(d.Message, ReplyPrefix):
			return edited(a, 0, outcomeDenied+": ", strings.TrimPrefix(d.Message, ReplyPrefix)), true
		}
		return edited(a, 0, outcomeDenied, ""), true
	}
	if i < 0 || i >= len(a.Answers) || a.Answers[i] == nil {
		return "", false
	}
	var text string
	if json.Unmarshal(a.Answers[i], &text) == nil {
		return edited(a, i, "→ ", text), true
	}
	var labels []string
	if json.Unmarshal(a.Answers[i], &labels) == nil {
		return edited(a, i, "→ ", strings.Join(labels, ", ")), true
	}
	return "", false
}
