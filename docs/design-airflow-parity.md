# Design: Airflow Functional Parity

## Status

Draft — Ready for implementation

## Overview

This plan closes the feature gaps between Caesium and Apache Airflow while preserving Caesium's container-first identity. Each workstream is independent and can be implemented in parallel by separate agents. Workstreams are ordered by priority (P0 → P2).

---

## Workstream 1: Task Retries (P0)

**Why**: Table stakes for any production scheduler. Transient failures (OOM, network blip, image pull timeout) should not require manual intervention.

### Data Model

Add fields to `Task` model (`internal/models/task.go`):

```go
Retries           int           `gorm:"not null;default:0" json:"retries"`
RetryDelay        time.Duration `gorm:"not null;default:0" json:"retry_delay"`
RetryBackoff      bool          `gorm:"not null;default:false" json:"retry_backoff"`
```

Add fields to `TaskRun` model (`internal/models/run.go`):

```go
Attempt     int `gorm:"not null;default:1" json:"attempt"`
MaxAttempts int `gorm:"not null;default:1" json:"max_attempts"`
```

### Job Definition Schema

Add to `Step` struct (`pkg/jobdef/definition.go`):

```yaml
steps:
  - name: etl-extract
    image: etl:latest
    retries: 3
    retryDelay: 30s
    retryBackoff: true    # doubles delay each attempt
```

### Implementation Steps

1. **Schema**: Add `Retries`, `RetryDelay`, `RetryBackoff` to `Step` in `pkg/jobdef/definition.go`. Add `Attempt`, `MaxAttempts` to `TaskRun` in `internal/models/run.go`. GORM automigrate handles the column additions.
2. **Importer**: In `internal/jobdef/importer.go`, propagate the new step fields into the `Task` model when importing a job definition.
3. **Executor retry loop**: In `internal/job/job.go`, when a task fails and `task.Retries > 0 && taskRun.Attempt < taskRun.MaxAttempts`:
   - Compute delay: `retryDelay * 2^(attempt-1)` if backoff, else `retryDelay`.
   - Sleep for delay.
   - Create a new container execution (same atom, incremented attempt).
   - Update `TaskRun.Attempt`.
   - Only mark the task as permanently `failed` when all attempts are exhausted.
4. **Distributed mode**: In `internal/worker/worker.go`, the retry loop runs within the worker that claimed the task. If the worker dies mid-retry, lease expiry allows another worker to reclaim — the new worker reads `Attempt` from the DB and continues from the correct attempt number.
5. **Metrics**: Add `caesium_task_retries_total` counter to `internal/metrics/metrics.go`, labeled by `{job_alias, task_name, attempt}`.
6. **Events**: Emit a `TaskRetrying` event type from the event bus.
7. **UI/Console**: Show attempt number in task run detail view. Show retry count in run summary.
8. **Tests**: Unit tests for retry loop logic, backoff calculation, max-attempt exhaustion. Integration test with a container that fails N-1 times then succeeds.
9. **Docs**: Update `docs/job-schema-reference.md` with the new step fields.

### Files to Touch

- `pkg/jobdef/definition.go` — Step struct + validation
- `internal/models/task.go` — Task model
- `internal/models/run.go` — TaskRun model
- `internal/jobdef/importer.go` — propagate fields
- `internal/job/job.go` — retry loop in executor
- `internal/worker/worker.go` — distributed retry
- `internal/metrics/metrics.go` — retry counter
- `internal/event/bus.go` — TaskRetrying event type
- `ui/src/` — attempt display
- `docs/job-schema-reference.md`

---

## Workstream 2: Trigger Rules (P0)

**Why**: Unlocks error-handling DAG patterns — cleanup-on-failure tasks, always-run notification steps, conditional joins. Without this, DAGs can only express the happy path.

### Supported Rules

| Rule | Behavior |
|------|----------|
| `all_success` | Run when all predecessors succeeded (default, current behavior) |
| `all_done` | Run when all predecessors finished, regardless of status |
| `all_failed` | Run only when all predecessors failed |
| `one_success` | Run when at least one predecessor succeeded |
| `always` | Alias for `all_done` (provided for Airflow familiarity) |

