# Repository Guidelines

## Project Overview

Caesium is a self-hosted DAG scheduler for data pipelines: declarative YAML
jobs whose steps are container images, run on Docker, Podman, or Kubernetes.
It is one Go binary with an embedded Raft-replicated SQLite (dqlite) database,
a REST API, an embedded React console, and Prometheus metrics. The product
positioning is in `docs/design/differentiation-strategy.md`; the prioritised
plan is `docs/roadmap.md`.

## Project Structure & Module Organization
- `cmd/` – CLI entrypoints built with Cobra (`start`, `job`, `dev`, `test`, `run`, `why`, `blame`, `reproduce`, …); binaries assemble here.
- `internal/` – private packages for app logic (not imported externally).
- `pkg/` – public packages intended for reuse (`pkg/jobdef` is the YAML schema, `pkg/env` the configuration).
- `api/` – HTTP server, REST controllers and services.
- `ui/` – React + TypeScript + Vite console, embedded into the binary.
- `helm/` – the Kubernetes Helm chart.
- `reagents/` – the `caesiumcloud/{git-source,tf-discover,tf-warm,tf-runner}` images used by infrastructure deployment.
- `build/` – Dockerfiles (`Dockerfile`, `Dockerfile.build`) and build assets.
- `test/` – integration tests (run with `-tags=integration`).
- `docs/` – documentation; see [Documentation](#documentation) for the layout and rules.
- Root: `go.mod`, `go.sum`, `justfile`, CI under `.github/workflows/`.

## Build, Test, and Development Commands
- `just builder` – build the builder image used for reproducible builds.
- `just build` – build the runtime image `caesiumcloud/caesium:latest`.
- `just run` – start the server container locally (host network).
- `just rm` – remove the running container.
- `just unit-test` – run unit tests with race + coverage.
- `just lint` – go fmt + go vet + golangci-lint (formats the root package only; run `gofmt -l` on the directories you changed).
- `just integration-test` – run tests in `./test` with `-tags=integration`.
- `just ui-lint`, `just ui-test`, `just ui-e2e` – the console's lint, unit, and Playwright suites.
- `just helm-lint`, `just helm-template` – validate and render the chart.
- `just hydrate` – load example jobs into the running server.
- Containerized builds are required: use `just build` (or `just builder` + `just run`). Avoid invoking `go build` directly on the host so the toolchain and CGO deps stay consistent.
- CI runs in GitHub Actions (`.github/workflows/ci.yml`). `docs/ci.md` is the runbook: the job DAG, artifact flow, the `ci-ok` merge gate, and per-lane server env parity. `ci-ok` is a required status check on `master` and a merge queue is active.

## Local Development Workflow

```sh
caesium job lint --path jobs/           # Validate schemas and DAG
caesium job preview --path job.yaml     # ASCII DAG visualization
caesium dev --once --path job.yaml      # Run locally against Docker
caesium dev --path job.yaml             # Watch mode — re-run on save
caesium job diff --path jobs/           # Preview creates/updates vs server (add --prune for deletes)
caesium job apply --path jobs/          # Deploy to server
```

## Coding Style & Naming Conventions
- Go formatting: run `go fmt ./...` (CI expects formatted code).
- Lint/vet: run `go vet ./...` before submitting.
- Naming: packages lower-case short names; exported types, funcs, and consts use CamelCase; tests end with `_test.go`.
- Keep modules cohesive; prefer `internal/` for non-public code. `internal/guardrails/guardrails_test.go` enforces the import boundaries between `cmd/`, `api/`, `internal/`, and `pkg/`.
- Job definition schema is in `pkg/jobdef/definition.go`.
- Example manifests are in `docs/examples/*.job.yaml`.
- Base images are pinned repo-wide (`alpine:3.23`, `busybox:1.36.1`, `curlimages/curl:8.12.1`); a bare `alpine` anywhere in `.go/.yaml/.md/.ts` fails the unit tests.

## Testing Guidelines
- Unit tests live beside code or under package directories; name files `*_test.go`.
- Integration tests live in `test/` and require `-tags=integration` (use `just integration-test`).
- Aim for meaningful coverage on core packages; keep tests deterministic and hermetic.
- Generate coverage locally with `just unit-test` (writes `coverage.txt`).
- Run `just unit-test` before committing; do not rely on CI to catch compile errors.

### End-to-end coverage is the gate (drive the real surface)
"Green CI" must mean the feature works, not that internal functions pass on
synthetic inputs. A unit test that hand-seeds DB rows, mocks a clock, or
import-and-calls an internal function proves the function — never the wiring.
Several data-plane features shipped green-but-hollow this way (the lineage
impact query never persisted rows; `caesium why --json` / `caesium receipt get`
wrote machine output to stderr). So:

- **Every new CLI command and REST query ships with an integration test in
  `test/` that drives it through its real surface** — invoke the CLI binary
  (`s.runCLI*`) or hit the HTTP endpoint against the live server, and assert on
  observed output. Not a unit test on the internal handler.
- **For machine-readable CLI output (`--json`, receipts), assert stdout is
  clean and parseable, capturing stdout SEPARATELY from stderr** (`runCLIStdout`,
  not the stream-merging `runCLIRaw`). Log lines and cobra `cmd.Print*` both
  leak to the wrong stream; a merged capture hides it.
- **If a feature is config-gated** (e.g. lineage needs
  `CAESIUM_OPEN_LINEAGE_ENABLED=true`), enable it on the integration server in
  `just integration-up` so the path actually executes in CI. Every other
  `integration-up-*` recipe must carry the same `CAESIUM_*` vars; a guardrail
  test checks parity.
- A new `cmd/` subcommand or `api/rest/controller` endpoint with no
  corresponding `test/` integration scenario should block review.

## Commit & Pull Request Guidelines
- Commits: concise, imperative subject (50–72 chars). Example: `Add HTTP triggers for jobs (#51)`.
- Branch from and target `master`; a PR squash-merges to one commit.
- PRs: include a clear summary, linked issues, test evidence (logs or output), and docs updates when behavior/UI/CLI changes.
- Ensure `just unit-test` passes; include integration results if relevant.

## Security & Configuration Tips
- Configuration is via environment variables (parsed with `envconfig`); prefer explicit envs over flags in examples.
- Do not commit secrets; use local env files or CI secrets.
- Review Dockerfiles under `build/` for any changes affecting supply chain or runtime permissions.

## Documentation

Layout, and what belongs where:

| Location | Holds | Indexed by |
| --- | --- | --- |
| `README.md` | The pitch and a quick start. No reference tables; link into `docs/`. | — |
| `docs/*.md` | One guide per topic describing current behavior. | `docs/README.md` |
| `docs/design/*.md` | One design record per feature, plus dated specs (`<date>-<topic>-design.md`). Each opens with a `> Status:` banner within its first eight lines. | `docs/README.md` |
| `docs/exec-plans/active/`, `docs/exec-plans/completed/` | Execution plans in the `draft-exec-plan` shape, run with `exec-plan-wave`. | `docs/README.md` (active) |
| `docs/archive/` | Shipped, superseded, or evidence records with a pointer to the live successor. | `docs/archive/README.md` |
| `docs/examples/`, `docs/examples-k8s/` | Example manifests; every file must be listed in `docs/job-definitions.md`. | guardrail test |

Rules:

- `docs/job-schema-reference.md` is generated. Change `internal/jobdef/report/report.go` and regenerate with `caesium job schema --doc`; a test compares the two.
- Keep one document per topic. Extend the existing guide rather than adding a sibling; if you need a new guide, add it to `docs/README.md` in the same change (a guardrail test fails otherwise).
- Do not add per-PR evidence or audit write-ups under `docs/`. Put evidence in the PR description. If it must outlive the PR, it goes in `docs/archive/` with an entry in that index.
- When a design ships, update its `> Status:` banner to say so and link the completed plan; move the plan to `exec-plans/completed/`; flip the matching `docs/roadmap.md` status.
- Brainstorming specs go to `docs/design/<date>-<topic>-design.md`, not `docs/superpowers/`.
- Code comments may cite docs by path. When you move or rename a doc, grep the whole repo (`.go`, `.ts`, `.yaml`, `justfile`, skills) and fix every reference.
- The `.claude/skills/` and `.codex/skills/` directories hold the plan-drafting and wave-execution skills; keep their path conventions in step with this table.

## Generating Caesium Job Definitions

When asked to create or modify Caesium job definition YAML files, follow the full reference in `docs/caesium-job-llm-reference.md`. Summary of key rules:

- Every job needs `apiVersion: v1`, `kind: Job`, a unique `metadata.alias`, a `trigger`, and at least one step
- Trigger types: `cron` (5-field POSIX cron expression), `http` (with route path), `event`, or `freshness`
- Every step requires a unique `name` and an `image`; `engine` defaults to `docker`
- DAG wiring: if no step uses `next`/`dependsOn`, steps auto-link sequentially; once any step uses explicit edges, all must be explicit
- Data contracts: steps emit `echo '##caesium::output {"key": "value"}'`, downstream reads `$CAESIUM_OUTPUT_<STEP>_<KEY>`; declare `outputSchema`/`inputSchema` and set `metadata.schemaValidation` to `"warn"` or `"fail"`
- Secrets: use `secret://` URIs (`env`, `k8s`, `vault` providers) — never hardcode credentials
- File naming: use `.job.yaml` extension for Git sync glob matching
- Validate locally: `caesium job lint --path <dir>`, test with `caesium dev --once --path <file>`
- Full examples: `docs/examples/*.job.yaml`
