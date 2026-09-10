// Command relsign builds, signs and verifies a release manifest.
//
// Build tooling, not part of the daemon: it lives under internal/ so it stays
// out of ./cmd/..., which is the one shipped binary -- the same shape as
// internal/testfixture/cmd/fixturegen. It exists because the pipeline must
// produce and sign with the same code the fleet verifies with. A manifest
// written by coreutils and parsed by Go, or a signature made by openssl and
// checked by crypto/ed25519, agree until the day they do not.
//
// Producing the manifest here rather than shelling out to sha256sum also keeps
// `make release` working on macOS, which ships shasum under a different name.
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/christianparpart/agentic-stats/internal/release"
)

const usage = `relsign manifest <dir> <version> <manifest-out>
relsign sign     <key-file|-> <manifest> <signature-out>
relsign verify   <manifest> <signature> <artifact-dir>

manifest hashes every artifact in dir and writes a sha256sum-compatible file.
sign reads a base64 ed25519 private key, "-" meaning stdin, and writes a
detached signature. verify checks that signature against the public key
compiled into this build, then checks every artifact the manifest names.`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "relsign: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// Every verb takes exactly three operands, so one check covers them all.
	if len(args) != 4 {
		fmt.Fprintln(os.Stderr, usage)
		return errors.New("wrong number of arguments")
	}
	switch args[0] {
	case "manifest":
		return writeManifest(args[1], args[2], args[3])
	case "sign":
		return sign(args[1], args[2], args[3])
	case "verify":
		return verify(args[1], args[2], args[3])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// writeManifest hashes every artifact in dir, naming the release they belong to.
func writeManifest(dir, version, out string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}

	var b strings.Builder
	b.WriteString(release.VersionLine + version + "\n")
	covered := 0
	for _, entry := range entries {
		name := entry.Name()
		// The manifest describes artifacts, not itself or its signature.
		if entry.IsDir() || !strings.HasPrefix(name, "agentic-stats") {
			continue
		}
		digest, derr := digestFile(filepath.Join(dir, name))
		if derr != nil {
			return derr
		}
		// Two spaces: sha256sum's text form, which `sha256sum -c` reads
		// everywhere. Its binary form differs only by an asterisk, and this
		// package's parser takes either.
		fmt.Fprintf(&b, "%s  %s\n", digest, name)
		covered++
	}
	if covered == 0 {
		return fmt.Errorf("no artifacts in %s", dir)
	}

	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	fmt.Printf("%s covers %d artifacts of %s\n", out, covered, version)
	return nil
}

func digestFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer func() {
		// Read-only: Close has nothing to report that changes the digest.
		_ = f.Close()
	}()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func sign(keyPath, manifestPath, sigPath string) error {
	encoded, err := readKey(keyPath)
	if err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		return fmt.Errorf("decode key: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return fmt.Errorf("key is %d bytes, want %d", len(raw), ed25519.PrivateKeySize)
	}

	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	if err := os.WriteFile(sigPath, ed25519.Sign(ed25519.PrivateKey(raw), manifest), 0o644); err != nil {
		return fmt.Errorf("write signature: %w", err)
	}
	fmt.Printf("signed %s\n", manifestPath)
	return nil
}

// readKey reads the signing key, from stdin when the path is "-".
//
// Stdin so a pipeline can pass the key without it ever reaching a filesystem,
// where it would outlive the step that used it.
func readKey(path string) ([]byte, error) {
	if path == "-" {
		key, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read key from stdin: %w", err)
		}
		return key, nil
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key: %w", err)
	}
	return key, nil
}

func verify(manifestPath, sigPath, dir string) error {
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	signature, err := os.ReadFile(sigPath)
	if err != nil {
		return fmt.Errorf("read signature: %w", err)
	}

	v, err := release.Official()
	if err != nil {
		return err
	}
	m, err := v.Open(manifest, signature)
	if err != nil {
		return err
	}

	for _, name := range m.Names() {
		f, ferr := os.Open(filepath.Join(dir, name))
		if ferr != nil {
			return fmt.Errorf("open %s: %w", name, ferr)
		}
		cerr := m.Check(name, f)
		// Discarded deliberately: read-only, so Close has nothing to report
		// that would change the verdict the digest check just gave.
		_ = f.Close()
		if cerr != nil {
			return cerr
		}
	}
	fmt.Printf("%d artifacts of %s verify against the built-in release key\n",
		len(m.Names()), m.Version())
	return nil
}
