// Package store owns the Postgres connection and the tenant boundary.
//
// Every query that touches tenant data runs inside InTenantTx, which sets the
// app.user_id that row-level security reads. Queries that must run before a
// tenant is known -- resolving a bearer token to its owner -- go through
// InAuthTx, which is deliberately named so that its use stands out in review.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config is everything a DB needs.
type Config struct {
	// URL is the Postgres connection string. Required.
	URL string
	// MaxConns bounds the pool. Zero uses the driver default.
	MaxConns int32
	// ConnectTimeout bounds the initial connection. Zero selects a default.
	ConnectTimeout time.Duration
}

// pgxTx is the transaction interface used across this package.
type pgxTx = pgx.Tx

// DB is a pooled Postgres connection with tenant-aware transaction helpers.
type DB struct {
	pool *pgxpool.Pool
}

// Open connects to Postgres and verifies the connection.
func Open(ctx context.Context, cfg Config) (*DB, error) {
	if cfg.URL == "" {
		return nil, errors.New("store: Config.URL is required")
	}
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("store: parse connection string: %w", err)
	}
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}

	timeout := cfg.ConnectTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	connectCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(connectCtx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(connectCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &DB{pool: pool}, nil
}

// Close releases the pool.
func (db *DB) Close() { db.pool.Close() }

// Pool exposes the underlying pool for migrations and health checks.
func (db *DB) Pool() *pgxpool.Pool { return db.pool }

// InTenantTx runs fn in a transaction scoped to one user.
//
// It sets app.user_id with SET LOCAL, so the setting is bound to the
// transaction and cannot leak to the next borrower of the pooled connection.
// Row-level security reads that setting; a query that escapes this helper sees
// no rows rather than every tenant's rows.
func (db *DB) InTenantTx(ctx context.Context, userID string, fn func(context.Context, pgx.Tx) error) error {
	if userID == "" {
		return errors.New("store: InTenantTx requires a user id")
	}
	return db.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.user_id', $1, true)", userID); err != nil {
			return fmt.Errorf("store: establish tenant: %w", err)
		}
		return fn(ctx, tx)
	})
}

// InAuthTx runs fn without a tenant established.
//
// Reserved for resolving a credential to its owner, which by definition cannot
// know the tenant beforehand. Everything else belongs in InTenantTx.
func (db *DB) InAuthTx(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	return db.inTx(ctx, fn)
}

// inTx wraps fn in a transaction, rolling back on error or panic.
func (db *DB) inTx(ctx context.Context, fn func(context.Context, pgx.Tx) error) (err error) {
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			// Roll back before re-panicking so the connection is not returned
			// to the pool mid-transaction.
			_ = tx.Rollback(ctx)
			panic(p)
		}
		if err != nil {
			err = errors.Join(err, ignoreTxClosed(tx.Rollback(ctx)))
		}
	}()

	if err = fn(ctx, tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// ignoreTxClosed drops the benign error from rolling back an already-finished
// transaction, so it does not mask the real failure.
func ignoreTxClosed(err error) error {
	if err == nil || errors.Is(err, pgx.ErrTxClosed) {
		return nil
	}
	return err
}
