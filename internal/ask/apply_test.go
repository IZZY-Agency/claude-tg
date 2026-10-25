package ask

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/IZZY-Agency/claude-tg/internal/state"
)

const testID = "0123456789abcdef"

// questionAsk is the spike's two-question ask after both messages were sent.
func questionAsk() *state.Ask {
	return &state.Ask{ID: testID, Tool: "AskUserQuestion", ToolInput: json.RawMessage(spikeInput),
		MessageIDs: []int64{11, 12}, Texts: []string{"Q1", "Q2"},
		Ticked: [][]bool{nil, nil}, Answers: []json.RawMessage{nil, nil}}
}

// permissionAsk offers one suggestion, at index 1 of the original array.
func permissionAsk() *state.Ask {
	return &state.Ask{ID: testID, Tool: "Bash", Summary: "npm test",
		Suggestions: json.RawMessage(`[{"type":"setMode","mode":"acceptEdits","destination":"session"},` +
			`{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"npm test"}],"behavior":"allow","destination":"localSettings"}]`),
		MessageIDs: []int64{21}, Texts: []string{"P"}}
}

func clone(t *testing.T, a *state.Ask) *state.Ask {
	t.Helper()
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var c state.Ask
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// unchanged fails when apply changed the ask.
func unchanged(t *testing.T, a *state.Ask, apply func(*state.Ask) Result, toast string) {
	t.Helper()
	before := clone(t, a)
	r := apply(a)
	if r.Toast != toast || r.Edit != "" || r.KeyboardOnly || r.Keyboard != nil {
		t.Errorf("result = %+v, want only toast %q", r, toast)
	}
	if !reflect.DeepEqual(clone(t, a), before) {
		t.Errorf("ask changed: %+v, was %+v", a, before)
	}
}

func tap(q int, action byte, n int) Tap { return Tap{AskID: testID, Q: q, Action: action, N: n} }

func TestTapDataFits64Bytes(t *testing.T) {
	id := state.NewAskID()
	for _, act := range []byte{'o', 'd', 'x', 'y', 'n', 's'} {
		for _, v := range []int{0, 3, 999999, math.MaxInt} {
			if d := TapData(id, v, act, v); len(d) > 64 {
				t.Errorf("TapData(%q) is %d bytes", d, len(d))
			}
		}
	}
	for _, c := range []struct {
		q, n int
		act  byte
		want string
	}{
		{1, 2, 'o', "a:0123456789abcdef:1:o2"}, {1, 7, 'd', "a:0123456789abcdef:1:d"}, {0, 7, 'x', "a:0123456789abcdef:0:x"},
		{0, 7, 'y', "a:0123456789abcdef:0:y"}, {0, 7, 'n', "a:0123456789abcdef:0:n"}, {0, 3, 's', "a:0123456789abcdef:0:s3"},
	} {
		if got := TapData(testID, c.q, c.act, c.n); got != c.want {
			t.Errorf("TapData(%d, %c, %d) = %q, want %q", c.q, c.act, c.n, got, c.want)
		}
	}
}

func TestParseTapRoundTrip(t *testing.T) {
	id := state.NewAskID()
	for _, c := range []Tap{
		{id, 0, 'o', 0}, {id, 3, 'o', 2}, {id, 1, 'd', 0}, {id, 2, 'x', 0},
		{id, 0, 'y', 0}, {id, 0, 'n', 0}, {id, 0, 's', 0}, {id, 0, 's', 12},
	} {
		got, ok := ParseTap(TapData(c.AskID, c.Q, c.Action, c.N))
		if !ok || got != c {
			t.Errorf("ParseTap(TapData(%+v)) = %+v, %v", c, got, ok)
		}
	}
}

func TestParseTapRejectsGarbage(t *testing.T) {
	for _, d := range []string{
		"", "a:", "a::0:y", "s:0123456789abcdef:0:y", "b:0123456789abcdef:0:y",
		"a:0123456789abcde:0:y", "a:0123456789abcdef0:0:y", "a:0123456789ABCDEF:0:y", "a:0123456789abcdeg:0:y",
		"a:0123456789abcdef::y", "a:0123456789abcdef:x:y", "a:0123456789abcdef:-1:y", "a:0123456789abcdef:+1:y",
		"a:0123456789abcdef:0:", "a:0123456789abcdef:0:z", "a:0123456789abcdef:0:o", "a:0123456789abcdef:0:o-1",
		"a:0123456789abcdef:0:o+1", "a:0123456789abcdef:0:s", "a:0123456789abcdef:0:y1", "a:0123456789abcdef:0:d0",
		"a:0123456789abcdef:0:o1:extra", "a:0123456789abcdef:0:o99999999999999999999", "a:0123456789abcdef:0",
	} {
		if tp, ok := ParseTap(d); ok {
			t.Errorf("ParseTap(%q) = %+v, want rejection", d, tp)
		}
	}
}

func TestSingleChoiceAnswers(t *testing.T) {
	a := questionAsk()
	r := ApplyTap(a, tap(0, 'o', 1))
	if want := (Result{Toast: "sent", Index: 0, Edit: "Q1\n\n<i>→ Banana</i>"}); !reflect.DeepEqual(r, want) {
		t.Fatalf("result = %+v, want %+v", r, want)
	}
	if string(a.Answers[0]) != `"Banana"` || a.Answers[1] != nil || a.Done {
		t.Fatalf("answers = %s, done = %v", a.Answers, a.Done)
	}
	unchanged(t, a, func(a *state.Ask) Result { return ApplyTap(a, tap(0, 'o', 2)) }, "already answered")
	unchanged(t, a, func(a *state.Ask) Result { return ApplyTap(a, tap(0, 'x', 0)) }, "already answered")
	unchanged(t, a, func(a *state.Ask) Result { return ApplyReply(a, 0, "Mango") }, "already answered")
	// Taps that match nothing in the ask change nothing.
	for _, bad := range []Tap{tap(0, 'o', 3), tap(2, 'o', 0), tap(1, 'o', -1), tap(1, 'y', 0), tap(1, 's', 0), {AskID: "fedcba9876543210", Q: 1, Action: 'o', N: 0}} {
		unchanged(t, a, func(a *state.Ask) Result { return ApplyTap(a, bad) }, "this request is gone")
	}
	// Done has no button on a single-choice question.
	unchanged(t, questionAsk(), func(a *state.Ask) Result { return ApplyTap(a, tap(0, 'd', 0)) }, "this request is gone")
}

func TestMultiSelectToggleThenDone(t *testing.T) {
	a := questionAsk()
	for _, n := range []int{0, 1, 1, 2} {
		r := ApplyTap(a, tap(1, 'o', n))
		if !r.KeyboardOnly || r.Edit != "" || r.Toast != "" || r.Index != 1 {
			t.Fatalf("toggle %d: result = %+v", n, r)
		}
		if a.Answers[1] != nil {
			t.Fatalf("toggle %d answered the question", n)
		}
		if !reflect.DeepEqual(r.Keyboard, QuestionKeyboard(testID, 1, mustQuestions(t, a)[1], a.Ticked[1])) {
			t.Fatalf("toggle %d: keyboard = %+v", n, r.Keyboard)
		}
	}
	if !reflect.DeepEqual(a.Ticked[1], []bool{true, false, true}) {
		t.Fatalf("ticked = %v", a.Ticked[1])
	}
	r := ApplyTap(a, tap(1, 'd', 0))
	if want := (Result{Toast: "sent", Index: 1, Edit: "Q2\n\n<i>→ Red, Blue</i>"}); !reflect.DeepEqual(r, want) {
		t.Fatalf("done: result = %+v, want %+v", r, want)
	}
	if string(a.Answers[1]) != `["Red","Blue"]` {
		t.Fatalf("answer = %s", a.Answers[1])
	}
	unchanged(t, a, func(a *state.Ask) Result { return ApplyTap(a, tap(1, 'o', 1)) }, "already answered")
}

func mustQuestions(t *testing.T, a *state.Ask) []Question {
	t.Helper()
	qs, err := ParseQuestions(a.ToolInput)
	if err != nil {
		t.Fatal(err)
	}
	return qs
}

func TestToggleGrowsShortState(t *testing.T) {
	// The hook appends empty slots as it sends; a tap may find them short.
	a := questionAsk()
	a.Ticked, a.Answers = nil, nil
	r := ApplyTap(a, tap(1, 'o', 2))
	if !r.KeyboardOnly || !reflect.DeepEqual(a.Ticked[1], []bool{false, false, true}) {
		t.Fatalf("result = %+v, ticked = %v", r, a.Ticked)
	}
	r = ApplyTap(a, tap(1, 'd', 0))
	if r.Toast != "sent" || string(a.Answers[1]) != `["Blue"]` || len(a.Answers) != 2 || a.Done {
		t.Fatalf("result = %+v, answers = %s, done = %v", r, a.Answers, a.Done)
	}
}

func TestDoneWithNothingTicked(t *testing.T) {
	a := questionAsk()
	unchanged(t, a, func(a *state.Ask) Result { return ApplyTap(a, tap(1, 'd', 0)) }, "pick at least one")
	ApplyTap(a, tap(1, 'o', 0))
	ApplyTap(a, tap(1, 'o', 0))
	unchanged(t, a, func(a *state.Ask) Result { return ApplyTap(a, tap(1, 'd', 0)) }, "pick at least one")
}

func TestOtherOnlyToasts(t *testing.T) {
	for _, q := range []int{0, 1} {
		unchanged(t, questionAsk(), func(a *state.Ask) Result { return ApplyTap(a, tap(q, 'x', 0)) }, "reply to this message with your answer")
	}
}

func TestReplyOnMultiSelectUsesText(t *testing.T) {
	a := questionAsk()
	ApplyTap(a, tap(1, 'o', 0))
	r := ApplyReply(a, 1, "Purple <ish> & gold")
	if want := (Result{Toast: "sent", Index: 1, Edit: "Q2\n\n<i>→ Purple &lt;ish&gt; &amp; gold</i>"}); !reflect.DeepEqual(r, want) {
		t.Fatalf("result = %+v, want %+v", r, want)
	}
	if string(a.Answers[1]) != `"Purple <ish> & gold"` {
		t.Fatalf("answer = %s", a.Answers[1])
	}
	for _, v := range a.Ticked[1] {
		if v {
			t.Fatalf("ticks kept: %v", a.Ticked[1])
		}
	}
	// A long reply reaches Claude whole; the edit's echo is clipped to keep
	// the message within 4000 characters, or at worst within Telegram's 4096.
	long := strings.Repeat("w", 5000)
	for _, base := range []string{"Q1", strings.Repeat("x", 3990)} {
		a = questionAsk()
		a.Texts[0] = base
		r = ApplyReply(a, 0, long)
		if string(a.Answers[0]) != `"`+long+`"` {
			t.Fatalf("long answer clipped: %d bytes", len(a.Answers[0]))
		}
		visible := utf8.RuneCountInString(strings.NewReplacer("<i>", "", "</i>", "").Replace(r.Edit))
		if !strings.HasSuffix(r.Edit, "…</i>") || visible > 4096 || (base == "Q1" && visible != 4000) {
			t.Fatalf("base %d runes: edit has %d visible characters: %q", len(base), visible, r.Edit[len(r.Edit)-20:])
		}
	}
	unchanged(t, a, func(a *state.Ask) Result { return ApplyReply(a, 2, "x") }, "this request is gone")
}

func TestApplyOnClosedAsk(t *testing.T) {
	for closed, toast := range map[string]string{"mac": "already answered at the Mac", "expired": "expired", "ended": "session ended"} {
		for _, mk := range []func() *state.Ask{questionAsk, permissionAsk} {
			a := mk()
			a.Closed = closed
			unchanged(t, a, func(a *state.Ask) Result { return ApplyTap(a, tap(0, 'o', 0)) }, toast)
			unchanged(t, a, func(a *state.Ask) Result { return ApplyTap(a, tap(0, 'y', 0)) }, toast)
			unchanged(t, a, func(a *state.Ask) Result { return ApplyReply(a, 0, "hi") }, toast)
		}
	}
	a := permissionAsk()
	a.Done = true
	a.Decision = &state.Decision{Behavior: "allow", Suggestion: -1}
	unchanged(t, a, func(a *state.Ask) Result { return ApplyTap(a, tap(0, 'n', 0)) }, "already answered")
	unchanged(t, a, func(a *state.Ask) Result { return ApplyReply(a, 0, "no") }, "already answered")
}

func TestYesNoSuggestion(t *testing.T) {
	a := permissionAsk()
	r := ApplyTap(a, tap(0, 'y', 0))
	if want := (Result{Toast: "approved", Index: 0, Edit: "P\n\n<i>✅ approved from the phone</i>"}); !reflect.DeepEqual(r, want) {
		t.Fatalf("yes: result = %+v, want %+v", r, want)
	}
	if !a.Done || !reflect.DeepEqual(a.Decision, &state.Decision{Behavior: "allow", Suggestion: -1}) {
		t.Fatalf("yes: done = %v, decision = %+v", a.Done, a.Decision)
	}

	a = permissionAsk()
	r = ApplyTap(a, tap(0, 'n', 0))
	if want := (Result{Toast: "denied", Index: 0, Edit: "P\n\n<i>❌ denied from the phone</i>"}); !reflect.DeepEqual(r, want) {
		t.Fatalf("no: result = %+v, want %+v", r, want)
	}
	if !a.Done || !reflect.DeepEqual(a.Decision, &state.Decision{Behavior: "deny", Suggestion: -1, Message: NoMessage, Interrupt: true}) {
		t.Fatalf("no: done = %v, decision = %+v", a.Done, a.Decision)
	}
	if NoMessage != "The user denied this from their phone." {
		t.Fatalf("NoMessage = %q", NoMessage)
	}

	a = permissionAsk()
	r = ApplyTap(a, tap(0, 's', 1))
	if want := (Result{Toast: "approved", Index: 0, Edit: "P\n\n<i>✅ approved from the phone: Yes, always allow Bash(npm test) (this project, this Mac)</i>"}); !reflect.DeepEqual(r, want) {
		t.Fatalf("suggestion: result = %+v, want %+v", r, want)
	}
	if !a.Done || !reflect.DeepEqual(a.Decision, &state.Decision{Behavior: "allow", Suggestion: 1}) {
		t.Fatalf("suggestion: done = %v, decision = %+v", a.Done, a.Decision)
	}

	// Index 0 is the setMode entry, never offered; 2 does not exist; option
	// and question taps do not apply to a permission prompt.
	for _, bad := range []Tap{tap(0, 's', 0), tap(0, 's', 2), tap(0, 'o', 0), tap(0, 'd', 0), tap(0, 'x', 0), tap(1, 'y', 0)} {
		unchanged(t, permissionAsk(), func(a *state.Ask) Result { return ApplyTap(a, bad) }, "this request is gone")
	}
}

// The outcome names the chosen suggestion in full, however long: the
// button showed only its number.
func TestSuggestionOutcomeKeepsFullLabel(t *testing.T) {
	a := permissionAsk()
	a.Suggestions = json.RawMessage(`[{"type":"addRules","rules":[{"toolName":"Read"},{"toolName":"Bash","ruleContent":"npm run test:unit -- --watch=false"},` +
		`{"toolName":"Bash","ruleContent":"git log:*"}],"behavior":"allow","destination":"projectSettings"}]`)
	label := "Yes, always allow Read, Bash(npm run test:unit -- --watch=false), Bash(git log:*) (this project, shared)"
	r := ApplyTap(a, tap(0, 's', 0))
	if want := "P\n\n<i>✅ approved from the phone: " + label + "</i>"; r.Edit != want {
		t.Fatalf("edit = %q, want %q", r.Edit, want)
	}
}

func TestReplyDeniesPermission(t *testing.T) {
	a := permissionAsk()
	r := ApplyReply(a, 0, "use <yarn> & retry")
	if want := (Result{Toast: "denied", Index: 0, Edit: "P\n\n<i>❌ denied from the phone: use &lt;yarn&gt; &amp; retry</i>"}); !reflect.DeepEqual(r, want) {
		t.Fatalf("result = %+v, want %+v", r, want)
	}
	want := &state.Decision{Behavior: "deny", Suggestion: -1, Message: "The user denied this from their phone and replied: use <yarn> & retry"}
	if !a.Done || !reflect.DeepEqual(a.Decision, want) {
		t.Fatalf("done = %v, decision = %+v", a.Done, a.Decision)
	}
	if ReplyPrefix != "The user denied this from their phone and replied: " {
		t.Fatalf("ReplyPrefix = %q", ReplyPrefix)
	}
	unchanged(t, permissionAsk(), func(a *state.Ask) Result { return ApplyReply(a, 1, "x") }, "this request is gone")
}

func TestEmptyReply(t *testing.T) {
	for _, text := range []string{"", "  \n\t"} {
		unchanged(t, questionAsk(), func(a *state.Ask) Result { return ApplyReply(a, 0, text) }, "reply to this message with your answer")
		unchanged(t, permissionAsk(), func(a *state.Ask) Result { return ApplyReply(a, 0, text) }, "reply to this message with your answer")
	}
}

func TestDoneOnlyWhenAllQuestionsAnswered(t *testing.T) {
	a := questionAsk()
	ApplyTap(a, tap(1, 'o', 2))
	ApplyTap(a, tap(1, 'd', 0))
	if a.Done {
		t.Fatal("done with the first question unanswered")
	}
	ApplyReply(a, 0, "Mango, ripe please")
	if !a.Done {
		t.Fatal("not done with every question answered")
	}
	unchanged(t, a, func(a *state.Ask) Result { return ApplyTap(a, tap(0, 'o', 0)) }, "already answered")

	// Unsent questions have no answer slot yet: not done.
	a = questionAsk()
	a.Answers, a.Ticked, a.Texts, a.MessageIDs = []json.RawMessage{nil}, [][]bool{nil}, []string{"Q1"}, []int64{11}
	ApplyTap(a, tap(0, 'o', 0))
	if a.Done {
		t.Fatal("done before the second question was sent")
	}
}

// Answered rebuilds the edit that showed the phone's answer, for each way
// of answering: the relay's sweep shows it on a message whose ask its
// hook collected before the relay could edit.
func TestAnsweredMatchesTheEdit(t *testing.T) {
	cases := []struct {
		name  string
		ask   func() *state.Ask
		apply func(*state.Ask) Result
	}{
		{"yes", permissionAsk, func(a *state.Ask) Result { return ApplyTap(a, tap(0, 'y', 0)) }},
		{"no", permissionAsk, func(a *state.Ask) Result { return ApplyTap(a, tap(0, 'n', 0)) }},
		{"suggestion", permissionAsk, func(a *state.Ask) Result { return ApplyTap(a, tap(0, 's', 1)) }},
		{"swipe-reply", permissionAsk, func(a *state.Ask) Result { return ApplyReply(a, 0, "use yarn, not <npm>") }},
		{"an option", questionAsk, func(a *state.Ask) Result { return ApplyTap(a, tap(0, 'o', 1)) }},
		{"free text", questionAsk, func(a *state.Ask) Result { return ApplyReply(a, 0, "Mango, ripe") }},
		{"ticked", questionAsk, func(a *state.Ask) Result {
			ApplyTap(a, tap(1, 'o', 0))
			ApplyTap(a, tap(1, 'o', 2))
			return ApplyTap(a, tap(1, 'd', 0))
		}},
	}
	for _, c := range cases {
		a := c.ask()
		res := c.apply(a)
		if res.Edit == "" {
			t.Fatalf("%s: no edit", c.name)
		}
		if got, ok := Answered(a, res.Index); !ok || got != res.Edit {
			t.Errorf("%s: Answered = %q, %v; want %q", c.name, got, ok, res.Edit)
		}
	}
	if _, ok := Answered(questionAsk(), 0); ok {
		t.Error("an unanswered question reads as answered")
	}
	if _, ok := Answered(permissionAsk(), 0); ok {
		t.Error("an undecided prompt reads as answered")
	}
}
