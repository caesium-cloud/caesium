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
	"gorm.io/gorm"
)

type recoveryHarness struct {
	recovery  *runRecovery
	store     *run.Store
	jobID     uuid.UUID
	clock     time.Time
	driven    map[uuid.UUID]bool
	adopted   []uuid.UUID
	callbacks []error
}

const (
	recoveryTestTTL   = 30 * time.Second
	recoveryTestBeat  = recoveryTestTTL / 3
	recoveryTestGrace = 2 * recoveryTestBeat
	// recoveryTestStall is a writer stall longer than any wait in recovery.
	recoveryTestStall = 35 * time.Second
)

func newRecoveryHarness(t *testing.T, mode string) *recoveryHarness {
	t.Helper()
	j, store, _, _ := characterizationJob(t, 1, nil)
	h := &recoveryHarness{store: store, jobID: j.id, clock: time.Now(), driven: map[uuid.UUID]bool{}}
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
	h.recovery.now = func() time.Time { return h.clock }
	h.recovery.driving = func(id uuid.UUID) bool { return h.driven[id] }
	return h
}

// startRun admits a run with one task row per status. Nothing drives it, and no
// sweep has seen it.
func (h *recoveryHarness) startRun(t *testing.T, statuses ...run.TaskStatus) uuid.UUID {
	t.Helper()
	started, err := h.store.Start(h.jobID, nil)
	require.NoError(t, err)
	for _, status := range statuses {
		require.NoError(t, h.store.DB().Create(&models.TaskRun{ID: uuid.New(), JobRunID: started.ID, TaskID: uuid.New(),
			Engine: models.AtomEngineDocker, Status: string(status)}).Error)
	}
	return started.ID
}

// sweepAfter advances the clock and sweeps.
func (h *recoveryHarness) sweepAfter(t *testing.T, d time.Duration) {
	t.Helper()
	h.clock = h.clock.Add(d)
	h.recovery.sweep(t.Context())
}

// recoverAll sweeps every run from first sight through the mark to the outcome.
func (h *recoveryHarness) recoverAll(t *testing.T) {
	t.Helper()
	h.recovery.sweep(t.Context())
	h.sweepAfter(t, recoveryTestTTL)
	h.sweepAfter(t, recoveryTestGrace)
}

// stallNextUpdate holds the next write on the database for d of harness time, as
// a stalled writer would: the clock moves while the statement waits.
func (h *recoveryHarness) stallNextUpdate(t *testing.T, d time.Duration) {
	t.Helper()
	fired := false
	require.NoError(t, h.store.DB().Callback().Update().Before("gorm:update").Replace("test:stall-update", func(*gorm.DB) {
		if !fired {
			fired = true
			h.clock = h.clock.Add(d)
		}
	}))
}

// stallNextRead is stallNextUpdate for the next read of the run heartbeats.
func (h *recoveryHarness) stallNextRead(t *testing.T, d time.Duration) {
	t.Helper()
	fired := false
	require.NoError(t, h.store.DB().Callback().Query().Before("gorm:query").Replace("test:stall-read", func(*gorm.DB) {
		if !fired {
			fired = true
			h.clock = h.clock.Add(d)
		}
	}))
}

func (h *recoveryHarness) renew(t *testing.T, runID uuid.UUID) {
	t.Helper()
	require.NoError(t, h.store.RenewRunDrivers(t.Context(), []uuid.UUID{runID}))
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
			runID := h.startRun(t, status)

			h.recoverAll(t)

			current, err := h.store.Get(runID)
			require.NoError(t, err)
			require.Equal(t, run.StatusFailed, current.Status)
			require.Equal(t, run.ErrRunDriverLost.Error(), current.Error)
			require.Empty(t, h.adopted, "re-entry could execute the task a second time")
			require.Equal(t, []error{run.ErrRunDriverLost}, h.callbacks, "no engine remains to dispatch them")

			h.sweepAfter(t, recoveryTestBeat)
			require.Len(t, h.callbacks, 1, "a run is finalized once")
		})
	}
}

// A run with no task rows cannot have started anything, and recorded nothing.
func TestRecoveryFailsALocalRunThatNeverRegisteredTasks(t *testing.T) {
	h := newRecoveryHarness(t, executionModeLocal)
	runID := h.startRun(t)

	h.recoverAll(t)

	require.Equal(t, run.StatusFailed, h.status(t, runID))
	require.Empty(t, h.adopted)
}

