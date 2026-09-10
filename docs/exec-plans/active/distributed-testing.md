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

This plan follows the native `exec-plan-wave` structure: Progress records merged
evidence, Streams owns the backlog, and Sequencing defines dispatch readiness.

## Source-Of-Truth Note

The request above anchors intent. [AGENTS.md](../../../AGENTS.md) defines the
real-surface and containerized-verification requirements. The
[CI runbook](../../ci.md), [workflow](../../../.github/workflows/ci.yml), and
[execution operations guide](../../parallel-execution-operations.md) describe
existing wiring and operational contracts. The contract record lives in this
file under Strategic Decisions; A1 and F1 refine it without introducing another
design document. Stream A must reconcile those contracts with code before
asserting stronger guarantees. A gap between desired and implemented behavior
must remain visible; neither weakening the checker nor
silently redefining a product guarantee is an acceptable resolution.

The [roadmap](../../roadmap.md) and [closed-loop arc](closed-loop-arc.md) retain
product priority and sibling ownership. This plan is cross-cutting test work,
not a new numbered phase of that arc. Sibling product changes remain sequenced
by their owning plans. The completed [Trust the Substrate](../completed/trust-the-substrate.md)
plan is historical context, not a backlog to reimplement.

## Strategic Decisions

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
| Kubernetes lane | CI selects one replica without persistence. The existing `just k8s-distributed` recipe deploys three replicas through the Helm StatefulSet and headless discovery, but uses a Docker Desktop registry path and disables persistence. CI already provisions kind. | Reuse the chart and CI kind lifecycle with three replicas and persistent volumes first; do not run the local recipe unchanged against an arbitrary user cluster. |
| Release CLI | `scripts/ci-cli-smoke.sh` already tests the static CLI natively on amd64/arm64; publication checks its checksum markers. | D1 and release qualification extend these existing gates with complete developer/upgrade journeys. |
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
return. A1 must also resolve behavior **during quorum loss**: measure request
cancellation and dqlite retry behavior, then record which mutation endpoints
return a bounded error and the deadline/status contract. Until that is reviewed,
bounded server rejection is explicitly undefined: client timeout alone does
not prove server cancellation or a rejected write. B3 records unresolved writes
as possibly committed and cannot claim bounded rejection for that operation. State the fault and clock assumptions for each bound.

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
| Q1 | Required PR wall-clock and runner-cost budget | Measure current cold/warm CI, then agree the full matrix budget; 20–30 minutes is a proposal, not a measured commitment. The early G5 gate uses existing hosted runners and reports incremental cost. Keep complete existing suites. | G6 full-matrix promotion and G4 cadence/capacity. |
| Q2 | Controlled performance and multi-host infrastructure | Three separate Caesium processes/containers on one CI host for PR robustness; isolated repeatable performance runners and separate hosts for broader qualification. Record provisioned runner identity, CPU/memory/storage, networking privileges, cleanup owner, and access proof. Provisioning paid infrastructure is not part of this plan PR. | E4 controlled calibration and F3/G4 multi-host operation. |
| Q3 | Supported fault, execution, delivery, and recovery contracts | A1 produces the per-operation contract and selects the first conformance scenarios. Product-owner review resolves stronger or ambiguous guarantees with a recorded decision. A1 can finish with unresolved entries; dependent scenarios cannot claim those guarantees. | B2/B3, C1, D3, and F2 only for unresolved scenario contracts. |
| Q4 | Supported OS/browser and release compatibility matrix | Confirm shipped CLI platforms, supported browsers, previous release(s), mixed-version operation, rollback versus backup restore, and storage failure model. Record exact release artifacts and documented policy. | D1/D2 expanded platforms and F4/F2 upgrade/restore scenarios. |
| Q5 | Performance SLOs and regression tolerances | E2/E3 supply workloads and repeated comparisons; E4 measures variance and records minimum samples, acceptable relative degradation, absolute SLOs, and bounded inconclusive handling. No arbitrary global percentage becomes a gate. | E4 sign-off and G6 performance promotion. |
| Q6 | Repository settings and new gate promotion | Read current required checks/rulesets and permissions at execution time; record the selected merge-candidate strategy and settings owner. Apply settings only within the execution request's authorization. | G5 enforcement and G7 candidate/queue policy. |

## Progress (as of 2026-09-09)

No implementation waves have shipped. All 27 implementation items are unchecked.
N-1 below is a mandatory wave-close documentation checkpoint, not another
implementation item or a new agent role. The plan PR
adds this document and proposed-work links in `docs/ci.md`, `docs/roadmap.md`,
and `docs/README.md`. Review revisions preserve existing item IDs; E5, F4, and
G5–G7 add early guards and split the former G3 scope.
It makes no claim that testing infrastructure or product behavior has changed.

The orchestrator owns this dashboard, merged PR links, candidate/merge SHAs,
verification artifacts, and blockers. An item checked in an unmerged PR is
proposed completion, not shipped evidence. Resume an unfinished wave before
assigning another wave number.

### Stream Status

| Stream | Scope | Priority | Status |
| --- | --- | --- | --- |
| A | Contracts and scenario evidence (2 items) | P0 | A1 ready; A2 follows A1/G1 |
| B | Real multi-node robustness (3 items) | P0 | B1 follows A1 and ships the first pod-kill regression |
| C | Reference models, generated tests, and checker validation (3 items) | P0 | C1 follows A1; pure models stay in the unit lane |
| D | Developer and Console journeys (3 items) | P0 | Depends on A1; expanded support needs Q4 |
| E | Correct load reporting and performance comparison (5 items) | P0 | E1 ready; E5 is an early count guard; E4 needs Q2/Q5 |
| F | Upgrades, durability, and sustained faults (4 items) | P1 | F4 follows F1 without B3; F2 adds cluster qualification |
| G | Diagnostics, coverage, and CI enforcement (7 items) | P0 | G1 ready; G3/G5 establish the early required gate |

