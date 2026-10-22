package ldap

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

// testCA returns a self-signed CA certificate (PEM) and a server leaf for
// host signed by it.
func testCA(t *testing.T, host string) ([]byte, tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ldap test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}
}

func TestNormalizeCAFileVerifiesTheDirectory(t *testing.T) {
	caPEM, leaf := testCA(t, "dir.example.test")
	path := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(path, caPEM, 0o600))

	cfg := baseConfig()
	cfg.URL = "ldaps://dir.example.test:636"
	cfg.CAFile = "  " + path + "  "
	normalized, err := cfg.normalize()
	require.NoError(t, err)
	require.NotNil(t, normalized.TLSConfig)
	require.NotNil(t, normalized.TLSConfig.RootCAs)
	tlsCfg := normalized.tlsConfig()
	require.False(t, tlsCfg.InsecureSkipVerify)
	require.Equal(t, "dir.example.test", tlsCfg.ServerName)
	require.GreaterOrEqual(t, tlsCfg.MinVersion, uint16(tls.VersionTLS12))

	// A handshake against a server presenting the CA-signed leaf succeeds,
	// and one for another name fails: the CA adds trust, never skips checks.
	handshake := func(cfg *tls.Config, serverName string) error {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer ln.Close()
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			_ = tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12}).HandshakeContext(ctx)
		}()
		conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", ln.Addr().String())
		require.NoError(t, err)
		defer conn.Close()
		clientCfg := cfg.Clone()
		clientCfg.ServerName = serverName
		return tls.Client(conn, clientCfg).HandshakeContext(ctx)
	}
	require.NoError(t, handshake(tlsCfg, "dir.example.test"))
	require.Error(t, handshake(tlsCfg, "other.example.test"))

	// Without the CA file the system roots do not know the fixture CA.
	plain := baseConfig()
	plain.URL = "ldaps://dir.example.test:636"
	plainNormalized, err := plain.normalize()
	require.NoError(t, err)
	require.Error(t, handshake(plainNormalized.tlsConfig(), "dir.example.test"))
}

func TestNormalizeRejectsUnusableCAFile(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.pem")
	require.NoError(t, os.WriteFile(empty, []byte("not a certificate"), 0o600))
	for _, path := range []string{filepath.Join(dir, "missing.pem"), empty} {
		cfg := baseConfig()
		cfg.CAFile = path
		_, err := cfg.normalize()
		require.ErrorIs(t, err, ErrInvalidConfig, path)
	}
}
