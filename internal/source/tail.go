package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
)

// ErrStreamGone reports that a stream's backing file no longer exists.
//
// This is an expected condition, not a failure: assistants delete transcripts
// on a retention schedule, and a file may vanish mid-tail. Callers retire the
// stream; they must not treat it as the session having ended cleanly.
var ErrStreamGone = errors.New("source: stream no longer exists")

// fingerprintSize is how many leading bytes identify a stream's content.
//
// Large enough that two different transcripts will not collide on their first
// lines, small enough to re-read cheaply on every poll.
const fingerprintSize = 4096

// defaultMaxBytesPerRead bounds how much a single Read returns, so that the
// first pass over a large backlog does not materialize a whole file in memory.
const defaultMaxBytesPerRead = 4 << 20 // 4 MiB

// FileStreamConfig is everything a FileStream needs. A constructed FileStream
// is immediately usable (AGENT.md: configuration at construction time).
type FileStreamConfig struct {
	// ID identifies the stream. Required.
	ID StreamID
	// FS supplies filesystem access. Required.
	FS FileSystem
	// MaxBytesPerRead bounds one Read. Zero selects a sensible default.
	MaxBytesPerRead int64
}

// FileStream tails a single append-only file.
//
// It never returns a partial trailing line: a write may be observed mid-line,
// and emitting one would corrupt the archive. The incomplete tail is left for
// a later read.
type FileStream struct {
	id              StreamID
	fsys            FileSystem
	maxBytesPerRead int64
}

// NewFileStream returns a FileStream reading the file named by cfg.ID.Path.
func NewFileStream(cfg FileStreamConfig) (*FileStream, error) {
	if cfg.FS == nil {
		return nil, errors.New("source: FileStreamConfig.FS is required")
	}
	if cfg.ID.Path == "" {
		return nil, errors.New("source: FileStreamConfig.ID.Path is required")
	}
	maxRead := cfg.MaxBytesPerRead
	if maxRead <= 0 {
		maxRead = defaultMaxBytesPerRead
	}
	return &FileStream{id: cfg.ID, fsys: cfg.FS, maxBytesPerRead: maxRead}, nil
}

// ID implements Stream.
func (s *FileStream) ID() StreamID { return s.id }

// Read implements Stream. It returns the complete lines appended since from,
// and reports whether the file merely grew or was truncated or replaced.
//
// The returned error is named so that a failure to close the file is surfaced
// rather than discarded, without masking an error from the read itself.
func (s *FileStream) Read(ctx context.Context, from Cursor) (result ReadResult, err error) {
	if err := ctx.Err(); err != nil {
		return ReadResult{}, err
	}

	info, err := s.fsys.Stat(s.id.Path)
	if err != nil {
		return ReadResult{}, s.statError(err)
	}

	f, err := s.fsys.Open(s.id.Path)
	if err != nil {
		return ReadResult{}, s.statError(err)
	}
	defer func() {
		// On a read-only descriptor a close failure indicates a broken
		// descriptor rather than lost data, but it is still a fault worth
		// reporting; only report it if the read itself succeeded.
		if cerr := f.Close(); cerr != nil && err == nil {
			result, err = ReadResult{}, fmt.Errorf("source: close %s: %w", s.id.Path, cerr)
		}
	}()

	start, continuity, fingerprint, err := s.resolveStart(f, info, from)
	if err != nil {
		return ReadResult{}, err
	}

	lines, consumed, err := s.readLines(f, start, info.Size)
	if err != nil {
		return ReadResult{}, err
	}

	return ReadResult{
		Lines: lines,
		Next: Cursor{
			Offset:      start + consumed,
			Fingerprint: fingerprint,
			Identity:    info.Identity,
		},
		Continuity: continuity,
	}, nil
}

// resolveStart decides where to read from, detecting truncation and replacement.
func (s *FileStream) resolveStart(f io.ReadSeeker, info FileInfo, from Cursor) (
	start int64, continuity Continuity, fingerprint uint64, err error,
) {
	// A file shorter than where we left off cannot be the same content.
	if info.Size < from.Offset {
		fp, err := s.fingerprint(f, info.Size)
		return 0, ContinuityTruncated, fp, err
	}

	// Identity is the strongest signal, when the platform supplies it.
	identitiesKnown := !from.Identity.IsZero() && !info.Identity.IsZero()
	if identitiesKnown && from.Identity != info.Identity {
		fp, err := s.fingerprint(f, info.Size)
		return 0, ContinuityReplaced, fp, err
	}

	// A fresh cursor still needs a fingerprint recorded for next time.
	if from.Offset == 0 && from.Fingerprint == 0 {
		fp, err := s.fingerprint(f, info.Size)
		return 0, ContinuityAppended, fp, err
	}

	// Trust a matching identity and avoid re-hashing the head every poll.
	if identitiesKnown && from.Identity == info.Identity {
		return from.Offset, ContinuityAppended, from.Fingerprint, nil
	}

	// No usable identity: fall back to comparing content.
	fp, err := s.fingerprint(f, info.Size)
	if err != nil {
		return 0, ContinuityAppended, 0, err
	}
	if from.Fingerprint != 0 && fp != from.Fingerprint {
		return 0, ContinuityReplaced, fp, nil
	}
	return from.Offset, ContinuityAppended, fp, nil
}

// fingerprint hashes the stream's leading bytes.
func (s *FileStream) fingerprint(f io.ReadSeeker, size int64) (uint64, error) {
	n := int64(fingerprintSize)
	if size < n {
		n = size
	}
	if n == 0 {
		return 0, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("source: seek %s: %w", s.id.Path, err)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(f, buf); err != nil {
		return 0, fmt.Errorf("source: fingerprint %s: %w", s.id.Path, err)
	}
	h := fnv.New64a()
	_, _ = h.Write(buf)
	return h.Sum64(), nil
}

// readLines reads from start and splits out complete lines only.
func (s *FileStream) readLines(f io.ReadSeeker, start, size int64) ([]RawLine, int64, error) {
	avail := size - start
	if avail <= 0 {
		return nil, 0, nil
	}
	if avail > s.maxBytesPerRead {
		avail = s.maxBytesPerRead
	}

	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, 0, fmt.Errorf("source: seek %s: %w", s.id.Path, err)
	}
	buf := make([]byte, avail)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, 0, fmt.Errorf("source: read %s: %w", s.id.Path, err)
	}
	buf = buf[:n]

	var (
		lines    []RawLine
		consumed int64
	)
	for len(buf) > 0 {
		idx := bytes.IndexByte(buf, '\n')
		if idx < 0 {
			// Trailing partial line: leave it for the next read.
			break
		}
		data := make([]byte, idx)
		copy(data, buf[:idx])
		lines = append(lines, RawLine{
			Stream: s.id,
			Offset: start + consumed,
			Data:   data,
		})
		advance := int64(idx + 1)
		consumed += advance
		buf = buf[advance:]
	}
	return lines, consumed, nil
}

// statError normalizes a missing file into ErrStreamGone.
func (s *FileStream) statError(err error) error {
	if errors.Is(err, io.EOF) {
		return fmt.Errorf("source: %s: %w", s.id.Path, ErrStreamGone)
	}
	if isNotExist(err) {
		return fmt.Errorf("source: %s: %w", s.id.Path, ErrStreamGone)
	}
	return fmt.Errorf("source: stat %s: %w", s.id.Path, err)
}
