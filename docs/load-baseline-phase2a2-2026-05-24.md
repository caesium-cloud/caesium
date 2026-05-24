# Caesium Load Baseline — Phase 2 Phase A2 Measurement (2026-05-24)

> Measurement of PR #173 (the executor-side dispatch loop) on the 3-node k8s
> deployment. **Headline finding: the DB-polling dispatch loop races ClaimNext
> and loses, so dispatch almost never fires; and its per-tick polling adds
> enough read pressure to destabilize dqlite on a constrained cluster. A2 as
> built does not deliver the contention win — the fix requires ClaimNext to
> defer to the owner for owned runs, which is Phase B territory.**

## Environment

Same as [load-baseline-phase2a-2026-05-24.md](load-baseline-phase2a-2026-05-24.md):
3-pod StatefulSet on Docker Desktop k8s, `CAESIUM_EXECUTION_MODE=distributed`,
`CAESIUM_RUN_OWNER_ENABLED=true`, kubernetes engine, ephemeral storage,
commit `6ac4735` (PR #173 merged). **This time `CAESIUM_INTERNAL_WAKEUP_TOKEN`
is set** — without it, dispatch silently 401s (see Finding 0).

## Finding 0 — dispatch endpoints are token-gated (operational gotcha)

The first run was misconfigured: `CAESIUM_INTERNAL_WAKEUP_TOKEN` was unset.
The dispatch `Handler.authorized()` returns false when the token is empty, so
**every** `/internal/dispatch` POST returned 401, which `PostDispatch` counts
as `worker_rejected`. Result: `dispatch_sent_total = 0`,
`dispatch_rejected_total{worker_rejected} = 130`, and `db_busy_retries = 116`
(worse than Phase A's 101, because the dispatch loop's polling overhead piled
on with zero dispatch benefit).

**Operational takeaway:** run-owner mode requires `CAESIUM_INTERNAL_WAKEUP_TOKEN`
on every node, exactly like distributed wakeups. Startup currently only warns
about missing mTLS — it should *also* warn (or refuse to start) when owner mode
is on without a wakeup token, since dispatch is silently inert otherwise. This
is a concrete follow-up.

## Finding 1 — dispatch races ClaimNext and loses

With the token set and the cluster healthy, dispatch *can* succeed —
`dispatch_sent_total` climbed from 0 to 6. But it then **stayed at 6** across a
subsequent gentle 2-run workload that completed 2/2 successfully. Those 2 runs
went through ClaimNext entirely; the dispatch loop dispatched none of their
tasks.

Root cause: ClaimNext is wakeup-driven (sub-second latency, per the locking-fix
Phase 2 work), while the dispatch loop polls every `CAESIUM_RUN_OWNER_DISPATCH_INTERVAL`
(default 1s). When a task becomes ready, ClaimNext almost always claims it
before the dispatch loop's next tick. A2 added the dispatch path but **never
made ClaimNext defer to the owner for owned runs** — both paths run
concurrently, and the lower-latency one (ClaimNext) wins the race.

So the dispatch loop is mostly dead weight: it adds per-tick DB polling
(`OwnedRunsWithGenerations` + `PendingTasksForDispatch` per owned run) without
displacing the claim contention it was meant to replace.

## Finding 2 — polling pressure destabilizes dqlite under load

The stress workload (100 tasks, 10 jobs, fan-out=4, depth=3, 500ms, 5 concurrent)
with dispatch enabled produced **5/10 failed runs** and a p99 of **5m47s**
(vs. 31s in every prior config). Pod logs show the failures are dqlite
`"checkpoint in progress"` / poisoned-connection errors — the same class PR #162
addressed — re-triggered by the added read pressure from the dispatch loop's
1s polling stacked on the existing write contention.

| Config | Runs OK | p99 | db_busy_retries | dispatch_sent |
|---|---|---|---|---|
| Phase 1 only (#170) | 10/10 | 32s | 65 | n/a |
| Phase A substrate (#172) | 10/10 | 31s | 101 | n/a (no loop) |
| A2, no token | (stress) | — | 116 | 0 (all 401) |
| A2, token, stress | **5/10** | **5m47s** | 52* | 6 |
| A2, token, gentle | 2/2 | 16s | 2 | 6 (unchanged) |

\* The stress-run retry count is lower only because half the runs died early on
checkpoint errors, so fewer writes were attempted — not because contention
improved.

## Conclusions

1. **A2's DB-polling dispatch loop is the wrong shape.** Racing ClaimNext with a
   1s poll means dispatch loses almost every race, so the substrate's promised
   contention reduction never materializes — and the poll itself adds load.

2. **The missing ingredient is ClaimNext deferral.** For dispatch to take over,
   ClaimNext must skip tasks belonging to runs owned by another live node (let
   the owner push them). That requires the worker's claim query to join against
   `run_leases` — and to avoid the per-tick DB poll, the owner should hold its
   ready-task set in memory. Both are squarely **Phase B** (in-memory DAG
   state), not a small A3 tweak.

3. **The sandbox confounds the contention story.** 3 dqlite voters on one
   machine with ephemeral storage is fragile; the checkpoint cascade is partly a
   sandbox artifact. But Finding 1 (dispatch loses the race) is structural and
   reproduces regardless of cluster size.

## Recommendation

**Stop iterating on the DB-polling dispatch loop; proceed to Phase B.** Phase B's
design already calls for:
- In-memory DAG state on the owner (no per-tick DB poll — eliminates Finding 2's
  added read pressure).
- The owner dispatching from its in-memory ready set (no race with ClaimNext for
  owned runs).
- ClaimNext deferral for owned runs (the worker claim query checks `run_leases`
  and skips runs owned by a live peer), with ClaimNext remaining the recovery
  path for orphaned/owner-dead runs.

Before starting Phase B, ship two small fixes surfaced here:
1. **Startup guard**: when `CAESIUM_RUN_OWNER_ENABLED=true` and
   `CAESIUM_INTERNAL_WAKEUP_TOKEN` is empty, log a loud warning (dispatch is
   inert) — mirror the existing mTLS warning.
2. **Default-off remains correct**: A2's loop should not be enabled in any
   real deployment until Phase B lands, since on its own it only adds overhead.

## Sandbox caveats

Single physical node, 3 dqlite voters, ephemeral storage, 100-task workload.
Finding 2 (checkpoint instability) is partly sandbox-specific. Findings 0 and 1
are structural and independent of the sandbox.
