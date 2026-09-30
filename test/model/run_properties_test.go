package model_test

import (
	"testing"

	"github.com/caesium-cloud/caesium/test/model"
	"pgregory.net/rapid"
)

// TestRunLifecycleProperties drives a generated bounded DAG through a generated
// sequence of admission, dispatch, completion, cancellation, retry, worker
// claim expiry, owner takeover and checkpoint/recovery, checking the safety and
// liveness oracles after every single step.
//
// Checking after every step rather than at the end is the point. The defects
// this is aimed at — a fan-in released by one member of a predecessor group, a
// group dispatched past its parallelism cap, a consumer stranded downstream of
// a failure — are all transient states that a final-state assertion would walk
// straight past.
func TestRunLifecycleProperties(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dag := model.DAGGen(model.FannedConfig()).Draw(t, "dag")
		run, err := model.New("run-1", "job-1", dag)
		if err != nil {
			t.Fatalf("admission rejected a generated DAG: %v", err)
		}

		leases := model.NewLeaseTable()
		now := int64(1_000)
		const ttl = int64(30_000)
		if _, fresh := leases.Acquire("run-1", "node-a", now, ttl); !fresh {
			t.Fatalf("first acquisition must create the lease")
		}
		owner := "node-a"
		admitted := run.Checkpoint()
		epochStart := run.Checkpoint()

		invariants := func(t *rapid.T) {
			if err := model.CheckSafety(run); err != nil {
				t.Fatalf("%v", err)
			}
			if err := model.CheckLiveness(run); err != nil {
				t.Fatalf("%v", err)
			}
			if err := leases.CheckLeaseSafety(); err != nil {
				t.Fatalf("%v", err)
			}
			// A retry may reopen terminal work, but it must never replace the
			// identity acknowledged at admission. Keep that original baseline
			// even when epochStart moves after a legal retry.
			if err := model.CheckNoTerminalRegression(admitted, run.Checkpoint()); err != nil {
				t.Fatalf("%v", err)
			}
			if err := model.CheckNoTerminalRegression(epochStart, run.Checkpoint()); err != nil {
				t.Fatalf("%v", err)
			}
		}

		t.Repeat(map[string]func(*rapid.T){
			"": invariants,

			"advance_clock": func(t *rapid.T) {
				now += int64(rapid.IntRange(1, 20_000).Draw(t, "ms"))
			},

			"dispatch": func(t *rapid.T) {
				ready := run.Ready()
				if len(ready) == 0 {
					return
				}
				id := rapid.SampledFrom(ready).Draw(t, "instance")
				if !run.Dispatch(id, owner, now+ttl) {
					t.Fatalf("%s was reported ready but refused dispatch", id)
				}
			},

			"start": func(t *rapid.T) {
				running := run.Running()
				if len(running) == 0 {
					return
				}
				run.MarkStarted(rapid.SampledFrom(running).Draw(t, "instance"))
			},

			"complete": func(t *rapid.T) {
				running := run.Running()
				if len(running) == 0 {
					return
				}
				id := rapid.SampledFrom(running).Draw(t, "instance")
				outcome := model.OutcomeGen().Draw(t, "outcome")
				res := run.Complete(id, outcome, run.Generation())
				if res.Refused != model.RefusalNone {
					// A cancelled run refuses new outcomes; that is the only
					// legal refusal for an in-flight instance at the current
					// generation.
					if res.Refused != model.RefusalRunTerminal {
						t.Fatalf("completing in-flight %s was refused as %q", id, res.Refused)
					}
					return
				}
				if !res.Applied {
					t.Fatalf("completing in-flight %s applied nothing", id)
				}
				if res.Sequence == 0 || !res.Durable() {
					t.Fatalf("completing %s produced a non-durable result %+v", id, res)
				}
			},

			"duplicate_complete": func(t *rapid.T) {
				// A worker re-POSTs an identical completion whenever the owner
				// answers with a retryable error, so a repeat delivery is a
				// normal event, not an anomaly. It must replay the first
				// delivery's durable rows and advance nothing.
				journal := run.Journal()
				if len(journal) == 0 {
					return
				}
				rec := rapid.SampledFrom(journal).Draw(t, "record")
				before := run.Checkpoint()
				res := run.Complete(rec.Instance, rec.Status, run.Generation())
				after := run.Checkpoint()
				if res.Applied {
					t.Fatalf("a repeat completion of %s advanced the run", rec.Instance)
				}
				if err := model.CheckRefusalInert(before, after); err != nil {
					t.Fatalf("repeat completion of %s: %v", rec.Instance, err)
				}
				if res.Refused == model.RefusalNone && res.Sequence != 0 && res.Sequence != rec.Sequence {
					t.Fatalf("repeat completion of %s replayed sequence %d, first delivery stamped %d",
						rec.Instance, res.Sequence, rec.Sequence)
				}
			},

			"stale_complete": func(t *rapid.T) {
				// DT-COMPLETE-01: a completion carrying a superseded owner
				// generation is refused and mutates nothing.
				running := run.Running()
				if len(running) == 0 || run.Generation() <= 1 {
					return
				}
				id := rapid.SampledFrom(running).Draw(t, "instance")
				stale := rapid.Int64Range(0, run.Generation()-1).Draw(t, "stale_generation")
				before := run.Checkpoint()
				res := run.Complete(id, model.StatusSucceeded, stale)
				after := run.Checkpoint()
				if res.Refused != model.RefusalStaleGeneration {
					t.Fatalf("generation %d against current %d was not refused as stale (got %q)",
						stale, run.Generation(), res.Refused)
				}
				if err := model.CheckRefusalInert(before, after); err != nil {
					t.Fatalf("%v", err)
				}
			},

			"expire_worker_claim": func(t *rapid.T) {
				running := run.Running()
				if len(running) == 0 {
					return
				}
				id := rapid.SampledFrom(running).Draw(t, "instance")
				attemptBefore := run.AttemptOf(id)
				if !run.ExpireClaim(id, now) {
					return
				}
				if got, _ := run.StatusOf(id); got != model.StatusPending {
					t.Fatalf("a reaped claim left %s in %s, want pending", id, got)
				}
				if run.AttemptOf(id) != attemptBefore+1 {
					t.Fatalf("a reaped claim left %s at attempt %d, want %d",
						id, run.AttemptOf(id), attemptBefore+1)
				}
			},

			"owner_takeover": func(t *rapid.T) {
				// DT-OWNER-01: takeover requires an EXPIRED lease and strictly
				// increases the generation.
				lease, ok := leases.Get("run-1")
				if !ok {
					return
				}
				takeoverAt := lease.ExpiresAtMs + 1
				if takeoverAt > now {
					now = takeoverAt
				}
				next := "node-b"
				if owner == "node-b" {
					next = "node-a"
				}
				before := lease.Generation
				taken := leases.AcquireExpired(next, now, ttl)
				if len(taken) != 1 {
					t.Fatalf("an expired lease was not taken over: %v", taken)
				}
				after, _ := leases.Get("run-1")
				if after.Generation != before+1 {
					t.Fatalf("takeover moved the generation %d -> %d", before, after.Generation)
				}
				owner = next
				run.AdoptGeneration(after.Generation)
			},

			"renew_lease": func(t *rapid.T) {
				leases.RenewOwned(owner, now, now+ttl)
			},

			"checkpoint_recover": func(t *rapid.T) {
				// DT-RECOVER-01: a checkpoint plus the post-checkpoint terminal
				// tail reconstructs the state the owner held, and in-flight work
				// with no terminal row is re-dispatched rather than lost.
				snap := run.Checkpoint()
				tail := run.TailSince(snap.SequenceHigh)
				recovered, res, err := model.Recover(dag, run.Acknowledged(), &snap, tail)
				if err != nil {
					t.Fatalf("recovery failed: %v", err)
				}
				inFlight := run.Running()
				if len(res.ReDispatch) != len(inFlight) {
					t.Fatalf("recovery re-dispatches %v, owner had %v in flight", res.ReDispatch, inFlight)
				}
				if res.MaxSequence != run.Sequence() {
					t.Fatalf("recovery observed sequence %d, owner had %d", res.MaxSequence, run.Sequence())
				}
				if len(res.SequenceGaps) != 0 {
					t.Fatalf("a complete journal produced gaps %v", res.SequenceGaps)
				}
				for _, id := range dag.AllInstances() {
					want, _ := run.StatusOf(id)
					got, _ := recovered.StatusOf(id)
					if model.Terminal(want) && got != want {
						t.Fatalf("recovery reconstructed %s as %s, owner held %s", id, got, want)
					}
				}
				if err := model.CheckSafety(recovered); err != nil {
					t.Fatalf("recovered state violates safety: %v", err)
				}
			},

			"cancel": func(t *rapid.T) {
				// DT-CANCEL-01: work that has not started is resolved; work that
				// has started is left to reach its own terminal state.
				started := map[model.InstanceID]bool{}
				for _, id := range run.Running() {
					if c, ok := run.ClaimOf(id); ok && c.Started {
						started[id] = true
					}
				}
				run.Cancel()
				for id := range started {
					if got, _ := run.StatusOf(id); got != model.StatusRunning {
						t.Fatalf("cancel resolved started work %s to %s", id, got)
					}
				}
				for _, id := range dag.AllInstances() {
					got, _ := run.StatusOf(id)
					if got == model.StatusPending {
						t.Fatalf("cancel left %s pending", id)
					}
				}
			},

			"retry": func(t *rapid.T) {
				// DT-RETRY-01: succeeded work is retained, everything else is
				// reset. Reset is a legal terminal regression, which is why the
				// non-regression oracle is epoch-scoped and the baseline moves.
				retained := map[model.InstanceID]model.TaskStatus{}
				for _, id := range dag.AllInstances() {
					if s, _ := run.StatusOf(id); model.Succeeded(s) {
						retained[id] = s
					}
				}
				if !run.Retry() {
					return
				}
				if err := model.CheckNoTerminalRegression(admitted, run.Checkpoint()); err != nil {
					t.Fatalf("retry changed admission identity: %v", err)
				}
				for id, want := range retained {
					if got, _ := run.StatusOf(id); got != want {
						t.Fatalf("retry lost retained %s: %s became %s", id, want, got)
					}
				}
				for _, id := range dag.AllInstances() {
					s, _ := run.StatusOf(id)
					if _, kept := retained[id]; kept {
						continue
					}
					if s != model.StatusPending && !model.Terminal(s) {
						t.Fatalf("retry left %s in %s", id, s)
					}
					if s == model.StatusPending && run.AttemptOf(id) != 1 {
						t.Fatalf("retry left %s at attempt %d, want 1", id, run.AttemptOf(id))
					}
				}
				epochStart = run.Checkpoint()
			},
		})

		// A run that reached a terminal state must have every identity terminal
		// too, unless cancellation deliberately left started work alive.
		if model.IsTerminalRun(run.Status()) && run.Status() != model.RunCancelled && !run.IsComplete() {
			t.Fatalf("run settled %s with work outstanding", run.Status())
		}
	})
}

