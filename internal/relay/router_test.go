package relay

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/relay/relaytest"
	"github.com/IZZY-Agency/claude-tg/internal/session"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

const (
	me       = int64(123456789)
	sidShop  = "6449d606-55c1-4e6d-a915-4b14c777f57f" // acme-shop-92, idle, pane A1
	sidShopF = "46636a11-0000-4000-8000-000000000002" // acme-shop-fa, busy, pane A2
	sidBlog  = "1e3b8500-0000-4000-8000-000000000003" // blog-6a, idle, pane A3, followed
	sidOld   = "a0a0a0a0-0000-4000-8000-000000000004" // ended session
)

type fixture struct {
	api   *relaytest.API
	term  *relaytest.Term
	store *state.Store
	r     *Router
	now   time.Time
	slept time.Duration
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "st"))
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SaveSession(&state.Session{SessionID: sidShop, Name: "acme-shop-92", AgtermSessionID: "A1", LastMessageIDs: []int64{100, 101}})
	_ = st.SaveSession(&state.Session{SessionID: sidShopF, Name: "acme-shop-fa", AgtermSessionID: "A2", LastMessageIDs: []int64{110}})
	_ = st.SaveSession(&state.Session{SessionID: sidBlog, Name: "blog-6a", AgtermSessionID: "A3", Follow: true, LastMessageIDs: []int64{120}})
	_ = st.SaveSession(&state.Session{SessionID: sidOld, Name: "old-1", AgtermSessionID: "A9", LastMessageIDs: []int64{200}})
	idle, _ := os.ReadFile("../term/testdata/idle-screen.txt")
	perm, _ := os.ReadFile("../term/testdata/permission-screen.txt")
	f := &fixture{
		api:   &relaytest.API{},
		term:  &relaytest.Term{Screens: map[string]string{"A1": string(idle), "A2": string(idle), "A3": string(perm)}, Missing: map[string]bool{"A9": true}},
		store: st,
		now:   time.Date(2026, 10, 22, 21, 0, 0, 0, time.UTC),
	}
	f.r = &Router{
		API: f.api, Store: st, Term: f.term, ChatID: me, UserID: me,
		Agents: relaytest.AgentsOf(
			session.Agent{SessionID: sidShop, Name: "acme-shop-92", Kind: "interactive", Status: "idle", Cwd: "/Users/alex/code/acme-shop"},
			session.Agent{SessionID: sidShopF, Name: "acme-shop-fa", Kind: "interactive", Status: "busy", Cwd: "/Users/alex/code/acme-shop"},
			session.Agent{SessionID: sidBlog, Name: "blog-6a", Kind: "interactive", Status: "idle", Cwd: "/Users/alex/code/blog"},
		),
		Now: func() time.Time { return f.now }, Log: func(string, ...any) {}, StartedAt: time.Date(2026, 10, 22, 20, 0, 0, 0, time.UTC),
		// Virtual clock: sleeping advances Now so settle timers run without waiting.
		Sleep: func(_ context.Context, d time.Duration) { f.now = f.now.Add(d); f.slept += d },
	}
	return f
}

func (f *fixture) msg(text string, replyTo int64) telegram.Update {
	m := &telegram.Message{MessageID: 500, Date: f.now.Unix(), Chat: telegram.Chat{ID: me, Type: "private"}, From: &telegram.User{ID: me, FirstName: "Alex"}, Text: text}
	if replyTo != 0 {
		m.ReplyToMessage = &telegram.Message{MessageID: replyTo, Chat: m.Chat}
	}
	return telegram.Update{UpdateID: 1, Message: m}
}

func (f *fixture) handle(t *testing.T, u telegram.Update) {
	t.Helper()
	if err := f.r.Handle(context.Background(), u); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

// typedOnce asserts that exactly this text reached this session and nothing
// reached any other session.
func (f *fixture) typedOnce(t *testing.T, id, text string) {
	t.Helper()
	if got := f.term.TypedTexts(id); got != text {
		t.Fatalf("typed into %s: %q, want %q (all: %+v)", id, got, text, f.term.Typed)
	}
	for _, x := range f.term.Typed {
		if x.ID != id {
			t.Fatalf("typed into another session: %+v", f.term.Typed)
		}
	}
}

func TestDropsOtherSenders(t *testing.T) {
	f := newFixture(t)
	u := f.msg("hello", 100)
	u.Message.From.ID = 1
	f.handle(t, u)
	if len(f.term.Typed) != 0 || len(f.api.Sent) != 0 || len(f.api.Reactions) != 0 {
		t.Fatal("a stranger must get nothing")
	}
}

func TestReplyRoutesToSendingSession(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg("ship it", 101))
	f.typedOnce(t, "A1", " ship it\n")
	if len(f.api.Reactions) != 1 || f.api.Reactions[0].Emoji != "👍" || f.api.Reactions[0].MessageID != 500 {
		t.Fatalf("reactions=%+v", f.api.Reactions)
	}
}

func TestReplyToEndedSessionRefused(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg("hello?", 200))
	if len(f.term.Typed) != 0 || !strings.Contains(f.api.LastText(), "old-1") || !strings.Contains(f.api.LastText(), "ended") {
		t.Fatalf("typed=%+v last=%q", f.term.Typed, f.api.LastText())
	}
}

func TestPrefixExactAndUnique(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg("@acme-shop-92 run the tests", 0))
	f.typedOnce(t, "A1", " run the tests\n")
	f.term.Typed = nil
	f.term.Screens["A3"] = f.term.Screens["A1"] // blog-6a idle for this part
	f.handle(t, f.msg("blog-6a: check the queue", 0))
	f.typedOnce(t, "A3", " check the queue\n")
}

func TestPrefixAmbiguousShowsKeyboard(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg("@acme run it", 0))
	if len(f.term.Typed) != 0 {
		t.Fatal("ambiguous prefix must not type")
	}
	last := f.api.Sent[len(f.api.Sent)-1]
	if len(last.InlineKeyboard) < 2 || !strings.Contains(last.Text, "hich session") {
		t.Fatalf("keyboard not shown: %+v", last)
	}
}

func TestSingleFollowedIsDefault(t *testing.T) {
	f := newFixture(t)
	// blog-6a is the only followed session but its pane shows a permission prompt,
	// so switch it to idle for this test.
	f.term.Screens["A3"] = f.term.Screens["A1"]
	f.handle(t, f.msg("looks good, continue", 0))
	f.typedOnce(t, "A3", " looks good, continue\n")
}

func TestNoTargetShowsKeyboardAndCallbackDelivers(t *testing.T) {
	f := newFixture(t)
	_, _ = f.store.UpdateSession(sidBlog, func(s *state.Session) { s.Follow = false })
	f.handle(t, f.msg("please rerun", 0))
	if len(f.term.Typed) != 0 {
		t.Fatal("must not type without a target")
	}
	kb := f.api.Sent[len(f.api.Sent)-1]
	var data string
	for _, row := range kb.InlineKeyboard {
		for _, b := range row {
			if strings.HasPrefix(b.Text, "acme-shop-92") {
				data = b.Data
			}
		}
	}
	if data == "" || len(data) > 64 {
		t.Fatalf("no button for acme-shop-92: %+v", kb.InlineKeyboard)
	}
	cq := telegram.Update{UpdateID: 2, CallbackQuery: &telegram.CallbackQuery{ID: "cq1", From: &telegram.User{ID: me}, Message: &telegram.Message{MessageID: 1001, Chat: telegram.Chat{ID: me}}, Data: data}}
	f.handle(t, cq)
	f.typedOnce(t, "A1", " please rerun\n")
	if len(f.api.Edits) != 1 || !strings.Contains(f.api.Edits[0].Text, "acme-shop-92") || len(f.api.Answers) != 1 {
		t.Fatalf("edits=%+v answers=%+v", f.api.Edits, f.api.Answers)
	}
	// The owner's own message gets the same 👍 as a direct delivery.
	if want := []relaytest.Reaction{{MessageID: 500, Emoji: "👍"}}; !reflect.DeepEqual(f.api.Reactions, want) {
		t.Fatalf("reactions=%+v, want %+v", f.api.Reactions, want)
	}
}

