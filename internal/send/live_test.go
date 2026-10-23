package send

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/config"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

// TestLiveSendAndChunks talks to the real bot and the real chat. It runs only
// with TG_LIVE=1 and an existing config, and it puts two visible messages on
// the user's phone, both labelled as a test.
func TestLiveSendAndChunks(t *testing.T) {
	if os.Getenv("TG_LIVE") != "1" {
		t.Skip("set TG_LIVE=1 to run against the real bot")
	}
	path := os.Getenv("TG_CONFIG")
	if path == "" {
		path = config.DefaultPath()
	}
	cfg, err := config.Load(path)
	if errors.Is(err, config.ErrNotConfigured) {
		t.Skip("tg is not configured; run tg setup first")
	}
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(t.TempDir(), "st"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Sender{API: telegram.New(cfg.BotToken), Store: st, ChatID: cfg.ChatID, MinGap: 1100 * time.Millisecond}
	ctx := context.Background()
	ids, err := s.Send(ctx, Message{Label: "tg-live-test", Title: "short", Body: "tg live test " + time.Now().Format(time.RFC3339), Silent: true})
	if err != nil || len(ids) != 1 {
		t.Fatalf("short: ids=%v err=%v", ids, err)
	}
	long := strings.Repeat("tg live test: long message chunking check. ", 240) // ~10 KB
	start := time.Now()
	ids, err = s.Send(ctx, Message{Label: "tg-live-test", Body: long, Silent: true, Markdown: true})
	if err != nil || len(ids) != 3 {
		t.Fatalf("long: ids=%v err=%v", ids, err)
	}
	if el := time.Since(start); el < 2*time.Second {
		t.Fatalf("three chunks sent in %v; pacing of 1.1 s between sends is not applied", el)
	}
}