## Streams

Paths prefixed with `new` below are proposed files/directories. Directory
ownership covers cohesive helpers and test fixtures within that directory; it
does not grant adjacent product edits. Each implementation PR includes its tests and exact invocation/evidence in
its PR description and assigned plan notes. N-1 synchronizes the shared runbook
once the wave's implementation PRs have merged; the wave is not complete before
that sync. Any separately scoped product behavior fix still includes its
required product documentation in the implementation PR. Shared-file sequencing
below is mandatory, including append-only edits.

### Stream A — Contracts and evidence

- [ ] A1. Resolve the first fault contracts and choose the smallest usable harness
  Files: `docs/exec-plans/active/distributed-testing.md` (A1 Strategic Decisions record only).
  Depends on: none.
  Verify: Name each public operation, identity, acknowledgement point, safety/liveness oracle, fault/clock assumptions, and unresolved guarantee, including rejection while quorum is absent. First compare the existing Helm StatefulSet/headless membership plus CI kind setup with Testcontainers/Toxiproxy; the default B1 design reuses the chart, enables persistence, and kills a pod. Record exact image, node/owner discovery, test runner, and recorder reachability commands. A bounded spike must establish whether peer address advertisement permits proxies before adopting them. Compare a small injectable clock seam in lease/owner/worker renewal with existing Go timer test facilities, short test-only lease configuration, and external process pause; a clock seam does not replace real crash/commit faults. Enumerate any indispensable event-dispatch hook and sibling handoff, rather than assigning five product files speculatively. B1's supported owner-crash contract can be settled independently of the later partition/clock/storage envelope; retain unresolved Q1–Q6 entries.

- [ ] A2. Introduce a scenario manifest and a validator for complete evidence.
  Files: new `test/contracts/scenarios.json`, new `scripts/check-test-evidence.py`, new `scripts/test_test_evidence.py`.
  Depends on: A1, G1.
  Verify: each included contract maps to named real-surface scenarios and required topology/mode/feature flags, expected observations, and allowed skips with reasons. Validate schema and duplicate IDs, then reject synthetic reports with missing scenarios, unexpected skips, disabled gates, wrong artifact identity, absent fault-activation evidence, or checker timeouts. Distinguish pass, fail, and inconclusive. The manifest must not contain rows marked proven for scenarios that do not yet exist; G3 reconciles the early gate and G6 the full suite.

### Stream B — Real multi-node robustness

- [ ] B1. Reuse the Helm topology and ship the first real owner-crash regression
  Files: new `test/robustness/cluster/`, new `test/robustness/owner_crash_test.go`, new `test/robustness/recorder/`, new `scripts/robustness.sh`, new `build/Dockerfile.robustness`, new `helm/caesium/ci/test-values-robustness.yaml`.
  Depends on: A1.
  Verify: Use the existing chart and CI kind lifecycle with replicaCount=3 and persistence enabled, candidate images loaded into an owned cluster, and unique namespaces/volumes. Do not invoke the Docker Desktop registry recipe unchanged or alter a user's current cluster. Put Docker/Kubernetes-dependent Go drivers and their helpers behind `//go:build integration`; compile their runner in the container toolchain. Observe three real dqlite members and a quorum, apply/trigger a blocked multi-step job through HTTP/CLI, identify its actual owner, terminate that pod without graceful application shutdown, and observe a surviving owner finish the accepted run. Exercise owner=leader and owner!=leader, then restart/rejoin the old pod with its retained volume. Use a small HTTP effect sink hosted by the test runner outside the faulted pods; persist raw starts/completions and verify public final state. This is a complete named regression, not scaffolding waiting for C1/B2/B3. Reject accidental single-node startup, missing task/sink connectivity, absent kill evidence, and wrong candidate image digests. Demonstrate two isolated harness instances coexist and clean up only owned resources; no Testcontainers dependency is required for this first topology.

- [ ] B2. Add targeted faults and correlate public event and effect histories
  Files: new `test/robustness/faults/`, new `test/robustness/history/`, new `test/robustness/faults_test.go`, `test/robustness/recorder/` (created by B1), new `internal/testfault/`, `internal/event/bus_dispatch.go` (only A1-approved dispatch hook; EX-HOOKS required), `build/Dockerfile.robustness` (created by B1).
  Depends on: B1, C1; external EX-HOOKS for the instrumented event case.
  Verify: Add external pause/resume, asymmetric network partition, and delayed/dropped-response control with independent activation/heal evidence. Verify discovered peer/worker addresses traverse the injector. Extend B1's test-runner recorder with an SSE subscriber using the existing `/v1/events` surface; retain duplicates and compare reconnection results with persisted public event reads using the documented sequence scope, not an assumed gap-free global counter. SSE is implementation-produced evidence and cannot reveal an external effect whose completion event was lost; preserve the separate raw task-effect ledger. Keep possibly committed timeouts and controller-observed ordering; recorder loss is inconclusive. Limit product instrumentation to an A1-justified durable-event-before-publication boundary in `internal/event/bus_dispatch.go`, with test-only controls absent/inert in release builds. Do not edit `internal/run/store.go`, owner completion, dispatch handlers, or worker runtime speculatively. Before dispatching the hook work, EX-HOOKS must identify the actual merged sibling base and exclusive file owner; otherwise that case is blocked, never skipped as passing. Run the ordinary release image without faults as a baseline.

