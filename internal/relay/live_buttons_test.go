package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/agterm"
	"github.com/IZZY-Agency/claude-tg/internal/ask"
	"github.com/IZZY-Agency/claude-tg/internal/config"
	"github.com/IZZY-Agency/claude-tg/internal/render"
	"github.com/IZZY-Agency/claude-tg/internal/send"
	"github.com/IZZY-Agency/claude-tg/internal/session"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
	"github.com/IZZY-Agency/claude-tg/internal/term"
)

// TestLiveButtons answers the questions and permission prompts of a real
// Claude Code session from simulated taps, swipe-replies and a replied
// /approve. It needs TG_LIVE=1, TG_LIVE_SESSION_NAME=<name>, agterm (the
// test runs inside an agterm session) and the owner's tg config, which it
// only reads for the bot token and chat id. A run takes about two minutes;
// give it room:
//
//	TG_LIVE=1 TG_LIVE_SESSION_NAME=tg-live-buttons go test ./internal/relay/ -run TestLiveButtons -v -timeout 40m
//
// The test starts and quits its own session. In a temp dir it builds tg,
// writes a test config (away_after_seconds 0) and state dir, and a project
// <tmp>/<name> whose .claude/settings.json runs the built `tg hook
// permission-wait` against them as the PermissionRequest hook, next to a
// tiny hook that records the hook environment. It opens an agterm session
// named <name> running `claude --setting-sources project,local
// --permission-mode manual`, so the owner's own hooks stay out and the hook
// can read the session's pane. It holds the relay lock on the test state
// dir and records the "asks" feature in its relay.json, so the hook sees a
// relay that routes taps, and drives a Router built on the real Telegram
// client with synthetic updates: the owner's relay never sees them, but the
// hook's messages and every edit reach the owner's chat (eleven messages,
// each headed with the session's name). Toasts and reactions go to a
// recorder: a synthetic update has no real callback query or message to
// answer. The Router's terminal refuses to type: no answer may be typed.
// Each phone message's final state is checked with a no-op edit, which
// Telegram refuses as "message is not modified" only when the message
// already shows that text and no buttons; when it does not, that edit
// rewrites the message and the test says so.
func TestLiveButtons(t *testing.T) {
	name := os.Getenv("TG_LIVE_SESSION_NAME")
	if os.Getenv("TG_LIVE") != "1" || name == "" {
		t.Skip("set TG_LIVE=1 and TG_LIVE_SESSION_NAME")
	}
	lr := startLive(t, name)

	// 1. A Bash prompt approved by a tap on Yes.
	a := lr.permission("Run exactly this shell command and nothing else: touch approved.txt", "touch approved.txt")
	lr.checkAttended()
	lr.tap(a, 0, 'y', 0, "approved")
	lr.waitScreen("Allowed by PermissionRequest hook", time.Minute)
	lr.waitFile("approved.txt", true, time.Minute)
	lr.relayEdited(a, 0, "✅ approved from the phone")
	lr.waitAskGone(a, 30*time.Second)

	// 2. A Bash prompt denied by No: Claude stops, as after Esc, and does not
	// try the fallback the prompt offers it.
	a = lr.permission("Run exactly this shell command and nothing else: touch denied.txt — if that is refused, run touch fallback.txt instead.", "touch denied.txt")
	lr.tap(a, 0, 'n', 0, "denied")
	lr.relayEdited(a, 0, "❌ denied from the phone")
	lr.waitAskGone(a, 30*time.Second)
	lr.waitIdle()
	if s := lr.screen(); !strings.Contains(s, "Interrupted") && !strings.Contains(s, ask.NoMessage) {
		t.Errorf("no sign of the denial on screen:\n%s", s)
	}
	lr.noNewAsk(15 * time.Second)
	lr.waitFile("denied.txt", false, 0)
	lr.waitFile("fallback.txt", false, 0)

	// 3. /approve sent as a reply to the open 🔐 message decides that ask,
	// like ✅ Yes; nothing is typed.
	a = lr.permission("Run exactly this shell command and nothing else: touch replied.txt", "touch replied.txt")
	lr.command(a, 0, "/approve", "👌")
	lr.relayEdited(a, 0, "✅ approved from the phone")
	lr.waitScreen("Allowed by PermissionRequest hook", time.Minute)
	lr.waitFile("replied.txt", true, time.Minute)
	lr.waitAskGone(a, 30*time.Second)

	// 4. A question answered by a tap.
	a = lr.question(`Use the AskUserQuestion tool exactly once to ask me one single-choice question: question "Which fruit do you want?", header "Fruit", options "Apple", "Banana", "Cherry". After my answer, reply with exactly FRUIT=<my answer> and nothing else.`, 1)
	lr.tap(a, 0, 'o', lr.option(a, 0, "Banana"), "sent")
	lr.relayEdited(a, 0, "→ Banana")
	lr.waitScreen("FRUIT=Banana", 2*time.Minute)
	lr.waitAskGone(a, 30*time.Second)

	// 5. Two questions in one call: a multi-select answered with Done, then
	// a free-text swipe-reply.
	a = lr.question(`Use the AskUserQuestion tool exactly once, with two questions in that one call. First: question "Which colours do you like?", header "Colours", multiSelect true, options "Red", "Green", "Blue". Second: question "Which pet do you prefer?", header "Pet", single choice, options "Cat", "Dog". After my answers, reply with exactly COLOURS=<answer 1> PET=<answer 2> and nothing else.`, 2)
	lr.tap(a, 0, 'o', lr.option(a, 0, "Red"), "")
	lr.tap(a, 0, 'o', lr.option(a, 0, "Blue"), "")
	lr.keyboardTicked(a, 0, "✓ Red", "✓ Blue")
	lr.tap(a, 0, 'd', 0, "sent")
	lr.relayEdited(a, 0, "→ Red, Blue")
	lr.reply(a, 1, "a goldfish named tg-live")
	lr.relayEdited(a, 1, "→ a goldfish named tg-live")
	lr.waitScreen("COLOURS=Red, Blue", 2*time.Minute)
	lr.waitScreen("goldfish named tg-live", 10*time.Second)
	lr.waitAskGone(a, 30*time.Second)

	// 6. A suggestion saved to the project (localSettings): the rule lands in
	// .claude/settings.local.json and the same command runs again unasked.
	const py = "python3 -c 'print(42)'"
	a = lr.permission("Run exactly this shell command and nothing else: "+py, py)
	s := lr.suggestion(a, "localSettings")
	lr.tap(a, 0, 's', s.Index, "approved")
	lr.waitScreen("Allowed by PermissionRequest hook", time.Minute)
	lr.relayEdited(a, 0, "✅ approved from the phone: "+s.Label)
	lr.waitAskGone(a, 30*time.Second)
	lr.savedRule("Bash("+py+")", 10*time.Second)
	if again := lr.rerun("Run that exact same shell command again now: "+py, py); again != nil {
		t.Errorf("the saved rule did not stop the prompt for %s", py)
		lr.tap(again, 0, 'y', 0, "approved")
	}

	// 7. A suggestion for this session only: nothing is saved to the project.
	a = lr.permission("Run exactly this shell command and nothing else: touch session.txt", "touch session.txt")
	s = lr.suggestion(a, "session")
	lr.tap(a, 0, 's', s.Index, "approved")
	lr.waitScreen("Allowed by PermissionRequest hook", time.Minute)
	lr.waitFile("session.txt", true, time.Minute)
	lr.relayEdited(a, 0, "✅ approved from the phone: "+s.Label)
	lr.waitAskGone(a, 30*time.Second)
	// Running it again shows whether the grant covers the command itself.
	again := lr.rerun("Run that exact same shell command again now: touch session.txt", "touch session.txt")
	t.Logf("FACT: after the session suggestion %+v, the same command prompted again: %v", s, again != nil)
	if b, err := os.ReadFile(filepath.Join(lr.proj, ".claude", "settings.local.json")); err == nil && strings.Contains(string(b), "additionalDirectories") {
		t.Errorf("a session suggestion was saved to settings.local.json:\n%s", b)
	}

	// 8. Cancelled with Esc at the Mac: the phone message says so within 5 s.
	if again == nil {
		again = lr.permission("Run exactly this shell command and nothing else: touch esc.txt", "touch esc.txt")
	}
	lr.atMac(again, "\x1b", "answered at the Mac")
	lr.logToolResult(again)

	// 9. Answered at the Mac with 1.
	a = lr.permission("Run exactly this shell command and nothing else: touch mac.txt", "touch mac.txt")
	lr.atMac(a, "1", "answered at the Mac")
	lr.waitFile("mac.txt", true, time.Minute)

	// 10. The session quits while a prompt is open.
	a = lr.permission("Run exactly this shell command and nothing else: touch quit.txt", "touch quit.txt")
	closed := lr.closedLines()
	lr.quit()
	took := lr.waitAskGone(a, 15*time.Second)
	t.Logf("FACT: quit: the hook closed its ask %s after the tab closed", took.Round(10*time.Millisecond))
	lr.closedAs(closed, "ended")
	lr.phoneShows(a, 0, lr.hookEdit(a, 0, "session ended"))
	lr.noLeftovers()
	t.Logf("hook log:\n%s", lr.hookLog())
	t.Logf("toasts %q; reactions %q; %d edits; typed %q", lr.api.toasts, lr.api.reactions, len(lr.api.edits), lr.typer.typed)
	if len(lr.typer.typed) > 0 {
		t.Errorf("the Router tried to type into the session: %q", lr.typer.typed)
	}
}

