//go:build integration

package test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
)

// The namespace allow/deny matrix (identity-and-access A7, design §8 and §17).
//
// The harness is lane-agnostic: it builds one fixture of real objects in the
// default, marketing and finance namespaces, then drives every route of
// internal/auth/rbac.go's table, through a concrete probe per route, as each
// principal a lane supplies, and compares every response, collection body and
// audit row with an oracle that models the auth middleware (namespace
// decision, then authorizeScope) independently of it. The api-key lane runs it
// with namespaced keys and agent-session tokens
// (TestAuthNamespaceAllowDenyMatrix); the Keycloak lane runs it with SSO
// session principals.

// matrixKind is how a matrix principal authenticates.
type matrixKind string

const (
	matrixKindAPIKey  matrixKind = "api_key"
	matrixKindSession matrixKind = "session"
	matrixKindAgent   matrixKind = "agent"
)

// matrixPrincipal is one caller the matrix drives every route as.
type matrixPrincipal struct {
	// Name labels the principal in failures.
	Name string
	// Kind says how it authenticates. CLI-login confirmation routes refuse
	// every bearer credential in the handler (api/rest/controller/auth/cli.go
	// cliBrowserSession), so their outcome depends on it.
	Kind matrixKind
	// Grants is the principal's role per namespace; "*" is cluster-wide.
	Grants map[string]models.Role
	// JobScope is a job-scoped API key's exact aliases (nil: unscoped).
	JobScope []string
	// AgentIncident is the incident an agent-session token is bound to.
	AgentIncident string
	// AuditActor is the actor its audit rows carry (an API key's prefix, a
	// user's email). Empty skips its audit assertions.
	AuditActor string
	// Do sends req with the principal's credential (a cookie session also
	// adds its CSRF header on writes). It must not follow redirects.
	Do func(*http.Request) (*http.Response, error)
}

// matrixLaneCaps declares which optional route surfaces the lane's server
// mounts. A row on an absent surface is not coverage: the harness asserts the
// route is unmounted instead.
type matrixLaneCaps struct {
	SSO             bool // /auth/whoami, /auth/logout, /auth/cli/* (an SSO provider configured)
	UserAdmin       bool // /v1/auth/users*, /v1/auth/sessions (SSO session store)
	Connectors      bool // CAESIUM_CONNECTORS_ENABLED
	Contracts       bool // CAESIUM_CONTRACT_ENFORCEMENT set
	DataAssertions  bool // CAESIUM_DATA_ASSERTIONS_ENABLED
	LogConsole      bool // CAESIUM_LOG_CONSOLE_ENABLED (default on)
	DatabaseConsole bool // CAESIUM_DATABASE_CONSOLE_ENABLED
	Incidents       bool // CAESIUM_AGENT_REMEDIATION_ENABLED
}

// matrixSurface names the optional surface a route belongs to.
type matrixSurface string

const (
	surfaceCore            matrixSurface = ""
	surfaceSSO             matrixSurface = "sso"
	surfaceUserAdmin       matrixSurface = "user-admin"
	surfaceConnectors      matrixSurface = "connectors"
	surfaceContracts       matrixSurface = "contracts"
	surfaceDataAssertions  matrixSurface = "data-assertions"
	surfaceLogConsole      matrixSurface = "log-console"
	surfaceDatabaseConsole matrixSurface = "database-console"
	surfaceIncidents       matrixSurface = "incidents"
)

func (c matrixLaneCaps) mounts(surface matrixSurface) bool {
	switch surface {
	case surfaceCore:
		return true
	case surfaceSSO:
		return c.SSO
	case surfaceUserAdmin:
		return c.UserAdmin
	case surfaceConnectors:
		return c.Connectors
	case surfaceContracts:
		return c.Contracts
	case surfaceDataAssertions:
		return c.DataAssertions
	case surfaceLogConsole:
		return c.LogConsole
	case surfaceDatabaseConsole:
		return c.DatabaseConsole
	case surfaceIncidents:
		return c.Incidents
	}
	return false
}

// matrixNeed mirrors one namespace requirement a resolver in
// api/middleware/auth_namespace_resolvers.go returns: the role in every
// namespace, in any one (Any), or in any grant at all (AnyGrant; Namespaces
// then only feed the audit row).
type matrixNeed struct {
	Namespaces []string
	Role       models.Role
	Any        bool
	AnyGrant   bool
}

