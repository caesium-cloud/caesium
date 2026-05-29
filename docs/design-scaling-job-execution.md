# Design: Scaling Job Execution Beyond the Sharded Dqlite Ceiling

> Status: Phase 0 (load harness), Phase 1 write-volume reduction, and Phase 2 run-owner coordination are **shipped** behind `CAESIUM_RUN_OWNER_ENABLED` (default off), with B3 in-memory advancement further gated by `CAESIUM_RUN_OWNER_IN_MEMORY` (default off). Remaining: in-process catalog cache (1.3), delta/incremental checkpoints, the `run_commands` mutation path, and the default-on rollout. Layers on [design-database-locking-fix.md](design-database-locking-fix.md) Phase 4; measurements in [load-testing-history.md](load-testing-history.md). The Phase 2 sections below are the **design of record for the shipped run-owner system**, with remaining items flagged inline.

## What shipped

| Phase | What | Status / gate |
|---|---|---|
| 0 | Load harness + `caesium_db_writes_total{category}` counters (`internal/metrics`) | ✅ shipped; baselines in [load-testing-history.md](load-testing-history.md) |
| 1.1 | Event coalescing — batched `execution_events` insert per transaction | ✅ shipped |
| 1.2 | Per-node lease-renewal batching | ✅ shipped |
| 1.4 | Predecessor-counter `UPDATE` batching | ✅ shipped |
| 1.3 | In-process catalog cache with bus-driven invalidation | ⬜ remaining |
| 2A | Run-owner substrate: `run_leases`, `owner_generation`, `POST /internal/dispatch`+`/internal/complete`, batched lease renewal | ✅ shipped |
| 2B (B2) | Dispatch→execute→complete cycle (SQL advancement) | ✅ shipped; active when `CAESIUM_RUN_OWNER_ENABLED=true` |
| 2B (B3) | In-memory DAG advancement (`run.RunState`), `run_checkpoints`, checkpoint/replay, owner-failover | ✅ shipped behind `CAESIUM_RUN_OWNER_IN_MEMORY` (default off) |
| — | Delta/incremental checkpoints (v1 writes full snapshots only; `RUN_CHECKPOINT_FULL_EVERY` is advisory) | ⬜ remaining |
| — | `run_commands` mutation path (cancel/retry/signal via the owner) | ⬜ remaining |
| — | mTLS on the internal endpoints | ✅ auto-provisioned — see [archive/design-internal-mtls-auto-provisioning.md](archive/design-internal-mtls-auto-provisioning.md) |

Code: `internal/dispatch/` (dispatch/complete RPCs, loop, mTLS), `internal/run/owner_state.go`, `internal/run/checkpoint_store.go`, `internal/run/recovery.go`, `internal/models/run_lease.go`, `internal/models/run_checkpoint.go`. Env vars in `pkg/env/env.go`: `RUN_OWNER_ENABLED`, `RUN_OWNER_IN_MEMORY`, `RUN_LEASE_TTL`, `RUN_CHECKPOINT_EVENTS`/`_INTERVAL`/`_FULL_EVERY`.

## Problem statement

The locking-fix plan delivered the small/medium shapes (3–50 nodes). Database sharding (its Phase 4) multiplies leader-side write throughput by giving each shard its own engine thread, but a single dqlite Raft cluster is ultimately bounded by the leader's NVMe IOPS, network, and per-shard engine thread. At the large shape (100–500 nodes, tens of thousands of concurrent task lifecycles) the dominant constraint shifts from *per-shard write contention* to *per-task write footprint × shard count* — past a point, more shards stop helping because they all sit on the same leader.

This design targets the next order of magnitude of cluster-wide task starts/sec without leaving dqlite as the canonical store, without external services (PostgreSQL, etcd, Kafka), and without changing the single-binary operational story. The leverage point: **dqlite is a good durable store but a poor coordination hot path** — moving coordination state into the owning process's memory (with periodic checkpoints) is the win. **Run-grain is the natural sharding boundary** (a DAG run is internally cohesive, externally independent), and the run-owner reuses PR #157's `hash(run_id) % N` key. **Workers stay stateless executors.**

