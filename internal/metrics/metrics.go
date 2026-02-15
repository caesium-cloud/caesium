package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

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
)

// Register registers all custom Caesium metrics with the default Prometheus registry.
func Register() {
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
	)
}
