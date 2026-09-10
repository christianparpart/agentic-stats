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
	"sync"
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

// DefaultIdleTimeout bounds a single read or write once the connection is up.
//
// It is an idle timeout, not a total one: every operation that makes progress
// pushes it out again, so a multi-hour first backfill over a slow link is fine
// while a peer that has stopped speaking is not. A total deadline would have to
// be set to the worst imaginable sync and would therefore bound nothing useful.
const DefaultIdleTimeout = 60 * time.Second

// maxPendingHandshakes bounds authentications in flight.
//
// Authentication used to run inline in the accept loop, so one peer that
// completed TCP and then went quiet blocked every other peer for the whole
// handshake timeout. A port scanner did the same thing for free. Handshakes now
// run concurrently, and this caps how many can be pinned open at once; past the
// cap connections are dropped rather than queued, because queueing them is the
// same denial with extra memory.
//
// The number is deliberately far above a real fleet, because the cap bounds
// memory and not fairness. Every occupied slot is unavailable to a legitimate
// peer until its handshake times out, so a tight cap would turn a handful of
// stalled sockets into an outage for everyone else. A pending handshake costs
// little, so the generous figure is close to free.
const maxPendingHandshakes = 64

// keepAlive reaps the half-open connections a suspend leaves behind.
//
// When a laptop sleeps or a VM is suspended mid-exchange, its peer is left
// holding a socket that will never produce another byte and never signal a
// close. Without probes it lingers until the operating system's own default
// expires, which is measured in hours and differs on each of the three
// platforms this ships to. Probing settles it in about a minute, everywhere.
var keepAlive = net.KeepAliveConfig{
	Enable:   true,
	Idle:     30 * time.Second,
	Interval: 10 * time.Second,
	Count:    3,
}

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
	// IdleTimeout bounds one read or write. Zero uses DefaultIdleTimeout.
	IdleTimeout time.Duration
}

// idleTimeout resolves the configured bound.
func (c Config) idleTimeout() time.Duration {
	if c.IdleTimeout > 0 {
		return c.IdleTimeout
	}
	return DefaultIdleTimeout
}

// Conn is an authenticated connection to a peer.
//
// Read and Write are overridden to carry an idle deadline, so a peer that stops
// speaking mid-exchange fails in DefaultIdleTimeout rather than parking a
// goroutine until TCP gives up. Callers see an ordinary io.ReadWriter and need
// to know none of this.
type Conn struct {
	net.Conn
	// PeerNodeID is the peer's replication identity, proven only to the extent
	// that the peer holds the mesh key.
	PeerNodeID string

	idle time.Duration
	// guard, once set by Guard, is consulted before and after every operation.
	// It is written once before any I/O begins and never again.
	guard context.Context
}

// Guard makes ctx able to interrupt this connection, returning a stop function.
//
// A socket read blocked in the kernel does not observe context cancellation:
// the only thing that interrupts it is a deadline. Without this, cancelling the
// exchange context on shutdown or timeout leaves the read parked exactly as it
// was, which is the difference between a daemon that stops when asked and one
// that has to be killed.
//
// Call it before any I/O, and call the returned stop when the exchange is done
// so the watchdog goroutine does not outlive it.
func (c *Conn) Guard(ctx context.Context) (stop func()) {
	c.guard = ctx
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// A deadline in the past unblocks whatever is waiting, and Read and
			// Write then report the context's cause rather than a bare timeout.
			_ = c.Conn.SetDeadline(time.Now())
		case <-done:
		}
	}()
	return func() { close(done) }
}

// Read refreshes the idle deadline and reads.
func (c *Conn) Read(p []byte) (int, error) {
	if err := c.before(c.Conn.SetReadDeadline); err != nil {
		return 0, err
	}
	n, err := c.Conn.Read(p)
	return n, c.after(err)
}

// Write refreshes the idle deadline and writes.
func (c *Conn) Write(p []byte) (int, error) {
	if err := c.before(c.Conn.SetWriteDeadline); err != nil {
		return 0, err
	}
	n, err := c.Conn.Write(p)
	return n, c.after(err)
}

// before pushes the idle deadline out for one operation.
func (c *Conn) before(set func(time.Time) error) error {
	if c.guard != nil {
		if err := c.guard.Err(); err != nil {
			return err
		}
	}
	return set(time.Now().Add(c.idle))
}

// after reports the cancellation cause in place of the timeout it produced.
//
// Guard cancels by moving the deadline into the past, so the error surfacing
// from the socket is a timeout no matter why it was cancelled. Reporting that
// verbatim would make an orderly shutdown look like an unresponsive peer.
func (c *Conn) after(err error) error {
	if err == nil || c.guard == nil {
		return err
	}
	if cause := c.guard.Err(); cause != nil {
		return cause
	}
	return err
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
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{KeepAliveConfig: keepAlive},
		Config:    tlsCfg,
	}
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
	return &Conn{Conn: conn, PeerNodeID: peer, idle: d.cfg.idleTimeout()}, nil
}

// Listener accepts authenticated connections.
//
// Handshakes run off the accept loop, so a peer that connects and then says
// nothing delays only itself.
type Listener struct {
	inner  net.Listener
	cfg    Config
	ready  chan *Conn
	closed chan struct{}
	once   sync.Once
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
	// KeepAliveConfig here covers the accepted side; the dialer sets its own.
	lc := net.ListenConfig{KeepAliveConfig: keepAlive}
	base, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("channel: listen %s: %w", addr, err)
	}
	l := &Listener{
		inner:  tls.NewListener(base, tlsCfg),
		cfg:    cfg,
		ready:  make(chan *Conn),
		closed: make(chan struct{}),
	}
	go l.serve()
	return l, nil
}

// Addr reports the bound address.
func (l *Listener) Addr() net.Addr { return l.inner.Addr() }

// Close stops accepting.
func (l *Listener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return l.inner.Close()
}

// serve accepts connections and authenticates them concurrently.
func (l *Listener) serve() {
	pending := make(chan struct{}, maxPendingHandshakes)
	for {
		raw, err := l.inner.Accept()
		if err != nil {
			// Close is the ordinary reason to land here; anything else has
			// already broken the listener, and Accept reports it to the caller.
			return
		}
		select {
		case pending <- struct{}{}:
		default:
			// Shedding load beats queueing it: a caller holding the cap open is
			// exactly the caller we do not want to allocate for.
			_ = raw.Close()
			continue
		}
		go func() {
			defer func() { <-pending }()
			conn := l.authenticated(raw)
			if conn == nil {
				return
			}
			select {
			case l.ready <- conn:
			case <-l.closed:
				_ = conn.Close()
			}
		}()
	}
}

// authenticated completes one handshake, returning nil for anything that fails.
//
// A peer that fails authentication is closed and dropped rather than surfaced,
// so a caller cannot accidentally use one.
func (l *Listener) authenticated(raw net.Conn) *Conn {
	conn, ok := raw.(*tls.Conn)
	if !ok {
		_ = raw.Close()
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()

	peer, err := authenticate(ctx, conn, l.cfg, "server")
	if err != nil {
		// Someone else's mesh, a port scanner, or a half-open connection: all
		// expected, none worth a log line at this level.
		_ = conn.Close()
		return nil
	}
	return &Conn{Conn: conn, PeerNodeID: peer, idle: l.cfg.idleTimeout()}
}

// Accept returns the next authenticated connection.
func (l *Listener) Accept(ctx context.Context) (*Conn, error) {
	select {
	case conn := <-l.ready:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
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
