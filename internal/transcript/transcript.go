// Package transcript watches a Claude Code session transcript (JSONL) to learn
// whether a pending tool call's dialog was answered somewhere else.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
)

// firstScanWindow bounds how much of a large transcript the first Poll reads.
const firstScanWindow = 8 << 20

// Watch follows one tool call in a transcript.
type Watch struct {
	path    string
	tool    string
	input   any
	badJSON bool            // the input does not parse: no call matches
	start   int64           // the file's size when the watch was made
	offset  int64           // bytes consumed so far, including partial
	partial []byte          // trailing line without its newline yet
	scanned bool            // first scan done
	cands   []cand          // matching tool_use items, in transcript order
	id      string          // the watched call's tool_use id, once chosen
	fresh   bool            // id was written after start: final
	results map[string]bool // candidate ids that have a tool_result
}

// cand is a matching tool_use item and the offset of its line.
type cand struct {
	id  string
	off int64
}

// NewWatch watches the call to tool with the given input. It notes the
// file's current size: Claude Code may write the pending call only once its
// dialog is answered, together with the result, so any matching call after
// that point is the watched one. A transcript that cannot be stat'ed is an
// error. An input that is not JSON matches no call.
func NewWatch(path, tool string, input json.RawMessage) (*Watch, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	w := &Watch{path: path, tool: tool, start: st.Size(), results: map[string]bool{}}
	w.badJSON = json.Unmarshal(input, &w.input) != nil
	return w, nil
}

type line struct {
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type item struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
}

// Poll reads what the transcript gained since the last call and reports
// whether the watched call has a result (the dialog was answered, by anyone).
func (w *Watch) Poll() (resolved bool, err error) {
	f, err := os.Open(w.path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	size := st.Size()
	switch {
	case !w.scanned:
		// The window never starts past the watch's start: whatever came
		// after it must be read.
		if w.offset, err = lineStart(f, min(max(0, size-firstScanWindow), w.start)); err != nil {
			return false, err
		}
		w.scanned = true
	case size < w.offset:
		w.offset, w.partial, w.id, w.fresh, w.cands = 0, nil, "", false, nil
		w.results = map[string]bool{}
	}
	if _, err := f.Seek(w.offset, io.SeekStart); err != nil {
		return false, err
	}
	r := bufio.NewReader(f)
	for {
		b, rerr := r.ReadBytes('\n')
		w.offset += int64(len(b))
		if len(b) > 0 && b[len(b)-1] == '\n' {
			full := append(w.partial, b...)
			w.partial = nil
			w.handle(full, w.offset-int64(len(full)))
		} else {
			w.partial = append(w.partial, b...)
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				return false, rerr
			}
			break
		}
	}
	w.choose()
	return w.id != "" && w.results[w.id], nil
}

// choose picks the watched call. The first matching call written after the
// watch began is it, resolved or not, and the choice is final. Before such
// a call shows up, the last matching call from before the start that has
// no result stands in; older identical calls that finished never count.
//
// Limitation: the PermissionRequest input has no tool_use_id, so calls are
// told apart only by tool and input. When one message makes several
// identical calls, each with its own dialog, their hooks cannot tell which
// call is theirs: a hook may take another call's result for its own and
// close while its dialog is still open, or wait on a call that already
// finished until the screen guard or the deadline ends it.
func (w *Watch) choose() {
	if w.fresh {
		return
	}
	for _, c := range w.cands {
		if c.off >= w.start {
			w.id, w.fresh = c.id, true
			return
		}
	}
	if w.id != "" {
		return
	}
	for i := len(w.cands) - 1; i >= 0; i-- {
		if !w.results[w.cands[i].id] {
			w.id = w.cands[i].id
			return
		}
	}
}

// lineStart returns the offset of the line containing pos (pos itself when it
// already starts a line), so a first scan never cuts a line in half.
func lineStart(f *os.File, pos int64) (int64, error) {
	buf := make([]byte, 64<<10)
	for end := pos; end > 0; {
		n := int64(len(buf))
		if end < n {
			n = end
		}
		if _, err := f.ReadAt(buf[:n], end-n); err != nil {
			return 0, err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			return end - n + int64(i) + 1, nil
		}
		end -= n
	}
	return 0, nil
}

// handle reads one complete line that starts at offset off.
func (w *Watch) handle(b []byte, off int64) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || !bytes.Contains(b, []byte(`"content"`)) {
		return
	}
	var l line
	if json.Unmarshal(b, &l) != nil || len(l.Message.Content) == 0 || l.Message.Content[0] != '[' {
		return
	}
	var items []item
	if json.Unmarshal(l.Message.Content, &items) != nil {
		return
	}
	for _, it := range items {
		switch it.Type {
		case "tool_result":
			// A result always follows its call: keep only candidates'.
			if w.isCand(it.ToolUseID) {
				w.results[it.ToolUseID] = true
			}
		case "tool_use":
			if it.Name == w.tool && w.sameInput(it.Input) {
				w.cands = append(w.cands, cand{it.ID, off})
			}
		}
	}
}

func (w *Watch) isCand(id string) bool {
	for _, c := range w.cands {
		if c.id == id {
			return true
		}
	}
	return false
}

func (w *Watch) sameInput(raw json.RawMessage) bool {
	if w.badJSON {
		return false
	}
	var in any
	return json.Unmarshal(raw, &in) == nil && reflect.DeepEqual(in, w.input)
}
