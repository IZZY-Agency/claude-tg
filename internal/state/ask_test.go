package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func newAsk(id, session string) *Ask {
	return &Ask{ID: id, SessionID: session, SessionName: "acme-shop-92", Tool: "Bash", Summary: "ls",
		ToolInput: json.RawMessage(`{"command":"ls"}`), Suggestions: json.RawMessage(`[]`),
		HookPID: 42, Created: time.Now().UTC().Truncate(time.Second), Deadline: time.Now().Add(time.Hour).UTC().Truncate(time.Second)}
}

func TestAskRoundTrip(t *testing.T) {
	s := open(t)
	a := newAsk(NewAskID(), "sess-1")
	a.MessageIDs = []int64{7}
	a.Texts = []string{"<b>hi</b>"}
	a.Ticked = [][]bool{{true, false}}
	a.Answers = []json.RawMessage{nil}
	a.Decision = &Decision{Behavior: "deny", Suggestion: -1, Message: "no", Interrupt: true}
	if err := s.PutAsk(a); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadAsk(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	// MarshalIndent re-indents raw JSON; compare it compacted.
	for _, r := range []*json.RawMessage{&got.ToolInput, &got.Suggestions} {
		var buf bytes.Buffer
		if err := json.Compact(&buf, *r); err != nil {
			t.Fatal(err)
		}
		*r = buf.Bytes()
	}
	if !reflect.DeepEqual(got, a) {
		t.Fatalf("got %+v want %+v", got, a)
	}
	if len(NewAskID()) != 16 {
		t.Fatal("id length")
	}
	if err := s.DeleteAsk(a.ID); err != nil || s.DeleteAsk(a.ID) != nil {
		t.Fatal("delete should be idempotent")
	}
}

func TestAskFileMode0600(t *testing.T) {
	s := open(t)
	a := newAsk(NewAskID(), "s")
	if err := s.PutAsk(a); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.askPath(a.ID))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v err %v", info, err)
	}
}

