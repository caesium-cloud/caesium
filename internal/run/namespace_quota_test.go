package run

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/metrics"
	metricstest "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// quotaPolicy declares quota-a with a cap of one, quota-b with no quota, and
// default with no settings. Namespaces are unique to these tests so the
// process-wide holder they install cannot affect other packages' fixtures.
const quotaPolicy = `apiVersion: v1
kind: AccessPolicy
namespaces:
  default: {}
  quota-a:
    quotas:
      maxConcurrentRuns: 1
  quota-b: {}
`

// useAccessPolicy serves body as the process-wide policy file for the test.
func useAccessPolicy(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	holder, err := policy.LoadFile(t.Context(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	previous := policy.ProcessHolder()
	t.Cleanup(func() { policy.SetProcessHolder(previous) })
	policy.SetProcessHolder(holder)
}

func createNamespacedJob(t *testing.T, db *gorm.DB, alias, namespace string, concurrency *jobdefschema.Concurrency) *models.Job {
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

func seedNamespacedRun(t *testing.T, db *gorm.DB, jobID uuid.UUID, namespace string, status Status, mutate func(*models.JobRun)) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()
	row := &models.JobRun{
		ID:        uuid.New(),
		JobID:     jobID,
		Namespace: namespace,
		Status:    string(status),
		StartedAt: now.Add(-time.Minute),
		CreatedAt: now.Add(-time.Minute),
		UpdatedAt: now,
	}
	if status != StatusRunning {
		row.CompletedAt = &now
	}
	if mutate != nil {
		mutate(row)
	}
	require.NoError(t, db.Create(row).Error)
	return row.ID
}

func quotaDeferrals(t *testing.T, namespace string) float64 {
	t.Helper()
	return metricstest.CounterValue(t, metrics.NamespaceQuotaDeferralsTotal, namespace)
}

func runningIn(t *testing.T, db *gorm.DB, namespace string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Model(&models.JobRun{}).Where("namespace = ? AND status = ?", namespace, string(StatusRunning)).Count(&n).Error)
	return n
}

func quotaHeldRows(t *testing.T, db *gorm.DB, jobID uuid.UUID) []models.RunQueue {
	t.Helper()
	var rows []models.RunQueue
	require.NoError(t, db.Where("job_id = ?", jobID).Order("created_at ASC").Find(&rows).Error)
	return rows
}

// TestNamespaceQuotaParksTriggerStartsAtCap: at its namespace's cap a start
// is parked in run_queue with held_reason namespace_quota and the reason on
// its outcome; another namespace is unaffected; each park counts a deferral.
func TestNamespaceQuotaParksTriggerStartsAtCap(t *testing.T) {
	db, store := openIdempotencyStore(t)
	useAccessPolicy(t, quotaPolicy)
	first := createNamespacedJob(t, db, "quota-first", "quota-a", nil)
	second := createNamespacedJob(t, db, "quota-second", "quota-a", nil)
	other := createNamespacedJob(t, db, "quota-other", "quota-b", nil)
	before := quotaDeferrals(t, "quota-a")

	running, err := store.Start(first.ID, nil)
	require.NoError(t, err)
	require.Equal(t, StatusRunning, running.Status)

	result, err := store.StartWithResult(t.Context(), second.ID, nil)
	require.NoError(t, err)
	require.Equal(t, StartOutcomeQueued, result.Outcome)
	require.Equal(t, StartQueueReasonNamespaceQuota, result.Reason)
	require.NotNil(t, result.QueueID)
	require.Nil(t, result.RunID)

	_, err = store.Start(second.ID, nil)
	require.ErrorIs(t, err, ErrRunQueued, "a quota park is a queued start to every existing caller")
	require.ErrorIs(t, err, ErrRunQueuedByQuota)

	rows := quotaHeldRows(t, db, second.ID)
	require.Len(t, rows, 2)
	for _, row := range rows {
		require.True(t, row.IsQuotaHeld())
		require.NotNil(t, row.Namespace)
		require.Equal(t, "quota-a", *row.Namespace, "the queued request records the namespace it belongs to")
	}
	require.Equal(t, int64(1), runningIn(t, db, "quota-a"))
	require.Equal(t, before+2, quotaDeferrals(t, "quota-a"))

	otherRun, err := store.Start(other.ID, nil)
	require.NoError(t, err)
	require.NotNil(t, otherRun, "a namespace without a quota is unlimited")
}

// TestNamespaceQuotaOutranksEveryJobStrategy: the quota parks a start whatever
// the job's own concurrency strategy says — never a skip, a refusal or a
// replace-cancel of a live run.
func TestNamespaceQuotaOutranksEveryJobStrategy(t *testing.T) {
	db, store := openIdempotencyStore(t)
	useAccessPolicy(t, quotaPolicy)
	holder := createNamespacedJob(t, db, "quota-holder", "quota-a", nil)
	holderRun, err := store.Start(holder.ID, nil)
	require.NoError(t, err)

	for _, strategy := range []string{
		jobdefschema.ConcurrencyStrategySkip,
		jobdefschema.ConcurrencyStrategyFail,
		jobdefschema.ConcurrencyStrategyReplace,
		jobdefschema.ConcurrencyStrategyQueue,
	} {
		job := createNamespacedJob(t, db, "quota-"+strategy, "quota-a", &jobdefschema.Concurrency{MaxRuns: 3, Strategy: strategy})
		result, err := store.StartWithResult(t.Context(), job.ID, nil)
		require.NoError(t, err, strategy)
		require.Equal(t, StartOutcomeQueued, result.Outcome, strategy)
		require.Equal(t, StartQueueReasonNamespaceQuota, result.Reason, strategy)
	}
	live, err := store.Get(holderRun.ID)
	require.NoError(t, err)
	require.Equal(t, StatusRunning, live.Status, "a replace strategy must not cancel a run to dodge the quota")
}

// TestNamespaceQuotaConcurrentStartsAdmitExactlyCap: many concurrent starts
// in a capped namespace admit exactly the cap and park the rest.
func TestNamespaceQuotaConcurrentStartsAdmitExactlyCap(t *testing.T) {
	db, store := openIdempotencyStore(t)
	useAccessPolicy(t, quotaPolicy)
	const starts = 6
	jobs := make([]*models.Job, starts)
	for i := range jobs {
		jobs[i] = createNamespacedJob(t, db, "quota-race-"+string(rune('a'+i)), "quota-a", nil)
	}
	var wg sync.WaitGroup
	errs := make([]error, starts)
	for i := range jobs {
		wg.Go(func() {
			_, errs[i] = store.Start(jobs[i].ID, nil)
		})
	}
	wg.Wait()
	created, queued := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			created++
		case errors.Is(err, ErrRunQueuedByQuota):
			queued++
		default:
			t.Fatalf("unexpected start error: %v", err)
		}
	}
	require.Equal(t, 1, created)
	require.Equal(t, starts-1, queued)
	require.Equal(t, int64(1), runningIn(t, db, "quota-a"))
	var held int64
	require.NoError(t, db.Model(&models.RunQueue{}).Where("held_reason = ?", models.RunQueueHeldReasonNamespaceQuota).Count(&held).Error)
	require.Equal(t, int64(starts-1), held)
}

