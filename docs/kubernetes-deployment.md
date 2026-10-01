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

This pulls `caesiumcloud/caesium:<Chart.yaml appVersion>`. To deploy a
different image — a locally built one, or a tag other than the chart's pinned
release — set it explicitly:

```bash
helm install caesium ./helm/caesium --set image.tag=<tag>
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

### Quorum health

`/health` reports raft membership and liveness separately, under
`checks.cluster`. Membership is what the cluster was configured with;
liveness is what actually answered a bounded dqlite RPC:

```bash
kubectl port-forward service/caesium 8080:8080
curl -s http://127.0.0.1:8080/health | jq '.status, .checks.cluster.quorum'
```

```json
{
  "status": "degraded",
  "total_voters": 3,
  "reachable_voters": 2,
  "unreachable_voters": 1,
  "required_voters": 2,
  "available": true,
  "degraded": true,
  "leader_address": "10.244.0.8:9001"
}
```

- `available` — a majority of voters answered, so the cluster can serve writes.
- `degraded` — it is serving with less than full redundancy, or redundancy could
  not be confirmed.
- `status: "unavailable"` — fewer voters answered than a majority requires.
- `status: "unknown"` — liveness could not be determined. This is never
  reported as healthy.

The console's `/system` page renders `reachable_voters / total_voters`, so a
two-of-three cluster shows as `2/3` and DEGRADED rather than as fully
operational.

### Probe endpoints

The **body** describes the cluster. The **HTTP status code** describes only this
replica, which is why the two Kubernetes probes point at different endpoints:

| Endpoint | Question | Probe | Fails when |
| --- | --- | --- | --- |
| `/health/ready` | Can this replica serve? | readiness, startup | its own database check fails (503) |
| `/health/live` | Is this process running? | liveness | never, while it answers HTTP |
| `/health` | Both, for the console | — | same as `/health/ready` |

The split matters during a partition. A replica whose dqlite traffic is cut off
from its peers sees a quorum loss locally while the others carry on serving. It
must leave the Service endpoints — otherwise traffic keeps arriving at a node
that cannot answer — but it must *not* be restarted: restarting cures a
deadlocked process, never a dependency, and cannot restore a raft majority.

`/health/live` therefore touches no dependency at all, and `/health/ready`
returns 503 whenever this node cannot serve, whatever the cluster looks like.
The degraded or unavailable quorum assessment is present in the body either way,
so the console can explain an outage that the status code alone cannot.

Both probe endpoints are unauthenticated, like `/health`.

Liveness is probed in the background and served from a short-lived cache, so
`/health` never blocks on cluster RPCs. `checks.cluster.observed_at` carries the
observation time and `checks.cluster.stale` marks a result older than the
refresh interval.

## Configuration Reference

All settings are in `helm/caesium/values.yaml`.

> **Chart versioning.** `image.tag` defaults to the chart's `appVersion`
> (`helm/caesium/Chart.yaml`), which is pinned to a released image tag — so an
> un-overridden `helm install` deploys exactly that release. **Every `v*` tag
> bumps `Chart.yaml` `appVersion` to the tag, and `version` per semver, in the
> same PR**; the `publish` job in `.github/workflows/ci.yml` fails the release
> if `appVersion` does not equal the pushed tag. Override `image.tag`
> explicitly (as CI's `helm-integration-test` lane and `just k8s-distributed`
> do) whenever you deploy an image that is not a published release.

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
| `config.maxParallelTasks` | `CAESIUM_MAX_PARALLEL_TASKS` | `runtime.NumCPU()` |
| `config.databaseType` | `internal` (dqlite) or `postgres` | `internal` |
| `config.databaseDSN` | PostgreSQL DSN when using `postgres` | `""` |
| `config.extraEnv` | Extra env vars injected into pod spec | `[]` |
| `peerDiscovery.probeSeconds` | How long ordinal 0 with an empty data directory probes the other ordinals before bootstrapping a new cluster | `20` |
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

## Delegating scheduling to Kueue

Caesium does not bin-pack, prioritize, or gang-schedule workloads — it delegates
admission control to [Kueue](https://kueue.sigs.k8s.io/), the Kubernetes-native
job-queueing controller, so the data DAG inherits Kueue's quota, fair-share, and
preemption without Caesium reimplementing any of it.

When a step sets `kueue.queueName`, Caesium stamps the
`kueue.x-k8s.io/queue-name` label on the pod it creates. Kueue's admission
webhook then gates the pod — it injects the `kueue.x-k8s.io/admission`
scheduling gate (the pod-level equivalent of a suspended Job), holding the pod
out of scheduling until the named LocalQueue's ClusterQueue has quota, and
removes the gate once the workload is admitted. Caesium never schedules the pod
itself.

### Cluster prerequisites

1. **Install Kueue** and enable its pod integration (`pod` in
   `integrations.frameworks`), so its webhook manages plain pods. See the
   [Kueue installation guide](https://kueue.sigs.k8s.io/docs/installation/) and
   [Run Plain Pods](https://kueue.sigs.k8s.io/docs/tasks/run/plain_pods/).
2. **Provision a ClusterQueue and a LocalQueue** in the namespace Caesium runs
   pods in (`CAESIUM_KUBERNETES_NAMESPACE`). The `queueName` in a job manifest
   must match a LocalQueue `metadata.name`.
3. Ensure Caesium's namespace is in scope for Kueue's
   `managedJobsNamespaceSelector` (it must not be excluded like `kube-system`).

### Job manifest

```yaml
steps:
  - name: train
    engine: kubernetes
    image: ghcr.io/acme/trainer:1.4
    kueue:
      queueName: data-eng     # an existing Kueue LocalQueue in the pod namespace
