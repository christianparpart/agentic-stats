package sync

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Messages are length-prefixed rather than a bare JSON stream.
//
// The obvious alternative -- a json.Decoder over an io.LimitReader -- is a
// trap: LimitReader bounds the *connection*, not the message, so a long
// exchange hits EOF part-way through and looks like the peer hung up. A
// four-byte prefix bounds each message individually, which is what was
// actually wanted.
const frameHeaderBytes = 4

// writeFrame encodes one message with its length prefix.
func writeFrame(w io.Writer, m message) error {
	body, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("sync: encode message: %w", err)
	}
	if len(body) > maxMessageBytes {
		return fmt.Errorf("sync: message of %d bytes exceeds the limit of %d",
			len(body), maxMessageBytes)
	}
	var header [frameHeaderBytes]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	if _, err := w.Write(header[:]); err != nil {
		return fmt.Errorf("sync: write frame header: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("sync: write frame: %w", err)
	}
	return nil
}

// readFrame decodes one message, refusing anything oversized before it is read.
func readFrame(r io.Reader) (message, error) {
	var header [frameHeaderBytes]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return message{}, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size > maxMessageBytes {
		// Refused before allocating, so a hostile length cannot exhaust memory
		// even though the peer holds the mesh key.
		return message{}, fmt.Errorf("sync: peer announced a %d byte message, limit is %d",
			size, maxMessageBytes)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return message{}, fmt.Errorf("sync: read frame: %w", err)
	}
	var m message
	if err := json.Unmarshal(body, &m); err != nil {
		return message{}, fmt.Errorf("sync: decode message: %w", err)
	}
	return m, nil
}
