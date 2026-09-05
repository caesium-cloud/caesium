# Trust the Substrate — Plan 0 of the Closed-Loop Arc

Last updated: 2026-09-05

> Status: **Active — Plan 0 of [`closed-loop-arc.md`](closed-loop-arc.md).** Not started.

Everything Caesium has shipped must become **true** (the default execution
mode strands DAGs; lineage's `producing_step` is always empty; a cancelled run's
container keeps running; the auth-enabled integration lane executes zero
assertions; the tier-3 approval gate is unreachable from any agent proposal)
and **obtainable** (no GitHub release, no downloadable CLI, a Helm chart whose
`appVersion` is `latest`, a CI that does not gate merges) before the arc's
three loops are built on top of it. This plan is the arc's foundation wave:
scheduler correctness in the SQL lane, data-plane truth, an auth lane that
really runs, CI that really gates, a `v0.1.0` release with per-arch `caesium`
binaries, dead scaffolding deleted, and the ~20 unfiled follow-ups filed.

No design doc exists for this plan — **this plan is the design**; each item
carries its own rationale and fix sketch. Every claim below was re-verified
against `HEAD 48497e9` on 2026-09-05; the `## Recon Ledger` records where
verification corrected the original recon.

This plan follows the `exec-plan-wave` skill's structural convention:
`## Progress` is a wave-by-wave dashboard, `## Streams` is the work
backlog, `## Sequencing & Dependencies` captures cross-stream order,
and `## Acceptance Criteria` lists the gates that close out the entire
plan. Any agent can:

1. Pick a numbered checklist item from `## Streams` whose dependencies
   are satisfied (per `## Sequencing & Dependencies`).
2. Land it as a self-contained PR.
3. Run the verification block under `## Verification (Run For Every
   PR)`.
4. Tick the checkbox and update the active wave's per-stream bullet in
   `## Progress`.

For wave orchestration of the streams below, see
[`.claude/skills/exec-plan-wave/`](../../../.claude/skills/exec-plan-wave/).
For drafting new plans in this same shape, see
[`.claude/skills/draft-exec-plan/`](../../../.claude/skills/draft-exec-plan/).

## Recon Ledger (verified at HEAD 48497e9, 2026-09-05)

*(This section occupies the `plan-template.md` optional "Project Posture /
Strategic Decisions" slot between the boilerplate and `## Source-Of-Truth
Note`; every header `exec-plan-wave` reads — `## Source-Of-Truth Note`,
`## Progress`, `## Streams`, `## Sequencing & Dependencies`,
`## Verification`, `## Acceptance Criteria`, `## How To Pick Up Work` —
follows below in the canonical order.)*

Corrections and additions to the recon that produced this plan. Implementers
re-grep before editing (arc convention 6); this ledger is the starting point,
not a substitute.