### Data Model

Add to `Task` model (`internal/models/task.go`):

```go
TriggerRule string `gorm:"type:text;not null;default:'all_success'" json:"trigger_rule"`
```

### Job Definition Schema

```yaml
steps:
  - name: cleanup
    image: cleanup:latest
    dependsOn: [etl-load]
    triggerRule: all_done
```

### Implementation Steps

1. **Schema**: Add `TriggerRule` to `Step` in `pkg/jobdef/definition.go` with validation against the allowed values. Add `TriggerRule` to `Task` model.
2. **Importer**: Propagate `triggerRule` from step definition to Task model. Default to `all_success` when omitted.
3. **Executor logic**: In `internal/job/job.go`, modify the task-readiness check. Currently a task is ready when `outstanding_predecessors == 0`. Change to:
   - Decrement `outstanding_predecessors` as predecessors complete (any terminal status).
   - When `outstanding_predecessors == 0`, evaluate the trigger rule against the actual statuses of all predecessors.
   - If the rule is satisfied → run the task.
   - If the rule is not satisfied → mark the task `skipped`.
   - `always` rule: task is ready immediately when `outstanding_predecessors == 0`, regardless of statuses.
4. **Failure policy interaction**: The `continue` failure policy already skips descendants on failure. Trigger rules should be evaluated *before* the skip-descendants logic — a task with `all_done` or `all_failed` should not be skipped by the continue policy.
5. **Run store**: Add a method to `internal/run/store.go` to query predecessor statuses for a given task run.
6. **Tests**: Unit tests for each trigger rule. Integration tests for a DAG with mixed rules (happy path + failure path + cleanup).
7. **Docs**: Update `docs/job-schema-reference.md`.

### Files to Touch

- `pkg/jobdef/definition.go` — Step struct + validation
- `internal/models/task.go` — Task model
- `internal/jobdef/importer.go` — propagate field
- `internal/job/job.go` — readiness evaluation
- `internal/job/failure_policy.go` — interaction with trigger rules
- `internal/run/store.go` — predecessor status query
- `docs/job-schema-reference.md`

---

## Workstream 3: Run Parameters (P0)

**Why**: Enables parameterized triggers — pass a date, environment name, feature flag, or any config to a run. Without this, every variation requires a separate job definition.

### Data Model

Add to `JobRun` model (`internal/models/run.go`):

```go
Params datatypes.JSON `gorm:"type:json" json:"params,omitempty"`
```

### API

Modify `POST /v1/jobs/{id}/run` to accept an optional JSON body:

```json
{ "params": { "date": "2026-03-10", "env": "staging" } }
```

### Implementation Steps

1. **Model**: Add `Params` field to `JobRun`.
2. **API handler**: In `api/rest/controller/job/post.go`, parse optional `params` from the request body. Store on the `JobRun`.
3. **Env injection**: In `internal/job/job.go`, when building the container env map for a task, merge `JobRun.Params` as `CAESIUM_PARAM_<KEY>=<VALUE>` (upper-cased, prefixed). Also inject `CAESIUM_RUN_ID` and `CAESIUM_JOB_ALIAS`.
4. **Cron trigger params**: Allow `defaultParams` in the cron trigger configuration. These are used when a cron trigger fires, but can be overridden by HTTP trigger params.
5. **GraphQL**: Expose `params` on the `JobRun` type in `api/gql/schema/schema.go`.
6. **Callback metadata**: Include `params` in the callback `Metadata` struct so downstream webhooks receive them.
7. **UI**: Show params in the run detail view. Add a "Trigger with params" form to the job detail page.
8. **Console**: Display params in the run detail panel.
9. **Tests**: Unit tests for param merging, env injection, and cron default params.
10. **Docs**: Update `docs/job-schema-reference.md` and `docs/job-definitions.md`.

### Files to Touch

- `internal/models/run.go` — JobRun model
- `api/rest/controller/job/post.go` — HTTP trigger handler
- `internal/job/job.go` — env injection
- `internal/trigger/` — default params for cron
- `api/gql/schema/schema.go` — GraphQL
- `internal/callback/callback.go` — Metadata struct
- `ui/src/` — trigger form + run detail
- `docs/job-schema-reference.md`

