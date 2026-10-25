// Package send composes Telegram messages for a session and delivers them
// with pacing, retries, a plain-text fallback and a spool for outages.
package send

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/render"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

// API is the part of the Telegram client the sender needs.
type API interface {
	SendMessage(ctx context.Context, r telegram.SendRequest) (int64, error)
}

// ErrSpooled means the message was stored for a later attempt.
var ErrSpooled = errors.New("message spooled for later delivery")

// ErrLost means delivery failed and the message could not be spooled either.
var ErrLost = errors.New("message lost: delivery failed and the spool could not be written")

// errThrottled means Telegram kept answering 429; the message is still valid.
var errThrottled = errors.New("telegram throttled after retries")

// Sender delivers messages to one chat.
type Sender struct {
	API     API
	Store   *state.Store
	ChatID  int64
	MinGap  time.Duration        // pacing between sends across processes
	Sleep   func(time.Duration)  // overridable for tests; nil means context-aware real waiting
	Now     func() time.Time     // overridable for tests
	Log     func(string, ...any) // optional
	Secrets []string             // strings that must never appear in a message (the bot token)
}

// Kind selects the header icon. The zero value is an alert.
type Kind int

// Message kinds, each with its own header icon.
const (
	KindAlert      Kind = iota // 🔔 tg send (default level)
	KindReport                 // 📋 tg send --level info
	KindFeed                   // 💬 follow-mode feed
	KindPermission             // 🔐 waiting on a permission prompt
	KindInput                  // ✋ background agent needs input
	KindFailure                // 💥 session stopped on an API error
	KindSystem                 // ⚙️ follow on/off, test, setup
)

var icons = map[Kind]string{
	KindAlert: "🔔", KindReport: "📋", KindFeed: "💬", KindPermission: "🔐",
	KindInput: "✋", KindFailure: "💥", KindSystem: "⚙️",
}

// Icon returns the header icon for k.
func (k Kind) Icon() string {
	if i, ok := icons[k]; ok {
		return i
	}
	return icons[KindAlert]
}

// Message is one logical message; it may become several Telegram messages.
type Message struct {
	SessionID string
	Label     string // session name shown in the header; "mac" when empty
	Title     string
	Body      string
	Silent    bool
	Markdown  bool // render Body with MarkdownLite instead of escaping it
	Kind      Kind
}

// compose builds the HTML and plain-text forms of chunk i of n. Label and
// title are clipped so the whole message stays under Telegram's limit.
func compose(m Message, chunk string, i, n int) (html, plain string) {
	label := clip(m.Label, maxLabel)
	if label == "" {
		label = "mac"
	}
	m.Title = clip(m.Title, maxTitle)
	html = m.Kind.Icon() + " <b>" + render.EscapeHTML(label) + "</b>"
	plain = m.Kind.Icon() + " " + label
	if m.Title != "" {
		html += " · <b>" + render.EscapeHTML(m.Title) + "</b>"
		plain += " · " + m.Title
	}
	if n > 1 {
		html += fmt.Sprintf(" <i>(%d/%d)</i>", i+1, n)
		plain += fmt.Sprintf(" (%d/%d)", i+1, n)
	}
	if chunk != "" {
		if m.Markdown {
			html += "\n" + render.MarkdownLite(chunk)
		} else {
			html += "\n" + render.EscapeHTML(chunk)
		}
		plain += "\n" + chunk
	}
	return html, plain
}

const (
	maxAttempts   = 3
	maxRetryAfter = 30 * time.Second
	keepIDs       = 50
	maxLabel      = 64
	maxTitle      = 200
)

// sleep waits d, or until ctx ends; it returns false when the context ended.
func (s *Sender) sleep(ctx context.Context, d time.Duration) bool {
	if s.Sleep != nil {
		s.Sleep(d)
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

func (s *Sender) scrub(text string) string { return Scrub(text, s.Secrets) }

// Scrub replaces every occurrence of each secret (the bot token) in text
// with "[redacted]". Its tags and entities aside, HTML is scrubbed alike: a
// bot token has no character HTML escapes.
func Scrub(text string, secrets []string) string {
	for _, sec := range secrets {
		if sec != "" {
			text = strings.ReplaceAll(text, sec, "[redacted]")
		}
	}
	return text
}

// Clean is what SendButtons does to its plain text and button texts: the
// bot token and other configured secrets scrubbed, then credentials
// redacted. It is for raw text only; HTML takes CleanHTML. Callers that
// store a sent text, or edit it later, apply the same one themselves.
func (s *Sender) Clean(text string) string { return render.Redact(s.scrub(text)) }

// CleanHTML is Clean for HTML, what SendButtons does to its html: the
// secrets scrubbed, then credentials redacted between the tags only, so
// no tag is swallowed.
func (s *Sender) CleanHTML(html string) string { return render.RedactHTML(s.scrub(html)) }

func (s *Sender) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Sender) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}

