// Package resume notices that this machine stopped running for a while.
//
// A laptop that sleeps and a VM that is suspended both freeze the process
// without telling it. On waking, everything the daemon believed about the
// network is stale: connections are half-open, the addresses it backed off from
// may be reachable again, and it may not even be on the same network. Waiting
// out the ordinary timers means minutes of not converging, on exactly the
// machines that were away longest and have the most to catch up on.
package resume

import (
	"context"
	"log/slog"
	"time"
)

// DefaultInterval is how often the detector checks.
//
// Short enough that a resume is noticed promptly, long enough to be free.
const DefaultInterval = 10 * time.Second

// DefaultThreshold is the unaccounted-for gap that counts as a suspend.
//
// It must clear ordinary scheduling noise -- a loaded machine can miss a tick
// by a wide margin without having been suspended -- while staying well under
// the dial sweep it is there to pre-empt.
const DefaultThreshold = 45 * time.Second

// Config is everything the detector needs.
type Config struct {
	// Interval is how often to check. Zero uses DefaultInterval.
	Interval time.Duration
	// Threshold is the gap that counts as a suspend. Zero uses
	// DefaultThreshold.
	Threshold time.Duration
	// Clock supplies wall-clock time. Zero uses time.Now.
	//
	// Wall clock, deliberately: see Watch.
	Clock func() time.Time
	// Logger reports detections. Zero discards them.
	Logger *slog.Logger
	// Tick replaces the internal ticker when set.
	//
	// Injected so a test can drive the detector deterministically. Mixing a
	// real ticker with a fake clock races the detector's own first reading,
	// which is a property of the test rather than of the code under it.
	Tick <-chan time.Time
}

// Watch calls woke whenever the machine appears to have been suspended.
//
// Detection compares elapsed wall-clock time against the interval that was
// actually waited. While a process is frozen the wall clock keeps advancing and
// the process does not, so a tick that arrives far later than it was scheduled
// is the signal.
//
// It has to be the wall clock. The monotonic clock is the obvious choice and
// the wrong one here, because whether it advances across a suspend is exactly
// what the three target platforms disagree about: CLOCK_MONOTONIC excludes
// suspended time on Linux, mach_absolute_time excludes it on macOS, and Windows
// differs again between S3 sleep and modern standby. A wall clock that jumps is
// the one observation available everywhere.
//
// The cost of that choice is that a clock correction -- NTP stepping the clock,
// or a VM resynchronising after a snapshot restore -- also fires woke. That is
// acceptable and arguably right: a machine whose clock just jumped is very
// likely one that was suspended, and the response is only to retry peers early,
// which is harmless when it turns out to be unnecessary.
//
// Watch blocks until ctx is done.
func Watch(ctx context.Context, cfg Config, woke func()) {
	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	threshold := cfg.Threshold
	if threshold <= 0 {
		threshold = DefaultThreshold
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	tick := cfg.Tick
	if tick == nil {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		tick = ticker.C
	}

	// Round(0) strips the monotonic reading, so Sub compares wall clocks and
	// the jump is visible rather than corrected away.
	last := now().Round(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}

		current := now().Round(0)
		gap := current.Sub(last)
		last = current

		if gap < interval+threshold {
			continue
		}
		log.Info("resumed after an interruption; retrying peers now",
			"away", gap.Round(time.Second))
		woke()
	}
}
