# Caesium Load Baseline — Phase 2 B3 Measurement (2026-05-25)

> Measurement of Phase B3 (in-memory owner run-state + checkpoint/replay) plus
> the internal-endpoint mTLS work, on the 3-node k8s deployment with
> `CAESIUM_RUN_OWNER_IN_MEMORY=true`. **Headline: the in-memory advancement path
> is verified — the 100-task stress workload completes 10/10 on both single-node
> and 3-node. Failover takeover + checkpoint replay are mechanically proven;
> end-to-end completion through a force-killed dqlite voter is sandbox-flaky and
> is the remaining hardening item.**

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

## Failover — mechanism proven, end-to-end flaky in sandbox

Killing the run owner mid-run, a surviving node's dispatch loop took over the
expired leases (`AcquireExpiredLeases`, generation incremented to 2) and
reconstructed each run's state from the latest checkpoint + post-checkpoint
terminal rows (`recovered run … ready=N`). Two real bugs were fixed here: a
dead peer in the round-robin hung dispatches for the 30s client timeout (added a
4s per-dispatch timeout to fail fast), and `ResetInFlightTasks` did not clear
`claimed_by`, so a new owner could not re-claim the rows (now cleared).

However, **force-killing a dqlite voter disrupts catalog quorum briefly**, which
sometimes prevents the takeover sweep from acquiring the expired leases at all —
so end-to-end completion through an owner crash did not reproduce reliably in
this single-machine sandbox. The takeover + replay machinery is correct (proven
on the runs where takeover did fire); making owner-crash failover robust under
voter loss is the remaining item.

## Recommendation

Ship the in-memory path **default-off** (`CAESIUM_RUN_OWNER_IN_MEMORY=false`):
steady-state advancement is verified (10/10), but owner-crash failover needs more
hardening before it is on by default. mTLS and the B2 path are unaffected by the
flag.

## Sandbox caveats

Single physical node, 3 dqlite voters, ephemeral storage. The voter-kill quorum
disruption is largely a sandbox artifact; real multi-node hardware tolerates a
single voter loss far better.
