package relay

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/IZZY-Agency/claude-tg/internal/relay/relaytest"
	"github.com/IZZY-Agency/claude-tg/internal/render"
	"github.com/IZZY-Agency/claude-tg/internal/session"
	"github.com/IZZY-Agency/claude-tg/internal/state"
)

// label stores a session's label (session.Label) and Claude Code's name, as
// the hooks do.
func (f *fixture) label(t *testing.T, sid, name, claude string) {
	t.Helper()
	if _, err := f.store.UpdateSession(sid, func(s *state.Session) { s.Name, s.ClaudeName = name, claude }); err != nil {
		t.Fatal(err)
	}
}

// keyboardTexts are the button texts of the last message sent.
func (f *fixture) keyboardTexts() []string {
	var out []string
	for _, row := range f.api.Sent[len(f.api.Sent)-1].InlineKeyboard {
		for _, b := range row {
			out = append(out, b.Text)
		}
	}
	return out
}

// Sessions are addressed by their labels, an exact match ignoring case
// first ("@acme" is the tab Acme, not "acme · Fix…"), and by Claude Code's
// names, which keep working. A session matched by its label and its own
// Claude Code name is one session, not two.
func TestAddressByLabel(t *testing.T) {
	for _, c := range []struct {
		text, pane, typed string
	}{
		{"@acme run the tests", "A1", " run the tests\n"},
		{"@fix run the tests", "", ""}, // a word inside a label is no prefix
		{"@acme-shop-fa run the tests", "A2", " run the tests\n"},
		{"acme-shop-fa: run the tests", "A2", " run the tests\n"},
		{"@acme-shop-92 run the tests", "A1", " run the tests\n"},
		{"@ACME run the tests", "A1", " run the tests\n"},
	} {
		t.Run(c.text, func(t *testing.T) {
			f := newFixture(t)
			f.label(t, sidShop, "Acme", "acme-shop-92")
			f.label(t, sidShopF, "acme-shop · Fix the checkout bug", "acme-shop-fa")
			f.handle(t, f.msg(c.text, 0))
			if c.pane == "" {
				if len(f.term.Typed) != 0 {
					t.Fatalf("typed %+v, want nothing", f.term.Typed)
				}
				return
			}
			f.typedOnce(t, c.pane, c.typed)
		})
	}
}

// Two live sessions with the same label are told apart in the relay's
// lists and buttons by the end of Claude Code's name, and a prefix of the
// shared label is ambiguous.
func TestDuplicateLabels(t *testing.T) {
	f := newFixture(t)
	f.label(t, sidShop, "Acme", "acme-shop-92")
	f.label(t, sidShopF, "Acme", "acme-shop-fa")
	f.handle(t, f.msg("@acme run the tests", 0))
	if len(f.term.Typed) != 0 {
		t.Fatalf("typed %+v into one of two sessions named alike", f.term.Typed)
	}
	if got := f.keyboardTexts(); !slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "Acme · 92") }) ||
		!slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "Acme · fa") }) {
		t.Fatalf("buttons %q, want Acme · 92 and Acme · fa", got)
	}
	f.handle(t, f.msg("/sessions", 0))
	if last := plain(f.api.LastText()); !strings.Contains(last, "Acme · 92") || !strings.Contains(last, "Acme · fa") {
		t.Fatalf("/sessions = %q", last)
	}
}

// A session no hook has labelled yet goes by Claude Code's name.
func TestUnlabelledSessionUsesClaudeName(t *testing.T) {
	f := newFixture(t)
	f.label(t, sidShop, "", "")
	f.handle(t, f.msg("/sessions", 0))
	if last := plain(f.api.LastText()); !strings.Contains(last, "acme-shop-92") {
		t.Fatalf("/sessions = %q", last)
	}
}

// The relay's log lines name sessions, whose labels come from tab names
// and titles: secrets and credentials in them are masked.
func TestRelayLogMasksLabels(t *testing.T) {
	const bot = "123456:ABCDEFsecretbot"
	f := newFixture(t)
	f.r.Secrets = []string{bot}
	var logged []string
	f.r.Log = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	f.label(t, sidShop, "ops "+bot+" password=hunter2hunter2", "acme-shop-92")
	f.handle(t, f.msg("@acme-shop-92 run the tests", 0))
	f.typedOnce(t, "A1", " run the tests\n")
	all := strings.Join(logged, "\n")
	if !strings.Contains(all, "delivered") || strings.Contains(all, "secretbot") || strings.Contains(all, "hunter2") {
		t.Fatalf("log = %q", all)
	}
}

// A name is matched as it is shown: one holding a credential is shown
// redacted ("•••"), and that shown name, copied into an address, reaches
// it.
func TestShownNameAddresses(t *testing.T) {
	f := newFixture(t)
	f.label(t, sidShop, "ops password=abcdefghij", "acme-shop-92")
	f.handle(t, f.msg("/sessions", 0))
	if last := plain(f.api.LastText()); strings.Contains(last, "abcdefghij") || !strings.Contains(last, "ops password=•••") {
		t.Fatalf("/sessions = %q", last)
	}
	f.handle(t, f.msg("@ops password=••• run the tests", 0))
	f.typedOnce(t, "A1", " run the tests\n")
}

