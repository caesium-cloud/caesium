package ldap

import (
	"context"
	"crypto/tls"
	"errors"
	"sync"
	"testing"
	"time"

	authpkg "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	gldap "github.com/go-ldap/ldap/v3"
	"github.com/stretchr/testify/require"
)

func TestProviderImplementsCredentialAuthenticator(t *testing.T) {
	provider, err := New(baseConfig())
	require.NoError(t, err)
	require.Equal(t, ProviderName, provider.Name())
}

func TestRenderUserFilterEscapesUsername(t *testing.T) {
	filter, err := renderUserFilter("(uid=%s)", "alice*)(uid=*)")
	require.NoError(t, err)
	require.Equal(t, `(uid=alice\2a\29\28uid=\2a\29)`, filter)

	filter, err = renderUserFilter("(|(uid={username})(mail={username}))", "a(b)@example.com")
	require.NoError(t, err)
	require.Equal(t, `(|(uid=a\28b\29@example.com)(mail=a\28b\29@example.com))`, filter)
}

func TestRenderGroupFilterEscapesDNAndUsername(t *testing.T) {
	filter, err := renderGroupFilter(
		"(|(member={dn})(memberUid={username}))",
		"uid=al(ice),ou=People,dc=example,dc=com",
		"alice*",
	)
	require.NoError(t, err)
	require.Equal(t, `(|(member=uid=al\28ice\29,ou=People,dc=example,dc=com)(memberUid=alice\2a))`, filter)

	filter, err = renderGroupFilter("(member=%s)", "uid=al(ice),dc=example,dc=com", "ignored")
	require.NoError(t, err)
	require.Equal(t, `(member=uid=al\28ice\29,dc=example,dc=com)`, filter)
}

func TestAuthenticateSearchBindAndGroupFlow(t *testing.T) {
	fake := &fakeConn{
		userResult: &gldap.SearchResult{Entries: []*gldap.Entry{
			gldap.NewEntry("uid=al(ice),ou=People,dc=example,dc=com", map[string][]string{
				"uid":         {"alice*"},
				"mail":        {"alice@example.com"},
				"displayName": {"Alice Example"},
			}),
		}},
		groupResult: &gldap.SearchResult{Entries: []*gldap.Entry{
			gldap.NewEntry("cn=eng,ou=Groups,dc=example,dc=com", map[string][]string{"cn": {"eng"}}),
			gldap.NewEntry("cn=ops,ou=Groups,dc=example,dc=com", map[string][]string{"cn": {"ops", "eng"}}),
		}},
	}
	cfg := baseConfig()
	cfg.UserFilter = "(uid={username})"
	cfg.GroupBaseDN = "ou=Groups,dc=example,dc=com"
	cfg.GroupFilter = "(|(member={dn})(memberUid={username}))"
	cfg.Dial = func(ctx context.Context, got Config) (Conn, error) {
		require.NoError(t, ctx.Err())
		require.Equal(t, cfg.URL, got.URL)
		return fake, nil
	}
	provider, err := New(cfg)
	require.NoError(t, err)

	identity, err := provider.Authenticate(context.Background(), "al*)", "user-secret")
	require.NoError(t, err)

	require.Equal(t, ProviderName, identity.Issuer)
	require.Equal(t, "uid=al(ice),ou=People,dc=example,dc=com", identity.Subject)
	require.NotNil(t, identity.Refresh, "the user DN is the LDAP refresh grant")
	require.Equal(t, ProviderName, identity.Refresh.Provider)
	require.Equal(t, ProviderName, identity.Refresh.Issuer)
	require.Equal(t, identity.Subject, identity.Refresh.Handle.Reveal())
	require.Equal(t, "al*)", identity.Refresh.Login, "the typed name the user filter was rendered with")
	require.Equal(t, "alice@example.com", identity.Email)
	require.Equal(t, "Alice Example", identity.DisplayName)
	require.Equal(t, []string{"eng", "ops"}, identity.Groups)

	require.Equal(t, []bindCall{
		{dn: cfg.BindDN, password: cfg.BindPassword},
		{dn: "uid=al(ice),ou=People,dc=example,dc=com", password: "user-secret"},
		{dn: cfg.BindDN, password: cfg.BindPassword},
	}, fake.binds)
	require.Len(t, fake.searches, 2)
	require.Equal(t, "ou=People,dc=example,dc=com", fake.searches[0].BaseDN)
	require.Equal(t, `(uid=al\2a\29)`, fake.searches[0].Filter)
	require.Equal(t, []string{"uid", "mail", "displayName"}, fake.searches[0].Attributes)
	require.Equal(t, "ou=Groups,dc=example,dc=com", fake.searches[1].BaseDN)
	require.Equal(t, `(|(member=uid=al\28ice\29,ou=People,dc=example,dc=com)(memberUid=alice\2a))`, fake.searches[1].Filter)
	require.Equal(t, []string{"cn"}, fake.searches[1].Attributes)
	require.Equal(t, DefaultTimeout, fake.timeout)
	require.True(t, fake.closed)
}

