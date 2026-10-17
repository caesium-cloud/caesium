package worker

import (
	"context"
	"errors"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/log"
)

var errTaskStartNotPersisted = errors.New("task start persistence expired; awaiting claim recovery")

// persistTaskStart retains the already-created atom across migration write
// locks. Exhausting the store's short retry budget says nothing about the
// atom's result, so retry only the start transaction, never the execution.
func (e *runtimeExecutor) persistTaskStart(ctx context.Context, taskRun *models.TaskRun, runtimeID string) error {
	// Bound retention even when task/run timeouts are disabled. Earlier task
	// deadlines or claim-loss/shutdown cancellation still win.
	ctx, cancel := context.WithTimeoutCause(ctx, defaultLeaseTTL, errTaskStartNotPersisted)
	defer cancel()
	claim := run.TaskClaim{ClaimedBy: taskRun.ClaimedBy, ClaimAttempt: taskRun.ClaimAttempt}
	for {
		err := e.store.StartTaskClaimedContext(ctx, taskRun.JobRunID, taskRun.ID, runtimeID, claim)
		if err == nil {
			return nil
		}
		if cause := context.Cause(ctx); cause != nil {
			return errors.Join(ctx.Err(), cause)
		}
		if !isClaimContentionErr(err) {
			return err
		}
		log.Warn("worker task start contended; retaining container while retrying persistence",
			"run_id", taskRun.JobRunID, "task_run_id", taskRun.ID, "atom_id", runtimeID, "error", err)
		if err := sleepBusyRetry(ctx, 100*time.Millisecond); err != nil {
			return errors.Join(err, context.Cause(ctx))
		}
	}
}
