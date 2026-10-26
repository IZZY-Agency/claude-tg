package send

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/send/sendtest"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

func newSender(t *testing.T, rec *sendtest.Recorder) *Sender {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "st"))
	if err != nil {
		t.Fatal(err)
	}
	return &Sender{API: rec, Store: st, ChatID: 123456789, MinGap: 0, Sleep: func(time.Duration) {}}
}

func TestSendComposesHeaderAndChunks(t *testing.T) {
	rec := &sendtest.Recorder{}
	s := newSender(t, rec)
	body := strings.Repeat("word ", 1800) // ~9000 chars
	ids, err := s.Send(context.Background(), Message{SessionID: "sid", Label: "acme-shop-92", Body: body, Markdown: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Calls) != 3 || len(ids) != 3 {
		t.Fatalf("calls=%d ids=%d", len(rec.Calls), len(ids))
	}
	for i, c := range rec.Calls {
		if !strings.HasPrefix(c.Text, "🔔 <b>acme-shop-92</b>") {
			t.Fatalf("call %d lacks header: %q", i, c.Text[:40])
		}
		if want := fmt.Sprintf("<i>(%d/3)</i>\n", i+1); !strings.Contains(c.Text, want) {
			t.Fatalf("call %d lacks part counter %q: %q", i, want, c.Text[:60])
		}
		if c.ParseMode != "HTML" || c.ChatID != 123456789 {
			t.Fatalf("call %d parse=%q chat=%d", i, c.ParseMode, c.ChatID)
		}
		if i > 0 && c.ReplyToMessageID != ids[0] {
			t.Fatalf("call %d reply_to=%d want %d", i, c.ReplyToMessageID, ids[0])
		}
	}
	sess, _ := s.Store.LoadSession("sid")
	if len(sess.LastMessageIDs) != 3 {
		t.Fatalf("LastMessageIDs=%v", sess.LastMessageIDs)
	}
}

func TestSendTitleAndEscaping(t *testing.T) {
	rec := &sendtest.Recorder{}
	s := newSender(t, rec)
	_, err := s.Send(context.Background(), Message{Label: "a<b", Title: "deploy & co", Body: "1 < 2", Markdown: false})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Calls[0].Text; got != "🔔 <b>a&lt;b</b> · <b>deploy &amp; co</b>\n1 &lt; 2" {
		t.Fatalf("text=%q", got)
	}
}

func TestSendRetriesOn429(t *testing.T) {
	rec := &sendtest.Recorder{Script: []error{&telegram.APIError{Code: 429, Description: "Too Many Requests: retry after 1", RetryAfter: 1}}}
	s := newSender(t, rec)
	var slept time.Duration
	s.Sleep = func(d time.Duration) { slept += d }
	ids, err := s.Send(context.Background(), Message{Label: "x", Body: "hi"})
	if err != nil || len(ids) != 1 || len(rec.Calls) != 2 {
		t.Fatalf("ids=%v err=%v calls=%d", ids, err, len(rec.Calls))
	}
	if slept < time.Second {
		t.Fatalf("slept %v, want >= retry_after", slept)
	}
}

func TestSendFallsBackToPlainOn400(t *testing.T) {
	rec := &sendtest.Recorder{Script: []error{&telegram.APIError{Code: 400, Description: "Bad Request: can't parse entities"}}}
	s := newSender(t, rec)
	ids, err := s.Send(context.Background(), Message{Label: "x", Body: "**bold** <tag>", Markdown: true})
	if err != nil || len(ids) != 1 || len(rec.Calls) != 2 {
		t.Fatalf("ids=%v err=%v calls=%d", ids, err, len(rec.Calls))
	}
	second := rec.Calls[1]
	if second.ParseMode != "" || strings.Contains(second.Text, "<b>") || !strings.HasPrefix(second.Text, "🔔 x\n") {
		t.Fatalf("fallback request: parse=%q text=%q", second.ParseMode, second.Text)
	}
}

func TestSendSpoolsOnNetworkError(t *testing.T) {
	rec := &sendtest.Recorder{Script: []error{&telegram.NetError{Err: &net.DNSError{Err: "no such host", Name: "api.telegram.org"}}}}
	s := newSender(t, rec)
	_, err := s.Send(context.Background(), Message{SessionID: "sid", Label: "x", Body: "hi"})
	if !errors.Is(err, ErrSpooled) || Classify(err) != 4 {
		t.Fatalf("err=%v exit=%d", err, Classify(err))
	}
	items, _ := s.Store.SpoolList()
	if len(items) != 1 || items[0].SessionID != "sid" {
		t.Fatalf("spool=%+v", items)
	}
}

func TestFlushSpoolSendsOldestFirst(t *testing.T) {
	rec := &sendtest.Recorder{}
	s := newSender(t, rec)
	old := time.Now().Add(-time.Minute)
	for i, txt := range []string{"b", "a"} {
		_ = s.Store.SpoolPut(state.SpoolItem{ID: txt, Req: telegram.SendRequest{ChatID: 1, Text: txt}, Plain: txt, Created: old.Add(time.Duration(1-i) * time.Second)})
	}
	_ = s.Store.SpoolPut(state.SpoolItem{ID: "c", Req: telegram.SendRequest{ChatID: 1, Text: "c"}, Plain: "c", Created: time.Now()})
	n := s.FlushSpool(context.Background(), 10)
	if n != 3 || len(rec.Calls) != 3 || rec.Calls[0].Text != "a" || rec.Calls[1].Text != "b" || rec.Calls[2].Text != "c" {
		t.Fatalf("n=%d calls=%+v", n, rec.Calls)
	}
	if items, _ := s.Store.SpoolList(); len(items) != 0 {
		t.Fatalf("remaining spool=%+v", items)
	}
}

func TestSilentFlag(t *testing.T) {
	rec := &sendtest.Recorder{}
	s := newSender(t, rec)
	if _, err := s.Send(context.Background(), Message{Label: "x", Body: "hi", Silent: true}); err != nil {
		t.Fatal(err)
	}
	if !rec.Calls[0].Silent {
		t.Fatal("Silent not propagated")
	}
}

func TestClassify(t *testing.T) {
	if Classify(nil) != 0 || Classify(&telegram.APIError{Code: 403}) != 3 || Classify(errors.New("x")) != 4 {
		t.Fatal("classification")
	}
}

func TestKindIcons(t *testing.T) {
	rec := &sendtest.Recorder{}
	s := newSender(t, rec)
	kinds := []struct {
		kind Kind
		icon string
	}{{KindAlert, "🔔"}, {KindReport, "📋"}, {KindFeed, "💬"}, {KindPermission, "🔐"}, {KindInput, "✋"}, {KindFailure, "💥"}, {KindSystem, "⚙️"}}
	for _, k := range kinds {
		if _, err := s.Send(context.Background(), Message{Label: "n", Body: "b", Kind: k.kind}); err != nil {
			t.Fatal(err)
		}
	}
	for i, k := range kinds {
		if want := k.icon + " <b>n</b>\nb"; rec.Calls[i].Text != want {
			t.Fatalf("kind %v: %q want %q", k.kind, rec.Calls[i].Text, want)
		}
	}
}

// The configured token must never appear in a message.
func TestSendRedactsConfiguredSecret(t *testing.T) {
	rec := &sendtest.Recorder{}
	s := newSender(t, rec)
	s.Secrets = []string{"123456:SECRET-TOKEN"}
	_, err := s.Send(context.Background(), Message{Label: "x", Title: "t 123456:SECRET-TOKEN", Body: "curl https://api.telegram.org/bot123456:SECRET-TOKEN/getMe"})
	if err != nil {
		t.Fatal(err)
	}
	if txt := rec.Calls[0].Text; strings.Contains(txt, "SECRET") || !strings.Contains(txt, "[redacted]") {
		t.Fatalf("text=%q", txt)
	}
}

// A label holds a tab name or a title, so it is cleaned as a body is:
// credentials redacted in the header, as in a voice caption.
func TestSendRedactsCredentialInLabel(t *testing.T) {
	rec := &sendtest.Recorder{}
	s := newSender(t, rec)
	if _, err := s.Send(context.Background(), Message{Label: "ops password=hunter2hunter2", Body: "hi"}); err != nil {
		t.Fatal(err)
	}
	if txt := rec.Calls[0].Text; strings.Contains(txt, "hunter2") || !strings.Contains(txt, "[redacted]") {
		t.Fatalf("text=%q", txt)
	}
}

// Exhausted 429s are temporary; spool and exit 4, never drop.
func TestSendSpoolsAfterExhausted429(t *testing.T) {
	e := &telegram.APIError{Code: 429, RetryAfter: 1}
	rec := &sendtest.Recorder{Script: []error{e, e, e}}
	s := newSender(t, rec)
	_, err := s.Send(context.Background(), Message{SessionID: "sid", Label: "x", Body: "hi"})
	if !errors.Is(err, ErrSpooled) || Classify(err) != 4 {
		t.Fatalf("err=%v exit=%d", err, Classify(err))
	}
	if items, _ := s.Store.SpoolList(); len(items) != 1 {
		t.Fatalf("spool=%d", len(items))
	}
}

func TestFlushKeepsItemOn429(t *testing.T) {
	e := &telegram.APIError{Code: 429, RetryAfter: 1}
	rec := &sendtest.Recorder{Script: []error{e, e, e}}
	s := newSender(t, rec)
	_ = s.Store.SpoolPut(state.SpoolItem{ID: "a", Req: telegram.SendRequest{ChatID: 1, Text: "a"}, Plain: "a", Created: time.Now()})
	if n := s.FlushSpool(context.Background(), 10); n != 0 {
		t.Fatalf("flushed %d", n)
	}
	if items, _ := s.Store.SpoolList(); len(items) != 1 {
		t.Fatal("throttled item was dropped")
	}
}

// A new message must not overtake an undelivered older one.
func TestSendQueuesBehindBacklog(t *testing.T) {
	rec := &sendtest.Recorder{Script: []error{&telegram.NetError{Err: errors.New("down")}}}
	s := newSender(t, rec)
	_ = s.Store.SpoolPut(state.SpoolItem{ID: "old", SessionID: "sid", Req: telegram.SendRequest{ChatID: 1, Text: "old"}, Plain: "old", Created: time.Now().Add(-time.Minute)})
	_, err := s.Send(context.Background(), Message{SessionID: "sid", Label: "x", Body: "new"})
	if !errors.Is(err, ErrSpooled) {
		t.Fatalf("err=%v", err)
	}
	items, _ := s.Store.SpoolList()
	if len(items) != 2 || items[0].ID != "old" || !strings.Contains(items[1].Req.Text, "new") {
		t.Fatalf("spool=%+v", items)
	}
	if len(rec.Calls) != 1 || rec.Calls[0].Text != "old" {
		t.Fatalf("calls=%+v; the new message must not be sent ahead of the backlog", rec.Calls)
	}
}

// A failed spool write must not be reported as spooled.
func TestSpoolWriteFailureIsReported(t *testing.T) {
	rec := &sendtest.Recorder{Script: []error{&telegram.NetError{Err: errors.New("down")}}}
	s := newSender(t, rec)
	spool := filepath.Join(s.Store.Dir, "spool")
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits; run as a normal user")
	}
	if err := os.Chmod(spool, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(spool, 0o700) })
	_, err := s.Send(context.Background(), Message{Label: "x", Body: "hi"})
	if err == nil || errors.Is(err, ErrSpooled) || !errors.Is(err, ErrLost) || Classify(err) != 4 {
		t.Fatalf("err=%v exit=%d", err, Classify(err))
	}
}

