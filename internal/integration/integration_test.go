// Package integration_test exercises a whole node: collect, store, derive,
// serve. It needs no database server, so unlike its predecessor it actually
// runs in a plain `go test ./...` rather than skipping.
package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/derive"
	"github.com/christianparpart/agentic-stats/internal/ingest"
	"github.com/christianparpart/agentic-stats/internal/pricing"
	"github.com/christianparpart/agentic-stats/internal/seal"
	"github.com/christianparpart/agentic-stats/internal/store"
	"github.com/christianparpart/agentic-stats/internal/wire"
)

// node is a whole running node, minus the network.
type node struct {
	dir    string
	dbPath string
	db     *store.DB
	keys   *seal.Keys
	writer *ingest.Writer
	derive *derive.Service
}

func newNode(t *testing.T) *node {
	t.Helper()
	psk, err := seal.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return newNodeWithKey(t, psk)
}

func newNodeWithKey(t *testing.T, psk string) *node {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "archive.db")

	keys, err := seal.Derive(psk)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	db, err := store.Open(context.Background(), store.Config{Path: dbPath})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() }) // test cleanup

	writer, err := ingest.NewWriter(db, keys)
	if err != nil {
		t.Fatalf("ingest.NewWriter: %v", err)
	}
	prices, err := pricing.Load()
	if err != nil {
		t.Fatalf("pricing.Load: %v", err)
	}
	svc, err := derive.NewService(db, prices)
	if err != nil {
		t.Fatalf("derive.NewService: %v", err)
	}
	return &node{dir: dir, dbPath: dbPath, db: db, keys: keys, writer: writer, derive: svc}
}

// assistantLine is shaped like the real thing: one content block per line,
// every line repeating the whole usage object for the response.
func assistantLine(requestID, model string, output, cacheRead int64) string {
	return fmt.Sprintf(`{"type":"assistant","requestId":%q,"uuid":%q,`+
		`"sessionId":"session-1","timestamp":"2026-09-08T12:00:00.000Z",`+
		`"message":{"model":%q,"usage":{"input_tokens":10,"output_tokens":%d,`+
		`"cache_read_input_tokens":%d,"output_tokens_details":{"thinking_tokens":5},`+
		`"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":0}}}}`,
		requestID, requestID+"-"+fmt.Sprint(output)+"-"+fmt.Sprint(cacheRead), model, output, cacheRead)
}

func records(lines ...string) []wire.Record {
	out := make([]wire.Record, len(lines))
	for i, l := range lines {
		out[i] = wire.NewRecord("claudecode", "/logs/s.jsonl", int64(i*1000), []byte(l))
	}
	return out
}

// The property the whole pipeline rests on: re-collecting is free and harmless.
func TestReCollectingIsIdempotent(t *testing.T) {
	n := newNode(t)
	ctx := context.Background()
	batch := records(
		assistantLine("req-1", "claude-opus-5", 100, 1000),
		assistantLine("req-2", "claude-opus-5", 200, 2000),
	)

	first, err := n.writer.Ingest(ctx, batch)
	if err != nil {
		t.Fatalf("first Ingest: %v", err)
	}
	if first.Stored != 2 || first.Duplicates != 0 {
		t.Errorf("first ingest = %+v, want 2 stored", first)
	}
	for i := range 3 {
		again, err := n.writer.Ingest(ctx, batch)
		if err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if again.Stored != 0 || again.Duplicates != 2 {
			t.Errorf("replay %d = %+v, want 0 stored / 2 duplicates", i, again)
		}
	}
}

