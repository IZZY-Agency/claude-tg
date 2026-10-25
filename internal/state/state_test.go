package state

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "claude-tg"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSessionRoundTripAndList(t *testing.T) {
	s := open(t)
	empty, err := s.LoadSession("6449d606-55c1-4e6d-a915-4b14c777f57f")
	if err != nil || empty.SessionID != "6449d606-55c1-4e6d-a915-4b14c777f57f" || empty.Follow {
		t.Fatalf("zero session: %+v err=%v", empty, err)
	}
	empty.Name = "acme-shop-92"
	empty.Cwd = "/Users/alex/code/acme-shop"
	empty.PID = 10250
	empty.AgtermSessionID = "agt-1"
	empty.TmuxPane = "%5"
	empty.TmuxSocket = "/private/tmp/tmux-501/default"
	empty.Follow = true
	empty.LastMessageIDs = []int64{1, 2}
	empty.PendingPermission = &Pending{Tool: "Bash", Summary: "rm -rf node_modules", At: time.Unix(1700000000, 0).UTC()}
	if err := s.SaveSession(empty); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSession(empty.SessionID)
	if err != nil || got.Name != "acme-shop-92" || !got.Follow || got.PendingPermission == nil || got.PendingPermission.Summary != "rm -rf node_modules" || len(got.LastMessageIDs) != 2 || got.TmuxPane != "%5" || got.TmuxSocket != "/private/tmp/tmux-501/default" {
		t.Fatalf("round trip: %+v err=%v", got, err)
	}
	list, err := s.ListSessions()
	if err != nil || len(list) != 1 || list[0].Name != "acme-shop-92" {
		t.Fatalf("list: %+v err=%v", list, err)
	}
}

func TestDedupeWindow(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 10, 22, 15, 0, 0, 0, time.UTC)
	ok := func(key string, at time.Time) bool {
		v, err := s.Dedupe(key, 10*time.Minute, at)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if !ok("sid|needs permission: Bash: x", now) {
		t.Fatal("first send must be allowed")
	}
	if ok("sid|needs permission: Bash: x", now.Add(9*time.Minute)) {
		t.Fatal("repeat inside the window must be suppressed")
	}
	if !ok("sid|needs permission: Bash: y", now.Add(1*time.Minute)) {
		t.Fatal("a different alert must be allowed")
	}
	if !ok("sid|needs permission: Bash: x", now.Add(11*time.Minute)) {
		t.Fatal("after the window the alert must be allowed again")
	}
}

func TestSpoolOrder(t *testing.T) {
	s := open(t)
	base := time.Date(2026, 10, 22, 15, 0, 0, 0, time.UTC)
	for i, txt := range []string{"second", "first", "third"} {
		off := []time.Duration{2 * time.Second, 1 * time.Second, 3 * time.Second}[i]
		if err := s.SpoolPut(SpoolItem{ID: txt, SessionID: "sid", Req: telegram.SendRequest{ChatID: 1, Text: txt}, Created: base.Add(off)}); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.SpoolList()
	if err != nil || len(items) != 3 {
		t.Fatalf("items=%v err=%v", items, err)
	}
	if items[0].Req.Text != "first" || items[1].Req.Text != "second" || items[2].Req.Text != "third" {
		t.Fatalf("order: %s %s %s", items[0].Req.Text, items[1].Req.Text, items[2].Req.Text)
	}
	if err := s.SpoolDelete("first"); err != nil {
		t.Fatal(err)
	}
	items, _ = s.SpoolList()
	if len(items) != 2 {
		t.Fatalf("after delete: %d", len(items))
	}
}

// The pace holds sends minGap apart, measured as it keeps them: between the
// send times recorded at release. (Measured from after the first release
// returned, which comes a moment after its record, the gap read up to that
// moment short: 199.9 ms under -race.)
func TestPaceEnforcesGap(t *testing.T) {
	s := open(t)
	gap := 200 * time.Millisecond
	sent := func() time.Time {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(s.Dir, "send.last"))
		if err != nil {
			t.Fatal(err)
		}
		ns, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return time.Unix(0, ns)
	}
	rel, err := s.Pace(gap)
	if err != nil {
		t.Fatal(err)
	}
	rel()
	first := sent()
	rel2, err := s.Pace(gap)
	if err != nil {
		t.Fatal(err)
	}
	rel2()
	if d := sent().Sub(first); d < gap {
		t.Fatalf("second send %v after the first, want >= %v", d, gap)
	}
}

func TestLogTruncates(t *testing.T) {
	s := open(t)
	line := strings.Repeat("x", 1023) + "\n"
	for i := 0; i < 6*1024; i++ { // ~6 MB
		s.Logf("%s", line)
	}
	fi, err := os.Stat(filepath.Join(s.Dir, "tg.log"))
	if err != nil {
		t.Fatal(err)
	}
	// Rotation trims to the last 1 MB once the file passes 5 MB, and writes
	// continue afterwards: the file must stay under 5 MB plus one line and be
	// well below the ~6 MB written.
	if fi.Size() > logMax+2048 || fi.Size() >= 6*1024*1024 {
		t.Fatalf("log is %d bytes after writing ~6 MB; rotation did not bound it", fi.Size())
	}
}

// Concurrent writers must not lose each other's updates.
func TestUpdateSessionIsReadModifyWriteUnderLock(t *testing.T) {
	s := open(t)
	id := "6449d606-55c1-4e6d-a915-4b14c777f57f"
	_ = s.SaveSession(&Session{SessionID: id, Follow: true})
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			_, _ = s.UpdateSession(id, func(sess *Session) {
				sess.LastMessageIDs = append(sess.LastMessageIDs, int64(i))
			})
		}(i)
	}
	for i := 0; i < 20; i++ {
		<-done
	}
	got, _ := s.LoadSession(id)
	if len(got.LastMessageIDs) != 20 || !got.Follow {
		t.Fatalf("lost updates: ids=%d follow=%v", len(got.LastMessageIDs), got.Follow)
	}
}

