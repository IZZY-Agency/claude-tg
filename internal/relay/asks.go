package relay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/ask"
	"github.com/IZZY-Agency/claude-tg/internal/render"
	"github.com/IZZY-Agency/claude-tg/internal/send"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

const (
	toastGone           = "this request is gone"
	outcomeMac          = "answered at the Mac"
	outcomeExpired      = "expired: answer at the Mac"
	outcomeEnded        = "session ended"
	outcomeNotDelivered = "not delivered: answer at the Mac"
	notSentExpired      = "not sent: answer at the Mac"
	notSentEnded        = "not sent: session ended"
	// hookGrace is how long past its deadline an ask may outlive its hook:
	// the hook closes the ask at the deadline and Claude Code kills it a few
	// minutes later, so an ask still there after this has no hook, even when
	// its pid now belongs to another process.
	hookGrace = 10 * time.Minute
	// lockAge is how old an orphan ask lock must be before it is pruned.
	lockAge = time.Hour
	// corruptAge is how old an unreadable ask file must be before it is
	// removed.
	corruptAge = time.Hour
	// lockedEdit bounds an edit the relay makes holding an ask's lock: a
	// hook Claude Code signalled is killed about 1.5 s after the signal, and
	// has 1.4 s from it for that lock, the session's and its closing edits.
	// A stuck edit gives the lock up for lockYield, then is made once more
	// without it, within freeEdit.
	lockedEdit = 800 * time.Millisecond
	lockYield  = 100 * time.Millisecond
	freeEdit   = 10 * time.Second
	// sessionWait bounds the sweep's wait for a session's lock.
	sessionWait = 2 * time.Second
)

func (r *Router) alive(pid int) bool {
	if r.Alive != nil {
		return r.Alive(pid)
	}
	return syscall.Kill(pid, 0) == nil
}

// hookGone reports whether no waiting hook can still read the ask. An ask
// without a hook pid or a deadline was not made by a waiting hook, so none
// will read it.
func (r *Router) hookGone(a *state.Ask) bool {
	if a.HookPID <= 0 || a.Deadline.IsZero() || r.now().After(a.Deadline.Add(hookGrace)) {
		return true
	}
	return !r.alive(a.HookPID)
}

// sessionEnded reports whether the ask's session ended after the ask was
// made (an earlier end belongs to a session since resumed).
func (r *Router) sessionEnded(a *state.Ask) bool {
	sess, err := r.Store.LoadSession(a.SessionID)
	return err == nil && !sess.EndedAt.IsZero() && !sess.EndedAt.Before(a.Created)
}

// expireIfGone closes an open ask whose hook is gone, inside UpdateAsk, and
// reports whether it did: as "ended" when its session has ended, else as
// "expired".
func (r *Router) expireIfGone(a *state.Ask) bool {
	if !a.Open() || !r.hookGone(a) {
		return false
	}
	a.Closed = "expired"
	if r.sessionEnded(a) {
		a.Closed = "ended"
	}
	return true
}

// askRefusal is why nothing may be typed into the session now: an ask its
// hook has not closed and still waits on, its messages sent here or not
// yet, answered from the phone or not (until the hook hands the answer to
// Claude Code the picker may still be on the Mac's screen). It fails closed
// when the asks cannot be read; no asks directory means no asks. The error
// text is HTML for the user; nil means type away.
func (r *Router) askRefusal(l *Live) error {
	asks, err := r.Store.ListAsks()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		r.logf("list asks: %v", err)
		return errors.New("could not read open requests; try again")
	}
	waiting, sent, answered := false, false, false
	for _, a := range asks {
		if a.SessionID != l.SessionID || a.Closed != "" || r.hookGone(a) {
			continue
		}
		switch {
		case a.Done:
			answered = true
		case len(a.MessageIDs) > 0:
			sent = true
		default:
			waiting = true
		}
	}
	switch {
	case sent:
		return errors.New(onThePhone(l.Name))
	case answered:
		// The hook is about to hand the phone's answer to Claude Code.
		return fmt.Errorf("%s just got your answer; try again in a moment", bold(l.Name))
	case waiting:
		return fmt.Errorf("%s is waiting on a question or permission prompt; its buttons will arrive here shortly, or answer it on the Mac", bold(l.Name))
	}
	return nil
}

