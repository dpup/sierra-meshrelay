// Package companion implements the small slice of the MeshCore companion
// protocol the bridge needs: the serial/TCP frame header, and the APP_START /
// SELF_INFO identity check (plus DEVICE_QUERY for firmware and model).
//
// Verified against MeshCore companion-v1.18.0:
// src/helpers/ArduinoSerialInterface.cpp, SerialWifiInterface.cpp and
// examples/companion_radio/MyMesh.cpp.
package companion

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// Frame header bytes. Toward the node: '<' len_lo len_hi payload.
// From the node: '>' len_lo len_hi payload.
const (
	HeaderToNode   byte = 0x3c // '<'
	HeaderFromNode byte = 0x3e // '>'
)

// FirmwareMaxFrame is MeshCore's MAX_FRAME_SIZE.
const FirmwareMaxFrame = 176

// ErrFrameTooLarge means the node link announced a frame over the limit. The
// link is out of sync or broken and should be reopened.
var ErrFrameTooLarge = errors.New("companion: frame exceeds size limit")

// Encode wraps payload in the header for direction hdr.
func Encode(hdr byte, payload []byte) []byte {
	out := make([]byte, 3+len(payload))
	out[0] = hdr
	out[1] = byte(len(payload))
	out[2] = byte(len(payload) >> 8)
	copy(out[3:], payload)
	return out
}

// Reader reads framed payloads from a node link.
type Reader struct {
	r       *bufio.Reader
	hdr     byte
	max     int
	skipped int64
}

// NewReader reads frames that start with hdr and are at most max bytes.
func NewReader(r io.Reader, hdr byte, max int) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 1024), hdr: hdr, max: max}
}

// Skipped is the number of bytes discarded while looking for a frame header.
// USB serial nodes can print debug text between frames.
func (fr *Reader) Skipped() int64 { return fr.skipped }

// ReadFrame returns the next payload. Bytes before a header are skipped. A
// zero-length frame is returned as an empty, non-nil slice.
func (fr *Reader) ReadFrame() ([]byte, error) {
	for {
		b, err := fr.r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b == fr.hdr {
			break
		}
		fr.skipped++
	}
	var lenBuf [2]byte
	if _, err := io.ReadFull(fr.r, lenBuf[:]); err != nil {
		return nil, unexpected(err)
	}
	n := int(lenBuf[0]) | int(lenBuf[1])<<8
	if n > fr.max {
		return nil, fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, n, fr.max)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(fr.r, buf); err != nil {
		return nil, unexpected(err)
	}
	return buf, nil
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}
