package ldap

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/caesium-cloud/caesium/pkg/env"
)

const (
	// ProviderName is the auth method reported by the LDAP credential provider.
	ProviderName = "ldap"

	DefaultUserFilter           = "(uid={username})"
	DefaultGroupFilter          = "(member={dn})"
	DefaultUsernameAttribute    = "uid"
	DefaultEmailAttribute       = "mail"
	DefaultDisplayNameAttribute = "displayName"
	DefaultGroupAttribute       = "cn"
	DefaultTimeout              = 10 * time.Second
)

// Config configures the LDAP credential provider.
type Config struct {
	Enabled bool

	URL       string
	StartTLS  bool
	TLSConfig *tls.Config
	Timeout   time.Duration

	BindDN       string
	BindPassword string

	UserBaseDN string
	UserFilter string

	GroupBaseDN    string
	GroupFilter    string
	GroupAttribute string

	UsernameAttribute    string
	EmailAttribute       string
	DisplayNameAttribute string

	Dial DialFunc
}

// ConfigFromEnv converts Caesium environment config into provider config.
func ConfigFromEnv(vars env.Environment) Config {
	return Config{
		Enabled:              vars.AuthLDAPEnabled,
		URL:                  vars.AuthLDAPURL,
		StartTLS:             vars.AuthLDAPStartTLS,
		Timeout:              vars.AuthLDAPTimeout,
		BindDN:               vars.AuthLDAPBindDN,
		BindPassword:         vars.AuthLDAPBindPassword,
		UserBaseDN:           vars.AuthLDAPUserBaseDN,
		UserFilter:           vars.AuthLDAPUserFilter,
		GroupBaseDN:          vars.AuthLDAPGroupBaseDN,
		GroupFilter:          vars.AuthLDAPGroupFilter,
		GroupAttribute:       vars.AuthLDAPGroupAttribute,
		UsernameAttribute:    vars.AuthLDAPUsernameAttribute,
		EmailAttribute:       vars.AuthLDAPEmailAttribute,
		DisplayNameAttribute: vars.AuthLDAPDisplayNameAttribute,
	}
}

func (c Config) normalize() (Config, error) {
	c.URL = strings.TrimSpace(c.URL)
	c.BindDN = strings.TrimSpace(c.BindDN)
	c.UserBaseDN = strings.TrimSpace(c.UserBaseDN)
	c.UserFilter = strings.TrimSpace(c.UserFilter)
	c.GroupBaseDN = strings.TrimSpace(c.GroupBaseDN)
	c.GroupFilter = strings.TrimSpace(c.GroupFilter)
	c.GroupAttribute = strings.TrimSpace(c.GroupAttribute)
	c.UsernameAttribute = strings.TrimSpace(c.UsernameAttribute)
	c.EmailAttribute = strings.TrimSpace(c.EmailAttribute)
	c.DisplayNameAttribute = strings.TrimSpace(c.DisplayNameAttribute)

	if c.UsernameAttribute == "" {
		c.UsernameAttribute = DefaultUsernameAttribute
	}
	if c.EmailAttribute == "" {
		c.EmailAttribute = DefaultEmailAttribute
	}
	if c.DisplayNameAttribute == "" {
		c.DisplayNameAttribute = DefaultDisplayNameAttribute
	}
	if c.GroupAttribute == "" {
		c.GroupAttribute = DefaultGroupAttribute
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.UserFilter == "" {
		c.UserFilter = DefaultUserFilter
	}
	if c.GroupBaseDN != "" && c.GroupFilter == "" {
		c.GroupFilter = DefaultGroupFilter
	}

	if c.URL == "" {
		return c, errConfig("ldap URL is required")
	}
	ldapURL, err := parseLDAPURL(c.URL)
	if err != nil {
		return c, err
	}
	switch ldapURL.Scheme {
	case "ldaps":
		if c.StartTLS {
			return c, errConfig("ldap StartTLS requires an ldap:// URL")
		}
	case "ldap":
		if !c.StartTLS {
			return c, errConfig("ldap:// URL requires StartTLS")
		}
	default:
		return c, errConfig("ldap URL must use ldap or ldaps")
	}

	if c.BindDN == "" {
		return c, errConfig("ldap bind DN is required")
	}
	if c.BindPassword == "" {
		return c, errConfig("ldap bind password is required")
	}
	if c.UserBaseDN == "" {
		return c, errConfig("ldap user base DN is required")
	}
	if err := validateAttributeName(c.UsernameAttribute, "ldap username attribute"); err != nil {
		return c, err
	}
	if err := validateAttributeName(c.EmailAttribute, "ldap email attribute"); err != nil {
		return c, err
	}
	if err := validateAttributeName(c.DisplayNameAttribute, "ldap display name attribute"); err != nil {
		return c, err
	}
	if err := validateAttributeName(c.GroupAttribute, "ldap group attribute"); err != nil {
		return c, err
	}
	if err := validateUserFilter(c.UserFilter); err != nil {
		return c, err
	}
	if (c.GroupBaseDN == "") != (c.GroupFilter == "") {
		return c, errConfig("ldap group base DN and group filter must be configured together")
	}
	if c.GroupFilter != "" {
		if err := validateGroupFilter(c.GroupFilter); err != nil {
			return c, err
		}
	}

	return c, nil
}

func parseLDAPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: ldap URL is invalid: %v", ErrInvalidConfig, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, errConfig("ldap URL must be absolute")
	}
	switch u.Scheme {
	case "ldap", "ldaps":
	default:
		return nil, errConfig("ldap URL must use ldap or ldaps")
	}
	return u, nil
}

func (c Config) tlsConfig() *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.TLSConfig != nil {
		cfg = c.TLSConfig.Clone()
		if cfg.MinVersion == 0 {
			cfg.MinVersion = tls.VersionTLS12
		}
	}
	if cfg.ServerName == "" {
		if host := ldapServerName(c.URL); host != "" {
			cfg.ServerName = host
		}
	}
	return cfg
}

func ldapServerName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	if host == "" {
		host = u.Host
		if split, _, err := net.SplitHostPort(host); err == nil {
			host = split
		}
	}
	return strings.Trim(host, "[]")
}

func validateAttributeName(name, label string) error {
	if strings.EqualFold(name, "dn") {
		return nil
	}
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			continue
		}
		switch r {
		case '-', '.', '_', ';':
			continue
		default:
			return errConfig(fmt.Sprintf("%s contains invalid character %q", label, r))
		}
	}
	return nil
}

func validateUserFilter(filter string) error {
	if strings.Contains(filter, "{username}") {
		if strings.Contains(filter, "%s") {
			return errConfig("ldap user filter must not mix %s and {username} placeholders")
		}
		return nil
	}
	if strings.Count(filter, "%s") != 1 {
		return errConfig("ldap user filter must contain exactly one %s placeholder or a {username} token")
	}
	return nil
}

func validateGroupFilter(filter string) error {
	hasDN := strings.Contains(filter, "{dn}")
	hasUsername := strings.Contains(filter, "{username}")
	if hasDN || hasUsername {
		if strings.Contains(filter, "%s") {
			return errConfig("ldap group filter must not mix %s and named placeholders")
		}
		return nil
	}
	if strings.Count(filter, "%s") != 1 {
		return errConfig("ldap group filter must contain exactly one %s placeholder or {dn}/{username} tokens")
	}
	return nil
}
