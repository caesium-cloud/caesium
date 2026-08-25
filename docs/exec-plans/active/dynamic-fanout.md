# Dynamic Fan-Out — Data-Proportional Parallelism

Last updated: 2026-08-25

This plan ships **dynamic fan-out**: a producer step emits a partition list on
stdout (`##caesium::partitions [...]`), a downstream step declares `fanOut:` in
YAML, and Caesium materializes **N parallel task instances** — one `TaskRun` row
per partition, each with `CAESIUM_PARTITION=<value>` injected and its own
attempts, claims, cache identity, and rate-limit acquisition. The DAG *shape*
stays static (one catalog `Task` per step, cycle detection unchanged); only the
run-scoped `TaskRun` count is dynamic. Fan-in is group-level: downstream sees the
fanned predecessor as one node with one aggregate status, so existing trigger
rules apply unchanged.

Current state: a fanned workload is either hand-sharded in YAML (boilerplate, a
lie in the DAG), forked inside one container (losing per-unit retries/caching/
observability), or serialized in one long-running pod. Target state: parallelism
scales with runtime-discovered data volume, per-partition cache identity means
`caesium run retry` re-executes only the stragglers, and cluster elasticity for
large N stays Kubernetes/Kueue's problem. The surface area is wide but bounded —
the honest hard part is that the run store today addresses task state as
`WHERE job_run_id = ? AND task_id = ?` everywhere (one `TaskRun` per
`(run, task)` is a load-bearing implicit invariant); fan-out breaks that
invariant and every write path must re-key on the `TaskRun` primary key.

This plan follows the `exec-plan-wave` skill's structural convention:
`## Progress` is a wave-by-wave dashboard, `## Streams` is the work backlog,
`## Sequencing & Dependencies` captures cross-stream order, and
`## Acceptance Criteria` lists the gates that close out the entire plan. Any
agent can:

1. Pick a numbered checklist item from `## Streams` whose dependencies are
   satisfied (per `## Sequencing & Dependencies`).
2. Land it as a self-contained PR.
3. Run the verification block under `## Verification (Run For Every PR)`.
4. Tick the checkbox and update the active wave's per-stream bullet in
   `## Progress`.

For wave orchestration of the streams below, see
[`.claude/skills/exec-plan-wave/`](../../../.claude/skills/exec-plan-wave/).
For drafting new plans in this same shape, see
[`.claude/skills/draft-exec-plan/`](../../../.claude/skills/draft-exec-plan/).

## Source-Of-Truth Note

This plan implements [`docs/design-dynamic-fanout.md`](../../design-dynamic-fanout.md).
**The design doc is authoritative for INTENT and SCOPE and wins on any
disagreement** — what fan-out must do (the marker protocol, the expansion
transaction, per-partition cache identity, the fan-in aggregation contract, the
safety caps, and the explicit Non-Goals) is fixed by the design. No item may add
a new marker form, YAML field, config knob, endpoint, or metric beyond what the
design enumerates without first amending the design. In particular the design's
**Non-Goals bind this plan**: no nested/chained fan-out, no fan-out of `branch`
steps, no cluster autoscaling, no partition-aware backfill coupling, and Caesium
moves partition *labels* (≤256 B) never partition *data*. Two design contracts
are load-bearing and easy to get wrong, so they are called out here: (1) the
`fanOut` config is **scheduling metadata and is deliberately NOT folded into the
cache hash** — only the per-instance partition `key`, its `fingerprint`, and its
scalar attributes enter `cache.HashInput` (the sibling list, and a partition's
`dependsOn`, are scheduling instructions, not data inputs); (2) marker caps
**fail the producing task loudly, never truncate** (a truncated partition list
silently drops data).

