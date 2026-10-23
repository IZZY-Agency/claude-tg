package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// With the service manager guard set and no service file, remove never
// consults the manager and proceeds, --purge included, as it always did.
func TestRemovePurgeProceedsWithoutServiceFile(t *testing.T) {
	dir := isolate(t)
	claudeHome(t)
	writeConfig(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"remove", "--purge"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("remove code=%d stderr=%s", code, errb.String())
	}
	for _, p := range []string{filepath.Join(dir, "config.json"), filepath.Join(dir, "state")} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s still present after remove --purge", p)
		}
	}
	if !strings.Contains(out.String(), "Config and state: deleted\n") || strings.Contains(out.String(), "Relay agent") {
		t.Fatalf("out=%s", out.String())
	}
}

// "No service file" means the check said so. A service file that cannot
// be checked at all is not a licence to assume nothing runs.
func TestRemoveRefusesWhenServiceFileCannotBeChecked(t *testing.T) {
	dir := isolate(t)
	claudeHome(t)
	writeConfig(t, dir)
	// A regular file where the service directory belongs makes the check
	// fail with "not a directory" rather than "no such file".
	if err := os.WriteFile(filepath.Dir(serviceFileIn(dir)), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"remove", "--purge"}, strings.NewReader(""), &out, &errb); code == 0 {
		t.Fatal("remove must fail when the service file cannot be checked")
	}
	for _, want := range []string{"tg remove: cannot check for the relay service file", "not a directory", "nothing removed"} {
		if !strings.Contains(errb.String(), want) {
			t.Fatalf("stderr lacks %q:\n%s", want, errb.String())
		}
	}
	if out.Len() != 0 {
		t.Fatalf("stdout must stay empty, got %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatal("config must survive a refused remove")
	}
}
