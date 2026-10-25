package settings

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const bin = "/Users/alex/.local/bin/tg"

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/settings.before.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// generic view for assertions only
func view(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, b)
	}
	return m
}

func hookGroups(m map[string]any, event string) []any {
	h, _ := m["hooks"].(map[string]any)
	g, _ := h[event].([]any)
	return g
}

func commands(groups []any) []string {
	var out []string
	for _, g := range groups {
		gm := g.(map[string]any)
		for _, h := range gm["hooks"].([]any) {
			out = append(out, h.(map[string]any)["command"].(string))
		}
	}
	return out
}

func TestInstallAddsSixGroupsAndFourRules(t *testing.T) {
	src := fixture(t)
	before := view(t, src)
	out, changed, err := Install(src, bin)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	after := view(t, out)
	total := 0
	for _, ev := range []string{"Stop", "PermissionRequest", "Notification", "StopFailure", "SessionEnd"} {
		for _, c := range commands(hookGroups(after, ev)) {
			if strings.HasPrefix(c, bin+" hook ") {
				total++
			}
		}
	}
	if total != 6 {
		t.Fatalf("installed %d tg hooks, want 6", total)
	}
	// Existing groups survive, in order, first.
	for _, ev := range []string{"Notification", "PostToolUse", "PreToolUse", "Stop", "UserPromptSubmit"} {
		b := commands(hookGroups(before, ev))
		a := commands(hookGroups(after, ev))
		if len(a) < len(b) {
			t.Fatalf("%s lost hooks: before %v after %v", ev, b, a)
		}
		for i := range b {
			if a[i] != b[i] {
				t.Fatalf("%s reordered: before %v after %v", ev, b, a)
			}
		}
	}
	// Existing allow rules survive, in order, first; tg's four follow.
	allowBefore := before["permissions"].(map[string]any)["allow"].([]any)
	allow := after["permissions"].(map[string]any)["allow"].([]any)
	if len(allowBefore) == 0 || len(allow) != len(allowBefore)+4 {
		t.Fatalf("allow=%v (before %v)", allow, allowBefore)
	}
	for i, want := range allowBefore {
		if allow[i] != want {
			t.Fatalf("allow[%d]=%v want existing rule %s", i, allow[i], want)
		}
	}
	for i, want := range AllowRules {
		if allow[len(allowBefore)+i] != want {
			t.Fatalf("allow[%d]=%v want %s", len(allowBefore)+i, allow[len(allowBefore)+i], want)
		}
	}
	deny := after["permissions"].(map[string]any)["deny"].([]any)
	if len(deny) != len(before["permissions"].(map[string]any)["deny"].([]any)) {
		t.Fatal("deny list changed")
	}
	// Top-level keys preserved in order.
	bk, ak := topKeys(t, src), topKeys(t, out)
	if strings.Join(bk, ",") != strings.Join(ak, ",") {
		t.Fatalf("top-level keys changed: %v -> %v", bk, ak)
	}
}

func topKeys(t *testing.T, b []byte) []string {
	t.Helper()
	o, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return o.Keys
}

func TestInstallIdempotent(t *testing.T) {
	out1, _, err := Install(fixture(t), bin)
	if err != nil {
		t.Fatal(err)
	}
	out2, changed, err := Install(out1, bin)
	if err != nil || changed {
		t.Fatalf("second install changed=%v err=%v", changed, err)
	}
	if !bytes.Equal(out1, out2) {
		t.Fatalf("second install altered bytes:\n%s\n---\n%s", out1, out2)
	}
}

func TestRemoveRestoresOriginal(t *testing.T) {
	src := fixture(t)
	norm, _, err := Install(src, "/elsewhere/tg")
	if err != nil {
		t.Fatal(err)
	}
	out, changed, err := Remove(norm, []string{"/elsewhere/tg"})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	// Compare against the original normalised through the same writer.
	o, _ := Parse(src)
	want, _ := Marshal(o, DetectIndent(src), bytes.HasSuffix(src, []byte("\n")))
	if !bytes.Equal(out, want) {
		t.Fatalf("remove did not restore the original:\n%s\n---want---\n%s", out, want)
	}
}

func TestRoundTripPreservesNumbersAndStrings(t *testing.T) {
	src := []byte("{\n  \"a\": 1e3,\n  \"b\": \"<x>&\",\n  \"c\": {\n    \"z\": 1,\n    \"a\": 2.50\n  },\n  \"d\": [\n    true,\n    null,\n    \"é\"\n  ]\n}\n")
	o, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Marshal(o, "  ", true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, src) {
		t.Fatalf("round trip differs:\n%s\n---\n%s", src, out)
	}
}