func needEvery(role models.Role, namespaces ...string) matrixNeed {
	return matrixNeed{Namespaces: namespaces, Role: role}
}

func needAny(role models.Role, namespaces ...string) matrixNeed {
	return matrixNeed{Namespaces: namespaces, Role: role, Any: true}
}

func needAnyGrant(role models.Role, auditNamespaces ...string) matrixNeed {
	return matrixNeed{Namespaces: auditNamespaces, Role: role, AnyGrant: true}
}

// matrixMode says how often a probe runs.
type matrixMode int

const (
	// modeRead runs for every principal, concurrently.
	modeRead matrixMode = iota
	// modeIdempotent runs for every principal, sequentially: a write whose
	// admitted effect is the same however often it is repeated.
	modeIdempotent
	// modeOnce runs for every principal the oracle denies, then once for the
	// first admitted principal (the narrowest grant first): a write that
	// consumes its target.
	modeOnce
)

// matrixMarker is text that appears in a collection body exactly when the
// caller may read the row it belongs to.
type matrixMarker struct {
	// Namespaces own the row: it is visible to a caller reading any of them
	// ("*" rows are shared; none: the row is never visible in this body).
	Namespaces []string
	Alias      string // the row's job alias, for job-scoped keys ("" none)
	Text       string
	// Optional rows need not appear when visible (aggregates that may omit
	// them), but must never appear when not.
	Optional bool
}

func (m matrixMarker) visibleTo(p matrixPrincipal, role models.Role) bool {
	if p.JobScope != nil && m.Alias != "" && !slices.Contains(p.JobScope, m.Alias) {
		return false
	}
	for _, namespace := range m.Namespaces {
		if p.sees(namespace, role) {
			return true
		}
	}
	return false
}

// matrixProbe is one concrete request for a route.
type matrixProbe struct {
	Label   string
	Method  string
	Path    string
	Body    string
	Headers map[string]string
	Mode    matrixMode
	// Final runs the probe after every other one (a logout ends a cookie
	// session).
	Final bool
	// Stream marks an SSE route: only the status is read, then the request
	// is cancelled.
	Stream bool
	// Needs are the namespaced route's requirements for this target.
	Needs []matrixNeed
	// Admit is the handler's status for an admitted principal.
	Admit int
	// AdmitFor overrides Admit per principal (nil: Admit).
	AdmitFor func(matrixPrincipal) int
	// HandlerDeny models a namespace refusal the handler itself returns as an
	// audited *authz.Denial after the middleware admitted the caller: it
	// returns true when the principal is refused.
	HandlerDeny func(matrixPrincipal) bool
	// Aliases are the job aliases the request names (job-scoped keys).
	Aliases []string
	// Prune marks a jobdefs apply with prune (job-scoped keys are refused).
	Prune bool
	// Incident is the incident id an agent route names.
	Incident string
	// Markers are checked on every admitted principal's 2xx body.
	Markers []matrixMarker
	// MarkerRole is the role a marker's namespace must grant (default viewer).
	MarkerRole models.Role
	// Check runs extra assertions on an admitted principal's 2xx body.
	Check func(p matrixPrincipal, body string)
}

// matrixRow is the matrix entry of one route. Class and Role restate the
// design's classification and are required to equal rbac.go's, so a
// reclassification fails here until the row is changed on purpose.
type matrixRow struct {
	Class   auth.RouteClass
	Role    models.Role
	Surface matrixSurface
	Probes  func(fx *namespaceMatrixFixture) []matrixProbe
}

// matrixOutcome is the oracle's verdict for one principal and probe.
type matrixOutcome struct {
	Status  int
	Denied  bool   // an audited auth.denied row is expected
	AuditNS string // the denial row's namespace
	// Unrestricted reports that the principal's cluster role satisfied the
	// route (the decision resolved no namespace).
	Unrestricted bool
}

func roleLevel(role models.Role) int { return models.RoleLevel(role) }

func (p matrixPrincipal) clusterRole() models.Role { return p.Grants[models.ClusterNamespace] }

