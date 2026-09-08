package wire

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
)

// maxLineBytes bounds a single decoded NDJSON line, so a malformed or hostile
// body cannot exhaust server memory.
const maxLineBytes = 32 << 20 // 32 MiB

// EncodeBatch writes a gzipped NDJSON body: a Header line followed by one line
// per record.
func EncodeBatch(w io.Writer, header Header, records []Record) error {
	zw := gzip.NewWriter(w)
	enc := json.NewEncoder(zw)

	header.Count = len(records)
	if err := enc.Encode(header); err != nil {
		return fmt.Errorf("wire: encode header: %w", err)
	}
	for i := range records {
		if err := enc.Encode(records[i]); err != nil {
			return fmt.Errorf("wire: encode record %d: %w", i, err)
		}
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("wire: finish body: %w", err)
	}
	return nil
}

// DecodeBatch reads a body written by EncodeBatch.
//
// It streams: records are handed to fn one at a time rather than accumulated,
// so a large batch does not have to fit in memory twice.
func DecodeBatch(r io.Reader, fn func(Record) error) (Header, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return Header{}, fmt.Errorf("wire: open gzip: %w", err)
	}
	// Closing a gzip *reader* only releases buffers; there is nothing to flush
	// and no error that can invalidate what was already decoded.
	defer func() { _ = zr.Close() }()

	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	if !sc.Scan() {
		if serr := sc.Err(); serr != nil {
			return Header{}, fmt.Errorf("wire: read header: %w", serr)
		}
		return Header{}, fmt.Errorf("wire: empty body")
	}
	var header Header
	if err := json.Unmarshal(sc.Bytes(), &header); err != nil {
		return Header{}, fmt.Errorf("wire: decode header: %w", err)
	}

	for sc.Scan() {
		var rec Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return header, fmt.Errorf("wire: decode record: %w", err)
		}
		if err := fn(rec); err != nil {
			return header, err
		}
	}
	if err := sc.Err(); err != nil {
		return header, fmt.Errorf("wire: read body: %w", err)
	}
	return header, nil
}
