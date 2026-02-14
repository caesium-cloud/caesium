# Design: TUI Improvements

## Status

Draft

## Problem

The Jobs page in the caesium console is polished — it has a rich detail view with DAG visualization, live log streaming, run history, node inspection, filtering, and real-time status polling. The other three pages (Triggers, Atoms, Stats) are barebones read-only tables that don't offer meaningful operator value. They feel like afterthoughts.

An operator opening the console today can deeply inspect jobs, but if they want to understand *why* a job is scheduled (trigger config), *what* it runs (atom details), or get a fleet-wide operational picture (stats), they hit dead ends. The pages exist but don't do anything useful.

## Current State

### What works well (Jobs page)

- Table with status, last run time, duration, labels
- `enter` → rich detail view: metadata, run status, progress bar, DAG
- DAG navigation with `←/→/tab`, focus path with `f`
- `u` → run history modal with rerun support
- `g` → live log streaming with filter, follow, export
- Node detail modal with engine/image/command/predecessors/successors
- Filter by alias or labels with `/`
- Trigger jobs with `t` (confirmation modal)
- 2-second auto-refresh for running jobs
- Spinner animations on active tasks

### What doesn't work (everything else)

| Page | Current State | Issues |
|---|---|---|
| **Triggers** | 3-column table: Alias, Type, ID | No detail view. No way to see cron expression, timezone, associated jobs, next fire time, or fire history. Can't fire a trigger manually. |
| **Atoms** | 3-column table: Image, Engine, ID | No detail view. No way to see command, spec, mounts, env vars, or which jobs use this atom. No grouping — 24 rows of `busybox:1.36` and `alpine:3.20` with no way to tell them apart. |
| **Stats** | Overview + top failing + slowest jobs | Static snapshot, no drill-down. Can't click a failing job to investigate. No time-series view. No per-job stats. Missing: trigger fire rate, task duration trends, queue depth. |

### Structural observation

