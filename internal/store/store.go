// Package store is the node's local replica: an embedded SQLite database
// holding every record this node has collected or received from a peer.
//
// There is no tenant column and no row-level security. Membership is the
// pre-shared key, and one node holds one mesh. What replaces RLS as the
// at-rest protection is that record bodies arrive here already sealed.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Config is everything a DB needs.
type Config struct {
	// Path is the database file. ":memory:" is accepted for tests.
	Path string
	// Clock supplies timestamps. Zero uses the system clock.
	Clock func() time.Time
}

// DB is the local replica.
type DB struct {
	sql    *sql.DB
	now    func() time.Time
	origin string
}

// Open connects, applies migrations, and establishes this node's identity.
func Open(ctx context.Context, cfg Config) (*DB, error) {
	if cfg.Path == "" {
		return nil, errors.New("store: Config.Path is required")
	}
	now := cfg.Clock
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	// WAL and a real busy timeout are not optional at this write volume.
	dsn := "file:" + cfg.Path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(10000)" +
		"&_pragma=foreign_keys(ON)"

	handle, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", cfg.Path, err)
	}
	if err := handle.PingContext(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("store: ping %s: %w", cfg.Path, err), handle.Close())
	}

	db := &DB{sql: handle, now: now}
	if err := db.migrate(ctx); err != nil {
		return nil, errors.Join(err, handle.Close())
	}
	if err := db.loadOrMintIdentity(ctx); err != nil {
		return nil, errors.Join(err, handle.Close())
	}
	return db, nil
}

// Close releases the database.
func (db *DB) Close() error {
	if err := db.sql.Close(); err != nil {
		return fmt.Errorf("store: close: %w", err)
	}
	return nil
}

// OriginID is this node's replication identity.
func (db *DB) OriginID() string { return db.origin }

// SQL exposes the handle for read-only queries in derive.
func (db *DB) SQL() *sql.DB { return db.sql }

// Now returns the configured clock's time.
func (db *DB) Now() time.Time { return db.now() }

// inTx runs fn in a transaction, rolling back on error or panic.
//
// Lifted from the previous Postgres store: the discipline is identical and was
// already correct.
func (db *DB) inTx(ctx context.Context, fn func(context.Context, *sql.Tx) error) (err error) {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			// Roll back before re-panicking so the connection is not left
			// mid-transaction.
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			err = errors.Join(err, ignoreTxDone(tx.Rollback()))
		}
	}()

	if err = fn(ctx, tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// ignoreTxDone drops the benign error from rolling back a finished
// transaction, so it cannot mask the real failure.
func ignoreTxDone(err error) error {
	if err == nil || errors.Is(err, sql.ErrTxDone) {
		return nil
	}
	return err
}

// migrate applies every migration not yet recorded, in filename order.
func (db *DB) migrate(ctx context.Context) error {
	_, err := db.sql.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`)
	if err != nil {
		return fmt.Errorf("store: create migration table: %w", err)
	}

	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("store: list migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var applied int
		row := db.sql.QueryRowContext(ctx,
			`SELECT count(*) FROM schema_migrations WHERE name = ?`, name)
		if err := row.Scan(&applied); err != nil {
			return fmt.Errorf("store: check migration %s: %w", name, err)
		}
		if applied > 0 {
			continue
		}
		body, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("store: read migration %s: %w", name, err)
		}
		// The migration and its record commit together, so a failure part-way
		// leaves neither the change nor the claim that it was applied.
		err = db.inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				return fmt.Errorf("store: apply migration %s: %w", name, err)
			}
			_, err := tx.ExecContext(ctx,
				`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`,
				name, db.now().Format(time.RFC3339Nano))
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}
