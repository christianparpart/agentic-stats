package update_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/release"
	"github.com/christianparpart/agentic-stats/internal/update"
	"github.com/christianparpart/agentic-stats/internal/version"
)

// The fixtures in testdata are the genuine v0.1.0 manifest and signature, as
// published, and this build's real public key opens them.
//
// A fixture manifest built by the test proves the client agrees with itself.
// This proves it agrees with what the pipeline actually produced: the format,
// the version line, the artifact names, and one signature over all of it. If
// the release process ever changes shape, this fails rather than every node
// failing quietly at upgrade time.
//
// Only the metadata is committed -- the binaries are 16 MB each, and what is
// under test here is the agreement, not the download.
func TestAgreesWithThePublishedRelease(t *testing.T) {
	dir := filepath.Join("testdata", "v0.1.0")

	manifest, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	signature, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS.sig"))
	if err != nil {
		t.Fatalf("read signature: %v", err)
	}

	v, err := release.Official()
	if err != nil {
		t.Fatalf("Official: %v", err)
	}
	m, err := v.Open(manifest, signature)
	if err != nil {
		t.Fatalf("the published v0.1.0 manifest does not verify: %v", err)
	}

	if m.Version() != "v0.1.0" {
		t.Errorf("published manifest names %q, want v0.1.0", m.Version())
	}

	// Every platform this project builds for must be covered, or a node on one
	// of them would reach a release that has nothing in it for the machine.
	names := m.Names()
	for _, platform := range [][2]string{
		{"darwin", "amd64"}, {"darwin", "arm64"},
		{"linux", "amd64"}, {"linux", "arm64"},
		{"windows", "amd64"}, {"windows", "arm64"},
	} {
		want := update.ArtifactName(platform[0], platform[1])
		if !slices.Contains(names, want) {
			t.Errorf("published release does not cover %s/%s as %q; it has %v",
				platform[0], platform[1], want, names)
		}
	}
}

// The decision a node on an older build would reach against the real release.
func TestWouldTargetThePublishedRelease(t *testing.T) {
	got, ok := update.Target(version.Parse("v0.0.9"), []string{"v0.1.0"})
	if !ok || got != "v0.1.0" {
		t.Errorf("Target = %q, %v; want v0.1.0, true", got, ok)
	}
	if _, ok := update.Target(version.Parse("v0.1.0"), []string{"v0.1.0"}); ok {
		t.Error("a node already on v0.1.0 would upgrade to it again")
	}
}
