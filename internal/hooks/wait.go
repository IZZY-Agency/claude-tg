package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/IZZY-Agency/claude-tg/internal/ask"
	"github.com/IZZY-Agency/claude-tg/internal/render"
	"github.com/IZZY-Agency/claude-tg/internal/send"
	"github.com/IZZY-Agency/claude-tg/internal/session"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
	"github.com/IZZY-Agency/claude-tg/internal/term"
	"github.com/IZZY-Agency/claude-tg/internal/transcript"
)

// questionTool's asks get one message per question; any other tool's ask is
// a permission prompt with one message.
const questionTool = "AskUserQuestion"

const (
	defaultTick     = time.Second
	defaultAwayTick = 5 * time.Second
	defaultDeadline = 12 * time.Hour

	// The screen guard: the dialog must show on the pane within dialogWait
	// (read every dialogPoll); once a message is out the pane is read every
	// screenEvery, and two reads a tick apart without the dialog mean it was
	// answered at the Mac.
	dialogWait  = 3 * time.Second
	dialogPoll  = 500 * time.Millisecond
	screenEvery = 5 * time.Second

	// After a signal the transcript is watched this long, this often: an
	// Esc's result lands about 0.1 s after Claude Code's SIGTERM, and a
	// SIGKILL follows the SIGTERM about 1.5 s later (ruling R27).
	signalWait = 500 * time.Millisecond
	signalPoll = 100 * time.Millisecond
	// cancelBudget is the one deadline, from Claude Code's signal, for all
	// a signalled hook does as it closes: the transcript watch (signalWait),
	// the ask's lock (held by the relay for an edit, at most 0.8 s), the
	// session's lock, the closing edits. Claude Code kills the hook about
	// 1.5 s after the SIGTERM.
	cancelBudget = 1400 * time.Millisecond
	// closeWait bounds an ordinary close the same way.
	closeWait = 10 * time.Second
	// commitLockWait is how long the commit after stdout waits for the
	// ask's lock (ruling R44): Claude Code has the answer by then, and the
	// relay or its sweep finishes the ask.
	commitLockWait = time.Second

	// A turn not found phone-started is checked again every phoneRecheck
	// during the first phoneRecheckFor of the wait (ruling P14): the relay
	// records its line only once its Enter returns, which may be after
	// Claude Code took the prompt and the hook first looked.
	phoneRecheck    = 5 * time.Second
	phoneRecheckFor = 30 * time.Second

	// fallbackWindow is how long after the relay typed the phone's /approve
	// or /deny fallback into the session (state.Session.PhoneAnswerAt) a
	// dialog seen closed counts as closed from the phone (ruling P20).
	fallbackWindow = 30 * time.Second
)

// Why a wait ended without a phone answer: the Closed value and the line
// the hook adds to the messages it sent.
const (
	closedMac     = "mac"
	closedExpired = "expired"
	closedEnded   = "ended"
)

var outcomes = map[string]string{
	closedMac:     "answered at the Mac",
	closedExpired: "expired: answer at the Mac",
	closedEnded:   "session ended",
}

// notSent replaces "→ <answer>" on a question the phone answered when the
// wait ends before every question has an answer: the answer never reached
// Claude.
var notSent = map[string]string{
	closedExpired: "not sent: answer at the Mac",
	closedEnded:   "not sent: session ended",
}

// errAnswered stops the closing UpdateAsk from writing: the phone won.
var errAnswered = errors.New("answered from the phone")

func (d Deps) tick() time.Duration     { return orDefault(d.Tick, defaultTick) }
func (d Deps) awayTick() time.Duration { return orDefault(d.AwayTick, defaultAwayTick) }
func (d Deps) deadline() time.Duration { return orDefault(d.Deadline, defaultDeadline) }

