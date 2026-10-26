// Package state persists per-session facts, alert dedupe, the outbound spool,
// cross-process send pacing and the log under ~/.local/state/claude-tg.
package state

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	SessionID         string      `json:"session_id"`
	Name              string      `json:"name,omitempty"`        // the label tg shows (session.Label), cut to the 64 runes a header shows
	FullLabel         string      `json:"full_label,omitempty"`  // the label in full when Name is cut, to address the session by
	ClaudeName        string      `json:"claude_name,omitempty"` // Claude Code's own name (`claude agents`), resolved at NameResolvedAt
	Title             string      `json:"title,omitempty"`       // Claude Code's session title, from the transcript
	Tab               string      `json:"tab,omitempty"`         // the agterm tab's name when the owner named it
	Cwd               string      `json:"cwd,omitempty"`
	PID               int         `json:"pid,omitempty"`
	AgtermSessionID   string      `json:"agterm_session_id,omitempty"`
	AgtermWindowID    string      `json:"agterm_window_id,omitempty"`
	TmuxPane          string      `json:"tmux_pane,omitempty"`   // tmux pane id such as %5
	TmuxSocket        string      `json:"tmux_socket,omitempty"` // tmux server socket path
	Follow            bool        `json:"follow"`
	PendingAnswers    int         `json:"pending_answers,omitempty"` // answers still owed to the phone after replies typed from it
	LastSeen          time.Time   `json:"last_seen,omitempty"`
	NameResolvedAt    time.Time   `json:"name_resolved_at,omitempty"`
	EndedAt           time.Time   `json:"ended_at,omitempty"`
	LastMessageIDs    []int64     `json:"last_message_ids,omitempty"`
	AskMessageIDs     []int64     `json:"ask_message_ids,omitempty"` // which of them carried a prompt's buttons (SendButtons)
	AskCredits        []string    `json:"ask_credits,omitempty"`     // asks answered from the phone, each owing the phone one answer of the session (ArmAsk)
	VoiceMarks        []VoiceMark `json:"voice_marks,omitempty"`     // voice notes delivered, each making one answer of the session spoken (MarkVoice)
	PhoneLines        []PhoneLine `json:"phone_lines,omitempty"`     // lines the relay submitted from the phone, by which a hook knows a turn the phone started (RecordPhoneLine)
	PendingPermission *Pending    `json:"pending_permission,omitempty"`
	MacAnswerAt       time.Time   `json:"mac_answer_at,omitempty"`   // when a waiting hook last saw one of its prompts answered at the Mac: a turn the phone started before it no longer counts as one
	PhoneAnswerAt     time.Time   `json:"phone_answer_at,omitempty"` // when the relay last typed the /approve or /deny fallback into the session's dialog: the close that follows is the phone's, not the Mac's
}

// PhoneLine is a line the relay typed into the session from the phone and
// submitted: its fingerprint (PhoneSum), when it was submitted, and
// whether the session was busy then (Queued), so that the text waited
// behind the running turn. Only the fingerprint is kept, as the session
// file needs no copy of the text.
type PhoneLine struct {
	At     time.Time `json:"at"`
	Sum    string    `json:"sum"`
	Queued bool      `json:"queued,omitempty"`
	// Answered reports that a hook already took the line (TakePhoneLine):
	// its turn's answer was handled, so no later Stop treats it as new.
	Answered bool `json:"answered,omitempty"`
}

