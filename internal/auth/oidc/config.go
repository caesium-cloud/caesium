package oidc

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/pkg/env"
)

const (
	// ProviderName is the auth method reported by the OIDC redirect provider.
	ProviderName = "oidc"

	// DefaultScopes is the envconfig default for CAESIUM_AUTH_OIDC_SCOPES.
	DefaultScopes = "openid profile email groups"

	// DefaultGroupsClaim is the envconfig default for CAESIUM_AUTH_OIDC_GROUPS_CLAIM.
	DefaultGroupsClaim = "groups"

	// DefaultRedirectPath is the Caesium callback path used when only a public
	// base URL is configured.
	DefaultRedirectPath = "/auth/sso/oidc/callback"

	DefaultStateCookieName = "caesium_oidc_state"
	DefaultStateTTL        = 10 * time.Minute
)

// Config configures the OIDC redirect provider.
type Config struct {
	IssuerURL     string
	ClientID      string
	ClientSecret  string
	Scopes        []string
	GroupsClaim   string
	RedirectURL   string
	PublicBaseURL string

	StateCookieName string
	StateTTL        time.Duration
	CookieSecure    bool
	CookieSecret    []byte

	HTTPClient *http.Client
}

// ConfigFromEnv converts Caesium environment config into provider config. The
// caller still controls construction so startup can decide when OIDC is enabled.
func ConfigFromEnv(vars env.Environment) Config {
	return Config{
		IssuerURL:     vars.AuthOIDCIssuerURL,
		ClientID:      vars.AuthOIDCClientID,
		ClientSecret:  vars.AuthOIDCClientSecret,
		Scopes:        ParseScopes(vars.AuthOIDCScopes),
		GroupsClaim:   vars.AuthOIDCGroupsClaim,
		RedirectURL:   vars.AuthOIDCRedirectURL,
		PublicBaseURL: vars.AuthPublicBaseURL,
		CookieSecure:  vars.AuthRequireTLS,
		CookieSecret:  []byte(vars.AuthKeyHashSecret),
	}
}

// ParseScopes parses CAESIUM_AUTH_OIDC_SCOPES. Empty input returns the default
// OpenID Connect scopes.
func ParseScopes(raw string) []string {
	scopes := strings.Fields(raw)
	if len(scopes) == 0 {
		return strings.Fields(DefaultScopes)
	}
	return scopes
}

func (c Config) normalize() (Config, error) {
	c.IssuerURL = strings.TrimSpace(c.IssuerURL)
	c.ClientID = strings.TrimSpace(c.ClientID)
	c.ClientSecret = strings.TrimSpace(c.ClientSecret)
	c.RedirectURL = strings.TrimSpace(c.RedirectURL)
	c.PublicBaseURL = strings.TrimSpace(c.PublicBaseURL)
	c.GroupsClaim = strings.TrimSpace(c.GroupsClaim)
	c.StateCookieName = strings.TrimSpace(c.StateCookieName)

	if c.IssuerURL == "" {
		return c, fmt.Errorf("oidc issuer URL is required")
	}
	if c.ClientID == "" {
		return c, fmt.Errorf("oidc client ID is required")
	}
	if c.ClientSecret == "" {
		return c, fmt.Errorf("oidc client secret is required")
	}
	if c.GroupsClaim == "" {
		c.GroupsClaim = DefaultGroupsClaim
	}
	if c.StateCookieName == "" {
		c.StateCookieName = DefaultStateCookieName
	}
	if c.StateTTL <= 0 {
		c.StateTTL = DefaultStateTTL
	}
	if len(c.Scopes) == 0 {
		c.Scopes = ParseScopes("")
	}
	if !containsScope(c.Scopes, "openid") {
		return c, fmt.Errorf("oidc scopes must include openid")
	}
	if c.RedirectURL == "" {
		redirectURL, err := deriveRedirectURL(c.PublicBaseURL)
		if err != nil {
			return c, err
		}
		c.RedirectURL = redirectURL
	}
	if _, err := parseAbsoluteURL(c.IssuerURL, "oidc issuer URL"); err != nil {
		return c, err
	}
	if _, err := parseAbsoluteURL(c.RedirectURL, "oidc redirect URL"); err != nil {
		return c, err
	}
	if len(c.CookieSecret) == 0 {
		c.CookieSecret = []byte(c.ClientSecret)
	}
	return c, nil
}

func containsScope(scopes []string, target string) bool {
	for _, scope := range scopes {
		if scope == target {
			return true
		}
	}
	return false
}

func deriveRedirectURL(publicBaseURL string) (string, error) {
	if strings.TrimSpace(publicBaseURL) == "" {
		return "", fmt.Errorf("oidc redirect URL requires auth public base URL when no explicit redirect URL is configured")
	}
	base, err := parseAbsoluteURL(publicBaseURL, "auth public base URL")
	if err != nil {
		return "", err
	}
	base.Path = strings.TrimRight(base.Path, "/") + DefaultRedirectPath
	base.RawQuery = ""
	base.Fragment = ""
	return base.String(), nil
}

func parseAbsoluteURL(raw, label string) (*url.URL, error) {
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
