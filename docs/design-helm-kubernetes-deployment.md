# Design: Helm Charts for Kubernetes Deployment

## Status

Implemented (pending live-cluster validation)

## Problem

Caesium is currently deployed as a standalone Docker container, launched manually via `docker run` or equivalent. While dqlite already supports multi-node RAFT consensus clustering, there is no automated way to:

1. Deploy Caesium to a Kubernetes cluster (even a local Docker Desktop one)
2. Scale to a multi-node RAFT cluster with proper peer discovery
3. Integrate Kubernetes deployment into CI/CD

This design adds a Helm chart that supports both single-node and multi-node (RAFT cluster) deployments, with a stretch goal of deploying to a Kubernetes cluster from CircleCI.

## Current State

### How Caesium runs today

| Aspect | Current | Target |
|---|---|---|
| Deployment | Manual `docker run` | `helm install caesium ./helm/caesium` |
| Scaling | Single node | 1–N nodes via StatefulSet |
| Peer discovery | Manual `CAESIUM_DATABASE_NODES` env var | Automatic via headless Service DNS |
| Storage | Docker volume or bind mount | Kubernetes PersistentVolumeClaims |
| Health checks | `GET /health` (HTTP 200/503) | Mapped to K8s liveness/readiness probes |
| Networking | Host port mapping | ClusterIP Service + optional Ingress |

### Relevant application configuration

All configuration is via `CAESIUM_*` environment variables (processed by `pkg/env/env.go`):

| Variable | Default | Helm Relevance |
|---|---|---|
| `CAESIUM_PORT` | `8080` | Container/Service port |
| `CAESIUM_NODE_ADDRESS` | `127.0.0.1:9001` | Must be set per-pod to `$(POD_NAME).$(HEADLESS_SVC):9001` |
| `CAESIUM_DATABASE_NODES` | (empty) | Populated with peer pod DNS names for cluster join |
| `CAESIUM_DATABASE_PATH` | `/var/lib/caesium/dqlite` | PVC mount path |
| `CAESIUM_LOG_LEVEL` | `info` | Configurable via values.yaml |

### Existing infrastructure

- **Docker images**: Multi-arch (amd64/arm64) published to `caesiumcloud/caesium:<tag>`
- **Health endpoint**: `GET /health` returns 200 when healthy, 503 when degraded (checks database connectivity)
- **Ports**: 8080 (HTTP API), 9001 (dqlite RAFT)
- **Non-root user**: Container runs as UID 10001
- **CI/CD**: CircleCI with multi-arch build, unit tests, integration tests, and Docker Hub publish

## Design

### Directory structure

```
helm/
└── caesium/
    ├── Chart.yaml
    ├── values.yaml
    ├── templates/
    │   ├── _helpers.tpl
    │   ├── statefulset.yaml
    │   ├── service.yaml            # ClusterIP for API access
    │   ├── service-headless.yaml   # Headless for peer discovery
    │   ├── configmap.yaml
    │   ├── ingress.yaml            # Optional
    │   ├── serviceaccount.yaml
    │   ├── pdb.yaml                # PodDisruptionBudget
    │   ├── NOTES.txt
    │   └── tests/
    │       └── test-connection.yaml
    └── ci/
        └── test-values.yaml        # Values used in CI smoke tests
```

### Why StatefulSet (not Deployment)

Caesium's dqlite RAFT cluster requires:

1. **Stable network identities** — each node needs a predictable DNS name so peers can find each other (e.g., `caesium-0.caesium-headless.default.svc.cluster.local:9001`)
2. **Stable persistent storage** — each pod gets its own PVC for the dqlite data directory; rescheduling must reattach the same volume
3. **Ordered startup** — the first pod (`caesium-0`) bootstraps the cluster; subsequent pods join it

These are the exact semantics a StatefulSet provides. A Deployment cannot guarantee any of these.

### Peer discovery strategy

