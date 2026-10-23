package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/session"
	"github.com/IZZY-Agency/claude-tg/internal/term"
)

func init() {
	register("sessions", "list known sessions and follow state: tg sessions [--json]", cmdSessions)
}

func cmdSessions(c *cli, args []string) int {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	a, code := newApp(c)
	if code != exitOK {
		return code
	}
	alive := map[string]bool{}
	if agents, err := session.ListAgents(context.Background(), a.cfg.ClaudePath); err == nil {
		for _, ag := range agents {
			alive[ag.SessionID] = true
		}
	}
	list, err := a.store.ListSessions()
	if err != nil {
		fmt.Fprintf(c.stderr, "tg sessions: %v\n", err)
		return 4
	}
	if *asJSON {
		type row struct {
			SessionID string    `json:"session_id"`
			Name      string    `json:"name"`
			Follow    bool      `json:"follow"`
			Alive     bool      `json:"alive"`
			Cwd       string    `json:"cwd"`
			Terminal  string    `json:"terminal"`
			LastSeen  time.Time `json:"last_seen"`
		}
		rows := make([]row, 0, len(list))
		for _, s := range list {
			rows = append(rows, row{s.SessionID, s.Name, s.Follow, alive[s.SessionID], s.Cwd, s.Pane().String(), s.LastSeen})
		}
		enc := json.NewEncoder(c.stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rows)
		return exitOK
	}
	w := tabwriter.NewWriter(c.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tFOLLOW\tALIVE\tTERMINAL\tCWD\tLAST_SEEN")
	for _, s := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Name, onOff(s.Follow), yesNo(alive[s.SessionID]), terminalCell(s.Pane()), s.Cwd, ago(s.LastSeen))
	}
	w.Flush()
	return exitOK
}

// terminalCell is the TERMINAL column: the tmux pane id is short and useful,
// an agterm session id is a uuid and is not.
func terminalCell(p term.Pane) string {
	switch p.Backend {
	case "tmux":
		return "tmux " + p.Target
	case "":
		return "-"
	}
	return p.Backend
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t).Round(time.Minute)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return t.Format("2006-01-02")
}
