package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/ask"
	"github.com/IZZY-Agency/claude-tg/internal/hooks"
	"github.com/IZZY-Agency/claude-tg/internal/relay/relaytest"
	"github.com/IZZY-Agency/claude-tg/internal/send"
	"github.com/IZZY-Agency/claude-tg/internal/send/sendtest"
	"github.com/IZZY-Agency/claude-tg/internal/session"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

const (
	askP  = "0123456789abcdef" // a permission prompt
	askP2 = "0123456789abcde2" // a second permission prompt of the same session
	askQ  = "fedcba9876543210" // two questions
)

// questionInput is a single-choice question and a multi-select question;
// the last colour looks like a secret, so its button text is masked.
const questionInput = `{"questions":[` +
	`{"question":"Which fruit?","header":"Fruit","options":[{"label":"Apple","description":"red"},{"label":"Banana","description":"yellow"}],"multiSelect":false},` +
	`{"question":"Which colours?","header":"Colours","options":[{"label":"Red","description":""},{"label":"Green","description":""},{"label":"Blue","description":""},{"label":"token=abcdefgh1234","description":""}],"multiSelect":true}]}`

const suggestionInput = `[{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"npm test"}],"behavior":"allow","destination":"localSettings"}]`

func permAsk(id, sessionID, name string, msgID int64) *state.Ask {
	html, _ := ask.PermissionMessage(name, "Bash", "npm test", ask.ParseSuggestions(json.RawMessage(suggestionInput))...)
	return &state.Ask{ID: id, SessionID: sessionID, SessionName: name, Tool: "Bash", Summary: "npm test",
		Suggestions: json.RawMessage(suggestionInput), MessageIDs: []int64{msgID}, Texts: []string{html}}
}

func questionAsk(t *testing.T, msgIDs ...int64) *state.Ask {
	t.Helper()
	qs, err := ask.ParseQuestions(json.RawMessage(questionInput))
	if err != nil {
		t.Fatal(err)
	}
	a := &state.Ask{ID: askQ, SessionID: sidShop, SessionName: "acme-shop-92", Tool: "AskUserQuestion",
		ToolInput: json.RawMessage(questionInput), MessageIDs: msgIDs}
	for i := range msgIDs {
		html, _ := ask.QuestionMessage("acme-shop-92", qs[i], i+1, len(qs))
		a.Texts = append(a.Texts, html)
		a.Ticked = append(a.Ticked, nil)
		a.Answers = append(a.Answers, nil)
	}
	return a
}

