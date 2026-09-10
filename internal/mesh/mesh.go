// Package mesh runs a node's peering: it announces, discovers, dials, accepts,
// and converges with everyone holding the same pre-shared key.
//
// There is no leader and no membership protocol. A peer is anything that can
// complete the authenticated handshake, and every exchange is symmetric.
package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/christianparpart/agentic-stats/internal/mesh/beacon"
	"github.com/christianparpart/agentic-stats/internal/mesh/channel"
	meshsync "github.com/christianparpart/agentic-stats/internal/mesh/sync"
	"github.com/christianparpart/agentic-stats/internal/seal"
	"github.com/christianparpart/agentic-stats/internal/store"
	"github.com/christianparpart/agentic-stats/internal/supervise"
)

// dialInterval is how often the node sweeps its known peers.
const dialInterval = 60 * time.Second

// maxBackoff caps the wait after repeated failures. A peer may be off for a
// week; there is no point retrying it every minute forever, and no point
// giving up on it either.
const maxBackoff = 15 * time.Minute

// dialTimeout bounds one peer exchange.
const dialTimeout = 5 * time.Minute

// exchangeTimeout bounds an exchange a peer opened against us.
//
// Inbound previously carried the daemon's own lifetime as its context, so a
// peer that connected and then stalled held a goroutine and a connection until
// the process exited. Outbound had dialTimeout; this is its counterpart. It is
// generous because a first backfill between two long-lived nodes is genuinely
// large, and the idle deadline inside the connection is what catches a peer
// that has stopped making progress.
const exchangeTimeout = 30 * time.Minute

// maxInboundExchanges caps concurrent inbound convergence.
//
// A fleet is a handful of machines, so this is far above any legitimate load;
// it exists so that a peer opening connections in a loop costs a bounded amount
// of memory and database contention rather than an unbounded amount.
const maxInboundExchanges = 8

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
	// Hostname is what this node calls itself, announced to peers so they can
	// label it without a reverse lookup. Empty announces nothing.
	//
	// Injected rather than read here: os.Hostname is the environment, and a
	// test must be able to state what this machine is called.
	Hostname string
	// Version is the build this node is running, announced to peers so they
	// can see which of them are behind. Injected for the same reason Hostname
	// is: it is a fact about the build, not something this layer should read.
	Version string
}

// Mesh is a node's peering subsystem.
type Mesh struct {
	cfg    Config
	log    *slog.Logger
	syncer *meshsync.Syncer
	dialer *channel.Dialer
	nodeID string

	// wake asks the dial loop to sweep now. Buffered by one: several wake-ups
	// arriving together mean the same thing as one.
	wake chan struct{}

	mu      sync.Mutex
	backoff map[string]time.Time
	// sighted is the node ids discovery has reported this run, so only a
	// peer's first sighting nudges the dial loop.
	sighted map[string]struct{}
}

// Wake retries every peer immediately, discarding any backoff.
//
// Called when the machine has just resumed. Everything the backoff table
// records was learned before the interruption and none of it is trustworthy
// now: an address that was refusing connections may be up, and the machine may
// have moved to a different network entirely, where the failures it remembers
// were about somewhere else. Waiting out a fifteen-minute backoff to discover
// that is exactly the wrong behaviour on the machine that has been away longest.
//
// Safe to call from any goroutine, and cheap enough to call spuriously.
func (m *Mesh) Wake() {
	m.mu.Lock()
	clear(m.backoff)
	m.mu.Unlock()
	m.nudge()
}

