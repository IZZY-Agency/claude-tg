package hooks

import (
	"context"
	"os"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/transcript"
)

// phoneTurnTTL is how long a turn the phone started counts as one after
// its prompt (ruling P16): an owner who sent a task from the phone an hour
// ago may well be back at the Mac, and the turn's prompts then wait for
// the idle time as a Mac turn's do.
const phoneTurnTTL = time.Hour

// Claude Code 2.1.310 runs the Stop hook before it writes the turn's
// entries to the transcript, and writes them about 60 ms later while the
// hook runs (measured 2026-10-26). The Stop hook reads the transcript
// again every answerPoll, at most answerPolls times, until the answer is
// there, sleeping no longer than answerWait in all (stopPrompt).
const (
	answerPoll  = 50 * time.Millisecond
	answerPolls = 40
	answerWait  = answerPolls * answerPoll
)

// promptReader reads the prompt of a transcript's turn: its last prompt
// (transcript.LastPrompt), or for the Stop hook the prompt before its
// answer (transcript.PromptBefore).
type promptReader func(path string) (transcript.Prompt, bool, error)

// phoneCheck reports whether the turn the hook belongs to started from the
// phone, at now: the transcript's last prompt is a line the relay recorded
// in sess (state.Session.SentFromPhone), written no more than phoneTurnTTL
// before now, and no prompt of the session was answered at the Mac since
// (state.Session.MacAnswerAt). An owner who typed from the phone is not at
// the Mac, whatever its idle clock says, so the away gate does not apply
// to the turn's alerts and buttons. A session without phone lines is not
// phone-started, and its transcript is not read. A nil session, an
// unreadable transcript or no prompt in it all mean not phone-started:
// the gate stays.
//
// When a session holding phone lines is not phone-started, why says so
// for the log, never with the prompt's text; it is "" otherwise.
func phoneCheck(in *Input, sess *state.Session, now time.Time, read promptReader) (ok bool, why string) {
	_, ok, why = phonePrompt(in, sess, now, read)
	return ok, why
}

// phonePrompt is phoneCheck returning the phone's prompt too: p is the
// prompt read found when ok, the zero Prompt otherwise. The Stop hook
// reads the prompt before its answer (Deps.ReadPromptBefore), any other
// hook the last one.
func phonePrompt(in *Input, sess *state.Session, now time.Time, read promptReader) (p transcript.Prompt, ok bool, why string) {
	if in == nil || sess == nil || len(sess.PhoneLines) == 0 {
		return transcript.Prompt{}, false, ""
	}
	if in.TranscriptPath == "" {
		return transcript.Prompt{}, false, "transcript: none in the hook's input"
	}
	p, found, err := read(in.TranscriptPath)
	switch {
	case err != nil:
		why = "transcript: " + err.Error()
	case !found && p.Unfinished:
		why = "last line still being written"
	case !found && p.Pending:
		why = "answer not in the transcript"
	case !found:
		why = "no prompt found"
	case !sess.SentFromPhone(p.Text, p.At):
		why = "last prompt was not sent from the phone"
	case now.Sub(p.At) > phoneTurnTTL:
		why = "phone prompt is over an hour old"
	case sess.MacAnswerAt.After(p.At):
		why = "answered at the Mac since"
	default:
		return p, true, ""
	}
	return transcript.Prompt{}, false, why
}

// readPrompt is Deps.ReadPrompt, transcript.LastPrompt when unset.
func (d Deps) readPrompt() promptReader {
	if d.ReadPrompt != nil {
		return d.ReadPrompt
	}
	return transcript.LastPrompt
}

// readPromptBefore is Deps.ReadPromptBefore, transcript.PromptBefore when
// unset.
func (d Deps) readPromptBefore() func(path, answer string) (transcript.Prompt, bool, error) {
	if d.ReadPromptBefore != nil {
		return d.ReadPromptBefore
	}
	return transcript.PromptBefore
}

// stopPrompt reads the prompt of the turn that gave the Stop hook's answer
// (readPromptBefore). Claude Code may not have written the answer yet, so
// while it is not in the transcript (Pending) or the last line is still
// being written (Unfinished), the file is read again every answerPoll, at
// most answerPolls times, and only when it grew since the last read: a
// file that did not change gives the same result. It waits only while that
// can matter: the phone gets the answer (sess.Follow, or one is owed) and
// sess holds a phone line that no answer took yet whose prompt may be
// within phoneTurnTTL (unspentPhoneLine); otherwise one read decides.
//
// Its sleeps end answerWait after the wait began, or d.TextReserve before
// ctx does if that is sooner, as speech ends there (speakAnswer) so the
// text keeps its time. The stat and read after the last sleep may still
// run past that point, as a read cannot be cancelled: one PromptBefore
// over at most its 8 MiB window took 13 to 36 ms on a 28 MB transcript
// (measured 2026-10-26), against a reserve of seconds. The Stop hook runs
// async, so a late one may overlap the next turn, whose entries can push
// the answer out of that window; answerWait bounds what that costs.
func (d Deps) stopPrompt(ctx context.Context, sess *state.Session, path, answer string) (transcript.Prompt, bool, error) {
	read := d.readPromptBefore()
	size := func() int64 {
		fi, err := os.Stat(path)
		if err != nil {
			return -1
		}
		return fi.Size()
	}
	last := size()
	p, ok, err := read(path, answer)
	if ok || err != nil || !(p.Pending || p.Unfinished) || !(sess.Follow || sess.Owed() > 0) || !unspentPhoneLine(sess, d.now()) {
		return p, ok, err
	}
	if deadline, has := ctx.Deadline(); has && d.TextReserve > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline.Add(-d.TextReserve))
		defer cancel()
	}
	ctx, cancel := context.WithTimeout(ctx, answerWait)
	defer cancel()
	for i := 0; i < answerPolls && !ok && err == nil && (p.Pending || p.Unfinished); i++ {
		if ctx.Err() != nil || !d.sleep(ctx, answerPoll) {
			break
		}
		n := size()
		if n >= 0 && n == last {
			continue
		}
		last = n
		p, ok, err = read(path, answer)
	}
	return p, ok, err
}

// unspentPhoneLine reports whether sess holds a phone line no answer took
// yet (state.PhoneLine.Answered) whose turn may still count as the
// phone's: its prompt, written by LastPromptAt at the latest (a queued
// line can be taken in up to an hour after its record), is within
// phoneTurnTTL of now, as phonePrompt measures it.
func unspentPhoneLine(sess *state.Session, now time.Time) bool {
	for _, l := range sess.PhoneLines {
		if !l.Answered && now.Sub(l.LastPromptAt()) <= phoneTurnTTL {
			return true
		}
	}
	return false
}

// recentPhoneLine reports whether sess's newest phone line was recorded
// within phoneTurnTTL before now.
func recentPhoneLine(sess *state.Session, now time.Time) bool {
	n := len(sess.PhoneLines)
	return n > 0 && now.Sub(sess.PhoneLines[n-1].At) <= phoneTurnTTL
}

// phoneStarted is phoneCheck at d.now(), or Deps.FromPhone's answer, which
// gives no reason.
func (d Deps) phoneStarted(in *Input, sess *state.Session) (bool, string) {
	if d.FromPhone != nil {
		return d.FromPhone(in, sess), ""
	}
	return phoneCheck(in, sess, d.now(), d.readPrompt())
}

// logPhone logs why a turn of a session holding phone lines is not
// phone-started (phoneCheck); nothing when why is "".
func (d Deps) logPhone(sessionID, why string) {
	if why != "" {
		d.logf("phone check (%s): %s", sessionID, why)
	}
}