func (p matrixPrincipal) roleIn(namespace string) models.Role {
	namespace = models.NamespaceOrDefault(namespace)
	best := p.clusterRole()
	if role := p.Grants[namespace]; roleLevel(role) > roleLevel(best) {
		best = role
	}
	return best
}

func (p matrixPrincipal) highestRole() models.Role {
	var best models.Role
	for _, role := range p.Grants {
		if roleLevel(role) > roleLevel(best) {
			best = role
		}
	}
	return best
}

// sees reports whether p may read rows of namespace at role ("*" rows are
// shared: readable by every admitted caller).
func (p matrixPrincipal) sees(namespace string, role models.Role) bool {
	if namespace == models.ClusterNamespace {
		return true
	}
	return roleLevel(p.roleIn(namespace)) >= roleLevel(role)
}

func needsSatisfied(p matrixPrincipal, needs []matrixNeed) bool {
	if len(needs) == 0 {
		return false
	}
	for _, need := range needs {
		if need.AnyGrant {
			if roleLevel(p.highestRole()) < roleLevel(need.Role) {
				return false
			}
			continue
		}
		if len(need.Namespaces) == 0 {
			return false
		}
		matched := 0
		for _, namespace := range need.Namespaces {
			if roleLevel(p.roleIn(namespace)) >= roleLevel(need.Role) {
				matched++
			}
		}
		if need.Any && matched == 0 || !need.Any && matched != len(need.Namespaces) {
			return false
		}
	}
	return true
}

// needsAuditNamespace is the namespace a namespaced decision audits: the one
// resolved namespace, "*" for several, "" for none (internal/authz Decision
// AuditNamespace over requirementNamespaces).
func needsAuditNamespace(needs []matrixNeed) string {
	var namespaces []string
	for _, need := range needs {
		for _, namespace := range need.Namespaces {
			namespaces = append(namespaces, models.NamespaceOrDefault(namespace))
		}
	}
	slices.Sort(namespaces)
	namespaces = slices.Compact(namespaces)
	switch len(namespaces) {
	case 0:
		return ""
	case 1:
		return namespaces[0]
	}
	return models.ClusterNamespace
}

func classAuditNamespace(class auth.RouteClass, needs []matrixNeed) string {
	switch class {
	case auth.RouteClassIdentity:
		return ""
	case auth.RouteClassCluster, auth.RouteClassCollection:
		return models.ClusterNamespace
	}
	return needsAuditNamespace(needs)
}

// matrixExpect is the oracle: the auth middleware's namespace decision
// (api/middleware/auth_namespace.go authorizeRoute), then its scope check
// (api/middleware/auth_scope.go authorizeScope), then the handler.
func matrixExpect(p matrixPrincipal, key string, row matrixRow, probe matrixProbe) matrixOutcome {
	deny := matrixOutcome{Status: http.StatusForbidden, Denied: true, AuditNS: classAuditNamespace(row.Class, probe.Needs)}
	unrestricted := false
	switch {
	case row.Class == auth.RouteClassIdentity:
	case roleLevel(p.clusterRole()) >= roleLevel(row.Role):
		unrestricted = true
	case roleLevel(p.highestRole()) < roleLevel(row.Role):
		return deny
	case row.Class == auth.RouteClassCollection:
	case row.Class == auth.RouteClassNamespaced:
		if !needsSatisfied(p, probe.Needs) {
			return deny
		}
	default:
		return deny
	}
	if !scopeAdmits(p, key, probe) {
		return deny
	}
	if probe.HandlerDeny != nil && probe.HandlerDeny(p) {
		return deny
	}
	status := probe.Admit
	if probe.AdmitFor != nil {
		status = probe.AdmitFor(p)
	}
	return matrixOutcome{Status: status, Unrestricted: unrestricted}
}

