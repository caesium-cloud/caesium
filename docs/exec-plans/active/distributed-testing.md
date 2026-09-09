# Distributed Testing and Performance Confidence

Last updated: 2026-09-09

Make a passing required CI result meaningful evidence that Caesium preserves
its backend guarantees, developer workflows, and Console behavior under normal
operation and supported failures, without exceeding an agreed performance
regression budget. This plan extends the existing tests; it does not replace
full engine coverage with smoke suites or claim that finite tests prove every
possible execution correct.

The user requested comprehensive testing improvements and a plan PR for review.
This is a proposed testing initiative, not an implementation or a change to
current branch protection. Runtime defect fixes discovered by these tests need
their own scoped change with a reproducing regression. New product capabilities,
new execution guarantees, resource right-sizing, and pipeline backtesting are
not implicitly authorized by this plan.

## Source-Of-Truth Note

The request above anchors intent. [AGENTS.md](../../../AGENTS.md) defines the
real-surface and containerized-verification requirements. The
[CI runbook](../../ci.md), [workflow](../../../.github/workflows/ci.yml), and
[execution operations guide](../../parallel-execution-operations.md) describe
existing wiring and operational contracts. Stream A must reconcile those
contracts with code before asserting stronger guarantees. A gap between desired
and implemented behavior must remain visible; neither weakening the checker nor
silently redefining a product guarantee is an acceptable resolution.

The [roadmap](../../roadmap.md) and [closed-loop arc](closed-loop-arc.md) retain
product priority and sibling ownership. This plan is cross-cutting test work,
not a new numbered phase of that arc. Sibling product changes remain sequenced
by their owning plans. The completed [Trust the Substrate](../completed/trust-the-substrate.md)
plan is historical context, not a backlog to reimplement.

### Findings verified while drafting

Inspected checkout: `97733090` on 2026-09-09; upstream was refreshed to
`9caf594b` before publication. Upstream also contains dataset operator work in
PR #442; refresh the selected base and sibling dashboards before execution.
No suite, live branch-protection
audit, or current runtime benchmark was performed as part of this inspection.

| Surface | Existing behavior and evidence | Gap this plan addresses |
| --- | --- | --- |
| Required CI | `scripts/ci-ok.py` rejects missing, failed, cancelled, and unexpectedly skipped required jobs; `test/shard_test.go` partitions the real suite. | `integration-extra`, Podman, Helm, and arm64 integration are outside the current aggregate. Promotion must be explicit and preserve coverage. |
| Distributed lane | `justfile` recipe `integration-up-distributed` starts one server with owner/worker mode enabled; `internal/run/failover_test.go` simulates lease expiry in a test DB. | These exercise distributed code paths but cannot demonstrate a real multi-node partition, quorum loss, or takeover after process failure. |
| Kubernetes lane | `helm/caesium/ci/test-values-k8s.yaml` selects one replica and disables persistence. | Persistent multi-node restart, replacement, and upgrade behavior are unproven by that topology. |
| UI | Vitest, live-backend Playwright, auth projects, and `ui/e2e/operator-flow.spec.ts` already exist. | `ui/playwright.config.ts` allows two CI retries; the workflow does not upload the generated browser diagnostics. Recovery, visual/accessibility, and scale coverage need expansion. |
| Performance | `test/load/harness.go` generates DAGs and reports run outcomes and metrics; `ui/scripts/check-bundle-size.mjs` budgets the largest JS chunk. | Failed run counts do not determine harness exit status. Sampling starts after a concurrency-limited submission loop, so it can miss much of the workload. There is no base/candidate regression gate or route-level total asset budget. |
| Coverage and generation | `just unit-test` runs root Go tests with race and coverage. Four native fuzz targets exist in three files; UI unit tests exist. | No sustained fuzz invocation or `Benchmark` functions were found. Coverage is not collected from the actual CLI/server exercised by integration and browser tests. |

These findings identify verification gaps, not proof that the affected product
paths are currently broken. Existing full suites and their expected engine
skips remain the starting point.

### Proposed guarantees to resolve in A1

Assign stable contract IDs and classify each as documented, inferred and needing
confirmation, or proposed product behavior. At minimum cover acknowledged-run
durability; one authoritative owner generation; stale completion rejection;
terminal state within an execution generation; frozen retry recipes; DAG and
fan-in rules; cancellation races; event delivery/deduplication; authorization;
and bounded recovery after the required quorum, capacity, and dependencies
return. State the fault and clock assumptions for each bound.

Separate duplicate task attempts, duplicate terminal commits, and duplicate
external effects. The operations guide already documents at-least-once event
delivery. Do not infer exactly-once arbitrary task side effects from scheduler
fencing. Timed-out operations may have committed. Linearizability applies only
to operations whose contract supports a sequential specification; asynchronous
progress and eventual delivery need separate checkers.

### Open decisions and external prerequisites

These questions were offered during brainstorming and remain unanswered. None
prevents drafting or the first-wave items. Implementations may proceed only
within their resolved contract and available test infrastructure.

