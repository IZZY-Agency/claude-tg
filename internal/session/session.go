// Package session identifies the Claude Code session a tg invocation belongs
// to and resolves its human-readable name.
package session

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/state"
)

// Env is what the process environment tells us about the current session.
type Env struct {
	SessionID       string
	Cwd             string
	AgtermSessionID string
	AgtermWindowID  string
	TmuxPane        string // tmux pane id such as %5
	TmuxSocket      string // tmux server socket path, from $TMUX
	PID             int
	// Own is true when these facts come from the session's own process
	// environment (hooks, tg run inside the session). Only then may they
	// replace the recorded working directory, pid and terminal pane.
	Own bool
}

// FromEnv reads the session facts Claude Code, agterm and tmux export.
func FromEnv(get func(string) string, cwd string) Env {
	pid, _ := strconv.Atoi(get("CLAUDE_PID"))
	e := Env{
		SessionID:       get("CLAUDE_CODE_SESSION_ID"),
		Cwd:             cwd,
		AgtermSessionID: get("AGTERM_SESSION_ID"),
		AgtermWindowID:  get("AGTERM_WINDOW_ID"),
		TmuxPane:        get("TMUX_PANE"),
		TmuxSocket:      tmuxSocket(get("TMUX")),
		PID:             pid,
	}
	e.Own = e.SessionID != ""
	if e.TmuxSocket == "" {
		e.TmuxPane = "" // pane ids repeat across servers; without the socket the pane is useless
	}
	return e
}

// tmuxSocket extracts the socket path from $TMUX, which is
// "path,server-pid,session-index". The path may contain commas, so the two
// trailing fields are cut from the right.
func tmuxSocket(v string) string {
	i := strings.LastIndex(v, ",")
	if i <= 0 {
		return ""
	}
	j := strings.LastIndex(v[:i], ",")
	if j <= 0 {
		return ""
	}
	return v[:j]
}

// Agent is one entry of `claude agents --json`.
type Agent struct {
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Cwd       string `json:"cwd"`
	PID       int    `json:"pid"`
	Kind      string `json:"kind"`   // interactive or background
	Status    string `json:"status"` // interactive: busy or idle
	State     string `json:"state"`  // background: running, blocked, ...
}

// Busy reports whether the session is in the middle of a turn.
func (a Agent) Busy() bool { return a.Status == "busy" || a.State == "running" }

// ParseAgents decodes the JSON array printed by `claude agents --json`.
func ParseAgents(b []byte) ([]Agent, error) {
	var out []Agent
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListAgents runs `claude agents --json` with a 5 s timeout.
func ListAgents(ctx context.Context, claudePath string) ([]Agent, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if claudePath == "" {
		claudePath = "claude"
	}
	out, err := exec.CommandContext(ctx, claudePath, "agents", "--json").Output()
	if err != nil {
		return nil, err
	}
	return ParseAgents(out)
}

// FallbackLabel is used when the session name cannot be resolved.
func FallbackLabel(cwd, sessionID string) string {
	base := ""
	if cwd != "" {
		base = filepath.Base(cwd)
	}
	short := sessionID
	if len(short) > 6 {
		short = short[:6]
	}
	switch {
	case base != "" && short != "":
		return base + "-" + short
	case base != "":
		return base
	case short != "":
		return short
	}
	return "mac"
}

const nameTTL = 10 * time.Minute

// Resolve loads the session record, refreshes it from env, resolves the name
// through `claude agents --json` when the cached one is stale, falls back to
// FallbackLabel, saves and returns it. It never fails because claude is missing.
func Resolve(ctx context.Context, store *state.Store, claudePath string, env Env) (*state.Session, error) {
	sess, err := store.LoadSession(env.SessionID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	name, resolvedAt := sess.Name, sess.NameResolvedAt
	// Re-resolve when no real name was ever resolved (fallback in use) or the
	// cached one is older than nameTTL. This may run a subprocess, so it
	// happens outside the session lock.
	if resolvedAt.IsZero() || now.Sub(resolvedAt) > nameTTL {
		if agents, err := ListAgents(ctx, claudePath); err == nil {
			for _, a := range agents {
				if a.SessionID == env.SessionID && a.Name != "" {
					name, resolvedAt = a.Name, now
					break
				}
			}
		}
	}
	return store.UpdateSession(env.SessionID, func(s *state.Session) {
		if env.Own {
			// The session's own environment is authoritative: a session resumed
			// in another terminal must not keep a stale pane.
			if env.Cwd != "" {
				s.Cwd = env.Cwd
			}
			if env.PID != 0 {
				s.PID = env.PID
			}
			s.AgtermSessionID, s.AgtermWindowID = env.AgtermSessionID, env.AgtermWindowID
			s.TmuxPane, s.TmuxSocket = env.TmuxPane, env.TmuxSocket
		}
		s.LastSeen = now
		if name != "" {
			s.Name, s.NameResolvedAt = name, resolvedAt
		}
		if s.Name == "" {
			s.Name = FallbackLabel(s.Cwd, s.SessionID)
		}
	})
}