// The single highest-value test in the repository: the fold, end to end.
func TestDeriveFoldsRepeatedUsageByRequestID(t *testing.T) {
	n := newNode(t)
	ctx := context.Background()

	// One API response written as four lines, as Claude Code does for a
	// thinking + text + two tool_use response. Every line repeats the usage.
	batch := records(
		assistantLine("req-A", "claude-opus-5", 1000, 5000),
		assistantLine("req-A", "claude-opus-5", 1000, 5000),
		assistantLine("req-A", "claude-opus-5", 1000, 5000),
		assistantLine("req-A", "claude-opus-5", 1000, 5000),
		assistantLine("req-B", "claude-opus-5", 500, 2000),
		// A synthetic error line must never reach a cost calculation.
		`{"type":"assistant","requestId":null,"message":{"model":"<synthetic>","usage":{"output_tokens":0}}}`,
	)
	if _, err := n.writer.Ingest(ctx, batch); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	sum, err := n.derive.Summarize(ctx)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Requests != 2 {
		t.Errorf("requests = %d, want 2 (req-A folded from 4 lines, plus req-B)", sum.Requests)
	}
	if len(sum.Models) != 1 {
		t.Fatalf("models = %d, want 1", len(sum.Models))
	}
	// Naive summing would give 4*1000 + 500 = 4500.
	if got := sum.Models[0].OutputTokens; got != 1500 {
		t.Errorf("output tokens = %d, want 1500; naive summing would give 4500", got)
	}
	if got := sum.Models[0].CacheReadTokens; got != 7000 {
		t.Errorf("cache read tokens = %d, want 7000", got)
	}
	if !sum.Models[0].Priced {
		t.Error("claude-opus-5 must be priced")
	}
	if sum.CacheSavingsUSD <= 0 {
		t.Errorf("cache savings = %v, want positive", sum.CacheSavingsUSD)
	}

	activity, err := n.derive.Daily(ctx)
	if err != nil {
		t.Fatalf("Daily: %v", err)
	}
	if len(activity.Days) != 1 || activity.Days[0].Requests != 2 {
		t.Errorf("daily = %+v, want one day with 2 requests", activity.Days)
	}
}

