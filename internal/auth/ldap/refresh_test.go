package ldap

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	authpkg "github.com/caesium-cloud/caesium/internal/auth"
	gldap "github.com/go-ldap/ldap/v3"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const (
	aliceDN    = "uid=al(ice),ou=People,dc=example,dc=com"
	groupsBase = "ou=Groups,dc=example,dc=com"
)

func refreshConfig(fake *fakeConn, groupFilter string) Config {
	cfg := baseConfig()
	cfg.GroupBaseDN = groupsBase
	cfg.GroupFilter = groupFilter
	cfg.Dial = func(context.Context, Config) (Conn, error) { return fake, nil }
	return cfg
}

func ldapRefreshRequest(dn string) authpkg.RefreshRequest {
	return authpkg.RefreshRequest{
		UserID:  uuid.New(),
		Issuer:  ProviderName,
		Subject: dn,
		Grant:   authpkg.RefreshGrant{Provider: ProviderName, Issuer: ProviderName, Handle: authpkg.NewRefreshHandle(dn)},
	}
}

func requireLDAPRefreshError(t *testing.T, err error, kind authpkg.RefreshErrorKind, reason string) *authpkg.RefreshError {
	t.Helper()
	refreshErr, ok := err.(*authpkg.RefreshError)
	require.Truef(t, ok, "want *RefreshError, got %T: %v", err, err)
	require.Equal(t, kind, refreshErr.Kind, "kind of %v", err)
	require.Equal(t, reason, refreshErr.Reason)
	require.NotContains(t, err.Error(), baseConfig().BindPassword)
	return refreshErr
}

func TestRefreshGroupsRequeriesByDN(t *testing.T) {
	fake := &fakeConn{
		entries: map[string]*gldap.Entry{
			aliceDN: gldap.NewEntry(aliceDN, map[string][]string{"uid": {"alice*"}}),
		},
		groupResult: &gldap.SearchResult{Entries: []*gldap.Entry{
			gldap.NewEntry("cn=eng,"+groupsBase, map[string][]string{"cn": {"eng"}}),
			gldap.NewEntry("cn=ops,"+groupsBase, map[string][]string{"cn": {"ops", "eng"}}),
		}},
	}
	provider, err := New(refreshConfig(fake, "(|(member={dn})(memberUid={username}))"))
	require.NoError(t, err)

	req := ldapRefreshRequest(aliceDN)
	result, err := provider.RefreshGroups(t.Context(), req)
	require.NoError(t, err)
	require.True(t, result.GroupsKnown)
	require.Equal(t, []string{"eng", "ops"}, result.Groups)
	require.False(t, result.Rotated)
	require.Equal(t, aliceDN, result.Handle.Reveal())

	require.Equal(t, []bindCall{{dn: baseConfig().BindDN, password: baseConfig().BindPassword}}, fake.binds,
		"only the service account binds; the user's password is never needed")
	require.Len(t, fake.searches, 2)
	lookup := fake.searches[0]
	require.Equal(t, aliceDN, lookup.BaseDN)
	require.Equal(t, gldap.ScopeBaseObject, lookup.Scope)
	require.Equal(t, "(objectClass=*)", lookup.Filter)
	require.Equal(t, []string{DefaultUsernameAttribute}, lookup.Attributes)
	require.Equal(t, groupsBase, fake.searches[1].BaseDN)
	require.Equal(t, `(|(member=uid=al\28ice\29,ou=People,dc=example,dc=com)(memberUid=alice\2a))`, fake.searches[1].Filter)
	require.Equal(t, 1, fake.closes)
}

