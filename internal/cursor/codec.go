package cursor

import (
	"encoding/binary"
	"fmt"

	"github.com/christianparpart/agentic-stats/internal/source"
)

// encodedCursorSize is the fixed on-disk width of an encoded cursor:
// offset, fingerprint, device and serial, each 8 bytes.
const encodedCursorSize = 32

// encodeCursor serializes a cursor to a fixed-width record.
//
// A hand-rolled encoding rather than JSON: the record is fixed-size, written on
// every batch, and never read by anything but this package.
func encodeCursor(c source.Cursor) []byte {
	buf := make([]byte, encodedCursorSize)
	binary.BigEndian.PutUint64(buf[0:8], uint64(c.Offset))
	binary.BigEndian.PutUint64(buf[8:16], c.Fingerprint)
	binary.BigEndian.PutUint64(buf[16:24], c.Identity.Device)
	binary.BigEndian.PutUint64(buf[24:32], c.Identity.Serial)
	return buf
}

// decodeCursor restores a cursor written by encodeCursor.
func decodeCursor(raw []byte) (source.Cursor, error) {
	if len(raw) != encodedCursorSize {
		return source.Cursor{}, fmt.Errorf("malformed cursor record: %d bytes, want %d",
			len(raw), encodedCursorSize)
	}
	return source.Cursor{
		Offset:      int64(binary.BigEndian.Uint64(raw[0:8])),
		Fingerprint: binary.BigEndian.Uint64(raw[8:16]),
		Identity: source.FileIdentity{
			Device: binary.BigEndian.Uint64(raw[16:24]),
			Serial: binary.BigEndian.Uint64(raw[24:32]),
		},
	}, nil
}
