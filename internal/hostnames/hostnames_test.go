package hostnames_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/christianparpart/agentic-stats/internal/hostnames"
	"github.com/christianparpart/agentic-stats/internal/store"
)

// fakeStore is the persistence, in memory.
type fakeStore struct {
	mu     sync.Mutex
	peers  []store.Peer
	names  map[string]store.Hostname
	saved  []string
	now    func() time.Time
	failOn string
}

func newStore(now func() time.Time, peers ...store.Peer) *fakeStore {
	return &fakeStore{peers: peers, names: map[string]store.Hostname{}, now: now}
}

func (f *fakeStore) Peers(context.Context) ([]store.Peer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOn == "peers" {
		return nil, errors.New("peers unavailable")
	}
	return f.peers, nil
}

func (f *fakeStore) Hostnames(context.Context) (map[string]store.Hostname, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]store.Hostname, len(f.names))
	for k, v := range f.names {
		out[k] = v
	}
	return out, nil
}

func (f *fakeStore) SaveHostname(_ context.Context, ip, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = append(f.saved, ip)
	f.names[ip] = store.Hostname{Name: name, ResolvedAt: f.now().Format(time.RFC3339Nano)}
	return nil
}

func (f *fakeStore) name(ip string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.names[ip].Name
}

func (f *fakeStore) lookups() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.saved...)
}

// fakeLookup answers from a table and counts what it was asked.
type fakeLookup struct {
	mu      sync.Mutex
	answers map[string][]string
	fail    error
	asked   []string
	block   chan struct{}
}

func (f *fakeLookup) LookupAddr(ctx context.Context, addr string) ([]string, error) {
	f.mu.Lock()
	f.asked = append(f.asked, addr)
	block, fail := f.block, f.fail
	names, ok := f.answers[addr]
	f.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if fail != nil {
		return nil, fail
	}
	if !ok {
		return nil, errors.New("no such host")
	}
	return names, nil
}

func (f *fakeLookup) askedFor() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func resolver(t *testing.T, cfg hostnames.Config) *hostnames.Resolver {
	t.Helper()
	r, err := hostnames.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// The whole point: a name is on record before anyone asks for it.
func TestASweepNamesEveryPeerAddress(t *testing.T) {
	now := func() time.Time { return at("2026-09-10T10:00:00Z") }
	st := newStore(now,
		store.Peer{ID: "a", Addrs: []string{"192.168.1.10:8844"}},
		store.Peer{ID: "b", Addrs: []string{"192.168.1.11:8844", "10.0.0.5:8844"}},
	)
	look := &fakeLookup{answers: map[string][]string{
		"192.168.1.10": {"nuc.lan."},
		"192.168.1.11": {"laptop.lan."},
		"10.0.0.5":     {"vm.lan."},
	}}

	r := resolver(t, hostnames.Config{Store: st, Lookup: look, Clock: now})
	if err := r.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	want := map[string]string{
		"192.168.1.10": "nuc.lan",
		"192.168.1.11": "laptop.lan",
		"10.0.0.5":     "vm.lan",
	}
	for ip, name := range want {
		// The trailing dot is correct in DNS and wrong in a report.
		if got := st.name(ip); got != name {
			t.Errorf("%s named %q, want %q", ip, got, name)
		}
	}
}

// An address with no PTR record is the common case, not a fault. Recording the
// absence is what stops every sweep asking a resolver the same dead question.
func TestAnAddressWithNoNameIsRecordedAsHavingNone(t *testing.T) {
	now := func() time.Time { return at("2026-09-10T10:00:00Z") }
	st := newStore(now, store.Peer{ID: "a", Addrs: []string{"192.168.1.10:8844"}})
	look := &fakeLookup{answers: map[string][]string{}}

	r := resolver(t, hostnames.Config{Store: st, Lookup: look, Clock: now})
	if err := r.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got := st.name("192.168.1.10"); got != "" {
		t.Errorf("name = %q, want empty", got)
	}
	if len(st.lookups()) != 1 {
		t.Fatalf("recorded %d answers, want 1 — the absence has to be written down", len(st.lookups()))
	}

	// Immediately afterwards it must not be asked again.
	if err := r.Sweep(context.Background()); err != nil {
		t.Fatalf("second Sweep: %v", err)
	}
	if n := len(look.askedFor()); n != 1 {
		t.Errorf("asked the resolver %d times, want 1 — the miss was not cached", n)
	}
}

// Freshness is per answer: a name lasts far longer than the absence of one,
// because a machine gaining a record is the likelier change.
func TestWhatIsLookedUpAgainAndWhen(t *testing.T) {
	tests := []struct {
		name string
		// cached is what is already on record; nil means never looked up.
		cached  *store.Hostname
		elapsed time.Duration
		wantAsk bool
	}{
		{
			name:    "never looked up",
			cached:  nil,
			wantAsk: true,
		},
		{
			name:    "a fresh name is left alone",
			cached:  &store.Hostname{Name: "old.lan"},
			elapsed: hostnames.DefaultTTL / 2,
			wantAsk: false,
		},
		{
			name:    "a stale name is refreshed",
			cached:  &store.Hostname{Name: "old.lan"},
			elapsed: hostnames.DefaultTTL + time.Minute,
			wantAsk: true,
		},
		{
			name:    "a recent miss is not retried",
			cached:  &store.Hostname{},
			elapsed: hostnames.DefaultMissTTL / 2,
			wantAsk: false,
		},
		{
			// The case the shorter miss lifetime exists for: a peer that
			// joined before its lease registered.
			name:    "an old miss is retried",
			cached:  &store.Hostname{},
			elapsed: hostnames.DefaultMissTTL + time.Minute,
			wantAsk: true,
		},
		{
			name:    "an unreadable timestamp is replaced",
			cached:  &store.Hostname{Name: "old.lan", ResolvedAt: "not a time"},
			wantAsk: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := at("2026-09-10T10:00:00Z")
			clock := base.Add(tc.elapsed)
			now := func() time.Time { return clock }

			st := newStore(now, store.Peer{ID: "a", Addrs: []string{"192.168.1.10:8844"}})
			if tc.cached != nil {
				cached := *tc.cached
				if cached.ResolvedAt == "" {
					cached.ResolvedAt = base.Format(time.RFC3339Nano)
				}
				st.names["192.168.1.10"] = cached
			}
			look := &fakeLookup{answers: map[string][]string{"192.168.1.10": {"new.lan."}}}

			r := resolver(t, hostnames.Config{Store: st, Lookup: look, Clock: now})
			if err := r.Sweep(context.Background()); err != nil {
				t.Fatalf("Sweep: %v", err)
			}
			if asked := len(look.askedFor()) > 0; asked != tc.wantAsk {
				t.Errorf("asked the resolver = %v, want %v", asked, tc.wantAsk)
			}
		})
	}
}

