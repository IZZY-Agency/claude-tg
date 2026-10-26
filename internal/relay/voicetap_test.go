package relay

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/IZZY-Agency/claude-tg/internal/relay/relaytest"
	"github.com/IZZY-Agency/claude-tg/internal/render"
	"github.com/IZZY-Agency/claude-tg/internal/session"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
	"github.com/IZZY-Agency/claude-tg/internal/term"
)

// confirm hands the note to the router (Task 4's path) and returns the
// confirmation's parking key and message id.
func (f *fixture) confirm(t *testing.T, u telegram.Update) (string, int64) {
	t.Helper()
	n := len(f.api.Sent)
	f.handleVoice(t, u)
	if len(f.api.Sent) != n+1 {
		t.Fatalf("no confirmation: sent = %+v", f.api.Sent)
	}
	return confirmKey(t, f.api.Sent[n].InlineKeyboard), 1000 + int64(n+1)
}

// peekParked reads the transcript parked under key without taking it.
func (f *fixture) peekParked(t *testing.T, key string) state.Parked {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.store.Dir, "pending", key+".json"))
	if err != nil {
		t.Fatalf("nothing parked under %s: %v", key, err)
	}
	var p state.Parked
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// editsOf returns the edits of message id, in order.
func (f *fixture) editsOf(id int64) []relaytest.Edit {
	var out []relaytest.Edit
	for _, e := range f.api.Edits {
		if e.MessageID == id {
			out = append(out, e)
		}
	}
	return out
}

// knownKeyboard is a known-target confirmation's keyboard.
func knownKeyboard(k string) [][]telegram.Button {
	return [][]telegram.Button{
		{{Text: "✅ Send", Data: "v:" + k + ":y"}, {Text: "✖️ Cancel", Data: "v:" + k + ":n"}},
		{{Text: "↪ Other session", Data: "v:" + k + ":o"}},
	}
}

// whichKeyboard is the no-target confirmation's keyboard of the fixture's
// three sessions.
func whichKeyboard(k string) [][]telegram.Button {
	return [][]telegram.Button{
		{{Text: "acme-shop-92 · idle", Data: "s:" + sidShop + ":" + k}},
		{{Text: "acme-shop-fa · busy", Data: "s:" + sidShopF + ":" + k}},
		{{Text: "blog-6a · idle", Data: "s:" + sidBlog + ":" + k}},
		{{Text: "✖️ Cancel", Data: "v:" + k + ":n"}},
	}
}

var (
	reactSeen  = []relaytest.Reaction{{MessageID: 500, Emoji: "👀"}}
	reactSent  = []relaytest.Reaction{{MessageID: 500, Emoji: "👀"}, {MessageID: 500, Emoji: "👍"}}
	reactClear = []relaytest.Reaction{{MessageID: 500, Emoji: "👀"}, {MessageID: 500, Emoji: ""}}
)

func (f *fixture) reactions(t *testing.T, want []relaytest.Reaction) {
	t.Helper()
	if !reflect.DeepEqual(f.api.Reactions, want) {
		t.Fatalf("reactions = %+v, want %+v", f.api.Reactions, want)
	}
}

func (f *fixture) toastsAre(t *testing.T, want ...string) {
	t.Helper()
	if got := f.toasts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("toasts = %q, want %q", got, want)
	}
}

// editAt returns edit i, failing the test when there are fewer.
func (f *fixture) editAt(t *testing.T, i int) relaytest.Edit {
	t.Helper()
	if i >= len(f.api.Edits) {
		t.Fatalf("no edit %d: edits = %+v", i, f.api.Edits)
	}
	return f.api.Edits[i]
}

func sameEdit(t *testing.T, got, want relaytest.Edit) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("edit = %+v\nwant   %+v", got, want)
	}
}

func TestVoiceSendDelivers(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "please rerun"})
	k, id := f.confirm(t, f.voice(7, 101))
	f.handle(t, tapUpdate("v:"+k+":y", id))
	f.typedOnce(t, "A1", " please rerun\n")
	f.reactions(t, reactSent)
	if len(f.api.Edits) != 1 {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	sameEdit(t, f.editAt(t, 0), relaytest.Edit{MessageID: id, Text: "→ <b>acme-shop-92</b>\nplease rerun"})
	f.toastsAre(t, "sent to acme-shop-92")
	if f.parked(t) != 0 {
		t.Fatal("a delivered transcript is still parked")
	}
}

// A busy session queues the text; the outcome says so, as for typed text,
// without naming the session a second time (ruling R26).
func TestVoiceSendDeliveredWithNote(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "please rerun"})
	k, id := f.confirm(t, f.voice(7, 110)) // acme-shop-fa, busy
	f.handle(t, tapUpdate("v:"+k+":y", id))
	f.typedOnce(t, "A2", " please rerun\n")
	sameEdit(t, f.editAt(t, 0), relaytest.Edit{MessageID: id, Text: "→ <b>acme-shop-fa</b> · queued (busy)\nplease rerun"})
	f.toastsAre(t, "sent to acme-shop-fa")
	f.reactions(t, reactSent)
}

// The pane gets the transcript itself; the phone sees it escaped and
// redacted.
func TestVoiceSendTypesRawTranscript(t *testing.T) {
	const raw = "a<b & token=abcdefgh12345"
	f := newVoiceFixture(t, &transcriber{text: raw})
	k, id := f.confirm(t, f.voice(7, 101))
	f.handle(t, tapUpdate("v:"+k+":y", id))
	f.typedOnce(t, "A1", " "+raw+"\n")
	sameEdit(t, f.editAt(t, 0), relaytest.Edit{MessageID: id, Text: "→ <b>acme-shop-92</b>\na&lt;b &amp; token=[redacted]"})
}

// A second Send on the same confirmation, handled after the first one
// delivered (the router takes taps one at a time), finds the transcript
// taken: it types nothing and leaves the delivered outcome on the message.
func TestVoiceSendTwiceTypesOnce(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "please rerun"})
	k, id := f.confirm(t, f.voice(7, 101))
	f.handle(t, tapUpdate("v:"+k+":y", id))
	f.handle(t, tapUpdate("v:"+k+":y", id))
	f.typedOnce(t, "A1", " please rerun\n")
	f.toastsAre(t, "sent to acme-shop-92", "expired, send it again")
	if len(f.api.Edits) != 1 || f.api.Edits[0].Text != "→ <b>acme-shop-92</b>\nplease rerun" {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	f.reactions(t, reactSent)
}

