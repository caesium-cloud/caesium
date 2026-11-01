package job

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type recoveryHarness struct {
	recovery  *runRecovery
	store     *run.Store
	jobID     uuid.UUID
	adopted   []uuid.UUID
	callbacks []error
}

const recoveryTestTTL = 30 * time.Second

func newRecoveryHarness(t *testing.T, mode string) *recoveryHarness {
	t.Helper()
	j, store, _, _ := characterizationJob(t, 1, nil)
	h := &recoveryHarness{store: store, jobID: j.id}
	h.recovery = newRunRecovery(RunRecoveryConfig{
		Store: store, TTL: recoveryTestTTL, ExecutionMode: mode,
		Adopt: func(_ context.Context, _, runID uuid.UUID) error {
			h.adopted = append(h.adopted, runID)
			return nil
		},
		DispatchCallbacks: func(_ context.Context, _, _ uuid.UUID, runErr error) error {
			h.callbacks = append(h.callbacks, runErr)
			return nil
		},
	})
	h.recovery.quietUntil = time.Time{}
	return h
}

// startLapsedRun admits a run with one task row per status, its driver gone.
func (h *recoveryHarness) startLapsedRun(t *testing.T, statuses ...run.TaskStatus) uuid.UUID {
	t.Helper()
	started, err := h.store.Start(h.jobID, nil)
	require.NoError(t, err)
	for _, status := range statuses {
		require.NoError(t, h.store.DB().Create(&models.TaskRun{ID: uuid.New(), JobRunID: started.ID, TaskID: uuid.New(),
			Engine: models.AtomEngineDocker, Status: string(status)}).Error)
	}
	stale := time.Now().UTC().Add(-2 * recoveryTestTTL)
	require.NoError(t, h.store.DB().Model(&models.JobRun{}).Where("id = ?", started.ID).
		UpdateColumn("driver_heartbeat_at", stale).Error)
	return started.ID
}

func (h *recoveryHarness) status(t *testing.T, runID uuid.UUID) run.Status {
	t.Helper()
	current, err := h.store.Get(runID)
	require.NoError(t, err)
	return current.Status
}

func TestRecoveryFailsALocalRunWithUnfinishedTasks(t *testing.T) {
	for name, status := range map[string]run.TaskStatus{
		"running row: its container may be alive or already done":  run.TaskStatusRunning,
		"pending row: the engine creates before StartTask commits": run.TaskStatusPending,
	} {
		t.Run(name, func(t *testing.T) {
			h := newRecoveryHarness(t, executionModeLocal)
			runID := h.startLapsedRun(t, status)

			h.recovery.sweep(t.Context())

			current, err := h.store.Get(runID)
			require.NoError(t, err)
			require.Equal(t, run.StatusFailed, current.Status)
			require.Equal(t, run.ErrRunDriverLost.Error(), current.Error)
			require.Empty(t, h.adopted, "re-entry could execute the task a second time")
			require.Equal(t, []error{run.ErrRunDriverLost}, h.callbacks, "no engine remains to dispatch them")

			h.recovery.sweep(t.Context())
			require.Len(t, h.callbacks, 1, "a run is finalized once")
		})
	}
}

// A run with no task rows cannot have started anything, and recorded nothing.
func TestRecoveryFailsALocalRunThatNeverRegisteredTasks(t *testing.T) {
	h := newRecoveryHarness(t, executionModeLocal)
	runID := h.startLapsedRun(t)

	h.recovery.sweep(t.Context())

	require.Equal(t, run.StatusFailed, h.status(t, runID))
	require.Empty(t, h.adopted)
}

// Every outcome is recorded, so the ordinary path derives the run's result from
// the evidence: recovery neither guesses it nor executes anything.
func TestRecoveryAdoptsALocalRunWithEveryTaskRecorded(t *testing.T) {
	h := newRecoveryHarness(t, executionModeLocal)
	runID := h.startLapsedRun(t, run.TaskStatusSucceeded, run.TaskStatusSkipped)

	h.recovery.sweep(t.Context())

	require.Equal(t, []uuid.UUID{runID}, h.adopted)
	require.Equal(t, run.StatusRunning, h.status(t, runID), "adoption finalizes the run, recovery does not")
	require.Empty(t, h.callbacks)
}

// The real re-entry behind Adopt, for both recorded outcomes: nothing executes,
// and the run ends as its rows say.
func TestAdoptingASettledLocalRunExecutesNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		result string
		want   run.Status
	}{
		"every task succeeded": {"success", run.StatusSucceeded},
		"a task failed":        {"failure", run.StatusFailed},
	} {
		t.Run(name, func(t *testing.T) {
			j, store, engine, tasks := characterizationJob(t, 1, nil)
			started, err := store.Start(j.id, nil)
			require.NoError(t, err)
			require.NoError(t, store.RegisterTasks(started.ID, []run.RegisterTaskInput{{Task: tasks[0], Atom: fakeModelAtom(tasks[0].AtomID)}}))
			require.NoError(t, store.StartTask(started.ID, tasks[0].ID, "container"))
			require.NoError(t, store.CompleteTask(started.ID, tasks[0].ID, tc.result, nil, nil))

			_ = j.Run(run.WithContext(t.Context(), started.ID))

			require.Empty(t, engine.createRequestsForTask(tasks[0].ID), "the adopted run executed a recorded task")
			current, err := store.Get(started.ID)
			require.NoError(t, err)
			require.Equal(t, tc.want, current.Status)
		})
	}
}

