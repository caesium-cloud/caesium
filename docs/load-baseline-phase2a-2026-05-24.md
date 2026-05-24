# Caesium Load Baseline — Phase 2 Phase A Measurement (2026-05-24)

> First empirical measurement of Phase 2 Phase A (PR #171) on the same
> 3-node k8s deployment used for the pre-Phase-A distributed baseline
> ([load-baseline-distributed-2026-05-24.md](load-baseline-distributed-2026-05-24.md)).
> **Headline finding: Phase A as merged is incomplete — the substrate
> (lease acquisition, lease renewal, dispatch/complete endpoints) is
> active, but the executor-side dispatch call was never wired up, so
> workers still pull via ClaimNext. Phase A in this state delivers a
> regression, not an improvement.**

## Environment

| Parameter | Value |
|---|---|
| Date | 2026-05-24 |
| Caesium commit | `8e484eb` (master with PR #171 merged) |
| Deployment | `helm upgrade caesium`, replicaCount=3, `CAESIUM_EXECUTION_MODE=distributed`, `CAESIUM_RUN_OWNER_ENABLED=true`, `kubernetes.engine.enabled=true`, `persistence.enabled=false` |
| Cluster | Docker Desktop Kubernetes (single physical node, 3 pods → 3 dqlite voters) |
| Engine | `kubernetes` (tasks run as Job pods in the same cluster) |
| Workload | 10 jobs × fan-out=4 × depth=3 = 100 task lifecycles, 500ms tasks, 5 concurrent runs (matches PR #170 stress test) |

## Side-by-side

| Metric | Pre-Phase-A (#170) | Phase A on | Delta |
|---|---|---|---|
| Total rows | 489 | 645 | **+32%** |
| Total statements | 311 | 455 | **+46%** |
| Rows/stmt (overall) | 1.6 | 1.4 | -12% |
| `task_run_status` rows | 195 | 276 | +42% |
| `event_insert` rows | 194 | 269 | +39% |
| `caesium_db_busy_retries_total` | 65 | **101** | **+56% MORE contention** |
| `caesium_run_leases_owned` | n/a | 10 | (Phase A only) |
| `caesium_run_lease_renewals_total` | n/a | 13 | (Phase A only) |
| `caesium_complete_rejected_total` | n/a | 0 | endpoints never called |
| End-to-end p50 | 31s | 31s | unchanged |
| End-to-end p99 | 32s | 31s | unchanged |
| Wall-clock run time | 67s | 66s | unchanged |
| Claims total | 42 | 63 | +50% (claimer is still the hot path) |

10/10 runs succeeded in both configurations.

## What's working

- **Lease acquisition fires correctly:** `caesium_run_leases_owned 10` shows the originating node took ownership of all 10 runs as they started.
- **Lease renewal works:** 13 batched renewal cycles during the run; the new gauge resets cleanly when the owned set empties (PR #171 fix verified).
- **mTLS warning fired at startup** as expected: *"run-owner mode is enabled without mTLS material configured; this is not a supported configuration for production use. Phase B will require CAESIUM_INTERNAL_MTLS_CA, CAESIUM_INTERNAL_MTLS_CERT, and CAESIUM_INTERNAL_MTLS_KEY."*
- **Both `/internal/dispatch` and `/internal/complete` endpoints respond** to manual probes (returning 401 without the bearer token, etc.).
- **Backwards compatibility preserved:** the harness's 10/10 runs succeeded.

## What's not working: the dispatch loop

The Phase A brief (and the design doc) called for the executor to actively push tasks via `/internal/dispatch` when owner mode is on, with ClaimNext kept only as a recovery fallback. **That wire-up was skipped.** The current code:

1. Acquires a `run_leases` row on `Start()` (works).
2. Renews leases via the per-node ticker (works).
3. Stamps `owner_generation` on `task_runs` it claims (works — but only at claim time, not dispatch time).
4. Has dispatch and complete handlers ready to receive (works — but no one calls them).
5. **Workers still pick up tasks via ClaimNext** because nothing replaced that loop.

Evidence:
- `caesium_complete_rejected_total = 0` across all reason labels — no completions ever arrived at the new endpoint.
- `caesium_worker_claims_total = 63` — workers are claiming via the existing path, just like pre-Phase-A.
- 10 `run_leases` rows + lease renewals are pure overhead with no compensating reduction in claim contention.

This explains the +56% busy_retries: Phase A added writes (lease INSERT + 13 batched UPDATEs + `owner_generation` column writes) without removing any of the existing claim/start/complete contention. The substrate works in isolation; the routing decision that would replace ClaimNext for owned runs is missing.

## Conclusions

1. **Phase A as merged is the substrate only.** It builds the foundation that Phase B's in-memory state and checkpoint machinery sit on, but it does not move the contention needle on its own — and in fact regresses it because of the added lease-management writes.

2. **The missing piece is small and well-scoped.** The executor needs a loop that, for owned runs, calls `dispatch.PostDispatch(...)` against a chosen worker node instead of leaving the task for ClaimNext. With the current per-task DB-backed state (no in-memory map yet), this is straightforward: poll for pending tasks on owned runs, pick a worker by least-loaded heuristic, POST. ~150 lines of code in a new file like `internal/executor/dispatch_loop.go`.

3. **Phase B would deliver the rest of the win** by moving the dispatch loop into an in-memory tick on the owner (no DB poll) plus checkpointing. Until Phase A2 (the missing dispatch loop) lands, neither Phase A nor Phase B can be cleanly measured.

## Recommended follow-up

**Phase A2 — Wire the executor-side dispatch loop.** Small focused PR:
- Add a per-owner goroutine on each node that polls owned runs for pending tasks.
- For each ready task, choose a worker (least-loaded; fall back to round-robin) and `dispatch.PostDispatch(...)`.
- If PostDispatch returns false (worker rejected), leave `claimed_by=""` so ClaimNext recovery picks it up.
- Re-baseline with this measurement template; expect `caesium_db_busy_retries_total` to drop sharply for owned runs and `caesium_worker_claims_total` to fall.

After A2 measurements look reasonable, Phase B (in-memory state + checkpoints) becomes the next clear move.

## Sandbox caveats (same as PR #170)

- Single physical node, 3 dqlite voters share one machine — no inter-voter network RTT. Real multi-machine deployments will show more contention, not less.
- In-pod ephemeral storage; production NVMe-backed PVCs will change absolute write latency but not the relative contention pattern.
- 100-task workload is still below the "Medium" deployment shape; larger workloads need a multi-machine test rig.

The headline finding (Phase A as-merged is the substrate without the executor wire-up) doesn't depend on these caveats — it's structural, not load-dependent.
