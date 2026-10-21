package runqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/callback"
	jobexec "github.com/caesium-cloud/caesium/internal/job"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type LeaderCheck func(context.Context) (bool, error)
type LaunchFunc func(context.Context, *models.Job, *runstorage.JobRun)

type Config struct {
	DB                  *gorm.DB
	Store               *runstorage.Store
	NodeID              string
	Interval            time.Duration
	StaleClaimThreshold time.Duration
	LeaderCheck         LeaderCheck
	LaunchRun           LaunchFunc
	// DispatchRunCallbacks dispatches a job's completion callbacks for a
	// promoted run the dequeuer had to finalize itself because nothing ever
	// executed it (nil: internal/callback's default dispatcher).
	DispatchRunCallbacks func(ctx context.Context, jobID, runID uuid.UUID, runErr error) error
	// QuotaOnly runs the dequeuer for the namespace run quota alone
	// (identity-and-access D1): it drains only rows held by a namespace's
	// quotas.maxConcurrentRuns and never touches a row a job's own queue
	// policy parked. The server sets it when an access policy file is
	// configured but CAESIUM_RUN_QUEUE_ENABLED and
	// CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED are both off, so quota-held starts
	// always drain while ordinary queue rows behave exactly as before — and
	// also, with no file, when quota-held rows a since-removed file parked
	// remain, which then drain under the empty policy.
	QuotaOnly bool
}

type Dequeuer struct {
	db                  *gorm.DB
	store               *runstorage.Store
	nodeID              string
	interval            time.Duration
	staleClaimThreshold time.Duration
	leaderCheck         LeaderCheck
	launchRun           LaunchFunc
	dispatchCallbacks   func(ctx context.Context, jobID, runID uuid.UUID, runErr error) error
	quotaOnly           bool

	// Round-robin cursors (design §10.3). They live in memory on the leader:
	// a leader change restarts the deterministic order, which is fair enough.
	// nsCursor is the namespace that went first on the previous tick;
	// jobCursor[ns] is the job that last got a slot in ns.
	nsCursor  string
	jobCursor map[string]uuid.UUID
	// gaugeLabels are the namespaces this node currently exports
	// caesium_namespace_running_runs for.
	gaugeLabels map[string]struct{}
}

func NewDequeuer(cfg Config) *Dequeuer {
	if cfg.DB == nil {
		panic("run queue dequeuer requires database connection")
	}
	store := cfg.Store
	if store == nil {
		store = runstorage.NewStore(cfg.DB)
	}
	nodeID := strings.TrimSpace(cfg.NodeID)
	if nodeID == "" {
		nodeID = "runqueue"
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = time.Second
	}
	staleClaimThreshold := cfg.StaleClaimThreshold
	if staleClaimThreshold <= 0 {
		staleClaimThreshold = models.DefaultRunQueueClaimStaleAfter
	}
	dispatchCallbacks := cfg.DispatchRunCallbacks
	if dispatchCallbacks == nil {
		dispatchCallbacks = func(ctx context.Context, jobID, runID uuid.UUID, runErr error) error {
			return callback.Default().Dispatch(ctx, jobID, runID, runErr)
		}
	}
	return &Dequeuer{
		dispatchCallbacks:   dispatchCallbacks,
		db:                  cfg.DB,
		store:               store,
		nodeID:              nodeID,
		interval:            interval,
		staleClaimThreshold: staleClaimThreshold,
		leaderCheck:         cfg.LeaderCheck,
		launchRun:           cfg.LaunchRun,
		quotaOnly:           cfg.QuotaOnly,
		jobCursor:           map[string]uuid.UUID{},
		gaugeLabels:         map[string]struct{}{},
	}
}

func (d *Dequeuer) Run(ctx context.Context) {
	if err := d.DrainOnce(ctx); err != nil && ctx.Err() == nil {
		log.Error("run queue dequeuer drain failed", "error", err)
	}

	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.DrainOnce(ctx); err != nil && ctx.Err() == nil {
				log.Error("run queue dequeuer drain failed", "error", err)
			}
		}
	}
}

// queuedJob is one job with unclaimed queue rows, keyed by its CURRENT
// namespace: the namespace a promotion would admit into.
type queuedJob struct {
	JobID     uuid.UUID
	Namespace string
	// QuotaRows reports that at least one row is held by the namespace quota.
	QuotaRows bool
}

