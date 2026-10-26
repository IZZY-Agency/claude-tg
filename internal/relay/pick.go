package relay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/render"
	"github.com/IZZY-Agency/claude-tg/internal/state"
	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

const (
	// pickTTL is how long a pick lasts after it was made or a message last
	// went to its session through it.
	pickTTL = time.Hour
	// pickData starts the callback data of the buttons of /sessions:
	// "p:<sessionID>" picks that session, pickClear (✖ Clear) clears it.
	pickData  = "p:"
	pickClear = pickData + "-"
	// maxCallbackData is Telegram's limit on a button's callback data, in
	// bytes.
	maxCallbackData = 64
	// maxEndedName caps the session name in the note on a picked session
	// that ended, in UTF-16 code units, so that a voice confirmation's
	// first line with it stays as short as one naming a session (ruling
	// R31).
	maxEndedName = 40
)

// The copy of the pick that needs no argument (HTML).
const (
	pickCleared = "✖ Pick cleared"
	pickNow     = "📌 now: "
	pickNext    = "📌 Next messages go to "
)

// currentPick returns the owner's pick while it is fresh; a stale one is
// removed. A pick that cannot be read is an error with the reason for the
// phone (HTML): it never counts as no pick, which would send the message
// elsewhere.
func (r *Router) currentPick() (state.Pick, bool, error) {
	now := r.now()
	p, err := r.Store.UpdatePick(func(p *state.Pick) {
		if !p.Fresh(now, pickTTL) {
			*p = state.Pick{}
		}
	})
	if err != nil {
		r.logf("pick: %v", err)
		return state.Pick{}, false, fmt.Errorf("cannot read the session you picked: %s; pick again with /sessions", render.EscapeHTML(err.Error()))
	}
	return p, p.SessionID != "", nil
}

// dropPick removes the pick when it still names sessionID, as when that
// session has ended.
func (r *Router) dropPick(sessionID string) {
	if _, err := r.Store.UpdatePick(func(p *state.Pick) {
		if p.SessionID == sessionID {
			*p = state.Pick{}
		}
	}); err != nil {
		r.logf("drop the pick: %v", err)
	}
}

// usedPick restarts the pick's hour after a text was delivered to
// sessionID, by whatever route, when the pick still names that session
// (the owner may have picked another meanwhile), whatever its age: the
// hour may have run out while the text was typed.
func (r *Router) usedPick(sessionID string) {
	now := r.now()
	if _, err := r.Store.UpdatePick(func(p *state.Pick) {
		if p.SessionID == sessionID {
			p.At = now
		}
	}); err != nil {
		r.logf("refresh the pick: %v", err)
	}
}

// pickEnded is target's error when the picked session has ended: the pick
// is dropped, and the message gets the session keyboard with a note
// (endedNote). It is an errNeedTarget.
type pickEnded struct{ name string }

func (e *pickEnded) Error() string     { return "need target: the picked session has ended" }
func (e *pickEnded) Is(err error) bool { return err == errNeedTarget }

// endedPick drops the pick of sessionID, which is not live, and returns
// target's error for it, naming the session as its record does.
func (r *Router) endedPick(sessionID string) error {
	r.dropPick(sessionID)
	name := ""
	if sess, err := r.Store.LoadSession(sessionID); err == nil {
		name = sess.Name
	}
	return &pickEnded{name: name}
}

// endedNote is the line that tells the owner the picked session has ended
// when err is target's pickEnded, else "" (HTML). The name is cut only
// after the secrets are masked and credentials redacted, so a cut never
// leaves a secret's start behind for the outbound scrub to miss.
func (r *Router) endedNote(err error) string {
	var e *pickEnded
	if !errors.As(err, &e) {
		return ""
	}
	if e.name == "" {
		return "📌 the picked session has ended"
	}
	return "📌 " + bold(clipUTF16(render.Redact(r.scrub(e.name)), maxEndedName)) + " has ended"
}

// withNote puts note, when there is one, on a line above html.
func withNote(note, html string) string {
	if note == "" {
		return html
	}
	return note + "\n" + html
}

// pickKeyboard is the keyboard of /sessions: a button per live session
// that has a pane to type into, which picks it, and ✖ Clear when a pick
// is active.
func pickKeyboard(live []Live, picked bool) [][]telegram.Button {
	var rows [][]telegram.Button
	for _, l := range live {
		data := pickData + l.SessionID
		if l.Pane.IsZero() || l.SessionID == "" || len(data) > maxCallbackData {
			continue
		}
		rows = append(rows, []telegram.Button{{Text: l.Name, Data: data}})
	}
	if picked {
		rows = append(rows, []telegram.Button{{Text: "✖ Clear", Data: pickClear}})
	}
	return rows
}

// pickTap is a button of /sessions: callback data "p:<sessionID>" picks
// that session, so that plain text and voice notes go there, and pickClear
// clears the pick; either replaces a pick that cannot be read. The message is edited to say so and its buttons go. A
// session that has ended meanwhile is refused with a toast, the message
// left as it is. The caller has checked the sender.
func (r *Router) pickTap(ctx context.Context, cq *telegram.CallbackQuery) error {
	edit := func(html string) {
		if err := r.edit(ctx, cq.Message.MessageID, html); err != nil {
			r.logf("edit: %v", err)
		}
	}
	if cq.Data == pickClear {
		if err := r.Store.SavePick(state.Pick{}); err != nil {
			r.answerCallback(ctx, cq, "cannot clear the pick: "+err.Error())
			return err
		}
		edit(pickCleared)
		r.answerCallback(ctx, cq, "cleared")
		return nil
	}
	sid := strings.TrimPrefix(cq.Data, pickData)
	if sid == "" {
		r.answerCallback(ctx, cq, "unknown button")
		return nil
	}
	live, err := r.live(ctx)
	if err != nil {
		r.answerCallback(ctx, cq, "cannot list sessions")
		return err
	}
	l := bySession(live, sid)
	if l == nil {
		r.answerCallback(ctx, cq, "that session has ended")
		return nil
	}
	now := r.now()
	if err := r.Store.SavePick(state.Pick{SessionID: sid, At: now}); err != nil {
		r.answerCallback(ctx, cq, "cannot save the pick: "+err.Error())
		return err
	}
	edit(pickNext + bold(l.Name))
	r.answerCallback(ctx, cq, "next messages go to "+l.Name)
	return nil
}
