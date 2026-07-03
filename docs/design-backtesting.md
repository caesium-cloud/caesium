# Design: Pipeline Backtesting — Regression-Test a Change Against Recorded Production History

> Status: Brainstorm/Design — proposal for a pre-merge verb that replays a candidate change over the last N production runs' recorded inputs and reports output deltas per run. Composes shipped primitives (quarantined replay, execution descriptors, receipts, causal run diff); requires one significant piece of new machinery (controlled descriptor overrides) with its own safety analysis.

## Problem

Pipeline changes are deployed on faith. CI proves the YAML lints and maybe that
the job runs once against synthetic or staging data; the first real test is
tonight's production run — production skew, month-end edge cases, the vendor
file with the weird encoding. Staging data is not production data, so the
regression is discovered by the *consumer* and reconstructed after the fact.

Every mainstream orchestrator has this gap, because none *records* what a
production run consumed and produced in re-executable form. Caesium does: the
data-plane-memory substrate persists, per task run, an immutable
`TaskExecutionDescriptor` (`internal/models/run.go:154`), the decomposed
`HashInput` blob, typed step outputs (`TaskRun.Output`, run.go:80), resolved
image digests, and a content-addressed receipt (`internal/receipt/receipt.go`);
quarantined replay (`internal/replay/replay.go`) already re-executes a
historical run from those descriptors with Caesium-internal side effects
suppressed, and causal run diff (`internal/run/rundiff.go`) attributes *why*
two runs differ.

Backtesting is the composition: **before merge, replay the candidate change over
the last N production runs' recorded inputs and report output deltas per run** —
"your change alters output for 2 of 30 days; here is the diff" — as a PR comment.

## Fit with Design Principles

- **Declarative and GitOps-first (the star).** Roadmap §2.1 makes pipeline
  changes *reviewable* (lint → visual DAG diff → preview run → PR comment);
  backtesting makes them *testable against reality*. The unit under review is
  the manifest + image in git; the test fixture is production history the
  server already recorded.
- **Container-native execution.** The candidate is just a different
  image/command — no SDK, no test-harness contract.
- **Zero-dependency simplicity.** Baselines, descriptors, outputs, and reports
  all live in the existing dqlite store.
- **Smart by default.** Content-addressed caching makes N-run backtests
  affordable: only the changed step and its downstream re-execute (cost model).
- **Data engineering first.** "Does my refactor change the numbers?" is the
  question data teams ask before every transform merge.

## Overview

A **backtest** aggregates N quarantined replay runs, one per selected baseline
production run, each executed with a **candidate override** applied to the
reconstructed descriptors — a new image (by digest), a changed command or
schema, or param changes — plus an **output-delta computation** comparing each
replay's task outputs against its baseline's recorded outputs.

```
baseline runs (last 30)  ×  candidate image transform:pr-412@sha256:…
   ▼  N quarantined replays (descriptor-reconstructed, override applied)
   ▼  per-run output delta (TaskRun.Output + output-ref digests, ignore-paths)
   Backtest report: 28 unchanged · 2 changed · 0 failed  →  PR comment
```

Everything inherits the quarantined-replay safety model: replay runs are
`Quarantine=true`, write no production cache, emit no lineage, fire no
callbacks/notifications, pollute no metrics or run lists, and are gated by the
baseline-recorded `replaySafe` mark. What backtesting *adds* — executing code
that did **not** run at baseline — is the central new risk (see Safety).

## UX Example

```sh
$ caesium backtest --job daily-revenue --against last-30-runs \
    --image transform=registry.corp/transform:pr-412 \
    --ignore-output '*.generated_at' --server https://caesium.corp
backtest bt_9f2c…  baselines: 30 selected, 27 eligible
(3 skipped: 2 pre-date replaySafe, 1 cache proof expired)
replaying 27 runs …  cache-hit tasks: 41  re-executed: 22
RESULT: output changed in 2 of 27 runs
  2026-06-30  transform.row_count   41 872 → 40 619  (-3.0%)
  2026-05-31  transform.row_count   44 108 → 42 971  (-2.6%)
```

