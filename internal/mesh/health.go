package mesh

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/christianparpart/agentic-stats/internal/store"
)

// StaleAfter is how long a peer may go without converging before it is worth
// mentioning.
//
// Comfortably more than the dial interval and the longest backoff together, so
// a peer that is merely being retried patiently does not read as a problem. A
// machine that is switched off is not a fault -- it is a laptop in a bag -- so
// the report says how long it has been, and lets the reader decide.
const StaleAfter = time.Hour

// Reach describes how current a peer is.
//
// A named type rather than a pair of booleans: "converged recently", "converged
// a while ago" and "cannot be reached at all" are three states of one thing,
// and flags would let them contradict each other.
type Reach uint8

const (
	// ReachUnknown is a peer this node has never converged with. The zero
	// value, because it is what a freshly discovered peer is.
	ReachUnknown Reach = iota
	// ReachCurrent converged inside StaleAfter.
	ReachCurrent
	// ReachStale converged, but not lately.
	ReachStale
	// ReachFailing converged at some point; the most recent attempt did not.
	ReachFailing
)

// String implements fmt.Stringer.
func (r Reach) String() string {
	switch r {
	case ReachCurrent:
		return "current"
	case ReachStale:
		return "stale"
	case ReachFailing:
		return "failing"
	default:
		return "never converged"
	}
}

// MarshalJSON renders the name rather than the ordinal, so the wire form
// survives a reordering of the constants.
func (r Reach) MarshalJSON() ([]byte, error) { return json.Marshal(r.String()) }

// Address is one way to reach a peer, with the name it answers to.
//
// A pair rather than two parallel lists, so an address can never be shown
// under the wrong name.
type Address struct {
	Addr string `json:"addr"`
	// Name is the reverse-DNS name, empty when the address has none or none
	// has been looked up yet.
	//
	// Never resolved while a report is being rendered: see internal/hostnames
	// for why an archive report must not wait on DNS.
	Name string `json:"name,omitempty"`
}

// String renders the name with the address behind it, or just the address.
func (a Address) String() string {
	if a.Name == "" {
		return a.Addr
	}
	return a.Name + " (" + a.Addr + ")"
}

// PeerHealth is one peer's convergence state.
type PeerHealth struct {
	ID       string    `json:"id"`
	Addrs    []Address `json:"addrs"`
	Static   bool      `json:"static"`
	Reach    Reach     `json:"reach"`
	LastSeen string    `json:"last_seen,omitempty"`
	// LastConverged is when data last actually moved, empty if it never has.
	LastConverged string `json:"last_converged,omitempty"`
	// SinceConverged is that gap in seconds, for a reader that would rather
	// not parse timestamps.
	SinceConverged int64 `json:"since_converged_seconds,omitempty"`
	// LastError is why the most recent attempt failed, if it did.
	LastError string `json:"last_error,omitempty"`
	// Behind is how many records this node lacks that the peer said it held.
	//
	// Measured against the peer's announcement at the last exchange, so it is
	// a snapshot and not live: a peer that has been unreachable for a day may
	// have moved on considerably since. Zero means in step as of then.
	Behind int64 `json:"behind"`
	// Ahead is how many this node holds that the peer had not seen.
	Ahead int64 `json:"ahead"`
}

// Health is the node's own view of whether the mesh is working.
type Health struct {
	NodeID string       `json:"node_id"`
	Peers  []PeerHealth `json:"peers"`
	// Quarantined is the number of records held as fork evidence. Any at all
	// is worth investigating: it means two machines share an origin id.
	Quarantined int64 `json:"quarantined"`
}

// Health reports convergence with every known peer.
//
// It answers the question the log cannot: not "did an exchange fail" but "is
// this archive actually replicated". A peer announcing itself every thirty
// seconds while every exchange fails looks healthy by every other measure.
func (m *Mesh) Health(ctx context.Context) (Health, error) {
	peers, err := m.KnownPeers(ctx)
	if err != nil {
		return Health{}, err
	}
	mine, err := m.cfg.Store.Vector(ctx)
	if err != nil {
		return Health{}, fmt.Errorf("mesh: read version vector: %w", err)
	}
	quarantined, err := m.cfg.Store.Quarantined(ctx)
	if err != nil {
		return Health{}, fmt.Errorf("mesh: count quarantined: %w", err)
	}
	// Read, never resolved. The daemon looks these up in the background
	// precisely so that this call cannot wait on a resolver.
	names, err := m.cfg.Store.Hostnames(ctx)
	if err != nil {
		return Health{}, fmt.Errorf("mesh: read hostnames: %w", err)
	}

	now := time.Now().UTC()
	out := Health{NodeID: m.nodeID, Peers: make([]PeerHealth, 0, len(peers)), Quarantined: quarantined}
	for _, p := range peers {
		out.Peers = append(out.Peers, peerHealth(p, mine, names, now))
	}
	return out, nil
}

// peerHealth classifies one peer.
func peerHealth(p store.Peer, mine store.VersionVector,
	names map[string]store.Hostname, now time.Time,
) PeerHealth {
	h := PeerHealth{
		ID:            p.ID,
		Addrs:         addresses(p.Addrs, names),
		Static:        p.Static,
		LastSeen:      p.LastSeen,
		LastConverged: p.LastConverged,
		LastError:     p.LastError,
	}

	switch {
	case p.LastConverged == "":
		h.Reach = ReachUnknown
	case p.LastError != "":
		// It has worked before and is not working now, which is a more useful
		// thing to say than either half alone.
		h.Reach = ReachFailing
	default:
		h.Reach = ReachCurrent
	}

	if p.LastConverged != "" {
		if at, err := time.Parse(time.RFC3339Nano, p.LastConverged); err == nil {
			since := now.Sub(at)
			if since < 0 {
				// A peer's clock, or ours, has moved. Not worth reporting as a
				// negative age.
				since = 0
			}
			h.SinceConverged = int64(since.Seconds())
			if since > StaleAfter && h.Reach == ReachCurrent {
				h.Reach = ReachStale
			}
		}
	}

	h.Behind, h.Ahead = lag(mine, p.LastVector)
	return h
}

// addresses pairs each address with whatever name is on record for it.
//
// Always a list, never nil: an absent field and an empty one mean the same
// thing to a reader and different things to a JSON parser.
func addresses(addrs []string, names map[string]store.Hostname) []Address {
	out := make([]Address, 0, len(addrs))
	for _, addr := range addrs {
		a := Address{Addr: addr}
		host := addr
		if h, _, err := net.SplitHostPort(addr); err == nil {
			host = h
		}
		a.Name = names[host].Name
		out = append(out, a)
	}
	return out
}

// lag compares what we hold against what the peer last said it held.
//
// Counted per origin and summed, because a version vector is dense: the
// difference between two watermarks for one origin is exactly the number of
// records between them, with no gaps to account for.
func lag(mine store.VersionVector, peerVector string) (behind, ahead int64) {
	if peerVector == "" {
		return 0, 0
	}
	var theirs store.VersionVector
	if err := json.Unmarshal([]byte(peerVector), &theirs); err != nil {
		// A vector we cannot read is not worth failing the whole report over.
		return 0, 0
	}
	for origin, theirSeq := range theirs {
		if d := theirSeq - mine[origin]; d > 0 {
			behind += d
		}
	}
	for origin, mySeq := range mine {
		if d := mySeq - theirs[origin]; d > 0 {
			ahead += d
		}
	}
	return behind, ahead
}
