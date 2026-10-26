package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/classify"
	"github.com/IZZY-Agency/claude-tg/internal/send"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
	"github.com/IZZY-Agency/claude-tg/internal/transcript"
)

// weatherPrompt is a question typed on the phone, as the relay types it
// (with a leading space); weatherAnswer is the short factual answer to it.
const (
	weatherPrompt = " What is the weather in Paris?"
	weatherAnswer = "It is 14 degrees and cloudy in Paris today, with light rain this evening."
)

// parisAnswer has the shape of a real weather answer, too long to speak
// whole: a lead sentence (parisLead), a blank line, then bulleted figures.
// marseilleAnswer adds a trailing comparison table.
const (
	parisLead    = "It's raining in Paris this morning, and the rain should stop around midday, leaving the rest of the day cloudy."
	parisFigures = "- **Temperature:** 14 °C now, rising to 17 °C this afternoon and falling to 11 °C overnight.\n" +
		"- **Rain:** about 4 mm before noon, then dry; the chance of a shower after three is under ten percent.\n" +
		"- **Wind:** from the south-west at 15 km/h, gusting to 30 km/h near the river this morning.\n" +
		"- **Humidity:** 88 percent this morning, dropping to 65 percent by the evening as the clouds thin.\n" +
		"- **Sunrise and sunset:** 8:21 and 18:37; the sun may break through briefly around five.\n" +
		"- **Tomorrow:** dry and brighter, with sunny spells, a high of 19 °C and a light breeze from the west."
	parisAnswer = parisLead + "\n\n" + parisFigures

	marseilleLead   = "It is sunny and warm in Marseille today, with a light sea breeze and no rain until the weekend."
	marseilleAnswer = marseilleLead + "\n\n" + parisFigures + "\n\n" +
		"| | Marseille | Paris |\n|---|---|---|\n| High | 24 °C | 17 °C |\n| Rain | none | 4 mm |"
)

// judgeCall is one Judge call as the fake classifier saw it: its arguments
// and how far away its context's deadline was (0: no deadline).
type judgeCall struct {
	question, answer string
	left             time.Duration
}

// fakeClassifier stands in for TypeSafe's Jev (classify.Client): it records
// each call and answers j or err after delay. With block set it waits for
// its context to end instead.
type fakeClassifier struct {
	calls []judgeCall
	j     classify.Judgment
	err   error
	block bool
	delay time.Duration
}

func (f *fakeClassifier) Judge(ctx context.Context, question, answer string) (classify.Judgment, error) {
	c := judgeCall{question: question, answer: answer}
	if dl, ok := ctx.Deadline(); ok {
		c.left = time.Until(dl)
	}
	f.calls = append(f.calls, c)
	if f.block {
		<-ctx.Done()
		return classify.Judgment{}, ctx.Err()
	}
	select {
	case <-ctx.Done():
		return classify.Judgment{}, ctx.Err()
	case <-time.After(f.delay):
	}
	return f.j, f.err
}

// voiceHarness is the spoken-answer harness with a fake classifier that
// finds every answer short and factual, and a transcript whose prompt
// before the answer is prompt (Deps.ReadPromptBefore), its reads counted;
// with transcript set, the real file is read instead. The Stop hook never
// reads the transcript's last prompt (Deps.ReadPrompt).
type voiceHarness struct {
	*speakHarness
	cl     *fakeClassifier
	prompt transcript.Prompt
	reads  int
	sleeps []time.Duration // Deps.Sleep's calls, none of which waits
}

func newVoiceHarness(t *testing.T) *voiceHarness {
	t.Helper()
	h := &voiceHarness{speakHarness: newSpeakHarness(t), cl: &fakeClassifier{j: classify.Judgment{Factual: 0.94, Listenable: 0.91}}}
	h.prompt = transcript.Prompt{Text: weatherPrompt, At: h.now.Add(-time.Minute)}
	h.deps.Classifier = h.cl
	h.deps.ReadPromptBefore = func(path, answer string) (transcript.Prompt, bool, error) {
		h.reads++
		if h.transcript != "" {
			return transcript.PromptBefore(path, answer)
		}
		return h.prompt, true, nil
	}
	h.deps.ReadPrompt = func(string) (transcript.Prompt, bool, error) {
		t.Error("the Stop hook read the transcript's last prompt, not its answer's")
		return transcript.Prompt{}, false, nil
	}
	h.deps.Sleep = func(ctx context.Context, d time.Duration) bool {
		h.sleeps = append(h.sleeps, d)
		return ctx.Err() == nil
	}
	return h
}

// The entries of a test transcript, as Claude Code writes them: a prompt,
// a follow-up the phone queued to the busy session, an answer's text.
func userLine(text string, at time.Time) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":%s}}`+"\n", at.Format(time.RFC3339Nano), jsonText(text))
}

func queuedLine(text string, at time.Time) string {
	return fmt.Sprintf(`{"type":"attachment","timestamp":%q,"attachment":{"type":"queued_command","commandMode":"prompt","origin":{"kind":"human"},"prompt":%s}}`+"\n", at.Format(time.RFC3339Nano), jsonText(text))
}

func assistantLine(text string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":%s}]}}`+"\n", jsonText(text))
}

