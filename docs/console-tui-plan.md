# Caesium Console TUI Plan

- `cmd/console` now ships a Bubble Tea TUI with configurable API client, tabbed panes for Jobs/Triggers/Atoms, spinner-driven loading, and error handling.
- REST handlers expose `/v1/atoms`, `/v1/jobs`, and `/v1/triggers` (`api/rest/bind/bind.go`), returning the underlying GORM models.
- Job APIs provide task listings, DAG summaries, manual runs, run history, and streaming logs (`api/rest/controller/job/*`), giving the console a complete execution surface.
- An in-memory run store tracks live and historical executions (`internal/run`), and job details embed trigger metadata plus the latest run snapshot.
- Cron triggers are queued internally on the executor ticker, while HTTP triggers can be fired via both REST and the new manual run endpoint.

## Target Experience
- Deliver a k9s-style TUI that can browse jobs, triggers, atoms, and their DAG topology with paneled layouts for lists, details, and metadata.
- Enable direct actions from the UI: trigger jobs manually, cancel queued runs (future), and drill into DAG nodes to inspect associated atoms, commands, and dependencies.
- Offer a log viewport that can tail container/pod output for each atom run, capture historical logs when available, and provide search/filter support.
- Provide global UX niceties: navigable sections, keybindings, contextual help, status bar, and configurable API endpoint/theme.

## Implementation Phases
1. **Phase 0 – API Surface for the Console**
   - ✅ Add REST endpoints for tasks (`GET /v1/jobs/:id/tasks`), DAG summaries, and trigger activation beyond HTTP (`POST /v1/jobs/:id/run`) while reusing existing services.
   - ✅ Add log streaming support (chunked `GET /v1/jobs/:id/runs/:run_id/logs`) so the console can tail output.
   - ✅ Extend serializers/endpoints to enrich responses with related trigger data and expose run metadata (`/v1/jobs/:id`, `/v1/jobs/:id/runs`).
2. **Phase 1 – Console Foundation**
   - ✅ Establish dedicated packages under `cmd/console` for configuration, API access, and Bubble Tea state (`config`, `api`, `app`).
   - ✅ Introduce typed clients for jobs, triggers, atoms, and runs with configurable base URL + timeouts.
   - ✅ Bootstrap the Bubble Tea program with loading/error states, spinner feedback, and tabbed panes that render full-width tables.
   - ✅ Document the console configuration workflow and development process in `docs/`.
   - ☐ Expand unit coverage for UI/state transitions as additional components land.
3. **Phase 2 – Resource Lists**
   - ✅ Build reusable tabbed table panes for jobs, triggers, and atoms with shared layout primitives and column auto-sizing.
   - ✅ Wire up asynchronous loading commands and maintain shared selection state across panes (keyboard shortcuts for quick switching).
4. **Phase 3 – Detail + DAG View**
   - Implement a detail pane that fetches job information, associated trigger, full DAG topology, and latest run metadata.
   - Render the DAG using the persisted edge set (multi-successor aware) so the UI can visualise branches/fan-ins with Lip Gloss layouts, highlight the selected node, and show successor/predecessor badges for navigation.
5. **Phase 4 – Actions and Workflows**
   - Hook keybindings to call trigger/run endpoints, display confirmation modals, surface action results in a status bar, and refresh affected views.
   - Handle optimistic updates and unified error reporting.
6. **Phase 5 – Logs Experience**
   - Integrate the log API with a viewport component capable of streaming updates, pause/resume, search, and exporting snippets.
   - Allow switching between historical runs once the backing data exists.
7. **Phase 6 – Polish and Resilience**
   - Add theming, help overlays, configuration (env vars/flags), diagnostics (health check ping, latency indicators), and automated tests (model update tests, HTTP client fakes, snapshot tests).

## Key Considerations
- The new in-memory run store enables run listings today; evaluate longer-term persistence/backfill if historical data needs to survive restarts.
- Ensure the API client respects future auth/session requirements (headers, TLS) and expose knobs through environment variables (e.g., `CAESIUM_CONSOLE_API`).
- Logging endpoints should stream efficiently and close cleanly; align interfaces with existing engine `Logs` contracts to minimize duplication.
- Provide graceful degradation when the API or scheduler is unavailable: show inline errors, retry with backoff, and allow offline browsing of cached data.
- Ensure the DAG view clearly communicates parallel branches (fan-out/fan-in) and exposes keybindings to jump between sibling/parent nodes without losing context.
- Validate UX with unit tests for reducers and integration tests using a mocked API server, and document the console workflow/keybindings in `docs/`.

## Suggested Next Steps
1. Scope and merge the API extensions in small PRs (tasks, runs, logs).
2. Scaffold the `cmd/console` package with Bubble Tea plumbing and a mocked API client.
3. Iterate on list/detail views, including the branched DAG viewport, before tackling live logs to confirm the navigation model.
