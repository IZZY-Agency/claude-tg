package state

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Decision is the phone's answer to a permission prompt.
type Decision struct {
	Behavior   string `json:"behavior"`            // "allow" | "deny"
	Suggestion int    `json:"suggestion"`          // index into the original permission_suggestions; -1 = none
	Message    string `json:"message,omitempty"`   // deny only
	Interrupt  bool   `json:"interrupt,omitempty"` // deny only
}

// Ask is one open question or permission prompt a waiting hook process is
// blocked on. The hook creates it, the relay writes the phone's answer into
// it and the hook polls it.
type Ask struct {
	ID          string            `json:"id"`
	SessionID   string            `json:"session_id"`
	SessionName string            `json:"session_name,omitempty"`
	Tool        string            `json:"tool"`
	Summary     string            `json:"summary,omitempty"`
	ToolInput   json.RawMessage   `json:"tool_input,omitempty"`  // exact, echoed back for AskUserQuestion
	Suggestions json.RawMessage   `json:"suggestions,omitempty"` // exact permission_suggestions
	HookPID     int               `json:"hook_pid,omitempty"`
	Created     time.Time         `json:"created"`
	Deadline    time.Time         `json:"deadline"`
	MessageIDs  []int64           `json:"message_ids,omitempty"` // one per question; one for a permission prompt; append-only while sending
	Texts       []string          `json:"texts,omitempty"`       // HTML of each message as sent, base for edits
	Ticked      [][]bool          `json:"ticked,omitempty"`      // multi-select state per question
	Answers     []json.RawMessage `json:"answers,omitempty"`     // per question; nil = unanswered
	Decision    *Decision         `json:"decision,omitempty"`    // permission prompts
	Done        bool              `json:"done,omitempty"`        // fully answered from the phone: the hook may return
	Collected   bool              `json:"collected,omitempty"`   // the hook took the phone's answer for Claude Code (set just before it removes the ask)
	Armed       bool              `json:"armed,omitempty"`       // the relay gave the answer a credit in its session's AskCredits (taken back if the Mac wins)
	Settled     bool              `json:"settled,omitempty"`     // the relay made (or gave up) the edit that shows the phone's completed answer
	TakeBack    bool              `json:"take_back,omitempty"`   // the Armed answer never reached Claude: it is owed back (Store.TakeBackAnswer; the sweep finishes it)
	Closed      string            `json:"closed,omitempty"`      // "" | "mac" | "expired" | "ended"
	Signature   string            `json:"signature,omitempty"`   // Fingerprint of the dialog or picker its hook adopted on the pane (ClaimSignature)
}

// Open reports whether the ask still waits for an answer.
func (a *Ask) Open() bool { return !a.Done && a.Closed == "" }

// NewAskID returns 16 lowercase hex characters from crypto/rand.
func NewAskID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (s *Store) asksDir() string { return filepath.Join(s.Dir, "asks") }

// askLockTimeout bounds the wait for an ask's lock: a stuck holder must
// fail the operation, not hang the hook or the relay. A variable for tests.
var askLockTimeout = 10 * time.Second

// lockAsk takes the lock of the ask file at path, waiting at most
// askLockTimeout.
func lockAsk(path string) (func(), error) { return lockAskWithin(path, askLockTimeout) }

// ErrAskLocked is the error of a caller in a hurry whose wait for an ask's
// lock ran out (UpdateAskWithin, DeleteAskWithin).
var ErrAskLocked = errors.New("state: the ask is locked")

// lockAskWithin takes the lock of the ask file at path, waiting at most
// wait; a wait of 0 or less tries once. A wait that runs out is
// ErrAskLocked (and context.DeadlineExceeded).
func lockAskWithin(path string, wait time.Duration) (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), max(wait, 0))
	defer cancel()
	release, err := lockFile(ctx, path+".lock")
	if err != nil && ctx.Err() != nil {
		return nil, fmt.Errorf("%w: %w", ErrAskLocked, context.DeadlineExceeded)
	}
	return release, err
}

