package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/IZZY-Agency/claude-tg/internal/render"
	"github.com/IZZY-Agency/claude-tg/internal/session"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
	"github.com/IZZY-Agency/claude-tg/internal/term"
)

// API is the part of the Telegram client the router uses.
type API interface {
	SendMessage(ctx context.Context, r telegram.SendRequest) (int64, error)
	SetMessageReaction(ctx context.Context, chatID, messageID int64, emoji string) error
	AnswerCallbackQuery(ctx context.Context, id, text string) error
	EditMessageText(ctx context.Context, chatID, messageID int64, text, parseMode string) error
}

// Agents lists the live Claude Code sessions.
type Agents func(ctx context.Context) ([]session.Agent, error)

// Router handles one update at a time.
type Router struct {
	API       API
	Store     *state.Store
	Term      term.Terminal
	Agents    Agents
	ChatID    int64
	UserID    int64
	Now       func() time.Time
	Sleep     func(context.Context, time.Duration) // nil: real sleep; tests use a virtual clock
	Log       func(string, ...any)
	StartedAt time.Time
}

// Live is a running session as the relay sees it.
type Live struct {
	Name      string
	SessionID string
	Pane      term.Pane
	Cwd       string
	Busy      bool
	Follow    bool
}

const (
	maxText     = 4000
	screenLines = 40
	pendingTTL  = 10 * time.Minute
)

// Commands is the "/" menu the relay registers.
var Commands = []telegram.BotCommand{
	{Command: "sessions", Description: "live sessions and follow state"},
	{Command: "follow", Description: "follow a session: /follow name"},
	{Command: "unfollow", Description: "stop following: /unfollow name"},
	{Command: "approve", Description: "approve the permission prompt (reply or @name)"},
	{Command: "deny", Description: "deny the permission prompt (reply or @name)"},
	{Command: "status", Description: "relay status"},
	{Command: "help", Description: "how to address a session"},
}

func (r *Router) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Router) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

func (r *Router) userID() int64 {
	if r.UserID != 0 {
		return r.UserID
	}
	return r.ChatID
}

func (r *Router) allowed(from *telegram.User, chatID int64) bool {
	return from != nil && from.ID == r.userID() && (chatID == 0 || chatID == r.ChatID)
}

func bold(s string) string { return "<b>" + render.EscapeHTML(s) + "</b>" }

func (r *Router) send(ctx context.Context, replyTo int64, html string, kb [][]telegram.Button) (int64, error) {
	return r.API.SendMessage(ctx, telegram.SendRequest{ChatID: r.ChatID, Text: html, ParseMode: "HTML", ReplyToMessageID: replyTo, InlineKeyboard: kb})
}

func (r *Router) reply(ctx context.Context, m *telegram.Message, html string) error {
	_, err := r.send(ctx, m.MessageID, html, nil)
	return err
}

// react puts a reaction on the user's message; callers fall back to a text
// acknowledgment when it fails (Telegram accepts a fixed emoji set only).
func (r *Router) react(ctx context.Context, m *telegram.Message, emoji string) bool {
	if m == nil {
		return false
	}
	if err := r.API.SetMessageReaction(ctx, r.ChatID, m.MessageID, emoji); err != nil {
		r.logf("reaction: %v", err)
		return false
	}
	return true
}

// Allowed reports whether an update comes from the configured user in the
// configured chat.
func (r *Router) Allowed(u telegram.Update) bool {
	switch {
	case u.Message != nil:
		return r.allowed(u.Message.From, u.Message.Chat.ID)
	case u.CallbackQuery != nil:
		return u.CallbackQuery.Message != nil && r.allowed(u.CallbackQuery.From, u.CallbackQuery.Message.Chat.ID)
	}
	return false
}

// Handle routes one update. Errors are for the log; the loop keeps going.
func (r *Router) Handle(ctx context.Context, u telegram.Update) error {
	if u.CallbackQuery != nil {
		return r.handleCallback(ctx, u.CallbackQuery)
	}
	m := u.Message
	if m == nil {
		return nil
	}
	if !r.allowed(m.From, m.Chat.ID) {
		r.logf("dropped message from %v in chat %d", m.From, m.Chat.ID)
		return nil
	}
	text := strings.TrimSpace(m.Text)
	if text == "" {
		return nil
	}
	if strings.HasPrefix(text, "/") {
		return r.command(ctx, m, text)
	}
	return r.text(ctx, m, text)
}