As a PR comment (§2.1 Action):

> ### Caesium backtest: `daily-revenue` × `transform:pr-412`
> **Output changes in 2 of 27 replayed production runs** (3 ineligible)
>
> | Baseline run | Date | Verdict | Changed outputs |
> |---|---|---|---|
> | `run_88a1` | 2026-05-31 | ⚠ CHANGED | `transform.row_count` 44 108 → 42 971 |
> | `run_90bc` | 2026-06-30 | ⚠ CHANGED | `transform.row_count` 41 872 → 40 619 |
> | 25 runs | 2026-06-01 … 2026-07-02 | ✓ unchanged | — |
>
> Both changed runs are **month-end**. Cost: 22 tasks re-executed, 41 cached.
> [Full report](…) · [Per-run diff](…)

The month-end pattern jumping out of the run matrix *is* the feature: this
regression only manifests on data shapes staging never has.

## Scenarios

1. **Schema-mapping change backtests clean.** A vendor renamed a field; the fix
   updates the extract mapping and `outputSchema`. The 4 runs since the rename
   show corrected outputs (expected, annotated by the author); 26 older runs are
   byte-identical. The reviewer approves with evidence instead of hope.
2. **"Harmless" refactor changes month-end.** A rewrite "with no behavior
   change" backtests unchanged on 28 of 30 runs — but both month-end runs lose
   ~3% of rows: it mishandles a partition that only exists at month boundaries.
   Caught in review; the consumer never sees it.
3. **Dependency bump changes nothing.** A base-image CVE bump backtests 30/30
   byte-identical (output-ref digests equal). Merge with confidence,
   receipt-grade evidence in the PR.

## Backend Design

### Baseline selection

`--against last-30-runs` resolves server-side to the job's most recent terminal,
**non-quarantined** production runs (`quarantine IS NOT TRUE`, the predicate the
replay work added to all baseline-selecting queries). Alternatives: a date range
or an explicit run-ID list.

Each selected baseline is checked for **eligibility**, reusing replay's
fail-closed validation (`internal/replay/replay.go` `Prepare`): every task run
carries an `ExecutionDescriptor` at a supported schema version; tasks that would
re-execute were recorded `replay_safe = true` **at baseline**
(`TaskRun.ReplaySafe`, `internal/models/run.go:90` — read from the baseline row,
never the live definition, so a later apply cannot retroactively authorize an
old run); secret identities re-verify (env provider fails closed); unchanged
tasks have live cache proof (see retention below).

An ineligible baseline is **reported and skipped**, never silently dropped
("27 of 30 eligible", with per-run reasons); zero eligible baselines fails
loudly. State the consequence plainly: **backtesting is only available for jobs
that opted into `replaySafe`, and only over runs recorded since they did** —
teams adopt `replaySafe: true` now so history accumulates.

### Descriptor overrides — the core new machinery

This does not exist today and must not be hand-waved. The shipped replay
`Request` is `{BaselineRunID, Set, ReplayFingerprint}`
(`internal/replay/replay.go:77`): **params are the only overridable input.**
Image, command, env, and schema all come pinned from the descriptor —
`computeDescriptorHash` reads `desc.Runtime.Image` / `ResolvedImageDigest` /
`Command` (replay.go:474-506) and `taskRunRecord` stamps the replay `TaskRun`
from the same fields (replay.go:817-860); that pinning is the identical-code
guarantee replay sells. Backtesting extends the request with a typed, per-step
override set:

```go
type StepOverride struct {
    StepName string   // must match a descriptor Baseline.TaskName
    Image    string   // resolved to a digest at backtest-create time
    Command  []string // optional
    OutputSchema / InputSchema / ValidationMode … // optional, P1
}
```

Rules:

- **Digest-resolved up front, once.** The candidate tag resolves to `sha256:…`
  at backtest creation (reusing `internal/imagecheck/resolve.go`); all N replays
  run that digest. Unresolvable images are refused, not degraded — a tag moving
  mid-backtest would compare apples to oranges.
