package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/IZZY-Agency/claude-tg/internal/relay"
	"github.com/IZZY-Agency/claude-tg/internal/term"
)

// lookPathFn finds terminal tools on the PATH the relay agent runs with,
// not the shell's, so the report matches what the daemon will see; tests
// replace it.
var lookPathFn = func(name string) (string, error) {
	home, _ := os.UserHomeDir()
	return term.LookPathIn(relay.AgentPath(home), name)
}

// noBackendWarning is printed when neither terminal tool is installed.
const noBackendWarning = "warning: neither agtermctl nor tmux is on PATH; replies and /approve will be refused, /sessions, /follow and /status still work"

// backendsLine renders "terminal backends: agterm, tmux"; none is the word
// used when nothing was found.
func backendsLine(none string) string {
	found := term.Detect(lookPathFn)
	if len(found) == 0 {
		return "terminal backends: " + none
	}
	return "terminal backends: " + strings.Join(found, ", ")
}

// reportBackends prints the backends line to out and the warning to errw
// when no backend is present.
func reportBackends(out, errw io.Writer) {
	line := backendsLine("none")
	fmt.Fprintln(out, line)
	if strings.HasSuffix(line, " none") {
		fmt.Fprintln(errw, noBackendWarning)
	}
}
