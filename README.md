<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="brand/caesium-logo-dark.svg">
    <source media="(prefers-color-scheme: light)" srcset="brand/caesium-logo-light.svg">
    <img src="brand/caesium-logo-dark.svg" width="380" alt="Caesium">
  </picture>
</p>

<p align="center">
  <strong>A self-hosted DAG scheduler for data pipelines.<br>One Go binary. An embedded Raft database. Any container image.</strong>
</p>

<p align="center">
  <a href="https://github.com/caesium-cloud/caesium/actions/workflows/ci.yml"><img src="https://github.com/caesium-cloud/caesium/actions/workflows/ci.yml/badge.svg?branch=master" alt="CI"></a>
  <a href="https://github.com/caesium-cloud/caesium/releases"><img src="https://img.shields.io/github/release/caesium-cloud/caesium.svg" alt="Release"></a>
  <a href="https://hub.docker.com/r/caesiumcloud/caesium/"><img src="https://img.shields.io/docker/pulls/caesiumcloud/caesium" alt="Docker Pulls"></a>
  <a href="https://goreportcard.com/report/github.com/caesium-cloud/caesium"><img src="https://goreportcard.com/badge/github.com/caesium-cloud/caesium" alt="Go Report Card"></a>
  <a href="https://pkg.go.dev/github.com/caesium-cloud/caesium"><img src="https://pkg.go.dev/badge/github.com/caesium-cloud/caesium.svg" alt="Go Reference"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache%202.0-blue.svg" alt="License"></a>
</p>

<p align="center">
  <a href="#quick-start">Quick start</a> ·
  <a href="docs/getting-started.md">Getting started</a> ·
  <a href="docs/README.md">Documentation</a> ·
  <a href="docs/roadmap.md">Roadmap</a> ·
  <a href="CONTRIBUTING.md">Contributing</a>
</p>

---

Caesium runs pipelines you write as YAML DAGs. Every step is a container image. Caesium schedules it on Docker, Podman, or Kubernetes, skips work whose inputs have not changed, checks the data that flows between steps, and keeps a record of every run so you can ask it what happened and why.