- **Overrides flow into the hash honestly.** The override replaces the
  descriptor value *before* `computeDescriptorHash`, so the step's identity
  changes, it re-executes, and downstream re-executes via the
  `PredecessorHashes` cascade. Never hide an override from `HashInput` — the
  false-hit bug class the replay design forbids.
- **The replay TaskRun records both.** The stored descriptor keeps baseline
  values; the row's `Image`/`ResolvedImageDigest`/`Command` carry the candidate;
  a new `DescriptorOverrides` JSON column on the quarantined `JobRun` records
  the delta. `caesium why` / `run diff` attribute the re-run to the `image`
  field for free — the HashInput blob differs on exactly that field.
- **`--path candidate.job.yaml`:** the CLI computes the step-level delta between
  candidate jobdef and baseline descriptors and submits it as the same typed
  override set — the API never trusts a whole jobdef. Structural changes (steps
  added/removed, edges changed) are **rejected** toward `caesium job diff` +
  `dev --once`: a new step has no recorded baseline inputs.
- **Param overrides (`--set`)** reuse the existing mechanism unchanged, with its
  known v1 cost: `RunParams` is hashed wholesale into every task, so any param
  override re-runs the **full DAG** of every baseline run
  (`planTasks(..., forceReexecute=true)`, replay.go:186-190/443). The CLI prints
  this cost difference before dispatch.

**New safety implication:** shipped replay executes code that already ran in
production once; `replaySafe` attested that code. A backtest executes
**unvetted candidate code** against the baseline's real mounts, secrets,
network, and workload identity — the attestation does not transfer. See Safety.

### Output-delta computation

`RunDiff` today is cache-bust attribution — it diffs persisted `HashInput`
blobs (`internal/run/rundiff.go:91`, `DiffHashInputBlobs`), explaining *why a
task re-ran*; for a backtest that answer is always "you changed the image".
Backtest needs the other half: **did the outputs change?**

- **Comparison anchor:** per task, baseline `TaskRun.Output` (typed JSON
  key→value map, ≤64 KB total per `pkg/task/output.go` `MaxOutputBytes`) vs the
  replay task's `Output`. For large-object reference outputs (data-plane-memory
  C5), compare the **content digests** carried by the reference — byte-identical
  large outputs compare equal without moving data. A run-level roll-up digest
  over sorted terminal task outputs gives a single per-run "unchanged"
  attestation in the spirit of `internal/receipt` (deterministic,
  degraded-honest).
- **Verdicts per task:** `OUTPUT_UNCHANGED`; `OUTPUT_CHANGED` (per-key
  before/after `FieldChange`s, the `internal/run/whydiff.go` shape); `FAILED`
  (candidate errored where baseline succeeded — always a reported regression);
  `NOT_COMPARED` (cache-hit); `DEGRADED` (output missing on one side).
- **Ignore-paths, or timestamps lie to you.** Outputs routinely embed
  `generated_at`, run IDs, temp paths; without an ignore mechanism every
  backtest reports 30/30 changed and the feature is noise. Job-level config
  (`metadata.backtest.ignoreOutputs: ["*.generated_at", "report.run_id"]`) plus
  CLI override. Ignored keys are excluded from the delta *and listed in the
  report as ignored*, so a reviewer sees what the comparison chose not to see.
  Glob-on-`step.key` only; no regex-on-values in v1.

### Cache-aware cost model (and the retention bound)

Cost transparency is a first-class output, because the naive read is "30 × full
DAG = never".

- **Image/command override on step X:** only X and its transitive downstream
  re-execute per baseline run; every upstream task keeps a byte-identical
  `HashInput` and is served from baseline cache proof. A 6-step linear DAG
  changed at step 5 executes 2 + caches 4 per run — a ~3× win, larger for wide
  DAGs with a leaf change. The report prints the actual split (`22 re-executed /
  41 cache-hit` above); `caesium backtest --dry-run` prints the full plan
  *without dispatching anything*, so the PR Action can post cost before anyone
  approves execution.