| ID | Decision or prerequisite | Proposed direction and evidence required | Blocks |
| --- | --- | --- | --- |
| P1 | Required PR wall-clock and runner-cost budget | Measure current cold/warm CI, then agree a budget; 20–30 minutes is a proposal, not a measured commitment. Keep complete existing suites. | G3 gate promotion and G4 cadence/capacity. |
| P2 | Controlled performance and multi-host infrastructure | Three separate Caesium processes/containers on one CI host for PR robustness; isolated repeatable performance runners and separate hosts for broader qualification. Record provisioned runner identity, CPU/memory/storage, networking privileges, cleanup owner, and access proof. Provisioning paid infrastructure is not part of this plan PR. | E4 controlled calibration and F3/G4 multi-host operation. |
| P3 | Supported fault, execution, delivery, and recovery contracts | A1 produces the per-operation contract and selects the first conformance scenarios. Product-owner review resolves stronger or ambiguous guarantees with a recorded decision. A1 can finish with unresolved entries; dependent scenarios cannot claim those guarantees. | B2/B3, C1, D3, and F2 only for unresolved scenario contracts. |
| P4 | Supported OS/browser and release compatibility matrix | Confirm shipped CLI platforms, supported browsers, previous release(s), mixed-version operation, rollback versus backup restore, and storage failure model. Record exact release artifacts and documented policy. | D1/D2 expanded platform coverage and F2 upgrade/restore scenarios. |
| P5 | Performance SLOs and regression tolerances | E2/E3 supply workloads and repeated comparisons; E4 measures variance and records minimum samples, acceptable relative degradation, absolute SLOs, and bounded inconclusive handling. No arbitrary global percentage becomes a gate. | E4 sign-off and G3 performance promotion. |
| P6 | Repository settings and new gate promotion | Read current required checks/rulesets and permissions at execution time; record the selected merge-candidate strategy and settings owner. Apply settings only within the execution request's authorization. | G3 enforcement and any merge-queue activation. |

## Progress (as of 2026-09-09)

No implementation waves have shipped. All 22 items are unchecked. The plan PR
adds this document and proposed-work links in `docs/ci.md` and `docs/roadmap.md`.
It makes no claim that testing infrastructure or product behavior has changed.

The orchestrator owns this dashboard, merged PR links, candidate/merge SHAs,
verification artifacts, and blockers. An item checked in an unmerged PR is
proposed completion, not shipped evidence. Resume an unfinished wave before
assigning another wave number.

### Stream Status

| Stream | Scope | Priority | Status |
| --- | --- | --- | --- |
| A | Contracts and scenario evidence (2 items) | P0 | A1 ready; A2 depends on A1 |
| B | Real multi-node robustness (3 items) | P0 | Depends on A1; scenario contracts must resolve |
| C | Reference models, generated tests, and checker validation (3 items) | P0 | Depends on A1/B1 |
| D | Developer and Console journeys (3 items) | P0 | Depends on A1; expanded support needs P4 |
| E | Correct load reporting and performance comparison (4 items) | P0 | E1 ready; E4 needs P2/P5 |
| F | Upgrades, durability, and sustained faults (3 items) | P1 | F1 follows A1; F2/F3 need stated prerequisites |
| G | Diagnostics, coverage, and CI enforcement (4 items) | P0 | G1 ready; promotions follow verified capabilities |

## Streams

Paths prefixed with `new` below are proposed files/directories. Directory
ownership covers cohesive helpers and test fixtures within that directory; it
does not grant adjacent product edits. Every implementation item includes its
tests and required operating guidance in the same PR. Shared-file sequencing
below is mandatory, including append-only edits.

### Stream A — Contracts and evidence

- [ ] A1. Produce a bounded test-contract and harness-design memo.
  Files: `docs/exec-plans/active/distributed-testing.md` (contract/design decision record; documentation integrator).
  Depends on: none.
  Verify: update this plan's Source-Of-Truth decision record rather than creating a companion design document. Every proposed guarantee names its public operation, identity, acknowledgement point, supported fault/clock assumptions, expected safety/liveness result, evidence source, and unresolved decision. Map the first owner-crash and stale-owner tests through real persistence, dispatch, and read paths. The design names exact hook sites, transport routes including peer discovery, recorder protocol, readiness checks, and container build/run commands; compare Testcontainers/Toxiproxy plus Go checkers with a full Jepsen harness and record the choice. Record infrastructure and compatibility decisions without inventing support. This is an investigation deliverable, not permission to implement a new product guarantee.

- [ ] A2. Introduce a scenario manifest and a validator for complete evidence.
  Files: new `test/contracts/scenarios.json`, new `scripts/check-test-evidence.py`, new `scripts/test_test_evidence.py`, `docs/ci.md` (documentation integrator).
  Depends on: A1, G1.
  Verify: each included contract maps to named real-surface scenarios and required topology/mode/feature flags, expected observations, and allowed skips with reasons. Validate schema and duplicate IDs, then reject synthetic reports with missing scenarios, unexpected skips, disabled gates, wrong artifact identity, absent fault-activation evidence, or checker timeouts. Distinguish pass, fail, and inconclusive. The manifest must not contain rows marked proven for scenarios that do not yet exist; G3 reconciles discovery and execution against it.

### Stream B — Real multi-node robustness

- [ ] B1. Build an isolated three-node cluster harness with persistent state.
  Files: new `test/robustness/cluster/`, new `test/robustness/cluster_test.go`, new `scripts/robustness.sh`, new `build/Dockerfile.robustness`, `docs/ci.md` (documentation integrator), `go.mod`, `go.sum`.
  Depends on: A1.
  Verify: the containerized runner creates three independently addressable Caesium processes with separate volumes, observes three real dqlite members and a usable quorum, applies/triggers a job through HTTP/CLI, and observes real worker execution and durable results. Restart a node with its volume and rejoin it. Test owner=leader and owner!=leader placements; reject accidental single-node startup. Verify task/recorder reachability without localhost assumptions. Unique names, ports, image digests, and cleanup scope permit two harness instances to coexist. Compare candidate image provenance with the requested SHA; missing selected artifacts fail. Root module dependencies are added only when used by this harness.