func TestAuthenticateGroupDNsCanResolveRoleMappings(t *testing.T) {
	groupDN := "cn=caesium-operators,ou=Groups,dc=example,dc=com"
	fake := &fakeConn{
		userResult: &gldap.SearchResult{Entries: []*gldap.Entry{
			gldap.NewEntry("uid=pat,ou=People,dc=example,dc=com", map[string][]string{
				"uid":  {"pat"},
				"mail": {"pat@example.com"},
			}),
		}},
		groupResult: &gldap.SearchResult{Entries: []*gldap.Entry{
			gldap.NewEntry(groupDN, map[string][]string{"cn": {"caesium-operators"}}),
		}},
	}
	cfg := baseConfig()
	cfg.GroupBaseDN = "ou=Groups,dc=example,dc=com"
	cfg.GroupFilter = "(memberUid={username})"
	cfg.GroupAttribute = "dn"
	cfg.Dial = func(_ context.Context, _ Config) (Conn, error) { return fake, nil }
	provider, err := New(cfg)
	require.NoError(t, err)

	identity, err := provider.Authenticate(context.Background(), "pat", "user-secret")
	require.NoError(t, err)
	require.Equal(t, []string{groupDN}, identity.Groups)

	mapper, err := authpkg.NewRoleMapper(groupDN+"=operator", "")
	require.NoError(t, err)
	grants := mapper.Resolve(identity.Groups, "")
	require.NotEmpty(t, grants)
	require.Equal(t, models.RoleOperator, grants.ClusterRole())
	require.Equal(t, `(memberUid=pat)`, fake.searches[1].Filter)
	require.Equal(t, []string{"dn"}, fake.searches[1].Attributes)
}

func TestAuthenticateFailsClosedWhenGroupSearchFails(t *testing.T) {
	groupSearchErr := errors.New("directory unavailable")
	fake := &fakeConn{
		userResult: &gldap.SearchResult{Entries: []*gldap.Entry{
			gldap.NewEntry("uid=alice,ou=People,dc=example,dc=com", map[string][]string{
				"uid":  {"alice"},
				"mail": {"alice@example.com"},
			}),
		}},
		groupSearchErr: groupSearchErr,
	}
	cfg := baseConfig()
	cfg.GroupBaseDN = "ou=Groups,dc=example,dc=com"
	cfg.GroupFilter = "(member={dn})"
	cfg.Dial = func(_ context.Context, _ Config) (Conn, error) { return fake, nil }
	provider, err := New(cfg)
	require.NoError(t, err)

	identity, err := provider.Authenticate(context.Background(), "alice", "user-secret")
	require.Nil(t, identity)
	require.ErrorContains(t, err, "search ldap groups")
	require.ErrorIs(t, err, groupSearchErr)
	require.Len(t, fake.searches, 2)
	require.Equal(t, "ou=Groups,dc=example,dc=com", fake.searches[1].BaseDN)
	require.Equal(t, `(member=uid=alice,ou=People,dc=example,dc=com)`, fake.searches[1].Filter)
	require.Equal(t, []string{"cn"}, fake.searches[1].Attributes)
	require.Equal(t, []bindCall{
		{dn: cfg.BindDN, password: cfg.BindPassword},
		{dn: "uid=alice,ou=People,dc=example,dc=com", password: "user-secret"},
		{dn: cfg.BindDN, password: cfg.BindPassword},
	}, fake.binds)
	require.True(t, fake.closed)
}

