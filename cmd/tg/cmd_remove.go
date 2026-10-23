package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/IZZY-Agency/claude-tg/internal/assets"
	"github.com/IZZY-Agency/claude-tg/internal/config"
	"github.com/IZZY-Agency/claude-tg/internal/settings"
)

func init() {
	register("remove", "remove the hooks, allow rules, skill and CLAUDE.md block: tg remove [--purge]", cmdRemove)
}

func cmdRemove(c *cli, args []string) int {
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	purge := fs.Bool("purge", false, "also delete the config and state")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	p := resolvePaths(c)
	// The relay goes first, and only a confirmed stop (or no service file
	// at all) lets the rest proceed: --purge would otherwise delete the
	// config and state under a relay that is still running. With the
	// service manager guard set, the file is dropped without asking it.
	if _, err := os.Stat(serviceFilePath(c)); err == nil {
		if err := removeRelayService(c, serviceManagerDisabled(c)); err != nil {
			fmt.Fprintf(c.stderr, "tg remove: %v\n", err)
			return exitUsage
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(c.stderr, "tg remove: cannot check for the relay service file (%v); nothing removed\n", err)
		return exitUsage
	}
	prefixes := []string{binaryPath()}
	existing, err := config.Load(p.config)
	if err == nil && existing.BinaryPath != "" {
		prefixes = append(prefixes, existing.BinaryPath)
	}

	if src, err := os.ReadFile(p.settings); err == nil {
		out, changed, err := settings.Remove(src, prefixes)
		if err != nil {
			fmt.Fprintf(c.stderr, "tg remove: settings.json is not valid JSON, nothing written: %v\n", err)
			return exitUsage
		}
		if changed {
			backup, err := backupFile(p.settings, src)
			if err != nil {
				fmt.Fprintf(c.stderr, "tg remove: backup: %v\n", err)
				return exitUsage
			}
			if err := replaceFile(p.settings, out); err != nil {
				fmt.Fprintf(c.stderr, "tg remove: write settings: %v\n", err)
				return exitUsage
			}
			fmt.Fprintf(c.stdout, "Hooks and allow rules: removed from %s (backup: %s)\n", p.settings, backup)
		} else {
			fmt.Fprintln(c.stdout, "Hooks and allow rules: nothing to remove")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(c.stderr, "tg remove: read settings: %v\n", err)
		return exitUsage
	}

	removed, err := assets.RemoveSkill(p.skills)
	if err != nil {
		fmt.Fprintf(c.stderr, "tg remove: skill: %v\n", err)
		return exitUsage
	}
	fmt.Fprintf(c.stdout, "Skill: %s\n", removedWord(removed))

	changed, err := assets.RemoveClaudeMD(p.claudeMD)
	if err != nil {
		fmt.Fprintf(c.stderr, "tg remove: CLAUDE.md: %v\n", err)
		return exitUsage
	}
	fmt.Fprintf(c.stdout, "CLAUDE.md: %s\n", removedWord(changed))

	if *purge {
		if err := os.Remove(p.config); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(c.stderr, "tg remove: config: %v\n", err)
			return exitUsage
		}
		if err := os.RemoveAll(stateDir(c)); err != nil {
			fmt.Fprintf(c.stderr, "tg remove: state: %v\n", err)
			return exitUsage
		}
		fmt.Fprintln(c.stdout, "Config and state: deleted")
	}
	return exitOK
}

func removedWord(b bool) string {
	if b {
		return "removed"
	}
	return "nothing to remove"
}
