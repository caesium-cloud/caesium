package saml

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/pkg/env"
	crewsaml "github.com/crewjam/saml"
	"github.com/crewjam/saml/samlsp"
)

const (
	// ProviderName is the auth method reported by the SAML redirect provider.
	ProviderName = "saml"

	// DefaultGroupsAttribute is the envconfig default for
	// CAESIUM_AUTH_SAML_GROUPS_ATTRIBUTE.
	DefaultGroupsAttribute = "groups"

	DefaultACSPath      = "/auth/sso/saml/acs"
	DefaultMetadataPath = "/auth/sso/saml/metadata"

	DefaultStateCookieName = "caesium_saml_state"
	DefaultStateTTL        = 10 * time.Minute

	DefaultMetadataFetchTimeout = 30 * time.Second
)

var defaultMetadataHTTPClient = &http.Client{Timeout: DefaultMetadataFetchTimeout}

// Config configures the SAML redirect provider.
type Config struct {
	IDPMetadataURL  string
	IDPMetadataXML  string
	IDPMetadataFile string

	SPEntityID  string
	SPCertPath  string
	SPKeyPath   string
	ACSURL      string
	MetadataURL string

	PublicBaseURL   string
	GroupsAttribute string

	StateCookieName string
	StateTTL        time.Duration
	CookieSecure    bool
	CookieSecret    []byte

	HTTPClient  *http.Client
	ReplayCache AssertionReplayCache
}

// ConfigFromEnv converts Caesium environment config into provider config. The
// caller must still attach a ReplayCache backed by the catalog DB before
// constructing the provider.
func ConfigFromEnv(vars env.Environment) Config {
	return Config{
		IDPMetadataURL:  vars.AuthSAMLIDPMetadataURL,
		IDPMetadataXML:  vars.AuthSAMLIDPMetadataXML,
		IDPMetadataFile: vars.AuthSAMLIDPMetadataFile,
		SPEntityID:      vars.AuthSAMLSPEntityID,
		SPCertPath:      vars.AuthSAMLSPCert,
		SPKeyPath:       vars.AuthSAMLSPKey,
		ACSURL:          vars.AuthSAMLACSURL,
		MetadataURL:     vars.AuthSAMLMetadataURL,
		PublicBaseURL:   vars.AuthPublicBaseURL,
		GroupsAttribute: vars.AuthSAMLGroupsAttribute,
		CookieSecure:    vars.AuthRequireTLS,
		CookieSecret:    []byte(vars.AuthKeyHashSecret),
	}
}

func (c Config) normalize() (Config, error) {
	c.IDPMetadataURL = strings.TrimSpace(c.IDPMetadataURL)
	c.IDPMetadataXML = strings.TrimSpace(c.IDPMetadataXML)
	c.IDPMetadataFile = strings.TrimSpace(c.IDPMetadataFile)
	c.SPEntityID = strings.TrimSpace(c.SPEntityID)
	c.SPCertPath = strings.TrimSpace(c.SPCertPath)
	c.SPKeyPath = strings.TrimSpace(c.SPKeyPath)
	c.ACSURL = strings.TrimSpace(c.ACSURL)
	c.MetadataURL = strings.TrimSpace(c.MetadataURL)
	c.PublicBaseURL = strings.TrimSpace(c.PublicBaseURL)
	c.GroupsAttribute = strings.TrimSpace(c.GroupsAttribute)
	c.StateCookieName = strings.TrimSpace(c.StateCookieName)

	sources := 0
	if c.IDPMetadataURL != "" {
		sources++
		if _, err := parseHTTPSURL(c.IDPMetadataURL, "saml idp metadata URL"); err != nil {
			return c, err
		}
	}
	if c.IDPMetadataXML != "" {
		sources++
	}
	if c.IDPMetadataFile != "" {
		sources++
	}
	switch sources {
	case 0:
		return c, fmt.Errorf("saml idp metadata URL, XML, or file is required")
	case 1:
	default:
		return c, fmt.Errorf("configure only one saml idp metadata source")
	}

	if c.GroupsAttribute == "" {
		c.GroupsAttribute = DefaultGroupsAttribute
	}
	if c.StateCookieName == "" {
		c.StateCookieName = DefaultStateCookieName
	}
	if c.StateTTL <= 0 {
		c.StateTTL = DefaultStateTTL
	}

	if c.ACSURL == "" {
		acsURL, err := deriveServiceURL(c.PublicBaseURL, DefaultACSPath, "saml ACS URL")
		if err != nil {
			return c, err
		}
		c.ACSURL = acsURL
	}
	if c.MetadataURL == "" {
		metadataURL, err := deriveServiceURL(c.PublicBaseURL, DefaultMetadataPath, "saml metadata URL")
		if err != nil {
			return c, err
		}
		c.MetadataURL = metadataURL
	}
	if _, err := parseHTTPURL(c.ACSURL, "saml ACS URL"); err != nil {
		return c, err
	}
	if _, err := parseHTTPURL(c.MetadataURL, "saml metadata URL"); err != nil {
		return c, err
	}
	if c.SPEntityID == "" {
		c.SPEntityID = c.MetadataURL
	}

	if (c.SPCertPath == "") != (c.SPKeyPath == "") {
		return c, fmt.Errorf("saml SP cert and key must be configured together")
	}
	if len(c.CookieSecret) == 0 {
		return c, fmt.Errorf("saml cookie secret is required; set AUTH_KEY_HASH_SECRET")
	}
	if c.ReplayCache == nil {
		return c, fmt.Errorf("saml assertion replay cache is required")
	}
	return c, nil
}

