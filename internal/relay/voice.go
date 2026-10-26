package relay

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/render"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

// Transcriber turns a voice note's audio into text: the owner's
// voice_command. ext is the extension of the file as Telegram names it
// (".oga"). A non-empty transcript comes back on success; a failure's
// error text is shown on the phone as it is.
type Transcriber interface {
	Transcribe(ctx context.Context, audio []byte, ext string) (string, error)
}

const (
	maxVoiceSeconds = 300      // a longer note is refused before download
	maxVoiceBytes   = 20 << 20 // what a bot may download from Telegram
	voiceQueue      = 16       // notes that may wait behind the one transcribed
	voicePreview    = 300      // runes of a too-long transcript shown back
	downloadTimeout = 60 * time.Second
	voiceStopWait   = 5 * time.Second // the relay's wait for the voice worker as it stops
	// signalGrace is how long a note whose command was killed by a signal
	// waits for the relay's own stop before failing on the phone.
	signalGrace = time.Second
)

// The replies to a voice note and other media that need no argument (HTML).
const (
	voiceUnset     = "voice notes need <code>voice_command</code> in tg's config; see the README"
	otherMedia     = "tg reads text and voice notes only"
	voiceTooBig    = "too big for a bot to download (over 20 MB)"
	voiceQueueFull = "too many voice notes waiting; send it again in a minute"
	notRecognized  = "could not transcribe: nothing recognized"
)

// voiceNote takes a voice note from the owner. It is refused at once when
// voice notes are not configured, or the note is too long or too big to
// fetch; otherwise it gets 👀 and waits its turn with the worker
// (transcribeNote), so the update loop never waits on a download or the
// command.
func (r *Router) voiceNote(ctx context.Context, m *telegram.Message) error {
	v := m.Voice
	switch {
	case r.Transcriber == nil:
		return r.reply(ctx, m, voiceUnset)
	case v.Duration > maxVoiceSeconds:
		return r.reply(ctx, m, fmt.Sprintf("too long: %d min, the limit is %d", (v.Duration+59)/60, maxVoiceSeconds/60))
	case v.FileSize > maxVoiceBytes:
		return r.reply(ctx, m, voiceTooBig)
	}
	r.react(ctx, m, "👀")
	if !r.enqueueVoice(ctx, m) {
		err := r.reply(ctx, m, voiceQueueFull)
		r.clearReaction(ctx, m.MessageID)
		return err
	}
	return nil
}

// enqueueVoice queues the note for the worker, starting one with ctx (the
// loop's, which ends as the relay stops) when none runs. It reports false
// when the queue is full.
func (r *Router) enqueueVoice(ctx context.Context, m *telegram.Message) bool {
	r.voiceMu.Lock()
	defer r.voiceMu.Unlock()
	if r.voiceQ == nil {
		r.voiceQ = make(chan *telegram.Message, voiceQueue)
		go r.voiceWorker(ctx, r.voiceQ)
	}
	r.voiceWG.Add(1)
	select {
	case r.voiceQ <- m:
		return true
	default:
		r.voiceWG.Done()
		return false
	}
}

// voiceWorker transcribes the queued notes one at a time, in arrival
// order, until ctx ends. Then it lets go of its queue, drops the note it
// was on and every queued one without a word to the phone (a note queued
// as ctx ended included), logs each, clears their 👀 best-effort, and
// exits. Run never hands the router a note after stopping it; a Handle
// after that would start a new worker on its own ctx.
func (r *Router) voiceWorker(ctx context.Context, q chan *telegram.Message) {
	var dropped []*telegram.Message
	for ctx.Err() == nil {
		select {
		case m := <-q:
			if r.transcribeNote(ctx, m) {
				r.voiceWG.Done()
			} else {
				dropped = append(dropped, m)
			}
		case <-ctx.Done():
		}
	}
	r.voiceMu.Lock()
	r.voiceQ = nil // no note reaches q from here on
	r.voiceMu.Unlock()
	for len(q) > 0 {
		dropped = append(dropped, <-q)
	}
	// ctx is done: the reactions go out on a context of their own, bounded
	// like the relay's wait for this worker.
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), voiceStopWait)
	defer cancel()
	for _, m := range dropped {
		r.logf("voice note %d dropped: relay stopping", m.MessageID)
		r.clearReaction(detached, m.MessageID)
		r.voiceWG.Done()
	}
}