func jsonText(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// writeTranscript writes a test transcript and returns its path.
func writeTranscript(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// phoneLine records the transcript's prompt as a line the relay submitted
// from the phone when Claude Code wrote it.
func (h *voiceHarness) phoneLine(t *testing.T) {
	t.Helper()
	if _, err := h.store.UpdateSession(sid, func(s *state.Session) { s.RecordPhoneLine(h.prompt.Text, h.prompt.At, false) }); err != nil {
		t.Fatal(err)
	}
}

// voiceLog is the voice check's log line for the harness's session.
func voiceLog(decision string) string { return "voice check (acme-shop-92): " + decision }

// voiceLogs are the voice check lines the hook logged.
func (h *voiceHarness) voiceLogs() []string {
	var out []string
	for _, l := range h.logs {
		if strings.HasPrefix(l, "voice check") {
			out = append(out, l)
		}
	}
	return out
}

// spokenThenText checks that the answer went out as one voice message
// followed by its text.
func (h *voiceHarness) spokenThenText(t *testing.T) {
	t.Helper()
	if !slices.Equal(h.sn.order, []string{"voice", "text"}) || len(h.rec.Voices) != 1 || len(h.sp.texts) != 1 {
		t.Fatalf("order=%v voices=%d speaks=%d, want one voice then the text", h.sn.order, len(h.rec.Voices), len(h.sp.texts))
	}
}

// A question typed on the phone gets its short factual answer as a voice
// message, then as text: Jev was asked about the cleaned prompt and answer
// and found it short, factual and listenable.
func TestStopTypedPhoneAnswerSpoken(t *testing.T) {
	h := newVoiceHarness(t)
	h.owe(t, time.Time{})
	h.phoneLine(t)
	if err := h.stop(t, weatherAnswer); err != nil {
		t.Fatal(err)
	}
	h.spokenThenText(t)
	if len(h.cl.calls) != 1 || h.cl.calls[0].question != "What is the weather in Paris?" || h.cl.calls[0].answer != weatherAnswer {
		t.Fatalf("Judge calls=%+v, want one with the phone's question and the answer", h.cl.calls)
	}
	if want := []string{voiceLog("voice (factual 0.94, listenable 0.91)")}; !slices.Equal(h.voiceLogs(), want) {
		t.Fatalf("voice check log=%q, want %q", h.voiceLogs(), want)
	}
	if s := h.session(t); len(s.PhoneLines) != 1 || !s.PhoneLines[0].Answered || s.PendingAnswers != 0 {
		t.Fatalf("phone line or answer not spent: %+v", s)
	}
}

// An answer Jev does not find factual is text only.
func TestStopTypedPhoneAnswerText(t *testing.T) {
	h := newVoiceHarness(t)
	h.cl.j = classify.Judgment{Factual: 0.12, Listenable: 0.91}
	h.owe(t, time.Time{})
	h.phoneLine(t)
	if err := h.stop(t, weatherAnswer); err != nil {
		t.Fatal(err)
	}
	h.textOnly(t)
	if len(h.cl.calls) != 1 || len(h.sp.texts) != 0 {
		t.Fatalf("Judge calls=%d speaks=%d, want one and none", len(h.cl.calls), len(h.sp.texts))
	}
	if want := []string{voiceLog("text (factual 0.12, listenable 0.91)")}; !slices.Equal(h.voiceLogs(), want) {
		t.Fatalf("voice check log=%q, want %q", h.voiceLogs(), want)
	}
}

// Code is always text: an answer holding a fenced block, however short
// and wherever the block is, or one that is only a table, is never sent to
// TypeSafe.
func TestStopCodeNeverAsksJev(t *testing.T) {
	for _, tc := range []struct{ name, answer string }{
		{"code block", "The deploy finished and the shop is live again on the new server.\n\n```sh\nmake deploy\n```"},
		{"code block after a long answer", parisAnswer + "\n\n```sh\ncurl wttr.in/Paris\n```"},
		{"only a table", "| run | ok |\n|---|---|\n| unit | yes |"},
		{"only a table without outer pipes", "City | High\n--- | ---\nParis | 17 °C"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newVoiceHarness(t)
			h.owe(t, time.Time{})
			h.phoneLine(t)
			if err := h.stop(t, tc.answer); err != nil {
				t.Fatal(err)
			}
			h.textOnly(t)
			if len(h.cl.calls) != 0 || len(h.sp.texts) != 0 {
				t.Fatalf("Judge calls=%d speaks=%d, want none", len(h.cl.calls), len(h.sp.texts))
			}
			if want := []string{voiceLog("text (code block or table)")}; !slices.Equal(h.voiceLogs(), want) {
				t.Fatalf("voice check log=%q, want %q", h.voiceLogs(), want)
			}
		})
	}
}

// The code rule decides before any wait: with no time left for speech,
// a code answer still logs its own reason, not the lack of time.
func TestStopCodeRuleBeforeTimeCheck(t *testing.T) {
	h := newVoiceHarness(t)
	h.deps.TextReserve = time.Second
	h.owe(t, time.Time{})
	h.phoneLine(t)
	ctx, cancel := context.WithTimeout(context.Background(), h.deps.TextReserve/2)
	defer cancel()
	if err := h.stopCtx(t, ctx, "The deploy finished and the shop is live again on the new server.\n\n```sh\nmake deploy\n```"); err != nil {
		t.Fatal(err)
	}
	h.textOnly(t)
	if want := []string{voiceLog("text (code block or table)")}; !slices.Equal(h.voiceLogs(), want) || h.logged("speech: no time left before the text") {
		t.Fatalf("log=%q, want only %q", h.logs, want)
	}
}

// A long answer is text only, and never sent to TypeSafe.
func TestStopLongAnswerNeverAsksJev(t *testing.T) {
	h := newVoiceHarness(t)
	h.owe(t, time.Time{})
	h.phoneLine(t)
	if err := h.stop(t, strings.Repeat("Готово. ", 75)); err != nil { // 600 runes of prose
		t.Fatal(err)
	}
	h.textOnly(t)
	if len(h.cl.calls) != 0 || len(h.sp.texts) != 0 {
		t.Fatalf("Judge calls=%d speaks=%d, want none", len(h.cl.calls), len(h.sp.texts))
	}
	if want := []string{voiceLog("text (long or mostly code)")}; !slices.Equal(h.voiceLogs(), want) {
		t.Fatalf("voice check log=%q, want %q", h.voiceLogs(), want)
	}
}

// A long factual answer speaks its opening paragraph: Jev judges the lead
// sentence alone, the voice says it, and the 💬 text that follows holds the
// whole answer. A lead Jev does not find factual is text, and the log says
// it was the opening paragraph that was judged.
func TestStopLongFactualSpeaksOpeningParagraph(t *testing.T) {
	h := newVoiceHarness(t)
	h.owe(t, time.Time{})
	h.phoneLine(t)
	if err := h.stop(t, parisAnswer); err != nil {
		t.Fatal(err)
	}
	h.spokenThenText(t)
	if len(h.cl.calls) != 1 || h.cl.calls[0].question != "What is the weather in Paris?" || h.cl.calls[0].answer != parisLead {
		t.Fatalf("Judge calls=%+v, want one with the phone's question and the lead sentence", h.cl.calls)
	}
	if !slices.Equal(h.sp.texts, []string{parisLead}) {
		t.Fatalf("speaker got %q, want the lead sentence", h.sp.texts)
	}
	if text := h.rec.Calls[0].Text; !strings.Contains(text, "the rain should stop around midday") || !strings.Contains(text, "a high of 19 °C and a light breeze") {
		t.Fatalf("text=%q, want the whole answer", text)
	}
	if want := []string{voiceLog("voice (opening paragraph; factual 0.94, listenable 0.91)")}; !slices.Equal(h.voiceLogs(), want) {
		t.Fatalf("voice check log=%q, want %q", h.voiceLogs(), want)
	}
	if !slices.ContainsFunc(h.logs, func(l string) bool { return strings.HasPrefix(l, "spoke 111 characters (") }) {
		t.Fatalf("log=%q, want the lead's length spoken", h.logs)
	}

	h = newVoiceHarness(t)
	h.cl.j = classify.Judgment{Factual: 0.12, Listenable: 0.91}
	h.owe(t, time.Time{})
	h.phoneLine(t)
	if err := h.stop(t, parisAnswer); err != nil {
		t.Fatal(err)
	}
	h.textOnly(t)
	if len(h.cl.calls) != 1 || h.cl.calls[0].answer != parisLead || len(h.sp.texts) != 0 {
		t.Fatalf("Judge calls=%+v speaks=%d, want the lead judged and nothing spoken", h.cl.calls, len(h.sp.texts))
	}
	if want := []string{voiceLog("text (opening paragraph; factual 0.12, listenable 0.91)")}; !slices.Equal(h.voiceLogs(), want) {
		t.Fatalf("voice check log=%q, want %q", h.voiceLogs(), want)
	}
}

// A table rules out only the whole answer: its prose lead is still a
// candidate, short (the table alone too much dropped to say) or long (the
// Marseille shape: a lead, figures, a trailing table).
func TestStopTableAnswerSpeaksOpening(t *testing.T) {
	const shortLead = "Both test runs passed on the new server, and the shop is live again."
	for _, tc := range []struct{ name, answer, lead string }{
		{"short", shortLead + "\n\n| run | ok |\n|---|---|", shortLead},
		{"Marseille", marseilleAnswer, marseilleLead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newVoiceHarness(t)
			h.owe(t, time.Time{})
			h.phoneLine(t)
			if err := h.stop(t, tc.answer); err != nil {
				t.Fatal(err)
			}
			h.spokenThenText(t)
			if len(h.cl.calls) != 1 || h.cl.calls[0].answer != tc.lead || !slices.Equal(h.sp.texts, []string{tc.lead}) {
				t.Fatalf("Judge calls=%+v speaks=%q, want the lead judged and spoken", h.cl.calls, h.sp.texts)
			}
			if want := []string{voiceLog("voice (opening paragraph; factual 0.94, listenable 0.91)")}; !slices.Equal(h.voiceLogs(), want) {
				t.Fatalf("voice check log=%q, want %q", h.voiceLogs(), want)
			}
		})
	}
}

