package relay

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

type fakePoller struct {
	batches [][]telegram.Update
	calls   []int64 // offsets requested
}

func (p *fakePoller) GetUpdates(_ context.Context, offset int64, _ int) ([]telegram.Update, error) {
	p.calls = append(p.calls, offset)
	if len(p.batches) == 0 {
		return nil, nil
	}
	b := p.batches[0]
	p.batches = p.batches[1:]
	return b, nil
}

func TestPartitionStale(t *testing.T) {
	now := time.Date(2026, 10, 22, 21, 0, 0, 0, time.UTC)
	ups := []telegram.Update{
		{UpdateID: 1, Message: &telegram.Message{Date: now.Add(-time.Hour).Unix()}},
		{UpdateID: 2, Message: &telegram.Message{Date: now.Add(-5 * time.Minute).Unix()}},
		{UpdateID: 3, CallbackQuery: &telegram.CallbackQuery{Message: &telegram.Message{Date: now.Add(-20 * time.Minute).Unix()}}},
	}
	fresh, stale := PartitionStale(ups, now, 10*time.Minute)
	if len(fresh) != 1 || fresh[0].UpdateID != 2 || len(stale) != 2 {
		t.Fatalf("fresh=%+v stale=%+v", fresh, stale)
	}
}

func TestRunOnceSkipsStaleAndAdvancesOffset(t *testing.T) {
	f := newFixture(t)
	old := f.now.Add(-time.Hour).Unix()
	stale1 := f.msg("old one", 101)
	stale1.UpdateID, stale1.Message.Date = 7, old
	stale2 := f.msg("old two", 101)
	stale2.UpdateID, stale2.Message.Date = 8, old
	fresh := f.msg("fresh one", 101)
	fresh.UpdateID = 9
	p := &fakePoller{batches: [][]telegram.Update{{stale1, stale2, fresh}}}
	l := &Loop{Poll: p, Router: f.r, Store: f.store, Once: true, MaxAge: 10 * time.Minute, Log: func(string, ...any) {}}
	if err := l.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.typedOnce(t, "A1", " fresh one\n")
	skipped := false
	for _, s := range f.api.Sent {
		if strings.Contains(s.Text, "skipped 2") {
			skipped = true
		}
	}
	if !skipped {
		t.Fatalf("no skipped notice in %+v", f.api.Sent)
	}
	rs, _ := f.store.LoadRelay()
	if rs.Offset != 10 {
		t.Fatalf("offset=%d want 10", rs.Offset)
	}
	if len(p.calls) == 0 || p.calls[0] != 0 {
		t.Fatalf("first poll offset=%v", p.calls)
	}
}