It ships as a single static binary with an embedded, Raft-replicated SQLite database ([dqlite](https://dqlite.io)). There is no PostgreSQL to run, no Redis, no message broker, and no vendor control plane. High availability, RBAC, SSO, audit logging, lineage, and Kubernetes execution are all in the Apache-2.0 build.

## A pipeline in thirty lines

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: nightly-etl
  schemaValidation: fail            # a step that emits the wrong shape fails, not its consumers
trigger:
  type: cron
  configuration:
    cron: "0 2 * * *"
    timezone: UTC
steps:
  - name: extract
    image: ghcr.io/acme/extract:1.4.2
    command: ["python", "extract.py"]        # prints: ##caesium::output {"row_count": 48213}
    outputSchema:
      type: object
      required: [row_count]
      properties:
        row_count: { type: integer }
  - name: transform
    image: ghcr.io/acme/transform:1.4.2
    command: ["sh", "-c", "python transform.py --rows $CAESIUM_OUTPUT_EXTRACT_ROW_COUNT"]
    cache: true                              # skipped on the next run if its inputs are identical
  - name: load
    image: ghcr.io/acme/load:1.4.2
    command: ["python", "load.py"]
    retries: 3
    retryDelay: 30s
    retryBackoff: true
```

```bash
caesium job lint --path nightly-etl.job.yaml       # schema and DAG checks, no server needed
caesium dev --once --path nightly-etl.job.yaml     # run it on your local Docker daemon
caesium job apply --path nightly-etl.job.yaml --server http://localhost:8080
```

No SDK, no decorators, no Python environment on the scheduler. If it runs in a container, it is a Caesium step. The Acme images above stand in for your own; the [quick start](#quick-start) runs a public-image example.

## Why Caesium

**Nothing to operate but Caesium.** The scheduler, its database, its API, and its console are one process. Three nodes form a Raft cluster with no external coordinator. You can `scp` the binary to an air-gapped host, an edge box, or a regulated on-prem network and it runs. Upgrades are a binary swap.

**It remembers what ran.** Every task records the exact image digest, arguments, environment, parameters, and the typed outputs of its predecessors. That record is what makes content-addressed caching safe, and it is what you query when something breaks: `caesium why`, `caesium blame`, `caesium run diff`, `caesium reproduce`, and tamper-evident receipts you can commit next to the pipeline.

**Data-aware, not just DAG-aware.** Steps declare output and input schemas. Cross-job contracts are checked at lint, diff, and apply time, before a producer breaks its consumers. Datasets carry freshness SLOs, so a job can run when data arrives instead of at a time you guessed. Runs can emit OpenLineage events to any compatible consumer.

**Free means free.** HA, RBAC, native OIDC / SAML / LDAP sign-in, an audit log, Kubernetes execution, lineage, and the console are in the open-source build, not behind an enterprise tier or a hosted service. See [docs/sovereignty.md](docs/sovereignty.md) for the comparison with Airflow, Dagster, Prefect, and Kestra.

## What's in the box

| | |
| --- | --- |
| **Pipelines** | YAML jobs with `lint`, `preview`, `diff`, and `apply`. Fan-out and fan-in, branching, dynamic fan-out from runtime partitions, retries with backoff, trigger rules, per-task and per-run timeouts, named volumes, `secret://` references. Git sync keeps a server in step with a repository. |
| **Triggers** | Cron. HTTP webhooks with HMAC, bearer, or basic auth and JSONPath parameter extraction. Events with content filters and job-to-job chaining. Freshness, to run when a dataset is stale. Backfills over a date range. Manual and API starts with idempotency keys. |
| **Execution** | Docker, Podman, and Kubernetes engines (with optional Kueue delegation). Run concurrency strategies (queue, replace, skip, fail), priorities, and shared rate limits. Content-addressed task cache with restart-from-failure. Distributed workers over a dqlite Raft cluster. `linux/amd64` and `linux/arm64`. |
| **Data** | Typed step outputs via a stdout marker, per-step schemas, cross-job contract enforcement, dataset freshness and watermarks, OpenLineage emission, lineage impact queries. |
| **Investigation** | `why`, `blame`, `run diff`, quarantined `run replay`, local `reproduce`, tamper-evident receipts with `verify`. Agent-assisted incident triage with approval-gated remediation, off by default. |
| **Operations** | Embedded console with live run updates, DAG and timeline views, log streaming, and backfill controls. REST API with an SSE event stream. Prometheus metrics. API keys with roles, native SSO, audit log. Helm chart. Terraform stacks as dependency-ordered DAGs via the reagent images. |

## Ask it what happened

Caesium keeps the execution record, so the questions you would otherwise answer by rereading logs have commands.

| Command | Answers |
| --- | --- |
| `caesium why <run-id> --task <task> --job-id <job-id>` | Why did this task run, get skipped, or fail? Field-level causes, not a status. |
| `caesium blame <job-id-or-alias>` | Which change broke the job: the manifest, an image, or an input? |
| `caesium run diff <left-run> <right-run> --job-id <job-id>` | What differed between two runs of the same job, and why? |
| `caesium run replay <run-id> --job-id <job-id> --set k=v` | Re-execute a completed run with overrides, in quarantine, with Caesium-side side effects suppressed. |
| `caesium reproduce <run-id> --job-id <job-id> --task <task>` | Rebuild one historical task on your own Docker daemon from its recorded descriptor. Add `--shell` to poke at it. |
| `caesium receipt get` / `caesium verify <receipt-file>` | A tamper-evident receipt for a task you can commit to git and check later. |
| `caesium contract check --path jobs/` | Will this manifest change break a downstream consumer's input schema? |
| `caesium dataset status <name>` | Is this dataset fresh, stale, or held? |
| `caesium backfill create --job-id <job-id> --start … --end …` | Replay a cron job over a range of logical dates. |

The walkthrough in [docs/getting-started.md](docs/getting-started.md) ends with `why` and a receipt against a real run.

## Quick start

**1. Install the CLI.** Linux gets a static binary; everything else runs the CLI inside the release image.

```bash
# Linux (amd64 or arm64)
curl -LO https://github.com/caesium-cloud/caesium/releases/download/v0.1.0/caesium-linux-amd64
curl -LO https://github.com/caesium-cloud/caesium/releases/download/v0.1.0/SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS
chmod +x caesium-linux-amd64 && sudo mv caesium-linux-amd64 /usr/local/bin/caesium
```

```bash
# macOS, or anywhere with Docker: a wrapper that runs the CLI in caesiumcloud/caesium:v0.1.0
git clone https://github.com/caesium-cloud/caesium.git && cd caesium
just tag=v0.1.0 cli        # writes ./.tmp/caesium-cli/caesium
```

**2. Run a server.** It needs the host container socket to launch task containers and a volume so the embedded database survives restarts.

```bash
docker run -d --name caesium-server -p 8080:8080 \
  -v /var/run/docker.sock:/var/run/docker.sock -e DOCKER_HOST=unix:///var/run/docker.sock \
  -v caesium-data:/var/lib/caesium --user 0:0 \
  caesiumcloud/caesium:v0.1.0 start
curl http://localhost:8080/health
```

The console is at [http://localhost:8080](http://localhost:8080). From a clone, `just run` builds and starts the same thing from source.

**3. Run a job locally, then apply it.** Start with [docs/examples/minimal.job.yaml](docs/examples/minimal.job.yaml), three sequential steps on the public `alpine:3.23` image. The Acme pipeline above needs images you build yourself.

```bash
curl -LO https://raw.githubusercontent.com/caesium-cloud/caesium/master/docs/examples/minimal.job.yaml
caesium dev --once --path minimal.job.yaml
caesium job apply --path minimal.job.yaml --server http://localhost:8080
```

On macOS the CLI runs inside a container, so call the generated wrapper and address the server through Docker Desktop's host alias:

```bash
./.tmp/caesium-cli/caesium dev --once --path docs/examples/minimal.job.yaml
./.tmp/caesium-cli/caesium job apply --path docs/examples/minimal.job.yaml --server http://host.docker.internal:8080
```

[docs/getting-started.md](docs/getting-started.md) continues from here: trigger a run, ask `why`, fetch a receipt, and covers the wrapper's socket and kubeconfig handling, Podman, and Kubernetes credentials in detail.

## Deploy

| Target | How |
| --- | --- |
| Single host | The `docker run` above, or the binary under systemd. See [docs/sovereignty.md](docs/sovereignty.md) for the air-gapped path. |
| Kubernetes | `helm install caesium ./helm/caesium`, and `--set replicaCount=3` for a Raft cluster. Backup, member replacement, Kueue delegation, and air-gapped image loading are in [docs/kubernetes-deployment.md](docs/kubernetes-deployment.md). |
| Multi-node, no Kubernetes | Point each binary at its peers with `CAESIUM_NODE_ADDRESS` and `CAESIUM_DATABASE_NODES`. Every execution and dqlite knob is in [docs/distributed-execution.md](docs/distributed-execution.md). |

Authentication, SSO, and the REST API are covered in [docs/sso-authentication.md](docs/sso-authentication.md) and [docs/rest-api.md](docs/rest-api.md).

## Documentation

| | |
| --- | --- |
| [docs/README.md](docs/README.md) | The index. Start here. |
| [docs/getting-started.md](docs/getting-started.md) | Install, run, write a job, ask `why`, get a receipt. |
| [docs/job-definitions.md](docs/job-definitions.md) | The authoring reference, with the generated [schema reference](docs/job-schema-reference.md) beside it. |
| [docs/caesium-job-llm-reference.md](docs/caesium-job-llm-reference.md) | A compact reference for coding assistants that write Caesium YAML. |
| [docs/distributed-execution.md](docs/distributed-execution.md) | Clusters, workers, dqlite, troubleshooting. |
| [docs/design/](docs/README.md#design-records) | One design record per feature, each with a status banner. |
| [docs/roadmap.md](docs/roadmap.md) | Design principles and what is next. |

## Status

Caesium is pre-1.0. The latest release is [v0.1.0](https://github.com/caesium-cloud/caesium/releases/tag/v0.1.0), with Linux binaries and a multi-arch image. The job schema is versioned (`apiVersion: v1`) and [docs/upgrade-notes.md](docs/upgrade-notes.md) lists behavior changes between releases. Current work is the closed-loop arc (data circuit breaker, resource right-sizing, backtesting), namespaces and policy-as-code for corporate SSO, and execution connectors; the plans are indexed in [docs/README.md](docs/README.md#active-execution-plans).

Caesium is open source and not a managed service. There is no cloud tier; every feature is designed for self-hosting.

## Building from source

```bash
just build              # release image for the host platform (containerized; no host Go toolchain needed)
just run                # start it
just unit-test          # Go unit tests with the race detector
just integration-test   # drives the CLI and REST API against a real server
```

[CONTRIBUTING.md](CONTRIBUTING.md) has the full workflow, the UI and Helm checks, and a walkthrough for adding triggers, runtimes, endpoints, and console pages. Questions go to [GitHub Discussions](https://github.com/caesium-cloud/caesium/discussions).

## License

[Apache 2.0](LICENSE).