// Header parts are bounded so the whole message stays under 4096.
func TestHeaderBounded(t *testing.T) {
	rec := &sendtest.Recorder{}
	s := newSender(t, rec)
	label := strings.Repeat("l", 300)
	title := strings.Repeat("t", 700)
	_, err := s.Send(context.Background(), Message{Label: label, Title: title, Body: strings.Repeat("b", 3500)})
	if err != nil {
		t.Fatal(err)
	}
	txt := rec.Calls[0].Text
	if strings.Contains(txt, strings.Repeat("l", 65)) || strings.Contains(txt, strings.Repeat("t", 201)) {
		t.Fatal("label or title not truncated")
	}
	if n := len([]rune(txt)); n > 4096 {
		t.Fatalf("message is %d runes", n)
	}
}

// Replay after a 400 must fall back to the stored plaintext.
func TestFlushUsesPlainOn400(t *testing.T) {
	rec := &sendtest.Recorder{Script: []error{&telegram.APIError{Code: 400, Description: "can't parse entities"}}}
	s := newSender(t, rec)
	_ = s.Store.SpoolPut(state.SpoolItem{ID: "a", Req: telegram.SendRequest{ChatID: 1, Text: "<b>x</b>", ParseMode: "HTML"}, Plain: "x", Created: time.Now()})
	if n := s.FlushSpool(context.Background(), 10); n != 1 {
		t.Fatalf("flushed %d", n)
	}
	if len(rec.Calls) != 2 || rec.Calls[1].ParseMode != "" || rec.Calls[1].Text != "x" {
		t.Fatalf("calls=%+v", rec.Calls)
	}
}

