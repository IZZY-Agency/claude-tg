package agterm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/term"
)

var _ term.Terminal = (*CLI)(nil)

func TestCLIExistsLive(t *testing.T) {
	id := os.Getenv("AGTERM_SESSION_ID")
	if _, err := exec.LookPath("agtermctl"); err != nil || id == "" {
		t.Skip("needs agtermctl and a session id")
	}
	c := &CLI{}
	if !c.Exists(context.Background(), term.Pane{Backend: Backend, Target: id}) {
		t.Fatalf("this session %s should exist", id)
	}
	if c.Exists(context.Background(), term.Pane{Backend: Backend, Target: "00000000-0000-0000-0000-000000000000"}) {
		t.Fatal("a made-up id must not exist")
	}
}

// An agterm tree, as `agtermctl tree --json` prints it (trimmed): one tab
// the owner named, one carrying Claude Code's terminal title.
const treeJSON = `{"ok":true,"result":{"tree":{"workspaces":[{"id":"W1","name":"work","sessions":[` +
	`{"id":"37E65173-A408-4C8A-B459-A198E80C798E","name":"Nova","title":"✳ NOV-132","cwd":"/Users/alex/work/nova"},` +
	`{"id":"F0B23286-0202-4D2D-9A15-37A3E8BA907F","name":"✳ Login token claim failure on prod","title":"✳ Login token claim failure on prod"}]},` +
	`{"id":"W2","name":"izzy","sessions":[{"id":"E053ADA7-9CFE-4D2F-91CA-C396CC919FE2","name":"Ultracoins","title":"⠂ Ultracoins loyalty rewards"}]}]}}}`

// A tab is found by its session id, ignoring case, in any workspace.
func TestFindTab(t *testing.T) {
	for _, c := range []struct {
		id   string
		want Tab
		ok   bool
	}{
		{"37e65173-a408-4c8a-b459-a198e80c798e", Tab{Name: "Nova", Title: "✳ NOV-132"}, true},
		{"E053ADA7-9CFE-4D2F-91CA-C396CC919FE2", Tab{Name: "Ultracoins", Title: "⠂ Ultracoins loyalty rewards"}, true},
		{"00000000-0000-0000-0000-000000000000", Tab{}, false},
		{"", Tab{}, false},
	} {
		if got, ok := findTab([]byte(treeJSON), c.id); got != c.want || ok != c.ok {
			t.Errorf("findTab(%q) = %+v, %v; want %+v, %v", c.id, got, ok, c.want, c.ok)
		}
	}
	if _, ok := findTab([]byte("not json"), "37E65173-A408-4C8A-B459-A198E80C798E"); ok {
		t.Error("found a tab in a tree that is not JSON")
	}
}

// stub is an agtermctl that records its arguments, one per line, runs body,
// answers `tree` with tree and keeps what `session type` gets on stdin.
func stub(t *testing.T, tree, body string) (c *CLI, args, typed string) {
	t.Helper()
	return stubChanging(t, tree, tree, body)
}

