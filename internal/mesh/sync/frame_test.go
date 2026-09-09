package sync

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

// The bug this replaced: a json.Decoder over an io.LimitReader bounds the
// whole connection, so a long exchange hit EOF part-way and looked like the
// peer had hung up. Many frames over one stream must simply work.
func TestManyFramesOverOneStream(t *testing.T) {
	var buf bytes.Buffer
	const count = 500
	for i := range count {
		m := message{Kind: kindRecords, Records: []wireRecord{{
			OriginID: "origin", Seq: int64(i), Sealed: bytes.Repeat([]byte("x"), 4096),
		}}}
		if err := writeFrame(&buf, m); err != nil {
			t.Fatalf("writeFrame %d: %v", i, err)
		}
	}
	if err := writeFrame(&buf, message{Kind: kindDone}); err != nil {
		t.Fatalf("writeFrame done: %v", err)
	}

	for i := range count {
		m, err := readFrame(&buf)
		if err != nil {
			t.Fatalf("readFrame %d: %v", i, err)
		}
		if m.Kind != kindRecords || len(m.Records) != 1 {
			t.Fatalf("frame %d decoded as %+v", i, m)
		}
		if m.Records[0].Seq != int64(i) {
			t.Errorf("frame %d carried seq %d", i, m.Records[0].Seq)
		}
	}
	last, err := readFrame(&buf)
	if err != nil {
		t.Fatalf("readFrame done: %v", err)
	}
	if last.Kind != kindDone {
		t.Errorf("final frame = %q, want done", last.Kind)
	}
	if _, err := readFrame(&buf); err == nil {
		t.Error("expected EOF after the last frame")
	}
}

// An oversized length must be refused before anything is allocated.
func TestOversizedFrameIsRefusedBeforeAllocation(t *testing.T) {
	var header [frameHeaderBytes]byte
	binary.BigEndian.PutUint32(header[:], ^uint32(0))
	// Only the header is present: if the reader tried to allocate and read the
	// announced four gigabytes it would block or panic rather than return.
	_, err := readFrame(bytes.NewReader(header[:]))
	if err == nil {
		t.Fatal("an oversized frame was accepted")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error = %v, want it to name the limit", err)
	}
}

func TestTruncatedFrameIsAnError(t *testing.T) {
	var buf bytes.Buffer
	if err := writeFrame(&buf, message{Kind: kindDone}); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	truncated := buf.Bytes()[:buf.Len()-1]
	if _, err := readFrame(bytes.NewReader(truncated)); err == nil {
		t.Error("a truncated frame was accepted")
	}
}

func TestEmptyStreamReportsEOF(t *testing.T) {
	if _, err := readFrame(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want io.EOF so the caller can treat it as a clean hangup", err)
	}
}
