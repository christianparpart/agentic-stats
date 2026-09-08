package cursor_test

import (
	"path/filepath"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/cursor"
	"github.com/christianparpart/agentic-stats/internal/source"
)

func openStore(t *testing.T) *cursor.BoltStore {
	t.Helper()
	s, err := cursor.OpenBoltStore(cursor.BoltStoreConfig{
		Path: filepath.Join(t.TempDir(), "cursors.db"),
	})
	if err != nil {
		t.Fatalf("OpenBoltStore: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func TestUnknownStreamStartsFromZero(t *testing.T) {
	s := openStore(t)
	got, err := s.Load(cursor.Key{Source: "claudecode", Path: "/never/seen.jsonl"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != (source.Cursor{}) {
		t.Errorf("cursor = %+v, want the zero cursor", got)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s := openStore(t)
	key := cursor.Key{Source: "claudecode", Path: "/logs/a.jsonl"}
	want := source.Cursor{
		Offset:      1 << 40, // deliberately beyond 32 bits
		Fingerprint: 0xDEADBEEFCAFEF00D,
		Identity:    source.FileIdentity{Device: 66, Serial: 1234567890123},
	}
	if err := s.Save(key, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Errorf("round trip changed the cursor:\n got %+v\nwant %+v", got, want)
	}
}

func TestSaveOverwritesAndDeleteResets(t *testing.T) {
	s := openStore(t)
	key := cursor.Key{Source: "claudecode", Path: "/logs/a.jsonl"}

	if err := s.Save(key, source.Cursor{Offset: 10}); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := s.Save(key, source.Cursor{Offset: 20}); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	got, err := s.Load(key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Offset != 20 {
		t.Errorf("offset = %d, want 20", got.Offset)
	}

	if err := s.Delete(key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	after, err := s.Load(key)
	if err != nil {
		t.Fatalf("Load after Delete: %v", err)
	}
	if after != (source.Cursor{}) {
		t.Errorf("after delete = %+v, want the zero cursor", after)
	}
}

// Two streams differing only by source must not share a position.
func TestKeysAreDistinctPerSource(t *testing.T) {
	s := openStore(t)
	a := cursor.Key{Source: "claudecode", Path: "/logs/x.jsonl"}
	b := cursor.Key{Source: "somethingelse", Path: "/logs/x.jsonl"}

	if err := s.Save(a, source.Cursor{Offset: 111}); err != nil {
		t.Fatalf("Save a: %v", err)
	}
	got, err := s.Load(b)
	if err != nil {
		t.Fatalf("Load b: %v", err)
	}
	if got.Offset != 0 {
		t.Errorf("b offset = %d, want 0: keys must not collide across sources", got.Offset)
	}
}

func TestOpenRequiresAPath(t *testing.T) {
	if _, err := cursor.OpenBoltStore(cursor.BoltStoreConfig{}); err == nil {
		t.Error("expected an error when Path is empty")
	}
}

// A cursor must survive process restart; that is the entire point.
func TestCursorsPersistAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cursors.db")
	key := cursor.Key{Source: "claudecode", Path: "/logs/a.jsonl"}

	first, err := cursor.OpenBoltStore(cursor.BoltStoreConfig{Path: path})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := first.Save(key, source.Cursor{Offset: 4242}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := cursor.OpenBoltStore(cursor.BoltStoreConfig{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = second.Close() }() // test cleanup; failure cannot affect the assertion

	got, err := second.Load(key)
	if err != nil {
		t.Fatalf("Load after reopen: %v", err)
	}
	if got.Offset != 4242 {
		t.Errorf("offset after reopen = %d, want 4242", got.Offset)
	}
}
