package hooks

import (
	"context"
	"time"
	"unicode/utf8"

	"github.com/IZZY-Agency/claude-tg/internal/speech"
	"github.com/IZZY-Agency/claude-tg/internal/state"
)

// Speaker turns speakable text into an OGG/Opus voice message: the owner's
// speech_command (voice.Runner.Speak). lang is speech.Language's guess.
type Speaker interface {
	Speak(ctx context.Context, text, lang string) ([]byte, error)
}

// maxSpeechWait is the longest the text waits for speech: the spec's
// 30 second cap, over TypeSafe's judgment, the speech command, the voice
// upload, its pacing and its 429 retries together.
const maxSpeechWait = 30 * time.Second

// speechWindow is d.SpeechWindow, maxSpeechWait when unset.
func (d Deps) speechWindow() time.Duration {
	if d.SpeechWindow > 0 {
		return d.SpeechWindow
	}
	return maxSpeechWait
}

// speakAnswer sends a short answer as a voice message, for the Stop hook to
// follow with the text: an answer to a question the owner asked by voice
// (a voice note, or a prompt answered by voice), or, with judge set, a
// typed phone answer voiceCheck passes. The speech command gets the text a
// 💬 message would show (the bot token scrubbed, credentials redacted),
// made speakable. It never fails the hook: whatever goes wrong only costs
// the voice, which is never spooled, and the reason goes to the log
// (cleaned like a message, as is the session name), never the answer's
// text.
//
// The hard rules (speech.Choose) come first, with no network and before
// any wait, so they always decide what they can: an answer holding a
// fenced code block is text, and so is one of which neither the whole nor
// the opening paragraph is short prose (a table rules out the whole answer
// only); a long answer's opening paragraph, with the next one after a
// short lead-in, is then all the voice says. An answer to a question asked
// by voice is spoken on the hard rules alone (owner's ruling, 2026-10-26).
// With judge set, what passes goes to TypeSafe's judgment (voiceCheck,
// Deps.Classifier) with question, the phone's prompt cleaned as a message
// is ("" when it is not to be sent). The voice speaks the text's own
// language, or lang, the question's, when the text has too few letters to
// tell ("2 + 2 = 4.": speech.LanguageOr), with the symbols voices read out
// rewritten (speech.Voiced). Each decision is logged once
// (Deps.logVoice), never with a text.
//
// Time: everything here ends d.TextReserve before ctx does, so the text
// is sent rather than starved into the spool; with no more time than that
// left, nothing is spoken. The spool is flushed first, and while older
// messages are still undelivered nothing is spoken either, and TypeSafe
// is not asked about a voice that would not go: the voice would overtake
// them, while the text queues behind them. voiceCheck (TypeSafe's
// judgment, at most classify.DefaultTimeout), the speech command and the
// voice upload, with its pacing and retries, then share one window
// (Deps.SpeechWindow, maxSpeechWait by default), so the text never waits
// longer than that for them: 0.4.0's bound for a spoken answer.
func speakAnswer(ctx context.Context, d Deps, sess *state.Session, answer, question, lang string, judge bool) {
	answer = d.Sender.Clean(answer)
	choice := speech.Choose(answer)
	if choice.Reason != "" {
		d.logVoice(sess, "text ("+choice.Reason+")")
		return
	}
	text := choice.Text
	fail := func(reason string) { d.logf("speech: %s", d.Sender.Clean(reason)) }
	if deadline, ok := ctx.Deadline(); ok && d.TextReserve > 0 {
		end := deadline.Add(-d.TextReserve)
		if !time.Now().Before(end) { // the context's clock, not d.now
			fail("no time left before the text")
			return
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, end)
		defer cancel()
	}
	d.Sender.FlushSpool(ctx, 10)
	if backlog, err := d.Store.SpoolList(); err != nil || len(backlog) > 0 {
		reason := "older messages are still undelivered"
		if err != nil {
			reason = err.Error()
		}
		fail(reason)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, d.speechWindow())
	defer cancel()
	switch {
	case judge && !voiceCheck(ctx, d, sess, choice, question):
		return
	case !judge && choice.Opening:
		d.logVoice(sess, "voice (asked by voice; opening paragraph)")
	case !judge:
		d.logVoice(sess, "voice (asked by voice)")
	}
	spoken := speech.LanguageOr(text, lang)
	audio, err := d.Speaker.Speak(ctx, speech.Voiced(text, spoken), spoken)
	var dur time.Duration
	if err == nil {
		dur, err = speech.Duration(audio)
	}
	if err == nil {
		_, err = d.Sender.SendVoice(ctx, sess.SessionID, sess.Name, audio, dur, true)
	}
	if err != nil {
		fail(err.Error())
		return
	}
	d.logf("spoke %d characters (%.1f s) for %s", utf8.RuneCountInString(text), dur.Seconds(), d.Sender.Clean(spokenName(sess)))
}

// spokenName is the session's name as a voice's caption shows it
// (send.Sender.SendVoice): "mac" for a session with none.
func spokenName(sess *state.Session) string {
	if sess.Name == "" {
		return "mac"
	}
	return sess.Name
}
