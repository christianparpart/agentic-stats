// Package channel establishes authenticated connections between peers.
//
// Go's crypto/tls has no TLS-PSK and is not getting it, so authentication is
// TLS 1.3 for confidentiality plus an HMAC over the connection's exported
// keying material. A relay that terminates TLS on both sides must run two
// distinct handshakes, which export different material, so a tag computed on
// one leg cannot be replayed onto the other. That is RFC 9266 channel binding
// rather than a construction invented here.
//
// The package exposes only Dial and Accept, both of which return an
// already-authenticated Conn. A raw *tls.Conn never escapes, so no caller can
// forget to verify.
package channel

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/christianparpart/agentic-stats/internal/certs"
	"github.com/christianparpart/agentic-stats/internal/seal"
)

// ErrNotInMesh reports a peer that could not prove it holds the pre-shared key.
var ErrNotInMesh = errors.New("channel: peer is not in this mesh")

// exporterLabel identifies our use of the TLS exporter. RFC 5705 labels must
// be unique to the application.
const exporterLabel = "EXPORTER-agentic-stats-psk-binding-v1"

// exporterLength is how much keying material to bind to.
const exporterLength = 32

// handshakeTimeout bounds the authentication exchange.
const handshakeTimeout = 20 * time.Second

// greeting is what each side sends once TLS is up.
type greeting struct {
	NodeID string `json:"node_id"`
	Tag    []byte `json:"tag"`
}

// Config is everything the channel needs.
type Config struct {
	// Keys derive the tag. Required.
	Keys *seal.Keys
	// NodeID identifies this node to its peer. Required.
	NodeID string
	// Certificate is presented by the listener. Zero generates an ephemeral
	// one, which is correct here: the certificate is not the trust anchor.
	Certificate *tls.Certificate
}

// Conn is an authenticated connection to a peer.
type Conn struct {
	net.Conn
	// PeerNodeID is the peer's replication identity, proven only to the extent
	// that the peer holds the mesh key.
	PeerNodeID string
}

// Dialer opens authenticated connections.
type Dialer struct {
	cfg Config
}

// NewDialer returns a Dialer.
func NewDialer(cfg Config) (*Dialer, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Dialer{cfg: cfg}, nil
}

func (c Config) validate() error {
	if c.Keys == nil {
		return errors.New("channel: Config.Keys is required")
	}
	if c.NodeID == "" {
		return errors.New("channel: Config.NodeID is required")
	}
	return nil
}

// Dial connects to a peer and authenticates it.
func (d *Dialer) Dial(ctx context.Context, addr string) (*Conn, error) {
	// The certificate is deliberately unverified: it is not a trust anchor.
	// Authentication is the exporter-bound tag below, which a relay cannot
	// forge without the mesh key.
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
		NextProtos:         []string{"agentic-stats/1"},
	}
	dialer := &tls.Dialer{Config: tlsCfg}
	raw, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("channel: dial %s: %w", addr, err)
	}
	conn, ok := raw.(*tls.Conn)
	if !ok {
		return nil, errors.Join(errors.New("channel: unexpected connection type"), raw.Close())
	}
	peer, err := authenticate(ctx, conn, d.cfg, "client")
	if err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	return &Conn{Conn: conn, PeerNodeID: peer}, nil
}

// Listener accepts authenticated connections.
type Listener struct {
	inner net.Listener
	cfg   Config
}

// Listen binds addr and authenticates everything accepted on it.
func Listen(cfg Config, addr string) (*Listener, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cert := cfg.Certificate
	if cert == nil {
		generated, err := certs.EnsureCertificate(certs.Config{})
		if err != nil {
			return nil, err
		}
		cert = &generated
	}
	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{*cert},
		NextProtos:   []string{"agentic-stats/1"},
	}
	inner, err := tls.Listen("tcp", addr, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("channel: listen %s: %w", addr, err)
	}
	return &Listener{inner: inner, cfg: cfg}, nil
}

// Addr reports the bound address.
func (l *Listener) Addr() net.Addr { return l.inner.Addr() }

// Close stops accepting.
func (l *Listener) Close() error { return l.inner.Close() }

// Accept returns the next authenticated connection.
//
// A peer that fails authentication is closed and skipped rather than surfaced,
// so a caller cannot accidentally use one.
func (l *Listener) Accept(ctx context.Context) (*Conn, error) {
	for {
		raw, err := l.inner.Accept()
		if err != nil {
			return nil, fmt.Errorf("channel: accept: %w", err)
		}
		conn, ok := raw.(*tls.Conn)
		if !ok {
			_ = raw.Close()
			continue
		}
		peer, err := authenticate(ctx, conn, l.cfg, "server")
		if err != nil {
			_ = conn.Close()
			if errors.Is(err, ErrNotInMesh) {
				// Expected: someone else's mesh, or a port scanner.
				continue
			}
			continue
		}
		return &Conn{Conn: conn, PeerNodeID: peer}, nil
	}
}

// authenticate performs the mutual exporter-bound exchange.
//
// No application data may be written before this returns successfully, or a
// relay would read the prologue in the clear.
func authenticate(ctx context.Context, conn *tls.Conn, cfg Config, role string) (string, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(handshakeTimeout)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return "", fmt.Errorf("channel: set deadline: %w", err)
	}
	defer func() { _ = conn.SetDeadline(time.Time{}) }() // restore blocking behaviour

	if err := conn.HandshakeContext(ctx); err != nil {
		return "", fmt.Errorf("channel: tls handshake: %w", err)
	}

	state := conn.ConnectionState()
	ekm, err := state.ExportKeyingMaterial(exporterLabel, nil, exporterLength)
	if err != nil {
		// Only possible if renegotiation was enabled or the version dropped,
		// both of which we forbid; treat it as fatal rather than degrade.
		return "", fmt.Errorf("channel: export keying material: %w", err)
	}

	peerRole := "server"
	if role == "server" {
		peerRole = "client"
	}

	mine := greeting{NodeID: cfg.NodeID, Tag: cfg.Keys.ChannelTag(ekm, role)}
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(io.LimitReader(conn, 4096))

	// The client speaks first so a scanner learns nothing by connecting.
	send := func() error { return enc.Encode(mine) }
	recv := func() (greeting, error) {
		var g greeting
		if err := dec.Decode(&g); err != nil {
			return greeting{}, fmt.Errorf("channel: read greeting: %w", err)
		}
		return g, nil
	}

	var theirs greeting
	if role == "client" {
		if err := send(); err != nil {
			return "", fmt.Errorf("channel: send greeting: %w", err)
		}
		if theirs, err = recv(); err != nil {
			return "", err
		}
	} else {
		if theirs, err = recv(); err != nil {
			return "", err
		}
		if err := send(); err != nil {
			return "", fmt.Errorf("channel: send greeting: %w", err)
		}
	}

	want := cfg.Keys.ChannelTag(ekm, peerRole)
	if !seal.Equal(theirs.Tag, want) {
		return "", ErrNotInMesh
	}
	if theirs.NodeID == "" {
		return "", fmt.Errorf("channel: peer sent no node id")
	}
	return theirs.NodeID, nil
}
