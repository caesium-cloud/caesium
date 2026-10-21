package runqueue

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/metrics"
	metricstest "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

const dequeuerQuotaPolicy = `apiVersion: v1
kind: AccessPolicy
namespaces:
  default: {}
  quota-a:
    quotas:
      maxConcurrentRuns: 1
  quota-c:
    quotas:
      maxConcurrentRuns: 1
  quota-b: {}
`

func useDequeuerPolicy(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	holder, err := policy.LoadFile(t.Context(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	previous := policy.ProcessHolder()
	t.Cleanup(func() { policy.SetProcessHolder(previous) })
	policy.SetProcessHolder(holder)
}

func quotaJob(t *testing.T, db *gorm.DB, alias, namespace string, concurrency *jobdef.Concurrency) *models.Job {
	t.Helper()
	job := &models.Job{ID: uuid.New(), Alias: alias, Namespace: namespace}
	if concurrency != nil {
		raw, err := json.Marshal(concurrency)
		require.NoError(t, err)
		job.Concurrency = datatypes.JSON(raw)
	}
	require.NoError(t, db.Create(job).Error)
	return job
}

func seedQueueRow(t *testing.T, db *gorm.DB, jobID uuid.UUID, namespace *string, held bool, age time.Duration) uuid.UUID {
	t.Helper()
	row := &models.RunQueue{
		ID:        uuid.New(),
		JobID:     jobID,
		Priority:  runstorage.PriorityNormalValue,
		CreatedAt: time.Now().UTC().Add(-age),
		Namespace: namespace,
	}
	if held {
		reason := models.RunQueueHeldReasonNamespaceQuota
		row.HeldReason = &reason
	}
	require.NoError(t, db.Create(row).Error)
	return row.ID
}

func ptr(s string) *string { return &s }

// launchRecorder records each promoted run's job and finishes the run, so
// the next tick sees the namespace slot free again.
type launchRecorder struct {
	db       *gorm.DB
	launched []uuid.UUID
	finish   bool
}

func (r *launchRecorder) launch(_ context.Context, job *models.Job, started *runstorage.JobRun) {
	r.launched = append(r.launched, job.ID)
	if r.finish {
		_ = r.db.Model(&models.JobRun{}).Where("id = ?", started.ID).Update("status", string(runstorage.StatusSucceeded)).Error
	}
}

func newQuotaDequeuer(db *gorm.DB, rec *launchRecorder, quotaOnly bool) *Dequeuer {
	return NewDequeuer(Config{
		DB:        db,
		Store:     runstorage.NewStore(db),
		NodeID:    "node-q",
		LaunchRun: rec.launch,
		QuotaOnly: quotaOnly,
	})
}

func sortedIDs(ids ...uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b uuid.UUID) int {
		switch as, bs := a.String(), b.String(); {
		case as < bs:
			return -1
		case as > bs:
			return 1
		}
		return 0
	})
	return out
}

// TestDequeuerRoundRobinsJobsInAQuotaNamespace: in a namespace with a quota
// of one, two jobs with deep queues take turns — X, Y, X, Y — instead of the
// first job draining its whole queue before the other gets a slot.
func TestDequeuerRoundRobinsJobsInAQuotaNamespace(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	useDequeuerPolicy(t, dequeuerQuotaPolicy)
	a := quotaJob(t, db, "rr-a", "quota-a", nil)
	b := quotaJob(t, db, "rr-b", "quota-a", nil)
	for i := range 3 {
		seedQueueRow(t, db, a.ID, ptr("quota-a"), true, time.Duration(10-i)*time.Second)
		seedQueueRow(t, db, b.ID, ptr("quota-a"), true, time.Duration(10-i)*time.Second)
	}
	rec := &launchRecorder{db: db, finish: true}
	d := newQuotaDequeuer(db, rec, false)
	for range 6 {
		before := len(rec.launched)
		require.NoError(t, d.DrainOnce(t.Context()))
		require.Len(t, rec.launched, before+1, "a quota of one admits one run per freed slot")
	}
	first, second := sortedIDs(a.ID, b.ID)[0], sortedIDs(a.ID, b.ID)[1]
	require.Equal(t, []uuid.UUID{first, second, first, second, first, second}, rec.launched)
}

