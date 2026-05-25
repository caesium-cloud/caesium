package dispatch

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMTLSConfig_Configured(t *testing.T) {
	require.True(t, MTLSConfig{CAFile: "ca", CertFile: "c", KeyFile: "k"}.Configured())
	require.False(t, MTLSConfig{CertFile: "c", KeyFile: "k"}.Configured(), "missing CA")
	require.False(t, MTLSConfig{CAFile: "ca", KeyFile: "k"}.Configured(), "missing cert")
	require.False(t, MTLSConfig{CAFile: "ca", CertFile: "c"}.Configured(), "missing key")
	require.False(t, MTLSConfig{}.Configured())
}

// genCA returns a self-signed CA certificate + key and its PEM encoding.
func genCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// genLeaf signs a leaf certificate (server or client) with the given CA.
func genLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, ips []net.IP) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, data, 0o600))
	return p
}

// materialFromCA writes a CA + a CA-signed leaf to temp files and returns an
// MTLSConfig pointing at them.  loopback adds 127.0.0.1 as a SAN for server use.
func materialFromCA(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, name string, loopback bool) MTLSConfig {
	t.Helper()
	dir := t.TempDir()
	var ips []net.IP
	if loopback {
		ips = []net.IP{net.ParseIP("127.0.0.1")}
	}
	certPEM, keyPEM := genLeaf(t, ca, caKey, name, ips)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})
	return MTLSConfig{
		CAFile:   writeFile(t, dir, "ca.pem", caPEM),
		CertFile: writeFile(t, dir, name+".pem", certPEM),
		KeyFile:  writeFile(t, dir, name+"-key.pem", keyPEM),
	}
}

// leafDER signs a leaf certificate with the given CA and returns its DER bytes.
func leafDER(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey) []byte {
	t.Helper()
	certPEM, _ := genLeaf(t, ca, caKey, "leaf", nil)
	block, _ := pem.Decode(certPEM)
	require.NotNil(t, block)
	return block.Bytes
}

// TestVerifyChainAgainst checks the client's CA-chain verification (used in place
// of the default verification that InsecureSkipVerify disables): a CA-signed
// cert is accepted, an untrusted-CA cert and an empty chain are rejected.
func TestVerifyChainAgainst(t *testing.T) {
	ca1, ca1Key, _ := genCA(t)
	ca2, ca2Key, _ := genCA(t)
	pool := x509.NewCertPool()
	pool.AddCert(ca1)
	verify := verifyChainAgainst(pool)

	require.NoError(t, verify([][]byte{leafDER(t, ca1, ca1Key)}, nil),
		"a cert signed by the trusted CA must verify")
	require.Error(t, verify([][]byte{leafDER(t, ca2, ca2Key)}, nil),
		"a cert signed by an untrusted CA must be rejected")
	require.Error(t, verify(nil, nil), "an empty chain must be rejected")
}

// TestClientTLSConfig_SkipsHostname proves the client accepts a CA-signed server
// certificate even when the dialed address is absent from the cert's SANs — the
// behavior dynamic pod IPs require.
func TestClientTLSConfig_SkipsHostname(t *testing.T) {
	ca, caKey, _ := genCA(t)
	// Server cert deliberately has NO 127.0.0.1 SAN (loopback=false).
	serverMat := materialFromCA(t, ca, caKey, "server-no-san", false)
	clientMat := materialFromCA(t, ca, caKey, "client", false)
	clientMat.CAFile = serverMat.CAFile

	serverTLS, err := ServerTLSConfig(serverMat)
	require.NoError(t, err)
	clientTLS, err := ClientTLSConfig(clientMat)
	require.NoError(t, err)
	clientTLS.MaxVersion = tls.VersionTLS12

	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if tc, ok := c.(*tls.Conn); ok {
				_ = tc.HandshakeContext(context.Background())
			}
			_ = c.Close()
		}
	}()

	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 3 * time.Second}, Config: clientTLS}
	conn, err := d.DialContext(context.Background(), "tcp", ln.Addr().String())
	require.NoError(t, err, "client must accept a CA-signed server cert despite the dialed host being absent from its SANs")
	_ = conn.Close()
}

func TestServerTLSConfig_MissingFiles(t *testing.T) {
	_, err := ServerTLSConfig(MTLSConfig{CAFile: "/nope/ca", CertFile: "/nope/c", KeyFile: "/nope/k"})
	require.Error(t, err)
	_, err = ClientTLSConfig(MTLSConfig{CAFile: "/nope/ca", CertFile: "/nope/c", KeyFile: "/nope/k"})
	require.Error(t, err)
}

// TestInternalMTLS_Handshake stands up a TLS listener with ServerTLSConfig and
// asserts the handshake accepts a client cert from the trusted CA, and rejects
// both a client presenting no certificate and one signed by a different CA.
func TestInternalMTLS_Handshake(t *testing.T) {
	ca, caKey, caPEM := genCA(t)
	serverMat := materialFromCA(t, ca, caKey, "server", true)
	clientMat := materialFromCA(t, ca, caKey, "client", false)
	// Point the client's CA file at the same CA so it trusts the server cert.
	clientMat.CAFile = serverMat.CAFile

	serverTLS, err := ServerTLSConfig(serverMat)
	require.NoError(t, err)
	clientTLS, err := ClientTLSConfig(clientMat)
	require.NoError(t, err)

	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	require.NoError(t, err)
	defer ln.Close()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Force the handshake so client-cert verification runs, then close.
			if tc, ok := c.(*tls.Conn); ok {
				_ = tc.HandshakeContext(context.Background())
			}
			_ = c.Close()
		}
	}()

	addr := ln.Addr().String()
	ctx := context.Background()
	dial := func(cfg *tls.Config) (net.Conn, error) {
		d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 3 * time.Second}, Config: cfg}
		return d.DialContext(ctx, "tcp", addr)
	}

	// Pin TLS 1.2 on the dialing side: under TLS 1.3 a client-cert rejection is
	// delivered as a post-handshake alert (surfacing on first I/O, not at Dial),
	// which makes the negative assertions racy.  The enforcement mechanism
	// (RequireAndVerifyClientCert) is version-independent; 1.2 just makes the
	// rejection synchronous at handshake so the test can assert it directly.
	clientTLS.MaxVersion = tls.VersionTLS12

	// Valid client certificate → handshake succeeds.
	conn, err := dial(clientTLS)
	require.NoError(t, err, "valid client cert should be accepted")
	_ = conn.Close()

	// No client certificate → server requires one, handshake fails.
	noCert := &tls.Config{RootCAs: clientTLS.RootCAs, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12}
	_, err = dial(noCert)
	require.Error(t, err, "absent client cert must be rejected")

	// Client certificate signed by a DIFFERENT CA → verification fails.
	otherCA, otherKey, _ := genCA(t)
	otherMat := materialFromCA(t, otherCA, otherKey, "intruder", false)
	otherMat.CAFile = serverMat.CAFile // still trust the real server
	intruderTLS, err := ClientTLSConfig(otherMat)
	require.NoError(t, err)
	intruderTLS.MaxVersion = tls.VersionTLS12
	_, err = dial(intruderTLS)
	require.Error(t, err, "client cert from an untrusted CA must be rejected")

	_ = caPEM // (CA PEM is written via materialFromCA; retained for clarity)
}