// stubChanging answers `tree` with after once something was typed.
func stubChanging(t *testing.T, tree, after, body string) (c *CLI, args, typed string) {
	t.Helper()
	dir := t.TempDir()
	args, typed = filepath.Join(dir, "args"), filepath.Join(dir, "typed")
	treeFile, afterFile := filepath.Join(dir, "tree.json"), filepath.Join(dir, "after.json")
	for f, b := range map[string]string{treeFile: tree, afterFile: after} {
		if err := os.WriteFile(f, []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "agtermctl")
	script := "#!/bin/sh\ncase \"$1 $2\" in\n" +
		"'tree --json') cat '" + treeFile + "'; exit 0;;\n" +
		"'session type') cat >> '" + typed + "'; cp '" + afterFile + "' '" + treeFile + "';;\nesac\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> '" + args + "'; done\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &CLI{Path: path, Settle: time.Millisecond}, args, typed
}

const noTabs = `{"result":{"tree":{"workspaces":[]}}}`

// shellTab is a tab of the shape agterm reports for a shell at rest.
const shellTab = `{"id":"T1","name":"Shop","cwd":"/code/shop","foregroundShell":"fish","overlay":false,"realized":true}`

func treeOf(tabs ...string) string {
	return `{"result":{"tree":{"workspaces":[{"name":"work","sessions":[` + strings.Join(tabs, ",") + `]}]}}}`
}

var launch = term.Launch{Line: "/bin/claude --session-id X", Process: "/bin/zsh -lc 'claude'"}

// With no tab to use, a new session opens in the background in the Telegram
// workspace, not in whichever workspace is active: the selection at the Mac
// stays, and the program is the session's own process.
func TestStartOpensANewTabInTheTelegramWorkspace(t *testing.T) {
	c, args, _ := stub(t, noTabs, "echo A789687D-3CC2-4403-95E0-27CAA24F0775")
	got, err := c.Start(context.Background(), nil, "/code/shop", "shop", launch)
	if err != nil {
		t.Fatal(err)
	}
	if want := (term.Pane{Backend: Backend, Target: "A789687D-3CC2-4403-95E0-27CAA24F0775"}); got != want {
		t.Fatalf("pane = %+v, want %+v", got, want)
	}
	b, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	// --name=: a folder named --command=x stays a name.
	want := "session\nnew\n--no-select\n--workspace-name\nTelegram\n--create-workspace\n--cwd\n/code/shop\n--name=shop\n--command\n/bin/zsh -lc 'claude'\n"
	if string(b) != want {
		t.Fatalf("args = %q, want %q", b, want)
	}
}

// A tab already resting at a shell in the folder takes the session. Ctrl-C
// goes first: the shell may hold a half-typed command, a read, or a line
// waiting for its continuation, and any of them would take the launch line.
func TestStartUsesAnIdleTab(t *testing.T) {
	c, args, typed := stub(t, treeOf(shellTab), "")
	got, err := c.Start(context.Background(), nil, "/code/shop", "shop", launch)
	if err != nil {
		t.Fatal(err)
	}
	if want := (term.Pane{Backend: Backend, Target: "T1"}); got != want {
		t.Fatalf("pane = %+v, want %+v", got, want)
	}
	b, err := os.ReadFile(typed)
	if err != nil {
		t.Fatal(err)
	}
	if want := "\x03 /bin/claude --session-id X\n"; string(b) != want {
		t.Fatalf("typed %q, want %q", b, want)
	}
	a, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(a), "new") {
		t.Fatalf("opened a tab too: %q", a)
	}
}

func TestStartTypingFails(t *testing.T) {
	c, _, _ := stub(t, treeOf(shellTab), "echo 'session busy' >&2; exit 1")
	if _, err := c.Start(context.Background(), nil, "/code/shop", "shop", launch); err == nil || !strings.Contains(err.Error(), "session busy") {
		t.Fatalf("err = %v", err)
	}
}

func TestIdleTab(t *testing.T) {
	rest := func(extra string) string { return strings.TrimSuffix(shellTab, "}") + "," + extra + "}" }
	for _, c := range []struct {
		name string
		tabs []string
		want string
	}{
		{"a shell at rest", []string{shellTab}, "T1"},
		{"the first of two", []string{shellTab, strings.ReplaceAll(shellTab, "T1", "T2")}, "T1"},
		{"another folder", []string{strings.ReplaceAll(shellTab, "/code/shop", "/code/blog")}, ""},
		{"a folder inside", []string{strings.ReplaceAll(shellTab, "/code/shop", "/code/shop/web")}, ""},
		{"a running program", []string{`{"id":"T1","cwd":"/code/shop","foreground":["vim"],"realized":true}`}, ""},
		// agterm's own words: a process it cannot read shows neither field.
		{"a pane it cannot read", []string{`{"id":"T1","cwd":"/code/shop","realized":true}`}, ""},
		{"an overlay", []string{strings.ReplaceAll(shellTab, `"overlay":false`, `"overlay":true`)}, ""},
		{"an overlay on one pane", []string{rest(`"paneOverlays":["left"]`)}, ""},
		{"a message panel", []string{rest(`"hud":{"message":"working"}`)}, ""},
		{"a question", []string{rest(`"ask":{"id":"Q1"}`)}, ""},
		{"another machine", []string{rest(`"remoteHost":"air"`)}, ""},
		{"no terminal yet", []string{strings.ReplaceAll(shellTab, `"realized":true`, `"realized":false`)}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := idleTab([]byte(treeOf(c.tabs...)), "/code/shop", nil)
			if got != c.want || ok != (c.want != "") {
				t.Fatalf("idleTab = %q %v, want %q", got, ok, c.want)
			}
		})
	}
}

