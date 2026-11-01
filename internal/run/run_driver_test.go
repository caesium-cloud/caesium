package run

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func driverTestStore(t *testing.T) (*Store, *gorm.DB) {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	return NewStore(db), db
}

// ageHeartbeat rewinds a run's heartbeat, or clears it for a nil at.
func ageHeartbeat(t *testing.T, db *gorm.DB, runID uuid.UUID, at *time.Time) {
	t.Helper()
	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", runID).
		UpdateColumn("driver_heartbeat_at", at).Error)
}

// runWithTaskRows starts a run holding one task row per status.
func runWithTaskRows(t *testing.T, store *Store, db *gorm.DB, statuses ...TaskStatus) uuid.UUID {
	t.Helper()
	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "driver-" + jobID.String()}).Error)
	started, err := store.Start(jobID, nil)
	require.NoError(t, err)
	for _, status := range statuses {
		require.NoError(t, db.Create(&models.TaskRun{ID: uuid.New(), JobRunID: started.ID, TaskID: uuid.New(),
			Engine: models.AtomEngineDocker, Status: string(status)}).Error)
	}
	return started.ID
}

func readHeartbeat(t *testing.T, db *gorm.DB, runID uuid.UUID) *time.Time {
	t.Helper()
	var row models.JobRun
	require.NoError(t, db.Select("driver_heartbeat_at").First(&row, "id = ?", runID).Error)
	return row.DriverHeartbeatAt
}

func TestAdmissionStampsTheDriverHeartbeat(t *testing.T) {
	store, db := driverTestStore(t)
	started, err := store.Start(uuid.New(), nil)
	require.NoError(t, err)
	require.NotNil(t, readHeartbeat(t, db, started.ID), "a fresh run must never read as having no driver")

	// The conditional INSERT a concurrency policy goes through is raw SQL, so
	// it cannot inherit the model's stamp by accident.
	job := createConcurrencyJob(t, db, "driver-policy", jobdef.ConcurrencyStrategyQueue, 1)
	admitted, err := store.Start(job.ID, nil)
	require.NoError(t, err)
	require.NotNil(t, readHeartbeat(t, db, admitted.ID), "policy admission must stamp the heartbeat too")
}

func TestRetryReadmissionStampsTheDriverHeartbeat(t *testing.T) {
	longAgo := time.Now().UTC().Add(-time.Hour)
	for name, admit := range map[string]bool{"unconditional": false, "conditional": true} {
		t.Run(name, func(t *testing.T) {
			store, db := driverTestStore(t)
			job := createConcurrencyJob(t, db, "driver-retry", jobdef.ConcurrencyStrategyQueue, 1)
			runID := seedFailedRun(t, db, job.ID)
			ageHeartbeat(t, db, runID, &longAgo)

			var row models.JobRun
			require.NoError(t, db.First(&row, "id = ?", runID).Error)
			require.NoError(t, store.readmitRetryTx(db, &row, admit))

			require.True(t, readHeartbeat(t, db, runID).After(longAgo),
				"a reopened run must not read as lapsed before its new driver's first heartbeat")
		})
	}
}

func TestRunHeartbeatsListsWhatRecoveryMayWatch(t *testing.T) {
	store, db := driverTestStore(t)
	start := func() uuid.UUID {
		started, err := store.Start(uuid.New(), nil)
		require.NoError(t, err)
		return started.ID
	}
	live := start()
	preColumn := start()
	ageHeartbeat(t, db, preColumn, nil)
	terminal := start()
	require.NoError(t, store.Complete(terminal, nil))
	quarantined := start()
	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", quarantined).Update("quarantine", true).Error)

	got, err := store.RunHeartbeats(context.Background())
	require.NoError(t, err)
	byID := map[uuid.UUID]*time.Time{}
	for _, r := range got {
		byID[r.ID] = r.Heartbeat
	}
	require.Len(t, byID, 2)
	require.NotNil(t, byID[live])
	require.Contains(t, byID, preColumn)
	require.Nil(t, byID[preColumn], "a run from before the column reads as no heartbeat at all")
}

// Several nodes sweep the same database: exactly one swap may land, on the value
// as it really reads back, which is what makes this more than a test of Equal.
func TestReplaceRunHeartbeatHasOneWinnerAcrossSweepers(t *testing.T) {
	for name, clear := range map[string]bool{"stamped": false, "null": true} {
		t.Run(name, func(t *testing.T) {
			store, db := driverTestStore(t)
			started, err := store.Start(uuid.New(), nil)
			require.NoError(t, err)
			if clear {
				ageHeartbeat(t, db, started.ID, nil)
			}
			runs, err := store.RunHeartbeats(context.Background())
			require.NoError(t, err)
			require.Len(t, runs, 1)

			var winners, failures atomic.Int32
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					replaced, swapErr := store.ReplaceRunHeartbeat(context.Background(), started.ID, runs[0].Heartbeat)
					if swapErr != nil {
						failures.Add(1)
					}
					if replaced != nil {
						winners.Add(1)
					}
				})
			}
			wg.Wait()
			require.Zero(t, failures.Load())
			require.Equal(t, int32(1), winners.Load())
		})
	}
}

