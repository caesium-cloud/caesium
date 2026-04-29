# Caesium Web UI Implementation Plan

> Status: Closed. This document is the historical record of the v1 feature scope and the 2026-04 UI refresh. No new work tracked here.

## Current State

The embedded UI is already in production shape for the core operator workflow:

- React + TypeScript + Vite app under `ui/`
- Embedded into the Go binary with `go:embed`
- TanStack Router app shell and feature routes
- Jobs list, Job Detail, Run Detail, Stats, Triggers, Atoms, JobDefs, and System pages
- SSE-backed live run updates
- React Flow DAG rendering
- xterm-based task log streaming and retained log viewing
- Backfill, pause/unpause, trigger run, callback retry, and job definition apply surfaces
- System log console and database console operator tools

## Completed Work

### Phase 1 Core UI

- [x] Embedded web UI with SPA fallback and Docker build integration
- [x] Jobs list with live updates and trigger controls
- [x] Job Detail DAG view and task metadata inspection
- [x] Run Detail live DAG updates and log access
- [x] Task log streaming with retained-log fallback
- [x] Operator actions for pause/unpause, trigger, backfill, and callback retry
- [x] Command palette, keyboard shortcuts, theme support, and general UI polish

### UI Refresh (2026-04) — Shipped

Full visual-layer redesign shipped across PRs [#146](https://github.com/caesium-cloud/caesium/pull/146), [#147](https://github.com/caesium-cloud/caesium/pull/147), and [#148](https://github.com/caesium-cloud/caesium/pull/148).

- [x] Design tokens, status semantics, `<AtomLogo>`, and UI primitives (Phase 0)
- [x] App shell visual upgrade: animated sidebar, breadcrumb header, UTC clock (Phase 0)
- [x] Jobs list, Job Detail + DAG, Run Detail + log viewer (Phase 1)
- [x] Stats page, Triggers page (Phase 2)
- [x] System page with cluster topology, JobDefs editor with CodeMirror lint/diff (Phase 3)

### Phase 2 Work Already Landed

- [x] Recharts adoption for the stats surface
- [x] Success-rate trend chart
- [x] Failure-distribution chart
- [x] Trigger and atom inspection in Job Detail
- [x] Airflow-parity UI for paused jobs, run params, retries, trigger rules, and node selectors
- [x] Baseline frontend test coverage for `StatsPage`, `TaskNode`, API helpers, SSE helpers, and key operator panels

## Remaining Gaps

### 1. Stats Surface

- [x] Add daily run-volume data to the stats API (`run_count` on each daily trend entry)
- [x] Add a Daily Run Volume chart to the Stats page
- [ ] Decide whether the slowest-jobs table should remain as-is or be promoted to a chart in a future pass

### 2. Component Coverage

- [x] Add focused `JobDAG` tests covering:
  - node type mapping
  - status normalization
  - selected-node state
  - output-bearing and contract-bearing edges
- [x] Expand `TaskNode` coverage only for behavior not already tested
- [ ] Add equivalent focused tests for any future custom DAG edge or branch-node behavior that becomes more complex

### 3. Browser E2E

- [x] Add Playwright coverage for the critical operator flow:
  - apply a fixture job
  - trigger a run
  - observe live run-state updates
  - open the Run Detail page
  - click a task node
  - verify live logs
  - verify retained logs after completion
- [x] Add a dedicated `ui-e2e` workflow in CircleCI
- [x] Add a local developer entrypoint via `just ui-e2e`

## Acceptance Criteria

The remaining UI plan is considered closed when:

- stats payloads continue to include a full seven-day window with both `success_rate` and `run_count`
- Stats page renders both trend and volume charts without breaking existing summary tables
- DAG component tests cover node and edge mapping logic without relying on fragile DOM internals
- Playwright runs against a real Caesium instance and verifies live plus retained task logs
- documentation and contributor guidance describe the shipped UI rather than the original scaffold plan
