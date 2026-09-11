package channel_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/christianparpart/agentic-stats/internal/mesh/channel"
)

// pair brings up one authenticated connection in both directions.
func pair(t *testing.T, cfg func(role string) channel.Config) (client, server *channel.Conn) {
	t.Helper()
	l, accepted := listen(t, cfg("server-node"))

	d, err := channel.NewDialer(cfg("client-node"))
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err = d.Dial(ctx, l.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	server = <-accepted
	if server == nil {
		t.Fatal("listener did not accept the connection")
	}
	t.Cleanup(func() { _ = server.Close() })
	return client, server
}

// A peer that completes the handshake and then goes quiet -- a suspended laptop
// is the ordinary way this happens -- must not park a reader indefinitely.
func TestASilentPeerFailsOnTheIdleDeadline(t *testing.T) {
	k, _ := keys(t)
	const idle = 300 * time.Millisecond
	client, _ := pair(t, func(role string) channel.Config {
		return channel.Config{Keys: k, NodeID: role, IdleTimeout: idle}
	})

	// The server is deliberately never written to again.
	start := time.Now()
	_, err := client.Read(make([]byte, 1))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("read from a silent peer succeeded; want a timeout")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Errorf("error = %v, want a timeout", err)
	}
	if elapsed > 5*idle {
		t.Errorf("took %v to give up on a silent peer, want about %v", elapsed, idle)
	}
}

// The idle deadline must bound silence, not total duration: a slow peer that
// keeps making progress is a first backfill over a poor link, and killing it
// would make large syncs impossible to complete.
func TestASlowButProgressingPeerIsNotCutOff(t *testing.T) {
	k, _ := keys(t)
	const idle = 300 * time.Millisecond
	client, server := pair(t, func(role string) channel.Config {
		return channel.Config{Keys: k, NodeID: role, IdleTimeout: idle}
	})

	const chunks = 6
	go func() {
		for range chunks {
			time.Sleep(idle / 2)
			if _, err := server.Write([]byte("x")); err != nil {
				return
			}
		}
	}()

	// Total time here is 3x the idle timeout, so a total deadline would fail.
	buf := make([]byte, 1)
	for i := range chunks {
		if _, err := client.Read(buf); err != nil {
			t.Fatalf("chunk %d: a progressing peer was cut off: %v", i, err)
		}
	}
}

// Cancelling the exchange context must unblock a read already parked in the
// kernel. Without Guard the context is advisory only and shutdown hangs.
func TestCancellingTheContextUnblocksAParkedRead(t *testing.T) {
	k, _ := keys(t)
	client, _ := pair(t, func(role string) channel.Config {
		// Long idle timeout: the cancellation must be what ends this, not the
		// deadline quietly rescuing the test.
		return channel.Config{Keys: k, NodeID: role, IdleTimeout: time.Hour}
	})

	ctx, cancel := context.WithCancel(context.Background())
	stop := client.Guard(ctx)
	defer stop()

	done := make(chan error, 1)
	go func() {
		_, err := client.Read(make([]byte, 1))
		done <- err
	}()

	time.Sleep(50 * time.Millisecond) // let the read reach the kernel
	cancel()

	select {
	case err := <-done:
		// The cause must survive: Guard cancels by moving the deadline into the
		// past, and reporting that timeout verbatim would make an orderly
		// shutdown look like an unresponsive peer.
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the context did not unblock the read")
	}
}

// Once guarded and cancelled, the connection must stay unblocked: a later
// operation must not push the deadline back out and resume waiting.
func TestAGuardedConnectionStaysCancelled(t *testing.T) {
	k, _ := keys(t)
	client, _ := pair(t, func(role string) channel.Config {
		return channel.Config{Keys: k, NodeID: role, IdleTimeout: time.Hour}
	})

	ctx, cancel := context.WithCancel(context.Background())
	stop := client.Guard(ctx)
	defer stop()
	cancel()

	if _, err := client.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Errorf("read after cancel = %v, want context.Canceled", err)
	}
	if _, err := client.Write([]byte("x")); !errors.Is(err, context.Canceled) {
		t.Errorf("write after cancel = %v, want context.Canceled", err)
	}
}

