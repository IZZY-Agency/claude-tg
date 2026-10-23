package relay

import (
	"context"
	"os"
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
