package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/relay"
)

func transcribeSetup(t *testing.T, voice string) (dir, audio string) {
	t.Helper()
	dir = isolate(t)
	cfg := `{"bot_token":"1:x","chat_id":1`
	if voice != "" {
		cfg += `,"voice_command":"` + voice + `"`
	}
	cfg += `}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	audio = filepath.Join(dir, "note.ogg")
	if err := os.WriteFile(audio, []byte("hi there\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, audio
}

func TestTranscribePrintsTranscript(t *testing.T) {
	_, audio := transcribeSetup(t, `cat \"$TG_VOICE_FILE\"`)
	var out, errb bytes.Buffer
	code := run([]string{"transcribe", audio}, strings.NewReader(""), &out, &errb)
	if code != 0 || out.String() != "hi there\n" {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errb.String())
	}
}

func TestTranscribeUnsetCommand(t *testing.T) {
	_, audio := transcribeSetup(t, "")
	var errb bytes.Buffer
	code := run([]string{"transcribe", audio}, strings.NewReader(""), io.Discard, &errb)
	if code != exitUsage || !strings.Contains(errb.String(), "voice_command is not set") {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
}

func TestTranscribeNeedsAFile(t *testing.T) {
	transcribeSetup(t, "exit 0")
	var errb bytes.Buffer
	code := run([]string{"transcribe"}, strings.NewReader(""), io.Discard, &errb)
	if code != exitUsage || !strings.Contains(errb.String(), "usage: tg transcribe <file>") {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
}

func TestTranscribeFailure(t *testing.T) {
	_, audio := transcribeSetup(t, "exit 4")
	var errb bytes.Buffer
	code := run([]string{"transcribe", audio}, strings.NewReader(""), io.Discard, &errb)
	if code != exitUsage || errb.String() != "tg transcribe: could not transcribe: exit status 4\n" {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
}

// Ctrl-C or SIGTERM during tg transcribe stops the command: tg kills its
// process group and removes the copy of the audio before it exits. The
// signal goes to this test process, which tg transcribe listens on while
// the command runs.
//
// Because the signal hits the whole test binary, this test and its
// subtests must stay sequential, and no cmd/tg test may use t.Parallel
// while it exists: a test running beside it would get the signal too,
// which kills the binary unless something listens for it.
func TestTranscribeInterrupted(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			dir, audio := transcribeSetup(t, "sleep 33.1416")
			voiceDir := filepath.Join(dir, "state", "voice")
			var errb bytes.Buffer
			done := make(chan int, 1)
			go func() { done <- run([]string{"transcribe", audio}, strings.NewReader(""), io.Discard, &errb) }()
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
				if code != exitUsage || errb.String() != "tg transcribe: interrupted\n" {
					t.Fatalf("code=%d err=%q", code, errb.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("tg transcribe did not stop on the signal")
			}
			if entries, err := os.ReadDir(voiceDir); err != nil || len(entries) != 0 {
				t.Fatalf("voice folder = %v, %v", entries, err)
			}
			if _, err := exec.LookPath("pgrep"); err == nil && exec.Command("pgrep", "-f", "sleep 33.1416").Run() == nil {
				_ = exec.Command("pkill", "-f", "sleep 33.1416").Run()
				t.Fatal("the command survived tg transcribe")
			}
		})
	}
}

// tg transcribe runs the command with the relay's PATH, not the shell's,
// so a command that works here works in the relay: a tool in ~/.local/bin
// is found though the shell's PATH lacks it.
func TestTranscribeUsesRelayPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin")
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "acme-asr"), []byte("#!/bin/sh\necho from the relay path\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for command, want := range map[string]string{
		`acme-asr`:            "from the relay path",
		`printf %s \"$PATH\"`: relay.AgentPath(home),
	} {
		_, audio := transcribeSetup(t, command)
		var out, errb bytes.Buffer
		if code := run([]string{"transcribe", audio}, strings.NewReader(""), &out, &errb); code != exitOK || out.String() != want+"\n" {
			t.Fatalf("%s: code=%d out=%q err=%q", command, code, out.String(), errb.String())
		}
	}
}

func TestTranscribeNotConfigured(t *testing.T) {
	isolate(t)
	var errb bytes.Buffer
	code := run([]string{"transcribe", "note.ogg"}, strings.NewReader(""), io.Discard, &errb)
	if code != exitNoConfig || !strings.Contains(errb.String(), "tg setup") {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
}

// The relay and tg transcribe run the command in the same place: under
// the state dir, in "voice".
func TestVoiceRunnerUnderStateDir(t *testing.T) {
	dir := isolate(t)
	r := voiceRunner(&cli{env: os.Getenv}, "cat \"$TG_VOICE_FILE\"")
	if r.Command != `cat "$TG_VOICE_FILE"` || r.Dir != filepath.Join(dir, "state", "voice") {
		t.Fatalf("runner = %+v", r)
	}
}
