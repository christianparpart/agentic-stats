package mesh

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/christianparpart/agentic-stats/internal/store"
)

var now = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) string { return now.Add(-d).Format(time.RFC3339Nano) }

func vectorJSON(t *testing.T, v store.VersionVector) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal vector: %v", err)
	}
	return string(b)
}

// The distinction that matters: a peer can be seen constantly and still not be
// converging. Classifying on last_seen would call that healthy, which is the
// failure this report exists to catch.
func TestReachClassification(t *testing.T) {
	tests := []struct {
		name string
		peer store.Peer
		want Reach
	}{
		{
			name: "never converged",
			peer: store.Peer{ID: "p", LastSeen: ago(time.Second)},
			want: ReachUnknown,
		},
		{
			name: "converged just now",
			peer: store.Peer{ID: "p", LastConverged: ago(time.Minute)},
			want: ReachCurrent,
		},
		{
			name: "converged a long time ago",
			peer: store.Peer{ID: "p", LastConverged: ago(3 * StaleAfter)},
			want: ReachStale,
		},
		{
			name: "worked before, failing now",
			peer: store.Peer{ID: "p", LastConverged: ago(time.Minute), LastError: "connection refused"},
			want: ReachFailing,
		},
		{
			name: "announcing constantly but never converging",
			peer: store.Peer{ID: "p", LastSeen: ago(time.Second)},
			want: ReachUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := peerHealth(tc.peer, store.VersionVector{}, now)
			if got.Reach != tc.want {
				t.Errorf("reach = %v, want %v", got.Reach, tc.want)
			}
		})
	}
}

// Failing must win over stale, or a peer that has been broken for a week would
// be reported only as out of date.
func TestFailingOutranksStale(t *testing.T) {
	p := store.Peer{
		ID:            "p",
		LastConverged: ago(3 * StaleAfter),
		LastError:     "i/o timeout",
	}
	if got := peerHealth(p, store.VersionVector{}, now).Reach; got != ReachFailing {
		t.Errorf("reach = %v, want %v", got, ReachFailing)
	}
}

// Lag is the answer to "am I actually replicated", so both directions have to
// be right: behind means records exist that this node does not hold.
func TestLagIsCountedInBothDirections(t *testing.T) {
	tests := []struct {
		name              string
		mine, theirs      store.VersionVector
		wantBehind, ahead int64
	}{
		{
			name:   "in step",
			mine:   store.VersionVector{"a": 10, "b": 5},
			theirs: store.VersionVector{"a": 10, "b": 5},
		},
		{
			name:       "behind on one origin",
			mine:       store.VersionVector{"a": 10},
			theirs:     store.VersionVector{"a": 17},
			wantBehind: 7,
		},
		{
			name:   "ahead on one origin",
			mine:   store.VersionVector{"a": 20},
			theirs: store.VersionVector{"a": 12},
			ahead:  8,
		},
		{
			name:       "behind on one and ahead on another",
			mine:       store.VersionVector{"a": 20, "b": 1},
			theirs:     store.VersionVector{"a": 12, "b": 9},
			wantBehind: 8,
			ahead:      8,
		},
		{
			name:       "an origin we have never heard of",
			mine:       store.VersionVector{},
			theirs:     store.VersionVector{"c": 4},
			wantBehind: 4,
		},
		{
			name:   "an origin the peer has never heard of",
			mine:   store.VersionVector{"c": 4},
			theirs: store.VersionVector{},
			ahead:  4,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			behind, ahead := lag(tc.mine, vectorJSON(t, tc.theirs))
			if behind != tc.wantBehind || ahead != tc.ahead {
				t.Errorf("behind, ahead = %d, %d; want %d, %d",
					behind, ahead, tc.wantBehind, tc.ahead)
			}
		})
	}
}

// A peer that has never announced, or announced something unreadable, must not
// take the whole report down with it.
func TestLagToleratesAMissingOrBrokenVector(t *testing.T) {
	for _, v := range []string{"", "not json", "{", `{"a":"not a number"}`} {
		behind, ahead := lag(store.VersionVector{"a": 5}, v)
		if behind != 0 || ahead != 0 {
			t.Errorf("vector %q gave behind %d ahead %d, want 0 and 0", v, behind, ahead)
		}
	}
}

// A clock that has moved must not produce a negative age.
func TestAFutureConvergenceReadsAsZero(t *testing.T) {
	p := store.Peer{ID: "p", LastConverged: now.Add(time.Hour).Format(time.RFC3339Nano)}
	if got := peerHealth(p, store.VersionVector{}, now).SinceConverged; got != 0 {
		t.Errorf("since converged = %d, want 0", got)
	}
}

// A machine configured in mesh.peers is recorded twice -- once under the
// address, once under the node id it proves on answering. Left alone, the
// placeholder copy reads as a peer that has never converged, which is precisely
// the shape of a real fault and would teach the reader to ignore it.
func TestAPlaceholderIsDroppedOnceTheMachineAnswers(t *testing.T) {
	tests := []struct {
		name  string
		peers []store.Peer
		want  []string
	}{
		{
			name: "the machine has answered",
			peers: []store.Peer{
				// The peer reports the ephemeral port it dialled from, not the
				// one it listens on, so only the host can match.
				{ID: "realnodeid", Addrs: []string{"127.0.0.1:53588"}, LastConverged: ago(time.Minute)},
				{ID: staticPrefix + "127.0.0.1:8851", Addrs: []string{"127.0.0.1:8851"}, Static: true},
			},
			want: []string{"realnodeid"},
		},
		{
			name: "the machine has never answered",
			peers: []store.Peer{
				{ID: staticPrefix + "vm.internal:8844", Addrs: []string{"vm.internal:8844"}, Static: true},
			},
			// Kept: it is the only evidence the address was configured at all.
			want: []string{staticPrefix + "vm.internal:8844"},
		},
		{
			name: "a different machine answered",
			peers: []store.Peer{
				{ID: "realnodeid", Addrs: []string{"10.0.0.9:8844"}, LastConverged: ago(time.Minute)},
				{ID: staticPrefix + "vm.internal:8844", Addrs: []string{"vm.internal:8844"}, Static: true},
			},
			want: []string{"realnodeid", staticPrefix + "vm.internal:8844"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := withoutRedundantPlaceholders(tc.peers)
			var ids []string
			for _, p := range got {
				ids = append(ids, p.ID)
			}
			if len(ids) != len(tc.want) {
				t.Fatalf("kept %v, want %v", ids, tc.want)
			}
			for i := range ids {
				if ids[i] != tc.want[i] {
					t.Errorf("kept %v, want %v", ids, tc.want)
					break
				}
			}
		})
	}
}

// The JSON form has to be the name, so a reordering of the constants cannot
// silently change what a monitor is reading.
func TestReachMarshalsAsItsName(t *testing.T) {
	b, err := json.Marshal(ReachFailing)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `"failing"` {
		t.Errorf("marshalled as %s, want \"failing\"", b)
	}
}
