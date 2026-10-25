package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func use(id, name, input string) string {
	return `{"type":"assistant","timestamp":"2026-10-24T10:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":` + input + `}]}}` + "\n"
}

func result(id string) string {
	return `{"type":"user","timestamp":"2026-10-24T10:00:01Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `","content":"(Bash completed with no output)","is_error":false}]}}` + "\n"
}

const noise = `{"type":"attachment","attachment":{"type":"x"}}` + "\n" +
	`{"type":"system","subtype":"turn_duration","durationMs":5}` + "\n" +
	`{"type":"user","message":{"role":"user","content":"plain string content"}}` + "\n"

const bashInput = `{"command":"touch spike1.txt","description":"Create empty file spike1.txt"}`

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func watch(t *testing.T, path, tool string, input json.RawMessage) *Watch {
	t.Helper()
	w, err := NewWatch(path, tool, input)
	if err != nil {
		t.Fatalf("NewWatch: %v", err)
	}
	return w
}

func poll(t *testing.T, w *Watch) bool {
	t.Helper()
	r, err := w.Poll()
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	return r
}

func TestWatchMatchesAndResolves(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	write(t, p, noise+use("toolu_01AbC", "Bash", bashInput))
	// Same keys, different order and spacing: still JSON-equal.
	w := watch(t, p, "Bash", json.RawMessage(`{"description":"Create empty file spike1.txt", "command":"touch spike1.txt"}`))
	if poll(t, w) {
		t.Fatal("resolved before any result")
	}
	appendTo(t, p, noise+result("toolu_01AbC"))
	if !poll(t, w) {
		t.Fatal("not resolved after tool_result")
	}
}

func TestWatchIgnoresOtherCalls(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	write(t, p, use("toolu_A", "Bash", bashInput)+use("toolu_B", "Bash", `{"command":"ls"}`)+use("toolu_C", "Read", bashInput))
	w := watch(t, p, "Bash", json.RawMessage(bashInput))
	appendTo(t, p, result("toolu_B")+result("toolu_C"))
	if poll(t, w) {
		t.Fatal("result of another call resolved the watch")
	}
	appendTo(t, p, result("toolu_A"))
	if !poll(t, w) {
		t.Fatal("own result did not resolve")
	}
}

func TestWatchPicksUnresolvedDuplicate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	write(t, p, use("toolu_1", "Bash", bashInput)+result("toolu_1")+use("toolu_2", "Bash", bashInput))
	w := watch(t, p, "Bash", json.RawMessage(bashInput))
	if poll(t, w) {
		t.Fatal("resolved by the first call's result")
	}
	if w.id != "toolu_2" {
		t.Fatalf("watching %q, want toolu_2", w.id)
	}
	appendTo(t, p, result("toolu_2"))
	if !poll(t, w) {
		t.Fatal("second call's result did not resolve")
	}
}

func TestWatchHandlesHugeLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	huge := `{"type":"api-request-blob","blob":"` + strings.Repeat("x", 2<<20) + `"}` + "\n"
	write(t, p, huge+use("toolu_H", "AskUserQuestion", `{"questions":[{"question":"Which?"}]}`))
	w := watch(t, p, "AskUserQuestion", json.RawMessage(`{"questions":[{"question":"Which?"}]}`))
	if poll(t, w) {
		t.Fatal("resolved too early")
	}
	appendTo(t, p, result("toolu_H"))
	if !poll(t, w) {
		t.Fatal("not resolved")
	}
}

func TestWatchReadsOnlyAppendedBytes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	write(t, p, use("toolu_X", "Bash", bashInput))
	w := watch(t, p, "Bash", json.RawMessage(bashInput))
	poll(t, w)
	first := w.offset
	if first == 0 {
		t.Fatal("offset not advanced")
	}
	// Corrupt the first byte without changing the size: a rescan would lose the match.
	f, err := os.OpenFile(p, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("#"), 0); err != nil {
		t.Fatal(err)
	}
	f.Close()
	appendTo(t, p, result("toolu_X"))
	if !poll(t, w) {
		t.Fatal("not resolved")
	}
	if w.offset <= first {
		t.Fatalf("offset %d did not grow past %d", w.offset, first)
	}
}

func TestWatchPartialLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	write(t, p, use("toolu_P", "Bash", bashInput))
	w := watch(t, p, "Bash", json.RawMessage(bashInput))
	poll(t, w)
	r := result("toolu_P")
	appendTo(t, p, r[:40])
	if poll(t, w) {
		t.Fatal("resolved from half a line")
	}
	appendTo(t, p, r[40:])
	if !poll(t, w) {
		t.Fatal("not resolved after the line completed")
	}
}

func TestWatchShrunkFileRescans(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	write(t, p, noise+noise+use("toolu_S", "Bash", bashInput))
	w := watch(t, p, "Bash", json.RawMessage(bashInput))
	poll(t, w)
	write(t, p, use("toolu_S2", "Bash", bashInput))
	if poll(t, w) {
		t.Fatal("resolved before the new call's result")
	}
	appendTo(t, p, result("toolu_S2"))
	if !poll(t, w) {
		t.Fatal("shrunk file was not rescanned")
	}
}

// A transcript that cannot be stat'ed gives no watch: the hook cannot
// tell when the dialog closes.
func TestNewWatchNeedsTheTranscript(t *testing.T) {
	w, err := NewWatch(filepath.Join(t.TempDir(), "nope.jsonl"), "Bash", json.RawMessage(bashInput))
	if err == nil || w != nil {
		t.Fatalf("got (%v, %v), want (nil, error)", w, err)
	}
}

func TestWatchMissingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	write(t, p, noise)
	w := watch(t, p, "Bash", json.RawMessage(bashInput))
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	r, err := w.Poll()
	if err == nil || r {
		t.Fatalf("got (%v, %v), want (false, error)", r, err)
	}
}

func TestWatchIgnoresOldResolvedIdenticalCall(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	write(t, p, use("toolu_1", "Bash", bashInput)+result("toolu_1"))
	w := watch(t, p, "Bash", json.RawMessage(bashInput))
	if poll(t, w) {
		t.Fatal("old resolved call counted as resolved")
	}
	appendTo(t, p, use("toolu_2", "Bash", bashInput))
	if poll(t, w) {
		t.Fatal("resolved before the pending call's result")
	}
	appendTo(t, p, result("toolu_2"))
	if !poll(t, w) {
		t.Fatal("pending call's result did not resolve")
	}
}

func TestWatchFirstScanKeepsHugePendingLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	body := strings.Repeat("y", 10<<20)
	in := `{"file_path":"/x","content":"` + body + `"}`
	write(t, p, noise+use("toolu_W", "Write", in))
	w := watch(t, p, "Write", json.RawMessage(in))
	if poll(t, w) {
		t.Fatal("resolved too early")
	}
	if w.id != "toolu_W" {
		t.Fatalf("watching %q, want toolu_W", w.id)
	}
	appendTo(t, p, result("toolu_W"))
	if !poll(t, w) {
		t.Fatal("not resolved")
	}
}

// The first scan reads only the last 8 MiB, from the start of the line the
// window lands in: an unanswered call before it is never seen.
func TestWatchFirstScanWindowMidLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	blob := `{"type":"api-request-blob","blob":"` + strings.Repeat("z", 9<<20) + `"}` + "\n"
	write(t, p, use("toolu_old", "Bash", bashInput)+noise+blob)
	w := watch(t, p, "Bash", json.RawMessage(bashInput))
	if poll(t, w) || w.id != "" {
		t.Fatalf("watching %q: the call before the window was read", w.id)
	}
	appendTo(t, p, use("toolu_M", "Bash", bashInput))
	if poll(t, w) || w.id != "toolu_M" {
		t.Fatalf("watching %q, want toolu_M", w.id)
	}
	appendTo(t, p, result("toolu_M"))
	if !poll(t, w) {
		t.Fatal("not resolved")
	}
}

// An Esc at the Mac can write the call's tool_use line only together with
// its result: a call that appears after the watch began is the pending one
// even when it arrives already resolved.
func TestWatchCallAppearingResolvedAfterStart(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	write(t, p, noise)
	w := watch(t, p, "Bash", json.RawMessage(bashInput))
	if poll(t, w) {
		t.Fatal("resolved before the call appeared")
	}
	appendTo(t, p, use("toolu_E", "Bash", bashInput)+result("toolu_E"))
	if !poll(t, w) {
		t.Fatal("call written together with its result not seen as resolved")
	}
}