---

## Workstream 4: Backfill & Catchup (P1)

**Why**: Critical for data pipeline use cases. When a pipeline is deployed late or a historical reprocess is needed, operators must be able to fill in missing runs for a date range.

### API

New endpoint:

```
POST /v1/jobs/{id}/backfill
{
  "start": "2026-03-01T00:00:00Z",
  "end": "2026-03-10T00:00:00Z",
  "max_concurrent": 2,
  "reprocess": "failed"  // "none" | "failed" | "all"
}
```

Returns a backfill ID and the list of logical dates that will be processed.

### Implementation Steps

1. **Backfill model**: Create `internal/models/backfill.go` with a `Backfill` struct (ID, JobID, Start, End, MaxConcurrent, Reprocess, Status, CreatedAt). Add to `models.All`.
2. **Logical date generation**: Given a job's cron schedule, enumerate all fire times between `start` and `end`. Filter based on `reprocess` policy (skip dates with existing successful runs if `reprocess=none`; re-run only failed if `reprocess=failed`).
3. **Backfill executor**: In `internal/job/backfill.go`, create a goroutine that processes the logical date queue with a concurrency semaphore (`max_concurrent`). For each date, create a `JobRun` with `Params: {"logical_date": "<ISO8601>"}` and execute it through the existing job executor.
4. **API handler**: In `api/rest/controller/job/`, add `PostBackfill` and `GetBackfill` handlers. Wire in `api/rest/bind/bind.go`.
5. **Catchup flag**: Add `catchup: bool` to cron trigger configuration in `pkg/jobdef/definition.go`. When `true`, the trigger executor (`internal/trigger/`) checks on startup for missed fire times since the last recorded run and queues them automatically.
6. **Status tracking**: The backfill model tracks overall progress (total/completed/failed counts). Expose via API.
7. **CLI**: Add `caesium backfill create --job <alias> --start <date> --end <date>` to `cmd/`.
8. **Tests**: Unit tests for logical date enumeration, reprocess filtering, concurrency limiting.
9. **Docs**: New `docs/backfill.md` explaining backfill and catchup behavior.

### Files to Touch

- `internal/models/backfill.go` — new model
- `internal/models/models.go` — register model
- `internal/job/backfill.go` — new backfill executor
- `internal/trigger/` — catchup logic
- `pkg/jobdef/definition.go` — catchup flag
- `api/rest/controller/job/` — backfill handlers
- `api/rest/bind/bind.go` — route registration
- `cmd/` — CLI command
- `docs/backfill.md`

---

## Workstream 5: Sensors (P1)

**Why**: Waiting for external conditions (file arrives, API returns 200, upstream job completes) is a core orchestration primitive. Without sensors, users must build polling into their container images, duplicating logic across every pipeline.

### Concept

A sensor is a step with `type: sensor` that repeatedly runs a container until it exits with code 0 (condition met). Non-zero exits are retried at `pokeInterval` until `timeout` is reached.

### Job Definition Schema

```yaml
steps:
  - name: wait-for-data
    type: sensor
    image: check-s3:latest
    command: ["python", "check.py", "--bucket", "raw-data"]
    pokeInterval: 60s
    timeout: 1h
    softFail: false    # true = skip downstream instead of failing
```

### Implementation Steps

1. **Schema**: Add `Type` (default `task`, also `sensor`), `PokeInterval`, `Timeout`, `SoftFail` to `Step` in `pkg/jobdef/definition.go`.
2. **Model**: Add corresponding fields to `Task` model. Add `task` and `sensor` as `TaskType` constants.
3. **Importer**: Propagate sensor fields from step definition to Task model.
4. **Sensor executor**: In `internal/job/sensor.go`, implement the sensor execution loop:
   - Run the container.
   - If exit code 0 → sensor satisfied, mark `succeeded`.
   - If exit code non-zero → sleep `pokeInterval`, re-run.
   - If total elapsed > `timeout` → mark `failed` (or `skipped` if `softFail`).
   - Emit `SensorPoke` events for observability.
