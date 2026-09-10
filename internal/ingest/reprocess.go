package ingest

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/christianparpart/agentic-stats/internal/derive"
	"github.com/christianparpart/agentic-stats/internal/store"
)

// ExtractionVersion is what the current build extracts from a sealed body.
//
// Bump it whenever Identify learns to read a new field, and every archive
// re-walks itself on the next start. That is the whole upgrade procedure for a
// new extracted column: one constant, no migration of data, no second pass to
// write.
//
//	1 -- the working directory and the git branch.
const ExtractionVersion = 1

// DefaultReprocessBatch is how many records one transaction re-extracts.
//
// Small on purpose. `records` is WITHOUT ROWID with the sealed body stored
// inline, so filling in a column that was NULL changes the row's length and
// SQLite cannot take its same-size overwrite path: it deletes and reinserts the
// cell, rewriting the body and its overflow pages. The work is therefore
// proportional to the whole archive rather than to the bytes added, and all of
// it goes through the write-ahead log.
//
// Five hundred rows at a mean of a few kilobytes is a couple of megabytes per
// transaction: short enough that the store's write lock is held for tens of
// milliseconds and neither collection nor a peer exchange is starved, and small
// enough that the WAL is checkpointed as the pass proceeds instead of growing
// to the size of the archive. Matching sync's batch size is not a coincidence;
// it is the same trade.
const DefaultReprocessBatch = 500

// DefaultReprocessInterval is how often a running daemon looks for records left
// to re-extract.
//
// Long, because a complete archive costs one small grouped query to establish
// that there is nothing to do. The pass that matters is the one at startup.
const DefaultReprocessInterval = 10 * time.Minute

// Archive is the persistence a Reprocessor needs.
//
// Vector and Since are the same reads anti-entropy walks: the reprocessor is
// anti-entropy against itself, which is why it needs no query of its own.
type Archive interface {
	Vector(ctx context.Context) (store.VersionVector, error)
	Since(ctx context.Context, origin string, after int64, limit int) ([]store.Record, error)
	ExtractionMarks(ctx context.Context, version int) (map[string]int64, error)
	SaveExtracted(ctx context.Context, version int, origin string, upTo int64, rows []store.Extracted) error
}

// Opener unseals a stored body. *seal.Keys satisfies it.
type Opener interface {
	OpenPayload(sealed []byte) ([]byte, error)
}

// ReprocessConfig is everything a Reprocessor needs.
type ReprocessConfig struct {
	// Archive holds the records to walk. Required.
	Archive Archive
	// Keys unseals their bodies. Required.
	Keys Opener
	// Logger reports progress. Zero discards it.
	Logger *slog.Logger
	// Batch overrides DefaultReprocessBatch. Zero uses it.
	Batch int
	// Interval overrides DefaultReprocessInterval. Zero uses it.
	Interval time.Duration
	// Tick replaces the internal ticker, so a test can drive passes
	// deterministically instead of waiting for one.
	Tick <-chan time.Time
}

// Reprocessor re-derives the extracted columns of records already archived.
//
// It exists because the extracted columns are a cache of what is inside the
// sealed body, and a build that learns to read a new field finds an archive
// full of records written before it could. The body stays authoritative, so the
// repair is always possible -- this is the thing that makes that promise real.
//
// It also heals records received from a peer running an older build, which
// arrive with the columns the older wire format did not carry. That path has no
// other cure: a record already held is a duplicate on the next exchange, and
// duplicates are the common case in anti-entropy, so filling columns in from a
// peer's copy would turn every exchange into a write storm and would make a
// local column depend on a peer rather than on the local sealed body.
type Reprocessor struct {
	archive  Archive
	keys     Opener
	log      *slog.Logger
	batch    int
	interval time.Duration
	tick     <-chan time.Time
}

// NewReprocessor returns a Reprocessor ready to run.
func NewReprocessor(cfg ReprocessConfig) (*Reprocessor, error) {
	if cfg.Archive == nil {
		return nil, errors.New("ingest: ReprocessConfig.Archive is required")
	}
	if cfg.Keys == nil {
		return nil, errors.New("ingest: ReprocessConfig.Keys is required")
	}
	r := &Reprocessor{
		archive:  cfg.Archive,
		keys:     cfg.Keys,
		log:      cfg.Logger,
		batch:    cfg.Batch,
		interval: cfg.Interval,
		tick:     cfg.Tick,
	}
	if r.log == nil {
		r.log = slog.New(slog.DiscardHandler)
	}
	if r.batch <= 0 {
		r.batch = DefaultReprocessBatch
	}
	if r.interval <= 0 {
		r.interval = DefaultReprocessInterval
	}
	return r, nil
}

// ReprocessStats reports what one pass did.
type ReprocessStats struct {
	// Examined is how many records were read.
	Examined int
	// Updated is how many actually needed a column changed. Far smaller than
	// Examined on every pass after the first.
	Updated int
	// Unreadable is how many bodies this node could not unseal, and therefore
	// knows nothing about beyond the metadata already in their columns.
	Unreadable int
}

