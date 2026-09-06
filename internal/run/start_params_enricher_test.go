package run

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
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
	registerStartParamsEnricher(t, func(_ context.Context, _ *gorm.DB, id uuid.UUID, params map[string]string) (map[string]string, error) {
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
	registerStartParamsEnricher(t, func(ctx context.Context, handle *gorm.DB, id uuid.UUID, params map[string]string) (map[string]string, error) {
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

	registerStartParamsEnricher(t, func(_ context.Context, _ *gorm.DB, _ uuid.UUID, _ map[string]string) (map[string]string, error) {
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

	registerStartParamsEnricher(t, func(_ context.Context, _ *gorm.DB, _ uuid.UUID, _ map[string]string) (map[string]string, error) {
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