// Even before the first Poll: the watch's start is fixed when it is made.
func TestWatchCallResolvedBeforeFirstPoll(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	write(t, p, use("toolu_1", "Bash", bashInput)+result("toolu_1"))
	w := watch(t, p, "Bash", json.RawMessage(bashInput))
	appendTo(t, p, noise+use("toolu_2", "Bash", bashInput)+result("toolu_2"))
	if !poll(t, w) {
		t.Fatal("call answered before the first Poll not seen as resolved")
	}
}

func TestWatchOldResolvedCallAloneNeverResolves(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	write(t, p, use("toolu_1", "Bash", bashInput)+result("toolu_1"))
	w := watch(t, p, "Bash", json.RawMessage(bashInput))
	if poll(t, w) {
		t.Fatal("old resolved call counted as resolved")
	}
	appendTo(t, p, noise)
	if poll(t, w) {
		t.Fatal("old resolved call counted as resolved after more lines")
	}
}

func TestWatchTakesFirstNewCall(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	write(t, p, noise)
	w := watch(t, p, "Bash", json.RawMessage(bashInput))
	appendTo(t, p, use("toolu_A", "Bash", bashInput)+use("toolu_B", "Bash", bashInput))
	if poll(t, w) || w.id != "toolu_A" {
		t.Fatalf("watching %q, want toolu_A", w.id)
	}
	appendTo(t, p, result("toolu_B"))
	if poll(t, w) {
		t.Fatal("the second new call's result resolved the watch")
	}
	appendTo(t, p, result("toolu_A"))
	if !poll(t, w) {
		t.Fatal("not resolved")
	}
}

// An older identical call that never got a result (an interrupted run)
// gives way to the call that appears after the watch began.
func TestWatchNewCallWinsOverStaleUnresolved(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	write(t, p, use("toolu_old", "Bash", bashInput))
	w := watch(t, p, "Bash", json.RawMessage(bashInput))
	if poll(t, w) {
		t.Fatal("resolved too early")
	}
	appendTo(t, p, use("toolu_new", "Bash", bashInput)+result("toolu_new"))
	if !poll(t, w) {
		t.Fatal("new call's result did not resolve")
	}
}

// An input that is not JSON matches no call, not even one whose input is null.
func TestWatchUnparsableInputNeverMatches(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	write(t, p, use("toolu_1", "Bash", "null"))
	w := watch(t, p, "Bash", json.RawMessage(`{"command":`))
	appendTo(t, p, result("toolu_1")+use("toolu_2", "Bash", "null")+result("toolu_2"))
	if poll(t, w) || w.id != "" {
		t.Fatalf("matched %q", w.id)
	}
}

// Only results of matching calls are kept: a long session has thousands.
func TestWatchKeepsOnlyCandidateResults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	write(t, p, use("toolu_A", "Bash", bashInput)+use("toolu_B", "Bash", `{"command":"ls"}`)+result("toolu_B")+result("toolu_A")+result("toolu_Z"))
	w := watch(t, p, "Bash", json.RawMessage(bashInput))
	poll(t, w)
	if len(w.results) != 1 || !w.results["toolu_A"] {
		t.Fatalf("results=%v, want only toolu_A", w.results)
	}
}

func TestLineStart(t *testing.T) {
	long := strings.Repeat("y", 200<<10) // longer than lineStart's 64 KiB read
	content := "aaa\nbbbb\n" + long + "\ncc"
	p := filepath.Join(t.TempDir(), "t.jsonl")
	write(t, p, content)
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	longAt := int64(len("aaa\nbbbb\n"))
	ccAt := longAt + int64(len(long)) + 1
	cases := []struct{ pos, want int64 }{
		{0, 0}, {2, 0}, {3, 0}, // inside the first line, and its newline
		{4, 4}, {6, 4}, // a line start stays; mid-line backs up
		{longAt, longAt}, {longAt + 100<<10, longAt}, {ccAt - 1, longAt},
		{ccAt, ccAt}, {ccAt + 1, ccAt},
	}
	for _, c := range cases {
		if got, err := lineStart(f, c.pos); err != nil || got != c.want {
			t.Errorf("lineStart(%d) = %d, %v; want %d", c.pos, got, err, c.want)
		}
	}
}