5. **Integration with job executor**: In `internal/job/job.go`, dispatch to sensor executor when `task.Type == "sensor"`.
6. **Distributed mode**: Sensor tasks are claimed like regular tasks. The claiming worker runs the poke loop. Lease renewal must account for long-running sensors.
7. **Metrics**: Add `caesium_sensor_pokes_total` and `caesium_sensor_timeout_total` counters.
8. **Built-in sensor images**: Provide example Dockerfiles for common sensors (HTTP endpoint, file existence, job completion) in `docs/examples/sensors/`.
9. **Cross-job sensor**: Add an `ExternalJobSensor` that polls `GET /v1/jobs/{alias}/runs?status=succeeded&limit=1` to wait for another Caesium job to complete.
10. **Tests**: Unit tests for poke loop, timeout, soft fail. Integration test with a sensor that succeeds after 3 pokes.
11. **Docs**: Update `docs/job-schema-reference.md`. Add `docs/sensors.md`.

### Files to Touch

- `pkg/jobdef/definition.go` — Step struct
- `internal/models/task.go` — Task model
- `internal/jobdef/importer.go` — propagate fields
- `internal/job/sensor.go` — new file
- `internal/job/job.go` — dispatch to sensor executor
- `internal/worker/worker.go` — lease renewal for sensors
- `internal/metrics/metrics.go` — sensor metrics
- `internal/event/bus.go` — SensorPoke event type
- `docs/examples/sensors/` — example Dockerfiles
- `docs/sensors.md`

---

## Workstream 6: Branching & Conditional Execution (P1)

**Why**: Enables dynamic DAGs where the execution path depends on runtime conditions — run different pipelines for weekdays vs weekends, skip expensive steps when data hasn't changed, fan into error-specific handlers.

### Concept

A branch step runs a container whose stdout (or a designated output file) emits the name(s) of the downstream step(s) to execute. All other downstream steps are marked `skipped`.

### Job Definition Schema

```yaml
steps:
  - name: check-data-freshness
    type: branch
    image: branch-check:latest
    next: [full-refresh, incremental-update, skip-entirely]

  - name: full-refresh
    image: etl:latest
    command: ["full"]
    dependsOn: [check-data-freshness]

  - name: incremental-update
    image: etl:latest
    command: ["incremental"]
    dependsOn: [check-data-freshness]

  - name: skip-entirely
    image: alpine:latest
    command: ["echo", "no-op"]
    dependsOn: [check-data-freshness]
```

The container for `check-data-freshness` prints `full-refresh` to stdout → only that branch runs.

### Implementation Steps

1. **Schema**: Add `branch` as a `Type` option for steps. Branch steps with `next` entries define the set of selectable paths; an empty `next` list is valid and enables short-circuit behavior (all downstream steps are skipped).
2. **Model**: Add `branch` to `TaskType` constants.
3. **Branch executor**: In `internal/job/branch.go`:
   - Run the container, capture stdout.
   - Parse stdout as a newline-separated list of step names.
   - Validate each name is a valid `next` target.
   - Mark non-selected downstream steps as `skipped` (BFS through their descendants).
   - Continue DAG execution with only the selected branches.
4. **Integration with job executor**: In `internal/job/job.go`, dispatch to branch executor when `task.Type == "branch"`.
5. **Short-circuit**: If a branch step outputs no step names (empty stdout), all downstream steps are skipped.
6. **Tests**: Unit tests for branch selection, skip propagation, invalid branch names. Integration test with a multi-branch DAG.
7. **Docs**: Update `docs/job-schema-reference.md`. Add branching examples to `docs/examples/`.

### Files to Touch

- `pkg/jobdef/definition.go` — Step type validation
- `internal/models/task.go` — TaskType constant
- `internal/job/branch.go` — new file
- `internal/job/job.go` — dispatch to branch executor
- `docs/job-schema-reference.md`
- `docs/examples/`

---

## Workstream 7: Dynamic Task Mapping (P2)

**Why**: Enables fan-out patterns where the number of parallel tasks isn't known until runtime — process each file in a directory, each partition in a table, each item in an API response.

### Concept

