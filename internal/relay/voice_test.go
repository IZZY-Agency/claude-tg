package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/relay/relaytest"
	"github.com/IZZY-Agency/claude-tg/internal/session"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

// voiceAudio is the voice note's file on the fake Telegram.
const voiceAudio = "OggS a voice note"

// transcriber is a fake Transcriber: it returns text or err. With block
// set it waits until block is closed or its ctx ends; when its ctx ends it
// says so on cancelled (when set), waits for finish to be closed (when set:
// the command's group being killed, its directory removed), then records
// that it ended. With deaf it waits for block only, like a command that
// ignores its signal. started, when set, gets a value as each call begins.
type transcriber struct {
	text      string
	err       error
	block     chan struct{}
	deaf      bool
	started   chan struct{}
	cancelled chan struct{}
	finish    chan struct{}
	then      func() // runs as a call returns its result

	mu    sync.Mutex
	calls []transcribeCall
	ended bool
}

type transcribeCall struct{ audio, ext string }

func (t *transcriber) Transcribe(ctx context.Context, audio []byte, ext string) (string, error) {
	t.mu.Lock()
	t.calls = append(t.calls, transcribeCall{string(audio), ext})
	t.mu.Unlock()
	if t.started != nil {
		t.started <- struct{}{}
	}
	if t.block != nil {
		done := ctx.Done()
		if t.deaf {
			done = nil
		}
		select {
		case <-t.block:
		case <-done:
			if t.cancelled != nil {
				t.cancelled <- struct{}{}
			}
			if t.finish != nil {
				<-t.finish
			}
			t.mu.Lock()
			t.ended = true
			t.mu.Unlock()
			return "", ctx.Err()
		}
	}
	if t.then != nil {
		t.then()
	}
	return t.text, t.err
}

// signalError is a command killed by a signal it did not get from the
// relay, as voice.Error reports it.
type signalError struct{}

func (signalError) Error() string  { return "killed by signal 15" }
func (signalError) Signaled() bool { return true }

// finishOnCancel lets the transcription finish its cleanup as soon as it
// says its ctx ended.
func (tr *transcriber) finishOnCancel(t *testing.T) {
	go func() {
		defer close(tr.finish)
		select {
		case <-tr.cancelled:
		case <-time.After(5 * time.Second):
			t.Error("the transcription never saw its ctx end")
		}
	}()
}

// sawEnd reports whether a call saw its ctx end and finished cleaning up.
func (t *transcriber) sawEnd() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ended
}

func (t *transcriber) called() []transcribeCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]transcribeCall(nil), t.calls...)
}

// newVoiceFixture is the router fixture with the note's file on the fake
// Telegram and tr as the configured Transcriber (nil: not configured).
func newVoiceFixture(t *testing.T, tr *transcriber) *fixture {
	t.Helper()
	f := newFixture(t)
	f.api.Files = map[string][]byte{"voice-1": []byte(voiceAudio)}
	if tr != nil {
		f.r.Transcriber = tr
	}
	return f
}

// voice is a voice note of seconds from the owner, a swipe-reply to
// replyTo when non-zero.
func (f *fixture) voice(seconds int, replyTo int64) telegram.Update {
	u := f.msg("", replyTo)
	u.Message.Voice = &telegram.Voice{FileID: "voice-1", FileUniqueID: "uniq-1", Duration: seconds, MimeType: "audio/ogg", FileSize: int64(len(voiceAudio))}
	return u
}

// handleVoice hands the note to the router and waits until it is handled.
func (f *fixture) handleVoice(t *testing.T, u telegram.Update) {
	t.Helper()
	f.handle(t, u)
	f.r.WaitVoice()
}

// parked counts the parked texts on disk.
func (f *fixture) parked(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.store.Dir, "pending"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

var keyRe = regexp.MustCompile(`^v:([0-9a-f]{8}):n$`)

// confirmKey returns the parking key a confirmation's ✖️ Cancel carries,
// and checks every callback fits Telegram's 64 bytes.
func confirmKey(t *testing.T, kb [][]telegram.Button) string {
	t.Helper()
	key := ""
	for _, row := range kb {
		for _, b := range row {
			if len(b.Data) > 64 {
				t.Fatalf("callback data over 64 bytes: %q", b.Data)
			}
			if m := keyRe.FindStringSubmatch(b.Data); m != nil {
				key = m[1]
			}
		}
	}
	if key == "" {
		t.Fatalf("no ✖️ Cancel in %+v", kb)
	}
	return key
}

// within runs fn and fails the test when it does not return within d.
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %v", what, d)
	}
}

func TestVoiceUnsetReplies(t *testing.T) {
	f := newVoiceFixture(t, nil)
	f.handleVoice(t, f.voice(7, 101))
	if len(f.api.Sent) != 1 || f.api.Sent[0].ReplyToMessageID != 500 ||
		f.api.Sent[0].Text != "voice notes need <code>voice_command</code> in tg's config; see the README" {
		t.Fatalf("sent = %+v", f.api.Sent)
	}
	if len(f.api.Reacted()) != 0 || len(f.api.FileCalls) != 0 || f.parked(t) != 0 {
		t.Fatalf("reactions = %+v file calls = %v parked = %d", f.api.Reacted(), f.api.FileCalls, f.parked(t))
	}
}

