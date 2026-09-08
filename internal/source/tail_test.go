package source

import (
	"context"
	"errors"
	"testing"
)

const testPath = "/logs/session.jsonl"

func newTestStream(t *testing.T, m *memFS) *FileStream {
	t.Helper()
	s, err := NewFileStream(FileStreamConfig{
		ID: StreamID{Source: "test", Path: testPath},
		FS: m,
	})
	if err != nil {
		t.Fatalf("NewFileStream: %v", err)
	}
	return s
}

func lineData(lines []RawLine) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = string(l.Data)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFileStreamRead(t *testing.T) {
	id1 := FileIdentity{Device: 1, Serial: 100}

	tests := []struct {
		name           string
		content        string
		identity       FileIdentity
		from           Cursor
		wantLines      []string
		wantContinuity Continuity
		wantNextOffset int64
	}{
		{
			name:           "reads complete lines from the start",
			content:        "alpha\nbeta\ngamma\n",
			identity:       id1,
			wantLines:      []string{"alpha", "beta", "gamma"},
			wantContinuity: ContinuityAppended,
			wantNextOffset: 17,
		},
		{
			// A write can be observed mid-line; emitting it would corrupt the archive.
			name:           "withholds a trailing partial line",
			content:        "alpha\nbeta\ngam",
			identity:       id1,
			wantLines:      []string{"alpha", "beta"},
			wantContinuity: ContinuityAppended,
			wantNextOffset: 11,
		},
		{
			name:           "empty file yields nothing",
			content:        "",
			identity:       id1,
			wantLines:      nil,
			wantContinuity: ContinuityAppended,
			wantNextOffset: 0,
		},
		{
			name:           "a single unterminated line is withheld entirely",
			content:        "no newline here",
			identity:       id1,
			wantLines:      nil,
			wantContinuity: ContinuityAppended,
			wantNextOffset: 0,
		},
		{
			name:     "resumes from a cursor without repeating",
			content:  "alpha\nbeta\ngamma\n",
			identity: id1,
			from: Cursor{
				Offset:      11,
				Fingerprint: fingerprintOf("alpha\nbeta\ngamma\n"),
				Identity:    id1,
			},
			wantLines:      []string{"gamma"},
			wantContinuity: ContinuityAppended,
			wantNextOffset: 17,
		},
		{
			// The retention reaper and worktree relocation both produce this.
			name:     "detects truncation and restarts",
			content:  "short\n",
			identity: id1,
			from: Cursor{
				Offset:      9000,
				Fingerprint: 12345,
				Identity:    id1,
			},
			wantLines:      []string{"short"},
			wantContinuity: ContinuityTruncated,
			wantNextOffset: 6,
		},
		{
			name:     "detects replacement by changed identity",
			content:  "brand\nnew\n",
			identity: FileIdentity{Device: 1, Serial: 999},
			from: Cursor{
				Offset:      6,
				Fingerprint: fingerprintOf("brand\nnew\n"),
				Identity:    id1,
			},
			wantLines:      []string{"brand", "new"},
			wantContinuity: ContinuityReplaced,
			wantNextOffset: 10,
		},
		{
			// Platforms that cannot supply an identity fall back to content.
			name:     "detects replacement by changed fingerprint when identity is unavailable",
			content:  "different\ncontent\n",
			identity: FileIdentity{},
			from: Cursor{
				Offset:      6,
				Fingerprint: 424242,
			},
			wantLines:      []string{"different", "content"},
			wantContinuity: ContinuityReplaced,
			wantNextOffset: 18,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newMemFS()
			m.put(testPath, tc.content, tc.identity)
			got, err := newTestStream(t, m).Read(context.Background(), tc.from)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if want := tc.wantLines; !equalStrings(lineData(got.Lines), want) {
				t.Errorf("lines = %q, want %q", lineData(got.Lines), want)
			}
			if got.Continuity != tc.wantContinuity {
				t.Errorf("continuity = %s, want %s", got.Continuity, tc.wantContinuity)
			}
			if got.Next.Offset != tc.wantNextOffset {
				t.Errorf("next offset = %d, want %d", got.Next.Offset, tc.wantNextOffset)
			}
		})
	}
}