// Retry waits must honour the context.
func TestDeliverStopsWhenContextDone(t *testing.T) {
	e := &telegram.APIError{Code: 429, RetryAfter: 5}
	rec := &sendtest.Recorder{Script: []error{e, e, e}}
	s := newSender(t, rec)
	s.Sleep = nil // real, context-aware waiting
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.Send(ctx, Message{Label: "x", Body: "hi"})
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v elapsed=%v", err, time.Since(start))
	}
}

func TestSendButtonsRecordsMessageID(t *testing.T) {
	rec := &sendtest.Recorder{}
	s := newSender(t, rec)
	kb := [][]telegram.Button{{{Text: "Yes", Data: "a:1"}, {Text: "No", Data: "a:2"}}}
	id, err := s.SendButtons(context.Background(), "sid", "<b>ask</b>", "ask", kb)
	if err != nil || id != 1 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	req := rec.Calls[0]
	if req.ParseMode != "HTML" || req.Text != "<b>ask</b>" || req.Silent || len(req.InlineKeyboard) != 1 || req.InlineKeyboard[0][1].Data != "a:2" {
		t.Fatalf("request: %+v", req)
	}
	sess, err := s.Store.LoadSession("sid")
	if err != nil || len(sess.LastMessageIDs) != 1 || sess.LastMessageIDs[0] != 1 {
		t.Fatalf("session=%+v err=%v", sess, err)
	}
	// It is also known as a prompt's message, after its ask is gone: a
	// reply to it then never reaches another prompt. A plain message is not.
	if !sess.AskMessage(1) {
		t.Fatalf("the button message is not recorded as a prompt's: %+v", sess)
	}
	if _, err := s.Send(context.Background(), Message{SessionID: "sid", Label: "acme", Body: "done"}); err != nil {
		t.Fatal(err)
	}
	sess, _ = s.Store.LoadSession("sid")
	if len(sess.LastMessageIDs) != 2 || sess.AskMessage(sess.LastMessageIDs[1]) {
		t.Fatalf("session=%+v", sess)
	}
}

