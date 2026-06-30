# Concurrency Strategies & Priority Queues — Run-Level Scheduling Control

Last updated: 2026-06-28

Caesium's concurrency model today is a single per-run knob: `metadata.maxParallelTasks`
bounds how many **tasks within one run** execute at once (a `worker.Pool` semaphore at
`internal/worker/pool.go:18`, defaulting to `runtime.NumCPU()`). There is **no control
over concurrent runs of the same job**, no rate limiting for shared external resources,
and **no priority** — in distributed mode the claimer drains pending tasks strictly FIFO
(`ORDER BY tr.created_at ASC` at `internal/worker/claimer.go:211`). The consequences are
the roadmap's two open **P1** items: a cron job that overruns its interval double-fires
with no policy (§1.3), and a critical pipeline gets no preferential treatment when the
cluster is saturated (§1.4).

This plan ships **run-level scheduling control** in three capabilities, plus their
operator surface:

- **Run concurrency strategies** (§1.3) — a `metadata.concurrency` block (`maxRuns` +
  `strategy` ∈ `queue`/`replace`/`skip`/`fail`) that gates a *new run* of a job when the
  job already has `maxRuns` active. `queue` parks the overflow run in a durable
  `run_queue` that a leader-gated background dequeuer drains as slots free.
- **Priority queues** (§1.4) — a job-level `metadata.priority` (and a per-run override)
  that orders both the distributed task claimer and the run-queue dequeuer
  (`high`/`normal`/`low` → `3`/`2`/`1`), strictly ordering, never preempting.
- **Resource rate limiting** (§1.3, lower priority) — a `metadata.rateLimits` /
  step-level `rateLimit` that throttles tasks consuming a shared resource via a
  durable sliding-window counter, re-queuing (not blocking) a rate-limited task so the
  worker slot stays free.

Every new field, CLI verb, and REST surface ships with a `test/` integration test that
drives the **real** surface against a live server (per the `CLAUDE.md`
end-to-end-coverage gate): a unit test that hand-orders a slice of `TaskRun`s proves the
sort, never the claim path; an integration test that fires three real runs at different
priorities and asserts the claim order proves the wiring.

This plan follows the `exec-plan-wave` skill's structural convention:
`## Progress` is a wave-by-wave dashboard, `## Streams` is the work backlog,
`## Sequencing & Dependencies` captures cross-stream order, and
`## Acceptance Criteria` lists the gates that close out the entire plan. Any agent can:

1. Pick a numbered checklist item from `## Streams` whose dependencies are satisfied
   (per `## Sequencing & Dependencies`).
2. Land it as a self-contained PR.
3. Run the verification block under `## Verification (Run For Every PR)`.
4. Tick the checkbox and update the active wave's per-stream bullet in `## Progress`.

For wave orchestration of the streams below, see
[`.claude/skills/exec-plan-wave/`](../../../.claude/skills/exec-plan-wave/).

## Source-Of-Truth Note

This plan implements [`design-concurrency-priority.md`](../../design-concurrency-priority.md).
**The design doc is authoritative for INTENT and SCOPE** (the four strategies, the
priority tiers, the rate-limit model). Strategic priority/status lives in
[`docs/roadmap.md`](../../roadmap.md) §1.3 + §1.4 (the roadmap wins on
priority/status disagreements). The job-definition contract lives in
[`pkg/jobdef/definition.go`](../../../pkg/jobdef/definition.go) (the schema wins on
field-shape disagreements).

**Where this plan records a correction below — each verified against the real code —
the PLAN's corrected contract wins**, and the design is reconciled to match in N-1.
A pre-draft survey of the live backend plus two adversarial-review rounds found the design
(status: *Proposed*) misdescribes the *current* code in the places below. Implementers
follow these corrected contracts, NOT the stale design snippets:

1. **The local "semaphore" is task-parallelism, not run-concurrency.** The design's §1
   snippet (`make(chan struct{}, MaxParallelTasks)` gating runs) conflates two things.
   The real semaphore is `worker.Pool` (`internal/worker/pool.go:18`), sized by
   `maxParallelTasks`, bounding tasks *within* a run. Run-level concurrency is **net-new**.
2. **Parsed scheduling config does not reach runtime for free — it must be persisted on
   the catalog models.** The runtime `job` struct is built from `models.Job`, **not**
   `jobdef.Metadata` (`job.New(m *models.Job, …)` copies `m.MaxParallelTasks` etc. at
   `internal/job/job.go:127-138`); `models.Job` (`internal/models/job.go:12`) has **no**
   priority/concurrency columns, and `models.Task` (`internal/models/task.go`) has no
   rate-limit columns. A field that's only on `jobdef.Metadata` parses, lint-validates,
   and then **evaporates**. So A3 persists the config onto `models.Job` / `models.Task`
   and maps it in `internal/jobdef/importer.go` (the `&models.Job{…}` create literal
   ~`:392` AND the `existing.X = def.Metadata.X` update block ~`:413`, plus
   `populateTaskFromStep` ~`:594`); B/C/D depend on A3, not just A1.