// liveRun is one TestLiveButtons run: the throwaway session and the Router
// that plays the relay.
type liveRun struct {
	t          *testing.T
	ctx        context.Context
	dir, proj  string
	bin        string
	stateDir   string
	claudePath string
	chatID     int64
	client     *telegram.Client
	sender     *send.Sender // only for Clean, as the hook applies it
	api        *liveAPI
	st         *state.Store
	r          *Router
	tt         term.Terminal
	typer      *noTyping
	pane       term.Pane
	sid        string // the Claude session id, once the first ask names it
	seen       map[string]bool
	closed     bool
	nextMsg    int64
}

func startLive(t *testing.T, name string) *liveRun {
	t.Helper()
	owner, err := config.Load(config.DefaultPath())
	if err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lr := &liveRun{
		t: t, ctx: context.Background(), dir: dir, proj: filepath.Join(dir, name),
		bin: filepath.Join(dir, "tg"), stateDir: filepath.Join(dir, "state"),
		claudePath: owner.ClaudePath, chatID: owner.ChatID,
		client: telegram.New(owner.BotToken), seen: map[string]bool{}, nextMsg: 1 << 40,
		tt: &agterm.CLI{},
	}
	if lr.claudePath == "" {
		lr.claudePath = "claude"
	}
	lr.sender = &send.Sender{Secrets: []string{owner.BotToken}}
	lr.api = &liveAPI{Client: lr.client}

	build := exec.Command("go", "build", "-o", lr.bin, "github.com/IZZY-Agency/claude-tg/cmd/tg")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build tg: %v\n%s", err, out)
	}
	cfg := map[string]any{"bot_token": owner.BotToken, "chat_id": owner.ChatID, "claude_path": lr.claudePath, "away_after_seconds": 0}
	if owner.UserID != 0 {
		cfg["user_id"] = owner.UserID
	}
	cfgPath := filepath.Join(dir, "config.json")
	writeJSON(t, cfgPath, cfg)
	if lr.st, err = state.Open(lr.stateDir); err != nil {
		t.Fatal(err)
	}
	release, err := lr.st.LockRelay()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	// The hook sends buttons only to a relay that says it routes them.
	rs, err := lr.st.LoadRelay()
	if err != nil {
		t.Fatal(err)
	}
	rs.Features, rs.PID, rs.StartedAt = []string{state.FeatureAsks}, os.Getpid(), time.Now()
	if err := lr.st.SaveRelay(rs); err != nil {
		t.Fatal(err)
	}

	envHook := filepath.Join(dir, "hook-env.sh")
	script := "#!/bin/sh\ncat >/dev/null\nprintf 'CLAUDE_CODE_SESSION_ATTENDED=%s\\nCLAUDE_PID=%s\\n' \"$CLAUDE_CODE_SESSION_ATTENDED\" \"$CLAUDE_PID\" > " + shq(filepath.Join(dir, "hook-env.txt")) + "\n"
	if err := os.WriteFile(envHook, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	wait := fmt.Sprintf("env TG_CONFIG=%s TG_STATE_DIR=%s %s hook permission-wait", shq(cfgPath), shq(lr.stateDir), shq(lr.bin))
	settings := map[string]any{"hooks": map[string]any{"PermissionRequest": []any{map[string]any{"hooks": []any{
		map[string]any{"type": "command", "command": wait, "timeout": 43500},
		map[string]any{"type": "command", "command": shq(envHook), "timeout": 30},
	}}}}}
	if err := os.MkdirAll(filepath.Join(lr.proj, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(lr.proj, ".claude", "settings.json"), settings)
	if out, err := exec.Command("git", "-C", lr.proj, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	args := []string{"session", "new", "--cwd", lr.proj, "--name", name, "--no-select",
		"--command", fmt.Sprintf("%s -lc %s", shell, shq(lr.claudePath+" --setting-sources project,local --permission-mode manual --model sonnet"))}
	if after := os.Getenv("AGTERM_SESSION_ID"); after != "" {
		args = append(args, "--after", after)
	}
	out, err := exec.Command("agtermctl", args...).Output()
	if err != nil {
		t.Fatalf("agtermctl session new: %v", err)
	}
	lr.pane = term.Pane{Backend: "agterm", Target: strings.TrimSpace(string(out))}
	t.Logf("session %s in agterm %s, project %s", name, lr.pane.Target, lr.proj)
	t.Cleanup(func() {
		if !lr.closed {
			lr.quit()
		}
	})
	lr.typer = &noTyping{Terminal: lr.tt}
	lr.r = &Router{
		API: lr.api, Store: lr.st, Term: term.Mux{Backends: map[string]term.Terminal{"agterm": lr.typer}}, ChatID: owner.ChatID, UserID: owner.UserID,
		Agents: func(ctx context.Context) ([]session.Agent, error) { return session.ListAgents(ctx, lr.claudePath) },
		Log:    t.Logf, StartedAt: time.Now(), Secrets: []string{owner.BotToken},
	}

	// The folder-trust dialog: digits do nothing there; Down, then Enter.
	lr.waitScreen("trust this folder", time.Minute)
	lr.keys("\x1b[B")
	time.Sleep(500 * time.Millisecond)
	lr.keys("\r")
	lr.waitScreen("for shortcuts", time.Minute)
	return lr
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// shq quotes s for sh.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (lr *liveRun) keys(s string) {
	lr.t.Helper()
	if err := lr.tt.Type(lr.ctx, lr.pane, s); err != nil {
		lr.t.Fatalf("type %q: %v", s, err)
	}
}

func (lr *liveRun) screen() string {
	s, _ := lr.tt.Screen(lr.ctx, lr.pane, 60)
	return s
}

func (lr *liveRun) waitScreen(want string, within time.Duration) string {
	lr.t.Helper()
	deadline := time.Now().Add(within)
	for {
		s := lr.screen()
		if strings.Contains(s, want) {
			return s
		}
		if time.Now().After(deadline) {
			lr.t.Fatalf("%q never appeared on screen:\n%s", want, s)
		}
		time.Sleep(time.Second)
	}
}

// agent is the session's `claude agents` entry, found by session id once
// known, else by its folder.
func (lr *liveRun) agent() *session.Agent {
	agents, err := session.ListAgents(lr.ctx, lr.claudePath)
	if err != nil {
		return nil
	}
	for _, a := range agents {
		if lr.sid != "" && a.SessionID == lr.sid {
			return &a
		}
		if lr.sid == "" && a.Kind == "interactive" {
			if cwd, err := filepath.EvalSymlinks(a.Cwd); err == nil && cwd == lr.proj {
				return &a
			}
		}
	}
	return nil
}

// waitIdle waits until the session is between turns.
func (lr *liveRun) waitIdle() {
	lr.t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if a := lr.agent(); a != nil && a.Status == "idle" {
			time.Sleep(time.Second)
			return
		}
		time.Sleep(time.Second)
	}
	lr.t.Fatalf("the session never went idle:\n%s", lr.screen())
}

// send types a prompt once the session is idle.
func (lr *liveRun) send(text string) {
	lr.t.Helper()
	lr.waitIdle()
	lr.keys("\x15")
	lr.keys(text)
	time.Sleep(700 * time.Millisecond)
	lr.keys("\r")
}

// waitAsk returns the first ask not seen before once it has n messages sent.
func (lr *liveRun) waitAsk(n int, within time.Duration) *state.Ask {
	lr.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		asks, _ := lr.st.ListAsks()
		for _, a := range asks {
			if lr.seen[a.ID] || len(a.MessageIDs) < n {
				continue
			}
			lr.seen[a.ID] = true
			lr.sid = a.SessionID
			lr.t.Logf("ask %s (%s): messages %v, first: %q", a.ID, a.Tool, a.MessageIDs, a.Texts[0])
			return a
		}
		time.Sleep(500 * time.Millisecond)
	}
	lr.t.Fatalf("no ask with %d message(s) appeared:\n%s", n, lr.screen())
	return nil
}