| # | Finding | Evidence (symbol) | Consequence for this plan |
|---|---|---|---|
| L1 | The SQL lane never advances a **failed plain task's** successors. `resolveInstanceFailureTx` returns at `!isFanOutInstance(row)` with the comment "an unfanned task's successors are advanced by the ordinary trigger-rule path" — no such path exists; `shouldRunTaskTx` has exactly **four** callers, none of them a plain-failure path: `completeTask`'s success branch, `cacheHitTask`'s own successor loop, `advanceCrossStepSuccessorsTx` (fanned groups) and `skipTaskAndDescendantsTx`. The identical `trigger rule %q not satisfied` skip reason is spelled out in three of them (`cacheHitTask`, `completeTask`, `advanceCrossStepSuccessorsTx`). | `internal/run/fanout.go` `resolveInstanceFailureTx`, `advanceCrossStepSuccessorsTx`; `internal/run/store.go` `completeTask` (status==Failed branch), `cacheHitTask`, `skipTaskAndDescendantsTx`, `failTask`, `shouldRunTaskTx`, `ClaimTaskForDispatch` / `PendingTasksForDispatch` (both require `outstanding_predecessors = 0`) | In **local** mode a *plain* tolerant consumer still runs (the in-memory Kahn loop in `internal/job/job.go` decrements its own `indegree` map) but a **fanned** tolerant consumer is stranded (`runFannedGroup` reads the row scalar; the straggler sweep records "fan-out instance was never dispatched (unresolved in-group dependency)"). In **distributed** mode *every* tolerant consumer is stranded. A1/A2 cover both. |
| L2 | `markTaskSkippedTx` only touches `status = pending` rows, so a second skip of the same task is a no-op. | `internal/run/store.go` `markTaskSkippedTx` | A1 can let the store skip `all_success` consumers on a plain failure without breaking the local executor's own `store.SkipTask` cascade (`internal/job/job.go` `skipDescendantsFiltered`) — but watch for a **second `task_skipped` event** in event-count assertions. |
| L3 | Both executors already `engine.Stop(Force: true)` when the task ctx ends: the local executor only on `context.DeadlineExceeded`, the worker's `monitorTask` on any ctx error. `CancelRun`/`cancelRunTx` publish `TypeRunCancelled` but nothing maps a run to a `context.CancelFunc`. | `internal/job/job.go` (the `select` on `taskCtx.Done()` after `runner.engine.Wait`), `internal/worker/runtime_executor.go` `monitorTask`, `internal/run/store.go` `CancelRun`, `cancelRunTx`, `internal/event/bus.go` `TypeRunCancelled` | A3/A4 are plumbing, not engine work: a per-run cancel registry fed by `TypeRunCancelled` (local) and claim-loss detection in `Worker.runLeaseRenewal` (distributed). No engine interface changes. |
| L4 | Containers carry no run/task labels (`pkg/container` `Spec.Labels` is caller-supplied and neither executor sets one) and `models.TaskRun` records no container id. | `pkg/container/spec.go` `Spec`, `internal/models/run.go` `TaskRun` | The A5 scenario identifies the orphan by a unique marker in the step command and `ContainerInspect(...).Config.Cmd` via the existing `s.dockerClient()` helper (`test/dataplane_test.go`). |
| L5 | `internal/lineage/mapper.go` `persistTaskDatasets` writes `FacetSummary` as `{"step_name": …}` (flat); `internal/lineage/impact.go` `stepNameFromFacet` reads `caesium_dataset.step_name` (nested). `TestStepNameFromFacet` hand-builds the nested shape. The UI **already renders** `producing_step` (`ui/src/features/jobs/LineageGraph.tsx` `producingStep`) — the #255 descoping was reversed later; no UI change needed. `TestLineageImpactReturnsDownstream` (`test/data_plane_e2e_test.go`) never asserts `producing_step`. | as cited | B1 is backend + tests only. |
| L6 | `models.Task` has no JSON tags on `ID`, `JobID`, `AtomID`, **and `CreatedAt`/`UpdatedAt`**; `ui/src/lib/api.ts` `normalizeJobTask` shims all five (`ID`/`JobID`/`AtomID`/`CreatedAt`/`UpdatedAt`), `ui/src/lib/__tests__/api.test.ts` "getJobTasks normalizes task IDs from Go model casing" pins it, and `test/fanout_test.go` `jobTaskIDByName` accepts both casings. `GET /v1/jobs/:id/tasks` (`api/rest/controller/job/tasks.go` `Tasks`) has no other consumer. | as cited | B2 tags all five fields; the test helper and shim both go. |
| L7 | Freshness-derived runs already carry the evaluator's start-time consumed view as run param `_consumed_watermarks` (`internal/freshness/evaluator.go` `freshnessConsumedWatermarksParam`, passed via `runstorage.WithStartParams` in `derive`); the `Capturer` ignores it and re-reads current watermarks at completion (`internal/freshness/subscriber.go` `consumedSnapshot`, "KNOWN LIMITATION" comment). | as cited | B3 has a bounded fix path: prefer the run's `_consumed_watermarks` param; fall back to a `TypeRunStarted` snapshot for non-derived runs. |
| L8 | **The agent-auth lane is hollow.** `just integration-test-agent` sets `CAESIUM_AGENT_AUTH_LANE=true` in the runner container (no test reads it) but not `CAESIUM_AUTH_MODE`/`CAESIUM_AGENT_REMEDIATION_ENABLED`; the two lane scenarios skip on exactly those (`test/agent_mcp_test.go` `TestAgentMCPToolsListBundleAndIncidentScope` checks `vars.AuthMode != "api-key" \|\| !vars.AgentRemediationEnabled`; `test/agent_remediation_cli_test.go` `TestIncidentCLIListJSONStdout` checks `envBool("CAESIUM_AGENT_REMEDIATION_ENABLED")`). Latest master run 33676408439, job `build-and-integration-test-agent-auth` (100403010508): `ok github.com/caesium-cloud/caesium/test 0.068s`. | justfile `integration-test-agent` (runner `-e` block) vs. the two skip guards | H-1 must make the guards true **and** fail the recipe when fewer than N scenarios PASS, or every C-stream scenario will be green-by-skip. |
| L9 | **The tier-3 approval pipeline is not wired at either end.** `agentsvc.SetActionExecutor` has no caller, so `api/rest/service/agent/actions.go` `ProposeAction` always takes its "executor == nil" fallback and records a `proposed` `AgentAction` with no playbook/tier evaluation; `internal/incident/executor.go` `Execute`'s `decisionApprove` branch says "the tier-3 approval flow creates the ApprovalRequest" and nothing does (`grep -rn 'models.ApprovalRequest{'` hits only tests and the decide UPDATE in `api/rest/service/incident/approvals.go`). `TypeApprovalRequested` has no publisher. On the far end, `api/rest/service/incident/approvals.go` `decide` marks the action `approved` and the incident `triaging` ("the executor runs the approved action") but nothing executes it, and `internal/incident/actions.go` `dispatch` has **no case** for `apply_jobdef_patch`, `skip_task` or `override_schema_gate` (`default:` → "has no autonomous executor"); `internal/incident/provenance.go` (the B4 router) does not exist and `CAESIUM_GIT_WRITE_CREDENTIALS` is not an env field. The completed plan ticks B4 and D1; PR #281's subject is "tier-1/2 catalog". | `api/rest/service/agent/actions.go` `SetActionExecutor`/`ProposeAction`; `internal/incident/executor.go` `Execute`; `internal/incident/actions.go` `dispatch`; `api/rest/service/incident/approvals.go` `decide`; `cmd/start/start.go` (constructs `incExecutor` for rules/timers only); `internal/event/bus.go` `TypeApprovalRequested` | Sixth substrate bug, and the one every loop's durable action depends on. C4 wires proposal → `ApprovalRequest`; C7 wires approve → execute and the three tier-3 dispatches (direct `apply_jobdef_patch` route; the Git-PR route is Plan 1 Stream F); C3's approve/reject scenario must go through a **real** proposal and assert the approved action **ran**. |
| L10 | `TestIncidentRoutesGatedOffByDefault` (`test/incident_gating_test.go`) asserts the incident routes 404 — true on the no-auth lanes, false on the widened auth lane. | as cited | H-1 gives it the inverse guard (skip when on the auth lane) before widening `-run` to `TestIncident`. |
| L11 | `/jobs/:id/unpause` is **`PUT`**, not POST (`api/rest/bind/bind.go` `g.PUT("/jobs/:id/unpause", job.Unpause)`). `whoami` is mounted at `/auth/whoami` (not `/v1/…`) in `api/api.go` with `authMiddleware`; `internal/auth/rbac.go` maps `GET /auth/whoami` → `RoleViewer`; `api/middleware/auth_scope.go` `authorizeScope` has no `/auth/whoami` case, so a job-scoped key falls to the trailing `insufficient permissions` deny. `ui/e2e/auth/auth-smoke.spec.ts` "a job-scoped key is denied the global whoami (API-only principal)" pins the 403. | as cited | C1/C6. |
| L12 | Master red causes (last 17 runs, 6 failures): default lane (`build-and-integration-test`) ×2 → `panic: test timed out after 10m0s` — the recipe passes **no `-timeout`**, so the suite now exceeds Go's default; helm/kind lane ×1 → `test timed out after 15m0s`; owner-memory ×2 → `TestFanOutHTTPRetryPartition` "a reset instance never ran again" (runs 33563305235, 33563129021 — pre-#384; not seen since); podman ×1 → `TestGenericUnitPipelineCachesPerUnit` "unit pipeline run … should succeed" (run 33563332037); unit-test ×1 → `no such vfs` in `TestRouterRoutePersistsEventAndMatches` / `TestRouterRouteAfterSharedTriggerJobDeleteFiresSibling` (run 33254267485). | `gh run view <id> --log` | D1/H-2 start from these, not from a fresh triage. |
| L13 | `gh api repos/caesium-cloud/caesium/branches/master/protection` → `required_status_checks.contexts: []`, `checks: []`. | live API | D2. |
| L14 | `helm/caesium/Chart.yaml` is already `version: 0.1.0` (only `appVersion` is `"latest"`), so E3's chart work is an `appVersion` pin, not a `version` bump. `caesiumcloud/caesium:latest` **exists** on Docker Hub, but the `publish` job has never run (no `v*` tag; `gh release list` empty) and it pushes only `${IMAGE_TAG}` (never `latest`); `helm/caesium/values.yaml` `image.tag: ""` defaults to `Chart.appVersion` = `"latest"`, so every Helm install today pulls an image CI never built. `just run` builds from source. `build/Dockerfile.build` builds with `GOOS=linux GOARCH=${TARGETARCH}` against a CGO dqlite built in the same stage — no darwin target is possible from this builder. | justfile `run`/`push`/`push-multiarch`; `.github/workflows/ci.yml` `publish`; `helm/caesium/Chart.yaml`; `build/Dockerfile.build` | E1 is linux-only; E3 pins `appVersion` to the tag. |
| L15 | `README.md` is **not** scanned by `TestPinnedContainerImageVersionsAreConsistent` (`scanDirs` in `internal/guardrails/guardrails_test.go` lists `api build cmd docs helm internal pkg test ui .github`) — which is why its stale base-image tag (3.20) survives. `docs/` **is** scanned: this plan and every doc the plan touches must use `alpine:3.23` / `busybox:1.36.1`. | as cited | N-1 fixes the drift; all docs items obey the pin. |
| L16 | `docs/roadmap.md` Phase 5 table and `docs/README.md` already link `exec-plans/active/trust-the-substrate.md` — but **both describe this plan as "fix the five known bugs"** (`docs/roadmap.md` row 0 of the Phase 5 table; `docs/README.md` the `exec-plans/active/trust-the-substrate.md` bullet), which the ledger's six bugs (L1, L3, L5, L6, L7, L9) and the arc's own AC 1 ("the six ledger bugs") contradict. `docs/roadmap.md`'s Phase 5 table has **four** columns (#, Plan, Loop, Plan doc) and no status column — arc convention 7 forbids adding one. `.gitignore` has `.claude/*` (18 worktree checkouts under `.claude/worktrees` are ignored, not tracked, but **are** on disk and inside any bare `grep -r` scan set); `ui/test-results/.last-run.json` **is** tracked. | `git ls-files ui/test-results`; `git check-ignore -v .claude/worktrees`; `docs/roadmap.md` Phase 5 table; `docs/README.md` active-plans list | Two sibling-doc wording fixes are needed after all — N-4 carries them (this corrects the draft-time "no sibling doc edits needed"); D3 handles hygiene; F1's verification grep must exclude the worktree checkouts. |

## Source-Of-Truth Note

When this plan and [`docs/exec-plans/active/closed-loop-arc.md`](closed-loop-arc.md)
disagree on **scope** (why an item exists, cross-plan ordering, what "done"
means for the arc), the arc wins. When this plan and the **code** disagree on
**mechanics** (a symbol name, a route verb, a lane's env), the code wins and the
implementer fixes the citation in the same PR. This plan restates none of the
arc's shared conventions 1–8; items link to them by number. There is no design
doc: each item's rationale travels with the item.

**Deliberate deviations from the shared conventions, recorded so a wave agent
does not hunt for missing artifacts:**

- *Convention 1 (feature gate)* — N/A. Plan 0 ships no new loop and adds no
  `CAESIUM_<X>_ENABLED` flag; C4/C7 wire a pipeline that is already gated by
  the shipped `CAESIUM_AGENT_REMEDIATION_ENABLED` (`pkg/env/env.go`
  `AgentRemediationEnabled`, whose `Validate` already refuses it under
  `AUTH_MODE=none`).
- *Convention 7 (docs / N- items)* — Plan 0 is the foundation plan, not a
  loop, so convention 7's design-banner flip, `docs/examples/*.job.yaml`
  manifest, schema-reference regeneration and **loop tour** clauses are all
  N/A (no schema change, no loop, no design doc of record). Its close-out
  obligations — arc-dashboard tick, arc `## Sequence` status, roadmap /
  `docs/README.md` repointing, move to `completed/` — are carried by **N-4**
  (the close-out item that runs last), not by N-1 (which is the README
  rewrite). Convention 7's "the arc dashboard row is ticked in the same PR"
  therefore means N-4's PR here.
- *Convention 3 (explainability)* — honoured by **C8**, this plan's single
  explainability item, with `TestIncidentApprovalWhyExplains` as its
  integration scenario.

## Progress (as of 2026-09-05)

No implementation waves have shipped yet. The plan was published with the
recon of 2026-09-04 re-verified at `48497e9` on 2026-09-05; the first wave is
the next eligible run of the `exec-plan-wave` skill against this doc
(suggested W1: Streams A, B, C, F with H-1 first — C4 → C7 → C3 → C8 is the
one serial chain inside W1; W2: Streams D, E, N-3, then N-1/N-2 as a serial
tail; E4 and N-4 last).

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Scheduler correctness in the SQL lane — failed-plain-task advancement, run-cancel reaches the container | **P0** | Not started |
| B | Data-plane truth — lineage facet shape, `Task` JSON tags, freshness consumed-snapshot timing | **P0** | Not started |
| C | Auth surface end-to-end on the (widened, de-hollowed) auth lane; the tier-3 approval pipeline wired at both ends (proposal → `ApprovalRequest`, approve → execute, direct `apply_jobdef_patch` route), and its explainability item (C8) | **P0** | Not started |
| D | CI gates merges and master is honestly green | P1 | Not started |
| E | Release & install — `v0.1.0`, per-arch CLI binaries, `just cli`, chart versioning | P1 | Not started |
| F | Dead scaffolding — `api/gql`, `internal/task`, `pkg/client`, `pkg/bytes`, `pkg/compare` | P2 | Not started |
| H | Harness — the auth lane that actually runs (H-1), lane time budgets + hollow-lane guard (H-2) | **P0** | Not started |
| N | README verbs + install, `docs/getting-started.md`, `docs/README.md` split, filed follow-ups, close-out | P1 | Not started |

## Streams

### Stream A — Scheduler correctness in the SQL lane

The SQL store is the dispatch authority for distributed workers and for the
local executor's fan-out groups (`CAESIUM_RUN_OWNER_ENABLED=false` is the
default; the in-memory owner path in `internal/run/owner_state.go`
`RunState.ApplyCompletion` — tested by `TestRunState_AllDoneRunsAfterFailure` —
is correct but off by default). Two holes: a failed plain task never releases
its tolerant-rule successors (Ledger L1), and a cancelled run's container keeps
running (L3). Plan 1's admission gate must be able to distinguish "downstream
skipped because held" from "downstream never dispatched", which is why the arc
orders this stream first.

- [ ] A1. Advance a failed plain task's successors in the SQL lane the way the
      success path does. In `resolveInstanceFailureTx`, replace the
      `!isFanOutInstance(row) → return nil` early-out with a call to
      `advanceCrossStepSuccessorsTx(tx, runID, catalogTaskID, …)` for a
      non-fanned terminal-failed row (the fanned branch keeps its
      `groupAllTerminalTx` gate). `advanceCrossStepSuccessorsTx` already does
      the right thing per successor: `batchDecrementPredecessorsTx`, then
      `shouldRunTaskTx` → `task_ready` or `skipTaskAndDescendantsTx` with
      `trigger rule %q not satisfied`. Both routes into the failure (`failTask`
      and `completeTask`'s `status == TaskStatusFailed` branch) already share
      `resolveInstanceFailureTx`, so one edit covers both. Rationale for doing
      it in the store rather than the executors: the local executor's
      in-memory `indegree` map already handles plain consumers, the
      `runFannedGroup` path and the distributed claimer both read the row
      scalar — the store is the only place all three agree. Guard against the
      local executor's own `store.SkipTask` cascade double-skipping
      (`markTaskSkippedTx` is pending-only, L2) and update any unit test that
      counts `task_skipped` events. A **cache hit** already advances its own
      successors through a fourth, separate `shouldRunTaskTx` copy inside
      `cacheHitTask` (`internal/run/store.go`; L1) — leave that copy alone,
      but assert in the unit test that the new failure path emits the
      **byte-identical** `trigger rule %q not satisfied` skip reason the other
      three copies produce, so the four sites cannot drift. Unit test in `internal/run`: seed a run
      with plain `a` → fanned `b` (`triggerRule: all_done`) and plain `a` →
      plain `c` (`all_success`); fail `a` through **both** `FailTask` and
      `CompleteTask(result "failure")`; assert every `b` instance row reaches
      `outstanding_predecessors = 0` with a `task_ready` event, and `c` is
      skipped with the rule reason. Extend `TestRunState_AllDoneRunsAfterFailure`'s
      sibling in `store_fanout_test.go` or add `store_plain_failure_test.go`.
      Files: `internal/run/fanout.go` (`resolveInstanceFailureTx`),
      `internal/run/store.go` (only if the failure branches need a new
      argument), new `internal/run/store_plain_failure_test.go`.
- [ ] A2. Integration scenarios that fail before A1 and pass after, on the lanes
      where the bug bites. (a) Default (local) lane, new
      `test/trigger_rule_failure_test.go`
      `TestPlainFailureReleasesAllDoneFannedConsumer`: plain `list` succeeds,
      plain `gate` exits 1, fanned `process` (`dependsOn: [gate]`,
      `triggerRule: all_done`, partitions from `list`) must run every
      partition and the run must reach a terminal status with no
      "never dispatched" skip reason; assert via `GET /v1/jobs/:id/runs/:run_id`
      and the partitions endpoint used by `test/fanout_test.go`. (b) Distributed
      + owner-memory lanes, same file,
      `TestPlainFailureReleasesAllDoneConsumerDistributed`: plain `gate` fails,
      **plain** `report` (`all_done`) must run, `all_success` sibling must be
      skipped with `trigger rule "all_success" not satisfied`. **Expect
      red-before only on the distributed lane.** `just integration-up-owner-memory`
      sets `CAESIUM_RUN_OWNER_ENABLED=true` **and**
      `CAESIUM_RUN_OWNER_IN_MEMORY=true`, so that lane resolves completions
      through `internal/run/owner_state.go` `RunState.ApplyCompletion` — the
      path this plan's Stream A preamble records as already correct. On
      `-owner-memory` the scenario is a **green-before / green-after
      regression guard**, not a reproduction; do not chase a red run there.
      Add
      `TestPlainFailure` to the `-run` regexes of `integration-test-distributed`
      and `integration-test-owner-memory` in the justfile. Use `alpine:3.23`.
      Files: new `test/trigger_rule_failure_test.go`, `justfile`
      (`integration-test-distributed`, `integration-test-owner-memory` `-run`
      patterns). Depends on: A1 + H-1 (justfile sequencing).
- [ ] A3. Make run cancellation reach the local executor's container. Add a
      process-wide run-cancel registry in `internal/job` (new
      `internal/job/cancel_registry.go`: `Register(runID) (ctx, release)`,
      `Cancel(runID)`), and derive **every** detached run context from a
      registered cancellable context. There are five such sites today
      (re-grep `WithContext(context.Background()` before editing): the manual
      run entry `api/rest/controller/job/run/post.go` `Post`, the retry entry
      `api/rest/controller/job/run/retry.go`, the partition-retry entry
      `api/rest/controller/job/run/partitions.go`, `internal/job/job.go`
      `startReplacementRun` (the in-process partition-retry replacement
      kickoff — **not** the main run entry) and `internal/job/backfill.go`.
      Missing any one of them leaves a cancel during a retry or a
      partition-retry with the container still running, i.e. the bug A3
      exists to fix. Subscribe the
      registry to `TypeRunCancelled` on the in-process bus in
      `cmd/start/start.go` so `CancelRun` **and** the concurrency `replace`
      admission (`cancelRunTx` publishes the same event) both fire it. Extend
      the `taskCtx.Done()` branch in `job.go` to `engine.Stop(Force: true)` on
      `context.Canceled` as it already does on `DeadlineExceeded`, and make the
      resulting task write a no-op against the cancelled row (PR #275's
      terminal guards in `StartTask`/`completeTask`/`cacheHitTask` already
      make it one). Do **not** edit `internal/run/store.go` — the event is the
      seam, which keeps A1 and A3 conflict-free. Unit test: a fake engine
      records `Stop`; cancel the run mid-`Wait`; assert `Stop` was called and
      the row stayed `cancelled`. Files: new `internal/job/cancel_registry.go`,
      `internal/job/job.go` (`startReplacementRun`), `internal/job/backfill.go`,
      `api/rest/controller/job/run/post.go`,
      `api/rest/controller/job/run/retry.go`,
      `api/rest/controller/job/run/partitions.go` (each detached goroutine
      registers), `cmd/start/start.go`, new
      `internal/job/cancel_registry_test.go`.
- [ ] A4. Make run cancellation reach a distributed worker's container. A
      cancelled run strips `claimed_by` from its tasks (`cancelRunTx`), so
      the worker's batched `RenewLeases` (`internal/run/store.go`
      `RenewLeases`, called from `Worker.runLeaseRenewal`) already sees
      `RowsAffected < len(ids)` on the next tick. Track per-task
      `context.CancelFunc`s in the pool (`internal/worker/pool.go`), and when
      renewal loses a claim, cancel that task's context — `monitorTask`
      already `engine.Stop`s on any ctx error. Also honour the renewal-loss
      path for lease expiry (same mechanism; today the container runs on
      while another node re-executes). Unit test with the existing fake
      renewer in `internal/worker/run_lease_renewal_test.go`. Files:
      `internal/worker/worker.go` (`runLeaseRenewal`),
      `internal/worker/pool.go`, `internal/worker/runtime_executor.go` (only
      if the ctx threading needs it), `internal/worker/run_lease_renewal_test.go`.
- [ ] A5. Integration scenario: replace-cancel stops the orphaned container.
      Extend `test/run_concurrency_test.go` "replace cancels oldest and starts
      fresh" (or add `TestReplaceCancelStopsOrphanedContainer` in a new
      `test/run_cancel_container_test.go`) so the first run's step is
      `sh -c 'sleep 120 # <unique-marker>'`, trigger the replacing run, and
      poll `s.dockerClient().ContainerList(All: true)` +
      `ContainerInspect` for a container whose `Config.Cmd` contains the
      marker: it must be absent (or `State.Running == false`) within the
      cancel deadline. Add a `TestReplaceCancel` term to the distributed
      lane's `-run` regex so A4 is covered too. Files:
      `test/run_concurrency_test.go` or new `test/run_cancel_container_test.go`,
      `justfile` (`integration-test-distributed` `-run`). Depends on: A3 + A4
      + H-1.

