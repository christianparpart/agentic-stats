// Package storetest connects integration tests to a real Postgres.
//
// These tests exercise behaviour that cannot be faked meaningfully: row-level
// security, ON CONFLICT deduplication, and the JSON extraction the fold relies
// on. A stub would prove nothing about any of them.
package storetest

import (
	"context"
	"os"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/store"
)

// EnvURL names the environment variable holding the test connection string.
//
// It must point at a NON-superuser role: superusers bypass row-level security,
// so an isolation test run as one would pass while proving nothing.
const EnvURL = "AGENTIC_STATS_TEST_DATABASE_URL"

// Open returns a migrated database, skipping the test when none is configured.
func Open(t *testing.T) *store.DB {
	t.Helper()

	url := os.Getenv(EnvURL)
	if url == "" {
		t.Skipf("set %s to run integration tests", EnvURL)
	}

	ctx := context.Background()
	db, err := store.Open(ctx, store.Config{URL: url})
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(db.Close)

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	Reset(t, db)
	return db
}

// Reset empties every table so tests do not observe each other's rows.
func Reset(t *testing.T, db *store.DB) {
	t.Helper()
	ctx := context.Background()
	// Truncating users cascades to devices, tokens and raw_lines.
	if _, err := db.Pool().Exec(ctx, `TRUNCATE users CASCADE`); err != nil {
		t.Fatalf("reset test database: %v", err)
	}
}
