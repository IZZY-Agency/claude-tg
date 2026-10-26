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
	"unicode"
	"unicode/utf8"

	"github.com/IZZY-Agency/claude-tg/internal/render"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/transcript"
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
	TranscriptPath  string // the session's transcript, from a hook's input; "" otherwise
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

// TabFunc looks up the name of agterm session agtermID's tab; ok is false
// when agterm has no such session.
type TabFunc func(ctx context.Context, agtermID string) (name string, ok bool, err error)

// Options are what Resolve labels a session with: Tab looks up its agterm
// tab's name (nil = no lookup), and Clean cleans a tab name and a title as
// a message is (the secrets scrubbed, credentials redacted) before the
// label cuts them (nil = as they are).
type Options struct {
	Tab   TabFunc
	Clean func(string) string
}

// tabTimeout bounds the tab lookup: agtermctl answers in 10 to 20 ms, and
// the Stop hook's answer must not wait on a stalled agterm.
const tabTimeout = time.Second

// UserTab returns a tab name the owner chose, trimmed, and "" for a frozen
// copy of Claude Code's terminal title, which agterm gives a tab the owner
// did not name: its first rune is not a letter or a digit, and a space
// follows ("✳ Login token…", "◐ Claude Code"). (A tab the owner names
// "🔥 hotfix" is taken for such a copy.)
func UserTab(name string) string {
	name = strings.TrimSpace(name)
	r, size := utf8.DecodeRuneInString(name)
	if name == "" || !unicode.IsLetter(r) && !unicode.IsDigit(r) && strings.HasPrefix(name[size:], " ") {
		return ""
	}
	return name
}

// titleRunes is the most runes of Claude Code's title a label shows.
const titleRunes = 30

// Label is the name tg shows for a session (spec
// 2026-10-26-session-labels-and-picker-design.md): the owner's tab name
// (UserTab), else the last element of cwd, " · " and Claude Code's title
// cut to titleRunes (shortTitle), else Claude Code's name, else
// FallbackLabel.
func Label(tab, cwd, title, claudeName, sessionID string) string {
	if t := UserTab(tab); t != "" {
		return t
	}
	if title = shortTitle(title); title != "" {
		if cwd == "" {
			return title
		}
		return filepath.Base(cwd) + " · " + title
	}
	if strings.TrimSpace(claudeName) != "" {
		return claudeName
	}
	return FallbackLabel(cwd, sessionID)
}

// shortTitle returns title's words whole, as many as fit in titleRunes
// with a "…" after them, or title itself when it fits; a first word too
// long alone is cut.
func shortTitle(title string) string {
	words := strings.Fields(title)
	title = strings.Join(words, " ")
	if utf8.RuneCountInString(title) <= titleRunes {
		return title
	}
	out := ""
	for _, w := range words {
		next := w
		if out != "" {
			next = out + " " + w
		}
		if utf8.RuneCountInString(next) > titleRunes-1 {
			break
		}
		out = next
	}
	if out == "" {
		out = string([]rune(words[0])[:titleRunes-1])
	}
	return out + "…"
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

// Resolve loads the session record, refreshes it from env, resolves Claude
// Code's name through `claude agents --json` when the cached one is stale,
// reads the session's title from env's transcript (transcript.Title) and
// its tab name through opt.Tab within tabTimeout, cleans both (opt.Clean),
// labels the session (Label), saves and returns it. A title or tab it
// cannot read now keeps the one stored; a session whose own environment
// has no agterm id has no tab. It never fails because claude or agterm is
// missing.
func Resolve(ctx context.Context, store *state.Store, claudePath string, env Env, opt Options) (*state.Session, error) {
	clean := opt.Clean
	if clean == nil {
		clean = func(s string) string { return s }
	}
	sess, err := store.LoadSession(env.SessionID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	name, resolvedAt := sess.ClaudeName, sess.NameResolvedAt
	if name == "" && !resolvedAt.IsZero() {
		name = sess.Name // a record from before labels: Name was Claude Code's
	}
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
	title, haveTitle := "", false
	if env.TranscriptPath != "" {
		if t, ok, err := transcript.Title(env.TranscriptPath); err == nil && ok {
			title, haveTitle = clean(t), true
		}
	}
	agtermID := sess.AgtermSessionID
	if env.Own {
		agtermID = env.AgtermSessionID
	}
	tabName, haveTab := "", false
	switch {
	case agtermID == "" && env.Own:
		haveTab = true // out of agterm: no tab
	case agtermID != "" && opt.Tab != nil:
		tctx, cancel := context.WithTimeout(ctx, tabTimeout)
		n, ok, err := opt.Tab(tctx, agtermID)
		cancel()
		if err == nil {
			haveTab = true
			if ok {
				tabName = UserTab(clean(n))
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
			s.ClaudeName, s.NameResolvedAt = name, resolvedAt
		}
		if haveTitle {
			s.Title = title
		}
		if haveTab {
			s.Tab = tabName
		}
		// Every part cleaned, a title before it is cut, and the label too
		// (the folder), in the form every message shows it
		// (render.ShownName): the label stored is the label shown, and one
		// copied from a message addresses the session.
		label := clean(Label(clean(s.Tab), s.Cwd, clean(s.Title), clean(s.ClaudeName), s.SessionID))
		s.Name, s.FullLabel = render.ShownName(label), ""
		if full := render.FullName(label); full != s.Name {
			s.FullLabel = full // a name typed in full addresses the session too
		}
	})
}
