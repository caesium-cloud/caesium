# Pipeline Backtesting — Regression-Test a Change Against Recorded Production History

Last updated: 2026-07-03

Caesium can already replay a single historical production run from its immutable
per-task `TaskExecutionDescriptor` with all Caesium-internal side effects
suppressed (quarantined replay, `internal/replay/replay.go`), and attribute *why*
two runs differ (causal run diff, `internal/run/rundiff.go`). What it cannot do
is answer the question data teams ask before every transform merge: **does my
change alter the numbers?** Pipeline changes ship on faith today — CI proves the
YAML lints and maybe that the job runs once against staging data, and the first
real test is tonight's production run, where the regression is discovered by the
*consumer* and reconstructed after the fact.

This plan ships **backtesting**: a pre-merge verb that replays a candidate change
over the last N production runs' recorded inputs and reports output deltas per
run ("your change alters output for 2 of 30 days; here is the diff"). It is the
composition of shipped primitives — quarantined replay, execution descriptors,
receipts, causal run diff — plus one significant piece of new machinery
(controlled **descriptor overrides**, so a replay can execute a candidate image /
command that did *not* run at baseline) with its own safety analysis. A backtest
aggregates N quarantined replays (one per selected baseline production run), each
executed with the candidate override applied to the reconstructed descriptors,
plus a per-run output-delta computation against the baseline's recorded outputs,
surfaced as a verdict matrix in the CLI, the REST API, the Console, and a PR
comment.

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
`--dry-run`) ship in Stream D. Every new CLI verb and REST endpoint ships with a
`test/` integration test that drives the real surface against a live distributed
server (per the `CLAUDE.md` end-to-end-coverage gate).

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
tracked in [`docs/roadmap.md`](../../roadmap.md) §2.1 and the §2 exploration
table (the roadmap wins on priority/status disagreements).

## Progress (as of 2026-07-03)

No implementation waves have shipped yet. The plan was published from the
[`design-backtesting.md`](../../design-backtesting.md) brainstorm/design proposal;
the first wave is the next eligible run of the `exec-plan-wave` skill against this
doc. The first-wave-eligible leaf items are **A1** (the backtest data model +
store + config) and **H-1** (the integration-server enablement) — neither has an
unmet dependency.

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Backtest runtime engine (P0) — `Backtest`/`BacktestRun` models + store, baseline selection + eligibility, output-delta + verdict classification, orchestration + metrics + startup wiring, `metadata.backtest` schema | **P0** | Not started |
| B | Backtest REST API — `POST /v1/jobs/:id/backtest` (idempotent, dry-run), the report + list reads, authorization capability | **P0** | Not started |
| C | Descriptor overrides (P1 headline) — typed `StepOverride`, honest override→hash plumbing, candidate-digest resolution, `DescriptorOverrides` column, `--path` delta + capability split | P1 | Not started |
| D | Backtest CLI — `caesium backtest` create/report, poll-to-terminal, `--json`/`--format markdown`/`--dry-run`, override flags | P1 | Not started |
| E | Console UI — backtest report view, run-matrix heat strip, Backtests tab, RunDiffView drill-down, features gate | P2 | Not started |
| H-1 | Integration harness — `CAESIUM_BACKTEST_ENABLED` + override capability on the live distributed integration server | — | Not started |
| N-1 | Docs — roadmap flip, design banner, `metadata.backtest` + `backtest` verb schema docs, example manifest, README repoint | — | Not started |
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
      Files: new `internal/models/backtest.go`, `internal/models/models.go`,
      new `internal/backtest/store.go` (+ `store_test.go`), `pkg/env/env.go`.
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
      (`internal/replay/replay.go` `Prepare`): every task run carries an
      `ExecutionDescriptor` at a supported schema version; tasks that would
      re-execute were recorded `replay_safe = true` **at baseline**
      (`TaskRun.ReplaySafe`, read from the baseline row, never the live definition);
      secret identities re-verify (env provider fails closed); unchanged tasks have
      live cache proof. An ineligible baseline is **reported and skipped with a
      per-run reason** ("pre-dates replaySafe", "cache proof expired (job TTL
      168h)"), never silently dropped; zero eligible baselines fails loudly.
      Files: new `internal/backtest/selection.go` (+ `selection_test.go`); reads
      `internal/replay/replay.go` `Prepare` (no edit).
      Depends on: A1.
