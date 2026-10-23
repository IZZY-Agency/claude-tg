package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func init() {
	register("log", "print the tail of tg.log: tg log [-n N]", cmdLog)
}

func cmdLog(c *cli, args []string) int {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	n := fs.Int("n", 50, "number of lines")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *n < 0 {
		fmt.Fprintln(c.stderr, "tg log: -n must be zero or more")
		return exitUsage
	}
	b, err := os.ReadFile(filepath.Join(stateDir(c), "tg.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return exitOK
		}
		fmt.Fprintf(c.stderr, "tg log: %v\n", err)
		return 4
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if *n < len(lines) {
		lines = lines[len(lines)-*n:]
	}
	for _, l := range lines {
		fmt.Fprintln(c.stdout, l)
	}
	return exitOK
}
