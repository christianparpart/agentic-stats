// Package mesh runs a node's peering: it announces, discovers, dials, accepts,
// and converges with everyone holding the same pre-shared key.
//
// There is no leader and no membership protocol. A peer is anything that can
// complete the authenticated handshake, and every exchange is symmetric.
package mesh

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/christianparpart/agentic-stats/internal/mesh/beacon"
	"github.com/christianparpart/agentic-stats/internal/mesh/channel"
	meshsync "github.com/christianparpart/agentic-stats/internal/mesh/sync"
	"github.com/christianparpart/agentic-stats/internal/seal"
	"github.com/christianparpart/agentic-stats/internal/store"
)

// dialInterval is how often the node sweeps its known peers.
const dialInterval = 60 * time.Second

// maxBackoff caps the wait after repeated failures. A peer may be off for a
// week; there is no point retrying it every minute forever, and no point
// giving up on it either.
const maxBackoff = 15 * time.Minute

// dialTimeout bounds one peer exchange.
const dialTimeout = 5 * time.Minute

// Config is everything the mesh needs.
type Config struct {
	Store  *store.DB
	Keys   *seal.Keys
	Logger *slog.Logger

	// Listen is the address peers connect to. Empty disables inbound peering.
	Listen string
	// StaticPeers are addresses discovery cannot reach — anything across a
	// tunnel, since a point-to-point link carries no multicast.
	StaticPeers []string
	// Discovery enables the multicast beacon.
	Discovery bool
	// ExcludeInterfaces keeps beacons off VM bridges and similar.
	ExcludeInterfaces []string
}

// Mesh is a node's peering subsystem.
type Mesh struct {
	cfg    Config
	log    *slog.Logger
	syncer *meshsync.Syncer
	dialer *channel.Dialer
	nodeID string

	mu      sync.Mutex
	backoff map[string]time.Time
}

// New returns a Mesh over the given store.
func New(cfg Config) (*Mesh, error) {
	if cfg.Store == nil {
		return nil, errors.New("mesh: a store is required")
	}
	if cfg.Keys == nil {
		return nil, errors.New("mesh: keys are required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	nodeID := cfg.Store.OriginID()

	syncer, err := meshsync.New(cfg.Store, log)
	if err != nil {
		return nil, err
	}
	dialer, err := channel.NewDialer(channel.Config{Keys: cfg.Keys, NodeID: nodeID})
	if err != nil {
		return nil, err
	}
	return &Mesh{
		cfg: cfg, log: log, syncer: syncer, dialer: dialer,
		nodeID: nodeID, backoff: make(map[string]time.Time),
	}, nil
}

// Run peers until ctx is done.
func (m *Mesh) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup

	if m.cfg.Listen != "" {
		listener, err := channel.Listen(
			channel.Config{Keys: m.cfg.Keys, NodeID: m.nodeID}, m.cfg.Listen)
		if err != nil {
			return err
		}
		defer func() { _ = listener.Close() }() // released with Run

		m.log.Info("mesh listening", "addr", listener.Addr().String(), "node", m.nodeID)
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.accept(ctx, listener)
		}()
	}

	if m.cfg.Discovery {
		b, err := beacon.New(beacon.Config{
			Keys:              m.cfg.Keys,
			NodeID:            m.nodeID,
			SyncPort:          m.listenPort(),
			ExcludeInterfaces: m.cfg.ExcludeInterfaces,
			Logger:            m.log,
		})
		if err != nil {
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := b.Run(ctx, m.discovered); err != nil {
				m.log.Warn("beacon stopped", "error", err)
			}
		}()
	}

	if err := m.seedStaticPeers(ctx); err != nil {
		m.log.Warn("record static peers", "error", err)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		m.dialLoop(ctx)
	}()

	<-ctx.Done()
	cancel()
	wg.Wait()
	return nil
}

// listenPort extracts the port peers should use to reach us.
func (m *Mesh) listenPort() int {
	if m.cfg.Listen == "" {
		return 0
	}
	for i := len(m.cfg.Listen) - 1; i >= 0; i-- {
		if m.cfg.Listen[i] == ':' {
			if p, err := strconv.Atoi(m.cfg.Listen[i+1:]); err == nil {
				return p
			}
			return 0
		}
	}
	return 0
}

// seedStaticPeers records configured addresses so they survive restarts and
// participate in the ordinary dial sweep.
func (m *Mesh) seedStaticPeers(ctx context.Context) error {
	for _, addr := range m.cfg.StaticPeers {
		// A configured peer's node id is unknown until it answers, so the
		// address doubles as its provisional identity.
		if err := m.cfg.Store.SavePeer(ctx, store.Peer{
			ID: "static:" + addr, Addrs: []string{addr}, Static: true,
		}); err != nil {
			return err
		}
	}
	return nil
}

