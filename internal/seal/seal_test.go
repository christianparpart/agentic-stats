package seal_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/christianparpart/agentic-stats/internal/seal"
)

func mustDerive(t *testing.T, psk string) *seal.Keys {
	t.Helper()
	k, err := seal.Derive(psk)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	return k
}

func TestGenerateProducesAcceptableKeys(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		psk, err := seal.Generate()
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if seen[psk] {
			t.Fatal("Generate returned a duplicate key")
		}
		seen[psk] = true
		if _, err := seal.Derive(psk); err != nil {
			t.Fatalf("a generated key must be accepted: %v", err)
		}
	}
}

// The PSK gets read aloud, retyped and pasted. Formatting must not change it.
func TestParseNormalizesFormatting(t *testing.T) {
	psk, err := seal.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	canonical, err := seal.Parse(psk)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	for _, variant := range []string{
		strings.ReplaceAll(psk, "-", ""),
		strings.ToLower(psk),
		"  " + psk + "\t",
		strings.ReplaceAll(psk, "-", " "),
	} {
		got, err := seal.Parse(variant)
		if err != nil {
			t.Errorf("Parse(%q): %v", variant, err)
			continue
		}
		if !bytes.Equal(got, canonical) {
			t.Errorf("Parse(%q) differs from the canonical form", variant)
		}
	}
}

// A weak PSK must be refused outright, never stretched into looking strong.
func TestWeakKeysAreRefused(t *testing.T) {
	for _, weak := range []string{"", "hunter2", "correct-horse", strings.Repeat("a", 19)} {
		if _, err := seal.Derive(weak); !errors.Is(err, seal.ErrWeakPSK) {
			t.Errorf("Derive(%q) = %v, want ErrWeakPSK", weak, err)
		}
	}
	// A long passphrase is acceptable even though it is not base32.
	if _, err := seal.Derive(strings.Repeat("a", 40)); err != nil {
		t.Errorf("a sufficiently long passphrase should be accepted: %v", err)
	}
}