3. **`CountActive` / `enqueueRun` / `replaceOldestRun` do not exist** and belong on
   `run.Store` (`internal/run/store.go`), **not** the `job` struct as the design's
   `j.runStore.CountActive(j.ID)` snippet implies.
4. **There is no single choke point — admission is a helper invoked from THREE creation
   paths.** REST creates the `JobRun` upstream (`runsvc.Start`→`store.Start`, `store.go:579`)
   then `job.Run` attaches via `run.FromContext` (`job.go:359`). But the cron/HTTP path is
   `job.Run`→`resolveRun`→`store.FindRunning` (`job.go:370`), which **on the overrun case —
   a prior run still active, exactly §1.3's scenario — attaches to the running run and
   never reaches `store.Start`**; and `store.StartForBackfill` (`store.go:682`) is a third
   creation path. A gate placed only in `store.Start` therefore misses the headline
   cron-overrun case. C1 extracts an `admit(tx, jobID, strategy, maxRuns)` helper called
   from (a) `store.Start` (REST/explicit), (b) `resolveRun` **before** the `FindRunning`
   attach (cron/HTTP triggers), and (c) `StartForBackfill` — or backfill runs are
   explicitly excluded from the active count (`backfill_id IS NULL`) and C1 states which.
   Correcting the original draft's "single choke point at job.Run" claim.
5. **Admission must be ATOMIC — `CountActive` then `Start` is a TOCTOU race.** Per-process
   write serialization (`pkg/db/db.go:158-167`) does **not** serialize across nodes (each
   node has its own pool; "writes serialize through Raft, busy_timeout is a no-op"),
   and even single-node cron fires each in their own `go func()` (`cron.go:141`). Two
   admissions can both read `active < maxRuns` and both insert, exceeding `maxRuns`. C1
   makes admission a single conditional `INSERT … SELECT WHERE (SELECT count(*) …
   status='running') < maxRuns RETURNING` (checking `RowsAffected`), not a read-then-write.
6. **Priority / Concurrency / RateLimits MUST be excluded from the cache key.** They are
   scheduling metadata, not execution identity — the `Kueue.QueueName` precedent (excluded
   at `internal/cache/hash.go:329-336`, enforced in three places: the `HasIdentityFields`
   gate ~`:337`, omission from the digest writes, and `hashableKubernetes()` ~`:426`).
   `HashInput` (`hash.go:266`) has no scheduling fields, so they only reach the hash if
   explicitly added — A1 ships a hash-stability test proving they don't.
7. **Priority propagates JobRun → TaskRun, and the claimer needs an index for it.** The
   claimer orders `task_runs`, so `priority` is stamped onto each `TaskRun` at
   materialization (`run.Store.RegisterTasks`, `store.go:781`) from the owning `JobRun`.
   The new `ORDER BY tr.priority DESC, tr.created_at ASC` runs on the hottest query in the
   system (`task_runs` is a hot table, `pkg/db/router.go:25`); B1 adds a GORM composite
   index covering the claim predicate + `(priority DESC, created_at ASC)`.
8. **`run_queue` + `rate_limit_tokens` are catalog tables**, not the design's
   `id TEXT` / `params TEXT` + raw `CREATE INDEX`. Caesium uses `uuid.UUID` PKs,
   `datatypes.JSON`, and GORM struct-tag composite indexes. They are per-job / global
   (low-volume), **not** hot per-run-execution tables: do **not** add them to
   `hotPathModels()` (`pkg/db/db.go`) or `hotTables` (`pkg/db/router.go`). The rate-limit
   conditional upsert needs **raw SQL** (`tx.Exec`) — the codebase's `clause.OnConflict`
   only does `DoNothing`/`DoUpdates`, never a WHERE-guarded update — deriving
   `acquired = (RowsAffected == 1)` from a single atomic statement.
9. **`replace` requires a net-new cancellation primitive.** There is no `cancelled`
   status (only `running`/`succeeded`/`failed`, `store.go:39-43`) and no run-cancel
   method. C2 scopes that work: a `StatusCancelled` terminal status + its event, and a
   store method that atomically transitions the `JobRun` **and** its non-terminal
   `task_runs` in one transaction so the claimer's `jr.status='running'` JOIN predicate
   (`claimer.go:198-218`) stops selecting the cancelled run's tasks.

No item may add a NEW trigger type, endpoint, table, or config knob *beyond these
recorded amendments* without first amending the design.

## Project Posture