func TestVoiceTooLongDuration(t *testing.T) {
	tr := &transcriber{text: "please rerun"}
	f := newVoiceFixture(t, tr)
	f.handleVoice(t, f.voice(301, 101))
	if len(f.api.Sent) != 1 || f.api.Sent[0].Text != "too long: 6 min, the limit is 5" || f.api.Sent[0].ReplyToMessageID != 500 {
		t.Fatalf("sent = %+v", f.api.Sent)
	}
	if len(f.api.Reacted()) != 0 || len(f.api.FileCalls) != 0 || len(tr.called()) != 0 {
		t.Fatalf("reactions = %+v file calls = %v transcribed = %v", f.api.Reacted(), f.api.FileCalls, tr.called())
	}
	// Five minutes exactly is within the limit.
	f.handleVoice(t, f.voice(300, 101))
	if len(f.api.FileCalls) != 1 || len(tr.called()) != 1 {
		t.Fatalf("a 300 s note was refused: file calls = %v", f.api.FileCalls)
	}
}

func TestVoiceTooBigRefused(t *testing.T) {
	tr := &transcriber{text: "please rerun"}
	f := newVoiceFixture(t, tr)
	u := f.voice(7, 101)
	u.Message.Voice.FileSize = 20<<20 + 1
	f.handleVoice(t, u)
	if len(f.api.Sent) != 1 || f.api.Sent[0].Text != "too big for a bot to download (over 20 MB)" {
		t.Fatalf("sent = %+v", f.api.Sent)
	}
	if len(f.api.Reacted()) != 0 || len(f.api.FileCalls) != 0 {
		t.Fatalf("reactions = %+v file calls = %v", f.api.Reacted(), f.api.FileCalls)
	}
}

func TestVoiceKnownTargetConfirms(t *testing.T) {
	tr := &transcriber{text: "please rerun"}
	f := newVoiceFixture(t, tr)
	f.handleVoice(t, f.voice(7, 101))
	if want := []relaytest.Reaction{{MessageID: 500, Emoji: "👀"}}; !reflect.DeepEqual(f.api.Reacted(), want) {
		t.Fatalf("reactions = %+v, want %+v", f.api.Reacted(), want)
	}
	if len(f.api.Sent) != 1 {
		t.Fatalf("sent = %+v", f.api.Sent)
	}
	s := f.api.Sent[0]
	if s.ReplyToMessageID != 500 || s.ParseMode != "HTML" || s.Text != "🎙 to <b>acme-shop-92</b>:\nplease rerun" {
		t.Fatalf("confirmation = %+v", s)
	}
	k := confirmKey(t, s.InlineKeyboard)
	want := [][]telegram.Button{
		{{Text: "✅ Send", Data: "v:" + k + ":y"}, {Text: "✖️ Cancel", Data: "v:" + k + ":n"}},
		{{Text: "↪ Other session", Data: "v:" + k + ":o"}},
	}
	if !reflect.DeepEqual(s.InlineKeyboard, want) {
		t.Fatalf("keyboard = %+v, want %+v", s.InlineKeyboard, want)
	}
	// The note's own audio and extension reach the command.
	if c := tr.called(); len(c) != 1 || c[0].audio != voiceAudio || c[0].ext != ".oga" {
		t.Fatalf("transcribed = %+v", c)
	}
	if len(f.term.Typed) != 0 {
		t.Fatalf("typed before a tap: %+v", f.term.Typed)
	}
	p, ok := f.store.TakePending(k, f.now, pendingTTL)
	if !ok || !p.Voice || p.SessionID != sidShop || p.MessageID != 500 || p.Text != "please rerun" ||
		p.Header != "🎙 to <b>acme-shop-92</b>:" || !p.Created.Equal(f.now) || p.AskID != "" {
		t.Fatalf("parked = %+v ok = %v", p, ok)
	}
}

func TestVoiceNoTargetConfirms(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "please rerun"})
	_, _ = f.store.UpdateSession(sidBlog, func(s *state.Session) { s.Follow = false })
	f.handleVoice(t, f.voice(7, 0))
	if len(f.api.Sent) != 1 {
		t.Fatalf("sent = %+v", f.api.Sent)
	}
	s := f.api.Sent[0]
	if s.ReplyToMessageID != 500 || s.Text != "🎙 Which session?\nplease rerun" {
		t.Fatalf("confirmation = %+v", s)
	}
	k := confirmKey(t, s.InlineKeyboard)
	want := [][]telegram.Button{
		{{Text: "acme-shop-92 · idle", Data: "s:" + sidShop + ":" + k}},
		{{Text: "acme-shop-fa · busy", Data: "s:" + sidShopF + ":" + k}},
		{{Text: "blog-6a · idle", Data: "s:" + sidBlog + ":" + k}},
		{{Text: "✖️ Cancel", Data: "v:" + k + ":n"}},
	}
	if !reflect.DeepEqual(s.InlineKeyboard, want) {
		t.Fatalf("keyboard = %+v, want %+v", s.InlineKeyboard, want)
	}
	p, ok := f.store.TakePending(k, f.now, pendingTTL)
	if !ok || !p.Voice || p.SessionID != "" || p.AskID != "" || p.MessageID != 500 || p.Text != "please rerun" || p.Header != "🎙 Which session?" {
		t.Fatalf("parked = %+v ok = %v", p, ok)
	}
}

