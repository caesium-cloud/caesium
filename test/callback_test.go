//go:build integration

package test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/caesium-cloud/caesium/internal/callback"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/jsonutil"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These tests exercise the callback dispatcher end-to-end with a real DB (sqlite)
// to align with integration expectations while avoiding external engines.

func TestIntegrationCallbackDispatchAndRetry(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	trig := &models.Trigger{
		ID:            uuid.New(),
		Alias:         "integration-cb",
		Type:          models.TriggerTypeCron,
		Configuration: "{}",
	}
	require.NoError(t, db.Create(trig).Error)

	job := &models.Job{
		ID:        uuid.New(),
		Alias:     "integration-callback-job",
		TriggerID: trig.ID,
	}
	require.NoError(t, db.Create(job).Error)

	atom := &models.Atom{
		ID:     uuid.New(),
		Engine: models.AtomEngineDocker,
		Image:  "alpine:3",
	}
	cmd, err := jsonutil.MarshalSliceString([]string{"echo", "hello"})
	require.NoError(t, err)
	atom.Command = cmd
	require.NoError(t, db.Create(atom).Error)

	task := &models.Task{
		ID:     uuid.New(),
		JobID:  job.ID,
		AtomID: atom.ID,
	}
	require.NoError(t, db.Create(task).Error)

	store := run.NewStore(db)
	runEntry, err := store.Start(job.ID, nil)
	require.NoError(t, err)
	require.NoError(t, store.RegisterTask(runEntry.ID, task, atom, 0))
	require.NoError(t, store.StartTask(runEntry.ID, task.ID, "runtime-1"))
	require.NoError(t, store.CompleteTask(runEntry.ID, task.ID, "success"))
	require.NoError(t, store.Complete(runEntry.ID, nil))

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if attempts.Add(1) == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cb := &models.Callback{
		ID:            uuid.New(),
		JobID:         job.ID,
		Type:          models.CallbackTypeNotification,
		Configuration: `{"url":"` + server.URL + `"}`,
	}
	require.NoError(t, db.Create(cb).Error)

	dispatcher := callback.NewDispatcher(db)
	dispatcher.WithHTTPClient(server.Client())

	// First attempt should record failure.
	err = dispatcher.Dispatch(context.Background(), job.ID, runEntry.ID, nil)
	require.Error(t, err)

	var callbackRuns []models.CallbackRun
	require.NoError(t, db.Where("job_run_id = ?", runEntry.ID).Order("started_at asc").Find(&callbackRuns).Error)
	require.Len(t, callbackRuns, 1)
	require.Equal(t, models.CallbackRunStatusFailed, callbackRuns[0].Status)

	// Retry should succeed and create another run record.
	err = dispatcher.RetryFailed(context.Background(), runEntry.ID)
	require.NoError(t, err)

	callbackRuns = nil
	require.NoError(t, db.Where("job_run_id = ?", runEntry.ID).Order("started_at asc").Find(&callbackRuns).Error)
	require.Len(t, callbackRuns, 2)
	require.Equal(t, models.CallbackRunStatusFailed, callbackRuns[0].Status)
	require.Equal(t, models.CallbackRunStatusSucceeded, callbackRuns[1].Status)
}