func TestAuthenticateRejectsEmptyPasswordBeforeDial(t *testing.T) {
	cfg := baseConfig()
	cfg.Dial = func(context.Context, Config) (Conn, error) {
		t.Fatal("dial should not be called for empty passwords")
		return nil, nil
	}
	provider, err := New(cfg)
	require.NoError(t, err)

	_, err = provider.Authenticate(context.Background(), "alice", "")
	require.ErrorIs(t, err, ErrInvalidCredentials)
}

func TestAuthenticateFailsClosedWhenUserSearchReturnsNilResult(t *testing.T) {
	fake := &fakeConn{nilUserResult: true}
	cfg := baseConfig()
	cfg.Dial = func(_ context.Context, _ Config) (Conn, error) { return fake, nil }
	provider, err := New(cfg)
	require.NoError(t, err)

	identity, err := provider.Authenticate(context.Background(), "alice", "secret")
	require.Nil(t, identity)
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.ErrorContains(t, err, "user search returned nil result")
	require.Equal(t, []bindCall{{dn: cfg.BindDN, password: cfg.BindPassword}}, fake.binds)
	require.Len(t, fake.searches, 1)
	require.Equal(t, "ou=People,dc=example,dc=com", fake.searches[0].BaseDN)
	require.True(t, fake.closed)
}

func TestAuthenticateFailsClosedWhenUserEntryMissingDN(t *testing.T) {
	fake := &fakeConn{
		userResult: &gldap.SearchResult{Entries: []*gldap.Entry{
			{DN: "   "},
		}},
	}
	cfg := baseConfig()
	cfg.Dial = func(_ context.Context, _ Config) (Conn, error) { return fake, nil }
	provider, err := New(cfg)
	require.NoError(t, err)

	identity, err := provider.Authenticate(context.Background(), "alice", "secret")
	require.Nil(t, identity)
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.ErrorContains(t, err, "user entry is missing DN")
	require.Equal(t, []bindCall{{dn: cfg.BindDN, password: cfg.BindPassword}}, fake.binds)
	require.Len(t, fake.searches, 1)
	require.True(t, fake.closed)
}

func TestAuthenticateRejectsDuplicateUserSearchResults(t *testing.T) {
	fake := &fakeConn{
		userResult: &gldap.SearchResult{Entries: []*gldap.Entry{
			gldap.NewEntry("uid=one,dc=example,dc=com", nil),
			gldap.NewEntry("uid=two,dc=example,dc=com", nil),
		}},
	}
	cfg := baseConfig()
	cfg.Dial = func(context.Context, Config) (Conn, error) { return fake, nil }
	provider, err := New(cfg)
	require.NoError(t, err)

	_, err = provider.Authenticate(context.Background(), "alice", "secret")
	require.ErrorIs(t, err, ErrAmbiguousUser)
}

func TestAuthenticateRejectsBadUserBind(t *testing.T) {
	fake := &fakeConn{
		userResult: &gldap.SearchResult{Entries: []*gldap.Entry{
			gldap.NewEntry("uid=alice,dc=example,dc=com", map[string][]string{"uid": {"alice"}}),
		}},
		userBindErr: errors.New("invalid credentials"),
	}
	cfg := baseConfig()
	cfg.Dial = func(context.Context, Config) (Conn, error) { return fake, nil }
	provider, err := New(cfg)
	require.NoError(t, err)

	_, err = provider.Authenticate(context.Background(), "alice", "wrong")
	require.ErrorIs(t, err, ErrInvalidCredentials)
}