// The session chooser's buttons carry session names: their texts are
// masked like every other text the relay sends; the callback data is not.
func TestSessionChooserMasksButtonTexts(t *testing.T) {
	const bot = "123456:ABCDEFsecretbot"
	f := newFixture(t)
	f.r.Secrets = []string{bot}
	f.r.Agents = relaytest.AgentsOf(
		session.Agent{SessionID: sidShop, Name: "bot" + bot, Kind: "interactive", Status: "idle", Cwd: "/Users/alex/code/acme-shop"},
		session.Agent{SessionID: sidShopF, Name: "deploy token=abcdefgh1234", Kind: "interactive", Status: "busy", Cwd: "/Users/alex/code/acme-shop"},
	)
	f.label(t, sidShop, "bot"+bot, "bot"+bot) // labels the hooks keep
	f.label(t, sidShopF, "deploy token=abcdefgh1234", "deploy token=abcdefgh1234")
	f.handle(t, f.msg("please rerun", 0))
	kb := f.api.Sent[len(f.api.Sent)-1].InlineKeyboard
	if len(kb) != 2 {
		t.Fatalf("keyboard = %+v", kb)
	}
	got := []string{kb[0][0].Text, kb[1][0].Text}
	if want := []string{"bot••• · idle", "deploy token=••• · busy"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("buttons = %q, want %q", got, want)
	}
	if !strings.HasPrefix(kb[0][0].Data, "s:"+sidShop+":") || !strings.HasPrefix(kb[1][0].Data, "s:"+sidShopF+":") {
		t.Fatalf("callback data changed: %+v", kb)
	}
}

func TestCallbackExpired(t *testing.T) {
	f := newFixture(t)
	_, _ = f.store.UpdateSession(sidBlog, func(s *state.Session) { s.Follow = false })
	f.handle(t, f.msg("please rerun", 0))
	kb := f.api.Sent[len(f.api.Sent)-1]
	data := kb.InlineKeyboard[0][0].Data
	f.now = f.now.Add(11 * time.Minute)
	cq := telegram.Update{UpdateID: 2, CallbackQuery: &telegram.CallbackQuery{ID: "cq1", From: &telegram.User{ID: me}, Message: &telegram.Message{MessageID: 1001, Chat: telegram.Chat{ID: me}}, Data: data}}
	f.handle(t, cq)
	if len(f.term.Typed) != 0 || len(f.api.Answers) != 1 || !strings.Contains(f.api.Answers[0].Text, "expired") {
		t.Fatalf("typed=%+v answers=%+v", f.term.Typed, f.api.Answers)
	}
}

func TestPermissionPromptBlocksText(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg("do it", 120)) // blog-6a shows the permission dialog
	if len(f.term.Typed) != 0 || !strings.Contains(f.api.LastText(), "/approve") {
		t.Fatalf("typed=%+v last=%q", f.term.Typed, f.api.LastText())
	}
}

func TestApproveTypesOneAndReacts(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg("/approve", 120))
	// A digit alone picks the option (spec fact 12); an Enter after it
	// could confirm the next queued dialog or submit a draft.
	f.typedOnce(t, "A3", "1")
	if len(f.api.Reactions) != 1 || f.api.Reactions[0].Emoji != "👌" {
		t.Fatalf("reactions=%+v", f.api.Reactions)
	}
}

func TestDenyTypesEscape(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg("/deny @blog", 0))
	f.typedOnce(t, "A3", "\x1b")
	if len(f.api.Reactions) != 1 || f.api.Reactions[0].Emoji != "👎" {
		t.Fatalf("reactions=%+v", f.api.Reactions)
	}
}

func TestApproveWithoutPromptRefused(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg("/approve", 101)) // acme-shop-92 is idle
	if len(f.term.Typed) != 0 || !strings.Contains(f.api.LastText(), "no permission prompt") {
		t.Fatalf("typed=%+v last=%q", f.term.Typed, f.api.LastText())
	}
}

func TestSessionsCommand(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg("/sessions", 0))
	out := f.api.LastText()
	for _, want := range []string{"acme-shop-92", "acme-shop-fa", "busy", "blog-6a", "follow on"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
	if strings.Contains(out, "old-1") {
		t.Fatal("ended sessions must not be listed")
	}
}

func TestFollowCommand(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg("/follow acme-shop-92", 0))
	s, _ := f.store.LoadSession(sidShop)
	if !s.Follow || !strings.Contains(plain(f.api.LastText()), "following acme-shop-92") {
		t.Fatalf("follow=%v last=%q", s.Follow, f.api.LastText())
	}
	f.handle(t, f.msg("/unfollow blog", 0))
	s, _ = f.store.LoadSession(sidBlog)
	if s.Follow || !strings.Contains(plain(f.api.LastText()), "stopped following blog-6a") {
		t.Fatalf("follow=%v last=%q", s.Follow, f.api.LastText())
	}
}

// /unfollow forgets the session's voice marks in the same write: a mark
// made while it was followed must not make the answer to a later typed
// swipe-reply spoken. /follow keeps them.
func TestUnfollowClearsVoiceMarks(t *testing.T) {
	f := newFixture(t)
	for _, sid := range []string{sidShop, sidBlog} {
		if _, err := f.store.UpdateSession(sid, func(s *state.Session) {
			s.MarkVoice("", "", f.now)
			s.MarkVoice(askP, "", f.now)
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.handle(t, f.msg("/follow acme-shop-92", 0))
	if s, _ := f.store.LoadSession(sidShop); !s.Follow || len(s.VoiceMarks) != 2 {
		t.Fatalf("follow=%v marks=%+v, want followed with its marks", s.Follow, s.VoiceMarks)
	}
	f.handle(t, f.msg("/unfollow blog", 0))
	if s, _ := f.store.LoadSession(sidBlog); s.Follow || len(s.VoiceMarks) != 0 {
		t.Fatalf("follow=%v marks=%+v, want unfollowed without marks", s.Follow, s.VoiceMarks)
	}
}

func TestBusySessionQueued(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg("and then deploy", 110)) // acme-shop-fa is busy
	f.typedOnce(t, "A2", " and then deploy\n")
	if f.api.LastText() != "queued in <b>acme-shop-fa</b> (busy)" {
		t.Fatalf("last=%q", f.api.LastText())
	}
}

func TestLongTextRefused(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg(strings.Repeat("x", 4001), 101))
	if len(f.term.Typed) != 0 || !strings.Contains(f.api.LastText(), "4000") {
		t.Fatalf("typed=%+v last=%q", f.term.Typed, f.api.LastText())
	}
}

func TestNewlinesJoined(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg("first line\nsecond line", 101))
	f.typedOnce(t, "A1", " first line second line\n")
}

// phoneLines is the session's recorded phone lines as stored.
func (f *fixture) phoneLines(t *testing.T, sessionID string) []state.PhoneLine {
	t.Helper()
	s, err := f.store.LoadSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return s.PhoneLines
}

