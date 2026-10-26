//go:build liveprobe

package classify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/IZZY-Agency/claude-tg/internal/render"
	"github.com/IZZY-Agency/claude-tg/internal/speech"
)

// TestLiveProbe runs real question/answer pairs through TypeSafe, for
// tuning the questions and the threshold (questions.go) and for checking
// them again before Model moves to a new Jev version. It is not part of
// the suite: it needs the build tag and a key, and it bills the key.
//
//	TYPESAFE_API_KEY=… TS_PROBE_IN=pairs.json TS_PROBE_OUT=results.tsv \
//	  go test -tags liveprobe -run TestLiveProbe -count=1 ./internal/classify/
//
// pairs.json is a list of {"id", "q", "a", "label"} objects, kept outside
// the repository: real answers are private. The question and the answer
// are cleaned as the hook cleans them (the key, the bot token when
// TELEGRAM_BOT_TOKEN holds it, then render.Redact), then go through the
// hard rules (speech.Choose) as the Stop hook's do. results.tsv gets one
// line per pair: id, label, the rule's reason or whether the opening
// paragraph was judged, both probabilities and the decision; a TypeSafe
// error by its status only.
func TestLiveProbe(t *testing.T) {
	key, in, out := os.Getenv("TYPESAFE_API_KEY"), os.Getenv("TS_PROBE_IN"), os.Getenv("TS_PROBE_OUT")
	if key == "" || in == "" || out == "" {
		t.Skip("set TYPESAFE_API_KEY, TS_PROBE_IN and TS_PROBE_OUT")
	}
	var pairs []struct{ ID, Q, A, Label string }
	b, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &pairs); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c := &Client{Key: key}
	// As the hook's Sender.Clean: the known secrets first (the key, and the
	// bot token when TELEGRAM_BOT_TOKEN holds it), then the generic redaction.
	secrets := []string{key, os.Getenv("TELEGRAM_BOT_TOKEN")}
	clean := func(s string) string {
		for _, sec := range secrets {
			if sec != "" {
				s = strings.ReplaceAll(s, sec, "[redacted]")
			}
		}
		return render.Redact(s)
	}
	for _, p := range pairs {
		q, a := clean(p.Q), clean(p.A)
		ch := speech.Choose(a)
		if ch.Reason != "" {
			fmt.Fprintf(f, "%s\t%s\t%s\n", p.ID, p.Label, ch.Reason)
			continue
		}
		j, err := c.Judge(context.Background(), q, ch.Markdown)
		if err != nil {
			// By status only, as the hook logs it: an API error's detail may
			// echo the request.
			var api *APIError
			if errors.As(err, &api) {
				err = fmt.Errorf("HTTP %d: %s", api.Status, statusText(api.Status))
			}
			fmt.Fprintf(f, "%s\t%s\terror: %v\n", p.ID, p.Label, err)
			continue
		}
		fmt.Fprintf(f, "%s\t%s\topening=%v\t%.2f\t%.2f\t%v\n", p.ID, p.Label, ch.Opening, j.Factual, j.Listenable, Decide(j))
	}
}
