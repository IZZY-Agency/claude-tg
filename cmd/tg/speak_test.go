package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/relay"
	"github.com/IZZY-Agency/claude-tg/internal/speech"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

// speakFixture is the OGG/Opus file the test speech commands "synthesize",
// kept as base64 text (the public snapshot holds text files only).
const speakFixture = "../../internal/speech/testdata/ru.ogg.b64"

// fixtureAudio is speakFixture decoded.
func fixtureAudio(t *testing.T) []byte {
	t.Helper()
	b64, err := os.ReadFile(speakFixture)
	if err != nil {
		t.Fatal(err)
	}
	b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(b64)), ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// sendVoiceCall is one multipart sendVoice request the fake server saw.
type sendVoiceCall struct {
	method string
	fields map[string]string
	audio  int
}

// speakSetup isolates config and state, writes a config with the given
// speech_command (none when empty) and points the Telegram client at a fake
// server. The returned func lists the requests it received.
func speakSetup(t *testing.T, command string) func() []sendVoiceCall {
	t.Helper()
	return speakSetupToken(t, "1:x", command)
}

// speakSetupToken is speakSetup with the given bot token in the config.
func speakSetupToken(t *testing.T, token, command string) func() []sendVoiceCall {
	t.Helper()
	dir := isolate(t)
	cfg := map[string]any{"bot_token": token, "chat_id": 1}
	if command != "" {
		cfg["speech_command"] = command
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(t.TempDir(), "ru.ogg")
	if err := os.WriteFile(abs, fixtureAudio(t), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TG_TEST_FIXTURE", abs)
	var mu sync.Mutex
	var calls []sendVoiceCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := sendVoiceCall{method: filepath.Base(r.URL.Path), fields: map[string]string{}}
		if err := r.ParseMultipartForm(1 << 20); err == nil {
			for k, v := range r.MultipartForm.Value {
				call.fields[k] = v[0]
			}
			if fh := r.MultipartForm.File["voice"]; len(fh) == 1 {
				call.audio = int(fh[0].Size)
			}
		}
		mu.Lock()
		calls = append(calls, call)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":42}}`))
	}))
	t.Cleanup(srv.Close)
	orig := hookClient
	t.Cleanup(func() { hookClient = orig })
	hookClient = func(token string) *telegram.Client {
		return &telegram.Client{Token: token, HTTP: srv.Client(), BaseURL: srv.URL}
	}
	return func() []sendVoiceCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]sendVoiceCall(nil), calls...)
	}
}

const copyFixture = `cp "$TG_TEST_FIXTURE" "$TG_SPEECH_OUT"`

func wantSpoken(t *testing.T) string {
	t.Helper()
	b := fixtureAudio(t)
	d, err := speech.Duration(b)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("sent a %.1f s voice message\n", d.Seconds())
}

func checkOneVoice(t *testing.T, calls []sendVoiceCall) {
	t.Helper()
	if len(calls) != 1 || calls[0].method != "sendVoice" {
		t.Fatalf("calls=%+v", calls)
	}
	c := calls[0]
	if c.fields["caption"] != "🔊 mac" || c.audio == 0 {
		t.Fatalf("call=%+v", c)
	}
	if _, ok := c.fields["disable_notification"]; ok {
		t.Fatalf("voice must be audible: %+v", c)
	}
}

func TestSpeakSendsVoice(t *testing.T) {
	calls := speakSetup(t, copyFixture)
	var out, errb bytes.Buffer
	code := run([]string{"speak", "Привет"}, strings.NewReader(""), &out, &errb)
	if code != exitOK || out.String() != wantSpoken(t) {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errb.String())
	}
	checkOneVoice(t, calls())
}

func TestSpeakStdin(t *testing.T) {
	calls := speakSetup(t, copyFixture)
	var out, errb bytes.Buffer
	code := run([]string{"speak", "-"}, strings.NewReader("Привет, как дела?\n"), &out, &errb)
	if code != exitOK || out.String() != wantSpoken(t) {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errb.String())
	}
	checkOneVoice(t, calls())
}