// VoiceMark is a voice note's transcript delivered to the session, or a
// voice reply that completed one of its asks (AskID): the session's next
// answer is spoken. At is when it was made, as marks lapse (VoiceMarkTTL).
type VoiceMark struct {
	At    time.Time `json:"at"`
	AskID string    `json:"ask_id,omitempty"`
	// Lang is the language of what the owner said (speech.Language of the
	// transcript), "" when it could not tell: the language an answer too
	// short to tell its own is spoken in.
	Lang string `json:"lang,omitempty"`
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
// back, there is nothing to drop. The ask's voice mark (MarkVoice) goes
// with it, whether or not a credit was there: a followed session has a
// mark and no credit.
func (s *Session) DropAsk(id string) bool {
	s.DropVoiceMark(id)
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

// VoiceMarkTTL is how long a voice mark waits to be spent. A stray spoken
// answer is worse than a missed one, which only falls back to text, so a
// mark the session never answered (its turn was interrupted at the Mac,
// which fires no hook) lapses rather than speak a later, unrelated answer.
const VoiceMarkTTL = time.Hour

// keepMarks bounds a session's voice marks, as keepCredits its credits.
const keepMarks = 50

// MarkVoice records that a voice note reached the session at now, so that
// one of its next answers is spoken (TakeVoice): a transcript delivered
// (askID empty), or a voice reply that completed ask askID; lang is the
// language of what was said (VoiceMark.Lang). An ask is marked once, as
// it is credited once (ArmAsk). The newest keepMarks are kept.
func (s *Session) MarkVoice(askID, lang string, now time.Time) {
	if askID != "" && slices.ContainsFunc(s.VoiceMarks, func(m VoiceMark) bool { return m.AskID == askID }) {
		return
	}
	s.VoiceMarks = append(s.VoiceMarks, VoiceMark{At: now, AskID: askID, Lang: lang})
	if n := len(s.VoiceMarks); n > keepMarks {
		s.VoiceMarks = s.VoiceMarks[n-keepMarks:]
	}
}

// DropVoiceMark drops ask askID's voice mark (MarkVoice), whether or not
// its credit is still there, and leaves the credit alone.
func (s *Session) DropVoiceMark(askID string) {
	s.VoiceMarks = slices.DeleteFunc(s.VoiceMarks, func(m VoiceMark) bool { return m.AskID == askID })
}

// TakeVoice spends one voice mark, for the Stop hook deciding whether an
// answer is spoken: marks older than VoiceMarkTTL lapse first, then the
// oldest left is spent. It returns that mark and reports whether there
// was one.
func (s *Session) TakeVoice(now time.Time) (VoiceMark, bool) {
	s.VoiceMarks = slices.DeleteFunc(s.VoiceMarks, func(m VoiceMark) bool { return now.Sub(m.At) > VoiceMarkTTL })
	if len(s.VoiceMarks) == 0 {
		return VoiceMark{}, false
	}
	m := s.VoiceMarks[0]
	s.VoiceMarks = s.VoiceMarks[1:]
	return m, true
}

// SetFollow turns the session's follow feed on or off. Turning it off
// drops the voice marks too: a mark made while followed stood for an
// answer posted anyway, and must not make the answer to a later typed
// reply spoken. One left from before following goes as well, erring on
// the side of silence (a missed spoken answer only falls back to text).
func (s *Session) SetFollow(on bool) {
	s.Follow = on
	if !on {
		s.ClearVoice()
	}
}

// ClearVoice drops all the session's voice marks: the turn that would
// have answered them failed (StopFailure), or the session was unfollowed
// or ended (SetFollow).
func (s *Session) ClearVoice() { s.VoiceMarks = nil }

// TakeBackAnswer drops ask askID's credit and voice mark (DropAsk) when
// its answer never reached Claude Code, under the session's lock, waiting
// for that lock only until ctx ends. Each goes only while it is there, so
// running it again changes nothing. Credits and marks are spent in their
// own oldest-first orders (TakeAnswer, TakeVoice): the ask's mark may be
// left after its credit was spent, and is dropped then too.
func (s *Store) TakeBackAnswer(ctx context.Context, sessionID, askID string) error {
	_, err := s.UpdateSessionCtx(ctx, sessionID, func(sess *Session) { sess.DropAsk(askID) })
	return err
}

// DropVoiceMark drops ask askID's voice mark and keeps its credit
// (Session.DropVoiceMark), under the session's lock, waiting for that lock
// only until ctx ends: for an answer that may have reached Claude Code
// without its hook saying so, whose text is still owed but which must not
// make a later answer spoken. Running it again changes nothing.
func (s *Store) DropVoiceMark(ctx context.Context, sessionID, askID string) error {
	_, err := s.UpdateSessionCtx(ctx, sessionID, func(sess *Session) { sess.DropVoiceMark(askID) })
	return err
}

// keepPhoneLines bounds a session's phone lines: a hook only asks about
// the prompt of the turn it runs in, so the newest few are enough.
const keepPhoneLines = 10

// phoneLineWindow is how far apart a prompt's transcript entry and the
// recorded phone line may be and still count as one. The relay records
// the line right after its Enter; Claude Code writes the prompt when it
// takes the text, as a user entry, or as a queued_command attachment when
// it injects text sent to a busy session into the running turn
// (transcript.LastPrompt). An identical line typed at the Mac further away
// is the Mac's.
const phoneLineWindow = 2 * time.Minute

// queuedLineWindow is how long after a queued line's record
// (PhoneLine.Queued) its entry may come and still count (ruling P15):
// Claude Code takes text sent to a busy session in only when the running
// step ends, which may be a long build or test run later. An identical
// line typed at the Mac within it counts as the phone's.
const queuedLineWindow = time.Hour

// PhoneSum is a line's fingerprint: the lowercase hex SHA-256 of the line
// with its whitespace runs collapsed to one space and trimmed, as the
// relay types it (term.OneLine). The relay's leading space, and the
// newlines a phone message held, never change it.
func PhoneSum(text string) string {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(text), " ")))
	return hex.EncodeToString(sum[:])
}

