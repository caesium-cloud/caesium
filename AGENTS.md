# Repository Guidelines

## Project Structure & Module Organization
- `cmd/` – CLI entrypoints built with Cobra; binaries assemble here.
- `internal/` – private packages for app logic (not imported externally).
- `pkg/` – public packages intended for reuse.
- `api/` – HTTP/GraphQL handlers and related schema/routes.
- `build/` – Dockerfiles (`Dockerfile`, `Dockerfile.build`) and build assets.
- `test/` – integration tests (run with `-tags=integration`).
- `docs/` – user and developer documentation.
- Root: `go.mod`, `go.sum`, `justfile`, CI configs under `.circleci/` and `.github/`.

## Build, Test, and Development Commands
- `just builder` – build the builder image used for reproducible builds.
- `just build` – build the runtime image `caesiumcloud/caesium:latest`.
- `just run` – start the server container locally (host network).
- `just rm` – remove the running container.
- `just console` – open the interactive console.
- `just unit-test` – run unit tests with race + coverage.
- `just integration-test` – run tests in `./test` with `-tags=integration`.
- Containerized builds are required: use `just build` (or `just builder` + `just run`). Avoid invoking `go build` directly on the host so the toolchain and CGO deps stay consistent.

## Coding Style & Naming Conventions
- Go formatting: run `go fmt ./...` (CI expects formatted code).
- Lint/vet: run `go vet ./...` before submitting.
- Naming: packages lower-case short names; exported types, funcs, and consts use CamelCase; tests end with `_test.go`.
- Keep modules cohesive; prefer `internal/` for non-public code.

## Testing Guidelines
- Unit tests live beside code or under package directories; name files `*_test.go`.
- Integration tests live in `test/` and require `-tags=integration` (use `just integration-test`).
- Aim for meaningful coverage on core packages; keep tests deterministic and hermetic.
- Generate coverage locally with `just unit-test` (writes `coverage.txt`).

## Commit & Pull Request Guidelines
- Commits: concise, imperative subject (50–72 chars). Example: `Add HTTP triggers for jobs (#51)`.
- PRs: include a clear summary, linked issues, test evidence (logs or output), and docs updates when behavior/UI/CLI changes.
- Ensure `just unit-test` passes; include integration results if relevant.

## Security & Configuration Tips
- Configuration is via environment variables (parsed with `envconfig`); prefer explicit envs over flags in examples.
- Do not commit secrets; use local env files or CI secrets.
- Review Dockerfiles under `build/` for any changes affecting supply chain or runtime permissions.

## Current TUI & API Notes
- `cmd/console/` now houses `config`, `api`, and `app` packages; the console boots a Bubble Tea program that lists jobs, triggers, and atoms with loading/error states.
- REST API mounts `/v1/atoms`, `/v1/jobs`, and `/v1/triggers` via Echo; job routes include tasks, DAG, run history, and log streaming helpers for the console (`api/rest/controller/job/`).
- An in-memory run store under `internal/run` tracks active and historical executions so the console can list runs and retrieve metadata without persisting to the database yet.
- Job execution (cron + HTTP triggers) records task lifecycle information and exposes `/v1/jobs/:id/runs/:run_id/logs` to tail engine output.
- Console roadmap and progress notes live in `docs/console-tui-plan.md`; Phase 1 checklist is tracked in `docs/console-tui-foundation-todo.md`, and user-facing instructions are available in `docs/console.md`.
