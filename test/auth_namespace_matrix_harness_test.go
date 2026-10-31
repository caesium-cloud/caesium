//go:build integration

package test

import (
	"bufio"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
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
	// All makes the row visible only to a caller reading every one of
	// Namespaces (a contract edge is shown whole only with both endpoints).
	All   bool
	Alias string // the row's job alias, for job-scoped keys ("" none)
	Text  string
	// Optional rows need not appear when visible (aggregates that may omit
	// them), but must never appear when not.
	Optional bool
}

func (m matrixMarker) visibleTo(p matrixPrincipal, role models.Role) bool {
	if p.JobScope != nil && m.Alias != "" && !slices.Contains(p.JobScope, m.Alias) {
		return false
	}
	seen := 0
	for _, namespace := range m.Namespaces {
		if p.sees(namespace, role) {
			seen++
		}
	}
	if m.All {
		return seen > 0 && seen == len(m.Namespaces)
	}
	return seen > 0
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
	// Late runs the probe after every non-final probe and the fixture's
	// beforeLate hook (a write that changes state other probes read).
	Late bool
	// Stream marks an SSE route: only the status is read, then the request
	// is cancelled.
	Stream bool
	// Before runs ahead of a sequential probe the oracle admits: it settles
	// state the handler's answer depends on (a namespace's run quota).
	Before func()
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
	// Check runs extra assertions on an admitted principal's body, at any
	// status (a handler refusal names itself in the body).
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
	// Resource is a successful write's resource id, where the audit row
	// carries one: a job-scoped key's job alias, or the object a controller
	// audited its write on.
	Resource string
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

	started := time.Now()
	fx := s.newNamespaceMatrixFixture(caps)
	defer fx.close()
	fixtureTime := time.Since(started)
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
	s.assertUnknownIDsAreNotServerErrors(fx, rows)

	// Coverage report: the surfaces this lane did not mount were asserted
	// unmounted, not counted.
	covered := 0
	for _, key := range matrixRouteKeys(rows) {
		if !slices.Contains(unmounted, key) {
			covered++
		}
	}
	s.T().Logf("namespace matrix: %d routes covered, %d unmounted on this lane %v, %d principals, %d requests; fixture %s, total %s",
		covered, len(unmounted), unmounted, len(callers), len(results), fixtureTime.Round(time.Millisecond), time.Since(started).Round(time.Millisecond))
}

