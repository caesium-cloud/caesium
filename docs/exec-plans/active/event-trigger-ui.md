# Event-Trigger UI — Surface Event Triggers and Their Delivery Log in the Console

Last updated: 2026-11-04

The completed [Event-Driven Trigger Routing](../completed/event-trigger-routing.md)
plan shipped `event` triggers, `POST /v1/events`, trigger chaining, and durable
delivery history **CLI + REST only**, and deferred the UI surface (design
[Phase 4 item 15](../../design/event-triggers.md): "event-trigger visualization in
the DAG view, event log on trigger detail") to this follow-on, the same way
[Data-Plane Memory UI](../completed/data-plane-memory-ui.md) followed the causal
verbs. Roadmap §1.2 is Shipped; this plan closes its remaining console gap.

**Current state** (verified against `ui/` and the backend on 2026-11-04). The
original deferral overstated the gap; part of the surface already exists:

- `ui/src/features/triggers/trigger-utils.ts` already classifies `event` triggers
  (`describeTrigger` returns `kind: "event"`, a summary such as `deployment.* from
  github-actions`, and a detail such as `2 filters, 3 mapped params, 2 default
  params`) and `TriggersPage` renders them, with a `ui/e2e/triggers.spec.ts`
  scenario that applies `docs/examples/event-trigger.job.yaml` against the live
  server. That stays; it is a regression anchor, not work here.
- **Missing:** (1) no client for `GET /v1/triggers/:id/events` in
  `ui/src/lib/api.ts` (no `getTriggerEvents`, no event types); (2) no delivery log
  anywhere: the Triggers page expands a card but shows no history; (3) the job
  detail DAG (`JobDAG.tsx`) renders only task nodes, so a job's trigger (and
  event-trigger wiring) is invisible there; the trigger appears only as text in
  the Config tab (`ConfigurationView` / `renderTriggerSummary`); (4) event triggers
  use the generic `Globe` icon like HTTP; (5) there is **no trigger-detail page or
  route** (`ui/src/router.tsx` has only `triggers`), so "trigger detail" means the
  expanded card on `TriggersPage`.

**Target state:** an operator can see, in the console, which jobs an event
trigger serves, what events it matched or skipped (with the payload, skip reason,
error, and the runs started), and the trigger as the root node of a job's DAG,
each gated by a Playwright e2e that drives the real UI against a live server.

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

## Backend Contract (verified 2026-11-04)

Every UI item is grounded in this contract. Re-verify against the code at pickup;
where code and this section disagree, the code wins and this section gets fixed.

**`GET /v1/triggers/:id/events`** — handler `Events` in
`api/rest/controller/trigger/events.go`, service `ListTriggerEvents` in
`api/rest/service/event/event.go`, route in `api/rest/bind/bind.go`
(`g.GET("/triggers/:id/events", trigger.Events)`).

- Response: a **bare JSON array** (never an envelope; the service returns a
  non-nil slice) of `TriggerEvent`, newest `matched_at` first (ties by match id
  desc): `match_id`, `event_id`, `trigger_id` (uuids); `type` (string); `source`
  (string, omitted when empty); `data` (arbitrary JSON, omitted when empty);
  `created_at` (event ingest time); `matched_at` (delivery time);
  `runs_started` (uuid array, omitted when empty); `skipped` (bool, always
  present); `skip_reason`, `error` (strings, omitted when empty).
  **There is no job id, job alias, or run status** on a row.
- **A row is one delivery to the whole trigger, and outcomes can be mixed.**
  `EventTrigger.FireWithParams` (`internal/trigger/event/event.go`) appends one
  `FireOutcome` per job the trigger serves; `triggerRouteResult` /
  `eventTriggerMatchRow` (`internal/trigger/event/router.go`) fold them into one
  row: `runs_started` is every started run, `skip_reason` / `error` aggregate the
  non-started jobs, and `skipped` is forced `false` once any run started. So a
  shared trigger where one job started and another was paused, held, or errored
  yields a row with a non-empty `runs_started` **and** a non-empty `skip_reason`
  or `error`. `skipped: true` means *nothing* started.