// TestRefreshGroupsUsesTheCurrentUsername is R6: the group search uses the
// username the entry has now, never one remembered from login.
func TestRefreshGroupsUsesTheCurrentUsername(t *testing.T) {
	t.Run("rename", func(t *testing.T) {
		fake := &fakeConn{entries: map[string]*gldap.Entry{
			aliceDN: gldap.NewEntry(aliceDN, map[string][]string{"uid": {"alice-renamed"}}),
		}}
		provider, err := New(refreshConfig(fake, "(memberUid={username})"))
		require.NoError(t, err)
		_, err = provider.RefreshGroups(t.Context(), ldapRefreshRequest(aliceDN))
		require.NoError(t, err)
		require.Equal(t, "(memberUid=alice-renamed)", fake.searches[1].Filter)
	})
	t.Run("reassignment", func(t *testing.T) {
		// "alice" now belongs to another entry, and alice's entry carries a
		// new uid: the lookup by DN decides, so the search never uses the
		// reassigned name.
		const newAliceDN = "uid=alice,ou=People,dc=example,dc=com"
		fake := &fakeConn{entries: map[string]*gldap.Entry{
			aliceDN:    gldap.NewEntry(aliceDN, map[string][]string{"uid": {"alice-old"}}),
			newAliceDN: gldap.NewEntry(newAliceDN, map[string][]string{"uid": {"alice"}}),
		}}
		provider, err := New(refreshConfig(fake, "(memberUid={username})"))
		require.NoError(t, err)
		_, err = provider.RefreshGroups(t.Context(), ldapRefreshRequest(aliceDN))
		require.NoError(t, err)
		require.Equal(t, aliceDN, fake.searches[0].BaseDN)
		require.Equal(t, "(memberUid=alice-old)", fake.searches[1].Filter)
	})
	t.Run("missing username attribute", func(t *testing.T) {
		fake := &fakeConn{entries: map[string]*gldap.Entry{aliceDN: gldap.NewEntry(aliceDN, nil)}}
		provider, err := New(refreshConfig(fake, "(memberUid={username})"))
		require.NoError(t, err)
		result, err := provider.RefreshGroups(t.Context(), ldapRefreshRequest(aliceDN))
		requireLDAPRefreshError(t, err, authpkg.RefreshTransient, "missing_username_attribute")
		require.Zero(t, result.Groups)
		require.Len(t, fake.searches, 1, "no group search with an empty username")
	})
	t.Run("missing username attribute is fine for a DN-only filter", func(t *testing.T) {
		fake := &fakeConn{
			entries:     map[string]*gldap.Entry{aliceDN: gldap.NewEntry(aliceDN, nil)},
			groupResult: &gldap.SearchResult{Entries: []*gldap.Entry{gldap.NewEntry("cn=eng,"+groupsBase, map[string][]string{"cn": {"eng"}})}},
		}
		provider, err := New(refreshConfig(fake, "(member=%s)"))
		require.NoError(t, err)
		result, err := provider.RefreshGroups(t.Context(), ldapRefreshRequest(aliceDN))
		require.NoError(t, err)
		require.Equal(t, []string{"eng"}, result.Groups)
	})
	t.Run("dn as the username attribute", func(t *testing.T) {
		fake := &fakeConn{entries: map[string]*gldap.Entry{aliceDN: gldap.NewEntry(aliceDN, nil)}}
		cfg := refreshConfig(fake, "(memberUid={username})")
		cfg.UsernameAttribute = "dn"
		provider, err := New(cfg)
		require.NoError(t, err)
		_, err = provider.RefreshGroups(t.Context(), ldapRefreshRequest(aliceDN))
		require.NoError(t, err)
		require.Equal(t, []string{"1.1"}, fake.searches[0].Attributes)
		require.Equal(t, `(memberUid=uid=al\28ice\29,ou=People,dc=example,dc=com)`, fake.searches[1].Filter)
	})
}

