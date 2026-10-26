package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Pick is the session the owner picked in the relay's /sessions: plain
// text and voice notes go there. At is when it was picked, or when a
// message last went to the session through it; the relay lets it lapse a
// while after that (Fresh).
type Pick struct {
	SessionID string    `json:"session_id"`
	At        time.Time `json:"at"`
}

// Fresh reports whether the pick names a session and At lies within ttl
// of now, on either side, so a clock set back cannot keep it for good.
func (p Pick) Fresh(now time.Time, ttl time.Duration) bool {
	d := now.Sub(p.At)
	return p.SessionID != "" && d <= ttl && d >= -ttl
}

func (s *Store) pickPath() string { return filepath.Join(s.Dir, "pick.json") }

// LoadPick returns the stored pick, zero when there is none. A pick that
// cannot be read or parsed, or names no session or no time, is an error,
// never "no pick" (nor an expired one): the relay must not send a message
// elsewhere because of it.
// SavePick replaces it.
func (s *Store) LoadPick() (Pick, error) {
	var p Pick
	b, err := os.ReadFile(s.pickPath())
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return Pick{}, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return Pick{}, fmt.Errorf("pick.json: %w", err)
	}
	if p.SessionID == "" {
		return Pick{}, errors.New("pick.json names no session")
	}
	if p.At.IsZero() {
		return Pick{}, errors.New("pick.json has no time")
	}
	return p, nil
}

// SavePick replaces the pick under pick.lock, whatever the file held
// before (an unreadable one too); a pick without a session removes it.
func (s *Store) SavePick(p Pick) error {
	release, err := lockFile(context.Background(), filepath.Join(s.Dir, "pick.lock"))
	if err != nil {
		return err
	}
	defer release()
	if p.SessionID == "" {
		if err := os.Remove(s.pickPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return writeAtomic(s.pickPath(), p)
}

// UpdatePick loads the pick, applies fn and saves the result, all under
// pick.lock, so the update loop and the voice worker never lose each
// other's changes. A result without a session removes the pick, and
// nothing is written when fn changes nothing. It returns the pick as it
// now stands; a pick LoadPick cannot read fails it, fn unapplied.
func (s *Store) UpdatePick(fn func(*Pick)) (Pick, error) {
	release, err := lockFile(context.Background(), filepath.Join(s.Dir, "pick.lock"))
	if err != nil {
		return Pick{}, err
	}
	defer release()
	was, err := s.LoadPick()
	if err != nil {
		return Pick{}, err
	}
	p := was
	fn(&p)
	if p.SessionID == "" {
		p = Pick{}
	}
	switch {
	case p.SessionID == was.SessionID && p.At.Equal(was.At):
		return p, nil
	case p.SessionID == "":
		if err := os.Remove(s.pickPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return was, err
		}
		return p, nil
	}
	if err := writeAtomic(s.pickPath(), p); err != nil {
		return was, err
	}
	return p, nil
}