// permission sends a prompt that runs command and returns its ask, once the
// dialog is on screen and the phone has the message.
func (lr *liveRun) permission(prompt, command string) *state.Ask {
	lr.t.Helper()
	lr.send(prompt)
	a := lr.waitAsk(1, 2*time.Minute)
	if a.Tool != "Bash" || !strings.Contains(a.Summary, command) {
		lr.t.Fatalf("ask is not for %q: %s %q", command, a.Tool, a.Summary)
	}
	lr.waitScreen("Do you want to proceed?", 30*time.Second)
	return a
}

func (lr *liveRun) question(prompt string, n int) *state.Ask {
	lr.t.Helper()
	lr.send(prompt)
	a := lr.waitAsk(1, 2*time.Minute)
	if a.Tool != "AskUserQuestion" {
		lr.t.Fatalf("ask is %s, not a question", a.Tool)
	}
	qs, err := ask.ParseQuestions(a.ToolInput)
	if err != nil || len(qs) != n {
		lr.t.Fatalf("want %d question(s), got %d (%v)", n, len(qs), err)
	}
	for len(a.MessageIDs) < n {
		time.Sleep(500 * time.Millisecond)
		if a, err = lr.st.LoadAsk(a.ID); err != nil {
			lr.t.Fatalf("ask gone before all %d messages were sent: %v", n, err)
		}
	}
	return a
}

