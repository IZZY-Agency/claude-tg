// Package sendtest holds an in-memory Telegram API double for tests.
package sendtest

import (
	"context"
	"sync"

	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

// Recorder records SendMessage and SendVoice calls and returns scripted errors in order.
type Recorder struct {
	mu     sync.Mutex
	Calls  []telegram.SendRequest
	Voices []telegram.VoiceRequest
	Script []error // consumed one per call (text or voice); nil means success
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

// SendVoice implements send.VoiceAPI, consuming Script like SendMessage.
func (r *Recorder) SendVoice(_ context.Context, req telegram.VoiceRequest) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Voices = append(r.Voices, req)
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
