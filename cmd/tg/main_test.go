package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	var out bytes.Buffer
	code := run([]string{"version"}, strings.NewReader(""), &out, io.Discard)
	if code != 0 || !strings.Contains(out.String(), "tg "+version) {
		t.Fatalf("code=%d out=%q", code, out.String())
	}
}

func TestRunUnknownCommandIsUsage(t *testing.T) {
	code := run([]string{"bogus"}, strings.NewReader(""), io.Discard, io.Discard)
	if code != 1 {
		t.Fatalf("want 1, got %d", code)
	}
}
