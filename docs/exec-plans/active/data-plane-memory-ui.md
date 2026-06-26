# Data-Plane Memory UI — Surface the Causal Verbs in the Web UI

Last updated: 2026-06-26

The two completed Data-Plane Memory plans shipped the entire Retain/causal layer
**CLI + REST only** — `caesium run diff`, quarantined `replay`, `caesium why`,
`caesium blame`, the reproducibility receipt + `verify`, and the `/lineage/impact`
cross-job graph all exist as commands and HTTP endpoints, but **nothing in `ui/`
wires to them** (confirmed: zero `ui/` references). `docs/roadmap.md` §3.4
explicitly left "the UI timeline/step-through replay surface" as remaining. This
plan closes that gap: it makes the causal verbs **discoverable to non-CLI users**
inside the existing React/Vite web UI, driving the **already-shipped REST
endpoints with NO new backend**.

Current state: a user inspecting a run in the UI (`RunDetailPage` →
`TaskDetailPanel`) sees status, logs, cache info, and the DAG — but cannot ask
*why* a task re-ran, *diff* it against another run, *replay* it as a what-if,
*blame* a topology change to a commit, check its *receipt*, or see its cross-job
*lineage impact*. Target state: each of those is a first-class affordance on the
natural surface, gated by a Playwright e2e that drives the real UI against a live
backend.

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

## Source-Of-Truth Note

This is a **frontend-only** plan: every item drives an **existing** REST endpoint
shipped by the completed [data-plane-memory](../completed/data-plane-memory.md) and
[data-plane-memory-ii](../completed/data-plane-memory-ii.md) plans. **When this
plan and the shipped REST endpoints (or those completed plans) disagree, the
endpoints/plans win** — no item may add or change a backend endpoint, model, or
the job-definition schema. If a UI need genuinely requires a backend change, that
is out of scope: stop and record it as a follow-up against a *backend* plan, do
not grow an endpoint here. The exact endpoint paths/response shapes are derived
from the existing CLI HTTP clients (`cmd/run/diff.go`, `cmd/run/replay.go`,
`cmd/blame/`, `cmd/why/`, `cmd/receipt/` + `cmd/verify/`) and — for lineage, which
has **no CLI** — the `api/rest/controller/lineage/impact.go` controller. Those are
the authoritative contract for the `ui/src/lib/api.ts` methods.

## Progress (as of 2026-06-26)

**Wave 1 shipped the foundation (H-1).** Wave 2 fans out to H-2 + A1 + C1. The
foundation chain is H-1 → H-2 → H-3; feature streams unlock as their H deps land
(A/C/D/E need only H-1; B and F also need H-2/H-3).

### Wave 1 — H-1 shipped (the shared API client)

