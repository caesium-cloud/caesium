# Design: Quarantined Replay

> Status: Design - authoritative for replay safety invariants (data-plane-memory-ii B1).

This memo is the B1 design gate for
[`data-plane-memory-ii`](exec-plans/active/data-plane-memory-ii.md). It fixes the
replay safety model before B2-B6 add runtime code. It complements
[`design-data-plane-memory.md`](design-data-plane-memory.md): that older design is
authoritative for the shipped substrate and honest-scope rules, while this memo
and the active plan are authoritative for replay quarantine, idempotency,
side-effect suppression, observability isolation, and the `replaySafe` gate.

## Overview

Quarantined replay is a cache-backed what-if run over a completed baseline run.
The operator supplies override params with `--set key=value`; Caesium constructs a
new replay run, reuses baseline cache/results for tasks whose full effective
identity is unchanged, and re-executes tasks whose identity changes.

The cache-correctness invariant is unchanged: a miss is always safe; a false hit
is a bug. Replay therefore never hides overrides, secret drift, descriptor gaps,
or missing baseline cache entries to manufacture a hit. When it cannot prove that
an unchanged task can reuse its baseline result, replay fails closed before
dispatching that task.

"Quarantine" means the replay run is non-authoritative inside Caesium: it cannot
write production cache entries, cannot emit authoritative lineage, cannot reach
Caesium-controlled callbacks/notifications/SSE default streams, and cannot pollute
production health metrics or run lists. Quarantine does not sandbox the container.
A re-executed task still runs its real container command with its configured
engine, mounts, secrets, network, and workload identity. This is why v1 has one
durable gate: only jobs/steps recorded as `replaySafe` at baseline execution time
may be replayed.

## Threat Model

Replay must defend against these failures:

- A scoped runner replays an unreviewed deploy/delete job under a "what-if"
  banner and hits production systems.
- A quarantined run writes a `TaskCache` row that a later production run treats as
  authoritative.
- A quarantined run emits OpenLineage or writes `lineage_datasets`, causing the
  impact graph to treat what-if data as production data.
- A quarantined run fires callbacks, notification webhooks, Slack/email/PagerDuty
  messages, or default `/events` SSE automation.
- A slow or failed replay dents production stats, Prometheus alerts, notification
  alert counters, or UI run lists.
- A client retry after timeout creates a second replay and re-runs real container
  side effects.
- A replay silently re-executes a task whose baseline cache/result was pruned.
- A replay reconstructs work from live `Job`/`Task`/`Atom` rows after an apply,
  not from the baseline's immutable execution descriptor.
- A rotated or missing `secret://` value changes task behavior while the replay
  claims identical-code semantics.

The defensive posture is fail-closed. Missing markers, missing descriptors,
missing idempotency keys, missing baseline results, secret identity mismatch, and
unmarked `replaySafe` state all abort. "Degraded" is reporting language only; it
never grants permission to execute with drifted inputs.

## Quarantine Isolation

The quarantine marker rides three coherent carriers fed from one source — the
owning run/task marker. Two are persisted, queryable columns; the third is the
in-process event carrier every live-bus subscriber checks.

Persist quarantine in three queryable places:

- `JobRun.Quarantine bool`, `gorm:"not null;default:false;index"`.
- `TaskRun.Quarantine bool`, `gorm:"not null;default:false;index"`, copied onto
  every materialized task row for the replay run before dispatch.
- `ExecutionEvent.Quarantine bool`, `gorm:"not null;default:false;index"`, copied
  from the run/task marker at event creation time
  (`internal/models/execution_event.go` — added alongside the existing
  `BusDispatchPending` backlog column). This persisted twin is read ONLY by the
  backlog query (`internal/event/store.go` `ListSince`) and the
  `internal/event/bus_dispatch.go` republisher.

Add the live-channel half of the marker so in-process bus consumers suppress
without a DB round-trip:

- `event.Event.Quarantine bool` (`internal/event/bus.go`, on the `Event` struct
  which today has `Sequence/Type/JobID/RunID/TaskID/Timestamp/Payload` and NO
  quarantine field). It is set at publish time from the owning run/task marker
  and is the single authoritative field every live subscriber checks. The
  notification subscriber drops `evt.Quarantine` at the top of `handleEvent`
  before `recordEventMetric` (`internal/notification/subscriber.go`); the lineage
  subscriber drops it at the top of `handleEvent` before `mapEvent` /
  `persistTaskDatasets` / `transport.Emit` (`internal/lineage/subscriber.go`);
  the `/events` SSE stream filters it (the bus `Filter` in `internal/event/bus.go`
  gains an `ExcludeQuarantine` predicate checked in `matches`, OR the controller
  in `api/rest/controller/event/stream.go` drops marked events before
  `writeEvent` — the live loop there has no drop logic today and would otherwise
  leak every quarantined lifecycle event to default UI/SSE/automation clients).

