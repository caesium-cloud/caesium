# Console Operator Loop UX — Job Detail, Run, and DAG Surfaces

Last updated: 2026-07-02

This plan closes the gap between what the Caesium console *shows* and what an
operator actually needs from the job-detail, run-detail, and DAG surfaces:
*what is this pipeline, is it healthy, what happened last run, and let me act.*
It is driven by a focused UX review of those surfaces. The core loop is
currently obscured in four ways: the run page leads with the reproducibility
receipt and buries status/timeline/DAG/logs; healthy `SUCCEEDED` runs are
dressed in red because digest-pinning is off by default; the trigger fires with
no params/confirmation and lands on the attestation view; and the job-detail
header overflows so that `Pause` is clipped off-screen with no overflow menu.

The receipt/`why`/`blame`/`diff`/`replay` affordances these pages carry were
shipped by the completed
[`data-plane-memory-ui.md`](../completed/data-plane-memory-ui.md) plan and the
`2.4 UI Refresh` initiative. This plan does **not** change those features'
semantics — it changes their *placement and visual weight* so the diagnostic
substance survives contact with a real operator. The work is UI-first and
cleanly partitioned across four surfaces (run page, job-detail page, DAG canvas,
log drawer), so the streams parallelize with no shared-file collisions; the one
backend touch (a queue-cancel endpoint) is isolated to a single item.

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

## Source-Of-Truth Note

When this plan and `docs/roadmap.md` disagree on strategic priority, the roadmap
wins (the operator-loop refinement is a follow-up to `§2.4 UI Refresh` and
overlaps the remaining UI surface of `§3.4 Live DAG Debugging`). The
receipt / `why` / `blame` / run-diff / replay affordances this plan
*reorganizes* are owned by the completed
[`data-plane-memory-ui.md`](../completed/data-plane-memory-ui.md); when the two
disagree on how those affordances behave, that plan's contract wins — this plan
only relocates and re-weights them, it does not redefine their meaning. Every
stream is UI-gated by the `just ui-lint && just ui-test && just ui-e2e` chain;
a stream is not done until its behavior is asserted through the real surface
(component test or Playwright e2e against a live backend), never a snapshot of
internal state.

## Progress (as of 2026-07-02)

No implementation waves have shipped yet. The plan was published from the
job-detail + DAG UX review; the first wave is the next eligible run of the
`exec-plan-wave` skill against this doc. All four streams are leaf-eligible on
wave 1 (see `## Sequencing & Dependencies` for the two soft cross-stream
edges).

