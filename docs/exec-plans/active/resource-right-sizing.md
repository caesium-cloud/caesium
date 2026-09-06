# Resource Right-Sizing — The Compute Loop

Last updated: 2026-09-05

> **Plan 2 of [`closed-loop-arc.md`](closed-loop-arc.md)** — *the compute loop*.
> Re-cut on 2026-09-05 against the arc: Stream E recast as an incident action,
> Kubernetes pulled into scope on the kind lane (new H-2), fan-out partition
> stats folded into A1/D1, a shared quantile-parameterised history reader
> defined in D1, and an explainability item added (C3).
> **Adversarial review 2026-09-05** corrected the re-cut in nine places: the
> deterministic-rule gate (`ExecutePolicy` bypasses the playbook, so E1/E2 are
> env- + wiring-gated, not "opt-in by construction"), the `DefaultRules()`
> symbol name, Plan 1's Git-PR item id (**F3**, drafted), the feature-gate scope
> (`resources:` validation is ungated; only `rightSizing:` is gated), E2's
> trigger and cooldown storage, four `Depends on:` lines, the suggested waves,
> Plan 4 B1's already-corrected ownership text, and roadmap §2.5's item list
> (1, 2 **and 4**). It also **restored** two acceptance-criteria clauses the
> re-cut dropped — Stream D's closing evidence, and Stream E's
> `AUTH_MODE=none` refusal / `[min, max]` clamp / conservative-downsizing gates.
> Every item's original text and rationale is preserved — superseded text lives
> in the *Design history* / *Superseded* subsections; no item was renumbered.

The loop this plan closes:

> **observe** peak memory / CPU seconds / OOM per attempt (fan-out partitions
> included) → **escalate now** (retry the attempt at a larger size, clamped and
> quantised) → **`oom` incident** → **`propose_resources`** carrying a
> `p99(peak) × headroom` recommendation → **approval** → **PR or apply**
> through the one `apply_jobdef_patch` router — and every step of it is
> answerable with `caesium why`.

Caesium runs every containerized task and observes **nothing** about its
resource consumption: a step cannot declare CPU/memory limits at all
(`container.Spec` in `pkg/container/spec.go` has exactly `Env`, `WorkDir`,
`Mounts`, `ResolvedVolumeMounts`, `Kubernetes` — no `Resources`), and an OOM
kill is recorded as `killed` — indistinguishable from an operator SIGKILL —
because all three engines map exit code `137 → atom.Killed` through a
`resultMap` (`internal/atom/docker/docker.go`, `internal/atom/podman/podman.go`,
`internal/atom/kubernetes/kubernetes.go`) and never consult the runtime's OOM
flag. `atom.ResourceFailure` (`internal/atom/atom.go`) is defined with a
human-facing message already waiting in the run store (`internal/run/store.go`)
but is dead code no engine returns. This plan ships the tractable *vertical*
slice of "Dataflow-style compute sized to the ETL": per-container sizing,
learned from run history, applied through the engines Caesium already drives.

