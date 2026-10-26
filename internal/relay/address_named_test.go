package relay

import (
	"strings"
	"testing"
)

// Addressing weighs every name of every session at once, grouped by
// session: an exact name before a prefix, ignoring case after an exact
// match with it, and the longest whole name the text starts with, spaces
// and all, before a single word (labels hold spaces: "Ops staging",
// "acme-shop · Fix the checkout bug").
func TestAddressNamed(t *testing.T) {
	names := []Named{
		{"nova-ca work", "A"}, {"nova-aa", "A"},
		{"Nova", "B"}, {"nova-ca", "B"},
		{"Ops", "C"}, {"ops-11", "C"},
		{"Ops staging", "D"}, {"ops-22", "D"},
		{"voice-test-2", "E"}, {"voicetest2-5b", "E"},
		{"blog-6a", "F"}, {"blog-6a", "G"},
		{"NOVA-CA", "H"}, {"other-77", "H"},
	}
	for _, c := range []struct {
		text string
		kind AddrKind
		key  string
		rest string
	}{
		{"@nova-ca deploy", AddrResolved, "B", "deploy"},      // an exact name beats another session's label prefix
		{"@nova-ca work deploy", AddrResolved, "A", "deploy"}, // the longest whole name
		{"@nova deploy", AddrResolved, "B", "deploy"},         // exact, ignoring case
		{"@Ops staging deploy", AddrResolved, "D", "deploy"},  // a whole label with a space
		{"@Ops deploy", AddrResolved, "C", "deploy"},          // the shorter one when the text says no more
		{"Ops staging: deploy", AddrResolved, "D", "deploy"},  // name: with a space in the name
		{"ops: deploy", AddrResolved, "C", "deploy"},          // name: ignoring case
		{"@voice hi", AddrResolved, "E", "hi"},                // two names of one session are one session
		{"@blog-6a hi", AddrAmbiguous, "", "hi"},              // a label equal to another session's Claude name
		{"@nov hi", AddrAmbiguous, "", "hi"},                  // a prefix of two sessions
		{"@nothing hi", AddrUnknown, "", "hi"},
		{"@NOVA-CA deploy", AddrResolved, "H", "deploy"}, // a whole name matching exactly beats one ignoring case
		{"NOVA-CA: deploy", AddrResolved, "H", "deploy"},
		{"@nova-ca deploy", AddrResolved, "B", "deploy"},
		{"@NOVA-Ca deploy", AddrAmbiguous, "", "deploy"}, // neither exactly: two sessions
		{"note: remember this", AddrNone, "", "note: remember this"},
		{"plain text", AddrNone, "", "plain text"},
	} {
		t.Run(c.text, func(t *testing.T) {
			a := AddressNamed(c.text, names)
			if a.Kind != c.kind || a.Key != c.key || a.Rest != c.rest {
				t.Fatalf("AddressNamed(%q) = %+v; want kind %v key %q rest %q", c.text, a, c.kind, c.key, c.rest)
			}
		})
	}
}

// A command's argument is matched the same way, as a whole.
func TestMatchNamed(t *testing.T) {
	names := []Named{{"Ops", "C"}, {"Ops staging", "D"}, {"nova-ca work", "A"}, {"nova-ca", "B"}, {"voice-test-2", "E"}, {"voicetest2-5b", "E"}}
	for cand, want := range map[string]string{
		"@Ops staging": "D",
		"ops":          "C",
		"nova-ca":      "B",
		"@voice":       "E",
		"nova":         "", // a prefix of two sessions
		"":             "",
	} {
		key, ok := MatchNamed(cand, names)
		if key != want || ok != (want != "") {
			t.Errorf("MatchNamed(%q) = %q, %v; want %q", cand, key, ok, want)
		}
	}
}

// Decorated duplicates are unique: when the end of Claude Code's name does
// not tell two sessions apart, the session id does.
func TestDistinguishUnique(t *testing.T) {
	live := []Live{
		{Name: "Nova", Claude: "project-a-92", SessionID: "aaaa1111-0000"},
		{Name: "Nova", Claude: "project-b-92", SessionID: "bbbb2222-0000"},
		{Name: "Nova · 92", Claude: "other-01", SessionID: "cccc3333-0000"},
		{Name: "Solo", Claude: "solo-01", SessionID: "dddd4444-0000"},
	}
	distinguish(live)
	seen := map[string]bool{}
	for _, l := range live {
		if seen[l.Name] {
			t.Fatalf("names not unique: %+v", live)
		}
		seen[l.Name] = true
	}
	if live[3].Name != "Solo" || live[2].Name != "Nova · 92" {
		t.Fatalf("a unique name changed: %+v", live)
	}
}

// Even when every suffix is taken, two sessions alike end up with names of
// their own.
func TestDistinguishExhausted(t *testing.T) {
	a := Live{Name: "Nova", Claude: "x-92", SessionID: "aaaa1111bbbb"}
	b := Live{Name: "Nova", Claude: "y-92", SessionID: "aaaa1111cccc"}
	live := []Live{a, b}
	for _, s := range []string{"92", "aaaa", "aaaa1111", "aaaa1111bbbb", "aaaa1111cccc"} {
		live = append(live, Live{Name: "Nova · " + s, SessionID: "z" + s})
	}
	distinguish(live)
	seen := map[string]bool{}
	for _, l := range live {
		if seen[strings.ToLower(l.Name)] {
			t.Fatalf("names not unique: %+v", live)
		}
		seen[strings.ToLower(l.Name)] = true
	}
}