// TestDequeuerRotatesNamespaces: namespaces are served in a rotating
// deterministic order, so the namespace that went first on one tick goes
// last on the next.
func TestDequeuerRotatesNamespaces(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	useDequeuerPolicy(t, dequeuerQuotaPolicy)
	a := quotaJob(t, db, "ns-a", "quota-a", nil)
	c := quotaJob(t, db, "ns-c", "quota-c", nil)
	for i := range 2 {
		seedQueueRow(t, db, a.ID, ptr("quota-a"), true, time.Duration(10-i)*time.Second)
		seedQueueRow(t, db, c.ID, ptr("quota-c"), true, time.Duration(10-i)*time.Second)
	}
	rec := &launchRecorder{db: db, finish: true}
	d := newQuotaDequeuer(db, rec, false)
	require.NoError(t, d.DrainOnce(t.Context()))
	require.Equal(t, []uuid.UUID{a.ID, c.ID}, rec.launched, "tick one serves quota-a first")
	require.NoError(t, d.DrainOnce(t.Context()))
	require.Equal(t, []uuid.UUID{a.ID, c.ID, c.ID, a.ID}, rec.launched, "tick two starts after the namespace that went first")
}

// TestDequeuerFullNamespaceMakesNoClaimAndRecordsTheHold: a full quota
// namespace is skipped without claiming anything, and the rows of a job that
// has free slots of its own are marked as waiting on the quota — once.
func TestDequeuerFullNamespaceMakesNoClaimAndRecordsTheHold(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	useDequeuerPolicy(t, dequeuerQuotaPolicy)
	holder := quotaJob(t, db, "full-holder", "quota-a", nil)
	store := runstorage.NewStore(db)
	_, err := store.Start(holder.ID, nil)
	require.NoError(t, err)
	queued := quotaJob(t, db, "full-queued", "quota-a", &jobdef.Concurrency{MaxRuns: 2, Strategy: jobdef.ConcurrencyStrategyQueue})
	ids := []uuid.UUID{
		seedQueueRow(t, db, queued.ID, ptr("quota-a"), false, 30*time.Second),
		seedQueueRow(t, db, queued.ID, ptr("quota-a"), false, 20*time.Second),
		seedQueueRow(t, db, queued.ID, ptr("quota-a"), false, 10*time.Second),
	}
	before := metricstest.CounterValue(t, metrics.NamespaceQuotaDeferralsTotal, "quota-a")

	rec := &launchRecorder{db: db}
	d := newQuotaDequeuer(db, rec, false)
	require.NoError(t, d.DrainOnce(t.Context()))
	require.NoError(t, d.DrainOnce(t.Context()))
	require.Empty(t, rec.launched)

	var rows []models.RunQueue
	require.NoError(t, db.Where("job_id = ?", queued.ID).Order("created_at ASC").Find(&rows).Error)
	require.Len(t, rows, 3)
	for _, row := range rows {
		require.Empty(t, row.ClaimedBy, "a full namespace is skipped without a claim")
	}
	require.Equal(t, ids[0], rows[0].ID)
	require.True(t, rows[0].IsQuotaHeld(), "the job has two free slots; its head rows wait on the namespace")
	require.True(t, rows[1].IsQuotaHeld())
	require.False(t, rows[2].IsQuotaHeld(), "a row beyond the job's own slots waits on the job")
	require.Equal(t, before+2, metricstest.CounterValue(t, metrics.NamespaceQuotaDeferralsTotal, "quota-a"),
		"each row is counted once, however many ticks it waits")
}