// onThePhone is the refusal while a prompt of the session waits on the
// phone (HTML).
func onThePhone(name string) string {
	return bold(name) + " is waiting on your answer to the question or permission prompt above; tap a button on it or reply to it"
}

// questionTool's asks are questions, with no Yes or No to decide.
const questionTool = "AskUserQuestion"

// sentAsks lists the session's asks that are on the phone and whose hook
// still waits, newest first: their messages went out, and they are open or
// answered from the phone and not yet collected (a decision then says
// "already answered", as a tap does). An error reading the asks fails
// closed, as askRefusal does; no asks directory means no asks.
func (r *Router) sentAsks(l *Live) ([]*state.Ask, error) {
	asks, err := r.Store.ListAsks()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		r.logf("list asks: %v", err)
		return nil, errors.New("could not read open requests; try again")
	}
	var out []*state.Ask
	for i := len(asks) - 1; i >= 0; i-- {
		if a := asks[i]; a.SessionID == l.SessionID && a.Closed == "" && len(a.MessageIDs) > 0 && !r.hookGone(a) {
			out = append(out, a)
		}
	}
	return out, nil
}

// promptClosed is the reply to /approve or /deny sent as a reply to the
// message of a prompt that is closed or gone.
const promptClosed = "that prompt is no longer open"

// decideReplied is /approve or /deny sent as a reply to message of ask a:
// it decides that prompt and no other, as a tap on its ✅ Yes or ❌ No
// would; a closed one is refused.
func (r *Router) decideReplied(ctx context.Context, m *telegram.Message, a *state.Ask, approve bool) error {
	if a.Closed != "" {
		return r.reply(ctx, m, promptClosed)
	}
	name := a.SessionName
	if name == "" {
		name = a.SessionID
	}
	return r.decideAsk(ctx, m, name, []*state.Ask{a}, approve)
}

// decideAsk is /approve or /deny on asks[0]: the prompt replied to, or the
// newest prompt of the session on the phone (ruling R38). It is exactly a
// tap on its ✅ Yes or ❌ No, through the same ApplyTap, edit and settle,
// the toast becoming the 👌 or 👎 reaction on the command. A question has
// no Yes or No: the reply points at its buttons. With several open prompts
// the reply says which one.
func (r *Router) decideAsk(ctx context.Context, m *telegram.Message, name string, asks []*state.Ask, approve bool) error {
	a := asks[0]
	if a.Tool == questionTool {
		return r.reply(ctx, m, onThePhone(name))
	}
	action, emoji, word := byte('y'), "👌", "approved"
	if !approve {
		action, emoji, word = 'n', "👎", "denied"
	}
	var res ask.Result
	b, expired, done, err := r.changeAsk(a.ID, func(a *state.Ask) { res = ask.ApplyTap(a, ask.Tap{AskID: a.ID, Action: action}) })
	if errors.Is(err, os.ErrNotExist) {
		return r.reply(ctx, m, toastGone)
	}
	if err != nil {
		return r.reply(ctx, m, "could not read this request: "+render.EscapeHTML(err.Error()))
	}
	if toast := r.settle(ctx, b, res, expired, done); res.Edit == "" || toast != res.Toast {
		// Answered or closed before or meanwhile: the tap's toast.
		return r.reply(ctx, m, render.EscapeHTML(toast))
	}
	open := 0
	for _, o := range asks {
		if o.Open() {
			open++
		}
	}
	note := ""
	if open > 1 {
		what := a.Summary
		if what == "" {
			what = a.Tool
		}
		note = fmt.Sprintf("%s the newest of %d open prompts in %s: %s", word, open, bold(name), render.EscapeHTML(clipText(what, 80)))
	}
	if !r.react(ctx, m, emoji) && note == "" {
		note = word + " " + bold(name)
	}
	if note != "" {
		return r.reply(ctx, m, note)
	}
	return nil
}