// A session matched both ignoring case and exactly counts as exact: with
// another session exact too, the address is ambiguous, never the other's.
func TestWholeNameExactPerSession(t *testing.T) {
	names := []Named{{"NOVA-CA", "A"}, {"Other", "B"}, {"nova-ca", "A"}, {"nova-ca", "B"}}
	if a := AddressNamed("@nova-ca deploy", names); a.Kind != AddrAmbiguous {
		t.Fatalf("AddressNamed = %+v, want ambiguous", a)
	}
}

// A message that keeps agreeing with another session's longer name past
// the name it matched (and, for "@name", the space after it) is
// ambiguous, never the shorter session's: a header copied from a long or a
// decorated name does not reach a session named with its start.
func TestLongerNameGuard(t *testing.T) {
	long := "Ops " + strings.Repeat("x", 65)
	names := []Named{{"Ops", "B"}, {"Ops staging · 92", "A"}, {"Ops staging · fa", "C"}, {long, "D"}}
	for _, c := range []struct {
		text string
		kind AddrKind
		key  string
	}{
		{"@Ops staging deploy", AddrAmbiguous, ""},
		{"Ops staging: deploy", AddrAmbiguous, ""},
		{"@Ops deploy", AddrResolved, "B"},          // agrees with "Ops staging" only through the space
		{"@Ops start the build", AddrAmbiguous, ""}, // agrees with "Ops staging" for "Ops st": asked, not guessed
		{"@" + string([]rune(long)[:63]) + "… deploy", AddrAmbiguous, ""},
		{"@" + long + " deploy", AddrResolved, "D"},
	} {
		if a := AddressNamed(c.text, names); a.Kind != c.kind || a.Key != c.key {
			t.Errorf("AddressNamed(%.40q) = %+v; want kind %v key %q", c.text, a, c.kind, c.key)
		}
	}
}

// A name matched by its first word takes with it the rest of the name the
// message goes on with, so only the message is typed.
func TestFirstWordTakesTheNamesRest(t *testing.T) {
	names := []Named{{"Ops staging · 92", "A"}, {"blog-6a", "F"}}
	if a := AddressNamed("@Ops staging deploy now", names); a.Kind != AddrResolved || a.Key != "A" || a.Rest != "deploy now" {
		t.Fatalf("AddressNamed = %+v", a)
	}
	if a := AddressNamed("@blog run it", names); a.Kind != AddrResolved || a.Key != "F" || a.Rest != "run it" {
		t.Fatalf("AddressNamed = %+v", a)
	}
}

// The colon form is guarded the same way: "Ops:: deploy" copied from a
// duplicate shown as "Ops: · 92" does not reach the session "Ops".
func TestColonNameGuard(t *testing.T) {
	names := []Named{{"Ops", "A"}, {"Ops: · 92", "B"}, {"Ops: · fa", "C"}}
	if a := AddressNamed("Ops:: deploy", names); a.Kind != AddrAmbiguous {
		t.Fatalf("AddressNamed = %+v, want ambiguous", a)
	}
	if a := AddressNamed("@Ops: deploy", names); a.Kind != AddrAmbiguous {
		t.Fatalf("AddressNamed = %+v, want ambiguous", a)
	}
}

// Only whole words of the name are dropped from the message: "deploy" is
// not "deployment".
func TestNameRestWholeWords(t *testing.T) {
	names := []Named{{"Ops deployment", "A"}}
	if a := AddressNamed("@Ops deploy production", names); a.Kind != AddrResolved || a.Rest != "deploy production" {
		t.Fatalf("AddressNamed = %+v", a)
	}
	if a := AddressNamed("@Ops deployment now", names); a.Kind != AddrResolved || a.Rest != "now" {
		t.Fatalf("AddressNamed = %+v", a)
	}
}

// Names are matched on the message with its whitespace runs read as one
// space, so a tab, a line break or two spaces in an address cannot reach a
// shorter name; the message typed keeps its own whitespace.
func TestAddressWhitespace(t *testing.T) {
	names := []Named{{"Ops staging", "A"}, {"Ops", "B"}}
	for _, c := range []struct{ text, key, rest string }{
		{"@Ops\tstaging deploy", "A", "deploy"},
		{"@Ops\nstaging deploy", "A", "deploy"},
		{"@Ops  staging deploy", "A", "deploy"},
		{"Ops\tstaging: deploy", "A", "deploy"},
		{"@Ops staging run   this\tnow", "A", "run   this\tnow"},
	} {
		if a := AddressNamed(c.text, names); a.Kind != AddrResolved || a.Key != c.key || a.Rest != c.rest {
			t.Errorf("AddressNamed(%q) = %+v; want %s %q", c.text, a, c.key, c.rest)
		}
	}
}

// A name matched by its first word is guarded too: a message going on like
// another session's longer name (typed in full past the 64 runes shown) is
// asked about, not the shorter session's.
func TestFirstWordGuard(t *testing.T) {
	long := "Ops " + strings.Repeat("x", 70)
	names := []Named{{"Ops", "B"}, {string([]rune(long)[:63]) + "…", "A"}}
	if a := AddressNamed("@"+long+" deploy", names); a.Kind != AddrAmbiguous {
		t.Fatalf("AddressNamed = %+v, want ambiguous", a)
	}
}