// scopeAdmits models authorizeScope for agent-session tokens and job-scoped
// keys; every other principal passes it.
func scopeAdmits(p matrixPrincipal, key string, probe matrixProbe) bool {
	method, route, _ := strings.Cut(key, " ")
	if p.Kind == matrixKindAgent {
		return strings.HasPrefix(route, "/v1/agent/") && probe.Incident != "" && probe.Incident == p.AgentIncident
	}
	if p.JobScope == nil {
		return true
	}
	inScope := func(aliases []string) bool {
		if len(aliases) == 0 {
			return false
		}
		for _, alias := range aliases {
			if !slices.Contains(p.JobScope, alias) {
				return false
			}
		}
		return true
	}
	switch {
	case strings.HasPrefix(route, "/v1/connectors"):
		return false
	case route == "/auth/whoami":
		return true
	case route == "/v1/jobs" && method == http.MethodGet:
		return true
	case route == "/v1/jobs" && method == http.MethodPost:
		return len(probe.Aliases) == 1 && inScope(probe.Aliases)
	case route == "/v1/jobdefs/apply":
		return !probe.Prune && inScope(probe.Aliases)
	case route == "/v1/triggers/:id/fire" && method == http.MethodPost:
		return inScope(probe.Aliases)
	case route == "/v1/events" && method == http.MethodPost:
		return true
	case route == "/v1/events" && method == http.MethodGet:
		return inScope(probe.Aliases)
	case strings.HasPrefix(route, "/v1/jobs/:id"):
		return inScope(probe.Aliases)
	}
	return false
}

// matrixAuditTuple is one expected audit row.
type matrixAuditTuple struct {
	Actor, Action, PolicyKey, Namespace string
}

// matrixResult is one observed response.
type matrixResult struct {
	key       string
	probe     matrixProbe
	principal matrixPrincipal
	want      matrixOutcome
	status    int
	body      string
	err       error
}

// do sends one probe as p and returns the status and (for non-stream probes)
// the body.
func (s *IntegrationTestSuite) matrixDo(ctx context.Context, p matrixPrincipal, probe matrixProbe) (int, string, error) {
	timeout := 30 * time.Second
	if probe.Stream {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var body io.Reader
	if probe.Body != "" {
		body = strings.NewReader(probe.Body)
	}
	req, err := http.NewRequestWithContext(ctx, probe.Method, s.caesiumURL+probe.Path, body)
	if err != nil {
		return 0, "", err
	}
	if probe.Body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range probe.Headers {
		req.Header.Set(k, v)
	}
	resp, err := p.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if probe.Stream && resp.StatusCode == http.StatusOK {
		// Header-only: an admitted stream stays open, so never drain it.
		cancel()
		return resp.StatusCode, "", nil
	}
	out, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out), err
}

// bearerPrincipal is an API-key or agent-session principal.
func bearerPrincipal(name string, kind matrixKind, token, prefix string, grants map[string]models.Role) matrixPrincipal {
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return matrixPrincipal{
		Name:       name,
		Kind:       kind,
		Grants:     grants,
		AuditActor: prefix,
		Do: func(req *http.Request) (*http.Response, error) {
			req.Header.Set("Authorization", "Bearer "+token)
			return client.Do(req)
		},
	}
}

// matrixRouteKeys returns the sorted keys of m.
func matrixRouteKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// requireMatrixCoversRouteTable fails unless the matrix has exactly one row
// per rbac.go route, with the same class and role: a new route cannot ship
// unmatrixed, and a stale row cannot outlive its route.
func (s *IntegrationTestSuite) requireMatrixCoversRouteTable(rows map[string]matrixRow) {
	s.T().Helper()
	table := auth.RoutePolicies()
	var missing, stale, drift []string
	for _, key := range matrixRouteKeys(table) {
		row, ok := rows[key]
		if !ok {
			missing = append(missing, key)
			continue
		}
		policy := table[key]
		if row.Class != policy.Class || row.Role != policy.Role {
			drift = append(drift, fmt.Sprintf("%s: matrix %s/%s, rbac.go %s/%s", key, row.Class, row.Role, policy.Class, policy.Role))
		}
		if policy.Pending() {
			drift = append(drift, fmt.Sprintf("%s: still pending on %s", key, policy.PendingItem))
		}
	}
	for _, key := range matrixRouteKeys(rows) {
		if _, ok := table[key]; !ok {
			stale = append(stale, key)
		}
	}
	s.Require().Empty(missing, "routes in internal/auth/rbac.go with no namespace matrix row (add one to namespaceMatrixRows)")
	s.Require().Empty(stale, "namespace matrix rows for routes rbac.go no longer has")
	s.Require().Empty(drift, "namespace matrix rows whose class/role differ from rbac.go")
}