Non-goals: multi-cluster federation, alternative durable stores (PostgreSQL stays an optional swap-in), dynamic resharding, and any job-authoring change (YAML/lint/dev/diff/apply/`pkg/jobdef` untouched — this is purely runtime architecture).

## Scaling target

| Shape | Nodes | Concurrent lifecycles | Task starts/sec | How |
|---|---|---|---|---|
| Small | 3–5 | hundreds | tens | Locking-fix Phase 0–1 |
| Medium | 10–50 | low thousands | hundreds | Locking-fix Phase 0–2 |
| Large | 100–500 | tens of thousands | low thousands | Locking-fix Phase 0–4 (sharding) |
| Extra-large | 100–500 | tens of thousands | tens of thousands | **This design**: write reduction + run-owner |

## Phase 0 — load harness (shipped)

The `caesium_db_writes_total{category}` counters (categories: `task_run_insert`, `task_run_status`, `event_insert`, `lease_renewal`, `checkpoint`, `command`, `callback`) and the parameterized DAG load harness shipped and produced the baseline series. The full Phase 0 → 2B measurement narrative — including the finding that `task_run_status` (44.4%) narrowly led `event_insert` (41.7%) as the dominant write category, which motivated Phase 1.4 — now lives in [load-testing-history.md](load-testing-history.md).

## Phase 1 — write-volume reduction (Family A)

Shrinks the per-task write footprint without changing the claim/shard model; composes with PR #157.

- **1.1 Event coalescing (shipped).** `internal/run/store.go` collects the events from a transaction (e.g. `CompleteTask` decrementing successors + emitting `task_ready`) into a single multi-row insert keyed by `sequence`; consumers dedupe on `sequence`. A 50-task DAG drops from ~150 event rows to ~10–20.
- **1.2 Lease-renewal batching (shipped).** A per-node ticker issues one `UPDATE task_runs SET claim_expires_at = ? WHERE claimed_by = ? AND id IN (...)` for all active claims, skipping renewal unless a claim is within `lease_ttl/2` of expiry. ~16× fewer renewal writes at pool size 16.
- **1.4 Predecessor-counter batching (shipped).** `completeTask`/`cacheHitTask`/`skipTaskAndDescendantsTx` issue one batched `UPDATE … WHERE job_run_id = ? AND task_id IN (...)` plus a SELECT of updated rows to find newly-zero successors; trigger-rule/branch logic runs on the in-memory result set, no extra round-trips. A fan-out=4 completion drops from 4+4 writes to 1+1.
- **1.3 In-process catalog cache (⬜ remaining).** Catalog reads (`jobs`, `tasks`, `task_edges`, `triggers`) hit the leader on every `RegisterTasks`/`ClaimNext`. Proposed: an in-memory LRU + per-row version cache on each node, invalidated by `catalog_updated` bus events, with a 30s belt-and-suspenders TTL. New `pkg/db/catalog_cache.go`; gate behind `CAESIUM_CATALOG_CACHE_ENABLED`. Target ≥99% hit ratio on `ClaimNext` reads, `<2s` p99 invalidation propagation.

**Phase 1 → 2 gate:** after Phase 1, re-running the harness showed status/predecessor UPDATEs as the next dominant category — exactly what the run-owner pattern eliminates — so Phase 2 proceeded.

## Phase 2 — run-owner coordination (Family B, shipped behind flags)

Each in-flight run is owned by exactly one node for its lifetime. The owner holds DAG state in memory and writes to dqlite as bounded-rate checkpoints + terminal-state-only rows, not per-transition writes. Shipped in two stages: **B2** (the dispatch→execute→complete cycle with SQL advancement, active when `CAESIUM_RUN_OWNER_ENABLED=true`) and **B3** (in-memory advancement + checkpoint/replay + failover, gated by `CAESIUM_RUN_OWNER_IN_MEMORY`).

### Owner election

The owner is `hash(run_id) % owner_eligible_nodes` (live nodes with `RUN_OWNER_ENABLED=true`) — separate from the run's storage shard (`hash(run_id) % CAESIUM_DATABASE_SHARDS`), though the shared key gives cache-affinity when the two counts match. On `RunStarted` the originating node writes a `run_leases` row (catalog DB):

