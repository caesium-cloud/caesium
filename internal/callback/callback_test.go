package callback

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/jsonutil"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestDispatchNotificationSuccess(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	trig := &models.Trigger{
		ID:            uuid.New(),
		Alias:         "demo",
		Type:          models.TriggerTypeCron,
		Configuration: "{}",
	}
	require.NoError(t, db.Create(trig).Error)

	job := &models.Job{
		ID:        uuid.New(),
		Alias:     "demo-job",
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
	runEntry, err := store.Start(job.ID)
	require.NoError(t, err)

	require.NoError(t, store.RegisterTask(runEntry.ID, task, atom, 0))
	require.NoError(t, store.StartTask(runEntry.ID, task.ID, "runtime-1"))
	require.NoError(t, store.CompleteTask(runEntry.ID, task.ID, "success"))
	require.NoError(t, store.Complete(runEntry.ID, nil))

	var received atomic.Bool
	var receivedMeta Metadata

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		received.Store(true)
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))

		if err := json.NewDecoder(r.Body).Decode(&receivedMeta); err != nil {
			t.Fatalf("decode payload: %v", err)
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

	dispatcher := NewDispatcher(db)
	dispatcher.WithHTTPClient(server.Client())

	err = dispatcher.Dispatch(context.Background(), job.ID, runEntry.ID, nil)
	require.NoError(t, err)

	require.True(t, received.Load(), "callback should have been sent")
	require.Equal(t, job.Alias, receivedMeta.JobAlias)
	require.Equal(t, run.StatusSucceeded, run.Status(receivedMeta.Status))
	require.Len(t, receivedMeta.Tasks, 1)
	require.Equal(t, "runtime-1", receivedMeta.Tasks[0].RuntimeID)

	var callbackRuns []models.CallbackRun
	require.NoError(t, db.Where("job_run_id = ?", runEntry.ID).Find(&callbackRuns).Error)
	require.Len(t, callbackRuns, 1)
	require.Equal(t, models.CallbackRunStatusSucceeded, callbackRuns[0].Status)
	require.NotNil(t, callbackRuns[0].CompletedAt)
}

func TestDispatchMissingHandler(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	job := &models.Job{
		ID:        uuid.New(),
		Alias:     "demo-job",
		TriggerID: uuid.New(),
	}
	require.NoError(t, db.Create(job).Error)

	store := run.NewStore(db)
	runEntry, err := store.Start(job.ID)
	require.NoError(t, err)
	require.NoError(t, store.Complete(runEntry.ID, nil))

	cb := &models.Callback{
		ID:            uuid.New(),
		JobID:         job.ID,
		Type:          models.CallbackType("custom"),
		Configuration: `{}`,
	}
	require.NoError(t, db.Create(cb).Error)

	dispatcher := NewDispatcher(db)
	dispatcher.timeout = time.Second

	err = dispatcher.Dispatch(context.Background(), job.ID, runEntry.ID, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no handler registered for callback type")
}

func TestRetryFailedCallbacks(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	trig := &models.Trigger{
		ID:            uuid.New(),
		Alias:         "demo",
		Type:          models.TriggerTypeCron,
		Configuration: "{}",
	}
	require.NoError(t, db.Create(trig).Error)

	job := &models.Job{
		ID:        uuid.New(),
		Alias:     "demo-job",
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
	runEntry, err := store.Start(job.ID)
	require.NoError(t, err)

	require.NoError(t, store.RegisterTask(runEntry.ID, task, atom, 0))
	require.NoError(t, store.StartTask(runEntry.ID, task.ID, "runtime-1"))
	require.NoError(t, store.CompleteTask(runEntry.ID, task.ID, "success"))
	require.NoError(t, store.Complete(runEntry.ID, nil))

	var attempt int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		if atomic.AddInt32(&attempt, 1) == 1 {
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

	dispatcher := NewDispatcher(db)
	dispatcher.WithHTTPClient(server.Client())

	err = dispatcher.Dispatch(context.Background(), job.ID, runEntry.ID, nil)
	require.Error(t, err)

	var callbackRuns []models.CallbackRun
	require.NoError(t, db.Where("job_run_id = ?", runEntry.ID).Order("started_at asc").Find(&callbackRuns).Error)
	require.Len(t, callbackRuns, 1)
	require.Equal(t, models.CallbackRunStatusFailed, callbackRuns[0].Status)
	require.NotEmpty(t, callbackRuns[0].Error)

	err = dispatcher.RetryFailed(context.Background(), runEntry.ID)
	require.NoError(t, err)

	callbackRuns = nil
	require.NoError(t, db.Where("job_run_id = ?", runEntry.ID).Order("started_at asc").Find(&callbackRuns).Error)
	require.Len(t, callbackRuns, 2)
	require.Equal(t, models.CallbackRunStatusFailed, callbackRuns[0].Status)
	require.Equal(t, models.CallbackRunStatusSucceeded, callbackRuns[1].Status)
	require.Empty(t, callbackRuns[1].Error)
	require.NotNil(t, callbackRuns[1].CompletedAt)
}