func (s *Store) askPath(id string) string {
	return filepath.Join(s.asksDir(), safeName(id)+".json")
}

// PutAsk writes the ask (mode 0600) under its lock.
func (s *Store) PutAsk(a *Ask) error {
	if a.ID == "" {
		return errors.New("state: ask without an id")
	}
	release, err := lockAsk(s.askPath(a.ID))
	if err != nil {
		return err
	}
	defer release()
	return writeAtomic(s.askPath(a.ID), a)
}

// LoadAsk reads one ask; errors.Is(err, os.ErrNotExist) when it is gone.
func (s *Store) LoadAsk(id string) (*Ask, error) {
	b, err := os.ReadFile(s.askPath(id))
	if err != nil {
		return nil, err
	}
	var a Ask
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, err
	}
	a.normalize()
	return &a, nil
}

// normalize turns the JSON null an unanswered slot is stored as back into nil.
func (a *Ask) normalize() {
	for i, r := range a.Answers {
		if string(r) == "null" {
			a.Answers[i] = nil
		}
	}
}

// ErrRemoveAsk, returned by an UpdateAsk fn, removes the ask under its lock
// instead of saving it; UpdateAsk then returns the ask as fn left it and no
// error.
var ErrRemoveAsk = errors.New("state: remove the ask")

// CollectAsk marks the phone's answer collected: its hook handed it to
// Claude Code. The ask is done with once the relay has also shown the
// answer on the phone (Settled); whichever of the two comes second
// removes it, so the relay's edit always finds the ask to edit by.
func (s *Store) CollectAsk(id string) error { return s.CollectAskWithin(id, askLockTimeout) }

// CollectAskWithin is CollectAsk waiting at most wait for the ask's lock;
// ErrAskLocked when it ran out.
func (s *Store) CollectAskWithin(id string, wait time.Duration) error {
	_, err := s.UpdateAskWithin(id, wait, func(a *Ask) error {
		a.Collected = true
		if a.Settled {
			return ErrRemoveAsk
		}
		return nil
	})
	return err
}

// UpdateAsk loads the ask, applies fn and saves it under a per-ask lock so
// the hook and the relay never lose each other's changes. An fn error
// writes nothing and is returned as is, but ErrRemoveAsk.
func (s *Store) UpdateAsk(id string, fn func(*Ask) error) (*Ask, error) {
	return s.UpdateAskWithin(id, askLockTimeout, fn)
}

// UpdateAskWithin is UpdateAsk waiting at most wait for the lock (once for
// a wait of 0 or less); ErrAskLocked when it ran out.
func (s *Store) UpdateAskWithin(id string, wait time.Duration, fn func(*Ask) error) (*Ask, error) {
	release, err := lockAskWithin(s.askPath(id), wait)
	if err != nil {
		return nil, err
	}
	defer release()
	return s.updateAsk(id, fn)
}

// ForceUpdateAsk is UpdateAsk without the lock, for a caller that could not
// get it in time and must still put its change on record (a hook about to
// be killed): best effort, as an update holding the lock meanwhile may
// overwrite it.
func (s *Store) ForceUpdateAsk(id string, fn func(*Ask) error) (*Ask, error) {
	return s.updateAsk(id, fn)
}

// updateAsk is UpdateAsk's load, fn and save.
func (s *Store) updateAsk(id string, fn func(*Ask) error) (*Ask, error) {
	// Load under the lock: a DeleteAsk that won the lock first leaves nothing
	// to load, so a deleted ask is never recreated.
	a, err := s.LoadAsk(id)
	if err != nil {
		return nil, err
	}
	if err := fn(a); errors.Is(err, ErrRemoveAsk) {
		if err := os.Remove(s.askPath(id)); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		return a, nil
	} else if err != nil {
		return nil, err
	}
	if err := writeAtomic(s.askPath(id), a); err != nil {
		return nil, err
	}
	return a, nil
}

