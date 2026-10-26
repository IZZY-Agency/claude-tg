package state

import (
	"context"
	"encoding/json"
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
	if err := s.PutPending("k1", Parked{Text: "ship it", MessageID: 500, Created: now}); err != nil {
		t.Fatal(err)
	}
	p, ok := s.TakePending("k1", now.Add(time.Minute), 10*time.Minute)
	if !ok || p.Text != "ship it" || p.MessageID != 500 {
		t.Fatalf("take: %+v ok=%v", p, ok)
	}
	if _, ok := s.TakePending("k1", now.Add(time.Minute), 10*time.Minute); ok {
		t.Fatal("second take must fail")
	}
	_ = s.PutPending("k2", Parked{Text: "late", Created: now})
	if _, ok := s.TakePending("k2", now.Add(11*time.Minute), 10*time.Minute); ok {
		t.Fatal("expired text must not be returned")
	}
}

// An expired parked text is still deleted, and handed back with false so
// the caller can clean up after it (the reaction on the owner's note).
func TestTakePendingExpiredReturnsRecord(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 10, 22, 21, 0, 0, 0, time.UTC)
	if err := s.PutPending("k", Parked{Text: "late", MessageID: 500, Created: now}); err != nil {
		t.Fatal(err)
	}
	p, ok := s.TakePending("k", now.Add(11*time.Minute), 10*time.Minute)
	if ok || p.MessageID != 500 || p.Text != "late" {
		t.Fatalf("take expired: %+v ok=%v", p, ok)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "pending", "k.json")); !os.IsNotExist(err) {
		t.Fatalf("expired record still on disk (err %v)", err)
	}
	if p, ok := s.TakePending("k", now, 10*time.Minute); ok || p != (Parked{}) {
		t.Fatalf("take missing: %+v ok=%v", p, ok)
	}
}

// A parked voice transcript keeps its target and its confirmation's header.
func TestPendingKeepsVoiceFields(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 10, 22, 21, 0, 0, 0, time.UTC)
	want := Parked{Text: "please rerun", MessageID: 500, Created: now, Voice: true,
		SessionID: "6449d606-55c1-4e6d-a915-4b14c777f57f", AskID: "fedcba9876543210", AskIndex: 1, Header: "🎙 to <b>acme-shop-92</b>:"}
	if err := s.PutPending("k", want); err != nil {
		t.Fatal(err)
	}
	got, ok := s.TakePending("k", now, 10*time.Minute)
	if !ok || !got.Created.Equal(now) {
		t.Fatalf("take: %+v ok=%v", got, ok)
	}
	got.Created = now
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
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
	_ = s.PutPending("k", Parked{Text: "text", Created: now})
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

// Turning follow on keeps the session's voice marks; turning it off drops
// them: a mark made while followed must not make the answer to a later
// typed reply spoken.
func TestSetFollow(t *testing.T) {
	at := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	s := &Session{SessionID: "s"}
	s.MarkVoice("", "", at)
	s.SetFollow(true)
	if !s.Follow || len(s.VoiceMarks) != 1 {
		t.Fatalf("follow on: %+v", s)
	}
	s.MarkVoice("a1", "", at)
	s.SetFollow(false)
	if s.Follow || len(s.VoiceMarks) != 0 {
		t.Fatalf("follow off: %+v", s)
	}
}

// A voice note delivered to a session marks it (VoiceMarks): the Stop hook
// spends the oldest mark first, and a mark left unspent longer than
// VoiceMarkTTL lapses. An ask taken back (DropAsk) takes its mark with
// its credit. The session keeps its newest 50 marks.
// A mark keeps the language of what the owner said by voice, and the Stop
// hook gets it with the mark it spends: the oldest live one.
func TestVoiceMarkLanguage(t *testing.T) {
	at := time.Date(2026, 10, 26, 15, 0, 0, 0, time.UTC)
	s := &Session{SessionID: "s"}
	s.MarkVoice("", "ru", at)
	s.MarkVoice("a1", "uk", at.Add(time.Minute))
	for _, want := range []string{"ru", "uk"} {
		m, ok := s.TakeVoice(at.Add(2 * time.Minute))
		if !ok || m.Lang != want {
			t.Fatalf("TakeVoice = %+v, %v; want a mark in %q", m, ok, want)
		}
	}
	if m, ok := s.TakeVoice(at.Add(2 * time.Minute)); ok || m != (VoiceMark{}) {
		t.Fatalf("TakeVoice = %+v, %v; want none", m, ok)
	}
}

