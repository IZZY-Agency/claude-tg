package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/IZZY-Agency/claude-tg/internal/render"
	"github.com/IZZY-Agency/claude-tg/internal/state"
)

func TestParseAgentsReal(t *testing.T) {
	b, err := os.ReadFile("testdata/agents.json")
	if err != nil {
		t.Fatal(err)
	}
	agents, err := ParseAgents(b)
	if err != nil || len(agents) == 0 {
		t.Fatalf("agents=%d err=%v", len(agents), err)
	}
	nameRe := regexp.MustCompile(`^[a-z0-9-]+-[0-9a-f]{2}$`)
	found := false
	for _, a := range agents {
		if a.Kind == "interactive" && nameRe.MatchString(a.Name) && a.SessionID != "" && a.Cwd != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no interactive agent with a session-style name in %+v", agents)
	}
}

func TestFromEnv(t *testing.T) {
	env := map[string]string{
		"CLAUDE_CODE_SESSION_ID": "6449d606-55c1-4e6d-a915-4b14c777f57f",
		"AGTERM_SESSION_ID":      "agt-s1",
		"AGTERM_WINDOW_ID":       "agt-w1",
		"CLAUDE_PID":             "10250",
		"TMUX_PANE":              "%5",
		"TMUX":                   "/private/tmp/tmux-501/default,4242,0",
	}
	e := FromEnv(func(k string) string { return env[k] }, "/Users/alex/code/acme-shop")
	if e.SessionID != env["CLAUDE_CODE_SESSION_ID"] || e.AgtermSessionID != "agt-s1" || e.AgtermWindowID != "agt-w1" || e.PID != 10250 || e.Cwd != "/Users/alex/code/acme-shop" {
		t.Fatalf("env=%+v", e)
	}
	if e.TmuxPane != "%5" || e.TmuxSocket != "/private/tmp/tmux-501/default" {
		t.Fatalf("tmux facts: %+v", e)
	}
	none := FromEnv(func(string) string { return "" }, "/x")
	if none.TmuxPane != "" || none.TmuxSocket != "" {
		t.Fatalf("no tmux: %+v", none)
	}
}

func TestFallbackLabel(t *testing.T) {
	if got := FallbackLabel("/Users/alex/code/acme-shop", "6449d606-55c1-4e6d-a915-4b14c777f57f"); got != "acme-shop-6449d6" {
		t.Fatalf("got %q", got)
	}
	if got := FallbackLabel("", ""); got != "mac" {
		t.Fatalf("got %q", got)
	}
	if got := FallbackLabel("/Users/x/proj", ""); got != "proj" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveWithoutClaude(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "st"))
	if err != nil {
		t.Fatal(err)
	}
	env := Env{SessionID: "6449d606-55c1-4e6d-a915-4b14c777f57f", Own: true, Cwd: "/Users/alex/code/acme-shop", PID: 1, AgtermSessionID: "agt-s1", TmuxPane: "%5", TmuxSocket: "/private/tmp/tmux-501/default"}
	start := time.Now()
	sess, err := Resolve(context.Background(), store, "/nonexistent/claude", env, Options{})
	if err != nil {
		t.Fatalf("Resolve must not fail when claude is missing: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("Resolve took %v", time.Since(start))
	}
	if sess.Name != "acme-shop-6449d6" || sess.AgtermSessionID != "agt-s1" || sess.Cwd != env.Cwd || sess.TmuxPane != "%5" || sess.TmuxSocket != "/private/tmp/tmux-501/default" {
		t.Fatalf("sess=%+v", sess)
	}
	again, _ := store.LoadSession(env.SessionID)
	if again.Name != "acme-shop-6449d6" || again.LastSeen.IsZero() {
		t.Fatalf("not persisted: %+v", again)
	}
}

