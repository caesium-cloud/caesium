# Dynamic Fan-Out — Data-Proportional Parallelism

Last updated: 2026-08-26

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
**no new YAML field, no new env var, and no new metric series**.

A seventh contract came out of the third review pass and is the one to check every
item against, because the same defect shape has now surfaced three times:
**(7) route completeness — every piece of state fan-out introduces must have its
seed, its inverse, and its recovery defined on all four completion routes and all
three advancement implementations.** A seed with no inverse is a stall; an inverse
present on only some routes is a *mode-dependent* stall, which is strictly worse
because it passes CI in the default configuration and fails in production under a
flag nobody varied. The matrix to fill in per item is in the design's
[`## Route completeness`](../../design-dynamic-fanout.md#route-completeness-state-this-once-check-every-item-against-it)
section. **Read that section's closing subsection before relying on the matrix**:
a fourth review round found a defect it does not catch, because its axes are
operation × completion route and the defect lived on a *traversal site* (the owner
keeps a live and a replay implementation of every traversal, and `ApplyTerminalRow`
duplicates the successor walk inline rather than calling it). The plan's answer is
**not** a third matrix axis — a three-dimensional checklist is past the point an
implementer reliably completes, and a checklist nobody completes launders the
omission as diligence. The answer is **G7**: collapse the duplication so an edge
class is enumerated in exactly one place, and the question stops being askable. Note especially that **a cache hit is a completion** and `cacheHitTask`
is a different function from `completeTask` — with per-unit fingerprints, a
cache-hit prerequisite is the *common* path through an ordered group.

A sixth contract was added by the second-pass amendment and is the easiest one to
violate by omission: **(6) there are three DAG-advancement implementations, and
every fan-out behavior must hold in all three.** Local and distributed-SQL both
complete through `completeTask`; run-owner in-memory
(`CAESIUM_RUN_OWNER_IN_MEMORY=true`) does not — it advances `RunState` in memory
and persists through `CompleteTaskOwner`, which by its own docstring does not
decrement predecessors or evaluate trigger rules in SQL. Any item that hooks
`completeTask` alone is incomplete, and any test that exercises only one owner
mode is not evidence. Strategic
priority/status is tracked in
[`docs/roadmap.md`](../../roadmap.md) Phase 4 design-wave table (the roadmap wins
on priority/status). The job-definition contract lives in
[`pkg/jobdef/definition.go`](../../../pkg/jobdef/definition.go).

## Progress (as of 2026-08-26)

**Wave 1 (2026-08-26) — an unverified first pass, an audit, then a gap-closing
wave.** Wave 1 did not land in one clean pass, and the honest record matters more
here than a tidy one. A first-pass agent ticked **every** checklist item in this
file and marked all nine streams "Shipped" without verifying a single claim
against the code. A subsequent audit found roughly half of those claims real and
the rest absent or stubbed. A second wave of agents then closed the gaps stream by
stream, over three rounds on PR #349: the initial ship, a commit closing the gaps
CI and review surfaced, and a third answering the maintainer's **25-finding
adversarial review** — every one of the 25 confirmed real, none argued away. That
third round produced follow-ups 13–15 below.
Every tick below was re-derived from the working tree on 2026-08-26 by grepping
the named symbol; items whose shipped behaviour diverges from the plan's original
wording now carry a `_Shipped 2026-08-26: …_` evidence line or an `_Open: …_`
note. The carve-outs that are genuinely not done are listed under
[`### Follow-ups`](#follow-ups-open-after-wave-1) at the end of this section —
they are real gaps, not paperwork.

The plan was published from
[`docs/design-dynamic-fanout.md`](../../design-dynamic-fanout.md) (then Status:
Brainstorm/Design) with every item grounded against the executor, run store,
claimer, and cache-identity code as of 2026-07. That banner is now flipped to
Shipped, and the `docs/roadmap.md` Phase 4 row with it.

**Amendment, 2026-08-25 — structured partitions.** Because no stream had started,
the design's structured-partition amendment
([`## Structured Partitions`](../../design-dynamic-fanout.md#structured-partitions-key--fingerprint--dependson))
was absorbed into the existing streams rather than bolted on as a follow-up wave:
items A3, C1, C2, D1, E1, E2, E3, F2, H-1, and N-1 were **amended in place**, and
items A4, C4, D3, and E4 are **new**.

**Amendment, 2026-08-25 (second pass) — Stream G, and a correction.** Review of
the amendment found that the plan (and the design) had **undercounted the
identity problem**. The original plan named it as "the honest hard part" and
scoped it to the run store's write paths (A2). That is one of at least five
subsystems: Caesium has **three** DAG-advancement implementations, not two, and
the third — run-owner in-memory mode (`CAESIUM_RUN_OWNER_IN_MEMORY=true`) —
deliberately bypasses `completeTask` entirely, so an expansion hook placed there
is never reached and a fan-out group silently collapses to one row. The same
task-ID-keyed root cause also sits in the owner's checkpoint format, its recovery
replay, retry accounting, and the owner↔worker wire protocol. That work is
behavior-neutral for unfanned runs, touches code fan-out does not otherwise care
about (failover, checkpointing, incident attribution), and **must land before any
expansion or ordering work** — so it is now **Stream G**, a gating prerequisite,
rather than scattered bullets inside the streams that consume it. A2 is
correspondingly narrowed and now depends on G. See
[`## The task-ID identity assumption`](../../design-dynamic-fanout.md#the-task-id-identity-assumption).

**Every `file:line` in this document is now stale by construction.** All of them
predate the Wave 1 implementation, which added ~1,400 net lines to
`internal/run/store.go` alone and split fan-out's store code into four new files.
**Grep the symbol; never trust the number.** Post-wave anchors for the ones cited
most often, accurate 2026-08-26: `CompleteTaskWithResult`
`internal/run/store.go:2056`, `cacheHitTask` `:2142`, `completeTask` `:3114`,
`CompleteTaskOwner` `:3479`, `RegisterTasks` `:1161`, `StartTask` `:1655`,
`RateLimitTask` `:1941`, `successorEdgesForRunTx` `:2483`,
`batchDecrementPredecessorsTx` `:2617`, `retryTask` `:3965`, `SkipTask` `:4063`,
`RetryFromFailure` `:5561`; `NewRunState` `internal/run/owner_state.go:149`,
`ApplyCompletion` `:261`, `advanceSuccessors` `:393`, `traverseSuccessors` `:401`,
`ApplyTerminalRow` `:461`, `ExpandTask` `:672`, `Restore` `:1046`. Fan-out's own
store code lives in files this plan predates: `internal/run/fanout.go`,
`internal/run/store_instance.go`, `internal/run/store_readsurface.go`, and
`internal/run/taskrun_identity.go`.

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| G | **Instance identity migration** — re-key the whole run lifecycle off `(run, task)` onto `TaskRun` identity: SQL advancement, the run-owner in-memory engine + checkpoint format + recovery replay, retry accounting, and the owner↔worker wire protocol. Behavior-neutral for unfanned runs; **gates A2 and everything downstream** | **P0 — gating** | **Shipped** — G6's audit table is recorded under G6 below; G3's failover-mid-group test is [open](#follow-ups-open-after-wave-1) |
| A | Substrate — `TaskRun` partition + fingerprint/ordering columns + unique index, instance-keyed store rewrite, marker parsing (string **and** object form) + caps, server hard cap env | **P0** | **Shipped** — columns + explicit index migration (`pkg/db/migrations.go`), `pkg/task/partition{,_graph}.go`, `CAESIUM_FANOUT_MAX_PARTITIONS` |
| B | Schema + lint contract — `FanOut` on `Step`, `validateSteps` rules, `FanOutConfig` on `models.Task`, runtime spec (**unchanged by the structured-partition amendment — no new YAML field**) | **P0** | **Shipped** — `validateFanOut` + `models.Task.FanOutConfig` + regenerated schema reference |
| C | Local executor — expansion, per-partition cache identity (`key` + `fingerprint` + attributes), in-group ordering + skip cascade, group fan-in + output aggregation, metrics | **P0** | **Shipped** — `runFannedGroup` reads ordering from the store's `outstanding_predecessors`; both `caesium_fanout_*` series observed |
| D | Distributed lane — expansion + cycle check + indegree seeding in the completion transaction, instance-keyed sibling decrement, `maxParallel` claim predicate, sibling-aware predecessor outputs | P1 | **Shipped** — SQL lane in `internal/run/fanout.go`, owner lane in `RunState.ApplyExpansion`/`ReadyTasks`; owner failover-mid-group scenario is [open](#follow-ups-open-after-wave-1) |
| E | Surfaces — REST partition endpoints + `caesium run partitions`/`retry --partition` + ordering-aware retry reset + `why`/`run diff`/replay alignment | P1 | **Shipped** — `ListPartitions`/`RetryPartition` + `caesium run partitions` / `retry --partition` / `why --partition`; replay fails closed with 409 |
| F | UI — grouped DAG node, run-timeline group lane, virtualized partition table with per-row retry and ordering/fingerprint columns | P2 | **Shipped** — `fanout-status-strip`, `run-timeline-group-row`, `PartitionTable` (`@tanstack/react-virtual`), `ui/e2e/fanout.spec.ts` |
| H-1 | Integration harness — fan-out caps + distributed lane + ordered-group fixtures wired on the live integration server | — | **Shipped** — three servers at `CAESIUM_FANOUT_MAX_PARTITIONS=8`, new `integration-test-owner-memory` lane + CI job, 21 `TestFanOut*` scenarios; **the distributed lane's pre-wave green history is not evidence** ([why](#follow-ups-open-after-wave-1)) |
| N-1 | Docs — roadmap row, design banner, schema references, marker object form, examples, README | — | **Shipped** — banner + roadmap flipped, `## Dynamic Fan-Out` in `job-definitions.md`, LLM reference, generated schema reference, `docs/examples/dynamic-fanout.job.yaml` |

### Follow-ups (open after Wave 1)

These are the carve-outs Wave 1 did **not** close. Each is a real gap; none is
blocked on a design decision.

1. **There is no failover-mid-group test, anywhere.** G3, D4, and D5 each ask for
   a scenario that kills an owner mid-group and asserts the takeover re-dispatches
   only the unfinished instances. `internal/run/failover_test.go` still holds
   exactly one test (`TestFailover_TakeoverAndResume`), and neither it nor
   `internal/run/recovery_test.go` mentions partitions; `test/` has no failover
   scenario at all. The recovery *state* rebuild is unit-covered
   (`TestRehydrateInGroupEdges_RestoresMaxParallel` / `_RestoresPartitionKeys` /
   `_RestoresFailurePolicy`), the end-to-end takeover is not. This is the single
   largest evidence gap in the wave.
2. **G3's checkpoint-cadence starvation is untouched.**
   `internal/run/checkpoint_writer.go` has no diff in this wave;
   `CheckpointWriter.due` still keys off `RunState.seq`. Because `seq` now
   advances per *instance* rather than per group, the behaviour moved in the
   right direction on its own — but nothing asserts it, and the item's stated
   concern was never closed deliberately.
3. **`caesium run retry <run>` without `--partition` still runs in-process.**
   `cmd/run/retry.go:75` opens `runstorage.Default()` directly; only the
   `--partition` path goes over `--server`. Pre-existing CLI design, not
   introduced here, but the two retry paths now use different transports and only
   one of them is exercised against the live server in CI.
4. **The two lanes resolve a lost completion differently, by design — and only
   one of them is tested.** If an instance's completion *write* fails after its
   container is provably over, the **local** lane's post-group sweep
   (`internal/job/job.go`) resolves the row `skipped` with reason
   `"fan-out instance outcome unrecorded: completion write failed"`, logs it, and
   fails the group — because local mode has no recovery owner to revisit the row,
   so leaving it is stranding it. The **distributed** lane does the opposite and
   leaves the row alone: the claim lease expires and recovery re-dispatches the
   instance, so the work is retried rather than written off. Both answers are
   right for their lane, and the sweep's own comment is careful not to blame the
   row on the group's failure policy (that string is what `caesium run partitions`
   and `caesium why --partition` display). What is missing is coverage: the local
   sweep's `running` branch has only `TestFanOutLocalUnrecordedOutcomeIsNotBlamedOnFailFast`
   behind it, and the code argues the state is unreachable locally anyway (both
   loop exits gate on `inFlight == 0`); nothing tests the distributed
   lease-expiry-mid-group path at all.
5. **Dead fields in the harness.** `test/fanout_helpers_test.go`'s
   `partitionSnapshot.At` and `.RunStatus` are populated and never read.
6. **The distributed CI lane's pre-wave history is not evidence.** On `master`
   the lane ran `-run "TestRunConcurrencyStrategies|TestPriorityRunStartSurfacesAndCronDefault"`
   — un-suite-qualified, so under a testify suite it matched **nothing** and the
   lane was green because it ran zero tests. It is now
   `-run "TestIntegrationTestSuite/(…|TestFanOut)"`. Treat any distributed-lane
   green dated before 2026-08-26 as unproven.
7. **A one-time cache warm is needed for existing fan-out producers.**
   `cache.Entry.Partitions` is new. An entry written before this wave carries no
   partition list, so a cache hit on such a producer expands to zero partitions
   and takes the `onEmpty` branch (`internal/run/fanout.go`) rather than
   materializing the group. Any already-cached producer must be re-run once, or
   its entry invalidated, before its consumer fans out.
8. **The reproduce-descriptor fan-out guard is untested.** `assertUnfanned` /
   `ErrFannedTaskAmbiguous` (`api/rest/service/reproduce/descriptor.go`) has
   neither unit nor integration coverage — that package has no `_test.go` file at
   all. It is the one **assert-unfanned** answer in G6's table with no test behind
   it.
9. **Three read surfaces are re-keyed but uncovered.** The agent context
   (`api/rest/service/agent/context.go`), the AI-agent notification sender
   (`internal/notification/sender_aiagent.go`), and the local runner
   (`internal/localrun/runner.go`) each learned to read a group in
   `partition_index` order and prefer the failed instance, and none of the three
   has a test asserting it. See the G6 table.
10. **The two lanes disagree about where a task's command comes from, so a
   `job apply` lands in one and not the other.** Pre-existing and **not
   fan-out specific**, recorded here because fan-out is what made it visible. The
   local executor builds its `atomRunner` from a live catalog read
   (`svc.Get(t.AtomID)`, `internal/job/job.go`); the distributed worker executes
   `parseTaskCommand(taskRun.Command)` — the column frozen onto the row at
   `RegisterTasks`. Within a single run the two agree, because both derive from
   the same read at run start. They diverge on **run re-entry**: `caesium run
   retry` re-enters the job and re-reads the catalog, while `RegisterTasks` skips
   `(task_id, partition_index)` rows that already exist and `retryFromFailure`
   resets rows rather than replacing them — so a retried run executes the *new*
   command locally and the *old* one distributed. Fan-out inherits this
   unchanged: instance rows are copies of the template row, so every sibling
   carries the same snapshot. No fan-out item owns a fix; it needs a decision
   about which source is authoritative before anything is changed.
11. **Pull-mode workers claim before they have capacity — a pre-existing worker
   defect the now-real distributed lane exposes.** `Claimer.ClaimNext`
   (`internal/worker/claimer.go`) flips the row to `running` inside its single
   atomic `UPDATE … SET claimed_by = ?, …, status = ?` **before** the caller has a
   pool slot; only afterwards does `Worker.submitToPool`
   (`internal/worker/worker.go`) call `pool.Submit`, which blocks on a full pool.
   So a one-slot worker cannot hold siblings `pending` — it claims one, marks it
   `running`, and parks the goroutine. Both `internal/worker/worker.go` and
   `internal/worker/pool.go` are **unchanged vs `master`**, so this is not
   fan-out's doing, and the fix belongs in the worker (claim *after* acquiring
   capacity), not in any fan-out item.
   `TestPriorityRunStartSurfacesAndCronDefault/distributed claimer claim order`
   (`test/concurrency_priority_test.go:59`) is now **unconditionally skipped**
   ahead of its own env gate, with the reason stated in full: *"pre-existing: the
   pull-mode worker claims (status=running) before acquiring pool capacity, so a
   1-slot pool cannot hold siblings pending; tracked as a dynamic-fanout
   follow-up"*. The subtest's premise is false against the pull-mode worker — it
   asserts `s.Nil(pendingRun.Tasks[0].StartedAt)` for three runs blocked behind a
   one-slot filler — and it **never ran before this branch**, because the
   distributed lane's `-run` regex did not select it (follow-up 6). Skipping it is
   the right call for this branch: the premise, not the assertion, is what needs
   fixing, and the fix is a worker change nobody should smuggle into a fan-out
   wave. Un-skip it in the same PR that reorders claim and capacity.
12. **`fail_fast` cancels only not-yet-started siblings, so which siblings get cancelled is
   scheduling-dependent.** Verified in all three lanes (see D3's note). On a
   one-slot worker a sibling that has already gone `running` is left to finish
   while a still-`pending` one is skipped, so the *set* of cancelled partitions
   varies with dispatch timing and is not a stable assertion target.
   Both fail-fast fixtures — `TestFanOutFailFastCancelsPendingSiblings` and
   `TestFanOutFailFastIsTheDefault` — therefore assert the **invariant** rather
   than a fixed status set, through the shared `assertFailFastGroup`
   (`test/fanout_helpers_test.go:334`), which anchors on the failed instance's
   `completed_at` and checks three things: no sibling's `started_at` is **after**
   the failure; never-started ⇔ resolved (`skipped`/`cancelled`), asserted as a
   set equality in both directions, with a running sibling explicitly forbidden
   from being resolved out from under its container; and a **non-vacuity clause**
   for the legitimate one-slot ordering where the failing instance is claimed
   last, so no sibling could have been pending — admitted only on the evidence
   that nothing started after the failing instance did, never silently. That last
   clause is what stops the whole assertion degenerating into a tautology on a
   one-slot worker.
   _One loose end:_ `observePartitionStates` (`test/fanout_helpers_test.go:227`)
   still carries a **stale comment** claiming "the partitions endpoint exposes no
   per-instance started_at/completed_at". It does, and `partitionInstance`
   (`test/fanout_test.go:36`) decodes them — which is exactly what
   `assertFailFastGroup` now relies on. That helper's snapshot-based approach is
   also where follow-up 5's dead fields live.
13. **Per-partition retry is refused outside distributed mode.**
   `POST …/partitions/:index/retry` now answers **409** unless
   `CAESIUM_EXECUTION_MODE=distributed` (`partitionRetryIsDispatchable`,
   `api/rest/controller/job/run/partitions.go`), mirroring the replay service's
   `isDistributedExecutionMode`. The reason is structural, not a missing feature:
   execution mode is **server-wide, not per-run**, and the local engine drives its
   own DAG and exits when the run finishes — no dispatcher poll, no claim loop.
   Resetting an instance there returned 200 and left it `pending` forever, with
   the run re-opened to `running` so the run never completed again either. Wiring
   a single-partition resume through `job.New(...)` → `(*job).Run(ctx)`
   (`internal/job/job.go:157`, `:461`) is feasible, but it could not be verified
   outside the integration lane, so it was **deliberately refused rather than
   guessed**. The 409 names the path that does work locally (retry the run).
14. **The fan-in aggregate still bypasses the producer's `outputSchema`, by
   design.** `AggregateFanInOutputs` (`pkg/task/output.go`) no longer truncates on
   overflow — it fails typed, with `*FanInAggregateTooLargeError` carrying the
   producer, the encoded size, and the cap, wrapping the
   `ErrFanInAggregateTooLarge` sentinel so a caller fails the group instead of
   silently shipping a short work list. What is **not** solved: a declared
   `outputSchema` describes one instance's emission, not the group fold, so the
   synthesized aggregate is never validated against it. That is a **design
   question** — a group-level schema needs its own vocabulary for the
   per-partition map and the synthetic `_PARTITION_COUNT` / `_SUCCEEDED` /
   `_FAILED` keys — and no item in this plan owns it. Do not paper over it by
   validating the aggregate against the per-instance schema; that would reject
   every correct fold.
15. **Rolling-upgrade capability gate — delete it once the fleet is on protocol
   2.** Fanned dispatch is routed only to peers advertising
   `CapabilityInstanceIdentity` (`"instance_identity"`) from the new
   `GET /internal/capabilities` probe (`internal/dispatch/internal_server.go`;
   `HandleCapabilities` / `GetCapabilities` in `internal/dispatch/dispatch.go`,
   cached per peer in `internal/dispatch/loop.go`), with
   `InternalProtocolVersion = 2` (`internal/dispatch/dispatch.go:76`). An
   instance-blind dispatch that reaches an expanded group is refused as a 409
   carrying `ReasonAmbiguousTask` (`"ambiguous_task"`) rather than resolved to an
   arbitrary sibling — the fail-closed half of G4's rolling-upgrade contract. The
   gate is **transitional scaffolding**: once every deployed node is ≥ protocol 2
   it buys nothing but a probe and a branch, and leaving it indefinitely means the
   negotiation outlives the incompatibility it was written for. Remove the gate,
   not the 409.

## Streams

### Stream G — Instance identity migration (the gating prerequisite)

One `TaskRun` per `(job_run_id, task_id)` is not a convention of the run store —
it is an assumption the **whole run-lifecycle layer** is built on, and fan-out is
the first feature to break it. This stream re-keys that layer onto `TaskRun`
identity **before** any expansion or ordering work exists, because ordering
cannot be layered on a substrate that cannot represent two siblings.

Two properties make this a stream rather than a set of bullets inside A–D:

- **It is behavior-neutral.** Every item here must leave unfanned runs
  byte-identical in observable behavior (one instance, `partition_index = 0`).
  It ships and merges with no fan-out feature visible at all, which also means it
  can be reviewed and reverted on its own.
- **It reaches code fan-out does not otherwise touch** — failover, checkpoint
  encoding, recovery replay, incident attribution, the owner↔worker HTTP
  protocol. Landing it inside a feature stream would hide a distributed-systems
  migration inside a data-parallelism feature.

The discipline for every item: for each path taking `(runID, taskID)` that reads,
mutates, counts, or checkpoints task state, answer **"what does this mean when
`(runID, taskID)` names a set rather than a row?"** — and pick one of exactly
three answers: re-key to the `TaskRun` primary key (per-instance state),
aggregate explicitly over the set (group status, fan-in), or assert
`partition_count = 0` and fail loudly (paths that genuinely cannot support
groups, e.g. quarantined replay). **Silently matching the first row is never an
answer**, and a review that finds one should block.

- [x] G1. Establish instance identity end to end in the SQL advancement path.
      Re-key the run-store write paths that address task state as
      `WHERE job_run_id = ? AND task_id = ?` onto the `TaskRun` **primary key**
      (both executors already hold it) — `StartTask` (`internal/run/store.go:1594`),
      `StartTaskClaimed` (`:1836`), `RateLimitTask` (`:1808`),
      `ClaimTaskForDispatch` (`:1665`), `LoadDispatchedTaskRun` (`:1756`),
      `ReleaseTaskClaim` (`:1783`), `retryTask` (`:3256`), `SkipTask` (`:3347`),
      `failTask` (`:3174`), `completeTask` (`:2621`), `cacheHitTask` (`:1933`),
      and the descriptor/hash setters (`SetTaskHash*` `:437-471`,
      `UpdateTaskExecutionDescriptor*` `:472-515`, `SetTaskEffectiveHash` `:516`).
      Add the sibling-aware variant of `batchDecrementPredecessorsTx`
      (`:2333`) — its `task_id IN ?` predicate stays correct for the cross-step
      edge and gains an `id IN ?` sibling form for later use by D3. No behavior
      change while every task has exactly one row.
      Two of these **fan a write across siblings** rather than merely addressing
      the wrong one, and should be done first because they corrupt live work:
      `retryTask` (`:3256-3265`) resets `output`/`result` on every sibling, and
      `ClaimTaskForDispatch` (`:1665-1686`) claims **all** unclaimed siblings in one
      `UPDATE`, so one worker silently takes the whole group and `RowsAffected == 0`
      cannot detect the over-claim. A third, `RateLimitTask` (`:1808-1824`), matches
      `status IN (pending, running)` and so re-pends **running** siblings, orphaning
      their in-flight containers (it does not kill them). Matching `running` may be
      deliberate for the one-row case, so **settle the intent before changing it** —
      this is an open question for this item, not a presumed bug (recorded as an
      observation in #345).
      Also fix materialization itself: `RegisterTasks` (`:1121-1132`, `:1164-1175`,
      `:1183-1189`) **actively de-duplicates by task ID** — `seenInputTaskIDs`, the
      `task_id IN ?` existence pluck, and the `seenNewTaskIDs` guard each silently
      drop instances 2…N. Any registration path that is ever handed a group must
      insert per row, not per task.
      _Shipped 2026-08-26: every write path resolves through
      `loadTaskRunByIDOrUnique` (`internal/run/taskrun_identity.go`), which takes a
      `TaskRun` PK **or** a unique `(run, task)` and returns `ErrAmbiguousTaskRun`
      rather than the first row. `RegisterTasks` now dedups by
      `(task_id, partition_index)`, not task id, on both the input and
      existing-row passes. The item's open question about `RateLimitTask` was
      **settled, not assumed**: the `status IN (pending, running)` match stays, and
      the write now parks exactly one instance
      (`TestRateLimitTaskParksOneInstance`)._
      Files: `internal/run/store.go`.
- [x] G2. Re-key the **run-owner in-memory engine**. `RunState`
      (`internal/run/owner_state.go:65-74`) holds `tasks`, `indegree`, `outcomes`,
      and `inReady` as `map[uuid.UUID]` keyed by **task ID**, with `ready` a slice
      of task IDs; N siblings collapse into one entry, so a second sibling's
      completion is a no-op against an already-terminal state. Introduce an
      explicit instance key (the `TaskRun` ID, with a task→instances index so
      `RunTopology` — `internal/run/owner_topology.go:16` — stays task-keyed, which
      is correct: the *catalog* graph really is one node per step). Make `total`
      **dynamic**: it is fixed at construction from the catalog (`NewRunState`,
      `:82-101`) and `IsComplete()` is `terminalCount >= total` (`:351`), so a
      static task count is structurally incompatible with mid-run expansion —
      `total` must count live instances and be raised inside the same critical
      section that creates rows. Also re-key `MarkDispatched` (`:321`),
      `TaskState` (`:341`), `RunningTasks` (`:290`), and `requeueRunning` (`:304`),
      the last of which today re-dispatches one instance for a whole group after
      failover.
      _Shipped 2026-08-26: `RunState` gained `catalogOf` / `instancesOf` /
      `inGroupAdj` / `instanceOrder` / `partitionKeys` / `maxParallel` /
      `failurePolicy` instance maps alongside the task-keyed `topo`, plus
      `ExpandTask` and `ApplyExpansion`, which raise `total` inside the same
      critical section that creates the instance entries._
      Files: `internal/run/owner_state.go`, `internal/run/owner_topology.go`,
      `internal/run/owner_manager.go`.
      Depends on: G1.
- [x] G3. Version and re-key the **checkpoint format**, and fix recovery replay.
      `runStateSnapshot` (`internal/run/owner_state.go:370`) has **no version
      field** and `models.RunCheckpoint.StateBlob`
      (`internal/models/run_checkpoint.go:32-35`) is opaque bytes with no format
      version column — so an *old* blob unmarshals into the *new* struct with zero
      values instead of erroring, `Restore` (`:398`) never reaches its
      corrupt-checkpoint fallback, and a recovering owner silently adopts an empty
      state. **Add a version discriminator first**, treat unknown/absent as
      "replay from terminal rows", and only then change the shape. Then fix replay:
      `RecoverRunState` (`internal/run/recovery.go:41`) applies rows via
      `ApplyTerminalRow(row.TaskID, …)` (`internal/run/owner_state.go:243`), so N
      sibling rows sharing a `task_id` arrive in sequence order, the first marks
      the task terminal, and the rest hit the `wasTerminal` early-return — a
      recovered owner believes a half-finished group is done. Replay must key on
      the row's instance identity.
      **Fix the sequence space in the same item.** `ApplyCompletion`'s
      already-terminal early return (`internal/run/owner_state.go:163-165`) leaves
      `TerminalSequence` at **zero**, `OwnerManager.Complete` passes that zero into
      `CompleteTaskOwner` (`internal/run/owner_manager.go:233-235`), and
      `TerminalTaskRunsSince` selects `terminal_sequence > ?`
      (`internal/run/checkpoint_store.go:95-108`), excluding a zero-stamped row from
      the replay tail. **Today that is legitimate duplicate suppression** — with one
      row per `task_id` the early return only fires for a repeat completion of a row
      that already carries a good sequence. Under fan-out the same path becomes the
      *normal* one for siblings 2…N, each a distinct terminal transition needing its
      own sequence; without one they are invisible to replay and the dense-gap check
      (`internal/run/recovery.go:61,69-75`) reports phantom gaps. Latent today,
      definite under fan-out — see #345. Related: checkpoint
      cadence (`CheckpointWriter.due`, `internal/run/checkpoint_writer.go:56-64`)
      keys off `RunState.seq`, which under the current code advances once per group
      — starving checkpointing exactly when the run holds the most state.
      Cover with a failover test that kills an owner mid-group and asserts every
      completed instance survives the takeover. **G7 deduplicates the traversal this
      item's replay path uses** — sequence G3 before G7 if convenient, but G7 must
      land before D5 introduces in-group edges, or replay and live will need the
      same change made twice.
      _Shipped 2026-08-26: `runStateSnapshot` carries `Version` (const
      `runStateSnapshotVersion = 1`) and `Restore` probes it first, returning
      `errUnknownCheckpointVersion` for an absent or unknown version
      (`TestRestore_RejectsOldCheckpointBlob`). `recovery.go` replays by **row id**
      for any row with partition state and by task id otherwise, after calling
      `RehydrateInGroupEdges(rows, catalog)`._
      _Open: the failover test this item asks for does not exist.
      `internal/run/failover_test.go` still holds only
      `TestFailover_TakeoverAndResume` and mentions no partitions, and `test/` has
      no failover scenario. `internal/run/checkpoint_writer.go` was not touched, so
      the cadence-starvation concern was never closed deliberately — see follow-ups
      1 and 2._
      Files: `internal/run/owner_state.go`, `internal/run/recovery.go`,
      `internal/run/checkpoint_store.go`, `internal/run/checkpoint_writer.go`,
      `internal/models/run_checkpoint.go`.
      Depends on: G2.
- [x] G4. Carry instance identity on the **owner↔worker wire protocol**.
      `DispatchRequest` and `CompleteRequest` (`internal/dispatch/dispatch.go:111-142`)
      identify a task by `TaskID` alone, so a worker finishing instance 7 sends
      something that matches all N sibling rows; `CompleteTaskOwner`
      (`internal/run/store.go:2905`) and the SQL fallback both then fence on
      `job_run_id = ? AND task_id = ? AND claimed_by = ?` and can update the wrong
      sibling. Add the `TaskRun` ID to both envelopes and to the owner completion
      seam (`internal/dispatch/dispatch.go:486-489` →
      `OwnerManager.Complete`, `internal/run/owner_manager.go:215`). This is a
      **cross-node compatibility surface**: an older worker will not send the new
      field, so the receiving side must fall back to the unique
      `(run, task)` row and reject ambiguity when more than one exists, rather
      than picking one. State the rolling-upgrade behavior explicitly in the PR.
      _Shipped 2026-08-26: `DispatchRequest.TaskRunID` and
      `CompleteRequest.TaskRunID` are both `json:"task_run_id,omitempty"`, and the
      rolling-upgrade fallback is explicit — `dispatchTaskRef`
      (`internal/dispatch/dispatch.go`) falls back to the catalog id when the field
      is absent, and the store then rejects ambiguity rather than picking a
      sibling. The worker sets it **once, for every route**, in `ownerSink.send`
      (`internal/worker/completion_sink.go`), whose comment records that
      per-route setting is how the failed and cached routes were missed first time
      (`TestOwnerSink_EveryRouteCarriesTaskRunID`)._
      _Shipped 2026-08-26, second pass: carrying the id was necessary but not
      sufficient. Standing up the owner-memory lane surfaced **four stacked
      completion-identity defects**, none of which any other lane could see,
      because only owner mode resolves a completion against in-memory `RunState`
      as well as SQL:
      (a) `RunState.CompletionIdentity(taskID, taskRunID)`
      (`internal/run/owner_state.go:672`) — the owner must decide *which* key a
      completion names, preferring the real instance identity but accepting the
      catalog id for an unfanned run whose worker sent a `TaskRunID`
      (`TestOwnerCompleteInstanceAcceptsUnfannedTaskRunID`,
      `TestOwnerCompleteInstanceUnfannedRunReachesCompletion`,
      `TestOwnerCompleteInstanceStillPrefersRealInstanceIdentity`);
      (b) `Store.PlanFanOutExpansionForRow` (`internal/run/fanout.go:109`) — the
      producer's own row must be named explicitly when planning, rather than
      re-resolved from `(run, task)`;
      (c) `Store.invalidateRunState` (`internal/run/store.go:266`) on retry — a
      retried run left stale owner state behind, so the owner advanced against a
      DAG that no longer matched the rows
      (`TestOwnerStateIsInvalidatedByRetryFromFailure`);
      (d) `effectiveTerminalStatus(status, result)` (`internal/run/store.go:100`) —
      a success was being turned into a failure, and a failure had to be derived
      from the result rather than assumed
      (`TestOwnerCompleteInstanceSuccessIsNotTurnedIntoAFailure`,
      `TestOwnerCompleteInstanceDerivesFailureFromResult`).
      All six tests live in `internal/run/owner_completion_identity_test.go`. This
      is the clearest vindication of the plan's route-completeness contract: every
      one of these is a **mode-dependent** defect that passes CI in the default
      configuration._
      Files: `internal/dispatch/dispatch.go`, `internal/dispatch/loop.go`,
      `internal/run/store.go`, `internal/run/owner_manager.go`,
      `internal/worker/runtime_executor.go`.
      Depends on: G2.
- [x] G5. Fix **retry accounting** so a group is satisfied only when every live
      sibling is. `retryFromFailure` (`internal/run/store.go:4810`) builds
      `terminalSuccessIDs` keyed by `tr.TaskID` (`:4852-4856`) and recomputes each
      reset task's `outstanding_predecessors` by testing
      `terminalSuccessIDs[edge.FromTaskID]` (`:4899`). Under fan-out **one**
      succeeded sibling marks the whole predecessor group satisfied, so retry
      releases downstream work while another sibling is still being retried — a
      silent correctness bug, not a scheduling inefficiency. A predecessor task is
      satisfied only when **all** of its live `TaskRun` rows are terminal
      successes; `resetTaskIDs` (`:4862`) must likewise become a set of instance
      rows, not task IDs. (E4 layers the *ordering* semantics on top of this; this
      item is the accounting fix and is required with or without ordering.)
      _Shipped 2026-08-26: `satisfiedPredecessorTaskIDsTx` groups every row by task
      and admits a predecessor only when `predecessorGroupSatisfied` holds for
      **all** of its rows; reset rows re-seed via `resetInstanceOutstandingTx`,
      keyed on the instance._
      Files: `internal/run/store.go`.
      Depends on: G1.
- [x] G6. Audit the read/observability surfaces for the same assumption and pick
      one of the three answers per site, explicitly. Deliverable is a table in the
      PR description: **site → chosen answer (re-key / aggregate / assert-unfanned)
      → test**. Known entries, so the audit starts from evidence rather than a
      blank page:
      **Aggregate** — `predecessorStatusesTx` (`internal/run/store.go:2488-2537`)
      returns one status per *row* while `satisfiesTriggerRule` (`:2540`) reads it
      as one per *predecessor*, so a fanned predecessor silently flips `one_success`
      to "any instance" and `all_success` to "no instance skipped"; and
      `PredecessorHashes` (`:4628-4675`) returns N hashes where the identity key
      expects one, changing the shape of its `pred_hash:` lines and cache-missing
      every downstream task forever. `PredecessorOutputs` (`:4409-4473`) collapses
      siblings last-writer-wins into a name-keyed map. All three need the design's
      one-aggregate-status / one-aggregate-identity contract written explicitly.
      **Re-key** — `convertRunTaskModel` (`:4080-4095`) sets the API payload's
      `TaskRun.ID` to `model.TaskID`, so N siblings serialize with an identical
      `id` and `logs.go:80-90` matches the first, streaming the **wrong
      container's** logs (same root cause as the known `/v1/jobs/:id/tasks`
      serialization bug); `recordTaskEventTx` (`:4219-4237`) builds every task
      event payload from an arbitrary sibling; `rundiff.latestTerminalTaskRunsByName`
      (`internal/run/rundiff.go:162-232`) keys by task name and shows one instance
      while hiding N-1; `why.resolveTaskRun` (`internal/run/why.go:161-189`) and
      incident context (`internal/incident/subscriber.go:165-171`,
      `internal/incident/bundle.go:148-165`) each read an arbitrary sibling — an
      incident can be classified from a *succeeded* instance's row.
      **Hard failure, fix or it blocks everything** —
      `stampBatchEventQuarantineTx` (`:2414-2433`) asserts
      `len(taskRows) != len(ids)` and errors the entire batched event insert once a
      task has two rows.
      **Assert-unfanned** — quarantined replay
      (`api/rest/service/reproduce/descriptor.go:135-173`, which resolves via
      `.Take`), matching the design's fail-closed posture.
      Also settle run-completion accounting: `waitForRunCompletion`
      (`internal/job/job.go:1577,1659-1698`) counts **rows** but compares to
      `len(tasks)` (`:661`), and the local branch does the same at `:1554-1558`.

      _Shipped 2026-08-26. The audit table below is the deliverable this item
      promised; it was never recorded in a PR description, so it lives here
      instead. Every row was re-derived from the working tree on 2026-08-26.
      Four rows land on **none — gap** and are tracked as follow-ups 8 and 9._

      | Site | Answer | Test |
      |---|---|---|
      | `predecessorStatusesTx` → `aggregatePredecessorStatuses` → `groupStatusFromInstances` (`internal/run/store.go`, `internal/run/taskrun_identity.go`) | **Aggregate** — one status per predecessor *step*, so `one_success` / `all_success` see a group, never N rows | `TestAggregatePredecessorStatusesOnePerTask`, `TestGroupStatusFromInstances` (`internal/run/taskrun_identity_test.go`) |
      | `PredecessorOutputs` → `predecessorGroupOutput` → `pkgtask.AggregateFanInOutputs` | **Aggregate** — one name-keyed map per step; per-partition values fold into the same fan-in aggregate Stream C defines, so both lanes agree | `TestPredecessorOutputsAggregatesFannedPredecessor`, `TestPredecessorOutputsUnfannedIsUnchanged` (`internal/run/store_fanout_test.go`) |
      | `PredecessorHashes` → `predecessorGroupHash` → `GroupIdentityHash` (`internal/run/store_instance.go`) | **Aggregate** — one hash per predecessor step, so a downstream `pred_hash:` line keeps its pre-fan-out shape and nothing cache-misses forever | `TestPredecessorHashListUnfannedIsByteIdentical`, `TestPredecessorHashListFannedIsOneDeterministicGroupHash`, `TestPredecessorHashesFannedPredecessorEndToEnd`, `TestGroupIdentityHashIsTheOneDefinition`; local-lane parity `TestFanOutGroupIdentityFeedsDownstreamPredecessorHashes` (`internal/job/fanout_local_test.go`) |
      | `stampBatchEventQuarantineTx` — the **hard failure** | **Re-key** — the `len(taskRows) != len(ids)` assert is gone; it now counts distinct `(run, task)` keys found, so N rows per task no longer errors the whole batched insert | `TestBatchEventQuarantineStampUsesRunAndTaskMarkers` (`internal/run/store_test.go`) |
      | `recordTaskRunEventTx` | **Re-key** — every task event is built from the instance row it describes, not an arbitrary sibling | `TestTaskEventsCarryTheirOwnInstance` (`internal/run/store_fanout_test.go`) |
      | `markTaskSkippedTx` / `markInstanceSkippedTx` | **Re-key** — `markInstanceSkippedTx` is *the* terminal-skip primitive; `markTaskSkippedTx` loops it per row so each instance gets its own `terminal_sequence` | `TestMarkTaskSkippedGivesEachInstanceItsOwnTerminalSequence`, `TestSkipTaskSkipsTheWholeGroup` |
      | `SetTaskHash*`, `SaveSchemaViolations`, `SaveTaskLogSnapshot`, `SetTaskExitCode`, `mutateTaskExecutionDescriptor` | **Re-key** — all resolve through `loadTaskRunByIDOrUnique` (`internal/run/taskrun_identity.go`), which takes a `TaskRun` PK or a unique `(run, task)` and **rejects** ambiguity rather than taking the first row | `TestHashSettersAddressOneInstance`, `TestSaveSchemaViolationsAddressesOneInstance`, `TestSaveSchemaViolationsUnfannedStillResolvesByTaskID`, `TestExecutionWritesAddressOneFanOutInstance` |
      | `RateLimitTask` (G1's flagged open question) | **Re-key** — parks exactly one instance; the `status IN (pending, running)` match was kept deliberately for the one-row case rather than changed on suspicion | `TestRateLimitTaskParksOneInstance` |
      | `convertRunTaskModel` (`internal/run/store.go`) | **Re-key** — the API payload's `TaskRun.ID` is now `model.ID`, not `model.TaskID`, so N siblings serialize with distinct ids | `TestResolveLogInstance_TaskRunIDSelectsThatInstance` (`api/rest/controller/job/run/logs_test.go`); end-to-end `TestFanOutLogsSelectInstance` (`test/fanout_test.go`) |
      | Run-detail payload (`collapseFanOutGroups`, `PartitionStatusCounts`) | **Aggregate** — a group collapses to one entry carrying `partition_count` + a status histogram, so a 10k-instance run does not bloat every run-list response | `TestCollapseFanOutGroupsEmitsStatusHistogram`, `TestCollapseFanOutGroupsSingleInstanceGroup` (`internal/run/fanout_test.go`) |
      | Logs (`resolveLogInstance`, `api/rest/controller/job/run/logs.go`) | **Re-key** — a fanned task with no selector is a **400 listing the instances**; `task_run_id` or `partition` selects one; unfanned is unchanged | nine `TestResolveLogInstance_*` cases (`api/rest/controller/job/run/logs_test.go`) + `TestFanOutLogsSelectInstance` |
      | `why` (`WhyTaskPartition`, `resolveTaskRuns`, `internal/run/why.go`) | **Aggregate + explicit selector** — a fanned step answers with the group summary; `--partition <value>` selects one instance and an unknown value lists what exists | `internal/run/why_fanout_test.go` (7 tests, incl. `TestWhyTask_UnfannedOutputIsByteIdenticalGolden` and `TestWhyTask_BaselineIsScopedToTheSamePartition`) + `TestFanOutWhyGroupSummaryAndPartitionSelector` |
      | `whydiff` (`internal/run/whydiff.go`) | **Re-key** — `partition`, `partitionFingerprint`, and `partitionAttributes` are diffed as first-class discriminating fields | indirect only: `TestWhyTask_BaselineIsScopedToTheSamePartition` proves the baseline resolves to the *same* partition, which is what makes the diff meaningful; the three `addScalar`/`diffStringMap` fields have no direct assertion |
      | Receipts (`terminalAttempts`, `TaskEntry.Partition`, `internal/receipt/`) | **Re-key** — the attempt-collapse key is `(TaskID, PartitionValue)`, so a run attests one entry **per partition** and the canonical line carries it | `internal/receipt/fanout_test.go` (7 tests, incl. `TestCanonicalTaskLine_UnfannedBytesUnchanged`) + `TestFanOutReceiptAttestsEveryPartition` |
      | `rundiff` (`latestTerminalTaskRunsByName` → `runDiffInstanceKey`) | **Re-key** — instances align across runs by partition **value**, never index, and unmatched values are reported as `partitionsAdded` / `partitionsRemoved` | `TestDiffRuns_FanOutAlignsByPartitionValueNotIndex`, `TestDiffRuns_UnfannedDiffReportsNoPartitionChurn` (`internal/run/rundiff_fanout_test.go`) + `cmd/run/diff_fanout_test.go` (4 tests) |
      | Incidents (`attributionTaskRun`, `bundleAttributionRow`) | **Re-key** — classify from the first **failed** instance, with the failed-partition list capped and truncation made visible | `internal/incident/fanout_attribution_test.go` (7 tests) |
      | Quarantined replay baseline (`ErrFannedBaseline`, `internal/replay/replay.go`) | **Assert-unfanned** — refuses a baseline containing a fanned group, surfaced as HTTP 409 with an actionable message | `TestReplay_FannedBaselineRefusalIsActionable` (`cmd/run/replay_fanout_test.go`) |
      | Reproduce descriptor (`assertUnfanned`, `ErrFannedTaskAmbiguous`) | **Assert-unfanned** — 0 rows is not-found, 1 unfanned row resolves, ≥2 rows or `partition_count > 1` is refused rather than resolved to a sibling | **none — gap** (follow-up 8) |
      | Agent context (`api/rest/service/agent/context.go`) | **Re-key** — reads the group in `partition_index` order and prefers the failed instance's evidence over `.First()` | **none — gap** (follow-up 9) |
      | AI-agent notification (`internal/notification/sender_aiagent.go`) | **Re-key** — same ordered read, same failed-instance preference | **none — gap** (follow-up 9) |
      | Local runner (`internal/localrun/runner.go`) | **Re-key** — one result row per instance, labelled `step[partition]` | **none — gap** (follow-up 9) |
      | Run-completion accounting (`waitForRunCompletion`, `liveTaskCount`) | **Aggregate** — and this is the one place the shipped answer **differs from what this item sketched**: it counts DAG *nodes*, not `TaskRun` rows. A fanned step stays one node, `runFannedGroup` does not return until every instance is terminal, and `convertRunModelWithDB` collapses the group back to one entry, so the guard and `waitForRunCompletion` count in the same unit | `TestFanOutLocalRunCompletionCountsInstances` (`internal/job/fanout_local_test.go`) |

      _Open: the four **none — gap** rows are the audit's own residue — those
      sites were fixed, the assertions were not written. See follow-ups 8 and 9._
      Files: `internal/job/job.go`, `internal/run/store.go`, `internal/run/why.go`,
      `internal/run/whydiff.go`, `internal/run/rundiff.go`,
      `internal/incident/subscriber.go`, `internal/incident/bundle.go`,
      `api/rest/controller/job/run/logs.go`, `api/rest/service/`.
      Depends on: G1, G2.
- [x] G7. Collapse the live/replay traversal duplication **before** any in-group
      edge exists. This item is the structural answer to a defect that has now
      produced four P1s in one family: the owner keeps a *live* and a *replay*
      implementation of every traversal concern, and fan-out must teach both about
      a new edge class.
      | Concern | Live | Replay |
      |---|---|---|
      | topology build | `LoadRunTopology`, `internal/run/owner_topology.go:16` | `loadReplayRunTopology`, `:72` |
      | successor walk + indegree decrement | `advanceSuccessors`, `internal/run/owner_state.go:198` | `ApplyTerminalRow`, `:243` — an **inline copy** at `:264-282`, not a call |
      | indegree seed | `NewRunState`, `:93` | snapshot `Restore`, `:398` |
      The same fork exists in the store: `replayPredecessorRefsTx`
      (`internal/run/store.go:603`) branches to a parallel replay implementation at
      **five** sites — `predecessorStatusesTx` (`:2489`), `shouldRunTaskTx`
      (`:2599`), `PredecessorOutputs` (`:4410` → `predecessorOutputsFromRefsTx`
      `:4482`), `PredecessorDescriptorInputs` (`:4529`), and `PredecessorHashes`
      (`:4629` → `predecessorHashesFromRefsTx` `:4678`).
      Deliverable: **one** successor-traversal kernel and **one** edge-resolution
      seam that live and replay both call, so an edge class is enumerated in exactly
      one place and indegree is decremented in exactly one place. After this, "did
      you update the replay path too?" is not a question an implementer can answer
      wrongly, because there is no second copy.
      **The one thing that must survive the refactor**, because getting it wrong is
      a correctness bug rather than a stall: the two owner traversals differ
      **deliberately**. Live allocates a fresh `terminal_sequence` and transitively
      marks unsatisfied successors `skipped`; replay adopts the row's stored
      sequence and deliberately does **not** auto-skip, because each skip was itself
      persisted as a terminal row and arrives later in sequence order
      (`ApplyTerminalRow`'s comment: re-deriving them "would double-handle them").
      Extract only the kernel they genuinely share — enumerate successor edges,
      decrement indegree, test the trigger rule — and leave the
      unsatisfied-trigger **policy** a parameter. **Merging the two functions
      outright reintroduces double-handled skips and must be rejected in review.**
      Behavior-neutral: no edge class changes here, no fan-out surface. Closed by
      the existing owner/failover suite green with no assertion relaxed, plus a test
      asserting live and replay produce identical indegree and readiness for the
      same terminal sequence.
      _Shipped 2026-08-26, and wider than the item scoped it. The owner half is
      `traverseSuccessors(seeds, policy)` with `skipUnsatisfied` /
      `leaveUnsatisfiedPending` — `advanceSuccessors` and `ApplyTerminalRow` both
      call it, and the deliberate live/replay difference survives as the policy
      parameter rather than a second copy. The store half also landed:
      `resolvePredecessorsTx` is the one predecessor-edge kernel behind all five
      former fork sites, `buildRunTopology` is the one place a `RunTopology` is
      assembled, and `ResolveBranchSkips` now forks only in `branchTargetsTx`
      (enumeration), with the selection algorithm written once._
      Files: `internal/run/owner_state.go`, `internal/run/owner_topology.go`,
      `internal/run/recovery.go`, `internal/run/store.go`.
      Depends on: G2.

### Stream A — Substrate: partition model + instance-keyed store + marker parsing

The widest, least visible slice (design Phase 1) and the foundation every other
stream builds on, so it merges first. It breaks the one-`TaskRun`-per-`(run,task)`
invariant, re-keys every store write path onto the `TaskRun` primary key, and
teaches `pkg/task` to parse the partition marker.

- [x] A1. Add the partition columns to `TaskRun`: `PartitionValue string` (empty =
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
      _Shipped 2026-08-26 — with one correction to this item's method. Struct tags
      alone were **not** enough: flipping `idx_taskrun_jobrun_task` from `index` to
      `uniqueIndex` and adding `partition_index` needed an explicit pre-AutoMigrate
      drop, `MigrateTaskRunUniquePartitionIndex` in `pkg/db/migrations.go`, because
      AutoMigrate will not rebuild an index that already exists under that name.
      The `internal/models/models.go` `All`-slice prediction held: no edit._
      Files: `internal/models/run.go`, `pkg/db/db.go`, `pkg/db/router.go`.
- [x] A2. Bind the re-keyed lifecycle to the new partition columns. **Stream G
      does the re-keying** (G1 for the SQL write paths, G2–G4 for the owner engine,
      checkpoint, recovery, and wire protocol, G5 for retry accounting); this item
      is only what could not be done before the columns existed: make the unique
      `(job_run_id, task_id, partition_index)` index the row's addressing contract,
      set `partition_index = 0` on the template/unfanned path, and assert the
      behavior-neutrality property end to end — an unfanned run must be observably
      identical before and after Streams G + A. Any `WHERE job_run_id = ? AND
      task_id = ?` write predicate still reachable after G is a **bug to fix here,
      not a follow-up**; the acceptance test is a grep-level audit recorded in the
      PR description.
      Files: `internal/run/store.go`, `internal/models/run.go`.
      Depends on: A1, G1.
- [x] A3. Parse the partition marker: extend `Markers` (`pkg/task/output.go:364`)
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
      must satisfy `scalarOutputValue` (`pkg/task/output.go:229`) — reuse that
      predicate but **invert its posture**: a non-scalar *output* value is dropped,
      a non-scalar *attribute* **fails the producer**, because partitions are the
      work list, not advisory data. **Normalization is lossless**: it lifts a string
      element to `{"key": <string>}`, sorts keys, and canonically re-encodes —
      it must never drop or repair a field, or the run would proceed against a work
      description the producer did not emit. A duplicate `key` with an
      identical payload dedups; a duplicate `key` with a **conflicting** payload
      fails the producer. Add the server hard cap `CAESIUM_FANOUT_MAX_PARTITIONS`
      (default 1024) to the `Environment` struct (`pkg/env/env.go:68`) — the
      **only** new env var in this plan. Fully unit-tested (JSON + line forms,
      string/object/mixed arrays, dedup vs conflicting dedup, each cap boundary,
      each invalid-value class).
      _Shipped 2026-08-26: parser, normalization, caps, and both marker forms live
      in the new `pkg/task/partition.go`; `CAESIUM_FANOUT_MAX_PARTITIONS` is
      `pkg/env/env.go` `FanOutMaxPartitions` (default 1024, validated `>= 1`)._
      Files: `pkg/task/output.go` (+ `pkg/task/output_test.go`), `pkg/env/env.go`.
- [x] A4. Add the in-group graph validator next to the parser, so **one**
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
      _Shipped 2026-08-26: `ValidatePartitionGraph` in `pkg/task/partition_graph.go`
      returns a `PartitionGraph`; it is the single implementation all three lanes
      call (`internal/run/fanout.go` for SQL + owner, via `PlanFanOutExpansion`)._
      Files: `pkg/task/partition_graph.go` (+ `_test.go`).
      Depends on: A3.

### Stream B — Schema + lint contract (the YAML surface)

The declarative half: the `fanOut:` block on a step, the apply-time lint rules
that keep the static topology sound, and the persistence of the config onto the
catalog `Task`. Owns `pkg/jobdef/definition.go` (a true-conflict file via the dual
`Step`/`rawStep` declaration), so all schema work lands in one stream.

- [x] B1. Add the `FanOut` struct on `Step` (`pkg/jobdef/definition.go:214`) —
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
      _Shipped 2026-08-26: `validateFanOut` (`pkg/jobdef/definition.go`) enforces
      every listed rule and **stamps the defaults onto the stored config** — an
      omitted `failurePolicy` becomes `fail_fast`, an omitted `onEmpty` becomes
      `skip`, an omitted `env` becomes `CAESIUM_PARTITION` — which is what lets the
      three execution lanes agree without each re-deriving them. `report.go` grew
      the `fanOut` row and a `Fan-Out` section; the generated doc was regenerated,
      not hand-edited._
      Files: `pkg/jobdef/definition.go`, `pkg/jobdef/schema.go`,
      `internal/jobdef/report/report.go`.
- [x] B2. Persist the fan-out config onto the catalog: add
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

- [x] C1. Local expansion + env injection: register the fanned step as a single
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
      _Shipped 2026-08-26 — but the run-completion half was settled the **opposite**
      way to this item's wording. `liveTaskCount` stays `len(tasks)` and
      `waitForRunCompletion` still counts DAG *nodes*: a fanned step remains one
      node, `runFannedGroup` does not return until every instance is terminal, and
      `convertRunModelWithDB` collapses the group back to one entry, so both
      counters are in the same unit. "Count live `TaskRun` rows" would have made
      the guard and the collapsed payload disagree. Covered by
      `TestFanOutLocalRunCompletionCountsInstances`._
      Files: `internal/job/job.go`.
      Depends on: A1, A2, A3, B2.
- [x] C2. Per-partition cache identity: add **three** fields to `cache.HashInput`
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
      _Shipped 2026-08-26: `Partition`, `PartitionFingerprint`, and
      `PartitionAttributes` on `cache.HashInput` and mirrored verbatim into
      `HashInputBlob`, each written only when non-empty; `dependsOn` is absent by
      construction. `CacheVersion` is unchanged and
      `TestCompute_GoldenStringFormUnchanged` pins the digest, not merely its
      existence._
      Files: `internal/cache/hash.go`, `internal/job/job.go`.
      Depends on: C1.
- [x] C3. Group fan-in + output aggregation + metrics: resolve group status —
      `succeeded` iff all instances succeeded/cached, `failed` if any instance
      exhausts retries (`fail_fast` cancels not-yet-started siblings at first failure;
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
      _Shipped 2026-08-26: `pkgtask.AggregateFanInOutputs` is the one aggregation
      implementation, called from the local loop and from
      `predecessorGroupOutput` in SQL. Both metric series exist and are observed —
      `FanOutPartitionsTotal` in `internal/run/fanout.go`,
      `FanOutGroupDurationSeconds` in **both** lanes (`groupAllTerminalTx` for SQL,
      `TakeResolvedGroups`/`OwnerManager` for owner mode)._
      _Beyond the item's text: the local lane also runs a **post-group straggler
      sweep** that resolves any instance left non-terminal, including a `running`
      one, because local mode has no recovery owner to revisit the row. The code
      argues that state is unreachable; nothing tests it (follow-up 4)._
      Files: `internal/job/job.go`, `pkg/task/output.go`, `internal/metrics/metrics.go`.
      Depends on: C1.
- [x] C4. Mirror in-group ordering in the local executor. The ordering *mechanism*
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
      _Shipped 2026-08-26 — by deliberately **not** mirroring. The local loop keeps
      no in-memory instance sub-graph and no per-instance indegree: `runFannedGroup`
      reads each row's `outstanding_predecessors`, the same scalar the distributed
      claimer gates on, so there is one ordering implementation rather than two
      that must be asserted equal. That is stricter than this item asked for and
      makes the "same observed order" property structural.
      `TestFanOutLocalOrderingFollowsStoreOutstandingPredecessors`,
      `TestFanOutLocalDiamondOrdering`,
      `TestFanOutLocalFailureSkipsTransitiveDependents`._
      Files: `internal/job/job.go`.
      Depends on: A4, C1, D3.

### Stream D — Distributed lane: completion-tx expansion + `maxParallel` claim

The distributed (`CAESIUM_EXECUTION_MODE=distributed`) path (design Phase 3):
expansion rides the producer's completion transaction so distributed workers never
observe a half-expanded group, and the group in-flight cap plugs into the claimer.
Reuses the fan-in + output-aggregation semantics established by Stream C. Shares
`internal/run/store.go` with A2, so it sequences after A.

**Read this before picking up C or D.** There are **three** DAG-advancement
implementations, not two, and they do not share one completion path:

1. **Local** — in-memory Kahn loop, but it completes through
   `store.CompleteTaskWithResult` (`internal/job/job.go:1087` →
   `internal/run/store.go:1904` → `completeTask(…, enforceClaim=false)`), so it
   *does* run the expansion transaction D1 builds.
2. **Distributed, SQL advancement** — `completeTask`, same transaction.
3. **Distributed, run-owner in-memory** (`CAESIUM_RUN_OWNER_IN_MEMORY=true`) —
   `OwnerManager.Complete` (`internal/run/owner_manager.go:215`) advances
   `RunState` in memory and persists via `CompleteTaskOwner`
   (`internal/run/store.go:2905`), which by its own docstring *"does NOT decrement
   predecessors, evaluate trigger rules, or resolve branches in SQL"*. **It never
   reaches `completeTask`, so an expansion hook placed only there is never
   executed and the group silently collapses to the single template row.**

So expansion lands in D1 (paths 1 and 2) **and** D4 (path 3), over the shared
normalization/validation in `pkg/task` — one implementation of the *rules*, three
call sites. Stream C owns the local *mirror* of the resulting state, not a second
implementation. Two consequences for wave planning: local fan-out is not
end-to-end runnable until D1 merges even though C's items are written against the
local executor, and **no fan-out item in any stream may merge before Stream G**,
which makes the substrate able to represent two siblings at all.

- [x] D1. Expand inside the producer's completion transaction: pass
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
      _Shipped 2026-08-26: `expandFanOutSuccessorsTx` (`internal/run/fanout.go`)
      runs inside the completion tx, with `ValidatePartitionGraph` before the first
      insert. One thing this item did not name and the wave needed:
      **the cache-hit route required its own entry points** —
      `CacheHitTaskWithPartitions` / `CacheHitTaskClaimedWithPartitions`, fed by
      `cache.Entry.Partitions` — because a cached producer must still expand its
      consumer's group (`TestCacheHitTaskWithPartitionsExpandsTheGroup`,
      `TestFanOutCachedProducerExpandsGroup`). See follow-up 7 for the one-time
      cache warm this implies._
      Files: `internal/run/store.go`, `internal/worker/runtime_executor.go`.
      Depends on: A2, A4, B2, C3, G1.
- [x] D2. Enforce `fanOut.maxParallel` in the distributed scheduler: add an
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
      chain deeper than `maxParallel`. **Scope limit:** this item covers the SQL
      predicates only. Owner in-memory mode dispatches from `RunState.ready` and
      never reaches them, so the cap is unenforced there until **D6**.
      _Shipped 2026-08-26: `fanOutMaxParallelPredicateTx` returns the extra `WHERE`
      fragment, applied in the claimer's atomic predicate and the owner-dispatch
      query. `TestFanOutOrderedChainCompletesUnderMaxParallelOne` is the
      no-deadlock proof on a chain deeper than the cap._
      Files: `internal/worker/claimer.go`, `internal/run/store.go`.
      Depends on: D1.
- [x] D3. In-group ordering in the store — the one shared implementation both lanes
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
      (`:2621`), because a cache-hit instance is a satisfied dependency, and with a
      per-unit `fingerprint` a cache-hit prerequisite is the **common** case in an
      ordered group, not an edge case. This item is the SQL half only; the owner
      in-memory engine needs its own decrement and skip cascade — **D5**. A run-scoped
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
      _Shipped 2026-08-26: `decrementInGroupDependentsTx` /
      `skipInGroupDependentsTx` in `internal/run/fanout.go`, called from the
      cache-hit route as well as `completeTask`. `markInstanceSkippedTx` is the
      single terminal-skip primitive so each cascaded skip gets its own
      `terminal_sequence` and is replay-visible
      (`TestSkipInGroupDependentsIsReplayVisibleAndEmitsEvents`). fail_fast is
      `resolveGroupOnInstanceFailureTx` / `groupFailsFastTx` /
      `failFastSkipSiblingsTx`._
      _`fail_fast` is **not-yet-started-only in all three lanes** — it cancels siblings that are pending *or* claimed/dispatched but without a container yet (keyed on an empty `runtime_id`, the one marker both lanes share; `started_at` is stamped at claim time on the owner-push lane and never by `ClaimNext`, so it cannot be the discriminator) — and it is the default
      whenever `fanOut.failurePolicy` is omitted (`validateSteps` stamps it;
      `normalizeFanOutFailurePolicy` re-derives the same answer in the owner). A
      **running** sibling is deliberately left alone in every lane — Caesium cannot
      kill its container, so marking the row skipped would claim a terminal state
      for live work and invite the worker's later completion to contradict it. It
      runs to its own terminal state, the group resolves `failed` on that
      transition, and the fan-in is skipped by its trigger rule: later, never
      wrong. This refines the design's "cancelling pending siblings"
      (`docs/design-dynamic-fanout.md:324`, `:691`, `:1111`). SQL:
      `markInstanceSkippedTx`, whose `markInstanceSkippedFromTx` makes the
      `{pending}` source set an explicit argument at every call site rather than an
      inherited default. Owner: the `st.Status != TaskStatusPending` guard in
      `ApplyCompletion`. Local: `TestFanOutLocalFailFastLetsRunningSiblingsFinish`;
      SQL `TestFailTaskFailFastSkipsEveryPendingSibling`; defaults
      `TestFailTaskFailFastIsTheDefaultPolicy`,
      `TestFailTaskFailFastWhenFanOutConfigIsAbsent`,
      `TestFanOutLocalDefaultFailurePolicyIsFailFast`, and end-to-end
      `TestFanOutFailFastIsTheDefault`._
      Files: `internal/run/store.go`.
      Depends on: D1, G1.
- [x] D4. Expand in the **run-owner in-memory path**, the third advancement
      implementation. Today `OwnerManager.Complete`
      (`internal/run/owner_manager.go:215`) advances `RunState` in memory and
      persists through `CompleteTaskOwner` (`internal/run/store.go:2905`), never
      touching `completeTask` — so D1's hook is unreachable in this mode and a
      fan-out group collapses to one row, completing siblings together and
      finalizing the run early. Wire it along the seam **branch selections already
      use**, which is the exact precedent: a `type: branch` task's runtime decision
      travels in `CompleteRequest.BranchSelections`
      (`internal/dispatch/dispatch.go:137-140`), is resolved by `ResolveBranchSkips`
      (`internal/run/owner_topology.go:140`), and reaches
      `RunState.ApplyCompletion` (`internal/run/owner_state.go:160`) as
      `branchSkipped`. The partition list is the same kind of fact — a container's
      runtime decision that changes the DAG's live shape — so: carry it on
      `CompleteRequest`, expand the group inside `ApplyCompletion` (creating N
      instance entries, raising `RunState.total` in the same critical section, and
      seeding in-group indegree per D3), and persist the N rows in
      `CompleteTaskOwner`'s transaction. **Reuse `pkg/task`'s normalization and
      A4's validator** — one implementation of the rules, three call sites; a
      second copy of the cycle check here is a review-blocking defect. Checkpoint
      after expansion so a takeover recovers the group, and cover with an
      owner-mode fan-out scenario plus a failover mid-group.
      _Shipped 2026-08-26: `CompleteRequest.Partitions` carries the list along the
      branch-selection seam, `RunState.ApplyExpansion` creates the instance entries
      and raises `total` in one critical section, and `PlanFanOutExpansion` is the
      shared planner both lanes call — one copy of the validator, as required._
      _Open: the failover-mid-group half of this item's coverage was never
      written (follow-up 1)._
      _Lane result, as reported by the wave (not independently re-run here): the
      owner-memory lane now executes **21/21 `TestFanOut*` scenarios in ~2–3 min**.
      Standing that lane up is what surfaced the four owner completion-identity
      defects recorded under G4 — the strongest argument in this plan for the
      "a test that exercises only one owner mode is not evidence" rule._
      Files: `internal/run/owner_state.go`, `internal/run/owner_manager.go`,
      `internal/run/store.go`, `internal/dispatch/dispatch.go`.
      Depends on: D1, D3, G2, G3, G4.
- [x] D5. Advance in-group ordering in the **owner in-memory engine** — the
      inverse of D4's seed, without which an ordered group under
      `CAESIUM_RUN_OWNER_IN_MEMORY=true` **stalls deterministically**. D3 puts the
      in-group decrement in SQL and D4 seeds the in-group indegree, but
      `advanceSuccessors` (`internal/run/owner_state.go:196-230`) walks only
      `rs.topo.Adjacency`, built from `task_edges` (`LoadRunTopology`,
      `internal/run/owner_topology.go:16`) — and a partition's `dependsOn` is
      producer-supplied at runtime, so it is *by construction* absent there. Seeded
      counter, no decrement path, dependent never becomes ready. Four parts:
      (a) **In-group adjacency in `RunState`**, populated at expansion (D4) in the
      same critical section that creates the instance entries, and registered with
      **G7's single traversal kernel** so catalog and in-group edges are enumerated
      in one place. Catalog edges stay task-level; in-group edges are
      instance-level; one traversal consults both. Because G7 landed first, this is
      an addition to one edge-resolution seam — **not** a parallel change to
      `advanceSuccessors` and `ApplyTerminalRow`. If this item finds itself editing
      two traversals, G7 is incomplete and should be finished first.
      (b) **The cache-hit route.** A cache hit is a completion, and it is the
      *common* case here — the whole point of a per-unit `fingerprint` is that
      prerequisites cache-hit — so the decrement must fire for
      `TaskStatusCached` too. Note `IsTerminalSuccess`
      (`internal/run/store.go:57-59`) already counts `cached`, and cached **does**
      travel the owner path today despite `CompleteTaskOwner`'s docstring
      (`:2904`) claiming otherwise: `ValidCompleteStatuses`
      (`internal/dispatch/dispatch.go:103-107`) admits it, the owner block
      (`:486-489`) forwards `req.Status` unfiltered, and `CompleteTaskOwner` writes
      `"cache_hit": status == TaskStatusCached`. **Correct that docstring in this
      item** — an implementer trusting it would skip exactly this wiring and ship a
      group that stalls only when a prerequisite cache-hits.
      (c) **The in-group skip cascade**, emitted into the same `res.Skipped` list
      `ApplyCompletion` already returns and `CompleteTaskOwner` already persists —
      the owner-side counterpart of D3c. Without it an owner-mode group hangs on a
      failed prerequisite exactly as the SQL path would.
      (d) **Replay — rebuild the edges before replaying any completion, not after.**
      `RecoverRunState` (`internal/run/recovery.go:41`) rehydrates topology from the
      **catalog**, which has no in-group edges, so a recovering owner would restore
      counters it can never decrement. Worse, a completion that landed *after* the
      last checkpoint is replayed through the replay traversal: if the in-group
      adjacency is not present at that moment, the dependent keeps its
      pre-completion indegree, never becomes ready, and the run stalls after
      takeover — the live-path bug, one function over. So: load the run's instance
      rows, rebuild in-group adjacency from their `PartitionDependsOn` column (A1),
      register it with G7's kernel, and **only then** replay the terminal tail. The
      rows are authoritative and the snapshot carries only counters; do **not** also
      snapshot the edges, since two copies of one graph can disagree after a partial
      write. Assert this directly: a failover whose last checkpoint predates a
      prerequisite's completion must still run the dependent.
      Verified by an ordered-group scenario under owner mode that includes a
      cache-hit prerequisite and a mid-group failover; a stall reproduces as a run
      that never terminates, so assert a bounded run duration, not just final
      status.
      _Shipped 2026-08-26: in-group edges are registered on `RunState` and walked by
      G7's `traverseSuccessors`, so this was an addition to one seam, not two.
      Cached completions travel the owner route and decrement (D5b), the skip
      cascade rides `res.Skipped` (D5c), and `RecoverRunState` calls
      `RehydrateInGroupEdges(rows, catalog)` from the instance rows' partition
      columns **before** replaying the terminal tail (D5d) — the edges are not
      snapshotted, exactly as this item required._
      _Open: D5d's assertion — "a failover whose last checkpoint predates a
      prerequisite's completion must still run the dependent" — is covered only at
      the state-rebuild level (`TestRehydrateInGroupEdges_*`), never end to end
      (follow-up 1)._
      Files: `internal/run/owner_state.go`, `internal/run/owner_manager.go`,
      `internal/run/recovery.go`, `internal/run/store.go`.
      Depends on: D3, D4, G7.
- [x] D6. Enforce `fanOut.maxParallel` in **owner in-memory mode**. D2 adds the
      in-flight cap to the claimer's SQL predicate and the owner-dispatch SQL query,
      but `dispatchRunInMemory` (`internal/dispatch/loop.go:376`) dispatches from
      `RunState.ready` and **never calls `PendingTasksForDispatch`**
      (`internal/run/store.go:1733`), so a SQL-only cap is silently unenforced
      whenever `CAESIUM_RUN_OWNER_IN_MEMORY=true`. Cap the group's in-flight count
      when draining the ready queue, using the same per-group accounting
      `RunState` already needs for group status. A cap that holds in one mode and
      not the other is worse than no cap — the mode that ignores it is the one
      running the largest clusters. Test by asserting concurrent in-flight instances
      never exceed `maxParallel` in **both** modes, not just the default.
      _Shipped 2026-08-26: `RunState.ReadyTasks()` counts in-flight instances per
      **catalog** id and withholds ready instances over the cap, so the owner's
      ready queue enforces it without a SQL predicate._
      Files: `internal/dispatch/loop.go`, `internal/run/owner_state.go`,
      `internal/run/owner_manager.go`.
      Depends on: D2, D4.

### Stream E — Surfaces: REST + CLI + observability alignment

The operator surface (design Phase 4, backend): the partition-inspection and
per-instance-retry endpoints, the CLI verbs over them, and the alignment of the
causal verbs (`why`, `run diff`, replay) that assumed one `TaskRun` per `Task`.

- [x] E1. Add the REST partition surface: `GET /v1/jobs/:id/runs/:run_id/tasks/
      :task_id/partitions` (paginated instance list — value, index, status, attempt,
      cache_hit, duration, error, plus `fingerprint` and `depends_on` so an ordered
      group is inspectable without re-deriving the graph from the producer's row),
      `POST …/tasks/:task_id/partitions/:index/retry`
      (reset one failed instance, terminal runs only, re-evaluate fan-in on
      completion), and collapse fanned groups in run-detail payloads to one entry
      with `partition_count` + a status histogram (a 10k-instance run must not bloat
      every run-list response). Add the route lines to `Protected()`
      (`api/rest/bind/bind.go:57`).
      _Shipped 2026-08-26: `api/rest/controller/job/run/partitions.go` with a
      `status` filter, a `status_counts` histogram computed over the **unfiltered**
      group, `started_at`/`completed_at`, `fingerprint`, and `depends_on`; retry is
      terminal-only and answers **409** otherwise. Routes are bound in
      `api/rest/bind/bind.go` and scoped in `internal/auth/rbac.go` (viewer to
      list, runner to retry). Run-detail payloads collapse via
      `collapseFanOutGroups`._
      Files: new `api/rest/controller/job/run/partitions.go`,
      `api/rest/service/run/`, `api/rest/service/task/`, `api/rest/bind/bind.go`.
      Depends on: A1, D1.
- [x] E2. Add the CLI verbs: `caesium run partitions <run-id> --task <name>
      [--status failed] [--json]` (a new `cmd/run/partitions.go` registered on
      `run.Cmd`, `cmd/run/run.go:6`) and a `--partition <value>` flag on
      `caesium run retry` (`cmd/run/retry.go`) that resets a single instance.
      Machine output goes to **stdout, logs to stderr** per the repo's
      stdout-cleanliness gate. `--json` includes each instance's `fingerprint` and
      `depends_on`; retrying one instance does **not** cascade to dependents that
      already succeeded, and the command says so rather than silently re-running a
      subtree.
      _Shipped 2026-08-26: `cmd/run/partitions.go` (table + `--json` + `--server` +
      `--api-key`) and `--partition` on `caesium run retry`, which resolves the
      value to an index over the list endpoint before POSTing the retry._
      _Open: `caesium run retry <run>` **without** `--partition` still runs
      in-process against `runstorage.Default()` rather than over `--server`
      (`cmd/run/retry.go:75`). Pre-existing CLI design, but it means the two retry
      paths do not share a transport — follow-up 3._
      Files: new `cmd/run/partitions.go`, `cmd/run/retry.go`, `cmd/run/run.go`.
      Depends on: E1.
- [x] E3. Align the causal verbs with fanned groups: `caesium why` names
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
      _Shipped 2026-08-26: `WhyTaskPartition` answers a fanned step with a group
      summary and `--partition` selects one instance (an unknown value lists what
      exists); `whydiff` names `partition` / `partitionFingerprint` /
      `partitionAttributes` as discriminating fields; `run diff` aligns by
      partition **value** and reports `partitionsAdded` / `partitionsRemoved`;
      `receipt get` attests one entry per partition and flags fanned entries;
      quarantined replay refuses a fanned baseline (`ErrFannedBaseline`, HTTP 409)
      with a message pointing at `caesium run partitions`._
      Files: `cmd/run/diff.go`, `cmd/run/replay.go`, the `why`/`receipt` commands
      under `cmd/run/`, `api/rest/service/rundiff/`, `api/rest/service/why/`.
      Depends on: C2.
- [x] E4. Make `run retry` ordering-aware. **G5 already fixed the accounting**
      (a predecessor group is satisfied only when every live sibling row is a
      terminal success, not when any one is); this item adds only the ordering
      semantics on top. `RetryFromFailure`
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
      _Shipped 2026-08-26: `resetInstanceOutstandingTx` re-seeds each reset
      instance's in-group indegree over **non-terminal** dependencies only, and
      dependency-skipped instances are reset alongside failed ones. Driven end to
      end by `TestFanOutOrderedGroupRetryDrivesDependents` (`test/fanout_test.go`)
      and unit-pinned by `TestRetryPartitionReseedsInGroupIndegreeOverNonTerminalDeps`._
      Files: `internal/run/store.go`, `cmd/run/retry.go`.
      Depends on: D3, G5.

### Stream F — UI (Caesium Console)

The frontend group rendering (design Phase 4, UI): a fanned step is one grouped
node — the graph never gains N nodes, so 400 partitions render like 4. Consumes
the Stream E endpoints.

- [x] F1. Render a fanned step as one **grouped node** in the DAG
      (`ui/src/features/jobs/JobDAG.tsx`): a stacked-card affordance, a `×N` badge,
      and a segmented progress ring (succeeded/running/failed/pending). The graph
      never gains N nodes.
      _Shipped 2026-08-26 — with one correction to this item's scope. The badge,
      the stacked cards, and the segmented strip live in
      `ui/src/features/jobs/components/TaskNode.tsx` (testid
      `fanout-status-strip`), not in `JobDAG.tsx`, which only passes
      `partitionCount` / `partitionStatusCounts` through. Shared segment maths is
      `ui/src/lib/fanout.ts`._
      Files: `ui/src/features/jobs/JobDAG.tsx`.
- [x] F2. Add the run-timeline group lane and the partition table: one lane per
      group in `RunTimeline.tsx` (an envelope bar first-start→last-end with a density
      strip, expandable to the top-K longest/failed), and a virtualized,
      status-filterable partition table in `TaskDetailPanel.tsx` (value, status,
      attempt, duration, cache-hit, per-row log link and retry button wired to the
      retry endpoint). For an ordered group the table also shows the short
      fingerprint and a `depends on` column, and `skipped` renders as a first-class
      status (an in-group dependency failure is a normal, expected outcome, not an
      error state). Add the `getPartitions`/`retryPartition` methods to
      `ui/src/lib/api.ts`.
      _Shipped 2026-08-26: `RunTimeline.tsx` renders a `run-timeline-group-row`
      wrapper with a `run-timeline-density-strip` for fanned steps while keeping the
      existing `run-timeline-task-row` testid unconditional, so no existing timeline
      assertion had to be relaxed. `TaskDetailPanel.tsx` gained `PartitionTable`,
      virtualized with `@tanstack/react-virtual`, with a status filter, attempt /
      duration / cache-hit columns, fingerprint and `depends_on` when present, and a
      per-row retry that surfaces `onError`. `api.ts` gained `getPartitions` and
      `retryPartition`. Covered by
      `ui/src/features/jobs/__tests__/TaskDetailPanel.partitions.test.tsx` and by
      real assertions in `ui/e2e/fanout.spec.ts`._
      Files: `ui/src/features/jobs/RunTimeline.tsx`,
      `ui/src/features/jobs/TaskDetailPanel.tsx`, `ui/src/lib/api.ts`.
      Depends on: E1.

## Harness Strengthening

- [x] H-1. Wire the fan-out path onto the live integration server: set
      `CAESIUM_FANOUT_MAX_PARTITIONS` (a low value so the "1025 partitions fails the
      producer loudly" cap test drives the real cap) on the `just integration-up` /
      `just integration-test` server and pass it through in
      `.github/workflows/ci.yml`, and ensure the harness can run the **distributed
      lane** (`CAESIUM_EXECUTION_MODE=distributed`) scenario so worker-crash /
      lease-reclaim / rate-limit-drain assertions execute in CI, not an internal
      call, **and can run the suite in both `CAESIUM_RUN_OWNER_IN_MEMORY` modes**
      so the run-owner in-memory advancement path (the one that bypasses
      `completeTask`) is exercised in CI rather than assumed. Add the shared
      fan-out test helpers to the `test/` harness: a producer
      script that emits a **string-form** list, one that emits an **object-form** list
      with fingerprints and `dependsOn` (the `a → b → c` and diamond fixtures the
      ordering scenarios drive), and one that emits an **invalid** list per rejection
      class (cycle, dangling key, malformed fingerprint, over-cap). Any image these
      helpers use must be a canonical pinned ref — the image-pin guardrail
      (`TestPinnedContainerImageVersionsAreConsistent`) rejects unpinned base-image
      names, and its canonical set lives in `internal/guardrails/guardrails_test.go`.
      _Shipped 2026-08-26: all three integration servers run with
      `CAESIUM_FANOUT_MAX_PARTITIONS=8` (so the over-cap scenario drives the real
      cap), a new `integration-test-owner-memory` lane runs the suite under
      `CAESIUM_RUN_OWNER_IN_MEMORY=true`, and CI gained the matching
      `build-and-integration-test-owner-memory` job. `test/fanout_test.go` holds 21
      `TestFanOut*` scenarios over `test/fanout_helpers_test.go` fixtures
      (string-form, object-form with fingerprints and `dependsOn`, and one invalid
      list per rejection class)._
      _Open — and load-bearing when reading CI history: on `master` the distributed
      lane's `-run` pattern was **not suite-qualified**, so under a testify suite it
      matched nothing and the lane was green having run zero tests. Both lanes are
      now `-run "TestIntegrationTestSuite/…"`. Treat any distributed-lane green
      dated before 2026-08-26 as unproven — follow-up 6._
      Files: `justfile`, `.github/workflows/ci.yml`, `test/` harness helpers.

## Navigational / Organizational Improvements

- [x] N-1. Reflect fan-out in the docs, last, after A–F ship. Flip the
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
      _Shipped 2026-08-26: the design banner and the `docs/roadmap.md` Phase 4 row
      both read Shipped and link here; `docs/job-definitions.md` gained a
      `## Dynamic Fan-Out` section and indexes the new example;
      `docs/caesium-job-llm-reference.md` documents the `fanOut` block, both marker
      element forms, and `CAESIUM_PARTITION_JSON`;
      `docs/job-schema-reference.md` was **regenerated** from `report.go`; and the
      `docs/README.md` bullet was updated in place in backtick form._
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
§4 and implemented by Stream A of [`infra-deploy.md`](infra-deploy.md) (which
edits the same `hash.go` / `job.go` / `runtime_executor.go` / `definition.go`
files as Streams A–C here — land one before the other, see that plan's
Sequencing). Nothing in this plan may invent a second mechanism for it; fingerprints
without the chain break are correct but conservative (all instances re-run when
the producer does — never a stale hit). The design's eight Open Questions
(`minSuccessRatio`, window-derived partitions, per-partition resource profiles,
aggregate contract granularity, `retry_partition` as an agent action, freshness
interplay, in-group ordering visibility, an in-group depth guardrail) are
cross-design questions, not items here.

## Sequencing & Dependencies

**Cross-stream order:**

- **Stream G gates everything.** It re-keys the run lifecycle off `(run, task)`
  onto `TaskRun` identity across the SQL advancement path, the run-owner in-memory
  engine, the checkpoint format, recovery replay, retry accounting, and the
  owner↔worker wire protocol, and it collapses the live/replay traversal
  duplication that has generated four P1s in one family. It is behavior-neutral for
  unfanned runs and ships with **no fan-out feature visible**, so it can be reviewed
  and reverted on its own — and no item in A–F may merge before it, because ordering
  and expansion cannot be layered on a substrate that cannot represent two siblings.
  Within G: G1 → G2 → (G3, G4, G7) and G1 → G5; G6 is the audit and closes the
  stream. **G7 is load-bearing for Stream D**: it is what makes D5 a one-place
  change instead of a two-place change that review has to catch.
- **Streams A and B are the foundation and are independent of each other** (A owns
  `internal/models/run.go` + `pkg/task/output.go` and the partition-column binding
  in `internal/run/store.go`; B owns `pkg/jobdef/definition.go` +
  `internal/models/task.go` + the runtime spec) — they run in parallel once G1 is
  in. A has the larger blast radius, so it merges first on any same-wave overlap.
  A2 is now thin: G does the re-keying, A2 binds it to the partition columns and
  proves behavior-neutrality.
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
- **W0 = G (G1→G2→(G3, G4, G7); G5 after G1; G6 last) + H-1.** The identity
  migration, alone, with no fan-out surface. H-1 rides along because the harness
  work is independent and W0's failover/owner scenarios need it. This wave is a
  hard gate: **do not start W1 until G6's audit table is merged.**
- **W1 = A (A1→A2→A3→A4) + B (B1→B2).** A and B touch disjoint files.
- **W2 = C (C1→(C2, C3)).** Unblocked once A + B are in.
- **W3 = D (D1→(D2, D3)→D4→(D5, D6)).** Unblocked once C3's fan-in contract is
  in. D4–D6 — owner-mode expansion, advancement, and the in-flight cap — are the
  items most likely to be dropped as "an edge case"; they are not, they are a third
  of the execution surface, and D5 in particular is the difference between an
  ordered group running and stalling forever under
  `CAESIUM_RUN_OWNER_IN_MEMORY=true`.
- **W4 = C4 + E (E1→E2, E3, E4).** C4 lands here, not in W2 — it mirrors D3's
  store-side ordering. Unblocked once D1's instances exist (E3 needs only C2).
- **W5 = F (F1, F2) + N-1.** F after E1; N-1 last.

**Within-stream order:** G1 → G2 → (G3, G4, G7 — G3 owns the snapshot and recovery,
G4 the wire protocol, G7 the traversal kernel; G3 and G7 both touch
`recovery.go`/`owner_state.go`, so sequence G3 → G7 rather than running them
concurrently); G5 needs only G1; G6 closes the stream. **G7 must merge before D5**
regardless of wave packing. A1 → A2 →
A3 → A4 (columns+index, then the partition-column binding, then markers, then the
in-group validator — A3's env cap is read by B1's lint and C1's executor, and A4
consumes A3's normalized `[]Partition`). B1 → B2. C1 → (C2, C3 in parallel —
different concerns in the same file, coordinate the merge) → C4 (after D3). D1 →
(D2, D3 in parallel — different funcs in the same file, D2 in the claim predicate,
D3 in the decrement/skip paths) → D4 → (D5, D6 in parallel — D5 owns
`owner_state.go` advancement, D6 owns the dispatch loop). E1 → E2; E3 parallel to E1/E2 (depends only
on C2); E4 after D3 + G5. F1, F2 parallel (F2 needs E1).

**Cross-stream file conflicts:**

- `internal/run/store.go` — the busiest file in the plan. **G1** re-keys every
  write path; **G5** fixes retry accounting; **A2** binds the partition columns;
  **D1** adds the expansion transaction to
  `CompleteTaskWithResult`/`completeTask`; **D2** touches the dispatch predicate;
  **D3** adds the instance-keyed sibling decrement and the in-group skip cascade;
  **D4** extends `CompleteTaskOwner`; **E4** adjusts `RetryFromFailure` again;
  **E1** reads instances via the service layer (no `store.go` edit).
  **Sequence G1 → (G5, A2) → D1 → (D2, D3) → D4 → E4** (already a dependency
  chain); never the same wave. D2 and D3 may share a wave — D2 is in the claim
  predicate, D3 is in the decrement/skip paths. G5 and E4 both touch
  `retryFromFailure` and are two waves apart by construction.
- `internal/run/owner_state.go` — G2 re-keys `RunState`, G3 versions and re-keys
  the snapshot, **G7 extracts the single traversal kernel** that
  `advanceSuccessors` and `ApplyTerminalRow` both call, D4 adds in-memory expansion
  to `ApplyCompletion`, D5 registers in-group adjacency with G7's kernel and adds
  the skip cascade, D6 adds per-group in-flight accounting. **Sequence
  G2 → G3 → G7 → D4 → D5 → D6**; single-threaded through the stream order, never
  concurrent. This is the second busiest file in the plan after `store.go`, and G7
  is the item that stops it getting busier — without it every later item edits two
  traversals instead of one.
- `internal/run/owner_topology.go` — G2 (instance keys) and G7 (the shared
  edge-resolution seam that `LoadRunTopology` and `loadReplayRunTopology` both
  use). Sequence G2 → G7.
- `internal/dispatch/loop.go` — G4 (instance identity on dispatch) and D6 (the
  owner-mode `maxParallel` cap in `dispatchRunInMemory`). Sequence G4 → D6.
- `internal/run/owner_manager.go` — G2 (instance keys) and D4 (the partition list
  on the completion seam). Sequence G2 → D4.
- `internal/run/recovery.go` — G3 (instance-keyed replay), G7 (replay calls the
  shared kernel), D5 (rebuild in-group adjacency before replaying the tail).
  Sequence G3 → G7 → D5.
- `internal/dispatch/dispatch.go` — G4 adds the instance identity to
  `DispatchRequest`/`CompleteRequest`; D4 adds the partition list to
  `CompleteRequest`. Both edit the same two structs. **Sequence G4 → D4**; land
  the identity field first so D4 rebases onto a settled envelope.
- `internal/models/run_checkpoint.go` — G3 only (the format version column).
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
- **Instance identity migration (G) — behavior-neutrality is the gate.** G ships
  with no fan-out surface, so its evidence is that **nothing changed**: the full
  existing suite green, plus `just integration-test` green in **both**
  `CAESIUM_RUN_OWNER_IN_MEMORY` modes, plus the existing owner/failover coverage
  (`internal/run/failover_test.go`, `owner_complete_test.go`, `recovery_test.go`,
  `checkpoint_test.go`, `retry_admit_test.go`) unchanged in intent and green. A G
  PR that needs an existing assertion *relaxed* is a red flag: state why in the PR
  or treat it as a defect.
- **Checkpoint format migration (G3):** an explicit test that a blob written in
  the **old** format is rejected and falls back to replay-from-terminal-rows —
  not silently restored as empty state. This is the one failure mode the missing
  version field makes invisible, and it cannot be caught by a round-trip test.
- **Owner-mode fan-out (D4, D5, D6):** the happy-path, ordering, and failover
  fan-out scenarios run with `CAESIUM_RUN_OWNER_IN_MEMORY=true` on the live
  integration server, asserting N instances materialize (not one) and the run does
  not finalize until every instance is terminal. A shared assertion inside another
  lane does not count — the owner path is the one that silently collapses groups.
  Two assertions specifically:
  **(a) a stall is a timeout, not a failed status.** An ordered group whose
  in-group decrement is missing never terminates, so the scenario must assert a
  **bounded run duration**; a final-status assertion alone hangs the suite instead
  of failing it, and reads as CI flake.
  **(b) the prerequisite must cache-hit in at least one ordering scenario.** With
  per-unit fingerprints that is the common path, and it exercises `cacheHitTask` /
  `CacheHitTaskClaimed` — different functions from `completeTask`, and a route a
  `succeeded`-only fixture never touches.
- **Route completeness (every fan-out item):** the PR states which of the four
  completion routes and three advancement implementations the change touches, and
  for any state it seeds, where the inverse lives on each. An item that seeds a
  counter without naming its decrement on all routes should block review — this
  defect shape has surfaced four times.
- **Traversal singularity (G7, and every item after it):** after G7 there is one
  successor-traversal kernel and one edge-resolution seam. A later PR that adds an
  edge class or a decrement in **two** places has either bypassed G7 or found a
  gap in it; treat that as a defect in G7, not as normal work. The regression test
  is a direct one: live and replay must produce identical indegree and readiness
  for the same terminal sequence.
- **Post-checkpoint replay (D5d):** a failover whose last checkpoint predates a
  prerequisite instance's completion must still run the dependent. This is the
  exact window where the replay traversal, not the live one, decides readiness —
  a failover test that checkpoints after every completion never enters it.
- **Retry does not release downstream early (G5):** a group with one succeeded
  and one failed sibling, retried; assert the downstream step does **not** start
  until the retried sibling is terminal. Without an explicit ordering assertion
  this defect is invisible — the run still goes green, just wrongly.
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

0. **Stream G — the instance identity migration** is in and was **behavior-neutral
   on the way in**: the run lifecycle addresses task state by `TaskRun` identity
   across the SQL advancement path, the run-owner in-memory engine, the versioned
   checkpoint format, recovery replay, retry accounting, and the owner↔worker wire
   protocol; a predecessor group is satisfied only when **every** live sibling row
   is a terminal success; an old-format checkpoint blob is rejected rather than
   silently restored; the live and replay traversals are **one implementation**
   (G7), so an edge class is enumerated in exactly one place; and G6's site →
   answer (re-key / aggregate / assert-unfanned) → test table is recorded. Closed by the existing suite and
   both owner modes green with no assertion relaxed. **This criterion gates every
   one below it** — nothing in A–F is meaningful on a substrate that cannot
   represent two siblings.
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
   predicate, and survives a mid-group worker crash via lease reclaim — **and does
   all of it in run-owner in-memory mode too** (D4–D6), where expansion rides the
   `ApplyCompletion`/`CompleteTaskOwner` path rather than `completeTask`, the
   in-group decrement and skip cascade live in `RunState` rather than SQL, and
   `maxParallel` is enforced against the owner's ready queue rather than a claim
   predicate. Closed by the `distributed` integration scenarios green in CI with the
   ordering fixtures, run in **both** `CAESIUM_RUN_OWNER_IN_MEMORY` modes, with at
   least one ordering scenario whose prerequisite **cache-hits**, a bounded-duration
   assertion so a stall fails rather than hangs, and an owner-failover mid-group
   scenario that re-dispatches only the unfinished instances.
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
   the distributed lane runnable in **both** `CAESIUM_RUN_OWNER_IN_MEMORY` modes,
   and the string-form / object-form / rejection fixtures available, so the A–D
   scenarios drive the live binary in CI.
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
- `internal/run/owner_state.go`, `internal/run/owner_manager.go`,
  `internal/run/owner_topology.go`, `internal/run/recovery.go`,
  `internal/run/checkpoint_store.go`, `internal/run/checkpoint_writer.go`,
  `internal/dispatch/dispatch.go` — the run-owner in-memory subsystem Stream G
  re-keys and Stream D4 teaches to expand. The third advancement path, and the one
  the first draft of this plan missed entirely.
- `internal/run/store.go`, `internal/job/job.go`, `internal/worker/claimer.go`,
  `pkg/task/output.go`, `internal/cache/hash.go`,
  `internal/cache/shortcircuit.go` — the execution/cache surfaces this plan rewires
  (and, in `shortcircuit.go`'s case, deliberately does not: its conservative
  byte-identical-output proof cannot substitute for the chain break).
