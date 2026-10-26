package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
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

func TestGetFile(t *testing.T) {
	var path, raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		raw = strings.TrimSpace(string(b))
		_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"f1","file_size":1234,"file_path":"voice/file_7.oga"}}`))
	}))
	t.Cleanup(srv.Close)
	c := &Client{Token: "T", HTTP: srv.Client(), BaseURL: srv.URL}
	f, err := c.GetFile(context.Background(), "f1")
	if err != nil {
		t.Fatal(err)
	}
	if f != (File{FileID: "f1", FileSize: 1234, FilePath: "voice/file_7.oga"}) {
		t.Fatalf("file = %+v", f)
	}
	if !strings.HasSuffix(path, "/getFile") || raw != `{"file_id":"f1"}` {
		t.Fatalf("path=%q body=%q", path, raw)
	}
}

func TestDownload(t *testing.T) {
	const token = "123456:SECRET-TOKEN"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/file/bot"+token+"/voice/file_7.oga" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("hello"))
	}))
	t.Cleanup(srv.Close)
	c := &Client{Token: token, HTTP: srv.Client(), BaseURL: srv.URL}
	b, err := c.Download(context.Background(), "voice/file_7.oga", 20)
	if err != nil || string(b) != "hello" {
		t.Fatalf("b=%q err=%v", b, err)
	}
	if _, err := c.Download(context.Background(), "voice/file_7.oga", 4); err == nil || !strings.Contains(err.Error(), "file too big") {
		t.Fatalf("err = %v", err)
	}
}

func TestDownloadErrorHidesToken(t *testing.T) {
	const token = "123456:SECRET-TOKEN"
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	c := &Client{Token: token, HTTP: srv.Client(), BaseURL: srv.URL}
	_, err := c.Download(context.Background(), "voice/x.oga", 10)
	if err == nil || !strings.Contains(err.Error(), "http 404") || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("404 err = %v", err)
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	c = &Client{Token: token, HTTP: dead.Client(), BaseURL: dead.URL}
	_, err = c.Download(context.Background(), "voice/x.oga", 10)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("transport err = %v", err)
	}
}

func TestSetMessageReactionClears(t *testing.T) {
	for emoji, want := range map[string]string{"": `"reaction":[]`, "👀": `"reaction":[{"emoji":"👀","type":"emoji"}]`} {
		var raw string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			raw = string(b)
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		}))
		c := &Client{Token: "T", HTTP: srv.Client(), BaseURL: srv.URL}
		err := c.SetMessageReaction(context.Background(), 1, 2, emoji)
		srv.Close()
		if err != nil || !strings.Contains(raw, want) {
			t.Fatalf("emoji %q: err=%v body=%s want %s", emoji, err, raw, want)
		}
	}
}

func TestMessageDecodesVoiceAndMedia(t *testing.T) {
	var v, s, p Message
	for raw, m := range map[string]*Message{
		`{"message_id":5,"voice":{"file_id":"f","duration":7,"mime_type":"audio/ogg","file_size":9}}`: &v,
		`{"message_id":6,"sticker":{"file_id":"s"}}`:                                                  &s,
		`{"message_id":7,"text":"hi"}`:                                                                &p,
	} {
		if err := json.Unmarshal([]byte(raw), m); err != nil {
			t.Fatal(err)
		}
	}
	if v.Voice == nil || v.Voice.FileID != "f" || v.Voice.Duration != 7 || v.Voice.MimeType != "audio/ogg" || v.Voice.FileSize != 9 {
		t.Fatalf("voice = %+v", v.Voice)
	}
	if v.HasMedia() || !s.HasMedia() || p.HasMedia() {
		t.Fatalf("HasMedia voice=%v sticker=%v text=%v", v.HasMedia(), s.HasMedia(), p.HasMedia())
	}
}

// Every kind of content tg cannot read counts as media, a location or a
// poll as much as a photo; a service message (a pin, a member joining)
// carries no content and does not.
func TestHasMediaKinds(t *testing.T) {
	for raw, want := range map[string]bool{
		`{"message_id":1,"photo":[{"file_id":"p"}]}`:                                         true,
		`{"message_id":2,"audio":{"file_id":"a"}}`:                                           true,
		`{"message_id":3,"video":{"file_id":"v"}}`:                                           true,
		`{"message_id":4,"video_note":{"file_id":"n"}}`:                                      true,
		`{"message_id":5,"document":{"file_id":"d"}}`:                                        true,
		`{"message_id":6,"animation":{"file_id":"g"}}`:                                       true,
		`{"message_id":7,"location":{"latitude":48.85,"longitude":2.35}}`:                    true,
		`{"message_id":8,"contact":{"phone_number":"+10000000000","first_name":"Alex"}}`:     true,
		`{"message_id":9,"poll":{"id":"1","question":"lunch?","options":[]}}`:                true,
		`{"message_id":10,"dice":{"emoji":"🎲","value":3}}`:                                   true,
		`{"message_id":11,"venue":{"location":{"latitude":1,"longitude":2},"title":"cafe"}}`: true,
		`{"message_id":12,"pinned_message":{"message_id":3}}`:                                false,
		`{"message_id":13,"new_chat_members":[{"id":5,"first_name":"Sam"}]}`:                 false,
	} {
		var m Message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		if m.HasMedia() != want {
			t.Errorf("HasMedia(%s) = %v, want %v", raw, m.HasMedia(), want)
		}
	}
}

// The size cap must not overflow at the extremes.
func TestDownloadLimitExtremes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("hello")) }))
	t.Cleanup(srv.Close)
	c := &Client{Token: "T", HTTP: srv.Client(), BaseURL: srv.URL}
	if b, err := c.Download(context.Background(), "f", math.MaxInt64); err != nil || string(b) != "hello" {
		t.Fatalf("max int64: b=%q err=%v", b, err)
	}
	for _, max := range []int64{0, -1} {
		if _, err := c.Download(context.Background(), "f", max); err == nil || !strings.Contains(err.Error(), "file too big") {
			t.Fatalf("max %d: err=%v", max, err)
		}
	}
}

// voiceServer serves one sendVoice call and hands the parsed form to check.
func voiceServer(t *testing.T, check func(r *http.Request)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/bot123456:SECRET-TOKEN/sendVoice" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse form: %v", err)
		}
		check(r)
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
	}))
	t.Cleanup(srv.Close)
	return &Client{Token: "123456:SECRET-TOKEN", HTTP: srv.Client(), BaseURL: srv.URL}
}

func TestSendVoiceMultipart(t *testing.T) {
	audio := []byte("OggS\x00fake-opus-bytes")
	c := voiceServer(t, func(r *http.Request) {
		f := r.MultipartForm
		for k, want := range map[string]string{"chat_id": "123456789", "duration": "7", "caption": "🔊 tgvoice-73", "disable_notification": "true"} {
			if got := f.Value[k]; len(got) != 1 || got[0] != want {
				t.Errorf("%s = %v, want %q", k, got, want)
			}
		}
		files := f.File["voice"]
		if len(files) != 1 {
			t.Fatalf("voice parts: %d", len(files))
		}
		fh := files[0]
		if fh.Filename != "voice.ogg" || fh.Header.Get("Content-Type") != "audio/ogg" {
			t.Errorf("file %q type %q", fh.Filename, fh.Header.Get("Content-Type"))
		}
		file, err := fh.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		got, _ := io.ReadAll(file)
		if string(got) != string(audio) {
			t.Errorf("audio bytes differ: %q", got)
		}
	})
	id, err := c.SendVoice(context.Background(), VoiceRequest{ChatID: 123456789, Audio: audio, Duration: 7, Caption: "🔊 tgvoice-73", Silent: true})
	if err != nil || id != 42 {
		t.Fatalf("id=%d err=%v", id, err)
	}
}

func TestSendVoiceOmitsEmptyFields(t *testing.T) {
	c := voiceServer(t, func(r *http.Request) {
		for _, k := range []string{"duration", "caption", "disable_notification"} {
			if v, has := r.MultipartForm.Value[k]; has {
				t.Errorf("%s should be omitted, got %v", k, v)
			}
		}
		if got := r.MultipartForm.Value["chat_id"]; len(got) != 1 || got[0] != "5" {
			t.Errorf("chat_id = %v", got)
		}
	})
	if _, err := c.SendVoice(context.Background(), VoiceRequest{ChatID: 5, Audio: []byte("x")}); err != nil {
		t.Fatal(err)
	}
}

func TestSendVoiceErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		net    bool
	}{
		{"502", http.StatusBadGateway, `<html>bad gateway</html>`, true},
		{"400", http.StatusOK, `{"ok":false,"error_code":400,"description":"Bad Request: wrong file"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c := &Client{Token: "123456:SECRET-TOKEN", HTTP: srv.Client(), BaseURL: srv.URL}
			_, err := c.SendVoice(context.Background(), VoiceRequest{ChatID: 1, Audio: []byte("x")})
			if err == nil {
				t.Fatal("want error")
			}
			var api *APIError
			if tc.net != IsNetwork(err) || tc.net == errors.As(err, &api) {
				t.Fatalf("misclassified: %#v", err)
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("error leaks the token: %v", err)
			}
		})
	}
}

// A refused connection wraps the request URL, which carries the token.
func TestSendVoiceTransportErrorHidesToken(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := &Client{Token: "123456:SECRET-TOKEN", BaseURL: url}
	_, err := c.SendVoice(context.Background(), VoiceRequest{ChatID: 1, Audio: []byte("x")})
	if !IsNetwork(err) || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("err=%v", err)
	}
}
