// Package relay turns Telegram messages into keystrokes for one Claude Code
// session and answers the bot's slash commands.
package relay

import (
	"strings"
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
// first, then a unique case-insensitive prefix.
func MatchName(cand string, names []string) (string, bool) {
	cand = strings.TrimPrefix(strings.TrimSpace(cand), "@")
	if cand == "" {
		return "", false
	}
	for _, n := range names {
		if n == cand {
			return n, true
		}
	}
	var hits []string
	for _, n := range names {
		if strings.HasPrefix(strings.ToLower(n), strings.ToLower(cand)) {
			hits = append(hits, n)
		}
	}
	if len(hits) == 1 {
		return hits[0], true
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

// Addr is the result of Address.
type Addr struct {
	Kind AddrKind
	Name string
	Rest string
}

// Address recognises "@name text" (exact or unique prefix) and "name: text"
// (exact name only). A colon word that is merely a prefix of some session
// is reported ambiguous rather than guessed, and anything else with a colon
// ("note: ...", "https://...") is plain text.
func Address(text string, names []string) Addr {
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, "@") {
		cand, rest := t[1:], ""
		if sp := strings.IndexAny(t, " \n"); sp >= 0 {
			cand, rest = t[1:sp], strings.TrimSpace(t[sp+1:])
		}
		cand = strings.TrimSpace(cand)
		if cand == "" {
			return Addr{Kind: AddrNone, Rest: t}
		}
		switch hits := prefixHits(cand, names); {
		case len(hits) == 1:
			return Addr{Kind: AddrResolved, Name: hits[0], Rest: rest}
		case len(hits) > 1:
			return Addr{Kind: AddrAmbiguous, Rest: rest}
		}
		return Addr{Kind: AddrUnknown, Rest: rest}
	}
	i := strings.Index(t, ":")
	if i <= 0 || strings.ContainsAny(t[:i], " \n\t") {
		return Addr{Kind: AddrNone, Rest: t}
	}
	cand, rest := t[:i], strings.TrimSpace(t[i+1:])
	for _, n := range names {
		if n == cand {
			return Addr{Kind: AddrResolved, Name: n, Rest: rest}
		}
	}
	if len(prefixHits(cand, names)) > 0 {
		return Addr{Kind: AddrAmbiguous, Rest: rest}
	}
	return Addr{Kind: AddrNone, Rest: t}
}

// prefixHits returns the exact match alone, or every case-insensitive prefix match.
func prefixHits(cand string, names []string) []string {
	for _, n := range names {
		if n == cand {
			return []string{n}
		}
	}
	var hits []string
	for _, n := range names {
		if strings.HasPrefix(strings.ToLower(n), strings.ToLower(cand)) {
			hits = append(hits, n)
		}
	}
	return hits
}
