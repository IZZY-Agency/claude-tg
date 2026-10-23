package relay

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Label is the launchd job label of the relay agent.
const Label = "agency.izzy.claude-tg.relay"

// AgentPath is the PATH the relay agent runs with: the only directories in
// which it can find agtermctl and tmux.
func AgentPath(home string) string {
	return filepath.Join(home, ".local", "bin") + ":/opt/homebrew/bin:/home/linuxbrew/.linuxbrew/bin:/usr/local/bin:/usr/bin:/bin"
}

// PlistPath is where the user agent definition lives.
func PlistPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// Plist renders the launchd user agent that keeps `tg relay` running. extra
// holds environment variables the agent must inherit, such as TG_CONFIG and
// TG_STATE_DIR, so it reads the same config and state as the CLI.
func Plist(binary, stateDir, home string, extra map[string]string) string {
	path := AgentPath(home)
	log := filepath.Join(stateDir, "relay.out.log")
	var env strings.Builder
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&env, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", xmlEscape(k), xmlEscape(extra[k]))
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>relay</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>%s</string>
		<key>HOME</key>
		<string>%s</string>
%s	</dict>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, Label, xmlEscape(binary), xmlEscape(path), xmlEscape(home), env.String(), xmlEscape(log), xmlEscape(log))
}
