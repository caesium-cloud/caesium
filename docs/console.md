# Caesium Console

The Caesium console provides a terminal UI for exploring jobs, triggers, and atoms exposed via the REST API. It renders tabbed panes (Jobs/Triggers/Atoms) that occupy the full screen, with keyboard-driven navigation and status feedback.

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
| `1` / `2` / `3` | Switch between Jobs, Triggers, and Atoms tabs |
| `Tab` / `Shift+Tab` | Cycle forward/backward through tabs |
| `↑` / `↓` | Navigate within the active table |
| `r` | Reload data from the API |
| `Enter` | (Jobs tab) open the detail/DAG screen |
| `t` | (Jobs tab) trigger the selected job manually |
| `Esc` / `q` | Exit the detail screen; press again to quit the console |
| `q` or `Ctrl+C` | Quit the console |

## Development Notes

- API clients live under `cmd/console/api`. Extend these services when additional REST endpoints are needed (e.g. detailed runs, logs).
- Bubble Tea state and view logic resides in `cmd/console/app`. Panes share common layout helpers for tabbed navigation and column distributions; add new panes here when introducing more resources.
- Configuration loading and validation is handled by `cmd/console/config`.
- Run `go test ./cmd/console/...` for quick feedback or `just unit-test` for the full containerized suite.
