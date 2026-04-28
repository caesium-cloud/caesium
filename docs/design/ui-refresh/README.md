# UI Refresh — Reference Prototype

> Status: Reference material. Not shipping code. The execution plan in [`docs/ui-refresh-execution-plan.md`](../../ui-refresh-execution-plan.md) is the source of truth for what gets built; this directory is what an agent reads to understand the visual intent.

The files here came from a "Claude Design" exploration of a console refresh. They use raw `style={{}}` and CSS custom properties to move fast — they are deliberately *not* a port target. The production work re-expresses every visual decision through the existing design tokens (`ui/src/index.css`) and shadcn-style primitives (`ui/src/components/ui/`).

## What's in this folder

- `prototype/index.html` — entry point that loads each module against React via Babel-in-browser. Open it in any static server to see the design live.
- `prototype/standalone.html` — single-file build of the same prototype (1.5 MB). Use this if you need to share the design without a local dev server.
- `prototype/styles.css` — design tokens and global styles. The token names that survive into production (`--cyan`, `--gold`, `--surface-*`, `--text-1..4`) are documented in [`docs/design-ui-refresh.md`](../../design-ui-refresh.md).
- `prototype/ui-kit.jsx` — shared components: animated `AtomLogo`, lucide-style `I` icon set, `StatusBadge`, `Sparkline`, `Btn`, `ToastProvider`, `UTCClock`, `EmptyState`, time helpers.
- `prototype/layout.jsx` — sidebar (with brand mark, nav, cluster footer) and header (breadcrumb, command bar trigger, UTC clock, theme toggle, notifications).
- `prototype/pages-jobs.jsx` — Jobs list and Job Detail pages (includes the DAG layout helper).
- `prototype/pages-other.jsx` — Stats, Triggers, Run Detail (Gantt + log viewer).
- `prototype/pages-system.jsx` — System (health banner, KPI strip, nodes table, quorum mini-map, operator tools, health checks, Prometheus reference) and JobDefs (editor, lint, diff, history, schema reference).
- `prototype/data.js` — mock fixtures (jobs, DAG, triggers, activity feed, stats, log lines, system) — read this to understand what the UI assumes the backend returns.
- `prototype/tweaks-panel.jsx` — design exploration tool (density / accent / badge style / etc.). Most of these are not shipping; see §9 of the original plan and §"Tweaks triage" in the design doc.
- `original-implementation-plan.md` — the plan as authored by the design system. We adapted it (with real Caesium file paths, real query hook names, real existing primitives, real API gaps) into [`docs/ui-refresh-execution-plan.md`](../../ui-refresh-execution-plan.md). Keep this around for "what did the design system originally suggest?" lookups.

## Brand assets

The animated atom motif in the prototype is the same shape as the existing `ui/src/components/caesium-logo.tsx`. The only delta is animation (orbit spin + nucleus pulse). The standalone SVGs in the zip (`brand/caesium-icon.svg`, `brand/caesium-logo-{dark,light}.svg`) are formatting-only differences from the SVGs already in `/brand` — no copy required.

## How to use this material

When working a phase in the execution plan:

1. Open `prototype/index.html` in a browser to see the page in motion.
2. Read the matching `prototype/pages-*.jsx` for layout structure, spacing, and color usage.
3. Translate visual decisions through tokens (`hsl(var(--cyan))` → `text-caesium-cyan` / `bg-primary` / etc.) and existing primitives (`Btn` → existing `<Button>`, `StatusBadge` → new `components/ui/status-badge.tsx`).
4. Check `data.js` to confirm what shape the API needs to return.

If a prototype file disagrees with the design doc, the design doc wins. If the design doc disagrees with the execution plan, the execution plan wins.