// nudge asks the dial loop to sweep now, leaving backoff intact.
//
// Separate from Wake because the two events mean different things. A resume
// invalidates everything we believed about the network, so backoff goes. A peer
// appearing on the network says nothing about the peers that are failing, and
// clearing their backoff would turn one machine waking up into a retry storm
// against every machine that is switched off.
func (m *Mesh) nudge() {
	select {
	case m.wake <- struct{}{}:
	default: // a sweep is already pending; one is enough
	}
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

	syncer, err := meshsync.New(cfg.Store, meshsync.Config{Log: log, Host: cfg.Hostname, Version: cfg.Version})
	if err != nil {
		return nil, err
	}
	dialer, err := channel.NewDialer(channel.Config{Keys: cfg.Keys, NodeID: nodeID})
	if err != nil {
		return nil, err
	}
	return &Mesh{
		cfg: cfg, log: log, syncer: syncer, dialer: dialer,
		nodeID:  nodeID,
		backoff: make(map[string]time.Time),
		sighted: make(map[string]struct{}),
		wake:    make(chan struct{}, 1),
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
			ID: staticPrefix + addr, Addrs: []string{addr}, Static: true,
		}); err != nil {
			return err
		}
	}
	return nil
}

// discovered records a peer seen on the network.
//
// A peer we have not seen before also wakes the dial loop. Recording it and
// waiting for the next sweep meant a node could discover a neighbour within a
// second and then sit next to it for the best part of a minute before speaking
// to it -- and on a fresh node, which has no stored peers and swept an empty
// list on startup, that was the whole of its first minute.
//
// Only the first sighting nudges. A beacon arrives from every peer every thirty
// seconds, and sweeping on each would be a busy loop wearing a discovery
// protocol as a hat.
func (m *Mesh) discovered(p beacon.Peer) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.cfg.Store.SavePeer(ctx, store.Peer{ID: p.NodeID, Addrs: p.Addrs}); err != nil {
		m.log.Debug("record discovered peer", "peer", p.NodeID, "error", err)
		return
	}
	if m.firstSighting(p.NodeID) {
		m.log.Info("discovered a peer", "peer", p.NodeID, "addrs", p.Addrs)
		m.nudge()
	}
}

// firstSighting reports whether this is the first time we have seen a node id
// since this process started, recording it either way.
func (m *Mesh) firstSighting(nodeID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, known := m.sighted[nodeID]; known {
		return false
	}
	m.sighted[nodeID] = struct{}{}
	return true
}

// accept converges with every peer that connects to us.
func (m *Mesh) accept(ctx context.Context, l *channel.Listener) {
	inFlight := make(chan struct{}, maxInboundExchanges)
	var wg sync.WaitGroup
	// Inbound exchanges borrow the accept loop's lifetime, so they must finish
	// before it returns or they would write to a closing store.
	defer wg.Wait()

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
		select {
		case inFlight <- struct{}{}:
		default:
			// Already converging with as many peers as we are willing to. The
			// peer will try again on its own sweep; refusing now is cheaper for
			// both of us than queueing.
			m.log.Warn("refusing inbound exchange; already at capacity",
				"peer", conn.PeerNodeID, "limit", maxInboundExchanges)
			_ = conn.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-inFlight }()
			exCtx, cancel := context.WithTimeout(ctx, exchangeTimeout)
			defer cancel()
			m.exchange(exCtx, conn, "inbound")
		}()
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
		case <-m.wake:
			// A resume, or anything else that invalidated what we believed
			// about the network. Sweep now rather than at the next tick.
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
	// This is where bytes a peer chose meet code that parses them, so it is
	// where a malformed frame would panic. Contained here rather than left to
	// the supervisor because restarting the whole mesh would drop every other
	// peer's exchange over one peer's bad frame.
	defer supervise.Recover(m.log, "peer exchange with "+label)
	defer func() { _ = conn.Close() }() // one exchange per connection

	// Without this, cancelling ctx does not reach a read already blocked in the
	// kernel, and the timeouts above would bound nothing.
	defer conn.Guard(ctx)()

	if conn.PeerNodeID == m.nodeID {
		// Ourselves, reached through a loopback route or a duplicated
		// identity. Either way there is nothing to exchange.
		m.log.Warn("peer reports our own node id; check for a cloned database",
			"addr", label)
		return
	}

	stats, err := m.syncer.Exchange(ctx, conn, conn.PeerNodeID)
	m.recordOutcome(conn.PeerNodeID, stats.PeerVector, err)
	// Before the error check: the name rides on the first frame, so a peer
	// that announced itself and then failed mid-transfer has still told us
	// what it is called, and a report about a failing peer is exactly where
	// its name is most wanted.
	m.recordPeerHost(conn.PeerNodeID, stats.PeerHost)
	m.recordPeerVersion(conn.PeerNodeID, stats.PeerVersion)
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

