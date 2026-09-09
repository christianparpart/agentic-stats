// Package beacon announces this node on every local network and listens for
// others doing the same.
//
// This is a hand-rolled multicast beacon rather than mDNS. Nothing else on the
// network needs to browse for us, so DNS-SD's probing, conflict resolution and
// TXT conventions are most of its complexity and none of our requirement --
// and the maintained Go DNS-SD libraries either take a single interface and
// never re-join, or have not been touched in years.
//
// A beacon authorises nothing. It is a reachability hint: acting on a replayed
// one costs an attacker-triggered TCP connection that then fails the real
// handshake. That is what makes a generous clock-skew window safe.
package beacon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"time"

	"golang.org/x/net/ipv4"

	"github.com/christianparpart/agentic-stats/internal/seal"
)

// Group and Port are the multicast rendezvous.
//
// 239.255.0.0/16 is the IPv4 administratively-scoped block. Notably not
// 224.0.0.0/24, which IANA reserves for link-local control protocols.
var Group = net.IPv4(239, 255, 42, 7)

// Port is the beacon port.
const Port = 45897

// Interval is how often a node announces itself.
const Interval = 30 * time.Second

// EpochSeconds is the beacon's time granularity.
//
// Five minutes, accepting one epoch either side, tolerates roughly ten minutes
// of clock skew. That matters more than it sounds: laptops resume from sleep
// and VMs are restored from snapshots, and a thirty-second window would make
// discovery fail in exactly those cases. The wider replay window is harmless
// because a beacon authorises nothing.
const EpochSeconds = 300

// interfaceScanInterval is how often the interface set is re-examined.
//
// There is no portable API for interface-change notification, so this polls.
// A VPN coming up or a laptop waking must re-join the group or the node goes
// quietly deaf.
const interfaceScanInterval = 10 * time.Second

// maxDatagram bounds a received beacon.
const maxDatagram = 2048

// packet is what travels on the wire.
type packet struct {
	Epoch  int64    `json:"e"`
	Nonce  []byte   `json:"n"`
	NodeID string   `json:"i"`
	Port   int      `json:"p"`
	Addrs  []string `json:"a"`
	Tag    []byte   `json:"t"`
}

// Peer is a node seen on the network.
type Peer struct {
	NodeID string
	Addrs  []string
}

// Config is everything the beacon needs.
type Config struct {
	// Keys authenticate beacons. Required.
	Keys *seal.Keys
	// NodeID is announced so peers can recognise us. Required.
	NodeID string
	// SyncPort is where this node accepts peer connections.
	SyncPort int
	// ExcludeInterfaces are names to keep beacons off, such as VM bridges.
	ExcludeInterfaces []string
	// Logger receives anomalies. Zero discards them.
	Logger *slog.Logger
	// Now supplies time. Zero uses the system clock.
	Now func() time.Time
}

// Beacon announces and discovers.
type Beacon struct {
	cfg Config
	log *slog.Logger
	now func() time.Time
}

// New returns a Beacon.
func New(cfg Config) (*Beacon, error) {
	if cfg.Keys == nil {
		return nil, errors.New("beacon: Config.Keys is required")
	}
	if cfg.NodeID == "" {
		return nil, errors.New("beacon: Config.NodeID is required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Beacon{cfg: cfg, log: log, now: now}, nil
}

// Run announces and listens until ctx is done, reporting peers to found.
func (b *Beacon) Run(ctx context.Context, found func(Peer)) error {
	lc := net.ListenConfig{Control: reuseControl}
	conn, err := lc.ListenPacket(ctx, "udp4", fmt.Sprintf("0.0.0.0:%d", Port))
	if err != nil {
		return fmt.Errorf("beacon: listen: %w", err)
	}
	defer func() { _ = conn.Close() }() // released with the run loop

	pc := ipv4.NewPacketConn(conn)
	// TTL 1 so a beacon never leaves the link it was sent on.
	if err := pc.SetMulticastTTL(1); err != nil {
		b.log.Warn("set multicast ttl", "error", err)
	}
	// Loopback stays ON. Our own beacons are already dropped by node id, and
	// disabling it would also stop two nodes on one host from seeing each
	// other -- a machine and the VM running on it, which is a real deployment
	// and not merely a test arrangement.
	if err := pc.SetMulticastLoopback(true); err != nil {
		b.log.Debug("set multicast loopback", "error", err)
	}

	joined := b.rejoin(pc, nil)

	go b.listen(ctx, pc, found)

	announce := time.NewTicker(Interval)
	defer announce.Stop()
	scan := time.NewTicker(interfaceScanInterval)
	defer scan.Stop()

	b.announce(pc, joined)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-announce.C:
			b.announce(pc, joined)
		case <-scan.C:
			// The interface set changes on sleep/wake, VPN up/down and DHCP.
			joined = b.rejoin(pc, joined)
		}
	}
}

// rejoin joins the group on every eligible interface, leaving those that went
// away. It returns the interfaces currently joined.
func (b *Beacon) rejoin(pc *ipv4.PacketConn, current []net.Interface) []net.Interface {
	want := b.eligible()
	group := &net.UDPAddr{IP: Group}

	for _, ifi := range current {
		if !containsInterface(want, ifi) {
			_ = pc.LeaveGroup(&ifi, group)
		}
	}
	var joined []net.Interface
	for _, ifi := range want {
		if containsInterface(current, ifi) {
			joined = append(joined, ifi)
			continue
		}
		if err := pc.JoinGroup(&ifi, group); err != nil {
			// Common and benign: an interface may not carry multicast even
			// when it claims to.
			b.log.Debug("join group", "interface", ifi.Name, "error", err)
			continue
		}
		joined = append(joined, ifi)
	}
	return joined
}

