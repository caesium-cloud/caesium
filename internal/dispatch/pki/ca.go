package pki

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
)

const (
	caCommonName = "caesium-internal-ca"
	leafBackdate = 5 * time.Minute
)

const (
	EnrollmentStatusPending  = "pending"
	EnrollmentStatusSigned   = "signed"
	EnrollmentStatusRejected = "rejected"
)

// NewCAGeneration creates a sealed catalog CA generation row.
func NewCAGeneration(generation int, kek []byte, now time.Time, ttl time.Duration) (*models.InternalCAGeneration, *x509.Certificate, error) {
	if generation < 1 {
		return nil, nil, fmt.Errorf("pki: CA generation must be positive")
	}
	certPEM, keyPEM, cert, err := GenerateCA(now, ttl)
	if err != nil {
		return nil, nil, err
	}
	ciphertext, nonce, err := SealCAKey(kek, keyPEM)
	if err != nil {
		return nil, nil, err
	}
	return &models.InternalCAGeneration{
		Generation:    generation,
		CertPEM:       string(certPEM),
		KeyCiphertext: ciphertext,
		KeyNonce:      nonce,
		NotBefore:     cert.NotBefore,
		NotAfter:      cert.NotAfter,
		CreatedAt:     now.UTC(),
	}, cert, nil
}

