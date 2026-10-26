package speech

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"time"
)

// ErrNotOpus is returned when the data is not an Ogg stream that starts with
// an Opus header.
var ErrNotOpus = errors.New("speech_command did not write an OGG/Opus file")

const (
	oggHeaderLen   = 27         // fixed part of an Ogg page header
	opusSampleRate = 48000      // Opus granule positions always count 48 kHz samples
	noGranule      = ^uint64(0) // granule of a page on which no packet ends
)

// Duration returns the playing time of an OGG/Opus file: the granule
// position of its last page, less the pre-skip from the OpusHead packet,
// at 48 kHz. It walks the page structure rather than searching for "OggS",
// because audio payload may contain those bytes. A trailing page cut short
// is ignored. A stream with no audio past the pre-skip, or one too long to
// represent, is ErrNotOpus, so a successful result is always positive.
func Duration(ogg []byte) (time.Duration, error) {
	if !bytes.HasPrefix(ogg, []byte("OggS")) {
		return 0, ErrNotOpus
	}
	bodyStart, firstEnd, ok := oggPageAt(ogg, 0)
	if !ok {
		return 0, ErrNotOpus
	}
	// RFC 7845: OpusHead is the first packet, so it starts the first body.
	head := ogg[bodyStart:firstEnd]
	if !bytes.HasPrefix(head, []byte("OpusHead")) || len(head) < 12 {
		return 0, ErrNotOpus
	}
	preSkip := uint64(binary.LittleEndian.Uint16(head[10:]))

	var last uint64
	for off := 0; off < len(ogg); {
		_, end, ok := oggPageAt(ogg, off)
		if !ok {
			break
		}
		if g := binary.LittleEndian.Uint64(ogg[off+6:]); g != noGranule {
			last = g
		}
		off = end
	}
	// No audio page, or nothing past the pre-skip, is not usable audio.
	if last <= preSkip || (last-preSkip)/opusSampleRate > uint64(math.MaxInt64/int64(time.Second)-1) {
		return 0, ErrNotOpus
	}
	samples := last - preSkip
	return time.Duration(samples/opusSampleRate)*time.Second +
		time.Duration(samples%opusSampleRate)*time.Second/opusSampleRate, nil
}

// oggPageAt parses the page header at off. It returns where the page body
// starts and where the page ends, or ok false when there is no complete page
// at off.
func oggPageAt(ogg []byte, off int) (body, end int, ok bool) {
	if len(ogg)-off < oggHeaderLen || !bytes.Equal(ogg[off:off+4], []byte("OggS")) {
		return 0, 0, false
	}
	segments := int(ogg[off+26])
	body = off + oggHeaderLen + segments
	if body > len(ogg) {
		return 0, 0, false
	}
	end = body
	for _, n := range ogg[off+oggHeaderLen : body] {
		end += int(n)
	}
	if end > len(ogg) {
		return 0, 0, false
	}
	return body, end, true
}
