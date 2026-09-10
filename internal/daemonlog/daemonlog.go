// Package daemonlog builds the logger a background service should use.
//
// A daemon started at login has nowhere to write: there is no terminal
// attached, and on Windows there is not even a console. Its log has to go
// somewhere durable and somewhere an operator will actually look, and those are
// usually not the same place -- an operator opens Event Viewer, while anyone
// debugging wants a file they can grep.
package daemonlog

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// MaxFileBytes is the size at which the log file is rotated.
const MaxFileBytes = 8 << 20

// KeptFiles is how many rotated files are retained, newest first.
//
// Small on purpose. This is a breadcrumb trail for diagnosing a daemon, not an
// audit log, and the archive it protects is already the thing worth keeping.
const KeptFiles = 3

// Config is everything the logger needs.
type Config struct {
	// Dir receives the log file. Required.
	Dir string
	// Level is the minimum level written. Zero is slog.LevelInfo.
	Level slog.Level
	// Console, when set, also receives records -- used when the daemon is run
	// in a terminal rather than as a service.
	Console io.Writer
	// EventLog, when set, receives operator-facing records. Nil skips it,
	// which is correct everywhere except a Windows service.
	EventLog EventSink
	// EventLevel is the minimum level reaching EventLog. Nil uses
	// DefaultEventLevel.
	//
	// Separate from Level, and higher, because the two logs answer different
	// questions. The file is for working out what happened; the Application log
	// is where someone looks to find out whether anything is wrong. Sending
	// routine progress to both fills Event Viewer with a line every poll
	// interval and buries the entries that matter.
	//
	// A Leveler rather than a Level, because slog.LevelInfo is zero: with a
	// plain Level there is no way to ask for Info without it being read as
	// "unset", which is the one value someone is most likely to want here.
	EventLevel slog.Leveler
}

// DefaultEventLevel is the minimum severity worth putting in front of an
// operator: something that needs attention, not something that went fine.
const DefaultEventLevel = slog.LevelWarn

// EventSink is the operating system's own log, where one exists.
//
// An interface declared here rather than a concrete type, because only Windows
// has one and the rest of the package must not know that.
type EventSink interface {
	Info(msg string) error
	Warning(msg string) error
	Error(msg string) error
	Close() error
}

// Logger is a daemon's logger and the resources behind it.
type Logger struct {
	*slog.Logger
	closes []func() error
}

// Close releases the file and event-log handles.
func (l *Logger) Close() error {
	var err error
	for _, c := range l.closes {
		if cerr := c(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

// New builds a logger writing to a rotating file, and to the event log and
// console where those are configured.
func New(cfg Config) (*Logger, error) {
	if cfg.Dir == "" {
		return nil, fmt.Errorf("daemonlog: Config.Dir is required")
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("daemonlog: create %s: %w", cfg.Dir, err)
	}
	file, err := newRotatingFile(filepath.Join(cfg.Dir, "agentic-stats.log"))
	if err != nil {
		return nil, err
	}

	out := []io.Writer{file}
	if cfg.Console != nil {
		out = append(out, cfg.Console)
	}
	handler := slog.Handler(slog.NewTextHandler(io.MultiWriter(out...),
		&slog.HandlerOptions{Level: cfg.Level}))

	l := &Logger{closes: []func() error{file.Close}}
	if cfg.EventLog != nil {
		// The event log gets the same records, rendered as prose. It is read by
		// a person looking for "is this thing working", so the full structured
		// detail belongs in the file rather than in a dialog box.
		eventLevel := slog.Leveler(DefaultEventLevel)
		if cfg.EventLevel != nil {
			eventLevel = cfg.EventLevel
		}
		handler = teeHandler{primary: handler, events: cfg.EventLog, min: eventLevel.Level()}
		l.closes = append(l.closes, cfg.EventLog.Close)
	}
	l.Logger = slog.New(handler)
	return l, nil
}

// teeHandler writes each record to a handler and to the operating system log.
type teeHandler struct {
	primary slog.Handler
	events  EventSink
	// min is the lowest level forwarded to events; everything reaches primary.
	min slog.Level
}

func (t teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return t.primary.Enabled(ctx, l)
}

func (t teeHandler) Handle(ctx context.Context, r slog.Record) error {
	// The file is the record of truth, so its error is the one returned; a
	// failing event log must not cost us the log line that explains why.
	err := t.primary.Handle(ctx, r)
	if r.Level < t.min {
		return err
	}

	msg := r.Message
	r.Attrs(func(a slog.Attr) bool {
		msg += " " + a.Key + "=" + a.Value.String()
		return true
	})
	switch {
	case r.Level >= slog.LevelError:
		_ = t.events.Error(msg)
	case r.Level >= slog.LevelWarn:
		_ = t.events.Warning(msg)
	default:
		_ = t.events.Info(msg)
	}
	return err
}

func (t teeHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return teeHandler{primary: t.primary.WithAttrs(as), events: t.events, min: t.min}
}

func (t teeHandler) WithGroup(name string) slog.Handler {
	return teeHandler{primary: t.primary.WithGroup(name), events: t.events, min: t.min}
}

// rotatingFile is an append-only log file that rolls over at MaxFileBytes.
//
// Hand-rolled rather than pulled in: rotation by size with a fixed number of
// kept files is thirty lines, and a dependency the collector has to carry onto
// every machine needs a better reason than that (AGENT.md).
type rotatingFile struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func newRotatingFile(path string) (*rotatingFile, error) {
	r := &rotatingFile{path: path}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("daemonlog: open %s: %w", r.path, err)
	}
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("daemonlog: stat %s: %w", r.path, err)
	}
	r.f, r.size = f, info.Size()
	return nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.size+int64(len(p)) > MaxFileBytes {
		if err := r.rotate(); err != nil {
			// Keep writing to the current file rather than losing the line:
			// an oversized log is a smaller problem than a missing one.
			_, _ = fmt.Fprintf(r.f, "log rotation failed: %v\n", err)
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate renames the current file out of the way and starts a new one.
func (r *rotatingFile) rotate() error {
	if err := r.f.Close(); err != nil {
		return err
	}
	// Shift the numbered files up, dropping the oldest.
	_ = os.Remove(r.numbered(KeptFiles))
	for i := KeptFiles - 1; i >= 1; i-- {
		_ = os.Rename(r.numbered(i), r.numbered(i+1))
	}
	if err := os.Rename(r.path, r.numbered(1)); err != nil && !os.IsNotExist(err) {
		// Reopen regardless, or every subsequent write fails.
		_ = r.open()
		return err
	}
	return r.open()
}

// numbered renders the path of the nth rotated file.
func (r *rotatingFile) numbered(n int) string {
	return fmt.Sprintf("%s.%d", r.path, n)
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}