// WithAsk runs fn on the ask as it is under its lock, and saves nothing:
// whoever acts on the ask meanwhile (an UpdateAsk, a DeleteAsk) waits for
// fn. errors.Is(err, os.ErrNotExist) when the ask is gone; an fn error is
// returned as is.
func (s *Store) WithAsk(id string, fn func(*Ask) error) error {
	release, err := lockAsk(s.askPath(id))
	if err != nil {
		return err
	}
	defer release()
	a, err := s.LoadAsk(id)
	if err != nil {
		return err
	}
	return fn(a)
}

// DeleteAsk removes the ask under its lock, so it waits for an in-flight
// UpdateAsk and no update can recreate it afterwards. The tiny .lock file is
// left behind on purpose: unlinking a lock that may be held would let two
// updaters run at once.
func (s *Store) DeleteAsk(id string) error { return s.DeleteAskWithin(id, askLockTimeout) }

// DeleteAskWithin is DeleteAsk waiting at most wait for the lock (once for
// a wait of 0 or less); ErrAskLocked when it ran out.
func (s *Store) DeleteAskWithin(id string, wait time.Duration) error {
	release, err := lockAskWithin(s.askPath(id), wait)
	if err != nil {
		return err
	}
	defer release()
	if err := os.Remove(s.askPath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// readAskFile parses one ask file; an ask without an id is corrupt too.
func readAskFile(path string) (*Ask, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var a Ask
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, err
	}
	if a.ID == "" {
		return nil, errors.New("no id")
	}
	a.normalize()
	return &a, nil
}

// askFiles returns the paths of the ask files (not their locks or temp files).
func (s *Store) askFiles() ([]string, error) {
	entries, err := os.ReadDir(s.asksDir())
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, filepath.Join(s.asksDir(), e.Name()))
		}
	}
	return out, nil
}

// loggedCorrupt holds the paths of the unreadable ask files this process
// has logged: the relay lists the asks on every message and sweep, and one
// line per file is enough.
var loggedCorrupt sync.Map

// ListAsks returns all readable asks, oldest first. An unreadable or
// corrupt file is skipped and logged once per process; the relay's sweep
// removes it once it is old (PruneCorruptAsks).
func (s *Store) ListAsks() ([]*Ask, error) {
	files, err := s.askFiles()
	if err != nil {
		return nil, err
	}
	var out []*Ask
	for _, f := range files {
		a, err := readAskFile(f)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) { // deleted since the listing
				continue
			}
			if _, seen := loggedCorrupt.LoadOrStore(f, true); !seen {
				s.Logf("ask file %s unreadable: %v", filepath.Base(f), err)
			}
			continue
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

// listAsksLogged is ListAsks for lookups that cannot return an error: a
// failure is logged and means no asks.
func (s *Store) listAsksLogged() []*Ask {
	asks, err := s.ListAsks()
	if err != nil {
		s.Logf("list asks: %v", err)
	}
	return asks
}

// FindAskByMessageID returns the ask owning the Telegram message and the
// index of that message within the ask. An error listing the asks is
// logged and reads as not found.
func (s *Store) FindAskByMessageID(msgID int64) (a *Ask, index int, ok bool) {
	for _, c := range s.listAsksLogged() {
		for i, id := range c.MessageIDs {
			if id == msgID {
				return c, i, true
			}
		}
	}
	return nil, 0, false
}

// OpenAskForSession returns the newest open ask of the session. An error
// listing the asks is logged and reads as none.
func (s *Store) OpenAskForSession(sessionID string) (*Ask, bool) {
	asks := s.listAsksLogged()
	for i := len(asks) - 1; i >= 0; i-- {
		if asks[i].SessionID == sessionID && asks[i].Open() {
			return asks[i], true
		}
	}
	return nil, false
}

// PruneCorruptAsks removes the ask files no one can read once they are
// more than olderThan old at now: no hook waits on an ask it cannot read.
// A file whose lock someone holds is being written and is kept. Each
// removal is logged. It returns how many it removed.
func (s *Store) PruneCorruptAsks(olderThan time.Duration, now time.Time) (int, error) {
	files, err := s.askFiles()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil || now.Sub(info.ModTime()) <= olderThan {
			continue
		}
		if _, err := readAskFile(f); err == nil || errors.Is(err, os.ErrNotExist) {
			continue
		}
		if s.removeCorruptAsk(f) {
			s.Logf("removed corrupt ask file %s", filepath.Base(f))
			n++
		}
	}
	return n, nil
}