// recordedOnce asserts the session holds exactly one phone line, the
// fingerprint of line, recorded at at, queued or not.
func (f *fixture) recordedOnce(t *testing.T, sessionID, line string, at time.Time, queued bool) {
	t.Helper()
	if l := f.phoneLines(t, sessionID); len(l) != 1 || l[0].Sum != state.PhoneSum(line) || !l[0].At.Equal(at) || l[0].Queued != queued {
		t.Fatalf("phone lines of %s = %+v, want one for %q (%s) at %v, queued %v", sessionID, l, line, state.PhoneSum(line), at, queued)
	}
}

// Every line the relay submits to a session is recorded in it (the line as
// typed, whitespace runs collapsed), once the Enter went out, so the hooks
// can tell a turn the phone started: a swipe-reply, a busy session's queued
// text (marked queued: it waits behind the running turn, ruling P15), a
// followed session's, one picked from the session list, a voice note's
// transcript (in the same write as its voice mark), and one whose Enter
// failed, as it may have gone through (ruling R30). A delivery that fails
// before its submit records nothing.
func TestDeliverRecordsPhoneLine(t *testing.T) {
	for _, c := range []struct {
		name   string
		sid    string
		line   string // "" when nothing is recorded
		queued bool   // the session was busy
		run    func(t *testing.T, f *fixture)
	}{
		{"swipe-reply", sidShop, "ship it now", false, func(t *testing.T, f *fixture) {
			f.handle(t, f.msg("ship\n  it   now", 101))
			f.typedOnce(t, "A1", " ship it now\n")
		}},
		{"busy session", sidShopF, "and then deploy", true, func(t *testing.T, f *fixture) {
			f.handle(t, f.msg("and then deploy", 110))
			f.typedOnce(t, "A2", " and then deploy\n")
		}},
		{"followed session", sidBlog, "looks good, continue", false, func(t *testing.T, f *fixture) {
			f.term.Screens["A3"] = f.term.Screens["A1"] // blog-6a idle
			f.handle(t, f.msg("looks good, continue", 0))
			f.typedOnce(t, "A3", " looks good, continue\n")
		}},
		{"picked from the session list", sidShop, "please rerun", false, func(t *testing.T, f *fixture) {
			_, _ = f.store.UpdateSession(sidBlog, func(s *state.Session) { s.Follow = false })
			f.handle(t, f.msg("please rerun", 0))
			kb := f.api.Sent[len(f.api.Sent)-1].InlineKeyboard
			var data string
			for _, row := range kb {
				for _, b := range row {
					if strings.HasPrefix(b.Text, "acme-shop-92") {
						data = b.Data
					}
				}
			}
			f.handle(t, tapUpdate(data, 1001))
			f.typedOnce(t, "A1", " please rerun\n")
		}},
		{"voice note", sidShop, "please rerun", false, func(t *testing.T, f *fixture) {
			f.r.Transcriber = &transcriber{text: "please   rerun"}
			f.api.Files = map[string][]byte{"voice-1": []byte(voiceAudio)}
			k, id := f.confirm(t, f.voice(7, 101))
			f.handle(t, tapUpdate("v:"+k+":y", id))
			f.typedOnce(t, "A1", " please rerun\n")
		}},
		{"submit unknown", sidShop, "ship it", false, func(t *testing.T, f *fixture) {
			f.r.Term = enterFailingTerm{Term: f.term}
			f.handle(t, f.msg("ship it", 101))
			f.typedOnce(t, "A1", " ship it")
		}},
		{"type error", sidShop, "", false, func(t *testing.T, f *fixture) {
			f.term.TypeErr = errors.New("agtermctl: timed out")
			f.handle(t, f.msg("ship it", 101))
		}},
		{"input never took the text", sidShop, "", false, func(t *testing.T, f *fixture) {
			f.term.MangleNext = true
			f.term.FailBackspace = errors.New("agtermctl: timed out")
			f.handle(t, f.msg("ship it", 101))
		}},
		{"refused: a dialog is open", sidShop, "", false, func(t *testing.T, f *fixture) {
			f.term.Screens["A1"] = termScreen(t, "permission-screen.txt")
			f.handle(t, f.msg("ship it", 101))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			typed := false
			var linesTyping []state.PhoneLine
			f.term.OnType = func(string, string) {
				if !typed {
					typed, linesTyping = true, f.phoneLines(t, c.sid)
				}
			}
			c.run(t, f)
			if len(linesTyping) != 0 {
				t.Fatalf("recorded before the submit: %+v", linesTyping)
			}
			if c.line == "" {
				if l := f.phoneLines(t, c.sid); len(l) != 0 {
					t.Fatalf("a failed delivery recorded %+v", l)
				}
				return
			}
			f.recordedOnce(t, c.sid, c.line, f.now, c.queued)
			if s, _ := f.store.LoadSession(c.sid); !s.SentFromPhone(" "+c.line, f.now.Add(time.Minute)) {
				t.Fatalf("the typed line is not known as the phone's: %+v", s.PhoneLines)
			}
			if c.name == "voice note" {
				f.markedOnce(t, c.sid, "", f.now)
			} else if m := f.marks(t, c.sid); len(m) != 0 {
				t.Fatalf("typed text marked for voice: %+v", m)
			}
		})
	}
}

func TestStatusAndHelp(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg("/status", 0))
	if !strings.Contains(f.api.LastText(), "up 1h") || !strings.Contains(f.api.LastText(), "3 live") {
		t.Fatalf("status=%q", f.api.LastText())
	}
	f.handle(t, f.msg("/help", 0))
	help := f.api.LastText()
	for _, want := range []string{"Reply to", "@name", "@acme", "/approve", "/deny", "Esc", "/follow", "/unfollow", "/sessions", "/status", "🔐", "💬", "👍", "👌", "👎", "which session", "pick a session in", "✖ Clear",
		`they type only into a plain Bash, Read or MCP "Do you want to proceed?" dialog`} {
		if !strings.Contains(help, want) {
			t.Fatalf("help lacks %q:\n%s", want, help)
		}
	}
	if len([]rune(help)) > 3000 {
		t.Fatalf("help is %d runes; keep it readable on a phone", len([]rune(help)))
	}
	f.handle(t, f.msg("/start", 0))
	if f.api.LastText() != help {
		t.Fatal("/start must show the same help")
	}
}

func TestParseCommandAndPrefix(t *testing.T) {
	if c, a := ParseCommand("/approve@acme_notify_bot  @blog "); c != "approve" || a != "@blog" {
		t.Fatalf("got %q %q", c, a)
	}
	names := []string{"acme-shop-92", "acme-shop-fa", "blog-6a"}
	if a := Address("@blog do it", names); a.Kind != AddrResolved || a.Name != "blog-6a" || a.Rest != "do it" {
		t.Fatalf("got %+v", a)
	}
	if a := Address("@acme do it", names); a.Kind != AddrAmbiguous {
		t.Fatalf("ambiguous @prefix: %+v", a)
	}
	if a := Address("@nothing do it", names); a.Kind != AddrUnknown {
		t.Fatalf("unknown @name: %+v", a)
	}
	if a := Address("blog-6a: do it", names); a.Kind != AddrResolved || a.Rest != "do it" {
		t.Fatalf("exact colon: %+v", a)
	}
	if a := Address("acme: deploy", names); a.Kind != AddrAmbiguous {
		t.Fatalf("colon prefix of several: %+v", a)
	}
	if a := Address("blog: deploy", names); a.Kind != AddrAmbiguous {
		t.Fatalf("colon prefix needs the exact name: %+v", a)
	}
	for _, plain := range []string{"plain text", "note: remember this", "https://example.com/x", "see: nothing"} {
		if a := Address(plain, names); a.Kind != AddrNone {
			t.Fatalf("%q must be plain text: %+v", plain, a)
		}
	}
}

