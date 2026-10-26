package voice

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newRunner returns a Runner that works in a fresh temp dir.
func newRunner(t *testing.T, command string) (*Runner, string) {
	t.Helper()
	dir := t.TempDir()
	return &Runner{Command: command, Dir: dir}, dir
}

// assertEmpty fails when dir still holds any entry.
func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("%s not empty: %v", dir, entries)
	}
}

func TestTranscribeCollapsesStdout(t *testing.T) {
	r, dir := newRunner(t, `printf '  hello\n\n  world \n'`)
	got, err := r.Transcribe(context.Background(), []byte("x"), ".oga")
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello world" {
		t.Errorf("got %q", got)
	}
	assertEmpty(t, dir)
}

func TestTranscribeEnv(t *testing.T) {
	r, dir := newRunner(t, `test -f "$TG_VOICE_FILE" && test -d "$TG_VOICE_DIR" && case "$TG_VOICE_FILE" in "$TG_VOICE_DIR"/voice.oga) cat "$TG_VOICE_FILE";; esac`)
	got, err := r.Transcribe(context.Background(), []byte("abc"), ".oga")
	if err != nil {
		t.Fatal(err)
	}
	if got != "abc" {
		t.Errorf("got %q", got)
	}
	assertEmpty(t, dir)
}

func TestTranscribeDefaultExtAndMissingDir(t *testing.T) {
	dir := t.TempDir() + "/nested"
	r := &Runner{Command: `case "$TG_VOICE_FILE" in */voice.ogg) echo ok;; esac`, Dir: dir}
	got, err := r.Transcribe(context.Background(), []byte("x"), "")
	if err != nil || got != "ok" {
		t.Fatalf("got %q, %v", got, err)
	}
	assertEmpty(t, dir)
}

func TestTranscribeFailures(t *testing.T) {
	tests := []struct {
		name, command, reason string
	}{
		{"exit status", `exit 3`, "exit status 3"},
		{"stderr last line", `echo boom >&2; echo last line >&2; echo >&2; exit 1`, "last line"},
		{"nothing recognized", `true`, "nothing recognized"},
		{"output too long", `head -c 70000 /dev/zero | tr '\0' a`, "output too long"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, dir := newRunner(t, tt.command)
			_, err := r.Transcribe(context.Background(), []byte("x"), ".oga")
			var ve *Error
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *Error", err)
			}
			if ve.Reason != tt.reason || ve.Error() != tt.reason {
				t.Errorf("reason = %q, want %q", ve.Reason, tt.reason)
			}
			assertEmpty(t, dir)
		})
	}
}

func TestTranscribeStderrReasonCapped(t *testing.T) {
	r, _ := newRunner(t, `head -c 1000 /dev/zero | tr '\0' b >&2; exit 1`)
	_, err := r.Transcribe(context.Background(), []byte("x"), ".oga")
	var ve *Error
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v", err)
	}
	if n := len([]rune(ve.Reason)); n != 300 {
		t.Errorf("reason is %d runes, want 300", n)
	}
}

func TestTranscribeTimeoutKillsGroup(t *testing.T) {
	r, dir := newRunner(t, `sleep 31.4159 & wait`)
	r.Timeout = 300 * time.Millisecond
	start := time.Now()
	_, err := r.Transcribe(context.Background(), []byte("x"), ".oga")
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v", d)
	}
	var ve *Error
	if !errors.As(err, &ve) || ve.Reason != "timed out after 300ms" {
		t.Fatalf("err = %v", err)
	}
	assertEmpty(t, dir)
	assertNoProcess(t, "sleep 31.4159")
}

// assertNoProcess fails when a process whose command line contains marker
// is still running (skipped without pgrep), and kills any it finds.
func assertNoProcess(t *testing.T, marker string) {
	t.Helper()
	if _, err := exec.LookPath("pgrep"); err != nil {
		return
	}
	time.Sleep(100 * time.Millisecond)
	if exec.Command("pgrep", "-f", marker).Run() == nil {
		exec.Command("pkill", "-f", marker).Run()
		t.Errorf("%q survived", marker)
	}
}

func TestTranscribeKeepsTranscriptWhenGrandchildHoldsStdout(t *testing.T) {
	r, _ := newRunner(t, `echo hi; sleep 31.7 &`)
	got, err := r.Transcribe(context.Background(), []byte("x"), ".oga")
	if err != nil || got != "hi" {
		t.Fatalf("got %q, %v", got, err)
	}
	assertNoProcess(t, "sleep 31.7")
}

