# Caesium Load Baseline — Phase 2 Phase B2 Measurement (2026-05-24)

> Measurement of Phase B2 (the dispatch→execute→complete cycle) on the 3-node
> k8s deployment. **Headline finding: B2 closes the run-owner execution loop —
> the 100-task stress workload went from B1's 0/10 to a stable 10/10 across
> three consecutive runs. Reaching 10/10 required hardening two run-owner paths
> against transient dqlite contention; both fixes are validated firing under
> real load.**

## Environment

Same 3-pod StatefulSet on Docker Desktop k8s as the prior phase docs:
`CAESIUM_EXECUTION_MODE=distributed`, `CAESIUM_RUN_OWNER_ENABLED=true`,
`CAESIUM_INTERNAL_WAKEUP_TOKEN` set, kubernetes engine, ephemeral storage.
Branch `phase2-b2-dispatch-execute-complete` (base commit `b00c869` plus the two
contention fixes below). Standard workload: 10 jobs × fan-out=4 × depth=3 ×
500ms task duration × concurrency=5 = 100 tasks per run.

## Finding 0 — B2's cycle works; first stress run was 8/10

The B2 commit (`b00c869`) wired the cycle: the owner pushes tasks via
`/internal/dispatch`, the worker executes on its existing pool, and reports the
outcome back via `/internal/complete` (the `CompletionSink` routing
ClaimNext'd tasks to the local store and dispatched tasks to the owner). A
2-job smoke test passed 2/2, and the first 100-task stress run scored **8/10** —
already a step change from B1's **0/10** (B1 deferred ClaimNext to the owner but
the owner executed nothing; B2 supplies the missing execution).

## Finding 1 — lost completions on owner-side contention (fixed)

The 2 failures in the 8/10 run traced to the owner's completion apply
(`CompleteTaskClaimed`) hitting `"database is locked"` under the burst of
concurrent completions. That error is not `ErrTaskClaimMismatch`, so
`HandleComplete` fell through to a generic **409**, and the worker treated 409
as a terminal fence rejection — dropping a completion for a task that had
actually succeeded. The run then stalled and failed.

**Fix:** distinguish transient contention from a real fence violation.
- Owner: on a `dqlite.IsContentionError` apply failure, `HandleComplete` now
  returns **503** (retryable) via `rejectRetryable`, not 409, and records
  `caesium_complete_retryable_total{reason="contention"}`.
- `PostComplete` surfaces 503 as the `ErrOwnerBusy` sentinel.
- Worker `ownerSink.send` retries on `ErrOwnerBusy` with bounded backoff
  (~1.55s across 6 tries) before giving up; an exhausted retry is recorded as
  `caesium_complete_report_failed_total{reason="owner_busy"}`.

## Finding 2 — run-start fails the whole run on a contention blip (fixed)

After Finding 1's fix, a re-run scored **7/10** with **zero** completion-failure
metrics — a *different* cause. All 3 failures were runs that died ~30ms after
start with `total_tasks=0` and `error: "checkpoint in progress"`, clustered in a
~45ms window at the start of the workload (a WAL checkpoint was in progress when
the first concurrent batch fired).

Root cause: the run-start / DAG-materialization reads (`atomService.Get`,
`taskEdgeService.List`, `taskService.List`, `store.Get`) fail the *entire run*
on a transient contention error. The global connection-pool retry (PR #176)
misses it because dqlite can surface `"checkpoint in progress"` during **row
iteration**, after `QueryContext` has already returned cleanly — so the
pool-layer wrapper never sees it. The 30ms-to-failure timing confirmed no retry
layer engaged.

**Fix:** `retryOnContention` (internal/job) wraps the five idempotent run-start
reads with a bounded backoff (~630ms across 6 tries) keyed on
`dqlite.IsContentionError`. These reads have no side effects, so re-running them
is safe. (Also fixed a latent `err`-scoping reference at job.go:1071 that was
always `nil` in practice.)

## Finding 3 — duplicate execution was a symptom of Finding 1, not a separate bug

The 7/10 run showed tasks executing 2–6×, which looked like duplicate dispatch.
With both fixes in place, `caesium_dispatch_sent_total` is **exactly** 100 per
run (300 across three runs) — every task dispatched once, no re-dispatch. The
earlier duplication was downstream of lost completions: a dropped completion
left a task looking un-terminal, its claim lease expired, and recovery
re-executed it. Fixing the completion path (Finding 1) removed the churn.

## Results

| Config | Runs OK | dispatch_sent | complete_retryable | complete_report_failed |
|---|---|---|---|---|
| B1 (#175) | 0/10 | n/a (owner executed nothing) | — | — |
| B2 base (`b00c869`) | 8/10 | ~104 | n/a (metric not yet added) | post_error (lost) |
| B2 + completion fix | 7/10 | 70 | 0 | 0 (ran-start failures instead) |
| **B2 + both fixes** | **10/10 ×3** | **100/run (300 total)** | **8 (all recovered)** | **0** |

End-to-end p50 ≈ 17s, p99 ≈ 31s (unchanged from the healthy baselines). The 8
`complete_retryable` events across the three 10/10 runs are direct evidence the
completion-503 path fires under real contention and the worker retry recovers
every one — zero lost completions, zero fence rejections.

## Conclusions

1. **B2 delivers run-owner execution.** 0/10 → a stable 10/10. The owner is the
   single writer for its run's coordination rows, and dispatched tasks report
   back over `/internal/complete` with the owner-generation fence intact.

2. **Contention hardening was the gating work, not the cycle itself.** Both
   failure modes were transient dqlite contention escaping a retry layer (one on
   the completion apply, one on run-start reads surfacing at iteration time).
   The cycle mechanism was correct from `b00c869`.

3. **`CAESIUM_RUN_OWNER_ENABLED` stays default-off.** B2 is verified in the
   sandbox but mTLS for the internal endpoints is still warn-only; owner mode
   should not be enabled in a real deployment until that lands.

## Sandbox caveats

Single physical node, 3 dqlite voters, ephemeral storage. The checkpoint
contention is more frequent here than on dedicated hardware would be — which
makes it a useful stress test for the retry hardening, but the absolute failure
rates of the intermediate (8/10, 7/10) configs are partly sandbox-amplified. The
fixes are correctness improvements (no lost completions, no run killed by a
transient blip) that hold regardless of cluster size.
