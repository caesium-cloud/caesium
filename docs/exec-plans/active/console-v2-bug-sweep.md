# Caesium Console v2 — Bug Sweep & Hardening

Last updated: 2026-07-02

This plan fixes a batch of real defects found during a hands-on interactive
walkthrough of the shipped **Caesium Console v2** web UI (roadmap §2.4). None
of them are new features — every item is a correctness or accuracy fix to a
page that already renders. The headline defects are user-visible and
embarrassing: **every valid cron trigger is labelled "Invalid cron"**, the
**Live Activity feed double-lists events and shows contradictory
completed+failed pairs**, the **JobDefs linter reports "No steps" for
multi-step manifests**, and **task commands render raw JSON unicode escapes
(`>`)**. A long tail of smaller inaccuracies (dropped-failure DAG counts,
image-labelled timeline rows, unlabelled local vs. UTC times, redundant
duplicate API fetches, an empty HISTORY column, unsurfaced failed callbacks)
rounds out the sweep.

The work is almost entirely in `ui/src/`. Where a fix needs data the API does
not yet return (the HISTORY sparkline needs a `last_runs` array; the lint bar
needs a real step count) the item carries the matching backend change and
ships with a `test/` integration scenario per the CLAUDE.md end-to-end gate.
Several defects that *looked* like they needed backend work do **not**: the
run payload already serializes `callbacks` (`internal/run/store.go:160`), and
atom commands are already retrievable as data — so those fixes are
frontend-only.

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

This is a bug-fix plan against a shipped UI, so the **running backend wins**:
when this plan and the actual `/v1` REST responses (the controllers under
`api/rest/controller/**` and the run serializers in `internal/run/store.go`)
disagree about the shape of the data the UI receives, the API is correct and
the plan's assumption is the bug. For the one item that reconstructs an
authoring manifest (C3), the job-definition schema wins: when the
reconstructed YAML and `pkg/jobdef/definition.go` / `pkg/jobdef/schema.go`
disagree about field names or structure, the schema is authoritative. There is
no sibling exec-plan that owns any of this scope — the Console v2 build shipped
via [`docs/exec-plans/completed/data-plane-memory-ui.md`](../completed/data-plane-memory-ui.md)
and the archived `ui_implementation_plan.md`, both closed.

## Progress (as of 2026-07-02)

No implementation waves have shipped yet. The plan was published from the
interactive UI walkthrough that enumerated the defects; every root cause below
was verified against the current source on `master`. The first wave is the
next eligible run of the `exec-plan-wave` skill against this doc.

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Triggers page — cron validation + event-trigger rendering | **P0** | Not started |
| B | Jobs list, Live Activity feed & data-fetching | **P0** | Not started |
| C | Job detail view — DAG counts, command decode, clean-manifest YAML | P1 | Not started |
| D | Run detail & execution timeline — ordering, labels, callbacks | P1 | Not started |
| E | JobDefs lint accuracy — step summary | P1 | Not started |
| F | Shell & display consistency — nodes card, 404, timestamps, short-id | P2 | Not started |

## Streams

### Stream A — Triggers page correctness

The Triggers page (`ui/src/features/triggers/TriggersPage.tsx`) mis-handles the
trigger `configuration` blob in two visible ways. Both fixes are frontend-only:
the `/v1/triggers` payload is correct — each trigger is `{id, alias, type,
configuration}` where `configuration` is a JSON **string** the UI must parse
(`internal/models/trigger.go`).