- [ ] B2. Add observable fault control and independent history recording.
  Files: new `test/robustness/faults/`, new `test/robustness/history/`, new `test/robustness/recorder/`, new `test/robustness/faults_test.go`, new `internal/testfault/`, `internal/run/owner_manager.go`, `internal/run/store.go`, `internal/dispatch/dispatch.go`, `internal/event/bus_dispatch.go`, `internal/worker/runtime_executor.go`, `build/Dockerfile.robustness`, `docs/ci.md` (documentation integrator).
  Depends on: B1, C1.
  Verify: exercised kill, pause/resume, asymmetric partition, delayed/dropped response, and commit/dispatch boundary faults produce independent activation evidence and heal reliably. All targeted peer and worker paths traverse the injector, including advertised/discovered addresses. A recorder outside the faulted cluster retains raw attempt/effect records plus logical identities; it must not deduplicate away evidence. Record invocation/response events with a controller-owned ordering basis rather than comparing unsynchronized host clocks. A checker preserves possibly committed timeouts and treats recorder loss as inconclusive. Restrict hooks to A1's exact sites; fault controls are absent/inert in release builds and cannot mutate application records to manufacture outcomes. Verify baseline behavior without injected faults on the ordinary release image as well.

- [ ] B3. Ship the required core failure scenarios and complete histories.
  Files: new `test/robustness/core_test.go`, new `test/robustness/testdata/`, `test/contracts/scenarios.json` (created by A2), `docs/ci.md` (documentation integrator).
  Depends on: A2, B2.
  Verify: drive acknowledged-run crash/restart, owner pause beyond lease followed by stale completion, 2–1 split/heal, commit-before-response loss, durable-event-before-delivery crash, and cancel/retry/completion races through real surfaces. Include fan-out/fan-in and frozen retry recipes. Under the agreed contracts, assert accepted-state durability, authoritative generation, terminal outcomes, dependency policy, external attempt accounting, and bounded progress after healing. Neither permanently rejecting requests nor merely reaching a healthy endpoint passes. Persist every history, fault timeline, node log, topology, seed, and candidate digest; a named scenario whose fault never fired fails validation.

### Stream C — Generated state transitions and checker strength

- [ ] C1. Implement an independent reference model and generated lifecycle tests.
  Files: new `test/model/`, new `internal/run/model_properties_test.go`, new `internal/run/recovery_properties_test.go`, `docs/ci.md` (documentation integrator), `go.mod`, `go.sum`.
  Depends on: A1, B1.
  Verify: generate bounded DAGs and legal/illegal sequences of admission, completion, cancellation, retry, lease expiry, checkpoint, and recovery using Rapid. Minimize and retain failing cases. Use Porcupine only for contract operations with a valid sequential model; keep liveness and eventual-event checks separate. The reference transition rules must not call production decision functions. Check checkpoint/replay equivalence, partition accounting, and supported execution-mode equivalence. Include adversarial histories that the model must reject; these tests complement B3's real processes rather than stand in for them.

- [ ] C2. Expand native fuzzing and deterministic concurrency regressions.
  Files: `pkg/jobdef/schemacompat/fuzz_test.go`, `internal/jobdef/diff/fuzz_test.go`, `internal/trigger/cron/fuzz_test.go`, new `internal/run/descriptor_fuzz_test.go`, new `internal/run/recovery_fuzz_test.go`, new `internal/worker/renewal_synctest_test.go`, new `scripts/fuzz-tests.sh`, `docs/ci.md` (documentation integrator).
  Depends on: C1.
  Verify: the containerized script discovers/selects each intended fuzz target, performs a bounded exploration rather than seed-only execution, and preserves corpus artifacts. Persist minimized failures as normal regressions. Exercise isolated timer/cancellation/renewal logic with `testing/synctest` where supported; real sockets and CGO/dqlite remain outside its deterministic claim. Repeat selected concurrency tests with race detection and varied scheduling. No sleep-only success oracle or arbitrary valid-input rejection substitutes for a property.

- [ ] C3. Prove the checkers still detect known classes of defects.
  Files: new `test/model/oracle_regression_test.go`, new `test/model/testdata/`, new `scripts/validate-test-oracles.sh`, `docs/ci.md` (documentation integrator).
  Depends on: A2, B3, C2.
  Verify: lost acknowledged state, accepted stale generations, invalid fan-in, missing replay, and unaccounted external effects fail their respective checkers. Include legal duplicate delivery and ambiguous timeout histories that must not be falsely rejected. Reproduce selected historical defects or temporary intentional mutations in an isolated checkout with a recorded known-bad SHA/patch; the tests catch them and the fixed candidate passes. Never ship mutations or change the user's working tree to run this validation. Missing evidence and checker resource exhaustion cannot become green.

### Stream D — Developer and Console journeys

- [ ] D1. Extend binary-driven developer workflows and cleanup assertions.
  Files: `test/local_dev_test.go`, new `test/developer_journey_test.go`, new `test/developer_testdata/`, `docs/ci.md` (documentation integrator).
  Depends on: A1, C1.
  Verify: use the container-built release CLI in an empty temporary workspace for lint, preview, dev-once, watch/edit, interrupt, apply, and inspect. Check malformed input, paths with spaces, unavailable engines, cancellation/timeouts, and owned-resource cleanup. Parse JSON exclusively from stdout captured separately from stderr and assert exit status. Preserve existing helpers; testscript is an optional future harness substitution, not a reason to rewrite working tests. Run the currently shipped Linux architectures; add native platforms only after P4 confirms support. Inspect the job-definition reference before writing any YAML fixtures.

