package relay

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// UnitName is the systemd user unit that keeps `tg relay` running on Linux,
// the counterpart of the launchd Label.
const UnitName = "claude-tg-relay.service"

// UnitPath is where the user unit definition lives.
func UnitPath(home string) string {
	return filepath.Join(home, ".config", "systemd", "user", UnitName)
}

// unitSpecifiers doubles % so systemd does not read it as a specifier;
// every directive the unit uses resolves specifiers (systemd.unit(5)).
func unitSpecifiers(s string) string {
	return strings.ReplaceAll(s, "%", "%%")
}

// unitWord renders one word of a space-separated directive (ExecStart,
// Environment) the way systemd.syntax(7) unquotes it: wrapped in double
// quotes with backslash escapes when it holds a space, quotes or
// backslashes, bare otherwise. Control characters never get here: Unit
// refuses them.
func unitWord(s string) string {
	s = unitSpecifiers(s)
	if !strings.ContainsAny(s, ` "'\`) {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// execCommand renders an ExecStart= command line. systemd substitutes $VAR
// and ${VAR} in the arguments and takes $$ for a literal dollar; the
// program, the first word, is never a variable (systemd.service(5)), so a $
// in its path must stay single or the path changes.
func execCommand(program string, args ...string) string {
	words := make([]string, 0, 1+len(args))
	words = append(words, unitWord(program))
	for _, a := range args {
		words = append(words, unitWord(strings.ReplaceAll(a, "$", "$$")))
	}
	return strings.Join(words, " ")
}

// checkUnitValue refuses a value with a control character: a newline would
// start a new directive, and the rest have no place in a unit file.
func checkUnitValue(what, s string) error {
	if i := strings.IndexFunc(s, unicode.IsControl); i >= 0 {
		r, _ := utf8.DecodeRuneInString(s[i:])
		return fmt.Errorf("%s %q contains control character %q", what, s, r)
	}
	return nil
}

// Unit renders the systemd user service that keeps `tg relay` running: the
// same command, PATH, HOME, extra environment, restart policy and log file as
// the launchd Plist. extra holds variables such as TG_CONFIG and TG_STATE_DIR
// so the service reads the same config and state as the CLI. It returns an
// error instead of a unit when any value holds a control character.
func Unit(binary, stateDir, home string, extra map[string]string) (string, error) {
	for _, v := range []struct{ what, s string }{{"binary path", binary}, {"state directory", stateDir}, {"home directory", home}} {
		if err := checkUnitValue(v.what, v.s); err != nil {
			return "", err
		}
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := checkUnitValue("environment variable name", k); err != nil {
			return "", err
		}
		if err := checkUnitValue("environment variable "+k, extra[k]); err != nil {
			return "", err
		}
	}
	log := unitSpecifiers(filepath.Join(stateDir, "relay.out.log"))
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=claude-tg Telegram relay\n\n[Service]\n")
	fmt.Fprintf(&b, "ExecStart=%s\n", execCommand(binary, "relay"))
	b.WriteString("Restart=always\nRestartSec=10\n")
	// A stop signals the relay alone, which then ends its voice command's
	// process group itself; signalled together, the command could die
	// first and its failure reach the phone.
	b.WriteString("KillMode=mixed\n")
	// Environment= resolves specifiers but gives $ no meaning (systemd.exec(5)).
	fmt.Fprintf(&b, "Environment=%s\n", unitWord("PATH="+AgentPath(home)))
	fmt.Fprintf(&b, "Environment=%s\n", unitWord("HOME="+home))
	for _, k := range keys {
		fmt.Fprintf(&b, "Environment=%s\n", unitWord(k+"="+extra[k]))
	}
	fmt.Fprintf(&b, "StandardOutput=append:%s\nStandardError=append:%s\n", log, log)
	b.WriteString("\n[Install]\nWantedBy=default.target\n")
	return b.String(), nil
}
