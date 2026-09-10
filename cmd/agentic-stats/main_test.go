package main

import (
	"strings"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/mesh"
)

func TestNodeNamePrefersTheConfiguredName(t *testing.T) {
	const hostname = "darkleon"

	tests := []struct {
		name       string
		configured string
		want       string
	}{
		{"unset falls back to the hostname", "", hostname},
		{"configured wins", "darkleon-win", "darkleon-win"},
		// A dual-boot machine reports one hostname from either side; the
		// configured name is the only thing that tells the two halves apart.
		{"configured may differ only by suffix", "darkleon-linux", "darkleon-linux"},
		{"surrounding space is not part of a name", "  darkleon-win  ", "darkleon-win"},
		{"a name of only space is no name", "   ", hostname},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nodeName(tt.configured, func() string { return hostname })
			if got != tt.want {
				t.Errorf("nodeName(%q) = %q, want %q", tt.configured, got, tt.want)
			}
		})
	}
}

// An unreadable hostname must not become a name of its own.
func TestNodeNameCarriesAnEmptyFallbackThrough(t *testing.T) {
	if got := nodeName("", func() string { return "" }); got != "" {
		t.Errorf("nodeName = %q, want empty so peers fall back to reverse DNS", got)
	}
}

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
			// Two dual-boot halves report one hostname; the prefix is what
			// separates them in a report.
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

// Reporting the build must not depend on the environment.
//
// `version` is what an installer or a "is this node behind?" probe runs first,
// and it needs nothing from the config directory. os.UserConfigDir fails on
// Linux with neither $XDG_CONFIG_HOME nor $HOME set -- a minimal container, or
// a unit file without Environment=HOME= -- and an exit 1 there reads as a
// broken binary rather than a missing variable.
func TestVersionNeedsNoConfigDirectory(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("AppData", "")

	for _, spelling := range []string{"version", "--version"} {
		t.Run(spelling, func(t *testing.T) {
			if err := run([]string{spelling}); err != nil {
				t.Errorf("run(%q) = %v, want it to report the build regardless", spelling, err)
			}
		})
	}
}

// A command that does need the config path must still surface that failure
// rather than inheriting the exemption above.
func TestOtherCommandsStillReportAMissingConfigDirectory(t *testing.T) {
	// os.UserConfigDir reads %AppData% on Windows and $XDG_CONFIG_HOME or
	// $HOME elsewhere, and reports an error when the one it wants is empty --
	// so clearing all three exercises the failure on every platform.
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("AppData", "")

	if err := run([]string{"status"}); err == nil {
		t.Error("run(status) succeeded with no config directory, want the resolution error")
	}
}
