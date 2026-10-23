//go:build !darwin && !linux

package idle

import (
	"context"
	"errors"
)

// IdleSeconds has no probe on this platform. The error makes Away treat the
// user as away, so alerts are delivered rather than silently suppressed.
func IdleSeconds(ctx context.Context) (int, error) {
	return 0, errors.New("idle time is not available on this platform")
}