- [ ] A1. Fix the cron validator so valid 5-field POSIX expressions render
      their next-fire time instead of "Invalid cron" — the `NextFire`
      component's `try` around `cronParser.parse(expression, …)` throws and
      falls through to the "Invalid cron" span for **every** expression.
      **Reproduce first, then diagnose — do not assume the cause.** A prior
      guess that the `cron-parser` v5 default-import API was wrong was
      *incorrect*: v5.5.0's default export **is** `CronExpressionParser` and
      `cronParser.parse(expr, { tz })` / `.next().toDate()` is the correct,
      working v5 API — swapping to a named import would be a no-op. Verified
      candidate causes to check against a real trigger's `configuration` JSON:
      (a) the `{ tz: timezone }` option throwing in the browser bundle when
      the IANA timezone data isn't available/resolvable at runtime (a
      whole-page failure mode, since every seeded trigger carries a timezone);
      (b) the expression extraction at `TriggersPage.tsx:391`
      `const expr = (config.expression || config.cron || trigger.configuration)`
      falling through to the **raw JSON config string** when the cron lives
      under a key the UI doesn't read — the backend's cron parser accepts
      `expression`, `cron`, **and `schedule`** (`internal/trigger/cron/cron.go`
      ~257), so a trigger stored under `schedule` yields `expr =
      '{"schedule":"…"}'` and fails to parse. Fix whichever reproduces, and
      add a unit test that runs the real fixtures (`0 * * * *`, `*/15 * * * *`,
      `0 */6 * * *`, `0 2 * * *`, with and without a timezone) and asserts a
      next-fire date, not null. Files:
      `ui/src/features/triggers/TriggersPage.tsx` (`NextFire` ~150-172, the
      cron-parser import at line 19, expr extraction ~391), new
      `ui/src/features/triggers/__tests__/TriggersPage.test.tsx` (or a
      cron-util unit test).
- [ ] A2. Render `event` triggers (and any non-cron / non-http type) as a
      human-readable summary in the schedule column instead of dumping the raw
      configuration JSON (`{"defaultParams":{"chain_name":"nightly-…`
      truncated). Add an explicit `type === "event"` branch that parses the
      config and shows a concise descriptor (e.g. the event type(s) / source),
      mirroring how `cron` and `http` are already special-cased. Factor a
      small `describeTrigger(trigger)` helper so the desktop and mobile
      renderers share one code path. Files:
      `ui/src/features/triggers/TriggersPage.tsx` (schedule cell ~421-435
      desktop, ~491-504 mobile), optional new helper in
      `ui/src/features/triggers/` + unit test.

### Stream B — Jobs list, Live Activity feed & data-fetching

The Jobs page is the app's landing view and carries the most visible glitches.
The Live Activity feed duplicates and contradicts itself; the page re-fetches
`/v1/jobs` four times per load; the command palette over-matches; and the
HISTORY column never renders. All four items are independent.

- [ ] B1. Fix the Live Activity feed. Two symptoms, one root cause plus an
      alias miss: (a) **duplicates + contradiction** — every terminal run
      emits both a specific `run_completed`/`run_failed` event **and** a
      separate `run_terminal` event (`internal/run/store.go` ~3395-3417), and
      `useJobsView` subscribes `onRunEvent` to *all* of them while
      `JobsPage.tsx` maps `run_terminal → "Run completed"` and colors it green
      (~335/342). So a successful run shows "Run completed" **twice**, and a
      failed run shows a contradictory red "Run failed" + green "Run completed"
      pair. Fix by dropping `run_terminal` from the activity subscription (or
      deduping entries by run id and keeping the specific terminal status /
      correcting the `run_terminal` label). (b) **raw UUID** — "run started"
      entries print the job UUID because the started payload lacks `job_alias`
      and the jobs cache isn't populated yet (`useJobsView.ts` ~109-112
      fallback to `jobID`); resolve the alias tolerant of a cache miss (or
      defer the row until it resolves). While here, remove the dead
      `onmessage` handler in `events.ts` (~66-73) — named SSE events only
      dispatch to `addEventListener`, and the server emits only named events
      (`api/rest/controller/event/stream.go` ~238), so `onmessage` never fires;
      keep the `events.test.ts` simulation honest (it currently drives delivery
      through `onmessage`). Files: `ui/src/features/jobs/useJobsView.ts`
      (~109-124, subscription ~156), `ui/src/features/jobs/JobsPage.tsx`
      (activity feed label/color map ~335-342), `ui/src/lib/events.ts` (~66-73),
      `ui/src/lib/__tests__/events.test.ts`.
- [ ] B2. Eliminate redundant duplicate fetches (one page load fires
      `/v1/jobs` ×4 and `/v1/triggers`, `/v1/atoms`, `/health` ×2). Set a
      sensible global react-query default (`staleTime` > 0 and/or
      `refetchOnMount: false`) on the `QueryClient`, and make the nav-count
      hooks reuse the canonical query keys (`["jobs"]`, `["triggers"]`,
      `["atoms"]`) instead of the parallel `["nav-counts", …]` keys, so
      simultaneously-mounted components (command palette + jobs view + sidebar
      counts) share one in-flight request. Files: `ui/src/main.tsx`
      (`new QueryClient()`), `ui/src/features/jobs/useNavCounts.ts`,
      `ui/src/features/jobs/useJobsView.ts`, `ui/src/components/command-menu.tsx`.
