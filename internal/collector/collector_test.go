package collector_test

import (
	"context"
	"errors"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/collector"
	"github.com/christianparpart/agentic-stats/internal/cursor"
	"github.com/christianparpart/agentic-stats/internal/source"
	"github.com/christianparpart/agentic-stats/internal/source/claudecode"
	"github.com/christianparpart/agentic-stats/internal/source/sourcetest"
	"github.com/christianparpart/agentic-stats/internal/wire"
)

const home = "/home/user"

// fakeUploader records what it was given and can be made to fail.
type fakeUploader struct {
	batches [][]wire.Record
	err     error
}

func (f *fakeUploader) Ingest(_ context.Context, records []wire.Record) (wire.IngestResult, error) {
	if f.err != nil {
		return wire.IngestResult{}, f.err
	}
	cp := make([]wire.Record, len(records))
	copy(cp, records)
	f.batches = append(f.batches, cp)
	return wire.IngestResult{Received: len(records), Stored: len(records)}, nil
}

func (f *fakeUploader) allData() []string {
	var out []string
	for _, b := range f.batches {
		for _, r := range b {
			out = append(out, r.Data)
		}
	}
	return out
}

func newCollector(t *testing.T, m *sourcetest.MapFS, up collector.Uploader, cur cursor.Store) *collector.Collector {
	t.Helper()
	src, err := claudecode.New(claudecode.Config{FS: m, Home: home, Platform: "linux"})
	if err != nil {
		t.Fatalf("claudecode.New: %v", err)
	}
	c, err := collector.New(collector.Config{
		Sources:  []source.Source{src},
		Cursors:  cur,
		Uploader: up,
	})
	if err != nil {
		t.Fatalf("collector.New: %v", err)
	}
	return c
}

func equal(a, b []string) bool {
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

func TestCollectUploadsEveryLineOnce(t *testing.T) {
	m := sourcetest.NewMapFS()
	m.Put(home+"/.claude/projects/-p/s.jsonl", "one\ntwo\nthree\n")

	up := &fakeUploader{}
	c := newCollector(t, m, up, cursor.NewMemoryStore())

	stats, err := c.CollectOnce(context.Background())
	if err != nil {
		t.Fatalf("CollectOnce: %v", err)
	}
	if want := []string{"one", "two", "three"}; !equal(up.allData(), want) {
		t.Errorf("uploaded %v, want %v", up.allData(), want)
	}
	if stats.Lines != 3 {
		t.Errorf("Lines = %d, want 3", stats.Lines)
	}
}

// The central durability property: a second pass with no new data must upload
// nothing at all.
func TestCollectIsIdempotentAcrossPasses(t *testing.T) {
	m := sourcetest.NewMapFS()
	m.Put(home+"/.claude/projects/-p/s.jsonl", "one\ntwo\n")

	up := &fakeUploader{}
	store := cursor.NewMemoryStore()
	c := newCollector(t, m, up, store)

	if _, err := c.CollectOnce(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	firstCount := len(up.allData())

	for i := range 3 {
		stats, err := c.CollectOnce(context.Background())
		if err != nil {
			t.Fatalf("idle pass %d: %v", i, err)
		}
		if stats.Lines != 0 {
			t.Fatalf("idle pass %d uploaded %d lines, want 0", i, stats.Lines)
		}
	}
	if len(up.allData()) != firstCount {
		t.Errorf("idle passes changed upload count: %d, want %d", len(up.allData()), firstCount)
	}

	// New data appended later must be picked up, and only that data.
	m.Append(home+"/.claude/projects/-p/s.jsonl", "three\n")
	if _, err := c.CollectOnce(context.Background()); err != nil {
		t.Fatalf("pass after append: %v", err)
	}
	if want := []string{"one", "two", "three"}; !equal(up.allData(), want) {
		t.Errorf("uploaded %v, want %v", up.allData(), want)
	}
}

// A failed upload must not advance the cursor, so the data is re-sent.
func TestCursorDoesNotAdvanceWhenUploadFails(t *testing.T) {
	m := sourcetest.NewMapFS()
	m.Put(home+"/.claude/projects/-p/s.jsonl", "one\ntwo\n")

	failing := &fakeUploader{err: errors.New("network down")}
	store := cursor.NewMemoryStore()
	c := newCollector(t, m, failing, store)

	if _, err := c.CollectOnce(context.Background()); err == nil {
		t.Fatal("expected an error when the upload fails")
	}

	// Recover: the same lines must be delivered, none skipped.
	up := &fakeUploader{}
	c2 := newCollector(t, m, up, store)
	if _, err := c2.CollectOnce(context.Background()); err != nil {
		t.Fatalf("recovery pass: %v", err)
	}
	if want := []string{"one", "two"}; !equal(up.allData(), want) {
		t.Errorf("after recovery uploaded %v, want %v", up.allData(), want)
	}
}

// A truncated or replaced file must be re-read from the beginning.
func TestCollectRestartsAfterReplacement(t *testing.T) {
	path := home + "/.claude/projects/-p/s.jsonl"
	m := sourcetest.NewMapFS()
	m.Put(path, "one\ntwo\n")

	up := &fakeUploader{}
	store := cursor.NewMemoryStore()
	c := newCollector(t, m, up, store)
	if _, err := c.CollectOnce(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	// Put assigns a fresh identity, as a replaced file would have.
	m.Put(path, "fresh\n")

	stats, err := c.CollectOnce(context.Background())
	if err != nil {
		t.Fatalf("pass after replacement: %v", err)
	}
	if stats.Restarted != 1 {
		t.Errorf("Restarted = %d, want 1", stats.Restarted)
	}
	if want := []string{"one", "two", "fresh"}; !equal(up.allData(), want) {
		t.Errorf("uploaded %v, want %v", up.allData(), want)
	}
}

// A vanished transcript is routine, not a failure.
func TestCollectToleratesVanishedStreams(t *testing.T) {
	m := sourcetest.NewMapFS()
	m.Put(home+"/.claude/projects/-p/s.jsonl", "one\n")

	up := &fakeUploader{}
	c := newCollector(t, m, up, cursor.NewMemoryStore())
	if _, err := c.CollectOnce(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	m.Remove(home + "/.claude/projects/-p/s.jsonl")
	m.Put(home+"/.claude/projects/-p/other.jsonl", "still here\n")

	stats, err := c.CollectOnce(context.Background())
	if err != nil {
		t.Fatalf("pass after deletion: %v", err)
	}
	if !contains(up.allData(), "still here") {
		t.Error("surviving stream was not collected")
	}
	_ = stats
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func TestNewRequiresItsDependencies(t *testing.T) {
	m := sourcetest.NewMapFS()
	src, err := claudecode.New(claudecode.Config{FS: m, Home: home, Platform: "linux"})
	if err != nil {
		t.Fatalf("claudecode.New: %v", err)
	}
	cases := []struct {
		name string
		cfg  collector.Config
	}{
		{"no sources", collector.Config{Cursors: cursor.NewMemoryStore(), Uploader: &fakeUploader{}}},
		{"no cursors", collector.Config{Sources: []source.Source{src}, Uploader: &fakeUploader{}}},
		{"no uploader", collector.Config{Sources: []source.Source{src}, Cursors: cursor.NewMemoryStore()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := collector.New(tc.cfg); err == nil {
				t.Error("expected a constructor error")
			}
		})
	}
}
