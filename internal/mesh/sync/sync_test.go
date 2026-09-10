package sync_test

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"

	meshsync "github.com/christianparpart/agentic-stats/internal/mesh/sync"
	"github.com/christianparpart/agentic-stats/internal/store"
)

func openStore(t *testing.T, name string) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), store.Config{
		Path: filepath.Join(t.TempDir(), name+".db"),
	})
	if err != nil {
		t.Fatalf("store.Open(%s): %v", name, err)
	}
	t.Cleanup(func() { _ = db.Close() }) // test cleanup
	return db
}

func rec(n int) store.Record {
	return store.Record{
		Source:      "claudecode",
		Path:        "/logs/s.jsonl",
		ByteOffset:  int64(n * 100),
		ContentHash: fmt.Sprintf("hash-%d", n),
		SessionID:   "session-1",
		LineUUID:    fmt.Sprintf("uuid-%d", n),
		CapturedAt:  "2026-09-09T10:00:00Z",
		RequestID:   fmt.Sprintf("req-%d", n),
		Model:       "claude-opus-5",
		Output:      int64(n * 10),
		Sealed:      []byte(fmt.Sprintf("sealed-%d", n)),
	}
}

func fill(t *testing.T, db *store.DB, from, to int) {
	t.Helper()
	var recs []store.Record
	for i := from; i < to; i++ {
		recs = append(recs, rec(i))
	}
	if _, err := db.AppendLocal(context.Background(), recs); err != nil {
		t.Fatalf("AppendLocal: %v", err)
	}
}

// exchange runs one full sync between two stores over an in-memory pipe.
func exchange(t *testing.T, a, b *store.DB) (meshsync.Stats, meshsync.Stats) {
	t.Helper()
	sa, err := meshsync.New(a, meshsync.Config{})
	if err != nil {
		t.Fatalf("sync.New: %v", err)
	}
	sb, err := meshsync.New(b, meshsync.Config{})
	if err != nil {
		t.Fatalf("sync.New: %v", err)
	}

	left, right := net.Pipe()
	t.Cleanup(func() { _ = left.Close(); _ = right.Close() })

	ctx := context.Background()
	type result struct {
		stats meshsync.Stats
		err   error
	}
	done := make(chan result, 1)
	go func() {
		s, err := sb.Exchange(ctx, right, "a")
		done <- result{s, err}
	}()

	statsA, err := sa.Exchange(ctx, left, "b")
	if err != nil {
		t.Fatalf("Exchange on A: %v", err)
	}
	rb := <-done
	if rb.err != nil {
		t.Fatalf("Exchange on B: %v", rb.err)
	}
	return statsA, rb.stats
}

func counts(t *testing.T, db *store.DB) int64 {
	t.Helper()
	n, err := db.Count(context.Background())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	return n
}

// The headline property: two nodes with disjoint records converge, both ways,
// in one exchange.
func TestDisjointNodesConverge(t *testing.T) {
	a := openStore(t, "a")
	b := openStore(t, "b")
	fill(t, a, 0, 10)
	fill(t, b, 100, 117)

	exchange(t, a, b)

	if got := counts(t, a); got != 27 {
		t.Errorf("A holds %d records, want 27", got)
	}
	if got := counts(t, b); got != 27 {
		t.Errorf("B holds %d records, want 27", got)
	}

	// And their version vectors must agree, not merely their counts.
	ctx := context.Background()
	va, err := a.Vector(ctx)
	if err != nil {
		t.Fatalf("Vector A: %v", err)
	}
	vb, err := b.Vector(ctx)
	if err != nil {
		t.Fatalf("Vector B: %v", err)
	}
	if len(va) != len(vb) {
		t.Fatalf("vectors cover different origins: %v vs %v", va, vb)
	}
	for origin, seq := range va {
		if vb[origin] != seq {
			t.Errorf("origin %s: A has %d, B has %d", origin, seq, vb[origin])
		}
	}
}

// Syncing again with nothing new must move nothing at all.
func TestSecondExchangeIsQuiet(t *testing.T) {
	a := openStore(t, "a")
	b := openStore(t, "b")
	fill(t, a, 0, 5)

	exchange(t, a, b)
	statsA, statsB := exchange(t, a, b)

	if statsA.Sent != 0 || statsB.Sent != 0 {
		t.Errorf("a settled pair still sent records: A %d, B %d", statsA.Sent, statsB.Sent)
	}
	if statsA.Stored != 0 || statsB.Stored != 0 {
		t.Errorf("a settled pair stored records: A %d, B %d", statsA.Stored, statsB.Stored)
	}
}

// A third node started empty must catch up from either peer, including records
// that peer only holds because it synced them itself.
func TestThirdNodeCatchesUpTransitively(t *testing.T) {
	a := openStore(t, "a")
	b := openStore(t, "b")
	c := openStore(t, "c")
	fill(t, a, 0, 6)
	fill(t, b, 50, 54)

	exchange(t, a, b) // A and B now both hold 10
	exchange(t, b, c) // C learns everything through B, including A's records

	if got := counts(t, c); got != 10 {
		t.Errorf("C holds %d records, want 10 — A's records did not reach it through B", got)
	}
	ctx := context.Background()
	vc, err := c.Vector(ctx)
	if err != nil {
		t.Fatalf("Vector C: %v", err)
	}
	if len(vc) != 3 {
		t.Errorf("C knows %d origins, want 3 (A, B and itself)", len(vc))
	}
	if vc[a.OriginID()] != 6 {
		t.Errorf("C has A up to %d, want 6", vc[a.OriginID()])
	}
}

