# Trust the Substrate — Plan 0 of the Closed-Loop Arc

Last updated: 2026-09-07

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
| L14 | `helm/caesium/Chart.yaml` is already `version: 0.1.0` (only `appVersion` is `"latest"`), so E3's chart work is an `appVersion` pin, not a `version` bump. `caesiumcloud/caesium:latest` **exists** on Docker Hub, but the `publish` job has never run (no `v*` tag; `gh release list` empty) and it pushes only `${IMAGE_TAG}` (never `latest`); `helm/caesium/values.yaml` `image.tag: ""` defaults to `Chart.appVersion` = `"latest"`, so every Helm install today pulls an image CI never built. `just run` builds from source. `build/Dockerfile.build` builds with `GOOS=linux GOARCH=${TARGETARCH}` against a CGO dqlite built in the same stage — no darwin target is possible from this builder — and the result is **dynamically linked**: `build/Dockerfile`'s builder stage runs `ldd /dist/bin/caesium` to collect its `.so` closure into the image, so the executable alone is not a host binary. | justfile `run`/`push`/`push-multiarch`; `.github/workflows/ci.yml` `publish`; `helm/caesium/Chart.yaml`; `build/Dockerfile.build` | E1 is linux-only and must ship a statically linked (or lib-bundled) artifact with a bare-host smoke test; E3 pins `appVersion` to the tag. |
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

## Progress (as of 2026-09-07)

Wave 1 shipped Streams A, B, C, F and H-1 (plus the time-budget half of H-2)
across eight PRs, 2026-09-06 → 2026-09-07. Every PR passed the orchestrator's
scope-aware integration gate before merge (lint + unit + the lanes its diff
touches), and the two PRs the review bot did not review (#392, #390's fix
commits) got a substitute adversarial review that found real P1s. Wave 2
(Streams D, E, N-1..N-4 and the guard half of H-2) is the next eligible run.

### Wave 1 — Substrate true and auth surface real (2026-09-06/07)

