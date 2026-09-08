// Package source discovers and tails append-only streams produced by AI coding
// assistants.
//
// It deliberately knows nothing about what the lines mean. Interpretation lives
// in internal/derive, server-side, so that collection keeps working when a log
// format changes underneath us. See AGENT.md.
package source

import (
	"context"
	"io"
	"io/fs"
	"time"
)

// FileIdentity identifies a file independently of its path, so that a rename,
// a replacement, or a truncation is distinguishable from an ordinary append.
//
// On Unix this is the device and inode; on Windows, the volume serial and file
// index. A zero FileIdentity means the platform could not supply one, in which
// case callers must fall back to content fingerprinting alone.
type FileIdentity struct {
	Device uint64
	Serial uint64
}

// IsZero reports whether the identity is unavailable.
func (id FileIdentity) IsZero() bool { return id.Device == 0 && id.Serial == 0 }

// FileInfo is the subset of file metadata the collector needs.
type FileInfo struct {
	Size     int64
	ModTime  time.Time
	Identity FileIdentity
}

// FileSystem is the filesystem access the collector requires.
//
// It is an interface so that tailing can be tested without touching a real
// disk, and so truncation, replacement and mid-write reads can be exercised
// deterministically (AGENT.md: dependency injection).
type FileSystem interface {
	// Open opens a file for reading at arbitrary offsets.
	Open(name string) (io.ReadSeekCloser, error)
	// Stat reports the file's current size, modification time and identity.
	Stat(name string) (FileInfo, error)
	// WalkDir walks the tree rooted at root, in the manner of fs.WalkDir.
	WalkDir(root string, fn fs.WalkDirFunc) error
}

// Clock supplies the current time. Injected so that time-dependent behaviour
// is testable (AGENT.md: dependency injection).
type Clock interface {
	Now() time.Time
}

// StreamID identifies an append-only stream stably across daemon restarts.
//
// Path alone is not sufficient: a session transcript may be copied to a second
// directory and continue appending there, and the retention reaper may delete
// and later recreate a path. Identity disambiguates those cases.
type StreamID struct {
	// Source names the adapter that discovered the stream, e.g. "claudecode".
	Source string
	// Path is the absolute path at discovery time.
	Path string
	// Identity is the path-independent file identity, where available.
	Identity FileIdentity
}

// RawLine is one uninterpreted line of a stream, exactly as it appeared on disk.
//
// The bytes are never parsed here. Preserving them verbatim is what allows the
// archive to be re-derived years later against a parser that does not exist yet.
type RawLine struct {
	Stream StreamID
	// Offset is the byte offset of the first byte of the line.
	Offset int64
	// Data is the line content, without its terminating newline.
	Data []byte
}

// Cursor is a resumable position within a stream.
//
// The zero Cursor means "start from the beginning".
type Cursor struct {
	// Offset is the byte offset immediately after the last complete line read.
	Offset int64
	// Fingerprint is a hash of the stream's leading bytes, used to detect that
	// a file was replaced rather than appended to.
	Fingerprint uint64
	// Identity is the file identity observed when the cursor was written.
	Identity FileIdentity
}

// Continuity describes how a read related to the cursor it was given.
//
// It is a named type rather than a bool because the cases are not two: a read
// may continue normally, or resume after the file was truncated, or after it
// was replaced entirely — and each calls for different downstream handling
// (AGENT.md: named types over bool).
type Continuity uint8

const (
	// ContinuityAppended means the stream grew and was read from the cursor.
	// It is the zero value, so a zero-valued result reads as the ordinary case.
	ContinuityAppended Continuity = iota
	// ContinuityTruncated means the stream is shorter than the cursor offset.
	ContinuityTruncated
	// ContinuityReplaced means the stream's identity or leading bytes changed.
	ContinuityReplaced
)

// String implements fmt.Stringer.
func (c Continuity) String() string {
	switch c {
	case ContinuityAppended:
		return "appended"
	case ContinuityTruncated:
		return "truncated"
	case ContinuityReplaced:
		return "replaced"
	default:
		return "unknown"
	}
}

// ReadResult is the outcome of reading a stream from a cursor.
type ReadResult struct {
	// Lines are the complete lines read, in file order. A trailing partial
	// line is never returned; it is left for a subsequent read.
	Lines []RawLine
	// Next is the cursor to supply to the following read.
	Next Cursor
	// Continuity reports how this read related to the supplied cursor.
	Continuity Continuity
}

// Stream is a single append-only file being followed.
type Stream interface {
	// ID returns the stream's stable identity.
	ID() StreamID
	// Read returns complete lines appended since the cursor.
	Read(ctx context.Context, from Cursor) (ReadResult, error)
}

// Source discovers the streams belonging to one assistant or tool.
//
// Adding support for another assistant means implementing this interface and
// registering it — not editing the agent (AGENT.md: data-driven design).
type Source interface {
	// Name is the adapter's stable identifier, recorded on every line.
	Name() string
	// Discover enumerates the streams currently visible.
	Discover(ctx context.Context) ([]Stream, error)
}
