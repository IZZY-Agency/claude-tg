package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/agterm"
	"github.com/IZZY-Agency/claude-tg/internal/relay"
	"github.com/IZZY-Agency/claude-tg/internal/session"
	"github.com/IZZY-Agency/claude-tg/internal/term"
	"github.com/IZZY-Agency/claude-tg/internal/tmux"
)

func init() {
	register("relay", "deliver Telegram replies to sessions: tg relay [--once] [--verbose] | tg relay install|uninstall|status", cmdRelay)
}

func cmdRelay(c *cli, args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "install":
			return relayInstall(c, args[1:])
		case "uninstall":
			return relayUninstall(c, args[1:])
		case "status":
			return relayStatus(c)
		}
		if !strings.HasPrefix(args[0], "-") {
			fmt.Fprintf(c.stderr, "tg relay: unknown subcommand %q\n", args[0])
			return exitUsage
		}
	}
	fs := flag.NewFlagSet("relay", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	once := fs.Bool("once", false, "handle pending updates and exit")
	verbose := fs.Bool("verbose", false, "also log to stderr")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	a, code := newApp(c)
	if code != exitOK {
		return code
	}
	logf := a.store.Logf
	if *verbose {
		logf = func(format string, v ...any) {
			a.store.Logf(format, v...)
			fmt.Fprintf(c.stderr, format+"\n", v...)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := a.client.SetMyCommands(ctx, relay.Commands); err != nil {
		logf("setMyCommands: %v", err)
	}
	logf("%s", backendsLine("none (replies will be refused)"))
	terminals := term.Mux{Backends: map[string]term.Terminal{agterm.Backend: &agterm.CLI{}, tmux.Backend: &tmux.CLI{}}}
	router := &relay.Router{
		API: a.client, Store: a.store, Term: terminals, ChatID: a.cfg.ChatID, UserID: a.cfg.UserID, Secrets: a.cfg.Secrets(),
		Agents: func(ctx context.Context) ([]session.Agent, error) { return session.ListAgents(ctx, a.cfg.ClaudePath) },
		Log:    logf, StartedAt: time.Now(),
	}
	if a.cfg.VoiceCommand != "" {
		router.Transcriber = voiceRunner(c, a.cfg.VoiceCommand)
	}
	// A run killed before it cleaned up (a crash, kill -9, a power loss)
	// left its folder, audio included.
	if n, err := voiceRunner(c, a.cfg.VoiceCommand).RemoveStale(); err != nil {
		logf("voice: remove old note folders: %v", err)
	} else if n > 0 {
		logf("voice: removed %d note folder(s) left by an earlier run", n)
	}
	loop := &relay.Loop{Poll: a.client, Router: router, Store: a.store, Once: *once, Log: logf}
	if err := loop.Run(ctx); err != nil {
		fmt.Fprintf(c.stderr, "tg relay: %v\n", err)
		return exitUsage
	}
	logf("relay stopped")
	return exitOK
}

func relayInstall(c *cli, args []string) int {
	fs := flag.NewFlagSet("relay install", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	print := fs.Bool("print", false, "print the service file and exit")
	noLoad := fs.Bool("no-load", false, "write the service file without loading it")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	a, code := newApp(c)
	if code != exitOK {
		return code
	}
	home, _ := os.UserHomeDir()
	bin := a.cfg.BinaryPath
	if bin == "" {
		bin = binaryPath()
	}
	unit, err := renderServiceFile(bin, stateDir(c), home, map[string]string{"TG_CONFIG": configPath(c), "TG_STATE_DIR": stateDir(c)})
	if err != nil {
		fmt.Fprintf(c.stderr, "tg relay install: %v\n", err)
		return exitUsage
	}
	if *print {
		fmt.Fprint(c.stdout, unit)
		return exitOK
	}
	path := serviceFilePath(c)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(c.stderr, "tg relay install: %v\n", err)
		return exitUsage
	}
	if err := replaceFile(path, []byte(unit)); err != nil {
		fmt.Fprintf(c.stderr, "tg relay install: %v\n", err)
		return exitUsage
	}
	fmt.Fprintf(c.stdout, "Agent: %s\n", path)
	reportBackends(c.stdout, c.stderr)
	if *noLoad {
		return exitOK
	}
	if out, err := newServiceManager(c).Install(path); err != nil {
		if out != "" {
			err = fmt.Errorf("%w: %s", err, out)
		}
		fmt.Fprintf(c.stderr, "tg relay install: %v\n", err)
		return exitUsage
	}
	fmt.Fprintln(c.stdout, "Loaded. The relay starts now and on every login.")
	return relayStatus(c)
}

func relayUninstall(c *cli, args []string) int {
	fs := flag.NewFlagSet("relay uninstall", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	noLoad := fs.Bool("no-load", false, "remove the service file without talking to the service manager")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if err := removeRelayService(c, *noLoad); err != nil {
		fmt.Fprintf(c.stderr, "tg relay uninstall: %v\n", err)
		return exitUsage
	}
	return exitOK
}

// removeRelayService stops the relay through the service manager, unless
// noLoad says to leave the manager alone, and deletes the service file. The
// file stays, and the error says why nothing was removed, unless the manager
// confirmed the service is gone: "still active" and "unknown" both mean the
// relay may be running.
func removeRelayService(c *cli, noLoad bool) error {
	if !noLoad {
		if err := newServiceManager(c).Uninstall(); err != nil {
			return fmt.Errorf("%w; nothing removed", err)
		}
	}
	if err := os.Remove(serviceFilePath(c)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Fprintln(c.stdout, "Relay agent removed.")
	return nil
}

func relayStatus(c *cli) int {
	m := newServiceManager(c)
	path := serviceFilePath(c)
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(c.stdout, "%s: not installed (%s)\n", m.Kind(), path)
	} else {
		fmt.Fprintf(c.stdout, "%s: %s\n", m.Kind(), m.Status())
	}
	store, err := openStore(c)
	if err != nil {
		return exitOK
	}
	rs, _ := store.LoadRelay()
	if store.RelayLocked() {
		fmt.Fprintf(c.stdout, "relay: running (pid %d, since %s)\n", rs.PID, ago(rs.StartedAt))
	} else {
		fmt.Fprintln(c.stdout, "relay: not running")
	}
	fmt.Fprintln(c.stdout, backendsLine("none"))
	if !rs.LastUpdateAt.IsZero() {
		fmt.Fprintf(c.stdout, "last update: %s (offset %d)\n", ago(rs.LastUpdateAt), rs.Offset)
	}
	return exitOK
}