// TestDequeuerQuotaOnlyLeavesOrdinaryRowsAlone: with the queue flags off the
// dequeuer drains only quota-held rows; a row a job's own policy queued, and a
// legacy row of a policy-free job, stay exactly where they were.
func TestDequeuerQuotaOnlyLeavesOrdinaryRowsAlone(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	useDequeuerPolicy(t, dequeuerQuotaPolicy)
	policyJob := quotaJob(t, db, "qo-policy", "quota-b", &jobdef.Concurrency{MaxRuns: 5, Strategy: jobdef.ConcurrencyStrategyQueue})
	ordinary := seedQueueRow(t, db, policyJob.ID, ptr("quota-b"), false, 20*time.Second)
	held := seedQueueRow(t, db, policyJob.ID, ptr("quota-b"), true, 10*time.Second)
	freeJob := quotaJob(t, db, "qo-free", "quota-b", nil)
	legacy := seedQueueRow(t, db, freeJob.ID, nil, false, 10*time.Second)

	rec := &launchRecorder{db: db}
	d := newQuotaDequeuer(db, rec, true)
	require.NoError(t, d.DrainOnce(t.Context()))
	require.Equal(t, []uuid.UUID{policyJob.ID}, rec.launched, "only the quota-held row is promoted")

	var remaining []uuid.UUID
	require.NoError(t, db.Model(&models.RunQueue{}).Order("created_at ASC").Pluck("id", &remaining).Error)
	require.ElementsMatch(t, []uuid.UUID{ordinary, legacy}, remaining)
	require.NotContains(t, remaining, held)

	// The full dequeuer drains the ordinary policy row as it always has, and
	// still leaves a policy-free job's legacy row alone.
	full := newQuotaDequeuer(db, rec, false)
	require.NoError(t, full.DrainOnce(t.Context()))
	remaining = nil
	require.NoError(t, db.Model(&models.RunQueue{}).Pluck("id", &remaining).Error)
	require.Equal(t, []uuid.UUID{legacy}, remaining)
}

// TestDequeuerQuotaOnlyWaitsForThePolicy: quota-only mode promotes nothing
// until the server has installed the policy file it is configured with.
func TestDequeuerQuotaOnlyWaitsForThePolicy(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	previous := policy.ProcessHolder()
	t.Cleanup(func() {
		runstorage.ExpectNamespacePolicy(false)
		policy.SetProcessHolder(previous)
	})
	policy.SetProcessHolder(nil)
	runstorage.ExpectNamespacePolicy(true)
	job := quotaJob(t, db, "qo-wait", "quota-a", nil)
	seedQueueRow(t, db, job.ID, ptr("quota-a"), true, time.Second)

	rec := &launchRecorder{db: db}
	require.NoError(t, newQuotaDequeuer(db, rec, true).DrainOnce(t.Context()))
	require.Empty(t, rec.launched)
}

// TestTwoDrainersPromoteAReclaimedRequestOnce: drainer A claims a request and
// stalls past the stale threshold; drainer B's reaper releases the claim and
// B promotes the request. When A resumes, its promotion creates nothing: the
// admission consumes the row only under the claim that is current.
func TestTwoDrainersPromoteAReclaimedRequestOnce(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	useDequeuerPolicy(t, dequeuerQuotaPolicy)
	job := quotaJob(t, db, "two-drainers", "quota-b", nil)
	seedQueueRow(t, db, job.ID, ptr("quota-b"), true, time.Minute)
	store := runstorage.NewStore(db)

	stalled, err := store.DequeueNextQuotaRun(t.Context(), job.ID, "node-a/stalled")
	require.NoError(t, err)
	require.NotNil(t, stalled)
	require.NoError(t, db.Model(&models.RunQueue{}).Where("id = ?", stalled.ID).
		Update("claimed_at", time.Now().UTC().Add(-time.Hour)).Error)

	rec := &launchRecorder{db: db}
	require.NoError(t, newQuotaDequeuer(db, rec, true).DrainOnce(t.Context()))
	require.Equal(t, []uuid.UUID{job.ID}, rec.launched, "drainer B reclaims and promotes")

	_, err = store.StartQueuedRun(t.Context(), stalled)
	require.ErrorIs(t, err, runstorage.ErrQueuedRunClaimLost)
	var runs int64
	require.NoError(t, db.Model(&models.JobRun{}).Where("job_id = ?", job.ID).Count(&runs).Error)
	require.Equal(t, int64(1), runs, "one request, one run")
	var left int64
	require.NoError(t, db.Model(&models.RunQueue{}).Count(&left).Error)
	require.Zero(t, left)
}