// DrainOnce promotes queued runs whose capacity has freed.
//
// Fairness (identity-and-access D1, design §10.3): namespaces are served in a
// deterministic rotating order — sorted, starting after the namespace that
// went first on the previous tick — and inside a namespace that carries a
// run quota, jobs take turns one promotion at a time, starting after the job
// that last got a slot, so one job with a deep queue cannot starve its
// neighbours of the namespace's slots. Every promotion re-admits through the
// store (Store.StartQueuedRun → admit), so the namespace gate and each job's
// own concurrency policy still apply. A namespace without a quota drains
// exactly as it always has: each job in turn, fully.
func (d *Dequeuer) DrainOnce(ctx context.Context) error {
	if runstorage.NamespacePolicyPending() {
		// The dequeuer starts before the server installs its access policy
		// file. Until then no namespace quota can be read: promoting a row
		// (in either mode) could start runs past the quota that parked them,
		// so nothing is claimed. A server with no file configured is not
		// pending: a quota-only dequeuer it started for rows a since-removed
		// file parked drains them, every namespace now being unlimited.
		return nil
	}
	if d.leaderCheck != nil {
		leader, err := d.leaderCheck(ctx)
		if err != nil {
			return err
		}
		if !leader {
			// Only the leader exports the namespace gauge, so a sum across
			// nodes is the cluster value.
			d.clearNamespaceGauge()
			return nil
		}
	}

	if err := d.reclaimStaleClaims(ctx); err != nil {
		return err
	}

	// The namespaces carrying a quota under the policy served right now (a
	// reload takes effect here), and one grouped count of their running runs,
	// which feeds both the gauge and the full-namespace check. No quota, no
	// query.
	quotas := runstorage.QuotaNamespaces()
	var running map[string]int64
	if len(quotas) > 0 {
		counts, err := d.store.RunningByNamespace(ctx)
		if err != nil {
			return err
		}
		running = counts
		d.observeNamespaceGauge(quotas, running)
	} else {
		d.clearNamespaceGauge()
	}

	queued, err := d.listQueuedJobs(ctx)
	if err != nil {
		return err
	}
	if len(queued) == 0 {
		return nil
	}

	byNamespace := map[string][]queuedJob{}
	for _, job := range queued {
		ns := models.NamespaceOrDefault(strings.TrimSpace(job.Namespace))
		byNamespace[ns] = append(byNamespace[ns], job)
	}
	order := rotateAfter(slices.Sorted(maps.Keys(byNamespace)), d.nsCursor)
	d.nsCursor = order[0]
	for _, ns := range order {
		jobs := byNamespace[ns]
		slices.SortFunc(jobs, func(a, b queuedJob) int { return strings.Compare(a.JobID.String(), b.JobID.String()) })
		if limit := quotas[ns]; limit > 0 {
			if err := d.drainQuotaNamespace(ctx, ns, limit, running[ns], jobs); err != nil {
				return err
			}
			continue
		}
		for _, job := range rotateJobsAfter(jobs, d.jobCursor[ns]) {
			if err := d.drainJob(ctx, job); err != nil {
				return err
			}
		}
	}
	return nil
}

// listQueuedJobs lists the jobs with unclaimed queue rows — in quota-only
// mode, only rows held by the namespace quota — with each job's current
// namespace. Rows of a soft-deleted job are left alone, as before.
func (d *Dequeuer) listQueuedJobs(ctx context.Context) ([]queuedJob, error) {
	query := d.db.WithContext(ctx).
		Table("run_queue").
		Select("run_queue.job_id AS job_id, jobs.namespace AS namespace, "+
			"MAX(CASE WHEN run_queue.held_reason = ? THEN 1 ELSE 0 END) AS quota_rows", models.RunQueueHeldReasonNamespaceQuota).
		Joins("JOIN jobs ON jobs.id = run_queue.job_id AND jobs.deleted_at IS NULL").
		Where("run_queue.claimed_by = ''")
	if d.quotaOnly {
		query = query.Where("run_queue.held_reason = ?", models.RunQueueHeldReasonNamespaceQuota)
	}
	var rows []struct {
		JobID     uuid.UUID
		Namespace string
		QuotaRows int
	}
	if err := query.Group("run_queue.job_id, jobs.namespace").Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]queuedJob, 0, len(rows))
	for _, row := range rows {
		out = append(out, queuedJob{JobID: row.JobID, Namespace: row.Namespace, QuotaRows: row.QuotaRows > 0})
	}
	return out, nil
}

