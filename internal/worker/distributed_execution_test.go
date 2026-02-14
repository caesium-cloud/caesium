package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestDistributedWorkersProcessAllTasksWithoutDuplicateClaims(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() {
		testutil.CloseDB(db)
	})

	store := run.NewStore(db)

	jobID := uuid.New()
	runRecord, err := store.Start(jobID)
	require.NoError(t, err)

	atom := &models.Atom{
		ID:      uuid.New(),
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:3.20",
		Command: `["echo","ok"]`,
	}
	require.NoError(t, db.Create(atom).Error)

	const taskCount = 8
	base := time.Now().UTC().Add(-2 * time.Minute)
	for i := 0; i < taskCount; i++ {
		createdAt := base.Add(time.Duration(i) * time.Second)
		task := &models.Task{
			ID:        uuid.New(),
			JobID:     jobID,
			AtomID:    atom.ID,
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
		}
		require.NoError(t, db.Create(task).Error)
		require.NoError(t, store.RegisterTask(runRecord.ID, task, atom, 0))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var completed int32
	var mu sync.Mutex
	processed := make(map[uuid.UUID]int, taskCount)
	execErrs := make(chan error, taskCount*2)

	executor := func(_ context.Context, taskRun *models.TaskRun) {
		if err := store.StartTaskClaimed(taskRun.JobRunID, taskRun.TaskID, "runtime-"+taskRun.ClaimedBy, taskRun.ClaimedBy); err != nil {
			if !errors.Is(err, run.ErrTaskClaimMismatch) {
				execErrs <- fmt.Errorf("start claimed task %s: %w", taskRun.TaskID, err)
			}
			return
		}

		if err := store.CompleteTaskClaimed(taskRun.JobRunID, taskRun.TaskID, "ok", taskRun.ClaimedBy); err != nil {
			if !errors.Is(err, run.ErrTaskClaimMismatch) {
				execErrs <- fmt.Errorf("complete claimed task %s: %w", taskRun.TaskID, err)
			}
			return
		}

		mu.Lock()
		processed[taskRun.TaskID]++
		taskExecutions := processed[taskRun.TaskID]
		mu.Unlock()

		if taskExecutions > 1 {
			execErrs <- fmt.Errorf("task %s executed %d times", taskRun.TaskID, taskExecutions)
			return
		}

		if atomic.AddInt32(&completed, 1) == taskCount {
			cancel()
		}
	}

	workerA := NewWorker(NewClaimer("node-a", store, time.Minute), NewPool(2), time.Millisecond, executor)
	workerB := NewWorker(NewClaimer("node-b", store, time.Minute), NewPool(2), time.Millisecond, executor)

	workerErrs := make(chan error, 2)
	go func() { workerErrs <- workerA.Run(ctx) }()
	go func() { workerErrs <- workerB.Run(ctx) }()

	for i := 0; i < 2; i++ {
		select {
		case workerErr := <-workerErrs:
			require.NoError(t, workerErr)
		case <-time.After(6 * time.Second):
			t.Fatal("timed out waiting for worker shutdown")
		}
	}

	close(execErrs)
	for execErr := range execErrs {
		require.NoError(t, execErr)
	}

	require.Equal(t, int32(taskCount), atomic.LoadInt32(&completed))

	finalRun, err := store.Get(runRecord.ID)
	require.NoError(t, err)
	require.Len(t, finalRun.Tasks, taskCount)
	for _, taskState := range finalRun.Tasks {
		require.Equal(t, run.TaskStatusSucceeded, taskState.Status)
	}
}
