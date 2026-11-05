package relay

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/relay/relaytest"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

var (
	errOtherPoller = &telegram.APIError{Code: 409, Description: "Conflict: terminated by other getUpdates request; make sure that only one bot instance is running"}
	errWebhook     = &telegram.APIError{Code: 409, Description: "Conflict: can't use getUpdates method while webhook is active; use deleteWebhook to delete the webhook first"}
)

// pollStep is one scripted poll: the fixture's clock moves on by after,
// then the poll returns err (or no updates), then check runs, if set,
// once the loop has handled the poll before (at the next poll).
type pollStep struct {
	after time.Duration
	err   error
	check func()
}

// scriptPoller plays steps in order on the loop's goroutine, then cancels
// the run. Each step's check runs at the next poll, after the loop has
// acted on the step's result.
type scriptPoller struct {
	f      *fixture
	steps  []pollStep
	n      int
	cancel context.CancelFunc
}

func (p *scriptPoller) GetUpdates(context.Context, int64, int) ([]telegram.Update, error) {
	if p.n > 0 && p.steps[p.n-1].check != nil {
		p.steps[p.n-1].check()
	}
	if p.n == len(p.steps) {
		p.cancel()
		return nil, nil
	}
	s := p.steps[p.n]
	p.n++
	p.f.now = p.f.now.Add(s.after)
	return nil, s.err
}

// runScript runs a loop over steps and returns what it logged.
func runScript(t *testing.T, f *fixture, steps []pollStep) []string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logged []string
	l := &Loop{
		Poll: &scriptPoller{f: f, steps: steps, cancel: cancel}, Router: f.r, Store: f.store,
		MaxAge: 10 * time.Minute, backoff: time.Microsecond,
		Log: func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) },
	}
	if err := l.Run(ctx); err != nil {
		t.Fatal(err)
	}
	return logged
}

// conflicts is n conflicting polls every gap.
func conflicts(n int, gap time.Duration) []pollStep {
	var s []pollStep
	for range n {
		s = append(s, pollStep{after: gap, err: errOtherPoller})
	}
	return s
}

// quiet is n successful polls every gap.
func quiet(n int, gap time.Duration) []pollStep {
	var s []pollStep
	for range n {
		s = append(s, pollStep{after: gap})
	}
	return s
}

func sentWith(f *fixture, sub string) int {
	n := 0
	for _, m := range f.api.Sent {
		if strings.Contains(m.Text, sub) {
			n++
		}
	}
	return n
}

func conflictSince(t *testing.T, f *fixture) time.Time {
	t.Helper()
	rs, err := f.store.LoadRelay()
	if err != nil {
		t.Fatal(err)
	}
	return rs.PollConflictSince
}

func alerted(t *testing.T, f *fixture) bool {
	t.Helper()
	rs, err := f.store.LoadRelay()
	if err != nil {
		t.Fatal(err)
	}
	return rs.PollConflictAlerted
}

const (
	alertText = "Another program is reading this bot"
	clearText = "stopped reading this bot"
)

func TestPollConflictAlertsOnceThenClears(t *testing.T) {
	f := newFixture(t)
	start := f.now
	var steps []pollStep
	steps = append(steps, conflicts(5, 8*time.Second)...) // 40 s: too short to alert
	steps[len(steps)-1].check = func() {
		if n := sentWith(f, alertText); n != 0 {
			t.Errorf("alerted after 40 s of conflict: %d", n)
		}
	}
	steps = append(steps, quiet(1, 8*time.Second)...) // a poll that wins in between keeps the episode
	steps = append(steps, conflicts(5, 8*time.Second)...)
	steps[len(steps)-1].check = func() {
		if n := sentWith(f, alertText); n != 1 {
			t.Errorf("after 88 s of conflict: %d alerts, want 1; sent=%+v", n, f.api.Sent)
		}
		if got, want := conflictSince(t, f), start.Add(8*time.Second); !got.Equal(want) {
			t.Errorf("relay.json poll_conflict_since=%v, want %v (the first conflict)", got, want)
		}
	}
	steps = append(steps, conflicts(40, 8*time.Second)...) // it goes on: no second alert
	steps = append(steps, quiet(2, 50*time.Second)...)     // 100 s quiet: not over yet
	steps[len(steps)-1].check = func() {
		if n := sentWith(f, clearText); n != 0 {
			t.Errorf("all-clear after only 100 s without a conflict")
		}
	}
	steps = append(steps, quiet(2, 50*time.Second)...) // past 2 min quiet: over
	logged := runScript(t, f, steps)

	if n := sentWith(f, alertText); n != 1 {
		t.Errorf("%d alerts, want 1", n)
	}
	if n := sentWith(f, clearText); n != 1 {
		t.Errorf("%d all-clears, want 1; sent=%+v", n, f.api.Sent)
	}
	if got := conflictSince(t, f); !got.IsZero() {
		t.Errorf("poll_conflict_since=%v after the conflict ended, want zero", got)
	}
	for _, m := range f.api.Sent {
		if strings.Contains(m.Text, alertText) && !strings.Contains(m.Text, "telegram@claude-plugins-official") {
			t.Errorf("alert does not name the usual culprit: %q", m.Text)
		}
	}
	// 50 identical errors: logged once when they start, once with the count when they stop.
	n409 := 0
	for _, line := range logged {
		if strings.Contains(line, "terminated by other getUpdates request") {
			n409++
		}
	}
	if n409 > 4 {
		t.Errorf("logged the repeating 409 %d times; want it collapsed:\n%s", n409, strings.Join(logged, "\n"))
	}
}

