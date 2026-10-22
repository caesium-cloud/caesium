package ldap

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	authpkg "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/pkg/log"
	gldap "github.com/go-ldap/ldap/v3"
)

var _ authpkg.GroupRefresher = (*Provider)(nil)

// RefreshGroups re-applies the login admission check and re-reads the user's
// groups with the service account. The grant is the user DN plus the
// username the user typed at login. A refresh succeeds only while login would
// still admit the same entry:
//
//   - the DN is still under the configured user base (compared RDN by RDN,
//     case-insensitively, never as raw text), and
//   - the login's own predicate, the user filter rendered with the typed
//     username, still matches the entry at that DN (a base-scope search).
//
// So an account the filter now excludes (an Active Directory account disabled
// through userAccountControl, a user removed from a memberOf gate group, an
// entry moved out of a narrowed base) stops refreshing, as its login would
// fail. The filter is rendered with the TYPED username, exactly as at login,
// because a filter such as (mail={username}) need not match the username
// attribute. The group search then uses the entry's CURRENT username
// attribute, read the way login reads it and never stored, so a rename or a
// reassigned username cannot pull in someone else's groups.
//
// Classification (see authpkg.RefreshErrorKind):
//   - definitive (DenialOfSubject): the entry is gone (user_not_found), no
//     longer matches the login filter (user_not_admitted), or lies outside
//     the user base (user_outside_base), in every case only while the
//     configured user base is still visible to the service account;
//   - transient: everything else. An LDAP refresh only reads: it redeems and
//     rotates nothing, so no failure can leave the grant in an uncertain
//     state. That covers a dial, bind or search failure, the context ending
//     (the connection is closed to unblock a pending bind or search, and an
//     answer read after that is never treated as definitive), an invisible
//     user base, a grant for another provider or without its login name, and
//     an entry without the username attribute the group filter needs.
func (p *Provider) RefreshGroups(ctx context.Context, req authpkg.RefreshRequest) (authpkg.GroupRefresh, error) {
	grant := req.Grant
	if grant.Provider != ProviderName {
		return authpkg.GroupRefresh{}, transientRefresh("provider_mismatch", nil)
	}
	if grant.Issuer != ProviderName || req.Issuer != ProviderName {
		return authpkg.GroupRefresh{}, transientRefresh("issuer_mismatch", nil)
	}
	dn := grant.Handle.Reveal()
	if strings.TrimSpace(dn) == "" {
		return authpkg.GroupRefresh{}, transientRefresh("empty_grant", nil)
	}
	// The LDAP subject is the DN, so the grant must name the user's own
	// entry.
	if dn != req.Subject {
		log.Warn("ldap refresh grant does not name the user's entry; not refreshing", "user_id", req.UserID.String())
		return authpkg.GroupRefresh{}, transientRefresh("subject_mismatch", nil)
	}
	if strings.TrimSpace(grant.Login) == "" {
		log.Warn("ldap refresh grant carries no login name; not refreshing", "user_id", req.UserID.String())
		return authpkg.GroupRefresh{}, transientRefresh("missing_login", nil)
	}
	underBase, err := dnWithin(dn, p.cfg.UserBaseDN)
	if err != nil {
		return authpkg.GroupRefresh{}, transientRefresh("invalid_dn", err)
	}
	if err := ctx.Err(); err != nil {
		return authpkg.GroupRefresh{}, transientRefresh("context_done", err)
	}

	conn, err := p.dial(ctx, p.cfg)
	if err != nil {
		return authpkg.GroupRefresh{}, transientRefresh("connect_failed", err)
	}
	// Bind and Search take no context: closing the connection when ctx ends
	// unblocks them. Close runs exactly once, whichever side gets there.
	var closeOnce sync.Once
	closeConn := func() { closeOnce.Do(func() { _ = conn.Close() }) }
	stop := context.AfterFunc(ctx, closeConn)
	defer func() {
		stop()
		closeConn()
	}()
	conn.SetTimeout(p.refreshTimeout(ctx))

	if err := conn.Bind(p.cfg.BindDN, p.cfg.BindPassword); err != nil {
		return authpkg.GroupRefresh{}, transientRefresh(contextReason(ctx, "service_bind_failed"), err)
	}
	if !underBase {
		return authpkg.GroupRefresh{}, p.denySubject(ctx, conn, req, "user_outside_base")
	}
	entry, absence, err := p.admitUserByDN(conn, dn, grant.Login)
	if err != nil {
		return authpkg.GroupRefresh{}, transientRefresh(contextReason(ctx, "user_lookup_failed"), err)
	}
	if entry == nil {
		return authpkg.GroupRefresh{}, p.denySubject(ctx, conn, req, absence)
	}

	username := firstEntryValue(entry, p.cfg.UsernameAttribute)
	if username == "" && strings.Contains(p.cfg.GroupFilter, "{username}") {
		log.Warn("ldap user entry lacks the username attribute the group filter needs; groups not refreshed",
			"user_id", req.UserID.String(), "attribute", p.cfg.UsernameAttribute)
		return authpkg.GroupRefresh{}, transientRefresh("missing_username_attribute", nil)
	}
	groups, err := p.searchGroups(ctx, conn, dn, username)
	if err != nil {
		return authpkg.GroupRefresh{}, transientRefresh(contextReason(ctx, "group_search_failed"), err)
	}
	return authpkg.GroupRefresh{Groups: groups, GroupsKnown: true, Handle: grant.Handle}, nil
}

