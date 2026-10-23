package render

import (
	"fmt"
	"strings"
)

// SourceChunk is the source-text budget per Telegram message; the visible
// limit is 4096 and headers plus fence repairs need headroom.
const SourceChunk = 3500

// MaxChunks caps how many messages one send may produce.
const MaxChunks = 4

// fenceMargin is reserved so a chunk can close and the next reopen a fence.
const fenceMargin = 8

type piece struct {
	text   string
	joiner string // how this piece attaches to the previous one in the same chunk
}

// Chunk splits src into pieces of at most max characters (runes), preferring
// blank-line, then newline, then space boundaries, then a hard split. A
// chunk that ends inside a ``` fence gets it closed, and the next chunk
// reopens it.
func Chunk(src string, max int) []string {
	if src == "" {
		return nil
	}
	limit := max - fenceMargin
	if limit < 16 {
		limit = max
	}
	var pieces []piece
	for i, para := range strings.Split(src, "\n\n") {
		j := "\n\n"
		if i == 0 {
			j = ""
		}
		pieces = append(pieces, split(para, j, limit)...)
	}
	var chunks []string
	var cur strings.Builder
	curLen := 0
	for _, p := range pieces {
		pl := len([]rune(p.text))
		if curLen > 0 && curLen+len([]rune(p.joiner))+pl > limit {
			chunks = append(chunks, cur.String())
			cur.Reset()
			curLen = 0
		}
		if curLen > 0 {
			cur.WriteString(p.joiner)
			curLen += len([]rune(p.joiner))
		}
		cur.WriteString(p.text)
		curLen += pl
	}
	if curLen > 0 || len(chunks) == 0 {
		chunks = append(chunks, cur.String())
	}
	return balanceFences(chunks)
}

// split breaks one paragraph into pieces no longer than limit.
func split(para, joiner string, limit int) []piece {
	if len([]rune(para)) <= limit {
		return []piece{{para, joiner}}
	}
	var out []piece
	first := true
	for _, line := range strings.Split(para, "\n") {
		j := "\n"
		if first {
			j = joiner
			first = false
		}
		if len([]rune(line)) <= limit {
			out = append(out, piece{line, j})
			continue
		}
		wfirst := true
		for _, word := range strings.Split(line, " ") {
			wj := " "
			if wfirst {
				wj = j
				wfirst = false
			}
			r := []rune(word)
			for len(r) > limit {
				out = append(out, piece{string(r[:limit]), wj})
				r = r[limit:]
				wj = ""
			}
			out = append(out, piece{string(r), wj})
		}
	}
	return out
}

func countFences(s string) int {
	n := 0
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "```") {
			n++
		}
	}
	return n
}

func balanceFences(chunks []string) []string {
	inFence := false
	for i := range chunks {
		startIn := inFence
		endIn := startIn != (countFences(chunks[i])%2 == 1) // state from the original markers
		if startIn {
			chunks[i] = "```\n" + chunks[i]
		}
		if endIn {
			chunks[i] += "\n```"
		}
		inFence = endIn
	}
	return chunks
}

// Truncate keeps at most maxChunks and appends a note about dropped text.
func Truncate(chunks []string, maxChunks int) []string {
	if len(chunks) <= maxChunks {
		return chunks
	}
	dropped := 0
	for _, c := range chunks[maxChunks:] {
		dropped += len([]rune(c))
	}
	kept := append([]string(nil), chunks[:maxChunks]...)
	kept[maxChunks-1] += fmt.Sprintf("\n… truncated, %d more characters", dropped)
	return kept
}
