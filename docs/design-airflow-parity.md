# Design: Airflow Functional Parity

> Status: Phase 1 + most of Phase 2 shipped — 9 of 15 workstreams implemented (WS1–4, 6, 8, 10, 12, 15) and SLA tracking (WS11) partially shipped. The shipped operator-facing subset is documented in [airflow-parity.md](airflow-parity.md); this file now tracks the five remaining workstreams (sensors, dynamic mapping, task pools, priority weights, templating).

## Overview

This plan closes the feature gaps between Caesium and Apache Airflow while preserving Caesium's container-first identity. Each workstream is independent (save the dependencies in the [graph](#dependency-graph)) and can be implemented in parallel. Workstreams are ordered P0 → P2.

## What shipped

Operator-facing behaviour for the shipped subset lives in [airflow-parity.md](airflow-parity.md), [backfill.md](backfill.md), and [sso-authentication.md](sso-authentication.md). Condensed record (the "why" is preserved; full implementation now lives in code):

| WS | Feature | Why it mattered | Lands in |
|----|---------|-----------------|----------|
| 1 | Task retries — `retries`/`retryDelay`/`retryBackoff`, attempt tracking, exponential backoff | transient failures (OOM, network blip, pull timeout) shouldn't need manual intervention | `internal/job/failure_policy.go` (`computeRetryDelay`) |
| 2 | Trigger rules — `all_success`/`all_done`/`all_failed`/`one_success`/`always` | unlocks error-handling DAG patterns (cleanup-on-failure, always-run notify, conditional joins) | `internal/job/failure_policy.go` (`satisfiesTriggerRule`) |
| 3 | Run parameters — `defaultParams`, `POST /v1/jobs/{id}/run` params, `CAESIUM_PARAM_*` env | parameterized runs without a job per variation | `internal/job/job.go` (`buildParamEnv`) |
| 4 | Backfill & catchup — date-range replay, `reprocess` none/failed/all, concurrency cap | historical reprocessing for data pipelines | `internal/job/backfill.go`, [backfill.md](backfill.md) |
| 6 | Branching — `type: branch`, `##caesium::branch` marker, `BranchSelections` persisted | runtime-conditional DAGs | `internal/job/branch.go`, `pkg/task/output.go` |
| 8 | Task outputs / XCom — `##caesium::output` → `CAESIUM_OUTPUT_<STEP>_<KEY>` env (64KB, last-write-wins) | inter-task data passing with no external system | `pkg/task/output.go` (`ParseOutput`) |
| 10 | Pause / unpause — `Paused`, `PUT /v1/jobs/{id}/pause`\|`/unpause`, 409 on manual run | suspend a schedule without deleting it | `api/rest/controller/job/pause.go` |
| 12 | Run timeout — `runTimeout` wraps the run in `context.WithTimeout` | cap runaway pipelines | `internal/job/job.go` |
| 15 | Auth & API keys — keyed-hash API-key middleware + roles, and native **SSO** (OIDC/SAML/LDAP) built on top | secure the API | `internal/auth/`, [sso-authentication.md](sso-authentication.md) |

> WS8 shipped differently from the original design: instead of a file-based `/caesium/output/` scheme it uses the stdout-marker protocol above. Non-string JSON values coerce to strings; malformed JSON is skipped.

### Known gaps in shipped features

These are real, actionable limitations of already-shipped workstreams — track them here rather than burying them:

- **WS6 branching is local-mode only.** Under `CAESIUM_EXECUTION_MODE=distributed`, the worker completes a branch task via `CompleteTaskClaimed` without evaluating branch selections, so every successor unblocks. Distributed branching needs the orchestrator to parse branch selections from task logs after completion and apply skips before successors become claimable. (Same crash-window class as trigger-rule evaluation: a run resumed after a crash between `CompleteTask` and the skip writes can run branches that should have been skipped — a transactional successor-resolution or WAL-based recovery fixes both.)
- **WS8 task outputs** still lack a dedicated read API (`GET /v1/jobs/{id}/runs/{run_id}/tasks/{task_id}/outputs`) and a mount-based path for larger payloads.
- **WS11 SLA tracking is partial:** breach detection ships (`internal/notification/watcher.go` scans running and completed-by SLAs and emits `SLAMissed`), but predictive at-risk alerting and escalation chains are designed separately in [design-sla-management.md](design-sla-management.md) and not yet built.

