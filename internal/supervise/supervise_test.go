package supervise_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/christianparpart/agentic-stats/internal/supervise"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// instant serves every backoff immediately and records what was asked for, so
// the restart schedule can be asserted rather than waited out.
func instant(seen *[]time.Duration) func(time.Duration) <-chan time.Time {
	return func(d time.Duration) <-chan time.Time {
		*seen = append(*seen, d)
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch
	}
}

// A subsystem that fails transiently must come back, because the alternative is
// a daemon that stops collecting the moment a network blips.
func TestATransientFailureIsRestarted(t *testing.T) {
	var attempts atomic.Int64
	var waits []time.Duration
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := supervise.Run(ctx, supervise.Config{
		Name:   "flaky",
		Logger: quiet(),
		After:  instant(&waits),
		Run: func(context.Context) error {
			if attempts.Add(1) < 3 {
				return errors.New("network went away")
			}
			return nil // recovered
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("ran %d times, want 3", got)
	}
	if len(waits) != 2 {
		t.Fatalf("backed off %d times, want 2", len(waits))
	}
	// Doubling, and never below the floor or above the ceiling. Jitter is
	// added on top of each, so these are ranges rather than exact values.
	if waits[0] < supervise.MinBackoff || waits[0] >= 2*supervise.MinBackoff {
		t.Errorf("first backoff %v, want it in [%v, %v)",
			waits[0], supervise.MinBackoff, 2*supervise.MinBackoff)
	}
	if waits[1] <= waits[0] {
		t.Errorf("backoff did not grow: %v then %v", waits[0], waits[1])
	}
}

// The wait must never exceed MaxBackoff, or a peer that has been off for a week
// stops being retried on any useful cadence.
func TestBackoffIsCapped(t *testing.T) {
	var attempts atomic.Int64
	var waits []time.Duration
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const tries = 20
	err := supervise.Run(ctx, supervise.Config{
		Name:   "long-gone",
		Logger: quiet(),
		After:  instant(&waits),
		Run: func(context.Context) error {
			if attempts.Add(1) < tries {
				return errors.New("still gone")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Jitter adds up to half the wait on top, so the ceiling to assert is 1.5x.
	ceiling := supervise.MaxBackoff + supervise.MaxBackoff/2
	for i, w := range waits {
		if w > ceiling {
			t.Errorf("backoff %d was %v, want at most %v", i, w, ceiling)
		}
	}
}

// Operator error must stop the daemon. A restart loop around a typo buries the
// one message that would explain it.
func TestAPermanentFailureStopsTheDaemon(t *testing.T) {
	var attempts atomic.Int64
	boom := errors.New("listen tcp: address nonsense: missing port")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := supervise.Run(ctx, supervise.Config{
		Name:   "dashboard",
		Logger: quiet(),
		Run: func(context.Context) error {
			attempts.Add(1)
			return supervise.Permanent(boom)
		},
	})
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want it to wrap the permanent cause", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("ran %d times, want 1: a permanent failure must not be retried", got)
	}
}

// A panic in a subsystem is contained and reported, not fatal. The bug still
// has to be visible, so the stack must reach the log.
func TestAPanicIsContainedAndLogged(t *testing.T) {
	var logged strings.Builder
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var attempts atomic.Int64
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var waits []time.Duration
	err := supervise.Run(ctx, supervise.Config{
		Name:   "peering",
		Logger: log,
		After:  instant(&waits),
		Run: func(context.Context) error {
			if attempts.Add(1) == 1 {
				panic("malformed frame from a peer")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("ran %d times, want 2: the panic should have been retried", got)
	}
	out := logged.String()
	if !strings.Contains(out, "panicked") {
		t.Errorf("the panic was not logged: %q", out)
	}
	if !strings.Contains(out, "stack") {
		t.Error("the panic was logged without a stack, which is the part that helps")
	}
}

// Cancellation must stop the supervisor promptly, including while it is waiting
// out a backoff -- otherwise shutdown stalls for up to MaxBackoff.
func TestCancellationStopsTheSupervisorDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() {
		done <- supervise.Run(ctx, supervise.Config{
			Name:   "always-fails",
			Logger: quiet(),
			Run:    func(context.Context) error { return errors.New("nope") },
		})
	}()

	time.Sleep(100 * time.Millisecond) // let it fail once and enter backoff
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor did not stop when cancelled")
	}
}

func TestRunRequiresItsDependencies(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		cfg  supervise.Config
	}{
		{"no name", supervise.Config{Run: func(context.Context) error { return nil }, Logger: quiet()}},
		{"no run", supervise.Config{Name: "x", Logger: quiet()}},
		{"no logger", supervise.Config{Name: "x", Run: func(context.Context) error { return nil }}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := supervise.Run(ctx, tc.cfg); err == nil {
				t.Error("Run accepted an incomplete configuration")
			}
		})
	}
}

// Recover is the goroutine-boundary form, used where there is no supervisor to
// convert the panic into a restart.
func TestRecoverContainsAPanic(t *testing.T) {
	var logged strings.Builder
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	func() {
		defer supervise.Recover(log, "one peer exchange")
		panic("bad frame")
	}()

	out := logged.String()
	if !strings.Contains(out, "bad frame") {
		t.Errorf("the panic value was not logged: %q", out)
	}
	if !strings.Contains(out, "one peer exchange") {
		t.Errorf("the location was not logged: %q", out)
	}
}

// The common case must stay free: no panic, no log line.
func TestRecoverIsSilentWithoutAPanic(t *testing.T) {
	var logged strings.Builder
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	func() { defer supervise.Recover(log, "quiet work") }()

	if logged.Len() != 0 {
		t.Errorf("logged %q with nothing to report", logged.String())
	}
}
