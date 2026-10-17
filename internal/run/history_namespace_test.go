package run

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// movedJobHistory creates a job whose first run was admitted in marketing and
// whose second, after the job moved to finance, in finance.
func movedJobHistory(t *testing.T) (*gorm.DB, *Store, *models.Job, *JobRun, *JobRun) {
	t.Helper()
	db, store := openIdempotencyStore(t)
	job := &models.Job{ID: uuid.New(), Alias: "history-moved-" + uuid.NewString()[:8], Namespace: "marketing"}
	require.NoError(t, db.Create(job).Error)
	old, err := store.Start(job.ID, nil)
	require.NoError(t, err)
	require.NoError(t, db.Model(job).Update("namespace", "finance").Error)
	newer, err := store.Start(job.ID, nil)
	require.NoError(t, err)
	// Make the order deterministic: the marketing run is the older one.
	earlier := time.Now().UTC().Add(-time.Hour)
	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", old.ID).
		Updates(map[string]any{"started_at": earlier, "created_at": earlier}).Error)
	require.Equal(t, "marketing", old.Namespace)
	require.Equal(t, "finance", newer.Namespace)
	return db, store, job, old, newer
}

func runIDs(runs []*JobRun) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	return ids
}

// TestHistoryReadsFilterByPersistedRunNamespace: under a history scope the run
// list (page, total, more) and the latest run of a moved job hold only runs
// whose persisted namespace the caller may read; without a scope they are
// exactly List and Latest (identity-and-access B4).
func TestHistoryReadsFilterByPersistedRunNamespace(t *testing.T) {
	_, store, job, old, newer := movedJobHistory(t)
	ctx := context.Background()

	plain, plainTotal, plainMore, err := store.List(job.ID, 10, 0)
	require.NoError(t, err)
	all, total, more, err := store.ListContext(ctx, job.ID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, runIDs(plain), runIDs(all), "unrestricted ListContext is List")
	require.Equal(t, plainTotal, total)
	require.Equal(t, plainMore, more)
	require.Equal(t, []uuid.UUID{newer.ID, old.ID}, runIDs(all))
	require.EqualValues(t, 2, total)

	finance := authz.WithAllowedNamespaces(ctx, []string{"finance"})
	runs, total, more, err := store.ListContext(finance, job.ID, 1, 0)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{newer.ID}, runIDs(runs))
	require.EqualValues(t, 1, total, "the total counts only visible runs")
	require.False(t, more, "no hidden run makes the page look incomplete")
	require.True(t, authz.Filtered(finance))

	marketing := authz.WithAllowedNamespaces(ctx, []string{"marketing"})
	runs, total, _, err = store.ListContext(marketing, job.ID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{old.ID}, runIDs(runs))
	require.EqualValues(t, 1, total)

	latest, err := store.LatestContext(finance, job.ID)
	require.NoError(t, err)
	require.Equal(t, newer.ID, latest.ID)
	latest, err = store.LatestContext(marketing, job.ID)
	require.NoError(t, err)
	require.Equal(t, old.ID, latest.ID, "the newest run the caller can see")
	_, err = store.LatestContext(authz.WithAllowedNamespaces(ctx, []string{"default"}), job.ID)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	latest, err = store.Latest(job.ID)
	require.NoError(t, err)
	require.Equal(t, newer.ID, latest.ID)
}