func TestResolveUsesCachedNameWithinTenMinutes(t *testing.T) {
	store, _ := state.Open(filepath.Join(t.TempDir(), "st"))
	id := "6449d606-55c1-4e6d-a915-4b14c777f57f"
	_ = store.SaveSession(&state.Session{SessionID: id, Name: "acme-shop-92", NameResolvedAt: time.Now().Add(-2 * time.Minute)})
	sess, err := Resolve(context.Background(), store, "/nonexistent/claude", Env{SessionID: id, Cwd: "/x/acme-shop"}, Options{})
	if err != nil || sess.Name != "acme-shop-92" {
		t.Fatalf("sess=%+v err=%v", sess, err)
	}
}

func TestTmuxSocketKeepsCommasInPath(t *testing.T) {
	cases := map[string]string{
		"/private/tmp/tmux-501/default,4242,0": "/private/tmp/tmux-501/default",
		"/tmp/work,team,4242,0":                "/tmp/work,team",
		"":                                     "",
		"garbage":                              "",
		"/tmp/x,1":                             "",
	}
	for in, want := range cases {
		if got := tmuxSocket(in); got != want {
			t.Errorf("tmuxSocket(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFromEnvDropsTmuxPaneWithoutSocket(t *testing.T) {
	env := map[string]string{"CLAUDE_CODE_SESSION_ID": "6449d606-55c1-4e6d-a915-4b14c777f57f", "TMUX_PANE": "%5"}
	e := FromEnv(func(k string) string { return env[k] }, "/x")
	if e.TmuxPane != "" || e.TmuxSocket != "" {
		t.Fatalf("pane without socket must be dropped: %+v", e)
	}
	if !e.Own {
		t.Fatal("an env with the session id is the session's own")
	}
	if FromEnv(func(string) string { return "" }, "/x").Own {
		t.Fatal("no session id, not own")
	}
}

func TestResolveReplacesTerminalBinding(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "st"))
	if err != nil {
		t.Fatal(err)
	}
	sid := "6449d606-55c1-4e6d-a915-4b14c777f57f"
	_ = store.SaveSession(&state.Session{SessionID: sid, Name: "x-1", AgtermSessionID: "A1", TmuxPane: "%5", TmuxSocket: "/s", Cwd: "/a", PID: 7})
	// The session was resumed in agterm: its own env now has no tmux facts.
	own := Env{SessionID: sid, Own: true, Cwd: "/b", PID: 9, AgtermSessionID: "A2"}
	sess, err := Resolve(context.Background(), store, "/nonexistent/claude", own, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if sess.TmuxPane != "" || sess.TmuxSocket != "" || sess.AgtermSessionID != "A2" || sess.Cwd != "/b" || sess.PID != 9 {
		t.Fatalf("own env must replace the binding: %+v", sess)
	}
	// A foreign env (tg ... --session from elsewhere) must not touch any of it.
	foreign := Env{SessionID: sid, Cwd: "/c", PID: 11, TmuxPane: "%9", TmuxSocket: "/t", AgtermSessionID: "A3"}
	sess, err = Resolve(context.Background(), store, "/nonexistent/claude", foreign, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if sess.TmuxPane != "" || sess.AgtermSessionID != "A2" || sess.Cwd != "/b" || sess.PID != 9 {
		t.Fatalf("foreign env must leave the binding alone: %+v", sess)
	}
}

// A tab name is the owner's unless it is a frozen copy of Claude Code's
// terminal title, which starts with a status glyph and a space.
func TestUserTab(t *testing.T) {
	for in, want := range map[string]string{
		"Nova":                                "Nova",
		"  HEDG ":                             "HEDG",
		"voice-test-2":                        "voice-test-2",
		"PR.COM":                              "PR.COM",
		"1password":                           "1password",
		"_scratch":                            "_scratch",
		"✳ Login token claim failure on prod": "",
		"◐ Claude Code":                       "",
		"⠂ Ultracoins loyalty rewards":        "",
		"🔥 hotfix":                            "",
		"":                                    "",
	} {
		if got := UserTab(in); got != want {
			t.Errorf("UserTab(%q) = %q, want %q", in, got, want)
		}
	}
}

// A label is the owner's tab name, else the project and Claude Code's
// title (cut at a word to 30 runes with "…"), else Claude Code's name, else
// the folder and the id's start. The examples are the owner's sessions.
func TestLabel(t *testing.T) {
	const id = "6449d606-55c1-4e6d-a915-4b14c777f57f"
	for _, c := range []struct {
		tab, cwd, title, claude, want string
	}{
		{"Nova", "/Users/alex/work/nova", "NOV-132", "nova-6a", "Nova"},
		{"✳ Login token claim failure on prod", "/Users/alex/work/nova", "Login token claim failure on prod", "nova-ca", "nova · Login token claim failure on…"},
		{"◐ Claude Code", "/Users/alex/work/storefront", "Product catalog image redirect loop", "storefront-37", "storefront · Product catalog image…"},
		{"", "/Users/alex/work/dashboard", "Tasks/brief roadmap audit", "dashboard-9f", "dashboard · Tasks/brief roadmap audit"},
		{"", "", "Ready check", "", "Ready check"},
		{"", "/Users/alex/work/nova", "Averyveryverylongsingleworditlookslikethis", "", "nova · Averyveryverylongsinglewordit…"},
		{"", "/Users/alex/work/nova", "", "nova-ca", "nova-ca"},
		{"", "/Users/alex/code/acme-shop", "", "", "acme-shop-6449d6"},
		{"", "/Users/alex/code/acme-shop", "", "  ", "acme-shop-6449d6"},
	} {
		if got := Label(c.tab, c.cwd, c.title, c.claude, id); got != c.want {
			t.Errorf("Label(%q, %q, %q, %q) = %q, want %q", c.tab, c.cwd, c.title, c.claude, got, c.want)
		}
	}
}

// Resolve labels the session on every run: the owner's tab name when the
// agterm tab has one, else the project and the transcript's title, and it
// keeps Claude Code's name, the title and the tab apart in the record. A
// title it cannot read now (the last line still being written) and a tab it
// cannot look up keep what was stored; a session that left agterm loses
// its tab.
func TestResolveLabels(t *testing.T) {
	const id = "6449d606-55c1-4e6d-a915-4b14c777f57f"
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "t.jsonl")
	write := func(s string) {
		if err := os.WriteFile(transcriptPath, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := state.Open(filepath.Join(dir, "st"))
	if err != nil {
		t.Fatal(err)
	}
	tabName, tabErr := "✳ Product catalog image redirect loop", error(nil)
	tabs := func(_ context.Context, agtermID string) (string, bool, error) {
		if agtermID != "agt-s1" {
			t.Errorf("looked up agterm session %q", agtermID)
		}
		return tabName, tabErr == nil, tabErr
	}
	env := Env{SessionID: id, Own: true, Cwd: "/Users/alex/work/storefront", AgtermSessionID: "agt-s1", TranscriptPath: transcriptPath}
	resolve := func() *state.Session {
		t.Helper()
		sess, err := Resolve(context.Background(), store, "/nonexistent/claude", env, Options{Tab: tabs})
		if err != nil {
			t.Fatal(err)
		}
		return sess
	}

	write(`{"type":"ai-title","aiTitle":"Product catalog image redirect loop"}` + "\n")
	if s := resolve(); s.Name != "storefront · Product catalog image…" || s.Title != "Product catalog image redirect loop" || s.Tab != "" {
		t.Fatalf("an unnamed tab: %+v", s)
	}
	tabName = "Ultracoins"
	if s := resolve(); s.Name != "Ultracoins" || s.Tab != "Ultracoins" {
		t.Fatalf("a named tab: %+v", s)
	}
	write(`{"type":"ai-title","aiTitle":"Other"}` + "\n" + `{"type":"user","message":{"content":"hal`)
	tabErr = errors.New("agtermctl: not running")
	if s := resolve(); s.Name != "Ultracoins" || s.Title != "Product catalog image redirect loop" {
		t.Fatalf("an unreadable title and tab kept: %+v", s)
	}
	env.AgtermSessionID = ""
	if s := resolve(); s.Tab != "" || s.Name != "storefront · Product catalog image…" {
		t.Fatalf("a session out of agterm: %+v", s)
	}
}

// A record from before labels holds Claude Code's name in Name: it is kept
// as that name, so the label falls back to it.
func TestResolveKeepsAnOldRecordsName(t *testing.T) {
	store, _ := state.Open(filepath.Join(t.TempDir(), "st"))
	id := "6449d606-55c1-4e6d-a915-4b14c777f57f"
	_ = store.SaveSession(&state.Session{SessionID: id, Name: "acme-shop-92", NameResolvedAt: time.Now().Add(-2 * time.Minute)})
	sess, err := Resolve(context.Background(), store, "/nonexistent/claude", Env{SessionID: id, Cwd: "/x/acme-shop"}, Options{})
	if err != nil || sess.Name != "acme-shop-92" || sess.ClaudeName != "acme-shop-92" {
		t.Fatalf("sess=%+v err=%v", sess, err)
	}
}

// A tab name and a title are cleaned before the label cuts them, so a
// secret is never cut into a fragment the cleaning cannot recognise.
func TestResolveCleansBeforeCutting(t *testing.T) {
	const secret = "123456:ABCDEFsecretbotTOKENxyz"
	dir := t.TempDir()
	transcriptPath := filepath.Join(dir, "t.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"ai-title","aiTitle":"`+secret+` rotate it today please"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(filepath.Join(dir, "st"))
	clean := func(s string) string { return strings.ReplaceAll(s, secret, "[redacted]") }
	env := Env{SessionID: "6449d606-55c1-4e6d-a915-4b14c777f57f", Own: true, Cwd: "/x/acme-shop", TranscriptPath: transcriptPath}
	sess, err := Resolve(context.Background(), store, "/nonexistent/claude", env, Options{Clean: clean})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Name != "acme-shop · ••• rotate it today…" || strings.Contains(sess.Name, "secretbot") {
		t.Fatalf("label = %q", sess.Name)
	}
	env.AgtermSessionID = "agt-s1"
	sess, err = Resolve(context.Background(), store, "/nonexistent/claude", env, Options{Clean: clean, Tab: func(context.Context, string) (string, bool, error) { return "ops " + secret, true, nil }})
	if err != nil || sess.Name != "ops •••" {
		t.Fatalf("label = %q err=%v", sess.Name, err)
	}
}

// A title stored before, or a folder or Claude Code name, is cleaned too:
// the label stored is the label shown, cut only after cleaning.
func TestResolveCleansStoredParts(t *testing.T) {
	const secret = "123456:ABCDEFsecretbotTOKENxyz"
	store, _ := state.Open(filepath.Join(t.TempDir(), "st"))
	id := "6449d606-55c1-4e6d-a915-4b14c777f57f"
	_ = store.SaveSession(&state.Session{SessionID: id, Title: secret + " rotate it today please"})
	clean := func(s string) string { return render.Redact(strings.ReplaceAll(s, secret, "[redacted]")) }
	sess, err := Resolve(context.Background(), store, "/nonexistent/claude", Env{SessionID: id, Own: true, Cwd: "/x/password=abcdefghij"}, Options{Clean: clean})
	if err != nil || strings.Contains(sess.Name, "secretbot") || strings.Contains(sess.Name, "abcdefghij") || !strings.Contains(sess.Name, "•••") {
		t.Fatalf("label = %q err=%v", sess.Name, err)
	}
}

// The label stored is the label every message shows (render.AsShown): a
// value that HTML escaping would lengthen past the redaction threshold is
// stored redacted (as "•••"), so a header and the relay's session list
// read alike.
func TestResolveStoresTheShownLabel(t *testing.T) {
	store, _ := state.Open(filepath.Join(t.TempDir(), "st"))
	id := "6449d606-55c1-4e6d-a915-4b14c777f57f"
	tabs := func(context.Context, string) (string, bool, error) { return "Ops password=ab&cd", true, nil }
	sess, err := Resolve(context.Background(), store, "/nonexistent/claude", Env{SessionID: id, Own: true, Cwd: "/x/ops", AgtermSessionID: "agt-s1"}, Options{Tab: tabs})
	if err != nil || sess.Name != "Ops password=•••" {
		t.Fatalf("label = %q err=%v", sess.Name, err)
	}
}

// A label is at most as long as a header shows one (64 runes, the last
// "…"), so the header and the stored label are the same text; the label
// in full is kept beside it, to address the session by.
func TestResolveClipsTheLabel(t *testing.T) {
	store, _ := state.Open(filepath.Join(t.TempDir(), "st"))
	id := "6449d606-55c1-4e6d-a915-4b14c777f57f"
	long := "Ops " + strings.Repeat("x", 80)
	tabs := func(context.Context, string) (string, bool, error) { return long, true, nil }
	sess, err := Resolve(context.Background(), store, "/nonexistent/claude", Env{SessionID: id, Own: true, Cwd: "/x/ops", AgtermSessionID: "agt-s1"}, Options{Tab: tabs})
	if err != nil || utf8.RuneCountInString(sess.Name) != 64 || !strings.HasSuffix(sess.Name, "…") || render.AsShown(sess.Name) != sess.Name {
		t.Fatalf("label = %q (%d runes) err=%v", sess.Name, utf8.RuneCountInString(sess.Name), err)
	}
	if sess.FullLabel != long {
		t.Fatalf("full label = %q, want %q", sess.FullLabel, long)
	}
}

// A stored label holds no tab or line break, and shows a redacted value as
// "•••", which no later cleaning or cut changes: the 64-rune cut cannot
// leave a marker half cut that the relay would expand again.
func TestResolveLabelIsFinal(t *testing.T) {
	store, _ := state.Open(filepath.Join(t.TempDir(), "st"))
	id := "6449d606-55c1-4e6d-a915-4b14c777f57f"
	for _, tab := range []string{strings.Repeat("x", 46) + " password=abcdefghij", "Ops\tstaging\nnow"} {
		tabs := func(context.Context, string) (string, bool, error) { return tab, true, nil }
		sess, err := Resolve(context.Background(), store, "/nonexistent/claude", Env{SessionID: id, Own: true, Cwd: "/x/ops", AgtermSessionID: "agt-s1"}, Options{Tab: tabs})
		if err != nil {
			t.Fatal(err)
		}
		n := sess.Name
		if strings.ContainsAny(n, "\t\n") || strings.Contains(n, "abcdefghij") || render.AsShown(n) != n || render.Redact(n) != n || utf8.RuneCountInString(n) > 64 {
			t.Fatalf("label = %q", n)
		}
	}
}

// A tab lookup that hangs costs at most tabTimeout, and keeps the stored
// tab: the Stop hook's answer must not wait on agterm.
func TestResolveTabLookupTimesOut(t *testing.T) {
	store, _ := state.Open(filepath.Join(t.TempDir(), "st"))
	id := "6449d606-55c1-4e6d-a915-4b14c777f57f"
	_ = store.SaveSession(&state.Session{SessionID: id, Tab: "Ultracoins", Name: "Ultracoins"})
	hang := func(ctx context.Context, _ string) (string, bool, error) {
		<-ctx.Done()
		return "", false, ctx.Err()
	}
	start := time.Now()
	sess, err := Resolve(context.Background(), store, "/nonexistent/claude", Env{SessionID: id, Own: true, Cwd: "/x/storefront", AgtermSessionID: "agt-s1"}, Options{Tab: hang})
	if err != nil || sess.Name != "Ultracoins" {
		t.Fatalf("sess=%+v err=%v", sess, err)
	}
	if took := time.Since(start); took > tabTimeout+time.Second {
		t.Fatalf("Resolve took %v", took)
	}
}
