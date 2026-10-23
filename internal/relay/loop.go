package relay

import (
	"context"
	"errors"
	"fmt"
	"os"
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

func updateTime(u telegram.Update) time.Time {
	switch {
	case u.Message != nil && u.Message.Date != 0:
		return time.Unix(u.Message.Date, 0)
	case u.CallbackQuery != nil && u.CallbackQuery.Message != nil && u.CallbackQuery.Message.Date != 0:
		return time.Unix(u.CallbackQuery.Message.Date, 0)
	}
	return time.Time{}
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
// was in flight at the crash is lost, never repeated. While the startup
// backlog drains, updates older than MaxAge are skipped and counted; one
// notice reports the count.
func (l *Loop) Run(ctx context.Context) error {
	release, err := l.Store.LockRelay()
	if err != nil {
		return err
	}
	defer release()
	maxAge := l.MaxAge
	if maxAge <= 0 {
		maxAge = defaultMaxAge
	}
	rs, err := l.Store.LoadRelay()
	if err != nil {
		return err
	}
	rs.StartedAt, rs.PID = l.Router.now(), os.Getpid()
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
			if err := l.Router.Handle(ctx, u); err != nil {
				l.logf("update %d: %v", u.UpdateID, err)
			}
			rs.LastUpdateAt = l.Router.now()
		}
		if len(ups) > 0 {
			_ = l.Store.SaveRelay(rs)
		}
		if draining && len(ups) < 100 {
			draining = false
		}
		if !draining && skipped > 0 {
			l.logf("skipped %d stale update(s)", skipped)
			_, _ = l.Router.send(ctx, 0, fmt.Sprintf("skipped %d message(s) older than %d min while the relay was down", skipped, int(maxAge.Minutes())), nil)
			skipped = 0
		}
		if l.Once {
			return nil
		}
	}
}

// ErrNotLoaded is returned by status helpers when launchd does not know the agent.
var ErrNotLoaded = errors.New("relay agent is not loaded")