- [ ] B3. Fix the command palette (`⌘K`) matcher: it uses cmdk's default
      subsequence scoring, so "cron" matches "pro**c**ess-p**r**oducti**o**n"
      via scattered letters. Supply a stricter custom `filter` (word / substring
      match against alias + short-id) and index `triggers` and `atoms` as their
      own search groups, not just jobs. Files:
      `ui/src/components/command-menu.tsx`, `ui/src/components/ui/command.tsx`
      (if the custom filter mounts there).
- [ ] B4. Populate the HISTORY sparkline column, which is `—` for every job.
      The sparkline reads `job.lastRuns` (from a `last_runs` field) but
      `/v1/jobs` only returns a single `latest_run`, so the array is always
      empty. Extend the list controller/service to return a bounded
      `last_runs` status array (e.g. last 10 run statuses) per job, add the
      field to the UI `Job` type, and render it. Ships with a `test/`
      integration scenario asserting `/v1/jobs` includes `last_runs`. Files:
      `api/rest/controller/job/list.go` (+ `api/rest/service/job/`),
      `ui/src/lib/api.ts` (`Job.last_runs`),
      `ui/src/features/jobs/useJobsView.ts` (~221),
      `ui/src/features/jobs/JobsPage.tsx` (~188 Sparkline), new
      `test/*_test.go` scenario.

### Stream C — Job detail view

Three independent fixes on the job detail page
(`ui/src/features/jobs/JobDetailPage.tsx`). All frontend-only — the data is
already present in the responses the page fetches.

- [ ] C1. Include failed tasks in the DAG overlay counts. A failed run's header
      reads "0/3 done · 2 queued" — the failed task appears in neither number.
      `done` counts `succeeded|completed` and `queued` counts
      `pending|queued`; add a `failed` count (and any `running`) so the tallies
      reconcile to the total, and surface the failure (e.g. "1 failed"). Files:
      `ui/src/features/jobs/JobDetailPage.tsx` (~521-532).
- [ ] C2. Decode task commands so JSON unicode escapes stop rendering
      literally (`echo … > /out/files.json`). The atom's `command` field is
      a JSON-encoded **string** (`internal/models/atom.go` — `Command string`,
      unmarshalled by `Atom.Cmd()`; `ui/src/lib/api.ts:111` types it `string`),
      and `JobDetailPage` reads `atoms[atom_id].command` raw through
      `formatCommand`, so the `>` etc. show as escapes. Normalize the value —
      `JSON.parse` the array-string when it is one (which decodes the escapes),
      then join for display — in `JobDetailPage`'s `formatCommand` helper.
      (BlameView has its own `formatCommand`, but its command comes from the
      blame API already as a decoded `string[]` — no escape bug there, leave it
      alone.) Files: `ui/src/features/jobs/JobDetailPage.tsx` (`formatCommand`
      ~799, use ~647).
- [ ] C3. Render the "YAML" tab as a clean authoring manifest instead of
      `yamlStringify(job)`, which dumps `latest_run` runtime state, internal
      UUIDs, empty provenance, and the double-encoded `configuration` string.
      Reconstruct an `apiVersion: v1` / `kind: Job` / `metadata` / `trigger` /
      `steps` document from the job + tasks + trigger + atoms data the page
      already loads, and YAML-stringify that. Keep field names aligned with
      `pkg/jobdef/definition.go` (the source of truth). Files:
      `ui/src/features/jobs/JobDetailPage.tsx` (~495), new manifest-builder
      helper under `ui/src/features/jobs/` + unit test.
      (Alternative, deferred: a backend `GET /v1/jobs/:id/manifest` export
      endpoint — recorded in Rejected below.)

#### Rejected / deferred for Stream C

- A backend `GET /v1/jobs/:id/manifest` (or `/export`) endpoint that
  reconstructs the manifest server-side from `pkg/jobdef` is the more reusable
  long-term fix (the CLI could share it), but it is deferred: the UI already
  has every field it needs to reconstruct client-side (C3), and a new endpoint
  widens scope without unblocking anything in this sweep.

