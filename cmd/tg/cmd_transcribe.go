package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/config"
	"github.com/IZZY-Agency/claude-tg/internal/relay"
	"github.com/IZZY-Agency/claude-tg/internal/voice"
)

func init() {
	register("transcribe", "run voice_command on an audio file and print the text: tg transcribe <file>", cmdTranscribe)
}

// cmdTranscribe runs the configured voice_command on a local audio file, the
// way the relay does for a voice note, so the owner can try it from a shell:
// with the PATH the relay's service gets (relay.AgentPath), not the
// shell's, so a command that works here works in the relay. It needs only
// the config, not the bot token or the network.
func cmdTranscribe(c *cli, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(c.stderr, "usage: tg transcribe <file>")
		return exitUsage
	}
	path := configPath(c)
	cfg, err := config.Load(path)
	if err != nil {
		if errors.Is(err, config.ErrNotConfigured) {
			fmt.Fprintln(c.stderr, "tg: not configured; run `tg setup`")
			return exitNoConfig
		}
		fmt.Fprintf(c.stderr, "tg transcribe: %v\n", err)
		return exitUsage
	}
	if cfg.VoiceCommand == "" {
		fmt.Fprintf(c.stderr, "tg transcribe: voice_command is not set in %s; see the README\n", path)
		return exitUsage
	}
	audio, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintf(c.stderr, "tg transcribe: %v\n", err)
		return exitUsage
	}
	// Ctrl-C or SIGTERM cancels the run, so the command's process group is
	// killed and the copy of the audio removed before tg exits.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r := voiceRunner(c, cfg.VoiceCommand)
	home, _ := os.UserHomeDir()
	r.Path = relay.AgentPath(home)
	text, err := r.Transcribe(ctx, audio, filepath.Ext(args[0]))
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(c.stderr, "tg transcribe: interrupted")
		return exitUsage
	}
	if err != nil {
		fmt.Fprintf(c.stderr, "tg transcribe: could not transcribe: %v\n", err)
		return exitUsage
	}
	fmt.Fprintln(c.stdout, text)
	return exitOK
}

// voiceRunner runs the owner's voice_command as the relay and tg
// transcribe both do: each run in a fresh directory under the state dir's
// "voice".
func voiceRunner(c *cli, command string) *voice.Runner {
	return &voice.Runner{Command: command, Dir: filepath.Join(stateDir(c), "voice")}
}

// speechRunner runs the owner's speech_command the way the relay's Stop hook
// and tg speak do: each run in a fresh directory under the state dir's
// "voice", with the PATH the relay's service gets and a 30 second limit.
func speechRunner(c *cli, command string) *voice.Runner {
	home, _ := os.UserHomeDir()
	return &voice.Runner{
		Command: command,
		Dir:     filepath.Join(stateDir(c), "voice"),
		Path:    relay.AgentPath(home),
		Timeout: 30 * time.Second,
	}
}
