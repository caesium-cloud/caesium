package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var registerOnce sync.Once

// DBWriteCategory constants for use with DBWritesTotal.
const (
	DBWriteCategoryTaskRunInsert = "task_run_insert"
	DBWriteCategoryTaskRunStatus = "task_run_status"
	DBWriteCategoryEventInsert   = "event_insert"
	DBWriteCategoryLeaseRenewal  = "lease_renewal"
	DBWriteCategoryCallback      = "callback"
	DBWriteCategoryCommand       = "command"
	DBWriteCategoryCheckpoint    = "checkpoint"
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

	TaskRegisterBatchSize = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "caesium_task_register_batch_size",
			Help:    "Number of task registration inputs per RegisterTasks call.",
			Buckets: []float64{0, 1, 2, 5, 10, 25, 50, 100, 250, 500, 1000},
		},
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

	TaskPriorityClaimTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_task_priority_claim_total",
			Help: "Total number of tasks successfully claimed by priority.",
		},
		[]string{"priority"},
	)

	WorkerClaimContentionTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_worker_claim_contention_total",
			Help: "Total number of worker claim contention events.",
		},
		[]string{"node_id"},
	)

	DBBusyRetriesTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "caesium_db_busy_retries_total",
			Help: "Total number of database busy/locked retry attempts.",
		},
	)

	ReclaimDurationSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "caesium_reclaim_duration_seconds",
			Help:    "Duration of expired task lease reclaim attempts in seconds.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		},
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

	// TaskCacheShortCircuitsTotal counts value-verified short-circuits (D2): a
	// task that re-executed because its own identity changed but produced
	// byte-identical output to a prior successful run, so it presented that
	// prior identity to downstream consumers and spared them a re-run.
	TaskCacheShortCircuitsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_task_cache_short_circuits_total",
			Help: "Total number of value-verified short-circuits (unchanged output despite a changed identity hash).",
		},
		[]string{"job_alias", "task_name"},
	)

	TaskCacheEntries = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "caesium_task_cache_entries",
			Help: "Current number of task cache entries.",
		},
	)

	// The resource label is bounded by declared metadata.rateLimits resources,
	// not per-request free-form input.
	RateLimitAcquiredTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_rate_limit_acquired_total",
			Help: "Total rate-limit token acquisitions by declared resource.",
		},
		[]string{"resource"},
	)

	// The resource label is bounded by declared metadata.rateLimits resources,
	// not per-request free-form input.
	RateLimitRejectedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_rate_limit_rejected_total",
			Help: "Total rate-limit token rejections by declared resource.",
		},
		[]string{"resource"},
	)

	RunSkippedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_run_skipped_total",
			Help: "Total runs or run tasks skipped by scheduler control, by job and reason.",
		},
		[]string{"job_alias", "reason"},
	)

	RunReplacedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_run_replaced_total",
			Help: "Total running job runs cancelled and replaced by the run concurrency policy.",
		},
		[]string{"job_alias"},
	)

	RunQueueDepth = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "caesium_run_queue_depth",
			Help: "Current number of queued runs by job and priority.",
		},
		[]string{"job_alias", "priority"},
	)

	RunQueueWaitSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "caesium_run_queue_wait_seconds",
			Help:    "Time a queued run waited before being launched.",
			Buckets: []float64{0.5, 1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600},
		},
		[]string{"job_alias"},
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

	SSOLoginsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_sso_logins_total",
			Help: "Total SSO login attempts by provider and outcome.",
		},
		[]string{"provider", "outcome"},
	)

	SSOLoginDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "caesium_sso_login_duration_seconds",
			Help:    "Duration of shared SSO login completion attempts in seconds.",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		},
		[]string{"provider"},
	)

	SSOLogoutsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_sso_logouts_total",
			Help: "Total SSO logout attempts by outcome.",
		},
		[]string{"outcome"},
	)

	WebhookAuthFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_webhook_auth_failures_total",
			Help: "Total webhook authentication failures by trigger path and reason.",
		},
		[]string{"path", "reason"},
	)

	EventTriggerMatchesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_event_trigger_matches_total",
			Help: "Total event trigger matches by configured trigger.",
		},
		[]string{"trigger_id"},
	)

	EventsIngestedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_events_ingested_total",
			Help: "Total events ingested by low-cardinality origin.",
		},
		[]string{"origin"},
	)

	EventBridgeFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_event_bridge_failures_total",
			Help: "Total event bridge routing failures by low-cardinality origin.",
		},
		[]string{"origin"},
	)

	EventBusDroppedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_event_bus_dropped_total",
			Help: "Total events dropped because an event bus subscriber buffer was full.",
		},
		[]string{"event_type"},
	)

	TriggerChainDepth = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "caesium_trigger_chain_depth",
			Help:    "Observed trigger chain depth for lifecycle-event-triggered job runs.",
			Buckets: []float64{1, 2, 3, 4, 5, 10, 20, 50},
		},
	)

	TriggerChainRejectedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "caesium_trigger_chain_rejected_total",
			Help: "Total event trigger fires rejected because trigger chain depth exceeded the configured maximum.",
		},
	)

	// DBWritesTotal counts durable database writes by category, measured in
	// rows touched (so a batched UPDATE/INSERT increments by the row count, not
	// by one). Use this for capacity-planning and "how much work is the DB
	// doing." For "how many round-trips" use DBStatementsTotal below.
	//
	// Categories:
	//   task_run_insert   – new task_runs row (RegisterTasks)
	//   task_run_status   – status UPDATE on task_runs (claim, start, complete, fail, skip, retry)
	//   event_insert      – new execution_events row
	//   lease_renewal     – UPDATE task_runs SET claim_expires_at (worker heartbeat)
	//   callback          – INSERT or UPDATE on callback_runs
	//   command           – reserved for future run_commands table writes
	//   checkpoint        – reserved for future run_checkpoints table writes
	DBWritesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_db_writes_total",
			Help: "Total durable database writes by category, measured in rows touched.",
		},
		[]string{"category"},
	)

	// DBStatementsTotal counts distinct SQL statements executed by category —
	// each batched INSERT or UPDATE increments by 1 regardless of row count.
	// Pair with DBWritesTotal: rows/statements ratio quantifies batching
	// effectiveness. Phase 1.1 (event coalescing) and 1.4 (predecessor-counter
	// UPDATE batching) reduce statement count without changing row count.
	DBStatementsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_db_statements_total",
			Help: "Total distinct SQL statements executed by category. A batched UPDATE/INSERT increments by 1 regardless of row count; pair with caesium_db_writes_total to compute rows/statement (batching factor).",
		},
		[]string{"category"},
	)

	// CompleteRejectedTotal counts completions rejected by /internal/complete
	// due to fence violations.  The reason label distinguishes the specific
	// rejection cause so operators can diagnose stale owners, partition events,
	// or misconfigured workers.
	//
	// reason values:
	//   stale_generation  – owner_generation in the envelope != current lease generation
	//   wrong_worker      – worker_node != the node that received the original dispatch
	//   invalid_status    – status outside {succeeded, failed, cached} (esp. skipped)
	//   task_not_running  – task_runs.status != running at the time of the complete
	//   not_owner         – this node does not currently own the run
	//   missing_run       – run_id not found
	//   malformed         – JSON decode or validation failure
	CompleteRejectedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_complete_rejected_total",
			Help: "Total /internal/complete requests rejected by the run-owner fence, by reason.",
		},
		[]string{"reason"},
	)

	// RunLeaseRenewalsTotal counts batched run-lease renewal UPDATE statements.
	RunLeaseRenewalsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "caesium_run_lease_renewals_total",
			Help: "Total batched run-lease renewal UPDATE statements issued by this node.",
		},
	)

	// RunLeasesOwned is the current number of run leases held by this node.
	RunLeasesOwned = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "caesium_run_leases_owned",
			Help: "Current number of run leases owned by this node (run-owner mode).",
		},
	)

	// DispatchRejectedTotal counts owner-push dispatch attempts that were not
	// accepted by a worker, categorised by reason.
	//
	// reason values:
	//   network_error    – PostDispatch returned a non-nil error (network / timeout)
	//   worker_rejected  – worker returned 409 (busy, claim mismatch, etc.)
	//   no_peers         – peer discovery returned an empty list or failed
	DispatchRejectedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_dispatch_rejected_total",
			Help: "Total owner-push dispatch attempts rejected or failed, by reason.",
		},
		[]string{"reason"},
	)

	// DispatchSentTotal counts dispatch attempts accepted by a worker (202 ACK).
	DispatchSentTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "caesium_dispatch_sent_total",
			Help: "Total owner-push dispatch requests accepted by a worker (202 ACK).",
		},
	)

	// CompleteReportFailedTotal counts the worker-side failures to report a
	// dispatched task's completion back to the owner via /internal/complete.
	// This is the run-owner counterpart to a lost completion: the task ran but
	// the owner never heard about it, so the claim lease will expire and either
	// ClaimNext recovery (for an un-owned/expired run) or a re-dispatch picks it
	// up.  The reason label distinguishes a transport failure from an explicit
	// owner-side fence rejection so operators can tell a partition apart from a
	// stale-owner reject.
	//
	// reason values:
	//   post_error      – PostComplete returned a non-nil error (network/timeout)
	//   owner_rejected  – the owner returned {"accepted": false} (fence violation)
	CompleteReportFailedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_complete_report_failed_total",
			Help: "Total worker-side failures to report a dispatched task's completion to the owner, by reason.",
		},
		[]string{"reason"},
	)

	// CompleteRetryableTotal counts /internal/complete requests the owner could
	// not apply because of transient dqlite contention and answered with 503 so
	// the worker retries the same request.  Unlike CompleteRejectedTotal (a
	// terminal fence violation), a retryable answer is expected under burst load
	// and is not itself a lost completion — it only becomes one if the worker
	// exhausts its retries, which is tracked by
	// caesium_complete_report_failed_total{reason="owner_busy"}.
	//
	// reason values:
	//   contention – the apply (CompleteTaskClaimed/etc) returned a dqlite contention error
	CompleteRetryableTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_complete_retryable_total",
			Help: "Total /internal/complete requests answered 503 (retryable) due to transient contention, by reason.",
		},
		[]string{"reason"},
	)

	// IncidentsTotal counts incident lifecycle transitions by failure class and
	// resulting status. The class label is bounded by the classifier's fixed set
	// of failure classes and status by the incident status machine, so cardinality
	// is bounded (agent-in-the-loop remediation, Phase 0).
	IncidentsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_incidents_total",
			Help: "Total incident lifecycle transitions by failure class and status.",
		},
		[]string{"class", "status"},
	)

	// IncidentResolutionSeconds measures time from incident open to a terminal
	// disposition, labelled by failure class.
	IncidentResolutionSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "caesium_incident_resolution_seconds",
			Help:    "Time from incident open to terminal disposition in seconds, by failure class.",
			Buckets: []float64{1, 5, 30, 60, 300, 900, 1800, 3600, 14400, 86400},
		},
		[]string{"class"},
	)

	// AgentActionsTotal counts remediation actions recorded by the incident action
	// executor, by action type, tier, and actor (policy|agent|human). The type
	// label is bounded by the fixed typed-action catalog, tier by {0,1,2,3}, and
	// actor by the fixed actor set, so cardinality is bounded (agent-in-the-loop
	// remediation, Stream B).
	AgentActionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_agent_actions_total",
			Help: "Total remediation actions recorded by the incident action executor, by action type, tier, and actor.",
		},
		[]string{"type", "tier", "actor"},
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
			TaskRegisterBatchSize,
			JobsActive,
			CallbackRunsTotal,
			TriggerFiresTotal,
			WorkerClaimsTotal,
			TaskPriorityClaimTotal,
			WorkerClaimContentionTotal,
			DBBusyRetriesTotal,
			ReclaimDurationSeconds,
			WorkerLeaseExpirationsTotal,
			TaskRetriesTotal,
			BackfillRunsTotal,
			BackfillsActive,
			TaskCacheHitsTotal,
			TaskCacheMissesTotal,
			TaskCacheShortCircuitsTotal,
			TaskCacheEntries,
			RateLimitAcquiredTotal,
			RateLimitRejectedTotal,
			RunSkippedTotal,
			RunReplacedTotal,
			RunQueueDepth,
			RunQueueWaitSeconds,
			AuthRequestsTotal,
			AuthFailuresTotal,
			AuthKeyAgeSeconds,
			AuditLogEntriesTotal,
			SSOLoginsTotal,
			SSOLoginDurationSeconds,
			SSOLogoutsTotal,
			WebhookAuthFailuresTotal,
			EventTriggerMatchesTotal,
			EventBusDroppedTotal,
			TriggerChainDepth,
			TriggerChainRejectedTotal,
			EventsIngestedTotal,
			EventBridgeFailuresTotal,
			DBWritesTotal,
			DBStatementsTotal,
			CompleteRejectedTotal,
			RunLeaseRenewalsTotal,
			RunLeasesOwned,
			DispatchRejectedTotal,
			DispatchSentTotal,
			CompleteReportFailedTotal,
			CompleteRetryableTotal,
			IncidentsTotal,
			IncidentResolutionSeconds,
			AgentActionsTotal,
		)
	})
}