func plain(html string) string {
	return strings.NewReplacer("<b>", "", "</b>", "").Replace(html)
}

// v2: delivering text or a decision arms a one-off answer back to the phone.
func TestDeliveryArmsFollowOnce(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg("ship it", 101))
	s, _ := f.store.LoadSession(sidShop)
	if s.PendingAnswers == 0 {
		t.Fatal("PendingAnswers not set after a delivered reply")
	}
	f.term.Screens["A2"] = f.term.Screens["A3"] // acme-shop-fa now shows a permission dialog
	f.handle(t, f.msg("/approve", 110))
	s, _ = f.store.LoadSession(sidShopF)
	if s.PendingAnswers == 0 {
		t.Fatal("PendingAnswers not set after /approve")
	}
	// A followed session needs no one-off counter.
	f.handle(t, f.msg("/approve", 120))
	s, _ = f.store.LoadSession(sidBlog)
	if s.PendingAnswers != 0 {
		t.Fatal("PendingAnswers must stay zero for a followed session")
	}
}

func TestRefusedDeliveryDoesNotArmFollowOnce(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg("do it", 120)) // refused: permission prompt
	s, _ := f.store.LoadSession(sidBlog)
	if s.PendingAnswers != 0 {
		t.Fatal("PendingAnswers must not be set when nothing was typed")
	}
}

// Review regressions.

func TestReplyToUnknownMessageRefused(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg("ship it", 999)) // no session sent message 999
	if len(f.term.Typed) != 0 || !strings.Contains(f.api.LastText(), "which session") {
		t.Fatalf("typed=%+v last=%q", f.term.Typed, f.api.LastText())
	}
	f.handle(t, f.msg("/approve", 999))
	if len(f.term.Typed) != 0 {
		t.Fatalf("approve fell through: %+v", f.term.Typed)
	}
}

func TestTextIsVerifiedBeforeEnter(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg("ship it", 101))
	if len(f.term.Typed) != 2 || f.term.Typed[0].Text != " ship it" || f.term.Typed[1].Text != "\n" {
		t.Fatalf("typed=%+v, want the text (space-prefixed) then Enter separately", f.term.Typed)
	}
}

// Enter in a dialog confirms its first option: a permission dialog of any
// tool, or a question picker, that pops up while the text is typed
// withholds it (ruling R28).
func TestDialogAppearingBeforeEnterBlocksSubmit(t *testing.T) {
	for _, name := range []string{"permission-screen.txt", "edit-dialog.txt", "webfetch-dialog.txt", "picker-single.txt", "plan-approval.txt"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			dialog := termScreen(t, name)
			f.term.OnType = func(id, text string) {
				if id == "A1" {
					f.term.Screens["A1"] = dialog // a dialog pops up right after the text was typed
				}
			}
			f.handle(t, f.msg("1 more thing", 101))
			for _, x := range f.term.Typed {
				if x.Text == "\n" {
					t.Fatal("Enter must not be sent when a dialog appeared")
				}
			}
			if last := f.api.LastText(); last != "a permission prompt or question appeared in <b>acme-shop-92</b> while typing; your text was not submitted. Answer it, then send the text again" {
				t.Fatalf("last=%q", last)
			}
			if len(f.api.Reactions) != 0 {
				t.Fatal("no 👍 for an unsubmitted text")
			}
		})
	}
}

// Ruling R28: text is never typed into a permission dialog of any tool or
// into a question picker, whether or not a waiting hook handles it. Real
// screens. /approve and /deny are offered only where they work: the strict
// Bash dialog (ruling R38).
func TestDialogsAndPickersRefuseTyping(t *testing.T) {
	const (
		prompt   = "<b>acme-shop-92</b> is waiting on a permission prompt; answer it on the Mac, or send /approve or /deny"
		other    = "<b>acme-shop-92</b> is waiting on a permission prompt; answer it on the Mac"
		question = "<b>acme-shop-92</b> is waiting on a question; answer it on the Mac"
		plan     = "<b>acme-shop-92</b> is waiting on a plan approval; answer it on the Mac"
	)
	cases := map[string]struct{ screen, want string }{
		"permission-screen.txt":                        {termScreen(t, "permission-screen.txt"), prompt},
		"bash-gutter-100.txt":                          {termScreen(t, "bash-gutter-100.txt"), prompt},
		"read-outside-dialog.txt":                      {termScreen(t, "read-outside-dialog.txt"), prompt},
		"mcp-dialog.txt":                               {termScreen(t, "mcp-dialog.txt"), prompt},
		"edit-dialog.txt":                              {termScreen(t, "edit-dialog.txt"), other},
		"write-dialog.txt":                             {termScreen(t, "write-dialog.txt"), other},
		"write-overwrite-dialog.txt":                   {termScreen(t, "write-overwrite-dialog.txt"), other},
		"webfetch-dialog.txt":                          {termScreen(t, "webfetch-dialog.txt"), other},
		"a plan option scrolled above the Bash dialog": {bashWithPlanAbove(t), other},
		"plan-approval.txt":                            {termScreen(t, "plan-approval.txt"), plan},
		"the plan at 80 columns with a question line":  {planAt80WithQuestion(t), plan},
		"picker-single.txt":                            {termScreen(t, "picker-single.txt"), question},
		"picker-single-typing.txt":                     {termScreen(t, "picker-single-typing.txt"), question},
		"picker-multi-q1.txt":                          {termScreen(t, "picker-multi-q1.txt"), question},
		"picker-preview.txt":                           {termScreen(t, "picker-preview.txt"), question},
		"a picker asking to proceed":                   {pickerAskingToProceed(t), question},
		// The review screen has no footer; a digit submits or cancels it.
		"picker-multi-review.txt":      {termScreen(t, "picker-multi-review.txt"), question},
		"picker-multi-review-none.txt": {termScreen(t, "picker-multi-review-none.txt"), question},
		"picker-multi-review-all.txt":  {termScreen(t, "picker-multi-review-all.txt"), question},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.term.Screens["A1"] = c.screen
			f.handle(t, f.msg("@acme-shop-92 run the tests", 0))
			if len(f.term.Typed) != 0 || f.api.LastText() != c.want {
				t.Fatalf("typed = %+v last = %q, want %q", f.term.Typed, f.api.LastText(), c.want)
			}
		})
	}
}

