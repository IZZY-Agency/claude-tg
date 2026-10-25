// Package state persists per-session facts, alert dedupe, the outbound spool,
// cross-process send pacing and the log under ~/.local/state/claude-tg.
package state

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/IZZY-Agency/claude-tg/internal/term"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Store is the state directory.
type Store struct{ Dir string }

// Open creates the directory layout (0700) and returns the store.
func Open(dir string) (*Store, error) {
	for _, d := range []string{dir, filepath.Join(dir, "sessions"), filepath.Join(dir, "spool"), filepath.Join(dir, "asks")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	return &Store{Dir: dir}, nil
}

// Pending is a permission request seen but not yet answered.
type Pending struct {
	Tool    string    `json:"tool"`
	Summary string    `json:"summary"`
	At      time.Time `json:"at"`
}

// Session is what tg knows about one Claude Code session. The agterm ids and
// LastMessageIDs exist for v2 reply routing and are not used by v1 logic.
type Session struct {
	SessionID         string    `json:"session_id"`
	Name              string    `json:"name,omitempty"`
	Cwd               string    `json:"cwd,omitempty"`
	PID               int       `json:"pid,omitempty"`
	AgtermSessionID   string    `json:"agterm_session_id,omitempty"`
	AgtermWindowID    string    `json:"agterm_window_id,omitempty"`
	TmuxPane          string    `json:"tmux_pane,omitempty"`   // tmux pane id such as %5
	TmuxSocket        string    `json:"tmux_socket,omitempty"` // tmux server socket path
	Follow            bool      `json:"follow"`
	PendingAnswers    int       `json:"pending_answers,omitempty"` // answers still owed to the phone after replies typed from it
	LastSeen          time.Time `json:"last_seen,omitempty"`
	NameResolvedAt    time.Time `json:"name_resolved_at,omitempty"`
	EndedAt           time.Time `json:"ended_at,omitempty"`
	LastMessageIDs    []int64   `json:"last_message_ids,omitempty"`
	AskMessageIDs     []int64   `json:"ask_message_ids,omitempty"` // which of them carried a prompt's buttons (SendButtons)
	AskCredits        []string  `json:"ask_credits,omitempty"`     // asks answered from the phone, each owing the phone one answer of the session (ArmAsk)
	PendingPermission *Pending  `json:"pending_permission,omitempty"`
}

// AskMessage reports whether message id carried a question's or permission
// prompt's buttons, whether or not its ask is still there.
func (s *Session) AskMessage(id int64) bool {
	for _, m := range s.AskMessageIDs {
		if m == id {
			return true
		}
	}
	return false
}

// Pane is the terminal pane the relay types into. tmux wins over agterm: a
// pane id names the exact pane, while an agterm session id names a whole tab
// that may itself be running tmux.
func (s *Session) Pane() term.Pane {
	switch {
	case s.TmuxPane != "":
		return term.Pane{Backend: "tmux", Target: s.TmuxPane, Socket: s.TmuxSocket}
	case s.AgtermSessionID != "":
		return term.Pane{Backend: "agterm", Target: s.AgtermSessionID}
	}
	return term.Pane{}
}

func safeName(id string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, id)
}

func (s *Store) sessionPath(id string) string {
	return filepath.Join(s.Dir, "sessions", safeName(id)+".json")
}

// LoadSession returns the stored session, or a zero Session carrying the id.
func (s *Store) LoadSession(id string) (*Session, error) {
	b, err := os.ReadFile(s.sessionPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return &Session{SessionID: id}, nil
	}
	if err != nil {
		return nil, err
	}
	var sess Session
	if err := json.Unmarshal(b, &sess); err != nil {
		// A corrupt file must not wedge the hook: start over for this session.
		return &Session{SessionID: id}, nil
	}
	if sess.SessionID == "" {
		sess.SessionID = id
	}
	return &sess, nil
}

