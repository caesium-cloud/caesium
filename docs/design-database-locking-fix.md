# Design: Distributed Mode Database Locking Remediation

> Status: Proposed. Tracks remediation work for `database is locked` errors observed in 3-node distributed (dqlite) deployments, and lays out a path to scale the same dqlite-backed architecture to hundreds of worker nodes and thousands of concurrent tasks. Items are grouped by phase; tick them off as PRs land.

## Problem Statement

Distributed-mode operators see `database is locked` errors and `unknown data type: 0` warnings in logs, especially when many task-heavy jobs run concurrently across 3+ nodes. End users observe stalled `ClaimNext` calls, lease expirations that fire late, and the occasional `RegisterTask` failure that aborts a run.

The audit identified four contributing factors, all of which are confirmed by code inspection:

1. **High-frequency global reclamation.** Every worker calls `ReclaimExpired` on every iteration of its loop (default 2s), producing redundant cluster-wide UPDATEs.
2. **Unbatched task registration.** Each task in a DAG is inserted in its own transaction, producing N transactions per job start.
3. **Synchronized polling / thundering herd.** Multiple nodes hit the same hot rows on the same cadence; jitter only applies when idle, not under load.
4. **Protocol warnings.** `unknown data type: 0` from go-dqlite suggests serialization stress during contention windows.

Code inspection surfaced additional root causes the audit missed; they're called out in [Findings](#findings) and addressed by the action items below.

