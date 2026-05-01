# Design: Distributed Mode Database Locking Remediation

> Status: Proposed. Tracks remediation work for `database is locked` errors observed in 3-node distributed (dqlite) deployments. Items are grouped by phase; tick them off as PRs land.

## Problem Statement

Distributed-mode operators see `database is locked` errors and `unknown data type: 0` warnings in logs, especially when many task-heavy jobs run concurrently across 3+ nodes. End users observe stalled `ClaimNext` calls, lease expirations that fire late, and the occasional `RegisterTask` failure that aborts a run.

The audit identified four contributing factors, all of which are confirmed by code inspection:

1. **High-frequency global reclamation.** Every worker calls `ReclaimExpired` on every iteration of its loop (default 2s), producing redundant cluster-wide UPDATEs.
2. **Unbatched task registration.** Each task in a DAG is inserted in its own transaction, producing N transactions per job start.
3. **Synchronized polling / thundering herd.** Multiple nodes hit the same hot rows on the same cadence; jitter only applies when idle, not under load.
4. **Protocol warnings.** `unknown data type: 0` from go-dqlite suggests serialization stress during contention windows.

Code inspection surfaced additional root causes the audit missed; they're called out in [Findings](#findings) and addressed by the action items below.

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

- [ ] **Add `busy_timeout` and `synchronous` PRAGMAs to dqlite open.**
  - File: `pkg/dqlite/dqlite.go` (after `app.Open` at line 78, before the `sqlite_version()` query).
  - Run `PRAGMA busy_timeout=5000; PRAGMA synchronous=NORMAL;`. Do **not** set `journal_mode` — dqlite owns it.
  - Acceptance: PRAGMAs are set on the single dqlite conn at startup; `SELECT * FROM pragma_busy_timeout` returns 5000 after `Connection()`.
  - Risk: low. `synchronous=NORMAL` weakens single-node durability slightly, but dqlite's Raft replication provides durability at the cluster level.