- [ ] D2. Expand functional, visual, accessibility, and scale browser coverage.
  Files: new `ui/e2e/accessibility.spec.ts`, new `ui/e2e/visual.spec.ts`, new `ui/e2e/scale.spec.ts`, new `ui/e2e/network-recovery.spec.ts`, new `ui/e2e/visual.spec.ts-snapshots/`, `ui/e2e/helpers/fixtures.ts`, `ui/package.json`, `ui/package-lock.json`, `ui/playwright.config.ts`, `docs/ci.md` (documentation integrator).
  Depends on: A1, G1.
  Verify: against the live backend, check create/apply or existing authoring workflows, trigger, logs, failure diagnosis, retry/cancel, reload, permission denial, credential expiry, reconnect, and stale-request races. Require keyboard/focus behavior and scoped axe checks. Review deterministic screenshots with fixed fonts, viewport, timestamps, and dataset. Load large DAGs, many partitions, paginated history, and long logs; assert all expected data remains reachable through virtualization/pagination. Fail on unexpected console/page errors. Chromium remains required; P4 selects additional supported browser projects. Synthetic response manipulation tests are explicitly labeled and do not replace live persistence tests.

- [ ] D3. Exercise the operator journey across actual cluster failure.
  Files: new `ui/e2e/cluster-recovery.spec.ts`, new `ui/e2e/helpers/cluster.ts`, `ui/playwright.config.ts`, `test/contracts/scenarios.json` (created by A2), `docs/ci.md` (documentation integrator).
  Depends on: A2, B3, D2.
  Verify: trigger from the Console, observe a run, fault its owner, reconnect through the supported entry point, and confirm the UI converges on the independently checked durable outcome and retained logs. Exercise both authenticated permissions and event-stream recovery. Observe the fault while the browser is connected; an API-only scenario with a final screenshot is insufficient. Reject duplicate/stale rows and false terminal success. Require available data to remain inspectable after reload.

### Stream E — Performance with correctness

- [ ] E1. Make the existing load harness report and exit honestly.
  Files: `test/load/harness.go`, new `test/load/harness_test.go`, `justfile` (load-test recipe only), `docs/ci.md` (documentation integrator).
  Depends on: none.
  Verify: reject invalid/zero configuration, use an overall deadline, sample metrics concurrently with submission, and return failure when expected runs fail, time out, cannot be triggered, or required samples are missing. Emit versioned machine-readable results with exact expected/observed counts and a usable failure classification. A live successful workload exits zero; deliberate bad-image and unavailable-server workloads exit nonzero. A controlled slow workload proves samples cover early and middle execution, including concurrency=1. Validate the reporter with fixtures as well as the live runs. Preserve useful existing human reports and execute the recipe inside the repository's containerized toolchain.

- [ ] E2. Build representative workloads and measure actual lifecycle intervals.
  Files: new `test/performance/`, new `test/performance/workloads.json`, new `test/performance/load.js`, `docs/ci.md` (documentation integrator).
  Depends on: A1, E1.
  Verify: extend/reuse E1's result schema for tiny and realistic tasks, wide/deep DAGs, fan-out, queue saturation, cache hit/miss, API reads, event subscribers, and backlog drain. Use k6 arrival-rate traffic for open-loop pressure and account for dropped offered work; track admitted, rejected, completed, and remaining work separately. Record scheduling intervals from real observed events; mark unavailable intervals explicitly instead of inventing timestamps. Collect existing metrics and external CPU/RSS/I/O/FD/goroutine/container observations where available. Measure statements separately from rows. Every admitted run is reconciled; faster admission with a growing backlog is not improvement. Per-task production resource telemetry remains owned by resource-right-sizing.

- [ ] E3. Compare base and candidate across backend and browser workloads.
  Files: new `scripts/compare-performance.py`, new `scripts/test_compare_performance.py`, new `scripts/performance.sh`, new `internal/run/owner_benchmark_test.go`, new `internal/run/recovery_benchmark_test.go`, new `ui/e2e/performance.spec.ts`, `ui/scripts/check-bundle-size.mjs`, `docs/ci.md` (documentation integrator).
  Depends on: E2, D2, C2.
  Verify: build base and candidate through the same containerized toolchain and run release-equivalent uninstrumented images. Pin workload/data and settings, isolate competing load, interleave repeated runs, separate cold/warm cases, and record image/CLI/toolchain/host provenance. Report benchstat comparisons for targeted hot paths and distributions for system metrics. Browser comparisons include route readiness, action-to-render, long-session memory, and total route assets, so splitting a large chunk cannot evade the budget. All completion/error checks must pass before speed is compared. Test the comparator against known faster/slower/noisy/undersampled results and mismatched environments; it must not accept missing data or call every insignificant difference equivalent.

- [ ] E4. Calibrate and approve enforceable performance budgets.
  Files: new `test/performance/budgets.json`, new `test/performance/baseline.json`, `scripts/compare-performance.py` (created by E3), `scripts/test_compare_performance.py` (created by E3), `docs/ci.md` (documentation integrator).
  Depends on: E3.
  Verify: satisfy P2/P5 with repeated same-code control runs on the chosen runner, sufficient tail samples, and a reviewed workload-specific decision rule. Record absolute SLOs, bounded relative degradation, uncertainty/non-inferiority method, aggregation/multiple-comparison policy, and minimum sample sizes. Pass only when evidence establishes the allowed bound and correctness/SLOs pass; fail material regression; classify inadequate evidence as inconclusive, with a bounded rerun policy that blocks the strict gate if unresolved. Keep both a target-base comparison and a versioned fixed baseline to expose cumulative regression. Intentional budget changes require visible rationale and review; neutral performance is valid and optimization claims must identify tradeoffs. No promotion before calibration evidence exists.

### Stream F — Compatibility, durability, and sustained operation