- Query params: `type`, `source` (exact match on the *event's* type/source, not
  the trigger's pattern); `limit` (default 100, **max 500**, silently clamped);
  `offset`; time window `created_after`/`after`/`from`/`start` and
  `created_before`/`before`/`to`/`end` (RFC3339), applied to the **event's**
  `created_at`, not `matched_at`. A malformed value is `400 bad request`.
- **No total count, no next cursor, no `Link` header.** "Has more" can only be
  inferred from `rows.length === limit`. Offset paging over a table that grows
  under the reader can skip or repeat rows.
- A bad uuid is `400`. The handler has no existence check: an id that serves no
  trigger returns `200 []` when auth is off; with auth on, the namespace resolver
  (`resolveTrigger` in `api/middleware/auth_namespace_resolvers.go`) rejects
  unknown ids first.
- Retention: matches and events are pruned after `CAESIUM_EVENT_RETENTION`
  (default 168h, `pkg/env/env.go`), so the log is a rolling week, not history.
- **Auth/RBAC** (`internal/auth/rbac.go`): `GET /v1/triggers/:id/events` is a
  `namespacedRoute(viewer)`, resolved against **every namespace the trigger's
  jobs live in** (a trigger serving no job is cluster-only). A
  namespace-confined viewer additionally gets a **filtered** history
  (`deliveryOwnedSQL` / `provenanceOwnedSQL`): only deliveries that started at
  least one run, with every started run in a readable namespace, and no skip and
  no error. Skipped, failed, and zero-run deliveries are **invisible** to a
  confined caller, lifecycle events (`source: "caesium"`) whose originating run
  is unreadable are hidden, and `job_labels` / data-assertion baseline evidence
  are stripped from lifecycle payloads. So an empty or short log for a confined
  viewer is not "nothing happened", and the UI must say so rather than imply
  completeness. Job-scoped API keys cannot sign in to the console (see
  [Data-Plane Memory UI](../completed/data-plane-memory-ui.md), Wave 3 H-3), so
  scoped behaviour is asserted at the API level only.
- **Liveness:** there is **no SSE/bus event** for event ingestion or trigger
  matches. `internal/event/bus.go` defines run/task/job/dataset/incident types
  only. The console's SSE stream (`GET /v1/events`, `ui/src/lib/events.ts`)
  therefore cannot push new deliveries; the log must poll (see A3, B2).

**Related, already shipped**
- `GET /v1/triggers` (`api/rest/controller/trigger/list.go`): `limit`, `offset`,
  `order_by` (`id|alias|type|created_at|updated_at`), `namespace`. `GET
  /v1/triggers/:id` returns the `models.Trigger` JSON the UI already types as
  `Trigger` (`id, alias, type, configuration` (a JSON **string**), timestamps,
  plus `provenance_*` fields the client type omits). Event configuration is
  `{"events":[{"type","source","filter"}],"paramMapping","defaultParams"}`
  (validated by `validateEventTriggerConfiguration` in
  `pkg/jobdef/definition.go`); `trigger-utils.ts` also tolerates legacy
  `event` / flat shapes.
- **Trigger to jobs reverse lookup already exists:** `GET /v1/jobs?trigger_id=<uuid>`
  (`parseListRequest` in `api/rest/controller/job/list.go`; the service validates
  the uuid and filters `trigger_id = ?` in `api/rest/service/job/job.go`, with
  the usual namespace filtering). The UI client's `api.getJobs` takes only a
  namespace today, so the gap is the typed client, not the backend.
- `GET /v1/events/ingested` (`ListIngested`, same filters, newest first, same
  bare-array / no-cursor shape): the *global* ingested-event feed, not per
  trigger. Not used by this plan.
- Job to trigger is `job.trigger_id`; the UI already fetches it in
  `JobDetailPage.tsx` (`api.getTrigger(job.trigger_id)`). The reverse direction
  is the `trigger_id` job filter above.
- A run row (`JobRun`) carries `trigger_type` and `trigger_alias`
  (`internal/models/run.go`) but **no event id / match id**, so a run cannot be
  linked back to the event that started it today.
- Chained triggers are ordinary event triggers over lifecycle events
  (`source: "caesium"`, `run_completed` etc.) filtered on `job_alias`
  (`docs/design/event-triggers.md`, WS3).
- The run page route is `/jobs/$jobId/runs/$runId` and the run API is
  `GET /v1/jobs/:id/runs/:run_id`: **both need the job id, which
  `runs_started` does not carry** (backend gap B1).

## Source-Of-Truth Note

This plan is **frontend-first**. When this plan and the shipped REST endpoints
(`api/rest/bind/bind.go` and the controllers above) or the completed
[event-trigger-routing](../completed/event-trigger-routing.md) plan disagree, the
endpoints and that plan win. Backend changes are confined to **Stream B** (named,
additive, each independently justified); no UI stream may assume a field or
endpoint that `## Backend Contract` does not list. When this plan and
`docs/design/event-triggers.md` disagree, the design record wins on semantics and
this plan wins on sequencing. Job-definition schema (`pkg/jobdef/definition.go`)
is out of scope: no schema change.

## Progress (as of 2026-11-04)

No implementation waves have shipped yet. The plan resolves the deferred note in
the completed event-trigger-routing plan and GitHub issue #409; the first wave is
the next eligible run of the `exec-plan-wave` skill against this doc. Leaf items
(eligible for Wave 1): H1, B1.

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| H | Typed `getTriggerEvents` client, e2e event-push helper, auth-lane coverage | **P0** | Not started |
| A | Delivery log on the Triggers page (filters, paging, payload, outcome, runs) | **P1** | Not started |
| B | Backend gaps the log needs: run linkage (B1), optional ingest/match bus event (B2) | **P1** | Not started |
| C | Trigger as the root node of a job's DAG; event-aware list affordances | P2 | Not started |
| N | Docs, roadmap, design banner, forward links | — | Not started |

## Streams

### Stream H — Client and test harness

The shared foundation. Nothing else writes event types into `ui/src/lib/api.ts`.

- [ ] H1. Add `TriggerEvent` (fields exactly as in `## Backend Contract`; `data`
      as `unknown`, omitted fields optional) and `TriggerEventsQuery` (`type`,
      `source`, `limit`, `offset`, `created_after`, `created_before`) to
      `ui/src/lib/api.ts`, plus `api.getTriggerEvents(id, query)` calling
      `request<TriggerEvent[]>` on `/triggers/${id}/events?...`. Null-guard the
      body to `[]` defensively. Vitest asserts the query-string encoding, the
      bare-array round trip, omitted-field handling, and a 400.
      Files: `ui/src/lib/api.ts`, `ui/src/lib/__tests__/api.triggerEvents.test.ts`.
- [ ] H2. Add an e2e helper `pushEvent(request, {type, source, data})` that POSTs
      `/v1/events` (202 body: `event_id`, `matched_triggers`, `runs_started`,
      `delivered`, `skipped`, `skipped_namespaces`) and a poll-until helper for
      "delivery visible on `GET /v1/triggers/:id/events`", so specs create real
      deliveries through the real ingest path rather than stubbing the list.
      Confirm the ui-e2e server accepts `POST /v1/events` unauthenticated (the
      default lane has auth off and `CAESIUM_EVENT_INGEST_API_KEY` unset). Reuse
      `docs/examples/event-trigger.job.yaml` via `loadFixtureDefinition`.
      Files: `ui/e2e/helpers/fixtures.ts`.
      Depends on: H1 (types only).
- [ ] H3. Decide and wire the auth lane for A-stream coverage: extend
      `ui/e2e/auth/` (the `just ui-e2e-auth` lane, `loginAtUrl` pattern) with a
      viewer principal, and confirm which policy fixture (if any) provides two
      namespaces so the confined-viewer filtering in `## Backend Contract` can be
      observed in the UI. If the lane cannot express it, record that and rely on
      the API-level scenario that already exists
      (`test/auth_namespace_history_test.go`). Keep `ui-e2e-*` /
      `integration-up-*` env parity if `justfile` changes (a guardrail checks).
      Files: `ui/e2e/auth/*`, `justfile` only if needed.
      Depends on: H2.

### Stream A — Delivery log on the Triggers page

Make the expanded `TriggersPage` card for an `event` trigger show a delivery log
driven by `GET /v1/triggers/:id/events`. HTTP and cron triggers get no panel.
No new route: the card is the "trigger detail".

- [ ] A1. `TriggerEventLog` component mounted in the expanded card when
      `description.kind === "event"`. Columns: matched-at (relative + absolute,
      via the existing `RelativeTime`), event type, source, outcome chip, run
      count. Outcome is derived with an explicit precedence: `partial` (warning
      styling, shows both counts and the reason) when `runs_started` is non-empty
      **and** `skip_reason` or `error` is set; `started` only when runs started
      with neither; `error` when `error` is set and nothing started; `skipped`
      with `skip_reason` when `skipped` is true; else `no run`. Put the
      derivation in a pure, unit-tested helper (`trigger-event-outcome.ts`) with
      a case per branch. Loading, empty, and error states.
      Files: `ui/src/features/triggers/TriggerEventLog.tsx`,
      `ui/src/features/triggers/trigger-event-outcome.ts`,
      `ui/src/features/triggers/TriggersPage.tsx`.
      Depends on: H1.
- [ ] A2. Filters and paging. Type and source inputs (exact match, per the
      contract), a time window (created-after / created-before), and "Load more"
      using `offset` with `limit` 50; "has more" is `rows.length === limit` and
      the component says "end of retained history" otherwise. Clamp the UI limit
      to 500, and de-duplicate by `match_id` because offset paging over a live
      table can repeat rows.
      Files: `ui/src/features/triggers/TriggerEventLog.tsx`.
      Depends on: A1.
- [ ] A3. Liveness by polling, not SSE (none exists). `refetchInterval` of about
      5s while the card is expanded and `document.visibilityState === "visible"`,
      paused when collapsed or hidden. If B2 ships, subscribe to its bus event
      instead and keep a slow poll as the reconcile (the `RunDetailPage`
      pattern).
      Files: `ui/src/features/triggers/TriggerEventLog.tsx`.
      Depends on: A1. (B2 optional.)
- [ ] A4. Row detail. Expanding a row shows the event `data` as a read-only,
      size-capped JSON view (reuse the `pre` treatment from `ConfigurationView`),
      `event_id` / `match_id` with copy buttons, `created_at` vs `matched_at`,
      the full `skip_reason` / `error`, and the `runs_started` ids. Never render
      `data` as HTML.
      Files: `ui/src/features/triggers/TriggerEventLog.tsx`.
      Depends on: A1.
- [ ] A5. Visibility honesty for confined viewers. Using the principal from
      `usePrincipal()` (data-plane-memory-ui H-2), when the caller is not
      cluster-wide show a persistent note: "Showing deliveries that started runs
      you can read. Skipped, failed and cross-namespace deliveries are not
      shown." Do not claim totals; the empty state for such a viewer says no
      *visible* deliveries.
      Files: `ui/src/features/triggers/TriggerEventLog.tsx`.
      Depends on: A1, H3.
- [ ] A6. Run references. Without B1, render run ids as plain monospace chips (no
      link, since the run route needs a job id). With B1, link each chip to
      `/jobs/$jobId/runs/$runId` and show the job alias. Implement the
      plain-chip path first so A ships without B.
      Files: `ui/src/features/triggers/TriggerEventLog.tsx`.
      Depends on: A1; B1 for the linked form.
- [ ] A7. Playwright e2e `ui/e2e/trigger-events.spec.ts`, **live server, no route
      stubbing on the primary path**: apply `event-trigger.job.yaml` (unique alias
      suffix), `pushEvent` one matching event (type `deployment.completed`,
      source `github-actions`, data satisfying the filter) and one non-matching
      event, open `/triggers`, expand the card, assert the matched delivery row
      shows type / source / `started`, the payload JSON contains the pushed
      value, the non-matching event is absent, the type filter narrows, and a
      second push appears without reload (A3).
      Stubbing is allowed only for the failure-state case (a 500 from the events
      call). The mixed-outcome row is not part of this Playwright spec: apply
      cannot produce it (see A7b).
      Files: `ui/e2e/trigger-events.spec.ts`.
      Depends on: A1-A4, H2.
- [ ] A7b. Mixed-outcome delivery, integration test, not Playwright. Apply does
      not share a trigger. `upsertTriggerTx` (`internal/jobdef/importer.go`)
      allocates `uuid.New()` for every new job, and
      `internal/jobdef/trigger_tenancy.go` records that no API path makes two
      jobs share a row. Two applied jobs with identical event configuration
      therefore get two trigger ids and two delivery rows (one started, one
      skipped), never one `partial` row.
      Seed a legacy shared row the way
      `TestAuthJobdefNamespaceDiffBaselineIsConfined` does: apply both jobs,
      then `UPDATE jobs SET trigger_id = ?` so the second job points at the
      first job's trigger (`openIntegrationCatalogDB`). Before pushing the
      event, read both jobs and assert their `trigger_id` values are equal.
      Pause one, push one matching event, and assert a single delivery row
      with a non-empty `runs_started` and a "job paused" reason (`skipped`
      stays false). Pausing one of two separately-applied jobs does not
      satisfy this.
      Files: `test/` (extend the trigger-events integration scenarios).
      Depends on: H2.
- [ ] A8. Auth-lane e2e: a viewer sees the log; a confined viewer sees the A5
      note, asserted against the real API through the browser session.
      API-level assertions for scoped keys (they cannot UI-login).
      Files: `ui/e2e/auth/trigger-events.spec.ts`.
      Depends on: A5, H3.

### Stream B — Backend gaps the UI needs (explicit, not assumed)

None of these exist today. Each is additive and optional for the plan's core
path; the UI streams degrade gracefully without them (see A3, A6). Any B item
that adds or changes an endpoint field ships with a `test/` integration scenario
that drives the real endpoint (CLAUDE.md gate) and, where the CLI prints it, a
split-stream `--json` assertion (`caesium trigger events`).

- [ ] B1. Run linkage on delivery rows. Add additive fields to `TriggerEvent`
      (for example `runs: [{run_id, job_id, job_alias, status}]` alongside the
      existing `runs_started`, which stays for compatibility), populated in
      `triggerEventFromMatch` / `ListTriggerEvents` from `job_runs`, honouring the
      same ownership rule (a confined caller already receives only deliveries
      whose runs it can read). Update the `caesium trigger events` JSON.
      Files: `api/rest/service/event/event.go`, `cmd/` (the `trigger events`
      verb), `test/` (extend the existing trigger-events integration scenarios).
- [ ] B2. (Optional) Publish a bus event when an ingested event is persisted and
      each time a delivery is recorded (`event_ingested` / `trigger_matched`
      types in `internal/event/bus.go`), namespace-filtered by the same rules as
      other lifecycle events on `GET /v1/events`. Only justified if A3's polling
      proves too coarse; otherwise drop. It needs a redaction decision mirroring
      `redactLifecycleData`, so treat it as a design question first.
      Files: `internal/event/bus.go`, `internal/event/ingest_store.go`,
      `api/rest/controller/event/`, `test/`.
      Depends on: B1 (sequence, both touch `internal/event/`).

### Stream C — Trigger in the DAG and list affordances

- [ ] C1. A read-only **trigger node** at the left edge of the job detail DAG
      (`JobDetailPage` to `JobDAG`): an optional `trigger` prop on `JobDAGProps`
      rendering a distinct node (type icon + `describeTrigger().summary`) with
      edges to the DAG roots (tasks with no incoming edge), excluded from
      `onNodeClick` / task selection, counters, and the task timeline. Do **not**
      change the run-page DAG (`RunDetailPage`) in this item; run topology is
      per-run. Existing `JobDAG.test.tsx` node-count assertions and
      `ui/e2e/visual.spec.ts` snapshots must be reviewed and regenerated
      deliberately.
      Files: `ui/src/features/jobs/JobDAG.tsx`,
      `ui/src/features/jobs/JobDetailPage.tsx`,
      `ui/src/features/jobs/__tests__/JobDAG.test.tsx`.
- [ ] C2. On `TriggersPage`, replace the generic `Globe` for `event` with a
      distinct icon, add a "Used by" list in the expanded card (extend the typed client:
      `api.getJobs` gains an optional `trigger_id` argument that sends the
      existing `GET /v1/jobs?trigger_id=` filter; link rows to `/jobs/$jobId`), and, for
      chained triggers whose pattern filter names `job_alias` (verify the exact
      enriched key at pickup in the lifecycle bridge under
      `internal/trigger/event`), show "chained from <alias>" linking to that job.
      Files: `ui/src/features/triggers/TriggersPage.tsx`,
      `ui/src/features/triggers/trigger-utils.ts`, `ui/src/lib/api.ts`.
      Depends on: sequence after A1 (same file) and H1 (`api.ts`).
- [ ] C3. Playwright e2e `ui/e2e/trigger-dag.spec.ts` on the live server: apply
      the event-trigger fixture, open the job page, assert the trigger node
      appears with the pattern summary and is connected to the root task; apply a
      cron job and assert its trigger node differs; assert a chained pair
      (producer plus a `source: caesium` consumer) shows the "chained from" link
      on `/triggers`. No stubbing on the primary path.
      Files: `ui/e2e/trigger-dag.spec.ts`.
      Depends on: C1, C2.

#### Rejected / Deferred
- A dedicated `/triggers/$triggerId` route: the expanded card is sufficient and
  the router has no trigger-detail precedent. Revisit if the log outgrows the
  card.
- Authoring or editing `event` triggers in the console (the editor is HTTP-only
  by design; event triggers are declarative YAML).
- A global ingested-event explorer over `GET /v1/events/ingested`. A possible
  follow-on; not required by issue #409.

## Navigational / Organizational Improvements

- [ ] N-1. Close-out docs, last. Mention the console surface in roadmap §1.2 and
      link this plan; update the `> Status:` banner and Phase 4 item 15 in
      `docs/design/event-triggers.md`; replace the "Deferred — Event-trigger UI"
      text in the completed event-trigger-routing plan with a forward link; move
      this plan to `exec-plans/completed/` and fix its `docs/README.md` row.
      Files: `docs/roadmap.md`, `docs/design/event-triggers.md`,
      `docs/exec-plans/completed/event-trigger-routing.md`, `docs/README.md`.
      Depends on: every other stream.

## Sequencing & Dependencies

- **Cross-stream order:** H1 first (types and client), then H2 and H3. Stream A
  depends on H. Stream C is independent of A and B and needs only the existing
  `getTrigger` / `getJobs`. B1 is independent backend work and can land in Wave 1
  in parallel with H1; A6's linked form waits on it. B2 is optional and
  gated on a demonstrated need. N-1 runs last.
- **Within-stream:** A1, then A2 / A3 / A4 / A5 / A6, then A7 / A7b / A8. C1 and C2,
  then C3. H1, then H2, then H3.
- **Cross-stream file conflicts:** A and C2 both edit `TriggersPage.tsx`
  (sequence A before C2). H1 and B1 both define the delivery-row shape (`api.ts`
  vs `event.go`): land B1's fields before or with H1, or have H1 mark them
  optional. C1 touches `JobDAG.tsx` and its unit/visual snapshots; no other
  stream may touch them in the same wave. B1 and B2 both touch `internal/event/`;
  sequence them.

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Per-stream conditional gates:

```sh
just ui-lint && just ui-test && just ui-e2e   # any ui/** change (H, A, C)
just ui-e2e-auth                              # A8, H3 (auth lane)
go test ./internal/guardrails/                # docs-only PRs skip unit-test in CI
gofmt -l api/ cmd/ internal/                  # B-stream Go changes (just lint formats the root only)
```

End-to-end coverage gate (`CLAUDE.md`): every UI item that adds a surface ships a
Playwright scenario against the live server, with **no route stubbing on the
primary path** (A7, C3). Any new REST field or endpoint (B1, B2) ships a `test/`
integration scenario hitting the real endpoint, and CLI JSON output is asserted
on stdout captured separately from stderr. A Playwright run that only passes
with the events call stubbed does not satisfy the gate.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream H:** `api.getTriggerEvents` and the `TriggerEvent` type exist with a
   vitest round trip; the e2e `pushEvent` helper creates real deliveries; the
   auth-lane decision (H3) is recorded and its scenario is green, or its
   API-level substitute is in `test/`.
2. **Stream A:** `ui/e2e/trigger-events.spec.ts` is green in `just ui-e2e`
   against the live server, covering matched row, payload, filter, and a
   live-appearing delivery. The mixed-outcome (`partial`) delivery is green
   in a `test/` integration scenario that seeds one shared `trigger_id`
   (A7b) and asserts that equality before the push. The confined-viewer note
   (A5) is asserted in the auth lane.
3. **Stream B:** B1 is merged with an integration scenario asserting the new
   fields from the real endpoint (and the CLI JSON if changed); B2 is
   either merged with a scenario or recorded in this plan as dropped, with the
   reason.
4. **Stream C:** `ui/e2e/trigger-dag.spec.ts` is green; the job-detail DAG shows
   the trigger node; `JobDAG` unit tests and visual baselines are updated
   deliberately.
5. **Stream N:** `docs/roadmap.md` §1.2, the design banner, and the completed
   routing plan point at the shipped UI; this plan is in `exec-plans/completed/`.
6. **Cross-cutting:** this plan's per-stream Progress entries match the merged
   PRs, and issue #409 is closed by the completion PR.

## How To Pick Up Work

1. Read this file end-to-end, including `## Backend Contract`, and re-verify the
   contract entries your item relies on against the code.
2. Pick an unchecked item under `## Streams` whose `Depends on:` line is
   satisfied (consult `## Sequencing & Dependencies`).
3. Branch from `master` (or land in a worktree if dispatched by
   `exec-plan-wave`); do the work as a self-contained PR.
4. Run the verification block under `## Verification (Run For Every PR)`.
5. Tick the checkbox for your item, add a per-stream bullet to the active wave
   subsection in `## Progress` (or open a new wave subsection if none exists
   yet), and update any cross-linked doc in the same PR.
6. Open the PR with title format
   `<Imperative subject> (event-trigger-ui <wave>-<stream>)`, for example
   `Add trigger delivery log to the console (event-trigger-ui W1-β)`. GitHub
   appends `(#NNN)` on squash-merge.

## Cross-References

- [`docs/roadmap.md`](../../roadmap.md) §1.2 Event-Driven Trigger Routing
- [Event-Driven Triggers design](../../design/event-triggers.md), Phase 4 item 15
- [Event-Driven Trigger Routing](../completed/event-trigger-routing.md), the
  "Deferred — Event-trigger UI" section this plan resolves
- [Data-Plane Memory UI](../completed/data-plane-memory-ui.md), the precedent
  (client first, role/scope retention, auth-lane e2e, live-server gate)
- [Closed-Loop Arc](closed-loop-arc.md), which lists this follow-on among the
  items Plan 0 N-3 filed as issues
- Code: `ui/src/features/triggers/`, `ui/src/features/jobs/JobDAG.tsx`,
  `ui/src/lib/api.ts`, `api/rest/controller/trigger/events.go`,
  `api/rest/service/event/event.go`, `internal/auth/rbac.go`