- **Param override:** full-DAG per run (wholesale `RunParams` hashing, above).
  Honest and loud.
- **Retention bounds backtest depth.** Verified reality: `JobRun`/`TaskRun` rows
  (with their descriptors and outputs) are **not pruned today** — no run pruner
  exists. But `TaskCache` entries expire (`CacheExpiresAt` +
  `cache.Store.Prune()`, `internal/cache/store.go:173`), and the shipped replay
  planner **hard-aborts** an unchanged, cache-enabled task whose entry is gone
  (`replay.go:538`, `ErrUnavailableBaselineProof`; only cache-*disabled* tasks
  fall back to the baseline `TaskRun.Result`). So the practical window for
  cache-enabled jobs is `min(N requested, runs younger than cache TTL)`; the
  eligibility report says "skipped: cache proof expired (job TTL 168h)" and the
  docs say **size your cache TTL to your desired backtest depth**. Relaxing
  that abort is open question 1 — a replay-layer decision, not a silent one
  here.

### Scheduling, throttling, env

N replays are N real container workloads. Backtest feeds replays into the
**existing** dispatch machinery, gated by `CAESIUM_BACKTEST_ENABLED` (default
`false`), capped by `CAESIUM_BACKTEST_MAX_PARALLEL_REPLAYS` (default 2), and
sequenced oldest-first so partial results are meaningful; the cap keeps
quarantined work from starving production claims. Re-executing replay requires
distributed mode (`ErrReplayRequiresDistributedMode`,
`api/rest/service/replay/replay.go:32,146`); backtest inherits that for any run
not fully cache-served.

### Data model

Two new GORM models in `internal/models` (house `AutoMigrate` pattern):
`Backtest` (`ID`, `JobID`, `Status`, `Overrides` + `IgnorePaths` JSON, unique
nullable `Fingerprint`, requested/eligible/changed/unchanged/failed counters)
and `BacktestRun` (`BacktestID`, `BaselineRunID`, nullable `ReplayRunID`,
`Verdict` unchanged/changed/failed/skipped/degraded, `SkipReason`,
`OutputDelta` JSON, re-executed/cached counts). Creation is idempotent the way
replay creation is: `Idempotency-Key` header, scoped fingerprint (job +
baseline set + overrides + principal + key), insert before dispatch, resume on
duplicate. Each child replay's fingerprint derives from backtest fingerprint +
baseline run ID, so a crashed backtest resumes without double-executing any
baseline.

### REST

- `POST /v1/jobs/:id/backtest` — baseline selector, overrides, ignore paths,
  `dryRun`; `Idempotency-Key` required; returns `202` + backtest ID.
- `GET /v1/jobs/:id/backtests/:btid` — the report (verdict matrix + deltas);
  `GET /v1/jobs/:id/backtests` — list.
- Drill-down reuses the shipped
  `GET /v1/jobs/:id/runs/diff?left=<baseline>&right=<replay>` unchanged.

Extending `POST …/runs/:run_id/replay` was rejected: its contract is
"params-only, identical code", its body is deliberately closed
(`DisallowUnknownFields`, `api/rest/controller/replay/replay.go:113`), and
widening it would smuggle code overrides under the old attestation. New
capability, new endpoint, new authorization surface.

## CLI

