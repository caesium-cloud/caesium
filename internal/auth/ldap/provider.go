package ldap

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	authpkg "github.com/caesium-cloud/caesium/internal/auth"
	gldap "github.com/go-ldap/ldap/v3"
)

var (
	ErrInvalidConfig      = errors.New("invalid ldap config")
	ErrInvalidCredentials = errors.New("invalid ldap credentials")
	ErrAmbiguousUser      = errors.New("ldap user search returned multiple entries")
)

// DialFunc opens an LDAP connection. Tests can provide one to avoid a live LDAP
// server while still exercising bind/search flow.
type DialFunc func(ctx context.Context, cfg Config) (Conn, error)

// Conn is the subset of go-ldap's connection used by the provider.
type Conn interface {
	Bind(username, password string) error
	Search(searchRequest *gldap.SearchRequest) (*gldap.SearchResult, error)
	StartTLS(config *tls.Config) error
	SetTimeout(timeout time.Duration)
	Close() error
}

// Provider implements Caesium credential authentication for LDAP directories.
type Provider struct {
	cfg  Config
	dial DialFunc
}

var _ authpkg.CredentialAuthenticator = (*Provider)(nil)

// New constructs an LDAP credential provider. It does not connect to LDAP until
// Authenticate is called.
func New(cfg Config) (*Provider, error) {
	cfg, err := cfg.normalize()
	if err != nil {
		return nil, err
	}
	dial := cfg.Dial
	if dial == nil {
		dial = defaultDial
	}
	return &Provider{cfg: cfg, dial: dial}, nil
}

// Name reports the provider id used by the shared SSO completion path.
func (p *Provider) Name() string {
	return ProviderName
}

// Authenticate verifies user credentials with search-then-bind and returns a
// normalized external identity for the shared SSO completion path.
func (p *Provider) Authenticate(ctx context.Context, username, password string) (*authpkg.ExternalIdentity, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return nil, ErrInvalidCredentials
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	conn, err := p.dial(ctx, p.cfg)
	if err != nil {
		return nil, fmt.Errorf("connect ldap: %w", err)
	}
	defer func() {
		_ = conn.Close()
	}()
	conn.SetTimeout(p.cfg.Timeout)

	if err := p.bindService(ctx, conn); err != nil {
		return nil, err
	}
	userEntry, err := p.searchUser(ctx, conn, username)
	if err != nil {
		return nil, err
	}
	if err := p.bindUser(ctx, conn, userEntry.DN, password); err != nil {
		return nil, err
	}
	if err := p.bindService(ctx, conn); err != nil {
		return nil, err
	}

	identityUsername := firstEntryValue(userEntry, p.cfg.UsernameAttribute)
	if identityUsername == "" {
		identityUsername = username
	}
	groups, err := p.searchGroups(ctx, conn, userEntry.DN, identityUsername)
	if err != nil {
		return nil, err
	}

	email := firstEntryValue(userEntry, p.cfg.EmailAttribute)
	displayName := firstEntryValue(userEntry, p.cfg.DisplayNameAttribute)
	if displayName == "" {
		displayName = identityUsername
	}
	if displayName == "" {
		displayName = email
	}
	if displayName == "" {
		displayName = userEntry.DN
	}

	return &authpkg.ExternalIdentity{
		Issuer:      ProviderName,
		Subject:     userEntry.DN,
		Email:       email,
		DisplayName: displayName,
		Groups:      groups,
	}, nil
}

func defaultDial(ctx context.Context, cfg Config) (Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: cfg.Timeout}
	opts := []gldap.DialOpt{gldap.DialWithDialer(dialer)}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(cfg.URL)), "ldaps://") {
		opts = append(opts, gldap.DialWithTLSConfig(cfg.tlsConfig()))
	}
	conn, err := gldap.DialURL(cfg.URL, opts...)
	if err != nil {
		return nil, err
	}
	if cfg.StartTLS {
		if err := conn.StartTLS(cfg.tlsConfig()); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("start ldap TLS: %w", err)
		}
	}
	return conn, nil
}

func (p *Provider) bindService(ctx context.Context, conn Conn) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := conn.Bind(p.cfg.BindDN, p.cfg.BindPassword); err != nil {
		return fmt.Errorf("%w: service bind failed: %v", ErrInvalidConfig, err)
	}
	return nil
}

func (p *Provider) bindUser(ctx context.Context, conn Conn, dn, password string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(dn) == "" || password == "" {
		return ErrInvalidCredentials
	}
	if err := conn.Bind(dn, password); err != nil {
		return fmt.Errorf("%w: bind failed", ErrInvalidCredentials)
	}
	return nil
}