**Run-level, not preemptive.** Priority is strictly ordering: a high-priority task waits
for a worker slot; it never kills a running low-priority task. `replace` cancels the
*oldest active run* (a real, atomic cancellation — correction #9), logging a warning, and
is documented as for idempotent/restartable jobs only.

**Distributed-correct by atomic admission.** Concurrency counts, the run queue, and
rate-limit windows are all DB-backed, but DB-backed is not automatically race-free:
caesium's write serialization is **per-process, not cross-node** (`pkg/db/db.go:162` —
"writes serialize through Raft; busy_timeout is a no-op there"), and the cron/trigger
scheduler (`executor.Start`, `cmd/start/start.go:564`) runs **ungated on every node**,
each fire a detached goroutine. So a naive check-then-act admits more than `maxRuns` runs.
This plan enforces every invariant with a single atomic statement a concurrent writer
cannot bypass: (a) **admission** is one conditional `INSERT … SELECT WHERE count < maxRuns`
(C1); (b) the **dequeuer** is leader-gated (`dqlite.IsLocalLeader`, the `WithReclaimGate`
precedent at `start.go:528`) AND claims each `run_queue` row with an optimistic `UPDATE …
WHERE claimed_by=''` (C3); (c) the **rate limiter** is a single conditional upsert (D1).
No invariant relies on per-node write serialization.

**Fairness is deferred.** Per-namespace round-robin + quotas (design §4) wait on
Multi-Tenancy (roadmap §3.1). Priority and rate-limit storage are designed to accept a
later `namespace` scoping column without a rewrite; that is the only forward-compat
concession this plan makes.

## Progress (as of 2026-06-28)

No implementation waves have shipped yet. The plan was published from the
`design-concurrency-priority.md` design, a six-subsystem survey of the live backend, and
**two adversarial-review rounds** — which caught **eleven blockers** (the
metadata-never-persisted gap on both create and update paths, the TOCTOU admission race,
the cron-overrun bypass, the REST-path choke-point error, the per-node dequeuer
double-launch, the missing cancellation primitive, the fictional RBAC file, the
step-rateLimit-never-persisted gap, the `skip`/`fail` nil-panic, and the `StartForBackfill`
path), folded into the **nine** as-built corrections in the Source-Of-Truth Note. The first
wave is the next eligible run of the `exec-plan-wave` skill; Stream A is the only stream
with no unmet dependency.

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Job-schema foundation — `priority`/`concurrency`/`rateLimits` schema, enum validation, **cache-hash exclusion**, **catalog persistence + importer mapping + runtime wiring**, schema docs | **P1** | Not started |
| B | Priority queues — `priority` on `JobRun`+`TaskRun` (+ claim index), JobRun→TaskRun propagation, claimer ordering, `caesium run start --priority` + REST/trigger priority | **P1** | Not started |
| C | Run concurrency strategies — **atomic** admission gate in `run.Store.Start` (`skip`/`fail`), `replace` (+ cancellation primitive), `run_queue` model + **leader-gated** dequeuer | **P1** | Not started |
| D | Resource rate limiting — durable single-statement sliding-window `Limiter`, `rate_limit_tokens` model, task-dispatch acquire/re-queue, token pruner | P2 | Not started |
| E | Observability surfaces — run-queue UI on job detail, rate-limit status indicator, `caesium job queue <alias>` | P2 | Not started |
| (Fairness) | Per-namespace round-robin + quotas (design §4) | — | **Deferred** — blocked on Multi-Tenancy (roadmap §3.1) |

## Streams

### Stream A — Job-schema foundation

Adds the three scheduling fields to the job-definition contract, keeps them out of the
cache identity, AND persists them onto the catalog so they reach runtime. Everything else
reads these fields, so A merges first. `pkg/jobdef/definition.go` is a true-conflict file
(the dual `Step`/`rawStep` declaration is parsed in both `UnmarshalYAML` *and*
`UnmarshalJSON`), so **all** schema additions live in this one stream.

- [ ] A1. Add the scheduling fields to the job schema and **exclude them from the cache
      key**. Add `Priority string`, `Concurrency *Concurrency` (`MaxRuns int`,
      `Strategy string`), and `RateLimits []RateLimit` (`Resource`, `Limit`, `Window`)
      to `Metadata`; add `RateLimit *StepRateLimit` (`Resource`, `Units`) to `Step`,
      mirrored in **both** `rawStep` blocks (`UnmarshalYAML` ~217-287 and `UnmarshalJSON`
      ~291-352) and their field-by-field assignment. Then verify — with a regression
      test — that none reach `internal/cache/hash.go` `HashInput.Compute()` (scheduling
      metadata, the `Kueue.QueueName` precedent at `hash.go:329-336`); a job that changes
      only `priority`/`concurrency`/`rateLimits` must produce an identical task hash.
      Files: `pkg/jobdef/definition.go`, `internal/cache/hash.go` (verify-only +
      `hash_test.go` stability case).
- [ ] A2. Validate the new enums + duration at lint time. Extend `Definition.Validate()`
      (`definition.go:367`, the `fmt.Errorf` enum pattern at ~378-382, mirroring the
      Kueue DNS-1123 validator at ~790-804): reject `priority` ∉ {`high`,`normal`,`low`},
      `concurrency.strategy` ∉ {`queue`,`replace`,`skip`,`fail`}, `concurrency.maxRuns` < 0,
      a `rateLimits[].window` that fails `time.ParseDuration`, a blank
      `rateLimits[].resource`, and a step `rateLimit.resource` that doesn't match a
      job-level `rateLimits[]` entry. `cmd/job/lint.go:47` already calls `Validate()`.
      Files: `pkg/jobdef/definition.go`.
      Depends on: A1.
