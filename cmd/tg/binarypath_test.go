package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A Homebrew install resolves to a versioned Cellar directory that disappears
// on upgrade; the hooks and the relay must record the stable opt path instead.
func TestStableBinaryPathPrefersHomebrewOpt(t *testing.T) {
	prefix := t.TempDir()
	cellar := filepath.Join(prefix, "Cellar", "claude-tg", "0.2.0", "bin")
	opt := filepath.Join(prefix, "opt", "claude-tg", "bin")
	for _, d := range []string{cellar, opt} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cellar, "tg"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(cellar, "tg"), filepath.Join(opt, "tg")); err != nil {
		t.Fatal(err)
	}
	if got := stableBinaryPath(filepath.Join(cellar, "tg")); got != filepath.Join(opt, "tg") {
		t.Fatalf("got %q, want the opt path", got)
	}
	// An opt entry that is not this executable (another program, a dangling
	// link, a directory) must not be recorded.
	stranger := filepath.Join(prefix, "Cellar", "stranger", "2.0", "bin")
	if err := os.MkdirAll(filepath.Join(prefix, "opt", "stranger", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stranger, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stranger, "tg"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prefix, "opt", "stranger", "bin", "tg"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := stableBinaryPath(filepath.Join(stranger, "tg")); got != filepath.Join(stranger, "tg") {
		t.Fatalf("a different program under opt was accepted: %q", got)
	}
	dangling := filepath.Join(prefix, "Cellar", "dangling", "1.0", "bin")
	if err := os.MkdirAll(dangling, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dangling, "tg"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, "opt", "dangling", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(prefix, "nowhere"), filepath.Join(prefix, "opt", "dangling", "bin", "tg")); err != nil {
		t.Fatal(err)
	}
	if got := stableBinaryPath(filepath.Join(dangling, "tg")); got != filepath.Join(dangling, "tg") {
		t.Fatalf("a dangling opt link was accepted: %q", got)
	}
	// Without an opt counterpart the Cellar path is all there is.
	other := filepath.Join(prefix, "Cellar", "other", "1.0", "bin", "tg")
	if got := stableBinaryPath(other); got != other {
		t.Fatalf("got %q, want %q", got, other)
	}
	// Ordinary locations pass through.
	if got := stableBinaryPath("/Users/alex/.local/bin/tg"); got != "/Users/alex/.local/bin/tg" {
		t.Fatalf("got %q", got)
	}
}
