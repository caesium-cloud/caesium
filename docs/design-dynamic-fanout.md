# Design: Dynamic Fan-Out (Data-Proportional Parallelism)

> Status: Shipped — runtime-materialized parallel task instances via `fanOut` and `##caesium::partitions`. Implementation: [`exec-plans/completed/dynamic-fanout.md`](exec-plans/completed/dynamic-fanout.md). Grounded against the executor, run store, claimer, and cache identity code as of 2026-07, amended 2026-08-25 with [`## Structured Partitions`](#structured-partitions-key--fingerprint--dependson) (re-grounded on that date) and 2026-08-29 with [`## Group Output Contracts`](#group-output-contracts-groupoutputschema) (design only — not implemented; issue #357).

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
   embarrassingly parallel over a runtime-discovered set. The set is not always
   *embarrassingly* parallel, though: a dbt project's models are a graph with
   per-model checksums, which is why a partition can be a structured object
   rather than a bare label
   ([`## Structured Partitions`](#structured-partitions-key--fingerprint--dependson)).

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
- **Expansion is transactional**, inside the producer's completion transaction —
  normalization, validation (including the in-group cycle check), and every
  instance insert commit together, so distributed workers never observe a
  half-expanded group. Caesium has **three** DAG-advancement paths, though, and
  the run-owner in-memory one does not route through that transaction; expansion
  is wired into all three along the seam branch selections already use
  ([`## Three advancement paths, one expansion`](#three-advancement-paths-one-expansion)).
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
`parseMarkers`, `pkg/task/output.go:374`); values are strings, trimmed,
deduplicated preserving first-seen order (the `ParseBranches` posture,
`pkg/task/output.go:295`):

```sh
echo '##caesium::partitions ["2026-07-01","2026-07-02"]'   # JSON array
ls /drop/*.csv | while read f; do
  echo "##caesium::partition $f"                            # one per line
done
```

An array element may also be a **JSON object** carrying a per-unit content
fingerprint and intra-group ordering — a strict superset of the string form, with
identical behavior and identical hashes when only strings are emitted. See
[`## Structured Partitions`](#structured-partitions-key--fingerprint--dependson).

## Scenario Walkthroughs

**1. The 400-file vendor drop.** `list-files` emits 400 partition lines.
Caesium materializes 400 `TaskRun` instances; `maxParallel: 20` plus the
`vendor-api` rate limit (`ratelimit.RuleForTask`, `internal/job/job.go:1210`)
throttle them. Wall clock drops from hours to minutes ÷ 20. `publish` starts
once all 400 are terminal.

**2. Failure at partition 371, then retry.** With `failurePolicy: continue`,
siblings keep running; the group resolves `failed`, `publish` skips under
`all_success`, the run fails. `caesium run retry <run>` (`RetryFromFailure`
keeps succeeded/cached rows) re-runs only the failures: `list-files` cache-hits
(same inputs → same hash), so the 399 succeeded instances' identities
(predecessor hash + partition value) are unchanged and **cache-hit**; only
partition 371 re-executes. No new cache machinery — just the partition folded
into `HashInput` (below).

**3. Empty drop.** `list-files` emits no partitions. With `onEmpty: skip` the
group resolves `skipped` in the same transaction; `propagateSkipped` semantics
(`internal/job/job.go:706`) and trigger rules decide what runs downstream — a
`publish` with `triggerRule: all_done` still runs, an `all_success` one skips.

**4. The dbt model DAG.** `dbt ls --select state:modified+ --output json` emits
one partition per model, each an object carrying the model's file checksum as
`fingerprint` and its parents as `dependsOn`. One `run-model` step becomes 300
instances that respect dimension-before-fact ordering inside the group, and each
instance's identity carries its own checksum — so an unchanged model can
cache-hit while its changed sibling misses. This is the case bare-string
partitions cannot express at all; see
[`## Structured Partitions`](#structured-partitions-key--fingerprint--dependson).

## Backend Design

### Marker parsing (`pkg/task`)

Extend `Markers` (`pkg/task/output.go:330`) with `Partitions []Partition` — a
normalized element type whose string form is `{Key: <value>}` and whose object
form additionally carries `Fingerprint`, `DependsOn`, and scalar `Attributes`
([`## Structured Partitions`](#structured-partitions-key--fingerprint--dependson))
— parsed for `##caesium::partitions ` (JSON array, appended across lines) and
`##caesium::partition ` (single value, string form only) in `parseMarkers`. Parse-time
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
existing validated node and no new edges exist **in the catalog graph** at
runtime, so apply-time cycle detection remains sound over it. That is now only
half the story — a partition object's `dependsOn` introduces run-scoped edges
*inside* a group that apply-time validation cannot see, so the instance graph
carries its own cycle check at expansion time
([`## Structured Partitions`](#structured-partitions-key--fingerprint--dependson)).

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
unfanned tasks keep their keys and no `CacheVersion` bump is needed. Mirror the
field into `HashInputBlob` (hash.go:71) so `caesium why` can name the partition
as the discriminating field. Two deliberate contracts:

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

A partition's optional `fingerprint` adds two more hashed fields
(`PartitionFingerprint`, `PartitionAttributes`) and is what finally makes that
deferred contract *expressible*; it does not on its own lift the honest limit
above, which needs an orthogonal chain break. Both are worked through in
[`## Structured Partitions`](#structured-partitions-key--fingerprint--dependson).

### Fan-in: outputs, trigger rules, N-to-one

- **Group status** = `succeeded` iff all instances succeeded/cached; `failed`
  if any instance exhausted retries (under `continue`, evaluated when the last
  sibling lands; under `fail_fast`, at first failure, cancelling not-yet-started
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
  overflow the group **fails** with a typed `*FanInAggregateTooLargeError`
  (`pkg/task/output.go:631`) rather than degrading to the counters, because
  dropping the per-key aggregates silently changed the contract a downstream
  step read — see [`### The size cap comes
  first`](#the-size-cap-comes-first). The aggregate itself carries no declared
  contract until one is declared: `outputSchema` describes one instance, not the
  fold ([`## Group Output
  Contracts`](#group-output-contracts-groupoutputschema)).
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
- Quarantined replay: **shipped** (v1 refused baselines containing fanned
  groups; the follow-up below landed on #359). Each fan-out producer's
  descriptor carries a `fanOut` section — the normalized partition list
  (key + fingerprint + `dependsOn` + attributes) and the fanned successor steps
  it expanded — and replay re-materializes the group from that recorded list,
  never from a re-executed producer. A producer that does re-execute under a
  param override runs and is reproduced, but its freshly emitted list is
  ignored: its completion finds an already-expanded successor and leaves it
  alone. The fail-closed refusal narrows to the cases that genuinely cannot be
  reconstructed — no list recorded (a baseline predating this capture), a list
  that disagrees with the instances the baseline materialized, or a reused cache
  entry whose own recorded list disagrees with the baseline's.

### REST

- `GET /v1/jobs/:id/runs/:run_id/tasks/:task_id/partitions` — paginated
  instance list: value, index, status, attempt, cache_hit, duration, error.
- `POST …/tasks/:task_id/partitions/:index/retry` — reset one failed instance
  (instance must be failed; a finished run is re-opened and resumed through
  `job.New` → `Run` in local mode, or the dispatcher in distributed mode;
  re-evaluates fan-in on completion). Does not cascade to dependents that
  already succeeded.
- Run detail payloads collapse fanned groups to one entry with
  `partition_count` + a status histogram; a 10k-instance run must not bloat
  every run list response.

## Structured Partitions (`key` + `fingerprint` + `dependsOn`)

> Amendment, 2026-08-25. The `file:line` anchors **in this section** were re-read
> against `master` on that date; anchors in the sections above were captured
> 2026-07 and have drifted as those files grew (e.g. `CompleteTaskWithResult` is
> now `internal/run/store.go:1904`, `RetryFromFailure` `:4790`, `Step`
> `pkg/jobdef/definition.go:541`). Nothing above is wrong in substance; the
> numbers are stale.

The forcing consumer is
[`superpowers/specs/2026-08-25-dag-native-infrastructure-deployment-design.md`](superpowers/specs/2026-08-25-dag-native-infrastructure-deployment-design.md)
§5.4, which builds per-unit infrastructure delivery on this feature and cannot
express its pattern on bare-string partitions. That spec is one consumer; the
capability is data-engineering-first and is specified here, not there. The
converse also holds: the chain break (`cache.chain: values`, spec §4) that makes
a per-unit fingerprint *effective* is specified there and implemented by Stream
A of `exec-plans/completed/infra-deploy.md`, not here.

### Why a bare string is not enough

A partition today is an opaque label. Two things a discovering producer knows,
and cannot say:

1. **What version of this unit it found.** The design folds the partition
   *value* into the identity hash (`### Caching: partition identity`, above),
   which distinguishes partitions from each other but never *versions* of the
   same partition. `dim_customer` cannot cache-hit when its SQL is unchanged and
   miss when it changed, because nothing about its content reaches the key. dbt
   already computes exactly this number — the per-model file checksum that
   `state:modified+` compares — and throws it away at the container boundary.
2. **What has to run before what.** Fan-out is parallelism, not a graph. dbt
   models depend on models (dimensions before facts); Terraform stacks depend on
   stacks (VPC before apps); a partitioned load may need `create-table` before
   `load-partition`. None of it is expressible inside a group.

Both are carried by letting a partition be a JSON **object** instead of a string.

### Marker grammar

```sh
# object form — any element may be an object
echo '##caesium::partitions [
  {"key":"dim_customer", "fingerprint":"sha256:ab…", "dependsOn":[]},
  {"key":"fct_orders",   "fingerprint":"sha256:cd…", "dependsOn":["dim_customer"], "materialization":"incremental"}
]'

# string form — unchanged meaning, byte-identical behavior
echo '##caesium::partitions ["2026-07-01","2026-07-02"]'
ls /drop/*.csv | while read f; do echo "##caesium::partition $f"; done
```

An element is either a JSON **string** (the partition key, exactly as today) or a
JSON **object**:

| Field | Type | Required | Meaning |
|---|---|---|---|
| `key` | string | yes | the partition value; becomes `CAESIUM_PARTITION`. Same rules as the string form (non-empty, ≤ 256 B, valid UTF-8, unique in the set) |
| `fingerprint` | string | no | content address of this unit's inputs, `sha256:<64 hex>`. Folded into **this instance's** identity hash |
| `dependsOn` | array[string] | no | sibling `key`s that must reach terminal success before this instance becomes ready |
| *anything else* | scalar | no | free-form per-unit attributes (`root`, `materialization`, `region`, …). Carried to the container and **hashed** |

`##caesium::partition <value>` (the one-per-line form) stays string-only; a
producer that needs objects uses the array form. Mixing forms in one array is
legal — `["a", {"key":"b","dependsOn":["a"]}]` is a two-partition group in which
`a` carries no fingerprint.

### Backward compatibility

A bare-string array keeps **exactly** its current meaning and, critically, its
current *hash*. A string element normalizes to `{key: <string>}` with no
fingerprint, no `dependsOn`, no attributes; every added `HashInput` field is
omit-when-empty (the `ResolvedImageDigest` pattern,
`internal/cache/hash.go:301-303`), so a string-form instance hashes
byte-identically to what the unamended design computes. **No `CacheVersion`
bump.** A job that never emits an object never observes this amendment — not in
the key, not in the env, not in ordering.

### Validation — what fails the producer, loudly

The design's cap posture is "fail the producing task, never truncate", because a
silently dropped partition is a silently skipped unit of work. Structured
partitions extend that posture rather than soften it. Parse-time (`parseMarkers`,
`pkg/task/output.go:408`) and expansion-time rejections both **fail the
producer**:

| Condition | Why it is fatal, not lenient |
|---|---|
| element is neither string nor object | ambiguous input; guessing is how data goes missing |
| `key` missing / empty / > 256 B / invalid UTF-8 | the rule the string form already enforces |
| duplicate `key`, *identical* payload | deduplicated first-seen — the `ParseBranches` posture (`pkg/task/output.go:329`). **Not** fatal |
| duplicate `key`, *conflicting* payload | two different fingerprints for one unit: the producer's model of the world is inconsistent |
| `fingerprint` not `sha256:<64 hex>` | reuses `validSHA256Ref` (`pkg/task/output.go:193`). A malformed digest in a cache key silently weakens content addressing — the argument that validator already makes for `##caesium::output-ref` |
| attribute value is null / object / array | reuses `scalarOutputValue` (`pkg/task/output.go:229`) as the *predicate*, but **not** its posture: a non-scalar output value is dropped, a non-scalar attribute **fails the producer**. Normalization never removes a field (see below) — accepting a silently-shortened work description is the failure this whole table exists to prevent |
| `dependsOn` names a key not in the set | the producer believes in a unit it did not emit. Ignoring it would run a fact table before its dimension — the exact failure ordering exists to prevent |
| `dependsOn` contains the element's own key | a 1-cycle |
| the in-group graph has a cycle | see below |

Note the asymmetry with `##caesium::output`, which *skips* malformed lines
(`pkg/task/output.go:290-293`). Outputs are advisory data; partitions are the
work list. A dropped output degrades a downstream env var; a dropped partition
means a file never got processed and the run went green.

#### Normalization is lossless, by definition

"Normalized", used throughout this section, means exactly three things and
**never** a fourth:

1. a string element is lifted to `{"key": <string>}`;
2. object keys are sorted;
3. the result is canonically re-encoded.

Normalization **never drops a field, and never repairs one**. Every rejection in
the table above happens *before* normalization and fails the producing task; a
partition that normalizes is a partition that was already wholly valid. This is
load-bearing: if normalization could quietly discard, say, a non-scalar
attribute, the run would proceed against a work description the producer did not
emit — omitted from `CAESIUM_PARTITION_JSON`, omitted from the identity hash, and
succeeding with different semantics than the container intended. The only thing
a producer may emit that Caesium silently absorbs is a *byte-identical duplicate
key*, which by construction changes no semantics.

### Caps

Objects are bigger than strings, and the existing 64 KB list cap and
1024-partition count cap become mutually unsatisfiable for the object form
(1024 × ~150 B of dbt model name + checksum ≈ 150 KB). The caps are therefore
restated over the **normalized** encoding — the same bytes that are hashed,
injected, and persisted:

| Limit | Value | Enforcement |
|---|---|---|
| `MaxPartitionListBytes` | **256 KB** normalized (was 64 KB) | parse-time; fails the producer |
| Per-partition object | 2 KB normalized | parse-time; fails the producer |
| Attributes per object | 16 keys | parse-time; fails the producer |
| `key` value | 256 B | parse-time (unchanged) |
| Count | `min(fanOut.maxPartitions, CAESIUM_FANOUT_MAX_PARTITIONS)` | parse-time (unchanged) |

256 KB is not free: the normalized list is persisted on the producer's
`TaskRun.Partitions` column and rides one dqlite Raft write, which is why the
byte cap exists at all and why it stays a constant rather than becoming a new
operator dial. It is deliberately **not** shared with `MaxOutputBytes`
(`pkg/task/output.go:45`): that 64 KB budget covers a step's scalar outputs and
the fan-in aggregate, and a large partition list must not eat into it.

### Cache identity — exactly which fields

`cache.HashInput` (`internal/cache/hash.go:266`) gains three fields beyond the
`Partition string` the base design already adds, all written with the
omit-when-empty pattern:

| Field | Hashed line | When |
|---|---|---|
| `Partition string` | `partition:<key>` | non-empty (base design) |
| `PartitionFingerprint string` | `partition_fingerprint:<value>` | non-empty |
| `PartitionAttributes map[string]string` | `partition_attr:<k>=<v>`, keys sorted | non-empty |

Sorted-map iteration mirrors how `Env`, `PredecessorOutputs`, and `RunParams` are
already folded in (`internal/cache/hash.go:307-314,369-384,387-394`). All three
mirror into `HashInputBlob` (`internal/cache/hash.go:71`) **verbatim** —
partition data is labels, not credentials — so `caesium why` can name
`partition_fingerprint` as *the* discriminating field instead of reporting "the
hashes differ".

`dependsOn` is **not hashed**. It is a scheduling instruction, exactly like the
sibling list, `kueue.queueName`, and `rateLimit`
(`pkg/jobdef/definition.go:561-566`; `internal/cache/hash.go:337`). Reordering a
group must not invalidate work that did not change. The corollary is a rule for
step authors: **a container must not derive data behavior from `dependsOn`.**
Anything behavioral belongs in an attribute, which *is* hashed.

Attributes are hashed for a concrete reason:
`{"key":"app-api","root":"stacks/api"}` tells the container *where to work*. If
`root` moved and the key did not, an unhashed attribute would serve a stale hit
against the wrong directory. Hashing every non-`dependsOn` field closes that hole
by construction, and is why `CAESIUM_PARTITION_JSON` carries the normalized
object rather than the emitted bytes: injected, hashed, and persisted are the
same object, and normalization is lossless so that object is also what the
producer emitted.

#### The honest limit: a fingerprint alone does not buy per-unit skip

This is the part that does not survive a casual reading of the proposal. An
instance's identity folds the producer's effective hash through
`PredecessorHashes` (`internal/job/job.go:964-971`; in the distributed lane
`internal/run/store.go:4628`, which reads `COALESCE(effective_hash, hash)`). So
when the producer's own inputs change — a new git commit, a new listing — the
producer's hash changes, that change propagates into **every** instance, and all
N re-run *no matter what the fingerprints say*. The base design already records
this as an honest limit; adding a fingerprint does not by itself lift it.

Two mechanisms close the gap, and neither is this amendment's to invent:

- **`cache.EquivalentPriorHash`** (`internal/cache/shortcircuit.go:56`)
  suppresses the cascade when a re-executed producer emits **byte-identical
  output**. It does not help here: one changed model in three hundred changes one
  fingerprint, the emitted list is no longer byte-identical, and the substitution
  is (correctly) refused. It is conservative by construction and stays that way.
- **`cache.chain: values`** — the step-level knob added, and shipped (Stream A
  of `exec-plans/completed/infra-deploy.md`), by
  [`2026-08-25-dag-native-infrastructure-deployment-design.md`](superpowers/specs/2026-08-25-dag-native-infrastructure-deployment-design.md)
  §4, which excludes `PredecessorHashes` from a step's key while still hashing
  `PredecessorOutputs`. That is the chain break, and it is orthogonal to this
  amendment.

So the composition is explicit: **`fanOut` gives the instances, `fingerprint`
gives each instance a per-unit discriminator, and `cache.chain: values` decides
whether the producer's own churn still dominates the key.** Fingerprints without
the chain break are correct but conservative — every instance re-runs when the
producer does; never a stale hit. With it, `dim_customer` cache-hits on its
unchanged checksum while `fct_orders` misses on its changed one, which is the
"value-verified per-partition skip" the base design defers: made *expressible*
here, *enabled* there.

One operational corollary, because it will bite someone: under `chain: values`
the producer's `PredecessorOutputs` **are** still hashed, so a discover step that
emits `##caesium::output {"unit_count":"41"}` re-keys every instance whenever the
count moves. Per-unit data belongs in partition attributes, not in the producer's
scalar outputs.

### Ordering inside the group

Ordering rides the mechanism that already exists. Readiness is a single scalar on
the row in the two SQL-gated paths:

- the distributed claimer's atomic claim gates on
  `tr.outstanding_predecessors = ?` bound to `0`
  (`internal/worker/claimer.go:257,266`);
- owner dispatch gates on the same column (`internal/run/store.go:1739`);
- the local executor re-seeds its in-memory indegree map **from that column**
  (`internal/job/job.go:676`).

The run-owner in-memory path is the exception and needs the same seeding applied
to `RunState.indegree` (`internal/run/owner_state.go:65-74`), which it maintains
itself and never re-reads from SQL. Ordering must be seeded once at expansion and
mirrored into whichever engine is advancing the run — see
[`## Three advancement paths, one expansion`](#three-advancement-paths-one-expansion).

So an instance is seeded at expansion with
`outstanding_predecessors = <template's current value> + <in-group indegree>`,
and the existing "decrement to zero → ready" machinery carries ordering for
free. **The distributed claimer needs no new predicate to honour ordering** —
contrary to the obvious worry, the claim side is already general enough. Two
other places are not:

1. **The decrement is keyed wrong for in-group edges.**
   `batchDecrementPredecessorsTx` (`internal/run/store.go:2333`) updates
   `WHERE job_run_id = ? AND task_id IN ?`. That is exactly right for the
   *cross-step* edge (producer → group: one completion must decrement every
   sibling) and exactly wrong for the *in-group* edge (`dim_customer` completing
   must decrement only its dependents). In-group edges therefore need a sibling
   decrement keyed on the `TaskRun` **primary key** — `WHERE id IN ?`. Inside the
   completing instance's transaction: one indexed range read of the group over
   `idx_taskrun_jobrun_task`, decode the non-terminal siblings'
   `PartitionDependsOn`, one `UPDATE … WHERE id IN (…)`. Bounded by N ≤ 1024
   rows. A run-scoped instance-edge table was considered and rejected: a new
   model and a new migration to replace one bounded scan.
2. **The decrement must ride every terminal-success path**, not only
   `completeTask` (`internal/run/store.go:2621`). `cacheHitTask`
   (`internal/run/store.go:1933`) is a separate path that also releases
   successors, and a cache-hit instance is a satisfied dependency.

#### Failure inside an ordered group is mandatory skip, not a hang

If `dim_customer` exhausts its retries, `fct_orders`'s counter is never
decremented and never reaches zero. Nothing claims it; nothing fails it. The
distributed run waits until the run timeout, and the local run trips the
terminal-count guard with "remaining tasks may be waiting on unresolved
dependencies" (`internal/job/job.go:1554-1558`). The skip cascade is therefore
**load-bearing, not a nicety**: `failTask` (`internal/run/store.go:3174`) must
mark the failed instance's transitive in-group dependents `skipped` with reason
`"fan-out dependency <key> failed"` — the instance-keyed analogue of
`skipTaskAndDescendantsTx` (`internal/run/store.go:3075`), reusing
`markTaskSkippedTx` (`internal/run/store.go:2457`) per row.

Trigger rules are unaffected and stay a **step-level** concept.
`predecessorStatusesTx` (`internal/run/store.go:2488`) resolves predecessors from
`task_edges`, which contains no in-group edges, so an instance's
`shouldRunTaskTx` check (`internal/run/store.go:2596`) evaluates the group's
cross-step predecessors exactly as today. In-group dependency failure is governed
by the skip cascade above, never by `triggerRule`.

### Cycle detection at expansion time

The base design asserts: *"Static topology invariants hold by construction:
instances are replicas of an existing validated node and no new edges exist at
runtime, so apply-time cycle detection remains sound."* **`dependsOn` makes the
second clause false.** Run-scoped edges now exist, they are supplied by a
container at runtime, and the apply-time cycle check
(`pkg/jobdef/definition.go:2044`, over `computeStepAdjacency` at
`pkg/jobdef/definition.go:1851`) cannot see them.

The invariant splits, and both halves must be stated:

- **Catalog graph** — unchanged, validated at apply time, still acyclic. Fan-out
  still adds no node and no edge to it.
- **Instance graph** — run-scoped, producer-supplied, and validated at
  **expansion** time: a Kahn pass over the normalized partition set inside the
  producer's completion transaction, *before* any instance row is inserted. A
  cycle fails the producing task with the offending keys named and the
  transaction rolls back — either the producer is complete and a valid group
  exists, or neither.

The in-group indegree that Kahn pass computes is the number seeded onto each
instance row, so detection and scheduling come from one traversal.

### Interaction with the existing knobs

| Knob | Interaction |
|---|---|
| `maxParallel` | Composes with no special handling: ordering decides which instances are *ready*, `maxParallel` decides how many ready ones are *in flight*. Deadlock is impossible — readiness derives from terminal siblings, never from free slots. A deep chain simply has fewer ready instances than the cap allows |
| `failurePolicy: fail_fast` | Unchanged: the first failure cancels every sibling that has not started (pending, or claimed/dispatched but with no container yet — an empty `runtime_id`); a sibling whose container is running finishes on its own. Dependents of the failure were pending, so the existing rule already cancels them and the skip cascade is a no-op |
| `failurePolicy: continue` | The skip cascade above is required. Independent instances keep running; the failed instance's transitive dependents resolve `skipped`; group status is `failed` because a sibling exhausted retries. A group may therefore contain succeeded, failed, **and** skipped instances |
| `onEmpty` | Unchanged. Ordering over an empty set is vacuous |
| `rateLimit` | Unchanged. Parking via `RateLimitTask` (`internal/run/store.go:1808`) delays an instance; its dependents wait, which is correct |
| `retries` | Unchanged per instance. A dependent becomes ready only when its dependency reaches terminal success, so retries serialize correctly against ordering |
| `run retry` | `RetryFromFailure` (`internal/run/store.go:4790`) keeps succeeded/cached instances and resets failed ones. Two separate defects to fix, not one: **(a)** its terminal-success set is keyed by task ID (`terminalSuccessIDs[tr.TaskID]`, `internal/run/store.go:4852-4856`) and its `outstanding_predecessors` recount tests `terminalSuccessIDs[edge.FromTaskID]` (`:4899`), so **one** succeeded sibling marks the whole predecessor group satisfied and releases downstream work while another sibling is still being retried — the predecessor group must be satisfied only when **every** live sibling row is a terminal success; **(b)** it must also reset instances that were `skipped` for a failed in-group dependency and re-seed every reset instance's `outstanding_predecessors` to its in-group indegree counted over **non-terminal** dependencies only (0 when its dependencies already succeeded). The group is still not re-expanded |
| `run retry --partition` | Retrying one instance does **not** cascade to dependents that already succeeded. v1 states this as a limitation rather than silently re-running a subtree |

### Three advancement paths, one expansion

An earlier draft of this section claimed expansion could live in exactly one
place — `completeTask` — and that "both lanes" would inherit it. **That was
wrong, and the error was structural rather than cosmetic: Caesium has three DAG
advancement implementations, not two, and the third deliberately bypasses
`completeTask`.**

| Path | Advancement | Terminal write |
|---|---|---|
| Local (`caesium dev`, `CAESIUM_EXECUTION_MODE` unset) | in-memory Kahn loop, `internal/job/job.go:1428-1441,1502-1517` | `CompleteTaskWithResult` → `completeTask(…, enforceClaim=false)` |
| Distributed, SQL advancement | `outstanding_predecessors` decrement in SQL, `internal/run/store.go:2621` | `completeTask` |
| Distributed, **run-owner in-memory** (`CAESIUM_RUN_OWNER_IN_MEMORY=true`, `pkg/env/env.go:303-308`) | `run.RunState`, `internal/run/owner_state.go:65` | `CompleteTaskOwner`, `internal/run/store.go:2905` |

Three advancement implementations, but **four completion routes** reach them, and
the fourth is the one this feature exercises most:

| Route | Reaches |
|---|---|
| `localSink` (`internal/worker/completion_sink.go:70-81`) — the ClaimNext pull path | `completeTask` / `failTask` / **`cacheHitTask`** |
| `ownerSink` → `POST /internal/complete`, run tracked here | `OwnerManager.Complete` → `ApplyCompletion` + `CompleteTaskOwner` |
| the same handler's SQL fallback when `res.Owned == false` (`internal/dispatch/dispatch.go:516,523-580`) | `CompleteTaskClaimed` / `FailTaskClaimed` / **`CacheHitTaskClaimed`** |
| local executor (`internal/job/job.go:1032,1087`) | **`CacheHitTask`** / `CompleteTaskWithResult` |

**A cache hit is a completion, and `cacheHitTask` (`internal/run/store.go:1933`)
is a different function from `completeTask` (`:2621`).** Every in-group operation
therefore needs a home in both. This is not an edge case for this feature — the
entire point of a per-unit `fingerprint` is that prerequisites in an ordered group
*cache-hit*, so "the prerequisite was cached" is the **common** path through an
ordered group, not a rare one.

One correction while mapping this: `CompleteTaskOwner`'s docstring claims
*"Cache-hit completions are not handled here (they remain on the
`CacheHitTaskClaimed` path); the owner routes only succeeded/failed through this."*
**That is stale.** `ValidCompleteStatuses` (`internal/dispatch/dispatch.go:103-107`)
admits `cached`, the owner block (`:486-489`) forwards `req.Status` unfiltered, and
`CompleteTaskOwner` itself writes `"cache_hit": status == TaskStatusCached`. So
cached *does* travel the owner path today. The docstring is dangerous precisely
here: an implementer trusting it would conclude cache hits bypass the owner engine
and would not wire the in-group decrement into `ApplyCompletion` for them —
producing a group that stalls only when a prerequisite cache-hits. Fix the
docstring in the same change that relies on the behavior.

The third path's contract is explicit in its own docstring: `CompleteTaskOwner`
*"only persists terminal rows — it does NOT decrement predecessors, evaluate
trigger rules, or resolve branches in SQL"*. The owner advanced the DAG in memory
first (`OwnerManager.Complete`, `internal/run/owner_manager.go:215` →
`RunState.ApplyCompletion`, `internal/run/owner_state.go:160`), and the SQL write
is a durable record of a decision already made. So an expansion hook placed only
in `completeTask` is **never reached** in owner mode, and a fan-out group would
silently collapse to the single template row: no instances created, siblings
completed as one, and the run finalized early.

Expansion must therefore be wired into the owner engine as well. The good news is
that the seam already exists and has a precedent with exactly the right shape:
**branch selections.** A `type: branch` task's runtime decision travels from the
worker in `CompleteRequest.BranchSelections`
(`internal/dispatch/dispatch.go:137-140`), is resolved to task IDs by
`ResolveBranchSkips` (`internal/run/owner_topology.go:140`), and is handed to
`RunState.ApplyCompletion` as `branchSkipped`. **The partition list is the same
kind of fact — a container's runtime decision that changes the DAG's live shape —
and must travel the same route**: `CompleteRequest` gains the emitted partition
list, `RunState.ApplyCompletion` expands the group in memory, and
`CompleteTaskOwner` persists the N instance rows in its transaction. One
normalization/validation implementation (`pkg/task`), three call sites that
consume it.

`RunState` itself is the harder half, and it is not fan-out-shaped today:

- `tasks`, `indegree`, `outcomes`, `inReady` are all `map[uuid.UUID]` **keyed by
  task ID** (`internal/run/owner_state.go:65-74`), and `ready` is a
  `[]uuid.UUID` of task IDs. N siblings sharing one `task_id` collapse into one
  entry, so the second sibling's completion is a no-op against an
  already-terminal state.
- `total` is fixed at construction from the catalog (`NewRunState`,
  `internal/run/owner_state.go:82-101`) and `IsComplete()` is
  `terminalCount >= total` (`:351`). Fan-out changes the live row count
  **mid-run**, so a completion predicate built on a static task count is
  structurally incompatible with dynamic expansion — it must count live
  instances, and expansion must raise `total` inside the same critical section
  that creates the rows.
- Recovery replays terminal rows by `row.TaskID` (`RecoverRunState`,
  `internal/run/recovery.go:41`, calling `ApplyTerminalRow`,
  `internal/run/owner_state.go:243`). N sibling rows arrive with the same
  `TaskID` and different `terminal_sequence`; the first marks the task terminal
  and the rest hit the `wasTerminal` early-return, so a recovered owner believes
  a partly-finished group is done.
- **The sequence space assumes one terminal transition per task.**
  `ApplyCompletion` returns early for an already-terminal task
  (`internal/run/owner_state.go:163-165`) with `TerminalSequence` left at zero, and
  `OwnerManager.Complete` passes that zero into `CompleteTaskOwner`
  (`internal/run/owner_manager.go:233-235`); `TerminalTaskRunsSince` selects
  `terminal_sequence > ?` (`internal/run/checkpoint_store.go:95-108`), which
  excludes a zero-stamped row from the replay tail. **Today this is benign and
  arguably correct**: with one row per `task_id` the early return only fires on a
  duplicate completion for a row that already carries a good sequence, so
  suppressing it is the intended behavior. Under fan-out the same code path stops
  being duplicate suppression and starts being the *normal* path for siblings
  2…N — each is a distinct terminal transition that needs its own sequence, and
  without one it is invisible to replay while the dense-gap check
  (`internal/run/recovery.go:61,69-75`) reports phantom gaps. Latent today,
  definite under fan-out. Tracked as an observation in #345.
- Checkpoint cadence is driven by `RunState.seq` (`CheckpointWriter.due`,
  `internal/run/checkpoint_writer.go:56-64`), which only advances on the first
  sibling — a wide group advances the counter once and starves checkpointing
  exactly when the run has the most state to lose.
- `requeueRunning` (`internal/run/owner_state.go:304`) re-dispatches by task ID,
  so a failover mid-group re-dispatches one instance for the whole group.
- **`advanceSuccessors` cannot see in-group edges at all.** It walks
  `rs.topo.Adjacency` (`internal/run/owner_state.go:196-230`), which is built from
  `task_edges` (`LoadRunTopology`, `internal/run/owner_topology.go:16`). A
  partition's `dependsOn` is producer-supplied at runtime and is *by construction*
  absent from `task_edges` — that is the whole reason the instance graph needs its
  own cycle check. So seeding in-group indegree in the owner engine without also
  giving it an in-group adjacency to decrement along is a **guaranteed stall**, not
  a race: the dependent's counter is set once and nothing ever lowers it. The
  in-group graph must be a first-class part of `RunState`, walked by
  `advanceSuccessors` alongside the catalog adjacency, and the in-group skip
  cascade must emit into the same `res.Skipped` list `CompleteTaskOwner` already
  persists.
- Recovery compounds it: `RecoverRunState` rehydrates topology from the **catalog**
  (`internal/run/recovery.go:41`), which by definition has no in-group edges. The
  edges must be rebuilt on recovery from the durable instance rows'
  `PartitionDependsOn` column — the rows are the authoritative source, the snapshot
  carries only the counters. Snapshotting the edges as well would duplicate graph
  state in two places and let them disagree after a partial write.

The checkpoint format carries a migration hazard worth calling out on its own:
`runStateSnapshot` (`internal/run/owner_state.go:370`) has **no version field**,
and `models.RunCheckpoint.StateBlob` (`internal/models/run_checkpoint.go:32-35`)
is documented as opaque bytes with no format version column. An instance-keyed
snapshot is a different shape, but JSON unmarshalling of an *old* blob into the
*new* struct succeeds with zero values rather than erroring — so `Restore`
(`:398`) never reaches its corrupt-checkpoint fallback and a recovering owner
silently adopts an empty state. A version discriminator, with unknown/absent
treated as "replay from terminal rows", is a prerequisite of changing the
snapshot at all.

Finally, the owner↔worker **wire protocol** is task-ID keyed: `DispatchRequest`
and `CompleteRequest` (`internal/dispatch/dispatch.go:111-142`) carry `TaskID`
and no instance identity, so a worker finishing instance 7 reports something that
matches all N sibling rows. This is a cross-node compatibility surface, not just
an internal refactor.

The local path's remaining need is smaller but real. Its loop maintains
`adjacency`/`indegree` keyed by **`Task` ID** and decrements them itself rather
than re-reading the store, so it cannot see rows the completion transaction just
created. `CompleteTaskResult` (`internal/run/store.go:1887`, today only
`SkippedTaskIDs`) therefore returns the expansion: the instance `TaskRun` IDs,
their partition keys, their seeded `outstanding_predecessors`, and the in-group
edges.

**None of this is fan-out-specific work, and that is the point.** See
[`## The task-ID identity assumption`](#the-task-id-identity-assumption) below.

### Route completeness (state this once, check every item against it)

Three review rounds surfaced the same shape of defect — fan-out state seeded in
one place with no corresponding inverse in another — so it is worth stating as an
invariant rather than rediscovering per item. A fourth round then found one the
invariant as stated does *not* catch; that case, and why the answer is to
restructure rather than to extend the checklist, is at the end of this section:

> **Every piece of state fan-out introduces must have its seed, its inverse, and
> its recovery defined on *all four* completion routes and *all three* advancement
> implementations. A seed with no inverse is a stall. An inverse present on some
> routes only is a mode-dependent stall, which is strictly worse: it passes CI in
> the default configuration and fails in production under a flag nobody varied.**

The matrix an item must be able to fill in:

| Operation | SQL advancement | Owner in-memory | Local Kahn loop | Cache-hit route | Survives replay? |
|---|---|---|---|---|---|
| in-group indegree **seed** | expansion tx | `ApplyCompletion` | from the row | n/a (seed happens once, at expansion) | rows are durable |
| in-group **decrement** | instance-keyed `UPDATE` | in-group adjacency in `RunState` | in-memory mirror | **both** `cacheHitTask` and `CacheHitTaskClaimed` | counters snapshotted, edges rebuilt from rows |
| in-group **skip cascade** | `failTask` | `res.Skipped` → `CompleteTaskOwner` | mirror | n/a (a cache hit is a success) | skips are terminal rows |
| live-instance **count** (`total`) | live row count | `RunState.total`, raised in the expansion critical section | live row count | unchanged | snapshotted |
| `maxParallel` **in-flight cap** | claim predicate | **owner ready queue** — `dispatchRunInMemory` (`internal/dispatch/loop.go:376`) never consults `PendingTasksForDispatch`, so a SQL-only predicate does not apply | dispatch loop | unchanged | derived from live rows |

The bottom row is the third instance of the same asymmetry and was found by
applying the invariant rather than by review: `maxParallel` enforced only in the
claimer's SQL predicate is silently unenforced whenever
`CAESIUM_RUN_OWNER_IN_MEMORY=true`, because owner-mode readiness comes from
`RunState.ready` and never touches that query. A cap that holds in one mode and
not another is worse than no cap, because the mode that ignores it is the one
running the biggest clusters.

The general rule this expresses: **a fan-out behavior is not implemented until it
is implemented on the route that does not use SQL to advance the DAG**, and not
verified until it is tested in both `CAESIUM_RUN_OWNER_IN_MEMORY` modes.

#### The matrix has a missing axis — and growing it is the wrong fix

A fourth review round found a defect the matrix above does **not** catch, and the
miss is instructive. Recovery replays a post-checkpoint completion through
`ApplyTerminalRow` (`internal/run/owner_state.go:243`), which does not call
`advanceSuccessors` — it **duplicates the successor walk inline** at `:264-282`
over `rs.topo.Adjacency` only. Teaching `advanceSuccessors` about in-group edges
therefore leaves replay walking catalog edges alone: after a takeover the
dependent keeps its pre-completion indegree and the run stalls. Same failure as
the live-path bug, one function over.

The matrix's axes are *operation × completion route*. This bug is not on a
completion route at all — it is on a **traversal site**, and the owner has two of
those for every traversal concern:

| Concern | Live implementation | Replay implementation |
|---|---|---|
| topology build | `LoadRunTopology`, `internal/run/owner_topology.go:16` | `loadReplayRunTopology`, `:72` |
| successor walk + indegree decrement | `advanceSuccessors`, `internal/run/owner_state.go:198` | `ApplyTerminalRow`, `:243` (inline copy) |
| indegree seed | `NewRunState`, `:93` | snapshot `Restore`, `:398` |

The pattern is not confined to the owner. `replayPredecessorRefsTx`
(`internal/run/store.go:603`) forks a parallel replay implementation at **five**
call sites — `predecessorStatusesTx` (`:2489`), `shouldRunTaskTx` (`:2599`),
`PredecessorOutputs` (`:4410` → `predecessorOutputsFromRefsTx` `:4482`),
`PredecessorDescriptorInputs` (`:4529`), and `PredecessorHashes` (`:4629` →
`predecessorHashesFromRefsTx` `:4678`). Every question of the form *"what are this
task's edges?"* has a live answer and a replay answer that must agree.

The tempting fix is a third matrix axis (live | replay). **That is the wrong
call.** A three-dimensional checklist — operation × route × traversal site — is
past the point where an implementer reliably fills it in, and a checklist nobody
completes is worse than no checklist because it launders the omission as
diligence. Four P1s in one family is evidence that the enumeration is not the
control that works here.

The right fix is to **collapse the axis by construction**: extract the shared
traversal so there is exactly one place where an edge class is enumerated and one
place where indegree is decremented, and have both the live and replay paths call
it. Then "did you also update replay?" stops being a question an implementer can
get wrong, because there is no second copy to update. Concretely this is a
prerequisite inside the identity migration (Stream G), *before* in-group edges are
introduced — it makes the fan-out change smaller, not larger.

One caveat that must survive the refactor, because getting it wrong is a
correctness bug rather than a stall: **the two owner traversals differ
deliberately.** The live path allocates a fresh `terminal_sequence` and
transitively marks unsatisfied successors `skipped`; the replay path adopts the
row's stored sequence and deliberately does **not** auto-skip, because every skip
was itself persisted as a terminal row and arrives later in sequence order —
`ApplyTerminalRow`'s own comment says re-deriving them "would double-handle them".
So the centralization must extract the *kernel* they genuinely share — enumerate
successor edges, decrement, test the trigger rule — and leave the
unsatisfied-trigger **policy** as a parameter. Merging the two functions outright
would reintroduce double-handled skips.

### The task-ID identity assumption

One `TaskRun` per `(job_run_id, task_id)` is not a local convention of the run
store — it is an assumption the **entire run-lifecycle layer** is built on, and
fan-out is the first feature to break it. The base design already named the store
write paths as "the honest hard part". That was an undercount. The same root
cause appears in at least four independent subsystems:

| Subsystem | Representative site | What breaks under fan-out |
|---|---|---|
| SQL advancement | `batchDecrementPredecessorsTx`, `internal/run/store.go:2333` (`task_id IN ?`) | one sibling's completion decrements **every** sibling |
| Owner in-memory engine | `RunState`, `internal/run/owner_state.go:65-74`; `IsComplete`, `:351` | siblings collapse to one entry; run finalizes early on a static task count |
| Checkpoint + recovery | `runStateSnapshot` `:370`; `ApplyTerminalRow` `:243`; `RecoverRunState`, `internal/run/recovery.go:41` | unversioned snapshot silently mis-restores; replay collapses N terminal rows into one |
| Retry accounting | `retryFromFailure`, `internal/run/store.go:4852-4856` (`terminalSuccessIDs[tr.TaskID]`) | one succeeded sibling marks the whole group satisfied, releasing downstream while another sibling is still retrying |
| Owner↔worker protocol | `DispatchRequest` / `CompleteRequest`, `internal/dispatch/dispatch.go:111-142` | a completion for instance 7 is indistinguishable from a completion for the group |
| Materialization | `RegisterTasks`, `internal/run/store.go:1121-1132,1164-1175,1183-1189` | it **actively de-duplicates by task ID** (`seenInputTaskIDs`, the `task_id IN ?` existence pluck), so any path registering N instances through it silently keeps one |
| Sibling-fanning writes | `retryTask` `:3256-3265`; `ClaimTaskForDispatch` `:1665-1686`; `RateLimitTask` `:1808-1824` (`status IN (pending, running)`) | retrying one instance wipes every sibling's `output`/`result`; one claim takes every unclaimed sibling. `RateLimitTask` additionally re-pends **running** siblings, orphaning their in-flight containers rather than killing them — matching `running` may well be deliberate for the one-row case, so this is an open question for G1 to settle, not a presumed bug (observation in #345) |
| Identity on the wire and in the API | `convertRunTaskModel`, `internal/run/store.go:4080-4095` (`TaskRun.ID` is set to `model.TaskID`) | N siblings serialize with an **identical `id`**, so `logs.go:80-90` matches the first and streams the wrong container's logs. Same root cause as the known `/v1/jobs/:id/tasks` serialization bug |
| Cardinality assertions | `stampBatchEventQuarantineTx`, `internal/run/store.go:2414-2433` (`len(taskRows) != len(ids)`) | asserts exactly one row per task id and **hard-errors the whole batched event insert** — a loud failure rather than a silent one, and the only one in this table that surfaces immediately |

The honest consequence for sequencing: **re-keying the run lifecycle onto
`TaskRun` identity is a prerequisite migration in its own right, not a step
inside the fan-out feature.** It touches code fan-out does not otherwise care
about (failover, checkpointing, incident attribution), it must be behavior-neutral
for unfanned runs, and it must land *before* any expansion or ordering work —
otherwise ordering is layered on top of a substrate that cannot represent two
siblings. The execution plan carries it as a dedicated stream with that gating
property, rather than as scattered bullets inside the streams that consume it.

A useful discipline follows from stating it that way: for every code path that
takes `(runID, taskID)` and reads, mutates, counts, or checkpoints task state,
the question is not "does fan-out touch this?" but "**what does this mean when
`(runID, taskID)` names a set rather than a row?**" — and there are exactly three
valid answers: re-key it to the `TaskRun` primary key (per-instance state),
aggregate it explicitly over the set (group status, fan-in), or assert
`partition_count = 0` and fail loudly (paths that genuinely cannot support
groups, such as quarantined replay). Silently matching the first row is never one
of them.

Two sites deserve naming because "aggregate" is the right answer and the *current*
behavior is a silent change of meaning, not an obvious break:

- `predecessorStatusesTx` (`internal/run/store.go:2488-2537`) already selects
  `task_id IN ?` and returns **one status per row**. `satisfiesTriggerRule`
  (`:2540`) reads that slice as one status per *predecessor*. So the moment a
  predecessor is a group, `one_success` starts passing when any single instance
  succeeded and `all_success` starts failing when any instance was skipped —
  the trigger rule quietly changes semantics with no code change and no error.
  This design's contract is that a fanned predecessor presents **one aggregate
  status**; that aggregation has to be written, not inherited.
- `PredecessorHashes` (`internal/run/store.go:4628-4675`) likewise returns N
  hashes where downstream expects one, changing the *shape* of the
  `pred_hash:` lines in the identity key and cache-missing every downstream task
  forever. A fanned predecessor must contribute a single deterministic aggregate
  identity, for the same reason its outputs aggregate.

### `CAESIUM_PARTITION_JSON`

Each instance receives, alongside `CAESIUM_PARTITION=<key>`:

```
CAESIUM_PARTITION_JSON={"dependsOn":["dim_customer"],"fingerprint":"sha256:cd…","key":"fct_orders","materialization":"incremental"}
```

The **normalized** object — every field the producer emitted, key-sorted and
canonically encoded, nothing removed (normalization is lossless; an invalid
attribute failed the producer long before this point) — so what the container
reads is exactly what was hashed and what was persisted.
It is injected at the same merge point as `CAESIUM_RUN_ID` / `CAESIUM_JOB_ALIAS`
(`internal/job/job.go:800-810`, from `buildParamEnv` at `internal/job/job.go:326`;
the distributed analogue in `internal/worker/runtime_executor.go`), which means
it is **excluded from the hashed `mergedEnv`** (`internal/job/job.go:956-962`,
`internal/worker/runtime_executor.go:214-222`). That exclusion is required, not
incidental: `dependsOn` lives inside this JSON, and hashing the env var would
drag a scheduling instruction into the cache key through the back door. The
hashable content of the object is folded in explicitly and visibly as the three
`HashInput` fields above — the same "fold it explicitly, never smuggle it through
`Env`" contract the base design states for the partition value.

The name is fixed and is **not** derived from `fanOut.env`. `fanOut.env` renames
the scalar value var to fit an existing container contract; the JSON var is a new
Caesium-namespaced fact with no such history.

### Persistence

Beyond the base design's partition columns, the instance `TaskRun` row carries
`PartitionFingerprint string`, `PartitionAttributes datatypes.JSON`, and
`PartitionDependsOn datatypes.JSON` (the resolved sibling keys). The `dependsOn`
column is what makes the sibling decrement and the skip cascade single indexed
queries, and what lets the REST partition list and the UI render the in-group
graph without re-deriving it from the producer. The producer's row keeps the full
normalized list in `Partitions` for `why`, run-diff, and replay.

`PartitionIndex` remains **emission order, not topological order** — instance 0
is the rewritten template row. Run-diff continues to align instances across runs
by partition *value*, never by index.

### What this amendment does not change

- **No new YAML field.** `fanOut:` is untouched — `from`, `env`, `maxPartitions`,
  `maxParallel`, `onEmpty`, `failurePolicy`. Structure is a property of what the
  producer emits, not of what the consumer declares, so `pkg/jobdef` and the
  generated [`job-schema-reference.md`](job-schema-reference.md) gain nothing
  from this amendment beyond what the base design already required.
- **No new env var / operator dial.** `CAESIUM_FANOUT_MAX_PARTITIONS` remains the
  only one.
- **No new metric series.** A group's ordering depth is recorded on the
  producer's row and its execution descriptor, not as a new collector.
- **No `CacheVersion` bump**, per the backward-compatibility rule above.
- **Non-goals stand.** No chained fan-out (a partition still cannot itself fan
  out), no fan-out of `branch` steps, no cross-partition communication, and
  Caesium still moves partition *labels and their digests*, never partition data.

## Group Output Contracts (`groupOutputSchema`)

> Amendment, 2026-08-29, resolving [Open Question 4](#open-questions) and issue
> #357. The `file:line` anchors **in this section** were re-read against
> `master` on that date; anchors in the sections above the Structured Partitions
> amendment were captured in 2026-07 and have drifted.

### The gap, precisely

A fanned step's `outputSchema` is validated **per instance**, on that instance's
own row, before it is marked terminal — on the *execution* path; a cache hit
skips it entirely (see below) — via `ValidateTaskOutputSchemaInstance`
(`internal/run/store_instance.go:358`), called from the local group runner
(`internal/job/job.go:1501`) and from the worker
(`internal/worker/runtime_executor.go:836`, inside `executeTask`). That is
correct and it stays.

The **fold** is a different value with a different shape, and nothing validates
it. `AggregateFanInOutputs` (`pkg/task/output.go:660`) turns N per-instance
output maps into one map in which every user key holds a JSON object keyed by
partition value, plus three synthetic counters. Its only gate is a size cap:
over `MaxOutputBytes` it returns `*FanInAggregateTooLargeError`
(`pkg/task/output.go:631`) and no map at all. Under the cap it is published
verbatim to every consumer as `CAESIUM_OUTPUT_<STEP>_<KEY>` (`BuildOutputEnv`,
`pkg/task/output.go:589`).

Validating that fold against the per-instance `outputSchema` would reject every
correct group: a schema saying `row_count: {type: integer}` describes `"42"`,
while the aggregate holds `{"a":"42","b":"17"}`. The fold needs its own
vocabulary, or it gets no contract at all — which is today's state.

#### What per-instance validation already catches — and where it stops

Be precise about the residual gap, because per-instance validation covers more
of it than "the aggregate is unvalidated" suggests. There is **no**
empty-output short-circuit on the validation path: `ValidateTaskOutputSchemaInstance`
returns early only for an absent schema or a disabled mode
(`internal/run/store_instance.go:364-366`), and `ValidateOutput` only for an
empty schema (`pkg/task/schema.go:27`). An instance that emitted nothing is
validated as the empty object `{}`, so a `required` key is violated exactly as
it is for a partial map (`pkg/task/schema_test.go:40-51` pins the partial case).
In `fail` mode that instance therefore **fails on its own**, the group fails,
and an `all_success` consumer skips (an `all_done` consumer still runs — the
distinction this doc insists on below). A silent partition under `required` +
`fail` is already caught, and a group schema is not what catches it.

What per-instance validation structurally cannot see is a fold that is wrong
while every instance is individually right:

1. **Cache-hit instances are never validated at all.** The two call sites are
   both post-execution — `internal/job/job.go:1501`, after `executeAtom`, and
   `internal/worker/runtime_executor.go:836`, inside `executeTask`. A cache hit
   returns before either (`internal/job/job.go:1461-1478` restores
   `entry.Output` and hands it straight to the group runner). So a cached
   instance replays a stored output past the current schema unchecked — and
   cache hits are the *normal* case for this feature, whose headline retry story
   is "unchanged partitions cache-hit".
2. **A key that is optional per instance but mandatory across the group.** If
   an empty input file legitimately must not fail its own partition, `row_count`
   cannot be `required` — and then nothing anywhere asserts that the other 399
   partitions reported it.
3. **`warn` mode.** Violations are recorded per instance and the group publishes
   regardless; there is no single group verdict and nothing gates the consumer.
4. **The fold's own shape.** The reserved-name collision (vocabulary rule 3
   below), the sparse per-key objects, `PARTITION_COUNT` against `SUCCEEDED`,
   the size cap — all group-level facts no per-instance check can reach.

That residue is what this section is for, and the scenario below is built on
case 2 rather than on a silent partition that `required` already rejects.

### Decision: a separate `groupOutputSchema`, on the fanned step

The group schema is a **new step-level field**, `groupOutputSchema`, sibling to
`outputSchema`, legal **only** on a step that declares `fanOut:`.

Three placements were live, and the two rejected ones are rejected for reasons
worth recording:

- **On the consumer, as an `inputSchema` entry for the fanned predecessor.**
  Rejected. `inputSchema` is not enforced at runtime today at all — it feeds
  `caesium job lint`'s contract summary (`cmd/job/lint.go:492`) and the lineage
  mapper, and nothing else reads it during a run. Hanging the first runtime
  enforcement of `inputSchema` off the fan-in case would silently change what
  that field means for unfanned steps too. Worse, it makes the contract a
  property of whoever happens to consume the group: a group with two consumers
  gets two contracts and a group with none gets no contract, when the thing
  being described is what the *producer* promised to fold.
- **Nested inside `fanOut:`.** Rejected. `fanOut` is scheduling metadata,
  documented and implemented as excluded from the cache identity hash
  (`pkg/jobdef/definition.go:593-595`). A data contract is not scheduling
  metadata, and burying it there would put the two schemas a step declares at
  two different levels — for no gain, since `fanOut`'s presence is already what
  makes the field legal.

`groupOutputSchema` sits next to `outputSchema` and `inputSchema` so the one
place a reader looks for a step's data contracts holds all three, and so
`internal/contract/derive.go`, which walks `step.OutputSchema` today, meets the
group form in the same walk when it needs it. The name says what it validates
(the group), what kind of thing it is (an output schema), and sorts adjacent to
the field it modifies in the generated reference.

Declaring `groupOutputSchema` on a step without `fanOut` is a **lint error**,
not a silent no-op — the posture `datasets.produces[].schemaFrom: output`
already takes when it requires a non-empty `outputSchema`
(`pkg/jobdef/definition.go:1085`).

### YAML surface

```yaml
  - name: process-file
    image: ingest-tools:1.4
    dependsOn: [list-files]
    next: [publish]
    fanOut: { from: list-files, maxPartitions: 500 }
    outputSchema:                    # per INSTANCE — unchanged meaning
      type: object
      required: [row_count]
      properties:
        row_count: {type: integer}
    groupOutputSchema: derived       # scalar form: derive from outputSchema
```

or, spelled out:

```yaml
    groupOutputSchema:               # object form: JSON Schema over the fold
      type: object
      required: [row_count, PARTITION_COUNT]
      properties:
        row_count:
          type: object
          additionalProperties: {type: string}
          x-caesium-coverage: all    # every succeeded partition contributed it
        PARTITION_COUNT: {type: integer, minimum: 1}
```

Scalar-or-object is an existing idiom in this schema, not a new one: `cache` is
`interface{}` for exactly this reason (`pkg/jobdef/definition.go:604`), and
`datasets.consumes` accepts a bare name or an object. `derived` is the only
legal scalar value; any other string is a lint error rather than a schema that
silently matches nothing.

The field is per-step, so a job may fan out twice with different contracts.
`metadata.schemaValidation` stays the single tri-state dial
(`pkg/jobdef/definition.go:118`) — there is no `fanOut`-local override, for the
same reason the base design gives fan-out no local cache dial.

### The aggregate's shape vocabulary

The fold is a `map[string]string`, exactly like every other output map, so it
validates through machinery that already exists: `ValidateOutput`
(`pkg/task/schema.go:26`) compiles the declared schema and coerces each string
value using the declared property `type` before validating, and its `object` and
`array` cases already `json.Unmarshal` the string (`coerceValue`,
`pkg/task/schema.go:82`). **No new schema dialect, no second validator, no
change to `pkg/task/schema.go`.** The vocabulary is a set of rules about what
the fold puts in that map:

| Aggregate key | Value in the map | Declare it as |
|---|---|---|
| any user output key `K` that any instance emitted | JSON object `{<partition key>: <value>}` | `type: object`, `additionalProperties: {type: string}` |
| `PARTITION_COUNT` | decimal string | `type: integer` |
| `SUCCEEDED` | decimal string | `type: integer` |
| `FAILED` | decimal string | `type: integer` |

Four properties of the fold are load-bearing and easy to get wrong, so they are
stated as vocabulary rather than left for each author to rediscover:

1. **Every value inside a per-key object is a string, whatever the per-instance
   schema said.** `AggregateFanInOutputs` JSON-encodes the per-instance
   `map[string]string`; it never consults `outputSchema` and never coerces. A
   per-instance `row_count: {type: integer}` becomes `{"a":"42"}` — an object of
   *strings*. Writing `additionalProperties: {type: integer}` in the group
   schema is the likeliest authoring mistake, and it fails every run.
2. **Per-key objects are sparse.** An instance that emitted no `K` contributes
   no entry for `K`. Partition keys are runtime values, so `required` inside a
   per-key object cannot name them; `minProperties` is the expressible bound,
   and `x-caesium-coverage` (below) is the one actually wanted.
3. **The three counters are reserved names.** A user output key literally named
   `PARTITION_COUNT`, `SUCCEEDED`, or `FAILED` is silently overwritten by the
   counter today — `AggregateFanInOutputs` writes the user keys first and then
   assigns the three counters over them (`pkg/task/output.go:691-695`). The
   group vocabulary reserves those three names: a fanned step whose
   `outputSchema` declares any of them is a **lint error**, so the collision is
   caught at apply time instead of becoming a wrong number in a downstream env
   var.
4. **Within a validated group, `FAILED` is always `0`.** A group with any failed
   instance has already resolved `failed` and is not validated (see *Partial
   failure*, below). `FAILED: {maximum: 0}` is therefore vacuous, and a group
   schema is **not** the place to express a failure budget — that is Open
   Question 1 (`minSuccessRatio`), and it changes group *status*, which is
   scheduling, not shape. `SUCCEEDED` and `PARTITION_COUNT` are the useful two,
   and the gap between them is the point: `SUCCEEDED` counts instances that ran
   green, `PARTITION_COUNT` counts partitions that contributed at least one
   output key, and `PARTITION_COUNT < SUCCEEDED` is a run in which some
   partition did its work and emitted nothing.

### `derived`: the group schema you should not have to write

`groupOutputSchema: derived` builds the group schema mechanically from the
step's `outputSchema`, because the fold is mechanical. The rule is exactly:

- for each `k` in `outputSchema.properties` → a property `k` of
  `{type: object, additionalProperties: {type: string}}`;
- for each `k` in `outputSchema.required` → that property additionally carries
  `x-caesium-coverage: all`, and `k` joins the group schema's `required`;
- `PARTITION_COUNT`, `SUCCEEDED`, `FAILED` → `{type: integer}`, all three in
  `required`;
- `additionalProperties` is **never** set to `false` at the group level. The
  per-instance schema's own posture governs which keys an instance may emit;
  restating it here would fail a group for a key the instance schema allowed.

Derivation reads only top-level `properties` and `required`. A step whose
`outputSchema` expresses itself through `$ref`, `allOf`/`anyOf`/`oneOf`, or
`patternProperties` has no derivable key set, and `derived` is then a **lint
error naming the construct** — the same honest "cannot prove it, so say so"
verdict [`design-contract-enforcement.md`](design-contract-enforcement.md) uses
for schema constructs outside its subset. Silently deriving an empty schema
would be a contract that passes everything.

`derived` is expanded at **apply time**, in `pkg/jobdef`, and the expansion is
what persists on `models.Task`. A run therefore validates against a concrete
schema, `caesium job diff` shows the real shape, and editing `outputSchema`
re-derives the group schema in the same apply — the two cannot drift.

### `x-caesium-coverage`: the assertion JSON Schema cannot make

The assertion a fan-in author actually wants is *"every partition that succeeded
contributed this key"*. JSON Schema cannot express it: it compares no two
properties, and `SUCCEEDED` is not knowable at authoring time. `minProperties`
is the closest available and it is a constant — wrong the day the partition
count changes, which is the day fan-out exists for.

So the vocabulary adds exactly **one** extension keyword, on a per-key property:

```yaml
        row_count:
          type: object
          additionalProperties: {type: string}
          x-caesium-coverage: all     # or: any (default)
```

- `all` — every terminal-success instance in the group contributed this key. A
  missing entry is a violation whose `Key` is the output key and whose message
  names the offending partitions (capped, with a count).
- `any` (the default, and the behavior when the keyword is absent) — at least
  one entry, i.e. the key exists in the fold at all.

Caesium **strips `x-caesium-*` keywords before compiling** the schema and
evaluates them itself against the instance rows, rather than relying on the
compiler ignoring unknown keywords. Coverage is a fact about rows, not about the
JSON document, so it could not be a compiled keyword in any case; stripping also
keeps `santhosh-tekuri/jsonschema/v6` fed a clean document, and keeps a future
strict-vocabulary mode from turning an annotation into a compile error.

One extension keyword is the whole extension surface. Anything else a group
wants to say is either ordinary JSON Schema over the table above, or it is a
status question and belongs in `fanOut:`.

### When it runs — one fold, three advancement paths

**Decision: the fold is computed once per group resolution, and validated there
— before any successor is released.** Not on the consumer's read path.

That is not a free choice, because today the lanes disagree about when the
aggregate even exists:

- **Local Kahn executor**: the group runner folds once when the last instance
  lands (`internal/job/job.go:1935`) and stashes the result in `taskOutputs`.
- **Distributed / owner**: there is no fold at resolution.
  `predecessorGroupOutput` (`internal/run/store.go:5509`) recomputes the
  aggregate **lazily, once per consumer**, from the sibling rows, reached
  through `PredecessorOutputs` (`internal/run/store.go:5438`) and its descriptor
  and hash twins.

Lazy recomputation cannot carry a contract. A group with no consumer would never
be validated; a group with three consumers would be validated three times; and —
the sharp one — a per-partition retry after a consumer has already started
changes `SUCCEEDED`/`FAILED` and the key set, so the aggregate a second consumer
sees is not the one the first consumer saw or the one that was validated. The
same asymmetry already bites the size cap: an over-cap group fails the *run* at
fold time locally and fails the *consumer* lazily in the distributed lane, so a
distributed group with no consumer exceeds `MaxOutputBytes` in silence.

So the enabling change, a prerequisite of this section rather than a side effect
of it: **the fold is persisted at group resolution** on the group's anchor row
(a `FanInAggregate datatypes.JSON` column on `TaskRun`), and
`predecessorGroupOutput` reads it, recomputing only for group rows written
before this lands.

The anchor row is the group's **lowest `partition_index`, ties broken by `id`**.
That is the tuple `lockGroupForTerminalDecisionTx` uses, but only in its
PostgreSQL branch (`internal/run/fanout.go:763-766`); dqlite and SQLite return
an empty statement and select no row, and neither the local Kahn lane nor the
owner engine calls the function at all. So this is a **rule each of the three
lanes must implement**, not a row some existing code already hands them.

**The freeze is write-always, not write-if-absent.** A group can resolve more
than once: `caesium run retry <run-id> --task <fanned> --partition <value>` is a
shipped surface (`cmd/run/retry.go:108-111`) that re-runs one instance, after
which the group becomes fully terminal again and every fold point below fires
again. Under write-if-absent the persisted aggregate would then still carry the
pre-retry `FAILED` count and key set while the rows say otherwise — the exact
staleness the freeze exists to remove, made durable instead of transient. So the
`FanInAggregate` and `GroupContractFailed` columns are **rewritten on every
group re-resolution**, and group validation runs **once per resolution, not once
per run**: the retried group is re-folded and re-validated against the same
declared schema, and a group that failed its contract before the retry can pass
after it (or the reverse). `GroupContractFailed` must therefore be *cleared*, not
merely re-evaluated, when an instance is reset. The *moment* is the one
`retryResetColumns` (`internal/run/store.go:5835-5860`) already handles — it
clears `schema_violations` to `nil` on exactly this path — but not the same
*row*: that map is applied to the reset instance's own row by id
(`internal/run/store.go:4206,4211`, `:6169`, `internal/run/store_instance.go:63,138`),
while the frozen fold and the bit live on the group's **anchor** row, a
different row whenever the retried partition is not the lowest-index one — the
common case for `run retry --partition <value>`. So the anchor's
`FanInAggregate`, `GroupContractFailed`, and group-scoped violations are cleared
by a **separate update on the anchor row**, in the same transaction as the
instance reset. Doing it in one transaction matters: `predecessorGroupOutput`
has no terminality gate either, so a consumer reading in a reset →
re-resolution window would otherwise be served the pre-retry fold.

Group resolution is a real, already-identified moment in all three advancement
paths — the route-completeness rule in [`## Route
completeness`](#route-completeness-state-this-once-check-every-item-against-it)
applies to this item like every other:

1. **Local Kahn executor** — the group runner, at the existing
   `AggregateFanInOutputs` call (`internal/job/job.go:1935`), before
   `taskOutputs[taskID]` is published to successors.
2. **Distributed SQL advancement** — inside `completeTask`, in the
   `isFanOutInstance` gate that returns early until `groupAllTerminalTx` is true
   (`internal/run/store.go:3530-3542`), and its cache-hit twin
   (`internal/run/store.go:2403-2414`); after `allTerm`, before
   `batchDecrementPredecessorsTx` releases the cross-step successors. Exactly
   one transaction observes the complete terminal set — that is what
   `lockGroupForTerminalDecisionTx` buys on PostgreSQL and what Raft
   serialization gives on dqlite — so the fold is computed and validated exactly
   once per resolution even under concurrent sibling completions.
3. **Run-owner in-memory engine** — in `OwnerManager.CompleteInstance`
   (`internal/run/owner_manager.go:314`), **between `ApplyCompletion` returning
   (`:416`) and `CompleteTaskOwner` (`:439`)**, with the aggregate passed into
   that call so it is written in the same transaction that stamps the last
   instance's terminal row.

   That seam is where the two halves of the fold actually meet, and neither half
   can move to the other:

   - **Detection lives in `RunState`.** It already knows a group resolved —
     `groupResolved` (`internal/run/owner_state.go:1006`) — so
     `ApplyCompletion` reports "this completion resolved group G" through
     `CompletionResult` (`:59-65`), which today carries
     `{TerminalSequence, Ready, Skipped, Complete, Applied}` and gains that
     signal.
   - **The data does not.** `RunState` (`:102-141`) holds topology, statuses,
     and the fan-out maps but **no outputs** — `OwnerTaskState` (`:18-31`) is
     `{Status, Attempt, ClaimedBy, LeaseExpiresAtMs, Started}`, and the string
     `output` does not appear in `owner_state.go` at all. `ApplyCompletion`
     (`:269`) is not passed the completing instance's output either, and
     `RunState` holds no store handle, so it cannot read the siblings'. Folding
     *inside* `ApplyCompletion` is therefore not merely awkward — it is not
     expressible.
   - **The manager has both.** `CompleteInstance` holds the completing
     instance's `output` as a parameter, and it already reads this exact group's
     rows a few lines earlier — `m.store.TaskRunsForTask(runID, catalogID)`
     (`:406-407`, guarded by `staged.CatalogTaskID(identity)`). The fold is
     those sibling rows plus the in-hand output.
   - **`CompleteTaskOwner` has no group logic to hook into.** Unlike
     `completeTask`, `Store.CompleteTaskOwner` (`internal/run/store.go:3688-3891`)
     contains no `isFanOutInstance`, `groupAllTerminalTx`, or
     `decrementInGroupDependentsTx` — in this lane the group decision lives
     entirely in the owner's memory. So the aggregate is handed to it as an
     argument rather than derived inside it.

   **This is still "before release."** Publication is `or.state = staged`
   (`:449`), not `advanceSuccessors`: the latter runs on `staged`, a
   `Clone()` taken at `:345`, so nothing a dispatcher can observe changes until
   the assignment. Every safety property this section wants is satisfied by
   folding anywhere before `:439`, and the seam above is the last point that
   still rides the terminal-row write.

   **Not `TakeResolvedGroups`**, despite its name. By the time it runs
   (`internal/run/owner_manager.go:465`) the DAG has already advanced:
   `ApplyCompletion` made the successors ready (`:416`), `CompleteTaskOwner`
   committed the terminal rows (`:439`), and `or.state = staged` published the
   transition (`:449`). Worse, `TakeResolvedGroups` only *reports* — its result
   is consumed after `or.mu.Unlock()` (`:466`), so a dispatcher taking the lock
   in between can start a successor before the fold has been validated at all.
   Freezing there would also open a crash window in which a group is durably
   terminal with no persisted aggregate. `TakeResolvedGroups` is the right shape
   for `caesium_fanout_group_duration_seconds`, which gates nothing; it is the
   wrong shape for a gate. Riding the terminal-row transaction gives the owner
   lane the same all-or-nothing property the SQL lane gets from `completeTask`.

Ordering inside the fold point is fixed: **cap → coverage → compiled schema →
publish**. Coverage before the compiled schema so a missing key is reported as
"partition `2026-07-04` contributed no `row_count`" rather than as a
`minProperties` failure that names no partition.

### `warn` and `fail` for a group

`metadata.schemaValidation` governs the group exactly as it governs an instance;
there is no second dial.

- **`warn`** — violations are persisted on the anchor row with `scope: "group"`
  — a **new optional field** on `pkg/task.SchemaViolation`, which is `{Key,
  Message}` today (`pkg/task/schema.go:14-17`) and is persisted as
  `models.TaskRun.SchemaViolations` (`internal/models/run.go:131`) and surfaced
  to REST as `schema_violations`. Violations written before the field exists
  carry no `scope` and are read as instance-scoped —
  a `schema_violation_recorded` event is published so the leader-gated incident
  subscriber opens a `schema_violation` incident (the reason
  `publishSchemaViolationEvent` exists in warn mode at all,
  `internal/run/schema_validation.go`), the group's status is unchanged, and
  consumers receive the aggregate.
- **`fail`** — the group resolves **`failed`** *and its aggregate is suppressed*:
  successors are released to their trigger rules with a failed predecessor, so
  an `all_success` consumer skips and an `all_done` consumer runs with no
  `CAESIUM_OUTPUT_<GROUP>_*` variables at all.

**Suppressing the aggregate is new behavior, and it is deliberately narrow.**
It is *not* "the shape a failed predecessor already has" — for a fanned
predecessor that shape is the opposite. `predecessorGroupOutput`
(`internal/run/store.go:5509-5551`) has **no group-status gate**: it counts
terminal successes, counts failures, and folds whatever outputs exist. A group
with one failed instance therefore publishes its *partial* aggregate today, and
`internal/run/store_fanout_test.go:135-172` pins exactly that — three
partitions, one failed, and `PredecessorOutputs` still returns
`{"east":"10","west":"20"}` with `FAILED=1` to the consumer. (The test calls
`PredecessorOutputs` directly and evaluates no trigger rule — its fixture step is
`TriggerRuleAllSuccess` (`:139`). An `all_done` consumer is the one that would
actually run and read that aggregate in production.)

So the two failure kinds are decided separately and must not be conflated:

- **Instance-failed group** (one or more partitions failed): **unchanged.** It
  keeps publishing its partial aggregate with `FAILED > 0`, exactly as today.
  Backward compatibility is the whole reason: an `all_done` consumer that reads
  a partially-successful group's outputs is an existing, working pattern, and
  this design has no business breaking it. Such a group is also never
  group-validated (see *Partial failure*, below), so there is no verdict to act
  on.
- **Contract-failed group** (every instance succeeded; the *fold* violated the
  declared group schema): the aggregate is suppressed. There is no honest
  partial to publish — the object a consumer would read is precisely the one
  declared invalid, and publishing it while calling the group failed would be
  the silent contract change this whole section exists to prevent.

Two consequences are decisions, not details.

**No instance row is marked failed, and nothing is retried.** The instances
honored their own contracts; what broke is the group's. Failing the anchor
instance to express a group verdict was the tempting shortcut, and it is
rejected twice over: it reports a partition as failed for a reason that has
nothing to do with that partition, and — because the counters are derived from
row statuses — it would mutate `SUCCEEDED`/`FAILED` and drop that partition's
outputs from the very aggregate an `all_done` consumer then reads. Retrying is
equally pointless: the fold is a pure function of terminal instance outputs, so
re-running a container that already succeeded cannot change the verdict. The
operator's fix is a definition change, or a producer fix plus a full re-run, and
`caesium run retry` correctly finds no failed instance to reset.

**Group status is derived from rows today, so the verdict needs one bit.** A
`GroupContractFailed` flag on the anchor row. Inventing a stored group row to
hold one boolean would be a much larger change for the same effect — but the
bit is worthless if a route forgets to read it, so enumerate the routes the way
[`### Route completeness`](#route-completeness-state-this-once-check-every-item-against-it)
demands. There are **two** kinds of reader:

*Status derivation* — two functions, six non-test call sites. The bit is read
once inside each function body and every caller inherits it:

| Function | Defined | Called from |
|---|---|---|
| `groupStatusFromInstances` | `internal/run/taskrun_identity.go:99` | `internal/run/why.go:436`, `internal/run/owner_state.go:1038`, `internal/run/store.go:3208`, `internal/run/store.go:5131` |
| `predecessorGroupSatisfied` | `internal/run/taskrun_identity.go:144` | `internal/run/fanout.go:715` (`groupAllSucceededTx`), `internal/run/store.go:5875` (`satisfiedPredecessorTaskIDsTx`) |

plus the local Kahn runner's own group outcome, which derives status in memory
rather than through either function.

*Aggregate publication* — the **fourth site**, and the one most easily missed
because it is a read path rather than a status path. Wiring the bit only into
the status functions ships a `fail` mode in which `all_success` consumers
correctly skip while `all_done` consumers still receive
`CAESIUM_OUTPUT_<GROUP>_*` from a fold that was just declared invalid:

- `predecessorGroupOutput` (`internal/run/store.go:5509`) — the single fold
  reader, reached from `PredecessorOutputs` (`:5462`) and from
  `PredecessorDescriptorInputs` (`:5563`, folding at `:5586`), so one gate there
  covers the env-var path and the *outputs* half of the descriptor path.
  Predecessor **hashes** are deliberately unaffected: they are derived from row
  statuses, not from the fold — `predecessorGroupHash(successes)`
  (`internal/run/store.go:5599`) for the descriptor's hash half, and
  `PredecessorHashes` (`:5619`), which never calls `predecessorGroupOutput` at
  all and returns `predecessorHashList(taskRuns)` (`:5641`). A suppressed
  aggregate must not silently re-key a downstream step;
- the local runner's `taskOutputs[taskID]` publication
  (`internal/job/job.go:1935`), the same gate on the in-memory lane.

A contract-failed group publishes no aggregate through any of them. An
instance-failed group publishes exactly what it publishes today.

### Partial failure: a failed group is not schema-validated

**A group that already resolved `failed` or `skipped` is not evaluated against
its group schema.** Under `failurePolicy: continue` the failed instances
contribute no outputs, so the fold is incomplete *by construction*; validating
it would report contract violations caused by the failure rather than by the
contract, bury the real first failure under schema noise in `caesium why`, and
open a `schema_violation` incident on top of the task failure that already
opened one. The group's status already carries the bad news. `onEmpty: skip`
never folds at all.

This is also what makes rule 4 of the vocabulary true (`FAILED` is always `0` in
a validated group), and what keeps the feature honest: a group schema asserts
the shape of a *successful* fold. It is not a failure budget and not a
substitute for `failurePolicy`.

### The size cap comes first

`MaxOutputBytes` (64 KB, `pkg/task/output.go:48`) is a hard limit, not a
contract. An over-cap fold fails the group **regardless of
`metadata.schemaValidation` and regardless of whether a group schema is
declared**: there is no `warn` for it, and a declared group schema neither
softens nor hardens it. The cap is evaluated first, so a group schema is never
validated against a partial aggregate.

The group schema is in fact the strongest argument for keeping
`*FanInAggregateTooLargeError` a hard error rather than the silent
degrade-to-counters it replaced. A truncated aggregate that still carried the
counters would satisfy a group schema requiring `PARTITION_COUNT`, and the
declared contract would then certify a fold whose user keys had all been
dropped. The remedy is unchanged and now has a schema pointing at it:
per-partition payloads belong behind `##caesium::output-ref`, and an aggregate
of bounded references is what that mechanism is for.

### Backward compatibility (group contracts)

- **No `groupOutputSchema` declared ⇒ nothing changes.** No fold-time
  validation, no new violations, no new events, no status change; the aggregate
  is byte-identical to today's. The same omit-when-absent posture the rest of
  this design uses.
- **`outputSchema` keeps exactly its current meaning** — the per-instance
  contract, validated per instance, on the instance's own row. This design does
  **not** validate the aggregate against `outputSchema`, which would reject
  every correct fold.
- **Not hashed, so no `CacheVersion` bump.** `cache.HashInput`
  (`internal/cache/hash.go`) contains no schema field today, and adding one
  would invalidate every instance key in a group the moment an operator
  *tightened* a contract — punishing exactly the change worth making. A contract
  describes what the work produced; it is not an input to the work.
- **The generated reference is generated.** `groupOutputSchema` reaches
  [`job-schema-reference.md`](job-schema-reference.md) by editing
  `internal/jobdef/report/report.go:169-170`, never the doc itself; a guardrail
  test compares the two.

### Contract derivation, lint, and `caesium why`

- **Cross-job contract derivation keeps reading `outputSchema`, never
  `groupOutputSchema`.** `internal/contract/derive.go` resolves
  `datasets.produces[].schemaFrom: output` (`:686`, `:810`) and checks
  `paramMapping` keys against a producer's output schemas (`:1262`, `:1331`).
  Every one of those is a per-unit fact about a dataset row or an event payload
  key. The aggregate is a within-run env-var shape that never crosses a job
  boundary — the contract design says exactly that of `CAESIUM_OUTPUT_*` and
  `inputSchema`. Feeding the group schema into `schemaFrom: output` would tell a
  downstream team that `row_count` is an object of strings, which is false about
  the dataset the fanned step wrote. A `schemaFrom: outputGroup` spelling is a
  **non-goal**: a fanned step produces its dataset per partition.
- **Within-job lint reports it.** `contractSummary` (`cmd/job/lint.go:492`)
  lists a group schema's required keys for the fanned step, marked group-level,
  so `caesium job lint` shows both contracts a fanned step carries.
  Breaking-change derivation over a group schema is within-job and uses the same
  `pkg/jobdef/schemacompat` walk when that lands; it never enters the cross-job
  graph.
- **`caesium why` answers at the group.** `WhyGroup`
  (`internal/run/why.go:131-165`) is already the group-level answer for a fanned
  step, and its `Notes` field exists for exactly this class of qualifier — how
  this group's result was arrived at. A group whose schema was evaluated gains a
  note naming the verdict and the violation count. The violations themselves live
  on the anchor row, so the group note must **name that row's partition value**:
  `why --partition` takes a partition *value*, and an operator has no way to
  guess which one is the lowest-index instance. Naming it (or inlining the
  group-scoped violations into the group form outright) is what makes the
  pointer followable. `Baseline.Kind` stays
  `per_partition` and `Verdict` is untouched: a contract verdict is not a cache
  verdict, and conflating them would make a schema failure read as a cache miss.
- **Receipts** record the frozen aggregate alongside the per-instance
  descriptors, so a reproduction compares the fold that was validated rather
  than re-deriving one from rows that may since have been retried. Because the
  freeze is write-always, a receipt holds the fold of **the group resolution
  that was current when the receipt was recorded** — a later per-partition retry
  re-resolves the group and writes a new fold, and it does not rewrite receipts
  already taken. Two receipts from the same run either side of a partition retry
  legitimately differ, and the group's resolution is what distinguishes them.

### Scenario: the six silent partitions

`process-file` fans out over 400 files and emits `##caesium::output
{"row_count":"…"}` per file. Six files match the day's `find` but contain no
rows, so `process-one.sh` exits 0 having emitted nothing — a real and common
shape, not a contrived one.

Crucially, `row_count` is declared in `properties` but **not** in `required`,
and that is a deliberate authoring choice rather than an oversight: an empty
input file is not a broken partition, and failing it would page someone about a
file that was correctly processed as empty. What is broken is the *day* — and no
single partition is in a position to say so. (Were `row_count` `required`, each
silent instance would fail on its own in `fail` mode, per *What per-instance
validation already catches* above; this scenario is the case that survives.)

*Today.* 400 instances succeed and all 400 satisfy `outputSchema` — the six that
emitted nothing are validated as `{}` against a schema that requires nothing, so
they pass honestly. The fold publishes `row_count` with 394 entries,
`PARTITION_COUNT=394`, `SUCCEEDED=400`, `FAILED=0`. `publish` reads 394 entries
and writes a 394-partition day. The run is green. Note the two counters already
disagree — 394 against 400 — and nothing reads them. The discrepancy surfaces a
week later in a reconciliation report.

*With a group schema and `metadata.schemaValidation: fail`.* Because
`row_count` is optional per instance, `derived` would give it only
`x-caesium-coverage: any`; this step declares the object form and asserts
`x-caesium-coverage: all` on `row_count` — the group requires what the instance
does not. At group resolution — one transaction, after the 400th instance lands,
before `publish` is released — the fold is computed, persisted on the anchor
row, and evaluated. Coverage fails with one violation:
`row_count: 6 of 400 succeeded partitions contributed no value (2026-07-01,
2026-07-04, 2026-07-11, … +3)`. The group resolves `failed` with no instance
marked failed, `publish` (`all_success`) skips, a `cleanup` step (`all_done`)
still runs, and the run fails. `caesium why --task process-file` reports the
group form with the note and the six partition keys.

*Under `warn`.* Identical detection, no status change: `publish` runs on the
394-entry aggregate, the violation is on the anchor row, and a
`schema_violation` incident is open with a row behind it. This is the mode to
adopt a contract in — turn it on, watch a week of runs, then switch to `fail`.

*And the `derived` case is not redundant.* When `row_count` **is** `required`
per instance, `derived`'s `x-caesium-coverage: all` overlaps with per-instance
validation in `fail` mode — deliberately, since the two agree — but it still
covers what that mode cannot: an instance that **cache-hit** was never validated
(neither call site is on the cache-hit path), and in `warn` mode nothing gates
the consumer. `derived` is the cheap way to keep the group honest about
already-required keys; the object form is for keys the instance contract
deliberately leaves optional.

### Prerequisite: `PARTITION_COUNT` has two definitions today

The vocabulary above defines `PARTITION_COUNT` as *the number of partitions that
contributed at least one output key*, which is what `AggregateFanInOutputs`
computes on its main path (`len(byPartition)`, `pkg/task/output.go:693`) and
what both lanes are pinned to agree on.

Its empty-fold branch computes something else: when **no** partition emitted any
output, it returns `succeeded + failed` (`pkg/task/output.go:663`) — the group
size. So a 5-instance group reports `PARTITION_COUNT=1` when one partition
contributed and `PARTITION_COUNT=5` when none did. The counter is non-monotonic
in coverage, and it reads highest in precisely the case a coverage assertion
exists to catch.

Nothing depends on that today, because nothing validates the fold. A group
schema makes it load-bearing: `PARTITION_COUNT: {minimum: N}` and the `all`
coverage rule are only meaningful if the counter means one thing. Reconciling
the empty branch to `0` is therefore a **prerequisite** of the phasing below. It
changes an observable output value for an already-shipped feature, so it wants
its own issue and its own release note, not a silent ride-along with this
design.

### Phasing and testing

This is not a sixth phase of the base design; it is an increment on the shipped
feature, in four individually shippable steps:

1. **Freeze the fold.** `TaskRun.FanInAggregate`, written at group resolution in
   all three advancement paths, read by `predecessorGroupOutput` with a
   recompute fallback for older rows. Reconcile `PARTITION_COUNT`'s empty
   branch. Behavior-neutral for every existing job, and the step that removes
   the local/distributed asymmetry in the existing over-cap failure.
2. **Declare.** `groupOutputSchema` on `Step` — the exported struct **and both**
   inner `rawStep` declarations, in `UnmarshalYAML`
   (`pkg/jobdef/definition.go:609`) *and* in `UnmarshalJSON` (`:690`), each with
   its own field entry and its own copy-out. Missing the JSON one is not a
   cosmetic omission: the field would work through `caesium job apply` from YAML
   and vanish silently on the JSON/REST apply path. Then `derived` expansion at
   apply time, the lint rules (requires `fanOut`; reserved counter names;
   underivable `outputSchema`) wired into **both** lint surfaces (see phase 4),
   the `report.go` entry, and persistence on `models.Task`.
3. **Enforce.** Coverage evaluation, compiled-schema validation, `warn`/`fail`
   semantics, `GroupContractFailed` at the status-derivation points **and the
   aggregate-publication gate** (the fourth site, and the one the body calls
   easiest to miss), `scope: "group"` on persisted violations.
4. **Surface.** The `caesium why` group note, the group schema in `job lint`'s
   contract summary — in **both** copies of it, `cmd/job/lint.go:492` and its
   server twin `api/rest/controller/jobdef/lint.go:105`, so `--server` lint
   and offline lint do not disagree about a step's contracts — and group
   violations in the REST partition and run payloads.

Testing follows the repo gate — integration scenarios in `test/` driving the
real server and CLI, not unit tests on the fold function:

1. **No group schema declared**: the aggregate, the events, and the consumer's
   env are byte-identical to a pre-change run. The backward-compatibility
   assertion, and it comes first.
2. **`warn`, coverage miss**: 5 partitions, 2 emit no `row_count`, with
   `row_count` **optional** in `outputSchema` so per-instance validation passes
   honestly and the group verdict is the only one under test; the group
   succeeds, the consumer runs, violations with `scope: "group"` are on the
   anchor row, and a `schema_violation_recorded` event is observed.
3. **`fail`, same fixture**: the group is `failed`, the `all_success` consumer
   skipped, an `all_done` consumer ran, **no instance row is failed** (the
   assertion that separates a group verdict from a per-instance one), and
   `caesium run retry` re-runs nothing and leaves the run failed.
4. **Coverage sees a cache hit**: a group where some partitions cache-hit and
   one cached output lacks the covered key — per-instance validation never runs
   on that instance (neither call site is on the cache-hit path), and coverage
   still fails the group. This is the case that makes `derived` non-redundant
   for an already-`required` key.
5. **`derived` equivalence**: `derived` and the hand-written schema it expands
   to produce identical verdicts on the same fixture — asserted against the
   persisted expansion, so the derivation rule itself is pinned.
6. **Reserved names**: a fanned step whose `outputSchema` declares `FAILED`
   fails `caesium job lint`, with the key named.
7. **Cap precedence**: an over-cap fold fails the group under
   `schemaValidation: warn` with a satisfied group schema declared.
8. **Partial failure**: a group with one failed instance records **no**
   `scope: "group"` violations, and `why`'s first failure is the failed
   partition rather than a schema note.
9. **All three lanes**: local, distributed
   (`CAESIUM_EXECUTION_MODE=distributed`), and run-owner in-memory
   (`CAESIUM_RUN_OWNER_IN_MEMORY=true`). The owner lane gets its own scenario
   for the reason the base design's test 13 gives — it is the path most likely
   to be forgotten.
10. **CLI**: `caesium why --task <fanned> --json` is clean and parseable on
   stdout via `runCLIStdout` (never the stream-merging capture) and carries the
   group note.

### Open questions carried forward

1. **Consumer-side declaration, later.** If `inputSchema` ever becomes
   runtime-enforced, should a consumer be allowed to declare its own required
   subset of a fanned predecessor's aggregate, the way
   `datasets.consumes[].schema` lets a cross-job consumer state a subset? The
   producer-side contract is the right one to ship first; the consumer-side one
   is only coherent once `inputSchema` does anything at run time.
2. **Where `minSuccessRatio` lives.** Open Question 1's failure budget wants
   `SUCCEEDED`/`FAILED`, which are right here in the fold — but it changes group
   *status*, and this section deliberately keeps the group schema out of status.
   Is `fanOut.minSuccessRatio` (scheduling) plus a group schema that then sees a
   partially-failed fold coherent, or does admitting partial success mean the
   "failed groups are not validated" rule needs a third case?
3. **Is the frozen aggregate worth its own surface?** Once the fold is
   persisted, `caesium run aggregate <run-id> --task <name> --json` is nearly
   free, and it is what an operator debugging a group contract actually wants to
   read. Or are the group form of `why` plus `run partitions` enough, and one
   more command the wrong kind of growth?

## CLI

```sh
caesium run partitions <run-id> --task process-file [--status failed] [--limit N] [--offset N] [--json]
caesium run retry <run-id> --task process-file --partition "2026-07-01"
caesium job lint --path jobs/          # fanOut validation errors
caesium dev --once --path job.yaml     # local fan-out with live group progress
```

`--json` output on stdout, logs on stderr, per the repo's stdout-cleanliness gate.

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
| Partition value (`key`) size | 256 bytes | parse-time |
| Partition list bytes | 256 KB normalized | parse-time |
| Per-partition object bytes | 2 KB normalized | parse-time |
| Attributes per partition object | 16 keys | parse-time |
| `fingerprint` shape | `sha256:<64 hex>` (`validSHA256Ref`) | parse-time |
| In-group `dependsOn` graph | acyclic, no dangling keys | expansion-time; **fails the producer** |
| Group in-flight | `fanOut.maxParallel` | claim predicate / dispatch loop |
| Aggregate output size | `MaxOutputBytes` 64 KB | fan-in; over-cap **fails the group** (typed `*FanInAggregateTooLargeError`), never truncates — see [`### The size cap comes first`](#the-size-cap-comes-first) |
| No chained fan-out | — | lint |

Metrics: new `caesium_fanout_partitions_total{job,task}` and
`caesium_fanout_group_duration_seconds` series (not labels on existing series,
per the observability-isolation lesson in the replay design). Quarantine
propagation to instances is mandatory (`TaskRun.Quarantine`, run.go:82).

## Testing (integration-first)

Per the repo gate, every surface ships with a `test/` integration scenario
driving the real binary/server (no hand-seeded rows):

1. Happy path: producer emits 5 partitions; 5 instances run, each sees
   `CAESIUM_PARTITION`; fan-in runs once with the aggregate env visible.
2. Failure matrix: `fail_fast` cancels not-yet-started siblings; `continue` resolves
   the group failed after all siblings; downstream `all_done` still runs.
3. Retry: fail one partition, `caesium run retry`, assert the 4 unchanged
   instances **cache-hit** (per-partition identity) and only one re-executes.
4. `onEmpty` both modes; caps (1025 partitions fails the producer loudly).
5. Distributed lane (`CAESIUM_EXECUTION_MODE=distributed`): expansion; worker
   crash mid-group → lease reclaim, siblings unaffected; rate-limit parking +
   drain with no over-issue.
6. CLI: `run partitions --json` stdout clean and parseable (`runCLIStdout`,
   never the stream-merging capture); partition retry end-to-end.
7. Playwright: grouped DAG node, partition table, per-partition retry.
8. Structured partitions — backward compatibility: a string-form producer's
   instance hashes are **byte-identical** to the pre-amendment values (asserted
   against a golden digest, not merely "a hash exists").
9. Structured partitions — ordering: a producer emits `a → b → c`; assert
   observed start order in **both** the local and distributed lanes, that
   `maxParallel: 5` never runs `c` before `b` is terminal, and that a failed `a`
   under `failurePolicy: continue` resolves `b`/`c` as **skipped** rather than
   hanging the run to its timeout.
10. Structured partitions — rejection matrix: a `dependsOn` cycle, a dangling
    `dependsOn` key, a malformed `fingerprint`, a conflicting duplicate `key`,
    and an over-cap object each **fail the producing task** with the offending
    key named, leaving no instance rows behind (assert row count, not just the
    error string).
11. Structured partitions — identity: two runs whose producer emits the same set
    with one changed `fingerprint` re-execute exactly that instance under the
    chain-break configuration, and `caesium why` names
    `partition_fingerprint` as the discriminating field.
12. Structured partitions — env: `CAESIUM_PARTITION_JSON` is present, is the
    normalized object, and changing only `dependsOn` between runs does **not**
    change any instance's hash.
13. **Run-owner in-memory lane** (`CAESIUM_RUN_OWNER_IN_MEMORY=true`): the same
    fan-out fixtures run under the owner path, asserting N instances materialize
    (not one), the run does **not** finalize until every instance is terminal,
    and ordering is honoured — the third advancement path is the one most likely
    to be forgotten, so it gets its own lane, not a shared assertion.
14. **Owner failover mid-group:** kill the owner with a group half-complete;
    the recovering owner rebuilds from checkpoint + terminal tail and
    re-dispatches only the *unfinished instances*, never the whole group and
    never a single instance standing in for it. Includes a checkpoint written by
    the old format being rejected (replay-from-rows) rather than silently
    restored as empty state.
15. **Retry does not release downstream early:** a group where one sibling
    succeeded and one failed, retried — assert the downstream step does **not**
    start until the retried sibling is terminal. This is the task-ID-keyed
    `terminalSuccessIDs` defect and it fails silently without an explicit
    ordering assertion.

Group output contracts add ten more scenarios — backward-compat byte-identity,
`warn`/`fail`, coverage over a cache hit, `derived` equivalence, reserved names,
cap precedence, partial-failure non-validation, all three lanes, and the CLI
stdout assertion —
listed with the rest of that work in [`### Phasing and
testing`](#phasing-and-testing).

## Phasing

1. **Substrate:** partition columns + unique index; instance-keyed store write
   paths; marker parsing + caps. (Widest, least visible.)
2. **Local executor:** expansion, group fan-in, env injection, `Partition` in
   `HashInput`, per-instance retries; `dev`/lint support.
3. **Distributed:** expansion in the completion tx, `maxParallel` claim
   predicate, distributed integration lane.
4. **Surfaces:** REST + CLI + UI group rendering; `why`/`run diff` alignment.
5. **Follow-ups:** replay re-expansion (**shipped**, #359 — see
   [`### Receipts, replay, why, run-diff`](#receipts-replay-why-run-diff)),
   value-verified per-partition skip.

Phase 1's scope is wider than "the store's write paths": it is the whole
task-ID-identity migration described in
[`## The task-ID identity assumption`](#the-task-id-identity-assumption) —
SQL advancement, the run-owner in-memory engine, the checkpoint format and its
version discriminator, recovery replay, retry accounting, and the owner↔worker
wire protocol. That migration is behavior-neutral for unfanned runs and must be
complete before any expansion or ordering work, because ordering cannot be
layered on a substrate that cannot represent two siblings.

Structured partitions are not a sixth phase: normalization and caps land with the
marker parsing in phase 1; the fingerprint fields land with `HashInput` in phase
2; expansion-time cycle detection, indegree seeding, the sibling decrement, and
the in-group skip cascade land with expansion in phases 2–3; the ordering and
fingerprint columns surface in phase 4. The one thing that stays a follow-up is
the *benefit* of the fingerprint — per-unit skip needs the orthogonal chain break
described above.

Group output contracts are not a sixth phase either, and unlike structured
partitions they are not yet implemented: they are an increment on the shipped
feature, phased separately in [`### Phasing and
testing`](#phasing-and-testing) (freeze the fold → declare → enforce → surface).
Step 1 of that sequence — persisting the fan-in fold at group resolution — is
behavior-neutral substrate work that stands on its own.

## Non-Goals

- **No shuffle/reshard, no cross-partition communication.** `dependsOn` orders
  instances; it does not connect them. There is no data channel between
  partitions and no per-sibling output plumbing — anything needing exchange
  between partitions is a data-plane framework (Spark/Beam) running *inside* one
  step.
- **No `dependsOn` across groups.** A partition may only depend on a sibling in
  its own group. Cross-group ordering is what step edges are for.
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
4. ~~**Contract granularity.** Does `outputSchema` apply per instance (current
   plan) or additionally to the aggregate, and how does
   [`design-contract-enforcement.md`](design-contract-enforcement.md) count
   per-partition violations?~~ **Resolved (#357)** — see [`## Group Output
   Contracts`](#group-output-contracts-groupoutputschema). `outputSchema` stays
   strictly per instance; the fold gets its own `groupOutputSchema` with its own
   vocabulary, validated once per group resolution. Contract enforcement keeps
   deriving cross-job edges from `outputSchema` only, and per-partition
   violations stay on their own instance rows. Three questions the section could
   not close are listed at its end.
5. **Agent remediation.** Should the
   [`design-agent-in-the-loop.md`](design-agent-in-the-loop.md) action catalog
   gain `retry_partition` as a Tier-1 action (cheap, bounded, obviously safe)?
6. **Freshness interplay.** When
   [`design-freshness-scheduling.md`](design-freshness-scheduling.md) drives
   re-runs, does per-partition staleness justify pulling the deferred
   value-verified per-partition skip forward?
7. **Ordering visibility.** Should the in-group `dependsOn` graph be renderable
   (a mini-DAG in the partition table, an ordering column in
   `run partitions --json`), or is the group deliberately opaque below the node
   level? The data is on the row either way; this is a surface question.
8. **Group-level ordering caps.** Deep chains defeat the point of fan-out — 300
   models in one 300-deep chain is a serial pipeline wearing a fan-out costume.
   Is a maximum in-group depth (or a warning above some ratio of depth to N) a
   guardrail worth its complexity?