// TestNamespaceQuotaAdmitRunDefersToStart: AdmitRun writes nothing for a
// policy-free job, so the executor's following Start is the one the quota
// gates — the legacy resume path is untouched.
func TestNamespaceQuotaAdmitRunDefersToStart(t *testing.T) {
	db, store := openIdempotencyStore(t)
	useAccessPolicy(t, quotaPolicy)
	holder := createNamespacedJob(t, db, "quota-admit-holder", "quota-a", nil)
	_, err := store.Start(holder.ID, nil)
	require.NoError(t, err)
	job := createNamespacedJob(t, db, "quota-admit", "quota-a", nil)

	admitted, handled, err := store.AdmitRun(job.ID, nil)
	require.NoError(t, err)
	require.False(t, handled)
	require.Nil(t, admitted)
	require.Empty(t, quotaHeldRows(t, db, job.ID), "AdmitRun must not park a policy-free start")

	_, err = store.Start(job.ID, nil)
	require.ErrorIs(t, err, ErrRunQueuedByQuota)
}

// TestNamespaceQuotaCountsReplaysAndBackfills: quarantined replays and
// backfill children are real compute and occupy the namespace's slots.
func TestNamespaceQuotaCountsReplaysAndBackfills(t *testing.T) {
	for name, mutate := range map[string]func(*models.JobRun){
		"quarantined replay": func(r *models.JobRun) { r.Quarantine = true },
		"backfill child": func(r *models.JobRun) {
			id := uuid.New()
			r.BackfillID = &id
		},
	} {
		t.Run(name, func(t *testing.T) {
			db, store := openIdempotencyStore(t)
			useAccessPolicy(t, quotaPolicy)
			occupant := createNamespacedJob(t, db, "quota-occupant", "quota-a", nil)
			seedNamespacedRun(t, db, occupant.ID, "quota-a", StatusRunning, mutate)
			job := createNamespacedJob(t, db, "quota-blocked", "quota-a", nil)
			_, err := store.Start(job.ID, nil)
			require.ErrorIs(t, err, ErrRunQueuedByQuota)
		})
	}
}