```
run_leases (run_id TEXT PRIMARY KEY, owner_node TEXT, acquired_at DATETIME,
            lease_expires_at DATETIME, generation INTEGER)
```

Only the lease holder may write the run's hot rows, fenced at two layers: **in-process** (the data-layer router checks local node identity against the lease cache; mismatch → structured retry error) and **database** (`owner_generation` column on `task_runs`/`execution_events`/`callback_runs`/`run_checkpoints`; every coordination write includes `AND owner_generation = <current>`; zero rows affected ⇒ treated as a fence failure). Lease renewal piggybacks on the Phase 1.2 ticker. Owner death → lease expiry → next node CAS-acquires, incrementing `generation`; takeover bounded by `RUN_LEASE_TTL` (default 30s).

### In-memory owner state (B3)

Per owned run (`internal/run/owner_state.go`): DAG topology (loaded once), per-task status map, `outstanding_predecessors` counters, priority-ordered ready queue, in-flight lease tracker, un-checkpointed event ring, and a per-run monotonic sequence cursor. All ephemeral and derivable from `run_checkpoints` + terminal `task_runs` — the owner can lose memory at any time; recovery is bounded by the checkpoint interval.

### Dispatch and completion

The owner pushes ready tasks via `POST /internal/dispatch`; workers ack, execute, and POST results to `POST /internal/complete`. Both carry a fencing envelope:

```
DispatchEnvelope { run_id, task_id, owner_generation, attempt, worker_node, dispatch_token (128-bit nonce), deadline }
CompleteEnvelope { run_id, task_id, owner_generation, attempt, worker_node, dispatch_token,
                   status: succeeded|failed|cached,  result, outputs, error }
```

`/internal/complete` validates: run owned by this node; an outstanding dispatch record matches `(run_id, task_id, attempt)` with the right `dispatch_token`; `worker_node` matches; task is `running` in owner memory; `status ∈ {succeeded, failed, cached}`. Any mismatch → 409 + `caesium_complete_rejected_total{reason}`, completion not applied. This blocks stale-generation workers, duplicate re-dispatch completions, and forgery (the token is per-task-per-attempt, owner-memory-only). **`skipped` is excluded from worker-reported status** — it's an owner-side DAG decision (branch logic, trigger-rule propagation via `skipTaskAndDescendantsTx`/`markTaskSkippedTx`); workers report only execution outcomes. mTLS on the internal endpoints (auto-provisioned, distinct from `CAESIUM_INTERNAL_WAKEUP_TOKEN`) is the transport fence. ClaimNext is preserved as a **recovery-only** path for unowned tasks left by a crashed owner.

### Checkpointing

The owner writes a checkpoint every `min(RUN_CHECKPOINT_EVENTS, RUN_CHECKPOINT_INTERVAL)` (defaults 100 events / 2s) to the hot-shard `run_checkpoints` table (`run_id, sequence_high, created_at, owner_generation, state_proto BLOB, is_incremental`). `state_proto` holds the active (non-terminal) task states, outstanding-predecessor counters, ready queue, and cursors.

> **Remaining:** v1 writes **full snapshots only** — `RUN_CHECKPOINT_FULL_EVERY` is advisory until delta/incremental checkpoints land. The planned optimization (active-only snapshot + every-Nth-full with intervening deltas) keeps a 10,000-task / ~200-active run's checkpoint blob in the low-KB range instead of growing with total task count.

Terminal task states are always persisted as `task_runs` rows immediately (the durability backstop), stamped with a per-run monotonic `terminal_sequence` and the owner's `owner_generation`. A composite index `(job_run_id, terminal_sequence)` supports O(log N + K) recovery scans. Owner commit order: allocate sequence → durably persist terminal rows → advance in-memory state + dispatch → extend checkpoint coverage. A 1000-task run that produced ~3000+ writes becomes ~1000 terminal rows + ~30 checkpoints + ~30 event batches.

### Failure recovery

