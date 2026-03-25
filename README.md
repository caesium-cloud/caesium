<p align="center">
  <img src="brand/caesium-icon.svg" width="120" alt="Caesium logo" />
</p>

<h1 align="center">caesium</h1>

<p align="center">
  <strong>Open-source distributed job scheduler with DAG pipelines, multi-runtime support, and built-in web UI</strong>
</p>

<p align="center">
  <a href="https://pkg.go.dev/github.com/caesium-cloud/caesium"><img src="https://pkg.go.dev/badge/github.com/caesium-cloud/caesium.svg" alt="Go Reference"></a>
  <a href="https://goreportcard.com/report/github.com/caesium-cloud/caesium"><img src="https://goreportcard.com/badge/github.com/caesium-cloud/caesium" alt="Go Report Card"></a>
  <a href="https://codecov.io/gh/caesium-cloud/caesium"><img src="https://codecov.io/gh/caesium-cloud/caesium/branch/develop/graph/badge.svg?token=YXM50NU5GI" alt="Coverage"></a>
  <a href="https://github.com/caesium-cloud/caesium/releases"><img src="https://img.shields.io/github/release/caesium-cloud/caesium.svg" alt="Release"></a>
  <a href="https://hub.docker.com/r/caesiumcloud/caesium/"><img src="https://img.shields.io/docker/pulls/caesiumcloud/caesium?style=plastic" alt="Docker Pulls"></a>
</p>

Caesium is an open source distributed job scheduler. Define pipelines as declarative YAML DAGs, execute them on Docker, Podman, or Kubernetes, and iterate locally with instant feedback — no running server required.

## Local Developer Experience

Caesium ships with first-class local development tools that let you validate, visualise, and execute pipelines on your laptop before pushing to a cluster. This is a deliberate departure from Airflow-style schedulers where testing a DAG typically means deploying it to a server, waiting for a scheduler tick, and tailing logs across multiple services.

### Validate definitions

```bash
# Validate YAML schemas and analyse DAG topology
caesium test --path jobs/

#   PASS  nightly-etl
#          Steps: extract -> transform -> load (3 steps, max parallelism: 1)
#   PASS  fanout-join-demo
#          Steps: bootstrap -> [lint, unit-test] -> package -> publish (5 steps, max parallelism: 2)
```

Use `--verbose` to see root/leaf steps, dependency details, and engine configuration. Add `--check-images` to verify that every container image referenced in your definitions is available locally.

### Visualise the DAG

```bash
caesium job preview --path jobs/fanout-join.job.yaml

# ┌───────────┐      ┌───────────┐      ┌─────────┐      ┌─────────┐
# │ bootstrap │───┬─>│   lint    │───┬─>│ package │─────>│ publish │
# └───────────┘   │  └───────────┘   │  └─────────┘      └─────────┘
#                 │                  │
#                 │  ┌───────────┐   │
#                 └─>│ unit-test │───┘
#                    └───────────┘
```

### Run locally

```bash
# Execute the DAG against your local Docker daemon — no server needed
caesium dev --once --path jobs/nightly-etl.yaml

# caesium dev nightly-etl  jobs/nightly-etl.yaml
# ------------------------------------------------------------
#   OK    nightly-etl completed successfully
```

`caesium dev` without `--once` watches your YAML files and re-runs the DAG automatically on every save — a hot-reload loop for pipelines. Use `--run-timeout` and `--task-timeout` to catch runaway containers early.

### How it works

The local runner creates an **ephemeral in-memory SQLite database** per execution and reuses 100% of the production DAG execution engine via dependency injection. No mock schedulers, no Docker Compose stacks, no database services to manage. Your laptop is the only dependency.

### Why this matters (vs. Airflow)

| | Caesium | Airflow |
|---|---|---|
| **Validate a DAG** | `caesium test --path .` | Import DAG into scheduler, check web UI for parse errors |
| **Run a DAG locally** | `caesium dev --once` | `docker compose up` (webserver, scheduler, worker, Postgres, Redis) |
| **Iterate on changes** | `caesium dev` watches files, re-runs on save | Restart scheduler, wait for DAG re-parse (~30s), trigger manually |
| **Visualise the DAG** | `caesium job preview` in any terminal | Open browser, navigate to DAG graph view |
| **Time to first run** | Seconds (needs only Docker) | Minutes (needs 5+ services running) |
| **CI dry-run** | `caesium test` in any container | Requires Airflow + DB installed or a custom `python -c "import dag"` wrapper |

## Quick Start

### 1. Write a job definition

```yaml
# jobs/etl.yaml
apiVersion: v1
kind: Job
metadata:
  alias: my-etl
trigger:
  type: cron
  configuration:
    expression: "0 2 * * *"
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

### 2. Validate and preview

```bash
caesium test --path jobs/ --verbose
caesium job preview --path jobs/etl.yaml
```

### 3. Run locally

```bash
caesium dev --once --path jobs/etl.yaml
```

### 4. Deploy to a cluster

```bash
# Start the server
just run