// The owner asked by voice, so the answer is spoken by the hard rules
// alone, with a key or without (owner's ruling, 2026-10-26): a voice
// note's long answer speaks its opening paragraph, and Jev is never asked.
func TestStopVoiceNoteLongAnswerSpeaksOpening(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  bool
	}{{"with a key", true}, {"without a key", false}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newVoiceHarness(t)
			if !tc.key {
				h.deps.Classifier = nil
			}
			h.owe(t, h.now)
			if err := h.stop(t, parisAnswer); err != nil {
				t.Fatal(err)
			}
			h.spokenThenText(t)
			if len(h.cl.calls) != 0 || h.sp.texts[0] != parisLead {
				t.Fatalf("Judge calls=%d spoke=%q, want no judgment and the opening paragraph", len(h.cl.calls), h.sp.texts)
			}
			if want := []string{voiceLog("voice (asked by voice; opening paragraph)")}; !slices.Equal(h.voiceLogs(), want) {
				t.Fatalf("voice check log=%q, want %q", h.voiceLogs(), want)
			}
		})
	}
}

// A voice note's answer that opens with a short lead-in speaks the lead-in
// and the paragraph after it, not the lead-in alone.
func TestStopVoiceNoteLeadInSpeaksNextParagraph(t *testing.T) {
	const answer = "Звісно, ось українською.\n\nСтатусу поки немає: у цій сесії ще не було жодного завдання.\n\n" +
		"- У робочій теці `voicetest` лежить лише службовий файл `.logstart` (4 байти).\n" +
		"- Пам'ять для цього проєкту порожня, записів про минулі завдання немає."
	h := newVoiceHarness(t)
	h.owe(t, h.now)
	if err := h.stop(t, answer); err != nil {
		t.Fatal(err)
	}
	h.spokenThenText(t)
	if want := "Звісно, ось українською. Статусу поки немає: у цій сесії ще не було жодного завдання."; h.sp.texts[0] != want {
		t.Fatalf("spoke %q, want %q", h.sp.texts[0], want)
	}
	if want := []string{voiceLog("voice (asked by voice; opening paragraph)")}; !slices.Equal(h.voiceLogs(), want) {
		t.Fatalf("voice check log=%q, want %q", h.voiceLogs(), want)
	}
}