type bindCall struct {
	dn       string
	password string
}

type fakeConn struct {
	binds          []bindCall
	searches       []*gldap.SearchRequest
	timeout        time.Duration
	closed         bool
	userResult     *gldap.SearchResult
	nilUserResult  bool
	groupResult    *gldap.SearchResult
	userBindErr    error
	groupSearchErr error

	// Refresh lookups: base-scope searches answer from entries by DN, else
	// noSuchObject; baseSearchErr overrides. The user base entry is visible
	// unless userBaseHidden.
	entries        map[string]*gldap.Entry
	admits         func(entry *gldap.Entry, filter string) bool // nil admits every entry
	baseSearchErr  error
	userBaseHidden bool
	serviceErr     error
	// blockBind / blockSearchBase block that call until Close, signalling
	// blocked first, like a directory that stops answering.
	blockBind       bool
	blockSearchBase string
	blocked         chan struct{}

	mu     sync.Mutex
	doneCh chan struct{}
	closes int
}

func (f *fakeConn) done() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.doneCh == nil {
		f.doneCh = make(chan struct{})
	}
	return f.doneCh
}

func (f *fakeConn) blockUntilClosed() error {
	if f.blocked != nil {
		close(f.blocked)
	}
	<-f.done()
	return gldap.NewError(gldap.ErrorNetwork, errors.New("ldap: connection closed"))
}

func (f *fakeConn) Bind(username, password string) error {
	f.binds = append(f.binds, bindCall{dn: username, password: password})
	if f.blockBind {
		return f.blockUntilClosed()
	}
	if username == baseConfig().BindDN && f.serviceErr != nil {
		return f.serviceErr
	}
	if username != baseConfig().BindDN && f.userBindErr != nil {
		return f.userBindErr
	}
	return nil
}

func (f *fakeConn) Search(req *gldap.SearchRequest) (*gldap.SearchResult, error) {
	f.searches = append(f.searches, req)
	if f.blockSearchBase != "" && req.BaseDN == f.blockSearchBase {
		return nil, f.blockUntilClosed()
	}
	if req.Scope == gldap.ScopeBaseObject {
		if req.BaseDN == baseConfig().UserBaseDN {
			if f.userBaseHidden {
				return nil, gldap.NewError(gldap.LDAPResultNoSuchObject, errors.New("no such object"))
			}
			return &gldap.SearchResult{Entries: []*gldap.Entry{gldap.NewEntry(req.BaseDN, nil)}}, nil
		}
		if f.baseSearchErr != nil {
			return nil, f.baseSearchErr
		}
		if entry, ok := f.entries[req.BaseDN]; ok {
			if entry == nil || (f.admits != nil && !f.admits(entry, req.Filter)) {
				return &gldap.SearchResult{}, nil
			}
			return &gldap.SearchResult{Entries: []*gldap.Entry{entry}}, nil
		}
		return nil, gldap.NewError(gldap.LDAPResultNoSuchObject, errors.New("no such object"))
	}
	switch req.BaseDN {
	case baseConfig().UserBaseDN:
		if f.nilUserResult {
			return nil, nil
		}
		if f.userResult != nil {
			return f.userResult, nil
		}
	case "ou=Groups,dc=example,dc=com":
		if f.groupSearchErr != nil {
			return nil, f.groupSearchErr
		}
		if f.groupResult != nil {
			return f.groupResult, nil
		}
	}
	return &gldap.SearchResult{}, nil
}

func (f *fakeConn) StartTLS(*tls.Config) error {
	return nil
}

func (f *fakeConn) SetTimeout(timeout time.Duration) {
	f.timeout = timeout
}

func (f *fakeConn) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	f.closes++
	if f.doneCh == nil {
		f.doneCh = make(chan struct{})
	}
	select {
	case <-f.doneCh:
	default:
		close(f.doneCh)
	}
	return nil
}
