// Package voice turns a downloaded voice note into text by running the
// owner's configured voice_command, and text into a voice message by running
// their speech_command, shell strings from their own config.
package voice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultTimeout   = 3 * time.Minute
	defaultMaxOutput = 64 << 10
	// defaultMaxAudio is the audio cap when Runner.MaxAudio is 0.
	defaultMaxAudio int64 = 10 << 20
	// stderrKeep is how much of stderr's tail is retained to find its last line.
	stderrKeep = 8 << 10
	// reasonRunes caps a stderr line used as a failure reason.
	reasonRunes = 300
	// waitDelay bounds how long Wait lingers on pipes a surviving
	// grandchild holds open once the process group is killed.
	waitDelay = time.Second
	// staleAge is when RemoveStale takes a run's folder: well past
	// defaultTimeout, so a run in progress is never taken.
	staleAge = 10 * time.Minute
)

// Runner runs Command on a voice note.
type Runner struct {
	Command   string        // shell string, run as /bin/sh -c
	Dir       string        // parent of the per-run dirs, created 0700 if missing
	Path      string        // the command's PATH; "" keeps the process's own
	Timeout   time.Duration // 0 means 3 minutes
	MaxOutput int           // stdout cap in bytes; 0 means 64 KiB
	MaxAudio  int64         // Speak's audio cap in bytes; 0 means 10 MB
}

// Error is a transcription failure whose Reason is safe to show the owner.
type Error struct {
	Reason   string
	signaled bool
}

func (e *Error) Error() string { return e.Reason }

// Signaled reports whether the command was killed by a signal it did not
// get from the Runner (whose own kills on timeout, overflow or ctx end
// have their own reasons): a shutdown that signals every process, say.
func (e *Error) Signaled() bool { return e.signaled }

// Transcribe writes audio to voice<ext> (default .ogg) in a fresh 0700
// directory, runs the command with TG_VOICE_FILE and TG_VOICE_DIR added to
// the environment, and returns its stdout with whitespace runs collapsed.
// The directory is removed whatever happens. On timeout, output overflow or
// context end the whole process group is killed with SIGKILL. A cancelled
// ctx returns ctx.Err(); every other failure is an *Error. TELEGRAM_BOT_TOKEN
// and TYPESAFE_API_KEY are removed from the command's environment, and PATH
// set to Path when that is set.
func (r *Runner) Transcribe(ctx context.Context, audio []byte, ext string) (string, error) {
	if ext == "" {
		ext = ".ogg"
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	limit := r.MaxOutput
	if limit <= 0 {
		limit = defaultMaxOutput
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return "", prepareError(err)
	}
	dir, err := os.MkdirTemp(r.Dir, "voice-")
	if err != nil {
		return "", prepareError(err)
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "voice"+ext)
	if err := os.WriteFile(file, audio, 0o600); err != nil {
		return "", prepareError(err)
	}

	stdout, err := r.run(ctx, runSpec{
		env:     []string{"TG_VOICE_FILE=" + file, "TG_VOICE_DIR=" + dir},
		timeout: timeout,
		limit:   limit,
	})
	if err != nil {
		return "", err
	}
	text := strings.Join(strings.Fields(stdout), " ")
	if text == "" {
		return "", &Error{Reason: "nothing recognized"}
	}
	return text, nil
}

// runSpec is what one run of the command needs beyond the Runner itself.
type runSpec struct {
	env     []string      // added to the command's environment
	stdin   io.Reader     // nil means /dev/null
	timeout time.Duration // already defaulted
	limit   int           // stdout cap in bytes; 0 discards stdout
}

// run executes the command as /bin/sh -c in its own process group with the
// token-free environment (PATH set to Path when that is set) plus spec.env.
// On timeout, stdout overflow or context end the whole group is killed with
// SIGKILL. It returns the command's stdout; a cancelled ctx returns
// ctx.Err() and every other failure is an *Error, its reason the last line
// of stderr when there is one.
func (r *Runner) run(ctx context.Context, spec runSpec) (string, error) {
	cmd := exec.Command("/bin/sh", "-c", r.Command)
	cmd.Env = append(commandEnv(r.Path), spec.env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = waitDelay
	cmd.Stdin = spec.stdin
	// over stays nil when stdout is discarded: a nil channel never fires.
	out := &limitedBuffer{limit: spec.limit}
	var over <-chan struct{}
	if spec.limit > 0 {
		out.over = make(chan struct{})
		over = out.over
		cmd.Stdout = out
	}
	var errTail tailBuffer
	cmd.Stderr = &errTail
	if err := cmd.Start(); err != nil {
		return "", &Error{Reason: err.Error()}
	}
	pgid := cmd.Process.Pid
	killGroup := func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }
	// On a normal exit this reaps strays the shell left in the group. The
	// leader is already reaped by then, so pgid reuse is possible in theory;
	// the window is tiny and accepted. The other paths kill once, explicitly.
	killed := false
	defer func() {
		if !killed {
			killGroup()
		}
	}()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(spec.timeout)
	defer timer.Stop()

	// If ctx ends at the same moment the command exits, either case may win;
	// both are fine, as the caller discards the result of a cancelled run.
	var waitErr error
	select {
	case waitErr = <-done:
	case <-over:
		killed = true
		killGroup()
		<-done
		return "", &Error{Reason: "output too long"}
	case <-timer.C:
		killed = true
		killGroup()
		<-done
		return "", &Error{Reason: timeoutReason(spec.timeout)}
	case <-ctx.Done():
		killed = true
		killGroup()
		<-done
		return "", ctx.Err()
	}

	if out.exceeded() {
		return "", &Error{Reason: "output too long"}
	}
	// A grandchild still holding stdout makes Wait report ErrWaitDelay even
	// after a clean exit; stdout is complete by then, so it is not a failure.
	if errors.Is(waitErr, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0 {
		waitErr = nil
	}
	if waitErr != nil {
		e := &Error{Reason: waitErr.Error()}
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			e.Reason = fmt.Sprintf("exit status %d", ee.ExitCode())
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				e.Reason, e.signaled = fmt.Sprintf("killed by signal %d", int(ws.Signal())), true
			}
		}
		if reason := lastLine(errTail.String()); reason != "" {
			e.Reason = reason
		}
		return "", e
	}
	return out.String(), nil
}