func TestStalePathReplaced(t *testing.T) {
	out, _, err := Install(fixture(t), "/old/tg")
	if err != nil {
		t.Fatal(err)
	}
	out2, changed, err := Install(out, "/new/tg")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	after := view(t, out2)
	old, nw := 0, 0
	for _, ev := range []string{"Stop", "PermissionRequest", "Notification", "StopFailure", "SessionEnd"} {
		for _, c := range commands(hookGroups(after, ev)) {
			if strings.HasPrefix(c, "/old/tg ") {
				old++
			}
			if strings.HasPrefix(c, "/new/tg ") {
				nw++
			}
		}
	}
	if old != 0 || nw != 6 {
		t.Fatalf("old=%d new=%d", old, nw)
	}
}

func TestInstallOnEmptyFile(t *testing.T) {
	for _, src := range [][]byte{[]byte("{}"), []byte(""), []byte("{}\n")} {
		out, changed, err := Install(src, bin)
		if err != nil || !changed {
			t.Fatalf("src=%q changed=%v err=%v", src, changed, err)
		}
		m := view(t, out)
		if len(hookGroups(m, "Stop")) != 1 || len(m["permissions"].(map[string]any)["allow"].([]any)) != 4 {
			t.Fatalf("src=%q out=%s", src, out)
		}
		back, changed, err := Remove(out, []string{bin})
		if err != nil || !changed {
			t.Fatalf("remove: changed=%v err=%v", changed, err)
		}
		if m2 := view(t, back); len(m2) != 1 || len(m2["permissions"].(map[string]any)["allow"].([]any)) != 0 {
			t.Fatalf("after remove: %s", back)
		}
	}
}

func TestDetectIndent(t *testing.T) {
	if DetectIndent([]byte("{\n    \"a\": 1\n}")) != "    " {
		t.Fatal("4 spaces not detected")
	}
	if DetectIndent([]byte("{\n\t\"a\": 1\n}")) != "\t" {
		t.Fatal("tab not detected")
	}
	if DetectIndent([]byte("{}")) != "  " {
		t.Fatal("default must be two spaces")
	}
}

func TestStatusReportsMissing(t *testing.T) {
	installed, missing, _, missingRules, err := Status(fixture(t), bin)
	if err != nil || len(installed) != 0 || len(missing) != 6 || len(missingRules) != 4 {
		t.Fatalf("installed=%d missing=%d rules=%d err=%v", len(installed), len(missing), len(missingRules), err)
	}
	out, _, _ := Install(fixture(t), bin)
	installed, missing, _, missingRules, _ = Status(out, bin)
	if len(installed) != 6 || len(missing) != 0 || len(missingRules) != 0 {
		t.Fatalf("after install: installed=%d missing=%d rules=%d", len(installed), len(missing), len(missingRules))
	}
}

// Duplicate keys would be silently dropped on rewrite; refuse instead.
func TestParseRejectsDuplicateKeys(t *testing.T) {
	if _, err := Parse([]byte(`{"hooks": {}, "a": 1, "hooks": {"Stop": []}}`)); err == nil {
		t.Fatal("want an error for duplicate keys")
	}
}

// Remove must leave groups it never touched alone, even empty ones.
func TestRemoveLeavesUnrelatedEmptyGroups(t *testing.T) {
	src := []byte("{\n  \"permissions\": {\n    \"allow\": []\n  },\n  \"hooks\": {\n    \"PreToolUse\": [\n      {\n        \"matcher\": \"Bash\",\n        \"hooks\": []\n      }\n    ]\n  }\n}\n")
	installed, _, err := Install(src, bin)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := Remove(installed, []string{bin})
	if err != nil {
		t.Fatal(err)
	}
	o, _ := Parse(src)
	want, _ := Marshal(o, "  ", true)
	if !bytes.Equal(out, want) {
		t.Fatalf("remove altered untouched groups:\n%s\n---want---\n%s", out, want)
	}
}

// A renamed binary must be recognised through its recorded old path.
func TestInstallReplacesOldBinaryPrefix(t *testing.T) {
	out1, _, err := Install(fixture(t), "/x/tg-v1")
	if err != nil {
		t.Fatal(err)
	}
	out2, changed, err := Install(out1, "/x/tg-v2", "/x/tg-v1")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	after := view(t, out2)
	old, nw := 0, 0
	for _, ev := range []string{"Stop", "PermissionRequest", "Notification", "StopFailure", "SessionEnd"} {
		for _, c := range commands(hookGroups(after, ev)) {
			if strings.HasPrefix(c, "/x/tg-v1 ") {
				old++
			}
			if strings.HasPrefix(c, "/x/tg-v2 ") {
				nw++
			}
		}
	}
	if old != 0 || nw != 6 {
		t.Fatalf("old=%d new=%d", old, nw)
	}
}