func TestSendButtonsNeverSpools(t *testing.T) {
	rec := &sendtest.Recorder{Script: []error{&telegram.NetError{Err: &net.DNSError{Err: "no such host", Name: "api.telegram.org"}}}}
	s := newSender(t, rec)
	id, err := s.SendButtons(context.Background(), "sid", "<b>ask</b>", "ask", [][]telegram.Button{{{Text: "Yes", Data: "a:1"}}})
	if err == nil || id != 0 || errors.Is(err, ErrSpooled) {
		t.Fatalf("id=%d err=%v", id, err)
	}
	if items, _ := s.Store.SpoolList(); len(items) != 0 {
		t.Fatalf("spool=%+v", items)
	}
}

func TestSendButtonsPlainFallbackKeepsKeyboard(t *testing.T) {
	rec := &sendtest.Recorder{Script: []error{&telegram.APIError{Code: 400, Description: "Bad Request: can't parse entities"}}}
	s := newSender(t, rec)
	kb := [][]telegram.Button{{{Text: "Yes", Data: "a:1"}}, {{Text: "No", Data: "a:2"}}}
	if _, err := s.SendButtons(context.Background(), "sid", "<b>ask</b>", "ask", kb); err != nil || len(rec.Calls) != 2 {
		t.Fatalf("err=%v calls=%d", err, len(rec.Calls))
	}
	second := rec.Calls[1]
	if second.ParseMode != "" || second.Text != "ask" || fmt.Sprint(second.InlineKeyboard) != fmt.Sprint(kb) {
		t.Fatalf("fallback request: %+v", second)
	}
}