// recordOutcome persists whether this exchange actually converged.
//
// Written on failure as well as success. A peer that announces itself on the
// network every thirty seconds while every exchange with it fails would
// otherwise show up as recently seen and perfectly healthy, which is the
// failure mode most worth catching: the mesh looks fine and is not replicating.
//
// Its own context, because the exchange context is very often already cancelled
// by the time we get here -- that being why the exchange failed -- and the
// record of the failure is the thing we least want to lose to it.
func (m *Mesh) recordOutcome(peerID string, vector store.VersionVector, cause error) {
	if peerID == "" {
		return
	}
	encoded := ""
	if len(vector) > 0 {
		if b, err := json.Marshal(vector); err == nil {
			encoded = string(b)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.cfg.Store.RecordConvergence(ctx, peerID, encoded, cause); err != nil {
		m.log.Debug("record convergence", "peer", peerID, "error", err)
	}
}

// recordPeerHost stores the name a peer reported for itself.
//
// Its own context for the same reason recordOutcome has one: the exchange
// context is very often already cancelled by the time we get here.
func (m *Mesh) recordPeerHost(peerID, host string) {
	if peerID == "" || host == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.cfg.Store.SavePeerHostname(ctx, peerID, host); err != nil {
		m.log.Debug("record peer hostname", "peer", peerID, "error", err)
	}
}

// recordPeerVersion stores the build a peer reported running.
//
// Its own context for the same reason recordPeerHost has one: the exchange
// context is very often already cancelled by the time we get here.
func (m *Mesh) recordPeerVersion(peerID, version string) {
	if peerID == "" || version == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.cfg.Store.SavePeerVersion(ctx, peerID, version); err != nil {
		m.log.Debug("record peer version", "peer", peerID, "error", err)
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

// staticPrefix marks a configured address whose node id is not yet known.
const staticPrefix = "static:"

// KnownPeers reports what this node knows, for `status`.
func (m *Mesh) KnownPeers(ctx context.Context) ([]store.Peer, error) {
	peers, err := m.cfg.Store.Peers(ctx)
	if err != nil {
		return nil, fmt.Errorf("mesh: read peers: %w", err)
	}
	peers = slices.DeleteFunc(peers, func(p store.Peer) bool { return p.ID == m.nodeID })
	return withoutRedundantPlaceholders(peers), nil
}

// withoutRedundantPlaceholders drops a configured address once the machine
// behind it has introduced itself.
//
// A peer listed in mesh.peers is recorded under "static:<addr>" because its
// node id is unknown until it answers. Once it does, it is recorded again under
// its real identity, and the placeholder lingers -- one machine appearing twice,
// the second copy permanently reading as never converged. That is exactly the
// shape of a real problem, so leaving it in a health report would teach the
// reader to ignore the thing the report exists to show them.
//
// The placeholder is kept while the peer has genuinely never answered, because
// then it is the only evidence the address was configured at all.
func withoutRedundantPlaceholders(peers []store.Peer) []store.Peer {
	answered := make(map[string]struct{})
	for _, p := range peers {
		if strings.HasPrefix(p.ID, staticPrefix) {
			continue
		}
		for _, a := range p.Addrs {
			answered[a] = struct{}{}
		}
	}
	return slices.DeleteFunc(peers, func(p store.Peer) bool {
		if !strings.HasPrefix(p.ID, staticPrefix) {
			return false
		}
		// Match on host, since the peer reports the ephemeral port it dialled
		// from rather than the one it listens on.
		for _, a := range p.Addrs {
			if hostsOverlap(a, answered) {
				return true
			}
		}
		return false
	})
}

// hostsOverlap reports whether addr's host has answered on any port.
func hostsOverlap(addr string, answered map[string]struct{}) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	for a := range answered {
		other, _, oerr := net.SplitHostPort(a)
		if oerr != nil {
			other = a
		}
		if other == host {
			return true
		}
	}
	return false
}