// TestNamespaceQuotaRefusesBackfillChildAtCap: a backfill child is never
// queued; admission refuses it with nothing written and the driver waits.
func TestNamespaceQuotaRefusesBackfillChildAtCap(t *testing.T) {
	db, store := openIdempotencyStore(t)
	useAccessPolicy(t, quotaPolicy)
	holder := createNamespacedJob(t, db, "quota-bf-holder", "quota-a", nil)
	_, err := store.Start(holder.ID, nil)
	require.NoError(t, err)
	job := createNamespacedJob(t, db, "quota-bf", "quota-a", nil)

	_, err = store.StartForBackfill(job.ID, uuid.New(), map[string]string{"logical_date": "2026-01-01T00:00:00Z"})
	quota, ok := errors.AsType[*NamespaceQuotaError](err)
	require.True(t, ok, "want a namespace quota refusal, got %v", err)
	require.Equal(t, "quota-a", quota.Namespace)
	require.Equal(t, 1, quota.Limit)
	require.Zero(t, countJobRuns(t, db, job.ID))
	require.Empty(t, quotaHeldRows(t, db, job.ID), "a backfill child is never queued")
}

// TestNamespaceQuotaRefusesRetriesAtCap: whole-run retries — manual, agent and
// quarantined alike — are refused at cap and leave the run terminal; they
// succeed once a slot frees.
func TestNamespaceQuotaRefusesRetriesAtCap(t *testing.T) {
	db, store := openIdempotencyStore(t)
	useAccessPolicy(t, quotaPolicy)
	holder := createNamespacedJob(t, db, "quota-retry-holder", "quota-a", nil)
	holderRun := seedNamespacedRun(t, db, holder.ID, "quota-a", StatusRunning, nil)
	job := createNamespacedJob(t, db, "quota-retry", "quota-a", nil)
	manual := seedNamespacedRun(t, db, job.ID, "quota-a", StatusFailed, nil)
	agent := seedNamespacedRun(t, db, job.ID, "quota-a", StatusFailed, nil)
	quarantined := seedNamespacedRun(t, db, job.ID, "quota-a", StatusFailed, func(r *models.JobRun) { r.Quarantine = true })
	before := quotaDeferrals(t, "quota-a")

	_, err := store.RetryFromFailure(manual)
	require.ErrorIs(t, err, ErrNamespaceQuota)
	_, err = store.RetryFromFailureAdmitted(agent)
	require.ErrorIs(t, err, ErrNamespaceQuota)
	_, err = store.RetryFromFailure(quarantined)
	require.ErrorIs(t, err, ErrNamespaceQuota)
	for _, id := range []uuid.UUID{manual, agent, quarantined} {
		var row models.JobRun
		require.NoError(t, db.First(&row, "id = ?", id).Error)
		require.Equal(t, string(StatusFailed), row.Status, "a refused retry leaves the run terminal")
	}
	require.Equal(t, before+3, quotaDeferrals(t, "quota-a"))

	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", holderRun).Update("status", string(StatusSucceeded)).Error)
	retried, err := store.RetryFromFailure(manual)
	require.NoError(t, err)
	require.Equal(t, StatusRunning, retried.Status)
}

