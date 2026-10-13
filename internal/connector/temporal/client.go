// Package temporal is the optional Temporal observation adapter.
// It compiles go.temporal.io/sdk v1.49.0. With connectors left disabled, nothing
// in this package is dialed. Queries, Updates, receipts, and HTTP routes are
// later items and are not implemented here.
package temporal

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"strings"

	"github.com/caesium-cloud/caesium/internal/connector"
	"go.temporal.io/sdk/client"
)

// minRedactedSecretLen matches the connector config redactor. Shorter values
// occur inside ordinary error text and are not stripped.
const minRedactedSecretLen = 8

// DialConfig is the transport for one connection. Endpoint is host:port.
// Namespace is the connection scope. APIKey is optional and is never logged.
// CertificatePaths are classified by base name only.
type DialConfig struct {
	Endpoint         string
	Namespace        string
	APIKey           string
	CertificatePaths []string
}

// ClientOptions builds SDK dial options without dialing.
// Plaintext stays loopback-only: a nil TLS config, with TLSDisabled, is used
// only for localhost, 127.0.0.1, or ::1 when no certificate path is set.
// TLSDisabled keeps the SDK from turning an API key into TLS at dial time.
// Any other host uses TLS. Empty certificate paths then mean the system
// roots, not plaintext.
func ClientOptions(cfg DialConfig) (client.Options, error) {
	secrets := []string{cfg.APIKey}
	opts, err := buildClientOptions(cfg, &secrets)
	if err != nil {
		return client.Options{}, redactError(err, secrets)
	}
	return opts, nil
}

// Dial dials with ClientOptions. Callers that only need observation can pass
// the resulting client's workflow service to NewObserver.
func Dial(ctx context.Context, cfg DialConfig) (client.Client, error) {
	opts, err := ClientOptions(cfg)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c, err := client.DialContext(ctx, opts)
	if err != nil {
		return nil, redactError(err, []string{cfg.APIKey})
	}
	return c, nil
}

func buildClientOptions(cfg DialConfig, secrets *[]string) (client.Options, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return client.Options{}, errors.New("temporal endpoint is required")
	}
	if strings.TrimSpace(cfg.Namespace) == "" {
		return client.Options{}, errors.New("temporal namespace is required")
	}
	loopback, err := loopbackHost(cfg.Endpoint)
	if err != nil {
		return client.Options{}, err
	}
	opts := client.Options{
		HostPort:  cfg.Endpoint,
		Namespace: cfg.Namespace,
		Logger:    discardLog{},
	}
	if cfg.APIKey != "" {
		opts.Credentials = client.NewAPIKeyStaticCredentials(cfg.APIKey)
	}
	if loopback && len(cfg.CertificatePaths) == 0 {
		opts.ConnectionOptions.TLS = nil
		opts.ConnectionOptions.TLSDisabled = true
		return opts, nil
	}
	tlsConfig, err := tlsConfigFor(cfg.CertificatePaths, secrets)
	if err != nil {
		return client.Options{}, err
	}
	opts.ConnectionOptions.TLS = tlsConfig
	return opts, nil
}

func loopbackHost(endpoint string) (bool, error) {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil || host == "" {
		return false, errors.New("temporal endpoint must be host:port")
	}
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true, nil
	default:
		return false, nil
	}
}

func tlsConfigFor(paths []string, secrets *[]string) (*tls.Config, error) {
	var cas, certs, keys []string
	for _, certPath := range paths {
		switch path.Base(certPath) {
		case "ca.crt", "ca.pem":
			cas = append(cas, certPath)
		case "tls.crt", "client.crt":
			certs = append(certs, certPath)
		case "tls.key", "client.key":
			keys = append(keys, certPath)
		default:
			return nil, fmt.Errorf("unrecognized certificate file %q", path.Base(certPath))
		}
	}
	if len(certs) > 1 || len(keys) > 1 {
		return nil, errors.New("only one client certificate and one key are allowed")
	}
	if len(certs) != len(keys) {
		return nil, errors.New("client certificate and key must both be set")
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if len(cas) > 0 {
		pool := x509.NewCertPool()
		for _, certPath := range cas {
			data, err := readSecretFile(certPath, secrets)
			if err != nil {
				return nil, err
			}
			if !pool.AppendCertsFromPEM(data) {
				return nil, fmt.Errorf("certificate %s contained no PEM certificates", path.Base(certPath))
			}
		}
		cfg.RootCAs = pool
	}
	if len(certs) == 1 {
		certPEM, err := readSecretFile(certs[0], secrets)
		if err != nil {
			return nil, err
		}
		keyPEM, err := readSecretFile(keys[0], secrets)
		if err != nil {
			return nil, err
		}
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("client certificate %s: %w", path.Base(certs[0]), err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, nil
}

func readSecretFile(certPath string, secrets *[]string) ([]byte, error) {
	data, err := os.ReadFile(certPath)
	if len(data) >= minRedactedSecretLen {
		*secrets = append(*secrets, string(data))
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

func redactError(err error, secrets []string) error {
	return connector.RedactError(err, secrets, nil)
}

type discardLog struct{}

func (discardLog) Debug(string, ...any) {}
func (discardLog) Info(string, ...any)  {}
func (discardLog) Warn(string, ...any)  {}
func (discardLog) Error(string, ...any) {}