```

The queue is scheduling metadata, not an execution input, so it is excluded from
Caesium's cache identity hash — changing the queue never busts the task cache.
The full field shape is in
[`job-schema-reference.md`](job-schema-reference.md#kueue). A pod stuck in the
`SchedulingGated` state is waiting on Kueue quota; inspect its Workload with
`kubectl get workloads` and the [Kueue pod troubleshooting guide](https://kueue.sigs.k8s.io/docs/tasks/troubleshooting/troubleshooting_pods/).

## Persistence and Backup

- dqlite data is stored under `/var/lib/caesium/dqlite` in each pod.
- With `StatefulSet` + PVCs enabled, each ordinal gets a stable volume.
- For backup/restore, snapshot or back up the PVCs using your storage platform tooling.
- If `persistence.enabled=false`, all data is ephemeral and lost on pod restart/recreation.

### Pod replacement and the dqlite node address

A StatefulSet pod keeps its identity and its PVC when it is replaced — by a
`helm upgrade`, a node drain, an eviction or a reschedule — but Kubernetes never
promises it the same pod IP. The chart advertises each node to dqlite as
`$(POD_IP):9001`, and dqlite records that address in `info.yaml` inside the data
directory, so a replacement pod normally comes back with a data directory that
disagrees with its own address.

Caesium reconciles this on startup: it rewrites the persisted identity to the
current address, keeping the node ID, and then corrects a multi-member cluster's
raft configuration through the leader so the other members dial the address the
pod actually has. A sole member instead recovers its local raft configuration
after checking that no peer is reachable. If membership cannot be verified or
repaired, startup fails and the rollout waits rather than accepting reduced
quorum. The migration is logged as
`dqlite node address changed since this data directory was created; migrating`,
followed by
`dqlite cluster membership updated to this node's current address`. No manual
step is needed and the volume must not be discarded.

The address cannot simply be pinned to the pod's stable headless-service DNS
name: dqlite binds its raft listener to the advertised address, and
`dqlite_node_set_bind_address` accepts only a numeric IP.

Two limits are worth knowing:

- Replace pods **one at a time** and let the StatefulSet return to full
  readiness in between, which is what a normal rolling upgrade does. The
  repair runs through the cluster leader, so it needs a quorum of members
  still reachable at their recorded addresses.
- If every member's IP changes at once while the cluster is down, no member can
  reach another and the repair has nothing to talk to. Recover by restoring
  from a volume snapshot.

### Replacing a member whose volume was lost

Replacing a pod with an **empty** PVC (the disk was lost, or the PVC was
deleted) is a different case: the node has no identity to keep, so it joins the
cluster as a **new member** with a fresh dqlite node ID and receives the data
by snapshot from the leader. No manual step is needed.

