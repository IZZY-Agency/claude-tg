package telegram

import (
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
