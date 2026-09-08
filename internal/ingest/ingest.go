// Package ingest stores raw lines. It does not interpret them.
//
// Whether a line is valid JSON, which session it belongs to, and what its
// tokens cost are all questions for internal/derive, asked later against the
// archive. Keeping ingest incurious is what lets collection survive a format
// change: an unrecognized line is still stored, and a parser written years
// from now can still reach it.
package ingest

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/christianparpart/agentic-stats/internal/auth"
	"github.com/christianparpart/agentic-stats/internal/store"
	"github.com/christianparpart/agentic-stats/internal/wire"
)

// maxBatchRecords bounds one request, so a single client cannot force the
// server to buffer without limit.
const maxBatchRecords = 50_000

// Service stores incoming batches.
type Service struct {
	db *store.DB
}

// NewService returns a Service backed by db.
func NewService(db *store.DB) (*Service, error) {
	if db == nil {
		return nil, errors.New("ingest: a database is required")
	}
	return &Service{db: db}, nil
}

// Store writes a batch and reports how much of it was new.
//
// Duplicates are expected, not exceptional: the collector advances its cursor
// only after acknowledgement, so any interrupted upload is retried in full.
func (s *Service) Store(
	ctx context.Context, dev auth.Device, header wire.Header, records []wire.Record,
) (wire.IngestResult, error) {
	if len(records) == 0 {
		return wire.IngestResult{}, nil
	}
	if len(records) > maxBatchRecords {
		return wire.IngestResult{}, fmt.Errorf("ingest: batch of %d exceeds limit of %d",
			len(records), maxBatchRecords)
	}

	sources := make([]string, len(records))
	paths := make([]string, len(records))
	offsets := make([]int64, len(records))
	hashes := make([]string, len(records))
	raws := make([]string, len(records))
	for i, r := range records {
		sources[i] = r.Source
		paths[i] = r.Path
		offsets[i] = r.Offset
		hashes[i] = r.Hash
		raws[i] = r.Data
	}

	var stored int
	err := s.db.InTenantTx(ctx, dev.UserID, func(ctx context.Context, tx pgx.Tx) error {
		// Refresh what the machine reports about itself, so an OS upgrade or a
		// timezone move is picked up without a re-enrollment.
		if _, err := tx.Exec(ctx, `
			UPDATE devices
			   SET last_seen = now(), timezone = $2, agent_version = $3
			 WHERE id = $1::uuid`,
			dev.ID, header.Device.Timezone, header.Device.AgentVersion); err != nil {
			return fmt.Errorf("ingest: touch device: %w", err)
		}

		tag, err := tx.Exec(ctx, `
			INSERT INTO raw_lines
			    (user_id, device_id, source, path, byte_offset, content_hash, raw)
			SELECT $1::uuid, $2::uuid, u.source, u.path, u.byte_offset, u.content_hash, u.raw
			  FROM unnest($3::text[], $4::text[], $5::bigint[], $6::text[], $7::text[])
			       AS u(source, path, byte_offset, content_hash, raw)
			ON CONFLICT DO NOTHING`,
			dev.UserID, dev.ID, sources, paths, offsets, hashes, raws)
		if err != nil {
			return fmt.Errorf("ingest: store lines: %w", err)
		}
		stored = int(tag.RowsAffected())
		return nil
	})
	if err != nil {
		return wire.IngestResult{}, err
	}

	return wire.IngestResult{
		Received:   len(records),
		Stored:     stored,
		Duplicates: len(records) - stored,
	}, nil
}