// A run from before the column has no heartbeat to change, so it lapses from
// first sight and its swap has to match NULL.
func TestRecoveryFailsARunFromBeforeTheColumn(t *testing.T) {
	h := newRecoveryHarness(t, executionModeLocal)
	runID := h.startRun(t, run.TaskStatusRunning)
	require.NoError(t, h.store.DB().Model(&models.JobRun{}).Where("id = ?", runID).
		UpdateColumn("driver_heartbeat_at", nil).Error)

	h.recoverAll(t)

	require.Equal(t, run.StatusFailed, h.status(t, runID))
}

// Every outcome is recorded, so the ordinary path derives the run's result from
// the evidence: recovery neither guesses it nor executes anything.
func TestRecoveryAdoptsALocalRunWithEveryTaskRecorded(t *testing.T) {
	h := newRecoveryHarness(t, executionModeLocal)
	runID := h.startRun(t, run.TaskStatusSucceeded, run.TaskStatusSkipped)

	h.recoverAll(t)

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
	runID := h.startRun(t, run.TaskStatusRunning)

	h.recoverAll(t)

	require.Equal(t, []uuid.UUID{runID}, h.adopted)
	require.Equal(t, run.StatusRunning, h.status(t, runID))
}

func TestRecoveryLeavesARunWithALiveDriverAlone(t *testing.T) {
	h := newRecoveryHarness(t, executionModeLocal)
	runID := h.startRun(t, run.TaskStatusRunning)

	h.recovery.sweep(t.Context())
	for range 12 {
		h.renew(t, runID)
		h.sweepAfter(t, recoveryTestBeat)
	}

	require.Equal(t, run.StatusRunning, h.status(t, runID))
	require.Empty(t, h.adopted)
}

// The ordering the first review found. The sweep has read a value that has been
// unchanged for a TTL and is about to write when a renewal commits, stamped so
// long ago that any age test would call it expired. It committed first.
func TestRecoveryLosesToARenewalThatCommitsAfterItsRead(t *testing.T) {
	h := newRecoveryHarness(t, executionModeLocal)
	runID := h.startRun(t, run.TaskStatusRunning)
	h.recovery.sweep(t.Context())

	stampedBeforeItQueued := time.Now().UTC().Add(-time.Hour)
	h.recovery.afterRead = func() {
		h.recovery.afterRead = nil
		require.NoError(t, h.store.DB().Model(&models.JobRun{}).Where("id = ?", runID).
			UpdateColumn("driver_heartbeat_at", stampedBeforeItQueued).Error)
	}
	h.sweepAfter(t, recoveryTestTTL)
	h.sweepAfter(t, recoveryTestGrace)

	require.Equal(t, run.StatusRunning, h.status(t, runID), "a healthy run was failed by a slow renewal")
	require.True(t, h.recovery.observed[runID].heartbeat.Equal(stampedBeforeItQueued), "the sweep must take the new value as a sign of life")
	require.False(t, h.recovery.observed[runID].marked)
}

// A write stall holds renewals back longer than the TTL, and a sweep's mark gets
// through first. The mark only marks: the renewal that lands behind it still
// saves the run.
func TestARenewalBehindTheMarkSavesTheRun(t *testing.T) {
	h := newRecoveryHarness(t, executionModeLocal)
	runID := h.startRun(t, run.TaskStatusRunning)
	h.recovery.sweep(t.Context())
	h.sweepAfter(t, recoveryTestTTL)
	require.True(t, h.recovery.observed[runID].marked)

	h.renew(t, runID)
	h.sweepAfter(t, recoveryTestGrace)

	require.Equal(t, run.StatusRunning, h.status(t, runID))
	require.Empty(t, h.callbacks)
}

// The second review's ordering: the mark waited on the stalled writer, and the
// next sweep arrives BEFORE the healthy driver's renewal could land behind it.
// The grace has to start when the mark committed, or the stall itself uses it up.
func TestADelayedMarkStillGivesTheDriverItsGrace(t *testing.T) {
	h := newRecoveryHarness(t, executionModeLocal)
	runID := h.startRun(t, run.TaskStatusRunning)
	h.recovery.sweep(t.Context())

	h.stallNextUpdate(t, recoveryTestStall)
	h.sweepAfter(t, recoveryTestTTL)
	require.True(t, h.recovery.observed[runID].marked)

	h.sweepAfter(t, time.Second)
	require.Equal(t, run.StatusRunning, h.status(t, runID), "the stall used up the grace, so a healthy run failed before its renewal landed")
	require.Empty(t, h.callbacks)

	h.renew(t, runID)
	h.sweepAfter(t, recoveryTestGrace)
	require.Equal(t, run.StatusRunning, h.status(t, runID))
	require.False(t, h.recovery.observed[runID].marked)
}