func (lr *liveRun) option(a *state.Ask, q int, label string) int {
	lr.t.Helper()
	qs, _ := ask.ParseQuestions(a.ToolInput)
	for i, o := range qs[q].Options {
		if o.Label == label {
			return i
		}
	}
	lr.t.Fatalf("question %d has no option %q: %+v", q, label, qs[q].Options)
	return 0
}

// suggestion returns the offered suggestion with that destination.
func (lr *liveRun) suggestion(a *state.Ask, destination string) ask.Suggestion {
	lr.t.Helper()
	var list []map[string]any
	_ = json.Unmarshal(a.Suggestions, &list)
	lr.t.Logf("permission_suggestions: %s", a.Suggestions)
	for i, s := range ask.ParseSuggestions(a.Suggestions) {
		if list[s.Index]["destination"] == destination {
			// The button shows only the number; the message lists it.
			line := string(rune('①'+i)) + " " + render.EscapeHTML(s.Label)
			if !strings.Contains(a.Texts[0], line) {
				lr.t.Errorf("the message does not list %q:\n%s", line, a.Texts[0])
			}
			lr.t.Logf("offered: %q", s.Label)
			return s
		}
	}
	lr.t.Fatalf("no offered suggestion with destination %s", destination)
	return ask.Suggestion{}
}

