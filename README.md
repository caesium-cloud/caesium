<p align="center">
  <img src="brand/caesium-icon.svg" width="120" alt="Caesium logo" />
</p>

<h1 align="center">caesium</h1>

<p align="center">
  <strong>Open-source distributed job scheduler with DAG pipelines, multi-runtime support, and an embedded web UI</strong>
</p>

<p align="center">
  <a href="https://pkg.go.dev/github.com/caesium-cloud/caesium"><img src="https://pkg.go.dev/badge/github.com/caesium-cloud/caesium.svg" alt="Go Reference"></a>
  <a href="https://goreportcard.com/report/github.com/caesium-cloud/caesium"><img src="https://goreportcard.com/badge/github.com/caesium-cloud/caesium" alt="Go Report Card"></a>
  <a href="https://codecov.io/gh/caesium-cloud/caesium"><img src="https://codecov.io/gh/caesium-cloud/caesium/branch/develop/graph/badge.svg?token=YXM50NU5GI" alt="Coverage"></a>
  <a href="https://github.com/caesium-cloud/caesium/releases"><img src="https://img.shields.io/github/release/caesium-cloud/caesium.svg" alt="Release"></a>
  <a href="https://hub.docker.com/r/caesiumcloud/caesium/"><img src="https://img.shields.io/docker/pulls/caesiumcloud/caesium?style=plastic" alt="Docker Pulls"></a>
</p>

Caesium lets you define jobs as declarative YAML DAGs, run them on Docker, Podman, or Kubernetes, and operate them through a REST API, GraphQL endpoint, Prometheus metrics, and an embedded React UI.

## Local Developer Experience

Caesium is designed so job authors can validate, visualize, and execute pipelines locally before pushing them to a server.

### Validate definitions

```bash
caesium test --path jobs/ --verbose
```

Use `--check-images` to verify local image availability.

### Visualize a DAG

```bash
caesium job preview --path jobs/fanout-join.job.yaml
```

### Run locally

```bash
caesium dev --once --path jobs/nightly-etl.job.yaml
```

`caesium dev` without `--once` watches YAML files and re-runs the DAG on save. The local runner uses an in-memory SQLite database and the same execution engine as the server path.

## Quick Start

### 1. Write a job definition

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: nightly-etl
trigger:
  type: cron
  configuration:
    cron: "0 2 * * *"
    timezone: "UTC"
steps:
  - name: extract
    image: alpine:3.20
    command: ["sh", "-c", "echo extracting"]
    next: transform
  - name: transform
    image: alpine:3.20
    command: ["sh", "-c", "echo transforming"]
    dependsOn: extract
    next: load
  - name: load
    image: alpine:3.20
    command: ["sh", "-c", "echo loading"]
    dependsOn: transform
```

### 2. Validate and preview it

```bash
caesium test --path jobs/ --verbose
caesium job preview --path jobs/nightly-etl.job.yaml
caesium job lint --path jobs/
```

### 3. Run it locally

```bash
caesium dev --once --path jobs/nightly-etl.job.yaml
```

### 4. Start the server and apply definitions

```bash
# Start the server
just run

# Apply definitions
caesium job apply --path jobs/ --server http://localhost:8080
```

## Features

- Declarative YAML job definitions with validation, diffing, schema reporting, and Git sync.
- DAG execution with fan-out, fan-in, retry controls, trigger rules, and run parameters.
- Docker, Podman, and Kubernetes task runtimes.
- Cron and HTTP triggers.
- Distributed execution backed by dqlite, including mixed `amd64` and `arm64` clusters.
- Embedded operator UI with live run updates, DAG inspection, backfill controls, and log streaming.
- OpenLineage event emission.
- Prometheus metrics plus optional in-browser operator tools for server logs and database inspection.

## Server Workflow

### Prerequisites

- [Docker](https://docs.docker.com/get-docker/) or Podman
- [just](https://github.com/casey/just)

### Run the server

```bash
just run
```

The API and embedded UI are served from `http://localhost:8080`.

### Load example jobs

```bash
just hydrate
```

### Trigger a run manually

```bash
curl -X POST http://localhost:8080/v1/jobs/<job-id>/run
```

### Backfill a cron job

```bash
caesium backfill create \
  --job-id <job-id> \
  --start 2026-03-01T00:00:00Z \
  --end 2026-03-03T00:00:00Z \
  --server http://localhost:8080
```

## Job Definitions

Jobs use the `apiVersion` / `kind` / `metadata` / `trigger` / `steps` schema. For full authoring guidance see [docs/job-definitions.md](docs/job-definitions.md) and the generated reference in [docs/job-schema-reference.md](docs/job-schema-reference.md).

