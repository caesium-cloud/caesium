# UI Refresh — Execution Plan

> Status: Active — Phase 0 shipped (2026-04-28, branch `claude/eager-rubin-E3djS`). Phase 1 shipped (2026-04-28, branch `claude/wizardly-sutherland-c39d8e`). Phase 2 is next. Companion to [`design-ui-refresh.md`](design-ui-refresh.md). Each phase is its own PR train; each step lists files to touch, API gaps, and acceptance criteria so an agent can pick up a single bullet without rereading the design.

## How to use this plan

- **One phase = one PR train.** Don't merge across phase boundaries. Each phase ends with the app in a shippable state.
- **One step = one PR.** Steps are small, vertical-slice work items: a primitive plus its test, or a page plus its query hook plus its API extension. If a step is bigger than ~500 lines, split it.
- **Every step has Acceptance criteria.** Don't mark a step complete until the criteria are checked. CI + a manual pass against the prototype at [`design/ui-refresh/prototype/index.html`](design/ui-refresh/prototype/index.html) is the bar.
- **Files referenced by full path** (`ui/src/...`). When this plan disagrees with `design-ui-refresh.md`, the design doc wins.
- **Backwards-compatibility shims are fine during a phase**, but the phase doesn't close until the shim is removed.

---

## Phase 0 · Foundations ✅ Shipped 2026-04-28

> Branch: `claude/eager-rubin-E3djS`. All acceptance criteria verified: 79/79 vitests pass, `npm run lint` clean, production build clean.

Phase 0 changes propagate automatically into every page. Land them first; the rest becomes additive.

### 0.1 — Extend design tokens in `ui/src/index.css` ✅

**Shipped:**
- `ui/src/index.css` — added `--cyan-glow`, `--cyan-dim`, `--gold-dim`, `--void`, `--midnight`, `--obsidian`, `--graphite`, `--silt`, `--text-1..4`, `--success`, `--warning`, `--danger`, `--running`, `--cached`, `--chart-1..5`. Light theme overrides for the same keys. Brand keyframes (`orbit-spin`, `nucleus-pulse`, `cyan-pulse`, `gold-pulse`) + reduced-motion CSS guard.
- `ui/tailwind.config.js` — extended `theme.extend.colors` with all new tokens (utility classes `bg-midnight`, `text-text-2`, `bg-danger`, etc.). Merged `keyframes` + `animation` extensions alongside existing accordion ones.

### 0.2 — Status semantics helper ✅

**Shipped:**
- `ui/src/lib/status.ts` — `statusMeta(status)` returns `{ label, fg, bg, border, dotClass }` for seven statuses + alias normalization + unknown fallback.
- `ui/src/lib/__tests__/status.test.ts` — 5 vitests covering all seven statuses, animation classes, alias normalization, and null/undefined inputs.
- `rg "succeeded.*hsl|failed.*hsl|running.*hsl" ui/src` returns no matches.

### 0.3 — Brand primitive: animated `AtomLogo` ✅

**Shipped:**
- `ui/src/components/brand/atom-logo.tsx` — three orbital ellipses + nucleus + three gold satellites. `animated?: boolean` (default `true`). `useReducedMotion()` disables spin. Exposes `forceReducedMotion` test hook. Stable per-instance gradient id via `useId()`.
- `ui/src/hooks/useReducedMotion.ts` — subscribes to `prefers-reduced-motion: reduce` media query.
- `ui/src/components/brand/__tests__/atom-logo.test.tsx` — 4 vitests (default/animated-off/reduced-motion/reduced-motion-forced).
- `ui/src/components/caesium-logo.tsx` — converted to a static re-export shim (`<AtomLogo animated={false} />`); remove in Phase 1 cleanup.

### 0.4 — UI primitives ✅

**Shipped:**

| Primitive | File | Tests |
| --------- | ---- | ----- |
| `<StatusBadge>` | `ui/src/components/ui/status-badge.tsx` | `__tests__/status-badge.test.tsx` (4 tests) |
| `<Sparkline>` | `ui/src/components/ui/sparkline.tsx` | `__tests__/sparkline.test.tsx` (3 tests) |
| `<EmptyState>` | `ui/src/components/ui/empty-state.tsx` | `__tests__/empty-state.test.tsx` (4 tests) |
| `<UTCClock>` + `UTCClockProvider` | `ui/src/components/ui/utc-clock.tsx` | `__tests__/utc-clock.test.tsx` (4 tests) |
| `<UsageBar>` | `ui/src/components/ui/usage-bar.tsx` | `__tests__/usage-bar.test.tsx` (5 tests) |
| `USAGE_THRESHOLDS` / `usageLevel` | `ui/src/lib/thresholds.ts` | covered by usage-bar tests |