A `map` step runs a container whose output is a JSON array. The executor dynamically creates one task instance per array element from a template step, then waits for all instances before continuing the DAG.

### Job Definition Schema

```yaml
steps:
  - name: discover-partitions
    image: discover:latest
    type: map
    mapTemplate: process-partition

  - name: process-partition
    image: etl:latest
    command: ["process", "--partition"]
    # Receives CAESIUM_MAP_VALUE and CAESIUM_MAP_INDEX as env vars
```

### Implementation Steps

1. **Schema**: Add `map` as a `Type` option for steps. Add `MapTemplate` field (name of the template step to fan out). Validate that the template step exists and has no explicit `dependsOn`.
2. **Model**: Add `map` to `TaskType` constants. Add `MapParentRunID` to `TaskRun` to track which map step spawned the instance.
3. **Dynamic TaskRun creation**: In `internal/job/map.go`:
   - Run the map container, capture stdout as a JSON array.
   - For each element, create a `TaskRun` for the template step with `CAESIUM_MAP_VALUE=<element>` and `CAESIUM_MAP_INDEX=<i>` injected as env vars.
   - Track all spawned TaskRun IDs.
4. **Aggregation**: After all mapped TaskRuns complete, evaluate the trigger rule of downstream steps against the aggregate result (all succeeded? any failed?).
5. **Concurrency**: Mapped tasks respect the job's `maxParallelTasks` limit.
6. **UI**: Show mapped tasks as a collapsible group in the DAG visualization, with individual status per instance.
7. **Tests**: Unit tests for JSON parsing, fan-out creation, aggregation. Integration test mapping over a 3-element array.
8. **Docs**: Add `docs/dynamic-tasks.md`.

### Files to Touch

- `pkg/jobdef/definition.go` — Step struct + validation
- `internal/models/task.go` — TaskType constant
- `internal/models/run.go` — MapParentRunID
- `internal/job/map.go` — new file
- `internal/job/job.go` — dispatch to map executor
- `ui/src/` — grouped task display
- `docs/dynamic-tasks.md`

---

## Workstream 8: XCom / Inter-Task Data Passing (P1)

**Why**: Tasks in a pipeline need to pass small data (file paths, row counts, status flags) to downstream tasks without requiring an external system.

### Concept

Container-native approach: tasks write output to a convention path (`/caesium/output/`). The executor captures these files after the container exits and stores them. Downstream tasks receive predecessor outputs as env vars or mounted files.

### Implementation Steps

1. **Output capture**: After a task container exits successfully, check for files in `/caesium/output/`:
   - `/caesium/output/result.json` — parsed as key-value pairs.
   - Any file in `/caesium/output/` is stored.
2. **Storage model**: New `TaskOutput` model (`internal/models/task_output.go`):
   ```go
   type TaskOutput struct {
       ID        uuid.UUID `gorm:"type:uuid;primaryKey"`
       TaskRunID uuid.UUID `gorm:"type:uuid;index;not null"`
       Key       string    `gorm:"type:text;not null"`
       Value     string    `gorm:"type:text;not null"`
       CreatedAt time.Time `gorm:"not null"`
   }
   ```
3. **Env injection**: When a downstream task starts, inject predecessor outputs as `CAESIUM_OUTPUT_<STEP_NAME>_<KEY>=<VALUE>`.
4. **Size limit**: Cap individual values at 64KB. Larger data should use shared mounts or external storage — log a warning if exceeded.
5. **API**: Add `GET /v1/jobs/{id}/runs/{run_id}/tasks/{task_id}/outputs` endpoint.
6. **Mount option**: Optionally mount predecessor output files into the downstream container at `/caesium/input/<step_name>/`.
7. **Tests**: Unit tests for output capture, env injection, size limit enforcement.
8. **Docs**: Add `docs/data-passing.md`.

### Files to Touch

- `internal/models/task_output.go` — new model
- `internal/models/models.go` — register model
- `internal/job/job.go` — output capture + env injection
- `internal/atom/docker/engine.go` — mount `/caesium/output/`
- `internal/atom/kubernetes/engine.go` — mount output volume
- `internal/atom/podman/engine.go` — mount `/caesium/output/`
- `api/rest/controller/job/` — outputs endpoint
- `api/rest/bind/bind.go` — route registration
- `docs/data-passing.md`