func TestUpdateAskConcurrent(t *testing.T) {
	s := open(t)
	a := newAsk(NewAskID(), "s")
	if err := s.PutAsk(a); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.UpdateAsk(a.ID, func(a *Ask) error {
				a.MessageIDs = append(a.MessageIDs, int64(i))
				return nil
			}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	got, _ := s.LoadAsk(a.ID)
	if len(got.MessageIDs) != 20 {
		t.Fatalf("got %d entries", len(got.MessageIDs))
	}
}

func TestUpdateAskFnErrorWritesNothing(t *testing.T) {
	s := open(t)
	a := newAsk(NewAskID(), "s")
	_ = s.PutAsk(a)
	boom := errors.New("boom")
	_, err := s.UpdateAsk(a.ID, func(a *Ask) error { a.Done = true; return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	got, _ := s.LoadAsk(a.ID)
	if got.Done {
		t.Fatal("fn error must not write")
	}
}

func TestLoadAskMissing(t *testing.T) {
	s := open(t)
	if _, err := s.LoadAsk("nope"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err %v", err)
	}
	if _, err := s.UpdateAsk("nope", func(*Ask) error { return nil }); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("update err %v", err)
	}
}

func TestFindAskByMessageID(t *testing.T) {
	s := open(t)
	a := newAsk(NewAskID(), "s")
	a.MessageIDs = []int64{100, 101}
	_ = s.PutAsk(a)
	_ = s.PutAsk(newAsk(NewAskID(), "other"))
	got, idx, ok := s.FindAskByMessageID(101)
	if !ok || idx != 1 || got.ID != a.ID {
		t.Fatalf("got %v %d %v", got, idx, ok)
	}
	if _, _, ok := s.FindAskByMessageID(999); ok {
		t.Fatal("unexpected hit")
	}
}

func TestOpenAskForSessionSkipsDoneAndClosed(t *testing.T) {
	s := open(t)
	now := time.Now()
	mk := func(id string, age time.Duration, f func(*Ask)) {
		a := newAsk(id, "s")
		a.Created = now.Add(-age)
		f(a)
		_ = s.PutAsk(a)
	}
	mk("aaaa", 3*time.Minute, func(a *Ask) {})
	mk("bbbb", 2*time.Minute, func(a *Ask) {})
	mk("cccc", 1*time.Minute, func(a *Ask) { a.Done = true })
	mk("dddd", 30*time.Second, func(a *Ask) { a.Closed = "mac" })
	_ = s.PutAsk(newAsk("eeee", "other"))
	got, ok := s.OpenAskForSession("s")
	if !ok || got.ID != "bbbb" {
		t.Fatalf("got %v %v", got, ok)
	}
	if _, ok := s.OpenAskForSession("none"); ok {
		t.Fatal("unexpected")
	}
}

func TestDeleteAskWaitsForUpdateAndStaysDeleted(t *testing.T) {
	s := open(t)
	a := newAsk(NewAskID(), "s")
	_ = s.PutAsk(a)
	inFn, release := make(chan struct{}), make(chan struct{})
	updated := make(chan error, 1)
	go func() {
		_, err := s.UpdateAsk(a.ID, func(a *Ask) error {
			close(inFn)
			<-release
			a.Done = true
			return nil
		})
		updated <- err
	}()
	<-inFn
	deleted := make(chan error, 1)
	go func() { deleted <- s.DeleteAsk(a.ID) }()
	select {
	case <-deleted:
		t.Fatal("DeleteAsk returned while an update held the lock")
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.askPath(a.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ask file should be gone: %v", err)
	}
	if _, err := s.UpdateAsk(a.ID, func(*Ask) error { return nil }); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("update after delete: %v", err)
	}
	if _, err := os.Stat(s.askPath(a.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("update recreated the ask")
	}
}

func TestPutAskRejectsEmptyID(t *testing.T) {
	if err := open(t).PutAsk(&Ask{}); err == nil {
		t.Fatal("want error")
	}
}

func TestPruneAskLocks(t *testing.T) {
	s := open(t)
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	touch := func(path string, at time.Time) {
		t.Helper()
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	live := newAsk(NewAskID(), "s") // an ask that still exists keeps its lock, however old
	if err := s.PutAsk(live); err != nil {
		t.Fatal(err)
	}
	liveLock := s.askPath(live.ID) + ".lock"
	if err := os.Chtimes(liveLock, old, old); err != nil {
		t.Fatal(err)
	}
	orphan := s.askPath("0000000000000001") + ".lock" // deleted ask, old lock: removed
	touch(orphan, old)
	fresh := s.askPath("0000000000000002") + ".lock" // deleted ask, recent lock: kept
	touch(fresh, now.Add(-30*time.Minute))
	held := s.askPath("0000000000000003") + ".lock" // old but someone holds it: kept
	touch(held, old)
	release, err := lockFile(context.Background(), held)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	other := filepath.Join(s.asksDir(), "notes.lock") // not an ask lock: kept
	touch(other, old)

	n, err := s.PruneAskLocks(time.Hour, now)
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, err %v; want 1", n, err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan lock still there: %v", err)
	}
	for _, p := range []string{liveLock, s.askPath(live.ID), fresh, held, other} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s was removed: %v", filepath.Base(p), err)
		}
	}
}

func readLog(t *testing.T, s *Store) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.Dir, "tg.log"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

