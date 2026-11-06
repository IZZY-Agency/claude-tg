package relay

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/agterm"
	"github.com/IZZY-Agency/claude-tg/internal/config"
	"github.com/IZZY-Agency/claude-tg/internal/relay/relaytest"
	"github.com/IZZY-Agency/claude-tg/internal/session"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
	"github.com/IZZY-Agency/claude-tg/internal/term"
	"github.com/IZZY-Agency/claude-tg/internal/tmux"
)

// TestLiveDeliverToSession types into a real Claude Code session through
// agterm or tmux. It needs TG_LIVE=1 and TG_LIVE_SESSION_NAME=<session name> of a
// throwaway session started with --permission-mode default (the second half
// needs a real permission dialog) that has already run one turn, so the
// hooks recorded its agterm pane. The Telegram side is a recorder;
// everything else is real.
func TestLiveDeliverToSession(t *testing.T) {
	name := os.Getenv("TG_LIVE_SESSION_NAME")
	if os.Getenv("TG_LIVE") != "1" || name == "" {
		t.Skip("set TG_LIVE=1 and TG_LIVE_SESSION_NAME")
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(config.StateDir())
	if err != nil {
		t.Fatal(err)
	}
	api := &relaytest.API{}
	tt := term.Mux{Backends: map[string]term.Terminal{"agterm": &agterm.CLI{}, "tmux": &tmux.CLI{}}}
	r := &Router{
		API: api, Store: st, Term: tt, ChatID: cfg.ChatID, UserID: cfg.UserID,
		Agents: func(ctx context.Context) ([]session.Agent, error) { return session.ListAgents(ctx, cfg.ClaudePath) },
		Log:    func(string, ...any) {}, StartedAt: time.Now(),
	}
	ctx := context.Background()
	live, err := r.live(ctx)
	if err != nil {
		t.Fatal(err)
	}
	target := byName(live, name)
	if target == nil || target.Pane.IsZero() {
		t.Fatalf("session %q not live with a pane: %+v", name, live)
	}
	msg := func(text string) telegram.Update {
		return telegram.Update{Message: &telegram.Message{MessageID: 900, Date: time.Now().Unix(), Chat: telegram.Chat{ID: cfg.ChatID, Type: "private"}, From: &telegram.User{ID: r.userID()}, Text: text}}
	}
	waitScreen := func(want string, within time.Duration) string {
		deadline := time.Now().Add(within)
		for time.Now().Before(deadline) {
			s, _ := tt.Screen(ctx, target.Pane, 60)
			if strings.Contains(s, want) {
				return s
			}
			time.Sleep(2 * time.Second)
		}
		s, _ := tt.Screen(ctx, target.Pane, 60)
		return s
	}

	// 1. Plain text addressed by name lands as a prompt.
	if err := r.Handle(ctx, msg("@"+name+" Reply with exactly these words: v2 delivery ok")); err != nil {
		t.Fatal(err)
	}
	if len(api.Reactions) != 1 || api.Reactions[0].Emoji != "👍" {
		t.Fatalf("no 👍: reactions=%+v sent=%+v", api.Reactions, api.Sent)
	}
	if s := waitScreen("v2 delivery ok", 90*time.Second); !strings.Contains(s, "v2 delivery ok") {
		t.Fatalf("typed text never appeared on screen:\n%s", s)
	}
	// An answer is now owed to the phone; the Stop hook pays it off.
	sess, _ := st.LoadSession(target.SessionID)
	if sess.PendingAnswers == 0 && !sess.Follow {
		t.Fatalf("no pending answer armed: %+v", sess)
	}

	// 2. A command that needs approval, approved from the phone.
	marker := "/tmp/tg-v2-marker-" + time.Now().Format("150405")
	if s := waitScreen("❯ \n", 90*time.Second); !strings.Contains(s, "❯") {
		t.Fatalf("session did not return to its prompt:\n%s", s)
	}
	time.Sleep(3 * time.Second)
	if err := r.Handle(ctx, msg("@"+name+" Run exactly this shell command and nothing else: touch "+marker)); err != nil {
		t.Fatal(err)
	}
	if s := waitScreen("Do you want to proceed?", 120*time.Second); !term.PromptVisible(s) {
		t.Fatalf("no permission dialog appeared:\n%s", s)
	}
	// Plain text must be refused while the dialog is up.
	before := len(api.Sent)
	_ = r.Handle(ctx, msg("@"+name+" hello"))
	if len(api.Sent) != before+1 || !strings.Contains(api.Sent[before].Text, "/approve") {
		t.Fatalf("plain text was not refused during the dialog: %+v", api.Sent[before:])
	}
	if err := r.Handle(ctx, msg("/approve @"+name)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			_ = os.Remove(marker)
			return
		}
		time.Sleep(2 * time.Second)
	}
	s, _ := tt.Screen(ctx, target.Pane, 60)
	t.Fatalf("marker %s never appeared after /approve:\n%s", marker, s)
}