// removeCorruptAsk unlinks the ask file at path if it is still corrupt
// under its lock, without waiting for a lock someone holds.
func (s *Store) removeCorruptAsk(path string) bool {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // lockFile tries once before it looks at the context
	release, err := lockFile(ctx, path+".lock")
	if err != nil {
		return false
	}
	defer release()
	if _, err := readAskFile(path); err == nil || errors.Is(err, os.ErrNotExist) {
		return false
	}
	return os.Remove(path) == nil
}

// PruneAskLocks removes the lock files of asks that no longer exist once
// they are more than olderThan old at now: DeleteAsk leaves its lock behind
// on purpose, and an update of an unknown id creates one. A lock someone
// holds is kept. It returns how many it removed.
func (s *Store) PruneAskLocks(olderThan time.Duration, now time.Time) (int, error) {
	entries, err := os.ReadDir(s.asksDir())
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json.lock") {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) <= olderThan {
			continue
		}
		if removeOrphanLock(filepath.Join(s.asksDir(), e.Name())) {
			n++
		}
	}
	return n, nil
}

// removeOrphanLock unlinks an ask lock whose ask file is gone, unless
// someone holds the lock. The ask is checked again under the lock.
func removeOrphanLock(lock string) bool {
	askFile := strings.TrimSuffix(lock, ".lock")
	if _, err := os.Stat(askFile); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	f, err := os.OpenFile(lock, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return false
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	if _, err := os.Stat(askFile); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	// Unlinking a lock we hold is safe here: no one acts on an ask whose
	// file is missing (UpdateAsk and DeleteAsk find nothing to change), and
	// ids are never reused, so no PutAsk will want this lock again.
	return os.Remove(lock) == nil
}

// Fingerprint is what an ask stores of a dialog's or picker's signature:
// the sha256 of it, in hex. The signature is screen text, which can hold
// what the command line holds (a password the summary masks); it stays in
// the waiting hook's memory only.
func Fingerprint(sig string) string {
	sum := sha256.Sum256([]byte(sig))
	return hex.EncodeToString(sum[:])
}

// ClaimSignature records the Fingerprint of sig, the signature of the
// dialog or picker a waiting hook found on its pane, on ask id, unless that
// dialog belongs to another call of the same session (ruling R30b): another
// ask of the session that is neither closed nor collected (open, or
// answered and not yet handed to Claude Code) holds it, or an older open
// ask of the session holds nothing
// yet and is still looking for its dialog (made less than lookFor before
// now): dialogs show in the order of their calls, so the one on the pane is
// the older call's. One lock serialises every claim, so two hooks never
// both claim a dialog. It reports whether the claim holds.
func (s *Store) ClaimSignature(id, sig string, lookFor time.Duration, now time.Time) (bool, error) {
	fp := Fingerprint(sig)
	ctx, cancel := context.WithTimeout(context.Background(), askLockTimeout)
	defer cancel()
	release, err := lockFile(ctx, filepath.Join(s.asksDir(), "claim.lock"))
	if err != nil {
		return false, err
	}
	defer release()
	self, err := s.LoadAsk(id)
	if err != nil {
		return false, err
	}
	asks, err := s.ListAsks()
	if err != nil {
		return false, err
	}
	for _, o := range asks {
		if o.ID == id || o.SessionID != self.SessionID || o.Closed != "" || o.Collected {
			continue
		}
		if o.Signature == fp {
			return false, nil
		}
		if o.Signature == "" && o.Open() && o.Created.Before(self.Created) && now.Sub(o.Created) < lookFor {
			return false, nil
		}
	}
	if _, err := s.UpdateAsk(id, func(a *Ask) error { a.Signature = fp; return nil }); err != nil {
		return false, err
	}
	return true, nil
}