// A corrupt ask file is skipped and logged; the readable asks still come back.
func TestListAsksLogsCorruptFiles(t *testing.T) {
	s := open(t)
	good := newAsk(NewAskID(), "s")
	if err := s.PutAsk(good); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"badjson.json": "{", "noid.json": `{"session_id":"s"}`} {
		if err := os.WriteFile(filepath.Join(s.Dir, "asks", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	asks, err := s.ListAsks()
	if err != nil || len(asks) != 1 || asks[0].ID != good.ID {
		t.Fatalf("asks=%+v err=%v", asks, err)
	}
	log := readLog(t, s)
	for _, name := range []string{"badjson.json", "noid.json"} {
		if !bytes.Contains([]byte(log), []byte(name)) {
			t.Fatalf("%s not logged: %q", name, log)
		}
	}
}

// Find and OpenAskForSession keep their signatures but log a listing error.
func TestAskLookupsLogListErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0 directory")
	}
	s := open(t)
	dir := filepath.Join(s.Dir, "asks")
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, _, ok := s.FindAskByMessageID(7); ok {
		t.Fatal("found an ask in an unreadable directory")
	}
	if _, ok := s.OpenAskForSession("s"); ok {
		t.Fatal("found an open ask in an unreadable directory")
	}
	log := readLog(t, s)
	if c := bytes.Count([]byte(log), []byte("list asks")); c != 2 {
		t.Fatalf("want two logged list errors: %q", log)
	}
}

// The relay removes ask files nobody can read once they are old: a crash
// can leave one, and no hook waits on it.
func TestPruneCorruptAsks(t *testing.T) {
	s := open(t)
	good := newAsk(NewAskID(), "s")
	if err := s.PutAsk(good); err != nil {
		t.Fatal(err)
	}
	asks := filepath.Join(s.Dir, "asks")
	old := time.Now().Add(-2 * time.Hour)
	write := func(name, body string, mtime time.Time) string {
		p := filepath.Join(asks, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return p
	}
	oldBad := write("oldbad.json", "{", old)
	newBad := write("newbad.json", "{", time.Now())
	heldBad := write("heldbad.json", "{", old)
	if err := os.Chtimes(filepath.Join(asks, good.ID+".json"), old, old); err != nil {
		t.Fatal(err)
	}
	release, err := lockFile(context.Background(), heldBad+".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	n, err := s.PruneCorruptAsks(time.Hour, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if _, err := os.Stat(oldBad); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old corrupt file kept: %v", err)
	}
	for _, p := range []string{newBad, heldBad, filepath.Join(asks, good.ID+".json")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s removed: %v", filepath.Base(p), err)
		}
	}
	if !bytes.Contains([]byte(readLog(t, s)), []byte("oldbad.json")) {
		t.Fatal("removal not logged")
	}
}

// A stuck ask lock fails an ask operation after the timeout instead of
// blocking it for ever.
func TestAskLockTimesOut(t *testing.T) {
	s := open(t)
	a := newAsk(NewAskID(), "s")
	if err := s.PutAsk(a); err != nil {
		t.Fatal(err)
	}
	defer func(d time.Duration) { askLockTimeout = d }(askLockTimeout)
	if askLockTimeout != 10*time.Second {
		t.Fatalf("askLockTimeout = %v, want 10s", askLockTimeout)
	}
	askLockTimeout = 50 * time.Millisecond
	release, err := lockFile(context.Background(), s.askPath(a.ID)+".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for name, op := range map[string]func() error{
		"put":    func() error { return s.PutAsk(a) },
		"update": func() error { _, err := s.UpdateAsk(a.ID, func(*Ask) error { return nil }); return err },
		"delete": func() error { return s.DeleteAsk(a.ID) },
	} {
		if err := op(); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s: err=%v, want a timeout", name, err)
		}
	}
}

