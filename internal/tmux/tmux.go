// Package tmux reaches Claude Code sessions that run inside tmux: it checks
// that the pane exists, reads its screen and types into it with send-keys.
package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/IZZY-Agency/claude-tg/internal/term"
)

// Backend is the name this backend has in a term.Pane.
const Backend = "tmux"

// maxLiteralBytes caps one send-keys literal argument. tmux rejects command
// messages above 16 KiB, so a 4000-rune prompt is sent in several pieces.
const maxLiteralBytes = 4096

// CLI implements term.Terminal with the tmux binary. The pane's Target is a
// tmux pane id such as %5 and Socket the server socket path from $TMUX.
// Needs tmux 3.1 or newer (send-keys -H).
type CLI struct {
	Path string // defaults to "tmux" on PATH
}

func (c *CLI) path() string {
	if c.Path != "" {
		return c.Path
	}
	return "tmux"
}

// args prefixes the tmux arguments with the pane's socket when one is known.
func (c *CLI) args(p term.Pane, rest ...string) []string {
	if p.Socket == "" {
		return rest
	}
	return append([]string{"-S", p.Socket}, rest...)
}

func (c *CLI) run(ctx context.Context, p term.Pane, args ...string) ([]byte, error) {
	if p.Socket == "" {
		// Pane ids repeat across tmux servers; without the socket the default
		// server could hold an unrelated %N.
		return nil, errors.New("tmux pane " + p.Target + " has no socket path recorded; cannot tell which tmux server owns it")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.path(), c.args(p, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("tmux %s: %s", args[0], msg)
	}
	return out, nil
}

// Exists reports whether the pane is still known to its tmux server.
func (c *CLI) Exists(ctx context.Context, p term.Pane) bool {
	out, err := c.run(ctx, p, "display-message", "-p", "-t", p.Target, "#{pane_id}")
	return err == nil && strings.TrimSpace(string(out)) == p.Target
}

// Type sends the text to the pane: ordinary characters literally, the key
// sequences from package term as named keys.
func (c *CLI) Type(ctx context.Context, p term.Pane, text string) error {
	for _, cmd := range Commands(text) {
		args := append([]string{"send-keys", "-t", p.Target}, cmd...)
		if _, err := c.run(ctx, p, args...); err != nil {
			return err
		}
	}
	return nil
}

// Screen returns the last lines of the pane, scrollback included.
func (c *CLI) Screen(ctx context.Context, p term.Pane, lines int) (string, error) {
	out, err := c.run(ctx, p, "capture-pane", "-p", "-t", p.Target, "-S", "-"+strconv.Itoa(lines))
	if err != nil {
		return "", err
	}
	return Tail(string(out), lines), nil
}

var keyNames = map[rune]string{'\n': "Enter", '\x1b': "Escape", '\x7f': "BSpace"}

// Commands splits text into send-keys argument lists. A literal run becomes
// one or more {"-l", "--", chunk} of at most maxLiteralBytes; trailing
// semicolons of a chunk are sent as the hex key 3b because tmux's command
// parser eats an unescaped ";" at the end of an argument. A run of the same
// key becomes {"-N", n, Key}.
func Commands(text string) [][]string {
	var out [][]string
	var lit strings.Builder
	var key string
	keyCount := 0
	flushLit := func() {
		if lit.Len() > 0 {
			out = append(out, literalCommands(lit.String())...)
			lit.Reset()
		}
	}
	flushKeys := func() {
		if keyCount > 0 {
			out = append(out, repeated(keyCount, key))
			key, keyCount = "", 0
		}
	}
	for _, r := range text {
		if name, ok := keyNames[r]; ok {
			flushLit()
			if name != key {
				flushKeys()
				key = name
			}
			keyCount++
			continue
		}
		flushKeys()
		lit.WriteRune(r)
	}
	flushLit()
	flushKeys()
	return out
}

// repeated renders n presses of the given key arguments.
func repeated(n int, keyArgs ...string) []string {
	if n == 1 {
		return keyArgs
	}
	return append([]string{"-N", strconv.Itoa(n)}, keyArgs...)
}

// literalCommands chunks a literal run and protects trailing semicolons.
func literalCommands(s string) [][]string {
	var out [][]string
	for len(s) > 0 {
		chunk := s
		if len(chunk) > maxLiteralBytes {
			cut := maxLiteralBytes
			for cut > 0 && !utf8.RuneStart(s[cut]) {
				cut--
			}
			chunk = s[:cut]
		}
		s = s[len(chunk):]
		body := strings.TrimRight(chunk, ";")
		if body != "" {
			out = append(out, []string{"-l", "--", body})
		}
		if n := len(chunk) - len(body); n > 0 {
			out = append(out, repeated(n, "-H", "3b"))
		}
	}
	return out
}

// Tail drops trailing blank lines from capture-pane output and keeps the
// last n lines, so the result has the shape of agterm's `session text`.
func Tail(out string, n int) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