For ordinals 1 and up this was always the case: the `peer-discovery` init
container seeds ordinal *N* with ordinals `0..N-1`. Ordinal 0 is special
because it is also the member that bootstraps a brand new cluster, so the init
container decides between the two (#582):

- If `/var/lib/caesium/dqlite/info.yaml` exists, ordinal 0 is an existing
  member restarting and nothing is probed.
- Otherwise it TCP-probes the dqlite port of every other ordinal through the
  headless Service (which publishes not-ready addresses) for up to
  `peerDiscovery.probeSeconds` (default 20 s). If any answers, a cluster
  already exists and ordinal 0 is seeded with the other ordinals, so it joins.
  On a new install the other ordinals do not exist yet, nothing answers, and
  ordinal 0 bootstraps after the window. A new install's first start of
  ordinal 0 is therefore up to that much slower.
- Caesium itself repeats the check (`CAESIUM_DATABASE_BOOTSTRAP_PEERS`, set by
  the chart to the other ordinals): a node with an empty data directory and no
  seeds probes those peers for up to 10 s more and joins if any answers. It
  logs either
  `this node has no dqlite identity but a bootstrap peer answered; joining the existing cluster as a new member instead of bootstrapping`
  or
  `no dqlite bootstrap peer answered within the probe window; bootstrapping a new cluster`.

### Removing the lost member's entry

The replacement does not take over the lost member's raft entry. That entry
stays in the configuration under its old node ID (for the original ordinal 0,
dqlite's bootstrap ID `3297041220608546238`) at the old pod address. go-dqlite's
role adjustment, which runs on the leader every 30 s, promotes the new member
to voter and then demotes the unreachable entry to a spare, so the cluster is
back to three live voters within about a minute. Nothing removes the spare. It
has no vote, but every member's `/health` keeps reporting the cluster's nodes
as degraded because a member is unreachable. It also stays in
`GET /v1/system/nodes` and keeps its old address. Once the replacement is Ready,
remove it:

```sh
# The stale entry is a spare (or standby) whose reachability is "unreachable".
kubectl exec caesium-1 -c caesium -- caesium system nodes list
# ID                    ADDRESS           ROLE   REACHABILITY  LEADER
# 3297041220608546238   10.244.2.5:9001   spare  unreachable   false
# ...

kubectl exec caesium-1 -c caesium -- caesium system nodes remove 3297041220608546238
# Removed dqlite member 3297041220608546238 (10.244.2.5:9001, was spare); the configuration now has 3 members.
```

The command calls `DELETE /v1/system/nodes/<id>`, where `<id>` is the decimal
`id` that `GET /v1/system/nodes` and `/health` (`checks.cluster.members`)
report. When authentication is enabled the route needs an unscoped **admin**
key, and a job-scoped key is refused. From outside the pod, pass `--server` and
set `CAESIUM_API_KEY`. The removal is written to the audit log as
`cluster.member_remove`. When authentication is disabled the route is as open as
the rest of the API, but the checks below still apply.

The server checks the leader's current configuration, probes the addresses,
checks the configuration again, and removes the entry through the leader. It
removes the entry only if every check passes. Otherwise it changes nothing and
answers with a machine-readable reason. `caesium system nodes remove` then
exits non-zero with the reason on stderr. With `--json`, stdout carries the
server's answer either way: `{"status":"removed",...}` with the removed member
and the remaining configuration, or `{"status":"refused","reason":...,"reasons":[...],"retryable":...}`.
`reasons` lists every configuration reason that applies, not only the first.

| Reason | HTTP | Meaning | What to do |
| --- | --- | --- | --- |
| `not_a_member` | 404 | No member has this ID. It was never a member or is already removed. | Nothing, if you just removed it. Otherwise check the ID. |
| `local_node` | 409 | The ID is the node serving the request. | That member is alive. Check the ID. |
| `leader` | 409 | The ID is the current leader. | That member is alive. Check the ID. |
| `voter` | 409 | The member still has a vote. | If `retryable` is true, the member is unreachable and not yet demoted. Wait up to a minute for role adjustment and run the command again. Otherwise it is a live voter. |
| `insufficient_voters` | 409 | The configuration would be left with fewer than three voters. | Restore the cluster to three voters first. |
| `reachable` | 409 | A dqlite node answers at the entry's address. | The member is not gone. If a new pod was given the lost member's address, see below. |
| `voters_unreachable` | 409 | At least one voter does not answer. | Bring every voter back before changing membership. |
| `configuration_change_in_progress` | 409 | The leader is applying another membership or role change. | Run it again. A promotion stalled on an unreachable member holds changes off for up to about a minute. |
| `no_leader` | 503 | No leader could be reached, or leadership moved during the request. | Run it again once the cluster has a leader. |
| `invalid_id` | 400 | The ID is not a decimal dqlite node ID. | Use an ID from `caesium system nodes list`. |
| `not_clustered` | 409 | This deployment does not run dqlite. | Nothing to remove. |

`retryable` is true only for `configuration_change_in_progress`, `no_leader`,
and an unreachable `voter` that role adjustment has yet to demote. Every other
refusal needs an operator action or a different ID.

The F2 lifecycle qualification exercises this command after an ordinal-1 and
an ordinal-0 disk-loss replacement. It first checks that removing the leader or
a live voter is refused. It then removes the stale entries and checks that
they are gone from every member's own raft configuration and `/health` view.

Residual risks and limits:

- Removal is manual. Nothing removes a stale entry on its own. A stale spare is
  harmless to quorum, so skipping the removal only leaves health degraded.
- If ordinal 0 loses its disk while **every** other member is unreachable at
  once (for example, all pods are being rescheduled), both probes find nothing
  and ordinal 0 bootstraps an empty cluster of its own. Replace one member at a
  time and do not delete ordinal 0's PVC while the other members are down.
- If the replacement pod is given exactly the old pod's IP, raft refuses the
  join because the stale entry holds that address, and the pod stays not Ready
  instead of serving a divergent database. The removal refuses too
  (`reachable`), because the protocol cannot tell which node answers at an
  address. Delete the pod so it gets a new IP, then remove the entry. This
  path is not covered by the qualification.
- Removal does not fence the old identity. If the lost member's volume turns
  out not to be lost and a pod starts on it again, Caesium's membership repair
  treats the missing entry like an interrupted repair and adds that node back
  under its old ID. Delete such a PVC rather than reattach it.

## Air-Gapped Deployment Notes

Caesium itself has **no external runtime dependencies** — the binary embeds dqlite and requires no outbound network access to operate. The considerations for an air-gapped Kubernetes deployment are specific to the cluster runtime and image availability, not to Caesium itself.

### Pre-loading images

In an air-gapped cluster, images must be available to the cluster runtime before jobs run. The two common approaches:

**Option 1: Internal container registry.** Mirror the images your jobs need into a registry reachable from within the cluster (e.g. Harbor, Artifactory, a private ECR endpoint). Update job definitions to reference the internal registry:

```yaml
steps:
  - name: extract
    image: registry.internal/my-team/etl-extract:1.4.2
```

**Option 2: Pre-load directly into the node runtime.** For small clusters or edge nodes, load images directly into containerd or CRI-O using `ctr images import` or `crictl`:

```bash
# Save image on a connected host
docker save my-etl-extract:1.4.2 | gzip > etl-extract.tar.gz

# Transfer to each node (e.g. via scp or USB)
scp etl-extract.tar.gz user@k8s-node:/tmp/

# Import into containerd on the node
ssh user@k8s-node "ctr -n=k8s.io images import /tmp/etl-extract.tar.gz"
```

The Caesium Kubernetes engine sets `imagePullPolicy: IfNotPresent` on every pod it creates (`internal/atom/kubernetes/engine.go`). This means if the image is already present on the node (pre-loaded via one of the options above), the kubelet will not attempt a registry pull. No job-definition field is needed — pre-loading the image onto the node is sufficient.

### No Caesium control plane egress

Once installed, the Caesium server pod does not make outbound network calls. It does not phone home, emit telemetry, or contact a license server. The Helm chart installs cleanly in a cluster with egress blocked at the network policy level.

### Image digest pinning for regulated environments

In air-gapped or regulated deployments where reproducibility must be auditable, enable digest pinning so cache keys are computed from content-addressed `sha256:` digests rather than mutable tags. This is tracked in the data-plane memory substrate — see [`cache.pinDigests` in `design-data-plane-memory.md`](design-data-plane-memory.md) for details and current build status. Do not duplicate that configuration here; cross-link instead.

### Further reading

For the full zero-dependency story and a non-Kubernetes quickstart, see [`sovereignty.md`](sovereignty.md).

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
