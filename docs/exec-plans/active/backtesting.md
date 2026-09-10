# Pipeline Backtesting — The Proof Loop

Last updated: 2026-09-05

> **Plan 3 of [`closed-loop-arc.md`](closed-loop-arc.md) — the proof loop.**
> Every proposal the arc's loops produce — a data-loop producer patch, a
> compute-loop `resources:` right-size — can be **backtested against recorded
> production runs before a human approves it**, and an assertion threshold can
> be backtested against metric history before it is enabled. The plan was
> re-cut on 2026-09-05 (design refresh + new Stream F); each refreshed item
> carries a `Refresh (2026-09-05)` note. Original 2026-07-03 text and rationale
> are preserved in place.

Caesium can already replay a single historical production run from its immutable
per-task `TaskExecutionDescriptor` with all Caesium-internal side effects
suppressed (quarantined replay, `internal/replay/replay.go`), and attribute *why*
two runs differ (causal run diff, `internal/run/rundiff.go` `Store.DiffRuns`).
What it cannot do is answer the question data teams ask before every transform
merge: **does my change alter the numbers?** Pipeline changes ship on faith today
— CI proves the YAML lints and maybe that the job runs once against staging
data, and the first real test is tonight's production run, where the regression
is discovered by the *consumer* and reconstructed after the fact.

This plan ships **backtesting**: a pre-merge verb that replays a candidate change
over the last N production runs' recorded inputs and reports output deltas per
run ("your change alters output for 2 of 30 days; here is the diff"). It is the
composition of shipped primitives — quarantined replay, execution descriptors,
receipts, causal run diff, and the `internal/outputdiff` comparator shipped by
`reproduce` C1 (#339) — plus one significant piece of new machinery (controlled
**descriptor overrides**, so a replay can execute a candidate image / command
that did *not* run at baseline) with its own safety analysis. A backtest
aggregates N quarantined replays (one per selected baseline production run),
each executed with the candidate override applied to the reconstructed
descriptors, plus a per-run output-delta computation against the baseline's
recorded outputs, surfaced as a verdict matrix in the CLI, the REST API, the
Console, a PR comment — and, new in this cut, on the **incident approval card**
the arc's agent-in-the-loop runtime already renders.

The work follows the design's phasing. **P0** (Stream A) is the same-code
backtest — N quarantined replays with an *empty* override set plus the
output-delta/report plumbing — which carries exactly the shipped replay risk and
already catches environment drift (moved tags, rotated secrets, changed source
data). **P1** (Stream C, the headline) adds descriptor overrides — a candidate
image/command/schema — with the new-risk safety gates, because a backtest
executes **unvetted candidate code** against the baseline's real mounts, secrets,
network, and workload identity, and the `replaySafe` attestation does not
transfer. **P2** (the CI Action step) lives in the external `caesium-action`
repo and is recorded deferred; its caesium-side enablers (`--format markdown`,
`--dry-run`) ship in Stream D. **Stream F** (new) is the arc synergy: proposal
verification in the approval flow, assertion-threshold backtests over
`DatasetMetric` history, and (stretch) resource-override backtests. Every new
CLI verb and REST endpoint ships with a `test/` integration test that drives the
real surface against a live distributed server (per the `CLAUDE.md`
end-to-end-coverage gate).

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

**Arc rule.** This plan is Plan 3 of
[`closed-loop-arc.md`](closed-loop-arc.md). When this plan and the arc disagree
on *why* something is in scope or on cross-plan ordering, **the arc wins**; when
they disagree on *how* a stream is built, this plan (and its design doc) wins.
This plan inherits the arc's **shared conventions 1–8** by link — feature gate
(1), harness lanes (2), explainability (3), agent actions (4), stats substrate
(5), symbol citations (6), docs/N-items (7), wave hygiene (8) — and does not
restate them; an item that contradicts one says so explicitly.

This plan implements [`docs/design-backtesting.md`](../../design-backtesting.md).
**The design doc is authoritative for INTENT and SCOPE and wins on any
disagreement** with this plan — if an item here contradicts the design, the
design's contract holds and the item is corrected, not the design. No item may
add a new verb, endpoint, config knob, or job-schema field beyond what the design
enumerates without first amending the design. The design in turn defers its
reused **safety invariants** to
[`docs/design-quarantined-replay.md`](../../design-quarantined-replay.md)
(authoritative for every quarantine/`replaySafe`/suppression invariant this plan
inherits verbatim) and the descriptor substrate to
[`docs/design-data-plane-memory.md`](../../design-data-plane-memory.md). The
job-definition contract for the new `metadata.backtest` block lives in
[`pkg/jobdef/definition.go`](../../../pkg/jobdef/definition.go) — if an item finds
it needs a struct change beyond the design's `ignoreOutputs` / `backtestMode`
fields, stop and reconcile against the design first. Strategic priority/status is
tracked in [`docs/roadmap.md`](../../roadmap.md) (Phase 5 sequence table row 3
and the Phase-4 exploration-table "Pipeline backtesting" row; the roadmap wins on
priority/status disagreements).

**Refresh (2026-09-05).** Two things the design already says that the
2026-07-03 cut of this plan missed: the design's Related Documents names
`internal/outputdiff` as the output comparator backtesting **must reuse** (A3 is
corrected below — no second comparator); and `internal/replay/replay.go` has
since become fan-out-aware (#374 "Replay fanned baselines from recorded
partition lists"), which C1 must reconcile with. Stream F's items (six after the
2026-09-05 adversarial review split F1 and the old F4 into single-purpose items)
are the
arc's synergy (arc table row "Backtesting had no consumer for its report except a
human reading a PR"); the design doc's `design-agent-in-the-loop.md` cross-link
("the agent can attach a backtest report as evidence when proposing a jobdef
patch") is their intent statement.

## Progress (as of 2026-09-05)

No implementation waves have shipped yet. The plan was published on 2026-07-03
from the [`design-backtesting.md`](../../design-backtesting.md) brainstorm/design
proposal and **re-cut on 2026-09-05** as Plan 3 of the closed-loop arc (design
refresh, symbol citations, Stream F, explainability item, lane-aware harness).
The first wave is the next eligible run of the `exec-plan-wave` skill against
this doc, **after Plan 0 (`trust-the-substrate.md`) closes** per the arc
sequence. The first-wave-eligible leaf items are **A1** (the backtest data model
+ store + config), **H-1** (the integration-server enablement) and **N-2** (the
design amendment D1/F1/F3/F4 are blocked on) — none has an unmet in-plan
dependency. Stream F additionally waits on **Plan 0 C4/C7** and **Plan 1
Stream F**, not merely on Plan 1 (corrected 2026-09-05).

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Backtest runtime engine (P0) — `Backtest`/`BacktestRun` models + store, baseline selection + eligibility, output-delta via `internal/outputdiff` + verdict roll-up, orchestration + metrics + startup wiring, `metadata.backtest` schema | **P0** | Not started |
| B | Backtest REST API — `POST /v1/jobs/:id/backtest` (idempotent, dry-run), the report + list reads, authorization capability | **P0** | Not started |
| C | Descriptor overrides (P1 headline) — typed `StepOverride`, honest override→hash plumbing reconciled with the fan-out-aware planner, candidate-digest resolution, `DescriptorOverrides` column, `--path` delta + capability split | P1 | Not started |
| D | Backtest CLI — `caesium backtest` create/report, poll-to-terminal, `--json`/`--format markdown`/`--dry-run`, override flags | P1 | Not started |
| E | Console UI — backtest report view, run-matrix heat strip, Backtests tab, RunDiffView drill-down, features gate | P2 | Not started |
| F | Proposal verification & assertion backtests (arc synergy) — `backtest_patch` incident action (F1); `apply_jobdef_patch` proposals carry a backtest verdict on the approval card / `incident get` / MCP (F2); assertion-threshold backtest over `DatasetMetric` history, no re-execution (F3); resource-override backtest (F4, stretch); `why` provenance (F5) + timeline chain (F6) | P1 (F4 optional) | Not started — F1/F2 blocked on **Plan 0 C4/C7** (the approval pipeline) + Plan 1 F; F3 blocked on Plan 1 substrate |
| H-1 | Integration harness — `CAESIUM_BACKTEST_ENABLED` + override capability on the distributed lane, the agent/auth lane and every other self-server lane (**deliberately NOT the default `integration-up` lane**, which hosts the gated-off 404 scenario); approval-card scenarios on the auth lane | — | Not started |
| N-1 | Docs — roadmap flip, design banner, `metadata.backtest` + `backtest` verb schema docs (generator), example manifest, README repoint, `docs/tour-proof-loop.md`, arc dashboard row | — | Not started |
| N-2 | **Design amendment (runs FIRST, before D1/F1–F4)** — land the surface this plan adds beyond the 2026-07 design (`backtest create` subcommand, `mode: assertions`, `backtest assertions`, the `backtest_patch` remediation action, `--resources`) into `docs/design-backtesting.md` so the Source-Of-Truth rule holds | — | Not started |
| (CI Action) | §2.1 Action fourth step `lint→diff→backtest→comment` + dry-run-then-label cost guard | — | **Deferred** — external `caesium-action` repo |

## Streams

### Stream A — Backtest runtime engine (P0 core)

The reactive substrate every other stream builds on: the persistent `Backtest`
and `BacktestRun` models, the store, baseline selection + eligibility (reusing
replay's fail-closed `Prepare`), the output-delta/verdict computation, and the
orchestration engine that feeds N quarantined replays into the **existing**
dispatch machinery. This is the P0 same-code backtest — an empty override set,
carrying exactly the shipped replay risk N times — and it is independently useful
("is my pipeline deterministic over its recorded inputs?") because it already
catches environment drift. Largest blast radius, so it merges first. Mirror the
shipped replay service (`internal/replay/`, `api/rest/service/replay/`) and the
job-scoped async-operation shape of backfill (`internal/backfill/store.go`,
`api/rest/controller/backfill/`).

**Refresh (2026-09-05) — distributed mode.** Every replay that has a task to
re-execute requires distributed execution mode: `api/rest/service/replay/replay.go`
`Service.Replay` refuses with `ErrReplayRequiresDistributedMode` when
`PreparedReplay.RequiresDispatch()` is true and `Service.isDistributedExecutionMode()`
is false (and `Service.resumePending` re-checks it on resume). A backtest is N
such replays, so **every backtest scenario that re-executes anything runs on the
distributed lane** — `just integration-test-distributed` (CI job
`build-and-integration-test-distributed` in `.github/workflows/ci.yml`, server
from `just integration-up-distributed` with `CAESIUM_EXECUTION_MODE=distributed`).
Only a fully cache-served P0 backtest (`RequiresDispatch()` false for every
baseline) or a `--dry-run` completes on the default local-mode lane. The A-level
engine must surface the refusal per baseline, not per backtest (see A4).

- [ ] A1. Add the `Backtest` and `BacktestRun` GORM models + a typed store + the
      config. `Backtest` = `ID`, `JobID`, `Status`, `Overrides` + `IgnorePaths`
      JSON, unique nullable `Fingerprint`, requested/eligible/changed/unchanged/
      failed counters; `BacktestRun` = `BacktestID`, `BaselineRunID`, nullable
      `ReplayRunID`, `Verdict` (unchanged/changed/failed/skipped/degraded),
      `SkipReason`, `OutputDelta` JSON, re-executed/cached counts. Register **both**
      in the `All` slice (parents before FK children — `Backtest` before
      `BacktestRun`). These are **catalog/observability tables, NOT hot per-run
      tables**, so do NOT add them to `hotPathModels()` / the `hotTables` router
      map. The store's create is **idempotent like replay creation**:
      `Idempotency-Key`-scoped fingerprint (job + baseline set + overrides +
      principal + key), insert-before-dispatch, resume-on-duplicate; each child
      `BacktestRun`'s replay fingerprint derives from the backtest fingerprint +
      baseline run ID so a crashed backtest resumes without double-executing any
      baseline. Add `CAESIUM_BACKTEST_ENABLED` (default `false`) and
      `CAESIUM_BACKTEST_MAX_PARALLEL_REPLAYS` (default `2`) to the `Environment`
      struct. Report retention is open question 6 — keep rows forever (like runs)
      in v1; no pruner.
      **Refresh (2026-09-05):** mirror the replay fingerprint derivation
      (`api/rest/service/replay/replay.go` `Fingerprint` — versioned payload,
      principal identity via `principalIdentity`, sorted overrides) rather than
      inventing a second scheme, and add the arc's feature-gate wiring
      (convention 1): the flag also gates route mounting in `api/rest/bind/bind.go`
      (B1) and the `metadata.backtest` schema surface (A3) the way
      `pkg/jobdef/definition.go` `validateTrigger` gates `TriggerFreshness` behind
      `freshnessFeatureEnabled()`. Also add a nullable, indexed `BacktestID` column
      to `JobRun` (`internal/models/run.go`) so a quarantined replay materialized
      by a backtest is attributable to it — E1's "run-detail page links back to its
      owning backtest" and F5's `why` provenance both read it. (`JobRun` is already
      registered; this is an additive column, not a `models.go` edit.)
      Files: new `internal/models/backtest.go`, `internal/models/models.go`,
      `internal/models/run.go` (`JobRun.BacktestID`), new
      `internal/backtest/store.go` (+ `store_test.go`), `pkg/env/env.go`.
- [ ] A2. Implement baseline selection + per-baseline eligibility. Resolve
      `--against last-30-runs` server-side to the job's most recent
      **succeeded**, **non-quarantined** production runs (`quarantine IS NOT TRUE`,
      the predicate the replay work added to baseline-selecting queries). **Default
      to succeeded baselines, not merely terminal ones:** a failed baseline has no
      trustworthy recorded output, so diffing a succeeded candidate against it yields
      meaningless `OUTPUT_CHANGED`/`DEGRADED` verdicts and noise. An explicit
      `--include-failed-baselines` opt-in may widen the set for the "does my fix make
      the failing run pass?" question, but then each such baseline is labeled
      `baseline-failed` in the report so its delta is read as expected, not a
      regression. Support the date-range and explicit run-ID-list alternatives
      (which take the runs as given, still labeling any non-succeeded ones). Check each selected baseline for
      eligibility by **reusing replay's fail-closed validation**
      (`internal/replay/replay.go` `Constructor.Prepare`): every task run carries an
      `ExecutionDescriptor` at a supported schema version; tasks that would
      re-execute were recorded `replay_safe = true` **at baseline**
      (`TaskRun.ReplaySafe`, read from the baseline row, never the live definition);
      secret identities re-verify (env provider fails closed); unchanged tasks have
      live cache proof. An ineligible baseline is **reported and skipped with a
      per-run reason** ("pre-dates replaySafe", "cache proof expired (job TTL
      168h)"), never silently dropped; zero eligible baselines fails loudly.
      **Refresh (2026-09-05):** map each replay sentinel to a stable skip reason —
      `ErrReplayUnsafe`, `ErrUnavailableBaselineProof` (the cache-expired case is
      raised by `Constructor.cacheSourceForUnchanged` when the task is
      `Cache.Enabled` and `cache.Store.Get` finds no entry), `ErrSecretIdentity`,
      `ErrMissingDescriptor` / `ErrUnsupportedDescriptor`, `ErrQuarantinedBaseline`,
      `ErrBaselineNotTerminal`, and **`ErrFannedBaseline`** — which since #374 no
      longer means "the baseline is fanned" but "the fan-out group cannot be
      re-expanded from the recorded partition list" (a baseline recorded before
      descriptors captured the list, a list that disagrees with the materialized
      instances, or an invalid group — see `groupBaselineTasks` and
      `adoptRecordedPartitions`). A fanned baseline with a recorded list is
      **eligible**, and its `TaskDecision`s come back one per instance
      (`TaskDecision.Partition` non-empty, shared `TaskID`) — A3 aggregates them
      per catalog task.
      Files: new `internal/backtest/selection.go` (+ `selection_test.go`); reads
      `internal/replay/replay.go` `Constructor.Prepare` (no edit).
      Depends on: A1.
