package relay

import (
	"context"
	"errors"
	"strings"
	"unicode/utf16"

	"github.com/IZZY-Agency/claude-tg/internal/render"
	"github.com/IZZY-Agency/claude-tg/internal/send"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

// The copy of the taps on a parked text that needs no argument.
const (
	voiceWhich       = "🎙 Which session?"       // a voice confirmation's no-target first line (HTML)
	expiredToast     = "expired, send it again" // a tap that finds nothing to send any more
	voiceExpiredEdit = "expired; send it again"
)

const (
	// maxMessage is Telegram's limit on a message's text, in UTF-16 code
	// units of the text as shown: tags dropped, entities decoded.
	maxMessage = 4096
	// maxVoiceName caps a session name in a voice confirmation's header and
	// outcome, in UTF-16 code units, so at most 64 runes (ruling R22): with
	// the transcript within maxText (voiceLen), they fit (ruling R31).
	maxVoiceName = 64
)

// voiceName is a session name as a voice confirmation shows it (plain).
func voiceName(name string) string { return clipUTF16(name, maxVoiceName) }

// utf16Len is the length of s in UTF-16 code units, as Telegram counts it.
func utf16Len(s string) int {
	n := 0
	for _, c := range s {
		n += utf16.RuneLen(c)
	}
	return n
}

// clipUTF16 cuts s to at most limit UTF-16 code units, the last one "…".
func clipUTF16(s string, limit int) string {
	if utf16Len(s) <= limit {
		return s
	}
	n := 0
	for i, c := range s {
		if n+utf16.RuneLen(c) > limit-1 {
			return s[:i] + "…"
		}
		n += utf16.RuneLen(c)
	}
	return s
}

// voiceTap handles a voice confirmation's own buttons, callback data
// "v:<key>:<action>": ✅ Send (y), ✖️ Cancel (n) and ↪ Other session (o).
// The tap takes the parked transcript, so two taps can never send it twice;
// a refused Send puts it back. The caller has checked the sender. Every
// path answers the tap once, after its edit.
func (r *Router) voiceTap(ctx context.Context, cq *telegram.CallbackQuery) error {
	parts := strings.Split(cq.Data, ":")
	if len(parts) != 3 || parts[1] == "" || (parts[2] != "y" && parts[2] != "n" && parts[2] != "o") {
		r.answerCallback(ctx, cq, "unknown button")
		return nil
	}
	key := parts[1]
	p, ok := r.Store.TakePending(key, r.now(), pendingTTL)
	if !ok {
		r.voiceExpired(ctx, cq, p)
		return nil
	}
	switch parts[2] {
	case "y":
		if p.AskID != "" {
			r.voiceAnswer(ctx, cq, p)
			return nil
		}
		live, err := r.voiceLive(ctx, cq, key, p)
		if err != nil {
			return err
		}
		if untargeted(p) {
			// Its message still showed ✅ Send (ruling R20): offer the
			// sessions again.
			r.voiceShowWhich(ctx, cq, key, p, p, live, "pick a session")
			return nil
		}
		r.voiceDeliver(ctx, cq, key, p, bySession(live, p.SessionID), live)
	case "n":
		r.voiceEdit(ctx, cq, "✖️ not sent\n"+render.EscapeHTML(p.Text), nil)
		r.clearReaction(ctx, p.MessageID)
		r.answerCallback(ctx, cq, "cancelled")
	case "o":
		live, err := r.voiceLive(ctx, cq, key, p)
		if err != nil {
			return err
		}
		q := p
		q.SessionID, q.AskID, q.AskIndex, q.Header = "", "", 0, voiceWhich
		r.voiceShowWhich(ctx, cq, key, p, q, live, "")
	}
	return nil
}

// voicePick is a session button on a voice confirmation (its no-target
// form, first or made by ↪ Other session), callback data
// "s:<sessionID>:<key>": the tap is the confirmation, and the transcript
// goes to that session as Send sends it. ok is TakePending's.
func (r *Router) voicePick(ctx context.Context, cq *telegram.CallbackQuery, sessionID, key string, p state.Parked, ok bool) error {
	if !ok {
		r.voiceExpired(ctx, cq, p)
		return nil
	}
	live, err := r.voiceLive(ctx, cq, key, p)
	if err != nil {
		return err
	}
	r.voiceDeliver(ctx, cq, key, p, bySession(live, sessionID), live)
	return nil
}

// voiceLive lists the live sessions for a tap. When it cannot, the
// transcript is parked again and the confirmation left as it was, buttons
// and all, for another try; the tap is answered and the error returned for
// the log.
func (r *Router) voiceLive(ctx context.Context, cq *telegram.CallbackQuery, key string, p state.Parked) ([]Live, error) {
	live, err := r.live(ctx)
	if err != nil {
		r.voicePark(ctx, cq, key, p)
		r.answerCallback(ctx, cq, "cannot list sessions")
	}
	return live, err
}

// voiceDeliver types the transcript into session l (nil: it has ended)
// through deliver, with all its refusals, marking the session once the
// text is submitted so that its next answer is spoken (on a failed submit
// too, as its owed answers stay). Delivered, the note gets 👍 and
// the confirmation shows the outcome without buttons; refused, the
// transcript is parked again and the confirmation says why and keeps its
// buttons, so the owner can tap again until it expires. A failed submit
// may have gone through (submitUnknown): the transcript is not parked
// again, as a second Send could submit it twice (ruling R21); the
// confirmation says to check the session, without buttons, and the note's
// 👀 is cleared.
func (r *Router) voiceDeliver(ctx context.Context, cq *telegram.CallbackQuery, key string, p state.Parked, l *Live, live []Live) {
	if l == nil {
		r.voiceRefused(ctx, cq, key, p, "that session is gone", voiceKeyboard(key, p, live))
		return
	}
	// deliver's refusals name the session as the confirmation does.
	shown := *l
	shown.Name = voiceName(l.Name)
	queued, err := r.deliver(ctx, &shown, p.Text, true)
	var unknown *submitUnknown
	if errors.As(err, &unknown) {
		// The session is named once; the toast leads with the warning, so
		// its clip never cuts that off.
		why := "could not submit: " + clipText(unknown.cause.Error(), voicePreview)
		r.voiceOutcome(ctx, cq, "✗ "+render.EscapeHTML(why)+" — the text may have been sent; check "+bold(shown.Name), p, nil)
		r.clearReaction(ctx, p.MessageID)
		r.answerCallback(ctx, cq, "the text may have been sent — check "+shown.Name+" ("+why+")")
		return
	}
	if err != nil {
		r.voiceRefused(ctx, cq, key, p, err.Error(), voiceKeyboard(key, p, live))
		return
	}
	r.react(ctx, &telegram.Message{MessageID: p.MessageID}, "👍")
	head := "→ " + bold(shown.Name)
	if queued {
		head += " · queued (busy)" // the session is named once (ruling R26)
	}
	r.voiceEdit(ctx, cq, head+"\n"+render.EscapeHTML(p.Text), nil)
	r.answerCallback(ctx, cq, "sent to "+shown.Name)
}

// voiceAnswer is ✅ Send on the confirmation of a swipe-reply to a
// question or permission prompt: the transcript answers it exactly as the
// typed reply would (answerAsk), and when that completes the ask the
// session is marked, so that its next answer is spoken. A refusal (the
// prompt closed, was answered or is gone meanwhile) is final: an answer
// is not retried, so the buttons go and the note's 👀 is cleared.
func (r *Router) voiceAnswer(ctx context.Context, cq *telegram.CallbackQuery, p state.Parked) {
	a, toast, ok := r.answerAsk(ctx, p.AskID, p.AskIndex, p.Text, true)
	if !ok {
		r.voiceOutcome(ctx, cq, "✗ "+render.EscapeHTML(toast), p, nil)
		r.clearReaction(ctx, p.MessageID)
		r.answerCallback(ctx, cq, toast)
		return
	}
	r.react(ctx, &telegram.Message{MessageID: p.MessageID}, "👍")
	r.voiceEdit(ctx, cq, "→ "+bold(voiceName(askName(a)))+" · answered\n"+render.EscapeHTML(p.Text), nil)
	r.answerCallback(ctx, cq, toast)
}

// voiceShowWhich edits the confirmation into the no-target form of q (its
// session keyboard and ✖️ Cancel) and parks q under the same key; the tap
// is answered with toast. When no session can be offered or the message
// not changed, the confirmation stays as it was and orig, the record it
// shows, is parked again instead. ↪ Other session gives orig with its
// target and q without; a Send on a record without a target gives the same
// record twice.
func (r *Router) voiceShowWhich(ctx context.Context, cq *telegram.CallbackQuery, key string, orig, q state.Parked, live []Live, toast string) {
	kb := voiceKeyboard(key, q, live)
	if len(kb) < 2 { // ✖️ Cancel alone: no session to offer
		r.voicePark(ctx, cq, key, orig)
		r.answerCallback(ctx, cq, noTypeable)
		return
	}
	if err := r.voiceEdit(ctx, cq, voiceText(q), kb); err != nil {
		r.voicePark(ctx, cq, key, orig)
		r.answerCallback(ctx, cq, "could not show the sessions; try again")
		return
	}
	if !r.voicePark(ctx, cq, key, q) {
		toast = "cannot park the message"
	}
	r.answerCallback(ctx, cq, toast)
}

// voiceExpired answers a tap that found no transcript to take. One that
// expired (TakePending hands it out a last time, stale) loses its buttons
// and the note's 👀; its transcript stays on the message, to copy (ruling
// R25). One already taken by an earlier tap leaves the message alone
// (ruling R16): that tap's outcome is on it, and "expired" must not
// replace "delivered".
func (r *Router) voiceExpired(ctx context.Context, cq *telegram.CallbackQuery, p state.Parked) {
	if !p.Created.IsZero() {
		r.voiceEdit(ctx, cq, voiceExpiredEdit+"\n"+render.EscapeHTML(p.Text), nil)
		if p.MessageID != 0 {
			r.clearReaction(ctx, p.MessageID)
		}
	}
	r.answerCallback(ctx, cq, expiredToast)
}

// voiceRefused parks the transcript again and edits the confirmation to
// "✗ <reason>" over its text, keeping keyboard kb (voiceOutcome); it
// toasts the reason. reason is HTML.
func (r *Router) voiceRefused(ctx context.Context, cq *telegram.CallbackQuery, key string, p state.Parked, reason string, kb [][]telegram.Button) {
	if r.voicePark(ctx, cq, key, p) {
		r.voiceOutcome(ctx, cq, "✗ "+reason, p, kb)
	}
	r.answerCallback(ctx, cq, stripTags(reason))
}

// voiceOutcome edits the confirmation of p to line (HTML) over its text,
// with keyboard kb (nil: the buttons go). When that would not fit one
// Telegram message (ruling R22), the confirmation keeps its text, and its
// buttons too when kb is set, and line goes out as a reply to it instead,
// cut as plain text when it would not fit a message of its own either (a
// backend's huge error).
func (r *Router) voiceOutcome(ctx context.Context, cq *telegram.CallbackQuery, line string, p state.Parked, kb [][]telegram.Button) {
	html := line + "\n" + voiceText(p)
	if r.fits(html) {
		r.voiceEdit(ctx, cq, html, kb)
		return
	}
	if !r.fits(line) {
		line = r.cutToFit(line)
	}
	id := cq.Message.MessageID
	if kb == nil {
		if err := r.API.EditReplyMarkup(ctx, r.ChatID, id, nil); err != nil {
			r.logf("voice confirmation %d: drop the buttons: %v", id, err)
		}
	}
	if _, err := r.send(ctx, id, line, nil); err != nil {
		r.logf("voice confirmation %d: reply: %v", id, err)
	}
}

// cutToFit cuts html's text, as plain text, until what the relay sends of
// it fits one message: measured as sent, since redaction can grow it
// (key=abcdefgh becomes key=[redacted]).
func (r *Router) cutToFit(html string) string {
	plain := render.StripHTML(html)
	for n := maxText; ; n = n * 9 / 10 {
		cut := render.EscapeHTML(clipUTF16(plain, n))
		if r.fits(cut) || n < 2 {
			return cut
		}
	}
}

// fits reports whether html fits one Telegram message (maxMessage).
func (r *Router) fits(html string) bool { return r.shownLen(html) <= maxMessage }

// shownLen is the length Telegram counts for html as the relay sends it
// (secrets scrubbed, credentials redacted): UTF-16 code units of its text,
// tags dropped and entities decoded.
func (r *Router) shownLen(html string) int {
	return utf16Len(render.StripHTML(render.RedactHTML(r.scrub(html))))
}

// voicePark puts the transcript back under its key, with its original
// Created so retries never extend its expiry, and reports whether it
// could. When it cannot, no button can send it any more: the confirmation
// says so without buttons and the note's 👀 is cleared.
func (r *Router) voicePark(ctx context.Context, cq *telegram.CallbackQuery, key string, p state.Parked) bool {
	err := r.Store.PutPending(key, p)
	if err == nil {
		return true
	}
	r.logf("voice: park the transcript again: %v", err)
	r.voiceOutcome(ctx, cq, "✗ cannot park the message: "+render.EscapeHTML(err.Error()), p, nil)
	r.clearReaction(ctx, p.MessageID)
	return false
}

// untargeted reports whether the transcript waits for a session tap: no
// session and no prompt to answer.
func untargeted(p state.Parked) bool { return p.SessionID == "" && p.AskID == "" }

// voiceKeyboard is the confirmation's keyboard for the transcript's
// target: ✅ Send and ✖️ Cancel for an answer to a prompt, plus ↪ Other
// session for a known session; untargeted, the session keyboard of live
// and ✖️ Cancel (Cancel alone when no session is typeable).
func voiceKeyboard(key string, p state.Parked, live []Live) [][]telegram.Button {
	sendBtn, cancelBtn := voiceButton("✅ Send", key, "y"), voiceButton("✖️ Cancel", key, "n")
	switch {
	case p.AskID != "":
		return [][]telegram.Button{{sendBtn, cancelBtn}}
	case p.SessionID != "":
		return [][]telegram.Button{{sendBtn, cancelBtn}, {voiceButton("↪ Other session", key, "o")}}
	}
	return append(KeyboardFor(live, key), []telegram.Button{cancelBtn})
}

// voiceText is the confirmation's text: its first line, then the
// transcript (HTML).
func voiceText(p state.Parked) string {
	return p.Header + "\n" + render.EscapeHTML(p.Text)
}

// voiceEdit replaces the confirmation's text with html: with kb nil its
// buttons go (edit), else they become kb, the texts cleaned as every
// keyboard the relay sends. A failure is logged and returned.
func (r *Router) voiceEdit(ctx context.Context, cq *telegram.CallbackQuery, html string, kb [][]telegram.Button) error {
	var err error
	if kb == nil {
		err = r.edit(ctx, cq.Message.MessageID, html)
	} else {
		err = send.EditHTML(ctx, r.API, r.ChatID, cq.Message.MessageID, r.scrub(html), r.cleanKeyboard(kb))
	}
	if err != nil {
		r.logf("voice confirmation %d: edit: %v", cq.Message.MessageID, err)
	}
	return err
}