func (p *Provider) searchUser(ctx context.Context, conn Conn, username string) (*gldap.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req, err := p.userSearchRequest(username)
	if err != nil {
		return nil, err
	}
	res, err := conn.Search(req)
	if err != nil {
		return nil, fmt.Errorf("search ldap user: %w", err)
	}
	if res == nil {
		return nil, fmt.Errorf("%w: user search returned nil result", ErrInvalidConfig)
	}
	switch len(res.Entries) {
	case 0:
		return nil, ErrInvalidCredentials
	case 1:
		if strings.TrimSpace(res.Entries[0].DN) == "" {
			return nil, fmt.Errorf("%w: user entry is missing DN", ErrInvalidConfig)
		}
		return res.Entries[0], nil
	default:
		return nil, ErrAmbiguousUser
	}
}

func (p *Provider) searchGroups(ctx context.Context, conn Conn, userDN, username string) ([]string, error) {
	if p.cfg.GroupFilter == "" {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req, err := p.groupSearchRequest(userDN, username)
	if err != nil {
		return nil, err
	}
	res, err := conn.Search(req)
	if err != nil {
		return nil, fmt.Errorf("search ldap groups: %w", err)
	}
	if res == nil {
		return nil, fmt.Errorf("%w: group search returned nil result", ErrInvalidConfig)
	}
	groups := make([]string, 0, len(res.Entries))
	seen := make(map[string]struct{}, len(res.Entries))
	for _, entry := range res.Entries {
		for _, value := range entryValues(entry, p.cfg.GroupAttribute) {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			groups = append(groups, value)
		}
	}
	return groups, nil
}

func (p *Provider) userSearchRequest(username string) (*gldap.SearchRequest, error) {
	filter, err := renderUserFilter(p.cfg.UserFilter, username)
	if err != nil {
		return nil, err
	}
	return gldap.NewSearchRequest(
		p.cfg.UserBaseDN,
		gldap.ScopeWholeSubtree,
		gldap.NeverDerefAliases,
		2,
		timeLimitSeconds(p.cfg.Timeout),
		false,
		filter,
		p.userAttributes(),
		nil,
	), nil
}

func (p *Provider) groupSearchRequest(userDN, username string) (*gldap.SearchRequest, error) {
	filter, err := renderGroupFilter(p.cfg.GroupFilter, userDN, username)
	if err != nil {
		return nil, err
	}
	return gldap.NewSearchRequest(
		p.cfg.GroupBaseDN,
		gldap.ScopeWholeSubtree,
		gldap.NeverDerefAliases,
		0,
		timeLimitSeconds(p.cfg.Timeout),
		false,
		filter,
		[]string{p.cfg.GroupAttribute},
		nil,
	), nil
}

func (p *Provider) userAttributes() []string {
	attrs := []string{p.cfg.UsernameAttribute, p.cfg.EmailAttribute, p.cfg.DisplayNameAttribute}
	out := make([]string, 0, len(attrs))
	seen := make(map[string]struct{}, len(attrs))
	for _, attr := range attrs {
		if strings.EqualFold(attr, "dn") {
			continue
		}
		key := strings.ToLower(attr)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, attr)
	}
	return out
}

func renderUserFilter(template, username string) (string, error) {
	if err := validateUserFilter(template); err != nil {
		return "", err
	}
	escaped := gldap.EscapeFilter(username)
	if strings.Contains(template, "{username}") {
		return strings.ReplaceAll(template, "{username}", escaped), nil
	}
	return strings.Replace(template, "%s", escaped, 1), nil
}

func renderGroupFilter(template, userDN, username string) (string, error) {
	if err := validateGroupFilter(template); err != nil {
		return "", err
	}
	if strings.Contains(template, "{dn}") || strings.Contains(template, "{username}") {
		filter := strings.ReplaceAll(template, "{dn}", gldap.EscapeFilter(userDN))
		filter = strings.ReplaceAll(filter, "{username}", gldap.EscapeFilter(username))
		return filter, nil
	}
	return strings.Replace(template, "%s", gldap.EscapeFilter(userDN), 1), nil
}

func firstEntryValue(entry *gldap.Entry, attr string) string {
	for _, value := range entryValues(entry, attr) {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func entryValues(entry *gldap.Entry, attr string) []string {
	if entry == nil {
		return nil
	}
	if strings.EqualFold(attr, "dn") {
		if strings.TrimSpace(entry.DN) == "" {
			return nil
		}
		return []string{entry.DN}
	}
	return entry.GetEqualFoldAttributeValues(attr)
}

func timeLimitSeconds(timeout time.Duration) int {
	seconds := int(timeout / time.Second)
	if timeout%time.Second != 0 {
		seconds++
	}
	if seconds <= 0 {
		return int(DefaultTimeout / time.Second)
	}
	return seconds
}

func errConfig(msg string) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfig, msg)
}