// A slow mark delays recovery of a run that really is dead. It does not stop it.
func TestADelayedMarkStillRecoversADeadRun(t *testing.T) {
	h := newRecoveryHarness(t, executionModeLocal)
	runID := h.startRun(t, run.TaskStatusRunning)
	h.recovery.sweep(t.Context())

	h.stallNextUpdate(t, recoveryTestStall)
	h.sweepAfter(t, recoveryTestTTL)
	h.sweepAfter(t, recoveryTestGrace)

	require.Equal(t, run.StatusFailed, h.status(t, runID))
}

// The same class on the adoption path: the wait for a new driver's first renewal
// starts when the claim committed, not when it was attempted.
func TestADelayedClaimDoesNotReMarkTheAdoptedRun(t *testing.T) {
	h := newRecoveryHarness(t, executionModeDistributed)
	runID := h.startRun(t, run.TaskStatusRunning)
	h.recovery.sweep(t.Context())
	h.sweepAfter(t, recoveryTestTTL)

	h.stallNextUpdate(t, recoveryTestStall)
	h.sweepAfter(t, recoveryTestGrace)
	require.Equal(t, []uuid.UUID{runID}, h.adopted)

	h.sweepAfter(t, time.Second)
	require.False(t, h.recovery.observed[runID].marked, "the stall was counted as time the new driver had to renew")
}

// And on the read. The read may have seen the row at any moment after it began,
// so a stall inside it cannot prove the value stayed unchanged that long.
func TestAStalledReadIsNotCountedAsUnchangedTime(t *testing.T) {
	h := newRecoveryHarness(t, executionModeLocal)
	runID := h.startRun(t, run.TaskStatusRunning)
	h.recovery.sweep(t.Context())

	h.stallNextRead(t, recoveryTestStall)
	h.sweepAfter(t, time.Second)

	require.False(t, h.recovery.observed[runID].marked, "the sweep marked a run on the strength of its own stall")
}

// A run first seen by a stalled read gets its whole TTL from when it was seen.
func TestAStalledFirstReadStartsTheWaitWhenItReturned(t *testing.T) {
	h := newRecoveryHarness(t, executionModeLocal)
	runID := h.startRun(t, run.TaskStatusRunning)

	h.stallNextRead(t, recoveryTestStall)
	h.recovery.sweep(t.Context())
	h.sweepAfter(t, time.Second)

	require.False(t, h.recovery.observed[runID].marked)
}

// A process's own write stall stops its renewals for as long as it lasts, which
// is all a single-node deployment would have to go on. It knows better.
func TestRecoveryNeverTakesARunThisProcessDrives(t *testing.T) {
	h := newRecoveryHarness(t, executionModeLocal)
	runID := h.startRun(t, run.TaskStatusRunning)
	h.driven[runID] = true

	h.recovery.sweep(t.Context())
	for range 30 {
		h.sweepAfter(t, recoveryTestTTL)
	}

	require.Equal(t, run.StatusRunning, h.status(t, runID))
	require.Empty(t, h.callbacks)
	require.False(t, h.recovery.observed[runID].marked, "a mark would be a claim on its own run")
}

// Greptile's finding on the first round: a non-UTC clock must not make a fresh
// heartbeat look expired. Time is only ever measured between two readings of
// this process's own clock, so no zone can reach the database.
func TestRecoveryIsIndifferentToTheHostTimeZone(t *testing.T) {
	h := newRecoveryHarness(t, executionModeLocal)
	h.clock = h.clock.In(time.FixedZone("ahead", 14*60*60))
	runID := h.startRun(t, run.TaskStatusRunning)

	h.recovery.sweep(t.Context())
	h.sweepAfter(t, recoveryTestBeat)

	require.Equal(t, run.StatusRunning, h.status(t, runID))
	require.False(t, h.recovery.observed[runID].marked)
}

// A failing adoption ends. Its claim changed the value, so each retry waits a
// full lapse, and after the bound the run fails and frees what it holds.
func TestRepeatedlyFailingAdoptionEndsTheRun(t *testing.T) {
	h := newRecoveryHarness(t, executionModeDistributed)
	runID := h.startRun(t, run.TaskStatusRunning)
	attempts := 0
	h.recovery.cfg.Adopt = func(context.Context, uuid.UUID, uuid.UUID) error {
		attempts++
		return errors.New("job row unreadable")
	}

	h.recoverAll(t)
	for range maxRunAdoptions {
		require.Equal(t, run.StatusRunning, h.status(t, runID))
		h.sweepAfter(t, recoveryTestTTL)
		h.sweepAfter(t, recoveryTestGrace)
	}

	require.Equal(t, maxRunAdoptions, attempts)
	require.Equal(t, run.StatusFailed, h.status(t, runID))
	require.Equal(t, []error{run.ErrRunDriverLost}, h.callbacks)
}