// The #711 finding: a renewal stamped before it queued on the writer commits
// after the sweep read, carrying a value older than anything a cutoff would
// spare. It committed first, so it wins: the value changed, whatever it says.
func TestAnyRenewalBeatsASwapOnTheObservedValue(t *testing.T) {
	store, db := driverTestStore(t)
	started, err := store.Start(uuid.New(), nil)
	require.NoError(t, err)
	observed := readHeartbeat(t, db, started.ID)

	stampedLongAgo := observed.Add(-time.Hour)
	ageHeartbeat(t, db, started.ID, &stampedLongAgo)

	replaced, err := store.ReplaceRunHeartbeat(context.Background(), started.ID, observed)
	require.NoError(t, err)
	require.Nil(t, replaced, "a swap overrode a renewal that committed after the observation")
	finalized, err := store.FinalizeLostRun(started.ID, observed)
	require.NoError(t, err)
	require.False(t, finalized, "a healthy run was failed")
	got, err := store.Get(started.ID)
	require.NoError(t, err)
	require.Equal(t, StatusRunning, got.Status)
}

func TestFinalizeLostRunSettlesARunStillHoldingTheValue(t *testing.T) {
	store, db := driverTestStore(t)
	runID := runWithTaskRows(t, store, db, TaskStatusRunning)

	finalized, err := store.FinalizeLostRun(runID, readHeartbeat(t, db, runID))
	require.NoError(t, err)
	require.True(t, finalized)
	again, err := store.FinalizeLostRun(runID, readHeartbeat(t, db, runID))
	require.NoError(t, err)
	require.False(t, again, "a run is finalized once")

	lost, err := store.RunDriverLost(context.Background(), runID)
	require.NoError(t, err)
	require.True(t, lost)
}

func TestRunDriverLostIgnoresOrdinaryFailures(t *testing.T) {
	store, _ := driverTestStore(t)
	started, err := store.Start(uuid.New(), nil)
	require.NoError(t, err)
	require.NoError(t, store.Complete(started.ID, errors.New("task failed")))

	lost, err := store.RunDriverLost(context.Background(), started.ID)
	require.NoError(t, err)
	require.False(t, lost)
}

func TestRenewRunDriversLeavesOtherRunsAlone(t *testing.T) {
	store, db := driverTestStore(t)
	stale := time.Now().UTC().Add(-time.Hour)
	renewed, err := store.Start(uuid.New(), nil)
	require.NoError(t, err)
	other, err := store.Start(uuid.New(), nil)
	require.NoError(t, err)
	finished, err := store.Start(uuid.New(), nil)
	require.NoError(t, err)
	for _, id := range []uuid.UUID{renewed.ID, other.ID, finished.ID} {
		ageHeartbeat(t, db, id, &stale)
	}
	require.NoError(t, store.Complete(finished.ID, nil))

	var before models.JobRun
	require.NoError(t, db.Select("updated_at").First(&before, "id = ?", renewed.ID).Error)
	require.NoError(t, store.RenewRunDrivers(context.Background(), []uuid.UUID{renewed.ID, finished.ID}))

	require.True(t, readHeartbeat(t, db, renewed.ID).After(stale))
	require.True(t, readHeartbeat(t, db, other.ID).Equal(stale), "only this node's runs are its to renew")
	require.True(t, readHeartbeat(t, db, finished.ID).Equal(stale), "a terminal run has no driver")

	var after models.JobRun
	require.NoError(t, db.Select("updated_at").First(&after, "id = ?", renewed.ID).Error)
	require.True(t, after.UpdatedAt.Equal(before.UpdatedAt), "a heartbeat is not a change to the run")
}

// Whole-run settlement: no row may stay non-terminal under a terminal run, and
// a task that never ran must not look like one that succeeded.
func TestDriverLostFailsEveryUnfinishedTask(t *testing.T) {
	store, db := driverTestStore(t)
	runID := runWithTaskRows(t, store, db, TaskStatusRunning, TaskStatusPending)

	finalized, err := store.CompleteIfActive(runID, ErrRunDriverLost)
	require.NoError(t, err)
	require.True(t, finalized)

	got, err := store.Get(runID)
	require.NoError(t, err)
	require.Equal(t, StatusFailed, got.Status)
	require.Equal(t, ErrRunDriverLost.Error(), got.Error)
	require.Len(t, got.Tasks, 2)
	for _, task := range got.Tasks {
		require.Equal(t, TaskStatusFailed, task.Status)
		require.Equal(t, ErrRunDriverLost.Error(), task.Error)
	}
}

func TestRunTasksSettled(t *testing.T) {
	store, db := driverTestStore(t)
	for name, tc := range map[string]struct {
		statuses []TaskStatus
		settled  bool
	}{
		"no rows records nothing": {nil, false},
		"a running row":           {[]TaskStatus{TaskStatusSucceeded, TaskStatusRunning}, false},
		"a pending row":           {[]TaskStatus{TaskStatusSucceeded, TaskStatusPending}, false},
		"all succeeded":           {[]TaskStatus{TaskStatusSucceeded, TaskStatusCached}, true},
		"a recorded failure":      {[]TaskStatus{TaskStatusFailed, TaskStatusSkipped}, true},
	} {
		t.Run(name, func(t *testing.T) {
			settled, err := store.RunTasksSettled(context.Background(), runWithTaskRows(t, store, db, tc.statuses...))
			require.NoError(t, err)
			require.Equal(t, tc.settled, settled)
		})
	}
}

// A driver presumed dead that was only frozen must stop executing a final run.
func TestCancelledRunIDsReportsARunRecoveryFailed(t *testing.T) {
	store, _ := driverTestStore(t)
	lost, err := store.Start(uuid.New(), nil)
	require.NoError(t, err)
	require.NoError(t, store.Complete(lost.ID, ErrRunDriverLost))
	ordinary, err := store.Start(uuid.New(), nil)
	require.NoError(t, err)
	require.NoError(t, store.Complete(ordinary.ID, errors.New("task failed")))

	got, err := store.CancelledRunIDs(context.Background(), []uuid.UUID{lost.ID, ordinary.ID})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{lost.ID}, got)
}
