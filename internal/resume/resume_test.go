package resume_test

import (
	"context"
	"testing"
	"time"

	"github.com/christianparpart/agentic-stats/internal/resume"
)

const (
	interval  = 10 * time.Second
	threshold = time.Minute
)

// detector drives Watch by hand: the test decides both what time it is and when
// the detector gets to look, so no assertion depends on real elapsed time.
type detector struct {
	now   time.Time
	tick  chan time.Time
	woke  chan struct{}
	stop  context.CancelFunc
	ended chan struct{}
}

func start(t *testing.T) *detector {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	d := &detector{
		now:   time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		tick:  make(chan time.Time),
		woke:  make(chan struct{}, 16),
		stop:  cancel,
		ended: make(chan struct{}),
	}
	go func() {
		defer close(d.ended)
		resume.Watch(ctx, resume.Config{
			Interval:  interval,
			Threshold: threshold,
			// Read under the tick handshake below, never concurrently.
			Clock: func() time.Time { return d.now },
			Tick:  d.tick,
		}, func() { d.woke <- struct{}{} })
	}()
	t.Cleanup(func() {
		cancel()
		<-d.ended
	})

	// Settle: one tick with the clock untouched. Watch takes its first reading
	// on entry, so without this the test's first elapse would be folded into
	// that reading instead of measured against it.
	d.tick <- time.Time{}
	if d.detected() {
		t.Fatal("the detector reported a resume before any time had passed")
	}
	return d
}

// elapse moves the clock and lets the detector look exactly once.
//
// The unbuffered tick is the handshake: the send returns only once Watch has
// received it, so the clock is never written while Watch is reading it.
func (d *detector) elapse(by time.Duration) {
	d.now = d.now.Add(by)
	d.tick <- time.Time{}
}

// detected reports whether the last tick was treated as a resume. Watch calls
// woke before returning to the loop, so by the time the next tick is accepted
// the signal has already been sent.
func (d *detector) detected() bool {
	// Give the callback a moment to land without making the test wait when it
	// has not fired; one more tick would be cleaner but changes the state.
	select {
	case <-d.woke:
		return true
	case <-time.After(50 * time.Millisecond):
		return false
	}
}

// Time passing normally must not look like a suspend, or every tick would
// trigger a peer sweep.
func TestNormalOperationDoesNotLookLikeASuspend(t *testing.T) {
	d := start(t)
	for range 10 {
		d.elapse(interval)
		if d.detected() {
			t.Fatal("normal operation was reported as a resume")
		}
	}
}

// A gap far larger than the interval is the signal a suspended machine leaves.
func TestALargeGapIsDetected(t *testing.T) {
	d := start(t)
	d.elapse(2 * time.Hour) // a laptop shut for the afternoon
	if !d.detected() {
		t.Error("a two-hour gap was not detected as a resume")
	}
}

// Sub-threshold lateness is scheduling noise on a loaded machine. Treating it
// as a suspend would sweep peers constantly under load.
func TestSubThresholdLatenessIsNotASuspend(t *testing.T) {
	d := start(t)
	tests := []struct {
		name string
		gap  time.Duration
	}{
		{"on time", interval},
		{"a little late", interval + threshold/4},
		{"almost at the threshold", interval + threshold - time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d.elapse(tc.gap)
			if d.detected() {
				t.Errorf("a gap of %v was reported as a resume", tc.gap)
			}
		})
	}
}

// Just past the threshold must fire, so the boundary is pinned from both sides.
func TestJustPastTheThresholdIsASuspend(t *testing.T) {
	d := start(t)
	d.elapse(interval + threshold + time.Second)
	if !d.detected() {
		t.Error("a gap just past the threshold was not detected")
	}
}

// Waking must not latch: a machine suspended twice has to be noticed twice.
func TestASecondSuspendIsDetected(t *testing.T) {
	d := start(t)
	for i := range 3 {
		d.elapse(time.Hour)
		if !d.detected() {
			t.Fatalf("suspend %d was not detected", i+1)
		}
	}
}

// Watch must return when cancelled rather than outliving the daemon.
func TestWatchStopsWhenCancelled(t *testing.T) {
	d := start(t)
	d.stop()
	select {
	case <-d.ended:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return when cancelled")
	}
}
