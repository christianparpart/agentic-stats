package source

import (
	"bytes"
	"io"
	"io/fs"
	"time"
)

// memFile is one in-memory file with an identity, so that replacement and
// truncation can be simulated exactly.
type memFile struct {
	data     []byte
	identity FileIdentity
	modTime  time.Time
}

// memFS is an in-memory FileSystem for tests. It exists so tailing semantics —
// truncation, replacement, mid-write reads — are exercised deterministically
// rather than by racing a real disk.
type memFS struct {
	files map[string]*memFile
}

func newMemFS() *memFS { return &memFS{files: make(map[string]*memFile)} }

func (m *memFS) put(name, content string, id FileIdentity) {
	m.files[name] = &memFile{
		data:     []byte(content),
		identity: id,
		modTime:  time.Unix(0, 0),
	}
}

func (m *memFS) append(name, more string) {
	f, ok := m.files[name]
	if !ok {
		panic("append to missing file: " + name)
	}
	f.data = append(f.data, more...)
}

func (m *memFS) remove(name string) { delete(m.files, name) }

func (m *memFS) Open(name string) (io.ReadSeekCloser, error) {
	f, ok := m.files[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return nopCloser{bytes.NewReader(f.data)}, nil
}

func (m *memFS) Stat(name string) (FileInfo, error) {
	f, ok := m.files[name]
	if !ok {
		return FileInfo{}, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return FileInfo{
		Size:     int64(len(f.data)),
		ModTime:  f.modTime,
		Identity: f.identity,
	}, nil
}

func (m *memFS) WalkDir(string, fs.WalkDirFunc) error { return nil }

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }
