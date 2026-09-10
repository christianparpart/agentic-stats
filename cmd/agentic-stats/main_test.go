package main

import (
	"strings"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/mesh"
)

func TestPeerLabelNamesAPeerWithoutLosingItsOriginID(t *testing.T) {
	const full = "898962c0fdba8df8391bfda9504630cf"

	tests := []struct {
		name string
		peer mesh.PeerHealth
		want string
	}{
		{
			// Nothing to say beyond the id, so the id is the whole label.
			name: "no reported name",
			peer: mesh.PeerHealth{ID: full},
			want: full,
		},
		{
			name: "named peer keeps a prefix of its id",
			peer: mesh.PeerHealth{ID: full, Host: "darkleon"},
			want: "darkleon (898962c0)",
		},
		{
			// Two nodes can report one hostname -- a dual-boot machine, or two
			// VMs off one image. The prefix is what separates them.
			name: "same host, different origin",
			peer: mesh.PeerHealth{ID: "1f4c9a02b7e3d5560a1c8842f90b6e77", Host: "darkleon"},
			want: "darkleon (1f4c9a02)",
		},
		{
			// The handshake only requires a non-empty id, and nothing binds
			// the id a peer claims to the mesh key. Reporting must not panic.
			name: "id shorter than the prefix",
			peer: mesh.PeerHealth{ID: "abc", Host: "darkleon"},
			want: "darkleon (abc)",
		},
		{
			name: "id exactly the prefix length",
			peer: mesh.PeerHealth{ID: "898962c0", Host: "darkleon"},
			want: "darkleon (898962c0)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := peerLabel(tt.peer)
			if got != tt.want {
				t.Errorf("peerLabel = %q, want %q", got, tt.want)
			}
			if tt.peer.Host != "" && !strings.Contains(got, tt.peer.Host) {
				t.Errorf("peerLabel = %q, want it to carry the reported name", got)
			}
		})
	}
}
