# Caesium Load Baseline — Phase 2 B3 Measurement (2026-05-25)

> Measurement of Phase B3 (in-memory owner run-state + checkpoint/replay) plus
> the internal-endpoint mTLS work, on the 3-node k8s deployment with
> `CAESIUM_RUN_OWNER_IN_MEMORY=true`. **Headline: the in-memory advancement path
> is verified — the 100-task stress workload completes 10/10 on both single-node
> and 3-node. Owner-crash failover is now verified end-to-end: killing the run
> owner mid-run, a survivor takes over the lease, replays from checkpoint, and
> drives the run to completion (all tasks succeed). Two further failover bugs
> were found and fixed — takeover was gated behind peer discovery, and the claim
> fence rejected the new owner's generation — on branch
> `phase2-failover-hardening`.**

## Environment

3-pod StatefulSet on Docker Desktop k8s, `CAESIUM_EXECUTION_MODE=distributed`,
`CAESIUM_RUN_OWNER_ENABLED=true`, `CAESIUM_RUN_OWNER_IN_MEMORY=true`, internal
mTLS configured (CA + shared node cert as a Secret, dedicated `:8443` listener),
kubernetes engine, ephemeral storage. Branch
`phase2-b3-checkpoint-replay-and-internal-mtls`.

## mTLS — verified

The dedicated internal mTLS listener came up on every node, and owner→worker
dispatches reached `HandleDispatch` over mutual TLS (handshakes succeed
pod-to-pod, using the dynamic-IP chain-verify-but-skip-hostname client config).
Run-owner mode hard-fails at startup without all three `CAESIUM_INTERNAL_MTLS_*`
files.

## The stall bug (found + fixed)

First in-memory runs stalled: roots dispatched and succeeded, but successors were
never dispatched. The owner's terminal write stamped `terminal_sequence` and the
DAG advanced **in memory** — but `ClaimTaskForDispatch` (run by the worker on
receipt of a dispatch) required `outstanding_predecessors = 0` in the DB. Because
the owner advances in memory and deliberately does *not* decrement the DB
counter, an in-memory-ready successor still showed `outstanding > 0` in the DB,
so its claim was rejected (409) and the loop spun re-dispatching it.

**Fix:** the owner is authoritative for readiness, so `ClaimTaskForDispatch`
takes a `trustOwnerReadiness` flag (set in in-memory mode) that drops the
`outstanding_predecessors = 0` predicate. In SQL mode the check is unchanged
(the owner only dispatches `outstanding = 0` tasks there anyway).

## Result — in-memory advancement verified

| Config | Runs OK |
|---|---|
| single-node, in-memory | 10/10 |
| 3-node, in-memory | 10/10 |

Per-run logs confirm correct DAG advancement: a completion advances the
in-memory state, stamps a monotonic `terminal_sequence`, readies successors, and
finalizes the run when the DAG completes — with terminal-only DB writes (no
per-transition predecessor UPDATEs) and periodic `run_checkpoints`.

## Failover — verified end-to-end

Killing the run owner mid-run, a surviving node's dispatch loop takes over the
expired lease (`AcquireExpiredLeases`, generation incremented to 2), reconstructs
the run's state from the latest checkpoint + post-checkpoint terminal rows
(`recovered run … ready=N`), re-dispatches the task that was in-flight when the
owner died, and drives the DAG to completion.

Four bugs were found and fixed to get here:

1. **Dead-peer dispatch hang** — a dead peer in the round-robin hung dispatches
   for the 30s client timeout; added a 4s per-dispatch timeout to fail fast.
2. **`ResetInFlightTasks` left the claim** — it didn't clear `claimed_by`, so a
   new owner could not re-claim the reset rows; now cleared.
3. **Takeover gated behind peer discovery** (`phase2-failover-hardening`) — the
   expired-lease sweep ran *after* peer discovery, which is exactly what fails
   during the dqlite quorum disruption an owner crash causes, so the event that
   needed failover also blocked it. Moved the sweep to the top of the tick,
   independent of cluster membership.
4. **Claim fence rejected the new owner's generation** (`phase2-failover-hardening`)
   — after takeover the new owner re-queued the in-flight task but could never
   re-dispatch it: `ClaimTaskForDispatch` fenced on `(owner_generation = ? OR = 0)`,
   but an in-flight task carries the dead owner's generation N while the new owner
   claims at N+1, so the claim returned `task claim mismatch` (409) every tick and
   the run wedged behind that one task. Changed the fence to `owner_generation <= ?`
   (monotonic-generation invariant: claim tasks touched by this generation or any
   older one; reject only a row stamped by a *newer* generation). Guarded by
   `TestFailover_TakeoverAndResume`, which fails on the old fence and passes on the new.

**End-to-end run (3-node, hardened image):** owner killed mid-flight with 1 task
in-flight and 6 pending. The survivor took the lease ~10s later (= lease TTL),
recovered at generation 2, re-dispatched the in-flight task with zero rejections,
and completed all 8 tasks. No pod crashes; 2/3 quorum held throughout.

## Recommendation

Steady-state advancement (10/10) and owner-crash failover are both now verified,
with a deterministic unit test guarding the failover claim path.
`CAESIUM_RUN_OWNER_IN_MEMORY=true` is now a viable default-on candidate.

The one robustness follow-up — after the owner pod restarts with a new IP, peer
discovery still lists the dead node's old IP, so dispatch attempts wasted ~4s on
a dead peer (no stall; the round-robin still reached live workers, but failover
was slower than it needs to be) — is **addressed on `phase2-dispatch-peer-liveness`**:
the dispatch loop now circuit-breaks a peer that fails with a network error,
benching it for a cooldown so the round-robin skips it until it recovers. That is
unit-verified; a 3-node k8s confirmation of the breaker is the last step before
flipping the default on. mTLS and the B2 path are unaffected by the flag.

## Sandbox caveats

Single physical node, 3 dqlite voters, ephemeral storage. A **single** voter kill
keeps 2/3 quorum and now resumes cleanly end-to-end. Killing **multiple** different
voters across one session leaves several dead members referenced in the cluster,
dropping below quorum and stalling all writes (including takeover) — the earlier
"flaky" behavior was this cumulative-churn artifact compounded by the now-fixed
claim fence, not a failover-logic fault. Real multi-node hardware with stable
addressing tolerates single voter loss far better.
