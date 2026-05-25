package dispatch

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// MTLSConfig holds the file paths for the internal mutual-TLS material
// (CAESIUM_INTERNAL_MTLS_CA/CERT/KEY).  The same key pair is this node's
// identity in both directions: the server certificate on its internal listener
// and the client certificate it presents when POSTing to a peer.
type MTLSConfig struct {
	CAFile   string
	CertFile string
	KeyFile  string
}

// Configured reports whether all three material paths are set.  Run-owner mode
// requires this to be true (enforced at startup).
func (c MTLSConfig) Configured() bool {
	return c.CAFile != "" && c.CertFile != "" && c.KeyFile != ""
}

// loadCertPool reads a PEM CA bundle into an x509 pool.
func loadCertPool(caFile string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: read CA %q: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("mtls: no certificates found in CA %q", caFile)
	}
	return pool, nil
}

// ServerTLSConfig builds the *tls.Config for the internal listener: it presents
// this node's certificate and requires + verifies a client certificate signed
// by the configured CA on every connection.  A peer with no certificate, or one
// signed by an unknown CA, fails the TLS handshake before reaching a handler.
func ServerTLSConfig(c MTLSConfig) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: load server keypair: %w", err)
	}
	pool, err := loadCertPool(c.CAFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ClientTLSConfig builds the *tls.Config used when this node POSTs to a peer's
// internal endpoints (PostDispatch / PostComplete): it presents this node's
// certificate and verifies the peer's server certificate against the CA.
func ClientTLSConfig(c MTLSConfig) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: load client keypair: %w", err)
	}
	pool, err := loadCertPool(c.CAFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
