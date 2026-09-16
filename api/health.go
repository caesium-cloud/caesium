package api

import (
	"context"
	"net/http"
	"time"

	"github.com/caesium-cloud/caesium/internal/cluster"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/labstack/echo/v5"
)

var startedAt time.Time

func init() {
	startedAt = time.Now()
}

// databaseCheckTimeout bounds every database-backed check on the /health path.
//
// It has to exist. The dqlite driver is configured with an unlimited retry
// limit (RetryLimit 0), so leader discovery loops until its CONTEXT is done —
// and `SELECT 1` issued without one waits forever. On a real quorum loss that
// turned /health into a hang: the handler never reached the cluster assessment
// it was supposed to report, and Kubernetes probes timed out and restarted the
// surviving replicas. A check that cannot answer in time is a FAILED check, not
// a reason to stop answering.
//
// The budget sits under the Kubernetes probe timeout (default 1s, see
// helm/caesium/templates/statefulset.yaml) so the handler always responds
// before the probe gives up.
const databaseCheckTimeout = 900 * time.Millisecond

// Status enumerates the health statuses of Caesium.
type Status string

const (
	Healthy Status = "healthy"
	// Degraded means Caesium is serving but with reduced redundancy or a
	// failing dependency.
	Degraded Status = "degraded"
	// Unavailable means a dependency cannot serve at all — notably a dqlite
	// cluster that has lost quorum.
	Unavailable Status = "unavailable"
	// Unknown means the state could not be determined. It is never healthy.
	Unknown Status = "unknown"
)

// CheckResult describes the outcome of an individual health check.
type CheckResult struct {
	Status    Status `json:"status,omitempty"`
	LatencyMs int64  `json:"latency_ms,omitempty"`
	Count     int64  `json:"count,omitempty"`
}

// ClusterCheck reports raft membership liveness and quorum availability.
//
// Membership (how many nodes the cluster is configured with) and availability
// (how many of them actually answer) are reported separately: presenting the
// former as the latter is what let a crashed replica render as "quorum 3/3"
// (issue #494).
type ClusterCheck struct {
	Status Status `json:"status"`
	// Clustered is false for deployments not backed by dqlite, where there is
	// no quorum to report.
	Clustered bool           `json:"clustered"`
	Quorum    cluster.Quorum `json:"quorum"`
	// Nodes assesses EVERY member, including the standbys and spares that
	// quorum arithmetic deliberately ignores.
	Nodes   cluster.NodeSummary `json:"nodes"`
	Members []cluster.Member    `json:"members"`
	// Observed is false until the first liveness probe has completed; until
	// then member liveness is unknown, not healthy.
	Observed   bool      `json:"observed"`
	ObservedAt time.Time `json:"observed_at,omitempty"`
	Stale      bool      `json:"stale,omitempty"`
}

// HealthChecks holds all dependency check results.
type HealthChecks struct {
	Database   *CheckResult  `json:"database"`
	ActiveRuns *CheckResult  `json:"active_runs"`
	Triggers   *CheckResult  `json:"triggers"`
	Nodes      *CheckResult  `json:"nodes"`
	Cluster    *ClusterCheck `json:"cluster,omitempty"`
}

// HealthResponse defines the data the Health REST endpoint returns.
type HealthResponse struct {
	Status Status        `json:"status"`
	Uptime time.Duration `json:"uptime"`
	Checks *HealthChecks `json:"checks,omitempty"`
}

// healthChecker collects the individual probes behind /health. The funcs are
// fields so the handler can be driven end to end in tests without a database —
// including a deliberately blocking one, to prove the handler still answers.
type healthChecker struct {
	database   func(context.Context) *CheckResult
	activeRuns func(context.Context) *CheckResult
	triggers   func(context.Context) *CheckResult
	workers    func(context.Context) int64
	cluster    func() cluster.View
	uptime     func() time.Duration
	timeout    time.Duration
}

func defaultHealthChecker() healthChecker {
	return healthChecker{
		database:   checkDatabase,
		activeRuns: checkActiveRuns,
		triggers:   checkTriggers,
		workers:    countWorkerNodes,
		cluster:    cluster.Snapshot,
		uptime:     func() time.Duration { return time.Since(startedAt) },
		timeout:    databaseCheckTimeout,
	}
}

// LivenessResponse is the minimal body behind /health/live.
type LivenessResponse struct {
	Status Status        `json:"status"`
	Uptime time.Duration `json:"uptime"`
}

// activeHealthChecker builds the checker the exported handlers use. It is a var
// only so a test can drive the REAL handlers — the same routes, status-code
// logic and JSON — against a real dqlite cluster without also standing up a
// database. Production never writes it.
var activeHealthChecker = defaultHealthChecker

// Health reports full cluster and dependency health, and answers whether THIS
// node can serve. It is also what /health/ready answers.
func Health(c *echo.Context) error {
	return activeHealthChecker().handle(c)
}