- [ ] F1. Resolve the upgrade and storage qualification matrix.
  Files: `docs/exec-plans/active/distributed-testing.md` (lifecycle decision record; documentation integrator).
  Depends on: A1.
  Verify: add the lifecycle decision record to this plan rather than creating a separate design. Audit existing migrations, release artifacts, chart persistence, membership/replacement procedures, and backup/restore support. Record the exact supported old-to-new paths and allowed rollback/restore behavior, fixture-generation mechanism, retained-run/queue/event cases, and failure assumptions. Distinguish process kill, OS/power loss, lost unflushed writes, and actual disk loss. Deliver the P4 decisions and exact F2/F3 test procedure; unsupported backup/rollback capabilities become explicit external product prerequisites, not runnable placeholder tests.

- [ ] F2. Qualify persistent restart and supported upgrades under work.
  Files: new `test/lifecycle/`, new `test/lifecycle/versions.json`, new `scripts/lifecycle-tests.sh`, new `helm/caesium/ci/test-values-lifecycle.yaml`, `docs/ci.md` (documentation integrator).
  Depends on: F1, B3.
  Verify: create state through the old release's real API/CLI, retain volumes with active/queued runs and history, upgrade to the candidate, and reconcile state and execution outcomes through current public reads and the independent recorder. Test mixed versions only where P4/F1 permits them. Verify supported rollback or backup restore on isolated volumes, membership replacement, and snapshot catch-up. Use actual release digests and a persistent multi-node chart; template rendering alone is insufficient. Any unavailable prerequisite blocks the affected qualification rather than silently skipping it.

- [ ] F3. Add seeded sustained faults and multi-host qualification.
  Files: new `test/robustness/exploratory_test.go`, new `test/robustness/workloads/`, new `test/chaos/`, new `scripts/soak-tests.sh`, `docs/ci.md` (documentation integrator).
  Depends on: B3, C3, E4, F2.
  Verify: after P2 provisioning, run bounded seeded fault/workload sequences on separate hosts and repeatable single-host exploratory jobs. Chaos Mesh or the A1-selected equivalent injects Kubernetes partitions/latency/bandwidth plus explicitly supported storage, clock, and resource faults. Combine slow consumers, queue overload, retention, repeated failover, and node replacement. Check safety throughout and progress after healing; verify resource use stabilizes after drain and owned containers/FDs do not leak. Retain actual fault schedules as well as seeds because OS scheduling is not fully reproducible. No process-kill test is labeled power-loss qualification. A short version of every soak scenario must be locally reproducible before scheduled execution.

### Stream G — Diagnostics, coverage, and enforceable CI

- [ ] G1. Preserve first-attempt failures and actionable existing diagnostics.
  Files: `ui/playwright.config.ts`, `.github/workflows/ci.yml`, `scripts/test_ci.py`, `docs/ci.md`.
  Depends on: none.
  Verify: use Playwright's supported fail-on-flaky behavior so retries collect evidence without erasing initial failure; demonstrate with a temporary controlled fail-once test that the job fails and retains its trace. Upload reports/screenshots/video/server logs on failure and always retain structured outcome counts, including setup failures. Keep upload failures visible without hiding the original test result. Regression-check workflow wiring. Any quarantine has an issue, owner, expiry, and explicit confidence impact; no automatic quarantine or reduced suite floor is introduced.

- [ ] G2. Collect actual CLI/server integration coverage with provenance.
  Files: new `build/Dockerfile.coverage`, new `scripts/integration-coverage.sh`, new `scripts/check-coverage.py`, new `scripts/test_coverage.py`, `docs/ci.md` (documentation integrator).
  Depends on: A2, G1.
  Verify: use Go coverage-instrumented binaries built in containers, explicit package selection and `GOCOVERDIR`, and merge compatible profiles from CLI, server, and live browser journeys. Collect on graceful shutdown and provide explicit flushing where required; killed-process or missing profiles are incomplete evidence, not zero coverage or success. Label profile provenance, separate unit/integration/browser contributions, and report uncovered changed paths and critical contract gaps. Set package/diff ratchets after measuring a baseline instead of requiring a vanity global percentage. Demonstrate coverage of a real request-to-write-to-read path. Keep coverage/fault instrumentation out of performance artifacts and audit the separate `reagents/go.mod` scope when relevant.

- [ ] G3. Wire and promote the required PR and merge-candidate gates.
  Files: `.github/workflows/ci.yml`, `.github/actions/run-integration/action.yml`, `build/ci.docker-bake.hcl`, `build/Dockerfile.integration`, `justfile`, `scripts/ci-ok.py`, `scripts/test_ci.py`, `scripts/integration-test.sh`, `test/shard_test.go`, `test/contracts/scenarios.json` (created by A2), `docs/ci.md` (documentation integrator).
  Depends on: B3, C3, D3, E4, F2, G2.
  Verify: satisfy P1/P6, then add explicit recipes and artifact producers/consumers for the completed robustness, generated, journey, performance, lifecycle, and coverage capabilities. Account for the new test packages: the current precompiled integration binary covers only `./test`, not its new subpackages. Test selectors on backend/UI/fixture/schema/dependency/build/workflow-only changes and prove required scenarios execute. Preserve all existing engine/architecture suite coverage and existing aggregate fail-closed behavior. Audit optional distributed/owner-memory/Podman/Helm/arm64 integration promotion using actual stability evidence and explicit decisions; unpromoted coverage stays outside the claimed PR guarantee. Test the prospective merge commit via an agreed up-to-date-base or merge-queue policy; if using a queue, wire its event and verify required check names on that event. Reconcile workflow, manifest, docs, and live required settings. Unexpected skip, missing artifact, inconclusive required evidence, cancelled dependency, or insufficient performance evidence blocks. Do not claim enforcement until settings are verified and the actual candidate has passed.