func TestVoiceSendRefusedKeepsButtons(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "please rerun"})
	created := f.now
	k, id := f.confirm(t, f.voice(7, 101))
	idle, perm := f.term.Screens["A1"], f.term.Screens["A3"]
	f.term.Screens["A1"] = perm // a permission dialog is open
	f.now = f.now.Add(time.Minute)
	f.handle(t, tapUpdate("v:"+k+":y", id))
	if len(f.term.Typed) != 0 {
		t.Fatalf("typed into a dialog: %+v", f.term.Typed)
	}
	why := refusal("acme-shop-92", perm)
	if len(f.api.Edits) != 1 {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	sameEdit(t, f.editAt(t, 0), relaytest.Edit{MessageID: id, Text: "✗ " + why + "\n🎙 to <b>acme-shop-92</b>:\nplease rerun", Keyboard: knownKeyboard(k)})
	f.toastsAre(t, stripTags(why))
	f.reactions(t, reactSeen)
	if p := f.peekParked(t, k); !p.Created.Equal(created) || p.Text != "please rerun" || p.SessionID != sidShop || !p.Voice {
		t.Fatalf("re-parked = %+v", p)
	}
	// A second refusal shows only the newest reason.
	f.handle(t, tapUpdate("v:"+k+":y", id))
	if e := f.editAt(t, 1); e.Text != "✗ "+why+"\n🎙 to <b>acme-shop-92</b>:\nplease rerun" {
		t.Fatalf("second refusal = %+v", e)
	}
	// Once the dialog is answered, Send delivers.
	f.term.Screens["A1"] = idle
	f.handle(t, tapUpdate("v:"+k+":y", id))
	f.typedOnce(t, "A1", " please rerun\n")
	sameEdit(t, f.editAt(t, 2), relaytest.Edit{MessageID: id, Text: "→ <b>acme-shop-92</b>\nplease rerun"})
	f.reactions(t, reactSent)
	f.toastsAre(t, stripTags(why), stripTags(why), "sent to acme-shop-92")
	if f.parked(t) != 0 {
		t.Fatal("a delivered transcript is still parked")
	}
}

// enterFailingTerm fails the Enter that submits the typed text with err
// (nil: "agtermctl: timed out"), as a terminal backend that times out
// after passing the key on would: whether the text was submitted is
// unknown.
type enterFailingTerm struct {
	*relaytest.Term
	err error
}

func (t enterFailingTerm) Type(ctx context.Context, p term.Pane, text string) error {
	if text == term.Enter {
		if t.err != nil {
			return t.err
		}
		return errors.New("agtermctl: timed out")
	}
	return t.Term.Type(ctx, p, text)
}

// A Send whose final Enter failed may have submitted the text: the
// transcript is not parked again, since another Send could submit it twice
// (ruling R21); the confirmation says to check the session, without
// buttons, and the note's 👀 is cleared.
func TestVoiceSendSubmitUnknown(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "please rerun"})
	k, id := f.confirm(t, f.voice(7, 101))
	f.r.Term = enterFailingTerm{Term: f.term}
	at := f.now
	f.handle(t, tapUpdate("v:"+k+":y", id))
	f.typedOnce(t, "A1", " please rerun")
	// The session is named once; the toast leads with the warning, so its
	// 200-character clip never cuts that off.
	const line = "could not submit: agtermctl: timed out — the text may have been sent; check <b>acme-shop-92</b>"
	const toast = "the text may have been sent — check acme-shop-92 (could not submit: agtermctl: timed out)"
	if len(f.api.Edits) != 1 {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	sameEdit(t, f.editAt(t, 0), relaytest.Edit{MessageID: id, Text: "✗ " + line + "\n🎙 to <b>acme-shop-92</b>:\nplease rerun"})
	f.toastsAre(t, toast)
	f.reactions(t, reactClear)
	if f.parked(t) != 0 {
		t.Fatal("a transcript that may have been sent was parked again")
	}
	// The text may have gone through: the session's answer stays owed to
	// the phone (ruling R30), and its voice mark and phone line stay with
	// it.
	if n := f.pending(t, sidShop); n != 1 {
		t.Fatalf("answers owed = %d, want 1", n)
	}
	f.markedOnce(t, sidShop, "", at)
	f.recordedOnce(t, sidShop, "please rerun", at, false)
	// A second Send finds nothing to send.
	f.handle(t, tapUpdate("v:"+k+":y", id))
	f.typedOnce(t, "A1", " please rerun")
	if len(f.api.Edits) != 1 {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	f.toastsAre(t, toast, "expired, send it again")
}

// Typed text whose Enter failed keeps its own reply, and the answers it
// armed stay owed to the phone, as the text may have gone through (ruling
// R30): one for an idle session, two for a busy one.
func TestTextSubmitUnknown(t *testing.T) {
	for _, c := range []struct {
		name    string
		replyTo int64
		sid     string
		owed    int
	}{
		{"acme-shop-92", 101, sidShop, 1},
		{"acme-shop-fa", 110, sidShopF, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.r.Term = enterFailingTerm{Term: f.term}
			f.handle(t, f.msg("ship it", c.replyTo))
			if got := f.api.LastText(); got != "could not submit in <b>"+c.name+"</b>: agtermctl: timed out" {
				t.Fatalf("last = %q", got)
			}
			if n := f.pending(t, c.sid); n != c.owed {
				t.Fatalf("answers owed = %d, want %d", n, c.owed)
			}
		})
	}
}

// A refusal before the Enter (the input never took the text cleanly)
// still rolls back the answers it armed: nothing was submitted.
func TestTextRefusedBeforeSubmitRollsBack(t *testing.T) {
	f := newFixture(t)
	f.term.FailBackspace = errors.New("agtermctl: timed out")
	f.term.MangleNext = true
	f.handle(t, f.msg("ship it", 101))
	if !strings.Contains(f.api.LastText(), "nothing was submitted") {
		t.Fatalf("last = %q", f.api.LastText())
	}
	if n := f.pending(t, sidShop); n != 0 {
		t.Fatalf("answers owed = %d, want 0", n)
	}
}

// A session that ended since the confirmation refuses like a busy pane:
// the buttons stay, so ↪ Other session can still redirect the text.
func TestVoiceSendSessionGone(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "please rerun"})
	k, id := f.confirm(t, f.voice(7, 101))
	live, _ := f.r.Agents(context.Background())
	f.r.Agents = relaytest.AgentsOf(live[1:]...) // acme-shop-92 is gone
	f.handle(t, tapUpdate("v:"+k+":y", id))
	sameEdit(t, f.editAt(t, 0), relaytest.Edit{MessageID: id, Text: "✗ that session is gone\n🎙 to <b>acme-shop-92</b>:\nplease rerun", Keyboard: knownKeyboard(k)})
	f.toastsAre(t, "that session is gone")
	if f.parked(t) != 1 || len(f.term.Typed) != 0 {
		t.Fatalf("parked = %d typed = %+v", f.parked(t), f.term.Typed)
	}
}

func TestVoiceRetryDoesNotExtendExpiry(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "please rerun"})
	start := f.now
	k, id := f.confirm(t, f.voice(7, 101))
	idle := f.term.Screens["A1"]
	f.term.Screens["A1"] = f.term.Screens["A3"]
	f.now = start.Add(9 * time.Minute)
	f.handle(t, tapUpdate("v:"+k+":y", id))
	if len(f.api.Edits) != 1 || f.api.Edits[0].Keyboard == nil {
		t.Fatalf("refusal = %+v", f.api.Edits)
	}
	f.term.Screens["A1"] = idle
	f.now = start.Add(11 * time.Minute)
	f.handle(t, tapUpdate("v:"+k+":y", id))
	if len(f.term.Typed) != 0 {
		t.Fatalf("typed after expiry: %+v", f.term.Typed)
	}
	if len(f.api.Edits) != 2 {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	sameEdit(t, f.editAt(t, 1), relaytest.Edit{MessageID: id, Text: "expired; send it again\nplease rerun"})
	if got := f.toasts(); len(got) != 2 || got[1] != "expired, send it again" {
		t.Fatalf("toasts = %q", got)
	}
	f.reactions(t, reactClear)
	if f.parked(t) != 0 {
		t.Fatal("an expired transcript is still parked")
	}
}