All three carriers are populated from the same run/task marker; the `event.Event`
field and the `ExecutionEvent` column must be stamped from the same value so the
backlog republisher and the live bus agree.

Default queries that serve production surfaces use `quarantine IS NOT TRUE` or a
backfilled `false`, never a nullable `= false` predicate that would hide
historical rows after migration.

Cache behavior is deliberately asymmetric:

- Cache reads are allowed for unchanged tasks, because the baseline cache entry is
  the proof that the same task identity already produced the result.
- Authoritative cache writes are forbidden for quarantined tasks. Local execution
  in `internal/job/job.go` and distributed execution in
  `internal/worker/runtime_executor.go` both write cache entries after success
  today, so both must branch on `TaskRun.Quarantine` before `cacheStore.Put` and
  before value-verified short-circuit publication to production metrics/state.
- If replay wants private per-replay cache accounting later, it must use a
  separate replay-only store or replay-only series. It must not write `TaskCache`
  entries that production cache lookup can hit.

Result-vs-cache precedence for an unchanged task (the two reuse sources have
different lifetimes — the by-`Hash` `TaskCache` row prunes via `CacheExpiresAt`,
while the baseline `TaskRun.Result`/`Output` persists):

- For an unchanged task, replay MAY reuse the baseline `TaskRun.Result`/`Output`
  even when the by-`Hash` `TaskCache` row was pruned, because the baseline
  `TaskRun` is immutable proof the identity already produced that result. The
  durable baseline result is authoritative.
- Hard-abort only when NEITHER a live `TaskCache` entry NOR a non-empty baseline
  `TaskRun.Result`/`Output` is present.
- A present-but-empty `Result` for a task that produced output at baseline is
  itself a hard abort (descriptor/result corruption), not a hit. This makes B3's
  fail-closed branch and B6's pruned-cache test precise.

Lineage behavior is also asymmetric:

- Replay may read baseline lineage/diff state.
- Replay must never call OpenLineage transports or write `lineage_datasets`.
  Suppression belongs in the subscriber/mapper path before both `transport.Emit`
  and dataset persistence, because lineage emission is currently driven from the
  event bus, not directly from the executor.

## Override->Hash Plumbing

`--set key=value` overrides are normalized into the replay `JobRun.Params` before
task identity is computed. The normalized map is sorted by key, rejects duplicate
or malformed keys at the CLI/API boundary, and is part of the idempotency
fingerprint.

The replay task hash is computed from the same `cache.HashInput` contract as
normal runs:

- job alias and task name
- image and `ResolvedImageDigest`
- command
- env plus predecessor output env
- workdir
- mounts and resolved volume mounts
- Kubernetes spec
- predecessor hashes and predecessor outputs
- run params
- cache version

Current source injects every run param as `CAESIUM_PARAM_<KEY>` for every task
(`internal/job/job.go`, `internal/worker/runtime_executor.go`) and folds the full
run params map into `HashInput.RunParams` (`internal/cache/hash.go`). Therefore
v1 must be conservative: a task is "unchanged" only when its full reconstructed
`HashInput` is byte-identical after overrides and predecessor/effective-hash
substitution. Do not hide an override from `HashInput` to force selective hits.
If a future version wants narrower per-step param dependencies, it needs an
explicit schema/API/test contract; command or script inspection is not a safe
dependency analysis.

Replay uses this decision procedure per task:

1. Reconstruct the task's effective runtime envelope from the immutable baseline
   descriptor.
2. Apply the replay run params and predecessor outputs/effective hashes produced
   by earlier replay tasks.
3. Compute the normal `HashInput`.
4. If the hash equals the baseline effective hash, require the baseline cache entry
   or recorded baseline result to be present and use it as a cache hit.
5. If the hash differs, execute the real container command only after the
   `replaySafe` and descriptor/secret checks pass.
6. If any required unchanged-task baseline cache/result is absent or expired,
   abort the replay. Do not silently re-execute an "unchanged" task.

### Consequence: any override re-runs the full DAG in v1

`internal/cache/hash.go` folds the entire global `RunParams` map into every
task's hash (the sorted `param:<k>=<v>` lines, hash.go ~384-392), and both
executors inject every run param as `CAESIUM_PARAM_<KEY>` into every task. So any
`--set` override changes every task's identity:

- v1 replay re-executes the FULL DAG whenever any param is overridden — the
  cache-hit path (step 4) is reached only for a no-override replay (or an override
  re-resolving to byte-identical params, e.g. `--set k=<existing value>`). There
  is no "re-run only the affected task while the rest cache-hit" under wholesale
  `RunParams` hashing.
