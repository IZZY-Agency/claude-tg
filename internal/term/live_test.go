package term_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/agterm"
	"github.com/IZZY-Agency/claude-tg/internal/term"
)

// TestLiveSignatureStable reads a real Claude Code session's agterm pane
// while it shows a permission dialog or a question picker, and checks what
// the waiting hook's screen guard relies on (ruling R30): the signature is
// found, the dialog shows the call's key (a picker: in the question on
// screen, term.PickerView), and the signature is the same on every read.
// It needs
// TG_LIVE=1, TG_LIVE_PANE=<agterm session id>, TG_LIVE_KIND=permission or
// picker, and TG_LIVE_KEY=<the call's key: the command's first 40
// characters, the file's base name, the host or the question>. TG_LIVE_FOR
// (default 3m) and TG_LIVE_EVERY (default 5s) set how long and how often it
// reads; TG_LIVE_SAVE=<dir> keeps every screen read. Nothing is typed into
// the pane. Run with a -timeout above TG_LIVE_FOR:
//
//	TG_LIVE=1 TG_LIVE_PANE=… TG_LIVE_KIND=permission TG_LIVE_KEY='touch x' \
//	  go test -run TestLiveSignatureStable -v -timeout 10m ./internal/term/
func TestLiveSignatureStable(t *testing.T) {
	pane, kind, key := os.Getenv("TG_LIVE_PANE"), os.Getenv("TG_LIVE_KIND"), os.Getenv("TG_LIVE_KEY")
	if os.Getenv("TG_LIVE") != "1" || pane == "" || key == "" || (kind != "permission" && kind != "picker") {
		t.Skip("set TG_LIVE=1, TG_LIVE_PANE, TG_LIVE_KIND=permission|picker and TG_LIVE_KEY")
	}
	dur, every := liveDuration(t, "TG_LIVE_FOR", 3*time.Minute), liveDuration(t, "TG_LIVE_EVERY", 5*time.Second)
	signature, shows := term.PermissionSignature, term.PermissionSignature
	if kind == "picker" {
		signature, shows = term.PickerSignature, term.PickerView
	}
	save := os.Getenv("TG_LIVE_SAVE")
	cli := &agterm.CLI{}
	p := term.Pane{Backend: agterm.Backend, Target: pane}
	var first string
	reads, differ, failed := 0, 0, 0
	start := time.Now()
	for {
		at := time.Since(start).Round(time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		screen, err := cli.Screen(ctx, p, 60)
		cancel()
		reads++
		switch {
		case err != nil:
			failed++
			t.Logf("read %d at %s: %v", reads, at, err)
		default:
			if save != "" {
				if err := os.WriteFile(filepath.Join(save, fmt.Sprintf("%s-%02d.txt", kind, reads)), []byte(screen), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			sig := signature(screen)
			switch {
			case reads == 1 && (sig == "" || !term.SignatureHas(shows(screen), key)):
				t.Fatalf("no %s showing %q on the pane (signature %q):\n%s", kind, key, sig, screen)
			case reads == 1:
				first = sig
			case sig != first:
				differ++
				t.Errorf("read %d at %s: signature %q, want %q\n%s", reads, at, sig, first, screen)
			}
		}
		if time.Since(start)+every > dur+time.Second {
			break
		}
		time.Sleep(every)
	}
	t.Logf("%s: %d reads over %s every %s, %d differing, %d failed; signature %q",
		kind, reads, time.Since(start).Round(time.Second), every, differ, failed, first)
}

func liveDuration(t *testing.T, name string, def time.Duration) time.Duration {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return d
}