// Names are matched as the relay's HTML shows them: a value that HTML
// escaping lengthens past the redaction threshold is shown redacted, so
// two names that show alike are told apart, and the shown name addresses
// its own session.
func TestShownNameAfterEscaping(t *testing.T) {
	f := newFixture(t)
	f.label(t, sidShop, "Ops password=ab&cd", "acme-shop-92")
	f.label(t, sidShopF, "Ops password=abcdefghij", "acme-shop-fa")
	f.handle(t, f.msg("/sessions", 0))
	last := plain(f.api.LastText())
	if !strings.Contains(last, "Ops password=••• · 92") || !strings.Contains(last, "Ops password=••• · fa") {
		t.Fatalf("/sessions = %q", last)
	}
	f.handle(t, f.msg("@Ops password=••• · 92 run the tests", 0))
	f.typedOnce(t, "A1", " run the tests\n")
}

// A header copied from a message addresses its own session, not one whose
// name is a prefix of it: the hooks store the label as messages show it.
func TestCopiedHeaderAddressesItsSession(t *testing.T) {
	f := newFixture(t)
	f.label(t, sidShop, render.ShownName("Ops password=ab&cd"), "acme-shop-92")
	f.label(t, sidShopF, "Ops", "acme-shop-fa")
	f.handle(t, f.msg("@"+render.ShownName("Ops password=ab&cd")+" deploy", 0))
	f.typedOnce(t, "A1", " deploy\n")
}

// A record stored by an older tg may hold a tab: the relay shapes its name
// as messages now show it, and an address written with the tab reaches
// that session, never the followed one as plain text.
func TestOldRecordWithTab(t *testing.T) {
	f := newFixture(t)
	f.label(t, sidShop, "Ops\tstaging", "acme-shop-92")
	_, _ = f.store.UpdateSession(sidBlog, func(s *state.Session) { s.Follow = false })
	_, _ = f.store.UpdateSession(sidShopF, func(s *state.Session) { s.Follow = true }) // idle, takes plain text
	f.handle(t, f.msg("Ops\tstaging: deploy", 0))
	f.typedOnce(t, "A1", " deploy\n") // its own session, not the followed one
	f.term.Typed = nil
	f.label(t, sidShopF, "Ops\tstaging", "acme-shop-fa")
	f.handle(t, f.msg("/sessions", 0))
	if last := plain(f.api.LastText()); !strings.Contains(last, "Ops staging · 92") || strings.Contains(last, "\t") {
		t.Fatalf("/sessions = %q", last)
	}
}

// Claude Code's name addresses its session in full, however long: the
// relay shows it cut, but matches it whole, so "name: text" reaches it
// rather than the followed session.
func TestLongClaudeNameAddresses(t *testing.T) {
	long := strings.Repeat("a", 65) + "-92"
	f := newFixture(t)
	f.r.Agents = relaytest.AgentsOf(
		session.Agent{SessionID: sidShop, Name: long, Kind: "interactive", Status: "idle", Cwd: "/Users/alex/code/acme-shop"},
		session.Agent{SessionID: sidShopF, Name: "acme-shop-fa", Kind: "interactive", Status: "idle", Cwd: "/Users/alex/code/acme-shop"},
	)
	f.label(t, sidShop, "Shop", long)
	_, _ = f.store.UpdateSession(sidShopF, func(s *state.Session) { s.Follow = true })
	f.handle(t, f.msg(long+": deploy", 0))
	f.typedOnce(t, "A1", " deploy\n")
}

// A label over the 64 runes a header shows addresses its session in full
// too, in either form, never falling through to the followed session.
func TestLongLabelAddressesInFull(t *testing.T) {
	long := "Ops " + strings.Repeat("x", 70)
	f := newFixture(t)
	_, _ = f.store.UpdateSession(sidShop, func(s *state.Session) {
		s.Name, s.FullLabel, s.ClaudeName = render.ShownName(long), long, "acme-shop-92"
	})
	f.label(t, sidShopF, "Ops", "acme-shop-fa")
	_, _ = f.store.UpdateSession(sidBlog, func(s *state.Session) { s.Follow = false })
	_, _ = f.store.UpdateSession(sidShopF, func(s *state.Session) { s.Follow = true })
	for _, text := range []string{long + ": deploy", "@" + long + " deploy"} {
		f.term.Typed = nil
		f.handle(t, f.msg(text, 0))
		f.typedOnce(t, "A1", " deploy\n")
	}
}

// A record holding a long tab name but no full label (written before
// FullLabel) is addressed by the tab name in full all the same.
func TestLongTabWithoutFullLabel(t *testing.T) {
	long := "Ops " + strings.Repeat("x", 70)
	f := newFixture(t)
	_, _ = f.store.UpdateSession(sidShop, func(s *state.Session) {
		s.Name, s.Tab, s.FullLabel, s.ClaudeName = render.ShownName(long), long, "", "acme-shop-92"
	})
	f.label(t, sidShopF, "Ops", "acme-shop-fa")
	_, _ = f.store.UpdateSession(sidBlog, func(s *state.Session) { s.Follow = false })
	_, _ = f.store.UpdateSession(sidShopF, func(s *state.Session) { s.Follow = true })
	f.handle(t, f.msg(long+": deploy", 0))
	f.typedOnce(t, "A1", " deploy\n")
}
