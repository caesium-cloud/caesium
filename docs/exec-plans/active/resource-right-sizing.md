# Resource Right-Sizing — Learned Sizing + OOM Retry Escalation

Last updated: 2026-07-03

Caesium runs every containerized task and observes **nothing** about its
resource consumption: a step cannot declare CPU/memory limits at all (the
complete `container.Spec` field set is `Env`/`WorkDir`/`Mounts`/
`ResolvedVolumeMounts`/`Kubernetes`, `pkg/container/spec.go:99-105`), and an
OOM kill is recorded as `killed` — indistinguishable from an operator SIGKILL —
because all three engines map exit code `137 → atom.Killed` through a
`resultMap` and never consult the runtime's OOM flag. `atom.ResourceFailure`
(`internal/atom/atom.go:130-134`) is defined with a human-facing message
already waiting in the run store (`internal/run/store.go`) but is dead code no
engine returns. This plan ships the tractable *vertical* slice of
"Dataflow-style compute sized to the ETL": per-container sizing, learned from
run history, applied through the engines Caesium already drives.

The work lands in five phases mapped to six streams: **Phase 0 / Stream A** —
capture peak memory, CPU seconds, exit code and an honest OOM flag onto
`task_runs` and reclassify OOM kills to `ResourceFailure` (this *is* roadmap
§2.5 implementation items 1–2 and co-delivers the agent-in-the-loop doc's
exit-code need); **Phase 1 / Stream B** — a `resources:` block flowing through
Docker/Podman/Kubernetes, deliberately excluded from the cache hash per the
`QueueName` precedent; **Phase 2 / Stream C** — an `onOOM` escalation ladder in
both executors that retries an OOM at `memory × factor` clamped to bounds
instead of dying identically; **Phase 3 / Stream D** — a compute-on-read
recommendation engine (`p99(peak) × headroom`, clamped) surfaced via REST + CLI;
**Phase 4 / Stream E** — provenance-routed apply that opens a Git PR for
git-synced jobs and never silently diverges the DB from Git. **Stream F** carries
the `ui/src/features/jobs/` panels. Everything is env-gated
(`CAESIUM_RESOURCE_STATS_ENABLED`, `CAESIUM_RIGHT_SIZING_ENABLED`, both default
`false`) so `resources:` still applies statically with the learning machinery off.

Per the `CLAUDE.md` end-to-end gate, every new REST endpoint and CLI verb ships
with an integration test in `test/` that drives the real surface against the
live server using a small `build/` stress image that allocates N MiB and OOMs
against real Docker in CI — a unit test that hand-seeds a `TaskRun` with a peak
value proves the recommendation math, never the wiring.

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

This plan implements
[`docs/design-resource-right-sizing.md`](../../design-resource-right-sizing.md).
**The design doc is authoritative for INTENT and SCOPE** — the phasing, the
YAML contract (`resources:` / `rightSizing:` shapes), the cache-identity
exclusion decision, the recommendation formula, the provenance-routing rule,
and the Non-Goals (no mid-run resize, no autoscaling, no cost/dollar modeling,
no per-run manual overrides). **When this plan and the design doc disagree, the
design doc wins**, and this plan is corrected to match. No item may add a new
YAML knob, engine field, endpoint, or `CAESIUM_*` config beyond what the design
enumerates without first amending the design.

Two subordinate contracts: strategic priority/status is tracked in
[`docs/roadmap.md`](../../roadmap.md) — the Phase-4 design-wave table row and
§2.5 (Cost Tracking & Resource Awareness, whose stats substrate this plan's
Phase 0 delivers); the roadmap wins on priority/status disagreements. The
job-definition YAML contract lives in
[`pkg/jobdef/definition.go`](../../../pkg/jobdef/definition.go) and the cache
identity in [`internal/cache/hash.go`](../../../internal/cache/hash.go); because
`Step` embeds `container.Spec` inline (`definition.go:214-247`), `resources:`
flows to the atom model automatically, but the hash exclusion (Stream B) is
load-bearing and the schema wins on any field-name disagreement.

## Progress (as of 2026-07-03)

No implementation waves have shipped yet. The plan was published from
[`docs/design-resource-right-sizing.md`](../../design-resource-right-sizing.md)
(Status: Brainstorm/Design); the first wave is the next eligible run of the
`exec-plan-wave` skill against this doc. The design doc's `> Status:` banner
flips to "active — this plan" as part of N-1.

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Phase 0 substrate — `TaskRun` stats columns, `Stats()` on the engine interface + sampling, honest OOM reclassification to `ResourceFailure`, Prometheus families | **P0** | Not started |
| B | Phase 1 — `resources:` block through all three engines, lint validation, cache-identity exclusion + test, descriptor schema bump, distributed flow | **P0** | Not started |
| C | Phase 2 — `onOOM` escalation ladder in both executors, local-loop retryability fix, persisted escalation state | P1 | Not started |
| D | Phase 3 — compute-on-read recommendation engine + `GET /v1/jobs/:id/resources` + `GET /v1/stats/resources` + `caesium job resources` CLI | P2 | Not started |
| E | Phase 4 — provenance-routed `POST /v1/jobs/:id/resources/apply` (Git PR / `jobdefs` apply / degrade-to-suggest), `mode: auto`, downsizing cooldowns | P3 | Not started |
| F | UI — JobDetail Resources panel, attempt-trail badges, RunDetail anomaly ribbon, stats reclaim view | P2 | Not started |
| H-1 | Integration harness — `build/` stress image, feature envs on the live integration server | — | Not started |
| N-1 | Docs — design banner, roadmap, schema references, examples, README | — | Not started |

