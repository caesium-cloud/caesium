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
)

func TestWorkerRunExecutesClaimedTasks(t *testing.T) {
	var executed int32
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	claimer := &sequenceClaimer{
		responses: []claimerResponse{
			{task: &models.TaskRun{ID: uuid.New()}},
			{task: &models.TaskRun{ID: uuid.New()}},
		},
	}

	worker := NewWorker(claimer, NewPool(2), time.Millisecond, func(_ context.Context, _ *models.TaskRun) {
		if atomic.AddInt32(&executed, 1) == 2 {
			cancel()
		}
	})

	if err := worker.Run(ctx); err != nil {
		t.Fatalf("worker run failed: %v", err)
	}

	if got := atomic.LoadInt32(&executed); got != 2 {
		t.Fatalf("expected 2 executed tasks, got %d", got)
	}
}

func TestWorkerRunContinuesAfterClaimErrors(t *testing.T) {
	var executed int32
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	claimer := &sequenceClaimer{
		responses: []claimerResponse{
			{err: errors.New("transient claim failure")},
			{task: &models.TaskRun{ID: uuid.New()}},
		},
	}

	worker := NewWorker(claimer, NewPool(1), time.Millisecond, func(_ context.Context, _ *models.TaskRun) {
		atomic.AddInt32(&executed, 1)
		cancel()
	})

	if err := worker.Run(ctx); err != nil {
		t.Fatalf("worker run failed: %v", err)
	}

	if got := atomic.LoadInt32(&executed); got != 1 {
		t.Fatalf("expected 1 executed task, got %d", got)
	}
}

type claimerResponse struct {
	task *models.TaskRun
	err  error
}

type sequenceClaimer struct {
	mu        sync.Mutex
	responses []claimerResponse
}

func (s *sequenceClaimer) ClaimNext(context.Context) (*models.TaskRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.responses) == 0 {
		return nil, nil
	}

	resp := s.responses[0]
	s.responses = s.responses[1:]
	return resp.task, resp.err
}
