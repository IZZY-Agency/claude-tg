package state

import "testing"

// A relay serves a feature only while it runs (holds the lock) and its
// state lists the feature; a relay from before the feature drops the list
// when it rewrites its state on start.
func TestRelayServes(t *testing.T) {
	s := open(t)
	if err := s.SaveRelay(RelayState{PID: 7, Features: []string{FeatureAsks}}); err != nil {
		t.Fatal(err)
	}
	if s.RelayServes(FeatureAsks) {
		t.Fatal("no relay holds the lock")
	}
	release, err := s.LockRelay()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !s.RelayServes(FeatureAsks) {
		t.Fatal("running relay lists asks")
	}
	if err := s.SaveRelay(RelayState{PID: 8}); err != nil {
		t.Fatal(err)
	}
	if s.RelayServes(FeatureAsks) {
		t.Fatal("an older relay does not list asks")
	}
	rs, _ := s.LoadRelay()
	if rs.PID != 8 || rs.Features != nil {
		t.Fatalf("state=%+v", rs)
	}
}
