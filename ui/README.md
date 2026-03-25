# Caesium UI

This package contains the embedded React operator UI that is built into the Caesium Go binary.

## Stack

- React 19 + TypeScript
- Vite
- Tailwind CSS + shadcn/ui primitives
- TanStack Query + TanStack Router
- React Flow for DAG rendering
- Recharts for stats visualizations
- xterm.js for task log rendering
- Vitest + Testing Library for component tests
- Playwright for browser E2E

## Routes

The UI is served from `/` by the Go API and currently includes:

- `/jobs`
- `/jobs/:jobId`
- `/jobs/:jobId/runs/:runId`
- `/stats`
- `/triggers`
- `/atoms`
- `/jobdefs`
- `/system`
- `/system/logs`
- `/system/database`

## Development

Install dependencies:

```bash
cd ui
npm ci
```

Run the Vite dev server:

```bash
npm run dev
```

By default Vite proxies `/v1` requests to `http://localhost:8080`, so the usual workflow is:

```bash
just run
cd ui
npm run dev
```

## Build and Embedding

The production UI is embedded into the Go binary:

- `build/Dockerfile` runs `npm ci && npm run build` inside `ui/`.
- The generated `ui/dist/` bundle is embedded through [embed.go](embed.go).
- [api/ui.go](../api/ui.go) serves the embedded assets and falls back to `index.html` for SPA routes.

For local production-style verification:

```bash
cd ui
npm run build
npm run preview
```

## Tests

Unit and component tests:

```bash
cd ui
npm test
```

Current frontend coverage includes:

- API client and SSE client behavior
- Stats page rendering
- Database console utilities
- Task detail panel resize behavior
- Task node rendering states
- Job DAG layout/status/edge mapping

Browser E2E:

```bash
just ui-e2e
```

`just ui-e2e` starts a real Caesium server with the embedded UI, installs Chromium if needed, and runs Playwright against the operator flow.

## Notes

- The UI depends on real REST and SSE endpoints; it is not a standalone mock frontend.
- Some operator tools are env-gated on the backend:
  - `CAESIUM_LOG_CONSOLE_ENABLED`
  - `CAESIUM_DATABASE_CONSOLE_ENABLED`
- Bundle budget enforcement runs as part of `npm run build:ci` and `just ui-test`.
