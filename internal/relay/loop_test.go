package relay

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/ask"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

type fakePoller struct {
	mu      sync.Mutex
	batches [][]telegram.Update
	calls   []int64 // offsets requested
}

// polls is how many times the loop has polled; safe while it runs.
func (p *fakePoller) polls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *fakePoller) GetUpdates(_ context.Context, offset int64, _ int) ([]telegram.Update, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
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
		// An ask tap carries only its message's date, not when it was tapped.
		{UpdateID: 4, CallbackQuery: &telegram.CallbackQuery{Data: "a:0123456789abcdef:0:y", Message: &telegram.Message{Date: now.Add(-20 * time.Minute).Unix()}}},
		// So does a voice confirmation's button and a session pick (R19).
		{UpdateID: 5, CallbackQuery: &telegram.CallbackQuery{Data: "v:0a1b2c3d:y", Message: &telegram.Message{Date: now.Add(-20 * time.Minute).Unix()}}},
		{UpdateID: 6, CallbackQuery: &telegram.CallbackQuery{Data: "s:" + sidShop + ":0a1b2c3d", Message: &telegram.Message{Date: now.Add(-20 * time.Minute).Unix()}}},
		// And a button of /sessions, tapped long after the list was sent.
		{UpdateID: 7, CallbackQuery: &telegram.CallbackQuery{Data: "p:" + sidShop, Message: &telegram.Message{Date: now.Add(-20 * time.Minute).Unix()}}},
	}
	fresh, stale := PartitionStale(ups, now, 10*time.Minute)
	if len(fresh) != 5 || fresh[0].UpdateID != 2 || fresh[1].UpdateID != 4 || fresh[2].UpdateID != 5 || fresh[3].UpdateID != 6 || fresh[4].UpdateID != 7 || len(stale) != 2 {
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
		if s.Text == "skipped 2 message(s) older than 10 min (the relay was down or the Mac slept)" {
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
		for p.polls() < 3 {
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

// runPolls runs the loop until it has polled n times, then stops it.
func runPolls(t *testing.T, l *Loop, p *fakePoller, n int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // also when Run fails: the watcher below must stop
	done := make(chan struct{})
	go func() {
		defer close(done)
		for p.polls() < n && ctx.Err() == nil {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	err := l.Run(ctx)
	cancel()
	<-done
	if err != nil {
		t.Fatal(err)
	}
}

func skippedNotices(f *fixture) int {
	n := 0
	for _, m := range f.api.Sent {
		if strings.Contains(m.Text, "skipped") {
			n++
		}
	}
	return n
}

// Telegram dates a callback by its message: a tap on a 30-minute-old ask
// message is a fresh tap, in steady state too.
func TestRunHonoursOldAskTapInSteadyState(t *testing.T) {
	f := newFixture(t)
	f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	tap := tapUpdate(ask.TapData(askP, 0, 'y', 0), 700)
	tap.UpdateID, tap.CallbackQuery.Message.Date = 2, f.now.Add(-30*time.Minute).Unix()
	p := &fakePoller{batches: [][]telegram.Update{{}, {tap}}} // the empty batch ends the startup drain
	l := &Loop{Poll: p, Router: f.r, Store: f.store, MaxAge: 10 * time.Minute, Log: func(string, ...any) {}}
	runPolls(t, l, p, 3)
	a := f.loadAsk(t, askP)
	if !a.Done || a.Decision == nil || a.Decision.Behavior != "allow" {
		t.Fatalf("tap not honoured: %+v", a)
	}
	if got := f.toasts(); len(got) != 1 || got[0] != "approved" {
		t.Fatalf("toasts = %q", got)
	}
	if n := skippedNotices(f); n != 0 {
		t.Fatalf("the tap was reported as skipped: %+v", f.api.Sent)
	}
}

// A relay that kept running while the Mac slept must not type hours-old
// text after wake: past the drain, old messages are still skipped. An old
// session pick is not skipped (ruling R19): its parked text expired with
// it, so the pick types nothing and says so.
func TestRunSkipsOldMessageInSteadyState(t *testing.T) {
	f := newFixture(t)
	old := f.msg("ship it", 101)
	old.UpdateID, old.Message.Date = 2, f.now.Add(-30*time.Minute).Unix()
	if err := f.store.PutPending("deadbeef", state.Parked{Text: "please rerun", MessageID: 500, Created: f.now.Add(-30 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	pick := tapUpdate("s:"+sidShop+":deadbeef", 1001)
	pick.UpdateID, pick.CallbackQuery.Message.Date = 3, f.now.Add(-30*time.Minute).Unix()
	p := &fakePoller{batches: [][]telegram.Update{{}, {old, pick}}}
	l := &Loop{Poll: p, Router: f.r, Store: f.store, MaxAge: 10 * time.Minute, Log: func(string, ...any) {}}
	runPolls(t, l, p, 3)
	if len(f.term.Typed) != 0 || len(f.api.Reactions) != 0 {
		t.Fatalf("an old update was delivered: typed = %+v reactions = %+v", f.term.Typed, f.api.Reactions)
	}
	if got := f.toasts(); !reflect.DeepEqual(got, []string{"expired, send the text again"}) {
		t.Fatalf("toasts = %q", got)
	}
	if len(f.api.Edits) != 1 || f.api.Edits[0].MessageID != 1001 || f.api.Edits[0].Text != "expired; send the text again" {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	if n := skippedNotices(f); n != 1 || !strings.Contains(f.api.LastText(), "skipped 1 message") {
		t.Fatalf("want one notice for the old message: %+v", f.api.Sent)
	}
}

// While the startup backlog drains, an old message is still skipped, but an
// ask tap is honoured: the ask itself says whether it still waits.
func TestRunHonoursAskTapInStartupDrain(t *testing.T) {
	f := newFixture(t)
	f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	old := f.msg("ship it", 101)
	old.UpdateID, old.Message.Date = 7, f.now.Add(-time.Hour).Unix()
	tap := tapUpdate(ask.TapData(askP, 0, 'n', 0), 700)
	tap.UpdateID, tap.CallbackQuery.Message.Date = 8, f.now.Add(-time.Hour).Unix()
	closed := permAsk(askP2, sidShop, "acme-shop-92", 720)
	closed.Closed = "mac"
	f.putAsk(t, closed)
	late := tapUpdate(ask.TapData(askP2, 0, 'y', 0), 720)
	late.UpdateID, late.CallbackQuery.Message.Date = 9, f.now.Add(-time.Hour).Unix()
	p := &fakePoller{batches: [][]telegram.Update{{old, tap, late}}}
	l := &Loop{Poll: p, Router: f.r, Store: f.store, Once: true, MaxAge: 10 * time.Minute, Log: func(string, ...any) {}}
	if err := l.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.term.Typed) != 0 {
		t.Fatalf("the stale message was typed: %+v", f.term.Typed)
	}
	sameDecision(t, f.loadAsk(t, askP).Decision, state.Decision{Behavior: "deny", Suggestion: -1, Message: ask.NoMessage, Interrupt: true})
	if got := f.toasts(); len(got) != 2 || got[0] != "denied" || got[1] != "already answered at the Mac" {
		t.Fatalf("toasts = %q", got)
	}
	if n := skippedNotices(f); n != 1 || !strings.Contains(f.api.LastText(), "skipped 1 message") {
		t.Fatalf("want one notice for the one stale message: %+v", f.api.Sent)
	}
}
