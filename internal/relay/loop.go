package relay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

// Poller is the part of the Telegram client the loop uses.
type Poller interface {
	GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]telegram.Update, error)
}

// Loop long-polls Telegram and hands each update to the router.
type Loop struct {
	Poll   Poller
	Router *Router
	Store  *state.Store
	Once   bool          // handle what is pending, then return
	MaxAge time.Duration // updates older than this on start are skipped
	Log    func(string, ...any)

	stopWait time.Duration // bound on the wait for the voice worker once Run stops it; 0: voiceStopWait
}

const (
	longPoll      = 50 // seconds
	pollBackoff   = 5 * time.Second
	defaultMaxAge = 10 * time.Minute
)

func (l *Loop) logf(format string, args ...any) {
	if l.Log != nil {
		l.Log(format, args...)
	}
}

// updateTime is when an update was sent, or zero when unknown. A callback
// carries only its message's date: when the message with the button was
// sent, not when it was tapped. So the relay's own taps have no usable
// time, and what they act on decides instead: an ask tap ("a:") its ask,
// which says whether it still waits and takes an answer once; a voice
// confirmation's button ("v:") and a session pick ("s:") the parked text,
// whose pendingTTL counts from the same moment as the message's date
// (ruling R19), so an expired one still gets its expired outcome; a
// button of /sessions ("p:") the session it names, which picks it only
// while it is live.
func updateTime(u telegram.Update) time.Time {
	switch {
	case u.CallbackQuery != nil && undatedTap(u.CallbackQuery.Data):
		return time.Time{}
	case u.Message != nil && u.Message.Date != 0:
		return time.Unix(u.Message.Date, 0)
	case u.CallbackQuery != nil && u.CallbackQuery.Message != nil && u.CallbackQuery.Message.Date != 0:
		return time.Unix(u.CallbackQuery.Message.Date, 0)
	}
	return time.Time{}
}

// undatedTap reports whether callback data is one of the relay's own
// taps, which updateTime leaves undated.
func undatedTap(data string) bool {
	for _, prefix := range []string{"a:", "v:", "s:", pickData} {
		if strings.HasPrefix(data, prefix) {
			return true
		}
	}
	return false
}

// PartitionStale splits updates into those younger than maxAge and the rest.
// Updates without a usable timestamp count as fresh.
func PartitionStale(ups []telegram.Update, now time.Time, maxAge time.Duration) (fresh, stale []telegram.Update) {
	for _, u := range ups {
		t := updateTime(u)
		if !t.IsZero() && now.Sub(t) > maxAge {
			stale = append(stale, u)
			continue
		}
		fresh = append(fresh, u)
	}
	return fresh, stale
}

// Run holds the single-instance lock and polls until ctx ends (or, with
// Once, until the first batch is handled). Every update is claimed (offset
// saved) before it is acted on, so a crash never replays a keystroke; what
// was in flight at the crash is lost, never repeated. Updates older than
// MaxAge (the startup backlog, or what piled up while the Mac slept) are
// skipped and counted; the relay's button taps (asks, voice
// confirmations, session picks) have no date of their own and are never
// skipped. While the startup backlog drains the count adds up; after that,
// each batch that skipped anything sends one notice with its count. The
// router handles updates on a ctx of Run's own, which ends whenever Run
// returns, and the voice worker with it (stopVoice).
func (l *Loop) Run(ctx context.Context) error {
	release, err := l.Store.LockRelay()
	if err != nil {
		return err
	}
	defer release()
	routed, stop := context.WithCancel(ctx)
	defer l.stopVoice(ctx, stop)
	maxAge := l.MaxAge
	if maxAge <= 0 {
		maxAge = defaultMaxAge
	}
	rs, err := l.Store.LoadRelay()
	if err != nil {
		return err
	}
	rs.StartedAt, rs.PID = l.Router.now(), os.Getpid()
	rs.Features = []string{state.FeatureAsks} // waiting hooks send buttons only to a relay that routes their taps
	l.logf("relay starting (pid %d, once=%v, offset %d)", rs.PID, l.Once, rs.Offset)
	if l.Router.StartedAt.IsZero() {
		l.Router.StartedAt = rs.StartedAt
	}
	if err := l.Store.SaveRelay(rs); err != nil {
		return err
	}
	draining := true
	skipped := 0
	for {
		if ctx.Err() != nil {
			return nil
		}
		timeout := longPoll
		if draining || l.Once {
			timeout = 0
		}
		ups, err := l.Poll.GetUpdates(ctx, rs.Offset, timeout)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			l.logf("getUpdates: %v", err)
			if l.Once {
				return err
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(pollBackoff):
			}
			continue
		}
		for _, u := range ups {
			if ctx.Err() != nil {
				return nil // unclaimed updates are delivered by the next run
			}
			if u.UpdateID >= rs.Offset {
				rs.Offset = u.UpdateID + 1
			}
			if err := l.Store.SaveRelay(rs); err != nil {
				l.logf("cannot persist the offset (%v); stopping rather than risk a replay", err)
				return err
			}
			if !l.Router.Allowed(u) {
				l.logf("update %d: not from the configured user; dropped", u.UpdateID)
				continue
			}
			if t := updateTime(u); !t.IsZero() && l.Router.now().Sub(t) > maxAge {
				skipped++
				continue
			}
			if err := l.Router.Handle(routed, u); err != nil {
				l.logf("update %d: %v", u.UpdateID, err)
			}
			rs.LastUpdateAt = l.Router.now()
		}
		if len(ups) > 0 {
			_ = l.Store.SaveRelay(rs)
		}
		// Retire asks whose waiting hook died: no one else closes them.
		l.Router.SweepAsks(ctx)
		if draining && len(ups) < 100 {
			draining = false
		}
		if !draining && skipped > 0 {
			l.logf("skipped %d stale update(s)", skipped)
			_, _ = l.Router.send(ctx, 0, fmt.Sprintf("skipped %d message(s) older than %d min (the relay was down or the Mac slept)", skipped, int(maxAge.Minutes())), nil)
			skipped = 0
		}
		if l.Once {
			return nil
		}
	}
}

// stopVoice ends the router's ctx (stop) and waits for the voice worker
// before Run returns. With Once the queued notes first get all the time
// they take, each bounded by the command's own timeout, unless ctx ends
// (Ctrl-C): a first Whisper run that loads its model is not cut off
// (ruling R23). Otherwise they are cancelled at once. After stop the wait
// is bounded by stopWait: it lets a cancelled command's process group be
// killed and its directory removed, and the dropped notes' 👀 cleared,
// before the process exits; a transcription never completes after Run
// returned, its ctx being done.
func (l *Loop) stopVoice(ctx context.Context, stop context.CancelFunc) {
	wait := l.stopWait
	if wait <= 0 {
		wait = voiceStopWait
	}
	done := make(chan struct{})
	go func() {
		l.Router.WaitVoice()
		close(done)
	}()
	if l.Once {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	stop()
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}

// ErrNotLoaded is returned by status helpers when launchd does not know the agent.
var ErrNotLoaded = errors.New("relay agent is not loaded")