### 0.5 — App shell visual upgrade ✅

**Shipped:**
- `ui/src/components/layout/Sidebar.tsx` — gold accent rail, animated `AtomLogo`, count badges (from `useNavCounts`), cluster health footer (gated on `state !== 'unknown'`). Active route shows 2px gold inset shadow.
- `ui/src/components/layout/Header.tsx` — sticky + backdrop-blur, route-aware breadcrumb, command-menu, `<UTCClock />`, notifications icon, theme toggle.
- `ui/src/components/layout/AppShell.tsx` — wraps children in `UTCClockProvider` for a single shared tick.
- `ui/src/features/jobs/useNavCounts.ts` — client-side count fallback (30s refetch). API gap filed: `GET /v1/jobs?count_only=true`.
- `ui/src/features/system/useClusterHealth.ts` — polls `/v1/system/health` every 15s; classifies to `operational | degraded | incident | unknown`.

---

## Phase 1 · High-traffic paths ✅ Shipped 2026-04-28

> Branch: `claude/wizardly-sutherland-c39d8e`. All acceptance criteria verified: 79/79 vitests pass, tsc --noEmit clean, eslint clean (one expected TanStack Virtual incompatible-library warning).

These are the pages operators stare at. Land Phase 0 first; skip ahead and you'll reinvent the primitives.

### 1.1 — Jobs list ✅

**Shipped:**
- `ui/src/features/jobs/useJobsView.ts` — `useJobsView()` hook: URL-param-driven filter/search/sort via `window.history.replaceState`, SSE event subscriptions (run events + pause events + task_cached), client-side status counts, `lastRuns` mapping from `job.last_runs` (backend field, gracefully empty until API ships), live activity feed (capped at 20 entries).
- `ui/src/features/jobs/JobsPage.tsx` — full rewrite: eyebrow label, 5-chip `<FilterBar>` segmented control with counts, Tailwind `grid-cols` layout (not `<table>`), `<StatusBadge>` throughout, `<Sparkline>` column (lazy renders after first frame), action buttons appear on row hover only, `<ActivityFeed>` from SSE stream, `<EmptyState>` when filters yield zero rows.

### 1.2 — Job Detail + DAG ✅

**Shipped:**
- `ui/src/features/jobs/JobDetailPage.tsx` — upgraded header: eyebrow label, `<StatusBadge>` replacing raw `<Badge>`, `shortId` instead of full UUID. Live overlay strip: cyan pulse `<Zap>` icon when active, `<DagCounters>` mini strip (done/active/cached/queued). URL hash node selection: `#taskId` written on select, cleared on deselect, read on mount for deep-link sharing. `onClose` uses `handleNodeSelect(null)` to also clear the hash.

### 1.3 — Run Detail + logs ✅

**Shipped:**
- `ui/src/features/jobs/RunDetailPage.tsx` — breadcrumb nav, eyebrow label, `<StatusBadge>`, collapsible Gantt timeline section (wraps existing `RunTimeline`), action cluster (All runs / Re-run / Cancel), task-level `<RunLogViewer>` opens below the timeline when a DAG node is selected.
- `ui/src/features/jobs/RunLogViewer.tsx` — new component: `@tanstack/react-virtual` row virtualizer (60fps at 50k lines), streaming fetch from `/v1/jobs/.../logs`, status pill (Loading / Live / Complete / Error / Empty), at-bottom detection with `setAtBottom`, "Jump to live" pill shown when scrolled up during a running task, scroll position persisted to `sessionStorage` keyed on `runId:taskId`.

**Remaining API gaps (unchanged):**
- `lastRuns` field on Job DTO — sparklines show dash until backend ships this field.

---

### 1.1 — Jobs list (original spec)

**File:** `ui/src/features/jobs/JobsPage.tsx` (replace existing).

