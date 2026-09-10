// Package hostnames keeps the peer address-to-name cache filled.
//
// A reverse lookup is a round trip to a resolver that may itself be gone. On a
// laptop that has just left a VPN, or a VM whose network came back on a
// different bridge, reliably so -- and that is exactly when someone runs
// `status` to find out what is wrong. Doing the lookup there would make a
// report about the archive wait on DNS, and time out with it.
//
// So the daemon looks addresses up as it learns them and writes the answers
// down. Every reader takes what is written and never asks. A name that is
// missing is missing; it is not worth a report that hangs.
package hostnames

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/christianparpart/agentic-stats/internal/store"
)

// DefaultTTL is how long a name is trusted before it is looked up again.
//
// Long, because the answer is a machine's name and machines are not renamed
// often. The cost of being stale for half a day is a label; the cost of
// re-asking constantly is traffic on a resolver that is sometimes not there.
const DefaultTTL = 12 * time.Hour

// DefaultMissTTL is how long "this address has no name" is trusted.
//
// Shorter than DefaultTTL because the likelier change is a machine gaining a
// record rather than losing one -- a peer that joins before its DHCP lease
// registers is the ordinary case -- and because retrying a handful of unnamed
// addresses hourly costs nothing.
const DefaultMissTTL = time.Hour

// DefaultTimeout bounds one lookup.
//
// Short on purpose. Nothing waits on this, so a resolver that is merely slow
// should be abandoned and retried next sweep rather than held onto.
const DefaultTimeout = 2 * time.Second

// DefaultInterval is how often the peer list is swept for addresses to name.
//
// A sweep with nothing due makes no queries at all, so this is chosen for how
// soon a newly discovered peer gets a name rather than for what it costs.
const DefaultInterval = time.Minute

// maxParallel bounds concurrent lookups.
//
// A mesh is a handful of machines, and a resolver that has gone away should be
// found out by one small batch of timeouts rather than by many at once.
const maxParallel = 4

// Lookup is the reverse DNS this needs. *net.Resolver satisfies it.
type Lookup interface {
	LookupAddr(ctx context.Context, addr string) ([]string, error)
}

// Store is the persistence this needs.
type Store interface {
	Peers(ctx context.Context) ([]store.Peer, error)
	Hostnames(ctx context.Context) (map[string]store.Hostname, error)
	SaveHostname(ctx context.Context, ip, name string) error
}

// Config is everything a Resolver needs.
type Config struct {
	// Store holds the peers to name and the answers. Required.
	Store Store
	// Lookup performs the reverse lookup. Zero uses net.DefaultResolver.
	Lookup Lookup
	// Logger reports failures. Zero discards them.
	Logger *slog.Logger
	// Clock supplies the time answers are stamped against. Zero uses time.Now.
	Clock func() time.Time

	// TTL, MissTTL, Timeout and Interval override the package defaults. Zero
	// uses them.
	TTL      time.Duration
	MissTTL  time.Duration
	Timeout  time.Duration
	Interval time.Duration

	// Tick replaces the internal ticker when set, so a test can drive sweeps
	// deterministically instead of waiting for one.
	Tick <-chan time.Time
}

// Resolver fills the cache of peer address names.
type Resolver struct {
	store    Store
	lookup   Lookup
	log      *slog.Logger
	now      func() time.Time
	ttl      time.Duration
	missTTL  time.Duration
	timeout  time.Duration
	interval time.Duration
	tick     <-chan time.Time
}