// TestNamespaceMovedOutranksQuota: a retry of a run its job left is refused as
// namespace_moved even when the old namespace is full.
func TestNamespaceMovedOutranksQuota(t *testing.T) {
	db, store := openIdempotencyStore(t)
	useAccessPolicy(t, quotaPolicy)
	holder := createNamespacedJob(t, db, "quota-moved-holder", "quota-a", nil)
	seedNamespacedRun(t, db, holder.ID, "quota-a", StatusRunning, nil)
	job := createNamespacedJob(t, db, "quota-moved", "quota-b", nil)
	old := seedNamespacedRun(t, db, job.ID, "quota-a", StatusFailed, nil)

	_, err := store.RetryFromFailure(old)
	require.ErrorIs(t, err, ErrRunNamespaceMoved)
	require.NotErrorIs(t, err, ErrNamespaceQuota)
}

// TestNamespaceQuotaGatesPartitionReopen: a partition retry that reopens a
// finished run passes the quota; it is refused at cap with the run left
// terminal, and admitted once a slot frees.
func TestNamespaceQuotaGatesPartitionReopen(t *testing.T) {
	f := newFanOutFixture(t, &jobdefschema.FanOut{From: "discover", MaxPartitions: 16})
	_, err := f.expand(t, strParts("a", "b"))
	require.NoError(t, err)
	rows := finishFannedRun(t, f, TaskStatusFailed, TaskStatusSucceeded)
	useAccessPolicy(t, `apiVersion: v1
kind: AccessPolicy
namespaces:
  default:
    quotas:
      maxConcurrentRuns: 1
`)
	occupant := createNamespacedJob(t, f.db, "quota-partition-occupant", models.DefaultNamespace, nil)
	occupantRun := seedNamespacedRun(t, f.db, occupant.ID, models.DefaultNamespace, StatusRunning, nil)

	_, reopened, err := f.store.RetryPartition(context.Background(), f.runID, rows[0].ID)
	require.ErrorIs(t, err, ErrNamespaceQuota)
	require.False(t, reopened)
	var run models.JobRun
	require.NoError(t, f.db.First(&run, "id = ?", f.runID).Error)
	require.Equal(t, string(StatusFailed), run.Status)

	require.NoError(t, f.db.Model(&models.JobRun{}).Where("id = ?", occupantRun).Update("status", string(StatusSucceeded)).Error)
	_, reopened, err = f.store.RetryPartition(context.Background(), f.runID, rows[0].ID)
	require.NoError(t, err)
	require.True(t, reopened)
}

// TestNamespaceUndeclaredRefusesEveryStart: with a policy file, a namespace
// the served revision does not declare admits nothing — not unlimited, not
// parked forever. Without a file every namespace is unlimited, as before.
func TestNamespaceUndeclaredRefusesEveryStart(t *testing.T) {
	db, store := openIdempotencyStore(t)
	job := createNamespacedJob(t, db, "quota-undeclared", "removed-ns", nil)
	failed := seedNamespacedRun(t, db, job.ID, "removed-ns", StatusFailed, nil)

	policy.SetProcessHolder(nil)
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	unlimited, err := store.Start(job.ID, nil)
	require.NoError(t, err, "without a policy file any namespace is unlimited")
	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", unlimited.ID).Update("status", string(StatusSucceeded)).Error)

	useAccessPolicy(t, quotaPolicy)
	_, err = store.Start(job.ID, nil)
	require.ErrorIs(t, err, ErrNamespaceUndeclared)
	_, err = store.StartForBackfill(job.ID, uuid.New(), nil)
	require.ErrorIs(t, err, ErrNamespaceUndeclared)
	_, err = store.RetryFromFailure(failed)
	require.ErrorIs(t, err, ErrNamespaceUndeclared)
	require.Equal(t, int64(2), countJobRuns(t, db, job.ID), "a refused start writes nothing")
	require.Empty(t, quotaHeldRows(t, db, job.ID))
}