func TestRefreshGroupsUserGoneIsDefinitive(t *testing.T) {
	for name, fake := range map[string]*fakeConn{
		"no such object":       {entries: map[string]*gldap.Entry{}},
		"no entry":             {entries: map[string]*gldap.Entry{aliceDN: nil}},
		"wrapped noSuchObject": {baseSearchErr: gldap.NewError(gldap.LDAPResultNoSuchObject, errors.New("gone"))},
	} {
		t.Run(name, func(t *testing.T) {
			provider, err := New(refreshConfig(fake, "(member={dn})"))
			require.NoError(t, err)
			_, err = provider.RefreshGroups(t.Context(), ldapRefreshRequest(aliceDN))
			refreshErr := requireLDAPRefreshError(t, err, authpkg.RefreshDefinitive, "user_not_found")
			require.Equal(t, authpkg.DenialOfSubject, refreshErr.Scope, "a vanished entry revokes even an uncertain grant")
			require.Len(t, fake.searches, 2, "the user lookup, then the user-base check")
			require.Equal(t, baseConfig().UserBaseDN, fake.searches[1].BaseDN)
			require.Equal(t, gldap.ScopeBaseObject, fake.searches[1].Scope)
		})
	}
}

// TestRefreshGroupsInvisibleUserBaseIsNotAbsence: a directory that cannot
// show the user base (a replica mid-resync, a service account that lost read
// access) answers noSuchObject for everyone, which must not revoke everyone.
func TestRefreshGroupsInvisibleUserBaseIsNotAbsence(t *testing.T) {
	fake := &fakeConn{entries: map[string]*gldap.Entry{}, userBaseHidden: true}
	provider, err := New(refreshConfig(fake, "(member={dn})"))
	require.NoError(t, err)
	_, err = provider.RefreshGroups(t.Context(), ldapRefreshRequest(aliceDN))
	requireLDAPRefreshError(t, err, authpkg.RefreshTransient, "user_base_not_visible")
}

func TestRefreshGroupsTransientFailures(t *testing.T) {
	entries := func() map[string]*gldap.Entry {
		return map[string]*gldap.Entry{aliceDN: gldap.NewEntry(aliceDN, map[string][]string{"uid": {"alice"}})}
	}
	for _, tc := range []struct {
		name   string
		fake   *fakeConn
		dial   error
		req    func(*authpkg.RefreshRequest)
		reason string
	}{
		{name: "dial failure", dial: gldap.NewError(gldap.ErrorNetwork, &net.OpError{Op: "dial", Err: errors.New("refused")}), reason: "connect_failed"},
		{name: "service bind failure", fake: &fakeConn{entries: entries(), serviceErr: gldap.NewError(gldap.LDAPResultInvalidCredentials, errors.New("bad"))}, reason: "service_bind_failed"},
		{name: "lookup failure", fake: &fakeConn{baseSearchErr: gldap.NewError(gldap.LDAPResultBusy, errors.New("busy"))}, reason: "user_lookup_failed"},
		{name: "insufficient access is not absence", fake: &fakeConn{baseSearchErr: gldap.NewError(gldap.LDAPResultInsufficientAccessRights, errors.New("acl"))}, reason: "user_lookup_failed"},
		{name: "group search failure", fake: &fakeConn{entries: entries(), groupSearchErr: errors.New("directory unavailable")}, reason: "group_search_failed"},
		{name: "grant for another provider", req: func(r *authpkg.RefreshRequest) { r.Grant.Provider = "oidc" }, reason: "provider_mismatch"},
		{name: "grant from another issuer", req: func(r *authpkg.RefreshRequest) { r.Grant.Issuer = "https://idp" }, reason: "issuer_mismatch"},
		{name: "user from another issuer", req: func(r *authpkg.RefreshRequest) { r.Issuer = "https://idp" }, reason: "issuer_mismatch"},
		{name: "grant for another entry", req: func(r *authpkg.RefreshRequest) { r.Subject = "uid=bob,ou=People,dc=example,dc=com" }, reason: "subject_mismatch"},
		{name: "empty grant", req: func(r *authpkg.RefreshRequest) { r.Grant.Handle = authpkg.NewRefreshHandle(" ") }, reason: "empty_grant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dialed := false
			cfg := refreshConfig(tc.fake, "(member={dn})")
			cfg.Dial = func(context.Context, Config) (Conn, error) {
				dialed = true
				if tc.dial != nil {
					return nil, tc.dial
				}
				if tc.fake == nil {
					return &fakeConn{entries: entries()}, nil
				}
				return tc.fake, nil
			}
			provider, err := New(cfg)
			require.NoError(t, err)
			req := ldapRefreshRequest(aliceDN)
			if tc.req != nil {
				tc.req(&req)
			}
			result, err := provider.RefreshGroups(t.Context(), req)
			requireLDAPRefreshError(t, err, authpkg.RefreshTransient, tc.reason)
			require.Zero(t, result.Groups)
			if tc.req != nil {
				require.False(t, dialed, "a grant this provider will not redeem never reaches the directory")
			}
			if tc.fake != nil {
				require.Equal(t, 1, tc.fake.closes)
			}
		})
	}
}

