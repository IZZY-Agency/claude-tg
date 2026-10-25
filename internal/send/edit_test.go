package send

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

type editCall struct {
	text, parseMode string
	kb              [][]telegram.Button
}

// editRecorder records EditMessage calls and returns scripted errors in order.
type editRecorder struct {
	calls  []editCall
	script []error
}

func (e *editRecorder) EditMessage(_ context.Context, chatID, messageID int64, text, parseMode string, kb [][]telegram.Button) error {
	e.calls = append(e.calls, editCall{text, parseMode, kb})
	if len(e.script) > 0 {
		err := e.script[0]
		e.script = e.script[1:]
		return err
	}
	return nil
}

func TestEditHTMLRedactsBetweenTags(t *testing.T) {
	rec := &editRecorder{}
	kb := [][]telegram.Button{{{Text: "Yes", Data: "a:1"}}}
	if err := EditHTML(context.Background(), rec, 1, 7, "<b>key=abcdefgh123</b>\n\n<i>→ ok</i>", kb); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 1 || rec.calls[0].text != "<b>key=[redacted]</b>\n\n<i>→ ok</i>" || rec.calls[0].parseMode != "HTML" ||
		fmt.Sprint(rec.calls[0].kb) != fmt.Sprint(kb) {
		t.Fatalf("calls=%+v", rec.calls)
	}
}

// A 400 on the HTML is retried once as plain text, as sends are: tags
// dropped, entities decoded, redacted again (a value split by a tag is
// whole in the plain text), keyboard kept.
func TestEditHTMLFallsBackToPlainOn400(t *testing.T) {
	rec := &editRecorder{script: []error{&telegram.APIError{Code: 400, Description: "Bad Request: can't parse entities"}}}
	kb := [][]telegram.Button{{{Text: "Yes", Data: "a:1"}}}
	html := "🔐 <b>a&lt;b&gt;</b>\nneeds permission: <code>password=</code>s3cr3t1234 <b>x</b>ghp_abcdefghijklmnopqrstuvwxyz0123\n\n<i>✅ approved from the phone</i>"
	if err := EditHTML(context.Background(), rec, 1, 7, html, kb); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 2 {
		t.Fatalf("calls=%+v", rec.calls)
	}
	got := rec.calls[1]
	if got.parseMode != "" || got.text != "🔐 a<b>\nneeds permission: password=[redacted] x[redacted]\n\n✅ approved from the phone" || fmt.Sprint(got.kb) != fmt.Sprint(kb) {
		t.Fatalf("fallback=%+v", got)
	}
}

func TestEditHTMLOtherOutcomes(t *testing.T) {
	// Already showing that text: nothing to fall back for, not a failure.
	rec := &editRecorder{script: []error{&telegram.APIError{Code: 400, Description: "Bad Request: message is not modified: specified new message content and reply markup are exactly the same"}}}
	if err := EditHTML(context.Background(), rec, 1, 7, "<b>x</b>", nil); err != nil || len(rec.calls) != 1 {
		t.Fatalf("not modified: err=%v calls=%+v", err, rec.calls)
	}
	// The plain text is refused too: that error is returned.
	refused := &telegram.APIError{Code: 400, Description: "Bad Request: message to edit not found"}
	rec = &editRecorder{script: []error{refused, refused}}
	if err := EditHTML(context.Background(), rec, 1, 7, "<b>x</b>", nil); !errors.Is(err, refused) || len(rec.calls) != 2 {
		t.Fatalf("refused: err=%v calls=%+v", err, rec.calls)
	}
	// Anything but a 400 is returned as is, without a retry.
	net := &telegram.NetError{Err: errors.New("connection reset")}
	rec = &editRecorder{script: []error{net}}
	if err := EditHTML(context.Background(), rec, 1, 7, "<b>x</b>", nil); !errors.Is(err, net) || len(rec.calls) != 1 {
		t.Fatalf("network: err=%v calls=%+v", err, rec.calls)
	}
}