func (lr *liveRun) user() *telegram.User { return &telegram.User{ID: lr.r.userID()} }

func (lr *liveRun) chat() telegram.Chat { return telegram.Chat{ID: lr.chatID, Type: "private"} }

// tap presses a button of message q of the ask and checks the toast ("" =
// any).
func (lr *liveRun) tap(a *state.Ask, q int, action byte, n int, toast string) {
	lr.t.Helper()
	lr.nextMsg++
	cq := &telegram.CallbackQuery{
		ID: fmt.Sprintf("live-%d", lr.nextMsg), From: lr.user(), Data: ask.TapData(a.ID, q, action, n),
		Message: &telegram.Message{MessageID: a.MessageIDs[q], Date: time.Now().Unix(), Chat: lr.chat()},
	}
	before := len(lr.api.toasts)
	if err := lr.r.Handle(lr.ctx, telegram.Update{CallbackQuery: cq}); err != nil {
		lr.t.Fatalf("tap %s: %v", cq.Data, err)
	}
	if len(lr.api.toasts) != before+1 {
		lr.t.Fatalf("tap %s: no toast", cq.Data)
	}
	if got := lr.api.toasts[before]; toast != "" && got != toast {
		lr.t.Fatalf("tap %s: toast %q, want %q", cq.Data, got, toast)
	}
}

// reply swipe-replies text to message q of the ask.
func (lr *liveRun) reply(a *state.Ask, q int, text string) {
	lr.t.Helper()
	lr.nextMsg++
	m := &telegram.Message{MessageID: lr.nextMsg, Date: time.Now().Unix(), Chat: lr.chat(), From: lr.user(), Text: text,
		ReplyToMessage: &telegram.Message{MessageID: a.MessageIDs[q], Chat: lr.chat()}}
	before := len(lr.api.reactions)
	if err := lr.r.Handle(lr.ctx, telegram.Update{Message: m}); err != nil {
		lr.t.Fatalf("reply: %v", err)
	}
	if len(lr.api.reactions) != before+1 {
		lr.t.Fatalf("reply to message %d was not taken as an answer (no 👍)", a.MessageIDs[q])
	}
}

// command sends text (a / command) as a reply to message q of the ask and
// checks the reaction it gets.
func (lr *liveRun) command(a *state.Ask, q int, text, reaction string) {
	lr.t.Helper()
	lr.nextMsg++
	m := &telegram.Message{MessageID: lr.nextMsg, Date: time.Now().Unix(), Chat: lr.chat(), From: lr.user(), Text: text,
		ReplyToMessage: &telegram.Message{MessageID: a.MessageIDs[q], Chat: lr.chat()}}
	before := len(lr.api.reactions)
	if err := lr.r.Handle(lr.ctx, telegram.Update{Message: m}); err != nil {
		lr.t.Fatalf("%s: %v", text, err)
	}
	if len(lr.api.reactions) != before+1 || lr.api.reactions[before] != reaction {
		lr.t.Fatalf("%s: reactions %q, want %q; replies %q", text, lr.api.reactions[before:], reaction, lr.api.sent)
	}
}