func TestVoiceCancel(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "please <rerun>"})
	k, id := f.confirm(t, f.voice(7, 101))
	f.handle(t, tapUpdate("v:"+k+":n", id))
	if len(f.api.Edits) != 1 {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	sameEdit(t, f.editAt(t, 0), relaytest.Edit{MessageID: id, Text: "✖️ not sent\nplease &lt;rerun&gt;"})
	f.toastsAre(t, "cancelled")
	f.reactions(t, reactClear)
	if f.parked(t) != 0 || len(f.term.Typed) != 0 {
		t.Fatalf("parked = %d typed = %+v", f.parked(t), f.term.Typed)
	}
	// Cancelled is final: a late Send types nothing.
	f.handle(t, tapUpdate("v:"+k+":y", id))
	if len(f.term.Typed) != 0 || len(f.api.Edits) != 1 {
		t.Fatalf("typed = %+v edits = %+v", f.term.Typed, f.api.Edits)
	}
	f.toastsAre(t, "cancelled", "expired, send it again")
}

// Cancel on the no-target form works the same.
func TestVoiceCancelNoTarget(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "please rerun"})
	_, _ = f.store.UpdateSession(sidBlog, func(s *state.Session) { s.Follow = false })
	k, id := f.confirm(t, f.voice(7, 0))
	f.handle(t, tapUpdate("v:"+k+":n", id))
	sameEdit(t, f.editAt(t, 0), relaytest.Edit{MessageID: id, Text: "✖️ not sent\nplease rerun"})
	f.toastsAre(t, "cancelled")
	f.reactions(t, reactClear)
}

func TestVoiceOtherSession(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "please rerun"})
	created := f.now
	k, id := f.confirm(t, f.voice(7, 101))
	f.now = f.now.Add(time.Minute)
	f.handle(t, tapUpdate("v:"+k+":o", id))
	if len(f.api.Edits) != 1 {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	sameEdit(t, f.editAt(t, 0), relaytest.Edit{MessageID: id, Text: "🎙 Which session?\nplease rerun", Keyboard: whichKeyboard(k)})
	f.toastsAre(t, "")
	p := f.peekParked(t, k)
	if p.Header != "🎙 Which session?" || p.SessionID != "" || p.Text != "please rerun" || !p.Voice || !p.Created.Equal(created) {
		t.Fatalf("parked = %+v", p)
	}
	if len(f.term.Typed) != 0 {
		t.Fatalf("typed before a pick: %+v", f.term.Typed)
	}
	f.term.Screens["A3"] = f.term.Screens["A1"] // blog-6a idle
	f.handle(t, tapUpdate("s:"+sidBlog+":"+k, id))
	f.typedOnce(t, "A3", " please rerun\n")
	sameEdit(t, f.editAt(t, 1), relaytest.Edit{MessageID: id, Text: "→ <b>blog-6a</b>\nplease rerun"})
	f.toastsAre(t, "", "sent to blog-6a")
	f.reactions(t, reactSent)
	if f.parked(t) != 0 {
		t.Fatal("a delivered transcript is still parked")
	}
}

func TestVoiceSessionPickRefusedKeepsKeyboard(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "please rerun"})
	_, _ = f.store.UpdateSession(sidBlog, func(s *state.Session) { s.Follow = false })
	created := f.now
	k, id := f.confirm(t, f.voice(7, 0))
	f.handle(t, tapUpdate("s:"+sidBlog+":"+k, id)) // blog-6a shows a permission dialog
	if len(f.term.Typed) != 0 {
		t.Fatalf("typed into a dialog: %+v", f.term.Typed)
	}
	why := refusal("blog-6a", f.term.Screens["A3"])
	if len(f.api.Edits) != 1 {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	sameEdit(t, f.editAt(t, 0), relaytest.Edit{MessageID: id, Text: "✗ " + why + "\n🎙 Which session?\nplease rerun", Keyboard: whichKeyboard(k)})
	f.toastsAre(t, stripTags(why))
	f.reactions(t, reactSeen)
	if p := f.peekParked(t, k); !p.Created.Equal(created) || p.Header != "🎙 Which session?" {
		t.Fatalf("re-parked = %+v", p)
	}
	f.handle(t, tapUpdate("s:"+sidShop+":"+k, id))
	f.typedOnce(t, "A1", " please rerun\n")
	sameEdit(t, f.editAt(t, 1), relaytest.Edit{MessageID: id, Text: "→ <b>acme-shop-92</b>\nplease rerun"})
	f.reactions(t, reactSent)
}

// A session tap on an expired voice confirmation gets the voice copy, the
// transcript kept under it for copying (ruling R25), and clears the note's
// 👀.
func TestVoiceSessionPickExpired(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "please <rerun> & go"})
	_, _ = f.store.UpdateSession(sidBlog, func(s *state.Session) { s.Follow = false })
	k, id := f.confirm(t, f.voice(7, 0))
	f.now = f.now.Add(pendingTTL + time.Second)
	f.handle(t, tapUpdate("s:"+sidShop+":"+k, id))
	if len(f.term.Typed) != 0 {
		t.Fatalf("typed after expiry: %+v", f.term.Typed)
	}
	sameEdit(t, f.editAt(t, 0), relaytest.Edit{MessageID: id, Text: "expired; send it again\nplease &lt;rerun&gt; &amp; go"})
	f.toastsAre(t, "expired, send it again")
	f.reactions(t, reactClear)
}

func TestVoiceUnknownButton(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "please rerun"})
	k, id := f.confirm(t, f.voice(7, 101))
	for _, data := range []string{"v:" + k + ":x", "v:" + k, "v::y", "v:" + k + ":yy"} {
		f.handle(t, tapUpdate(data, id))
	}
	f.toastsAre(t, "unknown button", "unknown button", "unknown button", "unknown button")
	if f.parked(t) != 1 || len(f.api.Edits) != 0 {
		t.Fatalf("parked = %d edits = %+v", f.parked(t), f.api.Edits)
	}
}

// Send on an answer to a prompt applies it exactly as the typed swipe-reply
// does: the same stored answer or decision and the same edit of the
// prompt's message.
func TestVoiceAskSend(t *testing.T) {
	for _, c := range []struct {
		name, text, toast string
		ask               func(t *testing.T) *state.Ask
		askID             string
		replyTo           int64
	}{
		{"question", "Mango <ripe>", "sent", func(t *testing.T) *state.Ask { return questionAsk(t, 710, 711) }, askQ, 711},
		{"permission", "use pnpm instead", "denied", func(*testing.T) *state.Ask { return permAsk(askP, sidShop, "acme-shop-92", 700) }, askP, 700},
	} {
		t.Run(c.name, func(t *testing.T) {
			typed := newFixture(t)
			typed.putAsk(t, c.ask(t))
			typed.handle(t, typed.msg(c.text, c.replyTo))

			f := newVoiceFixture(t, &transcriber{text: c.text})
			f.putAsk(t, c.ask(t))
			k, id := f.confirm(t, f.voice(7, c.replyTo))
			f.handle(t, tapUpdate("v:"+k+":y", id))

			got, want := f.loadAsk(t, c.askID), typed.loadAsk(t, c.askID)
			if !reflect.DeepEqual(got.Answers, want.Answers) || !reflect.DeepEqual(got.Decision, want.Decision) || got.Done != want.Done {
				t.Fatalf("ask = answers %s decision %+v done %v\nwant answers %s decision %+v done %v",
					got.Answers, got.Decision, got.Done, want.Answers, want.Decision, want.Done)
			}
			if f.pending(t, sidShop) != typed.pending(t, sidShop) {
				t.Fatalf("pending = %d, typed reply's %d", f.pending(t, sidShop), typed.pending(t, sidShop))
			}
			if g, w := f.editsOf(c.replyTo), typed.editsOf(c.replyTo); len(w) != 1 || !reflect.DeepEqual(g, w) {
				t.Fatalf("prompt edits = %+v, typed reply's %+v", g, w)
			}
			edits := f.editsOf(id)
			if len(edits) != 1 {
				t.Fatalf("confirmation edits = %+v", edits)
			}
			sameEdit(t, edits[0], relaytest.Edit{MessageID: id, Text: "→ <b>acme-shop-92</b> · answered\n" + strings.NewReplacer("<", "&lt;", ">", "&gt;").Replace(c.text)})
			f.toastsAre(t, c.toast)
			f.reactions(t, reactSent)
			if len(f.term.Typed) != 0 || f.parked(t) != 0 {
				t.Fatalf("typed = %+v parked = %d", f.term.Typed, f.parked(t))
			}
		})
	}
}

