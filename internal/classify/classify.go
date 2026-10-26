// Package classify asks TypeSafe's Jev model two yes/no questions about an
// answer, and decides in code whether it should be sent as a voice message.
package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultTimeout is the limit for one request when Client.Timeout is 0.
	DefaultTimeout = 5 * time.Second
	// defaultBaseURL is TypeSafe's API when Client.BaseURL is empty.
	defaultBaseURL = "https://api.typesafe.ai"
	// maxBody caps how much of a reply is read.
	maxBody = 1 << 20
	// detailRunes caps the detail an APIError carries.
	detailRunes = 200
)

// ErrRejected is what Check returns when TypeSafe answers 401.
var ErrRejected = errors.New("key rejected")

// ErrTooLarge is what a reply over 1 MiB is reported as.
var ErrTooLarge = errors.New("typesafe: reply exceeds 1 MiB")

// APIError is a non-200 reply from TypeSafe. Detail is the server's own
// text and may echo the request, so it is for the owner's terminal only
// (tg classify); a caller that logs, such as the Stop hook, must log Status
// and the status text, never Detail.
type APIError struct {
	Status int
	Detail string // whitespace collapsed, at most 200 runes, never the key
}

// Error implements error.
func (e *APIError) Error() string {
	d := e.Detail
	if d == "" {
		d = statusText(e.Status)
	}
	return fmt.Sprintf("typesafe: HTTP %d: %s", e.Status, d)
}

// Client talks to TypeSafe. The zero value of every field but Key is usable.
type Client struct {
	Key     string
	BaseURL string        // "" = https://api.typesafe.ai; tests point it at httptest
	HTTP    *http.Client  // nil = http.DefaultClient
	Timeout time.Duration // 0 = DefaultTimeout
}

// Judgment is Jev's probability for each question, 0 to 1.
type Judgment struct {
	Factual, Listenable float64
	Model               string // as the reply names it
}

// Decide reports whether both probabilities reach Threshold.
func Decide(j Judgment) bool {
	return j.Factual >= Threshold && j.Listenable >= Threshold
}

// questionBody is a question in a request.
type questionBody struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

// asBody renders q for a request.
func (q question) asBody() questionBody {
	return questionBody{"noul", q.Instructions, map[string]string{"true": q.True, "false": q.False}}
}

// Judge asks both questions about answer; prompt is the prompt it replies
// to and may be empty. One attempt, bounded by Timeout and ctx.
func (c *Client) Judge(ctx context.Context, prompt, answer string) (Judgment, error) {
	state := map[string]string{"answer": answer}
	if prompt != "" {
		state["question"] = prompt
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	err := enc.Encode(map[string]any{
		"model": Model,
		"state": state,
		"questions": map[string]questionBody{
			"factual":    factual.asBody(),
			"listenable": listenable.asBody(),
		},
	})
	if err != nil {
		return Judgment{}, err
	}
	body, err := c.do(ctx, http.MethodPost, "/v1/systemone", &buf)
	if err != nil {
		return Judgment{}, err
	}
	var rep struct {
		Model   string `json:"model"`
		Answers map[string]struct {
			Noul *float64 `json:"noul"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(body, &rep); err != nil {
		return Judgment{}, fmt.Errorf("typesafe: bad reply: %w", err)
	}
	var p [2]float64
	for i, id := range [...]string{"factual", "listenable"} {
		a, ok := rep.Answers[id]
		if !ok || a.Noul == nil {
			return Judgment{}, fmt.Errorf("typesafe: reply has no %s answer", id)
		}
		if v := *a.Noul; !(v >= 0 && v <= 1) {
			return Judgment{}, fmt.Errorf("typesafe: %s answer %v is outside 0..1", id, v)
		}
		p[i] = *a.Noul
	}
	return Judgment{Factual: p[0], Listenable: p[1], Model: rep.Model}, nil
}

// Check verifies the key with a call that bills no tokens. A rejected key is
// ErrRejected.
func (c *Client) Check(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodGet, "/v1/models", nil)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
		return ErrRejected
	}
	return err
}

// do sends one request and returns the body of a 200 reply. Any other status
// is an *APIError; a transport failure is wrapped as it came (a *url.Error
// names the URL, which holds no key).
func (c *Client) do(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	t := c.Timeout
	if t == 0 {
		t = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, t)
	defer cancel()
	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, body)
	if err != nil {
		return nil, fmt.Errorf("typesafe: %w", c.scrub(err))
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("typesafe: %w", c.scrub(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("typesafe: reading reply: %w", c.scrub(err))
	}
	if len(data) > maxBody {
		return nil, ErrTooLarge
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{resp.StatusCode, c.detail(resp.StatusCode, data)}
	}
	return data, nil
}

// scrub returns err unchanged unless its text holds the key, in which case it
// returns a keyless error.
func (c *Client) scrub(err error) error {
	if c.Key != "" && strings.Contains(err.Error(), c.Key) {
		return errors.New("invalid request")
	}
	return err
}

// detail makes an error body into one short line without the key, and never
// echoes the request: 400 and 422 carry {"detail": ...}, of which only a
// string or the msg and loc of list items are kept. Anything unrecognised is
// the status text.
func (c *Client) detail(status int, data []byte) string {
	s := ""
	if status == http.StatusBadRequest || status == http.StatusUnprocessableEntity {
		var d struct {
			Detail json.RawMessage `json:"detail"`
		}
		if json.Unmarshal(data, &d) == nil {
			var str string
			var items []struct {
				Loc []any  `json:"loc"`
				Msg string `json:"msg"`
			}
			switch {
			case json.Unmarshal(d.Detail, &str) == nil:
				s = str
			case json.Unmarshal(d.Detail, &items) == nil:
				var parts []string
				for _, it := range items {
					p := it.Msg
					if len(it.Loc) > 0 {
						loc := make([]string, len(it.Loc))
						for i, l := range it.Loc {
							loc[i] = fmt.Sprint(l)
						}
						p = strings.Join(loc, ".") + ": " + p // body.state: Field required
					}
					parts = append(parts, p)
				}
				s = strings.Join(parts, "; ")
			}
		}
	}
	if c.Key != "" {
		s = strings.ReplaceAll(s, c.Key, "[key]")
	}
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) == 0 {
		return statusText(status)
	}
	if len(r) > detailRunes {
		r = r[:detailRunes]
	}
	return string(r)
}

// statusText names a status; Go knows no name for TypeSafe's 529.
func statusText(status int) string {
	if status == 529 {
		return "Overloaded"
	}
	if t := http.StatusText(status); t != "" {
		return t
	}
	return "error"
}
