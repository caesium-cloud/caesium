# Design: UI Refresh (Caesium Console v2)

> Status: Proposed. Reference prototype lives at [`design/ui-refresh/`](design/ui-refresh/). Phased execution plan lives at [`ui-refresh-execution-plan.md`](ui-refresh-execution-plan.md).

## Problem Statement

The embedded console (under `ui/`) reached functional parity with the operator workflow but is visually generic — shadcn defaults, no consistent status semantics, and primitives that each page reinvents (job rows, status pills, sparklines). The product reads as a competent admin panel, not as the focused control plane it actually is. A design exploration ("Caesium UI Refresh") proposed a tighter visual system with a coherent identity (atom motif, cyan + gold against a deep void) and a consistent set of primitives that every page reuses.

This document is the durable design record for that refresh. It captures the visual intent, the token layer, the status semantics, the primitive inventory, and what is deliberately out of scope. The phased execution plan ([`ui-refresh-execution-plan.md`](ui-refresh-execution-plan.md)) is the engineering breakdown.

## Goals

1. **One coherent visual system.** Every page renders status, density, motion, and color through the same primitives. Per-page CSS should be the exception, not the rule.
2. **Operator density without clutter.** The console is desktop-first and information-dense. The refresh leans into that — sparklines on the Jobs list, live activity feed, KPI strips on System and Stats — without becoming noisy.
3. **Identity that matches the project.** Caesium's brand is the atom: a stable nucleus with deterministic orbits. The UI should evoke that calmly (subtle animations on the logo, cyan pulse on running rows) rather than performatively.
4. **Additive, not destructive.** The refresh ships behind small primitives + token changes that automatically propagate to existing pages. No "big bang" merge.

## Non-Goals

- **Mobile or responsive layouts.** The console is desktop-first. Operators have asked for higher density; nobody has asked for phone support.
- **Internationalization.** Copy stays English. If we ever need i18n, that's a separate pass and shouldn't block this refresh.
- **A multi-accent theme system.** Caesium has one accent. The Tweaks panel in the prototype is design exploration, not a settings surface to ship.
- **Replacing the data layer.** TanStack Query, the SSE event bus, the existing API contract all stay. Some endpoints get extended; none get rewritten.

## Visual Language

### Palette

The refresh extends the existing token set in `ui/src/index.css` rather than replacing it. Names are CSS custom properties as `H S% L%` triplets so they compose with `hsl(var(--name) / <alpha>)`.

**Brand**

| Token              | Dark           | Light          | Use                                       |
| ------------------ | -------------- | -------------- | ----------------------------------------- |
| `--cyan`           | `191 100% 47%` | `191 100% 35%` | Primary accent, running state, links      |
| `--cyan-glow`      | `191 100% 60%` | `191 100% 42%` | Hover/focus highlights, active indicators |
| `--cyan-dim`       | `191 80% 35%`  | (inherits)     | Disabled / secondary cyan                 |
| `--gold`           | `38 92% 56%`   | (inherits)     | Eyebrows, accent rail, paused, "warn"     |
| `--gold-dim`       | `38 80% 42%`   | (inherits)     | Disabled / secondary gold                 |

**Surfaces** (dark theme; light theme uses the same names with light values)

| Token         | Dark           | Use                                   |
| ------------- | -------------- | ------------------------------------- |
| `--void`      | `240 33% 4%`   | App background                        |
| `--midnight`  | `240 25% 7%`   | Card / surface background             |
| `--obsidian`  | `240 18% 11%`  | Hover row, muted surface              |
| `--graphite`  | `240 14% 17%`  | Hairline borders                      |
| `--silt`      | `240 10% 22%`  | Scrollbar thumb, separators           |

**Text** (dark)

| Token       | Dark           | Use                                |
| ----------- | -------------- | ---------------------------------- |
| `--text-1`  | `210 40% 96%`  | Primary text                       |
| `--text-2`  | `220 14% 72%`  | Secondary text                     |
| `--text-3`  | `220 12% 50%`  | Tertiary text, eyebrow labels      |
| `--text-4`  | `220 14% 32%`  | Disabled / structural text         |

**Status** — keep these semantic; never write a hex / hsl literal at a call site.