// denySubject turns "login would no longer admit this user" into a
// definitive denial, but only from a directory that still shows the user
// base. A replica mid-resync, or a service account that lost read access,
// answers as if every user were gone, and that must not revoke the whole LDAP
// population at once.
func (p *Provider) denySubject(ctx context.Context, conn Conn, req authpkg.RefreshRequest, reason string) error {
	if ctx.Err() != nil {
		return transientRefresh("context_done", ctx.Err())
	}
	if err := p.userBaseVisible(conn); err != nil {
		log.Warn("ldap user base is not visible to the service account; not treating the user as gone",
			"user_id", req.UserID.String(), "user_base_dn", p.cfg.UserBaseDN)
		return transientRefresh(contextReason(ctx, "user_base_not_visible"), err)
	}
	return authpkg.NewRefreshDenial(authpkg.DenialOfSubject, reason)
}

// dnWithin reports whether dn is base or lies under it, comparing parsed
// RDNs case-insensitively (a raw suffix test would accept
// "uid=x,ou=evilou=people,…" and reject differently cased or escaped forms).
func dnWithin(dn, base string) (bool, error) {
	parsedDN, err := gldap.ParseDN(dn)
	if err != nil {
		return false, fmt.Errorf("parse user DN: %w", err)
	}
	parsedBase, err := gldap.ParseDN(base)
	if err != nil {
		return false, fmt.Errorf("parse user base DN: %w", err)
	}
	return parsedBase.EqualFold(parsedDN) || parsedBase.AncestorOfFold(parsedDN), nil
}

func transientRefresh(reason string, cause error) error {
	return authpkg.NewRefreshError(authpkg.RefreshTransient, reason, cause)
}

func contextReason(ctx context.Context, reason string) string {
	if ctx.Err() != nil {
		return "context_done"
	}
	return reason
}

// refreshTimeout bounds each LDAP request by the configured timeout and by
// what is left of ctx.
func (p *Provider) refreshTimeout(ctx context.Context) time.Duration {
	timeout := p.cfg.Timeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = max(remaining, time.Millisecond)
		}
	}
	return timeout
}

// userBaseVisible checks that the service account can read the configured
// user base entry.
func (p *Provider) userBaseVisible(conn Conn) error {
	res, err := conn.Search(gldap.NewSearchRequest(
		p.cfg.UserBaseDN,
		gldap.ScopeBaseObject,
		gldap.NeverDerefAliases,
		1,
		timeLimitSeconds(p.cfg.Timeout),
		false,
		"(objectClass=*)",
		[]string{"1.1"},
		nil,
	))
	if err != nil {
		return fmt.Errorf("read ldap user base: %w", err)
	}
	if res == nil || len(res.Entries) != 1 {
		return errors.New("read ldap user base: no entry")
	}
	return nil
}

// admitUserByDN runs the login predicate (the user filter rendered with the
// typed login name) as a base-scope search at dn. It returns the entry when
// login would still admit it; otherwise a nil entry and why: user_not_found
// (no entry at dn) or user_not_admitted (the entry no longer matches).
func (p *Provider) admitUserByDN(conn Conn, dn, login string) (*gldap.Entry, string, error) {
	filter, err := renderUserFilter(p.cfg.UserFilter, login)
	if err != nil {
		return nil, "", err
	}
	attributes := []string{p.cfg.UsernameAttribute}
	if strings.EqualFold(p.cfg.UsernameAttribute, "dn") {
		attributes = []string{"1.1"} // RFC 4511 §4.5.1.8: no attributes
	}
	res, err := conn.Search(gldap.NewSearchRequest(
		dn,
		gldap.ScopeBaseObject,
		gldap.NeverDerefAliases,
		1,
		timeLimitSeconds(p.cfg.Timeout),
		false,
		filter,
		attributes,
		nil,
	))
	if err != nil {
		if gldap.IsErrorWithCode(err, gldap.LDAPResultNoSuchObject) {
			return nil, "user_not_found", nil
		}
		return nil, "", fmt.Errorf("look up ldap user by DN: %w", err)
	}
	if res == nil {
		return nil, "", errors.New("look up ldap user by DN: nil result")
	}
	switch len(res.Entries) {
	case 0:
		return nil, "user_not_admitted", nil
	case 1:
		return res.Entries[0], "", nil
	default:
		return nil, "", ErrAmbiguousUser
	}
}
