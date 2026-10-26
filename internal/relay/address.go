// Package relay turns Telegram messages into keystrokes for one Claude Code
// session and answers the bot's slash commands.
package relay

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// ParseCommand splits "/approve@bot  @acme" into ("approve", "@acme").
// Text that is not a command returns ("", text).
func ParseCommand(text string) (cmd, arg string) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", text
	}
	fields := strings.Fields(text[1:])
	if len(fields) == 0 {
		return "", ""
	}
	cmd = fields[0]
	if i := strings.Index(cmd, "@"); i >= 0 {
		cmd = cmd[:i]
	}
	return strings.ToLower(cmd), strings.TrimSpace(strings.Join(fields[1:], " "))
}

// MatchName resolves a typed name against live session names: exact match
// first, then the one match ignoring case, then a unique case-insensitive
// prefix (MatchNamed, each name its own session).
func MatchName(cand string, names []string) (string, bool) {
	return MatchNamed(cand, keyed(names))
}

// Named is a name that addresses a session, and Key the session it
// addresses: a session may have several (its label and Claude Code's name).
type Named struct {
	Name string
	Key  string
}

// keyed names each name its own session.
func keyed(names []string) []Named {
	out := make([]Named, len(names))
	for i, n := range names {
		out[i] = Named{Name: n, Key: n}
	}
	return out
}

// MatchNamed resolves a typed name, a leading "@" dropped, against names
// (namedHits): it reports the one session it names.
func MatchNamed(cand string, names []Named) (key string, ok bool) {
	cand = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cand), "@"))
	if cand == "" {
		return "", false
	}
	if hits := namedHits(cand, names); len(hits) == 1 {
		return hits[0].Key, true
	}
	return "", false
}

// AddrKind classifies how a message addresses a session.
type AddrKind int

// Address kinds.
const (
	AddrNone      AddrKind = iota // plain text, no address
	AddrResolved                  // one session matched
	AddrAmbiguous                 // an address that matches several (or a colon prefix)
	AddrUnknown                   // an explicit @name that matches nothing
)

// Addr is the result of Address: for AddrResolved, the name matched and
// the session it names.
type Addr struct {
	Kind AddrKind
	Name string
	Key  string
	Rest string
}

// Address is AddressNamed with each name its own session.
func Address(text string, names []string) Addr {
	return AddressNamed(text, keyed(names))
}

// AddressNamed recognises "@name text" and "name: text". The longest whole
// name the text starts with, ignoring case and spaces and all ("@Ops
// staging deploy", "Ops staging: deploy"), comes first: a session label
// holds spaces. Otherwise "@word" takes an exact name, then one ignoring
// case, then a unique prefix (namedHits); a colon word that is merely a
// prefix of some session is reported ambiguous rather than guessed, and
// anything else with a colon ("note: ...", "https://...") is plain text.
// The names of one session count as one.
//
// Names are matched on the message with each run of whitespace read as one
// space (session names hold single spaces only), so a tab, a line break or
// two spaces cannot make an address reach a shorter name; the rest keeps
// the message's own whitespace.
func AddressNamed(text string, names []Named) Addr {
	t := strings.TrimSpace(text)
	nt, at := oneSpace(t)
	a := addressNamed(nt, names)
	if a.Kind == AddrNone {
		a.Rest = t
	} else if a.Rest != "" {
		a.Rest = strings.TrimSpace(t[at[len(nt)-len(a.Rest)]:])
	}
	return a
}

// oneSpace returns t, trimmed, with each run of whitespace one space, and
// at, where each byte of it, and its end, lie in t.
func oneSpace(t string) (string, []int) {
	var b strings.Builder
	at := make([]int, 0, len(t)+1)
	space := false
	for i, r := range t {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
			at = append(at, i)
		}
		space = false
		n := b.Len()
		b.WriteRune(r)
		for j := 0; j < b.Len()-n; j++ {
			at = append(at, i+j)
		}
	}
	at = append(at, len(t))
	return b.String(), at
}

// addressNamed is AddressNamed on t, trimmed and with single spaces.
func addressNamed(t string, names []Named) Addr {
	if strings.HasPrefix(t, "@") {
		body := t[1:]
		if hits, rest, n := wholeName(body, names, func(r string) (string, bool) {
			if r == "" {
				return "", true
			}
			if c, _ := utf8.DecodeRuneInString(r); unicode.IsSpace(c) {
				return strings.TrimSpace(r), true
			}
			return "", false
		}); len(hits) > 0 {
			return guarded(body, hits, n+1, rest, names) // the space after the name may agree
		}
		cand, rest := body, ""
		if sp := strings.IndexAny(body, " \n"); sp >= 0 {
			cand, rest = body[:sp], strings.TrimSpace(body[sp+1:])
		}
		cand = strings.TrimSpace(cand)
		if cand == "" {
			return Addr{Kind: AddrNone, Rest: t}
		}
		hits := namedHits(cand, names)
		if len(hits) == 1 {
			rest = nameRest(body, hits[0].Name, utf8.RuneCountInString(cand), rest)
		}
		if len(hits) > 0 {
			return resolved(hits, rest)
		}
		return Addr{Kind: AddrUnknown, Rest: rest}
	}
	if hits, rest, n := wholeName(t, names, func(r string) (string, bool) {
		if after, ok := strings.CutPrefix(r, ":"); ok {
			return strings.TrimSpace(after), true
		}
		return "", false
	}); len(hits) > 0 {
		return guarded(t, hits, n, rest, names) // the colon may be part of a longer name
	}
	i := strings.Index(t, ":")
	if i <= 0 {
		return Addr{Kind: AddrNone, Rest: t}
	}
	if head := strings.Join(strings.Fields(t[:i]), " "); head != t[:i] {
		// Words before a colon that start a name holding spaces ("Ops
		// staging: deploy" for "Ops staging · 92"; any whitespace, as a
		// header an older tg sent may hold a tab) are asked about, not
		// taken for plain text.
		for _, nm := range names {
			if _, ok := cutPrefixFold(nm.Name, head); ok {
				return Addr{Kind: AddrAmbiguous, Rest: strings.TrimSpace(t[i+1:])}
			}
		}
		return Addr{Kind: AddrNone, Rest: t}
	}
	if len(namedHits(t[:i], names)) > 0 {
		return Addr{Kind: AddrAmbiguous, Rest: strings.TrimSpace(t[i+1:])}
	}
	return Addr{Kind: AddrNone, Rest: t}
}

