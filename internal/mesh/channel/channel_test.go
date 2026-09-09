package channel_test

import (
	"context"
	"crypto/tls"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/christianparpart/agentic-stats/internal/mesh/channel"
	"github.com/christianparpart/agentic-stats/internal/seal"
)

func keys(t *testing.T) (*seal.Keys, string) {
	t.Helper()
	psk, err := seal.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	k, err := seal.Derive(psk)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	return k, psk
}

// listen starts a listener that accepts one connection and reports the result.
func listen(t *testing.T, cfg channel.Config) (*channel.Listener, <-chan *channel.Conn) {
	t.Helper()
	l, err := channel.Listen(cfg, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	accepted := make(chan *channel.Conn, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, err := l.Accept(ctx)
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()
	return l, accepted
}

func TestPeersWithTheSameKeyAuthenticate(t *testing.T) {
	k, _ := keys(t)
	l, accepted := listen(t, channel.Config{Keys: k, NodeID: "server-node"})

	d, err := channel.NewDialer(channel.Config{Keys: k, NodeID: "client-node"})
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := d.Dial(ctx, l.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = client.Close() }() // test cleanup

	server := <-accepted
	if server == nil {
		t.Fatal("listener did not accept the connection")
	}
	defer func() { _ = server.Close() }() // test cleanup

	if client.PeerNodeID != "server-node" {
		t.Errorf("client saw peer %q, want server-node", client.PeerNodeID)
	}
	if server.PeerNodeID != "client-node" {
		t.Errorf("server saw peer %q, want client-node", server.PeerNodeID)
	}

	// The connection must actually carry data afterwards.
	go func() { _, _ = server.Write([]byte("hello peer")) }()
	buf := make([]byte, len("hello peer"))
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("read after authentication: %v", err)
	}
	if string(buf) != "hello peer" {
		t.Errorf("read %q, want hello peer", buf)
	}
}

// Someone else's mesh on the same network must get nowhere.
func TestDifferentKeyIsRejected(t *testing.T) {
	ours, _ := keys(t)
	theirs, _ := keys(t)

	l, accepted := listen(t, channel.Config{Keys: ours, NodeID: "ours"})
	d, err := channel.NewDialer(channel.Config{Keys: theirs, NodeID: "theirs"})
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, err := d.Dial(ctx, l.Addr().String())
	if err == nil {
		_ = conn.Close()
		t.Fatal("a peer with the wrong key completed the handshake")
	}

	select {
	case c, ok := <-accepted:
		if ok && c != nil {
			_ = c.Close()
			t.Fatal("the listener surfaced an unauthenticated peer")
		}
	case <-time.After(500 * time.Millisecond):
		// The listener is still waiting for a real peer, which is correct.
	}
}

// The property the exporter binding exists for: a relay that terminates TLS on
// both sides, holding no key, must not be able to join the two legs.
func TestTLSTerminatingRelayIsDefeated(t *testing.T) {
	k, _ := keys(t)

	// The real peer.
	realListener, _ := listen(t, channel.Config{Keys: k, NodeID: "real-server"})

	// The relay: it accepts TLS from the client and opens its own TLS session
	// to the real peer, forwarding bytes verbatim. It never sees the mesh key.
	relayCert, err := selfSigned()
	if err != nil {
		t.Fatalf("relay certificate: %v", err)
	}
	relay, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{relayCert},
		NextProtos:   []string{"agentic-stats/1"},
	})
	if err != nil {
		t.Fatalf("relay listen: %v", err)
	}
	defer func() { _ = relay.Close() }() // test cleanup

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		front, err := relay.Accept()
		if err != nil {
			return
		}
		defer func() { _ = front.Close() }()

		dialer := &tls.Dialer{Config: &tls.Config{
			MinVersion:         tls.VersionTLS13,
			InsecureSkipVerify: true,
			NextProtos:         []string{"agentic-stats/1"},
		}}
		back, err := dialer.DialContext(context.Background(), "tcp", realListener.Addr().String())
		if err != nil {
			return
		}
		defer func() { _ = back.Close() }()

		go func() { _, _ = io.Copy(back, front) }()
		_, _ = io.Copy(front, back)
	}()

	d, err := channel.NewDialer(channel.Config{Keys: k, NodeID: "client"})
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, err := d.Dial(ctx, relay.Addr().String())
	if err == nil {
		_ = conn.Close()
		t.Fatal("a TLS-terminating relay completed the handshake; " +
			"the exporter binding is not doing its job")
	}
	_ = relay.Close()
	wg.Wait()
}

// selfSigned mints a throwaway certificate for the relay in the test above.
func selfSigned() (tls.Certificate, error) {
	// Reuse the production helper: the relay is meant to look ordinary.
	return ephemeralCert()
}

func TestConfigRequiresItsDependencies(t *testing.T) {
	k, _ := keys(t)
	if _, err := channel.NewDialer(channel.Config{NodeID: "n"}); err == nil {
		t.Error("expected an error when Keys is missing")
	}
	if _, err := channel.NewDialer(channel.Config{Keys: k}); err == nil {
		t.Error("expected an error when NodeID is missing")
	}
	if _, err := channel.Listen(channel.Config{Keys: k}, "127.0.0.1:0"); err == nil {
		t.Error("expected an error when NodeID is missing")
	}
}