// HealthReady is the Kubernetes READINESS answer: can this replica serve
// requests right now? It is the same report as /health, so its status code
// tracks this node's own serviceability — a replica that cannot reach a raft
// leader, for whatever reason, must leave the Service endpoints rather than
// keep receiving traffic it cannot answer.
func HealthReady(c *echo.Context) error {
	return activeHealthChecker().handle(c)
}

// HealthLive is the Kubernetes LIVENESS answer: is the process running and
// able to serve HTTP at all?
//
// It deliberately touches nothing else. Liveness failure RESTARTS the
// container, and restarting cures a deadlocked process — never a dependency.
// A replica whose dqlite traffic is partitioned from its peers is healthy as a
// process: it must be pulled out of the ready endpoints (readiness) but must
// not be restart-looped, which would destroy its state and cannot restore a
// raft majority.
func HealthLive(c *echo.Context) error {
	return c.JSON(http.StatusOK, LivenessResponse{
		Status: Healthy,
		Uptime: time.Since(startedAt),
	})
}

func (h healthChecker) handle(c *echo.Context) error {
	resp, code := h.report(c.Request().Context())
	return c.JSON(code, resp)
}

func (h healthChecker) report(ctx context.Context) (HealthResponse, int) {
	checks := &HealthChecks{}

	// The cluster assessment is read FIRST and comes from an in-memory
	// snapshot, so it is always reported — even when every leader-dependent
	// query below is failing because the cluster has no leader to talk to.
	view := h.cluster()
	checks.Cluster = clusterCheck(view)

	// Everything else needs the database. Bound it: these queries route through
	// dqlite and cannot complete while quorum is lost.
	dbChecks := h.runDatabaseChecks(ctx, view)
	checks.Database = dbChecks.database
	checks.ActiveRuns = dbChecks.activeRuns
	checks.Triggers = dbChecks.triggers
	checks.Nodes = nodesCheck(view, dbChecks.workers)

	overall := checks.Database.Status
	if checks.Cluster != nil {
		overall = worstStatus(overall, checks.Cluster.Status)
	}
	overall = worstStatus(overall, checks.ActiveRuns.Status)
	overall = worstStatus(overall, checks.Triggers.Status)

	// The HTTP status code answers a different question from the body. The body
	// describes the CLUSTER; the code describes THIS REPLICA's ability to serve
	// the request that just arrived, because this is what the Kubernetes
	// readiness probe reads (helm/caesium/templates/statefulset.yaml).
	//
	// So a failing database check always fails it. A locally observed quorum
	// loss is not proof of a cluster-wide outage: a replica whose dqlite
	// traffic is partitioned from its peers sees exactly the same thing while B
	// and C carry on serving, and it must leave the Service endpoints rather
	// than keep accepting traffic it cannot answer.
	//
	// Restart-looping that replica would be wrong, which is why LIVENESS points
	// at /health/live instead and never consults a dependency.
	code := http.StatusOK
	if checks.Database.Status != Healthy {
		code = http.StatusServiceUnavailable
	}

	return HealthResponse{
		Status: overall,
		Uptime: h.uptime(),
		Checks: checks,
	}, code
}

type databaseChecks struct {
	database   *CheckResult
	activeRuns *CheckResult
	triggers   *CheckResult
	workers    int64
}

// checkKind identifies a database-backed check as its result is published.
type checkKind int

const (
	kindDatabase checkKind = iota
	kindActiveRuns
	kindTriggers
	kindWorkers
)

type publishedCheck struct {
	kind   checkKind
	result *CheckResult
	count  int64
}

