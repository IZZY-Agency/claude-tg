// Package config stores the bot credentials and install facts for tg.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrNotConfigured is returned by Load when no config file exists yet.
var ErrNotConfigured = errors.New("tg is not configured; run `tg setup`")

// DefaultAwayAfterSeconds is the idle threshold used when the key is absent.
const DefaultAwayAfterSeconds = 120

// Config is the on-disk configuration (mode 0600).
type Config struct {
	BotToken         string    `json:"bot_token"`
	ChatID           int64     `json:"chat_id"`
	UserID           int64     `json:"user_id,omitempty"`
	BotUsername      string    `json:"bot_username,omitempty"`
	ClaudePath       string    `json:"claude_path,omitempty"`
	BinaryPath       string    `json:"binary_path,omitempty"`
	AwayAfterSeconds int       `json:"away_after_seconds"`
	InstalledAt      time.Time `json:"installed_at,omitempty"`
	// VoiceCommand is a shell string that turns a voice note into text; see
	// the voice package. Empty means voice notes are not supported.
	VoiceCommand string `json:"voice_command,omitempty"`
	// SpeechCommand is a shell string that turns text into an OGG/Opus file;
	// see the voice package. Empty means answers are never spoken.
	SpeechCommand string `json:"speech_command,omitempty"`
	// TypesafeAPIKey is the TypeSafe API key with which a hook asks whether
	// a typed phone answer is worth speaking; see the classify package.
	// Empty means typed answers are never spoken.
	TypesafeAPIKey string `json:"typesafe_api_key,omitempty"`
}

// Secrets are the configured values that no message, button or log line
// may show (send.Sender.Secrets): the bot token and the TypeSafe API key,
// those that are set.
func (c *Config) Secrets() []string {
	var out []string
	for _, s := range []string{c.BotToken, c.TypesafeAPIKey} {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.Getenv("HOME")
}

// DefaultPath is $XDG_CONFIG_HOME/claude-tg/config.json, or ~/.config/claude-tg/config.json.
func DefaultPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(homeDir(), ".config")
	}
	return filepath.Join(base, "claude-tg", "config.json")
}

// StateDir is $XDG_STATE_HOME/claude-tg, or ~/.local/state/claude-tg.
func StateDir() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		base = filepath.Join(homeDir(), ".local", "state")
	}
	return filepath.Join(base, "claude-tg")
}

// Load reads the config at path. A missing file yields ErrNotConfigured.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotConfigured
		}
		return nil, err
	}
	// Decode into a shadow struct so an absent away_after_seconds can be told
	// apart from an explicit 0 (which disables the gate).
	var raw struct {
		Config
		AwayAfterSeconds *int `json:"away_after_seconds"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c := raw.Config
	if raw.AwayAfterSeconds == nil {
		c.AwayAfterSeconds = DefaultAwayAfterSeconds
	} else {
		c.AwayAfterSeconds = *raw.AwayAfterSeconds
	}
	return &c, nil
}

// Save writes the config atomically with directory mode 0700 and file mode 0600.
func (c *Config) Save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
