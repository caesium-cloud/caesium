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
// recovery settles every run whose heartbeat stopped changing. That is what
// brings a run to an outcome after its server stopped, whatever triggers its job
// has.
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
//
// Failing a live run is the worst thing this can do, so a run must survive three
// checks, each against a different way a heartbeat can look dead:
//
//   - A process never recovers a run it is driving. A node's own write stall
//     stops its renewals for as long as the stall lasts (#600 records over a
//     minute), so this is what makes a single node immune to it.
//   - A sweeper times "this value did not change for a TTL" on its own clock.
//     Reading a stamp's age could not work: a renewal is stamped before it
//     queues on dqlite's single writer, so one that commits late reads as
//     already expired. Any commit changes the value and restarts the wait.
//     Observing from the sweeper's own start also holds it back a full TTL after
//     a boot, and a failed read restarts the wait the same way.
//   - The first write only marks the run. A mark has to pass through the same
//     writer as the renewals, so its commit proves any stall is over, and any
//     live peer then has a beat to overwrite it before the final write, which is
//     a compare-and-swap on the mark.
//
// That covers a stall on the writer. It cannot cover a live peer that stays cut
// off from the leader for a TTL and a beat while the sweeper can still write:
// that peer is indistinguishable from a dead one. Its run is failed though it
// was healthy, which is why the cost is bounded: nothing is executed twice, the
// peer's cancel reconciler stops its engine once it can read again, callbacks
// fire once, and the run can be retried. Raise CAESIUM_RUN_LEASE_TTL to tolerate
// longer partitions, at the price of slower recovery.

// maxRunAdoptions bounds re-entries of one run from one process. An adopted run
// that lapses again left job.Run without finalizing, or never got there, and
// nothing guarantees the next attempt differs. Counting adoptions rather than
// adopter errors also catches a job.Run that gives up silently.
const maxRunAdoptions = 3

// runDriverBeat is how often a heartbeat is written and runs are swept: a live
// driver gets several chances to renew inside one TTL.
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
	// TTL is how long a heartbeat must go unchanged before its driver is suspect.
	TTL           time.Duration
	ExecutionMode string
	// Adopt re-enters a run through job.Run on a context carrying its id.
	Adopt func(ctx context.Context, jobID, runID uuid.UUID) error
	// DispatchCallbacks defaults to the job's completion callbacks.
	DispatchCallbacks func(ctx context.Context, jobID, runID uuid.UUID, runErr error) error
}

// runObservation is what this process has seen of one run.
type runObservation struct {
	heartbeat *time.Time
	// changedAt is this process's clock when heartbeat last changed.
	changedAt time.Time
	// marked means heartbeat is this process's own mark.
	marked    bool
	adoptions int
}

// runRecovery is deliberately not leader-gated: every write is a compare-and-swap
// on the heartbeat, so sweeping on every node settles each run once, and a run
// is recovered as soon as any node can write.
type runRecovery struct {
	cfg      RunRecoveryConfig
	now      func() time.Time
	observed map[uuid.UUID]*runObservation
	// driving reports whether this process is driving a run.
	driving func(uuid.UUID) bool
	// afterRead lets a test land a write between the read and the swap.
	afterRead func()
}

func newRunRecovery(cfg RunRecoveryConfig) *runRecovery {
	if cfg.DispatchCallbacks == nil {
		cfg.DispatchCallbacks = func(ctx context.Context, jobID, runID uuid.UUID, runErr error) error {
			return callback.Default().Dispatch(ctx, jobID, runID, runErr)
		}
	}
	return &runRecovery{
		cfg: cfg, now: time.Now, observed: map[uuid.UUID]*runObservation{},
		driving: defaultCancelRegistry.drives,
	}
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
	runs, err := r.cfg.Store.RunHeartbeats(ctx)
	if err != nil {
		// While nothing can be read, nothing was observed, and renewals may not
		// be landing either.
		for _, seen := range r.observed {
			seen.changedAt, seen.marked = now, false
		}
		if ctx.Err() == nil {
			log.Warn("run recovery could not read running runs", "error", err)
		}
		return
	}
	if r.afterRead != nil {
		r.afterRead()
	}

	running := make(map[uuid.UUID]struct{}, len(runs))
	for _, current := range runs {
		running[current.ID] = struct{}{}
		seen, known := r.observed[current.ID]
		if !known {
			r.observed[current.ID] = &runObservation{heartbeat: current.Heartbeat, changedAt: now}
			continue
		}
		if r.driving(current.ID) || !sameHeartbeat(seen.heartbeat, current.Heartbeat) {
			seen.heartbeat, seen.changedAt, seen.marked = current.Heartbeat, now, false
			continue
		}
		wait := r.cfg.TTL
		if seen.marked {
			wait = runDriverBeat(r.cfg.TTL)
		}
		if ctx.Err() != nil || now.Sub(seen.changedAt) < wait {
			continue
		}
		if err := r.advance(ctx, current, seen, now); err != nil {
			log.Error("run recovery failed; a later sweep retries",
				"job_id", current.JobID, "run_id", current.ID, "error", err)
		}
	}
	for id := range r.observed {
		if _, still := running[id]; !still {
			delete(r.observed, id)
		}
	}
}

func sameHeartbeat(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// advance takes a run whose heartbeat stopped changing one step closer to an
// outcome. A swap that loses needs no handling: the next sweep sees the value
// that beat it and restarts the wait.
func (r *runRecovery) advance(ctx context.Context, current run.RunHeartbeat, seen *runObservation, now time.Time) error {
	if !seen.marked {
		mark, err := r.cfg.Store.ReplaceRunHeartbeat(ctx, current.ID, seen.heartbeat)
		if err != nil || mark == nil {
			return err
		}
		seen.heartbeat, seen.changedAt, seen.marked = mark, now, true
		return nil
	}

	settle := seen.adoptions >= maxRunAdoptions
	if !settle && normalizeExecutionMode(r.cfg.ExecutionMode) == executionModeLocal {
		settled, err := r.cfg.Store.RunTasksSettled(ctx, current.ID)
		if err != nil {
			return err
		}
		settle = !settled
	}
	if settle {
		return r.finalize(ctx, current, seen)
	}

	claim, err := r.cfg.Store.ReplaceRunHeartbeat(ctx, current.ID, seen.heartbeat)
	if err != nil || claim == nil {
		return err
	}
	seen.heartbeat, seen.changedAt, seen.marked = claim, now, false
	seen.adoptions++
	log.Warn("adopting a run whose driver stopped", "job_id", current.JobID, "run_id", current.ID, "adoption", seen.adoptions)
	return r.cfg.Adopt(ctx, current.JobID, current.ID)
}

func (r *runRecovery) finalize(ctx context.Context, current run.RunHeartbeat, seen *runObservation) error {
	finalized, err := r.cfg.Store.FinalizeLostRun(current.ID, seen.heartbeat)
	if err != nil || !finalized {
		return err
	}
	log.Warn("failed a run whose driver stopped before it finished",
		"job_id", current.JobID, "run_id", current.ID, "adoptions", seen.adoptions)
	// No engine remains to do this, and only the call that finalized may.
	if err := r.cfg.DispatchCallbacks(context.WithoutCancel(ctx), current.JobID, current.ID, run.ErrRunDriverLost); err != nil {
		log.Error("callback dispatch failure", "job_id", current.JobID, "run_id", current.ID, "error", err)
	}
	return nil
}
