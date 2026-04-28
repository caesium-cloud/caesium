# Caesium UI Refresh — Implementation Plan

A step-by-step guide for porting this design refresh into the real `caesium/ui` codebase (React + TypeScript + Tailwind + shadcn/ui + TanStack Query).

This is **not** a port of the prototype's inline-style code. The prototype uses raw `style={{}}` and CSS custom properties for speed; the production work re-expresses every visual decision through the existing design tokens and component primitives so nothing fights the established system.

---

## 0 · Foundations (do these first, once)

These changes land before any page work. They make every subsequent step a small, additive PR.

### 0.1 — Audit and extend design tokens

In `ui/src/index.css` (or wherever the Tailwind theme lives):

- Add the brand palette as CSS custom properties on `:root` and `.dark`:
  - `--brand-cyan: 188 96% 52%` and `--brand-cyan-glow: 188 96% 65%`
  - `--brand-gold: 45 92% 58%`
  - `--surface-void: 220 30% 4%`, `--surface-obsidian: 220 22% 7%`, `--surface-midnight: 220 18% 10%`
  - `--graphite: 220 14% 18%` for hairlines
- Map `accent`, `chart-1..5`, and the status colors (`success`, `warning`, `destructive`) to the new palette so existing shadcn components inherit the refresh without per-component overrides.
- Add `--font-display: "Space Grotesk", …` and `--font-mono: "IBM Plex Mono", …`. Update `tailwind.config.ts` `fontFamily` to point at them.

**Acceptance:** existing pages render in the new color scheme with no per-component changes; Storybook (if present) reflects the palette globally.

### 0.2 — Status semantics

The prototype hardcodes seven run statuses (`running`, `succeeded`, `failed`, `queued`, `paused`, `cached`, `skipped`). In the real codebase:

- Confirm the canonical status enum in `ui/src/lib/api/types.ts` (or wherever `RunStatus` lives).
- Create `ui/src/lib/status.ts` that exports `statusMeta(status): { label, fg, bg, border, dotClass }`. Every place that renders a status — badges, dots, sparkline tints, log level chips — calls this. No more inline color choices.

### 0.3 — Shared primitives

