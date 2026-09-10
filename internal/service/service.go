// Package service installs the daemon so it starts when the user logs in.
//
// Every platform gets a *user* service rather than a system one: the daemon
// reads one person's transcripts from their home directory, so it has no
// business running as root, and a user service needs no administrator rights
// to install.
package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Label identifies the service to the operating system. Reverse-DNS because
// launchd expects it and the others tolerate it.
const Label = "io.github.christianparpart.agentic-stats"

// Name is the short service name used where a label would be unidiomatic.
const Name = "agentic-stats"

// ErrNotInstalled reports that no service definition is present.
var ErrNotInstalled = errors.New("service: not installed")

// Config describes the service to install.
type Config struct {
	// Executable is the binary to run. Empty resolves the running binary.
	Executable string
	// ConfigPath is passed to the daemon, so a service keeps working when the
	// default location changes.
	ConfigPath string
	// StatePath is the archive location. Empty lets the daemon decide.
	StatePath string
	// LogDir receives stdout and stderr. Empty selects a platform default.
	LogDir string
	// Notify receives progress a person should see before it happens, such as
	// a warning that an elevation prompt is about to appear. Zero discards it.
	//
	// A callback rather than printing from here, because a package that
	// installs services has no business deciding where output goes -- and the
	// one message that matters must arrive *before* the prompt does, so it
	// cannot be part of the returned error.
	Notify func(string)
}

// notify reports progress, if anyone is listening.
//
// A plain string rather than a format: every message here is assembled from
// paths and usernames, so a format-style signature would only invite the
// non-constant-format-string mistake without ever saving a Sprintf.
func (c Config) notify(msg string) {
	if c.Notify != nil {
		c.Notify(msg)
	}
}

// Status is what the platform reports about the service.
type Status struct {
	// Installed is whether a definition exists on disk.
	Installed bool
	// Running is whether the platform believes it is currently running.
	// Some platforms cannot answer cheaply, in which case it is false and
	// Detail says why.
	Running bool
	// DefinitionPath is where the unit, plist or task lives.
	DefinitionPath string
	// Detail is a short human-readable note.
	Detail string
}

// Manager installs and removes the service for one platform.
type Manager interface {
	// Install writes the definition and starts the service.
	Install(Config) (Status, error)
	// Uninstall stops the service and removes the definition. Removing
	// something already absent is not an error.
	Uninstall() error
	// Status reports what the platform knows.
	Status() (Status, error)

	// Start runs an installed service. Starting one already running is not an
	// error: the caller asked for it to be running, and it is.
	Start() error
	// Stop halts a running service, leaving it installed. Stopping one that is
	// not running is likewise not an error.
	Stop() error
}

// Restart stops a service and starts it again.
//
// A helper rather than a method, because every platform implements it as its
// two halves and a third entry point per platform would be three more chances
// for them to disagree.
func Restart(m Manager) error {
	if err := m.Stop(); err != nil {
		return err
	}
	return m.Start()
}

// ErrNotRunning reports that the service is installed but stopped.
var ErrNotRunning = errors.New("service: not running")

// New returns the Manager for this platform.
func New() (Manager, error) { return newManager() }

// resolve fills in the defaults a Config leaves empty.
func (c Config) resolve() (Config, error) {
	if c.Executable == "" {
		exe, err := os.Executable()
		if err != nil {
			return Config{}, fmt.Errorf("service: locate the running binary: %w", err)
		}
		// Resolve symlinks so the service survives the link being repointed,
		// which is exactly what a package upgrade does.
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		c.Executable = exe
	}
	if !filepath.IsAbs(c.Executable) {
		abs, err := filepath.Abs(c.Executable)
		if err != nil {
			return Config{}, fmt.Errorf("service: resolve %s: %w", c.Executable, err)
		}
		c.Executable = abs
	}
	if _, err := os.Stat(c.Executable); err != nil {
		return Config{}, fmt.Errorf("service: %s is not runnable: %w", c.Executable, err)
	}
	c.Executable = serviceBinary(c.Executable, c.notify)
	return c, nil
}

// args builds the daemon's command line.
func (c Config) args() []string {
	args := []string{"run"}
	if c.ConfigPath != "" {
		args = append(args, "--config", c.ConfigPath)
	}
	if c.StatePath != "" {
		args = append(args, "--state", c.StatePath)
	}
	return args
}