func TestPollConflictShortBlipStaysQuiet(t *testing.T) {
	f := newFixture(t)
	steps := conflicts(4, 8*time.Second) // 24 s, e.g. tg setup polling once
	steps = append(steps, quiet(4, 50*time.Second)...)
	steps = append(steps, conflicts(4, 8*time.Second)...) // a new blip later starts its own clock
	steps = append(steps, quiet(4, 50*time.Second)...)
	runScript(t, f, steps)
	if len(f.api.Sent) != 0 {
		t.Fatalf("sent %+v for two short blips", f.api.Sent)
	}
	if got := conflictSince(t, f); !got.IsZero() {
		t.Errorf("poll_conflict_since=%v, want zero", got)
	}
}

func TestWebhookConflictIsNotAPollConflict(t *testing.T) {
	f := newFixture(t)
	steps := []pollStep{}
	for range 20 {
		steps = append(steps, pollStep{after: 8 * time.Second, err: errWebhook})
	}
	runScript(t, f, steps)
	if n := sentWith(f, alertText); n != 0 {
		t.Fatalf("a webhook 409 raised the other-poller alert: %+v", f.api.Sent)
	}
}

func TestPollConflictCarriedAcrossRestart(t *testing.T) {
	f := newFixture(t)
	since := f.now.Add(-3 * time.Hour)
	if err := f.store.SaveRelay(state.RelayState{Offset: 7, PollConflictSince: since, PollConflictAlerted: true}); err != nil {
		t.Fatal(err)
	}
	// The conflict is still on after the restart: no second alert, the record stays.
	steps := conflicts(20, 8*time.Second)
	steps[len(steps)-1].check = func() {
		if n := sentWith(f, alertText); n != 0 {
			t.Errorf("re-alerted a conflict the last run alerted: %+v", f.api.Sent)
		}
		if got := conflictSince(t, f); !got.Equal(since) {
			t.Errorf("poll_conflict_since=%v, want the earlier run's %v", got, since)
		}
	}
	// Then it ends: the all-clear the user is waiting for comes.
	steps = append(steps, quiet(4, 50*time.Second)...)
	runScript(t, f, steps)
	if n := sentWith(f, clearText); n != 1 {
		t.Fatalf("%d all-clears, want 1; sent=%+v", n, f.api.Sent)
	}
	if got := conflictSince(t, f); !got.IsZero() {
		t.Errorf("poll_conflict_since=%v, want zero", got)
	}
}

func TestPollConflictEndsWhileRelayWasDown(t *testing.T) {
	f := newFixture(t)
	if err := f.store.SaveRelay(state.RelayState{PollConflictSince: f.now.Add(-time.Hour), PollConflictAlerted: true}); err != nil {
		t.Fatal(err)
	}
	runScript(t, f, quiet(4, 50*time.Second)) // no conflict at all after the restart
	if n := sentWith(f, clearText); n != 1 {
		t.Fatalf("%d all-clears, want 1; sent=%+v", n, f.api.Sent)
	}
}