- [ ] G4. Schedule broader qualification and publish the operating procedure.
  Files: `.github/workflows/ci.yml`, new `.github/workflows/testing-qualification.yml`, `scripts/test_ci.py`, `docs/ci.md` (documentation integrator).
  Depends on: F3, G3.
  Verify: schedule longer fuzz/seed campaigns, broader browsers, multi-host faults, and soaks with P1/P2 capacity limits, owned cleanup, evidence retention, and issue-triage responsibility. Require supported release qualification in the actual publish dependency chain or through verified same-SHA evidence; tag-time execution must not publish first and test later. Demonstrate a scheduled/manual run and a failing qualification that blocks the release path. Nightly success is broader evidence, not retroactive proof for every PR. Document one-command reproduction, flake/inconclusive handling, baseline changes, known limitations, and how feature owners add scenarios without editing multiple conflicting sources of truth.

## Sequencing & Dependencies

### First wave and ordering

**First dependency-ready candidates: A1, E1, G1.** Their implementation files
are disjoint; the documentation integrator serializes their plan/runbook updates
as described below.
A1 is bounded investigation, E1 repairs existing measurement behavior,
and G1 repairs existing CI evidence; none requires an unanswered product choice.
Do not launch implementation merely because this plan has been drafted.

After A1, E2/F1 can advance when their own dependencies are met; A2 also
needs G1. The core distributed path is `A1 -> B1 -> C1 -> B2 -> B3`;
D1 and C2 follow C1, while D2
follows A1/G1. C3 combines B3/C2; D3 combines B3/D2. Performance proceeds
`E1 -> E2 -> E3 -> E4` with D2/C2 prerequisites at E3. F2 combines F1/B3; G2
combines A2/G1. G3 joins the verified required capabilities; F3 and G4 extend
them to broader qualification. Dependency references in the item blocks are
authoritative. A prerequisite is ready only after its deliverable is merged and
its relevant decisions are resolved; stacked work needs explicit execution scope.

### One writer per overlapping surface

| Surface | Owner and serialized order |
| --- | --- |
| Plan dashboard and cross-plan handoffs | Orchestrator; item workers only update assigned checkboxes/evidence. |
| Root `go.mod`/`go.sum` | B1 introduces its used harness dependencies; C1 adds its used model libraries after B1. Any later dependency requires an explicit ownership reassignment and toolchain-generated sums. |
| Runtime hooks/completion/recovery/event composition | B2 only, at A1's enumerated sites. C writes separate test files. Product fixes and sibling edits must merge first or be separately assigned; B2 is not authority to redesign execution. |
| Scenario manifest | A2 creates; B3 fills its completed scenarios; D3 extends after B3; G3 owns final discovery/gate reconciliation. Other streams supply entries to that writer. |
| `justfile` | E1 owns only the existing load recipe; G3 later owns new lane recipes. All new harness scripts must already execute useful work before a recipe is added. |
| Workflow and `scripts/test_ci.py` | G1 -> G3 -> G4. Feature streams supply tested commands to G3, not concurrent workflow edits. |
| Plan and `docs/ci.md` content | One documentation integrator appointed by the orchestrator writes each PR's required doc patch. A1 -> F1 for contract/lifecycle decisions; G1 -> A2 -> G2 -> G3 -> G4 for evidence/gating guidance; E1 -> E2 -> E3 -> E4 for performance guidance. Serialize all doc patches at PR assembly, refresh against the latest merged document, and resolve overlap before the next patch; stream workers do not concurrently edit the runbook. |
| UI dependencies/config/shared fixtures | G1 config -> D2 dependencies/config/fixtures -> D3 config. E3 uses separate browser performance tests after D2. |
| Performance result/schema/docs | E1 -> E2 -> E3 -> E4. G3/G4 consume the established schema and budgets. |
| Lifecycle artifacts/docs | F1 contract -> F2 harness -> F3 broader qualification. |

### Sibling ownership and handoff conditions

- [Resource right-sizing](resource-right-sizing.md) owns production per-task
  resource capture, OOM semantics, resource fields, engine adapters, and its
  recommendation/UI work. E2 uses existing metrics and external observations;
  it does not create those fields or a second telemetry pipeline. Adding its
  new product scenarios requires a merged sibling SHA and verified public
  schema/engine behavior. This is an optional extension, not a prerequisite
  for E2's baseline workloads.
- [Backtesting](backtesting.md) owns replay-over-history and proposal verification
  as product features. This plan's base/candidate system benchmark does not
  implement that feature or execute production data and credentials.
- [Window scheduling](window-scheduling.md) owns window/deadline policy and
  predictor semantics. Test existing queue/priority behavior without promising
  starvation freedom or new deadline behavior. New window scenarios await that
  plan's merged contracts.
- [Data circuit breaker](data-circuit-breaker.md) owns its operator UI and
  incident/freshness integration. Refresh PR #442 and subsequent wave status
  at the execution base; reuse merged scenarios rather than recreating them.
  B2 and any sibling writer of `internal/run/store.go`, runtime execution,
  event/metrics wiring, UI shared files, or CI must have a recorded serialized
  merge order. A local checked box is not sufficient handoff evidence.

### Documentation consolidation

This initiative has **one execution plan: this file**, and uses the existing
**`docs/ci.md` runbook** for shipped testing commands, guarantees, troubleshooting,
and enforcement. Fold A1/F1 design decisions into this plan. Do not create
per-stream testing plans, design memos, Console/developer guides, or separate
performance/coverage/robustness Markdown documents. Each stream supplies its
required runbook change to the one documentation integrator, and that patch
lands with the implementation PR. The integrator serializes patches and owns
conflict resolution; parallel appends are not considered safe automatically.

