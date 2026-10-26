package transcript

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"time"
)

const (
	// promptWindow bounds how much of a transcript LastPrompt and
	// PromptBefore read, from its end, as firstScanWindow bounds the
	// waiting hook's first scan.
	promptWindow = 8 << 20
	// promptChunk is how much they read at a time.
	promptChunk = 64 << 10
)

// Prompt is a prompt Claude Code wrote to the transcript: its text as
// submitted (a relay-typed one keeps the leading space it was typed with)
// and the entry's timestamp (zero when it has none that parses).
// Unfinished is set, with no prompt found, when the transcript's last line
// is still being written (LastPrompt, PromptBefore). Pending is set, with
// no prompt found, when PromptBefore finds no entry with the answer in the
// window it reads: Claude Code 2.1.310 runs the Stop hook before it writes
// the turn's entries, so the answer may still be on its way. An answer
// older than the window is Pending too; it never comes, and a caller that
// reads again must bound how long it does.
type Prompt struct {
	Text       string
	At         time.Time
	Unfinished bool
	Pending    bool
}

// LastPrompt returns the prompt of the transcript's current turn: the
// newest "user" entry or queued_command attachment (text sent to a busy
// session that Claude Code injected into the running turn: queuedPrompt),
// stepping over those that are not the owner's prompts: tool results
// (content holding only tool_result parts), isMeta entries, auto-compact
// summaries, queued commands from a background task or another agent, and
// the <task-notification> entries Claude Code adds when a background task
// ends. These continue the work an earlier prompt started. Any other one
// is the last prompt, whatever it says, so that a slash command or an
// "[Request interrupted by user]" typed at the Mac counts as the Mac's:
// its text is the string content, or the first text part of a list (""
// when it has none). Lines that are not JSON are skipped.
//
// A file whose last byte is not a newline ends in a line Claude Code is
// still writing: the newest entry is unknown, and may be a prompt typed at
// the Mac, so no prompt is reported (ruling P13), p.Unfinished set: an
// older phone prompt must not stand in for it.
//
// The file is read backwards from its end, at most promptWindow bytes;
// ok is false, with no error, when no prompt is found there. A file that
// cannot be read is an error.
func LastPrompt(path string) (p Prompt, ok bool, err error) {
	unfinished, err := scanBack(path, func(b []byte) bool {
		p, ok = promptOf(b)
		return ok
	})
	switch {
	case err != nil:
		return Prompt{}, false, err
	case unfinished:
		return Prompt{Unfinished: true}, false, nil
	}
	return p, ok, nil
}

// PromptBefore returns the prompt of the turn that gave answer (the Stop
// hook's last_assistant_message): the newest assistant entry whose text,
// trimmed of whitespace, is answer trimmed (answerText), then the newest
// prompt before that entry, by LastPrompt's rules. A prompt written after
// the entry is not it: a follow-up the phone queued to the busy session
// starts its own turn as this one ends, and may be on disk before this
// turn's Stop hook reads the file.
//
// ok is false, with no error, when no entry with the answer (p.Pending
// set), or no prompt before it, is found in the window LastPrompt reads,
// and when the newest
// assistant text strictly before that prompt, past any prompts between
// (the answer that ended the turn before, whatever was queued after it),
// is the same text: which of the two turns' Stop the hook is then is
// unknown. The answer showing earlier only as passing text, in a turn
// that ended on another answer, is no ambiguity.
// A last line still being written: as LastPrompt, no prompt is reported,
// p.Unfinished set. A file that cannot be read is an error.
func PromptBefore(path, answer string) (p Prompt, ok bool, err error) {
	want := strings.TrimSpace(answer)
	if want == "" {
		return Prompt{}, false, nil
	}
	isAnswer := func(b []byte) bool {
		text, ok := answerText(b)
		return ok && strings.TrimSpace(text) == want
	}
	const (
		seekAnswer = iota // the newest entry with the answer
		seekPrompt        // the prompt before it
		turnBefore        // the newest assistant text before that prompt is not the answer
	)
	stage, ambiguous := seekAnswer, false
	unfinished, err := scanBack(path, func(b []byte) bool {
		switch stage {
		case seekAnswer:
			if isAnswer(b) {
				stage = seekPrompt
			}
		case seekPrompt:
			if q, ok := promptOf(b); ok {
				p, stage = q, turnBefore
			}
		case turnBefore:
			text, ok := answerText(b)
			if ok {
				ambiguous = strings.TrimSpace(text) == want
			}
			return ok // prompts between are passed over
		}
		return false
	})
	switch {
	case err != nil:
		return Prompt{}, false, err
	case unfinished:
		return Prompt{Unfinished: true}, false, nil
	case stage == seekAnswer:
		return Prompt{Pending: true}, false, nil
	case stage != turnBefore || ambiguous:
		return Prompt{}, false, nil
	}
	return p, true, nil
}

