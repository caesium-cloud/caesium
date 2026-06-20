# Sovereignty: What Caesium Ships Free vs. What Competitors Paywall

Caesium is a **single zero-dependency Go binary** with an embedded dqlite database. It runs where Dagster, Airflow, Flyte, and Kestra architecturally cannot — air-gapped, edge, regulated on-prem, factory floors, SCIFs — and ships every feature they paywall, free.

This document covers two things:

1. **Feature comparison** — what Caesium ships free vs. what competitors paywall or require a managed control plane for.
2. **Zero-dependency / air-gapped quickstart** — the concrete steps to run Caesium in a network-isolated environment.

---

## Free vs. Paywalled: Feature Comparison

The following table compares Caesium against the open-core data orchestrators engineers most commonly evaluate. "Paywalled" means the feature is gated behind a paid tier, a managed cloud offering, or an enterprise license. "Requires managed control plane" means the OSS version mandates a hosted service the vendor operates. Claims are grounded in each project's published pricing and documentation as of mid-2026.

| Feature | Caesium | Dagster | Kestra | Prefect | Airflow |
|---|---|---|---|---|---|
| **High availability / multi-node** | Free (RAFT via dqlite, `replicaCount=3`) | Dagster+ (paid) | Enterprise (paid) | Prefect Cloud or self-host w/ Postgres+Redis | Free (but requires Postgres + Celery/K8s executor) |
| **RBAC** | Free (shipped) | Dagster+ (paid) | Enterprise (paid) | Free (limited) / Prefect Cloud (full) | Free (but community-maintained; limited) |
| **SSO (OIDC / SAML / LDAP)** | Free (shipped; all three native) | Dagster+ (paid) | Enterprise (paid) | Prefect Cloud (paid tier) | Free (but requires third-party proxy or custom plugin) |
| **Audit log** | Free (shipped) | Dagster+ (paid) | Enterprise (paid) | Prefect Cloud (paid) | Free (but no structured audit trail out of the box) |
| **Kubernetes execution** | Free (shipped; Helm chart) | Free (OSS) | Enterprise (paid) | Free (Kubernetes work pool) | Free (K8s executor) |
| **Data lineage / OpenLineage** | Free (shipped; OpenLineage transport) | Dagster+ (paid Insights/Catalog) | Enterprise (paid) | Not native | Astronomer (paid) |
| **Single binary / zero external deps** | **Yes** (Go binary + embedded dqlite) | No (Postgres required) | No (JVM + Postgres + Redis) | No (Postgres + Redis) | No (Postgres + broker + executor) |
| **Air-gapped / offline operation** | **Yes** (no outbound network required at runtime) | No (control plane phones home) | No (JVM telemetry; external DB) | No (cloud-first architecture) | Possible, but multi-process complexity |
| **Self-hosted with no vendor account** | **Yes** | OSS tier (limited features) | OSS tier (limited features) | Limited (some features require account) | Yes |
| **Data-plane memory (explain/reproduce/skip)** | Free (shipped; see [`design-data-plane-memory.md`](design-data-plane-memory.md)) | Paid (Dagster+ Insights) | Not available | Not available | Not available |

### What this means in practice

- **Dagster** and **Kestra** are the most dangerous lookalikes: both are YAML-friendly, both have strong OSS communities. But Dagster gates HA, RBAC, SSO, and its observability/catalog products behind Dagster+. Kestra's OSS edition is a 5-process JVM platform (Kafka + Postgres + Elasticsearch + Zookeeper + the application server) — it mandates a database before a single job runs. Neither can become "`scp` one binary to an air-gapped node and it just runs" without detonating their own dependency graphs.
- **Prefect**'s best features (scheduling UI, SSO, teams, audit) live in Prefect Cloud. The self-hosted path is functional but the platform engineers running regulated workloads most often ask: "can I air-gap it?" — and the answer is effectively no.
- **Airflow** is free and self-hosted, but its architecture mandates a metadata database and a broker (Celery or KubernetesExecutor), plus a separate scheduler process. An air-gapped Airflow installation is possible in theory but requires maintaining Postgres, a broker, and all of Airflow's Python dependency surface offline. Airflow 3.0 does not change this picture.
- **Flyte** requires Kubernetes plus a control plane plus an object store plus an RDBMS. It is not the right tool for edge or constrained environments.

---

## Zero-Dependency / Air-Gapped Quickstart

This quickstart is for engineers who need to run a pipeline scheduler in a network-isolated environment: an air-gapped SCIF, a factory floor, a regulated bank network, an edge device, or any host without outbound internet access. It is also the fastest possible path to a running Caesium instance anywhere.

Common search terms that land here: *lightweight Airflow alternative no database*, *self-hosted orchestrator no Postgres*, *air-gapped pipeline scheduler*.

### What you need

