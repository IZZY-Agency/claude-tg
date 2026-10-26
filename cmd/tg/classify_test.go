package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const jevReply = `{"model":"jev-1.14.0","answers":{"factual":{"type":"noul","noul":0.94},"listenable":{"type":"noul","noul":0.91}}}`

// fakeTypesafe points tg at an httptest TypeSafe and counts its requests.
func fakeTypesafe(t *testing.T, h http.HandlerFunc) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		h(w, r)
	}))
	t.Cleanup(s.Close)
	old := classifyBaseURL
	classifyBaseURL = s.URL
	t.Cleanup(func() { classifyBaseURL = old })
	return &n
}

func writeKeyConfig(t *testing.T, dir, key string) {
	t.Helper()
	cfg := `{"bot_token":"1:x","chat_id":1,"typesafe_api_key":"` + key + `"}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyPrintsDecision(t *testing.T) {
	dir := isolate(t)
	writeKeyConfig(t, dir, "k-1")
	fakeTypesafe(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, jevReply) })
	var out, errb bytes.Buffer
	code := run([]string{"classify", "How is the weather?", "It is 14 degrees and rainy in Paris."}, strings.NewReader(""), &out, &errb)
	want := "hard rules: pass\nfactual: 0.94\nlistenable: 0.91\nmodel: jev-1.14.0\ndecision: voice\n"
	if code != 0 || out.String() != want {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errb.String())
	}
}

func TestClassifyTextDecision(t *testing.T) {
	dir := isolate(t)
	writeKeyConfig(t, dir, "k-1")
	fakeTypesafe(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"model":"jev-1.14.0","answers":{"factual":{"type":"noul","noul":0.12},"listenable":{"type":"noul","noul":0.91}}}`)
	})
	var out bytes.Buffer
	run([]string{"classify", "q", "Some answer."}, strings.NewReader(""), &out, io.Discard)
	if !strings.Contains(out.String(), "factual: 0.12\n") || !strings.HasSuffix(out.String(), "decision: text\n") {
		t.Fatalf("out=%q", out.String())
	}
}

func TestClassifyHardRulesStop(t *testing.T) {
	dir := isolate(t)
	writeKeyConfig(t, dir, "k-1")
	n := fakeTypesafe(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, jevReply) })
	var out bytes.Buffer
	code := run([]string{"classify", "q", "Here is the command that lists every file in the folder for you.\n```sh\nls -la\n```\n"}, strings.NewReader(""), &out, io.Discard)
	if code != 0 || out.String() != "hard rules: text (code block or table)\n" || n.Load() != 0 {
		t.Fatalf("code=%d out=%q requests=%d", code, out.String(), n.Load())
	}
	out.Reset()
	long := strings.Repeat("This is a sentence that goes on. ", 40)
	code = run([]string{"classify", "q", long}, strings.NewReader(""), &out, io.Discard)
	if code != 0 || out.String() != "hard rules: text (long or mostly code)\n" || n.Load() != 0 {
		t.Fatalf("long: code=%d out=%q requests=%d", code, out.String(), n.Load())
	}
}

func TestClassifyAnswerFromStdin(t *testing.T) {
	dir := isolate(t)
	writeKeyConfig(t, dir, "k-1")
	var body string
	fakeTypesafe(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		io.WriteString(w, jevReply)
	})
	var out bytes.Buffer
	code := run([]string{"classify", "q", "-"}, strings.NewReader("Piped answer text."), &out, io.Discard)
	if code != 0 || !strings.Contains(body, "Piped answer text.") || !strings.HasSuffix(out.String(), "decision: voice\n") {
		t.Fatalf("code=%d out=%q body=%q", code, out.String(), body)
	}
}

func TestClassifyWithoutKey(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir)
	var out, errb bytes.Buffer
	code := run([]string{"classify", "q", "a"}, strings.NewReader(""), &out, &errb)
	if code != exitNoConfig || errb.String() != "tg classify: no TypeSafe key; run tg setup\n" || out.Len() != 0 {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errb.String())
	}
}

func TestClassifyAPIFailureExits1(t *testing.T) {
	dir := isolate(t)
	writeKeyConfig(t, dir, "k-secret-9")
	fakeTypesafe(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(529) })
	var out, errb bytes.Buffer
	code := run([]string{"classify", "q", "An answer."}, strings.NewReader(""), &out, &errb)
	if code != 1 || !strings.HasPrefix(errb.String(), "tg classify: ") || strings.Contains(errb.String(), "k-secret-9") {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
}