# Apply definitions
caesium job apply --path jobs/ --server http://localhost:8080
```

---

## Features

- **Declarative job definitions**: author pipelines in YAML with schema validation and diffing
- **DAG execution**: express fan-out/fan-in task dependencies; tasks run in parallel where possible
- **Multi-runtime support**: run tasks in Docker, Podman, or Kubernetes pods
- **Multiple trigger types**: schedule jobs with cron expressions or fire them via HTTP triggers
- **Run parameters and retry rules**: pass per-run params and configure task retries, backoff, and trigger rules
- **Web UI**: embedded React application with DAG visualization, live log streaming, and run history
- **REST and GraphQL APIs**: programmatic control over jobs, runs, triggers, and atoms
- **OpenLineage integration**: emit lineage events for cross-platform data observability
- **Distributed execution**: consensus-based clustering via dqlite; mixed `amd64`/`arm64` clusters supported
- **Prometheus metrics**: built-in `/metrics` endpoint for observability

## Server Deployment

### Prerequisites

- [Docker](https://docs.docker.com/get-docker/) or Podman
- [just](https://github.com/casey/just) for the standard build and dev workflow

### Run the server

```bash
just run
```

The API and embedded web UI are served from `http://localhost:8080`.

### Load example jobs

```bash
just hydrate
```

### Trigger a run manually

```bash
curl -X POST http://localhost:8080/v1/jobs/<job-id>/run
```

## Job Definitions

Jobs are declared in YAML. A minimal example:

```yaml
name: hello-world
description: A minimal one-task job
tasks:
  - name: greet
    image: alpine:3
    command: ["echo", "hello from caesium"]
triggers:
  - type: cron
    expression: "0 * * * *"
```

For the full schema, including DAG dependencies, fan-out/fan-in patterns, callbacks, retries, trigger rules, and environment injection, see [docs/job-definitions.md](docs/job-definitions.md) and [docs/job-schema-reference.md](docs/job-schema-reference.md).

### Validate a manifest

```bash
caesium job lint ./my-job.yaml
```

### Diff a manifest against the deployed version

```bash
caesium job diff ./my-job.yaml
```

## Building

Runtime images are published as multi-arch Docker manifests. `docker pull caesiumcloud/caesium:<tag>` automatically resolves to the native architecture.

| Command | Description |
|---|---|
| `just build` | Build a release image for the host platform |
| `CAESIUM_PLATFORM=linux/arm64 just build` | Cross-build for a specific architecture |
| `just build-cross linux/arm64` | Cross-build a single platform with buildx |
| `just build-multiarch tag=<tag>` | Build and push a multi-arch manifest |

### Supported Architectures

- `linux/amd64`
- `linux/arm64`

Mixed `amd64`/`arm64` clusters are supported. Task container images must support the architecture of the node they run on, and multi-arch task images are pulled automatically when manifests include both architectures.

## Development

```bash
# Run Go unit tests with race detector and coverage
just unit-test

# Run React unit tests and bundle budget checks
just ui-test

# Run linters
just lint
just ui-lint

# Run integration tests
just integration-test

# Validate the Helm chart
just helm-lint
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for setup, conventions, and the PR process.

## Deploying to Kubernetes

Caesium ships a Helm chart under `helm/caesium/`. See [docs/kubernetes-deployment.md](docs/kubernetes-deployment.md) for installation, scaling, and persistent volume guidance.

## API Reference

The server exposes a REST API on port `8080` and a GraphQL endpoint at `/gql`.

| Endpoint | Purpose |
|---|---|
| `GET /health` | Health check |
| `GET /metrics` | Prometheus metrics |
| `GET /v1/jobs` | List job definitions |
| `POST /v1/jobs/:id/run` | Trigger a new run |
| `PUT /v1/jobs/:id/pause` | Pause a job |
| `PUT /v1/jobs/:id/unpause` | Unpause a job |
| `GET /v1/jobs/:id/dag` | Retrieve the task dependency graph |
| `GET /v1/jobs/:id/runs/:run_id/logs` | Stream task logs (plain text stream) |
| `GET /v1/triggers` | List triggers |
| `GET /v1/atoms` | List registered execution atoms |
| `GET /v1/events` | Subscribe to lifecycle events (SSE) |
| `GET/POST /gql` | GraphQL endpoint |

## Documentation

| Guide | Description |
|---|---|
| [docs/README.md](docs/README.md) | Documentation index |
| [docs/job-definitions.md](docs/job-definitions.md) | Authoring, linting, diffing, and applying job manifests |
| [docs/job-schema-reference.md](docs/job-schema-reference.md) | Full YAML schema reference |
| [docs/airflow-parity.md](docs/airflow-parity.md) | Airflow parity progress and semantics |
| [docs/ui_implementation_plan.md](docs/ui_implementation_plan.md) | Embedded web UI architecture and roadmap |
| [docs/kubernetes-deployment.md](docs/kubernetes-deployment.md) | Helm-based Kubernetes deployment |
| [docs/parallel-execution-operations.md](docs/parallel-execution-operations.md) | Distributed worker configuration and troubleshooting |
| [docs/open_lineage.md](docs/open_lineage.md) | OpenLineage integration and event configuration |

## Contributing

Contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for how to get started, the development workflow, coding conventions, and the PR process.

## License

Caesium is open source. See [LICENSE](LICENSE) for details.
