// Package supervise keeps a long-running daemon running.
//
// The archive exists because transcripts are deleted on a rolling window. A
// daemon that exits on a transient fault stops collecting, and the data it
// would have collected is gone for good -- so the default answer to a failing
// subsystem is to restart it, not to take the process down. The exception is
// operator error, which restarting cannot fix and which must stay loud.
package supervise

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"time"
)

// MinBackoff is the first wait after a subsystem fails.
const MinBackoff = time.Second

// MaxBackoff caps the wait between restarts.
const MaxBackoff = time.Minute

// permanent marks a failure that restarting cannot fix.
type permanent struct{ err error }

func (p permanent) Error() string { return p.err.Error() }
func (p permanent) Unwrap() error { return p.err }

// Permanent marks err as unfixable by restarting, so the daemon stops instead.
//
// Reserve it for operator error -- an address that cannot be parsed, a
// configuration that cannot be satisfied. A restart loop around a typo is worse
// than an exit, because it buries the one message that would explain it.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanent{err: err}
}

// IsPermanent reports whether err was marked by Permanent.
func IsPermanent(err error) bool {
	var p permanent
	return errors.As(err, &p)
}

// Recover turns a panic into a log line and lets the goroutine end quietly.
//
// This is a deliberate departure from the project's rule that a panic is a
// programmer error and should crash. In a daemon whose whole purpose is getting
// bytes to safety before something else deletes them, a parsing bug provoked by
// one peer's malformed frame must not stop collection on this machine: the bug
// costs one exchange, while the crash costs every transcript that expires
// before someone notices the process is gone.
//
// The bug is not hidden. It is logged at error level with its stack, which is
// the same information a crash would have produced, minus the outage.
//
// Use it only at a goroutine boundary that owns one unit of work -- one peer
// exchange, one collection pass. Never around a store transaction, where
// inTx's own rollback-then-repanic must keep running.
func Recover(log *slog.Logger, what string) {
	p := recover()
	if p == nil {
		return
	}
	log.Error("recovered from a panic; this is a bug",
		"in", what, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
}

// Config describes one supervised subsystem.
type Config struct {
	// Name identifies the subsystem in logs. Required.
	Name string
	// Run does the work and returns when it is finished or has failed.
	// Required.
	Run func(context.Context) error
	// Logger receives restart reporting. Required.
	Logger *slog.Logger
	// Clock supplies time. Zero uses the system clock.
	Clock func() time.Time
	// After waits out a backoff. Zero uses time.After.
	//
	// Injected so a test can assert the schedule instead of serving it: a
	// restart policy whose correctness is checked by sleeping through it is
	// both slow and only checked at one point.
	After func(time.Duration) <-chan time.Time
}

// Run supervises one subsystem until ctx is done or it fails permanently.
//
// A subsystem that returns nil has finished its work and is not restarted. One
// that returns an error is restarted after a backoff that doubles to MaxBackoff
// and resets once it has stayed up. A panic inside it is contained the same way
// an error is, since from out here the distinction is academic.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Name == "" || cfg.Run == nil || cfg.Logger == nil {
		return errors.New("supervise: Name, Run and Logger are required")
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	after := cfg.After
	if after == nil {
		after = time.After
	}

	wait := MinBackoff
	for {
		started := now()
		err := attempt(ctx, cfg)

		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err == nil:
			cfg.Logger.Info("subsystem finished", "subsystem", cfg.Name)
			return nil
		case IsPermanent(err):
			// Operator error: exiting is the only way this gets noticed.
			return fmt.Errorf("%s: %w", cfg.Name, err)
		}

		// A subsystem that ran for a good while before failing is not in a
		// restart loop, so it should not inherit one's backoff.
		if now().Sub(started) > MaxBackoff {
			wait = MinBackoff
		}
		// Jitter so a fleet that loses the same network does not return to it
		// in lockstep.
		delay := wait + time.Duration(rand.Int64N(int64(wait/2)+1))
		cfg.Logger.Warn("subsystem failed; restarting",
			"subsystem", cfg.Name, "error", err, "in", delay)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-after(delay):
		}
		wait = min(wait*2, MaxBackoff)
	}
}

// attempt runs the subsystem once, converting a panic into an error.
func attempt(ctx context.Context, cfg Config) (err error) {
	defer func() {
		if p := recover(); p != nil {
			cfg.Logger.Error("subsystem panicked; this is a bug",
				"subsystem", cfg.Name, "panic", fmt.Sprint(p),
				"stack", string(debug.Stack()))
			err = fmt.Errorf("supervise: %s panicked: %v", cfg.Name, p)
		}
	}()
	return cfg.Run(ctx)
}