- [ ] B3. Extend the core failure suite with fenced recovery, dispatch, and authorization cases
  Files: new `test/robustness/core_test.go`, new `test/robustness/testdata/`, `test/contracts/scenarios.json` (created by A2).
  Depends on: A2, B2, G3.
  Verify: Extend B1's owner-crash regression with owner pause past lease/stale completion, 2–1 split/heal, commit-before-response loss, durable-event-before-delivery crash, and cancel/retry/completion races, including fan-out/fan-in and frozen retry recipes. Make a worker unreachable, observe network-error rejection and peer benching through actual dispatch progress and available `caesium_dispatch_rejected_total`/`caesium_dispatch_stalled_total` metrics, then prove it receives new work after the cooldown. Send wrong-token `/internal/dispatch` and `/internal/complete` requests between real processes; cover valid-token stale-generation refusal separately and invalid peer credentials where mTLS is supported. Assert denied operations have no task/state/effect mutation. During quorum loss, enforce A1's bounded error contract only where resolved; retain uncertain writes and do not equate client timeout with rejection. Check accepted-state durability, generation authority, legal terminal/dependency behavior, raw external attempts, and recovery after healing. Persist histories/fault timelines/topology/digests; missing fault activation or stalling cannot pass.

### Stream C — Generated state transitions and checker strength

- [ ] C1. Implement independent pure models and generated lifecycle tests
  Files: new `test/model/`, new `internal/run/model_properties_test.go`, new `internal/run/recovery_properties_test.go`, `go.mod`, `go.sum`.
  Depends on: A1.
  Verify: Generate bounded DAGs and sequences of admission, completion, cancellation, retry, lease expiry, checkpoint, and recovery using Rapid; minimize and retain failures. The independent `test/model` package is deliberately untagged, pure Go, and has no cluster/client startup, Docker socket, real network, or product decision-function dependency, so it belongs in `just unit-test`. Any test that launches external infrastructure must instead be integration-tagged and run by the system runner. Use Porcupine only for valid sequential contracts, with separate liveness/event models. Check checkpoint/replay equivalence and partition accounting with hermetic fixtures; compare real execution modes in B3 rather than claiming the pure model proves wiring. C1 owns its used model dependencies; adding a future proxy/container library needs a separately assigned owner.

- [ ] C2. Expand native fuzzing and deterministic concurrency regressions.
  Files: `pkg/jobdef/schemacompat/fuzz_test.go`, `internal/jobdef/diff/fuzz_test.go`, `internal/trigger/cron/fuzz_test.go`, new `internal/run/descriptor_fuzz_test.go`, new `internal/run/recovery_fuzz_test.go`, new `internal/worker/renewal_synctest_test.go`, new `scripts/fuzz-tests.sh`.
  Depends on: C1.
  Verify: the containerized script discovers/selects each intended fuzz target, performs a bounded exploration rather than seed-only execution, and preserves corpus artifacts. Persist minimized failures as normal regressions. Exercise isolated timer/cancellation/renewal logic with `testing/synctest` where supported; real sockets and CGO/dqlite remain outside its deterministic claim. Repeat selected concurrency tests with race detection and varied scheduling. No sleep-only success oracle or arbitrary valid-input rejection substitutes for a property.

- [ ] C3. Prove the checkers still detect known classes of defects.
  Files: new `test/model/oracle_regression_test.go`, new `test/model/testdata/`, new `scripts/validate-test-oracles.sh`.
  Depends on: A2, B3, C2.
  Verify: lost acknowledged state, accepted stale generations, invalid fan-in, missing replay, and unaccounted external effects fail their respective checkers. Include legal duplicate delivery and ambiguous timeout histories that must not be falsely rejected. Reproduce selected historical defects or temporary intentional mutations in an isolated checkout with a recorded known-bad SHA/patch; the tests catch them and the fixed candidate passes. Never ship mutations or change the user's working tree to run this validation. Missing evidence and checker resource exhaustion cannot become green.

### Stream D — Developer and Console journeys

- [ ] D1. Extend binary-driven developer workflows and cleanup assertions.
  Files: `test/local_dev_test.go`, new `test/developer_journey_test.go`, new `test/developer_testdata/`.
  Depends on: A1.
  Verify: use the container-built release CLI in an empty temporary workspace for lint, preview, dev-once, watch/edit, interrupt, apply, and inspect. Check malformed input, paths with spaces, unavailable engines, cancellation/timeouts, and owned-resource cleanup. Parse JSON exclusively from stdout captured separately from stderr and assert exit status. Preserve existing helpers; testscript is an optional future harness substitution, not a reason to rewrite working tests. Run the currently shipped Linux architectures; add native platforms only after Q4 confirms support. Inspect the job-definition reference before writing any YAML fixtures.

- [ ] D2. Expand functional, visual, accessibility, and scale browser coverage.
  Files: new `ui/e2e/accessibility.spec.ts`, new `ui/e2e/visual.spec.ts`, new `ui/e2e/scale.spec.ts`, new `ui/e2e/network-recovery.spec.ts`, new `ui/e2e/visual.spec.ts-snapshots/`, `ui/e2e/helpers/fixtures.ts`, `ui/package.json`, `ui/package-lock.json`, `ui/playwright.config.ts`.
  Depends on: A1, G1.
  Verify: against the live backend, check create/apply or existing authoring workflows, trigger, logs, failure diagnosis, retry/cancel, reload, permission denial, credential expiry, reconnect, and stale-request races. Require keyboard/focus behavior and scoped axe checks. Review deterministic screenshots with fixed fonts, viewport, timestamps, and dataset. Load large DAGs, many partitions, paginated history, and long logs; assert all expected data remains reachable through virtualization/pagination. Fail on unexpected console/page errors. Chromium remains required; Q4 selects additional supported browser projects. Synthetic response manipulation tests are explicitly labeled and do not replace live persistence tests.