| Token         | Dark            | Status mapping              |
| ------------- | --------------- | --------------------------- |
| `--success`   | `152 65% 45%`   | `succeeded`                 |
| `--warning`   | `38 92% 56%`    | `paused`, soft warning      |
| `--danger`    | `0 78% 62%`     | `failed`                    |
| `--running`   | `191 100% 50%`  | `running` (cyan pulse)      |
| `--cached`    | `178 60% 50%`   | `cached` (teal accent)      |
| `--text-3/4`  | (above)         | `queued`, `skipped`         |

The full palette is in [`design/ui-refresh/prototype/styles.css`](design/ui-refresh/prototype/styles.css).

### Typography

- **Display + sans:** Space Grotesk (already in `:root` font stack). Used for headings, body, and UI chrome.
- **Mono:** JetBrains Mono (preferred), with IBM Plex Mono as fallback. Used for IDs, durations, schedules, log content, and any tabular numeric column.
- **Eyebrow utility:** 10px, `letter-spacing: 0.32em`, uppercase, color `--text-3`. Used on every page-header label ("Pipelines", "Telemetry", "Schedules & Webhooks") and every section heading.
- **Tabular numerics:** any column that compares numbers (durations, counts, percentages, IDs) gets `font-variant-numeric: tabular-nums` so values line up.

### Motion

- **Atom logo:** three orbits at 22s / 30s / 38s linear (one reversed); nucleus pulses 2.4s. Honor `prefers-reduced-motion: reduce` — the logo renders static if the user opts out.
- **Running state:** a 1.6s cyan pulse on the dot, plus a 2px cyan scan-line on the left edge of running table rows.
- **Cached state:** a soft 178 60% 60% teal — no animation. Cached is *good*; it shouldn't compete with running for attention.
- **Page entry:** a 0.45s `fade-up` on each page's root container. Charts and DAGs do not animate on entry — only structural surfaces.
- **No purely decorative animation.** Every animation maps to state (running, paused, applying, etc.).

### Layout

- **Sidebar** (220–248px wide, collapsible to 220 in compact density): brand mark, nav (with count badges per route), cluster health footer, gold accent rail. Active route gets a 2px gold tab on the left edge.
- **Header** (54px, sticky, blurred over `--void`): breadcrumb, command-bar trigger (⌘K), live UTC clock, theme toggle, notifications.
- **Page padding:** 20px / 28px (vertical / horizontal) at the page root. Cards use 16px padding.
- **Density modes:** the existing UI ships at "regular." Compact and cozy modes drop / grow row height by ~10px each. Persist as a user preference (see §"Tweaks triage").

### DAG canvas

- Background is a radial gradient over `--obsidian → --void`, plus a 24px grid of `--graphite / 0.3` lines.
- Nodes are 188×76 rectangles, `--midnight` fill, `--graphite` stroke (cyan when selected). A 3px status-colored stripe runs down the left edge.
- Edges are bezier curves. Active edges (running successor) animate a 3px circle along the path on a 1.6s loop. Completed edges are solid `--success / 0.5`; pending edges are `--text-4 / 0.5`.

## Status Semantics

The console renders seven statuses everywhere:

| Status      | Token       | Animation        | Notes                                |
| ----------- | ----------- | ---------------- | ------------------------------------ |
| `running`   | `--cyan`    | pulse + scan     | Drives row scan-line + pulsing dot   |
| `succeeded` | `--success` | none             |                                      |
| `failed`    | `--danger`  | none             |                                      |
| `queued`    | `--text-3`  | none             | Neutral grey                         |
| `paused`    | `--gold`    | gold-pulse       |                                      |
| `cached`    | `178 60% 50%` | none           | Teal — distinct from running         |
| `skipped`   | `--text-4`  | none             | Faded grey                           |

Implementation: a `statusMeta(status)` helper at `ui/src/lib/status.ts` returns `{ label, fg, bg, border, dotClass }`. **Every** status-tinted surface (badge, dot, sparkline tint, log-level chip, edge color, row left-border) reads from this. Per-component color choices are a refactoring target.