---

## Remaining workstreams

## Workstream 5: Sensors (P1)

**Why**: Waiting for external conditions (file arrives, API returns 200, upstream job completes) is a core orchestration primitive. Without sensors, users must build polling into their container images, duplicating logic across every pipeline.

A sensor is a step with `type: sensor` that repeatedly runs a container until it exits 0 (condition met). Non-zero exits are retried at `pokeInterval` until `timeout`.

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

### Implementation steps

1. **Schema**: Add `Type` (default `task`, also `sensor`), `PokeInterval`, `Timeout`, `SoftFail` to `Step` in `pkg/jobdef/definition.go`.
2. **Model**: Add corresponding fields to `Task`; add `task`/`sensor` `TaskType` constants.
3. **Importer**: Propagate sensor fields from step definition to Task model.
4. **Sensor executor** (`internal/job/sensor.go`): run the container; exit 0 → `succeeded`; non-zero → sleep `pokeInterval`, re-run; elapsed > `timeout` → `failed` (or `skipped` if `softFail`). Emit `SensorPoke` events.
5. **Integration**: in `internal/job/job.go`, dispatch to the sensor executor when `task.Type == "sensor"`.
6. **Distributed mode**: sensor tasks are claimed like regular tasks; the claiming worker runs the poke loop. Lease renewal must account for long-running sensors.
7. **Metrics**: `caesium_sensor_pokes_total`, `caesium_sensor_timeout_total`.
8. **Built-in sensor images**: example Dockerfiles (HTTP endpoint, file existence, job completion) under `docs/examples/sensors/`.
9. **Cross-job sensor**: an `ExternalJobSensor` that polls `GET /v1/jobs/{alias}/runs?status=succeeded&limit=1`.
10. **Tests**: poke loop, timeout, soft fail; integration test succeeding after 3 pokes.
11. **Docs**: update `docs/job-schema-reference.md`; add `docs/sensors.md`.

Files: `pkg/jobdef/definition.go`, `internal/models/task.go`, `internal/jobdef/importer.go`, `internal/job/sensor.go` (new), `internal/job/job.go`, `internal/worker/worker.go`, `internal/metrics/metrics.go`, `internal/event/bus.go`, `docs/examples/sensors/`, `docs/sensors.md`.

## Workstream 7: Dynamic Task Mapping (P2)

**Why**: Fan-out patterns where the number of parallel tasks isn't known until runtime — one task per file in a directory, per partition in a table, per item in an API response.

A `map` step runs a container whose stdout is a JSON array. The executor creates one task instance per element from a template step, then waits for all instances before continuing the DAG.

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

### Implementation steps

1. **Schema**: add `map` as a `Type` and a `MapTemplate` field (the template step to fan out); validate the template exists and has no explicit `dependsOn`.
2. **Model**: add `map` to `TaskType`; add `MapParentRunID` to `TaskRun`.
3. **Dynamic TaskRun creation** (`internal/job/map.go`): run the map container, capture stdout JSON array, create a `TaskRun` per element with `CAESIUM_MAP_VALUE`/`CAESIUM_MAP_INDEX` injected; track spawned IDs.
4. **Aggregation**: after all mapped runs complete, evaluate downstream trigger rules against the aggregate result.
5. **Concurrency**: mapped tasks respect the job's `maxParallelTasks`.
6. **UI**: show mapped tasks as a collapsible group with per-instance status.
7. **Tests**: JSON parsing, fan-out creation, aggregation; integration over a 3-element array.
8. **Docs**: `docs/dynamic-tasks.md`.

Files: `pkg/jobdef/definition.go`, `internal/models/task.go`, `internal/models/run.go`, `internal/job/map.go` (new), `internal/job/job.go`, `ui/src/`, `docs/dynamic-tasks.md`.

## Workstream 9: Task Pools (P1)