// driveNamespaceMatrix sends every probe as every principal the probe's mode
// selects. Reads fan out over a bounded worker pool; writes run in order.
func (s *IntegrationTestSuite) driveNamespaceMatrix(fx *namespaceMatrixFixture, rows map[string]matrixRow, callers []matrixPrincipal) ([]matrixResult, []string) {
	s.T().Helper()
	ctx := s.T().Context()
	var (
		reads, writes, last, late, final []matrixResult
		unmounted                        []string
	)
	admin := fx.adminPrincipal()
	for _, key := range matrixRouteKeys(rows) {
		row := rows[key]
		probes := row.Probes(fx)
		s.Require().NotEmptyf(probes, "%s has no probe", key)
		for i := range probes {
			if probes[i].Method == "" {
				probes[i].Method, _, _ = strings.Cut(key, " ")
			}
		}
		if !fx.caps.mounts(row.Surface) {
			// Not mounted on this lane: the cluster admin is refused or 404,
			// never the handler; no principal's outcome counts. The probe is
			// the row's own, with the row's method: a write route's GET would
			// answer 405 even if the route were wrongly mounted.
			unmounted = append(unmounted, key)
			status, body, err := s.matrixDo(ctx, admin, probes[0])
			s.NoErrorf(err, "%s unmounted probe", key)
			s.Containsf([]int{http.StatusNotFound, http.StatusForbidden, http.StatusMethodNotAllowed}, status,
				"%s is on surface %q, which this lane does not mount, but answered %d: %s", key, row.Surface, status, body)
			continue
		}
		for _, probe := range probes {
			switch probe.Mode {
			case modeRead:
				for _, p := range callers {
					reads = append(reads, matrixResult{key: key, probe: probe, principal: p, want: matrixExpect(p, key, row, probe)})
				}
			case modeIdempotent:
				for _, p := range callers {
					r := matrixResult{key: key, probe: probe, principal: p, want: matrixExpect(p, key, row, probe)}
					switch {
					case probe.Final:
						final = append(final, r)
					case probe.Late:
						late = append(late, r)
					default:
						writes = append(writes, r)
					}
				}
			case modeOnce:
				admitted := false
				for _, p := range narrowestFirst(callers) {
					want := matrixExpect(p, key, row, probe)
					r := matrixResult{key: key, probe: probe, principal: p, want: want}
					switch {
					case !want.Denied && admitted:
						continue
					case !want.Denied:
						admitted = true
						last = append(last, r)
					case probe.Late:
						late = append(late, r)
					default:
						writes = append(writes, r)
					}
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
	s.assertMatrixStats(fx, rows, callers)
	s.assertMatrixEventStream(fx, callers)
	run := func(batch []matrixResult) {
		for i := range batch {
			r := &batch[i]
			if r.probe.Before != nil && !r.want.Denied {
				r.probe.Before()
			}
			r.status, r.body, r.err = s.matrixDo(ctx, r.principal, r.probe)
		}
	}
	run(writes)
	run(last)
	// Late probes rewrite what earlier probes read (job definition applies
	// replace the declaring jobs' dataset declarations; cache deletes remove
	// the entries a replay reuses), so they run after all of them, once the
	// runs those probes started finish and the fixture's seeded co-producer
	// state is withdrawn.
	fx.beforeLate()
	run(late)
	run(final)
	results := slices.Concat(reads, writes, last, late, final)
	return results, unmounted
}

// assertUnknownIDsAreNotServerErrors sends every mounted route that takes an
// id, with every id unknown, as the cluster admin. That caller skips the
// namespace lookup whose miss is a confined caller's 404, so the handler itself
// answers, and a missing row must never be a 5xx. The ids match nothing, so no
// route acts on anything; the body and headers only get a write past its
// request validation to its lookup.
func (s *IntegrationTestSuite) assertUnknownIDsAreNotServerErrors(fx *namespaceMatrixFixture, rows map[string]matrixRow) {
	s.T().Helper()
	admin := fx.adminPrincipal()
	missing := uuid.NewString()
	for _, key := range matrixRouteKeys(rows) {
		method, route, _ := strings.Cut(key, " ")
		if !strings.Contains(route, ":id") || !fx.caps.mounts(rows[key].Surface) {
			continue
		}
		p := matrixProbe{
			Method:  method,
			Path:    strings.ReplaceAll(strings.ReplaceAll(route, ":id", missing), "*", "history"),
			Headers: map[string]string{"Idempotency-Key": "mx-" + missing},
		}
		if method != http.MethodGet {
			p.Body = `{"confirmed":true,"revision":1,"input":{}}`
		}
		status, body, err := s.matrixDo(s.T().Context(), admin, p)
		if s.NoErrorf(err, "%s with unknown ids", key) {
			s.Lessf(status, http.StatusInternalServerError, "%s with unknown ids: %s %s\nbody: %s", key, p.Method, p.Path, truncate(body, 300))
		}
	}
}

// matrixStats is every number GET /v1/stats and GET /v1/stats/summary report,
// by its JSON name.
type matrixStats struct {
	Jobs struct {
		Total              int64   `json:"total"`
		RecentRuns         int64   `json:"recent_runs"`
		SuccessRate        float64 `json:"success_rate"`
		AvgDurationSeconds float64 `json:"avg_duration_seconds"`
	} `json:"jobs"`
	TopFailing []struct {
		JobID        string `json:"job_id"`
		FailureCount int64  `json:"failure_count"`
	} `json:"top_failing"`
	TopFailingAtoms []struct {
		JobID        string `json:"job_id"`
		AtomName     string `json:"atom_name"`
		FailureCount int64  `json:"failure_count"`
	} `json:"top_failing_atoms"`
	SlowestJobs []struct {
		JobID              string  `json:"job_id"`
		AvgDurationSeconds float64 `json:"avg_duration_seconds"`
	} `json:"slowest_jobs"`
	SuccessRateTrend []struct {
		Date        string  `json:"date"`
		RunCount    int64   `json:"run_count"`
		SuccessRate float64 `json:"success_rate"`
	} `json:"success_rate_trend"`
}

// matrixStatsRun is one job_runs row inside the seven-day stats window.
type matrixStatsRun struct {
	Namespace, JobID, Status, Day string
	Recent                        bool            // started in the last 24 hours
	Seconds                       sql.NullFloat64 // a completed run's duration
}

// matrixStatsAtom is the failed task runs of one task, per run namespace.
type matrixStatsAtom struct {
	Namespace, JobID, Name string
	Failures               int64
}

// matrixStatsCatalog is the rows the stats are computed from, read directly,
// so nothing in it has passed through the server's namespace filter.
type matrixStatsCatalog struct {
	Jobs  map[string]int64 // live jobs per namespace
	Runs  []matrixStatsRun
	Atoms []matrixStatsAtom
}

func (fx *namespaceMatrixFixture) statsCatalog() matrixStatsCatalog {
	s := fx.s
	s.T().Helper()
	ctx := s.T().Context()
	now := time.Now().UTC()
	day, week := now.Add(-24*time.Hour), now.Add(-7*24*time.Hour)
	catalog := matrixStatsCatalog{Jobs: map[string]int64{}}
	scan := func(row func(*sql.Rows) error, query string, args ...any) {
		rows, err := fx.f.catalogDB.QueryContext(ctx, query, args...)
		s.Require().NoError(err, query)
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			s.Require().NoError(row(rows), query)
		}
		s.Require().NoError(rows.Err(), query)
	}
	scan(func(rows *sql.Rows) error {
		var namespace string
		var total int64
		err := rows.Scan(&namespace, &total)
		catalog.Jobs[namespace] += total
		return err
	}, `SELECT COALESCE(namespace, ''), COUNT(*) FROM jobs WHERE deleted_at IS NULL GROUP BY 1`)
	scan(func(rows *sql.Rows) error {
		var run matrixStatsRun
		err := rows.Scan(&run.Namespace, &run.JobID, &run.Status, &run.Day, &run.Recent, &run.Seconds)
		catalog.Runs = append(catalog.Runs, run)
		return err
	}, `SELECT COALESCE(namespace, ''), job_id, status, strftime('%Y-%m-%d', started_at, 'utc'), started_at >= ?,
		CASE WHEN completed_at IS NOT NULL THEN (JULIANDAY(completed_at) - JULIANDAY(started_at)) * 86400 END
		FROM job_runs WHERE started_at >= ? AND quarantine IS NOT TRUE`, day, week)
	scan(func(rows *sql.Rows) error {
		var atom matrixStatsAtom
		err := rows.Scan(&atom.Namespace, &atom.JobID, &atom.Name, &atom.Failures)
		catalog.Atoms = append(catalog.Atoms, atom)
		return err
	}, `SELECT COALESCE(job_runs.namespace, ''), job_runs.job_id, tasks.name, COUNT(*)
		FROM task_runs JOIN job_runs ON job_runs.id = task_runs.job_run_id JOIN tasks ON tasks.id = task_runs.task_id
		WHERE task_runs.status = 'failed' AND job_runs.started_at >= ? AND task_runs.quarantine IS NOT TRUE AND job_runs.quarantine IS NOT TRUE
		GROUP BY 1, 2, 3`, week)
	return catalog
}

// matrixStatsWant is what a caller must be told: the catalog folded over the
// namespaces it reads. The top lists are kept whole, per job, because a list
// is cut at five and ties have no fixed order.
type matrixStatsWant struct {
	Total, RecentRuns int64
	SuccessRate       float64
	AvgDuration       float64
	Failures          map[string]int64    // failed runs per job
	AtomFailures      map[string]int64    // failed task runs per job and task
	JobDurations      map[string]float64  // mean run duration per job
	Trend             map[string][2]int64 // runs and successes per day
}

func (c matrixStatsCatalog) want(p matrixPrincipal) matrixStatsWant {
	w := matrixStatsWant{
		Failures:     map[string]int64{},
		AtomFailures: map[string]int64{},
		JobDurations: map[string]float64{},
		Trend:        map[string][2]int64{},
	}
	for namespace, total := range c.Jobs {
		if p.sees(namespace, rV) {
			w.Total += total
		}
	}
	var finished, succeeded, timed int64
	var seconds float64
	jobTimed := map[string]int64{}
	for _, run := range c.Runs {
		if !p.sees(run.Namespace, rV) {
			continue
		}
		if run.Recent {
			w.RecentRuns++
		}
		if run.Seconds.Valid {
			timed++
			seconds += run.Seconds.Float64
			jobTimed[run.JobID]++
			w.JobDurations[run.JobID] += run.Seconds.Float64
		}
		if run.Status != "succeeded" && run.Status != "failed" {
			continue
		}
		finished++
		point := w.Trend[run.Day]
		point[0]++
		if run.Status == "succeeded" {
			succeeded++
			point[1]++
		} else {
			w.Failures[run.JobID]++
		}
		w.Trend[run.Day] = point
	}
	if finished > 0 {
		w.SuccessRate = float64(succeeded) / float64(finished)
	}
	if timed > 0 {
		w.AvgDuration = seconds / float64(timed)
	}
	for job, runs := range jobTimed {
		w.JobDurations[job] /= float64(runs)
	}
	for _, atom := range c.Atoms {
		if p.sees(atom.Namespace, rV) {
			w.AtomFailures[atom.JobID+"/"+atom.Name] += atom.Failures
		}
	}
	return w
}

// diff lists every number of got that differs from w.
func (w matrixStatsWant) diff(got matrixStats) []string {
	var out []string
	count := func(name string, have, want int64) {
		if have != want {
			out = append(out, fmt.Sprintf("%s: got %d, want %d", name, have, want))
		}
	}
	float := func(name string, have, want float64) {
		if math.Abs(have-want) > 1e-6 {
			out = append(out, fmt.Sprintf("%s: got %g, want %g", name, have, want))
		}
	}
	count("jobs.total", got.Jobs.Total, w.Total)
	count("jobs.recent_runs", got.Jobs.RecentRuns, w.RecentRuns)
	float("jobs.success_rate", got.Jobs.SuccessRate, w.SuccessRate)
	float("jobs.avg_duration_seconds", got.Jobs.AvgDurationSeconds, w.AvgDuration)
	count("len(top_failing)", int64(len(got.TopFailing)), int64(min(5, len(w.Failures))))
	for _, entry := range got.TopFailing {
		count("top_failing "+entry.JobID, entry.FailureCount, w.Failures[entry.JobID])
	}
	count("len(top_failing_atoms)", int64(len(got.TopFailingAtoms)), int64(min(5, len(w.AtomFailures))))
	for _, entry := range got.TopFailingAtoms {
		count("top_failing_atoms "+entry.JobID+"/"+entry.AtomName, entry.FailureCount, w.AtomFailures[entry.JobID+"/"+entry.AtomName])
	}
	count("len(slowest_jobs)", int64(len(got.SlowestJobs)), int64(min(5, len(w.JobDurations))))
	for _, entry := range got.SlowestJobs {
		float("slowest_jobs "+entry.JobID, entry.AvgDurationSeconds, w.JobDurations[entry.JobID])
	}
	for _, point := range got.SuccessRateTrend {
		want := w.Trend[point.Date]
		count("success_rate_trend "+point.Date+" run_count", point.RunCount, want[0])
		rate := 0.0
		if want[0] > 0 {
			rate = float64(want[1]) / float64(want[0])
		}
		float("success_rate_trend "+point.Date+" success_rate", point.SuccessRate, rate)
	}
	return out
}

// assertMatrixStats requires every number of both stats routes, as read by
// each admitted principal, to equal the catalog's rows folded over exactly the
// namespaces the principal's grants read (design §8.3). The alias markers on
// the rows cannot do this: a foreign run moves a count or a rate without
// naming its job.
//
// The comparison is a pass of its own, between the concurrent reads and the
// writes, because it reads the catalog alongside the response: the suite's
// scenarios run one at a time, but a cron job an earlier scenario left behind
// can still finish a run between the two reads. So a mismatch is read again.
// That cannot hide a leak, which stays in every read while foreign rows exist,
// and the fixture guarantees they do: a confined caller's job and recent-run
// counts must be positive and below the unrestricted ones.
func (s *IntegrationTestSuite) assertMatrixStats(fx *namespaceMatrixFixture, rows map[string]matrixRow, callers []matrixPrincipal) {
	s.T().Helper()
	const attempts = 5
	admin := fx.adminPrincipal()
	catalog := fx.statsCatalog()
	for _, key := range []string{"GET /v1/stats", "GET /v1/stats/summary"} {
		probe := rows[key].Probes(fx)[0]
		probe.Method, _, _ = strings.Cut(key, " ")
		for _, p := range callers {
			if matrixExpect(p, key, rows[key], probe).Denied {
				continue
			}
			var diffs []string
			for attempt := range attempts {
				if attempt > 0 {
					time.Sleep(250 * time.Millisecond)
					catalog = fx.statsCatalog()
				}
				status, body, err := s.matrixDo(s.T().Context(), p, probe)
				s.Require().NoErrorf(err, "%s as %s", key, p.Name)
				s.Require().Equalf(http.StatusOK, status, "%s as %s: %s", key, p.Name, truncate(body, 300))
				var got matrixStats
				s.Require().NoError(json.Unmarshal([]byte(body), &got), body)
				if diffs = catalog.want(p).diff(got); len(diffs) == 0 {
					break
				}
			}
			s.Emptyf(diffs, "%s as %s: numbers that differ from the catalog over the namespaces it reads", key, p.Name)
			if p.clusterRole() == "" {
				own, all := catalog.want(p), catalog.want(admin)
				s.Positivef(own.RecentRuns, "%s: the fixture leaves %s no recent run", key, p.Name)
				s.Lessf(own.RecentRuns, all.RecentRuns, "%s: no foreign run exists for %s, so a leak would not show", key, p.Name)
				s.Lessf(own.Total, all.Total, "%s: no foreign job exists for %s, so a leak would not show", key, p.Name)
			}
		}
	}
}

// matrixEventReader collects what one open GET /v1/events stream delivers.
type matrixEventReader struct {
	mu     sync.Mutex
	events []event.Event
}

func (r *matrixEventReader) read(body io.Reader) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 1<<20), 4<<20)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		var evt event.Event
		if json.Unmarshal([]byte(data), &evt) == nil {
			r.mu.Lock()
			r.events = append(r.events, evt)
			r.mu.Unlock()
		}
	}
}