The canonical run-status enum already lives in `ui/src/lib/api.ts` (or whichever file owns API types) — the helper extends it; the enum stays the source of truth.

## Primitive Inventory

These ship in `ui/src/components/ui/` (or `components/brand/` for brand-specific) before any page work begins. Each one ships with a matching test in `__tests__/`.

| Primitive        | Path                                          | Notes                                                         |
| ---------------- | --------------------------------------------- | ------------------------------------------------------------- |
| `<AtomLogo>`     | `components/brand/atom-logo.tsx`              | `animated?: boolean`. Honors `prefers-reduced-motion`. Replaces / extends the existing `caesium-logo.tsx` — keep the old export for migration, then delete. |
| `<StatusBadge>`  | `components/ui/status-badge.tsx`              | Variants: `filled` (default), `soft`, `dot`. Drives off `statusMeta`. |
| `<Sparkline>`    | `components/ui/sparkline.tsx`                 | SVG. Accepts `data: RunSummary[]`, `accent?: string`. Lazy-renders after first paint. |
| `<EmptyState>`   | `components/ui/empty-state.tsx`               | Title + subtitle + optional action. Renders a static `AtomLogo` motif. |
| `<UTCClock>`     | `components/ui/utc-clock.tsx`                 | Single `setInterval` shared via context — don't fan out timers. |
| `<UsageBar>`     | `components/ui/usage-bar.tsx`                 | CPU/mem bar. Thresholds (65 / 85) live in `lib/thresholds.ts`. |

The existing `<Button>` already covers `<Btn>`'s API; we just need to make sure every page uses the `icon` slot consistently rather than rendering icons inline.

## Page Intent

A short note on each page. The execution plan has the file-by-file breakdown.

### Jobs list
The high-traffic page. Segmented filter bar (`All` / `Running` / `Recent failures` / `Paused`) with live counts; freetext alias filter; table with alias + description, status, 7d sparkline, last run, duration with cache-hit count, row actions. Live activity feed below the table consumes the existing SSE stream.

### Job Detail
Header with alias, status, schedule, last run, p95 duration. Tabs: DAG (default), Runs, Tasks, Config, YAML. Live overlay strip (cyan pulse, run progress: done / active / cached / queued counts) reads from the latest run. DAG canvas as described above. Selecting a node opens an in-place side panel; URL hash persists the selection for sharing.

### Run Detail
Top: Gantt timeline (one row per atom run, color = `statusMeta(status).bg`). Bottom: virtualized log viewer with level filter (`ALL`/`INFO`/`WARN`/`DEBUG`/`ERROR`), freetext filter, "Follow tail" toggle. Hover over a Gantt bar highlights the matching log lines (cross-component selection in a tiny zustand store scoped to this page).

### Stats
KPI strip (Total jobs / Runs 24h / Success rate / Avg duration), 30-day dual-axis chart (volume bars + success-rate line), failure distribution, top-failing + slowest tables. Time-range selector (24h / 7d / 30d) drives all three sections in one query.

### Triggers
Typed pill column (`cron` cyan vs `webhook` gold). Webhook rows show inline copy-to-clipboard URL. "Next fire" computed client-side via `cron-parser`.

### System
Health banner (color + dot + copy from `/v1/system/health`). KPI strip (DB / active runs / triggers / nodes). Nodes table with role badges, arch, CPU/mem usage bars, workers ratio. Cluster topology mini-map (SVG ring; pure presentation from `nodes[]`). Operator tools row (existing DB console + log console + cache-prune dialog). Health checks list. Prometheus metrics reference.

### JobDefs
Three tabs: Editor / Diff / History. Editor uses CodeMirror 6 with `@codemirror/lang-yaml` and a server-side lint provider. Lint footer shows error count + step count + lint duration. Diff tab is a typed changeset against current server state. History tab is the apply audit log per alias.

## Tweaks triage

The prototype's Tweaks panel is exploration scaffolding, not a settings UI to ship.

