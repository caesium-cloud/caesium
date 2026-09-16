package api

import (
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
	Clustered bool             `json:"clustered"`
	Quorum    cluster.Quorum   `json:"quorum"`
	Members   []cluster.Member `json:"members"`
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
// fields so the handler can be driven end to end in tests without a database.
type healthChecker struct {
	database   func() *CheckResult
	activeRuns func() *CheckResult
	triggers   func() *CheckResult
	workers    func() int64
	cluster    func() cluster.View
	uptime     func() time.Duration
}

func defaultHealthChecker() healthChecker {
	return healthChecker{
		database:   checkDatabase,
		activeRuns: checkActiveRuns,
		triggers:   checkTriggers,
		workers:    countWorkerNodes,
		cluster:    cluster.Snapshot,
		uptime:     func() time.Duration { return time.Since(startedAt) },
	}
}

// Health is used to determine if Caesium is healthy.
func Health(c *echo.Context) error {
	return defaultHealthChecker().handle(c)
}

func (h healthChecker) handle(c *echo.Context) error {
	resp, code := h.report()
	return c.JSON(code, resp)
}

func (h healthChecker) report() (HealthResponse, int) {
	checks := &HealthChecks{}

	// Database check — the only check that gates this node's ability to serve.
	checks.Database = h.database()

	// Active runs (informational)
	checks.ActiveRuns = h.activeRuns()

	// Trigger count (informational, does not affect overall status)
	checks.Triggers = h.triggers()

	// Cluster liveness: actually probed reachability of the raft members.
	view := h.cluster()
	checks.Cluster = clusterCheck(view)
	checks.Nodes = nodesCheck(view, h.workers)

	overall := Healthy
	if checks.Database.Status == Degraded {
		overall = Degraded
	}
	if checks.Cluster != nil {
		overall = worstStatus(overall, checks.Cluster.Status)
	}

	// The HTTP status code answers a different question from the body: it is
	// what the Kubernetes liveness and readiness probes read (see
	// helm/caesium/templates/statefulset.yaml). Only THIS node's inability to
	// serve may fail it. A cluster-wide condition — a peer down, or a lost
	// quorum — is reported in the body and must not fail every replica's probe,
	// which would restart or de-register the whole StatefulSet in response to
	// one sick member.
	code := http.StatusOK
	if checks.Database.Status == Degraded {
		code = http.StatusServiceUnavailable
	}

	return HealthResponse{
		Status: overall,
		Uptime: h.uptime(),
		Checks: checks,
	}, code
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
		Status:     clusterStatus(view.Quorum.Status),
		Clustered:  true,
		Quorum:     view.Quorum,
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
// number of raft members that actually answered, and the status mirrors quorum
// health — previously this was a bare worker count with no status at all, which
// the console rendered as unconditionally green.
func nodesCheck(view cluster.View, workers func() int64) *CheckResult {
	if !view.Clustered {
		return &CheckResult{Status: Healthy, Count: workers()}
	}

	reachable := int64(0)
	for _, m := range view.Members {
		if m.Reachability == cluster.Reachable {
			reachable++
		}
	}
	return &CheckResult{
		Status: clusterStatus(view.Quorum.Status),
		Count:  reachable,
	}
}

func checkDatabase() *CheckResult {
	conn := db.Connection()
	start := time.Now()

	var result int
	err := conn.Raw("SELECT 1").Scan(&result).Error
	latency := time.Since(start)

	if err != nil || latency > time.Second {
		return &CheckResult{Status: Degraded, LatencyMs: latency.Milliseconds()}
	}

	return &CheckResult{Status: Healthy, LatencyMs: latency.Milliseconds()}
}

func checkActiveRuns() *CheckResult {
	conn := db.Connection()
	var count int64
	conn.Model(&models.JobRun{}).Where("status = ?", "running").Count(&count)
	return &CheckResult{Status: Healthy, Count: count}
}

func checkTriggers() *CheckResult {
	conn := db.Connection()
	var count int64
	conn.Model(&models.Trigger{}).Count(&count)
	return &CheckResult{Status: Healthy, Count: count}
}

// countWorkerNodes counts the distinct nodes currently executing tasks. It is
// only used where there is no raft membership to report instead.
func countWorkerNodes() int64 {
	conn := db.Connection()
	var count int64
	conn.Model(&models.TaskRun{}).Where("status = ?", "running").Select("COUNT(DISTINCT claimed_by)").Scan(&count)
	return count
}
