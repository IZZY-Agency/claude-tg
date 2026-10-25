package ask

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/IZZY-Agency/claude-tg/internal/state"
)

func compact(t *testing.T, s string) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(s)); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestOutputQuestionEchoesQuestionsExactly(t *testing.T) {
	// Odd key order, whitespace, unknown fields, a null preview and HTML
	// specials: Claude must get the questions value back unchanged.
	questions := `[ { "multiSelect" : false, "question": "Which <fruit> & why?",
		"options": [ {"label":"Banana","description":"x","preview":null}, {"label":"Kiwi","description":"y"} ],
		"header":"Fruit", "zz":{"b":1,"a":[2, 1]} },
		{"question":"Which colours?","header":"Colours","multiSelect":true,"options":[{"label":"Red","description":""},{"label":"Blue","description":""}]} ]`
	a := &state.Ask{ID: testID, Tool: "AskUserQuestion", Done: true,
		ToolInput: json.RawMessage(`{ "metadata": {"source":"x"}, "questions": ` + questions + ` }`),
		// The store writes the ask indented, so answers come back with whitespace.
		Answers: []json.RawMessage{json.RawMessage(`"Banana"`), json.RawMessage("[\n  \"Red\",\n  \"Blue\"\n]")}}
	out, err := Output(a)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow","updatedInput":{"questions":` +
		compact(t, questions) + `,"answers":{"Which <fruit> & why?":"Banana","Which colours?":["Red","Blue"]}}}}}`
	if string(out) != want {
		t.Fatalf("Output =\n%s\nwant\n%s", out, want)
	}
	var got struct {
		HookSpecificOutput struct {
			Decision struct {
				UpdatedInput struct {
					Questions json.RawMessage `json:"questions"`
				} `json:"updatedInput"`
			} `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if q := string(got.HookSpecificOutput.Decision.UpdatedInput.Questions); q != compact(t, questions) {
		t.Fatalf("questions bytes =\n%s\nwant\n%s", q, compact(t, questions))
	}
}

func TestOutputAllow(t *testing.T) {
	a := permissionAsk()
	a.Done, a.Decision = true, &state.Decision{Behavior: "allow", Suggestion: -1}
	out, err := Output(a)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`; string(out) != want {
		t.Fatalf("Output = %s, want %s", out, want)
	}
}

func TestOutputSuggestion(t *testing.T) {
	chosen := `{ "type": "addRules", "rules": [ {"toolName":"Bash","ruleContent":"npm <test> & lint"} ],
		"behavior": "allow", "destination": "localSettings" }`
	a := &state.Ask{ID: testID, Tool: "Bash", Done: true, Decision: &state.Decision{Behavior: "allow", Suggestion: 1},
		Suggestions: json.RawMessage(`[ {"type":"setMode","mode":"acceptEdits","destination":"session"}, ` + chosen + ` ]`)}
	out, err := Output(a)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow","updatedPermissions":[` + compact(t, chosen) + `]}}}`
	if string(out) != want {
		t.Fatalf("Output =\n%s\nwant\n%s", out, want)
	}
	// Only an offered suggestion is ever granted: 0 is the setMode entry, 2
	// does not exist. No output means no decision.
	for _, n := range []int{0, 2} {
		a.Decision.Suggestion = n
		if out, err := Output(a); err == nil || out != nil {
			t.Errorf("suggestion %d: Output = %s, %v; want nil and an error", n, out, err)
		}
	}
}

func TestOutputAnswersInQuestionOrder(t *testing.T) {
	// "Which fruit?" comes first but sorts after "Which colours?".
	a := questionAsk()
	ApplyTap(a, tap(0, 'o', 1))
	ApplyTap(a, tap(1, 'o', 0))
	ApplyTap(a, tap(1, 'o', 2))
	ApplyTap(a, tap(1, 'd', 0))
	out, err := Output(a)
	if err != nil {
		t.Fatal(err)
	}
	var in struct{ Questions json.RawMessage }
	if err := json.Unmarshal([]byte(spikeInput), &in); err != nil {
		t.Fatal(err)
	}
	want := `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow","updatedInput":{"questions":` +
		string(in.Questions) + `,"answers":{"Which fruit?":"Banana","Which colours?":["Red","Blue"]}}}}}`
	if string(out) != want {
		t.Fatalf("Output =\n%s\nwant\n%s", out, want)
	}

	// Answers are keyed by question text: two questions with one text
	// cannot both be answered, so there is no decision at all.
	same := `{"questions":[{"question":"Same?","options":[{"label":"A"}]},{"question":"Other?","options":[{"label":"B"}]},{"question":"Same?","options":[{"label":"C"}]}]}`
	a = &state.Ask{ID: testID, Tool: "AskUserQuestion", Done: true, ToolInput: json.RawMessage(same),
		Answers: []json.RawMessage{json.RawMessage(`"A"`), json.RawMessage(`"B"`), json.RawMessage(`"C"`)}}
	if out, err = Output(a); err == nil || out != nil {
		t.Fatalf("Output = %s, %v; want no decision and an error", out, err)
	}
}

func TestOutputDenyInterrupt(t *testing.T) {
	a := permissionAsk()
	a.Done, a.Decision = true, &state.Decision{Behavior: "deny", Suggestion: -1, Message: NoMessage, Interrupt: true}
	out, err := Output(a)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"The user denied this from their phone.","interrupt":true}}}`; string(out) != want {
		t.Fatalf("Output = %s, want %s", out, want)
	}

	a.Decision = &state.Decision{Behavior: "deny", Suggestion: -1, Message: ReplyPrefix + "use <yarn> & retry"}
	out, err = Output(a)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"The user denied this from their phone and replied: use <yarn> & retry"}}}`; string(out) != want {
		t.Fatalf("Output = %s, want %s", out, want)
	}
}

func TestOutputNotDone(t *testing.T) {
	q := questionAsk()
	q.Answers[0] = json.RawMessage(`"Banana"`)
	p := permissionAsk()
	broken := questionAsk()
	broken.Done = true // flagged done with an answer missing
	for _, a := range []*state.Ask{q, p, broken} {
		if out, err := Output(a); err == nil || out != nil {
			t.Errorf("Output(%+v) = %s, %v; want nil and an error", a, out, err)
		}
	}
	p.Done = true // done without a decision
	if out, err := Output(p); err == nil || out != nil {
		t.Errorf("Output without a decision = %s, %v", out, err)
	}
}