// runDatabaseChecks runs every database-backed check under a shared deadline
// and returns within it whatever the queries managed to do. The deadline is
// passed to the queries so the dqlite driver abandons leader discovery, and the
// select is the backstop that guarantees the handler answers even if a query
// ignores it.
//
// Each check publishes its own result the moment it finishes, rather than the
// batch publishing once at the end. Batching meant one slow INFORMATIONAL count
// discarded an already-successful `SELECT 1` and reported the database check as
// failed — a spurious 503 (and, now that readiness reads this code, a replica
// needlessly pulled out of the Service) caused by a query that proves nothing
// about serviceability. A timeout must degrade only the checks that actually
// timed out.
func (h healthChecker) runDatabaseChecks(ctx context.Context, view cluster.View) databaseChecks {
	timeout := h.timeout
	if timeout <= 0 {
		timeout = databaseCheckTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	// Buffered for every possible publication, so a check that finishes after
	// the deadline still hands off its result and its goroutine exits.
	published := make(chan publishedCheck, 4)
	expected := 3

	go func() {
		published <- publishedCheck{kind: kindDatabase, result: h.database(ctx)}
		published <- publishedCheck{kind: kindActiveRuns, result: h.activeRuns(ctx)}
		published <- publishedCheck{kind: kindTriggers, result: h.triggers(ctx)}
		if !view.Clustered {
			published <- publishedCheck{kind: kindWorkers, count: h.workers(ctx)}
		}
	}()
	if !view.Clustered {
		expected++
	}

	var checks databaseChecks
	for range expected {
		select {
		case p := <-published:
			switch p.kind {
			case kindDatabase:
				checks.database = p.result
			case kindActiveRuns:
				checks.activeRuns = p.result
			case kindTriggers:
				checks.triggers = p.result
			case kindWorkers:
				checks.workers = p.count
			}
		case <-ctx.Done():
			return withTimedOutChecks(checks, time.Since(start).Milliseconds())
		}
	}
	return checks
}

// withTimedOutChecks fills in only the checks that had not published a result
// by the deadline. Anything already answered keeps its answer.
func withTimedOutChecks(checks databaseChecks, elapsedMs int64) databaseChecks {
	if checks.database == nil {
		// Serviceability could not be established in time, which for this check
		// is a failure — not a hang, and not something to infer from a slower,
		// purely informational query.
		checks.database = &CheckResult{Status: Degraded, LatencyMs: elapsedMs}
	}
	if checks.activeRuns == nil {
		checks.activeRuns = &CheckResult{Status: Unknown}
	}
	if checks.triggers == nil {
		checks.triggers = &CheckResult{Status: Unknown}
	}
	return checks
}

// worstStatus orders the health vocabulary so the overall status can never be
// better than its worst check.
func worstStatus(a, b Status) Status {
	rank := func(s Status) int {
		switch s {
		case Healthy:
			return 0
		case Degraded:
			return 1
		case Unknown:
			return 2
		case Unavailable:
			return 3
		default:
			return 2
		}
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

func clusterCheck(view cluster.View) *ClusterCheck {
	if !view.Clustered {
		// No raft cluster backs this deployment; there is no quorum to report
		// and nothing to degrade.
		return nil
	}

	members := view.Members
	if members == nil {
		members = []cluster.Member{}
	}

	return &ClusterCheck{
		// View.Status is the worse of quorum availability and node liveness,
		// so an unreachable standby degrades cluster health even though it can
		// never affect the voter majority.
		Status:     clusterStatus(view.Status()),
		Clustered:  true,
		Quorum:     view.Quorum,
		Nodes:      view.Nodes,
		Members:    members,
		Observed:   view.Observed,
		ObservedAt: view.ObservedAt,
		Stale:      view.Stale,
	}
}

func clusterStatus(s cluster.Status) Status {
	switch s {
	case cluster.StatusAvailable:
		return Healthy
	case cluster.StatusDegraded:
		return Degraded
	case cluster.StatusUnavailable:
		return Unavailable
	default:
		return Unknown
	}
}

// nodesCheck reports node liveness. On a clustered deployment the count is the
// number of raft members that actually answered and the status covers EVERY
// member, voter or not — previously this was a bare worker count with no status
// at all, which the console rendered as unconditionally green.
func nodesCheck(view cluster.View, workers int64) *CheckResult {
	if !view.Clustered {
		return &CheckResult{Status: Healthy, Count: workers}
	}

	return &CheckResult{
		Status: clusterStatus(view.Nodes.Status),
		Count:  int64(view.Nodes.Reachable),
	}
}

func checkDatabase(ctx context.Context) *CheckResult {
	conn := db.Connection().WithContext(ctx)
	start := time.Now()

	var result int
	err := conn.Raw("SELECT 1").Scan(&result).Error
	latency := time.Since(start)

	if err != nil || latency > databaseCheckTimeout {
		return &CheckResult{Status: Degraded, LatencyMs: latency.Milliseconds()}
	}

	return &CheckResult{Status: Healthy, LatencyMs: latency.Milliseconds()}
}

func checkActiveRuns(ctx context.Context) *CheckResult {
	conn := db.Connection().WithContext(ctx)
	var count int64
	if err := conn.Model(&models.JobRun{}).Where("status = ?", "running").Count(&count).Error; err != nil {
		return &CheckResult{Status: Unknown}
	}
	return &CheckResult{Status: Healthy, Count: count}
}

func checkTriggers(ctx context.Context) *CheckResult {
	conn := db.Connection().WithContext(ctx)
	var count int64
	if err := conn.Model(&models.Trigger{}).Count(&count).Error; err != nil {
		return &CheckResult{Status: Unknown}
	}
	return &CheckResult{Status: Healthy, Count: count}
}

// countWorkerNodes counts the distinct nodes currently executing tasks. It is
// only used where there is no raft membership to report instead.
func countWorkerNodes(ctx context.Context) int64 {
	conn := db.Connection().WithContext(ctx)
	var count int64
	conn.Model(&models.TaskRun{}).Where("status = ?", "running").Select("COUNT(DISTINCT claimed_by)").Scan(&count)
	return count
}
