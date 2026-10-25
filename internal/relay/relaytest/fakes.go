// Package relaytest holds in-memory doubles for the relay router tests.
package relaytest

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/IZZY-Agency/claude-tg/internal/session"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
	"github.com/IZZY-Agency/claude-tg/internal/term"
)

// Reaction, Answer and Edit record feedback calls.
type Reaction struct {
	MessageID int64
	Emoji     string
}
type Answer struct{ ID, Text string }
type Edit struct {
	MessageID int64
	Text      string
	Keyboard  [][]telegram.Button
}

// API records every Telegram call the router makes.
type API struct {
	mu        sync.Mutex
	Sent      []telegram.SendRequest
	Reactions []Reaction
	Answers   []Answer
	Edits     []Edit
	ReactErr  error
	next      int64
}

func (a *API) SendMessage(_ context.Context, r telegram.SendRequest) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Sent = append(a.Sent, r)
	a.next++
	return 1000 + a.next, nil
}
func (a *API) SetMessageReaction(_ context.Context, _ int64, messageID int64, emoji string) error {
	if a.ReactErr != nil {
		return a.ReactErr
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Reactions = append(a.Reactions, Reaction{messageID, emoji})
	return nil
}
func (a *API) AnswerCallbackQuery(_ context.Context, id, text string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Answers = append(a.Answers, Answer{id, text})
	return nil
}
func (a *API) EditMessageText(_ context.Context, _ int64, messageID int64, text, _ string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Edits = append(a.Edits, Edit{MessageID: messageID, Text: text})
	return nil
}
func (a *API) EditMessage(_ context.Context, _ int64, messageID int64, text, _ string, kb [][]telegram.Button) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Edits = append(a.Edits, Edit{MessageID: messageID, Text: text, Keyboard: kb})
	return nil
}
func (a *API) EditReplyMarkup(_ context.Context, _ int64, messageID int64, kb [][]telegram.Button) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Edits = append(a.Edits, Edit{MessageID: messageID, Text: "", Keyboard: kb})
	return nil
}

// LastText returns the text of the last message sent, or "".
func (a *API) LastText() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.Sent) == 0 {
		return ""
	}
	return a.Sent[len(a.Sent)-1].Text
}

// Typed is one injection into a terminal session.
type Typed struct{ ID, Text string }

// Term is a fake terminal with an input-line model per session: typed text
// lands after the "❯ " prompt at column 0, backspace (\x7f) removes a rune,
// Enter clears the line (the prompt was submitted), Esc is ignored. A screen
// without such a prompt line (a dialog) swallows text like Claude Code does.
// DoubleNext makes the next typed text land twice, MangleNext replaces it
// with junk; both model the intermittent input glitch seen in the field.
// Screens and Missing are keyed by the pane target (agterm id or tmux pane
// id); Panes records every pane handed to the terminal, in order.
type Term struct {
	mu            sync.Mutex
	Screens       map[string]string
	Missing       map[string]bool
	Panes         []term.Pane
	TypeErr       error
	FailBackspace error // returned by Type for a pure backspace run
	Typed         []Typed
	OnType        func(id, text string)
	ScreenCalls   int
	ScreenLines   []int                                    // the lines asked of each Screen call, in order
	NextScreen    func(call int, id string) (string, bool) // override for a given call number
	DoubleNext    bool
	MangleNext    bool
	Lag           int // reads after a literal Type that show only a 5-rune prefix
	lagLeft       int
	Submitted     map[string][]string // per session, the lines submitted with Enter
	input         map[string][]rune
}

func (t *Term) Exists(_ context.Context, p term.Pane) bool {
	t.notePane(p)
	return !t.Missing[p.Target]
}

func (t *Term) notePane(p term.Pane) {
	t.mu.Lock()
	t.Panes = append(t.Panes, p)
	t.mu.Unlock()
}

func (t *Term) Type(_ context.Context, p term.Pane, text string) error {
	t.notePane(p)
	id := p.Target
	if t.TypeErr != nil {
		return t.TypeErr
	}
	if t.FailBackspace != nil && text != "" && strings.Trim(text, "\x7f") == "" {
		return t.FailBackspace
	}
	t.mu.Lock()
	t.Typed = append(t.Typed, Typed{id, text})
	if t.input == nil {
		t.input = map[string][]rune{}
	}
	if t.Submitted == nil {
		t.Submitted = map[string][]string{}
	}
	if hasPromptLine(t.Screens[id]) { // a dialog screen swallows keystrokes
		for _, r := range text {
			switch r {
			case '\x7f':
				if n := len(t.input[id]); n > 0 {
					t.input[id] = t.input[id][:n-1]
				}
			case '\n':
				t.Submitted[id] = append(t.Submitted[id], string(t.input[id]))
				t.input[id] = nil
			case '\x1b':
			default:
				t.input[id] = append(t.input[id], r)
			}
		}
		if t.DoubleNext && text != "\n" && text != "\x1b" && text != "\x7f" {
			t.input[id] = append(t.input[id], []rune(text)...)
			t.DoubleNext = false
		}
		if t.MangleNext && text != "\n" && text != "\x1b" && text != "\x7f" {
			t.input[id] = []rune("garbage")
			t.MangleNext = false
		}
		if strings.Trim(text, "\n\x1b\x7f") != "" {
			t.lagLeft = t.Lag
		}
	}
	t.mu.Unlock()
	if t.OnType != nil {
		t.OnType(id, text)
	}
	return nil
}

func hasPromptLine(screen string) bool {
	return inputLine(strings.Split(screen, "\n")) >= 0
}

// inputLine is the index of the input prompt line: the last line starting
// with "❯" that sits between the input box's borders, a line of "─" right
// above and right below it. A prompt echoed in the transcript and a
// picker's cursor line "❯ 1. …" at column 0 are not it; -1 when the screen
// has none (a dialog or picker is up).
func inputLine(lines []string) int {
	for i := len(lines) - 2; i > 0; i-- {
		if strings.HasPrefix(lines[i], "❯") && strings.HasPrefix(lines[i-1], "─") && strings.HasPrefix(lines[i+1], "─") {
			return i
		}
	}
	return -1
}

// render puts the input line into the screen, on the input prompt line.
func (t *Term) render(id string) string {
	shown := t.input[id]
	if t.lagLeft > 0 {
		t.lagLeft--
		if len(shown) > 5 {
			shown = shown[:5]
		}
	}
	lines := strings.Split(t.Screens[id], "\n")
	i := inputLine(lines)
	if i < 0 {
		return t.Screens[id]
	}
	lines[i] = "❯ " + string(shown)
	return strings.Join(lines, "\n")
}

func (t *Term) Screen(_ context.Context, p term.Pane, lines int) (string, error) {
	t.notePane(p)
	id := p.Target
	if t.Missing[id] {
		return "", errors.New("no such session")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ScreenCalls++
	t.ScreenLines = append(t.ScreenLines, lines)
	if t.NextScreen != nil {
		if s, ok := t.NextScreen(t.ScreenCalls, id); ok {
			return s, nil
		}
	}
	return t.render(id), nil
}

// Input returns the current input line of a session.
func (t *Term) Input(id string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.input[id])
}

// TypedTexts joins the typed log into one string per session id.
func (t *Term) TypedTexts(id string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var b string
	for _, x := range t.Typed {
		if x.ID == id {
			b += x.Text
		}
	}
	return b
}

// AgentsOf returns an agents lister over a fixed list.
func AgentsOf(list ...session.Agent) func(context.Context) ([]session.Agent, error) {
	return func(context.Context) ([]session.Agent, error) { return list, nil }
}
