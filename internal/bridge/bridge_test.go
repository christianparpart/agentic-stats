package bridge_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/bridge"
	"github.com/christianparpart/agentic-stats/internal/bridge/fsbackend"
	"github.com/christianparpart/agentic-stats/internal/seal"
	"github.com/christianparpart/agentic-stats/internal/store"
)

func newStore(t *testing.T, name string) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), store.Config{
		Path: filepath.Join(t.TempDir(), name+".db"),
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() }) // test cleanup
	return db
}

func newKeys(t *testing.T) (*seal.Keys, string) {
	t.Helper()
	psk, err := seal.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	k, err := seal.Derive(psk)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	return k, psk
}

func rec(n int) store.Record {
	return store.Record{
		Source: "claudecode", Path: "/logs/s.jsonl", ByteOffset: int64(n * 100),
		ContentHash: fmt.Sprintf("hash-%d", n),
		SessionID:   "session-1", LineUUID: fmt.Sprintf("uuid-%d", n),
		CapturedAt: "2026-09-09T10:00:00Z",
		Sealed:     []byte(fmt.Sprintf("sealed-payload-%d", n)),
	}
}

func fill(t *testing.T, db *store.DB, n int) {
	t.Helper()
	var recs []store.Record
	for i := range n {
		recs = append(recs, rec(i))
	}
	if _, err := db.AppendLocal(context.Background(), recs); err != nil {
		t.Fatalf("AppendLocal: %v", err)
	}
}

func newBridge(t *testing.T, dir string, db *store.DB, k *seal.Keys) *bridge.Bridge {
	t.Helper()
	be, err := fsbackend.New(dir)
	if err != nil {
		t.Fatalf("fsbackend.New: %v", err)
	}
	b, err := bridge.New(bridge.Config{Backend: be, Store: db, Keys: k})
	if err != nil {
		t.Fatalf("bridge.New: %v", err)
	}
	return b
}

// Two nodes that can never see each other converge through a shared folder.
func TestNodesConvergeThroughSharedStorage(t *testing.T) {
	shared := t.TempDir()
	k, psk := newKeys(t)
	_ = psk
	ctx := context.Background()

	a := newStore(t, "a")
	b := newStore(t, "b")
	fill(t, a, 30)
	fill(t, b, 12)

	ba := newBridge(t, shared, a, k)
	bb := newBridge(t, shared, b, k)

	// A publishes, B fetches; then the reverse.
	if _, err := ba.Sync(ctx); err != nil {
		t.Fatalf("A sync: %v", err)
	}
	if _, err := bb.Sync(ctx); err != nil {
		t.Fatalf("B sync: %v", err)
	}
	if _, err := ba.Sync(ctx); err != nil {
		t.Fatalf("A second sync: %v", err)
	}

	countA, err := a.Count(ctx)
	if err != nil {
		t.Fatalf("Count A: %v", err)
	}
	countB, err := b.Count(ctx)
	if err != nil {
		t.Fatalf("Count B: %v", err)
	}
	if countA != 42 || countB != 42 {
		t.Errorf("A holds %d and B holds %d, want 42 each", countA, countB)
	}
}

// Re-running must upload nothing new: publishing is stateless, decided purely
// from what the store already contains.
func TestRepublishingIsQuiet(t *testing.T) {
	shared := t.TempDir()
	k, _ := newKeys(t)
	ctx := context.Background()

	a := newStore(t, "a")
	fill(t, a, 20)
	ba := newBridge(t, shared, a, k)

	first, err := ba.Publish(ctx)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if first != 20 {
		t.Errorf("first publish moved %d records, want 20", first)
	}
	for i := range 3 {
		again, err := ba.Publish(ctx)
		if err != nil {
			t.Fatalf("republish %d: %v", i, err)
		}
		if again != 0 {
			t.Errorf("republish %d moved %d records, want 0", i, again)
		}
	}
}

// The storage provider is not trusted: bundles on disk must be ciphertext.
func TestBundlesOnDiskAreEncrypted(t *testing.T) {
	shared := t.TempDir()
	k, _ := newKeys(t)
	ctx := context.Background()

	a := newStore(t, "a")
	fill(t, a, 5)
	if _, err := newBridge(t, shared, a, k).Publish(ctx); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	found := 0
	err := filepath.WalkDir(shared, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		found++
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		// Both the payload and the metadata around it must be sealed: a
		// third-party store must not learn file paths or model names either.
		for _, marker := range []string{"sealed-payload-", "/logs/s.jsonl", "claudecode", "session-1"} {
			if bytes.Contains(raw, []byte(marker)) {
				t.Errorf("bundle %s contains %q in the clear", d.Name(), marker)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if found == 0 {
		t.Fatal("no bundles were written; the check proved nothing")
	}
}

// A different mesh sharing the same folder must be ignored, not merged.
func TestBundlesFromAnotherMeshAreSkipped(t *testing.T) {
	shared := t.TempDir()
	ours, _ := newKeys(t)
	theirs, _ := newKeys(t)
	ctx := context.Background()

	stranger := newStore(t, "stranger")
	fill(t, stranger, 8)
	if _, err := newBridge(t, shared, stranger, theirs).Publish(ctx); err != nil {
		t.Fatalf("stranger Publish: %v", err)
	}

	mine := newStore(t, "mine")
	fetched, stored, forked, err := newBridge(t, shared, mine, ours).Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if fetched != 0 || stored != 0 || forked != 0 {
		t.Errorf("read another mesh's bundles: fetched %d, stored %d, forked %d",
			fetched, stored, forked)
	}
	n, err := mine.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 0 {
		t.Errorf("holding %d records from a foreign mesh, want 0", n)
	}
}

// Bundles must cross the batch boundary without loss or duplication.
func TestLargeArchiveSpansMultipleBundles(t *testing.T) {
	shared := t.TempDir()
	k, _ := newKeys(t)
	ctx := context.Background()

	a := newStore(t, "a")
	const n = 12_000 // more than two bundles of 5000
	fill(t, a, n)
	if _, err := newBridge(t, shared, a, k).Publish(ctx); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	b := newStore(t, "b")
	fetched, stored, _, err := newBridge(t, shared, b, k).Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if fetched != n || stored != n {
		t.Errorf("fetched %d and stored %d, want %d each", fetched, stored, n)
	}
	recs, err := b.Since(ctx, a.OriginID(), 0, n+10)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(recs) != n {
		t.Fatalf("read back %d records, want %d", len(recs), n)
	}
	for i, r := range recs {
		if want := int64(i + 1); r.Seq != want {
			t.Fatalf("record %d has seq %d, want %d", i, r.Seq, want)
		}
	}
}

func TestNewRequiresItsDependencies(t *testing.T) {
	k, _ := newKeys(t)
	db := newStore(t, "x")
	be, err := fsbackend.New(t.TempDir())
	if err != nil {
		t.Fatalf("fsbackend.New: %v", err)
	}
	for _, tc := range []struct {
		name string
		cfg  bridge.Config
	}{
		{"no backend", bridge.Config{Store: db, Keys: k}},
		{"no store", bridge.Config{Backend: be, Keys: k}},
		{"no keys", bridge.Config{Backend: be, Store: db}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := bridge.New(tc.cfg); err == nil {
				t.Error("expected a constructor error")
			}
		})
	}
	if _, err := fsbackend.New(""); err == nil {
		t.Error("expected an error for an empty directory")
	}
}