// A read that shows the picker's marks but does not hold a picker whole
// (its review screen cut to its last lines, its footer clipped at the
// pane's edge) is refused for typing all the same; so are pickers whose
// tab bar is clipped or scrolled, or whose top is above the read. The
// relay reads 60 lines, as the waiting hook does.
func TestPartialPickersRefuseTyping(t *testing.T) {
	const (
		partial  = "<b>acme-shop-92</b> shows a question tg can't read fully; answer it on the Mac"
		question = "<b>acme-shop-92</b> is waiting on a question; answer it on the Mac"
	)
	swap := func(s, old, new string) string {
		if !strings.Contains(s, old) {
			t.Fatalf("%q not on the screen", old)
		}
		return strings.Replace(s, old, new, 1)
	}
	tail := func(s string, n int) string {
		lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
		return strings.Join(lines[len(lines)-n:], "\n") + "\n"
	}
	const bar = "←  ☐ Colours  ☐ Pet  ✔ Submit  →"
	for name, c := range map[string]struct{ screen, want string }{
		"review cut":           {tail(termScreen(t, "picker-multi-review-none.txt"), 5), partial},
		"footer clipped":       {swap(termScreen(t, "picker-single.txt"), "Enter to select · ↑/↓ to navigate · Esc to cancel", "Enter to sel"), partial},
		"tab bar clipped":      {swap(termScreen(t, "picker-multi-q1.txt"), bar, "←  ☐ Colours  ☐ Pet  ✔ Sub"), question},
		"tab bar scrolled":     {swap(termScreen(t, "picker-multi-q2-untouched.txt"), bar, "←  ☐ Pet  ✔ Submit  →"), question},
		"taller than the read": {tail(termScreen(t, "picker-multi-q2-untouched.txt"), 11), question},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.term.Screens["A1"] = c.screen
			f.handle(t, f.msg("@acme-shop-92 run the tests", 0))
			if len(f.term.Typed) != 0 || f.api.LastText() != c.want {
				t.Fatalf("typed = %+v last = %q, want %q", f.term.Typed, f.api.LastText(), c.want)
			}
			for _, n := range f.term.ScreenLines {
				if n != 60 {
					t.Fatalf("read %v lines, want 60", f.term.ScreenLines)
				}
			}
		})
	}
}

// rewrap is the screen redrawn at width columns, as a narrower terminal
// draws it: a rule is cut, a longer text line breaks between words and
// continues with one space of indent (so the plan approval's question ends
// " Would you like to" / " proceed?").
func rewrap(s string, width int) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimRight(l, " ")
		r := []rune(l)
		switch {
		case len(r) <= width:
			out = append(out, l)
		case strings.Trim(strings.TrimSpace(l), "─╌") == "":
			out = append(out, string(r[:width]))
		default:
			line := ""
			for _, w := range strings.Fields(l) {
				if line != "" && len([]rune(line))+1+len([]rune(w)) > width {
					out = append(out, line)
					line = " " + w
				} else if line == "" {
					line = " " + w
				} else {
					line += " " + w
				}
			}
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// planAt80WithQuestion is the real plan approval, with a plan line that
// reads like a dialog's question, read at 80 columns: its question wraps
// before "proceed?".
func planAt80WithQuestion(t *testing.T) string {
	t.Helper()
	plan := strings.Replace(termScreen(t, "plan-approval.txt"), "\n Steps\n", "\n Do you want to keep the old flag? Open question.\n\n Steps\n", 1)
	w := rewrap(plan, 80)
	if !strings.Contains(w, "Would you like to\n proceed?\n") || !strings.Contains(w, "Do you want to keep the old flag?") {
		t.Fatalf("plan screen not as intended:\n%s", w)
	}
	return w
}

// pickerAskingToProceed is the real single-choice picker asking "Do you
// want to proceed?", with those words in the transcript above it too.
func pickerAskingToProceed(t *testing.T) string {
	t.Helper()
	s := termScreen(t, "picker-single.txt")
	out := strings.Replace(s, "\nWhich fruit do you want for the picker probe?\n", "\nDo you want to proceed?\n", 1)
	out = strings.Replace(out, "\n──", "\n⏺ I will ask with a picker: Do you want to proceed? Apple, Banana or Cherry.\n──", 1)
	if strings.Count(out, "Do you want to proceed?") != 2 {
		t.Fatal("picker screen not changed")
	}
	return out
}

// bashWithPlanAbove is the real Bash dialog with one of the plan approval's
// option lines still on the read above it: a plan marker anywhere in the
// read means nothing is typed (ruling R38).
func bashWithPlanAbove(t *testing.T) string {
	t.Helper()
	s := strings.Replace(termScreen(t, "permission-screen.txt"), "\n⏺ Skill(telegram-notify)\n", "\n   2. Yes, manually approve edits\n", 1)
	if !strings.Contains(s, "manually approve edits") {
		t.Fatal("Bash screen not changed")
	}
	return s
}

func termScreen(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "term", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestApproveRefusesWhenDialogChangesBetweenReads(t *testing.T) {
	f := newFixture(t)
	other := strings.ReplaceAll(f.term.Screens["A3"], "command -v tg && tg follow on", "rm -rf /")
	f.term.NextScreen = func(call int, id string) (string, bool) {
		if call == 2 {
			return other, true
		}
		return "", false
	}
	f.handle(t, f.msg("/approve", 120))
	if len(f.term.Typed) != 0 || !strings.Contains(f.api.LastText(), "changed") {
		t.Fatalf("typed=%+v last=%q", f.term.Typed, f.api.LastText())
	}
}

func TestColonPrefixShowsKeyboardNotFallthrough(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg("acme: deploy", 0))
	if len(f.term.Typed) != 0 || len(f.api.Sent[len(f.api.Sent)-1].InlineKeyboard) == 0 {
		t.Fatalf("typed=%+v sent=%+v", f.term.Typed, f.api.Sent)
	}
}

func TestCallbackCarriesFullSessionID(t *testing.T) {
	f := newFixture(t)
	twin := sidShop[:8] + "-ffff-4000-8000-000000000009" // shares the first 8 characters
	_ = f.store.SaveSession(&state.Session{SessionID: twin, Name: "twin-01", AgtermSessionID: "A7"})
	f.term.Screens["A7"] = f.term.Screens["A1"]
	f.r.Agents = relaytest.AgentsOf(
		session.Agent{SessionID: sidShop, Name: "acme-shop-92", Kind: "interactive", Status: "idle"},
		session.Agent{SessionID: twin, Name: "twin-01", Kind: "interactive", Status: "idle"},
	)
	f.handle(t, f.msg("please rerun", 0))
	kb := f.api.Sent[len(f.api.Sent)-1].InlineKeyboard
	var data string
	for _, row := range kb {
		for _, b := range row {
			if strings.HasPrefix(b.Text, "twin-01") {
				data = b.Data
			}
		}
	}
	if !strings.Contains(data, twin) || len(data) > 64 {
		t.Fatalf("button data %q must carry the full session id", data)
	}
	cq := telegram.Update{UpdateID: 2, CallbackQuery: &telegram.CallbackQuery{ID: "cq1", From: &telegram.User{ID: me}, Message: &telegram.Message{MessageID: 1001, Chat: telegram.Chat{ID: me}}, Data: data}}
	f.handle(t, cq)
	f.typedOnce(t, "A7", " please rerun\n")
}

func TestCallbackFromOtherChatDropped(t *testing.T) {
	f := newFixture(t)
	_, _ = f.store.UpdateSession(sidBlog, func(s *state.Session) { s.Follow = false })
	f.handle(t, f.msg("please rerun", 0))
	data := f.api.Sent[len(f.api.Sent)-1].InlineKeyboard[0][0].Data
	cq := telegram.Update{UpdateID: 2, CallbackQuery: &telegram.CallbackQuery{ID: "cq1", From: &telegram.User{ID: me}, Message: &telegram.Message{MessageID: 1001, Chat: telegram.Chat{ID: 42}}, Data: data}}
	f.handle(t, cq)
	if len(f.term.Typed) != 0 || len(f.api.Answers) != 0 || len(f.api.Edits) != 0 {
		t.Fatalf("callback from another chat acted: typed=%+v answers=%+v", f.term.Typed, f.api.Answers)
	}
}

func TestUnreadableSessionFileIsSkipped(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.store.Dir, "sessions", sidShopF+".json")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	f.handle(t, f.msg("/sessions", 0))
	if !strings.Contains(f.api.LastText(), "acme-shop-92") {
		t.Fatalf("listing failed: %q", f.api.LastText())
	}
}

