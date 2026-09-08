// Package wire defines what travels between the collector and the server.
//
// Records carry raw lines verbatim as text. They are not parsed here, and the
// server stores them before interpreting them, so a format the collector has
// never seen still reaches the archive intact.
package wire

import (
	"crypto/sha256"
	"encoding/hex"
)

// ContentType is the media type of an ingest body.
const ContentType = "application/x-ndjson"

// Record is one collected line as transmitted.
type Record struct {
	// Source is the adapter that produced the line, e.g. "claudecode".
	Source string `json:"source"`
	// Path is the absolute path of the stream on the originating machine.
	Path string `json:"path"`
	// Offset is the byte offset of the line within that stream.
	Offset int64 `json:"offset"`
	// Hash is the SHA-256 of Data, hex encoded. It gives uuid-less lines a
	// stable identity and lets the server verify nothing was mangled.
	Hash string `json:"hash"`
	// Data is the line exactly as it appeared on disk, without its newline.
	Data string `json:"data"`
}

// NewRecord builds a Record, computing its content hash.
func NewRecord(sourceName, path string, offset int64, data []byte) Record {
	sum := sha256.Sum256(data)
	return Record{
		Source: sourceName,
		Path:   path,
		Offset: offset,
		Hash:   hex.EncodeToString(sum[:]),
		Data:   string(data),
	}
}

// Device describes the machine a batch came from.
//
// It travels with every batch so that a newly enrolled machine is described
// without a separate registration round-trip, and so changes (an OS upgrade, a
// timezone move) are picked up automatically.
type Device struct {
	// Hostname is the machine's name, for display.
	Hostname string `json:"hostname"`
	// OS and Arch are the Go platform identifiers.
	OS   string `json:"os"`
	Arch string `json:"arch"`
	// Timezone is the IANA zone name, so "busy hours" can be rendered in the
	// machine's local time rather than smeared across zones.
	Timezone string `json:"timezone"`
	// AgentVersion identifies the collector build.
	AgentVersion string `json:"agent_version"`
}

// Header is the first line of an ingest body.
type Header struct {
	// Device describes the originating machine.
	Device Device `json:"device"`
	// Count is the number of records that follow, for early validation.
	Count int `json:"count"`
}

// IngestResult is the server's report on a batch.
type IngestResult struct {
	// Received is how many records the server read.
	Received int `json:"received"`
	// Stored is how many were new.
	Stored int `json:"stored"`
	// Duplicates is how many were already present. A healthy retry after a
	// failed upload reports duplicates rather than an error.
	Duplicates int `json:"duplicates"`
}
