package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/IZZY-Agency/claude-tg/internal/send"
	"github.com/IZZY-Agency/claude-tg/internal/state"
)

func init() {
	register("follow", "live feed of this session: tg follow on|off|status [--session id]", cmdFollow)
}

func cmdFollow(c *cli, args []string) int {
	usage := func() int {
		fmt.Fprintln(c.stderr, "usage: tg follow on|off|status [--session id]")
		return exitUsage
	}
	if len(args) == 0 {
		return usage()
	}
	action := args[0]
	if action != "on" && action != "off" && action != "status" {
		return usage()
	}
	fs := flag.NewFlagSet("follow", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	sessionFlag := fs.String("session", "", "session id (defaults to CLAUDE_CODE_SESSION_ID)")
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(c.stderr, "tg follow: unexpected argument %q\n", fs.Arg(0))
		return usage()
	}
	pos := []string{action}
	a, code := newApp(c)
	if code != exitOK {
		return code
	}
	// Reuse the hooks' client seam, so tests can point tg follow at a fake server.
	a.sender.API = hookClient(a.cfg.BotToken)
	sid := a.sessionID(*sessionFlag)
	if sid == "" {
		fmt.Fprintln(c.stderr, "tg follow: no session (run inside Claude Code or pass --session <id>)")
		return exitNoConfig
	}
	ctx := context.Background()
	sess, err := a.resolve(ctx, a.envFor(sid))
	if err != nil {
		fmt.Fprintf(c.stderr, "tg follow: %v\n", err)
		return 4
	}
	switch pos[0] {
	case "status":
		state := "off"
		if sess.Follow {
			state = "on"
		}
		fmt.Fprintf(c.stdout, "follow: %s (%s)\n", state, sess.Name)
		return exitOK
	case "on", "off":
		want := pos[0] == "on"
		// Off also forgets the session's voice marks (state.Session.SetFollow).
		if _, err := a.store.UpdateSession(sess.SessionID, func(s *state.Session) { s.SetFollow(want) }); err != nil {
			fmt.Fprintf(c.stderr, "tg follow: %v\n", err)
			return 4
		}
		text := "following " + sess.Name
		if !want {
			text = "stopped following " + sess.Name
		}
		if _, err := a.sender.Send(ctx, send.Message{SessionID: sess.SessionID, Label: sess.Name, Body: text, Silent: true, Kind: send.KindSystem}); err != nil {
			a.store.Logf("follow: confirmation: %v", err)
		}
		fmt.Fprintf(c.stdout, "follow: %s (%s)\n", pos[0], sess.Name)
		return exitOK
	}
	return exitUsage
}