- [ ] **Wrap `ClaimNext` and `ReclaimExpired` with bounded `SQLITE_BUSY`/`LOCKED` retry.**
  - File: `internal/worker/claimer.go`.
  - Helper `withBusyRetry(ctx, fn)` performing up to 5 attempts with exponential backoff (10ms, 20ms, 40ms, 80ms, 160ms) plus per-attempt jitter, total ~310ms cap. Reuse `isClaimContentionErr` (line 231).
  - Wrap the transaction calls at `claimer.go:67` and `:170`. Increment `caesium_worker_claim_contention_total` on each retry, not just on the first.
  - Acceptance: under simulated contention (see [Test plan](#test-plan)), `ClaimNext` returns success when `busy_timeout` plus retry can resolve it. Errors only bubble up after exhausting retries.
  - Risk: low. Retries are bounded; backoff is short.

- [ ] **Throttle `ReclaimExpired` so it does not run every iteration.**
  - File: `internal/worker/worker.go:66-69`.
  - Run reclaim only when `ClaimNext` returned no task in the previous iteration **and** at least `reclaimInterval` has passed since the last reclaim (default 30s, env-configurable as `CAESIUM_WORKER_RECLAIM_INTERVAL`).
  - Stagger by adding a per-worker random offset at `Worker` init so the cluster-wide reclaim cadence is naturally desynchronized.
  - Acceptance: in a 3-node cluster idle test, the rate of reclaim transactions falls by ~15× while no expired lease lingers more than `leaseTTL + reclaimInterval`.
  - Risk: low. Lease TTL is 5min by default; even a 30s reclaim cadence has ample headroom.

- [ ] **Surface a `caesium_db_busy_retries_total` counter and `caesium_reclaim_duration_seconds` histogram.**
  - File: `internal/metrics/metrics.go`.
  - Increment on every retry attempt; observe at the end of each `ReclaimExpired`.
  - Acceptance: metrics exposed at `/metrics`; visible in default Grafana dashboard config (`docs/observability.md` if present).
  - Risk: none.

### Phase 1 — Cut write volume (moderate, requires care)

Goal: collapse N transactions into 1 where possible; replace read-then-write with single CAS.

- [ ] **Batch task registration into a single transaction.**
  - Files: `internal/job/job.go:456-462`, `internal/run/store.go:356`.
  - Add `Store.RegisterTasks(runID uuid.UUID, inputs []RegisterTaskInput) error` that:
    1. Performs one `SELECT id, schema_validation, cache_config FROM jobs WHERE id = ?` and one `SELECT job_id FROM job_runs WHERE id = ?` ahead of the loop.
    2. Builds `[]models.TaskRun` records in memory.
    3. Inserts them with a single `tx.Create(&records)` (GORM batches into a multi-row INSERT).
    4. Inserts `task_ready` events for tasks with `outstanding_predecessors = 0` in a single `tx.Create(&events)`.
  - Keep existing `RegisterTask` as a thin wrapper that calls the batch path with one input, so callers outside `internal/job/job.go` continue to work.
  - Acceptance: registering a 50-task DAG produces 1 outer transaction and at most 3 SQL statements (jobs lookup, task_runs insert, events insert), confirmed via GORM logger in debug mode.
  - Risk: medium. Need to preserve idempotency — current code skips already-existing rows (`store.go:362-365`); preserve by pre-querying existing `task_id`s and excluding them from the batch.

- [ ] **Replace `ClaimNext` read-then-update with a single `UPDATE ... RETURNING`.**
  - File: `internal/worker/claimer.go:67-150`.
  - Issue one `UPDATE task_runs SET claimed_by = ?, claim_expires_at = ?, claim_attempt = claim_attempt + 1, status = 'running' WHERE id = (SELECT tr.id FROM task_runs tr JOIN job_runs jr ON jr.id = tr.job_run_id WHERE jr.status = 'running' AND tr.status = 'pending' AND tr.outstanding_predecessors = 0 AND (tr.claimed_by = '' OR tr.claim_expires_at IS NULL OR tr.claim_expires_at < ?) AND <node-selector predicate> ORDER BY tr.created_at ASC LIMIT 1) RETURNING *`.
  - dqlite supports `RETURNING` (already detected at `pkg/dqlite/dqlite.go:91`).
  - Node selector: if all selectors are equality on string keys, encode as SQL predicates against a normalized projection; otherwise keep a small candidate read (limit 8) and fall back to per-row CAS UPDATE for those matches.
  - Acceptance: the common case (no selectors / equality only) issues a single SQL statement per `ClaimNext`. Existing `claimer_test.go` cases pass, plus new tests covering selector matching and miss-the-race scenarios.
  - Risk: medium. Selector logic is the only tricky part; gate behind `CAESIUM_WORKER_CLAIM_MODE=single_statement` initially.

- [ ] **Raise `MaxOpenConns` for dqlite from 1 to 4 (configurable).**
  - File: `pkg/db/db.go:67`.
  - Add `CAESIUM_DATABASE_MAX_OPEN_CONNS` env var (default 4) and `CAESIUM_DATABASE_MAX_IDLE_CONNS` (default 2). Apply both to dqlite and postgres paths.
  - Acceptance: under a load test driving 50 concurrent claims, p99 `ClaimNext` latency drops at least 30% versus Phase 0 baseline. No `dqlite: database is locked` errors observed up to N=8.
  - Risk: medium-high. dqlite serializes writes at the leader regardless, but multiple local conns let reads and writes overlap. Too high may worsen leader-side contention or memory use; bake at 4 first.

- [ ] **Hoist `RegisterTask` per-row lookups out of the per-task loop.**
  - File: `internal/run/store.go:386,443`.
  - Folded into the batch refactor above; explicit checkbox so reviewers verify both lookups now happen once per batch.
  - Acceptance: `select * from jobs` and `select * from job_runs` execute at most once per `RegisterTasks` call regardless of input length (verify via GORM SQL log).
  - Risk: low.

### Phase 2 — Better cross-node coordination (moderate, optional after Phase 1 measurement)

Goal: reduce the need for tight polling once write contention is no longer the bottleneck.

- [ ] **Introduce distributed wakeups for task readiness.**
  - Files: `cmd/start/start.go:235-236`, new `internal/worker/wakeup_distributed.go`.
  - On task-ready / lease-expired events, fanout a small UDP or HTTP `POST /internal/wakeup` to peer node addresses (already known via `env.Variables().DatabaseNodes` in `pkg/dqlite/dqlite.go:67`).
  - Receiving node signals its local wakeup channel.
  - Authenticate with a shared secret (`CAESIUM_INTERNAL_WAKEUP_TOKEN`); drop unauthenticated requests.
  - Acceptance: registering tasks on node A causes node B to claim within `<200ms` instead of waiting for the next poll. Confirmed via integration test in `test/`.
  - Risk: medium. Network failures must not block event publication; treat wakeups as best-effort hints, not deliveries.

- [ ] **Make `ReclaimExpired` a leader-only or single-flight responsibility.**
  - File: `internal/worker/claimer.go:162`, `cmd/start/start.go`.
  - Option A (preferred): use `client.Leader()` from the dqlite client to check if this node is the current Raft leader; non-leaders skip reclaim.
  - Option B (fallback): add a `cluster_locks` row with `name='reclaim_expired'`, `held_by`, `expires_at`; nodes try to acquire via a CAS UPDATE, only the holder runs reclaim.
  - Acceptance: in a 3-node cluster, reclaim transactions execute on exactly one node at a time. Failover within `2 × reclaimInterval` if the leader/holder dies.
  - Risk: medium. Must handle leader churn cleanly; option B is simpler but adds a row.

- [ ] **Raise default poll interval from 2s to 15s** (only after distributed wakeups land).
  - Files: `pkg/env/env.go:60`, `internal/worker/worker.go:38`.
  - Acceptance: end-to-end run latency unchanged within 5% versus baseline once wakeups are in place; cluster-wide DB QPS for the task_runs table drops by an order of magnitude.
  - Risk: low after wakeups; high without them. Strictly gated on prior checkbox.

### Phase 3 — Cleanup and observability

Goal: investigate residual issues and tighten the operational surface area.

- [ ] **Investigate the `unknown data type: 0` log line.**
  - File: `pkg/dqlite/dqlite.go:47-61`.
  - Capture the surrounding context (statement / params) when `client.LogWarn` fires with this string. If it persists post-Phase 0, file an upstream issue against `canonical/go-dqlite`.
  - Acceptance: either the warnings are gone after Phase 0, or we have a minimal repro pinned to the code path.
  - Risk: none — investigative.

- [ ] **Add `caesium_task_register_batch_size` histogram.**
  - File: `internal/metrics/metrics.go`.
  - Observe the input length on each `RegisterTasks` call.
  - Acceptance: dashboards show batch-size distribution, useful for diagnosing degenerate single-task registrations.
  - Risk: none.

- [ ] **Document new env vars and operational guidance.**
  - File: `docs/configuration.md` (or wherever env vars are listed).
  - Cover `CAESIUM_WORKER_RECLAIM_INTERVAL`, `CAESIUM_DATABASE_MAX_OPEN_CONNS`, `CAESIUM_DATABASE_MAX_IDLE_CONNS`, `CAESIUM_INTERNAL_WAKEUP_TOKEN`, `CAESIUM_WORKER_CLAIM_MODE`.
  - Acceptance: docs PR merged alongside the last code change that introduces each var.
  - Risk: none.

- [ ] **Consider durable event outbox.** (Stretch.)
  - File: `internal/run/store.go:454`.
  - Today, `publishEvents` runs after the transaction commits; events are lost on crash. Move publication onto a polled outbox table.
  - Acceptance: event delivery survives a `SIGKILL` between commit and publish in an integration test.
  - Risk: medium. Orthogonal to locking but a known bug — schedule independently if reviewers prefer.

---

## Test plan

Each phase needs measurement before and after. All numbers below are illustrative targets, not gates.

- **Unit tests**
  - `internal/worker/claimer_test.go`: extend with a contention harness that fakes `SQLITE_BUSY` returns and asserts retry behaviour.
  - `internal/run/store_test.go`: add `RegisterTasks` cases — empty input, mixed new/existing tasks, idempotent re-registration, batch with mixed `outstanding_predecessors`.
- **Integration tests** (`test/`, run with `-tags=integration`)
  - 3-node dqlite cluster, 20 jobs × 50 tasks each, 4 concurrent runs per job. Assert no `database is locked` surfaced to callers; assert end-to-end completion within a budget.
- **Load test** (manual, documented in this PR)
  - Use the `just integration-test` harness extended with a contention scenario. Capture: `caesium_worker_claim_contention_total` rate, `caesium_db_busy_retries_total` rate, p50/p99 of `ClaimNext` and `ReclaimExpired`, count of `database is locked` log lines, end-to-end run wall time.

---

## Rollout

- Each phase ships behind feature flags / env vars where listed; default-on for Phase 0, default-on for Phase 1 once a release cycle passes, default-on for Phase 2 only after distributed wakeups have soaked.
- A single release note per phase, calling out new env vars, default changes, and any operational guidance (e.g., upgrading dqlite peers in lockstep).

## Open questions

- Is there appetite to adopt postgres for distributed mode as a longer-term option, or is dqlite a hard requirement? Several Phase 1/2 changes (single-statement claim with `RETURNING`, `MaxOpenConns`) are also wins on postgres but the calculus differs.
- Should `CAESIUM_WORKER_CLAIM_MODE` default to `single_statement` after Phase 1, or stay opt-in until a release cycle of telemetry validates it?
- Phase 2 distributed wakeups: UDP vs HTTP? UDP is lower overhead; HTTP reuses our existing TLS / auth story. Recommend HTTP unless profiling shows it matters.
