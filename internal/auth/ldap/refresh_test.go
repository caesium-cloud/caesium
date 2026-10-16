package ldap

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
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
	return ldapRefreshRequestAs(dn, "alice")
}

// ldapRefreshRequestAs builds a request for a user who signed in as login.
func ldapRefreshRequestAs(dn, login string) authpkg.RefreshRequest {
	return authpkg.RefreshRequest{
		UserID:  uuid.New(),
		Issuer:  ProviderName,
		Subject: dn,
		Grant: authpkg.RefreshGrant{
			Provider: ProviderName, Issuer: ProviderName, Handle: authpkg.NewRefreshHandle(dn), Login: login,
		},
	}
}

// equalityAdmits evaluates a single-equality filter "(attr=value)" against
// the entry, case-insensitively, as a directory would for uid or mail.
func equalityAdmits(entry *gldap.Entry, filter string) bool {
	inner, ok := strings.CutPrefix(filter, "(")
	if !ok {
		return false
	}
	inner, ok = strings.CutSuffix(inner, ")")
	if !ok || strings.ContainsAny(inner, "()&|!") {
		panic("equalityAdmits handles a single equality filter only: " + filter)
	}
	attr, escaped, ok := strings.Cut(inner, "=")
	if !ok {
		return false
	}
	var value strings.Builder
	for i := 0; i < len(escaped); i++ {
		if escaped[i] == '\\' && i+2 < len(escaped) {
			decoded, err := hex.DecodeString(escaped[i+1 : i+3])
			if err != nil {
				return false
			}
			value.Write(decoded)
			i += 2
			continue
		}
		value.WriteByte(escaped[i])
	}
	for _, v := range entry.GetEqualFoldAttributeValues(attr) {
		if strings.EqualFold(v, value.String()) {
			return true
		}
	}
	return false
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
		admits: equalityAdmits,
		groupResult: &gldap.SearchResult{Entries: []*gldap.Entry{
			gldap.NewEntry("cn=eng,"+groupsBase, map[string][]string{"cn": {"eng"}}),
			gldap.NewEntry("cn=ops,"+groupsBase, map[string][]string{"cn": {"ops", "eng"}}),
		}},
	}
	provider, err := New(refreshConfig(fake, "(|(member={dn})(memberUid={username}))"))
	require.NoError(t, err)

	req := ldapRefreshRequestAs(aliceDN, "alice*")
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
	require.Equal(t, `(uid=alice\2a)`, lookup.Filter, "the login's own predicate, rendered with the typed name")
	require.Equal(t, []string{DefaultUsernameAttribute}, lookup.Attributes)
	require.Equal(t, groupsBase, fake.searches[1].BaseDN)
	require.Equal(t, `(|(member=uid=al\28ice\29,ou=People,dc=example,dc=com)(memberUid=alice\2a))`, fake.searches[1].Filter)
	require.Equal(t, 1, fake.closes)
}