## Streams

### Stream A — Phase 0: stats substrate + honest OOM detection

The reactive substrate every other stream builds on. Today the `Engine`
interface is Get/List/Create/Wait/Stop/Logs (`internal/atom/atom.go:27-34`) with
no `Stats()`, `TaskRun` (`internal/models/run.go:45+`) has no memory/CPU/exit
columns, and OOM is invisible. This stream captures the truth and reclassifies
OOM kills — the largest blast radius (the engine interface + all three engine
adapters + `TaskRun` + the run store + metrics), so it merges first. Gate the
new behavior behind `CAESIUM_RESOURCE_STATS_ENABLED` (default `false`) so
enabling it is the only thing that changes OOM classification (release-noted).

- [ ] A1. Add the Phase 0 `TaskRun` columns, their run-store persistence, the
      Prometheus families, and the `CAESIUM_RESOURCE_STATS_ENABLED` /
      `..._SAMPLE_INTERVAL` (default 10s) env gate. Columns:
      `PeakMemoryBytes *int64`, `CPUSeconds *float64`,
      `StatsSource string` (`sampled|oom_inferred|none`), `ExitCode *int`,
      `OOMKilled bool`, `AppliedResources datatypes.JSON` (the limits the final
      attempt ran with), `EscalationLevel int` (written by Stream C, but the
      column is added here so Phase 0 owns the single migration). No new table,
      so **no `internal/models/models.go` change** — `TaskRun` is already in the
      `All` slice; note this explicitly to whoever picks up the item. Metrics:
      `caesium_task_oom_kills_total`, plus the §2.5-named
      `caesium_task_memory_peak_bytes` and `caesium_task_cpu_seconds_total`
      (declared in the `var (...)` block AND added to the `prometheus.MustRegister`
      list at `internal/metrics/metrics.go:498+` — two edit sites — with a
      `internal/metrics/testutil` assertion in a `*_test.go`).
      Files: `internal/models/run.go`, `internal/run/store.go`,
      `internal/metrics/metrics.go` (+ `metrics_test.go`), `pkg/env/env.go`.
- [ ] A2. Reclassify OOM kills to `atom.ResourceFailure` per engine, at the
      inspect each engine already performs, and capture the exit code onto the
      result. Docker: consult `InspectResponse.State.OOMKilled` in `Result()`
      (`internal/atom/docker/atom.go:35`) **before** the exit-code `resultMap`
      (`docker/docker.go:25-33`). Kubernetes: check
      `terminatedState(pod).Reason == "OOMKilled"` (the terminated state is
      already fetched at `internal/atom/kubernetes/atom.go:39-53,93-103`, but only
      its `ExitCode` is read today). Podman: check
      `InspectContainerState.OOMKilled` (`internal/atom/podman/atom.go:27`). Gate
      the reclassification on `CAESIUM_RESOURCE_STATS_ENABLED` so off ⇒ OOM stays
      `killed` (compatibility, honestly). Update the three `resultMap` round-trip
      tests (`*/atom_test.go`) to cover the OOM path.
      Files: `internal/atom/docker/atom.go`, `internal/atom/docker/docker.go`,
      `internal/atom/kubernetes/atom.go`, `internal/atom/podman/atom.go`
      (+ the three `atom_test.go`).
      Depends on: A1.
- [ ] A3. Add `Stats()` to the `atom.Engine` interface and a sampling loop in
      both executors, writing `PeakMemoryBytes`/`CPUSeconds`/`StatsSource`.
      Docker/Podman sample `ContainerStats` (cgroup v2 dropped
      `max_usage_in_bytes`, so peak = max of samples; under-reports sub-interval
      spikes — the OOM flag from A2 is the corrective ground truth). Kubernetes
      reads `metrics.k8s.io`; absent metrics-server, degrade to
      `StatsSource=oom_inferred` (or `none`), never guess. Wire the sampler into
      the local executor (`internal/job/job.go`) and the distributed worker
      (`internal/worker/runtime_executor.go`), gated by
      `CAESIUM_RESOURCE_STATS_ENABLED`. Unit-test the sample-max reducer and the
      k8s degradation path against fake stats.
      Files: `internal/atom/atom.go`, `internal/atom/docker/engine.go`,
      `internal/atom/kubernetes/engine.go`, `internal/atom/podman/engine.go`,
      `internal/job/job.go`, `internal/worker/runtime_executor.go`.
      Depends on: A2.