// Run re-extracts until ctx is done.
//
// The first pass happens immediately rather than after one interval: the work
// is already in the archive from before this build was installed, and until it
// is done the dashboard attributes that history to no project at all.
func (r *Reprocessor) Run(ctx context.Context) error {
	tick := r.tick
	if tick == nil {
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		tick = ticker.C
	}
	for {
		stats, err := r.Pass(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil
		case err != nil:
			// Re-extraction is a repair, not a duty. Losing it must not take
			// down the subsystem supervising this, and the next pass resumes
			// from the mark rather than starting over.
			r.log.Warn("could not re-extract archived records", "error", err)
		case stats.Unreadable > 0:
			// Worth a warning rather than a note: it means this node holds
			// records it cannot read, which is a key that does not match
			// somewhere in the mesh, and nothing else reports it.
			r.log.Warn("re-extracted archived records, some unreadable",
				"examined", stats.Examined, "updated", stats.Updated,
				"unreadable", stats.Unreadable)
		case stats.Updated > 0:
			r.log.Info("re-extracted archived records",
				"examined", stats.Examined, "updated", stats.Updated)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
		}
	}
}

// Pass walks every origin once, from its mark to the end of what this node
// holds.
func (r *Reprocessor) Pass(ctx context.Context) (ReprocessStats, error) {
	var stats ReprocessStats

	vector, err := r.archive.Vector(ctx)
	if err != nil {
		return stats, err
	}
	marks, err := r.archive.ExtractionMarks(ctx, ExtractionVersion)
	if err != nil {
		return stats, err
	}

	for _, origin := range sortedOrigins(vector) {
		for after := marks[origin]; after < vector[origin]; {
			// Read the whole batch before writing any of it. A cursor left open
			// over rows an UPDATE is moving within the same index is the
			// Halloween problem and its results are undefined; Since returns a
			// materialised slice, which is what makes this safe.
			batch, err := r.archive.Since(ctx, origin, after, r.batch)
			if err != nil {
				return stats, err
			}
			if len(batch) == 0 {
				break
			}
			rows, unreadable := r.reextract(batch)
			upTo := batch[len(batch)-1].Seq
			if err := r.archive.SaveExtracted(ctx, ExtractionVersion, origin, upTo, rows); err != nil {
				return stats, err
			}
			stats.Examined += len(batch)
			stats.Updated += len(rows)
			stats.Unreadable += unreadable
			after = upTo

			if err := ctx.Err(); err != nil {
				// Reported rather than swallowed: this pass did not finish, and
				// a caller that asked for one is entitled to know. It costs
				// nothing -- the mark covers every batch committed so far, so
				// the next pass resumes here instead of starting over -- and
				// Run treats a cancelled pass as the shutdown it is.
				return stats, err
			}
		}
	}
	return stats, nil
}

// reextract unseals each record and returns only those whose stored columns
// disagree with what the body says, and how many could not be read at all.
//
// Only the differences, because the write is the expensive half: rewriting a
// row that already holds the right answer costs the same page churn as one that
// does not, and every pass after the first would pay for the whole archive
// again.
//
// A body that will not unseal is skipped rather than fatal. This is not
// leniency for its own sake: a node can hold whole origins it has no key for,
// and treating one as an error stopped the pass dead -- so a single unreadable
// origin would deny every readable one its columns, forever, and which of them
// happened to be walked first decided how much of the archive worked. Measured
// on a real archive: 441,367 records extracted, then one unreadable peer origin
// aborted the run. The columns of a record we cannot read stay empty, which is
// the honest answer, and the count is reported rather than swallowed.
func (r *Reprocessor) reextract(batch []store.Record) (rows []store.Extracted, unreadable int) {
	for _, rec := range batch {
		plain, err := r.keys.OpenPayload(rec.Sealed)
		if err != nil {
			unreadable++
			continue
		}
		id := derive.Identify(plain)
		want := store.Extracted{
			OriginID:  rec.OriginID,
			Seq:       rec.Seq,
			CWD:       id.CWD,
			GitBranch: id.GitBranch,
			PRRepo:    id.PRRepo,
			PRNumber:  id.PRNumber,
		}
		if want.CWD == rec.CWD && want.GitBranch == rec.GitBranch &&
			want.PRRepo == rec.PRRepo && want.PRNumber == rec.PRNumber {
			continue
		}
		rows = append(rows, want)
	}
	return rows, unreadable
}

// sortedOrigins fixes the walk order, so two nodes reprocessing the same
// archive do the same work in the same order and a partially completed pass is
// reproducible rather than incidental.
func sortedOrigins(v store.VersionVector) []string {
	out := make([]string, 0, len(v))
	for origin := range v {
		out = append(out, origin)
	}
	slices.Sort(out)
	return out
}