// TestRetryRefusesRunLeftInPreviousNamespace: every retry entry point (whole
// run, agent retry, one partition) refuses a run whose persisted namespace
// the job has left, inside its transaction, and changes nothing.
func TestRetryRefusesRunLeftInPreviousNamespace(t *testing.T) {
	db, store, job, old, _ := movedJobHistory(t)
	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", old.ID).Update("status", string(StatusFailed)).Error)

	_, err := store.RetryFromFailure(old.ID)
	require.ErrorIs(t, err, ErrRunNamespaceMoved)
	moved, ok := errors.AsType[*RunNamespaceMovedError](err)
	require.True(t, ok)
	require.Equal(t, RunNamespaceMovedError{RunID: old.ID, RunNamespace: "marketing", JobNamespace: "finance"}, *moved)

	_, err = store.RetryFromFailureAdmitted(old.ID)
	require.ErrorIs(t, err, ErrRunNamespaceMoved, "an incident agent's retry is refused too")

	_, _, err = store.RetryPartition(t.Context(), old.ID, uuid.New())
	require.ErrorIs(t, err, ErrRunNamespaceMoved)

	var persisted models.JobRun
	require.NoError(t, db.First(&persisted, "id = ?", old.ID).Error)
	require.Equal(t, string(StatusFailed), persisted.Status, "a refused retry changes nothing")

	// Moving the job back makes the run retryable again.
	require.NoError(t, db.Model(job).Update("namespace", "marketing").Error)
	_, err = store.RetryFromFailure(old.ID)
	require.NotErrorIs(t, err, ErrRunNamespaceMoved)

	require.NoError(t, CheckRunNamespace(old.ID, "", "default"), "a legacy empty namespace is default")
	require.Error(t, CheckRunNamespace(old.ID, "default", "finance"))
}

// TestWhyBaselineIgnoresUnreadableRuns: the why explainer never diffs a
// confined caller's run against a baseline run it cannot read, for the prior
// run, the cache origin and the cache-entry fallback alike.
func TestWhyBaselineIgnoresUnreadableRuns(t *testing.T) {
	db, store, job, old, newer := movedJobHistory(t)
	taskID := uuid.New()
	blob := datatypes.JSON(`{"v":1}`)
	started := time.Now().UTC().Add(-30 * time.Minute)
	taskRun := func(runID uuid.UUID, status TaskStatus, mutate func(*models.TaskRun)) *models.TaskRun {
		row := &models.TaskRun{ID: uuid.New(), JobRunID: runID, TaskID: taskID, AtomID: uuid.New(), Engine: "docker",
			Image: "busybox:1.36.1", Command: "true", Status: string(status), HashInputBlob: blob, Hash: "h-" + runID.String(), StartedAt: &started}
		if mutate != nil {
			mutate(row)
		}
		require.NoError(t, db.Create(row).Error)
		return row
	}
	taskRun(old.ID, TaskStatusSucceeded, nil)
	subject := taskRun(newer.ID, TaskStatusSucceeded, nil)
	newerRun, err := store.Get(newer.ID)
	require.NoError(t, err)

	ctx := context.Background()
	_, baseline, err := store.resolveBaseline(ctx, subject, job.ID, newerRun.StartedAt)
	require.NoError(t, err)
	require.Equal(t, "prior_run", baseline.Kind, "an unrestricted caller diffs across the move")
	require.Equal(t, old.ID, *baseline.RunID)

	finance := authz.WithAllowedNamespaces(ctx, []string{"finance"})
	_, baseline, err = store.resolveBaseline(finance, subject, job.ID, newerRun.StartedAt)
	require.NoError(t, err)
	require.Equal(t, "none", baseline.Kind, "the old owner's run is never a confined caller's baseline")
	both := authz.WithAllowedNamespaces(ctx, []string{"finance", "marketing"})
	_, baseline, err = store.resolveBaseline(both, subject, job.ID, newerRun.StartedAt)
	require.NoError(t, err)
	require.Equal(t, "prior_run", baseline.Kind)

	// A cache hit whose origin (and cache entry) sits in marketing.
	cached := taskRun(newer.ID, TaskStatusCached, func(row *models.TaskRun) {
		row.TaskID = uuid.New()
		row.CacheOriginRunID = &old.ID
		row.Hash = "cached-hash"
	})
	taskRun(old.ID, TaskStatusSucceeded, func(row *models.TaskRun) { row.TaskID = cached.TaskID; row.Hash = "cached-hash" })
	require.NoError(t, db.Create(&models.TaskCache{Hash: "cached-hash", JobID: job.ID, TaskName: "t", RunID: old.ID, TaskRunID: uuid.New(),
		Result: "succeeded", HashInputBlob: blob, CreatedAt: started}).Error)
	_, baseline, err = store.resolveBaseline(ctx, cached, job.ID, newerRun.StartedAt)
	require.NoError(t, err)
	require.Equal(t, "cache_origin", baseline.Kind)
	_, baseline, err = store.resolveBaseline(finance, cached, job.ID, newerRun.StartedAt)
	require.NoError(t, err)
	require.Equal(t, "none", baseline.Kind)
	require.Nil(t, baseline.RunID)
}

