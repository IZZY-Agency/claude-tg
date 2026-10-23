package state

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Dedupe reports whether an alert with key may be sent now and records it
// when it may. The check and the update happen under a lock so two hooks
// cannot both pass. A persistence failure still allows the send and is
// returned so the caller can log it.
func (s *Store) Dedupe(key string, window time.Duration, now time.Time) (bool, error) {
	path := filepath.Join(s.Dir, "dedupe.json")
	release, err := lockFile(context.Background(), path+".lock")
	if err != nil {
		return true, err
	}
	defer release()
	seen := map[string]time.Time{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &seen)
	}
	for k, t := range seen {
		if now.Sub(t) > time.Hour {
			delete(seen, k)
		}
	}
	if last, ok := seen[key]; ok && now.Sub(last) < window && !now.Before(last) {
		return false, nil
	}
	seen[key] = now
	if err := writeAtomic(path, seen); err != nil {
		return true, err
	}
	return true, nil
}