- [ ] A3. **Persist the scheduling config to the catalog and wire it to runtime** (the
      "make the parsed fields reach the engine" item — without it, B/C/D read nothing).
      Add columns to `models.Job` (`Priority string` + `Concurrency datatypes.JSON` +
      `RateLimits datatypes.JSON`, mirroring the existing `SLA datatypes.JSON` precedent
      at `job.go:27`) and to `models.Task` (`RateLimitResource string`, `RateLimitUnits
      int`). Map them in `internal/jobdef/importer.go` at **BOTH** the create literal
      (`&models.Job{…}` ~`:392`) **AND the update allow-list maps** — the update path
      writes `tx.Updates(updates)` where `updates`/`taskUpdates` are explicit
      `map[string]any` (~`:426` and ~`:604`), NOT struct assignment, so a field set only
      on the struct silently never persists on `caesium job apply` re-apply (the common
      case — this is the round-1 "parses then evaporates" trap re-introduced for updates).
      Add `"priority"`/`"concurrency"`/`"rate_limits"` to `updates` and
      `"rate_limit_resource"`/`"rate_limit_units"` to `taskUpdates`; extend
      `populateTaskFromStep` (~`:594`). Read the columns into the runtime `job` struct in
      `internal/job/job.go` `New()` (~`:127-138`) + the struct def (~`:104`). AutoMigrate
      derives the columns from struct tags (non-null constant defaults are safe on the
      hot tables). The A3 integration test must **update an existing job** (not just
      create one) and assert the value round-trips to runtime.
      Files: `internal/models/job.go`, `internal/models/task.go`,
      `internal/jobdef/importer.go`, `internal/job/job.go`.
      Depends on: A1.
- [ ] A4. Document the schema + ship pinned examples. Add the `concurrency`/`priority`/
      `rateLimits` fields to the three references and one example per capability.
      Files: `docs/caesium-job-llm-reference.md`, `docs/job-definitions.md`,
      `docs/job-schema-reference.md`, `new docs/examples/concurrency-skip.job.yaml`,
      `new docs/examples/priority-ratelimit.job.yaml` (every image fully pinned to a tagged
      form such as `alpine:3.23` — the guardrail rejects an untagged image name;
      `debian:12-slim` is unscanned but fine).
      Depends on: A1.

### Stream B — Priority queues (roadmap §1.4)

Threads a priority value from job metadata (and an optional per-run override) onto the
durable run, propagates it to every task, and teaches the distributed claimer to honor
it — with the supporting index. Strictly ordering, never preemptive.

- [ ] B1. Persist priority on the run + tasks, with the claim index. Add a
      `Priority int` column (`gorm:"not null;default:2"`) to `JobRun` **and** `TaskRun`
      (`internal/models/run.go:11` and `:39`); add a GORM **composite index** on `TaskRun`
      covering the claim predicate (`status`, `outstanding_predecessors`, `claimed_by`) +
      `(priority DESC, created_at ASC)` so the new claimer sort doesn't full-scan the
      hottest query. Stamp priority onto every `TaskRun` from the owning `JobRun` in
      `run.Store.RegisterTasks` (`store.go:781`) — and **add `"priority"` to that function's
      explicit `Select(...)` of the JobRun (`store.go:801`)**, or `jobRun.Priority` reads
      as the zero value (0, below `low`) regardless of what's persisted. Accept a priority
      on the run-creation path defaulting to the persisted `models.Job.Priority` (A3) →
      normal/2. Because both B1 and C1 extend `store.Start` (referenced via the
      `run.Service` interface at `api/rest/service/run/run.go:17` and called at
      `event.go:163`, `post.go:51`, `job.go:363,386`), thread the new inputs via an
      **options struct / variadic**, not a positional arg, so the signature change doesn't
      break five call sites mid-stream. Add a `low|normal|high ↔ 1|2|3` mapping helper.
      Files: `internal/models/run.go`, `internal/run/store.go`,
      `api/rest/service/run/run.go` (interface).
      Depends on: A3.
- [ ] B2. Order the distributed claimer by priority. Change `ORDER BY tr.created_at ASC`
      (`internal/worker/claimer.go:211`) to `ORDER BY tr.priority DESC, tr.created_at ASC`;
      keep the optimistic-lock `UPDATE … WHERE claimed_by = '' …` intact. Increment
      `caesium_task_priority_claim_total{priority}` (3-value label — safe cardinality).
      Files: `internal/worker/claimer.go`, `internal/metrics/metrics.go` (two edit sites).
      Depends on: B1.