### Stream B — Phase 1: declare `resources:` through the engines

Independently valuable — today Caesium cannot set limits at all. Add a
`resources:` block that flows YAML → `container.Spec` → atom → runner/descriptor
→ every engine's native knob, and (load-bearing) **exclude it from the cache
hash** so a sizing change never busts the DAG.

- [ ] B1. Add `container.Resources` (`memory`, `cpu` as k8s-style quantity
      strings) as `Resources *container.Resources` on `container.Spec`
      (`pkg/container/spec.go:99-105`), plus the `resources:` and job/step-level
      `rightSizing:` YAML shapes on `Step` / `metadata` in
      `pkg/jobdef/definition.go` (the struct, the dual `Step`/`rawStep`
      declaration + `UnmarshalYAML` at `definition.go:249+`, and `Validate()`)
      and the JSON-schema surface (`pkg/jobdef/schema.go`). Lint-enforced
      semantics: `resources` without `rightSizing` is valid (static limits);
      `rightSizing` requires `resources`; `memory.max ≥ resources.memory ≥
      memory.min`; parse quantities at lint/apply. Because `Step` embeds
      `container.Spec` inline (`definition.go:214-247`) and `RuntimeSpecForStep`
      (`definition.go:959-1016`) persists the resolved spec, the field reaches
      the atom model with no extra plumbing — assert that in a unit test.
      Files: `pkg/container/spec.go`, `pkg/jobdef/definition.go`,
      `pkg/jobdef/schema.go`, `internal/jobdef/runtime/spec.go`.
- [ ] B2. Apply `resources` through the three engine adapters. Docker: map to
      `HostConfig.Resources.Memory` / `NanoCPUs`
      (`internal/atom/docker/engine.go:112-115` builds `HostConfig` for mounts
      only today). Podman: specgen `ResourceLimits`. Kubernetes: set
      `requests = limits` for memory and `requests` only for CPU on the pod spec
      (`internal/atom/kubernetes/engine.go:132-150`, which sets no `Resources`
      today) — note that changing a memory request changes **Kueue admission**
      arithmetic for `kueue:`-queued steps (disclosed later in the recommendation
      UI). Limits-only; no admission control on plain Docker hosts (the kernel's
      OOM killer is the signal A2 now catches).
      Files: `internal/atom/docker/engine.go`,
      `internal/atom/kubernetes/engine.go`, `internal/atom/podman/engine.go`.
      Depends on: B1, and Stream A (A2/A3 edit the same three engine packages —
      sequence B after A, see conflicts).
- [ ] B3. Exclude `resources` from cache identity, prove it, and carry the
      applied limits on the descriptor. Do **not** add `Resources` to
      `cache.HashInput` (`internal/cache/hash.go:266-287`) — follow the
      `QueueName` "scheduling metadata, not an execution input" precedent
      (`hash.go:329-336`); add a `hash_test.go` case asserting two specs
      differing only in `resources` hash byte-identically. Bump the execution
      descriptor schema (v1 carries no resources field) so `AppliedResources`
      and the escalation trail can ride the descriptor to a distributed worker
      (`internal/worker/runtime_executor.go:153` applies `descriptor.ContainerSpec`
      on its own node). Document the honest counter-case (a self-sizing JVM whose
      output depends on its limit is non-deterministic under this rule; escape
      hatch is `cache: false` or a `version` bump).
      Files: `internal/cache/hash.go` (+ `hash_test.go`), the execution
      descriptor (`internal/worker/` descriptor + `ContainerSpec`),
      `internal/worker/runtime_executor.go`.
      Depends on: B1.

### Stream C — Phase 2: OOM retry escalation

Turn an OOM into a green run instead of an identical death. Hook the existing
per-attempt loops in both executors; the local loop is asymmetric today
(`internal/job/job.go:1055-1198` retries only *execution errors* and returns an
unsuccessful container result without retry at `job.go:1161-1163`, whereas the
distributed worker `internal/worker/runtime_executor.go:307-376` already retries
unsuccessful results), so escalation needs a local-loop retryability fix or it
works only in distributed mode.

- [ ] C1. Make OOM (`resource_failure`) results retryable in the local loop and
      grant the escalation budget **dynamically**. Leave the registration stamp at
      today's `MaxAttempts = Retries+1` (`internal/run/store.go`); do **not**
      pre-stamp `Retries + 1 + onOOM.maxEscalations`. Pre-stamping breaks the
      terminal invariant for the common case — a task that fails *normally* stops at
      `Retries+1` attempts, leaving `Attempts < MaxAttempts` forever, so run views,
      monitoring, and "is this run done?" checks read it as still-active. Instead,
      **only when an OOM escalation actually fires**, atomically bump `MaxAttempts`
      by 1 (bounded so the total escalation grants never exceed
      `onOOM.maxEscalations`). Escalation grants are **class-gated**: a bump happens
      only when the previous attempt classified as OOM; a plain failure that
      exhausted `Retries` terminates at `Attempts == MaxAttempts` with no bump. Fix
      `internal/job/job.go` so an OOM-classified unsuccessful result re-enters the
      attempt loop locally. Gate the whole escalation behavior behind
      `CAESIUM_RIGHT_SIZING_ENABLED` (default `false`, added here — this is the
      earliest stream that needs it).
      Files: `internal/job/job.go`, `internal/run/store.go`, `pkg/env/env.go`.
      Depends on: A1 (the `EscalationLevel`/`AppliedResources` columns +
      `ExitCode`/`OOMKilled` classification), B1/B2 (the `resources` field to
      escalate).