// A prompt closed between the confirmation and Send refuses for good: no
// retry for an answer, so the buttons go and the 👀 is cleared.
func TestVoiceAskSendClosedMeanwhile(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "Mango"})
	f.putAsk(t, questionAsk(t, 710, 711))
	k, id := f.confirm(t, f.voice(7, 711))
	if _, err := f.store.UpdateAsk(askQ, func(a *state.Ask) error { a.Closed = "mac"; return nil }); err != nil {
		t.Fatal(err)
	}
	f.handle(t, tapUpdate("v:"+k+":y", id))
	if a := f.loadAsk(t, askQ); answerOf(t, a, 1) != nil || a.Done {
		t.Fatalf("a closed ask was answered: %s", a.Answers)
	}
	if len(f.api.Edits) != 1 {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	sameEdit(t, f.editAt(t, 0), relaytest.Edit{MessageID: id, Text: "✗ already answered at the Mac\n🎙 answer to <b>acme-shop-92</b>:\nMango"})
	f.toastsAre(t, "already answered at the Mac")
	f.reactions(t, reactClear)
	if f.parked(t) != 0 || len(f.term.Typed) != 0 {
		t.Fatalf("parked = %d typed = %+v", f.parked(t), f.term.Typed)
	}
}

// A prompt removed meanwhile (its hook collected an answer and left) is
// refused the same way.
func TestVoiceAskSendGone(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "use pnpm instead"})
	f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	k, id := f.confirm(t, f.voice(7, 700))
	if err := f.store.DeleteAsk(askP); err != nil {
		t.Fatal(err)
	}
	f.handle(t, tapUpdate("v:"+k+":y", id))
	sameEdit(t, f.editAt(t, 0), relaytest.Edit{MessageID: id, Text: "✗ this request is gone\n🎙 answer to <b>acme-shop-92</b>:\nuse pnpm instead"})
	f.toastsAre(t, "this request is gone")
	f.reactions(t, reactClear)
}

// The typed-text keyboard keeps its own flow: the outcome edit names the
// session only, the owner's message gets 👍, and a refusal is final.
func TestTextKeyboardUnchanged(t *testing.T) {
	f := newFixture(t)
	_, _ = f.store.UpdateSession(sidBlog, func(s *state.Session) { s.Follow = false })
	pick := func(text, sid string) (string, int64) {
		t.Helper()
		f.handle(t, f.msg(text, 0))
		s := f.api.Sent[len(f.api.Sent)-1]
		if s.Text != "Which session?" {
			t.Fatalf("keyboard message = %+v", s)
		}
		for _, row := range s.InlineKeyboard {
			if d := row[0].Data; strings.HasPrefix(d, "s:"+sid+":") {
				return d, 1000 + int64(len(f.api.Sent))
			}
		}
		t.Fatalf("no button for %s: %+v", sid, s.InlineKeyboard)
		return "", 0
	}
	data, id := pick("please rerun", sidShop)
	f.handle(t, tapUpdate(data, id))
	f.typedOnce(t, "A1", " please rerun\n")
	sameEdit(t, f.editAt(t, 0), relaytest.Edit{MessageID: id, Text: "→ <b>acme-shop-92</b>"})
	f.toastsAre(t, "sent to acme-shop-92")
	if want := []relaytest.Reaction{{MessageID: 500, Emoji: "👍"}}; !reflect.DeepEqual(f.api.Reactions, want) {
		t.Fatalf("reactions = %+v", f.api.Reactions)
	}

	data, id = pick("check the queue", sidBlog) // blog-6a shows a permission dialog
	f.handle(t, tapUpdate(data, id))
	why := refusal("blog-6a", f.term.Screens["A3"])
	sameEdit(t, f.editAt(t, 1), relaytest.Edit{MessageID: id, Text: "✗ " + why})
	f.toastsAre(t, "sent to acme-shop-92", stripTags(why))
	if f.parked(t) != 0 {
		t.Fatal("a refused text was parked again")
	}
}

// A busy session picked on the text keyboard queues the text; the outcome
// names the session once, as the voice outcome does (ruling R29).
func TestTextKeyboardBusyOutcome(t *testing.T) {
	f := newFixture(t)
	_, _ = f.store.UpdateSession(sidBlog, func(s *state.Session) { s.Follow = false })
	f.handle(t, f.msg("please rerun", 0))
	data := ""
	for _, row := range f.api.Sent[0].InlineKeyboard {
		if d := row[0].Data; strings.HasPrefix(d, "s:"+sidShopF+":") {
			data = d
		}
	}
	f.handle(t, tapUpdate(data, 1001))
	f.typedOnce(t, "A2", " please rerun\n")
	sameEdit(t, f.editAt(t, 0), relaytest.Edit{MessageID: 1001, Text: "→ <b>acme-shop-fa</b> · queued (busy)"})
	f.toastsAre(t, "sent to acme-shop-fa")
}

func TestHelpMentionsVoice(t *testing.T) {
	const (
		voiceLine  = "• Or send a <b>voice note</b>: I reply with the transcript and buttons, and type it only after you tap ✅ Send (needs <code>voice_command</code>, see the README)."
		spokenLine = "• A short answer to a voice note comes back spoken too, before the text (needs <code>speech_command</code>, see the README)."
	)
	for _, want := range []string{
		voiceLine + "\n" + spokenLine + "\n", // the spoken answer right after the voice note
		"👀 transcribing a voice note",
	} {
		if !strings.Contains(helpText, want) {
			t.Fatalf("help lacks %q", want)
		}
	}
	talking := helpText[:strings.Index(helpText, "<b>Questions and permission prompts</b>")]
	if !strings.Contains(talking, "voice note") {
		t.Fatal("the voice bullet is not under Talking to a session")
	}
}