func TestReactionFailureFallsBackToText(t *testing.T) {
	f := newFixture(t)
	f.api.ReactErr = errors.New("REACTION_INVALID")
	f.handle(t, f.msg("/approve", 120))
	if !strings.Contains(plain(f.api.LastText()), "approved blog-6a") {
		t.Fatalf("last=%q", f.api.LastText())
	}
}

func TestFollowOnceCountsQueuedTurn(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg("and then deploy", 110)) // acme-shop-fa is busy
	s, _ := f.store.LoadSession(sidShopF)
	if s.PendingAnswers != 2 {
		t.Fatalf("busy delivery: PendingAnswers=%d want 2", s.PendingAnswers)
	}
	f.handle(t, f.msg("ship it", 101)) // idle
	s, _ = f.store.LoadSession(sidShop)
	if s.PendingAnswers != 1 {
		t.Fatalf("idle delivery: PendingAnswers=%d want 1", s.PendingAnswers)
	}
}

func TestTypeFailureDisarmsPendingAnswer(t *testing.T) {
	f := newFixture(t)
	f.term.TypeErr = errors.New("socket closed")
	f.handle(t, f.msg("ship it", 101))
	s, _ := f.store.LoadSession(sidShop)
	if s.PendingAnswers != 0 {
		t.Fatalf("PendingAnswers=%d after a failed type", s.PendingAnswers)
	}
}

// Seen in the field on 2026-10-22: a typed line occasionally lands twice in the
// session's input before Enter. The relay must verify the input and repair it.
func TestDoubledInputIsRepairedBeforeEnter(t *testing.T) {
	f := newFixture(t)
	f.term.DoubleNext = true
	f.handle(t, f.msg("ship it", 101))
	if got := f.term.Submitted["A1"]; len(got) != 1 || got[0] != " ship it" {
		t.Fatalf("submitted=%q, want exactly one clean line", got)
	}
	if len(f.api.Reactions) != 1 || f.api.Reactions[0].Emoji != "👍" {
		t.Fatalf("reactions=%+v", f.api.Reactions)
	}
	var backspaces int
	for _, x := range f.term.Typed {
		backspaces += strings.Count(x.Text, "\x7f")
	}
	if backspaces == 0 {
		t.Fatal("the doubled input was not cleared with backspaces")
	}
}

func TestUnrepairableInputIsRefused(t *testing.T) {
	f := newFixture(t)
	f.term.MangleNext = true
	f.term.OnType = func(id, text string) {
		if text != "\n" && text != "\x7f" && !strings.Contains(text, "\x7f") {
			f.term.MangleNext = true // mangle every retype too
		}
	}
	f.handle(t, f.msg("ship it", 101))
	if got := f.term.Submitted["A1"]; len(got) != 0 {
		t.Fatalf("submitted=%q, want nothing", got)
	}
	if !strings.Contains(f.api.LastText(), "did not take") {
		t.Fatalf("last=%q", f.api.LastText())
	}
	if len(f.api.Reactions) != 0 {
		t.Fatal("no 👍 for an unsubmitted text")
	}
}

func TestInputVerifiedOnWrappedLine(t *testing.T) {
	long := strings.Repeat("word ", 30) + "end"
	region := InputText("❯  " + long[:60] + "\n" + long[60:] + "\n────────\n  status")
	if region != " "+long {
		t.Fatalf("InputText = %q", region)
	}
}

func TestPaneOfPrefersTmux(t *testing.T) {
	both := &state.Session{AgtermSessionID: "A1", TmuxPane: "%5", TmuxSocket: "/private/tmp/tmux-501/default"}
	if p := both.Pane(); p.Backend != "tmux" || p.Target != "%5" || p.Socket != "/private/tmp/tmux-501/default" {
		t.Fatalf("both: %+v", p)
	}
	if p := (&state.Session{AgtermSessionID: "A1"}).Pane(); p.Backend != "agterm" || p.Target != "A1" || p.Socket != "" {
		t.Fatalf("agterm only: %+v", p)
	}
	if p := (&state.Session{}).Pane(); !p.IsZero() {
		t.Fatalf("none: %+v", p)
	}
}

func TestTextRefusesSessionWithoutPane(t *testing.T) {
	f := newFixture(t)
	bare := "b0b0b0b0-0000-4000-8000-000000000005"
	_ = f.store.SaveSession(&state.Session{SessionID: bare, Name: "bare-01", LastMessageIDs: []int64{300}})
	f.r.Agents = relaytest.AgentsOf(session.Agent{SessionID: bare, Name: "bare-01", Kind: "interactive", Status: "idle"})
	f.handle(t, f.msg("hello", 300))
	last := f.api.Sent[len(f.api.Sent)-1]
	if len(f.term.Typed) != 0 || !strings.Contains(last.Text, "runs in a terminal I cannot type into (replies need agterm or tmux)") {
		t.Fatalf("typed=%+v reply=%q", f.term.Typed, last.Text)
	}
}

func TestTextReportsUnreachableTmuxPane(t *testing.T) {
	f := newFixture(t)
	gone := "c0c0c0c0-0000-4000-8000-000000000006"
	_ = f.store.SaveSession(&state.Session{SessionID: gone, Name: "gone-01", TmuxPane: "%7", TmuxSocket: "/private/tmp/tmux-501/default", LastMessageIDs: []int64{310}})
	f.term.Missing["%7"] = true
	f.r.Agents = relaytest.AgentsOf(session.Agent{SessionID: gone, Name: "gone-01", Kind: "interactive", Status: "idle"})
	f.handle(t, f.msg("hello", 310))
	last := f.api.Sent[len(f.api.Sent)-1]
	if len(f.term.Typed) != 0 || !strings.Contains(last.Text, "tmux pane %7 is not reachable (is tmux installed and the pane still open?)") {
		t.Fatalf("typed=%+v reply=%q", f.term.Typed, last.Text)
	}
}

func TestDeliverTypesIntoTmuxPane(t *testing.T) {
	f := newFixture(t)
	tm := "d0d0d0d0-0000-4000-8000-000000000007"
	_ = f.store.SaveSession(&state.Session{SessionID: tm, Name: "tmuxed-01", AgtermSessionID: "A1", TmuxPane: "%3", TmuxSocket: "/private/tmp/tmux-501/default", LastMessageIDs: []int64{320}})
	f.term.Screens["%3"] = f.term.Screens["A1"]
	f.r.Agents = relaytest.AgentsOf(session.Agent{SessionID: tm, Name: "tmuxed-01", Kind: "interactive", Status: "idle"})
	f.handle(t, f.msg("go on", 320))
	f.typedOnce(t, "%3", " go on\n")
	if len(f.term.Panes) == 0 || f.term.Panes[0].Backend != "tmux" || f.term.Panes[0].Socket != "/private/tmp/tmux-501/default" {
		t.Fatalf("pane handed to the terminal: %+v", f.term.Panes)
	}
}

// When the input cannot be cleared, nothing
// may be retyped or submitted.
func TestClearFailureStopsDelivery(t *testing.T) {
	f := newFixture(t)
	f.term.DoubleNext = true
	f.term.FailBackspace = errors.New("tmux send-keys: command too long")
	f.handle(t, f.msg("hello", 100))
	if n := len(f.term.Submitted["A1"]); n != 0 {
		t.Fatalf("submitted %d lines after a failed clear", n)
	}
	last := f.api.Sent[len(f.api.Sent)-1]
	if !strings.Contains(last.Text, "could not clear") {
		t.Fatalf("reply=%q", last.Text)
	}
	sess, _ := f.store.LoadSession(sidShop)
	if sess.PendingAnswers != 0 {
		t.Fatalf("pending answers not rolled back: %d", sess.PendingAnswers)
	}
}

