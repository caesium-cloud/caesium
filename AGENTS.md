# Repository Guidelines

## Project Overview

Caesium is a distributed job scheduler with declarative YAML DAG pipelines supporting Docker, Podman, and Kubernetes runtimes.

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
- `just unit-test` – run unit tests with race + coverage.
- `just lint` – go fmt + go vet + golangci-lint.
- `just integration-test` – run tests in `./test` with `-tags=integration`.
- `just hydrate` – load example jobs into running server.
- Containerized builds are required: use `just build` (or `just builder` + `just run`). Avoid invoking `go build` directly on the host so the toolchain and CGO deps stay consistent.
- CI runs in `.circleci/`, not GitHub Actions.

## Local Development Workflow

```sh
caesium job lint --path jobs/           # Validate schemas and DAG
caesium job preview --path job.yaml     # ASCII DAG visualization
caesium dev --once --path job.yaml      # Run locally against Docker
caesium dev --path job.yaml             # Watch mode — re-run on save
caesium job diff --path jobs/           # Preview changes vs server
caesium job apply --path jobs/          # Deploy to server
```

## Coding Style & Naming Conventions
- Go formatting: run `go fmt ./...` (CI expects formatted code).
- Lint/vet: run `go vet ./...` before submitting.
- Naming: packages lower-case short names; exported types, funcs, and consts use CamelCase; tests end with `_test.go`.
- Keep modules cohesive; prefer `internal/` for non-public code.
- Job definition schema is in `pkg/jobdef/definition.go`.
- Example manifests are in `docs/examples/*.job.yaml`.

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

## Generating Caesium Job Definitions

When asked to create or modify Caesium job definition YAML files, follow the full reference in `docs/caesium-job-llm-reference.md`. Summary of key rules:

- Every job needs `apiVersion: v1`, `kind: Job`, a unique `metadata.alias`, a `trigger`, and at least one step
- Trigger types: `cron` (5-field POSIX cron expression) or `http` (with route path)
- Every step requires a unique `name` and an `image`; `engine` defaults to `docker`
- DAG wiring: if no step uses `next`/`dependsOn`, steps auto-link sequentially; once any step uses explicit edges, all must be explicit
- Data contracts: steps emit `echo '##caesium::output {"key": "value"}'`, downstream reads `$CAESIUM_OUTPUT_<STEP>_<KEY>`; declare `outputSchema`/`inputSchema` and set `metadata.schemaValidation` to `"warn"` or `"fail"`
- Secrets: use `secret://` URIs (`env`, `k8s`, `vault` providers) — never hardcode credentials
- File naming: use `.job.yaml` extension for Git sync glob matching
- Validate locally: `caesium job lint --path <dir>`, test with `caesium dev --once --path <file>`
- Full examples: `docs/examples/*.job.yaml`
