# Kubernetes Deployment with Helm

This guide covers deploying Caesium on Kubernetes using the Helm chart at `helm/caesium`.

## Prerequisites

- Kubernetes cluster access (Docker Desktop Kubernetes, kind, or managed Kubernetes)
- `kubectl` configured for the target cluster
- Helm 3.x installed
- A Caesium image tag available in Docker Hub (`caesiumcloud/caesium:<tag>`) or loaded into the cluster runtime

## Quick Start (Single Node)

Install a single-node Caesium instance:

```bash
helm install caesium ./helm/caesium
```

Wait for readiness:

```bash
kubectl get pods -l app.kubernetes.io/instance=caesium
```

Verify health:

```bash
kubectl port-forward service/caesium 8080:8080
curl -sf http://127.0.0.1:8080/health
```

Upgrade an existing release:

```bash
helm upgrade caesium ./helm/caesium
```

Uninstall:

```bash
helm uninstall caesium
```

## Multi-Node RAFT Cluster

Deploy a 3-node dqlite-backed RAFT cluster:

```bash
helm install caesium ./helm/caesium --set replicaCount=3
```

For ephemeral testing without PVCs:

```bash
helm install caesium ./helm/caesium \
  --set replicaCount=3 \
  --set persistence.enabled=false
```

Check all pods become Ready:

```bash
kubectl get pods -l app.kubernetes.io/instance=caesium -w
```

Inspect per-pod peer discovery output:

```bash
kubectl exec caesium-1 -- cat /etc/caesium/database-nodes
kubectl exec caesium-2 -- cat /etc/caesium/database-nodes
```

## Configuration Reference

All settings are in `helm/caesium/values.yaml`.

| Key | Purpose | Default |
|---|---|---|
| `replicaCount` | Number of Caesium replicas | `1` |
| `image.repository` | Container image repository | `caesiumcloud/caesium` |
| `image.tag` | Container image tag (defaults to chart `appVersion`) | `""` |
| `image.pullPolicy` | Kubernetes image pull policy | `IfNotPresent` |
| `serviceAccount.create` | Create a dedicated ServiceAccount | `true` |
| `config.logLevel` | `CAESIUM_LOG_LEVEL` | `info` |
| `config.port` | HTTP API/container port (`CAESIUM_PORT`) | `8080` |
| `config.dqlitePort` | dqlite RAFT port | `9001` |
| `config.maxParallelTasks` | `CAESIUM_MAX_PARALLEL_TASKS` | `1` |
| `config.databaseType` | `internal` (dqlite) or `postgres` | `internal` |
| `config.databaseDSN` | PostgreSQL DSN when using `postgres` | `""` |
| `config.extraEnv` | Extra env vars injected into pod spec | `[]` |
| `persistence.enabled` | Enable PVC-backed dqlite storage | `true` |
| `persistence.storageClass` | StorageClass name (empty = cluster default) | `""` |
| `persistence.accessModes` | PVC access modes | `[ReadWriteOnce]` |
| `persistence.size` | PVC size | `1Gi` |
| `service.type` | API service type | `ClusterIP` |
| `service.port` | API service port | `8080` |
| `headlessService.port` | Headless peer-discovery service port | `9001` |
| `ingress.enabled` | Enable Ingress resource | `false` |
| `ingress.className` | Ingress class | `""` |
| `ingress.hosts` | Ingress host/path mappings | `caesium.local` |
| `resources` | CPU/memory requests and limits | `{}` |
| `podDisruptionBudget.enabled` | Enable PodDisruptionBudget | `false` |
| `podDisruptionBudget.minAvailable` | Minimum available pods | `""` |
| `podDisruptionBudget.maxUnavailable` | Maximum unavailable pods | `1` |
| `nodeSelector` | Node selector constraints | `{}` |
| `tolerations` | Pod tolerations | `[]` |
| `affinity` | Pod affinity/anti-affinity rules | `{}` |
| `podSecurityContext.fsGroup` | Pod-level fsGroup | `10001` |
| `securityContext.*` | Container security settings | non-root + read-only rootfs |

## Accessing the API

### Port-forward (default)

```bash
kubectl port-forward service/caesium 8080:8080
curl -sf http://127.0.0.1:8080/health
```

### LoadBalancer service

```bash
helm upgrade caesium ./helm/caesium --set service.type=LoadBalancer
kubectl get svc caesium -w
```

### Ingress

```bash
helm upgrade caesium ./helm/caesium \
  --set ingress.enabled=true \
  --set ingress.className=nginx \
  --set ingress.hosts[0].host=caesium.local
```

## Persistence and Backup

- dqlite data is stored under `/var/lib/caesium/dqlite` in each pod.
- With `StatefulSet` + PVCs enabled, each ordinal gets a stable volume.
- For backup/restore, snapshot or back up the PVCs using your storage platform tooling.
- If `persistence.enabled=false`, all data is ephemeral and lost on pod restart/recreation.

## Troubleshooting

Check release resources:

```bash
kubectl get all,pvc -l app.kubernetes.io/instance=caesium
```

Inspect logs:

```bash
kubectl logs statefulset/caesium -c caesium
kubectl logs pod/caesium-0 -c peer-discovery
```

Describe a failing pod:

```bash
kubectl describe pod caesium-0
```

Render chart locally before install:

```bash
just helm-lint
just helm-template
```

Run Helm test hook:

```bash
helm test caesium --timeout 120s
```
