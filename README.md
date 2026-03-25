# Caesium

[![Pkg Widget]][Pkg]
[![Drone CI Widget]][Drone CI]
[![Go Report Widget]][Go Report]
[![Codecov Widget]][Codecov]
[![GitHub Widget]][GitHub]
[![Docker Widget]][Docker]

----

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

## Supported Architectures

- `linux/amd64`
- `linux/arm64`

Runtime images are published as multi-arch Docker manifests so `docker pull caesiumcloud/caesium:<tag>` resolves to the native architecture automatically.

## Building by Architecture

- Host-default platform (auto-detected): `just build`
- Override target platform: `CAESIUM_PLATFORM=linux/arm64 just build`
- Cross-build one platform with buildx: `just build-cross linux/arm64`
- Build and push multi-arch images: `just build-multiarch tag=<tag>`

## Server Deployment

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

## Mixed-Arch Cluster Notes

- Mixed `amd64`/`arm64` caesium clusters are supported when all nodes run compatible versions.
- Task container images must support the architecture of the node that runs them.
- On Docker/Kubernetes/Podman, multi-arch task images are pulled automatically when manifests include both architectures.

## Documentation

- Docs index: [docs/README.md](docs/README.md)
- Airflow parity progress: [docs/airflow-parity.md](docs/airflow-parity.md)
- Embedded web UI plan: [docs/ui_implementation_plan.md](docs/ui_implementation_plan.md)
- Kubernetes + Helm guide: [docs/kubernetes-deployment.md](docs/kubernetes-deployment.md)
- Job manifest guide: [docs/job-definitions.md](docs/job-definitions.md)

## API Reference

- `GET /health` for health checks
- `GET /metrics` for Prometheus metrics
- `GET /v1/jobs` to list jobs
- `POST /v1/jobs/:id/run` to start a run
- `GET /v1/jobs/:id/runs/:run_id` to inspect a run
- `GET /v1/events` to subscribe to lifecycle events
- `GET/POST /gql` for GraphQL access

## Contributing

Contributions are welcome. Start with [CONTRIBUTING.md](CONTRIBUTING.md) for development workflow, testing expectations, and PR conventions.

[Pkg]: https://pkg.go.dev/github.com/caesium-cloud/caesium
[Pkg Widget]: https://pkg.go.dev/badge/github.com/caesium-cloud/caesium.svg
[Drone CI]: https://cloud.drone.io/caesium-cloud/caesium
[Drone CI Widget]: https://cloud.drone.io/api/badges/caesium-cloud/caesium/status.svg
[Go Report]: https://goreportcard.com/report/github.com/caesium-cloud/caesium
[Go Report Widget]: https://goreportcard.com/badge/github.com/caesium-cloud/caesium
[Codecov]: https://codecov.io/gh/caesium-cloud/caesium
[Codecov Widget]: https://codecov.io/gh/caesium-cloud/caesium/branch/develop/graph/badge.svg?token=YXM50NU5GI
[GitHub]: https://github.com/caesium-cloud/caesium/releases
[GitHub Widget]: https://img.shields.io/github/release/caesium-cloud/caesium.svg
[Docker]: https://hub.docker.com/r/caesiumcloud/caesium/
[Docker Widget]: https://img.shields.io/docker/pulls/caesiumcloud/caesium?style=plastic
