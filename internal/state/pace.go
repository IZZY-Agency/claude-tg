package state

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Pace is PaceCtx with a background context.
func (s *Store) Pace(minGap time.Duration) (release func(), err error) {
	return s.PaceCtx(context.Background(), minGap)
}

// PaceCtx takes the cross-process send lock and sleeps until at least minGap
// has passed since the previous send, giving up when ctx ends. The returned
// func records the send time and releases the lock; callers must invoke it
// right after the HTTP call.
func (s *Store) PaceCtx(ctx context.Context, minGap time.Duration) (release func(), err error) {
	lastPath := filepath.Join(s.Dir, "send.last")
	unlock, err := lockFile(ctx, filepath.Join(s.Dir, "send.lock"))
	if err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(lastPath); err == nil {
		if ns, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
			if wait := minGap - time.Since(time.Unix(0, ns)); wait > 0 {
				select {
				case <-ctx.Done():
					unlock()
					return nil, ctx.Err()
				case <-time.After(wait):
				}
			}
		}
	}
	return func() {
		_ = os.WriteFile(lastPath, []byte(strconv.FormatInt(time.Now().UnixNano(), 10)), 0o600)
		unlock()
	}, nil
}
