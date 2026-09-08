package cursor

import "github.com/christianparpart/agentic-stats/internal/source"

// MemoryStore keeps cursors in memory only.
//
// It backs tests and the agent's dry-run mode, where progress must deliberately
// not persist.
type MemoryStore struct {
	cursors map[Key]source.Cursor
}

// NewMemoryStore returns an empty in-memory cursor store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{cursors: make(map[Key]source.Cursor)}
}

// Load implements Store.
func (s *MemoryStore) Load(k Key) (source.Cursor, error) { return s.cursors[k], nil }

// Save implements Store.
func (s *MemoryStore) Save(k Key, c source.Cursor) error {
	s.cursors[k] = c
	return nil
}

// Delete implements Store.
func (s *MemoryStore) Delete(k Key) error {
	delete(s.cursors, k)
	return nil
}

// Close implements Store.
func (s *MemoryStore) Close() error { return nil }