func TestIdleTabInABadTree(t *testing.T) {
	if _, ok := idleTab([]byte("not json"), "/code/shop", nil); ok {
		t.Fatal("a bad tree has no tab")
	}
}

// A link a tab was opened through may point elsewhere by now, and the launch
// has no cd in it: only the very path counts.
func TestIdleTabThroughALinkIsNoMatch(t *testing.T) {
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "code")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	tree := treeOf(strings.ReplaceAll(shellTab, "/code/shop", link))
	if got, ok := idleTab([]byte(tree), real, nil); ok {
		t.Fatalf("idleTab = %q", got)
	}
}

// A tab tg knows a session in is not free, whatever it looks like: Claude Code
// suspended in it leaves a shell in front, and one just started has not taken
// the foreground yet.
func TestStartLeavesTheTabsOfSessionsAlone(t *testing.T) {
	c, args, typed := stub(t, treeOf(shellTab), "echo NEW")
	got, err := c.Start(context.Background(), []term.Pane{{Backend: Backend, Target: "t1"}}, "/code/shop", "shop", launch)
	if err != nil {
		t.Fatal(err)
	}
	if got.Target != "NEW" {
		t.Fatalf("pane = %+v", got)
	}
	if _, err := os.Stat(typed); err == nil {
		t.Fatal("typed into a session's tab")
	}
	if b, _ := os.ReadFile(args); !strings.Contains(string(b), "new") {
		t.Fatalf("args = %q", b)
	}
}

// A tab can stop resting after the first look: the line then stays out of it.
func TestStartLooksAgainBeforeTheLine(t *testing.T) {
	busy := strings.ReplaceAll(shellTab, `"foregroundShell":"fish"`, `"foreground":["vim"]`)
	c, _, typed := stubChanging(t, treeOf(shellTab), treeOf(busy), "echo NEW")
	got, err := c.Start(context.Background(), nil, "/code/shop", "shop", launch)
	if err != nil {
		t.Fatal(err)
	}
	if got.Target != "NEW" {
		t.Fatalf("pane = %+v", got)
	}
	b, err := os.ReadFile(typed)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "\x03" {
		t.Fatalf("typed %q", b)
	}
}

func TestStartNoID(t *testing.T) {
	c, _, _ := stub(t, noTabs, "true")
	if _, err := c.Start(context.Background(), nil, "/code/shop", "shop", launch); err == nil {
		t.Fatal("a session with no id must be an error")
	}
}

func TestStartRefused(t *testing.T) {
	c, _, _ := stub(t, noTabs, "echo 'no window open' >&2; exit 1")
	_, err := c.Start(context.Background(), nil, "/code/shop", "shop", launch)
	if err == nil || !strings.Contains(err.Error(), "no window open") {
		t.Fatalf("err = %v", err)
	}
}

// agterm creates no terminal while the display sleeps: a session that has
// none yet is told apart from one that cannot be read.
func TestScreenNotRealized(t *testing.T) {
	c, _, _ := stub(t, noTabs, "echo 'session not realized' >&2; exit 1")
	_, err := c.Screen(context.Background(), term.Pane{Backend: Backend, Target: "A"}, 10)
	if !errors.Is(err, term.ErrNotRealized) {
		t.Fatalf("err = %v", err)
	}
	c, _, _ = stub(t, noTabs, "echo 'session not found' >&2; exit 1")
	if _, err := c.Screen(context.Background(), term.Pane{Backend: Backend, Target: "A"}, 10); err == nil || errors.Is(err, term.ErrNotRealized) {
		t.Fatalf("err = %v", err)
	}
}