- `caesium backtest --job <alias|id> --against last-30-runs [--image step=ref]
  [--command step='…'] [--set k=v] [--path candidate.job.yaml]
  [--ignore-output glob]… [--dry-run] [--json] [--format markdown]
  [--idempotency-key k] [--timeout d]` — create, poll to terminal (a client-side
  loop like `replay --diff`), render the matrix. `--json` writes clean stdout
  with logs on stderr (the repo's hard-learned rule); `--format markdown` emits
  the PR-comment body.
- `caesium backtest report <backtest-id> --job <id>` — re-render a stored report
  (the Action's comment step; also how humans re-attach after a timeout).
- Non-zero exit when any verdict is `changed`/`failed` unless `--allow-changes`
  (intentional behavior changes render as *expected*, still shown).

## CI Integration (roadmap §2.1)

The §2.1 Action grows a fourth chained step — **lint → diff → backtest → comment**:

```yaml
- uses: caesium-cloud/caesium-action@v1
  with:
    server: ${{ vars.CAESIUM_SERVER }}
    api-key: ${{ secrets.CAESIUM_API_KEY }}
    steps: lint,diff,backtest,comment
    backtest-against: last-30-runs
    backtest-image: transform=ghcr.io/corp/t:${{ github.sha }}
```

Auth reality: `AUTH_MODE` defaults to `none` (`pkg/env/env.go:169`). CI calling
a production Caesium API to execute containers must not hit an unauthenticated
server: the Action fails fast if auth is off; docs require `AUTH_MODE=api-key`
plus a least-privilege `backtest`-capability key in CI secrets — the §2.1
preview-run story's requirement, solved once there. The candidate image must
already be pushed (build & push → backtest). Cost guard: the Action posts the
`--dry-run` plan first and requires a `backtest` label or explicit re-run to
execute, so a busy repo doesn't burn 30 replays per push.

## Frontend (Caesium Console)

- **Backtest report view** (`/jobs/:id/backtests/:btid`): header (candidate
  digest vs baseline range, cost split), then a **run-matrix heat strip** — one
  cell per baseline run in date order: green unchanged / amber changed / red
  failed / grey skipped. Month-end clustering is visible at a glance.
- A cell drills into the per-run view: the output-delta table plus the existing
  **RunDiffView**, reused unchanged — baseline left, replay right.
- Job page gets a "Backtests" tab; a quarantined replay's run-detail page links
  back to its owning backtest.

## Safety

**Front and center: quarantine does not sandbox the container, and candidate
code is by definition unvetted.** Shipped replay's risk story leaned on two
facts — the code already ran in production once, and a human marked it
`replaySafe`. Backtest keeps the second and *loses the first*: a buggy or
malicious candidate image, running with the baseline's real mounts, secrets,
network egress, and workload identity, can write to the production warehouse 30
times. Caesium-internal suppression (cache/lineage/callbacks/metrics/SSE —
inherited, enforced at `internal/worker/runtime_executor.go:116-124` and the
`run.Store` metric gates) does nothing about external effects.

Layered posture, honest about which layers are enforcement and which are not:

1. **`replaySafe` remains the hard gate (enforced).** Baseline-recorded, shipped
   semantics: no `--force`, no retroactive grant, request bodies cannot clear
   quarantine — inherited verbatim.
2. **`backtestMode: readOnly` job attestation (not enforcement).** A job opts
   into candidate-code backtesting by declaring its steps read-from-sources /
   write-only-declared-outputs. Caesium records and displays this; it **cannot
   verify it** — a pipeline-owner policy statement, like `replaySafe` itself.
   Say exactly that in the docs.
3. **Network-policy guidance (deployment-level enforcement, not Caesium's).**
   Document running backtest replays under an egress profile that allows
   sources and blocks sinks. Caesium provides the hook (quarantined runs are
   identifiable), not the firewall.
4. **Authorization (enforced).** Overrides require a higher-privilege API-key
   capability than params-only replay; idempotent creation, bounded override
   sizes (mirroring the replay controller's caps), digest-resolved images only.
5. **Audit (enforced).** `Backtest.Overrides`, per-replay `DescriptorOverrides`,
   and candidate digests are durable; each replay's descriptor records the
   baseline it deviated from.

## Testing

Per the repo gate, every new CLI command and REST endpoint ships with a `test/`
integration test driving its real surface.

- Integration (`just integration-test`, distributed server): seed a `replaySafe`
  job, run it N times with varying params/outputs. P0 (no override): 100%
  unchanged, all cache-served; `--json` stdout clean and parseable via
  `runCLIStdout`, stderr separate. P1: image override to a container that emits
  different output for one param shape → exactly those runs `changed`; assert
  re-executed/cached counts, zero production cache writes, zero lineage rows, no
  metric drift (reuse the replay suppression assertions). Candidate exiting
  non-zero → `failed` verdict, non-zero CLI exit. A pre-`replaySafe` baseline
  and an expired cache entry both surface as `skipped` with reasons.
- Unit: override→hash plumbing (only the intended step's `HashInput` field
  changes; upstream hashes byte-identical), ignore-path globbing, fingerprint
  derivation, verdict classification, dry-run plan math.
- E2e (Playwright): report view, heat strip, RunDiffView drill-down, auth lane.

## Phasing

- **P0 — same-code backtest (no overrides).** N quarantined replays with an
  empty override set + the output-delta computation, report, and CLI. Zero new
  execution risk (exactly the shipped replay risk, N times); validates the
  aggregation/comparison/report plumbing; and it
  already catches **environment drift** — a moved unpinned tag, a rotated
  secret, or changed source data surface as changed/failed/ineligible runs.
  Independently useful: "is my pipeline deterministic over its recorded inputs?"
- **P1 — descriptor overrides.** Image (digest-resolved), command, and schema
  overrides with the safety gates above; the endpoint capability split;
  `--path candidate.job.yaml` delta extraction. This is the headline.
- **P2 — PR-comment integration.** The §2.1 Action step, `--format markdown`,
  the dry-run-then-label cost guard. Console heat strip lands alongside P1.

## Non-Goals

- **Not CI for data quality.** Backtest compares candidate vs baseline over
  *recorded history*; judging whether tonight's fresh data is sane is
  [`design-data-circuit-breaker.md`](design-data-circuit-breaker.md)'s problem.
- **Not a staging environment.** No environment redirection, synthetic data, or
  alternate namespaces (explicitly cut from replay v1; still cut here).
- **Not row/column dataset diffing.** Deltas are over typed step outputs and
  output-ref digests; value-level dataset diffs remain a Datafold/dbt handoff.
- **Not topology backtesting.** Added/removed steps have no recorded baseline
  inputs; that stays `job diff` + `dev --once` territory.
- **Not statistical tolerance in v1.** A delta is a delta; "within 0.1%"
  thresholds are open question 4, not a launch feature.

## Open Questions

1. Relax `replay.go:538` to trust durable baseline `TaskRun.Result`/`Output` for
   cache-enabled unchanged tasks whose `TaskCache` row expired? Decouples depth
   from cache TTL but touches replay's fail-closed proof rules — decide there.
2. Per-step param dependency contracts (so `--set` backtests stop paying
   full-DAG cost) — shared open item with replay v2.
3. A `backtestServiceAccount` / egress-profile override for k8s: genuine
   enforcement for safety layer 3, but breaks baseline workload identity —
   which does a backtest want?
4. Output tolerances (`row_count: ±0.5%`) and expected-change annotations.
5. Backfill interplay: `last-30-runs` on a backfill-heavy job may pick 30
   near-identical logical dates — need `--against distinct-params`?
6. Report retention: TTL-prune `Backtest` rows, or keep forever like runs?

## Related Documents

- [`design-quarantined-replay.md`](design-quarantined-replay.md) — the inherited
  safety model; authoritative for every invariant reused here.
- [`design-data-plane-memory.md`](design-data-plane-memory.md) — the substrate
  that recorded everything backtest replays.
- [`design-reproduce.md`](design-reproduce.md) — same descriptor substrate,
  single-task local reproduction; backtest is the N-run server-side counterpart.
- [`design-contract-enforcement.md`](design-contract-enforcement.md) — the
  *static* half of pre-merge safety; backtest is the *dynamic* half.
- [`design-data-circuit-breaker.md`](design-data-circuit-breaker.md) — runtime
  data-quality gating on fresh data; complementary, explicitly not this design.
- [`design-agent-in-the-loop.md`](design-agent-in-the-loop.md) — the agent can
  attach a backtest report as evidence when proposing a jobdef patch.
- [`roadmap.md`](roadmap.md) §2.1 — the PR-preview-runs Action this ships inside.
