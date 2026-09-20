//go:build integration

package cluster

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	dispatchpki "github.com/caesium-cloud/caesium/internal/dispatch/pki"
)

const InternalPort = 8443

// InternalClient talks to /internal/dispatch and /internal/complete on the
// dedicated mTLS listener. The bearer token and leaf key stay in memory.
type InternalClient struct {
	HTTP  *http.Client
	Token string
	Kind  string // "valid-leaf", "wrong-token", "invalid-cert"
}

func InternalBase(ip string) string {
	return fmt.Sprintf("https://%s:%d", ip, InternalPort)
}

// MintInternalClient builds a client presenting a leaf signed by the cluster
// CA (the same provisioning path the members use). The catalog CA key is
// unsealed with the shared test token and never written to records.
func MintInternalClient(ctx context.Context, h *HTTP, base, token, bearer string) (*InternalClient, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("inconclusive: empty internal token")
	}
	resp, _, err := h.Query(ctx, base,
		"SELECT generation, cert_pem, key_ciphertext, key_nonce FROM internal_ca_generations ORDER BY generation DESC", 1)
	if err != nil {
		return nil, fmt.Errorf("inconclusive: read internal CA: %w", err)
	}
	if len(resp.Rows) == 0 || len(resp.Rows[0]) < 4 {
		return nil, fmt.Errorf("inconclusive: no internal_ca_generations row")
	}
	row := resp.Rows[0]
	caPEM := []byte(fmt.Sprint(row[1]))
	ciphertext, err := DecodeBlob(row[2])
	if err != nil {
		return nil, fmt.Errorf("inconclusive: CA ciphertext: %w", err)
	}
	nonce, err := DecodeBlob(row[3])
	if err != nil {
		return nil, fmt.Errorf("inconclusive: CA nonce: %w", err)
	}
	keys, err := dispatchpki.DeriveKeys(token)
	if err != nil {
		return nil, err
	}
	keyPEM, err := dispatchpki.OpenCAKey(keys.CAKEK, ciphertext, nonce)
	if err != nil {
		return nil, fmt.Errorf("inconclusive: open CA key: %w", err)
	}
	caCerts, err := dispatchpki.ParseCertificatesPEM(caPEM)
	if err != nil || len(caCerts) == 0 {
		return nil, fmt.Errorf("inconclusive: parse CA cert: %w", err)
	}
	caKey, err := dispatchpki.ParsePrivateKeyPEM(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("inconclusive: parse CA key: %w", err)
	}
	leafReq, err := dispatchpki.GenerateLeafRequest("robustness-runner:9001")
	if err != nil {
		return nil, err
	}
	leafPEM, _, err := dispatchpki.SignCSR(leafReq.CSRPEM, "robustness-runner:9001", caCerts[0], caKey, time.Now().UTC(), time.Hour)
	if err != nil {
		return nil, fmt.Errorf("sign robustness leaf: %w", err)
	}
	cert, err := tls.X509KeyPair(leafPEM, leafReq.KeyPEM)
	if err != nil {
		return nil, err
	}
	pool, _, err := dispatchpki.CertPool(caPEM)
	if err != nil {
		return nil, err
	}
	kind := "valid-leaf"
	if bearer != token {
		kind = "wrong-token"
	}
	return &InternalClient{
		HTTP:  newInternalHTTP(cert, pool, true),
		Token: bearer,
		Kind:  kind,
	}, nil
}

// InvalidCertClient presents a self-signed certificate the cluster CA did not
// issue. The server must fail the handshake before any handler runs.
func InvalidCertClient(ctx context.Context, h *HTTP, base string) (*InternalClient, error) {
	resp, _, err := h.Query(ctx, base,
		"SELECT cert_pem FROM internal_ca_generations ORDER BY generation DESC", 1)
	if err != nil {
		return nil, fmt.Errorf("inconclusive: read CA for invalid-peer client: %w", err)
	}
	if len(resp.Rows) == 0 || len(resp.Rows[0]) == 0 {
		return nil, fmt.Errorf("inconclusive: no CA cert to verify the server")
	}
	caPEM := []byte(fmt.Sprint(resp.Rows[0][0]))
	pool, _, err := dispatchpki.CertPool(caPEM)
	if err != nil {
		return nil, err
	}
	cert, err := selfSignedClientCert()
	if err != nil {
		return nil, err
	}
	return &InternalClient{
		HTTP:  newInternalHTTP(cert, pool, true),
		Token: "not-the-cluster-token",
		Kind:  "invalid-cert",
	}, nil
}

func newInternalHTTP(cert tls.Certificate, pool *x509.CertPool, verifyServer bool) *http.Client {
	cfg := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // hostname skipped for pod IPs; chain verified below
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &cert, nil
		},
	}
	if verifyServer && pool != nil {
		cfg.RootCAs = pool
		cfg.VerifyPeerCertificate = verifyServerChain(pool)
	}
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: cfg,
		},
	}
}

func verifyServerChain(pool *x509.CertPool) func([][]byte, [][]*x509.Certificate) error {
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
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}); err != nil {
			return fmt.Errorf("mtls: peer certificate not signed by trusted CA: %w", err)
		}
		return nil
	}
}

func selfSignedClientCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "robustness-invalid-peer"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return tls.X509KeyPair(certPEM, keyPEM)
}

type InternalExchange struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	Body   string `json:"body"`
	Err    string `json:"err,omitempty"`
	Kind   string `json:"kind"`
}

func (c *InternalClient) Post(ctx context.Context, base, path string, payload any) InternalExchange {
	ex := InternalExchange{Method: http.MethodPost, Path: path, Kind: c.Kind}
	var rdr io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			ex.Err = err.Error()
			return ex
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+path, rdr)
	if err != nil {
		ex.Err = err.Error()
		return ex
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(c.Token) != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		ex.Err = err.Error()
		return ex
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	ex.Status = resp.StatusCode
	ex.Body = string(raw)
	return ex
}

func (c *InternalClient) Complete(ctx context.Context, base string, payload map[string]any) InternalExchange {
	return c.Post(ctx, base, "/internal/complete", payload)
}

func (c *InternalClient) Dispatch(ctx context.Context, base string, payload map[string]any) InternalExchange {
	return c.Post(ctx, base, "/internal/dispatch", payload)
}