// Send flushes the spool, then delivers m as one or more chunks. While older
// messages are still undelivered, m is queued behind them instead of
// overtaking them.
func (s *Sender) Send(ctx context.Context, m Message) ([]int64, error) {
	m.Label, m.Title, m.Body = s.scrub(m.Label), s.scrub(m.Title), s.Clean(m.Body)
	chunks := render.Truncate(render.Chunk(m.Body, render.SourceChunk), render.MaxChunks)
	if len(chunks) == 0 {
		chunks = []string{""}
	}
	s.FlushSpool(ctx, 10)
	if backlog, _ := s.Store.SpoolList(); len(backlog) > 0 {
		if err := s.spoolFrom(m, chunks, 0, 0); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrLost, err)
		}
		return nil, fmt.Errorf("%w: %d older message(s) still undelivered", ErrSpooled, len(backlog))
	}
	var ids []int64
	var first int64
	for i, c := range chunks {
		html, plain := compose(m, c, i, len(chunks))
		req := telegram.SendRequest{ChatID: s.ChatID, Text: html, ParseMode: "HTML", Silent: m.Silent}
		if i > 0 {
			req.ReplyToMessageID = first
		}
		id, err := s.deliver(ctx, req, plain)
		if err != nil {
			if temporary(err) {
				s.record(m.SessionID, ids, false)
				if serr := s.spoolFrom(m, chunks, i, first); serr != nil {
					return ids, fmt.Errorf("%w: %v (after %v)", ErrLost, serr, err)
				}
				return ids, fmt.Errorf("%w: %v", ErrSpooled, err)
			}
			s.record(m.SessionID, ids, false)
			return ids, err
		}
		ids = append(ids, id)
		if i == 0 {
			first = id
		}
	}
	s.record(m.SessionID, ids, false)
	return ids, nil
}

// SendButtons posts one message with an inline keyboard. It is never spooled:
// buttons delivered hours late would mislead, so a failure is returned for the
// caller to retry. html (CleanHTML), plain and every button text (Clean)
// are redacted; callback data is passed through untouched.
func (s *Sender) SendButtons(ctx context.Context, sessionID, html, plain string, kb [][]telegram.Button) (int64, error) {
	html, plain = s.CleanHTML(html), s.Clean(plain)
	keyboard := make([][]telegram.Button, len(kb))
	for i, row := range kb {
		keyboard[i] = make([]telegram.Button, len(row))
		for j, b := range row {
			keyboard[i][j] = telegram.Button{Text: s.Clean(b.Text), Data: b.Data}
		}
	}
	req := telegram.SendRequest{ChatID: s.ChatID, Text: html, ParseMode: "HTML", InlineKeyboard: keyboard}
	id, err := s.deliver(ctx, req, plain)
	if err != nil {
		return 0, err
	}
	s.record(sessionID, []int64{id}, true)
	return id, nil
}