- Selective per-task re-run requires a future per-step param-dependency contract
  (an explicit schema/API declaring which params each step consumes, so unaffected
  steps keep their hash). That is out of v1 scope. v1 must NOT hide an override
  from `HashInput` to manufacture selective hits — command/script inspection is
  not a safe dependency analysis and a hidden override is a false-hit bug.

This is why the plan's Stream B intro and B6 acceptance assertion are scoped to
the honest v1 behavior: a no-override replay cache-hits all unchanged tasks; any
`--set` param override re-runs the full DAG.

## `--diff` Contract

`caesium run replay <run-id> --job-id <id> --set k=v --diff` has two phases:

1. Create or resume the quarantined replay run through the replay endpoint.
2. After the replay reaches a terminal state, call the Stream A run-diff endpoint
   (`GET /v1/jobs/:id/runs/diff?left=<baseline>&right=<replay>`, Stream A / plan
   A2) with baseline as the left run and replay as the right run. **B5
   hard-depends on A2**: `--diff` has no diff surface without the A2 endpoint.

Await contract (replay re-executes real containers and may run long):

- The CLI polls the replay run status until terminal or a `--timeout` elapses.
- On timeout it returns the replay run id and a non-zero exit with no synthetic
  diff (the operator can re-poll or re-attach with the printed idempotency key).
- `--diff` never blocks server-side; awaiting is a client-side poll loop.
- A still-running replay yields the replay pending/error status, not a partial
  diff.

The diff is replay-vs-baseline, not live-job-vs-replay. It reports the same causal
field changes as `run diff`/`why`: `HashInput` fields, task status/cache-hit
state, and degraded indicators when the substrate lacks field-level detail. It
does not claim row/column data diffs and does not compare external datasets.

If replay aborts before dispatch or during fail-closed validation, `--diff`
returns the replay error and no synthetic diff. If the baseline or replay
`HashInputBlob` is oversized or missing, the diff degrades honestly to digest-level
where Stream A already does so.

## Honest Scope

Replay does not resurrect data. It re-executes baseline code against available
inputs, pinned digests when present, run params, and predecessor outputs/results
recorded by Caesium. If a source table, object, volume, or external API response
changed outside Caesium's recorded data contracts, replay cannot restore the old
value.

Pinned image digests are part of the identical-code claim. When a baseline task
has `ResolvedImageDigest`, replay must use that descriptor value when reconstructing
the task and must report if current resolution disagrees. When the baseline used
an unpinned or unresolved mutable tag, replay is degraded: it may still be allowed
for a `replaySafe` task, but surfaces must not claim content-identical code.

The system may report "degraded" for unpinned tags or oversized/missing diff
blobs. It must not report "degraded" for missing descriptors, missing baseline
cache/results for unchanged tasks, unresolvable secrets, or secret identity
mismatch; those are hard pre-dispatch aborts.

## The Single `replaySafe` Gate

The only containment mechanism in v1 is a durable, baseline-scoped `replaySafe`
mark on the job/step schema, implemented by B7 and recorded on the baseline
`TaskRun` when that task ran.

Rules:

- A baseline task without recorded `replay_safe = true` is refused, full stop.
- The gate reads the baseline `TaskRun` record, not the current live job
  definition. A later apply cannot retroactively authorize an older unsafe run.
- Request bodies cannot set or clear quarantine. An attempted `quarantine: false`,
  `force`, acknowledgement field, or alternate namespace request is rejected.
- There is no CLI `--force` or inline "I know it is risky" flag.
- Non-quarantined re-execution is the existing `caesium run retry` flow, not
  replay.
- Any future break-glass is a separate admin-only, audited workflow with its own
  schema, RBAC, and tests. It is named here only to keep it out of v1.

The alternate environment namespace idea is cut from v1. It did not prove that
secrets, volumes, workload identity, network destinations, and external refs were
redirected off production, and it had no schema/API/RBAC/test contract.

## Side-Effect Producer Audit

Audit commands used while writing this memo:

```sh
rg -n "Subscribe\\(|Publish\\(|persistAndPublish|transport\\.Emit|g\\.GET\\(\"/events\"|ExecutionEvent|callback\\.Default\\(\\)\\.Dispatch|RegisterSender|http\\.NewRequestWithContext|smtp|pagerDutyEventsURL" internal api cmd pkg -g '*.go'
rg -n "LineageEventsEmitted|NotificationSendsTotal|recordEventMetric|TaskFailuresTotal|RunTimeoutsTotal|SLAMissesTotal" internal -g '*.go'
rg -n "cacheStore\\.Put|storeCacheEntry|TaskCacheHitsTotal|TaskCacheMissesTotal|TaskCacheShortCircuitsTotal|TaskRetriesTotal" internal/job internal/worker internal/run -g '*.go'
```

Confirmed Caesium-controlled outward producers:

