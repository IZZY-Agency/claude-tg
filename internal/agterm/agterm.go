// Package agterm reaches Claude Code sessions through agterm's control CLI:
// it checks that a session exists, reads its screen and types into it.
package agterm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/term"
)

// Backend is the name this backend has in a term.Pane.
const Backend = "agterm"

// CLI implements term.Terminal with agtermctl. The pane's Target is the
// agterm session id.
type CLI struct {
	Path   string        // defaults to "agtermctl" on PATH
	Settle time.Duration // defaults to settleWait; tests shorten it
}

func (c *CLI) path() string {
	if c.Path != "" {
		return c.Path
	}
	return "agtermctl"
}

func (c *CLI) run(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.path(), args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("agtermctl %s: %s", args[0], msg)
	}
	return out, nil
}

// Exists reports whether a session with this id is in the agterm tree.
func (c *CLI) Exists(ctx context.Context, p term.Pane) bool {
	out, err := c.run(ctx, "", "tree", "--json")
	if err != nil {
		return false
	}
	var tree struct {
		Result struct {
			Tree struct {
				Workspaces []struct {
					Sessions []struct {
						ID string `json:"id"`
					} `json:"sessions"`
				} `json:"workspaces"`
			} `json:"tree"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &tree) != nil {
		return false
	}
	for _, ws := range tree.Result.Tree.Workspaces {
		for _, s := range ws.Sessions {
			if strings.EqualFold(s.ID, p.Target) {
				return true
			}
		}
	}
	return false
}

// Tab is an agterm session's tab: its name, which the owner may have set,
// and its terminal title.
type Tab struct {
	Name  string
	Title string
}

// Tab returns the tab of agterm session id. ok is false, with no error,
// when the tree has no such session.
func (c *CLI) Tab(ctx context.Context, id string) (Tab, bool, error) {
	out, err := c.run(ctx, "", "tree", "--json")
	if err != nil {
		return Tab{}, false, err
	}
	t, ok := findTab(out, id)
	return t, ok, nil
}

// findTab finds session id, ignoring case, in an agterm tree printed by
// `agtermctl tree --json`.
func findTab(tree []byte, id string) (Tab, bool) {
	if id == "" {
		return Tab{}, false
	}
	var t struct {
		Result struct {
			Tree struct {
				Workspaces []struct {
					Sessions []struct {
						ID    string `json:"id"`
						Name  string `json:"name"`
						Title string `json:"title"`
					} `json:"sessions"`
				} `json:"workspaces"`
			} `json:"tree"`
		} `json:"result"`
	}
	if json.Unmarshal(tree, &t) != nil {
		return Tab{}, false
	}
	for _, ws := range t.Result.Tree.Workspaces {
		for _, s := range ws.Sessions {
			if strings.EqualFold(s.ID, id) {
				return Tab{Name: s.Name, Title: s.Title}, true
			}
		}
	}
	return Tab{}, false
}

// Type injects text into the session's primary pane.
func (c *CLI) Type(ctx context.Context, p term.Pane, text string) error {
	_, err := c.run(ctx, text, "session", "type", "--stdin", "--target", p.Target)
	return err
}

// Screen returns the last lines of the session's terminal buffer.
func (c *CLI) Screen(ctx context.Context, p term.Pane, lines int) (string, error) {
	out, err := c.run(ctx, "", "session", "text", "--lines", strconv.Itoa(lines), "--target", p.Target)
	if err != nil {
		if strings.Contains(err.Error(), "session not realized") {
			return "", fmt.Errorf("%w: %v", term.ErrNotRealized, err)
		}
		return "", err
	}
	return string(out), nil
}

// Workspace is the agterm workspace a new session opens in, so that it does
// not land in whichever one is active.
const Workspace = "Telegram"

// interrupt is typed into a shell before a launch line. A tab resting at a
// shell may not be at a prompt: a builtin such as read can hold it, or a
// command can wait on a continuation line, and either would take the launch
// line as its own. Ctrl-C ends both, and drops a half-typed command.
const interrupt = "\x03"

// settleWait is how long the shell gets to come back from interrupt.
const settleWait = 300 * time.Millisecond

// Start starts l in a tab resting at a shell in dir when there is one (the
// owner's tab, which stays a shell when the program ends), else in a new
// session in Workspace. No tab of panes is used. The tab is looked at again
// after the interrupt, right before the line goes in. The leading space keeps
// the line out of the shell's history: it names a session id that can only be
// used once. A new session opens in the background, the selection at the Mac
// staying, and closes when the program exits. The name goes in --name= form: a
// folder can be called --command=x.
func (c *CLI) Start(ctx context.Context, panes []term.Pane, dir, name string, l term.Launch) (term.Pane, error) {
	if id, ok := c.restingTab(ctx, dir, panes); ok {
		p := term.Pane{Backend: Backend, Target: id}
		if err := c.Type(ctx, p, interrupt); err != nil {
			return term.Pane{}, err
		}
		select {
		case <-ctx.Done():
			return term.Pane{}, ctx.Err()
		case <-time.After(c.settle()):
		}
		if again, ok := c.restingTab(ctx, dir, panes); ok && again == id {
			if err := c.Type(ctx, p, " "+l.Line+term.Enter); err != nil {
				return term.Pane{}, err
			}
			return p, nil
		}
	}
	out, err := c.run(ctx, "", "session", "new", "--no-select", "--workspace-name", Workspace, "--create-workspace", "--cwd", dir, "--name="+name, "--command", l.Process)
	if err != nil {
		return term.Pane{}, err
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return term.Pane{}, errors.New("agtermctl session: no session id")
	}
	return term.Pane{Backend: Backend, Target: id}, nil
}

func (c *CLI) settle() time.Duration {
	if c.Settle > 0 {
		return c.Settle
	}
	return settleWait
}

// restingTab reads the tree for idleTab, leaving out the tabs of panes.
func (c *CLI) restingTab(ctx context.Context, dir string, panes []term.Pane) (string, bool) {
	tree, err := c.run(ctx, "", "tree", "--json")
	if err != nil {
		return "", false
	}
	taken := map[string]bool{}
	for _, p := range panes {
		if p.Backend == Backend {
			taken[strings.ToUpper(p.Target)] = true
		}
	}
	return idleTab(tree, dir, taken)
}

// tab is what the tree says about a session, for idleTab.
type tab struct {
	ID              string          `json:"id"`
	Cwd             string          `json:"cwd"`
	Foreground      []string        `json:"foreground"`
	ForegroundShell string          `json:"foregroundShell"`
	Overlay         bool            `json:"overlay"`
	PaneOverlays    []string        `json:"paneOverlays"`
	HUD             json.RawMessage `json:"hud"`
	Ask             json.RawMessage `json:"ask"`
	RemoteHost      string          `json:"remoteHost"`
	Realized        bool            `json:"realized"`
}

// resting reports whether the tab's main pane is at a shell and nothing
// covers it. A tab of any other kind is left alone: keys typed into it would
// reach a program, or a shell the owner cannot see.
func (t tab) resting() bool {
	return t.ForegroundShell != "" && len(t.Foreground) == 0 && t.Realized &&
		!t.Overlay && len(t.PaneOverlays) == 0 && t.HUD == nil && t.Ask == nil && t.RemoteHost == ""
}

// idleTab finds the first tab resting in exactly folder dir that is not in
// taken (upper-case ids). dir has its links resolved, and a tab must report
// that very path: a link it was opened through may point elsewhere by now.
func idleTab(tree []byte, dir string, taken map[string]bool) (string, bool) {
	var doc struct {
		Result struct {
			Tree struct {
				Workspaces []struct {
					Sessions []tab `json:"sessions"`
				} `json:"workspaces"`
			} `json:"tree"`
		} `json:"result"`
	}
	if json.Unmarshal(tree, &doc) != nil {
		return "", false
	}
	for _, ws := range doc.Result.Tree.Workspaces {
		for _, t := range ws.Sessions {
			if t.resting() && t.Cwd == dir && !taken[strings.ToUpper(t.ID)] {
				return t.ID, true
			}
		}
	}
	return "", false
}
