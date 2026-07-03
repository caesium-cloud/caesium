# Design: Dynamic Fan-Out (Data-Proportional Parallelism)

> Status: Brainstorm/Design — proposal for runtime-materialized parallel task instances; nothing here is shipped. Grounded against the executor, run store, claimer, and cache identity code as of 2026-07.

## Problem

A vendor drops 400 files at 02:00 instead of the usual 40. Today a Caesium step
is one container: `process-files` loops over all 400 files serially inside one
pod for hours. The alternatives are all bad:

- **Hand-sharding**: `process-shard-1` … `process-shard-16` in YAML with a
  modulo convention in each command. Boilerplate, wrong the day the file count
  doubles, and a lie in the DAG (16 nodes that are really one step).
- **Parallelism inside the container**: a step that forks 400 workers loses
  per-unit retries, caching, observability, and rate limiting — everything the
  orchestrator exists to provide.
- **Backfill abuse**: backfill fans out across *runs* (one `JobRun` per
  interval, `internal/models/backfill.go`), but the partition set here is only
  knowable at runtime, *inside* a run, after a listing step executes.

Airflow has dynamic task mapping; Dagster has dynamic partitions. Both require
their SDK inside the task process. Caesium's differentiator is that any
container is a valid task — so the partition list must cross the container
boundary the way outputs already do: as a stdout marker. This is the tractable
*horizontal* slice of "Dataflow-style compute sized to the ETL": parallelism
scales with data volume; cluster elasticity (more nodes when N is large) stays
Kubernetes/Kueue's problem (`Step.Kueue`, `pkg/jobdef/definition.go:198-205`).

## Fit with Design Principles

1. **Container-native.** The producer emits `##caesium::partitions [...]` on
   stdout — the same protocol as `##caesium::output` and `##caesium::branch`
   (`pkg/task/output.go:17,38`). No SDK; each instance is an ordinary container
   with one extra env var.
2. **Declarative.** The consumer declares `fanOut:` in YAML; the DAG *shape*
   stays static and lint-checkable. Only the instance count is dynamic.
3. **Zero-dependency.** Instances are `TaskRun` rows in dqlite, claimed by the
   existing worker claimer. No queue, no broker.
4. **Smart by default.** The partition value feeds the cache identity hash, so
   on `caesium run retry` (`store.RetryFromFailure`,
   `internal/run/store.go:4614`) unchanged partitions cache-hit and only
   stragglers re-execute.
5. **Data engineering first.** Files, dates, table shards — daily ETL is
   embarrassingly parallel over a runtime-discovered set.

## Overview

```
                        ┌──────────────────────────────┐
                        │  fan-out group: process-file │
  ┌────────────┐        │  ┌─────┐ ┌─────┐     ┌─────┐ │        ┌────────────┐
  │ list-files │──────▶ │  │ f=a │ │ f=b │ ... │ f=Z │ │──────▶ │  publish   │
  └────────────┘        │  └─────┘ └─────┘     └─────┘ │        └────────────┘
   emits                │   N instances, one Task,     │         waits for ALL
   ##caesium::partitions│   N TaskRun rows, each with  │         instances
   ["a","b",...,"Z"]    │   CAESIUM_PARTITION=<value>  │         (trigger rules
                        └──────────────────────────────┘          see ONE group
                                                                   status)
```

- **One `Task` row, N `TaskRun` rows.** The static catalog (`models.Task`,
  `internal/models/task.go:11`) is untouched — the DAG validated at apply time
  (cycle detection, `pkg/jobdef/definition.go:1346`) keeps one node per step.
  Fan-out multiplies the *run-scoped* `TaskRun` rows
  (`internal/models/run.go:45`), where attempts, claims, hashes, and
  descriptors already live per-execution.
- **Expansion is transactional**, inside the producer's completion transaction,
  so distributed workers never observe a half-expanded group.
- **Fan-in is group-level.** Downstream sees the fanned predecessor as one node
  with one aggregate status; existing trigger rules
  (`collectPredecessorStatuses` / `satisfiesTriggerRule`,
  `internal/job/job.go:728-736`) apply unchanged.