- [ ] D3. Exercise the operator journey across actual cluster failure.
  Files: new `ui/e2e/cluster-recovery.spec.ts`, new `ui/e2e/helpers/cluster.ts`, `ui/playwright.config.ts`, `test/contracts/scenarios.json` (created by A2).
  Depends on: A2, B3, D2.
  Verify: trigger from the Console, observe a run, fault its owner, reconnect through the supported entry point, and confirm the UI converges on the independently checked durable outcome and retained logs. Exercise both authenticated permissions and event-stream recovery. Observe the fault while the browser is connected; an API-only scenario with a final screenshot is insufficient. Reject duplicate/stale rows and false terminal success. Require available data to remain inspectable after reload.

### Stream E — Performance with correctness

- [ ] E1. Make the existing load harness report and exit honestly.
  Files: `test/load/harness.go`, new `test/load/harness_test.go`, `justfile` (load-test recipe only).
  Depends on: none.
  Verify: reject invalid/zero configuration, use an overall deadline, sample metrics concurrently with submission, and return failure when expected runs fail, time out, cannot be triggered, or required samples are missing. Emit versioned machine-readable results with exact expected/observed counts and a usable failure classification. A live successful workload exits zero; deliberate bad-image and unavailable-server workloads exit nonzero. A controlled slow workload proves samples cover early and middle execution, including concurrency=1. Validate the reporter with fixtures as well as the live runs. Preserve useful existing human reports and execute the recipe inside the repository's containerized toolchain.

- [ ] E2. Extend the Go load driver with open-loop workloads and lifecycle measurements
  Files: `test/load/harness.go`, `test/load/harness_test.go` (created by E1), new `test/performance/workloads.json`, new `test/performance/load_test.go`.
  Depends on: A1, E1.
  Verify: Extend the containerized Go harness E1 already owns instead of introducing k6 or an unowned toolchain dependency. Support arrival-rate scheduling independent of completion, with offered/dropped/admitted/rejected/completed/backlog counts and bounded overload handling. Cover tiny/realistic tasks, wide/deep DAGs, fan-out, queues, cache hit/miss, API reads, subscribers, and drain. Tag the live driver `//go:build integration`; keep pure scheduling/report tests hermetic. Measure actual lifecycle intervals from observed events, marking unavailable values explicitly. Collect existing metrics and external resource observations, separating statements from rows and timed background work. Reconcile every admitted run; faster admission with backlog growth is not improvement. Production per-task resource telemetry remains owned by right-sizing.

- [ ] E3. Compare base and candidate across backend and browser workloads.
  Files: new `scripts/compare-performance.py`, new `scripts/test_compare_performance.py`, new `scripts/performance.sh`, new `internal/run/owner_benchmark_test.go`, new `internal/run/recovery_benchmark_test.go`, new `ui/e2e/performance.spec.ts`, `ui/scripts/check-bundle-size.mjs`.
  Depends on: E2, D2, C2.
  Verify: build base and candidate through the same containerized toolchain and run release-equivalent uninstrumented images. Pin workload/data and settings, isolate competing load, interleave repeated runs, separate cold/warm cases, and record image/CLI/toolchain/host provenance. Report benchstat comparisons for targeted hot paths and distributions for system metrics. Browser comparisons include route readiness, action-to-render, long-session memory, and total route assets, so splitting a large chunk cannot evade the budget. All completion/error checks must pass before speed is compared. Test the comparator against known faster/slower/noisy/undersampled results and mismatched environments; it must not accept missing data or call every insignificant difference equivalent.

- [ ] E4. Calibrate and approve enforceable performance budgets.
  Files: new `test/performance/budgets.json`, new `test/performance/baseline.json`, `scripts/compare-performance.py` (created by E3), `scripts/test_compare_performance.py` (created by E3).
  Depends on: E3.
  Verify: satisfy Q2/Q5 with repeated same-code control runs on the chosen runner, sufficient tail samples, and a reviewed workload-specific decision rule. Record absolute SLOs, bounded relative degradation, uncertainty/non-inferiority method, aggregation/multiple-comparison policy, and minimum sample sizes. Pass only when evidence establishes the allowed bound and correctness/SLOs pass; fail material regression; classify inadequate evidence as inconclusive, with a bounded rerun policy that blocks the strict gate if unresolved. Keep both a target-base comparison and a versioned fixed baseline to expose cumulative regression. Intentional budget changes require visible rationale and review; neutral performance is valid and optimization claims must identify tradeoffs. No promotion before calibration evidence exists.

- [ ] E5. Add an early SQL-work budget inside the existing integration suite
  Files: new `test/statement_budget_test.go`, new `test/statement_budget_testdata/`.
  Depends on: E1.
  Verify: Run fixed successful workloads through real HTTP/CLI and assert per-category deltas of the existing `caesium_db_statements_total` and `caesium_db_writes_total`. The tagged scenario is discovered by the current `IntegrationTestSuite` and sharding, so it gates with the existing required integration lane before E4. Isolate the server/workload and use bounded, workload-driven categories; timer-driven lease renewals, replay, and unrelated traffic are not assumed deterministic. Establish a repeatable count baseline and explicit justified tolerance on existing CI runners; where a category cannot be isolated, report that limitation rather than imposing a false exact bound. Demonstrate detection of an instrumented extra-statement/lost-batching mutation while completion counts still match. This guards the measured SQL-work classes and needs neither paid runners nor timing SLO decisions Q2/Q5; it does not prove latency or cover uninstrumented queries.

### Stream F — Compatibility, durability, and sustained operation

