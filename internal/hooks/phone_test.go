package hooks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/transcript"
)

// promptLine is one user prompt of a test transcript.
type promptLine struct {
	At   time.Time
	Text string
}

// writePrompts writes a transcript whose entries are the given prompts,
// each a user entry stamped with its time.
func writePrompts(t *testing.T, prompts ...promptLine) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.jsonl")
	var b []byte
	for _, p := range prompts {
		b = fmt.Appendf(b, `{"type":"user","timestamp":%q,"message":{"role":"user","content":%q}}`+"\n",
			p.At.Format(time.RFC3339Nano), p.Text)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// checkAt is phoneCheck at now with the real transcript reader.
func checkAt(in *Input, sess *state.Session, now time.Time) (bool, string) {
	return phoneCheck(in, sess, now, transcript.LastPrompt)
}

// The prompt is the relay's: a leading space, as the relay types it, and
// the same words recorded on the session within the window.
func TestPhoneCheckReal(t *testing.T) {
	at := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	sess := &state.Session{}
	sess.RecordPhoneLine("Привет", at, false)

	in := &Input{TranscriptPath: writePrompts(t, promptLine{At: at.Add(time.Second), Text: " Привет"})}
	if ok, why := checkAt(in, sess, at.Add(time.Minute)); !ok || why != "" {
		t.Fatalf("the relay's own prompt: %v %q, want phone-started", ok, why)
	}
	// A later prompt, typed at the Mac, is the turn's prompt now.
	in.TranscriptPath = writePrompts(t,
		promptLine{At: at.Add(time.Second), Text: " Привет"},
		promptLine{At: at.Add(time.Minute), Text: "continue"})
	if ok, _ := checkAt(in, sess, at.Add(time.Minute)); ok {
		t.Fatal("a Mac prompt after the phone's counts as phone-started")
	}
	in.TranscriptPath = filepath.Join(t.TempDir(), "missing.jsonl")
	if ok, _ := checkAt(in, sess, at); ok {
		t.Fatal("a missing transcript counts as phone-started")
	}
	for _, c := range []struct {
		in   *Input
		sess *state.Session
	}{{in, nil}, {nil, sess}, {&Input{}, sess}} {
		if ok, _ := checkAt(c.in, c.sess, at); ok {
			t.Fatalf("input %+v, session %+v counts as phone-started", c.in, c.sess)
		}
	}
}

// A session without phone lines is not phone-started, and its transcript
// is never read; one with phone lines reads it once.
func TestPhoneCheckReadsOnlyWithPhoneLines(t *testing.T) {
	at := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	reads := 0
	read := func(path string) (transcript.Prompt, bool, error) {
		reads++
		return transcript.LastPrompt(path)
	}
	in := &Input{TranscriptPath: filepath.Join(t.TempDir(), "would-fail.jsonl")}
	if ok, why := phoneCheck(in, &state.Session{}, at, read); ok || why != "" || reads != 0 {
		t.Fatalf("no phone lines: %v %q, %d read(s); want false, no reason, no read", ok, why, reads)
	}
	sess := &state.Session{}
	sess.RecordPhoneLine("Привет", at, false)
	if ok, why := phoneCheck(in, sess, at, read); ok || !strings.HasPrefix(why, "transcript: ") || reads != 1 {
		t.Fatalf("with phone lines: %v %q, %d read(s); want the transcript error after one read", ok, why, reads)
	}
}

// A phone-started turn ends an hour after its prompt, and once a prompt of
// the session was answered at the Mac after it (ruling P16). Every way a
// session holding phone lines falls short of phone-started says why, with
// no prompt text.
func TestPhoneCheckReasons(t *testing.T) {
	at := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	prompt := at.Add(time.Second)
	phone := writePrompts(t, promptLine{At: prompt, Text: " deploy the shop"})
	sess := func(macAnswer time.Time) *state.Session {
		s := &state.Session{MacAnswerAt: macAnswer}
		s.RecordPhoneLine("deploy the shop", at, false)
		return s
	}
	unfinished := filepath.Join(t.TempDir(), "unfinished.jsonl")
	b, _ := os.ReadFile(phone)
	if err := os.WriteFile(unfinished, append(b, `{"type":"user","message":{"role":"user","content":"typed at the M`...), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, path string
		sess       *state.Session
		now        time.Time
		ok         bool
		why        string
	}{
		{"59 minutes after the prompt", phone, sess(time.Time{}), prompt.Add(59 * time.Minute), true, ""},
		{"exactly an hour after the prompt", phone, sess(time.Time{}), prompt.Add(time.Hour), true, ""},
		{"61 minutes after the prompt", phone, sess(time.Time{}), prompt.Add(61 * time.Minute), false, "phone prompt is over an hour old"},
		{"a Mac answer after the prompt", phone, sess(prompt.Add(time.Minute)), prompt.Add(2 * time.Minute), false, "answered at the Mac since"},
		{"a Mac answer before the prompt", phone, sess(prompt.Add(-time.Minute)), prompt.Add(2 * time.Minute), true, ""},
		{"a Mac prompt", writePrompts(t, promptLine{At: prompt, Text: "deploy the blog"}), sess(time.Time{}), prompt, false, "last prompt was not sent from the phone"},
		{"no prompt", writePrompts(t), sess(time.Time{}), prompt, false, "no prompt found"},
		{"a last line still being written", unfinished, sess(time.Time{}), prompt, false, "last line still being written"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ok, why := checkAt(&Input{TranscriptPath: c.path}, c.sess, c.now)
			if ok != c.ok || why != c.why {
				t.Fatalf("phoneCheck = %v %q, want %v %q", ok, why, c.ok, c.why)
			}
		})
	}
	ok, why := checkAt(&Input{TranscriptPath: filepath.Join(t.TempDir(), "gone.jsonl")}, sess(time.Time{}), prompt)
	if ok || !strings.HasPrefix(why, "transcript: open ") {
		t.Fatalf("missing transcript: %v %q", ok, why)
	}
}