// With no session to offer on the keyboard the note is refused, as text is.
func TestVoiceNoTypeableSessionRefused(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "please rerun"})
	f.r.Agents = relaytest.AgentsOf(session.Agent{SessionID: "c0c0c0c0-0000-4000-8000-000000000005", Name: "acme-cli", Kind: "interactive", Status: "idle", Cwd: "/Users/alex/code/acme-cli"})
	f.handleVoice(t, f.voice(7, 0))
	if len(f.api.Sent) != 1 || f.api.Sent[0].Text != "no live session has a terminal pane I can type into" || f.api.Sent[0].InlineKeyboard != nil {
		t.Fatalf("sent = %+v", f.api.Sent)
	}
	if want := []relaytest.Reaction{{MessageID: 500, Emoji: "👀"}, {MessageID: 500, Emoji: ""}}; !reflect.DeepEqual(f.api.Reacted(), want) {
		t.Fatalf("reactions = %+v, want %+v", f.api.Reacted(), want)
	}
	if f.parked(t) != 0 {
		t.Fatal("a refused note was parked")
	}
}

func TestVoiceAskReplyConfirms(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "Mango"})
	f.putAsk(t, questionAsk(t, 710, 711))
	f.handleVoice(t, f.voice(7, 711))
	if len(f.api.Sent) != 1 {
		t.Fatalf("sent = %+v", f.api.Sent)
	}
	s := f.api.Sent[0]
	if s.ReplyToMessageID != 500 || s.Text != "🎙 answer to <b>acme-shop-92</b>:\nMango" {
		t.Fatalf("confirmation = %+v", s)
	}
	k := confirmKey(t, s.InlineKeyboard)
	want := [][]telegram.Button{{{Text: "✅ Send", Data: "v:" + k + ":y"}, {Text: "✖️ Cancel", Data: "v:" + k + ":n"}}}
	if !reflect.DeepEqual(s.InlineKeyboard, want) {
		t.Fatalf("keyboard = %+v, want %+v", s.InlineKeyboard, want)
	}
	// Nothing is answered before Send.
	if a := f.loadAsk(t, askQ); answerOf(t, a, 1) != nil || a.Done || len(f.api.Edits) != 0 {
		t.Fatalf("ask answered before a tap: answers = %s edits = %+v", a.Answers, f.api.Edits)
	}
	p, ok := f.store.TakePending(k, f.now, pendingTTL)
	if !ok || !p.Voice || p.AskID != askQ || p.AskIndex != 1 || p.SessionID != "" || p.MessageID != 500 ||
		p.Text != "Mango" || p.Header != "🎙 answer to <b>acme-shop-92</b>:" {
		t.Fatalf("parked = %+v ok = %v", p, ok)
	}
}

func TestVoiceEndedSessionRefused(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "hello?"})
	f.handleVoice(t, f.voice(7, 200))
	if len(f.api.Sent) != 1 || f.api.Sent[0].ReplyToMessageID != 500 || f.api.Sent[0].InlineKeyboard != nil ||
		f.api.Sent[0].Text != "<b>old-1</b> has ended; pick another session" {
		t.Fatalf("sent = %+v", f.api.Sent)
	}
	if want := []relaytest.Reaction{{MessageID: 500, Emoji: "👀"}, {MessageID: 500, Emoji: ""}}; !reflect.DeepEqual(f.api.Reacted(), want) {
		t.Fatalf("reactions = %+v, want %+v", f.api.Reacted(), want)
	}
	if f.parked(t) != 0 {
		t.Fatal("a refused note was parked")
	}
}

func TestVoiceTranscribeFailure(t *testing.T) {
	for _, c := range []struct {
		name string
		text string
		err  error
		want string
	}{
		{"exit", "", errors.New("exit status 3"), "could not transcribe: exit status 3"},
		{"stderr", "", errors.New("whisper: <model> & weights missing"), "could not transcribe: whisper: &lt;model&gt; &amp; weights missing"},
		{"empty", "", nil, "could not transcribe: nothing recognized"}, // no error, no text
		{"blank", " \t\n ", nil, "could not transcribe: nothing recognized"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newVoiceFixture(t, &transcriber{text: c.text, err: c.err})
			f.handleVoice(t, f.voice(7, 101))
			if len(f.api.Sent) != 1 || f.api.Sent[0].Text != c.want || f.api.Sent[0].ReplyToMessageID != 500 {
				t.Fatalf("sent = %+v", f.api.Sent)
			}
			if want := []relaytest.Reaction{{MessageID: 500, Emoji: "👀"}, {MessageID: 500, Emoji: ""}}; !reflect.DeepEqual(f.api.Reacted(), want) {
				t.Fatalf("reactions = %+v, want %+v", f.api.Reacted(), want)
			}
			if f.parked(t) != 0 {
				t.Fatal("a failed note was parked")
			}
		})
	}
}