// GenerateCA creates a self-signed Caesium internal CA certificate and key.
func GenerateCA(now time.Time, ttl time.Duration) (certPEM, keyPEM []byte, cert *x509.Certificate, err error) {
	if ttl <= 0 {
		return nil, nil, nil, fmt.Errorf("pki: CA TTL must be positive")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("pki: generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: caCommonName},
		NotBefore:             now.UTC(),
		NotAfter:              now.Add(ttl).UTC(),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("pki: create CA certificate: %w", err)
	}
	cert, err = x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("pki: parse generated CA certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("pki: marshal CA key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		cert,
		nil
}

// LeafRequest holds a locally generated private key and CSR. KeyPEM must never
// be written to the catalog.
type LeafRequest struct {
	CSRPEM []byte
	CSRDER []byte
	KeyPEM []byte
}

// GenerateLeafRequest creates a node-local leaf keypair and CSR.
func GenerateLeafRequest(nodeID string) (LeafRequest, error) {
	if nodeID == "" {
		return LeafRequest{}, fmt.Errorf("pki: node ID is required")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return LeafRequest{}, fmt.Errorf("pki: generate leaf key: %w", err)
	}
	csrTmpl := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: nodeID},
	}
	host, ips, dns := nodeAddressNames(nodeID)
	csrTmpl.DNSNames = dns
	csrTmpl.IPAddresses = ips
	if len(csrTmpl.DNSNames) == 0 && len(csrTmpl.IPAddresses) == 0 && host != "" {
		csrTmpl.DNSNames = []string{host}
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTmpl, key)
	if err != nil {
		return LeafRequest{}, fmt.Errorf("pki: create leaf CSR: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return LeafRequest{}, fmt.Errorf("pki: marshal leaf key: %w", err)
	}
	return LeafRequest{
		CSRPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}),
		CSRDER: csrDER,
		KeyPEM: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// CSRMac returns HMAC-SHA256(csr-mac-key, csrDER).
func CSRMac(macKey, csrDER []byte) []byte {
	mac := hmac.New(sha256.New, macKey)
	_, _ = mac.Write(csrDER)
	return mac.Sum(nil)
}

// VerifyCSRMac checks a CSR HMAC using hmac.Equal.
func VerifyCSRMac(macKey, csrDER, got []byte) bool {
	expected := CSRMac(macKey, csrDER)
	return hmac.Equal(expected, got)
}

// SignCSR verifies csrPEM and signs a constrained non-CA leaf for nodeID. The
// resulting certificate uses only the CSR public key and the validated node
// identity; CSR-requested extensions are intentionally ignored.
func SignCSR(csrPEM []byte, nodeID string, caCert *x509.Certificate, caKey crypto.Signer, now time.Time, ttl time.Duration) ([]byte, *x509.Certificate, error) {
	if nodeID == "" {
		return nil, nil, fmt.Errorf("pki: node ID is required")
	}
	if caCert == nil || caKey == nil {
		return nil, nil, fmt.Errorf("pki: CA certificate and key are required")
	}
	if ttl <= 0 {
		return nil, nil, fmt.Errorf("pki: leaf TTL must be positive")
	}
	csr, err := ParseCSRPEM(csrPEM)
	if err != nil {
		return nil, nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, nil, fmt.Errorf("pki: CSR signature invalid: %w", err)
	}
	if csr.Subject.CommonName != nodeID {
		return nil, nil, fmt.Errorf("pki: CSR common name %q does not match node ID %q", csr.Subject.CommonName, nodeID)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	host, ips, dns := nodeAddressNames(nodeID)
	if len(dns) == 0 && len(ips) == 0 && host != "" {
		dns = []string{host}
	}
	notAfter := now.Add(ttl).UTC()
	if caCert.NotAfter.Before(notAfter) {
		notAfter = caCert.NotAfter.UTC()
	}
	notBefore := now.Add(-leafBackdate).UTC()
	if !notAfter.After(notBefore) {
		return nil, nil, fmt.Errorf("pki: CA generation expires before requested leaf validity")
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: nodeID},
		DNSNames:              dns,
		IPAddresses:           ips,
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, csr.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("pki: sign leaf certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("pki: parse signed leaf certificate: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), cert, nil
}

// TrustPoolFromGenerations builds the union trust pool from every non-expired
// CA generation.
func TrustPoolFromGenerations(gens []models.InternalCAGeneration, now time.Time) (*x509.CertPool, []*x509.Certificate, error) {
	pool := x509.NewCertPool()
	certs := make([]*x509.Certificate, 0, len(gens))
	for _, gen := range gens {
		if !gen.NotAfter.After(now) {
			continue
		}
		parsed, err := ParseCertificatesPEM([]byte(gen.CertPEM))
		if err != nil {
			return nil, nil, fmt.Errorf("pki: parse CA generation %d: %w", gen.Generation, err)
		}
		for _, cert := range parsed {
			if !cert.IsCA {
				return nil, nil, fmt.Errorf("pki: generation %d certificate is not a CA", gen.Generation)
			}
			pool.AddCert(cert)
			certs = append(certs, cert)
		}
	}
	if len(certs) == 0 {
		return nil, nil, fmt.Errorf("pki: no non-expired CA generations")
	}
	return pool, certs, nil
}

// ParseCSRPEM parses a single PEM-encoded certificate request.
func ParseCSRPEM(data []byte) (*x509.CertificateRequest, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("pki: no certificate request PEM block found")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("pki: trailing data after certificate request PEM")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pki: parse CSR: %w", err)
	}
	return csr, nil
}

// ParseCertificatesPEM parses one or more PEM certificates.
func ParseCertificatesPEM(data []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse certificate: %w", err)
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("no certificate PEM blocks found")
	}
	return certs, nil
}

// ParsePrivateKeyPEM parses an ECDSA or PKCS#8 private key PEM.
func ParsePrivateKeyPEM(data []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("pki: no private key PEM block found")
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pki: parse private key: %w", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("pki: private key does not implement crypto.Signer")
	}
	return signer, nil
}

func randomSerial() (*big.Int, error) {
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, fmt.Errorf("pki: generate certificate serial: %w", err)
	}
	return serial, nil
}

func nodeAddressNames(nodeID string) (host string, ips []net.IP, dns []string) {
	host = nodeID
	if h, _, err := net.SplitHostPort(nodeID); err == nil {
		host = h
	}
	if ip := net.ParseIP(host); ip != nil {
		return host, []net.IP{ip}, nil
	}
	if host != "" {
		return host, nil, []string{host}
	}
	return "", nil, nil
}
