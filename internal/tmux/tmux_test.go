package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/IZZY-Agency/claude-tg/internal/term"
)

var _ term.Terminal = (*CLI)(nil)

func TestCommandsSplitsTextAndKeys(t *testing.T) {
	cases := []struct {
		in   string
		want [][]string
	}{
		{" hello world", [][]string{{"-l", "--", " hello world"}}},
		{"\n", [][]string{{"Enter"}}},
		{"1\n", [][]string{{"-l", "--", "1"}, {"Enter"}}},
		{"\x1b", [][]string{{"Escape"}}},
		{term.Down, [][]string{{"Down"}}},
		{"\x7f\x7f\x7f", [][]string{{"-N", "3", "BSpace"}}},
		{"\x7f\n", [][]string{{"BSpace"}, {"Enter"}}},
		// tmux's parser eats an unescaped trailing ";" of an argument, so
		// trailing semicolons go out as hex keys.
		{"hello;", [][]string{{"-l", "--", "hello"}, {"-H", "3b"}}},
		{"a;;", [][]string{{"-l", "--", "a"}, {"-N", "2", "-H", "3b"}}},
		{";", [][]string{{"-H", "3b"}}},
		{"x;y;", [][]string{{"-l", "--", "x;y"}, {"-H", "3b"}}},
		{"end\\;", [][]string{{"-l", "--", "end\\"}, {"-H", "3b"}}},
		{"a;\n", [][]string{{"-l", "--", "a"}, {"-H", "3b"}, {"Enter"}}},
		{" a\x7fb\n", [][]string{{"-l", "--", " a"}, {"BSpace"}, {"-l", "--", "b"}, {"Enter"}}},
		{" -n looks like a flag", [][]string{{"-l", "--", " -n looks like a flag"}}},
		{"", nil},
	}
	for _, c := range cases {
		if got := Commands(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Commands(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestTailKeepsLastLinesWithoutTrailingBlanks(t *testing.T) {
	out := "a\nb\nc\nd\n\n\n"
	if got := Tail(out, 2); got != "c\nd" {
		t.Fatalf("got %q", got)
	}
	if got := Tail(out, 10); got != "a\nb\nc\nd" {
		t.Fatalf("got %q", got)
	}
	if got := Tail("", 5); got != "" {
		t.Fatalf("got %q", got)
	}
	// Lines that are only spaces count as blank at the end, inner ones stay.
	if got := Tail("x\n   \ny\n   \n", 10); got != "x\n   \ny" {
		t.Fatalf("got %q", got)
	}
}

func TestArgsUseSocketWhenKnown(t *testing.T) {
	c := &CLI{}
	if got := c.args(term.Pane{Backend: Backend, Target: "%5", Socket: "/tmp/tmux-501/default"}, "send-keys", "-t", "%5", "Enter"); !reflect.DeepEqual(got, []string{"-S", "/tmp/tmux-501/default", "send-keys", "-t", "%5", "Enter"}) {
		t.Fatalf("got %v", got)
	}
	if got := c.args(term.Pane{Backend: Backend, Target: "%5"}, "display-message"); !reflect.DeepEqual(got, []string{"display-message"}) {
		t.Fatalf("got %v", got)
	}
}

func TestCommandsChunksLongLiteralRuns(t *testing.T) {
	text := strings.Repeat("é", 5000) // 10000 bytes
	var back strings.Builder
	for _, c := range Commands(text) {
		if len(c) != 3 || c[0] != "-l" {
			t.Fatalf("unexpected command %v", c)
		}
		if len(c[2]) > maxLiteralBytes || !utf8.ValidString(c[2]) {
			t.Fatalf("chunk of %d bytes, valid=%v", len(c[2]), utf8.ValidString(c[2]))
		}
		back.WriteString(c[2])
	}
	if back.String() != text {
		t.Fatal("chunks do not add up to the text")
	}
}

func TestRefusesPaneWithoutSocket(t *testing.T) {
	c := &CLI{Path: "/nonexistent/tmux"}
	p := term.Pane{Backend: Backend, Target: "%0"}
	ctx := context.Background()
	if c.Exists(ctx, p) {
		t.Fatal("a pane without a socket must not exist")
	}
	if err := c.Type(ctx, p, "x"); err == nil || !strings.Contains(err.Error(), "socket") {
		t.Fatalf("Type: %v", err)
	}
	if _, err := c.Screen(ctx, p, 10); err == nil || !strings.Contains(err.Error(), "socket") {
		t.Fatalf("Screen: %v", err)
	}
}

// stub is a tmux that records its arguments, environment and folder, and
// runs body.
func stub(t *testing.T, body string) (*CLI, string, string) {
	t.Helper()
	dir := t.TempDir()
	args, env := filepath.Join(dir, "args"), filepath.Join(dir, "env")
	path := filepath.Join(dir, "tmux")
	script := "#!/bin/sh\nenv > '" + env + "'\npwd >> '" + args + "'\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> '" + args + "'; done\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &CLI{Path: path}, args, env
}

const started = "echo '%7 /tmp/tmux-1/default'"

// tmux expands formats in -c, and #(...) in a format runs a shell command:
// a folder named demo#(id) would run id. So the folder is where tmux is
// started, which it takes as it is.
func TestStartOnTheServerOfTheLikePane(t *testing.T) {
	c, args, _ := stub(t, started)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Start(context.Background(), []term.Pane{{Backend: Backend, Target: "%1", Socket: "/tmp/tmux-1/default"}}, dir, "shop", term.Launch{Process: "/bin/zsh -lc 'claude'"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (term.Pane{Backend: Backend, Target: "%7", Socket: "/tmp/tmux-1/default"}); got != want {
		t.Fatalf("pane = %+v, want %+v", got, want)
	}
	b, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	want := dir + "\n-S\n/tmp/tmux-1/default\nnew-session\n-d\n-P\n-F\n#{pane_id} #{socket_path}\n/bin/zsh -lc 'claude'\n"
	if string(b) != want {
		t.Fatalf("args = %q, want %q", b, want)
	}
}

// One call: a second one to learn the socket could fail after the session
// started, which would be reported as a session that did not.
func TestStartOnTheDefaultServer(t *testing.T) {
	c, args, _ := stub(t, started)
	got, err := c.Start(context.Background(), nil, t.TempDir(), "shop", term.Launch{Process: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (term.Pane{Backend: Backend, Target: "%7", Socket: "/tmp/tmux-1/default"}); got != want {
		t.Fatalf("pane = %+v, want %+v", got, want)
	}
	b, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), "new-session") != 1 || strings.Contains(string(b), "display-message") {
		t.Fatalf("args = %q", b)
	}
}

// A socket path can hold a space.
func TestStartSocketWithASpace(t *testing.T) {
	c, _, _ := stub(t, "echo '%7 /tmp/my tmux/default'")
	got, err := c.Start(context.Background(), nil, t.TempDir(), "shop", term.Launch{Process: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Socket != "/tmp/my tmux/default" {
		t.Fatalf("pane = %+v", got)
	}
}

// A tmux server started here passes its environment to every pane it ever
// opens: the relay's secrets stay out of it.
func TestStartKeepsSecretsOutOfTheServer(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "123:secret")
	t.Setenv("TYPESAFE_API_KEY", "key-secret")
	c, _, env := stub(t, started)
	if _, err := c.Start(context.Background(), []term.Pane{{Backend: Backend, Socket: "/s"}}, t.TempDir(), "shop", term.Launch{Process: "claude"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(env)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secret") {
		t.Fatalf("a secret reached tmux: %s", b)
	}
}

func TestStartRefused(t *testing.T) {
	c, _, _ := stub(t, "echo 'no server' >&2; exit 1")
	_, err := c.Start(context.Background(), []term.Pane{{Backend: Backend, Socket: "/s"}}, t.TempDir(), "shop", term.Launch{Process: "claude"})
	if err == nil || !strings.Contains(err.Error(), "no server") {
		t.Fatalf("err = %v", err)
	}
}

func TestStartNoPaneID(t *testing.T) {
	c, _, _ := stub(t, "true")
	if _, err := c.Start(context.Background(), []term.Pane{{Backend: Backend, Socket: "/s"}}, t.TempDir(), "shop", term.Launch{Process: "claude"}); err == nil {
		t.Fatal("a session with no pane id must be an error")
	}
}

func TestStartNoSuchFolder(t *testing.T) {
	c, _, _ := stub(t, started)
	if _, err := c.Start(context.Background(), nil, filepath.Join(t.TempDir(), "gone"), "shop", term.Launch{Process: "claude"}); err == nil {
		t.Fatal("a folder that is gone must be an error")
	}
}

// TestStartLive opens panes on a real tmux: on a server of its own, and on
// the default server, whose socket it has to find out. It needs TG_LIVE=1 and
// tmux on the PATH.
func TestStartLive(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil || os.Getenv("TG_LIVE") != "1" {
		t.Skip("set TG_LIVE=1, with tmux on the PATH")
	}
	ctx := context.Background()
	t.Setenv("TMUX_TMPDIR", t.TempDir()) // the default server, for this test only
	c := &CLI{}
	for name, panes := range map[string][]term.Pane{
		"like pane":      {{Backend: Backend, Target: "%0", Socket: filepath.Join(t.TempDir(), "server")}},
		"default server": nil,
	} {
		t.Run(name, func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			// The name of a folder is never run, whatever it says. A "/" in it
			// makes folders inside folders, which is still one path.
			proof := filepath.Join(base, "ran")
			dir := filepath.Join(base, "demo#(touch "+proof+")")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				time.Sleep(time.Second) // tmux runs a format's job in the background
				if _, err := os.Stat(proof); err == nil {
					t.Error("the folder's name ran as a command")
				}
			})
			p, err := c.Start(ctx, panes, dir, "ignored", term.Launch{Process: "pwd; cat -v"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = c.run(ctx, p, "kill-server") })
			if p.Backend != Backend || p.Socket == "" || !strings.HasPrefix(p.Target, "%") {
				t.Fatalf("pane = %+v", p)
			}
			if !c.Exists(ctx, p) {
				t.Fatalf("pane %+v does not exist", p)
			}
			// Enter shows the key cat received: Down reached the pane as Down.
			if err := c.Type(ctx, p, term.Down); err != nil {
				t.Fatal(err)
			}
			if err := c.Type(ctx, p, term.Enter); err != nil {
				t.Fatal(err)
			}
			// A server that already runs opens the next pane in its folder too.
			other, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			q, err := c.Start(ctx, []term.Pane{p}, other, "ignored", term.Launch{Process: "pwd; cat"})
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				screen, err := c.Screen(ctx, p, 20)
				second, err2 := c.Screen(ctx, q, 20)
				// A long path wraps at the pane's width.
				flat := strings.ReplaceAll(screen, "\n", "")
				if err == nil && err2 == nil && strings.Contains(flat, dir) && strings.Contains(flat, "^[[B") && strings.Contains(strings.ReplaceAll(second, "\n", ""), other) {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("screen = %q, err = %v", screen, err)
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
	}
}
