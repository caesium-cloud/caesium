<p align="center">
  <img src="brand/caesium-icon.svg" width="120" alt="Caesium logo" />
</p>

<h1 align="center">caesium</h1>

<p align="center">
  <strong>Open-source distributed job scheduler with DAG pipelines, multi-runtime support, and a built-in web UI</strong>
</p>

<p align="center">
  <a href="https://pkg.go.dev/github.com/caesium-cloud/caesium"><img src="https://pkg.go.dev/badge/github.com/caesium-cloud/caesium.svg" alt="Go Reference"></a>
  <a href="https://goreportcard.com/report/github.com/caesium-cloud/caesium"><img src="https://goreportcard.com/badge/github.com/caesium-cloud/caesium" alt="Go Report Card"></a>
  <a href="https://codecov.io/gh/caesium-cloud/caesium"><img src="https://codecov.io/gh/caesium-cloud/caesium/branch/develop/graph/badge.svg?token=YXM50NU5GI" alt="Coverage"></a>
  <a href="https://github.com/caesium-cloud/caesium/releases"><img src="https://img.shields.io/github/release/caesium-cloud/caesium.svg" alt="Release"></a>
  <a href="https://hub.docker.com/r/caesiumcloud/caesium/"><img src="https://img.shields.io/docker/pulls/caesiumcloud/caesium?style=plastic" alt="Docker Pulls"></a>
</p>

---

Caesium is an open-source distributed job scheduler named after the element whose atoms define the second — a nod to the precision it aims to bring to task orchestration. Define jobs as declarative YAML manifests, express complex task dependencies as DAGs, and execute them across Docker, Podman, or Kubernetes with full visibility through a web UI and terminal console.

## Features

- **Declarative job definitions** — author pipelines in YAML with full schema validation and diffing
- **DAG execution** — express fan-out/fan-in task dependencies; tasks run in parallel where possible
- **Multi-runtime support** — run tasks in Docker, Podman, or Kubernetes pods
- **Multiple trigger types** — schedule jobs with cron expressions or fire them via HTTP triggers
- **Web UI** — embedded React application with DAG visualization, live log streaming, and run history
- **Terminal console** — interactive TUI for job inspection, log tailing, and trigger management
- **REST and GraphQL APIs** — full programmatic control over jobs, runs, triggers, and atoms
- **OpenLineage integration** — emit lineage events for cross-platform data observability
- **Distributed execution** — consensus-based clustering via dqlite; mixed `amd64`/`arm64` clusters supported
- **Prometheus metrics** — built-in `/metrics` endpoint for observability

## Quick Start

### Prerequisites

- [Docker](https://docs.docker.com/get-docker/) (or Podman / a Kubernetes cluster)
- [just](https://github.com/casey/just) — task runner used for all build and dev commands

### Run the server

```bash
just run
```

This pulls the latest `caesiumcloud/caesium` image and starts the server on `http://localhost:8080`. The web UI is available at the root path.

### Apply your first job

```bash
# Seed the bundled example jobs
just hydrate

# Or apply your own manifest
caesium job apply --server http://localhost:8080 --path ./my-jobs/
```

### Open the terminal console

```bash
just console
```

See [docs/console.md](docs/console.md) for keyboard shortcuts and configuration.

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

For the full schema — including DAG dependencies, fan-out/fan-in patterns, callbacks, and environment injection — see [docs/job-definitions.md](docs/job-definitions.md) and the [schema reference](docs/job-schema-reference.md).

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

### Supported architectures

- `linux/amd64`
- `linux/arm64`

Mixed `amd64`/`arm64` clusters are supported. Task container images must support the architecture of the node they run on; multi-arch task images are pulled automatically when manifests include both architectures.

## Development

```bash
# Run Go unit tests with race detector and coverage
just unit-test

# Run React unit tests
just ui-test

# Run all linters (go fmt, go vet, golangci-lint, eslint)
just lint && just ui-lint

# Run integration tests (requires Docker-in-Docker)
just integration-test

# Validate the Helm chart
just helm-lint
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for full setup instructions, code conventions, and the PR process.

## Deploying to Kubernetes

Caesium ships a Helm chart under `helm/caesium/`. See [docs/kubernetes-deployment.md](docs/kubernetes-deployment.md) for a step-by-step guide covering installation, scaling, and persistent volume configuration.

## API Reference

The server exposes a REST API on port `8080` and a GraphQL endpoint at `/gql`.

| Endpoint | Purpose |
|---|---|
| `GET /health` | Health check (database, triggers, active runs) |
| `GET /metrics` | Prometheus metrics |
| `GET /v1/jobs` | List job definitions |
| `POST /v1/jobs` | Create a job |
| `GET /v1/jobs/{id}/dag` | Retrieve task dependency graph |
| `POST /v1/jobs/{id}/runs` | Trigger a new run |
| `GET /v1/jobs/{id}/runs/{run_id}/logs` | Stream task logs (SSE) |
| `GET /v1/triggers` | List triggers |
| `PUT /v1/triggers` | Update a trigger |
| `GET /v1/atoms` | List registered execution atoms |
| `GET /v1/events` | Subscribe to lifecycle events (SSE) |
| `GET/POST /gql` | GraphQL endpoint |

## Documentation

| Guide | Description |
|---|---|
| [docs/job-definitions.md](docs/job-definitions.md) | Authoring, linting, diffing, and applying job manifests |
| [docs/job-schema-reference.md](docs/job-schema-reference.md) | Full YAML schema reference |
| [docs/console.md](docs/console.md) | Terminal UI usage and keyboard shortcuts |
| [docs/kubernetes-deployment.md](docs/kubernetes-deployment.md) | Helm-based Kubernetes deployment |
| [docs/parallel-execution-operations.md](docs/parallel-execution-operations.md) | Distributed worker configuration and troubleshooting |
| [docs/open_lineage.md](docs/open_lineage.md) | OpenLineage integration and event configuration |

## Contributing

Contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for how to get started, the development workflow, coding conventions, and the PR process.

## License

Caesium is open source. See [LICENSE](LICENSE) for details.
