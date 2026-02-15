# Design: Parallel Job Execution

## Status

In Progress

## Progress Update (2026-02-14)

Implemented on branch `codex/parallel-exec-phase1a`:

- Phase 1.1 startup execution config logging (`max_parallel_tasks`).
- Phase 1.2 worker pool (`internal/worker/pool.go`) with unit tests.
- Phase 1.3 failure policy (`halt`/`continue`) with dependent-task skip behavior.
- Phase 1.4 task timeout (`CAESIUM_TASK_TIMEOUT`) with forced stop + persisted task failure.
- TUI support for skipped task state in DAG and detail views.

Implemented on branch `codex/parallel-exec-phase2a`:

- Phase 2.1/2.2 claim metadata added to `task_runs` (`claimed_by`, `claim_expires_at`, `claim_attempt`) via model/automigration updates.
- Phase 2.3 initial `internal/worker/claimer.go` implementation with atomic claim semantics.
- Phase 2.4 `internal/worker/worker.go` polling loop abstraction with bounded pool execution.
- Phase 2.5 worker/execution env variables added to `pkg/env/env.go`.
- Phase 2.6 worker loop startup wired in `cmd/start/start.go` for distributed mode.
- Phase 2.7 `internal/job/job.go` now gates local execution vs distributed enqueue+wait path via `CAESIUM_EXECUTION_MODE`.
- Phase 2.7a attempt-suffixed runtime names added in distributed worker execution.
- Phase 2.8 initial lease renewal + expired-lease reclaim implemented in worker executor/claimer flow.
- Phase 2.9 added distributed multi-worker simulation coverage (`internal/worker/distributed_execution_test.go`).
- Phase 2.10 run-store task lifecycle now has claim-aware methods to enforce `claimed_by` ownership during status transitions.
- Added claimer unit tests for ready-task selection, lease-expiry reclaim, and no-work behavior.

Implemented on branch `codex/parallel-exec-phase3a`:

- Phase 3.1 worker metrics added:
  - `caesium_worker_claims_total{node_id}` for successful claims per node.
  - `caesium_worker_claim_contention_total{node_id}` for claim contention events.
  - `caesium_worker_lease_expirations_total{node_id}` for reclaimed expired leases.
- Claimer instrumentation now records claim success/contention and expired lease reclaim counts.
- Metrics and claimer tests expanded to cover the new counters.

Implemented on branch `codex/parallel-exec-phase3b`:

- Phase 3.2 worker status API:
  - Added `GET /v1/nodes/:address/workers`.
  - Added `api/rest/service/worker` status aggregation over claimed tasks by node.
  - Worker status response now includes claimed-task totals by status, running claims, expired leases, total claim attempts, last activity timestamp, and active running claim details.
  - Added service-level tests for empty and populated worker status views.

Not yet complete:

- Full Phase 1.6 coverage for end-to-end parallel execution and timeout behavior in `internal/job/job_test.go`.
- Phase 2 distributed run lifecycle polish and production hardening remain pending.

## Problem

Currently, caesium executes jobs only on the Raft leader node. While within a single job run the DAG scheduler in `internal/job/job.go` already supports concurrent task dispatch (controlled by `CAESIUM_MAX_PARALLEL_TASKS`, default 1), there are two key limitations:

1. **Single-node execution** — All tasks run on whichever node is the dqlite leader. Follower nodes sit idle.
2. **Serial default** — `MaxParallelTasks` defaults to 1, so most deployments run tasks one at a time within a job.

We want to enable true parallel execution — both within a single node (multiple goroutines) and, ideally, across multiple cluster nodes.

## Current Architecture Summary

| Component | File | Role |
|---|---|---|
| Job runner | `internal/job/job.go` | Builds DAG, dispatches tasks via goroutines, waits on channel |
| Run store | `internal/run/store.go` | Persists job/task run state in dqlite/postgres |
| Executor | `internal/executor/executor.go` | Polls for cron triggers, fires jobs on leader |
| Trigger (cron) | `internal/trigger/cron/cron.go` | Listens for schedule, calls `job.New(j).Run(ctx)` |
| Trigger (HTTP) | `internal/trigger/http/http.go` | Fires jobs on HTTP PUT |
| Env config | `pkg/env/env.go` | `MaxParallelTasks` (default 1) |
| Startup | `cmd/start/start.go` | Launches API + executor on every node |