A round of Codex adversarial review (2026-07-02) hardened **B5** (the queue-cancel
endpoint — the plan's only backend mutation): it now pins the exact route/method,
requires an explicit Operator RBAC policy entry (verified against the nearest
analog `PUT …/backfills/:id/cancel` and the `TestAuthMiddlewareMountedRoutesHaveRBACPolicy`
completeness test), a race-safe conditional delete on the unclaimed row with a
409 once the dequeuer has claimed it, and an integration test asserting no
`JobRun` is created on cancel plus the claimed-race case.

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Run page: lead with status/timeline/DAG/logs, demote & de-alarm the receipt, order tasks | **P0** | Not started |
| B | Job-detail page: overflow-safe header, intentional trigger, honest queue & counts, deep-links | **P0** | Not started |
| C | DAG canvas & node legibility: task-name-first nodes, trivial-graph fit, no watermark | P1 | Not started |
| D | Task log drawer: room to breathe, no mid-word wrap, disambiguated state labels | P1 | Not started |

## Streams

### Stream A — Run page: lead with reality, demote attestation

The run page currently opens with ~1.5 screens of attestation metadata
(`RECEIPT_DIGEST`, `MANIFEST_CONTENT_HASH`, per-task `IDENTITY_HASH`,
`DEGRADED=true`) before the operator reaches status, the execution timeline, the
DAG, or per-task logs. Worse, a perfectly healthy `SUCCEEDED` run is covered in
red "degraded-unverifiable" / "digest_pinned=false" badges and amber "cannot
attest" warnings purely because digest-pinning/caching is off by default — red
is the universal "something's broken" signal, so every normal run looks
alarming. This stream leads with what happened and reserves red for real
failure.

- [ ] A1. Reorder `RunDetailPage` so the run leads with status + execution
      timeline + DAG + per-task logs; move `ReceiptPanel` into a collapsed
      "Reproducibility" section (or dedicated tab) below the fold, so attestation
      is one interaction away rather than the first thing on the page.
      Files: `ui/src/features/jobs/RunDetailPage.tsx`.
- [ ] A2. Reserve red for real failures in the receipt panel: render "not
      attested", "caching disabled", "digest_pinned=false", and
      "degraded / unverifiable" as neutral, informational text (muted/secondary
      styling), not red/amber alarm badges. Keep red strictly for actual task
      failure or attestation *tampering* (a computed digest that does not match
      the recorded one).
      Files: `ui/src/features/jobs/ReceiptPanel.tsx`.
- [ ] A3. Reconcile the run page's timeline-vs-DAG surfaces so they no longer
      read as a duplicated DAG (the review flagged "the DAG is rendered twice —
      overlay up top, full DAG at bottom"). Label the execution-timeline gantt
      and the interactive DAG distinctly, and/or collapse the redundant surface;
      if the run page in fact renders only one `JobDAG`, document the
      reconciliation in the PR and resolve as verified-not-a-bug.
      Files: `ui/src/features/jobs/RunDetailPage.tsx`.
      Depends on: A1.
- [ ] A4. Order the run's task rows in execution order (by start time, breaking
      ties with DAG topological order) before rendering the timeline, so the
      task list reads top-to-bottom in the order things actually ran ("convert
      before list").
      Files: `ui/src/features/jobs/RunTimeline.tsx`.

### Stream B — Job-detail page: actionable header, intentional trigger, honest queue & counts

The job-detail header crams six view-"tabs" (Runs, Tasks, Config, YAML,
Backfills, Cache) plus three action buttons (Trigger, Backfill, Pause) into one
non-wrapping row; at ~1150px `Pause` is clipped off-screen with no overflow
menu, so the operator cannot pause a job at all, and the icon-only controls
carry no accessible labels. Trigger fires instantly with no param entry and no
confirmation, then throws the operator to the attestation view. The DAG overlay
counters silently drop failed tasks ("0/3 done · 2 queued" for a run with one
failure), and a stale "1 pending" queue row reads as an alarm with no
explanation or action. This stream makes the page's controls reachable, its
trigger deliberate, and its status honest.

- [ ] B1. Rework the header layout: split the view-"tabs" from the action
      buttons, move secondary actions (Backfill, Pause) into an
      always-reachable "⋯" overflow menu so `Pause` is never clipped at narrow
      widths, and add `aria-label`s to every icon-only control (Trigger, Backfill,
      Pause, and the theme/search icons).
      Files: `ui/src/features/jobs/JobDetailPage.tsx`.
- [ ] B2. Trigger with intent: replace the one-click Trigger with a dialog that
      offers optional run params (e.g. `logical_date`) and an explicit confirm
      (or a brief undo window), and land the operator on the live DAG +
      streaming-logs view — which A1's run-page reorder now leads with — rather
      than the attestation view.
      Files: `ui/src/features/jobs/JobDetailPage.tsx`, new
      `ui/src/features/jobs/TriggerDialog.tsx`.
      Depends on: A1 (for the live-progress landing view).
- [ ] B3. Fix the DAG overlay counters (`DagCounters`) to surface failures and
      blocked tasks instead of dropping them: render "N done · N failed · N
      blocked" (with running/cached as today) so a failed run reads
      "0 done · 1 failed · 2 blocked", not "0/3 done · 2 queued".
      Files: `ui/src/features/jobs/JobDetailPage.tsx`.
- [ ] B4. Queue triage (diagnostic, UI-only): on each run-queue row show *why*
      it is pending (priority/position, and blocked-vs-waiting reason where the
      API exposes it) alongside its age, and link the row to inspect the queued
      run. Confirm a stale "enqueued 3h ago" row reflects real queue state, not
      seed data or a genuine scheduler stall — capture the finding in the PR.
      Files: `ui/src/features/jobs/JobDetailPage.tsx`.
- [ ] B5. Wire a queue-cancel affordance end-to-end (no dead buttons). This is
      the plan's **one backend mutation** and there is currently no dequeue
      endpoint (only `cancelBackfill`), so it is specified tightly — an
      under-specified version risks a Cancel button that 403s under auth, is
      assigned too-permissive a role, or reports success while the run still
      starts:
      - **Route**: add `DELETE /v1/jobs/:id/queue/:queue_id` (new
        `api/rest/controller/job/queue/delete.go` + a `Cancel`/`Dequeue` method
        in `api/rest/service/job/`), bound in `Protected()` of
        `api/rest/bind/bind.go` next to the existing queue route. Mind the two
        path forms: `bind.go` registers routes **group-relative** — the existing
        entry is `g.GET("/jobs/:id/queue", …)` and the new one is
        `g.DELETE("/jobs/:id/queue/:queue_id", …)` (no `/v1`; the parent group
        adds it) — while the **public URL and the RBAC policy key** carry the
        full `/v1/jobs/:id/queue/:queue_id` prefix (see `internal/auth/rbac.go`,
        where the existing policy key is `GET /v1/jobs/:id/queue`).
      - **Auth**: add an explicit entry to the policy map in
        `internal/auth/rbac.go` gated at `models.RoleOperator` — matching the
        nearest analog `PUT /v1/jobs/:id/backfills/:id/cancel` (settle
        Operator-vs-Runner deliberately; a Runner-gated variant is defensible if
        the team wants trigger-parity, but pick one on purpose). A mounted route
        with no policy entry fails
        `TestAuthMiddlewareMountedRoutesHaveRBACPolicy`
        (`api/auth_rbac_policy_completeness_test.go`) and is unreachable under
        auth. Add scoped-API-key allow/deny/audit coverage per
        `api/middleware/auth_scope.go`.
      - **Race-safe semantics**: cancel is a **conditional delete on the
        unclaimed row only** — `DELETE … WHERE id = ? AND claimed_by = ''`.
        `RunQueue` carries `ClaimedBy`/`ClaimedAt`; the dequeuer claims a row
        *before* it becomes a `JobRun`, and `queue.List` surfaces only unclaimed
        rows. If zero rows are affected (already claimed/started), return
        **409 Conflict** — never report success, because deleting a claimed row
        would not stop the run that is already starting.
      - **Integration test** (`test/`, `//go:build integration`, driven through
        the live server): (a) enqueue → cancel the unclaimed row → assert the row
        is gone **and no `JobRun` was created**; (b) a claimed/dequeuer-race case
        → cancel returns **409** and the run proceeds to a `JobRun`. Capture
        stdout separately if a CLI verb is added.
      - **UI**: an `api.ts` client method + a Cancel action on the queue row that
        surfaces the 409 ("already started — can't cancel") distinctly from
        success.
      - If the team descopes the mutation, B4 ships inspect-only and this item is
        recorded as deferred — do not ship a Cancel button wired to nothing.
      Files: `api/rest/controller/job/queue/`, `api/rest/service/job/`,
      `api/rest/bind/bind.go`, `internal/auth/rbac.go`,
      `api/auth_rbac_policy_completeness_test.go`, the run-queue store
      (`internal/run/store.go` or the run-queue store package),
      `ui/src/lib/api.ts`, `ui/src/features/jobs/JobDetailPage.tsx`, `test/`.
      Depends on: B4.
- [ ] B6. Make the secondary views (Runs, Tasks, Config, YAML, Backfills, Cache)
      linkable sub-routes instead of modal state, so an operator can deep-link
      to a job's Config/YAML and the browser back button closes the view instead
      of navigating away.
      Files: `ui/src/features/jobs/JobDetailPage.tsx`, `ui/src/router.tsx`.
      Depends on: B1.

### Stream C — DAG canvas & node legibility

The DAG renders cleanly left-to-right, but the node design fights the operator's
scan: the node's *largest* text is the image (`alpine:3.23`) while the task name
— the primary identifier — is small, `shortId`-truncated, and hard-cut mid-word
("bootstra", "process-", "emit-liv"). Single-task DAGs waste the whole canvas
(one node floats in a huge empty grid because auto-fit doesn't scale it up) and
render dangling input/output handles on nodes with no edges. And a "React Flow"
attribution watermark shows bottom-right on every DAG, which reads as unbranded
for a product surface.

- [x] C1. Promote the task name to the node's primary line — the full task name
      with `text-overflow: ellipsis` + a `title` tooltip — and demote the image
      to a secondary line; stop `shortId`-truncating the label so operators scan
      the identifier they came for.
      Files: `ui/src/features/jobs/components/TaskNode.tsx`.
      Note (W1-γ): `TaskNode` now renders the task label as the primary
      truncated/title line and moves the image to secondary metadata.
- [x] C2. Handle trivial and disconnected graphs: tighten zoom-to-fit (or render
      a compact card) for single-node DAGs so one node no longer floats in an
      empty canvas, and hide the input/output `Handle`s on nodes with no incident
      edges (pass edge-degree into the node and render handles conditionally).
      Files: `ui/src/features/jobs/JobDAG.tsx`,
      `ui/src/features/jobs/components/TaskNode.tsx`,
      `ui/src/features/jobs/components/BranchNode.tsx`.
      Depends on: C1.
      Note (W1-γ): `JobDAG` passes incoming/outgoing/total edge degree into DAG
      nodes, isolated nodes hide handles, and single-node DAGs use tighter
      `fitViewOptions`.
- [ ] C3. (Deferred 2026-07-07 — the operator chose to keep the on-canvas watermark; no React Flow Pro subscription / OSS exception is confirmed. C1/C2 shipped without it.) Hide the React Flow attribution watermark on every DAG
      (`proOptions={{ hideAttribution: true }}` on the `ReactFlow` element) and
      drop the now-dead `.react-flow__attribution` styling.
      **Licensing gate**: xyflow's terms ask that you either keep the "React
      Flow" attribution or hold a Pro subscription / approved OSS exception to
      remove it (the library is MIT, but `hideAttribution` is governed by their
      ToS). Before shipping this item, confirm the project's stance; if a
      subscription/exception isn't in place, the fallback is to keep a
      lightweight "Powered by React Flow" credit in an About/footer surface
      rather than the on-canvas watermark, which still de-clutters the DAG.
      Files: `ui/src/features/jobs/JobDAG.tsx`, `ui/src/index.css`.
      Note (W1-γ): implemented on-canvas attribution hiding; orchestrator still
      needs to confirm the Pro/OSS exception stance or add the fallback footer
      credit before publication.

### Stream D — Task log drawer

The docked log drawer is a real strength — colorized, retained, filterable — but
it is starved for space: it wraps structured lines mid-word
("work\ner=demo-node", "throughput\n_rps=172"), truncates at the right edge, and
overlaps the DAG node it describes. Its state toggles (`Ready` / `Retained`) are
ambiguous. This stream gives the drawer room and makes its state legible.

- [ ] D1. Give the log drawer room to breathe: default it wider / to full height,
      stop it occluding the DAG node it describes (offset or dim the canvas
      rather than overlapping the selected node), and stop breaking structured
      log lines mid-word.
      **Implementation constraint**: `LogViewer.tsx` renders via `xterm.js`
      (`Terminal` + `FitAddon`, `convertEol: true`), a terminal emulator that
      hard-wraps to its column width and has no native horizontal scroll. The
      widen-and-offset work is straightforward, but true no-wrap /
      horizontal-scroll for long structured lines requires a design decision by
      the stream owner: either (a) render structured logs through a scrollable
      text/HTML viewer (e.g. a `<pre>` with `overflow-x:auto`,
      `white-space:pre`) instead of xterm, or (b) pin a fixed wide terminal
      column inside a horizontally scrollable container — which fights the
      `FitAddon` auto-resize and is the more brittle path. Prefer (a) for the
      structured-log case; keep xterm for free-form/colorized streaming.
      Files: `ui/src/features/jobs/TaskDetailPanel.tsx`,
      `ui/src/features/jobs/LogViewer.tsx`.
- [ ] D2. Disambiguate the log-state labels (`Ready` / `Retained` / `Live` /
      `Truncated`) with clearer wording and tooltips explaining what each state
      means (e.g. "Retained → persisted logs from a finished task").
      Files: `ui/src/features/jobs/LogViewer.tsx`.
      Depends on: D1.

## Sequencing & Dependencies

**Cross-stream order.** Streams A, B, C, and D are file-partitioned and run in
parallel — no two streams write the same file. The only cross-stream edges are
soft:

- **B2 depends on A1**: B2's "land on live progress" only pays off once A1 has
  reordered the run page to lead with the live DAG + logs. B2 and A1 touch
  different files, so they can be built in the same wave; if both land in one
  wave, merge A1 before B2 so the landing view exists.
- No other cross-stream dependency exists. C and D are fully independent of A
  and B and of each other.

**Within-stream order.**

- A: `A1 → A3` (A3 reconciles the reordered run page). A2 and A4 are independent
  and can run alongside A1.
- B: `B1 → B6` (B6's route migration builds on B1's tab/action split) and
  `B4 → B5` (B5 wires the cancel mutation the B4 row surfaces). B2 depends on A1
  (cross-stream). B3 is independent. All B items serialize on
  `JobDetailPage.tsx` (see file conflicts) — one agent should own the stream
  across a wave.
- C: `C1 → C2` (both edit `TaskNode.tsx`). C3 is independent.
- D: `D1 → D2` (both edit `LogViewer.tsx`).

**Cross-stream file conflicts.** None across streams — the partition is clean.
The conflict-prone files are all *within* a single stream and must be serialized
by that stream's owning agent, not split across a wave:

- `ui/src/features/jobs/JobDetailPage.tsx` — B1, B2, B3, B4, B5, B6 all edit it;
  Stream B is one agent's stream, items land sequentially.
- `ui/src/features/jobs/RunDetailPage.tsx` — A1, A3.
- `ui/src/features/jobs/components/TaskNode.tsx` — C1, C2.
- `ui/src/features/jobs/LogViewer.tsx` — D1, D2.
- `ui/src/router.tsx` (B6 only) and `ui/src/lib/api.ts` (B5 only) — single-item
  touches, no conflict. No stream adds a Go/JS dependency, so there is no
  `go.sum` / `package-lock.json` conflict to resolve.
- B5's backend surface (`api/rest/controller/job/queue/`, `api/rest/service/job/`,
  `internal/auth/rbac.go`, `api/auth_rbac_policy_completeness_test.go`, the
  run-queue store, an additive route in `api/rest/bind/bind.go`) is touched by no
  other stream, so it does not cross-conflict — but it is the plan's only Go
  change and its own review surface. A wave may split B5 off as a backend-owned
  sub-task while the rest of Stream B proceeds on the frontend; keep the
  `api.ts` + `JobDetailPage.tsx` wiring with Stream B to avoid splitting those
  files across owners.

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Every stream in this plan touches `ui/**`, so **every PR additionally runs the
UI chain** and asserts its behavior through the real surface:

```sh
just ui-lint           # eslint + tsc
just ui-test           # vitest component/unit (JobDAG.test.tsx, TaskNode.test.tsx, TaskDetailPanel.test.tsx, …)
just ui-e2e            # Playwright against a live backend (ui/e2e/*.spec.ts)
```

Per-stream / per-item gates:

- **A1/A2** — extend `ui/e2e/operator-flow.spec.ts` (or `receipt-verify.spec.ts`)
  to assert the execution timeline + DAG render above the receipt and that a
  `SUCCEEDED` run shows no red "degraded/unverifiable" badge.
- **B1** — a component/e2e assertion that `Pause` is reachable (via the overflow
  menu) at a ≤1150px viewport and that icon controls expose accessible names.
- **B3** — a `DagCounters` unit/component test asserting a run with a failed task
  renders a "failed" count.
- **B5** — a `test/` integration scenario (`//go:build integration`, driven
  through the live server) that (a) enqueues then cancels an **unclaimed** row
  and asserts the row is gone **and no `JobRun` was created**, and (b) exercises
  the **claimed/dequeuer race** so cancel returns **409** and the run proceeds;
  plus an explicit `internal/auth/rbac.go` policy entry (so
  `TestAuthMiddlewareMountedRoutesHaveRBACPolicy` passes) and scoped-key
  allow/deny/audit coverage. A new `api/rest` endpoint with no `test/` scenario,
  or with no RBAC policy entry, blocks review.
- **C1/C2/C3** — `TaskNode.test.tsx` / `JobDAG.test.tsx` assertions: full task
  name is the primary line, single-node graphs fit without dangling handles, and
  no `.react-flow__attribution` node is present.
- **D1/D2** — `TaskDetailPanel.test.tsx` assertions that the drawer does not
  overlap the selected node and that state labels carry disambiguating tooltips.
- This plan's checkbox ticked, per-stream `## Progress` bullet appended for the
  active wave, and `docs/roadmap.md` refreshed in the same PR.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream A — run page leads with reality**: `RunDetailPage` opens with status
   + execution timeline + DAG + logs, the reproducibility receipt is collapsed
   below the fold, a `SUCCEEDED` run shows no red "degraded / unverifiable /
   digest_pinned=false" alarm styling, and the task rows render in execution
   order — all asserted by a Playwright e2e in `ui/e2e/` driving a real run.
2. **Stream B — job-detail page is actionable and honest**: `Pause` is reachable
   at ≤1150px via an overflow menu with accessible labels, Trigger opens a
   param+confirm dialog and lands on the live-progress view, the DAG overlay
   counters surface failed/blocked tasks, queue rows explain why-pending with
   age + inspect, and — if built — the queue-cancel endpoint carries an explicit
   Operator RBAC policy entry and is green in a `test/` integration scenario
   asserting both a clean unclaimed cancel (row gone, no `JobRun` created) and a
   409 on the claimed/dequeuer race; the secondary views are deep-linkable
   routes.
3. **Stream C — DAG nodes read at a glance**: the node's primary line is the full
   task name (ellipsis + tooltip), single-node DAGs fit tightly with no dangling
   handles, and no React Flow watermark renders — asserted by
   `TaskNode.test.tsx` / `JobDAG.test.tsx`.
4. **Stream D — the log drawer has room and legible state**: the drawer no longer
   occludes its node, structured lines scroll horizontally instead of wrapping
   mid-word, and the state labels are disambiguated — asserted by
   `TaskDetailPanel.test.tsx`.
5. **Cross-cutting**: `docs/roadmap.md` (`§2.4 UI Refresh`, and `§3.4` where the
   DAG-debugging surface overlaps) notes this operator-loop refinement, this
   plan's per-stream `## Progress` entries match merged PRs, and every shipped
   stream is green under `just ui-lint && just ui-test && just ui-e2e`.

## How To Pick Up Work

1. Read this file end-to-end so you understand the streams, their
   interdependencies, and which acceptance criterion the item closes.
2. Pick an unchecked item under `## Streams` whose `Depends on:` line
   is satisfied (consult `## Sequencing & Dependencies`).
3. Branch from `master` (or land in a worktree if dispatched by
   `exec-plan-wave`); do the work as a self-contained PR.
4. Run the verification block under `## Verification (Run For Every
   PR)`.
5. Tick the checkbox for your item, add a per-stream bullet to the
   active wave subsection in `## Progress` (or open a new wave
   subsection if none exists yet), and update the cross-linked
   `docs/roadmap.md` section in the same PR.
6. Open the PR with title format
   `<Imperative subject> (<plan-slug> <wave>-<stream>)` —
   e.g. `Lead run page with status and timeline (console-operator-loop-ux W1-α)`.
   GitHub appends `(#NNN)` on squash-merge.

## Cross-References

- [`docs/roadmap.md`](../../roadmap.md) — `§2.4 UI Refresh (Caesium Console v2)`
  and `§3.4 Live DAG Debugging & Run Diff`; this plan is the operator-loop
  refinement follow-up.
- [`docs/exec-plans/completed/data-plane-memory-ui.md`](../completed/data-plane-memory-ui.md)
  — owns the receipt / `why` / `blame` / run-diff / replay affordances this plan
  relocates and re-weights.
- [`docs/README.md`](../../README.md) — active records index.
- Surfaces touched: `ui/src/features/jobs/RunDetailPage.tsx`,
  `ui/src/features/jobs/JobDetailPage.tsx`,
  `ui/src/features/jobs/ReceiptPanel.tsx`,
  `ui/src/features/jobs/JobDAG.tsx`,
  `ui/src/features/jobs/components/TaskNode.tsx`,
  `ui/src/features/jobs/TaskDetailPanel.tsx`,
  `ui/src/features/jobs/LogViewer.tsx`, `ui/src/router.tsx`.
