package relay

import (
	"context"
	"errors"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

// Telegram hands a bot's updates to one getUpdates reader at a time. A
// second program polling with the same token (the Claude Code Telegram
// channel plugin left enabled, or a second relay, often on another
// machine) takes over each time it polls, and the relay's pending poll
// ends with 409 "terminated by other getUpdates request". The two keep
// knocking each other off, so the relay's polls still succeed now and
// then, and each message reaches whichever program holds the poll at that
// moment: what the other one gets never reaches a session.
const (
	conflictAlertAfter = time.Minute      // conflicting polls this long: alert
	conflictQuiet      = 2 * time.Minute  // no conflict this long: over
	errRepeatEvery     = 10 * time.Minute // a repeating poll error is logged again this often
)

// pollHealth follows one Run's polls. A repeating error is logged once,
// then as a count every errRepeatEvery and when it stops. A conflict
// episode begins with the first conflict, is recorded and alerted once it
// has lasted conflictAlertAfter, and is over at the first successful poll
// conflictQuiet after the latest conflict; the polls that succeed in
// between do not end it. A message counts as sent only once Telegram took
// it; a failed one is tried again after notifyRetry, doubling up to
// notifyRetryMax and never before Telegram's retry_after. Even a refusal
// such as 403 is retried that slowly: then nothing else reaches the user
// either, and it heals by itself. A failed save of the record is tried
// again at every poll, whatever the poll brings.
type pollHealth struct {
	lastErr  string    // the error that repeats; "" after a successful poll
	repeats  int       // times lastErr came again since it was last logged
	loggedAt time.Time // when lastErr was last logged

	since    time.Time // first conflict of the episode; zero: none
	last     time.Time // latest conflict
	alerted  bool      // the alert reached the user
	retryAt  time.Time // no alert or all-clear before this, after one that failed
	failures int       // failed sends in a row
	dirty    bool      // the record in rs did not reach relay.json
}

const (
	notifyRetry    = 30 * time.Second
	notifyRetryMax = 10 * time.Minute
)

// sendFailed paces the next try after a failed alert or all-clear and
// says how long it waits. now is when the failure came back, not when
// the send began: retry_after counts from the answer.
func (h *pollHealth) sendFailed(now time.Time, err error) time.Duration {
	wait := notifyRetryMax
	if h.failures < 5 {
		wait = min(notifyRetry<<h.failures, notifyRetryMax)
	}
	var api *telegram.APIError
	if errors.As(err, &api) && time.Duration(api.RetryAfter)*time.Second > wait {
		wait = time.Duration(api.RetryAfter) * time.Second
	}
	h.failures++
	h.retryAt = now.Add(wait)
	return wait
}

// startHealth picks up the previous run's episode: no second alert while
// it goes on, and the all-clear once it ends, even when it ended while the
// relay was down. Its quiet time counts from now.
func (l *Loop) startHealth(rs state.RelayState) pollHealth {
	if rs.PollConflictSince.IsZero() {
		return pollHealth{}
	}
	return pollHealth{since: rs.PollConflictSince, last: l.Router.now(), alerted: rs.PollConflictAlerted}
}

func (l *Loop) pollFailed(ctx context.Context, h *pollHealth, rs *state.RelayState, err error) {
	l.retrySave(h, rs)
	now := l.Router.now()
	if msg := err.Error(); msg == h.lastErr {
		h.repeats++
		if now.Sub(h.loggedAt) >= errRepeatEvery {
			l.logRepeats(h)
			h.loggedAt = now
		}
	} else {
		l.logRepeats(h)
		l.logf("getUpdates: %v", err)
		h.lastErr, h.loggedAt = msg, now
	}
	if !telegram.PollConflict(err) {
		return
	}
	if h.since.IsZero() {
		h.since = now
	}
	h.last = now
	if now.Sub(h.since) < conflictAlertAfter {
		return
	}
	if rs.PollConflictSince.IsZero() {
		rs.PollConflictSince = h.since
		l.saveHealth(h, rs)
	}
	if h.alerted || now.Before(h.retryAt) {
		return
	}
	if _, err := l.Router.send(ctx, 0, conflictAlert(h.since, now), nil); err != nil {
		l.logf("poll conflict alert: %v (trying again in %s)", err, h.sendFailed(l.Router.now(), err))
		return
	}
	h.alerted, rs.PollConflictAlerted = true, true
	h.failures, h.retryAt = 0, time.Time{}
	l.saveHealth(h, rs)
	l.logf("another program has been reading this bot's updates since %s; told the user", h.since.Local().Format(time.TimeOnly))
}

func (l *Loop) pollSucceeded(ctx context.Context, h *pollHealth, rs *state.RelayState) {
	l.retrySave(h, rs)
	l.logRepeats(h)
	h.lastErr = ""
	now := l.Router.now()
	if h.since.IsZero() || now.Sub(h.last) < conflictQuiet {
		return
	}
	if h.alerted {
		if now.Before(h.retryAt) {
			return
		}
		if _, err := l.Router.send(ctx, 0, conflictOver, nil); err != nil {
			l.logf("poll conflict all-clear: %v (trying again in %s)", err, h.sendFailed(l.Router.now(), err))
			return
		}
		l.logf("the other program stopped reading this bot's updates; told the user")
	}
	h.since, h.last, h.alerted, h.retryAt, h.failures = time.Time{}, time.Time{}, false, time.Time{}, 0
	if !rs.PollConflictSince.IsZero() || rs.PollConflictAlerted {
		rs.PollConflictSince, rs.PollConflictAlerted = time.Time{}, false
		l.saveHealth(h, rs)
	}
}

// saveHealth writes the record; a failure marks it for retrySave.
func (l *Loop) saveHealth(h *pollHealth, rs *state.RelayState) {
	if err := l.Store.SaveRelay(*rs); err != nil {
		if !h.dirty {
			l.logf("cannot save the poll conflict record (%v); trying again at each poll", err)
		}
		h.dirty = true
		return
	}
	h.dirty = false
}

func (l *Loop) retrySave(h *pollHealth, rs *state.RelayState) {
	if h.dirty {
		l.saveHealth(h, rs)
	}
}

// logRepeats logs how often the repeating error came since it was last
// logged, if it did.
func (l *Loop) logRepeats(h *pollHealth) {
	if h.repeats == 0 {
		return
	}
	l.logf("getUpdates: %s (%d more time(s) since %s)", h.lastErr, h.repeats, h.loggedAt.Local().Format(time.TimeOnly))
	h.repeats = 0
}

func conflictAlert(since, now time.Time) string {
	return "⚙️ Another program is reading this bot's updates (since " + clockAt(since, now) + "). " +
		"Telegram gives them to one reader at a time, so your replies, voice notes and button taps may not reach tg.\n" +
		"Turn the other one off, on this machine or another: usually the Claude Code Telegram plugin " +
		"(<code>claude plugin disable telegram@claude-plugins-official</code>) or a second <code>tg relay</code> with this bot's token."
}

const conflictOver = "⚙️ The other program stopped reading this bot's updates: your replies reach tg again."

// clockAt is t's local time of day, with the date when it is not today.
func clockAt(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}