// eligible returns the interfaces worth announcing on.
func (b *Beacon) eligible() []net.Interface {
	all, err := net.Interfaces()
	if err != nil {
		b.log.Warn("enumerate interfaces", "error", err)
		return nil
	}
	var out []net.Interface
	for _, ifi := range all {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		if ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		// A point-to-point link is not a broadcast domain, which is exactly
		// why multicast cannot cross a WireGuard or Tailscale tunnel. Peers
		// over those are reached from the configured peer list instead.
		if ifi.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		if slices.Contains(b.cfg.ExcludeInterfaces, ifi.Name) {
			continue
		}
		out = append(out, ifi)
	}
	return out
}

func containsInterface(list []net.Interface, want net.Interface) bool {
	return slices.ContainsFunc(list, func(i net.Interface) bool { return i.Index == want.Index })
}

// announce sends one beacon on each joined interface.
func (b *Beacon) announce(pc *ipv4.PacketConn, ifaces []net.Interface) {
	addrs := b.localAddrs()
	if len(addrs) == 0 {
		return
	}
	pkt, err := b.build(addrs)
	if err != nil {
		b.log.Warn("build beacon", "error", err)
		return
	}
	body, err := json.Marshal(pkt)
	if err != nil {
		b.log.Warn("encode beacon", "error", err)
		return
	}
	dst := &net.UDPAddr{IP: Group, Port: Port}
	for _, ifi := range ifaces {
		if err := pc.SetMulticastInterface(&ifi); err != nil {
			continue
		}
		if _, err := pc.WriteTo(body, nil, dst); err != nil {
			b.log.Debug("send beacon", "interface", ifi.Name, "error", err)
		}
	}
}

// build assembles an authenticated beacon for the current epoch.
func (b *Beacon) build(addrs []string) (packet, error) {
	nonce := make([]byte, 16)
	if _, err := randRead(nonce); err != nil {
		return packet{}, err
	}
	epoch := b.now().Unix() / EpochSeconds
	p := packet{
		Epoch:  epoch,
		Nonce:  nonce,
		NodeID: b.cfg.NodeID,
		Port:   b.cfg.SyncPort,
		Addrs:  addrs,
	}
	p.Tag = b.cfg.Keys.BeaconTag(epoch, nonce, p.NodeID, joinAddrs(addrs))
	return p, nil
}

// listen reads beacons and reports the authentic ones.
func (b *Beacon) listen(ctx context.Context, pc *ipv4.PacketConn, found func(Peer)) {
	buf := make([]byte, maxDatagram)
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		if err := pc.SetReadDeadline(b.now().Add(time.Second)); err != nil {
			return
		}
		n, _, src, err := pc.ReadFrom(buf)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue
			}
			return
		}
		peer, ok := b.verify(buf[:n], src)
		if !ok {
			continue
		}
		if peer.NodeID == b.cfg.NodeID {
			continue
		}
		found(peer)
	}
}

// verify authenticates a received beacon.
//
// All three candidate epochs are always computed, so the time taken does not
// reveal which one matched.
func (b *Beacon) verify(body []byte, src net.Addr) (Peer, bool) {
	var p packet
	if err := json.Unmarshal(body, &p); err != nil {
		return Peer{}, false
	}
	now := b.now().Unix() / EpochSeconds
	ok := false
	for _, epoch := range []int64{now - 1, now, now + 1} {
		want := b.cfg.Keys.BeaconTag(epoch, p.Nonce, p.NodeID, joinAddrs(p.Addrs))
		if seal.Equal(p.Tag, want) && p.Epoch == epoch {
			ok = true
		}
	}
	if !ok || p.NodeID == "" || p.Port <= 0 {
		return Peer{}, false
	}

	// Prefer the addresses the sender advertised, but always include the one
	// it actually came from: a node behind NAT or with a stale advertisement
	// is still reachable at its observed address.
	addrs := make([]string, 0, len(p.Addrs)+1)
	for _, a := range p.Addrs {
		addrs = append(addrs, net.JoinHostPort(a, strconv.Itoa(p.Port)))
	}
	if host, _, err := net.SplitHostPort(src.String()); err == nil {
		observed := net.JoinHostPort(host, strconv.Itoa(p.Port))
		if !slices.Contains(addrs, observed) {
			addrs = append(addrs, observed)
		}
	}
	return Peer{NodeID: p.NodeID, Addrs: addrs}, true
}

// localAddrs returns this machine's routable unicast addresses.
func (b *Beacon) localAddrs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
			continue
		}
		out = append(out, ipnet.IP.String())
	}
	slices.Sort(out)
	return out
}

// joinAddrs renders addresses for the tag, so a forged address list cannot
// reuse a captured tag.
func joinAddrs(addrs []string) string {
	sorted := slices.Clone(addrs)
	slices.Sort(sorted)
	out := ""
	for _, a := range sorted {
		out += a + ";"
	}
	return out
}
