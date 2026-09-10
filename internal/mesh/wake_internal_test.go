package mesh

import (
	"testing"
	"time"
)

// newForWake builds only the parts Wake touches. Wake is deliberately
// independent of the store and the network, so it can be tested without them.
func newForWake() *Mesh {
	return &Mesh{
		backoff: make(map[string]time.Time),
		sighted: make(map[string]struct{}),
		wake:    make(chan struct{}, 1),
	}
}

// Discovering a peer must make the node speak to it, not merely write it down.
// Waiting for the next sweep meant a fresh node -- which swept an empty list on
// startup and has no stored peers -- sat next to its neighbour for the best part
// of a minute.
func TestOnlyTheFirstSightingOfAPeerNudges(t *testing.T) {
	m := newForWake()

	if !m.firstSighting("peer-a") {
		t.Error("the first sighting of a peer was not treated as new")
	}
	// A beacon arrives from every peer every thirty seconds. Sweeping on each
	// would be a busy loop wearing a discovery protocol as a hat.
	for range 5 {
		if m.firstSighting("peer-a") {
			t.Error("a repeat sighting was treated as new")
		}
	}
	if !m.firstSighting("peer-b") {
		t.Error("a different peer's first sighting was not treated as new")
	}
}

// nudge asks for a sweep; unlike Wake it must leave backoff alone, or one
// machine waking up would trigger a retry storm against every machine that is
// switched off.
func TestNudgeSweepsWithoutClearingBackoff(t *testing.T) {
	m := newForWake()
	const addr = "sleeping.example:8844"
	m.penalize(addr)

	m.nudge()

	select {
	case <-m.wake:
	default:
		t.Error("nudge did not ask for a sweep")
	}
	if !m.inBackoff(addr) {
		t.Error("nudge cleared backoff; only a resume should do that")
	}
}

// After a resume, nothing in the backoff table is trustworthy: it was learned
// before the interruption, possibly on a different network.
func TestWakeClearsEveryBackoff(t *testing.T) {
	m := newForWake()
	addrs := []string{"laptop:8844", "vm.internal:8844", "10.0.0.4:8844"}
	for _, a := range addrs {
		m.penalize(a)
		if !m.inBackoff(a) {
			t.Fatalf("%s was not backed off after a failure", a)
		}
	}

	m.Wake()

	for _, a := range addrs {
		if m.inBackoff(a) {
			t.Errorf("%s is still backed off after a resume", a)
		}
	}
}

// The dial loop must actually be told, or clearing the table only takes effect
// at the next ordinary tick and the resume saved nothing.
func TestWakeSignalsTheDialLoop(t *testing.T) {
	m := newForWake()
	m.Wake()

	select {
	case <-m.wake:
	default:
		t.Error("Wake did not signal the dial loop")
	}
}

// Several wake-ups arriving together mean the same thing as one, and Wake must
// never block: it is called from the detector's goroutine, which has no
// business waiting on the dial loop.
func TestRepeatedWakesCoalesceWithoutBlocking(t *testing.T) {
	m := newForWake()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			m.Wake()
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wake blocked when the dial loop was not listening")
	}

	// One pending sweep, not a hundred.
	<-m.wake
	select {
	case <-m.wake:
		t.Error("wake-ups queued up instead of coalescing")
	default:
	}
}

// Backoff must still grow for an address that keeps failing; Wake resets the
// table, it does not disable it.
func TestBackoffStillAppliesAfterAWake(t *testing.T) {
	m := newForWake()
	const addr = "gone.example:8844"

	m.Wake()
	m.penalize(addr)
	if !m.inBackoff(addr) {
		t.Fatal("an address that failed after a resume was not backed off")
	}
}
