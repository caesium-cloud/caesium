# Design: Airflow Functional Parity

## Status

Phase 1 complete. Six of fifteen workstreams are implemented (WS1, WS2, WS3, WS8, WS10, WS12).

## Overview

This plan closes the feature gaps between Caesium and Apache Airflow while preserving Caesium's container-first identity. Each workstream is independent and can be implemented in parallel by separate agents. Workstreams are ordered by priority (P0 → P2).

---

## Workstream 1: Task Retries (P0) — DONE

**Status**: Implemented.

**Why**: Table stakes for any production scheduler. Transient failures (OOM, network blip, image pull timeout) should not require manual intervention.

### Implementation

- `Retries`, `RetryDelay`, `RetryBackoff` on Step definition and Task model
- `Attempt`, `MaxAttempts` on TaskRun model
- Retry loop in `internal/job/job.go` with exponential backoff (`retryDelay * 2^(attempt-1)`)
- `computeRetryDelay()` in `internal/job/failure_policy.go`
- `caesium_task_retries_total` metric with labels `{job_alias, task_name, attempt}`
- `task_retrying` event type emitted via `store.RetryTask()`
- UI shows attempt number and retry configuration in TaskMetadataPanel

---

## Workstream 2: Trigger Rules (P0) — DONE

**Status**: Implemented.

**Why**: Unlocks error-handling DAG patterns — cleanup-on-failure tasks, always-run notification steps, conditional joins. Without this, DAGs can only express the happy path.

### Implementation

- `TriggerRule` field on Step definition (validated against `all_success`, `all_done`, `all_failed`, `one_success`, `always`) and Task model
- `satisfiesTriggerRule()` in `internal/job/failure_policy.go` evaluates all 5 rules against predecessor statuses
- `collectPredecessorStatuses()` gathers in-memory outcome state for evaluation
- `skipDescendantsFiltered()` walks the DAG to skip non-tolerant descendants when upstream fails
- `isTolerantRule()` identifies rules that handle failures (all_done, all_failed, always, one_success) to prevent incorrect skipping
- Integrated into the main execution loop in `internal/job/job.go`

---

## Workstream 3: Run Parameters (P0) — DONE

**Status**: Implemented.

**Why**: Enables parameterized triggers — pass a date, environment name, feature flag, or any config to a run. Without this, every variation requires a separate job definition.

### Implementation

- `Params` field (datatypes.JSON) on JobRun model
- `POST /v1/jobs/{id}/run` accepts optional `{"params": {...}}` body
- `buildParamEnv()` injects `CAESIUM_PARAM_<UPPERCASE_KEY>`, `CAESIUM_RUN_ID`, and `CAESIUM_JOB_ALIAS` into each task's container environment
- `WithParams()` exported option on job constructor
- `defaultParams` on Trigger definition, extracted and used by cron trigger's `Fire()` method
- UI shows run params in the run detail view and latest run panel

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

## Workstream 8: XCom / Inter-Task Data Passing (P1) — DONE

**Status**: Implemented in PR #107 (merged).

**Why**: Tasks in a pipeline need to pass small data (file paths, row counts, status flags) to downstream tasks without requiring an external system.

### Implementation (differs from original design)

Instead of the file-based `/caesium/output/` approach, a simpler stdout marker protocol was implemented:

- Tasks emit `##caesium::output {"key": "value"}` to stdout
- After container completion, logs are scanned for markers, parsed into `map[string]string`, and stored on the `TaskRun` model (`Output` field, `json` column)
- Downstream tasks receive predecessor outputs as `CAESIUM_OUTPUT_<STEP_NAME>_<KEY>=<VALUE>` environment variables
- Works in both local and distributed (worker) execution modes
- 64KB size limit per task output
- Last-write-wins merge semantics for multiple marker lines
- Non-string JSON values are coerced to strings; malformed JSON is silently skipped
- UI displays task outputs in the TaskMetadataPanel

### Files Changed