func TestSpeakUsage(t *testing.T) {
	calls := speakSetup(t, copyFixture)
	var errb bytes.Buffer
	code := run([]string{"speak"}, strings.NewReader(""), io.Discard, &errb)
	if code != exitUsage || errb.String() != "usage: tg speak <text> | tg speak -\n" || len(calls()) != 0 {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
}

func TestSpeakNotConfigured(t *testing.T) {
	isolate(t)
	var errb bytes.Buffer
	code := run([]string{"speak", "hi"}, strings.NewReader(""), io.Discard, &errb)
	if code != exitNoConfig || errb.String() != "tg: not configured; run `tg setup`\n" {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
}

func TestSpeakNoCommand(t *testing.T) {
	calls := speakSetup(t, "")
	var errb bytes.Buffer
	code := run([]string{"speak", "hi"}, strings.NewReader(""), io.Discard, &errb)
	want := "tg speak: speech_command is not set in " + os.Getenv("TG_CONFIG") + "; see the README\n"
	if code != exitUsage || errb.String() != want || len(calls()) != 0 {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
}

func TestSpeakNothingToSay(t *testing.T) {
	calls := speakSetup(t, copyFixture)
	var errb bytes.Buffer
	code := run([]string{"speak", "```go\nfmt.Println(1)\n```"}, strings.NewReader(""), io.Discard, &errb)
	if code != exitUsage || errb.String() != "tg speak: nothing to say\n" || len(calls()) != 0 {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
}

func TestSpeakCommandFails(t *testing.T) {
	calls := speakSetup(t, "exit 3")
	var out, errb bytes.Buffer
	code := run([]string{"speak", "Привет"}, strings.NewReader(""), &out, &errb)
	if code != exitUsage || errb.String() != "tg speak: exit status 3\n" || out.Len() != 0 || len(calls()) != 0 {
		t.Fatalf("code=%d out=%q err=%q calls=%+v", code, out.String(), errb.String(), calls())
	}
}

// The speech command gets the text a 💬 message would show, as in the
// Stop hook: the bot token scrubbed and credentials redacted, on standard
// input and in $TG_SPEECH_FILE alike.
func TestSpeakScrubsSecrets(t *testing.T) {
	const token = "123456:ABCDEFsecretbot"
	out := t.TempDir()
	t.Setenv("TG_TEST_OUT", out)
	calls := speakSetupToken(t, token, `cat > "$TG_TEST_OUT/stdin.txt"; cp "$TG_SPEECH_FILE" "$TG_TEST_OUT/file.txt"; `+copyFixture)
	var errb bytes.Buffer
	code := run([]string{"speak", "token " + token + " and password=s3cr3tvalue, done."}, strings.NewReader(""), io.Discard, &errb)
	if code != exitOK {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
	checkOneVoice(t, calls())
	for _, name := range []string{"stdin.txt", "file.txt"} {
		b, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := string(b); strings.Contains(got, "ABCDEFsecretbot") || strings.Contains(got, "s3cr3tvalue") || !strings.Contains(got, "[redacted]") {
			t.Fatalf("%s = %q, want the secrets redacted", name, got)
		}
	}
}

// The Stop hook and tg speak run speech_command alike: under the state
// dir's "voice", with the relay's PATH and the 30 second limit.
func TestSpeechRunner(t *testing.T) {
	dir := isolate(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	r := speechRunner(&cli{env: os.Getenv}, copyFixture)
	if r.Command != copyFixture || r.Dir != filepath.Join(dir, "state", "voice") || r.Path != relay.AgentPath(home) || r.Timeout != 30*time.Second {
		t.Fatalf("runner = %+v", r)
	}
}

// errReader fails every read.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestSpeakStdinReadError(t *testing.T) {
	calls := speakSetup(t, copyFixture)
	var errb bytes.Buffer
	code := run([]string{"speak", "-"}, errReader{}, io.Discard, &errb)
	if code != exitUsage || errb.String() != "tg speak: read stdin: broken pipe\n" || len(calls()) != 0 {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
}

// Ctrl-C, SIGTERM or a hangup (the terminal closing) during tg speak
// stops the command, removes its folder and sends nothing. The signal goes
// to this test process, which tg speak listens on while the command runs,
// so this test must stay sequential (see TestTranscribeInterrupted).
func TestSpeakInterrupted(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			calls := speakSetup(t, "sleep 33.2718")
			var errb bytes.Buffer
			done := make(chan int, 1)
			go func() { done <- run([]string{"speak", "Привет"}, strings.NewReader(""), io.Discard, &errb) }()
			voiceDir := filepath.Join(os.Getenv("TG_STATE_DIR"), "voice")
			// The run's folder exists once the command is about to start.
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
				if entries, _ := os.ReadDir(voiceDir); len(entries) > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the command never started")
				}
			}
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			select {
			case code := <-done:
				if code != exitUsage || errb.String() != "tg speak: interrupted\n" || len(calls()) != 0 {
					t.Fatalf("code=%d err=%q", code, errb.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("tg speak did not stop on the signal")
			}
			if left, err := filepath.Glob(filepath.Join(voiceDir, "speech-*")); err != nil || len(left) != 0 {
				t.Fatalf("speech folders left = %v, %v", left, err)
			}
			// The killed command may take a moment to be reaped; one that
			// survived would still be there for 33 seconds.
			if _, err := exec.LookPath("pgrep"); err == nil {
				for deadline := time.Now().Add(3 * time.Second); exec.Command("pgrep", "-f", "sleep 33.2718").Run() == nil; time.Sleep(20 * time.Millisecond) {
					if time.Now().After(deadline) {
						_ = exec.Command("pkill", "-f", "sleep 33.2718").Run()
						t.Fatal("the command survived tg speak")
					}
				}
			}
		})
	}
}