- [ ] A3. Implement the output-delta computation, verdict classification, and the
      `metadata.backtest` job-schema block. Per task, compare baseline
      `TaskRun.Output` (typed JSON key→value map, ≤`MaxOutputBytes` per
      `pkg/task/output.go:45`) against the replay task's `Output`; for large-object
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
      Files: new `internal/backtest/delta.go` (+ `delta_test.go`),
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
      Files: new `internal/backtest/engine.go` (+ `engine_test.go`),
      `cmd/start/start.go`, `internal/metrics/metrics.go`.
      Depends on: A2 + A3.

### Stream B — Backtest REST API + observability

The HTTP surface over the engine: the create endpoint that drives the
orchestration, and the report/list reads. New capability, new endpoint, new
authorization surface — **do NOT extend `POST …/runs/:run_id/replay`**: its
contract is "params-only, identical code", its body is deliberately closed
(`DisallowUnknownFields`, `api/rest/controller/replay/replay.go:113`), and
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
`{BaselineRunID, Set, ReplayFingerprint}` (`internal/replay/replay.go:77`) —
**params are the only overridable input**; image/command/env/schema all come
pinned from the descriptor (`computeDescriptorHash` reads `desc.Runtime.Image` /
`ResolvedImageDigest` / `Command`), and that pinning is the identical-code
guarantee replay sells. Backtest extends the request with a typed per-step
override set so a replay can execute a candidate that did **not** run at baseline.
This heavily edits `internal/replay/replay.go` (a true-conflict file), so it is a
single stream and sequences after the engine that drives it.

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
      via `internal/imagecheck/resolve.go` — all N replays run that digest;
      unresolvable images are **refused, not degraded**.
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
      Files: `internal/backtest/` (override delta extraction),
      `api/rest/controller/backtest/` (capability gate), `pkg/env/env.go`.
      Depends on: C1.

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
      Files: new `cmd/backtest/`, `cmd/execute.go`.
      Depends on: B1 + B2.
- [ ] D2. Add the override flags to the CLI: `--image step=ref`,
      `--command step='…'`, and `--path candidate.job.yaml` (submitting C2's
      server-safe step-level delta). These carry the typed override set into B1;
      structural-change rejection surfaces the `job diff` / `dev --once` guidance.
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

## Harness Strengthening

- [ ] H-1. Ensure the integration server exercises the real backtest path: set
      `CAESIUM_BACKTEST_ENABLED=true` (the feature is gated `false` by default) and
      the override capability key (`CAESIUM_BACKTEST_OVERRIDE_API_KEY`) on the
      `just integration-up` / `just integration-test` server, and ensure the
      integration server runs in **distributed execution mode** (backtest inherits
      `ErrReplayRequiresDistributedMode` for any non-cache-served run), so the
      Stream A/B/C/D scenarios drive the live surface rather than an internal call
      — mirroring the lineage `CAESIUM_OPEN_LINEAGE_ENABLED` precedent the
      `CLAUDE.md` end-to-end gate calls out.
      Files: `justfile`, `.github/workflows/ci.yml`, `test/` harness helpers.

## Navigational / Organizational Improvements

- [ ] N-1. Flip the roadmap and reconcile the docs. Update the
      [`docs/roadmap.md`](../../roadmap.md) §2 exploration-table "Pipeline
      backtesting" row (line ~225) and the §2.1 Action note to reflect the shipped
      state; update the [`design-backtesting.md`](../../design-backtesting.md)
      `> Status:` banner from Brainstorm/Design to shipped-per-stream. Document the
      `metadata.backtest` fields (`ignoreOutputs`, `backtestMode`) and the
      `caesium backtest` verb across `docs/job-schema-reference.md`,
      `docs/job-definitions.md`, and `docs/caesium-job-llm-reference.md`; add a
      backtest example (`metadata.backtest` block, pinned images) under
      `docs/examples/`. In `docs/README.md`, **update the existing backtick
      reference** to `design-backtesting.md` (line ~42) to point at this plan — keep
      it backtick/inline-code form and do NOT add a clickable subdirectory link
      (the `TestDocsREADMEIndexesEveryTopLevelDoc` guardrail rejects those). Runs
      last, after the runtime ships.
      Files: `docs/roadmap.md`, `docs/design-backtesting.md`,
      `docs/job-schema-reference.md`, `docs/job-definitions.md`,
      `docs/caesium-job-llm-reference.md`, `docs/examples/`, `docs/README.md`.
      Depends on: A–E (runs last, after the runtime ships).

## Sequencing & Dependencies

**Cross-stream order:**

- **Stream A is the foundation** — B, C, D, and E all consume the models, the
  store, the engine, or the endpoints A backs. A merges first (largest blast
  radius). A1 → A2 → A3 → A4 is a strict chain (model+store, then selection,
  then delta+schema, then orchestration that plugs both in).