// New returns a Resolver ready to run.
func New(cfg Config) (*Resolver, error) {
	if cfg.Store == nil {
		return nil, errors.New("hostnames: Config.Store is required")
	}
	r := &Resolver{
		store:    cfg.Store,
		lookup:   cfg.Lookup,
		log:      cfg.Logger,
		now:      cfg.Clock,
		ttl:      cfg.TTL,
		missTTL:  cfg.MissTTL,
		timeout:  cfg.Timeout,
		interval: cfg.Interval,
		tick:     cfg.Tick,
	}
	if r.lookup == nil {
		r.lookup = net.DefaultResolver
	}
	if r.log == nil {
		r.log = slog.New(slog.DiscardHandler)
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.ttl <= 0 {
		r.ttl = DefaultTTL
	}
	if r.missTTL <= 0 {
		r.missTTL = DefaultMissTTL
	}
	if r.timeout <= 0 {
		r.timeout = DefaultTimeout
	}
	if r.interval <= 0 {
		r.interval = DefaultInterval
	}
	return r, nil
}

// Run sweeps until ctx is done.
//
// The first sweep happens immediately rather than after one interval, because
// the addresses worth naming are usually already in the store from the last
// time this node ran, and a name that appears a minute after start is a name
// missing from the first `status` someone types.
func (r *Resolver) Run(ctx context.Context) error {
	tick := r.tick
	if tick == nil {
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		tick = ticker.C
	}
	for {
		if err := r.Sweep(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Naming peers is a convenience. Losing it must not take down the
			// subsystem that is supervising this, and the next sweep will try
			// again in any case.
			r.log.Warn("could not refresh peer hostnames", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
		}
	}
}

// Sweep looks up every peer address whose name is missing or due again.
func (r *Resolver) Sweep(ctx context.Context) error {
	peers, err := r.store.Peers(ctx)
	if err != nil {
		return fmt.Errorf("hostnames: read peers: %w", err)
	}
	known, err := r.store.Hostnames(ctx)
	if err != nil {
		return fmt.Errorf("hostnames: read cache: %w", err)
	}

	due := r.due(peers, known)
	if len(due) == 0 {
		return nil
	}

	var (
		wg   sync.WaitGroup
		gate = make(chan struct{}, maxParallel)
		mu   sync.Mutex
		errs []error
	)
	for _, ip := range due {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case gate <- struct{}{}:
				defer func() { <-gate }()
			case <-ctx.Done():
				return
			}
			if err := r.resolveOne(ctx, ip); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// resolveOne looks one address up and records what came back, including
// nothing.
func (r *Resolver) resolveOne(ctx context.Context, ip string) error {
	lookupCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	name := ""
	names, err := r.lookup.LookupAddr(lookupCtx, ip)
	switch {
	case err != nil && ctx.Err() != nil:
		// Shutdown, not a failed lookup. Recording an empty name here would
		// cache "no name" for an address nobody asked about yet.
		return nil
	case err != nil:
		// An address with no PTR record is reported as an error by every
		// resolver, and it is the common case rather than a fault: record it
		// as "no name" so the next sweep does not ask again. A resolver that
		// is genuinely down is indistinguishable from here, and costs at
		// worst one MissTTL of a missing label.
		r.log.Debug("no name for peer address", "address", ip, "error", err)
	case len(names) > 0:
		name = strings.TrimSuffix(names[0], ".")
	}

	if err := r.store.SaveHostname(ctx, ip, name); err != nil {
		return fmt.Errorf("hostnames: save %s: %w", ip, err)
	}
	return nil
}

// due returns the addresses worth looking up now, deduplicated.
func (r *Resolver) due(peers []store.Peer, known map[string]store.Hostname) []string {
	now := r.now().UTC()
	seen := make(map[string]struct{})
	var out []string

	for _, p := range peers {
		for _, addr := range p.Addrs {
			ip, ok := address(addr)
			if !ok {
				continue
			}
			if _, dup := seen[ip]; dup {
				continue
			}
			seen[ip] = struct{}{}
			if r.fresh(known[ip], now) {
				continue
			}
			out = append(out, ip)
		}
	}
	return out
}

// fresh reports whether a cached answer still stands.
func (r *Resolver) fresh(h store.Hostname, now time.Time) bool {
	if h.ResolvedAt == "" {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, h.ResolvedAt)
	if err != nil {
		// An unparseable timestamp is a row worth replacing.
		return false
	}
	ttl := r.ttl
	if h.Name == "" {
		ttl = r.missTTL
	}
	return now.Sub(at) < ttl
}

// address extracts the IP worth a reverse lookup from a peer address.
//
// A configured peer given by name is skipped: it already carries the answer,
// and asking DNS to name the address behind a name the operator chose would
// replace what they wrote with whatever the network says.
func address(addr string) (string, bool) {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if host == "" || net.ParseIP(host) == nil {
		return "", false
	}
	return host, true
}
