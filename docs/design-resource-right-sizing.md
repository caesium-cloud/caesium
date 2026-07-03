# Design: Learned Resource Right-Sizing & OOM Retry Escalation

> Status: Brainstorm/Design — proposal for learning per-step resource
> requirements from run history, proposing/applying right-sized requests, and
> escalating memory on OOM retries instead of failing identically. No
> implementation yet. Depends on and delivers the stats-collection substrate
> planned in roadmap §2.5 (Cost Tracking & Resource Awareness). Composes with
> [`design-agent-in-the-loop.md`](design-agent-in-the-loop.md) (the `oom`
> failure class becomes a deterministic rule) and shares the provenance-routed
> GitOps-patch machinery designed there.

## Problem

Two failure modes bracket every containerized pipeline:

- **OOMKilled at 3 a.m.** A step that fit in 512Mi for six months hits a
  quarter-end batch and dies at its limit. Declared `retries` re-run the
  *identical* container with the *identical* limit — it dies identically,
  and a human gets paged to do the one mechanical fix an orchestrator could
  have done: retry with more memory.
- **10× overprovisioning.** Nobody knows what a step needs, so everyone
  requests 4Gi "to be safe." On Kubernetes those requests are reserved
  capacity — quota consumed, nodes underpacked, Kueue admission delayed —
  and nobody ever walks the YAML back down, because nobody has the data.