func TestRepeatingPollErrorLoggedWithCount(t *testing.T) {
	f := newFixture(t)
	net := &telegram.NetError{Err: fmt.Errorf("post: no route to host")}
	var steps []pollStep
	for range 200 { // 200 × 6 s = 20 min of the same error
		steps = append(steps, pollStep{after: 6 * time.Second, err: net})
	}
	steps = append(steps, quiet(1, time.Second)...)
	logged := runScript(t, f, steps)
	var lines []string
	for _, l := range logged {
		if strings.Contains(l, "no route to host") {
			lines = append(lines, l)
		}
	}
	// First occurrence, a count every 10 min (2), a final count when it stops.
	if len(lines) < 2 || len(lines) > 4 {
		t.Fatalf("logged %d lines for 200 identical errors:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	total := 1
	count := regexp.MustCompile(`\((\d+) more time\(s\) since`)
	for _, l := range lines[1:] {
		m := count.FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("no count in %q", l)
		}
		n, _ := strconv.Atoi(m[1])
		total += n
	}
	if total != 200 {
		t.Errorf("the counts add up to %d, want 200:\n%s", total, strings.Join(lines, "\n"))
	}
}

func TestStatusShowsPollConflict(t *testing.T) {
	f := newFixture(t)
	if err := f.store.SaveRelay(state.RelayState{PollConflictSince: f.now.Add(-10 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := f.r.status(context.Background(), f.msg("/status", 0).Message); err != nil {
		t.Fatal(err)
	}
	if len(f.api.Sent) != 1 || !strings.Contains(f.api.Sent[0].Text, "another program") {
		t.Fatalf("status=%+v, want it to mention the conflict", f.api.Sent)
	}
}

func TestPollConflictAlertRetriedWhenSendFails(t *testing.T) {
	f := newFixture(t)
	steps := conflicts(9, 8*time.Second) // alert due at the 9th (64 s)
	steps[0].check = func() { f.api.SendErr = &telegram.NetError{Err: fmt.Errorf("post: no route to host")} }
	steps[len(steps)-1].check = func() {
		if alerted(t, f) {
			t.Errorf("recorded the alert as delivered although it failed")
		}
		if conflictSince(t, f).IsZero() {
			t.Errorf("the confirmed conflict is not recorded for the status lines")
		}
		f.api.SendErr = nil
	}
	steps = append(steps, conflicts(3, 8*time.Second)...) // 24 s: inside the retry gap
	steps[len(steps)-1].check = func() {
		if n := sentWith(f, alertText); n != 0 {
			t.Errorf("retried within the gap: %d", n)
		}
	}
	steps = append(steps, conflicts(3, 8*time.Second)...) // past it: retried, delivered
	logged := runScript(t, f, steps)
	if n := sentWith(f, alertText); n != 1 {
		t.Fatalf("%d alerts delivered, want 1; sent=%+v", n, f.api.Sent)
	}
	if !alerted(t, f) {
		t.Errorf("the delivered alert was not recorded")
	}
	told := 0
	for _, l := range logged {
		if strings.Contains(l, "told the user") {
			told++
		}
	}
	if told != 1 {
		t.Errorf("logged %q %d times, want once (after the delivery):\n%s", "told the user", told, strings.Join(logged, "\n"))
	}
}

func TestPollConflictAllClearRetriedWhenSendFails(t *testing.T) {
	f := newFixture(t)
	if err := f.store.SaveRelay(state.RelayState{PollConflictSince: f.now.Add(-time.Hour), PollConflictAlerted: true}); err != nil {
		t.Fatal(err)
	}
	steps := quiet(3, 50*time.Second) // the 3rd (150 s) is due the all-clear
	steps[0].check = func() { f.api.SendErr = &telegram.APIError{Code: 429, Description: "Too Many Requests", RetryAfter: 3} }
	steps[len(steps)-1].check = func() {
		if got := conflictSince(t, f); got.IsZero() {
			t.Errorf("cleared the record although the all-clear failed")
		}
		f.api.SendErr = nil
	}
	steps = append(steps, quiet(1, 50*time.Second)...)
	runScript(t, f, steps)
	if n := sentWith(f, clearText); n != 1 {
		t.Fatalf("%d all-clears delivered, want 1; sent=%+v", n, f.api.Sent)
	}
	if got := conflictSince(t, f); !got.IsZero() {
		t.Errorf("poll_conflict_since=%v after the all-clear, want zero", got)
	}
}

func TestPollConflictClearSavedAfterSaveFails(t *testing.T) {
	f := newFixture(t)
	if err := f.store.SaveRelay(state.RelayState{PollConflictSince: f.now.Add(-time.Hour), PollConflictAlerted: true}); err != nil {
		t.Fatal(err)
	}
	dir := f.store.Dir
	steps := quiet(2, 50*time.Second)
	// Before the 3rd poll (the all-clear) the state dir refuses writes.
	steps[len(steps)-1].check = func() {
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
	}
	steps = append(steps, quiet(1, 50*time.Second)...)
	steps[len(steps)-1].check = func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	steps = append(steps, quiet(1, 5*time.Second)...) // an empty poll: the save is retried anyway
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	runScript(t, f, steps)
	if n := sentWith(f, clearText); n != 1 {
		t.Fatalf("%d all-clears, want 1; sent=%+v", n, f.api.Sent)
	}
	rs, err := f.store.LoadRelay()
	if err != nil {
		t.Fatal(err)
	}
	if !rs.PollConflictSince.IsZero() || rs.PollConflictAlerted {
		t.Fatalf("relay.json still says conflict (%v, alerted %v) after the save could be retried", rs.PollConflictSince, rs.PollConflictAlerted)
	}
}

func TestUnalertedConflictEndsSilently(t *testing.T) {
	f := newFixture(t)
	f.api.SendErr = &telegram.NetError{Err: fmt.Errorf("post: no route to host")}
	steps := conflicts(12, 8*time.Second) // confirmed, the alert never gets through
	steps[len(steps)-1].check = func() { f.api.SendErr = nil }
	steps = append(steps, quiet(4, 50*time.Second)...)
	runScript(t, f, steps)
	if len(f.api.Sent) != 0 {
		t.Fatalf("an all-clear for an alert the user never got: %+v", f.api.Sent)
	}
	if !conflictSince(t, f).IsZero() || alerted(t, f) {
		t.Fatalf("the record outlived the conflict")
	}
}

func countLogged(logged []string, sub string) int {
	n := 0
	for _, l := range logged {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func TestPollConflictAllClearHonoursRetryAfter(t *testing.T) {
	f := newFixture(t)
	if err := f.store.SaveRelay(state.RelayState{PollConflictSince: f.now.Add(-time.Hour), PollConflictAlerted: true}); err != nil {
		t.Fatal(err)
	}
	f.api.SendErr = &telegram.APIError{Code: 429, Description: "Too Many Requests", RetryAfter: 300}
	steps := quiet(3, 50*time.Second)                   // the 3rd (150 s) tries the all-clear: 429, retry after 300 s
	steps = append(steps, quiet(100, 2*time.Second)...) // updates keep polls quick: 200 s, all inside retry_after
	steps[len(steps)-1].check = func() { f.api.SendErr = nil }
	steps = append(steps, quiet(60, 2*time.Second)...) // past it: delivered
	logged := runScript(t, f, steps)
	if n := countLogged(logged, "poll conflict all-clear:"); n != 1 {
		t.Errorf("%d failed all-clear attempts within retry_after, want 1:\n%s", n, strings.Join(logged, "\n"))
	}
	if n := sentWith(f, clearText); n != 1 {
		t.Fatalf("%d all-clears delivered, want 1", n)
	}
}

func TestPollConflictAlertBacksOff(t *testing.T) {
	f := newFixture(t)
	f.api.SendErr = &telegram.NetError{Err: fmt.Errorf("post: no route to host")}
	logged := runScript(t, f, conflicts(150, 8*time.Second)) // 20 min of conflict, every alert fails
	// Due at 1 min, then 30 s, 1, 2, 4, 8 min: 6 tries by 20 min, never more than one per 10 min after that.
	if n := countLogged(logged, "poll conflict alert:"); n < 4 || n > 7 {
		t.Fatalf("%d failed alert attempts in 20 min, want a backoff (4-7):\n%s", n, strings.Join(logged, "\n"))
	}
}

// slowSendAPI's SendMessage takes cost on the fixture's clock and fails
// with err while it is set.
type slowSendAPI struct {
	*relaytest.API
	f    *fixture
	cost time.Duration
	err  error
}

func (a *slowSendAPI) SendMessage(ctx context.Context, r telegram.SendRequest) (int64, error) {
	a.f.now = a.f.now.Add(a.cost)
	if a.err != nil {
		return 0, a.err
	}
	return a.API.SendMessage(ctx, r)
}

func TestRetryAfterCountsFromTheAnswer(t *testing.T) {
	f := newFixture(t)
	if err := f.store.SaveRelay(state.RelayState{PollConflictSince: f.now.Add(-time.Hour), PollConflictAlerted: true}); err != nil {
		t.Fatal(err)
	}
	api := &slowSendAPI{API: f.api, f: f, cost: 9 * time.Second, err: &telegram.APIError{Code: 429, Description: "Too Many Requests", RetryAfter: 30}}
	f.r.API = api
	steps := quiet(3, 50*time.Second)                  // all-clear tried at 150 s; the 429 comes back at 159 s
	steps = append(steps, quiet(1, 25*time.Second)...) // 184 s: 34 s after the try, 25 s after the answer
	steps[len(steps)-1].check = func() { api.err = nil }
	logged := runScript(t, f, steps)
	if n := countLogged(logged, "poll conflict all-clear:"); n != 1 || sentWith(f, clearText) != 0 {
		t.Fatalf("tried again 25 s after a retry_after of 30 s (failures logged %d, delivered %d):\n%s", n, sentWith(f, clearText), strings.Join(logged, "\n"))
	}
}
