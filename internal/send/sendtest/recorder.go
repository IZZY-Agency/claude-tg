// Package sendtest holds an in-memory Telegram API double for tests.
package sendtest

import (
	"context"
	"sync"

	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

// Recorder records SendMessage calls and returns scripted errors in order.
type Recorder struct {
	mu     sync.Mutex
	Calls  []telegram.SendRequest
	Script []error // consumed one per call; nil means success
	nextID int64
}

// SendMessage implements send.API.
func (r *Recorder) SendMessage(_ context.Context, req telegram.SendRequest) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Calls = append(r.Calls, req)
	if len(r.Script) > 0 {
		e := r.Script[0]
		r.Script = r.Script[1:]
		if e != nil {
			return 0, e
		}
	}
	r.nextID++
	return r.nextID, nil
}
