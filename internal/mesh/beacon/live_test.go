package beacon

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

// Two beacons on this host must find each other. A machine and the VM running
// on it is exactly this arrangement, so it has to work rather than merely work
// across separate hosts.
func TestBeaconsOnOneHostFindEachOther(t *testing.T) {
	if testing.Short() {
		t.Skip("uses real multicast sockets")
	}
	// Probe first: a sandbox that forbids multicast, or a stray daemon holding
	// the port, is an environment problem rather than a regression. A failure
	// *after* both beacons bind is a real one and must not be skipped.
	lc := net.ListenConfig{Control: reuseControl}
	probe, err := lc.ListenPacket(context.Background(), "udp4",
		fmt.Sprintf("0.0.0.0:%d", Port))
	if err != nil {
		t.Skipf("multicast port unavailable in this environment: %v", err)
	}
	if err := probe.Close(); err != nil {
		t.Fatalf("close probe: %v", err)
	}
	k := keys(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var (
		mu    sync.Mutex
		seen  = map[string]bool{}
		found = make(chan string, 8)
	)
	report := func(who string) func(Peer) {
		return func(p Peer) {
			mu.Lock()
			defer mu.Unlock()
			key := who + "<-" + p.NodeID
			if !seen[key] {
				seen[key] = true
				found <- key
			}
		}
	}

	for _, spec := range []struct{ id, label string }{{"node-alpha", "alpha"}, {"node-beta", "beta"}} {
		b, err := New(Config{Keys: k, NodeID: spec.id, SyncPort: 8844})
		if err != nil {
			t.Fatalf("New(%s): %v", spec.id, err)
		}
		go func() {
			if err := b.Run(ctx, report(spec.label)); err != nil {
				t.Errorf("beacon %s stopped: %v", spec.id, err)
			}
		}()
	}

	// A third node on a different key must never appear.
	stranger, err := New(Config{Keys: keys(t), NodeID: "node-stranger", SyncPort: 8844})
	if err != nil {
		t.Fatalf("New(stranger): %v", err)
	}
	go func() {
		if err := stranger.Run(ctx, report("stranger")); err != nil {
			t.Errorf("stranger beacon stopped: %v", err)
		}
	}()

	deadline := time.After(15 * time.Second)
	for {
		select {
		case key := <-found:
			if key == "alpha<-node-stranger" || key == "beta<-node-stranger" {
				t.Fatalf("a node on a different mesh key was discovered: %s", key)
			}
			mu.Lock()
			done := seen["alpha<-node-beta"] && seen["beta<-node-alpha"]
			mu.Unlock()
			if done {
				return
			}
		case <-deadline:
			mu.Lock()
			t.Fatalf("beacons did not find each other within the deadline; saw %v", seen)
		}
	}
}