func TestVoiceMarks(t *testing.T) {
	at := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	s := &Session{SessionID: "s"}
	took := func(now time.Time) bool { _, ok := s.TakeVoice(now); return ok }
	s.MarkVoice("", "", at)
	s.MarkVoice("", "", at.Add(time.Minute))
	if !took(at.Add(2*time.Minute)) || len(s.VoiceMarks) != 1 || !s.VoiceMarks[0].At.Equal(at.Add(time.Minute)) {
		t.Fatalf("the oldest mark is spent first: %+v", s.VoiceMarks)
	}
	if !took(at.Add(2*time.Minute)) || took(at.Add(2*time.Minute)) {
		t.Fatalf("two marks, two answers, then none: %+v", s.VoiceMarks)
	}

	// A mark 61 minutes old has lapsed: it is not spent, and it is gone.
	s.MarkVoice("", "", at)
	if took(at.Add(61*time.Minute)) || len(s.VoiceMarks) != 0 {
		t.Fatalf("a lapsed mark was spent or kept: %+v", s.VoiceMarks)
	}
	// One that lapsed beside a fresh one: the fresh one is spent.
	s.MarkVoice("", "", at)
	s.MarkVoice("", "", at.Add(30*time.Minute))
	if !took(at.Add(61*time.Minute)) || len(s.VoiceMarks) != 0 {
		t.Fatalf("lapsed and fresh: %+v", s.VoiceMarks)
	}

	s.MarkVoice("", "", at)
	s.MarkVoice("a1", "", at)
	s.ClearVoice()
	if len(s.VoiceMarks) != 0 || took(at) {
		t.Fatalf("cleared: %+v", s.VoiceMarks)
	}

	// An ask is marked once, as it is credited once: marking it again (its
	// answer saved after a failed save and a failed take-back) changes
	// nothing, while transcripts each get their own mark.
	s.MarkVoice("a1", "", at)
	s.MarkVoice("a1", "", at.Add(time.Minute))
	s.MarkVoice("", "", at.Add(2*time.Minute))
	s.MarkVoice("", "", at.Add(3*time.Minute))
	if want := []VoiceMark{{At: at, AskID: "a1"}, {At: at.Add(2 * time.Minute)}, {At: at.Add(3 * time.Minute)}}; !sameMarks(s.VoiceMarks, want) {
		t.Fatalf("marked twice: %+v, want %+v", s.VoiceMarks, want)
	}
	s.ClearVoice()

	// DropAsk takes the ask's mark with its credit and still reports the
	// credit only: a followed session has a mark and no credit.
	s.ArmAsk("a1")
	s.MarkVoice("a1", "", at)
	s.MarkVoice("", "", at.Add(time.Minute))
	if !s.DropAsk("a1") || !sameMarks(s.VoiceMarks, []VoiceMark{{At: at.Add(time.Minute)}}) {
		t.Fatalf("drop a1: credits %q marks %+v", s.AskCredits, s.VoiceMarks)
	}
	followed := &Session{Follow: true}
	followed.MarkVoice("a2", "", at)
	if followed.DropAsk("a2") || len(followed.VoiceMarks) != 0 {
		t.Fatalf("followed drop a2: %+v", followed.VoiceMarks)
	}

	// The newest 50 are kept.
	many := &Session{}
	for i := range 51 {
		many.MarkVoice("", "", at.Add(time.Duration(i)*time.Second))
	}
	if len(many.VoiceMarks) != 50 || !many.VoiceMarks[0].At.Equal(at.Add(time.Second)) || !many.VoiceMarks[49].At.Equal(at.Add(50*time.Second)) {
		t.Fatalf("kept %d marks, from %v to %v", len(many.VoiceMarks), many.VoiceMarks[0].At, many.VoiceMarks[len(many.VoiceMarks)-1].At)
	}
}

// Voice marks survive the session file; a session without any leaves the
// key out.
func TestVoiceMarksRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	st := open(t)
	sess := &Session{SessionID: "s"}
	sess.MarkVoice("", "", at)
	sess.MarkVoice("a1", "uk", at.Add(time.Minute))
	b, err := json.Marshal(sess)
	if err != nil {
		t.Fatal(err)
	}
	const key = `"voice_marks":[{"at":"2026-10-25T09:00:00Z"},{"at":"2026-10-25T09:01:00Z","ask_id":"a1","lang":"uk"}]`
	if !strings.Contains(string(b), key) {
		t.Fatalf("json = %s, want %s", b, key)
	}
	if err := st.SaveSession(sess); err != nil {
		t.Fatal(err)
	}
	got, err := st.LoadSession("s")
	if err != nil || !sameMarks(got.VoiceMarks, sess.VoiceMarks) {
		t.Fatalf("loaded %+v err=%v", got, err)
	}
	if b, _ := json.Marshal(&Session{SessionID: "t"}); strings.Contains(string(b), "voice_marks") {
		t.Fatalf("empty session json = %s", b)
	}
}

