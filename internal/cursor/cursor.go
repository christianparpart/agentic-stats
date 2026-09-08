// Package cursor persists how far the collector has read in each stream.
//
// Delivery is at-least-once: a cursor advances only after the server has
// acknowledged the lines it covers. A crash or a failed upload therefore
// re-sends, and the server deduplicates. That is deliberately simpler than a
// second write-ahead queue, and it is safe because the lines still exist on
// disk until the assistant's retention window expires.
package cursor

import (
	"errors"
	"fmt"
	"time"

	"go.etcd.io/bbolt"

	"github.com/christianparpart/agentic-stats/internal/source"
)

// Key identifies a stream's position independently of process lifetime.
type Key struct {
	// Source is the adapter name, e.g. "claudecode".
	Source string
	// Path is the absolute path of the stream.
	Path string
}

// String renders the key for use as a storage key.
func (k Key) String() string { return k.Source + "\x00" + k.Path }

// KeyOf derives the storage key for a stream.
func KeyOf(id source.StreamID) Key {
	return Key{Source: id.Source, Path: id.Path}
}

// Store persists cursors durably across restarts.
type Store interface {
	// Load returns the stored cursor, or the zero cursor if none exists.
	Load(Key) (source.Cursor, error)
	// Save records a cursor, replacing any previous value.
	Save(Key, source.Cursor) error
	// Delete removes a cursor, for a stream that no longer exists.
	Delete(Key) error
	// Close releases the underlying resources.
	Close() error
}

var cursorsBucket = []byte("cursors")

// BoltStore is a Store backed by a single-file embedded database.
type BoltStore struct {
	db *bbolt.DB
}

// BoltStoreConfig is everything a BoltStore needs.
type BoltStoreConfig struct {
	// Path is the database file location. Required.
	Path string
	// Timeout bounds how long to wait for the file lock. Zero selects a default.
	Timeout time.Duration
}

// OpenBoltStore opens or creates the cursor database.
func OpenBoltStore(cfg BoltStoreConfig) (*BoltStore, error) {
	if cfg.Path == "" {
		return nil, errors.New("cursor: BoltStoreConfig.Path is required")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	db, err := bbolt.Open(cfg.Path, 0o600, &bbolt.Options{Timeout: timeout})
	if err != nil {
		return nil, fmt.Errorf("cursor: open %s: %w", cfg.Path, err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		_, berr := tx.CreateBucketIfNotExists(cursorsBucket)
		return berr
	})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("cursor: init %s: %w", cfg.Path, err), db.Close())
	}
	return &BoltStore{db: db}, nil
}

// Load implements Store.
func (s *BoltStore) Load(k Key) (source.Cursor, error) {
	var c source.Cursor
	err := s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(cursorsBucket).Get([]byte(k.String()))
		if raw == nil {
			return nil
		}
		var derr error
		c, derr = decodeCursor(raw)
		return derr
	})
	if err != nil {
		return source.Cursor{}, fmt.Errorf("cursor: load %s: %w", k.Path, err)
	}
	return c, nil
}

// Save implements Store.
func (s *BoltStore) Save(k Key, c source.Cursor) error {
	err := s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(cursorsBucket).Put([]byte(k.String()), encodeCursor(c))
	})
	if err != nil {
		return fmt.Errorf("cursor: save %s: %w", k.Path, err)
	}
	return nil
}

// Delete implements Store.
func (s *BoltStore) Delete(k Key) error {
	err := s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(cursorsBucket).Delete([]byte(k.String()))
	})
	if err != nil {
		return fmt.Errorf("cursor: delete %s: %w", k.Path, err)
	}
	return nil
}

// Close implements Store.
func (s *BoltStore) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("cursor: close: %w", err)
	}
	return nil
}