// A partial line must be completed, not dropped, once the writer finishes it.
func TestFileStreamCompletesPartialLineOnNextRead(t *testing.T) {
	m := newMemFS()
	m.put(testPath, "alpha\nbet", FileIdentity{Device: 1, Serial: 1})
	s := newTestStream(t, m)

	first, err := s.Read(context.Background(), Cursor{})
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if want := []string{"alpha"}; !equalStrings(lineData(first.Lines), want) {
		t.Fatalf("first lines = %q, want %q", lineData(first.Lines), want)
	}

	m.append(testPath, "a\ngamma\n")

	second, err := s.Read(context.Background(), first.Next)
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if want := []string{"beta", "gamma"}; !equalStrings(lineData(second.Lines), want) {
		t.Errorf("second lines = %q, want %q", lineData(second.Lines), want)
	}
	if second.Continuity != ContinuityAppended {
		t.Errorf("continuity = %s, want appended", second.Continuity)
	}
}

// Repeated reads with no writer activity must produce nothing at all.
func TestFileStreamIsIdempotentWhenIdle(t *testing.T) {
	m := newMemFS()
	m.put(testPath, "alpha\nbeta\n", FileIdentity{Device: 1, Serial: 1})
	s := newTestStream(t, m)

	first, err := s.Read(context.Background(), Cursor{})
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	for i := range 3 {
		again, err := s.Read(context.Background(), first.Next)
		if err != nil {
			t.Fatalf("idle Read %d: %v", i, err)
		}
		if len(again.Lines) != 0 {
			t.Fatalf("idle Read %d returned %d lines, want 0", i, len(again.Lines))
		}
		if again.Next.Offset != first.Next.Offset {
			t.Fatalf("idle Read %d moved cursor to %d, want %d", i, again.Next.Offset, first.Next.Offset)
		}
	}
}

// Transcripts are deleted on a retention schedule; a vanished file is expected.
func TestFileStreamReportsGoneRatherThanFailing(t *testing.T) {
	m := newMemFS()
	m.put(testPath, "alpha\n", FileIdentity{Device: 1, Serial: 1})
	s := newTestStream(t, m)
	m.remove(testPath)

	_, err := s.Read(context.Background(), Cursor{})
	if !errors.Is(err, ErrStreamGone) {
		t.Fatalf("err = %v, want ErrStreamGone", err)
	}
}

// A large backlog must not be materialized in one read.
func TestFileStreamBoundsReadSize(t *testing.T) {
	m := newMemFS()
	m.put(testPath, "aaaa\nbbbb\ncccc\ndddd\n", FileIdentity{Device: 1, Serial: 1})
	s, err := NewFileStream(FileStreamConfig{
		ID:              StreamID{Source: "test", Path: testPath},
		FS:              m,
		MaxBytesPerRead: 10,
	})
	if err != nil {
		t.Fatalf("NewFileStream: %v", err)
	}

	got, err := s.Read(context.Background(), Cursor{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if want := []string{"aaaa", "bbbb"}; !equalStrings(lineData(got.Lines), want) {
		t.Errorf("lines = %q, want %q", lineData(got.Lines), want)
	}

	rest, err := s.Read(context.Background(), got.Next)
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if want := []string{"cccc", "dddd"}; !equalStrings(lineData(rest.Lines), want) {
		t.Errorf("remaining lines = %q, want %q", lineData(rest.Lines), want)
	}
}

func TestNewFileStreamRequiresItsDependencies(t *testing.T) {
	if _, err := NewFileStream(FileStreamConfig{ID: StreamID{Path: testPath}}); err == nil {
		t.Error("expected an error when FS is missing")
	}
	if _, err := NewFileStream(FileStreamConfig{FS: newMemFS()}); err == nil {
		t.Error("expected an error when Path is missing")
	}
}