// runNamespaceMatrix builds the fixture, asks principals for the lane's
// callers, drives every route row as each of them, and asserts statuses,
// collection bodies and audit rows. It is the one entry point every lane
// shares; the fixture is cleaned up when it returns.
func (s *IntegrationTestSuite) runNamespaceMatrix(caps matrixLaneCaps, principals func(fx *namespaceMatrixFixture) []matrixPrincipal) {
	s.T().Helper()
	rows := namespaceMatrixRows()
	s.requireMatrixCoversRouteTable(rows)

	fx := s.newNamespaceMatrixFixture(caps)
	defer fx.close()
	callers := principals(fx)
	s.Require().NotEmpty(callers)
	names := map[string]bool{}
	for _, p := range callers {
		s.Require().False(names[p.Name], "duplicate matrix principal %s", p.Name)
		names[p.Name] = true
		s.Require().NotNil(p.Do, p.Name)
	}

	before := s.matrixAuditSnapshot(callers)
	results, unmounted := s.driveNamespaceMatrix(fx, rows, callers)
	s.assertMatrixResults(results)
	s.assertMatrixAudit(rows, callers, results, before)

	// Coverage report: the surfaces this lane did not mount were asserted
	// unmounted, not counted.
	covered := 0
	for _, key := range matrixRouteKeys(rows) {
		if !slices.Contains(unmounted, key) {
			covered++
		}
	}
	s.T().Logf("namespace matrix: %d routes covered, %d unmounted on this lane %v, %d principals, %d requests",
		covered, len(unmounted), unmounted, len(callers), len(results))
}