The work lands in five phases mapped to six streams: **Phase 0 / Stream A** —
capture peak memory, CPU seconds, exit code and an honest OOM flag onto
`task_runs` and reclassify OOM kills to `ResourceFailure` **and to the `oom`
incident class** (this *is* roadmap §2.5 implementation items 1, 2 **and 4** —
its `Status:` line already says so — and
co-delivers the agent-in-the-loop doc's exit-code need); **Phase 1 / Stream B** —
a `resources:` block flowing through Docker/Podman/Kubernetes, deliberately
excluded from the cache hash per the `QueueName` precedent; **Phase 2 /
Stream C** — an `onOOM` escalation ladder in both executors that retries an OOM
at `memory × factor` clamped to bounds instead of dying identically, plus the
plan's provenance/explainability item; **Phase 3 / Stream D** — a
compute-on-read recommendation engine (`p99(peak) × headroom`, clamped) over a
shared quantile-parameterised history reader, surfaced via REST + CLI;
**Phase 4 / Stream E** — the durable half, recast as a `propose_resources`
incident action that materialises an `apply_jobdef_patch` proposal through the
one approval pipeline the arc builds. **Stream F** carries the
`ui/src/features/jobs/` panels. Everything is env-gated
(`CAESIUM_RESOURCE_STATS_ENABLED`, `CAESIUM_RIGHT_SIZING_ENABLED`, both default
`false`, neither of which exists today) so `resources:` still applies statically
with the learning machinery off.

Per the `CLAUDE.md` end-to-end gate, every new REST endpoint and CLI verb ships
with an integration test in `test/` that drives the real surface against the
live server using a small `build/` stress image that allocates N MiB and OOMs
against real Docker in CI — a unit test that hand-seeds a `TaskRun` with a peak
value proves the recommendation math, never the wiring. **Kubernetes is not
exempt** (arc convention 2): kind-in-CI is the verification bar, and H-2 makes
the `helm-integration-test` lane exercise the pod OOM reason, requests/limits
application, and `Stats()` via `metrics.k8s.io`.

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

**The arc wins on why-in-scope and cross-plan ordering.**
[`closed-loop-arc.md`](closed-loop-arc.md) is the program-level source of truth
for this plan. Its
[**shared conventions 1–8**](closed-loop-arc.md#shared-conventions-do-not-restate-these-in-child-plans-link-here)
are inherited verbatim and are **not restated here** — read them there. This
plan's binding of each, and nothing more:

- **Convention 1 (feature gate)** → `CAESIUM_RESOURCE_STATS_ENABLED` (A1) and
  `CAESIUM_RIGHT_SIZING_ENABLED` (C1) on `pkg/env/env.go` `Environment`,
  surfaced as `Features.RightSizing` by D2. **Scope, stated precisely because
  the two flags do not cover the same surface:** they gate route mounting (D2's
  conditional bind), the deterministic-rule wiring in `cmd/start/start.go`
  (E1(b)), the OOM reclassification (A2), the sampler (A3), and the escalation
  ladder (C1) — **and job-definition validation of `rightSizing:` only**.
  `resources:` validation is **ungated**: static limits apply with the learning
  machinery off (design: "off ⇒ no routes bound, `resources:` still applies
  statically"), so gating its validation would make a valid manifest fail
  `caesium job lint` on a default deployment. See B1.
- **Convention 2 (harness)** → H-1, H-2.
- **Convention 3 (explainability)** → C3.
- **Convention 4 (agent actions)** → Stream E. **Overrides** the design doc's
  `POST /v1/jobs/:id/resources/apply` router; see Stream E and its *Design
  history* subsection.
- **Convention 5 (stats substrate)** → A1 (columns), D1 (the definition site of
  the quantile-parameterised `HistorySource`).
- **Convention 6 (citations)** → symbols, never line numbers.
- **Conventions 7–8** → N-1, and the wave hygiene in `## Sequencing`.

This plan implements
[`docs/design-resource-right-sizing.md`](../../design-resource-right-sizing.md).
**The design doc is authoritative for INTENT and SCOPE** — the phasing, the
YAML contract (`resources:` / `rightSizing:` shapes), the cache-identity
exclusion decision, the recommendation formula, the provenance-routing rule,
and the Non-Goals (no mid-run resize, no autoscaling, no cost/dollar modeling,
no per-run manual overrides). **When this plan and the design doc disagree, the
design doc wins**, and this plan is corrected to match — **except** in exactly
**three** enumerated deviations, each called out inline at its item and each
amended in the design doc by N-1:

- (a) **Stream E's apply surface** — arc convention 4 overrides the design's
  `POST /v1/jobs/:id/resources/apply` router (see Stream E).
- (b) **The Kubernetes kind lane** — arc convention 2 promotes the design's
  deferred lane to the in-scope H-2.
- (c) **C1's dynamic `MaxAttempts` grant** — *not* arc-driven: a correctness
  correction to the design's "Attempt budget" paragraph, which pre-stamps
  `MaxAttempts = Retries + 1 + onOOM.maxEscalations` at registration and
  thereby breaks the terminal invariant for plain failures. Rationale in C1;
  amended by N-1.
No item may add a new YAML knob, engine field, endpoint, or `CAESIUM_*` config
beyond what the design enumerates without first amending the design.

Two subordinate contracts: strategic priority/status is tracked in
[`docs/roadmap.md`](../../roadmap.md) — the Phase-4 design-wave table row and
§2.5 (Cost Tracking & Resource Awareness, whose stats substrate this plan's
Phase 0 delivers); the roadmap wins on priority/status disagreements, except
that per arc convention 7 **plan status lives in exactly one place — the arc
dashboard**. The job-definition YAML contract lives in
[`pkg/jobdef/definition.go`](../../../pkg/jobdef/definition.go) and the cache
identity in [`internal/cache/hash.go`](../../../internal/cache/hash.go); because
`Step` embeds `container.Spec` inline (`definition.go` `type Step struct` →
`container.Spec` with a `yaml:",inline" json:",inline"` tag, mirrored in both
`rawStep` declarations), `resources:` flows to the atom model automatically, but
the hash exclusion (Stream B) is load-bearing and the schema wins on any
field-name disagreement.

## Progress (as of 2026-09-05)

No implementation waves have shipped yet. The plan was re-cut on 2026-09-05
against [`closed-loop-arc.md`](closed-loop-arc.md); the first wave is the next
eligible run of the `exec-plan-wave` skill against this doc, and it may only run
after Plan 0 ([`trust-the-substrate.md`](trust-the-substrate.md)) and Plan 1
([`data-circuit-breaker.md`](data-circuit-breaker.md)) — see the arc's
§ Cross-plan sequencing. The design doc's `> Status:` banner already points at
this plan; N-1 flips it to "implemented" and ticks the arc dashboard row.

### Verified facts (re-verified 2026-09-05 — re-grep before you edit)

These are the ground truths every stream below stands on. Each was checked
against `master` on the date above.

1. **`container.Spec` has no `Resources` field.** `pkg/container/spec.go`
   `type Spec struct` is exactly `Env`, `WorkDir`, `Mounts`,
   `ResolvedVolumeMounts`, `Kubernetes`. B1 adds it.
2. **`atom.Engine` has no `Stats()`.** `internal/atom/atom.go` `type Engine
   interface` is `Get`/`List`/`Create`/`Wait`/`Stop`/`Logs`. A3 adds it.
3. **No engine returns `atom.ResourceFailure`.** `grep -rn ResourceFailure`
   over `*.go` hits only the constant (`internal/atom/atom.go`), the classifier
   (`internal/incident/classifier.go` `Classify` + its doc comment), one
   agent-session mapping (`internal/incident/session.go` `terminalState`), and
   the classifier test. All three `resultMap`s map `137 → atom.Killed`; Docker's
   `(*Atom).Result` (`internal/atom/docker/atom.go`) reads only
   `metadata.State.ExitCode`, Podman's `(*Atom).Result` only
   `a.metadata.State.ExitCode`, and Kubernetes' `(*Atom).Result` only
   `terminatedState(c.metadata).ExitCode`. **An OOM today is `killed`.**
4. **`Classify` maps `ResourceFailure` to the WRONG class for this loop.**
   `internal/incident/classifier.go` `Classify` has
   `case atom.StartupFailure, atom.ResourceFailure: return ClassTransientInfra`,
   and that `switch atom.Result(sig.Result)` runs **before** the log-tail regex
   table and the exit-code table. `ClassOOM` is therefore produced **only** by
   `oomLogRe` (`defaultLogRules`) and by `defaultExitCodeRules`' `137 →
   ClassOOM`. So A2 must do **both**: (a) make the engines return
   `ResourceFailure` with OOM evidence, **and** (b) add an OOM-evidenced branch
   that classifies `oom` **ahead of** the `transient_infra` case — otherwise
   making engines honest would *regress* today's accidental `oom` classification
   into `transient_infra`. This is the single most load-bearing fact in the plan.
5. **`TaskRun` already has `ExitCode *int`** (`internal/models/run.go`, with a
   `gorm:"type:integer"` tag and the "0 is a real code, NULL is never captured"
   contract). It has **none** of `PeakMemoryBytes`, `CPUSeconds`, `StatsSource`,
   `OOMKilled`, `AppliedResources`, `EscalationLevel`. A1's add-list is the six
   columns, **not** `ExitCode`.
6. **Neither `CAESIUM_RESOURCE_STATS_ENABLED` nor `CAESIUM_RIGHT_SIZING_ENABLED`
   nor `CAESIUM_GIT_WRITE_CREDENTIALS` exists** anywhere in `*.go`, `*.yml`, or
   the `justfile`. `internal/rightsizing/` does not exist. `internal/windowsched/`
   does not exist either (Plan 4 creates it).
7. **`ActionTypeApplyJobdefPatch` is a catalogue name only.**
   `internal/incident/actions.go` declares it and `actionCatalog` maps it to
   `TierApproval`, but `(*Executor).dispatch` has **no case** for it — it falls
   to the `default:` branch and returns `ErrUnknownAction`. Its pipeline is
   built by Plan 0 C4 (proposal → `ApprovalRequest` → `awaiting_approval`) and
   C7 (approve → `ExecuteApproved` → `dispatch`, including the **direct**
   `apply_jobdef_patch` route through the shipped jobdefs diff/apply service),
   plus Plan 1 **F3** (the **Git-PR provenance route** of `apply_jobdef_patch`,
   the item that also adds `CAESIUM_GIT_WRITE_CREDENTIALS` to `Environment`;
   [`data-circuit-breaker.md`](data-circuit-breaker.md) § Stream F is drafted,
   F1–F5). **Stream E consumes that
   pipeline; it does not build any of it.**
8. **Fan-out instances are their own `TaskRun` rows.** `internal/models/run.go`
   `TaskRun` carries `PartitionValue`, `PartitionIndex`, `PartitionCount`,
   `PartitionFingerprint`, `PartitionAttributes`, `PartitionDependsOn`, and the
   unique index `idx_taskrun_jobrun_task` is `(job_run_id, task_id,
   partition_index)`
   ([`dynamic-fanout.md`](../completed/dynamic-fanout.md): "one `TaskRun` row"
   per instance). **Per-partition peak stats therefore come free in Stream A** —
   A1 adds columns to the one row shape every instance already uses; no
   fan-out-specific code, no new stream.
9. **`RetryTaskClaimed` no longer exists.** `internal/run/store.go` documents
   that it was removed as unusable; the distributed lane uses
   `(*Store).RetryTaskClaimedInstance` in `internal/run/store_instance.go`
   (called from `internal/worker/runtime_executor.go` `(*runtimeExecutor).Execute`).
   The local lane uses `(*Store).RetryTask` → `retryTask`. **C2's citation is
   refreshed accordingly.**
10. **The local attempt loop still does not retry unsuccessful results.** In
    `internal/job/job.go`, the `runTask` closure inside `(*job).Run` loops
    `for attempt := 1; attempt <= maxAttempts; attempt++` around `executeAtom`;
    when `execErr == nil` but the result is unsuccessful it calls
    `CompleteTaskWithPartitions` and returns
    `fmt.Errorf("task %s failed with result %q", ...)` — **without re-entering
    the loop**. The distributed worker (`internal/worker/runtime_executor.go`
    `(*runtimeExecutor).Execute`) does retry unsuccessful results. C1's
    local-loop retryability fix is still required.
11. **`Playbook.decide` never auto-executes tier 3.**
    `internal/incident/executor.go` `(Playbook).decide` returns
    `decisionApprove` for every `tier >= TierApproval`, unconditionally. So
    `mode: auto` (E2) can automate **proposing**, never **approving** — see E2.
12. **Deterministic class→action rules exist — and they BYPASS the playbook.**
    `internal/incident/rules.go` declares `type DeterministicRule` (fields
    `Name`, `Class`, `ActionType`, params; `Name` is "the operator-facing rule
    name recorded on the incident timeline", e.g. `RuleAutoRetryBackoff` /
    `RuleSnoozeUntilCron`), `func DefaultRules() []DeterministicRule` (today:
    `transient_infra → retry_from_failure`, `data_unavailable →
    snooze_retry`), `NewRules(...)` and
    `DefaultRuleSet() *Rules { return NewRules(DefaultRules()...) }` — the
    latter assembled exactly once, in `cmd/start/start.go`
    (`incidentSub.SetRemediator(incExecutor, incident.DefaultRuleSet())`).
    Rules are matched by `(*Rules).Match` and executed by
    `(*Executor).ApplyDeterministicRule`, which calls
    `(*Executor).ExecutePolicy` (actor `policy`). **`ExecutePolicy` never calls
    `(Playbook).decide` and dispatches with a zero `Playbook{}`** — its doc
    comment says so outright ("no playbook allowlist gate (a deterministic rule
    is pre-approved by being deterministic)"). Only `(*Executor).Execute`
    consults `pb.Allow`. **Consequence, load-bearing for E1/E2: anything added
    to `DefaultRules()` fires on every matching incident in every deployment.**
    E1's hook is therefore the rule table *plus* an explicit env-gated
    assembly in `cmd/start/start.go` — not `DefaultRules()` itself. Note the
    side effect of
    fact 4: once OOM classifies as `oom`, it stops matching the
    `transient_infra → retry_from_failure` rule — correct, because C's in-run
    escalation is the better first response.
13. **No engine sets resource limits.** `internal/atom/kubernetes/engine.go`
    builds `Containers: []v1.Container{...}` with no `Resources` field (the only
    `Resources:` in that file is `v1.VolumeResourceRequirements` on a PVC
    claim template); `internal/atom/docker/engine.go` builds
    `&dockercontainer.HostConfig{Mounts: mounts}`; `internal/atom/podman/engine.go`
    builds a `specgen.SpecGenerator` with no `ContainerResourceConfig`.

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Phase 0 substrate — `TaskRun` stats columns (per-partition for free), `Stats()` on the engine interface + sampling, honest OOM reclassification to `ResourceFailure` **and to `ClassOOM`**, Prometheus families | **P0** | Not started |
| B | Phase 1 — `resources:` block through all three engines, lint validation, cache-identity exclusion + test, descriptor schema bump, distributed flow | **P0** | Not started |
| C | Phase 2 — `onOOM` escalation ladder in both executors, local-loop retryability fix, persisted escalation state, **`why` provenance (C3)** | P1 | Not started |
| D | Phase 3 — shared `HistorySource` reader + compute-on-read recommendation engine (p99 across fan-out partitions) + `GET /v1/jobs/:id/resources` + `GET /v1/stats/resources` + `caesium job resources` CLI | P2 | Not started |
| E | Phase 4 — `propose_resources` incident action → `apply_jobdef_patch` proposal through the Plan 0/Plan 1 pipeline; `mode: auto`; conservative downsizing | P3 | Not started |
| F | UI — JobDetail Resources panel, attempt-trail badges, RunDetail anomaly ribbon, stats reclaim view | P2 | Not started |
| H-1 | Integration harness — `build/` stress image, feature envs on **every self-server lane** | — | Not started |
| H-2 | Kubernetes kind lane — pod OOM reason, requests/limits, `metrics.k8s.io` stats (arc convention 2) | — | Not started |
| N-1 | Docs — design banner, roadmap, generated schema reference, examples, README, **`docs/tour-compute-loop.md`**, arc dashboard | — | Not started |

## Streams

### Stream A — Phase 0: stats substrate + honest OOM detection

The reactive substrate every other stream builds on. Today the `Engine`
interface is Get/List/Create/Wait/Stop/Logs (`internal/atom/atom.go`) with
no `Stats()`, `TaskRun` (`internal/models/run.go`) has the `ExitCode` column but
no memory/CPU/OOM columns, and OOM is invisible. This stream captures the truth
and reclassifies OOM kills — the largest blast radius (the engine interface +
all three engine adapters + `TaskRun` + the run store + metrics + the incident
classifier), so it merges first. Gate the new behavior behind
`CAESIUM_RESOURCE_STATS_ENABLED` (default `false`) so enabling it is the only
thing that changes OOM classification (release-noted).

- [ ] A1. Add the Phase 0 `TaskRun` columns, their run-store persistence, the
      Prometheus families, and the `CAESIUM_RESOURCE_STATS_ENABLED` /
      `..._SAMPLE_INTERVAL` (default 10s) env gate. Columns:
      `PeakMemoryBytes *int64`, `CPUSeconds *float64`,
      `StatsSource string` (`sampled|oom_inferred|none`),
      `OOMKilled bool`, `AppliedResources datatypes.JSON` (the limits the final
      attempt ran with), `EscalationLevel int` (written by Stream C, but the
      column is added here so Phase 0 owns the single migration).
      **`ExitCode *int` ALREADY EXISTS** on `TaskRun` (`internal/models/run.go`,
      shipped for the incident classifier) — do **not** re-add it; read it, and
      make sure the engines' `ExitCode()` value keeps reaching it on the OOM
      path A2 introduces. No new table, so **no `internal/models/models.go`
      change** — `TaskRun` is already in the `All` slice; note this explicitly to
      whoever picks up the item. Metrics:
      `caesium_task_oom_kills_total`, plus the §2.5-named
      `caesium_task_memory_peak_bytes` and `caesium_task_cpu_seconds_total`
      (declared in the `var (...)` block AND added to the `prometheus.MustRegister`
      list inside `internal/metrics/metrics.go` `Register()` — two edit sites —
      with a `internal/metrics/testutil` assertion in a `*_test.go`).
      **Fan-out synergy (arc § Synergies, "Fan-out children inherit the template
      step's `resources:`"):** a fanned instance is its **own `TaskRun` row**
      (`PartitionValue`/`PartitionIndex`/`PartitionCount`; unique index
      `idx_taskrun_jobrun_task` = `(job_run_id, task_id, partition_index)`), so
      these columns are **per partition for free** — every write site must
      address the row by its own `TaskRun` primary key, never by
      `(job_run_id, task_id)`, or one partition's peak overwrites its siblings'
      (the exact bug class `internal/run/store.go` `retryTask`'s doc comment
      records). Add a store unit test that writes distinct peaks to two
      instances of one fanned group and reads both back.
      **Also add the completion-path setter A2 and A3 call** — model it on the
      shipped `(*Store).SetTaskExitCode` (`internal/run/store.go`), which
      resolves the row through `loadTaskRunByIDOrUnique` "so a fan-out instance
      records its own exit code instead of overwriting its siblings'": a
      sibling `(*Store).SetTaskResourceOutcome(runID, taskRef uuid.UUID, …)`
      writing `OOMKilled` / `PeakMemoryBytes` / `CPUSeconds` / `StatsSource`
      under the same taskRef contract. Naming it here means A2/A3 add call
      sites, not query shapes.
      Files: `internal/models/run.go`, `internal/run/store.go`,
      `internal/metrics/metrics.go` (+ `metrics_test.go`), `pkg/env/env.go`.
- [ ] A2. Reclassify OOM kills to `atom.ResourceFailure` per engine, at the
      inspect each engine already performs, and capture the exit code onto the
      result. Docker: consult `InspectResponse.State.OOMKilled` in
      `(*Atom).Result()` (`internal/atom/docker/atom.go`) **before** the
      exit-code `resultMap` (`internal/atom/docker/docker.go`). Kubernetes: check
      `terminatedState(c.metadata).Reason == "OOMKilled"` in
      `(*Atom).Result()` (`internal/atom/kubernetes/atom.go` — the terminated
      state is already fetched by `terminatedState` and shared with
      `(*Atom).ExitCode()`, but only its `ExitCode` is read today). Podman:
      check `InspectContainerState.OOMKilled` in `(*Atom).Result()`
      (`internal/atom/podman/atom.go`). Gate the reclassification on
      `CAESIUM_RESOURCE_STATS_ENABLED` so off ⇒ OOM stays `killed`
      (compatibility, honestly). Update the three `resultMap` round-trip
      tests (`*/atom_test.go`) to cover the OOM path.
      **AND (the half the arc's synergy table names, without which this item is
      a regression):** `internal/incident/classifier.go` `Classify` today runs
      `switch atom.Result(sig.Result) { case atom.StartupFailure,
      atom.ResourceFailure: return ClassTransientInfra }` **before** the log-tail
      and exit-code tables, so `ClassOOM` is reachable only via `oomLogRe` and
      the `137 → ClassOOM` entry in `defaultExitCodeRules`. Add an
      **OOM-evidenced branch that returns `ClassOOM` ahead of that
      `transient_infra` case** — evidence being the new `TaskRun.OOMKilled`
      column surfaced on `Signal` (add an `OOMKilled bool` field to
      `incident.Signal` and populate it where the signal is derived, in
      `internal/incident/subscriber.go`). A `ResourceFailure` **without** OOM
      evidence (eviction, quota rejection) keeps falling through to
      `transient_infra` — that is still the right class for it. Update the
      `Classify` doc-comment precedence list and
      `internal/incident/classifier_test.go` (whose
      `{"resource_failure", …, ClassTransientInfra}` row must gain an
      OOM-evidenced twin asserting `ClassOOM`). Record in the item's PR body
      that `internal/incident/rules.go` `DefaultRules()` maps `transient_infra →
      retry_from_failure`, so this reclassification deliberately stops a plain
      identical retry from firing on OOM — Stream C's escalation replaces it.
      **AND persist the flag — nothing else does.** `TaskRun.OOMKilled` is the
      evidence the classifier branch stands on, and no other item writes it:
      A1 adds the column + setter, A3 owns the *sampler's*
      `PeakMemoryBytes`/`CPUSeconds`/`StatsSource` writes only. So at task
      completion, alongside the two shipped `store.SetTaskExitCode(...,
      a.ExitCode())` call sites — `internal/job/job.go` (inside `(*job).Run`'s
      completion path) and `internal/worker/runtime_executor.go` — call A1's
      `SetTaskResourceOutcome` with the engine's OOM verdict, addressed by the
      same taskRef contract so a fanned instance records its own flag.
      Files: `internal/atom/docker/atom.go`, `internal/atom/docker/docker.go`,
      `internal/atom/kubernetes/atom.go`, `internal/atom/podman/atom.go`
      (+ the three `atom_test.go`), `internal/incident/classifier.go`,
      `internal/incident/classifier_test.go`, `internal/incident/subscriber.go`,
      `internal/job/job.go`, `internal/worker/runtime_executor.go`,
      `internal/run/store.go` (the A1 setter's call sites only).
      Depends on: A1.
- [ ] A3. Add `Stats()` to the `atom.Engine` interface and a sampling loop in
      both executors, writing `PeakMemoryBytes`/`CPUSeconds`/`StatsSource`.
      Docker/Podman sample `ContainerStats` (cgroup v2 dropped
      `max_usage_in_bytes`, so peak = max of samples; under-reports sub-interval
      spikes — the OOM flag from A2 is the corrective ground truth). Kubernetes
      reads `metrics.k8s.io`; absent metrics-server, degrade to
      `StatsSource=oom_inferred` (or `none`), never guess. Wire the sampler into
      the local executor (`internal/job/job.go`, alongside the `executeAtom`
      call in the `runTask` closure) and the distributed worker
      (`internal/worker/runtime_executor.go` `(*runtimeExecutor).monitorTask` is
      the existing per-task watch loop — the natural host), gated by
      `CAESIUM_RESOURCE_STATS_ENABLED`. Unit-test the sample-max reducer and the
      k8s degradation path against fake stats. **The k8s path is not
      unit-test-only any more** — H-2 drives it on kind; if metrics-server is
      not installable in that lane, H-2 asserts the *documented graceful-degrade
      path* (`StatsSource=oom_inferred`) instead, and this item's k8s branch must
      make that degradation observable rather than silent.
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
      (`pkg/container/spec.go` `type Spec struct` — verified today to hold only
      `Env`/`WorkDir`/`Mounts`/`ResolvedVolumeMounts`/`Kubernetes`), plus the
      `resources:` and job/step-level `rightSizing:` YAML shapes on `Step` /
      `metadata` in `pkg/jobdef/definition.go` (the `type Step struct`, **both**
      `rawStep` declarations inside `UnmarshalYAML` and `UnmarshalJSON`, and
      `Validate()`) and the JSON-schema surface (`pkg/jobdef/schema.go`).
      Lint-enforced semantics: `resources` without `rightSizing` is valid
      (static limits); `rightSizing` requires `resources`;
      `memory.max ≥ resources.memory ≥ memory.min`; parse quantities at
      lint/apply.
      **Gate scope (arc convention 1, stated precisely):** `resources:`
      validation is **ungated** — static limits are valid on a default
      deployment with the learning machinery off, so a flag check here would
      make a valid manifest fail `caesium job lint`. Only the `rightSizing:`
      block is gated: add a `rightSizingFeatureEnabled()` helper to
      `pkg/jobdef/definition.go` modelled exactly on the shipped
      `freshnessFeatureEnabled()` in that same file (and called from `Validate()`
      the way the freshness check is), reading `CAESIUM_RIGHT_SIZING_ENABLED`;
      off ⇒ a `rightSizing:` block is rejected with the same
      "feature disabled" shape freshness uses (that helper is a direct
      `os.Getenv` + `strconv.ParseBool` read, so B1 does **not** need C1's
      `pkg/env/env.go` field to have landed). **No `cmd/start/start.go` change
      belongs to this item** — the only startup wiring this plan adds is E1(b)'s
      env-gated rule assembly; route mounting is D2's conditional bind.
      Because `Step` embeds `container.Spec` inline (the
      `yaml:",inline" json:",inline"` tag) and `(*Definition).RuntimeSpecForStep`
      persists the resolved spec, the field reaches the atom model with no extra
      plumbing — assert that in a unit test.
      Files: `pkg/container/spec.go`, `pkg/jobdef/definition.go`,
      `pkg/jobdef/schema.go`, `internal/jobdef/runtime/spec.go`.
- [ ] B2. Apply `resources` through the three engine adapters. Docker: map to
      `HostConfig.Resources.Memory` / `NanoCPUs` — `internal/atom/docker/engine.go`
      builds `&dockercontainer.HostConfig{Mounts: mounts}` for mounts only today.
      Podman: `specgen.SpecGenerator`'s `ContainerResourceConfig` /
      `ResourceLimits` (`internal/atom/podman/engine.go` sets
      `ContainerBasicConfig`/`ContainerStorageConfig`/`ContainerHealthCheckConfig`
      only). Kubernetes: set `requests = limits` for memory and `requests` only
      for CPU on the `v1.Container` in the pod spec
      (`internal/atom/kubernetes/engine.go` — its `Containers: []v1.Container{…}`
      sets no `Resources` today; the only `Resources:` in the file is the
      `v1.VolumeResourceRequirements` on a PVC claim template, do not confuse
      them) — note that changing a memory request changes **Kueue admission**
      arithmetic for `kueue:`-queued steps (`container.KubernetesSpec.QueueName`;
      disclosed later in the recommendation UI). Limits-only; no admission
      control on plain Docker hosts (the kernel's OOM killer is the signal A2
      now catches).
      Files: `internal/atom/docker/engine.go`,
      `internal/atom/kubernetes/engine.go`, `internal/atom/podman/engine.go`.
      A2/A3 edit the same three engine packages (different methods), so B2
      sequences after them — see `## Sequencing`, cross-stream file conflicts.
      Depends on: B1, A2, A3.
- [ ] B3. Exclude `resources` from cache identity, prove it, and carry the
      applied limits on the descriptor. Do **not** add `Resources` to
      `cache.HashInput` (`internal/cache/hash.go`) — follow the
      `QueueName` "scheduling metadata, not an execution input" precedent
      (the `HasIdentityFields` / `out.QueueName = ""` normalisation in
      `hash.go` and `pkg/container/spec.go`); add a `hash_test.go` case asserting
      two specs differing only in `resources` hash byte-identically. Bump the
      execution descriptor schema — `models.TaskExecutionDescriptorSchemaVersion`
      is `1` and `models.TaskExecutionDescriptor` carries `ContainerSpec
      container.Spec` with no resources field — so `AppliedResources`
      and the escalation trail can ride the descriptor to a distributed worker
      (`internal/worker/runtime_executor.go` `(*runtimeExecutor).loadAtomSpec` /
      the `descriptor.ContainerSpec` application path runs on its own node).
      Document the honest counter-case (a self-sizing JVM whose
      output depends on its limit is non-deterministic under this rule; escape
      hatch is `cache: false` or a `version` bump).
      **Downstream consumer:** [`backtesting.md`](backtesting.md) **F3
      (optional/stretch)** — the resources-only replay override — relies on
      *both* halves of this item: the hash exclusion (so a resource override is
      not a new identity) and the descriptor field (so the replay knows what the
      recorded run actually ran with). Because the exclusion means a
      resources-only change is a cache **hit**, F3 also needs a forced
      re-execution path; **that forced re-execution is Plan 3 F3's to own**, not
      this item's. Say so in the descriptor field's doc comment.
      Files: `internal/cache/hash.go` (+ `hash_test.go`), the execution
      descriptor (`internal/models/run.go` `TaskExecutionDescriptor` +
      `TaskExecutionDescriptorSchemaVersion`), `internal/worker/runtime_executor.go`.
      Depends on: B1.

### Stream C — Phase 2: OOM retry escalation (+ the plan's `why` provenance)

Turn an OOM into a green run instead of an identical death. Hook the existing
per-attempt loops in both executors; the local loop is asymmetric today (the
`runTask` closure inside `internal/job/job.go` `(*job).Run` retries only
*execution errors* — when `execErr == nil` and the result is unsuccessful it
calls `CompleteTaskWithPartitions` and returns
`fmt.Errorf("task %s failed with result %q", …)` without re-entering the loop —
whereas the distributed worker `internal/worker/runtime_executor.go`
`(*runtimeExecutor).Execute` already retries unsuccessful results), so
escalation needs a local-loop retryability fix or it works only in distributed
mode.

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
      **Deviation from the design doc, deliberate and already reconciled:** the
      design's "Attempt budget" paragraph says to stamp
      `MaxAttempts = Retries + 1 + onOOM.maxEscalations` at registration. This
      item does not, for the terminal-invariant reason above; the design's
      Safety section's "they extend `MaxAttempts` explicitly at registration"
      line is amended by N-1 to say "dynamically, on an actual escalation".
      Files: `internal/job/job.go`, `internal/run/store.go`, `pkg/env/env.go`.
      Depends on: A1 (the `EscalationLevel`/`AppliedResources`/`OOMKilled`
      columns; `ExitCode` already exists), A2 (the OOM classification),
      B1, B2 (the `resources` field to escalate), B3 (the descriptor bump that
      carries the escalated per-attempt spec to a distributed worker — the
      prerequisite C2's escalated spec copy names).
- [ ] C2. Implement the escalation step + its persistence across both executors.
      Next attempt's memory = `min(applied × factor, memory.max)` quantized up to
      64Mi; already at `memory.max` ⇒ no attempt consumed (fail now, classified,
      trail attached — never burn an attempt on a doomed identical retry). Run a
      per-attempt spec copy with escalated `Resources`, nothing else changed
      (this is why the local-loop fix in C1 and the descriptor bump in B3 are
      prerequisites). **Citation refreshed 2026-09-05:** the distributed
      per-attempt reset is `(*Store).RetryTaskClaimedInstance`
      (`internal/run/store_instance.go`, called from
      `internal/worker/runtime_executor.go` `(*runtimeExecutor).Execute`) — the
      `RetryTaskClaimed` the earlier draft named **no longer exists** and its
      removal is documented in `internal/run/store.go` above `(*Store).RetryTask`
      (it re-pended a row that `StartTaskClaimed` would then refuse to start).
      `RetryTaskClaimedInstance` additionally persists `EscalationLevel` +
      the next attempt's `AppliedResources` so a re-claimed distributed task
      resumes at the escalated size; the local lane's `(*Store).RetryTask` →
      `retryTask` does the same for local attempts. `(*Store).RetryFromFailure`
      (`internal/run/store.go`; and its admitted twin
      `RetryFromFailureAdmitted`) resets escalation state to level 0 with the
      attempt reset. Because escalation state lives on the **instance** row, a
      fanned group escalates per partition independently — assert that in a
      store test (one partition OOMs and escalates; its siblings' rows are
      untouched). Metrics: `caesium_task_oom_escalations_total` (var block +
      `Register()`); escalated attempts also count in the existing
      `metrics.TaskRetriesTotal` (`caesium_task_retries_total`).
      Files: `internal/job/job.go`, `internal/worker/runtime_executor.go`,
      `internal/run/store.go`, `internal/run/store_instance.go`,
      `internal/metrics/metrics.go`.
      Depends on: C1.
- [ ] C3. **(Arc convention 3 — this plan's single explainability item.)** Make
      every automated compute-loop decision a persisted event that the shipped
      **task-scoped** explainer renders. Add three `event.Type` constants in
      `internal/event/bus.go` following the existing `snake_case` noun_verb
      convention (`TypeTaskRetrying = "task_retrying"`, `TypeApprovalRequested =
      "approval_requested"`, `TypeDatasetAdvanced = "dataset_advanced"` are the
      shape to match): `task_resources_escalated` (attempt N OOM'd at M, attempt
      N+1 runs at M′), `task_resources_applied` (the limits an attempt actually
      ran with, including "inherited from the template step" for a fanned
      instance), and `resource_recommendation_proposed` (published by E1 — the
      constant lands here so `internal/event/bus.go` is touched by exactly one
      item in this plan, per the arc's file-conflict table). Flow them through
      `internal/notification/subscriber.go` like their neighbours. Render them in
      the attempt block of `internal/run/why.go` (`(*Store).WhyTask` /
      `WhyTaskPartition` → the `WhyExplanation` attempt/verdict fields, beside
      `loadTrigger`/`WhyTrigger`) and in `cmd/why/why.go` `renderTable`, so
      `caesium why <run-id> --task <task> --job-id <job-id>` and
      `GET /v1/jobs/:id/runs/:run_id/why` (`api/rest/controller/why` `Get`, bound
      in `api/rest/bind/bind.go` `Protected()`) both say it. **Do not add a
      run-level `why` verb** (arc convention 3). One integration scenario in
      `test/` drives the real surface: force an OOM with the H-1 stress image at
      a low limit, let C2 escalate, then assert the CLI's **stdout** (captured
      with the stream-separating `runCLIStdout`, `test/data_plane_e2e_test.go`,
      never the merging `runCLIRaw` in `test/local_dev_test.go`) contains the
      causal sentence — "ran at &lt;N&gt; because attempt 1 OOM'd at &lt;M&gt;" —
      and that the REST `why` payload carries the same provenance.
      Files: `internal/event/bus.go`, `internal/notification/subscriber.go`,
      `internal/run/why.go`, `cmd/why/why.go`, new/extended `test/` scenario.
      Depends on: C2 (the escalation it explains). E1 publishes
      `resource_recommendation_proposed` into the type this item defines.

### Stream D — Phase 3: shared history reader + recommendation engine + REST + CLI

Compute-on-read — no new store, no background fleet scans. `window = last N
successful runs of (job, task name)` (default N=20, reset on image-digest
change); `suggest = quantize_up(p99(peak_mem) × (1 + headroom))` clamped to
`[min, max]`, never below `max(window)`; CPU the same as a suggestion only.

- [ ] D1. Build the recommendation engine and its guard rails as a pure package,
      **on top of the shared quantile-parameterised history reader this item
      defines** (arc convention 5). Minimum sample count (default 5); downward
      suggestions suppressed while the §2.5 anomaly condition holds (latest run
      > 2× rolling average); OOM-killed attempts are censored observations
      (peak ≥ limit) forcing the suggestion to at least
      `applied × onOOM.factor`; exclude quarantined replays and
      backtesting runs (`quarantine IS NOT TRUE` — `models.JobRun.Quarantine`
      and `models.TaskRun.Quarantine` are the shipped columns) and backfill
      storms unless opted in. Percentile-plus-headroom, not a model — fully
      unit-tested (p99 math, clamping, never-below-max, OOM-censoring, anomaly
      suppression, insufficient-samples). Env tuning knobs:
      `CAESIUM_RIGHT_SIZING_WINDOW_RUNS` (20), `..._PERCENTILE` (99),
      `..._HEADROOM` (0.2), `..._MIN_SAMPLES` (5).
      **`HistorySource` (the arc's "one signal-source interface"):** define a
      small injectable interface in `internal/rightsizing/` — **quantile-
      parameterised**, so one query shape serves `p99` for right-sizing and
      `p95` for windows — over (a) the `TaskRun` timestamps that exist **today**
      (`StartedAt`/`CompletedAt`; the same `completed_at − started_at` shape
      `api/rest/service/stats/stats.go` `(*Service).durationExpr` computes,
      tz/driver-aware) and (b) the A1 stats columns (`PeakMemoryBytes`,
      `CPUSeconds`, `StatsSource`, `OOMKilled`, `AppliedResources`,
      `EscalationLevel`). It must expose the quantile as a parameter, filter
      cache-short-circuited rows (`TaskRun.CacheHit`) and quarantined rows, and
      let a caller exclude escalated/OOM-killed attempts.
      [`window-scheduling.md`](window-scheduling.md) **B1 adopts this reader**
      for its `P95(jobID)` / `ETA(runID)` predictor rather than querying
      `task_runs` its own way; per arc convention 5 **this item is the
      definition site**. Plan 4 B1 has already been re-cut to import it (its
      text: "**Read history through the quantile-parameterised `HistorySource`
      reader Plan 2 D1 defines**… B1 **does not define** that interface", and
      its Files line reads "importing `internal/rightsizing`'s `HistorySource`
      (Plan 2 D1) — no new reader"), so there is no discrepancy to flag. **Keep
      the interface shape stable** — the quantile parameter and the
      `CacheHit`/quarantine/escalated-attempt filters — because Plan 4 B1
      carries a documented timestamp-only fallback *of the same shape* for the
      case where this item has not shipped when it lands.
      **Fan-out synergy:** the window for a fanned step is the set of **instance
      rows** across runs, so aggregate `p99` **across partitions of the same
      template step** (group by `(job, task name)` and fold every
      `partition_index`), not per partition — one `resources:` block is declared
      on the template step and fan-out children **inherit** it (design
      Non-Goals: "the horizontal analog is `design-dynamic-fanout.md`'s
      territory, whose fan-out children inherit the template step's
      `resources`"). Expose per-partition peaks as detail for the D2 read and
      the F2 UI, but recommend one number per step. Unit-test that a
      100-partition group yields one suggestion computed over all 100 peaks and
      does not treat each partition as a separate step.
      Files: new `internal/rightsizing/` (`recommend.go`, `history.go` +
      `_test.go` for each), `pkg/env/env.go`.
      Depends on: A1 (the stats columns to read), B1 (declared bounds).
- [ ] D2. Add the observability reads as Echo controllers beside
      `api/rest/controller/stats/`: `GET /v1/jobs/:id/resources` (per-step
      declared vs observed — p50/p99/max/OOM over the window, with per-partition
      detail for fanned steps — plus the suggestion and utilization %) and
      `GET /v1/stats/resources` (fleet rollup, complementing §2.5's planned
      `/v1/jobs/:id/costs`). Bind both in
      `Protected()` of `api/rest/bind/bind.go`, gated by
      `CAESIUM_RIGHT_SIZING_ENABLED` (off ⇒ routes not bound; the
      conditional-mount precedent is the `if contractsvc.Enabled()` guard around
      `/contracts/graph` in that file). Add a
      `RightSizing` field to the `Features` struct
      (`api/rest/service/system/system.go` `type Features struct`, beside
      `FreshnessEnabled`/`ContractEnforcementEnabled`) so the UI gates on it.
      Metric for pending-suggestion count if surfaced. **These are reads only** —
      the apply route the earlier draft put beside them is gone (see Stream E).
      Files: new `api/rest/controller/resources/`, new
      `api/rest/service/resources/`, `api/rest/bind/bind.go`,
      `api/rest/service/system/system.go`, new/extended `test/` scenario (the
      `CLAUDE.md` gate: a new REST endpoint with no live-surface scenario blocks
      review).
      Depends on: D1.
- [ ] D3. Add the CLI: `caesium job resources <alias> [--json]` (observed vs
      declared + suggestions), `--apply [--step transform]` (wired in Stream E),
      and `caesium job resources --all --format markdown` (fleet report /
      PR-body-ready). `--json` and `--format markdown` go to **stdout, clean and
      parseable**, asserted with the stream-separating `runCLIStdout`
      (`test/data_plane_e2e_test.go`) — never the merging `runCLIRaw`
      (`test/local_dev_test.go`). New `cmd/job/resources.go` registered
      under the existing `job.Cmd` group (`cmd/job/job.go`) — no `cmd/execute.go`
      change (the `job` group is already in the `cmds` slice).
      Files: new `cmd/job/resources.go`, `cmd/job/job.go`, new/extended `test/`
      scenario driving the CLI binary (the `CLAUDE.md` gate: a new `cmd/`
      subcommand with no live-surface scenario blocks review).
      Depends on: D2.

#### Deferred — `resource_recommendations` cache table

The design lists an optional `resource_recommendations` **lazily-recomputed
cache** table for Phase 3. This plan computes on read (no new table, no
`internal/models/models.go` change), matching the design's "computed on read"
default. The cache table is **deferred** — draft it as a follow-on only if
read-time recomputation proves too slow on large fleets. Not a gate here.

### Stream E — Phase 4: `propose_resources`, through the one approval pipeline

**Recast 2026-09-05 per
[arc convention 4](closed-loop-arc.md#shared-conventions-do-not-restate-these-in-child-plans-link-here)**
(read the convention there; the code-grounded decision that follows is this
plan's, and stays). The durable half of the compute loop is an
**incident action**, not a new REST router: an `oom` incident (the class A2
makes reachable) carries a `propose_resources` action whose parameters are the
Stream D recommendation; executing it materialises an `apply_jobdef_patch`
proposal that flows through the pipeline **Plan 0 C4/C7 builds** (proposal →
`ApprovalRequest` → `awaiting_approval` → human `approve` →
`ExecuteApproved` → `dispatch`, with the **direct** jobdefs diff/apply route for
jobs without git provenance) and **Plan 1 F3** extends (the **Git-PR
provenance route**, behind the `CAESIUM_GIT_WRITE_CREDENTIALS` env field F3
adds). This stream
builds **none** of that pipeline and **adds nothing to it**.

**Why the design's `POST /v1/jobs/:id/resources/apply` provenance router is NOT
built — decided from the code, 2026-09-05.** The design specifies an endpoint in
`api/rest/service/resources/` that routes by `Job.Provenance*`, opens a Git PR
or round-trips `jobdefs` diff/apply, refuses under `CAESIUM_AUTH_MODE=none`, and
is cooldown-limited. Every one of those five behaviours is *already* the
contract of `apply_jobdef_patch`: `internal/incident/actions.go` catalogues it at
`TierApproval`, Plan 0 C7 implements exactly the provenance branch (direct apply
for non-git jobs, degrade-to-`escalate` with the rendered diff for git-synced
ones), Plan 1 F3 replaces that degradation with the PR route, and
`docs/design-agent-in-the-loop.md` already states the router is "provenance-
routed, enforced server-side" and refused when auth mode is `none`. A second
router would be a second implementation of the same five rules, a second audit
spine, and a second thing to keep refused under `AUTH_MODE=none` — the exact
duplication convention 4 forbids. **So the endpoint is dropped**, not
front-ended: a thin front would still need a job-scoped surface with no incident
to attach an action to, and `models.AgentAction` rows are incident-scoped by
construction (`(*Executor).verifyActionBoundary` enforces the incident boundary
as the security boundary). The operator-initiated path instead stays entirely
client-side (E1(c)).

- [ ] E1. Add `propose_resources` as an incident **action** and hang it off the
      `oom` class. (a) **Catalog:** add
      `ActionTypeProposeResources = "propose_resources"` to
      `internal/incident/actions.go` and register it in `actionCatalog` at
      `TierGated` (tier 2 — "autonomous only if explicitly allowed by the
      playbook"), extend `ActionParams` with the typed recommendation fields it
      needs (target step name, recommended memory/cpu, the window it was
      computed over, the observed p99/max, and the `AppliedResources` of the
      OOM-killed attempt), and add its `dispatch` case: call
      `internal/rightsizing/` for the recommendation (D1), clamp to the declared
      `[min, max]`, render the minimal YAML patch, and **create an
      `apply_jobdef_patch` proposal** through the executor rather than applying
      anything itself — the returned `map[string]any` records the patch and the
      proposal's action id. (b) **Deterministic rule — and its gate, which is
      NOT the playbook.** Add the rule *constant and constructor* to
      `internal/incident/rules.go`: `RuleProposeResources = "propose_resources"`
      beside `RuleAutoRetryBackoff`/`RuleSnoozeUntilCron` (`DeterministicRule.Name`
      is "the operator-facing rule name recorded on the incident timeline", so
      the literal is `{Name: RuleProposeResources, Class: ClassOOM, ActionType:
      ActionTypeProposeResources}`), exposed as its own constructor —
      e.g. `func ProposeResourcesRule() DeterministicRule`.
      **Do NOT add it to `DefaultRules()`.** Verified fact 12: a matched
      deterministic rule runs through `(*Executor).ApplyDeterministicRule` →
      `(*Executor).ExecutePolicy`, and `ExecutePolicy` **never calls
      `(Playbook).decide`** — it dispatches with a literal `Playbook{}` and its
      doc comment states "no playbook allowlist gate (a deterministic rule is
      pre-approved by being deterministic)". `pb.Allow` is consulted only on the
      `Execute` path. So a rule in `DefaultRules()` would fire on **every** `oom`
      incident in **every** deployment — the opposite of opt-in, and a silent
      behaviour change on upgrade. Gate it instead at the single assembly site:
      `cmd/start/start.go` calls
      `incidentSub.SetRemediator(incExecutor, incident.DefaultRuleSet())` exactly
      once — make that
      `incident.NewRules(append(incident.DefaultRules(),
      incident.ProposeResourcesRule())...)` **only when
      `CAESIUM_RIGHT_SIZING_ENABLED` is true**, otherwise leave
      `DefaultRuleSet()` untouched. Assert both branches (rule present/absent) in
      a unit test, and assert the off-branch end-to-end in the
      "gates off ⇒ inert" verification bullet. **Cross-plan wave note:**
      `cmd/start/start.go` is on the arc's sequential-by-plan file list, so this
      sub-item is another reason 2-E never shares a wave with 1-F3 or 3-F. (c)
      **Bundle + MCP:** carry the recommendation into the agent's evidence —
      add a `BundleResources` section to `internal/incident/bundle.go` `Bundle`
      (declared vs observed, the escalation trail, the D1 suggestion) beside
      `BundleFailure`/`BundleRun`, so the `get_bundle` and `get_context` tools in
      `internal/mcp/tools.go` expose it and the agent can reach
      `propose_action` with a grounded number instead of a guess. (d)
      **Operator-initiated `--apply` (D3's flag), client-side only:** render the
      recommendation as a minimal YAML patch; for a job **without** git
      provenance, hand it to the **shipped** jobdef apply surface the
      `caesium job apply` path already uses (no new endpoint, no new service);
      for a **git-synced** job (`models.Job`'s `ProvenanceSourceID/Repo/Ref/
      Commit/Path` set) **refuse the direct apply** and print the patch for the
      operator to commit — a direct DB write would be reverted by the next sync.
      (e) Publish the `resource_recommendation_proposed` event C3 defines.
      **Cross-plan prerequisites (documented here, not on the `Depends on:`
      line, per the playbook's in-plan-ids rule):**
      [`trust-the-substrate.md`](trust-the-substrate.md) **C4 + C7** — the
      proposal → `ApprovalRequest` → approve → execute pipeline and the
      **direct** `apply_jobdef_patch` route — and
      [`data-circuit-breaker.md`](data-circuit-breaker.md) **F3**, the **Git-PR
      provenance route** of `apply_jobdef_patch` (the item that also adds
      `CAESIUM_GIT_WRITE_CREDENTIALS` to `Environment`). Never in the same wave
      as Plan 1 Stream F or Plan 3 Stream F (shared `internal/incident/` files
      and `cmd/start/start.go`).
      Files: `internal/incident/actions.go`, `internal/incident/rules.go`,
      `internal/incident/bundle.go`, `internal/mcp/tools.go`,
      `cmd/start/start.go` (the env-gated rule assembly),
      `internal/rightsizing/`, `cmd/job/resources.go` (the `--apply` flag),
      new/extended `test/` scenario (the auth-lane propose → approve → execute
      path H-1 describes).
      Depends on: D2, D3.
- [ ] E2. Add `mode: auto` end-to-end and the conservative downsizing policy,
      **through the same E1 action**. **`auto` is a proposal-side setting only,
      and it is NOT a new trigger.** There is no window-boundary evaluator in
      this plan: no background processor, no `cmd/start/start.go` goroutine, and
      **no new table** (`internal/models/models.go` stays unchanged). `auto` on
      a step means exactly this: when E1(b)'s env-gated deterministic rule is
      installed and an `oom` incident opens for that job, the rule is allowed to
      produce the proposal with no human in the *propose* step — the human is
      still in the **approve** step. If a deployment wants a periodic
      right-sizing sweep independent of OOM incidents, that is a follow-on item
      (a `internal/rightsizing/proposer.go` + `cmd/start/start.go` wiring +
      per-job cooldown state), explicitly **out of scope here** — say so rather
      than implying a scheduler exists.
      **Proposal-frequency control, using state that already exists.** The
      "never one proposal per run" property is inherited, not built: incidents
      dedupe by key with occurrence-append and a cooldown window
      (`internal/incident/store.go` `OpenParams.Cooldown` /
      `(*Store).OpenOrAppend`, fed by `NewSubscriber`'s cooldown), so repeated
      OOMs on one job append to one incident instead of opening N. The
      remaining per-job proposal cooldown is **computed on read** from run
      history through D1's `HistorySource` (was there an accepted/executed
      `apply_jobdef_patch` for this step inside the cooldown window, and has a
      full window elapsed since?), matching Stream D's compute-on-read stance —
      no cooldown column, no cooldown table. If an implementer finds the
      on-read derivation insufficient, that is a scope change: add the item and
      the table explicitly, and reverse the "`internal/models/models.go` — no
      change" line in `## Sequencing`.
      **Verified constraint (do not design around
      it silently):** `internal/incident/executor.go` `(Playbook).decide` returns
      `decisionApprove` for every `tier >= TierApproval`, unconditionally, so a
      tier-3 `apply_jobdef_patch` **cannot** be auto-approved by any playbook
      setting that exists today. If true unattended apply is wanted, that is a
      change to tier semantics in the shipped runtime and must be raised as an
      amendment to `docs/design-agent-in-the-loop.md` first — **not** smuggled in
      here. **Note also that the playbook allowlist is not the gate on the
      deterministic path** (verified fact 12: `ExecutePolicy` dispatches with
      `Playbook{}` and never calls `decide`) — E1(b)'s env-gated rule assembly
      in `cmd/start/start.go` is; `pb.Allow` gates only agent-initiated
      `Execute` calls. Keep the conservative downsizing rules unchanged:
      **downsizing is conservative** — `auto` downsizes only after a full
      OOM-free window (D1's history reader answers "no `OOMKilled` attempt in
      the last N successful runs of this step"), and an OOM after an auto
      downsize reverts immediately and freezes downsizing for the cooldown
      (both derived from the same reader, per the frequency-control paragraph
      above). Record the composition seam with
      agent-in-the-loop inline: with the incident runtime enabled, `oom` is a
      deterministic rule that **defers to in-run escalation** (Stream C) and a
      proposal is produced only when bounds exhaust, pre-diagnosed
      ("OOMKilled at 4Gi and 6Gi; raise `memory.max`").
      Files: `internal/incident/rules.go`, `internal/rightsizing/`,
      `pkg/env/env.go`, new/extended `test/` scenario (the downsizing gates
      acceptance criterion 6 now asserts).
      Depends on: E1.

#### Design history — the original Stream E (superseded 2026-09-05)

Preserved verbatim so the rationale is not lost. The **intent** below is
unchanged and is delivered by E1/E2 above; only the **surface** changed, per arc
convention 4.

> `mode: auto` (and explicit `--apply`) routes exactly like the agent-in-the-loop
> doc's `apply_jobdef_patch`, reusing its provenance-routed GitOps-patch
> machinery. **Git-synced job** (`Job.Provenance*` fields set,
> `internal/models/job.go`): a direct DB apply is *rejected* (the next sync
> reverts it) — the recommendation renders as a minimal YAML patch to
> `ProvenancePath`, opened as a Git PR. **Non-git job**: staged through the normal
> `jobdefs/diff` + `apply` path, audit-logged.
>
> **E1 (original).** Add `POST /v1/jobs/:id/resources/apply` and its provenance
> router. On a git-synced job, render the recommendation as a minimal YAML patch
> and open a Git PR against `ProvenancePath` (requires
> `CAESIUM_GIT_WRITE_CREDENTIALS`; absent, degrade to `suggest` with the rendered
> diff attached), batched per job per window and cooldown-limited — never one PR
> per run. On a non-git job, round-trip the `jobdefs` diff/apply path,
> audit-logged. The applier never exceeds declared `[min, max]` bounds; the
> direct-apply route is **refused when `CAESIUM_AUTH_MODE` is `none`** (an
> unauthenticated apply route must not exist — the agent-doc master-gate
> reasoning; PR-routed proposals are safe regardless since a human merges). Wire
> the `--apply` flag into the D3 CLI. Operator-authenticated (`RoleRunner`/operator
> RBAC). Files: new `api/rest/controller/resources/apply.go`,
> `api/rest/service/resources/`, `api/rest/bind/bind.go`, `pkg/env/env.go`
> (`CAESIUM_GIT_WRITE_CREDENTIALS`), `cmd/job/resources.go` (the `--apply` flag).
> Depends on: D2, D3.
>
> **E2 (original).** Add `mode: auto` end-to-end and the conservative downsizing
> policy. `auto` applies within bounds through the E1 router on its window
> boundary; **downsizing is conservative** — `auto` downsizes only after a full
> OOM-free window, an OOM after an auto downsize reverts immediately and freezes
> downsizing for the cooldown. Record the composition seam with the
> agent-in-the-loop doc inline (with that doc enabled, `oom` becomes a
> deterministic rule deferring to in-run escalation and an incident opens only
> when bounds exhaust, pre-diagnosed) — no new caller here, documented as a
> forward reference. Files: `api/rest/service/resources/`,
> `internal/rightsizing/`, `pkg/env/env.go`. Depends on: E1.

What carried over unchanged: the provenance rule itself (git-synced ⇒ never a
direct DB write), the `[min, max]` clamp, the `AUTH_MODE=none` refusal (now
inherited from the one router), batching + cooldowns, the conservative
downsizing policy, and the agent-composition seam — which is no longer a
"forward reference" but the mechanism. What moved: `CAESIUM_GIT_WRITE_CREDENTIALS`
is now **Plan 1 F3's** env field, not this plan's, and
`api/rest/{controller,service}/resources/` holds **reads only** (D2).

### Stream F — UI (`ui/src/features/jobs/`)

Surfaces the backend through the jobs feature, gated on the `RightSizing`
`Features` flag from D2.

- [ ] F1. Add the JobDetail Resources panel: per-step declared limit vs
      observed-peak sparkline, utilization %, a suggestion badge
      ("declared 4Gi · p99 412Mi · suggest 512Mi"), and a one-click Apply
      rendered as "Open PR" with a diff preview on git-synced jobs. **Recast
      with Stream E:** the button no longer calls a bespoke apply endpoint —
      on a job with an open `oom` incident it deep-links to the incident's
      proposal/approval card (the shipped incident UI), and otherwise it shows
      the rendered patch with a copy affordance and the `caesium job resources
      --apply` invocation. New method(s) in `ui/src/lib/api.ts` for the D2
      reads; Playwright e2e against the live backend.
      Files: `ui/src/features/jobs/JobDetailPage.tsx` (+ a new Resources panel
      component under `ui/src/features/jobs/`), `ui/src/lib/api.ts`.
      Depends on: D2, E1.
- [ ] F2. Add the attempt-trail, anomaly ribbon, and fleet reclaim views.
      TaskDetail/TaskMetadata panels show the per-attempt applied limits with OOM
      badges ("attempt 1 OOMKilled at 1Gi → attempt 2 at 1.5Gi ✓"); RunDetail
      gets an anomaly ribbon when a run's peak exceeded 2× the rolling average
      (the §2.5 rule); the stats page gets a fleet reclaim view (top
      overprovisioned steps, reclaimable memory, OOM leaderboard) with a
      pending-suggestion count joining `useNavCounts.ts`. For a **fanned** step
      the attempt trail is per instance (each partition is its own `TaskRun`
      row) — show the partition key on the badge and the aggregate p99 on the
      step, matching D1's "recommend one number per step, expose per-partition
      detail".
      Files: the TaskDetail/TaskMetadata panels + `RunDetailPage` + the stats
      page under `ui/src/features/`, `ui/src/features/jobs/useNavCounts.ts`,
      `ui/src/lib/api.ts`.
      Depends on: C2 (the escalation trail data), D2 (observed/suggestion reads).

## Harness Strengthening

- [ ] H-1. Make **every self-server lane** exercise the real resource path
      ([arc convention 2](closed-loop-arc.md#shared-conventions-do-not-restate-these-in-child-plans-link-here)
      — rationale there, not repeated here). Add a small `build/` stress image
      (`build/Dockerfile.stress`, published as a `caesiumcloud/…` tag alongside
      the existing `build/Dockerfile`, `Dockerfile.build`, `Dockerfile.reagents`,
      `Dockerfile.triage-agent`) that allocates N MiB on demand so scenarios can
      force a **real** OOM against real Docker in CI. Set
      `CAESIUM_RESOURCE_STATS_ENABLED=true` and `CAESIUM_RIGHT_SIZING_ENABLED=true`
      (plus a tight `..._MIN_SAMPLES` / `onOOM` bound if the escalation tests need
      it) on the server block of **each** of these justfile recipes and their
      `.github/workflows/ci.yml` jobs, mirroring the lineage
      `CAESIUM_OPEN_LINEAGE_ENABLED` precedent the `CLAUDE.md` gate calls out:
      - `integration-up` / `integration-test` → CI job `build-and-integration-test`
        (and its arm64 twin `build-and-integration-test-arm64`)
      - `integration-up-distributed` / `integration-test-distributed` → CI job
        `build-and-integration-test-distributed`
      - `integration-up-owner-memory` / `integration-test-owner-memory` → CI job
        `build-and-integration-test-owner-memory`
      - `integration-up-agent` / `integration-test-agent` → CI job
        `build-and-integration-test-agent-auth`
      - `integration-up-infra` / `integration-test-infra` → CI jobs
        `build-and-integration-test-infra` and `-infra-arm64`
      - `integration-test-podman` → CI job `podman-integration-test`
      - the helm/kind lane → **H-2** (its env lives in a Helm values file, not a
        `docker run` block)

      **Auth-lane scenarios for Stream E.** Approvals are auth-gated, so the
      `propose_resources` → `apply_jobdef_patch` → approve → execute scenario runs
      on `just integration-test-agent`. That lane runs in **local** execution
      mode (arc convention 2), which suits this plan: OOM + escalation + the
      direct (non-git) `apply_jobdef_patch` route all work locally; nothing here
      needs a re-executing quarantined replay. **Cross-plan dependency, stated
      inline:** that lane is hollow today —
      [`trust-the-substrate.md`](trust-the-substrate.md) **H-1** makes it real
      and wide (its own Recon Ledger records why; do not re-derive it here) and
      **C4 + C7** wire the approval pipeline the scenario asserts; the Git-PR
      half additionally needs
      [`data-circuit-breaker.md`](data-circuit-breaker.md) **F3**. Land
      this item in the first wave so Stream A's end-to-end gate has a stress
      image + enabled stats to drive; the E scenario lands with E1.
      Files: new `build/Dockerfile.stress`, `justfile`,
      `.github/workflows/ci.yml`, `test/` harness helpers.
- [ ] H-2. **Make the helm/kind lane the Kubernetes verification bar** (arc
      convention 2: "Kubernetes runtime paths (OOM reason, requests/limits, pod
      stats) are exercised on the **helm/kind lane** — kind-in-CI is the
      verification bar for k8s work in this arc"). This **replaces** the earlier
      draft's "Deferred — Kubernetes kind lane" note, which is preserved below.
      The lane already exists: `.github/workflows/ci.yml` job
      `helm-integration-test` creates a kind cluster (`kind create cluster
      --wait 120s`), `kind load docker-image`s the Caesium image, `helm install`s
      with `helm/caesium/ci/test-values-k8s.yaml`, pre-loads job images
      (`docker pull alpine:3.23` + `kind load docker-image alpine:3.23`),
      port-forwards `service/caesium`, extracts the CLI, and runs
      `go test ./test/ -tags=integration` with `CAESIUM_TEST_ENGINE=kubernetes`.
      Three concrete hooks:
      1. **Feature envs** — append `CAESIUM_RESOURCE_STATS_ENABLED` and
         `CAESIUM_RIGHT_SIZING_ENABLED` to `config.extraEnv` in
         `helm/caesium/ci/test-values-k8s.yaml` (beside the existing
         `CAESIUM_OPEN_LINEAGE_ENABLED` / `CAESIUM_FRESHNESS_ENABLED` /
         `CAESIUM_CACHE_PIN_DIGESTS` entries), and to the
         `--set config.extraEnv[N]` chain in the justfile's `k8s-distributed`
         recipe so the local k8s path matches CI.
      2. **Stress image into the cluster** — extend the workflow's "Pre-load job
         images into kind" step to `kind load docker-image` the H-1 stress image
         (a kind node cannot pull a locally-built image otherwise), and pin any
         additional image to the canonical form (`alpine:3.23`,
         `busybox:1.36.1`, or a `caesiumcloud/…` tag).
      3. **`metrics.k8s.io`** — add a metrics-server install step after "Create
         kind cluster" (`kubectl apply` the components manifest with
         `--kubelet-insecure-tls`, then wait for the deployment) so A3's
         Kubernetes `Stats()` branch actually executes. **If that proves
         unreliable in kind, the honest v1 is to skip the install and assert the
         documented graceful-degrade path instead** — `StatsSource=oom_inferred`
         (or `none`) with a recommendation that reports "insufficient samples"
         rather than a guess. Decide once, and say which was chosen in the PR
         body and in N-1's tour; do **not** leave both possibilities in the doc.

      Scenarios (guarded on `CAESIUM_TEST_ENGINE=kubernetes`): a pod OOM is
      recorded as `atom.ResourceFailure` with `OOMKilled=true` and classified
      `oom` (A2's `terminatedState(...).Reason == "OOMKilled"` branch);
      `resources:` reaches the pod as requests+limits for memory and requests
      for CPU (B2 — assert via the recorded descriptor/`AppliedResources`, and
      via `kubectl get pod -o jsonpath` on the created pod where the lane can
      observe it); and either real `metrics.k8s.io` peaks or the degrade path
      per hook 3.
      Files: `helm/caesium/ci/test-values-k8s.yaml`,
      `.github/workflows/ci.yml` (`helm-integration-test`), `justfile`
      (`k8s-distributed`), `test/` (engine-guarded scenarios).
      Depends on: A2, A3, B2, H-1.

#### Superseded — "Deferred — Kubernetes kind lane" (kept for history)

The pre-arc draft deferred all of this. Preserved so the reasoning is on the
record; **it no longer holds** — arc convention 2 makes kind-in-CI the bar, and
the lane the note calls hypothetical already exists in `ci.yml`.

> K8s result-mapping and metrics-API degradation are **unit-tested with fake pod
> statuses** (CI has no cluster). A kind-based integration lane that runs the real
> K8s engine path (limits applied, OOM `Terminated.Reason` observed,
> metrics-server degradation) is **deferred to a follow-up**, approximated locally
> by `just k8s-distributed` + `just helm-test`. Not a gate here.

## Navigational / Organizational Improvements

- [ ] N-1. Reconcile the docs after A–F ship, per arc convention 7. Flip the
      [`docs/design-resource-right-sizing.md`](../../design-resource-right-sizing.md)
      `> Status:` banner from "Brainstorm/Design → active — Plan 2 of the
      closed-loop arc" to "implemented by this plan" (and mark shipped phases),
      and amend the **three** design paragraphs this plan deliberately deviates
      from (the same three the Source-Of-Truth Note enumerates): (a) the
      `POST /v1/jobs/:id/resources/apply` endpoint (Stream E routes through
      `apply_jobdef_patch` instead — cite arc convention 4), (b) the deferred
      Kubernetes kind lane (now H-2 — cite arc convention 2), and (c) the
      "Attempt budget" registration pre-stamp, whose Safety-section line "they
      extend `MaxAttempts` explicitly at registration" becomes "dynamically, on
      an actual escalation" (C1). Update `docs/roadmap.md`: the Phase-4
      design-wave table row (the "Resource right-sizing" row) and §2.5 (Cost
      Tracking & Resource Awareness) — **confirm its `Status:` line still reads
      "Items 1, 2 and 4 land as the Phase 5 Plan 2 Stream A stats substrate …"
      and flip its tense to shipped**; do **not** narrow it to "items 1–2": item
      4 is the `caesium_task_cpu_seconds_total` /
      `caesium_task_memory_peak_bytes` metric pair A1 registers. **Status itself
      lives only in
      the arc dashboard**, so the roadmap row links rather than duplicating a
      status column. Document the `resources:` / `rightSizing:` / `onOOM` fields
      in `docs/job-definitions.md` and `docs/caesium-job-llm-reference.md`, and
      **for `docs/job-schema-reference.md` update the GENERATOR
      (`internal/jobdef/report`), never the doc by hand** —
      `TestGeneratedSchemaReferenceIsCurrent` compares the doc to the
      generator's output and a hand-edit turns master red. Add a static-limits
      example and a right-sizing example under `docs/examples/*.job.yaml` with
      **canonically pinned images** (`alpine:3.23`, `busybox:1.36.1`, or
      `caesiumcloud/…` — `internal/guardrails/guardrails_test.go`
      `TestPinnedContainerImageVersionsAreConsistent` scans
      `.md`/`.yaml`/`.ts`/`.tsx`/`.go`/Dockerfiles under `docs/`, `build/`,
      `test/`, `ui/` and friends and fails any unpinned or off-version
      reference to the images in its `pinnedImages` map). Index this plan in
      `docs/README.md` in
      **backtick/inline-code** form (the `TestDocsREADMEIndexesEveryTopLevelDoc`
      guardrail rejects clickable subdirectory links). **Write
      `docs/tour-compute-loop.md`** (arc convention 7): the 10-minute
      walkthrough a newcomer follows on their own machine to see the loop
      close — declare `resources:` too small, watch the OOM become
      `resource_failure`, watch the escalation retry green, read
      `caesium why` naming the escalation, read `caesium job resources --json`,
      and see the `oom` incident's `propose_resources` → approval → patch. Tick
      this plan's row in the [`closed-loop-arc.md`](closed-loop-arc.md) **arc
      dashboard** and repoint its links when the plan moves to
      `docs/exec-plans/completed/`, in the same PR. Runs last.
      Files: `docs/design-resource-right-sizing.md`, `docs/roadmap.md`,
      `internal/jobdef/report/` (the schema-reference generator) +
      `docs/job-schema-reference.md` (regenerated), `docs/job-definitions.md`,
      `docs/caesium-job-llm-reference.md`, `docs/examples/`, `docs/README.md`,
      new `docs/tour-compute-loop.md`,
      `docs/exec-plans/active/closed-loop-arc.md`.
      Depends on: A3, B3, C3, D3, E2, F1, F2, H-1, H-2 (i.e. the leaf of every
      stream — runs last, after the runtime ships).

## Sequencing & Dependencies

**Cross-plan order (the arc wins).** This plan runs **after Plan 0
([`trust-the-substrate.md`](trust-the-substrate.md)) and Plan 1
([`data-circuit-breaker.md`](data-circuit-breaker.md))**. Two hard edges:

- **Stream E after Plan 0 C4/C7 and after Plan 1 F3.** E1 depends on the
  approval pipeline *existing* (C4 proposal → `ApprovalRequest`; C7 approve →
  execute + the direct `apply_jobdef_patch` route) and on Plan 1 F3 for the
  Git-PR route. Verified fact 7: `dispatch` has no `apply_jobdef_patch` case
  today. **Sequential by plan; never the same wave as Plan 1 Stream F or Plan 3
  Stream F** (all three edit `internal/incident/{actions,rules,executor}.go`).
- **Stream A may overlap Plan 1's late waves but never Plan 1 Stream A** — both
  edit `internal/job/job.go` and `internal/worker/runtime_executor.go` (arc
  file-conflict table).
- **Plan 4 Stream B1 adopts D1's `HistorySource`** (arc convention 5). Plan 4 is
  optional and runs after Plan 3; nothing here waits on it.
- **Plan 3 Stream F3 (optional) reads B3's** hash exclusion + descriptor
  resources and owns the forced re-execution a resources-only replay needs.

**Cross-stream order (within this plan):**

- **Stream A is the foundation** — B, C, and D all consume the stats columns,
  the OOM classification, or the `Stats()` sampling A adds. A merges first
  (largest blast radius: engine interface + three adapters + `TaskRun` + run
  store + metrics + the incident classifier). **A1 → A2 → A3 is strict.**
- **Stream B** depends on A: B2 edits the same three engine packages A2/A3 edit
  (different methods — A touches `(*Atom).Result()`/inspect + `Stats()`; B
  touches `Create()`/pod spec), so B sequences **after** A rather than parallel.
- **Stream C** depends on A1 (the `EscalationLevel`/`AppliedResources`/`OOMKilled`
  columns; `ExitCode` already exists), A2 (the OOM class) and B1/B2/B3 (the
  `resources` field to escalate and the descriptor bump that carries escalated
  limits to workers). C3 depends on C2.
- **Stream D** depends on A1 (stats columns) + B1 (declared bounds); it does not
  need C or E.
- **Stream E** depends on D2 + D3 **and cross-plan on Plan 0 C4/C7 + Plan 1 F3.**
- **Stream F** depends on D2 (reads) + E1 (the proposal deep-link) + C2 (the
  escalation-trail data for the attempt badges).
- **H-1** is independent (build image / justfile / CI / test harness); land it
  in the first wave so the A end-to-end gate has a stress image + enabled stats.
  **H-2** depends on A2/A3/B2 (it verifies them) and on H-1 (the stress image).
- **N-1** runs last, after A–F ship.

**Suggested waves:**
- **W1 = A (A1 → A2 → A3) + H-1.** A is one strict chain; H-1 provides the
  stress image and enabled-stats server the A scenarios drive.
- **W2 = B (B1 → (B2, B3)).** Unblocked once A's engine edits are in; B2 and B3
  are parallel after B1 (different files). **H-2 is not in this wave** — it
  depends on B2, and `exec-plan-wave` dispatches a wave's streams in parallel
  from `master`, so an in-wave edge is a dispatch failure, not a hint.
- **W3 = C (C1 → C2 → C3) + D (D1 → D2 → D3) + H-2.** C and D are both
  unblocked once B is in; H-2 verifies A2/A3/B2 with B merged. C touches the
  executors + run store + `internal/event/bus.go` + `why`; D touches a new
  package + controllers; H-2 touches `justfile`/`ci.yml`/helm values — different
  files, sharing only additive `pkg/env/env.go` and
  `internal/metrics/metrics.go` append sites.
- **W4 = E (E1 → E2).** E depends on D and on Plan 0 C4/C7 + Plan 1 F3 having
  merged. E1 → E2 is a strict chain inside the wave.
- **W5 = F (F1, F2) + N-1.** F1 depends on E1 (the proposal deep-link) and D2;
  F2 on C2 + D2. N-1 runs last, after every leaf item.

**Within-stream order:** A1 → A2 → A3 (strict — columns/env, then per-engine OOM
classification + classifier branch, then `Stats()` sampling). B1 → (B2, B3)
parallel. C1 → C2 → C3. D1 → D2 → D3. E1 → E2. F1, F2 parallel (F2 also needs C2).

**Cross-stream file conflicts:**

- `internal/atom/{docker,kubernetes,podman}/` — A2/A3 (OOM classification +
  `Stats()`) and B2 (apply limits) both edit the three engine packages, in
  different methods. **Sequence A → B** (already a dependency); never the same
  wave.
- `internal/job/job.go` + `internal/worker/runtime_executor.go` — A2 (the
  `SetTaskResourceOutcome` call beside each shipped `SetTaskExitCode` call
  site), A3 (sampling loop) and C1/C2 (escalation loop) all edit both
  executors; A2 → A3 is already strict within Stream A. **Sequence A → C**
  (A in W1, C in W3). B3 also touches the descriptor-apply region of
  `runtime_executor.go` in W2 — different region, but flag the A→B→C ordering on
  this file. **Cross-plan:** the arc's file-conflict table pairs these with
  Plan 0 A3/A4 (cancel plumbing) and Plan 1 A4 — 2-A may overlap Plan 1's D/E
  waves, never Plan 1's Stream A.
- `internal/incident/{classifier,actions,rules,executor,bundle}.go` — A2
  (classifier branch + `Signal.OOMKilled`) in W1 and E1 (action + rule + bundle)
  in W4. Different files within the package, different waves. **Cross-plan this
  is the sharpest edge:** the arc's table lists `internal/incident/{classifier,
  actions,executor}.go` + `cmd/start/start.go` as **sequential by plan** across
  0-C4/C7, 1-F, 2-E, 3-F. Never run 2-E in a wave with 1-F or 3-F.
- `cmd/start/start.go` — **only E1** in this plan (the env-gated
  `SetRemediator(incExecutor, …)` rule-set assembly; no other item touches
  startup wiring). The arc lists this file as **sequential by plan** across
  0-C4/C7, 1-F, 2-E, 3-F — never run 2-E in a wave with any of them.
- `internal/run/store.go` + `internal/run/store_instance.go` — A1 (column
  persistence + registration `MaxAttempts` stamp) and C1/C2 (dynamic
  `MaxAttempts` bump on actual OOM escalation — never a pre-stamp;
  `RetryTaskClaimedInstance` escalation persist; `RetryFromFailure` reset).
  **Sequence A → C**.
- `internal/models/run.go` (`TaskRun`) — **only A1** adds columns (including
  `EscalationLevel`/`AppliedResources` that C writes) and **only B3** bumps
  `TaskExecutionDescriptorSchemaVersion` / `TaskExecutionDescriptor`. A1 in W1,
  B3 in W2 — different regions, different waves. C writes existing columns.
- `internal/event/bus.go` + `internal/notification/subscriber.go` — **only C3**
  in this plan (all three event types land there at once, including the one E1
  publishes). Arc rule: one plan's explainability item per wave across plans.
- `internal/run/why.go` + `cmd/why/why.go` — **only C3**. Same arc rule.
- `pkg/jobdef/definition.go` — **only B1** (the `resources`/`rightSizing` schema +
  `Validate` + `Step` + **both** `rawStep` declarations). The true-conflict file
  is owned by one stream; the arc pairs it with 1-A3, 3-A3, 4-A2 — one plan per
  wave.
- `internal/cache/hash.go` — **only B3** (the exclusion + test). No other stream
  touches the hash.
- `pkg/env/env.go` — A1 (`CAESIUM_RESOURCE_STATS_ENABLED`, `..._SAMPLE_INTERVAL`),
  C1 (`CAESIUM_RIGHT_SIZING_ENABLED`), D1 (`..._WINDOW_RUNS/_PERCENTILE/_HEADROOM/
  _MIN_SAMPLES`), E2 (auto/cooldown knobs) all append fields.
  Additive across waves; within **W3, C1 + D1 both append** — flag for a clean
  rebase (different lines). **`CAESIUM_GIT_WRITE_CREDENTIALS` is no longer this
  plan's field** — Plan 1 F3 adds it.
- `internal/metrics/metrics.go` — A1 (oom_kills / memory_peak / cpu_seconds) in
  W1, then C2 (oom_escalations) in W3, each a two-site edit (`var (...)` +
  `Register()`). C2 is the only W3 metrics append (D reads, adds none) — no
  same-wave metrics overlap.
- `api/rest/bind/bind.go` — **only D2** in this plan now (two GET routes in
  `Protected()`); E adds no route. Additive; one plan's item per wave.
- `api/rest/service/system/system.go` — **only D2** adds the `RightSizing`
  `Features` field.
- `cmd/job/job.go` — D3 registers `resources.go` under the `job` group; **no
  `cmd/execute.go` change** (the `job` group is already in the `cmds` slice).
  E1 adds the `--apply` behaviour inside `cmd/job/resources.go` only.
- `ui/src/lib/api.ts` + `ui/src/features/jobs/` — F1 + F2 both append; same
  stream, sequences mechanically. Arc rule: one plan's UI stream per wave.
- `justfile` + `.github/workflows/ci.yml` — H-1 and H-2 both edit both. **H-1
  first** (H-2 loads the image H-1 builds); never the same wave as another
  plan's H-1.
- `internal/models/models.go` — **no change** (no new table;
  `resource_recommendations` is deferred). No hot-table router entry either.

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Per-stream additions:

- **New REST endpoint / CLI verb (D):** an integration scenario in `test/`
  that drives the **real surface** — `GET /v1/jobs/:id/resources` against the
  live server, or the CLI binary via `s.runCLI*` — and asserts observed output.
  For `--json` / `--format markdown`, capture **stdout separately from stderr**
  with `runCLIStdout` (`test/data_plane_e2e_test.go`) and assert clean,
  parseable output; the merged-stream `runCLIRaw` capture hides log leaks.
- **OOM classification + escalation (A, B, C):** run the `build/` stress image at
  a low memory limit that forces a real OOM against real Docker in CI → assert
  `result == resource_failure`, `OOMKilled == true`, **the incident class is
  `oom` and not `transient_infra`**, exit code + peak recorded
  (via `GET /v1/jobs/:id/resources` and `caesium job resources --json`); an
  escalation green-run (attempt 2 at the escalated size succeeds, identical cache
  hash across attempts); bounds-exhaustion → classified failure; a plain non-OOM
  failure does **not** consume escalation attempts; the distributed lane with a
  forced mid-ladder re-claim (escalation level persists through
  `RetryTaskClaimedInstance`).
- **Kubernetes (A2, A3, B2 — H-2):** the same three assertions on the kind lane,
  guarded on `CAESIUM_TEST_ENGINE=kubernetes`: pod OOM reason →
  `resource_failure` + `oom`; `resources:` present on the pod as
  requests+limits (memory) / requests (CPU); `Stats()` from `metrics.k8s.io`
  **or** the documented degrade (`StatsSource=oom_inferred`), whichever H-2
  chose.
- **Fan-out (A1, C2, D1):** a fanned group where one partition OOMs — its
  sibling rows keep their own peaks and are not escalated; the D1 recommendation
  is one number computed across all partitions of the template step.
- **Explainability (C3):** the `why` scenario asserting the causal sentence
  ("ran at N because attempt 1 OOM'd at M") on **stdout** via `runCLIStdout`,
  plus the same provenance in the `GET /v1/jobs/:id/runs/:run_id/why` payload.
- **Approval path (E):** on the auth-enabled lane (`just integration-test-agent`),
  an `oom` incident produces a `propose_resources` action, that action creates an
  `apply_jobdef_patch` proposal in `awaiting_approval`, `caesium incident
  approve` **executes** it, and a non-git job's definition actually changes.
  Requires Plan 0 H-1/C4/C7 merged. Three more assertions on the same lane:
  the executed apply is **refused when the server runs with
  `CAESIUM_AUTH_MODE=none`** (drive it on a lane/sub-case with auth off, or
  assert the refusal at the service boundary the one router enforces); the
  patch that is applied is **clamped to the declared `[min, max]`** even when
  the raw recommendation exceeds `memory.max`; and an `auto` step **does not**
  propose a downsize until a full OOM-free window has passed, with a
  post-downsize OOM producing an immediate revert + a frozen downsize for the
  cooldown (seed the history the derivation reads).
- **Gates off ⇒ inert (A, B, C, D, E):** with the feature envs unset, assert no
  stats columns written, no right-sizing routes bound, **the
  `propose_resources` rule is never installed** (the `cmd/start/start.go`
  assembly leaves `DefaultRuleSet()` untouched — a unit test on both branches,
  since `ExecutePolicy` would otherwise fire it unconditionally), and OOM
  results stay `killed` (and classify as they did before). **`resources:` is
  the exception and must stay valid with both flags off** — assert
  `caesium job lint` accepts a static-`resources:` manifest on an ungated
  server, and rejects a `rightSizing:` block there.
- **New metric (A1, C2):** assert via `internal/metrics/testutil` in a `*_test.go`;
  the collector must also appear in `Register()`.
- **Job-schema change (B1):** `caesium job lint --path docs/examples/` green on
  the new `resources:` / `rightSizing:` examples; an invalid bound
  (`memory.max < resources.memory`) rejected at lint; the generated
  `docs/job-schema-reference.md` regenerated from `internal/jobdef/report` so
  `TestGeneratedSchemaReferenceIsCurrent` stays green.
- **Cache identity (B3):** a `hash_test.go` case proving two specs differing only
  in `resources` hash byte-identically.
- **UI changes (F):** `just ui-lint && just ui-test && just ui-e2e` (Playwright
  against the live backend).
- **This plan's checkbox ticked**, the active-wave `## Progress` bullet appended,
  and any cross-linked doc (design banner / roadmap / schema generator)
  refreshed in the same PR.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream A — the Phase 0 substrate** is live: with `CAESIUM_RESOURCE_STATS_ENABLED`
   on, an OOM-killed task is recorded as `resource_failure` with `OOMKilled=true`,
   an exit code, and a peak-memory value on `TaskRun`; **the incident classifier
   returns `oom` for it, not `transient_infra`**; and
   `caesium_task_oom_kills_total` / `caesium_task_memory_peak_bytes` /
   `caesium_task_cpu_seconds_total` are registered. Per-partition peaks are
   recorded on each fanned instance's own row. Closed by a `test/` scenario that
   OOMs the `build/` stress image against real Docker and asserts the recorded
   stats + classification.
2. **Stream B — declared `resources:`** flows through all three engines: a step
   with `resources: {memory, cpu}` runs under those limits (Docker/Podman/K8s),
   the field is excluded from the cache hash (proven by `hash_test.go`), and it
   rides the bumped descriptor to a distributed worker. Closed by a lint scenario
   on the new examples + the cache-identity unit test + a distributed run.
3. **Stream C — OOM retry escalation** works in both executors: an OOM at the
   declared limit retries at `memory × factor` clamped to `memory.max`, a green
   run results, the attempt trail + `EscalationLevel` + `AppliedResources` persist
   (surviving a distributed re-claim through `RetryTaskClaimedInstance`),
   bounds-exhaustion yields a classified failure, and a plain failure does not
   consume escalation attempts. Closed by the escalation-green-run +
   bounds-exhaustion + distributed-reclaim scenarios, green in CI, with
   `caesium_task_oom_escalations_total` asserted.
4. **C3 — the loop is explainable:** `caesium why <run-id> --task <task>
   --job-id <job-id>` says "ran at N because attempt 1 OOM'd at M", the same
   provenance appears in `GET /v1/jobs/:id/runs/:run_id/why`, and the assertion
   is on **stdout** captured with `runCLIStdout`.
5. **Stream D — the recommendation engine + reads + CLI** ship:
   `GET /v1/jobs/:id/resources` returns declared-vs-observed (p50/p99/max/OOM) +
   a clamped `p99 × headroom` suggestion, `GET /v1/stats/resources` returns the
   fleet rollup, and `caesium job resources --json` emits clean parseable stdout;
   a fanned step yields **one** suggestion aggregated across its partitions; and
   the quantile-parameterised `HistorySource` in `internal/rightsizing/` is the
   only run-history query shape this plan adds. **Closed by integration
   scenarios that seed N real runs and assert the suggested value via both REST
   and the CLI binary** (CLI output asserted with the stream-separating
   `runCLIStdout`).
6. **Stream E — the durable action** is live: an `oom` incident carries a
   `propose_resources` action (tier 2, playbook-gated) whose execution creates an
   `apply_jobdef_patch` **proposal**; that proposal reaches `awaiting_approval`,
   and on `caesium incident approve` it **executes** — applying the patch for a
   non-git job through the shipped jobdefs diff/apply route, or opening a Git PR
   for a git-synced job once Plan 1 F3's route exists. The agent bundle
   carries declared-vs-observed + the escalation trail. **No second apply path
   and no second approval surface were added** —
   `POST /v1/jobs/:id/resources/apply` does not exist. **The safety properties
   carried over from the superseded Stream E hold and are asserted, not merely
   described:** (a) an approved `apply_jobdef_patch` is **refused when
   `CAESIUM_AUTH_MODE` is `none`** (inherited from the one router, not
   re-implemented); (b) the applied patch **never exceeds the declared
   `[min, max]` bounds**; (c) an `auto` **downsize happens only after an
   OOM-free window**, and an OOM after one **reverts immediately and freezes
   downsizing for the cooldown**; and (d) with `CAESIUM_RIGHT_SIZING_ENABLED`
   unset the `propose_resources` rule is **not installed** at all
   (`DefaultRuleSet()` unchanged), so no proposal is ever produced. Closed by
   the auth-lane propose → approve → execute scenario plus the auth-off
   refusal, bounds-clamp, and downsize/revert scenarios.
7. **Stream F — the UI** surfaces it: the JobDetail Resources panel (declared vs
   observed + suggestion badge + the proposal deep-link/patch preview), the
   attempt-trail OOM badges, the RunDetail anomaly ribbon, and the stats fleet
   reclaim view render against the live backend, gated on the `RightSizing`
   `Features` flag. Closed by Playwright e2e.
8. **H-1 — every self-server lane** exercises the real resource path: the
   `build/` stress image is built and the feature envs are set on the default,
   distributed, owner-memory, agent-auth, infra (+arm64), arm64 and podman lanes
   in both the `justfile` and `.github/workflows/ci.yml`, so the A/B/C/D/E
   scenarios run against the live binary, not an internal call.
9. **H-2 — Kubernetes is a gate, not a deferral** (arc convention 2 and arc
   acceptance criterion 3, "on Docker, Podman, **and kind**"): the
   `helm-integration-test` lane runs with both flags set via
   `helm/caesium/ci/test-values-k8s.yaml`, loads the stress image into kind, and
   asserts pod-OOM → `resource_failure` + `oom`, requests/limits applied to the
   pod, and either real `metrics.k8s.io` stats or the documented graceful
   degrade — whichever H-2 chose, stated once in the tour.
10. **N-1 — docs reflect reality:** the design-doc `> Status:` banner flipped and
    its three deviations amended (apply surface, kind lane, `MaxAttempts`
    grant), `docs/roadmap.md` (Phase-4 row + the §2.5 "items 1, 2 and 4" note)
    updated, the `resources:` / `rightSizing:` / `onOOM` fields documented across
    `docs/job-definitions.md` and `docs/caesium-job-llm-reference.md` with
    `docs/job-schema-reference.md` **regenerated from `internal/jobdef/report`**,
    working `docs/examples/` manifests with canonically pinned images, this plan
    indexed in `docs/README.md` (backtick form), `docs/tour-compute-loop.md`
    written, and the **arc dashboard row ticked** in the same PR.
11. **Cross-cutting:** `docs/roadmap.md`, `docs/design-resource-right-sizing.md`,
    the arc dashboard, and this plan's per-stream `## Progress` entries reflect
    every shipped stream and match the merged PRs. (The
    `resource_recommendations` cache table remains explicitly deferred — not a
    gate here. The Kubernetes kind lane is **no longer** deferred; see 9.)

## How To Pick Up Work

1. Read [`closed-loop-arc.md`](closed-loop-arc.md) first, then this file
   end-to-end, so you understand the streams, their interdependencies, the
   shared conventions you inherit, and which acceptance criterion the item
   closes. When this plan and the arc disagree on *why* something is in scope or
   on cross-plan ordering, **the arc wins**; on *how* a stream is built, this
   plan and its design doc win.
2. **Re-grep before you edit.** Every citation here is a symbol, not a line
   number, and the `### Verified facts` block above is dated — confirm the fact
   your item stands on still holds.
3. Pick an unchecked item under `## Streams` whose `Depends on:` line is satisfied
   (consult `## Sequencing & Dependencies`), including its **cross-plan**
   dependencies.
4. Branch from `master` (or land in a worktree if dispatched by
   `exec-plan-wave`); do the work as a self-contained PR.
5. Run the verification block under `## Verification (Run For Every PR)`.
6. Tick the checkbox for your item, add a per-stream bullet to the active wave
   subsection in `## Progress` (or open a new wave subsection if none exists yet),
   and update any cross-linked design doc / roadmap section in the same PR.
7. Open the PR with title format
   `<Imperative subject> (resource-right-sizing <wave>-<stream>)` — e.g.
   `Add TaskRun stats columns and honest OOM detection (resource-right-sizing W1-α)`.
   GitHub appends `(#NNN)` on squash-merge. Sweep review-bot comments before
   merge; they post asynchronously and re-review on push.

## Cross-References

- [`closed-loop-arc.md`](closed-loop-arc.md) — the umbrella arc; this is its
  **Plan 2**. Its loop table ("Compute" row), § Synergies (the classifier row,
  the Stream-E recast row, the fan-out row, the `HistorySource` row, the
  explainability row), § Shared conventions 1–8, § Cross-plan sequencing (the
  `internal/incident/` and executor conflict rows), and **arc acceptance
  criterion 3** govern this plan.
- [`trust-the-substrate.md`](trust-the-substrate.md) — **Plan 0.** Its **C4**
  (proposal → `ApprovalRequest` → `awaiting_approval`), **C7** (approve →
  `ExecuteApproved` → `dispatch`, including the direct `apply_jobdef_patch`
  route) and **H-1** (the de-hollowed, widened auth lane) are hard prerequisites
  for Stream E. Its Recon Ledger is the verified bug list this plan does not
  re-litigate.
- [`data-circuit-breaker.md`](data-circuit-breaker.md) — **Plan 1.** Its
  **F3** (in the drafted § "Stream F — Agent & freshness integration", items
  F1–F5) builds the **Git-PR provenance route** of `apply_jobdef_patch` and
  adds `CAESIUM_GIT_WRITE_CREDENTIALS` to `Environment` — the route Stream E's
  git-synced branch needs. Never share a wave with Plan 1 Stream F.
- [`backtesting.md`](backtesting.md) — **Plan 3.** Its **Stream F** attaches
  backtest reports to proposals (including this plan's `propose_resources`
  ones); its **F3 (optional)** resource-override replay reads B3's hash
  exclusion + descriptor resources and owns its own forced re-execution. Never
  share a wave with Stream E.
- [`window-scheduling.md`](window-scheduling.md) — **Plan 4 (optional).** Its
  **Stream B1** predictor adopts the quantile-parameterised `HistorySource`
  **D1 defines** in `internal/rightsizing/` (arc convention 5) and reads A1's
  stats columns. That plan has already been re-cut to say so ("B1 **does not
  define** that interface"; Files: "importing `internal/rightsizing`'s
  `HistorySource` (Plan 2 D1) — no new reader"), so nothing needs flagging —
  just keep D1's interface shape stable, since B1's documented fallback
  reproduces it.
- [`docs/design-resource-right-sizing.md`](../../design-resource-right-sizing.md)
  — the design of record. Source of truth for intent, scope, phasing, and the
  YAML/cache/recommendation contracts. Deviated from in exactly three places —
  Stream E's surface and the kind lane (arc conventions 4 and 2) plus C1's
  dynamic `MaxAttempts` grant (a correctness correction) — each flagged inline
  and amended by N-1.
- [`docs/design-agent-in-the-loop.md`](../../design-agent-in-the-loop.md) — the
  provenance-routed `apply_jobdef_patch` router and the `oom`
  incident-composition seam Stream E routes through, plus the tier semantics
  (`Playbook.decide`) that bound E2's `mode: auto`.
- [`docs/roadmap.md`](../../roadmap.md) — the Phase-4 design-wave table (this
  plan's row) and §2.5 Cost Tracking & Resource Awareness (whose implementation
  items **1, 2 and 4** — `Stats()`, per-task snapshots, and the
  `caesium_task_cpu_seconds_total` / `caesium_task_memory_peak_bytes` metric
  pair — this plan's Phase 0 delivers; its `Status:` line already says so).
  Roadmap wins on priority; **status
  lives only in the arc dashboard**.
- [`pkg/jobdef/definition.go`](../../../pkg/jobdef/definition.go),
  [`pkg/container/spec.go`](../../../pkg/container/spec.go),
  [`internal/cache/hash.go`](../../../internal/cache/hash.go) — the schema, the
  container spec, and the cache identity this plan extends/excludes.
- [`dynamic-fanout.md`](../completed/dynamic-fanout.md) — the shipped
  representation this plan's per-partition stats ride on: **one `TaskRun` row per
  instance**, unique on `(job_run_id, task_id, partition_index)`, children
  inheriting the template step's spec.
- `docs/design-window-scheduling.md`, `docs/design-backtesting.md`,
  `docs/design-freshness-scheduling.md`, `docs/design-dynamic-fanout.md` —
  sibling designs referenced by the Non-Goals (fan-out children inherit
  `resources`; quarantine/backtesting runs are excluded from the window).
- `docs/job-schema-reference.md` (**generated** — update
  `internal/jobdef/report`), `docs/job-definitions.md`,
  `docs/caesium-job-llm-reference.md` — the schema docs N-1 extends with the
  `resources:` / `rightSizing:` fields.