// Workers own a distributed run's tasks through claims, so its rows prove
// nothing about the lost driver: the run is adopted, never failed.
func TestRecoveryAdoptsADistributedRunWithTasksInFlight(t *testing.T) {
	h := newRecoveryHarness(t, executionModeDistributed)
	runID := h.startLapsedRun(t, run.TaskStatusRunning)

	h.recovery.sweep(t.Context())

	require.Equal(t, []uuid.UUID{runID}, h.adopted)
	require.Equal(t, run.StatusRunning, h.status(t, runID))
}

func TestRecoveryLeavesARunWithALiveDriverAlone(t *testing.T) {
	h := newRecoveryHarness(t, executionModeLocal)
	started, err := h.store.Start(h.jobID, nil)
	require.NoError(t, err)

	h.recovery.sweep(t.Context())

	require.Equal(t, run.StatusRunning, h.status(t, started.ID))
	require.Empty(t, h.adopted)
}

// A claim survives only until its heartbeat lapses: a node that dies after
// claiming must not take the run's recovery with it.
func TestRecoveryRetriesAFailedAdoption(t *testing.T) {
	h := newRecoveryHarness(t, executionModeDistributed)
	runID := h.startLapsedRun(t, run.TaskStatusRunning)
	failing := true
	h.recovery.cfg.Adopt = func(_ context.Context, _, id uuid.UUID) error {
		if failing {
			return errors.New("job row unreadable")
		}
		h.adopted = append(h.adopted, id)
		return nil
	}

	h.recovery.sweep(t.Context())
	require.Empty(t, h.adopted)
	h.recovery.sweep(t.Context())
	require.Empty(t, h.adopted, "the claim fences the next sweep until it lapses")

	failing = false
	h.recovery.now = func() time.Time { return time.Now().Add(2 * recoveryTestTTL) }
	h.recovery.sweep(t.Context())
	require.Equal(t, []uuid.UUID{runID}, h.adopted)
}

// A heartbeat stops arriving while the database is unreachable too, so an old
// heartbeat proves a driver dead only after live ones have had a TTL to renew.
func TestRecoveryWaitsATTLAfterBootOrAnOutage(t *testing.T) {
	h := newRecoveryHarness(t, executionModeLocal)
	runID := h.startLapsedRun(t, run.TaskStatusRunning)

	now := time.Now()
	h.recovery.now = func() time.Time { return now }
	h.recovery.quietUntil = now.Add(recoveryTestTTL)
	h.recovery.sweep(t.Context())
	require.Equal(t, run.StatusRunning, h.status(t, runID), "a server that just booted gave peers no time to renew")

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	now = now.Add(recoveryTestTTL)
	h.recovery.sweep(cancelled)
	require.Equal(t, now.Add(recoveryTestTTL), h.recovery.quietUntil, "a failed read restarts the wait")
	h.recovery.sweep(t.Context())
	require.Equal(t, run.StatusRunning, h.status(t, runID))

	now = now.Add(recoveryTestTTL)
	h.recovery.sweep(t.Context())
	require.Equal(t, run.StatusFailed, h.status(t, runID))
}

func TestHeartbeatRenewsTheRunsThisProcessDrives(t *testing.T) {
	j, store, _, _ := characterizationJob(t, 1, nil)
	driven, err := store.Start(j.id, nil)
	require.NoError(t, err)
	other, err := store.Start(j.id, nil)
	require.NoError(t, err)
	stale := time.Now().UTC().Add(-time.Hour)
	require.NoError(t, store.DB().Model(&models.JobRun{}).Where("id IN ?", []uuid.UUID{driven.ID, other.ID}).
		UpdateColumn("driver_heartbeat_at", stale).Error)

	_, release := RegisterRunCancel(t.Context(), driven.ID)
	defer release()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	StartRunDriverHeartbeat(ctx, store, 30*time.Millisecond)

	heartbeat := func(id uuid.UUID) time.Time {
		var row models.JobRun
		require.NoError(t, store.DB().Select("driver_heartbeat_at").First(&row, "id = ?", id).Error)
		return *row.DriverHeartbeatAt
	}
	require.Eventually(t, func() bool { return heartbeat(driven.ID).After(stale) }, 5*time.Second, 10*time.Millisecond)
	require.True(t, heartbeat(other.ID).Equal(stale), "another driver's run is not this process's to renew")
}
