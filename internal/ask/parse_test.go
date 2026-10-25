package ask

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/IZZY-Agency/claude-tg/internal/state"
)

// spikeInput is the two-question AskUserQuestion input of spec fact 4: a
// single-choice question answered with a label and a multi-select question
// answered with an array of labels.
const spikeInput = `{"questions":[` +
	`{"question":"Which fruit?","header":"Fruit","options":[` +
	`{"label":"Apple","description":"Crisp and red"},` +
	`{"label":"Banana","description":"Sweet and yellow"},` +
	`{"label":"Mango","description":"Ripe and juicy"}],"multiSelect":false},` +
	`{"question":"Which colours?","header":"Colours","options":[` +
	`{"label":"Red","description":"Warm"},` +
	`{"label":"Green","description":""},` +
	`{"label":"Blue","description":"Cool","preview":"#0000ff"}],"multiSelect":true}]}`

// spikeSuggestions is spec fact 6's permission_suggestions for `touch
// spike1.txt` in a new folder.
const spikeSuggestions = `[` +
	`{"type":"addDirectories","directories":["/Users/alex/hookspike/proj"],"destination":"session"},` +
	`{"type":"setMode","mode":"acceptEdits","destination":"session"}]`

func TestParseQuestionsSpikeShape(t *testing.T) {
	qs, err := ParseQuestions(json.RawMessage(spikeInput))
	if err != nil {
		t.Fatal(err)
	}
	want := []Question{
		{Question: "Which fruit?", Header: "Fruit", Options: []Option{
			{Label: "Apple", Description: "Crisp and red"},
			{Label: "Banana", Description: "Sweet and yellow"},
			{Label: "Mango", Description: "Ripe and juicy"},
		}},
		{Question: "Which colours?", Header: "Colours", MultiSelect: true, Options: []Option{
			{Label: "Red", Description: "Warm"},
			{Label: "Green"},
			{Label: "Blue", Description: "Cool", Preview: "#0000ff"},
		}},
	}
	if !reflect.DeepEqual(qs, want) {
		t.Fatalf("ParseQuestions = %+v\nwant %+v", qs, want)
	}
	for _, bad := range []string{``, `not json`, `{}`, `{"questions":[]}`, `{"questions":{}}`} {
		if _, err := ParseQuestions(json.RawMessage(bad)); err == nil {
			t.Errorf("ParseQuestions(%q) succeeded", bad)
		}
	}
}