### Stream D — Run detail & execution timeline

Fixes on the run detail page and its timeline
(`ui/src/features/jobs/RunDetailPage.tsx`, `RunTimeline.tsx`,
`ui/src/lib/status.ts`). All frontend-only.

- [ ] D1. Order run-detail tasks by DAG/topological order (e.g. list → convert
      → publish) rather than in the unspecified DB/payload order they arrive
      in, which currently lists "convert" before "list". Tasks are rendered
      unsorted via `<RunTimeline tasks={run.tasks} />`
      (`RunDetailPage.tsx` ~431) and `RunTimeline` maps them in array order —
      there is **no** existing task sort (the `.sort` at `RunDetailPage.tsx:205`
      is the unrelated run-comparison picker). Impose DAG order in `RunTimeline`
      (or sort before passing them in). Note `run.tasks` are `TaskRun[]` which
      carry **no `next_id`** — the DAG edges live on the `JobTask` definitions
      (`taskDefinitions` keyed by `task_id`, built at
      `RunDetailPage.tsx:186-192`), so the fix must **thread `taskDefinitions`
      into `RunTimeline`**, then topo-sort by `next_id`, falling back to
      `started_at`. Files: `ui/src/features/jobs/RunDetailPage.tsx` (~431 the
      `RunTimeline` call + ~186-192 the task defs), `ui/src/features/jobs/RunTimeline.tsx`.
- [ ] D2. Fix execution-timeline labels and legend: (a) rows are labelled by
      the container image name (the walkthrough saw steps show their base
      image name rather than the step name) — label by **task name** instead,
      falling
      back to image/short-id only when the name is absent; note `TaskRun` has
      no `name` field, so this reuses the same `JobTask`-definition threading
      as D1 (task name is `JobTask.name` keyed by `task_id`). (b) the legend's
      "skipped" and "pending" (a relabel of `queued`) swatches are
      near-identical grays — give them visually distinct colors. Files:
      `ui/src/features/jobs/RunTimeline.tsx` (row label ~103-104, legend relabel
      ~210), `ui/src/lib/status.ts` (`skipped` / `queued` `META` entries
      ~57-82). Depends on: D1 (shares the `JobTask`-defs threading into
      `RunTimeline`).
- [ ] D3. Surface a callbacks section on the run detail so failed callbacks
      ("webhook responded 404: no_team") are visible. The run payload
      **already** includes them — `internal/run/store.go:160` serializes
      `callbacks: []CallbackRun{id, callback_id, status, error, started_at,
      completed_at}` and loads them per run — but the UI `JobRun` type omits
      the field and no component renders it. Add `callbacks` to the UI
      `JobRun`/type, and render a section that lists callback runs and
      highlights failures with their `error`. Frontend-only. Files:
      `ui/src/lib/api.ts` (`JobRun.callbacks` + `CallbackRun` type),
      `ui/src/features/jobs/RunDetailPage.tsx` (new callbacks section).

#### Rejected / deferred for Stream D

- Enriching `CallbackRun` with `http_status` / `response_body` /
  `retry_count` (the model stores only a flat `error` string today) would give
  operators more to debug, but it is a backend model + migration change out of
  scope for a UI bug sweep. Deferred; D3 surfaces the existing `error`.

### Stream E — JobDefs lint accuracy

- [ ] E1. Make the JobDefs live-lint status bar report an accurate step
      summary. It currently reads "Schema valid · No steps" even for a
      two-step manifest, because the frontend renders
      `lintResult.summary?.steps || "No steps"` and the backend's
      `summary.steps` is a **data-contract** string (producer→consumer edges)
      that is empty whenever no `inputSchema` is declared — it was never a step
      count. Fix the backend lint summary to report the real step count (the
      `contractSummary` code already has `allSteps` in hand) — e.g. "2 steps"
      or "2 steps · 1 contract" — and update the frontend label so an empty
      summary no longer reads as "No steps". (No need to add a stepless-invalid
      check: `Definition.Validate()` already errors on zero steps
      — `pkg/jobdef/definition.go` ~438 "steps must contain at least one
      entry" — which `lint.go` surfaces as a validation error, and the summary
      is only computed when there are no errors, so a stepless def never renders
      "Schema valid".) Ships with a `test/` integration scenario hitting
      `/jobdefs/lint` with a multi-step manifest and asserting the summary.
      Files:
      `api/rest/controller/jobdef/lint.go` (`LintSummary` struct ~24,
      `contractSummary` / summary assembly ~64-124),
      `ui/src/lib/api.ts` (`LintSummary`),
      `ui/src/features/jobdefs/JobDefsPage.tsx` (~304, ~325-329), new
      `test/*_test.go` scenario.