- [ ] A3. Implement the output-delta computation, verdict classification, and the
      `metadata.backtest` job-schema block. Per task, compare baseline
      `TaskRun.Output` (typed JSON key→value map, ≤`MaxOutputBytes` per
      `pkg/task/output.go` `MaxOutputBytes`) against the replay task's `Output`; for large-object
      reference outputs compare the carried **content digests** (byte-identical
      large outputs compare equal without moving data); a run-level roll-up digest
      over sorted terminal-task outputs gives a single per-run "unchanged"
      attestation in the spirit of `internal/receipt`. Verdicts per task:
      `OUTPUT_UNCHANGED`; `OUTPUT_CHANGED` (per-key before/after `FieldChange`s,
      reusing the `internal/run/whydiff.go` shape); `FAILED` (candidate errored
      where baseline succeeded — always a reported regression); `NOT_COMPARED`
      (cache-hit); `DEGRADED` (output missing on one side). Add the ignore-paths
      mechanism — **glob on `step.key` only, no regex-on-values** — sourced from a
      new job-level `metadata.backtest.ignoreOutputs: [...]` block plus a CLI
      override; ignored keys are excluded from the delta **and listed in the report
      as ignored**. Add the `metadata.backtest` block (`ignoreOutputs`, and the
      `backtestMode: readOnly` **attestation** — recorded and displayed, explicitly
      NOT enforced) to `pkg/jobdef/definition.go` + `pkg/jobdef/schema.go` with
      validation. **No `internal/cache/hash.go` change**: `metadata.backtest` is
      comparison/attestation metadata that does not participate in step execution
      identity, so the cache key is untouched (unlike a step field, which would
      require hashing).
      **Refresh (2026-09-05) — reuse `internal/outputdiff`, no second comparator.**
      The per-task comparison is `outputdiff.Compare(baseline.Output, replay.Output)`
      → `outputdiff.Diff{Added, Removed, Changed}` with `Diff.Empty()` and the
      deterministic `Diff.Render()` (shipped by `reproduce` C1, #339, and named by
      the design's Related Documents as the comparator backtesting must reuse).
      Large-object references **do** need a normalisation step — the 2026-07 claim
      that "`Compare` already compares digests" is **wrong and is corrected here**.
      `pkg/task` `OutputRef.Encode` marshals *all four* fields
      (`caesiumOutputRef`, `path`, `digest`, `size`) into the map value, and
      `outputdiff.Compare` compares raw `map[string]string` values for string
      inequality — so a payload moved to a new path with byte-identical content
      (same `Digest`) diffs as `Changed`, contradicting this item's own
      "byte-identical large outputs compare equal without moving data" goal. (The
      digest-only equality documented on `OutputRef` is the *cache* hash rule, not
      `Compare`.) Therefore `internal/backtest/delta.go` **normalises reference
      values on both sides before calling `Compare`**: for every value where
      `pkg/task` `IsOutputRef` holds, decode with `pkg/task` `DecodeOutputRef` and
      substitute the canonical form `sha256:…` (the `Digest` alone), dropping
      `path` and `size`; a value that fails to decode is compared verbatim.
      Display decodes the original `OutputRef` again to show path/size beside the
      digest. This normalisation is `delta.go`-owned work alongside verdicts,
      aggregation and ignore-globbing — do **not** change `internal/outputdiff`,
      which `reproduce` also uses. The `FieldChange`/`whydiff` sentence above is **superseded**:
      `BacktestRun.OutputDelta` embeds `outputdiff.Diff` verbatim (its `Change`
      shape is `{key, recorded, reproduced}`), so the CLI, the PR comment, and the
      Console render one diff shape across `reproduce --diff` and `backtest`.
      `ignoreOutputs` is applied by filtering both maps **before** `Compare`
      (recording the ignored keys beside the diff). What backtest **adds on top**
      of `outputdiff` — and what `internal/backtest/delta.go` owns — is only:
      (1) verdict classification of one `Diff` + the two task statuses into the
      five verdicts above (`Empty()` → `OUTPUT_UNCHANGED`; non-empty →
      `OUTPUT_CHANGED`; replay row not succeeded where baseline succeeded →
      `FAILED`; replay row `CacheHit` → `NOT_COMPARED`; one side missing →
      `DEGRADED`); (2) **fan-out aggregation** — a fanned group's N instance rows
      (matched baseline↔replay by `PartitionValue`) roll up to one per-task verdict
      (worst-of, with per-partition diffs kept in `OutputDelta`), and a partition
      present on one side only is `DEGRADED`; (3) the per-run roll-up (worst-of
      over tasks + the receipt-style digest over sorted terminal outputs) and the
      per-backtest counters (changed/unchanged/failed/skipped) across N runs.
      Files: new `internal/backtest/delta.go` (+ `delta_test.go`) — verdicts,
      aggregation, ignore globbing, and large-object reference normalisation only;
      imports `internal/outputdiff` and `pkg/task`;
      `pkg/jobdef/definition.go`, `pkg/jobdef/schema.go`.
      Depends on: A1.
- [ ] A4. Add the orchestration engine + metrics + startup wiring. Aggregate N
      quarantined replays feeding the **existing** dispatch machinery, gated by
      `CAESIUM_BACKTEST_ENABLED`, capped by `CAESIUM_BACKTEST_MAX_PARALLEL_REPLAYS`,
      sequenced **oldest-first** so partial results are meaningful and the cap keeps
      quarantined work from starving production claims. Re-executing replay requires
      distributed mode (`ErrReplayRequiresDistributedMode`,
      `api/rest/service/replay/replay.go`) — backtest inherits that for any run not
      fully cache-served. Implement `--dry-run`: compute and return the **full plan
      + cost split** (re-executed vs cache-hit task counts) **without dispatching
      anything**, so the PR Action can post cost before approval. Persist per-run
      verdicts via A3's delta into the `BacktestRun` rows and roll up the backtest
      counters. Wire the engine as an env-gated subsystem in `cmd/start/start.go`.
      Add `caesium_backtest_runs_total{verdict}`,
      `caesium_backtest_tasks_reexecuted_total`, and a parallel-replays gauge to
      `internal/metrics/metrics.go` (both the `var (...)` block AND the
      `Register()` list — two edit sites, same file), asserted via
      `internal/metrics/testutil`.
      **Refresh (2026-09-05):** drive each baseline through
      `Constructor.Prepare` → (distributed-mode check) → `Constructor.Materialize`
      with a `replay.Dispatcher` (the REST service's `AsyncDispatcher` in
      `api/rest/service/replay/replay.go` is the production one); the dry-run plan
      is the `TaskDecision` list `Prepare` returns (`Reexecute`/`CacheHit` per row,
      per partition for fanned groups) summed per baseline. A baseline that
      `RequiresDispatch()` on a local-mode server is recorded `skipped` with reason
      `requires distributed execution mode` — per baseline, so a mixed set still
      reports the cache-served ones. Materialization stamps `JobRun.BacktestID`
      (A1) on every replay run it creates; the replay run's existing
      `TriggerType`/`TriggerAlias` (`"replay"`/`"quarantined-replay"`, set in
      `Constructor.materialize`) are left as-is so shipped `why`/list filters keep
      working. Completion of a replay run is observed through the event bus
      (`event.TypeRunCompleted`/`TypeRunFailed` with `Quarantine: true` — subscribe
      with `event.Filter{IncludeQuarantine: true}`), not by polling.
      Files: new `internal/backtest/engine.go` (+ `engine_test.go`),
      `cmd/start/start.go`, `internal/metrics/metrics.go`.
      Depends on: A2 + A3.

### Stream B — Backtest REST API + observability

The HTTP surface over the engine: the create endpoint that drives the
orchestration, and the report/list reads. New capability, new endpoint, new
authorization surface — **do NOT extend `POST …/runs/:run_id/replay`**: its
contract is "params-only, identical code", its body is deliberately closed
(`DisallowUnknownFields` in `api/rest/controller/replay/replay.go`'s request
decoder, with the `maxReplayRequestBodyBytes` / `maxReplaySetEntries` /
`maxReplaySetKeyBytes` caps), and
widening it would smuggle code overrides under the old attestation. Mirror the
replay controller/service split (`api/rest/controller/replay/`,
`api/rest/service/replay/`).

- [ ] B1. Add `POST /v1/jobs/:id/backtest` — body is the baseline selector,
      overrides, ignore paths, and `dryRun`; `Idempotency-Key` **required**; returns
      `202` + backtest ID. `DisallowUnknownFields` and bounded override sizes
      (mirror the replay controller's caps). Call the Stream A engine
      (dry-run short-circuits to the plan; otherwise insert-before-dispatch). Add
      the **authorization capability** seam: a params-only / no-override backtest
      needs the same privilege as replay, but a request carrying **code overrides**
      requires a higher-privilege capability and is refused without it (the actual
      code-override key wiring lands in C2; B1 establishes the check point and
      refuses overrides it is not yet configured to accept). Add
      `caesium_backtest_created_total`.
      **Refresh (2026-09-05):** mount the routes only when
      `CAESIUM_BACKTEST_ENABLED` is set (arc convention 1 — off means no routes);
      an unmounted route 404s, which H-1's "gated off by default" scenario asserts
      the way `test/incident_gating_test.go` `TestIncidentRoutesGatedOffByDefault`
      does for incidents — and, exactly like that precedent, **on the default
      `just integration-test` lane, whose server deliberately does NOT set
      `CAESIUM_BACKTEST_ENABLED`** (see H-1). Every functional backtest scenario
      lives on a lane that does set it.
      Files: new `api/rest/controller/backtest/backtest.go`, new
      `api/rest/service/backtest/backtest.go`, `api/rest/bind/bind.go`,
      `internal/metrics/metrics.go`.
      Depends on: A4.
- [ ] B2. Add the observability reads: `GET /v1/jobs/:id/backtests/:btid` (the
      report — verdict matrix + per-run deltas + cost split, read from the durable
      `BacktestRun` rows, **not** a re-evaluation) and `GET /v1/jobs/:id/backtests`
      (list, bounded + paginated). Per-run drill-down **reuses the shipped**
      `GET /v1/jobs/:id/runs/diff?left=<baseline>&right=<replay>` endpoint
      unchanged — do not fork it.
      Files: `api/rest/controller/backtest/`, `api/rest/service/backtest/`,
      `api/rest/bind/bind.go`.
      Depends on: A1 + B1.

### Stream C — Descriptor overrides + candidate execution (P1 headline)

The core new machinery: today the replay `Request` is
`{BaselineRunID, Set, ReplayFingerprint}` (`internal/replay/replay.go` `Request`) —
**params are the only overridable input**; image/command/env/schema all come
pinned from the descriptor (`computeDescriptorInstanceHash` — the fan-out-aware
successor of `computeDescriptorHash`, which now delegates to it — reads
`desc.Runtime.Image` / `ResolvedImageDigest` / `Command` / `CommandRaw`), and
that pinning is the identical-code
guarantee replay sells. Backtest extends the request with a typed per-step
override set so a replay can execute a candidate that did **not** run at baseline.
This heavily edits `internal/replay/replay.go` (a true-conflict file — the arc's
conflict table reserves it for **3-C1 only** and requires re-verifying the
post-#374 shape first), so it is a
single stream and sequences after the engine that drives it.

**Refresh (2026-09-05) — the fan-out-aware planner, and what an override may
target.** Since #374, `Constructor.loadBaseline` collapses baseline rows into one
`baselineGroup` per catalog task (`groupBaselineTasks`), re-expands every fanned
group from the partition list **recorded on its producer's descriptor**
(`TaskExecutionFanOut.Partitions`, adopted by `adoptRecordedPartitions`, ordered
by `inGroupPlanOrder`), and `planTasks` hashes each instance with its partition
folded in (`computeDescriptorInstanceHash`), presenting one aggregate output and
one aggregate identity downstream (`plannedGroupOutput`, `plannedGroupHash`).
Three consequences fix v1's override semantics — decided from the code, not the
design:

1. **An override keys on the step name** (`StepOverride.StepName` ==
   `desc.Baseline.TaskName`, the same for every instance of a fanned group) and
   **applies to every instance of a fanned template step**: replacing
   `desc.Runtime.Image`/`Command` on each instance's descriptor before
   `computeDescriptorInstanceHash` changes all N hashes, all N re-execute,
   in-group dependents cascade through `pendingSiblings`, and the fan-in
   consumer re-executes through the group-level `pendingPredecessors`. This is
   the honest cost of "the job's `image:` for this step changed" and is what
   a `--path candidate.job.yaml` delta can express (a jobdef has one `image:`
   per step).
2. **Overriding a single fanned instance (a partition-scoped override) is
   refused in v1** with a typed error: no jobdef field maps to it (so it can
   never come from `--path`), the group still presents one aggregate identity so
   downstream re-executes anyway (no cost saving), and `TaskDecision`s would
   report a group whose instances ran different code. The override struct
   carries no partition field; the controller rejects a `step=partition` form.
3. **Overriding a fan-out PRODUCER step is refused in v1** with a typed error
   (`ErrOverrideOnFanOutProducer`, wrapping `ErrFannedBaseline`'s intent). The
   replay pre-materializes the fanned group from the **recorded** list at
   `Constructor.materialize` → `taskRunRecord`, and when a re-executed replay
   producer completes, `internal/run/fanout.go`'s expansion path **skips**
   re-expansion on `ErrAmbiguousTaskRun` ("replay must re-expand from the
   RECORDED list, never from a re-executed producer"). A candidate producer that
   emits a different partition list would therefore run against the baseline's
   group and the report would be silently wrong. Lifting this (compare the
   candidate's emitted list with the recorded one after the fact and mark the
   run `DEGRADED` on mismatch) is a v2 item — recorded under
   `#### Deferred — fan-out producer overrides` below.

- [ ] C1. Add the typed `StepOverride` set to the replay request and the honest
      hash plumbing. Extend the replay `Request` / `PreparedReplay` with a
      per-step override (`StepName` matching a descriptor `Baseline.TaskName`,
      `Image`, optional `Command`, optional schema fields). The override **replaces
      the descriptor value BEFORE `computeDescriptorHash`** so the step's identity
      changes honestly, it re-executes, and downstream re-executes via the
      `PredecessorHashes` cascade — **never hide an override from `HashInput`** (the
      false-hit bug class the replay design forbids). The stored descriptor keeps
      baseline values; the replay `TaskRun` row carries the candidate
      `Image`/`ResolvedImageDigest`/`Command`; add a `DescriptorOverrides` JSON
      column to the quarantined `JobRun` recording the delta (so `caesium why` /
      `run diff` attribute the re-run to the `image` field for free). Digest-resolve
      the candidate tag to `sha256:…` **up front, once, at backtest-create time**
      via `internal/imagecheck/resolve.go` — the resolved `sha256:…` is stored
      on the `Backtest` row and it is the **digest, never the tag**, that is
      written into every replay's candidate descriptor, so all N replays run
      one image even if the tag moves mid-backtest; a candidate that cannot be
      digest-resolved is **refused, not degraded, on every engine** (see the
      engine rule in the Refresh below).
      **Refresh (2026-09-05):** apply the override inside `planTasks` per group
      (it is the group's `descriptor()` that names the step) to each instance's
      `task.descriptor` copy before `computeDescriptorInstanceHash`, so a fanned
      template step re-hashes every partition (rule 1 above); refuse a
      partition-scoped override (rule 2) and an override whose target group has
      `producerFanOut != nil` (rule 3) at `Prepare` time so the refusal is a
      per-baseline eligibility reason, not a materialized failure.
      **Name the candidate field explicitly** — the 2026-07 wording ("keep the
      planned `descriptor` as the baseline descriptor and write the candidate onto
      the row in `taskRunRecord`") is not achievable as written, because
      `taskRunRecord` derives `Image`, `Command` (via `encodeCommand`) and
      `ResolvedImageDigest` *from* `plan.descriptor` (`desc := plan.descriptor`),
      and `planTasks` sets `descriptor: task.descriptor` with the comment "The
      replay TaskRun stores the baseline descriptor unchanged". So: add
      `plannedTask.candidate *models.TaskExecutionDescriptor` (nil when the step
      has no override). `taskRunRecord` reads `Image`, `Command` and
      `ResolvedImageDigest` from `candidate` when it is non-nil and from
      `plan.descriptor` otherwise, while `ExecutionDescriptor` stays
      `plan.descriptor` (the baseline audit reference) — with the candidate delta
      recorded in the new `JobRun.DescriptorOverrides` column beside the existing
      `ReplayOverrides` (params) column. **Hash side:** the override is applied to
      a *copy* of the instance descriptor (that same `candidate`) which is what
      `computeDescriptorInstanceHash` consumes, so the identity changes honestly
      while `task.descriptor` remains the untouched baseline. Resolve digests with
      `imagecheck.Default().Resolve(ctx, engine, ref, 0)` (TTL 0 forces a fresh
      registry round-trip; a `@sha256:` reference is trusted verbatim by
      `digestFromReference`); `imagecheck.ErrDigestUnavailable` is the refusal.
      **Engine coverage (revised 2026-09-09, #405 shipped):**
      `imagecheck.NewResolver` now wires a digest backend for **every**
      engine: docker resolves via the local daemon, then the registry, then an
      authenticated pull; podman and kubernetes resolve through the
      engine-independent registry client (`imagecheck.RegistryClient`, a
      manifest `HEAD` with bearer/basic auth from `CAESIUM_REGISTRY_AUTH` →
      `secret://` providers). `Resolve` still returns `ErrDigestUnavailable`
      + `cacheNegative` when the registry is unreachable, rejects the
      configured credentials, or the tag does not exist — those are the
      *genuinely unresolvable* references. **v1 rule (revised 2026-09-06 after
      review; scope narrowed 2026-09-09):** a candidate whose digest cannot be
      established is **refused on every engine** with a typed
      `ErrCandidateUnpinned` whose message names the accepted form
      (`image@sha256:…`) and the CLI flag that produces it — accepting a
      mutable tag on podman/kubernetes would let each replay task resolve the
      image independently at container-create time, so a tag that moves
      during an N-run backtest (or workers holding different cached versions)
      would combine results from different images under one candidate
      verdict, which is exactly the identity guarantee the design requires.
      With #405 the refusal fires only for references the server itself cannot
      resolve (no `ErrDigestUnavailable` is engine-dependent any more), so a
      plain tag on a reachable registry — public or private with
      `CAESIUM_REGISTRY_AUTH` — is pinned server-side on podman/k8s exactly as
      on docker. A `@sha256:` candidate reference is still accepted on any
      engine (`digestFromReference` short-circuits before the backend lookup),
      and D2's `--image` keeps its `--resolve-digest` behaviour (default on)
      that pins client-side before submission, so an operator can type a tag
      even where the server has no registry credentials. No
      `digest_unresolved` caveat state exists — a backtest either has one digest
      for all N runs or it is not created.
      Replay's own fan-out
      cross-checks (`assertReusedProducerListMatches`, `adoptRecordedPartitions`)
      are untouched — an image override never changes a partition list, because
      producers cannot be overridden.
      Files: `internal/replay/replay.go`, `internal/models/run.go` (the `JobRun`
      `DescriptorOverrides` column); reads `internal/imagecheck/resolve.go`.
      Depends on: A4 + B1.
- [ ] C2. Add the `--path` server-safe delta extraction and the authorization
      capability split. The CLI computes the step-level delta between a candidate
      `job.yaml` and the baseline descriptors and submits it as the **same typed
      override set** — the API **never trusts a whole jobdef**. **Structural
      changes** (steps added/removed, edges changed) are **rejected** toward
      `caesium job diff` + `dev --once` (a new step has no recorded baseline
      inputs). Wire the higher-privilege **override capability** check in the
      backtest controller (code overrides require it; the params-only path is
      unchanged) behind a `CAESIUM_BACKTEST_OVERRIDE_API_KEY` (or capability) env,
      with bounded override sizes. Param `--set` overrides reuse the existing
      mechanism unchanged, with the known v1 cost: `RunParams` is hashed wholesale
      into every task, so any param override re-runs the **full DAG** of every
      baseline run — the cost is printed before dispatch.
      **Refresh (2026-09-05):** the full-DAG cost is `Constructor.Prepare`'s
      `paramsChanged` → `planTasks(..., forceReexecute=true)`; a `--path` delta
      that changes a step's `fanOut` block or a fan-out producer's image/command
      is a structural change under rules 2–3 and is rejected with the same
      guidance. Under `CAESIUM_AUTH_MODE=api-key` prefer a capability on the API
      key (the `iauth.Principal` the replay fingerprint already sees) over a
      second shared secret.
      **Decision (2026-09-05) — the `AUTH_MODE=none` rule, scoped.** The 2026-07
      wording refused code overrides outright under `CAESIUM_AUTH_MODE=none`. That
      is unworkable and is **corrected here**: `pkg/env/env.go` defaults
      `AuthMode` to `"none"` (`envconfig:"AUTH_MODE" default:"none"`) and
      `just integration-up-distributed` sets no `CAESIUM_AUTH_MODE` — so the only
      lane that can re-execute a replay runs in `none` mode, and a blanket refusal
      would make Stream C's override scenario and acceptance criterion 3
      unrunnable. **Scope the refusal to jobdef-mutating proposals:** arc
      convention 4's "refused under `CAESIUM_AUTH_MODE=none`" governs the
      `apply_jobdef_patch` router (which edits a job definition), not a
      quarantined backtest (which executes candidate code with every Caesium-side
      side effect suppressed and writes nothing to the job definition). This plan
      therefore **explicitly deviates from a literal reading of convention 4** and
      states why: in `AUTH_MODE=none` the shared-secret form
      `CAESIUM_BACKTEST_OVERRIDE_API_KEY` authorizes code overrides (presented as
      a request header, constant-time compared, absent ⇒ 403); in
      `AUTH_MODE=api-key` the principal capability is required and the env key is
      ignored. F1/F2's `backtest_patch` → `apply_jobdef_patch` chain is unaffected:
      the *patch* half still goes through the convention-4 router and still
      requires an active auth mode, which is why F2's scenario runs on the
      auth-enabled agent lane. H-1 sets the env key on the distributed lane and AC
      3 asserts the override path there.
      Files: `internal/backtest/` (override delta extraction),
      `api/rest/controller/backtest/` (capability gate), `pkg/env/env.go`.
      Depends on: C1.

#### Deferred — fan-out producer overrides (v2)

Lifting rule 3 above: allow an override on a fan-out producer, let the candidate
producer re-execute, then compare the list it emitted against the recorded one
(`TaskExecutionFanOut.Partitions`) and mark the run `DEGRADED` ("candidate
changed the partition set: +2/−1 partitions") rather than replaying the recorded
group against it. Needs a completion-side hook in `internal/run/fanout.go`'s
expansion path that records the emitted list on the quarantined producer row
without expanding, and a decision on whether new partitions are "changed output"
or "structural change". Not a gate for this plan.

### Stream D — Backtest CLI

The operator surface: a new top-level `caesium backtest` Cobra group. `caesium
replay` / `run diff` live under `cmd/run/`, but backtest is a new top-level verb
group appended to the `cmds` slice in `cmd/execute.go`. Clean machine output is
the repo's hard-learned rule: `--json` writes parseable stdout with logs on
stderr, captured separately in the integration test (`runCLIStdout`).

- [ ] D1. Add the P0 `caesium backtest` CLI. New `cmd/backtest/` group appended to
      `cmds` in `cmd/execute.go`: **create** (`--job <alias|id>`,
      `--against last-30-runs`, `--set k=v`, `--ignore-output glob`…, `--dry-run`,
      `--json`, `--format markdown`, `--allow-changes`, `--idempotency-key`,
      `--timeout`) driving `POST /v1/jobs/:id/backtest` then a client-side
      poll-to-terminal loop (like `replay --diff`) that renders the verdict matrix;
      and `caesium backtest report <backtest-id> --job <id>` re-rendering a stored
      report from `GET …/backtests/:btid` (the Action's comment step; also how
      humans re-attach after a timeout). `--format markdown` emits the PR-comment
      body. Clean stdout via `cmd.OutOrStdout()`, logs to stderr; **non-zero exit**
      when any verdict is `changed`/`failed` unless `--allow-changes`.
      **Refresh (2026-09-05):** render per-task diffs with `outputdiff.Diff.Render()`
      (the same text `reproduce --diff` prints) so the two verbs read identically;
      `--json` embeds the `Diff` struct. Reserve the `assertions` subcommand name
      for F3 (do not use it for anything else).
      **Deliberate divergence from the design (2026-09-05).**
      `docs/design-backtesting.md` `## CLI` specifies create as flags on the *root*
      command (`caesium backtest --job <alias|id> --against last-30-runs …`) plus a
      `backtest report <id> --job <id>` subcommand. This plan instead ships create
      as an explicit **`caesium backtest create`** subcommand, matching the arc's
      acceptance criterion 4 (`caesium backtest create --against last-30-runs
      --image …`) and leaving the root command free to host `report` and F3's
      `assertions` without a flags-vs-subcommand ambiguity. Per the Source-Of-Truth
      Note this divergence is only legal once the design says so: **N-2 amends
      `docs/design-backtesting.md` first** and D1 depends on it.
      Files: new `cmd/backtest/`, `cmd/execute.go`.
      Depends on: B1 + B2 + N-2.
- [ ] D2. Add the override flags to the CLI: `--image step=ref`,
      `--command step='…'`, and `--path candidate.job.yaml` (submitting C2's
      server-safe step-level delta). These carry the typed override set into B1;
      structural-change rejection surfaces the `job diff` / `dev --once` guidance.
      **Refresh (2026-09-05):** the CLI refuses `--image step=partition=ref` and
      any override naming a fan-out producer client-side with the same message the
      server returns (C1 rules 2–3), so the user never waits on a 4xx for it.
      Files: `cmd/backtest/`.
      Depends on: D1 + C2.

### Stream E — Console UI: backtest report view

The web surface the design specs: a backtest report view whose run-matrix heat
strip makes the month-end clustering visible at a glance. Reuses the shipped
`RunDiffView` unchanged for per-run drill-down. `ui/**` gate applies.

- [ ] E1. Add the backtest report view + run-matrix heat strip + Backtests tab.
      `/jobs/:id/backtests/:btid`: header (candidate digest vs baseline range, cost
      split), then a **run-matrix heat strip** — one cell per baseline run in date
      order: green unchanged / amber changed / red failed / grey skipped. A cell
      drills into the per-run view: the output-delta table plus the existing
      **RunDiffView** (baseline left, replay right), reused unchanged. Add a
      "Backtests" tab to the job page and link a quarantined replay's run-detail
      page back to its owning backtest. Add the route in `ui/src/router.tsx`, the
      API methods in `ui/src/lib/api.ts`, and a `BacktestEnabled` field to the
      `Features` struct in `api/rest/service/system/system.go` (so the UI gates on
      the server config).
      **Refresh (2026-09-05):** the run-detail back-link reads `JobRun.BacktestID`
      (A1) from the run payload; the per-run delta table renders `outputdiff.Diff`
      rows (`changed`/`added`/`removed`) — one component, reused by F2's approval
      card summary. Fanned steps show one row per partition under the step. The
      heat-strip colour map is defined here over the **output** verdict vocabulary
      only (A1's closed five); F3 extends the same map for its assertion
      vocabulary and owns that change.
      Files: new `ui/src/features/backtests/`, `ui/src/router.tsx`,
      `ui/src/lib/api.ts`, `api/rest/service/system/system.go`.
      Depends on: B1 + B2.

#### Deferred — CI Action fourth step (P2, external repo)

The §2.1 Action gaining a fourth chained step (**lint → diff → backtest →
comment**), the dry-run-then-label cost guard, and the marketplace publish live
in the external `caesium-action` repo, **not this one**, and are recorded
deferred here. The caesium-side enablers — `--format markdown` output and
`--dry-run` cost plan — ship in Stream D (D1) and B (A4/B1), so the external
Action can chain them once this backend lands. This deferral is not part of this
plan's acceptance criteria.

### Stream F — Proposal verification & assertion backtests (arc synergy)

The arc's "prove" column, made concrete: the backtest report gets a consumer
other than a human reading a PR. An `apply_jobdef_patch` proposal — whether it
comes from a `data_quality_hold` incident (Plan 1
[`data-circuit-breaker.md`](data-circuit-breaker.md) Stream F) or an `oom`
incident (Plan 2 [`resource-right-sizing.md`](resource-right-sizing.md)
Stream E) — can carry a backtest, and the approval surfaces show its verdict;
an assertion threshold can be backtested against `DatasetMetric` history before
it is enabled, with **no re-execution**; and (stretch) a `resources:` override
can be backtested for fit. Every item extends `internal/incident/` — the arc's
conflict table sequences 1-F → 2-E → 3-F on that package, **never in the same
wave**.

**Cross-plan dependencies (stated once, inline per item below).** Corrected
2026-09-05 against the arc: the tier-3 pipeline is **Plan 0's**, not Plan 1's.
- **F1/F2** need **Plan 0 C4/C7** — C4 creates the `ApprovalRequest` in
  `Executor.Execute`'s `decisionApprove` branch and calls
  `agentsvc.SetActionExecutor(...)` from `cmd/start/start.go`; C7 adds
  `Executor.ExecuteApproved(ctx, actionID)` so an approved action actually runs,
  plus the **direct** `apply_jobdef_patch` route — **and Plan 1 Stream F**, which
  adds the **Git-PR** route of `apply_jobdef_patch` (behind the new
  `CAESIUM_GIT_WRITE_CREDENTIALS` env field) and the `data_quality_hold` incident
  class that produces F2's proposals. Plan 2 Stream E's `propose_resources`
  reuses the same pipeline and is needed only for the `oom` variant of F2.
- **F3** needs Plan 1 A2 (`DatasetMetric` model + retention pruner), A5
  (`internal/run/baseline.go` compute-on-read baseline) and B1 (the evaluator in
  `internal/run/data_assertions.go`).
- **F4** needs Plan 2 A1 (`TaskRun` stats columns: peak memory, OOM-killed,
  applied resources) and B3 (`resources` excluded from `cache.HashInput`, applied
  resources carried on the descriptor).

**Verified gap the whole stream sits on (2026-09-05).** The shipped incident
runtime records tier-3 proposals but does not yet complete the loop: in the
current tree `models.ApprovalRequest` rows are created only by a unit test
(`api/rest/service/incident/incident_test.go`) — no production producer;
`api/rest/service/agent/actions.go` `SetActionExecutor` is never called from
`cmd/start/start.go`, so `propose_action` (REST `POST /v1/agent/incidents/:id/actions`
and MCP `propose_action` in `api/rest/controller/agent/mcp.go`) takes the
degraded path that only inserts a `proposed` `AgentAction`; `Service.decide`
(`api/rest/service/incident/approvals.go`) flips the action to `approved` but
nothing executes an approved action; and `internal/incident/provenance.go` (the
`apply_jobdef_patch` provenance router `agent-in-the-loop-remediation.md` B4
cites as its file) does not exist. **Correction (2026-09-05):** the 2026-07 cut
said "Plan 1 Stream F must build it"; the arc assigns it to **Plan 0 Stream C**
— arc Synergies row: "Plan 0, Stream C (C4 proposal → `ApprovalRequest`; C7
approve → execute + the direct `apply_jobdef_patch` route); Plan 1, Stream F (the
Git-PR provenance route)", convention 4 "Plan 0 C4/C7 builds it", and the
cross-plan sequencing line "Plan 2 Stream E and Plan 3 Stream F … depend on the
pipeline *existing* (Plan 0 C4/C7 + Plan 1 F)". Both `trust-the-substrate.md` C4
and C7 exist and own exactly that work. **Plan 0 C4/C7 (and Plan 1 F for the
Git-PR route + `data_quality_hold`) must have merged before F1/F2 are eligible.**
This stream is written against the shape those items specify and builds none of
it.

- [ ] F1. Add the `backtest_patch` remediation action to the catalog and make it
      dispatchable. Add a **tier-2 (`TierGated`)** action `backtest_patch` to
      `internal/incident/actions.go` (`actionCatalog`) — tier 2 because it launches
      N container workloads, so a playbook must explicitly allow it. `ActionOps`
      gains `Backtest(ctx, jobID, overrides, against) (backtestID uuid.UUID, err
      error)`, implemented in `cmd/start/incident_ops.go` over the Stream A engine;
      `dispatch` gains the case and records `{"backtest_id": …}` in the action
      result. The agent-session token is scoped to `/v1/agent/*`
      (`api/middleware/auth_scope.go`), so this action is the **only** way an agent
      can start a backtest — it never reaches `POST /v1/jobs/:id/backtest`.
      **Job-schema allow-list (verified, easy to miss):** `pkg/jobdef/definition.go`
      keeps a **closed** `remediationActions` lookup set and
      `validateRemediationActionList` errors `"%q is not a known remediation
      action"` for anything outside it — so without an entry, `caesium job lint`
      rejects any playbook whose `metadata.remediation.autonomy.allow` (or
      `perClass[].allow` / `requireApproval`) names `backtest_patch`, and a tier-2
      action can never be permitted. F1 therefore also adds
      `RemediationActionBacktestPatch = "backtest_patch"` beside
      `RemediationActionApplyJobdefPatch` **and** its `remediationActions` entry,
      and appends the name to the hard-coded action-name list in
      `internal/jobdef/report/report.go` `Markdown()`'s `## Remediation` block
      (regenerating `docs/job-schema-reference.md` — never hand-edit it;
      `TestGeneratedSchemaReferenceIsCurrent` pins it). Unit-test that a manifest
      allowing `backtest_patch` lints green.
      Files: `internal/incident/actions.go`, `cmd/start/incident_ops.go`,
      `pkg/jobdef/definition.go` (action const + `remediationActions` entry),
      `internal/jobdef/report/report.go` (+ regenerated
      `docs/job-schema-reference.md`).
      Depends on: C2 + N-2. Blocked cross-plan on Plan 0 C4/C7.
- [ ] F2. Link a backtest to an `apply_jobdef_patch` proposal and show the verdict
      everywhere the proposal is decided. **Linkage:** `ActionParams` gains
      `BacktestID *uuid.UUID json:"backtest_id,omitempty"`; on an
      `apply_jobdef_patch` proposal the executor verifies the referenced
      `Backtest` belongs to the incident's job (extend `verifyActionBoundary`) and
      that its `Overrides` equal the patch's step-level delta (C2's extractor;
      mismatch is recorded as a failed action, not silently accepted), then copies
      the durable counters (`requested/eligible/changed/unchanged/failed`) into
      the action's `Result` so the card never re-reads the engine. The MCP tool
      schema for `propose_action` (`internal/mcp/tools.go` `AgentTools`) documents
      `params.backtest_id` — the `params` object is already free-form, so this is
      a description change plus the server-side validation above. **Surfaces:**
      `ui/src/features/incidents/ApprovalCard.tsx` `JobDefPatchPreview` renders a
      "backtested over N runs: K changed, F failed, S skipped" badge row (from
      `action.result`) with a link to `/jobs/:id/backtests/:btid` (E1) and the E1
      delta component for changed runs; a proposal with no backtest renders "not
      backtested" (never blank); `caesium incident get <id>` (`cmd/incident/incident.go`
      `getCmd`, which prints `GET /v1/incidents/:id`'s `Detail{Actions, Approvals}`
      via `cliutil.WritePrettyJSON`) carries the same fields because they live on
      the action row — assert them, do not add a second renderer. **Scenario (auth
      lane):** a `data_quality_hold` (Plan 1 F) or `oom` (Plan 2 E) incident whose
      agent runs `backtest_patch` then proposes `apply_jobdef_patch` with
      `backtest_id`; the approval card (Playwright, `just ui-e2e-auth`) and
      `caesium incident get --json` (`runCLIStdout`) show the verdict; a mismatched
      `backtest_id` is refused.
      **The scenario MUST seed a permitting profile — verified blocker.**
      `just integration-up-agent` sets
      `CAESIUM_AGENT_DEFAULT_PROFILE=triage-only`, and
      `api/rest/service/agentprofile/agentprofile.go` `SeedDefaults` seeds that
      profile with `{"autonomy": {"allow": []}}`. `internal/incident/executor.go`
      `Playbook.decide` returns `decisionDeny` for a `TierGated` (tier-2) action
      unless `pb.Allow[actionType]` is true, and `Execute` then finishes the row
      `rejected` with `ErrActionNotPermitted` — so under the lane's default profile
      `backtest_patch` is **never dispatched** and no backtest is ever started.
      The scenario therefore creates (or updates) an `AgentProfile` whose
      `autonomy.allow` contains `backtest_patch` and attaches it to the seeded job
      (or sets the job's `metadata.remediation.allow`, which F1's schema entry now
      accepts) **before** triggering the incident, and asserts the action reaches
      `executed`, not `rejected`. Keeping `backtest_patch` at tier 2 is deliberate
      (N container workloads is not zero-risk autonomy); the cost is this explicit
      allow-listing step.
      Because `integration-up-agent` runs in **local** mode, the auth-lane backtest
      must be one that never re-executes (a P0 same-code backtest over cache-served
      baselines, verdict "N unchanged") — the override-verdict path is covered on
      the distributed lane by Stream C's scenario; see H-1 and the open question on
      flipping the agent lane to distributed. Depends inline on **Plan 0 C4/C7**
      (proposal → `ApprovalRequest` → approve → execute) and **Plan 1 Stream F**
      (the Git-PR `apply_jobdef_patch` route + `data_quality_hold`); for the `oom`
      variant only, Plan 2 Stream E.
      Files: `internal/incident/executor.go` (`ActionParams`,
      `verifyActionBoundary`), `internal/mcp/tools.go`,
      `api/rest/service/agent/actions.go` (params passthrough),
      `ui/src/features/incidents/ApprovalCard.tsx`, `ui/src/lib/api.ts`,
      `test/` (auth-lane scenario), `ui/tests/` (Playwright spec).
      Depends on: F1 + D1 + E1.
- [ ] F3. Add the assertion-threshold backtest — pure metric replay, no
      re-execution. `caesium backtest assertions --job <alias|id> --step <name>
      --against last-30-runs (--path candidate.job.yaml | --assertions-file f.yaml)
      [--json]` extracts the candidate `produces[].assertions` block (Plan 1 A3's
      schema: `rowCount`, `nullRate`, `freshness`, `custom[]` with `onViolation`
      and `deltaFromBaseline`-style thresholds) and submits `mode: assertions` to
      `POST /v1/jobs/:id/backtest` (same endpoint, same idempotency; the engine
      short-circuits before any replay). For each selected baseline run
      (oldest-first), evaluate the candidate block against that run's recorded
      `DatasetMetric` rows using the rolling baseline **as it would have been at
      that run** — the preceding `CAESIUM_BASELINE_WINDOW` clean samples only, via
      Plan 1 A5's compute-on-read helper in `internal/run/baseline.go` with an
      explicit `asOf` cut so no future sample leaks — and Plan 1 B1's evaluator
      exposed as a pure function (sibling change: `run.EvaluateDataAssertions`
      must factor a side-effect-free `evaluate(spec, sample, baseline) []Violation`
      that F3 can call without executors, holds, or `DataViolation` writes).
      Per-run verdict: `would_pass` / `would_warn` / `would_fail` / `would_hold`
      (by the candidate's `onViolation`) / `no_sample` (the run recorded no
      metrics for that dataset — the cache-short-circuit edge Plan 1 A3
      documents) / `cold_start` (fewer than `CAESIUM_BASELINE_MIN_SAMPLES`
      preceding samples, mirroring B1's seeding rule). The report says "this
      threshold would have held 3 of 30 runs (2026-05-31, 2026-06-30, …)" — the
      pre-enable check that prevents an alert storm at rollout (arc synergy
      table). **Keep A1's output-verdict enum closed.** Rows persist as
      `BacktestRun`s, but F3 adds a discriminator column
      `BacktestRun.VerdictKind` (`"output"` — the A1 default backfilled for every
      existing row — or `"assertion"`) rather than widening A1's five-value
      `Verdict` enum with a second, incompatible six-value vocabulary; readers
      switch on `VerdictKind` before interpreting `Verdict`. `OutputDelta` carries
      the evaluated metric/baseline/threshold triple. E1's heat strip does **not**
      render this unchanged — E1 is specified over the four output colours only
      (green unchanged / amber changed / red failed / grey skipped) and ships two
      waves earlier — so **F3 owns extending E1's colour map** for
      `VerdictKind: "assertion"` (`would_pass` → green, `would_warn` → amber,
      `would_fail`/`would_hold` → red, `no_sample`/`cold_start` → grey), in
      `ui/src/features/backtests/`. Because nothing
      re-executes this runs on **every** lane that has the flag on (per H-1 the
      default `integration-up` lane deliberately does not), and it needs
      `CAESIUM_DATA_ASSERTIONS_ENABLED=true` (Plan 1 H-1) and metric history —
      the scenario runs a `produces`+`##caesium::metrics` job N times with a
      drifting row count, then backtests a tighter threshold and asserts the
      exact would-hold run set from `--json` stdout (`runCLIStdout`). Depends
      inline on Plan 1 A2 + A5 + B1 (merged).
      Files: new `internal/backtest/assertions.go` (+ `assertions_test.go`),
      `internal/models/backtest.go` (`BacktestRun.VerdictKind`),
      `api/rest/service/backtest/`, `api/rest/controller/backtest/`,
      `cmd/backtest/` (the `assertions` subcommand),
      `ui/src/features/backtests/` (assertion colour map), `test/`.
      Depends on: A4 + B1 + D1 + E1 + N-2.
- [ ] F4. *(Stretch — explicitly optional, not an acceptance gate.)* Add the
      resource-override backtest. `--resources step=<mem>[,cpu=<n>]` adds a
      `Resources` field to `StepOverride`. Because Plan 2 B3 **excludes**
      `resources` from `cache.HashInput` (the `QueueName` "scheduling metadata,
      not an execution input" precedent), a resources-only override does not
      change any hash — so C1's honest-hash rule cannot drive re-execution here
      and the planner must **force** the overridden step to re-execute
      (`plannedTask.reexecute = true` for that group, with the normal
      `pendingPredecessors` cascade downstream; a resources-only override on a
      fully cache-served baseline is otherwise a no-op). The candidate size rides
      the descriptor's applied-resources field (Plan 2 B3's descriptor bump) to
      the worker; per replay attempt the engine reads Plan 2 A1's `TaskRun` stats
      columns (peak memory, OOM-killed, applied resources, escalation level —
      arc convention 5, no second stats table) and the per-run verdict becomes
      `fits` (peak ≤ candidate × headroom) / `tight` / `oom` (OOM-killed at the
      candidate size) — reported beside the output verdict, never replacing it.
      Cost is the full re-execution of the step and its downstream per baseline,
      printed by `--dry-run`. The scenario (distributed lane, Docker) backtests a
      known-peak stress image at a size below and above its peak and asserts
      `oom` vs `fits`; k8s parity is deferred to the helm/kind lane per arc
      convention 2. Depends inline on Plan 2 A1 + B3 (merged).
      Files: `internal/replay/replay.go` (the `Resources` override + forced
      re-execution — **same true-conflict file as C1; never in C1's wave**),
      `internal/backtest/`, `cmd/backtest/`, `test/`.
      Depends on: C2 + D2 + N-2.
- [ ] F5. Make every backtest verdict a persisted event and `why`-explainable
      (arc convention 3 — this plan's explainability item). **Event:** add
      `event.TypeBacktestRunVerdict` (`"backtest_run_verdict"`) to
      `internal/event/bus.go`, emitted per `BacktestRun` on verdict with
      `Quarantine: true`, `RunID` = the replay run, and a payload
      `{backtest_id, baseline_run_id, verdict, changed_keys, overrides,
      requested_by: action_id|principal}`; persisted through the event store and
      flowing through the notification subscriber
      (`internal/notification/subscriber.go`) so a channel can subscribe to
      verdicts without polling. **`why`:** the explainer is **task-scoped** (arc
      convention 3) — the shipped surfaces are
      `caesium why <run-id> --task <task> --job-id <job-id> [--partition <p>]`
      (`cmd/why/why.go`, whose required flags are `whyJobID`/`whyTask`) and
      `GET /v1/jobs/:id/runs/:run_id/why?task=<t>` (the only `why` route
      `api/rest/bind/bind.go` mounts: `g.GET("/jobs/:id/runs/:run_id/why",
      whyctrl.Get)`). **There is no `GET /v1/runs/:id/why` and no run-level
      `caesium why <run>` form** — the 2026-07 cut cited both and is corrected
      here; this plan adds **no** run-level `why` verb (convention 3 forbids it
      unless an item says otherwise, and none does). So: on a task of a backtest
      replay run, `caesium why <replay-run> --job-id <job-id> --task <task> --json`
      reports that the run is a quarantined replay owned by backtest `<id>` for
      baseline `<run>` with override `<step: image a → b>` —
      `internal/run/why.go` `WhyExplanation` gains a
      `Backtest *WhyBacktest{ID, BaselineRunID, Overrides, Verdict}` block read
      from `JobRun.BacktestID` (A1) + `JobRun.DescriptorOverrides` (C1) + the
      `BacktestRun` row, rendered by `cmd/why/why.go` `renderTable` beside
      `TRIGGER` (which already prints `replay (quarantined-replay)` from the run's
      `TriggerType`/`TriggerAlias`); the existing `Diff` half attributes the
      re-run to the `image` field for free. **Scenario:** on the distributed lane,
      an image-override backtest over N runs; assert
      `caesium why <replay-run> --job-id <job-id> --task <overridden-step> --json`
      (`runCLIStdout`) names the backtest, baseline, and override, and assert the
      `backtest_run_verdict` events via the SSE/event read the shipped
      `test/sse_test.go` uses.
      Files: `internal/event/bus.go`, `internal/notification/subscriber.go`,
      `internal/backtest/engine.go` (emit), `internal/run/why.go`,
      `cmd/why/why.go`, `test/`.
      Depends on: C1 + D1.
- [ ] F6. Show the proposal → backtest → approval chain on the incident timeline.
      The incident detail (`api/rest/service/incident/incident.go` `Detail`) and
      `ui/src/features/incidents/IncidentDetailPage.tsx` render the chain
      `backtest_patch` action → backtest `<id>` (link to `/jobs/:id/backtests/:btid`)
      → `apply_jobdef_patch` proposal (with verdict) → approval decision, all from
      rows that already exist after F1/F2 (no new table). **Scenario:** on the
      auth lane, extend F2's scenario to assert the timeline chain in
      `caesium incident get --json` (`runCLIStdout`) and in the Playwright
      incident-detail spec (`just ui-e2e-auth`).
      Files: `api/rest/service/incident/incident.go`,
      `ui/src/features/incidents/IncidentDetailPage.tsx`, `ui/src/lib/api.ts`,
      `test/`, `ui/tests/`.
      Depends on: F2 + F5.

## Harness Strengthening

- [ ] H-1. Ensure the integration server exercises the real backtest path: set
      `CAESIUM_BACKTEST_ENABLED=true` (the feature is gated `false` by default) and
      the override capability key (`CAESIUM_BACKTEST_OVERRIDE_API_KEY`) on the
      integration servers (**superseded in the Refresh below: NOT on the default
      `just integration-up` lane, which hosts the gated-off 404 scenario**), and
      ensure the lane that runs backtest scenarios runs in
      **distributed execution mode** (backtest inherits
      `ErrReplayRequiresDistributedMode` for any non-cache-served run), so the
      Stream A/B/C/D scenarios drive the live surface rather than an internal call
      — mirroring the lineage `CAESIUM_OPEN_LINEAGE_ENABLED` precedent the
      `CLAUDE.md` end-to-end gate calls out.
      **Refresh (2026-09-05) — lanes, by name (arc convention 2).** The default
      lane (`integration-up` → `integration-test`) is **local** mode and cannot
      re-execute a replay; do not try to flip it. Instead:

      (1) **Leave `CAESIUM_BACKTEST_ENABLED` OFF on the default `integration-up`
      recipe, and set it on every other server-starting lane.** This is a
      **deliberate, stated deviation from arc convention 2** (which says a plan's
      H-1 enables its flag on `just integration-up` *and* every self-server lane),
      taken for the same reason Plan 0 took it for incidents: item (4) below needs
      one lane where the routes are genuinely unmounted, and
      `justfile` `integration-up` sets no `CAESIUM_AGENT_REMEDIATION_ENABLED`
      precisely so `test/incident_gating_test.go`
      `TestIncidentRoutesGatedOffByDefault` can pass there. The 2026-07 wording
      ("add it to **every** server-starting recipe … a 'gated off by default'
      scenario on the default lane") was self-contradictory and is corrected here.
      So add `CAESIUM_BACKTEST_ENABLED=true` + `CAESIUM_BACKTEST_OVERRIDE_API_KEY`
      to: `integration-up-distributed`, `integration-up-owner-memory`,
      `integration-up-agent`, `integration-up-infra`, the podman recipe
      (`integration-test-podman` starts its own server inline), the helm/kind
      lane's **`config.extraEnv` list in `helm/caesium/ci/test-values-k8s.yaml`**
      (the `.github/workflows/ci.yml` step passes only
      `--values ./helm/caesium/ci/test-values-k8s.yaml --set image.tag=…`, so the
      env belongs in the values file, not in a `--set` on the workflow — the
      2026-07 wording misplaced it), and **`ui-e2e` and `ui-e2e-auth`**, which each
      `docker run` their own server with their own `-e CAESIUM_*` list and are the
      gates for E1's and F2/F6's Playwright specs — without the flag those specs
      would render a `BacktestEnabled: false` feature. Lanes that start their own
      server silently go red (or hollow) when a flag is added to only one lane.
      Because the default lane has the flag off, every functional backtest
      scenario guards on a shared `requireBacktestLane()` helper (new
      `test/backtest_lane_test.go`, keyed on an env var the enabling recipes set,
      mirroring `CAESIUM_AGENT_AUTH_LANE`) so it skips there rather than failing.

      (2) The re-executing scenarios (Stream A override path, C, D2, F4, F5) run
      on **`just integration-test-distributed`**
      (`build-and-integration-test-distributed`), whose `go test -run` filter is a
      **suite-qualified explicit list** (`TestIntegrationTestSuite/(TestRunConcurrencyStrategies|…|TestFanOut|…)`)
      — add `TestBacktest` to that alternation, and name every backtest scenario
      `TestBacktest*` (a bare method name matches nothing; the fanned-replay
      scenarios are named `TestFanOut*` for exactly this reason —
      `test/replay_fanout_test.go`). Scenarios that must also pass on another lane
      branch on `distributedLane()` (`test/fanout_helpers_test.go`) and assert the
      per-baseline `requires distributed execution mode` skip there. That recipe
      sets **no** `CAESIUM_AUTH_MODE`, and `pkg/env/env.go` defaults `AuthMode` to
      `"none"` — which is why C2's override authorization uses the
      `CAESIUM_BACKTEST_OVERRIDE_API_KEY` shared-secret form in `none` mode
      (see C2's decision note). **Do not** add `CAESIUM_AUTH_MODE=api-key` to
      `integration-up-distributed`: every other scenario on that lane is written
      against an unauthenticated server.

      (3) The approval-card / `incident get` / MCP scenarios (F2, F6's auth half)
      run on the **auth-enabled lane** `just integration-test-agent`
      (`build-and-integration-test-agent-auth`; `integration-up-agent` sets
      `CAESIUM_AUTH_MODE=api-key`, `CAESIUM_AGENT_REMEDIATION_ENABLED=true` and
      `CAESIUM_AGENT_DEFAULT_PROFILE=triage-only`), whose `-run` filter is
      `agent_integration_run` (default `TestIntegrationTestSuite/TestAgent`) —
      widened by **Plan 0 H-1** (not Plan 0 Stream C; `trust-the-substrate.md`
      H-1 step (c)) to the exact alternation
      `TestIntegrationTestSuite/(TestAgent|TestAuth|TestIncident|TestScoped|TestHold)`.
      Name F2's scenario `TestIncidentBacktestedProposal…` so the `TestIncident`
      arm selects it, and add the Playwright approval-card spec to the
      **`just ui-e2e-auth`** recipe (its real name — there is no "`ui-e2e` auth
      lane"). Remember the `triage-only` profile allows no tier-2 action, so F2's
      scenario must seed a permitting `AgentProfile` (see F2).

      (4) A **"gated off by default" scenario on the default `just integration-test`
      lane** asserts the backtest routes 404 when the flag is unset — the direct
      mirror of `test/incident_gating_test.go`, and the reason item (1) keeps the
      flag off there. Name it so it is *not* `TestBacktest*`-guarded by
      `requireBacktestLane()` (e.g. `TestBacktestRoutesGatedOffByDefault` with the
      inverse guard: skip on any lane that sets the flag).
      Files: `justfile`, `.github/workflows/ci.yml`,
      `helm/caesium/ci/test-values-k8s.yaml`, `test/` harness helpers (new
      `test/backtest_lane_test.go`).

## Navigational / Organizational Improvements

- [ ] N-1. Flip the roadmap and reconcile the docs. Update the
      [`docs/roadmap.md`](../../roadmap.md) Phase-4 exploration-table "Pipeline
      backtesting" row, the Phase 5 sequence-table row 3 ("The proof loop"), and
      the §2.1 Action note to reflect the shipped
      state; update the [`design-backtesting.md`](../../design-backtesting.md)
      `> Status:` banner from Brainstorm/Design to shipped-per-stream (N-2 has
      already amended that doc's `## CLI` / `### REST` / action surface). Document the
      `metadata.backtest` fields (`ignoreOutputs`, `backtestMode`) and the
      `caesium backtest` verb across `docs/job-schema-reference.md`,
      `docs/job-definitions.md`, and `docs/caesium-job-llm-reference.md`; add a
      backtest example (`metadata.backtest` block, pinned images) under
      `docs/examples/`. In `docs/README.md`, refresh **both** existing entries to
      the shipped state: the top-level `design-backtesting.md` bullet — which is a
      **markdown link** (`- [design-backtesting.md](design-backtesting.md): …`)
      and is legal because its basename *is* a top-level `docs/*.md`, so keep it
      as a link — and the **backticked** `exec-plans/active/backtesting.md` bullet
      under "Active Exec Plans", which must stay in backtick/inline-code form
      because `TestDocsREADMEIndexesEveryTopLevelDoc`
      (`guardrails_test.go`) rejects a markdown link whose basename is not a
      top-level `docs/*.md`. (The 2026-07 wording called the first entry a
      "backtick reference"; it is not.) When this plan moves to
      `docs/exec-plans/completed/`, move the bullet to the completed list and
      repoint the path. Runs last, after the runtime ships.
      **Refresh (2026-09-05) — arc convention 7.** `docs/job-schema-reference.md`
      is **generated**: add the `metadata.backtest` section to
      `internal/jobdef/report/report.go` `Markdown()` (a `## Backtest` block beside
      `## Remediation`) and regenerate — never edit the doc by hand
      (`TestGeneratedSchemaReferenceIsCurrent` pins it). Write
      **`docs/tour-proof-loop.md`**: the 10-minute walkthrough — apply a
      `replaySafe` job with a `produces`/`assertions` block, run it a few times,
      `caesium backtest create --against last-5-runs --image transform=…` and read
      the matrix, `caesium backtest assertions` a tighter threshold and read the
      would-hold set, then (with `CAESIUM_AGENT_REMEDIATION_ENABLED`) watch an
      incident proposal arrive with its verdict on the approval card and
      `caesium why <replay-run> --job-id <job-id> --task <step>` name the
      backtest — every image in the tour in
      the repo's pinned form. Tick the **arc dashboard row** for Plan 3 in
      `closed-loop-arc.md` (waves shipped, last PR) in the same PR, move this
      plan to `docs/exec-plans/completed/`, and repoint the arc's links. If this
      is Plan 3's final wave, draft `tell-it.md` per the arc's "Closing wave"
      section (or confirm it is already drafted).
      Files: `docs/roadmap.md`, `docs/design-backtesting.md`,
      `internal/jobdef/report/report.go` (+ regenerated
      `docs/job-schema-reference.md`), `docs/job-definitions.md`,
      `docs/caesium-job-llm-reference.md`, `docs/examples/`, `docs/README.md`,
      new `docs/tour-proof-loop.md`, `docs/exec-plans/active/closed-loop-arc.md`
      (dashboard row only).
      Depends on: A–F (runs last, after the runtime ships; F4 excluded).
- [ ] N-2. Amend the design doc for the surface this plan adds beyond it, **before
      D1 / F1 / F3 / F4 ship**. The Source-Of-Truth Note above forbids adding "a new
      verb, endpoint, config knob, or job-schema field beyond what the design
      enumerates without first amending the design", and this cut adds five such
      things that `docs/design-backtesting.md` does not enumerate:
      (a) **`caesium backtest create`** as a subcommand — the design's `## CLI`
      section specifies create as flags on the root command
      (`caesium backtest --job <alias|id> --against last-30-runs …`) plus
      `backtest report <backtest-id> --job <id>`; the arc's acceptance criterion 4
      says `caesium backtest create --against last-30-runs --image …`, and this
      plan follows the arc (D1);
      (b) **`mode: assertions`** on the `POST /v1/jobs/:id/backtest` body — the
      design's `### REST` enumerates only "baseline selector, overrides, ignore
      paths, `dryRun`" (F3);
      (c) **`caesium backtest assertions`** and the `BacktestRun.VerdictKind`
      discriminator + assertion verdict vocabulary (F3);
      (d) the **`backtest_patch`** remediation action and the `params.backtest_id`
      linkage on an `apply_jobdef_patch` proposal (F1/F2);
      (e) **`--resources step=…`** on the override set (F4, stretch — record it as
      stretch in the design too).
      Add a short "Arc synergy (2026-09)" section to the design carrying (b)–(e)
      and amend `## CLI`/`### REST` for (a)–(c), citing the arc's Synergies row
      "Backtesting had no consumer for its report except a human reading a PR"
      (owned by Plan 3, new Stream F) as the authority for the scope expansion.
      No code, no roadmap flip — N-1 still owns the status banner.
      Files: `docs/design-backtesting.md`.
      Depends on: nothing (first-wave eligible).

## Sequencing & Dependencies

**Cross-stream order:**

- **Stream A is the foundation** — B, C, D, E, and F all consume the models, the
  store, the engine, or the endpoints A backs. A merges first (largest blast
  radius). A1 → A2 → A3 → A4 is a strict chain (model+store, then selection,
  then delta+schema, then orchestration that plugs both in).
- **Stream B** (REST) depends on A4 (the engine). B1 (create) → B2 (report/list
  reads B1's rows).
- **Stream C** (overrides) depends on A4 + B1 — C1/C2 extend `internal/replay/`
  and the backtest controller established by A/B, so C runs **after** them, not in
  parallel. C1 → C2. **Before C1 starts, re-grep `internal/replay/replay.go`** for
  the post-#374 shape (`baselineGroup`, `computeDescriptorInstanceHash`,
  `groupBaselineTasks`) — the arc's conflict table names this as the pre-condition.
- **Stream D** (CLI): D1 (P0 create/report) depends on B1 + B2 + **N-2**; D2
  (override flags) depends on D1 + C2.
- **Stream E** (UI) depends on B1 + B2 (the endpoints it renders).
- **Stream F** runs **after D** (F2/F3 drive `cmd/backtest/`; F5 renders through
  `cmd/why/`) **and after the cross-plan incident substrate**: F1 → F2 need
  **Plan 0 C4/C7** (the tier-3 proposal → `ApprovalRequest` → approve → execute
  pipeline plus the direct `apply_jobdef_patch` route) **and Plan 1 Stream F**
  (the Git-PR route of `apply_jobdef_patch` and the `data_quality_hold` class);
  the `oom` variant of F2 additionally needs Plan 2 Stream E. F3 needs Plan 1
  A2/A5/B1. F4 needs Plan 2 A1/B3. In-plan: F1 → F2; F3 is independent of F1/F2;
  F5 depends on C1 + D1 only; F6 depends on F2 + F5; F4 is optional and last.
  `internal/incident/` is shared with Plan 0 C4/C7, Plan 1 F and Plan 2 E —
  **never the same wave** as any of them (arc conflict table).
- **H-1** is independent (justfile/CI/helm-values/test harness) and supports the
  A/B/C/D integration scenarios; land it in the first wave so the engine's
  end-to-end gate has a live, distributed surface to drive. Its
  `justfile`/`ci.yml` edits collide with every other plan's H-1 — one plan's H-1
  per wave.
- **N-2** is a docs-only design amendment with no dependencies and **must land
  before D1, F1, F3 and F4** (the Source-Of-Truth rule). Land it in the first
  wave alongside A1/H-1.
- **N-1** runs last, after A–F ship, so the roadmap/schema/design docs reflect
  reality.

**Suggested waves:**
- **W1 = A (A1→A2→A3→A4) + H-1 + N-2.** A is one strict chain; H-1 wires the live
  surface the A integration scenario needs; N-2 is the docs-only design amendment
  D1/F1/F3/F4 are blocked on.
- **W2 = B (B1→B2).** Unblocked once A's engine is in.
- **W3 = C (C1→C2) + E (E1).** Both depend on B; C edits `internal/replay/` +
  `internal/models/run.go` + the controller, E edits `ui/**` +
  `api/rest/service/system/system.go` — no file overlap, safe in parallel.
- **W4 = D (D1→D2).** D2 needs C2's override delta; D1 needs B + N-2.
- **W5 = F3 + F5 (parallel).** F3 is gated on Plan 1 A2/A5/B1 having merged; F5
  needs only C1 + D1. Disjoint files (F3: `internal/backtest/assertions.go`,
  `cmd/backtest/`, `internal/models/backtest.go`, the backtest UI colour map;
  F5: `internal/event/bus.go`, `internal/run/why.go`, `cmd/why/why.go`). Neither
  touches `internal/incident/`, so this wave is not blocked on Plan 0/1/2.
- **W6 = F1 → F2 → F6.** Gated on **Plan 0 C4/C7 and Plan 1 Stream F** having
  merged (and Plan 2 E for the `oom` variant of F2). All three touch
  `internal/incident/` — never in the same wave as Plan 0 C4/C7, Plan 1 F, or
  Plan 2 E. F1 also edits `pkg/jobdef/definition.go`, so this wave is the plan's
  second (and last) claim on that cross-plan true-conflict file after A3.
- **W7 (optional) = F4.** Only if Plan 2 A1 + B3 have merged and the arc still
  has momentum; edits `internal/replay/replay.go` (C1's file) in its own wave.
- **W8 = N-1.** Docs last (W7 when F4 is skipped).

**Within-stream order:** A1 → A2 → A3 → A4 (strict). B1 → B2. C1 → C2. D1 → D2.
E1 standalone. F1 → F2 → F6; F3 and F5 independent of that chain; F4 optional,
last. N-2 first, N-1 last.

**Cross-stream file conflicts:**

**Arc conflict-table rows this plan participates in** (the 2026-07 cut claimed
`internal/replay/replay.go` was "this plan's only true-conflict file"; that is
**wrong** — the arc's table also names `pkg/jobdef/definition.go` for `3-A3`, and
F1 now needs it too. The full list):

| Arc conflict-table row | This plan's items | Rule |
|---|---|---|
| `internal/replay/replay.go` | 3-C1, 3-F4 (optional) | C1 and F4 never in the same wave; re-verify the post-#374 shape first |
| `pkg/jobdef/definition.go` (`Step`/`rawStep`, `Validate()`) | 3-A3 (`metadata.backtest`), 3-F1 (`RemediationActionBacktestPatch` + `remediationActions`) | true conflict across plans (1-A3, 2-B1, 3-A3, 4-A2) — one plan per wave; A3 (W1) and F1 (W6) are different waves |
| `internal/incident/{classifier,actions,executor}.go` + `cmd/start/start.go` incident wiring | 3-F1, 3-F2 | sequential by plan — never a wave shared with 0-C4/C7, 1-F or 2-E |
| `internal/event/bus.go` + `internal/notification/subscriber.go` | 3-F5 | one plan's explainability item per wave |
| `internal/run/why.go` + `cmd/why/why.go` | 3-F5 | one plan's explainability item per wave |
| `pkg/env/env.go`, `internal/metrics/metrics.go`, `internal/models/models.go`, `api/rest/bind/bind.go`, `cmd/execute.go`, `cmd/start/start.go` | A1, A4, B1, B2, C2, D1 | additive; one plan's item per wave; `models.All` order matters |
| `ui/src/lib/api.ts`, `ui/src/router.tsx`, `Sidebar.tsx` | E1, F2, F3, F6 | one plan's UI stream per wave |
| `justfile`, `.github/workflows/ci.yml` | H-1 | one plan's H-1 per wave |
| `docs/roadmap.md`, `docs/README.md`, `closed-loop-arc.md` | N-1 | last item of the plan that ships the change |

In-plan detail:

- `internal/replay/replay.go` — **Stream C only** edits it (C1's override
  plumbing) within the core plan; **F4** (optional) is the one exception and must
  not share a wave with C1. A2 and B *read* replay's `Prepare` /
  `ErrReplayRequiresDistributedMode` but do not edit `replay.go`.
- `pkg/jobdef/definition.go` — **A3** (the `metadata.backtest` block) **and F1**
  (the `backtest_patch` remediation action const + `remediationActions` entry,
  without which `validateRemediationActionList` rejects any playbook allowing the
  action). Different waves (W1, W6), so no in-plan collision; across plans it is
  true-conflict (1-A3, 2-B1, 3-A3, 4-A2) — one plan per wave, and this plan claims
  it twice, so neither W1 nor W6 may share it with another plan.
- `internal/jobdef/report/report.go` (+ the generated
  `docs/job-schema-reference.md`) — **F1** (the action-name list in the
  `## Remediation` block) and **N-1** (the `## Backtest` metadata block).
  Different waves; regenerate, never hand-edit the doc.
- `internal/models/models.go` — **A1 only** appends `Backtest` + `BacktestRun`
  to the `All` slice. `internal/models/run.go` — **A1** adds `JobRun.BacktestID`
  (W1) and **C1** adds `JobRun.DescriptorOverrides` (W3): additive columns in
  different waves (`JobRun` is already registered, so neither touches
  `models.go`). No same-wave collision between A and C.
- `internal/incident/{actions.go,executor.go}`, `cmd/start/incident_ops.go`,
  `internal/mcp/tools.go` — **F1 + F2** in this plan (F1 the catalog entry and
  `ActionOps.Backtest`, F2 the `ActionParams.BacktestID` + `verifyActionBoundary`
  check); shared with **Plan 0 C4/C7**, Plan 1 F and Plan 2 E across plans —
  sequential by plan, never the same wave.
- `api/rest/service/incident/incident.go` — **F6 only** (the `Detail` chain).
- `internal/event/bus.go`, `internal/notification/subscriber.go` — **F5 only**;
  additive type + subscriber case (every plan's explainability item appends here
  — one plan's item per wave).
- `internal/run/why.go`, `cmd/why/why.go` — **F5 only** (additive block; no new
  route and no run-level verb — the shipped task-scoped
  `GET /v1/jobs/:id/runs/:run_id/why` is extended, not forked).
- `internal/models/backtest.go` — **A1** creates it; **F3** adds
  `BacktestRun.VerdictKind` (additive column, later wave).
- `internal/metrics/metrics.go` — A4 (`caesium_backtest_runs_total` etc.) and B1
  (`caesium_backtest_created_total`) each add a collector (two edit sites: the
  `var (...)` block + `Register()`). A4 is W1, B1 is W2 — different waves, no
  same-wave overlap.
- `pkg/env/env.go` — A1 (`CAESIUM_BACKTEST_ENABLED`,
  `CAESIUM_BACKTEST_MAX_PARALLEL_REPLAYS`) and C2
  (`CAESIUM_BACKTEST_OVERRIDE_API_KEY`) append fields in different waves (W1, W3);
  additive, rebases mechanically.
- `api/rest/bind/bind.go` — B1 + B2 add routes (same stream B); additive import
  block. `cmd/execute.go` — D1 appends one command group (single stream).
  `api/rest/service/system/system.go` — E1 only (the `Features` struct).
- `ui/src/lib/api.ts`, `ui/src/router.tsx` — E1 (W3), F3/F5 (W5) and F2/F6 (W6):
  different waves; across plans one plan's UI stream per wave.
  `ui/src/features/backtests/` — E1 owns it; F3 extends only its verdict colour
  map (later wave).
- `justfile`, `.github/workflows/ci.yml` — H-1 only; one plan's H-1 per wave.
- **No `go.mod`/`go.sum` change is expected** — the feature composes shipped
  packages (`internal/replay`, `internal/outputdiff`, `internal/imagecheck`,
  `internal/run`, `internal/receipt`); if a stream does add a dependency, flag
  the `go.sum` conflict for `go mod tidy` resolution, not a hand-merge.
- **No `internal/cache/hash.go` change**: `metadata.backtest` is comparison/
  attestation metadata that does not participate in step execution identity, and
  the descriptor-override honest-hash plumbing lives in `internal/replay/`
  (`computeDescriptorInstanceHash`), not the job-schema cache key — so the cache
  key is untouched. (F4's forced re-execution is a planner decision, not a hash
  change, precisely because Plan 2 B3 keeps `resources` out of `HashInput`.)

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Per-stream additions:

- **New REST endpoint / CLI verb (A, B, C, D):** an integration scenario in
  `test/` that drives the **real surface** against a live **distributed** server
  (`just integration-test-distributed`, scenario named `TestBacktest*` so the
  lane's `-run` alternation selects it — H-1) —
  seed a `replaySafe` job, run it N times with varying params/outputs, then
  `POST /v1/jobs/:id/backtest` (or the `caesium backtest` binary via the
  `s.runCLI*` helpers) and assert observed output: the P0 no-override backtest is
  100% unchanged and all cache-served; an image override that changes output for
  one param shape marks exactly those runs `changed` (assert re-executed/cached
  counts, **zero production cache writes, zero lineage rows, no metric drift** —
  reuse the replay suppression assertions in `test/replay_matrix_e2e_test.go`); a
  candidate exiting non-zero →
  `failed` verdict + non-zero CLI exit; a pre-`replaySafe` baseline and an expired
  cache entry both surface `skipped` with reasons; a fanned baseline (the
  `test/replay_fanout_test.go` manifest) backtests with per-partition decisions
  and an override on its producer is refused. A unit test that hand-builds a
  delta proves the delta, not the wiring — both are required.
- **Machine-readable CLI output (D, F3, F5, F6):** assert `--json` stdout is clean
  and parseable, captured **separately** from stderr via `runCLIStdout` (not the
  stream-merging capture).
- **New metric (A4, B1):** assert via `internal/metrics/testutil` in a
  `*_test.go`; the collector must also appear in `Register()`.
- **Job-schema change (A3, F1):** `caesium job lint --path docs/examples/` green on
  the new `metadata.backtest` example manifest (A3) and on a manifest whose
  `metadata.remediation.autonomy.allow` names `backtest_patch` (F1); `just
  unit-test` green on `TestGeneratedSchemaReferenceIsCurrent` after F1 and N-1
  regenerate the reference from `internal/jobdef/report/report.go`.
- **UI change (E, F2, F3, F6):** `just ui-lint && just ui-test && just ui-e2e` for
  the report view / heat strip / RunDiffView drill-down (both `ui-e2e` and
  `ui-e2e-auth` set `CAESIUM_BACKTEST_ENABLED` per H-1, or the feature renders
  disabled); the approval-card verdict and incident-timeline specs run on
  **`just ui-e2e-auth`**.
- **Incident / approval paths (F1, F2, F6):** `just integration-test-agent` with
  the scenario named to match `agent_integration_run`'s widened alternation
  `TestIntegrationTestSuite/(TestAgent|TestAuth|TestIncident|TestScoped|TestHold)`
  (Plan 0 H-1); the scenario seeds an `AgentProfile` allowing the tier-2
  `backtest_patch` (the lane's `triage-only` default allows none), and the
  same-code backtest on that local-mode lane must be fully cache-served.
- **Explainability (F5):** one scenario asserts
  `caesium why <replay-run> --job-id <job-id> --task <step> --json` names the
  backtest/baseline/override (arc convention 3 — the explainer is task-scoped;
  there is no run-level `why`).
- **This plan's checkbox ticked**, the active-wave `## Progress` bullet appended,
  and any cross-linked doc (roadmap/schema) refreshed in the same PR.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream A — the P0 backtest runtime** is a runtime feature: a same-code
   backtest selects N non-quarantined baselines, reports eligibility with per-run
   skip reasons (including `ErrFannedBaseline` and the local-mode
   `requires distributed execution mode` case), runs N quarantined replays
   through the existing dispatch machinery gated by `CAESIUM_BACKTEST_ENABLED`
   and capped by `CAESIUM_BACKTEST_MAX_PARALLEL_REPLAYS`, and computes per-run
   output-delta verdicts **with `internal/outputdiff`** (no second comparator in
   `internal/backtest/`). Closed by a `test/` integration scenario: seed → run N
   times → no-override backtest reports 100% unchanged, all cache-served, green
   in CI on the distributed lane, with the `caesium_backtest_*` metrics
   registered and asserted.
2. **Stream B — the REST API** is live: `POST /v1/jobs/:id/backtest`
   (`Idempotency-Key` required, `dryRun` returns the cost plan without
   dispatching, code overrides refused without the capability) returns `202` + a
   backtest ID; `GET /v1/jobs/:id/backtests/:btid` reads the durable verdict
   matrix and `GET /v1/jobs/:id/backtests` lists; the routes are unmounted when
   the flag is off. Closed by integration scenarios
   hitting the live server, including the dry-run cost plan and the idempotent
   re-create on a **flag-enabled** lane, plus the gated-off 404 on the default
   `just integration-test` lane, whose server deliberately leaves
   `CAESIUM_BACKTEST_ENABLED` unset (H-1).
3. **Stream C — descriptor overrides** work: an image override resolves to a
   digest up front, flows honestly into `computeDescriptorInstanceHash` (only the
   intended step's `HashInput` field changes; upstream hashes byte-identical),
   re-executes that step and its downstream, records the candidate on the replay
   `TaskRun` (from `plannedTask.candidate`, with `ExecutionDescriptor` still the
   baseline) + the `DescriptorOverrides` column, and requires the higher-privilege
   override authorization — **the `CAESIUM_BACKTEST_OVERRIDE_API_KEY` shared
   secret under `CAESIUM_AUTH_MODE=none` (the distributed lane's mode) and a
   principal capability under `api-key` mode** (C2's scoped rule); a structural
   (`--path`) change is rejected toward `job diff`; an override on a fanned
   template step re-executes every partition, while partition-scoped and
   fan-out-producer overrides are refused with the typed errors; a candidate
   whose digest cannot be established is refused with `ErrCandidateUnpinned`
   on every engine, and every replay of a backtest runs the single digest
   stored on the `Backtest` row (asserted by inspecting the N replay
   `TaskRun.ResolvedImageDigest` values). Closed by an
   override integration scenario on `just integration-test-distributed` asserting
   exactly the changed runs, re-executed/cached counts, the fan-out rules, and
   zero production side effects.
4. **Stream D — the CLI** ships: `caesium backtest` create/report drive the real
   endpoints, poll to terminal, render the verdict matrix (per-task diffs via
   `outputdiff.Diff.Render()`), emit `--format markdown`, exit non-zero on
   changed/failed unless `--allow-changes`. Closed by
   an integration test driving the real binary with `--json` stdout asserted
   clean and parseable via `runCLIStdout` (captured separately from stderr).
5. **Stream E — the Console** surfaces the backtest report view, the run-matrix
   heat strip, the Backtests tab, the RunDiffView drill-down, and the
   replay-run → owning-backtest back-link, gated by the `BacktestEnabled`
   feature flag. Closed by the Playwright e2e (`just ui-e2e`) green in CI.
6. **Stream F — proposal verification & assertion backtests** close the arc's
   proof column: `backtest_patch` is a tier-2 action in the incident catalog and
   an accepted `metadata.remediation` action name in `pkg/jobdef/definition.go`
   (F1); an `apply_jobdef_patch` proposal can reference a backtest the agent
   started through it, the approval card and `caesium incident get` show
   "backtested over N runs: K changed", and a mismatched reference is refused
   (F2, auth lane, with an `AgentProfile` allowing the tier-2 action);
   `caesium backtest assertions` evaluates a candidate `produces.assertions`
   block against `DatasetMetric` history with no re-execution and reports the
   exact would-hold run set under `VerdictKind: "assertion"` (F3);
   `caesium why <replay-run> --job-id <job-id> --task <step>` names the backtest,
   baseline, and override and the `backtest_run_verdict` event is persisted (F5);
   the incident timeline shows the proposal → backtest → approval chain (F6).
   Closed by the F1/F2/F3/F5/F6 scenarios green on their lanes. **F4
   (resource-override backtest) is explicitly optional and is not a gate** — if
   shipped, its scenario asserts `oom` vs `fits`; if not, it is recorded as
   deferred.
7. **H-1 — the integration servers** exercise the backtest path
   (`CAESIUM_BACKTEST_ENABLED=true` + `CAESIUM_BACKTEST_OVERRIDE_API_KEY` on every
   server-starting lane **except the default `integration-up`**, which stays off
   to host the gated-off 404 scenario — including `ui-e2e`, `ui-e2e-auth`, the
   podman recipe and `helm/caesium/ci/test-values-k8s.yaml`'s `config.extraEnv`;
   `TestBacktest` added to the distributed lane's `-run` alternation; F2's
   scenario selected by the auth lane's
   `(TestAgent|TestAuth|TestIncident|TestScoped|TestHold)` filter), so the Stream
   A/B/C/D/F scenarios run against the live binary in CI, not an internal call.
8. **N-1 — docs reflect reality:** the `docs/roadmap.md` backtest rows and §2.1
   Action note updated, the design-doc `> Status:` banner flipped, the
   `metadata.backtest` fields + `caesium backtest` verb documented via the
   `internal/jobdef/report` generator with a working `docs/examples/` manifest,
   `docs/tour-proof-loop.md` written, this plan indexed in `docs/README.md`, and
   the arc dashboard row ticked. **N-2 — the design amendment** landed *before*
   D1/F1/F3/F4, so no shipped verb, endpoint, config knob, action name or schema
   field exceeds what `docs/design-backtesting.md` enumerates.
9. **Cross-cutting:** `docs/roadmap.md`, `docs/design-backtesting.md`,
   `closed-loop-arc.md`'s dashboard, and this plan's per-stream `## Progress`
   entries reflect every shipped stream and match the merged PRs. (The CI Action
   fourth step remains explicitly deferred to the external `caesium-action`
   repo — not a gate here; fan-out producer overrides remain deferred to v2.)

## How To Pick Up Work

1. Read [`closed-loop-arc.md`](closed-loop-arc.md) first, then this file
   end-to-end so you understand the streams, their interdependencies, and which
   acceptance criterion the item closes. When the two disagree on cross-plan
   ordering, the arc wins.
2. Pick an unchecked item under `## Streams` whose `Depends on:` line is satisfied
   (consult `## Sequencing & Dependencies`, including the inline cross-plan
   dependencies on Stream F items). Never run a Stream F item that touches
   `internal/incident/` (F1, F2) in the same wave as Plan 0 C4/C7, Plan 1 Stream F
   or Plan 2 Stream E — and do not start F1/F2 at all until Plan 0 C4/C7 and
   Plan 1 Stream F have merged.
3. Branch from `master` (or land in a worktree if dispatched by
   `exec-plan-wave`); do the work as a self-contained PR. Re-grep every symbol
   this plan cites before editing (arc convention 6) — `internal/replay/replay.go`
   in particular.
4. Run the verification block under `## Verification (Run For Every PR)`.
5. Tick the checkbox for your item, add a per-stream bullet to the active wave
   subsection in `## Progress` (or open a new wave subsection if none exists yet),
   and update any cross-linked design doc / roadmap section in the same PR.
6. Open the PR with title format
   `<Imperative subject> (backtesting <wave>-<stream>)` — e.g.
   `Add the backtest runtime engine (backtesting W1-α)`. GitHub appends `(#NNN)`
   on squash-merge.

## Cross-References

- [`closed-loop-arc.md`](closed-loop-arc.md) — the umbrella arc; this is Plan 3.
  Shared conventions 1–8, the synergy table row this plan's Stream F owns, the
  cross-plan file-conflict table, and the arc acceptance criterion 4.
- [`docs/design-backtesting.md`](../../design-backtesting.md) — the design of
  record. Source of truth for intent and scope.
- [`docs/design-quarantined-replay.md`](../../design-quarantined-replay.md) — the
  inherited safety model; authoritative for every quarantine/`replaySafe`/
  suppression invariant reused here.
- [`docs/design-data-plane-memory.md`](../../design-data-plane-memory.md) — the
  descriptor/output substrate backtest replays and compares against.
- [`docs/design-reproduce.md`](../../design-reproduce.md) and
  [`reproduce.md`](../completed/reproduce.md) (Stream C, C1 — #339) — the
  single-task local counterpart on the same descriptor substrate and the shared
  `internal/outputdiff` comparator this plan reuses.
- [`dynamic-fanout.md`](../completed/dynamic-fanout.md) and PR #374 ("Replay
  fanned baselines from recorded partition lists") — the fan-out-aware replay
  core Stream C reconciles with.
- [`docs/design-agent-in-the-loop.md`](../../design-agent-in-the-loop.md) and
  [`agent-in-the-loop-remediation.md`](../completed/agent-in-the-loop-remediation.md)
  — the incident runtime, action catalog, approval flow, and `apply_jobdef_patch`
  provenance router Stream F attaches to (see the verified-gap note in Stream F).
- [`trust-the-substrate.md`](../completed/trust-the-substrate.md) — **Plan 0**; C4/C7 build the
  tier-3 proposal → `ApprovalRequest` → approve → execute pipeline (and the direct
  `apply_jobdef_patch` route) that F1/F2 consume, and H-1 widens the auth lane's
  `agent_integration_run` filter to
  `TestIntegrationTestSuite/(TestAgent|TestAuth|TestIncident|TestScoped|TestHold)`
  so F2's scenario is selected.
- [`data-circuit-breaker.md`](data-circuit-breaker.md) — Plan 1; Stream F (the
  Git-PR route of `apply_jobdef_patch` behind `CAESIUM_GIT_WRITE_CREDENTIALS`, and
  the `data_quality_hold` class) is F2's producer, A2/A5/B1 (`DatasetMetric`,
  baseline read, evaluator) are F3's substrate.
- [`resource-right-sizing.md`](resource-right-sizing.md) — Plan 2; Stream E
  (`propose_resources` via `apply_jobdef_patch`) is F2's `oom` producer, A1/B3
  (stats columns, `resources` outside the cache identity) are F4's substrate.
- [`docs/roadmap.md`](../../roadmap.md) Phase 5 (row 3) and the Phase-4
  exploration table + §2.1 PR Preview Runs & Visual DAG Diff — the strategic
  entries this plan advances.
- [`pkg/jobdef/definition.go`](../../../pkg/jobdef/definition.go) — the
  job-definition contract N-1 documents with the `metadata.backtest` block (via
  the `internal/jobdef/report` generator).
- `internal/replay/`, `internal/outputdiff/`, `internal/imagecheck/`,
  `internal/run/` (rundiff/whydiff/why), `internal/receipt/`,
  `api/rest/service/replay/`, `internal/incident/`, `internal/mcp/` — the
  shipped primitives this plan composes.