func TestSendButtonsRedactsButtonText(t *testing.T) {
	rec := &sendtest.Recorder{}
	s := newSender(t, rec)
	s.Secrets = []string{"hunter2hunter2"}
	kb := [][]telegram.Button{{{Text: "run token=abcdefgh12345 hunter2hunter2", Data: "token=abcdefgh12345"}}}
	if _, err := s.SendButtons(context.Background(), "sid", "<b>token=abcdefgh12345</b>", "hunter2hunter2 token=abcdefgh12345", kb); err != nil {
		t.Fatal(err)
	}
	req := rec.Calls[0]
	b := req.InlineKeyboard[0][0]
	if strings.Contains(b.Text, "abcdefgh12345") || strings.Contains(b.Text, "hunter2") || strings.Contains(req.Text, "abcdefgh12345") {
		t.Fatalf("not redacted: %+v", req)
	}
	if b.Data != "token=abcdefgh12345" {
		t.Fatalf("data changed: %q", b.Data)
	}
}

// Clean (plain and button texts) and CleanHTML (HTML) are exactly what
// SendButtons applies, so a caller can store and later edit the text that
// was really sent.
func TestCleanMatchesSendButtons(t *testing.T) {
	rec := &sendtest.Recorder{}
	s := newSender(t, rec)
	s.Secrets = []string{"123456:ABCDEFsecretbot"}
	html := "<b>bot123456:ABCDEFsecretbot token=abcdefgh12345</b>"
	if got := s.CleanHTML(html); got != "<b>bot[redacted] token=[redacted]</b>" {
		t.Fatalf("CleanHTML=%q", got)
	}
	plain := "bot123456:ABCDEFsecretbot mysql --password=s3cr3t<dump.sql"
	if got := s.Clean(plain); got != "bot[redacted] mysql --password=[redacted]" {
		t.Fatalf("Clean=%q", got)
	}
	kb := [][]telegram.Button{{{Text: "123456:ABCDEFsecretbot", Data: "a:1"}}}
	if _, err := s.SendButtons(context.Background(), "sid", html, plain, kb); err != nil {
		t.Fatal(err)
	}
	if req := rec.Calls[0]; req.Text != s.CleanHTML(html) || req.InlineKeyboard[0][0].Text != s.Clean("123456:ABCDEFsecretbot") {
		t.Fatalf("request: %+v", req)
	}
}

