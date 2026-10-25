package state

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

// FeatureAsks marks a relay that routes button taps and swipe-replies to
// waiting hooks (see Ask).
const FeatureAsks = "asks"

// RelayState is what the relay persists between runs.
type RelayState struct {
	Offset       int64     `json:"offset"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	LastUpdateAt time.Time `json:"last_update_at,omitempty"`
	PID          int       `json:"pid,omitempty"`
	// Features the running relay handles, set when it starts. A relay from
	// before a feature rewrites its state without it, so the list always
	// describes the relay that started last.
	Features []string `json:"features,omitempty"`
}

func (s *Store) relayPath() string { return filepath.Join(s.Dir, "relay.json") }

// LoadRelay returns the persisted relay state, zero when absent.
func (s *Store) LoadRelay() (RelayState, error) {
	var rs RelayState
	b, err := os.ReadFile(s.relayPath())
	if errors.Is(err, os.ErrNotExist) {
		return rs, nil
	}
	if err != nil {
		return rs, err
	}
	if err := json.Unmarshal(b, &rs); err != nil {
		return RelayState{}, nil // start over rather than wedge the relay
	}
	return rs, nil
}

// SaveRelay persists the relay state atomically.
func (s *Store) SaveRelay(rs RelayState) error { return writeAtomic(s.relayPath(), rs) }

// LockRelay takes the single-instance lock without waiting; it fails when
// another relay holds it.
func (s *Store) LockRelay() (func(), error) {
	path := filepath.Join(s.Dir, "relay.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, errors.New("another tg relay is running")
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// RelayLocked reports whether a relay currently holds the lock.
func (s *Store) RelayLocked() bool {
	release, err := s.LockRelay()
	if err != nil {
		return true
	}
	release()
	return false
}

// RelayServes reports whether a relay runs (holds the lock) and lists
// feature in its state.
func (s *Store) RelayServes(feature string) bool {
	if !s.RelayLocked() {
		return false
	}
	rs, err := s.LoadRelay()
	return err == nil && slices.Contains(rs.Features, feature)
}

type pending struct {
	Text    string    `json:"text"`
	Created time.Time `json:"created"`
}

func (s *Store) pendingPath(key string) string {
	return filepath.Join(s.Dir, "pending", safeName(key)+".json")
}

// PutPending parks a text until a keyboard button picks its target.
func (s *Store) PutPending(key, text string, now time.Time) error {
	if err := os.MkdirAll(filepath.Join(s.Dir, "pending"), 0o700); err != nil {
		return err
	}
	return writeAtomic(s.pendingPath(key), pending{Text: text, Created: now})
}

// TakePending returns and deletes a parked text; expired or missing → false.
func (s *Store) TakePending(key string, now time.Time, maxAge time.Duration) (string, bool) {
	path := s.pendingPath(key)
	release, err := lockFile(context.Background(), filepath.Join(s.Dir, "pending.lock"))
	if err != nil {
		return "", false
	}
	defer release()
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	// Hand the text out only once it can never be handed out again.
	if err := os.Remove(path); err != nil {
		return "", false
	}
	var p pending
	if json.Unmarshal(b, &p) != nil || now.Sub(p.Created) > maxAge {
		return "", false
	}
	return p.Text, true
}

// FindSessionByMessageID returns the session that sent the Telegram message.
func (s *Store) FindSessionByMessageID(id int64) (*Session, bool) {
	list, err := s.ListSessions()
	if err != nil {
		return nil, false
	}
	for _, sess := range list {
		for _, m := range sess.LastMessageIDs {
			if m == id {
				return sess, true
			}
		}
	}
	return nil, false
}
