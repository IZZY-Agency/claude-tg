package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/term"
)

// modes are the values of Claude Code's --permission-mode. The list is fixed
// because the value goes into a command line.
var modes = []string{"acceptEdits", "auto", "bypassPermissions", "dontAsk", "manual", "plan"}

var (
	plainPath = regexp.MustCompile(`^[A-Za-z0-9_./+-]+$`)
	sessionID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// Launch is how to start Claude Code as session id, for a shell and for a
// pane with none: the latter goes through the login shell, since the
// terminal's own PATH lacks what Claude Code and its hooks need. mode is a
// --permission-mode; empty adds no flag, so the session starts in the owner's
// own default mode. Nothing in it is quoted beyond the one pair around the
// shell's command, so anything that would need quoting is refused, and no text
// from the phone is part of it.
func Launch(shell, claude, id, mode string) (term.Launch, error) {
	if !plainPath.MatchString(shell) {
		return term.Launch{}, fmt.Errorf("login shell %q is not a plain path", shell)
	}
	if !plainPath.MatchString(claude) {
		return term.Launch{}, fmt.Errorf("claude path %q is not a plain path", claude)
	}
	if !sessionID.MatchString(id) {
		return term.Launch{}, errors.New("not a session id: " + id)
	}
	if mode != "" && !ValidMode(mode) {
		return term.Launch{}, fmt.Errorf("new_session_permission_mode %q is not one of %s", mode, strings.Join(modes, ", "))
	}
	line := claude
	if mode != "" {
		line += " --permission-mode " + mode
	}
	line += " --session-id " + id
	return term.Launch{Line: line, Process: shell + " -lc '" + line + "'"}, nil
}

// ValidMode reports whether mode is one Claude Code's --permission-mode takes.
func ValidMode(mode string) bool { return slices.Contains(modes, mode) }

// LoginShell is the user's login shell: $SHELL, which a launchd agent does
// not have, else the system's user database, else /bin/sh.
func LoginShell(ctx context.Context) string {
	name := ""
	if out, err := output(ctx, "id", "-un"); err == nil {
		name = strings.TrimSpace(out)
	}
	return loginShell(os.Getenv("SHELL"), func(user string) string { return shellOf(ctx, user) }, name)
}

func loginShell(env string, lookup func(string) string, user string) string {
	if env != "" {
		return env
	}
	if s := lookup(user); s != "" {
		return s
	}
	return "/bin/sh"
}

func shellOf(ctx context.Context, user string) string {
	if user == "" {
		return ""
	}
	if runtime.GOOS == "darwin" {
		out, err := output(ctx, "dscl", ".", "-read", "/Users/"+user, "UserShell")
		if err != nil {
			return ""
		}
		return parseDscl(out)
	}
	out, err := output(ctx, "getent", "passwd", user)
	if err != nil {
		return ""
	}
	return parsePasswd(out)
}

func output(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// parseDscl reads "UserShell: /bin/zsh".
func parseDscl(out string) string {
	s, ok := strings.CutPrefix(strings.TrimSpace(out), "UserShell:")
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// parsePasswd reads the shell, the last field of a passwd line.
func parsePasswd(out string) string {
	fields := strings.Split(strings.TrimSpace(out), ":")
	if len(fields) < 7 {
		return ""
	}
	return fields[len(fields)-1]
}
