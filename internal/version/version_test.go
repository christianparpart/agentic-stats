package version_test

import (
	"testing"

	"github.com/christianparpart/agentic-stats/internal/version"
)

func TestParseClassifiesReleases(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		release bool
	}{
		{"a tagged release", "v1.2.3", true},
		{"the leading v is optional", "1.2.3", true},
		{"zeroes are a release", "v0.0.0", true},
		{"double-digit components", "v10.20.30", true},
		{"surrounding space is trimmed", "  v1.2.3\n", true},

		// git describe's own output, none of which is a release.
		{"an unstamped build", "dev", false},
		{"a dirty tree", "v1.2.3-dirty", false},
		{"ahead of its tag", "v1.2.3-5-gabc1234", false},
		{"no tag at all", "abc1234", false},
		{"a release candidate", "v1.2.3-rc1", false},

		{"empty", "", false},
		{"two components", "v1.2", false},
		{"four components", "v1.2.3.4", false},
		{"non-numeric", "v1.x.3", false},
		{"a signed component", "v1.-2.3", false},
		{"an empty component", "v1..3", false},
		{"trailing dot", "v1.2.3.", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := version.Parse(tt.in)
			if got.IsRelease() != tt.release {
				t.Errorf("Parse(%q).IsRelease() = %v, want %v", tt.in, got.IsRelease(), tt.release)
			}
		})
	}
}

// Whatever a build calls itself is worth reporting, release or not.
func TestParseKeepsTheRawText(t *testing.T) {
	for _, in := range []string{"v1.2.3", "dev", "v1.2.3-dirty", ""} {
		if got := version.Parse(in).String(); got != in {
			t.Errorf("Parse(%q).String() = %q, want it unchanged", in, got)
		}
	}
}

func TestCompareRanksVersions(t *testing.T) {
	tests := []struct {
		name string
		v    string
		with string
		want version.Order
	}{
		{"identical", "v1.2.3", "v1.2.3", version.OrderSame},
		{"the v does not change identity", "v1.2.3", "1.2.3", version.OrderSame},

		{"patch ahead", "v1.2.4", "v1.2.3", version.OrderNewer},
		{"patch behind", "v1.2.3", "v1.2.4", version.OrderOlder},
		{"minor ahead", "v1.3.0", "v1.2.9", version.OrderNewer},
		{"major ahead", "v2.0.0", "v1.9.9", version.OrderNewer},

		// The reason this is not a string comparison.
		{"ten is after nine", "v0.10.0", "v0.9.0", version.OrderNewer},
		{"nine is before ten", "v0.9.0", "v0.10.0", version.OrderOlder},

		// A release outranks anything unpublished; never the reverse. This is
		// what stops a development box dragging the fleet.
		{"a release beats dev", "v1.2.3", "dev", version.OrderNewer},
		{"dev loses to a release", "dev", "v1.2.3", version.OrderOlder},
		{"a release beats a dirty tree", "v1.2.3", "v9.9.9-dirty", version.OrderNewer},
		{"a release beats ahead-of-tag", "v1.2.3", "v9.9.9-5-gabc1234", version.OrderNewer},
		{"a release beats a candidate", "v1.2.3", "v9.9.9-rc1", version.OrderNewer},

		// Two unpublished builds say nothing about each other.
		{"dev against dev", "dev", "dev", version.OrderUnknown},
		{"dev against a dirty tree", "dev", "v1.2.3-dirty", version.OrderUnknown},
		{"empty against dev", "", "dev", version.OrderUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := version.Parse(tt.v).Compare(version.Parse(tt.with))
			if got != tt.want {
				t.Errorf("Parse(%q).Compare(Parse(%q)) = %v, want %v", tt.v, tt.with, got, tt.want)
			}
		})
	}
}

// Newer and older must be exact mirrors, or a fleet could have two nodes each
// believing the other is behind.
func TestCompareIsAntisymmetric(t *testing.T) {
	versions := []string{"v0.0.0", "v0.9.0", "v0.10.0", "v1.2.3", "v2.0.0", "dev", "v1.2.3-dirty", ""}

	opposite := map[version.Order]version.Order{
		version.OrderSame:    version.OrderSame,
		version.OrderNewer:   version.OrderOlder,
		version.OrderOlder:   version.OrderNewer,
		version.OrderUnknown: version.OrderUnknown,
	}

	for _, a := range versions {
		for _, b := range versions {
			forward := version.Parse(a).Compare(version.Parse(b))
			back := version.Parse(b).Compare(version.Parse(a))
			if back != opposite[forward] {
				t.Errorf("Compare(%q,%q) = %v but Compare(%q,%q) = %v, want %v",
					a, b, forward, b, a, back, opposite[forward])
			}
		}
	}
}

// The zero value must be the harmless one: a Version nobody set names no
// release and cannot outrank one.
func TestZeroValueNamesNoRelease(t *testing.T) {
	var zero version.Version
	if zero.IsRelease() {
		t.Error("the zero Version reports itself as a release")
	}
	if got := zero.Compare(version.Parse("v0.0.0")); got != version.OrderOlder {
		t.Errorf("zero.Compare(v0.0.0) = %v, want OrderOlder", got)
	}
}
