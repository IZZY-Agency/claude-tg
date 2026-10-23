package idle

import (
	"context"
	"runtime"
	"testing"
)

// Captured from this Mac with: ioreg -c IOHIDSystem -d 4 | grep HIDIdleTime
const ioregLine = `    | |     "HIDIdleTime" = 490478666`

func TestParseIdle(t *testing.T) {
	out := []byte("+-o IOHIDSystem  <class IOHIDSystem, id 0x100000418, registered, matched, active, busy 0 (0 ms), retain 24>\n    | {\n" + ioregLine + "\n    |   \"HIDParameters\" = {}\n    | }\n")
	got, err := ParseIdle(out)
	if err != nil {
		t.Fatal(err)
	}
	if got < 0 || got > 60*60*24*30 {
		t.Fatalf("implausible idle seconds %d from %q", got, ioregLine)
	}
	exact, err := ParseIdle([]byte(`    | |   "HIDIdleTime" = 657123456789`))
	if err != nil || exact != 657 {
		t.Fatalf("exact parse: %d err=%v", exact, err)
	}
	if _, err := ParseIdle([]byte("nothing here")); err == nil {
		t.Fatal("want error when the key is absent")
	}
}

func TestAwayZeroThresholdIsAlwaysTrue(t *testing.T) {
	if !Away(context.Background(), 0, func(context.Context) (int, error) { return 0, nil }) {
		t.Fatal("threshold 0 must disable the gate")
	}
}

func TestAwayCompares(t *testing.T) {
	probe := func(context.Context) (int, error) { return 100, nil }
	if Away(context.Background(), 120, probe) {
		t.Fatal("100 s idle is not away at a 120 s threshold")
	}
	if !Away(context.Background(), 90, probe) {
		t.Fatal("100 s idle is away at a 90 s threshold")
	}
}

func TestAwayOnProbeErrorIsTrue(t *testing.T) {
	probe := func(context.Context) (int, error) { return 0, context.DeadlineExceeded }
	if !Away(context.Background(), 120, probe) {
		t.Fatal("an unreadable idle time must never suppress an alert")
	}
}

func TestIdleSecondsLive(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	n, err := IdleSeconds(context.Background())
	if err != nil || n < 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}