// A shutdown signals every process at once (a systemd unit without
// KillMode=mixed, the machine going down): the command dies of the signal
// just before the relay's own stop shows. The note is dropped with the
// rest, not failed on the phone.
func TestVoiceSignalAtShutdownSilent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr := &transcriber{err: signalError{}, then: func() { time.AfterFunc(20*time.Millisecond, cancel) }}
	f := newVoiceFixture(t, tr)
	f.r.Sleep = nil // real time: the stop comes 20 ms after the failure
	if err := f.r.Handle(ctx, f.voice(7, 101)); err != nil {
		t.Fatal(err)
	}
	within(t, 5*time.Second, "WaitVoice", f.r.WaitVoice)
	if texts := f.api.Texts(); len(texts) != 0 {
		t.Fatalf("sent = %q", texts)
	}
	if want := []relaytest.Reaction{{MessageID: 500, Emoji: "👀"}, {MessageID: 500, Emoji: ""}}; !reflect.DeepEqual(f.api.Reacted(), want) {
		t.Fatalf("reactions = %+v, want %+v", f.api.Reacted(), want)
	}
}

// A command killed by a signal while the relay goes on fails on the phone,
// after a short wait for a stop that does not come.
func TestVoiceSignalWithoutShutdownReplies(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{err: signalError{}})
	f.handleVoice(t, f.voice(7, 101))
	if texts := f.api.Texts(); len(texts) != 1 || texts[0] != "could not transcribe: killed by signal 15" {
		t.Fatalf("sent = %q", texts)
	}
	if f.slept != signalGrace {
		t.Fatalf("waited %v for a stop, want %v", f.slept, signalGrace)
	}
	if want := []relaytest.Reaction{{MessageID: 500, Emoji: "👀"}, {MessageID: 500, Emoji: ""}}; !reflect.DeepEqual(f.api.Reacted(), want) {
		t.Fatalf("reactions = %+v, want %+v", f.api.Reacted(), want)
	}
}

// A transcript that is only an address leaves nothing to send.
func TestVoiceAddressOnlyRefused(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "@acme-shop-92"})
	f.handleVoice(t, f.voice(7, 0))
	if len(f.api.Sent) != 1 || f.api.Sent[0].Text != "could not transcribe: nothing recognized" || f.api.Sent[0].InlineKeyboard != nil {
		t.Fatalf("sent = %+v", f.api.Sent)
	}
	if want := []relaytest.Reaction{{MessageID: 500, Emoji: "👀"}, {MessageID: 500, Emoji: ""}}; !reflect.DeepEqual(f.api.Reacted(), want) {
		t.Fatalf("reactions = %+v, want %+v", f.api.Reacted(), want)
	}
	if f.parked(t) != 0 {
		t.Fatal("an address-only transcript was parked")
	}
}

// The file's extension comes from Telegram's file_path and becomes part
// of a file name and of TG_VOICE_FILE: only a plain one is passed on,
// anything else is replaced with .ogg.
func TestVoiceExtensionSanitized(t *testing.T) {
	for path, want := range map[string]string{
		"voice/file_7.oga":                ".oga",
		"voice/file_7.OGG":                ".OGG",
		"music/file_8.mp3":                ".mp3",
		"voice/file_7":                    ".ogg",
		"voice/file_7.":                   ".ogg",
		"voice/file_7.toolongext":         ".ogg",
		"voice/file_7.a b":                ".ogg",
		"voice/file_7.$(touch x)":         ".ogg",
		"voice/file_7.oga;rm":             ".ogg",
		"voice/file_7.\u043e\u0433\u0433": ".ogg",
	} {
		tr := &transcriber{text: "please rerun"}
		f := newVoiceFixture(t, tr)
		f.api.Paths = map[string]string{"voice-1": path}
		f.handleVoice(t, f.voice(7, 101))
		if c := tr.called(); len(c) != 1 || c[0].ext != want {
			t.Errorf("file_path %q: transcribed = %+v, want ext %q", path, c, want)
		}
	}
}

func TestVoiceDownloadFailure(t *testing.T) {
	tr := &transcriber{text: "please rerun"}
	f := newVoiceFixture(t, tr)
	f.api.FileErr = errors.New("telegram 400: Bad Request: file is too big")
	f.handleVoice(t, f.voice(7, 101))
	if len(f.api.Sent) != 1 || f.api.Sent[0].Text != "could not download the voice note: telegram 400: Bad Request: file is too big" {
		t.Fatalf("sent = %+v", f.api.Sent)
	}
	if want := []relaytest.Reaction{{MessageID: 500, Emoji: "👀"}, {MessageID: 500, Emoji: ""}}; !reflect.DeepEqual(f.api.Reacted(), want) {
		t.Fatalf("reactions = %+v, want %+v", f.api.Reacted(), want)
	}
	if len(tr.called()) != 0 || f.parked(t) != 0 {
		t.Fatalf("transcribed = %v parked = %d", tr.called(), f.parked(t))
	}
}

