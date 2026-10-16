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

// RefreshGroups re-reads a user's groups with the service account. The grant
// is the user DN; the entry is looked up by DN (a base-scope search) and the
// group search uses the entry's CURRENT username attribute, read the same way
// login reads it, never a stored one, so a rename or a reassigned username
// cannot pull in someone else's groups.
//
// Classification (see authpkg.RefreshErrorKind):
//   - definitive (DenialOfSubject): the entry no longer exists (noSuchObject,
//     or a base-scope search that returns nothing) while the configured user
//     base is still visible;
//   - transient: everything else. An LDAP refresh only reads: it redeems and
//     rotates nothing, so no failure can leave the grant in an uncertain
//     state. That covers a dial, bind or search failure, the context ending
//     (the connection is closed to unblock a pending bind or search, and an
//     answer read after that is never treated as definitive), a grant for
//     another provider, and an entry without the username attribute the
//     group filter needs.
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
	entry, err := p.lookupUserByDN(conn, dn)
	if err != nil {
		return authpkg.GroupRefresh{}, transientRefresh(contextReason(ctx, "user_lookup_failed"), err)
	}
	if entry == nil {
		if ctx.Err() != nil {
			return authpkg.GroupRefresh{}, transientRefresh("context_done", ctx.Err())
		}
		// A replica mid-resync, or a service account that lost read access,
		// answers noSuchObject for every user. Only a directory that still
		// shows the user base may say this one user is gone; otherwise the
		// whole LDAP population would be revoked at once.
		if err := p.userBaseVisible(conn); err != nil {
			log.Warn("ldap user base is not visible to the service account; not treating the user as gone",
				"user_id", req.UserID.String(), "user_base_dn", p.cfg.UserBaseDN)
			return authpkg.GroupRefresh{}, transientRefresh(contextReason(ctx, "user_base_not_visible"), err)
		}
		return authpkg.GroupRefresh{}, authpkg.NewRefreshDenial(authpkg.DenialOfSubject, "user_not_found")
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

// lookupUserByDN reads the user entry by DN with a base-scope search. A nil
// entry with a nil error means the entry does not exist.
func (p *Provider) lookupUserByDN(conn Conn, dn string) (*gldap.Entry, error) {
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
		"(objectClass=*)",
		attributes,
		nil,
	))
	if err != nil {
		if gldap.IsErrorWithCode(err, gldap.LDAPResultNoSuchObject) {
			return nil, nil
		}
		return nil, fmt.Errorf("look up ldap user by DN: %w", err)
	}
	if res == nil {
		return nil, errors.New("look up ldap user by DN: nil result")
	}
	switch len(res.Entries) {
	case 0:
		return nil, nil
	case 1:
		return res.Entries[0], nil
	default:
		return nil, ErrAmbiguousUser
	}
}
