# Design: Scaling Job Execution Beyond the Sharded Dqlite Ceiling

> Status: Proposed. Layers on top of [design-database-locking-fix.md](design-database-locking-fix.md) Phase 4 (PR #157). Tracks the next throughput frontier — cluster-wide task starts/sec — while preserving Caesium's "embedded dqlite is the only persistence operators back up" tenet.

## Problem Statement

The locking-fix plan delivered the small and medium deployment shapes (3–50 nodes, hundreds to low-thousands of concurrent task lifecycles). Phase 4 of that plan adds dqlite database sharding, which multiplies leader-side write throughput by giving each shard its own engine thread and write queue.

Database sharding alone is bounded. A single dqlite Raft cluster is ultimately constrained by the leader's NVMe IOPS, network bandwidth, and the per-shard engine thread. At the large shape (100–500 worker nodes, tens of thousands of concurrent task lifecycles) we expect the dominant constraint to shift from *per-shard write contention* to *per-task write footprint × shard count*. Past a point, opening more shards stops helping because every shard still sits on the same leader.

This design targets the next order of magnitude of cluster-wide task starts/sec without leaving dqlite as the canonical durable store, without introducing external services (PostgreSQL, etcd, Kafka), and without changing the single-binary, single-bootstrap operational story.

## Goals

- Lift cluster-wide task starts/sec at least one order of magnitude above the sharded-dqlite ceiling measured after the locking-fix Phase 4 lands.
- Keep dqlite as the only persistence layer operators back up and configure.
- Compose with — not replace — PR #157's sharding. The run-owner pattern uses the same hash routing key, so a run's owner and its hot-shard rows live on related (ideally co-located) nodes.
- Be measurement-driven: every architectural decision is justified by a number from a load harness, not by intuition.

## Non-Goals

- Multi-cluster federation. Going beyond a single Raft cluster is a different conversation.
- Alternative durable stores. PostgreSQL remains an optional swap-in for operators who prefer it; the *embedded* path stays primary and gets the new architecture first.
- Dynamic resharding. `CAESIUM_DATABASE_SHARDS` remains static per PR #157.
- Job authoring changes. YAML, lint, dev, diff, apply, the schema in `pkg/jobdef` — all untouched. This is purely runtime architecture.

## Scaling Target

| Shape | Nodes | Concurrent task lifecycles | Cluster-wide task starts/sec | How |
|---|---|---|---|---|
| Small | 3–5 | hundreds | tens | Locking-fix Phase 0–1. |
| Medium | 10–50 | low thousands | hundreds | Locking-fix Phase 0–2. |
| Large | 100–500 | tens of thousands | low thousands | Locking-fix Phase 0–4. |
| Extra-large | 100–500 | tens of thousands | tens of thousands | **This design**: Phase 1 write reduction + Phase 2 run-owner. |

The architectural anchors that make extra-large reachable without external infra:

- **dqlite is good at being a durable store; it is not good at being a coordination hot path.** Moving coordination state out of the DB (into the owning process's memory, with periodic checkpoints) is the leverage point.
- **Run-grain is the natural sharding boundary.** A DAG run is internally cohesive (tasks within it talk to each other) and externally independent (different runs don't share state). PR #157 already shards storage by `hash(job_run_id) % N`; this design extends the same key to shard *coordination*.
- **Workers stay stateless executors.** All run-coordination intelligence consolidates on the owner. Worker pods can be scaled freely, restarted, drained, autoscaled — the owner's lease machinery handles the rest.

## Architectural Approach

Three families of techniques were considered. The design pursues A then B in sequence; C is held in reserve as a smaller fallback if B proves too invasive.

**Family A — Write-volume reduction on the existing shape (Phase 1).** Shrink the per-task write footprint without changing the claim/poll/shard model. Event coalescing, lease-renewal batching, in-process catalog cache. Composes directly with PR #157.

**Family B — Run-owner coordination (Phase 2).** Each run is owned by exactly one node for its lifetime. Owner holds DAG state in memory, dispatches tasks to workers via push, persists checkpoints + terminal-state-only rows to the run's hot shard. Eliminates per-transition DB writes. This is the order-of-magnitude move.

**Family C — Push dispatch + per-node read replicas (deferred).** Replace ClaimNext with leader-pushed dispatch, without the in-memory run-state of Family B. Considered as a fallback if B is too invasive; otherwise subsumed by B (Family B *is* push dispatch, plus owner state).

## Phase 0: Load Harness

Before any code in Phase 1 or 2, build a load harness that answers two questions:

1. Which write category dominates per task lifecycle? Candidates: `task_runs` inserts, `execution_events` inserts, status UPDATEs from claim/complete, lease renewals, callback writes.
2. What is the per-shard leader ceiling? Drive synthetic-DAG runs at increasing concurrency until per-shard `caesium_db_busy_retries_total` and p99 ClaimNext latency knee.

**Deliverables.**

- `test/load/` harness invoked via `just load-test` that generates parameterized DAG workloads — fan-out width, depth, task duration (mock task images that sleep + emit one output line).
- New Prometheus counters: `caesium_db_writes_total{category}` where `category ∈ {task_run_insert, task_run_status, event_insert, lease_renewal, checkpoint, command, callback}`.
- A baseline report at `docs/load-baseline-YYYY-MM-DD.md` capturing: dominant write category, per-shard write ceiling on a reference hardware profile (single NVMe voter), p50/p99 of ClaimNext and end-to-end task latency, dqlite leader CPU and RSS at saturation.

Phase 0 is one PR; it gates Phase 1 sequencing and provides the comparison baseline for measuring later phases.

## Phase 1: Write-Volume Reduction (Family A)

Three tactical PRs. Each is independently shippable and each tightens the per-task write footprint.

### 1.1 Event coalescing

Today `internal/run/store.go` writes one `execution_events` row per lifecycle transition. Inside a single transaction context — e.g., `CompleteTask` decrementing successors and emitting `task_ready` for those that reached zero — collect the resulting events into a single multi-row INSERT keyed by per-event `sequence`. The bus dispatcher already handles batched delivery; SSE / lineage / notifications consumers continue to dedupe on `sequence`.

- Files: `internal/run/store.go`, `internal/event/bus.go` (verify batched dispatch path).
- Risk: low. Sequence already enforces idempotency.
- Expected impact: a 50-task DAG drops from ~150 event rows to ~10–20 (roughly one batch per fan-out boundary).
- Acceptance: a per-shard `caesium_db_writes_total{category="event_insert"}` rate falls ≥5× on the Phase 0 baseline workload while end-to-end DAG completion time is unchanged within 5%.

### 1.2 Lease renewal batching per node

Today each in-flight claim renews its own lease in `internal/worker/runtime_executor.go`. Replace with a per-node ticker that issues a single `UPDATE task_runs SET claim_expires_at = ? WHERE claimed_by = ? AND id IN (...)` covering every active claim on the node. Skip the renewal if no in-flight claim is within `lease_ttl/2` of expiry.

- Files: `internal/worker/runtime_executor.go`, `internal/worker/worker.go`.
- Risk: low. Bounded by `CAESIUM_WORKER_POOL_SIZE` per node.
- Expected impact: with pool size 16, drops lease-renewal write rate by ~16× per node.
- Acceptance: lease-renewal write count per node decreases proportionally to pool size; no lease expiry surprises in the locking-fix integration tests.

### 1.3 In-process catalog cache with bus-driven invalidation

Catalog reads (`jobs`, `tasks`, `task_edges`, `triggers`) hit the leader for every `RegisterTasks` and every `ClaimNext`. Add an in-memory LRU + per-row version cache on every node, invalidated by `catalog_updated` events fanned through the existing event bus. First read is a normal DB hit; subsequent reads serve from cache until the version cursor advances.

- Files: new `pkg/db/catalog_cache.go`, hooks in `internal/run/store.go` reads of catalog tables, publishers in `api/rest/service/job` etc. that emit `catalog_updated`.
- Risk: medium. Stale reads on partition. Mitigate with a short TTL (30s) as belt-and-suspenders, and a startup `SELECT max(updated_at)` cursor to seed the version map.
- Expected impact: catalog read QPS to leader drops ~10–100× depending on job churn.
- Acceptance: a synthetic catalog-update propagates to all nodes' caches within `<2s` p99; cache hit ratio on `ClaimNext` reads is ≥99% at steady state.

### Phase 1 → Phase 2 gate

After all three Phase 1 PRs land, re-run the Phase 0 harness. If the per-shard ceiling has moved past your target throughput, Phase 2 is deferred. Phase 2 proceeds only if the new measurement shows status UPDATEs and predecessor-counter UPDATEs are the next dominant write category — which is the bottleneck the run-owner pattern is specifically designed to eliminate.

## Phase 2: Run-Owner Coordination (Family B)

Each in-flight run is owned by exactly one node for its lifetime. The owner holds the run's coordination state in memory and writes to dqlite as bounded-rate checkpoints + terminal-state-only persistence, not per-task transitions.

### Owner election

The owner is the node assigned to a run by `hash(run_id) % owner_eligible_nodes`, where `owner_eligible_nodes` is the set of currently-live nodes with `CAESIUM_RUN_OWNER_ENABLED=true`. This is *separate from* the database shard the run's hot rows live in (`hash(run_id) % CAESIUM_DATABASE_SHARDS`, per PR #157) — there is one dqlite Raft cluster with one leader, and any owner's writes round-trip to that leader. Using the same hash key gives natural cache-affinity if `owner_eligible_nodes == CAESIUM_DATABASE_SHARDS`, but the two concepts are independent and either can be reconfigured without touching the other.

On `RunStarted` the originating node writes a `run_leases` row in the catalog DB:

```
run_leases (
  run_id          uuid primary key,
  owner_node      text not null,
  acquired_at     timestamptz not null,
  lease_expires_at timestamptz not null,
  generation      bigint not null
)
```

Only the lease holder is permitted to write to that run's hot rows (enforced at the data-layer router by checking the local node identity against the in-process lease cache; mismatch → reject with a structured error so the caller can retry against the new owner).

Lease renewal piggybacks on the per-node lease-renewal ticker built in Phase 1.2 — one batched UPDATE per node covers every run lease that node owns.

Owner death → lease expiry → next-in-line node acquires via CAS UPDATE on `run_leases` incrementing `generation`. Lease takeover is bounded by `run_lease_ttl + jitter` (default 30s, configurable via `CAESIUM_RUN_LEASE_TTL`).

### State held in memory by the owner

For every owned run:

- DAG topology (loaded once from catalog cache; constant for the run's lifetime).
- Per-task status map: `taskID → {status, claimed_by, lease_expires_at, attempt}`.
- `outstanding_predecessors` counter per task.
- Ready queue ordered by priority + creation time.
- In-flight lease tracker for tasks dispatched to worker nodes.
- Ring buffer of un-checkpointed events.
- Sequence cursor (monotonically increasing per run).

All of this is ephemeral and derivable from `run_checkpoints` + terminal `task_runs` rows in dqlite. The owner process can lose its memory at any time; recovery is bounded by checkpoint interval.

### Dispatch and completion path

Owner pushes ready tasks to worker nodes over an internal endpoint `POST /internal/dispatch`. Workers acknowledge dispatch synchronously, execute the task, and POST result + outputs back to `POST /internal/complete` on the owner. Owner updates in-memory state, advances any newly-ready tasks, appends to the un-checkpointed event ring, and the cycle continues.

Both endpoints carry a fencing envelope on every message:

```
DispatchEnvelope {
  run_id, task_id            // identifies the work
  owner_generation: int64    // current run_leases.generation when dispatch was issued
  attempt: int               // monotonically-increasing per task retry
  worker_node: string        // intended recipient (worker rejects if mismatch)
  dispatch_token: bytes      // 128-bit nonce, generated by owner per (task, attempt)
  deadline: timestamp
}

CompleteEnvelope {
  run_id, task_id, owner_generation, attempt, worker_node, dispatch_token
  status: succeeded | failed | skipped | cached
  result, outputs, error
}
```

Validation rules at the owner's `/internal/complete` handler:

1. Run is currently owned by this node (`run_leases.generation == owner_generation && holder == self`).
2. Owner has an outstanding dispatch record for `(run_id, task_id, attempt)` and the stored `dispatch_token` matches.
3. `worker_node` matches the recipient from the original dispatch.
4. Task is currently in `running` status in owner memory.

Any mismatch → 409 with a structured error; the completion is *not* applied. This prevents (a) stale workers from a previous owner generation, (b) duplicate completions after a re-dispatched retry, (c) completion forgery by any holder of the shared internal token — the dispatch token is per-task per-attempt, generated and held only by the current owner. Worker identity is asserted by the dispatched recipient string, not derived from the request; mTLS on the internal endpoints (separate credentials from the existing `CAESIUM_INTERNAL_WAKEUP_TOKEN`) is the recommended secondary fence and is required for `CAESIUM_RUN_OWNER_ENABLED=true`.

Selection uses the existing node-label affinity rules + a simple least-loaded heuristic — workers report their current in-flight count in the dispatch ACK.

ClaimNext is preserved as a **recovery path only** — used when a worker discovers an unowned ready task left behind by a crashed owner whose lease has not yet been reaped. The Phase 1 Family A optimizations to ClaimNext remain in place but its hot-path role is gone.

### Checkpointing

Owner writes a checkpoint to a new hot-shard table `run_checkpoints` every `min(N events, T seconds)` — proposed defaults 100 events or 2 seconds, configurable via `CAESIUM_RUN_CHECKPOINT_EVENTS` and `CAESIUM_RUN_CHECKPOINT_INTERVAL`.

```
run_checkpoints (
  run_id            uuid not null,
  sequence_high     bigint not null,    -- highest event sequence covered
  created_at        timestamptz not null,
  state_proto       bytea not null,     -- compact protobuf of in-memory state
  primary key (run_id, sequence_high)
)
```

`state_proto` is a compact protobuf:

```proto
message RunState {
  map<string, TaskState> tasks = 1;   // taskID → status, attempt, claimed_by, lease_expires_at
  map<string, int32> outstanding_predecessors = 2;
  repeated string ready_queue = 3;    // task IDs in dispatch order
  int64 sequence_high = 4;
}
```

Old checkpoints are pruned: keep last N=3 per run for safety, then delete on terminal-run archival.

**Terminal task states** are always persisted as `task_runs` rows immediately, never deferred. The terminal vocabulary matches the existing code: `succeeded`, `failed`, `skipped`, `cached` (see `internal/run/store.go:43-48` and the `IsTerminalSuccess` helper at `:52`). A future helper `IsTerminal(status)` should be introduced and reused everywhere terminal detection happens (owner replay, archiver, recovery scan) so the vocabulary lives in one place.

Every terminal `task_runs` write is stamped with the owner's current per-run event sequence cursor (`terminal_sequence` column, monotonically increasing per run, never reused). This is what makes recovery deterministic — see below.

The owner's commit order is:

1. Allocate the next event-sequence number for this batch of completes.
2. Durably persist terminal `task_runs` rows in batch with `terminal_sequence = <allocated>`.
3. Advance in-memory state and dispatch newly-ready tasks.
4. Opportunistically extend checkpoint coverage.

A 1000-task run that today produces ~3000+ row writes becomes ~1000 terminal rows + ~30 checkpoints + ~30 event batches.

### Failure recovery

New owner:

1. CAS-acquires the `run_leases` row (incrementing `generation`).
2. Reads the latest `run_checkpoints` row for the run.
3. Reads all `task_runs` rows for the run where `IsTerminal(status)` AND `terminal_sequence > checkpoint.sequence_high` — terminal rows that landed after the checkpoint, ordered by `terminal_sequence` for deterministic replay. Wall-clock timestamps are deliberately not used; ties or clock skew would silently lose or duplicate terminal application.
4. Reconstructs in-memory state: apply checkpoint, then apply terminal rows in `terminal_sequence` order to update statuses and decrement successor predecessor counts.
5. Any task whose status was `running` in the reconstructed state but has no post-checkpoint terminal row is treated as an expired lease — re-dispatched with `attempt += 1` and a fresh `dispatch_token`.
6. Begin normal dispatch loop.

The `terminal_sequence` column is per-run monotonic and dense from the owner's perspective: every terminal write uses the next sequence after the last issued. This also gives the recovery scan a cheap correctness check — gaps between `checkpoint.sequence_high + 1` and the next observed `terminal_sequence` should not exist; if they do, the owner crashed between sequence allocation and row persistence, and the gap is treated as "no terminal write happened" (re-dispatch).

Worst-case work loss on owner crash: tasks that completed but whose terminal write was still buffered. The commit-order rule above (terminal writes before in-memory advance) makes this an empty set in normal operation; the only loss is in-flight-but-not-yet-completed work, which is re-dispatched.

### External read/write surface

**Reads.** UI and external API continue to query `task_runs` from dqlite as before. Two write cadences land on the same `task_runs` table:

- *Terminal writes* (`succeeded`, `failed`, `skipped`, `cached`) — emitted immediately on every batch of completes the owner processes. Never deferred. This is the durability backstop and keeps "did this task finish?" queries strictly fresh.
- *Live-status snapshot writes* — UPDATEs to non-terminal columns (`status` for `pending→running`, `claimed_by`, `lease_expires_at`) batched at checkpoint cadence. UI sees bounded-staleness in-flight state (≤ checkpoint interval) but never stale terminal state.

This preserves the existing API contract; no UI or external query needs to know about the owner.

**Mutations.** Cancel, retry, signal, manual intervention go through a per-run `run_commands` table in the catalog DB:

```
run_commands (
  id              uuid primary key,
  run_id          uuid not null,
  command_type    text not null,        -- cancel, retry, signal, ...
  payload         jsonb,
  created_at      timestamptz not null,
  applied_at      timestamptz,
  applied_by      text                  -- owner_node:generation
)
```

Owner polls `run_commands WHERE run_id IN (owned) AND applied_at IS NULL` on its tick (every ~1s). Commands are applied in `created_at` order; applied row is marked atomically with the operation it caused. Owner unavailable → commands queue; next owner on lease takeover drains the queue before resuming dispatch.

### Compatibility with PR #157 and the locking-fix plan

- Same routing key (`hash(run_id) % N`). The owner for run R lives on the node hosting shard `hash(R) % N` by default; if that node's run-owner slot is full, fall back to least-loaded neighbor and write the assignment in the lease row.
- `run_checkpoints`, `run_leases`, `run_commands` are new tables. `run_leases` and `run_commands` live in the catalog DB (cross-run, low-volume). `run_checkpoints` lives in the hot shard for its run (per-run, transactionally local to other run rows).
- Phase 1 of the locking-fix plan (event coalescing, lease batching, catalog cache) is *Phase 1 of this design* — same work, called out here to make the layering explicit.
- Phase 2 of the locking-fix plan (distributed wakeups, leader-only reclaim, spare topology) remains useful: wakeups become the *cross-shard* hint channel, leader-only reclaim still applies to the recovery ClaimNext path, spare topology is how worker nodes scale.
- Phase 4 of the locking-fix plan (sharding) is the storage substrate this design assumes. Run-owner is the coordination layer on top.

## Data Flow Walkthrough

**Today (per the locking-fix Phase 0–4 plan).**

1. Trigger fires on leader → `job.Run` builds DAG → `RegisterTasks` batches inserts (1 transaction).
2. Each worker on each node polls ClaimNext (1 read + 1 UPDATE + 2 events per claim).
3. Worker executes task; renews lease periodically (1 UPDATE per renewal per task).
4. Worker calls `CompleteTask` (1 UPDATE on the task + N UPDATEs on successors + N events).
5. Repeat until DAG done.
6. Per-task write count ≈ 6–10 rows (events + status transitions + lease renewals).

**With this design.**

1. Trigger fires on leader → `job.Run` builds DAG → catalog reads from local cache (Phase 1.3, 0 DB hits steady state) → owner acquires `run_leases` row (1 write) → loads DAG into memory.
2. Owner pushes ready tasks to workers via `POST /internal/dispatch` (HTTP, no DB).
3. Worker executes; lease tracked in owner's memory (no DB write per renewal).
4. Worker calls `POST /internal/complete` → owner appends to event ring, advances in-memory state, dispatches newly-ready tasks (no DB write per transition).
5. Per batch of completes processed: owner writes terminal `task_runs` rows + 1 batched `execution_events` insert. (Terminal writes are never deferred — they're the durability backstop.)
6. Every checkpoint interval (~2s): owner writes 1 `run_checkpoints` row + 1 batched UPDATE of non-terminal `task_runs` rows (the "live status snapshot" — pending→running transitions, current `claimed_by`, current `lease_expires_at` for UI/API consumers).
7. Per-task write count ≈ 1 terminal row + amortized share of 1 checkpoint + amortized share of 1 event batch ≈ 1–1.5 writes amortized.

A 4–8× reduction in per-task write footprint, on top of Phase 1's 3–5× reduction in the per-write categories that remain.

## Failure Modes

| Failure | Detection | Recovery | Worst-case impact |
|---|---|---|---|
| Worker crash mid-task | Owner's lease expiry on dispatched task | Owner reassigns task (`attempt += 1`) | One task re-executes |
| Owner crash | `run_leases.lease_expires_at` passes | Another node CAS-acquires, replays checkpoint + terminal rows | One checkpoint interval of in-flight work re-dispatched |
| Network partition (owner isolated) | Owner cannot renew lease → loses ownership. New owner re-dispatches with fresh `dispatch_token` and incremented `attempt`. Stale owner's completes are rejected at `/internal/complete` by generation/token fence. | Stale owner drops state on next lease-renewal failure | Brief duplicate-dispatch window; worker-side idempotency on `(run_id, task_id, attempt)` plus owner-side fence rejection means no terminal state can be applied twice |
| Stale or forged complete RPC | `/internal/complete` fence check (generation, attempt, dispatch_token, worker_node) returns 409 | Owner emits a `caesium_complete_rejected_total{reason}` counter; rejected completion is dropped without state change | No correctness loss; observability surface for ongoing partition or misconfigured worker |
| Hot-shard leader change | dqlite role transition | Owner's writes briefly fail; retried with backoff | Bounded checkpoint lag (~1s typical) |
| Worker dispatch endpoint unreachable | Owner sees POST failure | Owner retries with backoff; falls back to writing the task to its hot shard with `claimed_by=""` so ClaimNext recovery path picks it up | Increased task start latency; no correctness loss |
| Bug in checkpoint format / replay | Detected on owner takeover, replay fails | Fall back to "rebuild from terminal rows only" — slower, but correct; alarm + page | Increased recovery time for affected runs |

## Testing Strategy

- **Unit tests.**
  - Owner state machine: status transitions, predecessor decrements, ready-queue ordering, checkpoint serialization round-trips.
  - Checkpoint + terminal-row replay: synthesize a checkpoint + N terminal rows covering all four terminal statuses (`succeeded`, `failed`, `skipped`, `cached`); assert reconstructed state equals state at the time of crash. Explicitly cover `cached` since it is the easiest status to miss in a terminal helper.
  - Sequence-based recovery: replay with terminal rows arriving out of `created_at` order but in correct `terminal_sequence` order; assert deterministic outcome. Replay with a gap in `terminal_sequence`; assert the gap'd task is treated as expired and re-dispatched.
  - Lease CAS: concurrent owner acquisition test (two goroutines race, one wins, one observes generation mismatch).
  - Fence rejection: `/internal/complete` table-driven test covering each rejection cause — stale `owner_generation`, stale `attempt`, wrong `worker_node`, missing or mismatched `dispatch_token`, run not currently owned by this node. Each case asserts the rejection counter increments and state is unchanged.
- **Integration tests** (`test/`, `-tags=integration`).
  - 3-node cluster, run-owner enabled, 20 jobs × 50 tasks. Assert per-task DB write count via the new `caesium_db_writes_total` counters; assert end-to-end DAG completion within Phase 0 baseline budget.
  - Owner-crash injection: kill the owner node mid-run, assert another node acquires, run completes, no task executed >1 time at terminal state (idempotency check via `attempt` field).
  - Partition test: simulate network partition between owner and one worker; assert worker's in-flight task is reassigned, original execution is dropped on partition heal.
- **Load test.**
  - The Phase 0 harness extended to compare Phase 1-only vs Phase 1+2 throughput on identical workloads.
  - 20-node cluster, `CAESIUM_DATABASE_SHARDS=8`, 200 jobs × 100 tasks. Target: ≥5× cluster-wide task starts/sec vs Phase 1-only baseline.

## Rollout

- Phase 0 ships as one PR. Default-on (the harness is just code; metrics are always emitted).
- Phase 1 ships as three PRs. Default-on for 1.1 and 1.2 (low risk). 1.3 ships behind `CAESIUM_CATALOG_CACHE_ENABLED` (default off) for one release, then default-on.
- Phase 2 ships behind `CAESIUM_RUN_OWNER_ENABLED` (default off) indefinitely. Default-on only after a multi-month soak in a Caesium-operated reference cluster, and once the recovery path has been exercised in integration tests under partition + crash injection.
- When `CAESIUM_RUN_OWNER_ENABLED=false`, the entire run-owner path is bypassed; the system behaves exactly as Phase 1 + PR #157.

## Operational Risks

| Risk | Mitigation |
|---|---|
| Owner becomes a hotspot for runs with many concurrent tasks | Per-node concurrent-runs cap (`CAESIUM_RUN_OWNER_MAX_RUNS`, default 1024); excess runs route to least-loaded peer |
| Checkpoint write bursts saturate hot-shard leader | Per-shard checkpoint write rate-limit; backpressure to owners via 429-equivalent |
| Stale-owner duplicate dispatch on partition | Per-(task, attempt) `dispatch_token` plus owner generation in dispatch/complete envelopes; owner rejects completes with stale generation/token/attempt/worker. Tokens are owner-memory-only — a new owner re-dispatches with a fresh token, so a forged or replayed token from any prior generation cannot mark a task terminal |
| Shared bearer token leaked or misused for internal endpoints | Run-owner dispatch/complete require mTLS (or a token distinct from `CAESIUM_INTERNAL_WAKEUP_TOKEN`); the dispatch fence is the primary safety, mTLS is the secondary |
| Catalog cache staleness causes claim of cancelled run | Always check `run_leases.generation` on completion writes; reject if stale |
| Recovery scan grows unbounded for very-long-running runs | Cap un-checkpointed tail at `2× CAESIUM_RUN_CHECKPOINT_INTERVAL`; force a checkpoint if exceeded |

## Open Questions

- Should `run_leases` live in the catalog DB or in each run's hot shard? Catalog DB is simpler (one place to query for "who owns what") but adds a tiny cross-database read on every owner takeover. Hot shard avoids that read but complicates the "who owns run R?" query (need to know R's shard first).
- Should the dispatch path use HTTP/JSON or gRPC? HTTP/JSON reuses the existing internal endpoint pattern (Phase 2 of the locking-fix plan); gRPC is more efficient at high dispatch rates. Defer to first-design measurement.
- Should worker nodes hold a local read replica of their owned-shard `task_runs`? Would speed up UI reads if a UI server happens to colocate, but adds complexity. Defer until measurement shows UI read latency is a real problem.
- How does `CAESIUM_RUN_OWNER_MAX_RUNS` interact with autoscaling? If a worker pod is autoscaled away, its owned runs need clean handoff (don't wait for lease expiry). Probably warrants a graceful-shutdown drain step.

## References

- [design-database-locking-fix.md](design-database-locking-fix.md) — the locking-fix plan whose Phase 4 this design layers on.
- [design-parallel-job-execution.md](design-parallel-job-execution.md) — the existing distributed-mode execution model.
- [parallel-execution-operations.md](parallel-execution-operations.md) — operator-facing config and rollout reference.
- [database-sharding.md](database-sharding.md) — table placement and routing contract introduced by PR #157.
- PR [#157](https://github.com/caesium-cloud/caesium/pull/157) — Phase 4 database shard router.
- Temporal "history shards" — single-owner-per-workflow pattern that this design adapts to Caesium's run-grain.
