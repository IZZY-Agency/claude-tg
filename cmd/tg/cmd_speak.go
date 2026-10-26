package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/config"
	"github.com/IZZY-Agency/claude-tg/internal/speech"
)

func init() {
	register("speak", "make a voice message with speech_command and send it: tg speak <text> | tg speak -", cmdSpeak)
}

// cmdSpeak runs the configured speech_command on the given text, the way the
// relay's Stop hook does for a short answer to a voice note, and sends the
// audio to the owner's chat as a voice message, so the owner can set the
// command up and hear it. It runs the command with the relay's PATH.
func cmdSpeak(c *cli, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, "usage: tg speak <text> | tg speak -")
		return exitUsage
	}
	path := configPath(c)
	cfg, err := config.Load(path)
	if err != nil {
		if errors.Is(err, config.ErrNotConfigured) {
			fmt.Fprintln(c.stderr, "tg: not configured; run `tg setup`")
			return exitNoConfig
		}
		fmt.Fprintf(c.stderr, "tg speak: %v\n", err)
		return exitUsage
	}
	if cfg.SpeechCommand == "" {
		fmt.Fprintf(c.stderr, "tg speak: speech_command is not set in %s; see the README\n", path)
		return exitUsage
	}
	input := strings.Join(args, " ")
	if input == "-" {
		b, err := io.ReadAll(c.stdin)
		if err != nil {
			fmt.Fprintf(c.stderr, "tg speak: read stdin: %v\n", err)
			return exitUsage
		}
		input = string(b)
	}
	a, code := newApp(c)
	if code != exitOK {
		return code
	}
	// Reuse the hooks' client seam, so tests can point tg speak at a fake server.
	a.sender.API = hookClient(cfg.BotToken)
	// The command gets what the Stop hook would give it: the text a 💬
	// message would show (the bot token scrubbed, credentials redacted),
	// made speakable. No length rule here: the owner may try any text.
	text, _, _ := speech.Prepare(a.sender.Clean(input))
	if text == "" {
		fmt.Fprintln(c.stderr, "tg speak: nothing to say")
		return exitUsage
	}
	// Ctrl-C, SIGTERM or a hangup (the terminal closing) cancels the run,
	// so the command's process group is killed before tg exits.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	d, err := speakAndSend(ctx, c, a, cfg.SpeechCommand, text)
	if err == nil {
		fmt.Fprintf(c.stdout, "sent a %.1f s voice message\n", d.Seconds())
		return exitOK
	}
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(c.stderr, "tg speak: interrupted")
		return exitUsage
	}
	fmt.Fprintf(c.stderr, "tg speak: %v\n", err)
	return exitUsage
}

// speakAndSend makes audio of text with the speech command and sends it as
// a voice message with the default caption, audibly and with no session
// recorded. It returns the audio's length.
func speakAndSend(ctx context.Context, c *cli, a *app, command, text string) (time.Duration, error) {
	lang := speech.Language(text)
	audio, err := speechRunner(c, command).Speak(ctx, speech.Voiced(text, lang), lang)
	if err != nil {
		return 0, err
	}
	d, err := speech.Duration(audio)
	if err != nil {
		return 0, err
	}
	_, err = a.sender.SendVoice(ctx, "", "", audio, d, false)
	return d, err
}