// A tap after the parked text's TTL is not skipped by the loop as an old
// update (its message is as old as the parking, ruling R19): it reaches
// the router and gets the expired outcome.
func TestRunTapsAfterTTLReachRouter(t *testing.T) {
	run := func(t *testing.T, f *fixture, tap telegram.Update) {
		t.Helper()
		tap.UpdateID = 9
		p := &fakePoller{batches: [][]telegram.Update{{tap}}}
		l := &Loop{Poll: p, Router: f.r, Store: f.store, Once: true, MaxAge: 10 * time.Minute, Log: func(string, ...any) {}}
		if err := l.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if n := skippedNotices(f); n != 0 {
			t.Fatalf("the tap was skipped: %+v", f.api.Sent)
		}
		if len(f.term.Typed) != 0 {
			t.Fatalf("typed after expiry: %+v", f.term.Typed)
		}
	}
	t.Run("voice", func(t *testing.T) {
		f := newVoiceFixture(t, &transcriber{text: "please rerun"})
		sent := f.now
		k, id := f.confirm(t, f.voice(7, 101))
		f.now = sent.Add(pendingTTL + time.Minute)
		tap := tapUpdate("v:"+k+":y", id)
		tap.CallbackQuery.Message.Date = sent.Unix()
		run(t, f, tap)
		sameEdit(t, f.editAt(t, 0), relaytest.Edit{MessageID: id, Text: "expired; send it again\nplease rerun"})
		f.toastsAre(t, "expired, send it again")
		f.reactions(t, reactClear)
	})
	t.Run("text", func(t *testing.T) {
		f := newFixture(t)
		_, _ = f.store.UpdateSession(sidBlog, func(s *state.Session) { s.Follow = false })
		sent := f.now
		f.handle(t, f.msg("please rerun", 0))
		data := f.api.Sent[0].InlineKeyboard[0][0].Data
		f.now = sent.Add(pendingTTL + time.Minute)
		tap := tapUpdate(data, 1001)
		tap.CallbackQuery.Message.Date = sent.Unix()
		run(t, f, tap)
		sameEdit(t, f.editAt(t, 0), relaytest.Edit{MessageID: 1001, Text: "expired; send the text again"})
		f.toastsAre(t, "expired, send the text again")
	})
}

// A second tap on a session button finds nothing parked: it only toasts,
// and the delivered outcome stays on the message (ruling R16), for a voice
// confirmation's session keyboard and for the text keyboard alike.
func TestSessionPickTwiceTypesOnce(t *testing.T) {
	t.Run("voice", func(t *testing.T) {
		f := newVoiceFixture(t, &transcriber{text: "please rerun"})
		_, _ = f.store.UpdateSession(sidBlog, func(s *state.Session) { s.Follow = false })
		k, id := f.confirm(t, f.voice(7, 0))
		f.handle(t, tapUpdate("s:"+sidShop+":"+k, id))
		f.handle(t, tapUpdate("s:"+sidShop+":"+k, id))
		f.typedOnce(t, "A1", " please rerun\n")
		if len(f.api.Edits) != 1 {
			t.Fatalf("edits = %+v", f.api.Edits)
		}
		sameEdit(t, f.editAt(t, 0), relaytest.Edit{MessageID: id, Text: "→ <b>acme-shop-92</b>\nplease rerun"})
		f.toastsAre(t, "sent to acme-shop-92", "expired, send it again")
		f.reactions(t, reactSent)
	})
	t.Run("text", func(t *testing.T) {
		f := newFixture(t)
		_, _ = f.store.UpdateSession(sidBlog, func(s *state.Session) { s.Follow = false })
		f.handle(t, f.msg("please rerun", 0))
		data := f.api.Sent[0].InlineKeyboard[0][0].Data // acme-shop-92
		f.handle(t, tapUpdate(data, 1001))
		f.handle(t, tapUpdate(data, 1001))
		f.typedOnce(t, "A1", " please rerun\n")
		if len(f.api.Edits) != 1 {
			t.Fatalf("edits = %+v", f.api.Edits)
		}
		sameEdit(t, f.editAt(t, 0), relaytest.Edit{MessageID: 1001, Text: "→ <b>acme-shop-92</b>"})
		f.toastsAre(t, "sent to acme-shop-92", "expired, send it again")
	})
}

// A Send on a transcript without a target (parked again by ↪ Other
// session while the message still showed ✅ Send) offers the sessions
// again (ruling R20).
func TestVoiceSendWithoutTargetOffersSessions(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "please rerun"})
	created := f.now
	k, id := f.confirm(t, f.voice(7, 101))
	f.handle(t, tapUpdate("v:"+k+":o", id))
	f.now = f.now.Add(time.Minute)
	f.handle(t, tapUpdate("v:"+k+":y", id))
	if len(f.term.Typed) != 0 {
		t.Fatalf("typed without a target: %+v", f.term.Typed)
	}
	if len(f.api.Edits) != 2 {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	sameEdit(t, f.editAt(t, 1), relaytest.Edit{MessageID: id, Text: "🎙 Which session?\nplease rerun", Keyboard: whichKeyboard(k)})
	f.toastsAre(t, "", "pick a session")
	f.reactions(t, reactSeen)
	if p := f.peekParked(t, k); !untargeted(p) || p.Header != "🎙 Which session?" || !p.Created.Equal(created) {
		t.Fatalf("parked = %+v", p)
	}
}

// When the sessions cannot be listed, a Send, an ↪ Other session or a
// session pick leaves the confirmation as it is, buttons and all, and
// parks the transcript again for another try.
func TestVoiceTapSessionListError(t *testing.T) {
	for _, c := range []struct {
		name     string
		noTarget bool
		data     func(k string) string
	}{
		{"send", false, func(k string) string { return "v:" + k + ":y" }},
		{"other", false, func(k string) string { return "v:" + k + ":o" }},
		{"pick", true, func(k string) string { return "s:" + sidShop + ":" + k }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newVoiceFixture(t, &transcriber{text: "please rerun"})
			replyTo := int64(101)
			if c.noTarget {
				_, _ = f.store.UpdateSession(sidBlog, func(s *state.Session) { s.Follow = false })
				replyTo = 0
			}
			k, id := f.confirm(t, f.voice(7, replyTo))
			before := f.peekParked(t, k)
			f.r.Agents = func(context.Context) ([]session.Agent, error) { return nil, errors.New("claude: not found") }
			if err := f.r.Handle(context.Background(), tapUpdate(c.data(k), id)); err == nil {
				t.Fatal("the listing error was not returned for the log")
			}
			if len(f.api.Edits) != 0 || len(f.term.Typed) != 0 {
				t.Fatalf("edits = %+v typed = %+v", f.api.Edits, f.term.Typed)
			}
			f.toastsAre(t, "cannot list sessions")
			f.reactions(t, reactSeen)
			if p := f.peekParked(t, k); !reflect.DeepEqual(p, before) {
				t.Fatalf("parked = %+v, was %+v", p, before)
			}
		})
	}
}

// editFailingAPI is Telegram failing every edit.
type editFailingAPI struct{ *relaytest.API }

func (a *editFailingAPI) EditMessage(context.Context, int64, int64, string, string, [][]telegram.Button) error {
	return errors.New("telegram 502: Bad Gateway")
}

// ↪ Other session with no session to offer, or a message that cannot be
// edited, leaves the confirmation as it was, its transcript parked with
// its target.
func TestVoiceOtherSessionCannotShowSessions(t *testing.T) {
	for _, c := range []struct {
		name, toast string
		breakIt     func(f *fixture)
	}{
		{"none typeable", "no live session has a terminal pane I can type into", func(f *fixture) { f.r.Agents = relaytest.AgentsOf() }},
		{"edit fails", "could not show the sessions; try again", func(f *fixture) { f.r.API = &editFailingAPI{f.api} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newVoiceFixture(t, &transcriber{text: "please rerun"})
			k, id := f.confirm(t, f.voice(7, 101))
			before := f.peekParked(t, k)
			c.breakIt(f)
			f.handle(t, tapUpdate("v:"+k+":o", id))
			if len(f.api.Edits) != 0 {
				t.Fatalf("edits = %+v", f.api.Edits)
			}
			f.toastsAre(t, c.toast)
			f.reactions(t, reactSeen)
			if p := f.peekParked(t, k); !reflect.DeepEqual(p, before) || p.SessionID != sidShop {
				t.Fatalf("parked = %+v, was %+v", p, before)
			}
		})
	}
}