| Producer | Source evidence | Quarantine rule |
|---|---|---|
| Callback dispatch | `internal/job/job.go` calls `callback.Default().Dispatch`; `internal/callback/callback.go` creates `CallbackRun` rows and invokes handlers; `internal/callback/notification.go` performs HTTP notification callbacks. | Skip callback dispatch for quarantined runs before callback rows, callback metrics, or HTTP handlers are created. `RetryFailed` must not retry callbacks for quarantined runs. |
| Notification subscriber | `internal/notification/subscriber.go` subscribes to lifecycle events and handles `RunCompleted`, `TaskSucceeded`, `RunFailed`, `TaskFailed`, `RunTimedOut`, and `SLAMissed`. | Drop quarantined events at the start of `handleEvent`, before `recordEventMetric`, policy matching, channel load, send duration, or send counters. |
| Notification watcher | `internal/notification/watcher.go` scans `models.JobRun` directly in `scanRunningRuns` and `scanCompletedBySLA`, then emits via `persistAndPublish` through three emitters: `run_timed_out` (`emitTimeoutEvent` :248), and `sla_missed` from both `emitSLAEvent` (:266) and `emitCompletedBySLAEvent` (:291) — both the latter use `event.TypeSLAMissed`. | Exclude quarantined runs from both scans (`scanRunningRuns` + `scanCompletedBySLA`), which suppresses all three emitters. Do not create timeout/SLA events for replay runs at all. |
| Notification senders | `cmd/start/start.go` registers webhook, Slack, email, and PagerDuty senders; implementations post HTTP, send SMTP, or call PagerDuty Events. | Covered by subscriber/watcher suppression. The senders should never be invoked for quarantined events. |
| OpenLineage subscriber and dataset persistence | `internal/lineage/subscriber.go` subscribes to run/task lifecycle events and calls `transport.Emit`; `internal/lineage/mapper.go` persists `lineage_datasets` for task events before returning the OpenLineage event. Transports include HTTP, file, and console. | Drop quarantined events before mapper side effects. No transport emit, no lineage metric, and no `lineage_datasets` row. |
| `/events` SSE live stream and backlog | `api/rest/bind/bind.go` registers `GET /events`; `api/rest/controller/event/stream.go` subscribes to the live bus and reads backlog through `internal/event/store.go`; `internal/models/execution_event.go` has `Sequence`, `Type`, `JobID`, `RunID`, `TaskID`, `Payload`, `BusDispatchPending`, `BusDispatchedAt`, `CreatedAt` — but NO queryable quarantine column, and `Payload` JSON is not filterable by the store/backlog (which filter by job/run/type). | Add `execution_events.quarantine` alongside the existing `BusDispatchPending` backlog column. Default live stream and backlog exclude quarantined events. The live loop in `stream.go` has no drop logic today, so the controller (or the bus `Filter`) must check the marker before `writeEvent`. The initiating client uses an explicit replay-scoped subscription authorized by run owner. |
| Event bus backlog dispatcher | `internal/event/bus_dispatch.go` republishes pending `ExecutionEvent` rows to the bus. | Preserve and honor the event quarantine column when replaying pending rows, and stamp the republished `event.Event.Quarantine` from the column so the live carrier matches. Do not leak quarantined rows into default subscribers after restart. |
| Cron catchup watermark | `internal/run/store.go` `LatestSuccessfulCronRun` (filters `job_id` / `status = succeeded` / `trigger_type = cron` ONLY, no quarantine predicate, store.go ~2462); consumed by `internal/trigger/cron/cron.go` `fireCatchup` (~163) as `since := latest.StartedAt`, the missed-run watermark. | Add `quarantine IS NOT TRUE` to `LatestSuccessfulCronRun` (and any watermark/baseline-selection query), OR guarantee replay runs never carry `trigger_type = cron`. A quarantined replay that inherits `trigger_type = cron` and succeeds would become the latest successful cron run, advancing the watermark and silently suppressing real scheduled production runs — a scheduling corruption from a what-if. |

The audit did not find another independent authoritative producer beyond these
surfaces. It did find concrete notification sender transports and OpenLineage
transport variants behind the subscriber paths; those are intentionally covered by
the single subscriber/watcher suppression rule instead of per-sender patches.
Read-only log endpoints and UI run/log views are observability surfaces, covered
below, not independent emitters.

Marker-stamping chokepoint. Lifecycle events are created and persisted in
`internal/run/store.go`: `recordTaskEventTx` (`TypeTaskStarted`/`Succeeded`/
`Failed`/`Skipped`, calling `event.Store.AppendTx` at ~2735) and the inline
`RunCompleted`/`RunFailed` events at the run-finalize tx (~2305-2336, `AppendTx`
at 2321/2333). All event creation flows through `event.Store.AppendTx` (callsites
in store.go at 237/377/475/1329/2321/2333/2735/3114). The quarantine marker MUST
be set on `execution_events.quarantine` at this creation site (derived from the
owning `JobRun.Quarantine` in the same tx) AND on the in-memory `event.Event`
that is published — `event.Store.AppendTx` only copies passed-in event fields and
never reads `JobRun`, so the value must flow from `run.Store` at the point the
event is constructed. A column added without populating it at this chokepoint is
green-but-hollow.

