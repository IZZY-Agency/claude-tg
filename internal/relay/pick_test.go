package relay

import (
	"context"
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

// tapPick taps the /sessions button that picks sessionID.
func (f *fixture) tapPick(t *testing.T, sessionID string) {
	t.Helper()
	f.handle(t, tapUpdate(pickData+sessionID, 1001))
}

// storedPick is the pick as the store holds it.
func (f *fixture) storedPick(t *testing.T) state.Pick {
	t.Helper()
	p, err := f.store.LoadPick()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// lastSent is the last message the relay sent.
func (f *fixture) lastSent(t *testing.T) telegram.SendRequest {
	t.Helper()
	if len(f.api.Sent) == 0 {
		t.Fatal("nothing sent")
	}
	return f.api.Sent[len(f.api.Sent)-1]
}

// idleBlog puts the followed blog-6a's pane at an idle prompt, so that
// text could go to it.
func (f *fixture) idleBlog() { f.term.Screens["A3"] = f.term.Screens["A1"] }

// /sessions keeps its list and adds a button per live session that has a
// pane, each picking its session, within Telegram's 64 bytes of callback
// data; with no pick there is no "📌 now" line and no ✖ Clear.
func TestSessionsOffersPickButtons(t *testing.T) {
	f := newFixture(t)
	const sidNoPane = "c0c0c0c0-0000-4000-8000-000000000005"
	_ = f.store.SaveSession(&state.Session{SessionID: sidNoPane, Name: "plain-1"})
	f.r.Agents = relaytest.AgentsOf(
		session.Agent{SessionID: sidShop, Name: "acme-shop-92", Kind: "interactive", Status: "idle", Cwd: "/Users/alex/code/acme-shop"},
		session.Agent{SessionID: sidShopF, Name: "acme-shop-fa", Kind: "interactive", Status: "busy", Cwd: "/Users/alex/code/acme-shop"},
		session.Agent{SessionID: sidBlog, Name: "blog-6a", Kind: "interactive", Status: "idle", Cwd: "/Users/alex/code/blog"},
		session.Agent{SessionID: sidNoPane, Name: "plain-1", Kind: "interactive", Status: "idle", Cwd: "/Users/alex/code/plain"},
	)
	f.handle(t, f.msg("/sessions", 0))
	last := f.lastSent(t)
	want := strings.Join([]string{
		"<b>acme-shop-92</b> · idle · follow off · acme-shop · agterm",
		"<b>acme-shop-fa</b> · busy · follow off · acme-shop · agterm",
		"<b>blog-6a</b> · idle · follow on · blog · agterm",
		"<b>plain-1</b> · idle · follow off · plain · no pane",
	}, "\n")
	if last.Text != want || last.ReplyToMessageID != 500 {
		t.Fatalf("text = %q (reply to %d), want %q", last.Text, last.ReplyToMessageID, want)
	}
	wantKB := [][]telegram.Button{
		{{Text: "acme-shop-92", Data: "p:" + sidShop}},
		{{Text: "acme-shop-fa", Data: "p:" + sidShopF}},
		{{Text: "blog-6a", Data: "p:" + sidBlog}},
	}
	if !reflect.DeepEqual(last.InlineKeyboard, wantKB) {
		t.Fatalf("keyboard = %+v, want %+v", last.InlineKeyboard, wantKB)
	}
	for _, row := range last.InlineKeyboard {
		if len(row[0].Data) > maxCallbackData {
			t.Fatalf("callback data over 64 bytes: %q", row[0].Data)
		}
	}
}

// Tapping a session's button picks it: the store holds the session and
// the time, and the message says where the next messages go, without its
// buttons. A button of a session that has ended meanwhile only says so.
func TestPickTapPicksAndEditsMessage(t *testing.T) {
	f := newFixture(t)
	f.handle(t, tapUpdate(pickData+sidOld, 1001))
	if p := f.storedPick(t); p != (state.Pick{}) || len(f.api.Edits) != 0 || len(f.api.Answers) != 1 || f.api.Answers[0].Text != "that session has ended" {
		t.Fatalf("ended session: pick=%+v edits=%+v answers=%+v", p, f.api.Edits, f.api.Answers)
	}
	f.api.Answers = nil
	f.tapPick(t, sidShop)
	if p := f.storedPick(t); p.SessionID != sidShop || !p.At.Equal(f.now) {
		t.Fatalf("pick = %+v, want %s at %v", p, sidShop, f.now)
	}
	want := []relaytest.Edit{{MessageID: 1001, Text: "📌 Next messages go to <b>acme-shop-92</b>"}}
	if !reflect.DeepEqual(f.api.Edits, want) {
		t.Fatalf("edits = %+v, want %+v", f.api.Edits, want)
	}
	if len(f.api.Answers) != 1 || f.api.Answers[0].Text != "next messages go to acme-shop-92" {
		t.Fatalf("answers = %+v", f.api.Answers)
	}
	if len(f.term.Typed) != 0 {
		t.Fatalf("a pick types nothing: %+v", f.term.Typed)
	}
}

// Plain text goes to the picked session, ahead of the single followed
// session.
func TestPlainTextGoesToPick(t *testing.T) {
	f := newFixture(t)
	f.idleBlog()
	f.tapPick(t, sidShop)
	f.handle(t, f.msg("run the tests", 0))
	f.typedOnce(t, "A1", " run the tests\n")
}

// A swipe-reply and @name go where they point, for that message only:
// the pick stays, and deliveries to other sessions leave its time alone.
// The next plain text goes to the pick again and refreshes it.
func TestReplyAndAddressOverridePickOnce(t *testing.T) {
	f := newFixture(t)
	f.idleBlog()
	f.tapPick(t, sidShop)
	picked := f.now
	f.now = f.now.Add(10 * time.Minute)
	f.handle(t, f.msg("and then deploy", 110)) // a reply to acme-shop-fa's message
	f.typedOnce(t, "A2", " and then deploy\n")
	f.term.Typed = nil
	f.handle(t, f.msg("@blog check the queue", 0))
	f.typedOnce(t, "A3", " check the queue\n")
	if p := f.storedPick(t); p.SessionID != sidShop || !p.At.Equal(picked) {
		t.Fatalf("pick = %+v, want %s at %v, untouched", p, sidShop, picked)
	}
	f.term.Typed = nil
	f.handle(t, f.msg("and the docs", 0))
	f.typedOnce(t, "A1", " and the docs\n")
	if p := f.storedPick(t); p.SessionID != sidShop || !p.At.Equal(f.now) {
		t.Fatalf("pick = %+v, want %s refreshed at %v", p, sidShop, f.now)
	}
}

// A voice note's transcript goes to the picked session, ahead of the
// single followed one, and its Send refreshes the pick.
func TestVoiceNoteGoesToPick(t *testing.T) {
	f := newFixture(t)
	f.idleBlog()
	f.r.Transcriber = &transcriber{text: "please rerun"}
	f.api.Files = map[string][]byte{"voice-1": []byte(voiceAudio)}
	f.tapPick(t, sidShop)
	f.now = f.now.Add(30 * time.Minute)
	k, id := f.confirm(t, f.voice(7, 0))
	if got := f.lastSent(t).Text; got != "🎙 to <b>acme-shop-92</b>:\nplease rerun" {
		t.Fatalf("confirmation = %q", got)
	}
	f.now = f.now.Add(time.Minute)
	f.handle(t, tapUpdate("v:"+k+":y", id))
	f.typedOnce(t, "A1", " please rerun\n")
	if p := f.storedPick(t); p.SessionID != sidShop || !p.At.Equal(f.now) {
		t.Fatalf("pick = %+v, want %s refreshed at %v", p, sidShop, f.now)
	}
}

// A pick lapses an hour after it was made or last used: a delivery
// through it restarts the hour; after an hour without one, plain text
// gets the keyboard again (no note: the session did not end) and the pick
// is gone.
func TestPickLapsesAfterAnHourUnlessUsed(t *testing.T) {
	f := newFixture(t)
	_, _ = f.store.UpdateSession(sidBlog, func(s *state.Session) { s.Follow = false })
	f.tapPick(t, sidShop)
	f.now = f.now.Add(50 * time.Minute)
	f.handle(t, f.msg("one", 0))
	f.typedOnce(t, "A1", " one\n")
	f.now = f.now.Add(59 * time.Minute) // 1h49m after the pick, 59m after "one"
	f.term.Typed = nil
	f.handle(t, f.msg("two", 0))
	f.typedOnce(t, "A1", " two\n")
	f.now = f.now.Add(61 * time.Minute)
	f.term.Typed = nil
	f.handle(t, f.msg("three", 0))
	if len(f.term.Typed) != 0 {
		t.Fatalf("a lapsed pick still routes: %+v", f.term.Typed)
	}
	if last := f.lastSent(t); last.Text != "Which session?" || len(last.InlineKeyboard) == 0 {
		t.Fatalf("last = %+v, want the keyboard", last)
	}
	if p := f.storedPick(t); p != (state.Pick{}) {
		t.Fatalf("a lapsed pick is kept: %+v", p)
	}
}

// A pick whose session has ended is dropped: the message gets the
// keyboard with a note, not the single followed session, and the next one
// goes by the usual rules.
func TestPickOfEndedSessionDropped(t *testing.T) {
	f := newFixture(t)
	f.idleBlog()
	f.tapPick(t, sidShop)
	f.r.Agents = relaytest.AgentsOf(
		session.Agent{SessionID: sidShopF, Name: "acme-shop-fa", Kind: "interactive", Status: "busy", Cwd: "/Users/alex/code/acme-shop"},
		session.Agent{SessionID: sidBlog, Name: "blog-6a", Kind: "interactive", Status: "idle", Cwd: "/Users/alex/code/blog"},
	)
	f.handle(t, f.msg("hello", 0))
	if len(f.term.Typed) != 0 {
		t.Fatalf("typed for an ended pick: %+v", f.term.Typed)
	}
	last := f.lastSent(t)
	if last.Text != "📌 <b>acme-shop-92</b> has ended\nWhich session?" || len(last.InlineKeyboard) != 2 {
		t.Fatalf("last = %+v, want the keyboard with the note", last)
	}
	if p := f.storedPick(t); p != (state.Pick{}) {
		t.Fatalf("pick of an ended session kept: %+v", p)
	}
	f.handle(t, f.msg("again", 0))
	f.typedOnce(t, "A3", " again\n")
}

// A voice note gets the same: the session keyboard under the note.
func TestVoicePickOfEndedSessionDropped(t *testing.T) {
	f := newFixture(t)
	f.idleBlog()
	f.r.Transcriber = &transcriber{text: "please rerun"}
	f.api.Files = map[string][]byte{"voice-1": []byte(voiceAudio)}
	f.tapPick(t, sidShop)
	f.r.Agents = relaytest.AgentsOf(
		session.Agent{SessionID: sidBlog, Name: "blog-6a", Kind: "interactive", Status: "idle", Cwd: "/Users/alex/code/blog"},
	)
	f.confirm(t, f.voice(7, 0))
	last := f.lastSent(t)
	if last.Text != "📌 <b>acme-shop-92</b> has ended\n🎙 Which session?\nplease rerun" || len(last.InlineKeyboard) != 2 || !strings.HasPrefix(last.InlineKeyboard[0][0].Data, "s:"+sidBlog+":") {
		t.Fatalf("confirmation = %+v, want the session keyboard under the note", last)
	}
	if p := f.storedPick(t); p != (state.Pick{}) {
		t.Fatalf("pick of an ended session kept: %+v", p)
	}
}

// While a pick is active, /sessions names it and offers ✖ Clear; the tap
// clears it, and plain text goes by the usual rules again. A pick of an
// ended session is dropped by /sessions and not shown.
func TestSessionsShowsPickAndClears(t *testing.T) {
	f := newFixture(t)
	f.idleBlog()
	f.tapPick(t, sidShop)
	f.handle(t, f.msg("/sessions", 0))
	last := f.lastSent(t)
	if !strings.HasSuffix(last.Text, "agterm\n\n📌 now: <b>acme-shop-92</b>") {
		t.Fatalf("text = %q", last.Text)
	}
	kb := last.InlineKeyboard
	if len(kb) != 4 || !reflect.DeepEqual(kb[3], []telegram.Button{{Text: "✖ Clear", Data: "p:-"}}) {
		t.Fatalf("keyboard = %+v", kb)
	}
	f.api.Edits, f.api.Answers = nil, nil
	f.handle(t, tapUpdate(kb[3][0].Data, 1002))
	if p := f.storedPick(t); p != (state.Pick{}) {
		t.Fatalf("pick after ✖ Clear = %+v", p)
	}
	if want := []relaytest.Edit{{MessageID: 1002, Text: "✖ Pick cleared"}}; !reflect.DeepEqual(f.api.Edits, want) {
		t.Fatalf("edits = %+v, want %+v", f.api.Edits, want)
	}
	if len(f.api.Answers) != 1 || f.api.Answers[0].Text != "cleared" {
		t.Fatalf("answers = %+v", f.api.Answers)
	}
	f.handle(t, f.msg("hello", 0))
	f.typedOnce(t, "A3", " hello\n")

	f.tapPick(t, sidShop)
	f.r.Agents = relaytest.AgentsOf(
		session.Agent{SessionID: sidBlog, Name: "blog-6a", Kind: "interactive", Status: "idle", Cwd: "/Users/alex/code/blog"},
	)
	f.handle(t, f.msg("/sessions", 0))
	last = f.lastSent(t)
	if strings.Contains(last.Text, "📌") || len(last.InlineKeyboard) != 1 {
		t.Fatalf("an ended pick is shown: %+v", last)
	}
	if p := f.storedPick(t); p != (state.Pick{}) {
		t.Fatalf("/sessions kept the pick of an ended session: %+v", p)
	}
}

// The pick is relay state on disk: a new router (a relay restart) keeps
// sending plain text to it.
func TestPickSurvivesRestart(t *testing.T) {
	f := newFixture(t)
	f.idleBlog()
	f.tapPick(t, sidShop)
	r := &Router{
		API: f.api, Store: f.store, Term: f.term, ChatID: me, UserID: me, Agents: f.r.Agents,
		Now: f.r.Now, Sleep: f.r.Sleep, Log: func(string, ...any) {}, StartedAt: f.now,
	}
	if err := r.Handle(context.Background(), f.msg("still there?", 0)); err != nil {
		t.Fatal(err)
	}
	f.typedOnce(t, "A1", " still there?\n")
}

// Every delivery to the picked session refreshes the pick, whatever route
// it took: a swipe-reply, @name, the "Which session?" keyboard, a voice
// note's Send.
func TestDeliveryToPickedSessionRefreshesPick(t *testing.T) {
	f := newFixture(t)
	f.r.Transcriber = &transcriber{text: "please rerun"}
	f.api.Files = map[string][]byte{"voice-1": []byte(voiceAudio)}
	f.tapPick(t, sidShop)
	refreshed := func(how string) {
		t.Helper()
		if p := f.storedPick(t); p.SessionID != sidShop || !p.At.Equal(f.now) {
			t.Fatalf("after %s: pick = %+v, want %s refreshed at %v", how, p, sidShop, f.now)
		}
		f.term.Typed = nil
		f.now = f.now.Add(10 * time.Minute)
	}
	f.now = f.now.Add(10 * time.Minute)
	f.handle(t, f.msg("ship it", 101)) // a reply to acme-shop-92's message
	f.typedOnce(t, "A1", " ship it\n")
	refreshed("a swipe-reply")
	f.handle(t, f.msg("@acme-shop-92 and test", 0))
	f.typedOnce(t, "A1", " and test\n")
	refreshed("@name")
	f.handle(t, f.msg("@acme rerun", 0)) // ambiguous: the keyboard
	var data string
	for _, row := range f.lastSent(t).InlineKeyboard {
		if strings.HasPrefix(row[0].Text, "acme-shop-92") {
			data = row[0].Data
		}
	}
	f.handle(t, tapUpdate(data, 1001))
	f.typedOnce(t, "A1", " @acme rerun\n")
	refreshed("the session keyboard")
	k, id := f.confirm(t, f.voice(7, 101))
	f.handle(t, tapUpdate("v:"+k+":y", id))
	f.typedOnce(t, "A1", " please rerun\n")
	refreshed("a voice note's Send")
}

// A message the pick routed at 59m59s whose typing ends after the hour
// still refreshes the pick: a delivery to the session the pick names
// refreshes it whatever its age.
func TestPickRefreshedWhenHourRunsOutWhileTyping(t *testing.T) {
	f := newFixture(t)
	f.tapPick(t, sidShop)
	picked := f.now
	f.now = picked.Add(time.Hour - time.Second)
	f.term.OnType = func(string, string) { f.now = f.now.Add(2 * time.Second) }
	f.handle(t, f.msg("last one", 0))
	f.typedOnce(t, "A1", " last one\n")
	if !f.now.After(picked.Add(time.Hour)) {
		t.Fatalf("the delivery ended at %v, within the hour", f.now.Sub(picked))
	}
	if p := f.storedPick(t); p.SessionID != sidShop || !p.At.Equal(f.now) {
		t.Fatalf("pick = %+v, want %s refreshed at %v", p, sidShop, f.now)
	}
}

// A pick that cannot be read (a malformed file, one without a time, a
// read error) never lets
// a message go elsewhere: text and voice notes are refused with the
// reason and nothing is typed. /sessions reports it with ✖ Clear, and a
// new pick or ✖ Clear replaces it.
func TestUnreadablePickRefusesMessages(t *testing.T) {
	for _, c := range []struct {
		name   string
		damage func(t *testing.T, path string)
	}{
		{"malformed", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"no time", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(`{"session_id":"`+sidShop+`"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"read error", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.idleBlog() // the single followed session, which must not get it
			f.r.Transcriber = &transcriber{text: "please rerun"}
			f.api.Files = map[string][]byte{"voice-1": []byte(voiceAudio)}
			c.damage(t, filepath.Join(f.store.Dir, "pick.json"))
			f.handle(t, f.msg("hello", 0))
			f.handleVoice(t, f.voice(7, 0))
			if len(f.term.Typed) != 0 {
				t.Fatalf("typed with an unreadable pick: %+v", f.term.Typed)
			}
			texts := f.api.Texts()
			if len(texts) != 2 {
				t.Fatalf("sent = %q, want two refusals", texts)
			}
			for _, text := range texts {
				if !strings.HasPrefix(text, "cannot read the session you picked: ") || !strings.HasSuffix(text, "; pick again with /sessions") {
					t.Fatalf("refusal = %q", text)
				}
			}
			for _, sent := range f.api.Sent {
				if len(sent.InlineKeyboard) != 0 {
					t.Fatalf("a refusal with buttons: %+v", sent)
				}
			}
			f.handle(t, f.msg("/sessions", 0))
			last := f.lastSent(t)
			kb := last.InlineKeyboard
			if !strings.Contains(last.Text, "\n\n📌 cannot read the session you picked: ") || len(kb) != 4 || kb[3][0].Data != pickClear {
				t.Fatalf("/sessions = %+v, want the reason and ✖ Clear", last)
			}
			if c.name == "malformed" {
				f.tapPick(t, sidShop)
				f.handle(t, f.msg("hello again", 0))
				f.typedOnce(t, "A1", " hello again\n")
				return
			}
			f.handle(t, tapUpdate(pickClear, 1002))
			f.handle(t, f.msg("hello again", 0))
			f.typedOnce(t, "A3", " hello again\n")
		})
	}
}

// The ended-session note cuts the name only after the configured secrets
// are masked and credentials redacted, so a cut never leaves a secret's
// start behind.
func TestEndedNoteMasksBeforeCutting(t *testing.T) {
	const bot = "123456:ABCDEFsecretbot"
	for _, c := range []struct{ name, label, leak string }{
		{"secret", strings.Repeat("x", 30) + bot, "123456"},
		{"credential", strings.Repeat("y", 30) + " token=abcdefgh1234", "token=ab"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.r.Secrets = []string{bot}
			f.tapPick(t, sidShop)
			_, _ = f.store.UpdateSession(sidShop, func(s *state.Session) { s.Name = c.label })
			f.r.Agents = relaytest.AgentsOf(
				session.Agent{SessionID: sidBlog, Name: "blog-6a", Kind: "interactive", Status: "idle", Cwd: "/Users/alex/code/blog"},
			)
			f.handle(t, f.msg("hello", 0))
			text := f.lastSent(t).Text
			if !strings.HasPrefix(text, "📌 <b>") || !strings.Contains(text, "has ended") || strings.Contains(text, c.leak) {
				t.Fatalf("note = %q leaks %q", text, c.leak)
			}
		})
	}
}

// With no live session, /sessions still reports a pick that cannot be
// read, with ✖ Clear, above "no live sessions"; the tap clears it.
func TestSessionsReportsUnreadablePickWithoutSessions(t *testing.T) {
	f := newFixture(t)
	f.r.Agents = relaytest.AgentsOf()
	path := filepath.Join(f.store.Dir, "pick.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.handle(t, f.msg("/sessions", 0))
	last := f.lastSent(t)
	if !strings.HasPrefix(last.Text, "📌 cannot read the session you picked: ") || !strings.HasSuffix(last.Text, "; pick again with /sessions\nno live sessions") {
		t.Fatalf("text = %q", last.Text)
	}
	if want := [][]telegram.Button{{{Text: "✖ Clear", Data: pickClear}}}; !reflect.DeepEqual(last.InlineKeyboard, want) {
		t.Fatalf("keyboard = %+v, want %+v", last.InlineKeyboard, want)
	}
	f.handle(t, tapUpdate(pickClear, 1002))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("✖ Clear left the unreadable pick: %v", err)
	}
}