- [ ] F1. Resolve the upgrade and storage qualification matrix.
  Files: `docs/exec-plans/active/distributed-testing.md` (F1 decision record only).
  Depends on: A1.
  Verify: add the lifecycle decision record to this plan rather than creating a separate design. Audit existing migrations, release artifacts, chart persistence, membership/replacement procedures, and backup/restore support. Record the exact supported old-to-new paths and allowed rollback/restore behavior, fixture-generation mechanism, retained-run/queue/event cases, and failure assumptions. Distinguish process kill, OS/power loss, lost unflushed writes, and actual disk loss. Deliver the Q4 decisions and exact F2/F3 test procedure; unsupported backup/rollback capabilities become explicit external product prerequisites, not runnable placeholder tests.

- [ ] F2. Qualify persistent cluster upgrades, replacement, and supported recovery paths
  Files: new `test/lifecycle/cluster_test.go`, `test/lifecycle/versions.json` (created by F4), `scripts/lifecycle-tests.sh` (created by F4), new `helm/caesium/ci/test-values-lifecycle.yaml`.
  Depends on: F1, F4, B3.
  Verify: Extend F4's known release fixtures to three persistent members. Upgrade with active/queued work, reconcile history and raw effects, and test mixed versions only where Q4/F1 permits them. Exercise supported rollback or backup restore on isolated volumes, membership replacement, and snapshot catch-up. Use actual release digests and the chart, retain real HTTP/CLI observations, and fail or block unavailable prerequisites explicitly. Single-node migration qualification is already delivered by F4 and does not wait for this item.

- [ ] F3. Add seeded sustained faults and multi-host qualification.
  Files: new `test/robustness/exploratory_test.go`, new `test/robustness/workloads/`, new `test/chaos/`, new `scripts/soak-tests.sh`.
  Depends on: B3, C3, E4, F2.
  Verify: after Q2 provisioning, run bounded seeded fault/workload sequences on separate hosts and repeatable single-host exploratory jobs. Chaos Mesh or the A1-selected equivalent injects Kubernetes partitions/latency/bandwidth plus explicitly supported storage, clock, and resource faults. Combine slow consumers, queue overload, retention, repeated failover, and node replacement. Check safety throughout and progress after healing; verify resource use stabilizes after drain and owned containers/FDs do not leak. Retain actual fault schedules as well as seeds because OS scheduling is not fully reproducible. No process-kill test is labeled power-loss qualification. A short version of every soak scenario must be locally reproducible before scheduled execution.

- [ ] F4. Ship single-node previous-release upgrade qualification independently
  Files: new `test/lifecycle/standalone_test.go`, new `test/lifecycle/versions.json`, new `scripts/lifecycle-tests.sh`.
  Depends on: F1.
  Verify: With Q4's supported version pair, start the previous release on an owned persistent volume, create jobs/history/queued work through its public surface, stop it, and start the candidate on that same volume. Assert migrated/publicly readable state and resumed eligible work; also test a failed/unsupported transition according to F1's policy. Use integration-tagged tests and the existing container toolchain. Reuse versioned release artifacts and existing amd64/arm64 CLI checksum smoke evidence; do not require a three-node cluster, B3, or new paid infrastructure. Emit same-SHA qualification evidence and a direct containerized command; G6 later wires this standalone runner into the declared CI/release matrix.

### Stream G — Diagnostics, coverage, and enforceable CI

- [ ] G1. Preserve first-attempt failures and discover all Python validator tests
  Files: `ui/playwright.config.ts`, `.github/workflows/ci.yml`, `scripts/test_ci.py`.
  Depends on: none.
  Verify: Make browser retries retain diagnostic evidence without erasing initial failure; a temporary controlled fail-once scenario must fail and upload its trace. Upload reports/screenshots/video/server logs including setup failure, and retain structured outcomes. In the same PR widen ci-config discovery from `test_ci.py` to `test_*.py` and assert that command in `scripts/test_ci.py`; a temporary extra matching test that fails must fail ci-config. This ensures A2's `test_test_evidence.py`, E3's `test_compare_performance.py`, and G2's `test_coverage.py` run immediately when introduced. Keep upload errors visible without masking the original failure; no automatic quarantine, reduced floors, or erased first attempts.

- [ ] G2. Collect actual CLI/server integration coverage with provenance.
  Files: new `build/Dockerfile.coverage`, new `scripts/integration-coverage.sh`, new `scripts/check-coverage.py`, new `scripts/test_coverage.py`.
  Depends on: A2, G1.
  Verify: use Go coverage-instrumented binaries built in containers, explicit package selection and `GOCOVERDIR`, and merge compatible profiles from CLI, server, and live browser journeys. Collect on graceful shutdown and provide explicit flushing where required; killed-process or missing profiles are incomplete evidence, not zero coverage or success. Label profile provenance, separate unit/integration/browser contributions, and report uncovered changed paths and critical contract gaps. Set package/diff ratchets after measuring a baseline instead of requiring a vanity global percentage. Demonstrate coverage of a real request-to-write-to-read path. Keep coverage/fault instrumentation out of performance artifacts and audit the separate `reagents/go.mod` scope when relevant.