// breakParkingOnList makes parking fail from the next session listing on
// (a tap lists them after taking the transcript): the pending directory
// becomes a file, so PutPending cannot create it.
func (f *fixture) breakParkingOnList(t *testing.T) {
	t.Helper()
	orig := f.r.Agents
	var once sync.Once
	f.r.Agents = func(ctx context.Context) ([]session.Agent, error) {
		once.Do(func() {
			dir := filepath.Join(f.store.Dir, "pending")
			if err := os.Rename(dir, dir+".moved"); err != nil {
				t.Error(err)
			}
			if err := os.WriteFile(dir, nil, 0o600); err != nil {
				t.Error(err)
			}
		})
		return orig(ctx)
	}
}

// A transcript that cannot be parked again can no longer be sent: the
// confirmation says why without buttons and the note's 👀 is cleared.
func TestVoiceParkFailure(t *testing.T) {
	const parkFailed = "✗ cannot park the message: "
	t.Run("refused send", func(t *testing.T) {
		f := newVoiceFixture(t, &transcriber{text: "please rerun"})
		k, id := f.confirm(t, f.voice(7, 101))
		perm := f.term.Screens["A3"]
		f.term.Screens["A1"] = perm
		f.breakParkingOnList(t)
		f.handle(t, tapUpdate("v:"+k+":y", id))
		if len(f.api.Edits) != 1 {
			t.Fatalf("edits = %+v", f.api.Edits)
		}
		e := f.editAt(t, 0)
		if e.MessageID != id || e.Keyboard != nil || !strings.HasPrefix(e.Text, parkFailed) || !strings.HasSuffix(e.Text, "\n🎙 to <b>acme-shop-92</b>:\nplease rerun") {
			t.Fatalf("edit = %+v", e)
		}
		f.toastsAre(t, stripTags(refusal("acme-shop-92", perm)))
		f.reactions(t, reactClear)
	})
	t.Run("other session", func(t *testing.T) {
		f := newVoiceFixture(t, &transcriber{text: "please rerun"})
		k, id := f.confirm(t, f.voice(7, 101))
		f.breakParkingOnList(t)
		f.handle(t, tapUpdate("v:"+k+":o", id))
		if len(f.api.Edits) != 2 {
			t.Fatalf("edits = %+v", f.api.Edits)
		}
		e := f.editAt(t, 1)
		if e.Keyboard != nil || !strings.HasPrefix(e.Text, parkFailed) || !strings.HasSuffix(e.Text, "\n🎙 Which session?\nplease rerun") {
			t.Fatalf("edit = %+v", e)
		}
		f.toastsAre(t, "cannot park the message")
		f.reactions(t, reactClear)
	})
}

// A session name is capped at 64 runes in a voice confirmation's header and
// outcome and in its toast (ruling R22), so header and transcript always
// fit one message.
func TestVoiceLongSessionNameCapped(t *testing.T) {
	long := "acme-" + strings.Repeat("я", 95) // 100 runes
	capped := "acme-" + strings.Repeat("я", 58) + "…"
	t.Run("session", func(t *testing.T) {
		f := newVoiceFixture(t, &transcriber{text: "please rerun"})
		f.r.Agents = relaytest.AgentsOf(session.Agent{SessionID: sidShop, Name: long, Kind: "interactive", Status: "busy", Cwd: "/Users/alex/code/acme-shop"})
		f.label(t, sidShop, long, long) // the label the hooks keep
		k, id := f.confirm(t, f.voice(7, 101))
		if got := f.api.Sent[0].Text; got != "🎙 to <b>"+capped+"</b>:\nplease rerun" {
			t.Fatalf("confirmation = %q", got)
		}
		f.handle(t, tapUpdate("v:"+k+":y", id))
		sameEdit(t, f.editAt(t, 0), relaytest.Edit{MessageID: id, Text: "→ <b>" + capped + "</b> · queued (busy)\nplease rerun"})
		f.toastsAre(t, "sent to "+capped)
	})
	t.Run("submit unknown", func(t *testing.T) {
		f := newVoiceFixture(t, &transcriber{text: "please rerun"})
		f.r.Agents = relaytest.AgentsOf(session.Agent{SessionID: sidShop, Name: long, Kind: "interactive", Status: "idle", Cwd: "/Users/alex/code/acme-shop"})
		f.label(t, sidShop, long, long) // the label the hooks keep
		k, id := f.confirm(t, f.voice(7, 101))
		// A long backend error: the warning still leads the clipped toast.
		f.r.Term = enterFailingTerm{Term: f.term, err: errors.New(strings.Repeat("agtermctl failed ", 30))}
		f.handle(t, tapUpdate("v:"+k+":y", id))
		e := f.editAt(t, 0)
		line, _, _ := strings.Cut(e.Text, "\n")
		if strings.Contains(e.Text, long) || strings.Count(line, capped) != 1 || !strings.HasSuffix(line, " — the text may have been sent; check <b>"+capped+"</b>") {
			t.Fatalf("edit = %q", e.Text)
		}
		if got := f.toasts(); len(got) != 1 || !strings.HasPrefix(got[0], "the text may have been sent — check "+capped+" (could not submit: agtermctl failed") {
			t.Fatalf("toasts = %q", got)
		}
	})
	// deliver's other refusals name the session as the confirmation does.
	t.Run("refusal", func(t *testing.T) {
		f := newVoiceFixture(t, &transcriber{text: "please rerun"})
		f.r.Agents = relaytest.AgentsOf(session.Agent{SessionID: sidShop, Name: long, Kind: "interactive", Status: "idle", Cwd: "/Users/alex/code/acme-shop"})
		f.label(t, sidShop, long, long) // the label the hooks keep
		k, id := f.confirm(t, f.voice(7, 101))
		perm := f.term.Screens["A3"]
		f.term.Screens["A1"] = perm
		f.handle(t, tapUpdate("v:"+k+":y", id))
		why := refusal(capped, perm)
		sameEdit(t, f.editAt(t, 0), relaytest.Edit{MessageID: id, Text: "✗ " + why + "\n🎙 to <b>" + capped + "</b>:\nplease rerun", Keyboard: knownKeyboard(k)})
		f.toastsAre(t, stripTags(why))
	})
	t.Run("answer", func(t *testing.T) {
		f := newVoiceFixture(t, &transcriber{text: "use pnpm instead"})
		f.putAsk(t, permAsk(askP, sidShop, long, 700))
		k, id := f.confirm(t, f.voice(7, 700))
		if got := f.api.Sent[0].Text; got != "🎙 answer to <b>"+capped+"</b>:\nuse pnpm instead" {
			t.Fatalf("confirmation = %q", got)
		}
		f.handle(t, tapUpdate("v:"+k+":y", id))
		if e := f.editsOf(id); len(e) != 1 || e[0].Text != "→ <b>"+capped+"</b> · answered\nuse pnpm instead" {
			t.Fatalf("edits = %+v", e)
		}
	})
}

