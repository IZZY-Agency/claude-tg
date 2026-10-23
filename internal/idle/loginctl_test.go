package idle

import (
	"strconv"
	"testing"
	"time"
)

func TestParseLoginctl(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	since := now.Add(-7 * time.Minute).UnixMicro()
	tests := []struct {
		name      string
		out       string
		graphical bool
		want      int
		wantErr   bool
	}{
		{"idle on a desktop", "IdleHint=yes\nIdleSinceHint=" + itoa(since) + "\n", true, 420, false},
		{"idle hint set but no graphical env", "IdleHint=yes\nIdleSinceHint=" + itoa(since) + "\n", false, 420, false},
		{"order does not matter", "IdleSinceHint=" + itoa(since) + "\nIdleHint=yes\n", true, 420, false},
		{"clock skew never goes negative", "IdleHint=yes\nIdleSinceHint=" + itoa(now.Add(time.Minute).UnixMicro()) + "\n", true, 0, false},
		{"at the keyboard on a desktop", "IdleHint=no\nIdleSinceHint=0\n", true, 0, false},
		{"not idle but no desktop session: away", "IdleHint=no\nIdleSinceHint=0\n", false, 0, true},
		{"missing IdleHint", "IdleSinceHint=" + itoa(since) + "\n", true, 0, true},
		{"idle without a timestamp", "IdleHint=yes\n", true, 0, true},
		{"garbage timestamp", "IdleHint=yes\nIdleSinceHint=soon\n", true, 0, true},
		{"unexpected hint", "IdleHint=maybe\nIdleSinceHint=0\n", true, 0, true},
		{"empty output", "", true, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseLoginctl([]byte(tc.out), now, tc.graphical)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
}

func TestGraphical(t *testing.T) {
	for st, want := range map[string]bool{"x11": true, "wayland": true, "tty": false, "": false, "unspecified": false, "mir": false} {
		if Graphical(st) != want {
			t.Fatalf("Graphical(%q)=%v want %v", st, !want, want)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