dqlite requires `CAESIUM_DATABASE_NODES` to contain the addresses of existing cluster members when a node joins. The Helm chart uses a **headless Service** plus an **init container** to handle this:

```
Pod: caesium-0  (bootstrap node — DATABASE_NODES left empty)
Pod: caesium-1  (joins with DATABASE_NODES=caesium-0.caesium-headless:9001)
Pod: caesium-2  (joins with DATABASE_NODES=caesium-0.caesium-headless:9001,caesium-1.caesium-headless:9001)
```

**Init container logic** (shell script in ConfigMap):

```bash
#!/bin/sh
# Determine this pod's ordinal from the hostname (e.g., caesium-0 → 0)
ORDINAL=${HOSTNAME##*-}

if [ "$ORDINAL" -eq 0 ]; then
  # Bootstrap node — no peers to join
  echo "" > /etc/caesium/database-nodes
else
  # Build a comma-separated list of ALL lower-ordinal peers.
  # This avoids a single point of failure on pod-0: if pod-0 is
  # temporarily unavailable, the joining node can reach any other
  # existing cluster member. CAESIUM_DATABASE_NODES is a []string,
  # so dqlite will try each address in turn.
  PEERS=""
  i=0
  while [ "$i" -lt "$ORDINAL" ]; do
    ADDR="${RELEASE_NAME}-${i}.${HEADLESS_SVC}.${NAMESPACE}.svc.cluster.local:9001"
    if [ -z "$PEERS" ]; then
      PEERS="$ADDR"
    else
      PEERS="${PEERS},${ADDR}"
    fi
    i=$((i + 1))
  done
  echo "$PEERS" > /etc/caesium/database-nodes
fi
```

The main container reads this file via an `emptyDir` shared volume and sets `CAESIUM_DATABASE_NODES` accordingly.

Each pod's own `CAESIUM_NODE_ADDRESS` is set using the downward API:

```yaml
env:
  - name: POD_NAME
    valueFrom:
      fieldRef:
        fieldPath: metadata.name
  - name: CAESIUM_NODE_ADDRESS
    value: "$(POD_NAME).{{ headless-service }}.{{ namespace }}.svc.cluster.local:9001"
```

### Health probes

The existing `/health` endpoint maps directly to Kubernetes probes:

```yaml
livenessProbe:
  httpGet:
    path: /health
    port: http
  initialDelaySeconds: 15
  periodSeconds: 20
  failureThreshold: 3

readinessProbe:
  httpGet:
    path: /health
    port: http
  initialDelaySeconds: 5
  periodSeconds: 10
  failureThreshold: 3

startupProbe:
  httpGet:
    path: /health
    port: http
  initialDelaySeconds: 5
  periodSeconds: 5
  failureThreshold: 30  # Allow up to 150s for dqlite cluster formation
```

A startup probe is important because dqlite cluster join can take time, especially on the first boot. Without it, the liveness probe would kill the pod before it finishes joining.

### values.yaml design

```yaml
# -- Number of Caesium replicas (1 = standalone, 3+ = RAFT cluster)
replicaCount: 1

image:
  repository: caesiumcloud/caesium
  tag: ""          # Defaults to Chart.appVersion
  pullPolicy: IfNotPresent

imagePullSecrets: []
nameOverride: ""
fullnameOverride: ""

serviceAccount:
  create: true
  annotations: {}
  name: ""

# -- Caesium application configuration
config:
  logLevel: info
  port: 8080
  dqlitePort: 9001
  maxParallelTasks: 1
  # -- Database backend: "internal" (dqlite) or "postgres"
  databaseType: internal
  # -- PostgreSQL DSN (only used when databaseType=postgres)
  databaseDSN: ""
  # -- Additional environment variables
  extraEnv: []

# -- Persistent storage for dqlite data
persistence:
  enabled: true
  storageClass: ""   # Use cluster default
  accessModes:
    - ReadWriteOnce
  size: 1Gi

service:
  type: ClusterIP
  port: 8080

# -- Headless service for StatefulSet peer discovery (always created)
headlessService:
  port: 9001

ingress:
  enabled: false
  className: ""
  annotations: {}
  hosts:
    - host: caesium.local
      paths:
        - path: /
          pathType: Prefix
  tls: []

resources: {}
  # requests:
  #   cpu: 100m
  #   memory: 128Mi
  # limits:
  #   cpu: 500m
  #   memory: 256Mi

podDisruptionBudget:
  enabled: false
  # -- Minimum available pods (set to majority for RAFT quorum, e.g., 2 for a 3-node cluster)
  minAvailable: ""
  maxUnavailable: 1

nodeSelector: {}
tolerations: []
affinity: {}

# -- Pod-level security context
podSecurityContext:
  fsGroup: 10001

# -- Container-level security context
securityContext:
  runAsUser: 10001
  runAsGroup: 10001
  runAsNonRoot: true
  readOnlyRootFilesystem: true
  allowPrivilegeEscalation: false
```

