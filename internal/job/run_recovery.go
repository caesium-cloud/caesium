package job

import (
	"context"
	"time"

	"github.com/caesium-cloud/caesium/internal/callback"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

// A process that drives runs proves it with a heartbeat (run/run_driver.go), and
// recovery settles every run whose heartbeat lapsed. That is what brings a run
// to an outcome after its server stopped, whatever triggers its job has.
//
// Recovery never resumes a local run with unfinished tasks. A local attempt has
// no claim, so a row cannot say whether its container is still running or
// already succeeded, and even a pending row can have a live container: the
// engine creates it before StartTask commits. Executing either again could run
// the step twice, so the run fails with ErrRunDriverLost and re-executing it is
// an operator's decision (`caesium run retry`). What remains is safe to adopt
// through job.Run: a local run with every task recorded executes nothing, and a
// distributed run's tasks stay governed by worker claims, so its driver only
// waits for them and finalizes.

// runRecoveryBatch bounds one sweep; the next tick takes the rest.
const runRecoveryBatch = 100

// runDriverBeat is how often a heartbeat is written and lapsed runs are swept: a
// live driver gets several chances to renew inside one TTL.
func runDriverBeat(ttl time.Duration) time.Duration { return ttl / 3 }

// StartRunDriverHeartbeat renews the heartbeat of every run this process is
// driving until ctx is done. The cancel registry already holds exactly that set,
// so one statement per tick serves all of them. Every process that drives runs
// against a shared database must start it, not only servers. A non-positive ttl
// disables it, together with recovery.
func StartRunDriverHeartbeat(ctx context.Context, store *run.Store, ttl time.Duration) {
	if store == nil || ttl <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(runDriverBeat(ttl))
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := store.RenewRunDrivers(ctx, defaultCancelRegistry.registeredRunIDs()); err != nil && ctx.Err() == nil {
					log.Warn("run driver heartbeat failed; retrying on the next tick", "error", err)
				}
			}
		}
	}()
}

// RunRecoveryConfig configures StartRunRecovery.
type RunRecoveryConfig struct {
	Store *run.Store
	// TTL is how old a heartbeat must be before its driver counts as dead.
	TTL           time.Duration
	ExecutionMode string
	// Adopt re-enters a run through job.Run on a context carrying its id.
	Adopt func(ctx context.Context, jobID, runID uuid.UUID) error
	// DispatchCallbacks defaults to the job's completion callbacks.
	DispatchCallbacks func(ctx context.Context, jobID, runID uuid.UUID, runErr error) error
}

// runRecovery is deliberately not leader-gated: ClaimLapsedRun is an atomic
// conditional update, so sweeping on every node settles each run once, and a run
// is recovered as soon as any node can write.
type runRecovery struct {
	cfg RunRecoveryConfig
	now func() time.Time
	// quietUntil holds recovery back until live drivers have had a full TTL to
	// renew. A heartbeat also goes stale while the database is unreachable, so
	// after a boot or a failed read an old heartbeat proves nothing yet.
	quietUntil time.Time
}

func newRunRecovery(cfg RunRecoveryConfig) *runRecovery {
	if cfg.DispatchCallbacks == nil {
		cfg.DispatchCallbacks = func(ctx context.Context, jobID, runID uuid.UUID, runErr error) error {
			return callback.Default().Dispatch(ctx, jobID, runID, runErr)
		}
	}
	return &runRecovery{cfg: cfg, now: time.Now, quietUntil: time.Now().Add(cfg.TTL)}
}

// StartRunRecovery sweeps for lapsed runs until ctx is done.
func StartRunRecovery(ctx context.Context, cfg RunRecoveryConfig) {
	if cfg.Store == nil || cfg.TTL <= 0 {
		log.Warn("run recovery is disabled; a run whose server stops stays running", "ttl", cfg.TTL)
		return
	}
	recovery := newRunRecovery(cfg)
	go func() {
		ticker := time.NewTicker(runDriverBeat(cfg.TTL))
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				recovery.sweep(ctx)
			}
		}
	}()
}

func (r *runRecovery) sweep(ctx context.Context) {
	now := r.now()
	cutoff := now.Add(-r.cfg.TTL)
	lapsed, err := r.cfg.Store.LapsedRuns(ctx, cutoff, runRecoveryBatch)
	if err != nil {
		r.quietUntil = now.Add(r.cfg.TTL)
		if ctx.Err() == nil {
			log.Warn("run recovery could not read running runs", "error", err)
		}
		return
	}
	if now.Before(r.quietUntil) {
		return
	}
	for _, candidate := range lapsed {
		if ctx.Err() != nil {
			return
		}
		if err := r.recoverRun(ctx, candidate, cutoff); err != nil {
			// The claim's heartbeat lapses in turn, so a later sweep retries.
			log.Error("run recovery failed; retrying after the heartbeat lapses again",
				"job_id", candidate.JobID, "run_id", candidate.ID, "error", err)
		}
	}
}

func (r *runRecovery) recoverRun(ctx context.Context, candidate run.LapsedRun, cutoff time.Time) error {
	claimed, err := r.cfg.Store.ClaimLapsedRun(ctx, candidate.ID, cutoff)
	if err != nil || !claimed {
		return err
	}
	if normalizeExecutionMode(r.cfg.ExecutionMode) == executionModeLocal {
		settled, err := r.cfg.Store.RunTasksSettled(ctx, candidate.ID)
		if err != nil {
			return err
		}
		if !settled {
			return r.finalize(ctx, candidate)
		}
	}
	log.Warn("adopting a run whose driver stopped", "job_id", candidate.JobID, "run_id", candidate.ID)
	return r.cfg.Adopt(ctx, candidate.JobID, candidate.ID)
}

func (r *runRecovery) finalize(ctx context.Context, candidate run.LapsedRun) error {
	finalized, err := r.cfg.Store.CompleteIfActive(candidate.ID, run.ErrRunDriverLost)
	if err != nil || !finalized {
		return err
	}
	log.Warn("failed a run whose driver stopped with tasks unfinished", "job_id", candidate.JobID, "run_id", candidate.ID)
	// No engine remains to do this, and only the call that finalized may.
	if err := r.cfg.DispatchCallbacks(context.WithoutCancel(ctx), candidate.JobID, candidate.ID, run.ErrRunDriverLost); err != nil {
		log.Error("callback dispatch failure", "job_id", candidate.JobID, "run_id", candidate.ID, "error", err)
	}
	return nil
}
