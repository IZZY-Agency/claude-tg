package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestVersionFlagsPrintTheVersionCommand pins `tg --version` and `tg -v` to
// the exact output of `tg version`: the Homebrew formula test matches the
// version against `tg --version`.
func TestVersionFlagsPrintTheVersionCommand(t *testing.T) {
	invoke := func(arg string) (int, string) {
		var out bytes.Buffer
		code := run([]string{arg}, strings.NewReader(""), &out, io.Discard)
		return code, out.String()
	}
	wantCode, want := invoke("version")
	if wantCode != 0 || want != "tg "+version+"\n" {
		t.Fatalf("version: code=%d out=%q", wantCode, want)
	}
	for _, flag := range []string{"--version", "-v"} {
		code, got := invoke(flag)
		if code != 0 || got != want {
			t.Errorf("%s: code=%d out=%q, want 0 and %q", flag, code, got, want)
		}
	}
}