// An answer with too few letters to tell its language ("2 + 2 = 4.") is
// spoken in the language of the question: the voice mark's for a question
// asked by voice, the phone prompt's for a typed one. An answer that has
// its own language keeps it.
func TestStopSpeechLanguageFallsBackToTheQuestion(t *testing.T) {
	for _, tc := range []struct {
		name, answer, want string
		voice              bool
	}{
		{"a voice note, a sum", "2 + 2 = 4.", "ru", true},
		{"a typed question, a sum", "2 + 2 = 4.", "ru", false},
		{"a voice note, an English answer", "Done, the tests pass.", "en", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newVoiceHarness(t)
			h.prompt.Text = " Сколько будет два плюс два?"
			if tc.voice {
				if _, err := h.store.UpdateSession(sid, func(s *state.Session) {
					s.PendingAnswers++
					s.MarkVoice("", "ru", h.now)
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				h.owe(t, time.Time{})
				h.phoneLine(t)
			}
			if err := h.stop(t, tc.answer); err != nil {
				t.Fatal(err)
			}
			h.spokenThenText(t)
			if !slices.Equal(h.sp.langs, []string{tc.want}) {
				t.Fatalf("spoke in %q, want %q", h.sp.langs, tc.want)
			}
		})
	}
}

// Two voice notes in two languages, each answered by a sum: each answer is
// spoken in its own note's language, the oldest mark first.
func TestStopSpeechLanguagePerVoiceMark(t *testing.T) {
	h := newVoiceHarness(t)
	if _, err := h.store.UpdateSession(sid, func(s *state.Session) {
		s.PendingAnswers += 2
		s.MarkVoice("", "ru", h.now.Add(-time.Minute))
		s.MarkVoice("", "uk", h.now)
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := h.stop(t, "2 + 2 = 4."); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(h.sp.langs, []string{"ru", "uk"}) {
		t.Fatalf("spoke in %q, want ru then uk", h.sp.langs)
	}
}

// The voice gets the text with the symbols voices read out rewritten
// (speech.Voiced): «» as “”, a slash between letters and, in Ukrainian, a
// time's colon as spaces.
func TestStopSpeechVoicedText(t *testing.T) {
	h := newVoiceHarness(t)
	if _, err := h.store.UpdateSession(sid, func(s *state.Session) {
		s.PendingAnswers++
		s.MarkVoice("", "uk", h.now)
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.stop(t, "Хмарно у «Ліоні», вітер 6 км/год о 19:02."); err != nil {
		t.Fatal(err)
	}
	h.spokenThenText(t)
	if want := "Хмарно у “Ліоні”, вітер 6 км год о 19 02."; h.sp.texts[0] != want || h.sp.langs[0] != "uk" {
		t.Fatalf("spoke %q in %q, want %q in uk", h.sp.texts[0], h.sp.langs[0], want)
	}
}

// Code is text even when the owner asked by voice, with a key or without:
// the hard rules still decide, and the mark is spent with the answer.
func TestStopVoiceNoteCodeAnswerText(t *testing.T) {
	for _, key := range []bool{true, false} {
		t.Run(fmt.Sprintf("key %v", key), func(t *testing.T) { voiceNoteCodeAnswerText(t, key) })
	}
}

func voiceNoteCodeAnswerText(t *testing.T, key bool) {
	h := newVoiceHarness(t)
	if !key {
		h.deps.Classifier = nil
	}
	h.owe(t, h.now)
	// Prose with a short command: the 0.4.0 rule (speech.Speakable) spoke it.
	if err := h.stop(t, "The deploy finished and the shop is live again on the new server, with every check green and the cache warmed.\n\n```sh\nmake deploy\n```"); err != nil {
		t.Fatal(err)
	}
	h.textOnly(t)
	if len(h.cl.calls) != 0 || len(h.sp.texts) != 0 {
		t.Fatalf("Judge calls=%d speaks=%d, want none", len(h.cl.calls), len(h.sp.texts))
	}
	if want := []string{voiceLog("text (code block or table)")}; !slices.Equal(h.voiceLogs(), want) {
		t.Fatalf("voice check log=%q, want %q", h.voiceLogs(), want)
	}
	if s := h.session(t); len(s.VoiceMarks) != 0 {
		t.Fatalf("mark not spent: %+v", s.VoiceMarks)
	}
}

// A turn started at the Mac, with no voice mark, is never sent to
// TypeSafe and logs no voice check: not in a session without phone lines
// (whose transcript is not read, and which logs nothing of it), nor in one
// whose last prompt was typed at the Mac after an earlier phone line, which
// logs why the turn is not the phone's, with no prompt text.
func TestStopMacTurnNeverAsksJev(t *testing.T) {
	for _, tc := range []struct {
		name      string
		phoneLine string // "" = none
		reads     int
		phoneLog  []string
	}{
		{"no phone lines", "", 0, nil},
		{"a Mac prompt after a phone line", "deploy the shop", 1, []string{"phone check (" + sid + "): last prompt was not sent from the phone"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newVoiceHarness(t)
			h.follow(t)
			if tc.phoneLine != "" {
				if _, err := h.store.UpdateSession(sid, func(s *state.Session) { s.RecordPhoneLine(tc.phoneLine, h.prompt.At, false) }); err != nil {
					t.Fatal(err)
				}
			}
			if err := h.stop(t, weatherAnswer); err != nil {
				t.Fatal(err)
			}
			h.textOnly(t)
			if len(h.cl.calls) != 0 || len(h.voiceLogs()) != 0 || h.reads != tc.reads {
				t.Fatalf("Judge calls=%d voice check log=%q reads=%d, want none, none, %d", len(h.cl.calls), h.voiceLogs(), h.reads, tc.reads)
			}
			if s := h.session(t); slices.ContainsFunc(s.PhoneLines, func(l state.PhoneLine) bool { return l.Answered }) {
				t.Fatalf("a phone line was spent by a Mac turn: %+v", s.PhoneLines)
			}
			var phoneLog []string
			for _, l := range h.logs {
				if strings.HasPrefix(l, "phone check") {
					phoneLog = append(phoneLog, l)
				}
			}
			if !slices.Equal(phoneLog, tc.phoneLog) {
				t.Fatalf("phone check log=%q, want %q", phoneLog, tc.phoneLog)
			}
		})
	}
}

// One phone line makes one answer a candidate: a second Stop of the same
// turn (a background task's answer after the real one) is text only, and
// TypeSafe is asked once.
func TestStopOnePhoneLineOneCandidate(t *testing.T) {
	h := newVoiceHarness(t)
	h.follow(t)
	h.phoneLine(t)
	for range 2 {
		if err := h.stop(t, weatherAnswer); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.cl.calls) != 1 || len(h.voiceLogs()) != 1 {
		t.Fatalf("Judge calls=%d voice check log=%q, want one of each", len(h.cl.calls), h.voiceLogs())
	}
	if !slices.Equal(h.sn.order, []string{"voice", "text", "text"}) || len(h.sp.texts) != 1 {
		t.Fatalf("order=%v speaks=%d, want the first answer spoken and the second text only", h.sn.order, len(h.sp.texts))
	}
}

// With a key, a voice note's answer is still spoken by the hard rules
// alone: Jev is never asked, however it would judge, and the mark and the
// voice note's phone line are both spent.
func TestStopVoiceNoteWithClassifier(t *testing.T) {
	h := newVoiceHarness(t)
	h.cl.j = classify.Judgment{Factual: 0.2, Listenable: 0.9}
	h.owe(t, h.now) // the transcript delivered marks the session...
	h.phoneLine(t)  // ...and records its line
	if err := h.stop(t, shortAnswer); err != nil {
		t.Fatal(err)
	}
	h.spokenThenText(t)
	if len(h.cl.calls) != 0 {
		t.Fatalf("Judge calls=%+v, want none", h.cl.calls)
	}
	if want := []string{voiceLog("voice (asked by voice)")}; !slices.Equal(h.voiceLogs(), want) {
		t.Fatalf("voice check log=%q, want %q", h.voiceLogs(), want)
	}
	if s := h.session(t); len(s.VoiceMarks) != 0 || !s.PhoneLines[0].Answered {
		t.Fatalf("mark or phone line not spent: %+v", s)
	}
}

// Without a key a voice note's short answer is spoken as with one; the
// transcript is not read and the phone line is left as it was.
func TestStopVoiceNoteWithoutClassifier(t *testing.T) {
	h := newVoiceHarness(t)
	h.deps.Classifier = nil
	h.owe(t, h.now)
	h.phoneLine(t)
	if err := h.stop(t, shortAnswer); err != nil {
		t.Fatal(err)
	}
	h.spokenThenText(t)
	if h.reads != 0 {
		t.Fatalf("reads=%d, want none", h.reads)
	}
	if want := []string{voiceLog("voice (asked by voice)")}; !slices.Equal(h.voiceLogs(), want) {
		t.Fatalf("voice check log=%q, want %q", h.voiceLogs(), want)
	}
	if s := h.session(t); len(s.VoiceMarks) != 0 || s.PhoneLines[0].Answered {
		t.Fatalf("session=%+v, want the mark spent and the phone line untouched", s)
	}
}

// A prompt answered by voice in a turn started at the Mac makes its answer
// spoken by the hard rules alone, as a voice note's: Jev is never asked.
func TestStopVoiceAskInMacTurn(t *testing.T) {
	h := newVoiceHarness(t)
	_, err := h.store.UpdateSession(sid, func(s *state.Session) {
		s.AskCredits = []string{"a1b2c3d4"}
		s.MarkVoice("a1b2c3d4", "", h.now)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.stop(t, shortAnswer); err != nil {
		t.Fatal(err)
	}
	h.spokenThenText(t)
	if len(h.cl.calls) != 0 || h.reads != 0 {
		t.Fatalf("Judge calls=%+v reads=%d, want no judgment and no transcript read", h.cl.calls, h.reads)
	}
}

// Any TypeSafe failure means text only, and the log says why; an error
// that names TypeSafe already is not named twice.
func TestStopClassifierErrorMeansText(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{errors.New("boom"), "text (typesafe: boom)"},
		{&classify.APIError{Status: 529, Detail: "overloaded"}, "text (typesafe: HTTP 529: Overloaded)"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			h := newVoiceHarness(t)
			h.cl.err = tc.err
			h.owe(t, time.Time{})
			h.phoneLine(t)
			if err := h.stop(t, weatherAnswer); err != nil {
				t.Fatal(err)
			}
			h.textOnly(t)
			if want := []string{voiceLog(tc.want)}; !slices.Equal(h.voiceLogs(), want) {
				t.Fatalf("voice check log=%q, want %q", h.voiceLogs(), want)
			}
		})
	}
}

// TypeSafe never answering costs only the voice: Jev's call ends with the
// text's reserve kept (Deps.TextReserve), so the text is sent, not
// spooled. Jev gets classify.DefaultTimeout at most, within the hook's
// budget.
func TestStopClassifierTimeoutStillSendsText(t *testing.T) {
	h := newVoiceHarness(t)
	h.cl.block = true
	h.deps.TextReserve = time.Second
	h.owe(t, time.Time{})
	h.phoneLine(t)
	ctx, cancel := context.WithTimeout(context.Background(), h.deps.TextReserve+300*time.Millisecond)
	defer cancel()
	if err := h.stopCtx(t, ctx, weatherAnswer); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("Jev's call used the text's reserve")
	}
	h.textOnly(t)
	if items, err := h.store.SpoolList(); err != nil || len(items) != 0 {
		t.Fatalf("spool=%+v err=%v, want the text sent", items, err)
	}
	if len(h.cl.calls) != 1 || h.cl.calls[0].left <= 0 || h.cl.calls[0].left > classify.DefaultTimeout {
		t.Fatalf("Judge calls=%+v, want one with a deadline at most %s away", h.cl.calls, classify.DefaultTimeout)
	}
	if want := []string{voiceLog("text (typesafe: " + context.DeadlineExceeded.Error() + ")")}; !slices.Equal(h.voiceLogs(), want) {
		t.Fatalf("voice check log=%q, want %q", h.voiceLogs(), want)
	}

	// The hook's real budget leaves far more than 5 s: Jev still gets 5 s.
	h = newVoiceHarness(t)
	h.cl.err = errors.New("boom")
	h.deps.TextReserve = 15 * time.Second
	h.owe(t, time.Time{})
	h.phoneLine(t)
	ctx, cancel = context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.stopCtx(t, ctx, weatherAnswer); err != nil {
		t.Fatal(err)
	}
	if len(h.cl.calls) != 1 || h.cl.calls[0].left <= classify.DefaultTimeout-time.Second || h.cl.calls[0].left > classify.DefaultTimeout {
		t.Fatalf("Judge calls=%+v, want one with a deadline %s away", h.cl.calls, classify.DefaultTimeout)
	}
}

// Jev runs inside the speech window: classification, speech and the voice
// upload share it (Deps.SpeechWindow), so the text is never later than
// 0.4.0's bound. A classifier and a speech command that each take most of
// the window cannot both finish in it: the voice is dropped, and the text
// goes as the window ends.
func TestStopJevInsideSpeechWindow(t *testing.T) {
	h := newVoiceHarness(t)
	h.deps.SpeechWindow = 1500 * time.Millisecond
	h.deps.TextReserve = time.Second
	h.cl.delay = time.Second // each well within the window, both together well over it
	h.sp.delay = time.Second
	h.owe(t, time.Time{})
	h.phoneLine(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	if err := h.stopCtx(t, ctx, weatherAnswer); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > h.deps.SpeechWindow+500*time.Millisecond {
		t.Fatalf("the text came after %s, want the %s window and little more", took, h.deps.SpeechWindow)
	}
	h.textOnly(t)
	if len(h.cl.calls) != 1 || len(h.sp.texts) != 1 {
		t.Fatalf("Judge calls=%d speaks=%d, want one of each", len(h.cl.calls), len(h.sp.texts))
	}
	if left := h.cl.calls[0].left; left <= 0 || left > h.deps.SpeechWindow {
		t.Fatalf("Jev's deadline was %s away, want within the %s window", left, h.deps.SpeechWindow)
	}
	if !h.logged("speech: " + context.DeadlineExceeded.Error()) {
		t.Fatalf("log=%q", h.logs)
	}
}

// Without a speech command nothing is spoken, so TypeSafe is never asked
// and the transcript is not read.
func TestStopNoSpeakerNeverAsksJev(t *testing.T) {
	h := newVoiceHarness(t)
	h.deps.Speaker = nil
	h.owe(t, time.Time{})
	h.phoneLine(t)
	if err := h.stop(t, weatherAnswer); err != nil {
		t.Fatal(err)
	}
	h.textOnly(t)
	if len(h.cl.calls) != 0 || h.reads != 0 || len(h.voiceLogs()) != 0 {
		t.Fatalf("Judge calls=%d reads=%d voice check log=%q, want none", len(h.cl.calls), h.reads, h.voiceLogs())
	}
}

// TypeSafe gets the prompt and the answer cleaned as a 💬 message is:
// credentials redacted, never the password; nor does the log hold it.
func TestStopJevGetsCleanedText(t *testing.T) {
	const secret = "hunter2secret"
	h := newVoiceHarness(t)
	h.prompt.Text = " Log in with password=" + secret + " and tell me the weather in Paris"
	h.owe(t, time.Time{})
	h.phoneLine(t)
	if err := h.stop(t, "Logged in with password="+secret+"; it is 14 degrees and cloudy in Paris."); err != nil {
		t.Fatal(err)
	}
	if len(h.cl.calls) != 1 {
		t.Fatalf("Judge calls=%d, want one", len(h.cl.calls))
	}
	for _, arg := range []string{h.cl.calls[0].question, h.cl.calls[0].answer} {
		if !strings.Contains(arg, "[redacted]") || strings.Contains(arg, secret) {
			t.Fatalf("Judge got %q, want the password redacted", arg)
		}
	}
	if strings.Contains(strings.Join(h.logs, "\n"), secret) {
		t.Fatalf("the password reached the log: %q", h.logs)
	}
}

// A phone line whose spending could not be saved makes no candidate: it is
// still on disk, unanswered, and the answer is text only, with TypeSafe
// never asked.
func TestStopPhoneLineWriteFailsText(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	for _, followed := range []bool{false, true} {
		t.Run(fmt.Sprintf("followed=%v", followed), func(t *testing.T) {
			h := newVoiceHarness(t)
			if followed {
				h.follow(t)
			} else {
				h.owe(t, time.Time{})
			}
			h.phoneLine(t)
			dir := filepath.Join(h.store.Dir, "sessions")
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			if err := h.stop(t, weatherAnswer); err != nil {
				t.Fatal(err)
			}
			_ = os.Chmod(dir, 0o700)
			h.textOnly(t)
			if len(h.cl.calls) != 0 || len(h.sp.texts) != 0 {
				t.Fatalf("Judge calls=%d speaks=%d on a line the hook could not spend, want none", len(h.cl.calls), len(h.sp.texts))
			}
			if s := h.session(t); len(s.PhoneLines) != 1 || s.PhoneLines[0].Answered {
				t.Fatalf("phone lines=%+v, want the line unanswered", s.PhoneLines)
			}
		})
	}
}

// An unfollowed session's phone line is spent only with an answer owed:
// when nothing is owed, nothing is sent, TypeSafe is not asked and the
// line is left for the answer that is.
func TestStopPhoneLineNotOwedNotSpent(t *testing.T) {
	h := newVoiceHarness(t)
	h.phoneLine(t)
	if err := h.stop(t, weatherAnswer); err != nil {
		t.Fatal(err)
	}
	if len(h.rec.Calls) != 0 || len(h.rec.Voices) != 0 || len(h.cl.calls) != 0 {
		t.Fatalf("calls=%d voices=%d Judge calls=%d, want nothing", len(h.rec.Calls), len(h.rec.Voices), len(h.cl.calls))
	}
	if s := h.session(t); len(s.PhoneLines) != 1 || s.PhoneLines[0].Answered {
		t.Fatalf("phone lines=%+v, want the line unanswered", s.PhoneLines)
	}
}

// A key TypeSafe rejects logs the client's own error once named: a real
// *classify.APIError from a 401, never "typesafe: typesafe:", never the key.
func TestStopClassifierRejectedKeyLogsOnce(t *testing.T) {
	const key = "k-secret-123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"invalid key ` + key + `"}`)) // echoing the key back
	}))
	defer srv.Close()
	h := newVoiceHarness(t)
	h.deps.Classifier = &classify.Client{Key: key, BaseURL: srv.URL, HTTP: srv.Client()}
	h.owe(t, time.Time{})
	h.phoneLine(t)
	if err := h.stop(t, weatherAnswer); err != nil {
		t.Fatal(err)
	}
	h.textOnly(t)
	if want := []string{voiceLog("text (typesafe: HTTP 401: Unauthorized)")}; !slices.Equal(h.voiceLogs(), want) {
		t.Fatalf("voice check log=%q, want %q", h.voiceLogs(), want)
	}
	if strings.Contains(strings.Join(h.logs, "\n"), key) {
		t.Fatalf("the key reached the log: %q", h.logs)
	}
}

// The Stop hook judges and spends only the phone line of the turn it
// stops. A follow-up the phone queued to the busy session is on disk once
// the turn ends, its prompt after the answer: Jev never sees it, and its
// line stays for its own turn's Stop. The line the answer's own turn
// started from, if any, is the one spent.
func TestStopSpendsOnlyItsTurnsPhoneLine(t *testing.T) {
	const followUp = " And what about Lyon?"
	for _, tc := range []struct {
		name  string
		asked string // the turn's prompt
		phone bool   // typed on the phone
	}{
		{"a phone turn", weatherPrompt, true},
		{"a Mac turn", "what is the weather in Paris?", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newVoiceHarness(t)
			asked, queued := h.now.Add(-time.Minute), h.now.Add(-20*time.Second)
			h.transcript = writeTranscript(t, userLine(tc.asked, asked)+assistantLine(weatherAnswer)+queuedLine(followUp, h.now.Add(-time.Second)))
			h.owe(t, time.Time{})
			if _, err := h.store.UpdateSession(sid, func(s *state.Session) {
				if tc.phone {
					s.RecordPhoneLine(tc.asked, asked, false)
				}
				s.RecordPhoneLine(followUp, queued, true)
			}); err != nil {
				t.Fatal(err)
			}
			if err := h.stop(t, weatherAnswer); err != nil {
				t.Fatal(err)
			}
			for _, c := range h.cl.calls {
				if strings.Contains(c.question, "Lyon") {
					t.Fatalf("Jev got the next turn's prompt: %+v", h.cl.calls)
				}
			}
			s := h.session(t)
			if l := s.PhoneLines[len(s.PhoneLines)-1]; l.Answered {
				t.Fatalf("the follow-up's line was spent: %+v", s.PhoneLines)
			}
			if !tc.phone {
				h.textOnly(t)
				if len(h.cl.calls) != 0 {
					t.Fatalf("Judge calls=%+v on a Mac turn, want none", h.cl.calls)
				}
				return
			}
			h.spokenThenText(t)
			if len(h.cl.calls) != 1 || h.cl.calls[0].question != "What is the weather in Paris?" {
				t.Fatalf("Judge calls=%+v, want one with the turn's own question", h.cl.calls)
			}
			if !s.PhoneLines[0].Answered {
				t.Fatalf("the turn's own line was not spent: %+v", s.PhoneLines)
			}
		})
	}
}

// The spool is looked at before Jev is asked: while older messages are
// still undelivered the voice would be refused, so no request is sent for
// it. The text queues behind them as always.
func TestStopBacklogNeverAsksJev(t *testing.T) {
	h := newVoiceHarness(t)
	netErr := &telegram.NetError{Err: errors.New("dial tcp: connection refused")}
	h.rec.Script = []error{netErr, netErr} // the hook's flush, then the text's
	older := state.SpoolItem{ID: "1-00-aaaaaa", SessionID: sid, Created: time.Now().Add(-time.Hour),
		Req: telegram.SendRequest{ChatID: 1, Text: "🔔 <b>acme-shop-92</b>\nolder", ParseMode: "HTML"}, Plain: "🔔 acme-shop-92\nolder"}
	if err := h.store.SpoolPut(older); err != nil {
		t.Fatal(err)
	}
	h.owe(t, time.Time{})
	h.phoneLine(t)
	if err := h.stop(t, weatherAnswer); !errors.Is(err, send.ErrSpooled) {
		t.Fatalf("err=%v, want the text spooled", err)
	}
	if len(h.cl.calls) != 0 || len(h.sp.texts) != 0 || len(h.voiceLogs()) != 0 {
		t.Fatalf("Judge calls=%d speaks=%d voice check log=%q behind a backlog, want none", len(h.cl.calls), len(h.sp.texts), h.voiceLogs())
	}
	if !h.logged("speech: older messages are still undelivered") {
		t.Fatalf("log=%q", h.logs)
	}
	if s := h.session(t); !s.PhoneLines[0].Answered {
		t.Fatalf("phone line not spent: %+v", s.PhoneLines)
	}
}

// The phone's prompt goes to Jev as question cut to 1000 runes, on a rune
// boundary and marked with "…", after it is cleaned: a configured secret
// crossing the cut leaves no prefix of itself. One of 1000 runes goes
// whole.
func TestStopQuestionCapped(t *testing.T) {
	const key = "tsk_live_9QzXv7LmPq2Rw8Yb"
	long := strings.Repeat("ж", 995) + " " + key + " and more"
	for _, tc := range []struct {
		name, prompt, want string
	}{
		{"long", " " + long, strings.Repeat("ж", 995) + " [re…"},
		{"1000 runes", " " + strings.Repeat("ж", 1000), strings.Repeat("ж", 1000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newVoiceHarness(t)
			h.deps.Sender.Secrets = []string{key}
			h.prompt.Text = tc.prompt
			h.owe(t, time.Time{})
			h.phoneLine(t)
			if err := h.stop(t, weatherAnswer); err != nil {
				t.Fatal(err)
			}
			if len(h.cl.calls) != 1 {
				t.Fatalf("Judge calls=%d, want one", len(h.cl.calls))
			}
			if q := h.cl.calls[0].question; q != tc.want || strings.Contains(q, key[:4]) {
				t.Fatalf("question=%q (%d runes), want %q", q, len([]rune(q)), tc.want)
			}
		})
	}
}

// A TypeSafe error reply never puts the server's text in the log: an
// APIError logs its status and the status's name, never its detail, which
// a 422 fills with the request's own words.
func TestStopClassifierErrorLogsNoServerText(t *testing.T) {
	const msg = "Input should be a short reply"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = fmt.Fprintf(w, `{"detail":[{"loc":["body","state","answer"],"msg":%s}]}`, jsonText(msg+": "+weatherAnswer))
	}))
	defer srv.Close()
	h := newVoiceHarness(t)
	h.deps.Classifier = &classify.Client{Key: "k-secret-123", BaseURL: srv.URL, HTTP: srv.Client()}
	h.owe(t, time.Time{})
	h.phoneLine(t)
	if err := h.stop(t, weatherAnswer); err != nil {
		t.Fatal(err)
	}
	h.textOnly(t)
	if want := []string{voiceLog("text (typesafe: HTTP 422: Unprocessable Entity)")}; !slices.Equal(h.voiceLogs(), want) {
		t.Fatalf("voice check log=%q, want %q", h.voiceLogs(), want)
	}
	if logs := strings.Join(h.logs, "\n"); strings.Contains(logs, "14 degrees") || strings.Contains(logs, msg) {
		t.Fatalf("the server's text reached the log: %q", h.logs)
	}
}

