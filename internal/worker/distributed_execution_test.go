package worker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestDistributedWorkersProcessAllTasksWithoutDuplicateClaims(t *testing.T) {
	const taskCount = 8
	queue := newSharedTaskQueue(taskCount)
	for i := 0; i < taskCount; i++ {
		queue.enqueue(&models.TaskRun{
			ID:     uuid.New(),
			TaskID: uuid.New(),
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var completed int32
	var mu sync.Mutex
	processed := make(map[uuid.UUID]int, taskCount)
	execErrs := make(chan error, taskCount)

	executor := func(_ context.Context, taskRun *models.TaskRun) {
		mu.Lock()
		processed[taskRun.TaskID]++
		taskExecutions := processed[taskRun.TaskID]
		mu.Unlock()

		if taskExecutions > 1 {
			execErrs <- errors.New("task executed more than once")
			return
		}

		time.Sleep(2 * time.Millisecond)
		if atomic.AddInt32(&completed, 1) == taskCount {
			cancel()
		}
	}

	workerA := NewWorker(queueClaimer{nodeID: "node-a", queue: queue}, NewPool(1), time.Millisecond, executor)
	workerB := NewWorker(queueClaimer{nodeID: "node-b", queue: queue}, NewPool(1), time.Millisecond, executor)

	workerErrs := make(chan error, 2)
	go func() { workerErrs <- workerA.Run(ctx) }()
	go func() { workerErrs <- workerB.Run(ctx) }()

	for i := 0; i < 2; i++ {
		select {
		case workerErr := <-workerErrs:
			require.NoError(t, workerErr)
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for worker shutdown")
		}
	}

	close(execErrs)
	for execErr := range execErrs {
		require.NoError(t, execErr)
	}

	require.Equal(t, int32(taskCount), atomic.LoadInt32(&completed))
}

type sharedTaskQueue struct {
	mu    sync.Mutex
	tasks []*models.TaskRun
	next  int
}

func newSharedTaskQueue(capacity int) *sharedTaskQueue {
	return &sharedTaskQueue{tasks: make([]*models.TaskRun, 0, capacity)}
}

func (q *sharedTaskQueue) enqueue(task *models.TaskRun) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.tasks = append(q.tasks, task)
}

func (q *sharedTaskQueue) claim(nodeID string) *models.TaskRun {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.next >= len(q.tasks) {
		return nil
	}
	task := *q.tasks[q.next]
	q.next++
	task.ClaimedBy = nodeID
	return &task
}

type queueClaimer struct {
	nodeID string
	queue  *sharedTaskQueue
}

func (c queueClaimer) ClaimNext(ctx context.Context) (*models.TaskRun, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.queue.claim(c.nodeID), nil
}