Both are the same missing feature: **Caesium runs every task and observes
nothing about its resource consumption.** The gap is even more basic than
tuning — a Caesium step cannot declare resource limits at all (see "Grounded
reality"), and an OOM kill is not even distinguishable from a manual
`SIGKILL` in the recorded result.

This design is the tractable *vertical* slice of "Dataflow-style compute
sized to the ETL": per-container sizing, learned from history, applied through
the engines Caesium already drives. It never reshapes a running computation —
no resharding, no live resize, no autoscaling (see Non-Goals).

## Fit with Design Principles

Quoting the six principles from [`roadmap.md`](roadmap.md):

1. **"Container-native execution."** Limits apply through each engine's
   native knobs (`HostConfig.Resources`, pod `resources`, podman
   `ResourceLimits`); stats come from what each runtime already exposes. No
   agent inside the user's container, no SDK.
2. **"Declarative and GitOps-first."** `resources:`/`rightSizing:` live in
   the YAML, linted and PR-reviewed; recommendations for git-synced jobs are
   *proposed as a Git PR* — Caesium never silently diverges the DB from Git.
3. **"Zero-dependency simplicity."** Stats are columns on `task_runs`;
   recommendations are computed from history in dqlite. Prometheus metrics
   are *exported*, never *required*. One disclosed exception: k8s
   peak-memory capture needs metrics-server, degrading gracefully without.
4. **"Smart by default."** Stats on ⇒ every step gets observed-vs-declared
   visibility free; escalation/auto-apply are opt-in with declared bounds.
5. **"Data engineering first."** Recurring batch runs give a stable
   distribution to learn from; "retry the 3 a.m. OOM bigger" is a data-eng
   pager item.
6. **"Open source, community-driven."** Self-hosted, no managed telemetry;
   right-sizing is a headline feature FinOps teams otherwise buy from SaaS
   orchestrators.

## Grounded Reality (what exists today)

Read before designing; every later claim builds on these:

- **`container.Spec` carries no resource fields** — its complete field set
  is `Env`, `WorkDir`, `Mounts`, `ResolvedVolumeMounts`, `Kubernetes`
  (`pkg/container/spec.go:99-105`) — and **no engine applies limits**:
  Docker `Create` builds a `HostConfig` only to carry mounts
  (`internal/atom/docker/engine.go:112-115`), the Kubernetes pod spec sets
  no `Resources` (`internal/atom/kubernetes/engine.go:132-150`), podman
  likewise. No step, in any engine, can request or limit CPU/memory today.
- **`atom.ResourceFailure` is dead code.** Defined at
  `internal/atom/atom.go:134`, with a human message already waiting in the
  run store ("atom exhausted resources (e.g. OOM)",
  `internal/run/store.go:2991`) — but no engine ever returns it. All three
  map exit codes through a `resultMap` where `137 → atom.Killed`
  (`internal/atom/docker/docker.go:25-33`,
  `internal/atom/kubernetes/kubernetes.go:21-29`,
  `internal/atom/podman/podman.go:27-35`); none consults Docker's
  `State.OOMKilled`, Kubernetes' `ContainerStateTerminated.Reason ==
  "OOMKilled"` (the terminated state is fetched at
  `internal/atom/kubernetes/atom.go:93-103` but only its `ExitCode` read),
  or podman's `InspectContainerState.OOMKilled`. **An OOM today is recorded
  as `killed`, indistinguishable from an operator SIGKILL.**
- **No stats collection.** The `Engine` interface is
  Get/List/Create/Wait/Stop/Logs (`internal/atom/atom.go:27-34`) — §2.5's
  `Stats()` does not exist — and `TaskRun` (`internal/models/run.go:45-145`)
  has no memory/CPU columns and no exit code. **This design DEPENDS on
  §2.5's stats capture and scopes Phase 0 to the minimal slice it needs.**
- **The retry loops are the escalation hook, and they are asymmetric.**
  Local: `internal/job/job.go:1055-1198` loops attempts
  (`maxAttempts = Retries+1`; `computeRetryDelay`,
  `internal/job/failure_policy.go:31-39`; persisted via `store.RetryTask`,
  `job.go:1182`) — but it only retries *execution errors*; a container that
  runs and exits unsuccessfully is completed with that result and returned
  without retry (`job.go:1161-1163`). The distributed worker loop
  (`internal/worker/runtime_executor.go:307-376`) *does* retry unsuccessful
  results — `executeTask` converts them to an error
  (`runtime_executor.go:584-585`) — persisting via `RetryTaskClaimed`
  (`:356`). Escalation must make the OOM class retryable in the local loop
  too, or the feature silently works only in distributed mode.
- **Resource limits do not feed cache identity — because they can't exist
  yet.** `cache.HashInput` (`internal/cache/hash.go:266-287`) hashes
  image/command/env/mounts/K8s-identity-fields/predecessors/params. The
  needed precedent exists: `KubernetesSpec.QueueName` is deliberately
  excluded as "scheduling metadata, not an execution input"
  (`internal/cache/hash.go:329-336`, `pkg/container/spec.go:76-81`).
- **In distributed mode the worker applies the spec** from
  `descriptor.ContainerSpec` (`internal/worker/runtime_executor.go:153`) on
  its own node — resources in `container.Spec` reach workers with zero new
  plumbing, but *escalation state must persist on the row*: a lease-expired
  task is re-claimed by a worker that sees only `TaskRun.Attempt`.
- **Git provenance exists for PR routing.** `Job` carries
  `ProvenanceSourceID/Repo/Ref/Commit/Path` (`internal/models/job.go:19-23`),
  maintained by the git sync in `internal/jobdef/git/`.

## Overview

```
                     run N completes
                          │
        ┌─────────────────▼──────────────────┐
        │ Stats capture (per attempt)        │   Phase 0
        │ peak mem / cpu-secs / oom flag /   │   = roadmap §2.5 items 1–2
        │ exit code → task_runs columns      │
        └───────┬─────────────────┬──────────┘
                │                 │
    OOM on attempt k         history (last N runs)
                │                 │
   ┌────────────▼───────┐   ┌─────▼──────────────────────┐
   │ Retry escalation    │   │ Recommendation engine      │
   │ next attempt at     │   │ p99(peak) × headroom,      │
   │ mem × factor,       │   │ quantized, clamped to      │
   │ clamped to bounds   │   │ declared bounds            │
   └────────────┬───────┘   └─────┬──────────────────────┘
                │                 │
        run turns green     mode: suggest ──▶ CLI/UI/REST recommendation
        (or bounds exhaust  mode: auto ─────▶ provenance-routed apply:
         → classified fail,          git-synced job → Git PR
         agent/incident              non-git job    → jobdefs apply
         hand-off)                   no write creds → degrade to suggest
```

## YAML

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: nightly-aggregate
  # optional job-level defaults; steps override field-wise
  rightSizing:
    mode: suggest                # suggest | auto | off   (default: off)
steps:
  - name: transform
    image: etl/transform:1.9
    engine: kubernetes
    retries: 2
    resources:                   # NEW — flows into container.Spec
      memory: 1Gi                # limit; also the k8s request (see Backend)
      cpu: 500m                  # k8s request / docker+podman NanoCPUs
    rightSizing:
      mode: auto                 # may rewrite resources.* within bounds
      memory: {min: 512Mi, max: 6Gi}
      cpu:    {min: 250m,  max: "2"}
      onOOM:                     # retry-with-escalation policy
        factor: 1.5              # next attempt = applied × 1.5 (quantized)
        maxEscalations: 2        # extra OOM-only attempts beyond `retries`
```

Semantics, all lint-enforced (`caesium job lint`):

- `resources` without `rightSizing` is valid: static limits, no learning.
- `rightSizing` requires `resources` (a declared baseline) and
  `memory.max ≥ resources.memory ≥ memory.min`.
- `suggest` computes recommendations but never mutates; `auto` may apply
  within `[min, max]` — on git-synced jobs "apply" *means opening a PR*,
  never a direct write (see Backend).
- `onOOM` works in every mode, including alongside `mode: off`; escalation
  is memory-only (CPU exhaustion throttles rather than kills — CPU is
  right-sized only via suggestions).

## Backend

### Phase 0 substrate: stats capture + honest OOM detection

This is roadmap §2.5's implementation-plan items 1–2, built here because
everything else stands on it (and shared with
[`design-agent-in-the-loop.md`](design-agent-in-the-loop.md) Phase 0, which
independently needs `TaskRun.ExitCode`):

- **OOM detection per engine**, at the inspect each engine already does:
  Docker consults `InspectResponse.State.OOMKilled` in `Result()` before the
  exit-code map and returns `atom.ResourceFailure`; Kubernetes checks
  `terminatedState(pod).Reason == "OOMKilled"` (the terminated state is
  already in hand at `internal/atom/kubernetes/atom.go:39-53` — only its
  `ExitCode` is read today); podman checks
  `InspectContainerState.OOMKilled`. Compatibility, stated honestly: OOM
  kills change from `killed` to `resource_failure` in persisted results —
  release-noted and gated by the feature env below.
- **`Stats()` on the engine interface** plus a sampling loop in both
  executors while awaiting completion. Docker/Podman: `ContainerStats`
  samples — honest limit: cgroup v2 dropped `max_usage_in_bytes`, so peak is
  the max of samples and **under-reports spikes shorter than the sample
  interval**; the OOM flag is the corrective ground truth (an OOM-killed
  attempt proves peak ≥ limit). Kubernetes: point-in-time usage from
  `metrics.k8s.io`, which requires metrics-server; when absent, k8s capture
  degrades to OOM-signal-only and the recommendation engine reports
  "insufficient samples" rather than guessing.
- **New `TaskRun` columns** (`internal/models/run.go`):
  `PeakMemoryBytes *int64`, `CPUSeconds *float64`, `StatsSource string`
  (`sampled|oom_inferred|none`), `ExitCode *int`, `OOMKilled bool`,
  `AppliedResources datatypes.JSON` (limits the final attempt ran with),
  `EscalationLevel int`. Earlier attempts' trail rides the execution
  descriptor (see below).
- **Prometheus export**: `caesium_task_memory_peak_bytes`,
  `caesium_task_cpu_seconds_total`, `caesium_task_oom_kills_total` — the
  §2.5 names, joining the existing `caesium_task_*` family
  (`internal/metrics/metrics.go:138`).

### Applying `resources` through the engines

Add `Resources *container.Resources` (`memory`, `cpu` as k8s-style quantity
strings, parsed at lint/apply) to `container.Spec`. Because `Step` embeds
`container.Spec` inline (`pkg/jobdef/definition.go:214-247`) and
`RuntimeSpecForStep` (`definition.go:959-1016`) persists the resolved spec
onto the atom model, the field flows automatically: YAML → atom →
`runner.spec` (local, `internal/job/job.go:555-559`) and YAML → execution
descriptor → worker. Engine mapping:

- Docker/Podman: `HostConfig.Resources.Memory` / `NanoCPUs` (resp. specgen
  `ResourceLimits`). Honest limit: these are limits only — Docker has no
  admission, so oversubscription protection on plain hosts is the kernel's
  OOM killer, which is exactly the signal we now catch.
- Kubernetes: `requests = limits` for memory (Guaranteed semantics),
  `requests` only for CPU (burstable). Changing memory requests changes
  **Kueue admission** arithmetic for `kueue:`-queued steps — disclosed; the
  recommendation UI shows the request delta for queued steps.

### Cache identity: stated honestly

`resources` (and per-attempt escalated values) are **excluded from
`HashInput`**, following the QueueName precedent
(`internal/cache/hash.go:329-336`): if limits fed the hash, every
right-sizing change would bust the cache and recompute the downstream DAG —
the feature would punish its own adoption. Consequences, stated plainly:

- An escalated retry keeps the *same* cache identity as the failed attempt —
  correct, since the computation is unchanged; retries already share one
  hash (computed once before the attempt loop, `internal/job/job.go:998`).
- `caesium why` / `run diff` will never attribute a re-run to a limits
  change, and a cached success is reusable regardless of current limits.
- The honest counter-case: unlike QueueName, limits **are visible inside the
  container** (cgroup files; JVM `MaxRAMPercentage`-style self-sizing). A
  step whose *output* genuinely depends on its memory limit is
  non-deterministic under this rule. Escape hatches: `cache: false` on the
  step, or a cache `version` bump alongside a limits change. We accept and
  document this rather than pretending limits are invisible.
- Receipts stay truthful without identity impact: `AppliedResources` and the
  escalation trail land on the TaskRun/descriptor (a descriptor
  schema-version bump — v1 has no resources field), so `caesium receipt get`
  shows what actually ran even though the hash doesn't fold it in.

### OOM retry escalation

Hook: the existing per-attempt loops, both executors.

- **Attempt budget.** At registration (`internal/run/store.go:1178-1180`
  stamps `MaxAttempts = Retries+1` today), stamp
  `MaxAttempts = Retries + 1 + onOOM.maxEscalations`. Escalation attempts
  are *class-gated*: consumable only when the previous attempt classified as
  OOM (`ResourceFailure` + `OOMKilled`); a plain failure that exhausted
  `Retries` fails even if escalation attempts remain. One accounting spine
  (the existing `Attempt`/`MaxAttempts` columns), with `EscalationLevel`
  recording how many consumed attempts were escalations.
- **Escalation step.** Next attempt's memory =
  `min(applied × factor, memory.max)`, quantized up to 64Mi. Already at
  `memory.max` ⇒ no attempt consumed: fail now, classified, trail attached —
  never burn attempts on a doomed identical retry.
- **Local executor change.** Make OOM-class results retryable in the local
  loop (today an unsuccessful result returns without retry,
  `internal/job/job.go:1161-1163`, unlike the worker path); the attempt runs
  a per-attempt spec copy with escalated `Resources`, nothing else changed.
- **Distributed persistence.** `RetryTaskClaimed` additionally persists
  `EscalationLevel` and the next attempt's `AppliedResources`, so a
  re-claimed task (worker death, lease expiry) resumes at the escalated size
  rather than resetting to baseline.
- **`RetryFromFailure` interplay.** The run-level retry
  (`internal/run/store.go:4614+`) resets `attempt` to 1; it must also reset
  `EscalationLevel`/`AppliedResources` to baseline — a manual retry starts
  the ladder over. (Per the sibling doc's finding, that path bypasses
  concurrency admission and `Job.Paused`; this design adds no new caller.)
- **Feedback into learning.** OOM-killed attempts are censored observations
  (peak ≥ limit), so suggestions rise even when sampling missed the spike —
  a success-after-escalation is the strongest recommendation signal.

### Recommendation engine

Computed on read (no new store, no background fleet scans):

```
window   = last N successful runs of (job, task name)   [default N=20]
           discarding runs older than the step's current image tag change
           when digest history (descriptor) shows a change
suggest  = quantize_up( p99(peak_mem over window) × (1 + headroom) )
           clamped to [memory.min, memory.max]; never below max(window)
cpu      = same over CPU seconds / duration (suggestion only)
```

Guard rails: minimum sample count (default 5) before any suggestion;
downward suggestions suppressed while the §2.5 anomaly condition holds
(latest run > 2× rolling average); `oom_inferred` censored points force the
suggestion to at least `applied × onOOM.factor`. Deliberately
percentile-plus-headroom, not a model — boring, explainable, auditable.
Quarantined replays and runs from
[`design-backtesting.md`](design-backtesting.md) are excluded from the
window (`quarantine IS NOT TRUE`, the established filter), as are backfill
storms unless opted in — backfill inputs can differ systematically from
steady state.

### Applying: provenance-routed, reusing the agent-doc router

`mode: auto` (and explicit `--apply`) routes exactly like
`apply_jobdef_patch` in
[`design-agent-in-the-loop.md`](design-agent-in-the-loop.md):

- **Git-synced job** (`Provenance*` fields set): direct DB apply is
  *rejected* — the next sync would revert it. The recommendation renders as
  a minimal YAML patch to `ProvenancePath`, opened as a Git PR (requires
  write credentials, config below; absent them, degrade to `suggest` with
  the rendered diff attached). PRs batch per job per window,
  cooldown-limited — never one PR per run.
- **Non-git job**: staged through the normal `jobdefs/diff` + `apply` path,
  recorded in the audit log.
- The applier never exceeds declared bounds, and the direct-apply endpoint
  is refused when auth mode is `none` (same reasoning as the agent doc's
  master gate — an unauthenticated apply route must not exist); PR-routed
  proposals are safe regardless, since a human merges.

### Data model & REST

- `TaskRun` columns as in Phase 0 (stats, OOM, applied resources, escalation
  level) — no new tables in the core loop; `resource_recommendations` is an
  optional lazily-recomputed cache table in Phase 3.
- Endpoints (Echo controllers beside `api/rest/controller/stats/`):
  - `GET /v1/jobs/:id/resources` — per-step declared vs observed
    (p50/p99/max/OOM count over window) + suggestion + utilization.
  - `POST /v1/jobs/:id/resources/apply` — provenance-routed apply
    (operator-authenticated; body may narrow to steps).
  - `GET /v1/stats/resources` — fleet rollup: top overprovisioned steps,
    reclaimable bytes, OOM leaderboard. Complements §2.5's planned
    `/v1/jobs/:id/costs`, which multiplies these same columns by a cost
    model.

### Config (env, `pkg/env/env.go`, envconfig pattern per `env.go:143`)

- `CAESIUM_RESOURCE_STATS_ENABLED` (default `false`) — Phase 0 gate (stats
  sampling + OOM reclassification);
  `CAESIUM_RESOURCE_STATS_SAMPLE_INTERVAL` (default `10s`).
- `CAESIUM_RIGHT_SIZING_ENABLED` (default `false`) — recommendations,
  escalation, apply routes; off ⇒ no routes bound, `resources:` still
  applies statically. Tuning: `..._WINDOW_RUNS` (20), `..._PERCENTILE` (99),
  `..._HEADROOM` (0.2), `..._MIN_SAMPLES` (5).
- `CAESIUM_GIT_WRITE_CREDENTIALS` (or a write token on the git-sync source
  config) — enables the PR route; absent ⇒ degrade to suggest.

## CLI

```
caesium job resources <alias> [--json]        # observed vs declared + suggestions
caesium job resources <alias> --apply [--step transform]   # provenance-routed
caesium job resources --all --format markdown # fleet report / PR-body ready
```

`--json` goes to stdout, clean and parseable — asserted with the
stream-separating `runCLIStdout` helper (`test/data_plane_e2e_test.go:31`),
per the repo's hard-won rule that merged-stream captures hide leaks.

## Frontend (`ui/src/features/jobs/`)

1. **JobDetailPage: per-step Resources panel.** Declared limit vs observed
   peak sparkline, utilization %, suggestion badge ("declared 4Gi · p99
   412Mi · suggest 512Mi"), one-click Apply — rendered as "Open PR" with a
   diff preview on git-synced jobs.
2. **TaskDetailPanel / TaskMetadataPanel: attempt trail.** Per-attempt
   applied limits with OOM badges: "attempt 1 OOMKilled at 1Gi → attempt 2
   at 1.5Gi ✓" — the receipt-grade evidence view.
3. **RunDetailPage: anomaly ribbon** when a run's peak exceeded 2× the
   rolling average (the §2.5 anomaly rule), linking to the panel.
4. **Stats page: fleet reclaim view** — top overprovisioned steps,
   reclaimable memory, OOM leaderboard; pending-suggestion count joins
   `useNavCounts.ts`.

## Safety

- **Bounds are absolute.** Escalation and auto-apply clamp to declared
  `[min, max]`; no bound, no auto behavior. Caesium does not discover
  cluster/namespace quota — a pod rejected by `ResourceQuota`/`LimitRange`
  surfaces as `StartupFailure` with the K8s message; bounds are the
  operator's declared envelope, honestly not a cluster guarantee.
- **Escalated attempts are accounted, visible, and capped** — they extend
  `MaxAttempts` explicitly at registration, are class-gated to OOM, recorded
  per attempt, and counted in `caesium_task_retries_total` plus a new
  `caesium_task_oom_escalations_total`.
- **Cache identity disclosed** (section above): limits are not execution
  inputs; steps whose outputs depend on limits must opt out of caching.
- **Auto never touches Git-owned truth directly** — provenance routing is
  server-enforced, and direct apply requires an active auth mode
  (`CAESIUM_AUTH_MODE` defaults to `none`, so this is a real gate).
- **Downsizing is conservative:** suggest-only by default; `auto` downsizes
  only after a full OOM-free window, and an OOM after an auto downsize
  immediately reverts and freezes downsizing for the cooldown.
- **Agent composition:** with
  [`design-agent-in-the-loop.md`](design-agent-in-the-loop.md) enabled, the
  `oom` class becomes a deterministic rule deferring to in-run escalation;
  an incident opens only when bounds exhaust, arriving pre-diagnosed
  ("OOMKilled at 4Gi and 6Gi; bound max 6Gi; raise `memory.max`" — a tier-3
  jobdef-patch proposal with the trail attached).

## Testing

Integration-first, per the repo gate — every surface driven for real in
`test/` against the live server, with a small stress image in `build/` that
allocates a configurable number of MiB (real OOMs against real Docker in CI):

1. **Stats capture:** run a job, assert `peak_memory_bytes`/`cpu_seconds`
   populated via `GET /v1/jobs/:id/resources` and `caesium job resources
   --json` (stdout-clean via `runCLIStdout`).
2. **OOM classification:** 64Mi limit, allocate 128Mi → result
   `resource_failure`, `OOMKilled=true`, exit code recorded.
3. **Escalation green-run:** `onOOM: {factor: 2, maxEscalations: 2}`,
   workload needs ~100Mi at a 64Mi limit → attempt 2 at 128Mi succeeds;
   assert attempt trail, `AppliedResources`, `EscalationLevel`, and an
   identical cache hash across attempts.
4. **Bounds exhaustion:** max below need → classified failure after
   class-gated attempts; a plain non-OOM failure does NOT consume
   escalation attempts.
5. **Distributed lane:** escalation through the claimed worker path,
   including a forced re-claim mid-ladder (escalation level persists).
6. **Recommendation math:** seed N real runs, assert the suggested value
   (percentile + headroom + quantization) through CLI and REST.
7. **Provenance routing:** git-synced apply is rejected/PR-routed; non-git
   round-trips `diff`/`apply`; auth-off refuses direct apply.
8. **Gates off ⇒ inert:** no columns written, no routes bound, OOM results
   stay pre-change (`killed`).
9. Kubernetes result-mapping and metrics-API degradation covered at unit
   level with fake pod statuses (CI has no cluster; a kind lane is a
   follow-up). Feature envs enabled in `just integration-up` so the paths
   execute in CI; UI panels get Playwright e2e against the live backend,
   matching the data-plane-memory-ui precedent.

## Phasing

- **Phase 0 — See truthfully.** `Stats()` + sampling for Docker/Podman,
  terminated-state capture for K8s, OOM reclassification to
  `ResourceFailure`, `TaskRun` columns incl. `ExitCode`, Prometheus metrics.
  This *is* roadmap §2.5 items 1–2 and co-delivers agent-doc Phase 0's
  exit-code need — build once.
- **Phase 1 — Declare.** `resources:` through all three engines, lint,
  descriptor schema bump, cache-identity exclusion test, distributed flow.
  Independently valuable: today Caesium cannot set limits at all.
- **Phase 2 — Escalate.** `onOOM` ladder in both executors, local-loop
  retryability fix, persisted escalation state, attempt-trail UI.
- **Phase 3 — Suggest.** Recommendation engine, REST + `caesium job
  resources`, JobDetail panel, fleet view.
- **Phase 4 — Apply.** Provenance-routed auto/`--apply`, Git-PR proposals,
  downsizing cooldowns, agent-incident composition.

## Non-Goals (v1)

- **No cluster autoscaling or bin-packing** — placement stays delegated
  (Kueue on K8s; the kernel on plain hosts). Caesium sizes containers, it
  does not schedule nodes.
- **No mid-run resize** — no VPA-style in-place pod resize, even where K8s
  supports it; escalation happens *between attempts*, never inside one.
- **No Beam/Dataflow-style resharding** of a running computation — the
  horizontal analog is
  [`design-dynamic-fanout.md`](design-dynamic-fanout.md)'s territory (whose
  fan-out children inherit the template step's `resources` — a deliberate
  composition point).
- **No cost/dollar modeling** — §2.5's cost layer multiplies the columns
  this design persists; substrate shared, scope not.
- **No per-run manual resource overrides** — sizing is learned or declared,
  not a run param (params feed `HashInput`, which would reopen the identity
  question by the back door).

## Open Questions

1. **Peak fidelity on cgroup v2.** Is sample-max + OOM-censoring enough, or
   should Docker/Podman read `memory.peak` from the cgroup fs when Caesium
   shares the host? Leaning: sampling first; host-path reads are an
   optimization with real portability cost.
2. **K8s requests-vs-limits policy.** Ship `request = limit` for memory
   only, or expose `resources.requests` separately? Separate fields are more
   honest for CPU but double the recommendation surface.
3. **Window reset triggers.** Image-digest changes reset the learning
   window; should a params-distribution shift too, and how is that detected
   cheaply? [`design-window-scheduling.md`](design-window-scheduling.md) /
   [`design-freshness-scheduling.md`](design-freshness-scheduling.md)
   cohorts could partition the window per data-window size.
4. **Opt-in identity folding.** Offer `rightSizing.hashResources: true` for
   steps whose outputs genuinely depend on limits (self-sizing JVMs), at the
   cost of cache busts on every change? Leaning: no in v1 — document
   `cache: false`; revisit with evidence.
5. **PR ergonomics.** One rolling Renovate-style PR per repo vs. discrete
   PRs per job per window? Interacts with how
   [`design-contract-enforcement.md`](design-contract-enforcement.md) and
   the agent doc route their proposals — a shared "Caesium proposals" PR
   channel may deserve its own mini-design.