// faultPromotionReadBacks fails detached run read-backs (the joined job_runs
// load outside a transaction): the first n, or all while n < 0 until disarm.
func faultPromotionReadBacks(t *testing.T, db *gorm.DB, n int) (disarm func()) {
	t.Helper()
	orig := committedRunReadBackoffs
	committedRunReadBackoffs = []time.Duration{0, 0}
	var reads atomic.Int64
	var off atomic.Bool
	const callback = "test:fault_promotion_read_back"
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		_, transaction := tx.Statement.ConnPool.(gorm.TxCommitter)
		if off.Load() || tx.Statement.Table != "job_runs" || len(tx.Statement.Joins) == 0 || transaction {
			return
		}
		if seen := reads.Add(1); n < 0 || seen <= int64(n) {
			_ = tx.AddError(errors.New("database is locked"))
		}
	}))
	t.Cleanup(func() {
		committedRunReadBackoffs = orig
		_ = db.Callback().Query().Remove(callback)
	})
	return func() { off.Store(true) }
}

// TestDequeuerLaunchesAPromotionWhoseReadBackRecovers: the admission
// committed (row consumed, run running) but its read-back failed; the
// dequeuer re-reads that exact run and launches it.
func TestDequeuerLaunchesAPromotionWhoseReadBackRecovers(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	useDequeuerPolicy(t, dequeuerQuotaPolicy)
	job := quotaJob(t, db, "readback-transient", "quota-a", nil)
	seedQueueRow(t, db, job.ID, ptr("quota-a"), true, time.Second)
	faultPromotionReadBacks(t, db, 2)

	rec := &launchRecorder{db: db}
	var dispatched []uuid.UUID
	require.NoError(t, newReadBackDequeuer(db, rec, &dispatched).DrainOnce(t.Context()))
	require.Equal(t, []uuid.UUID{job.ID}, rec.launched, "the committed run is launched")
	require.Empty(t, dispatched, "its executor owns the callbacks")
	var running int64
	require.NoError(t, db.Model(&models.JobRun{}).Where("job_id = ? AND status = ?", job.ID, string(runstorage.StatusRunning)).Count(&running).Error)
	require.Equal(t, int64(1), running)
}

// newReadBackDequeuer is a quota-only dequeuer that records the runs it
// dispatches completion callbacks for.
func newReadBackDequeuer(db *gorm.DB, rec *launchRecorder, dispatched *[]uuid.UUID) *Dequeuer {
	return NewDequeuer(Config{
		DB:        db,
		Store:     runstorage.NewStore(db),
		NodeID:    "node-q",
		LaunchRun: rec.launch,
		QuotaOnly: true,
		DispatchRunCallbacks: func(_ context.Context, _, runID uuid.UUID, runErr error) error {
			if runErr == nil {
				return errors.New("a finalized run's callbacks carry its failure")
			}
			*dispatched = append(*dispatched, runID)
			return nil
		},
	})
}