- **Stream B** (REST) depends on A4 (the engine). B1 (create) → B2 (report/list
  reads B1's rows).
- **Stream C** (overrides) depends on A4 + B1 — C1/C2 extend `internal/replay/`
  and the backtest controller established by A/B, so C runs **after** them, not in
  parallel. C1 → C2.
- **Stream D** (CLI): D1 (P0 create/report) depends on B1 + B2; D2 (override
  flags) depends on D1 + C2.
- **Stream E** (UI) depends on B1 + B2 (the endpoints it renders).
- **H-1** is independent (justfile/CI/test harness) and supports the A/B/C/D
  integration scenarios; land it in the first wave so the engine's end-to-end gate
  has a live, authenticated, distributed surface to drive.
- **N-1** runs last, after A–E ship, so the roadmap/schema/design docs reflect
  reality.

**Suggested waves:**
- **W1 = A (A1→A2→A3→A4) + H-1.** A is one strict chain; H-1 wires the live
  surface the A integration scenario needs.
- **W2 = B (B1→B2).** Unblocked once A's engine is in.
- **W3 = C (C1→C2) + E (E1).** Both depend on B; C edits `internal/replay/` +
  `internal/models/run.go` + the controller, E edits `ui/**` +
  `api/rest/service/system/system.go` — no file overlap, safe in parallel.
- **W4 = D (D1→D2).** D2 needs C2's override delta; D1 needs B.
- **W5 = N-1.** Docs last.

**Within-stream order:** A1 → A2 → A3 → A4 (strict). B1 → B2. C1 → C2. D1 → D2.
E1 standalone.

**Cross-stream file conflicts:**

- `internal/replay/replay.go` — **Stream C only** edits it (C1's override
  plumbing). A2 and B *read* replay's `Prepare` / `ErrReplayRequiresDistributedMode`
  but do not edit `replay.go`. True-conflict file kept to one stream.
- `pkg/jobdef/definition.go` — **Stream A only** (A3's `metadata.backtest`
  block). No other stream edits the schema, so no dual-`Step`/`Validate` collision.
- `internal/models/models.go` — **A1 only** appends `Backtest` + `BacktestRun`
  to the `All` slice. `internal/models/run.go` — **C1 only** adds the `JobRun`
  `DescriptorOverrides` column (`JobRun` is already registered, so C1 does not
  touch `models.go`). No same-file collision between A and C.
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
- **No `go.mod`/`go.sum` change is expected** — the feature composes shipped
  packages (`internal/replay`, `internal/imagecheck`, `internal/run`,
  `internal/receipt`); if a stream does add a dependency, flag the `go.sum`
  conflict for `go mod tidy` resolution, not a hand-merge.
- **No `internal/cache/hash.go` change**: `metadata.backtest` is comparison/
  attestation metadata that does not participate in step execution identity, and
  the descriptor-override honest-hash plumbing lives in `internal/replay/`
  (`computeDescriptorHash`), not the job-schema cache key — so the cache key is
  untouched.

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Per-stream additions:

- **New REST endpoint / CLI verb (A, B, C, D):** an integration scenario in
  `test/` that drives the **real surface** against a live **distributed** server —
  seed a `replaySafe` job, run it N times with varying params/outputs, then
  `POST /v1/jobs/:id/backtest` (or the `caesium backtest` binary via the
  `s.runCLI*` helpers) and assert observed output: the P0 no-override backtest is
  100% unchanged and all cache-served; an image override that changes output for
  one param shape marks exactly those runs `changed` (assert re-executed/cached
  counts, **zero production cache writes, zero lineage rows, no metric drift** —
  reuse the replay suppression assertions); a candidate exiting non-zero →
  `failed` verdict + non-zero CLI exit; a pre-`replaySafe` baseline and an expired
  cache entry both surface `skipped` with reasons. A unit test that hand-builds a
  delta proves the delta, not the wiring — both are required.
- **Machine-readable CLI output (D):** assert `--json` stdout is clean and
  parseable, captured **separately** from stderr via `runCLIStdout` (not the
  stream-merging capture).
- **New metric (A4, B1):** assert via `internal/metrics/testutil` in a
  `*_test.go`; the collector must also appear in `Register()`.
- **Job-schema change (A3):** `caesium job lint --path docs/examples/` green on
  the new `metadata.backtest` example manifest.
- **UI change (E):** `just ui-lint && just ui-test && just ui-e2e` for the report
  view / heat strip / RunDiffView drill-down / auth lane.
- **This plan's checkbox ticked**, the active-wave `## Progress` bullet appended,
  and any cross-linked doc (roadmap/schema) refreshed in the same PR.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream A — the P0 backtest runtime** is a runtime feature: a same-code
   backtest selects N non-quarantined baselines, reports eligibility with per-run
   skip reasons, runs N quarantined replays through the existing dispatch
   machinery gated by `CAESIUM_BACKTEST_ENABLED` and capped by
   `CAESIUM_BACKTEST_MAX_PARALLEL_REPLAYS`, and computes per-run output-delta
   verdicts. Closed by a `test/` integration scenario: seed → run N times →
   no-override backtest reports 100% unchanged, all cache-served, green in CI,
   with the `caesium_backtest_*` metrics registered and asserted.
2. **Stream B — the REST API** is live: `POST /v1/jobs/:id/backtest`
   (`Idempotency-Key` required, `dryRun` returns the cost plan without
   dispatching, code overrides refused without the capability) returns `202` + a
   backtest ID; `GET /v1/jobs/:id/backtests/:btid` reads the durable verdict
   matrix and `GET /v1/jobs/:id/backtests` lists. Closed by integration scenarios
   hitting the live server, including the dry-run cost plan and the idempotent
   re-create.
3. **Stream C — descriptor overrides** work: an image override resolves to a
   digest up front, flows honestly into `computeDescriptorHash` (only the intended
   step's `HashInput` field changes; upstream hashes byte-identical), re-executes
   that step and its downstream, records the candidate on the replay `TaskRun` +
   the `DescriptorOverrides` column, and requires the higher-privilege override
   capability; a structural (`--path`) change is rejected toward `job diff`.
   Closed by an override integration scenario asserting exactly the changed runs,
   re-executed/cached counts, and zero production side effects.
4. **Stream D — the CLI** ships: `caesium backtest` create/report drive the real
   endpoints, poll to terminal, render the verdict matrix, emit `--format
   markdown`, exit non-zero on changed/failed unless `--allow-changes`. Closed by
   an integration test driving the real binary with `--json` stdout asserted
   clean and parseable via `runCLIStdout` (captured separately from stderr).
5. **Stream E — the Console** surfaces the backtest report view, the run-matrix
   heat strip, the Backtests tab, and the RunDiffView drill-down, gated by the
   `BacktestEnabled` feature flag. Closed by the Playwright e2e (`just ui-e2e`)
   green in CI.
6. **H-1 — the integration server** exercises the backtest path
   (`CAESIUM_BACKTEST_ENABLED=true`, override capability set, distributed mode), so
   the Stream A/B/C/D scenarios run against the live binary in CI, not an internal
   call.
7. **N-1 — docs reflect reality:** the `docs/roadmap.md` backtest entry and §2.1
   Action note updated, the design-doc `> Status:` banner flipped, the
   `metadata.backtest` fields + `caesium backtest` verb documented in the schema
   references with a working `docs/examples/` manifest, and this plan indexed in
   `docs/README.md`.
8. **Cross-cutting:** `docs/roadmap.md`, `docs/design-backtesting.md`, and this
   plan's per-stream `## Progress` entries reflect every shipped stream and match
   the merged PRs. (The CI Action fourth step remains explicitly deferred to the
   external `caesium-action` repo — not a gate here.)

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
   `<Imperative subject> (backtesting <wave>-<stream>)` — e.g.
   `Add the backtest runtime engine (backtesting W1-α)`. GitHub appends `(#NNN)`
   on squash-merge.

## Cross-References

- [`docs/design-backtesting.md`](../../design-backtesting.md) — the design of
  record. Source of truth for intent and scope.
- [`docs/design-quarantined-replay.md`](../../design-quarantined-replay.md) — the
  inherited safety model; authoritative for every quarantine/`replaySafe`/
  suppression invariant reused here.
- [`docs/design-data-plane-memory.md`](../../design-data-plane-memory.md) — the
  descriptor/output substrate backtest replays and compares against.
- [`docs/design-reproduce.md`](../../design-reproduce.md) — the single-task local
  counterpart on the same descriptor substrate.
- [`docs/roadmap.md`](../../roadmap.md) §2.1 PR Preview Runs & Visual DAG Diff +
  the §2 exploration table — the strategic entries this plan advances.
- [`pkg/jobdef/definition.go`](../../../pkg/jobdef/definition.go) — the
  job-definition contract N-1 documents with the `metadata.backtest` block.
- `internal/replay/`, `internal/imagecheck/`, `internal/run/` (rundiff/whydiff),
  `internal/receipt/`, `api/rest/service/replay/` — the shipped primitives this
  plan composes.