// TestRefreshGroupsHonoursTheContext is R8: Bind and Search take no context,
// so the provider closes the connection when the caller's deadline passes,
// which unblocks them; the outcome is transient and the connection is closed
// exactly once.
func TestRefreshGroupsHonoursTheContext(t *testing.T) {
	entries := map[string]*gldap.Entry{aliceDN: gldap.NewEntry(aliceDN, map[string][]string{"uid": {"alice"}})}
	for _, tc := range []struct {
		name string
		fake *fakeConn
	}{
		{name: "blocked service bind", fake: &fakeConn{entries: entries, blockBind: true}},
		{name: "blocked user lookup", fake: &fakeConn{entries: entries, blockSearchBase: aliceDN}},
		{name: "blocked group search", fake: &fakeConn{entries: entries, blockSearchBase: groupsBase}},
	} {
		t.Run(tc.name+" on cancel", func(t *testing.T) {
			tc.fake.blocked = make(chan struct{})
			provider, err := New(refreshConfig(tc.fake, "(member={dn})"))
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			go func() {
				<-tc.fake.blocked
				cancel()
			}()
			done := make(chan error, 1)
			go func() {
				_, err := provider.RefreshGroups(ctx, ldapRefreshRequest(aliceDN))
				done <- err
			}()
			select {
			case err := <-done:
				requireLDAPRefreshError(t, err, authpkg.RefreshTransient, "context_done")
			case <-time.After(5 * time.Second):
				t.Fatal("RefreshGroups did not return after the context was cancelled")
			}
			tc.fake.mu.Lock()
			defer tc.fake.mu.Unlock()
			require.Equal(t, 1, tc.fake.closes)
		})
	}
	t.Run("deadline bounds the request timeout", func(t *testing.T) {
		fake := &fakeConn{entries: entries, blockSearchBase: aliceDN, blocked: make(chan struct{})}
		provider, err := New(refreshConfig(fake, "(member={dn})"))
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err = provider.RefreshGroups(ctx, ldapRefreshRequest(aliceDN))
		requireLDAPRefreshError(t, err, authpkg.RefreshTransient, "context_done")
		require.Less(t, time.Since(start), 5*time.Second)
		require.LessOrEqual(t, fake.timeout, 200*time.Millisecond)
		require.Positive(t, fake.timeout)
	})
	t.Run("already cancelled", func(t *testing.T) {
		dialed := false
		cfg := refreshConfig(&fakeConn{}, "(member={dn})")
		cfg.Dial = func(context.Context, Config) (Conn, error) { dialed = true; return &fakeConn{}, nil }
		provider, err := New(cfg)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err = provider.RefreshGroups(ctx, ldapRefreshRequest(aliceDN))
		requireLDAPRefreshError(t, err, authpkg.RefreshTransient, "context_done")
		require.False(t, dialed)
	})
}

func TestDefaultDialHonoursTheContext(t *testing.T) {
	// A listener that accepts but never speaks TLS: the LDAPS handshake
	// blocks until the context ends.
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	cfg := baseConfig()
	cfg.URL = "ldaps://" + listener.Addr().String()
	cfg.Timeout = time.Minute
	normalized, err := cfg.normalize()
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = defaultDial(ctx, normalized)
	require.Error(t, err)
	require.Less(t, time.Since(start), 10*time.Second)

	cancelled, cancelNow := context.WithCancel(t.Context())
	cancelNow()
	_, err = defaultDial(cancelled, normalized)
	require.ErrorIs(t, err, context.Canceled)
}
