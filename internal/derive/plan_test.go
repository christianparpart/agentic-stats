package derive

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/store"
)

// The dashboard's queries must be answerable from an index alone.
//
// This is not a micro-optimisation to protect. `records` is WITHOUT ROWID and
// carries the sealed body inline, so a row is kilobytes and an index entry
// that has to be resolved against the table costs a descent into a
// multi-gigabyte b-tree. On a 480k-record archive the difference measured 6.4
// seconds against 0.28 for one report, and the dashboard asks for three at
// once -- the failure mode is a page that renders blank because nothing has
// come back yet.
//
// A plan assertion catches the regression that a timing assertion cannot: it
// fails on a laptop with a warm cache and an empty test database, the moment
// someone adds a column to the fold that `records_fold` does not carry.
func TestTheDashboardQueriesAreAnsweredFromIndexesAlone(t *testing.T) {
	db := openArchive(t)

	queries := []struct{ name, sql string }{
		{"model usage", modelUsageQuery},
		{"daily usage", dailyUsageQuery},
		{"session links", sessionLinksQuery},
		{"deliveries", deliveriesQuery},
		{"attribution", attributionQuery},
	}
	for _, q := range queries {
		t.Run(q.name, func(t *testing.T) {
			for _, step := range queryPlan(t, db, q.sql) {
				if !touchesRecords(step) {
					continue
				}
				if !strings.Contains(step, "COVERING INDEX") {
					t.Errorf("reads whole records rows:\n  %s\n"+
						"widen the covering index or stop selecting the column that broke it", step)
				}
			}
		})
	}
}

// The fold must not be sorted into place. GROUP BY request_id is free while an
// index leads with request_id, and becomes a temp b-tree over every assistant
// record the moment one does not.
func TestTheFoldGroupsWithoutSorting(t *testing.T) {
	db := openArchive(t)
	for _, step := range queryPlan(t, db, modelUsageQuery) {
		if strings.Contains(step, "TEMP B-TREE FOR GROUP BY") &&
			strings.Contains(step, "request") {
			t.Errorf("the fold sorts to group: %s", step)
		}
	}
}

// touchesRecords reports whether a plan step reads the records table itself,
// as opposed to a CTE or an index whose name merely contains the word.
func touchesRecords(step string) bool {
	s := strings.TrimSpace(step)
	return strings.HasPrefix(s, "SCAN records") || strings.HasPrefix(s, "SEARCH records")
}

func queryPlan(t *testing.T, db *store.DB, query string) []string {
	t.Helper()
	rows, err := db.SQL().QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer func() { _ = rows.Close() }() // rows fully drained below

	var steps []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		steps = append(steps, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("the planner returned no steps")
	}
	return steps
}

// openArchive gives a migrated, empty archive. Empty is deliberate: nothing
// runs ANALYZE, in tests or in production, so the planner works from the same
// schema-only defaults either way and the plan here is the plan on a real node.
func openArchive(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), store.Config{
		Path: filepath.Join(t.TempDir(), "archive.db"),
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() }) // test cleanup
	return db
}
