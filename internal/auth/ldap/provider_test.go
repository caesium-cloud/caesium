package ldap

import (
	"context"
	"crypto/tls"
	"errors"
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
	cfg.Dial = func(context.Context, Config) (Conn, error) { return fake, nil }
	provider, err := New(cfg)
	require.NoError(t, err)

	identity, err := provider.Authenticate(context.Background(), "pat", "user-secret")
	require.NoError(t, err)
	require.Equal(t, []string{groupDN}, identity.Groups)

	mapper, err := authpkg.NewRoleMapper(groupDN+"=operator", "")
	require.NoError(t, err)
	role, ok := mapper.Resolve(identity.Groups)
	require.True(t, ok)
	require.Equal(t, models.RoleOperator, role)
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
	cfg.Dial = func(context.Context, Config) (Conn, error) { return fake, nil }
	provider, err := New(cfg)
	require.NoError(t, err)

	identity, err := provider.Authenticate(context.Background(), "alice", "user-secret")
	require.Nil(t, identity)
	require.ErrorContains(t, err, "search ldap groups")
	require.ErrorIs(t, err, groupSearchErr)
	require.Len(t, fake.searches, 2)
	require.Equal(t, "ou=Groups,dc=example,dc=com", fake.searches[1].BaseDN)
	require.Equal(t, `(member=uid=alice,ou=People,dc=example,dc=com)`, fake.searches[1].Filter)
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
	groupResult    *gldap.SearchResult
	userBindErr    error
	groupSearchErr error
}

func (f *fakeConn) Bind(username, password string) error {
	f.binds = append(f.binds, bindCall{dn: username, password: password})
	if username != baseConfig().BindDN && f.userBindErr != nil {
		return f.userBindErr
	}
	return nil
}

func (f *fakeConn) Search(req *gldap.SearchRequest) (*gldap.SearchResult, error) {
	f.searches = append(f.searches, req)
	switch req.BaseDN {
	case baseConfig().UserBaseDN:
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
	f.closed = true
	return nil
}
