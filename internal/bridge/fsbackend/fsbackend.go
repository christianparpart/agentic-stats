// Package fsbackend stores bundles in a directory.
//
// It is the simplest backend and, deliberately, the first: it has no
// dependencies, is testable with t.TempDir(), and it already delivers the
// feature -- point two machines at the same Dropbox, iCloud Drive, OneDrive,
// NAS or SMB share and they bridge with no cloud account at all.
//
// It also forces the two decisions every other backend inherits: publishing
// must be atomic, and listing must be cheap.
package fsbackend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/christianparpart/agentic-stats/internal/bridge"
)

// Backend is a directory of bundles.
type Backend struct {
	root string
}

// New returns a Backend rooted at dir, creating it if needed.
func New(dir string) (*Backend, error) {
	if dir == "" {
		return nil, errors.New("fsbackend: a directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("fsbackend: create %s: %w", dir, err)
	}
	return &Backend{root: dir}, nil
}

// Name implements bridge.Backend.
func (b *Backend) Name() string { return "filesystem:" + b.root }

// path resolves a key, refusing anything that would escape the root.
func (b *Backend) path(key string) (string, error) {
	if key == "" || strings.Contains(key, "..") || filepath.IsAbs(key) {
		return "", fmt.Errorf("fsbackend: refusing key %q", key)
	}
	return filepath.Join(b.root, filepath.FromSlash(key)), nil
}

// Put writes an object atomically.
//
// The temporary file is created in the destination directory, not in the
// system temp dir, so the rename stays within one filesystem and is atomic.
// This matters more than it looks: a sync client watching the folder must
// never see a half-written bundle and upload it.
func (b *Backend) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	dst, err := b.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("fsbackend: create directory: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return fmt.Errorf("fsbackend: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := io.Copy(tmp, r); err != nil {
		cleanup()
		return errors.Join(fmt.Errorf("fsbackend: write %s: %w", key, err), tmp.Close())
	}
	// Flush before renaming: a crash between rename and flush would otherwise
	// leave a correctly-named but empty object.
	if err := tmp.Sync(); err != nil {
		cleanup()
		return errors.Join(fmt.Errorf("fsbackend: sync %s: %w", key, err), tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("fsbackend: close %s: %w", key, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		cleanup()
		return fmt.Errorf("fsbackend: publish %s: %w", key, err)
	}
	return nil
}

// Get opens an object.
func (b *Backend) Get(_ context.Context, key string) (io.ReadCloser, error) {
	path, err := b.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("fsbackend: open %s: %w", key, err)
	}
	return f, nil
}

// List returns the objects under a prefix.
func (b *Backend) List(_ context.Context, prefix string) ([]bridge.ObjectInfo, error) {
	var out []bridge.ObjectInfo
	err := filepath.WalkDir(b.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Temp files are in-flight publishes, not objects.
		if strings.HasPrefix(d.Name(), ".tmp-") {
			return nil
		}
		// WalkDir only yields paths under root, so trimming the prefix is
		// exact and needs no error path.
		key := filepath.ToSlash(strings.TrimPrefix(path, b.root+string(filepath.Separator)))
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			return nil
		}
		// Size is advisory -- the bridge selects bundles by key alone -- so an
		// entry that vanished between the walk and the stat is still listed,
		// and the caller finds out when it tries to read it.
		var size int64
		if info, ierr := d.Info(); ierr == nil {
			size = info.Size()
		}
		out = append(out, bridge.ObjectInfo{Key: key, Size: size})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("fsbackend: list: %w", err)
	}
	return out, nil
}

// Delete removes an object. A missing object is not an error, so a repeated
// delete is safe.
func (b *Backend) Delete(_ context.Context, key string) error {
	path, err := b.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("fsbackend: delete %s: %w", key, err)
	}
	return nil
}