---

## Workstream 9: Task Pools (P1)

**Why**: Prevents overwhelming shared resources. When 50 tasks all hit the same database, you need a way to limit concurrency across jobs — not just within a single job.

### Implementation Steps

1. **Pool model**: New `Pool` model (`internal/models/pool.go`):
   ```go
   type Pool struct {
       ID        uuid.UUID `gorm:"type:uuid;primaryKey"`
       Name      string    `gorm:"uniqueIndex;not null"`
       Slots     int       `gorm:"not null;default:16"`
       CreatedAt time.Time `gorm:"not null"`
       UpdatedAt time.Time `gorm:"not null"`
   }
   ```
2. **Step schema**: Add optional `pool` field to `Step` (string, pool name). Add optional `poolSlots` (int, default 1) for tasks that consume multiple slots.
3. **Task model**: Add `Pool` and `PoolSlots` fields to `Task`.
4. **Pool manager**: In `internal/pool/pool.go`, implement a pool manager that tracks active slot usage. Before a task is dispatched, check if the pool has available slots. If not, the task waits in a queue.
5. **API**: Add CRUD endpoints for pools (`/v1/pools`).
6. **Default pool**: A `default` pool is created on startup with a configurable slot count (`CAESIUM_DEFAULT_POOL_SLOTS`).
7. **Metrics**: Add `caesium_pool_used_slots` and `caesium_pool_queued_tasks` gauges.
8. **Tests**: Unit tests for slot acquisition/release, queue ordering, multi-slot tasks.
9. **Docs**: Add `docs/pools.md`.

### Files to Touch

- `internal/models/pool.go` — new model
- `internal/models/models.go` — register model
- `internal/pool/pool.go` — new pool manager
- `pkg/jobdef/definition.go` — Step struct
- `internal/models/task.go` — Task model
- `internal/jobdef/importer.go` — propagate pool
- `internal/job/job.go` — pool check before dispatch
- `api/rest/controller/pool/` — new CRUD handlers
- `api/rest/bind/bind.go` — route registration
- `internal/metrics/metrics.go` — pool gauges
- `docs/pools.md`

---

## Workstream 10: Pause/Unpause Jobs (P0)

**Why**: Operators need to temporarily suspend a job's schedule without deleting it — during maintenance windows, incident response, or when a downstream system is unavailable.

### Implementation Steps

1. **Model**: Add `Paused bool` to `Job` model (`internal/models/job.go`). Default `false`.
2. **Trigger executor**: In `internal/trigger/`, skip firing triggers for jobs where `Paused == true`.
3. **API**: Add `PUT /v1/jobs/{id}/pause` and `PUT /v1/jobs/{id}/unpause` endpoints. Return the updated job.
4. **UI/Console**: Add pause/unpause toggle to the job list and job detail views.
5. **Events**: Emit `JobPaused` and `JobUnpaused` events.
6. **HTTP trigger**: Return `409 Conflict` when attempting to trigger a paused job via HTTP.
7. **Tests**: Unit tests for pause/unpause, trigger skip behavior.
8. **Docs**: Update `docs/job-schema-reference.md`.

### Files to Touch

- `internal/models/job.go` — Paused field
- `internal/trigger/` — skip paused jobs
- `api/rest/controller/job/` — pause/unpause handlers
- `api/rest/bind/bind.go` — route registration
- `internal/event/bus.go` — event types
- `ui/src/` — toggle UI
- `docs/job-schema-reference.md`

---

## Workstream 11: SLA Tracking (P2)

**Why**: Operators need to know when a pipeline is running longer than expected *before* it times out. SLAs provide early warning without killing the task.

### Implementation Steps