// A transcript is counted as Telegram counts a message, in UTF-16 code
// units (most emoji count two), of the text as typed and as shown
// (redacted credentials grow it), so the confirmation always fits one
// message (ruling R31).
func TestVoiceTranscriptTooLong(t *testing.T) {
	const head = "too long: %d characters, the limit is 4000; send a shorter note\n<i>"
	for _, c := range []struct {
		name, text, want string
	}{
		{"characters", strings.Repeat("я", 4001), fmt.Sprintf(head, 4001) + strings.Repeat("я", 300) + "…</i>"},
		{"emoji", strings.Repeat("😀", 2001), fmt.Sprintf(head, 4002) + strings.Repeat("😀", 300) + "…</i>"},
		// 3900 characters typed, 4500 shown: each key=… becomes key=[redacted].
		{"redacted", strings.Repeat("key=abcdefgh ", 300), fmt.Sprintf(head, 4500)},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newVoiceFixture(t, &transcriber{text: c.text})
			f.handleVoice(t, f.voice(7, 101))
			if len(f.api.Sent) != 1 || !strings.HasPrefix(f.api.Sent[0].Text, c.want) || f.api.Sent[0].InlineKeyboard != nil {
				t.Fatalf("sent = %.200q", f.api.Texts())
			}
			if want := []relaytest.Reaction{{MessageID: 500, Emoji: "👀"}, {MessageID: 500, Emoji: ""}}; !reflect.DeepEqual(f.api.Reacted(), want) {
				t.Fatalf("reactions = %+v, want %+v", f.api.Reacted(), want)
			}
			if f.parked(t) != 0 {
				t.Fatal("a too-long transcript was parked")
			}
		})
	}
	// 2000 emoji are exactly the limit.
	f := newVoiceFixture(t, &transcriber{text: strings.Repeat("😀", 2000)})
	f.handleVoice(t, f.voice(7, 101))
	if len(f.api.Sent) != 1 || f.api.Sent[0].InlineKeyboard == nil {
		t.Fatalf("a transcript at the limit was refused: %.100q", f.api.Texts())
	}
}

// The phone sees the transcript escaped and redacted; the parked text,
// which Send types, is the transcript itself.
func TestVoiceConfirmEscapesTranscript(t *testing.T) {
	const raw = "a<b & token=abcdefgh12345"
	f := newVoiceFixture(t, &transcriber{text: raw})
	f.handleVoice(t, f.voice(7, 0)) // blog-6a is the single followed session
	if len(f.api.Sent) != 1 || f.api.Sent[0].Text != "🎙 to <b>blog-6a</b>:\na&lt;b &amp; token=[redacted]" {
		t.Fatalf("sent = %+v", f.api.Sent)
	}
	k := confirmKey(t, f.api.Sent[0].InlineKeyboard)
	if p, ok := f.store.TakePending(k, f.now, pendingTTL); !ok || p.Text != raw || p.SessionID != sidBlog {
		t.Fatalf("parked = %+v ok = %v", p, ok)
	}
}

// A confirmation that cannot be sent leaves nothing parked and no 👀.
func TestVoiceConfirmSendFailureUnparks(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "please rerun"})
	f.api.SendErr = errors.New("telegram 400: Bad Request: message to be replied not found")
	f.handleVoice(t, f.voice(7, 101))
	if f.parked(t) != 0 {
		t.Fatal("transcript parked behind a confirmation that was never sent")
	}
	if want := []relaytest.Reaction{{MessageID: 500, Emoji: "👀"}, {MessageID: 500, Emoji: ""}}; !reflect.DeepEqual(f.api.Reacted(), want) {
		t.Fatalf("reactions = %+v, want %+v", f.api.Reacted(), want)
	}
}

// Transcription runs beside the update loop: a command (or a tap) is
// handled while a note is still being transcribed.
func TestVoiceDoesNotBlockTaps(t *testing.T) {
	tr := &transcriber{text: "please rerun", block: make(chan struct{}), started: make(chan struct{}, 1)}
	f := newVoiceFixture(t, tr)
	release := sync.OnceFunc(func() { close(tr.block) })
	t.Cleanup(release)
	within(t, 5*time.Second, "Handle(voice note)", func() { f.handle(t, f.voice(7, 101)) })
	select {
	case <-tr.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the transcription never started")
	}
	within(t, 5*time.Second, "Handle(/sessions)", func() { f.handle(t, f.msg("/sessions", 0)) })
	if texts := f.api.Texts(); len(texts) != 1 || !strings.Contains(texts[0], "acme-shop-92") {
		t.Fatalf("/sessions not answered while transcribing: %q", texts)
	}
	release()
	f.r.WaitVoice()
	if got := f.api.LastText(); got != "🎙 to <b>acme-shop-92</b>:\nplease rerun" {
		t.Fatalf("last sent = %q", got)
	}
}