// driveNamespaceMatrix sends every probe as every principal the probe's mode
// selects. Reads fan out over a bounded worker pool; writes run in order.
func (s *IntegrationTestSuite) driveNamespaceMatrix(fx *namespaceMatrixFixture, rows map[string]matrixRow, callers []matrixPrincipal) ([]matrixResult, []string) {
	s.T().Helper()
	ctx := s.T().Context()
	var (
		reads, writes, last, final []matrixResult
		unmounted                  []string
	)
	admin := fx.adminPrincipal()
	for _, key := range matrixRouteKeys(rows) {
		row := rows[key]
		probes := row.Probes(fx)
		s.Require().NotEmptyf(probes, "%s has no probe", key)
		if !fx.caps.mounts(row.Surface) {
			// Not mounted on this lane: the cluster admin is refused or 404,
			// never the handler; no principal's outcome counts.
			unmounted = append(unmounted, key)
			status, body, err := s.matrixDo(ctx, admin, probes[0])
			s.NoErrorf(err, "%s unmounted probe", key)
			s.Containsf([]int{http.StatusNotFound, http.StatusForbidden, http.StatusMethodNotAllowed}, status,
				"%s is on surface %q, which this lane does not mount, but answered %d: %s", key, row.Surface, status, body)
			continue
		}
		for _, probe := range probes {
			if probe.Method == "" {
				probe.Method, _, _ = strings.Cut(key, " ")
			}
			switch probe.Mode {
			case modeRead:
				for _, p := range callers {
					reads = append(reads, matrixResult{key: key, probe: probe, principal: p, want: matrixExpect(p, key, row, probe)})
				}
			case modeIdempotent:
				for _, p := range callers {
					if probe.Final {
						final = append(final, matrixResult{key: key, probe: probe, principal: p, want: matrixExpect(p, key, row, probe)})
						continue
					}
					writes = append(writes, matrixResult{key: key, probe: probe, principal: p, want: matrixExpect(p, key, row, probe)})
				}
			case modeOnce:
				admitted := false
				for _, p := range narrowestFirst(callers) {
					want := matrixExpect(p, key, row, probe)
					if !want.Denied {
						if admitted {
							continue
						}
						admitted = true
						last = append(last, matrixResult{key: key, probe: probe, principal: p, want: want})
						continue
					}
					writes = append(writes, matrixResult{key: key, probe: probe, principal: p, want: want})
				}
			}
		}
	}

	var wg sync.WaitGroup
	jobs := make(chan int)
	for range 8 {
		wg.Go(func() {
			for i := range jobs {
				r := &reads[i]
				r.status, r.body, r.err = s.matrixDo(ctx, r.principal, r.probe)
			}
		})
	}
	for i := range reads {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	for _, batch := range [][]matrixResult{writes, last, final} {
		for i := range batch {
			r := &batch[i]
			r.status, r.body, r.err = s.matrixDo(ctx, r.principal, r.probe)
		}
	}
	results := slices.Concat(reads, writes, last, final)
	return results, unmounted
}

// narrowestFirst orders principals so the first admitted one holds the
// fewest grants: an admitted write by a namespace-confined caller proves the
// namespace grant suffices.
func narrowestFirst(callers []matrixPrincipal) []matrixPrincipal {
	ordered := slices.Clone(callers)
	weight := func(p matrixPrincipal) int {
		w := 0
		if _, ok := p.Grants[models.ClusterNamespace]; ok {
			w += 100
		}
		w += 10 * len(p.Grants)
		for _, role := range p.Grants {
			w += roleLevel(role)
		}
		if p.Kind == matrixKindAgent || p.JobScope != nil {
			w += 1000 // scoped callers are probed for denial, never chosen to admit
		}
		return w
	}
	slices.SortStableFunc(ordered, func(a, b matrixPrincipal) int { return weight(a) - weight(b) })
	return ordered
}

func (s *IntegrationTestSuite) assertMatrixResults(results []matrixResult) {
	s.T().Helper()
	for _, r := range results {
		where := fmt.Sprintf("%s [%s] as %s", r.key, r.probe.Label, r.principal.Name)
		if !s.NoErrorf(r.err, "%s", where) {
			continue
		}
		if !s.Equalf(r.want.Status, r.status, "%s: %s %s\nbody: %s", where, r.probe.Method, r.probe.Path, truncate(r.body, 600)) {
			continue
		}
		if r.want.Denied || r.status >= 300 || r.probe.Stream {
			continue
		}
		role := r.probe.MarkerRole
		if role == "" {
			role = models.RoleViewer
		}
		for _, marker := range r.probe.Markers {
			switch {
			case !marker.visibleTo(r.principal, role):
				s.NotContainsf(r.body, marker.Text, "%s must not see the %v row %q", where, marker.Namespaces, marker.Text)
			case !marker.Optional:
				s.Containsf(r.body, marker.Text, "%s must list its %v row %q", where, marker.Namespaces, marker.Text)
			}
		}
		if r.probe.Check != nil {
			r.probe.Check(r.principal, r.body)
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// matrixWriteActions are the actions the auth middleware audits on a
// successful write (api/middleware/auth.go auditActionForRoute; GET routes
// excluded: an unrestricted read resolves no namespace for its row).
var matrixWriteActions = map[string]string{
	"POST /v1/jobs":                         auth.ActionJobCreate,
	"DELETE /v1/jobs/:id":                   auth.ActionJobDelete,
	"PUT /v1/jobs/:id/pause":                auth.ActionJobPause,
	"PUT /v1/jobs/:id/unpause":              auth.ActionJobUnpause,
	"POST /v1/jobs/:id/run":                 auth.ActionRunTrigger,
	"DELETE /v1/jobs/:id/queue/:id":         auth.ActionRunQueueCancel,
	"POST /v1/jobs/:id/runs/:id/retry":      auth.ActionRunRetry,
	"POST /v1/jobs/:id/backfill":            auth.ActionBackfill,
	"PUT /v1/jobs/:id/backfills/:id/cancel": auth.ActionBackfill,
	"POST /v1/jobdefs/apply":                auth.ActionJobdefApply,
	"POST /v1/cache/prune":                  auth.ActionCachePrune,
	"DELETE /v1/jobs/:id/cache":             auth.ActionCacheDelete,
	"DELETE /v1/jobs/:id/cache/:id":         auth.ActionCacheDelete,
	"DELETE /v1/system/nodes/:id":           auth.ActionClusterMemberRemove,
}

// matrixAuditSnapshot records the audit row ids each audited principal
// already has, so pre-existing rows are never matched.
func (s *IntegrationTestSuite) matrixAuditSnapshot(callers []matrixPrincipal) map[string]map[string]bool {
	s.T().Helper()
	seen := map[string]map[string]bool{}
	for _, p := range callers {
		if p.AuditActor == "" || seen[p.AuditActor] != nil {
			continue
		}
		rows, err := s.auditCLI("--actor", p.AuditActor)
		s.Require().NoError(err)
		ids := map[string]bool{}
		for _, row := range rows {
			ids[row.ID] = true
		}
		seen[p.AuditActor] = ids
	}
	return seen
}

func auditRowTuple(row auditRow) matrixAuditTuple {
	t := matrixAuditTuple{Actor: row.Actor, Action: row.Action, Namespace: row.Namespace}
	if row.Action == auth.ActionAuthDenied {
		var meta struct {
			PolicyKey string `json:"policy_key"`
		}
		_ = json.Unmarshal(row.Metadata, &meta)
		t.PolicyKey = meta.PolicyKey
	}
	return t
}

// assertMatrixAudit requires, per audited principal, that its new auth.denied
// rows are exactly the denials the oracle predicted (the multiset of
// actor/action/policy_key/namespace), and that every admitted, audited write
// left its success row with the decision's namespace. Each audit row id is
// counted once; rows that existed before the matrix ran are ignored.
func (s *IntegrationTestSuite) assertMatrixAudit(rows map[string]matrixRow, callers []matrixPrincipal, results []matrixResult, before map[string]map[string]bool) {
	s.T().Helper()
	wantDenied := map[string]map[matrixAuditTuple]int{}
	wantWrites := map[string]map[matrixAuditTuple]int{}
	for _, r := range results {
		actor := r.principal.AuditActor
		if actor == "" || r.err != nil || r.status != r.want.Status {
			continue // a status mismatch is already reported
		}
		if r.want.Denied {
			if wantDenied[actor] == nil {
				wantDenied[actor] = map[matrixAuditTuple]int{}
			}
			wantDenied[actor][matrixAuditTuple{actor, auth.ActionAuthDenied, r.key, r.want.AuditNS}]++
			continue
		}
		action, ok := matrixWriteActions[r.key]
		if !ok || r.status < 200 || r.status >= 300 {
			continue
		}
		if wantWrites[actor] == nil {
			wantWrites[actor] = map[matrixAuditTuple]int{}
		}
		wantWrites[actor][matrixAuditTuple{actor, action, "", classAuditNamespace(rows[r.key].Class, r.probe.Needs)}]++
	}

	done := map[string]bool{}
	for _, p := range callers {
		actor := p.AuditActor
		if actor == "" || done[actor] {
			continue
		}
		done[actor] = true
		denied, writes := wantDenied[actor], wantWrites[actor]
		wantRows := 0
		for _, n := range denied {
			wantRows += n
		}
		var gotDenied, gotWrites map[matrixAuditTuple]int
		// Audit rows are written after the response: wait for the count.
		deadline := time.Now().Add(30 * time.Second)
		for {
			auditRows, err := s.auditCLI("--actor", actor)
			s.Require().NoError(err)
			gotDenied, gotWrites = map[matrixAuditTuple]int{}, map[matrixAuditTuple]int{}
			ids := map[string]bool{}
			total := 0
			for _, row := range auditRows {
				if before[actor][row.ID] || ids[row.ID] {
					continue
				}
				ids[row.ID] = true
				t := auditRowTuple(row)
				switch {
				case row.Action == auth.ActionAuthDenied:
					gotDenied[t]++
					total++
				case row.Outcome == auth.OutcomeSuccess:
					gotWrites[t]++
				}
			}
			if total >= wantRows && writesCovered(writes, gotWrites) || time.Now().After(deadline) {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		s.Equalf(nonEmpty(denied), nonEmpty(gotDenied), "%s: its auth.denied rows (actor/action/policy_key/namespace) must be exactly the matrix denials", p.Name)
		for t, n := range writes {
			s.GreaterOrEqualf(gotWrites[t], n, "%s: audited write %+v", p.Name, t)
		}
	}
}

func writesCovered(want, got map[matrixAuditTuple]int) bool {
	for t, n := range want {
		if got[t] < n {
			return false
		}
	}
	return true
}

func nonEmpty(m map[matrixAuditTuple]int) map[matrixAuditTuple]int {
	if len(m) == 0 {
		return nil
	}
	return m
}