// TestLiveNewSession starts real Claude Code sessions the way /new does, in
// fresh git repositories, so Claude Code asks about trusting each. One is
// trusted and gets its first message: a fresh session's input line shows a
// suggestion, which must not stop the text. The other is cancelled, which
// must close its tab. It needs TG_LIVE=1 and a running agterm;
// TG_LIVE_PROJECTS is the folder to work in, by default a temporary one. The
// Telegram side is a recorder and the state folder a temporary one, so the
// owner's pick stays; everything else is real. The first message costs one
// short turn.
func TestLiveNewSession(t *testing.T) {
	if os.Getenv("TG_LIVE") != "1" {
		t.Skip("set TG_LIVE=1")
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		t.Fatal(err)
	}
	root := os.Getenv("TG_LIVE_PROJECTS")
	if root == "" {
		root = t.TempDir()
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	tt := term.Mux{Backends: map[string]term.Terminal{"agterm": &agterm.CLI{}, "tmux": &tmux.CLI{}}}
	ctx := context.Background()
	mode := os.Getenv("TG_LIVE_PERMISSION_MODE") // empty: no flag, Claude Code's default

	// asked starts a session in a new repository, up to the question.
	asked := func(t *testing.T, before func(project string)) (r *Router, api *relaytest.API, started *startedSession, yes, cancel string) {
		t.Helper()
		st, err := state.Open(filepath.Join(t.TempDir(), "st"))
		if err != nil {
			t.Fatal(err)
		}
		name := "live-new-" + newKey()
		project := filepath.Join(root, "client", name)
		if err := os.MkdirAll(project, 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("git", "init", "-q", project).CombinedOutput(); err != nil {
			t.Fatalf("git init: %v: %s", err, out)
		}
		if before != nil {
			before(project)
		}
		api, started = &relaytest.API{}, &startedSession{}
		r = &Router{
			API: api, Store: st, Term: tt, ChatID: cfg.ChatID, UserID: cfg.ChatID,
			Agents: func(ctx context.Context) ([]session.Agent, error) { return session.ListAgents(ctx, cfg.ClaudePath) },
			Log:    t.Logf, StartedAt: time.Now(), ProjectsDir: root,
			Start: func(ctx context.Context, panes []term.Pane, dir, name, sessionID string) (term.Pane, error) {
				launch, err := session.Launch(session.LoginShell(ctx), cfg.ClaudePath, sessionID, mode)
				if err != nil {
					return term.Pane{}, err
				}
				started.sessionID = sessionID
				started.pane, err = tt.Start(ctx, panes, dir, name, launch)
				return started.pane, err
			},
		}
		t.Cleanup(func() {
			if started.pane.Backend == "agterm" {
				_ = exec.Command("agtermctl", "session", "close", "--target", started.pane.Target).Run()
			}
		})
		if err := r.Handle(ctx, liveMsg(cfg, 1, "/new "+name)); err != nil {
			t.Fatal(err)
		}
		r.WaitStarts()
		if len(api.Edits) == 0 {
			t.Fatalf("no outcome; sent %+v", api.Sent)
		}
		e := api.Edits[len(api.Edits)-1]
		t.Logf("outcome: %s", e.Text)
		if !strings.HasPrefix(e.Text, "🔐 ") || len(e.Keyboard) != 1 {
			t.Fatal("Claude Code did not ask about the folder")
		}
		return r, api, started, e.Keyboard[0][0].Data, e.Keyboard[0][1].Data
	}
	tap := func(data string) telegram.Update {
		return telegram.Update{UpdateID: 2, CallbackQuery: &telegram.CallbackQuery{ID: "cq", From: &telegram.User{ID: cfg.ChatID},
			Message: &telegram.Message{MessageID: 1001, Chat: telegram.Chat{ID: cfg.ChatID}}, Data: data}}
	}

	t.Run("trusted, it takes the first message", func(t *testing.T) {
		r, api, started, yes, _ := asked(t, nil)
		if err := r.Handle(ctx, tap(yes)); err != nil {
			t.Fatal(err)
		}
		r.WaitStarts()
		outcome := api.Edits[len(api.Edits)-1].Text
		t.Logf("outcome: %s", outcome)
		if !strings.HasPrefix(outcome, "✅ ") {
			t.Fatal("the session did not come up")
		}
		if ws := liveWorkspace(t, started.pane.Target); ws != agterm.Workspace {
			t.Fatalf("the new tab is in workspace %q", ws)
		}
		// Claude Code is listed before it has drawn its status line.
		drawn := time.Now().Add(20 * time.Second)
		for {
			screen, err := tt.Screen(ctx, started.pane, screenLines)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(screen, "shift+tab to cycle") {
				want := map[string]string{"": "auto mode on", "bypassPermissions": "bypass permissions on"}[mode]
				if !strings.Contains(screen, want) {
					t.Fatalf("no %q on the screen:\n%s", want, screen)
				}
				break
			}
			if time.Now().After(drawn) {
				t.Fatalf("no status line:\n%s", screen)
			}
			time.Sleep(200 * time.Millisecond)
		}
		const text = "reply with the single word pong and nothing else"
		if err := r.Handle(ctx, liveMsg(cfg, 3, text)); err != nil {
			t.Fatal(err)
		}
		if len(api.Reactions) == 0 || api.Reactions[len(api.Reactions)-1].Emoji != "👍" {
			t.Fatalf("not delivered: reactions %+v, sent %+v", api.Reactions, api.Sent)
		}
		// The transcript, not the pane: a background tab is short, and the
		// answer scrolls out of what a read of it returns.
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		pattern := filepath.Join(home, ".claude", "projects", "*", started.sessionID+".jsonl")
		deadline := time.Now().Add(90 * time.Second)
		for {
			files, _ := filepath.Glob(pattern)
			for _, f := range files {
				b, _ := os.ReadFile(f)
				for _, line := range strings.Split(string(b), "\n") {
					if strings.Contains(line, `"type":"assistant"`) && strings.Contains(strings.ToLower(line), `"text":"pong`) {
						return
					}
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("no answer in %s", pattern)
			}
			time.Sleep(time.Second)
		}
	})

	// The owner's tab takes the session, whatever the shell in it was doing:
	// the launch line must not join these, or be taken as input. A list that
	// was already submitted, such as "read x; cmd", goes on past the read as
	// it would after Ctrl-C at the keyboard, which is why this read has none.
	for name, left := range map[string]func(proof string) string{
		"half typed, cursor inside": func(proof string) string { return "touch " + proof + "\x1b[D\x1b[D" },
		"waiting on a continuation": func(proof string) string { return "touch " + proof + " \\\n" },
		"waiting in read":           func(string) string { return "read answer\n" },
	} {
		t.Run("a tab at a shell takes it: "+name, func(t *testing.T) {
			var proof string
			var idle term.Pane
			r, api, started, yes, _ := asked(t, func(project string) {
				proof = filepath.Join(project, "ran")
				out, err := exec.Command("agtermctl", "session", "new", "--no-select", "--workspace-name", agterm.Workspace, "--create-workspace", "--cwd", project, "--name=live-idle").Output()
				if err != nil {
					t.Fatal(err)
				}
				idle = term.Pane{Backend: "agterm", Target: strings.TrimSpace(string(out))}
				t.Cleanup(func() { _ = exec.Command("agtermctl", "session", "close", "--target", idle.Target).Run() })
				deadline := time.Now().Add(20 * time.Second)
				for !liveResting(t, idle.Target) {
					if time.Now().After(deadline) {
						t.Fatal("the tab never came to rest at a shell")
					}
					time.Sleep(200 * time.Millisecond)
				}
				time.Sleep(time.Second) // the prompt is drawn after the shell takes the foreground
				if err := tt.Type(ctx, idle, left(proof)); err != nil {
					t.Fatal(err)
				}
				time.Sleep(500 * time.Millisecond)
			})
			if !strings.EqualFold(started.pane.Target, idle.Target) {
				t.Fatalf("opened %s instead of using %s", started.pane.Target, idle.Target)
			}
			if err := r.Handle(ctx, tap(yes)); err != nil {
				t.Fatal(err)
			}
			r.WaitStarts()
			if outcome := api.Edits[len(api.Edits)-1].Text; !strings.HasPrefix(outcome, "✅ ") {
				t.Fatalf("outcome: %s", outcome)
			}
			if _, err := os.Stat(proof); err == nil {
				t.Fatal("what was left in the shell ran")
			}
		})
	}

	t.Run("cancelled, its tab closes", func(t *testing.T) {
		r, api, started, _, cancel := asked(t, nil)
		if err := r.Handle(ctx, tap(cancel)); err != nil {
			t.Fatal(err)
		}
		if outcome := api.Edits[len(api.Edits)-1].Text; outcome != newCancelled {
			t.Fatalf("outcome: %s", outcome)
		}
		deadline := time.Now().Add(10 * time.Second)
		for tt.Exists(ctx, started.pane) {
			if time.Now().After(deadline) {
				t.Fatal("the tab is still open")
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
}

// liveTab is a tab as `agtermctl tree --json` shows it, with its workspace.
func liveTab(t *testing.T, id string) (workspace string, resting bool) {
	t.Helper()
	out, err := exec.Command("agtermctl", "tree", "--json").Output()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Result struct {
			Tree struct {
				Workspaces []struct {
					Name     string `json:"name"`
					Sessions []struct {
						ID              string `json:"id"`
						ForegroundShell string `json:"foregroundShell"`
					} `json:"sessions"`
				} `json:"workspaces"`
			} `json:"tree"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	for _, ws := range doc.Result.Tree.Workspaces {
		for _, s := range ws.Sessions {
			if strings.EqualFold(s.ID, id) {
				return ws.Name, s.ForegroundShell != ""
			}
		}
	}
	return "", false
}

func liveWorkspace(t *testing.T, id string) string {
	t.Helper()
	ws, _ := liveTab(t, id)
	return ws
}

func liveResting(t *testing.T, id string) bool {
	t.Helper()
	_, resting := liveTab(t, id)
	return resting
}

// startedSession is what a live /new started.
type startedSession struct {
	sessionID string
	pane      term.Pane
}

// liveMsg is a message from the owner.
func liveMsg(cfg *config.Config, id int64, text string) telegram.Update {
	return telegram.Update{UpdateID: id, Message: &telegram.Message{MessageID: id, Date: time.Now().Unix(),
		Chat: telegram.Chat{ID: cfg.ChatID, Type: "private"}, From: &telegram.User{ID: cfg.ChatID}, Text: text}}
}