- [ ] G3. Wire the first persistent three-node lane and its exact scenario selectors
  Files: `.github/workflows/ci.yml`, `.github/actions/run-integration/action.yml`, `build/ci.docker-bake.hcl`, `justfile`, `scripts/test_ci.py`, `test/contracts/scenarios.json` (created by A2).
  Depends on: A2, B1, E5, G1.
  Verify: Add the B1 runner/image/recipe and artifact consumers using the existing kind and Helm infrastructure. Register the owner-crash scenario and E5's existing-suite budget with exact topology, candidate digest, and scenario identity. A new `test/robustness` binary must be explicitly built with integration tags; the existing `./test` precompiled binary cannot contain its subpackage tests. Verify selectors for code, fixtures, chart, dependency, image/build, and workflow changes, and a missing artifact/scenario failure. Preserve complete existing engine/architecture suites. Ship an executable lane and selector tests; promotion/settings/merge policy are separate G5/G7 work, so G3 does not wait for full B3, E4, F2, or G2.

- [ ] G4. Schedule broader qualification and enforce release evidence
  Files: `.github/workflows/ci.yml`, new `.github/workflows/testing-qualification.yml`, `scripts/test_ci.py`.
  Depends on: F3, G6.
  Verify: Schedule longer seed/fuzz campaigns, browsers, multi-host faults, and soaks within Q1/Q2 limits, with cleanup, retained evidence, and triage ownership. Retain current native CLI checksum/smoke gates. Require supported release qualification in the actual publish chain or verified same-SHA evidence; demonstrate a failed qualification blocks publication. Run scheduled/manual campaigns and record one-command reproduction and explicit limits for N-1's runbook sync. Nightly results are broader evidence, not retroactive proof for every PR.

- [ ] G5. Promote the minimum credible gate into the fail-closed aggregate
  Files: `.github/workflows/ci.yml`, `scripts/ci-ok.py`, `scripts/test_ci.py`.
  Depends on: G3.
  Verify: On the existing hosted runner, require B1's demonstrated three-member owner-crash lane alongside E5's SQL-work budget and G1's honest browser outcomes. Record measured added duration and repeated stability without waiting for controlled-runner timing calibration or the full exploratory suite. Audit current required checks/rulesets under Q6 and verify that ci-ok is actually enforced; change settings only within execution authorization. Test failed/cancelled/skipped/missing new dependencies and require exact scenario/fault evidence. Existing optional mode/engine lanes retain their honest status until explicitly promoted. This milestone provides narrow documented confidence, not full partition/upgrade/performance equivalence.

- [ ] G6. Wire and qualify the remaining complete system suites
  Files: `.github/workflows/ci.yml`, `.github/actions/run-integration/action.yml`, `build/ci.docker-bake.hcl`, `build/Dockerfile.integration`, `justfile`, `scripts/ci-ok.py`, `scripts/test_ci.py`, `scripts/integration-test.sh`, `test/shard_test.go`, `test/contracts/scenarios.json` (created by A2).
  Depends on: B3, C3, D3, E4, F2, G2, G7.
  Verify: Extend G3/G5's functioning lane pattern to generated tests, full faults/journeys, standalone and cluster lifecycle runners, real coverage, and calibrated performance. Explicitly compile/run every new integration subpackage and validate fixture/config/path selection. Under Q1/Q2/Q5, measure capacity and audit promotion of the existing distributed/owner-memory/Podman/Helm/arm64 integration lanes; retain all real-surface coverage and document unpromoted limits. Fail closed on missing artifacts, unexpected skips, unexecuted scenarios, or inconclusive performance. Do not bundle new merge-policy machinery here; use G7's verified candidate identity.

- [ ] G7. Verify the merge-candidate policy independently of suite expansion
  Files: `.github/workflows/ci.yml`, `scripts/ci-ok.py`, `scripts/test_ci.py`.
  Depends on: G5.
  Verify: Resolve Q6's up-to-date-base versus merge-queue choice from current repository settings and test the actual prospective merge commit. If activating a queue, wire merge_group and prove producers, selectors, and required context names on that event; otherwise verify the chosen base-freshness enforcement. Bind evidence to the tested candidate SHA and test stale/missing candidate refusal. Record permissions/settings evidence and measured implications for Q1; no paid performance or multi-host infrastructure is a prerequisite.

## Sequencing & Dependencies

### First wave, early milestone, and ordering

**First dependency-ready candidates: A1, E1, G1.** Their Files lists are
pairwise disjoint. A1 resolves the first conformance scenario, E1 fixes existing
measurement behavior, and G1 makes browser results and Python discovery honest.
None requires controlled performance runners or a fully specified failure model.
Do not launch implementation merely because this plan has been drafted.

**Minimum credible gate target: the end of wave two.** After the foundation
wave, B1 ships a three-replica persistent kind/Helm pod-kill regression, E5 ships
a repeatable SQL-work budget in the existing integration suite, and A2 supplies
scenario/evidence validation. G3 then wires the new runner; G5 promotes the
validated early lane into the enforced aggregate. G3 and G5 are sequential
merge steps within that wave, not extra standalone waves or work that waits
for B2/B3/C3/E4/F2/G2. N-1 closes both waves. Missing correctness or setting
proof blocks this milestone; the target is not a promise to fit unmeasured CI
time. This gate proves a narrow owner-crash/SQL-work/browser-outcome envelope,
not all partitions, side effects, latency bounds, or upgrades.

C1 can follow A1 independently of B1; C2 follows C1. B2 needs B1/C1 and the
explicit hook handoff; B3 needs B2 plus G3 to serialize scenario-manifest edits.
D1 follows A1; D2 follows A1/G1; D3 follows B3/D2. F4 follows F1 without B3,
while F2 adds the cluster cases after F4/B3. E2/E3/E4 form the later timing
comparison path; E5 does not wait for it. G7 follows G5 and resolves candidate
identity. G6 joins the complete fault/model/journey/lifecycle/coverage and
calibrated performance capabilities after G7. F3/G4 add broader qualification.

