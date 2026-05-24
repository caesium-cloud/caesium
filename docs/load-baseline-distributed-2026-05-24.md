# Caesium Load Baseline — Distributed Multi-Node (2026-05-24)

> Distributed-mode counterpart to [load-baseline-2026-05-23.md](load-baseline-2026-05-23.md).
> First real numbers from a 3-node dqlite Raft cluster after all Phase 1 work
> (#165, #167) and PR #169's statement counter merged. Captures the
> contention regime the design's Phase 2 (run-owner) is meant to address.

## Environment

| Parameter | Value |
|---|---|
| Date | 2026-05-24 |
| Caesium commit | tip of `add-db-statements-counter` (one ahead of master with the statement counter) |
| Deployment | `just k8s-distributed` — 3-pod StatefulSet, Helm chart, `replicaCount=3`, `CAESIUM_EXECUTION_MODE=distributed`, kubernetes engine enabled |
| Cluster | Docker Desktop Kubernetes (single physical node, 3 pods → 3 dqlite voters) |
| Storage | `persistence.enabled=false` → ephemeral pod storage; no NVMe |
| `CAESIUM_DATABASE_SHARDS` | 1 (sharding router from PR #157 present but not enabled) |
| Engine | `kubernetes` — tasks run as Job pods in the same cluster |

## Methodology

Harness driven via `just load-test` with `CAESIUM_LOAD_ENGINE=kubernetes`. Two workload shapes, same shapes used in the single-node baseline (PR #166 / #168) for direct comparison.

The pod logs visibly showed `database is locked` errors on INSERT during the smoke test — exactly the symptom the locking-fix plan was built to address. The retry machinery (PR #154 / #155) absorbed them, but the contention shows up in `caesium_db_busy_retries_total`.

## Workload A — Moderate (25 tasks)

`5 jobs × fan-out=3 × depth=2 → 5 tasks/run × 5 = 25 task lifecycles, 2s tasks, 3 concurrent runs`

| Category | Rows | Stmts | Rows/Stmt | Share |
|---|---|---|---|---|
| `task_run_insert` | 25 | 5 | 5.0 | 13.3% |
| `task_run_status` | 82 | 72 | 1.1 | **43.6%** (dominant) |
| `event_insert` | 81 | 62 | 1.3 | 43.1% |
| `lease_renewal` | 0 | 0 | — | 0.0% |
| **TOTAL** | **188** | **139** | **1.4** | 100% |

- 5/5 runs succeeded.
- Wall-clock total: 52s.
- End-to-end p50/p99: **17s / 31s** per run (single-node local for similar workload: ~4s).
- `caesium_db_busy_retries_total`: **26** for 25 tasks — roughly one retry per task.
- `caesium_worker_claims_total`: 19 (3 nodes claiming, some races resolved by retry).

## Workload B — Stress (100 tasks)

`10 jobs × fan-out=4 × depth=3 → 10 tasks/run × 10 = 100 task lifecycles, 500ms tasks, 5 concurrent runs`

| Category | Rows | Stmts | Rows/Stmt | Share |
|---|---|---|---|---|
| `task_run_insert` | 100 | 10 | 10.0 | 20.4% |
| `task_run_status` | 195 | 165 | 1.2 | **39.9%** (dominant) |
| `event_insert` | 194 | 136 | 1.4 | 39.7% |
| `lease_renewal` | 0 | 0 | — | 0.0% |
| **TOTAL** | **489** | **311** | **1.6** | 100% |

- 10/10 runs succeeded.
- Wall-clock total: 1m7s.
- End-to-end p50/p99: **31s / 32s** per run.
- `caesium_db_busy_retries_total`: **65** for 100 tasks — ~0.65 retries per task at the stress workload.
- Peak write rate: 7.0 `task_run_status`/s, 6.8 `event_insert`/s.

## Single-node vs distributed — same workload, both modes

Workload B (100 tasks, 10 jobs, fan-out=4, depth=3, 500ms tasks) ran on both deployments:

| Metric | Single-node local (PR #169) | Distributed 3-node k8s | Delta |
|---|---|---|---|
| Total run time | 18s | 67s | **3.7× slower** |
| End-to-end p50 | 4s | 31s | **7.8× slower** |
| End-to-end p99 | 6s | 32s | **5.3× slower** |
| `db_busy_retries` | 9 | 65 | **7.2× more contention** |
| Total row writes | 720 | 489 | (-32%, but…) |
| Total statements | 510 | 311 | (-39%, but…) |
| Rows/stmt | 1.4 | 1.6 | similar |

The lower row counts in the distributed run reflect tasks completing later in the harness window (the `task_run_status` UPDATEs for some tasks happen after the final metric sample). This is itself a signal — distributed mode is slow enough that completion sweeps fall outside the run window.

## Key findings

1. **Distributed-mode contention is real and measurable.** A 100-task workload on a 3-node Raft cluster triggers ~7× more `db_busy_retries` than the same workload on single-node, and **end-to-end latency is 5–8× worse**. The retry machinery from the locking-fix plan keeps things correct but pays for it in latency.

2. **Phase 1 batching factors hold across deployment modes.** `task_run_insert` batches at 5–10× (RegisterTasks), `event_insert` at 1.3–1.4× (Phase 1.1 coalescing), `task_run_status` at 1.1–1.2× (Phase 1.4 predecessor batching). Same as single-node — confirms the batching is a function of code, not deployment shape.

3. **`lease_renewal` stays at 0 even in distributed mode at these workloads.** Tasks finish faster than the renewal trigger threshold (`lease_ttl/2`), and the skip-when-not-needed path correctly exits. To exercise this column requires tasks longer than 15s.

4. **`task_run_status` is still the dominant write category** (44% on workload A, 40% on workload B). Phase 1.4 reduced it but not enough to change the dominance.

5. **The "database is locked" symptom is what Phase 2 (run-owner) targets directly.** Moving coordination state out of dqlite eliminates most of the per-transition writes that drive the contention. The empirical numbers here argue *for* proceeding with Phase 2, not against.

## Phase 2 gate decision — supported by data

The original Phase 1 → Phase 2 gate in `design-scaling-job-execution.md` said: *"if after Phase 1 the dominant remaining category is `task_run_status` (CompleteTask's predecessor-counter UPDATEs and claim/start transitions), that confirms Phase 2's run-owner design is the right next step."*

Both workloads here confirm that condition. Phase 1 batching reduced statement count by ~30% (rows/stmt = 1.4–1.6 vs 1.0 baseline) but per-completion writes are still O(N) in fan-out width *at the per-task level* — the run-owner pattern collapses that to ~O(1) per run by keeping the DAG state in memory and writing only checkpoints + terminal-state rows.

**Recommendation: proceed with Phase 2 planning.** Specifically:

- Start with **Phase 2 Phase A** from the design doc: the `run_leases` table + owner election + dispatch RPC, *without* the checkpoint/replay machinery yet. Even just push-based dispatch from a single owner per run eliminates the per-claim `task_run_status` UPDATE (the largest current write category) for owned runs.
- Run this baseline again after Phase 2 Phase A and look for: `task_run_status` rows/stmt rising above 5× (because the owner batches more updates), `db_busy_retries` dropping by an order of magnitude (because the owner serializes its own work).

## Sandbox caveats (still)

- **Single physical node** — 3 dqlite voters live on one machine. Real multi-machine deployments have network RTT between voters that this run doesn't capture. Expect *more* contention there, not less.
- **In-pod ephemeral storage** — `persistence.enabled=false`. NVMe-backed PVCs in production would change absolute write latency but not the relative contention pattern.
- **Workload sizes are still modest** — 100 tasks is below the "Medium" deployment shape (low-thousands of concurrent tasks). Larger workloads need to be driven from a multi-node test rig that doesn't share CPU/memory with the cluster under test.

The findings here are directionally clear: distributed contention is the regime Phase 2 was designed for, and the numbers justify the next round of work.