// relayEdited checks that the relay's last edit of message q went through
// and ends with the outcome line, and that the phone still shows it.
func (lr *liveRun) relayEdited(a *state.Ask, q int, outcome string) {
	lr.t.Helper()
	mid := a.MessageIDs[q]
	e, ok := lr.api.lastEdit(mid, false)
	if !ok {
		lr.t.Fatalf("message %d was never edited", mid)
	}
	if e.err != nil {
		lr.t.Fatalf("edit of message %d failed: %v", mid, e.err)
	}
	if !strings.HasSuffix(e.text, "<i>"+render.EscapeHTML(outcome)+"</i>") {
		lr.t.Fatalf("message %d edited to %q, want the outcome %q", mid, e.text, outcome)
	}
	lr.phoneShowsHTML(mid, e.text)
}

// keyboardTicked checks the last keyboard-only edit of message q.
func (lr *liveRun) keyboardTicked(a *state.Ask, q int, want ...string) {
	lr.t.Helper()
	e, ok := lr.api.lastEdit(a.MessageIDs[q], true)
	if !ok || e.err != nil {
		lr.t.Fatalf("no keyboard edit of message %d (err %v)", a.MessageIDs[q], e.err)
	}
	var texts []string
	for _, row := range e.kb {
		for _, b := range row {
			texts = append(texts, b.Text)
		}
	}
	for _, w := range want {
		if !slices.Contains(texts, w) {
			lr.t.Fatalf("keyboard %q lacks %q", texts, w)
		}
	}
}

// hookEdit is the text the hook edits message q to when it closes the ask
// with outcome.
func (lr *liveRun) hookEdit(a *state.Ask, q int, outcome string) string {
	return render.RedactHTML(lr.sender.CleanHTML(ask.WithOutcome(a.Texts[q], outcome)))
}

func (lr *liveRun) phoneShows(a *state.Ask, q int, html string) {
	lr.t.Helper()
	lr.phoneShowsHTML(a.MessageIDs[q], html)
}

// phoneShowsHTML checks that the message shows html and no buttons: the
// same edit again must be refused as "message is not modified".
func (lr *liveRun) phoneShowsHTML(mid int64, html string) {
	lr.t.Helper()
	lr.t.Logf("check: message %d should show %q and no buttons", mid, html)
	err := lr.client.EditMessage(lr.ctx, lr.chatID, mid, html, "HTML", nil)
	var ae *telegram.APIError
	switch {
	case err == nil:
		lr.t.Errorf("message %d did not show %q with no buttons; this check's edit has now rewritten the phone message to that text", mid, html)
	case errors.As(err, &ae) && strings.Contains(ae.Description, "message is not modified"):
	default:
		lr.t.Errorf("check message %d: %v", mid, err)
	}
}

// waitAskGone waits for the hook to remove its ask and returns how long it
// took.
func (lr *liveRun) waitAskGone(a *state.Ask, within time.Duration) time.Duration {
	lr.t.Helper()
	start := time.Now()
	for time.Since(start) < within {
		if _, err := lr.st.LoadAsk(a.ID); errors.Is(err, os.ErrNotExist) {
			return time.Since(start)
		}
		time.Sleep(200 * time.Millisecond)
	}
	lr.t.Fatalf("ask %s still there after %s:\n%s", a.ID, within, lr.screen())
	return 0
}

// atMac answers the ask's dialog at the Mac with keys; the phone message
// must show the outcome within 5 s.
func (lr *liveRun) atMac(a *state.Ask, keys, outcome string) {
	lr.t.Helper()
	closed := lr.closedLines()
	lr.keys(keys)
	took := lr.waitAskGone(a, 30*time.Second)
	lr.t.Logf("%q at the Mac: hook closed the ask after %s", keys, took.Round(100*time.Millisecond))
	if took > 5*time.Second {
		lr.t.Errorf("the hook noticed the answer at the Mac after %s, want 5 s", took)
	}
	lr.closedAs(closed, "mac")
	lr.phoneShows(a, 0, lr.hookEdit(a, 0, outcome))
}

// closedLines is the reasons in the hook log's "closed:" lines, in order.
func (lr *liveRun) closedLines() []string {
	var out []string
	for _, line := range strings.Split(lr.hookLog(), "\n") {
		if _, after, ok := strings.Cut(line, "): closed: "); ok {
			out = append(out, after)
		}
	}
	return out
}

// closedAs checks that the hook logged exactly one "closed:" line since
// before (closedLines taken before the action), with reason want.
func (lr *liveRun) closedAs(before []string, want string) {
	lr.t.Helper()
	now := lr.closedLines()
	switch {
	case len(now) != len(before)+1:
		lr.t.Errorf("the hook logged %d new closed: line(s), want 1: %q", len(now)-len(before), now[min(len(before), len(now)):])
	case now[len(now)-1] != want:
		lr.t.Errorf("the hook closed the ask as %q, want %q", now[len(now)-1], want)
	}
}