// SaveSession replaces the stored session under the session lock. Prefer
// UpdateSession for read-modify-write changes.
func (s *Store) SaveSession(sess *Session) error {
	release, err := lockFile(context.Background(), s.sessionPath(sess.SessionID)+".lock")
	if err != nil {
		return err
	}
	defer release()
	return writeAtomic(s.sessionPath(sess.SessionID), sess)
}

// UpdateSession loads the session, applies fn and saves it, all under a
// per-session lock so concurrent hooks never lose each other's changes.
func (s *Store) UpdateSession(id string, fn func(*Session)) (*Session, error) {
	return s.UpdateSessionCtx(context.Background(), id, fn)
}

// UpdateSessionCtx is UpdateSession waiting for the session's lock only
// until ctx ends (it tries once even then), for a caller in a hurry.
func (s *Store) UpdateSessionCtx(ctx context.Context, id string, fn func(*Session)) (*Session, error) {
	release, err := lockFile(ctx, s.sessionPath(id)+".lock")
	if err != nil {
		return nil, err
	}
	defer release()
	sess, err := s.LoadSession(id)
	if err != nil {
		return nil, err
	}
	fn(sess)
	if err := writeAtomic(s.sessionPath(id), sess); err != nil {
		return nil, err
	}
	return sess, nil
}

// ListSessions returns all stored sessions sorted by name.
func (s *Store) ListSessions() ([]*Session, error) {
	entries, err := os.ReadDir(filepath.Join(s.Dir, "sessions"))
	if err != nil {
		return nil, err
	}
	var out []*Session
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.Dir, "sessions", e.Name()))
		if err != nil {
			continue
		}
		var sess Session
		if json.Unmarshal(b, &sess) == nil && sess.SessionID != "" {
			out = append(out, &sess)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func writeAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	var r [4]byte
	_, _ = rand.Read(r[:])
	tmp := path + ".tmp-" + hex.EncodeToString(r[:])
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// keepCredits bounds a session's ask credits: a session that never stops
// again must not grow them for good.
const keepCredits = 50

// ArmAsk makes the session owe the phone its next answer for ask id,
// answered from the phone, with a credit of its own (AskCredits), so
// taking it back later (DropAsk) never takes another answer's. It reports
// whether it added one: a followed session reports every answer anyway,
// and an ask is credited once.
func (s *Session) ArmAsk(id string) bool {
	if s.Follow || slices.Contains(s.AskCredits, id) {
		return false
	}
	s.AskCredits = append(s.AskCredits, id)
	if n := len(s.AskCredits); n > keepCredits {
		s.AskCredits = s.AskCredits[n-keepCredits:]
	}
	return true
}

// DropAsk takes back ask id's credit when its answer never reached Claude
// Code, and reports whether it was there: once spent (TakeAnswer) or taken
// back, there is nothing to drop.
func (s *Session) DropAsk(id string) bool {
	i := slices.Index(s.AskCredits, id)
	if i < 0 {
		return false
	}
	s.AskCredits = slices.Delete(s.AskCredits, i, i+1)
	return true
}

// TakeAnswer spends one answer owed to the phone, for the Stop hook: the
// oldest ask credit first, else one of a typed reply's (PendingAnswers).
// It reports whether one was owed.
func (s *Session) TakeAnswer() bool {
	switch {
	case len(s.AskCredits) > 0:
		s.AskCredits = s.AskCredits[1:]
	case s.PendingAnswers > 0:
		s.PendingAnswers--
	default:
		return false
	}
	return true
}

// Owed is how many of its answers the session owes the phone.
func (s *Session) Owed() int { return s.PendingAnswers + len(s.AskCredits) }

// TakeBackAnswer drops ask askID's credit (DropAsk) when its answer never
// reached Claude Code, under the session's lock, waiting for that lock
// only until ctx ends. Running it again, or after the credit was spent,
// changes nothing.
func (s *Store) TakeBackAnswer(ctx context.Context, sessionID, askID string) error {
	_, err := s.UpdateSessionCtx(ctx, sessionID, func(sess *Session) { sess.DropAsk(askID) })
	return err
}