**Steps:**
1. Wrap the existing `useJobs()` query in a new `useJobsView()` hook returning `{ rows, counts, search, setSearch, statusFilter, setStatusFilter, sort, setSort }`. URL params (`?status=failing&q=etl`) drive the hook so deep-links work.
2. New typed component `<JobsFilterBar counts={counts} />` — five-chip segmented control. Counts come from `GET /v1/jobs/summary` (preferred) or client-side counting (fallback).
3. Table layout via Tailwind `grid-cols`, not `<table>` — keeps row hover / focus styling clean. Columns per [`design-ui-refresh.md`](design-ui-refresh.md) §"Jobs list".
4. Sparkline column reads `lastRuns: RunSummary[]` (last 14) directly from the job DTO — extend the API rather than fanning out per-row queries.
5. Row hover scan-line + selection state via Tailwind variants. Gate behind the user `rowHover` preference (Phase 0 stub).
6. Live activity feed below the table consumes the existing SSE stream from `ui/src/lib/events.ts`. Cap at 20 entries; oldest fall off.

**API gaps:**
- `GET /v1/jobs/summary` (P1 cleanup; client-side counting acceptable as fallback).
- `lastRuns: RunSummary[]` on the Job DTO (blocking — sparkline doesn't ship without it).

**Acceptance:**
- `?status=failing&q=etl` deep-link works.
- Sparklines never block initial paint (lazy after first frame).
- Empty state shows when filters yield zero rows.
- The activity feed updates in real time as runs progress in a dev cluster.

### 1.2 — Job Detail + DAG

**File:** `ui/src/features/jobs/JobDetailPage.tsx`.

**Steps:**
1. Header strip with alias, status badge, schedule, last-run "ago", success rate, p95. Reads from `useJob(id)` and `useJobStats(id)`.
2. Extract `ui/src/features/jobs/JobDAG.tsx` if it isn't already standalone; replace its renderer with the prototype's column-lane layout. Keep `dagre` (already in `package.json`) as the layout engine — port [`prototype/pages-jobs.jsx`](design/ui-refresh/prototype/pages-jobs.jsx) `nodePos` math as a fallback for trivial graphs but use `dagre` for non-trivial topologies. Add vitest snapshot tests for: linear, fan-out, fan-in, diamond, multi-component graphs.
3. Live overlay strip above the DAG: cyan pulse + "Live overlay from <run-id>" + elapsed, plus the four mini counters (done / active / cached / queued).
4. Selecting a node opens an in-place side panel (existing `TaskDetailPanel.tsx` if it covers the same data; otherwise wrap it). Persist selection in URL hash.
5. "Recent runs" list below the DAG is a thin wrapper around the existing runs table.

**Acceptance:**
- DAG re-renders correctly when atoms are added/removed via JobDefs apply.
- Node selection persists in URL hash for sharing.
- Snapshot tests cover the five DAG topologies.

### 1.3 — Run Detail + logs

**File:** `ui/src/features/jobs/RunDetailPage.tsx`.

**Steps:**
1. Replace `RunTimeline.tsx` (or extend it) with a Gantt timeline: one row per atom run on a shared time axis, bar fill = `statusMeta(atomRun.status).bg`. Hover highlights matching log lines via a tiny zustand store scoped to this page (`ui/src/features/jobs/runDetailStore.ts`).
2. Virtualize the log viewer with `@tanstack/react-virtual` (already in `package.json`). Filters: level (`ALL`/`INFO`/`WARN`/`DEBUG`/`ERROR`), atom, freetext.
3. Tail-follow: when at-bottom and the run is `running`, append new log lines via the existing SSE stream (`ui/src/lib/events.ts`). Auto-detach on user scroll-up; reattach on "Jump to live" pill click. Persist last scroll position in `localStorage` keyed on `runId`.
4. Top-right action cluster: All runs (history) / Re-run / Cancel.

**Acceptance:**
- Opening a 50,000-line log scrolls smoothly at 60fps in dev tools.
- Tail-follow correctly stops on user scroll, resumes on "Jump to live."
- Hovering a Gantt bar dims the log viewer to matching lines only.

---

## Phase 2 · Lower-traffic paths

Land Phase 1 first; these pages are quieter and benefit from the primitives stabilizing.

### 2.1 — Stats

**File:** `ui/src/features/stats/StatsPage.tsx`.

**Steps:**
1. KPI strip (Total jobs / Runs 24h / Success rate / Avg duration) — read from `GET /v1/stats/summary?window=7d`. Add the endpoint if missing.
2. Dual-axis chart (run volume bars + success-rate line). Use `recharts` (already in `package.json`). Time bucket = day if range ≤ 30d, hour-of-day rollup otherwise.
3. Failure distribution component (top N failing atoms, horizontal bars). Click-through opens runs list filtered to that atom + `status=failed`.
4. Time-range selector (24h / 7d / 30d) drives all three sections through one query. Loading state = skeletons (use existing `<Skeleton />`), not spinners.

**API gaps:**
- `GET /v1/stats/summary?window=…` returning KPIs + trend + failure distribution in one payload.

**Acceptance:**
- Range selector updates all three sections in one query (verify in Network tab).
- Skeletons show during fetch; charts don't reflow when data lands.

### 2.2 — Triggers

**File:** `ui/src/features/triggers/TriggersPage.tsx`.

**Steps:**
1. Typed pill column: `cron` cyan with clock icon, `webhook` gold with webhook icon. Pull the icons from `lucide-react` (already in `package.json`).
2. Webhook rows show inline copy-to-clipboard URL (use existing `<Button variant="ghost">` + `navigator.clipboard.writeText`).
3. "Next fire" for cron triggers: compute on the client with a small cron parser. If a dependency-free implementation is acceptable (Caesium uses 5-field POSIX cron; a vendored parser is ~80 LOC), prefer that over adding `cron-parser` to bundle. Otherwise add `cron-parser` and accept the bundle increase.
4. State column reuses `<StatusBadge>` (paused vs active).

**Acceptance:**
- Webhook URLs copy to clipboard with toast confirmation (use existing `sonner`).
- "Next fire" stays accurate without polling — refreshes only on local clock tick.

---

## Phase 3 · Net-new server work

System and JobDefs need backend extensions. Ship behind `CAESIUM_UI_REFRESH_V2_SYSTEM` (server-side feature flag exposed via `/v1/system/features`) until the new endpoints stabilize.

### 3.1 — System

**File:** `ui/src/features/system/SystemPage.tsx` (replace).

**Steps:**
1. Health banner — `useClusterHealth()` (introduced in 0.5) drives banner color, dot, copy. Three states: `operational` (green), `degraded` (gold), `incident` (red).
2. KPI strip (DB / active runs / triggers / nodes) — extend the same `/v1/system/health` response shape rather than fanning out four queries.
3. Nodes table — `GET /v1/system/nodes`. Columns: address (with status dot + uptime), role badge (leader/voter), arch, CPU `<UsageBar />`, mem `<UsageBar />`, workers ratio.
4. Cluster topology mini-map — pure-presentation SVG ring from [`prototype/pages-system.jsx`](design/ui-refresh/prototype/pages-system.jsx) `ClusterTopology`. Vitest snapshots for 3-node and 7-node arrangements.
5. Operator tools row — three cards. DB console links to `/system/database` (existing route), log console links to `/system/logs` (existing route). Both gated by `CAESIUM_DATABASE_CONSOLE_ENABLED` / `CAESIUM_LOG_CONSOLE_ENABLED` flags from `/v1/system/features`. Cache-prune dialog calls `POST /v1/system/cache/prune`; confirm dialog (existing `<Dialog>`), optimistic toast, query invalidation on success.
6. Health checks list — binds to `health.checks[]` from the same health endpoint.
7. Prometheus metrics reference — emit a TS constant from the Go server's metrics registry via `scripts/generate-metrics-list.ts`. Keeps the docs honest.

**API gaps:**
- `GET /v1/system/health` (banner + KPIs + checks)
- `GET /v1/system/nodes`
- `GET /v1/system/features`
- `POST /v1/system/cache/prune`
- A Go-side script + generated TS constant for the metrics reference.

**Acceptance:**
- Killing a node in a dev cluster updates topology + KPI strip within 15s without page refresh.
- The cache-prune flow shows confirm + optimistic toast + correct query invalidation.
- Topology mini-map renders correctly at 3, 5, and 7 nodes.

### 3.2 — JobDefs

**File:** `ui/src/features/jobdefs/JobDefsPage.tsx` (replace existing).

**Steps:**
1. Replace the prototype's overlay-syntax-highlight hack with a real editor. Use `@codemirror/lang-yaml` + `@uiw/react-codemirror` (small, well-maintained). Wire CodeMirror's lint API to the server-side validator.
2. `POST /v1/jobdefs/lint` returns `{ errors, warnings, summary: { steps } }`. Debounce 300ms on every keystroke. Footer reads from this response — no client-side regex.
3. Diff vs server: `POST /v1/jobdefs/diff` returns a structured changeset (add/modify/remove with paths). Renderer is a typed component; no string parsing on the client.
4. Apply: `POST /v1/jobdefs/apply` (existing). On success, invalidate `jobs`, `atoms`, `triggers`, and `triggers/summary` query keys.
5. History tab: `GET /v1/jobdefs/history?alias={alias}` — apply audit log. If the endpoint isn't ready, ship the page with the tab disabled (tooltip: "Coming in v1.1").
6. Right rail: schema reference + tips. Static MDX content imported as a component so docs writers can edit it without touching the page.

**API gaps:**
- `POST /v1/jobdefs/lint`
- `POST /v1/jobdefs/diff`
- `GET /v1/jobdefs/history` (v1.1 — non-blocking)

**Acceptance:**
- Editor highlights syntax via CodeMirror; lint errors mark the gutter and the inline gutter.
- Apply success invalidates all dependent queries; the Jobs list reflects new aliases without a page refresh.
- Schema-valid manifests apply in <500ms in a dev cluster (matches existing `caesium job apply` perf).

---

## Phase 4 · Cleanup

After Phase 3 ships, close the loop.

- **Remove the v1 `caesium-logo.tsx`** in favor of `<AtomLogo />`. Verify no imports remain.
- **Delete the row-hover, badge-style, and accent preference scaffolding** introduced in Phase 0 if any of them remain (they should already be removed during their phase).
- **Drop the `CAESIUM_UI_REFRESH_V2_SYSTEM` flag** once the new System and JobDefs surfaces have soaked in production for at least one minor release.
- **Update [`ui_implementation_plan.md`](ui_implementation_plan.md)**: move the visual layer items to "shipped"; keep the closed list of acceptance criteria as historical record.
- **Audit `docs/design/ui-refresh/prototype/`** — keep it as reference. Removing it loses provenance for visual decisions if a question comes up later.

---

## API gap summary

Use this list when scheduling backend work in parallel with frontend work.

| Endpoint                                     | Phase    | Blocking?                        | Notes                                                       |
| -------------------------------------------- | -------- | -------------------------------- | ----------------------------------------------------------- |
| `GET /v1/jobs?count_only=true`               | 0.5      | No (fallback: full-list count)   | Sidebar nav badges                                          |
| `GET /v1/jobs/summary`                       | 1.1      | No (fallback: client counting)   | Status counts for filter chips                              |
| `lastRuns` field on Job DTO (last 14)        | 1.1      | **Yes**                          | Sparklines                                                  |
| `GET /v1/stats/summary?window=…`             | 2.1      | **Yes**                          | KPIs + trend + failure distribution in one payload          |
| `GET /v1/system/health`                      | 3.1      | **Yes**                          | Banner + KPI strip + checks                                 |
| `GET /v1/system/nodes`                       | 3.1      | **Yes**                          | Nodes table                                                 |
| `GET /v1/system/features`                    | 3.1      | **Yes**                          | Operator tools toggles                                      |
| `POST /v1/system/cache/prune`                | 3.1      | **Yes**                          | Cache-prune button                                          |
| `POST /v1/jobdefs/lint`                      | 3.2      | **Yes**                          | CodeMirror lint provider                                    |
| `POST /v1/jobdefs/diff`                      | 3.2      | **Yes**                          | Diff tab                                                    |
| `GET /v1/jobdefs/history?alias=…`            | 3.2      | No (tab can be disabled)         | History tab                                                 |
| `GET/PATCH /v1/users/me/preferences`         | 0.5      | No (localStorage fallback)       | Density / sparkline preferences                             |

---

## What this plan deliberately leaves out

- **No mobile breakpoints.** Desktop-first; not a roadmap item.
- **No i18n.** Hardcoded English. If we ever add i18n, it's a separate pass.
- **No accessibility audit beyond AA contrast + keyboard nav + reduced-motion.** Run `axe-core` in CI before each phase ships.
- **No marketing landing page work.** This refresh is the *operator console*. The brand site is out of scope.
