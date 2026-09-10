package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/derive"
	"github.com/christianparpart/agentic-stats/internal/ingest"
	"github.com/christianparpart/agentic-stats/internal/seal"
	"github.com/christianparpart/agentic-stats/internal/store"
)

// reprocessorFor builds one over a node's own archive and keys.
func reprocessorFor(t *testing.T, n *node) *ingest.Reprocessor {
	t.Helper()
	redo, err := ingest.NewReprocessor(ingest.ReprocessConfig{
		Archive: n.db,
		Keys:    n.keys,
		// Two, so a fixture of a handful of records exercises more than one
		// transaction and the mark has to be right in the middle of a walk.
		Batch: 2,
	})
	if err != nil {
		t.Fatalf("NewReprocessor: %v", err)
	}
	return redo
}

// appendAsAnOlderBuildWould stores lines the way a build that did not know
// about the extracted columns stored them: the sealed body carries everything,
// and the columns derived from it are blank.
//
// This is the state every archive is in immediately after the upgrade that
// taught it to read a new field, so it is the state re-extraction has to
// repair. Going through the collector instead would extract the columns on the
// way in and leave the reprocessor nothing to do -- which is a test that passes
// without exercising anything.
func appendAsAnOlderBuildWould(t *testing.T, n *node, lines ...string) {
	t.Helper()
	recs := make([]store.Record, 0, len(lines))
	for _, line := range lines {
		body, err := n.keys.SealPayload([]byte(line))
		if err != nil {
			t.Fatalf("SealPayload: %v", err)
		}
		id := derive.Identify([]byte(line))
		recs = append(recs, store.Record{
			Source: "claudecode", Path: "p", ByteOffset: int64(len(recs)),
			ContentHash: id.LineUUID + "h",
			SessionID:   id.SessionID, LineUUID: id.LineUUID,
			CapturedAt: id.CapturedAt, RequestID: id.RequestID, Model: id.Model,
			Input: id.Usage.Input, Output: id.Usage.Output,
			Thinking: id.Usage.Thinking, CacheRead: id.Usage.CacheRead,
			CacheWrite5m: id.Usage.CacheWrite5m, CacheWrite1h: id.Usage.CacheWrite1h,
			Sealed: body,
		})
	}
	if _, err := n.db.AppendLocal(context.Background(), recs); err != nil {
		t.Fatalf("AppendLocal: %v", err)
	}
}

// The promise the whole archive rests on: the sealed body is the truth, and
// every extracted column can be recomputed from it. This is that promise
// exercised -- records stored as an older build stored them, with the columns
// blank, are made complete without re-reading a single transcript.
func TestReprocessingRecoversWhatWasNeverExtracted(t *testing.T) {
	n := newNode(t)
	ctx := context.Background()

	appendAsAnOlderBuildWould(t, n,
		dayLineIn("2026-09-01", `D:\fastcached`, "master", "s1", "r1", "claude-opus-5", 1000),
		prLink("s1", "acme/widgets", 7))

	// Before: no project, and no pull request to join against.
	before, err := n.derive.Daily(ctx)
	if err != nil {
		t.Fatalf("Daily: %v", err)
	}
	if got := shareOf(before.Days[0], derive.StackProject, derive.NoneKey); got <= 0 {
		t.Fatalf("the un-extracted record already has a project; the fixture is not reproducing the old state")
	}

	stats, err := reprocessorFor(t, n).Pass(ctx)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if stats.Updated != 2 {
		t.Errorf("updated %d of 2 records", stats.Updated)
	}

	after, err := n.derive.Daily(ctx)
	if err != nil {
		t.Fatalf("Daily: %v", err)
	}
	d := after.Days[0]
	if got := shareOf(d, derive.StackProject, "fastcached"); math.Abs(got-d.CostUSD) > 1e-9 {
		t.Errorf("fastcached got $%.6f of the day, want all $%.6f", got, d.CostUSD)
	}
	// The pull-request column came back too, which is the half of this that
	// nothing else can repair.
	if got := shareOf(d, derive.StackPullRequest, "acme/widgets#7"); math.Abs(got-d.CostUSD) > 1e-9 {
		t.Errorf("the pull request got $%.6f, want all $%.6f", got, d.CostUSD)
	}
}