// The same bound for an adoption that reported no error: job.Run started, gave
// up before finalizing, and the run lapsed again.
func TestSilentlyAbandonedAdoptionEndsTheRun(t *testing.T) {
	h := newRecoveryHarness(t, executionModeLocal)
	runID := h.startRun(t, run.TaskStatusSucceeded)

	h.recoverAll(t)
	for range maxRunAdoptions {
		h.sweepAfter(t, recoveryTestTTL)
		h.sweepAfter(t, recoveryTestGrace)
	}

	require.Len(t, h.adopted, maxRunAdoptions)
	require.Equal(t, run.StatusFailed, h.status(t, runID))
}

// An adoption that takes hold is not an attempt to count against: it drives the
// run from then on, however long the run lasts.
func TestAHealthyAdoptionIsLeftToFinish(t *testing.T) {
	h := newRecoveryHarness(t, executionModeDistributed)
	runID := h.startRun(t, run.TaskStatusRunning)

	h.recoverAll(t)
	h.driven[runID] = true
	for range 30 {
		h.sweepAfter(t, recoveryTestTTL)
	}

	require.Len(t, h.adopted, 1)
	require.Equal(t, run.StatusRunning, h.status(t, runID))
}

// A heartbeat stops arriving while the database is unreachable too, so nothing
// can lapse until live drivers have had a TTL to renew after it comes back.
func TestRecoveryWaitsATTLAfterBootOrAnOutage(t *testing.T) {
	h := newRecoveryHarness(t, executionModeLocal)
	runID := h.startRun(t, run.TaskStatusRunning)

	h.recovery.sweep(t.Context())
	h.sweepAfter(t, recoveryTestTTL-time.Second)
	require.False(t, h.recovery.observed[runID].marked, "a server that just booted gave peers no time to renew")

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	h.clock = h.clock.Add(time.Second)
	h.recovery.sweep(cancelled)
	h.sweepAfter(t, recoveryTestTTL-time.Second)
	require.False(t, h.recovery.observed[runID].marked, "a failed read restarts the wait")

	h.sweepAfter(t, time.Second)
	require.True(t, h.recovery.observed[runID].marked)
}

func TestRecoveryForgetsRunsThatEnded(t *testing.T) {
	h := newRecoveryHarness(t, executionModeLocal)
	runID := h.startRun(t)
	h.recovery.sweep(t.Context())
	require.Contains(t, h.recovery.observed, runID)

	require.NoError(t, h.store.Complete(runID, nil))
	h.sweepAfter(t, recoveryTestBeat)
	require.NotContains(t, h.recovery.observed, runID)
}

// This defer is the ONLY dispatcher when something else finalized the run, so it
// must not grow a "skip unless this call finalized": that would silently lose
// every callback in these two cases. Dispatcher.Dispatch drops the duplicates.
func TestAnEngineStillDispatchesForARunAnotherPathFinalized(t *testing.T) {
	for name, finalize := range map[string]func(*testing.T, *run.Store, uuid.UUID){
		"the in-memory owner finalized it": func(t *testing.T, store *run.Store, runID uuid.UUID) {
			require.NoError(t, store.Complete(runID, nil))
		},
		"a cancel finalized it": func(t *testing.T, store *run.Store, runID uuid.UUID) {
			require.NoError(t, store.CancelRun(t.Context(), runID))
		},
	} {
		t.Run(name, func(t *testing.T) {
			j, store, _, _ := characterizationJob(t, 1, nil)
			dispatched := 0
			j.dispatchRunCallbacks = func(context.Context, uuid.UUID, uuid.UUID, error) error {
				dispatched++
				return nil
			}
			j.beforeComplete = func(runID uuid.UUID) { finalize(t, store, runID) }

			_ = j.Run(t.Context())

			require.Equal(t, 1, dispatched)
		})
	}
}

func TestAnEngineDispatchesOnceForARunItFinalized(t *testing.T) {
	j, _, _, _ := characterizationJob(t, 1, nil)
	dispatched := 0
	j.dispatchRunCallbacks = func(context.Context, uuid.UUID, uuid.UUID, error) error {
		dispatched++
		return nil
	}

	require.NoError(t, j.Run(t.Context()))

	require.Equal(t, 1, dispatched)
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
