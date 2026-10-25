package relay

import (
	"context"
	"slices"
	"testing"

	"github.com/IZZY-Agency/claude-tg/internal/state"
)

// A running relay tells waiting hooks it routes ask taps, so a hook never
// sends buttons that an older relay would leave dead.
func TestRunMarksAsksFeature(t *testing.T) {
	f := newFixture(t)
	if err := f.store.SaveRelay(state.RelayState{Offset: 42}); err != nil { // left by an older relay
		t.Fatal(err)
	}
	l := &Loop{Poll: &fakePoller{}, Router: f.r, Store: f.store, Once: true, Log: func(string, ...any) {}}
	if err := l.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	rs, _ := f.store.LoadRelay()
	if !slices.Contains(rs.Features, state.FeatureAsks) || rs.Offset != 42 {
		t.Fatalf("relay state %+v", rs)
	}
}