The design was **amended 2026-08-25** with
[`## Structured Partitions`](../../design-dynamic-fanout.md#structured-partitions-key--fingerprint--dependson):
a `##caesium::partitions` element may be a JSON object
(`{key, fingerprint, dependsOn, …scalar attributes}`) as well as a bare string.
That amendment is authoritative the same way the rest of the design is, and adds
three binding contracts on top of the two above: (3) **a bare-string array keeps
byte-identical hashes** — every new `HashInput` field is omit-when-empty and there
is **no `CacheVersion` bump**; (4) **the in-group `dependsOn` graph is validated
at expansion time** (cycles and dangling keys fail the producing task, in the same
transaction, before any instance row is inserted) because apply-time cycle
detection cannot see runtime edges; (5) **an in-group dependency failure must skip
its transitive dependents** — leaving them undecremented hangs the run to its
timeout, so the skip cascade is load-bearing, not a nicety. The amendment adds
**no new YAML field, no new env var, and no new metric series**. Strategic
priority/status is tracked in
[`docs/roadmap.md`](../../roadmap.md) Phase 4 design-wave table (the roadmap wins
on priority/status). The job-definition contract lives in
[`pkg/jobdef/definition.go`](../../../pkg/jobdef/definition.go).

## Progress (as of 2026-08-25)

No implementation waves have shipped yet. The plan was published from
[`docs/design-dynamic-fanout.md`](../../design-dynamic-fanout.md) (Status:
Brainstorm/Design) with every item grounded against the executor, run store,
claimer, and cache-identity code as of 2026-07; the first wave is the next
eligible run of the `exec-plan-wave` skill against this doc.

**Amendment, 2026-08-25 — structured partitions.** Because no stream had started,
the design's structured-partition amendment
([`## Structured Partitions`](../../design-dynamic-fanout.md#structured-partitions-key--fingerprint--dependson))
was absorbed into the existing streams rather than bolted on as a follow-up wave:
items A3, C1, C2, D1, E1, E2, E3, F2, H-1, and N-1 were **amended in place**, and
items A4, C4, D3, and E4 are **new**. Nothing moved between streams and no
dependency edge was reversed, so the wave plan below is unchanged in shape. Two
line-reference notes for whoever picks this up: (a) the `file:line` anchors in
items written 2026-07 have drifted as those files grew (`CompleteTaskWithResult`
is now `internal/run/store.go:1904`, `RetryFromFailure` `:4790`,
`RegisterTasks` `:1115`, `StartTask` `:1594`, `RateLimitTask` `:1808`,
`successorEdgesForRunTx` `:2203`, `batchDecrementPredecessorsTx` `:2333`,
`retryTask` `:3256`, `SkipTask` `:3347`; `Step` is `pkg/jobdef/definition.go:541`,
`validateSteps` `:1482`, `computeStepAdjacency` `:1851`, cycle detection `:2044`);
(b) anchors added by this amendment were re-read against `master` on 2026-08-25.
Grep the symbol, do not trust the number.

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Substrate — `TaskRun` partition + fingerprint/ordering columns + unique index, instance-keyed store rewrite, marker parsing (string **and** object form) + caps, server hard cap env | **P0** | Not started |
| B | Schema + lint contract — `FanOut` on `Step`, `validateSteps` rules, `FanOutConfig` on `models.Task`, runtime spec (**unchanged by the structured-partition amendment — no new YAML field**) | **P0** | Not started |
| C | Local executor — expansion, per-partition cache identity (`key` + `fingerprint` + attributes), in-group ordering + skip cascade, group fan-in + output aggregation, metrics | **P0** | Not started |
| D | Distributed lane — expansion + cycle check + indegree seeding in the completion transaction, instance-keyed sibling decrement, `maxParallel` claim predicate, sibling-aware predecessor outputs | P1 | Not started |
| E | Surfaces — REST partition endpoints + `caesium run partitions`/`retry --partition` + ordering-aware retry reset + `why`/`run diff`/replay alignment | P1 | Not started |
| F | UI — grouped DAG node, run-timeline group lane, virtualized partition table with per-row retry and ordering/fingerprint columns | P2 | Not started |
| H-1 | Integration harness — fan-out caps + distributed lane + ordered-group fixtures wired on the live integration server | — | Not started |
| N-1 | Docs — roadmap row, design banner, schema references, marker object form, examples, README | — | Not started |

## Streams

### Stream A — Substrate: partition model + instance-keyed store + marker parsing

The widest, least visible slice (design Phase 1) and the foundation every other
stream builds on, so it merges first. It breaks the one-`TaskRun`-per-`(run,task)`
invariant, re-keys every store write path onto the `TaskRun` primary key, and
teaches `pkg/task` to parse the partition marker.

- [ ] A1. Add the partition columns to `TaskRun`: `PartitionValue string` (empty =
      unfanned), `PartitionIndex int` (default 0), `PartitionCount int` (default 0
      = unfanned), `PartitionFingerprint string` (default `''`),
      `PartitionAttributes datatypes.JSON`, `PartitionDependsOn datatypes.JSON`
      (the resolved sibling keys — what makes the in-group decrement and the skip
      cascade single indexed queries, and what the REST/UI surfaces render), and
      `Partitions datatypes.JSON` on the *producer's* row (the normalized emitted
      list, capped, for observability/`why`/replay). `PartitionIndex` is **emission
      order, never topological order** (instance 0 is the rewritten template row).
      Widen the existing
      composite index `idx_taskrun_jobrun_task` (`internal/models/run.go:47,49`) to
      a **unique** index over `(job_run_id, task_id, partition_index)`. `TaskRun` is
      already a hot-path model (`hotPathModels()` in `pkg/db/db.go:281-284`;
      `hotTables` in `pkg/db/router.go:23`), so AutoMigrate picks the tag changes up
      on every shard — **no `internal/models/models.go` `All`-slice edit** (columns
      on an existing registered model migrate from struct tags), but confirm the
      unique-index migration lands on the sharded hot tables.
      Files: `internal/models/run.go`, `pkg/db/db.go`, `pkg/db/router.go`.
- [ ] A2. Re-key every run-store write path that today addresses task state as
      `WHERE job_run_id = ? AND task_id = ?` onto the `TaskRun` **primary key**
      (both executors already hold it): `StartTask` (`internal/run/store.go:1572`),
      `RateLimitTask` (:1786), `retryTask` (:3224), `SkipTask` (:3315),
      `completeTask` (:2589) / `CompleteTaskWithResult` (:1882) / the cache-hit path,
      and the descriptor updates. This is the mechanical-but-wide refactor the design
      calls "the honest hard part"; land it BEFORE expansion so no path can silently
      update the wrong sibling. No behavior change for unfanned tasks (one instance,
      `partition_index = 0`).
      Files: `internal/run/store.go`.
      Depends on: A1.
- [ ] A3. Parse the partition marker: extend `Markers` (`pkg/task/output.go:364`)
      with `Partitions []Partition` — a normalized element type
      (`Key`, `Fingerprint`, `DependsOn []string`, `Attributes map[string]string`)
      whose string form is `{Key: <value>}` — parsing `##caesium::partitions `
      (JSON array, appended across lines, elements may be strings **or** objects)
      and `##caesium::partition ` (one string value per line, string form only) in
      `parseMarkers` (`pkg/task/output.go:408`), trimmed and deduplicated
      first-seen-order (the `ParseBranches` posture, `pkg/task/output.go:329`).
      Enforce caps at parse time, **failing the producing task on overflow, never
      truncating**: `MaxPartitionListBytes` = **256 KB** over the *normalized*
      encoding (deliberately independent of `MaxOutputBytes`,
      `pkg/task/output.go:45`, which must stay reserved for scalar outputs + the
      fan-in aggregate), `MaxPartitionObjectBytes` = 2 KB normalized, ≤ 16
      attributes per object, a count cap passed in by the executor (effective
      `maxPartitions`), and a per-`key` rule (non-empty, ≤ 256 bytes, valid UTF-8).
      Reuse the existing validators rather than writing new ones: `fingerprint`
      must satisfy `validSHA256Ref` (`pkg/task/output.go:193`) and attribute values
      must satisfy `scalarOutputValue` (`pkg/task/output.go:229`; null/object/array
      are rejected here rather than silently dropped as they are for outputs —
      partitions are the work list, not advisory data). A duplicate `key` with an
      identical payload dedups; a duplicate `key` with a **conflicting** payload
      fails the producer. Add the server hard cap `CAESIUM_FANOUT_MAX_PARTITIONS`
      (default 1024) to the `Environment` struct (`pkg/env/env.go:68`) — the
      **only** new env var in this plan. Fully unit-tested (JSON + line forms,
      string/object/mixed arrays, dedup vs conflicting dedup, each cap boundary,
      each invalid-value class).
      Files: `pkg/task/output.go` (+ `pkg/task/output_test.go`), `pkg/env/env.go`.
- [ ] A4. Add the in-group graph validator next to the parser, so **one**
      implementation serves both execution lanes: a pure function over the
      normalized `[]Partition` that resolves `dependsOn` against the emitted key
      set, rejects a dangling key and a self-reference, runs a **Kahn pass to
      detect cycles**, and returns each element's in-group indegree (the number
      Stream C/D seed onto the instance row) plus the reverse adjacency (dependency
      key → dependent keys, which drives the sibling decrement and the skip
      cascade). Errors name the offending key(s) — a "cycle detected" with no key
      is unactionable against a 300-model project. This exists as its own item
      because it is the amendment's only genuinely new algorithm and because
      apply-time cycle detection (`pkg/jobdef/definition.go:2044`) **cannot** cover
      it: those edges do not exist until a container emits them. Fully unit-tested
      (empty deps, chain, diamond, self-cycle, 2-cycle, long cycle, dangling key,
      1024-node fan of depth 1, deep chain).
      Files: `pkg/task/partition_graph.go` (+ `_test.go`).
      Depends on: A3.

### Stream B — Schema + lint contract (the YAML surface)

The declarative half: the `fanOut:` block on a step, the apply-time lint rules
that keep the static topology sound, and the persistence of the config onto the
catalog `Task`. Owns `pkg/jobdef/definition.go` (a true-conflict file via the dual
`Step`/`rawStep` declaration), so all schema work lands in one stream.

- [ ] B1. Add the `FanOut` struct on `Step` (`pkg/jobdef/definition.go:214`) —
      fields `from`, `env` (default `CAESIUM_PARTITION`), `maxPartitions` (required),
      `maxParallel`, `onEmpty` (`skip`|`fail`), `failurePolicy`
      (`fail_fast`|`continue`) — mirroring the `Kueue` optional-struct pattern:
      declare it in **both** the YAML and JSON `rawStep` blocks (`definition.go:251`,
      `:328`) and copy it through in `UnmarshalYAML` (`:315`, `:382`). Add the lint
      rules in `validateSteps` (`definition.go:797`, using `computeStepAdjacency` at
      `:1166`): `fanOut.from` must name a declared predecessor; `maxPartitions`
      required, `> 0`, `≤` the server hard cap; a `fanOut` step **cannot be
      `type: branch`** and **cannot itself be named in another step's `fanOut.from`**
      (no chained fan-out in v1); `env` must be a valid env-var name outside the
      `CAESIUM_PARAM_*` / `CAESIUM_OUTPUT_*` namespaces, and may not be
      `CAESIUM_PARTITION_JSON` (that name is fixed and is not renameable via
      `fanOut.env`). Update `pkg/jobdef/schema.go`. **The structured-partition
      amendment adds no field here** — structure is a property of what the producer
      emits, not of what the consumer declares — but this item still owns the
      `fanOut` rows in the schema-reference **generator**
      (`internal/jobdef/report/report.go`, alongside the `kueue`/`rateLimit` rows at
      `:159-160`); `docs/job-schema-reference.md` is generated from it and must
      never be hand-edited (`TestGeneratedSchemaReferenceIsCurrent`).
      Files: `pkg/jobdef/definition.go`, `pkg/jobdef/schema.go`,
      `internal/jobdef/report/report.go`.
- [ ] B2. Persist the fan-out config onto the catalog: add
      `FanOutConfig datatypes.JSON` to `models.Task` (`internal/models/task.go:11`,
      the same carry-scheduling-metadata pattern as `RateLimitResource`/
      `RateLimitUnits` at `:28-29`), map the definition's `FanOut` onto it in the
      jobdef→task apply path, and carry it through the runtime spec
      (`internal/jobdef/runtime/spec.go`) so the executor can read a step's fan-out
      config from the run snapshot. **No per-container engine change**: the partition
      value reaches the container as an injected env var through the executor's env
      merge (Streams C/D), not through `internal/atom/{docker,kubernetes,podman}/
      engine.go`, so the three engines are intentionally untouched. **No
      `internal/cache/hash.go` change here**: the `fanOut` config is scheduling
      metadata and must not enter the cache key (only the per-instance value does, in
      C2).
      Files: `internal/models/task.go`, `internal/jobdef/runtime/spec.go`, the
      jobdef apply mapping under `internal/jobdef/`.
      Depends on: B1.

### Stream C — Local executor: expansion, cache identity, fan-in

The local (`caesium dev` / `CAESIUM_EXECUTION_MODE` unset) execution path (design
Phase 2): the in-memory Kahn loop materializes instances, injects the partition
env, folds the partition into the cache hash, and resolves the group. Owns
`internal/job/job.go`; establishes the fan-in aggregation contract in
`pkg/task/output.go` that the distributed lane (D) reuses.

- [ ] C1. Local expansion + env injection: register the fanned step as a single
      template `TaskRun` at run start (`RegisterTasks`, `internal/run/store.go:1093`,
      called from `internal/job/job.go:645`) with normal
      `OutstandingPredecessors` so nothing claims it early. In the in-memory Kahn
      loop (`internal/job/job.go:1280`), queue `TaskRun` identities (not `Task` IDs)
      for fanned steps, keep the fanned step a single node in `adjacency`/`indegree`
      (`job.go:591-634`) with a per-group live-instance counter, and have `runTask`
      (`job.go:896`) inject the partition env (the `fanOut.env` name) **plus the
      fixed `CAESIUM_PARTITION_JSON`** carrying the normalized partition object.
      Inject both at the same merge point as `paramEnv`
      (`internal/job/job.go:800-810`, from `buildParamEnv` at `:326`) so they land
      in the container but stay **out of the hashed `mergedEnv`**
      (`internal/job/job.go:956-962`) — required, not incidental: `dependsOn` lives
      inside that JSON and must not reach the cache key. Fix the run-completion
      accounting: `waitForRunCompletion` (`job.go:661,1571`) and the `len(tasks)`
      count at `job.go:1548` must count **live `TaskRun` rows from the run
      snapshot**, not the static task count. The local loop learns about instance
      rows from the expansion result D1 returns on `CompleteTaskResult`
      (`internal/run/store.go:1887`) — it decrements its own in-memory maps
      (`job.go:1428-1441,1502-1517`) and never re-reads the store mid-run, so
      expansion must hand it the instance IDs, their partition keys, their seeded
      `outstanding_predecessors`, and the in-group edges.
      Files: `internal/job/job.go`.
      Depends on: A1, A2, A3, B2.
- [ ] C2. Per-partition cache identity: add **three** fields to `cache.HashInput`
      (`internal/cache/hash.go:266`), each hashed **only when non-empty** — the
      omit-when-absent pattern of `ResolvedImageDigest` (`hash.go:301-303`), so
      unfanned tasks *and* string-form partitions keep byte-identical keys and **no
      `CacheVersion` bump is needed**:
      `Partition string` → `partition:<key>`;
      `PartitionFingerprint string` → `partition_fingerprint:<value>`;
      `PartitionAttributes map[string]string` → `partition_attr:<k>=<v>` with keys
      sorted (mirror the `Env`/`PredecessorOutputs`/`RunParams` loops at
      `hash.go:307-314,369-384,387-394`). **`dependsOn` is NOT hashed** — it is a
      scheduling instruction like the sibling list, `kueue.queueName`, and
      `rateLimit` (`hash.go:337`); reordering a group must not invalidate unchanged
      work. Mirror all three into `HashInputBlob` (`hash.go:71`) **verbatim**
      (partition data is labels, not credentials) so `caesium why` can name
      `partition_fingerprint` as *the* discriminating field. Fold them into the
      hashed identity **explicitly and visibly**, NOT smuggled through the hashed
      `mergedEnv` (both executors exclude volatile injected env, `job.go:956-962`,
      `internal/worker/runtime_executor.go:214-222`); an instance's identity folds
      its own partition object plus the producer's effective hash via
      `PredecessorHashes` (`job.go:964-971`), never the whole sibling list. Add a
      golden-digest regression test proving a string-form producer's instance hashes
      are byte-identical to the pre-C2 values. Wire per-instance retries (`retryTask`
      keyed by instance row); `RetryFromFailure` (`store.go:4790`) keeps
      succeeded/cached instance rows and does **not** re-expand the group.
      **Explicitly out of scope here:** breaking the predecessor-hash chain. A
      fingerprint discriminates instances but does not stop a changed producer hash
      from invalidating all N (`internal/cache/shortcircuit.go:56` cannot help — one
      changed fingerprint makes the emitted list non-identical and the substitution
      is correctly refused). The chain break is `cache.chain: values`, owned by
      `docs/superpowers/specs/2026-08-25-dag-native-infrastructure-deployment-design.md`
      §4. Do not invent a second mechanism for it in this item.
      Files: `internal/cache/hash.go`, `internal/job/job.go`.
      Depends on: C1.
- [ ] C3. Group fan-in + output aggregation + metrics: resolve group status —
      `succeeded` iff all instances succeeded/cached, `failed` if any instance
      exhausts retries (`fail_fast` cancels pending siblings at first failure;
      `continue` resolves when the last sibling lands), `skipped` if pre-expansion or
      `onEmpty: skip` fired — and decrement each downstream successor **once**, when
      the group resolves. Aggregate outputs in `BuildOutputEnv`
      (`pkg/task/output.go:515`): each scalar output key becomes a JSON object keyed
      by partition value (`..._ROW_COUNT={"a":"42",...}`) plus synthetic
      `..._PARTITION_COUNT`/`_SUCCEEDED`/`_FAILED`, sorted for determinism, counted
      against `MaxOutputBytes` with degrade-to-counts on overflow (a downstream
      `inputSchema` requiring a dropped key fails closed). Handle the `onEmpty: skip`
      pre-expansion path via `propagateSkipped` (`job.go:706`). Add
      `caesium_fanout_partitions_total{job,task}` and
      `caesium_fanout_group_duration_seconds` (new series, **not** labels on existing
      series) to `internal/metrics/metrics.go` (the `var (...)` block at `:22` **and**
      the `Register()` list at `:496`). With ordering in play a group may also
      contain **skipped** instances (C4): `succeeded` still requires every instance
      succeeded/cached, so a group containing an in-group-dependency skip resolves
      `failed`, not `succeeded`. **No new metric series for ordering** — depth is
      recorded on the producer's row and its execution descriptor.
      Files: `internal/job/job.go`, `pkg/task/output.go`, `internal/metrics/metrics.go`.
      Depends on: C1.
- [ ] C4. Mirror in-group ordering in the local executor. The ordering *mechanism*
      is store-side and shared (D3) — local execution reaches it through the same
      `store.CompleteTaskWithResult` call (`internal/job/job.go:1087` →
      `internal/run/store.go:1904` → `completeTask(…, enforceClaim=false)`), so the
      Kahn loop must not reimplement it. What this item owns is the mirror: build the
      instance-level sub-graph from the expansion payload D1/D3 return on
      `CompleteTaskResult`, seed each instance's in-memory indegree from the seeded
      `outstanding_predecessors` on its row (the same read the loop already does at
      `internal/job/job.go:676`), decrement it on sibling completion alongside the
      step-edge decrements (`job.go:1428-1441,1502-1517`), and reflect the store's
      in-group skips into `taskOutcomes`/`terminalTasks` so the terminal-count guard
      (`job.go:1554-1558`) stays accurate. Assert local and distributed produce the
      **same observed order** for the same partition set — a divergence here is the
      failure mode this item exists to prevent.
      Files: `internal/job/job.go`.
      Depends on: A4, C1, D3.

### Stream D — Distributed lane: completion-tx expansion + `maxParallel` claim

The distributed (`CAESIUM_EXECUTION_MODE=distributed`) path (design Phase 3):
expansion rides the producer's completion transaction so distributed workers never
observe a half-expanded group, and the group in-flight cap plugs into the claimer.
Reuses the fan-in + output-aggregation semantics established by Stream C. Shares
`internal/run/store.go` with A2, so it sequences after A.

**Read this before picking up C or D.** The expansion transaction this stream
builds is **not distributed-only** — local execution completes tasks through the
same `store.CompleteTaskWithResult` path (`internal/job/job.go:1087` →
`internal/run/store.go:1904` → `completeTask(…, enforceClaim=false)`). Expansion,
the in-group cycle check, indegree seeding, the sibling decrement, and the skip
cascade therefore live **once**, here, and both executors inherit them; Stream C
owns the local *mirror* of that state, not a second implementation. The practical
consequence for wave planning is that local fan-out is not end-to-end runnable
until D1 merges, even though C's items are written against the local executor.

- [ ] D1. Expand inside the producer's completion transaction: pass
      `markers.Partitions` into `CompleteTaskWithResult` (`internal/run/store.go:1882`)
      alongside output/branches, and in the same tx that walks successor edges
      (`successorEdgesForRunTx`, `store.go:2171`) and calls
      `batchDecrementPredecessorsTx` (`store.go:2298/:2301`): for each successor whose
      `Task.FanOutConfig.from` is this task, apply `onEmpty` to the template when
      `N = 0` (reuse the `SkipTask` path with a "fan-out produced no partitions"
      reason), else rewrite the template as instance 0 (`partition_value` set,
      `partition_count = N`) and insert instances 1…N-1 as copies (same
      `task_id`/image/command/priority/cache-snapshot columns, `Quarantine` copied
      per the distributed-parity rule, each inheriting the template's current
      `outstanding_predecessors`, plus the partition columns from A1 —
      `partition_fingerprint`, `partition_attributes`, `partition_dependsOn`). Run
      A4's validator **before the first insert**, in the same tx: a cycle, a dangling
      `dependsOn` key, or a conflicting duplicate fails the producing task and rolls
      the transaction back, naming the offending keys. Then the normal decrement runs
      (its `task_id IN ?` predicate already matches every sibling), and commit — only
      then are instances visible to the claimer. Extend `CompleteTaskResult`
      (`internal/run/store.go:1887`, today only `SkippedTaskIDs`) with the expansion
      payload the local executor needs (instance `TaskRun` IDs, partition keys, seeded
      `outstanding_predecessors`, in-group edges) — the local Kahn loop maintains its
      own maps and never re-reads the store mid-run, so without this it cannot see
      rows this transaction just created. In `internal/worker/runtime_executor.go`
      inject `CAESIUM_PARTITION` **and** `CAESIUM_PARTITION_JSON` (outside the hashed
      `mergedEnv` at `runtime_executor.go:214-222`, matching C1's local placement),
      and confirm `PredecessorOutputs` (`runtime_executor.go:200,497`) aggregates
      across sibling rows in SQL (same fan-in contract as C3). Where an instance
      carries a fingerprint, `PredecessorHashes` (`internal/run/store.go:4628`, which
      reads `COALESCE(effective_hash, hash)`) must return the same set the local lane
      folds in — the two lanes must not disagree about an instance's identity.
      Files: `internal/run/store.go`, `internal/worker/runtime_executor.go`.
      Depends on: A2, A4, B2, C3.
- [ ] D2. Enforce `fanOut.maxParallel` in the distributed scheduler: add an
      in-flight `COUNT(*) … status='running'` subquery to the claimer's atomic claim
      predicate (`internal/worker/claimer.go:248-270`) and the owner-dispatch path
      (`internal/run/store.go:1706` region), so no more than `maxParallel` instances
      of a group run at once. The job-level `maxParallelTasks` pool
      (`worker.NewPool`, `job.go:1201`) and per-instance rate limits
      (`acquireTaskRateLimit`, `job.go:1209`; `RateLimitTask` parking) continue to
      bound the total and are unchanged. Ordering (D3) needs **no** claim-predicate
      change and must not acquire one here: readiness is already the
      `outstanding_predecessors = 0` gate, and `maxParallel` is orthogonal —
      ordering decides which instances are *ready*, `maxParallel` decides how many
      ready ones are *in flight*. Deadlock is impossible because readiness derives
      from terminal siblings, never from free slots; add a test that proves it on a
      chain deeper than `maxParallel`.
      Files: `internal/worker/claimer.go`, `internal/run/store.go`.
      Depends on: D1.
- [ ] D3. In-group ordering in the store — the one shared implementation both lanes
      inherit. (a) **Seed**: at expansion (D1), set each instance's
      `outstanding_predecessors` to `<template's current value> + <in-group indegree
      from A4>`, so the existing "decrement to zero → ready" gate carries ordering
      with **no new predicate** anywhere — the claimer
      (`internal/worker/claimer.go:257,266`), owner dispatch
      (`internal/run/store.go:1739`), and the local indegree re-seed
      (`internal/job/job.go:676`) all already read that one column.
      (b) **Decrement**: add a sibling decrement keyed on the `TaskRun` **primary
      key**. `batchDecrementPredecessorsTx` (`internal/run/store.go:2333`) updates
      `WHERE job_run_id = ? AND task_id IN ?`, which is exactly right for the
      cross-step edge (one producer completion decrements every sibling) and exactly
      wrong for the in-group edge (one sibling must decrement only its dependents).
      Implement as one indexed group read over `idx_taskrun_jobrun_task` plus one
      `UPDATE … WHERE id IN (…)`, bounded by N ≤ 1024, and call it from **every**
      terminal-success path — `cacheHitTask` (`:1933`) as well as `completeTask`
      (`:2621`), because a cache-hit instance is a satisfied dependency. A run-scoped
      instance-edge table was considered and rejected in the design: a new model and
      migration to replace one bounded scan.
      (c) **Skip cascade**: `failTask` (`:3174`) marks the failed instance's
      transitive in-group dependents `skipped` with reason
      `"fan-out dependency <key> failed"` — the instance-keyed analogue of
      `skipTaskAndDescendantsTx` (`:3075`), reusing `markTaskSkippedTx` (`:2457`).
      **Load-bearing, not a nicety**: without it a dependent's counter never reaches
      zero, the distributed run waits out its timeout, and the local run trips the
      terminal-count guard at `internal/job/job.go:1554-1558`.
      Trigger rules stay step-level and are untouched: `predecessorStatusesTx`
      (`:2488`) resolves predecessors from `task_edges`, which holds no in-group
      edges, so an instance's `shouldRunTaskTx` check (`:2596`) evaluates cross-step
      predecessors exactly as today.
      Files: `internal/run/store.go`.
      Depends on: D1.

### Stream E — Surfaces: REST + CLI + observability alignment

The operator surface (design Phase 4, backend): the partition-inspection and
per-instance-retry endpoints, the CLI verbs over them, and the alignment of the
causal verbs (`why`, `run diff`, replay) that assumed one `TaskRun` per `Task`.

- [ ] E1. Add the REST partition surface: `GET /v1/jobs/:id/runs/:run_id/tasks/
      :task_id/partitions` (paginated instance list — value, index, status, attempt,
      cache_hit, duration, error, plus `fingerprint` and `depends_on` so an ordered
      group is inspectable without re-deriving the graph from the producer's row),
      `POST …/tasks/:task_id/partitions/:index/retry`
      (reset one failed instance, terminal runs only, re-evaluate fan-in on
      completion), and collapse fanned groups in run-detail payloads to one entry
      with `partition_count` + a status histogram (a 10k-instance run must not bloat
      every run-list response). Add the route lines to `Protected()`
      (`api/rest/bind/bind.go:57`).
      Files: new `api/rest/controller/job/run/partitions.go`,
      `api/rest/service/run/`, `api/rest/service/task/`, `api/rest/bind/bind.go`.
      Depends on: A1, D1.
- [ ] E2. Add the CLI verbs: `caesium run partitions <run-id> --task <name>
      [--status failed] [--json]` (a new `cmd/run/partitions.go` registered on
      `run.Cmd`, `cmd/run/run.go:6`) and a `--partition <value>` flag on
      `caesium run retry` (`cmd/run/retry.go`) that resets a single instance.
      Machine output goes to **stdout, logs to stderr** per the repo's
      stdout-cleanliness gate. `--json` includes each instance's `fingerprint` and
      `depends_on`; retrying one instance does **not** cascade to dependents that
      already succeeded, and the command says so rather than silently re-running a
      subtree.
      Files: new `cmd/run/partitions.go`, `cmd/run/retry.go`, `cmd/run/run.go`.
      Depends on: E1.
- [ ] E3. Align the causal verbs with fanned groups: `caesium why` names
      `partition` **and `partition_fingerprint`** as discriminating fields via the
      `HashInputBlob` fields (from C2) — a fingerprint-driven miss that reports only
      "the hashes differ" is the failure this item prevents;
      `caesium run diff` (`cmd/run/diff.go` + `api/rest/service/rundiff/`) aligns
      instances across runs by partition **value** (never index) and reports
      added/removed partitions; `receipt get` and `why --task` disambiguate the
      one-`TaskRun`-per-`Task` assumption (select via a `--partition` selector or
      default to the group summary); quarantined `replay` (`cmd/run/replay.go`)
      **refuses baselines containing fanned groups** (fail-closed, per the
      quarantined-replay design posture).
      Files: `cmd/run/diff.go`, `cmd/run/replay.go`, the `why`/`receipt` commands
      under `cmd/run/`, `api/rest/service/rundiff/`, `api/rest/service/why/`.
      Depends on: C2.
- [ ] E4. Make `run retry` ordering-aware. `RetryFromFailure`
      (`internal/run/store.go:4790`) keeps succeeded/cached instances and resets
      failed ones; with an ordered group it must **also** reset instances the store
      marked `skipped` for a failed in-group dependency (D3c) — otherwise retrying
      the failed root leaves its whole subtree permanently skipped — and re-seed each
      reset instance's `outstanding_predecessors` to its in-group indegree counted
      over **non-terminal** dependencies only (0 when its dependencies already
      succeeded, 1 when the dependency is itself being retried). The group is still
      **not** re-expanded: the producer is terminal and the recorded instances are
      reused. Drive it end to end: fail the root of an `a → b → c` group, retry, and
      assert `a` re-executes, `b`/`c` run in order after it, and any independent
      sibling cache-hits.
      Files: `internal/run/store.go`, `cmd/run/retry.go`.
      Depends on: D3.

### Stream F — UI (Caesium Console)

The frontend group rendering (design Phase 4, UI): a fanned step is one grouped
node — the graph never gains N nodes, so 400 partitions render like 4. Consumes
the Stream E endpoints.

- [ ] F1. Render a fanned step as one **grouped node** in the DAG
      (`ui/src/features/jobs/JobDAG.tsx`): a stacked-card affordance, a `×N` badge,
      and a segmented progress ring (succeeded/running/failed/pending). The graph
      never gains N nodes.
      Files: `ui/src/features/jobs/JobDAG.tsx`.
- [ ] F2. Add the run-timeline group lane and the partition table: one lane per
      group in `RunTimeline.tsx` (an envelope bar first-start→last-end with a density
      strip, expandable to the top-K longest/failed), and a virtualized,
      status-filterable partition table in `TaskDetailPanel.tsx` (value, status,
      attempt, duration, cache-hit, per-row log link and retry button wired to the
      retry endpoint). For an ordered group the table also shows the short
      fingerprint and a `depends on` column, and `skipped` renders as a first-class
      status (an in-group dependency failure is a normal, expected outcome, not an
      error state). Add the `getPartitions`/`retryPartition` methods to
      `ui/src/lib/api.ts`.
      Files: `ui/src/features/jobs/RunTimeline.tsx`,
      `ui/src/features/jobs/TaskDetailPanel.tsx`, `ui/src/lib/api.ts`.
      Depends on: E1.

## Harness Strengthening

- [ ] H-1. Wire the fan-out path onto the live integration server: set
      `CAESIUM_FANOUT_MAX_PARTITIONS` (a low value so the "1025 partitions fails the
      producer loudly" cap test drives the real cap) on the `just integration-up` /
      `just integration-test` server and pass it through in
      `.github/workflows/ci.yml`, and ensure the harness can run the **distributed
      lane** (`CAESIUM_EXECUTION_MODE=distributed`) scenario so worker-crash /
      lease-reclaim / rate-limit-drain assertions execute in CI, not an internal
      call. Add the shared fan-out test helpers to the `test/` harness: a producer
      script that emits a **string-form** list, one that emits an **object-form** list
      with fingerprints and `dependsOn` (the `a → b → c` and diamond fixtures the
      ordering scenarios drive), and one that emits an **invalid** list per rejection
      class (cycle, dangling key, malformed fingerprint, over-cap). Any image these
      helpers use must be a canonical pinned ref — the image-pin guardrail
      (`TestPinnedContainerImageVersionsAreConsistent`) rejects unpinned base-image
      names, and its canonical set lives in `internal/guardrails/guardrails_test.go`.
      Files: `justfile`, `.github/workflows/ci.yml`, `test/` harness helpers.

## Navigational / Organizational Improvements

- [ ] N-1. Reflect fan-out in the docs, last, after A–F ship. Flip the
      [`docs/design-dynamic-fanout.md`](../../design-dynamic-fanout.md) `> Status:`
      banner from "Brainstorm/Design" to shipped (pointing at this plan); update the
      "Dynamic fan-out" row in the `docs/roadmap.md` Phase 4 design-wave table
      (`docs/roadmap.md:222`) to Shipped with a plan link; document the `fanOut`
      block and the `##caesium::partitions` / `##caesium::partition` markers across
      `docs/job-schema-reference.md`, `docs/job-definitions.md`, and
      `docs/caesium-job-llm-reference.md` — including the **object element form**
      (`{key, fingerprint, dependsOn, …scalar attributes}`),
      `CAESIUM_PARTITION_JSON`, and the rule that `dependsOn` is scheduling metadata
      a container must not derive data behavior from. `docs/job-schema-reference.md`
      is **generated** from `internal/jobdef/report/report.go`
      (`TestGeneratedSchemaReferenceIsCurrent`): update the generator (B1) and
      regenerate, never hand-edit the doc. Add a fan-out example under
      `docs/examples/*.job.yaml` that `caesium job lint` accepts (canonical pinned
      image refs only — the image-pin guardrail rejects unpinned base-image names)
      and index it in `docs/job-definitions.md`
      (`TestJobDefinitionsDocReferencesEveryExampleManifest`); and update the
      existing `design-dynamic-fanout.md` bullet in `docs/README.md`
      (`docs/README.md:46`) in-place from "(proposed)" to shipped — keep it in
      backtick/inline-code form (do not add a clickable subdirectory link; the
      `TestDocsREADMEIndexesEveryTopLevelDoc` guardrail rejects them).
      Files: `docs/design-dynamic-fanout.md`, `docs/roadmap.md`,
      `docs/job-schema-reference.md`, `docs/job-definitions.md`,
      `docs/caesium-job-llm-reference.md`, `docs/examples/`, `docs/README.md`.
      Depends on: A–F (runs last, after the runtime ships).

#### Deferred — design Phase 5 follow-ups

Per the design's Phase 5 and Non-Goals, these are **out of scope** for this plan
and recorded as follow-ons: (1) **quarantined-replay re-expansion** — v1 refuses
baselines with fanned groups (fail-closed in E3); re-expanding from the recorded
partition list in descriptors is a follow-up. (2) **Value-verified per-partition
skip across producer re-runs** — v1's conservative identity re-runs all instances
when the producer's own inputs change. The structured-partition amendment makes
this *expressible* (C2 puts each unit's fingerprint in its key) but does **not**
enable it: the enabling half is the chain break, `cache.chain: values`, owned by
[`2026-08-25-dag-native-infrastructure-deployment-design.md`](../../superpowers/specs/2026-08-25-dag-native-infrastructure-deployment-design.md)
§4. Nothing in this plan may invent a second mechanism for it; fingerprints
without the chain break are correct but conservative (all instances re-run when
the producer does — never a stale hit). The design's eight Open Questions
(`minSuccessRatio`, window-derived partitions, per-partition resource profiles,
aggregate contract granularity, `retry_partition` as an agent action, freshness
interplay, in-group ordering visibility, an in-group depth guardrail) are
cross-design questions, not items here.

## Sequencing & Dependencies

**Cross-stream order:**

- **Streams A and B are the foundation and are independent of each other** (A owns
  `internal/run/store.go` + `internal/models/run.go` + `pkg/task/output.go`; B owns
  `pkg/jobdef/definition.go` + `internal/models/task.go` + the runtime spec) — they
  run in parallel in the first wave. A has the larger blast radius, so it merges
  first on any same-wave overlap.
- **Stream C** (local executor) depends on A (model + store re-key + markers) and B
  (the `FanOutConfig` the executor reads).
- **Stream D** (distributed) depends on A2 (the re-keyed store it extends) + A4 (the
  in-group validator D1 runs before its first insert) + B2 + **C3** (it reuses C's
  fan-in status + output-aggregation contract) — so D runs after C, not in parallel
  with it. **But note the one back-edge the structured-partition amendment
  introduces:** the expansion transaction D1 builds is shared by *both* lanes (local
  execution completes through the same `completeTask` path), so C4 — the local
  mirror of in-group ordering — depends on D3. C1–C3 are still unblocked by A + B;
  only C4 waits.
- **Stream E** depends on A1 (the partition columns it reads) + D1 (the instances it
  lists); E3 depends on C2 (the `HashInputBlob` partition fields); E4 depends on D3
  (the skipped-instance state it must reset).
- **Stream F** depends on E1 (the endpoints it calls).
- **H-1** is independent (justfile / CI / test harness) and supports the A–D
  integration scenarios; land it in the first wave so the engine's end-to-end gate
  has a live, capped, distributed-capable surface to drive.
- **N-1** runs last, after A–F ship, so the roadmap/schema/design docs reflect
  reality.

**Suggested waves:**
- **W1 = A (A1→A2→A3→A4) + B (B1→B2) + H-1.** A and B touch disjoint files.
- **W2 = C (C1→(C2, C3)).** Unblocked once A + B are in.
- **W3 = D (D1→(D2, D3)).** Unblocked once C3's fan-in contract is in.
- **W4 = C4 + E (E1→E2, E3, E4).** C4 lands here, not in W2 — it mirrors D3's
  store-side ordering. Unblocked once D1's instances exist (E3 needs only C2).
- **W5 = F (F1, F2) + N-1.** F after E1; N-1 last.

**Within-stream order:** A1 → A2 → A3 → A4 (columns+index, then the store re-key,
then markers, then the in-group validator — A3's env cap is read by B1's lint and
C1's executor, and A4 consumes A3's normalized `[]Partition`). B1 → B2. C1 → (C2,
C3 in parallel — different concerns in the same file, coordinate the merge) → C4
(after D3). D1 → (D2, D3 in parallel — different funcs in the same file, D2 in the
claim predicate, D3 in the decrement/skip paths). E1 → E2; E3 parallel to E1/E2
(depends only on C2); E4 after D3. F1, F2 parallel (F2 needs E1).

**Cross-stream file conflicts:**

- `internal/run/store.go` — A2 *re-keys* every write path; D1 *adds* the expansion
  transaction to `CompleteTaskWithResult`/`completeTask`; D2 touches the dispatch
  predicate; D3 adds the instance-keyed sibling decrement and the in-group skip
  cascade; E4 adjusts `RetryFromFailure`; E1 reads instances via the service layer
  (no `store.go` edit).
  **Sequence A2 → D1 → (D2, D3) → E4** (already a dependency chain); never the same
  wave. D2 and D3 may share a wave — D2 is in the claim predicate, D3 is in the
  decrement/skip paths.
- `internal/job/job.go` — C1, C2, C3, C4 all edit it (Kahn loop, hashing, fan-in,
  the ordering mirror). All in Stream C; land C1 first, then coordinate the C2/C3
  merge (different funcs); C4 comes later, after D3.
- `pkg/task/output.go` — A3 adds `Partitions` marker parsing and the `Partition`
  element type; C3 adds the `BuildOutputEnv` aggregation. **Sequence A → C**
  (already a dependency); different funcs, mechanical rebase. A4 lands in a **new**
  file (`pkg/task/partition_graph.go`), so it does not conflict with either.
- `internal/cache/hash.go` — C2 only (`Partition`, `PartitionFingerprint`,
  `PartitionAttributes` + the `HashInputBlob` mirror). No other stream edits it; the
  `fanOut` config and a partition's `dependsOn` deliberately do not enter the hash.
- `internal/models/models.go` — **no edit by any stream**: A1's `TaskRun` columns
  and B2's `Task.FanOutConfig` are columns on models already in the `All` slice, and
  AutoMigrate derives them from struct tags.
- `pkg/env/env.go` — A3 adds `CAESIUM_FANOUT_MAX_PARTITIONS`, the only new env
  field; single stream, no conflict.
- `internal/metrics/metrics.go` — C3 only (both the `var (...)` block and the
  `Register()` list); single stream.
- `pkg/jobdef/definition.go` — B1 only (the dual `Step`/`rawStep` declaration makes
  it a true-conflict file; keeping all schema work in Stream B avoids the collision).
- `internal/jobdef/report/report.go` — B1 only (the `fanOut` rows in the generated
  schema reference). `docs/job-schema-reference.md` is regenerated from it; N-1 must
  not hand-edit that doc (`TestGeneratedSchemaReferenceIsCurrent`).
- `internal/worker/runtime_executor.go` — D1 only (partition env injection +
  sibling-aware predecessor outputs); no other stream edits it.
- `api/rest/bind/bind.go` — E1 only (additive route lines).
- `cmd/run/run.go` / `cmd/run/retry.go` — E2 (register `partitions`, add
  `--partition`); E3 edits `cmd/run/diff.go` and `cmd/run/replay.go` — different
  files, same stream E.
- `ui/src/lib/api.ts` — F2 only (append two methods).
- `go.sum` — no stream adds a dependency; no `go mod tidy` conflict expected.

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Per-stream additions:

- **New REST endpoint / CLI verb (A, B, C, D, E):** an integration scenario in
  `test/` that drives the **real surface** against the live server — a producer that
  emits N partitions, N instances materialized and run each seeing
  `CAESIUM_PARTITION`, fan-in running once with the aggregate env visible; the retry
  scenario asserting unchanged partitions **cache-hit** and only the failed one
  re-executes; `onEmpty` both modes; the cap (1025 partitions **fails the producer
  loudly**). A unit test that hand-builds partitions and calls the matcher/hasher
  proves that unit, not the wiring — both are required.
- **Distributed lane (D):** the `CAESIUM_EXECUTION_MODE=distributed` scenario —
  expansion in the completion tx, a worker crash mid-group → lease reclaim with
  siblings unaffected, rate-limit parking + drain with no over-issue.
- **New metric (C3):** assert `caesium_fanout_partitions_total` /
  `caesium_fanout_group_duration_seconds` via `internal/metrics/testutil` in a
  `*_test.go`; both collectors must also appear in `Register()`.
- **Machine-readable CLI (E2):** `caesium run partitions --json` stdout clean and
  parseable, captured **separately from stderr** (`runCLIStdout`, never the
  stream-merging capture).
- **Job-schema validation (B1):** `caesium job lint --path docs/examples/` green on
  the new `fanOut` example; an invalid `fanOut` (bad `from`, chained fan-out, branch
  step, over-cap `maxPartitions`) rejected at lint.
- **UI changes (F):** `just ui-lint && just ui-test && just ui-e2e` — the grouped
  DAG node, the partition table, and per-partition retry driven under Playwright.
- **Structured-partition backward compatibility (A3, C2):** a **golden-digest**
  test proving a string-form producer's instance hashes are byte-identical to the
  pre-amendment values, and that `CacheVersion` is unchanged. "A hash was produced"
  is not the assertion; the specific digest is.
- **In-group graph rejection matrix (A4, D1):** each of cycle / dangling
  `dependsOn` key / self-reference / malformed `fingerprint` / conflicting duplicate
  `key` / over-cap object **fails the producing task** with the offending key named,
  and leaves **zero** instance rows behind — assert the row count, not only the
  error string, because a partially-expanded group is the failure the expansion
  transaction exists to prevent.
- **Ordering, both lanes (C4, D3):** the same `a → b → c` and diamond fixtures run
  under the local executor **and** `CAESIUM_EXECUTION_MODE=distributed`, asserting
  identical observed order; a chain deeper than `fanOut.maxParallel` completes
  without deadlock; and under `failurePolicy: continue` a failed root resolves its
  transitive dependents `skipped` **and the run terminates** rather than waiting out
  its timeout (assert the run's own duration bound, not just the final status).
- **Ordering-aware retry (E4):** fail the root of an ordered group, `caesium run
  retry`, and assert the previously-skipped dependents re-run **in order** after the
  root and independent siblings cache-hit.
- **`CAESIUM_PARTITION_JSON` (C1, D1):** present in the container, equal to the
  normalized object, and changing **only** `dependsOn` between two runs changes no
  instance's hash.
- **This plan's checkbox ticked**, the active-wave `## Progress` bullet appended,
  and any cross-linked doc (roadmap/schema) refreshed in the same PR.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream A — the substrate** is in: `TaskRun` carries partition columns under a
   unique `(job_run_id, task_id, partition_index)` index (migrated on the sharded
   hot tables), every run-store write path keys on the `TaskRun` primary key with no
   behavior change for unfanned tasks, and `pkg/task` parses the partition markers
   in **both** the string and object forms — with the caps that **fail the
   producer** on overflow and the in-group validator (A4) that names the offending
   key on a cycle or dangling `dependsOn`. Closed by unit coverage on the
   marker/caps/graph and a green integration run where the store handles multiple
   `TaskRun` rows per `(run, task)`.
2. **Stream B — the schema contract** is live: `fanOut:` parses on a step, the
   `validateSteps` rules reject a bad `from` / chained fan-out / branch fan-out /
   over-cap `maxPartitions`, and the config persists onto `models.Task.FanOutConfig`
   without entering the cache hash. Closed by `caesium job lint` accepting the valid
   example and rejecting the invalid cases in CI.
3. **Stream C — the local executor** materializes instances: a producer emitting N
   partitions runs N local instances each seeing `CAESIUM_PARTITION` and
   `CAESIUM_PARTITION_JSON`, the partition key/fingerprint/attributes fold into
   `cache.HashInput` (so retry cache-hits unchanged partitions) while `dependsOn`
   does not, a string-form producer's hashes are byte-identical to the
   pre-amendment values, and fan-in resolves the group once with the aggregate env.
   Closed by the happy-path + retry + `onEmpty` + golden-digest integration
   scenarios green in CI, plus the `caesium_fanout_*` metric assertions.
4. **Stream D — the distributed lane** materializes instances inside the producer's
   completion transaction (no half-expanded group observable, and no partially
   expanded group after a rejected `dependsOn` graph), seeds in-group ordering onto
   `outstanding_predecessors` so **no new claim predicate is needed**, decrements
   siblings by `TaskRun` primary key, skips a failed instance's transitive
   dependents instead of hanging, enforces `fanOut.maxParallel` in the claim
   predicate, and survives a mid-group worker crash via lease reclaim. Closed by the
   `distributed` integration scenarios green in CI, including the ordering fixtures.
5. **Stream E — the surfaces** ship: `GET …/partitions` (with `fingerprint` and
   `depends_on`) and the per-instance retry endpoint back
   `caesium run partitions --json` (clean stdout) and
   `caesium run retry --partition`; `why`/`run diff` align instances by partition
   value and name `partition_fingerprint` as a discriminating field; `run retry`
   resets dependency-skipped instances and re-seeds their in-group indegree; and
   quarantined `replay` fails closed on fanned baselines. Closed by CLI integration
   scenarios asserted via `runCLIStdout`.
6. **Stream F — the UI** renders a fanned step as one grouped node (never N nodes),
   with the run-timeline group lane and the virtualized partition table + per-row
   retry, and shows fingerprint / `depends on` / `skipped` for ordered groups.
   Closed by the Playwright scenario green under `just ui-e2e`.
7. **H-1 — the integration server** exercises the fan-out path with the cap set,
   the distributed lane runnable, and the string-form / object-form / rejection
   fixtures available, so the A–D scenarios drive the live binary in CI.
8. **N-1 — docs reflect reality:** the `design-dynamic-fanout.md` banner flipped,
   the `docs/roadmap.md` Phase 4 fan-out row marked Shipped, the `fanOut` block +
   both marker element forms + `CAESIUM_PARTITION_JSON` documented in the schema
   references (`docs/job-schema-reference.md` **regenerated**, never hand-edited)
   with a working `docs/examples/` manifest, and the `docs/README.md` bullet updated
   in place in backtick form.
9. **Cross-cutting:** `docs/roadmap.md`, `docs/design-dynamic-fanout.md`, and this
   plan's per-stream `## Progress` entries reflect every shipped stream and match the
   merged PRs. (Phase 5 replay re-expansion and value-verified per-partition skip
   remain explicitly deferred — not gates here. Per-partition skip in particular is
   gated on `cache.chain: values`, which this plan does not own.)

## How To Pick Up Work

1. Read this file end-to-end so you understand the streams, their
   interdependencies, and which acceptance criterion the item closes.
2. Pick an unchecked item under `## Streams` whose `Depends on:` line is satisfied
   (consult `## Sequencing & Dependencies`).
3. Branch from `master` (or land in a worktree if dispatched by
   `exec-plan-wave`); do the work as a self-contained PR.
4. Run the verification block under `## Verification (Run For Every PR)`.
5. Tick the checkbox for your item, add a per-stream bullet to the active wave
   subsection in `## Progress` (or open a new wave subsection if none exists yet),
   and update any cross-linked design doc / roadmap section in the same PR.
6. Open the PR with title format
   `<Imperative subject> (dynamic-fanout <wave>-<stream>)` — e.g.
   `Add TaskRun partition columns and instance-keyed store paths (dynamic-fanout W1-α)`.
   GitHub appends `(#NNN)` on squash-merge.

## Cross-References

- [`docs/design-dynamic-fanout.md`](../../design-dynamic-fanout.md) — the design of
  record and source of truth for intent, scope, and the Non-Goals that bind this
  plan; its
  [`## Structured Partitions`](../../design-dynamic-fanout.md#structured-partitions-key--fingerprint--dependson)
  section (amendment, 2026-08-25) is authoritative for the object element form,
  the fingerprint's place in the identity hash, and the in-group ordering
  semantics.
- [`docs/superpowers/specs/2026-08-25-dag-native-infrastructure-deployment-design.md`](../../superpowers/specs/2026-08-25-dag-native-infrastructure-deployment-design.md)
  — the consumer that forced the structured-partition amendment (§5.4), and the
  owner of the orthogonal `cache.chain: values` chain break (§4) that per-unit
  cache skip depends on. This plan implements the partition side of that contract
  and must not implement the chain-break side.
- [`docs/roadmap.md`](../../roadmap.md) Phase 4 design-wave table — the "Dynamic
  fan-out" entry this plan ships.
- [`pkg/jobdef/definition.go`](../../../pkg/jobdef/definition.go) — the job-definition
  schema Stream B extends with `fanOut`.
- [`docs/job-schema-reference.md`](../../job-schema-reference.md),
  `docs/job-definitions.md`, `docs/caesium-job-llm-reference.md` — the schema docs
  N-1 extends with the `fanOut` block and partition markers.
- [`docs/design-quarantined-replay.md`](../../design-quarantined-replay.md) — the
  fail-closed posture E3 follows for fanned baselines and the deferred
  per-partition skip.
- `internal/run/store.go`, `internal/job/job.go`, `internal/worker/claimer.go`,
  `pkg/task/output.go`, `internal/cache/hash.go`,
  `internal/cache/shortcircuit.go` — the execution/cache surfaces this plan rewires
  (and, in `shortcircuit.go`'s case, deliberately does not: its conservative
  byte-identical-output proof cannot substitute for the chain break).