Key observations:
- The DAG scheduler (`job.go:226-406`) already has a `maxParallel` semaphore pattern with goroutine dispatch. Bumping `CAESIUM_MAX_PARALLEL_TASKS` > 1 gives intra-job parallelism on one node today.
- Task state is persisted in the shared database (`OutstandingPredecessors` tracking, status transitions).
- `store.CompleteTask` already decrements successor predecessor counts in a DB transaction.
- The executor loop and triggers only run meaningful work on the leader (dqlite writes require leader).

## Design

### Phase 1: Reliable Single-Node Parallelism (low risk, high value)

The goroutine-based parallel dispatch in `job.go` already works. Phase 1 makes it production-ready.

#### 1.1 — Raise and validate `MaxParallelTasks` default

**File:** `pkg/env/env.go`

- Change `MaxParallelTasks` default from `1` to a sensible value (e.g. `4`), or leave at 1 but document the knob prominently.
- Add `CAESIUM_MAX_PARALLEL_TASKS` to the startup log output so operators can confirm the setting.

**File:** `cmd/start/start.go`

- Log `MaxParallelTasks` at startup alongside other config.

#### 1.2 — Add a worker pool instead of unbounded goroutines

**New file:** `internal/worker/pool.go`

Currently each dispatched task spawns a raw goroutine (`job.go:351`). Replace with a bounded worker pool:

```go
type Pool struct {
    sem  chan struct{}
    wg   sync.WaitGroup
}

func NewPool(size int) *Pool {
    return &Pool{sem: make(chan struct{}, size)}
}

func (p *Pool) Submit(ctx context.Context, fn func()) error {
    select {
    case p.sem <- struct{}{}:
        p.wg.Add(1)
        go func() {
            defer func() { <-p.sem; p.wg.Done() }()
            fn()
        }()
        return nil
    case <-ctx.Done():
        return ctx.Err()
    }
}

func (p *Pool) Wait() { p.wg.Wait() }
```

**File:** `internal/job/job.go`

- Create a `worker.Pool` at the start of `Run()` sized to `maxParallel`.
- Replace the manual `active` counter + raw goroutine dispatch with `pool.Submit()`.
- This is a contained refactor — the channel-based result collection stays the same.

#### 1.3 — Fail-fast vs. fail-continue policy

**File:** `pkg/env/env.go`

- Add `TaskFailurePolicy` env var: `"halt"` (default, current behavior) or `"continue"`.

**File:** `internal/job/job.go`

- When policy is `"continue"`, don't set `halt = true` on task failure. Instead, mark failed task's dependents as skipped and continue executing independent branches.
- Track skipped tasks so the final run status reflects partial failure.

#### 1.4 — Task-level timeout

**File:** `pkg/env/env.go`

- Add `TaskTimeout` env var (duration, default `0` = no timeout).

**File:** `internal/job/job.go`

- Wrap each `runTask()` call with a `context.WithTimeout` derived from the job context.
- On timeout, call `engine.Stop()` and `store.FailTask()`.

### Phase 2: Multi-Node Distributed Execution

This phase allows follower nodes to pick up and execute tasks, while the leader remains the coordinator.

#### 2.1 — Task Claiming Protocol

The core idea: instead of the leader executing all tasks locally, the leader (or any node) **enqueues** ready tasks in the database, and **any node** can claim and execute them.

**New DB column on `task_runs` table:**

```sql
ALTER TABLE task_runs ADD COLUMN claimed_by TEXT DEFAULT '';
ALTER TABLE task_runs ADD COLUMN claim_expires_at TIMESTAMP;
ALTER TABLE task_runs ADD COLUMN claim_attempt INTEGER DEFAULT 0;
```

- `claimed_by` — node address (`CAESIUM_NODE_ADDRESS`) of the node executing this task, empty if unclaimed.
- `claim_expires_at` — lease expiry to handle node failures.
- `claim_attempt` — monotonically increasing attempt counter, incremented on each claim. Used to generate unique container names (`taskID-attemptN`) so reclaimed tasks don't collide with containers from a previous (possibly still-running) attempt.

**New file:** `internal/worker/claimer.go`

```go
type Claimer struct {
    nodeID    string
    store     *run.Store
    pool      *Pool
    leaseTTL  time.Duration
}

// ClaimNext atomically claims an unclaimed, ready (predecessors=0, status=pending) task.
// Uses a two-step SELECT-then-UPDATE pattern because SQLite/dqlite does not support
// LIMIT on UPDATE statements.
func (c *Claimer) ClaimNext(ctx context.Context) (*models.TaskRun, error) {
    // Step 1: SELECT a single candidate row ID.
    //   SELECT id FROM task_runs
    //   WHERE status = 'pending' AND outstanding_predecessors = 0
    //     AND (claimed_by = '' OR claim_expires_at < NOW())
    //   ORDER BY created_at ASC
    //   LIMIT 1
    //
    // Step 2: Atomically UPDATE only that row, re-checking conditions to
    // guard against races (another node may have claimed it between steps).
    //   UPDATE task_runs
    //   SET claimed_by = ?, claim_expires_at = ?, status = 'running'
    //   WHERE id = ? AND status = 'pending'
    //     AND (claimed_by = '' OR claim_expires_at < NOW())
    //   RETURNING *
    //
    // If the UPDATE affects 0 rows, another node won the race — retry or back off.
}
```