Item dependency fields are authoritative. Existing IDs retain their identity:
G3 now owns initial lane wiring, G5 owns early promotion, G7 owns candidate
policy, and G6 owns the later matrix expansion carved out of the original G3.
A prerequisite is ready only after its deliverable is merged and its applicable
decisions are resolved. Stacked work needs explicit execution scope.

### One writer per overlapping surface

| Surface | Owner and serialized order |
| --- | --- |
| Plan decision records/dashboard | A1 then F1 own their assigned Strategic Decisions text; the native orchestrator owns Progress and reconciles item evidence. |
| Root `go.mod`/`go.sum` | C1 adds only its used model libraries. B1 reuses chart/CLI infrastructure. A later proxy/container dependency requires an explicit owner and toolchain-generated sums. |
| Runtime fault seam | B2 owns only A1's enumerated event-dispatch boundary after EX-HOOKS. No blanket ownership of run store, owner completion, dispatch handlers, or worker runtime. Any clock refactor requires a separately scoped plan amendment and sibling handoff. |
| Scenario manifest | A2 -> G3 -> B3 -> D3 -> G6. Other streams supply completed scenario entries to the next assigned writer. |
| `justfile` | E1 owns only the load recipe; G3 owns first-cluster recipes; G6 owns later lane recipes. All scripts execute useful work before recipes are added. |
| Workflow and `scripts/test_ci.py` | G1 -> G3 -> G5 -> G7 -> G6 -> G4. A2/E3/G2 add separate Python test files that G1's wildcard discovers; they do not edit the workflow. |
| `scripts/ci-ok.py` | G5 -> G7 -> G6; each preserves fail-closed behavior on the current required set. |
| `docs/ci.md`, documentation index/roadmap links | Reserved exclusively to N-1 at each wave close. No implementation stream lists the runbook in its Files. |
| UI config/dependencies/shared fixtures | G1 config -> D2 -> D3. E3 uses separate browser performance tests after D2. |
| Load implementation/results | E1 -> E2 -> E3 -> E4. E5 uses separate integration fixtures and measured counters after E1. |
| Cluster harness/recorder | B1 -> B2 -> B3; F2/F3 and D3 consume the completed harness and own separate scenario files. |
| Lifecycle runner/version matrix | F4 creates -> F2 extends -> F3 consumes. |

### N-1 — Wave-close runbook synchronization (coordination checkpoint)

N-1 is a step of each wave's native orchestrator, not a fictitious agent role,
an independently dispatchable placeholder, or an additional implementation
stream. Its sole file scope is `docs/ci.md`, relevant `docs/README.md` and
`docs/roadmap.md` links, and this plan's Progress. Once the wave's implementation
PRs are merged, the orchestrator authors one docs-only sync PR from that merged
base, recording the actual commands, outcomes, guarantees, limits, and evidence.
Land it after those PRs and before closing the wave or dispatching the next wave.
Record its PR and merge SHA as `W<n>/N-1` in Progress.

Until then, the current runbook continues to describe the prior completed wave;
implementation PRs provide their executable commands and evidence in their own
bodies/assigned notes. Do not claim docs/Progress agree with newly merged work
before N-1 completes. Required product behavior docs for an actual bug fix stay
with that product PR; N-1 consolidates shared testing/CI operations only.

### Sibling ownership and handoff conditions

**EX-HOOKS (B2 dispatch prerequisite):** A1 records the precise event-publication
hook and the owner of `internal/event/bus_dispatch.go` at the execution base.
Before allocating B2's instrumented case, verify the merged SHA/PR of any
intersecting [closed-loop arc](closed-loop-arc.md) work and record exclusive
ownership for the B2 wave, or record that no sibling has an active claim after
checking its current plan/PR. The prior draft's speculative run-store/worker
edits are removed. An unmerged sibling checkbox or invented future wave number
is not a valid handoff. A1 must amend the plan before any additional product
hook or clock-seam file is allowed.

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
  B2's event hook and any sibling event/metrics, UI shared-file, or CI writer
  must have a recorded serialized merge order. A local checked box is not sufficient handoff evidence.

### Documentation consolidation

This initiative has **one execution plan: this file**, and uses the existing
**`docs/ci.md` runbook** for shipped testing commands, guarantees, troubleshooting,
and enforcement. A1/F1 keep design decisions here. No per-stream testing plans,
design memos, Console/developer guides, or performance/coverage/robustness
Markdown documents are introduced. N-1 alone synchronizes the runbook after
each wave; this deliberately replaces the earlier contradictory promise that
every parallel implementation PR would also edit the same runbook.

Versioned scenarios, budgets, release matrices, and the machine-readable fixed
baseline live with their tests. Raw reports/profiles/logs are CI artifacts,
not dated Markdown files. `docs/load-testing-history.md` remains an explicitly
historical record, not a second current testing guide. Distinct product plans
retain their unshipped contracts instead of duplicating them here. N-1 folds
actual duplicated testing instructions into the runbook, updates inbound links,
and removes superseded copies while preserving unique historical evidence.

### Deferred or optional scope

Full deterministic simulation of CGO/dqlite, a bespoke Docker cluster before
reuse of the existing Helm path, a standalone Jepsen port, formal
TLA+/TLC specifications, testscript migration, unsupported native platforms,
k6 (the Go harness supplies open-loop traffic), a production clock-seam refactor
unless A1 justifies and scopes it, and new production telemetry/backup
capabilities are deferred. A1 can recommend a separately scoped follow-up with
a concrete design deliverable. Do not add
empty runnable items or count these as completed coverage. F3/G4 are included
later scope with external prerequisites, not substitutes for the early G5 gate.

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
separate reagent module. Every new Go file that launches or requires Docker,
Kubernetes, a live server, or fault infrastructure under `test/robustness/`,
`test/lifecycle/`, `test/chaos/`, or `test/performance/` must carry
`//go:build integration`, including live helpers and recorder drivers. Pure
reference models under `test/model/` and pure report/math tests remain untagged
and must not initialize external infrastructure. Compile the new system runners
explicitly with `-tags=integration` inside the builder; the current precompiled
`./test` runner does not discover subpackages. Verify ordinary `go test ./...`
from the unit container requires no cluster, and the selected tagged runners
actually execute their named system tests. Add the relevant existing checks:

| Changed behavior | Additional existing checks |
| --- | --- |
| Owner/worker/distributed | `just integration-test-distributed`, `just integration-test-owner-memory`, plus B's actual multi-node scenarios |
| CLI journeys | Binary-driven integration scenarios, separate stdout/stderr, static CLI smoke on each supported release architecture |
| UI | `just ui-lint`, `just ui-test`, `just ui-e2e`; auth changes also `just ui-e2e-auth` |
| Auth/permissions/agent surface | `just integration-test-agent` and relevant live browser authorization cases |
| Podman | `just integration-test-podman` and affected scenarios; do not reduce full existing suite coverage |
| Helm/Kubernetes | `just helm-lint`, `just helm-template`, and the actual kind/Helm integration procedure in CI with the scenario's required replica/persistence settings |
| Reagents | `just reagents-lint`, `just reagents-test`, `just integration-test-infra` as affected |
| Workflow/filter/gate | `actionlint .github/workflows/ci.yml` and `python3 -m unittest discover -s scripts -p 'test_*.py' -v` after G1 (currently `test_ci.py`); lint new workflows, run all new validators immediately, and prove real lane selection |

New commands in G3/G6 are **proposed**, not available today. Before their CI
wiring, each stream tests its containerized entry script and records it in its
PR, including exact image inputs and exit conditions. Run focused new tests plus the relevant
baseline; a unit pass alone cannot prove a cluster/browser scenario.

Build through `just` and the repository Dockerfiles. Use current candidate
artifacts, never stale tags or an unverified precompiled runner. Enable feature
gates on both the server and test runner wherever required. Honor the job YAML
reference. No new CLI command or REST query is planned; any separately approved
addition requires binary/HTTP coverage in `test/` under AGENTS.md.

Shared Docker image tags, fixed container names, sockets, and ports remain
serialized across existing integration/UI/Helm lanes. Coordinate with existing
test owners and record the owner and full command lifetime in Progress; this
repository currently provides no shared lane-lock implementation. Do not assume
a lock file or invent a prerequisite to execute these recipes. New harness
isolation must be proven before relaxing serialization. Operate only on owned
test resources; no production
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
   guarantee has discovered and executed real-surface scenarios. Open Q1–Q6
   decisions remain visible until resolved with evidence.
2. **Distributed correctness:** B1 proves real pod-kill recovery; B3 runs on three
   actual members with persistent volumes, demonstrates each named fault, and
   passes independent history/effect
   checks for owner loss, stale owners, partition/heal, ambiguous responses,
   event replay, peer benching/cooldown, cross-node negative auth, and concurrent
   lifecycle actions. Quorum-loss rejection is asserted only for A1-resolved
   endpoint contracts; unresolved writes remain uncertain. Recovery bounds state
   their preconditions.
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
6. **Performance:** E5 first enforces the repeatable instrumented SQL-work
   budget without claiming timing equivalence. E1–E4 reconcile
   offered/admitted/completed work, collect the full workload interval, compare
   identified base/candidate release artifacts,
   and enforce calibrated absolute/relative budgets with explicit uncertainty.
   Known regressions and insufficient evidence cannot yield a passing strict
   gate. Fixed-baseline trends expose cumulative degradation.
7. **Durability and lifecycle:** F4 qualifies single-node upgrades independently
   of B3; F1/F2 qualify every selected supported upgrade,
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
10. **Actual enforcement:** G3/G5 establish the minimum credible gate; G7
    establishes candidate identity; G6/G4 prove the selected required checks on
    the real merge candidate and block publication on missing/failed same-SHA release
    qualification. Workflow, branch settings, scenario inventory, runbook, and
    Progress agree after each N-1 checkpoint. Preserve existing full
    engine/architecture coverage and label unpromoted lanes honestly.
    Nightly results are not represented as
    evidence from an individual PR.

## How To Pick Up Work

1. Read this plan, its contracts, and the applicable `AGENTS.md`.
2. Select unchecked, included items whose dependencies are verified ready.
   Start with A1, E1, and G1; record Q1–Q6 resolution as it becomes available.
3. Use an assigned worktree and branch from the verified base. Keep changes
   within the stream's file ownership and include its tests and behavior docs.
4. Run the required verification on the actual candidate changes. Record what
   passed, failed, or could not run, including actual scenario execution.
5. Update only assigned item checkboxes and notes. The wave orchestrator owns
   Progress and records completion from verified merged PRs. Complete the
   W<n>/N-1 docs sync before closing a wave; never dispatch concurrent runbook edits.
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
| [Open/closed workload models](https://grafana.com/docs/k6/latest/using-k6/scenarios/concepts/open-vs-closed/) and [benchstat](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat) | Workload-design reference for the Go driver (no k6 install) and repeated benchmark comparisons; insignificance alone is not equivalence. |
| [Go integration coverage](https://go.dev/doc/build-cover) | Instrument actual CLI/server execution and merge compatible profiles; graceful collection and killed-process incompleteness need handling. |
| [Chaos Mesh network faults](https://chaos-mesh.org/docs/simulate-network-chaos-on-kubernetes/) | Kubernetes partition/latency/loss/bandwidth scenarios on a provisioned qualification cluster. |
