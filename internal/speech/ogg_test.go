package speech

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// fixtureDuration is what `ffprobe -v error -show_entries format=duration
// -of default=nw=1 testdata/ru.ogg` printed when the fixture was made
// (duration=1.941583).
const fixtureDuration = 1941583 * time.Microsecond

// oggPage builds one Ogg page around body with the given granule position.
func oggPage(granule uint64, seq uint32, body []byte) []byte {
	var lacing []byte
	for n := len(body); ; n -= 255 {
		if n >= 255 {
			lacing = append(lacing, 255)
			continue
		}
		lacing = append(lacing, byte(n))
		break
	}
	h := make([]byte, 27)
	copy(h, "OggS")
	binary.LittleEndian.PutUint64(h[6:], granule)
	binary.LittleEndian.PutUint32(h[18:], seq)
	h[26] = byte(len(lacing))
	return append(append(h, lacing...), body...)
}

// opusHead returns an OpusHead packet with the given pre-skip.
func opusHead(preSkip uint16) []byte {
	b := make([]byte, 19)
	copy(b, "OpusHead")
	b[8] = 1
	b[9] = 1
	binary.LittleEndian.PutUint16(b[10:], preSkip)
	binary.LittleEndian.PutUint32(b[12:], 48000)
	return b
}

func TestDuration(t *testing.T) {
	t.Run("fixture", func(t *testing.T) {
		b64, err := os.ReadFile("testdata/ru.ogg.b64")
		if err != nil {
			t.Fatal(err)
		}
		data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(b64)), ""))
		if err != nil {
			t.Fatal(err)
		}
		got, err := Duration(data)
		if err != nil {
			t.Fatal(err)
		}
		if diff := got - fixtureDuration; diff < -20*time.Millisecond || diff > 20*time.Millisecond {
			t.Errorf("Duration = %v, want %v within 20ms", got, fixtureDuration)
		}
	})

	t.Run("synthetic pages", func(t *testing.T) {
		// A payload that contains "OggS" must not be mistaken for a page.
		fake := append([]byte("xx OggS"), make([]byte, 40)...)
		var data []byte
		data = append(data, oggPage(0, 0, opusHead(312))...)
		data = append(data, oggPage(312+48000*3, 1, fake)...)
		data = append(data, oggPage(312+48000*6, 2, bytes.Repeat([]byte{7}, 600))...)
		got, err := Duration(data)
		if err != nil {
			t.Fatal(err)
		}
		if got != 6*time.Second {
			t.Errorf("Duration = %v, want 6s", got)
		}
	})

	t.Run("no usable audio", func(t *testing.T) {
		header := oggPage(0, 0, opusHead(312))
		for name, data := range map[string][]byte{
			"header only":         header,
			"granule at pre-skip": append(append([]byte{}, header...), oggPage(312, 1, []byte("x"))...),
			"granule below":       append(append([]byte{}, header...), oggPage(100, 1, []byte("x"))...),
			"overflow at limit":   append(append([]byte{}, header...), oggPage(312+48000*9223372036, 1, []byte("x"))...),
			"overflow":            append(append([]byte{}, header...), oggPage(1<<62, 1, []byte("x"))...),
		} {
			if got, err := Duration(data); !errors.Is(err, ErrNotOpus) {
				t.Errorf("%s: Duration = %v, %v; want ErrNotOpus", name, got, err)
			}
		}
	})

	t.Run("just below the overflow limit", func(t *testing.T) {
		data := append(oggPage(0, 0, opusHead(312)), oggPage(312+48000*9223372035+47999, 1, []byte("x"))...)
		if got, err := Duration(data); err != nil || got <= 0 {
			t.Errorf("Duration = %v, %v; want positive", got, err)
		}
	})

	t.Run("not opus", func(t *testing.T) {
		for name, data := range map[string][]byte{
			"riff":           []byte("RIFF...."),
			"head not first": oggPage(0, 0, append([]byte("xx"), opusHead(312)...)),
			"no opus head":   oggPage(100, 0, []byte("vorbis....")),
			"empty":          nil,
			"short header":   []byte("OggS"),
			"truncated head": oggPage(0, 0, opusHead(312))[:30],
		} {
			if _, err := Duration(data); !errors.Is(err, ErrNotOpus) {
				t.Errorf("%s: err = %v, want ErrNotOpus", name, err)
			}
		}
	})
}
