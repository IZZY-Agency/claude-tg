package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/agterm"
	"github.com/IZZY-Agency/claude-tg/internal/config"
	"github.com/IZZY-Agency/claude-tg/internal/hooks"
	"github.com/IZZY-Agency/claude-tg/internal/idle"
	"github.com/IZZY-Agency/claude-tg/internal/send"
	"github.com/IZZY-Agency/claude-tg/internal/session"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
	"github.com/IZZY-Agency/claude-tg/internal/term"
	"github.com/IZZY-Agency/claude-tg/internal/tmux"
)

func init() {
	register("hook", "internal: called by Claude Code hooks (stop|notification|permission-request|permission-wait|stop-failure|session-end)", cmdHook)
}

const (
	hookTimeout = 15 * time.Second
	// waitTimeout outlives the waiting hook's 12-hour deadline, so tg, not
	// this context, ends the wait and can edit the phone's message.
	waitTimeout = 12*time.Hour + 2*time.Minute
	// The waiting hook reads its own pane: enough lines for a question
	// picker with previews, and never stuck on a hung terminal tool.
	screenLines   = 60
	screenTimeout = 3 * time.Second
)

// waitPermissionFn is the waiting hook; tests replace it.
var waitPermissionFn = hooks.WaitPermission

// hookClient is the hooks' Telegram client; tests point it at a fake server.
var hookClient = telegram.New

// cmdHook always exits 0: a hook failure must never reach the session. Only
// permission-wait writes to stdout, and only a decision from the phone; the
// answer counts as collected only once stdout took it.
func cmdHook(c *cli, args []string) int {
	if len(args) != 1 {
		return exitOK
	}
	kind := args[0]
	wait := kind == "permission-wait"
	timeout := hookTimeout
	if wait {
		timeout = waitTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if wait {
		// Claude Code ending the session or the hook stops the wait cleanly.
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGHUP, os.Interrupt)
		defer stop()
	}
	defer func() {
		if r := recover(); r != nil {
			if store, err := openStore(c); err == nil {
				store.Logf("hook: panic: %v", r)
			}
		}
	}()
	store, err := openStore(c)
	if err != nil {
		return exitOK
	}
	readCtx, readCancel := context.WithTimeout(ctx, hookTimeout)
	in, err := readInput(readCtx, c.stdin)
	readCancel()
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
	env := session.FromEnv(c.env, cwd)
	client := hookClient(cfg.BotToken)
	deps := hooks.Deps{
		Sender: &send.Sender{API: client, Store: store, ChatID: cfg.ChatID, MinGap: minGap, Log: store.Logf, Secrets: []string{cfg.BotToken}},
		Store:  store,
		Resolve: func(ctx context.Context, env session.Env) (*state.Session, error) {
			return session.Resolve(ctx, store, cfg.ClaudePath, env)
		},
		Away:   func(ctx context.Context) bool { return idle.Away(ctx, cfg.AwayAfterSeconds, idle.IdleSeconds) },
		AwayIn: func(ctx context.Context) time.Duration { return awayIn(ctx, cfg.AwayAfterSeconds, idle.IdleSeconds) },
		Log:    store.Logf,
		Env:    env,
		// Redacted between the tags, plain text when Telegram refuses the
		// HTML; the keyboard goes either way.
		Edit: func(ctx context.Context, messageID int64, html string) error {
			return send.EditHTML(ctx, client, cfg.ChatID, messageID, html, nil)
		},
		RelayUp:    func() bool { return store.RelayServes(state.FeatureAsks) },
		Unattended: func() bool { return c.env("CLAUDE_CODE_SESSION_ATTENDED") == "0" },
		Attended: func(ctx context.Context, sessionID string) bool {
			list := func() ([]session.Agent, error) { return session.ListAgents(ctx, cfg.ClaudePath) }
			return attended(c.env("CLAUDE_CODE_SESSION_ATTENDED"), list, sessionID)
		},
		Alive: processAlive,
	}
	if pane := ownPane(env); wait && !pane.IsZero() {
		terminals := term.Mux{Backends: map[string]term.Terminal{agterm.Backend: &agterm.CLI{}, tmux.Backend: &tmux.CLI{}}}
		deps.Screen = func(ctx context.Context) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, screenTimeout)
			defer cancel()
			return terminals.Screen(ctx, pane, screenLines)
		}
	}
	if wait {
		if out, commit := waitPermissionFn(ctx, in, deps); out != nil {
			// A failed write leaves the ask answered and not collected:
			// the relay's sweep tells the phone it was not delivered.
			if err := writeFlushed(c.stdout, append(out, '\n')); err != nil {
				store.Logf("hook %s (%s): stdout: %v; the phone's answer was not delivered", kind, in.SessionID, err)
			} else if commit != nil {
				commit()
			}
		}
		return exitOK
	}
	if err := hooks.Handle(ctx, kind, in, deps); err != nil {
		store.Logf("hook %s (%s): %v", kind, in.SessionID, err)
	}
	return exitOK
}

// writeFlushed writes b to w and flushes w when it buffers.
func writeFlushed(w io.Writer, b []byte) error {
	if _, err := w.Write(b); err != nil {
		return err
	}
	if f, ok := w.(interface{ Flush() error }); ok {
		return f.Flush()
	}
	return nil
}

// awayIn is how long until the owner counts as away by the away gate
// (idle.Away): 0 when away now, when the gate is off (threshold <= 0) and
// when the idle time cannot be read. Idle time grows by a second a second,
// so the gate cannot open sooner.
func awayIn(ctx context.Context, threshold int, probe idle.Probe) time.Duration {
	if threshold <= 0 {
		return 0
	}
	n, err := probe(ctx)
	if err != nil || n >= threshold {
		return 0
	}
	return time.Duration(threshold-n) * time.Second
}

// ownPane is the terminal pane the hook's session runs in, from the hook's
// own environment, chosen as the relay chooses the pane it types into.
func ownPane(env session.Env) term.Pane {
	return (&state.Session{AgtermSessionID: env.AgtermSessionID, TmuxPane: env.TmuxPane, TmuxSocket: env.TmuxSocket}).Pane()
}

// attended reports whether the session has a person at a dialog: not a
// `claude -p` run (CLAUDE_CODE_SESSION_ATTENDED=0) and not a background
// agent. When `claude agents` cannot tell, only the explicit marker "1"
// counts as attended.
func attended(marker string, list func() ([]session.Agent, error), sessionID string) bool {
	if marker == "0" {
		return false
	}
	agents, err := list()
	if err != nil {
		return marker == "1"
	}
	for _, a := range agents {
		if a.SessionID == sessionID && a.Kind == "background" {
			return false
		}
	}
	return true
}

// processAlive reports whether pid runs. 0 (unknown) and negative values
// count as alive: kill(2) with them would address process groups.
func processAlive(pid int) bool {
	if pid <= 0 {
		return true
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
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
