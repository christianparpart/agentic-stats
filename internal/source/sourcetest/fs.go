// Package sourcetest provides an in-memory FileSystem for testing collectors.
//
// It exists so that discovery and tailing can be exercised without touching a
// real disk (AGENT.md: testability). The source package keeps its own
// equivalent internally, because importing this one from its in-package tests
// would form an import cycle.
package sourcetest

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/christianparpart/agentic-stats/internal/source"
)

// MapFS is a FileSystem backed by a map of path to contents.
type MapFS struct {
	files map[string]*entry
	// NextSerial supplies file identities so replacement can be simulated.
	nextSerial uint64
}

type entry struct {
	data     []byte
	identity source.FileIdentity
}

// NewMapFS returns an empty in-memory filesystem.
func NewMapFS() *MapFS {
	return &MapFS{files: make(map[string]*entry)}
}

// Put writes content at an absolute path, assigning it a fresh identity.
func (m *MapFS) Put(name, content string) {
	m.nextSerial++
	m.files[filepath.Clean(name)] = &entry{
		data:     []byte(content),
		identity: source.FileIdentity{Device: 1, Serial: m.nextSerial},
	}
}

// Append adds to an existing file, as a writer would.
func (m *MapFS) Append(name, more string) {
	e, ok := m.files[filepath.Clean(name)]
	if !ok {
		m.Put(name, more)
		return
	}
	e.data = append(e.data, more...)
}

// Remove deletes a file, as the retention reaper would.
func (m *MapFS) Remove(name string) { delete(m.files, filepath.Clean(name)) }

// Open implements source.FileSystem.
func (m *MapFS) Open(name string) (io.ReadSeekCloser, error) {
	e, ok := m.files[filepath.Clean(name)]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return nopCloser{bytes.NewReader(e.data)}, nil
}

// Stat implements source.FileSystem.
func (m *MapFS) Stat(name string) (source.FileInfo, error) {
	e, ok := m.files[filepath.Clean(name)]
	if !ok {
		return source.FileInfo{}, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return source.FileInfo{
		Size:     int64(len(e.data)),
		ModTime:  time.Unix(0, 0),
		Identity: e.identity,
	}, nil
}

// WalkDir implements source.FileSystem over the in-memory tree.
func (m *MapFS) WalkDir(root string, fn fs.WalkDirFunc) error {
	root = filepath.Clean(root)

	var matches []string
	dirs := map[string]struct{}{}
	for name := range m.files {
		if name != root && !strings.HasPrefix(name, root+string(filepath.Separator)) {
			continue
		}
		matches = append(matches, name)
		for d := filepath.Dir(name); len(d) >= len(root); d = filepath.Dir(d) {
			dirs[d] = struct{}{}
			if d == root {
				break
			}
		}
	}
	if len(matches) == 0 {
		return fn(root, nil, &fs.PathError{Op: "walk", Path: root, Err: fs.ErrNotExist})
	}

	var all []string
	for d := range dirs {
		all = append(all, d)
	}
	all = append(all, matches...)
	sort.Strings(all)

	for _, p := range all {
		_, isFile := m.files[p]
		if err := fn(p, dirEntry{name: filepath.Base(p), dir: !isFile}, nil); err != nil {
			if errors.Is(err, fs.SkipDir) {
				continue
			}
			return err
		}
	}
	return nil
}

type dirEntry struct {
	name string
	dir  bool
}

func (d dirEntry) Name() string      { return d.name }
func (d dirEntry) IsDir() bool       { return d.dir }
func (d dirEntry) Type() fs.FileMode { return 0 }
func (d dirEntry) Info() (fs.FileInfo, error) {
	return nil, &fs.PathError{Op: "info", Path: d.name, Err: fs.ErrInvalid}
}

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }
