package run

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// registerStartParamsEnricher installs an enricher for one test and clears it
// again, so the process-wide registration cannot leak into another test.
func registerStartParamsEnricher(t *testing.T, fn StartParamsEnricher) {
	t.Helper()
	SetStartParamsEnricher(fn)
	t.Cleanup(func() { SetStartParamsEnricher(nil) })
}

// runParams reads back the params actually persisted on the job_runs row — the
// point of the seam is that they are written WITH the row, not observed after it.
func runParams(t *testing.T, db *gorm.DB, runID uuid.UUID) map[string]string {
	t.Helper()
	var row models.JobRun
	require.NoError(t, db.Select("params").First(&row, "id = ?", runID).Error)
	if len(row.Params) == 0 {
		return nil
	}
	var params map[string]string
	require.NoError(t, json.Unmarshal(row.Params, &params))
	return params
}

func TestStartRunPersistsEnrichedParams(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	jobID := uuid.New()
	var sawJobID uuid.UUID
	registerStartParamsEnricher(t, func(_ context.Context, _ *gorm.DB, id uuid.UUID, params map[string]string, _ bool) (map[string]string, error) {
		sawJobID = id
		out := map[string]string{"_consumed_watermarks": `{"raw.vendor_x":"vendor-key-1"}`}
		for k, v := range params {
			out[k] = v
		}
		return out, nil
	})

	store := NewStore(db)
	runRecord, err := store.Start(jobID, nil, WithStartParams(map[string]string{"logical_date": "2026-06-25"}))
	require.NoError(t, err)
	require.NotNil(t, runRecord)
	require.Equal(t, jobID, sawJobID, "the enricher must be told which job the run is for")

	params := runParams(t, db, runRecord.ID)
	require.Equal(t, `{"raw.vendor_x":"vendor-key-1"}`, params["_consumed_watermarks"],
		"the enriched param must be written with the run row, not after it")
	require.Equal(t, "2026-06-25", params["logical_date"], "caller params must survive enrichment")
}

// TestStartRunEnricherReadsTheStoresOwnHandle is the regression for a deadlock:
// internal/trigger/event/router.go creates runs through a store built over its
// OPEN transaction, and an enricher that reads any other connection underneath
// that transaction wedges the database until the request's context is cancelled.
// The handle passed in must therefore be the store's own — proven here by
// reading a row that exists only inside the uncommitted transaction.
func TestStartRunEnricherReadsTheStoresOwnHandle(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	jobID := uuid.New()
	registerStartParamsEnricher(t, func(ctx context.Context, handle *gorm.DB, id uuid.UUID, params map[string]string, _ bool) (map[string]string, error) {
		var job models.Job
		if err := handle.WithContext(ctx).First(&job, "id = ?", id).Error; err != nil {
			return params, err
		}
		return map[string]string{"job_alias": job.Alias}, nil
	})

	var runID uuid.UUID
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		if err := tx.Create(&models.Job{
			ID: jobID, Alias: "uncommitted-job", CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		runRecord, err := NewStore(tx).Start(jobID, nil)
		if err != nil {
			return err
		}
		runID = runRecord.ID
		return nil
	}))

	require.Equal(t, "uncommitted-job", runParams(t, db, runID)["job_alias"],
		"the enricher must read the store's handle, not a second connection")
}

// TestStartRunEnrichesRunsWithoutCallerParams covers the ordinary cron/HTTP
// trigger: no params of its own, so the row would carry none at all unless the
// enricher's output is what gets marshalled.
func TestStartRunEnrichesRunsWithoutCallerParams(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	registerStartParamsEnricher(t, func(_ context.Context, _ *gorm.DB, _ uuid.UUID, _ map[string]string, _ bool) (map[string]string, error) {
		return map[string]string{"_consumed_watermarks": `{}`}, nil
	})

	store := NewStore(db)
	runRecord, err := store.Start(uuid.New(), nil)
	require.NoError(t, err)
	require.NotNil(t, runRecord)

	require.Equal(t, `{}`, runParams(t, db, runRecord.ID)["_consumed_watermarks"])
}

// TestStartRunSurvivesEnricherFailure pins the degrade: an optional subsystem's
// read failing must not stop a run from starting.
func TestStartRunSurvivesEnricherFailure(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	registerStartParamsEnricher(t, func(_ context.Context, _ *gorm.DB, _ uuid.UUID, _ map[string]string, _ bool) (map[string]string, error) {
		return nil, errors.New("catalog read failed")
	})

	store := NewStore(db)
	runRecord, err := store.Start(uuid.New(), nil, WithStartParams(map[string]string{"logical_date": "2026-06-25"}))
	require.NoError(t, err)
	require.NotNil(t, runRecord)

	params := runParams(t, db, runRecord.ID)
	require.Equal(t, "2026-06-25", params["logical_date"], "a failed enricher must fall back to the caller's params")
	require.NotContains(t, params, "_consumed_watermarks")
}

// TestStartQueuedRunRefreshesEnrichedParams drives the whole queue path — a run
// admitted into run_queue behind a busy slot, then promoted once the slot frees
// — because a queue-strategy run is enriched TWICE and only the second call
// happens at the moment the run really begins. The enricher is told which call
// it is (fromQueue), and the promoted row must carry the value it took then, not
// the one that rode the run_queue row.
func TestStartQueuedRunRefreshesEnrichedParams(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	job := createConcurrencyJob(t, db, "queue-refresh", jobdef.ConcurrencyStrategyQueue, 1)

	// Stands in for a watermark the enricher observes: it moves while the run
	// waits in the queue.
	view := "admission-key"
	var sawFromQueue []bool
	registerStartParamsEnricher(t, func(_ context.Context, _ *gorm.DB, _ uuid.UUID, params map[string]string, fromQueue bool) (map[string]string, error) {
		sawFromQueue = append(sawFromQueue, fromQueue)
		out := map[string]string{}
		for k, v := range params {
			out[k] = v
		}
		// The real enricher's rule: the value is a point-in-time observation, so
		// it is re-taken on every creation of the run — including the promotion,
		// which is the one that happens when the run truly begins. It owns this
		// key alone and never rewrites a caller's.
		out["_consumed_watermarks_start"] = view
		return out, nil
	})

	occupying, err := store.Start(job.ID, nil)
	require.NoError(t, err)
	require.NotNil(t, occupying)

	_, err = store.Start(job.ID, nil, WithStartParams(map[string]string{"_consumed_watermarks": "derivation-view"}))
	require.ErrorIs(t, err, ErrRunQueued)

	var queuedRow models.RunQueue
	require.NoError(t, db.First(&queuedRow, "job_id = ?", job.ID).Error)
	var queuedParams map[string]string
	require.NoError(t, json.Unmarshal(queuedRow.Params, &queuedParams))
	require.Equal(t, "admission-key", queuedParams["_consumed_watermarks_start"],
		"the enqueued row should carry the view taken at admission")

	// The input advances while the run waits, and the slot frees.
	view = "promotion-key"
	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", occupying.ID).
		Update("status", string(StatusSucceeded)).Error)

	claimed, err := store.DequeueNextRun(context.Background(), job.ID, "claim-a")
	require.NoError(t, err)
	require.NotNil(t, claimed)

	promoted, err := store.StartQueuedRun(context.Background(), claimed)
	require.NoError(t, err)
	require.NotNil(t, promoted)

	require.Equal(t, []bool{false, false, true}, sawFromQueue,
		"only the promotion out of run_queue is a fromQueue creation")
	promotedParams := runParams(t, db, promoted.ID)
	require.Equal(t, "promotion-key", promotedParams["_consumed_watermarks_start"],
		"a promoted run must record the view it started with, not the one it was queued with")
	require.Equal(t, "derivation-view", promotedParams["_consumed_watermarks"],
		"the refresh must not touch a param the caller owns")
}

// TestStartQueuedRunAppliesEnricherRetraction is the regression for a stale
// snapshot surviving a failed refresh. A promotion's params come off the
// run_queue row already carrying what the enricher wrote at admission, so when
// its re-read fails, dropping its returned map would persist that admission-time
// value onto a run starting now — recorded as if it were the view the run began
// with. The map returned alongside the error is the enricher's retraction and
// must be the one written to the row; the run still starts either way.
func TestStartQueuedRunAppliesEnricherRetraction(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	job := createConcurrencyJob(t, db, "queue-retract", jobdef.ConcurrencyStrategyQueue, 1)

	registerStartParamsEnricher(t, func(_ context.Context, _ *gorm.DB, _ uuid.UUID, params map[string]string, fromQueue bool) (map[string]string, error) {
		out := map[string]string{}
		for k, v := range params {
			out[k] = v
		}
		if fromQueue {
			// The re-read failed: retract the value taken at admission rather
			// than let it stand in for the one this run actually started on.
			delete(out, "_consumed_watermarks_start")
			return out, errors.New("watermark read failed")
		}
		out["_consumed_watermarks_start"] = "admission-key"
		return out, nil
	})

	occupying, err := store.Start(job.ID, nil)
	require.NoError(t, err)
	require.NotNil(t, occupying)

	_, err = store.Start(job.ID, nil, WithStartParams(map[string]string{"logical_date": "2026-06-25"}))
	require.ErrorIs(t, err, ErrRunQueued)

	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", occupying.ID).
		Update("status", string(StatusSucceeded)).Error)

	claimed, err := store.DequeueNextRun(context.Background(), job.ID, "claim-a")
	require.NoError(t, err)
	require.NotNil(t, claimed)

	promoted, err := store.StartQueuedRun(context.Background(), claimed)
	require.NoError(t, err, "a failed enricher must not stop the run from starting")
	require.NotNil(t, promoted)

	params := runParams(t, db, promoted.ID)
	require.NotContains(t, params, "_consumed_watermarks_start",
		"the stale admission-time view must not survive a failed refresh")
	require.Equal(t, "2026-06-25", params["logical_date"], "the caller's params must survive the retraction")
}

// TestStartRunWithoutEnricherIsUnchanged pins the nil-safe default: with nothing
// registered, run creation writes exactly what the caller passed.
func TestStartRunWithoutEnricherIsUnchanged(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	runRecord, err := store.Start(uuid.New(), nil, WithStartParams(map[string]string{"logical_date": "2026-06-25"}))
	require.NoError(t, err)
	require.NotNil(t, runRecord)

	require.Equal(t, map[string]string{"logical_date": "2026-06-25"}, runParams(t, db, runRecord.ID))
}