// rerun sends a prompt that runs command again and returns the new ask
// when it prompted, nil when the command ran without one.
func (lr *liveRun) rerun(prompt, command string) *state.Ask {
	lr.t.Helper()
	ran := lr.toolResults(command)
	lr.send(prompt)
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		asks, _ := lr.st.ListAsks()
		for _, a := range asks {
			if !lr.seen[a.ID] && len(a.MessageIDs) > 0 {
				lr.seen[a.ID] = true
				lr.waitScreen("Do you want to proceed?", 30*time.Second)
				return a
			}
		}
		if lr.toolResults(command) > ran {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	lr.t.Fatalf("%q neither ran nor prompted:\n%s", command, lr.screen())
	return nil
}

func (lr *liveRun) waitFile(name string, exists bool, within time.Duration) {
	lr.t.Helper()
	deadline := time.Now().Add(within)
	for {
		_, err := os.Stat(filepath.Join(lr.proj, name))
		if (err == nil) == exists {
			return
		}
		if time.Now().After(deadline) {
			lr.t.Fatalf("%s exists=%v, want %v", name, err == nil, exists)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// noNewAsk checks that no new ask appears for a while.
func (lr *liveRun) noNewAsk(within time.Duration) {
	lr.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		asks, _ := lr.st.ListAsks()
		for _, a := range asks {
			if !lr.seen[a.ID] {
				lr.t.Fatalf("Claude went on to another prompt: %s %q", a.Tool, a.Summary)
			}
		}
		time.Sleep(time.Second)
	}
}

// savedRule waits for .claude/settings.local.json to allow rule. Claude
// Code writes it once the hook has returned, and escapes the parentheses
// inside the rule's content.
func (lr *liveRun) savedRule(rule string, within time.Duration) {
	lr.t.Helper()
	open := strings.Index(rule, "(")
	escaped := rule[:open+1] + strings.NewReplacer("(", `\(`, ")", `\)`).Replace(rule[open+1:len(rule)-1]) + ")"
	path := filepath.Join(lr.proj, ".claude", "settings.local.json")
	deadline := time.Now().Add(within)
	for {
		var f struct {
			Permissions struct{ Allow []string } `json:"permissions"`
		}
		b, err := os.ReadFile(path)
		if err == nil && json.Unmarshal(b, &f) == nil {
			if i := slices.IndexFunc(f.Permissions.Allow, func(r string) bool { return r == rule || r == escaped }); i >= 0 {
				lr.t.Logf("FACT: settings.local.json allows %q", f.Permissions.Allow[i])
				return
			}
		}
		if time.Now().After(deadline) {
			lr.t.Errorf("settings.local.json does not allow %q (err %v):\n%s", rule, err, b)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (lr *liveRun) checkAttended() {
	lr.t.Helper()
	b, err := os.ReadFile(filepath.Join(lr.dir, "hook-env.txt"))
	if err != nil {
		lr.t.Fatalf("hook environment not recorded: %v", err)
	}
	lr.t.Logf("FACT: PermissionRequest hook environment:\n%s", b)
	if !strings.Contains(string(b), "CLAUDE_CODE_SESSION_ATTENDED=1\n") {
		lr.t.Errorf("CLAUDE_CODE_SESSION_ATTENDED is not 1 in the hook environment")
	}
}

// transcript is the session's transcript file.
func (lr *liveRun) transcript() string {
	home, _ := os.UserHomeDir()
	m, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", lr.sid+".jsonl"))
	if len(m) == 0 {
		return ""
	}
	return m[0]
}

type liveToolCall struct {
	ID      string
	Command string
	Result  string
	IsError bool
	HasRes  bool
}

// toolCalls reads the Bash calls of the transcript and their results.
func (lr *liveRun) toolCalls() []*liveToolCall {
	b, err := os.ReadFile(lr.transcript())
	if err != nil {
		return nil
	}
	var calls []*liveToolCall
	byID := map[string]*liveToolCall{}
	for _, line := range strings.Split(string(b), "\n") {
		var l struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &l) != nil || len(l.Message.Content) == 0 || l.Message.Content[0] != '[' {
			continue
		}
		var items []struct {
			Type      string                   `json:"type"`
			ID        string                   `json:"id"`
			Name      string                   `json:"name"`
			Input     struct{ Command string } `json:"input"`
			ToolUseID string                   `json:"tool_use_id"`
			Content   json.RawMessage          `json:"content"`
			IsError   bool                     `json:"is_error"`
		}
		if json.Unmarshal(l.Message.Content, &items) != nil {
			continue
		}
		for _, it := range items {
			switch {
			case it.Type == "tool_use" && it.Name == "Bash":
				c := &liveToolCall{ID: it.ID, Command: it.Input.Command}
				calls, byID[it.ID] = append(calls, c), c
			case it.Type == "tool_result" && byID[it.ToolUseID] != nil:
				c := byID[it.ToolUseID]
				c.HasRes, c.IsError = true, it.IsError
				var s string
				if json.Unmarshal(it.Content, &s) != nil {
					s = string(it.Content)
				}
				c.Result = s
			}
		}
	}
	return calls
}

// toolResults counts the results of Bash calls running command.
func (lr *liveRun) toolResults(command string) int {
	n := 0
	for _, c := range lr.toolCalls() {
		if strings.Contains(c.Command, command) && c.HasRes {
			n++
		}
	}
	return n
}

// logToolResult records what the transcript holds for the ask's call.
func (lr *liveRun) logToolResult(a *state.Ask) {
	lr.t.Helper()
	calls := lr.toolCalls()
	for i := len(calls) - 1; i >= 0; i-- {
		if c := calls[i]; strings.Contains(a.Summary, c.Command) {
			lr.t.Logf("FACT: transcript result for %q: has result %v, is_error %v: %q", c.Command, c.HasRes, c.IsError, c.Result)
			if !c.HasRes {
				lr.t.Errorf("no tool_result for %q", c.Command)
			}
			return
		}
	}
	lr.t.Errorf("no tool_use for %q in %s", a.Summary, lr.transcript())
}

// quit closes the agterm session, which ends Claude Code.
func (lr *liveRun) quit() {
	lr.closed = true
	if out, err := exec.Command("agtermctl", "session", "close", "--target", lr.pane.Target).CombinedOutput(); err != nil {
		lr.t.Logf("agtermctl session close: %v %s", err, out)
	}
}

// noLeftovers checks that neither Claude Code nor a waiting hook of this
// run is still running.
func (lr *liveRun) noLeftovers() {
	lr.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		out, _ := exec.Command("pgrep", "-fl", lr.bin+" hook permission-wait").Output()
		if len(out) == 0 && lr.agent() == nil {
			return
		}
		if time.Now().After(deadline) {
			lr.t.Errorf("left running after the session quit: %s (agent %+v)", out, lr.agent())
			return
		}
		time.Sleep(time.Second)
	}
}

func (lr *liveRun) hookLog() string {
	b, _ := os.ReadFile(filepath.Join(lr.stateDir, "tg.log"))
	return string(b)
}

// liveAPI is the real Telegram client, except that toasts and reactions,
// which synthetic updates cannot carry, are recorded instead of sent, and
// every edit is recorded with its outcome.
type liveAPI struct {
	*telegram.Client
	mu        sync.Mutex
	toasts    []string
	reactions []string
	sent      []string
	edits     []liveEdit
}

type liveEdit struct {
	id           int64
	text         string
	kb           [][]telegram.Button
	keyboardOnly bool
	err          error
}

func (a *liveAPI) AnswerCallbackQuery(_ context.Context, _, text string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.toasts = append(a.toasts, text)
	return nil
}

func (a *liveAPI) SetMessageReaction(_ context.Context, _, _ int64, emoji string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reactions = append(a.reactions, emoji)
	return nil
}

// SendMessage sends for real (the Router's replies reach the owner's chat)
// and records the text.
func (a *liveAPI) SendMessage(ctx context.Context, r telegram.SendRequest) (int64, error) {
	a.mu.Lock()
	a.sent = append(a.sent, r.Text)
	a.mu.Unlock()
	return a.Client.SendMessage(ctx, r)
}

// noTyping is the Router's terminal: it reads screens but refuses to type,
// recording what it was asked to type.
type noTyping struct {
	term.Terminal
	typed []string
}

func (n *noTyping) Type(_ context.Context, _ term.Pane, text string) error {
	n.typed = append(n.typed, text)
	return errors.New("live test: the Router must not type into the session")
}

func (a *liveAPI) EditMessage(ctx context.Context, chatID, messageID int64, text, parseMode string, kb [][]telegram.Button) error {
	err := a.Client.EditMessage(ctx, chatID, messageID, text, parseMode, kb)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.edits = append(a.edits, liveEdit{id: messageID, text: text, kb: kb, err: err})
	return err
}

func (a *liveAPI) EditReplyMarkup(ctx context.Context, chatID, messageID int64, kb [][]telegram.Button) error {
	err := a.Client.EditReplyMarkup(ctx, chatID, messageID, kb)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.edits = append(a.edits, liveEdit{id: messageID, kb: kb, keyboardOnly: true, err: err})
	return err
}

// lastEdit is the last text edit (or, with keyboardOnly, keyboard edit) of
// the message.
func (a *liveAPI) lastEdit(id int64, keyboardOnly bool) (liveEdit, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := len(a.edits) - 1; i >= 0; i-- {
		if e := a.edits[i]; e.id == id && e.keyboardOnly == keyboardOnly {
			return e, true
		}
	}
	return liveEdit{}, false
}