A new owner: CAS-acquires the lease (incrementing `generation`); reads the latest checkpoint; reads terminal `task_runs` where `terminal_sequence > checkpoint.sequence_high` ordered by `terminal_sequence` (wall-clock is deliberately unused — skew would lose/duplicate); reconstructs state; treats any `running` task without a post-checkpoint terminal row as an expired lease (re-dispatch with `attempt += 1`, fresh token); resumes the dispatch loop. `terminal_sequence` is dense per run, so a gap between `checkpoint.sequence_high + 1` and the next observed sequence means the owner crashed between allocation and persistence → treated as "no terminal write happened." Implemented in `internal/run/recovery.go`; exercised by `internal/run/failover_test.go`.

### External read/write surface

Reads are unchanged — UI/API query `task_runs` from dqlite. Two write cadences land there: **terminal writes** (immediate, never deferred) keep "did this finish?" strictly fresh, and **live-status snapshot writes** (pending→running, `claimed_by`, `lease_expires_at`) batch at checkpoint cadence (bounded staleness for in-flight UI state). The existing API contract is preserved; no consumer needs to know about the owner.

> **Remaining — mutations (`run_commands`).** Cancel/retry/signal through the owner is **not yet shipped**. The design: a catalog-DB `run_commands` table (`id, run_id, command_type, payload, created_at, applied_at, applied_by`) with a partial index `WHERE applied_at IS NULL`; push-then-poll delivery (a `run_command_created` bus event applies in tens of ms; a relaxed `CAESIUM_RUN_COMMAND_POLL_INTERVAL` ~10s fallback covers lease-handoff/bus-failure windows); applied in `created_at` order; pruned with cold-shard archival (or a 30d opportunistic vacuum for long-lived runs).

### Compatibility with PR #157 / the locking-fix plan

Same routing key (`hash(run_id) % N`); the owner co-locates with the run's shard by default, falling back to a least-loaded neighbor (recorded in the lease row). `run_leases`/`run_commands` live in the catalog DB (cross-run, low-volume); `run_checkpoints` lives in the run's hot shard. Locking-fix Phase 2 (distributed wakeups, leader-only reclaim, spare topology) stays useful: wakeups become the cross-shard hint channel, leader-only reclaim applies to the recovery ClaimNext path, spare topology is how workers scale.

## Data-flow comparison

**Before:** trigger → `RegisterTasks` (1 txn) → each worker polls ClaimNext (1 read + 1 UPDATE + 2 events/claim) → lease renewals (1 UPDATE/renewal) → `CompleteTask` (1 + N successor UPDATEs + N events). ≈ 6–10 row writes/task.

**With run-owner:** trigger → catalog read (cached, ~0 DB) → owner acquires lease (1 write), loads DAG → push dispatch (HTTP, no DB) → lease tracked in memory → `/internal/complete` advances in-memory state (no per-transition DB write) → per batch: terminal `task_runs` rows + 1 batched event insert → per checkpoint (~2s): 1 `run_checkpoints` row + 1 batched live-status UPDATE. ≈ 1–1.5 writes/task amortized — a 4–8× reduction on top of Phase 1's 3–5×.

## Failure modes

| Failure | Detection | Recovery | Worst case |
|---|---|---|---|
| Worker crash mid-task | owner lease expiry on the dispatched task | owner reassigns (`attempt += 1`) | one task re-executes |
| Owner crash | `run_leases.lease_expires_at` passes | new owner CAS-acquires, replays checkpoint + terminal rows | one checkpoint interval of in-flight work re-dispatched |
| Network partition (owner isolated) | owner can't renew lease → loses ownership | stale owner's completes rejected by generation/token fence; direct DB writes rejected by the `owner_generation` predicate | brief duplicate-dispatch window; idempotency on `(run_id, task_id, attempt)` + fences mean no terminal state applied twice |
| Stale/forged complete RPC | `/internal/complete` fence (generation, attempt, token, worker) → 409 | dropped, `caesium_complete_rejected_total{reason}` | no correctness loss; observability for partition/misconfig |
| Hot-shard leader change | dqlite role transition | owner writes briefly fail, retried with backoff | bounded checkpoint lag (~1s) |
| Bug in checkpoint replay | replay fails on takeover | fall back to "rebuild from terminal rows only" — slower but correct; alarm | longer recovery for affected runs |

## Testing

