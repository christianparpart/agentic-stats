package store_test

import (
	"context"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/store"
)

// A mark may only stand for work that is actually committed.
//
// Both in one transaction for the reason a replication watermark is: a mark
// that ran ahead of its writes would report an archive as fully extracted while
// part of it silently never was, and nothing would ever look at those rows
// again.
func TestTheExtractionMarkAdvancesWithItsBatch(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	if _, err := db.AppendLocal(ctx, []store.Record{rec(1), rec(2), rec(3)}); err != nil {
		t.Fatalf("AppendLocal: %v", err)
	}
	origin := db.OriginID()

	// Nothing walked yet, so everything is owed.
	pending, err := db.ExtractionPending(ctx, 1)
	if err != nil {
		t.Fatalf("ExtractionPending: %v", err)
	}
	if pending != 3 {
		t.Errorf("%d records pending, want 3", pending)
	}

	err = db.SaveExtracted(ctx, 1, origin, 2, []store.Extracted{
		{OriginID: origin, Seq: 1, CWD: `D:\fastcached`, GitBranch: "master"},
	})
	if err != nil {
		t.Fatalf("SaveExtracted: %v", err)
	}

	marks, err := db.ExtractionMarks(ctx, 1)
	if err != nil {
		t.Fatalf("ExtractionMarks: %v", err)
	}
	if marks[origin] != 2 {
		t.Errorf("mark is at %d, want the last sequence examined (2)", marks[origin])
	}
	if pending, err = db.ExtractionPending(ctx, 1); err != nil {
		t.Fatalf("ExtractionPending: %v", err)
	} else if pending != 1 {
		t.Errorf("%d records pending, want the one past the mark", pending)
	}

	// A different version has walked nothing, which is what makes a new
	// extracted column a version bump rather than a migration.
	if pending, err = db.ExtractionPending(ctx, 2); err != nil {
		t.Fatalf("ExtractionPending: %v", err)
	} else if pending != 3 {
		t.Errorf("version 2 reports %d pending, want the whole archive", pending)
	}
}

// The mark never goes backwards, so two passes racing cannot lose ground
// already covered.
func TestTheExtractionMarkNeverRetreats(t *testing.T) {
	db := open(t)
	ctx := context.Background()
	if _, err := db.AppendLocal(ctx, []store.Record{rec(1), rec(2), rec(3)}); err != nil {
		t.Fatalf("AppendLocal: %v", err)
	}
	origin := db.OriginID()

	if err := db.SaveExtracted(ctx, 1, origin, 3, nil); err != nil {
		t.Fatalf("SaveExtracted: %v", err)
	}
	if err := db.SaveExtracted(ctx, 1, origin, 1, nil); err != nil {
		t.Fatalf("SaveExtracted: %v", err)
	}
	marks, err := db.ExtractionMarks(ctx, 1)
	if err != nil {
		t.Fatalf("ExtractionMarks: %v", err)
	}
	if marks[origin] != 3 {
		t.Errorf("mark is at %d after a stale save, want 3", marks[origin])
	}
}

// Re-extraction fills columns in. It must not touch the sealed body, the
// content hash, or any column a peer keys or compares on -- a changed hash is
// how a fork is detected, so rewriting one would make a node that had
// reprocessed look forked to one that had not.
func TestSaveExtractedLeavesTheBodyAndTheKeysAlone(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	if _, err := db.AppendLocal(ctx, []store.Record{rec(1)}); err != nil {
		t.Fatalf("AppendLocal: %v", err)
	}
	origin := db.OriginID()
	before, err := db.Since(ctx, origin, 0, 10)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	digestsBefore, err := db.Digests(ctx, origin)
	if err != nil {
		t.Fatalf("Digests: %v", err)
	}

	err = db.SaveExtracted(ctx, 1, origin, 1, []store.Extracted{{
		OriginID: origin, Seq: 1,
		CWD: `D:\fastcached`, GitBranch: "master",
		PRRepo: "acme/widgets", PRNumber: 9,
	}})
	if err != nil {
		t.Fatalf("SaveExtracted: %v", err)
	}

	after, err := db.Since(ctx, origin, 0, 10)
	if err != nil {
		t.Fatalf("Since: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("the archive holds %d records, want 1", len(after))
	}

	// What must have changed.
	if after[0].CWD != `D:\fastcached` || after[0].GitBranch != "master" {
		t.Errorf("the extracted columns were not written: %+v", after[0])
	}
	if after[0].PRRepo != "acme/widgets" || after[0].PRNumber != 9 {
		t.Errorf("the pull-request columns were not written: %+v", after[0])
	}

	// What must not have.
	if string(after[0].Sealed) != string(before[0].Sealed) {
		t.Error("the sealed body changed")
	}
	if after[0].ContentHash != before[0].ContentHash {
		t.Error("the content hash changed, so a peer would now see a fork")
	}
	if after[0].SessionID != before[0].SessionID || after[0].LineUUID != before[0].LineUUID ||
		after[0].RequestID != before[0].RequestID || after[0].Model != before[0].Model ||
		after[0].Output != before[0].Output {
		t.Errorf("a keying or usage column changed:\n got %+v\nwant %+v", after[0], before[0])
	}

	digestsAfter, err := db.Digests(ctx, origin)
	if err != nil {
		t.Fatalf("Digests: %v", err)
	}
	if len(digestsAfter) != len(digestsBefore) {
		t.Fatalf("digest count changed")
	}
	for i := range digestsBefore {
		if digestsAfter[i] != digestsBefore[i] {
			t.Error("a digest changed; re-extraction must be invisible to peers")
		}
	}
}

// An empty archive owes nothing, and must say so rather than fail on a NULL
// sum.
func TestAnEmptyArchiveOwesNoExtraction(t *testing.T) {
	db := open(t)
	pending, err := db.ExtractionPending(context.Background(), 1)
	if err != nil {
		t.Fatalf("ExtractionPending: %v", err)
	}
	if pending != 0 {
		t.Errorf("%d records pending on an empty archive", pending)
	}
}
