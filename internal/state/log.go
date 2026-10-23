package state

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	logMax  = 5 << 20 // rotate above 5 MB
	logKeep = 1 << 20 // keep the last 1 MB
)

// Logf appends one timestamped line to tg.log, keeping the file bounded.
func (s *Store) Logf(format string, args ...any) {
	path := filepath.Join(s.Dir, "tg.log")
	if fi, err := os.Stat(path); err == nil && fi.Size() > logMax {
		if b, err := os.ReadFile(path); err == nil {
			_ = os.WriteFile(path+".tmp", b[len(b)-logKeep:], 0o600)
			_ = os.Rename(path+".tmp", path)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	msg := fmt.Sprintf(format, args...)
	if len(msg) == 0 || msg[len(msg)-1] != '\n' {
		msg += "\n"
	}
	fmt.Fprintf(f, "%s %s", time.Now().Format(time.RFC3339), msg)
}
