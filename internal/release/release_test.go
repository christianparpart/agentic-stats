package release_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/release"
)

// signer returns a verifier and a function that signs like the pipeline does.
func signer(t *testing.T) (*release.Verifier, func([]byte) []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	v, err := release.NewVerifier(pub)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v, func(b []byte) []byte { return ed25519.Sign(priv, b) }
}

// manifestFor renders one `sha256sum`-style line for content under a name.
func manifestFor(t *testing.T, name, content string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:]) + "  " + name + "\n"
}

func TestOpenRejectsAManifestTheKeyDidNotSign(t *testing.T) {
	v, sign := signer(t)
	manifest := []byte(manifestFor(t, "agentic-stats-linux-amd64", "the real binary"))

	// A signature over different content, which is what a substituted
	// manifest from an attacker who cannot sign looks like.
	_, otherSign := signer(t)

	tests := []struct {
		name string
		sig  []byte
	}{
		{"signed by another key", otherSign(manifest)},
		{"signature of different content", sign([]byte("something else"))},
		{"empty signature", nil},
		{"truncated signature", sign(manifest)[:32]},
		{"flipped bit", flip(sign(manifest))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := v.Open(manifest, tt.sig); !errors.Is(err, release.ErrNotSigned) {
				t.Errorf("Open = %v, want ErrNotSigned", err)
			}
		})
	}
}

func flip(sig []byte) []byte {
	out := make([]byte, len(sig))
	copy(out, sig)
	out[0] ^= 0x01
	return out
}

func TestOpenAcceptsAndCoversEveryArtifact(t *testing.T) {
	v, sign := signer(t)
	manifest := []byte(
		manifestFor(t, "agentic-stats-linux-amd64", "one") +
			manifestFor(t, "agentic-stats-darwin-arm64", "two") +
			manifestFor(t, "agentic-statsw-windows-amd64.exe", "three"))

	m, err := v.Open(manifest, sign(manifest))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := len(m.Names()); got != 3 {
		t.Errorf("manifest covers %d artifacts, want 3", got)
	}
	if err := m.Check("agentic-stats-linux-amd64", strings.NewReader("one")); err != nil {
		t.Errorf("Check of matching content: %v", err)
	}
}

func TestCheckRejectsContentThatIsNotWhatWasPublished(t *testing.T) {
	v, sign := signer(t)
	manifest := []byte(manifestFor(t, "agentic-stats-linux-amd64", "the real binary"))
	m, err := v.Open(manifest, sign(manifest))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	tests := []struct {
		name     string
		artifact string
		content  string
		want     error
	}{
		{"substituted content", "agentic-stats-linux-amd64", "a different binary", release.ErrDigestMismatch},
		{"truncated content", "agentic-stats-linux-amd64", "the real binar", release.ErrDigestMismatch},
		{"empty content", "agentic-stats-linux-amd64", "", release.ErrDigestMismatch},
		{"an artifact this release never published", "agentic-stats-plan9-386", "the real binary", release.ErrUnknownArtifact},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := m.Check(tt.artifact, strings.NewReader(tt.content))
			if !errors.Is(err, tt.want) {
				t.Errorf("Check = %v, want %v", err, tt.want)
			}
		})
	}
}

// sha256sum writes two spaces in text mode and a space plus an asterisk in
// binary mode, and which one a release gets depends on the machine that built
// it -- the Windows build of coreutils emits the binary form. Both name the
// same artifact, and a node must not care which its release was made on.
func TestOpenAcceptsBothChecksumFormats(t *testing.T) {
	v, sign := signer(t)
	sum := sha256.Sum256([]byte("the real binary"))
	digest := hex.EncodeToString(sum[:])

	tests := []struct {
		name string
		line string
	}{
		{"text mode", digest + "  agentic-stats-linux-amd64\n"},
		{"binary mode", digest + " *agentic-stats-linux-amd64\n"},
		{"no trailing newline", digest + "  agentic-stats-linux-amd64"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := v.Open([]byte(tt.line), sign([]byte(tt.line)))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if err := m.Check("agentic-stats-linux-amd64", strings.NewReader("the real binary")); err != nil {
				t.Errorf("Check: %v", err)
			}
		})
	}
}