Event publish callsites inspected for completeness:

- `internal/run/store.go` — run/task lifecycle (the marker-stamping chokepoint, in
  scope).
- `internal/notification/watcher.go` — timeout/SLA (in scope, see watcher row).
- `internal/event/bus_dispatch.go` — backlog republish (in scope, see dispatcher
  row).
- `api/rest/service/job/job.go` — job-apply lifecycle events (job-scoped, NOT
  replay-run-scoped, out of scope).

Single suppression invariant:

- A quarantined run is stamped in three coherent places fed from one source:
  `JobRun`/`TaskRun.Quarantine` (run/task rows), `event.Event.Quarantine` (every
  live-bus event), and `ExecutionEvent.Quarantine` (the persisted/backlog twin).
- Any bus subscriber checks `event.Event.Quarantine`; any DB query / aggregate /
  stream-backlog filters the column. Any Caesium-controlled external consumer or
  producer must check the marker before sending, persisting authoritative derived
  data, incrementing production-facing metrics, or serving default live/backlog
  streams.
- If a future producer cannot prove a run/event is non-quarantined, it suppresses
  and logs. A new bus producer that ignores `event.Event.Quarantine`, or a new
  query that omits the column filter, is a review-blocking regression.

## Observability Isolation

Quarantined runs are excluded from existing production counters, aggregates,
alerts, and default UI lists. This is mandatory exclusion, not a `quarantine`
label on existing series. Existing PromQL like `sum(caesium_job_runs_total)` would
still include a labelled replay series unless every dashboard and alert changed.
If replay accounting is useful, add separate `*_replay_*` series.

Database/API aggregates to filter:

- `api/rest/service/stats`: recent runs, success rate, average duration, top
  failing jobs, top failing atoms, slowest jobs, and success-rate trend all query
  `job_runs`/`task_runs` today and must add quarantine exclusion.
- Run-list/UI payloads: `run.Store.List`, `Latest`, `LatestSuccessfulCronRun`,
  and ANY run-selecting query (latest/recent/baseline/watermark pickers) exclude
  quarantined runs by default via `quarantine IS NOT TRUE`; only replay-scoped
  views opt in. `LatestSuccessfulCronRun` is a scheduling control input
  (the cron catchup watermark, see the producer-audit row), not merely a display
  query — omitting its filter corrupts production scheduling, not just a UI list.
- `/events` default SSE and backlog exclude replay events as described above.
- Lineage impact queries must be unaffected because quarantined runs write no
  `lineage_datasets` rows.

`internal/metrics` series replay touches and the v1 rule:

These six run/task health series are incremented in `internal/run/store.go`, NOT
in the executor (`internal/job/job.go` / `internal/worker/runtime_executor.go`):
`JobsActive.Inc` at run-start (~417/509/3133); `JobRunsTotal` / `JobsActive.Dec`
/ `JobRunDurationSeconds` at finalize (~2361-2372); `TaskRunsTotal` /
`TaskRunDurationSeconds` at task-completion (~1529/1801/2050);
`TaskRegisterBatchSize` (~526). **B2 must gate every one of these increments on
`JobRun.Quarantine` / `TaskRun.Quarantine` inside `run.Store`, in addition to the
executor cache/lineage/callback suppression.** Suppressing only in `job.go` and
`runtime_executor.go` leaves all run/task health metrics firing — a full
production run-health set per quarantined replay, and a dent in
`JobRunsTotal{status=failed}` on a failed what-if, directly violating the
non-pollution invariant.

- `caesium_jobs_active`: replay would increment at run start today (the
  `JobsActive.Inc` sites above). This gauge must remain untouched for the full
  replay lifetime, including mid-run scrapes. **Suppression must skip BOTH the
  `JobsActive.Inc` AND the `s.startedRuns[model.ID]` insertion that gates the
  finalize-time `Dec` (store.go ~2364-2370) for quarantined runs** — skipping only
  the `Inc` while still registering in `startedRuns` drives the gauge negative on
  completion. A quarantined run never enters the active-set bookkeeping at all.
- `caesium_job_runs_total` (finalize, store.go ~2361)
- `caesium_job_run_duration_seconds` (finalize, store.go ~2372)
- `caesium_task_register_batch_size` (store.go ~526)
- `caesium_task_runs_total` (task-completion, store.go ~1529/1801/2050)
- `caesium_task_run_duration_seconds` (task-completion, store.go ~1532/1803/2053)
- `caesium_task_retries_total`: in the executor retry path
  (`internal/worker/runtime_executor.go`), NOT in `run.Store` — suppress at the
  same `Quarantine` branch as the cache `Put`.
