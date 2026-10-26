package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/agterm"
	"github.com/IZZY-Agency/claude-tg/internal/config"
	"github.com/IZZY-Agency/claude-tg/internal/send"
	"github.com/IZZY-Agency/claude-tg/internal/session"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

const (
	exitOK       = 0
	exitUsage    = 1
	exitNoConfig = 2
	minGap       = 1100 * time.Millisecond
)

// app is the configured runtime shared by the commands that talk to Telegram.
type app struct {
	cfg     *config.Config
	cfgPath string
	store   *state.Store
	client  *telegram.Client
	sender  *send.Sender
	env     session.Env
}

func configPath(c *cli) string {
	if p := c.env("TG_CONFIG"); p != "" {
		return p
	}
	return config.DefaultPath()
}

func stateDir(c *cli) string {
	if p := c.env("TG_STATE_DIR"); p != "" {
		return p
	}
	return config.StateDir()
}

// openStore opens the state directory, independent of the config.
func openStore(c *cli) (*state.Store, error) {
	return state.Open(stateDir(c))
}

// newApp loads the config and wires the sender. It prints to stderr and
// returns a non-zero exit code when the tool is not configured.
func newApp(c *cli) (*app, int) {
	path := configPath(c)
	cfg, err := config.Load(path)
	if err != nil {
		if errors.Is(err, config.ErrNotConfigured) {
			fmt.Fprintln(c.stderr, "tg: not configured; run `tg setup`")
			return nil, exitNoConfig
		}
		fmt.Fprintf(c.stderr, "tg: %v\n", err)
		return nil, exitNoConfig
	}
	store, err := openStore(c)
	if err != nil {
		fmt.Fprintf(c.stderr, "tg: state dir: %v\n", err)
		return nil, exitNoConfig
	}
	cwd, _ := os.Getwd()
	client := telegram.New(cfg.BotToken)
	a := &app{
		cfg: cfg, cfgPath: path, store: store, client: client,
		sender: &send.Sender{API: client, Store: store, ChatID: cfg.ChatID, MinGap: minGap, Log: store.Logf, Secrets: cfg.Secrets()},
		env:    session.FromEnv(c.env, cwd),
	}
	return a, exitOK
}

func (a *app) resolve(ctx context.Context, env session.Env) (*state.Session, error) {
	return session.Resolve(ctx, a.store, a.cfg.ClaudePath, env, session.Options{Tab: agtermTab, Clean: a.sender.Clean})
}

// agtermTab is session.TabFunc through agtermctl.
func agtermTab(ctx context.Context, agtermID string) (string, bool, error) {
	t, ok, err := (&agterm.CLI{}).Tab(ctx, agtermID)
	return t.Name, ok, err
}

// envFor is the env to resolve sid with: the process's own facts for its own
// session, a bare id for any other session so a `--session` call from one
// pane can never rebind another session's terminal.
func (a *app) envFor(sid string) session.Env {
	if sid == a.env.SessionID {
		return a.env
	}
	return session.Env{SessionID: sid}
}

// sessionID picks the session from --session or the environment.
func (a *app) sessionID(flag string) string {
	if flag != "" {
		return flag
	}
	return a.env.SessionID
}
