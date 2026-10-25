// Package ask turns AskUserQuestion calls and permission prompts into
// Telegram messages with buttons, applies the owner's taps and swipe-replies
// to the ask record, and builds the decision JSON the waiting hook returns.
// It does no I/O and no redaction: callers redact what they send.
package ask

import (
	"encoding/json"
	"errors"
	"strings"
)

// Option is one choice of a question.
type Option struct{ Label, Description, Preview string }

// Question is one AskUserQuestion question.
type Question struct {
	Question, Header string
	Options          []Option
	MultiSelect      bool
}

// object reads a JSON object's members by their exact keys. encoding/json's
// struct decoding matches keys case-insensitively and lets the last
// near-duplicate win, while Claude Code reads exact keys: tg must show and
// answer exactly what it echoes back.
func object(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("ask: not a JSON object")
	}
	return m, nil
}

// member decodes the member with the exact key into v; a missing key leaves
// v as it is.
func member(m map[string]json.RawMessage, key string, v any) error {
	if r, ok := m[key]; ok {
		return json.Unmarshal(r, v)
	}
	return nil
}

// questionsValue is the exact "questions" member of an AskUserQuestion
// input: what the phone shows, and what Output echoes back.
func questionsValue(toolInput json.RawMessage) (json.RawMessage, error) {
	in, err := object(toolInput)
	if err != nil {
		return nil, err
	}
	raw, ok := in["questions"]
	if !ok {
		return nil, errors.New("ask: no questions in the tool input")
	}
	return raw, nil
}

// ParseQuestions reads the questions of an AskUserQuestion tool input.
func ParseQuestions(toolInput json.RawMessage) ([]Question, error) {
	raw, err := questionsValue(toolInput)
	if err != nil {
		return nil, err
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, errors.New("ask: no questions in the tool input")
	}
	qs := make([]Question, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, r := range list {
		q, err := parseQuestion(r)
		if err != nil {
			return nil, err
		}
		if seen[q.Question] {
			return nil, ErrDuplicateQuestion
		}
		seen[q.Question] = true
		qs = append(qs, q)
	}
	return qs, nil
}

// ErrDuplicateQuestion is ParseQuestions' error for two questions with one
// text: Claude Code keys the answers by question text, so the phone could
// never answer both. The waiting hook then sends no buttons (the plain
// alert goes instead) and Output gives no decision.
var ErrDuplicateQuestion = errors.New("ask: two questions have the same text")

func parseQuestion(raw json.RawMessage) (Question, error) {
	var q Question
	var options []json.RawMessage
	m, err := object(raw)
	if err != nil {
		return q, err
	}
	if err := errors.Join(member(m, "question", &q.Question), member(m, "header", &q.Header),
		member(m, "multiSelect", &q.MultiSelect), member(m, "options", &options)); err != nil {
		return q, err
	}
	for _, r := range options {
		o, err := parseOption(r)
		if err != nil {
			return q, err
		}
		q.Options = append(q.Options, o)
	}
	return q, nil
}

func parseOption(raw json.RawMessage) (Option, error) {
	var o Option
	m, err := object(raw)
	if err == nil {
		err = errors.Join(member(m, "label", &o.Label), member(m, "description", &o.Description),
			member(m, "preview", &o.Preview))
	}
	return o, err
}

// Suggestion is a permission suggestion tg offers as a button.
type Suggestion struct {
	Index   int    // into the original permission_suggestions array
	Label   string // what it does, in full, in the dialog's words
	Session bool   // lasts for the session only (destination "session")
}

// destinations says, in the button's words, what each destination does.
var destinations = map[string]string{
	"session":         " for this session",
	"localSettings":   " (this project, this Mac)",
	"projectSettings": " (this project, shared)",
	"userSettings":    " (all projects)",
}

// ParseSuggestions returns the offerable permission suggestions, in order:
// allow rules and added directories with a known destination, read by exact
// keys. Mode changes, removals, replacements and anything unreadable are
// left out.
func ParseSuggestions(raw json.RawMessage) []Suggestion {
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	var out []Suggestion
	for i, r := range list {
		if label, session, ok := suggestionLabel(r); ok {
			out = append(out, Suggestion{Index: i, Label: label, Session: session})
		}
	}
	return out
}

// suggestionLabel describes one suggestion and says whether it lasts for
// the session only.
func suggestionLabel(raw json.RawMessage) (label string, session, ok bool) {
	m, err := object(raw)
	if err != nil {
		return "", false, false
	}
	var typ, behavior, destination string
	var rules []json.RawMessage
	var dirs []string
	if errors.Join(member(m, "type", &typ), member(m, "behavior", &behavior), member(m, "destination", &destination),
		member(m, "rules", &rules), member(m, "directories", &dirs)) != nil {
		return "", false, false
	}
	suffix, known := destinations[destination]
	if !known {
		return "", false, false
	}
	// Ruling R8: "always" only when the change outlives the session.
	session = destination == "session"
	verb := "Yes, always allow "
	if session {
		verb = "Yes, allow "
	}
	var parts []string
	switch typ {
	case "addRules":
		if behavior != "allow" {
			return "", false, false
		}
		for _, r := range rules {
			var tool, content string
			rm, err := object(r)
			if err != nil || errors.Join(member(rm, "toolName", &tool), member(rm, "ruleContent", &content)) != nil || tool == "" {
				return "", false, false
			}
			if content == "" {
				parts = append(parts, tool)
			} else {
				parts = append(parts, tool+"("+content+")")
			}
		}
		if len(parts) == 0 {
			return "", false, false
		}
		return verb + strings.Join(parts, ", ") + suffix, session, true
	case "addDirectories":
		for _, d := range dirs {
			if d == "" {
				return "", false, false
			}
			parts = append(parts, shortDir(d))
		}
		if len(parts) == 0 {
			return "", false, false
		}
		return verb + "access to " + strings.Join(parts, ", ") + suffix, session, true
	}
	return "", false, false
}

// shortDir keeps the last two segments of a path, prefixed "…/" when it
// dropped any.
func shortDir(p string) string {
	if t := strings.TrimRight(p, "/"); t != "" {
		p = t
	}
	var segs []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	if len(segs) <= 2 {
		return p
	}
	return "…/" + strings.Join(segs[len(segs)-2:], "/")
}