// answerCallback answers a button press with a toast, the secrets scrubbed
// and credentials redacted (plain text).
func (r *Router) answerCallback(ctx context.Context, cq *telegram.CallbackQuery, text string) {
	if err := r.API.AnswerCallbackQuery(ctx, cq.ID, clipText(render.Redact(r.scrub(text)), 200)); err != nil {
		r.logf("answer callback: %v", err)
	}
}

// changeAsk runs fn on the ask under its lock after closing it as expired
// when its hook is gone, and reports whether it closed it and whether fn
// completed the ask. When fn completes the ask, the session's next answer
// is owed to the phone (armAsk: a credit of the ask's own) before the ask
// is saved done, in the same critical section: the hook may hand the
// answer to Claude Code the moment it reads the ask, and the session's
// Stop hook must then find it owed. Done is set once per ask, so the
// answer is credited once (Ask.Armed); the credit is taken back when the
// ask cannot be saved, and by the hook (or its sweep) when the Mac wins.
func (r *Router) changeAsk(id string, fn func(*state.Ask)) (a *state.Ask, expired, done bool, err error) {
	armed := ""
	a, err = r.Store.UpdateAsk(id, func(a *state.Ask) error {
		expired = r.expireIfGone(a)
		was := a.Done
		fn(a)
		done = !was && a.Done
		if done && r.armAsk(a.SessionID, a.ID) {
			a.Armed, armed = true, a.SessionID
		}
		return nil
	})
	if err != nil && armed != "" {
		r.disarm(armed, id)
	}
	return a, expired, done && err == nil, err
}

// tap applies an ask button press (callback data "a:…"). The caller has
// checked the sender.
func (r *Router) tap(ctx context.Context, cq *telegram.CallbackQuery) error {
	t, ok := ask.ParseTap(cq.Data)
	if !ok {
		r.answerCallback(ctx, cq, "unknown button")
		return nil
	}
	var res ask.Result
	a, expired, done, err := r.changeAsk(t.AskID, func(a *state.Ask) {
		// A button belongs to its question's message (a permission prompt
		// has one, index 0): data pressed on any other message is not it.
		if t.Q < len(a.MessageIDs) && a.MessageIDs[t.Q] != cq.Message.MessageID {
			res = ask.Result{Toast: toastGone, Index: t.Q}
			return
		}
		res = ask.ApplyTap(a, t)
	})
	if errors.Is(err, os.ErrNotExist) {
		if err := r.API.EditReplyMarkup(ctx, r.ChatID, cq.Message.MessageID, [][]telegram.Button{}); err != nil {
			r.logf("clear keyboard: %v", err)
		}
		r.answerCallback(ctx, cq, toastGone)
		return nil
	}
	if err != nil {
		r.answerCallback(ctx, cq, "could not read this request; try again")
		return err
	}
	// The toast last: the message shows the answer first.
	r.answerCallback(ctx, cq, r.settle(ctx, a, res, expired, done))
	return nil
}

// replyToAsk applies a swipe-reply to message index of an open ask: it
// answers a question or denies a permission prompt; it is never typed.
func (r *Router) replyToAsk(ctx context.Context, m *telegram.Message, id string, index int, text string) error {
	var res ask.Result
	a, expired, done, err := r.changeAsk(id, func(a *state.Ask) { res = ask.ApplyReply(a, index, text) })
	if errors.Is(err, os.ErrNotExist) {
		return r.reply(ctx, m, toastGone)
	}
	if err != nil {
		return r.reply(ctx, m, "could not read this request: "+render.EscapeHTML(err.Error()))
	}
	toast := r.settle(ctx, a, res, expired, done)
	if res.Edit != "" && toast == res.Toast && r.react(ctx, m, "👍") {
		return nil
	}
	return r.reply(ctx, m, render.EscapeHTML(toast))
}