// The phone check's reason is logged on an owed Stop only while the
// session's newest phone line is within phoneTurnTTL: a followed session
// that once got a phone line does not log it on every Mac turn after.
func TestStopPhoneCheckLogOnlyWhileRecent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ago    time.Duration
		logged bool
	}{
		{"a phone line 30 minutes old", 30 * time.Minute, true},
		{"a phone line over an hour old", phoneTurnTTL + time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newVoiceHarness(t)
			h.follow(t)
			// The transcript's last prompt (h.prompt) was typed at the Mac.
			if _, err := h.store.UpdateSession(sid, func(s *state.Session) { s.RecordPhoneLine("deploy the shop", h.now.Add(-tc.ago), false) }); err != nil {
				t.Fatal(err)
			}
			if err := h.stop(t, weatherAnswer); err != nil {
				t.Fatal(err)
			}
			h.textOnly(t)
			want := []string(nil)
			if tc.logged {
				want = []string{"phone check (" + sid + "): last prompt was not sent from the phone"}
			}
			var got []string
			for _, l := range h.logs {
				if strings.HasPrefix(l, "phone check") {
					got = append(got, l)
				}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("phone check log=%q, want %q", got, want)
			}
		})
	}
}

// Neither the bot token nor the TypeSafe key, in the phone's prompt or in
// the answer, ever reaches Jev or the log: both are scrubbed as a 💬
// message's are.
func TestStopJevNeverGetsConfiguredSecrets(t *testing.T) {
	const bot, key = "123456:ABCDEFsecretbot", "tsk_live_9QzXv7LmPq2Rw8Yb"
	h := newVoiceHarness(t)
	h.deps.Sender.Secrets = []string{bot, key}
	h.prompt.Text = " Is the bot " + bot + " set up with the key " + key + "?"
	h.owe(t, time.Time{})
	h.phoneLine(t)
	if err := h.stop(t, "Yes: the bot "+bot+" uses the key "+key+", and it works."); err != nil {
		t.Fatal(err)
	}
	if len(h.cl.calls) != 1 {
		t.Fatalf("Judge calls=%d, want one", len(h.cl.calls))
	}
	for _, arg := range []string{h.cl.calls[0].question, h.cl.calls[0].answer} {
		if strings.Contains(arg, bot) || strings.Contains(arg, key) || strings.Count(arg, "[redacted]") != 2 {
			t.Fatalf("Judge got %q, want both secrets scrubbed", arg)
		}
	}
	if logs := strings.Join(h.logs, "\n"); strings.Contains(logs, bot) || strings.Contains(logs, key) {
		t.Fatalf("a secret reached the log: %q", h.logs)
	}
}