- [ ] C2. Implement the escalation step + its persistence across both executors.
      Next attempt's memory = `min(applied × factor, memory.max)` quantized up to
      64Mi; already at `memory.max` ⇒ no attempt consumed (fail now, classified,
      trail attached — never burn an attempt on a doomed identical retry). Run a
      per-attempt spec copy with escalated `Resources`, nothing else changed
      (this is why the local-loop fix in C1 and the descriptor bump in B3 are
      prerequisites). `RetryTaskClaimed`
      (`internal/run/store.go:3220`) additionally persists `EscalationLevel` +
      the next attempt's `AppliedResources` so a re-claimed distributed task
      resumes at the escalated size; `RetryFromFailure`
      (`internal/run/store.go:4642`) resets escalation state to level 0 with the
      attempt reset. Metrics: `caesium_task_oom_escalations_total` (var block +
      `Register()`); escalated attempts also count in the existing
      `caesium_task_retries_total` (`metrics.go:138`).
      Files: `internal/job/job.go`, `internal/worker/runtime_executor.go`,
      `internal/run/store.go`, `internal/metrics/metrics.go`.
      Depends on: C1.

### Stream D — Phase 3: recommendation engine + REST + CLI

Compute-on-read — no new store, no background fleet scans. `window = last N
successful runs of (job, task name)` (default N=20, reset on image-digest
change); `suggest = quantize_up(p99(peak_mem) × (1 + headroom))` clamped to
`[min, max]`, never below `max(window)`; CPU the same as a suggestion only.

- [ ] D1. Build the recommendation engine and its guard rails as a pure package.
      Minimum sample count (default 5); downward suggestions suppressed while the
      §2.5 anomaly condition holds (latest run > 2× rolling average); OOM-killed
      attempts are censored observations (peak ≥ limit) forcing the suggestion to
      at least `applied × onOOM.factor`; exclude quarantined replays and
      backtesting runs (`quarantine IS NOT TRUE`) and backfill storms unless
      opted in. Percentile-plus-headroom, not a model — fully unit-tested (p99
      math, clamping, never-below-max, OOM-censoring, anomaly suppression,
      insufficient-samples). Env tuning knobs: `CAESIUM_RIGHT_SIZING_WINDOW_RUNS`
      (20), `..._PERCENTILE` (99), `..._HEADROOM` (0.2), `..._MIN_SAMPLES` (5).
      Files: new `internal/rightsizing/` (recommend.go + recommend_test.go),
      `pkg/env/env.go`.
      Depends on: A1 (the stats columns to read), B1 (declared bounds).
