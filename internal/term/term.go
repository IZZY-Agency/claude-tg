// Package term is the backend-neutral way the relay reaches a Claude Code
// session's terminal pane: check it exists, read its screen, type into it.
// Backends live in internal/agterm and internal/tmux.
package term

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Key sequences typed into a pane. Backends translate them as needed.
const (
	Enter     = "\n"
	Escape    = "\x1b"
	Backspace = "\x7f"
)

// Pane names the terminal pane a session runs in.
type Pane struct {
	Backend string // "agterm", "tmux" or "" when unknown
	Target  string // agterm session id, tmux pane id such as %5
	Socket  string // tmux server socket path, empty for agterm
}

// IsZero reports whether no pane is known.
func (p Pane) IsZero() bool { return p.Backend == "" || p.Target == "" }

// String renders "backend:target", or "-" when no pane is known.
func (p Pane) String() string {
	if p.IsZero() {
		return "-"
	}
	return p.Backend + ":" + p.Target
}

// Terminal is what the relay needs from a terminal backend.
type Terminal interface {
	Exists(ctx context.Context, p Pane) bool
	Type(ctx context.Context, p Pane, text string) error
	Screen(ctx context.Context, p Pane, lines int) (string, error)
}

// NoBackendError is returned when a pane names a backend the Mux lacks.
type NoBackendError struct{ Backend string }

func (e *NoBackendError) Error() string {
	if e.Backend == "" {
		return "no terminal pane known for this session"
	}
	return fmt.Sprintf("no terminal backend %q", e.Backend)
}

// Mux dispatches on Pane.Backend.
type Mux struct {
	Backends map[string]Terminal
}

func (m Mux) backend(p Pane) (Terminal, error) {
	if p.IsZero() {
		return nil, &NoBackendError{Backend: p.Backend}
	}
	t, ok := m.Backends[p.Backend]
	if !ok || t == nil {
		return nil, &NoBackendError{Backend: p.Backend}
	}
	return t, nil
}

// Exists is false for a zero pane or an unknown backend.
func (m Mux) Exists(ctx context.Context, p Pane) bool {
	t, err := m.backend(p)
	return err == nil && t.Exists(ctx, p)
}

// Type forwards to the pane's backend.
func (m Mux) Type(ctx context.Context, p Pane, text string) error {
	t, err := m.backend(p)
	if err != nil {
		return err
	}
	return t.Type(ctx, p, text)
}

// Screen forwards to the pane's backend.
func (m Mux) Screen(ctx context.Context, p Pane, lines int) (string, error) {
	t, err := m.backend(p)
	if err != nil {
		return "", err
	}
	return t.Screen(ctx, p, lines)
}

// Tools maps a backend name to the executable it needs on PATH.
var Tools = map[string]string{"agterm": "agtermctl", "tmux": "tmux"}

// Order is the fixed order backends are reported in.
var Order = []string{"agterm", "tmux"}

// Detect returns the backends whose tool lookPath finds, in Order.
func Detect(lookPath func(string) (string, error)) []string {
	var out []string
	for _, name := range Order {
		if _, err := lookPath(Tools[name]); err == nil {
			out = append(out, name)
		}
	}
	return out
}

// LookPathIn finds an executable file named name in the colon-separated
// path list, like exec.LookPath but for a PATH other than the process's.
func LookPathIn(path, name string) (string, error) {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found in %s", name, path)
}

// DetectIn is Detect against an explicit PATH.
func DetectIn(path string) []string {
	return Detect(func(name string) (string, error) { return LookPathIn(path, name) })
}
