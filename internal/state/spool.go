package state

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/IZZY-Agency/claude-tg/internal/telegram"
)

// SpoolItem is a message that could not be delivered yet.
type SpoolItem struct {
	ID        string               `json:"id"`
	SessionID string               `json:"session_id,omitempty"`
	Req       telegram.SendRequest `json:"req"`
	Plain     string               `json:"plain,omitempty"` // plaintext fallback for a 400 on replay
	Created   time.Time            `json:"created"`
}

// LockSpool serialises whole flush passes across processes.
func (s *Store) LockSpool(ctx context.Context) (func(), error) {
	return lockFile(ctx, filepath.Join(s.Dir, "spool.lock"))
}

func (s *Store) spoolPath(id string) string {
	return filepath.Join(s.Dir, "spool", safeName(id)+".json")
}

// SpoolPut stores item for a later FlushSpool.
func (s *Store) SpoolPut(item SpoolItem) error {
	if item.ID == "" {
		return errors.New("spool item needs an id")
	}
	return writeAtomic(s.spoolPath(item.ID), item)
}

// SpoolList returns spooled items, oldest first.
func (s *Store) SpoolList() ([]SpoolItem, error) {
	entries, err := os.ReadDir(filepath.Join(s.Dir, "spool"))
	if err != nil {
		return nil, err
	}
	var out []SpoolItem
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.Dir, "spool", e.Name()))
		if err != nil {
			continue
		}
		var it SpoolItem
		if json.Unmarshal(b, &it) == nil && it.ID != "" {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

// SpoolDelete removes a delivered or abandoned item.
func (s *Store) SpoolDelete(id string) error {
	err := os.Remove(s.spoolPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