The claim uses an atomic UPDATE with WHERE conditions so only one node wins the race. Dqlite serializes writes through the leader, so this is safe even though multiple nodes attempt claims.

**Important:** All DB writes in dqlite go through the Raft leader regardless. So follower nodes making claim UPDATEs will be forwarded to the leader via the dqlite protocol. This is already how dqlite works — no new Raft changes needed.

#### 2.2 — Worker Loop on Every Node

**New file:** `internal/worker/worker.go`

Each node runs a worker loop:

```go
func (w *Worker) Run(ctx context.Context) error {
    for {
        select {
        case <-ctx.Done():
            return nil
        default:
            task, err := w.claimer.ClaimNext(ctx)
            if err != nil || task == nil {
                // No work available, back off
                time.Sleep(w.pollInterval)
                continue
            }
            w.pool.Submit(ctx, func() {
                w.executeTask(ctx, task)
            })
        }
    }
}
```

**File:** `cmd/start/start.go`

- Launch the worker loop on every node (alongside the API server and executor).
- The executor/trigger still runs on the leader and is responsible for **creating job runs and registering tasks** (building the DAG, writing task_runs with correct predecessor counts).
- Workers on all nodes poll for claimable tasks.

#### 2.3 — Leader Coordination Changes

**File:** `internal/job/job.go`

The `Run()` method changes from "build DAG and execute locally" to "build DAG and enqueue, then wait":

1. Build the DAG and register tasks (unchanged — lines 96-218).
2. Instead of the local dispatch loop (lines 226-406), transition to a **wait loop** that polls the DB for run completion:

```go
// After registering tasks, just wait for all tasks to reach terminal state.
func (j *job) waitForCompletion(ctx context.Context, runID uuid.UUID, taskCount int) error {
    store := run.Default()
    ticker := time.NewTicker(5 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-ticker.C:
            snapshot, err := store.Get(runID)
            if err != nil {
                return err
            }
            completed, failed := 0, 0
            for _, t := range snapshot.Tasks {
                switch t.Status {
                case run.TaskStatusSucceeded:
                    completed++
                case run.TaskStatusFailed:
                    failed++
                }
            }
            if failed > 0 {
                return fmt.Errorf("task(s) failed in run %s", runID)
            }
            if completed == taskCount {
                return nil
            }
        }
    }
}
```

Workers handle the actual execution. The leader's `Run()` just orchestrates setup and waits.

#### 2.4 — Successor Unblocking (Distributed)

Currently `store.CompleteTask()` (`run/store.go:175-248`) decrements `outstanding_predecessors` for successor tasks in a DB transaction. This already works for distributed execution — any node completing a task will update the DB, and other nodes polling for claimable tasks (predecessors=0, status=pending) will see the newly unblocked tasks.

No changes needed here — the existing transaction-based approach is correct for multi-node.

#### 2.5 — Lease Renewal and Dead Node Recovery

**File:** `internal/worker/claimer.go`

- Each worker periodically renews its lease on active tasks (extend `claim_expires_at`).
- A background goroutine on every node checks for expired leases and resets those tasks to `pending` + `claimed_by = ''`.

```go
func (c *Claimer) ReclaimExpired(ctx context.Context) error {
    return c.store.DB().Model(&models.TaskRun{}).
        Where("status = ? AND claim_expires_at < ?", "running", time.Now().UTC()).
        Updates(map[string]interface{}{
            "status":     "pending",
            "claimed_by": "",
            "runtime_id": "",
            "started_at": nil,
        }).Error
}
```

#### 2.6 — Configuration

**File:** `pkg/env/env.go`

New environment variables:

| Variable | Default | Description |
|---|---|---|
| `CAESIUM_WORKER_ENABLED` | `true` | Enable the distributed worker on this node |
| `CAESIUM_WORKER_POLL_INTERVAL` | `2s` | How often to poll for claimable tasks |
| `CAESIUM_WORKER_LEASE_TTL` | `5m` | Lease duration for claimed tasks |
| `CAESIUM_WORKER_POOL_SIZE` | `4` | Max concurrent tasks per node |

## Implementation Plan

