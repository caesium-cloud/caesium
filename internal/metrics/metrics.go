package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var registerOnce sync.Once

var (
	JobRunsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_job_runs_total",
			Help: "Total number of job runs by status.",
		},
		[]string{"job_id", "status"},
	)

	JobRunDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "caesium_job_run_duration_seconds",
			Help:    "Duration of job runs in seconds.",
			Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600},
		},
		[]string{"job_id", "status"},
	)

	TaskRunsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_task_runs_total",
			Help: "Total number of task runs by status.",
		},
		[]string{"job_id", "task_id", "engine", "status"},
	)

	TaskRunDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "caesium_task_run_duration_seconds",
			Help:    "Duration of task runs in seconds.",
			Buckets: []float64{0.5, 1, 5, 15, 30, 60, 120, 300, 600},
		},
		[]string{"job_id", "engine", "status"},
	)

	JobsActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "caesium_jobs_active",
			Help: "Number of currently running jobs.",
		},
		[]string{"job_id"},
	)

	CallbackRunsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_callback_runs_total",
			Help: "Total number of callback executions by status.",
		},
		[]string{"job_id", "status"},
	)

	TriggerFiresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_trigger_fires_total",
			Help: "Total number of trigger fires by type.",
		},
		[]string{"job_id", "trigger_type"},
	)

	WorkerClaimsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_worker_claims_total",
			Help: "Total number of tasks successfully claimed by worker node.",
		},
		[]string{"node_id"},
	)

	WorkerClaimContentionTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_worker_claim_contention_total",
			Help: "Total number of worker claim contention events.",
		},
		[]string{"node_id"},
	)

	WorkerLeaseExpirationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_worker_lease_expirations_total",
			Help: "Total number of expired worker task leases reclaimed by node.",
		},
		[]string{"node_id"},
	)

	TaskRetriesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_task_retries_total",
			Help: "Total number of task retry attempts.",
		},
		[]string{"job_alias", "task_name", "attempt"},
	)

	BackfillRunsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_backfill_runs_total",
			Help: "Total number of runs spawned by backfills by status.",
		},
		[]string{"job_alias", "status"},
	)

	BackfillsActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "caesium_backfills_active",
			Help: "Number of currently running backfills.",
		},
		[]string{"job_alias"},
	)

	TaskCacheHitsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_task_cache_hits_total",
			Help: "Total number of task cache hits.",
		},
		[]string{"job_alias", "task_name"},
	)

	TaskCacheMissesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_task_cache_misses_total",
			Help: "Total number of task cache misses.",
		},
		[]string{"job_alias", "task_name"},
	)

	TaskCacheEntries = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "caesium_task_cache_entries",
			Help: "Current number of task cache entries.",
		},
	)

	// Auth metrics

	AuthRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_auth_requests_total",
			Help: "Total authenticated API requests by outcome, role, method, and path.",
		},
		[]string{"outcome", "role", "method", "path"},
	)

	AuthFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_auth_failures_total",
			Help: "Total authentication failures by reason.",
		},
		[]string{"reason"},
	)

	AuthKeyAgeSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "caesium_auth_key_age_seconds",
			Help:    "Age of API keys at request time in seconds.",
			Buckets: []float64{3600, 86400, 604800, 2592000, 7776000, 15552000, 31536000},
		},
	)

	AuditLogEntriesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_audit_log_entries_total",
			Help: "Total audit log entries by action and outcome.",
		},
		[]string{"action", "outcome"},
	)

	WebhookAuthFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_webhook_auth_failures_total",
			Help: "Total webhook authentication failures by trigger path and reason.",
		},
		[]string{"trigger", "reason"},
	)
)

// Register registers all custom Caesium metrics with the default Prometheus registry.
func Register() {
	registerOnce.Do(func() {
		prometheus.MustRegister(
			JobRunsTotal,
			JobRunDurationSeconds,
			TaskRunsTotal,
			TaskRunDurationSeconds,
			JobsActive,
			CallbackRunsTotal,
			TriggerFiresTotal,
			WorkerClaimsTotal,
			WorkerClaimContentionTotal,
			WorkerLeaseExpirationsTotal,
			TaskRetriesTotal,
			BackfillRunsTotal,
			BackfillsActive,
			TaskCacheHitsTotal,
			TaskCacheMissesTotal,
			TaskCacheEntries,
			AuthRequestsTotal,
			AuthFailuresTotal,
			AuthKeyAgeSeconds,
			AuditLogEntriesTotal,
			WebhookAuthFailuresTotal,
		)
	})
}
