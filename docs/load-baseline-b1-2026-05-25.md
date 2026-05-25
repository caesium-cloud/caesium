# Caesium Load Baseline — B0+B1 Measurement (2026-05-25)

> Measurement after PR #175 (ClaimNext run-lease deferral) and PR #176 (global
> dqlite retry) merged. **Headline finding: B1's deferral works exactly as
> designed — and in doing so it exposed that the push-dispatch path executes
> nothing. The entire receiving half of run-owner mode (execute dispatched
> task → report completion) was never built; in A2 it only appeared to work
> because ClaimNext was silently doing all the real execution. With ClaimNext
> deferring, owner-mode runs stall at their root task and time out.**

## Environment

3-pod k8s (Docker Desktop), `CAESIUM_EXECUTION_MODE=distributed`,
`CAESIUM_RUN_OWNER_ENABLED=true`, `CAESIUM_INTERNAL_WAKEUP_TOKEN` set,
kubernetes engine, ephemeral storage. Commit `11ff432` (master with #175 + #176).
Workload: 10 jobs × fan-out=4 × depth=3 = 100 tasks, 500ms, 5 concurrent —
identical to the #170 / #172 / #174 stress runs.

## Result

| Metric | Value |
|---|---|
| Runs succeeded | **0 / 10** |
| Runs failed | 2 |
| Runs timed out | **8** |
| Total run time | 1h0m (harness 30-min per-run poll cap hit) |
| `caesium_worker_claims_total` | **0** |
| `caesium_dispatch_sent_total` | **7** (across the whole hour) |
| `caesium_run_leases_owned` (caesium-0) | 10 |
| `task_run_status` writes | 3 |
| `db_busy_retries` | 4 |

caesium-0 owned all 10 runs; caesium-1/2 owned 0 and dispatched 0.

## What each number means

- **`claims_total = 0`**: B1's ClaimNext deferral works perfectly — ClaimNext correctly steps aside for every run that has a live lease. No double-claiming, no race. This part is *correct*.
- **`dispatch_sent = 7`, `task_run_status = 3`, `0 succeeded`**: the dispatch loop dispatched ~7 root tasks across an hour, almost none completed, and no DAG advanced past its root. Runs sat until the harness's 30-minute poll gave up.

## Root cause: the push path claims but never executes

Confirmed by code inspection, not just logs:

1. **`internal/dispatch/dispatch.go` `HandleDispatch`** calls `store.ClaimTaskForDispatch` (transitions the task to `running`, `claimed_by = worker`) and returns `202 Accepted`. **It never submits the task to the runtime executor.** There is no executor wiring anywhere in `internal/dispatch/`.
2. **`PostComplete`** (the worker→owner "task done" RPC) is *defined* but has **zero callers** in production code. Nothing ever reports a dispatched task's completion.
3. The **`/internal/complete` handler** is fully implemented (fence checks, applies result, advances DAG) but is **never invoked**, because step 2 never fires.

So a dispatched task is marked `running` and then orphaned — no process executes it, no completion is ever reported, `outstanding_predecessors` on its successors never decrements, and the run stalls forever.

### Why A2 appeared to work and B1 exposed this

In Phase A2 (PR #173, before deferral), the worker's pull loop (`ClaimNext` → execute → `CompleteTask`) was running concurrently. ClaimNext claimed and *executed* the tasks; the dispatch loop's claims were mostly redundant (and lost the race, per the #174 finding). The pull path carried the entire execution workload — the push path's missing executor was invisible.

PR #175's deferral made ClaimNext skip live-owned runs. That removed the crutch. With nothing executing dispatched tasks, owner-mode execution collapses to zero throughput.

**This is not a regression in #175 or #176** — both are correct in isolation, and owner mode is default-OFF so production is unaffected. It's the surfacing of a pre-existing gap: the push-execution path was never built end-to-end.

## Implications for Phase B

Phase B is materially larger than "in-memory DAG state." The execution path itself must be built. The minimum functional owner-mode cycle requires:

1. **Receiving-side execution.** `HandleDispatch` (or a queue it feeds) must hand the claimed task to the worker's existing runtime executor — the same execution path `ClaimNext`'d tasks use today (`internal/worker/runtime_executor.go` / the worker pool).
2. **Completion reporting.** After the worker finishes executing, it must call `dispatch.PostComplete` to the owner with the result/outputs/branch-selections (the wiring that's defined but never called).
3. **Owner advances the DAG.** `/internal/complete` already applies the result and advances predecessors — once steps 1–2 feed it, the cycle closes.

In-memory owner state (the original B2 framing) is an *optimization on top of* a working push-execution cycle — but the cycle has to exist first. Recommend re-scoping Phase B around the execution path, with the in-memory state as a later layer.

## Recommendation

1. **Keep `CAESIUM_RUN_OWNER_ENABLED` default-off** — it's non-functional for execution until the receiving half is built. (It already defaults off; no production exposure.)
2. **Next implementation step (call it B2):** wire the dispatch→execute→complete cycle — `HandleDispatch` submits to the runtime executor; the executor's completion calls `PostComplete`; verify a DAG runs end-to-end under owner mode. Measure again before adding in-memory state.
3. **Process note:** the four measurement cycles (A, A2, A2+token, B1) each peeled back one layer of the push path's incompleteness. A targeted code read ("does HandleDispatch execute?") would have caught this at A2. For the remaining owner-mode work, pair every measurement with a code-path trace of the full lifecycle, not just the metric deltas.

## Sandbox caveats

Single physical node, 3 voters, ephemeral storage. But the finding is structural (no executor wiring; `PostComplete` uncalled) and independent of the sandbox — it reproduces regardless of cluster size or load.