### Phase 1 — Single-Node Parallelism

- [x] **1.1** Log `MaxParallelTasks` at startup (`cmd/start/start.go`)
- [x] **1.2** Create `internal/worker/pool.go` — bounded worker pool
- [x] **1.3** Refactor `internal/job/job.go` dispatch loop to use worker pool
- [x] **1.4** Add `TaskFailurePolicy` env var and implement `"continue"` mode in job.go
- [x] **1.5** Add `TaskTimeout` env var and implement per-task timeouts in job.go
- [ ] **1.6** Add tests for parallel task execution, failure policies, and timeouts
- [x] **1.7** Update default `MaxParallelTasks` to 4 (or document the knob in startup logs)

### Phase 2 — Multi-Node Distributed Execution

- [x] **2.1** Add `claimed_by`, `claim_expires_at`, and `claim_attempt` columns to `task_runs` (DB migration)
- [x] **2.2** Add `TaskRun` model fields for `ClaimedBy`, `ClaimExpiresAt`, and `ClaimAttempt` (`internal/models/`)
- [x] **2.3** Create `internal/worker/claimer.go` — atomic task claiming with lease
- [x] **2.4** Create `internal/worker/worker.go` — per-node worker loop
- [x] **2.5** Add worker env vars to `pkg/env/env.go` (including `CAESIUM_EXECUTION_MODE`: `local`/`distributed`)
- [x] **2.6** Launch worker loop in `cmd/start/start.go` on every node
- [x] **2.7** Refactor `internal/job/job.go` `Run()` — split into "enqueue" (register tasks) and "wait for completion"; gate on `CAESIUM_EXECUTION_MODE`
- [x] **2.7a** Use attempt-suffixed container names (`taskID-attemptN`) in task execution to prevent name collisions on reclaim
- [x] **2.8** Implement lease renewal and expired-lease recovery in claimer
- [x] **2.9** Add distributed execution tests (multi-node simulation)
- [x] **2.10** Update run store methods to be claim-aware (respect `claimed_by` in status queries)

### Phase 3 — Observability and Polish

- [x] **3.1** Add metrics: tasks claimed per node, claim contention, lease expirations
- [x] **3.2** Expose worker status in the API (`GET /v1/nodes/:address/workers`)
- [ ] **3.3** Add node affinity labels (optional: prefer certain tasks on certain nodes)
- [ ] **3.4** Update the console TUI DAG view to show which node is executing each task
- [ ] **3.5** Document configuration and operational guidance

## Risks and Mitigations

| Risk | Mitigation |
|---|---|
| Claim contention under high load | Jittered poll intervals; dqlite serializes writes so correctness is guaranteed |
| Expired leases causing duplicate execution | Container names use task ID today (`Name: taskID.String()`), so a reclaimed task collides with a still-running container from the original worker. To fix: include an **attempt counter** (`claim_attempt` column, incremented on each claim) in the container name (`taskID-attemptN`). On reclaim, the recovery goroutine first calls `engine.Stop()` on the old container name before resetting the task to pending. Workers always create containers with the current attempt suffix, avoiding name collisions. |
| Dqlite write forwarding latency | Workers poll on interval; 2s default is generous. Monitor and tune. |
| Breaking change for existing single-node users | Phase 1 is backward compatible. Phase 2 defaults to `EXECUTION_MODE=local` (current behavior); operators opt in to distributed mode after all nodes are upgraded. |
| Container engine availability varies per node | Workers only claim tasks for engines they can reach (future: engine capability advertisement) |

## Migration Path

- **Phase 1** is a drop-in improvement. Operators just set `CAESIUM_MAX_PARALLEL_TASKS` > 1.
- **Phase 2** requires a DB migration (new columns) and a **coordinated cutover**, not a rolling upgrade:
  1. Add a `CAESIUM_EXECUTION_MODE` env var with values `"local"` (default, current behavior) and `"distributed"`.
  2. Deploy new binary to all nodes with `EXECUTION_MODE=local`. The DB migration runs (adds columns), but all nodes continue using the existing local dispatch loop. No behavioral change.
  3. Once all nodes are running the new binary, switch to `EXECUTION_MODE=distributed` (via config change + rolling restart, or a future runtime toggle API). This activates the worker claim loop and disables the local dispatch loop.
  4. **Why not rolling upgrade?** During a mixed-version window, old nodes still run the local dispatch loop while new nodes claim from the shared queue. A pending task could be executed by both the old node's local dispatcher and a new node's claim worker simultaneously. The feature flag ensures all nodes agree on the execution model before distributed claims are enabled.
- No breaking API changes in either phase.