1. **Schema**: Add `sla` duration to `Step` and `Metadata` (job-level default) in `pkg/jobdef/definition.go`.
2. **Model**: Add `SLA time.Duration` to `Task` model. Add `SLAMiss` model:
   ```go
   type SLAMiss struct {
       ID        uuid.UUID     `gorm:"type:uuid;primaryKey"`
       TaskRunID uuid.UUID     `gorm:"type:uuid;index;not null"`
       JobRunID  uuid.UUID     `gorm:"type:uuid;index;not null"`
       SLA       time.Duration `gorm:"not null"`
       Elapsed   time.Duration `gorm:"not null"`
       CreatedAt time.Time     `gorm:"not null"`
   }
   ```
3. **SLA checker**: In `internal/job/sla.go`, run a goroutine per job run that periodically checks running tasks against their SLA. When `elapsed > sla`:
   - Create an `SLAMiss` record.
   - Emit an `SLAMissed` event.
   - Fire SLA-specific callbacks (if configured).
   - Do NOT kill the task.
4. **Callback**: Add `sla_miss` callback type to the callback registry.
5. **API**: Add `GET /v1/sla-misses` endpoint with filtering by job/run/date range.
6. **Metrics**: Add `caesium_sla_misses_total` counter.
7. **Tests**: Unit tests for SLA checking, miss recording.
8. **Docs**: Update `docs/job-schema-reference.md`.

### Files to Touch

- `pkg/jobdef/definition.go` — Step + Metadata structs
- `internal/models/task.go` — SLA field
- `internal/models/sla_miss.go` — new model
- `internal/models/models.go` — register model
- `internal/job/sla.go` — new SLA checker
- `internal/callback/` — sla_miss callback type
- `api/rest/controller/` — SLA miss endpoint
- `api/rest/bind/bind.go` — route registration
- `internal/metrics/metrics.go` — SLA counter
- `internal/event/bus.go` — SLAMissed event type
- `docs/job-schema-reference.md`

---

## Workstream 12: DAG Run Timeout (P1)

**Why**: A job with many tasks needs an overall time cap to prevent runaway pipelines from consuming resources indefinitely.

### Implementation Steps

1. **Schema**: Add `runTimeout` to `Metadata` in `pkg/jobdef/definition.go`.
2. **Model**: Add `RunTimeout time.Duration` to `Job` model.
3. **Executor**: In `internal/job/job.go`, wrap the entire job execution in a `context.WithTimeout` using `RunTimeout`. When the context expires:
   - Cancel all running task contexts.
   - Mark remaining pending/running tasks as `failed` with error "run timeout exceeded".
   - Mark the JobRun as `failed`.
4. **Tests**: Unit test for run timeout cancellation.
5. **Docs**: Update `docs/job-schema-reference.md`.

### Files to Touch

- `pkg/jobdef/definition.go` — Metadata struct
- `internal/models/job.go` — RunTimeout field
- `internal/jobdef/importer.go` — propagate field
- `internal/job/job.go` — context timeout
- `docs/job-schema-reference.md`

---

## Workstream 13: Priority Weights (P2)

**Why**: When resources are constrained, high-priority tasks should run before low-priority ones.

### Implementation Steps

1. **Schema**: Add `priority` (int, default 0, higher = more important) to `Step` in `pkg/jobdef/definition.go`.
2. **Model**: Add `Priority int` to `Task` and `TaskRun` models.
3. **Executor**: In `internal/job/job.go`, when multiple tasks are ready simultaneously, sort by priority descending before dispatching.
4. **Distributed mode**: In `internal/worker/claimer.go`, order claimable tasks by priority descending.
5. **Pool integration**: When a pool queue has waiting tasks, dequeue highest priority first.
6. **Tests**: Unit test for priority ordering.
7. **Docs**: Update `docs/job-schema-reference.md`.

### Files to Touch

- `pkg/jobdef/definition.go` — Step struct
- `internal/models/task.go` — Priority field
- `internal/models/run.go` — Priority field on TaskRun
- `internal/jobdef/importer.go` — propagate field
- `internal/job/job.go` — priority sort
- `internal/worker/claimer.go` — priority ordering
- `docs/job-schema-reference.md`

---

## Workstream 14: Templating (P2)

**Why**: Enables runtime variable substitution in step env vars — pass the logical date, run ID, or job params into container configuration without hardcoding.

### Implementation Steps

