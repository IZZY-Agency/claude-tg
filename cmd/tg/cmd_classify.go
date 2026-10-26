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

	"github.com/IZZY-Agency/claude-tg/internal/classify"
	"github.com/IZZY-Agency/claude-tg/internal/config"
	"github.com/IZZY-Agency/claude-tg/internal/render"
	"github.com/IZZY-Agency/claude-tg/internal/send"
	"github.com/IZZY-Agency/claude-tg/internal/speech"
)

func init() {
	register("classify", "ask TypeSafe whether an answer would be spoken: tg classify <question> <answer> | tg classify <question> -", cmdClassify)
}

// cmdClassify runs the hard rules (speech.Choose) and, when they pass, the
// TypeSafe request the Stop hook would make, about the whole answer or its
// opening paragraph, and prints the decision with each probability. It
// sends nothing to Telegram.
func cmdClassify(c *cli, args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(c.stderr, "usage: tg classify <question> <answer> | tg classify <question> -")
		return exitUsage
	}
	cfg, err := config.Load(configPath(c))
	if err != nil {
		if errors.Is(err, config.ErrNotConfigured) {
			fmt.Fprintln(c.stderr, "tg: not configured; run `tg setup`")
			return exitNoConfig
		}
		fmt.Fprintf(c.stderr, "tg classify: %v\n", err)
		return exitUsage
	}
	if cfg.TypesafeAPIKey == "" {
		fmt.Fprintln(c.stderr, "tg classify: no TypeSafe key; run tg setup")
		return exitNoConfig
	}
	question := args[0]
	answer := strings.Join(args[1:], " ")
	if answer == "-" {
		b, err := io.ReadAll(c.stdin)
		if err != nil {
			fmt.Fprintf(c.stderr, "tg classify: read stdin: %v\n", err)
			return exitUsage
		}
		answer = string(b)
	}
	// Cleaned as the senders clean a message (send.Sender.Clean).
	question = render.Redact(send.Scrub(question, cfg.Secrets()))
	answer = render.Redact(send.Scrub(answer, cfg.Secrets()))

	choice := speech.Choose(answer)
	switch {
	case choice.Reason != "":
		fmt.Fprintf(c.stdout, "hard rules: text (%s)\n", choice.Reason)
		return exitOK
	case choice.Opening:
		fmt.Fprintln(c.stdout, "hard rules: pass (opening paragraph)")
	default:
		fmt.Fprintln(c.stdout, "hard rules: pass")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	j, err := (&classify.Client{Key: cfg.TypesafeAPIKey, BaseURL: classifyBaseURL}).Judge(ctx, question, choice.Markdown)
	if err != nil {
		fmt.Fprintf(c.stderr, "tg classify: %v\n", err)
		return 1
	}
	decision := "text"
	if classify.Decide(j) {
		decision = "voice"
	}
	fmt.Fprintf(c.stdout, "factual: %.2f\nlistenable: %.2f\nmodel: %s\ndecision: %s\n", j.Factual, j.Listenable, j.Model, decision)
	return exitOK
}
