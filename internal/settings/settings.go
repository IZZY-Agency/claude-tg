// Package settings merges tg's hook groups and allow rules into
// ~/.claude/settings.json without disturbing anything else in the file.
package settings

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// HookSpec is one hook group tg installs.
type HookSpec struct {
	Event   string
	Matcher string // "" means no matcher key
	Command string
	// Async installs the hook with "async": true. A hook Claude Code waits
	// on (the permission wait) is synchronous and carries a Timeout instead.
	Async bool
	// Timeout is the hook's "timeout" in seconds; 0 omits the key.
	Timeout int
	// Replaces lists older hook kinds this spec supersedes. Install migrates
	// such a hook in place instead of adding a second one.
	Replaces []string
}

// permissionWaitTimeout is the PermissionRequest hook timeout: the 12 h wait
// plus a margin.
const permissionWaitTimeout = 43500

// matchesKind reports whether kind is the spec's own kind or one it replaces.
func (h HookSpec) matchesKind(kind string) bool {
	return kind == h.Kind() || contains(h.Replaces, kind)
}

// Kind returns the `tg hook <kind>` suffix of the command.
func (h HookSpec) Kind() string {
	i := strings.LastIndex(h.Command, " hook ")
	if i < 0 {
		return ""
	}
	return h.Command[i+len(" hook "):]
}

// DesiredHooks lists the six hook groups for the given binary path.
func DesiredHooks(binary string) []HookSpec {
	return []HookSpec{
		{Event: "Stop", Command: HookCommand(binary, "stop"), Async: true},
		{Event: "PermissionRequest", Command: HookCommand(binary, "permission-wait"), Timeout: permissionWaitTimeout, Replaces: []string{"permission-request"}},
		{Event: "Notification", Matcher: "permission_prompt", Command: HookCommand(binary, "notification"), Async: true},
		{Event: "Notification", Matcher: "agent_needs_input", Command: HookCommand(binary, "notification"), Async: true},
		{Event: "StopFailure", Command: HookCommand(binary, "stop-failure"), Async: true},
		{Event: "SessionEnd", Command: HookCommand(binary, "session-end"), Async: true},
	}
}

var safeArg = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