// When the relay stops, the running transcription ends and nothing is
// sent, for it or for the notes queued behind it.
func TestVoiceShutdownSendsNothing(t *testing.T) {
	tr := &transcriber{text: "please rerun", block: make(chan struct{}), started: make(chan struct{}, 4)}
	f := newVoiceFixture(t, tr)
	logs := f.captureLog()
	ctx, cancel := context.WithCancel(context.Background())
	if err := f.r.Handle(ctx, f.voice(7, 101)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tr.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the transcription never started")
	}
	second := f.voice(7, 101)
	second.Message.MessageID = 501
	if err := f.r.Handle(ctx, second); err != nil {
		t.Fatal(err)
	}
	cancel()
	within(t, 5*time.Second, "WaitVoice after shutdown", f.r.WaitVoice)
	if len(f.api.Texts()) != 0 || f.parked(t) != 0 || len(tr.called()) != 1 {
		t.Fatalf("sent = %q parked = %d transcribed = %d", f.api.Texts(), f.parked(t), len(tr.called()))
	}
	// The dropped notes' 👀 are cleared, the running one's and the queued one's.
	want := []relaytest.Reaction{{MessageID: 500, Emoji: "👀"}, {MessageID: 501, Emoji: "👀"}, {MessageID: 500, Emoji: ""}, {MessageID: 501, Emoji: ""}}
	if !reflect.DeepEqual(f.api.Reacted(), want) {
		t.Fatalf("reactions = %+v, want %+v", f.api.Reacted(), want)
	}
	// Each dropped note is logged.
	for _, line := range []string{"voice note 500 dropped: relay stopping", "voice note 501 dropped: relay stopping"} {
		if !slices.Contains(logs(), line) {
			t.Fatalf("log lacks %q: %q", line, logs())
		}
	}
}

// captureLog records the router's log lines; the voice worker logs from
// its own goroutine.
func (f *fixture) captureLog() func() []string {
	var mu sync.Mutex
	var lines []string
	f.r.Log = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

// stoppingAPI stops the relay (stop) as a message goes out, which then
// fails like a request on a cancelled ctx.
type stoppingAPI struct {
	*relaytest.API
	stop context.CancelFunc
}

func (a *stoppingAPI) SendMessage(context.Context, telegram.SendRequest) (int64, error) {
	a.stop()
	return 0, context.Canceled
}

// The relay stopping while a failure or the confirmation goes out leaves
// the note to the worker, which clears its 👀 on a ctx of its own.
func TestVoiceStopWhileReplyingClearsReaction(t *testing.T) {
	for _, c := range []struct {
		name string
		tr   *transcriber
	}{
		{"failure", &transcriber{err: errors.New("exit status 3")}},
		{"confirmation", &transcriber{text: "please rerun"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newVoiceFixture(t, c.tr)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.r.API = &stoppingAPI{API: f.api, stop: cancel}
			if err := f.r.Handle(ctx, f.voice(7, 101)); err != nil {
				t.Fatal(err)
			}
			within(t, 5*time.Second, "WaitVoice", f.r.WaitVoice)
			if want := []relaytest.Reaction{{MessageID: 500, Emoji: "👀"}, {MessageID: 500, Emoji: ""}}; !reflect.DeepEqual(f.api.Reacted(), want) {
				t.Fatalf("reactions = %+v, want %+v", f.api.Reacted(), want)
			}
			if f.parked(t) != 0 {
				t.Fatal("a note dropped at shutdown stayed parked")
			}
		})
	}
}

// The worker stops with its ctx; a note that comes later starts a new one.
func TestVoiceWorkerStopsWithItsContext(t *testing.T) {
	tr := &transcriber{text: "please rerun", block: make(chan struct{}), started: make(chan struct{}, 4)}
	f := newVoiceFixture(t, tr)
	ctx, cancel := context.WithCancel(context.Background())
	if err := f.r.Handle(ctx, f.voice(7, 101)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tr.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the transcription never started")
	}
	cancel()
	within(t, 5*time.Second, "WaitVoice after shutdown", f.r.WaitVoice)
	close(tr.block)
	f.handleVoice(t, f.voice(7, 101))
	if got := f.api.LastText(); got != "🎙 to <b>acme-shop-92</b>:\nplease rerun" {
		t.Fatalf("last sent = %q", got)
	}
}

func TestVoiceQueueFull(t *testing.T) {
	tr := &transcriber{text: "please rerun", block: make(chan struct{}), started: make(chan struct{}, 32)}
	f := newVoiceFixture(t, tr)
	release := sync.OnceFunc(func() { close(tr.block) })
	t.Cleanup(release)
	f.handle(t, f.voice(7, 101))
	select {
	case <-tr.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the transcription never started")
	}
	// Sixteen notes wait behind the one being transcribed; the next is refused.
	for i := int64(1); i <= 16; i++ {
		u := f.voice(7, 101)
		u.Message.MessageID = 500 + i
		f.handle(t, u)
	}
	over := f.voice(7, 101)
	over.Message.MessageID = 600
	f.handle(t, over)
	if texts := f.api.Texts(); len(texts) != 1 || texts[0] != "too many voice notes waiting; send it again in a minute" {
		t.Fatalf("sent = %q", texts)
	}
	var onOver []string
	for _, r := range f.api.Reacted() {
		if r.MessageID == 600 {
			onOver = append(onOver, r.Emoji)
		}
	}
	if !reflect.DeepEqual(onOver, []string{"👀", ""}) {
		t.Fatalf("reactions on the refused note = %q", onOver)
	}
	release()
	f.r.WaitVoice()
	confirmations := 0
	for _, s := range f.api.Sent {
		if strings.HasPrefix(s.Text, "🎙 to ") {
			confirmations++
		}
	}
	if confirmations != 17 || len(tr.called()) != 17 {
		t.Fatalf("confirmations = %d transcribed = %d", confirmations, len(tr.called()))
	}
}

func TestOtherMediaReplies(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "please rerun"})
	u := f.msg("", 0)
	u.Message.Sticker = json.RawMessage(`{"file_id":"sticker-1"}`)
	f.handle(t, u)
	if len(f.api.Sent) != 1 || f.api.Sent[0].Text != "tg reads text and voice notes only" || f.api.Sent[0].ReplyToMessageID != 500 {
		t.Fatalf("sent = %+v", f.api.Sent)
	}
	// A shared location is content tg cannot read either.
	u = f.msg("", 0)
	u.Message.Location = json.RawMessage(`{"latitude":48.85,"longitude":2.35}`)
	f.handle(t, u)
	if len(f.api.Sent) != 2 || f.api.Sent[1].Text != "tg reads text and voice notes only" {
		t.Fatalf("sent = %+v", f.api.Sent)
	}
	// A message with neither text nor media (a service message) is still
	// ignored.
	f.handle(t, f.msg("", 0))
	if len(f.api.Sent) != 2 {
		t.Fatalf("sent = %+v", f.api.Sent)
	}
}