func (r *matrixEventReader) snapshot() []event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

// assertMatrixEventStream reads GET /v1/events with no run_id, which the
// probes stop at the headers of, as a caller confined to marketing and one
// confined to finance while both namespaces run. The runs go one after
// another, so a reader that has seen its own run complete has already been
// past the foreign run before it: a leak could not still be on its way.
func (s *IntegrationTestSuite) assertMatrixEventStream(fx *namespaceMatrixFixture, callers []matrixPrincipal) {
	s.T().Helper()
	confined := func(own, foreign string) *matrixPrincipal {
		for i := range callers {
			p := &callers[i]
			if p.Kind != matrixKindAgent && p.JobScope == nil && p.sees(own, models.RoleViewer) && !p.sees(foreign, models.RoleViewer) {
				return p
			}
		}
		s.Require().Failf("no confined caller", "the matrix needs a caller reading %s and not %s", own, foreign)
		return nil
	}
	mktViewer, finViewer := confined(nsMarketing, nsFinance), confined(nsFinance, nsMarketing)
	mkt, fin := fx.n(nsMarketing), fx.n(nsFinance)

	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, cancel := context.WithCancel(s.T().Context())
	defer cancel()
	open := func(p *matrixPrincipal) *matrixEventReader {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.caesiumURL+"/v1/events", nil)
		s.Require().NoError(err)
		// The handler subscribes before it flushes the headers, so the stream
		// misses nothing started after this returns.
		resp, err := p.Do(req)
		s.Require().NoError(err, p.Name)
		s.Require().Equal(http.StatusOK, resp.StatusCode, p.Name)
		r := &matrixEventReader{}
		wg.Go(func() {
			defer func() { _ = resp.Body.Close() }()
			r.read(resp.Body)
		})
		return r
	}
	mktReader, finReader := open(mktViewer), open(finViewer)

	run := func(n *matrixNamespace) string {
		id := s.triggerRun(n.Job.ID)
		s.Equal("succeeded", s.awaitRun(n.Job.ID, id, runTimeout).Status, n.Name)
		return id
	}
	finFirst := run(fin)
	mktRun := run(mkt)
	finLast := run(fin)

	seen := func(r *matrixEventReader, p *matrixPrincipal, own string) []event.Event {
		s.Require().Eventuallyf(func() bool { return streamHasRun(r.snapshot(), own, event.TypeRunCompleted) },
			20*time.Second, 100*time.Millisecond, "%s never saw its own run %s complete", p.Name, own)
		return r.snapshot()
	}
	foreignFree := func(events []event.Event, p *matrixPrincipal, foreignJob *jobSummary, foreignRun string) {
		s.Falsef(streamHasRun(events, foreignRun, ""), "%s received an event of foreign run %s", p.Name, foreignRun)
		for _, evt := range events {
			s.NotEqualf(foreignJob.ID, evt.JobID.String(), "%s received a %s event of a foreign job", p.Name, evt.Type)
		}
	}
	foreignFree(seen(mktReader, mktViewer, mktRun), mktViewer, fin.Job, finFirst)
	finEvents := seen(finReader, finViewer, finLast)
	s.Truef(streamHasRun(finEvents, finFirst, ""), "%s missed its own earlier run", finViewer.Name)
	foreignFree(finEvents, finViewer, mkt.Job, mktRun)
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
		if r.want.Denied || r.probe.Stream {
			continue
		}
		role := r.probe.MarkerRole
		if role == "" {
			role = models.RoleViewer
		}
		for _, marker := range r.probe.Markers {
			switch {
			case r.status >= 300:
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

// matrixControllerWrite is the row a handler, not the middleware, audits a
// successful write with. Resource is the id that row must carry.
type matrixControllerWrite struct {
	Action   string
	Resource func(matrixResult) string
}

// matrixControllerWrites are the writes on the matrix's routes their
// controllers audit themselves (api/rest/controller/auth key_*.go, policy.go
// and sso.go, api/rest/controller/notification). The rest of those routes'
// probes stop before the audit: an unknown user or connection is a 404.
var matrixControllerWrites = map[string]matrixControllerWrite{
	"POST /v1/auth/keys":                    {auth.ActionKeyCreate, resourceInBody},
	"POST /v1/auth/keys/:id/revoke":         {auth.ActionKeyRevoke, resourceInPath},
	"POST /v1/auth/keys/:id/rotate":         {auth.ActionKeyRotate, resourceInPath}, // the old key, not its replacement
	"POST /v1/notifications/channels":       {auth.ActionNotificationChannelCreate, resourceInBody},
	"PATCH /v1/notifications/channels/:id":  {auth.ActionNotificationChannelUpdate, resourceInPath},
	"DELETE /v1/notifications/channels/:id": {auth.ActionNotificationChannelDelete, resourceInPath},
	"POST /v1/notifications/policies":       {auth.ActionNotificationPolicyCreate, resourceInBody},
	"PATCH /v1/notifications/policies/:id":  {auth.ActionNotificationPolicyUpdate, resourceInPath},
	"DELETE /v1/notifications/policies/:id": {auth.ActionNotificationPolicyDelete, resourceInPath},
	"POST /v1/auth/policy/reload":           {auth.ActionPolicyReloaded, resourceInBody},
	// An API key's logout is an audited no-op. Its row names the caller's own
	// key or session, an id the matrix does not hold: auditRowTuple drops it.
	"POST /auth/logout": {auth.ActionAuthLogout, func(matrixResult) string { return "" }},
}

// resourceInPath is the id the request's path carries in the route's ":id".
func resourceInPath(r matrixResult) string {
	_, route, _ := strings.Cut(r.key, " ")
	return strings.Split(r.probe.Path, "/")[slices.Index(strings.Split(route, "/"), ":id")]
}

// resourceInBody is the id the response returned: a created object's, a
// created key's, or the policy revision a reload served.
func resourceInBody(r matrixResult) string {
	var out struct {
		ID       string `json:"id"`
		Revision string `json:"revision"`
		APIKey   struct {
			ID string `json:"id"`
		} `json:"api_key"`
	}
	_ = json.Unmarshal([]byte(r.body), &out)
	return cmp.Or(out.ID, out.APIKey.ID, out.Revision)
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
	} else if row.Action != auth.ActionAuthLogout {
		t.Resource = row.ResourceID
	}
	return t
}

// assertMatrixAudit requires, per audited principal, that its new auth.denied
// rows are exactly the denials the oracle predicted (the multiset of
// actor/action/policy_key/namespace), and that every admitted, audited write
// left its success row with the decision's namespace, whether the middleware
// wrote it (matrixWriteActions) or a controller did (matrixControllerWrites,
// which also fixes the row's resource id). Each audit row id is counted once;
// rows that existed before the matrix ran are ignored.
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
			wantDenied[actor][matrixAuditTuple{Actor: actor, Action: auth.ActionAuthDenied, PolicyKey: r.key, Namespace: r.want.AuditNS}]++
			continue
		}
		if r.status < 200 || r.status >= 300 {
			continue
		}
		tuple := matrixAuditTuple{Actor: actor, Namespace: classAuditNamespace(rows[r.key].Class, r.probe.Needs)}
		if action, ok := matrixWriteActions[r.key]; ok {
			tuple.Action = action
			if r.principal.JobScope != nil && len(r.probe.Aliases) > 0 {
				tuple.Resource = r.probe.Aliases[0]
			}
		} else if write, ok := matrixControllerWrites[r.key]; ok {
			tuple.Action, tuple.Resource = write.Action, write.Resource(r)
		} else {
			continue
		}
		if wantWrites[actor] == nil {
			wantWrites[actor] = map[matrixAuditTuple]int{}
		}
		wantWrites[actor][tuple]++
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
		var missing, extra []string
		for t, n := range denied {
			if got := gotDenied[t]; got < n {
				missing = append(missing, fmt.Sprintf("%s ns=%q ×%d", t.PolicyKey, t.Namespace, n-got))
			}
		}
		for t, n := range gotDenied {
			if want := denied[t]; n > want {
				extra = append(extra, fmt.Sprintf("%s ns=%q ×%d", t.PolicyKey, t.Namespace, n-want))
			}
		}
		sort.Strings(missing)
		sort.Strings(extra)
		s.Emptyf(missing, "%s: denials the matrix made that left no auth.denied row with that policy_key and namespace", p.Name)
		s.Emptyf(extra, "%s: auth.denied rows no matrix denial accounts for", p.Name)
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