// A Stop whose transcript's last line is still being written cannot tell
// its turn's prompt: the answer is text only, Jev is never asked, the
// phone line is left, and the log says why.
func TestStopUnfinishedTranscriptText(t *testing.T) {
	h := newVoiceHarness(t)
	asked := h.now.Add(-time.Minute)
	next := userLine("typed at the Mac", h.now)
	h.transcript = writeTranscript(t, userLine(weatherPrompt, asked)+assistantLine(weatherAnswer)+next[:len(next)/2])
	h.prompt.At = asked
	h.owe(t, time.Time{})
	h.phoneLine(t)
	if err := h.stop(t, weatherAnswer); err != nil {
		t.Fatal(err)
	}
	h.textOnly(t)
	if len(h.cl.calls) != 0 || len(h.voiceLogs()) != 0 {
		t.Fatalf("Judge calls=%d voice check log=%q, want none", len(h.cl.calls), h.voiceLogs())
	}
	if !h.logged("phone check (" + sid + "): last line still being written") {
		t.Fatalf("log=%q", h.logs)
	}
	if s := h.session(t); s.PhoneLines[0].Answered {
		t.Fatalf("phone line spent: %+v", s.PhoneLines)
	}
	if len(h.sleeps) != answerPolls || h.reads != 1 {
		t.Fatalf("sleeps=%d reads=%d, want %d sleeps and one read: the file never changed", len(h.sleeps), h.reads, answerPolls)
	}
}

