package classify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const okReply = `{"model":"jev-1.14.0","answers":{"factual":{"type":"noul","noul":0.94},"listenable":{"type":"noul","noul":0.91}}}`

// serve starts a fake TypeSafe whose handler is h.
func serve(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return &Client{Key: "k-123", BaseURL: s.URL}
}

func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

// paddedReply is a valid reply of exactly n bytes.
func paddedReply(n int) string {
	const head = `{"model":"jev-1.14.0","pad":"`
	const tail = `","answers":{"factual":{"type":"noul","noul":0.94},"listenable":{"type":"noul","noul":0.91}}}`
	return head + strings.Repeat("x", n-len(head)-len(tail)) + tail
}

func TestJudgeReplySizeCap(t *testing.T) {
	c := serve(t, reply(200, paddedReply(1<<20)))
	if _, err := c.Judge(context.Background(), "q", "a"); err != nil {
		t.Fatalf("exactly 1 MiB: %v", err)
	}
	c = serve(t, reply(200, paddedReply(1<<20+1)))
	if _, err := c.Judge(context.Background(), "q", "a"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("1 MiB + 1: %v", err)
	}
}

func TestJudgeRequestBody(t *testing.T) {
	var mu sync.Mutex
	var method, path, auth, ctype string
	var raw []byte
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		method, path = r.Method, r.URL.Path
		auth, ctype = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		raw, _ = io.ReadAll(r.Body)
		mu.Unlock()
		io.WriteString(w, okReply)
	})
	q, a := "Какая погода в Париже?", "Il fait 14 °C et il pleut."
	if _, err := c.Judge(context.Background(), q, a); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if method != "POST" || path != "/v1/systemone" {
		t.Fatalf("got %s %s", method, path)
	}
	if auth != "Bearer k-123" || ctype != "application/json" {
		t.Fatalf("headers: %q %q", auth, ctype)
	}
	var got struct {
		Model     string            `json:"model"`
		State     map[string]string `json:"state"`
		Questions map[string]struct {
			Type         string            `json:"type"`
			Instructions string            `json:"instructions"`
			Criteria     map[string]string `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "jev-1.14.0" {
		t.Fatalf("model %q", got.Model)
	}
	if !reflect.DeepEqual(got.State, map[string]string{"question": q, "answer": a}) {
		t.Fatalf("state %v", got.State)
	}
	if len(got.Questions) != 2 {
		t.Fatalf("questions %v", got.Questions)
	}
	for id, want := range map[string]question{"factual": factual, "listenable": listenable} {
		g, ok := got.Questions[id]
		if !ok {
			t.Fatalf("missing %s", id)
		}
		if g.Type != "noul" || g.Instructions != want.Instructions ||
			g.Criteria["true"] != want.True || g.Criteria["false"] != want.False {
			t.Fatalf("%s: %+v", id, g)
		}
	}
	if !strings.Contains(string(raw), "Какая") || strings.Contains(string(raw), `\u`) {
		t.Fatalf("text not sent verbatim: %s", raw)
	}
}

func TestJudgeOmitsEmptyQuestion(t *testing.T) {
	var mu sync.Mutex
	var raw []byte
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		raw = b
		mu.Unlock()
		io.WriteString(w, okReply)
	})
	if _, err := c.Judge(context.Background(), "", "Tests pass. a < b & c"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(string(raw), "a < b & c") {
		t.Fatalf("HTML escaped in %s", raw)
	}
	var got struct {
		State map[string]any `json:"state"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if _, has := got.State["question"]; has || got.State["answer"] != "Tests pass. a < b & c" {
		t.Fatalf("state %v", got.State)
	}
}

func TestJudgeReadsAnswers(t *testing.T) {
	c := serve(t, reply(200, okReply))
	j, err := c.Judge(context.Background(), "q", "a")
	if err != nil {
		t.Fatal(err)
	}
	if want := (Judgment{0.94, 0.91, "jev-1.14.0"}); j != want {
		t.Fatalf("got %+v", j)
	}
}

func TestJudgeBadReplies(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"401":        {401, `{"detail":"bad key"}`},
		"429":        {429, `{"detail":"slow down"}`},
		"529":        {529, `overloaded`},
		"malformed":  {200, `{"answers":`},
		"missing":    {200, `{"model":"m","answers":{"factual":{"type":"noul","noul":0.9}}}`},
		"above one":  {200, `{"model":"m","answers":{"factual":{"noul":1.2},"listenable":{"noul":0.9}}}`},
		"negative":   {200, `{"model":"m","answers":{"factual":{"noul":0.9},"listenable":{"noul":-0.1}}}`},
		"string":     {200, `{"model":"m","answers":{"factual":{"noul":"high"},"listenable":{"noul":0.9}}}`},
		"no noul":    {200, `{"model":"m","answers":{"factual":{},"listenable":{"noul":0.9}}}`},
		"2 MiB body": {200, paddedReply(2 << 20)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := serve(t, reply(tc.status, tc.body))
			_, err := c.Judge(context.Background(), "q", "a")
			if err == nil {
				t.Fatal("want error")
			}
			if tc.status >= 400 {
				var ae *APIError
				if !errors.As(err, &ae) || ae.Status != tc.status {
					t.Fatalf("want *APIError %d, got %v", tc.status, err)
				}
			}
		})
	}
}