// TestQueuedRunNeverPromotedAcrossANamespaceMove: a queued request belongs to
// the namespace it was enqueued in; after its job moves, promotion refuses it.
// A legacy row with no recorded namespace keeps the old behaviour.
func TestQueuedRunNeverPromotedAcrossANamespaceMove(t *testing.T) {
	db, store := openIdempotencyStore(t)
	useAccessPolicy(t, quotaPolicy)
	holder := createNamespacedJob(t, db, "quota-move-holder", "quota-a", nil)
	holderRun, err := store.Start(holder.ID, nil)
	require.NoError(t, err)
	job := createNamespacedJob(t, db, "quota-move", "quota-a", nil)
	_, err = store.Start(job.ID, nil)
	require.ErrorIs(t, err, ErrRunQueuedByQuota)
	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", holderRun.ID).Update("status", string(StatusSucceeded)).Error)
	require.NoError(t, db.Model(job).Update("namespace", "quota-b").Error)

	rows := quotaHeldRows(t, db, job.ID)
	require.Len(t, rows, 1)
	_, err = store.StartQueuedRun(t.Context(), &rows[0])
	moved, ok := errors.AsType[*QueuedRunNamespaceMovedError](err)
	require.True(t, ok, "want a namespace-moved refusal, got %v", err)
	require.Equal(t, "quota-a", moved.QueueNamespace)
	require.Equal(t, "quota-b", moved.JobNamespace)
	require.Zero(t, countJobRuns(t, db, job.ID))

	legacy := rows[0]
	legacy.Namespace = nil
	started, err := store.StartQueuedRun(t.Context(), &legacy)
	require.NoError(t, err, "a row queued before the column existed is promoted as before")
	require.Equal(t, "quota-b", started.Namespace)
}

// TestDropQueuedRunResolvesItsIdempotentStart: a dropped row resolves the
// keyed start that waited on it as dropped, with the reason.
func TestDropQueuedRunResolvesItsIdempotentStart(t *testing.T) {
	db, store := openIdempotencyStore(t)
	useAccessPolicy(t, quotaPolicy)
	holder := createNamespacedJob(t, db, "quota-drop-holder", "quota-a", nil)
	_, err := store.Start(holder.ID, nil)
	require.NoError(t, err)
	job := createNamespacedJob(t, db, "quota-drop", "quota-a", nil)
	result, err := store.StartWithResult(t.Context(), job.ID, nil, WithStartIdempotencyKey("drop-key"))
	require.NoError(t, err)
	require.Equal(t, StartOutcomeQueued, result.Outcome)

	rows := quotaHeldRows(t, db, job.ID)
	require.Len(t, rows, 1)
	require.NoError(t, store.DropQueuedRun(t.Context(), &rows[0], QueueDropReasonNamespaceMoved))
	require.Empty(t, quotaHeldRows(t, db, job.ID))

	replayed, err := store.StartWithResult(t.Context(), job.ID, nil, WithStartIdempotencyKey("drop-key"))
	require.NoError(t, err)
	require.True(t, replayed.Replayed)
	require.Equal(t, StartOutcomeDropped, replayed.Outcome)
	require.Equal(t, QueueDropReasonNamespaceMoved, replayed.Reason)
}

// TestAnnotateQuotaHeldRunsMarksOnlyTheBlockedHead: the full-namespace fast
// path marks the rows a job with free slots would start now, once.
func TestAnnotateQuotaHeldRunsMarksOnlyTheBlockedHead(t *testing.T) {
	db, store := openIdempotencyStore(t)
	job := createNamespacedJob(t, db, "quota-annotate", "quota-a", &jobdefschema.Concurrency{MaxRuns: 2, Strategy: jobdefschema.ConcurrencyStrategyQueue})
	now := time.Now().UTC()
	ids := make([]uuid.UUID, 3)
	for i := range ids {
		ids[i] = uuid.New()
		require.NoError(t, db.Create(&models.RunQueue{ID: ids[i], JobID: job.ID, Priority: PriorityNormalValue, CreatedAt: now.Add(time.Duration(i) * time.Second)}).Error)
	}
	marked, err := store.AnnotateQuotaHeldRuns(t.Context(), job.ID, 2)
	require.NoError(t, err)
	require.Equal(t, int64(2), marked)
	again, err := store.AnnotateQuotaHeldRuns(t.Context(), job.ID, 2)
	require.NoError(t, err)
	require.Zero(t, again, "an already-marked row is not counted twice")
	rows := quotaHeldRows(t, db, job.ID)
	require.True(t, rows[0].IsQuotaHeld())
	require.True(t, rows[1].IsQuotaHeld())
	require.False(t, rows[2].IsQuotaHeld(), "a row beyond the job's own free slots waits on the job, not the namespace")
}