func TestSealRoundTrip(t *testing.T) {
	k := mustDerive(t, mustGenerate(t))
	for _, plaintext := range [][]byte{
		[]byte(`{"type":"assistant","message":{"usage":{"output_tokens":42}}}`),
		[]byte(""),
		bytes.Repeat([]byte("x"), 1<<20),
	} {
		sealed, err := k.SealPayload(plaintext)
		if err != nil {
			t.Fatalf("SealPayload: %v", err)
		}
		if bytes.Contains(sealed, plaintext) && len(plaintext) > 0 {
			t.Error("ciphertext contains the plaintext verbatim")
		}
		got, err := k.OpenPayload(sealed)
		if err != nil {
			t.Fatalf("OpenPayload: %v", err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Error("round trip changed the payload")
		}
	}
}

// Sealing the same bytes twice must not produce the same ciphertext, or a
// stolen database would leak which records are identical.
func TestSealIsNotDeterministic(t *testing.T) {
	k := mustDerive(t, mustGenerate(t))
	a, err := k.SealPayload([]byte("same"))
	if err != nil {
		t.Fatalf("SealPayload: %v", err)
	}
	b, err := k.SealPayload([]byte("same"))
	if err != nil {
		t.Fatalf("SealPayload: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Error("identical plaintexts sealed to identical ciphertexts")
	}
}

// The central privacy claim: a stolen archive is worthless without the PSK.
func TestWrongKeyCannotOpen(t *testing.T) {
	mine := mustDerive(t, mustGenerate(t))
	theirs := mustDerive(t, mustGenerate(t))

	sealed, err := mine.SealPayload([]byte("customer source code"))
	if err != nil {
		t.Fatalf("SealPayload: %v", err)
	}
	if _, err := theirs.OpenPayload(sealed); !errors.Is(err, seal.ErrNotSealed) {
		t.Fatalf("another mesh opened our record: %v", err)
	}
	// Tampering must fail the same way, and be indistinguishable from it.
	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := mine.OpenPayload(tampered); !errors.Is(err, seal.ErrNotSealed) {
		t.Errorf("tampered ciphertext: %v, want ErrNotSealed", err)
	}
	if _, err := mine.OpenPayload([]byte("short")); !errors.Is(err, seal.ErrNotSealed) {
		t.Errorf("truncated ciphertext: %v, want ErrNotSealed", err)
	}
}

// Each job gets its own key, so compromising one does not hand over the rest.
func TestSubKeysAreIndependent(t *testing.T) {
	k := mustDerive(t, mustGenerate(t))
	beacon := k.BeaconTag(1, []byte("nonce"), "node", "addrs")
	channel := k.ChannelTag([]byte("nonce"), "node")
	dash := k.DashboardKey()

	if bytes.Equal(beacon, channel) {
		t.Error("beacon and channel tags collide")
	}
	if bytes.Equal(dash, beacon) || bytes.Equal(dash, channel) {
		t.Error("the dashboard key collides with a tag")
	}
}

// The exporter binding is what defeats a TLS-terminating relay: a tag computed
// over one leg's keying material must not verify on the other's.
func TestChannelTagBindsToKeyingMaterial(t *testing.T) {
	k := mustDerive(t, mustGenerate(t))
	legA := k.ChannelTag([]byte("exporter-from-leg-a"), "client")
	legB := k.ChannelTag([]byte("exporter-from-leg-b"), "client")
	if seal.Equal(legA, legB) {
		t.Error("tags from different TLS sessions matched; a relay would succeed")
	}
	// Role asymmetry stops the initiator's tag being reflected back at it.
	if seal.Equal(k.ChannelTag([]byte("same"), "client"), k.ChannelTag([]byte("same"), "server")) {
		t.Error("client and server tags matched; a tag could be reflected")
	}
	// The same inputs must of course agree, or no peer could ever connect.
	if !seal.Equal(legA, k.ChannelTag([]byte("exporter-from-leg-a"), "client")) {
		t.Error("the same inputs produced different tags")
	}
}

// Two nodes holding the same PSK must derive byte-identical material, however
// the key was typed, or they could never talk.
func TestSamePSKDerivesSameKeys(t *testing.T) {
	psk := mustGenerate(t)
	a := mustDerive(t, psk)
	b := mustDerive(t, strings.ToLower(strings.ReplaceAll(psk, "-", "")))
	if !seal.Equal(a.DashboardKey(), b.DashboardKey()) {
		t.Fatal("the same PSK derived different keys")
	}
	sealed, err := a.SealPayload([]byte("hello"))
	if err != nil {
		t.Fatalf("SealPayload: %v", err)
	}
	got, err := b.OpenPayload(sealed)
	if err != nil {
		t.Fatalf("a peer with the same PSK could not open our record: %v", err)
	}
	if string(got) != "hello" {
		t.Error("payload differed across peers")
	}
}

// Length-prefixing stops distinct field splits from colliding.
func TestBeaconTagFieldsCannotBeShifted(t *testing.T) {
	k := mustDerive(t, mustGenerate(t))
	if seal.Equal(
		k.BeaconTag(1, []byte("ab"), "cd", "ef"),
		k.BeaconTag(1, []byte("a"), "bcd", "ef"),
	) {
		t.Error("shifting bytes between fields produced the same tag")
	}
}

func mustGenerate(t *testing.T) string {
	t.Helper()
	psk, err := seal.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return psk
}

// A user-chosen passphrase is accepted, and must derive the same key on every
// node however it was typed.
func TestPassphrasesAreAcceptedAndStable(t *testing.T) {
	const phrase = "correct horse battery staple 42"
	a := mustDerive(t, phrase)
	b := mustDerive(t, "  "+phrase+"  ")
	if !seal.Equal(a.DashboardKey(), b.DashboardKey()) {
		t.Error("surrounding whitespace changed the derived key")
	}

	sealed, err := a.SealPayload([]byte("hello"))
	if err != nil {
		t.Fatalf("SealPayload: %v", err)
	}
	if _, err := b.OpenPayload(sealed); err != nil {
		t.Fatalf("a peer with the same passphrase could not open the record: %v", err)
	}

	// A passphrase must not collide with a different one.
	other := mustDerive(t, "a completely different passphrase entirely")
	if seal.Equal(a.DashboardKey(), other.DashboardKey()) {
		t.Error("two different passphrases derived the same key")
	}
}

// A passphrase is stretched rather than used raw, which is what makes offline
// guessing expensive against ciphertext that lives in a synced folder.
func TestPassphrasesAreStretched(t *testing.T) {
	const phrase = "a passphrase long enough to pass"
	raw, err := seal.Parse(phrase)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if string(raw) == phrase {
		t.Fatal("the passphrase was used verbatim as key material")
	}
	if len(raw) != 32 {
		t.Errorf("derived %d bytes, want 32", len(raw))
	}
	// Deterministic across calls, or two nodes could never agree.
	again, err := seal.Parse(phrase)
	if err != nil {
		t.Fatalf("Parse again: %v", err)
	}
	if !seal.Equal(raw, again) {
		t.Error("stretching is not deterministic; nodes would never agree")
	}
}

// Too short to be worth stretching.
func TestShortPassphrasesAreStillRefused(t *testing.T) {
	for _, weak := range []string{"hunter2", "short phrase", "12345678901234567890"[:19]} {
		if _, err := seal.Derive(weak); !errors.Is(err, seal.ErrWeakPSK) {
			t.Errorf("Derive(%q) = %v, want ErrWeakPSK", weak, err)
		}
	}
}