// TestOwnedBaselineCountsOnlyTheRunsNamespace: the live evaluator's baseline
// counts only samples of runs persisted in the evaluated run's namespace, so a
// moved job's old metrics never decide (or appear in) the new owner's verdict;
// a legacy empty run namespace counts as default, and the operator Baseline
// still sees every sample (identity-and-access B4).
func TestOwnedBaselineCountsOnlyTheRunsNamespace(t *testing.T) {
	db, _ := openIdempotencyStore(t)
	base := time.Now().UTC().Add(-24 * time.Hour)
	sample := func(namespace string, value float64, at time.Duration) {
		_, _, taskRunID, _ := seedTaskRun(t, db, string(TaskStatusSucceeded), false)
		var taskRun models.TaskRun
		require.NoError(t, db.First(&taskRun, "id = ?", taskRunID).Error)
		require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", taskRun.JobRunID).Update("namespace", namespace).Error)
		seedMetric(t, db, taskRunID, "warehouse/orders", "rowCount", value, base.Add(at))
	}
	sample("marketing", 10000, time.Minute)
	sample("marketing", 10000, 2*time.Minute)
	sample("finance", 10, 3*time.Minute)
	sample("", 500, 4*time.Minute)

	now := time.Now().UTC()
	finance, err := OwnedBaseline(context.Background(), db, "finance", "", "warehouse/orders", "rowCount", 20, now)
	require.NoError(t, err)
	require.Equal(t, 1, finance.Samples)
	require.InDelta(t, 10, finance.Median, 0.001, "marketing's samples never reach finance's baseline")
	marketing, err := OwnedBaseline(context.Background(), db, "marketing", "", "warehouse/orders", "rowCount", 20, now)
	require.NoError(t, err)
	require.Equal(t, 2, marketing.Samples)
	legacy, err := OwnedBaseline(context.Background(), db, "default", "", "warehouse/orders", "rowCount", 20, now)
	require.NoError(t, err)
	require.Equal(t, 1, legacy.Samples, "a legacy empty run namespace is default")
	empty, err := OwnedBaseline(context.Background(), db, "sales", "", "warehouse/orders", "rowCount", 20, now)
	require.NoError(t, err)
	require.Zero(t, empty.Samples, "no history in the run's namespace yields no baseline")
	all, err := Baseline(context.Background(), db, "", "warehouse/orders", "rowCount", 20, now)
	require.NoError(t, err)
	require.Equal(t, 4, all.Samples, "the operator baseline is unchanged")
	require.Equal(t, BaselineScopeNamespace, finance.Scope)
	require.Empty(t, all.Scope, "the operator baseline is not namespace-owned")

	namespace, err := persistedRunNamespace(context.Background(), db, uuid.New())
	require.ErrorIs(t, err, gorm.ErrRecordNotFound, "an unknown run has no namespace to scope a baseline by")
	require.Empty(t, namespace)

	// A verdict records whether its baseline was namespace-owned.
	spec := jobdef.AssertionSpec{DeltaFromBaseline: "50%"}
	owned := evaluateDelta("warehouse/orders", "rowCount", spec, 1000, finance, 0)
	require.NotNil(t, owned)
	require.Equal(t, BaselineScopeNamespace, owned.BaselineScope)
	unowned := evaluateDelta("warehouse/orders", "rowCount", spec, 1000, all, 0)
	require.NotNil(t, unowned)
	require.Empty(t, unowned.BaselineScope)
}