func TestJudgeTimesOut(t *testing.T) {
	done := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	defer s.Close()
	defer close(done)
	c := &Client{Key: "k-123", BaseURL: s.URL, Timeout: 100 * time.Millisecond}
	start := time.Now()
	_, err := c.Judge(context.Background(), "q", "a")
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
}

func TestErrorsNeverHoldTheKey(t *testing.T) {
	const key = "k-secret-123"
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		io.WriteString(w, `{"detail":"bad header `+r.Header.Get("Authorization")+`"}`)
	}))
	defer echo.Close()
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer hang.Close()
	defer close(release)
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	clients := map[string]*Client{
		"echo":    {Key: key, BaseURL: echo.URL},
		"timeout": {Key: key, BaseURL: hang.URL, Timeout: 50 * time.Millisecond},
		"refused": {Key: key, BaseURL: closedURL},
	}
	clients["transport"] = &Client{Key: key, BaseURL: "http://x.invalid",
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("boom " + r.Header.Get("Authorization"))
		})}}
	for name, c := range clients {
		_, err := c.Judge(context.Background(), "q", "a")
		if err == nil {
			t.Fatalf("%s: want error", name)
		}
		if strings.Contains(err.Error(), key) {
			t.Fatalf("%s: key in %q", name, err)
		}
		if err := c.Check(context.Background()); err == nil || strings.Contains(err.Error(), key) {
			t.Fatalf("%s: check %v", name, err)
		}
	}
}

// roundTripFunc is an http.RoundTripper made from a function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAPIErrorNeverEchoesTheRequest(t *testing.T) {
	const secret = "the-private-answer"
	cases := map[string]struct {
		status int
		body   string
		want   string
	}{
		"list":  {422, `{"detail":[{"type":"x","loc":["body","state"],"msg":"field required","input":"` + secret + `","ctx":{"v":"` + secret + `"}}]}`, "body"},
		"raw":   {422, `not json ` + secret, "Unprocessable Entity"},
		"echo":  {400, `{"detail":{"input":"` + secret + `"}}`, "Bad Request"},
		"500":   {500, `oops ` + secret, "Internal Server Error"},
		"empty": {529, ``, "Overloaded"},
	}
	for name, tc := range cases {
		c := serve(t, reply(tc.status, tc.body))
		_, err := c.Judge(context.Background(), "q", "a")
		if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCheck(t *testing.T) {
	var mu sync.Mutex
	var method, path, auth string
	h := func(status int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			method, path, auth = r.Method, r.URL.Path, r.Header.Get("Authorization")
			mu.Unlock()
			w.WriteHeader(status)
			io.WriteString(w, `{}`)
		}
	}
	if err := serve(t, h(200)).Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	ok := method == "GET" && path == "/v1/models" && auth == "Bearer k-123"
	mu.Unlock()
	if !ok {
		t.Fatalf("got %s %s %q", method, path, auth)
	}
	if err := serve(t, h(401)).Check(context.Background()); !errors.Is(err, ErrRejected) {
		t.Fatalf("401: %v", err)
	}
	err := serve(t, h(500)).Check(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 500 {
		t.Fatalf("500: %v", err)
	}
}

func TestAPIErrorDetail(t *testing.T) {
	long := strings.Repeat("я", 300)
	c := serve(t, reply(422, `{"detail":"a\n  b `+long+`"}`))
	_, err := c.Judge(context.Background(), "q", "a")
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatal(err)
	}
	if !strings.HasPrefix(err.Error(), "typesafe: HTTP 422: a b ") || len([]rune(ae.Detail)) > 200 {
		t.Fatalf("got %q (%d runes)", err, len([]rune(ae.Detail)))
	}
}

func TestDecide(t *testing.T) {
	for _, tc := range []struct {
		j    Judgment
		want bool
	}{
		{Judgment{Factual: 0.7, Listenable: 0.7}, true},
		{Judgment{Factual: 0.69, Listenable: 0.99}, false},
		{Judgment{Factual: 0.99, Listenable: 0.69}, false},
		{Judgment{Factual: 0.5, Listenable: 0.5}, false},
	} {
		if got := Decide(tc.j); got != tc.want {
			t.Errorf("%+v: got %v", tc.j, got)
		}
	}
}