// The longest transcript (4000 UTF-16 code units, all emoji) under the
// longest session name (emoji too) still fits one message in every form
// the confirmation takes (ruling R31): the name is capped at 64 UTF-16 code
// units.
func TestVoiceConfirmationAlwaysFits(t *testing.T) {
	long := "acme-" + strings.Repeat("🚀", 95)
	capped := "acme-" + strings.Repeat("🚀", 29) + "…" // 64 code units
	text := strings.Repeat("😀", 2000)
	fits := func(t *testing.T, what, html string) {
		t.Helper()
		if n := shownUnits(html); n > 4096 {
			t.Fatalf("%s is %d UTF-16 code units: %.80q", what, n, html)
		}
	}
	agents := relaytest.AgentsOf(session.Agent{SessionID: sidShop, Name: long, Kind: "interactive", Status: "busy", Cwd: "/Users/alex/code/acme-shop"})
	t.Run("session", func(t *testing.T) {
		f := newVoiceFixture(t, &transcriber{text: text})
		f.r.Agents = agents
		f.label(t, sidShop, long, long) // the label the hooks keep
		k, id := f.confirm(t, f.voice(7, 101))
		if !strings.HasPrefix(f.api.Sent[0].Text, "🎙 to <b>"+capped+"</b>:\n") {
			t.Fatalf("confirmation = %.80q", f.api.Sent[0].Text)
		}
		fits(t, "the confirmation", f.api.Sent[0].Text)
		f.handle(t, tapUpdate("v:"+k+":y", id))
		if e := f.editAt(t, 0); !strings.HasPrefix(e.Text, "→ <b>"+capped+"</b> · queued (busy)\n") {
			t.Fatalf("outcome = %.80q", e.Text)
		}
		fits(t, "the outcome", f.editAt(t, 0).Text)
	})
	t.Run("answer", func(t *testing.T) {
		f := newVoiceFixture(t, &transcriber{text: text})
		f.putAsk(t, permAsk(askP, sidShop, long, 700))
		k, id := f.confirm(t, f.voice(7, 700))
		fits(t, "the confirmation", f.api.Sent[0].Text)
		f.handle(t, tapUpdate("v:"+k+":y", id))
		fits(t, "the outcome", f.editsOf(id)[0].Text)
	})
	t.Run("expired", func(t *testing.T) {
		f := newVoiceFixture(t, &transcriber{text: text})
		f.r.Agents = agents
		f.label(t, sidShop, long, long) // the label the hooks keep
		k, id := f.confirm(t, f.voice(7, 101))
		f.now = f.now.Add(pendingTTL + time.Second)
		f.handle(t, tapUpdate("v:"+k+":y", id))
		fits(t, "the expired edit", f.editAt(t, 0).Text)
	})
}

// A refusal that would not fit one message over the transcript (Telegram's
// 4096, counted in UTF-16 code units of the text as shown) leaves the
// confirmation as it is, buttons and all, and goes out as a reply to it
// (ruling R22).
func TestVoiceRefusalTooLongForAnEdit(t *testing.T) {
	for _, c := range []struct {
		name, text string
	}{
		{"characters", strings.Repeat("я", 4000)},
		{"utf-16", strings.Repeat("😀", 1990)}, // 1990 runes, 3980 UTF-16 code units
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newVoiceFixture(t, &transcriber{text: c.text})
			created := f.now
			k, id := f.confirm(t, f.voice(7, 101))
			perm := f.term.Screens["A3"]
			f.term.Screens["A1"] = perm
			f.handle(t, tapUpdate("v:"+k+":y", id))
			why := refusal("acme-shop-92", perm)
			if len(f.api.Edits) != 0 {
				t.Fatalf("edits = %+v", f.api.Edits)
			}
			last := f.api.Sent[len(f.api.Sent)-1]
			if len(f.api.Sent) != 2 || last.Text != "✗ "+why || last.ReplyToMessageID != id || last.InlineKeyboard != nil {
				t.Fatalf("sent = %+v", f.api.Sent[1:])
			}
			f.toastsAre(t, stripTags(why))
			f.reactions(t, reactSeen)
			if p := f.peekParked(t, k); !p.Created.Equal(created) || p.Text != c.text {
				t.Fatalf("re-parked = %+v", p)
			}
		})
	}
	// Entities count as the characters they show: a transcript of 3000 "&"
	// fits an edit, though its HTML is five times as long.
	t.Run("entities", func(t *testing.T) {
		text := strings.Repeat("&", 3000)
		f := newVoiceFixture(t, &transcriber{text: text})
		k, id := f.confirm(t, f.voice(7, 101))
		f.term.Screens["A1"] = f.term.Screens["A3"]
		f.handle(t, tapUpdate("v:"+k+":y", id))
		if len(f.api.Edits) != 1 || f.api.Edits[0].Keyboard == nil || len(f.api.Sent) != 1 {
			t.Fatalf("edits = %d sent = %d", len(f.api.Edits), len(f.api.Sent))
		}
	})
}

// A reason too long for a message of its own (a backend's huge error)
// goes out cut, as plain text, so the fallback reply always fits too.
func TestVoiceRefusalReasonTooLongItself(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "please rerun"})
	k, id := f.confirm(t, f.voice(7, 101))
	f.term.TypeErr = errors.New("tmux: " + strings.Repeat("x", 5000))
	f.handle(t, tapUpdate("v:"+k+":y", id))
	if len(f.api.Edits) != 0 || len(f.api.Sent) != 2 {
		t.Fatalf("edits = %d sent = %d", len(f.api.Edits), len(f.api.Sent))
	}
	reply := f.api.Sent[1]
	if reply.ReplyToMessageID != id || !strings.HasPrefix(reply.Text, "✗ could not type into acme-shop-92: tmux: xxx") || !strings.HasSuffix(reply.Text, "x…") {
		t.Fatalf("reply = %.120q…", reply.Text)
	}
	if n := shownUnits(reply.Text); n > 4096 {
		t.Fatalf("reply is %d UTF-16 code units", n)
	}
	if f.parked(t) != 1 {
		t.Fatal("the transcript was not parked again")
	}
}

// The cut reason is measured as sent: redaction grows key=abcdefgh into
// key=[redacted], so a credential-heavy huge error is cut further, and the
// reply still fits.
func TestVoiceRefusalReasonCutAfterRedaction(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "please rerun"})
	k, id := f.confirm(t, f.voice(7, 101))
	f.term.TypeErr = errors.New("tmux: " + strings.Repeat("key=abcdefgh ", 600))
	f.handle(t, tapUpdate("v:"+k+":y", id))
	if len(f.api.Edits) != 0 || len(f.api.Sent) != 2 {
		t.Fatalf("edits = %d sent = %d", len(f.api.Edits), len(f.api.Sent))
	}
	reply := f.api.Sent[1]
	// The cut's "…" may itself end up redacted (key=abcdefg… is a
	// credential too), so only the start and the length are checked.
	if !strings.HasPrefix(reply.Text, "✗ could not type into acme-shop-92: tmux: key=[redacted] ") {
		t.Fatalf("reply = %.120q…", reply.Text)
	}
	if n := shownUnits(reply.Text); n > 4096 || n < 3000 {
		t.Fatalf("reply is %d UTF-16 code units, want a cut that fits", n)
	}
}

// shownUnits is the length of a sent HTML text as Telegram counts it.
func shownUnits(html string) int {
	return len(utf16.Encode([]rune(render.StripHTML(html))))
}