// Authentication runs off the accept loop, so a peer that opens a socket and
// then says nothing must not delay anyone else. Inline handshaking made this a
// denial of service that a port scanner performed by accident.
func TestAStalledHandshakeDoesNotBlockOtherPeers(t *testing.T) {
	k, _ := keys(t)
	l, err := channel.Listen(channel.Config{Keys: k, NodeID: "server-node"}, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = l.Close() }() // test cleanup

	// A raw TCP connection that never starts TLS, held open for the duration.
	stalled, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("dial stalled peer: %v", err)
	}
	defer func() { _ = stalled.Close() }() // test cleanup

	d, err := channel.NewDialer(channel.Config{Keys: k, NodeID: "client-node"})
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	accepted := make(chan *channel.Conn, 1)
	go func() {
		c, aerr := l.Accept(ctx)
		if aerr != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()

	client, err := d.Dial(ctx, l.Addr().String())
	if err != nil {
		t.Fatalf("a real peer could not connect past a stalled one: %v", err)
	}
	defer func() { _ = client.Close() }() // test cleanup

	select {
	case conn := <-accepted:
		if conn == nil {
			t.Fatal("listener returned no connection")
		}
		defer func() { _ = conn.Close() }() // test cleanup
		if conn.PeerNodeID != "client-node" {
			t.Errorf("accepted peer %q, want client-node", conn.PeerNodeID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a stalled handshake blocked the accept loop")
	}
}

// A burst of connections that never handshake must not wedge the listener.
//
// The regression this guards against is a leaked slot: if the pending-handshake
// semaphore is not released on every path, a burst permanently reduces capacity
// and eventually stops inbound peering altogether. The burst is deliberately
// larger than the cap so that shedding is exercised too.
//
// What is asserted is that capacity comes back, not that it comes back by any
// particular moment. Closing the client ends of the burst does not itself free
// the listener's slots -- the accept loop still has to take each socket off the
// backlog and let each handshake fail -- so for a short while afterwards the
// listener is still legitimately entitled to refuse.
func TestTheListenerRecoversAfterABurstOfDeadConnections(t *testing.T) {
	k, _ := keys(t)
	l, err := channel.Listen(channel.Config{Keys: k, NodeID: "server-node"}, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = l.Close() }() // test cleanup

	// Comfortably more raw connections than the cap, none of them handshaking.
	var stalled []net.Conn
	for range 128 {
		c, derr := (&net.Dialer{}).DialContext(context.Background(), "tcp", l.Addr().String())
		if derr != nil {
			break
		}
		stalled = append(stalled, c)
	}
	if len(stalled) == 0 {
		t.Fatal("could not open any connections")
	}
	// Drop the burst. Every slot it occupied must come back.
	for _, c := range stalled {
		_ = c.Close()
	}

	d, err := channel.NewDialer(channel.Config{Keys: k, NodeID: "client-node"})
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	accepted := make(chan *channel.Conn, 1)
	go func() {
		c, aerr := l.Accept(ctx)
		if aerr != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()

	// Retried until the deadline rather than attempted once. A single attempt
	// races the drain described above: an attempt that arrives while the burst
	// is still being shed is closed mid-handshake, which the dialer reports as
	// a connection that went away -- and that is the listener behaving as
	// designed, not the failure this test is looking for.
	//
	// Retrying still proves the thing it exists to prove. A genuinely leaked
	// slot never comes back, so no attempt within the deadline would succeed.
	var client *channel.Conn
	for {
		var derr error
		if client, derr = d.Dial(ctx, l.Addr().String()); derr == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("a real peer could not connect after the burst cleared: %v", derr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer func() { _ = client.Close() }() // test cleanup

	select {
	case conn := <-accepted:
		if conn == nil {
			t.Fatal("listener returned no connection after the burst cleared")
		}
		_ = conn.Close()
	case <-time.After(15 * time.Second):
		t.Fatal("the listener did not recover after the burst cleared")
	}
}
