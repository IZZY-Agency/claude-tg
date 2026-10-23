// Command tg posts one-way messages from Claude Code sessions to a private
// Telegram bot chat and installs its own Claude Code skill and hooks.
package main

import (
	"fmt"
	"io"
	"os"
	"sort"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// cli carries the streams and environment a command runs against, so tests
// can drive commands without touching the process globals.
type cli struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	env    func(string) string
}

type command struct {
	usage string
	run   func(c *cli, args []string) int
}

var commands = map[string]command{}

func register(name, usage string, fn func(c *cli, args []string) int) {
	commands[name] = command{usage: usage, run: fn}
}

func init() {
	register("version", "print the version (also: tg --version, tg -v)", func(c *cli, _ []string) int {
		fmt.Fprintf(c.stdout, "tg %s\n", version)
		return 0
	})
	register("help", "show this help", func(c *cli, _ []string) int {
		printUsage(c.stdout)
		return 0
	})
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: tg <command> [flags]")
	fmt.Fprintln(w)
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "  %-10s %s\n", n, commands[n].usage)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	c := &cli{stdin: stdin, stdout: stdout, stderr: stderr, env: os.Getenv}
	if len(args) == 0 {
		printUsage(stderr)
		return 1
	}
	name := args[0]
	// --version and -v are the version command under the spelling other
	// tools expect from a flag (the Homebrew formula test runs `tg --version`).
	if name == "--version" || name == "-v" {
		name = "version"
	}
	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(stderr, "tg: unknown command %q\n\n", args[0])
		printUsage(stderr)
		return 1
	}
	return cmd.run(c, args[1:])
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