// Minor from review: a binary path with spaces must be shell-quoted and still
// be recognised by Status and Remove.
func TestBinaryPathWithSpacesIsQuoted(t *testing.T) {
	spaced := "/Users/x/My Tools/tg"
	out, _, err := Install([]byte("{}"), spaced)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `'/Users/x/My Tools/tg' hook stop`) {
		t.Fatalf("command not quoted:\n%s", out)
	}
	installed, missing, _, _, _ := Status(out, spaced)
	if len(installed) != 6 || len(missing) != 0 {
		t.Fatalf("status installed=%d missing=%d", len(installed), len(missing))
	}
	out2, changed, _ := Install(out, spaced)
	if changed || !bytes.Equal(out, out2) {
		t.Fatal("quoted install is not idempotent")
	}
	back, changed, err := Remove(out, []string{spaced})
	if err != nil || !changed || strings.Contains(string(back), "hook stop") {
		t.Fatalf("remove: changed=%v err=%v\n%s", changed, err, back)
	}
	moved, changed, _ := Install(out, "/Users/x/bin/tg", spaced)
	if !changed || strings.Contains(string(moved), "My Tools") {
		t.Fatalf("stale quoted path not replaced:\n%s", moved)
	}
}

// v0.2.0 installed every hook async, PermissionRequest included, and ran
// `hook permission-request`.
const v020Settings = `{
  "hooks": {
    "PermissionRequest": [
      {"hooks": [{"type": "command", "command": "/usr/bin/my-gate"}]},
      {"hooks": [{"type": "command", "command": "/Users/alex/.local/bin/tg hook permission-request", "async": true}]}
    ],
    "Stop": [
      {"hooks": [{"type": "command", "command": "/Users/alex/.local/bin/tg hook stop", "async": true}]}
    ]
  }
}
`

func permissionHooks(t *testing.T, out []byte) []map[string]any {
	t.Helper()
	var hs []map[string]any
	for _, g := range hookGroups(view(t, out), "PermissionRequest") {
		for _, h := range g.(map[string]any)["hooks"].([]any) {
			hs = append(hs, h.(map[string]any))
		}
	}
	return hs
}

func TestInstallFreshPermissionWait(t *testing.T) {
	out, _, err := Install(fixture(t), bin)
	if err != nil {
		t.Fatal(err)
	}
	var tgHooks []map[string]any
	for _, h := range permissionHooks(t, out) {
		if strings.HasPrefix(h["command"].(string), bin+" hook ") {
			tgHooks = append(tgHooks, h)
		}
	}
	if len(tgHooks) != 1 {
		t.Fatalf("tg PermissionRequest hooks = %d", len(tgHooks))
	}
	h := tgHooks[0]
	if _, has := h["async"]; has {
		t.Fatalf("async key present: %v", h)
	}
	if h["timeout"] != float64(43500) {
		t.Fatalf("timeout = %v", h["timeout"])
	}
	if !strings.HasSuffix(h["command"].(string), " hook permission-wait") {
		t.Fatalf("command = %v", h["command"])
	}
	// Every other tg hook stays async with no timeout.
	for _, ev := range []string{"Stop", "Notification", "StopFailure", "SessionEnd"} {
		for _, g := range hookGroups(view(t, out), ev) {
			for _, h := range g.(map[string]any)["hooks"].([]any) {
				hm := h.(map[string]any)
				if !strings.HasPrefix(hm["command"].(string), bin+" hook ") {
					continue
				}
				if hm["async"] != true || hm["timeout"] != nil {
					t.Fatalf("%s hook: %v", ev, hm)
				}
			}
		}
	}
}

func TestInstallMigratesAsyncPermissionRequest(t *testing.T) {
	out, changed, err := Install([]byte(v020Settings), bin)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	hs := permissionHooks(t, out)
	if len(hs) != 2 {
		t.Fatalf("PermissionRequest hooks = %d, want 2 (user's + tg's): %v", len(hs), hs)
	}
	if hs[0]["command"] != "/usr/bin/my-gate" {
		t.Fatalf("user hook touched: %v", hs[0])
	}
	h := hs[1]
	if h["command"] != bin+" hook permission-wait" || h["timeout"] != float64(43500) {
		t.Fatalf("not migrated: %v", h)
	}
	if _, has := h["async"]; has {
		t.Fatalf("async not removed: %v", h)
	}
	if n := len(hookGroups(view(t, out), "PermissionRequest")); n != 2 {
		t.Fatalf("groups = %d, want 2", n)
	}
	again, changed, err := Install(out, bin)
	if err != nil || changed || !bytes.Equal(again, out) {
		t.Fatalf("second install not idempotent: changed=%v err=%v", changed, err)
	}
}

