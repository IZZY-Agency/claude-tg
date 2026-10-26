package telegram

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Button is one inline-keyboard button.
type Button struct {
	Text string
	Data string // callback_data, at most 64 bytes
}

// SendRequest is the subset of sendMessage parameters tg uses.
type SendRequest struct {
	ChatID           int64
	Text             string
	ParseMode        string     // "HTML" or "" for plain text
	Silent           bool       // disable_notification
	ReplyToMessageID int64      // reply_parameters.message_id when non-zero
	InlineKeyboard   [][]Button // reply_markup.inline_keyboard when non-empty
}

// VoiceRequest is the subset of sendVoice parameters tg uses.
type VoiceRequest struct {
	ChatID   int64
	Audio    []byte // an OGG/Opus file
	Duration int    // seconds; omitted when zero
	Caption  string // at most 1024 characters; omitted when empty
	Silent   bool   // disable_notification
}

// BotCommand is one entry of the "/" menu.
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// User is a Telegram user or bot.
type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	IsBot     bool   `json:"is_bot"`
}

// Chat is the chat a message belongs to.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// Message is the subset of a Telegram message tg reads.
type Message struct {
	MessageID      int64    `json:"message_id"`
	Date           int64    `json:"date"` // unix seconds
	Chat           Chat     `json:"chat"`
	From           *User    `json:"from"`
	Text           string   `json:"text"`
	ReplyToMessage *Message `json:"reply_to_message"`
	Voice          *Voice   `json:"voice"`

	// Other media kinds are kept only to tell that one is present.
	Photo     json.RawMessage `json:"photo,omitempty"`
	Sticker   json.RawMessage `json:"sticker,omitempty"`
	Audio     json.RawMessage `json:"audio,omitempty"`
	Video     json.RawMessage `json:"video,omitempty"`
	VideoNote json.RawMessage `json:"video_note,omitempty"`
	Document  json.RawMessage `json:"document,omitempty"`
	Animation json.RawMessage `json:"animation,omitempty"`
	Location  json.RawMessage `json:"location,omitempty"`
	Contact   json.RawMessage `json:"contact,omitempty"`
	Poll      json.RawMessage `json:"poll,omitempty"`
	Dice      json.RawMessage `json:"dice,omitempty"`
	Venue     json.RawMessage `json:"venue,omitempty"`
}

// HasMedia reports whether the message carries content tg does not read: a
// photo, sticker, audio, video, video note, document, animation, location,
// contact, poll, dice or venue. A voice note is not counted; it has its own
// field. A service message (a pin, a member joining) carries none.
func (m *Message) HasMedia() bool {
	for _, raw := range []json.RawMessage{m.Photo, m.Sticker, m.Audio, m.Video, m.VideoNote, m.Document, m.Animation, m.Location, m.Contact, m.Poll, m.Dice, m.Venue} {
		if len(raw) > 0 {
			return true
		}
	}
	return false
}

// Voice is a voice note.
type Voice struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Duration     int    `json:"duration"` // seconds
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
}

// File is the getFile answer: where to download a file from.
type File struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
	FilePath string `json:"file_path"`
}

// CallbackQuery is an inline-keyboard button press.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    *User    `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

// Update is one getUpdates entry.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

// APIError is a Telegram "ok": false response.
type APIError struct {
	Code        int
	Description string
	RetryAfter  int // seconds, set on 429
}

func (e *APIError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("telegram %d: %s (retry after %ds)", e.Code, e.Description, e.RetryAfter)
	}
	return fmt.Sprintf("telegram %d: %s", e.Code, e.Description)
}

// NetError wraps a transport-level failure (DNS, TCP, TLS, timeout).
type NetError struct{ Err error }

func (e *NetError) Error() string { return "telegram network: " + e.Err.Error() }
func (e *NetError) Unwrap() error { return e.Err }

// IsNetwork reports whether err is a transport failure rather than an API refusal.
func IsNetwork(err error) bool {
	var ne *NetError
	return errors.As(err, &ne)
}