- [ ] B3. Add the operator surface for per-run priority across **all** initiation paths.
      New `caesium run start --job-id <id> [--params …] [--priority high|normal|low]`
      subcommand (`cmd/run/run.go` is a parent with `replay`/`diff`/`retry`/`retry-callbacks`
      but no on-demand start verb); add `Priority string` to the REST `PostRequest`
      (`api/rest/controller/job/run/post.go:19`) threaded through `runsvc.Start` →
      `store.Start`; thread priority through the HTTP-trigger fire path
      (`internal/trigger/http/http.go` `FireWithParams`); cron-triggered runs derive
      priority from the persisted `models.Job.Priority` default (A3). A per-run priority
      overrides the job-metadata baseline.
      Files: `cmd/run/run.go` (new subcommand), `api/rest/controller/job/run/post.go`,
      `api/rest/service/run/run.go`, `internal/trigger/http/http.go`,
      `test/concurrency_priority_test.go` (mixed-priority claim-order integration test
      incl. a cron-path assertion).
      Depends on: B1 + B2.

### Stream C — Run concurrency strategies (roadmap §1.3)

Gates a *new run* when the job is already at `maxRuns`. The gate lives in `run.Store.Start`
— the one place REST, cron, and HTTP-trigger all create runs — and is **atomic** so it
holds across nodes.

- [ ] C1. Atomic admission + the simple strategies. Add `run.Store.CountActive(jobID)` and
      an `admit(tx, jobID, strategy, maxRuns)` helper (correction #4) invoked from
      `store.Start`, `resolveRun` (before the `FindRunning` attach), and `StartForBackfill`
      (or backfill excluded from the count). `admit` loads the job's `maxRuns`/`strategy`
      once (`store.Start` takes a `jobID`, not the job), then makes admission a SINGLE
      conditional statement — `INSERT INTO job_runs (…) SELECT <client uuid>, … WHERE
      (SELECT count(*) FROM job_runs WHERE job_id=? AND status='running' AND
      quarantine<>true) < ?` via raw `tx.Exec`, deriving `admitted = (RowsAffected == 1)`.
      **No RETURNING** — the run id is client-generated (`uuid.New()`), and
      `INSERT…SELECT…WHERE(subquery)…RETURNING` has no precedent on caesium's dqlite (only
      plain `UPDATE…RETURNING` at `claimer.go:218`); a throwaway spike must confirm the
      conditional-insert `RowsAffected` semantics against the dqlite test harness before
      committing. On a non-admit: `skip` (count `caesium_run_skipped_total{job_alias,reason}`,
      return a **sentinel `ErrRunSkipped`**) or `fail` (`ErrMaxConcurrentRunsReached`).
      **Surface the outcome at the callers** — `api/rest/controller/job/run/post.go:51`
      and `internal/trigger/event/event.go:163` both map any error → 500 and deref
      `run.ID`; map `fail` → **409**, treat `ErrRunSkipped` → 202/no-op, and guard every
      `run.ID` deref against a nil run.
      Files: `internal/run/store.go`, `api/rest/service/run/run.go`,
      `api/rest/controller/job/run/post.go`, `internal/trigger/event/event.go`,
      `internal/job/job.go` (the `resolveRun` admit call), `internal/metrics/metrics.go`.
      Depends on: A3.