- `caesium_task_cache_hits_total`
- `caesium_task_cache_misses_total`
- `caesium_task_cache_short_circuits_total`
- `caesium_task_cache_entries`: replay must not write authoritative `TaskCache`
  rows, so this production cache gauge must not change because of replay cache
  writes.

The cache hit/miss/short-circuit counters and `task_retries_total` live in the
executor read/retry path (`internal/job/job.go` and
`internal/worker/runtime_executor.go` — the miss counter is local-only on the
worker), and suppress at the same `Quarantine` branch as the cache `Put`. This is
DISTINCT from the six run/task health counters above, which live in `run.Store`;
an implementer must gate both surfaces, not just the executor.
- `caesium_callback_runs_total`: callback dispatch is suppressed before callback
  rows or metrics.
- Distributed/control-plane execution series that a replay can touch in
  distributed mode: `caesium_worker_claims_total`,
  `caesium_worker_claim_contention_total`,
  `caesium_worker_lease_expirations_total`,
  `caesium_reclaim_duration_seconds`, `caesium_dispatch_sent_total`,
  `caesium_dispatch_rejected_total`, `caesium_complete_rejected_total`,
  `caesium_complete_retryable_total`,
  `caesium_complete_report_failed_total`,
  `caesium_run_lease_renewals_total`, and `caesium_run_leases_owned`. B2 must
  make an explicit call-site decision for each. Default policy is not to count
  replay work in existing production task/dispatch health series; add replay-only
  series if operators need replay load accounting.
- DB load series that replay can touch:
  `caesium_db_busy_retries_total`, `caesium_db_writes_total`, and
  `caesium_db_statements_total`. These measure actual storage load, not
  production run health. If retained for capacity visibility, dashboards must
  classify them as control-plane load; if used as production run SLO inputs, add
  replay-only variants and suppress replay from the existing series.

`internal/notification/metrics.go` series to suppress before increment:

- `caesium_task_failures_total`
- `caesium_run_failures_total`
- `caesium_run_timeouts_total`
- `caesium_sla_misses_total`
- `caesium_notification_sends_total`
- `caesium_notification_send_duration_seconds`

The notification subscriber currently calls `recordEventMetric` before dispatch,
so suppression must happen before `recordEventMetric`, not merely before the
sender.

`internal/lineage/metrics.go` series to suppress:

- `caesium_lineage_events_emitted_total`
- `caesium_lineage_emit_duration_seconds`

Series a replay does NOT touch, verified by call-site (so a downstream
implementer can distinguish "deliberately excluded" from "forgotten"):

- `caesium_trigger_fires_total` — cron path only
  (`internal/trigger/cron/cron.go` :138/:186), not reached by replay.
- `caesium_backfill_runs_total` and `caesium_backfills_active` — backfill path
  only (`internal/job/backfill.go` ~141/142/245/257/260), not reached by replay.
- The auth/SSO/audit/webhook control-plane series.

These need no replay handling in v1. A future change that routes replay through a
trigger or backfill MUST revisit them.

Control-plane request/auth/audit metrics for the operator's API request are not
replay-run health. They may still record the authenticated replay API call, just as
they record any other REST request.

## Idempotency & Recoverable Creation

Replay creation is idempotent because it can run real side effects. The REST
endpoint requires a non-empty `Idempotency-Key`; missing or blank is `400`.

The durable identity is `JobRun.ReplayFingerprint`, a nullable column with a
unique index. It is not the raw key. It is a scoped fingerprint over:

- `job_id`
- `baseline_run_id`
- authenticated actor/principal
- normalized overrides
- `Idempotency-Key`

The normalized tuple is encoded deterministically and stored as a fixed-length
digest. When the tuple may include sensitive operator context (actor/principal,
normalized overrides) it MUST be a keyed HMAC with a versioned server key — not a
plain reusable hash — for the same offline-guessability reason as the secret
identity. (Idempotency reservations are short-lived, so a server-key rotation can
at most fail to dedup a retry that spans the rotation, never an unbounded
historical window.) Two unrelated replays that reuse `retry-1` must not collide
globally.

Creation is atomic:

1. Validate job/run ownership, `replaySafe`, descriptors, baseline cache/results,
   and secret identities to the extent possible before dispatch.
2. In one DB transaction, insert the quarantined `JobRun` with
   `ReplayFingerprint`, materialize replay work, and mark replay state `pending`.
3. Commit before dispatch.
4. Dispatch only from durable `pending`.
5. On duplicate fingerprint, return or resume the existing replay run.

This is a recoverable state machine or outbox, not reserve-then-best-effort. A
crash after reservation but before dispatch must not dedupe future retries to a
dead run. A retry or sweeper resumes `pending` reservations and progresses exactly
one replay.