func (r *Router) live(ctx context.Context) ([]Live, error) {
	agents, err := r.Agents(ctx)
	if err != nil {
		return nil, err
	}
	var out []Live
	for _, a := range agents {
		sess, err := r.Store.LoadSession(a.SessionID)
		if err != nil {
			r.logf("session %s: %v (skipped)", a.SessionID, err)
			continue
		}
		name := a.Name
		if name == "" {
			name = sess.Name
		}
		out = append(out, Live{Name: name, SessionID: a.SessionID, Pane: sess.Pane(), Cwd: a.Cwd, Busy: a.Busy(), Follow: sess.Follow})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// reachable returns nil when the session's pane can be typed into, else the
// message for the phone.
func (r *Router) reachable(ctx context.Context, l *Live) error {
	if l.Pane.IsZero() {
		return fmt.Errorf("%s runs in a terminal I cannot type into (replies need agterm or tmux)", bold(l.Name))
	}
	if !r.Term.Exists(ctx, l.Pane) {
		return fmt.Errorf("%s's %s pane %s is not reachable (is %s installed and the pane still open?)", bold(l.Name), l.Pane.Backend, render.EscapeHTML(l.Pane.Target), term.Tools[l.Pane.Backend])
	}
	return nil
}

func names(live []Live) []string {
	out := make([]string, len(live))
	for i, l := range live {
		out[i] = l.Name
	}
	return out
}

func bySession(live []Live, sessionID string) *Live {
	for i := range live {
		if live[i].SessionID == sessionID {
			return &live[i]
		}
	}
	return nil
}

func byName(live []Live, name string) *Live {
	for i := range live {
		if live[i].Name == name {
			return &live[i]
		}
	}
	return nil
}

func followed(live []Live) []Live {
	var out []Live
	for _, l := range live {
		if l.Follow {
			out = append(out, l)
		}
	}
	return out
}

// errNeedTarget means the message could not be tied to a session and the
// keyboard should be shown.
var errNeedTarget = errors.New("need target")

// target picks the session a message addresses: swipe-reply, @name / name:,
// then the single followed session. It returns the remaining text. A reply
// to a message no session is known to have sent, or to an ended session,
// is refused outright: never guess for an explicit reply.
func (r *Router) target(m *telegram.Message, text string, live []Live) (*Live, string, error) {
	if m.ReplyToMessage != nil {
		sess, ok := r.Store.FindSessionByMessageID(m.ReplyToMessage.MessageID)
		if !ok {
			return nil, "", errors.New("I can't tell which session sent that message; use @name or reply to a session's own message")
		}
		if l := bySession(live, sess.SessionID); l != nil {
			return l, text, nil
		}
		return nil, "", fmt.Errorf("%s has ended; pick another session", bold(sess.Name))
	}
	switch a := Address(text, names(live)); a.Kind {
	case AddrResolved:
		return byName(live, a.Name), a.Rest, nil
	case AddrAmbiguous, AddrUnknown:
		return nil, text, errNeedTarget
	}
	if f := followed(live); len(f) == 1 {
		return bySession(live, f[0].SessionID), text, nil
	}
	return nil, text, errNeedTarget
}

func (r *Router) text(ctx context.Context, m *telegram.Message, text string) error {
	live, err := r.live(ctx)
	if err != nil {
		return r.reply(ctx, m, "cannot list sessions: "+render.EscapeHTML(err.Error()))
	}
	l, rest, err := r.target(m, text, live)
	if errors.Is(err, errNeedTarget) {
		return r.askWhich(ctx, m, text, live)
	}
	if err != nil {
		return r.reply(ctx, m, err.Error())
	}
	note, err := r.deliver(ctx, l, rest)
	if err != nil {
		return r.reply(ctx, m, err.Error())
	}
	if !r.react(ctx, m, "👍") {
		if note == "" {
			note = "delivered to " + bold(l.Name)
		}
	}
	if note != "" {
		return r.reply(ctx, m, note)
	}
	return nil
}

// deliver types text into the session and submits it only after the input
// line has been read back and found to hold exactly that text. The text is
// typed with a leading space (a digit can then never select a dialog
// option). A dialog that appears meanwhile withholds Enter, since Enter in
// a dialog confirms "Yes". A doubled or garbled input line, seen in the
// field under load, is cleared with backspaces and retyped once.
// The returned error text is HTML for the user; note is a follow-up such as
// "queued".
func (r *Router) deliver(ctx context.Context, l *Live, text string) (string, error) {
	line := term.OneLine(text)
	if n := utf8.RuneCountInString(line); n > maxText {
		return "", fmt.Errorf("too long: %d characters, the limit is %d", n, maxText)
	}
	if line == "" {
		return "", errors.New("nothing to type")
	}
	if err := r.reachable(ctx, l); err != nil {
		return "", err
	}
	screen, err := r.Term.Screen(ctx, l.Pane, screenLines)
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %s", bold(l.Name), render.EscapeHTML(err.Error()))
	}
	if term.PromptVisible(screen) {
		return "", fmt.Errorf("%s is waiting on a permission prompt; send /approve or /deny first", bold(l.Name))
	}
	owed := 1
	if l.Busy {
		owed = 2 // the running turn's answer and the queued one
	}
	r.armAnswers(l, owed)
	fail := func(format string, args ...any) (string, error) {
		r.armAnswers(l, -owed)
		return "", fmt.Errorf(format, args...)
	}
	typed := " " + line
	want := normalize(typed)
	for attempt := 1; ; attempt++ {
		if err := r.Term.Type(ctx, l.Pane, typed); err != nil {
			return fail("could not type into %s: %s", bold(l.Name), render.EscapeHTML(err.Error()))
		}
		got, ok, err := r.awaitInput(ctx, l.Pane, want)
		if errors.Is(err, errDialog) {
			return fail("a permission prompt appeared in %s while typing; your text was not submitted. Send /approve or /deny, then send the text again", bold(l.Name))
		}
		if err != nil {
			return fail("cannot read %s while typing: %s", bold(l.Name), render.EscapeHTML(err.Error()))
		}
		if ok {
			break
		}
		r.logf("input of %s settled on %q (%d chars) instead of the typed text (%d chars); clearing", l.Name, got, utf8.RuneCountInString(got), utf8.RuneCountInString(want))
		if err := r.clearInput(ctx, l.Pane, got, typed); err != nil {
			return fail("could not clear %s's input (%s); nothing was submitted, check the pane and send it again", bold(l.Name), render.EscapeHTML(err.Error()))
		}
		if attempt >= 2 {
			return fail("%s's input did not take the text cleanly (saw %d characters, expected %d); nothing was submitted, please send it again", bold(l.Name), utf8.RuneCountInString(got), utf8.RuneCountInString(want))
		}
	}
	if err := r.Term.Type(ctx, l.Pane, term.Enter); err != nil {
		return fail("could not submit in %s: %s", bold(l.Name), render.EscapeHTML(err.Error()))
	}
	r.logf("delivered %d characters to %s (busy=%v)", utf8.RuneCountInString(line), l.Name, l.Busy)
	if l.Busy {
		return fmt.Sprintf("queued in %s (busy)", bold(l.Name)), nil
	}
	return "", nil
}

const (
	readPoll    = 250 * time.Millisecond // between two looks at the input line
	readSettle  = time.Second            // a mismatch counts only once the line stopped changing this long
	readTimeout = 8 * time.Second        // give up waiting for a match
)

var errDialog = errors.New("permission dialog appeared")

// awaitInput polls the pane's input line until it reads want (ok), or until
// it has stopped changing for readSettle without matching (not ok; got is
// the settled text), or readTimeout passes. Claude Code renders typed text
// with a visible delay, so a line that is still arriving is never judged.
func (r *Router) awaitInput(ctx context.Context, p term.Pane, want string) (got string, ok bool, err error) {
	start := r.now()
	var prev string
	var since time.Time // when prev was first seen
	for {
		screen, err := r.Term.Screen(ctx, p, screenLines)
		if err != nil {
			return "", false, err
		}
		if term.PromptVisible(screen) {
			return "", false, errDialog
		}
		got = normalize(InputText(screen))
		if got == want {
			return got, true, nil
		}
		now := r.now()
		if since.IsZero() || got != prev {
			prev, since = got, now
		}
		if now.Sub(since) >= readSettle || now.Sub(start) >= readTimeout {
			return got, false, nil
		}
		r.sleep(ctx, readPoll)
		if ctx.Err() != nil {
			return got, false, ctx.Err()
		}
	}
}

// clearInput erases the input line with more backspaces than it can hold
// (extra ones are harmless on an empty line; the pane may still be rendering
// text that has not shown up yet), then waits for the line to settle.
func (r *Router) clearInput(ctx context.Context, p term.Pane, current, typed string) error {
	n := 2*utf8.RuneCountInString(typed) + utf8.RuneCountInString(current) + 16
	if err := r.Term.Type(ctx, p, strings.Repeat(term.Backspace, n)); err != nil {
		r.logf("clear input: %v", err)
		return err
	}
	_, _, err := r.awaitInput(ctx, p, "")
	if errors.Is(err, errDialog) {
		return nil // the caller's next read reports the dialog
	}
	return err
}

func (r *Router) sleep(ctx context.Context, d time.Duration) {
	if r.Sleep != nil {
		r.Sleep(ctx, d)
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// normalize collapses whitespace so wrapped and padded screen lines compare
// equal to what was typed.
func normalize(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// InputText returns the content of Claude Code's input box as shown on
// screen: the last line starting with the prompt marker at column 0, plus
// its wrapped continuation lines up to the next rule or status line. The
// terminal pads lines, so callers compare with normalize.
func InputText(screen string) string {
	lines := strings.Split(screen, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "❯") {
			start = i
		}
	}
	if start < 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(strings.TrimPrefix(strings.TrimPrefix(lines[start], "❯"), " "))
	for _, l := range lines[start+1:] {
		trimmed := strings.TrimLeft(l, " ")
		if l == "" || strings.HasPrefix(l, "─") || strings.HasPrefix(trimmed, "[") || strings.HasPrefix(l, "❯") || strings.HasPrefix(l, "✻") || strings.HasPrefix(l, "⏺") {
			break
		}
		b.WriteString(l)
	}
	return b.String()
}

// armAnswers adds n to the answers the session owes the phone (n may be
// negative to roll back a failed delivery). Followed sessions report every
// answer anyway.
func (r *Router) armAnswers(l *Live, n int) {
	if l.Follow || n == 0 {
		return
	}
	if _, err := r.Store.UpdateSession(l.SessionID, func(s *state.Session) {
		s.PendingAnswers += n
		if s.PendingAnswers < 0 {
			s.PendingAnswers = 0
		}
	}); err != nil {
		r.logf("pending answers: %v", err)
	}
}

func newKey() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// KeyboardFor builds one button per typeable session.
func KeyboardFor(live []Live, key string) [][]telegram.Button {
	var rows [][]telegram.Button
	for _, l := range live {
		if l.Pane.IsZero() {
			continue
		}
		status := "idle"
		if l.Busy {
			status = "busy"
		}
		rows = append(rows, []telegram.Button{{Text: l.Name + " · " + status, Data: "s:" + l.SessionID + ":" + key}})
	}
	return rows
}

func (r *Router) askWhich(ctx context.Context, m *telegram.Message, text string, live []Live) error {
	key := newKey()
	kb := KeyboardFor(live, key)
	if len(kb) == 0 {
		return r.reply(ctx, m, "no live session has a terminal pane I can type into")
	}
	if err := r.Store.PutPending(key, text, r.now()); err != nil {
		return r.reply(ctx, m, "cannot park the message: "+render.EscapeHTML(err.Error()))
	}
	_, err := r.send(ctx, m.MessageID, "Which session?", kb)
	return err
}

func (r *Router) handleCallback(ctx context.Context, cq *telegram.CallbackQuery) error {
	if cq.Message == nil || !r.allowed(cq.From, cq.Message.Chat.ID) {
		r.logf("dropped callback from %v", cq.From)
		return nil
	}
	answer := func(text string) {
		if err := r.API.AnswerCallbackQuery(ctx, cq.ID, clipText(text, 200)); err != nil {
			r.logf("answer callback: %v", err)
		}
	}
	edit := func(html string) {
		if cq.Message == nil {
			return
		}
		if err := r.API.EditMessageText(ctx, r.ChatID, cq.Message.MessageID, html, "HTML"); err != nil {
			r.logf("edit: %v", err)
		}
	}
	parts := strings.Split(cq.Data, ":")
	if len(parts) != 3 || parts[0] != "s" {
		answer("unknown button")
		return nil
	}
	text, ok := r.Store.TakePending(parts[2], r.now(), pendingTTL)
	if !ok {
		answer("expired, send the text again")
		edit("expired; send the text again")
		return nil
	}
	live, err := r.live(ctx)
	if err != nil {
		answer("cannot list sessions")
		return err
	}
	l := bySession(live, parts[1])
	if l == nil {
		answer("that session is gone")
		edit("that session is gone")
		return nil
	}
	note, err := r.deliver(ctx, l, text)
	if err != nil {
		answer(stripTags(err.Error()))
		edit("✗ " + err.Error())
		return nil
	}
	msg := "→ " + bold(l.Name)
	if note != "" {
		msg += " · " + note
	}
	edit(msg)
	answer("sent to " + l.Name)
	return nil
}

func (r *Router) command(ctx context.Context, m *telegram.Message, text string) error {
	cmd, arg := ParseCommand(text)
	switch cmd {
	case "sessions":
		return r.sessions(ctx, m)
	case "follow", "unfollow":
		return r.setFollow(ctx, m, arg, cmd == "follow")
	case "approve", "deny":
		return r.decide(ctx, m, arg, cmd == "approve")
	case "status":
		return r.status(ctx, m)
	case "help", "start":
		return r.reply(ctx, m, helpText)
	}
	return r.reply(ctx, m, "unknown command /"+render.EscapeHTML(cmd)+"; see /help")
}

const helpText = `<b>Talking to a session</b>
Every message here comes from one Claude Code session; its name is in bold at the top.
• <b>Reply to a session's message</b> (swipe it) and your text goes to that session.
• Or start with <code>@name</code>, for example <code>@acme run the tests again</code>. A unique start of the name is enough.
• With exactly one followed session, plain text goes there. Otherwise I ask which session and show buttons.
Your text is typed into the session as its next prompt, and that session's next answer comes back here.

<b>Permission prompts</b> 🔐
When a session waits for your approval, plain text is refused until you decide:
<code>/approve</code> — chooses "Yes" (types 1)
<code>/deny</code> — cancels (sends Esc)
Target it by replying to the 🔐 message, or <code>/approve @name</code>.

<b>Following a session</b> 💬
<code>/follow name</code> — every answer of that session comes here
<code>/unfollow name</code> — stop

<b>More</b>
<code>/sessions</code> — live sessions: busy or idle, follow on or off
<code>/status</code> — relay status
<code>/help</code> — this text

<b>Icons</b>
🔔 alert · 📋 report · 💬 answer · 🔐 needs approval · ✋ needs input · 💥 session stopped · ⚙️ system
<b>Feedback on your messages</b>
👍 delivered · 👌 approved · 👎 denied · otherwise I reply with the reason`

func (r *Router) sessions(ctx context.Context, m *telegram.Message) error {
	live, err := r.live(ctx)
	if err != nil {
		return r.reply(ctx, m, "cannot list sessions: "+render.EscapeHTML(err.Error()))
	}
	if len(live) == 0 {
		return r.reply(ctx, m, "no live sessions")
	}
	var b strings.Builder
	for _, l := range live {
		status := "idle"
		if l.Busy {
			status = "busy"
		}
		follow := "follow off"
		if l.Follow {
			follow = "follow on"
		}
		pane := " · no pane"
		if !l.Pane.IsZero() {
			pane = " · " + l.Pane.Backend
		}
		fmt.Fprintf(&b, "%s · %s · %s · %s%s\n", bold(l.Name), status, follow, render.EscapeHTML(filepath.Base(l.Cwd)), pane)
	}
	return r.reply(ctx, m, strings.TrimRight(b.String(), "\n"))
}

// pick resolves a command's target: explicit name, swipe-reply, else the
// single followed session.
func (r *Router) pick(m *telegram.Message, arg string, live []Live) (*Live, string) {
	if arg != "" {
		name, ok := MatchName(arg, names(live))
		if !ok {
			return nil, "no live session matches " + bold(arg)
		}
		return byName(live, name), ""
	}
	if m.ReplyToMessage != nil {
		sess, ok := r.Store.FindSessionByMessageID(m.ReplyToMessage.MessageID)
		if !ok {
			return nil, "I can't tell which session sent that message; use @name or reply to a session's own message"
		}
		if l := bySession(live, sess.SessionID); l != nil {
			return l, ""
		}
		return nil, bold(sess.Name) + " has ended"
	}
	if f := followed(live); len(f) == 1 {
		return bySession(live, f[0].SessionID), ""
	}
	return nil, "say which session: add @name or reply to its message"
}

func (r *Router) setFollow(ctx context.Context, m *telegram.Message, arg string, want bool) error {
	live, err := r.live(ctx)
	if err != nil {
		return r.reply(ctx, m, "cannot list sessions: "+render.EscapeHTML(err.Error()))
	}
	l, why := r.pick(m, arg, live)
	if l == nil {
		return r.reply(ctx, m, why)
	}
	if _, err := r.Store.UpdateSession(l.SessionID, func(s *state.Session) { s.Follow = want }); err != nil {
		return r.reply(ctx, m, "cannot update "+bold(l.Name)+": "+render.EscapeHTML(err.Error()))
	}
	if want {
		return r.reply(ctx, m, "following "+bold(l.Name))
	}
	return r.reply(ctx, m, "stopped following "+bold(l.Name))
}

func (r *Router) decide(ctx context.Context, m *telegram.Message, arg string, approve bool) error {
	live, err := r.live(ctx)
	if err != nil {
		return r.reply(ctx, m, "cannot list sessions: "+render.EscapeHTML(err.Error()))
	}
	l, why := r.pick(m, arg, live)
	if l == nil {
		return r.reply(ctx, m, why)
	}
	if err := r.reachable(ctx, l); err != nil {
		return r.reply(ctx, m, err.Error())
	}
	screen, err := r.Term.Screen(ctx, l.Pane, screenLines)
	if err != nil {
		return r.reply(ctx, m, "cannot read "+bold(l.Name)+": "+render.EscapeHTML(err.Error()))
	}
	sig := term.DialogSignature(screen)
	if sig == "" {
		return r.reply(ctx, m, "no permission prompt visible in "+bold(l.Name))
	}
	// Read again right before the keystroke: the dialog must be the same one.
	again, err := r.Term.Screen(ctx, l.Pane, screenLines)
	if err != nil || term.DialogSignature(again) != sig {
		return r.reply(ctx, m, "the dialog in "+bold(l.Name)+" changed between two looks; check it and try again")
	}
	keys, emoji, word := "1"+term.Enter, "👌", "approved"
	if !approve {
		keys, emoji, word = term.Escape, "👎", "denied"
	}
	r.armAnswers(l, 1)
	if err := r.Term.Type(ctx, l.Pane, keys); err != nil {
		r.armAnswers(l, -1)
		return r.reply(ctx, m, "could not type into "+bold(l.Name)+": "+render.EscapeHTML(err.Error()))
	}
	if !r.react(ctx, m, emoji) {
		return r.reply(ctx, m, word+" "+bold(l.Name))
	}
	return nil
}

func (r *Router) status(ctx context.Context, m *telegram.Message) error {
	live, _ := r.live(ctx)
	rs, _ := r.Store.LoadRelay()
	last := "never"
	if !rs.LastUpdateAt.IsZero() {
		last = rs.LastUpdateAt.Local().Format("15:04:05")
	}
	up := r.now().Sub(r.StartedAt).Round(time.Minute)
	return r.reply(ctx, m, fmt.Sprintf("relay up %s · %d live sessions · last update %s", fmtDur(up), len(live), last))
}

func fmtDur(d time.Duration) string {
	h := int(d.Hours())
	mnt := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, mnt)
	}
	return fmt.Sprintf("%dm", mnt)
}

func clipText(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

func stripTags(s string) string {
	return strings.NewReplacer("<b>", "", "</b>", "", "<code>", "", "</code>", "", "&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(s)
}
