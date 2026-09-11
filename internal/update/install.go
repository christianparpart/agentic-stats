package update

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/christianparpart/agentic-stats/internal/service"
)

// executableMode is what a replaced binary is left as.
//
// Set explicitly rather than copied from the old file: a staged download
// arrives with whatever the temporary directory's umask gave it, and a binary
// that is not executable leaves the node unable to start at all.
const executableMode = 0o755

// BinaryInstaller replaces this node's executables with verified artifacts.
//
// The paths come from the running process rather than from configuration,
// because the only binary a node has any business replacing is the one it is
// running. On Windows that is two files -- the service runs the windowless
// twin -- and replacing one without the other would leave the command line and
// the service on different builds.
type BinaryInstaller struct {
	console    string
	windowless string
	goos       string
	goarch     string
	log        *slog.Logger
}

// InstallConfig is what a BinaryInstaller needs.
type InstallConfig struct {
	// Executable is the running binary's path. Empty asks the operating
	// system, which is what production wants; a test names a stub instead.
	Executable string
	// GOOS and GOARCH name the platform whose artifacts will arrive. Empty
	// uses this machine's.
	GOOS   string
	GOARCH string
	// Logger receives progress. Nil discards it.
	Logger *slog.Logger
}

// NewBinaryInstaller returns an installer for this node's own binaries.
func NewBinaryInstaller(cfg InstallConfig) (*BinaryInstaller, error) {
	exe := cfg.Executable
	if exe == "" {
		found, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("update: locate this executable: %w", err)
		}
		// Resolved for the same reason the service resolves it: a symlink is
		// a pointer to the build, and replacing the pointer is not replacing
		// the build.
		if resolved, rerr := filepath.EvalSymlinks(found); rerr == nil {
			found = resolved
		}
		exe = found
	}
	if !filepath.IsAbs(exe) {
		abs, err := filepath.Abs(exe)
		if err != nil {
			return nil, fmt.Errorf("update: resolve %s: %w", exe, err)
		}
		exe = abs
	}

	i := &BinaryInstaller{
		console:    service.ConsolePath(exe),
		windowless: service.WindowlessPath(exe),
		goos:       cfg.GOOS,
		goarch:     cfg.GOARCH,
		log:        cfg.Logger,
	}
	if i.goos == "" {
		i.goos = runtime.GOOS
	}
	if i.goarch == "" {
		i.goarch = runtime.GOARCH
	}
	if i.log == nil {
		i.log = slog.New(slog.DiscardHandler)
	}
	return i, nil
}

// Install puts every staged artifact in place.
//
// Each destination is checked writable before anything is moved, so the usual
// failure -- a binary somewhere this process may not write, which is what
// systemd's ProtectSystem=strict produces for anything outside the home
// directory -- stops the whole install rather than replacing one of two
// binaries and leaving the node mismatched.
func (i *BinaryInstaller) Install(_ context.Context, staged map[string]string) error {
	want := i.destinations()
	for name, dst := range want {
		if _, ok := staged[name]; !ok {
			return fmt.Errorf("update: the release did not provide %s", name)
		}
		if err := writable(dst); err != nil {
			return err
		}
	}

	for name, dst := range want {
		if err := replace(dst, staged[name]); err != nil {
			return fmt.Errorf("update: replace %s: %w", dst, err)
		}
		i.log.Info("replaced binary", "path", dst, "from", name)
	}
	return nil
}

// destinations maps a release artifact name to where it belongs on this node.
func (i *BinaryInstaller) destinations() map[string]string {
	names := artifactNames(i.goos, i.goarch)
	if len(names) == 1 {
		return map[string]string{names[0]: i.console}
	}
	// Windows: the console build and the windowless twin the service runs.
	return map[string]string{
		names[0]: i.console,
		names[1]: i.windowless,
	}
}

// writable reports whether this process could replace the file at dst.
//
// Checked by creating a file in the destination's directory rather than by
// reading permissions: the answer depends on the sandbox the daemon is running
// under as much as on the mode bits, and systemd's ProtectSystem makes a
// directory unwritable while leaving its permissions untouched.
func writable(dst string) error {
	dir := filepath.Dir(dst)
	f, err := os.CreateTemp(dir, ".update-probe-*")
	if err != nil {
		return fmt.Errorf("update: cannot write to %s: %w", dir, err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

// stageBeside copies src to a temporary file in dst's directory.
//
// In that directory rather than the system temporary one so the rename that
// follows stays on a single filesystem and is therefore atomic -- the same
// reason the bridge's object store does it.
func stageBeside(dst, src string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("open staged file: %w", err)
	}
	defer func() { _ = in.Close() }() // read-only

	out, err := os.CreateTemp(filepath.Dir(dst), ".update-*")
	if err != nil {
		return "", fmt.Errorf("create alongside %s: %w", dst, err)
	}
	tmp := out.Name()

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("copy into place: %w", err)
	}
	// Flushed before the rename, or a crash between the two leaves a file
	// that exists under the right name with nothing in it.
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("flush: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("close: %w", err)
	}
	if err := os.Chmod(tmp, executableMode); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("make executable: %w", err)
	}
	return tmp, nil
}

// SweepStale removes images an earlier update could not delete while they were
// still mapped. A no-op where replacement leaves nothing behind.
//
// Called at start-up, which is the first moment the previous build is no longer
// running and its file can finally go.
func (i *BinaryInstaller) SweepStale() {
	for _, dst := range i.destinations() {
		stale := stalePath(dst)
		if stale == "" {
			continue
		}
		if err := os.Remove(stale); err == nil {
			i.log.Info("removed the image left by an earlier update", "path", stale)
		}
	}
}
