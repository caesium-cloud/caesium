# Closed-Loop Orchestration — The Arc

Last updated: 2026-09-05

> Status: **Umbrella arc (active).** This document is the program-level source
> of truth for the next several execution plans. It is **not** itself a wave
> target — `exec-plan-wave` runs against the child plans listed under
> [`## Sequence`](#sequence). When a child plan and this doc disagree on *why*
> something is in scope or on cross-plan ordering, **this doc wins**; when they
> disagree on *how* a stream is built, **the child plan (and its design doc)
> wins**.

## Thesis

Caesium already has a **memory of the data plane**: execution descriptors,
receipts, lineage, per-task cache identity, run history, quarantined replay,
`caesium why` / `blame` / `run diff`, and an incident runtime with approval
gates. Those features *observe* and *explain*; the incident runtime *acts*,
but only on run failures and only with its autonomous tier-1/2 actions
(retry, snooze, pause, replay…). Nothing yet **acts on a data, compute, or
time signal**, and the tier-3 "agent proposes, human approves, Caesium
executes" path stops at *proposed* (see Plan 0).

This arc closes the loop:

> *Caesium remembers every run — and uses that memory to act. It holds bad data
> before it spreads, sizes compute to what the data actually needed, picks when
> to run, and proves a fix before it merges. An agent does the diagnosis, a
> human holds the approval, and every automated decision is answerable with
> `caesium why`.*

The four still-unshipped Phase-4 designs (data circuit breaker, resource
right-sizing, backtesting, window scheduling) are not four unrelated features.
They are three **loops over the same memory**, and the shipped agent-in-the-loop
runtime (`internal/incident/`, `internal/mcp/`) is the connective tissue that
makes each loop close durably rather than just react:

| Loop | Observe (memory) | Judge | Act — immediate | Act — durable (agent proposes, human approves) | Prove |
|---|---|---|---|---|---|
| **Data** | `##caesium::metrics` per declared dataset, rolling baselines, lineage impact cone | assertion violated | **hold** the dataset; downstream runs admit straight to `skipped` with reason `dataset_hold:<ns>/<name>`; alert exactly once | `data_quality_hold` incident → agent bundle carries metrics, baseline, `why`, blast radius → agent proposes `release_hold` or a producer patch | backtest the patch over the last N recorded runs before approval |
| **Compute** | peak memory / CPU seconds / OOM per attempt (fan-out partitions included) | OOM, or `p99(peak) × headroom` far from declared `resources:` | **escalate** — retry the attempt at a larger size, clamped and quantised | `oom` incident (today the `oom` class comes only from the log-tail/exit-code tables; Plan 2 A2 makes engines return `atom.ResourceFailure` with OOM evidence and classifies it `oom`) → agent proposes right-sized `resources:` as an `apply_jobdef_patch` proposal through the approval pipeline Plan 0 wires (Git PR for git-synced jobs once Plan 1 F adds that route, diff/apply otherwise) | backtest with a resource override: do the last N runs' peaks fit? |
| **Time** | per-step duration history from the same stats substrate | p95 vs. declared window + deadline | **park** the fire as a durable `run_queue` row; force-start at `deadline − p95 − buffer` | (later) agent proposes window changes when deadlines drift | — |

The **proof** column is what makes this a showcase rather than a demo: every
loop's durable action is a proposal, every proposal can be backtested against
production history, and every automated decision leaves a provenance event that
`caesium why` renders. The loop is explainable, not magic.

## Why this arc (and not one of the four features alone)

- **It completes a story the substrate already half-tells.** Contracts (shipped)
  catch schema breaks at apply time; the circuit breaker catches bad *values* at
  runtime; backtesting catches value changes *before merge*. Fan-out (shipped)
  sizes work horizontally; right-sizing sizes it vertically; window scheduling
  sizes it in time. Agent remediation (shipped) diagnoses failures; this arc
  hands it non-failure signals (holds, OOMs, drift) and a way to prove its
  proposals.
- **It compounds instead of sprawling.** Every plan below extends a table, a
  package, or a surface that already exists: the freshness dataset registry,
  the incident action catalog, the replay core, `run_queue`, the Console's
  lineage/run views.
- **It is the part competitors can't lift in a sprint.** Since
  [`differentiation-strategy.md`](../../differentiation-strategy.md) was
  written, Prefect acquired Dagster Labs (2026-07-13), Airflow 3 shipped
  assets + event-driven scheduling, and every orchestrator grew an MCP server —
  so freshness scheduling and "has an MCP" are now table stakes. What remains
  rare is *replay a candidate against recorded production runs, hold a
  poisoned dataset so downstream skips it, size the next attempt from the last
  one, and prove why* — which only a content-addressed memory makes possible.
  See the 2026-09 status update in the strategy doc.
- **It forces the foundation to be true.** A closed loop over a substrate with
  silent holes is worse than no loop. Plan 0 exists because the recon that
  produced this arc found six still-present holes (Plan 0's Recon Ledger
  L1/L3/L5/L6/L7/L9): the SQL lane strands tolerant-rule consumers of a failed
  plain task; a cancelled run's container keeps running; lineage's
  `producing_step` is always empty; task JSON casing; freshness's consumed
  snapshot is read at completion; and the tier-3 approval flow the completed
  agent-in-the-loop plan ticks as shipped (B4/D1) was never wired — no code
  creates an `ApprovalRequest`, `SetActionExecutor` has no caller, an approved
  action is never executed, and `apply_jobdef_patch` has no dispatch. Plus: the
  auth-enabled lane passes in 0.068s because its test runner never gets
  `CAESIUM_AUTH_MODE`, CI doesn't gate merges, and the "single binary" pitch
  has no binary to download.

## Sequence

Plans run in this order. Each is a normal `exec-plan-wave` target with its
own streams, dashboard, and acceptance criteria. Sizes are anchored on shipped
siblings (`freshness-scheduling` ≈ 4 waves / 11 PRs; `reproduce` ≈ 5 waves).

| # | Plan | Slug | Loop | Size | Status |
|---|---|---|---|---|---|
| 0 | **Trust the substrate** — fix the six ledger bugs, **close the approval loop** (proposal → `ApprovalRequest` → approve → execute, with the direct `apply_jobdef_patch` route), de-hollow and widen the auth-enabled integration lane, make CI gate merges, cut `v0.1.0` with a downloadable CLI, name the shipped verbs in the README, ship `docs/getting-started.md`, delete dead scaffolding, file the unfiled follow-ups | [`trust-the-substrate.md`](trust-the-substrate.md) | foundation | M–L (2–3 waves) | Not started |
| 1 | **The data loop** — data circuit breaker **plus** its previously-deferred Phase 3: `data_quality_hold` incidents, `release_hold` action, held ⇒ not-fresh, `why` provenance | [`data-circuit-breaker.md`](data-circuit-breaker.md) | data | XL (~4 waves) | Not started |
| 2 | **The compute loop** — resource right-sizing with Stream E recast as an incident action, k8s paths exercised in the kind lane, fan-out partition stats, `why` provenance for escalations | [`resource-right-sizing.md`](resource-right-sizing.md) | compute | XL (~4 waves) | Not started |
| 3 | **The proof loop** — backtesting, after a design refresh (`internal/outputdiff` reuse; reconcile with the fan-out-aware replay core), **plus** proposal verification in the approval flow and assertion-threshold backtests | [`backtesting.md`](backtesting.md) | proof | XL (~4–5 waves) | Not started |
| 4 | **The time loop** *(optional tail)* — window scheduling re-cut to P0 over the shared stats substrate; cost/carbon signals parked | [`window-scheduling.md`](window-scheduling.md) | time | L (~2–3 waves) | Not started — optional |
| ✦ | **Tell it** — README rewritten around the loop, strategy doc refreshed, onboarding tour, `v0.2.0` | drafted as `tell-it.md` when Plan 3 enters its final wave | narrative | S–M | Not started |

Plan 2's Stream A (stats + OOM reclassification) may overlap Plan 1's D/E
waves — its files are disjoint from those — but must not share a wave with
Plan 1's Stream A (both edit the two executors) or Plan 1's Stream F (both
edit `internal/incident/`). Plan 4 runs only if
the arc still has momentum after Plan 3; it is explicitly **not** an arc gate.

### Arc dashboard

Updated by each child plan's close-out (the plan's N-1 item ticks its row here).

