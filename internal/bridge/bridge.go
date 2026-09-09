// Package bridge carries records between machines that never share a network,
// through a storage backend both can reach.
//
// It is the same sync, with a dumb blob store standing in for a socket: the
// same version vectors decide what to publish, and the same append path
// applies what arrives. A node publishes its own records as bundles and reads
// everyone else's.
package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"

	"github.com/christianparpart/agentic-stats/internal/seal"
	"github.com/christianparpart/agentic-stats/internal/store"
)

// bundleRecords is how many records travel in one object.
//
// Large enough that a year of history is tens of objects rather than
// thousands, small enough that a partial transfer wastes little.
const bundleRecords = 5000

// maxBundleBytes bounds a bundle read from the backend.
const maxBundleBytes = 256 << 20

// ObjectInfo describes one stored object.
type ObjectInfo struct {
	Key  string
	Size int64
}

// Backend is a blob store. It needs no conditional writes, no ETags and no
// mutation, because bundles are immutable and named for what they contain.
//
// Implementations are the extension point: a filesystem path, an S3 bucket, a
// Drive folder. Adding one is implementing four methods.
type Backend interface {
	// Name identifies the backend in logs.
	Name() string
	// Put stores an object, replacing any object with the same key.
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	// Get opens an object for reading.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// List returns the objects under a prefix.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
	// Delete removes an object.
	Delete(ctx context.Context, key string) error
}

// bundle is the decoded contents of one object.
type bundle struct {
	OriginID string         `json:"origin_id"`
	First    int64          `json:"first"`
	Last     int64          `json:"last"`
	Records  []store.Record `json:"records"`
}

// Bridge publishes to and fetches from a backend.
type Bridge struct {
	backend Backend
	db      *store.DB
	keys    *seal.Keys
	log     *slog.Logger
}

// Config is everything the bridge needs.
type Config struct {
	Backend Backend
	Store   *store.DB
	Keys    *seal.Keys
	Logger  *slog.Logger
}

