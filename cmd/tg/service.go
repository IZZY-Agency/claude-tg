package main

import (
	"fmt"
	"strconv"
	"strings"
)

// serviceManager is the per-OS supervisor that keeps the relay running:
// launchd on macOS, systemd's user manager on Linux. The commands write and
// remove the service file themselves; the manager only loads and unloads it.
type serviceManager interface {
	// Install (re)loads the service from unitPath so the relay starts now
	// and on every login; a relay that is already running is replaced. The
	// string is the manager's output, for error messages.
	Install(unitPath string) (string, error)
	// Uninstall stops and unloads the service. It fails unless the manager
	// confirms the service is gone afterwards: still active, or a state it
	// cannot determine (no user bus, no systemctl), both keep the file.
	Uninstall() error
	// State is the manager's answer about the service; with serviceUnknown
	// the error says why it could not be asked.
	State() (serviceState, error)
	// Status is the phrase after "<Kind>: " in `tg relay status`, such as
	// "loaded, running, pid 42" or notLoadedStatus.
	Status() string
	// Kind names the manager in messages: "launchd agent" or
	// "systemd user service".
	Kind() string
}

// serviceState is the manager's answer about the relay service. The zero
// value is the one that commits to nothing.
type serviceState int

const (
	// serviceUnknown: the manager could not be asked (no user bus over a
	// bare ssh login, no systemctl), so the relay may well be running.
	serviceUnknown serviceState = iota
	// serviceInactive: the manager does not run the service.
	serviceInactive
	// serviceActive: the service runs or is on its way (starting,
	// reloading, stopping).
	serviceActive
)

func (s serviceState) String() string {
	switch s {
	case serviceInactive:
		return "inactive"
	case serviceActive:
		return "active"
	}
	return "unknown"
}

// notLoadedStatus is reported when the service file exists but the manager
// does not run it.
const notLoadedStatus = "installed but not loaded (run `tg relay install`)"

// serviceManagerDisabled is the hard guard tests set so no command reaches
// the real launchd or systemd. TG_NO_LAUNCHCTL is the older name and keeps
// working.
func serviceManagerDisabled(c *cli) bool {
	return c.env("TG_NO_SERVICE_MANAGER") != "" || c.env("TG_NO_LAUNCHCTL") != ""
}

// summarizeLaunchctl reduces `launchctl print` output to "<state>, pid <n>".
func summarizeLaunchctl(out string) string {
	state, pid := "state unknown", ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "state = "); ok {
			state = v
		}
		if v, ok := strings.CutPrefix(line, "pid = "); ok {
			if _, err := strconv.Atoi(v); err == nil {
				pid = v
			}
		}
	}
	if pid != "" {
		return state + ", pid " + pid
	}
	return state
}

// systemctlDetail is what a failed systemctl call said: its output, or the
// exec error when it printed nothing (no systemctl on PATH).
func systemctlDetail(out string, err error) string {
	if out = strings.TrimSpace(out); out != "" {
		return out
	}
	if err != nil {
		return err.Error()
	}
	return "no answer"
}

// systemdState maps `systemctl --user is-active` output and its error to a
// state. is-active exits non-zero for inactive, failed and starting units
// too, so the printed word decides; a word that is not a unit state (a bus
// error, a missing systemctl) is unknown, with the error carrying what
// systemctl said.
func systemdState(out string, err error) (serviceState, error) {
	switch strings.TrimSpace(out) {
	case "active", "activating", "reloading", "refreshing", "deactivating":
		return serviceActive, nil
	case "inactive", "failed":
		return serviceInactive, nil
	}
	return serviceUnknown, fmt.Errorf("systemctl --user is-active: %s", systemctlDetail(out, err))
}

// systemdStatus renders the status phrase from `systemctl is-active` output
// (and its error) plus `show -p MainPID --value` output: "active (pid 42)",
// the not-loaded hint for an inactive unit, or what systemctl said when it
// could not answer (no user manager, no systemctl).
func systemdStatus(out string, err error, mainPID string) string {
	state, stateErr := systemdState(out, err)
	word := strings.TrimSpace(out)
	switch state {
	case serviceActive:
		if pid, convErr := strconv.Atoi(strings.TrimSpace(mainPID)); convErr == nil && pid > 0 {
			return fmt.Sprintf("%s (pid %d)", word, pid)
		}
		return word
	case serviceInactive:
		if word == "failed" {
			return "failed (run `tg relay install`)"
		}
		return notLoadedStatus
	}
	return "unknown (" + stateErr.Error() + ")"
}