// guarded is resolved for whole-name hits, unless text agrees with
// another session's name for more than upTo runes: the message may then be
// meant for that session, under a name shown longer or cut short ("Ops
// staging · 92" copied as "Ops staging", a header cut at 64 runes), and it
// is ambiguous rather than the shorter one's. For "@name", upTo is the
// name and the one space after it, which any message has ("@Ops deploy"
// beside "Ops staging"), while a copy of the longer name agrees further;
// for "name:", it is the name alone, as the colon may belong to a longer
// name ("Ops:: deploy" beside "Ops: · 92").
func guarded(text string, hits []Named, upTo int, rest string, names []Named) Addr {
	for _, nm := range names {
		if !hasKey(hits, nm.Key) && commonPrefixFold(text, nm.Name) > upTo {
			return Addr{Kind: AddrAmbiguous, Rest: rest}
		}
	}
	return resolved(hits, rest)
}

// nameRest is the message after a name matched by its first word of
// first runes: when body goes on with more of the name, word by word, the
// words it shares whole with the name are dropped too ("deploy" is not
// "deployment"); otherwise rest.
func nameRest(body, name string, first int, rest string) string {
	r, nr := []rune(body), []rune(name)
	cp := commonPrefixFold(body, name)
	cut := first
	for k := cp; k > first; k-- {
		if (k == len(r) || unicode.IsSpace(r[k])) && (k == len(nr) || unicode.IsSpace(nr[k])) {
			cut = k
			break
		}
	}
	if cut == first {
		return rest
	}
	return strings.TrimSpace(string(r[cut:]))
}

// commonPrefixFold is how many runes a and b share from their start,
// ignoring case.
func commonPrefixFold(a, b string) int {
	n := 0
	for a != "" && b != "" {
		ra, sa := utf8.DecodeRuneInString(a)
		rb, sb := utf8.DecodeRuneInString(b)
		if !strings.EqualFold(string(ra), string(rb)) {
			break
		}
		a, b, n = a[sa:], b[sb:], n+1
	}
	return n
}

// resolved is the Addr for hits, one per session: resolved for one,
// ambiguous for more.
func resolved(hits []Named, rest string) Addr {
	if len(hits) == 1 {
		return Addr{Kind: AddrResolved, Name: hits[0].Name, Key: hits[0].Key, Rest: rest}
	}
	return Addr{Kind: AddrAmbiguous, Rest: rest}
}

// wholeName returns the longest names, one per session, that text starts
// with ignoring case and that next accepts what follows (next returns the
// rest of the message), that rest and their length in runes; of those,
// the sessions with a name text starts with exactly when there are any,
// as namedHits prefers an exact match.
func wholeName(text string, names []Named, next func(after string) (string, bool)) (hits []Named, rest string, best int) {
	var longest []Named
	for _, n := range names {
		after, ok := cutPrefixFold(text, n.Name)
		if !ok {
			continue
		}
		r, ok := next(after)
		if !ok {
			continue
		}
		switch l := utf8.RuneCountInString(n.Name); {
		case l > best:
			best, rest, longest = l, r, []Named{n}
		case l == best:
			longest = append(longest, n)
		}
	}
	var exact []Named
	for _, n := range longest {
		if strings.HasPrefix(text, n.Name) && !hasKey(exact, n.Key) {
			exact = append(exact, n)
		}
		if !hasKey(hits, n.Key) {
			hits = append(hits, n)
		}
	}
	if len(exact) > 0 {
		return exact, rest, best
	}
	return hits, rest, best
}

// cutPrefixFold returns s without prefix when s starts with it, ignoring
// case rune by rune; an empty prefix never matches.
func cutPrefixFold(s, prefix string) (string, bool) {
	if prefix == "" {
		return "", false
	}
	for _, p := range prefix {
		r, size := utf8.DecodeRuneInString(s)
		if size == 0 || !strings.EqualFold(string(r), string(p)) {
			return "", false
		}
		s = s[size:]
	}
	return s, true
}

// namedHits returns the names matching cand, one per session: the exact
// matches, else those equal ignoring case ("@acme" is the session "Acme",
// not "acme · Fix…"), else every case-insensitive prefix match.
func namedHits(cand string, names []Named) []Named {
	pick := func(match func(n string) bool) []Named {
		var hits []Named
		for _, n := range names {
			if n.Name != "" && match(n.Name) && !hasKey(hits, n.Key) {
				hits = append(hits, n)
			}
		}
		return hits
	}
	if hits := pick(func(n string) bool { return n == cand }); len(hits) > 0 {
		return hits
	}
	if hits := pick(func(n string) bool { return strings.EqualFold(n, cand) }); len(hits) > 0 {
		return hits
	}
	return pick(func(n string) bool { return strings.HasPrefix(strings.ToLower(n), strings.ToLower(cand)) })
}

// hasKey reports whether hits holds a name of session key.
func hasKey(hits []Named, key string) bool {
	for _, h := range hits {
		if h.Key == key {
			return true
		}
	}
	return false
}
