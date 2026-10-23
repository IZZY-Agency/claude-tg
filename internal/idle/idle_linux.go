package idle

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// IdleSeconds asks logind for this user's idle hint. Only a desktop session
// maintains it, so XDG_SESSION_TYPE decides what IdleHint=no means; see
// ParseLoginctl. A missing loginctl or a user without a logind session is
// an error, which Away reads as away.
func IdleSeconds(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "loginctl", "show-user", strconv.Itoa(os.Getuid()), "--property=IdleHint", "--property=IdleSinceHint").Output()
	if err != nil {
		return 0, err
	}
	return ParseLoginctl(out, time.Now(), Graphical(os.Getenv("XDG_SESSION_TYPE")))
}