// A final refusal (no retry) that would not fit loses only the buttons;
// the reason goes out as a reply.
func TestVoiceFinalRefusalTooLongForAnEdit(t *testing.T) {
	// The longest transcript under a capped name: the confirmation fits
	// (4079 code units), the refusal over it would not (4109).
	text := strings.Repeat("я", 4000)
	f := newVoiceFixture(t, &transcriber{text: text})
	f.putAsk(t, permAsk(askP, sidShop, "acme-"+strings.Repeat("x", 95), 700))
	k, id := f.confirm(t, f.voice(7, 700))
	if _, err := f.store.UpdateAsk(askP, func(a *state.Ask) error { a.Closed = "mac"; return nil }); err != nil {
		t.Fatal(err)
	}
	f.handle(t, tapUpdate("v:"+k+":y", id))
	if want := []relaytest.Edit{{MessageID: id}}; !reflect.DeepEqual(f.api.Edits, want) {
		t.Fatalf("edits = %+v, want the buttons dropped only", f.api.Edits)
	}
	last := f.api.Sent[len(f.api.Sent)-1]
	if len(f.api.Sent) != 2 || last.Text != "✗ already answered at the Mac" || last.ReplyToMessageID != id {
		t.Fatalf("sent = %+v", f.api.Sent[1:])
	}
	f.toastsAre(t, "already answered at the Mac")
	f.reactions(t, reactClear)
}

// marks is the session's voice marks as stored.
func (f *fixture) marks(t *testing.T, sessionID string) []state.VoiceMark {
	t.Helper()
	s, err := f.store.LoadSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return s.VoiceMarks
}

// markedOnce asserts the session holds exactly one voice mark, for askID
// ("" for a delivered transcript), made at at.
func (f *fixture) markedOnce(t *testing.T, sessionID, askID string, at time.Time) {
	t.Helper()
	if m := f.marks(t, sessionID); len(m) != 1 || m[0].AskID != askID || !m[0].At.Equal(at) {
		t.Fatalf("voice marks of %s = %+v, want one for ask %q at %v", sessionID, m, askID, at)
	}
}

// A transcript delivered to a session marks it, so that its next answer
// is spoken: one mark without an ask, made only once the text was
// submitted (a Stop of a running turn while it is typed must find no mark
// to spend), beside the answers owed as before, which are armed before
// the text is typed. A busy session owes two answers and still gets one
// mark. The same write records the line as the phone's. A tap in the
// session list delivers the same way as ✅ Send.
func TestVoiceDeliverMarksSession(t *testing.T) {
	for _, c := range []struct {
		name    string
		replyTo int64 // 0: no target, the session is picked from the list
		sid     string
		owed    int
	}{
		{"idle", 101, sidShop, 1},
		{"busy", 110, sidShopF, 2},
		{"picked", 0, sidShop, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newVoiceFixture(t, &transcriber{text: "please rerun"})
			_, _ = f.store.UpdateSession(sidBlog, func(s *state.Session) { s.Follow = false })
			k, id := f.confirm(t, f.voice(7, c.replyTo))
			f.now = f.now.Add(time.Minute)
			at := f.now
			typed, owedTyping := false, 0
			var marksTyping []state.VoiceMark
			var linesTyping []state.PhoneLine
			f.term.OnType = func(string, string) {
				if !typed {
					typed, marksTyping, owedTyping, linesTyping = true, f.marks(t, c.sid), f.pending(t, c.sid), f.phoneLines(t, c.sid)
				}
			}
			data := "v:" + k + ":y"
			if c.replyTo == 0 {
				data = "s:" + c.sid + ":" + k
			}
			f.handle(t, tapUpdate(data, id))
			if !typed || len(marksTyping) != 0 || len(linesTyping) != 0 || owedTyping != c.owed {
				t.Fatalf("while typing: marks %+v, phone lines %+v, owed %d (typed %v); want no mark, no line and %d owed", marksTyping, linesTyping, owedTyping, typed, c.owed)
			}
			f.markedOnce(t, c.sid, "", at)
			if m := f.marks(t, c.sid); m[0].Lang != "en" {
				t.Fatalf("mark %+v, want the transcript's language, en", m[0])
			}
			// A busy session's text waits behind its turn: queued.
			f.recordedOnce(t, c.sid, "please rerun", at, c.name == "busy")
			if n := f.pending(t, c.sid); n != c.owed {
				t.Fatalf("answers owed = %d, want %d", n, c.owed)
			}
		})
	}
}

// A delivery that fails before its submit marks nothing, records no phone
// line and takes back the answers it armed: the text could not be typed,
// the input never took it cleanly, a dialog appeared while it was typed,
// or the session refused it at once. A mark of an earlier voice note
// stays.
func TestVoiceDeliverFailureMarksNothing(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(f *fixture)
	}{
		{"type error", func(f *fixture) { f.term.TypeErr = errors.New("agtermctl: timed out") }},
		{"readback mismatch", func(f *fixture) {
			f.term.MangleNext = true
			f.term.FailBackspace = errors.New("agtermctl: timed out")
		}},
		{"dialog while typing", func(f *fixture) {
			dialog := termScreen(t, "permission-screen.txt")
			f.term.OnType = func(id, _ string) { f.term.Screens[id] = dialog }
		}},
		{"refused", func(f *fixture) { f.term.Screens["A1"] = termScreen(t, "permission-screen.txt") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newVoiceFixture(t, &transcriber{text: "please rerun"})
			earlier := f.now.Add(-time.Minute)
			_, _ = f.store.UpdateSession(sidShop, func(s *state.Session) { s.MarkVoice("", "", earlier) })
			k, id := f.confirm(t, f.voice(7, 101))
			c.setup(f)
			f.handle(t, tapUpdate("v:"+k+":y", id))
			if !strings.HasPrefix(f.editAt(t, 0).Text, "✗ ") {
				t.Fatalf("outcome = %+v", f.editAt(t, 0))
			}
			for _, x := range f.term.Typed {
				if strings.Contains(x.Text, "\n") {
					t.Fatalf("submitted: %+v", f.term.Typed)
				}
			}
			f.markedOnce(t, sidShop, "", earlier)
			if n := f.pending(t, sidShop); n != 0 {
				t.Fatalf("answers owed = %d, want 0", n)
			}
			if l := f.phoneLines(t, sidShop); len(l) != 0 {
				t.Fatalf("a failed delivery recorded %+v", l)
			}
		})
	}
}

// A followed session owes no answer (it reports every one anyway), but a
// voice note delivered to it marks it all the same, once submitted, and
// records its line; a delivery that fails before its submit adds neither.
func TestVoiceMarkFollowedSession(t *testing.T) {
	f := newVoiceFixture(t, &transcriber{text: "please rerun"})
	f.term.Screens["A3"] = f.term.Screens["A1"] // blog-6a idle
	k, id := f.confirm(t, f.voice(7, 120))
	at := f.now
	f.handle(t, tapUpdate("v:"+k+":y", id))
	f.typedOnce(t, "A3", " please rerun\n")
	f.markedOnce(t, sidBlog, "", at)
	f.recordedOnce(t, sidBlog, "please rerun", at, false)
	if n := f.pending(t, sidBlog); n != 0 {
		t.Fatalf("answers owed = %d, want 0", n)
	}

	k, id = f.confirm(t, f.voice(7, 120))
	f.term.TypeErr = errors.New("agtermctl: timed out")
	f.handle(t, tapUpdate("v:"+k+":y", id))
	f.markedOnce(t, sidBlog, "", at)
	f.recordedOnce(t, sidBlog, "please rerun", at, false)
}