### Single-node vs. multi-node

The chart works out of the box with `replicaCount: 1` for local development. For a RAFT cluster:

```bash
# Local Docker Desktop — 3-node cluster
helm install caesium ./helm/caesium --set replicaCount=3

# With persistence disabled (ephemeral, for testing)
helm install caesium ./helm/caesium --set replicaCount=3 --set persistence.enabled=false
```

When `replicaCount=1`, the init container still runs but simply produces an empty `DATABASE_NODES`, so the single node bootstraps itself.

### Security model

- Container runs as non-root (UID 10001, matching the existing Dockerfile)
- Read-only root filesystem with `emptyDir` for `/tmp` and `/etc/caesium`
- No privilege escalation
- ServiceAccount with no extra RBAC (caesium doesn't need to talk to the Kubernetes API for its own deployment — the K8s execution backend is a separate concern configured independently)
- Network policy can be layered on by the operator

## Implementation Plan

### Phase 1: Helm chart scaffold and single-node deployment

> Goal: `helm install caesium ./helm/caesium` works on Docker Desktop Kubernetes

- [x] **1.1** Create `helm/caesium/Chart.yaml` with chart metadata (name: caesium, version: 0.1.0, appVersion matching latest release tag)
- [x] **1.2** Create `helm/caesium/values.yaml` with the defaults defined above
- [x] **1.3** Create `helm/caesium/templates/_helpers.tpl` with standard name/label/selector helpers
- [x] **1.4** Create `helm/caesium/templates/serviceaccount.yaml`
- [x] **1.5** Create `helm/caesium/templates/configmap.yaml` containing the init container peer-discovery script
- [x] **1.6** Create `helm/caesium/templates/service.yaml` (ClusterIP, port 8080)
- [x] **1.7** Create `helm/caesium/templates/service-headless.yaml` (headless, port 9001, `clusterIP: None`)
- [x] **1.8** Create `helm/caesium/templates/statefulset.yaml` with:
  - Init container running the peer-discovery script
  - Main container with health probes (liveness, readiness, startup)
  - `CAESIUM_NODE_ADDRESS` set via downward API
  - `CAESIUM_DATABASE_NODES` read from init container output
  - PVC template for dqlite data (when `persistence.enabled=true`)
  - `emptyDir` volumes for `/tmp` and `/etc/caesium`
  - Security contexts matching the non-root Dockerfile user
- [x] **1.9** Create `helm/caesium/templates/NOTES.txt` with post-install instructions
- [x] **1.10** Create `helm/caesium/templates/tests/test-connection.yaml` (helm test pod that curls `/health`)
- [x] **1.11** Run `helm lint ./helm/caesium` to validate
- [ ] **1.12** Test single-node install on Docker Desktop: `helm install caesium ./helm/caesium`
- [ ] **1.13** Verify `/health` returns 200 via `kubectl port-forward`

### Phase 2: Multi-node RAFT cluster support

> Goal: `helm install caesium ./helm/caesium --set replicaCount=3` brings up a working 3-node dqlite cluster

- [ ] **2.1** Test 3-node deployment on Docker Desktop (`--set replicaCount=3`)
- [ ] **2.2** Verify RAFT cluster formation — all 3 pods reach Ready state
- [ ] **2.3** Validate that data written via the leader is readable from any node (port-forward to different pods)
- [x] **2.4** Create `helm/caesium/templates/pdb.yaml` with PodDisruptionBudget (conditional on `podDisruptionBudget.enabled`)
- [ ] **2.5** Test pod deletion recovery — kill one pod and verify the cluster re-heals
- [x] **2.6** Add `helm/caesium/templates/ingress.yaml` (conditional on `ingress.enabled`)

### Phase 3: CI/CD integration (just-based local workflow)

> Goal: `just helm-lint` and `just helm-test` work locally and in CI

- [x] **3.1** Add justfile recipes:
  - `helm-lint` — runs `helm lint ./helm/caesium`
  - `helm-template` — renders templates to stdout for visual inspection
  - `helm-test` — runs `helm test caesium` (assumes a running cluster)
- [x] **3.2** Create `helm/caesium/ci/test-values.yaml` with CI-specific overrides (e.g., `persistence.enabled: false`, reduced resources)
- [x] **3.3** Add a `helm-lint` job to `.circleci/config.yml`:
  - Uses a `docker` executor with a Helm image (no Kubernetes cluster needed)
  - Runs `helm lint` and `helm template` to catch template errors
  - Runs on the free tier (no machine executor needed)

### Phase 4: CI/CD Kubernetes deployment (stretch goal)

> Goal: Deploy Caesium to a real Kubernetes cluster from CircleCI

This phase explores whether a full in-CI Kubernetes deployment is feasible. Options ranked by practicality:

#### Option A: kind (Kubernetes IN Docker) — Recommended

CircleCI's free tier provides `machine` executors with Docker. [kind](https://kind.sigs.k8s.io/) runs a full Kubernetes cluster inside Docker containers, requiring no cloud resources.

- [x] **4.1** Add a `helm-integration-test` job to `.circleci/config.yml`:
  ```yaml
  helm-integration-test:
    machine:
      image: ubuntu-2404:current
    resource_class: medium
    steps:
      - checkout
      - run:
          name: Install kind, kubectl, helm
          command: |
            # Install kind
            curl -Lo ./kind https://kind.sigs.k8s.io/dl/latest/kind-linux-amd64
            chmod +x ./kind && sudo mv ./kind /usr/local/bin/kind
            # Install kubectl
            curl -LO "https://dl.k8s.io/release/$(curl -sL https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
            chmod +x kubectl && sudo mv kubectl /usr/local/bin/kubectl
            # Install helm
            curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
      - run:
          name: Create kind cluster
          command: kind create cluster --wait 120s
      - run:
          name: Load Caesium image into kind
          command: |
            IMAGE_TAG=${CIRCLE_TAG:-$CIRCLE_SHA1}-amd64
            kind load docker-image caesiumcloud/caesium:$IMAGE_TAG
      - run:
          name: Helm install (single node)
          command: |
            IMAGE_TAG=${CIRCLE_TAG:-$CIRCLE_SHA1}-amd64
            helm install caesium ./helm/caesium \
              --set image.tag=$IMAGE_TAG \
              --set persistence.enabled=false \
              --wait --timeout 120s
      - run:
          name: Helm test
          command: helm test caesium --timeout 60s
      - run:
          name: Verify health endpoint
          command: |
            kubectl port-forward svc/caesium 8080:8080 &
            PORT_FORWARD_PID=$!
            trap 'kill $PORT_FORWARD_PID' EXIT
            # Poll until the health endpoint is reachable (or timeout after 15s)
            tries=0
            until curl -sf http://localhost:8080/health > /dev/null; do
              tries=$((tries+1))
              if [ "$tries" -gt 15 ]; then
                echo "Port-forward did not become ready in time" >&2
                exit 1
              fi
              sleep 1
            done
            curl -sf http://localhost:8080/health
  ```
- [ ] **4.2** (Optional) Extend to test `replicaCount=3` in CI if time/resources allow
- [x] **4.3** Wire the job into the CircleCI workflow after `build-and-integration-test`

#### Option B: Remote cluster (not recommended for free tier)

Would require a persistent Kubernetes cluster (e.g., GKE, EKS) and service account credentials stored in CircleCI. Not feasible on free tier due to cloud costs, but documented here for future reference.

### Phase 5: Documentation

- [x] **5.1** Add a `docs/kubernetes-deployment.md` user guide covering:
  - Prerequisites (Docker Desktop with K8s enabled, or any K8s cluster)
  - Quick start: single-node `helm install`
  - Multi-node: RAFT cluster setup with `replicaCount=3`
  - Configuration reference (all values.yaml options)
  - Accessing the API (port-forward, Ingress, LoadBalancer)
  - Persistence and backup considerations
  - Troubleshooting (pod logs, dqlite cluster status)
- [x] **5.2** Update the project README to mention Helm/Kubernetes deployment

## Key Design Decisions

### 1. Init container for peer discovery (not a sidecar or operator)

**Chosen**: Init container with a simple shell script that writes peer addresses based on pod ordinal.

**Alternatives considered**:
- **Sidecar controller**: More complex, watches pod state — overkill for a static StatefulSet
- **Custom operator**: Maximum flexibility but massive implementation cost
- **DNS SRV lookup at app startup**: Would require application code changes

The init container approach is simple, requires zero application changes, and works perfectly with StatefulSet's ordered pod naming.

### 2. Headless Service for RAFT, ClusterIP for API

Two services cleanly separate concerns:
- **Headless** (`clusterIP: None`): Gives each pod a stable DNS A record (`caesium-0.caesium-headless...`) for dqlite peer-to-peer communication. No load balancing — RAFT needs direct pod-to-pod connections.
- **ClusterIP**: Load-balances HTTP API traffic across all pods. Clients don't need to know about individual pods.

### 3. kind for CI Kubernetes testing

**Chosen**: kind (Kubernetes IN Docker) over alternatives.

| Option | Pros | Cons |
|---|---|---|
| **kind** | Free, runs in Docker (available on CircleCI machine executors), full K8s conformance | ~60s startup, uses RAM |
| **k3s** | Lightweight, fast startup | Less conformant, different networking model |
| **minikube** | Feature-rich | Requires VM driver or Docker, heavier than kind |
| **Remote cluster** | Production-like | Costs money, credential management |

kind is the standard for Helm chart CI testing and fits perfectly in CircleCI's machine executor.

### 4. dqlite-only for Helm (not PostgreSQL)

The Helm chart targets the `internal` (dqlite) database backend because:
- It's self-contained — no external database dependency
- It leverages the RAFT clustering that this design is specifically enabling
- PostgreSQL deployments would typically use a managed database service, not a Helm-managed StatefulSet

Users who want PostgreSQL can set `config.databaseType: postgres` and `config.databaseDSN` and use `replicaCount: 1` (or multiple replicas with an external PostgreSQL — the RAFT layer becomes irrelevant in that case).

## Risks and Mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| dqlite cluster fails to form on K8s | High | Startup probe gives 150s; init container lists all lower-ordinal peers so join succeeds even if pod-0 is temporarily down; single-node works as fallback |
| PVC provisioner not available (e.g., bare metal) | Medium | `persistence.enabled: false` allows ephemeral testing; storageClass is configurable |
| CircleCI free tier limits | Low | Helm lint needs no machine executor; kind tests reuse existing machine executor pattern |
| Pod rescheduling loses quorum | Medium | PDB ensures majority remains during voluntary disruptions; dqlite handles temporary member loss |
| Docker Desktop K8s has limited resources | Low | Default `replicaCount: 1`; resource requests are optional |