// Seen live on both backends: Claude Code renders typed text with a delay,
// so a read right after typing shows only a prefix. The relay must wait for
// the line to settle instead of "repairing" text that is still arriving.
func TestInputReadBackWaitsForLaggingRender(t *testing.T) {
	f := newFixture(t)
	f.term.Lag = 3 // the next three reads after a Type show a 5-rune prefix
	f.handle(t, f.msg("Reply with exactly these words: v2 delivery ok", 100))
	if got := f.term.Submitted["A1"]; len(got) != 1 || got[0] != " Reply with exactly these words: v2 delivery ok" {
		t.Fatalf("submitted=%q, want exactly one clean line", got)
	}
	for _, x := range f.term.Typed {
		if strings.Contains(x.Text, "\x7f") {
			t.Fatalf("a lagging render was treated as a mismatch and cleared: %+v", f.term.Typed)
		}
	}
	if len(f.api.Reactions) != 1 || f.api.Reactions[0].Emoji != "👍" {
		t.Fatalf("reactions=%+v", f.api.Reactions)
	}
	if f.slept == 0 {
		t.Fatal("the relay never waited for the pane")
	}
}

// The relay's own replies are redacted between their tags, like every
// other text tg sends: an echoed secret is masked and the HTML stays valid.
func TestRepliesAreRedacted(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg("/follow password=s3cr3tvalue", 0))
	if got := f.api.LastText(); got != "no live session matches <b>password=[redacted]</b>" {
		t.Fatalf("reply = %q", got)
	}
}

// cantAnswer is /approve's and /deny's refusal for any dialog but the strict
// Bash dialog when no prompt of the session is on the phone (ruling R38).
const cantAnswer = "<b>blog-6a</b> shows a prompt tg can't answer from here; answer it on the Mac"

// Ruling R38: without a prompt on the phone, /approve and /deny type only
// into the strict v0.2 Bash dialog ("Do you want to proceed?" with the
// "Esc to cancel · Tab to amend" footer; the Read and MCP dialogs share the
// layout, ruling P17), read twice and the same both times: `1` alone
// (R37), or Esc. Every other screen is refused: Edit, Write and WebFetch
// dialogs, the plan approval
// (also wrapped at 80 columns, with a plan line that reads like a dialog's
// question), a question picker (also one asking "Do you want to
// proceed?"), and a Bash dialog with a plan option still on the read.
func TestApproveAndDenyTypeOnlyIntoTheBashDialog(t *testing.T) {
	for _, name := range []string{"permission-screen.txt", "bash-gutter-100.txt", "bash-heredoc-dialog.txt", "read-outside-dialog.txt", "mcp-dialog.txt"} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(termScreen(t, name), "❯ 1. Yes\n") {
				t.Fatalf("%s: option 1 is not Yes", name)
			}
			f := newFixture(t)
			f.term.Screens["A3"] = termScreen(t, name)
			f.handle(t, f.msg("/approve", 120))
			f.typedOnce(t, "A3", "1")
			f.term.Typed = nil
			f.handle(t, f.msg("/deny @blog", 0))
			f.typedOnce(t, "A3", "\x1b")
			if len(f.api.Reactions) != 2 || f.api.Reactions[0].Emoji != "👌" || f.api.Reactions[1].Emoji != "👎" {
				t.Fatalf("reactions=%+v", f.api.Reactions)
			}
		})
	}
	// Ruling P26: the Mac answers the first of two identical calls between
	// the reads; the second shows with the counter moved on.
	t.Run("an identical queued call comes up between the two reads", func(t *testing.T) {
		f := newFixture(t)
		first := termScreen(t, "mcp-queue-1of2.txt")
		f.term.Screens["A3"] = first
		next := strings.Replace(first, "1 of 2", "2 of 2", 1)
		f.term.NextScreen = func(call int, id string) (string, bool) { return next, call == 2 }
		for _, cmd := range []string{"/approve", "/deny @blog"} {
			f.handle(t, f.msg(cmd, 120))
			if len(f.term.Typed) != 0 || !strings.Contains(f.api.LastText(), "changed") {
				t.Fatalf("%s: typed=%+v last=%q", cmd, f.term.Typed, f.api.LastText())
			}
			f.term.ScreenCalls = 0
		}
	})
	t.Run("the dialog changes between the two reads", func(t *testing.T) {
		f := newFixture(t)
		other := termScreen(t, "bash-gutter-100.txt")
		f.term.NextScreen = func(call int, id string) (string, bool) { return other, call == 2 }
		f.handle(t, f.msg("/approve", 120))
		if len(f.term.Typed) != 0 || !strings.Contains(f.api.LastText(), "changed") {
			t.Fatalf("typed=%+v last=%q", f.term.Typed, f.api.LastText())
		}
	})
	refused := map[string]string{
		"edit-dialog.txt": termScreen(t, "edit-dialog.txt"), "edit-subdir-dialog.txt": termScreen(t, "edit-subdir-dialog.txt"),
		"write-dialog.txt": termScreen(t, "write-dialog.txt"), "write-overwrite-dialog.txt": termScreen(t, "write-overwrite-dialog.txt"),
		"webfetch-dialog.txt": termScreen(t, "webfetch-dialog.txt"), "plan-approval.txt": termScreen(t, "plan-approval.txt"),
		"the plan at 80 columns with a question line":  planAt80WithQuestion(t),
		"picker-single.txt":                            termScreen(t, "picker-single.txt"),
		"a picker asking to proceed":                   pickerAskingToProceed(t),
		"a plan option scrolled above the Bash dialog": bashWithPlanAbove(t),
		// A dialog whose top is above the read: its signature is only its
		// question, alike for any two such dialogs (ruling P26).
		"a Bash dialog with its top above the read": cropAbove(t, termScreen(t, "permission-screen.txt"), "\n Bash command\n"),
		"an MCP dialog with its top above the read": cropAbove(t, termScreen(t, "mcp-queue-1of2.txt"), "\n AccuWeather® — Current Weather Tool"),
	}
	for name, screen := range refused {
		for _, cmd := range []string{"/approve", "/deny @blog"} {
			t.Run(cmd+" on "+name, func(t *testing.T) {
				f := newFixture(t)
				f.term.Screens["A3"] = screen
				f.handle(t, f.msg(cmd, 120))
				if len(f.term.Typed) != 0 || f.api.LastText() != cantAnswer || len(f.api.Reactions) != 0 {
					t.Fatalf("typed=%+v last=%q reactions=%+v, want %q", f.term.Typed, f.api.LastText(), f.api.Reactions, cantAnswer)
				}
			})
		}
	}
}

// cropAbove is screen read from just after at, so the dialog's top border
// and title are above the read.
func cropAbove(t *testing.T, screen, at string) string {
	t.Helper()
	i := strings.Index(screen, at)
	if i < 0 {
		t.Fatalf("screen lacks %q", at)
	}
	return screen[i+len(at):]
}