// TestRefreshGroupsUsesTheCurrentUsername is R6: the group search uses the
// username the entry has now, never one remembered from login.
func TestRefreshGroupsUsesTheCurrentUsername(t *testing.T) {
	// The users sign in by mail, so their uid can change without changing
	// what login admits; the group search must follow the current uid.
	mailLogin := func(fake *fakeConn) Config {
		cfg := refreshConfig(fake, "(memberUid={username})")
		cfg.UserFilter = "(mail={username})"
		return cfg
	}
	t.Run("rename", func(t *testing.T) {
		fake := &fakeConn{admits: equalityAdmits, entries: map[string]*gldap.Entry{
			aliceDN: gldap.NewEntry(aliceDN, map[string][]string{"uid": {"alice-renamed"}, "mail": {"alice@example.com"}}),
		}}
		provider, err := New(mailLogin(fake))
		require.NoError(t, err)
		_, err = provider.RefreshGroups(t.Context(), ldapRefreshRequestAs(aliceDN, "alice@example.com"))
		require.NoError(t, err)
		require.Equal(t, "(memberUid=alice-renamed)", fake.searches[1].Filter)
	})
	t.Run("reassignment", func(t *testing.T) {
		// "alice" now belongs to another entry, and alice's entry carries a
		// new uid: the lookup by DN decides, so the search never uses the
		// reassigned name.
		const newAliceDN = "uid=alice,ou=People,dc=example,dc=com"
		fake := &fakeConn{admits: equalityAdmits, entries: map[string]*gldap.Entry{
			aliceDN:    gldap.NewEntry(aliceDN, map[string][]string{"uid": {"alice-old"}, "mail": {"alice@example.com"}}),
			newAliceDN: gldap.NewEntry(newAliceDN, map[string][]string{"uid": {"alice"}, "mail": {"new-alice@example.com"}}),
		}}
		provider, err := New(mailLogin(fake))
		require.NoError(t, err)
		_, err = provider.RefreshGroups(t.Context(), ldapRefreshRequestAs(aliceDN, "alice@example.com"))
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
	for name, tc := range map[string]struct {
		fake   *fakeConn
		reason string
	}{
		"no such object":       {fake: &fakeConn{entries: map[string]*gldap.Entry{}}, reason: "user_not_found"},
		"wrapped noSuchObject": {fake: &fakeConn{baseSearchErr: gldap.NewError(gldap.LDAPResultNoSuchObject, errors.New("gone"))}, reason: "user_not_found"},
		"no matching entry":    {fake: &fakeConn{entries: map[string]*gldap.Entry{aliceDN: nil}}, reason: "user_not_admitted"},
	} {
		t.Run(name, func(t *testing.T) {
			provider, err := New(refreshConfig(tc.fake, "(member={dn})"))
			require.NoError(t, err)
			_, err = provider.RefreshGroups(t.Context(), ldapRefreshRequest(aliceDN))
			refreshErr := requireLDAPRefreshError(t, err, authpkg.RefreshDefinitive, tc.reason)
			require.Equal(t, authpkg.DenialOfSubject, refreshErr.Scope, "a vanished entry revokes even an uncertain grant")
			require.Len(t, tc.fake.searches, 2, "the user lookup, then the user-base check")
			require.Equal(t, baseConfig().UserBaseDN, tc.fake.searches[1].BaseDN)
			require.Equal(t, gldap.ScopeBaseObject, tc.fake.searches[1].Scope)
		})
	}
}

// TestRefreshGroupsReappliesTheLoginFilter covers the review finding on
// #646: an account the login filter now excludes must stop refreshing, while
// a filter on another attribute than the username attribute must not revoke.
func TestRefreshGroupsReappliesTheLoginFilter(t *testing.T) {
	const gate = "cn=caesium-users,ou=Groups,dc=example,dc=com"
	excluded := func(*gldap.Entry, string) bool { return false } // the directory evaluates the filter to false

	t.Run("disabled through the filter", func(t *testing.T) {
		fake := &fakeConn{admits: excluded, entries: map[string]*gldap.Entry{
			aliceDN: gldap.NewEntry(aliceDN, map[string][]string{"uid": {"alice"}}),
		}}
		cfg := refreshConfig(fake, "(member={dn})")
		cfg.UserFilter = "(&(uid={username})(!(userAccountControl:1.2.840.113556.1.4.803:=2)))"
		provider, err := New(cfg)
		require.NoError(t, err)
		_, err = provider.RefreshGroups(t.Context(), ldapRefreshRequest(aliceDN))
		refreshErr := requireLDAPRefreshError(t, err, authpkg.RefreshDefinitive, "user_not_admitted")
		require.Equal(t, authpkg.DenialOfSubject, refreshErr.Scope)
		require.Equal(t, aliceDN, fake.searches[0].BaseDN)
		require.Equal(t, gldap.ScopeBaseObject, fake.searches[0].Scope)
		require.Equal(t, "(&(uid=alice)(!(userAccountControl:1.2.840.113556.1.4.803:=2)))", fake.searches[0].Filter)
	})
	t.Run("removed from the gate group", func(t *testing.T) {
		fake := &fakeConn{admits: excluded, entries: map[string]*gldap.Entry{
			aliceDN: gldap.NewEntry(aliceDN, map[string][]string{"uid": {"alice"}}),
		}}
		cfg := refreshConfig(fake, "(member={dn})")
		cfg.UserFilter = "(&(uid={username})(memberOf=" + gate + "))"
		provider, err := New(cfg)
		require.NoError(t, err)
		_, err = provider.RefreshGroups(t.Context(), ldapRefreshRequest(aliceDN))
		requireLDAPRefreshError(t, err, authpkg.RefreshDefinitive, "user_not_admitted")
		require.Equal(t, "(&(uid=alice)(memberOf="+gate+"))", fake.searches[0].Filter)
	})
	t.Run("moved out of the user base", func(t *testing.T) {
		const formerDN = "uid=alice,ou=Former,dc=example,dc=com"
		fake := &fakeConn{entries: map[string]*gldap.Entry{
			formerDN: gldap.NewEntry(formerDN, map[string][]string{"uid": {"alice"}}),
		}}
		provider, err := New(refreshConfig(fake, "(member={dn})"))
		require.NoError(t, err)
		_, err = provider.RefreshGroups(t.Context(), ldapRefreshRequest(formerDN))
		requireLDAPRefreshError(t, err, authpkg.RefreshDefinitive, "user_outside_base")
		require.Len(t, fake.searches, 1, "only the user-base check; the entry is never looked up")
		require.Equal(t, baseConfig().UserBaseDN, fake.searches[0].BaseDN)
	})
	t.Run("a look-alike base is not the base", func(t *testing.T) {
		// A raw suffix test would accept this DN: its last characters spell
		// the base, but its RDNs do not.
		const lookAlike = "uid=alice,ou=Evil\\,ou=People,dc=example,dc=com"
		fake := &fakeConn{entries: map[string]*gldap.Entry{lookAlike: gldap.NewEntry(lookAlike, nil)}}
		cfg := refreshConfig(fake, "(member={dn})")
		cfg.UserBaseDN = "ou=People,dc=example,dc=com"
		provider, err := New(cfg)
		require.NoError(t, err)
		within, err := dnWithin(lookAlike, cfg.UserBaseDN)
		require.NoError(t, err)
		require.False(t, within)
		within, err = dnWithin("uid=x,ou=EvilPeople,dc=example,dc=com", "ou=People,dc=example,dc=com")
		require.NoError(t, err)
		require.False(t, within)
		_, err = provider.RefreshGroups(t.Context(), ldapRefreshRequest(lookAlike))
		requireLDAPRefreshError(t, err, authpkg.RefreshDefinitive, "user_outside_base")
	})
	t.Run("the base compares case-insensitively", func(t *testing.T) {
		const shouted = "UID=alice,OU=people,DC=Example,DC=COM"
		fake := &fakeConn{admits: equalityAdmits, entries: map[string]*gldap.Entry{
			shouted: gldap.NewEntry(shouted, map[string][]string{"uid": {"alice"}}),
		}}
		provider, err := New(refreshConfig(fake, "(member={dn})"))
		require.NoError(t, err)
		_, err = provider.RefreshGroups(t.Context(), ldapRefreshRequest(shouted))
		require.NoError(t, err)
		within, err := dnWithin(baseConfig().UserBaseDN, baseConfig().UserBaseDN)
		require.NoError(t, err)
		require.True(t, within, "the base entry itself is within the base, as a subtree search includes it")
	})
	t.Run("an outside DN with an invisible base is not revoked", func(t *testing.T) {
		const formerDN = "uid=alice,ou=Former,dc=example,dc=com"
		fake := &fakeConn{userBaseHidden: true}
		provider, err := New(refreshConfig(fake, "(member={dn})"))
		require.NoError(t, err)
		_, err = provider.RefreshGroups(t.Context(), ldapRefreshRequest(formerDN))
		requireLDAPRefreshError(t, err, authpkg.RefreshTransient, "user_base_not_visible")
	})
	t.Run("a filter on another attribute than the username attribute must not revoke", func(t *testing.T) {
		// Login renders (mail={username}) with what the user typed, while
		// the username attribute is uid. Rendering the filter with the uid
		// would match nothing and revoke every such user.
		fake := &fakeConn{admits: equalityAdmits, entries: map[string]*gldap.Entry{
			aliceDN: gldap.NewEntry(aliceDN, map[string][]string{"uid": {"alice"}, "mail": {"alice@example.com"}}),
		}, groupResult: &gldap.SearchResult{Entries: []*gldap.Entry{gldap.NewEntry("cn=eng,"+groupsBase, map[string][]string{"cn": {"eng"}})}}}
		cfg := refreshConfig(fake, "(memberUid={username})")
		cfg.UserFilter = "(mail={username})"
		provider, err := New(cfg)
		require.NoError(t, err)
		result, err := provider.RefreshGroups(t.Context(), ldapRefreshRequestAs(aliceDN, "alice@example.com"))
		require.NoError(t, err)
		require.Equal(t, []string{"eng"}, result.Groups)
		require.Equal(t, "(mail=alice@example.com)", fake.searches[0].Filter)
		require.Equal(t, "(memberUid=alice)", fake.searches[1].Filter, "groups still follow the uid")
	})
	t.Run("a renamed login attribute is denied", func(t *testing.T) {
		// The user signed in as "alice"; uid is now "alice2", so the same
		// login would fail. Re-login is the expected remedy.
		fake := &fakeConn{admits: equalityAdmits, entries: map[string]*gldap.Entry{
			aliceDN: gldap.NewEntry(aliceDN, map[string][]string{"uid": {"alice2"}}),
		}}
		provider, err := New(refreshConfig(fake, "(member={dn})"))
		require.NoError(t, err)
		_, err = provider.RefreshGroups(t.Context(), ldapRefreshRequestAs(aliceDN, "alice"))
		requireLDAPRefreshError(t, err, authpkg.RefreshDefinitive, "user_not_admitted")
	})
	t.Run("a grant without its login name is not refreshed", func(t *testing.T) {
		dialed := false
		cfg := refreshConfig(&fakeConn{}, "(member={dn})")
		cfg.Dial = func(context.Context, Config) (Conn, error) { dialed = true; return &fakeConn{}, nil }
		provider, err := New(cfg)
		require.NoError(t, err)
		_, err = provider.RefreshGroups(t.Context(), ldapRefreshRequestAs(aliceDN, " "))
		requireLDAPRefreshError(t, err, authpkg.RefreshTransient, "missing_login")
		require.False(t, dialed)
	})
	t.Run("an unparsable DN is not refreshed", func(t *testing.T) {
		provider, err := New(refreshConfig(&fakeConn{}, "(member={dn})"))
		require.NoError(t, err)
		_, err = provider.RefreshGroups(t.Context(), ldapRefreshRequest("not a dn"))
		requireLDAPRefreshError(t, err, authpkg.RefreshTransient, "invalid_dn")
	})
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

// startTLSServer is a minimal directory that answers a StartTLS extended
// request with success and then either never speaks TLS (stall) or completes
// a real TLS handshake.
func startTLSServer(t *testing.T, stall bool) string {
	t.Helper()
	cert := selfSignedCert(t)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				id, err := readLDAPMessageID(conn)
				if err != nil {
					return
				}
				// ExtendedResponse [APPLICATION 24] { resultCode success,
				// matchedDN "", diagnosticMessage "" }.
				body := append([]byte{0x02, byte(len(id))}, id...)
				body = append(body, 0x78, 0x07, 0x0a, 0x01, 0x00, 0x04, 0x00, 0x04, 0x00)
				if _, err := conn.Write(append([]byte{0x30, byte(len(body))}, body...)); err != nil {
					return
				}
				if stall {
					_, _ = io.Copy(io.Discard, conn)
					return
				}
				tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
				if err := tlsConn.Handshake(); err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, tlsConn)
			}()
		}
	}()
	return listener.Addr().String()
}