// When Telegram refuses the HTML, the plain text that goes instead is
// cleaned too.
func TestSendButtonsPlainFallbackIsRedacted(t *testing.T) {
	rec := &sendtest.Recorder{Script: []error{&telegram.APIError{Code: 400, Description: "Bad Request: can't parse entities"}}}
	s := newSender(t, rec)
	s.Secrets = []string{"123456:ABCDEFsecretbot"}
	plain := "curl bot123456:ABCDEFsecretbot -u password=s3cr3t<dump.sql"
	if _, err := s.SendButtons(context.Background(), "sid", "<b>ask</b>", plain, [][]telegram.Button{{{Text: "Yes", Data: "a:1"}}}); err != nil || len(rec.Calls) != 2 {
		t.Fatalf("err=%v calls=%d", err, len(rec.Calls))
	}
	if got := rec.Calls[1]; got.ParseMode != "" || got.Text != "curl bot[redacted] -u password=[redacted]" {
		t.Fatalf("fallback request: %+v", got)
	}
}

// Buttons delivered hours late would mislead: throttling that outlasts the
// retries is an error for the caller, never a spooled message.
func TestSendButtonsExhausted429NeverSpools(t *testing.T) {
	e := &telegram.APIError{Code: 429, RetryAfter: 1}
	rec := &sendtest.Recorder{Script: []error{e, e, e}}
	s := newSender(t, rec)
	id, err := s.SendButtons(context.Background(), "sid", "<b>ask</b>", "ask", [][]telegram.Button{{{Text: "Yes", Data: "a:1"}}})
	if err == nil || id != 0 || errors.Is(err, ErrSpooled) || len(rec.Calls) != 3 {
		t.Fatalf("id=%d err=%v calls=%d", id, err, len(rec.Calls))
	}
	if items, _ := s.Store.SpoolList(); len(items) != 0 {
		t.Fatalf("spool=%+v", items)
	}
	if sess, _ := s.Store.LoadSession("sid"); len(sess.LastMessageIDs) != 0 {
		t.Fatalf("an unsent message was recorded: %v", sess.LastMessageIDs)
	}
}

func TestSenderSendVoice(t *testing.T) {
	for _, tc := range []struct {
		name, label string
		d           time.Duration
		caption     string
		duration    int
	}{
		{"rounds up", "name", 6200 * time.Millisecond, "🔊 name", 7},
		{"at least one second", "name", 300 * time.Millisecond, "🔊 name", 1},
		{"whole seconds stay", "name", 5 * time.Second, "🔊 name", 5},
		{"empty label", "", time.Second, "🔊 mac", 1},
		{"secret scrubbed", "x 123456:SECRET-TOKEN", time.Second, "🔊 x [redacted]", 1},
		{"label clipped", strings.Repeat("a", 100), time.Second, "🔊 " + strings.Repeat("a", maxLabel-1) + "…", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &sendtest.Recorder{}
			s := newSender(t, rec)
			s.Secrets = []string{"123456:SECRET-TOKEN"}
			id, err := s.SendVoice(context.Background(), "sid", tc.label, []byte("ogg"), tc.d, true)
			if err != nil || id == 0 {
				t.Fatalf("id=%d err=%v", id, err)
			}
			if len(rec.Voices) != 1 {
				t.Fatalf("voices=%d", len(rec.Voices))
			}
			v := rec.Voices[0]
			if v.Caption != tc.caption || v.Duration != tc.duration || v.ChatID != 123456789 || string(v.Audio) != "ogg" || !v.Silent {
				t.Fatalf("voice=%+v", v)
			}
			sess, _ := s.Store.LoadSession("sid")
			if len(sess.LastMessageIDs) != 1 || sess.LastMessageIDs[0] != id {
				t.Fatalf("LastMessageIDs=%v want %d", sess.LastMessageIDs, id)
			}
		})
	}
}