// errNoSave ends an UpdateAsk fn that changed nothing worth saving.
var errNoSave = errors.New("nothing to save")

// settle does what a tap or reply leaves to do after the ask was saved:
// retire an ask that just expired, or edit the message the result names.
// The edit is made under the ask's lock, on the ask read again there: a
// hook that closed the ask since it was saved (answered at the Mac, the
// session ended, the wait expired) has edited its messages, and one that
// closes it now waits for the lock and edits after, so the phone's answer
// never lands over "answered at the Mac". A closed or removed ask is not
// edited. When the result completed the ask (done), the edit, made or
// given up, is recorded (Settled), and the ask removed if its hook has
// already collected the answer: its hook leaves it until then
// (state.Store.CollectAsk). It returns the toast: the result's, or why its
// edit was dropped.
func (r *Router) settle(ctx context.Context, a *state.Ask, res ask.Result, expired, done bool) string {
	if expired {
		r.retire(ctx, a, true)
		return res.Toast
	}
	if res.Edit == "" && !res.KeyboardOnly {
		return res.Toast
	}
	if res.Index < 0 || res.Index >= len(a.MessageIDs) {
		r.logf("ask %s: no message %d to edit", a.ID, res.Index)
		return res.Toast
	}
	if r.afterApply != nil {
		r.afterApply(a.ID)
	}
	id, toast := a.MessageIDs[res.Index], res.Toast
	stuck := false
	_, err := r.Store.UpdateAsk(a.ID, func(cur *state.Ask) error {
		if cur.Closed != "" {
			toast = ask.ClosedToast(cur.Closed)
			return errNoSave
		}
		if !r.editAnswer(ctx, a.ID, id, res, lockedEdit) {
			stuck = true
			return errNoSave
		}
		return settled(cur, done)
	})
	if err != nil && !errors.Is(err, errNoSave) {
		r.logf("ask %s: message %d not edited: %v", a.ID, res.Index, err)
	}
	if !stuck {
		return toast
	}
	// The lock is free for a signalled hook a moment; then the ask is read
	// again and, still open to the answer, edited without the lock: a hook
	// closing it during that edit may see its text overwritten.
	time.Sleep(lockYield)
	cur, err := r.Store.LoadAsk(a.ID)
	switch {
	case err != nil:
		r.logf("ask %s: message %d not edited: %v", a.ID, res.Index, err)
		return toast
	case cur.Closed != "":
		return ask.ClosedToast(cur.Closed)
	}
	r.editAnswer(ctx, a.ID, id, res, freeEdit)
	if _, err := r.Store.UpdateAsk(a.ID, func(cur *state.Ask) error { return settled(cur, done) }); err != nil && !errors.Is(err, errNoSave) {
		r.logf("ask %s: record the edit: %v", a.ID, err)
	}
	return toast
}

// editAnswer makes a result's edit of message id (its text, or only its
// keyboard) within limit, and reports whether it ended before limit: an
// edit that failed is made, one that ran out of time is stuck.
func (r *Router) editAnswer(ctx context.Context, askID string, id int64, res ask.Result, limit time.Duration) bool {
	ectx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	var err error
	if res.KeyboardOnly {
		err = r.API.EditReplyMarkup(ectx, r.ChatID, id, r.cleanKeyboard(res.Keyboard))
	} else {
		err = r.edit(ectx, id, res.Edit)
	}
	if err != nil {
		r.logf("ask %s: edit message %d: %v", askID, res.Index, err)
	}
	return ctx.Err() != nil || !errors.Is(ectx.Err(), context.DeadlineExceeded)
}