// scanBack calls visit with each line of the file at path, newest first,
// until visit returns true. The file is read backwards from its end in
// promptChunk reads, at most promptWindow bytes; the window's first line
// is visited only when it starts there: at the start of the file, or
// right after a newline outside the window. visit must not keep the line,
// whose bytes are reused. A file whose last byte is not a newline ends in
// a line still being written: nothing is visited, and unfinished is set.
// A file that cannot be read is an error.
func scanBack(path string, visit func(line []byte) bool) (unfinished bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	size := st.Size()
	if size > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], size-1); err != nil {
			return false, err
		}
		if last[0] != '\n' {
			return true, nil
		}
	}
	floor := max(0, size-promptWindow)
	// buf holds the bytes from base to base+n, the chunk read last; stop
	// is where the line being looked at ends (its newline, or the end of
	// the file).
	buf := make([]byte, promptChunk)
	base, n, stop := size, int64(0), size
	// line returns the line from start to stop, from buf when it lies in
	// the chunk read last, read again when it ends in a later one.
	line := func(start int64) ([]byte, error) {
		if start >= base && stop <= base+n {
			return buf[start-base : stop-base], nil
		}
		b := make([]byte, stop-start)
		_, err := f.ReadAt(b, start)
		return b, err
	}
	for base > floor {
		n = min(promptChunk, base-floor)
		base -= n
		if _, err := f.ReadAt(buf[:n], base); err != nil {
			return false, err
		}
		for j := n; ; {
			i := int64(bytes.LastIndexByte(buf[:j], '\n'))
			if i < 0 {
				break
			}
			b, err := line(base + i + 1)
			if err != nil {
				return false, err
			}
			if visit(b) {
				return false, nil
			}
			stop, j = base+i, i
		}
	}
	if floor > 0 {
		var nl [1]byte
		if _, err := f.ReadAt(nl[:], floor-1); err != nil {
			return false, err
		}
		if nl[0] != '\n' {
			return false, nil
		}
	}
	b, err := line(floor)
	if err != nil {
		return false, err
	}
	visit(b)
	return false, nil
}

// answerText returns the text of an assistant entry: its string content,
// or its text parts joined by "\n" (Claude Code writes each block of an
// answer as an entry of its own, so there is one). ok is false for any
// other line, and for an entry with no text: a tool call, say.
func answerText(b []byte) (string, bool) {
	b = bytes.TrimSpace(b)
	// A cheap test before parsing: every assistant entry names its type.
	if !bytes.Contains(b, []byte(`"assistant"`)) {
		return "", false
	}
	var e struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
	}
	if json.Unmarshal(b, &e) != nil || e.Type != "assistant" {
		return "", false
	}
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(e.Message, &m) != nil {
		return "", false
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s, true
	}
	var parts []part
	if json.Unmarshal(m.Content, &parts) != nil {
		return "", false
	}
	var texts []string
	for _, p := range parts {
		if p.Type == "text" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n"), len(texts) > 0
}

// part is one item of a list content.
type part struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// promptOf reports whether one transcript line is the last prompt
// (LastPrompt), and returns it. Only the entry's type must decode: a line
// that is not JSON, a cut one included, is skipped, while a user entry or
// queued command whose other fields have unexpected types still counts,
// erring on the side of a Mac prompt.
func promptOf(b []byte) (Prompt, bool) {
	b = bytes.TrimSpace(b)
	// A cheap test before parsing assistant entries, which can be large:
	// every user entry names its type "user", every queued command
	// "queued_command".
	if len(b) == 0 || !bytes.Contains(b, []byte(`"user"`)) && !bytes.Contains(b, []byte(`"queued_command"`)) {
		return Prompt{}, false
	}
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(b, &head) != nil {
		return Prompt{}, false
	}
	var p Prompt
	var ok bool
	switch head.Type {
	case "user":
		p, ok = userPrompt(b)
	case "attachment":
		p, ok = queuedPrompt(b)
	}
	if !ok || strings.HasPrefix(strings.TrimSpace(p.Text), "<task-notification>") {
		return Prompt{}, false
	}
	return p, true
}

