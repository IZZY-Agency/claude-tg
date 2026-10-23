package telegram

import (
	"errors"
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