A core Caesium tenet is **no external infrastructure dependencies** — dqlite is the storage layer and stays the storage layer. The plan therefore targets dqlite throughout, including the horizontal-scale phase. See [Scaling target](#scaling-target) for the design center and [Operational risks at scale](#operational-risks-at-scale) for known dqlite-specific issues we accept and mitigate.

## Scaling target

The plan is sized for the following deployment shapes, all reachable on a single dqlite cluster (no PostgreSQL, etcd, Kafka, or other external systems):

| Shape | Nodes | Concurrent task lifecycles | How |
|---|---|---|---|
| Small | 3–5 | hundreds | Phase 0 + Phase 1 fixes; default voter-only topology. |
| Medium | 10–50 | low thousands | Phase 0–2; spare-role topology so most workers don't replicate the Raft log. |
| Large | 100–500 | tens of thousands | Phase 0–4; sharded write path via multiple dqlite databases on the same Raft cluster. |

The architectural anchors that make this possible without external infra:

- **dqlite roles separate Raft membership from application membership.** A cluster has 3 Voters and 3 Standbys (Raft-replicated); every additional node joins as a Spare and acts as a leader-aware client. The Raft cost is bounded at ~6 nodes regardless of total worker count.
- **`app.Open(name)` supports multiple databases per cluster.** Each database has an independent write queue and engine thread on the leader. Sharding `task_runs` / `events` across N databases multiplies write throughput linearly until the leader's NVMe / network is saturated.
- **Sharding stays inside one Raft cluster.** Operators still deploy one binary, one bootstrap command, one set of peer addresses. No cross-cluster federation, no second control plane.

Beyond ~500 worker nodes or sustained tens of thousands of writes/sec the plan deliberately stops; that regime needs a different conversation about whether to split into multiple Caesium control planes (still infra-free, just more than one). It is **not** addressed here.

---

## Findings

### Audit verification

| # | Audit claim | Verdict | Evidence |
|---|---|---|---|
| 1 | `ReclaimExpired` every 2s creates write contention | Confirmed; worse than stated | `internal/worker/worker.go:66-69` runs reclaim on every loop iteration ahead of `ClaimNext`. Each invocation does a JOIN scan + UPDATE on `task_runs` plus 2 events per expired row inside one transaction (`internal/worker/claimer.go:162-229`). |
| 2 | Unbatched task registration | Confirmed | `internal/job/job.go:456-462` loops over tasks; each `RegisterTask` opens its own transaction at `internal/run/store.go:431`. |
| 3 | Thundering-herd polling on `ClaimNext` | Confirmed | `internal/worker/claimer.go:67-150` reads up to 64 candidates then UPDATEs by ID, all in one transaction. Jitter at `internal/worker/worker.go:120-124` only fires on idle sleeps. |
| 4 | `unknown data type: 0` warnings | Plausibly serialization, lower priority | dqlite log adapter at `pkg/dqlite/dqlite.go:47-61` does not classify these. Symptom of contention rather than independent bug. |

### Additional issues

1. **`MaxOpenConns(1)` is the underlying serializer.** `pkg/db/db.go:67` caps the dqlite pool at one connection, so every read, write, transaction, and the long `ReclaimExpired` scan all queue behind each other on every node.
2. **No `_busy_timeout` on the dqlite connection.** `pkg/dqlite/dqlite.go:78` opens with no PRAGMAs. Test/local DBs use `_busy_timeout=5000` (`internal/localrun/runner.go:170`); production fails fast on `SQLITE_BUSY` instead of retrying.
3. **No `synchronous=NORMAL` PRAGMA.** Only `foreign_keys=ON` is set (`pkg/db/db.go:70`). dqlite manages its own WAL but explicit synchronous tuning reduces fsync pressure.
4. **Cross-node wakeups don't exist.** The wakeup channel (`internal/worker/wakeup.go`) subscribes to an in-process bus (`internal/event/bus.go:67`). When node A registers tasks, only node A wakes up; nodes B and C still wait the full poll interval. Polling is therefore the only cross-node coordination.
5. **`ClaimNext` reads candidates then UPDATEs.** Two statements where one CAS would do (`internal/worker/claimer.go:67-150`).
6. **No retry/backoff on `SQLITE_BUSY`.** `internal/worker/claimer.go:117-121` increments a metric and returns the error; the worker logs and sleeps. Throughput degrades linearly with contention.
7. **Per-task lookups inside `RegisterTask`.** `internal/run/store.go:386` selects `schema_validation`/`cache_config` from `jobs`, and `:443` selects `job_id` from `job_runs`. Both are constant within a registration burst and trivially hoistable.

---

## Action Plan

Phases are ordered by risk and impact. Each phase should land, get measured, and inform the next. Do not bundle phases into a single PR.

### Phase 0 — Stop the locking storm (small, safe, high impact)

Goal: eliminate the bulk of `database is locked` errors with low-risk changes that need no schema or API changes.

Status: implemented in PR #153. The production dqlite path uses the native `app.WithBusyTimeout` option because go-dqlite rejects SQL PRAGMA statements; injected connection paths still apply the SQL `busy_timeout` and `synchronous=NORMAL` PRAGMAs and are covered by tests.

- [x] **Add `busy_timeout` and `synchronous` PRAGMAs to dqlite open.**
  - File: `pkg/dqlite/dqlite.go`.
  - Use `app.WithBusyTimeout(5s)` for go-dqlite connections. For injected SQL connection pools, run `PRAGMA busy_timeout=5000; PRAGMA synchronous=NORMAL;`. Do **not** set `journal_mode` — dqlite owns it.
  - Acceptance: production dqlite connections are opened with a 5s busy timeout; injected SQL connection pools report `pragma_busy_timeout=5000` and `synchronous=NORMAL`.
  - Risk: low. `synchronous=NORMAL` weakens single-node durability slightly, but dqlite's Raft replication provides durability at the cluster level.

- [x] **Wrap `ClaimNext` and `ReclaimExpired` with bounded `SQLITE_BUSY`/`LOCKED` retry.**
  - File: `internal/worker/claimer.go`.
  - Helper `withBusyRetry(ctx, backoffs, fn)` performing up to 5 attempts with per-claimer exponential backoff (10ms, 20ms, 40ms, 80ms, 160ms) plus per-attempt jitter, total ~310ms cap. Reuse `isClaimContentionErr` (line 231).
  - Wrap the transaction calls at `claimer.go:67` and `:170`. Increment `caesium_worker_claim_contention_total` on each retry, not just on the first.
  - Acceptance: under simulated contention (see [Test plan](#test-plan)), `ClaimNext` returns success when `busy_timeout` plus retry can resolve it. Errors only bubble up after exhausting retries.
  - Risk: low. Retries are bounded; backoff is short.

- [x] **Throttle `ReclaimExpired` so it does not run every iteration.**
  - File: `internal/worker/worker.go:66-69`.
  - Run reclaim only when at least `reclaimInterval` has passed since the last reclaim (default 30s, env-configurable as `CAESIUM_WORKER_RECLAIM_INTERVAL`).
  - Stagger by adding a per-worker random offset at `Worker` init so the cluster-wide reclaim cadence is naturally desynchronized.
  - Acceptance: in a 3-node cluster test, the rate of reclaim transactions falls by ~15× while no expired lease lingers more than `leaseTTL + reclaimInterval`.
  - Risk: low. Lease TTL is 5min by default; even a 30s reclaim cadence has ample headroom.

- [x] **Surface a `caesium_db_busy_retries_total` counter and `caesium_reclaim_duration_seconds` histogram.**
  - File: `internal/metrics/metrics.go`.
  - Increment on every retry attempt; observe at the end of each `ReclaimExpired`.
  - Acceptance: metrics exposed at `/metrics`; visible in default Grafana dashboard config (`docs/observability.md` if present).
  - Risk: none.

### Phase 1 — Cut write volume (moderate, requires care)

Goal: collapse N transactions into 1 where possible; replace read-then-write with single CAS.

- [x] **Batch task registration into a single transaction.**
  - Files: `internal/job/job.go:456-462`, `internal/run/store.go:356`.
  - Add `Store.RegisterTasks(runID uuid.UUID, inputs []RegisterTaskInput) error` that:
    1. Performs one `SELECT id, schema_validation, cache_config FROM jobs WHERE id = ?` and one `SELECT job_id FROM job_runs WHERE id = ?` ahead of the loop.
    2. Builds `[]models.TaskRun` records in memory.
    3. Inserts them with a single `tx.Create(&records)` (GORM batches into a multi-row INSERT).
    4. Inserts `task_ready` events for tasks with `outstanding_predecessors = 0` in a single `tx.Create(&events)`.
  - Keep existing `RegisterTask` as a thin wrapper that calls the batch path with one input, so callers outside `internal/job/job.go` continue to work.
  - Acceptance: registering a 50-task DAG performs one job-run lookup, one job lookup, one existing-task prequery for idempotency, and batched `task_runs` / `events` inserts.
  - Risk: medium. Need to preserve idempotency — current code skips already-existing rows (`store.go:362-365`); preserve by pre-querying existing `task_id`s and excluding them from the batch.

- [x] **Replace `ClaimNext` read-then-update with a single `UPDATE ... RETURNING`.**
  - File: `internal/worker/claimer.go:67-150`.
  - Issue one `UPDATE task_runs SET claimed_by = ?, claim_expires_at = ?, claim_attempt = claim_attempt + 1, status = 'running' WHERE id = (SELECT tr.id FROM task_runs tr JOIN job_runs jr ON jr.id = tr.job_run_id WHERE jr.status = 'running' AND tr.status = 'pending' AND tr.outstanding_predecessors = 0 AND (tr.claimed_by = '' OR tr.claim_expires_at IS NULL OR tr.claim_expires_at < ?) AND <node-selector predicate> ORDER BY tr.created_at ASC LIMIT 1) RETURNING *`.
  - dqlite supports `RETURNING` (already detected at `pkg/dqlite/dqlite.go:91`).
  - Node selector: selectors are equality-only string maps in the current job schema, so encode matching directly with SQLite/dqlite `json_each` and PostgreSQL `json_each_text` predicates.
  - Acceptance: the common case (no selectors / equality only) issues a single SQL statement per `ClaimNext`. Existing `claimer_test.go` cases pass, plus new tests covering selector matching and miss-the-race scenarios.
  - Risk: medium. Selector logic is the only tricky part. Because Caesium is pre-alpha, this ships directly rather than behind a compatibility mode.

- [x] **Raise `MaxOpenConns` for dqlite from 1 to 4 (configurable).**
  - File: `pkg/db/db.go:67`.
  - Add `CAESIUM_DATABASE_MAX_OPEN_CONNS` env var (default 4) and `CAESIUM_DATABASE_MAX_IDLE_CONNS` (default 2). Apply both to dqlite and postgres paths.
  - Acceptance: under a load test driving 50 concurrent claims, p99 `ClaimNext` latency drops at least 30% versus Phase 0 baseline. No `dqlite: database is locked` errors observed up to N=8.
  - Risk: medium-high. dqlite serializes writes at the leader regardless, but multiple local conns let reads and writes overlap. Too high may worsen leader-side contention or memory use; bake at 4 first.

- [x] **Hoist `RegisterTask` per-row lookups out of the per-task loop.**
  - File: `internal/run/store.go:386,443`.
  - Folded into the batch refactor above; explicit checkbox so reviewers verify both lookups now happen once per batch.
  - Acceptance: `select * from jobs` and `select * from job_runs` execute at most once per `RegisterTasks` call regardless of input length (verify via GORM SQL log).
  - Risk: low.

### Phase 2 — Cross-node coordination and cluster topology (moderate)

Goal: bound the Raft cost as worker count grows, and reduce the need for tight polling once write contention is no longer the bottleneck. This is the "Medium" deployment shape from the [Scaling target](#scaling-target).

- [x] **Adopt the spare-role topology so worker count is decoupled from Raft cost.**
  - Files: `pkg/dqlite/dqlite.go:64-69`, `pkg/env/env.go`, `cmd/start/start.go`.
  - Today `app.New` is called with `WithAddress` and `WithCluster` only, accepting the dqlite library defaults. Add `app.WithVoters(3)` and `app.WithStandbys(3)` so the leader's `RolesAdjustmentFrequency` (default 30s) keeps exactly 3 voters and 3 standbys; every additional node automatically settles as a Spare. Spares don't replicate the Raft log and don't vote, but still act as leader-aware SQL clients via `app.Open` — i.e. fully functional Caesium worker nodes.
  - Add `CAESIUM_DATABASE_VOTERS` (default 3) and `CAESIUM_DATABASE_STANDBYS` (default 3); operators tune these for their HA target. Voter count must be odd and ≥ 3.
  - Document the recommended deployment: 3 control-plane nodes (voter), 0–3 standby nodes for failover headroom, all remaining nodes joined as spares.
  - Acceptance: a 10-node test cluster shows exactly 3 nodes with `RAFT_VOTER`, ≤3 with `RAFT_STANDBY`, and the rest with `RAFT_SPARE` in `dqlite-dump-cluster` (or equivalent client API). Killing a voter triggers promotion of a standby within `2 × RolesAdjustmentFrequency`.
  - Risk: low. Defaults match the library defaults; behaviour is unchanged for existing 3-node deployments. Larger clusters benefit immediately because Raft replication stops scaling with worker count.

- [x] **Introduce distributed wakeups for task readiness.**
  - Files: `cmd/start/start.go:235-236`, new `internal/worker/wakeup_distributed.go`.
  - On task-ready / lease-expired events, fanout an HTTP `POST /internal/wakeup` to peer node addresses (discovered via dqlite's cluster client API, **not** `env.Variables().DatabaseNodes` — that only contains bootstrap peers, not spares). Recommend reusing the existing internal HTTP server and TLS / auth story rather than adding a UDP path; the message is one packet per event and HTTP overhead is negligible at this rate.
  - Receiving node signals its local wakeup channel.
  - Authenticate with a shared secret (`CAESIUM_INTERNAL_WAKEUP_TOKEN`); drop unauthenticated requests.
  - At cluster sizes above ~50 nodes, switch from full fanout to **gossip** (forward to log(N) randomly-chosen peers, with a TTL of 2–3 hops) to keep wakeups O(N log N) cluster-wide. Implement behind `CAESIUM_WAKEUP_FANOUT_MODE=full|gossip` and default to `full` until cluster size warrants gossip.
  - Acceptance: registering tasks on node A causes node B to claim within `<200ms` instead of waiting for the next poll. In a 50-node test cluster, gossip mode delivers wakeups to ≥99% of peers within `<500ms`.
  - Risk: medium. Network failures must not block event publication; treat wakeups as best-effort hints, not deliveries. Gossip implementation must be tested for partition tolerance.

- [x] **Make `ReclaimExpired` a leader-only responsibility.**
  - Files: `internal/worker/claimer.go:162`, `cmd/start/start.go`.
  - Option A (preferred): use the dqlite client's `Leader()` lookup to check if this node hosts the current Raft leader; non-leaders skip reclaim entirely. Re-check on a short interval so failover takes over reclaim within `~2 × RolesAdjustmentFrequency`.
  - Option B (fallback): add a `cluster_locks` row with `name='reclaim_expired'`, `held_by`, `expires_at`; nodes try to acquire via a CAS UPDATE, only the holder runs reclaim. Useful if leader detection proves flaky.
  - Acceptance: in a 10-node cluster, reclaim transactions execute on exactly one node at a time. Failover within `2 × RolesAdjustmentFrequency` if the leader dies.
  - Risk: medium. Must handle leader churn cleanly; option B is simpler but adds a hot row.

- [x] **Raise default poll interval from 2s to 15s** (only after distributed wakeups land).
  - Files: `pkg/env/env.go:60`, `internal/worker/worker.go:38`.
  - Acceptance: end-to-end run latency unchanged within 5% versus baseline once wakeups are in place; cluster-wide DB QPS for the `task_runs` table drops by an order of magnitude.
  - Risk: low after wakeups; high without them. Strictly gated on prior checkbox.

### Phase 3 — Cleanup and observability

Goal: investigate residual issues and tighten the operational surface area.

- [x] **Investigate the `unknown data type: 0` log line.**
  - File: `pkg/dqlite/dqlite.go:47-61`.
  - Capture the surrounding context (statement / params) when `client.LogWarn` fires with this string. If it persists post-Phase 0, file an upstream issue against `canonical/go-dqlite`.
  - Acceptance: either the warnings are gone after Phase 0, or we have a minimal repro pinned to the code path.
  - Risk: none — investigative.
  - Status: instrumentation added. Matching dqlite warnings now include a bounded `recent_db_statements` field populated from GORM trace output so the nearby statement path can be identified if the warning persists.

- [x] **Add `caesium_task_register_batch_size` histogram.**
  - File: `internal/metrics/metrics.go`.
  - Observe the input length on each `RegisterTasks` call.
  - Acceptance: dashboards show batch-size distribution, useful for diagnosing degenerate single-task registrations.
  - Risk: none.

- [x] **Document new env vars and operational guidance.**
  - File: `docs/configuration.md` (or wherever env vars are listed).
  - Cover `CAESIUM_WORKER_RECLAIM_INTERVAL`, `CAESIUM_DATABASE_MAX_OPEN_CONNS`, `CAESIUM_DATABASE_MAX_IDLE_CONNS`, `CAESIUM_INTERNAL_WAKEUP_TOKEN`.
  - Acceptance: docs PR merged alongside the last code change that introduces each var.
  - Risk: none.

- [ ] **Consider durable event outbox.** (Stretch.)
  - File: `internal/run/store.go:454`.
  - Today, `publishEvents` runs after the transaction commits; events are lost on crash. Move publication onto a polled outbox table.
  - Acceptance: event delivery survives a `SIGKILL` between commit and publish in an integration test.
  - Risk: medium. Orthogonal to locking but a known bug — schedule independently if reviewers prefer.

### Phase 4 — Horizontal scale via sharded write path (large, design-level)

Goal: scale to the "Large" deployment shape — 100–500 worker nodes and tens of thousands of concurrent tasks — without leaving dqlite. Phase 4 is a design-level commitment with several sub-PRs; treat each checkbox as the milestone gate, not a single change.

The unlock is that `App.Open(name)` on a single dqlite cluster opens an independent database with its own engine thread and write queue on the leader. Sharding the hot tables across N databases linearly multiplies write throughput on the same Raft cluster. Cross-database `ATTACH` is intentionally disabled in dqlite (see [canonical/dqlite#441](https://github.com/canonical/dqlite/issues/441)), so each shard must be transactionally self-contained — manageable for our schema because DAG state is naturally per-job.

- [ ] **Define the shard boundary and routing key.**
  - Hot, write-heavy tables (`task_runs`, `events`, optionally `job_runs`) shard by `hash(job_run_id) % N`. All rows for a given run live in one shard so per-run transactions stay local.
  - Catalog tables (`jobs`, `triggers`, `atoms`, `tasks`, `secrets`, `users`) stay in the **catalog** database (`caesium`). They're write-light and read-heavy; replication via standbys covers them fine.
  - History tables (`task_runs`, `events` for terminal runs) move to a **cold** database (`caesium_history`) on a configurable lag. The hot path never scans cold rows.
  - Acceptance: a written-down schema doc enumerates which table lives in which database, and the runtime routes accordingly.

- [ ] **Build a shard router in the data layer.**
  - Files: `pkg/db/db.go`, new `pkg/db/router.go`.
  - Replace the single `*gorm.DB` returned by `Connection()` with a `Router` that dispatches by table + shard key. Most call sites get the catalog DB; run-scoped sites get the run's hot shard.
  - Static shard count `CAESIUM_DATABASE_SHARDS` (default 1, so this is a no-op until operators opt in). Power-of-two recommended for clean rebalancing later.
  - Acceptance: at `CAESIUM_DATABASE_SHARDS=1` the system is byte-identical to today; at `CAESIUM_DATABASE_SHARDS=8`, 50 concurrent runs distribute across 8 hot shards roughly evenly.
  - Risk: high. This is the largest refactor in the plan; gate behind a feature flag and ship the router with shards=1 first to flush out call-site mistakes before any deployment turns it up.

- [ ] **Per-shard ClaimNext and ReclaimExpired.**
  - Files: `internal/worker/claimer.go`, `cmd/start/start.go`.
  - Each worker iterates shards (round-robin, with a per-worker shard offset to avoid herd) and runs `ClaimNext` against each shard's DB connection. Reclaim runs once per shard on the leader.
  - Acceptance: with 8 shards and 16 workers, no single shard sees more than `2 × (workers/shards)` concurrent claim attempts.
  - Risk: medium. Per-shard fairness needs a test — pathological cases include all jobs hashing to one shard for a window.

- [ ] **Cold-shard archiver.**
  - File: new `internal/run/archiver.go`.
  - Background loop that, per hot shard, copies terminal `job_runs` + child rows to `caesium_history` and deletes from the hot shard. Configurable lag (default 24h) so live UI / debugging still hits hot data.
  - Acceptance: hot shard row counts plateau under steady load; history shard grows monotonically; UI queries that span both transparently union (helper at the router level).
  - Risk: medium. Two-shard delete-then-insert is not atomic; use idempotent upserts on the history side, only delete from hot once the history insert is confirmed.

- [ ] **Spare-aware bootstrap and operations.**
  - Files: `cmd/start/start.go`, deployment docs.
  - Document the recommended Helm/systemd shape: 3 voter pods (small dedicated nodes), N spare pods (worker pool, autoscaled). Spares need only `WithCluster([voter addresses])` to join; they pick up the spare role automatically per Phase 2.
  - Acceptance: `helm/` chart and example systemd units demonstrate the topology; a fresh operator can stand up a 50-spare cluster from the docs alone.
  - Risk: low. Documentation + chart updates only.

- [ ] **Tune dqlite for high-write workloads.**
  - File: `pkg/dqlite/dqlite.go`.
  - Apply `app.WithSnapshotParams(...)` and snapshot/trailing-log settings so WAL doesn't grow unbounded under sustained write load (see [Operational risks at scale](#operational-risks-at-scale)).
  - Acceptance: under a 24h soak at 1000 writes/sec/shard, on-disk footprint stays within 2× the steady-state working-set size.
  - Risk: medium. Snapshot tuning is the area where dqlite has the most reported production issues; needs careful validation.

---

## Test plan

Each phase needs measurement before and after. All numbers below are illustrative targets, not gates.

- **Unit tests**
  - `internal/worker/claimer_test.go`: extend with a contention harness that fakes `SQLITE_BUSY` returns and asserts retry behaviour.
  - `internal/run/store_test.go`: add `RegisterTasks` cases — empty input, mixed new/existing tasks, idempotent re-registration, batch with mixed `outstanding_predecessors`.
  - `pkg/db/router_test.go` (Phase 4): shard-key routing, catalog vs hot vs cold dispatch, fallback when shard count is 1.
- **Integration tests** (`test/`, run with `-tags=integration`)
  - **Small (Phase 0–1):** 3-node dqlite cluster, 20 jobs × 50 tasks each, 4 concurrent runs per job. Assert no `database is locked` surfaced to callers; assert end-to-end completion within a budget.
  - **Medium (Phase 2):** 10-node cluster with 3 voters + 3 standbys + 4 spares. Verify role assignment, leader-only reclaim, and distributed-wakeup latency. Kill a voter; confirm standby promotion within `2 × RolesAdjustmentFrequency`.
  - **Large (Phase 4):** 20-node cluster, `CAESIUM_DATABASE_SHARDS=8`, 200 jobs × 100 tasks. Verify per-shard write rate, archiver behaviour, and that catalog-table queries don't degrade under hot-shard load.
- **Load test** (manual, documented in this PR)
  - Use the `just integration-test` harness extended with a contention scenario. Capture: `caesium_worker_claim_contention_total` rate, `caesium_db_busy_retries_total` rate, p50/p99 of `ClaimNext` and `ReclaimExpired`, count of `database is locked` log lines, end-to-end run wall time, dqlite leader CPU, dqlite RSS, on-disk footprint over 24h.

---

## Operational risks at scale

dqlite has known production issues that surface specifically at the write rates and uptimes Phase 4 targets. We accept dqlite as the storage layer (per the no-external-infra tenet) and mitigate.

| Risk | Evidence | Mitigation |
|---|---|---|
| **Memory growth on the leader** | [k8s-dqlite#196](https://github.com/canonical/k8s-dqlite/issues/196), [dqlite#494](https://github.com/canonical/dqlite/issues/494) — MicroK8s observed steady RSS growth where SQLite/etcd were bounded. | Phase 3 metrics on dqlite RSS; alert on growth rate. Schedule periodic leader handover (`Handover()`) to recycle memory. Cap WAL via snapshot tuning (Phase 4 last item). |
| **Single core pinned to 100% under load** | [microk8s#3227](https://github.com/canonical/microk8s/issues/3227), [k8s-dqlite#36](https://github.com/canonical/k8s-dqlite/issues/36). dqlite's engine is single-threaded per database. | Phase 4 sharding directly addresses this — multiple databases give multiple engine threads on the leader. Run voter nodes on hosts with high single-thread performance. |
| **Write amplification / WAL growth** | [microk8s#3064](https://github.com/canonical/microk8s/issues/3064) — 30 TB written in 2 weeks. | Tune snapshot frequency and trailing-log retention via `app.WithSnapshotParams`. Treat dqlite leader disk as a hot path; run on NVMe with provisioned IOPS. |
| **Leader churn under load** | General Raft behaviour; aggravated by long write transactions blocking heartbeats. | Phase 1 batching shortens transaction duration. Phase 2 leader-only reclaim avoids long scans on followers. Set raft heartbeat / election timeouts conservatively; document recommended values. |
| **`unknown data type: 0` warnings** | Observed in our own logs; suspected serialization stress. | Phase 0 should make these vanish by reducing contention. Phase 3 captures repro if they persist; file upstream. |
| **No public reference of dqlite at >50-node clusters** | Search of LXD, MicroK8s, and Canonical docs returns no documented limits; largest production references are double-digit node counts. | Phase 2's spare topology is the unblocker — Raft cost stays at ~6 nodes regardless of worker count. Validate with the Phase 2 / Phase 4 integration tests before claiming production-readiness above 50 nodes. |

Operational guidance to publish alongside Phase 4:

- Run voters on dedicated nodes (small, well-resourced, NVMe, isolated from worker noise).
- Monitor: dqlite leader CPU (per core, not aggregated), RSS, on-disk WAL size, snapshot count, time-since-last-snapshot, role transitions per hour.
- Plan for periodic rolling leader handover (e.g. weekly) until upstream memory issues are resolved.

---

## Rollout

- Each phase ships behind feature flags / env vars where listed; default-on for Phase 0 and Phase 1 while Caesium remains pre-alpha, default-on for Phase 2 only after distributed wakeups have soaked.
- Phase 4 is opt-in indefinitely via `CAESIUM_DATABASE_SHARDS` (default 1). Defaults change only after a multi-month soak in a Caesium-operated reference cluster.
- A single release note per phase, calling out new env vars, default changes, and any operational guidance (e.g., upgrading dqlite peers in lockstep, rolling leader handover cadence).

## Open questions

- Phase 2 wakeups gossip threshold: hard-code at 50 nodes, or expose `CAESIUM_WAKEUP_GOSSIP_THRESHOLD` so operators can tune?
- Phase 4 archiver: keep terminal runs in the hot shard for 24h (current proposal), or shorter? Trade-off is UI freshness vs hot-shard size. Could be per-job configurable.
- Phase 4 shard count: static via env var (current proposal) or dynamic resharding? Dynamic is significantly more complex; defer until a real operator hits the static-shard ceiling.
- Is there value in exposing the catalog / hot / cold split in the data-source REST API (`api/rest/service/database/database.go:248`) so external query tools can target specific shards? Probably yes for debugging; defer until Phase 4 ships.
