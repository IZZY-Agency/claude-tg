package transcript

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Title returns the session's title from its transcript: the newest one the
// owner set with /rename (a "custom-title" entry), else the newest one
// Claude Code generated ("ai-title"), its whitespace runs made single
// spaces. ok is false, with no error, when there is none in the window
// LastPrompt reads, or when the last line is still being written (the
// newest entry is unknown, as for LastPrompt). A file that cannot be read
// is an error.
func Title(path string) (title string, ok bool, err error) {
	var generated string
	unfinished, err := scanBack(path, func(b []byte) bool {
		custom := bytes.Contains(b, []byte(`"custom-title"`))
		if !custom && (generated != "" || !bytes.Contains(b, []byte(`"ai-title"`))) {
			return false
		}
		var e struct {
			Type        string `json:"type"`
			CustomTitle string `json:"customTitle"`
			AITitle     string `json:"aiTitle"`
		}
		if json.Unmarshal(bytes.TrimSpace(b), &e) != nil {
			return false
		}
		switch e.Type {
		case "custom-title":
			if t := strings.Join(strings.Fields(e.CustomTitle), " "); t != "" {
				title = t
				return true
			}
		case "ai-title":
			generated = strings.Join(strings.Fields(e.AITitle), " ")
		}
		return false
	})
	switch {
	case err != nil:
		return "", false, err
	case unfinished:
		return "", false, nil
	case title != "":
		return title, true, nil
	}
	return generated, generated != "", nil
}