// TestWithholdUnownedBaselineEvidence: a confined reader's copy of a verdict
// recorded before baselines were namespace-owned loses its baseline median,
// sample count, derived bound and the message that renders them (also where
// the task's and the run's error quote it); a namespace-owned verdict and a
// verdict with no baseline pass unchanged. The JSON form, used for event
// payloads and lifecycle data, applies the same rule (identity-and-access B4).
func TestWithholdUnownedBaselineEvidence(t *testing.T) {
	median, bound, observed, ownMedian, ownBound, minimum := 10000.0, 5000.0, 10.0, 42.0, 21.0, 7.0
	legacyMessage := "observed 10 deviates 9990 from the baseline median 10000 (5 sample(s)), more than the declared 50%"
	legacy := DataViolation{Dataset: "d", Metric: "rowCount", Assertion: AssertionDeltaFromBaseline, Observed: &observed,
		Bound: &bound, DeltaFromBaseline: "50%", BaselineMedian: &median, BaselineSamples: 5, Message: legacyMessage}
	own := DataViolation{Dataset: "d", Metric: "rowCount", Assertion: AssertionDeltaFromBaseline, Observed: &observed,
		Bound: &ownBound, DeltaFromBaseline: "50%", BaselineMedian: &ownMedian, BaselineSamples: 6,
		BaselineScope: BaselineScopeNamespace, Message: "own baseline median 42"}
	absolute := DataViolation{Dataset: "d", Metric: "rowCount", Assertion: "min", Observed: &observed, Bound: &minimum, Message: "below the declared min 7"}
	run := &JobRun{
		Error: "task failed: " + legacyMessage,
		Tasks: []*TaskRun{{DataViolations: []DataViolation{legacy, own, absolute}, Error: "violates: " + legacyMessage}},
	}
	encoded, err := json.Marshal(run)
	require.NoError(t, err)

	run.WithholdUnownedBaselineEvidence()
	got := run.Tasks[0].DataViolations
	require.Nil(t, got[0].BaselineMedian)
	require.Zero(t, got[0].BaselineSamples)
	require.Nil(t, got[0].Bound, "a delta bound is derived from the baseline median")
	require.Equal(t, BaselineEvidenceWithheld, got[0].Message)
	require.Equal(t, observed, *got[0].Observed, "the run's own observation is kept")
	require.Equal(t, own, got[1], "a namespace-owned verdict is unchanged")
	require.Equal(t, absolute, got[2], "a verdict without a baseline is unchanged")
	require.Equal(t, "violates: "+BaselineEvidenceWithheld, run.Tasks[0].Error)
	require.Equal(t, "task failed: "+BaselineEvidenceWithheld, run.Error)
	(*JobRun)(nil).WithholdUnownedBaselineEvidence()

	var payload map[string]any
	require.NoError(t, json.Unmarshal(encoded, &payload))
	WithholdUnownedBaselineEvidenceJSON(payload)
	redacted, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NotContains(t, string(redacted), "10000", "neither the median nor the message that renders it remains")
	require.NotContains(t, string(redacted), "5000")
	require.Contains(t, string(redacted), BaselineEvidenceWithheld)
	require.Contains(t, string(redacted), "own baseline median 42")
	require.Contains(t, string(redacted), `"baseline_scope":"namespace"`)

	task := map[string]any{"data_violations": "not-a-list", "error": "x"}
	WithholdUnownedBaselineEvidenceJSON(task)
	require.NotContains(t, task, "data_violations", "a malformed verdict list is dropped")

	// A task error or an incident last_error quotes the verdict messages and
	// carries the verdict list separately. The quoted form withholds the same
	// messages, and a payload that is not a verdict list withholds nothing.
	raw, err := json.Marshal([]DataViolation{legacy, own, absolute})
	require.NoError(t, err)
	quoted := "prefix " + legacyMessage + " tail " + own.Message + " and " + absolute.Message
	gotText := WithholdQuotedBaselineEvidence(quoted, raw)
	require.NotContains(t, gotText, "10000")
	require.NotContains(t, gotText, "9990")
	require.Contains(t, gotText, BaselineEvidenceWithheld)
	require.Contains(t, gotText, own.Message, "a namespace-owned message stays quoted")
	require.Contains(t, gotText, absolute.Message, "a verdict without a baseline stays quoted")
	require.Equal(t, quoted, WithholdQuotedBaselineEvidence(quoted, []byte("not-json")))
	require.Equal(t, quoted, WithholdQuotedBaselineEvidence(quoted, nil))
	require.Empty(t, WithholdQuotedBaselineEvidence("", raw))
}