### Stream B — Data-plane truth

Three shipped surfaces report something other than what happened.

- [ ] B1. Write and read one `caesium_dataset` facet shape. In
      `internal/lineage/mapper.go` `persistTaskDatasets`, replace the flat
      `json.Marshal(map[string]string{"step_name": …})` with the nested
      `{"caesium_dataset": {"step_name": …}}` that `stepNameFromFacet` reads
      (matches the OpenLineage facet the same file already emits via
      `CaesiumDatasetFacet`); make `stepNameFromFacet` accept the legacy flat
      shape too so rows persisted before the fix still resolve. Replace
      `TestStepNameFromFacet`'s hand-built JSON with a round-trip through the
      **real** write path (`persistTaskDatasets` into a sqlite-backed store,
      then `ImpactQuery`) so the test cannot go hollow again. Assert
      `producing_step` in `test/data_plane_e2e_test.go`
      `TestLineageImpactReturnsDownstream` (the lane already sets
      `CAESIUM_OPEN_LINEAGE_ENABLED=true`). No UI change — Ledger L5.
      Files: `internal/lineage/mapper.go`, `internal/lineage/impact.go`,
      `internal/lineage/impact_test.go`, `test/data_plane_e2e_test.go`.
- [ ] B2. Give `models.Task` JSON tags (`id`, `job_id`, `atom_id`,
      `created_at`, `updated_at`) so `GET /v1/jobs/:id/tasks` serialises like
      every other endpoint; delete `RawJobTask`/`normalizeJobTask`/
      `normalizeJobTasks` in `ui/src/lib/api.ts` (keep `next_id` handling if
      the server ever emits it — verify; today no model field carries it) and
      the api.test.ts case that pins the capitalised keys, replacing it with
      one that pins snake_case; simplify `test/fanout_test.go`
      `jobTaskIDByName` to read `id`/`name` only; add
      `TestJobTasksSerialiseSnakeCaseIDs` in `test/job_test.go` asserting the
      raw JSON keys. Files: `internal/models/task.go`, `ui/src/lib/api.ts`,
      `ui/src/lib/__tests__/api.test.ts`, `test/fanout_test.go`,
      `test/job_test.go`.
- [ ] B3. Source the freshness consumed-dataset snapshot from the run's start,
      not its completion. Investigate, then implement if bounded: in
      `internal/freshness/subscriber.go` `handleRunCompleted`, read the run's
      params (`internal/run/store.go` `decodeRunParams`) and, when
      `_consumed_watermarks` is present (freshness-derived runs), use it as
      the consumed snapshot; for other triggers subscribe the `Capturer` to
      `TypeRunStarted` as well and keep a bounded per-run snapshot (in-memory
      map keyed by run id, evicted on completion) taken at start. Unit test
      in `subscriber_test.go`: advance an input between `run_started` and
      `run_completed` and assert the produced dataset's
      `ConsumedWatermarks` records the start-time value. Integration:
      `TestFreshnessConsumedSnapshotTakenAtRunStart` in
      `test/freshness_test.go`. If the change is not bounded (e.g. the
      `Capturer` needs a persisted table to survive restarts), stop and file
      it with the repro via N-3 — but that path also requires amending arc
      acceptance criterion 1 (which today demands an integration scenario for
      **all six** ledger bugs, L7 included) in the same PR; see the carve-out
      note on Acceptance Criterion 2. The item is done either way only when
      the decision **and** the arc amendment are recorded. Files: `internal/freshness/subscriber.go`,
      `internal/freshness/subscriber_test.go`, `test/freshness_test.go`.

### Stream C — Auth surface end-to-end, and the approval gate made reachable

Arc convention 2 says auth-gated paths are exercised on the auth-enabled lane.
Today that lane is `just integration-test-agent` and it executes nothing
(Ledger L8). H-1 fixes the lane; this stream fills it. Every scenario here
drives the CLI binary (`s.runCLIStdout` / `s.runCLISeparate` from
`test/data_plane_e2e_test.go`, stdout captured separately) or the live HTTP
surface, per the `CLAUDE.md` coverage gate. All scenarios are named
`TestAuth*`, `TestIncident*`, `TestScoped*` or `TestHold*` so the widened
`-run` pattern picks them up, and each starts with the H-1 `requireAuthLane()`
guard.

