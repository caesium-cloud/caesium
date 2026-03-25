# Caesium — Claude Code Instructions

## Project Overview

Caesium is a distributed job scheduler with declarative YAML DAG pipelines. See `AGENTS.md` for project structure, build commands, and coding conventions.

## Generating Job Definitions

When the user asks you to create, modify, or help with Caesium job definitions, follow the full schema reference in `docs/caesium-job-llm-reference.md`. Key rules:

- Always start with `apiVersion: v1` and `kind: Job`
- `metadata.alias` must be unique, lowercase, hyphenated
- Trigger type is either `cron` (5-field POSIX cron) or `http` (with path)
- Every step needs a unique `name` and an `image`
- When no step declares `next` or `dependsOn`, steps auto-link sequentially — once any step uses explicit edges, all edges must be explicit
- Steps emit structured output via `echo '##caesium::output {"key": "value"}'`; downstream steps consume it as `$CAESIUM_OUTPUT_<STEP>_<KEY>`
- For data contracts, declare `outputSchema`/`inputSchema` on steps and set `metadata.schemaValidation` to `"warn"` or `"fail"`
- Use `secret://` URIs for credentials — never hardcode secrets
- Save files with `.job.yaml` extension

## Local Development Workflow

```sh
caesium job lint --path jobs/           # Validate schemas and DAG
caesium job preview --path job.yaml     # ASCII DAG visualization
caesium dev --once --path job.yaml      # Run locally against Docker
caesium dev --path job.yaml             # Watch mode — re-run on save
caesium job diff --path jobs/           # Preview changes vs server
caesium job apply --path jobs/          # Deploy to server
```

## Build & Test

- `just unit-test` — Go unit tests with race detector
- `just lint` — go fmt + go vet + golangci-lint
- `just build` — Build runtime Docker image
- `just run` — Start server on localhost:8080
- `just hydrate` — Load example jobs into running server
- CI runs in `.circleci/`, not GitHub Actions

## Code Conventions

- Go code lives in `internal/` (private) and `pkg/` (public)
- Job definition schema is in `pkg/jobdef/definition.go`
- Example manifests are in `docs/examples/*.job.yaml`
- Integration tests require `-tags=integration` and live in `test/`
- Use containerized builds (`just build`), not bare `go build`
