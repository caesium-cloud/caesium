# Design: Dynamic Fan-Out (Data-Proportional Parallelism)

> Status: Shipped — runtime-materialized parallel task instances via `fanOut` and `##caesium::partitions`. Implementation: [`exec-plans/completed/dynamic-fanout.md`](exec-plans/completed/dynamic-fanout.md). Grounded against the executor, run store, claimer, and cache identity code as of 2026-07, amended 2026-08-25 with [`## Structured Partitions`](#structured-partitions-key--fingerprint--dependson) (re-grounded on that date).

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
  (terminal runs only; re-evaluates fan-in on completion).
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
| Aggregate output size | `MaxOutputBytes` 64 KB | fan-in; degrade to counts, fail closed under `inputSchema` |
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
7. **Ordering visibility.** Should the in-group `dependsOn` graph be renderable
   (a mini-DAG in the partition table, an ordering column in
   `run partitions --json`), or is the group deliberately opaque below the node
   level? The data is on the row either way; this is a surface question.
8. **Group-level ordering caps.** Deep chains defeat the point of fan-out — 300
   models in one 300-deep chain is a serial pipeline wearing a fan-out costume.
   Is a maximum in-group depth (or a warning above some ratio of depth to N) a
   guardrail worth its complexity?