// TestDequeuerFinalizesAPromotionThatNeverReadsBack: if the committed run
// never reads back, the dequeuer finalizes it instead of leaving it running
// with nothing executing it; quota-a's single slot frees and the next
// request is promoted.
func TestDequeuerFinalizesAPromotionThatNeverReadsBack(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	useDequeuerPolicy(t, dequeuerQuotaPolicy)
	stuck := quotaJob(t, db, "readback-stuck", "quota-a", nil)
	seedQueueRow(t, db, stuck.ID, ptr("quota-a"), true, 2*time.Second)
	disarm := faultPromotionReadBacks(t, db, -1)

	rec := &launchRecorder{db: db}
	var dispatched []uuid.UUID
	d := newReadBackDequeuer(db, rec, &dispatched)
	require.NoError(t, d.DrainOnce(t.Context()))
	require.Empty(t, rec.launched)
	var runs []models.JobRun
	require.NoError(t, db.Where("job_id = ?", stuck.ID).Find(&runs).Error)
	require.Len(t, runs, 1)
	require.Equal(t, string(runstorage.StatusFailed), runs[0].Status, "finalized, never left running")
	require.Equal(t, []uuid.UUID{runs[0].ID}, dispatched, "its job's callbacks go out once")
	var left int64
	require.NoError(t, db.Model(&models.RunQueue{}).Where("job_id = ?", stuck.ID).Count(&left).Error)
	require.Zero(t, left)

	disarm()
	next := quotaJob(t, db, "readback-next", "quota-a", nil)
	seedQueueRow(t, db, next.ID, ptr("quota-a"), true, time.Second)
	require.NoError(t, d.DrainOnce(t.Context()))
	require.Equal(t, []uuid.UUID{next.ID}, rec.launched, "quota-a's slot was freed")
	require.Equal(t, []uuid.UUID{runs[0].ID}, dispatched, "no second dispatch")
}

// TestDequeuerQuotaOnlyDrainsRowsAfterThePolicyIsRemoved: a policy file parked
// quota-held rows, then the operator removed it and restarted. The server
// starts the quota-only dequeuer for them (runQueueDrainMode) with no policy
// expected: every namespace is now unlimited, so each held row is promoted,
// and an ordinary policy row is still left alone.
func TestDequeuerQuotaOnlyDrainsRowsAfterThePolicyIsRemoved(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	previous := policy.ProcessHolder()
	t.Cleanup(func() { policy.SetProcessHolder(previous) })
	policy.SetProcessHolder(nil)
	runstorage.ExpectNamespacePolicy(false)
	free := quotaJob(t, db, "qo-removed-free", "quota-a", nil)
	seedQueueRow(t, db, free.ID, ptr("quota-a"), true, 3*time.Second)
	seedQueueRow(t, db, free.ID, ptr("quota-a"), true, 2*time.Second)
	policyJob := quotaJob(t, db, "qo-removed-policy", "quota-b", &jobdef.Concurrency{MaxRuns: 5, Strategy: jobdef.ConcurrencyStrategyQueue})
	ordinary := seedQueueRow(t, db, policyJob.ID, ptr("quota-b"), false, time.Second)

	has, err := runstorage.NewStore(db).HasQuotaHeldRuns(t.Context())
	require.NoError(t, err)
	require.True(t, has, "the startup check sees the leftover rows")

	rec := &launchRecorder{db: db}
	require.NoError(t, newQuotaDequeuer(db, rec, true).DrainOnce(t.Context()))
	require.Equal(t, []uuid.UUID{free.ID, free.ID}, rec.launched,
		"with no quota left, both held runs of the namespace start")
	var remaining []uuid.UUID
	require.NoError(t, db.Model(&models.RunQueue{}).Pluck("id", &remaining).Error)
	require.Equal(t, []uuid.UUID{ordinary}, remaining)

	has, err = runstorage.NewStore(db).HasQuotaHeldRuns(t.Context())
	require.NoError(t, err)
	require.False(t, has, "nothing is left for a later restart to drain")
}

// TestDequeuerFullModeWaitsForAConfiguredPolicy: with the queue flags on, the
// dequeuer still claims nothing while the server's configured policy file is
// not installed yet — a quota-held row (or an ordinary one) promoted then
// would start past a quota nobody has read. Once installed, the quota drains.
func TestDequeuerFullModeWaitsForAConfiguredPolicy(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	previous := policy.ProcessHolder()
	t.Cleanup(func() {
		runstorage.ExpectNamespacePolicy(false)
		policy.SetProcessHolder(previous)
	})
	policy.SetProcessHolder(nil)
	runstorage.ExpectNamespacePolicy(true)
	held := quotaJob(t, db, "full-wait-held", "quota-a", nil)
	seedQueueRow(t, db, held.ID, ptr("quota-a"), true, 2*time.Second)
	seedQueueRow(t, db, held.ID, ptr("quota-a"), true, time.Second)
	ordinary := quotaJob(t, db, "full-wait-policy", "quota-b", &jobdef.Concurrency{MaxRuns: 5, Strategy: jobdef.ConcurrencyStrategyQueue})
	seedQueueRow(t, db, ordinary.ID, ptr("quota-b"), false, time.Second)

	rec := &launchRecorder{db: db}
	d := newQuotaDequeuer(db, rec, false)
	require.NoError(t, d.DrainOnce(t.Context()))
	require.Empty(t, rec.launched, "nothing is promoted before the policy file is installed")
	var claimed int64
	require.NoError(t, db.Model(&models.RunQueue{}).Where("claimed_by <> ''").Count(&claimed).Error)
	require.Zero(t, claimed)

	useDequeuerPolicy(t, dequeuerQuotaPolicy)
	require.NoError(t, d.DrainOnce(t.Context()))
	require.ElementsMatch(t, []uuid.UUID{held.ID, ordinary.ID}, rec.launched,
		"once installed, quota-a's cap of one admits one held row and quota-b drains")
}