**Why**: Limit concurrency across jobs, not just within one. When 50 tasks across different jobs all hit the same database, a shared pool caps the blast radius.

### Implementation steps

1. **Pool model** (`internal/models/pool.go`): `Pool{ID, Name (uniqueIndex), Slots (default 16), CreatedAt, UpdatedAt}`.
2. **Step schema**: optional `pool` (pool name) and `poolSlots` (int, default 1) on `Step`.
3. **Task model**: `Pool`, `PoolSlots` fields.
4. **Pool manager** (`internal/pool/pool.go`): track active slot usage; before dispatch, acquire slots or queue the task.
5. **API**: CRUD endpoints at `/v1/pools`.
6. **Default pool**: created on startup with `CAESIUM_DEFAULT_POOL_SLOTS`.
7. **Metrics**: `caesium_pool_used_slots`, `caesium_pool_queued_tasks`.
8. **Tests**: slot acquire/release, queue ordering, multi-slot tasks.
9. **Docs**: `docs/pools.md`.

Files: `internal/models/pool.go` (new), `internal/models/models.go`, `internal/pool/pool.go` (new), `pkg/jobdef/definition.go`, `internal/models/task.go`, `internal/jobdef/importer.go`, `internal/job/job.go`, `api/rest/controller/pool/` (new), `api/rest/bind/bind.go`, `internal/metrics/metrics.go`, `docs/pools.md`.

## Workstream 11: SLA Tracking (P2) — partially shipped

Breach detection shipped (see [Known gaps](#known-gaps-in-shipped-features)): `internal/notification/watcher.go` scans running runs and `completedBy` deadlines and emits `SLAMissed` without killing the task; `SLAConfig` exists in `pkg/jobdef`. The remaining work — predictive **at-risk** alerting (EWMA over historical durations) and stage-based escalation chains — is owned by its dedicated design: **[design-sla-management.md](design-sla-management.md)**. Do not duplicate that design here.

## Workstream 13: Priority Weights (P2)

**Why**: When resources are constrained, high-priority tasks should run before low-priority ones.

Not shipped — the distributed claimer is pure FIFO today (`internal/worker/claimer.go`, `ORDER BY tr.created_at ASC`; no `Priority` column on `Task`/`TaskRun`/`jobdef`). The full design (priority-ordered distributed claiming, plus concurrency strategies and rate limiting) now lives in **[design-concurrency-priority.md](design-concurrency-priority.md)**; the mechanical change is a `Priority` column threaded Job→TaskRun plus a `ORDER BY tr.priority DESC, tr.created_at ASC` claim query.

## Workstream 14: Templating (P2)

**Why**: Runtime variable substitution in step env vars and commands — pass the logical date, run ID, or job params into container configuration without hardcoding.

### Implementation steps

1. **Template engine** (`internal/job/template.go`): Go `text/template` over step env values. Variables: `{{ .RunID }}`, `{{ .JobAlias }}`, `{{ .LogicalDate }}`, `{{ .Params.<key> }}`, `{{ .Env.<key> }}`.
2. **Rendering** (`internal/job/job.go`): render env values (and `command` elements) before passing to the runtime; template errors fail the task.
3. **Validation** (`pkg/jobdef/definition.go`): a `ValidateTemplates` method parses all env/command templates at definition time.
4. **Tests**: rendering, missing variables, malformed templates.
5. **Docs**: `docs/templating.md`.

Files: `internal/job/template.go` (new), `internal/job/job.go`, `pkg/jobdef/definition.go`, `docs/templating.md`.

---

## Dependency graph

All remaining workstreams are independent except:

```
Workstream 2 (Trigger Rules, shipped) ← Workstream 7 (Dynamic Mapping) uses rules for aggregation
Workstream 9 (Task Pools)             ← Workstream 13 (Priority) uses pools for ordering
```

Recommended order for the remaining work: WS5 (Sensors) and WS9 (Task Pools) first (highest operator value, P1), then WS7/WS13/WS14 (P2). WS11 and WS13 are tracked primarily in [design-sla-management.md](design-sla-management.md) and [design-concurrency-priority.md](design-concurrency-priority.md) respectively.