// Claude Code 2.1.310 runs the Stop hook before it writes the turn's
// entries to the transcript, and writes them about 60 ms later, while the
// hook runs. The hook reads again every answerPoll until the answer is
// there: the phone's typed question is then judged and spoken as usual.
func TestStopWaitsForTheAnswerInTheTranscript(t *testing.T) {
	h := newVoiceHarness(t)
	asked := h.now.Add(-time.Minute)
	h.transcript = writeTranscript(t, userLine(weatherPrompt, asked))
	h.prompt.At = asked
	h.owe(t, time.Time{})
	h.phoneLine(t)
	h.deps.Sleep = func(ctx context.Context, d time.Duration) bool {
		h.sleeps = append(h.sleeps, d)
		if len(h.sleeps) == 2 {
			f, err := os.OpenFile(h.transcript, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteString(assistantLine(weatherAnswer)); err != nil {
				t.Fatal(err)
			}
		}
		return true
	}
	if err := h.stop(t, weatherAnswer); err != nil {
		t.Fatal(err)
	}
	h.spokenThenText(t)
	if len(h.cl.calls) != 1 || h.cl.calls[0].question != "What is the weather in Paris?" {
		t.Fatalf("Judge calls=%+v, want one with the phone's question", h.cl.calls)
	}
	if h.reads != 2 || !slices.Equal(h.sleeps, []time.Duration{answerPoll, answerPoll}) {
		t.Fatalf("reads=%d sleeps=%v, want 2 reads (none while the file did not change), answerPoll apart", h.reads, h.sleeps)
	}
	if s := h.session(t); !s.PhoneLines[0].Answered {
		t.Fatalf("phone line not spent: %+v", s.PhoneLines)
	}
	for _, l := range h.logs {
		if strings.HasPrefix(l, "phone check") {
			t.Fatalf("logged %q", l)
		}
	}
}

// A line the phone queued to the busy session may be taken in up to an
// hour after its record, and its turn's hour runs from its prompt: the
// hook waits for the answer to a line recorded 70 minutes ago whose
// prompt was written 20 minutes ago.
func TestStopWaitsForTheAnswerToALateQueuedLine(t *testing.T) {
	h := newVoiceHarness(t)
	asked := h.now.Add(-20 * time.Minute)
	h.transcript = writeTranscript(t, queuedLine(weatherPrompt, asked))
	h.owe(t, time.Time{})
	if _, err := h.store.UpdateSession(sid, func(s *state.Session) { s.RecordPhoneLine(weatherPrompt, h.now.Add(-70*time.Minute), true) }); err != nil {
		t.Fatal(err)
	}
	h.deps.Sleep = func(ctx context.Context, d time.Duration) bool {
		h.sleeps = append(h.sleeps, d)
		if len(h.sleeps) == 1 {
			f, err := os.OpenFile(h.transcript, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteString(assistantLine(weatherAnswer)); err != nil {
				t.Fatal(err)
			}
		}
		return true
	}
	if err := h.stop(t, weatherAnswer); err != nil {
		t.Fatal(err)
	}
	h.spokenThenText(t)
	if s := h.session(t); !s.PhoneLines[0].Answered {
		t.Fatalf("phone line not spent: %+v", s.PhoneLines)
	}
}

// An answer that does not reach the transcript within answerPolls reads,
// or a hook whose time ends first, leaves the turn's prompt unknown: the
// answer is text only, Jev is never asked, the phone line is kept, and the
// log says why.
func TestStopAnswerNotInTranscriptText(t *testing.T) {
	for _, tc := range []struct {
		name   string
		awake  bool // Deps.Sleep's answer: false = the hook's time ended
		sleeps int
	}{
		{"never written", true, answerPolls},
		{"the hook's time ends", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newVoiceHarness(t)
			asked := h.now.Add(-time.Minute)
			h.transcript = writeTranscript(t, userLine(weatherPrompt, asked))
			h.prompt.At = asked
			h.owe(t, time.Time{})
			h.phoneLine(t)
			h.deps.Sleep = func(_ context.Context, d time.Duration) bool {
				h.sleeps = append(h.sleeps, d)
				return tc.awake
			}
			if err := h.stop(t, weatherAnswer); err != nil {
				t.Fatal(err)
			}
			h.textOnly(t)
			if len(h.cl.calls) != 0 || len(h.sleeps) != tc.sleeps {
				t.Fatalf("Judge calls=%d sleeps=%d, want none and %d", len(h.cl.calls), len(h.sleeps), tc.sleeps)
			}
			if !h.logged("phone check (" + sid + "): answer not in the transcript") {
				t.Fatalf("log=%q", h.logs)
			}
			if s := h.session(t); s.PhoneLines[0].Answered {
				t.Fatalf("phone line spent: %+v", s.PhoneLines)
			}
		})
	}
}

// Waiting for the answer ends Deps.TextReserve before the hook's time
// does, as speech does: the text keeps its time. With no more time than
// the reserve left, the hook does not wait at all. No sleep runs past
// answerWait after the wait began.
func TestStopAnswerWaitKeepsTheTextReserve(t *testing.T) {
	for _, tc := range []struct {
		name   string
		left   time.Duration // the hook's time
		sleeps int
	}{
		{"time to wait", time.Minute, answerPolls},
		{"no time beyond the reserve", 5 * time.Second, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newVoiceHarness(t)
			h.deps.TextReserve = 10 * time.Second
			asked := h.now.Add(-time.Minute)
			h.transcript = writeTranscript(t, userLine(weatherPrompt, asked))
			h.prompt.At = asked
			h.owe(t, time.Time{})
			h.phoneLine(t)
			ctx, cancel := context.WithTimeout(context.Background(), tc.left)
			defer cancel()
			outer, _ := ctx.Deadline()
			h.deps.Sleep = func(ctx context.Context, d time.Duration) bool {
				h.sleeps = append(h.sleeps, d)
				if dl, ok := ctx.Deadline(); !ok || dl.After(outer.Add(-h.deps.TextReserve)) || dl.After(time.Now().Add(answerWait)) {
					t.Errorf("waited with deadline %v (set %v), want by %v and within %v", dl, ok, outer.Add(-h.deps.TextReserve), answerWait)
				}
				return ctx.Err() == nil
			}
			if err := h.stopCtx(t, ctx, weatherAnswer); err != nil {
				t.Fatal(err)
			}
			h.textOnly(t)
			if len(h.sleeps) != tc.sleeps {
				t.Fatalf("sleeps=%d, want %d", len(h.sleeps), tc.sleeps)
			}
		})
	}
}

