package send

import (
	"context"
	"errors"
	"strings"

	"github.com/IZZY-Agency/claude-tg/internal/render"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

// Editor is the part of the Telegram client EditHTML needs.
type Editor interface {
	EditMessage(ctx context.Context, chatID, messageID int64, text, parseMode string, kb [][]telegram.Button) error
}

// EditHTML replaces a message's text with html and its keyboard with kb (nil
// or empty drops it). The html is redacted between its tags first. When
// Telegram refuses it with a 400, the edit is retried once as plain text,
// as a send is: tags dropped, entities decoded, redacted again (a value a
// tag split is whole there), keyboard kept. "message is not modified" means the message already shows that
// text: it is neither retried nor an error.
func EditHTML(ctx context.Context, api Editor, chatID, messageID int64, html string, kb [][]telegram.Button) error {
	html = render.RedactHTML(html)
	err := api.EditMessage(ctx, chatID, messageID, html, "HTML", kb)
	var e *telegram.APIError
	if err == nil || !errors.As(err, &e) || e.Code != 400 {
		return err
	}
	if notModified(e) {
		return nil
	}
	err = api.EditMessage(ctx, chatID, messageID, render.Redact(render.StripHTML(html)), "", kb)
	if errors.As(err, &e) && notModified(e) {
		return nil
	}
	return err
}

func notModified(e *telegram.APIError) bool {
	return strings.Contains(e.Description, "message is not modified")
}
