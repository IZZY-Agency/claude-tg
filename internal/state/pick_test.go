package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The pick round-trips through pick.json; a result without a session
// removes the file, and an unreadable file reads as no pick.
func TestPickRoundTrip(t *testing.T) {
	s := open(t)
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	if p, err := s.LoadPick(); err != nil || p != (Pick{}) {
		t.Fatalf("empty store: %+v, %v", p, err)
	}
	got, err := s.UpdatePick(func(p *Pick) { *p = Pick{SessionID: "sid-1", At: at} })
	if err != nil || got.SessionID != "sid-1" || !got.At.Equal(at) {
		t.Fatalf("UpdatePick = %+v, %v", got, err)
	}
	if p, err := s.LoadPick(); err != nil || p.SessionID != "sid-1" || !p.At.Equal(at) {
		t.Fatalf("LoadPick = %+v, %v", p, err)
	}
	if _, err := s.UpdatePick(func(p *Pick) { p.SessionID = "" }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "pick.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a cleared pick leaves its file: %v", err)
	}
}

// A pick that cannot be read is an error, never "no pick": a malformed
// file, one without a session or without a time, a read error. UpdatePick fails on it too,
// while SavePick replaces it, and a zero SavePick removes it.
func TestPickUnreadable(t *testing.T) {
	s := open(t)
	path := filepath.Join(s.Dir, "pick.json")
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for _, bad := range []string{"{not json", `{"at":"2026-10-26T12:00:00Z"}`, `{"session_id":"sid-1"}`} {
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if p, err := s.LoadPick(); err == nil {
			t.Fatalf("LoadPick of %q = %+v, want an error", bad, p)
		}
		if _, err := s.UpdatePick(func(p *Pick) {}); err == nil {
			t.Fatalf("UpdatePick over %q: no error", bad)
		}
	}
	if err := s.SavePick(Pick{SessionID: "sid-2", At: at}); err != nil {
		t.Fatal(err)
	}
	if p, err := s.LoadPick(); err != nil || p.SessionID != "sid-2" || !p.At.Equal(at) {
		t.Fatalf("after SavePick: %+v, %v", p, err)
	}
	if err := s.SavePick(Pick{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a zero SavePick leaves the file: %v", err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadPick(); err == nil {
		t.Fatal("LoadPick of a directory: no error")
	}
}

// A pick is fresh while it names a session and its time lies within the
// ttl of now, on either side.
func TestPickFresh(t *testing.T) {
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	p := Pick{SessionID: "sid-1", At: at}
	for _, c := range []struct {
		now  time.Time
		want bool
	}{
		{at, true},
		{at.Add(time.Hour), true},
		{at.Add(time.Hour + time.Second), false},
		{at.Add(-time.Hour), true},
		{at.Add(-time.Hour - time.Second), false},
	} {
		if got := p.Fresh(c.now, time.Hour); got != c.want {
			t.Errorf("Fresh(%v) = %v, want %v", c.now.Sub(at), got, c.want)
		}
	}
	if (Pick{At: at}).Fresh(at, time.Hour) {
		t.Error("a pick without a session is never fresh")
	}
}