- **α / H-1** — [#386](https://github.com/caesium-cloud/caesium/pull/386)
  `e0f05be`. The agent-auth lane executes its scenarios (was 0 in 0.068 s →
  5, now 29 after the C streams) with a `--- PASS` floor
  (`agent_integration_min_pass`). Two things the item missed: the runner also
  needs `CAESIUM_AUTH_KEY_HASH_SECRET`, and `pkg/db.Connection()` in the
  runner would `log.Fatal` on the shared netns (replaced by a dqlite-client
  gorm handle). Review: greptile 5/5, no findings.
- **δ / F1–F2** — [#387](https://github.com/caesium-cloud/caesium/pull/387)
  `a32413c`. `api/gql`, `internal/task`, `pkg/client`, `pkg/bytes`,
  `pkg/compare` removed; `go.mod` tidied. One P2 declined with rationale
  (`/gql` now falls to the SPA catch-all like any unknown path; keeping the
  literal would fail AC 6).
- **H-2 (time-budget half)** — [#389](https://github.com/caesium-cloud/caesium/pull/389)
  `3bd2870`, orchestrator fix-forward: every integration `go test` line now
  passes `-timeout 30m` (the default lane had reached 520 s of Go's 600 s
  default and CI was failing on the timeout, L12). The `--- PASS` floor half
  of H-2 remains open.
- **γ / B1–B3** — [#388](https://github.com/caesium-cloud/caesium/pull/388)
  `949276c`. Lineage facet written nested with a real-write-path round-trip
  test; `models.Task` JSON tags (five fields, L6 corrected — two `test/`
  helpers were real consumers); freshness consumed view captured
  **synchronously at run creation** through a `StartParamsEnricher` seam in
  `internal/run/store.go` (three review rounds: the async `run_started`
  snapshot was replaced; a read error now omits the view rather than writing
  `{}`; the start-time view lives under its own key so
  `hasActiveOrQueuedRun`'s dedupe on the derivation-time key is untouched).
  The lane caught a real deadlock in the first cut (the event router creates
  runs over its own open write transaction — the enricher must read through
  the store's own handle; pinned by `TestStartRunEnricherReadsTheStoresOwnHandle`).
- **ζ / C1, C2, C5, C6** — [#391](https://github.com/caesium-cloud/caesium/pull/391)
  `deb7540`. Scoped-key `whoami` allowed for API-key principals only; auth
  key/audit CLI + REST scenarios; the scoped allow/deny matrix (13 rows); the
  default-lane gap scenarios. Three shipped bugs found by driving the real
  surface: every `caesium auth`/`backfill` command printed to **stderr**
  (`auth key create > key.txt` lost the one-time key), `job apply` sent no
  `Authorization` header, `run retry-callbacks` had no `--server` mode. One
  P1 (prose + JSON on stdout) fixed by making stdout exactly one JSON value
  across `backfill` and `auth`. Follow-up [#393](https://github.com/caesium-cloud/caesium/pull/393)
  `f35b9ba` skips `TestRunRetryCallbacksCLI` on the kind lane (the in-cluster
  server cannot reach the test process's callback receiver).
- **β / A1–A5** — [#392](https://github.com/caesium-cloud/caesium/pull/392)
  `192f604`. Failed plain tasks advance their successors in the SQL lane;
  run cancellation reaches the container on both lanes (local cancel
  registry; worker per-tick claim inspector — L3's `RowsAffected` premise was
  wrong because batched renewal only fires near lease expiry). The substitute
  adversarial review found 2 P1s + 2 P2s, all fixed with red-before proof:
  trigger-originated runs (cron/http/event/webhook) were still uncancellable
  (registration moved inside `job.Run`); `retryTask` lacked a terminal guard
  and resurrected a cancelled row; the worker's `continue`-policy descendant
  sweep ignored trigger rules; a failed fan-out producer released its
  consumer's unexpanded template row (now skipped with a reason). A Go
  `select` race between `taskCtx.Done()` and the wait result left ~half of
  cancelled containers orphaned (surfaced on arm64; fixed via `abandonAtom`).
  A2 deviation: under `CAESIUM_TASK_FAILURE_POLICY=halt` no lane dispatches a
  tolerant consumer after a failure (both lanes agree, by design); A2 asserts
  the release and the byte-exact rule skip, and asserts execution under
  `continue` — the `halt` semantics are an N-3 item.
- **ε / C3, C4, C7, C8** — [#390](https://github.com/caesium-cloud/caesium/pull/390)
  `fd164f3`. The tier-3 approval pipeline is wired at both ends: proposal →
  `ApprovalRequest` + `awaiting_approval` (one transaction), human approve →
  execute (`ExecuteApproved` behind a conditional claim, plus a leader-gated
  `ApprovalRedriver`), `skip_task` / `override_schema_gate` / direct
  `apply_jobdef_patch` dispatch, `TypeAgentActionExecuted` rendered by the
  task-scoped `why`. Greptile 0/5 with five P1s on the first commit, all
  fixed (job-scoped playbook fail-closed; atomic approval; session container
  stop; redrive; delivered escalation), then a substitute review of the fix
  found one more P1 — an approved patch could rewrite the job's own
  `metadata.remediation` — now refused (`ErrPatchAltersRemediation`) with the
  field added to the diff vocabulary; playbook nil-vs-empty semantics made
  explicit (`Narrow` → `Override`; the shipped `triage-only` profile's
  `allow: []` had decoded as unconstrained). 29 agent-lane scenarios incl.
  `TestIncidentApprovedActionRedriveRecoversAfterCrash` and
  `TestIncidentApplyJobdefPatchCannotEditItsOwnPolicy`.

**Flakes classified (not merge-blocking):** `no such vfs` in
`internal/trigger/event` (×4, re-run green; D1(c)); the pre-#389 10-minute
default-lane timeout (fixed); `TestFanOutHTTPRetryPartition` on owner-memory
(L12 family); one owner-memory `TestFanOutReplayOverrideReexecutesRecordedGroup`
stall traced to the owner dispatch loop re-dispatching a completed producer
against a full worker pool (pre-existing, N-3).

**Wave-1 process notes:** parallel worktree agents share one Docker daemon —
every server lane runs under a host `mkdir` lock with a PID-first owner line
reclaimed only when the owner is dead (an unconditional reclaim stole a live
lock twice and produced two rounds of `connection refused` false failures).
Three Opus session-limit kills cost ~6 h; cap concurrent Opus streams at 3.

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Scheduler correctness in the SQL lane — failed-plain-task advancement, run-cancel reaches the container | **P0** | **Shipped** (W1, #392) |
| B | Data-plane truth — lineage facet shape, `Task` JSON tags, freshness consumed-snapshot timing | **P0** | **Shipped** (W1, #388) |
| C | Auth surface end-to-end on the (widened, de-hollowed) auth lane; the tier-3 approval pipeline wired at both ends (proposal → `ApprovalRequest`, approve → execute, direct `apply_jobdef_patch` route), and its explainability item (C8) | **P0** | **Shipped** (W1, #391 + #390) |
| D | CI gates merges and master is honestly green | P1 | Not started |
| E | Release & install — `v0.1.0`, per-arch CLI binaries, `just cli`, chart versioning | P1 | Not started |
| F | Dead scaffolding — `api/gql`, `internal/task`, `pkg/client`, `pkg/bytes`, `pkg/compare` | P2 | **Shipped** (W1, #387) |
| H | Harness — the auth lane that actually runs (H-1), lane time budgets + hollow-lane guard (H-2) | **P0** | H-1 **Shipped** (W1, #386); H-2 time budgets shipped (#389), pass-floor half open |
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

- [x] A1. Advance a failed plain task's successors in the SQL lane the way the
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
      **Done (W1-β).** `resolveInstanceFailureTx`'s `!isFanOutInstance(row)`
      early-out now calls `advanceCrossStepSuccessorsTx`; the fanned branch
      keeps its `groupAllTerminalTx` gate and `row == nil` still returns.
      `internal/run/store.go` needed no change. New
      `internal/run/store_plain_failure_test.go` drives BOTH routes
      (`FailTask`, `CompleteTask(result "failure")`) over a
      `produce → fan(all_done) ← gate → tail(all_success) → tail-child` fixture
      and pins the byte-identical `trigger rule %q not satisfied` reason, one
      `task_skipped` per task (L2 idempotence, asserted by re-issuing the
      executor's own `store.SkipTask` cascade) and a negative control that a
      fanned instance still gates on group-terminal. Two existing tests
      asserted the OLD behaviour and were updated in place:
      `internal/run/fanout_failure_route_test.go`
      `TestCompleteTaskFailureResultLeavesUnfannedTasksAlone` →
      `…ResolvesUnfannedSuccessorsByRule`, and `internal/job/job_test.go`
      `TestRunLocalContinuePolicySkipsFailedDescendants`, whose skipped row now
      carries the store's rule reason instead of the local executor's
      `skipped due to failed dependency task <id>` (the store resolves first;
      the executor's later cascade is a pending-only no-op). That reason change
      is user-visible and deliberate — it is the string the local, fanned and
      distributed lanes now all emit.
      **Two corrections from adversarial review.** (i) A1 did not actually
      deliver in distributed mode under `CAESIUM_TASK_FAILURE_POLICY=continue`:
      the worker's post-failure sweep (`collectDescendantsFromEdges`) walked
      every transitive descendant with **no trigger-rule filter**, unlike the
      local `skipDescendantsFiltered`, so the `all_done` consumer the store had
      just released was marked `skipped` before the owner's next dispatch tick.
      The predicate now lives once in `internal/run` as
      `IsTolerantTriggerRule` — `internal/job` imports `internal/worker`, so
      that is the only direction a shared helper can point — and both sweeps use
      it; a tolerant node also stops the walk, as the local one always did.
      Covered by `internal/worker/descendant_skip_test.go`. (ii) A failed
      fan-out PRODUCER released its consumer's unexpanded template row
      (`partition_count = 0`, `partition_value = ''`), which no dispatch
      predicate can tell from an ordinary unfanned task, so the fanned step
      would have run once, unpartitioned, with no `CAESIUM_PARTITION`.
      Expansion only ever happens inside the producer's completion transaction,
      so such a template can never materialize: `advanceCrossStepSuccessorsTx`
      now skips it with `fan-out producer %q did not produce a partition list`,
      the resolution `docs/design-dynamic-fanout.md` already prescribes for a
      group that cannot exist (`onEmpty: skip`). Pinned by
      `TestFailedFanOutProducerSkipsUnexpandedConsumerTemplate`, which also
      asserts `PendingTasksForDispatch` is left empty. Excluding templates in
      the three dispatch predicates was considered and rejected: none of them
      can distinguish a template without joining `tasks.fan_out_config`, and the
      row should never be left pending in the first place.
- [x] A2. Integration scenarios that fail before A1 and pass after, on the lanes
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
      **Done (W1-β), with one deviation recorded.** Both scenarios live in
      `test/trigger_rule_failure_test.go`; `TestPlainFailure` is in both `-run`
      regexes. **Deviation:** neither scenario asserts that the tolerant
      consumer *executes*, because no lane as configured can execute it —
      `CAESIUM_TASK_FAILURE_POLICY` defaults to `halt` (`pkg/env/env.go`) and no
      `integration-up*` recipe overrides it, so on a failure the local Kahn loop
      clears its queue (`internal/job/job.go`,
      `if !continueOnFailure { halt = true; queue = queue[:0] }`) and the
      distributed waiter finalizes the run (`waitForRunCompletion`,
      `failed > 0 && running == 0` → `"run %s halted after %d failed task(s)"`)
      before `ClaimNext`'s `jr.status = running` predicate could match the
      released row. Asserting execution would assert the failure policy, or
      race it: `CAESIUM_WORKER_POOL_SIZE=1` means nothing else is ever
      `running` at the instant of a failure, so the waiter's 500 ms tick and
      the worker's claim race on a ~50 ms window. The scenarios assert instead
      exactly what A1 changes and all three dispatch paths read: the `all_done`
      consumer reaches `outstanding_predecessors = 0` (exposed on
      `GET /v1/jobs/:id/runs/:run_id`) and no partition is swept
      "never dispatched", and the `all_success` consumer is `skipped` with the
      byte-exact rule reason instead of sitting `pending` on a terminal run.
      Red-before on the default lane, verified against the pre-A1 tree:
      `strict` `Status:pending`, `tolerant` `OutstandingPredecessors:1`. The
      `outstanding_predecessors` half is skipped on `-owner-memory`
      (`ownerInMemoryLane()`), which advances the DAG in memory and
      deliberately does not decrement the SQL scalar (`TestCompleteTaskOwner`:
      "owner path must not decrement successors in SQL"), so the rule-skip
      assertion is the regression guard there.
      **Corrected after review — the earlier claim that "no configuration
      produces the tolerant consumer executing" was wrong, and two real defects
      were hiding behind it.** Under `CAESIUM_TASK_FAILURE_POLICY=continue` the
      LOCAL executor has always run a released `all_done` consumer; the
      distributed lane did not, for two reasons now fixed under A1: the worker's
      unfiltered descendant sweep buried the consumer, and
      `waitForRunCompletion`'s `failed > 0 && running == 0` heuristic finalized
      the run in the gap between the failure transaction (which releases the
      successor) and the dispatcher's next tick, after which `ClaimNext`'s
      `jr.status = running` predicate refuses the row forever. A row that is
      pending with `outstanding_predecessors = 0` is precisely what the
      dispatcher is about to claim, so it is no longer treated as a stall.
      That guard is **deliberately scoped to `continue`**: under `halt` BOTH
      executors stop dispatching after a failure (the Kahn loop clears its
      queue), and making only the distributed waiter wait would have it run a
      tolerant successor the local lane refuses to — re-creating exactly the
      mode-dependent divergence this plan's route-completeness contract exists
      to prevent.
      So the scenarios still cannot assert execution, and the reason is now
      precise rather than a blanket claim: **no `integration-up*` recipe sets
      `CAESIUM_TASK_FAILURE_POLICY=continue`**, and adding one is a lane-env
      change owned by H-2, not by this stream. The remaining product question —
      should `halt` release rule-tolerant successors at all, in BOTH executors? —
      is an N-3 issue, not something to smuggle into a scheduler-correctness PR.
- [x] A3. Make run cancellation reach the local executor's container. Add a
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
      **Done (W1-β).** `internal/job/cancel_registry.go` holds a process-wide
      registry keyed by run id with a SET of cancel funcs per run (a partition
      retry runs a replacement engine against the same run id while the
      previous one drains, and a cancel must reach both);
      `RegisterRunCancel(parent, runID) (ctx, release)`,
      `CancelRunContexts(runID) int` and `SubscribeRunCancellations(ctx, bus)`
      are the API — the last so `cmd/start/start.go` gains exactly ONE additive
      line next to `runStore.SetBus(bus)` (plus the `internal/job` import).
      All five named sites register, **plus a sixth the re-grep found**:
      `api/rest/service/replay/replay.go` `AsyncDispatcher`, which detaches a
      quarantined replay run the same way. `internal/run/store.go` untouched.
      The `taskCtx.Done()` branch now force-stops on `context.Canceled` and
      reports a failed Stop rather than swallowing it. Unit tests in
      `internal/job/cancel_registry_test.go`, including
      `TestRunLocalCancelStopsAtom`, which drives the whole seam
      (`store.CancelRun` → `run_cancelled` on the bus → subscriber → registered
      run context → `engine.Stop(Force: true)`) against the existing fake
      engine and asserts the row stays `cancelled`; verified red without the
      `job.go` half.
      **Second correction, found by `unit-test-arm64`.** Stopping the atom on
      the `taskCtx.Done()` branch alone is not enough, because that branch wins
      only half the time: when the task context ends, `engine.Wait` returns
      `ctx.Err()` too (the real docker engine does this as well —
      `internal/atom/docker/engine.go` returns `waitCtx.Err()`), so
      `taskCtx.Done()` and `waitResult` become ready in the same instant and Go
      picks between them **uniformly at random**. The `waitResult` door returned
      the error without stopping anything, so roughly half of all cancelled
      containers were still abandoned — the exact orphan A3 exists to kill.
      arm64 surfaced it only because the slower runner lands the cancel before
      `Wait` starts polling more often; the race is arch-independent. Both doors
      now converge on one `abandonAtom` helper, which also force-stops on ANY
      wait error, matching what the distributed worker's `monitorTask` has
      always done ("Wait failed" means we stopped watching, never that the
      container stopped). Pinned by `TestRunLocalWaitErrorStopsAtom` via a new
      `waitErrByName` knob on the fake engine: the racing door cannot be
      selected on purpose, but the code behind it can be driven directly, and
      that test fails 100% of the time without the fix (the cancel test itself
      passed 40/40 locally pre-fix, which is exactly why it could not be the
      guard).
      **Third correction, from adversarial review: the six kickoff sites were
      the wrong place to register.** `job.Run` resolves the run id itself
      (`runID := snapshot.ID`), and the cron (scheduled *and* catch-up), http,
      event and webhook triggers all call `job.New(...).Run(ctx)` without one —
      so every trigger-originated run was uncancellable, and silently, because
      `CancelRunContexts` returns 0 and the log line is gated on `n > 0`.
      Registration now happens inside `job.Run` immediately after the run id
      exists, the single point all eleven paths converge on; the six kickoff
      registrations stay as belt-and-braces (they close the window between
      creating the run row and entering `Run`, and the registry holds a set per
      run id).
      **Fourth correction: the cancel was starting the container it existed to
      prevent.** Cancelling attempt 1 makes the attempt fail, and a `retries`
      budget turned that into attempt 2 — `retryTask` had no terminal guard
      (unlike its fanned twin `RetryTaskInstance`), so it flipped the cancelled
      row back to `pending`, `StartTask`'s guard then saw a legitimately pending
      row, and a second container started on a cancelled run, ending in a
      `failed` write that ran A1's advancement on a cancelled run. Fixed on both
      halves: `retryTask` gained the same status predicate and
      `ErrTaskInstanceNotRetryable` sentinel, and both executor attempt loops
      (the unfanned loop and `runFannedGroup`'s `dispatch`) refuse to start an
      attempt on a dead context — the `retryDelay > 0` select was the only
      previous check and the default delay is 0. `TestRunLocalCancelStopsAtom`
      is now table-driven over `retries` 0 and 1 and asserts exactly one
      container was created; without the fix the retry case creates a second
      (`…-attempt2`). `TestRetryTaskRefusesTerminalRow` pins the store half.
- [x] A4. Make run cancellation reach a distributed worker's container. A
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
      **Done (W1-β), with one file-placement deviation.** The per-task
      `context.CancelFunc` lives on `inFlightClaim` in
      `internal/worker/worker.go`, not in `pool.go`: the in-flight map is
      already the per-claim registry the renewal ticker reads, and `Pool` is a
      pure semaphore — splitting the state across the two would have let the
      cancel path race the tracking. `pool.go` and `runtime_executor.go` are
      untouched. `startOnReservedSlot` derives the executor's context from
      `context.WithCancel(execCtx)`; `renewLeasesNow` now acts on
      `RowsAffected < len(ids)` instead of discarding the count. A batch of one
      needs no probe (the batch named the loser); a larger batch identifies the
      losers by re-issuing the renewal one id at a time — a path a healthy
      worker never takes — so survivors are still renewed, and a probe ERROR is
      treated as "unknown", never "lost" (a database blip must not kill a
      container). Lost claims are cancelled and dropped from the in-flight set.
      Covers both causes: a cancelled run (blanked `claimed_by`) and a lease
      reassigned to another node. The pre-existing
      `TestBatchedRenewal_ZeroRowsAffectedNoLocalUpdate` asserted the old
      "keep the entry, leave the expiry" behaviour and is now
      `…ZeroRowsAffectedDropsTheClaim`.
      **Correction found by the distributed lane (this item's ledger row is
      incomplete).** `RowsAffected` alone is NOT a sufficient detector, because
      the renewal is only ISSUED when a claim is within `lease_ttl/2` of expiry
      — and on a run-owner lane `claim_expires_at` is stamped from the OWNER's
      dispatch deadline (`internal/dispatch/dispatch.go`
      `ttl := time.Until(req.Deadline)`, `CAESIUM_RUN_OWNER_DISPATCH_DEADLINE`,
      5m) rather than from `CAESIUM_WORKER_LEASE_TTL` (30s on the lane). A task
      cancelled seconds after dispatch is therefore not renewal-due for ~4m45s,
      `renewLeasesNow` short-circuits on `!needsRenewal`, and the container ran
      on: `just integration-test-distributed` failed
      `TestReplaceCancelStopsOrphanedContainer` with the container still alive
      after the full 90 s, and no claim-loss log line in the server output.
      The fix decouples the two questions: a new `ClaimInspector`
      (`run.Store.ClaimedTaskRunIDs`, one indexed `SELECT`, discovered from the
      `LeaseRenewer` by type assertion like `ExpiredReclaimer`) is asked on
      EVERY renewal tick by `Worker.cancelLostClaimsNow`, before
      `renewLeasesNow`'s unchanged skip-when-not-needed short-circuit.
      Detection latency is now one tick (`lease_ttl/4`). Seven tests in
      `internal/worker/run_lease_renewal_test.go`, including
      `TestClaimLivenessCancelsClaimThatIsNotRenewalDue` (a 5-minute expiry —
      the exact case the `RowsAffected` detector cannot see) and
      `TestRunLeaseRenewalTickCancelsLostClaim`, which drives the real ticker
      goroutine so dropping the call from `runLeaseRenewal` fails the build's
      tests rather than only the lane; that one was verified red against the
      pre-fix wiring. `internal/run/store.go` gains the read-only
      `ClaimedTaskRunIDs` (A1 is this stream's only other editor of that file).
      Detection latency is one tick of the WORKER-claim renewal ticker,
      `CAESIUM_WORKER_LEASE_TTL/4` — 7.5 s on the distributed lane, 75 s on the
      5 m default (**not** `CAESIUM_RUN_LEASE_TTL`, which governs a different
      ticker; the A5 comment said so and is corrected). `cancelLostClaimsNow`
      also skips an empty `claimedBy` group rather than querying "claimed by
      nobody", which matches exactly the rows a cancel has already released.
      **Known gap, for N-3 rather than this PR:** `run_cancelled` is delivered
      on the non-blocking in-process bus (`internal/event/bus.go` drops on a
      full subscriber buffer), so a dropped event orphans a LOCAL container
      permanently — there is no reconciliation loop. The distributed half is
      already self-healing (the liveness check re-asks the catalog every tick);
      the local half needs a periodic reconcile of registered runs against run
      status to match it.
- [x] A5. Integration scenario: replace-cancel stops the orphaned container.
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
      **Done (W1-β).** New `test/run_cancel_container_test.go`;
      `TestReplaceCancel` added to the distributed lane's `-run` regex.
      `test/run_concurrency_test.go` is untouched. Because both runs of a
      `replace` job share one command, the marker alone cannot tell the two
      containers apart — the scenario snapshots the FIRST run's container ids
      (matched by `Config.Cmd`) before triggering the replacement, asserts on
      exactly those, and force-removes every marker-carrying container on the
      way out so the replacement's own `sleep 120` never leaks onto the shared
      daemon. Skipped off the docker engine. The 90 s deadline is the
      distributed lane's: claim loss is detected on the lease-renewal cadence
      (`RUN_LEASE_TTL=30s` → renew every 7.5 s, acted on once a claim is within
      half the TTL of expiry), not immediately. Red-before verified against the
      pre-A3 tree: "the cancelled run's container(s) […] are still running"
      after the full 90 s.
      The scenario also RETIRES the run the `replace` admission started
      (`retireReplacementRun`): it runs the same `sleep 120`, the distributed
      lane has one worker slot, and walking away left the lane with no capacity
      for two minutes — which is how this scenario's first distributed run took
      `TestRetryAfterApplyExecutesRegisteredCommand` down with it as a 120 s
      "timeout waiting for run to complete" that looks nothing like its cause.
      The replacement's container is force-removed and its run is required to
      reach a terminal status before the scenario returns.

### Stream B — Data-plane truth

Three shipped surfaces report something other than what happened.

- [x] B1. Write and read one `caesium_dataset` facet shape. In
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
      *Done (W1-γ).* `persistTaskDatasets` now writes
      `{"caesium_dataset":{"step_name":…}}`; `stepNameFromFacet` falls back to
      the legacy flat key so pre-fix rows still resolve. `TestStepNameFromFacet`
      is a round-trip (`mapEvent` → `persistTaskDatasets` → `QueryImpact`) and
      **additionally pins the writer's blob shape directly** — necessary because
      the tolerant reader makes a flat write indistinguishable through
      `QueryImpact` alone; verified red-before by restoring the flat marshal
      (`producing_step must write the nested caesium_dataset facet`).
      `TestLineageImpactReturnsDownstream` now asserts
      `producing_step == "transform"`.
- [x] B2. Give `models.Task` JSON tags (`id`, `job_id`, `atom_id`,
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
      *Done (W1-γ).* `next_id` verified: **no** `models.Task` field carries it,
      so the shim's `NextID` arm was dead — the `JobTask.next_id?` field stays
      (job-detail-manifest's `fallbackNext` reads it) but nothing normalises it.
      `node_selector` is `omitempty` on the model, so with the normaliser gone
      it became optional on `JobTask` (every consumer already guarded it).
      **Ledger L6's "no other consumer" is wrong**: two integration helpers
      decode the exact tag `json:"AtomID"`, which does *not* case-insensitively
      match `atom_id`, so they silently returned empty — `fetchTasks`
      (`test/integration_test.go`) and `jobTaskCommand`
      (`test/retry_frozen_recipe_test.go`) were retagged in the same PR (out of
      the item's stated file list, by necessity). Helpers whose tags lack an
      underscore (`json:"ID"`, `json:"Name"`) still match case-insensitively and
      were left alone.
- [x] B3. Source the freshness consumed-dataset snapshot from the run's start,
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
      `internal/freshness/startparams.go`, `internal/freshness/startparams_test.go`,
      `internal/freshness/subscriber_test.go`, `internal/run/store.go`,
      `internal/run/start_params_enricher_test.go`, `cmd/start/start.go`,
      `test/freshness_test.go`.
      *Done (W1-γ) — **decision: FIXED, the change is bounded**; no persisted
      table and therefore no arc AC-1 amendment (the AC 2 carve-out is not
      exercised).* The consumed view is captured **synchronously at run
      creation** and persisted on the run row. `internal/run` gained one seam —
      `run.SetStartParamsEnricher`, a process-wide
      `func(ctx, db, jobID, params) (params, error)` that `startRun` applies just
      before `newStartRunModel`, so the enriched params are marshalled into
      `job_runs.params` by the same INSERT (and into the `run_queue` row on the
      queued path). It is registered process-wide, not per-`*Store`, because run
      stores are constructed ad hoc (`runstorage.NewStore(tx)` in
      `internal/trigger/event/router.go`), and a per-instance hook would silently
      skip event-triggered runs. The `db` argument is load-bearing for the same
      reason: that router creates runs inside its OWN open transaction, and the
      first cut of this fix read a captured connection instead — which deadlocked
      the whole database for 7m55s on the integration lane
      (`TestEventIngestRoutesEventTriggerJob`) until the request context was
      cancelled. Every enricher read now goes through the store's handle;
      `TestStartRunEnricherReadsTheStoresOwnHandle` pins it by reading a row that
      exists only inside the uncommitted transaction. Nil by default and
      non-fatal on error: with no enricher, or with a failing one, run creation
      is byte-identical to before.
      `internal/freshness.EnrichStartParams` implements it — one indexed
      `dataset_declarations` read, then `consumedSnapshot` for a job that both
      produces and consumes, stamping `_consumed_watermarks_start`
      (`ConsumedWatermarksStartParam`) in the evaluator's exact JSON format. It is
      a plain func, so `internal/freshness` still does not import `internal/run`;
      `cmd/start/start.go` wires it in one line inside the existing
      `vars.FreshnessEnabled` block. `Capturer.consumedForRun` reads that param
      first, then the evaluator's `_consumed_watermarks`, and falls back to the
      completion-time read only for runs created before this change or with
      freshness disabled at creation.
      **The async `run_started` design was rejected**, not merely improved on:
      the event only queues work for the subscriber, so the read could land after
      the run was already executing (an input advancing in between was credited
      to a run that never read it), and the non-blocking bus can drop the event
      entirely; its in-memory map also needed a TTL sweep that would expire a
      long-running run's snapshot (runs have no default timeout) and a 4096-entry
      cap that evicted live runs under load. The map, the 24 h TTL and the cap
      are deleted, and the `Capturer` is back to subscribing only
      `run_completed`. Unit tests: `internal/run` proves the enriched params are
      on the persisted row, that a failing enricher still starts the run, and
      that an unregistered enricher changes nothing; `internal/freshness` proves
      the creation-time value beats a mid-run advance end to end, the derived
      param is never overwritten, an empty view is stamped authoritatively, the
      caller's map is not mutated, and jobs with nothing to freeze are untouched.
      Integration `TestFreshnessConsumedSnapshotTakenAtRunStart` uses an
      **arrival-bound** external source as the input, so each mid-run advance is
      one ingest POST rather than a whole producer run — the consumer only has to
      stay alive for an HTTP round trip (30 s sleep, ~40 s total) instead of a
      container start — and asserts the output's `consumed_watermarks` carries
      the START-time watermark, with a guard that fails loudly if the consumer
      terminated before the mid-run advance landed; it is now deterministic (no
      sleep waiting for an observer) because the view is frozen with the row.
      Three review refinements followed. (a) A **failed** watermark read is no
      longer written down as an empty view: `consumedSnapshot` returns
      `(map, error)` so "every input is genuinely without a watermark" (stamp
      `{}`, authoritative) is distinguishable from "the read failed" (omit the
      param, so completion falls back to its own read). The failure stays
      non-fatal to run creation — freshness is optional and a run must still
      start — so it travels the warn-and-degrade path in `enrichedStartParams`.
      (b) The seam carries `fromQueue`, and a **queue-strategy run is re-enriched
      on promotion**: it is admitted twice (enqueue, then `StartQueuedRun` when
      the dequeuer frees a slot), and only the second call happens when the run
      actually begins, so keeping the admission-time view would credit the output
      to inputs the run never read and let freshness derive redundant catch-up
      work. (c) That refresh is why the capture now uses **two keys, not one**.
      They look alike — same JSON shape, same dataset keys — but they answer
      different questions and have different owners.
      `_consumed_watermarks` is the DERIVATION-time view: the evaluator stamps it
      in `derive` and matches on it in `hasActiveOrQueuedRun`, whose
      `sameDerivationParams` compares exactly `_derived_from_dataset` +
      `_consumed_watermarks` against every running `job_runs` row and every
      `run_queue` row for the job. The enricher never reads or writes it.
      `_consumed_watermarks_start` is the START-time view, written only by the
      enricher, unconditionally overwritten on every creation of the run, and
      **deleted** (never left stale) when that read fails, so completion degrades
      to its own read instead of believing a snapshot from a different run. The
      retraction is not gated on `fromQueue`, because promotion is not the only
      way a run inherits someone else's view: a retry re-runs with the params of
      the run it is retrying (`cmd/run/retry.go`,
      `api/rest/controller/job/run/retry.go`). With the key no longer shared,
      `fromQueue` stops being load-bearing here at all — it was what told
      admission's "keep what is there" from promotion's "take it again", and
      there is now nothing to keep. Collapsing them
      onto one key broke both ends: the promotion refresh overwrote the
      evaluator's decision view, and since the dequeuer deletes the `run_queue`
      row once `StartQueuedRun` succeeds, the running row is all the dedupe has
      left to match — it no longer matched, so the next tick derived a duplicate
      run for work already in flight. Retracting a value the enricher can no
      longer stand behind needs the seam's help: `enrichedStartParams` keeps the
      map an enricher returns *alongside* its error (nil still means "use the
      caller's params"), because on a promotion the caller's params are the
      run_queue row's and carry the very value the failed re-read invalidated.
      Covered by `TestStartQueuedRunRefreshesEnrichedParams` and
      `TestStartQueuedRunAppliesEnricherRetraction` (`internal/run`, real
      enqueue→dequeue→promote path) and, in `internal/freshness`,
      `TestStartParamsEnricherRefreshesOnQueuePromotion`,
      `TestStartParamsEnricherNeverTouchesTheDerivationView`,
      `TestStartParamsEnricherDropsTheStaleViewWhenThePromotionReadFails`,
      `TestStartParamsEnricherDropsAnInheritedViewWhenTheReadFails`,
      `TestCapturerPrefersTheStartViewOverTheDerivationView` and
      `TestQueuePromotionKeepsTheDerivationDedupe`, which drives the real
      `hasActiveOrQueuedRun` against a promoted run's persisted params rather
      than comparing strings; no integration variant, a queued-concurrency
      scenario would add a dequeuer poll and a second slow run for a rule the
      unit tests pin exactly.

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

- [x] C1. Allow `GET /auth/whoami` for any authenticated **API-key** principal.
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
      *Done (W1-ζ).* The `case "/auth/whoami"` is first in `authorizeScope`'s
      switch, ahead of the `/v1/jobs/:id` prefix branch; the agent-session arm
      returns before the switch, so an agent token is still `AgentScopeDenyMessage`
      -denied on whoami (pinned by `TestMiddlewareWhoamiDeniesAgentSessionToken`).
      **Extra beyond the item text:** "the response names the key's scope"
      required the whoami body to carry one — it serialised only
      kind/subject/role — so `api/rest/controller/auth/sso.go` now emits
      `scope: {jobs: […]}` for a scoped API key and omits the field for an
      unscoped one. No UI change was needed: `scopeFromWhoami`
      (`ui/src/lib/auth.ts`) already read `body.scope.jobs`; only its stale
      comment and the matching one in `LineageGraph.tsx` were corrected.
      - *`ApprovalAgentTokenDenyMessage` reachability (raised by W1-ε).* The
        finding is correct: `api/middleware/auth.go` checks RBAC before
        `authorizeScope`, and `auth.MintAgentSessionKey` issues at
        `AgentSessionKeyRole == RoleRunner` while the approval routes require
        `RoleOperator`, so a genuine agent token is denied
        `insufficient permissions` by the role gate and the specific message is
        never seen on the wire. **Decision: keep the arm as defence in depth.**
        It is the deny that still holds if an agent claim ever rides an
        operator-or-above key (a future minting path, a stamped scope, or any
        widening of the approval routes' required role) — deleting it would
        make "tier 3 always terminates at a human" depend on a single role
        constant. What was hollow was the *documentation*, not the code, so
        `auth_scope.go` now records the layering explicitly and
        `auth_scope_approval_test.go` gained
        `TestMiddlewareApprovalRouteDeniesMintedAgentKeyAtRBAC`, which mints a
        **real** agent key, asserts the handler is never reached, asserts the
        message is the RBAC one, and asserts
        `RoleLevel(AgentSessionKeyRole) < RoleLevel(RoleOperator)` so the
        premise fails loudly if the roles ever move. No scope was widened and
        no role was lowered to make the other arm reachable.
- [x] C2. Live scenarios for the key-management surface. `TestAuthKeyLifecycleCLI`
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
      *Done (W1-ζ).* All four scenarios live in `test/auth_keys_test.go`
      (the three named here plus `TestAuthJobApplyCLI`, see C6).
      **The item's own gate found a real bug on the first lane run:** every
      `caesium auth` subcommand wrote its output with cobra's `cmd.Print*`,
      and cobra's `Print`/`Printf`/`Println` write to `OutOrStderr` — so the
      **one-time plaintext API key** went to stderr. `caesium auth key create
      … > key.txt` produced an empty file and an unrecoverable key, and
      `auth key list` / `auth audit` were unpipeable. This is the exact failure
      mode `CLAUDE.md` warns about, caught only because the scenarios capture
      stdout separately (`runCLIStdout`/`runCLISeparate`).
      Review (#391) then caught the **second half** of the same rule: moving the
      bytes to stdout is not enough if they are prose *plus* JSON —
      `… | jq -r .key` still fails. So stdout is now **exactly one JSON value
      and nothing else** for every one of
      `cmd/auth/{key_create,key_rotate,key_list,key_revoke,audit}.go`, emitted
      through the shared `cliutil.WritePrettyJSON`, with the "save this, it will
      not be shown again" line on stderr — the split `cmd/receipt/get.go`
      already documents ("stdout must stay the receipt bytes and nothing
      else"). The plaintext is the response's `key` field, so nothing is lost.
      The test helper was rewritten to unmarshal the whole stream with **no
      stripping**: the earlier version scanned for a `csk_` token and sliced
      from the first `{`, which would have stayed green if the prose ever came
      back.
      `--api-key` is exercised on `auth key list` and its "visible in process
      listings" warning is asserted to land on **stderr** while stdout stays
      parseable JSON (`runCLISeparate`); the other steps use the runner's
      `CAESIUM_API_KEY`. The lifecycle also pins that the rotated-**out** key
      survives its `--grace-period 1m` window, and `TestAuthKeysREST` adds a
      404 on revoking an unknown key id. The shared helpers
      (`createAPIKeyCLI`, `parseCreatedKey`, `requestWithKey`) live here and
      are reused by C1/C5.
- [x] C3. Live approve/reject through a **real** proposal. `TestIncidentApprovalDecisionsCLI`:
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
      Note: W1-ε shipped `TestIncidentApprovalDecisionsCLI` on the auth lane. It
      applies a single-step always-failing job, waits for the real incident,
      mints an agent-session token the way `test/agent_mcp_test.go` does, posts
      `skip_task` through `POST /v1/agent/incidents/:id/actions` (asserting the
      202 disposition is `awaiting_approval`, the incident parked, and a pending
      `ApprovalRequest` exists), proves the agent token gets
      `ApprovalAgentTokenDenyMessage` on the approve route, then drives
      `caesium incident approve --json` through `runCLIStdout` and asserts the
      action reached `executed` AND the task row is `skipped` on a terminal run.
      A second incident is rejected and asserted `rejected` + `escalated`. The
      stale H-1 comment in `test/incident_gating_test.go` now points here.
      Deviations: the scenario applies the job over the **authenticated REST**
      surface, not `caesium job apply` — that command sends no `Authorization`
      header at all (`cmd/job/apply.go` `sendApplyRequest`), so it cannot reach
      an auth-enabled server; worth filing via N-3. Second deviation, also for
      N-3: `ApprovalAgentTokenDenyMessage` is **unreachable on the wire**. The
      auth middleware checks the RBAC role before `authorizeScope`
      (`api/middleware/auth.go`), and an agent-session key is minted `RoleRunner`
      while the approve route requires `RoleOperator`, so the role gate answers
      first with `insufficient permissions`;
      `api/middleware/auth_scope_approval_test.go` pins the specific arm by
      calling `authorizeScope` directly and therefore never proved the wire
      behaviour. The scenario asserts the 403 plus either documented denial AND
      that the incident stayed `awaiting_approval` with the approval `pending` —
      the property that actually matters. Fixing the ordering belongs to
      `api/middleware/auth_scope.go`'s owner (C1). The single-step job keeps the
      "task is skipped" assertion unambiguous, and the incident is matched by
      task name so a run-level twin cannot be latched onto. The `AUTH_MODE=none`
      `apply_jobdef_patch` refusal is unit-tested
      (`TestApplyJobdefPatchRefusedWithoutAuthMode`); its default-lane assertion
      remains C6's row.
- [x] C4. Wire the tier-3 approval flow the completed plan says exists. Call
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
      Note: W1-ε wired `agentsvc.SetActionExecutor` in `cmd/start/start.go` to a
      new `cmd/start/agent_action_executor.go` adapter (it also resolves the
      effective playbook and the proposing agent session, neither of which the
      HTTP surface can know). `Execute`'s `decisionApprove` branch now calls
      `requestApproval` in new `internal/incident/approval.go`: it creates the
      `ApprovalRequest`, walks the incident `open → triaging →
      awaiting_approval` through the store's transition table (there is no
      `open → awaiting_approval` edge), publishes a **persisted**
      `TypeApprovalRequested`, and ends the proposing session via a new
      `Supervisor.EndSession`. Two deviations from the item text, both recorded
      in code comments: the executor gained a `SetEventSink(bus, *event.Store)`
      seam (nothing in `internal/incident` could publish before — `TypeIncidentOpened`
      still has no publisher either), and the effective playbook can only be
      resolved from the `AgentProfile` document, because `metadata.remediation`
      is schema-validated but never persisted onto `models.Job`; the fallback is
      the zero `Playbook`, under which tier 3 always routes to approval.
      Unit tests: `internal/incident/approval_test.go`,
      `api/rest/service/agent/agent_test.go`
      (`TestProposeActionDelegatesToRegisteredExecutor` strengthened to assert
      the fallback row is NOT also written).
      Review follow-up (PR #390): the playbook is now JOB-SCOPED, closing the
      widening the deviation above left open. `metadata.remediation` IS persisted
      (`models.Job.Remediation`, mapped on both importer branches), and
      `agent_action_executor.go` resolves incident → job → the job's declared
      profile, narrowed by the job's own `autonomy` block via a new
      `Playbook.Narrow` (allow intersects, requireApproval unions, paramOverrides
      intersect keys and values — an emptied intersection denies rather than
      reopening, since an empty `Allow` means "unconstrained"). A job with no
      block still inherits `CAESIUM_AGENT_DEFAULT_PROFILE`; a job whose DECLARED
      profile cannot be resolved gets `incident.DenyAllPlaybook()`, never the
      default's allowlist.
      Review round 2 (PR #390): persisting the block made it a privilege-escalation
      surface, so `dispatchApplyJobdefPatch` now refuses — before the provenance
      router, so BOTH routes are covered — any patch whose `metadata.remediation`
      differs from the live `job.Remediation` (`ErrPatchAltersRemediation`, action
      recorded `failed`). An agent may not edit the policy that governs it. With
      that boundary in place the job block is treated as an AUTHORED policy that
      may grant as well as narrow, matching the design: `Playbook.Narrow` became
      `Playbook.Override` (job allow REPLACES, requireApproval unions). The
      nil-vs-empty distinction is now explicit and single-sourced through
      `allowsAutonomous(actionType, tier)` — nil means unconfigured (tier
      defaults), non-nil governs at every tier — which also fixes the shipped
      `triage-only` profile, whose `allow: []` decoded as unconstrained and
      therefore granted every tier-1 action under a "zero risk" profile (it now
      declares `allow: [escalate]`). `internal/jobdef/diff.JobSpec` gained
      `Remediation` so `caesium job diff` and the rendered approval diff show a
      remediation-only change instead of "no changes".
      Review round 2 (PR #390): `requestApproval` creates the `ApprovalRequest`
      and parks the incident in ONE transaction. Parking used to be best-effort,
      so a proposal against a terminal or concurrently-advanced incident could
      commit an approval no feed lists and no human can decide. It now fails
      whole with `ErrIncidentNotApprovable` and creates nothing. Relatedly
      `Supervisor.EndSession` now STOPS the session container through its
      `atom.Engine` (it only revoked the credential before, leaving the agent
      burning tokens against a revoked key), and every session state write is
      guarded on pending/running so a later finalization cannot rewrite an
      already-ended session as `timed_out`.
- [x] C5. Scoped-key allow/deny matrix as one table-driven live scenario
      (`TestScopedKeyAllowDenyMatrix`): for a key scoped to job `A`, assert
      200 on `GET /v1/jobs` (filtered to `A`), `GET /v1/jobs/:idA`,
      `POST /v1/jobs/:idA/run`, `GET /v1/events?run_id=<A run>`; 403 on
      `GET /v1/jobs/:idB`, `POST /v1/jobs/:idB/run`, `GET /v1/lineage/impact`
      (`LineageImpactScopedDenyMessage`), `GET /v1/contracts/graph`,
      `GET /v1/events` without `run_id`, `POST /v1/jobdefs/apply` with
      `prune`; 404 for a run id that does not exist. Each row cites the
      `authorizeScope` case it pins. Files: `test/auth_scoped_test.go` (shared
      with C1). Depends on: C1 + H-1.
      *Done (W1-ζ).* Every listed row is present, plus three the item did not
      name: a positive `POST /v1/jobdefs/apply` (no prune, in-scope alias), a
      deny for an apply naming an out-of-scope alias, and the trailing-deny row
      (`GET /v1/stats`). Two deviations forced by the code, not choices:
      (a) the key is minted at **operator**, because `POST /v1/jobs/:id/run`
      (runner) and `POST /v1/jobdefs/apply` (operator) would otherwise 403 on
      the RBAC role gate and pin the wrong thing; (b) the fixture carries a
      second in-scope alias that is never run, because an apply is refused 409
      while the job has a running run (`ensureJobNotRunningTx`) and the matrix
      deliberately starts runs. The `/v1/events` rows are outside the table:
      a 200 there is an open SSE stream whose body must not be drained.
      The scoped jobs are applied over `POST /v1/jobdefs/apply` rather than
      `caesium job apply` because the matrix pins `parseApplyAliasesForScope`'s
      branches (prune on/off, an out-of-scope alias in the body), which are
      properties of the request, not of the CLI. (`caesium job apply` could not
      authenticate at all when this was written; that is fixed under C6 and
      covered by `TestAuthJobApplyCLI`.)
- [x] C6. Close the no-test gaps recon found, each through its real surface:
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
      *Done (W1-ζ).* All five scenarios are green on `just integration-test`.
      Notes per row: `TestJobUnpauseRoute` proves unpause is load-bearing by
      asserting the paused job answers **409** on a manual run first (PUT, not
      POST, per Ledger L11). `TestNodeWorkersRoute` asserts the 200 shape —
      the handler answers for *any* address (`api/rest/service/worker`
      `Status`), so there is no 404 branch to document. **One production edit
      was unavoidable:** `caesium run retry-callbacks` had no `--server` flag
      and reached the database through `db.Connection()`, which opens a
      **native** dqlite node bound to `CAESIUM_NODE_ADDRESS` — impossible
      beside a server already holding that address, so the command was
      undrivable from any test runner (and from any operator not on the server
      host). `cmd/run/retry_callbacks.go` now takes `--server`/`--api-key` and
      posts to the shipped `POST /v1/jobs/:id/runs/:run_id/callbacks/retry`,
      using the exact `cmd.Flags().Changed("server")` convention of
      `caesium run retry`; the in-process path is unchanged apart from moving
      its success line to stdout so both transports agree. Unit tests in
      `cmd/run/retry_callbacks_test.go`.
      - *`caesium backfill` wrote to the wrong stream too.* `backfill create`,
        `list` and `cancel` all used cobra's `cmd.Print*` (→ `OutOrStderr`), so
        `backfill create | jq .id` read nothing. All three now emit **exactly
        one JSON value on stdout** via `cliutil.WritePrettyJSON`, with
        "Backfill started:" / "Backfill … cancelled" on stderr — the first fix
        only moved the stream and left `create` prefixing its JSON with
        "Backfill started:", which review on #391 correctly called a P1
        because the documented `| jq .id` workflow still got invalid JSON.
        `TestBackfillCLILifecycle` now parses stdout with no stripping and
        asserts the framing is on stderr and absent from stdout.
        Note for anyone extending that scenario: `total_runs` is **0** on the
        create response — `RunBackfill` enumerates the window on its own
        goroutine and calls `SetTotalRuns` afterwards
        (`internal/job/backfill.go`) — so the window round-trip is asserted
        synchronously on the echoed `start`/`end` and the fire-time count is
        polled. Asserting `total_runs` on the create response is a race, and it
        is the one this scenario tripped over on its first default-lane run.
      - *`caesium job apply` sent no `Authorization` header at all* (raised by
        W1-ε; confirmed in `cmd/job/apply.go` `sendApplyRequest`). The deploy
        verb the README and `CLAUDE.md` both name could therefore not reach a
        server with `CAESIUM_AUTH_MODE=api-key` — the auth surface cannot be
        true end to end while its primary write command 401s. Fixed with the
        **existing** shared helper (`cliutil.ResolveAPIKey` + a `--api-key`
        flag, exactly as `cmd/job/lint.go` already did), not a second
        convention, and covered on the auth lane by `TestAuthJobApplyCLI`:
        refused with the key cleared (and no job created), accepted via
        `CAESIUM_API_KEY`, accepted via `--api-key` with the "visible in
        process listings" warning on stderr. Checked the neighbours as asked:
        `caesium job lint --server` already resolved a key through the same
        helper (no change); `caesium job diff` has **no** `--server` mode at
        all — it reads `db.Connection()` locally — so it has no header to
        send and is out of scope here (worth a follow-up via N-3 if a
        server-side diff is wanted).
- [x] C7. Execute approved tier-3 actions (the far end of Ledger L9). (a) In
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
      Note: W1-ε added `Executor.ExecuteApproved(ctx, actionID)`
      (`internal/incident/approval.go`), invoked by `Service.Approve` AFTER the
      decision transaction commits, injected via
      `incidentsvc.SetApprovedActionExecutor` in `cmd/start/start.go`; unset it
      degrades to a logged "approved, not executed" and `DecideResult` gained
      `Executed`/`ExecutionError`. The decider is read from the `ApprovalRequest`
      (never the caller) and mirrored into the audit log as `human:<decider>` via
      a new `finishAs`/`mirrorAudit` actor override. All three `dispatch` cases
      and their `ActionOps` methods landed, implemented on `incidentActionOps`
      (`cmd/start/incident_ops.go`, now constructed from the `*gorm.DB`).
      Deviations, all commented in code: (1) `run.Store.SkipTask` is
      **pending-only** (`markTaskSkippedTx` filters `status = pending`), so it
      cannot skip the FAILED task the design names — the adapter calls the
      shipped store op for pending rows and flips terminal-`failed` rows itself,
      deliberately without re-advancing successors or emitting a second
      `task_skipped` (the failure path already resolved them). It errors when it
      finds neither, so an approved action that changed nothing is recorded
      `failed`. (2) `override_schema_gate` needed a column:
      `models.JobRun.SchemaGateOverride`, read by a new
      `run.SchemaGateOverridden` from BOTH `ValidateTaskOutputSchema`
      (`internal/run/schema_validation.go`) and
      `ValidateTaskOutputSchemaInstance` (`internal/run/store_instance.go`) — one
      read point, so the bypass cannot cover the unfanned path and miss the
      fanned one. A run param was rejected: params feed cache identity. (3)
      `apply_jobdef_patch` takes a whole `pkg/jobdef.Definition` and applies it
      through `internaljobdef.Importer.ValidateBatch` + `ApplyWithOptions` (the
      same in-process entry point `POST /v1/jobdefs/apply` uses), with the diff
      rendered by `jobdef/diff.Compare` scoped to the target alias; the alias
      must match the target job. The provenance route is derived in the executor
      (`gitSynced` over the four `Provenance*` fields) and the `AUTH_MODE=none`
      refusal (`ErrAuthModeNone`) mirrors `pkg/env`'s master-gate condition
      exactly, including the SSO clause.
      Review follow-up (PR #390): `Escalate` no longer only logs. It publishes a
      PERSISTED `event.TypeIncidentEscalated` carrying the incident, the
      requested channel, the rendered summary/diff and `job_alias`, and returns
      an error when it cannot — so `dispatch` records the action `executed` only
      once the escalation is actually on the stream, never for a page nobody
      received. `incidentActionOps` therefore takes the bus + event store
      (`newIncidentActionOps(conn, bus, eventStore)`).
      Review round 2 (PR #390): the claim is now pinned deterministically
      (`TestClaimApprovedIsOnceOnly` — deleting it fails to compile) plus a real
      two-goroutine race, and the sweeper has integration coverage
      (`TestIncidentApprovedActionRedriveRecoversAfterCrash` strands a real approved
      action in the catalog and asserts the LIVE sweeper redrives it).
      `ActionOps.Escalate` now returns `routed` — publishing is not delivery, and
      the subscriber silently drops an event no `NotificationPolicy` matches, so
      an unrouted escalation records `routed:false` with a warn log instead of
      reading as a completed page. The triage bundle and the executor now share
      one resolver (`incident.ResolvePlaybook`), so the policy the agent is
      briefed with is the policy enforced on its proposals.
      Review follow-up (PR #390): `ExecuteApproved` now CLAIMS the action with a
      conditional `approved → executing` UPDATE
      (`models.AgentActionStatusExecuting`), so dispatch is once-only across the
      synchronous post-decision path and the new leader-gated
      `incident.ApprovalRedriver` (`internal/incident/redrive.go`, wired in
      `cmd/start/start.go` under the master gate, tuned by
      `CAESIUM_AGENT_APPROVAL_REDRIVE_INTERVAL` / `_GRACE`). The redrive closes
      the crash window the decide/execute split opens: an action stranded
      `approved` by a process death is re-dispatched, since re-approving is
      refused by the pending-only guard. A row left `executing` is deliberately
      NOT auto-redriven — re-running a half-applied tier-3 mutation unattended is
      worse than a visible stuck row.
- [x] C8. Make the approved-action decision explainable (arc convention 3 —
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
      Note: W1-ε added `event.TypeAgentActionExecuted` (verified distinct from
      `TypeAgentActionRecorded`, which the approvals controller emits at DECISION
      time and which says nothing about execution), published from
      `ExecuteApproved`'s finish path with decider/type/tier/outcome, PERSISTED
      via `event.Store.AppendTx` before publish, and added to
      `notification.notifiableTypes` (so it is also policy-routable and appears
      in `ValidEventTypes`). `caesium why` gained `WhyExplanation.Remediation`
      (`internal/run/why_remediation.go`), a joined read of executed
      `AgentAction` rows whose `ApprovalRequest` is approved, attributed from the
      action's RESULT payload (run id, and task id for a task-scoped action) —
      never over-attributed. It renders in the summary's first line and as an
      `Approved remediation` block in `cmd/why/why.go` `renderTable`, printed
      before the group/diff early returns because a `skipped` task carries no
      diff. The API JSON needed no controller change (the `why` controller
      returns the struct verbatim). Deviation: `apply_jobdef_patch` is
      deliberately NOT surfaced in `why` — it is job-scoped and mutates future
      runs, so attributing it to a recorded task run would assert causality that
      may not hold. Integration scenario `TestIncidentApprovalWhyExplains` on the
      auth lane asserts both the table and `--json` forms through
      `runCLIStdout`; unit coverage in `internal/run/why_remediation_test.go`.

### Stream D — CI gates merges and master is honestly green

Arc convention 8 assumes required status checks exist. They do not (Ledger
L13), and 6 of the last 17 master runs are red for reasons the ledger already
names (L12).

- [x] D1. Fix or quarantine the root causes in L12. (a) `TestFanOutHTTPRetryPartition`
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
      **Done (W2-α):** all five entries fixed, none quarantined — no new
      `t.Skip`-behind-an-issue anywhere.
      *(a)* #384 did **not** fix it: it recurred on master run 34062579647
      (commit `192f604`, job 101565958611). Root cause is not
      `partitionRetryOutstandingTx` at all — it is how the store discards the
      owner's cached state. `Store.invalidateRunState` called
      `OwnerManager.Drop`, and Drop's contract is to force a final checkpoint of
      the state it is discarding: on this path, precisely the "run is complete"
      snapshot the retry just invalidated. That snapshot is durable until the
      `DeleteCheckpoints` that follows, and the dropped pointer is not marked
      stale, so an in-flight completion can write it again afterwards. Either
      copy makes the next recovery reconstruct a complete run, and a recovery
      never learns that a row *stopped* being terminal. Fixed by invalidating
      through `OwnerManager.Release` (which marks the state stale, deletes the
      checkpoints while still holding the run, then forgets it — the ordering
      its own doc comment already called load-bearing), and by adding a per-run
      invalidation epoch so a `Recover`/`Adopt` that was already rebuilding when
      the retry landed cannot publish its pre-retry view over the top
      (`put` refuses it, the rebuild retries off the current rows, and the
      generation checkpoint is written only after a successful publish). Three
      deterministic regression tests in `internal/run/owner_invalidation_test.go`;
      the first one fails against the old `Drop` path, verified. Adversarial
      review then found three more windows on the same seam, all fixed with
      their own regression tests: `Recover`'s post-publish checkpoint used a raw
      `writer.Force` that bypassed the `stale` flag; `Drop` forgot the run
      *before* forcing its final checkpoint, so a retry in that gap had nothing
      to mark stale and its delete preceded the write; and the per-run epoch
      counter was ABA-prone (reset to zero on publish), now a manager-wide
      monotonic stamp that `Drop` deliberately does not advance — `Drop` is this
      owner letting go of a run it just checkpointed, not a claim that the rows
      moved, so stamping there would only force spurious rebuilds and leak an
      entry per run ever owned.
      *(b)* Podman was green on all 8 recent master runs, so nothing to
      reproduce. Diffing the lane's own `docker run -e` block against
      `integration-up` found two envs missing — `CAESIUM_DATABASE_SHARDS=4` and
      `CAESIUM_FANOUT_MAX_PARTITIONS=8` — both added, with a comment on the step
      saying the block must stay a superset of `integration-up`.
      *(c)* Real fix, no quarantine, and it was not a test bug.
      `go-dqlite`'s package `init()` puts SQLite into **single-thread** mode
      process-wide (`go-dqlite/v3/config.go`), which disables the mutexes that
      make `sqlite3_initialize()` safe to call concurrently — and every binary
      here also drives SQLite directly through `mattn/go-sqlite3`. Two
      `t.Parallel()` tests opening their first connection in a fresh process
      race that initialization and the loser sees an unpopulated VFS list, which
      SQLite reports as `no such vfs: ` with an EMPTY name. Reproduced locally
      at ~1 in 40 fresh `-race` processes of `./internal/trigger/event/`
      (`-count=N` inside one process never shows it: only the first open is at
      risk). Fixed in `pkg/dqlite/threading.go` with the remedy go-dqlite
      documents for exactly this case — `ConfigMultiThread()` in an `init()`
      that runs after go-dqlite's and before anything opens a database — so it
      is fixed for every package, not just this one. `pkg/dqlite/threading_test.go`
      asserts the switch actually took (`sqlite3_config` returns SQLITE_MISUSE if
      something initialized SQLite first).
      *(d)* Now **13** call sites across 11 files (`incident_approval_test.go`
      added two since this item was written). The skip is a single
      `requireDirectCatalogAccess()` inside `openIntegrationCatalogDB`, keyed on
      the existing `CAESIUM_TEST_ENGINE` discriminator — no new env var. It
      fires only on `kubernetes` (server in-cluster, dqlite binds POD_IP, only
      :8080 is port-forwarded); podman shares the server's netns
      (`--network=container:caesium-server-podman`) and the docker lanes use
      `--network=host`, so default/distributed/owner-memory/infra/agent are
      untouched — the agent lane's `TestAgentMCPToolsListBundleAndIncidentScope`
      still reaches the catalog through this helper and still counts toward
      `agent_integration_min_pass`. It deliberately does not skip on mere
      unreachability: a dqlite that stops listening on a docker lane must stay a
      loud failure.
      *(e)* `TestFanOutLocalCancelledMidFlightStillResolvesPendingSiblings`
      (master run 34136265000, job 101788657387) — **test-timing assumption, not
      a limiter race.** `ratelimit.Limiter` is a FIXED-window limiter bucketing
      on `now.Truncate(window)` floored at a minute, so "2 per minute admits
      exactly two of these four partitions" only holds while the whole dispatch
      pass stays inside one bucket. The failing log shows three instances
      starting at 15:12:00.020/.022/.062 with only partition `d` parked: the
      pass straddled `:00`, the bucket rolled, and the fresh bucket admitted two
      more. Fixed by pinning the limiter's clock from `runFanOutInBackground`
      through a new unexported `job.rateLimitClock` seam (alongside the existing
      `beforeComplete` seam, and carried into a replacement engine the same
      way), which makes all four rate-limited fan-out tests boundary-independent
      rather than sleeping past the boundary.
      *(helm)* The lane's two W1 reds (`TestRunRetryCallbacksCLI`) were fixed by
      #393 — nothing left to do there.
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
- [x] D3. Repository hygiene: `git rm --cached ui/test-results/.last-run.json`
      and add `ui/test-results/` to `.gitignore`; add a `clean-worktrees`
      justfile recipe (appended at the end of the file) that runs
      `git worktree prune` and removes `.claude/worktrees/*` checkouts whose
      branch is merged into `master` (dry-run by default, `force=true` to
      delete), with a comment citing the 18-checkout / ~675k-LOC state that
      motivated it. Files: `.gitignore`, `justfile` (new recipe at EOF).
      *The README Codecov badge (`branch=develop` → `master`) is bullet (c)
      of N-1 — `README.md` has exactly one editor in W2.*
      **Done (W2-α):** `ui/test-results/.last-run.json` untracked and
      `ui/test-results/` ignored; `just clean-worktrees` appended at EOF with a
      `force := "false"` variable above it (`just force=true clean-worktrees`
      deletes). A checkout is removable when it is clean AND its work landed —
      reachable from master, tree-identical to master, or (the case that matters
      for this repo's squash-merge workflow, where a branch's commits never
      become master's ancestors) a merged pull request for its branch via `gh`.
      It resolves the MAIN worktree from any checkout, so it also works from
      inside an agent lane. Two review hardenings: a checkout sitting exactly on
      the master tip is never removable (a freshly-branched agent matches both
      "reachable from master" and "tree identical to master"), and the `gh` path
      requires the merged PR's `headRefOid` to equal the checkout's HEAD, so a
      branch carrying newer unmerged commits is kept. Dry run only on this host,
      with sibling lanes live: 3 removable, 9 kept — every live sibling
      correctly held back.

### Stream E — Release & install

The README's first sentence is "single self-contained binary"; there is no
binary to download (Ledger L14).

- [x] E1. Extend the `publish` job to create a GitHub Release with per-arch
      CLI binaries **that run on a bare Linux host**. The executable in the
      release image is **dynamically linked** — `CGO_ENABLED=1` in
      `build/Dockerfile.build`, and `build/Dockerfile`'s builder stage
      collects its shared-library closure with `ldd` (musl, `libdqlite`,
      `libuv`, `lz4`, `sqlite`) into the image — so extracting `/bin/caesium`
      alone (the first draft of this item) would publish a download that
      fails to load outside the image. (Revised 2026-09-06 after review.)
      (a) **Portable artifact, primary path — static link.** Add a
      `caesium-static` build target to `build/Dockerfile.build`/`build/Dockerfile`:
      in the `dqlite` stage also install the static archives
      (`sqlite-static`, `libuv-static`, `lz4-static`) and build dqlite with
      `--enable-static`; in a new `cli-static` stage link with
      `-tags libsqlite3 -ldflags '-linkmode external -extldflags "-static"'`
      (the form go-dqlite documents for static builds) and fail the stage
      unless `ldd` reports the binary is statically linked. Export it as a
      `release-cli-<arch>` artifact from the existing per-arch build jobs
      (additive step; no change to the image build).
      (b) **Bare-host smoke test, run natively where each artifact is
      built** — the gate, whichever artifact ships. The `publish` job runs on
      `ubuntu-24.04` (amd64) with no QEMU/binfmt setup, so it cannot execute
      the arm64 binary; instead each per-arch build job smoke-tests its own
      output on its own runner: `build-and-integration-test`
      (`ubuntu-24.04`) for amd64 and `build-and-integration-test-arm64`
      (`ubuntu-24.04-arm`) for arm64 — the two jobs that already build and
      save `release-amd64` / `release-arm64`. Each runs its binary inside a
      minimal container that carries **no** extra libraries (a plain glibc
      base, not the release image), asserting `caesium --help` exits 0 and
      `caesium job lint --path docs/examples/minimal.job.yaml` succeeds, and
      uploads `caesium-linux-<arch>` together with a `caesium-linux-<arch>.smoke-ok`
      marker (containing the binary's sha256 and the runner arch) as the
      `release-cli-<arch>` artifact. `publish` then **verifies, never
      executes**: both markers present, each sha256 matches its binary, then
      writes `SHA256SUMS` and attaches all three to a release for
      `${IMAGE_TAG}` via `gh release create --verify-tag --generate-notes`
      (or `softprops/action-gh-release`), plus the multi-arch image digests
      in the release body. (`docker/setup-qemu-action` in `publish` was
      considered and rejected: emulating a CGO/dqlite binary proves less than
      a native run on the arm64 runner CI already has.) Add `permissions:
      contents: write` on the **publish job only** (not the workflow
      top-level, to keep D1/H-2's edits to the test jobs conflict-free).
      (c) **Fallback, only if (a) cannot be made to link in the wave** (record
      the exact linker failure in the PR): publish
      `caesium-linux-<arch>.tar.gz` containing the executable plus the
      `ldd`-collected library closure the image build already produces and a
      `caesium` wrapper script that sets `LD_LIBRARY_PATH` to the bundle;
      label it as a bundled-libs artifact in the release notes and in N-1's
      install step, and file the static build via N-3. The (b) smoke test
      runs the wrapper, still natively per arch.
      **Linux only**: the builder compiles with `GOOS=linux` against CGO
      dqlite built in-stage, so a darwin binary is out of scope — say so in
      the release notes template and in N-1's install step (macOS users run
      the container via `just cli`/E2).
      Files: `build/Dockerfile.build`, `build/Dockerfile`,
      `.github/workflows/ci.yml` (`build-and-integration-test` and
      `build-and-integration-test-arm64`: static build export + native smoke
      test + `release-cli-<arch>` artifact; `publish`: marker/sha verification
      + release). The two build jobs are also edited by H-2 (timeouts) —
      sequence H-2 first, E1 rebases (both additive steps).
      **Done (W2-β):** path (a) — the **static** artifact shipped; the
      bundled-libs fallback (c) was not needed and no issue was filed.
      `build/Dockerfile` gained a `cli-static` stage (`FROM builder`, so it
      reuses the builder image CI already loads and only the link differs);
      `build/Dockerfile.build`'s dqlite stage now configures
      `--enable-static` and installs `sqlite-static`/`libuv-static`/
      `lz4-static`. Link line:
      `-tags "libsqlite3,containers_image_openpgp" -ldflags "-s -w -linkmode
      external -extldflags '-static -luv -llz4 -lsqlite3 -lm'"`. Two
      link failures had to be resolved and are recorded in the stage
      comments: (i) `libgpgme.a` does not resolve on musl
      (`undefined reference to gpgrt_lock_lock` /
      `gpg_err_code_from_syserror` — the dynamic build gets those through
      `libgpgme.so`'s `DT_NEEDED`; Alpine ships no `gpgme-static`), fixed with
      the `containers_image_openpgp` tag (pure-Go OpenPGP; only container-image
      *signature verification* differs, which caesium does not use);
      (ii) a static `libdqlite.a` has no `DT_NEEDED`, so `-luv -llz4
      -lsqlite3` are appended explicitly. The stage fails unless `ldd` shows no
      `=>` lines **and** `file` reports "statically linked". CI-proven: both
      `build-and-integration-test` and `build-and-integration-test-arm64` build
      the binary, smoke it natively (`ubuntu:24.04`, `caesium --help` +
      `caesium job lint --path docs/examples/minimal.job.yaml`) and upload
      `release-cli-amd64` / `release-cli-arm64` with a `.smoke-ok` marker
      (sha256 + runner arch). Review-only (cannot run before a `v*` tag): the
      `publish` job's marker/sha256 verification, `SHA256SUMS`, job-scoped
      `permissions: contents: write`, and `gh release create --verify-tag`
      with the Linux-only / static-artifact release-notes template —
      validated with `actionlint` (no new findings). Review fixes (W2, after
      the substitute adversarial review): the release step no longer passes
      `--generate-notes` (on a first release GitHub generates "What's
      Changed" from the repository's first commit and a body over 125,000
      characters 422s *after* the images are public) and is idempotent (a
      re-run re-uploads the assets with `--clobber` instead of failing on an
      existing release); the smoke also runs a bounded `caesium start` (which
      opens and migrates the embedded dqlite catalog under
      `CAESIUM_DATABASE_PATH`) and drives `caesium job apply` against it over
      HTTP in the same bare container — the cgo dqlite/sqlite path the static
      link exists for; `--help` and `job lint` are pure Go and prove nothing
      about that link. Verified locally on arm64 before CI.
- [x] E2. Add a `cli` justfile recipe that yields a **runnable** CLI on the
      host: `just tag=v0.1.0 cli` pulls `caesiumcloud/caesium:{{tag}}`
      (defaulting to the latest release tag resolved with
      `gh release view --json tagName` when `tag` is `latest`) and writes
      `./.tmp/caesium-cli/caesium` as a **wrapper script** that runs the CLI
      inside that image (`docker run --rm --network host -v "$PWD":/work -w
      /work caesiumcloud/caesium:<tag> caesium "$@"`, env passthrough for
      `CAESIUM_*`), prints the path, and refuses to run when the tag does not
      exist. A `docker cp` of the image's executable is **not** a host binary
      (E1's linking facts) — the existing `docker cp` in the integration
      recipes is only valid because the copied binary runs *inside* the
      builder container. On Linux, `just cli` prefers the E1 static binary
      from the release when present. Place the recipe directly after
      `push-multiarch`. Files: `justfile`.
      **Done (W2-β):** recipe added directly after `push-multiarch`. It
      resolves `tag=latest` through `gh release view --json tagName`, prefers
      the E1 static asset on Linux (`gh release download --pattern
      caesium-linux-<arch>`), and otherwise writes a wrapper script. The
      wrapper uses `--entrypoint /bin/caesium` (the plan's literal
      `… <image> caesium "$@"` would pass `caesium` as *argv[1]* to the
      image's `/bin/caesium` ENTRYPOINT), plus `--network host` on Linux
      (on macOS Docker Desktop's host network is the VM's, so the wrapper
      maps `host.docker.internal` to the host gateway instead and prints the
      `http://host.docker.internal:8080` hint — a W2 review fix),
      `-v "$PWD":/work -w /work`, `--user $(id -u):$(id -g)` so writes to the
      working directory land as the host user, and a `CAESIUM_*` env
      passthrough. A locally present image skips the pull, so the recipe is
      testable offline. Verified: `just cli` refuses with "no published GitHub
      release yet"; `just tag=v9.9.9-nope cli` refuses with the Docker Hub
      hint; `just tag=v0.0.0-w2local cli` (a local retag of
      `caesiumcloud/caesium:latest`) writes the wrapper and both
      `./.tmp/caesium-cli/caesium --help` and `… job lint --path
      docs/examples/minimal.job.yaml` succeed. `.tmp/` was already gitignored.
      Not wired: container-executing subcommands (`caesium dev`) would also
      need the Docker socket mounted.
- [x] E3. Version the Helm chart with the release. Set
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
      **Done (W2-β):** `appVersion: "v0.1.0"`, `version` left at `0.1.0`. The
      `publish` job now checks out the repo and fails on
      `appVersion` ≠ `$GITHUB_REF_NAME` before anything is pushed. **image.tag
      audit (the kind lane cannot go red on merge):** every lane that actually
      installs the chart already overrides the tag — CI's
      `helm-integration-test` passes `--set image.tag=${IMAGE_TAG}-amd64`, and
      `just k8s-distributed` sets both `image.repository` and `image.tag` to
      its local-registry dev tag. `just helm-lint` and `just helm-template`
      only lint/render (no pull), and `just helm-test` runs `helm test` against
      an already-installed release. Nothing needed fixing. The rule and the
      "override `image.tag` unless you are deploying a published release"
      guidance are documented in `docs/kubernetes-deployment.md` (Quick Start
      and Configuration Reference).
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

- [x] F1. Remove the GraphQL placeholder. Delete `api/gql/` (schema has one
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
      **Done (W1-δ):** all listed deletions/edits made as described;
      `go.mod`/`go.sum` regenerated via `go mod tidy` inside
      `caesiumcloud/caesium-builder:latest-full` (drops the two
      `graphql-go` requires). The prescribed replacement wording ("there
      is no GraphQL; surface features via REST") itself contains the
      substring the verify grep checks for case-insensitively, so it was
      reworded to "there is no alternative query API" in the three
      `.claude/skills/**` files to actually reach an empty grep. Also
      removed two further stale `pkg/client/` mentions found in
      `CONTRIBUTING.md` while editing it for this item (the repo-tree
      line and the "update the client package" step under "Adding a REST
      API endpoint") — that package is deleted by F2 in this same PR.
      Verify command confirmed empty; see F1's grep in this PR.
- [x] F2. Remove the zero-importer stubs. `internal/task/task.go` (one line:
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
      **Done (W1-δ):** all four packages had zero importers — confirmed
      via `grep -rn '"github.com/caesium-cloud/caesium/<path>"'` for each
      of `internal/task`, `pkg/client`, `pkg/bytes`, `pkg/compare` across
      the main module and separately across the `reagents/` module — and
      all four were deleted; none were kept. (`pkg/task`, a distinct,
      unrelated package with real importers and 87.4% unit coverage, was
      left untouched — do not confuse it with the deleted
      `internal/task`.) `go build ./... && go vet ./...` confirmed clean
      inside `just lint`'s container.

## Harness Strengthening

- [x] H-1. Make the auth-enabled lane real and wide. In the justfile
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
      Done: the lane now executes **5** scenarios (`TestAgentMCPToolsListBundleAndIncidentScope`,
      `TestAgentProfileCLIListJSONStdout`, `TestAgentProfileCRUD`,
      `TestAgentProfileCreateRejectsUnsupportedSecretProvider`,
      `TestIncidentCLIListJSONStdout`) with `TestIncidentRoutesGatedOffByDefault`
      inverse-skipped, up from **0**. `requireAuthLane()` (`test/auth_lane_test.go`)
      does not merely skip off-lane: on-lane it *asserts* the runner carries
      `CAESIUM_AUTH_MODE=api-key` / `CAESIUM_AGENT_REMEDIATION_ENABLED`, so
      losing the env again fails the lane instead of skipping it. Two facts the
      item did not anticipate: (i) the runner also needs
      `CAESIUM_AUTH_KEY_HASH_SECRET` to match the server's, or the agent-session
      key `TestAgentMCP…` mints hashes to a value the server cannot look up
      (401, not 200); (ii) that scenario's `pkg/db.Connection()` would open a
      *native* dqlite app bound to `CAESIUM_NODE_ADDRESS` — 127.0.0.1:9001,
      already held by the server on the shared netns — and `pkg/db` `log.Fatal`s
      on a connection error, so the guard becoming true would have killed the
      whole test binary; it now reaches the server's catalog through the dqlite
      *client* driver (`openIntegrationCatalogGorm`, over `openIntegrationCatalogDB`).
      The floor lives in the new `agent_integration_min_pass` justfile variable
      (`CAESIUM_AGENT_INTEGRATION_MIN_PASS`, default `3`) so H-2 can reuse the
      shape per lane; verified failing at `99` and passing at `3`.
- [x] H-2. Give every lane an explicit time budget and the hollow-lane guard.
      *W1 orchestrator fix-forward: the time-budget half landed early (every
      integration `go test` line now passes `-timeout 30m` — default, agent,
      podman recipes and CI's kind/podman jobs) because the default lane had
      reached 520 s against Go's 600 s default and W1 adds scenarios. The
      per-lane `--- PASS` floor half is still open.*
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
      **Done (floor half, W2):** added the same H-1(d) shape
      (`-v`, tee to a log, `grep -cE '^[[:space:]]*--- PASS:
      TestIntegrationTestSuite/'`, fail-below-floor with server-log dump,
      print-count-on-success) to the three remaining lanes that filter with
      `-run`. Observed count on a green run / chosen floor (roughly half,
      minimum 3) / env var: `integration-test-distributed` **41** scenarios
      (incl. nested subtests) / floor **20** / `CAESIUM_DISTRIBUTED_INTEGRATION_MIN_PASS`;
      `integration-test-owner-memory` **28** / floor **14** /
      `CAESIUM_OWNER_MEMORY_INTEGRATION_MIN_PASS`; `integration-test-infra`
      floor **6** (half of the 12 `TestInfra*` scenarios the `-run` pattern
      matches) / `CAESIUM_INFRA_INTEGRATION_MIN_PASS` — chosen as a static
      estimate because the local infra lane run hit Docker Desktop's VM
      running out of disk (`no space left on device` building the Terraform
      layer and git-cloning fixtures) before any real signal; per the item's
      own escape hatch this relied on CI, which confirmed all **12/12**
      `TestInfra*` scenarios PASS on both `build-and-integration-test-infra`
      and `-infra-arm64` for this PR. Both `-distributed` and `-owner-memory`
      were confirmed red at `..._MIN_PASS=99` against the saved green-run
      log (41 and 28 are both < 99), and CI reproduced the same 41/28 counts
      on both arches; `-owner-memory` also hit the pre-existing
      `TestFanOutHTTPRetryPartition` flake once (D1(a), sibling W2-α) with
      every other scenario green, distinct from a hollow-lane failure.
      `integration-test-agent` already had its floor from H-1. The
      helm and podman CI jobs run an unfiltered `go test ./test/
      -tags=integration` (no `-run`), so per the item's own carve-out they
      get no floor — only the four `-run`-filtered lanes do. `timeout-minutes`
      on `build-and-integration-test-distributed`/`-owner-memory` (45m vs.
      `-timeout 30m`) and `-infra`/`-infra-arm64` (60m vs. `-timeout 20m`,
      generous because `build-reagents` runs first) were already consistent
      from the W1 fix-forward; nothing needed raising.

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
- [x] N-3. File the unfiled follow-ups as GitHub issues. The item produces
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
      **Done (W1 close, 2026-09-07) — filed as #395–#419**, each body citing
      the deferring plan/doc and grep-verified symbols. Scheduler: #395
      fairness/quotas, #396 queue-view/claim-order/cancelRunTx, #397 `park`
      disposition, #398 local quarantined replay, #399 selective `replay
      --set` re-run, #400 owner dispatch loop spins on a completed producer
      (W1), #401 `halt` policy vs tolerant-rule successors (W1, A2), #402
      dropped `run_cancelled` reconciliation (W1). Data plane: #403
      partition-level freshness, #404 blame determinism/coverage/attribution,
      #405 registry auth + Podman/k8s digest resolution (Plan 3 C1 depends on
      it), #406 reproduce OQ #1/#4/#5, #407 manifest export endpoint, #408
      `CallbackRun` enrichment. UI: #409 event-trigger UI plan, #410 lineage
      follow-ups, #411 React Flow watermark. Platform: #412 node-affinity for
      RWO volumes, #413 notification audit log, #414 dpm-ii H-4 CI tier, #415
      infra-deploy RWX/podman lanes, #416 `perClass` narrowing unenforced
      (W1), #417 second pending approval unlistable (W1), #418 darwin CLI,
      #419 `TypeIncidentOpened` has no publisher (W1). B3 was fixed, not
      filed; D1's quarantines are filed by D1 when it runs.
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
- `.github/workflows/ci.yml`: D1/H-2 edit the **test** jobs; E1 adds steps to
  the two per-arch build jobs (`build-and-integration-test`,
  `build-and-integration-test-arm64`) and to `publish`; E3 edits `publish`
  only (job-level `permissions`, not top-level). Same wave is acceptable; H-2
  merges first, E1 rebases onto it, E3 onto E1.
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
   equals the tag and the rule is documented; the bare-host smoke test
   (E1(b)) passed **natively on each architecture's own runner** —
   `caesium --help` and `caesium job lint` succeed in a minimal glibc
   container with no extra libraries on `ubuntu-24.04` (amd64) and
   `ubuntu-24.04-arm` (arm64) — and `publish` verified both `.smoke-ok`
   markers and sha256s before attaching; the release notes state which
   artifact form shipped
   (static, or the bundled-libs fallback with the issue number); `just
   tag=v0.1.0 cli` yields a runnable `./.tmp/caesium-cli/caesium --help` on
   the host.
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