// A second pass must find nothing to do. Otherwise every start would rewrite
// the whole archive -- two gigabytes of page churn to reach the answer it
// already had.
func TestReprocessingASecondTimeChangesNothing(t *testing.T) {
	n := newNode(t)
	ctx := context.Background()

	if _, err := n.writer.Ingest(ctx, records(
		dayLine("2026-09-01", "s1", "r1", "claude-opus-5", 1000),
		dayLine("2026-09-01", "s2", "r2", "claude-opus-5", 500),
		prLink("s1", "acme/widgets", 1),
	)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	redo := reprocessorFor(t, n)
	first, err := redo.Pass(ctx)
	if err != nil {
		t.Fatalf("first Pass: %v", err)
	}
	// Ingest already extracted everything, so even the first pass rewrites
	// nothing: it only walks.
	if first.Updated != 0 {
		t.Errorf("rewrote %d records that were already correct", first.Updated)
	}

	second, err := redo.Pass(ctx)
	if err != nil {
		t.Fatalf("second Pass: %v", err)
	}
	if second.Examined != 0 || second.Updated != 0 {
		t.Errorf("a completed archive was walked again: examined %d, updated %d",
			second.Examined, second.Updated)
	}
	if pending, err := n.db.ExtractionPending(ctx, ingest.ExtractionVersion); err != nil {
		t.Fatalf("ExtractionPending: %v", err)
	} else if pending != 0 {
		t.Errorf("%d records still reported as pending", pending)
	}
}

// stopAfterFirstBatch is the archive with a shutdown wired into it: it cancels
// the pass the moment one batch has been committed, which is the only way to
// observe a half-finished walk deterministically.
type stopAfterFirstBatch struct {
	*store.DB
	cancel func()
	saves  int
}

func (a *stopAfterFirstBatch) SaveExtracted(ctx context.Context, version int, origin string, upTo int64, rows []store.Extracted) error {
	if err := a.DB.SaveExtracted(ctx, version, origin, upTo, rows); err != nil {
		return err
	}
	a.saves++
	if a.saves == 1 {
		a.cancel()
	}
	return nil
}

// A pass that is interrupted must resume, not restart. The mark advances in the
// transaction that commits the batch it covers, so a daemon stopped mid-walk
// costs the rest of the walk and nothing that was already done -- the same
// discipline a replication watermark follows, and for the same reason:
// advancing first and dying loses the work permanently and invisibly.
func TestReprocessingResumesWhereItStopped(t *testing.T) {
	n := newNode(t)
	ctx := context.Background()

	var lines []string
	for i := range 6 {
		lines = append(lines, dayLine("2026-09-01", "s1",
			fmt.Sprintf("r%d", i), "claude-opus-5", 100))
	}
	if _, err := n.writer.Ingest(ctx, records(lines...)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	total, err := n.db.ExtractionPending(ctx, ingest.ExtractionVersion)
	if err != nil {
		t.Fatalf("ExtractionPending: %v", err)
	}
	if total == 0 {
		t.Fatal("nothing to extract; the fixture proves nothing")
	}

	stopped, cancel := context.WithCancel(ctx)
	defer cancel()
	interrupted, err := ingest.NewReprocessor(ingest.ReprocessConfig{
		Archive: &stopAfterFirstBatch{DB: n.db, cancel: cancel},
		Keys:    n.keys,
		Batch:   2,
	})
	if err != nil {
		t.Fatalf("NewReprocessor: %v", err)
	}
	// A cancelled pass says so; what matters is that it committed what it had.
	if _, err := interrupted.Pass(stopped); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted Pass returned %v, want context.Canceled", err)
	}

	// Some progress is recorded, and the rest is still owed.
	partial, err := n.db.ExtractionPending(ctx, ingest.ExtractionVersion)
	if err != nil {
		t.Fatalf("ExtractionPending: %v", err)
	}
	if partial == 0 {
		t.Fatal("the interrupted pass reported the whole archive done")
	}
	if partial >= total {
		t.Errorf("%d of %d records pending after a partial pass; nothing was committed",
			partial, total)
	}

	if _, err := reprocessorFor(t, n).Pass(ctx); err != nil {
		t.Fatalf("resumed Pass: %v", err)
	}
	if pending, err := n.db.ExtractionPending(ctx, ingest.ExtractionVersion); err != nil {
		t.Fatalf("ExtractionPending: %v", err)
	} else if pending != 0 {
		t.Errorf("%d records still pending after resuming", pending)
	}
}

// Re-extraction must not touch the body, its hash, or the columns a record is
// keyed by. If it did, a node that had reprocessed would look forked to one
// that had not -- silent data loss reporting itself as a fault in the other
// machine.
//
// That the digests are unchanged is asserted where the write happens, in
// store's TestSaveExtractedLeavesTheBodyAndTheKeysAlone. This is the same
// property one level up: that the reprocessor asks for nothing more than that.
func TestReprocessingLeavesReplicationAlone(t *testing.T) {
	n := newNode(t)
	ctx := context.Background()

	// Unextracted, so the pass has real writes to make: a record that already
	// holds the right answer is skipped, and a test over those would assert
	// that nothing changed when nothing was written.
	appendAsAnOlderBuildWould(t, n,
		dayLine("2026-09-01", "s1", "r1", "claude-opus-5", 1000),
		prLink("s1", "acme/widgets", 1))

	before, err := n.db.Since(ctx, n.db.OriginID(), 0, 100)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}

	stats, err := reprocessorFor(t, n).Pass(ctx)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if stats.Updated == 0 {
		t.Fatal("the pass rewrote nothing, so this asserts nothing")
	}

	after, err := n.db.Since(ctx, n.db.OriginID(), 0, 100)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("the archive holds %d records, held %d", len(after), len(before))
	}
	for i := range before {
		if after[i].ContentHash != before[i].ContentHash {
			t.Errorf("record %d's content hash changed", i)
		}
		if string(after[i].Sealed) != string(before[i].Sealed) {
			t.Errorf("record %d's sealed body changed", i)
		}
		if after[i].SessionID != before[i].SessionID || after[i].LineUUID != before[i].LineUUID ||
			after[i].RequestID != before[i].RequestID || after[i].Model != before[i].Model {
			t.Errorf("record %d's keying columns changed", i)
		}
	}

}