1. **Template engine**: In `internal/job/template.go`, implement Go `text/template` rendering for step env values. Available variables:
   - `{{ .RunID }}` — JobRun UUID
   - `{{ .JobAlias }}` — job alias string
   - `{{ .LogicalDate }}` — ISO8601 timestamp (from params or trigger fire time)
   - `{{ .Params.<key> }}` — run parameter value
   - `{{ .Env.<key> }}` — server environment variable
2. **Rendering**: In `internal/job/job.go`, render all task env values through the template engine before passing to the container runtime. Template errors fail the task.
3. **Command templating**: Also render `command` array elements through the template engine.
4. **Validation**: In `pkg/jobdef/definition.go`, add a `ValidateTemplates` method that parses all env values and command elements as templates to catch syntax errors at definition time.
5. **Tests**: Unit tests for template rendering, missing variables, malformed templates.
6. **Docs**: Add `docs/templating.md` with available variables and examples.

### Files to Touch

- `internal/job/template.go` — new file
- `internal/job/job.go` — render before execution
- `pkg/jobdef/definition.go` — template validation
- `docs/templating.md`

---

## Workstream 15: Authentication & API Keys (P2)

**Why**: Caesium's API is currently fully open. Any production deployment needs at minimum API key authentication to prevent unauthorized access.

### Implementation Steps

1. **API key model**: New `APIKey` model (`internal/models/api_key.go`):
   ```go
   type APIKey struct {
       ID        uuid.UUID `gorm:"type:uuid;primaryKey"`
       Name      string    `gorm:"uniqueIndex;not null"`
       KeyHash   string    `gorm:"not null"`  // bcrypt hash
       Role      string    `gorm:"type:text;not null;default:'admin'"`
       CreatedAt time.Time `gorm:"not null"`
       ExpiresAt *time.Time
   }
   ```
2. **Roles**: `admin` (full access), `operator` (trigger runs, view everything), `viewer` (read-only).
3. **Middleware**: In `api/rest/middleware/auth.go`, implement an Echo middleware that:
   - Checks for `Authorization: Bearer <key>` header.
   - Hashes the key and looks up in the DB.
   - Rejects expired keys.
   - Sets the role on the request context.
   - Skips auth for `/health` and `/metrics` endpoints.
   - Auth is disabled when `CAESIUM_AUTH_ENABLED=false` (default, for backwards compatibility).
4. **Role enforcement**: In each handler, check the role from context against the required permission for the endpoint.
5. **CLI**: Add `caesium apikey create --name <name> --role <role>` and `caesium apikey list` commands.
6. **Initial key**: On first startup with auth enabled, generate and print an admin API key to stdout.
7. **Tests**: Unit tests for middleware, role enforcement, key expiry.
8. **Docs**: Add `docs/authentication.md`.

### Files to Touch

- `internal/models/api_key.go` — new model
- `internal/models/models.go` — register model
- `api/rest/middleware/auth.go` — new middleware
- `api/rest/bind/bind.go` — apply middleware
- `cmd/` — apikey commands
- `pkg/env/env.go` — CAESIUM_AUTH_ENABLED
- `docs/authentication.md`

---

## Dependency Graph

Workstreams are largely independent. The following dependencies exist:

```
Workstream 3 (Run Parameters) ← Workstream 4 (Backfill) uses params for logical_date
Workstream 3 (Run Parameters) ← Workstream 14 (Templating) uses params in templates
Workstream 2 (Trigger Rules)  ← Workstream 7 (Dynamic Mapping) uses rules for aggregation
Workstream 9 (Task Pools)     ← Workstream 13 (Priority) uses pools for ordering
```

All other workstreams can proceed in parallel from day one. Recommended execution order:

```
Phase 1 (parallel): WS1 (Retries), WS2 (Trigger Rules), WS3 (Run Parameters), WS10 (Pause/Unpause)
Phase 2 (parallel): WS4 (Backfill), WS5 (Sensors), WS6 (Branching), WS8 (XCom), WS9 (Pools), WS12 (Run Timeout)
Phase 3 (parallel): WS7 (Dynamic Mapping), WS11 (SLA), WS13 (Priority), WS14 (Templating), WS15 (Auth)
```