// A peer configured by name already carries the answer. Replacing it with
// whatever the network says about the address behind it loses what the
// operator wrote.
func TestAPeerGivenByNameIsNotLookedUp(t *testing.T) {
	now := func() time.Time { return at("2026-09-10T10:00:00Z") }
	st := newStore(now, store.Peer{ID: "a", Addrs: []string{"vpn-box.example:8844"}, Static: true})
	look := &fakeLookup{answers: map[string][]string{}}

	r := resolver(t, hostnames.Config{Store: st, Lookup: look, Clock: now})
	if err := r.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n := len(look.askedFor()); n != 0 {
		t.Errorf("asked the resolver %d times about a name it was already given", n)
	}
}

// The requirement this package exists for: DNS that never answers must cost a
// bounded wait inside the daemon and nothing at all anywhere else.
func TestAResolverThatNeverAnswersIsAbandoned(t *testing.T) {
	now := func() time.Time { return at("2026-09-10T10:00:00Z") }
	st := newStore(now, store.Peer{ID: "a", Addrs: []string{"192.168.1.10:8844"}})
	look := &fakeLookup{answers: map[string][]string{}, block: make(chan struct{})}
	defer close(look.block)

	r := resolver(t, hostnames.Config{
		Store: st, Lookup: look, Clock: now, Timeout: 50 * time.Millisecond,
	})

	done := make(chan error, 1)
	go func() { done <- r.Sweep(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Sweep: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep is still waiting on a resolver that will never answer")
	}
}

// Cancelling must not write "this address has no name" for an address that was
// never actually answered about.
func TestShutdownDoesNotCacheAnUnansweredLookup(t *testing.T) {
	now := func() time.Time { return at("2026-09-10T10:00:00Z") }
	st := newStore(now, store.Peer{ID: "a", Addrs: []string{"192.168.1.10:8844"}})
	look := &fakeLookup{answers: map[string][]string{}, block: make(chan struct{})}
	defer close(look.block)

	ctx, cancel := context.WithCancel(context.Background())
	r := resolver(t, hostnames.Config{Store: st, Lookup: look, Clock: now, Timeout: time.Minute})

	done := make(chan error, 1)
	go func() { done <- r.Sweep(ctx) }()
	// Wait until the lookup is in flight, then pull the context out from under it.
	for len(look.askedFor()) == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got := st.lookups(); len(got) != 0 {
		t.Errorf("cached %v on shutdown; an unanswered lookup is not an answer", got)
	}
}

// Run must survive a store that is briefly unavailable rather than ending the
// subsystem: a missing label is not worth taking anything down for.
func TestRunKeepsGoingWhenTheStoreFails(t *testing.T) {
	now := func() time.Time { return at("2026-09-10T10:00:00Z") }
	st := newStore(now, store.Peer{ID: "a", Addrs: []string{"192.168.1.10:8844"}})
	st.failOn = "peers"
	look := &fakeLookup{answers: map[string][]string{"192.168.1.10": {"nuc.lan."}}}

	tick := make(chan time.Time)
	r := resolver(t, hostnames.Config{Store: st, Lookup: look, Clock: now, Tick: tick})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// The first sweep fails. Recover, and the next tick must still work.
	st.mu.Lock()
	st.failOn = ""
	st.mu.Unlock()
	tick <- time.Time{}

	deadline := time.After(5 * time.Second)
	for st.name("192.168.1.10") == "" {
		select {
		case <-deadline:
			t.Fatal("never recovered from the store failure")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
}

func TestNewRequiresAStore(t *testing.T) {
	if _, err := hostnames.New(hostnames.Config{}); err == nil {
		t.Error("expected an error without a store")
	}
}