// A manifest names the release it belongs to, inside the signed bytes.
// Without it, an old and genuinely signed release passes every check when
// served in place of a new one.
func TestManifestCarriesItsRelease(t *testing.T) {
	v, sign := signer(t)
	line := manifestFor(t, "agentic-stats-linux-amd64", "one")

	tests := []struct {
		name string
		body string
		want string
	}{
		{"names its version", release.VersionLine + "v1.2.3\n" + line, "v1.2.3"},
		{"an unreleased build says so", release.VersionLine + "dev\n" + line, "dev"},
		// Manifests written before the line existed are still genuinely
		// signed; refusing them would strand every release already published.
		{"absent is empty, not an error", line, ""},
		{"an unrelated comment is ignored", "# something else\n" + line, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := v.Open([]byte(tt.body), sign([]byte(tt.body)))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if got := m.Version(); got != tt.want {
				t.Errorf("Version = %q, want %q", got, tt.want)
			}
			if len(m.Names()) != 1 {
				t.Errorf("comment lines were counted as artifacts: %v", m.Names())
			}
		})
	}
}

// Callers report these, so the order must be the same answer twice.
func TestNamesAreSorted(t *testing.T) {
	v, sign := signer(t)
	body := manifestFor(t, "agentic-stats-windows-amd64.exe", "c") +
		manifestFor(t, "agentic-stats-darwin-arm64", "a") +
		manifestFor(t, "agentic-stats-linux-amd64", "b")

	m, err := v.Open([]byte(body), sign([]byte(body)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	want := []string{
		"agentic-stats-darwin-arm64",
		"agentic-stats-linux-amd64",
		"agentic-stats-windows-amd64.exe",
	}
	if got := m.Names(); !slices.Equal(got, want) {
		t.Errorf("Names = %v, want %v", got, want)
	}
}

func TestParseRejectsMalformedManifests(t *testing.T) {
	v, sign := signer(t)

	tests := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"only whitespace", "   \n\n"},
		{"digest with no name", "abc123\n"},
		{"not hex", "zzzz  agentic-stats-linux-amd64\n"},
		{"digest too short", "abcd  agentic-stats-linux-amd64\n"},
		{"name missing after padding", strings.Repeat("a", 64) + "   \n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			if _, err := v.Open(body, sign(body)); err == nil {
				t.Error("Open accepted a malformed manifest")
			}
		})
	}
}

// The compiled-in key must be a usable ed25519 key, or every release fails
// verification on machines that already trust it.
func TestOfficialKeyIsWellFormed(t *testing.T) {
	if _, err := release.Official(); err != nil {
		t.Fatalf("Official: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(release.PublicKey)
	if err != nil {
		t.Fatalf("PublicKey is not base64: %v", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		t.Errorf("PublicKey is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
}

func TestNewVerifierRejectsAKeyOfTheWrongSize(t *testing.T) {
	for _, size := range []int{0, 16, 31, 33, 64} {
		if _, err := release.NewVerifier(make([]byte, size)); err == nil {
			t.Errorf("NewVerifier accepted a %d-byte key", size)
		}
	}
}

// A name in a manifest is eventually a path an updater writes to, so it must
// be a plain filename however trustworthy the signature is.
func TestParseRejectsNamesThatAreNotPlainFiles(t *testing.T) {
	v, sign := signer(t)
	sum := sha256.Sum256([]byte("x"))
	digest := hex.EncodeToString(sum[:])

	for _, name := range []string{
		"../escaped",
		"../../etc/passwd",
		"sub/dir",
		`sub\dir`,
		".",
		"..",
	} {
		t.Run(name, func(t *testing.T) {
			body := []byte(digest + "  " + name + "\n")
			if _, err := v.Open(body, sign(body)); err == nil {
				t.Errorf("Open accepted %q as an artifact name", name)
			}
		})
	}
}

// Two digests for one artifact is a generation fault. Taking the last one
// silently would publish a manifest that verifies but describes two things.
func TestParseRejectsADuplicateArtifact(t *testing.T) {
	v, sign := signer(t)
	body := []byte(
		manifestFor(t, "agentic-stats-linux-amd64", "one") +
			manifestFor(t, "agentic-stats-linux-amd64", "two"))

	if _, err := v.Open(body, sign(body)); err == nil {
		t.Error("Open accepted a manifest naming one artifact twice")
	}
}
