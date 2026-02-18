# Caesium Web UI Implementation Plan

## React + TypeScript + Vite + Tailwind + shadcn/ui + TanStack + React Flow + SSE

This document defines a **step-by-step implementation plan** for
building the Caesium Web UI.

It is designed to be executed iteratively by autonomous coding agents.

------------------------------------------------------------------------

# 0. Goals [COMPLETED]

Build a modern, operator-grade UI for Caesium with:

-   [x] Real-time updates via **SSE (first-class, v1)**
-   [x] Interactive DAG visualization
-   [x] Run inspection with status overlays
-   [x] Live log streaming
-   [x] Operator actions (trigger run, retry callbacks)
-   [x] Beautiful, polished UI (Akuity-style)

No authentication or multi-tenancy in v1.

------------------------------------------------------------------------

# 1. Repository Structure [COMPLETED]

Create a `ui/` directory inside the Caesium monorepo:

    /ui
      /src
        /app
        /components
        /features
        /lib
      index.html
      vite.config.ts
      tailwind.config.ts
      tsconfig.json
      package.json

UI is embedded into the Go binary using `go:embed`.

------------------------------------------------------------------------

# 2. Project Scaffold [COMPLETED]

- [x] Create Vite App (React + TS)
- [x] Install Core Dependencies (TanStack, React Flow, Lucide, xterm, etc.)
- [x] Configure Tailwind and PostCSS
- [x] Setup shadcn/ui and base components

------------------------------------------------------------------------

# 3. Core Architecture [COMPLETED]

## 3.1 Routing (TanStack Router)

Routes implemented:

-   `/jobs`
-   `/jobs/:jobId`
-   `/jobs/:jobId/runs/:runId`
-   `/triggers`
-   `/atoms`
-   `/stats`

- [x] Create `AppShell` layout with sidebar + header.

------------------------------------------------------------------------

# 4. API Layer [COMPLETED]

- [x] Create `src/lib/api.ts`.
- [x] Base Fetch Wrapper with error handling.
- [x] TypeScript types matching REST models (Job, Run, Task, Atom, etc.).

------------------------------------------------------------------------

# 5. SSE Integration [COMPLETED]

- [x] Define Event Types matching backend schema.
- [x] Create SSE Client Manager (`src/lib/events.ts`) with reconnect and subscription logic.
- [x] React Query Integration for granular cache invalidation.
- [x] Persistent Global SSE connection initialized at root.

------------------------------------------------------------------------

# 6. Jobs List Page [COMPLETED]

- [x] Fetch `/jobs`
- [x] Display in `shadcn` Table
- [x] Add "Trigger Run" action with toast notifications.
- [x] Real-time status and duration updates via SSE.

------------------------------------------------------------------------

# 7. Job Detail Page [COMPLETED]

- [x] DAG Rendering with React Flow + Dagre layout.
- [x] Node status styling (Running, Completed, Failed).
- [x] Tabbed view (DAG, Runs, Definition).
- [x] Integrated real-time DAG updates.

------------------------------------------------------------------------

# 8. Run Detail Page [COMPLETED]

- [x] Real-time status mapping from SSE events.
- [x] Interactive DAG with log access via node click.
- [x] Live run/task state visualization.

------------------------------------------------------------------------

# 9. Log Streaming [COMPLETED]

- [x] Use existing REST streaming endpoint (`/v1/jobs/:id/runs/:run_id/logs`).
- [x] Render with `xterm.js` for high-performance terminal output.
- [x] Integrated into DAG visualization on the Run Detail page.

------------------------------------------------------------------------

# 10. UI Polish [COMPLETED]

- [x] Toast notifications (Sonner)
- [x] Loading states (Skeletons)
- [x] Keyboard shortcuts (`g+j`, `g+t`, etc.)
- [x] Command palette (`cmd+k`)
- [x] Smooth transitions (Fade-in animations)
- [x] Native Dark Mode (default) with persistent theme switcher.
- [x] Advanced DAG Visualization (engine icons, YAML command lists, live errors).

------------------------------------------------------------------------

# 11. Embedding in Go Binary [COMPLETED]

- [x] Multi-stage Docker build producing `ui/dist`.
- [x] Go `//go:embed` integration in `ui/embed.go`.
- [x] Echo v5 route registration and SPA fallback in `api/ui.go`.

------------------------------------------------------------------------

# 12. Implementation Order Summary

1.  [x] Scaffold + Tailwind + shadcn
2.  [x] Router + AppShell
3.  [x] API client
4.  [x] SSE client manager
5.  [x] Jobs list
6.  [x] DAG view
7.  [x] Run detail + SSE updates
8.  [x] Log viewer
9.  [x] Polish
10. [x] Embed build

------------------------------------------------------------------------

# 13. Definition of Done [100% COMPLETED]

- [x] Jobs list works with real-time updates.
- [x] DAG fully interactive and descriptive.
- [x] Run page updates in real time (SSE).
- [x] Logs stream live with high performance.
- [x] Actions (Trigger/Search) reflected instantly.
- [x] Modern, polished "Operator-Grade" UI.
- [x] Build artifacts embedded in Echo v5 backend.