// Speak turns text into an OGG/Opus file: in a fresh 0700 "speech-*"
// directory it writes speech.txt, runs the command with the text on stdin
// and TG_SPEECH_FILE (that file), TG_SPEECH_OUT (<dir>/speech.ogg, where the
// command must write the audio), TG_SPEECH_DIR and TG_SPEECH_LANG (lang, or
// empty) added to the environment, discards its stdout and returns the bytes
// of speech.ogg. Process handling, the environment and the failure types are
// Transcribe's; a missing or empty output file is "speech_command wrote no
// audio" and one over MaxAudio (default 10 MB) is "audio too big". The
// directory is removed whatever happens.
func (r *Runner) Speak(ctx context.Context, text, lang string) ([]byte, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return nil, prepareError(err)
	}
	dir, err := os.MkdirTemp(r.Dir, "speech-")
	if err != nil {
		return nil, prepareError(err)
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "speech.txt")
	outFile := filepath.Join(dir, "speech.ogg")
	if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
		return nil, prepareError(err)
	}
	if _, err := r.run(ctx, runSpec{
		env: []string{
			"TG_SPEECH_FILE=" + file,
			"TG_SPEECH_OUT=" + outFile,
			"TG_SPEECH_DIR=" + dir,
			"TG_SPEECH_LANG=" + lang,
		},
		stdin:   strings.NewReader(text),
		timeout: timeout,
	}); err != nil {
		return nil, err
	}
	max := r.MaxAudio
	if max <= 0 {
		max = defaultMaxAudio
	}
	info, err := os.Stat(outFile)
	if err != nil || info.Size() == 0 {
		return nil, &Error{Reason: "speech_command wrote no audio"}
	}
	if info.Size() > max {
		return nil, &Error{Reason: audioTooBig(r.MaxAudio)}
	}
	audio, err := os.ReadFile(outFile)
	if err != nil {
		return nil, &Error{Reason: "speech_command wrote no audio"}
	}
	return audio, nil
}

// audioTooBig words the over-the-cap failure: "10 MB" for the default cap
// (max <= 0), the byte count for an explicit one (tests).
func audioTooBig(max int64) string {
	if max <= 0 {
		return "audio too big (over 10 MB)"
	}
	return fmt.Sprintf("audio too big (over %d bytes)", max)
}

// RemoveStale deletes the per-run folders under Dir ("voice-*" and
// "speech-*") last changed more than 10 minutes ago, audio and all: a run
// killed before it could clean up (a crash, kill -9, a power loss) leaves
// its folder. A younger folder may belong to a run in progress, here or in
// another process. It returns how many it removed; a missing Dir is nothing
// to remove.
func (r *Runner) RemoveStale() (int, error) {
	entries, err := os.ReadDir(r.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	var first error
	for _, e := range entries {
		if !e.IsDir() || !(strings.HasPrefix(e.Name(), "voice-") || strings.HasPrefix(e.Name(), "speech-")) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) <= staleAge {
			continue
		}
		if err := os.RemoveAll(filepath.Join(r.Dir, e.Name())); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		n++
	}
	return n, first
}

// timeoutReason words a timeout: whole minutes as "3 min", anything shorter
// than a minute (tests) as the duration itself.
func timeoutReason(d time.Duration) string {
	if d >= time.Minute {
		return fmt.Sprintf("timed out after %d min", int((d+30*time.Second)/time.Minute))
	}
	return "timed out after " + d.String()
}

// prepareError words a failure to set up the run directory or note file.
func prepareError(err error) error {
	return &Error{Reason: "cannot prepare the note: " + err.Error()}
}

// commandEnv is the process environment minus the bot token and the
// TypeSafe API key, its PATH replaced with path unless that is empty.
func commandEnv(path string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "TELEGRAM_BOT_TOKEN=") || strings.HasPrefix(kv, "TYPESAFE_API_KEY=") || path != "" && strings.HasPrefix(kv, "PATH=") {
			continue
		}
		env = append(env, kv)
	}
	if path != "" {
		env = append(env, "PATH="+path)
	}
	return env
}

// lastLine returns the last non-empty line of s, capped at reasonRunes runes.
func lastLine(s string) string {
	// The byte-truncated tail may begin inside a rune; drop the broken bytes.
	lines := strings.Split(strings.ToValidUTF8(s, ""), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			if r := []rune(line); len(r) > reasonRunes {
				return string(r[:reasonRunes])
			}
			return line
		}
	}
	return ""
}

// limitedBuffer collects stdout up to limit bytes; the first write past it
// closes over so the run can be killed, and further bytes are dropped.
type limitedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
	once  sync.Once
	over  chan struct{}
	hit   bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.hit || b.buf.Len()+len(p) > b.limit {
		b.hit = true
		b.once.Do(func() { close(b.over) })
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) exceeded() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hit
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// tailBuffer keeps the last stderrKeep bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > stderrKeep {
		b.buf = b.buf[len(b.buf)-stderrKeep:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
