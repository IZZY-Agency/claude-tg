package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/config"
	"github.com/IZZY-Agency/claude-tg/internal/hooks"
	"github.com/IZZY-Agency/claude-tg/internal/idle"
	"github.com/IZZY-Agency/claude-tg/internal/send"
	"github.com/IZZY-Agency/claude-tg/internal/session"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

func init() {
	register("hook", "internal: called by Claude Code hooks (stop|notification|permission-request|stop-failure|session-end)", cmdHook)
}

// cmdHook never writes to stdout and always exits 0: a hook failure must
// never reach the session.
func cmdHook(c *cli, args []string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			if store, err := openStore(c); err == nil {
				store.Logf("hook: panic: %v", r)
			}
		}
	}()
	if len(args) != 1 {
		return exitOK
	}
	kind := args[0]
	store, err := openStore(c)
	if err != nil {
		return exitOK
	}
	in, err := readInput(ctx, c.stdin)
	if err != nil {
		store.Logf("hook %s: %v", kind, err)
		return exitOK
	}
	cfg, err := config.Load(configPath(c))
	if err != nil {
		if !errors.Is(err, config.ErrNotConfigured) {
			store.Logf("hook %s: %v", kind, err)
		}
		return exitOK
	}
	cwd, _ := os.Getwd()
	client := telegram.New(cfg.BotToken)
	deps := hooks.Deps{
		Sender: &send.Sender{API: client, Store: store, ChatID: cfg.ChatID, MinGap: minGap, Log: store.Logf, Secrets: []string{cfg.BotToken}},
		Store:  store,
		Resolve: func(ctx context.Context, env session.Env) (*state.Session, error) {
			return session.Resolve(ctx, store, cfg.ClaudePath, env)
		},
		Away: func(ctx context.Context) bool { return idle.Away(ctx, cfg.AwayAfterSeconds, idle.IdleSeconds) },
		Log:  store.Logf,
		Env:  session.FromEnv(c.env, cwd),
	}
	if err := hooks.Handle(ctx, kind, in, deps); err != nil {
		store.Logf("hook %s (%s): %v", kind, in.SessionID, err)
	}
	return exitOK
}

// readInput parses hook stdin but gives up when the context ends, so a
// pipe that is never closed cannot hang the hook process.
func readInput(ctx context.Context, r io.Reader) (*hooks.Input, error) {
	type result struct {
		in  *hooks.Input
		err error
	}
	ch := make(chan result, 1)
	go func() {
		in, err := hooks.ParseInput(r)
		ch <- result{in, err}
	}()
	select {
	case res := <-ch:
		return res.in, res.err
	case <-ctx.Done():
		return nil, fmt.Errorf("reading stdin: %w", ctx.Err())
	}
}