// Incremental: only what is new should move.
func TestOnlyNewRecordsAreSent(t *testing.T) {
	a := openStore(t, "a")
	b := openStore(t, "b")
	fill(t, a, 0, 5)
	exchange(t, a, b)

	fill(t, a, 5, 8)
	statsA, statsB := exchange(t, a, b)

	if statsA.Sent != 3 {
		t.Errorf("A sent %d records, want exactly the 3 new ones", statsA.Sent)
	}
	if statsB.Stored != 3 {
		t.Errorf("B stored %d records, want 3", statsB.Stored)
	}
	if got := counts(t, b); got != 8 {
		t.Errorf("B holds %d, want 8", got)
	}
}

// Batching must not lose or duplicate anything at a boundary.
func TestLargeTransferCrossesBatchBoundaries(t *testing.T) {
	a := openStore(t, "a")
	b := openStore(t, "b")
	const n = 1200 // more than two batches of 500
	fill(t, a, 0, n)

	exchange(t, a, b)

	if got := counts(t, b); got != n {
		t.Errorf("B holds %d records, want %d", got, n)
	}
	// Every sequence must be present exactly once.
	recs, err := b.Since(context.Background(), a.OriginID(), 0, n+10)
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

// A cloned origin must be quarantined during sync, not merged.
func TestForkedOriginIsQuarantinedDuringSync(t *testing.T) {
	a := openStore(t, "a")
	b := openStore(t, "b")
	ctx := context.Background()

	// B already holds a record under an origin that A will also claim.
	impostor := rec(1)
	impostor.OriginID = "shared-origin"
	impostor.Seq = 1
	impostor.ContentHash = "a-different-hash"
	if _, err := b.AppendRemote(ctx, []store.Record{impostor}); err != nil {
		t.Fatalf("seed B: %v", err)
	}
	original := rec(1)
	original.OriginID = "shared-origin"
	original.Seq = 1
	if _, err := a.AppendRemote(ctx, []store.Record{original}); err != nil {
		t.Fatalf("seed A: %v", err)
	}

	_, statsB := exchange(t, a, b)
	if statsB.Forked != 1 {
		t.Errorf("B reported %d forks, want 1", statsB.Forked)
	}
	q, err := b.Quarantined(ctx)
	if err != nil {
		t.Fatalf("Quarantined: %v", err)
	}
	if q != 1 {
		t.Errorf("B quarantined %d records, want 1", q)
	}
}

func TestNewRequiresAStore(t *testing.T) {
	if _, err := meshsync.New(nil, meshsync.Config{}); err == nil {
		t.Error("expected an error when the store is missing")
	}
}

// exchangeNamed runs one exchange where each side reports a name for itself.
func exchangeNamed(t *testing.T, a, b *store.DB, hostA, hostB string) (meshsync.Stats, meshsync.Stats) {
	t.Helper()
	sa, err := meshsync.New(a, meshsync.Config{Host: hostA})
	if err != nil {
		t.Fatalf("sync.New: %v", err)
	}
	sb, err := meshsync.New(b, meshsync.Config{Host: hostB})
	if err != nil {
		t.Fatalf("sync.New: %v", err)
	}

	left, right := net.Pipe()
	t.Cleanup(func() { _ = left.Close(); _ = right.Close() })

	ctx := context.Background()
	type result struct {
		stats meshsync.Stats
		err   error
	}
	done := make(chan result, 1)
	go func() {
		s, err := sb.Exchange(ctx, right, "a")
		done <- result{s, err}
	}()
	statsA, err := sa.Exchange(ctx, left, "b")
	if err != nil {
		t.Fatalf("Exchange from a: %v", err)
	}
	res := <-done
	if res.err != nil {
		t.Fatalf("Exchange from b: %v", res.err)
	}
	return statsA, res.stats
}

// A machine on a LAN with no reverse zone has no name a resolver will give up,
// so it says what it is called itself.
func TestEachSideLearnsWhatThePeerCallsItself(t *testing.T) {
	a := openStore(t, "a")
	b := openStore(t, "b")
	fill(t, a, 0, 3)
	fill(t, b, 0, 2)

	statsA, statsB := exchangeNamed(t, a, b, "fedora", "darkleon")

	if statsA.PeerHost != "darkleon" {
		t.Errorf("a learned peer host %q, want %q", statsA.PeerHost, "darkleon")
	}
	if statsB.PeerHost != "fedora" {
		t.Errorf("b learned peer host %q, want %q", statsB.PeerHost, "fedora")
	}
}

// A node from before the field existed sends no name. It must still converge,
// and must not be reported as having announced an empty one -- an empty name
// would otherwise erase a name learned when that peer was last upgraded.
func TestAPeerThatReportsNoNameStillConverges(t *testing.T) {
	a := openStore(t, "a")
	b := openStore(t, "b")
	fill(t, a, 0, 4)

	statsA, statsB := exchangeNamed(t, a, b, "fedora", "")

	if statsA.PeerHost != "" {
		t.Errorf("a learned peer host %q, want empty", statsA.PeerHost)
	}
	if statsB.PeerHost != "fedora" {
		t.Errorf("b learned peer host %q, want %q", statsB.PeerHost, "fedora")
	}
	if statsB.Stored != 4 {
		t.Errorf("b stored %d records, want 4", statsB.Stored)
	}
}