// discovered records a peer seen on the network.
func (m *Mesh) discovered(p beacon.Peer) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.cfg.Store.SavePeer(ctx, store.Peer{ID: p.NodeID, Addrs: p.Addrs}); err != nil {
		m.log.Debug("record discovered peer", "peer", p.NodeID, "error", err)
	}
}

// accept converges with every peer that connects to us.
func (m *Mesh) accept(ctx context.Context, l *channel.Listener) {
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		conn, err := l.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.log.Debug("accept", "error", err)
			continue
		}
		go m.exchange(ctx, conn, "inbound")
	}
}

// dialLoop sweeps known peers on an interval.
func (m *Mesh) dialLoop(ctx context.Context) {
	ticker := time.NewTicker(dialInterval)
	defer ticker.Stop()
	for {
		m.sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sweep dials every peer that is not in backoff.
func (m *Mesh) sweep(ctx context.Context) {
	peers, err := m.cfg.Store.Peers(ctx)
	if err != nil {
		m.log.Warn("read peers", "error", err)
		return
	}
	for _, p := range peers {
		if p.ID == m.nodeID {
			continue
		}
		for _, addr := range p.Addrs {
			if m.inBackoff(addr) {
				continue
			}
			if m.dialAndSync(ctx, addr) {
				// One working address per peer is enough for this sweep.
				break
			}
		}
	}
}

// dialAndSync converges with one address, reporting whether it worked.
func (m *Mesh) dialAndSync(ctx context.Context, addr string) bool {
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	conn, err := m.dialer.Dial(dialCtx, addr)
	if err != nil {
		m.penalize(addr)
		m.log.Debug("dial peer", "addr", addr, "error", err)
		return false
	}
	m.clearBackoff(addr)
	m.exchange(dialCtx, conn, addr)
	return true
}

// exchange converges over an established connection and records the peer.
func (m *Mesh) exchange(ctx context.Context, conn *channel.Conn, label string) {
	defer func() { _ = conn.Close() }() // one exchange per connection

	if conn.PeerNodeID == m.nodeID {
		// Ourselves, reached through a loopback route or a duplicated
		// identity. Either way there is nothing to exchange.
		m.log.Warn("peer reports our own node id; check for a cloned database",
			"addr", label)
		return
	}

	stats, err := m.syncer.Exchange(ctx, conn, conn.PeerNodeID)
	if err != nil {
		m.log.Warn("exchange failed", "peer", conn.PeerNodeID, "via", label, "error", err)
		return
	}
	if stats.Sent > 0 || stats.Stored > 0 || stats.Forked > 0 {
		m.log.Info("converged with peer",
			"peer", conn.PeerNodeID, "sent", stats.Sent,
			"stored", stats.Stored, "duplicates", stats.Duplicates, "forked", stats.Forked)
	}

	// Record the peer under its real identity now that it has proven one.
	if host := remoteHost(conn); host != "" {
		saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.cfg.Store.SavePeer(saveCtx, store.Peer{
			ID: conn.PeerNodeID, Addrs: []string{host},
		}); err != nil {
			m.log.Debug("record peer", "peer", conn.PeerNodeID, "error", err)
		}
	}
}

// remoteHost renders the peer's address, if there is one.
func remoteHost(conn *channel.Conn) string {
	if conn.RemoteAddr() == nil {
		return ""
	}
	return conn.RemoteAddr().String()
}

// penalize backs an address off after a failure, with jitter so a fleet that
// restarts together does not retry in lockstep.
func (m *Mesh) penalize(addr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	wait := dialInterval
	if until, ok := m.backoff[addr]; ok {
		if remaining := time.Until(until); remaining > 0 {
			wait = remaining * 2
		}
	}
	wait = min(wait, maxBackoff)
	jitter := time.Duration(rand.Int64N(int64(wait / 4)))
	m.backoff[addr] = time.Now().Add(wait + jitter)
}

func (m *Mesh) clearBackoff(addr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.backoff, addr)
}

func (m *Mesh) inBackoff(addr string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	until, ok := m.backoff[addr]
	return ok && time.Now().Before(until)
}

// KnownPeers reports what this node knows, for `status`.
func (m *Mesh) KnownPeers(ctx context.Context) ([]store.Peer, error) {
	peers, err := m.cfg.Store.Peers(ctx)
	if err != nil {
		return nil, fmt.Errorf("mesh: read peers: %w", err)
	}
	return slices.DeleteFunc(peers, func(p store.Peer) bool { return p.ID == m.nodeID }), nil
}