// TestNamespaceQuotaIssuesNoSQLWithoutAQuota: without a policy file, and for
// a namespace without a quota, admission runs no namespace count at all.
func TestNamespaceQuotaIssuesNoSQLWithoutAQuota(t *testing.T) {
	db, store := openIdempotencyStore(t)
	var mu sync.Mutex
	var statements []string
	require.NoError(t, db.Callback().Query().After("gorm:query").Register("test:capture-quota-sql", func(tx *gorm.DB) {
		mu.Lock()
		defer mu.Unlock()
		statements = append(statements, tx.Statement.SQL.String())
	}))
	namespaceCounts := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, stmt := range statements {
			if strings.Contains(stmt, "job_runs") && strings.Contains(stmt, "count(") && strings.Contains(stmt, "namespace") {
				n++
			}
		}
		statements = nil
		return n
	}
	free := createNamespacedJob(t, db, "quota-sql-free", "quota-b", nil)
	capped := createNamespacedJob(t, db, "quota-sql-capped", "quota-a", nil)

	policy.SetProcessHolder(nil)
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	_, err := store.Start(capped.ID, nil)
	require.NoError(t, err)
	require.Zero(t, namespaceCounts(), "no policy file: no namespace count")

	useAccessPolicy(t, quotaPolicy)
	_, err = store.Start(free.ID, nil)
	require.NoError(t, err)
	require.Zero(t, namespaceCounts(), "a namespace without a quota: no namespace count")

	_, err = store.Start(capped.ID, nil)
	require.ErrorIs(t, err, ErrRunQueuedByQuota)
	require.Positive(t, namespaceCounts(), "the capped namespace is counted")
}

func TestNamespaceQuotaLockSQLByDialect(t *testing.T) {
	for _, dialect := range []string{"dqlite", "sqlite", "sqlite3"} {
		stmt, err := namespaceQuotaLockSQL(dialect)
		require.NoError(t, err)
		require.Empty(t, stmt, dialect)
	}
	stmt, err := namespaceQuotaLockSQL("postgres")
	require.NoError(t, err)
	require.Equal(t, "SELECT pg_advisory_xact_lock(?)", stmt)
	_, err = namespaceQuotaLockSQL("mysql")
	require.Error(t, err, "an unknown dialect fails closed")
	require.Equal(t, namespaceQuotaLockKey("quota-a"), namespaceQuotaLockKey("quota-a"))
	require.NotEqual(t, namespaceQuotaLockKey("quota-a"), namespaceQuotaLockKey("quota-b"))
}

// TestNamespaceGateRefusesUntilTheConfiguredPolicyIsInstalled: a server
// configured with a policy file starts admitting workers before it installs
// the file. In that window the gate refuses every start and retry, writing
// nothing, rather than treating every namespace as unlimited; once the file
// is installed its quota applies. A process that never expects a file keeps
// the no-policy behaviour.
func TestNamespaceGateRefusesUntilTheConfiguredPolicyIsInstalled(t *testing.T) {
	db, store := openIdempotencyStore(t)
	job := createNamespacedJob(t, db, "quota-pending", "quota-a", nil)
	failed := seedNamespacedRun(t, db, job.ID, "quota-a", StatusFailed, nil)
	previous := policy.ProcessHolder()
	t.Cleanup(func() {
		ExpectNamespacePolicy(false)
		policy.SetProcessHolder(previous)
	})
	policy.SetProcessHolder(nil)

	ExpectNamespacePolicy(true)
	require.True(t, NamespacePolicyPending())
	_, err := store.Start(job.ID, nil)
	require.ErrorIs(t, err, ErrNamespacePolicyPending)
	_, err = store.RetryFromFailure(failed)
	require.ErrorIs(t, err, ErrNamespacePolicyPending)
	require.Equal(t, int64(1), countJobRuns(t, db, job.ID), "a refused start writes nothing")
	require.Empty(t, quotaHeldRows(t, db, job.ID))

	useAccessPolicy(t, quotaPolicy)
	require.False(t, NamespacePolicyPending())
	first, err := store.Start(job.ID, nil)
	require.NoError(t, err)
	require.NotNil(t, first)
	_, err = store.Start(job.ID, nil)
	require.ErrorIs(t, err, ErrRunQueuedByQuota, "the installed file's quota of one applies")

	ExpectNamespacePolicy(false)
	policy.SetProcessHolder(nil)
	require.False(t, NamespacePolicyPending())
	_, err = store.Start(job.ID, nil)
	require.NoError(t, err, "a process that expects no policy file is unlimited, as before")
}
