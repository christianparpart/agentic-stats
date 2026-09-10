// Package release verifies that a downloaded artifact is one this project
// published.
//
// The mesh key cannot answer that question. Every node holds it and derives
// the same sub-keys, so a signature made from it proves only that some node
// made the signature -- which is exactly what an attacker who has the key also
// has. Running code is a different privilege from reading the archive, and it
// needs a secret the fleet does not carry: the private half of the key below
// never leaves the release pipeline.
//
// What is signed is the checksum manifest, not each artifact. One signature
// then covers the whole release at once, so a node cannot be talked into
// pairing this release's binary for its platform with a different release's
// binary for another -- and the thing to verify is small enough to fetch and
// check before committing to a 25 MB download.
package release

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// PublicKey is the key every published release is signed with, base64.
//
// Compiled in rather than fetched: a key retrieved at the same time as the
// thing it authenticates authenticates nothing. Replacing it means shipping a
// new binary, which is the property that makes it worth something.
const PublicKey = "9Yxg/9G3Y6YjROQmMp6PccUeUF64plq2nQEGqFIePCM="

// ErrNotSigned reports a manifest that the release key did not sign.
var ErrNotSigned = errors.New("release: manifest is not signed by the release key")

// ErrUnknownArtifact reports a name the manifest does not cover.
var ErrUnknownArtifact = errors.New("release: manifest does not cover that artifact")

// ErrDigestMismatch reports content that is not what the manifest describes.
var ErrDigestMismatch = errors.New("release: content does not match its digest")

// Verifier opens signed manifests.
type Verifier struct {
	key ed25519.PublicKey
}

// NewVerifier returns a Verifier for one public key.
//
// The key is a parameter rather than read from PublicKey directly so tests can
// sign with their own, and so a fleet running its own builds can point at a
// key of its own without patching this package.
func NewVerifier(key ed25519.PublicKey) (*Verifier, error) {
	if len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("release: public key is %d bytes, want %d",
			len(key), ed25519.PublicKeySize)
	}
	return &Verifier{key: key}, nil
}

// Official returns a Verifier for the compiled-in release key.
func Official() (*Verifier, error) {
	key, err := base64.StdEncoding.DecodeString(PublicKey)
	if err != nil {
		return nil, fmt.Errorf("release: decode built-in public key: %w", err)
	}
	return NewVerifier(key)
}

// Manifest is the set of artifacts one release published, by name.
type Manifest struct {
	version string
	digests map[string][]byte
}

// VersionLine is how a manifest names the release it belongs to.
//
// A leading '#' so `sha256sum -c` skips it as a comment, which keeps the file
// usable by hand. It is inside the signed bytes, which is the point: without
// it every manifest is structurally identical to every other, so an old and
// genuinely signed release passes every check when served in place of a new
// one. The signature binds the artifacts to each other; this binds them to the
// release the caller asked for.
const VersionLine = "# agentic-stats "

// Open checks the signature over manifest and parses what it covers.
//
// The signature is checked before a single line is read, so malformed input
// from an unsigned source is never parsed at all.
func (v *Verifier) Open(manifest, signature []byte) (Manifest, error) {
	if !ed25519.Verify(v.key, manifest, signature) {
		return Manifest{}, ErrNotSigned
	}
	return parseManifest(manifest)
}

// parseManifest reads `sha256sum` output: a hex digest, spaces, a name.
func parseManifest(data []byte) (Manifest, error) {
	digests := make(map[string][]byte)
	scan := bufio.NewScanner(bytes.NewReader(data))
	version := ""
	for line := 1; scan.Scan(); line++ {
		text := scan.Text()
		if strings.HasPrefix(text, "#") {
			if rest, ok := strings.CutPrefix(text, VersionLine); ok {
				version = strings.TrimSpace(rest)
			}
			continue
		}
		// Fields rather than a hand-rolled split: it absorbs the surrounding
		// whitespace, sha256sum's two-space text form and its one-space
		// binary form alike, and a digest left with no name after it. No
		// artifact this project publishes has a space in its name.
		fields := strings.Fields(text)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return Manifest{}, fmt.Errorf("release: manifest line %d: want a digest and a name", line)
		}
		sum, err := hex.DecodeString(fields[0])
		if err != nil || len(sum) != sha256.Size {
			return Manifest{}, fmt.Errorf("release: manifest line %d: %q is not a sha256 digest", line, fields[0])
		}
		// Binary mode marks the name with an asterisk; text mode does not.
		digests[strings.TrimPrefix(fields[1], "*")] = sum
	}
	if err := scan.Err(); err != nil {
		return Manifest{}, fmt.Errorf("release: read manifest: %w", err)
	}
	if len(digests) == 0 {
		return Manifest{}, errors.New("release: manifest covers no artifacts")
	}
	return Manifest{version: version, digests: digests}, nil
}

// Version is the release this manifest belongs to, empty if it does not say.
//
// Empty rather than an error: a manifest written before this line existed is
// still a genuinely signed set of artifacts, and refusing it would strand
// every release already published. A caller that cares asserts on it.
func (m Manifest) Version() string { return m.version }

// Names lists the artifacts this release published, in a stable order.
//
// Sorted because callers report them: map order would name a different
// artifact first on each run, which reads as flapping rather than as the same
// answer twice.
func (m Manifest) Names() []string {
	return slices.Sorted(maps.Keys(m.digests))
}

// Check streams r and reports whether it is the named artifact.
//
// Streaming rather than taking a slice: an artifact is tens of megabytes, and
// holding one in memory to hash it is a cost a daemon does not need to pay.
func (m Manifest) Check(name string, r io.Reader) error {
	want, ok := m.digests[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownArtifact, name)
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, r); err != nil {
		return fmt.Errorf("release: read %s: %w", name, err)
	}
	if !bytes.Equal(sum.Sum(nil), want) {
		return fmt.Errorf("%w: %s", ErrDigestMismatch, name)
	}
	return nil
}