// The hook waits only for an answer that can be judged as the phone's: one
// the phone gets, in a session holding a phone line no answer took yet,
// whose prompt may be within the hour. Otherwise one read decides: a Mac turn after
// the phone's answered line, a stale line or an answer owed to no one
// costs no wait.
func TestStopNoAnswerWaitWhenNothingToJudge(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *voiceHarness, t *testing.T)
	}{
		{"the phone line already answered", func(h *voiceHarness, t *testing.T) {
			h.owe(t, time.Time{})
			h.phoneLine(t)
			if _, err := h.store.UpdateSession(sid, func(s *state.Session) { s.PhoneLines[0].Answered = true }); err != nil {
				t.Fatal(err)
			}
		}},
		{"the phone line over an hour old", func(h *voiceHarness, t *testing.T) {
			h.prompt.At = h.now.Add(-2 * time.Hour)
			h.owe(t, time.Time{})
			h.phoneLine(t)
		}},
		{"nothing owed, not followed", func(h *voiceHarness, t *testing.T) {
			h.phoneLine(t)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newVoiceHarness(t)
			h.prompt.At = h.now.Add(-time.Minute)
			tc.setup(h, t)
			h.transcript = writeTranscript(t, userLine(weatherPrompt, h.prompt.At))
			if err := h.stop(t, weatherAnswer); err != nil {
				t.Fatal(err)
			}
			if len(h.sleeps) != 0 || h.reads != 1 || len(h.cl.calls) != 0 {
				t.Fatalf("sleeps=%d reads=%d Judge calls=%d, want one read, no wait, no judgment", len(h.sleeps), h.reads, len(h.cl.calls))
			}
		})
	}
}
