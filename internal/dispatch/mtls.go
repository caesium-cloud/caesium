package dispatch

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"

	dispatchpki "github.com/caesium-cloud/caesium/internal/dispatch/pki"
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

// ServerTLSConfig builds the *tls.Config for the internal listener: it presents
// this node's certificate and requires + verifies a client certificate signed
// by the configured CA on every connection.  A peer with no certificate, or one
// signed by an unknown CA, fails the TLS handshake before reaching a handler.
func ServerTLSConfig(c MTLSConfig) (*tls.Config, error) {
	holder, err := dispatchpki.NewStaticMaterialHolder(c.CAFile, c.CertFile, c.KeyFile)
	if err != nil {
		return nil, err
	}
	return ServerTLSConfigFromHolder(holder)
}

func ServerTLSConfigFromHolder(holder *dispatchpki.MaterialHolder) (*tls.Config, error) {
	if _, ok := holder.Material(); !ok {
		return nil, fmt.Errorf("mtls: TLS material not initialized")
	}
	pool, err := holder.CertPool()
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		ClientCAs:  pool,
		ClientAuth: tls.RequireAndVerifyClientCert,
		MinVersion: tls.VersionTLS12,
	}
	cfg.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		return holder.Certificate()
	}
	cfg.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		pool, err := holder.CertPool()
		if err != nil {
			return nil, err
		}
		next := cfg.Clone()
		next.ClientCAs = pool
		next.GetConfigForClient = nil
		return next, nil
	}
	return cfg, nil
}

// ClientTLSConfig builds the *tls.Config used when this node POSTs to a peer's
// internal endpoints (PostDispatch / PostComplete): it presents this node's
// certificate and verifies the peer's server certificate was signed by the CA.
//
// Hostname verification is deliberately disabled: cluster peers are reached by
// dynamic pod IPs / node addresses that a long-lived certificate can't enumerate
// in its SANs, so the built-in name check would reject valid peers.  Peer
// identity instead rests on (a) the certificate being signed by the shared
// internal CA, verified here against the chain, and (b) the application-layer
// owner-generation + worker-node fence on every dispatch/complete.  We therefore
// set InsecureSkipVerify (which only disables the name/standard verification)
// and re-implement chain verification in VerifyPeerCertificate.
func ClientTLSConfig(c MTLSConfig) (*tls.Config, error) {
	holder, err := dispatchpki.NewStaticMaterialHolder(c.CAFile, c.CertFile, c.KeyFile)
	if err != nil {
		return nil, err
	}
	return ClientTLSConfigFromHolder(holder)
}

func ClientTLSConfigFromHolder(holder *dispatchpki.MaterialHolder) (*tls.Config, error) {
	if _, ok := holder.Material(); !ok {
		return nil, fmt.Errorf("mtls: TLS material not initialized")
	}
	pool, err := holder.CertPool()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		RootCAs:            pool,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // chain verified below; hostname intentionally skipped for dynamic pod IPs
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return holder.Certificate()
		},
		VerifyPeerCertificate: verifyChainAgainstHolder(holder),
	}, nil
}

func verifyChainAgainstHolder(holder *dispatchpki.MaterialHolder) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
		pool, err := holder.CertPool()
		if err != nil {
			return err
		}
		return verifyChainAgainst(pool)(rawCerts, verifiedChains)
	}
}

// verifyChainAgainst returns a VerifyPeerCertificate callback that validates the
// peer's presented certificate chains to the trusted CA pool, without any
// hostname/DNS check.  Used by ClientTLSConfig in place of the default
// verification that InsecureSkipVerify disables.
func verifyChainAgainst(pool *x509.CertPool) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("mtls: peer presented no certificate")
		}
		certs := make([]*x509.Certificate, 0, len(rawCerts))
		for _, raw := range rawCerts {
			crt, err := x509.ParseCertificate(raw)
			if err != nil {
				return fmt.Errorf("mtls: parse peer certificate: %w", err)
			}
			certs = append(certs, crt)
		}
		intermediates := x509.NewCertPool()
		for _, crt := range certs[1:] {
			intermediates.AddCert(crt)
		}
		if _, err := certs[0].Verify(x509.VerifyOptions{
			Roots:         pool,
			Intermediates: intermediates,
			// We are verifying the peer as a SERVER (this is the client side):
			// require serverAuth so a client-only certificate can't be presented
			// as a server.  Without this, VerifyOptions defaults to
			// ExtKeyUsageAny and would accept it.
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}); err != nil {
			return fmt.Errorf("mtls: peer certificate not signed by trusted CA or not valid for server auth: %w", err)
		}
		return nil
	}
}
