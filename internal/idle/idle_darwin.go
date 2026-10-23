package idle

import (
	"context"
	"os/exec"
	"time"
)

// IdleSeconds runs ioreg and parses HIDIdleTime.
func IdleSeconds(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/ioreg", "-c", "IOHIDSystem", "-d", "4").Output()
	if err != nil {
		return 0, err
	}
	return ParseIdle(out)
}