// The /approve and /deny fallback answers from the phone: right before it
// types, one session write records when (PhoneAnswerAt, ruling P20), with
// the answer owed to the phone for an unfollowed session, so the waiting
// hook that then sees the dialog close does not take it for an answer at
// the Mac. A keystroke that fails takes both back.
func TestFallbackRecordsPhoneAnswer(t *testing.T) {
	for _, c := range []struct {
		name, sid, pane, cmd string
		replyTo              int64
		owed                 int
	}{
		{"followed session, /approve", sidBlog, "A3", "/approve", 120, 0},
		{"unfollowed session, /deny", sidShop, "A1", "/deny", 101, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.term.Screens[c.pane] = termScreen(t, "mcp-dialog.txt")
			var typing *state.Session
			f.term.OnType = func(string, string) { typing, _ = f.store.LoadSession(c.sid) }
			f.handle(t, f.msg(c.cmd, c.replyTo))
			if len(f.term.Typed) != 1 || typing == nil {
				t.Fatalf("typed=%+v", f.term.Typed)
			}
			if !typing.PhoneAnswerAt.Equal(f.now) || typing.PendingAnswers != c.owed {
				t.Fatalf("while typing: PhoneAnswerAt=%v owed=%d, want %v and %d", typing.PhoneAnswerAt, typing.PendingAnswers, f.now, c.owed)
			}
		})
	}
	t.Run("the keystroke fails", func(t *testing.T) {
		f := newFixture(t)
		earlier := f.now.Add(-time.Hour)
		_, _ = f.store.UpdateSession(sidShop, func(s *state.Session) { s.PhoneAnswerAt = earlier })
		f.term.Screens["A1"] = termScreen(t, "mcp-dialog.txt")
		f.term.TypeErr = errors.New("agtermctl: timed out")
		f.handle(t, f.msg("/approve", 101))
		s, _ := f.store.LoadSession(sidShop)
		if !s.PhoneAnswerAt.Equal(earlier) || s.PendingAnswers != 0 {
			t.Fatalf("after a failed keystroke: PhoneAnswerAt=%v owed=%d, want %v and 0", s.PhoneAnswerAt, s.PendingAnswers, earlier)
		}
	})
}

// A plan whose text has a line starting "Do you want to …" over its
// numbered options is still the plan approval: /approve would pick "Yes,
// and use auto mode". /approve, /deny and typed text are all refused, at
// the recorded width and at 80 columns, where the question wraps.
func TestPlanWithAQuestionLineIsNeverApproved(t *testing.T) {
	plan := strings.Replace(termScreen(t, "plan-approval.txt"), "\n Steps\n", "\n Do you want to keep the old flag? Open question.\n\n Steps\n", 1)
	const typing = "<b>blog-6a</b> is waiting on a plan approval; answer it on the Mac"
	for name, screen := range map[string]string{"recorded": plan, "80 columns": planAt80WithQuestion(t)} {
		for text, want := range map[string]string{"/approve": cantAnswer, "/deny": cantAnswer, "go ahead": typing} {
			f := newFixture(t)
			f.term.Screens["A3"] = screen
			f.handle(t, f.msg(text, 120))
			if len(f.term.Typed) != 0 || f.api.LastText() != want {
				t.Errorf("%s, %s: typed=%+v last=%q, want %q", name, text, f.term.Typed, f.api.LastText(), want)
			}
		}
	}
}

// Round 5: a pane narrower than the picker's footer wraps it; the picker
// is still refused for typing and by /approve and /deny, also when its
// option descriptions quote the Bash dialog's question and footer. The
// ticked multi-select picker has no prompt line above it: the fake
// terminal must leave its cursor line alone. An Edit dialog whose
// directory name holds the Bash strings is refused by /approve too.
func TestNarrowPickersAndInjectedStringsRefuse(t *testing.T) {
	const question = "<b>blog-6a</b> is waiting on a question; answer it on the Mac"
	quoting := strings.Replace(termScreen(t, "picker-single.txt"), "     A crisp red apple", "     Do you want to proceed?", 1)
	quoting = strings.Replace(quoting, "     A ripe yellow banana", "     Esc to cancel · Tab to amend", 1)
	ticked := termScreen(t, "picker-multi-q1-ticked.txt")
	if strings.HasPrefix(ticked, "❯") || !strings.Contains(ticked, "\n❯ 1. [✔] Red\n") {
		t.Fatal("the ticked picker is not as expected")
	}
	pickers := map[string]string{
		"single at 40": rewrap(termScreen(t, "picker-single.txt"), 40), "single at 46": rewrap(termScreen(t, "picker-single.txt"), 46),
		"multi at 56":   rewrap(termScreen(t, "picker-multi-q1.txt"), 56),
		"preview at 46": rewrap(termScreen(t, "picker-preview.txt"), 46), "preview at 50": rewrap(termScreen(t, "picker-preview.txt"), 50),
		"typing at 70":                 rewrap(termScreen(t, "picker-single-typing.txt"), 70),
		"ticked, no prompt line above": ticked, "ticked at 56": rewrap(ticked, 56),
		"quoting the Bash dialog at 46": rewrap(quoting, 46), "quoting the Bash dialog at 40": rewrap(quoting, 40),
	}
	for name, s := range pickers {
		t.Run(name, func(t *testing.T) {
			for text, want := range map[string]string{"go ahead": question, "/approve": cantAnswer, "/deny": cantAnswer} {
				f := newFixture(t)
				f.term.Screens["A3"] = s
				f.handle(t, f.msg(text, 120))
				if len(f.term.Typed) != 0 || f.api.LastText() != want {
					t.Errorf("%s: typed=%+v last=%q, want %q", text, f.term.Typed, f.api.LastText(), want)
				}
			}
		})
	}
	t.Run("an Edit with the Bash strings in its directory name", func(t *testing.T) {
		edit := strings.Replace(termScreen(t, "edit-outside-dialog.txt"), "      /Users/alex/scratch/fix2 for this session (shift+tab)",
			"      /Users/alex/scratch/fix2\nDo you want to proceed?\n Esc to cancel · Tab to amend\n      for this session (shift+tab)", 1)
		for _, cmd := range []string{"/approve", "/deny"} {
			f := newFixture(t)
			f.term.Screens["A3"] = edit
			f.handle(t, f.msg(cmd, 120))
			if len(f.term.Typed) != 0 || f.api.LastText() != cantAnswer {
				t.Errorf("%s: typed=%+v last=%q, want %q", cmd, f.term.Typed, f.api.LastText(), cantAnswer)
			}
		}
	})
}

// Typed text marks nothing: only a voice note's answer is spoken. Neither a
// swipe-reply nor the text keyboard leaves a voice mark, while both still
// arm the answer owed.
func TestTextDeliverDoesNotMark(t *testing.T) {
	f := newFixture(t)
	_, _ = f.store.UpdateSession(sidBlog, func(s *state.Session) { s.Follow = false })
	f.handle(t, f.msg("ship it", 101))
	f.handle(t, f.msg("check the queue", 0))
	data := ""
	for _, row := range f.api.Sent[len(f.api.Sent)-1].InlineKeyboard {
		if d := row[0].Data; strings.HasPrefix(d, "s:"+sidShopF+":") {
			data = d
		}
	}
	f.handle(t, tapUpdate(data, 1000+int64(len(f.api.Sent))))
	if f.term.TypedTexts("A1") != " ship it\n" || f.term.TypedTexts("A2") != " check the queue\n" {
		t.Fatalf("typed = %+v", f.term.Typed)
	}
	for _, c := range []struct {
		sid  string
		owed int
	}{{sidShop, 1}, {sidShopF, 2}} {
		if m := f.marks(t, c.sid); len(m) != 0 || f.pending(t, c.sid) != c.owed {
			t.Fatalf("%s: marks = %+v owed = %d, want none and %d", c.sid, m, f.pending(t, c.sid), c.owed)
		}
	}
}