## Immutable Baseline Execution Descriptor

Replay must reconstruct each re-executed task from an immutable per-TaskRun
execution descriptor captured at baseline execution time. It must not read live
`Job`/`Task`/`Atom` rows for behavior, because applies overwrite them. It must not
use the redacted `HashInputBlob` as the runtime source, because env values are
redacted and the blob may be oversized/degraded.

The descriptor is the complete effective runtime envelope. B2 owns persistence,
but the required field set is:

- Descriptor schema version and capture timestamp.
- Baseline identifiers: job id, job alias, task id, task name, atom id, baseline
  run id, trigger id/type/alias, and baseline `replay_safe` value.
- DAG context: explicit predecessor and successor task ids/names, trigger rule,
  branch-selection behavior, edge mode (implicit sequential vs explicit), task
  position, and outstanding predecessor count at materialization.
- Run params visible to the task, including trigger default params and runtime
  params captured on the baseline run.
- Predecessor outputs and predecessor effective hashes visible to the task.
- Engine and runtime command: engine (`docker`, `podman`, `kubernetes`), image,
  `ResolvedImageDigest`, command/argv, workdir, task type, node selector, retry
  count, retry delay, and retry backoff.
- Time controls: task timeout, run timeout, and any task/job-level cancellation
  semantics that affect execution.
- Cache controls: effective cache enabled flag, TTL, cache version, pin-digests
  flag, digest TTL, baseline computed hash, effective hash, and hash-input blob
  pointer/status for explanation.
- Schema controls: input schema, output schema, schema validation mode, and
  baseline schema-violation behavior.
- Full container spec: env map as refs/literals, mounts, resolved volume mounts,
  mount type/source/target/read-only/subPath, tmpfs options, PVC refs, claim
  templates, volume sources, bind/volume refs, and any other container spec field
  passed to the runtime. Store the RESOLVED `container.Spec` as produced by
  `Definition.RuntimeSpecForStep` at baseline-exec time (the already-merged
  `ResolvedVolumeMounts` and the job+step-merged `KubernetesSpec`), NOT the raw
  `step.VolumeMounts` + job-level `Volumes`/`Metadata` — per-engine volume source
  selection (`Volume.sourceForEngine`, definition.go ~716) and the job/step
  serviceAccountName/podAnnotations/automount merge are themselves apply-mutable
  inputs that must be pinned at baseline, not re-resolved at replay.
- Full Kubernetes spec and workload identity: the complete
  `container.KubernetesSpec` (`pkg/container/spec.go`) — `ServiceAccountName`,
  `PodAnnotations`, `AutomountServiceAccountToken`, and Kueue `QueueName` — plus
  `NodeSelector` (which lives on `Step`/`TaskRun`, e.g. `run.Store` TaskRun
  ~111/635/2590, not on `KubernetesSpec`). Capture ALL of these even though
  `QueueName` is cache-hash-excluded (`hashableKubernetes` in hash.go strips it):
  a re-executed task must run with the baseline workload identity and queue, not
  the live job's. Any new `KubernetesSpec`/workload-identity field must be added
  to this capture, enforced by a struct-completeness test (a test that fails when
  a new `KubernetesSpec` field is added without a matching descriptor field).
- Job-level behavior that changes execution or scheduling:
  `maxParallelTasks`, job labels/annotations only where the runtime consumes them,
  job cache defaults, task cache overrides, SLA metadata for reporting, and trigger
  configuration/default params that produced the baseline params.
- Secret references and identities for every `secret://` ref RE-RESOLVED on the
  task-execution path at replay time — step-env secrets via
  `ResolveContainerSpecSecrets` (`internal/jobdef/runtime/spec.go`) and any future
  runtime field resolved at container-create. Trigger-config and callback secrets
  are NOT re-resolved during replay (replay does not re-fire triggers and
  suppresses callbacks for quarantined runs), so they are captured for the
  descriptor RECORD but are NOT inputs to the pre-dispatch secret-identity abort
  gate. Listing them as hard-abort inputs would over-scope the gate: a rotated
  webhook signing secret irrelevant to execution would wrongly abort a valid
  replay.
- Large-object/reference inputs produced by the data-plane-memory substrate:
  reference URI/path, digest, size/metadata needed by the task, and whether the
  reference was available at baseline replay validation.

Secret values are never stored. Store refs plus provider-aware identity metadata.

This requires extending the `secret.Resolver` interface
(`internal/jobdef/secret/resolver.go`, today a single method
`Resolve(ctx, ref) (string, error)`) with
`ResolveWithIdentity(ctx, ref) (value, identity, error)`. The fan-out is:

- all three impls — `vault.go` (`VaultResolver`), `kubernetes.go`
  (`KubernetesResolver`), `env.go` (`EnvResolver`);