- [ ] C1. Allow `GET /auth/whoami` for any authenticated **API-key** principal.
      Decision: whoami is identity, not resource access. In
      `api/middleware/auth_scope.go` `authorizeScope`, add a
      `case "/auth/whoami"` returning an empty `scopeAuditContext` for
      job-scoped keys (before the `/v1/jobs/:id` prefix branch); leave the
      agent-session branch untouched (an agent token stays confined to
      `/v1/agent/*` — it must never be able to complete a UI login). Unit
      test in `api/middleware/auth_scope_test.go`. Flip
      `ui/e2e/auth/auth-smoke.spec.ts` "a job-scoped key is denied the global
      whoami (API-only principal)" to assert 200 and that the response names
      the key's scope, and update its header comment; add the positive
      auth-lane scenario `TestScopedKeyWhoamiAllowed` (scoped key created via
      `caesium auth key create --scope-jobs`, then `GET /auth/whoami` → 200).
      Files: `api/middleware/auth_scope.go`,
      `api/middleware/auth_scope_test.go`, `ui/e2e/auth/auth-smoke.spec.ts`,
      new `test/auth_scoped_test.go`. Depends on: H-1.
- [ ] C2. Live scenarios for the key-management surface. `TestAuthKeyLifecycleCLI`
      drives `caesium auth key create --role viewer --description …`,
      `key list`, `key rotate --id … --grace-period 1m`, `key revoke --id …`
      with `--server`/`--api-key` (flags in `cmd/auth/key_*.go`), asserting
      stdout carries the new key prefix and the revoked key is rejected by a
      subsequent `GET /v1/jobs`; `TestAuthAuditCLI` drives `caesium auth
      audit --action api_key.revoke` and asserts the revoke shows up;
      `TestAuthKeysREST` hits `GET/POST /v1/auth/keys`,
      `POST /v1/auth/keys/:id/{rotate,revoke}`, `GET /v1/auth/audit`
      (`api/rest/bind/bind.go` `bindAuth`) directly, including a 403 for a
      viewer-role key on `POST /v1/auth/keys`. Files: new
      `test/auth_keys_test.go`. Depends on: H-1.
- [ ] C3. Live approve/reject through a **real** proposal. `TestIncidentApprovalDecisionsCLI`:
      apply a failing job on the auth lane, wait for the incident
      (`GET /v1/incidents`), post a tier-3 action (`apply_jobdef_patch` or
      `skip_task`, `internal/incident/actions.go` `TierApproval`) through
      `POST /v1/agent/incidents/:id/actions` with an agent-session token
      minted the way `test/agent_mcp_test.go` mints one, assert the incident
      is `awaiting_approval` with an approval in `GET /v1/incidents/:id`,
      then `caesium incident approve <id> --approval <approval-id> --json`
      (stdout parseable) and **assert the approved action executed** — for
      `skip_task`, the named task row is `skipped` and the run reaches a
      terminal status; for `apply_jobdef_patch` on a job without git
      provenance, the job's definition reflects the patch (`GET /v1/jobs/:id`)
      and the `AgentAction` row is `executed` with the C7 result payload.
      On a second incident, `caesium incident reject … --reason …` leaves
      the action `rejected` and the incident `escalated`; assert the agent
      token itself gets `ApprovalAgentTokenDenyMessage` on the approve route.
      Update the stale "stood up by the plan's harness item (H-1)" comment in
      `test/incident_gating_test.go` to point here. Files: new
      `test/incident_approval_test.go`, `test/incident_gating_test.go`
      (comment). Depends on: C4 + C7 + H-1.
- [ ] C4. Wire the tier-3 approval flow the completed plan says exists. Call
      `agentsvc.SetActionExecutor(...)` in `cmd/start/start.go` next to the
      existing `incExecutor` construction (an adapter implementing
      `ExecuteAgentAction(ctx, agentsvc.ActionRequest) (*ActionResult, error)`
      over `incident.Executor.Execute`), and in `internal/incident/executor.go`
      `Execute`'s `decisionApprove` branch create the `models.ApprovalRequest`
      row (action ref, incident, requested-at), move the incident to
      `awaiting_approval` through the store's transition table
      (`internal/incident/incident.go`), publish `TypeApprovalRequested`, and
      end the agent session per the completed plan's D1 text. Preserve the
      fallback in `ProposeAction` only for the executor-nil unit tests.
      Unit tests in `internal/incident/executor_test.go` (approval row created,
      status transition, event published) and
      `api/rest/service/agent/actions_test.go` (delegation when wired).
      Rationale: Plan 1 Stream F, Plan 2 Stream E and Plan 3 Stream F all
      route through "agent proposes → human approves"; today that path stops
      at a `proposed` row nobody can approve (Ledger L9). Files:
      `cmd/start/start.go`, `internal/incident/executor.go`,
      `internal/incident/executor_test.go`,
      `api/rest/service/agent/actions.go`,
      `api/rest/service/agent/actions_test.go`.
- [ ] C5. Scoped-key allow/deny matrix as one table-driven live scenario
      (`TestScopedKeyAllowDenyMatrix`): for a key scoped to job `A`, assert
      200 on `GET /v1/jobs` (filtered to `A`), `GET /v1/jobs/:idA`,
      `POST /v1/jobs/:idA/run`, `GET /v1/events?run_id=<A run>`; 403 on
      `GET /v1/jobs/:idB`, `POST /v1/jobs/:idB/run`, `GET /v1/lineage/impact`
      (`LineageImpactScopedDenyMessage`), `GET /v1/contracts/graph`,
      `GET /v1/events` without `run_id`, `POST /v1/jobdefs/apply` with
      `prune`; 404 for a run id that does not exist. Each row cites the
      `authorizeScope` case it pins. Files: `test/auth_scoped_test.go` (shared
      with C1). Depends on: C1 + H-1.
- [ ] C6. Close the no-test gaps recon found, each through its real surface:
      `TestJobUnpauseRoute` (`PUT /v1/jobs/:id/pause` then `PUT …/unpause`,
      then a manual run succeeds); `TestRunRetryCallbacksCLI` (`caesium run
      retry-callbacks`, `cmd/run/retry_callbacks.go`, against a run whose
      callback target 500s, then flips to 200 — reuse the receiver in
      `test/callback_test.go`); `TestNotificationChannelAndPolicyByID`
      (`GET/PATCH/DELETE /v1/notifications/channels/:id` and
      `…/policies/:id`); `TestNodeWorkersRoute` (`GET /v1/nodes/:address/workers`
      — on the default lane assert the 200 shape for the local node or the
      documented 404); `TestBackfillCLILifecycle` (`caesium backfill create
      --job-id … --start … --end …`, `backfill list`, `backfill cancel`
      through the binary — `test/backfill_test.go` today uses raw HTTP only).
      These run on the default lane (no auth), so they are **not** prefixed
      for the auth lane. Files: `test/job_test.go`, `test/callback_test.go`,
      new `test/notification_routes_test.go`, new `test/node_workers_test.go`,
      `test/backfill_test.go`.
