// Package certs generates and persists the self-signed certificates a node
// uses for its peer channel and, when it binds off-loopback, its dashboard.
//
// The certificate is not a trust anchor here. Peers authenticate each other
// from the pre-shared key bound to the TLS exporter, so the certificate exists
// only to carry the key exchange. For the dashboard it is a genuine
// self-signed certificate with the usual browser interstitial.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Lifetime is how long a generated certificate is valid.
//
// Apple platforms reject server certificates valid for more than 398 days, and
// the industry ceiling is falling further. A year sits comfortably inside it.
const Lifetime = 365 * 24 * time.Hour

// renewBefore triggers regeneration while a certificate is still valid.
const renewBefore = 30 * 24 * time.Hour

// Config describes what a certificate must cover.
type Config struct {
	// CertPath and KeyPath are where the pair is persisted. Empty keeps it in
	// memory, which is right for a peer channel that no one has to trust.
	CertPath string
	KeyPath  string
	// Hosts are extra DNS names to cover, beyond localhost and this machine.
	Hosts []string
	// Now supplies time. Zero uses the system clock.
	Now func() time.Time
}

// EnsureCertificate loads a usable certificate, generating one if the stored
// pair is missing, expiring, or no longer covers this machine's addresses.
//
// The address check matters more than expiry in practice: every VPN connect,
// Wi-Fi change or DHCP lease invalidates the stored SAN list, and a
// certificate that does not name the address a browser used is a hard failure
// rather than a warning.
func EnsureCertificate(cfg Config) (tls.Certificate, error) {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	if cfg.CertPath != "" && cfg.KeyPath != "" {
		if cert, ok := loadUsable(cfg, now()); ok {
			return cert, nil
		}
	}

	cert, certPEM, keyPEM, err := generate(cfg, now())
	if err != nil {
		return tls.Certificate{}, err
	}
	if cfg.CertPath != "" && cfg.KeyPath != "" {
		if err := persist(cfg, certPEM, keyPEM); err != nil {
			return tls.Certificate{}, err
		}
	}
	return cert, nil
}

// loadUsable reads a stored pair and reports whether it can still be used.
func loadUsable(cfg Config, now time.Time) (tls.Certificate, bool) {
	cert, err := tls.LoadX509KeyPair(cfg.CertPath, cfg.KeyPath)
	if err != nil {
		return tls.Certificate{}, false
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return tls.Certificate{}, false
	}
	if now.Add(renewBefore).After(leaf.NotAfter) {
		return tls.Certificate{}, false
	}
	if !covers(leaf, localIPs()) {
		return tls.Certificate{}, false
	}
	cert.Leaf = leaf
	return cert, true
}

// covers reports whether the certificate names every current address.
func covers(leaf *x509.Certificate, ips []net.IP) bool {
	for _, ip := range ips {
		found := false
		for _, have := range leaf.IPAddresses {
			if have.Equal(ip) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// generate mints a fresh self-signed certificate.
func generate(cfg Config, now time.Time) (tls.Certificate, []byte, []byte, error) {
	// ECDSA P-256, not Ed25519: Chrome still rejects Ed25519 *server*
	// certificates. The 2026 Ed25519 support is WebCrypto, not TLS.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("certs: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("certs: generate serial: %w", err)
	}

	hostname, _ := os.Hostname()
	dns := []string{"localhost"}
	if hostname != "" {
		dns = append(dns, hostname, hostname+".local")
	}
	dns = append(dns, cfg.Hosts...)

	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{Organization: []string{"agentic-stats"}},
		// Backdated an hour so a peer with a slightly fast clock still accepts
		// it the moment it is generated.
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(Lifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              dns,
		IPAddresses:           localIPs(),
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("certs: create certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("certs: marshal key: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("certs: assemble pair: %w", err)
	}
	return cert, certPEM, keyPEM, nil
}

// persist writes the pair, keeping the private key owner-only.
func persist(cfg Config, certPEM, keyPEM []byte) error {
	if err := os.MkdirAll(filepath.Dir(cfg.CertPath), 0o700); err != nil {
		return fmt.Errorf("certs: create directory: %w", err)
	}
	if err := os.WriteFile(cfg.CertPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("certs: write certificate: %w", err)
	}
	if err := os.WriteFile(cfg.KeyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("certs: write key: %w", err)
	}
	return nil
}

// localIPs returns the loopback addresses plus every unicast address this
// machine currently holds.
func localIPs() []net.IP {
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			ips = append(ips, ipnet.IP)
		}
	}
	return ips
}
