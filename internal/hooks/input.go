// Package hooks turns Claude Code hook events into Telegram messages
// according to follow mode, the away gate and alert dedupe.
package hooks

import (
	"encoding/json"
	"errors"
	"io"
)

// Input is the subset of hook stdin JSON tg reads, across all events.
type Input struct {
	SessionID            string          `json:"session_id"`
	Cwd                  string          `json:"cwd"`
	HookEventName        string          `json:"hook_event_name"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	StopHookActive       bool            `json:"stop_hook_active"`
	Message              string          `json:"message"`
	Title                string          `json:"title"`
	NotificationType     string          `json:"notification_type"`
	ToolName             string          `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	Error                string          `json:"error"`
	ErrorDetails         string          `json:"error_details"`
}

// ParseInput decodes a hook payload; it rejects empty input and a missing session id.
func ParseInput(r io.Reader) (*Input, error) {
	b, err := io.ReadAll(io.LimitReader(r, 4<<20))
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, errors.New("hook: empty stdin")
	}
	var in Input
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, err
	}
	if in.SessionID == "" {
		return nil, errors.New("hook: payload has no session_id")
	}
	return &in, nil
}