// userPrompt reads a "user" entry: a prompt unless it is an isMeta entry,
// an auto-compact's summary (isCompactSummary: the compacted turn goes on,
// and a /compact typed at the Mac shows as its own command entry first)
// or a tool result. Its fields are decoded raw, so an unexpected type
// never makes it skipped.
func userPrompt(b []byte) (Prompt, bool) {
	var e struct {
		IsMeta           json.RawMessage `json:"isMeta"`
		IsCompactSummary json.RawMessage `json:"isCompactSummary"`
		Timestamp        json.RawMessage `json:"timestamp"`
		Message          json.RawMessage `json:"message"`
	}
	_ = json.Unmarshal(b, &e) // a JSON object always decodes into raw fields
	if isTrue(e.IsMeta) || isTrue(e.IsCompactSummary) {
		return Prompt{}, false
	}
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal(e.Message, &m) // no message object: a prompt with no text
	text, ok := promptText(m.Content)
	return Prompt{Text: text, At: timeOf(e.Timestamp)}, ok
}

// queuedPrompt reads an "attachment" entry. Text sent to a busy session
// may never get a user entry: Claude Code injects it into the running turn
// as a queued_command attachment, which is then the turn's last prompt.
// Its text is the attachment's prompt (a string, or a list's first text
// part) and its time the entry's timestamp, else the attachment's. Queued
// commands that are not the owner's prompts are skipped as task
// notifications are: a commandMode other than "prompt" (a background
// task's notification) or an origin kind other than "human" (another
// agent's message). One that names neither counts, erring on the side of
// a Mac prompt. Other attachments are not prompts.
func queuedPrompt(b []byte) (Prompt, bool) {
	var e struct {
		Timestamp  json.RawMessage `json:"timestamp"`
		Attachment json.RawMessage `json:"attachment"`
	}
	_ = json.Unmarshal(b, &e)
	var a struct {
		Type        json.RawMessage `json:"type"`
		CommandMode json.RawMessage `json:"commandMode"`
		Origin      json.RawMessage `json:"origin"`
		Prompt      json.RawMessage `json:"prompt"`
		Timestamp   json.RawMessage `json:"timestamp"`
	}
	if json.Unmarshal(e.Attachment, &a) != nil {
		return Prompt{}, false
	}
	if kind, _ := stringOf(a.Type); kind != "queued_command" {
		return Prompt{}, false
	}
	if mode, ok := stringOf(a.CommandMode); ok && mode != "prompt" {
		return Prompt{}, false
	}
	var o struct {
		Kind json.RawMessage `json:"kind"`
	}
	_ = json.Unmarshal(a.Origin, &o)
	if kind, ok := stringOf(o.Kind); ok && kind != "human" {
		return Prompt{}, false
	}
	text, ok := promptText(a.Prompt)
	at := timeOf(e.Timestamp)
	if at.IsZero() {
		at = timeOf(a.Timestamp)
	}
	return Prompt{Text: text, At: at}, ok
}

// isTrue reports whether a raw JSON value is true.
func isTrue(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "true" }

// stringOf returns a raw JSON value as a string, reporting whether it is
// one.
func stringOf(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// timeOf parses a raw JSON timestamp, zero when it is none.
func timeOf(raw json.RawMessage) time.Time {
	s, _ := stringOf(raw)
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// promptText returns a user entry's or queued command's text: its string
// content, or the first text part of its list content. It reports false
// for a tool result, a list of tool_result parts only; any other content
// is a prompt with no text.
func promptText(content json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s, true
	}
	var parts []part
	if json.Unmarshal(content, &parts) != nil {
		return "", true
	}
	results := 0
	for _, p := range parts {
		switch p.Type {
		case "text":
			return p.Text, true
		case "tool_result":
			results++
		}
	}
	return "", len(parts) == 0 || results < len(parts)
}