func sameMarks(got, want []VoiceMark) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if !got[i].At.Equal(want[i].At) || got[i].AskID != want[i].AskID || got[i].Lang != want[i].Lang {
			return false
		}
	}
	return true
}

// The relay records each line it submits (RecordPhoneLine) and a hook asks
// whether a turn's prompt was one of them (SentFromPhone): the same text,
// whitespace aside, written within 2 minutes of the line, before or after.
// An identical line typed at the Mac 3 minutes away is the Mac's. The
// session keeps its newest 10 lines, and they survive the session file.
func TestPhoneLines(t *testing.T) {
	at := time.Date(2026, 10, 25, 17, 16, 31, 0, time.UTC)
	if PhoneSum("  a\n b ") != PhoneSum("a b") {
		t.Fatal("whitespace runs must not change the fingerprint")
	}
	if got, want := PhoneSum("a b"), "c8687a08aa5d6ed2044328fa6a697ab8e96dc34291e8c2034ae8c38e6fcc6d65"; got != want {
		t.Fatalf("PhoneSum = %q, want the lowercase hex SHA-256 %q", got, want)
	}
	if PhoneSum("a b") == PhoneSum("a c") {
		t.Fatal("different lines share a fingerprint")
	}

	s := &Session{SessionID: "s"}
	s.RecordPhoneLine("run the tests", at, false)
	for _, c := range []struct {
		name string
		text string
		at   time.Time
		want bool
	}{
		{"same text, spaces and newlines aside, 30 s later", "  run\nthe   tests\n", at.Add(30 * time.Second), true},
		{"exactly 2 minutes later", "run the tests", at.Add(2 * time.Minute), true},
		{"exactly 2 minutes earlier", "run the tests", at.Add(-2 * time.Minute), true},
		{"the same line typed at the Mac 3 minutes later", "run the tests", at.Add(3 * time.Minute), false},
		{"3 minutes earlier", "run the tests", at.Add(-3 * time.Minute), false},
		{"different text", "run the linter", at, false},
		{"empty text", "", at, false},
	} {
		if got := s.SentFromPhone(c.text, c.at); got != c.want {
			t.Errorf("%s: SentFromPhone(%q) = %v, want %v", c.name, c.text, got, c.want)
		}
	}
	if (&Session{}).SentFromPhone("run the tests", at) {
		t.Error("a session without phone lines matched")
	}

	// The newest 10 are kept: the first of 11 no longer matches.
	many := &Session{SessionID: "m"}
	for i := range 11 {
		many.RecordPhoneLine("line "+strconv.Itoa(i), at.Add(time.Duration(i)*time.Second), false)
	}
	if len(many.PhoneLines) != 10 || many.SentFromPhone("line 0", at) || !many.SentFromPhone("line 1", at) || !many.SentFromPhone("line 10", at) {
		t.Fatalf("kept %d lines: %+v", len(many.PhoneLines), many.PhoneLines)
	}

	// Round trip through the session file; no lines, no key.
	st := open(t)
	if err := st.SaveSession(s); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if key := `"phone_lines":[{"at":"2026-10-25T17:16:31Z","sum":"` + PhoneSum("run the tests") + `"}]`; !strings.Contains(string(b), key) {
		t.Fatalf("json = %s, want %s", b, key)
	}
	got, err := st.LoadSession("s")
	if err != nil || len(got.PhoneLines) != 1 || !got.PhoneLines[0].At.Equal(at) || got.PhoneLines[0].Sum != PhoneSum("run the tests") || !got.SentFromPhone("run the tests", at) {
		t.Fatalf("loaded %+v err=%v", got, err)
	}
	if b, _ := json.Marshal(&Session{SessionID: "t"}); strings.Contains(string(b), "phone_lines") {
		t.Fatalf("empty session json = %s", b)
	}
}