func (d *Dequeuer) reclaimStaleClaims(ctx context.Context) error {
	// Same cutoff the queue view classifies rows against, so a row the operator
	// is shown as `stale` is exactly a row this reaper is about to release.
	cutoff := models.RunQueueStaleCutoff(time.Now(), d.staleClaimThreshold)
	stale := func() *gorm.DB {
		query := d.db.WithContext(ctx).
			Model(&models.RunQueue{}).
			Where("claimed_by <> '' AND (claimed_at IS NULL OR claimed_at < ?)", cutoff)
		if d.quotaOnly {
			query = query.Where("held_reason = ?", models.RunQueueHeldReasonNamespaceQuota)
		}
		return query
	}
	if d.quotaOnly {
		// Quota-only mode owns quota-held rows alone, and stays read-only
		// while it has nothing to release.
		var count int64
		if err := stale().Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return nil
		}
	}
	result := stale().Updates(map[string]any{
		"claimed_by": "",
		"claimed_at": nil,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected > 0 {
		log.Warn("run queue dequeuer reclaimed stale claims", "count", result.RowsAffected, "cutoff", cutoff)
	}
	return nil
}

// promotion is what one claim-and-start attempt did.
type promotion int

const (
	// promotionEmpty: no eligible row is left for the job.
	promotionEmpty promotion = iota
	// promotionStarted: a run was created and launched.
	promotionStarted
	// promotionConsumed: the row was removed without a run (skipped by the
	// job's policy, or dropped because it may not run in this namespace).
	promotionConsumed
	// promotionJobFull: the job's own concurrency refused it; try later.
	promotionJobFull
	// promotionNamespaceFull: the namespace quota refused it; try later.
	promotionNamespaceFull
)

// drainJob drains one job in a namespace without a quota. A job with a queue
// policy drains while it has free slots, as it always has (in quota-only mode
// only its quota-held rows); a job without one drains its quota-held rows.
func (d *Dequeuer) drainJob(ctx context.Context, job queuedJob) error {
	cfg, hasPolicy, err := loadConcurrencyConfig(ctx, d.db, job.JobID)
	if err != nil {
		return err
	}
	hasPolicy = hasPolicy && cfg.maxRuns > 0
	quotaRowsOnly := d.quotaOnly || !hasPolicy
	if quotaRowsOnly && !job.QuotaRows {
		return nil
	}
	var active int64
	if hasPolicy {
		if active, err = d.store.CountActive(job.JobID); err != nil {
			return err
		}
	}
	for !hasPolicy || active < int64(cfg.maxRuns) {
		outcome, err := d.promoteOne(ctx, job.JobID, quotaRowsOnly)
		if err != nil {
			return err
		}
		switch outcome {
		case promotionStarted:
			active++
		case promotionConsumed:
		default:
			return nil
		}
	}
	return nil
}

// drainQuotaNamespace drains a namespace that carries a run quota: jobs take
// turns, one promotion each per pass, until the namespace is full or no job
// can promote. running is the namespace's running count at the start of the
// tick; admission stays the authority, the count only avoids claims that are
// bound to be refused.
func (d *Dequeuer) drainQuotaNamespace(ctx context.Context, ns string, limit int, running int64, jobs []queuedJob) error {
	type candidate struct {
		job           queuedJob
		hasPolicy     bool
		maxRuns       int
		active        int64
		quotaRowsOnly bool
		done          bool
	}
	candidates := make([]*candidate, 0, len(jobs))
	for _, job := range rotateJobsAfter(jobs, d.jobCursor[ns]) {
		cfg, hasPolicy, err := loadConcurrencyConfig(ctx, d.db, job.JobID)
		if err != nil {
			return err
		}
		c := &candidate{job: job, hasPolicy: hasPolicy && cfg.maxRuns > 0, maxRuns: cfg.maxRuns}
		c.quotaRowsOnly = d.quotaOnly || !c.hasPolicy
		if c.quotaRowsOnly && !job.QuotaRows {
			continue
		}
		if c.hasPolicy {
			if c.active, err = d.store.CountActive(job.JobID); err != nil {
				return err
			}
		}
		candidates = append(candidates, c)
	}

	full := func() bool { return running >= int64(limit) }
	if full() {
		// Nothing can start here this tick. Claiming rows only to have the
		// gate refuse them would churn writes, so record instead why the rows
		// that would otherwise start are waiting.
		var blocked []blockedJob
		for _, c := range candidates {
			if c.hasPolicy && !c.quotaRowsOnly && c.active < int64(c.maxRuns) {
				blocked = append(blocked, blockedJob{jobID: c.job.JobID, slots: c.maxRuns - int(c.active)})
			}
		}
		return d.annotateBlockedByQuota(ctx, ns, blocked)
	}
	for {
		progressed := false
		for _, c := range candidates {
			if c.done {
				continue
			}
			if full() {
				return nil
			}
			if c.hasPolicy && c.active >= int64(c.maxRuns) {
				c.done = true
				continue
			}
			outcome, err := d.promoteOne(ctx, c.job.JobID, c.quotaRowsOnly)
			if err != nil {
				return err
			}
			switch outcome {
			case promotionStarted:
				progressed = true
				running++
				c.active++
				d.jobCursor[ns] = c.job.JobID
			case promotionConsumed:
				progressed = true
			case promotionNamespaceFull:
				return nil
			default:
				c.done = true
			}
		}
		if !progressed {
			return nil
		}
	}
}

// blockedJob is a job with free slots of its own in a full quota namespace.
type blockedJob struct {
	jobID uuid.UUID
	slots int
}

// annotateBlockedByQuota records why rows wait in a full quota namespace: for
// each job with a queue policy that has free slots of its own, the rows that
// would start now but for the namespace quota are marked namespace_quota
// (once; the reason is sticky). Rows the job's own policy holds are left
// as they are, and quota-only mode never relabels an ordinary row.
func (d *Dequeuer) annotateBlockedByQuota(ctx context.Context, ns string, jobs []blockedJob) error {
	if d.quotaOnly {
		return nil
	}
	for _, job := range jobs {
		marked, err := d.store.AnnotateQuotaHeldRuns(ctx, job.jobID, job.slots)
		if err != nil {
			return err
		}
		if marked > 0 {
			runstorage.RecordNamespaceQuotaDeferrals(ns, int(marked))
		}
	}
	return nil
}

// promoteOne claims the job's next row (only quota-held rows when
// quotaRowsOnly) and tries to start it.
func (d *Dequeuer) promoteOne(ctx context.Context, jobID uuid.UUID, quotaRowsOnly bool) (promotion, error) {
	claim := fmt.Sprintf("%s/%s", d.nodeID, uuid.NewString())
	var (
		queued *models.RunQueue
		err    error
	)
	if quotaRowsOnly {
		queued, err = d.store.DequeueNextQuotaRun(ctx, jobID, claim)
	} else {
		queued, err = d.store.DequeueNextRun(ctx, jobID, claim)
	}
	if err != nil {
		return promotionEmpty, err
	}
	if queued == nil {
		return promotionEmpty, nil
	}
	started, err := d.store.StartQueuedRun(ctx, queued)
	if _, committed := runstorage.CommittedRunID(err); committed {
		// The admission committed — the row is consumed and the run is
		// running — but its read-back failed. Launch exactly that run once it
		// reads back; if it never does, it was finalized so it frees its
		// namespace slot instead of holding it with nothing executing it.
		rec, recoverErr := d.store.RecoverCommittedRun(ctx, err, committedRunReadBackoffs)
		if recoverErr != nil {
			log.Error("run queue dequeuer could not read back a promoted run", "queue_id", queued.ID, "job_id", queued.JobID, "error", recoverErr)
			if rec.DispatchCallbacks() {
				d.dispatchFinalizedCallbacks(ctx, rec.JobID, rec.RunID, rec.Cause)
			}
			return promotionConsumed, nil
		}
		started, err = rec.Run, nil
	}
	if err != nil {
		switch {
		case errors.Is(err, runstorage.ErrQueuedRunClaimLost):
			// Another drainer reclaimed this row after our claim went stale
			// and owns it now (or already promoted it). Admission created
			// nothing; the row is not ours to release.
			log.Warn("run queue dequeuer lost its claim on a queued run before promoting it",
				"queue_id", queued.ID, "job_id", queued.JobID, "claim", claim)
			return promotionEmpty, nil
		case errors.Is(err, runstorage.ErrRunSkipped):
			// Skipped or held upstream: the admission consumed the row with
			// the outcome it recorded, in one transaction.
			return promotionConsumed, nil
		case errors.Is(err, runstorage.ErrQueuedRunNamespaceMoved), errors.Is(err, runstorage.ErrNamespaceUndeclared):
			// The request belongs to a namespace its job has left, or the
			// policy no longer declares its namespace: it must never run.
			reason := runstorage.QueueDropReasonNamespaceMoved
			if errors.Is(err, runstorage.ErrNamespaceUndeclared) {
				reason = runstorage.QueueDropReasonNamespaceUndeclared
			}
			if err := d.store.DropQueuedRun(ctx, queued, reason); err != nil {
				if errors.Is(err, runstorage.ErrQueuedRunClaimLost) {
					// Another drainer owns the row now; it decides.
					log.Warn("run queue dequeuer lost its claim on a queued run before dropping it",
						"queue_id", queued.ID, "job_id", queued.JobID, "claim", claim)
					return promotionEmpty, nil
				}
				d.release(ctx, queued, claim, "dropped")
				return promotionEmpty, err
			}
			log.Warn("run queue dequeuer dropped a queued run that may not start in its job's namespace",
				"queue_id", queued.ID, "job_id", queued.JobID, "reason", reason, "error", err)
			return promotionConsumed, nil
		case errors.Is(err, runstorage.ErrNamespaceQuota):
			flipped, releaseErr := d.store.ReleaseQueuedRunHeldByQuota(ctx, queued, claim)
			if releaseErr != nil {
				log.Warn("run queue dequeuer failed to release a quota-held queued run", "queue_id", queued.ID, "error", releaseErr)
			} else if flipped {
				runstorage.RecordNamespaceQuotaDeferral(err)
			}
			return promotionNamespaceFull, nil
		}
		d.release(ctx, queued, claim, "refused")
		if errors.Is(err, runstorage.ErrMaxConcurrentRunsReached) || errors.Is(err, runstorage.ErrRunQueued) {
			return promotionJobFull, nil
		}
		return promotionEmpty, err
	}
	if started == nil {
		if err := d.store.ReleaseQueuedRun(ctx, queued.ID, claim); err != nil {
			return promotionEmpty, err
		}
		return promotionJobFull, nil
	}
	// The admission that created the run consumed the row in its transaction.
	if err := d.launch(ctx, started); err != nil {
		// The run is committed and running with nothing to execute it:
		// settle it so it frees its slot.
		cause := fmt.Errorf("run queue dequeuer could not launch the promoted run: %w", err)
		finalized, completeErr := d.store.CompleteIfActive(started.ID, cause)
		if completeErr != nil {
			log.Error("run queue dequeuer could not launch or finalize a promoted run", "run_id", started.ID, "error", completeErr)
		} else if finalized && !started.Quarantine {
			d.dispatchFinalizedCallbacks(ctx, started.JobID, started.ID, cause)
		}
		return promotionEmpty, err
	}
	return promotionStarted, nil
}

// dispatchFinalizedCallbacks dispatches a job's completion callbacks for a
// promoted run this dequeuer finalized itself (nothing executed it, so no
// executor will). Callers check that the finalization was theirs and the run
// is not quarantined, so each run gets them exactly once.
func (d *Dequeuer) dispatchFinalizedCallbacks(ctx context.Context, jobID, runID uuid.UUID, cause error) {
	if err := d.dispatchCallbacks(context.WithoutCancel(ctx), jobID, runID, cause); err != nil {
		log.Error("run queue dequeuer callback dispatch failure", "job_id", jobID, "run_id", runID, "error", err)
	}
}

// committedRunReadBackoffs bound how long a promotion whose admission
// committed keeps re-reading its run before finalizing it.
var committedRunReadBackoffs = []time.Duration{50 * time.Millisecond, 250 * time.Millisecond, time.Second}

func (d *Dequeuer) release(ctx context.Context, queued *models.RunQueue, claim, why string) {
	if err := d.store.ReleaseQueuedRun(ctx, queued.ID, claim); err != nil {
		log.Warn("run queue dequeuer failed to release queued run", "queue_id", queued.ID, "after", why, "error", err)
	}
}

// observeNamespaceGauge sets caesium_namespace_running_runs for exactly the
// namespaces that carry a quota now, and deletes the series of namespaces
// that no longer do.
func (d *Dequeuer) observeNamespaceGauge(quotas map[string]int, running map[string]int64) {
	for ns := range d.gaugeLabels {
		if _, ok := quotas[ns]; !ok {
			metrics.NamespaceRunningRuns.DeleteLabelValues(ns)
			delete(d.gaugeLabels, ns)
		}
	}
	for ns := range quotas {
		metrics.NamespaceRunningRuns.WithLabelValues(ns).Set(float64(running[ns]))
		d.gaugeLabels[ns] = struct{}{}
	}
}

func (d *Dequeuer) clearNamespaceGauge() {
	for ns := range d.gaugeLabels {
		metrics.NamespaceRunningRuns.DeleteLabelValues(ns)
		delete(d.gaugeLabels, ns)
	}
}

// rotateAfter returns sorted rotated to start at the first element after
// cursor (wrapping), so consecutive ticks start with consecutive elements.
func rotateAfter(sorted []string, cursor string) []string {
	if len(sorted) == 0 || cursor == "" {
		return sorted
	}
	idx, _ := slices.BinarySearch(sorted, cursor)
	if idx < len(sorted) && sorted[idx] == cursor {
		idx++
	}
	idx %= len(sorted)
	return append(slices.Clone(sorted[idx:]), sorted[:idx]...)
}

// rotateJobsAfter is rotateAfter for jobs sorted by id.
func rotateJobsAfter(jobs []queuedJob, cursor uuid.UUID) []queuedJob {
	if len(jobs) == 0 || cursor == uuid.Nil {
		return jobs
	}
	key := cursor.String()
	idx, _ := slices.BinarySearchFunc(jobs, key, func(j queuedJob, k string) int { return strings.Compare(j.JobID.String(), k) })
	if idx < len(jobs) && jobs[idx].JobID == cursor {
		idx++
	}
	idx %= len(jobs)
	return append(slices.Clone(jobs[idx:]), jobs[:idx]...)
}

func (d *Dequeuer) launch(ctx context.Context, started *runstorage.JobRun) error {
	if started == nil {
		return nil
	}
	var jobModel models.Job
	if err := d.db.WithContext(ctx).First(&jobModel, "id = ?", started.JobID).Error; err != nil {
		return err
	}
	if d.launchRun != nil {
		d.launchRun(ctx, &jobModel, started)
		return nil
	}
	go func() {
		runCtx := runstorage.WithContext(context.WithoutCancel(ctx), started.ID)
		if err := jobexec.New(
			&jobModel,
			jobexec.WithRunStoreFactory(func() *runstorage.Store { return d.store }),
			jobexec.WithParams(started.Params),
		).Run(runCtx); err != nil {
			log.Error("run queue dequeuer job run failure", "job_id", started.JobID, "run_id", started.ID, "error", err)
		}
	}()
	return nil
}

type concurrencyConfig struct {
	maxRuns  int
	strategy string
}

func loadConcurrencyConfig(ctx context.Context, db *gorm.DB, jobID uuid.UUID) (concurrencyConfig, bool, error) {
	var row struct {
		Concurrency datatypes.JSON
	}
	err := db.WithContext(ctx).
		Model(&models.Job{}).
		Select("concurrency").
		Where("id = ?", jobID).
		Take(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return concurrencyConfig{}, false, nil
		}
		return concurrencyConfig{}, false, err
	}
	if len(row.Concurrency) == 0 {
		return concurrencyConfig{}, false, nil
	}
	var cfg *jobdefschema.Concurrency
	if err := json.Unmarshal(row.Concurrency, &cfg); err != nil {
		return concurrencyConfig{}, false, err
	}
	if cfg == nil {
		return concurrencyConfig{}, false, nil
	}
	strategy := strings.ToLower(strings.TrimSpace(cfg.Strategy))
	if strategy == "" {
		strategy = jobdefschema.ConcurrencyStrategyQueue
	}
	if strategy != jobdefschema.ConcurrencyStrategyQueue {
		return concurrencyConfig{}, false, nil
	}
	return concurrencyConfig{maxRuns: cfg.MaxRuns, strategy: strategy}, true, nil
}