- A single Linux host (x86_64 or ARM64). No Kubernetes, no Docker daemon, no database, no broker.
- A copy of the Caesium binary (one file, ~30–50 MB).
- Your job definition YAML files.
- (Optional) Docker or Podman installed on the host if your jobs run containers locally.

### Step 1: Copy the binary

Transfer the binary to the target host by any means available: `scp`, USB, internal artifact mirror, sneakernet.

```bash
# From a host with internet access — download the release binary
curl -fsSL https://github.com/caesium-cloud/caesium/releases/latest/download/caesium-linux-amd64 \
  -o caesium
chmod +x caesium

# Transfer to the air-gapped host
scp caesium user@airgapped-host:/usr/local/bin/caesium
```

For ARM64:

```bash
curl -fsSL https://github.com/caesium-cloud/caesium/releases/latest/download/caesium-linux-arm64 \
  -o caesium
```

### Step 2: Start the server

On the air-gapped host, no configuration is required for a single-node deployment:

```bash
caesium server
```

Caesium creates its embedded dqlite database in `$HOME/.caesium/` (or `CAESIUM_DATA_DIR` if set) and starts listening on port 8080. No Postgres, no Redis, no Kafka, no network egress.

Verify it is running:

```bash
curl -sf http://127.0.0.1:8080/health
```

### Step 3: Write a job definition

Create a job YAML file on the host. Example — a daily data pipeline with two steps:

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: nightly-etl
trigger:
  cron: "0 2 * * *"
steps:
  - name: extract
    image: my-registry.internal/etl-extract:1.4.2
    command: ["python", "extract.py"]
  - name: transform
    image: my-registry.internal/etl-transform:1.4.2
    command: ["python", "transform.py"]
```

Images must be reachable from the host. In an air-gapped environment, either:

- Use images already present on the host / in a local registry, or
- Pre-load images into Docker/Podman before starting (see [Air-Gapped Kubernetes Notes](kubernetes-deployment.md#air-gapped-deployment-notes) for the Kubernetes path).

For image digest pinning in air-gapped environments, see [`cache.pinDigests`](design-data-plane-memory.md) — this ensures the cache key is computed from a content-addressed digest rather than a mutable tag, which matters for regulated environments where reproducibility is auditable.

### Step 4: Validate and deploy the job

```bash
# Validate schema and DAG locally (no server required)
caesium job lint --path nightly-etl.job.yaml

# Preview the DAG
caesium job preview --path nightly-etl.job.yaml

# Deploy to the local server
caesium job apply --path nightly-etl.job.yaml
```

### Step 5: (Optional) High availability

For a resilient 3-node cluster — still with no external dependencies — copy the binary to three hosts and configure them to form a RAFT cluster via environment variables:

```bash
# On each node, set the peer list
export CAESIUM_DATABASE_NODES="node1:9001,node2:9001,node3:9001"
export CAESIUM_NODE_ID="node1"  # unique per node
caesium server
```

All three nodes use embedded dqlite with RAFT consensus. No external coordinator required.

### What Caesium does not need

| Dependency | Caesium needs it? | Notes |
|---|---|---|
| PostgreSQL / MySQL / SQLite (external) | No | Embedded dqlite |
| Redis / Valkey | No | — |
| Kafka / RabbitMQ / NATS | No | — |
| Object storage (S3, GCS, Minio) | No | — |
| Zookeeper / etcd | No | — |
| Python runtime | No | Server is a Go binary |
| JVM | No | — |
| Outbound internet access | No | — |
| Vendor account / license server | No | Pure OSS, Apache 2.0 |

### Upgrades in an air-gapped environment

Upgrading Caesium is a binary swap. Stop the server, replace the binary, restart. The embedded dqlite schema migrates automatically at startup (`AutoMigrate`). No migration scripts, no database admin, no downtime window coordination across external services.

```bash
# Transfer the new binary
scp caesium-new user@airgapped-host:/usr/local/bin/caesium-next
ssh user@airgapped-host "mv /usr/local/bin/caesium-next /usr/local/bin/caesium"

# Restart (systemd example)
ssh user@airgapped-host "systemctl restart caesium"
```

---

## Related Documents

- [`differentiation-strategy.md`](differentiation-strategy.md) — the full positioning thesis: why sovereignty sells by constraint, not comparison, and the kill-conditions that test the thesis.
- [`kubernetes-deployment.md`](kubernetes-deployment.md) — Helm-based Kubernetes deployment, including air-gapped cluster notes.
- [`design-data-plane-memory.md`](design-data-plane-memory.md) — the data-plane memory substrate: `caesium why`, reproducibility receipts, and value-verified skip — the "second act" differentiator within sovereignty.
- [`sso-authentication.md`](sso-authentication.md) — native OIDC, SAML, and LDAP SSO configuration.