func orDefault(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// relayUp reports whether a relay runs that routes taps to waiting hooks.
func (d Deps) relayUp() bool {
	if d.RelayUp != nil {
		return d.RelayUp()
	}
	return d.Store.RelayServes(state.FeatureAsks)
}

func (d Deps) unattended() bool { return d.Unattended != nil && d.Unattended() }

// awayIn is how long until the owner counts as away; 0 means away now.
// Without AwayIn, Away is asked again every awayTick.
func (d Deps) awayIn(ctx context.Context) time.Duration {
	switch {
	case d.AwayIn != nil:
		return d.AwayIn(ctx)
	case d.Away == nil || d.Away(ctx):
		return 0
	}
	return d.awayTick()
}

func (d Deps) attended(ctx context.Context, sessionID string) bool {
	return d.Attended == nil || d.Attended(ctx, sessionID)
}

// Watcher follows the call in the session's transcript: Poll reports
// whether it has a result, which means its dialog was answered.
type Watcher interface{ Poll() (bool, error) }

// watch starts the transcript watch; an error means the transcript cannot
// be stat'ed.
func (d Deps) watch(path, tool string, input json.RawMessage) (Watcher, error) {
	if d.Watch != nil {
		return d.Watch(path, tool, input)
	}
	w, err := transcript.NewWatch(path, tool, input)
	if err != nil {
		return nil, err // never a typed nil in the interface
	}
	return w, nil
}

// sleep waits dur or until ctx ends; false when ctx ended.
func (d Deps) sleep(ctx context.Context, dur time.Duration) bool {
	if d.Sleep != nil {
		return d.Sleep(ctx, dur)
	}
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// WaitPermission is the synchronous PermissionRequest hook. It records the
// request, then, once the owner is away, posts the prompt (or each question)
// with buttons and waits for the relay to write the phone's answer into the
// ask. It returns the decision JSON for Claude Code, or nil (no decision)
// when it skips, when the dialog is answered at the Mac (even if the phone
// answered too), the session ends, the wait expires, and on any error or
// panic (a phone answer completed before a panic is still returned).
// Buttons go out only beside a dialog the hook can see on its session's
// pane. With a decision comes commit, to run once stdout took it: it marks
// the phone's answer collected and removes the ask. Until then the ask
// stays, answered and not collected, so a decision that never reached
// Claude Code is reported as not delivered by the relay's sweep.
func WaitPermission(ctx context.Context, in *Input, d Deps) (out []byte, commit func()) {
	if in == nil {
		return nil, nil
	}
	var w *waiter
	defer func() {
		if r := recover(); r != nil {
			d.logf("permission-wait (%s): panic: %v", in.SessionID, r)
			out = nil
			if w != nil && w.id != "" {
				out = w.abandon()
			}
		}
		commit = nil
		if out != nil {
			commit = w.commit
		}
	}()
	// Note the transcript's size first: an answer at the Mac can write the
	// call and its result while the checks below still run.
	watch, werr := d.watch(in.TranscriptPath, in.ToolName, in.ToolInput)
	w = &waiter{d: d, in: in, n: 1, watch: watch, keys: callKey(in.ToolName, in.ToolInput, in.Cwd)}
	if err := handlePermissionRequest(ctx, in, d); err != nil {
		d.logf("permission-wait (%s): record: %v", in.SessionID, err)
	}
	// Cheapest first, and all before the first transcript scan (up to
	// 8 MiB); `claude agents` runs only once the ask exists, so the
	// six-second notification hook already sees it.
	switch {
	case in.ToolName == "ExitPlanMode", in.AgentID != "":
		return nil, nil
	case w.keys.empty():
		w.logf("no buttons: no text of this %s call to find its dialog by", in.ToolName)
		return nil, nil
	case !d.relayUp(), d.unattended():
		return nil, nil
	case werr != nil:
		w.logf("no buttons: transcript: %v", werr)
		return nil, nil
	case w.poll():
		return nil, nil
	}
	// Cleaned before it is cut and stored (Deps.summary): the ask and its
	// messages never hold a configured secret, or any prefix of one, or a
	// credential the call's input carries.
	w.summary = d.summary(in.ToolName, in.ToolInput)
	if in.ToolName == questionTool {
		qs, err := ask.ParseQuestions(in.ToolInput)
		if err != nil {
			d.logf("permission-wait (%s): %v", in.SessionID, err)
			return nil, nil
		}
		w.qs, w.n = qs, len(qs)
	}
	now := d.now()
	a := &state.Ask{
		ID:          state.NewAskID(),
		SessionID:   in.SessionID,
		Tool:        in.ToolName,
		Summary:     w.summary,
		Suggestions: in.PermissionSuggestions,
		HookPID:     os.Getpid(),
		Created:     now,
		Deadline:    now.Add(d.deadline()),
		Keys:        askKeys(w.keys, w.in.ToolInput, w.clean),
	}
	if w.qs != nil {
		// Only a question needs its input back; a Write or Edit input can
		// be megabytes.
		a.ToolInput = in.ToolInput
	}
	if err := d.Store.PutAsk(a); err != nil {
		d.logf("permission-wait (%s): %v", in.SessionID, err)
		return nil, nil
	}
	w.id, w.created, w.until = a.ID, a.Created, a.Deadline
	if !w.seeDialog(ctx) || !d.attended(ctx, in.SessionID) {
		w.remove()
		return nil, nil
	}
	return w.run(ctx), nil
}

// waiter is one waiting hook's view of its ask. sent and texts mirror the
// ask's MessageIDs and Texts so the messages can be closed even when the
// ask file can no longer be read.
type waiter struct {
	d       Deps
	in      *Input
	watch   Watcher
	qs      []ask.Question // nil for a permission prompt
	n       int            // messages to send: one per question, or one
	summary string
	keys    callKeys // texts of the call its dialog shows
	id      string
	created time.Time
	until   time.Time
	name    string
	sent    []int64
	texts   []string
	sig     string    // the dialog's or picker's signature on the pane; the ask stores only its fingerprint
	hurry   bool      // Claude Code signalled the hook: the ask's lock is waited for briefly (update)
	closeBy time.Time // the one deadline for closing (budget): lock waits and closing edits
	// A failure that repeats every tick or away check is logged once, until
	// the call works again.
	pollFailed, sendFailed, loadFailed, screenFailed, claimFailed, listFailed bool

	phoneLogged bool // why the turn is not phone-started was logged (fromPhone)
}

func (w *waiter) logf(format string, args ...any) {
	w.d.logf("permission-wait (%s): "+format, append([]any{w.in.SessionID}, args...)...)
}

// logOnce logs err unless *failed says it was logged already; a nil err
// clears the flag.
func (w *waiter) logOnce(failed *bool, what string, err error) {
	if err == nil {
		*failed = false
		return
	}
	if !*failed {
		w.logf("%s: %v", what, err)
		*failed = true
	}
}

// poll reports whether the call has a result. An error means "unknown",
// never "answered": keep waiting.
func (w *waiter) poll() bool {
	resolved, err := w.watch.Poll()
	w.logOnce(&w.pollFailed, "transcript", err)
	return err == nil && resolved
}

// sight is what one read of the pane says about the dialog.
type sight int

const (
	unknown sight = iota // the pane could not be read
	same                 // the dialog is still on screen
	gone                 // no dialog, or another one
)

// signature identifies the dialog or picker on screen; "" when none is.
func (w *waiter) signature(screen string) string {
	sig, _, _ := w.dialog(screen)
	return sig
}

// dialog is signature, the text of the dialog the call's key must show in,
// and whether the dialog's top border is in the read. The signature is the
// screen's alone, the same whichever hook reads it, so two calls whose keys
// both match one dialog compute one claim (state.Store.ClaimSignature) and
// only one gets it; matching the call is the key's job. A picker's
// signature is its set of questions (pickerSet), "" when the read does not
// hold it whole. Its key must show in the question on screen
// (term.PickerView): the first, when the hook first looks.
func (w *waiter) dialog(screen string) (sig, shows string, whole bool) {
	if w.qs != nil {
		p, ok := term.ReadPicker(screen)
		if !ok {
			return "", "", false
		}
		return w.pickerSet(p), p.View, true
	}
	sig, whole = term.PermissionDialog(screen)
	return sig, sig, whole
}

// pickerSet is the set signature of the picker read in p, the same on all
// its tabs and its review screen, so moving between them at the Mac
// answers nothing: for a call of several questions, its tab bar, whole and
// naming every question's header; for one question, its header and
// question. Another picker reads as another set: a tab bar for one
// question, a single question's header for several. "" when the read does
// not hold the picker whole (its top above the read, a tab bar clipped at
// the pane's edge or scrolled to the active tab): such a read can neither
// adopt a picker nor say it is gone.
func (w *waiter) pickerSet(p term.PickerRead) string {
	switch {
	case !p.Whole:
		return ""
	case len(w.qs) == 1 && p.Tabs != "":
		return p.Tabs
	case len(w.qs) == 1:
		return p.View
	case p.Tabs == "" && strings.HasPrefix(p.View, "←"):
		return "" // a tab bar not read whole
	case p.Tabs == "":
		return p.View // one question's picker
	}
	// Every header, exactly and in order, repeats included: a bar scrolled
	// to the active tab may show "Mode2" alone for headers Mode and Mode2.
	if len(p.TabLabels) != len(w.qs) {
		return ""
	}
	for i, q := range w.qs {
		if p.TabLabels[i] != strings.Join(strings.Fields(q.Header), " ") {
			return ""
		}
	}
	return p.Tabs
}

// look reads the pane once. A read error is logged once and is unknown,
// never gone: keep waiting. So is a read that shows the picker's marks but
// does not hold a picker whole (term.PickerMarkers): it says nothing about
// whether the call's picker is still there.
func (w *waiter) look(ctx context.Context) sight {
	if w.d.Screen == nil {
		return unknown
	}
	screen, err := w.d.Screen(ctx)
	w.logOnce(&w.screenFailed, "screen", err)
	if err != nil {
		return unknown
	}
	switch sig := w.signature(screen); {
	case sig == w.sig:
		return same
	case sig == "" && w.qs != nil && term.PickerMarkers(screen):
		return unknown
	}
	return gone
}

// seeDialog waits up to dialogWait for this call's dialog (or question
// picker) on the session's pane and records its signature. Without it the
// hook could never tell an approval at the Mac from a call still running.
// A dialog that does not show the call's key belongs to another call (one
// queued before it) and is never adopted (ruling R30), nor is one another
// ask of the session claimed, or may still claim, first (R30b): keys of
// concurrent calls can be alike.
//
// Calls made at once wait on one dialog, the first not yet answered shown
// with a queue counter (term.DialogQueued): a call's own dialog comes up
// only once the ones before it are answered. While the pane shows calls
// queued, the hook keeps looking, every tick, until dialogWait after the
// last read that showed them; it stops when the call has a result (the
// queue answered at the Mac), the session ends or the ask's deadline
// passes. A dialog that shows this call's keys and every key of another
// call still looking, keys that are more than this call's, is that call's
// (yields).
func (w *waiter) seeDialog(ctx context.Context) bool {
	d := w.d
	if d.Screen == nil {
		w.logf("no buttons: no terminal pane to read")
		return false
	}
	until, every := d.now().Add(dialogWait), dialogPoll
	queued := false
	for {
		screen, err := d.Screen(ctx)
		w.logOnce(&w.screenFailed, "screen", err)
		if err == nil {
			if sig, shows, whole := w.dialog(screen); sig != "" && w.keys.shownIn(shows, whole) && !w.yields(shows) && w.claim(sig) {
				w.sig = sig
				return true
			}
			if w.qs == nil && term.DialogQueued(screen) {
				queued, until = true, d.now().Add(dialogWait)
			}
		}
		switch {
		case queued && w.poll():
			w.logf("no buttons: answered at the Mac while queued behind another call")
			return false
		case queued && w.ended():
			w.logf("no buttons: the session ended while queued behind another call")
			return false
		case !d.now().Before(w.until):
			w.logf("no buttons: queued behind other calls until the deadline")
			return false
		}
		if !d.now().Before(until) {
			if queued {
				w.logf("no buttons: the queued calls went without this call's dialog")
			} else {
				w.logf("no buttons: no dialog of this call on the pane within %s", dialogWait)
			}
			return false
		}
		if queued {
			every = d.tick()
		}
		if !d.sleep(ctx, every) {
			w.logf("no buttons: no dialog of this call on the pane")
			return false
		}
	}
}

// yields reports whether a dialog showing this call's keys (shows, read
// whole) belongs to another call of the session whose hook still looks for
// its dialog: one whose keys are this call's and more, every one shown
// (ruling P27). The current weather's keys are all in the forecast's
// dialog, which shows a day count too. Only MCP calls have such keys
// (state.Ask.Keys); equal keys stay with the claim's order.
//
// The keys compare as their asks store them (askKeys), as sets. A call
// whose input holds a secret stores none: it neither takes a dialog from
// another this way nor gives one up.
func (w *waiter) yields(shows string) bool {
	ours := askKeys(w.keys, w.in.ToolInput, w.clean)
	if len(ours) == 0 {
		return false
	}
	asks, err := w.d.Store.ListAsks()
	w.logOnce(&w.listFailed, "list asks", err)
	for _, a := range asks {
		if a.ID == w.id || a.SessionID != w.in.SessionID || !a.Open() || a.Signature != "" || !w.d.alive(a.HookPID) {
			continue
		}
		theirs := a.Keys
		if len(theirs) > len(ours) && !slices.ContainsFunc(ours, func(k string) bool { return !slices.Contains(theirs, k) }) &&
			!slices.ContainsFunc(theirs, func(k string) bool { return !hasArg(shows, k) }) {
			return true
		}
	}
	return false
}

// askKeys is an MCP call's keys as its ask stores them (state.Ask.Keys),
// each once. A call whose input cleaning changes (the bot token, a
// credential) stores none: its keys shorten long values to keyLen runes,
// and a shortened secret no longer matches what cleaning masks.
// nil for any other call.
func askKeys(k callKeys, input json.RawMessage, clean func(string) string) []string {
	if clean(string(input)) != string(input) {
		return nil
	}
	return unique(k.every)
}

// unique is keys without repeats, in the order each first comes.
func unique(keys []string) []string {
	var out []string
	for _, k := range keys {
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// claim records sig on the ask unless the dialog belongs to another ask
// of the session (state.Store.ClaimSignature). A failure is logged once and
// claims nothing.
func (w *waiter) claim(sig string) bool {
	ok, err := w.d.Store.ClaimSignature(w.id, sig, dialogWait+dialogPoll, w.d.now())
	w.logOnce(&w.claimFailed, "claim dialog", err)
	return ok
}

func (w *waiter) run(ctx context.Context) []byte {
	d := w.d
	var nextAway, nextScreen time.Time
	suspect := false // the last read missed the dialog: the next one, a tick later, decides
	// A turn started from the phone has its owner away from the Mac, so
	// the buttons go out without waiting for the idle time. Judged once
	// the dialog is seen, on the session record loaded then, and while it
	// is not phone-started and messages are still to send, again every
	// phoneRecheck until phoneRecheckFor has passed; a turn found
	// phone-started then ends the idle wait at once. The screen guard, an
	// answer at the Mac and the deadline stay as they are.
	start := d.now()
	phone := w.fromPhone()
	nextPhone, phoneUntil := start.Add(phoneRecheck), start.Add(phoneRecheckFor)
	for {
		if ctx.Err() != nil {
			return w.cancelled()
		}
		if w.poll() {
			return w.close(closedMac)
		}
		if w.ended() {
			return w.close(closedEnded)
		}
		now := d.now()
		if !now.Before(w.until) {
			return w.close(closedExpired)
		}
		// The screen guard runs before each send and every screenEvery
		// once a message is out: the transcript cannot tell an approval
		// at the Mac from a call that is still running.
		if suspect || (len(w.sent) > 0 && !now.Before(nextScreen)) {
			switch w.look(ctx) {
			case gone:
				if suspect {
					return w.close(closedMac)
				}
				suspect = true
			case same:
				suspect = false
				nextScreen = now.Add(screenEvery)
			case unknown:
				// An unreadable pane says nothing: it breaks a run of
				// misses, and the pane is read again screenEvery later,
				// a send held until the next away check.
				suspect = false
				nextScreen = now.Add(screenEvery)
				if len(w.sent) < w.n {
					nextAway = now.Add(d.awayTick())
				}
			}
		}
		if !phone && len(w.sent) < w.n && now.Before(phoneUntil) && !now.Before(nextPhone) {
			nextPhone = now.Add(phoneRecheck)
			if phone = w.fromPhone(); phone {
				nextAway = now // the idle wait is over: send now
			}
		}
		if !suspect && len(w.sent) < w.n && !now.Before(nextAway) {
			var wait time.Duration
			if !phone {
				wait = d.awayIn(ctx)
			}
			if wait > 0 {
				// The idle time grows a second a second: the gate cannot
				// open sooner. At least a second, so no busy re-check.
				nextAway = now.Add(max(wait, time.Second))
			} else {
				switch w.look(ctx) {
				case gone:
					suspect, nextAway = true, now // send once the next read shows the dialog
				case unknown:
					nextAway = now.Add(d.awayTick()) // never send blind; logged once by look
				default:
					nextAway, nextScreen = now.Add(d.awayTick()), now.Add(screenEvery) // a failed send retries at nextAway
					if out, end := w.sendAll(ctx); end {
						return out
					}
				}
			}
		}
		if len(w.sent) > 0 {
			a, err := d.Store.LoadAsk(w.id)
			switch {
			case err == nil && a.Done:
				return w.finish(a)
			case err == nil && a.Closed != "":
				// Closed by someone else (the relay's sweep), who edited the messages.
				w.logf("ask closed elsewhere: %s", a.Closed)
				w.remove()
				return nil
			case errors.Is(err, os.ErrNotExist):
				w.logf("ask gone")
				return w.close(closedExpired)
			}
			w.logOnce(&w.loadFailed, "load ask", err)
		}
		if !d.sleep(ctx, d.tick()) {
			return w.cancelled()
		}
	}
}

// fromPhone loads the session afresh and reports whether the turn started
// from the phone (phoneStarted), logging the line that says so, or, once
// per hook call, why a session holding phone lines does not count.
func (w *waiter) fromPhone() bool {
	sess, err := w.d.Store.LoadSession(w.in.SessionID)
	if err != nil {
		return false
	}
	phone, why := w.d.phoneStarted(w.in, sess)
	if why != "" && !w.phoneLogged {
		w.phoneLogged = true
		w.d.logPhone(w.in.SessionID, why)
	}
	if phone {
		w.logf("phone-started turn: sending now")
	}
	return phone
}

// cancelled closes a wait whose context ended: SIGTERM, SIGHUP or SIGINT.
// Claude Code sends SIGTERM about 0.1 s after an Esc at the Mac, before the
// call's result is written, and as the session quits (its SessionEnd hooks
// run only after this hook is gone). Only the transcript decides (ruling
// R27): a result within signalWait is the Mac's answer, anything else the
// session ending. The pane is not read: quitting Claude Code in a pane that
// stays open takes the dialog away too. The hook's own deadline ends the
// wait before Claude Code's hook timeout: a signal never means expired.
func (w *waiter) cancelled() []byte {
	d := w.d
	w.hurry, w.closeBy = true, time.Now().Add(cancelBudget)
	// The hook's own context is done; the window needs one of its own.
	ctx, cancel := context.WithTimeout(context.Background(), signalWait+signalPoll)
	defer cancel()
	until := d.now().Add(signalWait)
	for {
		if w.poll() {
			return w.close(closedMac)
		}
		if !d.now().Before(until) || !d.sleep(ctx, signalPoll) {
			return w.close(closedEnded)
		}
	}
}

// ended reports whether the session quit after the request was made. The
// hook's own CLAUDE_PID wins over the recorded pid, which may belong to an
// earlier process of a resumed session.
func (w *waiter) ended() bool {
	sess, err := w.d.Store.LoadSession(w.in.SessionID)
	if err != nil {
		return false
	}
	if sess.EndedAt.After(w.created) {
		return true
	}
	pid := w.d.Env.PID
	if pid == 0 {
		pid = sess.PID
	}
	return !w.d.alive(pid)
}

// sendAll sends the messages not out yet. end reports that the wait is
// over: a message went out but could not be recorded, or Telegram refused
// one for good and the plain alert went instead. Any other failure is
// retried at the next away check.
func (w *waiter) sendAll(ctx context.Context) (out []byte, end bool) {
	err := w.send(ctx)
	switch {
	case errors.Is(err, errRecord):
		w.logf("send: %v", err)
		return w.close(closedExpired), true
	case permanent(err):
		w.logf("send: %v; plain alert instead", err)
		out := w.close(closedExpired)
		w.plainAlert(ctx)
		return out, true
	}
	w.logOnce(&w.sendFailed, "send", err)
	return nil, false
}

// errRecord marks a message that went out but could not be recorded in the
// ask: the relay could not route its taps.
var errRecord = errors.New("recording the sent message")

// send posts every message not sent yet, in order, recording each in the
// ask as it goes. It stops at the first failed send, which the next away
// check retries.
func (w *waiter) send(ctx context.Context) error {
	if w.name == "" {
		w.name = w.resolveName(ctx)
	}
	for i := len(w.sent); i < w.n; i++ {
		html, plain, kb := w.message(i)
		mid, err := w.d.Sender.SendButtons(ctx, w.in.SessionID, html, plain, kb)
		if err != nil {
			return fmt.Errorf("message %d of %d: %w", i+1, w.n, err)
		}
		w.sent, w.texts = append(w.sent, mid), append(w.texts, html)
		_, err = w.d.Store.UpdateAsk(w.id, func(a *state.Ask) error {
			a.SessionName = w.name
			a.MessageIDs = append([]int64(nil), w.sent...)
			a.Texts = append([]string(nil), w.texts...)
			// Sized by index, never appended: the relay grows these too
			// when a tap answers an earlier message.
			for len(a.Answers) < w.n {
				a.Answers = append(a.Answers, nil)
			}
			for len(a.Ticked) < w.n {
				a.Ticked = append(a.Ticked, nil)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("%w %d: %v", errRecord, i+1, err)
		}
	}
	w.logf("sent %d message(s)", w.n)
	return nil
}

// permanent reports a Telegram refusal no retry can fix: a 4xx other than
// 429, after the sender's plain-text fallback for a 400.
func permanent(err error) bool {
	var api *telegram.APIError
	return errors.As(err, &api) && api.Code >= 400 && api.Code < 500 && api.Code != 429
}

// plainAlert sends the prompt as today's plain 🔐 alert, deduped against
// the six-second notification hook's own.
func (w *waiter) plainAlert(ctx context.Context) {
	text := permissionText(w.in.ToolName, w.summary)
	if err := deliverAlert(ctx, w.d, w.in.SessionID, w.name, text, send.KindPermission, true); err != nil {
		w.logf("plain alert: %v", err)
	}
}

// clean is what the sender applies to every plain text and button text it
// sends (secrets scrubbed, credentials redacted), so stored texts match it.
func (w *waiter) clean(text string) string { return w.d.clean(text) }

// cleanHTML is clean for HTML: credentials are redacted between the tags
// only, so a masked value never swallows a closing tag. Every HTML text the
// hook sends, stores or edits goes through it.
func (w *waiter) cleanHTML(html string) string {
	if w.d.Sender == nil {
		return render.RedactHTML(html)
	}
	return w.d.Sender.CleanHTML(html)
}

// message renders message i exactly as it will be sent: the stored text is
// the base of every later edit.
func (w *waiter) message(i int) (html, plain string, kb [][]telegram.Button) {
	if w.qs == nil {
		// The buttons show only each suggestion's number; the message
		// lists what each number grants, so both get the same list.
		sugg := ask.ParseSuggestions(w.in.PermissionSuggestions)
		html, plain = ask.PermissionMessage(w.name, w.in.ToolName, w.summary, sugg...)
		kb = ask.PermissionKeyboard(w.id, sugg)
	} else {
		html, plain = ask.QuestionMessage(w.name, w.qs[i], i+1, w.n)
		kb = ask.QuestionKeyboard(w.id, i, w.qs[i], nil)
	}
	for _, row := range kb {
		for j := range row {
			row[j].Text = w.clean(row[j].Text)
		}
	}
	return w.cleanHTML(html), w.clean(plain), kb
}

func (w *waiter) resolveName(ctx context.Context) string {
	if w.d.Resolve != nil {
		sess, err := w.d.resolve(ctx, w.in)
		if err == nil && sess.Name != "" {
			return sess.Name
		}
		if err != nil {
			w.logf("resolve: %v", err)
		}
	}
	return session.FallbackLabel(w.in.Cwd, w.in.SessionID)
}

// finish is the decision for the phone's answer. The ask stays until the
// decision is delivered (commit).
func (w *waiter) finish(a *state.Ask) []byte {
	out, err := ask.Output(a)
	if err != nil {
		// The answer cannot reach Claude: send the owner back to the Mac.
		w.logf("output: %v", err)
		return w.end(closedExpired, nil)
	}
	return out
}

// budget is the one deadline for closing the wait, set at its first use
// (or at Claude Code's signal, cancelBudget from it) and never renewed:
// every lock wait and the closing edits share what is left of it.
func (w *waiter) budget() time.Time {
	if w.closeBy.IsZero() {
		w.closeBy = time.Now().Add(closeWait)
	}
	return w.closeBy
}

// end finishes a wait that closed for reason without the phone's answer
// reaching Claude Code. a is the ask as closed (closeAsk), or nil to close
// it here. The closure is on record first, the answer the relay counted as
// owed to the phone (Armed) marked owed back (TakeBack); a dialog answered
// at the Mac is recorded in the session (macAnswered); then that answer is
// taken back within the lock budget (giveBack, the session's lock), the
// messages are edited and the ask removed. An answer not taken back in
// time leaves the ask to the relay's sweep, which finishes it.
func (w *waiter) end(reason string, a *state.Ask) []byte {
	until := w.budget()
	if a == nil {
		var err error
		a, err = w.update(until, func(a *state.Ask) error { return closeAsk(a, reason) })
		if err != nil {
			w.logf("close: %v", err)
		}
	}
	w.logf("closed: %s", reason)
	if reason == closedMac {
		w.macAnswered(until)
	}
	owed := a != nil && a.TakeBack && !w.giveBack(until)
	edited := w.editAll(a, reason)
	switch {
	case owed:
		w.logf("the answer owed back is left to the relay's sweep")
	case !edited:
		w.logf("the closing edits are left to the relay's sweep")
	default:
		w.remove()
	}
	return nil
}

// closeAsk records on a that its wait ended for reason, an answer the
// relay counted as owed to the phone marked owed back.
func closeAsk(a *state.Ask, reason string) error {
	if a.Armed {
		a.TakeBack = true
	}
	a.Closed = reason
	return nil
}

// macAnswered records in the session, by until, that one of its prompts
// was just answered at the Mac (state.Session.MacAnswerAt): the owner is
// there, so a turn the phone started before now no longer counts as one
// (phoneCheck, ruling P16). A later time already there is kept. A dialog
// closed within fallbackWindow after the relay typed the phone's /approve
// or /deny into it (state.Session.PhoneAnswerAt) was answered from the
// phone, and nothing is recorded (P20). A failure is only logged: the
// turn's prompts may then still skip the idle time.
func (w *waiter) macAnswered(until time.Time) {
	ctx, cancel := context.WithDeadline(context.Background(), until)
	defer cancel()
	now := w.d.now()
	phone := false
	if _, err := w.d.Store.UpdateSessionCtx(ctx, w.in.SessionID, func(s *state.Session) {
		if d := now.Sub(s.PhoneAnswerAt); d >= 0 && d <= fallbackWindow {
			phone = true
			return
		}
		if now.After(s.MacAnswerAt) {
			s.MacAnswerAt = now
		}
	}); err != nil {
		w.logf("record the answer at the Mac: %v", err)
	}
	if phone {
		w.logf("closed by the phone's /approve or /deny: no answer at the Mac recorded")
	}
}

// giveBack takes back, by until, what the relay gave the phone's answer in
// the session (state.Store.TakeBackAnswer, only this ask's): its credit in
// AskCredits, unless the Stop hook already spent it, and its voice mark
// whenever that is still there. Credits and marks are spent in separate
// oldest-first orders, so either may be gone while the other is left. It
// reports false when the session could not be updated by until.
func (w *waiter) giveBack(until time.Time) bool {
	ctx, cancel := context.WithDeadline(context.Background(), until)
	defer cancel()
	if err := w.d.Store.TakeBackAnswer(ctx, w.in.SessionID, w.id); err != nil {
		w.logf("take back the answer owed: %v", err)
		return false
	}
	return true
}

// commit runs once the decision is on stdout: the phone's answer is
// collected (state.Store.CollectAsk). The ask goes now if the relay has
// shown the answer on the phone, else once it has: the relay's edit must
// find it. A relay sweep that finds a collected ask whose hook is gone
// knows the answer reached Claude Code. The ask's lock is waited for at
// most commitLockWait; past it the commit gives up, and the relay or its
// sweep finishes the ask.
func (w *waiter) commit() {
	if err := w.d.Store.CollectAskWithin(w.id, commitLockWait); err != nil {
		w.logf("mark collected: %v", err)
	}
	w.logf("answered from the phone")
}

// close ends the wait without a phone answer: it marks the ask closed so
// later taps are refused, edits every sent message and removes the ask. An
// ask the phone completed meanwhile is returned, unless the Mac answered:
// then the call is decided already, the phone's answer is dropped and the
// answer the relay counted as owed to the phone is taken back.
func (w *waiter) close(reason string) []byte {
	var done *state.Ask
	a, err := w.update(w.budget(), func(a *state.Ask) error {
		if a.Done && reason != closedMac {
			done = a
			return errAnswered
		}
		return closeAsk(a, reason)
	})
	if done != nil {
		return w.finish(done)
	}
	if err != nil {
		w.logf("close: %v", err)
		a = &state.Ask{Closed: reason} // the edits still go out
	}
	return w.end(reason, a)
}

// editAll closes every sent message with the outcome of reason, under a
// fresh context ending at the closing deadline (budget): the hook's own
// may be gone. An answer at the Mac replaces whatever the phone showed.
// Otherwise a message the phone answered (in a that is not fully
// answered) says its answer was not sent. It reports whether every edit
// went out; the relay's sweep makes them again for an ask left behind.
func (w *waiter) editAll(a *state.Ask, reason string) bool {
	if w.d.Edit == nil || len(w.sent) == 0 {
		return true
	}
	ctx, cancel := context.WithDeadline(context.Background(), w.budget())
	defer cancel()
	ok := true
	for i, mid := range w.sent {
		outcome := outcomes[reason]
		if line, sent := notSent[reason]; sent && a != nil && w.answered(a, i) {
			outcome = line
		}
		if err := w.d.Edit(ctx, mid, w.cleanHTML(ask.WithOutcome(w.texts[i], outcome))); err != nil {
			w.logf("edit message %d: %v", mid, err)
			ok = false
		}
	}
	return ok
}

// answered reports whether message i of a already shows a phone answer.
func (w *waiter) answered(a *state.Ask, i int) bool {
	if w.qs == nil {
		return a.Decision != nil
	}
	return i < len(a.Answers) && a.Answers[i] != nil
}

func (w *waiter) remove() {
	wait := 10 * time.Second
	if w.hurry {
		wait = 0 // left to the relay's sweep when the lock is held
	}
	if err := w.d.Store.DeleteAskWithin(w.id, wait); err != nil {
		w.logf("delete ask: %v", err)
	}
}

// update is UpdateAsk on the hook's ask, waiting for its lock until until.
// Once Claude Code signalled the hook (hurry) and the wait ran out, it
// tries once more, then puts its change on record without the lock
// (state.Store.ForceUpdateAsk): the relay holds the lock only for an edit,
// and a closure that edit may overwrite is better than none.
func (w *waiter) update(until time.Time, fn func(*state.Ask) error) (*state.Ask, error) {
	st := w.d.Store
	a, err := st.UpdateAskWithin(w.id, time.Until(until), fn)
	if !w.hurry {
		return a, err
	}
	if errors.Is(err, state.ErrAskLocked) {
		a, err = st.UpdateAskWithin(w.id, 0, fn)
	}
	if errors.Is(err, state.ErrAskLocked) {
		w.logf("ask locked; closing it without the lock")
		a, err = st.ForceUpdateAsk(w.id, fn)
	}
	return a, err
}

// abandon closes the ask after a panic, best effort. An answer the phone
// completed is still handed to Claude Code, as any other close does; on
// every other path, and on a second panic, there is no decision.
func (w *waiter) abandon() (out []byte) {
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	return w.close(closedExpired)
}

// keyLen is how much of a command or question the key keeps: enough to tell
// calls apart, short enough to be on screen even when the dialog clips a
// long one.
const keyLen = 40

// callKeys are texts of a call that its dialog shows, in its words (rulings
// R30, R30b). A dialog whose top border is in the read must show one of
// whole, or every one of every (each standing alone: hasArg); one taller
// than the read shows only its question, which must show one of tall
// (none: such a dialog is never adopted). A key is compared with
// whitespace and line breaks removed (term.SignatureHas); "─" stands for
// one of the dialog's rules.
type callKeys struct{ whole, tall, every []string }

func (k callKeys) empty() bool { return len(k.whole) == 0 && len(k.every) == 0 }

// shownIn reports whether a dialog's signature shows the keys: one of
// whole, or every one of every (a whole dialog); one of tall (a taller one).
func (k callKeys) shownIn(sig string, whole bool) bool {
	keys := k.tall
	if whole {
		keys = k.whole
		if len(k.every) > 0 && !slices.ContainsFunc(k.every, func(key string) bool { return !hasArg(sig, key) }) {
			return true
		}
	}
	for _, key := range keys {
		if term.SignatureHas(sig, key) {
			return true
		}
	}
	return false
}

// callKey is the keys of a call as Claude Code 2.1.310 shows it (the
// screens in internal/term/testdata). Each key carries the text the dialog
// puts around it, where those screens prove it, so a key never matches a
// longer text that merely starts or ends with it:
//   - Bash: the command between two rules; past keyLen characters only its
//     start is kept.
//   - Edit, Write: "Edit file", or "Create file" / "Overwrite file" /
//     "Write file", the path relative to the cwd, a rule; a file outside
//     the cwd: "/" its base name and a rule. Taller than the read: "Do you
//     want to make this edit to <base>?", "Do you want to create <base>?",
//     "… overwrite <base>?", "… write to <base>?".
//   - Read outside the cwd: "Read(" the absolute path, then ")" or, with an
//     offset or a limit, " ·".
//   - WebFetch: urlKeys.
//   - AskUserQuestion: the first question's first keyLen characters.
//   - Read inside the cwd, NotebookEdit, MultiEdit: the base name, without
//     a recorded dialog to delimit it.
//   - An MCP tool (mcp__ server and tool): "(MCP)" and one key for every
//     leaf of the input, nested objects and arrays included, as the
//     dialog prints the arguments (queryParams: { query: "Paris, France"
//     }); all of them must show, each standing alone (hasArg). A leaf
//     under an object key that is a plain identifier is that name, a
//     colon and the value (query:"Paris, France"), so a value under
//     another name, or another tool's argument, does not match; an array
//     item or a value under any other name is the value alone. The value
//     is a string in quotes, so a short one never matches a longer one it
//     starts, or, from keyLen runes on, an opening quote and the first
//     keyLen runes; a whole number, true or false as written. A string
//     with a quote, a backslash or a line break (the dialog escapes or
//     wraps it), a blank one, a float and a null give no key, and an
//     input without a usable string has no keys: numbers and booleans
//     only add precision (rulings P5, P12, P21). Never adopted from a
//     dialog taller than the read.
//
// No keys for any other tool or input: no dialog can be tied to the call.
func callKey(tool string, input json.RawMessage, cwd string) callKeys {
	if strings.HasPrefix(tool, "mcp__") {
		return mcpKeys(input)
	}
	var in struct {
		Command      string          `json:"command"`
		FilePath     string          `json:"file_path"`
		NotebookPath string          `json:"notebook_path"`
		URL          string          `json:"url"`
		Offset       json.RawMessage `json:"offset"`
		Limit        json.RawMessage `json:"limit"`
		Questions    []struct {
			Question string `json:"question"`
		} `json:"questions"`
	}
	if json.Unmarshal(input, &in) != nil {
		return callKeys{}
	}
	whole := func(k ...string) callKeys { return callKeys{whole: k} }
	p := in.FilePath
	if p == "" {
		p = in.NotebookPath
	}
	if strings.TrimSpace(p) == "" {
		p = ""
	}
	base := filepath.Base(p)
	rel, inside := relTo(cwd, p)
	switch tool {
	case "Bash":
		cmd := strings.Join(strings.Fields(in.Command), " ")
		switch {
		case cmd == "":
			return callKeys{}
		case len([]rune(cmd)) > keyLen:
			return whole("─" + head(cmd, keyLen))
		}
		return whole("─" + cmd + "─")
	case "Edit", "Write":
		if p == "" {
			return callKeys{}
		}
		// Claude Code 2.1.310 heads an Edit's dialog "Edit file" and a
		// Write's by what it does: "Create file" for a new file,
		// "Overwrite file" for an existing one, "Write file" for a network
		// path; the question says the same ("Do you want to create F?",
		// "… overwrite F?", "… write to F?").
		headers, verbs := []string{"Edit file "}, []string{"make this edit to "}
		if tool == "Write" {
			headers, verbs = []string{"Create file ", "Overwrite file ", "Write file "}, []string{"create ", "overwrite ", "write to "}
		}
		var k callKeys
		for _, v := range verbs {
			k.tall = append(k.tall, "Do you want to "+v+base+"?")
		}
		if !inside {
			k.whole = []string{"/" + base + " ─"}
			return k
		}
		for _, h := range headers {
			k.whole = append(k.whole, h+rel+" ─")
		}
		return k
	case "Read":
		switch {
		case p == "":
			return callKeys{}
		case inside || cwd == "" || !filepath.IsAbs(p):
			return whole(base)
		case len(in.Offset) > 0 || len(in.Limit) > 0:
			return whole("Read(" + filepath.Clean(p) + " ·")
		}
		return whole("Read(" + filepath.Clean(p) + ")")
	case "NotebookEdit", "MultiEdit":
		if p == "" {
			return callKeys{}
		}
		return whole(base)
	case "WebFetch":
		if keys := urlKeys(in.URL); len(keys) > 0 {
			return whole(keys...)
		}
	case questionTool:
		if len(in.Questions) > 0 && strings.TrimSpace(in.Questions[0].Question) != "" {
			return whole(head(in.Questions[0].Question, keyLen))
		}
	}
	return callKeys{}
}

// argName is an object key the dialog prints bare, as a plain identifier.
var argName = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

// mcpKeys is the keys of an MCP call (callKey): "(MCP)", which the
// dialog's title ends with, and a key for each usable leaf of the input,
// in the order of a walk with object keys sorted. Numbers and booleans
// add precision but never tie a call to a dialog alone (ruling P21): an
// input without a usable string has no keys, as one with no usable leaf.
// Numbers are kept as written (json.Number), so a whole number is told
// from a float by its text.
func mcpKeys(input json.RawMessage) callKeys {
	if !json.Valid(input) {
		return callKeys{}
	}
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return callKeys{}
	}
	var keys []string
	text := false // a string among the keys
	var walk func(name string, v any)
	walk = func(name string, v any) {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				walk("", e)
			}
		case map[string]any:
			for _, k := range slices.Sorted(maps.Keys(x)) {
				n := ""
				if argName.MatchString(k) {
					n = k
				}
				walk(n, x[k])
			}
		default:
			if val, ok := mcpValue(x); ok {
				_, isString := x.(string)
				text = text || isString
				if name != "" {
					val = name + ":" + val
				}
				if !slices.Contains(keys, val) { // a repeat proves nothing more
					keys = append(keys, val)
				}
			}
		}
	}
	walk("", v)
	if !text {
		return callKeys{}
	}
	return callKeys{every: append([]string{"(MCP)"}, keys...)}
}

// mcpValue is a leaf of an MCP call's input as the dialog prints it, and
// whether it gives a key at all. A string shorter than keyLen runes is in
// full with both quotes, so "Lyon" never finds a dialog that shows "Lyon,
// France"; a longer one is its opening quote and first keyLen runes (how
// Claude Code shortens it is unproven). A whole number (no ".", "e" or
// "E") and true or false are their JSON text. A string with a quote, a
// backslash or a line break, a blank one, a float and a null give none.
func mcpValue(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		switch {
		case strings.TrimSpace(x) == "" || strings.ContainsAny(x, "\"\\\n\r"):
			return "", false
		case len([]rune(x)) < keyLen:
			return `"` + x + `"`, true
		}
		return `"` + clip(x, keyLen), true
	case json.Number:
		if strings.ContainsAny(x.String(), ".eE") {
			return "", false
		}
		return x.String(), true
	case bool:
		return strconv.FormatBool(x), true
	}
	return "", false
}

// hasArg reports whether a dialog's signature shows key, an MCP call's key
// (mcpKeys), compared without whitespace like every key, and standing
// alone: a key that starts with a word character (a letter, a digit, "_",
// "$", ".", "+" or "-") does not follow one there, and one that ends with
// one, unless it holds a string, does not precede one. So "limit:5" is not
// found in "limit:50" or "limit:5.5", "5" not in "15" or "-5", and
// `query:"x"` not in `subquery:"x"`. A string's key ends at its closing
// quote, and a long string's is its start by design: what follows it is
// not looked at.
func hasArg(sig, key string) bool {
	k := strings.Join(strings.Fields(key), "")
	if k == "" {
		return false
	}
	first, _ := utf8.DecodeRuneInString(k)
	last, _ := utf8.DecodeLastRuneInString(k)
	checkBefore := wordRune(first)
	checkAfter := wordRune(last) && !strings.Contains(k, `"`)
	for i := 0; i < len(sig); {
		j := strings.Index(sig[i:], k)
		if j < 0 {
			return false
		}
		j += i
		before, _ := utf8.DecodeLastRuneInString(sig[:j])
		after, _ := utf8.DecodeRuneInString(sig[j+len(k):])
		if (!checkBefore || j == 0 || !wordRune(before)) && (!checkAfter || j+len(k) == len(sig) || !wordRune(after)) {
			return true
		}
		i = j + 1
	}
	return false
}

// wordRune reports whether r can be part of a name or a number as a dialog
// prints it.
func wordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_$.+-", r)
}

// clip is s clipped to n runes.
func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// relTo is path relative to cwd, and whether path lies inside cwd. Without
// a cwd or an absolute path it cannot tell: not inside.
func relTo(cwd, path string) (string, bool) {
	if cwd == "" || !filepath.IsAbs(path) || !filepath.IsAbs(cwd) {
		return "", false
	}
	r, err := filepath.Rel(filepath.Clean(cwd), filepath.Clean(path))
	if err != nil || r == "." || r == ".." || strings.HasPrefix(r, "../") {
		return "", false
	}
	return r, true
}

// urlKeys is what WebFetch's dialog shows of the url: "url: " and the url
// in its WHATWG form (scheme and host lower-cased, no default port, "/" for
// an empty path), then "prompt:". A "'" in the query is "%27" in that form,
// so a url with one has a key for either spelling. A url the dialog may
// spell otherwise (not https, user info, a fragment, a character it would
// percent-encode, a non-ASCII host) is matched by "fetch content from" its
// lower-cased host and the rule under it; a punycode host then simply finds
// no dialog.
func urlKeys(raw string) []string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme != "https" || u.User != nil || u.Fragment != "" || !plainURL(raw) {
		return []string{"fetch content from " + host + " ─"}
	}
	if port := u.Port(); port != "" && port != "443" {
		host += ":" + port
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	key := "https://" + host + path
	if u.RawQuery == "" {
		return []string{"url: " + key + " prompt:"}
	}
	keys := []string{"url: " + key + "?" + u.RawQuery + " prompt:"}
	if strings.Contains(u.RawQuery, "'") {
		keys = append(keys, "url: "+key+"?"+strings.ReplaceAll(u.RawQuery, "'", "%27")+" prompt:")
	}
	return keys
}

// plainURL reports whether raw has only characters a WHATWG url keeps as
// they are: printable ASCII other than space and "\"<>\\^`{|}.
func plainURL(raw string) bool {
	for _, r := range raw {
		if r <= ' ' || r >= 0x7f || strings.ContainsRune("\"<>\\^`{|}", r) {
			return false
		}
	}
	return true
}

// head is s, whitespace runs made single spaces, clipped to n runes.
func head(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
