package worker

import (
	"context"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
)

type TaskClaimer interface {
	ClaimNext(ctx context.Context) (*models.TaskRun, error)
}

type TaskExecutor func(ctx context.Context, task *models.TaskRun)

type ExpiredReclaimer interface {
	ReclaimExpired(ctx context.Context) error
}

type Worker struct {
	claimer      TaskClaimer
	pool         *Pool
	pollInterval time.Duration
	executor     TaskExecutor
}

func NewWorker(claimer TaskClaimer, pool *Pool, pollInterval time.Duration, executor TaskExecutor) *Worker {
	if claimer == nil {
		panic("worker requires task claimer")
	}
	if pool == nil {
		pool = NewPool(1)
	}
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	if executor == nil {
		executor = func(context.Context, *models.TaskRun) {}
	}

	return &Worker{
		claimer:      claimer,
		pool:         pool,
		pollInterval: pollInterval,
		executor:     executor,
	}
}

func (w *Worker) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			w.pool.Wait()
			return nil
		default:
		}

		if reclaimer, ok := w.claimer.(ExpiredReclaimer); ok {
			_ = reclaimer.ReclaimExpired(ctx)
		}

		task, err := w.claimer.ClaimNext(ctx)
		if err != nil {
			if ctx.Err() != nil {
				w.pool.Wait()
				return nil
			}
			if sleepErr := sleepWithContext(ctx, w.pollInterval); sleepErr != nil {
				w.pool.Wait()
				return nil
			}
			continue
		}
		if task == nil {
			if sleepErr := sleepWithContext(ctx, w.pollInterval); sleepErr != nil {
				w.pool.Wait()
				return nil
			}
			continue
		}

		if err := w.pool.Submit(ctx, func() {
			w.executor(ctx, task)
		}); err != nil {
			if ctx.Err() != nil {
				w.pool.Wait()
				return nil
			}
			return err
		}
	}
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