// settled records on an ask that the edit showing its completed answer
// (done) was made or given up, removing the ask when its hook has
// collected the answer already; for any other result it saves nothing.
func settled(a *state.Ask, done bool) error {
	if !done {
		return errNoSave
	}
	a.Settled = true
	if a.Collected {
		return state.ErrRemoveAsk
	}
	return nil
}

// armAsk makes the session owe its next answer to the phone once ask
// askID is answered from it, as a typed reply does, with a credit of the
// ask's own (state.Session.ArmAsk), and reports whether it added one;
// followed sessions report every answer anyway.
func (r *Router) armAsk(sessionID, askID string) bool {
	if sessionID == "" {
		return false
	}
	armed := false
	if _, err := r.Store.UpdateSession(sessionID, func(s *state.Session) { armed = s.ArmAsk(askID) }); err != nil {
		r.logf("pending answers: %v", err)
		return false
	}
	return armed
}

// disarm takes back the credit armAsk added for ask askID.
func (r *Router) disarm(sessionID, askID string) {
	if _, err := r.Store.UpdateSession(sessionID, func(s *state.Session) { s.DropAsk(askID) }); err != nil {
		r.logf("pending answers: %v", err)
	}
}

// retire removes an ask no hook will remove. When closed (it was open and
// has just been closed because its hook is gone), each sent message still
// waiting for an answer says why and loses its buttons first. An ask
// answered from the phone whose hook never collected the answer did not
// reach Claude: every message of it says so. One the hook collected did:
// if the relay never showed the answer (it stopped between saving it and
// its edit), each answered message shows it now; then it is removed.
func (r *Router) retire(ctx context.Context, a *state.Ask, closed bool) {
	if a.TakeBack {
		// Its hook could not take the phone's counted answer back in time
		// (the session's lock was busy); once per ask, so a hook that did
		// it before going changes nothing here.
		tctx, cancel := context.WithTimeout(ctx, sessionWait)
		err := r.Store.TakeBackAnswer(tctx, a.SessionID, a.ID)
		cancel()
		if err != nil {
			r.logf("ask %s: take back the answer owed: %v; the next sweep retries", a.ID, err)
			return
		}
	}
	switch {
	case closed:
		outcome := outcomeExpired
		if a.Closed == "ended" {
			outcome = outcomeEnded
		}
		r.editOutcome(ctx, a, outcome, true)
	case a.Collected && !a.Settled:
		r.logf("ask %s: collected, its answer not yet shown on the phone; showing it", a.ID)
		for i, id := range a.MessageIDs {
			if html, ok := ask.Answered(a, i); ok {
				if err := r.edit(ctx, id, html); err != nil {
					r.logf("ask %s: edit message %d: %v", a.ID, i, err)
				}
			}
		}
	case a.Closed != "":
		r.logf("ask %s: closed (%s) by its hook, which is gone; editing its messages again", a.ID, a.Closed)
		r.closingEdits(ctx, a)
	case a.Done && !a.Collected:
		r.logf("ask %s: answered from the phone, but its hook is gone; not delivered", a.ID)
		r.editOutcome(ctx, a, outcomeNotDelivered, false)
	}
	if err := r.Store.DeleteAsk(a.ID); err != nil {
		r.logf("ask %s: delete: %v", a.ID, err)
	}
}

// closingEdits makes again the edits a hook makes as it closes its ask
// (killed by Claude Code while it made them, it may not have): answered
// at the Mac, each message says so; expired or ended, each message the
// phone answered says its answer was not sent, and the others why they
// close.
func (r *Router) closingEdits(ctx context.Context, a *state.Ask) {
	outcome, notSent := outcomeExpired, notSentExpired
	switch a.Closed {
	case "mac":
		outcome, notSent = outcomeMac, outcomeMac
	case "ended":
		outcome, notSent = outcomeEnded, notSentEnded
	}
	for i, id := range a.MessageIDs {
		line := outcome
		if _, answered := ask.Answered(a, i); answered {
			line = notSent
		}
		base := ""
		if i < len(a.Texts) {
			base = a.Texts[i]
		}
		if err := r.edit(ctx, id, ask.WithOutcome(base, line)); err != nil {
			r.logf("ask %s: edit message %d: %v", a.ID, i, err)
		}
	}
}

