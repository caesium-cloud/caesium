package run

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func registerStartParamsFence(t *testing.T, fn StartParamsFence) {
	t.Helper()
	SetStartParamsFence(fn)
	t.Cleanup(func() { SetStartParamsFence(nil) })
}

// TestStartParamsFenceBindsParamsToTheFrozenNamespace: the fence runs inside
// admission with the namespace admission froze (after a move that landed
// after enrichment) and what it returns is what is persisted, on the run row
// and on the run_queue row of a queued start.
func TestStartParamsFenceBindsParamsToTheFrozenNamespace(t *testing.T) {
	db, store := openIdempotencyStore(t)
	plain := &models.Job{ID: uuid.New(), Alias: "fence-plain", Namespace: "finance"}
	require.NoError(t, db.Create(plain).Error)
	limited := createConcurrencyJob(t, db, "fence-limited", jobdef.ConcurrencyStrategyQueue, 1)
	require.NoError(t, db.Model(limited).Update("namespace", "finance").Error)

	registerStartParamsEnricher(t, func(_ context.Context, conn *gorm.DB, jobID uuid.UUID, params map[string]string, _ bool) (map[string]string, error) {
		// The view is read in finance; the job moves before admission.
		require.NoError(t, conn.Model(&models.Job{}).Where("id = ?", jobID).Update("namespace", "marketing").Error)
		return map[string]string{"caller": params["caller"], "view": "finance"}, nil
	})
	var seen []string
	registerStartParamsFence(t, func(_ context.Context, tx *gorm.DB, _ uuid.UUID, namespace string, params map[string]string) (map[string]string, error) {
		require.NotNil(t, tx)
		seen = append(seen, namespace)
		if params["view"] != namespace {
			delete(params, "view")
		}
		return params, nil
	})

	started, err := store.Start(plain.ID, nil, WithStartParams(map[string]string{"caller": "c"}))
	require.NoError(t, err)
	require.Equal(t, "marketing", started.Namespace)
	require.Equal(t, map[string]string{"caller": "c"}, runParams(t, db, started.ID))
	require.Equal(t, map[string]string{"caller": "c"}, started.Params)

	// Fill the one slot, then queue: the run_queue row is fenced too.
	_, err = store.Start(limited.ID, nil, WithStartParams(map[string]string{"caller": "first"}))
	require.NoError(t, err)
	_, err = store.Start(limited.ID, nil, WithStartParams(map[string]string{"caller": "queued"}))
	require.ErrorIs(t, err, ErrRunQueued)
	var queued models.RunQueue
	require.NoError(t, db.Where("job_id = ?", limited.ID).First(&queued).Error)
	var queuedParams map[string]string
	require.NoError(t, json.Unmarshal(queued.Params, &queuedParams))
	require.Equal(t, map[string]string{"caller": "queued"}, queuedParams)
	require.Equal(t, []string{"marketing", "marketing", "marketing"}, seen)
}

// TestStartParamsFenceFailureKeepsWhatItCanStandBehind: a failing fence does
// not fail the start; a nil result keeps the enriched params, a returned map
// replaces them.
func TestStartParamsFenceFailureKeepsWhatItCanStandBehind(t *testing.T) {
	db, store := openIdempotencyStore(t)
	job := &models.Job{ID: uuid.New(), Alias: "fence-fail", Namespace: "finance"}
	require.NoError(t, db.Create(job).Error)
	registerStartParamsEnricher(t, func(_ context.Context, _ *gorm.DB, _ uuid.UUID, params map[string]string, _ bool) (map[string]string, error) {
		return map[string]string{"caller": params["caller"], "view": "v"}, nil
	})
	failed := errors.New("fence read failed")

	registerStartParamsFence(t, func(context.Context, *gorm.DB, uuid.UUID, string, map[string]string) (map[string]string, error) {
		return nil, failed
	})
	kept, err := store.Start(job.ID, nil, WithStartParams(map[string]string{"caller": "c"}))
	require.NoError(t, err)
	require.Equal(t, map[string]string{"caller": "c", "view": "v"}, runParams(t, db, kept.ID))

	registerStartParamsFence(t, func(_ context.Context, _ *gorm.DB, _ uuid.UUID, _ string, params map[string]string) (map[string]string, error) {
		delete(params, "view")
		return params, failed
	})
	stripped, err := store.Start(job.ID, nil, WithStartParams(map[string]string{"caller": "c"}))
	require.NoError(t, err)
	require.Equal(t, map[string]string{"caller": "c"}, runParams(t, db, stripped.ID))
}
