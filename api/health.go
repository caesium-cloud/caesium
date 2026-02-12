package api

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/labstack/echo/v4"
)

var startedAt time.Time

func init() {
	startedAt = time.Now()
}

// Status enumerates the health statuses of Caesium.
type Status string

const (
	Healthy  Status = "healthy"
	Degraded Status = "degraded"
)

// CheckResult describes the outcome of an individual health check.
type CheckResult struct {
	Status    Status `json:"status,omitempty"`
	LatencyMs int64  `json:"latency_ms,omitempty"`
	Count     int64  `json:"count,omitempty"`
}

// HealthChecks holds all dependency check results.
type HealthChecks struct {
	Database   *CheckResult `json:"database"`
	ActiveRuns *CheckResult `json:"active_runs"`
	Triggers   *CheckResult `json:"triggers"`
}

// HealthResponse defines the data the Health REST endpoint returns.
type HealthResponse struct {
	Status Status        `json:"status"`
	Uptime time.Duration `json:"uptime"`
	Checks *HealthChecks `json:"checks,omitempty"`
}

// Health is used to determine if Caesium is healthy.
func Health(c echo.Context) error {
	overall := Healthy
	checks := &HealthChecks{}

	// Database check
	checks.Database = checkDatabase()
	if checks.Database.Status == Degraded {
		overall = Degraded
	}

	// Active runs (informational)
	checks.ActiveRuns = checkActiveRuns()

	// Trigger count
	checks.Triggers = checkTriggers()
	if checks.Triggers.Count == 0 {
		if overall == Healthy {
			// No triggers is a warning but we keep status as healthy
			// unless there's a harder failure.
		}
	}

	code := http.StatusOK
	if overall == Degraded {
		code = http.StatusServiceUnavailable
	}

	return c.JSON(code, HealthResponse{
		Status: overall,
		Uptime: time.Since(startedAt),
		Checks: checks,
	})
}

func checkDatabase() *CheckResult {
	conn := db.Connection()
	start := time.Now()

	sqlDB, err := conn.DB()
	if err != nil {
		return &CheckResult{Status: Degraded, LatencyMs: time.Since(start).Milliseconds()}
	}

	err = sqlDB.QueryRow("SELECT 1").Scan(new(sql.RawBytes))
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
	return &CheckResult{Count: count}
}

func checkTriggers() *CheckResult {
	conn := db.Connection()
	var count int64
	conn.Model(&models.Trigger{}).Count(&count)
	return &CheckResult{Count: count}
}
