package source

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// OSFileSystem is the real filesystem.
type OSFileSystem struct{}

// NewOSFileSystem returns a FileSystem backed by the operating system.
func NewOSFileSystem() OSFileSystem { return OSFileSystem{} }

// Open implements FileSystem.
func (OSFileSystem) Open(name string) (io.ReadSeekCloser, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Stat implements FileSystem.
func (OSFileSystem) Stat(name string) (FileInfo, error) {
	fi, err := os.Stat(name)
	if err != nil {
		return FileInfo{}, err
	}
	return FileInfo{
		Size:     fi.Size(),
		ModTime:  fi.ModTime(),
		Identity: identityOf(fi),
	}, nil
}

// WalkDir implements FileSystem.
func (OSFileSystem) WalkDir(root string, fn fs.WalkDirFunc) error {
	return filepath.WalkDir(root, fn)
}

// SystemClock reports the real wall-clock time.
type SystemClock struct{}

// NewSystemClock returns a Clock backed by the system clock.
func NewSystemClock() SystemClock { return SystemClock{} }

// Now implements Clock.
func (SystemClock) Now() time.Time { return time.Now().UTC() }
