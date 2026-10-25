package ask

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/IZZY-Agency/claude-tg/internal/state"
)

type decisionJSON struct {
	Behavior           string            `json:"behavior"`
	UpdatedInput       *updatedInput     `json:"updatedInput,omitempty"`
	UpdatedPermissions []json.RawMessage `json:"updatedPermissions,omitempty"`
	Message            string            `json:"message,omitempty"`
	Interrupt          bool              `json:"interrupt,omitempty"`
}

type updatedInput struct {
	Questions json.RawMessage `json:"questions"`
	Answers   json.RawMessage `json:"answers"` // an object in question order
}

// Output is the PermissionRequest decision JSON the waiting hook prints for
// an ask answered from the phone. The questions and the chosen suggestion
// are echoed with their values and key order unchanged (whitespace
// compacted), answers are keyed in question order, and no text is
// HTML-escaped. A suggestion tg does not offer is never granted: Output
// returns an error, and the hook then gives no decision.
func Output(a *state.Ask) ([]byte, error) {
	if !a.Done {
		return nil, errors.New("ask: not answered from the phone")
	}
	d, err := decision(a)
	if err != nil {
		return nil, err
	}
	var out struct {
		HookSpecificOutput struct {
			HookEventName string       `json:"hookEventName"`
			Decision      decisionJSON `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	out.HookSpecificOutput.HookEventName = "PermissionRequest"
	out.HookSpecificOutput.Decision = d
	return encode(out)
}

// encode is json.Marshal without HTML escaping: Claude gets "<", ">" and
// "&" as they were typed. Raw values in v come out compacted.
func encode(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

func decision(a *state.Ask) (decisionJSON, error) {
	if a.Tool == questionTool {
		questions, err := questionsValue(a.ToolInput)
		if err != nil {
			return decisionJSON{}, err
		}
		qs, err := ParseQuestions(a.ToolInput)
		if err != nil {
			return decisionJSON{}, err
		}
		answers, err := answersObject(qs, a.Answers)
		if err != nil {
			return decisionJSON{}, err
		}
		return decisionJSON{Behavior: "allow", UpdatedInput: &updatedInput{Questions: questions, Answers: answers}}, nil
	}
	d := a.Decision
	switch {
	case d == nil:
		return decisionJSON{}, errors.New("ask: no decision")
	case d.Behavior == "deny":
		return decisionJSON{Behavior: "deny", Message: d.Message, Interrupt: d.Interrupt}, nil
	case d.Behavior != "allow":
		return decisionJSON{}, fmt.Errorf("ask: unknown behavior %q", d.Behavior)
	case d.Suggestion < 0:
		return decisionJSON{Behavior: "allow"}, nil
	}
	// Grant only a suggestion tg offers: never a mode change or a removal,
	// whatever the record says.
	for _, s := range ParseSuggestions(a.Suggestions) {
		if s.Index != d.Suggestion {
			continue
		}
		var list []json.RawMessage
		if err := json.Unmarshal(a.Suggestions, &list); err != nil {
			return decisionJSON{}, err
		}
		return decisionJSON{Behavior: "allow", UpdatedPermissions: list[s.Index : s.Index+1]}, nil
	}
	return decisionJSON{}, fmt.Errorf("ask: permission suggestion %d is not offered", d.Suggestion)
}

// answersObject is the "answers" object, keys in question order (a Go map
// would sort them) and values unchanged. The question texts are distinct
// (ParseQuestions).
func answersObject(qs []Question, answers []json.RawMessage) (json.RawMessage, error) {
	b := []byte{'{'}
	for i, q := range qs {
		if i >= len(answers) || answers[i] == nil {
			return nil, fmt.Errorf("ask: question %d has no answer", i+1)
		}
		key, err := encode(q.Question)
		if err != nil {
			return nil, err
		}
		if len(b) > 1 {
			b = append(b, ',')
		}
		b = append(append(append(b, key...), ':'), answers[i]...)
	}
	return append(b, '}'), nil
}
