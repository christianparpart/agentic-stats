// Package ingest writes collected lines into this node's local replica.
//
// It is the local end of collector.Uploader: where the previous design posted
// batches to a server, a node now writes to its own database and lets the mesh
// replicate. The collector itself did not change — its Uploader interface
// carries no transport in its signature.
package ingest

import (
	"context"
	"errors"
	"fmt"

	"github.com/christianparpart/agentic-stats/internal/derive"
	"github.com/christianparpart/agentic-stats/internal/seal"
	"github.com/christianparpart/agentic-stats/internal/store"
	"github.com/christianparpart/agentic-stats/internal/wire"
)

// maxBatchRecords bounds one call, so neither a runaway collector nor a peer
// can force an unbounded transaction.
const maxBatchRecords = 50_000

// Writer stores records locally.
type Writer struct {
	db   *store.DB
	keys *seal.Keys
}

// NewWriter returns a Writer backed by db, sealing payloads with keys.
func NewWriter(db *store.DB, keys *seal.Keys) (*Writer, error) {
	if db == nil {
		return nil, errors.New("ingest: a store is required")
	}
	if keys == nil {
		return nil, errors.New("ingest: keys are required")
	}
	return &Writer{db: db, keys: keys}, nil
}

// Ingest implements collector.Uploader.
//
// Every payload is sealed before it reaches the disk, so the database file is
// worthless without the pre-shared key.
func (w *Writer) Ingest(ctx context.Context, records []wire.Record) (wire.IngestResult, error) {
	if len(records) == 0 {
		return wire.IngestResult{}, nil
	}
	if len(records) > maxBatchRecords {
		return wire.IngestResult{}, fmt.Errorf("ingest: batch of %d exceeds limit of %d",
			len(records), maxBatchRecords)
	}

	recs := make([]store.Record, 0, len(records))
	for _, r := range records {
		sealed, err := w.keys.SealPayload([]byte(r.Data))
		if err != nil {
			return wire.IngestResult{}, fmt.Errorf("ingest: seal record: %w", err)
		}
		id := derive.Identify([]byte(r.Data))
		recs = append(recs, store.Record{
			Source:       r.Source,
			Path:         r.Path,
			ByteOffset:   r.Offset,
			ContentHash:  r.Hash,
			SessionID:    id.SessionID,
			LineUUID:     id.LineUUID,
			CapturedAt:   id.CapturedAt,
			RequestID:    id.RequestID,
			Model:        id.Model,
			Input:        id.Usage.Input,
			Output:       id.Usage.Output,
			Thinking:     id.Usage.Thinking,
			CacheRead:    id.Usage.CacheRead,
			CacheWrite5m: id.Usage.CacheWrite5m,
			CacheWrite1h: id.Usage.CacheWrite1h,
			Sealed:       sealed,
		})
	}

	res, err := w.db.AppendLocal(ctx, recs)
	if err != nil {
		return wire.IngestResult{}, err
	}
	return wire.IngestResult{
		Received:   len(records),
		Stored:     res.Stored,
		Duplicates: res.Duplicates,
	}, nil
}
