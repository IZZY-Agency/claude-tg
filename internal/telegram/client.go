// Package telegram is a minimal Bot API client: sendMessage, getMe, getUpdates.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is the public Bot API endpoint.
const DefaultBaseURL = "https://api.telegram.org"

// Client talks to one bot.
type Client struct {
	Token   string
	HTTP    *http.Client
	BaseURL string
}

// New returns a client with a 10 s timeout.
func New(token string) *Client {
	return &Client{Token: token, HTTP: &http.Client{Timeout: 10 * time.Second}, BaseURL: DefaultBaseURL}
}

func (c *Client) methodURL(method string) string {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	return base + "/bot" + c.Token + "/" + method
}

// ParseResponse decodes a Bot API envelope into out (may be nil) and turns
// "ok": false into an *APIError.
func ParseResponse(body []byte, out any) error {
	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("telegram: invalid response: %w", err)
	}
	if !env.OK {
		return &APIError{Code: env.ErrorCode, Description: env.Description, RetryAfter: env.Parameters.RetryAfter}
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("telegram: invalid result: %w", err)
		}
	}
	return nil
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	var body io.Reader
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.methodURL(method), body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return c.scrub(wrapTransport(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return c.scrub(wrapTransport(err))
	}
	if resp.StatusCode >= 500 {
		return &NetError{Err: fmt.Errorf("http %d", resp.StatusCode)}
	}
	return ParseResponse(data, out)
}

// replyMarkup builds the inline_keyboard reply_markup for kb (never nil rows).
func replyMarkup(kb [][]Button) map[string]any {
	rows := make([][]map[string]any, 0, len(kb))
	for _, row := range kb {
		out := make([]map[string]any, 0, len(row))
		for _, b := range row {
			out = append(out, map[string]any{"text": b.Text, "callback_data": b.Data})
		}
		rows = append(rows, out)
	}
	return map[string]any{"inline_keyboard": rows}
}

// SendParams builds the sendMessage body for r.
func SendParams(r SendRequest) map[string]any {
	params := map[string]any{"chat_id": r.ChatID, "text": r.Text}
	if r.ParseMode != "" {
		params["parse_mode"] = r.ParseMode
	}
	if r.Silent {
		params["disable_notification"] = true
	}
	if r.ReplyToMessageID != 0 {
		params["reply_parameters"] = map[string]any{"message_id": r.ReplyToMessageID, "allow_sending_without_reply": true}
	}
	if len(r.InlineKeyboard) > 0 {
		params["reply_markup"] = replyMarkup(r.InlineKeyboard)
	}
	return params
}

// SendMessage posts r and returns the new message id.
func (c *Client) SendMessage(ctx context.Context, r SendRequest) (int64, error) {
	var msg Message
	if err := c.call(ctx, "sendMessage", SendParams(r), &msg); err != nil {
		return 0, err
	}
	return msg.MessageID, nil
}

// SetMessageReaction puts one emoji reaction on a message.
func (c *Client) SetMessageReaction(ctx context.Context, chatID, messageID int64, emoji string) error {
	params := map[string]any{"chat_id": chatID, "message_id": messageID, "reaction": []map[string]string{{"type": "emoji", "emoji": emoji}}}
	return c.call(ctx, "setMessageReaction", params, nil)
}

// AnswerCallbackQuery acknowledges a button press, optionally with a toast.
func (c *Client) AnswerCallbackQuery(ctx context.Context, id, text string) error {
	params := map[string]any{"callback_query_id": id}
	if text != "" {
		params["text"] = text
	}
	return c.call(ctx, "answerCallbackQuery", params, nil)
}

// EditMessageText replaces a bot message's text (and drops its keyboard).
func (c *Client) EditMessageText(ctx context.Context, chatID, messageID int64, text, parseMode string) error {
	params := map[string]any{"chat_id": chatID, "message_id": messageID, "text": text}
	if parseMode != "" {
		params["parse_mode"] = parseMode
	}
	return c.call(ctx, "editMessageText", params, nil)
}

// EditMessage replaces a bot message's text and sets its keyboard; a nil or
// empty kb drops the keyboard.
func (c *Client) EditMessage(ctx context.Context, chatID, messageID int64, text, parseMode string, kb [][]Button) error {
	params := map[string]any{"chat_id": chatID, "message_id": messageID, "text": text}
	if parseMode != "" {
		params["parse_mode"] = parseMode
	}
	if len(kb) > 0 {
		params["reply_markup"] = replyMarkup(kb)
	}
	return c.call(ctx, "editMessageText", params, nil)
}

// EditReplyMarkup replaces only a bot message's keyboard; an empty kb clears it.
func (c *Client) EditReplyMarkup(ctx context.Context, chatID, messageID int64, kb [][]Button) error {
	params := map[string]any{"chat_id": chatID, "message_id": messageID, "reply_markup": replyMarkup(kb)}
	return c.call(ctx, "editMessageReplyMarkup", params, nil)
}

// SetMyCommands fills the bot's "/" menu.
func (c *Client) SetMyCommands(ctx context.Context, cmds []BotCommand) error {
	return c.call(ctx, "setMyCommands", map[string]any{"commands": cmds}, nil)
}

// GetMe returns the bot's own user record; it validates the token.
func (c *Client) GetMe(ctx context.Context) (User, error) {
	var u User
	err := c.call(ctx, "getMe", nil, &u)
	return u, err
}

// GetUpdates long-polls for up to timeoutSec seconds.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]Update, error) {
	params := map[string]any{"offset": offset, "timeout": timeoutSec, "allowed_updates": []string{"message", "callback_query"}}
	httpc := c.HTTP
	if httpc != nil && httpc.Timeout > 0 && httpc.Timeout < time.Duration(timeoutSec+5)*time.Second {
		// Long polling needs a longer HTTP timeout than the default 10 s.
		cp := *httpc
		cp.Timeout = time.Duration(timeoutSec+5) * time.Second
		c = &Client{Token: c.Token, HTTP: &cp, BaseURL: c.BaseURL}
	}
	var ups []Update
	err := c.call(ctx, "getUpdates", params, &ups)
	return ups, err
}

// wrapTransport turns a transport failure into a NetError without the
// request URL, which carries the bot token.
func wrapTransport(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return &NetError{Err: fmt.Errorf("%s: %w", strings.ToLower(ue.Op), ue.Err)}
	}
	return &NetError{Err: err}
}

// scrub removes the token from an error message as a last line of defence.
func (c *Client) scrub(err error) error {
	if err == nil || c.Token == "" || !strings.Contains(err.Error(), c.Token) {
		return err
	}
	return &NetError{Err: errors.New(strings.ReplaceAll(err.Error(), c.Token, "***"))}
}