func TestParseSuggestionsDropsSetMode(t *testing.T) {
	got := ParseSuggestions(json.RawMessage(spikeSuggestions))
	want := []Suggestion{{Index: 0, Label: "Yes, allow access to …/hookspike/proj for this session", Session: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseSuggestions = %+v, want %+v", got, want)
	}
}

func TestSuggestionLabels(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"npm test"}],"behavior":"allow","destination":"localSettings"}`,
			"Yes, always allow Bash(npm test) (this project, this Mac)"},
		{`{"type":"addRules","rules":[{"toolName":"Read"},{"toolName":"Bash","ruleContent":"ls:*"}],"behavior":"allow","destination":"projectSettings"}`,
			"Yes, always allow Read, Bash(ls:*) (this project, shared)"},
		{`{"type":"addRules","rules":[{"toolName":"WebFetch"}],"behavior":"allow","destination":"userSettings"}`,
			"Yes, always allow WebFetch (all projects)"},
		// Ruling R8: "always" only for a destination that persists.
		{`{"type":"addRules","rules":[{"toolName":"Bash"}],"behavior":"allow","destination":"session"}`,
			"Yes, allow Bash for this session"},
		{`{"type":"addDirectories","directories":["/srv/www/acme/back/"],"destination":"localSettings"}`,
			"Yes, always allow access to …/acme/back (this project, this Mac)"},
		{`{"type":"addDirectories","directories":["/srv/www/acme/back"],"destination":"userSettings"}`,
			"Yes, always allow access to …/acme/back (all projects)"},
		{`{"type":"addDirectories","directories":["/tmp/x","/data"],"destination":"session"}`,
			"Yes, allow access to /tmp/x, /data for this session"},
		// Not offered: mode changes, removals, replacements, non-allow
		// rules, empty lists and destinations tg cannot describe.
		{`{"type":"setMode","mode":"acceptEdits","destination":"session"}`, ""},
		{`{"type":"replaceRules","rules":[{"toolName":"Bash"}],"behavior":"allow","destination":"session"}`, ""},
		{`{"type":"removeRules","rules":[{"toolName":"Bash"}],"behavior":"allow","destination":"session"}`, ""},
		{`{"type":"removeDirectories","directories":["/tmp/x"],"destination":"session"}`, ""},
		{`{"type":"addRules","rules":[{"toolName":"Bash"}],"behavior":"deny","destination":"session"}`, ""},
		{`{"type":"addRules","rules":[],"behavior":"allow","destination":"session"}`, ""},
		{`{"type":"addDirectories","directories":[],"destination":"session"}`, ""},
		{`{"type":"addRules","rules":[{"toolName":"Bash"}],"behavior":"allow","destination":"cliArg"}`, ""},
		{`"garbage"`, ""},
	}
	for _, c := range cases {
		got := ParseSuggestions(json.RawMessage("[" + c.in + "]"))
		var want []Suggestion
		if c.want != "" {
			// Every case with destination "session" says so in its label.
			want = []Suggestion{{Index: 0, Label: c.want, Session: strings.HasSuffix(c.want, " for this session")}}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ParseSuggestions(%s) = %+v, want %+v", c.in, got, want)
		}
	}
	for _, bad := range []string{``, `null`, `{}`, `not json`} {
		if got := ParseSuggestions(json.RawMessage(bad)); got != nil {
			t.Errorf("ParseSuggestions(%q) = %+v, want nil", bad, got)
		}
	}
}

// encoding/json's struct decoding matches keys case-insensitively and lets
// the last near-duplicate win; Claude Code reads exact keys. The phone must
// show, and answer, exactly what Output echoes back.
func TestQuestionsReadByExactKey(t *testing.T) {
	a := `[{"question":"A?","Question":"B?","header":"A","options":[{"label":"A1","LABEL":"B1","description":"a"}],"multiSelect":false}]`
	b := `[{"question":"B?","header":"B","options":[{"label":"B1"}],"multiSelect":true}]`
	input := `{"questions":` + a + `,"QUESTIONS":` + b + `}`
	qs, err := ParseQuestions(json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	want := []Question{{Question: "A?", Header: "A", Options: []Option{{Label: "A1", Description: "a"}}}}
	if !reflect.DeepEqual(qs, want) {
		t.Fatalf("ParseQuestions = %+v, want %+v", qs, want)
	}
	if html, _ := QuestionMessage("n", qs[0], 1, 1); !strings.Contains(html, "A?") || !strings.Contains(html, "A1") || strings.Contains(html, "B") {
		t.Fatalf("rendered %q", html)
	}
	ask := &state.Ask{ID: testID, Tool: "AskUserQuestion", ToolInput: json.RawMessage(input), Texts: []string{"Q"}, MessageIDs: []int64{1}}
	if r := ApplyTap(ask, tap(0, 'o', 0)); r.Toast != "sent" || string(ask.Answers[0]) != `"A1"` || !ask.Done {
		t.Fatalf("tap: %+v, answers %s, done %v", r, ask.Answers, ask.Done)
	}
	out, err := Output(ask)
	if err != nil {
		t.Fatal(err)
	}
	if want := `"updatedInput":{"questions":` + compact(t, a) + `,"answers":{"A?":"A1"}}`; !strings.Contains(string(out), want) {
		t.Fatalf("Output = %s, want it to contain %s", out, want)
	}
}

func TestSuggestionsReadByExactKey(t *testing.T) {
	if got := ParseSuggestions(json.RawMessage(`[{"type":"setMode","TYPE":"addRules","mode":"acceptEdits",` +
		`"rules":[{"toolName":"Bash"}],"behavior":"allow","destination":"session"}]`)); got != nil {
		t.Errorf("setMode with a TYPE twin offered: %+v", got)
	}
	got := ParseSuggestions(json.RawMessage(`[{"type":"addRules","behavior":"allow","Behavior":"deny",` +
		`"rules":[{"toolName":"Bash","TOOLNAME":"Read","ruleContent":"ls","RuleContent":"rm -rf /"}],` +
		`"destination":"localSettings","DESTINATION":"session"}]`))
	if want := []Suggestion{{Index: 0, Label: "Yes, always allow Bash(ls) (this project, this Mac)"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("ParseSuggestions = %+v, want %+v", got, want)
	}
}