The 4-tab layout creates a false equivalence between these pages. Jobs is a full operational workspace. Triggers and Atoms are reference data that makes more sense *in context* (when you're looking at a job) than as standalone pages. Stats could be genuinely useful but needs to be interactive.

## Design Philosophy

**The console should be organized around operator workflows, not data models.**

An operator's mental model is:
1. "What's happening right now?" → Dashboard / live status
2. "Tell me about this job" → Job detail (already great)
3. "Why did this fail?" → Logs + DAG + run history (already great)
4. "What's the overall health?" → Fleet stats with drill-down
5. "When does this run next?" → Trigger context (currently missing)
6. "What container does this task run?" → Atom context (currently missing)

Triggers and Atoms are supporting details, not top-level destinations. Stats should be a proper operational dashboard.

## Proposed Tab Structure

### Option A: Consolidate to 3 tabs (recommended)

```
1 Dashboard      2 Jobs      3 Activity                                    Cs ·caesium
```

- **Dashboard** — Replaces Stats. Fleet-wide operational overview with drill-down.
- **Jobs** — Unchanged as the primary workspace. Trigger and atom details are surfaced within the job detail view (already partially done — trigger is shown in the detail header).
- **Activity** — New. Live feed of recent events: runs starting/completing, triggers firing, failures. Think `kubectl get events` for caesium.

Remove Triggers and Atoms as standalone tabs. Their data is accessible via:
- Job detail view already shows trigger type + alias in the header
- Node detail modal already shows atom image + engine + command
- We enhance both to show the full picture (see Phase 2)

### Option B: Keep 4 tabs but make them useful

```
1 Jobs      2 Triggers      3 Activity      4 Dashboard                   Cs ·caesium
```

Keep Triggers as a standalone page (with a proper detail view), replace Atoms with Activity, upgrade Stats to Dashboard.

**Recommendation: Option A.** Three focused tabs > four mediocre ones. Triggers and atoms are job metadata — forcing operators to switch tabs to cross-reference a trigger's cron expression with a job breaks flow. Surfacing this data inline is better UX.

## Design — Phase by Phase

### Phase 1: Enrich Existing Detail Views (no structural changes)

Improve the information density of views that already exist, before restructuring tabs.

#### 1.1 — Trigger detail in job detail view

**File:** `cmd/console/ui/detail/view.go` — `renderHeader()`

Currently the header shows:
```
nightly-etl
ID: b93cdfea  •  Trigger: cron (nightly-etl)
```

Expand to show the full trigger configuration inline:

```
nightly-etl
ID: b93cdfea  •  Trigger: cron (nightly-etl)

  Schedule:  0 2 * * *  (America/New_York)
  Next fire: 2025-07-15 02:00:00 EDT  (in 6h 23m)
  Last fire: 2025-07-14 02:00:00 EDT
```

For HTTP triggers:
```
  Trigger:  http (http-ops-debug)
  Endpoint: PUT /v1/triggers/aaca58d6
```

**Data needed:** Parse `trigger.Configuration` JSON (already available in `JobDetail.Trigger`). Compute next fire time client-side using the cron expression (same parser as server). Add `nextFireTime()` helper to `cmd/console/api/` or a new `cmd/console/ui/cron/` package.

#### 1.2 — Richer atom/node detail modal

**File:** `cmd/console/app/model.go` — node detail modal rendering (`view.go:637-750`)

Currently shows: status, image, engine, command, start time, duration, predecessors, successors, atom ID.

Add:
- **Environment variables** from atom spec (masked values for secrets)
- **Mounts** from atom spec (source → target, read-only flag)
- **Resource limits** if set (CPU, memory)
- **Provenance** — which git repo/ref/path defined this atom

This data is already available via `client.Atoms().Get()` and `preloadAtomMetadata()`.

#### 1.3 — Atom disambiguation in tables and DAG

The Atoms page shows 24 rows of `busybox:1.36` and `alpine:3.20` with no way to tell them apart. Even in the DAG, node boxes show `image + command` but tasks within the same job often use the same image.

**Solution:** Show a truncated command preview as a second line in atom tables, and use the atom's provenance path as a disambiguator:

```
busybox:1.36  •  sh -c "echo hello"
alpine:3.20   •  /scripts/transform.sh
```

In the DAG, this is already done (command preview in node box). No change needed there.

#### 1.4 — Clickable cross-references

When viewing a job detail, allow navigating to related resources:
- In the runs modal (`u`), pressing `enter` on a run already switches the active run. Good.
- In the node detail modal, add `[j]` to jump to the parent job (useful if you navigated here from Activity feed).
- In stats drill-down (Phase 3), clicking a failing job opens its detail view.

### Phase 2: Restructure Tabs

#### 2.1 — Replace "Triggers" and "Atoms" tabs with "Activity" tab

**New tab: Activity**

A reverse-chronological feed of cluster events. Operators need to answer "what just happened?" without drilling into individual jobs.

```
1 Dashboard      2 Jobs      3 Activity                                    Cs ·caesium
──────────────────────────────────────────────────────────────────────────────────────────
  12 events (last hour)
 ╭──────────────────────────────────────────────────────────────────────────────────────╮
 │ Time         Event              Job                  Detail                          │
 │──────────────────────────────────────────────────────────────────────────────        │
 │ 19:34:02     ✓ run completed    nightly-etl          3/3 tasks  30.7s               │
 │ 19:33:31     ▸ run started      nightly-etl          triggered by cron              │
 │ 19:33:30     ⚡ trigger fired    nightly-etl          cron: 0 */30 * * *            │
 │ 19:32:15     ✓ run completed    csv-to-parquet       2/2 tasks  19.1s               │
 │ 19:31:56     ▸ run started      csv-to-parquet       triggered by cron              │
 │ 19:20:44     ✗ run failed       cron-failure-fast    task extract failed (exit 1)   │
 │ 19:20:30     ▸ run started      cron-failure-fast    triggered by cron              │
 │ 19:20:30     ⚡ trigger fired    cron-failure-fast    cron: */10 * * * *            │
 │ ...                                                                                  │
 ╰──────────────────────────────────────────────────────────────────────────────────────╯
 [1/2/3] switch  [tab] cycle  [r] reload  [enter] jump to job  [/] filter  [q] quit
```

**Data source:** Construct events from existing run data. Poll `GET /v1/jobs` + latest runs. Each run start/complete is an event. Each trigger fire (inferred from run creation) is an event. No new API endpoint needed initially — we can derive events client-side from the run list.

Future: Add a server-side `/v1/events` endpoint for efficient event streaming.

**Interactions:**
- `enter` on an event → jump to the corresponding job's detail view (switches to Jobs tab + opens detail)
- `/` filter by job alias, event type, or status
- Auto-refresh: poll every 5 seconds (same pattern as jobs page)

#### 2.2 — Remove standalone Triggers and Atoms tabs

**Files to modify:**
- `cmd/console/app/model.go` — Remove `sectionTriggers` and `sectionAtoms` from section enum. Update tab switching logic.
- `cmd/console/app/view.go` — Update `sectionNames`, `renderTabsBar()`, tab key handlers.
- `cmd/console/app/tables.go` — Keep trigger/atom row formatters (used in detail views), remove standalone table setup.

**Migration of trigger/atom data:**
- Trigger details → shown inline in job detail header (Phase 1.1)
- Atom details → shown in node detail modal (Phase 1.2)
- Trigger list → accessible via Dashboard "Triggers" section (Phase 3)
- Atom list → not needed as standalone; atoms are always viewed in context of a task

#### 2.3 — Update tab numbering and key bindings

```
1 Dashboard      2 Jobs      3 Activity
```

Update:
- `1/2/3` keys map to new tabs
- `tab/shift+tab` cycles 3 tabs
- Remove `4` key binding
- Update help modal

### Phase 3: Dashboard (replacing Stats)

#### 3.1 — Redesigned dashboard layout

The current Stats page is a static text dump. Redesign as a multi-section operational dashboard:

```
1 Dashboard      2 Jobs      3 Activity                                    Cs ·caesium
──────────────────────────────────────────────────────────────────────────────────────────
┌─ Fleet Health ──────────────────────────┐  ┌─ Active Now ────────────────────────────┐
│                                         │  │                                         │
│  Jobs: 8    Triggers: 8    Atoms: 24    │  │  ● nightly-etl         2/3 tasks  12s   │
│                                         │  │  ● csv-to-parquet      1/2 tasks   4s   │
│  Last 24h                               │  │                                         │
│  Runs:  42   ✓ 38  ✗ 4                 │  │  2 jobs running, 5 tasks active          │
│  Rate:  90%  ████████████████████░░     │  │                                         │
│  Avg:   28s  p95: 52s  p99: 94s        │  └─────────────────────────────────────────┘
│                                         │
└─────────────────────────────────────────┘  ┌─ Recent Failures ──────────────────────┐
                                             │                                         │
┌─ Triggers ──────────────────────────────┐  │  ✗ cron-failure-fast   19:20  exit 1    │
│                                         │  │  ✗ cron-failure-fast   18:10  exit 1    │
│  nightly-etl    cron  0 2 * * *   6h    │  │  ✗ callback-fail-demo  17:45  timeout   │
│  csv-to-parq…   cron  */30 * * *  12m   │  │  ✗ cron-failure-fast   16:00  exit 1    │
│  cron-success    cron  */15 * * *  3m   │  │                                         │
│  http-ops-deb…  http  PUT /v1/tri…  -   │  │  [enter] investigate                    │
│                                         │  │                                         │
└─────────────────────────────────────────┘  └─────────────────────────────────────────┘
```

**Sections:**

1. **Fleet Health** (top-left)
   - Object counts: jobs, triggers, atoms
   - 24h run summary: total, succeeded, failed
   - Success rate with visual bar
   - Duration stats: avg, p95, p99

2. **Active Now** (top-right)
   - Currently running jobs with task progress and elapsed time
   - Auto-refreshes with 2-second poll
   - If nothing running: "All quiet ✓"

3. **Triggers** (bottom-left)
   - All triggers with type, expression/endpoint, and time-until-next-fire
   - Sorted by next fire time (soonest first)
   - This replaces the standalone Triggers tab

4. **Recent Failures** (bottom-right)
   - Last N failed runs with job alias, time, and error summary
   - `enter` to jump to job detail view

**Interactions:**
- Arrow keys navigate between sections
- `enter` in Active Now → jump to that job's detail
- `enter` in Recent Failures → jump to that job's detail with the failed run selected
- `enter` in Triggers → jump to the associated job's detail
- `r` to reload all sections

#### 3.2 — Dashboard data model

**File:** `cmd/console/app/dashboard.go` (new)

```go
type DashboardModel struct {
    focusedSection int  // 0=health, 1=active, 2=triggers, 3=failures

    // Fleet health
    stats      *api.StatsResponse

    // Active now
    activeJobs []activeJob  // derived from jobs with running status

    // Triggers
    triggers   []triggerRow  // trigger + computed next fire time

    // Recent failures
    failures   []failureRow  // recent failed runs
}
```

**API changes needed:** Enhance `/v1/stats` to include p95/p99 duration, or compute client-side from run history. Add `nextFireTime` computation client-side (parse cron expression with `robfig/cron`).

#### 3.3 — Responsive layout

The dashboard uses a 2x2 grid on wide terminals (>120 cols) and stacks vertically on narrow terminals. Use `lipgloss.JoinHorizontal` and `lipgloss.JoinVertical` with width detection.

### Phase 4: Quality-of-Life Improvements

#### 4.1 — Global search (`/` from any tab)

Unify the filter experience. `/` from Dashboard or Activity searches across all jobs and events. Results are a combined list that you can `enter` to navigate to.

#### 4.2 — Breadcrumb navigation

When jumping from Dashboard → Job Detail → Log View, show a breadcrumb trail:

```
Dashboard > nightly-etl > Run a1b2c3 > Task extract (logs)
```

`esc` pops one level. This makes deep navigation less disorienting.

#### 4.3 — Keyboard shortcut overlay

The `?` help modal currently exists. Enhance it to be context-sensitive — show only the shortcuts relevant to the current view (detail, modal, list, dashboard section).

#### 4.4 — Notification badge on tab

When a job fails while you're on another tab, show a badge:

```
1 Dashboard      2 Jobs (!)      3 Activity
```

Clear when the user visits the tab.

#### 4.5 — Time range selector for stats

Allow switching the Dashboard time window: `1h / 6h / 24h / 7d`. Use `[` and `]` keys to cycle.

## Implementation Plan

### Phase 1 — Enrich Existing Views (no structural changes)

- [ ] **1.1** Parse trigger configuration JSON in detail view and display cron schedule, timezone, next fire time inline
- [ ] **1.2** Add cron expression parser to console (reuse `robfig/cron` or add lightweight client-side parser) for next-fire-time computation
- [ ] **1.3** Expand node detail modal to show environment variables, mounts, resource limits from atom spec
- [ ] **1.4** Show provenance (git repo/ref/path) in node detail modal for atoms
- [ ] **1.5** Add command preview as secondary line in atom-related displays for disambiguation

### Phase 2 — Restructure Tabs

- [ ] **2.1** Create Activity tab data model and event derivation logic (from run start/complete times)
- [ ] **2.2** Implement Activity tab view: reverse-chronological event table with icons
- [ ] **2.3** Add Activity tab interactions: `enter` to jump to job detail, `/` to filter, auto-refresh
- [ ] **2.4** Remove `sectionTriggers` and `sectionAtoms` from section enum and tab bar
- [ ] **2.5** Update tab numbering: `1` Dashboard, `2` Jobs, `3` Activity
- [ ] **2.6** Update help modal and footer key hints for new tab structure
- [ ] **2.7** Implement cross-tab navigation: `enter` on Activity/Dashboard event → Jobs tab + detail view

### Phase 3 — Dashboard

- [ ] **3.1** Create `cmd/console/app/dashboard.go` with `DashboardModel` and 4-section layout
- [ ] **3.2** Implement Fleet Health section (reuse existing stats API + add p95/p99)
- [ ] **3.3** Implement Active Now section (derive from running jobs, 2s auto-refresh)
- [ ] **3.4** Implement Triggers section (all triggers with next-fire-time, sorted by soonest)
- [ ] **3.5** Implement Recent Failures section (last N failed runs with error summary)
- [ ] **3.6** Add section-level focus navigation (arrow keys between quadrants)
- [ ] **3.7** Add drill-down from each Dashboard section to Jobs detail view
- [ ] **3.8** Implement responsive layout: 2x2 grid on wide terminals, vertical stack on narrow
- [ ] **3.9** Replace old `stats_pane.go` with new dashboard renderer

### Phase 4 — Quality of Life

- [ ] **4.1** Add breadcrumb trail showing navigation depth (Dashboard > Job > Run > Task)
- [ ] **4.2** Add notification badge on Jobs tab when failures occur while on another tab
- [ ] **4.3** Make `?` help modal context-sensitive (show shortcuts for current view only)
- [ ] **4.4** Add time range selector for Dashboard stats (`[`/`]` to cycle 1h/6h/24h/7d)
- [ ] **4.5** Implement global search (`/` from Dashboard/Activity searches jobs + events)

## Risks and Mitigations

| Risk | Mitigation |
|---|---|
| Removing Triggers/Atoms tabs confuses existing users | Trigger data is more visible than before (inline in detail, Dashboard section). Atom data accessible via node detail. Net improvement in discoverability. |
| Activity feed requires polling many runs | Derive events from existing job list + latest run data (already fetched). Cap to last 50 events. Future: server-side `/v1/events` endpoint. |
| Dashboard layout complexity on small terminals | Detect terminal width; stack vertically below 120 cols. Each section renders independently. |
| Next-fire-time computation drift | Compute client-side using same `robfig/cron` parser as server. Refresh on each poll cycle. Acceptable drift is <poll interval. |
| Cross-tab navigation breaks back-button mental model | Breadcrumb trail (Phase 4.1) makes navigation stack explicit. `esc` always pops one level. |

## Non-Goals

- **Web UI** — The console is a terminal tool. No browser dashboard.
- **Write operations beyond trigger** — The console is primarily for observability. Job creation/editing stays in YAML + git sync.
- **Historical trend charts** — Terminal rendering of time-series charts is limited. Show numeric summaries instead. Save charting for a future web UI or Grafana integration via Prometheus metrics.
- **Atom CRUD** — Atoms are defined in job specs, not managed independently. No standalone atom management UI needed.
