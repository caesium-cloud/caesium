package worker

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
)

const (
	defaultPollInterval    = 15 * time.Second
	defaultReclaimInterval = 30 * time.Second
)

type TaskClaimer interface {
	ClaimNext(ctx context.Context) (*models.TaskRun, error)
}

type TaskExecutor func(ctx context.Context, task *models.TaskRun)

type ExpiredReclaimer interface {
	ReclaimExpired(ctx context.Context) error
}

type ReclaimGate interface {
	CanReclaim(ctx context.Context) (bool, error)
}

type ReclaimGateFunc func(context.Context) (bool, error)

func (f ReclaimGateFunc) CanReclaim(ctx context.Context) (bool, error) {
	return f(ctx)
}

type Worker struct {
	claimer         TaskClaimer
	pool            *Pool
	pollInterval    time.Duration
	reclaimInterval time.Duration
	lastReclaim     time.Time
	reclaimGate     ReclaimGate
	executor        TaskExecutor
	wakeups         <-chan struct{}
}

func NewWorker(claimer TaskClaimer, pool *Pool, pollInterval time.Duration, executor TaskExecutor) *Worker {
	if claimer == nil {
		panic("worker requires task claimer")
	}
	if pool == nil {
		pool = NewPool(1)
	}
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}
	if executor == nil {
		executor = func(context.Context, *models.TaskRun) {}
	}

	return &Worker{
		claimer:         claimer,
		pool:            pool,
		pollInterval:    pollInterval,
		reclaimInterval: defaultReclaimInterval,
		lastReclaim:     initialLastReclaim(time.Now(), defaultReclaimInterval),
		executor:        executor,
	}
}

func (w *Worker) WithWakeups(ch <-chan struct{}) *Worker {
	w.wakeups = ch
	return w
}

func (w *Worker) WithReclaimGate(gate ReclaimGate) *Worker {
	w.reclaimGate = gate
	return w
}

func (w *Worker) WithReclaimInterval(interval time.Duration) *Worker {
	if interval <= 0 {
		interval = defaultReclaimInterval
	}
	w.reclaimInterval = interval
	w.lastReclaim = initialLastReclaim(time.Now(), interval)
	return w
}

func (w *Worker) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			w.pool.Wait()
			return nil
		default:
		}

		w.reclaimIfDue(ctx)

		task, err := w.claimer.ClaimNext(ctx)
		if err != nil {
			if ctx.Err() != nil {
				w.pool.Wait()
				return nil
			}
			log.Error("failed to claim next task", "error", err)
		}

		if err != nil || task == nil {
			if sleepErr := waitForWork(ctx, w.wakeups, w.pollInterval); sleepErr != nil {
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

func (w *Worker) reclaimIfDue(ctx context.Context) {
	reclaimer, ok := w.claimer.(ExpiredReclaimer)
	if !ok {
		return
	}
	if time.Since(w.lastReclaim) < w.reclaimInterval {
		return
	}

	w.lastReclaim = time.Now()
	if w.reclaimGate != nil {
		canReclaim, err := w.reclaimGate.CanReclaim(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("failed to evaluate reclaim leadership", "error", err)
			}
			return
		}
		if !canReclaim {
			return
		}
	}
	if err := reclaimer.ReclaimExpired(ctx); err != nil && ctx.Err() == nil {
		log.Error("failed to reclaim expired tasks", "error", err)
	}
}

func initialLastReclaim(now time.Time, interval time.Duration) time.Time {
	return now.Add(-interval).Add(randomReclaimOffset(interval))
}

func randomReclaimOffset(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(interval)))
}

func waitForWork(ctx context.Context, wakeups <-chan struct{}, d time.Duration) error {
	if wakeups == nil {
		return sleepWithContext(ctx, d)
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-wakeups:
		return nil
	case <-timer.C:
		return nil
	}
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	// Add up to 20% jitter to the sleep duration to avoid synchronized polling
	var jitter time.Duration
	if maxJitter := int64(d) / 5; maxJitter > 0 {
		jitter = time.Duration(rand.Int64N(maxJitter))
	}
	timer := time.NewTimer(d + jitter)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
