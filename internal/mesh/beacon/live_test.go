package beacon

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/ipv4"
)

// requireMulticastLoopback skips unless this host actually delivers a
// multicast datagram back to another socket on the same machine.
//
// Binding the port is not evidence that multicast works. GitHub's hosted macOS
// runners bind it happily and then deliver nothing, which made this test fail
// there for a reason that has nothing to do with the beacon -- the beacon sets
// SetMulticastLoopback(true) and is correct. A test that cannot tell "this
// environment has no multicast" from "discovery is broken" reports the wrong
// one, and the wrong one is the expensive mistake.
//
// The probe deliberately uses raw sockets rather than the beacon, so it
// measures the environment and cannot be fooled by a regression in our own
// send path.
func requireMulticastLoopback(t *testing.T) {
	t.Helper()
	lc := net.ListenConfig{Control: reuseControl}
	addr := fmt.Sprintf("0.0.0.0:%d", Port)

	rx, err := lc.ListenPacket(context.Background(), "udp4", addr)
	if err != nil {
		t.Skipf("multicast port unavailable in this environment: %v", err)
	}
	defer func() { _ = rx.Close() }() // probe only

	pc := ipv4.NewPacketConn(rx)
	if err := pc.SetMulticastLoopback(true); err != nil {
		t.Skipf("multicast loopback unavailable in this environment: %v", err)
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	joined := 0
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagMulticast == 0 || ifi.Flags&net.FlagUp == 0 {
			continue
		}
		if err := pc.JoinGroup(&ifi, &net.UDPAddr{IP: Group, Port: Port}); err == nil {
			joined++
		}
	}
	if joined == 0 {
		t.Skip("no interface on this host accepts a multicast join")
	}

	tx, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp4", "0.0.0.0:0")
	if err != nil {
		t.Skipf("cannot open a sender: %v", err)
	}
	defer func() { _ = tx.Close() }() // probe only

	probe := []byte("agentic-stats multicast probe")
	if _, err := tx.WriteTo(probe, &net.UDPAddr{IP: Group, Port: Port}); err != nil {
		t.Skipf("cannot send to the multicast group: %v", err)
	}
	if err := rx.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set probe deadline: %v", err)
	}
	buf := make([]byte, len(probe))
	if _, _, err := rx.ReadFrom(buf); err != nil {
		t.Skipf("this environment does not deliver multicast between local sockets: %v", err)
	}
}

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
	requireMulticastLoopback(t)
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
