# Design: Distributed Mode Database Locking Remediation

> Status: Phases 0–3 shipped; Phase 4 (sharded write path) foundation in place, call-site migration in progress. Operator-facing knobs live in [parallel-execution-operations.md](parallel-execution-operations.md); shard layout in [database-sharding.md](database-sharding.md); load-test measurements in [load-testing-history.md](load-testing-history.md). The active frontier in this document is [Phase 4](#phase-4--horizontal-scale-via-sharded-write-path-active).

## What shipped

Phases 0–3 eliminated the `database is locked` storm in 3–50 node distributed deployments and are live by default. The core changes, with the key design decision preserved for each:

- **Phase 0 — stop the locking storm (PR #153).** `busy_timeout(5s)` and `synchronous=NORMAL` on dqlite open (via `app.WithBusyTimeout` because go-dqlite rejects PRAGMA SQL), bounded `SQLITE_BUSY`/`LOCKED` retry around `ClaimNext`/`ReclaimExpired`, and reclaim throttled to a per-worker-staggered interval (`CAESIUM_WORKER_RECLAIM_INTERVAL`, default 30s) instead of running every ~2s loop iteration.
- **Phase 1 — cut write volume.** Batched task registration (`Store.RegisterTasks` → one job/job-run lookup + a single multi-row `INSERT`, idempotency preserved) and a single-statement `UPDATE … RETURNING` for `ClaimNext` (replacing read-then-update).
- **Connection-layer refinement beyond the original plan (PRs #176, #188).** The plan's naive "raise `MaxOpenConns` 1→4" was **superseded**: because writes serialize through Raft and `busy_timeout` is effectively a no-op there (`pkg/db/retry.go`), the fix that actually landed is a **read/write connection split** (`installDqliteReadWriteSplit`, `pkg/db/db.go`) — a read pool (`CAESIUM_DATABASE_MAX_OPEN_CONNS`, default 4) alongside a single serialized write connection — plus transient-contention retry at the connection-pool layer. This is the canonical "database is locked" remediation; treat the Phase 1 `MaxOpenConns` bullet below as historical.
- **Phase 2 — cross-node coordination.** Spare-role topology (`CAESIUM_DATABASE_VOTERS`/`STANDBYS`, default 3/3) so worker count is decoupled from Raft cost, distributed wakeups (`internal/worker/wakeup_distributed.go`, `POST /internal/wakeup`, authenticated with `CAESIUM_INTERNAL_WAKEUP_TOKEN`), and leader-only `ReclaimExpired`. Poll interval relaxed once wakeups landed.
- **Phase 3 — observability + cleanup.** `caesium_db_busy_retries_total`, `caesium_reclaim_duration_seconds`, `caesium_task_register_batch_size`; `unknown data type: 0` warnings now carry a bounded `recent_db_statements` context field; durable event-bus replay (`execution_events.bus_dispatch_pending`/`bus_dispatched_at`) survives a crash between commit and dispatch.

The historical audit that motivated this work — and the per-phase action checklists as they were executed — are preserved in [Findings](#findings) and the [Shipped phase detail](#shipped-phase-detail-historical) appendix.

## Problem statement

Distributed-mode operators saw `database is locked` errors and `unknown data type: 0` warnings, especially when many task-heavy jobs ran concurrently across 3+ nodes: stalled `ClaimNext` calls, late lease expirations, and occasional `RegisterTask` failures that aborted a run. The audit identified four contributing factors (all confirmed by code inspection): high-frequency global reclamation, unbatched task registration, synchronized polling / thundering herd, and protocol serialization warnings. Code inspection surfaced additional root causes the audit missed — see [Findings](#findings).

A core Caesium tenet is **no external infrastructure dependencies** — dqlite is the storage layer and stays the storage layer. The plan therefore targets dqlite throughout, including the horizontal-scale phase.

## Scaling target

The plan is sized for the following deployment shapes, all reachable on a single dqlite cluster (no PostgreSQL, etcd, Kafka, or other external systems):

| Shape | Nodes | Concurrent task lifecycles | How | Status |
|---|---|---|---|---|
| Small | 3–5 | hundreds | Phase 0 + Phase 1 fixes; default voter-only topology. | ✅ shipped |
| Medium | 10–50 | low thousands | Phase 0–2; spare-role topology so most workers don't replicate the Raft log. | ✅ shipped |
| Large | 100–500 | tens of thousands | Phase 0–4; sharded write path via multiple dqlite databases on the same Raft cluster. | 🔶 Phase 4 in progress |

The architectural anchors that make this possible without external infra:

- **dqlite roles separate Raft membership from application membership.** A cluster keeps 3 Voters and 3 Standbys (Raft-replicated); every additional node joins as a Spare and acts as a leader-aware client. The Raft cost is bounded at ~6 nodes regardless of total worker count.
- **`app.Open(name)` supports multiple databases per cluster.** Each database has an independent write queue and engine thread on the leader. Sharding `task_runs` / `events` across N databases multiplies write throughput linearly until the leader's NVMe / network is saturated.
- **Sharding stays inside one Raft cluster.** Operators still deploy one binary, one bootstrap command, one set of peer addresses. No cross-cluster federation, no second control plane.

Beyond ~500 worker nodes or sustained tens of thousands of writes/sec the plan deliberately stops; that regime needs a different conversation about whether to split into multiple Caesium control planes (still infra-free, just more than one). It is **not** addressed here.

## Findings

The pre-remediation audit and the additional root causes found during code inspection. File/line references are as of the audit and are retained as the historical record of what motivated each phase.

### Audit verification

| # | Audit claim | Verdict | Evidence (at audit time) |
|---|---|---|---|
| 1 | `ReclaimExpired` every 2s creates write contention | Confirmed; worse than stated | `internal/worker/worker.go` ran reclaim on every loop iteration ahead of `ClaimNext`; each invocation did a JOIN scan + UPDATE on `task_runs` plus 2 events per expired row inside one transaction. |
| 2 | Unbatched task registration | Confirmed | `internal/job/job.go` looped over tasks; each `RegisterTask` opened its own transaction. |
| 3 | Thundering-herd polling on `ClaimNext` | Confirmed | `internal/worker/claimer.go` read up to 64 candidates then UPDATEd by ID, all in one transaction. Jitter only fired on idle sleeps. |
| 4 | `unknown data type: 0` warnings | Plausibly serialization, lower priority | dqlite log adapter did not classify these. Symptom of contention rather than independent bug. |

### Additional issues found during inspection

1. **`MaxOpenConns(1)` was the underlying serializer** — every read, write, transaction, and the long `ReclaimExpired` scan queued behind each other. (Addressed by the read/write split, see [What shipped](#what-shipped).)
2. **No `_busy_timeout` on the production dqlite connection** — it failed fast on `SQLITE_BUSY` instead of retrying. (Phase 0.)
3. **No `synchronous=NORMAL` PRAGMA.** (Phase 0.)
4. **Cross-node wakeups didn't exist** — only the in-process bus woke a node, so polling was the sole cross-node coordination. (Phase 2.)
5. **`ClaimNext` read candidates then UPDATEd** — two statements where one CAS would do. (Phase 1.)
6. **No retry/backoff on `SQLITE_BUSY`** — throughput degraded linearly with contention. (Phase 0 + connection-layer retry.)
7. **Per-task lookups inside `RegisterTask`** — constant within a registration burst and trivially hoistable. (Phase 1.)

---

## Phase 4 — horizontal scale via sharded write path (active)

Goal: scale to the "Large" deployment shape — 100–500 worker nodes and tens of thousands of concurrent tasks — without leaving dqlite. Phase 4 is a design-level commitment with several sub-PRs; treat each checkbox as the milestone gate, not a single change. **This is the active work.**

The unlock is that `App.Open(name)` on a single dqlite cluster opens an independent database with its own engine thread and write queue on the leader. Sharding the hot tables across N databases linearly multiplies write throughput on the same Raft cluster. Cross-database `ATTACH` is intentionally disabled in dqlite (see [canonical/dqlite#441](https://github.com/canonical/dqlite/issues/441)), so each shard must be transactionally self-contained — manageable for our schema because DAG state is naturally per-job.

**Foundation in place:** named dqlite database opens, `CAESIUM_DATABASE_SHARDS`, the `pkg/db.Router` routing primitive + `db.DefaultRouter()` with unit tests, and the table-placement contract in [database-sharding.md](database-sharding.md). The compatibility `db.Connection()` still returns the catalog DB while run-scoped call sites migrate.

- [x] **Define the shard boundary and routing key.** Hot, write-heavy tables (`task_runs`, `events`, optionally `job_runs`) shard by `hash(job_run_id) % N`, so all rows for a run live in one shard and per-run transactions stay local. Catalog tables (`jobs`, `triggers`, `atoms`, `tasks`, `secrets`, `users`) stay in the **catalog** database; terminal-run history moves to a **cold** database on a configurable lag. Documented in [database-sharding.md](database-sharding.md).
- [~] **Build a shard router in the data layer.** `pkg/db.Router`, named dqlite database opens, and router unit tests are implemented; `CAESIUM_DATABASE_SHARDS` (default 1) makes it a no-op until opted in. **Remaining:** migrate run-scoped call sites off the compatibility catalog connection onto router-aware run-scoped paths before `CAESIUM_DATABASE_SHARDS>1` is production-ready. Power-of-two shard counts recommended for clean rebalancing later. Risk: high — the largest refactor in the plan; ship with shards=1 first to flush out call-site mistakes.
- [ ] **Per-shard `ClaimNext` and `ReclaimExpired`.** Each worker iterates shards (round-robin with a per-worker offset to avoid herd) and claims against each shard's connection; reclaim runs once per shard on the leader. Per-shard fairness needs a test — pathological cases include all jobs hashing to one shard for a window.
- [ ] **Cold-shard archiver** (`internal/run/archiver.go`). Background loop that copies terminal `job_runs` + child rows to `caesium_history` and deletes from the hot shard on a configurable lag (default 24h). Two-shard delete-then-insert is not atomic; use idempotent upserts on the history side and only delete from hot once the history insert is confirmed.
- [ ] **Spare-aware bootstrap and operations.** Document/ship the recommended Helm/systemd shape: 3 voter pods + N autoscaled spare pods; spares join with `WithCluster([voter addresses])` and pick up the spare role automatically. Documentation + chart updates only.
- [ ] **Tune dqlite for high-write workloads.** Apply `app.WithSnapshotParams(...)` and trailing-log settings so the WAL doesn't grow unbounded under sustained write load. Snapshot tuning is where dqlite has the most reported production issues; validate carefully.

### Test plan (Phase 4)

- **Unit:** `pkg/db/router_test.go` — shard-key routing, catalog vs hot vs cold dispatch, fallback at shard count 1.
- **Integration** (`test/`, `-tags=integration`): 20-node cluster, `CAESIUM_DATABASE_SHARDS=8`, 200 jobs × 100 tasks. Verify per-shard write rate, archiver behaviour, and that catalog-table queries don't degrade under hot-shard load.
- **Load:** extend the `just integration-test` contention harness; capture per-shard `ClaimNext`/`ReclaimExpired` p50/p99, `database is locked` log-line count, dqlite leader CPU (per core), RSS, and on-disk footprint over a 24h soak. See [load-testing-history.md](load-testing-history.md) for the Phase 0–2B baseline methodology.

### Operational risks at scale

dqlite has known production issues that surface specifically at the write rates and uptimes Phase 4 targets. We accept dqlite as the storage layer (per the no-external-infra tenet) and mitigate.

| Risk | Evidence | Mitigation |
|---|---|---|
| **Memory growth on the leader** | [k8s-dqlite#196](https://github.com/canonical/k8s-dqlite/issues/196), [dqlite#494](https://github.com/canonical/dqlite/issues/494). | Phase 3 metrics on dqlite RSS; alert on growth rate. Periodic leader handover (`Handover()`) to recycle memory; cap WAL via snapshot tuning. |
| **Single core pinned to 100% under load** | [microk8s#3227](https://github.com/canonical/microk8s/issues/3227), [k8s-dqlite#36](https://github.com/canonical/k8s-dqlite/issues/36) — dqlite's engine is single-threaded per database. | Phase 4 sharding directly addresses this — multiple databases give multiple engine threads. Run voters on high single-thread hosts. |
| **Write amplification / WAL growth** | [microk8s#3064](https://github.com/canonical/microk8s/issues/3064) — 30 TB written in 2 weeks. | Tune snapshot frequency and trailing-log retention via `app.WithSnapshotParams`. Treat leader disk as a hot path; NVMe with provisioned IOPS. |
| **Leader churn under load** | General Raft behaviour; aggravated by long write transactions blocking heartbeats. | Phase 1 batching shortens transactions; Phase 2 leader-only reclaim avoids long follower scans. Conservative heartbeat/election timeouts. |
| **No public reference of dqlite at >50-node clusters** | No documented limits in LXD/MicroK8s/Canonical docs; largest references are double-digit node counts. | Phase 2's spare topology bounds Raft cost at ~6 nodes regardless of worker count. Validate with Phase 4 integration tests before claiming >50-node production-readiness. |

Operational guidance to publish alongside Phase 4: run voters on dedicated NVMe nodes isolated from worker noise; monitor leader CPU (per core), RSS, WAL size, snapshot count, and role transitions/hour; plan periodic rolling leader handover until upstream memory issues are resolved.

### Open questions

- Wakeup gossip threshold: hard-code at 50 nodes, or expose `CAESIUM_WAKEUP_GOSSIP_THRESHOLD`? (Distributed wakeups currently default to full fanout; gossip mode is gated behind `CAESIUM_WAKEUP_FANOUT_MODE` for clusters above ~50 nodes.)
- Archiver lag: keep terminal runs hot for 24h, shorter, or per-job configurable? Trade-off is UI freshness vs hot-shard size.
- Shard count: static via `CAESIUM_DATABASE_SHARDS` (current proposal) or dynamic resharding? Defer dynamic until a real operator hits the static-shard ceiling.
- Expose the catalog / hot / cold split in the data-source REST API so external query tools can target specific shards? Probably useful for debugging; defer until Phase 4 ships.

---

## Shipped phase detail (historical)

The per-phase action checklists below are retained as the historical implementation record. All items are complete unless noted; the [What shipped](#what-shipped) summary above is the canonical statement of current behaviour. Note in particular that the Phase 1 `MaxOpenConns` item was superseded by the read/write connection split (#188).

### Phase 0 — stop the locking storm (PR #153)
- [x] Add `busy_timeout` + `synchronous=NORMAL` to dqlite open (`app.WithBusyTimeout(5s)` for go-dqlite; PRAGMAs for injected SQL pools; never set `journal_mode` — dqlite owns it).
- [x] Wrap `ClaimNext`/`ReclaimExpired` with bounded `SQLITE_BUSY`/`LOCKED` retry (`withBusyRetry`, ~5 attempts, exponential backoff + jitter, ~310ms cap).
- [x] Throttle `ReclaimExpired` to `CAESIUM_WORKER_RECLAIM_INTERVAL` (default 30s) with a per-worker offset to desynchronize the cluster.
- [x] Add `caesium_db_busy_retries_total` and `caesium_reclaim_duration_seconds`.

### Phase 1 — cut write volume
- [x] Batch task registration into one transaction (`Store.RegisterTasks`: one job-run lookup + one job lookup + single multi-row `INSERT` + batched `task_ready` events; `RegisterTask` kept as a thin wrapper; idempotency preserved by pre-querying existing task IDs).
- [x] Replace `ClaimNext` read-then-update with a single `UPDATE … RETURNING` (dqlite supports `RETURNING`; equality-only node selectors encoded via `json_each`).
- [x] ~~Raise `MaxOpenConns` 1→4.~~ **Superseded by the read/write connection split (#188)** — see [What shipped](#what-shipped). `CAESIUM_DATABASE_MAX_OPEN_CONNS`/`MAX_IDLE_CONNS` now size the read pool; writes serialize on a single connection.
- [x] Hoist `RegisterTask` per-row lookups out of the per-task loop (folded into the batch refactor).

### Phase 2 — cross-node coordination and cluster topology
- [x] Spare-role topology (`app.WithVoters(3)`/`WithStandbys(3)`; `CAESIUM_DATABASE_VOTERS`/`STANDBYS`) so worker count is decoupled from Raft cost.
- [x] Distributed wakeups (`internal/worker/wakeup_distributed.go`, `POST /internal/wakeup` over the internal HTTP server, peer discovery via the dqlite cluster client API, `CAESIUM_INTERNAL_WAKEUP_TOKEN` auth; gossip mode behind `CAESIUM_WAKEUP_FANOUT_MODE` for large clusters).
- [x] Leader-only `ReclaimExpired` (via the dqlite client's `Leader()` lookup; non-leaders skip reclaim).
- [x] Raise default poll interval once wakeups landed.

### Phase 3 — cleanup and observability
- [x] Instrument the `unknown data type: 0` log line with a bounded `recent_db_statements` context field.
- [x] Add `caesium_task_register_batch_size`.
- [x] Document new env vars and operational guidance.
- [x] Durable event-bus replay (`execution_events.bus_dispatch_pending` / `bus_dispatched_at`) — event delivery survives a `SIGKILL` between commit and bus dispatch.

## Rollout

- Phases 0–3 are default-on. Phase 4 is opt-in indefinitely via `CAESIUM_DATABASE_SHARDS` (default 1); defaults change only after a multi-month soak in a Caesium-operated reference cluster.
- One release note per phase, calling out new env vars, default changes, and operational guidance (e.g. upgrading dqlite peers in lockstep, rolling leader-handover cadence).