// A line the relay typed into a busy session is marked queued: Claude Code
// takes it in only when the running step ends, so its entry may come up to
// an hour after the record (ruling P15), never more than 2 minutes before.
// A line typed into an idle session keeps the 2-minute window both ways.
func TestQueuedPhoneLines(t *testing.T) {
	at := time.Date(2026, 10, 25, 17, 16, 31, 0, time.UTC)
	s := &Session{SessionID: "s"}
	s.RecordPhoneLine("and then deploy", at, true)
	if len(s.PhoneLines) != 1 || !s.PhoneLines[0].Queued {
		t.Fatalf("lines = %+v, want one queued", s.PhoneLines)
	}
	for _, c := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"taken in 10 minutes later", at.Add(10 * time.Minute), true},
		{"taken in 59 minutes later", at.Add(59 * time.Minute), true},
		{"exactly an hour later", at.Add(time.Hour), true},
		{"61 minutes later", at.Add(61 * time.Minute), false},
		{"2 minutes earlier", at.Add(-2 * time.Minute), true},
		{"3 minutes earlier", at.Add(-3 * time.Minute), false},
	} {
		if got := s.SentFromPhone("and then deploy", c.at); got != c.want {
			t.Errorf("queued, %s: SentFromPhone = %v, want %v", c.name, got, c.want)
		}
	}
	idle := &Session{SessionID: "i"}
	idle.RecordPhoneLine("and then deploy", at, false)
	if idle.PhoneLines[0].Queued || idle.SentFromPhone("and then deploy", at.Add(10*time.Minute)) {
		t.Fatalf("an idle line matched 10 minutes later: %+v", idle.PhoneLines)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"queued":true`) {
		t.Fatalf("json = %s, want the queued mark", b)
	}
	st := open(t)
	if err := st.SaveSession(s); err != nil {
		t.Fatal(err)
	}
	if got, err := st.LoadSession("s"); err != nil || !got.SentFromPhone("and then deploy", at.Add(30*time.Minute)) {
		t.Fatalf("loaded %+v err=%v", got, err)
	}
}

// TakePhoneLine marks the newest line SentFromPhone would match as
// answered, once; SentFromPhone keeps matching it.
// A line's last prompt time is the latest a prompt matching it can be
// written (SentFromPhone): two minutes after a typed line's record, an
// hour after a queued one's.
func TestPhoneLineLastPromptAt(t *testing.T) {
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		queued bool
		want   time.Time
	}{
		{false, at.Add(2 * time.Minute)},
		{true, at.Add(time.Hour)},
	} {
		var s Session
		s.RecordPhoneLine(" Is it raining?", at, tc.queued)
		l := s.PhoneLines[0]
		if got := l.LastPromptAt(); !got.Equal(tc.want) {
			t.Errorf("queued %v: LastPromptAt = %v, want %v", tc.queued, got, tc.want)
		}
		if !s.SentFromPhone(" Is it raining?", tc.want) || s.SentFromPhone(" Is it raining?", tc.want.Add(time.Nanosecond)) {
			t.Errorf("queued %v: SentFromPhone's window does not end at LastPromptAt", tc.queued)
		}
	}
}

func TestTakePhoneLine(t *testing.T) {
	t0 := time.Date(2026, 10, 25, 17, 16, 31, 0, time.UTC)
	s := &Session{SessionID: "s"}
	s.RecordPhoneLine("what is the weather", t0, false)
	if !s.TakePhoneLine("what is the weather", t0.Add(30*time.Second)) {
		t.Fatal("first take = false")
	}
	if s.TakePhoneLine("what is the weather", t0.Add(30*time.Second)) {
		t.Error("second take = true")
	}
	if !s.SentFromPhone("what is the weather", t0.Add(30*time.Second)) {
		t.Error("SentFromPhone false after taking")
	}
	if s.TakePhoneLine("another text", t0) {
		t.Error("another text taken")
	}
	s.RecordPhoneLine("later line", t0, false)
	if s.TakePhoneLine("later line", t0.Add(3*time.Minute)) {
		t.Error("text outside the window taken")
	}
	q := &Session{SessionID: "q"}
	q.RecordPhoneLine("queued line", t0, true)
	if !q.TakePhoneLine("queued line", t0.Add(50*time.Minute)) {
		t.Error("queued line not taken 50 minutes later")
	}
	if !q.SentFromPhone("queued line", t0.Add(50*time.Minute)) {
		t.Error("SentFromPhone false after taking a queued line")
	}
	// Two records of one text: the newest is taken first.
	d := &Session{SessionID: "d"}
	d.RecordPhoneLine("same", t0, false)
	d.RecordPhoneLine("same", t0.Add(time.Minute), false)
	if !d.TakePhoneLine("same", t0.Add(time.Minute)) || !d.PhoneLines[1].Answered || d.PhoneLines[0].Answered {
		t.Errorf("lines = %+v", d.PhoneLines)
	}
}
