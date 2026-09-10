package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/store"
)

// open returns a fresh node backed by a temp file. No external database, so
// these run in a plain `go test ./...` rather than being skipped.
func open(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), store.Config{
		Path: filepath.Join(t.TempDir(), "node.db"),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
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
		Sealed:      []byte(fmt.Sprintf("sealed-%d", n)),
	}
}

func TestIdentityIsMintedOnceAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.db")
	ctx := context.Background()

	first, err := store.Open(ctx, store.Config{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	id := first.OriginID()
	if len(id) < 32 {
		t.Errorf("origin id %q looks too short to be 128 bits", id)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening must not mint a new identity: a changed origin id would make
	// every peer treat this node as a brand new one.
	second, err := store.Open(ctx, store.Config{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = second.Close() }() // test cleanup
	if second.OriginID() != id {
		t.Errorf("origin id changed across restart: %q then %q", id, second.OriginID())
	}

	// A different database is a different node.
	other, err := store.Open(ctx, store.Config{Path: filepath.Join(dir, "other.db")})
	if err != nil {
		t.Fatalf("Open other: %v", err)
	}
	defer func() { _ = other.Close() }() // test cleanup
	if other.OriginID() == id {
		t.Error("two databases minted the same origin id")
	}
}

// Gap-freeness is load-bearing: "everything after my watermark" is only
// correct if the sequence has no holes.
func TestLocalSequenceIsDenseAndMonotonic(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	for batch := range 3 {
		var recs []store.Record
		for i := range 5 {
			recs = append(recs, rec(batch*5+i))
		}
		if _, err := db.AppendLocal(ctx, recs); err != nil {
			t.Fatalf("AppendLocal: %v", err)
		}
	}

	got, err := db.Since(ctx, db.OriginID(), 0, 1000)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(got) != 15 {
		t.Fatalf("held %d records, want 15", len(got))
	}
	for i, r := range got {
		if want := int64(i + 1); r.Seq != want {
			t.Fatalf("record %d has seq %d, want %d — the sequence has a hole", i, r.Seq, want)
		}
	}
}

// Re-collecting the same lines must consume no sequence numbers, or every
// restart would inflate the archive.
func TestReAppendingIsIdempotent(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	recs := []store.Record{rec(1), rec(2), rec(3)}

	first, err := db.AppendLocal(ctx, recs)
	if err != nil {
		t.Fatalf("first AppendLocal: %v", err)
	}
	if first.Stored != 3 || first.Duplicates != 0 {
		t.Fatalf("first append = %+v, want 3 stored", first)
	}

	for i := range 3 {
		again, err := db.AppendLocal(ctx, recs)
		if err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if again.Stored != 0 || again.Duplicates != 3 {
			t.Errorf("replay %d = %+v, want 0 stored / 3 duplicates", i, again)
		}
	}

	n, err := db.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 3 {
		t.Errorf("holding %d records after replays, want 3", n)
	}
}

// Two machines that share a username and a transcript path must not merge.
// The previous schema did exactly that, silently.
func TestDifferentOriginsDoNotMerge(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	mine := rec(1)
	if _, err := db.AppendLocal(ctx, []store.Record{mine}); err != nil {
		t.Fatalf("AppendLocal: %v", err)
	}

	// The same path, offset, session and uuid — but a different machine.
	theirs := rec(1)
	theirs.OriginID = "a-different-machine"
	theirs.Seq = 1
	res, err := db.AppendRemote(ctx, []store.Record{theirs})
	if err != nil {
		t.Fatalf("AppendRemote: %v", err)
	}
	if res.Stored != 1 {
		t.Errorf("another machine's identical-looking line = %+v, want it stored separately", res)
	}
	n, _ := db.Count(ctx)
	if n != 2 {
		t.Errorf("holding %d records, want 2 — origins must not merge", n)
	}
}

// The failure that would otherwise corrupt silently.
func TestForkIsQuarantinedNotDiscarded(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	original := rec(1)
	original.OriginID = "cloned-node"
	original.Seq = 1
	if _, err := db.AppendRemote(ctx, []store.Record{original}); err != nil {
		t.Fatalf("AppendRemote: %v", err)
	}

	// The same (origin, seq) with different content: proof of a duplicated id.
	impostor := rec(99)
	impostor.OriginID = "cloned-node"
	impostor.Seq = 1
	res, err := db.AppendRemote(ctx, []store.Record{impostor})
	if err != nil {
		t.Fatalf("AppendRemote of fork: %v", err)
	}
	if res.Forked != 1 {
		t.Errorf("result = %+v, want Forked 1", res)
	}
	if res.Stored != 0 {
		t.Errorf("a fork must not overwrite the held record: %+v", res)
	}

	q, err := db.Quarantined(ctx)
	if err != nil {
		t.Fatalf("Quarantined: %v", err)
	}
	if q != 1 {
		t.Errorf("quarantined %d, want 1 — evidence must be kept, never dropped", q)
	}

	// An identical resend is an ordinary duplicate, not a fork.
	dup, err := db.AppendRemote(ctx, []store.Record{original})
	if err != nil {
		t.Fatalf("AppendRemote of duplicate: %v", err)
	}
	if dup.Forked != 0 || dup.Duplicates != 1 {
		t.Errorf("identical resend = %+v, want 1 duplicate and no fork", dup)
	}
}

func TestVectorSummarizesWhatIsHeld(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	// A node with nothing still announces itself, so a peer can tell a silent
	// node from an absent one.
	vec, err := db.Vector(ctx)
	if err != nil {
		t.Fatalf("Vector: %v", err)
	}
	if seq, ok := vec[db.OriginID()]; !ok || seq != 0 {
		t.Errorf("empty node vector = %v, want its own origin at 0", vec)
	}

	if _, err := db.AppendLocal(ctx, []store.Record{rec(1), rec(2)}); err != nil {
		t.Fatalf("AppendLocal: %v", err)
	}
	remote := rec(7)
	remote.OriginID = "peer-node"
	remote.Seq = 42
	if _, err := db.AppendRemote(ctx, []store.Record{remote}); err != nil {
		t.Fatalf("AppendRemote: %v", err)
	}

	vec, err = db.Vector(ctx)
	if err != nil {
		t.Fatalf("Vector: %v", err)
	}
	if vec[db.OriginID()] != 2 {
		t.Errorf("own watermark = %d, want 2", vec[db.OriginID()])
	}
	if vec["peer-node"] != 42 {
		t.Errorf("peer watermark = %d, want 42", vec["peer-node"])
	}
}

func TestSinceIsOrderedAndBounded(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	var recs []store.Record
	for i := range 10 {
		recs = append(recs, rec(i))
	}
	if _, err := db.AppendLocal(ctx, recs); err != nil {
		t.Fatalf("AppendLocal: %v", err)
	}

	page, err := db.Since(ctx, db.OriginID(), 3, 4)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(page) != 4 {
		t.Fatalf("page has %d records, want 4", len(page))
	}
	for i, r := range page {
		if want := int64(4 + i); r.Seq != want {
			t.Errorf("page[%d].Seq = %d, want %d", i, r.Seq, want)
		}
	}
	if len(page[0].Sealed) == 0 {
		t.Error("sealed payload did not survive the round trip")
	}
}

func TestOpenRequiresAPath(t *testing.T) {
	if _, err := store.Open(context.Background(), store.Config{}); err == nil {
		t.Error("expected an error when Path is empty")
	}
}

// The write-ahead log must be bounded, on every connection.
//
// A reader that will not finish keeps a checkpoint from resetting the log, and
// every write meanwhile extends it: this node reached 263 MB that way and kept
// it, because SQLite reuses that space but never returns it. The limit is what
// makes the next good checkpoint give it back, and it is per connection -- the
// pool opens more on demand, and one unconfigured connection is enough to let
// the file grow again.
func TestEveryConnectionBoundsTheWriteAheadLog(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	// Hold several connections open at once so the pool has to make new ones,
	// then check the limit on each.
	var held []*sql.Conn
	for range 4 {
		conn, err := db.SQL().Conn(ctx)
		if err != nil {
			t.Fatalf("Conn: %v", err)
		}
		defer func() { _ = conn.Close() }() // test cleanup
		held = append(held, conn)
	}
	for i, conn := range held {
		var limit int64
		if err := conn.QueryRowContext(ctx, `PRAGMA journal_size_limit`).Scan(&limit); err != nil {
			t.Fatalf("connection %d: read journal_size_limit: %v", i, err)
		}
		if limit != store.MaxJournalBytes {
			t.Errorf("connection %d caps its write-ahead log at %d bytes, want %d",
				i, limit, store.MaxJournalBytes)
		}
	}
}

// The name a machine reports for itself outlives a failed reverse lookup, and
// a peer too old to report one must not erase what an upgraded one said.
func TestPeerHostnameIsKeptAndNeverErasedBySilence(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	if err := db.SavePeer(ctx, store.Peer{ID: "p1", Addrs: []string{"192.168.86.24:8844"}}); err != nil {
		t.Fatalf("SavePeer: %v", err)
	}
	if err := db.SavePeerHostname(ctx, "p1", "darkleon"); err != nil {
		t.Fatalf("SavePeerHostname: %v", err)
	}

	names, err := db.PeerNames(ctx)
	if err != nil {
		t.Fatalf("PeerNames: %v", err)
	}
	if names["p1"] != "darkleon" {
		t.Errorf("PeerNames[p1] = %q, want %q", names["p1"], "darkleon")
	}

	// An exchange with a node that reports no name at all.
	if err := db.SavePeerHostname(ctx, "p1", ""); err != nil {
		t.Fatalf("SavePeerHostname with no name: %v", err)
	}
	peers, err := db.Peers(ctx)
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	if len(peers) != 1 {
		t.Fatalf("got %d peers, want 1", len(peers))
	}
	if peers[0].Hostname != "darkleon" {
		t.Errorf("hostname = %q after a silent exchange, want it kept", peers[0].Hostname)
	}

	// Addresses the peer keeps answering on are not disturbed by naming it.
	if len(peers[0].Addrs) != 1 || peers[0].Addrs[0] != "192.168.86.24:8844" {
		t.Errorf("addrs = %v, want the address preserved", peers[0].Addrs)
	}
}

// A rename is the machine's own answer and must win over the previous one.
func TestPeerHostnameFollowsARename(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	for _, name := range []string{"old-name", "new-name"} {
		if err := db.SavePeerHostname(ctx, "p1", name); err != nil {
			t.Fatalf("SavePeerHostname(%q): %v", name, err)
		}
	}
	names, err := db.PeerNames(ctx)
	if err != nil {
		t.Fatalf("PeerNames: %v", err)
	}
	if names["p1"] != "new-name" {
		t.Errorf("PeerNames[p1] = %q, want %q", names["p1"], "new-name")
	}
}
