// Package authz carries the auth middleware's namespace decision into the
// handlers that act on it (identity-and-access design §8.1–§8.3).
//
// A collection route (a list across namespaces) is authorised by the
// middleware but filtered by its handler: the middleware stores the
// principal's allowed namespaces in the request context, and the handler
// narrows every query with NamespaceFilter (or filters in memory and calls
// MarkFiltered). Unrestricted principals get no restriction at all, so the
// handler's query is unchanged for them.
package authz

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// ContextKeyAllowedNamespaces mirrors the allowed set onto the Echo context for
// handlers that read context values directly. AllowedNamespaces (on the request
// context) is the authority.
const ContextKeyAllowedNamespaces = "auth.allowed_namespaces"

// contextKeyDecision stores the middleware's *Decision on the Echo context.
const contextKeyDecision = "auth.namespace_decision"

type scopeKey struct{}

// collectionScope is the restriction stored in a request context. It is
// shared by pointer so the handler can mark it filtered after the middleware
// stored it.
type collectionScope struct {
	allowed  []string
	filtered atomic.Bool
}

// WithAllowedNamespaces returns ctx restricted to the given namespaces. The
// stored set is sorted, de-duplicated and never nil: restricting to nothing
// is a valid restriction that matches no row.
func WithAllowedNamespaces(ctx context.Context, allowed []string) context.Context {
	set := make([]string, 0, len(allowed))
	for _, namespace := range allowed {
		if namespace = strings.TrimSpace(namespace); namespace != "" {
			set = append(set, namespace)
		}
	}
	slices.Sort(set)
	set = slices.Compact(set)
	return context.WithValue(ctx, scopeKey{}, &collectionScope{allowed: set})
}

func scopeFrom(ctx context.Context) *collectionScope {
	if ctx == nil {
		return nil
	}
	scope, _ := ctx.Value(scopeKey{}).(*collectionScope)
	return scope
}

// AllowedNamespaces reports the namespaces a collection may return.
// restricted=false means the principal is unrestricted (or auth is off): the
// handler must not filter. restricted=true means only rows in allowed may be
// returned; an empty allowed slice then means no row at all. The two cases can
// never be confused: an unrestricted result always has restricted=false.
func AllowedNamespaces(ctx context.Context) (allowed []string, restricted bool) {
	scope := scopeFrom(ctx)
	if scope == nil {
		return nil, false
	}
	return slices.Clone(scope.allowed), true
}

var columnPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)?$`)

// NamespaceFilter narrows db to rows whose column holds an allowed namespace.
// It reads the restriction from db's statement context, so db must be derived
// with WithContext(request context) — exactly how the services build their
// queries. Unrestricted: db is returned unchanged. Restricted to nothing, or
// an invalid column name: the query matches no row (fail closed). A legacy
// empty or NULL namespace counts as "default". Filtering marks the request
// filtered for the collection guard; a db without the request context finds
// no restriction and is not marked, which is how the guard catches it.
func NamespaceFilter(db *gorm.DB, column string) *gorm.DB {
	return namespaceFilter(db, column, false)
}

// SharedObjectFilter is NamespaceFilter for shared objects (notification
// channels and policies, agent profiles; design §10.4): a restricted caller
// also sees the cluster-shared rows (namespace "*"), which every principal
// with a grant may read. Unrestricted: db is returned unchanged; restricted to
// nothing still matches no row.
func SharedObjectFilter(db *gorm.DB, column string) *gorm.DB {
	return namespaceFilter(db, column, true)
}

func namespaceFilter(db *gorm.DB, column string, shared bool) *gorm.DB {
	if db == nil {
		return nil
	}
	var ctx context.Context
	if db.Statement != nil {
		ctx = db.Statement.Context
	}
	scope := scopeFrom(ctx)
	if scope == nil {
		return db
	}
	scope.filtered.Store(true)
	if !columnPattern.MatchString(column) || len(scope.allowed) == 0 {
		return db.Where("1 = 0")
	}
	allowed := scope.allowed
	if shared {
		allowed = append(slices.Clone(allowed), models.ClusterNamespace)
	}
	if slices.Contains(allowed, models.DefaultNamespace) {
		return db.Where(fmt.Sprintf("(%[1]s IN ? OR %[1]s = '' OR %[1]s IS NULL)", column), allowed)
	}
	return db.Where(fmt.Sprintf("%s IN ?", column), allowed)
}

// ParseNamespaceNarrow parses the optional ?namespace= view filter of the job,
// incident and trigger lists. narrow is false when the parameter is omitted or
// "*" (no narrowing); a present value must be a DNS label with no surrounding
// space, anything else is an error.
func ParseNamespaceNarrow(raw string) (namespace string, narrow bool, err error) {
	namespace, present, err := ParseNamespaceQuery(raw)
	if err != nil || !present || namespace == models.ClusterNamespace {
		return "", false, err
	}
	return namespace, true, nil
}

// NarrowToNamespace restricts db to rows whose column is namespace, on top of
// whatever NamespaceFilter already applied: the result is the rows in namespace
// within the caller's allowed set, so a namespace the caller cannot read yields
// no rows (never an error: no existence oracle). It never widens, and an empty
// or NULL column counts as "default", as in NamespaceFilter.
func NarrowToNamespace(db *gorm.DB, column, namespace string) *gorm.DB {
	if db == nil {
		return nil
	}
	if !columnPattern.MatchString(column) {
		return db.Where("1 = 0")
	}
	if namespace == models.DefaultNamespace {
		return db.Where(fmt.Sprintf("(%[1]s = ? OR %[1]s = '' OR %[1]s IS NULL)", column), namespace)
	}
	return db.Where(fmt.Sprintf("%s = ?", column), namespace)
}

// MarkFiltered records that the handler filtered its rows to the allowed
// namespaces itself (in memory, or through a query NamespaceFilter cannot
// express). It is a no-op for an unrestricted request.
func MarkFiltered(c *echo.Context) {
	if c == nil || c.Request() == nil {
		return
	}
	MarkFilteredContext(c.Request().Context())
}

// MarkFilteredContext is MarkFiltered for code that holds only the request
// context.
func MarkFilteredContext(ctx context.Context) {
	if scope := scopeFrom(ctx); scope != nil {
		scope.filtered.Store(true)
	}
}

// Filtered reports whether a restricted request's handler filtered its rows
// (NamespaceFilter or MarkFiltered). It is true for an unrestricted request,
// which has nothing to filter.
func Filtered(ctx context.Context) bool {
	scope := scopeFrom(ctx)
	return scope == nil || scope.filtered.Load()
}

// Decision is the namespace part of the middleware's authorisation decision,
// attached for audit (design §8.2 step 5).
type Decision struct {
	// Class is the route class.
	Class auth.RouteClass
	// Role is the role the route requires.
	Role models.Role
	// Namespaces are the namespaces the decision resolved (sorted, unique),
	// nil when none were resolved. "*" stands for the cluster-shared namespace.
	Namespaces []string
	// Unrestricted reports that the principal's cluster role satisfied the
	// route, so no namespace check was needed.
	Unrestricted bool
	// PendingItem is the route's pending plan item, if any.
	PendingItem string
}

// AuditNamespace is the value recorded in AuditLog.Namespace for the decision:
// "*" for a cluster or collection route and for a decision spanning several
// namespaces, "" for an identity route or an unresolved target, and the one
// namespace otherwise.
func (d *Decision) AuditNamespace() string {
	if d == nil {
		return ""
	}
	switch d.Class {
	case auth.RouteClassIdentity:
		return ""
	case auth.RouteClassCluster, auth.RouteClassCollection:
		return models.ClusterNamespace
	}
	switch len(d.Namespaces) {
	case 0:
		return ""
	case 1:
		return d.Namespaces[0]
	default:
		return models.ClusterNamespace
	}
}

// SetDecision attaches d to the request.
func SetDecision(c *echo.Context, d *Decision) {
	c.Set(contextKeyDecision, d)
}

// DecisionFrom returns the middleware's decision, or nil when auth is off.
func DecisionFrom(c *echo.Context) *Decision {
	if c == nil {
		return nil
	}
	d, _ := c.Get(contextKeyDecision).(*Decision)
	return d
}

// NoteNamespace lets a handler that has loaded the namespaced row it serves
// supply the namespace for the audit row, when the middleware resolved none
// (an unrestricted principal on a read route, where the middleware spends no
// lookup on audit). It never overrides a resolved decision.
func NoteNamespace(c *echo.Context, namespace string) {
	d := DecisionFrom(c)
	if d == nil || d.Class != auth.RouteClassNamespaced || d.Namespaces != nil {
		return
	}
	d.Namespaces = []string{models.NamespaceOrDefault(strings.TrimSpace(namespace))}
}

// AuditNamespace returns the decision's audit namespace for the request, or ""
// when the middleware made none.
func AuditNamespace(c *echo.Context) string {
	return DecisionFrom(c).AuditNamespace()
}

// Denial is an authorisation refusal a handler reaches after the middleware
// let the request through (for example an idempotent start that would replay
// a run in a namespace the caller cannot read). The auth middleware turns it
// into an audited 403 with Message as the body, exactly like its own denials.
type Denial struct {
	// Reason is the audit reason, e.g. "insufficient_role".
	Reason string
	// Required is the role that was missing.
	Required models.Role
	// Namespaces are the namespaces the check needed the role in.
	Namespaces []string
	// Message is the client-facing 403 message. It must not name another
	// namespace's resources.
	Message string
}

func (d *Denial) Error() string {
	return fmt.Sprintf("authz: %s: %s required in %v", d.Reason, d.Required, d.Namespaces)
}

// RequireRoleIn returns a *Denial when p is confined (its cluster role is
// below role) and lacks role in namespace. A nil principal (auth disabled) and
// an unrestricted principal always pass, so callers change nothing for them.
func RequireRoleIn(p *auth.Principal, role models.Role, namespace string) error {
	if p == nil || auth.HasRole(p.ClusterRole(), role) {
		return nil
	}
	namespace = models.NamespaceOrDefault(strings.TrimSpace(namespace))
	if auth.HasRole(p.RoleIn(namespace), role) {
		return nil
	}
	return &Denial{
		Reason:     "insufficient_role",
		Required:   role,
		Namespaces: []string{namespace},
		Message:    "insufficient permissions",
	}
}

// ParseNamespaceQuery validates a ?namespace= filter value. present=false when
// the parameter is empty (omitted). A present value must be "*" or a DNS label
// with no surrounding space; anything else is an error.
func ParseNamespaceQuery(raw string) (namespace string, present bool, err error) {
	if raw == "" {
		return "", false, nil
	}
	if raw != strings.TrimSpace(raw) {
		return "", true, fmt.Errorf("namespace %q must not carry surrounding space", raw)
	}
	namespace, err = models.NormalizeObjectNamespace(raw)
	if err != nil {
		return "", true, err
	}
	return namespace, true, nil
}
