package hooks

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/IZZY-Agency/claude-tg/internal/classify"
	"github.com/IZZY-Agency/claude-tg/internal/speech"
	"github.com/IZZY-Agency/claude-tg/internal/state"
)

// voiceCheck decides whether a candidate that passed the hard rules
// (speakAnswer: speech.Choose) is spoken (spec
// 2026-10-26-voice-classifier-design.md, "Behaviour"). TypeSafe's Jev
// judges choice.Markdown, the whole answer or its opening paragraph,
// cleaned as a 💬 message is (send.Sender.Clean), as is question: the
// phone's prompt, "" when the turn's prompt is not to be sent. Jev has
// ctx and at most classify.DefaultTimeout, and classify.Decide turns its
// judgment into the decision. Any failure means text. One line goes to the
// log for the decision (logVoice), with the probabilities or the reason
// (judgeFailure), never a text; a judged opening paragraph says so.
func voiceCheck(ctx context.Context, d Deps, sess *state.Session, choice speech.Choice, question string) bool {
	ctx, cancel := context.WithTimeout(ctx, classify.DefaultTimeout)
	defer cancel()
	j, err := d.Classifier.Judge(ctx, question, choice.Markdown)
	if err != nil {
		d.logVoice(sess, "text (typesafe: "+judgeFailure(d, err)+")")
		return false
	}
	spoken := classify.Decide(j)
	result, part := "text", ""
	if spoken {
		result = "voice"
	}
	if choice.Opening {
		part = "opening paragraph; "
	}
	d.logVoice(sess, fmt.Sprintf("%s (%sfactual %.2f, listenable %.2f)", result, part, j.Factual, j.Listenable))
	return spoken
}

// judgeFailure is a failed judgment's reason for the log, after
// "typesafe: " (classify's errors name TypeSafe already; the log names it
// once). A *classify.APIError gives its status and the status's name only
// ("HTTP 422: Unprocessable Entity"): its detail is the server's own
// text, which may echo the request's prompt or answer. Any other error, a
// transport failure or a malformed reply, gives its text, which classify
// keeps free of the key, cleaned as a message is.
func judgeFailure(d Deps, err error) string {
	var ae *classify.APIError
	if errors.As(err, &ae) {
		err = &classify.APIError{Status: ae.Status} // no detail: its Error names the status
	}
	return d.Sender.Clean(strings.TrimPrefix(err.Error(), "typesafe: "))
}

// logVoice logs a voice check's decision for sess, the session's name
// cleaned as a message is.
func (d Deps) logVoice(sess *state.Session, decision string) {
	d.logf("voice check (%s): %s", d.Sender.Clean(spokenName(sess)), decision)
}
