// Package collector drives one pass over every discovered stream: read what is
// new, upload it, and only then advance the cursor.
//
// Advancing the cursor after acknowledgement is what makes the pipeline safe to
// interrupt. A crash or a failed upload re-sends, and the server deduplicates.
package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/christianparpart/agentic-stats/internal/cursor"
	"github.com/christianparpart/agentic-stats/internal/source"
	"github.com/christianparpart/agentic-stats/internal/wire"
)

// defaultBatchSize is how many records accumulate before an upload.
const defaultBatchSize = 2000

// Uploader delivers records to the server. It is an interface so collection is
// testable without a server (AGENT.md: dependency injection).
type Uploader interface {
	Ingest(ctx context.Context, records []wire.Record) (wire.IngestResult, error)
}

// Stats summarizes one collection pass.
type Stats struct {
	// Streams is how many streams were discovered.
	Streams int
	// Lines is how many lines were read and uploaded.
	Lines int
	// Batches is how many uploads were performed.
	Batches int
	// Stored and Duplicates are the server's accounting.
	Stored     int
	Duplicates int
	// Gone is how many streams vanished mid-pass, which is expected: the
	// assistant deletes transcripts on a retention schedule.
	Gone int
	// Restarted is how many streams were re-read from the beginning because
	// they had been truncated or replaced.
	Restarted int
}

// Config is everything a Collector needs.
type Config struct {
	// Sources are the adapters to collect from. Required.
	Sources []source.Source
	// Cursors persists read positions. Required.
	Cursors cursor.Store
	// Uploader delivers records. Required.
	Uploader Uploader
	// BatchSize bounds records per upload. Zero selects a default.
	BatchSize int
	// Logger receives progress and anomalies. Zero discards them.
	Logger *slog.Logger
}

// Collector performs collection passes.
type Collector struct {
	sources   []source.Source
	cursors   cursor.Store
	uploader  Uploader
	batchSize int
	log       *slog.Logger
}

// New returns a Collector wired to the supplied dependencies.
func New(cfg Config) (*Collector, error) {
	if len(cfg.Sources) == 0 {
		return nil, errors.New("collector: at least one Source is required")
	}
	if cfg.Cursors == nil {
		return nil, errors.New("collector: Config.Cursors is required")
	}
	if cfg.Uploader == nil {
		return nil, errors.New("collector: Config.Uploader is required")
	}
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Collector{
		sources:   cfg.Sources,
		cursors:   cfg.Cursors,
		uploader:  cfg.Uploader,
		batchSize: batchSize,
		log:       log,
	}, nil
}

// pending couples a batch of records with the cursors they justify advancing.
type pending struct {
	records []wire.Record
	cursors map[cursor.Key]source.Cursor
}

func newPending() *pending {
	return &pending{cursors: make(map[cursor.Key]source.Cursor)}
}

// CollectOnce performs a single pass over every stream in every source.
func (c *Collector) CollectOnce(ctx context.Context) (Stats, error) {
	var stats Stats
	batch := newPending()

	for _, src := range c.sources {
		streams, err := src.Discover(ctx)
		if err != nil {
			return stats, fmt.Errorf("collector: discover %s: %w", src.Name(), err)
		}
		stats.Streams += len(streams)

		for _, stream := range streams {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			if err := c.drainStream(ctx, stream, batch, &stats); err != nil {
				return stats, err
			}
		}
	}

	if err := c.flush(ctx, batch, &stats); err != nil {
		return stats, err
	}
	return stats, nil
}

// drainStream reads a stream to its current end, flushing whenever the batch fills.
func (c *Collector) drainStream(
	ctx context.Context, stream source.Stream, batch *pending, stats *Stats,
) error {
	key := cursor.KeyOf(stream.ID())
	from, err := c.cursors.Load(key)
	if err != nil {
		return err
	}

	for {
		result, err := stream.Read(ctx, from)
		if err != nil {
			if errors.Is(err, source.ErrStreamGone) {
				// Expected: transcripts are deleted on a retention schedule.
				c.log.Debug("stream vanished", "path", stream.ID().Path)
				stats.Gone++
				return nil
			}
			return err
		}

		if result.Continuity != source.ContinuityAppended {
			c.log.Warn("stream restarted",
				"path", stream.ID().Path, "reason", result.Continuity.String())
			stats.Restarted++
		}

		for _, line := range result.Lines {
			batch.records = append(batch.records,
				wire.NewRecord(line.Stream.Source, line.Stream.Path, line.Offset, line.Data))
		}
		batch.cursors[key] = result.Next
		stats.Lines += len(result.Lines)

		if len(batch.records) >= c.batchSize {
			if err := c.flush(ctx, batch, stats); err != nil {
				return err
			}
		}

		// A bounded read returns early on a large backlog; keep going until
		// the stream yields nothing further.
		if len(result.Lines) == 0 || result.Next.Offset == from.Offset {
			return nil
		}
		from = result.Next
	}
}

// flush uploads the pending records and, only on success, commits their cursors.
func (c *Collector) flush(ctx context.Context, batch *pending, stats *Stats) error {
	if len(batch.records) == 0 {
		// Cursors may still need committing when a stream yielded a restart
		// with no new lines.
		return c.commit(batch)
	}

	result, err := c.uploader.Ingest(ctx, batch.records)
	if err != nil {
		return fmt.Errorf("collector: upload %d records: %w", len(batch.records), err)
	}
	stats.Batches++
	stats.Stored += result.Stored
	stats.Duplicates += result.Duplicates

	if err := c.commit(batch); err != nil {
		return err
	}
	batch.records = batch.records[:0]
	return nil
}

// commit persists the cursors justified by an acknowledged upload.
func (c *Collector) commit(batch *pending) error {
	for key, cur := range batch.cursors {
		if err := c.cursors.Save(key, cur); err != nil {
			return err
		}
		delete(batch.cursors, key)
	}
	return nil
}