// TestDrainedRunAlwaysTerminates is the liveness property stated as a closed
// experiment: from any generated state, completing every dispatchable instance
// until nothing is left must terminate the run.
//
// It catches exactly the shape that hides behind a CI timeout — a run with
// nothing in flight, nothing ready and nothing terminal — and it reports the
// stranded identities rather than the elapsed seconds.
func TestDrainedRunAlwaysTerminates(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dag := model.DAGGen(model.FannedConfig()).Draw(t, "dag")
		run, err := model.New("run-1", "job-1", dag)
		if err != nil {
			t.Fatalf("admission rejected a generated DAG: %v", err)
		}
		outcomes := rapid.SliceOfN(model.OutcomeGen(), 1, 24).Draw(t, "outcomes")

		// Bounded: every step either resolves at least one identity or the loop
		// exits, so the DAG size is the budget.
		budget := 4 * (len(dag.AllInstances()) + 1)
		for step := 0; step < budget; step++ {
			if run.IsComplete() {
				break
			}
			ready := run.Ready()
			if len(ready) == 0 {
				if len(run.Running()) == 0 {
					t.Fatalf("stranded at step %d: %v", step, model.CheckLiveness(run))
				}
				// Drain in-flight work oldest-first.
				id := run.Running()[0]
				run.Complete(id, outcomes[step%len(outcomes)], run.Generation())
				continue
			}
			id := ready[0]
			run.Dispatch(id, "node-a", 60_000)
			run.Complete(id, outcomes[step%len(outcomes)], run.Generation())
			if err := model.CheckSafety(run); err != nil {
				t.Fatalf("step %d: %v", step, err)
			}
		}
		if !run.IsComplete() {
			t.Fatalf("run did not terminate within %d steps: %v", budget, model.CheckLiveness(run))
		}
		if run.Status() == model.RunRunning {
			t.Fatalf("a complete run is still reported running")
		}
	})
}