// RecordPhoneLine records that the relay submitted line to the session at
// now, from the phone (SentFromPhone); queued reports that the session was
// busy, so that the line waits behind its running turn. The newest
// keepPhoneLines are kept.
func (s *Session) RecordPhoneLine(line string, now time.Time, queued bool) {
	s.PhoneLines = append(s.PhoneLines, PhoneLine{At: now, Sum: PhoneSum(line), Queued: queued})
	if n := len(s.PhoneLines); n > keepPhoneLines {
		s.PhoneLines = s.PhoneLines[n-keepPhoneLines:]
	}
}

// SentFromPhone reports whether text, a prompt Claude Code wrote at at, is
// a line the relay submitted from the phone: one recorded with the same
// fingerprint, the prompt written from phoneLineWindow before the record
// to phoneLineWindow after it, or, for a queued line, up to
// queuedLineWindow after it. The window runs both ways, as Claude Code
// may write the prompt before the relay, back from its Enter, records the
// line. An empty prompt never is one: the relay submits no empty line.
func (s *Session) SentFromPhone(text string, at time.Time) bool {
	return s.phoneLineIndex(text, at) >= 0
}

// TakePhoneLine finds the newest line SentFromPhone would match for text
// and at, and marks it Answered. It reports true only when the line was not
// answered before, so that one phone line is handled once however many
// Stops follow it. Only the newest match is looked at: an older line with
// the same text was answered by an earlier turn, or will never be. The
// caller saves the session.
func (s *Session) TakePhoneLine(text string, at time.Time) bool {
	i := s.phoneLineIndex(text, at)
	if i < 0 || s.PhoneLines[i].Answered {
		return false
	}
	s.PhoneLines[i].Answered = true
	return true
}

// LastPromptAt is the latest time a prompt matching l can have been
// written (SentFromPhone): phoneLineWindow after its record, or
// queuedLineWindow for a queued line.
func (l PhoneLine) LastPromptAt() time.Time {
	if l.Queued {
		return l.At.Add(queuedLineWindow)
	}
	return l.At.Add(phoneLineWindow)
}

// phoneLineIndex is the index of the newest recorded line that matches text
// written at at by SentFromPhone's rule (fingerprint and window, whether
// answered or not), or -1. An empty text matches nothing.
func (s *Session) phoneLineIndex(text string, at time.Time) int {
	if strings.TrimSpace(text) == "" {
		return -1
	}
	sum := PhoneSum(text)
	for i := len(s.PhoneLines) - 1; i >= 0; i-- {
		l := s.PhoneLines[i]
		if l.Sum == sum && !at.After(l.LastPromptAt()) && at.Sub(l.At) >= -phoneLineWindow {
			return i
		}
	}
	return -1
}
