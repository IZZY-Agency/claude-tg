package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Response bodies below are the documented shapes from core.telegram.org/bots/api.

func TestParseOK(t *testing.T) {
	var msg struct {
		MessageID int64 `json:"message_id"`
	}
	err := ParseResponse([]byte(`{"ok":true,"result":{"message_id":42,"date":1700000000,"chat":{"id":123456789,"type":"private"},"text":"hi"}}`), &msg)
	if err != nil || msg.MessageID != 42 {
		t.Fatalf("err=%v id=%d", err, msg.MessageID)
	}
}

func TestParse429(t *testing.T) {
	err := ParseResponse([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 7","parameters":{"retry_after":7}}`), nil)
	var api *APIError
	if !errors.As(err, &api) || api.Code != 429 || api.RetryAfter != 7 {
		t.Fatalf("got %#v", err)
	}
}

func TestParse400(t *testing.T) {
	err := ParseResponse([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities: Unsupported start tag \"x\" at byte offset 3"}`), nil)
	var api *APIError
	if !errors.As(err, &api) || api.Code != 400 || !strings.Contains(api.Description, "can't parse entities") {
		t.Fatalf("got %#v", err)
	}
	if IsNetwork(err) {
		t.Fatal("an API error must not be classified as a network error")
	}
}

func TestParseGarbageIsError(t *testing.T) {
	if err := ParseResponse([]byte(`<html>502</html>`), nil); err == nil {
		t.Fatal("want error for non-JSON body")
	}
}

func TestURLNeverLogsToken(t *testing.T) {
	c := New("123456:SECRET-TOKEN")
	u := c.methodURL("sendMessage")
	if !strings.HasSuffix(u, "/bot123456:SECRET-TOKEN/sendMessage") {
		t.Fatalf("url=%q", u)
	}
	api := &APIError{Code: 401, Description: "Unauthorized"}
	if strings.Contains(api.Error(), "SECRET") {
		t.Fatal("error text leaks the token")
	}
	ne := &NetError{Err: errors.New("dial tcp: lookup api.telegram.org: no such host")}
	if !IsNetwork(ne) || strings.Contains(ne.Error(), "SECRET") {
		t.Fatalf("NetError misclassified or leaks: %v", ne)
	}
}

// Transport errors wrap the request URL, which carries the token.
func TestTransportErrorHidesToken(t *testing.T) {
	raw := &url.Error{Op: "Post", URL: "https://api.telegram.org/bot123456:SECRET-TOKEN/sendMessage", Err: errors.New("dial tcp: lookup api.telegram.org: no such host")}
	err := wrapTransport(raw)
	if !IsNetwork(err) {
		t.Fatal("must stay a network error")
	}
	if strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "no such host") {
		t.Fatalf("sanitized error = %q", err.Error())
	}
}

// v2: updates carry replies, dates and callback queries.
func TestParseUpdateWithReplyAndCallback(t *testing.T) {
	var ups []Update
	body := `{"ok":true,"result":[
	 {"update_id":10,"message":{"message_id":55,"date":1761148800,"chat":{"id":123456789,"type":"private"},"from":{"id":123456789,"first_name":"Alex"},"text":"ship it","reply_to_message":{"message_id":42,"date":1761148700,"chat":{"id":123456789,"type":"private"},"text":"💬 acme-shop-92"}}},
	 {"update_id":11,"callback_query":{"id":"4382bfdfd30e","from":{"id":123456789,"first_name":"Alex"},"message":{"message_id":56,"date":1761148810,"chat":{"id":123456789,"type":"private"},"text":"which session?"},"data":"s:6449d606:k1"}}
	]}`
	if err := ParseResponse([]byte(body), &ups); err != nil {
		t.Fatal(err)
	}
	if len(ups) != 2 || ups[0].Message.ReplyToMessage == nil || ups[0].Message.ReplyToMessage.MessageID != 42 || ups[0].Message.Date != 1761148800 {
		t.Fatalf("message update: %+v", ups[0])
	}
	cq := ups[1].CallbackQuery
	if cq == nil || cq.ID != "4382bfdfd30e" || cq.Data != "s:6449d606:k1" || cq.From.ID != 123456789 || cq.Message.MessageID != 56 {
		t.Fatalf("callback update: %+v", ups[1])
	}
}

func TestSendParamsInlineKeyboard(t *testing.T) {
	p := SendParams(SendRequest{ChatID: 1, Text: "which?", InlineKeyboard: [][]Button{{{Text: "acme-shop-92 · busy", Data: "s:abc"}}}})
	rm, ok := p["reply_markup"].(map[string]any)
	if !ok {
		t.Fatalf("no reply_markup: %+v", p)
	}
	rows := rm["inline_keyboard"].([][]map[string]any)
	if rows[0][0]["callback_data"] != "s:abc" || rows[0][0]["text"] != "acme-shop-92 · busy" {
		t.Fatalf("keyboard: %+v", rows)
	}
	if _, has := SendParams(SendRequest{ChatID: 1, Text: "x"})["reply_markup"]; has {
		t.Fatal("reply_markup must be absent without a keyboard")
	}
}

// editServer records the method path and JSON body of the one call it receives.
func editServer(t *testing.T) (*Client, *string, *map[string]any) {
	t.Helper()
	var path string
	body := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &body); err != nil {
			t.Errorf("body: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	t.Cleanup(srv.Close)
	return &Client{Token: "T", HTTP: srv.Client(), BaseURL: srv.URL}, &path, &body
}

func TestEditMessageSendsKeyboard(t *testing.T) {
	c, path, body := editServer(t)
	kb := [][]Button{{{Text: "Yes", Data: "a:x"}}}
	if err := c.EditMessage(context.Background(), 1, 7, "hi", "HTML", kb); err != nil {
		t.Fatal(err)
	}
	if *path != "/botT/editMessageText" {
		t.Fatalf("path=%q", *path)
	}
	rm := (*body)["reply_markup"].(map[string]any)
	btn := rm["inline_keyboard"].([]any)[0].([]any)[0].(map[string]any)
	if btn["callback_data"] != "a:x" || btn["text"] != "Yes" {
		t.Fatalf("button: %+v", btn)
	}
	if (*body)["parse_mode"] != "HTML" || (*body)["text"] != "hi" {
		t.Fatalf("body: %+v", *body)
	}
}

func TestEditMessageWithoutKeyboardOmitsMarkup(t *testing.T) {
	c, _, body := editServer(t)
	if err := c.EditMessage(context.Background(), 1, 7, "hi", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, has := (*body)["reply_markup"]; has {
		t.Fatalf("reply_markup must be absent: %+v", *body)
	}
}

func TestEditReplyMarkupEmptyClearsKeyboard(t *testing.T) {
	c, path, body := editServer(t)
	if err := c.EditReplyMarkup(context.Background(), 1, 7, nil); err != nil {
		t.Fatal(err)
	}
	if *path != "/botT/editMessageReplyMarkup" {
		t.Fatalf("path=%q", *path)
	}
	got, _ := json.Marshal((*body)["reply_markup"])
	if string(got) != `{"inline_keyboard":[]}` {
		t.Fatalf("reply_markup=%s", got)
	}
}

func TestEditReplyMarkupSendsKeyboard(t *testing.T) {
	c, path, body := editServer(t)
	kb := [][]Button{{{Text: "✓ Red", Data: "a:x:1:o0"}}, {{Text: "Done", Data: "a:x:1:d"}, {Text: "✍️ Other", Data: "a:x:1:x"}}}
	if err := c.EditReplyMarkup(context.Background(), 1, 7, kb); err != nil {
		t.Fatal(err)
	}
	if *path != "/botT/editMessageReplyMarkup" {
		t.Fatalf("path=%q", *path)
	}
	got, _ := json.Marshal((*body)["reply_markup"])
	want := `{"inline_keyboard":[[{"callback_data":"a:x:1:o0","text":"✓ Red"}],[{"callback_data":"a:x:1:d","text":"Done"},{"callback_data":"a:x:1:x","text":"✍️ Other"}]]}`
	if string(got) != want {
		t.Fatalf("reply_markup=%s\nwant %s", got, want)
	}
	if (*body)["chat_id"] != float64(1) || (*body)["message_id"] != float64(7) {
		t.Fatalf("body: %+v", *body)
	}
	if _, has := (*body)["text"]; has {
		t.Fatalf("a keyboard edit must not send text: %+v", *body)
	}
}