- [ ] C2. The `replace` strategy + the cancellation primitive it needs (correction #9).
      Add a `StatusCancelled` terminal status (+ its `ExecutionEvent` type) and a
      `run.Store.CancelRun` that **atomically** transitions the target `JobRun` AND its
      non-terminal `task_runs` **AND expires its `run_leases` row** in one transaction. The
      claimer ties to job-run status in two places (the inner `JOIN job_runs … jr.status
      ='running'` and the outer `job_run_id IN (SELECT … status='running')`,
      `claimer.go:204-217`), so flipping the JobRun suffices to make its tasks unclaimable;
      flipping the task_runs is belt-and-suspenders, and the lease delete stops
      `RenewOwnedLeases` (`internal/run/lease.go:132`) refreshing the dead run's lease.
      On `replace` at `maxRuns`: cancel the oldest active run, then admit the new one;
      count `caesium_run_replaced_total{job_alias}`; handle a cancelled run in
      `waitForRunCompletion` (`job.go:1376`). **In the trigger/`resolveRun` path,
      `admit`'s `replace` cancels the oldest active run *and signals "create a fresh run"*
      so `resolveRun` proceeds to `store.Start` rather than re-attaching via `FindRunning`
      to a different still-running run** (define `admit`'s return as a decision —
      create / skip / fail — that both `store.Start` and `resolveRun` honor, so `replace`
      and `queue` behave identically across REST and cron).
      Files: `internal/run/store.go`, `internal/run/lease.go`,
      `internal/models/run.go` (status const), `internal/models/execution_event.go`
      (event type), `internal/job/job.go`, `internal/metrics/metrics.go`.
      Depends on: C1.
- [ ] C3. The durable run queue + **leader-gated** dequeuer for `queue`. New `RunQueue`
      catalog model (`uuid.UUID` PK, `JobID` FK→Job, `Params datatypes.JSON`,
      `Priority int`, `ClaimedBy string`, composite index `(job_id, priority DESC,
      created_at ASC)` via GORM tags — not raw SQL); register in `models.All` after `Job`;
      **not** in `hotPathModels`/`hotTables`. `enqueueRun`/`dequeueNextRun` on `run.Store`,
      where `dequeueNextRun` **atomically claims** a row (optimistic `UPDATE run_queue SET
      claimed_by=? WHERE id=(SELECT … ORDER BY priority DESC, created_at ASC) AND
      claimed_by=''`, the `claimer.go:198-218` precedent). A `CAESIUM_RUN_QUEUE_*`-gated
      background dequeuer (`runAsync` in `cmd/start/start.go`) **wrapped in
      `dqlite.IsLocalLeader`** (the `WithReclaimGate` precedent at `start.go:528`) so
      exactly one node drains, priority-first, when `CountActive < maxRuns`. Enforce a max
      queue depth (default 100, oldest evicted). Metrics
      `caesium_run_queue_depth{job_alias,priority}` + `caesium_run_queue_wait_seconds{job_alias}`.
      Files: `new internal/models/run_queue.go`, `internal/models/models.go`,
      `internal/run/store.go`, `new internal/runqueue/dequeuer.go`, `cmd/start/start.go`,
      `pkg/env/env.go`, `internal/metrics/metrics.go`,
      `test/run_concurrency_test.go` (skip/fail/replace/queue integration scenarios).
      Depends on: C1 + C2 + B1 (the queue orders by B's `priority`).

### Stream D — Resource rate limiting (roadmap §1.3, P2)

A durable sliding-window counter that throttles tasks against a named shared resource via
a single atomic statement, re-queuing (not blocking) over-limit tasks. Independent of B/C
except the shared schema (A).

- [ ] D1. Build the durable resource limiter + model. New `RateLimitToken` catalog model
      (composite PK `(resource, window_key)`, `consumed`/`limit_val`/`expires_at`; in
      `models.All` catalog section, **not** hot-sharded); new
      `internal/ratelimit.Limiter.Acquire(resource, units, limit, window) (bool, error)`
      implemented as a **single raw `tx.Exec`** conditional upsert
      (`INSERT … ON CONFLICT(resource,window_key) DO UPDATE SET consumed=consumed+? WHERE
      consumed+? <= limit_val`) — NOT `clause.OnConflict`, which can't express the guard —
      deriving `acquired = (RowsAffected == 1)`. Window granularity ≥ 1 minute (the
      clock-skew note). Metrics `caesium_rate_limit_acquired_total{resource}` +
      `caesium_rate_limit_rejected_total{resource}` (document that `{resource}` cardinality
      is bounded by declared rateLimits, not free-form per request).
      Files: `new internal/models/rate_limit_token.go`, `internal/models/models.go`,
      `new internal/ratelimit/limiter.go`, `internal/metrics/metrics.go`.
      Depends on: A1.
- [ ] D2. Integrate the limiter into task dispatch + add the pruner. Read the persisted
      per-task `RateLimitResource`/`RateLimitUnits` (A3) before dispatch; on rejection,
      re-queue the task `pending` with a retry-after delay (don't hold the slot) and count
      `caesium_run_skipped_total{reason="rate_limit"}`; a `CAESIUM_RATE_LIMIT_*`-gated
      background pruner (`runAsync` in `cmd/start/start.go`) deletes expired windows.
      Files: `internal/job/job.go` (task-dispatch path), `internal/run/store.go`,
      `new internal/ratelimit/pruner.go`, `cmd/start/start.go`, `pkg/env/env.go`,
      `test/rate_limit_test.go` (enforcement + window rollover + multi-task contention).
      Depends on: D1 + A3.

### Stream E — Observability surfaces (P2)

Makes the queue and rate-limit state legible to operators.

- [ ] E1. Add the run-queue inspection CLI + REST read with its RBAC policy. `caesium job
      queue <alias>` lists pending queued runs (position, priority, enqueued-at) via a new
      `GET /v1/jobs/:id/queue` endpoint (controller + service + `bind.go` route). Wire the
      auth correctly (correction class — there is no `api/rest/middleware/rbac.go`): add the
      route→action mapping in `api/middleware/auth.go` (the `auditActionForRoute` switch +
      policy table), the action→role grant in `internal/auth/rbac.go` `endpointPolicy`
      (`"GET /v1/jobs/:id/queue": models.RoleViewer`, or `TestAuthMiddlewareMountedRoutesHaveRBACPolicy`
      hard-fails), and a scoped-key decision in `api/middleware/auth_scope.go` (the
      `/v1/jobs/:id` prefix branch — decide + assert whether a job-scoped key may read its
      own queue). Clean stdout via `runCLISeparate` (assert stdout parseable AND logs on
      stderr, contrasting the stream-merging `runCLIRaw`).
      Files: `new cmd/job/queue.go`, `new api/rest/controller/job/queue/`,
      `api/rest/service/...`, `api/rest/bind/bind.go`, `api/middleware/auth.go`,
      `internal/auth/rbac.go`, `api/middleware/auth_scope.go`,
      `test/job_queue_cli_test.go` (incl. a scoped-key assertion).
      Depends on: C3.
- [ ] E2. Surface the queue + rate-limit state in the web UI. Run-queue panel on the
      job-detail page and a rate-limit status indicator on rate-limited tasks; add the
      capability to the `Features` struct (`api/rest/service/system/system.go`) if
      UI-gated; a Playwright e2e drives the real UI against a live backend.
      Files: `ui/src/features/jobs/`, `ui/src/lib/api.ts`, `ui/src/router.tsx`,
      `api/rest/service/system/system.go`, `ui/e2e/` (new spec).
      Depends on: C3 + D2 + E1.

## Harness Strengthening

- [ ] H-1. Enable the new config gates on the integration server so CI exercises the real
      paths. Add `CAESIUM_RUN_QUEUE_*` + `CAESIUM_RATE_LIMIT_*` to `just integration-up`
      (the lineage `CAESIUM_OPEN_LINEAGE_ENABLED` precedent) so the dequeuer + pruner +
      rate-limit path actually execute under `just integration-test`.
      Files: `justfile`, `test/` harness setup.
      Depends on: C3 + D2.

## Navigational / Organizational Improvements

- [ ] N-1. Flip the roadmap + reconcile the design doc banner. `docs/roadmap.md` §1.3 +
      §1.4 → Shipped (with the delivered surface); flip the `> Status:` banner on
      `docs/design-concurrency-priority.md` (Proposed → Shipped — **mandatory**, or
      `TestPlanningAndHistoricalDocsCarryStatusBanner` fails) and fold in the nine
      Source-Of-Truth corrections (task-vs-run semaphore, the catalog-persistence
      requirement, `run.Store` method placement, the `run.Store.Start` choke point, the
      atomic-admission requirement, the cache-hash exclusion, JobRun→TaskRun propagation +
      claim index, the catalog-table shapes + raw-SQL upsert, the cancellation primitive).
      The README already indexes `design-concurrency-priority.md` (no add needed).
      Files: `docs/roadmap.md`, `docs/design-concurrency-priority.md`.
      Depends on: every runtime stream merged (runs last).

## Sequencing & Dependencies

**Cross-stream order:**
- **Stream A merges first** — B, C, D all read the `models.Job`/`models.Task` columns A3
  persists (not just A1's schema). A owns `definition.go` + `hash.go` + the importer/model
  wiring, which no other stream touches.
- **B before C** — C3's run-queue dequeuer orders by B1's `priority` column, and both edit
  `internal/run/store.go` + the run-initiation path; sequencing avoids a true conflict.
- **D is independent of B/C** (own `internal/ratelimit` + `rate_limit_tokens`) and can run
  in parallel with B; D2 depends on A3 for the persisted per-task rate-limit fields.
- **E + H-1 + N-1 run last** — they observe/enable/document the runtime streams.
- Suggested waves: **W1** = A; **W2** = B + D (parallel); **W3** = C; **W4** = E + H-1 + N-1.

**Within-stream order:**
- A: A1 → (A2, A3, A4 — A3 is the runtime-wiring item B/C/D depend on).
- B: B1 → B2 → B3.   C: C1 → C2 → C3.   D: D1 → D2.   E: E1 → E2.

**Cross-stream file conflicts:**
- `internal/run/store.go` — **B** (Start/RegisterTasks priority) + **C** (CountActive/
  admission/enqueue/dequeue/cancel) + **D** (re-queue). Sequence **B → C → D**; different
  functions, but sequence (don't parallel-edit) the largest file.
- `internal/job/job.go` — **A3** (struct/New wiring) + **C2** (cancel handling) + **D2**
  (dispatch acquire). A3 first (W1), then C, then D.
- `internal/models/models.go` — **C** (`RunQueue`) + **D** (`RateLimitToken`) append to
  `All` at different points (additive). FK order: `RunQueue` after `Job`,
  `RateLimitToken` in the catalog section.
- `internal/metrics/metrics.go` — **B**, **C**, **D** each add metrics (two edit sites).
  Additive; mechanical rebase.
- `pkg/env/env.go` / `cmd/start/start.go` — **C** (dequeuer) + **D** (pruner) add fields /
  `runAsync` blocks (additive).
- `internal/trigger/http/http.go` — **B** (priority) + (no C edit; C gates in store.Start).
- No stream adds a third-party dependency, so **`go.sum` should not conflict**; if one
  does, resolve with `go mod tidy`, not a hand-merge.

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Per-stream conditional gates:
- **A** (job-schema change): `caesium job lint --path docs/examples/` rejects the bad
  enums/durations; the `hash_test.go` stability case proves the new fields are excluded
  from the cache key; an integration assertion proves a `maxRuns`/`priority` set in YAML
  is **readable at runtime** (round-trips through `models.Job`) — not just parsed.
- **B/C/D** (new metric): assert via `internal/metrics/testutil` in a `*_test.go` (the
  metric must also be in `Register()`); **B/C/D ship a `test/` integration scenario**
  driving the real surface (mixed-priority claim order through `caesium run start`;
  skip/fail/replace/queue through a real trigger; rate-limit enforcement) — not a unit
  test on the internal function.
- **C1/C3** (distributed admission): an integration scenario fires two near-simultaneous
  runs against `maxRuns: 1` and asserts exactly one is admitted (the TOCTOU guard holds).
- **E** (`ui/**`): `just ui-lint && just ui-test && just ui-e2e`.
- **B3/E1** (machine CLI output): `runCLISeparate` — stdout clean/parseable AND logs on
  stderr (contrast the stream-merging `runCLIRaw`).
- This plan's checkbox ticked, the per-stream `## Progress` bullet appended for the active
  wave, and any cross-linked doc refreshed in the same PR.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream A — schema foundation** is a runtime contract, not just a parser change: a job
   with `priority` / `concurrency` / `rateLimits` parses, lint-validates (`caesium job
   lint` rejects a bad `strategy`/`priority`/`window`), **round-trips through `models.Job`
   so the runtime reads it** (an integration assertion proves the value reaches the engine,
   not just the YAML), and a `hash_test.go` case proves changing only those fields yields
   an identical task hash.
2. **Stream B — priority queues** orders claims: a `test/` integration scenario fires three
   runs at `high`/`normal`/`low` (via `caesium run start --priority` and the REST field)
   and asserts the distributed claimer drains them high→normal→low over an index, not a
   full scan; priority defaults to `normal`.
3. **Stream C — run concurrency strategies** enforces every policy **atomically**:
   integration scenarios prove `skip`/`fail` at `maxRuns`, `replace` cancels the oldest
   active run (its in-flight tasks become unclaimable), `queue` parks then dequeues
   (priority-first, one node) when a slot frees, and two near-simultaneous admissions
   against `maxRuns: 1` admit exactly one — exercised through a real trigger.
4. **Stream D — resource rate limiting** throttles: an integration scenario proves the
   `(limit+1)`-th acquire in a window is rejected (`RowsAffected==0`) and the task
   re-queued, the window rolls over, and the pruner removes expired rows.
5. **Stream E — observability** is operator-legible: `caesium job queue <alias>` lists
   queued runs (clean stdout, scoped-key behavior asserted) and the job-detail UI shows the
   run queue + rate-limit state, each gated by a green integration/e2e test.
6. **H-1** wires `CAESIUM_RUN_QUEUE_*` + `CAESIUM_RATE_LIMIT_*` into `just integration-up`.
7. `docs/roadmap.md` §1.3 + §1.4 read **Shipped**, `design-concurrency-priority.md`'s
   banner is flipped + reconciled to the as-built contract, and this plan's per-stream
   `## Progress` entries match the merged PRs.

## How To Pick Up Work

1. Read this file end-to-end so you understand the streams, their interdependencies, and
   which acceptance criterion the item closes.
2. Pick an unchecked item under `## Streams` whose `Depends on:` line is satisfied
   (consult `## Sequencing & Dependencies`).
3. Branch from `master` (or land in a worktree if dispatched by `exec-plan-wave`); do the
   work as a self-contained PR.
4. Run the verification block under `## Verification (Run For Every PR)`.
5. Tick the checkbox for your item, add a per-stream bullet to the active wave subsection
   in `## Progress`, and update any cross-linked design doc / roadmap section in the same
   PR.
6. Open the PR with title format
   `<Imperative subject> (concurrency-priority-queues <wave>-<stream>)` — e.g.
   `Add priority claim ordering (concurrency-priority-queues W2-β)`. GitHub appends
   `(#NNN)` on squash-merge.

## Cross-References

- [`docs/roadmap.md`](../../roadmap.md) — §1.3 (Concurrency Strategies & Rate Limiting),
  §1.4 (Priority Queues); the priority/status source of truth.
- [`docs/design-concurrency-priority.md`](../../design-concurrency-priority.md) — the
  design of record; reconciled to the as-built contract by N-1.
- [`pkg/jobdef/definition.go`](../../../pkg/jobdef/definition.go) — the job-definition
  schema; the field-shape source of truth.
- [`internal/cache/hash.go`](../../../internal/cache/hash.go) — the cache identity; the
  scheduling fields must be excluded (the `Kueue.QueueName` precedent at `:329-336`).
- [`internal/worker/claimer.go`](../../../internal/worker/claimer.go) — the distributed
  task claimer that Stream B reorders by priority.
- [`internal/run/store.go`](../../../internal/run/store.go) — the run store; the atomic
  admission choke point (`Start`) and the cancellation primitive live here.
- Completed sibling: [`event-trigger-routing.md`](../completed/event-trigger-routing.md) —
  the same plan shape (schema-foundation-first, source-of-truth amendments, e2e-gated).