// TestDequeuerDropsRowsThatMayNotRunInTheirJobsNamespace: a row recorded in
// a namespace its job has left, and a row whose namespace the policy no longer
// declares, are dropped — never promoted under another owner.
func TestDequeuerDropsRowsThatMayNotRunInTheirJobsNamespace(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	useDequeuerPolicy(t, dequeuerQuotaPolicy)
	moved := quotaJob(t, db, "drop-moved", "quota-b", nil)
	seedQueueRow(t, db, moved.ID, ptr("quota-a"), true, time.Second)
	orphan := quotaJob(t, db, "drop-undeclared", "gone-ns", nil)
	seedQueueRow(t, db, orphan.ID, ptr("gone-ns"), true, time.Second)

	rec := &launchRecorder{db: db}
	require.NoError(t, newQuotaDequeuer(db, rec, false).DrainOnce(t.Context()))
	require.Empty(t, rec.launched)
	var rows, runs int64
	require.NoError(t, db.Model(&models.RunQueue{}).Count(&rows).Error)
	require.NoError(t, db.Model(&models.JobRun{}).Count(&runs).Error)
	require.Zero(t, rows)
	require.Zero(t, runs)
}

// TestDequeuerExportsTheNamespaceGaugeOnTheLeaderOnly: the leader exports the
// running count of exactly the namespaces that carry a quota (zero included);
// a node that is not the leader exports none.
func TestDequeuerExportsTheNamespaceGaugeOnTheLeaderOnly(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	useDequeuerPolicy(t, dequeuerQuotaPolicy)
	metrics.NamespaceRunningRuns.Reset()
	t.Cleanup(metrics.NamespaceRunningRuns.Reset)
	job := quotaJob(t, db, "gauge-job", "quota-a", nil)
	_, err := runstorage.NewStore(db).Start(job.ID, nil)
	require.NoError(t, err)

	leader := true
	d := NewDequeuer(Config{
		DB:          db,
		Store:       runstorage.NewStore(db),
		LeaderCheck: func(context.Context) (bool, error) { return leader, nil },
	})
	require.NoError(t, d.DrainOnce(t.Context()))
	require.Equal(t, 2, gaugeSeries(metrics.NamespaceRunningRuns), "quota-a and quota-c, not quota-b or default")
	require.Equal(t, float64(1), metricstest.GaugeValue(t, metrics.NamespaceRunningRuns.WithLabelValues("quota-a")))
	require.Equal(t, float64(0), metricstest.GaugeValue(t, metrics.NamespaceRunningRuns.WithLabelValues("quota-c")))

	leader = false
	require.NoError(t, d.DrainOnce(t.Context()))
	require.Zero(t, gaugeSeries(metrics.NamespaceRunningRuns), "a follower exports no namespace gauge")
}

// gaugeSeries counts the series a gauge vector currently exports.
func gaugeSeries(vec *prometheus.GaugeVec) int {
	ch := make(chan prometheus.Metric, 16)
	go func() {
		vec.Collect(ch)
		close(ch)
	}()
	n := 0
	for range ch {
		n++
	}
	return n
}