func TestRunRefusesSecondInstance(t *testing.T) {
	f := newFixture(t)
	release, err := f.store.LockRelay()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	l := &Loop{Poll: &fakePoller{}, Router: f.r, Store: f.store, Once: true, Log: func(string, ...any) {}}
	if err := l.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunSurvivesTerminalError(t *testing.T) {
	f := newFixture(t)
	f.term.TypeErr = errors.New("agtermctl: socket closed")
	u1 := f.msg("first", 101)
	u1.UpdateID = 1
	u2 := f.msg("/status", 0)
	u2.UpdateID = 2
	p := &fakePoller{batches: [][]telegram.Update{{u1, u2}}}
	l := &Loop{Poll: p, Router: f.r, Store: f.store, Once: true, MaxAge: 10 * time.Minute, Log: func(string, ...any) {}}
	if err := l.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	var sawErr, sawStatus bool
	for _, s := range f.api.Sent {
		if strings.Contains(s.Text, "socket closed") {
			sawErr = true
		}
		if strings.Contains(s.Text, "relay up") {
			sawStatus = true
		}
	}
	if !sawErr || !sawStatus {
		t.Fatalf("sent=%+v", f.api.Sent)
	}
}

func TestPlistContainsLabelAndPath(t *testing.T) {
	p := Plist("/Users/x/.local/bin/tg", "/Users/x/.local/state/claude-tg", "/Users/x", nil)
	for _, want := range []string{"<string>" + Label + "</string>", "<string>/Users/x/.local/bin/tg</string>", "<string>relay</string>", "KeepAlive", "RunAtLoad", "/Users/x/.local/bin:/opt/homebrew/bin:/home/linuxbrew/.linuxbrew/bin:/usr/local/bin:/usr/bin:/bin", "/Users/x/.local/state/claude-tg/relay.out.log"} {
		if !strings.Contains(p, want) {
			t.Fatalf("plist lacks %q:\n%s", want, p)
		}
	}
	if PlistPath("/Users/x") != "/Users/x/Library/LaunchAgents/"+Label+".plist" {
		t.Fatalf("path=%q", PlistPath("/Users/x"))
	}
}

func TestOffsetClaimedBeforeHandling(t *testing.T) {
	f := newFixture(t)
	var offsetAtType int64 = -1
	f.term.OnType = func(string, string) {
		rs, _ := f.store.LoadRelay()
		offsetAtType = rs.Offset
	}
	u := f.msg("ship it", 101)
	u.UpdateID = 41
	p := &fakePoller{batches: [][]telegram.Update{{u}}}
	l := &Loop{Poll: p, Router: f.r, Store: f.store, Once: true, MaxAge: 10 * time.Minute, Log: func(string, ...any) {}}
	if err := l.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if offsetAtType != 42 {
		t.Fatalf("offset at typing time = %d, want 42 (claimed before any keystroke)", offsetAtType)
	}
}

func TestStaleFilteredAcrossStartupBatches(t *testing.T) {
	f := newFixture(t)
	old := f.now.Add(-time.Hour).Unix()
	var first []telegram.Update
	for i := 1; i <= 100; i++ {
		u := f.msg("old", 101)
		u.UpdateID, u.Message.Date = int64(i), old
		first = append(first, u)
	}
	s1 := f.msg("old too", 101)
	s1.UpdateID, s1.Message.Date = 101, old
	fresh := f.msg("fresh", 101)
	fresh.UpdateID = 102
	p := &fakePoller{batches: [][]telegram.Update{first, {s1, fresh}}}
	l := &Loop{Poll: p, Router: f.r, Store: f.store, MaxAge: 10 * time.Minute, Log: func(string, ...any) {}}
	ctx, cancel := context.WithCancel(context.Background())
	// Stop after the second batch: the poller then returns nothing and we cancel.
	go func() {
		for len(p.calls) < 3 {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	_ = l.Run(ctx)
	f.typedOnce(t, "A1", " fresh\n")
	notices := 0
	for _, m := range f.api.Sent {
		if strings.Contains(m.Text, "skipped 101") {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("want one 'skipped 101' notice, sent=%+v", f.api.Sent)
	}
}

func TestCancelStopsBeforeClaiming(t *testing.T) {
	f := newFixture(t)
	u1 := f.msg("one", 101)
	u1.UpdateID = 1
	u2 := f.msg("two", 101)
	u2.UpdateID = 2
	ctx, cancel := context.WithCancel(context.Background())
	f.term.OnType = func(string, string) { cancel() } // cancel while handling the first
	p := &fakePoller{batches: [][]telegram.Update{{u1, u2}}}
	l := &Loop{Poll: p, Router: f.r, Store: f.store, Once: true, MaxAge: 10 * time.Minute, Log: func(string, ...any) {}}
	_ = l.Run(ctx)
	rs, _ := f.store.LoadRelay()
	if rs.Offset != 2 {
		t.Fatalf("offset=%d; the second update must not be claimed after cancellation", rs.Offset)
	}
}

func TestStrangerStaleNotCounted(t *testing.T) {
	f := newFixture(t)
	old := f.now.Add(-time.Hour).Unix()
	s := f.msg("spam", 0)
	s.UpdateID, s.Message.Date, s.Message.From.ID = 5, old, 1
	p := &fakePoller{batches: [][]telegram.Update{{s}}}
	l := &Loop{Poll: p, Router: f.r, Store: f.store, Once: true, MaxAge: 10 * time.Minute, Log: func(string, ...any) {}}
	_ = l.Run(context.Background())
	if len(f.api.Sent) != 0 {
		t.Fatalf("a stranger's stale message produced a notice: %+v", f.api.Sent)
	}
}

func TestAgentPathIsWhatThePlistUses(t *testing.T) {
	want := "/Users/x/.local/bin:/opt/homebrew/bin:/home/linuxbrew/.linuxbrew/bin:/usr/local/bin:/usr/bin:/bin"
	if got := AgentPath("/Users/x"); got != want {
		t.Fatalf("AgentPath=%q", got)
	}
	if !strings.Contains(Plist("/Users/x/.local/bin/tg", "/Users/x/.local/state/claude-tg", "/Users/x", nil), "<string>"+want+"</string>") {
		t.Fatal("plist PATH differs from AgentPath")
	}
}
