// Package seal owns the pre-shared key and everything derived from it.
//
// This is the only package that ever sees the PSK. Every other package asks it
// for a key or hands it a plaintext, so "where is the secret used?" has exactly
// one answer and a grep for the PSK outside this package should find nothing.
//
// One secret does four jobs, each behind its own derived key, so that
// compromising one does not hand over the others:
//
//	psk ──HKDF──┬─► beacon     discovery tags
//	            ├─► channel    peer authentication
//	            ├─► payload    sealing records at rest and in bundles
//	            └─► dashboard  the web login verifier
package seal

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

// MinPSKBytes is the least entropy a pre-shared key may carry.
//
// The PSK is the only boundary around the archive: it admits peers, encrypts
// every record, and unlocks the dashboard. A human-chosen value would not do,
// which is why Generate exists and why Parse refuses short ones.
const MinPSKBytes = 20

// ErrWeakPSK reports a pre-shared key with too little entropy.
var ErrWeakPSK = errors.New("seal: pre-shared key is too weak")

// ErrNotSealed reports a ciphertext that is malformed or was sealed under a
// different key. The two are deliberately indistinguishable.
var ErrNotSealed = errors.New("seal: cannot open")

// pskEncoding renders a PSK as unpadded, case-insensitive base32, which
// survives being read aloud, retyped, and pasted into a browser form.
var pskEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// Generate returns a fresh pre-shared key with 256 bits of entropy.
func Generate() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("seal: generate pre-shared key: %w", err)
	}
	return format(pskEncoding.EncodeToString(buf)), nil
}

// format groups an encoded key into readable runs.
func format(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && i%8 == 0 {
			b.WriteByte('-')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Keys are the sub-keys derived from one pre-shared key.
type Keys struct {
	beacon    []byte
	channel   []byte
	payload   []byte
	dashboard []byte
	aead      cipher
}

// cipher is the authenticated cipher used for payloads. Named so tests can see
// the indirection without exporting the key material.
type cipher interface {
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
	NonceSize() int
}

// Derive expands a pre-shared key into the four sub-keys.
//
// The PSK is normalized first, so a key retyped with different grouping or
// casing still derives the same material.
func Derive(psk string) (*Keys, error) {
	raw, err := Parse(psk)
	if err != nil {
		return nil, err
	}

	sub := func(label string) ([]byte, error) {
		k, herr := hkdf.Key(sha256.New, raw, nil, "agentic-stats "+label, 32)
		if herr != nil {
			return nil, fmt.Errorf("seal: derive %s key: %w", label, herr)
		}
		return k, nil
	}

	k := &Keys{}
	for _, spec := range []struct {
		label string
		dst   *[]byte
	}{
		{"beacon-v1", &k.beacon},
		{"channel-v1", &k.channel},
		{"payload-v1", &k.payload},
		{"dashboard-v1", &k.dashboard},
	} {
		key, derr := sub(spec.label)
		if derr != nil {
			return nil, derr
		}
		*spec.dst = key
	}

	// XChaCha20-Poly1305 rather than AES-GCM: its 24-byte nonce makes random
	// nonces safe for far more records than we will ever store, with no
	// counter to keep and no birthday bound to reason about.
	aead, err := chacha20poly1305.NewX(k.payload)
	if err != nil {
		return nil, fmt.Errorf("seal: init payload cipher: %w", err)
	}
	k.aead = aead
	return k, nil
}

// Parse normalizes and validates a pre-shared key, returning its raw bytes.
func Parse(psk string) ([]byte, error) {
	normalized := strings.ToUpper(strings.NewReplacer("-", "", " ", "", "\t", "").Replace(strings.TrimSpace(psk)))
	if normalized == "" {
		return nil, fmt.Errorf("%w: empty", ErrWeakPSK)
	}
	raw, err := pskEncoding.DecodeString(normalized)
	if err != nil {
		// Accept an arbitrary passphrase, but hold it to the same entropy bar
		// by length. Anything shorter is refused rather than stretched, since
		// stretching a weak secret would only disguise the weakness.
		raw = []byte(psk)
	}
	if len(raw) < MinPSKBytes {
		return nil, fmt.Errorf("%w: %d bytes, need at least %d (use `agentic-stats init` to generate one)",
			ErrWeakPSK, len(raw), MinPSKBytes)
	}
	return raw, nil
}

// SealPayload encrypts a record body. The nonce is prepended to the ciphertext.
func (k *Keys) SealPayload(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("seal: nonce: %w", err)
	}
	return k.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// OpenPayload decrypts a record body sealed by SealPayload.
func (k *Keys) OpenPayload(sealed []byte) ([]byte, error) {
	n := k.aead.NonceSize()
	if len(sealed) < n {
		return nil, ErrNotSealed
	}
	out, err := k.aead.Open(nil, sealed[:n], sealed[n:], nil)
	if err != nil {
		// Never distinguish "malformed" from "wrong key".
		return nil, ErrNotSealed
	}
	return out, nil
}

// BeaconTag authenticates a discovery beacon.
//
// The node data is inside the MAC deliberately: a tag over the epoch alone
// would be constant for its whole window, letting a passive observer count
// PSK groups and correlate their members over time.
func (k *Keys) BeaconTag(epoch int64, nonce []byte, nodeID string, addrs string) []byte {
	return mac(k.beacon, "beacon-v1", fmt.Sprint(epoch), string(nonce), nodeID, addrs)
}

// ChannelTag proves PSK possession over an established TLS channel.
//
// ekm must come from tls.ConnectionState.ExportKeyingMaterial. Binding to the
// exporter is what defeats a TLS-terminating relay: the two legs of a
// man-in-the-middle produce different exported material, so a tag from one
// cannot be replayed onto the other (RFC 9266).
//
// role differs per direction so the initiator's tag cannot be reflected back.
func (k *Keys) ChannelTag(ekm []byte, role string) []byte {
	return mac(k.channel, "channel-v1", role, string(ekm))
}

// DashboardKey is the verifier the web login compares against.
func (k *Keys) DashboardKey() []byte {
	out := make([]byte, len(k.dashboard))
	copy(out, k.dashboard)
	return out
}

// Equal compares two secrets in constant time.
func Equal(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

// mac computes a domain-separated HMAC over length-prefixed parts, so that
// distinct inputs cannot be made to collide by shifting bytes between fields.
func mac(key []byte, parts ...string) []byte {
	h := hmacNew(key)
	for _, p := range parts {
		var lenbuf [8]byte
		putUint64(lenbuf[:], uint64(len(p)))
		h.Write(lenbuf[:])
		h.Write([]byte(p))
	}
	return h.Sum(nil)
}
