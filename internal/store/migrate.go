package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrate applies every migration that has not yet been recorded.
//
// Migrations are applied in filename order and recorded in schema_migrations,
// so running this on every server start is safe and idempotent.
func (db *DB) Migrate(ctx context.Context) error {
	_, err := db.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       text        PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("store: create migration table: %w", err)
	}

	names, err := migrationNames()
	if err != nil {
		return err
	}

	for _, name := range names {
		var applied bool
		err := db.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, name).Scan(&applied)
		if err != nil {
			return fmt.Errorf("store: check migration %s: %w", name, err)
		}
		if applied {
			continue
		}

		body, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("store: read migration %s: %w", name, err)
		}
		if err := db.applyMigration(ctx, name, string(body)); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration runs one migration and records it in the same transaction, so
// a failure part-way leaves neither the change nor the record behind.
func (db *DB) applyMigration(ctx context.Context, name, body string) error {
	return db.inTx(ctx, func(ctx context.Context, tx pgxTx) error {
		if _, err := tx.Exec(ctx, body); err != nil {
			return fmt.Errorf("store: apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (name) VALUES ($1)`, name); err != nil {
			return fmt.Errorf("store: record migration %s: %w", name, err)
		}
		return nil
	})
}

// migrationNames lists the embedded migrations in application order.
func migrationNames() ([]string, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: list migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}