Useful CLI commands:

```bash
caesium job lint --path ./jobs
caesium job diff --path ./jobs
caesium job apply --path ./jobs --server http://localhost:8080
caesium job schema --doc
caesium run retry-callbacks --job-id <job-id> --run-id <run-id>
```

## Building and Testing

Runtime images are published as multi-arch Docker manifests. `docker pull caesiumcloud/caesium:<tag>` resolves to the native architecture automatically.

| Command | Description |
|---|---|
| `just build` | Build a release image for the host platform |
| `CAESIUM_PLATFORM=linux/arm64 just build` | Cross-build for a specific architecture |
| `just build-cross linux/arm64` | Cross-build a single platform with buildx |
| `just build-multiarch tag=<tag>` | Build and push a multi-arch manifest |
| `just unit-test` | Run Go unit tests with race detector and coverage |
| `just ui-test` | Run UI unit tests and bundle budget checks |
| `just ui-e2e` | Run Playwright against the embedded UI and a real Caesium server |
| `just integration-test` | Run integration tests |
| `just helm-lint` | Validate the Helm chart |

Supported runtime image targets:

- `linux/amd64`
- `linux/arm64`

## Operator Tools

The embedded UI exposes a few optional power-user surfaces:

- Server log console: enabled by `CAESIUM_LOG_CONSOLE_ENABLED=true` and backed by `GET /v1/logs/stream`, `GET /v1/logs/level`, and `PUT /v1/logs/level`.
- Database console: enabled by `CAESIUM_DATABASE_CONSOLE_ENABLED=true` and backed by `GET /v1/database/schema` and `POST /v1/database/query`.
- Worker inspection: `GET /v1/nodes/:address/workers`.
- Fleet-level stats: `GET /v1/stats`.

## API Reference

The server exposes REST on port `8080` plus GraphQL at `GET /gql`.

| Endpoint | Purpose |
|---|---|
| `GET /health` | Health check |
| `GET /metrics` | Prometheus metrics |
| `GET /gql` | GraphQL endpoint |
| `GET /v1/jobs` | List jobs |
| `GET /v1/jobs/:id` | Get one job |
| `GET /v1/jobs/:id/tasks` | List persisted task definitions for a job |
| `GET /v1/jobs/:id/dag` | Retrieve DAG nodes and edges |
| `POST /v1/jobs/:id/run` | Trigger a new run |
| `PUT /v1/jobs/:id/pause` | Pause a job |
| `PUT /v1/jobs/:id/unpause` | Unpause a job |
| `GET /v1/jobs/:id/runs` | List runs for a job |
| `GET /v1/jobs/:id/runs/:run_id` | Get one run |
| `GET /v1/jobs/:id/runs/:run_id/logs?task_id=<task-id>` | Stream or retrieve task logs |
| `POST /v1/jobs/:id/runs/:run_id/callbacks/retry` | Retry failed callbacks |
| `POST /v1/jobs/:id/backfill` | Start a backfill |
| `GET /v1/jobs/:id/backfills` | List backfills |
| `PUT /v1/jobs/:id/backfills/:backfill_id/cancel` | Cancel a backfill |
| `POST /v1/jobdefs/apply` | Apply one or more job definitions |
| `GET /v1/triggers` | List triggers |
| `GET /v1/atoms` | List atoms |
| `GET /v1/events` | Subscribe to lifecycle events over SSE |
| `GET /v1/stats` | Get aggregated job/run statistics |
| `GET /v1/nodes/:address/workers` | Inspect worker state for one node |

The log and database console endpoints are intentionally gated by environment variables because they are operator-facing debugging features rather than default public APIs.

## Documentation

| Guide | Description |
|---|---|
| [docs/README.md](docs/README.md) | Documentation index |
| [docs/job-definitions.md](docs/job-definitions.md) | Authoring, linting, diffing, and applying manifests |
| [docs/job-schema-reference.md](docs/job-schema-reference.md) | Generated schema reference |
| [docs/backfill.md](docs/backfill.md) | Backfill API, CLI, and UI behavior |
| [docs/parallel-execution-operations.md](docs/parallel-execution-operations.md) | Distributed execution configuration and troubleshooting |
| [docs/open_lineage.md](docs/open_lineage.md) | OpenLineage transport and configuration |
| [docs/kubernetes-deployment.md](docs/kubernetes-deployment.md) | Helm-based Kubernetes deployment |
| [docs/ui_implementation_plan.md](docs/ui_implementation_plan.md) | Embedded UI implementation status and remaining gaps |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for setup, development workflow, and PR guidance.

## License

See [LICENSE](LICENSE) for details.