// Collected marks an answer its hook has taken for Claude Code; an ask
// without it stores no such field.
func TestAskCollected(t *testing.T) {
	s := open(t)
	a := newAsk(NewAskID(), "s")
	if err := s.PutAsk(a); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Dir, "asks", a.ID+".json")
	if b, _ := os.ReadFile(path); bytes.Contains(b, []byte(`"collected"`)) {
		t.Fatalf("uncollected ask stores the field: %s", b)
	}
	if _, err := s.UpdateAsk(a.ID, func(a *Ask) error { a.Collected = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !bytes.Contains(b, []byte(`"collected": true`)) {
		t.Fatalf("file: %s", b)
	}
	if got, err := s.LoadAsk(a.ID); err != nil || !got.Collected {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// The relay lists the asks on every message and sweep: a corrupt file is
// logged once per process, not on every listing.
func TestListAsksLogsEachCorruptFileOnce(t *testing.T) {
	s := open(t)
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(s.Dir, "asks", name), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("first.json")
	for range 3 {
		if _, err := s.ListAsks(); err != nil {
			t.Fatal(err)
		}
	}
	same := &Store{Dir: s.Dir} // another handle on the same directory
	if _, _, ok := same.FindAskByMessageID(7); ok {
		t.Fatal("found an ask")
	}
	write("second.json")
	if _, err := s.ListAsks(); err != nil {
		t.Fatal(err)
	}
	log := []byte(readLog(t, s))
	if c := bytes.Count(log, []byte("first.json")); c != 1 {
		t.Fatalf("first.json logged %d times: %s", c, log)
	}
	if c := bytes.Count(log, []byte("second.json")); c != 1 {
		t.Fatalf("second.json logged %d times: %s", c, log)
	}
}

// ClaimSignature: one dialog, one ask. An unclosed ask of the same session
// holding the signature, or an older open ask of it still looking for its
// dialog, keeps the claim from holding.
func TestClaimSignature(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 10, 24, 20, 0, 0, 0, time.UTC)
	put := func(session string, ago time.Duration) *Ask {
		t.Helper()
		a := newAsk(NewAskID(), session)
		a.Created = now.Add(-ago)
		if err := s.PutAsk(a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	claim := func(a *Ask, sig string) bool {
		t.Helper()
		ok, err := s.ClaimSignature(a.ID, sig, 3500*time.Millisecond, now)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	older, newer := put("s", 2*time.Second), put("s", time.Second)
	if claim(newer, "SIG") {
		t.Fatal("claimed while an older ask still looks for its dialog")
	}
	if !claim(older, "SIG") {
		t.Fatal("the older ask could not claim")
	}
	if got, _ := s.LoadAsk(older.ID); got.Signature != Fingerprint("SIG") {
		t.Fatalf("claim not stored: %+v", got)
	}
	if claim(newer, "SIG") {
		t.Fatal("claimed a signature another ask holds")
	}
	if !claim(newer, "OTHER") {
		t.Fatal("could not claim another dialog")
	}
	// Another session's asks, closed asks and a long-gone looker do not count.
	if !claim(put("t", 0), "SIG") {
		t.Fatal("another session's claim blocked")
	}
	if _, err := s.UpdateAsk(older.ID, func(a *Ask) error { a.Closed = "mac"; return nil }); err != nil {
		t.Fatal(err)
	}
	stale := put("s", time.Minute) // its hook died looking
	_ = stale
	if !claim(put("s", 0), "SIG") {
		t.Fatal("a closed holder or a stale looker blocked the claim")
	}
	// An answered ask not yet collected still holds its dialog.
	held := put("u", 5*time.Second)
	if !claim(held, "X") {
		t.Fatal("claim")
	}
	if _, err := s.UpdateAsk(held.ID, func(a *Ask) error { a.Done = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if claim(put("u", 0), "X") {
		t.Fatal("claimed the dialog of an answered, uncollected ask")
	}
	// Once its hook collected the answer, its dialog is gone: a later call's
	// alike dialog is claimed, even while the ask waits for the relay's edit.
	if _, err := s.UpdateAsk(held.ID, func(a *Ask) error { a.Collected = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !claim(put("u", 0), "X") {
		t.Fatal("a collected ask still held its dialog")
	}
}

// The phone's answer is done with once both sides are: the hook collected
// it (CollectAsk) and the relay settled its edit (Settled). Whoever comes
// second removes the ask, in either order. An UpdateAsk fn removes the ask
// with ErrRemoveAsk.
func TestCollectAskRemovesOnlyASettledAsk(t *testing.T) {
	s := open(t)
	a := newAsk(NewAskID(), "s")
	a.Done = true
	if err := s.PutAsk(a); err != nil {
		t.Fatal(err)
	}
	if err := s.CollectAsk(a.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadAsk(a.ID)
	if err != nil || !got.Collected {
		t.Fatalf("hook first: ask %+v, %v; want it kept, collected, for the relay's edit", got, err)
	}
	if _, err := s.UpdateAsk(a.ID, func(a *Ask) error {
		a.Settled = true
		if a.Collected {
			return ErrRemoveAsk
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadAsk(a.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("relay second: ask still there (%v)", err)
	}

	b := newAsk(NewAskID(), "s")
	b.Done, b.Settled = true, true
	if err := s.PutAsk(b); err != nil {
		t.Fatal(err)
	}
	if err := s.CollectAsk(b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadAsk(b.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hook second: ask still there (%v)", err)
	}
	if err := s.CollectAsk(b.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("collecting a removed ask: %v", err)
	}
}

// The ask file keeps only a fingerprint of the claimed dialog: the raw
// screen text can hold what the command line holds (a password), and
// fingerprints compare as the signatures do.
func TestClaimSignatureStoresAFingerprint(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 10, 24, 20, 0, 0, 0, time.UTC)
	const sig = "─mysql-uroot--password=hunter2secretdb─Doyouwanttoproceed?"
	a, b := newAsk(NewAskID(), "s"), newAsk(NewAskID(), "s")
	a.Created, b.Created = now.Add(-time.Minute), now.Add(-time.Minute)
	for _, x := range []*Ask{a, b} {
		if err := s.PutAsk(x); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := s.ClaimSignature(a.ID, sig, time.Second, now); !ok || err != nil {
		t.Fatalf("claim: %v %v", ok, err)
	}
	raw, err := os.ReadFile(filepath.Join(s.Dir, "asks", a.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hunter2secret") || strings.Contains(string(raw), "mysql") {
		t.Fatalf("the ask file holds the screen text:\n%s", raw)
	}
	got, _ := s.LoadAsk(a.ID)
	if len(got.Signature) != 64 || got.Signature != Fingerprint(sig) || Fingerprint(sig) == Fingerprint(sig+"x") {
		t.Fatalf("signature %q, want the sha256 hex of the signature", got.Signature)
	}
	if ok, _ := s.ClaimSignature(b.ID, sig, time.Second, now); ok {
		t.Fatal("claimed a dialog another ask holds")
	}
}

// Hooks of concurrent calls claim at the same moment: exactly one wins.
func TestClaimSignatureConcurrent(t *testing.T) {
	s := open(t)
	now := time.Now()
	var asks []*Ask
	for range 8 {
		a := newAsk(NewAskID(), "s")
		a.Created = now
		if err := s.PutAsk(a); err != nil {
			t.Fatal(err)
		}
		asks = append(asks, a)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for _, a := range asks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.ClaimSignature(a.ID, "SIG", time.Second, now)
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d claims held, want 1", won)
	}
}

// holdAsk takes the ask's lock in another goroutine, as the relay's edit
// does, until release is called.
func holdAsk(t *testing.T, s *Store, id string) (release func()) {
	t.Helper()
	held, done, free := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_ = s.WithAsk(id, func(*Ask) error { close(held); <-free; return nil })
	}()
	<-held
	return func() { close(free); <-done }
}

// A caller in a hurry (a hook Claude Code will kill 1.5 s after SIGTERM)
// waits for an ask's lock only as long as it can: UpdateAskWithin and
// DeleteAskWithin give up with ErrAskLocked, a wait of 0 tries once, and
// ForceUpdateAsk writes without the lock.
func TestAskLockBudget(t *testing.T) {
	s := open(t)
	a := newAsk(NewAskID(), "s")
	if err := s.PutAsk(a); err != nil {
		t.Fatal(err)
	}
	release := holdAsk(t, s, a.ID)
	start := time.Now()
	if _, err := s.UpdateAskWithin(a.ID, 200*time.Millisecond, func(a *Ask) error { a.Closed = "mac"; return nil }); !errors.Is(err, ErrAskLocked) {
		t.Fatalf("err = %v, want ErrAskLocked", err)
	}
	if took := time.Since(start); took < 200*time.Millisecond || took > 600*time.Millisecond {
		t.Fatalf("gave up after %s, want about 200ms", took)
	}
	start = time.Now()
	if _, err := s.UpdateAskWithin(a.ID, 0, func(a *Ask) error { return nil }); !errors.Is(err, ErrAskLocked) || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("one try: err = %v after %s", err, time.Since(start))
	}
	if err := s.DeleteAskWithin(a.ID, 0); !errors.Is(err, ErrAskLocked) {
		t.Fatalf("delete: err = %v", err)
	}
	if _, err := s.ForceUpdateAsk(a.ID, func(a *Ask) error { a.Closed = "ended"; return nil }); err != nil {
		t.Fatal(err)
	}
	release()
	if got, _ := s.LoadAsk(a.ID); got.Closed != "ended" {
		t.Fatalf("forced write lost: %+v", got)
	}
	if err := s.DeleteAskWithin(a.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ForceUpdateAsk(a.ID, func(*Ask) error { return nil }); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("forced write of a removed ask: %v", err)
	}
}

// holdSession takes the session's lock in another goroutine until release.
func holdSession(t *testing.T, s *Store, id string) (release func()) {
	t.Helper()
	held, done, free := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.UpdateSession(id, func(*Session) { close(held); <-free })
	}()
	<-held
	return func() { close(free); <-done }
}

// A phone answer owes the phone the session's next answer through a credit
// of its own (Session.AskCredits): the Stop hook spends the oldest credit
// first, then a typed reply's (PendingAnswers); taking an answer back
// drops only that ask's credit, and nothing once it was spent. A followed
// session gets no credit: it reports every answer anyway.
func TestAskCredits(t *testing.T) {
	sess := &Session{SessionID: "s", PendingAnswers: 1}
	if !sess.ArmAsk("a") || sess.ArmAsk("a") || sess.Owed() != 2 {
		t.Fatalf("arm a: %+v", sess)
	}
	if !sess.TakeAnswer() || sess.Owed() != 1 || sess.PendingAnswers != 1 {
		t.Fatalf("Stop spends a's credit first: %+v", sess)
	}
	if !sess.ArmAsk("b") || sess.DropAsk("a") || sess.Owed() != 2 {
		t.Fatalf("taking spent a back took another credit: %+v", sess)
	}
	if !sess.DropAsk("b") || sess.Owed() != 1 {
		t.Fatalf("taking b back: %+v", sess)
	}
	if !sess.TakeAnswer() || sess.TakeAnswer() || sess.Owed() != 0 {
		t.Fatalf("the typed reply's credit, then none: %+v", sess)
	}
	followed := &Session{Follow: true}
	if followed.ArmAsk("c") || followed.Owed() != 0 {
		t.Fatalf("followed: %+v", followed)
	}
}

// TakeBackAnswer drops the ask's credit under the session's lock, waiting
// for that lock only as long as its context allows (UpdateSessionCtx).
func TestTakeBackAnswerWaitsWithinItsContext(t *testing.T) {
	s := open(t)
	if _, err := s.UpdateSession("s", func(sess *Session) { sess.ArmAsk("a1"); sess.ArmAsk("a2") }); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.TakeBackAnswer(context.Background(), "s", "a1"); err != nil {
			t.Fatal(err)
		}
	}
	if sess, _ := s.LoadSession("s"); sess.Owed() != 1 {
		t.Fatalf("owed %d, want 1", sess.Owed())
	}
	release := holdSession(t, s, "s")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := s.TakeBackAnswer(ctx, "s", "a2"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want a timeout", err)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("waited %s for the session lock", took)
	}
	release()
	if err := s.TakeBackAnswer(context.Background(), "s", "a2"); err != nil {
		t.Fatal(err)
	}
	if sess, _ := s.LoadSession("s"); sess.Owed() != 0 {
		t.Fatalf("owed %d, want 0", sess.Owed())
	}
}