Versioned scenario definitions, performance budgets, release matrices, and the
fixed machine-readable baseline live with their tests. Raw reports, profiles,
logs, and timing campaigns are CI artifacts linked from Progress, not new
`docs/load-baseline-*` or dated report documents. `docs/load-testing-history.md`
already consolidates earlier measurements and remains an explicitly historical
record, not a second current testing guide.

The product plans for backtesting, right-sizing, window scheduling, and the data
circuit breaker have distinct implementation contracts and unshipped work; this
plan consumes their merged interfaces without copying those backlogs. When an
actual duplicate testing instruction is encountered during execution, move its
unique content into the runbook, update inbound links, and remove the superseded
copy in the same PR. Preserve historical completion evidence and stable item
identities when consolidating any existing plan.

### Deferred or optional scope

Full deterministic simulation of CGO/dqlite, a standalone Jepsen port, formal
TLA+/TLC specifications, testscript migration, unsupported native platforms,
and new production telemetry/backup capabilities are deferred. A1 can recommend
a separately scoped follow-up with a concrete design deliverable. Do not add
empty runnable items or count these as completed coverage. F3/G4 are included
later scope with external prerequisites, not optional shortcuts around G3.

## Verification (Run For Every PR)

For this plan-only PR: validate template sections, unique item IDs, all
dependencies and acyclicity, Files paths (existing or explicitly new), ownership
ordering, relative Markdown targets/anchors, absence of template tokens, and
`git diff --check`. Review the new plan against the actual workflow and active
sibling ownership. No application suite result is implied by documentation-only
CI skips.

For implementation PRs changing runtime Go, API/CLI, DB, dependencies, build,
or test wiring, use the repository baseline from the candidate checkout:

```sh
just lint
just unit-test
just integration-test
```

Root tests do not replace compiling/running integration-tagged packages or the
separate reagent module. Add the relevant existing lane commands:

| Changed behavior | Additional existing checks |
| --- | --- |
| Owner/worker/distributed | `just integration-test-distributed`, `just integration-test-owner-memory`, plus B's actual multi-node scenarios |
| CLI journeys | Binary-driven integration scenarios, separate stdout/stderr, static CLI smoke on each supported release architecture |
| UI | `just ui-lint`, `just ui-test`, `just ui-e2e`; auth changes also `just ui-e2e-auth` |
| Auth/permissions/agent surface | `just integration-test-agent` and relevant live browser authorization cases |
| Podman | `just integration-test-podman` and affected scenarios; do not reduce full existing suite coverage |
| Helm/Kubernetes | `just helm-lint`, `just helm-template`, and the actual kind/Helm integration procedure in CI with the scenario's required replica/persistence settings |
| Reagents | `just reagents-lint`, `just reagents-test`, `just integration-test-infra` as affected |
| Workflow/filter/gate | `actionlint .github/workflows/ci.yml` and `python3 -m unittest discover -s scripts -p 'test_ci.py' -v`; lint any new workflow too, run new validator tests, and prove real lane selection |

New commands in G3 are **proposed**, not available today. Before G3, each stream
documents and tests its containerized entry script in its own PR, including
exact image inputs and exit conditions. Run focused new tests plus the relevant
baseline; a unit pass alone cannot prove a cluster/browser scenario.

Build through `just` and the repository Dockerfiles. Use current candidate
artifacts, never stale tags or an unverified precompiled runner. Enable feature
gates on both the server and test runner wherever required. Honor the job YAML
reference. No new CLI command or REST query is planned; any separately approved
addition requires binary/HTTP coverage in `test/` under AGENTS.md.

Shared Docker image tags, fixed container names, sockets, and ports remain
serialized across existing integration/UI/Helm lanes. Coordinate with existing
test owners and retain the established PID-first `/tmp/caesium-lane.lock` for
the full shared lane command. New harness isolation must be proven before
relaxing serialization. Operate only on owned test resources; no production
fault injection. Keep fault controls and coverage instrumentation out of normal
release/performance builds, and verify that exclusion. Record SHA, worktree,
commands, exit statuses, topology/config, and artifact paths without secrets.

Treat missing infrastructure, incomplete histories, timeouts in a checker, and
insufficient samples as blocked/inconclusive evidence. Do not retry indefinitely,
delete assertions, lower floors, or reinterpret a setup failure as a pass.
Revalidate after conflict resolution or changes that invalidate the tested SHA.

## Acceptance Criteria

1. **Contract coverage:** A1/A2 provide reviewed, separately identified safety,
   liveness, compatibility, and performance contracts; every claimed required
   guarantee has discovered and executed real-surface scenarios. Open P1–P6
   decisions remain visible until resolved with evidence.
2. **Distributed correctness:** B3 runs on three actual members with persistent
   volumes, demonstrates each named fault, and passes independent history/effect
   checks for owner loss, stale owners, partition/heal, ambiguous responses,
   event replay, and concurrent lifecycle actions. Quorum loss cannot pass by
   stalling indefinitely; recovery bounds state their preconditions.
3. **Generated coverage:** C1/C2 exercise generated state sequences and sustained
   fuzzing, preserve minimized regressions, and report reproducibility limits.
   C3 demonstrably rejects selected known-bad histories/implementations while
   accepting contract-permitted duplicate and uncertain outcomes.
4. **Developer experience:** D1 proves the shipped CLI workflow, machine-output
   contract, invalid-input behavior, watch/interrupt handling, and cleanup with
   actual binaries on the agreed platform matrix.
5. **Console correctness:** D2/D3 prove live operator/auth workflows, keyboard
   accessibility and reviewed visuals, large-data usability, and convergence
   after a real backend fault. Browser retry cannot erase first-attempt failure.
