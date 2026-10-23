// Package agterm reaches Claude Code sessions through agterm's control CLI:
// it checks that a session exists, reads its screen and types into it.
package agterm

import (
	"bytes"
	"context"
	"encoding/json"
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
	Path string // defaults to "agtermctl" on PATH
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

// Type injects text into the session's primary pane.
func (c *CLI) Type(ctx context.Context, p term.Pane, text string) error {
	_, err := c.run(ctx, text, "session", "type", "--stdin", "--target", p.Target)
	return err
}

// Screen returns the last lines of the session's terminal buffer.
func (c *CLI) Screen(ctx context.Context, p term.Pane, lines int) (string, error) {
	out, err := c.run(ctx, "", "session", "text", "--lines", strconv.Itoa(lines), "--target", p.Target)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
