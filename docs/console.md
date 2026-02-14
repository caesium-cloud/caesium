# Caesium Console

The Caesium console provides a terminal UI for exploring jobs, triggers, atoms, and statistics exposed via the REST API. It renders tabbed panes (Jobs/Triggers/Atoms/Stats) that occupy the full screen, with keyboard-driven navigation and status feedback.

## Running the Console

```sh
just console
```

The command builds the latest console binary and starts it with the appropriate terminal settings. Use `q` or `Ctrl+C` at any time to exit.

### Seeding Sample Data

When the runtime starts from a fresh volume it contains no jobs, so the console tables will appear empty. You can seed a handful of example definitions with:

```sh
just hydrate
```

This command assumes `just run` is already managing a local server on `http://127.0.0.1:8080`; it mounts the manifests under `docs/examples/` and applies them via the REST API so the console immediately has jobs, triggers, and atoms to display.

The hydration set now includes richer scenarios for TUI validation:

- DAG fan-out/fan-in with explicit `next`/`dependsOn` edges (`fanout-join.job.yaml`).
- HTTP-triggered operational workflows for manual triggering from the Jobs tab (`http-ops-debug.job.yaml`).
- Mixed run-history cases (success + intentional failure) in a multi-document manifest (`run-history.job.yaml`).
- Callback failure surfaces for run/callback status inspection (`callback-failure.job.yaml`).

## Configuration

The console connects to the Caesium API using the following environment variables:

| Variable | Description | Default |
| --- | --- | --- |
| `CAESIUM_BASE_URL` | Full base URL to the Caesium API. | `http://127.0.0.1:8080` |
| `CAESIUM_HOST` | Hostname (and optional port) used to derive the base URL when `CAESIUM_BASE_URL` is unset. | empty |

When both variables are unset, the console connects to the local API at `http://127.0.0.1:8080`. If only `CAESIUM_HOST` is provided, the default port `8080` is appended unless you specify one explicitly.

Example:

```sh
CAESIUM_BASE_URL="https://caesium.example.com" just console
```

## Keyboard Shortcuts

| Keys | Action |
| --- | --- |
| `1` / `2` / `3` / `4` | Switch between Jobs, Triggers, Atoms, and Stats tabs |
| `Tab` / `Shift+Tab` | Cycle forward/backward through tabs |
| `↑` / `↓` | Navigate within the active table |
| `r` | Reload data from the API |
| `p` | Run an API health ping (`/health`) and update diagnostics |
| `T` | Cycle console themes |
| `?` | Open/close the help overlay |
| `Enter` | (Jobs tab) open the detail/DAG screen |
| `u` | (Jobs detail) open the run selector |
| `t` | (Jobs tab) trigger the selected job manually |
| `g` | (Jobs detail) open/close task log streaming for the focused DAG node |
| `R` | (Run selector) request re-run confirmation for the selected run |
| `Space` | (Logs modal) toggle follow/pause |
| `/` | (Logs modal) start editing a log filter query |
| `c` | (Logs modal) clear the current log filter |
| `e` | (Logs modal) export the current filtered log snippet to a temp file |
| `Enter` / `y` | (Confirm modal) accept action |
| `Esc` / `n` | (Confirm modal) cancel action |
| `Esc` / `q` | Exit the detail screen; press again to quit the console |
| `q` or `Ctrl+C` | Quit the console |

When exporting logs, the console writes a snippet file to your OS temp directory and reports the full path in the status bar.

## Diagnostics and Resilience

- Startup loads now include bounded retry/backoff for jobs/triggers/atoms fetches.
- The status line shows API health and timing diagnostics: ping latency, load latency, retry count, and check timestamp.
- Use `p` anytime to force a fresh health ping and update the diagnostics state immediately.

## Development Notes

- API clients live under `cmd/console/api`. Extend these services when additional REST endpoints are needed (e.g. detailed runs, logs).
- Bubble Tea state and view logic resides in `cmd/console/app`. Panes share common layout helpers for tabbed navigation and column distributions; add new panes here when introducing more resources.
- Configuration loading and validation is handled by `cmd/console/config`.
- Run `go test ./cmd/console/...` for quick feedback or `just unit-test` for the full containerized suite.