// quoteArg single-quotes a path for `sh -c` when it needs it.
func quoteArg(p string) string {
	if safeArg.MatchString(p) {
		return p
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

// HookCommand is the command string installed for one hook kind.
func HookCommand(binary, kind string) string { return quoteArg(binary) + " hook " + kind }

// ParseHookCommand recognises a tg hook command and returns its binary path
// and kind. It understands the single-quoted form quoteArg produces.
func ParseHookCommand(cmd string) (binary, kind string, ok bool) {
	rest := cmd
	if strings.HasPrefix(cmd, "'") {
		var b strings.Builder
		i := 1
		for i < len(cmd) {
			if strings.HasPrefix(cmd[i:], `'\''`) {
				b.WriteByte('\'')
				i += 4
				continue
			}
			if cmd[i] == '\'' {
				i++
				break
			}
			b.WriteByte(cmd[i])
			i++
		}
		binary, rest = b.String(), cmd[i:]
	} else {
		sp := strings.IndexByte(cmd, ' ')
		if sp < 0 {
			return "", "", false
		}
		binary, rest = cmd[:sp], cmd[sp:]
	}
	kind, ok = strings.CutPrefix(rest, " hook ")
	if !ok || binary == "" || !kindRe.MatchString(kind) {
		return "", "", false
	}
	return binary, kind, true
}

var kindRe = regexp.MustCompile(`^[a-z-]+$`)

// AllowRules are the permission rules setup adds so Claude can call tg
// without a prompt. Deliberately not `Bash(tg:*)`: setup and remove still ask.
var AllowRules = []string{"Bash(tg send:*)", "Bash(tg follow:*)", "Bash(tg sessions:*)", "Bash(tg test:*)"}

func objectAt(parent *Object, key string, create bool) (*Object, bool) {
	v, ok := parent.Get(key)
	if ok {
		if o, isObj := v.(*Object); isObj {
			return o, false
		}
	}
	if !create {
		return nil, false
	}
	o := newObject()
	parent.Set(key, o)
	return o, true
}

func arrayAt(parent *Object, key string, create bool) ([]any, bool) {
	v, ok := parent.Get(key)
	if ok {
		if a, isArr := v.([]any); isArr {
			return a, true
		}
	}
	if !create {
		return nil, false
	}
	parent.Set(key, []any{})
	return []any{}, true
}

func groupMatcher(g *Object) string {
	if m, ok := g.Get("matcher"); ok {
		if s, isStr := m.(string); isStr {
			return s
		}
	}
	return ""
}

func groupHooks(g *Object) []*Object {
	var out []*Object
	if hs, ok := g.Get("hooks"); ok {
		if arr, isArr := hs.([]any); isArr {
			for _, h := range arr {
				if ho, isObj := h.(*Object); isObj {
					out = append(out, ho)
				}
			}
		}
	}
	return out
}

func commandOf(h *Object) string {
	if c, ok := h.Get("command"); ok {
		if s, isStr := c.(string); isStr {
			return s
		}
	}
	return ""
}

func newGroup(spec HookSpec) *Object {
	h := newObject()
	h.Set("type", "command")
	h.Set("command", spec.Command)
	if spec.Async {
		h.Set("async", true)
	}
	if spec.Timeout > 0 {
		h.Set("timeout", json.Number(strconv.Itoa(spec.Timeout)))
	}
	g := newObject()
	if spec.Matcher != "" {
		g.Set("matcher", spec.Matcher)
	}
	g.Set("hooks", []any{h})
	return g
}

// conforms reports whether the hook object already carries the spec's
// command and async flag, and its timeout when the spec sets one.
func conforms(h *Object, spec HookSpec) bool {
	if commandOf(h) != spec.Command {
		return false
	}
	async, hasAsync := h.Get("async")
	if spec.Async && async != true || !spec.Async && hasAsync {
		return false
	}
	if spec.Timeout == 0 {
		return true // a user's own timeout on an async hook is theirs
	}
	t, _ := h.Get("timeout")
	n, isNum := t.(json.Number)
	return isNum && n.String() == strconv.Itoa(spec.Timeout)
}

// conform rewrites the hook object to the spec: command, async flag, and the
// timeout when the spec sets one.
func conform(h *Object, spec HookSpec) {
	h.Set("command", spec.Command)
	if spec.Async {
		h.Set("async", true)
	} else {
		h.Delete("async")
	}
	if spec.Timeout > 0 {
		h.Set("timeout", json.Number(strconv.Itoa(spec.Timeout)))
	}
}

// Install adds the desired hook groups and allow rules. A hook that already
// exists for the same event, matcher and kind but points at a different
// binary (a previously recorded path in oldBinaries, or any path whose
// basename is tg) is updated in place.
func Install(src []byte, binary string, oldBinaries ...string) ([]byte, bool, error) {
	root, err := Parse(src)
	if err != nil {
		return nil, false, err
	}
	changed := false
	hooks, created := objectAt(root, "hooks", true)
	changed = changed || created
	for _, spec := range DesiredHooks(binary) {
		groups, _ := arrayAt(hooks, spec.Event, true)
		type cand struct {
			gi   int
			h    *Object
			kind string
		}
		var cands []cand
		for gi, g := range groups {
			go_, ok := g.(*Object)
			if !ok || groupMatcher(go_) != spec.Matcher {
				continue
			}
			for _, h := range groupHooks(go_) {
				hb, hk, ok := ParseHookCommand(commandOf(h))
				if ok && spec.matchesKind(hk) && (hb == binary || isStale(hb, oldBinaries)) {
					cands = append(cands, cand{gi, h, hk})
				}
			}
		}
		found := len(cands) > 0
		if found {
			// Keep the spec's own kind (conforming first), else the first
			// replaced-kind hook; drop the other replaced-kind duplicates.
			keep := 0
			rank := func(c cand, h *Object) int {
				switch {
				case c.kind == spec.Kind() && conforms(h, spec):
					return 0
				case c.kind == spec.Kind():
					return 1
				}
				return 2
			}
			for i, c := range cands {
				if rank(c, c.h) < rank(cands[keep], cands[keep].h) {
					keep = i
				}
			}
			if !conforms(cands[keep].h, spec) {
				conform(cands[keep].h, spec)
				changed = true
			}
			drop := map[*Object]bool{}
			for i, c := range cands {
				if i != keep && c.kind != spec.Kind() {
					drop[c.h] = true
				}
			}
			if len(drop) > 0 {
				var newGroups []any
				for _, g := range groups {
					go_, ok := g.(*Object)
					if !ok {
						newGroups = append(newGroups, g)
						continue
					}
					var keptHooks []any
					removed := false
					for _, h := range groupHooks(go_) {
						if drop[h] {
							removed = true
							continue
						}
						keptHooks = append(keptHooks, h)
					}
					if !removed {
						newGroups = append(newGroups, g)
						continue
					}
					if len(keptHooks) > 0 {
						go_.Set("hooks", keptHooks)
						newGroups = append(newGroups, go_)
					}
				}
				hooks.Set(spec.Event, newGroups)
				changed = true
			}
		}
		if !found {
			groups = append(groups, newGroup(spec))
			hooks.Set(spec.Event, groups)
			changed = true
		}
	}
	perms, created := objectAt(root, "permissions", true)
	changed = changed || created
	allow, _ := arrayAt(perms, "allow", true)
	for _, rule := range AllowRules {
		present := false
		for _, a := range allow {
			if s, ok := a.(string); ok && s == rule {
				present = true
				break
			}
		}
		if !present {
			allow = append(allow, rule)
			changed = true
		}
	}
	perms.Set("allow", allow)
	out, err := Marshal(root, DetectIndent(src), bytes.HasSuffix(src, []byte("\n")) || len(bytes.TrimSpace(src)) == 0)
	return out, changed, err
}

// isStale reports whether a hook installed from binary hb belongs to an
// earlier install: a recorded previous path, or any binary named tg.
func isStale(hb string, oldBinaries []string) bool {
	for _, old := range oldBinaries {
		if old != "" && hb == old {
			return true
		}
	}
	return strings.HasSuffix(hb, "/tg") || hb == "tg"
}

// Remove drops hook entries whose command starts with any prefix and the
// exact allow rules. Groups and events tg never touched are left exactly as
// they were; a group or event is dropped only when removing tg's own entries
// emptied it.
func Remove(src []byte, binaryPrefixes []string) ([]byte, bool, error) {
	root, err := Parse(src)
	if err != nil {
		return nil, false, err
	}
	changed := false
	matches := func(cmd string) bool {
		hb, _, ok := ParseHookCommand(cmd)
		if !ok {
			return false
		}
		for _, p := range binaryPrefixes {
			if p != "" && hb == p {
				return true
			}
		}
		return false
	}
	if hooks, _ := objectAt(root, "hooks", false); hooks != nil {
		hooksTouched := false
		for _, ev := range append([]string(nil), hooks.Keys...) {
			groups, ok := arrayAt(hooks, ev, false)
			if !ok {
				continue
			}
			var kept []any
			touched := false
			for _, g := range groups {
				go_, isObj := g.(*Object)
				if !isObj {
					kept = append(kept, g)
					continue
				}
				all := groupHooks(go_)
				var keptHooks []any
				removed := 0
				for _, h := range all {
					if matches(commandOf(h)) {
						removed++
						continue
					}
					keptHooks = append(keptHooks, h)
				}
				if removed == 0 {
					kept = append(kept, g) // untouched, byte-for-byte
					continue
				}
				touched, changed = true, true
				if len(keptHooks) == 0 {
					continue // we emptied it
				}
				go_.Set("hooks", keptHooks)
				kept = append(kept, go_)
			}
			if !touched {
				continue
			}
			hooksTouched = true
			if len(kept) == 0 {
				hooks.Delete(ev)
			} else {
				hooks.Set(ev, kept)
			}
		}
		if hooksTouched && len(hooks.Keys) == 0 {
			root.Delete("hooks")
		}
	}
	if perms, _ := objectAt(root, "permissions", false); perms != nil {
		if allow, ok := arrayAt(perms, "allow", false); ok {
			var kept []any
			for _, a := range allow {
				if s, isStr := a.(string); isStr && contains(AllowRules, s) {
					changed = true
					continue
				}
				kept = append(kept, a)
			}
			if kept == nil {
				kept = []any{}
			}
			perms.Set("allow", kept)
		}
	}
	out, err := Marshal(root, DetectIndent(src), bytes.HasSuffix(src, []byte("\n")))
	return out, changed, err
}

// Status reports which desired hooks and rules are present. A desired hook
// that exists for this binary but carries an older kind or settings (for
// example the async permission-request hook of v0.2.0) is listed in outdated,
// not missing; `tg setup` migrates it in place.
func Status(src []byte, binary string) (installed, missing, outdated []HookSpec, missingRules []string, err error) {
	root, err := Parse(src)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	hooks, _ := objectAt(root, "hooks", false)
	for _, spec := range DesiredHooks(binary) {
		var current, stale bool
		if hooks != nil {
			groups, _ := arrayAt(hooks, spec.Event, false)
			for _, g := range groups {
				go_, isObj := g.(*Object)
				if !isObj || groupMatcher(go_) != spec.Matcher {
					continue
				}
				for _, h := range groupHooks(go_) {
					hb, hk, ok := ParseHookCommand(commandOf(h))
					if !ok || hb != binary || !spec.matchesKind(hk) {
						continue
					}
					if conforms(h, spec) {
						current = true
					} else {
						stale = true
					}
				}
			}
		}
		switch {
		case current:
			installed = append(installed, spec)
		case stale:
			outdated = append(outdated, spec)
		default:
			missing = append(missing, spec)
		}
	}
	var allow []any
	if perms, _ := objectAt(root, "permissions", false); perms != nil {
		allow, _ = arrayAt(perms, "allow", false)
	}
	for _, rule := range AllowRules {
		present := false
		for _, a := range allow {
			if s, ok := a.(string); ok && s == rule {
				present = true
			}
		}
		if !present {
			missingRules = append(missingRules, rule)
		}
	}
	return installed, missing, outdated, missingRules, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
