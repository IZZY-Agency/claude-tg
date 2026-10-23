package session

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/state"
)

func TestParseAgentsReal(t *testing.T) {
	b, err := os.ReadFile("testdata/agents.json")
	if err != nil {
		t.Fatal(err)
	}
	agents, err := ParseAgents(b)
	if err != nil || len(agents) == 0 {
		t.Fatalf("agents=%d err=%v", len(agents), err)
	}
	nameRe := regexp.MustCompile(`^[a-z0-9-]+-[0-9a-f]{2}$`)
	found := false
	for _, a := range agents {
		if a.Kind == "interactive" && nameRe.MatchString(a.Name) && a.SessionID != "" && a.Cwd != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no interactive agent with a session-style name in %+v", agents)
	}
}

func TestFromEnv(t *testing.T) {
	env := map[string]string{
		"CLAUDE_CODE_SESSION_ID": "6449d606-55c1-4e6d-a915-4b14c777f57f",
		"AGTERM_SESSION_ID":      "agt-s1",
		"AGTERM_WINDOW_ID":       "agt-w1",
		"CLAUDE_PID":             "10250",
		"TMUX_PANE":              "%5",
		"TMUX":                   "/private/tmp/tmux-501/default,4242,0",
	}
	e := FromEnv(func(k string) string { return env[k] }, "/Users/alex/code/acme-shop")
	if e.SessionID != env["CLAUDE_CODE_SESSION_ID"] || e.AgtermSessionID != "agt-s1" || e.AgtermWindowID != "agt-w1" || e.PID != 10250 || e.Cwd != "/Users/alex/code/acme-shop" {
		t.Fatalf("env=%+v", e)
	}
	if e.TmuxPane != "%5" || e.TmuxSocket != "/private/tmp/tmux-501/default" {
		t.Fatalf("tmux facts: %+v", e)
	}
	none := FromEnv(func(string) string { return "" }, "/x")
	if none.TmuxPane != "" || none.TmuxSocket != "" {
		t.Fatalf("no tmux: %+v", none)
	}
}

func TestFallbackLabel(t *testing.T) {
	if got := FallbackLabel("/Users/alex/code/acme-shop", "6449d606-55c1-4e6d-a915-4b14c777f57f"); got != "acme-shop-6449d6" {
		t.Fatalf("got %q", got)
	}
	if got := FallbackLabel("", ""); got != "mac" {
		t.Fatalf("got %q", got)
	}
	if got := FallbackLabel("/Users/x/proj", ""); got != "proj" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveWithoutClaude(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "st"))
	if err != nil {
		t.Fatal(err)
	}
	env := Env{SessionID: "6449d606-55c1-4e6d-a915-4b14c777f57f", Own: true, Cwd: "/Users/alex/code/acme-shop", PID: 1, AgtermSessionID: "agt-s1", TmuxPane: "%5", TmuxSocket: "/private/tmp/tmux-501/default"}
	start := time.Now()
	sess, err := Resolve(context.Background(), store, "/nonexistent/claude", env)
	if err != nil {
		t.Fatalf("Resolve must not fail when claude is missing: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("Resolve took %v", time.Since(start))
	}
	if sess.Name != "acme-shop-6449d6" || sess.AgtermSessionID != "agt-s1" || sess.Cwd != env.Cwd || sess.TmuxPane != "%5" || sess.TmuxSocket != "/private/tmp/tmux-501/default" {
		t.Fatalf("sess=%+v", sess)
	}
	again, _ := store.LoadSession(env.SessionID)
	if again.Name != "acme-shop-6449d6" || again.LastSeen.IsZero() {
		t.Fatalf("not persisted: %+v", again)
	}
}

func TestResolveUsesCachedNameWithinTenMinutes(t *testing.T) {
	store, _ := state.Open(filepath.Join(t.TempDir(), "st"))
	id := "6449d606-55c1-4e6d-a915-4b14c777f57f"
	_ = store.SaveSession(&state.Session{SessionID: id, Name: "acme-shop-92", NameResolvedAt: time.Now().Add(-2 * time.Minute)})
	sess, err := Resolve(context.Background(), store, "/nonexistent/claude", Env{SessionID: id, Cwd: "/x/acme-shop"})
	if err != nil || sess.Name != "acme-shop-92" {
		t.Fatalf("sess=%+v err=%v", sess, err)
	}
}

func TestTmuxSocketKeepsCommasInPath(t *testing.T) {
	cases := map[string]string{
		"/private/tmp/tmux-501/default,4242,0": "/private/tmp/tmux-501/default",
		"/tmp/work,team,4242,0":                "/tmp/work,team",
		"":                                     "",
		"garbage":                              "",
		"/tmp/x,1":                             "",
	}
	for in, want := range cases {
		if got := tmuxSocket(in); got != want {
			t.Errorf("tmuxSocket(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFromEnvDropsTmuxPaneWithoutSocket(t *testing.T) {
	env := map[string]string{"CLAUDE_CODE_SESSION_ID": "6449d606-55c1-4e6d-a915-4b14c777f57f", "TMUX_PANE": "%5"}
	e := FromEnv(func(k string) string { return env[k] }, "/x")
	if e.TmuxPane != "" || e.TmuxSocket != "" {
		t.Fatalf("pane without socket must be dropped: %+v", e)
	}
	if !e.Own {
		t.Fatal("an env with the session id is the session's own")
	}
	if FromEnv(func(string) string { return "" }, "/x").Own {
		t.Fatal("no session id, not own")
	}
}

func TestResolveReplacesTerminalBinding(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "st"))
	if err != nil {
		t.Fatal(err)
	}
	sid := "6449d606-55c1-4e6d-a915-4b14c777f57f"
	_ = store.SaveSession(&state.Session{SessionID: sid, Name: "x-1", AgtermSessionID: "A1", TmuxPane: "%5", TmuxSocket: "/s", Cwd: "/a", PID: 7})
	// The session was resumed in agterm: its own env now has no tmux facts.
	own := Env{SessionID: sid, Own: true, Cwd: "/b", PID: 9, AgtermSessionID: "A2"}
	sess, err := Resolve(context.Background(), store, "/nonexistent/claude", own)
	if err != nil {
		t.Fatal(err)
	}
	if sess.TmuxPane != "" || sess.TmuxSocket != "" || sess.AgtermSessionID != "A2" || sess.Cwd != "/b" || sess.PID != 9 {
		t.Fatalf("own env must replace the binding: %+v", sess)
	}
	// A foreign env (tg ... --session from elsewhere) must not touch any of it.
	foreign := Env{SessionID: sid, Cwd: "/c", PID: 11, TmuxPane: "%9", TmuxSocket: "/t", AgtermSessionID: "A3"}
	sess, err = Resolve(context.Background(), store, "/nonexistent/claude", foreign)
	if err != nil {
		t.Fatal(err)
	}
	if sess.TmuxPane != "" || sess.AgtermSessionID != "A2" || sess.Cwd != "/b" || sess.PID != 9 {
		t.Fatalf("foreign env must leave the binding alone: %+v", sess)
	}
}