// The central privacy claim, checked against the file on disk rather than
// asserted: a stolen archive must yield nothing.
func TestArchiveOnDiskIsSealed(t *testing.T) {
	n := newNode(t)
	ctx := context.Background()

	const secret = "SUPER-SECRET-CUSTOMER-SOURCE-CODE"
	line := fmt.Sprintf(`{"type":"user","uuid":"u1","sessionId":"s1","message":{"content":%q}}`, secret)
	if _, err := n.writer.Ingest(ctx, records(line)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if err := n.db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	raw, err := os.ReadFile(n.dbPath)
	if err != nil {
		t.Fatalf("read database file: %v", err)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("the database file contains transcript content in the clear")
	}
	// Sanity: the marker really was long enough to find if it were present.
	if len(raw) < len(secret) {
		t.Fatal("database file is implausibly small; the check proved nothing")
	}
}

// A different mesh cannot read this one's records even holding the file.
func TestAnotherMeshCannotRead(t *testing.T) {
	mine := newNode(t)
	ctx := context.Background()
	if _, err := mine.writer.Ingest(ctx, records(
		assistantLine("req-1", "claude-opus-5", 10, 10))); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	held, err := mine.db.Since(ctx, mine.db.OriginID(), 0, 10)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(held) != 1 {
		t.Fatalf("held %d records, want 1", len(held))
	}

	stranger := newNode(t)
	if _, err := stranger.keys.OpenPayload(held[0].Sealed); err == nil {
		t.Fatal("a node with a different mesh key opened our record")
	}
	// Our own key must of course still work, or the archive is a brick.
	if _, err := mine.keys.OpenPayload(held[0].Sealed); err != nil {
		t.Fatalf("our own key could not open our record: %v", err)
	}
}

// Two nodes holding the same key converge on identical figures — the property
// the whole mesh exists to provide, tested here at the store level before any
// networking exists.
func TestReplicasWithTheSameKeyAgree(t *testing.T) {
	psk, err := seal.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	a := newNodeWithKey(t, psk)
	b := newNodeWithKey(t, psk)
	ctx := context.Background()

	batch := records(
		assistantLine("req-A", "claude-opus-5", 1000, 5000),
		assistantLine("req-A", "claude-opus-5", 1000, 5000),
		assistantLine("req-B", "claude-sonnet-5", 300, 900),
	)
	if _, err := a.writer.Ingest(ctx, batch); err != nil {
		t.Fatalf("Ingest on A: %v", err)
	}

	// Hand A's records to B exactly as a sync would, origin and sequence intact.
	shipped, err := a.db.Since(ctx, a.db.OriginID(), 0, 1000)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if _, err := b.db.AppendRemote(ctx, shipped); err != nil {
		t.Fatalf("AppendRemote on B: %v", err)
	}

	sumA, err := a.derive.Summarize(ctx)
	if err != nil {
		t.Fatalf("Summarize A: %v", err)
	}
	sumB, err := b.derive.Summarize(ctx)
	if err != nil {
		t.Fatalf("Summarize B: %v", err)
	}
	if sumA.Requests != sumB.Requests || sumA.Lines != sumB.Lines {
		t.Errorf("replicas disagree: A %d requests / %d lines, B %d / %d",
			sumA.Requests, sumA.Lines, sumB.Requests, sumB.Lines)
	}
	if fmt.Sprintf("%.6f", sumA.TotalCostUSD) != fmt.Sprintf("%.6f", sumB.TotalCostUSD) {
		t.Errorf("replicas disagree on cost: A %v, B %v", sumA.TotalCostUSD, sumB.TotalCostUSD)
	}
	// And B's copy must be readable, not just present.
	if sumB.TotalCostUSD <= 0 {
		t.Error("the replica derived no cost; the records did not survive the trip")
	}
}

// prLink is the line the assistant writes when it opens a pull request.
func prLink(session, repo string, number int) string {
	return fmt.Sprintf(`{"type":"pr-link","sessionId":%q,"prRepository":%q,"prNumber":%d,`+
		`"timestamp":"2026-09-08T12:00:00.000Z"}`, session, repo, number)
}

// sessionLine is an assistant response belonging to a named session.
func sessionLine(session, requestID string, output int64) string {
	return sessionLineWithUUID(session, requestID, requestID+"-u", output)
}

// sessionLineWithUUID is sessionLine with the line's own identity supplied, so
// a test can put two lines of the same request under different sessions.
func sessionLineWithUUID(session, requestID, uuid string, output int64) string {
	return fmt.Sprintf(`{"type":"assistant","requestId":%q,"uuid":%q,"sessionId":%q,`+
		`"timestamp":"2026-09-08T12:00:00.000Z","message":{"model":"claude-opus-5",`+
		`"usage":{"input_tokens":10,"output_tokens":%d,"cache_read_input_tokens":100,`+
		`"output_tokens_details":{"thinking_tokens":5},`+
		`"cache_creation":{"ephemeral_5m_input_tokens":10,"ephemeral_1h_input_tokens":0}}}}`,
		requestID, uuid, session, output)
}

// When a request's lines disagree about which session they belong to, the
// attributed share must follow the fold -- the same single line the cost is
// billed through -- and not count the request because some other line of it
// mentioned a session that shipped.
//
// This is not hypothetical: 80 requests in a 480k-record archive have lines
// under more than one session id. Counting lines made the ratio describe an
// attribution the table did not perform, which is a number that means nothing.
func TestAttributionFollowsTheFold(t *testing.T) {
	n := newNode(t)
	ctx := context.Background()

	if _, err := n.writer.Ingest(ctx, records(
		// The fold takes the lowest (origin, seq), so this request belongs to
		// the session that shipped nothing -- even though its second line
		// names the one that did.
		sessionLineWithUUID("no-pull-request", "req-split", "split-a", 100),
		sessionLineWithUUID("shipped", "req-split", "split-b", 100),
		sessionLine("shipped", "req-shipped", 100),
		prLink("shipped", "acme/widgets", 7),
	)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	d, err := n.derive.Deliveries(ctx)
	if err != nil {
		t.Fatalf("Deliveries: %v", err)
	}
	if len(d.PullRequests) != 1 {
		t.Fatalf("attributed %d pull requests, want 1", len(d.PullRequests))
	}
	// One of the two folded requests is billed to the pull request...
	if got := d.PullRequests[0].Requests; got != 1 {
		t.Errorf("pull request billed %d requests, want 1", got)
	}
	// ...so exactly half of the archive's requests are attributed. Counting
	// lines instead would say all of them.
	if got := d.Attributed; got < 0.5-1e-9 || got > 0.5+1e-9 {
		t.Errorf("attributed share = %.3f, want 0.5", got)
	}
}

// A session that opens several pull requests must have its cost split between
// them, not counted once per pull request. Counting it whole against each is
// the same double-count the requestId fold exists to prevent, arriving from a
// different direction.
func TestCostIsSplitAcrossSharedSessions(t *testing.T) {
	n := newNode(t)
	ctx := context.Background()

	// One session, three pull requests.
	batch := records(
		sessionLine("busy-session", "req-1", 1000),
		sessionLine("busy-session", "req-2", 1000),
		prLink("busy-session", "acme/widgets", 1),
		prLink("busy-session", "acme/widgets", 2),
		prLink("busy-session", "acme/widgets", 3),
		// A second session with a single pull request, for contrast.
		sessionLine("focused-session", "req-3", 600),
		prLink("focused-session", "acme/widgets", 4),
	)
	if _, err := n.writer.Ingest(ctx, batch); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	d, err := n.derive.Deliveries(ctx)
	if err != nil {
		t.Fatalf("Deliveries: %v", err)
	}
	if len(d.PullRequests) != 4 {
		t.Fatalf("attributed %d pull requests, want 4", len(d.PullRequests))
	}

	sum, err := n.derive.Summarize(ctx)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	// The headline property: allocation must never exceed what was spent.
	if d.TotalCostUSD > sum.TotalCostUSD+1e-9 {
		t.Errorf("allocated $%.6f across pull requests but only $%.6f was spent",
			d.TotalCostUSD, sum.TotalCostUSD)
	}
	if d.Attributed > 1.0 {
		t.Errorf("attributed share = %.3f, want at most 1.0", d.Attributed)
	}

	byNumber := map[int64]float64{}
	shared := map[int64]int64{}
	for _, pr := range d.PullRequests {
		byNumber[pr.Number] = pr.CostUSD
		shared[pr.Number] = pr.SharedSessions
	}
	// The three pull requests from one session must each carry a third.
	for _, n := range []int64{1, 2, 3} {
		if shared[n] == 0 {
			t.Errorf("pull request %d should be marked as sharing its session", n)
		}
	}
	if a, b := byNumber[1], byNumber[2]; a <= 0 || a != b {
		t.Errorf("shared pull requests carry %v and %v, want an equal split", a, b)
	}
	if shared[4] != 0 {
		t.Error("pull request 4 had a session to itself and must not be marked shared")
	}
	// The three shares must reconstitute the whole session: splitting must
	// redistribute cost, never destroy or invent it.
	whole := byNumber[1] + byNumber[2] + byNumber[3]
	if whole <= 0 {
		t.Fatal("the shared session contributed no cost at all")
	}
	perShare := whole / 3
	for _, n := range []int64{1, 2, 3} {
		if diff := byNumber[n] - perShare; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("pull request %d carries $%.6f, want an even third at $%.6f",
				n, byNumber[n], perShare)
		}
	}
}

// With no pull requests recorded the view must be empty rather than wrong.
func TestDeliveriesAreEmptyWithoutPullRequests(t *testing.T) {
	n := newNode(t)
	ctx := context.Background()
	if _, err := n.writer.Ingest(ctx, records(
		assistantLine("req-1", "claude-opus-5", 100, 10))); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	d, err := n.derive.Deliveries(ctx)
	if err != nil {
		t.Fatalf("Deliveries: %v", err)
	}
	if len(d.PullRequests) != 0 || d.TotalCostUSD != 0 || d.Attributed != 0 {
		t.Errorf("expected an empty delivery view, got %+v", d)
	}
}