### Stream F — Shell & display consistency

Lower-priority polish spanning the app shell. F1/F2/F4 are independent; F3 is a
cross-cutting timestamp sweep that shares files with Streams C and D, so it is
sequenced after them (see `## Sequencing & Dependencies`).

- [ ] F1. Fix the System page NODES card, which is labelled "Active workers"
      but displays a **node** count while the node row below shows WORKERS
      "0/4" (zero active). Either relabel the card sub-text to "Nodes" /
      "Tracked nodes", or add a real Workers KPI that sums `workers_busy` /
      `workers_total` across nodes. Files:
      `ui/src/features/system/SystemPage.tsx` (~138 `SysKpi`).
- [ ] F2. Unify the app's not-found states (issue #17 is "two inconsistent
      404s"). (a) Add a catch-all `NotFoundComponent` (or `*` route) so unknown
      SPA paths render a coherent 404 instead of a bare/empty fallback. (b)
      Normalize the two existing per-resource not-found renders, which are
      themselves inconsistent — `JobDetailPage.tsx:306`
      `<div className="p-8">Job not found</div>` vs `RunDetailPage.tsx:235`
      `<div className="p-8 text-text-3">Run not found</div>` (different text
      color) — into one shared styled not-found component so all three states
      match. Files: `ui/src/router.tsx` (~123-139 route tree),
      `ui/src/features/jobs/JobDetailPage.tsx` (~306),
      `ui/src/features/jobs/RunDetailPage.tsx` (~235), new shared not-found
      component under `ui/src/components/`. (Note: `/api/*` paths hitting the
      SPA is expected — the real API prefix is `/v1`, which `api/ui.go` already
      excludes from the SPA fallback; no backend change needed.)