// A duration that is not positive or that no voice message has (beyond
// maxVoiceDuration) is refused before any request; near the cap the
// seconds still round up rather than overflow.
func TestSenderSendVoiceDuration(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    time.Duration
		secs int // 0: refused
	}{
		{"zero", 0, 0},
		{"negative", -time.Second, 0},
		{"near the largest duration", time.Duration(math.MaxInt64), 0},
		{"just over the cap", maxVoiceDuration + time.Nanosecond, 0},
		{"the cap", maxVoiceDuration, 86400},
		{"just under the cap", maxVoiceDuration - time.Nanosecond, 86400},
		{"one nanosecond", time.Nanosecond, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &sendtest.Recorder{}
			s := newSender(t, rec)
			_, err := s.SendVoice(context.Background(), "sid", "x", []byte("ogg"), tc.d, false)
			if tc.secs == 0 {
				if err == nil || !strings.HasPrefix(err.Error(), "unsupported voice duration") || len(rec.Voices) != 0 {
					t.Fatalf("err=%v voices=%+v, want refused before any request", err, rec.Voices)
				}
				return
			}
			if err != nil || len(rec.Voices) != 1 || rec.Voices[0].Duration != tc.secs {
				t.Fatalf("err=%v voices=%+v, want %d s", err, rec.Voices, tc.secs)
			}
		})
	}
}

// A voice message is never spooled: heard hours late it would mislead.
func TestSenderSendVoiceNotSpooled(t *testing.T) {
	rec := &sendtest.Recorder{Script: []error{&telegram.NetError{Err: errors.New("dial tcp: refused")}}}
	s := newSender(t, rec)
	if _, err := s.SendVoice(context.Background(), "sid", "x", []byte("ogg"), time.Second, false); !telegram.IsNetwork(err) {
		t.Fatalf("err=%v", err)
	}
	if items, _ := s.Store.SpoolList(); len(items) != 0 {
		t.Fatalf("spooled %d items", len(items))
	}
	if sess, _ := s.Store.LoadSession("sid"); len(sess.LastMessageIDs) != 0 {
		t.Fatalf("recorded ids after a failure: %v", sess.LastMessageIDs)
	}
}

func TestSenderSendVoice429(t *testing.T) {
	rec := &sendtest.Recorder{Script: []error{&telegram.APIError{Code: 429, Description: "Too Many Requests: retry after 1", RetryAfter: 1}, nil}}
	s := newSender(t, rec)
	var slept time.Duration
	s.Sleep = func(d time.Duration) { slept += d }
	if _, err := s.SendVoice(context.Background(), "sid", "x", []byte("ogg"), time.Second, false); err != nil {
		t.Fatal(err)
	}
	if len(rec.Voices) != 2 || slept < time.Second {
		t.Fatalf("voices=%d slept=%v", len(rec.Voices), slept)
	}
}

// A 400 is final: there is no plain-text fallback for audio.
func TestSenderSendVoiceRefused(t *testing.T) {
	rec := &sendtest.Recorder{Script: []error{&telegram.APIError{Code: 400, Description: "Bad Request"}}}
	s := newSender(t, rec)
	_, err := s.SendVoice(context.Background(), "sid", "x", []byte("ogg"), time.Second, false)
	var api *telegram.APIError
	if !errors.As(err, &api) || len(rec.Voices) != 1 {
		t.Fatalf("err=%v voices=%d", err, len(rec.Voices))
	}
}

// textOnly implements API but not VoiceAPI.
type textOnly struct{}

func (textOnly) SendMessage(context.Context, telegram.SendRequest) (int64, error) { return 1, nil }

func TestSenderSendVoiceNoVoiceAPI(t *testing.T) {
	s := newSender(t, &sendtest.Recorder{})
	s.API = textOnly{}
	_, err := s.SendVoice(context.Background(), "sid", "x", []byte("ogg"), time.Second, false)
	if err == nil || err.Error() != "telegram client cannot send voice" {
		t.Fatalf("err=%v", err)
	}
}
