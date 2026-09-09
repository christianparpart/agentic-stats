package channel_test

import (
	"crypto/tls"

	"github.com/christianparpart/agentic-stats/internal/certs"
)

// ephemeralCert mints an in-memory certificate, as the listener does.
func ephemeralCert() (tls.Certificate, error) {
	return certs.EnsureCertificate(certs.Config{})
}