- the `MultiResolver` wrapper (`multi.go`) that dispatches by provider host; and
- `NewConfiguredResolver` / the factory (`factory.go`).

Critically, the identity capture and the dispatch resolve MUST be the SAME read
(single round-trip) to avoid a TOCTOU window — two separate reads let a secret
rotate between the identity check and the value used.

| Provider | Baseline identity | Replay rule |
|---|---|---|
| Vault | KV-v2: `metadata.version` (from `secret.Data["metadata"]`) PLUS a server-keyed HMAC of the resolved bytes. KV-v1 / any mount where `metadata.version` is absent: a hard no-identity sentinel (fail-closed). Never a plain value hash. | KV-v2: re-read the BASELINE version explicitly with `?version=<N>` (NOT latest — the current `VaultResolver.Resolve` calls `ReadWithContext` with no version pin and always returns latest, vault.go ~98), then HMAC the resolved bytes with the **versioned** server key and require an EXACT match. The stored identity records the HMAC **key id**; verification selects that key from a server keyring (current + retired keys), so a routine server HMAC-key rotation does NOT invalidate historical baselines (rotation-safe, the same way KV-v2 versioning is for the secret value). If the recorded key id is absent from the keyring (fully retired), that ref fails closed — operators retain retired keys for the replay-eligibility window. Version number alone is insufficient — rollback/destroy yield a matching number with different/absent value. KV-v1 and absent-version mounts FAIL CLOSED for that ref (same posture as env). |
| Kubernetes | Secret `resourceVersion` plus namespace/name/key (`kubernetes.go`). | Re-resolve and require matching `resourceVersion` for that namespace/name/key. Mismatch or missing secret aborts. |
| Env | No durable version exists (`env.go`). | v1 fails closed for env-sourced `secret://env/...` refs in replay because rotation cannot be verified. `ResolveWithIdentity` returns the un-verifiable sentinel that forces fail-closed. A future un-verifiable/degraded mode needs its own explicit contract. |

Identity metadata must be non-secret provider version data or a keyed HMAC using a
server-held key. Do not store a plain reusable hash of a secret value; it is
offline-guessable for low-entropy secrets and becomes sensitive itself. Any
server-held HMAC key is itself **versioned**: the identity records the key id and
verification resolves it against a keyring of current + retired keys, so rotating
the server key — routine security hygiene — does not silently abort every
historical replay. A key-id-less HMAC scheme is rejected for exactly this reason.

Hard aborts before TaskRun creation:

- Missing descriptor.
- Descriptor schema version unsupported.
- Missing or unresolvable secret ref.
- Secret identity mismatch.
- Env-provider secret in v1.
- Missing baseline result/cache for a task that replay classifies as unchanged.

## Distributed Parity

Replay quarantine must hold in local and distributed execution.

The local path in `internal/job/job.go` builds `cache.HashInput`, performs
`cacheStore.Get`, records cache hits/misses, retries tasks, performs
value-verified short-circuits, and calls `cacheStore.Put` after success. It also
dispatches callbacks after run completion today.

The distributed worker path in `internal/worker/runtime_executor.go` independently
loads the task spec and run params, rebuilds `cache.HashInput`, calls
`SetTaskHashWithBlob`, reads cache with `cacheStore.Get`, records cache-hit and
retry metrics, and calls `storeCacheEntry` -> `cacheStore.Put` after success. That
means fixing only `internal/job/job.go` is insufficient; a worker could otherwise
write authoritative cache entries for a replay run.

The existing scheduler-to-worker channel is the persisted `TaskRun` row plus
store-derived predecessor context. `TaskRun` already snapshots cache config,
schema validation, output schema, image/digest/hash fields, retry attempts, and
claim/owner fields; the worker reconstructs predecessor hashes/outputs from the
run store rather than from local scheduler memory. `TaskRun.Quarantine` must ride
that same durable path. B6 must test both propagation (replay materializes every
`TaskRun` with `Quarantine=true`) and honor (the worker suppresses cache writes,
lineage, callbacks/notifications, and production metrics for an already-marked
task).

## Non-Goals & Deferred

- Alternate environment namespaces are not in v1. They need a separate design
  proving secrets, volumes, workload identity, network destinations, and external
  refs are redirected before dispatch.
- Break-glass replay for unmarked jobs is not in v1. If added, it is admin-only,
  audited, and separate from the replay API/CLI.
- Full dataset resurrection is not a goal. Replay cannot recover overwritten
  source data outside Caesium's recorded contracts.
- Full row/column value diffing is not a goal. `--diff` reports causal hash/input
  differences and delegates data-value comparisons to external tools.
- Full-descriptor blame over every behavior field is deferred. The descriptor is
  required for replay correctness now; a future blame surface may use it, but v1
  does not promise historical authorship or field-level blame beyond the active
  plan's Stream C scope.