// putAsk stores an ask the way the waiting hook leaves it once its messages
// are sent: hook pid (this test process unless set), deadline, and the
// message ids recorded on the session as SendButtons does.
func (f *fixture) putAsk(t *testing.T, a *state.Ask) {
	t.Helper()
	if a.HookPID == 0 {
		a.HookPID = os.Getpid()
	}
	if a.Created.IsZero() {
		a.Created = f.now
	}
	if a.Deadline.IsZero() {
		a.Deadline = a.Created.Add(12 * time.Hour)
	}
	if err := f.store.PutAsk(a); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.UpdateSession(a.SessionID, func(s *state.Session) {
		s.LastMessageIDs = append(s.LastMessageIDs, a.MessageIDs...)
		s.AskMessageIDs = append(s.AskMessageIDs, a.MessageIDs...)
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) loadAsk(t *testing.T, id string) *state.Ask {
	t.Helper()
	a, err := f.store.LoadAsk(id)
	if err != nil {
		t.Fatalf("load ask %s: %v", id, err)
	}
	return a
}

func (f *fixture) askGone(t *testing.T, id string) {
	t.Helper()
	if _, err := f.store.LoadAsk(id); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ask %s still there (err %v)", id, err)
	}
}

func tapUpdate(data string, msgID int64) telegram.Update {
	return telegram.Update{UpdateID: 3, CallbackQuery: &telegram.CallbackQuery{ID: "cq-" + data, From: &telegram.User{ID: me},
		Message: &telegram.Message{MessageID: msgID, Chat: telegram.Chat{ID: me}}, Data: data}}
}

// pending is how many of its answers the session owes the phone: typed
// replies' and phone answers' credits (state.Session.Owed).
func (f *fixture) pending(t *testing.T, sessionID string) int {
	t.Helper()
	s, err := f.store.LoadSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return s.Owed()
}

// toasts returns the callback answers' texts in order.
func (f *fixture) toasts() []string {
	var out []string
	for _, a := range f.api.Answers {
		out = append(out, a.Text)
	}
	return out
}

// answerOf decodes question i's stored answer: the store keeps JSON values,
// not their bytes (it re-indents and escapes them).
func answerOf(t *testing.T, a *state.Ask, i int) any {
	t.Helper()
	if i >= len(a.Answers) || a.Answers[i] == nil {
		return nil
	}
	var v any
	if err := json.Unmarshal(a.Answers[i], &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func sameDecision(t *testing.T, got *state.Decision, want state.Decision) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("decision = %+v, want %+v", got, want)
	}
}

func TestTapApprovesPermission(t *testing.T) {
	f := newFixture(t)
	f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	f.handle(t, tapUpdate(ask.TapData(askP, 0, 'y', 0), 700))
	a := f.loadAsk(t, askP)
	if !a.Done {
		t.Fatal("ask not done")
	}
	sameDecision(t, a.Decision, state.Decision{Behavior: "allow", Suggestion: -1})
	if got := f.toasts(); !reflect.DeepEqual(got, []string{"approved"}) {
		t.Fatalf("toasts = %q", got)
	}
	if len(f.api.Edits) != 1 || f.api.Edits[0].MessageID != 700 || f.api.Edits[0].Keyboard != nil ||
		!strings.HasSuffix(f.api.Edits[0].Text, "\n\n<i>✅ approved from the phone</i>") ||
		!strings.HasPrefix(f.api.Edits[0].Text, "🔐 <b>acme-shop-92</b>") {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	if n := f.pending(t, sidShop); n != 1 {
		t.Fatalf("PendingAnswers = %d, want 1", n)
	}
	if len(f.term.Typed) != 0 {
		t.Fatalf("typed %+v", f.term.Typed)
	}
	// A second tap on the answered message changes nothing and arms nothing.
	f.handle(t, tapUpdate(ask.TapData(askP, 0, 'n', 0), 700))
	sameDecision(t, f.loadAsk(t, askP).Decision, state.Decision{Behavior: "allow", Suggestion: -1})
	if got := f.toasts(); got[len(got)-1] != "already answered" || len(f.api.Edits) != 1 || f.pending(t, sidShop) != 1 {
		t.Fatalf("toasts = %q edits = %+v pending = %d", got, f.api.Edits, f.pending(t, sidShop))
	}
	// A followed session reports every answer anyway: nothing is armed.
	f.putAsk(t, permAsk(askP2, sidBlog, "blog-6a", 720))
	f.handle(t, tapUpdate(ask.TapData(askP2, 0, 'y', 0), 720))
	if !f.loadAsk(t, askP2).Done || f.pending(t, sidBlog) != 0 {
		t.Fatalf("followed session: done=%v pending=%d", f.loadAsk(t, askP2).Done, f.pending(t, sidBlog))
	}
}

func TestTapAfterMacAnswer(t *testing.T) {
	f := newFixture(t)
	a := permAsk(askP, sidShop, "acme-shop-92", 700)
	a.Closed = "mac"
	f.putAsk(t, a)
	before := f.loadAsk(t, askP)
	f.handle(t, tapUpdate(ask.TapData(askP, 0, 'y', 0), 700))
	if got := f.toasts(); !reflect.DeepEqual(got, []string{"already answered at the Mac"}) {
		t.Fatalf("toasts = %q", got)
	}
	if after := f.loadAsk(t, askP); !reflect.DeepEqual(after, before) {
		t.Fatalf("ask changed: %+v, was %+v", after, before)
	}
	if len(f.api.Edits) != 0 || f.pending(t, sidShop) != 0 {
		t.Fatalf("edits = %+v pending = %d", f.api.Edits, f.pending(t, sidShop))
	}
}

func TestTapWithDeadHookExpires(t *testing.T) {
	f := newFixture(t)
	f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	f.r.Alive = func(int) bool { return false }
	f.handle(t, tapUpdate(ask.TapData(askP, 0, 'y', 0), 700))
	if got := f.toasts(); !reflect.DeepEqual(got, []string{"expired"}) {
		t.Fatalf("toasts = %q", got)
	}
	// Nobody will read an answer now: the ask is closed, its message says
	// so and loses its buttons, and the ask is removed.
	if len(f.api.Edits) != 1 || f.api.Edits[0].MessageID != 700 || f.api.Edits[0].Keyboard != nil ||
		!strings.HasSuffix(f.api.Edits[0].Text, "\n\n<i>expired: answer at the Mac</i>") {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	f.askGone(t, askP)
	if f.pending(t, sidShop) != 0 {
		t.Fatal("an expired tap must not arm an answer")
	}
}

func TestMultiSelectTapsAndDone(t *testing.T) {
	f := newFixture(t)
	f.putAsk(t, questionAsk(t, 710, 711))
	f.handle(t, tapUpdate(ask.TapData(askQ, 1, 'o', 0), 711))
	f.handle(t, tapUpdate(ask.TapData(askQ, 1, 'o', 2), 711))
	if len(f.api.Edits) != 2 {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	for i, e := range f.api.Edits {
		if e.MessageID != 711 || e.Text != "" {
			t.Fatalf("edit %d is not a keyboard-only edit of 711: %+v", i, e)
		}
	}
	labels := func(kb [][]telegram.Button) []string {
		var out []string
		for _, row := range kb {
			out = append(out, row[0].Text)
		}
		return out
	}
	if got := labels(f.api.Edits[0].Keyboard); !reflect.DeepEqual(got, []string{"✓ Red", "Green", "Blue", "token=[redacted]", "Done", "✍️ Other"}) {
		t.Fatalf("first keyboard = %q", got)
	}
	kb := f.api.Edits[1].Keyboard
	if got := labels(kb); !reflect.DeepEqual(got, []string{"✓ Red", "Green", "✓ Blue", "token=[redacted]", "Done", "✍️ Other"}) {
		t.Fatalf("second keyboard = %q", got)
	}
	if kb[3][0].Data != ask.TapData(askQ, 1, 'o', 3) {
		t.Fatalf("redaction touched the callback data: %q", kb[3][0].Data)
	}
	if got := f.toasts(); !reflect.DeepEqual(got, []string{"", ""}) {
		t.Fatalf("toggle toasts = %q", got)
	}
	f.handle(t, tapUpdate(ask.TapData(askQ, 1, 'd', 0), 711))
	a := f.loadAsk(t, askQ)
	if !reflect.DeepEqual(answerOf(t, a, 1), []any{"Red", "Blue"}) || a.Done {
		t.Fatalf("answers = %s done = %v", a.Answers, a.Done)
	}
	last := f.api.Edits[len(f.api.Edits)-1]
	if last.MessageID != 711 || last.Keyboard != nil || !strings.HasSuffix(last.Text, "\n\n<i>→ Red, Blue</i>") {
		t.Fatalf("done edit = %+v", last)
	}
	if got := f.toasts(); got[len(got)-1] != "sent" || f.pending(t, sidShop) != 0 {
		t.Fatalf("toasts = %q pending = %d (one question still open)", got, f.pending(t, sidShop))
	}
	// The last open question completes the ask, which arms one answer.
	f.handle(t, tapUpdate(ask.TapData(askQ, 0, 'o', 1), 710))
	a = f.loadAsk(t, askQ)
	if answerOf(t, a, 0) != "Banana" || !a.Done || f.pending(t, sidShop) != 1 {
		t.Fatalf("answers = %s done = %v pending = %d", a.Answers, a.Done, f.pending(t, sidShop))
	}
}

func TestSwipeReplyAnswersQuestion(t *testing.T) {
	f := newFixture(t)
	f.putAsk(t, questionAsk(t, 710, 711))
	f.handle(t, f.msg("Mango <ripe> & sweet", 710))
	if len(f.term.Typed) != 0 {
		t.Fatalf("a reply to an open question was typed: %+v", f.term.Typed)
	}
	a := f.loadAsk(t, askQ)
	if answerOf(t, a, 0) != "Mango <ripe> & sweet" || answerOf(t, a, 1) != nil || a.Done {
		t.Fatalf("answers = %s done = %v", a.Answers, a.Done)
	}
	if len(f.api.Reactions) != 1 || f.api.Reactions[0].MessageID != 500 || f.api.Reactions[0].Emoji != "👍" {
		t.Fatalf("reactions = %+v", f.api.Reactions)
	}
	if len(f.api.Edits) != 1 || f.api.Edits[0].MessageID != 710 || f.api.Edits[0].Keyboard != nil ||
		!strings.HasSuffix(f.api.Edits[0].Text, "\n\n<i>→ Mango &lt;ripe&gt; &amp; sweet</i>") {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	if len(f.api.Sent) != 0 {
		t.Fatalf("sent = %+v", f.api.Sent)
	}
}

func TestSwipeReplyReactionFailureRepliesWithNote(t *testing.T) {
	f := newFixture(t)
	f.putAsk(t, questionAsk(t, 710, 711))
	f.api.ReactErr = errors.New("REACTION_INVALID")
	f.handle(t, f.msg("Mango", 710))
	if f.api.LastText() != "sent" || f.api.Sent[0].ReplyToMessageID != 500 {
		t.Fatalf("sent = %+v", f.api.Sent)
	}
}

func TestSwipeReplyDeniesPermission(t *testing.T) {
	f := newFixture(t)
	f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	f.handle(t, f.msg("use pnpm instead", 700))
	if len(f.term.Typed) != 0 {
		t.Fatalf("typed %+v", f.term.Typed)
	}
	a := f.loadAsk(t, askP)
	if !a.Done {
		t.Fatal("ask not done")
	}
	sameDecision(t, a.Decision, state.Decision{Behavior: "deny", Suggestion: -1, Message: ask.ReplyPrefix + "use pnpm instead"})
	if len(f.api.Reactions) != 1 || f.api.Reactions[0].Emoji != "👍" {
		t.Fatalf("reactions = %+v", f.api.Reactions)
	}
	if len(f.api.Edits) != 1 || f.api.Edits[0].MessageID != 700 || f.api.Edits[0].Keyboard != nil ||
		!strings.HasSuffix(f.api.Edits[0].Text, "\n\n<i>❌ denied from the phone: use pnpm instead</i>") {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	if f.pending(t, sidShop) != 1 {
		t.Fatalf("PendingAnswers = %d, want 1", f.pending(t, sidShop))
	}
}

func TestSwipeReplyWithDeadHookExpires(t *testing.T) {
	f := newFixture(t)
	f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	f.r.Alive = func(int) bool { return false }
	f.handle(t, f.msg("use pnpm instead", 700))
	if len(f.term.Typed) != 0 || f.api.LastText() != "expired" || len(f.api.Reactions) != 0 {
		t.Fatalf("typed = %+v sent = %+v reactions = %+v", f.term.Typed, f.api.Sent, f.api.Reactions)
	}
	if len(f.api.Edits) != 1 || !strings.HasSuffix(f.api.Edits[0].Text, "<i>expired: answer at the Mac</i>") {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	f.askGone(t, askP)
}

func TestSwipeReplyToClosedAskTypesAsBefore(t *testing.T) {
	f := newFixture(t)
	a := permAsk(askP, sidShop, "acme-shop-92", 700)
	a.Closed = "mac"
	f.putAsk(t, a)
	before := f.loadAsk(t, askP)
	f.handle(t, f.msg("ship it", 700))
	f.typedOnce(t, "A1", " ship it\n")
	if after := f.loadAsk(t, askP); !reflect.DeepEqual(after, before) {
		t.Fatalf("ask changed: %+v", after)
	}
	if len(f.api.Edits) != 0 {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
}

const (
	askRefusal       = "<b>acme-shop-92</b> is waiting on your answer to the question or permission prompt above; tap a button on it or reply to it"
	askUnsentRefusal = "<b>acme-shop-92</b> is waiting on a question or permission prompt; its buttons will arrive here shortly, or answer it on the Mac"
)

func TestAtNameRefusedWhileAskOpen(t *testing.T) {
	f := newFixture(t)
	// While the hook still waits for the owner to be away nothing was sent,
	// but the picker is open at the Mac: typing would land in it.
	a := permAsk(askP, sidShop, "acme-shop-92", 0)
	a.MessageIDs, a.Texts, a.Signature = nil, nil, state.Fingerprint("dialog")
	f.putAsk(t, a)
	f.handle(t, f.msg("@acme-shop-92 run the tests", 0))
	if len(f.term.Typed) != 0 || f.api.LastText() != askUnsentRefusal {
		t.Fatalf("typed = %+v last = %q", f.term.Typed, f.api.LastText())
	}

	if _, err := f.store.UpdateAsk(askP, func(a *state.Ask) error {
		a.MessageIDs, a.Texts = []int64{700}, []string{"🔐"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.handle(t, f.msg("@acme-shop-92 run the tests", 0))
	if len(f.term.Typed) != 0 || f.api.LastText() != askRefusal {
		t.Fatalf("typed = %+v last = %q", f.term.Typed, f.api.LastText())
	}
	// The single followed session is addressed the same way.
	_, _ = f.store.UpdateSession(sidBlog, func(s *state.Session) { s.Follow = false })
	_, _ = f.store.UpdateSession(sidShop, func(s *state.Session) { s.Follow = true })
	f.handle(t, f.msg("run the tests", 0))
	if len(f.term.Typed) != 0 || f.api.LastText() != askRefusal {
		t.Fatalf("typed = %+v last = %q", f.term.Typed, f.api.LastText())
	}
	// An ask whose hook died blocks nothing.
	f.r.Alive = func(int) bool { return false }
	f.handle(t, f.msg("@acme-shop-92 run the tests", 0))
	f.typedOnce(t, "A1", " run the tests\n")
}

func TestKeyboardPickRefusedWhileAskOpen(t *testing.T) {
	f := newFixture(t)
	_, _ = f.store.UpdateSession(sidBlog, func(s *state.Session) { s.Follow = false })
	f.handle(t, f.msg("please rerun", 0))
	var data string
	for _, row := range f.api.Sent[len(f.api.Sent)-1].InlineKeyboard {
		if strings.HasPrefix(row[0].Text, "acme-shop-92") {
			data = row[0].Data
		}
	}
	if data == "" {
		t.Fatal("no button for acme-shop-92")
	}
	f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	f.handle(t, tapUpdate(data, 1001))
	if len(f.term.Typed) != 0 {
		t.Fatalf("typed %+v", f.term.Typed)
	}
	if got := f.toasts(); len(got) != 1 || got[0] != stripTags(askRefusal) {
		t.Fatalf("toasts = %q", got)
	}
	if len(f.api.Edits) != 1 || f.api.Edits[0].MessageID != 1001 || f.api.Edits[0].Text != "✗ "+askRefusal {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
}

func TestDialogWithoutAskRefusedWithFallbacks(t *testing.T) {
	f := newFixture(t)
	f.handle(t, f.msg("do it", 120)) // blog-6a shows a permission dialog, no ask
	if len(f.term.Typed) != 0 || f.api.LastText() != "<b>blog-6a</b> is waiting on a permission prompt; answer it on the Mac, or send /approve or /deny" {
		t.Fatalf("typed = %+v last = %q", f.term.Typed, f.api.LastText())
	}
}

func TestUnknownAskTap(t *testing.T) {
	f := newFixture(t)
	f.handle(t, tapUpdate(ask.TapData(askP, 0, 'y', 0), 777))
	if got := f.toasts(); !reflect.DeepEqual(got, []string{"this request is gone"}) {
		t.Fatalf("toasts = %q", got)
	}
	if len(f.api.Edits) != 1 || f.api.Edits[0].MessageID != 777 || f.api.Edits[0].Text != "" || len(f.api.Edits[0].Keyboard) != 0 {
		t.Fatalf("want the keyboard cleared on 777: %+v", f.api.Edits)
	}
	// A question index the ask does not have: the toast, nothing else.
	f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	f.handle(t, tapUpdate(ask.TapData(askP, 3, 'y', 0), 700))
	if got := f.toasts(); got[len(got)-1] != "this request is gone" || len(f.api.Edits) != 1 || !f.loadAsk(t, askP).Open() {
		t.Fatalf("toasts = %q edits = %+v", got, f.api.Edits)
	}
	// Malformed data is not an ask tap.
	f.handle(t, tapUpdate("a:nothex:0:y", 700))
	if got := f.toasts(); got[len(got)-1] != "unknown button" || len(f.api.Edits) != 1 {
		t.Fatalf("toasts = %q edits = %+v", got, f.api.Edits)
	}
}

func TestTwoAsksSameSessionTapDecidesOnlyItsOwn(t *testing.T) {
	f := newFixture(t)
	f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	f.putAsk(t, permAsk(askP2, sidShop, "acme-shop-92", 720))
	f.handle(t, tapUpdate(ask.TapData(askP, 0, 'n', 0), 700))
	sameDecision(t, f.loadAsk(t, askP).Decision, state.Decision{Behavior: "deny", Suggestion: -1, Message: ask.NoMessage, Interrupt: true})
	if b := f.loadAsk(t, askP2); !b.Open() || b.Decision != nil {
		t.Fatalf("the other ask was touched: %+v", b)
	}
	if len(f.api.Edits) != 1 || f.api.Edits[0].MessageID != 700 || !strings.HasSuffix(f.api.Edits[0].Text, "<i>❌ denied from the phone</i>") {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	// A swipe-reply to the second message decides the second ask only.
	f.handle(t, f.msg("not now", 720))
	sameDecision(t, f.loadAsk(t, askP2).Decision, state.Decision{Behavior: "deny", Suggestion: -1, Message: ask.ReplyPrefix + "not now"})
	sameDecision(t, f.loadAsk(t, askP).Decision, state.Decision{Behavior: "deny", Suggestion: -1, Message: ask.NoMessage, Interrupt: true})
	if len(f.api.Edits) != 2 || f.api.Edits[1].MessageID != 720 {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	if f.pending(t, sidShop) != 2 {
		t.Fatalf("PendingAnswers = %d, want one per request", f.pending(t, sidShop))
	}
}

func TestTapHandledByFreshRouter(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	a := permAsk(askP, sidShop, "acme-shop-92", 700)
	a.HookPID, a.Created, a.Deadline = os.Getpid(), now, now.Add(12*time.Hour)
	f.putAsk(t, a)
	// A relay started after the hook: its own store handle, real clock and
	// real liveness check.
	st, err := state.Open(f.store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	api := &relaytest.API{}
	r := &Router{API: api, Store: st, Term: f.term, Agents: f.r.Agents, ChatID: me, UserID: me}
	if err := r.Handle(context.Background(), tapUpdate(ask.TapData(askP, 0, 's', 0), 700)); err != nil {
		t.Fatal(err)
	}
	got := f.loadAsk(t, askP)
	if !got.Done {
		t.Fatal("ask not done")
	}
	sameDecision(t, got.Decision, state.Decision{Behavior: "allow", Suggestion: 0})
	if len(api.Answers) != 1 || api.Answers[0].Text != "approved" {
		t.Fatalf("answers = %+v", api.Answers)
	}
	if len(api.Edits) != 1 || !strings.HasSuffix(api.Edits[0].Text, "<i>✅ approved from the phone: Yes, always allow Bash(npm test) (this project, this Mac)</i>") {
		t.Fatalf("edits = %+v", api.Edits)
	}
}

func TestSweepClosesDeadHookAsks(t *testing.T) {
	f := newFixture(t)
	const deadPID = 999999
	f.r.Alive = func(pid int) bool { return pid != deadPID }
	// Dead hook, first question answered: only the open question's message
	// is edited.
	q := questionAsk(t, 710, 711)
	q.HookPID = deadPID
	q.Answers[0] = json.RawMessage(`"Apple"`)
	f.putAsk(t, q)
	// Dead hook, nothing sent yet: removed without an edit.
	unsent := permAsk(askP, sidShopF, "acme-shop-fa", 0)
	unsent.MessageIDs, unsent.Texts, unsent.HookPID = nil, nil, deadPID
	f.putAsk(t, unsent)
	// Live hook: untouched.
	f.putAsk(t, permAsk(askP2, sidShop, "acme-shop-92", 720))
	// Live pid but long past its deadline (the pid was reused): retired.
	stale := permAsk("00000000000000aa", sidBlog, "blog-6a", 730)
	stale.Created = f.now.Add(-13 * time.Hour)
	f.putAsk(t, stale)

	f.r.SweepAsks(context.Background())

	f.askGone(t, askQ)
	f.askGone(t, askP)
	f.askGone(t, "00000000000000aa")
	if !f.loadAsk(t, askP2).Open() {
		t.Fatal("the live ask was closed")
	}
	if len(f.api.Edits) != 2 {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	for i, id := range []int64{730, 711} { // oldest ask first
		e := f.api.Edits[i]
		if e.MessageID != id || e.Keyboard != nil || !strings.HasSuffix(e.Text, "\n\n<i>expired: answer at the Mac</i>") {
			t.Fatalf("edit %d = %+v, want message %d expired", i, e, id)
		}
	}
	if f.pending(t, sidShop) != 0 || len(f.api.Answers) != 0 || len(f.api.Sent) != 0 {
		t.Fatalf("sweep did more than close: answers = %+v sent = %+v", f.api.Answers, f.api.Sent)
	}
}

// An ask the hook closed is removed as it is. One answered from the phone
// whose hook died before collecting the answer never reached Claude: each
// of its messages says so first.
func TestSweepRemovesSettledAsksOfDeadHooks(t *testing.T) {
	f := newFixture(t)
	f.r.Alive = func(int) bool { return false }
	done := permAsk(askP, sidShop, "acme-shop-92", 700)
	done.Done, done.Decision = true, &state.Decision{Behavior: "allow", Suggestion: -1}
	f.putAsk(t, done)
	closed := permAsk(askP2, sidShop, "acme-shop-92", 720)
	closed.Closed = "mac"
	f.putAsk(t, closed)
	q := questionAsk(t, 710, 711)
	q.Answers = []json.RawMessage{json.RawMessage(`"Apple"`), json.RawMessage(`["Red"]`)}
	q.Done = true
	f.putAsk(t, q)
	f.r.SweepAsks(context.Background())
	f.askGone(t, askP)
	f.askGone(t, askP2)
	f.askGone(t, askQ)
	var got []int64
	for _, e := range f.api.Edits {
		want := "\n\n<i>not delivered: answer at the Mac</i>"
		if e.MessageID == 720 { // closed by its hook, killed before it removed the ask
			want = "\n\n<i>answered at the Mac</i>"
		}
		if e.Keyboard != nil || !strings.HasSuffix(e.Text, want) {
			t.Fatalf("edit = %+v", e)
		}
		got = append(got, e.MessageID)
	}
	slices.Sort(got)
	if !reflect.DeepEqual(got, []int64{700, 710, 711, 720}) {
		t.Fatalf("edited %v, want the answered asks' messages and the closed one's", got)
	}
	if !strings.HasPrefix(f.api.Edits[0].Text, "🔐 <b>acme-shop-92</b>") {
		t.Fatalf("edit lost its base: %q", f.api.Edits[0].Text)
	}
}

// A dead hook whose session has ended: its messages say "session ended",
// and a tap on one gets that toast.
func TestDeadHookOfEndedSessionSaysSessionEnded(t *testing.T) {
	f := newFixture(t)
	f.r.Alive = func(int) bool { return false }
	f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	f.putAsk(t, permAsk(askP2, sidShop, "acme-shop-92", 720))
	// Ended before the ask was made (the session was resumed since): not this ask's end.
	f.putAsk(t, permAsk("00000000000000aa", sidBlog, "blog-6a", 730))
	if _, err := f.store.UpdateSession(sidShop, func(s *state.Session) { s.EndedAt = f.now.Add(time.Minute) }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.UpdateSession(sidBlog, func(s *state.Session) { s.EndedAt = f.now.Add(-time.Hour) }); err != nil {
		t.Fatal(err)
	}
	f.handle(t, tapUpdate(ask.TapData(askP, 0, 'y', 0), 700))
	if got := f.toasts(); !reflect.DeepEqual(got, []string{"session ended"}) {
		t.Fatalf("toasts = %q", got)
	}
	f.r.SweepAsks(context.Background())
	if len(f.api.Edits) != 3 {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	want := map[int64]string{700: "session ended", 720: "session ended", 730: "expired: answer at the Mac"}
	for _, e := range f.api.Edits {
		if !strings.HasSuffix(e.Text, "\n\n<i>"+want[e.MessageID]+"</i>") {
			t.Fatalf("edit = %+v, want %q", e, want[e.MessageID])
		}
		delete(want, e.MessageID)
	}
	if len(want) != 0 {
		t.Fatalf("not edited: %v", want)
	}
	f.askGone(t, askP)
	f.askGone(t, askP2)
	f.askGone(t, "00000000000000aa")
}

// An ask without a hook pid or a deadline has no hook that could read it:
// it blocks nothing and the sweep retires it.
func TestAskWithoutPIDOrDeadlineIsHookless(t *testing.T) {
	f := newFixture(t)
	noPID := permAsk(askP, sidShop, "acme-shop-92", 700)
	noPID.Created, noPID.Deadline = f.now, f.now.Add(12*time.Hour)
	noDeadline := permAsk(askP2, sidShop, "acme-shop-92", 720)
	noDeadline.HookPID, noDeadline.Created = os.Getpid(), f.now
	for _, a := range []*state.Ask{noPID, noDeadline} {
		if err := f.store.PutAsk(a); err != nil {
			t.Fatal(err)
		}
	}
	f.handle(t, f.msg("@acme-shop-92 run the tests", 0))
	f.typedOnce(t, "A1", " run the tests\n")
	f.r.SweepAsks(context.Background())
	f.askGone(t, askP)
	f.askGone(t, askP2)
}

// Until its hook collects it, an answered ask still blocks typing: the
// picker may still be on the Mac's screen.
func TestAnsweredAskBlocksTypingUntilCollected(t *testing.T) {
	f := newFixture(t)
	a := permAsk(askP, sidShop, "acme-shop-92", 700)
	a.Done, a.Decision = true, &state.Decision{Behavior: "allow", Suggestion: -1}
	f.putAsk(t, a)
	f.handle(t, f.msg("@acme-shop-92 run the tests", 0))
	if len(f.term.Typed) != 0 || f.api.LastText() != "<b>acme-shop-92</b> just got your answer; try again in a moment" {
		t.Fatalf("typed = %+v last = %q", f.term.Typed, f.api.LastText())
	}
	// Closed by its hook (answered at the Mac): nothing blocks.
	if _, err := f.store.UpdateAsk(askP, func(a *state.Ask) error { a.Closed = "mac"; return nil }); err != nil {
		t.Fatal(err)
	}
	f.handle(t, f.msg("@acme-shop-92 run the tests", 0))
	f.typedOnce(t, "A1", " run the tests\n")
}

// The sweep removes ask files no one can read once they are an hour old.
func TestSweepRemovesOldCorruptAskFiles(t *testing.T) {
	f := newFixture(t)
	f.r.Now = time.Now
	asks := filepath.Join(f.store.Dir, "asks")
	old, fresh := filepath.Join(asks, "old.json"), filepath.Join(asks, "fresh.json")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	then := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, then, then); err != nil {
		t.Fatal(err)
	}
	f.r.SweepAsks(context.Background())
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old corrupt file kept: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh corrupt file removed: %v", err)
	}
}

func TestSweepPrunesOrphanAskLocks(t *testing.T) {
	f := newFixture(t)
	f.handle(t, tapUpdate(ask.TapData(askP, 0, 'y', 0), 777)) // an unknown ask leaves a lock behind
	lock := filepath.Join(f.store.Dir, "asks", askP+".json.lock")
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	f.r.Now = time.Now
	f.r.SweepAsks(context.Background())
	if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan lock kept: %v", err)
	}
}

func TestLoopSweepsAsksAfterEachBatch(t *testing.T) {
	f := newFixture(t)
	a := permAsk(askP, sidShop, "acme-shop-92", 700)
	a.HookPID = 999999
	f.putAsk(t, a)
	f.r.Alive = func(pid int) bool { return pid != 999999 }
	p := &fakePoller{batches: [][]telegram.Update{{}}}
	l := &Loop{Poll: p, Router: f.r, Store: f.store, Once: true, MaxAge: 10 * time.Minute, Log: func(string, ...any) {}}
	if err := l.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.askGone(t, askP)
	if len(f.api.Edits) != 1 || f.api.Edits[0].MessageID != 700 {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
}

func TestTapFromStrangerDropped(t *testing.T) {
	f := newFixture(t)
	f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	before := f.loadAsk(t, askP)
	stranger := tapUpdate(ask.TapData(askP, 0, 'y', 0), 700)
	stranger.CallbackQuery.From.ID = 1
	f.handle(t, stranger)
	otherChat := tapUpdate(ask.TapData(askP, 0, 'y', 0), 700)
	otherChat.CallbackQuery.Message.Chat.ID = 42
	f.handle(t, otherChat)
	noMessage := tapUpdate(ask.TapData(askP, 0, 'y', 0), 700)
	noMessage.CallbackQuery.Message = nil
	f.handle(t, noMessage)
	// A stranger's swipe-reply is dropped too.
	reply := f.msg("use pnpm", 700)
	reply.Message.From.ID = 1
	f.handle(t, reply)
	if after := f.loadAsk(t, askP); !reflect.DeepEqual(after, before) {
		t.Fatalf("ask changed: %+v", after)
	}
	if len(f.api.Answers) != 0 || len(f.api.Edits) != 0 || len(f.api.Sent) != 0 || len(f.api.Reactions) != 0 || f.pending(t, sidShop) != 0 {
		t.Fatalf("a stranger got a response: %+v", f.api)
	}
}

// Relay edits are redacted between their tags (render.RedactHTML): the
// masked echo still closes its tag, and a value with "<" in it goes whole.
func TestAskEditRedactionKeepsTags(t *testing.T) {
	f := newFixture(t)
	f.putAsk(t, questionAsk(t, 710, 711))
	f.handle(t, f.msg("token=abcdefgh1234", 710))
	f.handle(t, f.msg("mysql --password=s3cr3t<dump.sql", 711))
	if len(f.api.Edits) != 2 || !strings.HasSuffix(f.api.Edits[0].Text, "<i>→ token=[redacted]</i>") ||
		!strings.HasSuffix(f.api.Edits[1].Text, "<i>→ mysql --password=[redacted]</i>") {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	a := f.loadAsk(t, askQ)
	if answerOf(t, a, 0) != "token=abcdefgh1234" || answerOf(t, a, 1) != "mysql --password=s3cr3t<dump.sql" {
		t.Fatal("the answer to Claude must stay exact")
	}
}

func TestAskHelpAndCommandsOfferButtonsFirst(t *testing.T) {
	for _, want := range []string{"button", "reply to", "/approve", "/deny", "fallback"} {
		if !strings.Contains(helpText, want) {
			t.Fatalf("help lacks %q", want)
		}
	}
	for _, want := range []string{
		"While you are away from the Mac, or the session is working on something you sent from the phone, a session's question or permission prompt comes here with buttons.",
		"Plan approval and subagent prompts have no buttons: answer them at the Mac.",
	} {
		if !strings.Contains(helpText, want) {
			t.Fatalf("help lacks %q", want)
		}
	}
	// MCP prompts get buttons: no line saying what has none names them.
	// The fallback line names them as dialogs /approve types into.
	for _, line := range strings.Split(helpText, "\n") {
		if strings.Contains(line, "MCP") && (strings.Contains(line, "no buttons") || !strings.Contains(line, "/approve")) {
			t.Fatalf("help still says MCP prompts have no buttons: %q", line)
		}
	}
	for _, c := range Commands {
		if (c.Command == "approve" || c.Command == "deny") && !strings.Contains(c.Description, "fallback") {
			t.Fatalf("/%s: %q", c.Command, c.Description)
		}
	}
}

// closingAPI plays the waiting hook closing the ask at the Mac (and editing
// its messages) while the relay puts a ticked keyboard back: the relay
// edits under the ask's lock, so the hook, another process, waits for it,
// then closes and edits last.
type closingAPI struct {
	*relaytest.API
	store *state.Store
	id    string
	done  chan struct{}
}

func (c *closingAPI) EditReplyMarkup(ctx context.Context, chatID, messageID int64, kb [][]telegram.Button) error {
	if c.id != "" {
		id := c.id
		c.id, c.done = "", make(chan struct{})
		go func() {
			defer close(c.done)
			if _, err := c.store.UpdateAsk(id, func(a *state.Ask) error { a.Closed = "mac"; return nil }); err == nil {
				_ = c.API.EditMessage(context.Background(), chatID, messageID, "answered at the Mac", "HTML", nil)
			}
		}()
		time.Sleep(50 * time.Millisecond) // the hook tries now, while the relay edits
	}
	return c.API.EditReplyMarkup(ctx, chatID, messageID, kb)
}

func TestTickRacingMacAnswerLeavesNoButtons(t *testing.T) {
	f := newFixture(t)
	f.putAsk(t, questionAsk(t, 710, 711))
	c := &closingAPI{API: f.api, store: f.store, id: askQ}
	f.r.API = c
	f.handle(t, tapUpdate(ask.TapData(askQ, 1, 'o', 0), 711))
	<-c.done
	// The relay's ticked keyboard, then the hook's edit, which drops it.
	if len(f.api.Edits) != 2 || len(f.api.Edits[0].Keyboard) == 0 || f.api.Edits[1].MessageID != 711 ||
		f.api.Edits[1].Text != "answered at the Mac" || len(f.api.Edits[1].Keyboard) != 0 {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	// Closed before the relay's edit: the keyboard is not put back.
	f = newFixture(t)
	f.putAsk(t, questionAsk(t, 710, 711))
	f.r.afterApply = func(id string) { f.hookClosesAtTheMac(t, id, false) }
	f.handle(t, tapUpdate(ask.TapData(askQ, 1, 'o', 0), 711))
	if len(f.api.Edits) != 2 || f.api.Edits[1].Text == "" || len(f.api.Edits[1].Keyboard) != 0 ||
		!strings.HasSuffix(f.api.Edits[len(f.api.Edits)-1].Text, "<i>answered at the Mac</i>") {
		t.Fatalf("closed before the edit: edits = %+v", f.api.Edits)
	}
	// Without a race the keyboard stays.
	g := newFixture(t)
	g.putAsk(t, questionAsk(t, 710, 711))
	g.handle(t, tapUpdate(ask.TapData(askQ, 1, 'o', 0), 711))
	if len(g.api.Edits) != 1 || len(g.api.Edits[0].Keyboard) == 0 {
		t.Fatalf("edits = %+v", g.api.Edits)
	}
}

func TestSwipeReplyToAnsweredAskNotTyped(t *testing.T) {
	f := newFixture(t)
	a := permAsk(askP, sidShop, "acme-shop-92", 700)
	a.Done, a.Decision = true, &state.Decision{Behavior: "allow", Suggestion: -1}
	f.putAsk(t, a) // its hook is alive and about to return the answer
	before := f.loadAsk(t, askP)
	f.handle(t, f.msg("ship it", 700))
	if len(f.term.Typed) != 0 || f.api.LastText() != "already answered" {
		t.Fatalf("typed = %+v last = %q", f.term.Typed, f.api.LastText())
	}
	if after := f.loadAsk(t, askP); !reflect.DeepEqual(after, before) {
		t.Fatalf("ask changed: %+v", after)
	}
}

func TestTypingRefusedWhenAsksUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0 directory")
	}
	f := newFixture(t)
	dir := filepath.Join(f.store.Dir, "asks")
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	f.handle(t, f.msg("@acme-shop-92 run the tests", 0))
	if len(f.term.Typed) != 0 || f.api.LastText() != "could not read open requests; try again" {
		t.Fatalf("typed = %+v last = %q", f.term.Typed, f.api.LastText())
	}
	// No asks directory at all means no open request.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	f.handle(t, f.msg("@acme-shop-92 run the tests", 0))
	f.typedOnce(t, "A1", " run the tests\n")
}

func TestTapBoundToItsMessage(t *testing.T) {
	f := newFixture(t)
	f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	f.putAsk(t, questionAsk(t, 710, 711))
	// The permission prompt's Yes, pressed on some other message.
	f.handle(t, tapUpdate(ask.TapData(askP, 0, 'y', 0), 999))
	// Question 2's option, pressed on question 1's message.
	f.handle(t, tapUpdate(ask.TapData(askQ, 1, 'o', 0), 710))
	if got := f.toasts(); !reflect.DeepEqual(got, []string{"this request is gone", "this request is gone"}) {
		t.Fatalf("toasts = %q", got)
	}
	if p := f.loadAsk(t, askP); !p.Open() || p.Decision != nil {
		t.Fatalf("permission ask decided: %+v", p)
	}
	if q := f.loadAsk(t, askQ); len(q.Ticked) > 1 && len(q.Ticked[1]) > 0 && q.Ticked[1][0] {
		t.Fatalf("question ticked: %+v", q.Ticked)
	}
	if len(f.api.Edits) != 0 || f.pending(t, sidShop) != 0 {
		t.Fatalf("edits = %+v pending = %d", f.api.Edits, f.pending(t, sidShop))
	}
	// Pressed on its own message, the same data works.
	f.handle(t, tapUpdate(ask.TapData(askP, 0, 'y', 0), 700))
	if !f.loadAsk(t, askP).Done {
		t.Fatal("the bound tap was refused")
	}
}

// refusingAPI is Telegram refusing every HTML edit with a 400.
type refusingAPI struct{ *relaytest.API }

var errEntities = &telegram.APIError{Code: 400, Description: "Bad Request: can't parse entities"}

func (r *refusingAPI) EditMessage(ctx context.Context, chatID, messageID int64, text, parseMode string, kb [][]telegram.Button) error {
	if parseMode != "" {
		return errEntities
	}
	return r.API.EditMessage(ctx, chatID, messageID, text, parseMode, kb)
}

// Edits fall back to plain text on a 400, as sends do: an outcome, a
// sweep's closing edit and a session pick's edit all still land.
func TestEditsFallBackToPlainText(t *testing.T) {
	f := newFixture(t)
	f.r.API = &refusingAPI{f.api}
	f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	f.handle(t, tapUpdate(ask.TapData(askP, 0, 'y', 0), 700))
	if len(f.api.Edits) != 1 || f.api.Edits[0].MessageID != 700 || strings.ContainsAny(f.api.Edits[0].Text, "<>") ||
		!strings.HasPrefix(f.api.Edits[0].Text, "🔐 acme-shop-92\n") || !strings.HasSuffix(f.api.Edits[0].Text, "\n\n✅ approved from the phone") {
		t.Fatalf("outcome edits = %+v", f.api.Edits)
	}

	dead := permAsk(askP2, sidShop, "acme-shop-92", 720)
	dead.HookPID = 999999
	f.putAsk(t, dead)
	f.r.Alive = func(pid int) bool { return pid != 999999 }
	f.r.SweepAsks(context.Background())
	if len(f.api.Edits) != 2 || f.api.Edits[1].MessageID != 720 || !strings.HasSuffix(f.api.Edits[1].Text, "\n\nexpired: answer at the Mac") {
		t.Fatalf("sweep edits = %+v", f.api.Edits)
	}

	// An expired parked text gets its pick's edit (a missing one only a toast).
	if err := f.store.PutPending("deadbeef", state.Parked{Text: "please rerun", MessageID: 500, Created: f.now.Add(-pendingTTL - time.Minute)}); err != nil {
		t.Fatal(err)
	}
	f.handle(t, tapUpdate("s:"+sidShop+":deadbeef", 1001))
	if len(f.api.Edits) != 3 || f.api.Edits[2].MessageID != 1001 || f.api.Edits[2].Text != "expired; send the text again" {
		t.Fatalf("pick edits = %+v", f.api.Edits)
	}
}

// A swipe-reply starting with "/" to an open ask is an answer (a path, say),
// not a command; /approve and /deny stay the fallback commands.
func TestSlashReplyToOpenAskIsAnAnswer(t *testing.T) {
	f := newFixture(t)
	f.putAsk(t, questionAsk(t, 710, 711))
	f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	f.handle(t, f.msg("/srv/www/acme/back", 710))
	f.handle(t, f.msg("/help", 711))
	f.handle(t, f.msg("/tmp is fine, use that", 700))
	if len(f.term.Typed) != 0 || len(f.api.Sent) != 0 {
		t.Fatalf("typed = %+v sent = %+v", f.term.Typed, f.api.Sent)
	}
	q := f.loadAsk(t, askQ)
	if answerOf(t, q, 0) != "/srv/www/acme/back" || answerOf(t, q, 1) != "/help" || !q.Done {
		t.Fatalf("answers = %s done = %v", q.Answers, q.Done)
	}
	sameDecision(t, f.loadAsk(t, askP).Decision, state.Decision{Behavior: "deny", Suggestion: -1, Message: ask.ReplyPrefix + "/tmp is fine, use that"})

	// /approve and /deny replied to an open prompt are still commands, and
	// decide it like its ✅ Yes and ❌ No buttons (ruling R38): "/approve"
	// is never the text of a deny, and nothing is typed into the pane.
	g := newFixture(t)
	g.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	g.handle(t, g.msg("/approve", 700))
	sameDecision(t, g.loadAsk(t, askP).Decision, state.Decision{Behavior: "allow", Suggestion: -1})
	g.handle(t, g.msg("/deny", 700))
	if last := g.api.LastText(); last != "already answered" || len(g.term.Typed) != 0 {
		t.Fatalf("/deny after /approve: last = %q typed = %+v", last, g.term.Typed)
	}

	// A command replied to a closed ask is a command as before.
	h := newFixture(t)
	closed := permAsk(askP, sidShop, "acme-shop-92", 700)
	closed.Closed = "mac"
	h.putAsk(t, closed)
	h.handle(t, h.msg("/sessions", 700))
	if !strings.Contains(h.api.LastText(), "acme-shop-92") || !strings.Contains(h.api.LastText(), "follow") {
		t.Fatalf("last = %q", h.api.LastText())
	}
}

// An answer its hook collected reached Claude Code: a sweep that finds the
// ask left behind (the hook killed before it removed it) just removes it.
// An ask whose hook collected the answer and whose relay showed it
// (Settled) is done with: a leftover is removed, its hook alive or not,
// with no edit. One collected but never shown (the relay restarted
// between saving and editing) is shown by the sweep once its hook is gone,
// then removed.
func TestSweepFinishesCollectedAsks(t *testing.T) {
	f := newFixture(t)
	a := permAsk(askP, sidShop, "acme-shop-92", 700)
	a.Done, a.Collected, a.Settled, a.Decision = true, true, true, &state.Decision{Behavior: "allow", Suggestion: -1}
	f.putAsk(t, a)
	f.r.SweepAsks(context.Background())
	f.askGone(t, askP)
	if len(f.api.Edits) != 0 {
		t.Fatalf("edits = %+v", f.api.Edits)
	}

	f = newFixture(t)
	a = permAsk(askP, sidShop, "acme-shop-92", 700)
	a.Done, a.Collected, a.Decision = true, true, &state.Decision{Behavior: "allow", Suggestion: -1}
	f.putAsk(t, a)
	q := questionAsk(t, 710, 711)
	q.Answers = []json.RawMessage{json.RawMessage(`"Apple"`), json.RawMessage(`["Red","Blue"]`)}
	q.Done, q.Collected = true, true
	f.putAsk(t, q)
	f.r.SweepAsks(context.Background()) // their hooks are alive: about to exit
	if f.loadAsk(t, askP) == nil || len(f.api.Edits) != 0 {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	f.r.Alive = func(int) bool { return false }
	f.r.SweepAsks(context.Background())
	f.askGone(t, askP)
	f.askGone(t, askQ)
	want := map[int64]string{700: "<i>✅ approved from the phone</i>", 710: "<i>→ Apple</i>", 711: "<i>→ Red, Blue</i>"}
	if len(f.api.Edits) != 3 {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	for _, e := range f.api.Edits {
		if !strings.HasSuffix(e.Text, "\n\n"+want[e.MessageID]) || e.Keyboard != nil {
			t.Fatalf("edit = %+v, want it to end in %q", e, want[e.MessageID])
		}
	}
}

// The hook collects the phone's answer between the relay saving it and its
// edit: the ask stays (collected, not settled), the relay still shows the
// answer, then removes the ask.
func TestHookCollectsBeforeTheRelaysEdit(t *testing.T) {
	f := newFixture(t)
	f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	f.r.afterApply = func(id string) {
		if err := f.store.CollectAsk(id); err != nil {
			t.Fatal(err)
		}
	}
	f.handle(t, tapUpdate(ask.TapData(askP, 0, 'y', 0), 700))
	if len(f.api.Edits) != 1 || !strings.HasSuffix(f.api.Edits[0].Text, "\n\n<i>✅ approved from the phone</i>") {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	f.askGone(t, askP)
	if got := f.toasts(); !reflect.DeepEqual(got, []string{"approved"}) {
		t.Fatalf("toasts = %q", got)
	}
}

// The relay shows the answer first: it marks the ask settled and leaves it
// to the hook, whose collection removes it.
func TestRelaySettlesBeforeTheHookCollects(t *testing.T) {
	f := newFixture(t)
	f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	f.handle(t, tapUpdate(ask.TapData(askP, 0, 'y', 0), 700))
	if a := f.loadAsk(t, askP); !a.Done || !a.Settled || a.Collected || len(f.api.Edits) != 1 {
		t.Fatalf("ask = %+v edits = %+v", a, f.api.Edits)
	}
	if err := f.store.CollectAsk(askP); err != nil {
		t.Fatal(err)
	}
	f.askGone(t, askP)
	// A tap that does not complete the ask settles nothing.
	f = newFixture(t)
	f.putAsk(t, questionAsk(t, 710, 711))
	f.handle(t, tapUpdate(ask.TapData(askQ, 0, 'o', 1), 710))
	if a := f.loadAsk(t, askQ); a.Done || a.Settled {
		t.Fatalf("ask = %+v", a)
	}
}

// Ruling R38: a prompt that reached the phone is decided there. /approve
// and /deny on a session with an open ask with sent messages decide that
// ask exactly like a tap on ✅ Yes or ❌ No: the same decision, the same
// edit, the session's next answer armed, and nothing typed, whatever the
// pane shows. The toast becomes the 👌 or 👎 reaction on the command.
func TestApproveAndDenyDecideTheOpenAsk(t *testing.T) {
	f := newFixture(t)
	f.term.Screens["A1"] = termScreen(t, "permission-screen.txt") // a Bash dialog is up, and still nothing is typed
	f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	f.handle(t, f.msg("/approve @acme-shop-92", 0))
	a := f.loadAsk(t, askP)
	if !a.Done {
		t.Fatal("ask not done")
	}
	sameDecision(t, a.Decision, state.Decision{Behavior: "allow", Suggestion: -1})
	if len(f.api.Edits) != 1 || f.api.Edits[0].MessageID != 700 || f.api.Edits[0].Keyboard != nil ||
		!strings.HasSuffix(f.api.Edits[0].Text, "\n\n<i>✅ approved from the phone</i>") {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	if len(f.api.Reactions) != 1 || f.api.Reactions[0].Emoji != "👌" || f.api.Reactions[0].MessageID != 500 {
		t.Fatalf("reactions = %+v", f.api.Reactions)
	}
	if len(f.api.Sent) != 0 || len(f.term.Typed) != 0 || f.pending(t, sidShop) != 1 {
		t.Fatalf("sent = %+v typed = %+v pending = %d", f.api.Sent, f.term.Typed, f.pending(t, sidShop))
	}
	// A second /approve finds the ask answered: the tap's toast, as text.
	f.handle(t, f.msg("/approve @acme-shop-92", 0))
	if f.api.LastText() != "already answered" || len(f.api.Edits) != 1 || len(f.api.Reactions) != 1 || len(f.term.Typed) != 0 {
		t.Fatalf("second /approve: last = %q edits = %d reactions = %d typed = %+v", f.api.LastText(), len(f.api.Edits), len(f.api.Reactions), f.term.Typed)
	}

	// /deny by reply to the 🔐 message: like ❌ No.
	f = newFixture(t)
	f.term.Screens["A1"] = termScreen(t, "permission-screen.txt")
	f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	f.handle(t, f.msg("/deny", 700))
	a = f.loadAsk(t, askP)
	sameDecision(t, a.Decision, state.Decision{Behavior: "deny", Suggestion: -1, Message: ask.NoMessage, Interrupt: true})
	if len(f.api.Edits) != 1 || !strings.HasSuffix(f.api.Edits[0].Text, "\n\n<i>❌ denied from the phone</i>") {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	if len(f.api.Reactions) != 1 || f.api.Reactions[0].Emoji != "👎" || len(f.term.Typed) != 0 || len(f.api.Sent) != 0 {
		t.Fatalf("reactions = %+v typed = %+v sent = %+v", f.api.Reactions, f.term.Typed, f.api.Sent)
	}

	// When the reaction fails, the word comes as text, as for the typed path.
	f = newFixture(t)
	f.api.ReactErr = errors.New("REACTION_INVALID")
	f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	f.handle(t, f.msg("/approve @acme-shop-92", 0))
	if f.api.LastText() != "approved <b>acme-shop-92</b>" || !f.loadAsk(t, askP).Done {
		t.Fatalf("last = %q", f.api.LastText())
	}
}

// Several asks of the session on the phone: the newest is decided, the
// others stay open, and the reply says which one.
func TestApproveDecidesTheNewestAskAndSaysSo(t *testing.T) {
	f := newFixture(t)
	older := permAsk(askP, sidShop, "acme-shop-92", 700)
	older.Created = f.now.Add(-2 * time.Minute)
	f.putAsk(t, older)
	newer := permAsk(askP2, sidShop, "acme-shop-92", 720)
	newer.Summary = "npm run build"
	f.putAsk(t, newer)
	f.handle(t, f.msg("/approve @acme-shop-92", 0))
	if !f.loadAsk(t, askP2).Done || !f.loadAsk(t, askP).Open() {
		t.Fatalf("newest done = %v, older open = %v", f.loadAsk(t, askP2).Done, f.loadAsk(t, askP).Open())
	}
	if len(f.api.Edits) != 1 || f.api.Edits[0].MessageID != 720 {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	if len(f.api.Reactions) != 1 || f.api.Reactions[0].Emoji != "👌" {
		t.Fatalf("reactions = %+v", f.api.Reactions)
	}
	if got := f.api.LastText(); got != "approved the newest of 2 open prompts in <b>acme-shop-92</b>: npm run build" {
		t.Fatalf("last = %q", got)
	}
	if len(f.term.Typed) != 0 {
		t.Fatalf("typed %+v", f.term.Typed)
	}
}

// /approve or /deny sent as a reply to a 🔐 message decides the prompt that
// message shows, never another one of the session (the newest is chosen
// only for @name or the single followed session).
func TestReplyApproveDecidesThatPrompt(t *testing.T) {
	f := newFixture(t)
	older := permAsk(askP, sidShop, "acme-shop-92", 700)
	older.Created = f.now.Add(-2 * time.Minute)
	f.putAsk(t, older)
	newer := permAsk(askP2, sidShop, "acme-shop-92", 720)
	newer.Summary = "npm run build"
	f.putAsk(t, newer)
	f.handle(t, f.msg("/approve", 700))
	if a := f.loadAsk(t, askP); !a.Done || a.Decision.Behavior != "allow" || !f.loadAsk(t, askP2).Open() {
		t.Fatalf("replied-to done = %v, other open = %v", a.Done, f.loadAsk(t, askP2).Open())
	}
	if len(f.api.Edits) != 1 || f.api.Edits[0].MessageID != 700 || len(f.api.Sent) != 0 ||
		len(f.api.Reactions) != 1 || f.api.Reactions[0].Emoji != "👌" {
		t.Fatalf("edits = %+v sent = %+v reactions = %+v", f.api.Edits, f.api.Sent, f.api.Reactions)
	}
	f.handle(t, f.msg("/deny", 720))
	sameDecision(t, f.loadAsk(t, askP2).Decision, state.Decision{Behavior: "deny", Suggestion: -1, Message: ask.NoMessage, Interrupt: true})
	if len(f.term.Typed) != 0 || len(f.api.Sent) != 0 {
		t.Fatalf("typed = %+v sent = %+v", f.term.Typed, f.api.Sent)
	}
}

// A reply to the message of a prompt that is closed or gone is refused,
// even with another prompt of the session open or a Bash dialog on the
// pane: the reply names that prompt and no other.
func TestReplyApproveToAClosedPromptRefused(t *testing.T) {
	const closedText = "that prompt is no longer open"
	for _, tc := range []struct {
		name  string
		setup func(f *fixture)
	}{
		{"closed at the Mac", func(f *fixture) {
			a := permAsk(askP, sidShop, "acme-shop-92", 700)
			a.Closed = "mac"
			f.putAsk(t, a)
		}},
		{"gone", func(f *fixture) {
			f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
			if err := f.store.DeleteAsk(askP); err != nil {
				t.Fatal(err)
			}
		}},
		{"gone, another prompt open", func(f *fixture) {
			f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
			_ = f.store.DeleteAsk(askP)
			f.putAsk(t, permAsk(askP2, sidShop, "acme-shop-92", 720))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, cmd := range []string{"/approve", "/deny"} {
				f := newFixture(t)
				f.term.Screens["A1"] = termScreen(t, "permission-screen.txt") // the old fallback would type here
				tc.setup(f)
				f.handle(t, f.msg(cmd, 700))
				if f.api.LastText() != closedText || len(f.term.Typed) != 0 || len(f.api.Edits) != 0 || len(f.api.Reactions) != 0 {
					t.Fatalf("%s: last = %q typed = %+v edits = %+v", cmd, f.api.LastText(), f.term.Typed, f.api.Edits)
				}
				if a, err := f.store.LoadAsk(askP2); err == nil && !a.Open() {
					t.Fatalf("%s decided another prompt: %+v", cmd, a)
				}
			}
		})
	}
	// A reply to a plain message of a session with a prompt on the phone
	// names no prompt: it points at the buttons and decides nothing.
	f := newFixture(t)
	f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	f.handle(t, f.msg("/approve", 100)) // one of the session's earlier messages
	if f.api.LastText() != askRefusal || !f.loadAsk(t, askP).Open() || len(f.term.Typed) != 0 {
		t.Fatalf("last = %q open = %v typed = %+v", f.api.LastText(), f.loadAsk(t, askP).Open(), f.term.Typed)
	}
}

// A question on the phone has no Yes or No: /approve and /deny point at its
// buttons and change nothing.
func TestApproveOnAQuestionPointsAtItsButtons(t *testing.T) {
	f := newFixture(t)
	f.putAsk(t, questionAsk(t, 700, 701))
	for _, cmd := range []string{"/approve @acme-shop-92", "/deny @acme-shop-92"} {
		f.handle(t, f.msg(cmd, 0))
		if f.api.LastText() != askRefusal || len(f.term.Typed) != 0 || len(f.api.Edits) != 0 || len(f.api.Reactions) != 0 {
			t.Fatalf("%s: last = %q typed = %+v edits = %+v reactions = %+v", cmd, f.api.LastText(), f.term.Typed, f.api.Edits, f.api.Reactions)
		}
		if a := f.loadAsk(t, askQ); a.Done || answerOf(t, a, 0) != nil {
			t.Fatalf("%s: ask changed: %+v", cmd, a)
		}
	}
}

// An ask whose messages never went out (the hook still waits for the owner
// to be away), or whose hook is gone, is not on the phone: /approve falls
// back to the pane, and types into the Bash dialog there.
func TestApproveWithoutASentAskTypes(t *testing.T) {
	f := newFixture(t)
	f.term.Screens["A1"] = termScreen(t, "permission-screen.txt")
	unsent := permAsk(askP, sidShop, "acme-shop-92", 0)
	unsent.MessageIDs, unsent.Texts = nil, nil
	f.putAsk(t, unsent)
	f.handle(t, f.msg("/approve @acme-shop-92", 0))
	f.typedOnce(t, "A1", "1")
	if f.loadAsk(t, askP).Done {
		t.Fatal("the unsent ask must not be decided")
	}

	f = newFixture(t)
	f.term.Screens["A1"] = termScreen(t, "permission-screen.txt")
	f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	f.r.Alive = func(int) bool { return false }
	f.handle(t, f.msg("/approve @acme-shop-92", 0))
	f.typedOnce(t, "A1", "1")
}

// The relay masks the bot token (its configured secrets) as the hook's
// sender does, in every text it builds from the ask: the ticked keyboard,
// each outcome, a reply's echo and its notes. Claude still gets the label
// exactly.
func TestRelayScrubsTheBotToken(t *testing.T) {
	const bot = "123456:ABCDEFsecretbot"
	const input = `{"questions":[` +
		`{"question":"Which bot?","header":"Bot","options":[{"label":"bot` + bot + `","description":""},{"label":"none","description":""}],"multiSelect":false},` +
		`{"question":"Which bots?","header":"Bots","options":[{"label":"bot` + bot + `","description":""},{"label":"other","description":""}],"multiSelect":true},` +
		`{"question":"Which name?","header":"Name","options":[{"label":"x","description":""}],"multiSelect":false}]}`
	f := newFixture(t)
	f.r.Secrets = []string{bot}
	qs, err := ask.ParseQuestions(json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	a := &state.Ask{ID: askQ, SessionID: sidShop, SessionName: "acme-shop-92", Tool: "AskUserQuestion", ToolInput: json.RawMessage(input),
		MessageIDs: []int64{710, 711, 712}, Ticked: make([][]bool, 3), Answers: make([]json.RawMessage, 3)}
	clean := &send.Sender{Secrets: []string{bot}}
	for i, q := range qs {
		html, _ := ask.QuestionMessage("acme-shop-92", q, i+1, len(qs))
		a.Texts = append(a.Texts, clean.CleanHTML(html)) // as the hook stores it
	}
	f.putAsk(t, a)
	f.handle(t, tapUpdate(ask.TapData(askQ, 1, 'o', 0), 711)) // tick: the keyboard is rebuilt from the label
	f.handle(t, tapUpdate(ask.TapData(askQ, 1, 'd', 0), 711)) // done: "→ bot…"
	f.handle(t, tapUpdate(ask.TapData(askQ, 0, 'o', 0), 710)) // a single choice: "→ bot…"
	f.handle(t, f.msg("use "+bot, 712))                       // a swipe-reply's echo
	if len(f.api.Edits) != 4 {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	for i, e := range f.api.Edits {
		if strings.Contains(e.Text, bot) || strings.Contains(fmt.Sprint(e.Keyboard), bot) {
			t.Errorf("edit %d shows the token: %+v", i, e)
		}
	}
	if !strings.Contains(fmt.Sprint(f.api.Edits[0].Keyboard), "✓ bot[redacted]") || !strings.HasSuffix(f.api.Edits[1].Text, "<i>→ bot[redacted]</i>") ||
		!strings.HasSuffix(f.api.Edits[3].Text, "<i>→ use [redacted]</i>") {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	got := f.loadAsk(t, askQ)
	if answerOf(t, got, 0) != "bot"+bot || answerOf(t, got, 2) != "use "+bot {
		t.Fatalf("the answers to Claude must stay exact: %s", got.Answers)
	}

	// A note naming a prompt by its summary.
	f = newFixture(t)
	f.r.Secrets = []string{bot}
	older := permAsk(askP, sidShop, "acme-shop-92", 700)
	older.Created = f.now.Add(-time.Minute)
	f.putAsk(t, older)
	newer := permAsk(askP2, sidShop, "acme-shop-92", 720)
	newer.Summary = "curl https://api.telegram.org/bot" + bot + "/getMe"
	f.putAsk(t, newer)
	f.handle(t, f.msg("/approve @acme-shop-92", 0))
	if last := f.api.LastText(); strings.Contains(last, bot) || !strings.Contains(last, "bot[redacted]/getMe") {
		t.Fatalf("note = %q", last)
	}
}

// hookClosesAtTheMac plays the waiting hook seeing the Mac's answer: it
// closes the ask, edits every sent message and, with remove, deletes it.
func (f *fixture) hookClosesAtTheMac(t *testing.T, id string, remove bool) {
	t.Helper()
	a, err := f.store.UpdateAsk(id, func(a *state.Ask) error { a.Closed = "mac"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	for i, mid := range a.MessageIDs {
		if err := f.api.EditMessage(context.Background(), me, mid, ask.WithOutcome(a.Texts[i], "answered at the Mac"), "HTML", nil); err != nil {
			t.Fatal(err)
		}
	}
	if remove {
		if err := f.store.DeleteAsk(id); err != nil {
			t.Fatal(err)
		}
	}
}

// seqAPI records the router's Telegram calls in order and runs onCall
// before each one.
type seqAPI struct {
	*relaytest.API
	seq    []string
	onCall func(call string)
}

func (s *seqAPI) note(call string) {
	if s.onCall != nil {
		s.onCall(call)
	}
	s.seq = append(s.seq, call)
}

func (s *seqAPI) AnswerCallbackQuery(ctx context.Context, id, text string) error {
	s.note("toast " + text)
	return s.API.AnswerCallbackQuery(ctx, id, text)
}

func (s *seqAPI) EditMessage(ctx context.Context, chatID, messageID int64, text, mode string, kb [][]telegram.Button) error {
	s.note(fmt.Sprintf("edit %d", messageID))
	return s.API.EditMessage(ctx, chatID, messageID, text, mode, kb)
}

func (s *seqAPI) EditReplyMarkup(ctx context.Context, chatID, messageID int64, kb [][]telegram.Button) error {
	s.note(fmt.Sprintf("keyboard %d", messageID))
	return s.API.EditReplyMarkup(ctx, chatID, messageID, kb)
}

func (s *seqAPI) SetMessageReaction(ctx context.Context, chatID, messageID int64, emoji string) error {
	s.note("react " + emoji)
	return s.API.SetMessageReaction(ctx, chatID, messageID, emoji)
}

func (s *seqAPI) SendMessage(ctx context.Context, r telegram.SendRequest) (int64, error) {
	s.note("send")
	return s.API.SendMessage(ctx, r)
}

// The session owes its next answer to the phone before the ask is saved
// done: the hook may hand the answer to Claude Code the moment it reads the
// ask (here, while the toast or an edit is slow), and the session's Stop
// hook then looks for an answer owed. Every Telegram call checks it. Once
// per ask: a second tap arms nothing.
func TestAnswerArmedBeforeTheAskIsDone(t *testing.T) {
	cases := []struct {
		name string
		put  func(f *fixture) *state.Ask
		do   func(f *fixture)
	}{
		{"tap", func(f *fixture) *state.Ask { return permAsk(askP, sidShop, "acme-shop-92", 700) },
			func(f *fixture) { f.handle(t, tapUpdate(ask.TapData(askP, 0, 'y', 0), 700)) }},
		{"swipe-reply to a permission prompt", func(f *fixture) *state.Ask { return permAsk(askP, sidShop, "acme-shop-92", 700) },
			func(f *fixture) { f.handle(t, f.msg("use yarn", 700)) }},
		{"/approve", func(f *fixture) *state.Ask { return permAsk(askP, sidShop, "acme-shop-92", 700) },
			func(f *fixture) { f.handle(t, f.msg("/approve @acme-shop-92", 0)) }},
		{"the last question", func(f *fixture) *state.Ask {
			a := questionAsk(t, 710, 711)
			a.ID = askP
			a.Answers[1] = json.RawMessage(`["Red"]`)
			return a
		}, func(f *fixture) { f.handle(t, tapUpdate(ask.TapData(askP, 0, 'o', 1), 710)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.putAsk(t, tc.put(f))
			api := &seqAPI{API: f.api}
			api.onCall = func(call string) {
				a, err := f.store.LoadAsk(askP)
				if err == nil && a.Done && (f.pending(t, sidShop) != 1 || !a.Armed) {
					t.Errorf("at %q the ask is done, its answer not armed: pending %d, armed %v", call, f.pending(t, sidShop), a.Armed)
				}
			}
			f.r.API = api
			tc.do(f)
			if a := f.loadAsk(t, askP); !a.Done || !a.Armed || f.pending(t, sidShop) != 1 || len(api.seq) == 0 {
				t.Fatalf("done=%v armed=%v pending=%d calls=%q", a.Done, a.Armed, f.pending(t, sidShop), api.seq)
			}
			f.handle(t, tapUpdate(ask.TapData(askP, 0, 'n', 0), 700))
			if f.pending(t, sidShop) != 1 {
				t.Fatalf("a second tap armed again: pending %d", f.pending(t, sidShop))
			}
		})
	}
	// A followed session reports every answer anyway: nothing armed.
	f := newFixture(t)
	f.putAsk(t, permAsk(askP2, sidBlog, "blog-6a", 720))
	f.handle(t, tapUpdate(ask.TapData(askP2, 0, 'y', 0), 720))
	if a := f.loadAsk(t, askP2); !a.Done || a.Armed || f.pending(t, sidBlog) != 0 {
		t.Fatalf("followed: done=%v armed=%v pending=%d", a.Done, a.Armed, f.pending(t, sidBlog))
	}
}

// An answer whose ask cannot be saved is not counted: the arming is taken
// back and the tap asks to try again.
func TestArmTakenBackWhenTheAskIsNotSaved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	f := newFixture(t)
	f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
	dir := filepath.Join(f.store.Dir, "asks")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	_ = f.r.Handle(context.Background(), tapUpdate(ask.TapData(askP, 0, 'y', 0), 700))
	_ = os.Chmod(dir, 0o700)
	if a := f.loadAsk(t, askP); a.Done || f.pending(t, sidShop) != 0 {
		t.Fatalf("done=%v pending=%d", a.Done, f.pending(t, sidShop))
	}
	if got := f.toasts(); len(got) != 1 || !strings.Contains(got[0], "try again") {
		t.Fatalf("toasts = %q", got)
	}
}

// After a tap the relay saves the answer, edits the message, and answers
// the callback last: the toast never runs ahead of the message.
func TestTapEditsThenToasts(t *testing.T) {
	for _, tc := range []struct {
		name string
		put  func(f *fixture)
		tap  telegram.Update
		want []string
	}{
		{"yes", func(f *fixture) { f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700)) },
			tapUpdate(ask.TapData(askP, 0, 'y', 0), 700), []string{"edit 700", "toast approved"}},
		{"a tick", func(f *fixture) { f.putAsk(t, questionAsk(t, 710, 711)) },
			tapUpdate(ask.TapData(askQ, 1, 'o', 0), 711), []string{"keyboard 711", "toast "}},
		{"a request that is gone", func(*fixture) {},
			tapUpdate(ask.TapData(askP, 0, 'y', 0), 700), []string{"keyboard 700", "toast this request is gone"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.put(f)
			api := &seqAPI{API: f.api}
			f.r.API = api
			f.handle(t, tc.tap)
			if !reflect.DeepEqual(api.seq, tc.want) {
				t.Fatalf("calls = %q, want %q", api.seq, tc.want)
			}
		})
	}
}

// The hook sees the Mac's answer between the relay saving a tap and its
// edit: it has closed the ask and edited the message ("answered at the
// Mac"), and the relay, reading the ask again under its lock before the
// edit, leaves that text alone, whether the ask is still there (closed) or
// already removed. The toast says why when it can.
func TestMacCloseBetweenSaveAndEditKeepsTheMacText(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove bool
		toast  string
	}{
		{"closed", false, "already answered at the Mac"},
		{"closed and removed", true, "approved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
			f.r.afterApply = func(id string) { f.hookClosesAtTheMac(t, id, tc.remove) }
			f.handle(t, tapUpdate(ask.TapData(askP, 0, 'y', 0), 700))
			if len(f.api.Edits) != 1 || !strings.HasSuffix(f.api.Edits[0].Text, "\n\n<i>answered at the Mac</i>") {
				t.Fatalf("edits = %+v", f.api.Edits)
			}
			if got := f.toasts(); !reflect.DeepEqual(got, []string{tc.toast}) {
				t.Fatalf("toasts = %q, want %q", got, tc.toast)
			}
		})
	}
	// A swipe-reply and /approve leave it alone too.
	for _, text := range []string{"use yarn", "/approve"} {
		f := newFixture(t)
		f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
		f.r.afterApply = func(id string) { f.hookClosesAtTheMac(t, id, false) }
		f.handle(t, f.msg(text, 700))
		if len(f.api.Edits) != 1 || !strings.HasSuffix(f.api.Edits[0].Text, "<i>answered at the Mac</i>") ||
			f.api.LastText() != "already answered at the Mac" || len(f.api.Reactions) != 0 {
			t.Fatalf("%s: edits = %+v last = %q reactions = %+v", text, f.api.Edits, f.api.LastText(), f.api.Reactions)
		}
	}
}

// slowAPI makes its next slow edits hang until their context ends, as an
// edit stuck in the network does, and runs during at the start of each.
type slowAPI struct {
	*relaytest.API
	slow   int
	during func()
}

func (s *slowAPI) EditMessage(ctx context.Context, chatID, messageID int64, text, mode string, kb [][]telegram.Button) error {
	if s.slow > 0 {
		s.slow--
		if s.during != nil {
			s.during()
		}
		<-ctx.Done()
		return ctx.Err()
	}
	return s.API.EditMessage(ctx, chatID, messageID, text, mode, kb)
}

// The relay edits under the ask's lock for at most 0.8 s: a hook Claude
// Code has signalled needs that lock well within its deadline (here it
// waits 0.9 s). A stuck edit gives the lock up, and the relay reads the
// ask again: still open to the answer, it edits once more without the
// lock and records the edit (Settled); closed by the hook meanwhile, it
// leaves the hook's text.
func TestStuckEditGivesTheLockUp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close bool
		toast string
	}{
		{"the answer is still to show", false, "approved"},
		{"the hook closed it meanwhile", true, "already answered at the Mac"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
			var wg sync.WaitGroup
			var took time.Duration
			var hookErr error
			api := &slowAPI{API: f.api, slow: 1}
			api.during = func() {
				wg.Add(1)
				go func() { // the signalled hook
					defer wg.Done()
					start := time.Now()
					_, hookErr = f.store.UpdateAskWithin(askP, 900*time.Millisecond, func(a *state.Ask) error {
						if tc.close {
							a.Closed = "mac"
						}
						return nil
					})
					took = time.Since(start)
					if hookErr == nil && tc.close {
						_ = f.api.EditMessage(context.Background(), me, 700, "answered at the Mac", "HTML", nil)
					}
				}()
			}
			f.r.API = api
			f.handle(t, tapUpdate(ask.TapData(askP, 0, 'y', 0), 700))
			wg.Wait()
			if hookErr != nil || took > 900*time.Millisecond {
				t.Fatalf("the hook got the lock after %s: %v", took, hookErr)
			}
			if got := f.toasts(); !reflect.DeepEqual(got, []string{tc.toast}) {
				t.Fatalf("toasts = %q, want %q", got, tc.toast)
			}
			if tc.close {
				if len(f.api.Edits) != 1 || f.api.Edits[0].Text != "answered at the Mac" {
					t.Fatalf("edits = %+v", f.api.Edits)
				}
				return
			}
			if len(f.api.Edits) != 1 || !strings.HasSuffix(f.api.Edits[0].Text, "<i>✅ approved from the phone</i>") {
				t.Fatalf("edits = %+v", f.api.Edits)
			}
			if a := f.loadAsk(t, askP); !a.Settled {
				t.Fatalf("the edit was not recorded: %+v", a)
			}
		})
	}
}

// A hook killed while it closed its ask (Claude Code's SIGKILL 1.5 s after
// its SIGTERM) leaves the ask closed, its messages maybe not edited: the
// sweep makes the closing edits again, as the hook makes them, then
// removes it. An edit to the same text changes nothing.
func TestSweepRedoesTheClosingEditsOfAKilledHook(t *testing.T) {
	f := newFixture(t)
	f.r.Alive = func(int) bool { return false }
	mac := permAsk(askP, sidShop, "acme-shop-92", 700)
	mac.Closed = "mac"
	f.putAsk(t, mac)
	q := questionAsk(t, 710, 711)
	q.Closed = "ended"
	q.Answers[0] = json.RawMessage(`"Apple"`) // answered from the phone, never sent to Claude
	f.putAsk(t, q)
	f.r.SweepAsks(context.Background())
	f.askGone(t, askP)
	f.askGone(t, askQ)
	want := map[int64]string{700: "answered at the Mac", 710: "not sent: session ended", 711: "session ended"}
	if len(f.api.Edits) != 3 {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	for _, e := range f.api.Edits {
		if e.Keyboard != nil || !strings.HasSuffix(e.Text, "\n\n<i>"+want[e.MessageID]+"</i>") {
			t.Fatalf("edit = %+v, want %q", e, want[e.MessageID])
		}
	}
}

// An ask its hook closed with the phone's counted answer still owed back
// (TakeBack: the session's lock was busy when the hook had to go) is
// finished by the sweep once its hook is gone: the answer is taken back,
// once, however often the sweep or the hook got to it, then the ask goes.
func TestSweepTakesBackTheAnswerOwed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hookDid  bool
		pending  int
		wantLeft int
	}{
		{"the hook could not", false, 1, 0},
		{"the hook did, then could not say so", true, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.r.Alive = func(int) bool { return false }
			_, _ = f.store.UpdateSession(sidShop, func(s *state.Session) { s.ArmAsk(askP) })
			a := permAsk(askP, sidShop, "acme-shop-92", 700)
			a.Done, a.Decision, a.Armed, a.TakeBack, a.Closed = true, &state.Decision{Behavior: "allow", Suggestion: -1}, true, true, "mac"
			f.putAsk(t, a)
			if tc.hookDid {
				if err := f.store.TakeBackAnswer(context.Background(), sidShop, askP); err != nil {
					t.Fatal(err)
				}
			}
			f.r.SweepAsks(context.Background())
			f.r.SweepAsks(context.Background())
			f.askGone(t, askP)
			if n := f.pending(t, sidShop); n != tc.wantLeft {
				t.Fatalf("owed %d, want %d", n, tc.wantLeft)
			}
			if len(f.api.Edits) != 1 || !strings.HasSuffix(f.api.Edits[0].Text, "<i>answered at the Mac</i>") {
				t.Fatalf("edits = %+v", f.api.Edits)
			}
		})
	}
}

// An ask answered from the phone whose hook went without collecting the
// answer (its stdout write failed, or it wrote the answer and could not
// record that) is retired by the sweep, which drops its voice mark first,
// so no later answer is spoken for it, and keeps its credit: the answer
// may well have reached Claude, and its text is still owed to the phone.
// Other credits and marks stay. When dropping the mark fails, the ask is
// kept, unedited, for the next sweep.
func TestSweepDropsTheMarkOfAnUncollectedAnswer(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	f := newFixture(t)
	f.r.Alive = func(int) bool { return false }
	later := f.now.Add(time.Minute)
	if _, err := f.store.UpdateSession(sidShop, func(s *state.Session) {
		s.ArmAsk(askP)
		s.MarkVoice(askP, "", f.now)
		s.ArmAsk(askP2)
		s.MarkVoice("", "", later)
		s.PendingAnswers = 1
	}); err != nil {
		t.Fatal(err)
	}
	a := permAsk(askP, sidShop, "acme-shop-92", 700)
	a.Done, a.Decision, a.Armed = true, &state.Decision{Behavior: "allow", Suggestion: -1}, true
	f.putAsk(t, a)

	dir := filepath.Join(f.store.Dir, "sessions")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	f.r.SweepAsks(context.Background())
	_ = os.Chmod(dir, 0o700)
	if f.loadAsk(t, askP).ID != askP || len(f.api.Edits) != 0 {
		t.Fatalf("edits = %+v, want the ask kept as it was for the next sweep", f.api.Edits)
	}

	f.r.SweepAsks(context.Background())
	f.askGone(t, askP)
	if len(f.api.Edits) != 1 || !strings.HasSuffix(f.api.Edits[0].Text, "<i>not delivered: answer at the Mac</i>") {
		t.Fatalf("edits = %+v", f.api.Edits)
	}
	s, err := f.store.LoadSession(sidShop)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.AskCredits, []string{askP, askP2}) || s.PendingAnswers != 1 {
		t.Fatalf("credits = %q pending = %d, want every credit kept", s.AskCredits, s.PendingAnswers)
	}
	if m := s.VoiceMarks; len(m) != 1 || m[0].AskID != "" || !m[0].At.Equal(later) {
		t.Fatalf("marks = %+v, want only the uncollected ask's mark gone", m)
	}
}

// stopHook runs the session's Stop hook (hooks.Handle "stop") as Claude
// Code does after an answer, on the fixture's clock (the marks the relay
// made at f.now are live), and reports whether it sent that answer to the
// phone.
func (f *fixture) stopHook(t *testing.T, sessionID string) bool {
	t.Helper()
	rec := &sendtest.Recorder{}
	d := hooks.Deps{
		Sender: &send.Sender{API: rec, Store: f.store, ChatID: me, Sleep: func(time.Duration) {}},
		Store:  f.store,
		Resolve: func(_ context.Context, env session.Env) (*state.Session, error) {
			return f.store.LoadSession(env.SessionID)
		},
		Now: func() time.Time { return f.now },
	}
	in := &hooks.Input{SessionID: sessionID, HookEventName: "Stop", LastAssistantMessage: "done: 3 files changed"}
	if err := hooks.Handle(context.Background(), "stop", in, d); err != nil {
		t.Fatal(err)
	}
	return len(rec.Calls) == 1
}

// Each phone answer owes the phone the session's next answer, and taking
// one back takes back only its own: ask A is answered from the phone, the
// Mac wins anyway and the hook cannot take A's answer back in time
// (TakeBack); the session's next Stop uses A's credit; prompt B is then
// answered from the phone; the sweep takes A back. B's Stop must still
// send.
func TestTakingBackAnAnswerLeavesLaterOnes(t *testing.T) {
	f := newFixture(t)
	a := permAsk(askP, sidShop, "acme-shop-92", 700)
	a.HookPID = 999999
	f.putAsk(t, a)
	hookA := true
	f.r.Alive = func(pid int) bool { return pid != 999999 || hookA }
	f.handle(t, tapUpdate(ask.TapData(askP, 0, 'y', 0), 700))
	if _, err := f.store.UpdateAsk(askP, func(a *state.Ask) error {
		a.Closed, a.TakeBack = "mac", true // the Mac won; the take-back is deferred
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	hookA = false // and A's hook is gone
	if !f.stopHook(t, sidShop) {
		t.Fatal("the Stop after A did not send")
	}
	f.putAsk(t, permAsk(askP2, sidShop, "acme-shop-92", 720))
	f.handle(t, tapUpdate(ask.TapData(askP2, 0, 'y', 0), 720))
	f.r.SweepAsks(context.Background())
	f.askGone(t, askP)
	if !f.stopHook(t, sidShop) {
		t.Fatal("B's Stop sent nothing: taking A back took B's credit")
	}
	if f.stopHook(t, sidShop) {
		t.Fatal("a third Stop sent: one credit was counted twice")
	}
}

// A voice answer taken back takes back only its own mark: an older
// transcript marks the session, then ask A is answered by voice from the
// phone and the Mac wins anyway. The session's next Stop spends A's
// credit and the oldest mark, the transcript's, so A's mark is left (marks
// are spent apart from credits). A transcript and a voice answer to
// prompt B then mark the session; taking A back, by the sweep's retire
// (the hook could not in time) or by the hook's own give-back, drops A's
// mark and leaves the later marks and B's credit alone. The Stop hook
// runs on the fixture's clock, so no mark has lapsed.
func TestTakingBackAVoiceAnswerLeavesLaterMarks(t *testing.T) {
	for _, c := range []struct {
		name   string
		retire bool // the sweep's retire takes A back; else the hook does
	}{
		{"retire", true},
		{"hook give-back", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newVoiceFixture(t, &transcriber{text: "use pnpm instead"})
			older := f.now.Add(-time.Minute)
			if _, err := f.store.UpdateSession(sidShop, func(s *state.Session) { s.MarkVoice("", "", older) }); err != nil {
				t.Fatal(err)
			}
			a := permAsk(askP, sidShop, "acme-shop-92", 700)
			a.HookPID = 999999
			f.putAsk(t, a)
			hookA := true
			f.r.Alive = func(pid int) bool { return pid != 999999 || hookA }
			answeredA := f.voiceAnswer(t, 700)
			if m := f.marks(t, sidShop); len(m) != 2 || m[1].AskID != askP || !m[1].At.Equal(answeredA) {
				t.Fatalf("marks = %+v, want the older transcript's and A's", m)
			}
			if _, err := f.store.UpdateAsk(askP, func(a *state.Ask) error {
				a.Closed, a.TakeBack = "mac", c.retire // the Mac won
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !f.stopHook(t, sidShop) {
				t.Fatal("the Stop after A did not send")
			}
			f.markedOnce(t, sidShop, askP, answeredA) // the older mark was spent
			f.now = f.now.Add(time.Minute)
			delivered := f.voiceAnswer(t, 101) // a transcript for acme-shop-92
			f.typedOnce(t, "A1", " use pnpm instead\n")
			f.putAsk(t, permAsk(askP2, sidShop, "acme-shop-92", 720))
			f.now = f.now.Add(time.Minute)
			answered := f.voiceAnswer(t, 720)
			if m := f.marks(t, sidShop); len(m) != 3 || m[0].AskID != askP {
				t.Fatalf("marks before the take-back = %+v, want A's still first", m)
			}
			if c.retire {
				hookA = false // A's hook is gone
				f.r.SweepAsks(context.Background())
				f.askGone(t, askP)
			} else if err := f.store.TakeBackAnswer(context.Background(), sidShop, askP); err != nil {
				t.Fatal(err)
			}
			s, err := f.store.LoadSession(sidShop)
			if err != nil {
				t.Fatal(err)
			}
			if m := s.VoiceMarks; len(m) != 2 || m[0].AskID != "" || !m[0].At.Equal(delivered) || m[1].AskID != askP2 || !m[1].At.Equal(answered) {
				t.Fatalf("marks = %+v, want the transcript's at %v and B's at %v", m, delivered, answered)
			}
			if !slices.Equal(s.AskCredits, []string{askP2}) {
				t.Fatalf("credits = %q, want B's", s.AskCredits)
			}
		})
	}
}

// voiceAnswer confirms a voice note replying to message replyTo with ✅
// Send, and returns the time the Send was handled at.
func (f *fixture) voiceAnswer(t *testing.T, replyTo int64) time.Time {
	t.Helper()
	k, id := f.confirm(t, f.voice(7, replyTo))
	at := f.now
	f.handle(t, tapUpdate("v:"+k+":y", id))
	return at
}

// A voice reply marks the session when it completes an ask, with the ask's
// id, so that taking the ask's answer back takes the mark too: a permission
// prompt, which it completes at once, also in a followed session, which
// gets no credit. A typed reply to the same prompt marks nothing. A voice
// reply to one question of two marks nothing while the other is open, nor
// when buttons then answer the other; one answering the last open question
// marks.
func TestVoiceAnswerAskMarks(t *testing.T) {
	t.Run("permission prompt", func(t *testing.T) {
		f := newVoiceFixture(t, &transcriber{text: "use pnpm instead"})
		f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
		at := f.voiceAnswer(t, 700)
		if a := f.loadAsk(t, askP); !a.Done || !a.Armed || f.pending(t, sidShop) != 1 {
			t.Fatalf("done=%v armed=%v pending=%d", a.Done, a.Armed, f.pending(t, sidShop))
		}
		f.markedOnce(t, sidShop, askP, at)
		if m := f.marks(t, sidShop); m[0].Lang != "en" {
			t.Fatalf("mark %+v, want the reply's language, en", m[0])
		}
	})
	t.Run("followed session", func(t *testing.T) {
		f := newVoiceFixture(t, &transcriber{text: "use pnpm instead"})
		f.putAsk(t, permAsk(askP2, sidBlog, "blog-6a", 720))
		at := f.voiceAnswer(t, 720)
		if a := f.loadAsk(t, askP2); !a.Done || a.Armed || f.pending(t, sidBlog) != 0 {
			t.Fatalf("done=%v armed=%v pending=%d", a.Done, a.Armed, f.pending(t, sidBlog))
		}
		f.markedOnce(t, sidBlog, askP2, at)
	})
	t.Run("typed reply", func(t *testing.T) {
		f := newFixture(t)
		f.putAsk(t, permAsk(askP, sidShop, "acme-shop-92", 700))
		f.handle(t, f.msg("use pnpm instead", 700))
		if a := f.loadAsk(t, askP); !a.Done || f.pending(t, sidShop) != 1 || len(f.marks(t, sidShop)) != 0 {
			t.Fatalf("done=%v pending=%d marks=%+v", a.Done, f.pending(t, sidShop), f.marks(t, sidShop))
		}
	})
	t.Run("one question of two, then buttons", func(t *testing.T) {
		f := newVoiceFixture(t, &transcriber{text: "Mango"})
		f.putAsk(t, questionAsk(t, 710, 711))
		f.voiceAnswer(t, 710)
		if a := f.loadAsk(t, askQ); answerOf(t, a, 0) != "Mango" || a.Done || len(f.marks(t, sidShop)) != 0 {
			t.Fatalf("answers=%s done=%v marks=%+v", a.Answers, a.Done, f.marks(t, sidShop))
		}
		f.handle(t, tapUpdate(ask.TapData(askQ, 1, 'o', 0), 711))
		f.handle(t, tapUpdate(ask.TapData(askQ, 1, 'd', 0), 711))
		if a := f.loadAsk(t, askQ); !a.Done || f.pending(t, sidShop) != 1 || len(f.marks(t, sidShop)) != 0 {
			t.Fatalf("done=%v pending=%d marks=%+v", a.Done, f.pending(t, sidShop), f.marks(t, sidShop))
		}
	})
	t.Run("the last open question", func(t *testing.T) {
		f := newVoiceFixture(t, &transcriber{text: "Mango"})
		f.putAsk(t, questionAsk(t, 710, 711))
		f.handle(t, tapUpdate(ask.TapData(askQ, 1, 'o', 0), 711))
		f.handle(t, tapUpdate(ask.TapData(askQ, 1, 'd', 0), 711))
		if len(f.marks(t, sidShop)) != 0 {
			t.Fatalf("buttons marked: %+v", f.marks(t, sidShop))
		}
		at := f.voiceAnswer(t, 710)
		if a := f.loadAsk(t, askQ); !a.Done || f.pending(t, sidShop) != 1 {
			t.Fatalf("done=%v pending=%d", a.Done, f.pending(t, sidShop))
		}
		f.markedOnce(t, sidShop, askQ, at)
	})
}

// A voice answer whose ask cannot be saved is not counted: the credit and
// the mark it added are taken back, and so is the mark alone of a followed
// session, which gets no credit.
func TestVoiceAnswerAskSaveFailsTakesBack(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	for _, c := range []struct {
		name, id, sid, session string
		msg                    int64
	}{
		{"credit and mark", askP, sidShop, "acme-shop-92", 700},
		{"followed, mark only", askP2, sidBlog, "blog-6a", 720},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newVoiceFixture(t, &transcriber{text: "use pnpm instead"})
			f.putAsk(t, permAsk(c.id, c.sid, c.session, c.msg))
			k, id := f.confirm(t, f.voice(7, c.msg))
			dir := filepath.Join(f.store.Dir, "asks")
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			f.handle(t, tapUpdate("v:"+k+":y", id))
			_ = os.Chmod(dir, 0o700)
			if a := f.loadAsk(t, c.id); a.Done || f.pending(t, c.sid) != 0 || len(f.marks(t, c.sid)) != 0 {
				t.Fatalf("done=%v pending=%d marks=%+v", a.Done, f.pending(t, c.sid), f.marks(t, c.sid))
			}
			if got := f.toasts(); len(got) != 1 || !strings.Contains(got[0], "could not read this request") {
				t.Fatalf("toasts = %q", got)
			}
		})
	}
}

// A hook still looking for its dialog (a call queued behind another, ruling
// P25) claimed nothing and promises no buttons: the pane decides, here the
// queued dialog tg's fallback can answer.
func TestUnclaimedAskPromisesNoButtons(t *testing.T) {
	f := newFixture(t)
	a := permAsk(askP, sidShop, "acme-shop-92", 0)
	a.MessageIDs, a.Texts = nil, nil
	f.putAsk(t, a)
	f.term.Screens["A1"] = termScreen(t, "mcp-queue-1of2.txt")
	f.handle(t, f.msg("@acme-shop-92 run the tests", 0))
	if want := "<b>acme-shop-92</b> is waiting on a permission prompt; answer it on the Mac, or send /approve or /deny"; len(f.term.Typed) != 0 || f.api.LastText() != want {
		t.Fatalf("typed = %+v last = %q", f.term.Typed, f.api.LastText())
	}
}
