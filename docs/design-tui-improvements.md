# Design: TUI Improvements

## Status

Draft

## Problem

The Jobs page in the caesium console is polished. It has a rich detail view with DAG visualization, live log streaming, run history, node inspection, filtering, and real-time status polling. The other three pages (Triggers, Atoms, Stats) are barebones read-only tables that do not offer meaningful operator value.

An operator opening the console today can deeply inspect jobs, but if they want to understand why a job is scheduled (trigger config), what it runs (atom details), or get a fleet-wide operational picture (stats), they hit dead ends.

## Current State

### What works well (Jobs page)

- Table with status, last run time, duration, labels
- `enter` opens a rich detail view with metadata, run status, progress bar, and DAG
- DAG navigation with `left`/`right`/`tab`, focus path with `f`
- `u` opens run history with rerun support
- `g` opens live log streaming with filter, follow, and export
- Node detail modal with engine, image, command, predecessors, and successors
- Filter by alias or labels with `/`
- Trigger jobs with `t`
- Two-second auto-refresh for running jobs
- Spinner animations on active tasks

### What does not work well

| Page | Current state | Issues |
|---|---|---|
| **Triggers** | 3-column table: Alias, Type, ID | No detail view. No way to see cron expression, timezone, associated jobs, next fire time, or fire history. |
| **Atoms** | 3-column table: Image, Engine, ID | No detail view. No way to see command, mounts, env vars, or which jobs use this atom. |
| **Stats** | Overview + top failing + slowest jobs | Static snapshot, no drill-down, no time-series view, and no per-job stats. |

### Structural observation

The four-tab layout creates a false equivalence between these pages. Jobs is a full operational workspace. Triggers and Atoms are reference data that make more sense in context when you are already looking at a job. Stats could be useful, but needs to be interactive.

## Design Philosophy

The console should be organized around operator workflows, not data models.

An operator's mental model is:

1. What is happening right now?
2. Tell me about this job.
3. Why did this fail?
4. What is the overall health?
5. When does this run next?
6. What container does this task run?

Triggers and Atoms are supporting details, not top-level destinations. Stats should become a proper operational dashboard.

## Proposed Tab Structure

### Option A: Consolidate to 3 tabs

```text
1 Dashboard      2 Jobs      3 Activity
```

- **Dashboard**: Fleet-wide operational overview with drill-down.
- **Jobs**: Primary workspace. Trigger and atom details appear within the job detail view.
- **Activity**: Live feed of recent events such as runs starting, completing, triggers firing, and failures.

### Option B: Keep 4 tabs but improve them

```text
1 Jobs      2 Triggers      3 Activity      4 Dashboard
```

Keep Triggers as a standalone page, replace Atoms with Activity, and upgrade Stats to Dashboard.

Option A remains the better direction because it matches how operators actually debug systems.

## Phase Plan

### Phase 1: Enrich existing detail views

- Expand the job detail header to show trigger schedules, timezone, last fire, and next fire time.
- Expand the node detail modal to show env vars, mounts, resource limits, and provenance.
- Improve atom disambiguation by showing command previews and provenance hints.
- Add cross-references that let operators jump from related views back into the relevant job.

### Phase 2: Restructure tabs

- Add an Activity tab backed by recent run and trigger events.
- Remove standalone Triggers and Atoms tabs once their detail is visible in-context.
- Update key bindings and tab numbering to match the new layout.

### Phase 3: Replace Stats with a dashboard

- Add Fleet Health, Active Now, Triggers, and Recent Failures sections.
- Support drill-down from each section into job detail.
- Use a responsive layout that stacks on narrow terminals.

### Phase 4: Quality-of-life improvements

- Global search across jobs and events.
- Breadcrumb navigation for deep drill-downs.
- Context-sensitive help.
- Notification badges when failures happen off-screen.
- Time-range selection for dashboard data.

## Risks and Mitigations

| Risk | Mitigation |
|---|---|
| Removing Triggers and Atoms tabs confuses existing users | Surface the same data more prominently in job and node detail views. |
| Activity feed requires too much polling | Derive recent events from existing data, cap volume, and move to server-side streaming later. |
| Dashboard is too dense for small terminals | Detect terminal width and stack sections vertically. |
| Next-fire computation drifts from server behavior | Use the same cron parser as the backend where possible. |

## Non-Goals

- A browser dashboard. This document is about the terminal console.
- Full CRUD for atoms. Atoms are defined in job specs, not managed independently.
- Historical charting beyond lightweight summaries. Detailed charting belongs in the web UI or external observability tools.