Lift these prototype components into `ui/src/components/ui/` (or the project's equivalent), typed and Storybook-ready:

| Prototype component       | Production location                     | Notes                                          |
| ------------------------- | --------------------------------------- | ---------------------------------------------- |
| `AtomLogo`                | `components/brand/atom-logo.tsx`        | `animated?: boolean` prop; respects `prefers-reduced-motion` |
| `StatusBadge`             | `components/ui/status-badge.tsx`        | Variants: `filled`, `soft`, `dot` — drives off `statusMeta` |
| `Sparkline`               | `components/ui/sparkline.tsx`           | SVG; accepts `data: number[]` and an `accent`  |
| `Btn`                     | Already exists as `Button` — just port the `icon` slot pattern |
| `EmptyState`              | `components/ui/empty-state.tsx`         |                                                |
| `UTCClock`                | `components/ui/utc-clock.tsx`           | Single `setInterval` shared via context        |

Each ships with a Storybook entry covering states (loading, empty, error, every status) so QA can sign off in isolation.

---

## 1 · Shell (sidebar, header, theme)

**File:** `ui/src/components/layout/AppShell.tsx` (replace existing).

- Move the brand mark + nav items + cluster footer from the prototype's `Sidebar` into the existing layout component. Sidebar nav items already exist as a route registry — wire `count` badges from a lightweight `useNavCounts()` hook that fans out one batched query (`/api/v1/jobs?count_only=true` etc.) every 30s, so the numbers don't lie.
- Replace the header search field with the existing `<CommandPalette />` trigger if one exists; otherwise leave the visual but stub the open handler.
- The "🟢 connected" cluster footer in the sidebar reads from `useClusterHealth()` (see §6).

**Acceptance:** every existing route in `App.tsx` still resolves; nav badges update without page refresh.

---

## 2 · Jobs page

**File:** `ui/src/features/jobs/JobsPage.tsx`.

The prototype's table is the target. Migration steps:

1. Wrap the existing TanStack Query (`useJobs()`) data with a small `useJobsView()` hook that returns `{ rows, counts, search, setSearch, statusFilter, setStatusFilter, sort, setSort }` — the segmented filter bar drives state through this hook so the URL stays in sync (`?status=failing&q=etl`).
2. Build the segmented filter bar as a typed component (`<JobsFilterBar counts={counts} />`) with five chips. Counts come from a server-side rollup (add `GET /api/v1/jobs/summary` if it doesn't exist; fall back to client-side counting otherwise).
3. Sparkline column: each row reads `lastRuns: RunSummary[]` (last 14). If that field isn't on the existing job DTO, extend the API; do **not** issue a per-row request.
4. Status pill, last run "ago" timestamp, next run cron, owner, and tags follow the prototype layout. Use Tailwind grid-cols rather than table markup so row hover / focus styling stays clean.
5. Hover scan-line + selection state come from a Tailwind variant added to `tailwind.config.ts` (`hover:before:scale-x-100` etc.). Gate behind the `rowHover` setting persisted in the user's preferences (see §7).

**Acceptance:** `?status=failing&q=etl` deep-link works; sparklines never block initial paint (lazy-render after first frame); empty state shows when filter/search yields zero rows.

---

## 3 · Job Detail + DAG

**File:** `ui/src/features/jobs/JobDetailPage.tsx`.

1. Header strip (alias, trigger pill, last run, success rate, p95) reads from `useJob(id)` and `useJobStats(id)`.
2. DAG renderer: extract `DagGraph.tsx` as its own component. Input is the existing atom DTO + edges. Layout uses a topological sort + per-rank vertical stacking; the prototype's `dagrePos()` function is good enough — port it, but cover edge cases with vitest snapshot tests for these topologies: linear, fan-out, fan-in, diamond, multi-component.
3. Selecting a node opens the existing atom detail drawer in-place rather than a new route — keep it consistent with how the rest of the app handles secondary surfaces.
4. Recent runs list below the DAG is a thin wrapper around the existing `<RunsTable />`.

**Acceptance:** DAG re-renders correctly when atoms are added/removed via JobDefs apply; node selection persists in URL hash for sharing.

---

## 4 · Run Detail + Logs

**File:** `ui/src/features/runs/RunDetailPage.tsx`.

1. **Gantt timeline** — each atom run as a horizontal bar on a shared time axis. Bar color = `statusMeta(atomRun.status).bg`. Hovering a bar highlights the matching log lines (cross-component selection state lives in a tiny zustand store for this page only — `useRunDetailStore()`).
2. **Log viewer** — virtualized with `react-virtuoso` (already in repo, presumably). Filters: level (info/warn/error/debug) + atom + freetext.
3. **Tail-follow** — when at-bottom and the run is `running`, append new log lines via the existing SSE stream. Auto-detach on user scroll-up; reattach on "Jump to live" pill click. Persist last scroll position in `localStorage` keyed on `runId`.

**Acceptance:** opening a 50 000-line log scrolls smoothly at 60fps; tail-follow correctly stops/resumes.

---

## 5 · Stats dashboard

**File:** `ui/src/features/stats/StatsPage.tsx`.

The prototype's three-section layout (KPI strip, dual-axis chart, failure distribution) maps directly. Use `recharts` (or whatever's already in the repo):

- KPI cards read from `GET /api/v1/stats/summary?window=7d`. Add the endpoint if missing — schema in §8.
- Run-volume / success-rate dual-axis chart: bars for volume, line for success %. Time bucket = day if range ≤ 30d, else hour-of-day rollup.
- Failure distribution: top N failing atoms with horizontal bars, sorted by count. Click-through opens the runs list filtered to that atom + `status=failed`.

**Acceptance:** time-range selector (24h / 7d / 30d) updates all three sections in one query; loading state shows skeletons, not spinners.

---

## 6 · Triggers page

**File:** `ui/src/features/triggers/TriggersPage.tsx`.

Straightforward port of the prototype table. Two notes:

- Type column is a typed pill (`cron` vs `webhook`); icon + label come from a small map. Webhook rows show a copy-to-clipboard URL inline.
- "Next fire" for cron triggers: compute on the client with `cron-parser` (already in `package.json`) so it stays accurate without polling.

---

## 7 · System page

**File:** `ui/src/features/system/SystemPage.tsx`.

This is the most net-new work — most of it doesn't exist in the current UI.

1. **Health banner** binds to `useClusterHealth()` which polls `/api/v1/system/health` every 15s. Banner color, dot, and "all systems operational" / "degraded" / "incident" copy all derive from the response.
2. **KPI strip** (DB, active runs, triggers, nodes) comes from the same health endpoint — extend its response shape rather than fanning out four queries.
3. **Nodes table** binds to `/api/v1/system/nodes`. CPU/mem usage bars use the shared `<UsageBar />` primitive; thresholds (65/85) live in `lib/thresholds.ts`.
4. **Quorum topology mini-map** — the SVG ring from the prototype. Pure presentation: input is `nodes[]`, no extra API. Add a vitest test covering 3-node and 7-node arrangements.
5. **Operator tools row** — three cards. The DB console and log console each link to a separate route (`/system/db`, `/system/logs`); both are gated by `CAESIUM_DATABASE_CONSOLE_ENABLED` / `CAESIUM_LOG_CONSOLE_ENABLED` flags exposed via `/api/v1/system/features`. The cache prune dialog is a `POST /api/v1/system/cache/prune` action — confirm dialog, optimistic toast, query invalidation on success.
6. **Health checks list** binds to `health.checks[]` from the same health endpoint.
7. **Prometheus metrics reference** — static array of metric names, sourced from a generated TS constant emitted by the Go server's metrics registry (script: `scripts/generate-metrics-list.ts`). This keeps the docs honest.

**Acceptance:** killing a node in a dev cluster updates the topology and KPI strip within 15s without page refresh.

---

## 8 · JobDefs page

**File:** `ui/src/features/jobdefs/JobDefsPage.tsx` (replace existing).

Three tabs (Editor / Diff / History). Notes:

1. **Editor** — replace the prototype's overlay-syntax-highlight hack with a proper editor. Use `@codemirror/lang-yaml` + `@uiw/react-codemirror` (small, already in the orbit of the existing tooling). Wire CodeMirror's lint API to the server-side validator.
2. **Lint** — `POST /api/v1/jobdefs/lint` returns `{ errors, warnings, summary }`. Debounced (300ms) on every keystroke. The footer's "schema valid · 5 steps detected" line reads from this response, not from a client-side regex.
3. **Diff vs server** — `POST /api/v1/jobdefs/diff` returns a structured changeset (add/modify/remove nodes with paths). The view is a typed component; no string parsing on the client.
4. **Apply** — `POST /api/v1/jobdefs/apply` is the existing endpoint. On success, invalidate `jobs`, `atoms`, and `triggers` query keys.
5. **History tab** binds to `GET /api/v1/jobdefs/history?alias={alias}` — the audit log of past applies. If the endpoint doesn't exist, this tab is a v1.1 follow-up; ship the page without it for now.
6. **Right rail** (schema reference, tips) — static MDX content imported as a component, so docs writers can edit it without touching the page.

**API gaps to file:**
- `POST /api/v1/jobdefs/lint` (returns structured errors+warnings+summary)
- `POST /api/v1/jobdefs/diff` (returns changeset against current server state)
- `GET /api/v1/jobdefs/history` (audit log)

---

## 9 · Tweaks → user preferences

The prototype's Tweaks panel is a design exploration tool, **not** something to ship as-is. Triage:

| Tweak              | Ship as…                                                                  |
| ------------------ | ------------------------------------------------------------------------- |
| Density            | User preference, persisted in `/api/v1/users/me/preferences`. Settings page exposes it. |
| Accent             | Drop. The brand has one accent; multiple accents fragment the system.     |
| Badge style        | Drop. Pick `filled` and commit.                                           |
| Sparklines on/off  | User preference (some operators find them noisy at scale).                |
| Logo animation     | Auto-disable when `prefers-reduced-motion: reduce`. No setting needed.    |
| Row hover          | Drop. Pick `subtle` and commit.                                           |

---

## 10 · Rollout

1. **Phase 1 — Foundations only** (§0). Land the palette + status semantics + primitives. Visible change: existing app gets the refreshed look immediately.
2. **Phase 2 — Jobs + Job Detail + Run Detail** (§§2-4). The high-traffic paths.
3. **Phase 3 — Stats + Triggers** (§§5-6). Lower-traffic surfaces.
4. **Phase 4 — System + JobDefs** (§§7-8). Most net-new server work; ship behind a feature flag (`CAESIUM_UI_REFRESH_V2_SYSTEM`) until the new endpoints stabilize.

Each phase is its own PR train. No "big bang" merge.

---

## 11 · What this plan deliberately leaves out

- **No mobile breakpoints.** The console is desktop-first; users opened tickets explicitly asking for higher density, not responsive layouts. If mobile becomes a goal, it's a separate design pass.
- **No i18n.** All copy is hardcoded English in the prototype. Wrap in the existing i18n system if (and only if) the project already does this elsewhere.
- **No accessibility audit beyond AA contrast.** Assumed: keyboard nav, focus rings, ARIA roles on tabs/menus all flow through shadcn primitives. Run axe-core in CI before each phase ships.
