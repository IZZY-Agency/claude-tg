package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/IZZY-Agency/claude-tg/internal/send"
)

func init() {
	register("send", "post a message: tg send [--level critical|info] [--title t] [--silent] [--from label] [--session id] <text | ->", cmdSend)
}

func cmdSend(c *cli, args []string) int {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	level := fs.String("level", "critical", "critical (audible) or info (silent)")
	title := fs.String("title", "", "short bold subject")
	silent := fs.Bool("silent", false, "send without sound")
	from := fs.String("from", "", "label when not inside a Claude session")
	sessionFlag := fs.String("session", "", "session id (defaults to CLAUDE_CODE_SESSION_ID)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *level != "critical" && *level != "info" {
		fmt.Fprintln(c.stderr, "tg send: --level must be critical or info")
		return exitUsage
	}
	text := strings.Join(fs.Args(), " ")
	if text == "-" {
		b, err := io.ReadAll(c.stdin)
		if err != nil {
			fmt.Fprintf(c.stderr, "tg send: read stdin: %v\n", err)
			return exitUsage
		}
		text = string(b)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		fmt.Fprintln(c.stderr, "tg send: text is empty (pass it as arguments or `-` for stdin)")
		return exitUsage
	}
	a, code := newApp(c)
	if code != exitOK {
		return code
	}
	ctx := context.Background()
	m := send.Message{Title: *title, Body: text, Silent: *silent || *level == "info", Markdown: true, Kind: send.KindAlert}
	if *level == "info" {
		m.Kind = send.KindReport
	}
	if sid := a.sessionID(*sessionFlag); sid != "" {
		sess, err := a.resolve(ctx, a.envFor(sid))
		if err != nil {
			a.store.Logf("send: resolve: %v", err)
		} else {
			m.SessionID, m.Label = sess.SessionID, sess.Name
		}
	}
	if *from != "" {
		m.Label = *from
	}
	ids, err := a.sender.Send(ctx, m)
	if err != nil {
		a.store.Logf("send: %v", err)
		if code := send.Classify(err); code == 4 {
			fmt.Fprintln(c.stdout, "spooled")
			return code
		} else {
			fmt.Fprintf(c.stderr, "tg send: %v\n", err)
			return code
		}
	}
	fmt.Fprintf(c.stdout, "sent %s\n", joinIDs(ids))
	return exitOK
}

func joinIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}
	return strings.Join(parts, ",")
}
