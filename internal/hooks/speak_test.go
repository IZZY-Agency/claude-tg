package hooks

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/send"
	"github.com/IZZY-Agency/claude-tg/internal/send/sendtest"
	"github.com/IZZY-Agency/claude-tg/internal/session"
	"github.com/IZZY-Agency/claude-tg/internal/speech"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
	"github.com/IZZY-Agency/claude-tg/internal/voice"
)

// shortAnswer is short prose: spoken when it answers a voice note.
const shortAnswer = "Готово, тесты прошли."

// fakeSpeaker stands in for the speech command: it records what it was
// asked to say and answers with audio (the Russian fixture unless set) or
// err, after delay. With block set it waits for its context to end instead.
type fakeSpeaker struct {
	texts, langs []string
	audio        []byte
	err          error
	block        bool
	delay        time.Duration
}

func (f *fakeSpeaker) Speak(ctx context.Context, text, lang string) ([]byte, error) {
	f.texts = append(f.texts, text)
	f.langs = append(f.langs, lang)
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(f.delay):
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.audio, nil
}

// snoop wraps the Recorder to watch the sends as they happen: the order of
// voice and text, and (onText) the state just before the text goes out.
// With blockVoice the voice upload hangs until its context ends.
type snoop struct {
	*sendtest.Recorder
	order      []string
	onText     func()
	blockVoice bool
}

func (s *snoop) SendMessage(ctx context.Context, r telegram.SendRequest) (int64, error) {
	s.order = append(s.order, "text")
	if s.onText != nil {
		s.onText()
	}
	return s.Recorder.SendMessage(ctx, r)
}