- [ ] F3. Make wall-clock timestamps consistently labelled. Bare
      `toLocaleString()` renders unlabelled **local** time (e.g. "Jul 2, 2026,
      10:00 AM") right next to the UTC header clock, mixing zones without
      labels. Add one shared timestamp helper/component that emits a
      consistently-labelled full **date + time** in UTC (build it from
      `getUTC*` fields — `utc-clock.tsx`'s `formatUTC` is a pattern to mirror,
      not import: it is unexported and emits only `HH:MM:SS`, no date), and
      replace the bare `toLocaleString()` call sites. Files:
      `ui/src/lib/utils.ts` (+ optional
      `ui/src/components/` timestamp component),
      `ui/src/features/jobs/RunDetailPage.tsx` (~500),
      `ui/src/features/jobs/JobDetailPage.tsx` (~601),
      `ui/src/features/jobs/{BackfillsView,TaskDetailPanel,TaskMetadataPanel,RunDiffView,TaskWhyView,LineageGraph}.tsx`,
      `ui/src/features/jobs/components/TaskNode.tsx`,
      `ui/src/features/stats/components/TrendChart.tsx`.
      Depends on: C, D (shares `JobDetailPage.tsx` / `RunDetailPage.tsx`).
- [ ] F4. Resolve short-id deep links. The list displays an 8-char id but
      `GET /v1/jobs/:id` calls `uuid.Parse` (`api/rest/controller/job/get.go`
      ~24) and 400s on anything that isn't a full UUID, so a hand-typed/shared
      `/jobs/15bbde04` never resolves (row clicks already carry the full id).
      Prefer the backend fix: accept an unambiguous id **prefix**
      (`WHERE id LIKE 'prefix%'`, error on ambiguity) in the job get
      service, with a `test/` integration scenario. Alternatively, a
      frontend-only router resolver that expands a short id via the cached
      jobs list. Files: `api/rest/controller/job/get.go` +
      `api/rest/service/job/job.go` (`Get`), or `ui/src/router.tsx` +
      `ui/src/lib/api.ts`; `test/*_test.go` if backend.

## Harness Strengthening

- [ ] H-1. Seed a richer e2e/integration fixture set so the assertions the
      above items need have real data to render against: a job with a
      **multi-step DAG** that produces a **failed run** carrying a **failed
      callback**, plus an **event trigger** and one or more **cron triggers**.
      This backs the callbacks section (D3), event-trigger rendering (A2),
      DAG-count reconciliation (C1), Live Activity dedup (B1), and the cron
      validator (A1). Files: `ui/e2e/helpers/fixtures.ts`, new
      `ui/e2e/` fixture `*.job.yaml`, and any `test/` harness seed needed for
      the integration scenarios (B4, E1, F4).

## Navigational / Organizational Improvements

- [ ] N-1. Cross-link this plan. Add a short "Console v2 hardening" note under
      `docs/roadmap.md` §2.4 (the shipped UI-refresh section) pointing at this
      plan, and add an active-records bullet in `docs/README.md`. Land this as
      the final doc-sync item once the runtime streams have merged. Files:
      `docs/roadmap.md` (§2.4), `docs/README.md`.

## Sequencing & Dependencies

**Cross-stream order.**

- Streams **A, B, C, D, E are independent** and can run in parallel in the
  first wave — they own disjoint feature directories
  (`triggers/`, jobs-list files, `JobDetailPage.tsx`,
  `RunDetailPage.tsx`+`RunTimeline.tsx`, `jobdefs/`+`lint.go`).
- **Stream F** splits: F1 (system), F2 (router), F4 (job get) are independent
  and may run in the first wave; **F3 depends on C and D** because the
  timestamp sweep edits `JobDetailPage.tsx` (Stream C) and `RunDetailPage.tsx`
  (Stream D). Run F3 in a wave **after** C and D land, or the orchestrator will
  hit shared-file conflicts.
- **H-1** should land early (first wave) or alongside so B1/A2/C1/D3/B4/E1 have
  fixtures to assert against.
- **N-1** is last — the doc-sync closes the plan after the runtime streams
  merge.

**Within-stream order.**

- Stream B: all four items (B1–B4) are independent.
- Stream D: **D1 → D2** (both thread the `JobTask` definitions into
  `RunTimeline`; do the plumbing once in D1, then D2 uses it for task-name
  labels). D3 is independent.
- All other streams' items are mutually independent within the stream.

**Cross-stream file conflicts.**

- `ui/src/lib/api.ts` is touched by **B4** (`Job.last_runs`), **D3**
  (`JobRun.callbacks` + `CallbackRun` type), **E1** (`LintSummary`), and
  **F4** (short-id resolver). These are additive type/method edits in
  different blocks; they rebase mechanically, but if two land in the same wave
  expect a trivial import/interface-block conflict — sequence or resolve by
  hand, no `go`-level coordination needed.
- `ui/src/features/jobs/JobDetailPage.tsx` — Stream **C** (C1/C2/C3) and Stream
  **F** (F3, ~601). Sequence **C → F3**.
- `ui/src/features/jobs/RunDetailPage.tsx` — Stream **D** (D1/D3) and Stream
  **F** (F3, ~500). Sequence **D → F3**.
- `ui/src/router.tsx` — **F2** (404 route) and **F4** (short-id resolver, if
  the frontend variant is chosen) both live in Stream F; no cross-stream
  conflict.
- No two streams edit the same Go file: `list.go` (B4), `lint.go` (E1),
  `get.go`/service (F4) are disjoint. **No `api/rest/bind/bind.go` change is
  required** — every backend item reuses an existing route
  (`/jobs`, `/jobdefs/lint`, `/jobs/:id`).
- **No `go.mod`/`go.sum` changes** are expected (cron-parser, yaml, and cmdk
  are already dependencies) and **no `pkg/jobdef/definition.go` edit** — C3
  reads the schema, it does not change it.

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Because nearly every item changes `ui/**`, the UI gates are mandatory for those
PRs:

```sh
just ui-lint           # npm run lint
just ui-test           # vitest unit tests + build:ci
just ui-e2e            # Playwright specs against a real server (ui/e2e/*.spec.ts)
```

Per-item conditional gates:

- **B4, E1, F4** (backend-touching): the `just integration-test` gate must
  exercise the new/changed REST behavior through the live server — assert on
  the HTTP response (`last_runs` present; lint `summary.steps` accurate; short
  id resolves), not on an internal function.
- **A1, A2, B3, C2, C3, D2** ship a **vitest** unit test for the pure logic
  (cron parse, trigger describe, command decode, manifest reconstruction,
  status color map) — these are cheap and deterministic.
- **A1, A2, B1, C1, D3** should extend a `ui/e2e/*.spec.ts` spec (using the
  H-1 fixtures) so the fix is proven through the real rendered surface, not
  just a unit test — the Console v2 defects in this sweep are exactly the kind
  that pass unit tests while the page stays broken.
- Each PR ticks its checkbox, appends its per-stream bullet to the active
  wave in `## Progress`, and (for the last runtime PR) refreshes the N-1
  cross-links in the same or a trailing PR.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream A — Triggers page** renders a next-fire time for every valid cron
   trigger (no spurious "Invalid cron") and shows a human-readable summary for
   `event` triggers instead of raw JSON; a vitest test parses the real cron
   fixtures and a `ui/e2e` spec asserts the Triggers page shows next-fire times.
2. **Stream B — Jobs list & Live Activity** shows each activity event once with
   the job alias (no duplicates, no contradictory completed+failed pair, no raw
   UUIDs), issues a single fetch per endpoint on load, has a stricter multi-collection
   command palette, and renders a populated HISTORY sparkline backed by a
   `/v1/jobs` `last_runs` array proven by a green `test/` integration scenario.
3. **Stream C — Job detail** DAG overlay counts reconcile to the total
   (failures visible), commands display decoded (`>` not `>`), and the
   YAML tab shows a clean `apiVersion/kind/metadata/trigger/steps` manifest;
   covered by vitest tests for the decode + manifest reconstruction.
4. **Stream D — Run detail & timeline** lists tasks in DAG order, labels
   timeline rows by task name with visually distinct skipped/pending colors,
   and surfaces a callbacks section that shows the failed callback's error;
   a `ui/e2e` spec asserts the callbacks section against the H-1 failed-callback
   fixture.
5. **Stream E — JobDefs lint** status bar reports an accurate step count for a
   multi-step manifest (never "No steps" when steps exist) and flags a stepless
   definition invalid; a `test/` integration scenario against `/jobdefs/lint`
   asserts the summary.
6. **Stream F — Shell & display** System card is correctly labelled (or shows a
   real Workers KPI), unknown routes render a coherent 404, wall-clock
   timestamps are consistently labelled next to the UTC clock, and short-id
   deep links resolve (or degrade gracefully); backend-touching F4 is proven by
   an integration scenario if the backend variant ships.
7. **Harness (H-1)** the shared fixture set (multi-step failed run + failed
   callback + event/cron triggers) exists and is consumed by the e2e specs and
   integration scenarios above.
8. **Cross-cutting:** `docs/roadmap.md` §2.4 and `docs/README.md` reference this
   plan (N-1); this plan's per-stream `## Progress` entries match the merged
   PRs; and the full verification chain (`just lint`, `just unit-test`,
   `just integration-test`, plus `just ui-lint`/`ui-test`/`ui-e2e`) is green.

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
   subsection if none exists yet), and update any cross-linked design
   doc / roadmap section in the same PR.
6. Open the PR with title format
   `<Imperative subject> (console-v2-bug-sweep <wave>-<stream>)` —
   e.g. `Fix cron-parser v5 API on Triggers page (console-v2-bug-sweep W1-α)`.
   GitHub appends `(#NNN)` on squash-merge.

## Cross-References

- [`docs/roadmap.md`](../../roadmap.md) §2.4 UI Refresh (Caesium Console v2) —
  the shipped feature this plan hardens.
- [`docs/exec-plans/completed/data-plane-memory-ui.md`](../completed/data-plane-memory-ui.md) —
  the completed plan that surfaced the data-plane verbs in the Console; several
  of the pages touched here (run detail, blame, lineage) shipped there.
- [`docs/README.md`](../../README.md) — active-records index (updated by N-1).
- `pkg/jobdef/definition.go` / `pkg/jobdef/schema.go` — job-definition schema;
  authoritative for the C3 manifest reconstruction.
- `internal/run/store.go` — run serializer; authoritative for the run payload
  shape the run-detail fixes (D1/D3) depend on.