// temporary reports errors worth retrying later: transport failures,
// exhausted throttling and an expired context.
func temporary(err error) bool {
	return telegram.IsNetwork(err) || errors.Is(err, errThrottled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// deliver sends one request: paced, retried on 429, downgraded to plain text
// once on 400. Transport errors, exhausted throttling and context expiry are
// returned for the caller to spool.
func (s *Sender) deliver(ctx context.Context, req telegram.SendRequest, plain string) (int64, error) {
	fellBack := false
	throttled := 0
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		release, perr := s.Store.PaceCtx(ctx, s.MinGap)
		if perr != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			s.logf("pace: %v", perr)
			release = func() {}
		}
		id, err := s.API.SendMessage(ctx, req)
		release()
		if err == nil {
			return id, nil
		}
		var api *telegram.APIError
		if !errors.As(err, &api) {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			return 0, err // network: caller spools
		}
		switch {
		case api.Code == 429:
			throttled++
			wait := time.Duration(api.RetryAfter) * time.Second
			if wait <= 0 {
				wait = time.Second
			}
			if wait > maxRetryAfter {
				wait = maxRetryAfter
			}
			if throttled >= maxAttempts {
				return 0, fmt.Errorf("%w: %v", errThrottled, err)
			}
			s.logf("429, retrying in %s", wait)
			if !s.sleep(ctx, wait) {
				return 0, ctx.Err()
			}
		case api.Code == 400 && req.ParseMode != "" && !fellBack:
			s.logf("400 on HTML (%s), resending as plain text", api.Description)
			fellBack = true
			req.ParseMode = ""
			req.Text = plain
			attempt-- // the fallback is not one of the three attempts
		default:
			return 0, err
		}
	}
	return 0, fmt.Errorf("%w: gave up after %d attempts", errThrottled, maxAttempts)
}

// spoolFrom stores chunks[from:] for a later flush, in order.
func (s *Sender) spoolFrom(m Message, chunks []string, from int, first int64) error {
	base := s.now()
	for i := from; i < len(chunks); i++ {
		html, plain := compose(m, chunks[i], i, len(chunks))
		req := telegram.SendRequest{ChatID: s.ChatID, Text: html, ParseMode: "HTML", Silent: m.Silent}
		if first != 0 {
			req.ReplyToMessageID = first
		}
		item := state.SpoolItem{ID: newID(base, i), SessionID: m.SessionID, Req: req, Plain: plain, Created: base.Add(time.Duration(i))}
		if err := s.Store.SpoolPut(item); err != nil {
			s.logf("spool: %v", err)
			return err
		}
	}
	return nil
}

func newID(t time.Time, i int) string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%d-%02d-%s", t.UnixNano(), i, hex.EncodeToString(b[:]))
}

// record notes the session's sent messages, so a reply to one finds the
// session; prompt marks the message of a question or permission prompt
// (Session.AskMessage), so a reply to it never reaches another prompt.
func (s *Sender) record(sessionID string, ids []int64, prompt bool) {
	if sessionID == "" || len(ids) == 0 {
		return
	}
	_, _ = s.Store.UpdateSession(sessionID, func(sess *state.Session) {
		sess.LastMessageIDs = keepLast(append(sess.LastMessageIDs, ids...))
		if prompt {
			sess.AskMessageIDs = keepLast(append(sess.AskMessageIDs, ids...))
		}
	})
}

// keepLast is the last keepIDs ids.
func keepLast(ids []int64) []int64 {
	if n := len(ids); n > keepIDs {
		return ids[n-keepIDs:]
	}
	return ids
}

// FlushSpool delivers up to max spooled items, oldest first, under the spool
// lock so two processes never replay the same item. It stops at the first
// temporary failure (the item stays) and drops items Telegram refuses
// outright. It returns how many were sent.
func (s *Sender) FlushSpool(ctx context.Context, max int) int {
	unlock, err := s.Store.LockSpool(ctx)
	if err != nil {
		return 0
	}
	defer unlock()
	items, err := s.Store.SpoolList()
	if err != nil {
		return 0
	}
	sent := 0
	for _, it := range items {
		if sent >= max {
			break
		}
		plain := it.Plain
		if plain == "" {
			plain = it.Req.Text
		}
		if _, err := s.deliver(ctx, it.Req, plain); err != nil {
			if temporary(err) {
				return sent
			}
			s.logf("dropping spooled message %s: %v", it.ID, err)
			_ = s.Store.SpoolDelete(it.ID)
			continue
		}
		_ = s.Store.SpoolDelete(it.ID)
		sent++
	}
	return sent
}

// Classify maps a Send error to the CLI exit code: 3 when Telegram refused
// the message, 4 for anything temporary (spooled, throttled, lost).
func Classify(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, ErrSpooled) || errors.Is(err, ErrLost) || errors.Is(err, errThrottled) {
		return 4
	}
	var api *telegram.APIError
	if errors.As(err, &api) && api.Code != 429 {
		return 3
	}
	return 4
}