func TestSessionEndSurvivesConcurrentResolve(t *testing.T) {
	s := open(t)
	id := "6449d606-55c1-4e6d-a915-4b14c777f57f"
	_ = s.SaveSession(&Session{SessionID: id, Follow: true})
	// A resolver that pauses inside its update cannot clobber a SessionEnd that
	// lands first, because both go through the lock.
	started := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		_, _ = s.UpdateSession(id, func(sess *Session) {
			close(started)
			time.Sleep(150 * time.Millisecond)
			sess.Name = "resolved"
		})
		close(finished)
	}()
	<-started
	_, _ = s.UpdateSession(id, func(sess *Session) { sess.Follow = false })
	<-finished
	got, _ := s.LoadSession(id)
	if got.Follow || got.Name != "resolved" {
		t.Fatalf("got follow=%v name=%q; want follow=false and name=resolved", got.Follow, got.Name)
	}
}

// Dedupe must be atomic across processes and report persistence failures.
func TestDedupeConcurrentAllowsExactlyOne(t *testing.T) {
	s := open(t)
	now := time.Now()
	allowed := make(chan bool, 30)
	for i := 0; i < 30; i++ {
		go func() {
			ok, _ := s.Dedupe("k|same", 10*time.Minute, now)
			allowed <- ok
		}()
	}
	n := 0
	for i := 0; i < 30; i++ {
		if <-allowed {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d goroutines were allowed to send the same alert, want 1", n)
	}
}

func TestDedupeReportsWriteFailure(t *testing.T) {
	s := open(t)
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits; run as a normal user")
	}
	if err := os.Chmod(s.Dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.Dir, 0o700) })
	ok, err := s.Dedupe("k", time.Minute, time.Now())
	if !ok || err == nil {
		t.Fatalf("ok=%v err=%v; want allowed with an explicit error", ok, err)
	}
}

// Lock acquisition must honour the context.
func TestPaceHonoursContext(t *testing.T) {
	s := open(t)
	rel, err := s.PaceCtx(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := s.PaceCtx(ctx, 0); err == nil {
		t.Fatal("second Pace must fail while the lock is held and the context expires")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("Pace did not return promptly after the context expired")
	}
}

// The spool must carry the plaintext fallback.
func TestSpoolItemKeepsPlain(t *testing.T) {
	s := open(t)
	_ = s.SpoolPut(SpoolItem{ID: "a", Req: telegram.SendRequest{ChatID: 1, Text: "<b>x</b>", ParseMode: "HTML"}, Plain: "x", Created: time.Now()})
	items, _ := s.SpoolList()
	if len(items) != 1 || items[0].Plain != "x" {
		t.Fatalf("items=%+v", items)
	}
}

// v2 additions.
func TestFindSessionByMessageID(t *testing.T) {
	s := open(t)
	_ = s.SaveSession(&Session{SessionID: "a", Name: "a-1", LastMessageIDs: []int64{10, 11}})
	_ = s.SaveSession(&Session{SessionID: "b", Name: "b-1", LastMessageIDs: []int64{12}})
	got, ok := s.FindSessionByMessageID(12)
	if !ok || got.Name != "b-1" {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
	if _, ok := s.FindSessionByMessageID(99); ok {
		t.Fatal("unknown id must not match")
	}
}

func TestRelayStateRoundTrip(t *testing.T) {
	s := open(t)
	rs, err := s.LoadRelay()
	if err != nil || rs.Offset != 0 {
		t.Fatalf("fresh: %+v err=%v", rs, err)
	}
	rs.Offset, rs.PID = 123, 4242
	rs.StartedAt = time.Date(2026, 10, 22, 21, 0, 0, 0, time.UTC)
	if err := s.SaveRelay(rs); err != nil {
		t.Fatal(err)
	}
	got, _ := s.LoadRelay()
	if got.Offset != 123 || got.PID != 4242 || !got.StartedAt.Equal(rs.StartedAt) {
		t.Fatalf("got %+v", got)
	}
}

func TestPendingTakeOnceAndExpire(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 10, 22, 21, 0, 0, 0, time.UTC)
	if err := s.PutPending("k1", "ship it", now); err != nil {
		t.Fatal(err)
	}
	text, ok := s.TakePending("k1", now.Add(time.Minute), 10*time.Minute)
	if !ok || text != "ship it" {
		t.Fatalf("take: %q ok=%v", text, ok)
	}
	if _, ok := s.TakePending("k1", now.Add(time.Minute), 10*time.Minute); ok {
		t.Fatal("second take must fail")
	}
	_ = s.PutPending("k2", "late", now)
	if _, ok := s.TakePending("k2", now.Add(11*time.Minute), 10*time.Minute); ok {
		t.Fatal("expired text must not be returned")
	}
}

func TestLockRelayIsExclusive(t *testing.T) {
	s := open(t)
	release, err := s.LockRelay()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.LockRelay(); err == nil {
		t.Fatal("second lock must fail while held")
	}
	release()
	release2, err := s.LockRelay()
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	release2()
}

// A parked text is handed out only once removal succeeded.
func TestTakePendingFailsWhenRemoveFails(t *testing.T) {
	s := open(t)
	now := time.Now()
	_ = s.PutPending("k", "text", now)
	dir := filepath.Join(s.Dir, "pending")
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits; run as a normal user")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, ok := s.TakePending("k", now, time.Minute); ok {
		t.Fatal("text handed out although it could not be removed")
	}
}