// readLDAPMessageID reads one LDAPMessage and returns its messageID bytes.
func readLDAPMessageID(r io.Reader) ([]byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	if header[0] != 0x30 {
		return nil, errors.New("not an LDAPMessage")
	}
	length := int(header[1])
	if length&0x80 != 0 {
		size := make([]byte, length&0x7f)
		if _, err := io.ReadFull(r, size); err != nil {
			return nil, err
		}
		length = 0
		for _, b := range size {
			length = length<<8 | int(b)
		}
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	if len(body) < 3 || body[0] != 0x02 || int(body[1]) > len(body)-2 {
		return nil, errors.New("no messageID")
	}
	return body[2 : 2+int(body[1])], nil
}

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func startTLSConfig(t *testing.T, addr string) Config {
	t.Helper()
	cfg := baseConfig()
	cfg.URL = "ldap://" + addr
	cfg.StartTLS = true
	cfg.TLSConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // the test server's certificate is generated per run.
	cfg.Timeout = time.Minute
	normalized, err := cfg.normalize()
	require.NoError(t, err)
	return normalized
}

// TestDefaultDialStartTLSCancellation covers the pre-PR review finding on
// #646: StartTLS swaps the connection's transport without a lock, so
// cancellation closes the raw socket instead of calling Conn.Close. Run with
// -race.
func TestDefaultDialStartTLSCancellation(t *testing.T) {
	t.Run("upgrade completes", func(t *testing.T) {
		conn, err := defaultDial(t.Context(), startTLSConfig(t, startTLSServer(t, false)))
		require.NoError(t, err)
		require.NoError(t, conn.Close())
	})
	t.Run("cancelled during a stalled handshake", func(t *testing.T) {
		cfg := startTLSConfig(t, startTLSServer(t, true))
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := defaultDial(ctx, cfg)
		require.Error(t, err)
		require.Less(t, time.Since(start), 10*time.Second)
	})
	t.Run("cancelled racing the upgrade", func(t *testing.T) {
		cfg := startTLSConfig(t, startTLSServer(t, false))
		for i := range 40 {
			ctx, cancel := context.WithCancel(t.Context())
			delay := time.Duration(i%8) * 250 * time.Microsecond
			go func() {
				time.Sleep(delay)
				cancel()
			}()
			conn, err := defaultDial(ctx, cfg)
			if err == nil {
				require.NoError(t, conn.Close())
			}
			cancel()
		}
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