func TestStatusOldPermissionHookIsMissing(t *testing.T) {
	_, missing, outdated, _, err := Status([]byte(v020Settings), bin)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range outdated {
		if m.Event == "PermissionRequest" {
			found = true
		}
	}
	for _, m := range missing {
		if m.Event == "PermissionRequest" {
			t.Fatalf("outdated hook also listed missing: %v", missing)
		}
	}
	if !found {
		t.Fatalf("v0.2.0 PermissionRequest hook not reported outdated; outdated=%v", outdated)
	}
	out, _, _ := Install([]byte(v020Settings), bin)
	_, missing, outdated, _, _ = Status(out, bin)
	for _, m := range append(missing, outdated...) {
		if m.Event == "PermissionRequest" {
			t.Fatal("migrated hook still missing or outdated")
		}
	}
}

func TestRemoveDropsPermissionWait(t *testing.T) {
	out, _, _ := Install([]byte(v020Settings), bin)
	out, changed, err := Remove(out, []string{bin})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	for _, h := range permissionHooks(t, out) {
		if strings.Contains(h["command"].(string), "permission-wait") {
			t.Fatalf("permission-wait left behind: %v", h)
		}
	}
	if hs := permissionHooks(t, out); len(hs) != 1 || hs[0]["command"] != "/usr/bin/my-gate" {
		t.Fatalf("user hook lost: %v", hs)
	}
}

func TestInstallLeavesUserTimeoutOnAsyncHooks(t *testing.T) {
	src := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"` + bin + ` hook stop","async":true,"timeout":30}]}]}}`
	out, _, err := Install([]byte(src), bin)
	if err != nil {
		t.Fatal(err)
	}
	g := hookGroups(view(t, out), "Stop")
	if len(g) != 1 {
		t.Fatalf("Stop groups = %d", len(g))
	}
	h := g[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if h["timeout"] != float64(30) {
		t.Fatalf("user timeout changed: %v", h)
	}
	installed, _, outdated, _, _ := Status([]byte(src), bin)
	if len(outdated) != 0 {
		t.Fatalf("outdated = %v", outdated)
	}
	has := false
	for _, s := range installed {
		has = has || s.Event == "Stop"
	}
	if !has {
		t.Fatalf("Stop with user timeout not counted installed: %v", installed)
	}
}

func TestInstallDropsExtraReplacedKind(t *testing.T) {
	src := `{"hooks":{"PermissionRequest":[
	  {"hooks":[{"type":"command","command":"` + bin + ` hook permission-request","async":true}]},
	  {"hooks":[{"type":"command","command":"` + bin + ` hook permission-wait","timeout":43500}]},
	  {"hooks":[{"type":"command","command":"/usr/bin/my-gate"}]}
	]}}`
	out, changed, err := Install([]byte(src), bin)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	var cmds []string
	for _, h := range permissionHooks(t, out) {
		cmds = append(cmds, h["command"].(string))
	}
	want := []string{bin + " hook permission-wait", "/usr/bin/my-gate"}
	if strings.Join(cmds, "|") != strings.Join(want, "|") {
		t.Fatalf("commands = %v, want %v", cmds, want)
	}
	if n := len(hookGroups(view(t, out), "PermissionRequest")); n != 2 {
		t.Fatalf("groups = %d, want 2 (emptied group dropped)", n)
	}
	again, changed, _ := Install(out, bin)
	if changed || !bytes.Equal(again, out) {
		t.Fatal("not idempotent")
	}
}

func TestInstallMigratesOldBinaryPermissionRequest(t *testing.T) {
	const old = "/opt/old/claude-tg"
	src := `{"hooks":{"PermissionRequest":[{"hooks":[{"type":"command","command":"` + old + ` hook permission-request","async":true}]}]}}`
	out, changed, err := Install([]byte(src), bin, old)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	hs := permissionHooks(t, out)
	if len(hs) != 1 {
		t.Fatalf("hooks = %v", hs)
	}
	h := hs[0]
	if h["command"] != bin+" hook permission-wait" || h["timeout"] != float64(43500) {
		t.Fatalf("not migrated: %v", h)
	}
	if _, has := h["async"]; has {
		t.Fatalf("async kept: %v", h)
	}
}