// An alert held at the gate logs why the turn is not phone-started, once,
// through the hooks' logger, with no prompt text; a session without phone
// lines logs nothing of it.
func TestAlertLogsPhoneCheck(t *testing.T) {
	for _, c := range []struct {
		name   string
		prompt string // "" = no transcript
		lines  bool
		want   string
	}{
		{"a Mac prompt", "deploy the blog", true, "phone check (" + sid + "): last prompt was not sent from the phone"},
		{"no transcript", "", true, "phone check (" + sid + "): transcript: open "},
		{"no phone lines", "deploy the blog", false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.away = false
			var logs []string
			h.deps.Log = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
			in := load(t, "notification_permission.json")
			in.TranscriptPath = filepath.Join(t.TempDir(), "gone.jsonl")
			if c.prompt != "" {
				in.TranscriptPath = writePrompts(t, promptLine{At: h.now, Text: c.prompt})
			}
			if c.lines {
				if _, err := h.store.UpdateSession(sid, func(s *state.Session) { s.RecordPhoneLine("deploy the shop", h.now, false) }); err != nil {
					t.Fatal(err)
				}
			}
			_ = Handle(context.Background(), "notification", in, h.deps)
			if len(h.rec.Calls) != 0 {
				t.Fatalf("calls=%d, want the alert held", len(h.rec.Calls))
			}
			var phone []string
			for _, l := range logs {
				if strings.HasPrefix(l, "phone check") {
					phone = append(phone, l)
				}
			}
			if c.want == "" {
				if len(phone) != 0 {
					t.Fatalf("logged %q", phone)
				}
				return
			}
			if len(phone) != 1 || !strings.HasPrefix(phone[0], c.want) || strings.Contains(phone[0], "deploy") {
				t.Fatalf("phone check log = %q, want one line starting %q", phone, c.want)
			}
			if !slices.ContainsFunc(logs, func(l string) bool { return strings.HasPrefix(l, "alert suppressed (user at keyboard)") }) {
				t.Fatalf("logs = %q, want the suppressed line too", logs)
			}
		})
	}
}