// editOutcome puts outcome under each sent message of the ask (only the
// unanswered ones when unansweredOnly) and drops its buttons.
func (r *Router) editOutcome(ctx context.Context, a *state.Ask, outcome string, unansweredOnly bool) {
	for i, id := range a.MessageIDs {
		if unansweredOnly && i < len(a.Answers) && a.Answers[i] != nil {
			continue
		}
		base := ""
		if i < len(a.Texts) {
			base = a.Texts[i]
		}
		if err := r.edit(ctx, id, ask.WithOutcome(base, outcome)); err != nil {
			r.logf("ask %s: edit message %d: %v", a.ID, i, err)
		}
	}
}

// SweepAsks retires every ask whose waiting hook is gone (its process is
// dead, it has no hook pid or deadline, or it is long past its deadline):
// an open one is closed ("session ended" when its session has ended, else
// "expired: answer at the Mac") and its unanswered messages say so; one
// answered from the phone but never collected says "not delivered: answer
// at the Mac" (one its hook collected says nothing); each is then removed,
// since its hook never will. It also
// removes ask files no one can read once they are an hour old, and prunes
// the lock files removed asks leave behind.
func (r *Router) SweepAsks(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	asks, err := r.Store.ListAsks()
	if err != nil {
		r.logf("sweep asks: %v", err)
		return
	}
	for _, a := range asks {
		if a.Collected && a.Settled {
			// Both sides are done with it and its removal failed.
			if err := r.Store.DeleteAsk(a.ID); err != nil {
				r.logf("ask %s: delete: %v", a.ID, err)
			}
			continue
		}
		if !r.hookGone(a) {
			continue
		}
		closed := false
		b, err := r.Store.UpdateAsk(a.ID, func(a *state.Ask) error {
			closed = r.expireIfGone(a)
			return nil
		})
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			r.logf("sweep ask %s: %v", a.ID, err)
			continue
		}
		if closed {
			r.logf("ask %s: hook gone; closed as %s", a.ID, b.Closed)
		}
		r.retire(ctx, b, closed)
	}
	if n, err := r.Store.PruneCorruptAsks(corruptAge, r.now()); err != nil {
		r.logf("prune corrupt asks: %v", err)
	} else if n > 0 {
		r.logf("removed %d corrupt ask file(s)", n)
	}
	if n, err := r.Store.PruneAskLocks(lockAge, r.now()); err != nil {
		r.logf("prune ask locks: %v", err)
	} else if n > 0 {
		r.logf("pruned %d orphan ask lock(s)", n)
	}
}

// edit replaces a message's text with html and drops its buttons: the
// secrets scrubbed, redacted between its tags, and sent as plain text when
// Telegram refuses the HTML. Every outcome the relay shows goes through it.
func (r *Router) edit(ctx context.Context, messageID int64, html string) error {
	return send.EditHTML(ctx, r.API, r.ChatID, messageID, r.scrub(html), nil)
}

// cleanKeyboard masks the secrets and credentials in button texts (plain
// text, so Redact), as the hook's sender did for the first keyboard;
// callback data stays as is.
func (r *Router) cleanKeyboard(kb [][]telegram.Button) [][]telegram.Button {
	out := make([][]telegram.Button, len(kb))
	for i, row := range kb {
		out[i] = make([]telegram.Button, len(row))
		for j, b := range row {
			out[i][j] = telegram.Button{Text: render.Redact(r.scrub(b.Text)), Data: b.Data}
		}
	}
	return out
}
