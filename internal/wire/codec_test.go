package wire_test

import (
	"bytes"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/wire"
)

func TestEncodeDecodeRoundTripPreservesLinesVerbatim(t *testing.T) {
	// Includes content that a naive encoder would mangle: embedded quotes,
	// unicode, and a line that is itself JSON.
	inputs := []string{
		`{"type":"assistant","message":{"usage":{"output_tokens":42}}}`,
		`plain text, not json at all`,
		`{"emoji":"ok","quote":"he said \"hi\"","tab":"a\tb"}`,
		``,
	}
	records := make([]wire.Record, len(inputs))
	for i, in := range inputs {
		records[i] = wire.NewRecord("claudecode", "/logs/a.jsonl", int64(i*10), []byte(in))
	}

	var buf bytes.Buffer
	header := wire.Header{Device: wire.Device{Hostname: "test-host", OS: "linux"}}
	if err := wire.EncodeBatch(&buf, header, records); err != nil {
		t.Fatalf("EncodeBatch: %v", err)
	}

	var got []wire.Record
	gotHeader, err := wire.DecodeBatch(&buf, func(r wire.Record) error {
		got = append(got, r)
		return nil
	})
	if err != nil {
		t.Fatalf("DecodeBatch: %v", err)
	}

	if gotHeader.Device.Hostname != "test-host" {
		t.Errorf("hostname = %q, want test-host", gotHeader.Device.Hostname)
	}
	if gotHeader.Count != len(records) {
		t.Errorf("header count = %d, want %d", gotHeader.Count, len(records))
	}
	if len(got) != len(records) {
		t.Fatalf("decoded %d records, want %d", len(got), len(records))
	}
	for i := range records {
		if got[i].Data != inputs[i] {
			t.Errorf("record %d data = %q, want %q", i, got[i].Data, inputs[i])
		}
		if got[i].Hash != records[i].Hash {
			t.Errorf("record %d hash changed in transit", i)
		}
		if got[i].Offset != records[i].Offset {
			t.Errorf("record %d offset = %d, want %d", i, got[i].Offset, records[i].Offset)
		}
	}
}

func TestNewRecordHashesContent(t *testing.T) {
	a := wire.NewRecord("s", "/p", 0, []byte("same"))
	b := wire.NewRecord("s", "/other", 99, []byte("same"))
	if a.Hash != b.Hash {
		t.Error("identical content must hash identically regardless of location")
	}
	c := wire.NewRecord("s", "/p", 0, []byte("different"))
	if a.Hash == c.Hash {
		t.Error("different content must hash differently")
	}
}

func TestDecodeRejectsEmptyBody(t *testing.T) {
	var buf bytes.Buffer
	if err := wire.EncodeBatch(&buf, wire.Header{}, nil); err != nil {
		t.Fatalf("EncodeBatch: %v", err)
	}
	// A header with no records is valid and must decode cleanly.
	if _, err := wire.DecodeBatch(&buf, func(wire.Record) error { return nil }); err != nil {
		t.Fatalf("DecodeBatch of empty batch: %v", err)
	}
	if _, err := wire.DecodeBatch(bytes.NewReader(nil), func(wire.Record) error { return nil }); err == nil {
		t.Error("expected an error decoding a non-gzip body")
	}
}
