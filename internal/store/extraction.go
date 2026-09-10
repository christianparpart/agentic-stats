package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Extracted is the identity material re-derived from one record's sealed body.
//
// Only the columns a re-extraction may safely rewrite. Deliberately not
// session_id or line_uuid, which are the semantic unique key and could collide;
// not request_id, which is the fold key; and not the model or the token counts,
// which no change here affects. Rewriting an archive's keying is a different and
// much harder operation than filling in a column, and this is not it.
type Extracted struct {
	OriginID  string
	Seq       int64
	CWD       string
	GitBranch string
	PRRepo    string
	PRNumber  int64
}

// ExtractionMarks reports how far extraction at this version has walked each
// origin. An origin with no mark has not been walked at all.
func (db *DB) ExtractionMarks(ctx context.Context, version int) (map[string]int64, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT origin_id, seq FROM extraction WHERE version = ?`, version)
	if err != nil {
		return nil, fmt.Errorf("store: read extraction marks: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows fully drained below

	marks := make(map[string]int64)
	for rows.Next() {
		var origin string
		var seq int64
		if err := rows.Scan(&origin, &seq); err != nil {
			return nil, fmt.Errorf("store: scan extraction mark: %w", err)
		}
		marks[origin] = seq
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read extraction marks: %w", err)
	}
	return marks, nil
}

// SaveExtracted writes re-derived columns and advances the origin's mark to
// upTo, which is the last sequence *examined* rather than the last one changed.
//
// Both in one transaction, for the same reason a replication watermark advances
// with the records it covers: a mark that ran ahead of its writes would report
// an archive as fully extracted while some of it silently never was.
//
// upTo is the last examined sequence because a run of records that needed no
// change still has to count as walked. A mark that only tracked the rows it
// rewrote would re-read them on every pass forever.
func (db *DB) SaveExtracted(ctx context.Context, version int, origin string, upTo int64, rows []Extracted) error {
	return db.inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if len(rows) > 0 {
			stmt, err := tx.PrepareContext(ctx, `
				UPDATE records
				   SET cwd = ?, git_branch = ?, pr_repo = ?, pr_number = ?
				 WHERE origin_id = ? AND seq = ?`)
			if err != nil {
				return fmt.Errorf("store: prepare extraction update: %w", err)
			}
			defer func() { _ = stmt.Close() }() // statement is scoped to this tx

			for _, r := range rows {
				if _, err := stmt.ExecContext(ctx,
					nullable(r.CWD), nullable(r.GitBranch),
					nullable(r.PRRepo), nullableInt(r.PRNumber),
					r.OriginID, r.Seq); err != nil {
					return fmt.Errorf("store: update extracted columns: %w", err)
				}
			}
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO extraction (origin_id, version, seq, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (origin_id, version) DO UPDATE SET
			    seq = max(seq, excluded.seq), updated_at = excluded.updated_at`,
			origin, version, upTo, db.now().Format(time.RFC3339Nano))
		if err != nil {
			return fmt.Errorf("store: advance extraction mark: %w", err)
		}
		return nil
	})
}

// ExtractionPending is how many records are still to be walked at this version.
//
// The version vector minus the marks, so it needs neither a scan nor a
// predicate over the archive itself: both sides are small grouped reads. It is
// what lets `status` answer "why does my chart still say No project".
func (db *DB) ExtractionPending(ctx context.Context, version int) (int64, error) {
	var pending sql.NullInt64
	err := db.sql.QueryRowContext(ctx, `
		SELECT sum(held.high - coalesce(e.seq, 0))
		  FROM (SELECT origin_id, max(seq) AS high FROM records GROUP BY origin_id) AS held
		  LEFT JOIN extraction AS e
		         ON e.origin_id = held.origin_id AND e.version = ?`, version).Scan(&pending)
	if err != nil {
		return 0, fmt.Errorf("store: count pending extraction: %w", err)
	}
	// A mark can sit above the origin's highest sequence only if records were
	// removed, which nothing does; clamp anyway rather than report a negative.
	if pending.Int64 < 0 {
		return 0, nil
	}
	return pending.Int64, nil
}