func TestTranscribeSignalAndToken(t *testing.T) {
	r, _ := newRunner(t, `kill -9 $$`)
	_, err := r.Transcribe(context.Background(), []byte("x"), ".oga")
	var ve *Error
	if !errors.As(err, &ve) || ve.Reason != "killed by signal 9" {
		t.Fatalf("err = %v", err)
	}
	t.Setenv("TELEGRAM_BOT_TOKEN", "123:secret")
	t.Setenv("TG_KEEP", "yes")
	r, _ = newRunner(t, `echo "[${TELEGRAM_BOT_TOKEN-unset}] $TG_KEEP"`)
	got, err := r.Transcribe(context.Background(), []byte("x"), ".oga")
	if err != nil || got != "[unset] yes" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// A command killed by a signal from outside says so (Signaled), whether
// its reason is the signal or its last stderr line; a plain failure does
// not.
func TestTranscribeSignaled(t *testing.T) {
	tests := []struct {
		command, reason string
		signaled        bool
	}{
		{`kill -TERM $$`, "killed by signal 15", true},
		{`echo bye >&2; kill -TERM $$`, "bye", true},
		{`exit 3`, "exit status 3", false},
		{`echo nope >&2; exit 1`, "nope", false},
	}
	for _, tt := range tests {
		r, _ := newRunner(t, tt.command)
		_, err := r.Transcribe(context.Background(), []byte("x"), ".oga")
		var ve *Error
		if !errors.As(err, &ve) || ve.Reason != tt.reason || ve.Signaled() != tt.signaled {
			t.Errorf("%s: err = %v, signaled = %v; want %q, %v", tt.command, err, ve != nil && ve.Signaled(), tt.reason, tt.signaled)
		}
	}
}

// Path, when set, replaces the command's PATH; unset, the command keeps
// the process's own.
func TestTranscribePath(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin:/opt/acme/bin")
	r, _ := newRunner(t, `printf %s "$PATH"`)
	if got, err := r.Transcribe(context.Background(), []byte("x"), ".oga"); err != nil || got != "/usr/bin:/bin:/opt/acme/bin" {
		t.Fatalf("own PATH: got %q, %v", got, err)
	}
	r.Path = "/Users/alex/.local/bin:/usr/bin:/bin"
	if got, err := r.Transcribe(context.Background(), []byte("x"), ".oga"); err != nil || got != r.Path {
		t.Fatalf("Path: got %q, %v", got, err)
	}
}

func TestLastLineUTF8Safe(t *testing.T) {
	if got := lastLine("\xa9tail\nok"); got != "ok" {
		t.Errorf("got %q", got)
	}
	if got := lastLine("\xa9tail"); got != "tail" {
		t.Errorf("got %q", got)
	}
}

func TestTranscribeCancelled(t *testing.T) {
	r, dir := newRunner(t, `sleep 32.7183`)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err := r.Transcribe(ctx, []byte("x"), ".oga")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	assertEmpty(t, dir)
	assertNoProcess(t, "sleep 32.7183")
}

func TestTimeoutReasonMinutes(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{3 * time.Minute, "timed out after 3 min"},
		{time.Minute, "timed out after 1 min"},
		{300 * time.Millisecond, "timed out after 300ms"},
	}
	for _, tt := range tests {
		if got := timeoutReason(tt.d); got != tt.want {
			t.Errorf("timeoutReason(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

// The folders runs left behind (a crash, kill -9, a power loss) are
// removed once older than 10 minutes; a younger one may belong to a run in
// progress (a tg transcribe beside the relay), and anything that is not a
// run's folder is left alone.
func TestRemoveStale(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string, dirToo bool, age time.Duration) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if dirToo {
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(p, "voice.oga"), []byte("audio"), 0o600); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
		return p
	}
	stale := mk("voice-111", true, 11*time.Minute)
	keep := []string{
		mk("voice-222", true, 9*time.Minute),
		mk("other", true, time.Hour),
		mk("voice-file", false, time.Hour),
	}
	n, err := (&Runner{Dir: dir}).RemoveStale()
	if err != nil || n != 1 {
		t.Fatalf("removed %d, %v", n, err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s still there: %v", stale, err)
	}
	for _, p := range keep {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed: %v", p, err)
		}
	}
	// No folder yet: nothing to remove.
	if n, err := (&Runner{Dir: filepath.Join(dir, "missing")}).RemoveStale(); err != nil || n != 0 {
		t.Fatalf("missing dir: removed %d, %v", n, err)
	}
}

// speak runs Speak with a fresh runner and returns the audio, the
// runner's Dir and the error.
func speak(t *testing.T, command, text, lang string) ([]byte, string, error) {
	t.Helper()
	r, dir := newRunner(t, command)
	got, err := r.Speak(context.Background(), text, lang)
	return got, dir, err
}

// reasonOf fails unless err is an *Error and returns its Reason.
func reasonOf(t *testing.T, err error) string {
	t.Helper()
	var ve *Error
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *Error", err)
	}
	return ve.Reason
}

func TestSpeakFileAndOut(t *testing.T) {
	got, _, err := speak(t, `cp "$TG_SPEECH_FILE" "$TG_SPEECH_OUT"`, "привет", "")
	if err != nil || string(got) != "привет" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestSpeakStdin(t *testing.T) {
	got, _, err := speak(t, `cat > "$TG_SPEECH_OUT"`, "hello there", "")
	if err != nil || string(got) != "hello there" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestSpeakLangAndDir(t *testing.T) {
	got, _, err := speak(t, `printf '%s|%s' "$TG_SPEECH_LANG" "$(basename "$TG_SPEECH_DIR")" > "$TG_SPEECH_OUT"`, "x", "uk")
	if err != nil || !strings.HasPrefix(string(got), "uk|speech-") {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestSpeakNoAudio(t *testing.T) {
	_, dir, err := speak(t, `true`, "x", "")
	if r := reasonOf(t, err); r != "speech_command wrote no audio" {
		t.Fatalf("reason = %q", r)
	}
	assertEmpty(t, dir)
	// An empty file is no audio either.
	_, _, err = speak(t, `: > "$TG_SPEECH_OUT"`, "x", "")
	if r := reasonOf(t, err); r != "speech_command wrote no audio" {
		t.Fatalf("empty file: reason = %q", r)
	}
}

func TestSpeakTooBig(t *testing.T) {
	r, _ := newRunner(t, `head -c 2000 /dev/zero > "$TG_SPEECH_OUT"`)
	r.MaxAudio = 1000
	_, err := r.Speak(context.Background(), "x", "")
	if got := reasonOf(t, err); got != "audio too big (over 1000 bytes)" {
		t.Fatalf("reason = %q", got)
	}
	if got := audioTooBig(0); got != "audio too big (over 10 MB)" {
		t.Fatalf("default wording = %q", got)
	}
}

func TestSpeakTimeoutKillsGroup(t *testing.T) {
	r, dir := newRunner(t, `sleep 60.2718 & wait`)
	r.Timeout = 300 * time.Millisecond
	start := time.Now()
	_, err := r.Speak(context.Background(), "x", "")
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v", d)
	}
	if got := reasonOf(t, err); got != "timed out after 300ms" {
		t.Fatalf("reason = %q", got)
	}
	assertEmpty(t, dir)
	assertNoProcess(t, "sleep 60.2718")
}

func TestSpeakTokenStripped(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "x:secret")
	t.Setenv("TG_KEEP", "yes")
	got, _, err := speak(t, `env > "$TG_SPEECH_OUT"`, "x", "")
	if err != nil || strings.Contains(string(got), "TELEGRAM_BOT_TOKEN") || !strings.Contains(string(got), "TG_KEEP=yes") {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestSpeakCleansUp(t *testing.T) {
	_, dir, err := speak(t, `echo a > "$TG_SPEECH_OUT"`, "x", "")
	if err != nil {
		t.Fatal(err)
	}
	assertEmpty(t, dir)
	_, dir, err = speak(t, `exit 1`, "x", "")
	if err == nil {
		t.Fatal("want an error")
	}
	assertEmpty(t, dir)
}

func TestSpeakFailureReason(t *testing.T) {
	_, _, err := speak(t, `echo boom >&2; exit 3`, "x", "")
	if got := reasonOf(t, err); got != "boom" {
		t.Fatalf("reason = %q", got)
	}
	_, _, err = speak(t, `exit 3`, "x", "")
	if got := reasonOf(t, err); got != "exit status 3" {
		t.Fatalf("reason = %q", got)
	}
}

func TestSpeakCancelled(t *testing.T) {
	r, dir := newRunner(t, `sleep 60.1414 & wait`)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	_, err := r.Speak(ctx, "x", "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	assertEmpty(t, dir)
	assertNoProcess(t, "sleep 60.1414")
}

func TestRemoveStaleSpeechDirs(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string, age time.Duration) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
		return p
	}
	stale := mk("speech-x", 11*time.Minute)
	keep := []string{mk("speech-y", time.Minute), mk("other", time.Hour)}
	n, err := (&Runner{Dir: dir}).RemoveStale()
	if err != nil || n != 1 {
		t.Fatalf("removed %d, %v", n, err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s still there: %v", stale, err)
	}
	for _, p := range keep {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed: %v", p, err)
		}
	}
}

// The TypeSafe API key never reaches a child process, like the bot token.
func TestCommandEnvStripsTypesafeKey(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "k-secret")
	r, _ := newRunner(t, `echo "[${TYPESAFE_API_KEY-unset}]"`)
	got, err := r.Transcribe(context.Background(), []byte("x"), ".oga")
	if err != nil || got != "[unset]" {
		t.Fatalf("got %q, %v", got, err)
	}
}
