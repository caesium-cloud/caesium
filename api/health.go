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

// Health is used to determine if Caesium is healthy.
func Health(c *echo.Context) error {
	return defaultHealthChecker().handle(c)
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

	// The HTTP status code answers a different question from the body: it is
	// what the Kubernetes liveness and readiness probes read (see
	// helm/caesium/templates/statefulset.yaml). Only THIS node's inability to
	// serve may fail it.
	//
	// A cluster-wide condition must not: when quorum is lost, every replica's
	// database check fails at once, and failing every probe would restart or
	// de-register the whole StatefulSet — which cannot restore a raft majority,
	// and would destroy the one surface still able to report why. So a
	// database failure is attributed to the cluster when the snapshot has
	// positively observed a lost quorum, and to this node otherwise (including
	// when cluster liveness is merely unknown, which keeps the pre-existing
	// startup behaviour intact).
	code := http.StatusOK
	if checks.Database.Status != Healthy && !quorumLost(view) {
		code = http.StatusServiceUnavailable
	}

	return HealthResponse{
		Status: overall,
		Uptime: h.uptime(),
		Checks: checks,
	}, code
}

// quorumLost reports a POSITIVELY OBSERVED loss of quorum. An unknown or
// not-yet-observed cluster is not a lost one.
func quorumLost(view cluster.View) bool {
	return view.Clustered && view.Observed && view.Quorum.Status == cluster.StatusUnavailable
}

type databaseChecks struct {
	database   *CheckResult
	activeRuns *CheckResult
	triggers   *CheckResult
	workers    int64
}

// runDatabaseChecks runs every database-backed check under a shared deadline
// and returns within it whatever the queries do. The deadline is passed to the
// queries so the dqlite driver abandons leader discovery, and the select is the
// backstop that guarantees the handler answers even if a query ignores it.
func (h healthChecker) runDatabaseChecks(ctx context.Context, view cluster.View) databaseChecks {
	timeout := h.timeout
	if timeout <= 0 {
		timeout = databaseCheckTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	done := make(chan databaseChecks, 1) // buffered: the goroutine never blocks
	go func() {
		result := databaseChecks{
			database:   h.database(ctx),
			activeRuns: h.activeRuns(ctx),
			triggers:   h.triggers(ctx),
		}
		if !view.Clustered {
			result.workers = h.workers(ctx)
		}
		done <- result
	}()

	select {
	case result := <-done:
		return result
	case <-ctx.Done():
		elapsed := time.Since(start).Milliseconds()
		return databaseChecks{
			// A check that could not answer in time has failed. Whether that
			// failure is this node's fault or the cluster's is decided by the
			// HTTP status code above, not here.
			database: &CheckResult{Status: Degraded, LatencyMs: elapsed},
			// The informational counts are simply not known.
			activeRuns: &CheckResult{Status: Unknown},
			triggers:   &CheckResult{Status: Unknown},
		}
	}
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