- `pkg/task/output.go` — `ParseOutput`, `NormalizeStepName`, `BuildOutputEnv`
- `pkg/task/output_test.go` — unit tests for parsing and env building
- `internal/models/run.go` — `Output` field on `TaskRun`
- `internal/run/store.go` — `CompleteTask` accepts output map
- `internal/run/store_test.go` — store-level tests
- `internal/job/job.go` — output capture + env injection (local mode)
- `internal/worker/runtime_executor.go` — output capture (distributed mode)
- `internal/jobdef/importer.go` — step name stored on Task model
- `ui/src/features/jobs/TaskMetadataPanel.tsx` — output display
- `ui/src/lib/api.ts` — `output` field on `TaskRun` interface

### Remaining items (not yet implemented)

- Dedicated API endpoint for task outputs (`GET /v1/jobs/{id}/runs/{run_id}/tasks/{task_id}/outputs`)
- Mount-based option for larger data passing (`/caesium/input/<step_name>/`)
- Documentation (`docs/data-passing.md`)

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

## Workstream 10: Pause/Unpause Jobs (P0) — DONE

**Status**: Implemented.

**Why**: Operators need to temporarily suspend a job's schedule without deleting it — during maintenance windows, incident response, or when a downstream system is unavailable.

### Implementation

- `Paused bool` field on Job model (default false)
- `PUT /v1/jobs/{id}/pause` and `PUT /v1/jobs/{id}/unpause` endpoints in `api/rest/controller/job/pause.go`
- Cron trigger's `Fire()` skips paused jobs with log message
- `POST /v1/jobs/{id}/run` returns 409 Conflict for paused jobs
- `job_paused` and `job_unpaused` event types emitted
- UI pause/unpause toggle on jobs list page with real-time SSE event updates

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

## Workstream 12: DAG Run Timeout (P1) — DONE

**Status**: Implemented in PR #109 (pending merge).

**Why**: A job with many tasks needs an overall time cap to prevent runaway pipelines from consuming resources indefinitely.

### Implementation

1. **Schema**: Added `RunTimeout time.Duration` to `Metadata` in `pkg/jobdef/definition.go` (`yaml:"runTimeout"`).
2. **Model**: Added `RunTimeout time.Duration` to `Job` model (auto-migrated by GORM).
3. **Importer**: Propagates `RunTimeout` from definition to model in `internal/jobdef/importer.go`.
4. **Executor**: In `internal/job/job.go`, the `Run()` method wraps the entire execution in `context.WithTimeout` when `runTimeout > 0`. When the deadline expires:
   - All in-flight task contexts are cancelled (derived from the parent context).
   - Running containers are force-stopped.
   - The error is wrapped as `"run timed out after <duration>"`.
   - The deferred `store.Complete()` marks the JobRun as `failed`.
5. **Run vs task timeout disambiguation**: When a task context is cancelled, the executor checks `ctx.Err()` to distinguish run-level timeouts from task-level timeouts, ensuring correct error messages.
6. **UI**: Job detail page shows `runTimeout`, `taskTimeout`, and `maxParallelTasks` in the Configuration tab's Job Metadata card.
7. **Tests**: Unit test (`TestRunLocalRunTimeoutFailsRun`) + 4 integration tests in `test/run_test.go`.
8. **Docs**: `docs/job-schema-reference.md` updated with `runTimeout`, `taskTimeout`, and `maxParallelTasks` fields.

### Files Changed

- `pkg/jobdef/definition.go` — `RunTimeout` on `Metadata` struct
- `internal/models/job.go` — `RunTimeout` field
- `internal/jobdef/importer.go` — propagate field
- `internal/job/job.go` — `context.WithTimeout` wrapping + run/task timeout disambiguation
- `internal/job/job_test.go` — unit test
- `test/run_test.go` — integration tests
- `ui/src/lib/api.ts` — timeout fields on `Job` interface
- `ui/src/features/jobs/JobDetailPage.tsx` — display timeout fields
- `docs/job-schema-reference.md` — document all metadata fields

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
Phase 1 (DONE):     WS1 (Retries), WS2 (Trigger Rules), WS3 (Run Parameters), WS10 (Pause/Unpause)
Phase 2 (partial):  WS4 (Backfill), WS5 (Sensors), WS6 (Branching), WS8 (XCom ✓), WS9 (Pools), WS12 (Run Timeout ✓)
Phase 3 (pending):  WS7 (Dynamic Mapping), WS11 (SLA), WS13 (Priority), WS14 (Templating), WS15 (Auth)
```
