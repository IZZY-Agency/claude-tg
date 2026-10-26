package relay

import (
	"strings"
	"testing"
)

// mustUnit renders a unit from values that must be acceptable.
func mustUnit(t *testing.T, binary, stateDir, home string, extra map[string]string) string {
	t.Helper()
	u, err := Unit(binary, stateDir, home, extra)
	if err != nil {
		t.Fatalf("Unit: %v", err)
	}
	return u
}

func TestUnitContainsCommandEnvAndLog(t *testing.T) {
	u := mustUnit(t, "/home/x/.local/bin/tg", "/home/x/.local/state/claude-tg", "/home/x", map[string]string{
		"TG_STATE_DIR": "/home/x/.local/state/claude-tg",
		"TG_CONFIG":    "/home/x/.config/claude-tg/config.json",
	})
	for _, want := range []string{
		"[Unit]\nDescription=claude-tg Telegram relay\n",
		"[Service]\n",
		"ExecStart=/home/x/.local/bin/tg relay\n",
		"Restart=always\n",
		"RestartSec=10\n",
		// systemd signals only the relay, which ends its voice command itself
		"KillMode=mixed\n",
		"Environment=PATH=/home/x/.local/bin:/opt/homebrew/bin:/home/linuxbrew/.linuxbrew/bin:/usr/local/bin:/usr/bin:/bin\n",
		"Environment=HOME=/home/x\n",
		// extra variables come sorted, after PATH and HOME
		"Environment=HOME=/home/x\nEnvironment=TG_CONFIG=/home/x/.config/claude-tg/config.json\nEnvironment=TG_STATE_DIR=/home/x/.local/state/claude-tg\n",
		"StandardOutput=append:/home/x/.local/state/claude-tg/relay.out.log\n",
		"StandardError=append:/home/x/.local/state/claude-tg/relay.out.log\n",
		"[Install]\nWantedBy=default.target\n",
	} {
		if !strings.Contains(u, want) {
			t.Fatalf("unit lacks %q:\n%s", want, u)
		}
	}
	if UnitPath("/home/x") != "/home/x/.config/systemd/user/"+UnitName {
		t.Fatalf("path=%q", UnitPath("/home/x"))
	}
}

func TestUnitQuotesWordsWithSpaces(t *testing.T) {
	u := mustUnit(t, "/home/a b/bin/tg", "/home/a b/state", "/home/a b", map[string]string{"TG_CONFIG": `/home/a b/it's "here".json`})
	for _, want := range []string{
		`ExecStart="/home/a b/bin/tg" relay` + "\n",
		`Environment="PATH=/home/a b/.local/bin:/opt/homebrew/bin:/home/linuxbrew/.linuxbrew/bin:/usr/local/bin:/usr/bin:/bin"` + "\n",
		`Environment="HOME=/home/a b"` + "\n",
		`Environment="TG_CONFIG=/home/a b/it's \"here\".json"` + "\n",
		// StandardOutput takes one literal path, no word splitting, so no quotes
		"StandardOutput=append:/home/a b/state/relay.out.log\n",
	} {
		if !strings.Contains(u, want) {
			t.Fatalf("unit lacks %q:\n%s", want, u)
		}
	}
}

// % is a specifier in every directive and is doubled everywhere. $ is only
// expanded in ExecStart arguments: the program path is never a variable
// (systemd.service(5)) and Environment= gives $ no meaning
// (systemd.exec(5)), so a $ there must stay single or the path changes.
func TestUnitDoublesPercentEverywhereAndKeepsDollarInPaths(t *testing.T) {
	u := mustUnit(t, "/opt/100%/$bin/tg", "/var/100%/$x", "/home/x", map[string]string{"TG_CONFIG": "/etc/50%/$x.json"})
	for _, want := range []string{
		"ExecStart=/opt/100%%/$bin/tg relay\n",
		"StandardOutput=append:/var/100%%/$x/relay.out.log\n",
		"StandardError=append:/var/100%%/$x/relay.out.log\n",
		"Environment=TG_CONFIG=/etc/50%%/$x.json\n",
	} {
		if !strings.Contains(u, want) {
			t.Fatalf("unit lacks %q:\n%s", want, u)
		}
	}
	if strings.Contains(u, "$$") {
		t.Fatalf("unit doubles a $ outside ExecStart arguments:\n%s", u)
	}
}

// Only the arguments of the command line get $$ for a literal dollar.
func TestExecCommandDoublesDollarOnlyInArguments(t *testing.T) {
	got := execCommand("/opt/$bin/tg", "relay", "--chat=$id", "100%")
	want := "/opt/$bin/tg relay --chat=$$id 100%%"
	if got != want {
		t.Fatalf("execCommand=%q want %q", got, want)
	}
}

// A newline would start a new directive and other control characters have
// no place in a unit, so every value is checked and the error names the
// value and the character.
func TestUnitRefusesControlCharacters(t *testing.T) {
	tests := []struct {
		name                   string
		binary, stateDir, home string
		extra                  map[string]string
		wantErr                []string
	}{
		{"binary newline", "/opt/x\n/tg", "/var/x", "/home/x", nil, []string{"binary path", `"/opt/x\n/tg"`, `'\n'`}},
		{"state dir carriage return", "/opt/tg", "/var\r/x", "/home/x", nil, []string{"state directory", `'\r'`}},
		{"home tab", "/opt/tg", "/var/x", "/home/\tx", nil, []string{"home directory", `'\t'`}},
		{"extra key escape", "/opt/tg", "/var/x", "/home/x", map[string]string{"A\x1bB": "v"}, []string{"environment variable name", `"A\x1bB"`}},
		{"extra value delete", "/opt/tg", "/var/x", "/home/x", map[string]string{"TG_CONFIG": "/x\x7f.json"}, []string{"environment variable TG_CONFIG", `'\x7f'`}},
	}
	for _, tc := range tests {
		u, err := Unit(tc.binary, tc.stateDir, tc.home, tc.extra)
		if err == nil {
			t.Fatalf("%s: no error, unit:\n%s", tc.name, u)
		}
		if u != "" {
			t.Fatalf("%s: unit must be empty on error, got:\n%s", tc.name, u)
		}
		for _, want := range tc.wantErr {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: error %q lacks %q", tc.name, err, want)
			}
		}
	}
	// Ordinary values, including ones that need quoting, still render.
	mustUnit(t, "/opt/a b/tg", "/var/x", "/home/x", map[string]string{"TG_CONFIG": `/x/"q".json`})
}

func TestAgentPathIsWhatTheUnitUses(t *testing.T) {
	if !strings.Contains(mustUnit(t, "/home/x/.local/bin/tg", "/home/x/.local/state/claude-tg", "/home/x", nil), "Environment=PATH="+AgentPath("/home/x")+"\n") {
		t.Fatal("unit PATH differs from AgentPath")
	}
}
