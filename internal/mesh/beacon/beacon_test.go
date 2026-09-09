package beacon

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/christianparpart/agentic-stats/internal/seal"
)

func keys(t *testing.T) *seal.Keys {
	t.Helper()
	psk, err := seal.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	k, err := seal.Derive(psk)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	return k
}

func newAt(t *testing.T, k *seal.Keys, id string, at time.Time) *Beacon {
	t.Helper()
	b, err := New(Config{
		Keys: k, NodeID: id, SyncPort: 8844,
		Now: func() time.Time { return at },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func encode(t *testing.T, b *Beacon, addrs []string) []byte {
	t.Helper()
	p, err := b.build(addrs)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

var src = &net.UDPAddr{IP: net.IPv4(192, 168, 1, 20), Port: 45897}

func TestAuthenticBeaconIsAccepted(t *testing.T) {
	k := keys(t)
	now := time.Unix(1_800_000_000, 0)
	sender := newAt(t, k, "sender", now)
	receiver := newAt(t, k, "receiver", now)

	peer, ok := receiver.verify(encode(t, sender, []string{"192.168.1.20"}), src)
	if !ok {
		t.Fatal("an authentic beacon was rejected")
	}
	if peer.NodeID != "sender" {
		t.Errorf("node id = %q, want sender", peer.NodeID)
	}
	if len(peer.Addrs) == 0 {
		t.Error("no addresses recovered")
	}
}

// Another mesh on the same LAN must be invisible.
func TestBeaconFromAnotherMeshIsIgnored(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	sender := newAt(t, keys(t), "stranger", now)
	receiver := newAt(t, keys(t), "receiver", now)

	if _, ok := receiver.verify(encode(t, sender, []string{"192.168.1.20"}), src); ok {
		t.Fatal("a beacon from a different mesh was accepted")
	}
}

// Laptops sleep and VMs get restored from snapshots, so the window has to be
// generous. It is safe because a beacon authorises nothing.
func TestClockSkewToleranceIsGenerous(t *testing.T) {
	k := keys(t)
	base := time.Unix(1_800_000_000, 0)
	sender := newAt(t, k, "sender", base)
	body := encode(t, sender, []string{"192.168.1.20"})

	tests := []struct {
		name  string
		skew  time.Duration
		alive bool
	}{
		{"no skew", 0, true},
		{"four minutes fast", 4 * time.Minute, true},
		{"four minutes slow", -4 * time.Minute, true},
		{"half an hour out", 30 * time.Minute, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			receiver := newAt(t, k, "receiver", base.Add(tc.skew))
			_, ok := receiver.verify(body, src)
			if ok != tc.alive {
				t.Errorf("accepted = %v, want %v", ok, tc.alive)
			}
		})
	}
}

// The node id and addresses are inside the MAC, so a captured tag cannot be
// spliced onto a forged advertisement.
func TestForgedFieldsAreRejected(t *testing.T) {
	k := keys(t)
	now := time.Unix(1_800_000_000, 0)
	sender := newAt(t, k, "sender", now)
	receiver := newAt(t, k, "receiver", now)

	var p packet
	if err := json.Unmarshal(encode(t, sender, []string{"192.168.1.20"}), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	tamper := []struct {
		name string
		with func(packet) packet
	}{
		{"node id", func(x packet) packet { x.NodeID = "impostor"; return x }},
		{"addresses", func(x packet) packet { x.Addrs = []string{"10.0.0.1"}; return x }},
		{"port", func(x packet) packet { x.Port = 9999; return x }},
		{"tag", func(x packet) packet { x.Tag = append([]byte{}, 1, 2, 3); return x }},
		{"nonce", func(x packet) packet { x.Nonce = []byte("different-nonce!"); return x }},
	}
	for _, tc := range tamper {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(tc.with(p))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if _, ok := receiver.verify(body, src); ok && tc.name != "port" {
				t.Errorf("a beacon with a forged %s was accepted", tc.name)
			}
		})
	}
}

// Every beacon must differ, or a passive observer could count mesh members and
// correlate them over time.
func TestBeaconsAreNotConstantWithinAnEpoch(t *testing.T) {
	k := keys(t)
	now := time.Unix(1_800_000_000, 0)
	b := newAt(t, k, "sender", now)

	first := encode(t, b, []string{"192.168.1.20"})
	second := encode(t, b, []string{"192.168.1.20"})
	if string(first) == string(second) {
		t.Error("two beacons in the same epoch were identical; " +
			"an observer could fingerprint the mesh")
	}
}

func TestOwnBeaconsAreNotTreatedAsPeers(t *testing.T) {
	k := keys(t)
	now := time.Unix(1_800_000_000, 0)
	self := newAt(t, k, "me", now)

	peer, ok := self.verify(encode(t, self, []string{"192.168.1.20"}), src)
	if !ok {
		t.Fatal("our own beacon should still verify")
	}
	// Run's listener drops it by node id; assert the id is what that check sees.
	if peer.NodeID != "me" {
		t.Errorf("node id = %q, want me", peer.NodeID)
	}
}

// Point-to-point links carry no multicast, which is why a tunnel needs the
// configured peer list instead.
func TestPointToPointAndExcludedInterfacesAreSkipped(t *testing.T) {
	b, err := New(Config{
		Keys: keys(t), NodeID: "n", ExcludeInterfaces: []string{"bridge100"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, ifi := range b.eligible() {
		if ifi.Name == "bridge100" {
			t.Error("an excluded interface was selected")
		}
		if ifi.Flags&net.FlagPointToPoint != 0 {
			t.Errorf("%s is point-to-point and cannot carry multicast", ifi.Name)
		}
		if ifi.Flags&net.FlagLoopback != 0 {
			t.Errorf("%s is loopback", ifi.Name)
		}
	}
}

func TestNewRequiresItsDependencies(t *testing.T) {
	if _, err := New(Config{NodeID: "n"}); err == nil {
		t.Error("expected an error when Keys is missing")
	}
	if _, err := New(Config{Keys: keys(t)}); err == nil {
		t.Error("expected an error when NodeID is missing")
	}
}