func loadIDPMetadata(ctx context.Context, cfg Config) (*crewsaml.EntityDescriptor, error) {
	switch {
	case cfg.IDPMetadataURL != "":
		metadataURL, err := parseHTTPSURL(cfg.IDPMetadataURL, "saml idp metadata URL")
		if err != nil {
			return nil, err
		}
		client := cfg.HTTPClient
		if client == nil {
			client = defaultMetadataHTTPClient
		}
		metadata, err := samlsp.FetchMetadata(ctx, client, *metadataURL)
		if err != nil {
			return nil, fmt.Errorf("fetch saml idp metadata: %w", err)
		}
		return metadata, nil
	case cfg.IDPMetadataXML != "":
		metadata, err := samlsp.ParseMetadata([]byte(cfg.IDPMetadataXML))
		if err != nil {
			return nil, fmt.Errorf("parse saml idp metadata XML: %w", err)
		}
		return metadata, nil
	case cfg.IDPMetadataFile != "":
		data, err := os.ReadFile(cfg.IDPMetadataFile)
		if err != nil {
			return nil, fmt.Errorf("read saml idp metadata file: %w", err)
		}
		metadata, err := samlsp.ParseMetadata(data)
		if err != nil {
			return nil, fmt.Errorf("parse saml idp metadata file: %w", err)
		}
		return metadata, nil
	default:
		return nil, fmt.Errorf("saml idp metadata is required")
	}
}

func loadKeyPair(certPath, keyPath string) (*x509.Certificate, crypto.Signer, error) {
	if certPath == "" && keyPath == "" {
		return nil, nil, nil
	}

	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load saml SP key pair: %w", err)
	}
	if len(pair.Certificate) == 0 {
		return nil, nil, fmt.Errorf("load saml SP key pair: certificate chain is empty")
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, nil, fmt.Errorf("parse saml SP certificate: %w", err)
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, nil, fmt.Errorf("saml SP private key does not implement crypto.Signer")
	}
	return cert, signer, nil
}

func deriveServiceURL(publicBaseURL, path, label string) (string, error) {
	if strings.TrimSpace(publicBaseURL) == "" {
		return "", fmt.Errorf("%s requires auth public base URL when no explicit URL is configured", label)
	}
	base, err := parseHTTPURL(publicBaseURL, "auth public base URL")
	if err != nil {
		return "", err
	}
	base.Path = strings.TrimRight(base.Path, "/") + path
	base.RawQuery = ""
	base.Fragment = ""
	return base.String(), nil
}

func parseHTTPURL(raw, label string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s is invalid: %w", label, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("%s must be absolute", label)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf("%s must use http or https", label)
	}
	return u, nil
}

func parseHTTPSURL(raw, label string) (*url.URL, error) {
	u, err := parseHTTPURL(raw, label)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("%s must use https", label)
	}
	return u, nil
}
