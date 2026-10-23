package relay

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	f.typedOnce(t, "A3", "1\n")
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

func TestBusySessionQueued(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg("and then deploy", 110)) // acme-shop-fa is busy
	f.typedOnce(t, "A2", " and then deploy\n")
	if !strings.Contains(f.api.LastText(), "queued") {
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

func TestStatusAndHelp(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg("/status", 0))
	if !strings.Contains(f.api.LastText(), "up 1h") || !strings.Contains(f.api.LastText(), "3 live") {
		t.Fatalf("status=%q", f.api.LastText())
	}
	f.handle(t, f.msg("/help", 0))
	help := f.api.LastText()
	for _, want := range []string{"Reply to", "@name", "@acme", "/approve", "/deny", "Esc", "/follow", "/unfollow", "/sessions", "/status", "🔐", "💬", "👍", "👌", "👎", "which session"} {
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

func TestDialogAppearingBeforeEnterBlocksSubmit(t *testing.T) {
	f := newFixture(t)
	perm := f.term.Screens["A3"]
	f.term.OnType = func(id, text string) {
		if id == "A1" {
			f.term.Screens["A1"] = perm // a dialog pops up right after the text was typed
		}
	}
	f.handle(t, f.msg("1 more thing", 101))
	for _, x := range f.term.Typed {
		if x.Text == "\n" {
			t.Fatal("Enter must not be sent when a dialog appeared")
		}
	}
	if !strings.Contains(f.api.LastText(), "not submitted") {
		t.Fatalf("last=%q", f.api.LastText())
	}
	if len(f.api.Reactions) != 0 {
		t.Fatal("no 👍 for an unsubmitted text")
	}
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