// A fenced block decides first, as in the Stop hook: a long answer holding
// one is text for its code, not its length.
func TestClassifyLongAnswerWithCodeIsCode(t *testing.T) {
	dir := isolate(t)
	writeKeyConfig(t, dir, "k-1")
	n := fakeTypesafe(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, jevReply) })
	answer := strings.Repeat("This is a sentence that goes on. ", 40) + "\n```sh\nls\n```\n"
	var out bytes.Buffer
	code := run([]string{"classify", "q", answer}, strings.NewReader(""), &out, io.Discard)
	if code != 0 || out.String() != "hard rules: text (code block or table)\n" || n.Load() != 0 {
		t.Fatalf("code=%d out=%q requests=%d", code, out.String(), n.Load())
	}
}

// A long answer with a prose lead: TypeSafe judges the lead sentence alone,
// and the verdict says so.
func TestClassifyOpeningParagraph(t *testing.T) {
	const lead = "It's raining in Paris this morning, and the rain should stop around midday, leaving the rest of the day cloudy."
	figures := strings.Repeat("- **Rain:** about 4 mm before noon, then dry, with a high of 17 °C later on.\n", 8)
	dir := isolate(t)
	writeKeyConfig(t, dir, "k-1")
	var body string
	fakeTypesafe(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		io.WriteString(w, jevReply)
	})
	var out bytes.Buffer
	code := run([]string{"classify", "What is the weather in Paris?", lead + "\n\n" + figures}, strings.NewReader(""), &out, io.Discard)
	want := "hard rules: pass (opening paragraph)\nfactual: 0.94\nlistenable: 0.91\nmodel: jev-1.14.0\ndecision: voice\n"
	if code != 0 || out.String() != want {
		t.Fatalf("code=%d out=%q", code, out.String())
	}
	if !strings.Contains(body, "the rain should stop around midday") || strings.Contains(body, "4 mm before noon") {
		t.Fatalf("body=%q, want the lead sentence alone", body)
	}
}

// A long answer led by a short lead-in: TypeSafe judges the lead-in with
// the paragraph after it, not the figures.
func TestClassifyLeadInOpening(t *testing.T) {
	figures := strings.Repeat("- **Rain:** about 4 mm before noon, then dry, with a high of 17 °C later on.\n", 8)
	dir := isolate(t)
	writeKeyConfig(t, dir, "k-1")
	var body string
	fakeTypesafe(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		io.WriteString(w, jevReply)
	})
	var out bytes.Buffer
	answer := "Sure, here it is.\n\nIt's raining in Paris until midday, then cloudy.\n\n" + figures
	code := run([]string{"classify", "What is the weather in Paris?", answer}, strings.NewReader(""), &out, io.Discard)
	if want := "hard rules: pass (opening paragraph)\nfactual: 0.94\nlistenable: 0.91\nmodel: jev-1.14.0\ndecision: voice\n"; code != 0 || out.String() != want {
		t.Fatalf("code=%d out=%q", code, out.String())
	}
	if !strings.Contains(body, "Sure, here it is.") || !strings.Contains(body, "raining in Paris until midday") || strings.Contains(body, "4 mm before noon") {
		t.Fatalf("body=%q, want the lead-in and the paragraph after it alone", body)
	}
}

// The question and the answer are cleaned like a message before they go to
// TypeSafe: every configured secret scrubbed, the TypeSafe key included,
// then credentials redacted.
func TestClassifyScrubsSecrets(t *testing.T) {
	const key = "ts-key-0123456789"
	dir := isolate(t)
	writeKeyConfig(t, dir, key)
	var body string
	fakeTypesafe(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		io.WriteString(w, jevReply)
	})
	var out bytes.Buffer
	code := run([]string{"classify", "Is " + key + " my key?", "Yes, " + key + " is your key, and password=hunter2secret is your password."}, strings.NewReader(""), &out, io.Discard)
	if code != 0 || strings.Contains(body, key) || strings.Contains(body, "hunter2secret") || strings.Count(body, "[redacted]") != 3 {
		t.Fatalf("code=%d body=%q, want the key and the password redacted", code, body)
	}
}