// New returns a Bridge.
func New(cfg Config) (*Bridge, error) {
	if cfg.Backend == nil {
		return nil, errors.New("bridge: a backend is required")
	}
	if cfg.Store == nil {
		return nil, errors.New("bridge: a store is required")
	}
	if cfg.Keys == nil {
		return nil, errors.New("bridge: keys are required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Bridge{backend: cfg.Backend, db: cfg.Store, keys: cfg.Keys, log: log}, nil
}

// Stats summarizes one bridge run.
type Stats struct {
	Published int
	Fetched   int
	Stored    int
	Forked    int
}

// Sync publishes this node's records and applies everyone else's.
func (b *Bridge) Sync(ctx context.Context) (Stats, error) {
	var stats Stats
	published, err := b.Publish(ctx)
	if err != nil {
		return stats, err
	}
	stats.Published = published

	fetched, stored, forked, err := b.Fetch(ctx)
	stats.Fetched, stats.Stored, stats.Forked = fetched, stored, forked
	return stats, err
}

// Publish uploads any of this node's records the store does not yet hold.
//
// Bundles are named for the sequence range they contain, so deciding what is
// missing needs only a listing -- no state is kept about what was uploaded,
// and a bundle interrupted half-way is simply overwritten next time.
func (b *Bridge) Publish(ctx context.Context) (int, error) {
	origin := b.db.OriginID()
	vec, err := b.db.Vector(ctx)
	if err != nil {
		return 0, err
	}
	have := vec[origin]
	if have == 0 {
		return 0, nil
	}

	existing, err := b.listRanges(ctx, origin)
	if err != nil {
		return 0, err
	}

	published := 0
	for first := int64(1); first <= have; first += bundleRecords {
		last := min(first+bundleRecords-1, have)
		// Only re-publish a range that is absent or was cut short.
		if end, ok := existing[first]; ok && end >= last {
			continue
		}
		recs, err := b.db.Since(ctx, origin, first-1, bundleRecords)
		if err != nil {
			return published, err
		}
		if len(recs) == 0 {
			break
		}
		if err := b.putBundle(ctx, origin, first, recs[len(recs)-1].Seq, recs); err != nil {
			return published, err
		}
		published += len(recs)
	}
	return published, nil
}

// Fetch downloads and applies every bundle this node is missing.
func (b *Bridge) Fetch(ctx context.Context) (fetched, stored, forked int, err error) {
	objects, err := b.backend.List(ctx, "")
	if err != nil {
		return 0, 0, 0, fmt.Errorf("bridge: list: %w", err)
	}
	vec, err := b.db.Vector(ctx)
	if err != nil {
		return 0, 0, 0, err
	}

	for _, obj := range objects {
		origin, first, last, ok := parseKey(obj.Key)
		if !ok || origin == b.db.OriginID() {
			continue
		}
		// Skip a bundle entirely below our watermark; a partially-held one is
		// still fetched, and the append path deduplicates the overlap.
		if vec[origin] >= last {
			continue
		}
		recs, err := b.getBundle(ctx, obj.Key)
		if err != nil {
			b.log.Warn("skip unreadable bundle", "key", obj.Key, "error", err)
			continue
		}
		res, err := b.db.AppendRemote(ctx, recs)
		if err != nil {
			return fetched, stored, forked, err
		}
		fetched += len(recs)
		stored += res.Stored
		forked += res.Forked
		if res.Forked > 0 {
			b.log.Error("origin fork detected in a bundle; records quarantined",
				"key", obj.Key, "count", res.Forked)
		}
		_ = first
	}
	return fetched, stored, forked, nil
}

// putBundle seals and stores one range.
//
// The whole bundle is encrypted, not merely the record bodies inside it. The
// bodies are already sealed, but their metadata -- file paths, model names,
// timestamps -- is not, and a third-party store must not see that either.
func (b *Bridge) putBundle(ctx context.Context, origin string, first, last int64, recs []store.Record) error {
	body, err := json.Marshal(bundle{OriginID: origin, First: first, Last: last, Records: recs})
	if err != nil {
		return fmt.Errorf("bridge: encode bundle: %w", err)
	}
	sealed, err := b.keys.SealPayload(body)
	if err != nil {
		return fmt.Errorf("bridge: seal bundle: %w", err)
	}
	key := formatKey(origin, first, last)
	if err := b.backend.Put(ctx, key, bytes.NewReader(sealed), int64(len(sealed))); err != nil {
		return fmt.Errorf("bridge: put %s: %w", key, err)
	}
	return nil
}

// getBundle reads and opens one bundle.
func (b *Bridge) getBundle(ctx context.Context, key string) ([]store.Record, error) {
	rc, err := b.backend.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("bridge: get %s: %w", key, err)
	}
	defer func() { _ = rc.Close() }() // fully read below

	sealed, err := io.ReadAll(io.LimitReader(rc, maxBundleBytes))
	if err != nil {
		return nil, fmt.Errorf("bridge: read %s: %w", key, err)
	}
	body, err := b.keys.OpenPayload(sealed)
	if err != nil {
		// Another mesh's bundle in a shared folder, or a corrupted object.
		return nil, fmt.Errorf("bridge: open %s: %w", key, err)
	}
	var bun bundle
	if err := json.Unmarshal(body, &bun); err != nil {
		return nil, fmt.Errorf("bridge: decode %s: %w", key, err)
	}
	return bun.Records, nil
}

// listRanges reports the sequence ranges already published for an origin.
func (b *Bridge) listRanges(ctx context.Context, origin string) (map[int64]int64, error) {
	objects, err := b.backend.List(ctx, origin+"/")
	if err != nil {
		return nil, fmt.Errorf("bridge: list %s: %w", origin, err)
	}
	out := make(map[int64]int64, len(objects))
	for _, obj := range objects {
		if o, first, last, ok := parseKey(obj.Key); ok && o == origin {
			if existing, seen := out[first]; !seen || last > existing {
				out[first] = last
			}
		}
	}
	return out, nil
}

// formatKey names a bundle for exactly what it contains, which is what lets
// publishing be stateless.
func formatKey(origin string, first, last int64) string {
	return fmt.Sprintf("%s/%012d-%012d.bundle", origin, first, last)
}

// parseKey reverses formatKey.
func parseKey(key string) (origin string, first, last int64, ok bool) {
	slash := strings.LastIndexByte(key, '/')
	if slash < 0 || !strings.HasSuffix(key, ".bundle") {
		return "", 0, 0, false
	}
	origin = key[:slash]
	span := strings.TrimSuffix(key[slash+1:], ".bundle")
	dash := strings.IndexByte(span, '-')
	if dash < 0 {
		return "", 0, 0, false
	}
	first, err := strconv.ParseInt(span[:dash], 10, 64)
	if err != nil {
		return "", 0, 0, false
	}
	last, err = strconv.ParseInt(span[dash+1:], 10, 64)
	if err != nil {
		return "", 0, 0, false
	}
	return origin, first, last, true
}