## YAML

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: vendor-ingest
  maxParallelTasks: 16
  rateLimits:
    - { resource: vendor-api, limit: 100, window: 1m }
steps:
  - name: list-files
    image: ingest-tools:1.4
    command: ["list-new-files.sh"]        # emits ##caesium::partitions [...]
    next: [process-file]

  - name: process-file
    image: ingest-tools:1.4
    command: ["process-one.sh"]           # reads $CAESIUM_PARTITION
    dependsOn: [list-files]
    next: [publish]
    fanOut:
      from: list-files        # which predecessor's marker drives expansion
      env: CAESIUM_PARTITION  # injected var name (default CAESIUM_PARTITION)
      maxPartitions: 500      # lint + runtime cap (≤ server hard cap)
      maxParallel: 20         # in-flight cap for this group (≤ maxParallelTasks)
      onEmpty: skip           # skip | fail (empty partition list)
      failurePolicy: continue # fail_fast | continue (sibling handling)
    rateLimit: { resource: vendor-api, units: 1 }   # acquired per instance
    retries: 2                                      # per instance

  - name: publish
    image: ingest-tools:1.4
    command: ["publish.sh"]
    dependsOn: [process-file]   # fan-in: waits for the whole group
```

Marker forms (parsed in the same single pass as output/branch markers,
`parseMarkers`, `pkg/task/output.go:374`):

```sh
echo '##caesium::partitions ["2026-07-01","2026-07-02"]'   # JSON array
ls /drop/*.csv | while read f; do
  echo "##caesium::partition $f"                            # one per line
done
```

Values are strings, trimmed, deduplicated preserving first-seen order (the
`ParseBranches` posture, `pkg/task/output.go:295`). Limits below.

## Scenario Walkthroughs

**1. The 400-file vendor drop.** `list-files` emits 400 partition lines.
Caesium materializes 400 `TaskRun` instances; `maxParallel: 20` plus the
`vendor-api` rate limit (`ratelimit.RuleForTask`, `internal/job/job.go:1210`)
throttle them. Wall clock drops from hours to minutes ÷ 20. `publish` starts
once all 400 are terminal.

**2. Failure at partition 371, then retry.** With `failurePolicy: continue`,
siblings keep running; the group resolves `failed`, `publish` skips under
`all_success`, the run fails. The operator runs `caesium run retry <run>` —
`RetryFromFailure` keeps succeeded/cached rows. `list-files` cache-hits (same
inputs → same hash), so the 399 succeeded instances' identities (predecessor
hash + partition value) are unchanged and **cache-hit**; only partition 371
re-executes. The per-partition caching win needs no new cache machinery — just
the partition folded into `HashInput` (below).

**3. Empty drop.** `list-files` emits no partitions. With `onEmpty: skip` the
group resolves `skipped` in the same transaction; `propagateSkipped` semantics
(`internal/job/job.go:706`) and trigger rules decide what runs downstream — a
`publish` with `triggerRule: all_done` still runs, an `all_success` one skips.

## Backend Design

### Marker parsing (`pkg/task`)

Extend `Markers` (`pkg/task/output.go:330`) with `Partitions []string`, parsed
for `##caesium::partitions ` (JSON array, appended across lines) and
`##caesium::partition ` (single value) in `parseMarkers`. Parse-time
enforcement, mirroring the existing caps: scalar outputs are already capped at
`MaxOutputBytes` = 64 KB (`pkg/task/output.go:45,436-438`); partitions get an
independent `MaxPartitionListBytes` = 64 KB serialized plus a count cap passed
in by the executor (effective `maxPartitions`). Exceeding either **fails the
producing task** — truncating a partition list would silently drop data, which
is worse than a loud failure. A partition value must be non-empty, ≤ 256 bytes,
valid UTF-8; it is data (a filename, a date), never interpreted by Caesium.
Emitting partitions no successor consumes is a warning, not an error.

### Schema (`pkg/jobdef`)

New `FanOut` struct on `Step` (`pkg/jobdef/definition.go:214`), persisted onto
`models.Task` as a `FanOutConfig datatypes.JSON` column (the same
carry-scheduling-metadata-into-the-catalog pattern as
`RateLimitResource`/`RateLimitUnits`, `internal/models/task.go:28-29`). Lint
rules in `validateSteps` (`definition.go:797`): `fanOut.from` must name a
declared predecessor (via `computeStepAdjacency`); `maxPartitions` required,
> 0, ≤ the server hard cap; a `fanOut` step cannot be `type: branch` and cannot
itself be named in another step's `fanOut.from` (**no chained fan-out in v1** —
expansion of an expansion multiplies unboundedly); `env` must be a valid env
var name outside the `CAESIUM_PARAM_*` / `CAESIUM_OUTPUT_*` namespaces.

Static topology invariants hold by construction: instances are replicas of an
existing validated node and no new edges exist at runtime, so apply-time cycle
detection remains sound.

### Data model (`internal/models`)

On `TaskRun`:

- `PartitionValue string` (empty = not a fanned instance).
- `PartitionIndex int` default 0; `PartitionCount int` default 0 (0 = unfanned).
- Extend the existing composite index `idx_taskrun_jobrun_task`
  (`internal/models/run.go:47,49`) to a **unique** index over
  `(job_run_id, task_id, partition_index)`.

This is the honest hard part: the store addresses task state as
`WHERE job_run_id = ? AND task_id = ?` everywhere — `StartTask`
(store.go:1572), `RateLimitTask` (:1775), `retryTask` (:3198), `SkipTask`
(:3289), `completeTask`/`cacheHitTask`, descriptor updates. One row per
(run, task) is a load-bearing implicit invariant. Every write path that can
touch a fanned step must key on the `TaskRun` **primary key** (which both
executors already hold). A mechanical but wide refactor — Phase 1's bulk and
the reason this feature is not small.

On the *producer's* `TaskRun`: `Partitions datatypes.JSON` — the emitted list,
capped, for observability, `why`, and replay.

### Materialization: the expansion transaction

At run start, `RegisterTasks` (`internal/run/store.go:1093`, called from
`internal/job/job.go:645`) registers the fanned step as a single **template**
row (`partition_count = 0`) with its normal `OutstandingPredecessors`, so
nothing claims it early — `outstanding_predecessors = 0` gates both the claimer
(`internal/worker/claimer.go:257`) and owner dispatch (`store.go:1706`).

Expansion happens inside the producer's completion transaction (`completeTask`,
the same tx that today walks successor edges via `successorEdgesForRunTx`
(store.go:2157) and calls `batchDecrementPredecessorsTx` (store.go:2287)):

1. The executor passes `markers.Partitions` into `CompleteTaskWithResult`
   (store.go:1871) alongside output and branches.
2. For each successor whose `Task.FanOutConfig.from` is this task: **N = 0**
   applies `onEmpty` to the template row (skip reuses the `SkipTask` path with
   a `"fan-out produced no partitions"` reason); **N ≥ 1** rewrites the
   template as instance 0 (`partition_value` set, `partition_count = N`) and
   inserts instances 1…N-1 as copies — same task_id, image, command, priority,
   cache/schema snapshot columns, `Quarantine` copied (the distributed-parity
   rule from [`design-quarantined-replay.md`](design-quarantined-replay.md)) —
   each inheriting the template's *current* `outstanding_predecessors`.
3. The normal successor decrement runs (`batchDecrementPredecessorsTx`'s
   `task_id IN ?` predicate already matches every sibling row).
4. Commit. Only then are instances visible to the claimer.

One accounting fix rides along: run-completion checks compare terminal tasks
against the *static* task count today (`waitForRunCompletion(ctx, store, runID,
len(tasks), …)`, job.go:661,1548) — with expansion both must count live
`TaskRun` rows from the run snapshot instead.

Either the producer is complete AND the group exists, or neither — a crash
leaves the producer non-terminal and the retry re-runs it. dqlite's
single-writer Raft serializes concurrent predecessor completions, so expansion
and decrement cannot interleave. Write amplification is real: N inserts ride
one Raft transaction; the hard cap bounds it, one multi-row statement carries
it.

Local mode: the in-memory Kahn loop (`internal/job/job.go:1280`) queues
`TaskRun` identities rather than `Task` IDs for fanned steps; `runTask`
(job.go:896) takes the instance row and injects the partition env. The fanned
step stays a single node in `adjacency`/`indegree` (job.go:591-634); a
per-group counter tracks live instances.

### Scheduling, claiming, throttling

- **Distributed:** each instance is an ordinary pending `TaskRun` row; the
  claimer's atomic `UPDATE … ORDER BY tr.priority DESC, tr.created_at ASC
  LIMIT 1` (`internal/worker/claimer.go:248-270`) claims instances with zero
  changes — fan-out inherits priority ordering, lease expiry/reclaim, and
  node-selector filtering for free.
- **`fanOut.maxParallel`:** an in-flight `COUNT(*) … status='running'` subquery
  in the claim/dispatch predicates, and a check in the local dispatch loop
  before `taskPool.Submit`. The job-level `maxParallelTasks` pool
  (`worker.NewPool(maxParallel)`, job.go:1201) already bounds the total.
- **Rate limits:** unchanged. `acquireTaskRateLimit` (job.go:1209-1233) runs
  per dispatch; an over-limit instance parks via `RateLimitTask`'s
  `rate_limit_retry_after`, keyed per instance row — 400 instances against a
  `limit: 100/1m` resource drain in ~4 windows.

### Caching: partition identity (the big win)

Add `Partition string` to `cache.HashInput` (`internal/cache/hash.go:266`),
hashed as a `partition:<value>` line **only when non-empty** — the same
omit-when-absent pattern as `ResolvedImageDigest` (hash.go:301-303), so
existing cache entries for unfanned tasks keep their keys and no
`CacheVersion` bump is needed. Mirror the field into `HashInputBlob`
(hash.go:71) so `caesium why` can name the partition as the discriminating
field.

Two deliberate contracts:

- **The partition value is injected env but hashed as a first-class field, not
  smuggled through `Env`.** Both executors deliberately exclude volatile
  injected env (`CAESIUM_RUN_ID` etc.) from the hashed `mergedEnv`
  (job.go:950-956); the partition must be folded explicitly and visibly.
- **The sibling list is a scheduling instruction, not a data input.** An
  instance's identity folds its *own* partition value plus the producer's
  effective hash via `PredecessorHashes` (job.go:961-966) — never the whole
  list. The retry scenario thus works conservatively: producer cache-hits →
  same effective hash → unchanged partitions cache-hit. Honest limit: if the
  producer's own inputs changed (a genuinely new drop), its hash changes and
  all instances re-run. Per-partition skip across producer re-runs needs an
  explicit "this instance consumes only its partition value" contract —
  deferred, same posture as the per-step param-dependency deferral in
  [`design-quarantined-replay.md`](design-quarantined-replay.md).

### Fan-in: outputs, trigger rules, N-to-one

- **Group status** = `succeeded` iff all instances succeeded/cached; `failed`
  if any instance exhausted retries (under `continue`, evaluated when the last
  sibling lands; under `fail_fast`, at first failure, cancelling pending
  siblings); `skipped` if skipped pre-expansion or `onEmpty: skip` fired. This
  single status feeds `taskOutcomes` and trigger rules unchanged; the group
  decrements each downstream successor **once**, when it resolves — never once
  per instance.
- **Output aggregation** (`BuildOutputEnv`, `pkg/task/output.go:515`, consumed
  by the worker at `internal/worker/runtime_executor.go:497-502`): per-instance
  indexed env vars are rejected (400 partitions ⇒ env explosion). Each scalar
  output key aggregates to a JSON object keyed by partition value —
  `CAESIUM_OUTPUT_PROCESS_FILE_ROW_COUNT={"a":"42","b":"17",…}` — plus
  synthetic `…_PARTITION_COUNT/_SUCCEEDED/_FAILED`. Sorted keys make the
  aggregate deterministic, so it is safe as a `PredecessorOutputs` hash input
  (hash.go:369-384). The aggregate counts against `MaxOutputBytes` (64 KB); on
  overflow per-key aggregates are dropped (counts survive) and a downstream
  `inputSchema` requiring a dropped key fails closed per `schemaValidation`.
  Steps moving real data per partition should write to a BYO volume and emit
  `##caesium::output-ref` (output.go:32) — an aggregate of bounded references
  is exactly what that mechanism is for. The worker's
  `PredecessorOutputs(jobRunID, taskID)` (runtime_executor.go:200,497)
  aggregates across sibling rows in SQL — same contract in both modes.

### Retries

Per-instance: each instance carries its own `Attempt`/`MaxAttempts` from
`Task.Retries`, reusing `retryTask` (store.go:3198) keyed by instance row; a
sibling's retry never disturbs the others. Run-level `RetryFromFailure`
(store.go:4614) keeps succeeded/cached instance rows and resets failed ones —
the group is **not** re-expanded (the producer is terminal; recorded instances
are reused). Only a full re-run that re-executes the producer can change the
partition set.

### Receipts, replay, why, run-diff

- `TaskExecutionDescriptor` is already per-`TaskRun`
  (`internal/models/run.go:154`); each instance gets its own descriptor with
  the partition in `Runtime`, and the producer row records the emitted list.
  Receipts ([`design-reproduce.md`](design-reproduce.md)) gain a `--partition`
  selector; surfaces that assumed one `TaskRun` per `Task` (receipt get,
  `why --task`) must disambiguate or default to the group summary.
- `caesium why` names `partition` as a discriminating field via the blob.
  `caesium run diff` aligns instances across runs by partition **value** (never
  index — ordering is producer-dependent); added/removed partitions report as
  such.
- Quarantined replay: **v1 refuses baselines containing fanned groups**
  (fail-closed, consistent with that design's posture). Follow-up: re-expand
  from the recorded partition list in descriptors, never from a re-executed
  producer.

### REST

- `GET /v1/jobs/:id/runs/:run_id/tasks/:task_id/partitions` — paginated
  instance list: value, index, status, attempt, cache_hit, duration, error.
- `POST …/tasks/:task_id/partitions/:index/retry` — reset one failed instance
  (terminal runs only; re-evaluates fan-in on completion).
- Run detail payloads collapse fanned groups to one entry with
  `partition_count` + a status histogram; a 10k-instance run must not bloat
  every run list response.

## CLI

```sh
caesium run partitions <run-id> --task process-file [--status failed] [--json]
caesium run retry <run-id> --task process-file --partition "2026-07-01"
caesium job lint --path jobs/          # fanOut validation errors
caesium dev --once --path job.yaml     # local fan-out with live group progress
```

`--json` output on stdout, logs on stderr, per the repo's stdout-cleanliness
gate (CLAUDE.md testing guidelines).

## Frontend (Caesium Console)

- **JobDAG** (`ui/src/features/jobs/JobDAG.tsx`): a fanned step renders as one
  **grouped node** — stacked-card affordance, `×N` badge, segmented progress
  ring (succeeded/running/failed/pending). The graph never gains N nodes; 400
  partitions render identically to 4.
- **RunTimeline** (`RunTimeline.tsx`): one lane per group — an envelope bar
  (first instance start → last instance end) with a density strip, expandable
  to the top-K longest/failed instances.
- **TaskDetailPanel**: a virtualized, status-filterable partition table —
  value, status, attempt, duration, cache-hit, per-row log link and retry
  button (wired to the retry endpoint).

## Safety & Limits

| Limit | Default | Enforcement |
|---|---|---|
| Server hard cap on N | `CAESIUM_FANOUT_MAX_PARTITIONS` = 1024 | parse-time; exceeding **fails the producer** (never truncates) |
| Per-step cap | `fanOut.maxPartitions` (required) | lint (≤ hard cap) + parse-time |
| Partition value size | 256 bytes | parse-time |
| Partition list bytes | 64 KB serialized | parse-time |
| Group in-flight | `fanOut.maxParallel` | claim predicate / dispatch loop |
| Aggregate output size | `MaxOutputBytes` 64 KB | fan-in; degrade to counts, fail closed under `inputSchema` |
| No chained fan-out | — | lint |

Metrics: new `caesium_fanout_partitions_total{job,task}` and
`caesium_fanout_group_duration_seconds` series (not labels on existing series,
per the observability-isolation lesson in the replay design). Quarantine
propagation to instances is mandatory (`TaskRun.Quarantine`, run.go:82).

## Testing (integration-first)

Per the repo gate, every surface ships with a `test/` integration scenario
driving the real binary/server (no hand-seeded rows):

1. Apply a fan-out job; producer emits 5 partitions; assert 5 instances run,
   each sees `CAESIUM_PARTITION`, fan-in runs once, aggregate env visible
   downstream.
2. Failure matrix: `fail_fast` cancels pending siblings; `continue` resolves
   the group failed after all siblings; downstream `all_done` still runs.
3. Retry: fail one partition, `caesium run retry`, assert the 4 unchanged
   instances **cache-hit** (per-partition identity) and only one re-executes.
4. `onEmpty` both modes; caps (1025 partitions fails the producer loudly).
5. Distributed lane: expansion under `CAESIUM_EXECUTION_MODE=distributed`;
   worker crash mid-group → lease reclaim → siblings unaffected; rate-limit
   parking + drain with no over-issue.
6. CLI: `run partitions --json` stdout clean and parseable (`runCLIStdout`,
   never the stream-merging capture); partition retry end-to-end.
7. Playwright: grouped DAG node, partition table, per-partition retry.

## Phasing

1. **Substrate:** partition columns + unique index; instance-keyed store write
   paths; marker parsing + caps. (Widest, least visible.)
2. **Local executor:** expansion, group fan-in, env injection, `Partition` in
   `HashInput`, per-instance retries; `dev`/lint support.
3. **Distributed:** expansion in the completion tx, `maxParallel` claim
   predicate, distributed integration lane.
4. **Surfaces:** REST + CLI + UI group rendering; `why`/`run diff` alignment.
5. **Follow-ups:** replay re-expansion, value-verified per-partition skip.

## Non-Goals

- **No shuffle/reshard, no cross-partition communication.** Instances are
  independent; anything needing exchange between partitions is a data-plane
  framework (Spark/Beam) running *inside* one step.
- **Not a data plane.** Caesium moves partition *labels* (≤256 B), never
  partition data; data rides BYO volumes/object stores via `output-ref`.
- **No nested fan-out** (v1) and no fan-out of `branch` steps.
- **No cluster autoscaling** — node elasticity for large N belongs to K8s/Kueue
  ([`sovereignty.md`](sovereignty.md) posture).
- **No partition-aware backfill coupling** (v1): backfill remains fan-out across
  runs; M runs × N partitions is deliberate operator arithmetic under both caps.

## Open Questions

1. **Partial success.** Is a `minSuccessRatio` (≤k% partition failures still
   "succeeded-with-warnings") worth the trigger-rule ambiguity? A breaker keyed
   on partition-failure ratio is a natural trip signal for
   [`design-data-circuit-breaker.md`](design-data-circuit-breaker.md).
2. **Window-derived partitions.** Should `fanOut` optionally derive partitions
   from the scheduling window instead of a marker, aligning with
   [`design-window-scheduling.md`](design-window-scheduling.md) /
   [`design-backtesting.md`](design-backtesting.md) (backtests are fan-out
   across historical windows)? Marker-first keeps v1 honest.
3. **Per-partition resource profiles.** Should instances inherit right-sized
   requests from [`design-resource-right-sizing.md`](design-resource-right-sizing.md)
   keyed per partition (skewed file sizes), or per step only?
4. **Contract granularity.** Does `outputSchema` apply per instance (current
   plan) or additionally to the aggregate, and how does
   [`design-contract-enforcement.md`](design-contract-enforcement.md) count
   per-partition violations?
5. **Agent remediation.** Should the
   [`design-agent-in-the-loop.md`](design-agent-in-the-loop.md) action catalog
   gain `retry_partition` as a Tier-1 action (cheap, bounded, obviously safe)?
6. **Freshness interplay.** When
   [`design-freshness-scheduling.md`](design-freshness-scheduling.md) drives
   re-runs, does per-partition staleness justify pulling the deferred
   value-verified per-partition skip forward?