func (s *snoop) SendVoice(ctx context.Context, r telegram.VoiceRequest) (int64, error) {
	s.order = append(s.order, "voice")
	if s.blockVoice {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return s.Recorder.SendVoice(ctx, r)
}

// speakHarness is the hooks harness with a fake speech command, a snoop on
// the sends and the hook's log lines collected.
type speakHarness struct {
	*harness
	sp         *fakeSpeaker
	sn         *snoop
	logs       []string
	transcript string // the Stop's transcript_path when set
}

func newSpeakHarness(t *testing.T) *speakHarness {
	t.Helper()
	h := &speakHarness{harness: newHarness(t), sp: &fakeSpeaker{audio: fixture(t)}}
	h.sn = &snoop{Recorder: h.rec}
	h.deps.Sender.API = h.sn
	h.deps.Speaker = h.sp
	h.deps.Log = func(format string, args ...any) { h.logs = append(h.logs, fmt.Sprintf(format, args...)) }
	return h
}

// fixture is the OGG/Opus file a speech command might write, kept as
// base64 text (the public snapshot holds text files only).
func fixture(t *testing.T) []byte {
	t.Helper()
	b64, err := os.ReadFile(filepath.Join("..", "speech", "testdata", "ru.ogg.b64"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(b64)), ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// owe makes the session owe the phone one answer and marks it voice when
// markedAt is set.
func (h *speakHarness) owe(t *testing.T, markedAt time.Time) {
	t.Helper()
	_, err := h.store.UpdateSession(sid, func(s *state.Session) {
		s.PendingAnswers++
		if !markedAt.IsZero() {
			s.MarkVoice("", "", markedAt)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

// mark gives the session one voice mark made now.
func (h *speakHarness) mark(t *testing.T) {
	t.Helper()
	if _, err := h.store.UpdateSession(sid, func(s *state.Session) { s.MarkVoice("", "", h.now) }); err != nil {
		t.Fatal(err)
	}
}

// stop runs the Stop hook on answer.
func (h *speakHarness) stop(t *testing.T, answer string) error {
	t.Helper()
	return h.stopCtx(t, context.Background(), answer)
}

func (h *speakHarness) stopCtx(t *testing.T, ctx context.Context, answer string) error {
	t.Helper()
	in := load(t, "stop.json")
	in.LastAssistantMessage = answer
	if h.transcript != "" {
		in.TranscriptPath = h.transcript
	}
	return Handle(ctx, "stop", in, h.deps)
}

func (h *speakHarness) session(t *testing.T) *state.Session {
	t.Helper()
	s, err := h.store.LoadSession(sid)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (h *speakHarness) logged(line string) bool { return slices.Contains(h.logs, line) }

// textOnly checks that the answer went out as one 💬 text message and
// nothing was spoken or uploaded.
func (h *speakHarness) textOnly(t *testing.T) {
	t.Helper()
	if len(h.rec.Voices) != 0 || len(h.rec.Calls) != 1 || !strings.HasPrefix(h.rec.Calls[0].Text, "💬 <b>acme-shop-92</b>\n") {
		t.Fatalf("voices=%d calls=%+v, want the text only", len(h.rec.Voices), h.rec.Calls)
	}
}

func TestStopSpeaksMarkedShortAnswer(t *testing.T) {
	h := newSpeakHarness(t)
	h.owe(t, h.now)
	if err := h.stop(t, shortAnswer); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.sn.order, []string{"voice", "text"}) {
		t.Fatalf("order=%v, want the voice before the text", h.sn.order)
	}
	v := h.rec.Voices[0]
	if v.Caption != "🔊 acme-shop-92" || !v.Silent || v.Duration != 2 || !bytes.Equal(v.Audio, fixture(t)) {
		t.Fatalf("voice caption=%q silent=%v duration=%d audio=%d bytes", v.Caption, v.Silent, v.Duration, len(v.Audio))
	}
	if c := h.rec.Calls[0]; c.Text != "💬 <b>acme-shop-92</b>\n"+shortAnswer || !c.Silent {
		t.Fatalf("text=%q silent=%v", c.Text, c.Silent)
	}
	if !slices.Equal(h.sp.texts, []string{shortAnswer}) || !slices.Equal(h.sp.langs, []string{"ru"}) {
		t.Fatalf("speaker got texts=%q langs=%q", h.sp.texts, h.sp.langs)
	}
	if s := h.session(t); len(s.VoiceMarks) != 0 || s.PendingAnswers != 0 {
		t.Fatalf("mark or answer not spent: %+v", s)
	}
	d, err := speech.Duration(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("spoke 21 characters (%.1f s) for acme-shop-92", d.Seconds()); !h.logged(want) {
		t.Fatalf("log=%q, want %q", h.logs, want)
	}
}

func TestStopLongAnswerTextOnly(t *testing.T) {
	h := newSpeakHarness(t)
	h.owe(t, h.now)
	long := strings.Repeat("Готово. ", 75) // 600 runes of prose
	if err := h.stop(t, long); err != nil {
		t.Fatal(err)
	}
	h.textOnly(t)
	if want := []string{"voice check (acme-shop-92): text (long or mostly code)"}; len(h.sp.texts) != 0 || !slices.Equal(h.logs, want) {
		t.Fatalf("a long answer was spoken: %d calls, log=%q, want %q", len(h.sp.texts), h.logs, want)
	}
	if s := h.session(t); len(s.VoiceMarks) != 0 {
		t.Fatalf("mark not spent: %+v", s.VoiceMarks)
	}
}

func TestStopUnmarkedAnswerTextOnly(t *testing.T) {
	h := newSpeakHarness(t)
	h.owe(t, time.Time{})
	if err := h.stop(t, shortAnswer); err != nil {
		t.Fatal(err)
	}
	h.textOnly(t)
	if len(h.sp.texts) != 0 || len(h.logs) != 0 {
		t.Fatalf("an answer to a typed message was spoken: %q, log=%q", h.sp.texts, h.logs)
	}
}

// Only the answer to the voice note is spoken in a followed session; the
// next answer is posted as text, as every followed answer is.
func TestStopFollowedSessionWithMark(t *testing.T) {
	h := newSpeakHarness(t)
	h.follow(t)
	h.mark(t)
	if err := h.stop(t, shortAnswer); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.sn.order, []string{"voice", "text"}) || len(h.sp.texts) != 1 {
		t.Fatalf("first answer: order=%v speaks=%d", h.sn.order, len(h.sp.texts))
	}
	if err := h.stop(t, shortAnswer); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.sn.order, []string{"voice", "text", "text"}) || len(h.sp.texts) != 1 {
		t.Fatalf("second answer: order=%v speaks=%d, want text only", h.sn.order, len(h.sp.texts))
	}
	if s := h.session(t); len(s.VoiceMarks) != 0 || !s.Follow {
		t.Fatalf("session=%+v", s)
	}
}

// fileState is a session file's bytes and identity: writeAtomic replaces the
// file, so any write gives it a new identity even when the bytes match.
type fileState struct {
	b  []byte
	fi os.FileInfo
}

func readFileState(t *testing.T, path string) fileState {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fileState{b, fi}
}

// A followed session without marks is posted exactly as before spoken
// answers: the hook writes nothing of its own to the session (only the
// text's own message id is recorded once it is sent) and speaks nothing.
func TestStopFollowedNoMarkNoWrite(t *testing.T) {
	h := newSpeakHarness(t)
	h.follow(t)
	path := filepath.Join(h.store.Dir, "sessions", sid+".json")
	var before, atText fileState
	var resolved *state.Session
	resolve := h.deps.Resolve
	h.deps.Resolve = func(ctx context.Context, env session.Env) (*state.Session, error) {
		s, err := resolve(ctx, env)
		before = readFileState(t, path)
		resolved = s
		return s, err
	}
	h.sn.onText = func() { atText = readFileState(t, path) }
	if err := h.stop(t, shortAnswer); err != nil {
		t.Fatal(err)
	}
	h.textOnly(t)
	if len(h.sp.texts) != 0 {
		t.Fatalf("spoke without a mark: %q", h.sp.texts)
	}
	if !bytes.Equal(before.b, atText.b) || !os.SameFile(before.fi, atText.fi) {
		t.Fatalf("the hook wrote the session before the text:\nbefore %s\nat the text %s", before.b, atText.b)
	}
	after := h.session(t)
	if !slices.Equal(after.LastMessageIDs, []int64{1}) {
		t.Fatalf("text's message id not recorded: %v", after.LastMessageIDs)
	}
	after.LastMessageIDs = nil
	if !reflect.DeepEqual(after, resolved) {
		t.Fatalf("session changed beyond the text's message id:\nbefore %+v\nafter  %+v", *resolved, *after)
	}
}

// A mark left by a turn interrupted at the Mac lapses: an hour later the
// next answer is text only.
func TestStopLapsedMark(t *testing.T) {
	h := newSpeakHarness(t)
	h.owe(t, h.now.Add(-61*time.Minute))
	if err := h.stop(t, shortAnswer); err != nil {
		t.Fatal(err)
	}
	h.textOnly(t)
	if len(h.sp.texts) != 0 {
		t.Fatalf("spoke on a lapsed mark: %q", h.sp.texts)
	}
	if s := h.session(t); len(s.VoiceMarks) != 0 || s.PendingAnswers != 0 {
		t.Fatalf("lapsed mark or answer left: %+v", s)
	}
}

// The speech command gets the text a 💬 message would show: the bot token
// scrubbed and credentials redacted.
func TestStopScrubsSecret(t *testing.T) {
	const token = "123456:ABCDEFsecretbot"
	for _, tc := range []struct{ answer, secret string }{
		{"token is " + token + ", done.", "ABCDEFsecretbot"},
		{"Connected with password=s3cr3tvalue, done.", "s3cr3tvalue"},
	} {
		t.Run(tc.secret, func(t *testing.T) {
			h := newSpeakHarness(t)
			h.deps.Sender.Secrets = []string{token}
			h.owe(t, h.now)
			if err := h.stop(t, tc.answer); err != nil {
				t.Fatal(err)
			}
			if len(h.sp.texts) != 1 || !strings.Contains(h.sp.texts[0], "[redacted]") {
				t.Fatalf("speaker got %q", h.sp.texts)
			}
			if strings.Contains(h.sp.texts[0], tc.secret) || strings.Contains(h.sp.texts[0], token) {
				t.Fatalf("the secret reached the speech command: %q", h.sp.texts[0])
			}
			if strings.Contains(strings.Join(h.logs, "\n"), tc.secret) {
				t.Fatalf("the secret reached the log: %q", h.logs)
			}
		})
	}
}

// A failing speech command costs only the voice: the text goes out as
// always and the phone sees no error.
func TestStopSpeakerFails(t *testing.T) {
	h := newSpeakHarness(t)
	h.sp.err = &voice.Error{Reason: "boom"}
	h.owe(t, h.now)
	if err := h.stop(t, shortAnswer); err != nil {
		t.Fatal(err)
	}
	h.textOnly(t)
	if !h.logged("speech: boom") {
		t.Fatalf("log=%q", h.logs)
	}
	if s := h.session(t); len(s.VoiceMarks) != 0 {
		t.Fatalf("mark not spent: %+v", s.VoiceMarks)
	}
}

func TestStopSpeakerNotOpus(t *testing.T) {
	h := newSpeakHarness(t)
	h.sp.audio = []byte("RIFF\x24\x00\x00\x00WAVEfmt ")
	h.owe(t, h.now)
	if err := h.stop(t, shortAnswer); err != nil {
		t.Fatal(err)
	}
	h.textOnly(t)
	if !h.logged("speech: speech_command did not write an OGG/Opus file") {
		t.Fatalf("log=%q", h.logs)
	}
}

// A failed voice upload is dropped, never spooled; the text is sent, or
// spooled as usual when Telegram cannot be reached at all.
func TestStopVoiceUploadFails(t *testing.T) {
	netErr := &telegram.NetError{Err: errors.New("dial tcp: connection refused")}
	refused := &telegram.APIError{Code: 400, Description: "Bad Request: wrong file"}
	for _, tc := range []struct {
		name   string
		script []error
		sent   bool
	}{
		{"network", []error{netErr, nil}, true},
		{"refused", []error{refused, nil}, true},
		{"outage", []error{netErr, netErr}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSpeakHarness(t)
			h.rec.Script = tc.script
			h.owe(t, h.now)
			err := h.stop(t, shortAnswer)
			if !slices.Equal(h.sn.order, []string{"voice", "text"}) || len(h.rec.Calls) != 1 {
				t.Fatalf("order=%v calls=%d", h.sn.order, len(h.rec.Calls))
			}
			if !slices.ContainsFunc(h.logs, func(l string) bool { return strings.HasPrefix(l, "speech: ") }) {
				t.Fatalf("voice failure not logged: %q", h.logs)
			}
			items, lerr := h.store.SpoolList()
			if lerr != nil {
				t.Fatal(lerr)
			}
			if tc.sent {
				if err != nil || len(items) != 0 {
					t.Fatalf("err=%v spool=%+v, want the text sent and nothing spooled", err, items)
				}
				return
			}
			if !errors.Is(err, send.ErrSpooled) || len(items) != 1 || !strings.HasPrefix(items[0].Req.Text, "💬 <b>acme-shop-92</b>\n") {
				t.Fatalf("err=%v spool=%+v, want only the text spooled", err, items)
			}
		})
	}
}

// Without a speech command a marked answer is text only, and its mark is
// spent all the same.
func TestStopSpeakerNil(t *testing.T) {
	h := newSpeakHarness(t)
	h.deps.Speaker = nil
	h.owe(t, h.now)
	if err := h.stop(t, shortAnswer); err != nil {
		t.Fatal(err)
	}
	h.textOnly(t)
	if s := h.session(t); len(s.VoiceMarks) != 0 || s.PendingAnswers != 0 {
		t.Fatalf("mark or answer not spent: %+v", s)
	}
}

// A speech command that hangs and leaves a background child: its whole
// process group dies at the timeout and the text still goes out.
func TestStopSpeechHangs(t *testing.T) {
	h := newSpeakHarness(t)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("TG_TEST_PIDFILE", pidFile)
	h.deps.Speaker = &voice.Runner{
		Command: `sleep 60 & echo $! > "$TG_TEST_PIDFILE"; sleep 60`,
		Dir:     t.TempDir(),
		Timeout: 300 * time.Millisecond,
	}
	h.owe(t, h.now)
	start := time.Now()
	if err := h.stop(t, shortAnswer); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the text waited %s", took)
	}
	h.textOnly(t)
	if !h.logged("speech: timed out after 300ms") {
		t.Fatalf("log=%q", h.logs)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		t.Fatalf("pid file %q: %v", b, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	for deadline := time.Now().Add(5 * time.Second); !processGone(pid); {
		if time.Now().After(deadline) {
			t.Fatalf("background child %d outlived the speech timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// processGone reports whether pid no longer runs: kill(2) finds no
// process, or, on Linux, a zombie. A killed orphan stays a zombie that
// kill(2) still finds until the first process reaps it, which never
// happens in a container whose first process is `go test` itself.
func processGone(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return true
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// The state follows the command name, in parentheses that may hold
	// spaces or parentheses of their own.
	i := bytes.LastIndexByte(b, ')')
	return i >= 0 && i+2 < len(b) && (b[i+2] == 'Z' || b[i+2] == 'X')
}

// However long speech and its upload take, they leave the text its
// reserve (Deps.TextReserve), so it is sent, not spooled.
func TestStopSpeechLeavesTextReserve(t *testing.T) {
	h := newSpeakHarness(t)
	h.sp.block = true
	h.deps.TextReserve = time.Second
	h.owe(t, h.now)
	ctx, cancel := context.WithTimeout(context.Background(), h.deps.TextReserve+300*time.Millisecond)
	defer cancel()
	if err := h.stopCtx(t, ctx, shortAnswer); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("speech used the text's reserve")
	}
	h.textOnly(t)
	if !h.logged("speech: " + context.DeadlineExceeded.Error()) {
		t.Fatalf("log=%q", h.logs)
	}
}

// A budget that is all the text's reserve leaves no time for speech: the
// speech command is not started and the text goes out.
func TestStopNoTimeForSpeech(t *testing.T) {
	h := newSpeakHarness(t)
	h.deps.TextReserve = time.Second
	h.owe(t, h.now)
	ctx, cancel := context.WithTimeout(context.Background(), h.deps.TextReserve/2)
	defer cancel()
	if err := h.stopCtx(t, ctx, shortAnswer); err != nil {
		t.Fatal(err)
	}
	h.textOnly(t)
	if len(h.sp.texts) != 0 || !h.logged("speech: no time left before the text") {
		t.Fatalf("speaks=%d log=%q", len(h.sp.texts), h.logs)
	}
}

// A session with no name is "mac" in the log, as in the voice's caption.
func TestStopSpokenUnnamedSession(t *testing.T) {
	h := newSpeakHarness(t)
	h.deps.Resolve = func(ctx context.Context, env session.Env) (*state.Session, error) {
		return h.store.LoadSession(env.SessionID)
	}
	h.owe(t, h.now)
	if err := h.stop(t, shortAnswer); err != nil {
		t.Fatal(err)
	}
	if len(h.rec.Voices) != 1 || h.rec.Voices[0].Caption != "🔊 mac" {
		t.Fatalf("voices=%+v", h.rec.Voices)
	}
	d, err := speech.Duration(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("spoke 21 characters (%.1f s) for mac", d.Seconds()); !h.logged(want) {
		t.Fatalf("log=%q, want %q", h.logs, want)
	}
}

// A failed turn answers nothing: its marks go, whether or not the owner is
// at the keyboard, and the alert behaves as before.
func TestStopFailureClearsMarks(t *testing.T) {
	for _, away := range []bool{true, false} {
		t.Run(fmt.Sprintf("away=%v", away), func(t *testing.T) {
			h := newSpeakHarness(t)
			h.away = away
			h.mark(t)
			h.mark(t)
			if err := Handle(context.Background(), "stop-failure", load(t, "stop_failure.json"), h.deps); err != nil {
				t.Fatal(err)
			}
			if s := h.session(t); len(s.VoiceMarks) != 0 {
				t.Fatalf("marks left: %+v", s.VoiceMarks)
			}
			if !away {
				if len(h.rec.Calls) != 0 {
					t.Fatalf("alert sent while at the keyboard: %+v", h.rec.Calls)
				}
				return
			}
			if len(h.rec.Calls) != 1 || h.rec.Calls[0].Text != "💥 <b>acme-shop-92</b>\nstopped: <code>rate_limit</code>, 429 Too Many Requests" {
				t.Fatalf("calls=%+v", h.rec.Calls)
			}
		})
	}
}

// The mark is spent with the answer only when that write is saved: when
// the session record cannot be written nothing is spoken (the mark is
// still there) and the text goes out as before, followed or not.
func TestStopSessionWriteFailsNotSpoken(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	for _, followed := range []bool{false, true} {
		t.Run(fmt.Sprintf("followed=%v", followed), func(t *testing.T) {
			h := newSpeakHarness(t)
			if followed {
				h.follow(t)
				h.mark(t)
			} else {
				h.owe(t, h.now)
			}
			dir := filepath.Join(h.store.Dir, "sessions")
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			if err := h.stop(t, shortAnswer); err != nil {
				t.Fatal(err)
			}
			_ = os.Chmod(dir, 0o700)
			h.textOnly(t)
			if len(h.sp.texts) != 0 {
				t.Fatalf("spoke on a mark the hook could not spend: %q", h.sp.texts)
			}
			if s := h.session(t); len(s.VoiceMarks) != 1 {
				t.Fatalf("marks = %+v, want the unspent one", s.VoiceMarks)
			}
		})
	}
}

// An unfollowed session owes an answer only as the write that spends it
// finds: when that write fails before it can look (its lock file cannot
// be created, say), nothing is sent and nothing is spoken.
func TestStopSessionUnreadableOwesNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	h := newSpeakHarness(t)
	dir := filepath.Join(h.store.Dir, "sessions")
	if _, err := os.Stat(filepath.Join(dir, sid+".json.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock file already there: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := h.stop(t, shortAnswer); err != nil {
		t.Fatal(err)
	}
	if len(h.rec.Calls) != 0 || len(h.rec.Voices) != 0 || len(h.sp.texts) != 0 {
		t.Fatalf("calls=%+v voices=%d speaks=%d, want nothing sent", h.rec.Calls, len(h.rec.Voices), len(h.sp.texts))
	}
}

// The log shows the speech command's reason and the session name as a
// message would: the bot token scrubbed, credentials redacted.
func TestStopSpeechLogScrubbed(t *testing.T) {
	const token = "123456:ABCDEFsecretbot"
	t.Run("reason", func(t *testing.T) {
		h := newSpeakHarness(t)
		h.deps.Sender.Secrets = []string{token}
		h.sp.err = &voice.Error{Reason: "auth failed for " + token + " password=s3cr3tvalue"}
		h.owe(t, h.now)
		if err := h.stop(t, shortAnswer); err != nil {
			t.Fatal(err)
		}
		h.textOnly(t)
		if !h.logged("speech: auth failed for [redacted] password=[redacted]") {
			t.Fatalf("log=%q", h.logs)
		}
	})
	t.Run("session name", func(t *testing.T) {
		h := newSpeakHarness(t)
		h.deps.Sender.Secrets = []string{token}
		if _, err := h.store.UpdateSession(sid, func(s *state.Session) { s.Name = "acme " + token }); err != nil {
			t.Fatal(err)
		}
		h.owe(t, h.now)
		if err := h.stop(t, shortAnswer); err != nil {
			t.Fatal(err)
		}
		d, err := speech.Duration(fixture(t))
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("spoke 21 characters (%.1f s) for acme [redacted]", d.Seconds()); !h.logged(want) {
			t.Fatalf("log=%q, want %q", h.logs, want)
		}
		if strings.Contains(strings.Join(h.logs, "\n"), "ABCDEFsecretbot") {
			t.Fatalf("the token reached the log: %q", h.logs)
		}
	})
}

// Speech and the voice upload share one window (Deps.SpeechWindow, 30 s
// by default) from the start of the speech command, however much budget
// is left: a voice not uploaded by its end is dropped and the text follows.
func TestStopSpeechWindow(t *testing.T) {
	if got := (Deps{}).speechWindow(); got != 30*time.Second {
		t.Fatalf("default window %s, want 30s", got)
	}
	h := newSpeakHarness(t)
	h.deps.SpeechWindow = 300 * time.Millisecond
	h.deps.TextReserve = time.Second
	h.sp.delay = 200 * time.Millisecond // most of the window
	h.sn.blockVoice = true              // the upload would take longer than what is left
	h.owe(t, h.now)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := h.stopCtx(t, ctx, shortAnswer); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < h.deps.SpeechWindow || took > 2*time.Second {
		t.Fatalf("the text came after %s, want the %s window and little more", took, h.deps.SpeechWindow)
	}
	if !slices.Equal(h.sn.order, []string{"voice", "text"}) || len(h.sp.texts) != 1 {
		t.Fatalf("order=%v speaks=%d", h.sn.order, len(h.sp.texts))
	}
	h.textOnly(t)
	if !h.logged("speech: " + context.DeadlineExceeded.Error()) {
		t.Fatalf("log=%q", h.logs)
	}
	if items, err := h.store.SpoolList(); err != nil || len(items) != 0 {
		t.Fatalf("spool=%+v err=%v, want the text sent", items, err)
	}
}

// While older messages are still undelivered (Telegram unreachable), the
// answer is not spoken: its voice would overtake them. The spool is flushed
// first; the text then queues behind what is left, as always.
func TestStopSpeechSkippedBehindBacklog(t *testing.T) {
	h := newSpeakHarness(t)
	netErr := &telegram.NetError{Err: errors.New("dial tcp: connection refused")}
	h.rec.Script = []error{netErr, netErr} // the hook's flush, then the text's
	older := state.SpoolItem{ID: "1-00-aaaaaa", SessionID: sid, Created: time.Now().Add(-time.Hour),
		Req: telegram.SendRequest{ChatID: 1, Text: "🔔 <b>acme-shop-92</b>\nolder", ParseMode: "HTML"}, Plain: "🔔 acme-shop-92\nolder"}
	if err := h.store.SpoolPut(older); err != nil {
		t.Fatal(err)
	}
	h.owe(t, h.now)
	err := h.stop(t, shortAnswer)
	if !errors.Is(err, send.ErrSpooled) {
		t.Fatalf("err=%v, want the text spooled", err)
	}
	if len(h.sp.texts) != 0 || len(h.rec.Voices) != 0 {
		t.Fatalf("spoke behind a backlog: speaks=%d voices=%d", len(h.sp.texts), len(h.rec.Voices))
	}
	if !h.logged("speech: older messages are still undelivered") {
		t.Fatalf("log=%q", h.logs)
	}
	items, err := h.store.SpoolList()
	if err != nil || len(items) != 2 || items[0].ID != older.ID || !strings.HasPrefix(items[1].Req.Text, "💬 <b>acme-shop-92</b>\n") {
		t.Fatalf("spool=%+v err=%v, want the text behind the older message", items, err)
	}
	if s := h.session(t); len(s.VoiceMarks) != 0 {
		t.Fatalf("mark not spent: %+v", s.VoiceMarks)
	}
}