| Tweak              | Decision                                                                          |
| ------------------ | --------------------------------------------------------------------------------- |
| Density            | Ship as a user preference. Settings page exposes Compact / Regular / Cozy.        |
| Accent             | Drop. One brand, one accent.                                                      |
| Badge style        | Drop. Pick `filled` and commit.                                                   |
| Sparklines on/off  | Ship as a user preference (some operators find them noisy at scale).              |
| Logo animation     | Auto-disable when `prefers-reduced-motion: reduce`. No setting needed.            |
| Row hover          | Drop. Pick `subtle` (the gradient row-hover) and commit.                          |

User preferences land at `GET/PATCH /v1/users/me/preferences`; for now we can persist locally in `localStorage` until that endpoint exists.

## API surface gaps

The refresh assumes the following backend endpoints. Some exist; some are net-new and should be specified before the page that depends on them ships. The execution plan lists the dependency chain.

| Endpoint                                       | Status                              | Notes                                                                 |
| ---------------------------------------------- | ----------------------------------- | --------------------------------------------------------------------- |
| `GET /v1/jobs?count_only=true`                 | new (cheap)                         | Sidebar nav badges. Optional optimization.                            |
| `GET /v1/jobs/summary`                         | new                                 | Status counts for the Jobs filter bar (avoid client-side counting).   |
| Job DTO `lastRuns: RunSummary[]` (last 14)     | extend                              | Avoid per-row sparkline fetches.                                      |
| `GET /v1/stats/summary?window=7d|30d`          | extend existing stats endpoint      | KPI strip + dual-axis chart; one query per range.                     |
| `GET /v1/system/health`                        | new                                 | Banner + KPI strip + checks list.                                     |
| `GET /v1/system/nodes`                         | new                                 | Nodes table + topology mini-map.                                      |
| `GET /v1/system/features`                      | new                                 | Toggles for `CAESIUM_DATABASE_CONSOLE_ENABLED` etc.                   |
| `POST /v1/system/cache/prune`                  | new                                 | Existing maintenance op; needs a button.                              |
| `POST /v1/jobdefs/lint`                        | new                                 | Structured `{ errors, warnings, summary }`. Debounced from CodeMirror. |
| `POST /v1/jobdefs/diff`                        | new                                 | Structured changeset (add/modify/remove with paths).                  |
| `GET /v1/jobdefs/history?alias=…`              | new (v1.1 follow-up)                | Apply audit log. Tab can ship empty until ready.                      |
| Job DTO `nextRunAt`                            | optional                            | Otherwise compute on client via `cron-parser`.                        |

These shape the work — none of them is on the critical path for Phase 1 (Foundations). The plan calls out which endpoint blocks which phase.

## Accessibility floor

- AA contrast across the palette in both themes. Verified before each phase ships.
- Keyboard nav, focus rings, and ARIA roles flow through shadcn primitives — anything we hand-roll (status badge, sparkline, DAG node) needs explicit keyboard handling.
- `prefers-reduced-motion: reduce` disables the atom logo animation, the running scan-line, and the SSE row pulse.
- `axe-core` runs in CI before each phase merges.

## Rollout

Four phases, each its own PR train. Details in [`ui-refresh-execution-plan.md`](ui-refresh-execution-plan.md).

1. **Foundations.** Tokens, status semantics, primitives, AppShell. Visible change: existing pages take on the refreshed look immediately.
2. **High-traffic paths.** Jobs list, Job Detail, Run Detail.
3. **Lower-traffic paths.** Stats, Triggers.
4. **Net-new server work.** System, JobDefs. Behind the `CAESIUM_UI_REFRESH_V2_SYSTEM` feature flag until the new endpoints stabilize.

No "big bang" merge. Each phase is independently shippable, independently revertible.

## Related Documents

- [`ui-refresh-execution-plan.md`](ui-refresh-execution-plan.md) — file-by-file phase breakdown
- [`design/ui-refresh/`](design/ui-refresh/) — prototype source (JSX, CSS, mock fixtures, standalone HTML)
- [`design/ui-refresh/original-implementation-plan.md`](design/ui-refresh/original-implementation-plan.md) — the design system's original plan (pre-adaptation)
- [`ui_implementation_plan.md`](ui_implementation_plan.md) — the v1 UI plan; closed for the visual layer, still authoritative for shipped feature scope
- [`roadmap.md`](roadmap.md) — strategic priorities; UI Refresh is a Phase 2 entry