Unit (`internal/run/`): owner state machine; checkpoint + terminal-row replay across all four terminal statuses (explicitly `cached` and owner-applied `skipped`); `/internal/complete` rejects non-`{succeeded,failed,cached}` status; sequence-based recovery with out-of-`created_at`-order but in-`terminal_sequence`-order rows, and gap handling; DB-level generation fence (stale `owner_generation` → zero rows); lease CAS race; per-reason fence rejection table; mTLS-prerequisite startup check. Integration (`test/`, `-tags=integration`): 3-node, owner-enabled, 20×50 tasks asserting per-task write count and completion budget; owner-crash injection (no task terminal-applied twice); partition test (in-flight task reassigned, original dropped on heal). Load: Phase 1-only vs Phase 1+2 on identical workloads; 20-node `CAESIUM_DATABASE_SHARDS=8`, 200×100 tasks, target ≥5× task starts/sec. **Remaining test work tracks the remaining features** (catalog-cache propagation, delta-checkpoint replay, `run_commands` delivery).

## Rollout

- Phase 0 + Phase 1.1/1.2/1.4: default-on (shipped).
- Phase 1.3 catalog cache: ship behind `CAESIUM_CATALOG_CACHE_ENABLED` (default off) for one release, then default-on (remaining).
- Phase 2: shipped behind `CAESIUM_RUN_OWNER_ENABLED` (default off); B3 in-memory further behind `CAESIUM_RUN_OWNER_IN_MEMORY` (default off). Default-on only after a multi-month soak in a Caesium-operated reference cluster with the recovery path exercised under partition + crash injection. When disabled, the path is bypassed entirely.
- mTLS on `/internal/dispatch`+`/internal/complete` is auto-provisioned (see [archive/design-internal-mtls-auto-provisioning.md](archive/design-internal-mtls-auto-provisioning.md)); credentials are distinct from `CAESIUM_INTERNAL_WAKEUP_TOKEN`.

## Operational risks

| Risk | Mitigation |
|---|---|
| Owner hotspot for wide runs | per-node concurrent-runs cap (`CAESIUM_RUN_OWNER_MAX_RUNS`, planned default 1024); excess routes to least-loaded peer |
| Checkpoint bursts saturate hot-shard leader | per-shard checkpoint write rate-limit + backpressure |
| Stale-owner duplicate dispatch | per-(task, attempt) `dispatch_token` + generation fence; tokens are owner-memory-only so a replayed token can't mark a task terminal |
| Large-DAG checkpoint blob | active-only state + incremental checkpoints (remaining) + `RUN_CHECKPOINT_FULL_EVERY` |
| `run_commands` growth | bus-driven delivery, partial index, cold-shard archival, 30d vacuum (with the feature) |
| Unbounded recovery tail on long runs | cap un-checkpointed tail at `2 × RUN_CHECKPOINT_INTERVAL`; force a checkpoint if exceeded |

## Open questions

- `run_leases` in the catalog DB (simpler "who owns what") vs the run's hot shard (avoids a cross-DB read on takeover)? Currently catalog DB.
- Dispatch transport HTTP/JSON (reuses the internal-endpoint pattern) vs gRPC (more efficient at high dispatch rates)? Currently HTTP; revisit by measurement.
- Should worker nodes hold a local read replica of their owned-shard `task_runs` for colocated UI reads? Defer until measurement shows UI read latency is a real problem.
- How does `CAESIUM_RUN_OWNER_MAX_RUNS` interact with autoscaling? An autoscaled-away pod's owned runs need clean handoff (a graceful-shutdown drain) rather than waiting for lease expiry.

## References

- [design-database-locking-fix.md](design-database-locking-fix.md) — the locking-fix plan whose Phase 4 this layers on.
- [design-parallel-job-execution.md](archive/design-parallel-job-execution.md) — the (archived) original distributed-mode execution model.
- [parallel-execution-operations.md](parallel-execution-operations.md) — operator-facing config and rollout.
- [database-sharding.md](database-sharding.md) — table placement and routing contract (PR #157).
- [load-testing-history.md](load-testing-history.md) — Phase 0 → 2B measurement record.
- Temporal "history shards" — the single-owner-per-workflow pattern this adapts to Caesium's run-grain.