- [ ] D2. Add the observability reads as Echo controllers beside
      `api/rest/controller/stats/`: `GET /v1/jobs/:id/resources` (per-step
      declared vs observed — p50/p99/max/OOM over the window — plus the
      suggestion and utilization %) and `GET /v1/stats/resources` (fleet rollup,
      complementing §2.5's planned `/v1/jobs/:id/costs`). Bind both in
      `Protected()` of `api/rest/bind/bind.go`, gated by
      `CAESIUM_RIGHT_SIZING_ENABLED` (off ⇒ routes not bound). Add a
      `RightSizing` field to the `Features` struct
      (`api/rest/service/system/system.go:36`) so the UI gates on it. Metric for
      pending-suggestion count if surfaced.
      Files: new `api/rest/controller/resources/`, new
      `api/rest/service/resources/`, `api/rest/bind/bind.go`,
      `api/rest/service/system/system.go`.
      Depends on: D1.
- [ ] D3. Add the CLI: `caesium job resources <alias> [--json]` (observed vs
      declared + suggestions), `--apply [--step transform]` (wired in Stream E),
      and `caesium job resources --all --format markdown` (fleet report /
      PR-body-ready). `--json` and `--format markdown` go to **stdout, clean and
      parseable**, asserted with the stream-separating `runCLIStdout`
      (`test/data_plane_e2e_test.go:31`). New `cmd/job/resources.go` registered
      under the existing `job.Cmd` group (`cmd/job/job.go`) — no `cmd/execute.go`
      change (the `job` group is already in the `cmds` slice).
      Files: new `cmd/job/resources.go`, `cmd/job/job.go`.
      Depends on: D2.

#### Deferred — `resource_recommendations` cache table

The design lists an optional `resource_recommendations` **lazily-recomputed
cache** table for Phase 3. This plan computes on read (no new table, no
`internal/models/models.go` change), matching the design's "computed on read"
default. The cache table is **deferred** — draft it as a follow-on only if
read-time recomputation proves too slow on large fleets. Not a gate here.

### Stream E — Phase 4: provenance-routed apply

`mode: auto` (and explicit `--apply`) routes exactly like the agent-in-the-loop
doc's `apply_jobdef_patch`, reusing its provenance-routed GitOps-patch
machinery. **Git-synced job** (`Job.Provenance*` fields set,
`internal/models/job.go:19-23`): a direct DB apply is *rejected* (the next sync
reverts it) — the recommendation renders as a minimal YAML patch to
`ProvenancePath`, opened as a Git PR. **Non-git job**: staged through the normal
`jobdefs/diff` + `apply` path, audit-logged.

- [ ] E1. Add `POST /v1/jobs/:id/resources/apply` and its provenance router. On
      a git-synced job, render the recommendation as a minimal YAML patch and
      open a Git PR against `ProvenancePath` (requires `CAESIUM_GIT_WRITE_CREDENTIALS`;
      absent, degrade to `suggest` with the rendered diff attached), batched per
      job per window and cooldown-limited — never one PR per run. On a non-git
      job, round-trip the `jobdefs` diff/apply path, audit-logged. The applier
      never exceeds declared `[min, max]` bounds; the direct-apply route is
      **refused when `CAESIUM_AUTH_MODE` is `none`** (an unauthenticated apply
      route must not exist — the agent-doc master-gate reasoning; PR-routed
      proposals are safe regardless since a human merges). Wire the `--apply`
      flag into the D3 CLI. Operator-authenticated (`RoleRunner`/operator RBAC).
      Files: new `api/rest/controller/resources/apply.go`,
      `api/rest/service/resources/`, `api/rest/bind/bind.go`, `pkg/env/env.go`
      (`CAESIUM_GIT_WRITE_CREDENTIALS`), `cmd/job/resources.go` (the `--apply`
      flag).
      Depends on: D2, D3.
- [ ] E2. Add `mode: auto` end-to-end and the conservative downsizing policy.
      `auto` applies within bounds through the E1 router on its window boundary;
      **downsizing is conservative** — `auto` downsizes only after a full
      OOM-free window, an OOM after an auto downsize reverts immediately and
      freezes downsizing for the cooldown. Record the composition seam with the
      agent-in-the-loop doc inline (with that doc enabled, `oom` becomes a
      deterministic rule deferring to in-run escalation and an incident opens
      only when bounds exhaust, pre-diagnosed) — no new caller here, documented
      as a forward reference.
      Files: `api/rest/service/resources/`, `internal/rightsizing/`,
      `pkg/env/env.go`.
      Depends on: E1.

### Stream F — UI (`ui/src/features/jobs/`)

Surfaces the backend through the jobs feature, gated on the `RightSizing`
`Features` flag from D2.

- [ ] F1. Add the JobDetail Resources panel: per-step declared limit vs
      observed-peak sparkline, utilization %, a suggestion badge
      ("declared 4Gi · p99 412Mi · suggest 512Mi"), and a one-click Apply
      rendered as "Open PR" with a diff preview on git-synced jobs (calling the
      E1 apply endpoint). New method(s) in `ui/src/lib/api.ts`; Playwright e2e
      against the live backend.
      Files: `ui/src/features/jobs/JobDetailPage.tsx` (+ a new Resources panel
      component under `ui/src/features/jobs/`), `ui/src/lib/api.ts`.
      Depends on: D2, E1.
- [ ] F2. Add the attempt-trail, anomaly ribbon, and fleet reclaim views.
      TaskDetail/TaskMetadata panels show the per-attempt applied limits with OOM
      badges ("attempt 1 OOMKilled at 1Gi → attempt 2 at 1.5Gi ✓"); RunDetail
      gets an anomaly ribbon when a run's peak exceeded 2× the rolling average
      (the §2.5 rule); the stats page gets a fleet reclaim view (top
      overprovisioned steps, reclaimable memory, OOM leaderboard) with a
      pending-suggestion count joining `useNavCounts.ts`.
      Files: the TaskDetail/TaskMetadata panels + `RunDetailPage` + the stats
      page under `ui/src/features/`, `ui/src/lib/hooks/useNavCounts.ts`,
      `ui/src/lib/api.ts`.
      Depends on: C2 (the escalation trail data), D2 (observed/suggestion reads).

## Harness Strengthening

- [ ] H-1. Make the integration server exercise the real resource path. Add a
      small `build/` stress image (e.g. `build/Dockerfile.stress`) that allocates
      N MiB on demand so scenarios can force a real OOM against real Docker in CI.
      Set `CAESIUM_RESOURCE_STATS_ENABLED=true` and `CAESIUM_RIGHT_SIZING_ENABLED=true`
      (plus a tight `..._MIN_SAMPLES` / `onOOM` bound if the escalation tests need
      it) on the `just integration-up` / `just integration-test` server and in
      `.github/workflows/ci.yml`, mirroring the lineage `CAESIUM_OPEN_LINEAGE_ENABLED`
      precedent the `CLAUDE.md` gate calls out, so the A/B/C/D/E scenarios drive
      the live surface rather than an internal call. Land in the first wave so
      Stream A's end-to-end gate has a stress image + enabled stats to drive.
      Files: new `build/Dockerfile.stress`, `justfile`,
      `.github/workflows/ci.yml`, `test/` harness helpers.

#### Deferred — Kubernetes kind lane

K8s result-mapping and metrics-API degradation are **unit-tested with fake pod
statuses** (CI has no cluster). A kind-based integration lane that runs the real
K8s engine path (limits applied, OOM `Terminated.Reason` observed,
metrics-server degradation) is **deferred to a follow-up**, approximated locally
by `just k8s-distributed` + `just helm-test`. Not a gate here.

## Navigational / Organizational Improvements

- [ ] N-1. Reconcile the docs after A–F ship. Flip the
      [`docs/design-resource-right-sizing.md`](../../design-resource-right-sizing.md)
      `> Status:` banner from "Brainstorm/Design" to "active — implemented by this
      plan" (and mark shipped phases). Update `docs/roadmap.md`: the Phase-4
      design-wave table row (line ~221) to link this plan and mark it in
      progress/shipped, and the §2.5 (Cost Tracking & Resource Awareness)
      implementation-plan note that its items 1–2 (`Stats()` + per-task snapshots)
      landed via this plan's Phase 0. Document the `resources:` / `rightSizing:`
      / `onOOM` fields in `docs/job-schema-reference.md`, `docs/job-definitions.md`,
      and `docs/caesium-job-llm-reference.md`; add a static-limits example and a
      right-sizing example (pinned images) under `docs/examples/`; index this plan
      in `docs/README.md` in **backtick/inline-code** form (the
      `TestDocsREADMEIndexesEveryTopLevelDoc` guardrail rejects clickable
      subdirectory links). Runs last.
      Files: `docs/design-resource-right-sizing.md`, `docs/roadmap.md`,
      `docs/job-schema-reference.md`, `docs/job-definitions.md`,
      `docs/caesium-job-llm-reference.md`, `docs/examples/`, `docs/README.md`.
      Depends on: A–F (runs last, after the runtime ships).

## Sequencing & Dependencies

**Cross-stream order:**

- **Stream A is the foundation** — B, C, and D all consume the stats columns,
  the OOM classification, or the `Stats()` sampling A adds. A merges first
  (largest blast radius: engine interface + three adapters + `TaskRun` + run
  store + metrics).
- **Stream B** depends on A: B2 edits the same three engine packages A2/A3 edit
  (different methods — A touches `Result()`/inspect + `Stats()`; B touches
  `Create()`/pod spec), so B sequences **after** A rather than parallel.
- **Stream C** depends on A1 (the `EscalationLevel`/`AppliedResources`/`ExitCode`
  columns + OOM classification) and B1/B2/B3 (the `resources` field to escalate
  and the descriptor bump that carries escalated limits to workers).
- **Stream D** depends on A1 (stats columns) + B1 (declared bounds); it does not
  need C or E.
- **Stream E** depends on D2 + D3 (the recommendation + CLI it applies).
- **Stream F** depends on D2 (reads) + E1 (the Apply/Open-PR action) + C2 (the
  escalation-trail data for the attempt badges).
- **H-1** is independent (build image / justfile / CI / test harness); land it
  in the first wave so the A end-to-end gate has a stress image + enabled stats.
- **N-1** runs last, after A–F ship.

**Suggested waves:**
- **W1 = A (A1 → A2 → A3) + H-1.** A is one strict chain; H-1 provides the
  stress image and enabled-stats server the A scenarios drive.
- **W2 = B (B1 → (B2, B3)).** Unblocked once A's engine edits are in; B2 and B3
  are parallel after B1 (different files).
- **W3 = C (C1 → C2) + D (D1 → D2 → D3).** Both unblocked once B is in. C touches
  the executors + run store; D touches a new package + controllers — different
  files, sharing only additive `pkg/env/env.go` and `internal/metrics/metrics.go`
  append sites.
- **W4 = E (E1 → E2) + F (F1, F2) + N-1.** E depends on D; F depends on E1 + D2 +
  C2; N-1 last.

**Within-stream order:** A1 → A2 → A3 (strict — columns/env, then per-engine OOM
classification, then `Stats()` sampling). B1 → (B2, B3) parallel. C1 → C2.
D1 → D2 → D3. E1 → E2. F1, F2 parallel (F2 also needs C2).

**Cross-stream file conflicts:**

- `internal/atom/{docker,kubernetes,podman}/` — A2/A3 (OOM classification +
  `Stats()`) and B2 (apply limits) both edit the three engine packages, in
  different methods. **Sequence A → B** (already a dependency); never the same
  wave.
- `internal/job/job.go` + `internal/worker/runtime_executor.go` — A3 (sampling
  loop) and C1/C2 (escalation loop) both edit both executors. **Sequence A → C**
  (A in W1, C in W3). B3 also touches `runtime_executor.go:153` (descriptor
  apply) in W2 — different region, but flag the A→B→C ordering on this file.
- `internal/run/store.go` — A1 (column persistence + registration `MaxAttempts`
  stamp) and C1/C2 (dynamic `MaxAttempts` bump on actual OOM escalation — never a
  pre-stamp — `RetryTaskClaimed` escalation persist, `RetryFromFailure` reset).
  **Sequence A → C**.
- `internal/models/run.go` (`TaskRun`) — **only A1** adds columns (including
  `EscalationLevel`/`AppliedResources` that C writes). No same-file collision; C
  writes existing columns.
- `pkg/jobdef/definition.go` — **only B1** (the `resources`/`rightSizing` schema +
  `Validate` + dual `Step`/`rawStep` + `UnmarshalYAML`). The true-conflict file
  is owned by one stream.
- `internal/cache/hash.go` — **only B3** (the exclusion + test). No other stream
  touches the hash.
- `pkg/env/env.go` — A1 (`CAESIUM_RESOURCE_STATS_ENABLED`, `..._SAMPLE_INTERVAL`),
  C1 (`CAESIUM_RIGHT_SIZING_ENABLED`), D1 (`..._WINDOW_RUNS/_PERCENTILE/_HEADROOM/
  _MIN_SAMPLES`), E1 (`CAESIUM_GIT_WRITE_CREDENTIALS`) all append fields.
  Additive across waves; within **W3, C1 + D1 both append** — flag for a clean
  rebase (different lines).
- `internal/metrics/metrics.go` — A1 (oom_kills / memory_peak / cpu_seconds) in
  W1, then C2 (oom_escalations) in W3, each a two-site edit (`var (...)` +
  `Register()`). C2 is the only W3 metrics append (D reads, adds none) — no
  same-wave metrics overlap.
- `api/rest/bind/bind.go` — D2 (two GET routes) and E1 (one POST route) both add
  route lines in `Protected()`. D in W3, E in W4, so no same-wave collision;
  additive regardless.
- `api/rest/service/system/system.go` — **only D2** adds the `RightSizing`
  `Features` field.
- `cmd/job/job.go` — D3 registers `resources.go` under the `job` group; **no
  `cmd/execute.go` change** (the `job` group is already in the `cmds` slice).
- `ui/src/lib/api.ts` + `ui/src/features/jobs/` — F1 + F2 both append; same
  stream, sequences mechanically.
- `internal/models/models.go` — **no change** (no new table; `resource_recommendations`
  is deferred). No hot-table router entry either.

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Per-stream additions:

- **New REST endpoint / CLI verb (D, E):** an integration scenario in `test/`
  that drives the **real surface** — `GET /v1/jobs/:id/resources` against the
  live server, `POST /v1/jobs/:id/resources/apply`, or the CLI binary via
  `s.runCLI*` — and asserts observed output. For `--json` / `--format markdown`,
  capture **stdout separately from stderr** with `runCLIStdout` and assert clean,
  parseable output (the merged-stream capture hides log leaks).
- **OOM classification + escalation (A, B, C):** run the `build/` stress image at
  a low memory limit that forces a real OOM against real Docker in CI → assert
  `result == resource_failure`, `OOMKilled == true`, exit code + peak recorded
  (via `GET /v1/jobs/:id/resources` and `caesium job resources --json`); an
  escalation green-run (attempt 2 at the escalated size succeeds, identical cache
  hash across attempts); bounds-exhaustion → classified failure; a plain non-OOM
  failure does **not** consume escalation attempts; the distributed lane with a
  forced mid-ladder re-claim (escalation level persists).
- **Gates off ⇒ inert (A, C, D, E):** with the feature envs unset, assert no
  stats columns written, no right-sizing routes bound, and OOM results stay
  `killed`.
- **New metric (A1, C2):** assert via `internal/metrics/testutil` in a `*_test.go`;
  the collector must also appear in `Register()`.
- **Job-schema change (B1):** `caesium job lint --path docs/examples/` green on
  the new `resources:` / `rightSizing:` examples; an invalid bound
  (`memory.max < resources.memory`) rejected at lint.
- **Cache identity (B3):** a `hash_test.go` case proving two specs differing only
  in `resources` hash byte-identically.
- **UI changes (F):** `just ui-lint && just ui-test && just ui-e2e` (Playwright
  against the live backend).
- **This plan's checkbox ticked**, the active-wave `## Progress` bullet appended,
  and any cross-linked doc (design banner / roadmap / schema) refreshed in the
  same PR.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream A — the Phase 0 substrate** is live: with `CAESIUM_RESOURCE_STATS_ENABLED`
   on, an OOM-killed task is recorded as `resource_failure` with `OOMKilled=true`,
   an exit code, and a peak-memory value on `TaskRun`, and `caesium_task_oom_kills_total`
   / `caesium_task_memory_peak_bytes` / `caesium_task_cpu_seconds_total` are
   registered. Closed by a `test/` scenario that OOMs the `build/` stress image
   against real Docker and asserts the recorded stats + classification.
2. **Stream B — declared `resources:`** flows through all three engines: a step
   with `resources: {memory, cpu}` runs under those limits (Docker/Podman/K8s),
   the field is excluded from the cache hash (proven by `hash_test.go`), and it
   rides the bumped descriptor to a distributed worker. Closed by a lint scenario
   on the new examples + the cache-identity unit test + a distributed run.
3. **Stream C — OOM retry escalation** works in both executors: an OOM at the
   declared limit retries at `memory × factor` clamped to `memory.max`, a green
   run results, the attempt trail + `EscalationLevel` + `AppliedResources` persist
   (surviving a distributed re-claim), bounds-exhaustion yields a classified
   failure, and a plain failure does not consume escalation attempts. Closed by
   the escalation-green-run + bounds-exhaustion + distributed-reclaim scenarios,
   green in CI, with `caesium_task_oom_escalations_total` asserted.
4. **Stream D — the recommendation engine + reads + CLI** ship:
   `GET /v1/jobs/:id/resources` returns declared-vs-observed (p50/p99/max/OOM) +
   a clamped `p99 × headroom` suggestion, `GET /v1/stats/resources` returns the
   fleet rollup, and `caesium job resources --json` emits clean parseable stdout.
   Closed by integration scenarios that seed N real runs and assert the suggested
   value via both REST and the CLI binary (asserted with `runCLIStdout`).
5. **Stream E — provenance-routed apply** is live: `POST /v1/jobs/:id/resources/apply`
   (and `mode: auto` / `--apply`) opens a Git PR for a git-synced job (or degrades
   to suggest without `CAESIUM_GIT_WRITE_CREDENTIALS`), round-trips the `jobdefs`
   diff/apply path for a non-git job, refuses a direct apply when `CAESIUM_AUTH_MODE`
   is `none`, never exceeds declared bounds, and downsizes only after an OOM-free
   window (reverting + freezing on a subsequent OOM). Closed by provenance-routing
   integration scenarios (git-rejected/PR-routed, non-git round-trip, auth-off
   refusal).
6. **Stream F — the UI** surfaces it: the JobDetail Resources panel (declared vs
   observed + suggestion badge + Apply/Open-PR), the attempt-trail OOM badges, the
   RunDetail anomaly ribbon, and the stats fleet reclaim view render against the
   live backend, gated on the `RightSizing` `Features` flag. Closed by Playwright
   e2e.
7. **H-1 — the integration server** exercises the real resource path: the `build/`
   stress image is built and the feature envs are set on `just integration-up` /
   CI, so the A/B/C/D/E scenarios run against the live binary, not an internal
   call.
8. **N-1 — docs reflect reality:** the design-doc `> Status:` banner flipped to
   active, `docs/roadmap.md` (Phase-4 row + §2.5 note) updated, the `resources:`
   / `rightSizing:` / `onOOM` fields documented across the schema references with
   working `docs/examples/` manifests, and this plan indexed in `docs/README.md`
   (backtick form).
9. **Cross-cutting:** `docs/roadmap.md`, `docs/design-resource-right-sizing.md`,
   and this plan's per-stream `## Progress` entries reflect every shipped stream
   and match the merged PRs. (The `resource_recommendations` cache table and the
   K8s kind integration lane remain explicitly deferred — not gates here.)

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
   `<Imperative subject> (resource-right-sizing <wave>-<stream>)` — e.g.
   `Add TaskRun stats columns and honest OOM detection (resource-right-sizing W1-α)`.
   GitHub appends `(#NNN)` on squash-merge.

## Cross-References

- [`docs/design-resource-right-sizing.md`](../../design-resource-right-sizing.md)
  — the design of record. Source of truth for intent, scope, phasing, and the
  YAML/cache/recommendation/provenance contracts.
- [`docs/roadmap.md`](../../roadmap.md) — the Phase-4 design-wave table (this
  plan's row) and §2.5 Cost Tracking & Resource Awareness (whose stats substrate
  items 1–2 this plan's Phase 0 delivers). Roadmap wins on priority/status.
- [`pkg/jobdef/definition.go`](../../../pkg/jobdef/definition.go),
  [`pkg/container/spec.go`](../../../pkg/container/spec.go),
  [`internal/cache/hash.go`](../../../internal/cache/hash.go) — the schema, the
  container spec, and the cache identity this plan extends/excludes.
- [`docs/design-agent-in-the-loop.md`](../../design-agent-in-the-loop.md) — the
  provenance-routed GitOps-patch machinery Stream E reuses and the `oom`
  incident-composition seam.
- `docs/design-dynamic-fanout.md`, `docs/design-window-scheduling.md`,
  `docs/design-freshness-scheduling.md`, `docs/design-backtesting.md` — sibling
  Phase-4 designs referenced by the Non-Goals (fan-out children inherit
  `resources`; quarantine/backtesting runs are excluded from the window).
- `docs/job-schema-reference.md`, `docs/job-definitions.md`,
  `docs/caesium-job-llm-reference.md` — the schema docs N-1 extends with the
  `resources:` / `rightSizing:` fields.