- [ ] C7. Execute approved tier-3 actions (the far end of Ledger L9). (a) In
      `api/rest/service/incident/approvals.go` `decide`, after the transaction
      commits an `approved` decision, hand the `AgentAction` to a
      post-approval executor — add `Executor.ExecuteApproved(ctx, actionID)`
      in `internal/incident/executor.go` that reloads the action, re-derives
      the incident, runs `dispatch` for it under actor `human` (the decider)
      and finishes the row `executed`/`failed` exactly like the autonomous
      path (same `finish`/`observe`/`mirrorAudit`), so the audit spine shows
      who approved and what ran. Inject the executor into the incident REST
      service the same way C4 injects it into the agent service (a setter
      wired in `cmd/start/start.go`; the service degrades to "approved,
      not executed" with a logged warning when unset, and that degraded
      path is what today's `incident_test.go` seeds). (b) Add the three
      missing `dispatch` cases in `internal/incident/actions.go`. `dispatch`
      reaches the runtime **only** through `e.ops` (the `ActionOps`
      interface), which today declares exactly ten methods (`RetryFromFailure`
      … `ExtendSLAOnce`) and none of the three — so this half of the item is
      three interface methods plus their adapter, not three inline calls.
      Extend `ActionOps` in `internal/incident/actions.go` with
      `SkipTask(ctx context.Context, runID, taskID uuid.UUID, reason string) error`,
      `OverrideSchemaGateOnce(ctx context.Context, runID uuid.UUID) error` and
      `ApplyJobdefPatch(ctx context.Context, jobID uuid.UUID, patch …) (result, error)`,
      and implement all three on the concrete adapter `incidentActionOps` in
      `cmd/start/incident_ops.go` (constructed by `newIncidentActionOps` and
      handed to `incident.NewExecutor` from `cmd/start/start.go`). Then:
      `skip_task` → `e.ops.SkipTask(...)`, whose adapter calls the shipped
      `run.Store.SkipTask` (`internal/run/store.go` `SkipTask` — honours
      trigger rules via `skipTaskAndDescendantsTx`; record the design's Open
      Question 3 caveat in the result), `override_schema_gate`
      → a one-run bypass recorded on the `JobRun` (new nullable column or
      run-param; the executors' `ValidateTaskOutputSchema` call site reads
      it), and `apply_jobdef_patch` → the **direct** route only: for a job
      with no git provenance (`models.Job` git fields empty), render and
      apply the patch through the shipped jobdefs diff/apply service
      (`internal/jobdef/diff`, `api/rest/service/jobdefs` — find the
      in-process entry point the `caesium job apply` path uses) and return
      the diff in the result; for a git-synced job, **degrade to `escalate`**
      with the rendered diff in the notification, exactly as the completed
      plan's B4 text specifies for the no-credentials case. The Git-PR route
      (`CAESIUM_GIT_WRITE_CREDENTIALS`, `internal/incident/provenance.go`)
      is **Plan 1 Stream F's**, not this item's — say so in the code comment
      and in `docs/design-agent-in-the-loop.md`'s banner. Arc convention 4
      requires that this single apply router be **refused under
      `CAESIUM_AUTH_MODE=none`**: `apply_jobdef_patch` returns a typed refusal
      (not a panic, not a silent no-op) when `vars.AuthMode == "none"`
      (`pkg/env/env.go` `Environment.AuthMode`, default `"none"`), recorded on
      the `AgentAction` as `failed` with that reason. The existing
      `Environment.Validate` gate only refuses `AGENT_REMEDIATION_ENABLED`
      without an auth mode — that is a different gate and does not cover a
      deployment that enables remediation and later flips auth off. Unit-test
      the refusal in `internal/incident/executor_test.go`, and assert the
      refusal message on the **default (no-auth) lane** as one row of C6.
      (c) *Explainability is C8* — the `TypeAgentActionExecuted` publication
      and the `caesium why` / incident-timeline provenance ("approved by
      <decider>, executed at <ts>") that this item originally carried as a
      sub-bullet are promoted to their own item, per arc convention 3's
      "exactly one item per plan". C7 must leave the executor's `finish`/
      `observe`/`mirrorAudit` seam in a shape C8 can hang the event off.
      Unit tests: `executor_test.go` (approved action dispatches once;
      a second `decide` is refused by the pending-only guard; each of the
      three tier-3 cases with a fake `ActionOps`; the `AUTH_MODE=none`
      refusal), `approvals_test.go`
      (executor invoked after commit, degraded path when unset). Files:
      `api/rest/service/incident/approvals.go`, `internal/incident/executor.go`,
      `internal/incident/actions.go` (`ActionOps` + `dispatch`),
      `cmd/start/incident_ops.go` (`incidentActionOps` — the three new
      adapter methods), `internal/incident/executor_test.go`,
      `api/rest/service/incident/*_test.go`, `cmd/start/start.go`,
      `internal/models/run.go` (only if `override_schema_gate` needs a
      column), `docs/design-agent-in-the-loop.md` (banner caveat).
      Depends on: C4.
- [ ] C8. Make the approved-action decision explainable (arc convention 3 —
      this plan's single explainability item). Declare
      `TypeAgentActionExecuted` in `internal/event/bus.go` (today's incident
      types stop at `TypeApprovalRequested`; verify no existing type already
      carries the executed-action payload before adding one) and publish it
      from C7's `ExecuteApproved` finish path with the decider, the action
      type, the tier, and the result summary; route it through
      `internal/notification/subscriber.go` like the other incident events.
      Surface the provenance in the **task-scoped** explainer — arc convention
      3's `caesium why <run-id> --task <task> --job-id <job-id>`
      (`internal/run/why.go` `loadTrigger`/`WhyTrigger`, `cmd/why/why.go`
      `renderTable`) and `GET /v1/jobs/:id/runs/:run_id/why` — so a run whose
      task was `skip_task`-ed or whose schema gate was overridden by an
      approved action says who approved it and when. No run-level `why` verb
      (arc synergy table). Integration scenario on the auth lane:
      `TestIncidentApprovalWhyExplains` drives C3's approved `skip_task`
      through to a terminal run, then `caesium why <run> --task <skipped-task>
      --job-id <job>` with **stdout captured separately** (`runCLIStdout`, not
      the stream-merging `runCLIRaw`, per `CLAUDE.md`) and asserts the output
      names the decider and the action. Files: `internal/event/bus.go`,
      `internal/notification/subscriber.go`, `internal/run/why.go`,
      `cmd/why/why.go`, new `test/incident_approval_test.go` (shared with C3).
      Depends on: C7 + C3 + H-1.

### Stream D — CI gates merges and master is honestly green

Arc convention 8 assumes required status checks exist. They do not (Ledger
L13), and 6 of the last 17 master runs are red for reasons the ledger already
names (L12).

- [ ] D1. Fix or quarantine the root causes in L12. (a) `TestFanOutHTTPRetryPartition`
      on owner-memory: confirm #384 (`48497e9`) fixed it by reading the three
      master runs after it; if it recurs, bisect `internal/run/store.go`
      `RetryPartition`'s outstanding re-seed (`partitionRetryOutstandingTx`)
      against the owner in-memory lane. (b) `TestGenericUnitPipelineCachesPerUnit`
      on podman: reproduce with `just integration-test-podman`; the memory
      note says the podman/helm lanes drift red when a feature adds env to
      `integration-up` only — diff the podman `docker run -e` block in
      `ci.yml` against `integration-up` and add what is missing (or fix the
      test's engine assumption). (c) `no such vfs` in
      `internal/trigger/event` router tests: quarantine with `t.Skip` behind
      a filed issue **only** if a real fix (serialising the parallel dqlite
      openers) is not bounded — record the decision in the PR. (d) The
      **eight** `openIntegrationCatalogDB()` call sites that dial
      `dqlite:9001` directly (memory: skipped on k8s, need shared netns on
      podman) — `test/run_concurrency_test.go`, `test/reproduce_endpoint_test.go`,
      `test/job_queue_cancel_test.go`, `test/freshness_test.go`,
      `test/contract_enforcement_test.go`, `test/event_cli_test.go`,
      `test/freshness_arrival_test.go`, `test/reproduce_cli_test.go`
      (re-grep; the "two" in MEMORY.md is stale). Make the skip explicit and
      lane-aware **inside the helper itself** (`test/event_cli_test.go`
      `openIntegrationCatalogDB`, where a `t.Skip` on the non-shared-netns
      lanes covers all eight at once) rather than at each call site — eight
      hand-edited guards is how six of them silently drift. Files:
      `internal/run/store.go` (only for a),
      `.github/workflows/ci.yml` (`podman-integration-test` env block),
      `internal/trigger/event/*_test.go`, `test/event_cli_test.go` (the
      `openIntegrationCatalogDB` helper). Depends on: H-2 for the timeout half of L12.
- [ ] D2. Add required status checks and write the CI runbook. Run
      `gh api -X PATCH repos/caesium-cloud/caesium/branches/master/protection/required_status_checks`
      (or `PUT …/protection` with the full body) with `checks` =
      `lint`, `unit-test`, `unit-test-arm64`, `ui-test`, `ui-e2e`,
      `ui-e2e-auth`, `build-and-integration-test`,
      `build-and-integration-test-agent-auth`
      (the widened auth lane is now load-bearing for the arc; `ui-e2e-auth` is
      the **only** gate on C1's scoped-key `whoami` behaviour — C1 flips an
      assertion that runs in no other job), and `strict:
      false`. AC4's list must stay byte-identical to this one. Record the
      exact command, the chosen job list, why the
      distributed/owner-memory/podman/helm lanes are **not** required yet
      (their flake rate in L12) and the criterion for promoting them, in a
      new `docs/ci.md`. `docs/ci.md` must also record the distinction between
      **required-to-merge** (this `checks` list, branch protection) and
      **required-to-publish** (`publish.needs` in
      `.github/workflows/ci.yml`, a strictly wider set that today includes
      `ui-e2e-auth`, `build-and-integration-test-distributed`,
      `-owner-memory`, `-infra`, `-infra-arm64`, `-arm64`, `helm-lint`,
      `helm-integration-test` and `podman-integration-test` — i.e. exactly the
      lanes left non-required here), so a reader does not assume a green
      required set means a taggable commit. Plus the lane matrix (which justfile recipe each
      `ci.yml` job runs, which env each lane's server gets) and the "a lane
      that starts its own server goes red silently" rule. Index `docs/ci.md`
      in `docs/README.md` in the same PR (`TestDocsREADMEIndexesEveryTopLevelDoc`).
      Files: new `docs/ci.md`, `docs/README.md`. Depends on: D1 + H-2 (the
      required lanes must be green first).
- [ ] D3. Repository hygiene: `git rm --cached ui/test-results/.last-run.json`
      and add `ui/test-results/` to `.gitignore`; add a `clean-worktrees`
      justfile recipe (appended at the end of the file) that runs
      `git worktree prune` and removes `.claude/worktrees/*` checkouts whose
      branch is merged into `master` (dry-run by default, `force=true` to
      delete), with a comment citing the 18-checkout / ~675k-LOC state that
      motivated it. Files: `.gitignore`, `justfile` (new recipe at EOF).
      *The README Codecov badge (`branch=develop` → `master`) is bullet (c)
      of N-1 — `README.md` has exactly one editor in W2.*

### Stream E — Release & install

The README's first sentence is "single self-contained binary"; there is no
binary to download (Ledger L14).

- [ ] E1. Extend the `publish` job to create a GitHub Release with per-arch
      CLI binaries. After the existing "Load release images" step, extract
      `/bin/caesium` from `caesiumcloud/caesium:${IMAGE_TAG}-amd64` and
      `…-arm64` (`docker create` + `docker cp`, the same idiom the
      helm/podman jobs use), name them `caesium-linux-amd64` /
      `caesium-linux-arm64`, write `SHA256SUMS`, and attach all three to a
      release for `${IMAGE_TAG}` via `gh release create --verify-tag
      --generate-notes` (or `softprops/action-gh-release`), plus the
      multi-arch image digests in the release body. Add
      `permissions: contents: write` on the **publish job only** (not the
      workflow top-level, to keep D1/H-2's edits to the test jobs
      conflict-free). **Linux only**: the builder compiles with
      `GOOS=linux` against CGO dqlite built in-stage (`build/Dockerfile.build`),
      so a darwin binary is out of scope — say so in the release notes
      template and in N-1's install step (macOS users run the container via
      `just cli`/E2). Files: `.github/workflows/ci.yml` (`publish` job only).
- [ ] E2. Add a `cli` justfile recipe: `just tag=v0.1.0 cli` pulls
      `caesiumcloud/caesium:{{tag}}` (defaulting to the latest release tag
      resolved with `gh release view --json tagName` when `tag` is `latest`),
      `docker create` + `docker cp` the binary to `./.tmp/caesium-cli/caesium`
      (the path every integration recipe already uses), prints the path, and
      refuses to run when the tag does not exist. Place it directly after
      `push-multiarch`. Files: `justfile`.
- [ ] E3. Version the Helm chart with the release. Set
      `helm/caesium/Chart.yaml` `appVersion: "v0.1.0"` (from `"latest"`);
      **leave `version` at `0.1.0`** — it is already `0.1.0` (Ledger L14), so
      the first release needs no chart-version bump, and bumping it here would
      violate the semver rule this item itself documents. Every *later* chart
      change bumps `version` per semver. Make the `publish` job fail if `appVersion` does not equal the pushed
      tag (a one-line grep in the job), and document the rule — "every `v*`
      tag bumps `Chart.yaml` `appVersion` to the tag and `version` per
      semver in the same PR" — in `docs/kubernetes-deployment.md` next to
      the `image.tag` row. Files: `helm/caesium/Chart.yaml`,
      `.github/workflows/ci.yml` (`publish` job — same PR as E1 or rebased
      after it), `docs/kubernetes-deployment.md`. Depends on: E1.
- [ ] E4. Cut `v0.1.0`. The tag push is the **user's action**; the item is
      the checklist around it: E1–E3 merged; **every job in `publish.needs`
      (`.github/workflows/ci.yml`) green on the tagged commit** — a strictly
      wider set than D2's required-to-merge checks, and the real gate: `lint`,
      `unit-test`, `unit-test-arm64`, `ui-test`, `ui-e2e`, `ui-e2e-auth`,
      `build-and-integration-test`, `-distributed`, `-owner-memory`,
      `-agent-auth`, `-infra`, `-infra-arm64`, `-arm64`, `helm-lint`,
      `helm-integration-test`, `podman-integration-test` (re-read the
      `needs:` list before tagging; it includes exactly the flaky lanes D2
      deliberately leaves non-required, so a green required set is **not**
      sufficient — D1 must have made the podman and helm lanes honestly green
      or the tag build will not publish); then
      `git tag -a v0.1.0 -m "v0.1.0" && git push origin v0.1.0`,
      the `publish` job succeeds, the release page shows
      `caesium-linux-amd64`, `caesium-linux-arm64`, `SHA256SUMS`;
      `docker manifest inspect caesiumcloud/caesium:v0.1.0` resolves both
      arches; the digests are recorded in `docs/ci.md`'s release section and
      referenced by digest in `docs/getting-started.md` (N-2). Ends with:
      tag pushed, release page populated, images pulled by digest in docs.
      Files: `docs/ci.md`, `docs/getting-started.md` (digest lines only).
      Depends on: E1 + E2 + E3 + D2.
      *The README Quick Start install step 0 is bullet (b) of N-1.*

### Stream F — Dead scaffolding

- [ ] F1. Remove the GraphQL placeholder. Delete `api/gql/` (schema has one
      `place` → `"holder"` query, `JobRunType`/`jobRunParamsType` never
      wired, no `Mutation`), `registerGraphQL` and its call in `api/api.go`,
      the `/gql` prefix in `api/ui.go`'s SPA-bypass list, the two
      `TestRegisterGraphQL*` tests in `api/api_test.go`, and the
      `github.com/graphql-go/{graphql,handler}` requirements (`go mod tidy`).
      Scrub every mention: `README.md` (the "optional GraphQL endpoint"
      sentence, the API-reference paragraph and the `GET /gql` table row),
      `CLAUDE.md` and `AGENTS.md` (`api/` → "HTTP handlers and related
      schema/routes"), `CONTRIBUTING.md` (the repo-layout tree's
      "HTTP server, REST controllers, GraphQL" line),
      `.claude/skills/draft-exec-plan/PLAYBOOK.md` and
      `.claude/skills/exec-plan-wave/stream-agent-prompt{,-codex}.md` (the
      "GraphQL is a live-but-placeholder endpoint" guidance becomes "there is
      no GraphQL; surface features via REST"). **Only live surfaces are
      scrubbed**: historical exec plans (`docs/exec-plans/**` — including this
      plan and `closed-loop-arc.md`, both of which name GraphQL to describe
      the deletion) and superpowers specs (`docs/superpowers/specs/**` — e.g.
      the SSO design's §11.4 "GraphQL stays disabled") are a design record and
      keep their references; so do the 18 gitignored `.claude/worktrees/*`
      checkouts, which a bare `grep -r .claude` would otherwise scan. Verify
      with the scan set that can actually reach empty:
      `grep -rniE 'graphql|/gql' README.md CLAUDE.md AGENTS.md CONTRIBUTING.md api .claude/skills`
      → empty (a bare `… docs .claude api` grep returns ~870 hits today and
      can never reach zero — AC6 mirrors this command byte-for-byte).
      Files: `api/gql/` (delete), `api/api.go`, `api/ui.go`,
      `api/api_test.go`, `go.mod`, `go.sum`, `README.md`, `CLAUDE.md`,
      `AGENTS.md`, `CONTRIBUTING.md`,
      `.claude/skills/draft-exec-plan/PLAYBOOK.md`,
      `.claude/skills/exec-plan-wave/stream-agent-prompt.md`,
      `.claude/skills/exec-plan-wave/stream-agent-prompt-codex.md`.
- [ ] F2. Remove the zero-importer stubs. `internal/task/task.go` (one line:
      `package task`), `pkg/client/client.go` (an empty `Caesium` interface
      and a `client` struct), and — only after confirming zero importers —
      `pkg/bytes/` and `pkg/compare/`. Tool: `grep -rn '"github.com/caesium-cloud/caesium/pkg/bytes"'`
      (and the other three paths) across the repo **and** `reagents/` (a
      separate module) must return nothing; then `go build ./... && go vet
      ./...` inside `just lint`'s container (golangci-lint's `unused` does
      not flag exported symbols, so grep is the criterion, vet the
      confirmation). Keep any package that has an importer and record it
      here. Files: `internal/task/` (delete), `pkg/client/` (delete),
      `pkg/bytes/` and `pkg/compare/` (delete if unreferenced).

## Harness Strengthening

- [ ] H-1. Make the auth-enabled lane real and wide. In the justfile
      `integration-test-agent` recipe: (a) add
      `-e CAESIUM_AUTH_MODE=api-key -e CAESIUM_AGENT_REMEDIATION_ENABLED=true`
      to the **runner** container so the existing guards in
      `test/agent_mcp_test.go` and `test/agent_remediation_cli_test.go` pass,
      and add a shared `requireAuthLane()` helper (new
      `test/auth_lane_test.go`, keyed on `CAESIUM_AGENT_AUTH_LANE`) that new
      scenarios use — keep the two existing guards but route them through it;
      (b) give `TestIncidentRoutesGatedOffByDefault` the inverse guard (skip
      on the auth lane, Ledger L10); (c) widen `agent_integration_run` to
      `TestIntegrationTestSuite/(TestAgent|TestAuth|TestIncident|TestScoped|TestHold)`
      (`TestHold*` is reserved for Plan 1; matching nothing is fine); (d) run
      `go test … -v` and fail the recipe when fewer than **3** lines match
      the suite's subtest PASS lines — tee to a log and count with
      `grep -cE '^[[:space:]]*--- PASS: TestIntegrationTestSuite/' "$log"`.
      **The pattern must not be anchored at column 0**: testify suite methods
      are Go subtests, so `go test -v` prints
      `--- PASS: TestIntegrationTestSuite (…)` at column 0 and each scenario
      as `    --- PASS: TestIntegrationTestSuite/TestAgent… (…)` indented by
      four spaces; an `^--- PASS: TestIntegrationTestSuite/` pattern matches
      **zero** lines and fails even a fully green run. (`grep -c -- '--- PASS:
      TestIntegrationTestSuite/'` is an equally acceptable form.) With this,
      a hollow lane (0.068s `ok`, L8) can never be green again; (e) update the
      stale "stood up by the plan's harness item (H-1)" comment in
      `test/incident_gating_test.go` to name this plan. Rename nothing
      (`integration-test-agent`, the CI job `build-and-integration-test-agent-auth`,
      `CAESIUM_AGENT_AUTH_LANE`). Every lane that runs its own server per arc
      convention 2 is unaffected — this lane already does. Files: `justfile`
      (`integration-test-agent`, `agent_integration_run`), new
      `test/auth_lane_test.go`, `test/agent_mcp_test.go`,
      `test/agent_remediation_cli_test.go`, `test/incident_gating_test.go`.
- [ ] H-2. Give every lane an explicit time budget and the hollow-lane guard.
      The default recipe `integration-test` passes no `-timeout` (Go's 10m
      default; L12 shows the suite now takes longer), `integration-test-podman`
      and the helm job pass `10m`/`15m`. Set `-timeout 30m` on
      `integration-test`, `integration-test-podman`, `integration-test-infra`
      (already 20m), and the helm/podman `go test` lines in `ci.yml`;
      raise `timeout-minutes` on those jobs to match; add the same
      indentation-tolerant `--- PASS` count guard as H-1(d)
      (`grep -cE '^[[:space:]]*--- PASS: TestIntegrationTestSuite/'` — never
      anchored at column 0, see H-1(d)) to every lane that filters with
      `-run` (`integration-test-distributed`, `-owner-memory`,
      `-infra`, `-agent`) with a per-lane minimum. Files: `justfile`
      (`integration-test`, `integration-test-podman`,
      `integration-test-distributed`, `integration-test-owner-memory`,
      `integration-test-infra`), `.github/workflows/ci.yml`
      (`helm-integration-test`, `podman-integration-test`,
      `build-and-integration-test*` `timeout-minutes`).

## Navigational / Organizational Improvements

- [ ] N-1. `README.md` — the sole README editor in W2. (a) Add a section
      **"Beyond scheduling — what you can ask Caesium"** between `## Why
      Caesium` and `## Local Developer Experience`, one line + doc link each
      for: `caesium why` (per-task causal explainer — `docs/design-data-plane-memory.md`),
      `caesium blame` (which change broke the run — `docs/design-data-plane-memory.md`),
      `caesium run diff` (two runs, what differed — same), `caesium run
      replay` (quarantined re-execution — `docs/design-quarantined-replay.md`),
      `caesium reproduce` (rebuild one task locally — `docs/reproduce.md`),
      `caesium receipt get` / `caesium verify` (signed execution receipts —
      `docs/design-data-plane-memory.md`), `caesium contract check|graph`
      (cross-job schema contracts — `docs/design-contract-enforcement.md`),
      `caesium dataset status|list|advance` (freshness — `docs/design-freshness-scheduling.md`),
      `caesium backfill` (`docs/backfill.md`), `caesium incident` + the agent
      runtime and its MCP tools (`docs/design-agent-in-the-loop.md`), the
      infra-deploy reagents (`docs/infrastructure-deployment.md`). (b) Add a
      real step 0 to Quick Start: download `caesium-linux-<arch>` from the
      `v0.1.0` release (URL pattern, `chmod +x`, `SHA256SUMS`), or `just
      tag=v0.1.0 cli` on macOS/when Docker is available; keep `just run` as
      the from-source path. (c) Repoint the Codecov badge from
      `branch/develop` to `branch/master` (or drop it if Codecov is not
      wired for master — check `codecov` in `ci.yml`: today there is no
      upload step, so drop it and say why in the PR). (d) bump the Quick Start job's stale base-image tag (3.20) to the
      canonical `alpine:3.23`. (e) Remove the GraphQL sentences
      if F1 has not already (F1 runs in W1). Keep the sovereignty pitch as
      the close — the loop-led rewrite is the arc's closing wave (Tell it),
      not this item. Files: `README.md`. Depends on: E1 + E2 + F1.
- [ ] N-2. Onboarding. New `docs/getting-started.md`: install (step 0 from
      N-1, by digest per E4) → `caesium start` in a container / `just run`
      → first job (`docs/examples/*.job.yaml`, pinned `alpine:3.23`) with
      `caesium dev --once` → `caesium job apply` → trigger a run →
      `caesium why <run> --task … --job-id …` and `caesium receipt get`,
      every command copy-pasteable and byte-consistent with README.
      Reorganise `docs/README.md` into **Use Caesium** (getting-started,
      operator docs, tours, references — including `docs/ci.md` from D2)
      and **Design records** (strategy, `design-*`, exec plans, archive);
      it MUST still index every top-level `docs/*.md` exactly once and keep
      subdirectory references in backtick form
      (`TestDocsREADMEIndexesEveryTopLevelDoc`). Files: new
      `docs/getting-started.md`, `docs/README.md`. Depends on: D2 + N-1.
- [ ] N-3. File the unfiled follow-ups as GitHub issues. The item produces
      the `gh issue create` commands (title, body with the citing plan and
      symbol, labels) in a scratch file the user runs; the PR records the
      resulting issue numbers in this item. Group as: **scheduler** —
      fairness/quotas (`concurrency-priority-queues.md`), queue-view
      stale-claimed rows + claim-order pending-wait hardening +
      `cancelRunTx` read optimisation, `park` run disposition
      (circuit-breaker Phase 3 leftover, parked in the arc), local-executor
      quarantined replay (`internal/job/job.go` `ErrLocalQuarantinedReplayUnsupported`),
      selective per-task re-run for `replay --set`; **data plane** —
      freshness consumed-snapshot timing (only if B3 filed instead of fixed),
      partition-level freshness watermarks, blame tiebreak determinism +
      full-descriptor blame + author/ref attribution (`data-plane-memory-ii.md`),
      private-registry `RegistryAuth` + Podman/k8s pre-run digest resolution
      (`data-plane-memory.md` A1), reproduce OQ#1/#4/#5 (`reproduce.md`),
      `GET /v1/jobs/:id/manifest` export endpoint, `CallbackRun`
      `http_status`/`response_body`/`retry_count` enrichment; **UI** —
      event-trigger UI follow-on plan (`event-trigger-routing.md` deferred
      it), ImpactNode `run_id` click-through + dataset-from-run derivation +
      receipt-verify field-level drift (`data-plane-memory-ui.md`), Console
      React Flow watermark decision; **platform/ops** — node-affinity /
      co-location for RWO volumes in distributed mode (volumes spec),
      audit-log for operator-level notification mutations,
      `data-plane-memory-ii` optional H-4 distributed CI tier, infra-deploy
      RWX kind lane + podman lane coverage. Also file anything D1 quarantined
      and, from this plan's own recon, the darwin CLI binary (E1) if wanted.
      Files: this doc (issue numbers), scratch `gh` script (not committed).
- [ ] N-4. Close out (runs last). This item carries **all** of Plan 0's
      convention-7 close-out obligations (see the Source-Of-Truth Note's
      deviation list — N-1 here is the README rewrite, not the docs item).
      (a) Tick the Plan 0 row in the arc dashboard
      (`closed-loop-arc.md` § Arc dashboard: waves shipped, last PR) and set
      the Status cell of Plan 0's row in the arc's `## Sequence` table to
      Shipped. **Do not add a status column to `docs/roadmap.md`'s Phase 5
      table** — it has four columns by design and arc convention 7 puts status
      in exactly one place (the arc dashboard); the roadmap's row 0 change is
      only to repoint its plan link from `exec-plans/active/` to
      `exec-plans/completed/`. (b) The two sibling docs that described this plan as
      fixing "the five known bugs" (Ledger L16) were corrected at draft time
      to "fix the six ledger bugs, close the tier-3 approval loop" —
      `docs/roadmap.md` Phase 5 row 0 and the `trust-the-substrate.md` bullet
      in `docs/README.md`; re-check they still match the arc's acceptance
      criterion 1 ("the six ledger bugs (L1, L3, L5, L6, L7, L9)") at close-out
      that names the count. (c) Move this file to
      `docs/exec-plans/completed/trust-the-substrate.md`, repoint the links
      in `closed-loop-arc.md`, `docs/README.md` and `docs/roadmap.md`, and
      record the `v0.1.0` release URL in the arc dashboard notes. Files:
      `docs/exec-plans/active/closed-loop-arc.md`, `docs/roadmap.md`,
      `docs/README.md`, this file (moved). Depends on: every other item
      (including E4, the user's tag push).

## Sequencing & Dependencies

**Cross-stream order.**

- H-1 first in W1: Stream C's scenarios (C1–C3, C5) and A2/A5's justfile
  edits depend on it; nothing depends on C otherwise.
- C3 depends on C4 and C7 (a real proposal must exist, and approval must
  execute it, before approve/reject can be driven end-to-end); C8 (the
  explainability item) depends on C7 for the event seam and on C3 for the
  approved-`skip_task` run its scenario explains, so the chain is
  C4 → C7 → C3 → C8.
- Streams A, B, F are independent of each other and of C except through the
  shared files below; all four run in W1.
- Stream D depends on nothing in W1 but must follow it (D2 requires the
  widened auth lane to be green as a required check). H-2 precedes D1
  (timeout half of L12) and D2.
- Stream E is independent of D except E4 (needs D2's required checks green)
  and the `ci.yml` rule below.
- N-1 depends on E1 + E2 (install step) and F1 (GraphQL scrub); N-2 depends
  on D2 (docs/ci.md indexed) and N-1 (consistent commands); N-3 depends on B3
  and D1 only for their "filed instead of fixed" outcomes; N-4 depends on
  everything, E4 included.
- Suggested waves: **W1** = H-1 → {A1, A3, A4, B1, B2, B3, C1, C2, C4, C6,
  F1, F2} → {A2, A5, C5, C7} → {C3} → {C8}. Inside the first brace, **B2 and
  C6 both append to `test/job_test.go`** — sequence B2 → C6 (see the
  file-conflict list). **W2** = H-2 → {D1, D3, E1, E2, N-3} → {D2, E3}
  → {N-1} → {N-2} → E4 (user) → N-4. Everything after the first brace in W2
  is a serial tail; if the wave skill prefers, N-1/N-2/N-4 are a short W3.

**Within-stream order.**

- A: A1 → A2; A3, A4 in parallel → A5.
- B: B1, B2, B3 fully parallel.
- C: C4 → C7 → C3 → C8; C1 → C5; C2 and C6 independent.
- D: H-2 → D1 → D2; D3 independent.
- E: E1 → E3 → E4; E2 independent.
- F: F1, F2 independent (same wave, disjoint files).

**Cross-stream file conflicts.**

- `justfile`: H-1 (W1, `integration-test-agent`), then A2/A5 (W1, the
  distributed/owner-memory `-run` regexes — rebase after H-1); in W2, H-2
  (timeouts/guards on existing recipes), E2 (new `cli` recipe after
  `push-multiarch`), D3 (new `clean-worktrees` recipe at EOF) — different
  recipes, sequence H-2 first and let E2/D3 rebase mechanically. Arc rule
  "one plan's H-1 per wave" is satisfied (this is the only plan running).
- `.github/workflows/ci.yml`: D1/H-2 edit the **test** jobs; E1/E3 edit the
  **publish** job only (job-level `permissions`, not top-level). Same wave is
  acceptable; H-2 merges first, E1 rebases.
- `cmd/start/start.go`: A3 (cancel-registry subscriber), C4
  (`SetActionExecutor`) and C7 (incident-service executor setter) all add
  lines in W1 — additive; A3 merges first, C7 rebases onto C4.
- `internal/incident/executor.go`: C4 (`decisionApprove` branch) then C7
  (`ExecuteApproved`) then C8 (the executed-action event on the finish path)
  — sequential, C4 first. `internal/incident/actions.go` (`ActionOps` +
  `dispatch`) and `cmd/start/incident_ops.go` (`incidentActionOps`): C7 only.
- `test/job_test.go`: **B2** (`TestJobTasksSerialiseSnakeCaseIDs`, the
  snake-case JSON assertion) and **C6** (`TestJobUnpauseRoute`) both append to
  it in W1. Additive but the same file: sequence B2 → C6 and let C6 rebase, or
  move C6's unpause scenario into a new `test/job_pause_test.go` and drop the
  edge entirely (preferred if the two land in the same sub-wave).
- `test/incident_approval_test.go`: C3 creates it, C8 extends it — sequential,
  C3 first.
- `internal/event/bus.go` + `internal/notification/subscriber.go` +
  `internal/run/why.go` + `cmd/why/why.go`: C8 only (the arc file-conflict
  table's "one plan's explainability item per wave" rows — no other plan runs
  concurrently).
- `api/rest/controller/job/run/{post,retry,partitions}.go`: A3 only (the
  detached-run-context registration sites).
- `README.md`: N-1 only in W2 (D3's badge, E4's step 0 folded in); F1 edits
  it in W1 (GraphQL scrub) — different wave.
- `docs/README.md`: D2 (adds `docs/ci.md` entry) → N-2 (restructure) → N-4
  (repoint). Sequential.
- `internal/run/store.go` / `internal/run/fanout.go`: A1 only (A3 uses the
  `TypeRunCancelled` event as its seam and does not edit the store). D1(a)
  may touch `RetryPartition` in W2 — different wave.
- `internal/job/job.go`: A3 only. `internal/worker/*`: A4 only.
- `test/integration_test.go`: nobody — new helpers go in new files
  (`test/auth_lane_test.go`, `test/auth_scoped_test.go`, …).
- `go.mod`/`go.sum`: F1 only (removing `graphql-go`). No stream adds a
  dependency; if one appears, resolve `go.sum` with `go mod tidy`.
- `ui/src/lib/api.ts`: B2 only. `ui/e2e/**`: C1 only.
- Arc file-conflict table: this plan's A-stream edits to `internal/run/store.go`
  and `internal/job/job.go` + `internal/worker/runtime_executor.go` are the
  `0-A` rows; no other plan runs concurrently.

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Conditional gates for this plan:

- Streams A, C (scenarios), H: `just integration-test-agent` (C1–C5, C8),
  `just integration-test-distributed` and `just integration-test-owner-memory`
  (A2, A5).
- `ui/**` changes (B2, C1): `just ui-lint && just ui-test && just ui-e2e`;
  C1 additionally `just ui-e2e-auth`.
- `helm/**` (E3): `just helm-lint && just helm-template`.
- Podman-affecting changes (D1(b)): `just integration-test-podman`.
- Any doc that names an image uses `alpine:3.23` / `busybox:1.36.1`
  (`TestPinnedContainerImageVersionsAreConsistent` scans `docs/`); any new
  top-level `docs/*.md` is indexed in `docs/README.md` in the same PR
  (`TestDocsREADMEIndexesEveryTopLevelDoc`); `docs/job-schema-reference.md`
  is generated — never edited by hand (no item here touches the schema).
- This plan's checkbox ticked, per-stream `## Progress` bullet appended for
  the active wave, and any cross-linked doc refreshed in the same PR.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream A — scheduler correctness** is true in the SQL lane:
   `TestPlainFailureReleasesAllDoneFannedConsumer` (default lane) and
   `TestPlainFailureReleasesAllDoneConsumerDistributed` (distributed and
   owner-memory lanes) are green in CI; each was demonstrated **red on master
   before A1 on the default and distributed lanes** (link the failing run in
   the PR). On `-owner-memory` the same scenario is a **green-before /
   green-after regression guard**, not a reproduction — that lane resolves
   completions through the already-correct in-memory owner path
   (`internal/run/owner_state.go` `RunState.ApplyCompletion`; see Stream A's
   preamble and A2(b)), so a red run there is not required and not expected;
   `TestReplaceCancelStopsOrphanedContainer` is green on the default and
   distributed lanes; the `internal/run` unit test for A1 and the
   `internal/job` / `internal/worker` unit tests for A3/A4 exist.
2. **Stream B — data-plane truth:** `TestLineageImpactReturnsDownstream`
   asserts a non-empty `producing_step` and `TestStepNameFromFacet`
   round-trips through `persistTaskDatasets`; `TestJobTasksSerialiseSnakeCaseIDs`
   is green and `RawJobTask`/`normalizeJobTask` no longer exist in
   `ui/src/lib/api.ts`; B3 is green as
   `TestFreshnessConsumedSnapshotTakenAtRunStart`.
   **Carve-out (needs arc sign-off before it is exercised):** B3's
   "file instead of fix if unbounded" escape conflicts with arc acceptance
   criterion 1, which requires all six ledger bugs — L7 included — to have an
   integration scenario that failed before and passes after. If the
   investigation finds the fix genuinely unbounded (a persisted `Capturer`
   table), the implementer must **stop and get the arc doc amended** (record
   the L7 exception in `closed-loop-arc.md` AC 1) in the same PR that files
   the issue; a filed-not-fixed B3 does **not** close this criterion on its
   own. Preserved rationale for the escape hatch: the freshness `Capturer` is
   an in-memory subscriber, and a start-time snapshot that must survive a
   server restart is a schema change this plan has no other reason to make.
3. **Stream C — auth surface:** on `just integration-test-agent`,
   `TestAuthKeyLifecycleCLI`, `TestAuthAuditCLI`, `TestAuthKeysREST`,
   `TestScopedKeyWhoamiAllowed`, `TestScopedKeyAllowDenyMatrix` and
   `TestIncidentApprovalDecisionsCLI` PASS (visible as `--- PASS` lines in
   the CI log, not skips); `TestIncidentApprovalDecisionsCLI` reaches
   `awaiting_approval` through `POST /v1/agent/incidents/:id/actions`, not a
   seeded row, and its approved `skip_task` / direct `apply_jobdef_patch`
   actions are `executed` with observable effect while a git-synced job's
   patch degrades to `escalate` with the rendered diff; an
   `apply_jobdef_patch` attempted under `CAESIUM_AUTH_MODE=none` is refused
   with a typed reason (arc convention 4) — asserted as a C6 row on the
   default lane; `TestIncidentApprovalWhyExplains` (C8) PASSes on the auth
   lane and its `runCLIStdout` capture of `caesium why <run> --task … --job-id
   …` names the decider and the executed action, and
   `TypeAgentActionExecuted` is a persisted event (arc convention 3 and arc
   AC7); `ui-e2e-auth` asserts a scoped key gets 200 on
   `/auth/whoami`, and `ui-e2e-auth` is in D2's required-check list (it is the
   only job that runs that assertion); on the default lane `TestJobUnpauseRoute`,
   `TestRunRetryCallbacksCLI`, `TestNotificationChannelAndPolicyByID`,
   `TestNodeWorkersRoute`, `TestBackfillCLILifecycle` are green; the stale
   H-1 comment in `test/incident_gating_test.go` is gone.
4. **Stream D — CI gates merges:** `gh api repos/caesium-cloud/caesium/branches/master/protection --jq .required_status_checks.checks`
   lists exactly D2's list, byte-identical: `lint`, `unit-test`,
   `unit-test-arm64`, `ui-test`, `ui-e2e`, `ui-e2e-auth`,
   `build-and-integration-test`, `build-and-integration-test-agent-auth`;
   `docs/ci.md` exists, is indexed, and records the command + list + lane
   matrix + the required-to-merge vs. required-to-publish distinction
   (`publish.needs` is wider); the last 5 master runs after D1/H-2 have no failure in the four
   L12 lanes that is not a filed, linked quarantine; `git ls-files
   ui/test-results` is empty; `just clean-worktrees` exists.
5. **Stream E — release & install:** `https://github.com/caesium-cloud/caesium/releases/tag/v0.1.0`
   exists with `caesium-linux-amd64`, `caesium-linux-arm64`, `SHA256SUMS`;
   `docker manifest inspect caesiumcloud/caesium:v0.1.0` lists
   `linux/amd64` and `linux/arm64`; `helm/caesium/Chart.yaml` `appVersion`
   equals the tag and the rule is documented; `just tag=v0.1.0 cli` yields a
   runnable `./.tmp/caesium-cli/caesium --help`.
6. **Stream F — dead scaffolding:** `api/gql/`, `internal/task/`,
   `pkg/client/` are gone; `pkg/bytes`/`pkg/compare` are gone or the
   importer that kept them is named in F2; `go.mod` has no `graphql-go`
   lines; `grep -rniE 'graphql|/gql' README.md CLAUDE.md AGENTS.md
   CONTRIBUTING.md api .claude/skills` is empty (the same command F1 names —
   historical exec plans under `docs/exec-plans/**`, superpowers specs under
   `docs/superpowers/specs/**` and the gitignored `.claude/worktrees/*`
   checkouts keep their GraphQL references as design record and are **not**
   in the scan set; only live surfaces are scrubbed); `just lint` and
   `just unit-test` are green.
7. **Harness:** the `build-and-integration-test-agent-auth` job log shows
   `--- PASS: TestIntegrationTestSuite/TestAgent…`, `…/TestAuth…`,
   `…/TestIncident…`, `…/TestScoped…` lines (indented — they are subtests;
   the guard counts them with `grep -cE '^[[:space:]]*--- PASS:
   TestIntegrationTestSuite/'`, never a column-0 anchor) and the recipe fails
   on fewer than 3; every lane passes an explicit `-timeout`.
8. **Navigational:** `README.md` has the "Beyond scheduling" section naming
   every verb in N-1(a) with a link, an install step 0 that works without
   cloning the repo, no `develop` badge, `alpine:3.23`; `docs/getting-started.md`
   exists and is indexed; `docs/README.md` is split into Use Caesium / Design
   records and `TestDocsREADMEIndexesEveryTopLevelDoc` is green; N-3's issue
   numbers are recorded in this file.
9. **Cross-cutting:** the arc dashboard row and the `## Sequence` Status cell
   for Plan 0 in `closed-loop-arc.md` reflect every shipped stream, and
   `docs/roadmap.md` Phase 5 row 0 + the `docs/README.md` bullet point at
   `exec-plans/completed/` and say "the six ledger bugs" (N-4(b)) — **no
   status column is added to the roadmap's Phase 5 table** (arc convention 7:
   status lives only in the arc dashboard); this plan's
   per-stream Progress entries match merged PRs (one squashed commit each,
   titled `<Imperative subject> (trust-the-substrate <wave>-<stream>)`);
   arc acceptance criterion 1 holds **verbatim** — it already reads "the six
   ledger bugs (L1, L3, L5, L6, L7, L9)", and after N-4(b) no sibling doc
   still says "five".

## How To Pick Up Work

1. Read this file end-to-end so you understand the streams, their
   interdependencies, and which acceptance criterion the item closes. Read
   the `## Recon Ledger` row(s) your item cites and re-grep the symbols —
   the ledger is a hint, the code is the truth.
2. Pick an unchecked item under `## Streams` whose `Depends on:` line
   is satisfied (consult `## Sequencing & Dependencies`).
3. Branch from `master` (or land in a worktree if dispatched by
   `exec-plan-wave`); do the work as a self-contained PR.
4. Run the verification block under `## Verification (Run For Every
   PR)`, including the conditional gate for your stream — for Stream C that
   means `just integration-test-agent` locally and a CI log showing
   `--- PASS` lines, not `ok … 0.068s`.
5. Tick the checkbox for your item, add a per-stream bullet to the
   active wave subsection in `## Progress` (or open a new wave
   subsection if none exists yet), and update any cross-linked design
   doc / roadmap section in the same PR.
6. Open the PR with title format
   `<Imperative subject> (trust-the-substrate <wave>-<stream>)` —
   e.g. `Advance failed plain-task successors in the SQL lane (trust-the-substrate W1-α)`.
   GitHub appends `(#NNN)` on squash-merge.

## Cross-References

- [`closed-loop-arc.md`](closed-loop-arc.md) — the umbrella arc; scope
  authority; shared conventions 1–8; the `0-A` rows of its file-conflict
  table; arc acceptance criterion 1.
- [`docs/roadmap.md`](../../roadmap.md) Phase 5 — the Plan 0 row.
- `CLAUDE.md` § "End-to-end coverage is the gate" — the rule every C-stream
  and A2/A5/B scenario follows (CLI binary or live HTTP; stdout captured
  separately).
- [`../completed/dynamic-fanout.md`](../completed/dynamic-fanout.md) and
  PR #384 (`48497e9`) — where L1 was found; the route-completeness contract
  `resolveInstanceFailureTx` cites.
- [`../completed/concurrency-priority-queues.md`](../completed/concurrency-priority-queues.md)
  and PR #275 (`14ec203`) — the replace-cancel resurrection fix whose
  message says it "does not kill the detached container" (L3).
- [`../completed/data-plane-memory-ui.md`](../completed/data-plane-memory-ui.md)
  — #250 (`RawJobTask` shim, L6) and #255 (`producing_step` descoped, L5).
- [`../completed/freshness-scheduling.md`](../completed/freshness-scheduling.md)
  Stream B known limitation — L7.
- [`../completed/agent-in-the-loop-remediation.md`](../completed/agent-in-the-loop-remediation.md)
  D1 — the approval flow whose creation half is missing (L9).
- [`../../design-agent-in-the-loop.md`](../../design-agent-in-the-loop.md),
  [`../../design-data-plane-memory.md`](../../design-data-plane-memory.md),
  [`../../design-freshness-scheduling.md`](../../design-freshness-scheduling.md)
  — the designs the fixed surfaces belong to.
- [`../../parallel-execution-operations.md`](../../parallel-execution-operations.md)
  — lane/env reference D2's `docs/ci.md` links to rather than duplicates.
- [`../../kubernetes-deployment.md`](../../kubernetes-deployment.md) — E3's
  chart-versioning rule lands there.
- `.claude/skills/exec-plan-wave/` — wave orchestration; `justfile` and
  `.github/workflows/ci.yml` — the lanes H-1/H-2 change.
