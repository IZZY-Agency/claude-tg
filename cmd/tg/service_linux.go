package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/IZZY-Agency/claude-tg/internal/relay"
)

// systemdUserDir is ~/.config/systemd/user unless TG_SYSTEMD_USER_DIR
// (tests) says otherwise.
func systemdUserDir(c *cli) string {
	if p := c.env("TG_SYSTEMD_USER_DIR"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user")
}

// serviceFilePath is where `tg relay install` writes the systemd user unit.
func serviceFilePath(c *cli) string {
	return filepath.Join(systemdUserDir(c), relay.UnitName)
}

// renderServiceFile is the systemd user unit; it fails on values a unit
// cannot carry (control characters).
func renderServiceFile(binary, stateDir, home string, extra map[string]string) (string, error) {
	return relay.Unit(binary, stateDir, home, extra)
}

func newServiceManager(c *cli) serviceManager {
	return &systemdManager{disabled: serviceManagerDisabled(c)}
}

// systemctlFn runs `systemctl --user` with args; tests replace it and see
// the args without the --user.
var systemctlFn = func(args ...string) (string, error) {
	out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

type systemdManager struct {
	disabled bool
}

func (m *systemdManager) systemctl(args ...string) (string, error) {
	if m.disabled {
		return "", errors.New("systemctl disabled by TG_NO_SERVICE_MANAGER")
	}
	return systemctlFn(args...)
}

func (m *systemdManager) Kind() string { return "systemd user service" }

// State asks `systemctl --user is-active`; an answer that is not a unit
// state (no user bus, no systemctl) is unknown.
func (m *systemdManager) State() (serviceState, error) {
	return systemdState(m.systemctl("is-active", relay.UnitName))
}

// Install makes systemd read the new unit, enables it for every login and
// (re)starts it: restart rather than start so a reinstall picks up a new
// binary or unit, as the launchd bootout/bootstrap pair does.
func (m *systemdManager) Install(string) (string, error) {
	for _, step := range [][]string{{"daemon-reload"}, {"enable", relay.UnitName}, {"restart", relay.UnitName}} {
		if out, err := m.systemctl(step...); err != nil {
			return out, fmt.Errorf("systemctl --user %s: %w", step[0], err)
		}
	}
	return "", nil
}

// Uninstall stops and disables the unit, then asks systemd again: only a
// confirmed inactive unit lets the caller drop the file. An unknown answer
// (no user bus, no systemctl) is not "stopped": the relay may still run.
func (m *systemdManager) Uninstall() error {
	out, disableErr := m.systemctl("disable", "--now", relay.UnitName)
	state, stateErr := m.State()
	switch state {
	case serviceInactive:
		return nil
	case serviceActive:
		if disableErr != nil {
			return fmt.Errorf("the systemd user service is still active (systemctl --user disable --now: %s)", systemctlDetail(out, disableErr))
		}
		return errors.New("the systemd user service is still active after disable --now")
	}
	msg := "the systemd user service's state is unknown, so the relay may still be running (" + stateErr.Error()
	if disableErr != nil {
		// disable --now usually fails for the same reason; say it once.
		if detail := systemctlDetail(out, disableErr); !strings.HasSuffix(stateErr.Error(), detail) {
			msg += "; systemctl --user disable --now: " + detail
		}
	}
	return errors.New(msg + ")")
}

func (m *systemdManager) Status() string {
	out, err := m.systemctl("is-active", relay.UnitName)
	var pid string
	if state, _ := systemdState(out, err); state == serviceActive {
		pid, _ = m.systemctl("show", "-p", "MainPID", "--value", relay.UnitName)
	}
	return systemdStatus(out, err, pid)
}