- **Stream α (H-1):** the typed `ui/src/lib/api.ts` data-verb client for all six verbs
  (run-diff, replay, why, blame, receipt/verify, lineage-impact) + a vitest unit —
  PR #247, merged `712db52`. Opus adversarial review (0 blockers, 4 should-fix applied:
  message-aware replay error classification so overloaded 400/409/422 codes aren't
  mislabeled; `BlameOptions.task`; per-verb response round-trip assertion) + 3 bot fixes
  (null-body guard, `getTaskWhy` taskName, 401 test). Caught a real image-pin guardrail
  miss (a bare base-image name in a test fixture), not a flake.

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| H | Shared `api.ts` client (H-1 ✅), principal role/scope retention (H-2), auth-enabled ui-e2e lane (H-3) | **P0** | **H-1 shipped** (#247); H-2 in W2, H-3 pending |
| A | Run-diff view (causal cache-bust attribution) on `RunDetailPage` | **P1** | Not started |
| B | Replay (quarantined what-if) dialog + typed-refusal surfacing (no pre-emptive mode-gate; 409 inline) | **P1** | Not started |
| C | Per-task causal explainer (`why`) in `TaskDetailPanel` | **P1** | Not started |
| D | Blame view (commit/snapshot topology attribution) | P2 | Not started |
| E | Receipt display + `verify` of a user-supplied **committed** receipt (drift / degraded) | P2 | Not started |
| F | Dataset-keyed **downstream** lineage-impact graph (ReactFlow) + ui-e2e lineage enable | P2 | Not started |
| N | Roadmap §3.4 flip + cross-links (runs last) | — | Not started |

## Streams

### Stream H — Shared data-verb API client (foundation)

`ui/src/lib/api.ts` is the single REST client every feature touches; consolidating
all the new methods into ONE item keeps the feature streams from colliding on that
shared file. This item adds typed client methods + response types only — no UI.

- [x] H-1. Add the data-verb client surface to `ui/src/lib/api.ts`, mirroring the
      existing `request<T>` / `requestURL<T>` + `withAuthHeaders` pattern (no new
      backend). **All six endpoints are confirmed to exist as REST controllers** (codex
      round-1 verification) — derive each exact path + response type from the controller
      + service + the CLI client, NOT from guesswork:
      - `getRunDiff(jobId, left, right)` → `GET /v1/jobs/:id/runs/diff?left=&right=`
        (`api/rest/controller/rundiff/` + `service/rundiff/`; `cmd/run/diff.go`).
      - `postReplay(jobId, runId, {set}, idempotencyKey)` → `POST /v1/jobs/:id/runs/:run_id/replay`
        (`api/rest/controller/replay/`; `cmd/run/replay.go`) — sends `Idempotency-Key`;
        maps **400** (missing key) / **403** (insufficient role — replay requires
        `RoleRunner`, `internal/auth/rbac.go`) / **404** (cross-job) / **409** (requires
        distributed execution — dispatch-needing replay in local mode) / **422** (replay-safe
        gate) to **typed errors** the UI can switch on.
      - **403 across the board:** every method maps a **403** to a typed
        insufficient-role/scope error (the causal reads are viewer-level; replay needs
        runner; `/lineage/impact` denies *scoped* principals — `api/middleware/auth_scope.go`).
        See **H-2** (role/scope retention) + the per-stream auth handling.
      - `getTaskWhy(jobId, runId, taskId)` → the `why` endpoint (`api/rest/controller/why/`
        + `service/why/`; `internal/run/why.go` `WhyExplanation` = cache/hash/trigger/
        baseline + a `BlobDiff` whose `degraded` flag means **field-diff unavailable**
        — blob missing/oversized/version-skewed/unparsable — NOT the receipt's unpinned
        state).
      - `getBlame(jobId, {from?, to?})` → `GET /v1/jobs/:id/blame` (`api/rest/controller/blame/`
        + `service/blame/`; `cmd/blame/`).
      - `getReceipt(jobId, runId)` → `api/rest/controller/receipt/`; and
        `postVerify(committedReceiptBody)` → the verify route, which compares a
        **caller-supplied COMMITTED receipt** against freshly re-derived state
        (`internal/receipt/verify.go`) — see Stream E for the contract (it is NOT a
        self-check of the current receipt).
      - `getLineageImpact({namespace, name, maxDepth?})` → `GET /v1/lineage/impact?namespace=&name=&max_depth=`
        (`api/rest/controller/lineage/impact.go`; `internal/lineage/impact.go`). **There
        is NO lineage CLI** — derive from the controller. The response is **dataset-keyed:
        a root dataset + DOWNSTREAM only** (no upstream, no run-scoped graph) — see Stream F.
      Add the TypeScript response types next to the existing ones, and a vitest unit
      asserting the URL/headers/error-mapping each method builds.
      Files: `ui/src/lib/api.ts`.
      Note: Landed getRunDiff (`/jobs/:id/runs/diff`), postReplay (`/jobs/:id/runs/:run_id/replay`), getTaskWhy (`/jobs/:id/runs/:run_id/why`), getBlame (`/jobs/:id/blame`), getReceipt (`/jobs/:id/runs/:run_id/receipt`), postVerify (`/jobs/:id/runs/:run_id/receipt/verify`), and getLineageImpact (`/lineage/impact`) with `ApiError.kind` typed 403/replay refusals.
- [x] H-2. Retain the principal's **role + scope** for affordance-gating. The UI already
      calls `GET /auth/whoami` (`ui/src/lib/auth.ts:166`; the endpoint returns `role`,
      `api/rest/controller/auth/sso.go`) but discards the role — capture it (and any scope
      marker) into the auth store and expose a `usePrincipal()` accessor (`role`,
      `canRunner`, `isScoped`). Add a shared `InsufficientAccess` affordance component that
      renders the typed **403** (insufficient role / scoped-principal-denied) as an inline
      explanation, not a raw error. This is what B/F gate on. Files: `ui/src/lib/auth.ts`,
      `ui/src/features/auth/` (+ a small shared component). Depends on: H-1.
      Note: Retained `whoami` role/principal state in `usePrincipal()`; current `whoami` has no scope/jobs marker, so API-key scopedness remains unknown without backend support.
- [ ] H-3. Add an **auth-enabled ui-e2e lane** so the RBAC gating is actually exercised
      (today `just ui-e2e` runs auth-disabled — `justfile`/`.github/workflows/ci.yml` — so a
      viewer-sees-Replay or scoped-sees-lineage bug would never be caught). Add a Playwright
      project (or a dedicated server instance) started with auth enabled and **seeded
      viewer / runner / scoped keys**, plus an e2e helper to log in as each. The default
      lane stays auth-disabled for the happy-path specs. Files: `justfile`,
      `.github/workflows/ci.yml`, `ui/playwright.config.ts`, `ui/e2e/helpers/auth.ts`.
      Depends on: H-2.

### Stream A — Run Diff (causal cache-bust attribution)

Put "what changed between these two runs, and why did each task re-run" on the run
surface. Honest scoping (from the design): this is **cache-bust attribution**
(which step/output changed + why a task re-ran) — full row/column **value** diffs
are explicitly handed off to dbt/Datafold; the UI says so.

- [x] A1. Build the run-diff view: a "Compare to run…" affordance in the
      `RunDetailPage` header (a picker over the job's other runs) that opens a diff
      view rendering `getRunDiff` — per-task changed vs. cache-hit, the discriminating
      cache-bust field, and a one-line "value diffs → dbt/Datafold" note. Read-only;
      reuse the `TaskDetailPanel` metadata-grid shape and `StatusBadge`. Use a
      `/jobs/$jobId/runs/$runId/diff?to=$other` route (TanStack Router) so it is
      linkable.
      Files: new `ui/src/features/jobs/RunDiffView.tsx` (+ a `TaskDiffRow`),
      `ui/src/features/jobs/RunDetailPage.tsx` (the affordance), `ui/src/router.tsx`.
      Depends on: H-1.
      Note: W2-beta added the linkable `RunDiffView`, route, and run-header compare picker against the existing run-diff endpoint.
- [x] A2. Playwright e2e: apply a job, trigger two runs differing by a `--set`/param,
      open the diff, assert the changed task renders the discriminating field and an
      unchanged task renders as cache-hit. Assert on `data-testid` DOM, not internals.
      Files: `ui/e2e/run-diff.spec.ts`. Depends on: A1.
      Note: W2-beta added a Playwright spec that creates one command-changed task plus one unchanged cached task because v1 hashes run params into every task.

### Stream B — Replay (quarantined what-if)

Let an operator launch a quarantined replay from the run they are looking at.
**Design note (codex round 1):** execution mode is NOT exposed to the UI today
(`api/rest/service/system/system.go` features = console flags + external_url only;
`/health` = status/uptime/checks), and the backend only refuses a replay when the
*prepared* replay actually **requires dispatch** (`api/rest/service/replay/replay.go`;
`internal/replay/replay.go`) — a **no-override / cache-hit replay succeeds in local
mode**. So the UI does NOT pre-emptively disable replay (that would both be
unobservable and wrongly hide the supported cache-hit path). Instead it **submits
and surfaces the backend's typed refusal** (the 409 carries "requires distributed
execution mode"). Exposing execution-mode for a pre-emptive affordance is a
**backend follow-up, out of scope** for this frontend plan (record it, don't build it here).

- [ ] B1. Build the replay action: a "Replay…" button in the `RunDetailPage` header
      opening a `ReplayDialog` (key/value rows for `--set` overrides + an optional
      idempotency key; auto-generate + display one if omitted) that calls `postReplay`,
      shows the returned **quarantined** run (a `Quarantine` badge), and offers a
      "show diff vs baseline" that reuses Stream A's `RunDiffView`.
      Files: new `ui/src/features/jobs/ReplayDialog.tsx`,
      `ui/src/features/jobs/RunDetailPage.tsx`. Depends on: H-1 (A1 soft, for the diff reuse).
- [ ] B2. Role-gate + refusal surfacing (no pre-emptive *mode*-gate): replay requires
      `RoleRunner` — via H-2's `usePrincipal()`, show the Replay action only for runner+
      (a viewer sees it disabled with an explanation, not a dead button). Then map H-1's
      typed errors to clear inline messages — **403** → insufficient role (defense-in-depth
      if a non-runner reaches it); **409** → "this replay re-executes tasks, which requires
      distributed execution mode"; **422** → the replay-safe-gate refusal naming the step;
      **404** → cross-job/not-found; **400** → missing/blank idempotency key. Never render a
      silent success-shaped result for a refusal. (A no-override cache-hit replay succeeds
      and shows its quarantined run.)
      Files: `ui/src/features/jobs/ReplayDialog.tsx`. Depends on: B1, H-2.
- [ ] B3. Playwright e2e: on the **default (auth-disabled) lane** — submit a **no-override**
      replay (cache-hits → succeeds), assert the quarantined run appears AND is excluded from
      the normal run list; submit a `--set` override (dispatch-requiring) and assert the
      **409** renders as an inline error with no run-shaped success; assert a non-replay-safe
      baseline surfaces the 422. On the **auth-enabled lane (H-3)** — assert a *runner* key
      sees + can launch the no-override replay, and a *viewer* key does NOT get an actionable
      Replay (gated affordance, no 403 round-trip-as-success). (Mirror the B4/B5 constraint:
      only the no-override path can succeed locally.)
      Files: `ui/e2e/replay.spec.ts`. Depends on: B1, B2, H-3.

### Stream C — Causal explainer (`why`)

Answer "why did this task run / skip / re-run / cache-hit" inline on the task.

- [x] C1. Add a "Why this status?" section to the `TaskDetailPanel` **Details** tab
      driving `getTaskWhy`: render the `WhyExplanation` (cache/hash/trigger/baseline) and
      the discriminating `HashInput` field for why the task ran/skipped/re-ran/cache-hit.
      Render ONLY the backend-provided state — when `BlobDiff.degraded` is set, show the
      backend's reason that the **field-level diff is unavailable** (blob missing/oversized/
      version-skewed/unparsable); do NOT invent an "unpinned/unverifiable" claim here (that
      reproducibility state lives in receipt/verify, Stream E — not in the `why` endpoint).
      For a cache-hit, link to the source run/task.
      Files: new `ui/src/features/jobs/TaskWhyView.tsx`,
      `ui/src/features/jobs/TaskDetailPanel.tsx`. Depends on: H-1.
      Note: Added `TaskWhyView` in the Details tab, rendering verdict, trigger, baseline, diff/degraded reason, hash values, and cache-hit source run/task from `getTaskWhy`.
- [x] C2. Playwright e2e: run a job, open a re-run task and a cache-hit task, assert the
      "why" section renders the discriminating field for each (and the source-run link for
      the cache-hit). Files: `ui/e2e/why.spec.ts`. Depends on: C1.
      Note: Added an e2e that enables cache on an existing fixture, runs changed params for a `runParams.scenario` miss, then reruns matching params for a cache hit.

### Stream D — Blame (topology attribution)

- [ ] D1. Build a blame view (a `/jobs/$jobId/blame` route) driving `getBlame`: render
      commit/snapshot attribution over `dag_snapshot` (topology + image + command), with
      the **coverage caveat** surfaced in the UI (env/spec/retries/cache/schema/sla/
      triggerRules are intentionally untracked). Optional `--from/--to` commit-range
      pickers.
      Files: new `ui/src/features/jobs/BlameView.tsx`, `ui/src/router.tsx`.
      Depends on: H-1.
- [ ] D2. Playwright e2e: apply a job through the provenance path (apply-provenance /
      git-sync) with distinct commits, open the blame view, assert the commit attribution
      + the coverage caveat render. Files: `ui/e2e/blame.spec.ts`. Depends on: D1.

### Stream E — Reproducibility receipt + verify

**Design note (codex round 1):** `caesium verify`'s contract is to compare a
**caller-supplied COMMITTED receipt** (loaded from disk / git) against the server's
freshly re-derived state (`cmd/verify/verify.go`; `api/rest/controller/receipt/`;
`internal/receipt/verify.go`). Verifying the receipt the UI *just fetched* would be a
tautology (current-vs-current → always passes) and would NOT detect drift. So the UI
must let the user **provide the committed receipt** to verify against.

- [ ] E1. Receipt display: a run-level panel on `RunDetailPage` driving `getReceipt` —
      render the current content-addressed receipt (atoms/digests/inputs), with the
      **unverifiable markers the backend emits** (an unpinned mutable-tag task shown
      unverifiable, never silently presented as reproducible). Display only; no verify yet.
      Files: new `ui/src/features/jobs/ReceiptPanel.tsx`,
      `ui/src/features/jobs/RunDetailPage.tsx`. Depends on: H-1.
- [ ] E2. Verify action: in the receipt panel, accept a **committed receipt** as input
      (paste JSON / upload a receipt file — mirroring how `caesium verify` reads a committed
      receipt from disk), `postVerify` it, and render the **actual `VerifyResult.drifts`
      kinds the backend returns** (`internal/receipt/verify.go`) — reproducible /
      `manifest_changed` / `git_commit_changed` / degraded-unverifiable. Make explicit this
      checks a *committed* receipt against current state, NOT a self-check of the just-fetched
      one. **Codex round 2:** a UI re-apply writes a new snapshot but does NOT mutate the
      completed run's `task_runs`, so field-level *image/command* drift is NOT reachable via
      a UI re-apply (it needs the run's task rows to differ) — do NOT promise it; per-field
      image/command attribution is a **backend follow-up**, out of scope here.
      Files: `ui/src/features/jobs/ReceiptPanel.tsx`. Depends on: E1.
- [ ] E3. Playwright e2e: run a job, assert the receipt renders (incl. an unpinned-tag job
      → unverifiable markers); capture the committed receipt from `getReceipt`; verify it
      against UNCHANGED state → reproducible; then re-apply a changed definition and verify
      again → assert the **reachable** drift kind (`manifest_changed` / `git_commit_changed`),
      NOT a field-level image/command field. Files: `ui/e2e/receipt-verify.spec.ts`. Depends on: E2.

### Stream F — Lineage / impact graph

**Design note (codex round 1):** the only shipped endpoint is **dataset-keyed**
`GET /v1/lineage/impact?namespace=&name=&max_depth=` returning a **root dataset +
DOWNSTREAM only** (`api/rest/controller/lineage/impact.go`; `internal/lineage/impact.go`)
— there is no run-scoped or upstream graph API, and no lineage CLI. So Stream F is a
**dataset-keyed downstream-impact** view, not a run-scoped bidirectional one.

- [ ] F1. Build a dataset-impact view (route `/lineage?namespace=&name=`) driving
      `getLineageImpact`: a ReactFlow graph **reusing the dagre layout from `JobDAG.tsx`**
      rendering the root dataset + its DOWNSTREAM impact (the actual response shape; no
      upstream). **Codex round 2 constraints:** (a) `ImpactNode` carries `job_id`/`job_alias`
      but **NO run_id** — click-through is **job-level only** (to the dependent job), not
      run-level (run-level links are a backend follow-up); (b) the run/task JSON does not
      expose persisted lineage dataset identities, so the entry point is an **explicit
      namespace/name input** (deriving the dataset from a run is a backend follow-up); (c)
      `/v1/lineage/impact` **denies scoped principals** (`api/middleware/auth_scope.go`) — via
      H-2's `isScoped`, render an explanatory "cross-job lineage requires an unscoped key"
      state instead of firing a 403. Honest empty state: "no downstream impact recorded yet
      (lineage datasets populate as jobs declare/consume outputs)" when downstream is empty.
      Files: new `ui/src/features/jobs/LineageGraph.tsx`, `ui/src/router.tsx`.
      Depends on: H-1, H-2.
- [ ] F2. Enable lineage on the ui-e2e backend so F's success path is actually exercised:
      add `CAESIUM_OPEN_LINEAGE_ENABLED=true` (+ a console transport) to the `ui-e2e` server
      startup in the `justfile` and the CI `ui-e2e` job — today neither sets it, so an
      empty-state-only test would go green without ever rendering a real impact graph.
      Files: `justfile`, `.github/workflows/ci.yml`. Depends on: (none; can land with F1).
- [ ] F3. Playwright e2e (lineage enabled per F2): run jobs that declare/consume a dataset,
      open the impact view for that dataset (namespace/name), assert the **downstream** graph
      renders nodes/edges with **job-level** click-through; assert the honest empty state for a
      dataset with no downstream. On the **auth-enabled lane (H-3)**: assert a *scoped* key
      gets the "requires an unscoped key" explanation, not a raw 403.
      Files: `ui/e2e/lineage.spec.ts`. Depends on: F1, F2, H-3.

## Navigational / Organizational Improvements — Stream N

- [ ] N-1. Flip `docs/roadmap.md` §3.4 — replace "Remaining beyond that: the UI
      timeline/step-through replay surface" with the shipped UI verbs — and add this plan
      to `docs/README.md`'s index + a forward note in the completed data-plane-memory
      plans. Concentrate all roadmap/README edits here so the feature streams don't collide
      on those shared docs. **When referencing this plan in `docs/README.md`, use
      backtick/inline-code form** (`` `exec-plans/active/...` ``) — NOT a clickable `(….md)`
      link: the `TestDocsREADMEIndexesEveryTopLevelDoc` guardrail requires every README
      markdown-link basename to be a top-level `docs/*.md` file, so a subdirectory link
      fails the build (PR #245 hit exactly this).
      Files: `docs/roadmap.md`, `docs/README.md`. Depends on: A–F (runs last).

## Sequencing & Dependencies

**Cross-stream order.**
- **Stream H is the foundation chain: H-1 → H-2 → H-3.** H-1 (the `api.ts` client) gates
  every feature item; H-2 (role/scope retention) gates B (runner-only Replay) and F
  (scoped-lineage handling); H-3 (the auth-enabled e2e lane) gates the auth assertions in
  B3 and F3. H-1 lands first; H-2 next; H-3 can land alongside the first feature wave.
- After H-1, **A, C, D, E are independent** (each owns its own new component + e2e file).
  **B** additionally needs H-2 (B2) and H-3 (B3); **F** additionally needs H-2 (F1) and
  H-3 (F3). **B** soft-depends on A1 (it reuses `RunDiffView`); schedule B after A1 if the
  diff reuse is wanted, else stub it.
- **N-1 runs last**, after A–F, so the roadmap flip reflects what actually shipped.

**Within-stream order.** B1 → B2 (role-gate + refusals, needs H-2) → B3 (e2e, needs H-3).
E1 → E2 → E3. F1 (needs H-2) / F2 → F3 (needs H-3). Every feature stream's e2e item depends
on its build item(s).

**Cross-stream file conflicts (sequence, don't parallelize within a wave).**
- `ui/src/lib/api.ts` — **owned solely by H-1**; feature streams must NOT edit it
  (that is the whole point of consolidating it). If a feature finds a missing
  method, add it to H-1 (or a small H-2 follow-up), not inline.
- `ui/src/features/jobs/RunDetailPage.tsx` — **A1, B1, E1** each add a header
  affordance. Sequence **A1 → B1 → E1** (or split across waves); do not run them in
  the same parallel wave.
- `ui/src/router.tsx` — **A1, D1, F1** each register a route. Sequence them (route
  additions are small but conflict on the route tree); or land an H-2 that adds the
  route stubs first.
- `ui/src/features/jobs/TaskDetailPanel.tsx` — **C1 only** (no conflict).
- `ui/src/lib/auth.ts` / `ui/src/features/auth/` — **H-2 only** (role/scope retention); B
  and F consume the `usePrincipal()` accessor, they don't edit auth.
- `justfile` / `.github/workflows/ci.yml` — **H-3** (auth-enabled lane) and **F2** (lineage
  env) both edit these; sequence **H-3 → F2** (or land together) and keep them isolated from
  other waves' CI edits.
- `docs/roadmap.md` / `docs/README.md` — **N-1 only** (no conflict).

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Plus the **UI conditional gates (mandatory for this plan — every item touches
`ui/**`):**

```sh
just ui-lint           # eslint
just ui-test           # vitest run + production build (build:ci, bundle-size check)
just ui-e2e            # Playwright against a live server + backend — the end-to-end gate
```

The Go chain still runs because `ui/embed.go` bundles the built UI into the binary,
but **`just ui-e2e` is the primary signal for this plan**: per CLAUDE.md's
end-to-end-coverage principle, every new UI surface ships a Playwright spec in
`ui/e2e/` that drives the real UI against a live backend and asserts on observed
DOM (`data-testid`), not on component internals. Each PR also ticks its checkbox,
appends the active-wave `## Progress` bullet, and refreshes any cross-linked doc.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream H:** `ui/src/lib/api.ts` exposes typed methods (incl. a typed **403**) for all
   six verbs hitting the same REST route their CLI clients do (api unit test green); the auth
   store retains the principal role/scope from `/auth/whoami` with a `usePrincipal()`
   accessor (H-2); and an auth-enabled ui-e2e lane with seeded viewer/runner/scoped keys
   exists (H-3). No backend endpoint was added or changed.
2. **Stream A:** the run-diff view renders per-task cache-bust attribution from
   `RunDetailPage`; `ui/e2e/run-diff.spec.ts` green.
3. **Stream B:** the replay dialog is shown only to runner+ (role-gated via H-2), launches a
   no-override quarantined replay successfully, and surfaces the backend's typed refusals
   (403/409/422/404/400) as inline errors — no pre-emptive mode-gate; `ui/e2e/replay.spec.ts`
   green on both the auth-disabled and auth-enabled lanes.
4. **Stream C:** the `TaskDetailPanel` "why" section renders the discriminating
   `HashInput` field and the backend's `BlobDiff.degraded` reason when set;
   `ui/e2e/why.spec.ts` green.
5. **Stream D:** the blame view renders commit/snapshot attribution + the coverage
   caveat; `ui/e2e/blame.spec.ts` green.
6. **Stream E:** the receipt panel renders the receipt (incl. unverifiable markers) and
   verifies a user-supplied **committed** receipt, rendering the backend's reachable drift
   kinds (`manifest_changed`/`git_commit_changed`, not field-level image/command);
   `ui/e2e/receipt-verify.spec.ts` green.
7. **Stream F:** the dataset-keyed **downstream** lineage-impact graph renders nodes/edges
   with **job-level** click-through (lineage enabled on the ui-e2e backend), the honest empty
   state, and the scoped-principal "requires unscoped key" explanation; `ui/e2e/lineage.spec.ts`
   green.
8. **Cross-cutting:** `docs/roadmap.md` §3.4 reflects the shipped UI surface, this
   plan's per-stream Progress entries match merged PRs, and no new backend endpoints
   were introduced (the plan drove existing REST only).

## How To Pick Up Work

1. Read this file end-to-end so you understand the streams, their
   interdependencies, and which acceptance criterion the item closes.
2. Pick an unchecked item under `## Streams` whose `Depends on:` line is satisfied
   (consult `## Sequencing & Dependencies`) — H-1 first.
3. Branch from `master` (or land in a worktree if dispatched by `exec-plan-wave`);
   do the work as a self-contained PR. **Drive the existing REST endpoints only — no
   backend changes.**
4. Run the verification block under `## Verification (Run For Every PR)`, including
   the UI gates.
5. Tick the checkbox, add a per-stream bullet to the active wave subsection in
   `## Progress`, and update any cross-linked doc in the same PR.
6. Open the PR with title `<Imperative subject> (data-plane-memory-ui <wave>-<stream>)` —
   e.g. `Add run-diff view to RunDetailPage (data-plane-memory-ui W1-α)`.

## Cross-References

- `docs/roadmap.md` §3.4 (Live DAG Debugging & Run Diff) — the UI surface this plan ships.
- `docs/exec-plans/completed/data-plane-memory-ii.md` — the causal verbs (run diff /
  blame / replay) whose endpoints this UI drives.
- `docs/exec-plans/completed/data-plane-memory.md` — the substrate verbs (why /
  receipt / verify / lineage) whose endpoints this UI drives.
- `docs/design-quarantined-replay.md` — the replay safety model the replay UI must
  honor (quarantine, the replay-safe gate, distributed-only execution).
- `ui/src/lib/api.ts`, `ui/src/features/jobs/RunDetailPage.tsx`,
  `ui/src/features/jobs/TaskDetailPanel.tsx`, `ui/src/features/jobs/JobDAG.tsx` — the
  surfaces and patterns the streams extend.