// WaitVoice blocks until every voice note queued so far has been handled:
// confirmed, refused, or dropped because the relay stopped.
func (r *Router) WaitVoice() { r.voiceWG.Wait() }

// transcribeNote downloads the note, runs the Transcriber on it and puts
// the transcript up for confirmation (confirmVoice). A failure replies to
// the note with the reason and clears its 👀. It reports false when ctx
// ended first (the relay stops, which also ends the command): the note is
// dropped without a word, its 👀 left to the worker.
func (r *Router) transcribeNote(ctx context.Context, m *telegram.Message) bool {
	if ctx.Err() != nil {
		return false
	}
	audio, ext, err := r.downloadVoice(ctx, m.Voice)
	if err != nil {
		return r.voiceFailed(ctx, m, "could not download the voice note: "+render.EscapeHTML(err.Error()))
	}
	text, err := r.Transcriber.Transcribe(ctx, audio, ext)
	if killedBySignal(err) {
		// A shutdown signals every process at once, and the command may die
		// of it before the relay's own stop shows: wait for that a moment,
		// so the note is dropped with the rest instead of failing.
		r.sleep(ctx, signalGrace)
	}
	switch {
	case ctx.Err() != nil:
		return false
	case err != nil:
		return r.voiceFailed(ctx, m, "could not transcribe: "+render.EscapeHTML(err.Error()))
	case strings.TrimSpace(text) == "":
		return r.voiceFailed(ctx, m, notRecognized)
	}
	// Refused before any button, so the confirmation always fits a message
	// and shows exactly what Send types.
	if n := r.voiceLen(text); n > maxText {
		preview := []rune(text)
		if len(preview) > voicePreview {
			preview = preview[:voicePreview]
		}
		return r.voiceFailed(ctx, m, fmt.Sprintf("too long: %d characters, the limit is %d; send a shorter note\n<i>%s…</i>", n, maxText, render.EscapeHTML(string(preview))))
	}
	return r.confirmVoice(ctx, m, text)
}

// voiceLen is what a transcript counts against maxText (ruling R31): its
// length as Telegram counts a message, in UTF-16 code units, as typed or
// as shown (credentials redacted, which can grow it), whichever is longer.
// Within it, a confirmation always fits one message: its first line takes
// less than the 96 code units Telegram's limit leaves (maxVoiceName).
func (r *Router) voiceLen(text string) int {
	return max(utf16Len(text), r.shownLen(render.EscapeHTML(text)))
}

// killedBySignal reports whether err says the command was killed by a
// signal it did not get from the relay (voice.Error's Signaled).
func killedBySignal(err error) bool {
	var s interface{ Signaled() bool }
	return errors.As(err, &s) && s.Signaled()
}

// downloadVoice fetches the note's audio within downloadTimeout and
// returns it with its file's extension (voiceExt).
func (r *Router) downloadVoice(ctx context.Context, v *telegram.Voice) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	f, err := r.API.GetFile(ctx, v.FileID)
	if err != nil {
		return nil, "", err
	}
	audio, err := r.API.Download(ctx, f.FilePath, maxVoiceBytes)
	return audio, voiceExt(f.FilePath), err
}

// plainExt is an extension safe in a file name and an environment value.
var plainExt = regexp.MustCompile(`^\.[A-Za-z0-9]{1,8}$`)

