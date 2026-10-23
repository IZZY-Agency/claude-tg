package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/relay"
)

// launchAgentsDir is ~/Library/LaunchAgents unless TG_LAUNCH_AGENTS_DIR
// (tests) says otherwise.
func launchAgentsDir(c *cli) string {
	if p := c.env("TG_LAUNCH_AGENTS_DIR"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents")
}

// serviceFilePath is where `tg relay install` writes the launchd agent.
func serviceFilePath(c *cli) string {
	return filepath.Join(launchAgentsDir(c), relay.Label+".plist")
}

// renderServiceFile is the launchd plist; it has no value it cannot carry.
func renderServiceFile(binary, stateDir, home string, extra map[string]string) (string, error) {
	return relay.Plist(binary, stateDir, home, extra), nil
}

func newServiceManager(c *cli) serviceManager {
	return &launchdManager{disabled: serviceManagerDisabled(c)}
}

// launchctlFn runs launchctl; tests replace it.
var launchctlFn = func(args ...string) (string, error) {
	out, err := exec.Command("/bin/launchctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// launchdPoll paces the wait for launchd to finish unloading a job and the
// backoff between bootstrap attempts; tests shorten it.
var launchdPoll = 250 * time.Millisecond

type launchdManager struct {
	disabled bool
}

func (m *launchdManager) launchctl(args ...string) (string, error) {
	if m.disabled {
		return "", errors.New("launchctl disabled by TG_NO_SERVICE_MANAGER")
	}
	return launchctlFn(args...)
}

func (m *launchdManager) Kind() string { return "launchd agent" }

func (m *launchdManager) service() string {
	return fmt.Sprintf("gui/%d/%s", os.Getuid(), relay.Label)
}

// State asks launchd whether the relay job is known: `launchctl print`
// succeeds for a loaded job and fails for anything else, which counts as
// not loaded; launchd never answers unknown.
func (m *launchdManager) State() (serviceState, error) {
	if _, err := m.launchctl("print", m.service()); err != nil {
		return serviceInactive, nil
	}
	return serviceActive, nil
}

func (m *launchdManager) loaded() bool {
	state, _ := m.State()
	return state == serviceActive
}

// Install replaces a running relay job: it unloads the old one, waits for
// launchd to forget it, then bootstraps the new definition, retrying a few
// times because a bootstrap right after a bootout can fail with EIO.
func (m *launchdManager) Install(path string) (string, error) {
	target := fmt.Sprintf("gui/%d", os.Getuid())
	_, _ = m.launchctl("bootout", target+"/"+relay.Label)
	for i := 0; i < 20 && m.loaded(); i++ {
		time.Sleep(launchdPoll)
	}
	var out string
	var err error
	for attempt := 1; attempt <= 4; attempt++ {
		out, err = m.launchctl("bootstrap", target, path)
		if err == nil {
			return out, nil
		}
		time.Sleep(launchdPoll * time.Duration(attempt))
	}
	return out, fmt.Errorf("launchctl bootstrap: %w", err)
}

// Uninstall unloads the job and fails while launchd still knows it.
func (m *launchdManager) Uninstall() error {
	out, err := m.launchctl("bootout", m.service())
	if !m.loaded() {
		return nil
	}
	if err != nil {
		return fmt.Errorf("the launchd agent is still loaded (launchctl bootout: %s)", out)
	}
	return errors.New("the launchd agent is still loaded after bootout")
}

func (m *launchdManager) Status() string {
	out, err := m.launchctl("print", m.service())
	if err != nil {
		return notLoadedStatus
	}
	return "loaded, " + summarizeLaunchctl(out)
}