// With Once, the loop returns only after the notes it queued are handled.
func TestRunOnceWaitsForVoice(t *testing.T) {
	tr := &transcriber{text: "please rerun", block: make(chan struct{})}
	f := newVoiceFixture(t, tr)
	u := f.voice(7, 101)
	u.UpdateID = 9
	p := &fakePoller{batches: [][]telegram.Update{{u}}}
	l := &Loop{Poll: p, Router: f.r, Store: f.store, Once: true, MaxAge: 10 * time.Minute, Log: func(string, ...any) {}}
	time.AfterFunc(50*time.Millisecond, func() { close(tr.block) })
	if err := l.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.api.LastText(); got != "🎙 to <b>acme-shop-92</b>:\nplease rerun" {
		t.Fatalf("Run returned before the note was handled: last sent = %q", got)
	}
}

// funcPoller answers poll n (from 1) with what fn returns for it.
type funcPoller struct {
	mu sync.Mutex
	n  int
	fn func(n int) ([]telegram.Update, error)
}

func (p *funcPoller) GetUpdates(context.Context, int64, int) ([]telegram.Update, error) {
	p.mu.Lock()
	p.n++
	n := p.n
	p.mu.Unlock()
	return p.fn(n)
}

// startedWithin waits for the Transcriber's next call; it may run off the
// test's goroutine, so a timeout is an error, not a fatal one.
func startedWithin(t *testing.T, tr *transcriber) {
	t.Helper()
	select {
	case <-tr.started:
	case <-time.After(5 * time.Second):
		t.Error("the transcription never started")
	}
}

// voiceStopper returns a transcriber that blocks until its ctx ends, then
// finishes its cleanup as soon as it said so (finishOnCancel).
func voiceStopper(t *testing.T) *transcriber {
	tr := &transcriber{text: "please rerun", block: make(chan struct{}), started: make(chan struct{}, 1), cancelled: make(chan struct{}, 1), finish: make(chan struct{})}
	tr.finishOnCancel(t)
	return tr
}

// When the relay stops, Run returns only after the running transcription
// saw its ctx end and cleaned up; nothing is sent for it, and its 👀 is
// cleared.
func TestRunStopCancelsVoice(t *testing.T) {
	tr := voiceStopper(t)
	f := newVoiceFixture(t, tr)
	u := f.voice(7, 101)
	u.UpdateID = 9
	l := &Loop{Poll: &fakePoller{batches: [][]telegram.Update{{u}}}, Router: f.r, Store: f.store, MaxAge: 10 * time.Minute, Log: func(string, ...any) {}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		startedWithin(t, tr)
		cancel()
	}()
	var err error
	within(t, 10*time.Second, "Run", func() { err = l.Run(ctx) })
	ended := tr.sawEnd()
	within(t, 5*time.Second, "WaitVoice", f.r.WaitVoice)
	if err != nil {
		t.Fatal(err)
	}
	if !ended {
		t.Fatal("Run returned before the transcription saw its ctx end")
	}
	if texts := f.api.Texts(); len(texts) != 0 {
		t.Fatalf("sent = %q", texts)
	}
	if want := []relaytest.Reaction{{MessageID: 500, Emoji: "👀"}, {MessageID: 500, Emoji: ""}}; !reflect.DeepEqual(f.api.Reacted(), want) {
		t.Fatalf("reactions = %+v, want %+v", f.api.Reacted(), want)
	}
}

// Run returning on an error of its own (the offset cannot be saved) stops
// the transcription too, though its own ctx goes on.
func TestRunErrorCancelsVoice(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits; run as a normal user")
	}
	tr := voiceStopper(t)
	f := newVoiceFixture(t, tr)
	t.Cleanup(func() { _ = os.Chmod(f.store.Dir, 0o700) })
	p := &funcPoller{fn: func(n int) ([]telegram.Update, error) {
		u := f.voice(7, 101)
		u.UpdateID = int64(n)
		if n == 1 {
			return []telegram.Update{u}, nil
		}
		startedWithin(t, tr)
		if err := os.Chmod(f.store.Dir, 0o500); err != nil { // the next offset cannot be saved
			t.Error(err)
		}
		return []telegram.Update{u}, nil
	}}
	l := &Loop{Poll: p, Router: f.r, Store: f.store, MaxAge: 10 * time.Minute, Log: func(string, ...any) {}}
	var err error
	within(t, 10*time.Second, "Run", func() { err = l.Run(context.Background()) })
	ended := tr.sawEnd()
	if !ended {
		t.Fatal("Run returned on its error with the transcription still running")
	}
	within(t, 5*time.Second, "WaitVoice", f.r.WaitVoice)
	if err == nil {
		t.Fatal("Run did not fail")
	}
	if texts := f.api.Texts(); len(texts) != 0 {
		t.Fatalf("sent = %q", texts)
	}
}