6. **Performance:** E1–E4 reconcile offered/admitted/completed work, collect the
   full workload interval, compare identified base/candidate release artifacts,
   and enforce calibrated absolute/relative budgets with explicit uncertainty.
   Known regressions and insufficient evidence cannot yield a passing strict
   gate. Fixed-baseline trends expose cumulative degradation.
7. **Durability and lifecycle:** F1/F2 qualify every selected supported upgrade,
   persistent restart/replacement, and rollback/restore path against actual
   release artifacts and public reads. Unsupported capabilities are recorded
   prerequisites rather than invented or silently skipped.
8. **Broader qualification:** F3/G4 demonstrate bounded exploratory and sustained
   runs on the provisioned topology, including separate hosts, retained failure
   schedules, post-drain resource checks, and a reproducible short scenario.
9. **Evidence integrity:** G1/G2 retain first-attempt browser diagnostics and
   real CLI/server coverage with candidate provenance. Manifest validation fails
   on wrong topology, unexpected skips, missing instrumentation/evidence, or
   absent faults. Coverage reporting does not contaminate performance results.
10. **Actual enforcement:** G3/G4 prove the selected required checks on the real
    merge candidate and block publication on missing/failed same-SHA release
    qualification. Workflow, branch settings, scenario inventory, runbook, and
    Progress agree. Preserve existing full engine/architecture coverage and
    label unpromoted lanes honestly. Nightly results are not represented as
    evidence from an individual PR.

## How To Pick Up Work

1. Read this plan, its contracts, and the applicable `AGENTS.md`.
2. Select unchecked, included items whose dependencies are verified ready.
   Start with A1, E1, and G1; record P1–P6 resolution as it becomes available.
3. Use an assigned worktree and branch from the verified base. Keep changes
   within the stream's file ownership and include its tests and behavior docs.
4. Run the required verification on the actual candidate changes. Record what
   passed, failed, or could not run, including actual scenario execution.
5. Update only assigned item checkboxes and notes. The wave orchestrator owns
   Progress and records completion from verified merged PRs.
6. Follow the requested publication endpoint. Implementation PR titles use
   `<Imperative subject> (distributed-testing W<n>-<Greek stream suffix>)`.

Use `$exec-plan-wave` with this plan's path to orchestrate a wave in Codex.
Use `$draft-exec-plan` to revise the plan without starting implementation.

## Cross-References

- [CI runbook](../../ci.md), [roadmap](../../roadmap.md), and
  [execution operations](../../parallel-execution-operations.md).
- [Historical load measurements](../../load-testing-history.md): useful workload
  history, not a current performance baseline.
- [Getting started](../../getting-started.md) and
  [job-definition reference](../../caesium-job-llm-reference.md): developer-journey
  and fixture contracts.
- [Trust the Substrate](../completed/trust-the-substrate.md),
  [closed-loop arc](closed-loop-arc.md), [data circuit breaker](data-circuit-breaker.md),
  [resource right-sizing](resource-right-sizing.md), [backtesting](backtesting.md),
  and [window scheduling](window-scheduling.md): prior work and sibling ownership.
- [Native drafting skill](../../../.codex/skills/draft-exec-plan/SKILL.md) and
  [native execution skill](../../../.codex/skills/exec-plan-wave/SKILL.md).

Open source references researched for this proposal (2026-09-09); pin and verify
versions when adding dependencies, and keep them test-only where possible:

| Reference | Intended use and limit |
| --- | --- |
| [Jepsen](https://jepsen.io/services/analysis) and [etcd robustness](https://github.com/etcd-io/etcd/blob/main/tests/robustness/README.md) | Contract-based fault testing, histories, and regression validation; full Jepsen adoption is a later option. |
| [Testcontainers Go](https://golang.testcontainers.org/) and [Toxiproxy](https://github.com/Shopify/toxiproxy) | Isolated container lifecycle and controlled TCP faults; verify that discovered peer routes actually traverse the injector. |
| [Porcupine](https://github.com/anishathalye/porcupine) | Check histories against small sequential models; not a blanket checker for asynchronous DAG liveness. |
| [Rapid](https://pkg.go.dev/pgregory.net/rapid) and [Go fuzzing](https://go.dev/doc/security/fuzz/) | Generated state sequences, properties, and retained failing inputs. |
| [Go synctest](https://go.dev/blog/testing-time) and [gofail](https://github.com/etcd-io/gofail) | Isolated timer tests and a fault-hook design reference; real networking/CGO is not automatically deterministic. |
| [testscript](https://pkg.go.dev/github.com/rogpeppe/go-internal/testscript) | Optional isolated CLI fixture framework; existing binary helpers remain valid. |
| [Playwright accessibility](https://playwright.dev/docs/accessibility-testing), [visual assertions](https://playwright.dev/docs/api/class-pageassertions), and [flaky-test CLI behavior](https://playwright.dev/docs/test-cli) | Extend the existing browser stack with axe, selected reviewed screenshots, and honest retry outcomes. |
| [k6 arrival-rate models](https://grafana.com/docs/k6/latest/using-k6/scenarios/concepts/open-vs-closed/) and [benchstat](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat) | Offered-load control and repeated benchmark comparisons; statistical insignificance alone is not a non-regression guarantee. |
| [Go integration coverage](https://go.dev/doc/build-cover) | Instrument actual CLI/server execution and merge compatible profiles; graceful collection and killed-process incompleteness need handling. |
| [Chaos Mesh network faults](https://chaos-mesh.org/docs/simulate-network-chaos-on-kubernetes/) | Kubernetes partition/latency/loss/bandwidth scenarios on a provisioned qualification cluster. |
