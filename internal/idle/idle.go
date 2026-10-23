// Package idle reads how long the machine has been without keyboard or
// mouse input (ioreg on macOS, logind's idle hint on Linux), which gates
// hook-generated alerts.
package idle

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var idleRe = regexp.MustCompile(`"HIDIdleTime"\s*=\s*(\d+)`)

// ParseIdle extracts HIDIdleTime (nanoseconds) from ioreg output and returns seconds.
func ParseIdle(ioregOutput []byte) (int, error) {
	m := idleRe.FindSubmatch(ioregOutput)
	if m == nil {
		return 0, errors.New("HIDIdleTime not found in ioreg output")
	}
	ns, err := strconv.ParseInt(string(m[1]), 10, 64)
	if err != nil {
		return 0, err
	}
	return int(ns / 1e9), nil
}

// Graphical reports whether sessionType, the value of XDG_SESSION_TYPE,
// names a desktop session: the only kind of session that maintains logind's
// idle hint.
func Graphical(sessionType string) bool {
	return sessionType == "x11" || sessionType == "wayland"
}

// ParseLoginctl turns the output of
//
//	loginctl show-user <uid> --property=IdleHint --property=IdleSinceHint
//
// into idle seconds at now. The desktop environment sets the hint through
// logind's SetIdleHint once the session has had no input for a while, and
// IdleSinceHint is that moment in CLOCK_REALTIME microseconds, so
// IdleHint=yes gives now − IdleSinceHint. IdleHint=no means nothing has
// flagged the session idle: 0 (at the keyboard) when a graphical session
// exists, otherwise an error, because over ssh, on a tty or in a container
// no desktop maintains the hint, and Away turns the error into "away" so
// alerts are delivered there. Missing keys are an error for the same reason.
func ParseLoginctl(out []byte, now time.Time, graphical bool) (int, error) {
	hint, since := "", ""
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "IdleHint":
			hint = v
		case "IdleSinceHint":
			since = v
		}
	}
	switch hint {
	case "yes":
		if since == "" {
			return 0, errors.New("IdleSinceHint not found in loginctl output")
		}
		us, err := strconv.ParseInt(since, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("IdleSinceHint %q: %w", since, err)
		}
		n := int(now.Sub(time.UnixMicro(us)) / time.Second)
		if n < 0 {
			n = 0
		}
		return n, nil
	case "no":
		if graphical {
			return 0, nil
		}
		return 0, errors.New("no graphical session maintains logind's idle hint (XDG_SESSION_TYPE is not x11 or wayland)")
	case "":
		return 0, errors.New("IdleHint not found in loginctl output")
	default:
		return 0, fmt.Errorf("unexpected IdleHint %q in loginctl output", hint)
	}
}

// Probe reports idle seconds; IdleSeconds is the real one.
type Probe func(context.Context) (int, error)

// Away reports whether the user counts as away: threshold <= 0 disables the
// gate (always away), and a probe error never suppresses an alert.
func Away(ctx context.Context, threshold int, probe Probe) bool {
	if threshold <= 0 {
		return true
	}
	n, err := probe(ctx)
	if err != nil {
		return true
	}
	return n >= threshold
}