// A command deaf to its cancel holds Run back no longer than the bound
// once the relay stops (Ctrl-C with Once), in either mode; what it prints
// afterwards is never sent.
func TestRunStopWaitIsBounded(t *testing.T) {
	for _, once := range []bool{false, true} {
		t.Run(map[bool]string{false: "steady", true: "once"}[once], func(t *testing.T) {
			tr := &transcriber{text: "please rerun", block: make(chan struct{}), deaf: true, started: make(chan struct{}, 1)}
			f := newVoiceFixture(t, tr)
			u := f.voice(7, 101)
			u.UpdateID = 9
			l := &Loop{Poll: &fakePoller{batches: [][]telegram.Update{{u}}}, Router: f.r, Store: f.store, Once: once, MaxAge: 10 * time.Minute, Log: func(string, ...any) {},
				stopWait: 300 * time.Millisecond}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				startedWithin(t, tr)
				cancel()
			}()
			start := time.Now()
			within(t, 10*time.Second, "Run", func() { _ = l.Run(ctx) })
			if took := time.Since(start); took > 3*time.Second {
				t.Fatalf("Run took %v with a 300ms bound", took)
			}
			close(tr.block)
			within(t, 5*time.Second, "WaitVoice", f.r.WaitVoice)
			if texts := f.api.Texts(); len(texts) != 0 {
				t.Fatalf("a transcription completed after Run returned: sent = %q", texts)
			}
		})
	}
}

// With Once, Run waits for a queued note however long it takes (a first
// Whisper run loads its model), well past the bound that applies once the
// relay stops (ruling R23).
func TestRunOnceWaitsForSlowVoice(t *testing.T) {
	tr := &transcriber{text: "please rerun", block: make(chan struct{}), started: make(chan struct{}, 1)}
	f := newVoiceFixture(t, tr)
	u := f.voice(7, 101)
	u.UpdateID = 9
	const bound = 50 * time.Millisecond
	l := &Loop{Poll: &fakePoller{batches: [][]telegram.Update{{u}}}, Router: f.r, Store: f.store, Once: true, MaxAge: 10 * time.Minute, Log: func(string, ...any) {},
		stopWait: bound}
	ran := make(chan error, 1)
	go func() { ran <- l.Run(context.Background()) }()
	startedWithin(t, tr)
	// Nothing can end the note but its own completion: Run must still be
	// waiting long after the bound.
	select {
	case err := <-ran:
		t.Fatalf("Run returned (%v) while the note was still transcribing", err)
	case <-time.After(10 * bound):
	}
	close(tr.block)
	select {
	case err := <-ran:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the note was handled")
	}
	if got := f.api.LastText(); got != "🎙 to <b>acme-shop-92</b>:\nplease rerun" || tr.sawEnd() {
		t.Fatalf("last sent = %q, cancelled = %v", got, tr.sawEnd())
	}
}

// With Once, Ctrl-C (its ctx ending) cancels the queued notes, and Run
// returns after the running transcription cleaned up; nothing is sent for
// it, its 👀 is cleared and the drop is logged.
func TestRunOnceInterruptCancelsVoice(t *testing.T) {
	tr := voiceStopper(t)
	f := newVoiceFixture(t, tr)
	logs := f.captureLog()
	u := f.voice(7, 101)
	u.UpdateID = 9
	l := &Loop{Poll: &fakePoller{batches: [][]telegram.Update{{u}}}, Router: f.r, Store: f.store, Once: true, MaxAge: 10 * time.Minute, Log: func(string, ...any) {}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		startedWithin(t, tr)
		cancel()
	}()
	var err error
	within(t, 10*time.Second, "Run", func() { err = l.Run(ctx) })
	ended := tr.sawEnd()
	within(t, 5*time.Second, "WaitVoice", f.r.WaitVoice)
	if err != nil {
		t.Fatal(err)
	}
	if !ended {
		t.Fatal("Run returned before the transcription saw its ctx end")
	}
	if texts := f.api.Texts(); len(texts) != 0 {
		t.Fatalf("sent = %q", texts)
	}
	if want := []relaytest.Reaction{{MessageID: 500, Emoji: "👀"}, {MessageID: 500, Emoji: ""}}; !reflect.DeepEqual(f.api.Reacted(), want) {
		t.Fatalf("reactions = %+v, want %+v", f.api.Reacted(), want)
	}
	if !slices.Contains(logs(), "voice note 500 dropped: relay stopping") {
		t.Fatalf("log = %q", logs())
	}
}