// Two nodes holding the same records must draw the same legend, byte for byte.
// A difference here does not read as a bug: it reads as two machines disagreeing
// about what the archive says, which is the one thing replication exists to
// rule out.
func TestReplicasAgreeOnTheProjectLegend(t *testing.T) {
	psk, err := seal.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	a := newNodeWithKey(t, psk)
	b := newNodeWithKey(t, psk)
	ctx := context.Background()

	if _, err := a.writer.Ingest(ctx, records(
		dayLineIn("2026-09-01", `D:\fastcached`, "master", "s1", "r1", "claude-opus-5", 1000),
		dayLineIn("2026-09-01", `D:\fastcached\.claude\worktrees\issue-124`, "feature/124", "s2", "r2", "claude-opus-5", 700),
		dayLineIn("2026-09-01", `D:\fastcached-wt-139`, "feature/139", "s3", "r3", "claude-opus-5", 400),
		dayLineIn("2026-09-01", `D:\endo`, "master", "s4", "r4", "claude-opus-5", 250),
		prLink("s1", "acme/widgets", 1),
	)); err != nil {
		t.Fatalf("Ingest on A: %v", err)
	}

	shipped, err := a.db.Since(ctx, a.db.OriginID(), 0, 1000)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if _, err := b.db.AppendRemote(ctx, shipped); err != nil {
		t.Fatalf("AppendRemote on B: %v", err)
	}

	fromA, err := a.derive.Daily(ctx)
	if err != nil {
		t.Fatalf("Daily on A: %v", err)
	}
	fromB, err := b.derive.Daily(ctx)
	if err != nil {
		t.Fatalf("Daily on B: %v", err)
	}

	// Every dimension except the machine, which is the one that is legitimately
	// a property of who is looking.
	for _, stack := range []derive.Stack{
		derive.StackModel, derive.StackProject, derive.StackBranch,
		derive.StackPullRequest, derive.StackSession,
	} {
		wantJSON, err := json.Marshal(legendFor(t, fromA, stack))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		gotJSON, err := json.Marshal(legendFor(t, fromB, stack))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("the %q legends differ between replicas:\n  A: %s\n  B: %s",
				stack, wantJSON, gotJSON)
		}
	}
}

// A pull request opened on one machine has to be visible on every other one.
// It was not: the wire record carried no pr_repo, so the receiving node stored
// the link as NULL and its delivery report had nothing to join against --
// silently, for as long as the columns existed.
func TestPullRequestLinksSurviveReplication(t *testing.T) {
	psk, err := seal.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	a := newNodeWithKey(t, psk)
	b := newNodeWithKey(t, psk)
	ctx := context.Background()

	if _, err := a.writer.Ingest(ctx, records(
		dayLineIn("2026-09-01", `D:\fastcached`, "master", "s1", "r1", "claude-opus-5", 1000),
		prLink("s1", "acme/widgets", 42),
	)); err != nil {
		t.Fatalf("Ingest on A: %v", err)
	}
	shipped, err := a.db.Since(ctx, a.db.OriginID(), 0, 1000)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if _, err := b.db.AppendRemote(ctx, shipped); err != nil {
		t.Fatalf("AppendRemote on B: %v", err)
	}

	delivery, err := b.derive.Deliveries(ctx)
	if err != nil {
		t.Fatalf("Deliveries on B: %v", err)
	}
	if len(delivery.PullRequests) != 1 {
		t.Fatalf("B sees %d pull requests, want the one A opened", len(delivery.PullRequests))
	}
	if got := delivery.PullRequests[0].Number; got != 42 {
		t.Errorf("B sees pull request %d, want 42", got)
	}
}