// voiceExt is the extension of Telegram's file_path for the note's file,
// which the command sees in TG_VOICE_FILE. The path comes from the server,
// so anything but a plain extension becomes ".ogg", Telegram's own format.
func voiceExt(filePath string) string {
	if ext := path.Ext(filePath); plainExt.MatchString(ext) {
		return ext
	}
	return ".ogg"
}

// confirmVoice parks the transcript and replies to the note with it and
// buttons; nothing is typed or answered before a tap. The target follows
// the rules for text: a swipe-reply to a question or permission prompt
// that still takes a reply answers it (Handle), else target picks the
// session (the pick among its rules), and with none known the session
// keyboard is offered, under a note when the picked session has ended. It reports false, as transcribeNote does,
// when ctx ended first.
func (r *Router) confirmVoice(ctx context.Context, m *telegram.Message, text string) bool {
	key := newKey()
	p := state.Parked{Text: text, MessageID: m.MessageID, Created: r.now(), Voice: true}
	var live []Live
	note := "" // endedNote
	if a, i, ok := r.askReplied(m); ok {
		p.AskID, p.AskIndex = a.ID, i
		p.Header = "🎙 answer to " + bold(voiceName(askName(a))) + ":"
	} else {
		var err error
		if live, err = r.live(ctx); err != nil {
			return r.voiceFailed(ctx, m, "cannot list sessions: "+render.EscapeHTML(err.Error()))
		}
		l, rest, err := r.target(m, text, live)
		switch {
		case errors.Is(err, errNeedTarget):
			note = r.endedNote(err)
			p.Header = withNote(note, voiceWhich)
		case err != nil:
			return r.voiceFailed(ctx, m, err.Error())
		case strings.TrimSpace(rest) == "":
			// Only an address ("@name"): nothing to send.
			return r.voiceFailed(ctx, m, notRecognized)
		default:
			p.Text, p.SessionID = rest, l.SessionID
			p.Header = "🎙 to " + bold(voiceName(l.Name)) + ":"
		}
	}
	kb := voiceKeyboard(key, p, live)
	if untargeted(p) && len(kb) < 2 { // ✖️ Cancel alone: no session to offer
		return r.voiceFailed(ctx, m, withNote(note, noTypeable))
	}
	if err := r.Store.PutPending(key, p); err != nil {
		return r.voiceFailed(ctx, m, "cannot park the message: "+render.EscapeHTML(err.Error()))
	}
	if _, err := r.send(ctx, m.MessageID, p.Header+"\n"+render.EscapeHTML(p.Text), kb); err != nil {
		// Without its message no tap can take the transcript.
		r.Store.TakePending(key, r.now(), pendingTTL)
		if ctx.Err() != nil {
			return false
		}
		r.logf("voice confirmation: %v", err)
		r.clearReaction(ctx, m.MessageID)
		return ctx.Err() == nil
	}
	return true
}

// voiceButton is a confirmation button: callback data "v:<key>:<action>".
func voiceButton(text, key, action string) telegram.Button {
	return telegram.Button{Text: text, Data: "v:" + key + ":" + action}
}

// voiceFailed replies html to a note that gets no confirmation, clears its
// 👀 and reports true. When ctx has ended (the relay stops), before or
// while it does so, it reports false: the worker drops the note and clears
// the 👀 on a ctx of its own.
func (r *Router) voiceFailed(ctx context.Context, m *telegram.Message, html string) bool {
	if ctx.Err() != nil {
		return false
	}
	if err := r.reply(ctx, m, html); err != nil {
		r.logf("voice note %d: %v", m.MessageID, err)
	}
	r.clearReaction(ctx, m.MessageID)
	return ctx.Err() == nil
}

// clearReaction removes the bot's reaction from the owner's message (react
// logs a failure).
func (r *Router) clearReaction(ctx context.Context, messageID int64) {
	r.react(ctx, &telegram.Message{MessageID: messageID}, "")
}