| Plan | Waves shipped | Last PR | Notes |
|---|---|---|---|
| 0 trust-the-substrate | 0 | — | |
| 1 data-circuit-breaker | 0 | — | |
| 2 resource-right-sizing | 0 | — | |
| 3 backtesting | 0 | — | |
| 4 window-scheduling | 0 | — | optional |
| ✦ tell-it | 0 | — | not yet drafted |

## Synergies pulled into scope

Concrete cross-plan items the original four plans did not exploit. Each is
owned by exactly one child plan (listed) — this table is the map, not a second
checklist.

| Synergy | Owned by | What changes |
|---|---|---|
| `internal/incident/classifier.go` `Classify` maps `atom.StartupFailure, atom.ResourceFailure` → `transient_infra`; the `oom` class is produced only by the log-tail regex and exit-code (137) tables, and **no engine returns `ResourceFailure`** | Plan 2, Stream A | Engines return `atom.ResourceFailure` with OOM evidence (Docker `State.OOMKilled`, Podman `InspectContainerState.OOMKilled`, k8s terminated `Reason == OOMKilled`) and A2 adds an OOM-evidenced → `oom` branch ahead of the `transient_infra` case (a new `OOMKilled` field on `internal/incident/Signal`, populated where the subscriber derives the signal) — the incident class the compute loop hangs off |
| The tier-3 approval flow the completed agent-in-the-loop plan ticks (B4/D1) is not wired: `Executor.Execute` records tier-3 actions as `proposed` and nothing creates the `ApprovalRequest`; `agentsvc.SetActionExecutor` has no caller; `decide()` marks an action approved and nothing runs it; `dispatch` has no case for `apply_jobdef_patch`, `skip_task`, `override_schema_gate`; `internal/incident/provenance.go` does not exist | Plan 0, Stream C (C4 proposal → `ApprovalRequest`; C7 approve → execute + the direct `apply_jobdef_patch` route); Plan 1, Stream F (the Git-PR provenance route) | Every loop's durable action is a proposal through this one pipeline; Plans 2-E and 3-F reuse it and add nothing to it |
| Circuit-breaker Phase 3 was deferred ("draft against Stream C/D once this plan completes") | Plan 1, new Stream F | `data_quality_hold` incident class + `release_hold` action in `internal/incident/`; `get_bundle` / `get_context` carry metrics + baseline + impact cone (`BuildBundle`'s attribution assumes a *failed* `TaskRun`; a hold's producing task succeeded, so the bundle degrades rather than mislabels); held ⇒ not-fresh in `internal/freshness/`; the Git-PR route of `apply_jobdef_patch` (B4's unbuilt half); `EvaluateDataAssertions` factored so a pure `evaluate(spec, sample, baseline)` and an `asOf`-cut baseline exist for Plan 3 F3 |
| Right-sizing Stream E designed a separate `mode: auto` apply path and its own router | Plan 2, Stream E | Recast as an incident **action** (`propose_resources` → an `apply_jobdef_patch` proposal carrying the recommendation) through the Plan 0/Plan 1 pipeline — one approval surface, one apply path |
| Backtesting had no consumer for its report except a human reading a PR | Plan 3, new Stream F | Any `apply_jobdef_patch` proposal (data or compute) can carry a backtest report; the Console approval card shows "backtested over N runs: K changed"; assertion thresholds are backtested against `DatasetMetric` history before enabling (no alert storm at rollout); resource overrides are backtestable (stretch) |
| Fan-out (shipped) children inherit the template step's `resources:` | Plan 2, Streams A/D | Per-partition peak stats feed the recommendation (p99 across partitions); children inherit sized resources |
| Window scheduling specced its own p95 predictor | Plan 2, Stream D defines; Plan 4, Stream B adopts | One quantile-parameterised run-history reader (`HistorySource`: p99 for right-sizing, p95 for windows) over the `TaskRun` timestamps that exist today plus the stats columns Plan 2 A1 adds — one substrate, one query shape |
| Every loop's automated decision must be explainable | every plan, one item each | Hold / skip-with-reason / escalation / right-size proposal / park / force-start / release each emit a persisted event with provenance; the task-scoped `caesium why <run-id> --task <task> --job-id <job-id>` (`cmd/why`, `internal/run/why.go`) and `GET /v1/jobs/:id/runs/:run_id/why` render it in the trigger/attempt blocks — no plan adds a run-level `why` verb unless one item says so |

## Shared conventions (do not restate these in child plans — link here)

These are the rules every child plan inherits. A child plan's item that
contradicts one of these must say so explicitly and why.

1. **Feature gate.** Each loop ships behind one `CAESIUM_<X>_ENABLED` flag
   (default `false`) on the `Environment` struct in `pkg/env/env.go`, surfaced
   on the `Features` struct in `api/rest/service/system/system.go`. The flag
   gates **all three** of: startup wiring in `cmd/start/start.go`, route
   mounting in `api/rest/bind/bind.go` (the conditional-mount precedent is
   `contractsvc.Enabled()` around `/contracts/graph`; the freshness
   `/datasets*` routes are mounted unconditionally — an imperfect precedent,
   do not copy it), and **job-definition validation** of any new trigger type
   or step field (the `freshnessFeatureEnabled()` check inside
   `pkg/jobdef/definition.go` `validateTrigger`'s freshness case). Off means
   inert: no goroutine, no routes, no schema surface.
2. **Harness.** Each plan's H-1 enables its flag on `just integration-up` /
   `just integration-test` **and on every lane that runs its own server**
   — the server recipes are `integration-up`, `integration-up-distributed`,
   `integration-up-owner-memory`, `integration-up-agent`, `integration-up-infra`,
   `integration-test-podman` (starts its server inline), and the helm/kind
   lane whose env lives in `helm/caesium/ci/test-values-k8s.yaml` — lanes that
   start their own server silently go red when a flag is added to only the
   default lane. Auth-gated paths (hold
   release, incident approve/reject, proposals, `auth` CLI) are exercised on
   the **auth-enabled lane** (`just integration-test-agent`; Plan 0 H-1 first
   makes it real — today its test runner never receives `CAESIUM_AUTH_MODE`,
   so both of its scenarios skip and it passes in 0.068s — then widens it to
   `TestAuth*`/`TestIncident*`/`TestScoped*`/`TestHold*` and fails the recipe
   on fewer than N `--- PASS` lines). That lane runs in **local** execution
   mode; anything that needs a re-executing quarantined replay (backtests,
   `quarantine_replay`) runs on the **distributed** lane instead — design
   auth-lane scenarios around cache-served/dry-run paths. Kubernetes runtime
   paths (OOM reason, requests/limits, pod stats) are exercised on the
   **helm/kind lane** — kind-in-CI is the verification bar for k8s work in
   this arc.
3. **Explainability.** Every automated decision is a persisted event
   (`internal/event/bus.go` type, flowing through the notification subscriber)
   carrying enough provenance that the shipped task-scoped explainer names it:
   `caesium why <run-id> --task <task> --job-id <job-id>` (`cmd/why/why.go`
   `renderTable`, `internal/run/why.go` `loadTrigger`/`WhyTrigger`) and
   `GET /v1/jobs/:id/runs/:run_id/why` (`api/rest/controller/why`). Each plan
   has exactly one item for this, and one integration scenario that asserts
   the `why` output (stdout captured via `runCLIStdout`).
4. **Agent actions.** New incident classes extend `internal/incident/classifier.go`;
   new actions extend the catalog in `internal/incident/actions.go` and are
   reachable through the MCP `propose_action` tool as-is (it takes a free-form
   `{type, params}` — no `internal/mcp/tools.go` edit per action). The tier-3 pipeline —
   proposal → `ApprovalRequest` + `awaiting_approval` → human `approve` →
   execute — **is not wired today** (see Synergies); Plan 0 C4/C7 builds it,
   including `skip_task`, `override_schema_gate`, and the *direct*
   `apply_jobdef_patch` route (`jobdefs diff/apply` for jobs without git
   provenance; git-synced jobs degrade to `escalate` with the rendered diff
   until the Git-PR route exists). Plan 1 F adds the Git-PR route (behind
   `CAESIUM_GIT_WRITE_CREDENTIALS`, a new env field). Everything that edits a
   job definition goes through that one router; it is refused under
   `CAESIUM_AUTH_MODE=none`. No plan adds a second apply path or a second
   approval surface.
5. **Stats substrate.** `TaskRun` already records `ExitCode` and start/finish
   timestamps; Plan 2 A1 adds the stats columns (peak memory, CPU seconds,
   stats source, OOM-killed, applied resources, escalation level) and Plan 2
   D1 defines the quantile-parameterised `HistorySource` reader in
   `internal/rightsizing/` (not in `internal/windowsched/`). Right-sizing,
   the window predictor (Plan 4 B1 adopts `HistorySource`), and backtest cost
   plans read that substrate; no plan adds a second stats table or a second
   history query.
6. **Citations.** Plans cite **symbols**, not line numbers (`internal/run/store.go`
   `admit()`, not `store.go:711`). Every one of the four original plans had
   drifted 50–300 lines by the time this arc was drafted. An implementer
   re-greps before editing; a plan that quotes a line number is quoting a hint.
7. **Docs (N- items).** Each plan's N-1 does all of: the `docs/roadmap.md`
   Phase 4 design-table row flip (status lives in **one** place — the arc
   dashboard above; the roadmap Phase 5 table lists plans without a status
   column), design banner flip, schema references
   (`docs/job-schema-reference.md` is **generated** — update
   `internal/jobdef/report`, never the doc by hand), a
   `docs/examples/*.job.yaml` manifest with pinned images, the
   `docs/README.md` index entry (backtick form for subdirectory paths), **and
   a loop tour** (`docs/tour-<loop>.md`: the 10-minute walkthrough a newcomer
   follows to see the loop close end-to-end on their own machine). The arc
   dashboard row above is ticked in the same PR.
8. **Wave hygiene.** One PR = one squashed commit titled
   `<Imperative subject> (<plan-slug> <wave>-<stream>)`. Review-bot comments are
   swept before merge (they post asynchronously and re-review on push). CI is
   green — with required status checks (Plan 0 Stream D) actually gating.

## Cross-plan sequencing & file conflicts

**Order.** 0 → 1 → 2 → 3 → (4) → ✦. Within that:

- Plan 1 Stream C3 (release endpoint 403 under `AUTH_MODE=none`) and Stream F
  (hold incidents, approvals) need Plan 0 Stream C: the de-hollowed, widened
  auth lane (H-1) **and** the wired approval pipeline (C4/C7). Plan 1 F
  builds the Git-PR route of `apply_jobdef_patch` on top of it.
- Plan 1 Stream C2's admission gate relies on trigger-rule correctness; Plan 0
  Stream A fixes the default-mode stranding bug first so "downstream skipped
  because held" is distinguishable from "downstream never dispatched".
- Plan 2 Stream E and Plan 3 Stream F both extend `internal/incident/` and
  depend on the pipeline *existing* (Plan 0 C4/C7 + Plan 1 F), not merely on
  a pattern. Sequential by plan.
- Plan 3 F3 (assertion backtests) needs Plan 1 B1's pure `evaluate(...)` and
  A5's `asOf`-cut baseline helper — Plan 1 ships them in that shape.
- Plan 3 Stream F (F3, assertion-threshold backtests) reads Plan 1's
  `DatasetMetric` rows; Plan 3's resource-override backtest (F4, stretch)
  reads Plan 2's applied resources.
- Plan 4 Stream B (predictor) reads Plan 2 A1's stats columns.

**Files that must not be edited by two plans in the same wave** (additive
appends rebase; these are the true-conflict or order-sensitive sites):

| File | Plans / streams | Rule |
|---|---|---|
| `pkg/jobdef/definition.go` (`Step`/`rawStep`, `Validate()`) | 1-A3, 2-B1, 3-A3, 4-A2 | true conflict — one plan per wave |
| `internal/run/store.go` + `internal/run/fanout.go` | 0-A1, 1-B1/C2, 4-A1 | sequence by plan order |
| `internal/job/job.go` + `internal/worker/runtime_executor.go` | 0-A3/A4 (cancel plumbing), 1-A4, 2-A3/B2/C1 | 2-A may overlap 1's D/E waves, never 1-A |
| `internal/incident/{classifier,subscriber,actions,executor,rules,bundle}.go` + `cmd/start/start.go` incident wiring | 0-C4/C7/C8, 1-F, 2-A2 (classifier `oom` branch, `Signal.OOMKilled`) and 2-E, 3-F | sequential by plan — Plan 2 A2 must not share a wave with Plan 1 F |
| `internal/models/run.go` (`JobRun`, `TaskRun` — hot per-run models) | 1-C2 (`JobRun.SkipReason` + terminal-skipped INSERT: a concurrency skip creates no `JobRun` row today), 2-A1 (stats columns) | one plan per wave |
| `internal/replay/replay.go` | 3-C1, 3-F4 (optional) | Plan 3 re-verifies the post-#374 shape first; C1 and F4 never in the same wave |
| `internal/event/bus.go` + `internal/notification/subscriber.go` | every plan's explainability item (0-C8, 1-F5, 2-C3, 3-F5, 4-B4) | one plan's item per wave |
| `internal/run/why.go` + `cmd/why/why.go` | every plan's explainability item | one plan's item per wave |
| `pkg/env/env.go`, `internal/metrics/metrics.go`, `internal/models/models.go`, `api/rest/bind/bind.go`, `cmd/execute.go`, `cmd/start/start.go` | every plan | additive; one plan's item per wave; `models.All` order matters |
| `ui/src/lib/api.ts`, `ui/src/router.tsx`, `ui/src/components/layout/Sidebar.tsx` | every plan's UI stream | one plan's UI stream per wave |
| `justfile`, `.github/workflows/ci.yml` | every H-1 | one plan's H-1 per wave |
| `docs/roadmap.md`, `docs/README.md`, this file | every N-1 | last item of the plan that ships the change |

## Arc acceptance criteria

The arc is done when **all** of these hold (Plan 4 excepted — see 5):

1. **Plan 0 closed:** the six ledger bugs (L1, L3, L5, L6, L7, L9) have
   integration scenarios that failed before and pass after; a tier-3 proposal
   posted through `/v1/agent/incidents/:id/actions` reaches
   `awaiting_approval`, and `caesium incident approve` **executes** it;
   `just integration-test-agent` runs the auth/incident/scoped-key scenarios
   as visible `--- PASS` lines in CI (not skips) and fails on a hollow run;
   master's branch protection requires the lint, unit-test, ui-e2e, default
   integration and agent-auth jobs; a `v0.1.0` GitHub release exists with
   per-arch `caesium` CLI binaries and the multi-arch images the `publish`
   job builds; `README.md` names every shipped verb and has an install step
   that works without cloning the repo; `docs/getting-started.md` exists.
2. **Plan 1 closed:** a `hold`-mode violation opens exactly one hold and one
   `data_quality_hold` incident; the downstream consumer admits straight to
   `skipped` with reason `dataset_hold:<ns>/<name>`; the agent's bundle carries
   metrics + baseline + impact cone; `release_hold` executes through the
   approval gate on the auth lane; a held dataset reports not-fresh;
   `caesium why <downstream-run>` names the hold. Tour: `docs/tour-data-loop.md`.
3. **Plan 2 closed:** an OOM-killed attempt is recorded as `atom.ResourceFailure`
   on Docker, Podman, **and kind**; the escalation ladder retries at a larger
   size and `caesium why` says so; `caesium job resources` recommends from
   real peaks; an `oom` incident carries a `propose_resources` action whose
   approval opens a Git PR (or applies) through `apply_jobdef_patch`. Tour:
   `docs/tour-compute-loop.md`.
4. **Plan 3 closed:** `caesium backtest create --against last-30-runs --image …`
   reports per-run output deltas using `internal/outputdiff`; an incident
   proposal's approval card shows its backtest verdict; `caesium backtest
   assertions` evaluates a proposed threshold against metric history before
   it is enabled. Tour: `docs/tour-proof-loop.md`.
5. **Plan 4 (optional):** if run, a `trigger: {type: window}` job parks, is
   released by the predictor, and force-starts at the deadline-safe moment
   across a server restart. **Not running Plan 4 does not block the arc.**
6. **Tell it closed:** `README.md` leads with the loop (one recorded
   walkthrough: OOM → incident → PR → backtested → merged),
   `docs/differentiation-strategy.md` is re-scored against its own
   kill-conditions, `docs/roadmap.md` Phase 5 is marked shipped, and `v0.2.0`
   is released.
7. **Cross-cutting:** every automated decision introduced by the arc is
   asserted `why`-explainable by an integration scenario; every new CLI verb
   and REST route has a live-server scenario per the `CLAUDE.md` gate; the arc
   dashboard above matches merged PRs.

## Closing wave — Tell it

Enumerated here so nothing is lost; drafted into `tell-it.md` (canonical
`draft-exec-plan` shape) when Plan 3 enters its final wave.

- README rewrite: lead with the loop story in the first screen (the sovereignty
  pitch stays as the *close*, the loop is the *retain*); extend Plan 0's
  "Beyond scheduling" section with the loop verbs (`backtest`, `dataset
  holds`, `job resources`); one recorded terminal walkthrough (asciinema or
  GIF) of a loop closing. (`docs/getting-started.md` and the `docs/README.md`
  Use/Design split ship in Plan 0 N-2; the tours ship with each loop.)
- `docs/differentiation-strategy.md`: the 2026-09-05 section is an *interim*
  status update; the final re-score of every kill-condition — with the
  `v0.1.0`/`v0.2.0` inbound evidence that condition 1 needs — lands here.
- `docs/roadmap.md`: Phase 5 marked shipped; the Phase-4 table's four rows
  flipped; the Execution Priority table pruned of shipped rows.
- `v0.2.0`: tag, release notes written from the arc dashboard, Helm chart
  `version`/`appVersion` bumped in the same PR.

## Parked, archived, filed

Explicit decisions so they don't rot as prose inside archived plans:

- **Parked (still wanted, not this arc):** window scheduling P1 load gate
  (runs only with arc momentum) and P2 (cost/carbon signal sources, run-history
  window bar, weighted scoring — parked outright); `park` run disposition with
  release-drain (circuit-breaker Phase 3 leftover); per-partition freshness
  watermarks; selective per-task re-run for quarantined replay; a run-level
  `caesium why` verb (today's is task-scoped).
- **Parked designs (banner flipped, file left in place — the
  `TestPlanningAndHistoricalDocsCarryStatusBanner` guardrail pins their paths):**
  `docs/design-sla-management.md` — breach detection already shipped
  (`internal/notification/watcher.go`), freshness SLOs cover the declarative
  half, and the predictive-ETA engine folds into Plan 4's predictor.
  `docs/design-task-templates.md` — DX breadth, outside the arc's thesis and
  the strategy doc's "do not build connector/plugin breadth" line; door left
  open.
- **To be filed as issues by Plan 0 N-3:** the ~20 deferred items recon found
  unfiled across completed plans — event-trigger UI follow-on, node-affinity
  for RWO volumes in distributed mode, local-executor quarantined replay,
  freshness consumed-snapshot timing, blame tiebreak determinism, registry-auth
  + Podman/k8s pre-run digest resolution, `CallbackRun` enrichment, manifest
  export endpoint, React Flow watermark decision, fairness/quotas, and the
  rest enumerated in `trust-the-substrate.md`.
- **Deleted (Plan 0 Stream F):** the `api/gql` placeholder (`place` → `holder`,
  no mutations), the empty `internal/task` and `pkg/client` packages, and any
  other zero-importer stub `go vet`/`staticcheck` confirms.

## How to pick up work

1. Read this file, then the child plan you are working on, end to end.
2. Never run two child plans' items in the same wave if they share a row in
   the file-conflict table above.
3. Follow the child plan's `## How To Pick Up Work`; when it and this doc
   disagree on ordering across plans, this doc wins.
4. When a child plan closes (all its acceptance criteria met), its final N-1
   PR ticks the arc dashboard row, moves the plan to
   `docs/exec-plans/completed/`, and repoints the links in this file.

## Cross-references

- [`docs/differentiation-strategy.md`](../../differentiation-strategy.md) — the
  positioning this arc extends (see its 2026-09 status update).
- [`docs/roadmap.md`](../../roadmap.md) Phase 5 — the roadmap entry for this arc.
- [`trust-the-substrate.md`](trust-the-substrate.md) — Plan 0.
- [`data-circuit-breaker.md`](data-circuit-breaker.md) — Plan 1; design of
  record [`docs/design-data-circuit-breaker.md`](../../design-data-circuit-breaker.md).
- [`resource-right-sizing.md`](resource-right-sizing.md) — Plan 2; design of
  record [`docs/design-resource-right-sizing.md`](../../design-resource-right-sizing.md).
- [`backtesting.md`](backtesting.md) — Plan 3; design of record
  [`docs/design-backtesting.md`](../../design-backtesting.md).
- [`window-scheduling.md`](window-scheduling.md) — Plan 4 (optional); design of
  record [`docs/design-window-scheduling.md`](../../design-window-scheduling.md).
- [`docs/design-agent-in-the-loop.md`](../../design-agent-in-the-loop.md) and
  [`agent-in-the-loop-remediation.md`](../completed/agent-in-the-loop-remediation.md)
  — the shipped incident runtime every loop's durable action routes through.
- [`freshness-scheduling.md`](../completed/freshness-scheduling.md),
  [`dynamic-fanout.md`](../completed/dynamic-fanout.md),
  [`data-plane-memory-ii.md`](../completed/data-plane-memory-ii.md),
  [`reproduce.md`](../completed/reproduce.md) — the shipped substrate the loops
  observe and act on.
