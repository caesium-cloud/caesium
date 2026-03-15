package job

import (
	"context"
	"errors"
	"testing"
	"time"

	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestRetrySucceedsOnSecondAttempt verifies that a task that fails on the first
// attempt but succeeds on the retry is marked succeeded.
func TestRetrySucceedsOnSecondAttempt(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskID := uuid.New()
	atomID := uuid.New()

	taskSvc := &fakeTaskService{tasks: models.Tasks{
		{
			ID:      taskID,
			JobID:   jobID,
			AtomID:  atomID,
			Retries: 1, // one retry = two total attempts
		},
	}}
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{
		atomID: fakeModelAtom(atomID),
	}}
	persistGraph(t, db, taskSvc.tasks, nil)

	// First attempt (name = taskID.String()) fails.
	// Second attempt (name = taskID.String()+"-attempt2") succeeds (no error registered).
	engine.createErrByName[taskID.String()] = errors.New("transient failure")

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyHalt,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, &fakeTaskEdgeService{}, engine)

	err := New(&models.Job{ID: jobID}, opts...).Run(context.Background())
	require.NoError(t, err)

	snapshot := latestRunSnapshot(t, store, jobID)
	status := taskStatusByID(snapshot)
	require.Equal(t, run.TaskStatusSucceeded, status[taskID])
}

// TestRetryExhaustsAllAttemptsMarksFailed verifies that when all retry attempts
// fail the task is ultimately marked as failed.
func TestRetryExhaustsAllAttemptsMarksFailed(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskID := uuid.New()
	atomID := uuid.New()

	taskSvc := &fakeTaskService{tasks: models.Tasks{
		{
			ID:      taskID,
			JobID:   jobID,
			AtomID:  atomID,
			Retries: 2, // two retries = three total attempts
		},
	}}
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{
		atomID: fakeModelAtom(atomID),
	}}
	persistGraph(t, db, taskSvc.tasks, nil)

	// All three attempts fail: attempt1, attempt2, attempt3.
	engine.createErrByName[taskID.String()] = errors.New("always fails")
	engine.createErrByName[taskID.String()+"-attempt2"] = errors.New("always fails")
	engine.createErrByName[taskID.String()+"-attempt3"] = errors.New("always fails")

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyHalt,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, &fakeTaskEdgeService{}, engine)

	err := New(&models.Job{ID: jobID}, opts...).Run(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "always fails")

	snapshot := latestRunSnapshot(t, store, jobID)
	status := taskStatusByID(snapshot)
	require.Equal(t, run.TaskStatusFailed, status[taskID])
}

// TestRetryNoRetriesOnSuccess verifies that a task that succeeds on the first
// attempt does not trigger any retries even when retries are configured.
func TestRetryNoRetriesOnSuccess(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := run.NewStore(db)
	engine := newFakeEngine()

	jobID := uuid.New()
	taskID := uuid.New()
	atomID := uuid.New()

	taskSvc := &fakeTaskService{tasks: models.Tasks{
		{
			ID:      taskID,
			JobID:   jobID,
			AtomID:  atomID,
			Retries: 3, // retries configured, but first attempt should succeed
		},
	}}
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{
		atomID: fakeModelAtom(atomID),
	}}
	persistGraph(t, db, taskSvc.tasks, nil)

	opts := withTestDeps(store, env.Environment{
		MaxParallelTasks:  1,
		TaskFailurePolicy: taskFailurePolicyHalt,
		ExecutionMode:     executionModeLocal,
	}, taskSvc, atomSvc, &fakeTaskEdgeService{}, engine)

	err := New(&models.Job{ID: jobID}, opts...).Run(context.Background())
	require.NoError(t, err)

	snapshot := latestRunSnapshot(t, store, jobID)
	status := taskStatusByID(snapshot)
	require.Equal(t, run.TaskStatusSucceeded, status[taskID])

	// Verify only one atom was created (no retry atoms).
	engine.mu.Lock()
	atomCount := len(engine.atoms)
	engine.mu.Unlock()
	require.Equal(t, 1, atomCount, "expected exactly 1 atom created (no retries needed)")
}

// TestComputeRetryBackoffCalculation verifies the exponential backoff delay formula:
// delay = retryDelay * 2^(attempt-1).
func TestComputeRetryBackoffCalculation(t *testing.T) {
	tests := []struct {
		name        string
		retryDelay  time.Duration
		retryBackof bool
		attempt     int
		want        time.Duration
	}{
		{
			name:       "no backoff attempt 1",
			retryDelay: 100 * time.Millisecond,
			attempt:    1,
			want:       100 * time.Millisecond,
		},
		{
			name:       "no backoff attempt 2 stays constant",
			retryDelay: 100 * time.Millisecond,
			attempt:    2,
			want:       100 * time.Millisecond,
		},
		{
			name:        "backoff attempt 1 — no multiplication",
			retryDelay:  100 * time.Millisecond,
			retryBackof: true,
			attempt:     1,
			want:        100 * time.Millisecond, // 100ms * 2^0 = 100ms
		},
		{
			name:        "backoff attempt 2 — doubles",
			retryDelay:  100 * time.Millisecond,
			retryBackof: true,
			attempt:     2,
			want:        200 * time.Millisecond, // 100ms * 2^1 = 200ms
		},
		{
			name:        "backoff attempt 3 — quadruples",
			retryDelay:  100 * time.Millisecond,
			retryBackof: true,
			attempt:     3,
			want:        400 * time.Millisecond, // 100ms * 2^2 = 400ms
		},
		{
			name:        "backoff attempt 4",
			retryDelay:  50 * time.Millisecond,
			retryBackof: true,
			attempt:     4,
			want:        400 * time.Millisecond, // 50ms * 2^3 = 400ms
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			taskModel := &models.Task{
				RetryDelay:   tt.retryDelay,
				RetryBackoff: tt.retryBackof,
			}
			got := computeRetryDelay(taskModel, tt.attempt)
			require.Equal(t, tt.want, got)
		})
	}
}

// TestComputeRetryDelayZeroWhenNoRetryDelay verifies that tasks with no retry
// delay configured produce a zero delay regardless of attempt number or backoff.
func TestComputeRetryDelayZeroWhenNoRetryDelay(t *testing.T) {
	taskModel := &models.Task{
		Retries:      3,
		RetryDelay:   0,
		RetryBackoff: true,
	}
	for attempt := 1; attempt <= 4; attempt++ {
		got := computeRetryDelay(taskModel, attempt)
		require.Equal(t, time.Duration(0), got, "attempt %d should have zero delay", attempt)
	}
}
