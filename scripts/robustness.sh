#!/usr/bin/env bash
# B1 owner-crash robustness host controller.
# Unique kind cluster + namespace from CAESIUM_ROBUSTNESS_ID. Every kubectl/helm
# call uses the explicit kubeconfig. The in-cluster test never docker-execs nodes.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
die() { log "ERROR: $*"; exit 1; }

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

require_env() {
  local name="$1"
  if [[ -z "${!name:-}" ]]; then
    die "$name is required"
  fi
}

require_env CAESIUM_ROBUSTNESS_ID
require_env CAESIUM_ROBUSTNESS_ARTIFACTS
require_env CAESIUM_ROBUSTNESS_IMAGE
require_env CAESIUM_ROBUSTNESS_SERVER_IMAGE
require_env CAESIUM_ROBUSTNESS_KIND_IMAGE
require_env CAESIUM_ROBUSTNESS_TASK_IMAGE

# A1's documented invocation exports only CAESIUM_*; parent-shell assignments
# of CANDIDATE_SHA/ROBUSTNESS_ID/ARTIFACTS are not in the child environment.
ROBUSTNESS_ID="${ROBUSTNESS_ID:-$CAESIUM_ROBUSTNESS_ID}"
ARTIFACTS="${ARTIFACTS:-$CAESIUM_ROBUSTNESS_ARTIFACTS}"
if [[ -z "${CANDIDATE_SHA:-}" ]]; then
  CANDIDATE_SHA="${CAESIUM_ROBUSTNESS_SERVER_IMAGE##*:}"
fi
[[ -n "$CANDIDATE_SHA" && "$CANDIDATE_SHA" != "$CAESIUM_ROBUSTNESS_SERVER_IMAGE" ]] \
  || die "CANDIDATE_SHA is required (export it, or use caesiumcloud/caesium:<sha> as CAESIUM_ROBUSTNESS_SERVER_IMAGE)"

[[ "$ROBUSTNESS_ID" == "$CAESIUM_ROBUSTNESS_ID" ]] || die "ROBUSTNESS_ID ($ROBUSTNESS_ID) != CAESIUM_ROBUSTNESS_ID ($CAESIUM_ROBUSTNESS_ID)"
[[ "$ARTIFACTS" == "$CAESIUM_ROBUSTNESS_ARTIFACTS" ]] || die "ARTIFACTS ($ARTIFACTS) != CAESIUM_ROBUSTNESS_ARTIFACTS ($CAESIUM_ROBUSTNESS_ARTIFACTS)"

if ! python3 - "$ROBUSTNESS_ID" <<'PY'
import re, sys
name = sys.argv[1]
if not re.fullmatch(r"[a-z0-9]([-a-z0-9]*[a-z0-9])?", name) or len(name) > 47:
    raise SystemExit(f"ROBUSTNESS_ID must be a lowercase DNS-1123 name <= 47 chars, got {name!r}")
PY
then
  die "ROBUSTNESS_ID must be a lowercase DNS-1123 name <= 47 chars, got $ROBUSTNESS_ID"
fi

require_cmd kind
require_cmd kubectl
require_cmd helm
require_cmd docker
require_cmd python3

mkdir -p "$ARTIFACTS"
ARTIFACTS="$(cd "$ARTIFACTS" && pwd)"
CAESIUM_ROBUSTNESS_ARTIFACTS="$ARTIFACTS"

KIND_IMAGE="$CAESIUM_ROBUSTNESS_KIND_IMAGE"
SERVER_IMAGE="$CAESIUM_ROBUSTNESS_SERVER_IMAGE"
RUNNER_IMAGE="$CAESIUM_ROBUSTNESS_IMAGE"
TASK_IMAGE="$CAESIUM_ROBUSTNESS_TASK_IMAGE"
KUBECONFIG_PATH="$ARTIFACTS/kubeconfig"
ISO_KUBECONFIG="$ARTIFACTS/kubeconfig-iso"
OWNED_CLUSTERS="$ARTIFACTS/owned-clusters.txt"
FAULTED_NODE_FILE="$ARTIFACTS/faulted-node"
CORDONED_NODE_FILE="$ARTIFACTS/cordoned-node"
CANONICAL_SERVER_IMAGE="caesiumcloud/caesium:${CANDIDATE_SHA}"
NAMESPACE="$ROBUSTNESS_ID"
VALUES="$ROOT/helm/caesium/ci/test-values-robustness.yaml"
LAST_REQUEST_ID=""
: >"$OWNED_CLUSTERS"

# Never inherit the caller's kube context.
unset KUBECONFIG || true
export KUBECONFIG="$KUBECONFIG_PATH"

kc() {
  kubectl --kubeconfig "$KUBECONFIG_PATH" "$@"
}

kc_ns() {
  kubectl --kubeconfig "$KUBECONFIG_PATH" --namespace "$NAMESPACE" "$@"
}

record_cluster() {
  local name="$1"
  if ! grep -qxF "$name" "$OWNED_CLUSTERS" 2>/dev/null; then
    printf '%s\n' "$name" >>"$OWNED_CLUSTERS"
  fi
}

restart_faulted_node() {
  local node=""
  if [[ -f "$FAULTED_NODE_FILE" ]]; then
    node="$(cat "$FAULTED_NODE_FILE")"
  elif [[ -f "$CORDONED_NODE_FILE" ]]; then
    node="$(cat "$CORDONED_NODE_FILE")"
  fi
  [[ -n "$node" ]] || return 0
  log "cleanup: restoring kubelet/uncordon on $node"
  docker exec "$node" systemctl start kubelet >/dev/null 2>&1 || true
  if [[ -f "$KUBECONFIG_PATH" ]]; then
    kc uncordon "$node" >/dev/null 2>&1 || true
  fi
}

delete_owned_clusters() {
  local name
  if [[ ! -f "$OWNED_CLUSTERS" ]]; then
    return 0
  fi
  while IFS= read -r name; do
    [[ -n "$name" ]] || continue
    log "cleanup: kind delete cluster --name $name"
    kind delete cluster --name "$name" >/dev/null 2>&1 || true
  done <"$OWNED_CLUSTERS"
}

cleanup() {
  local status=$?
  set +e
  restart_faulted_node
  if [[ -f "$KUBECONFIG_PATH" ]]; then
    kc_ns logs pod/robustness-runner >"$ARTIFACTS/runner.log" 2>/dev/null || true
    kc_ns get cm robustness-records -o yaml >"$ARTIFACTS/robustness-records.yaml" 2>/dev/null || true
    kc get pods -A >"$ARTIFACTS/pods-all.txt" 2>/dev/null || true
    kc_ns describe pods >"$ARTIFACTS/describe-ns-pods.txt" 2>/dev/null || true
    kc_ns logs statefulset/caesium -c caesium --tail=200 >"$ARTIFACTS/caesium.log" 2>/dev/null || true
  fi
  if [[ "${CAESIUM_ROBUSTNESS_KEEP_CLUSTER:-}" != "1" ]]; then
    delete_owned_clusters
  else
    log "CAESIUM_ROBUSTNESS_KEEP_CLUSTER=1; leaving owned clusters"
  fi
  exit "$status"
}
trap cleanup EXIT INT TERM

log "candidate=$CANDIDATE_SHA robustness_id=$ROBUSTNESS_ID artifacts=$ARTIFACTS"

python3 - "$ARTIFACTS" "$KIND_IMAGE" "$ROBUSTNESS_ID" "$SERVER_IMAGE" "$RUNNER_IMAGE" "$TASK_IMAGE" "$CANDIDATE_SHA" <<'PY'
import json, pathlib, sys, datetime
art, kind_image, rid, server, runner, task, sha = sys.argv[1:]
pathlib.Path(art, "versions.json").write_text(json.dumps({
    "candidate_sha": sha,
    "robustness_id": rid,
    "kind_image": kind_image,
    "server_image": server,
    "runner_image": runner,
    "task_image": task,
    "started_at": datetime.datetime.utcnow().strftime("%Y-%m-%dT%H:%M:%SZ"),
}, indent=2) + "\n")
PY

docker image inspect "$KIND_IMAGE" >/dev/null || die "KIND_IMAGE not present locally: $KIND_IMAGE"
docker image inspect "$SERVER_IMAGE" >/dev/null || die "SERVER_IMAGE not present locally: $SERVER_IMAGE"
docker image inspect "$RUNNER_IMAGE" >/dev/null || die "RUNNER_IMAGE not present locally: $RUNNER_IMAGE"
docker image inspect "$TASK_IMAGE" >/dev/null || die "TASK_IMAGE not present locally: $TASK_IMAGE"

HOST_ARCH="$(uname -m)"
KIND_ARCH="$(docker image inspect --format '{{.Architecture}}' "$KIND_IMAGE")"
TASK_ARCH="$(docker image inspect --format '{{.Architecture}}' "$TASK_IMAGE")"
SERVER_ARCH="$(docker image inspect --format '{{.Architecture}}' "$SERVER_IMAGE")"
log "host_arch=$HOST_ARCH kind_arch=$KIND_ARCH task_arch=$TASK_ARCH server_arch=$SERVER_ARCH"
case "$HOST_ARCH" in
  x86_64|amd64) HOST_NORM=amd64 ;;
  aarch64|arm64) HOST_NORM=arm64 ;;
  *) HOST_NORM="$HOST_ARCH" ;;
esac
[[ "$KIND_ARCH" == "$HOST_NORM" || "$KIND_ARCH" == "$HOST_ARCH" ]] || die "KIND_IMAGE architecture $KIND_ARCH does not match host $HOST_ARCH"
[[ "$TASK_ARCH" == "$HOST_NORM" || "$TASK_ARCH" == "$HOST_ARCH" ]] || die "TASK_IMAGE architecture $TASK_ARCH does not match host $HOST_ARCH"

CANDIDATE_DIGEST="$(docker image inspect --format '{{.Id}}' "$SERVER_IMAGE")"
log "candidate_digest=$CANDIDATE_DIGEST"
printf '%s\n' "$CANDIDATE_DIGEST" >"$ARTIFACTS/candidate-digest.txt"
docker image inspect "$KIND_IMAGE" >"$ARTIFACTS/kind-image.json"
docker image inspect "$SERVER_IMAGE" >"$ARTIFACTS/server-image.json"
docker image inspect "$RUNNER_IMAGE" >"$ARTIFACTS/runner-image.json"
docker image inspect "$TASK_IMAGE" >"$ARTIFACTS/task-image.json"

if [[ "$SERVER_IMAGE" != "$CANONICAL_SERVER_IMAGE" ]]; then
  log "tagging $SERVER_IMAGE as $CANONICAL_SERVER_IMAGE for helm image.tag=$CANDIDATE_SHA"
  docker tag "$SERVER_IMAGE" "$CANONICAL_SERVER_IMAGE"
fi

cat >"$ARTIFACTS/kind.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: ${ROBUSTNESS_ID}
nodes:
  - role: control-plane
  - role: worker
  - role: worker
  - role: worker
EOF

cat >"$ARTIFACTS/kind-iso.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
EOF

SUFFIX="$(printf '%s' "$ROBUSTNESS_ID" | tr -cd 'a-z0-9' | tail -c 8)"
ISO_NAME="iso${SUFFIX}"
record_cluster "$ISO_NAME"
record_cluster "$ROBUSTNESS_ID"

ISO_NS_A="iso-a-${SUFFIX}"
ISO_NS_B="iso-b-${SUFFIX}"
log "isolation: creating $ISO_NAME and $ROBUSTNESS_ID concurrently"
kind create cluster \
  --name "$ISO_NAME" \
  --image "$KIND_IMAGE" \
  --config "$ARTIFACTS/kind-iso.yaml" \
  --kubeconfig "$ISO_KUBECONFIG" \
  --wait 120s &
iso_pid=$!
kind create cluster \
  --name "$ROBUSTNESS_ID" \
  --image "$KIND_IMAGE" \
  --config "$ARTIFACTS/kind.yaml" \
  --kubeconfig "$KUBECONFIG_PATH" \
  --wait 120s &
pri_pid=$!
wait "$iso_pid" || die "kind create $ISO_NAME failed"
wait "$pri_pid" || die "kind create $ROBUSTNESS_ID failed"
kubectl --kubeconfig "$ISO_KUBECONFIG" create namespace "$ISO_NS_A"
kc create namespace "$ISO_NS_B" >/dev/null

ISO_SERVER="$(kubectl --kubeconfig "$ISO_KUBECONFIG" config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
PRI_SERVER="$(kc config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
log "isolation kubeconfig servers iso=$ISO_SERVER primary=$PRI_SERVER"
[[ "$ISO_SERVER" != "$PRI_SERVER" ]] || die "isolation failed: kubeconfig servers collide"
[[ "$ISO_SERVER" != *":8080" && "$PRI_SERVER" != *":8080" ]] || die "refusing fixed host API port 8080"
if kubectl --kubeconfig "$ISO_KUBECONFIG" get namespace "$ISO_NS_B" >/dev/null 2>&1; then
  die "isolation failed: iso kubeconfig can see primary namespace $ISO_NS_B"
fi
if kc get namespace "$ISO_NS_A" >/dev/null 2>&1; then
  die "isolation failed: primary kubeconfig can see iso namespace $ISO_NS_A"
fi
ISO_CTX="$(kubectl --kubeconfig "$ISO_KUBECONFIG" config current-context)"
PRI_CTX="$(kc config current-context)"
[[ "$ISO_CTX" != "$PRI_CTX" ]] || die "isolation failed: contexts collide ($ISO_CTX)"
log "isolation proof passed for $ISO_NAME and $ROBUSTNESS_ID"
if kind delete cluster --name "$ISO_NAME"; then
  grep -vxF "$ISO_NAME" "$OWNED_CLUSTERS" >"$OWNED_CLUSTERS.tmp" || true
  mv "$OWNED_CLUSTERS.tmp" "$OWNED_CLUSTERS"
  rm -f "$ISO_KUBECONFIG"
else
  die "failed to delete isolation cluster $ISO_NAME"
fi
record_cluster "$ROBUSTNESS_ID"

log "diagnosing systemctl/ctr on kind workers before any fault"
KIND_NODES=()
while IFS= read -r node; do
  [[ -n "$node" ]] || continue
  KIND_NODES+=("$node")
done < <(kind get nodes --name "$ROBUSTNESS_ID")
[[ ${#KIND_NODES[@]} -eq 4 ]] || die "expected 4 kind nodes, found ${#KIND_NODES[@]}: ${KIND_NODES[*]}"
WORKER_COUNT=0
for node in "${KIND_NODES[@]}"; do
  log "node $node"
  docker exec "$node" sh -c 'command -v systemctl && command -v ctr && systemctl is-system-running || true' \
    | tee -a "$ARTIFACTS/node-tools-$node.txt"
  docker exec "$node" sh -c 'command -v systemctl >/dev/null && command -v ctr >/dev/null' \
    || die "systemctl/ctr unavailable on $node before faulting (selected KIND_IMAGE=$KIND_IMAGE)"
  docker exec "$node" ctr -n k8s.io tasks list >>"$ARTIFACTS/node-tools-$node.txt"
  if [[ "$node" != *control-plane* ]]; then
    WORKER_COUNT=$((WORKER_COUNT + 1))
  fi
done
[[ "$WORKER_COUNT" -eq 3 ]] || die "expected 3 kind workers, found $WORKER_COUNT"

load_kind_image() {
  local img="$1"
  log "kind load docker-image $img"
  kind load docker-image --name "$ROBUSTNESS_ID" "$img"
}

# Multi-arch tags and buildx attestation lists fail kind import
# ("ctr: content digest ... not found"). Flatten the task image to one
# platform without provenance, then load by name so pod ImageIDs match.
TASK_ARCH="$(docker image inspect --format '{{.Architecture}}' "$TASK_IMAGE")"
FLAT_TASK="caesium-robustness-task:${ROBUSTNESS_ID}"
log "flattening TASK_IMAGE $TASK_IMAGE ($TASK_ARCH) -> $FLAT_TASK"
BUILDX_NO_DEFAULT_ATTESTATIONS=1 docker build \
  --provenance=false --sbom=false \
  --platform "linux/${TASK_ARCH}" \
  -t "$FLAT_TASK" - <<EOF
FROM ${TASK_IMAGE}
EOF
TASK_IMAGE="$FLAT_TASK"
CAESIUM_ROBUSTNESS_TASK_IMAGE="$TASK_IMAGE"

log "loading images into $ROBUSTNESS_ID"
load_kind_image "$CANONICAL_SERVER_IMAGE"
if [[ "$SERVER_IMAGE" != "$CANONICAL_SERVER_IMAGE" ]]; then
  load_kind_image "$SERVER_IMAGE"
fi
load_kind_image "$RUNNER_IMAGE"
load_kind_image "$TASK_IMAGE"

imported_digest() {
  local node="$1" needle="$2"
  docker exec "$node" ctr -n k8s.io images ls | python3 -c '
import sys
needle = sys.argv[1]
for line in sys.stdin:
    if needle not in line:
        continue
    for tok in line.split():
        if tok.startswith("sha256:") and len(tok) > 20:
            print(tok)
            raise SystemExit
raise SystemExit("no sha256 digest for " + needle)
' "$needle"
}

WORKER_NODE=""
for n in "${KIND_NODES[@]}"; do
  if [[ "$n" != *control-plane* ]]; then
    WORKER_NODE="$n"
    break
  fi
done
[[ -n "$WORKER_NODE" ]] || die "no kind worker for imported digest"
IMPORTED_DIGEST="$(imported_digest "$WORKER_NODE" "caesiumcloud/caesium:${CANDIDATE_SHA}")"
log "host_image_id=$CANDIDATE_DIGEST imported_digest=$IMPORTED_DIGEST"
CANDIDATE_DIGEST="$IMPORTED_DIGEST"
printf '%s\n' "$CANDIDATE_DIGEST" >"$ARTIFACTS/candidate-digest.txt"
printf '%s\n' "$IMPORTED_DIGEST" >"$ARTIFACTS/imported-digest.txt"

TOKEN="$(python3 -c 'import secrets; print(secrets.token_urlsafe(48))')"
(( ${#TOKEN} >= 32 )) || die "generated internal token is shorter than 32 bytes"
printf '%s\n' "$TOKEN" >"$ARTIFACTS/internal-token.txt"

log "helm install caesium into namespace $NAMESPACE"
helm install caesium "$ROOT/helm/caesium" \
  --kubeconfig "$KUBECONFIG_PATH" \
  --namespace "$NAMESPACE" \
  --create-namespace \
  --values "$VALUES" \
  --set image.tag="$CANDIDATE_SHA" \
  --set config.extraEnv[0].name=CAESIUM_INTERNAL_WAKEUP_TOKEN \
  --set-string config.extraEnv[0].value="$TOKEN" \
  --wait --timeout 240s

log "verifying three bound PVCs, distinct members, and candidate image IDs"
python3 - "$KUBECONFIG_PATH" "$NAMESPACE" "$CANDIDATE_DIGEST" "$CANDIDATE_SHA" <<'PY'
import json, subprocess, sys

kube, ns, digest, tag = sys.argv[1:]
digest = digest.replace("sha256:", "")

def kc(*args):
    out = subprocess.check_output(["kubectl", "--kubeconfig", kube, "-n", ns, *args], text=True)
    return json.loads(out)

pvcs = kc("get", "pvc", "-o", "json")["items"]
bound = [p for p in pvcs if p.get("status", {}).get("phase") == "Bound" and p.get("spec", {}).get("volumeName")
         and p["metadata"]["name"].startswith("data-caesium-")]
if len(bound) < 3:
    raise SystemExit(f"expected 3 bound data PVCs, found {len(bound)}")

pods = kc("get", "pods", "-l", "app.kubernetes.io/instance=caesium", "-o", "json")["items"]
caesium = [p for p in pods if not p["metadata"].get("deletionTimestamp")
           and any(c["name"] == "caesium" for c in p.get("status", {}).get("containerStatuses") or [])]
if len(caesium) != 3:
    raise SystemExit(f"expected 3 caesium pods, found {len(caesium)}")

uids, ips, nodes, digests = set(), set(), set(), set()
for p in caesium:
    uid = p["metadata"]["uid"]
    ip = p.get("status", {}).get("podIP") or ""
    node = p.get("spec", {}).get("nodeName") or ""
    if not uid or not ip or not node:
        raise SystemExit(f"pod {p['metadata']['name']} missing uid/ip/node")
    if "control-plane" in node or "controlplane" in node:
        raise SystemExit(f"pod {p['metadata']['name']} on control-plane node {node}")
    cs = next(c for c in p["status"]["containerStatuses"] if c["name"] == "caesium")
    image = cs.get("image") or ""
    if tag not in image:
        raise SystemExit(f"pod {p['metadata']['name']} image {image} does not use candidate tag {tag}")
    image_id = cs.get("imageID") or ""
    if digest not in image_id.replace("sha256:", ""):
        raise SystemExit(f"pod {p['metadata']['name']} imageID {image_id} does not match imported candidate {digest}")
    uids.add(uid); ips.add(ip); nodes.add(node); digests.add(image_id)

if len(uids) != 3 or len(ips) != 3 or len(nodes) != 3:
    raise SystemExit(f"members not distinct uids={len(uids)} ips={len(ips)} nodes={len(nodes)}")
print(f"topology ok uids={len(uids)} ips={len(ips)} nodes={len(nodes)}")
PY

for i in 0 1 2; do
  kc_ns exec "caesium-$i" -c caesium -- sh -c 'printenv | grep ^CAESIUM_ | sort' \
    >"$ARTIFACTS/caesium-$i.env" || true
done

log "deploying in-cluster runner on the control-plane node"
cat >"$ARTIFACTS/runner.yaml" <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: robustness-runner
  namespace: ${NAMESPACE}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: robustness-runner
  namespace: ${NAMESPACE}
rules:
  - apiGroups: [""]
    resources: ["pods", "pods/log", "pods/status", "services", "endpoints", "configmaps", "persistentvolumeclaims", "events"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: robustness-runner
  namespace: ${NAMESPACE}
subjects:
  - kind: ServiceAccount
    name: robustness-runner
    namespace: ${NAMESPACE}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: robustness-runner
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: robustness-host-request
  namespace: ${NAMESPACE}
data:
  request_id: ""
  action: ""
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: robustness-host-ack
  namespace: ${NAMESPACE}
data:
  request_id: ""
  action: ""
  status: ""
---
apiVersion: v1
kind: Service
metadata:
  name: robustness-recorder
  namespace: ${NAMESPACE}
spec:
  selector:
    app.kubernetes.io/name: robustness-runner
  ports:
    - name: http
      port: 8090
      targetPort: 8090
---
apiVersion: v1
kind: Pod
metadata:
  name: robustness-runner
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: robustness-runner
spec:
  serviceAccountName: robustness-runner
  restartPolicy: Never
  automountServiceAccountToken: true
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchExpressions:
              - key: node-role.kubernetes.io/control-plane
                operator: Exists
  tolerations:
    - key: node-role.kubernetes.io/control-plane
      operator: Exists
      effect: NoSchedule
    - key: node-role.kubernetes.io/master
      operator: Exists
      effect: NoSchedule
  containers:
    - name: runner
      image: ${RUNNER_IMAGE}
      imagePullPolicy: IfNotPresent
      command: ["/bin/robustness.test"]
      args: ["-test.v", "-test.count=1", "-test.run", "^TestOwnerCrash$", "-test.timeout", "15m"]
      ports:
        - name: recorder
          containerPort: 8090
      env:
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
        - name: POD_IP
          valueFrom:
            fieldRef:
              fieldPath: status.podIP
        - name: CANDIDATE_SHA
          value: ${CANDIDATE_SHA}
        - name: ROBUSTNESS_ID
          value: ${ROBUSTNESS_ID}
        - name: CAESIUM_ROBUSTNESS_ID
          value: ${ROBUSTNESS_ID}
        - name: CAESIUM_ROBUSTNESS_TASK_IMAGE
          value: ${TASK_IMAGE}
        - name: CAESIUM_ROBUSTNESS_SERVER_IMAGE
          value: ${CANONICAL_SERVER_IMAGE}
        - name: CAESIUM_ROBUSTNESS_CANDIDATE_DIGEST
          value: ${CANDIDATE_DIGEST}
        - name: CAESIUM_MANUAL_TRIGGER_API_KEY
          value: caesium-robustness-manual-key
      resources:
        requests:
          cpu: 50m
          memory: 128Mi
        limits:
          cpu: "1"
          memory: 512Mi
EOF

kc apply -f "$ARTIFACTS/runner.yaml"
kc_ns wait --for=condition=Ready pod/robustness-runner --timeout=120s

write_ack() {
  local request_id="$1" action="$2" status="$3" evidence="${4:-}" error="${5:-}"
  REQUEST_ID="$request_id" ACTION="$action" STATUS="$status" EVIDENCE="$evidence" ERROR="$error" ARTIFACTS="$ARTIFACTS" python3 - <<'PY'
import json, os, pathlib
ack = {
    "request_id": os.environ["REQUEST_ID"],
    "action": os.environ["ACTION"],
    "status": os.environ["STATUS"],
    "evidence": os.environ.get("EVIDENCE", ""),
    "error": os.environ.get("ERROR", ""),
}
path = pathlib.Path(os.environ["ARTIFACTS"]) / "host-ack.json"
path.write_text(json.dumps(ack))
PY
  kc_ns create configmap robustness-host-ack \
    --from-file=payload="$ARTIFACTS/host-ack.json" \
    --from-literal=request_id="$request_id" \
    --from-literal=action="$action" \
    --from-literal=status="$status" \
    --dry-run=client -o yaml | kc apply -f -
}

task_dead() {
  local cid="$1" listing="$2" seen="$3"
  python3 - "$cid" "$listing" "$seen" <<'PY'
import sys
cid = sys.argv[1].strip()
listing = sys.argv[2]
seen = sys.argv[3] == "1"
short = cid[:12] if len(cid) >= 12 else cid
if not cid or len(short) < 8:
    raise SystemExit("empty container id")
matched = False
for line in listing.splitlines():
    if cid not in line and short not in line:
        continue
    matched = True
    l = line.lower()
    if "running" in l:
        raise SystemExit("container still running")
    if any(s in l for s in ("stopped", "exited", "killed")):
        raise SystemExit(0)
if matched:
    raise SystemExit("container still present without stopped evidence")
if seen:
    raise SystemExit(0)
raise SystemExit("container id never appeared in ctr tasks list")
PY
}

handle_host_request() {
  local action request_id node cid pod evidence listing
  action="$(kc_ns get configmap robustness-host-request -o jsonpath='{.data.action}' 2>/dev/null || true)"
  request_id="$(kc_ns get configmap robustness-host-request -o jsonpath='{.data.request_id}' 2>/dev/null || true)"
  [[ -n "$action" && -n "$request_id" ]] || return 0
  [[ "$request_id" != "$LAST_REQUEST_ID" ]] || return 0
  node="$(kc_ns get configmap robustness-host-request -o jsonpath='{.data.owner_kind_node}' 2>/dev/null || true)"
  cid="$(kc_ns get configmap robustness-host-request -o jsonpath='{.data.owner_container_id}' 2>/dev/null || true)"
  pod="$(kc_ns get configmap robustness-host-request -o jsonpath='{.data.owner_pod}' 2>/dev/null || true)"
  log "host request action=$action id=$request_id pod=$pod node=$node cid=$cid"

  case "$action" in
    cordon)
      [[ -n "$node" ]] || { write_ack "$request_id" "$action" "failed" "" "missing owner_kind_node"; LAST_REQUEST_ID="$request_id"; return 0; }
      if ! printf '%s\n' "${KIND_NODES[@]}" | grep -qxF "$node"; then
        write_ack "$request_id" "$action" "failed" "" "node $node is not in cluster $ROBUSTNESS_ID"
        LAST_REQUEST_ID="$request_id"
        return 0
      fi
      printf '%s\n' "$node" >"$CORDONED_NODE_FILE"
      if ! kc cordon "$node"; then
        write_ack "$request_id" "$action" "failed" "" "kubectl cordon $node failed"
        LAST_REQUEST_ID="$request_id"
        return 0
      fi
      write_ack "$request_id" "$action" "ok" "cordoned $node"
      ;;
    kill)
      [[ -n "$node" && -n "$cid" ]] || { write_ack "$request_id" "$action" "failed" "" "missing node/container id"; LAST_REQUEST_ID="$request_id"; return 0; }
      if ! printf '%s\n' "${KIND_NODES[@]}" | grep -qxF "$node"; then
        write_ack "$request_id" "$action" "failed" "" "node $node is not in cluster $ROBUSTNESS_ID"
        LAST_REQUEST_ID="$request_id"
        return 0
      fi
      # Register restart cleanup BEFORE stopping kubelet.
      printf '%s\n' "$node" >"$FAULTED_NODE_FILE"
      sync
      if ! docker exec "$node" systemctl stop kubelet; then
        write_ack "$request_id" "$action" "failed" "" "systemctl stop kubelet failed on $node"
        LAST_REQUEST_ID="$request_id"
        return 0
      fi
      listing=""
      killed=0
      seen_cid=0
      short_cid="${cid:0:12}"
      before="$(docker exec "$node" ctr -n k8s.io tasks list 2>&1 || true)"
      printf '%s\n' "$before" >"$ARTIFACTS/ctr-tasks-before-kill.txt"
      if [[ "$before" == *"$cid"* || "$before" == *"$short_cid"* ]]; then
        seen_cid=1
      else
        write_ack "$request_id" "$action" "failed" "$before" "owner container $cid not in ctr tasks list before kill"
        LAST_REQUEST_ID="$request_id"
        return 0
      fi
      for _try in $(seq 1 20); do
        docker exec "$node" ctr -n k8s.io tasks kill --signal SIGKILL "$cid" >"$ARTIFACTS/ctr-kill-$cid.txt" 2>&1 || true
        listing="$(docker exec "$node" ctr -n k8s.io tasks list 2>&1 || true)"
        printf '%s\n' "$listing" >"$ARTIFACTS/ctr-tasks-after-kill.txt"
        if task_dead "$cid" "$listing" "$seen_cid"; then
          killed=1
          break
        fi
        sleep 1
      done
      evidence="$(printf 'kubelet stopped on %s\nctr kill %s\n%s\n' "$node" "$cid" "$listing")"
      if [[ "$killed" -ne 1 ]]; then
        write_ack "$request_id" "$action" "failed" "$evidence" "process still running"
        LAST_REQUEST_ID="$request_id"
        return 0
      fi
      write_ack "$request_id" "$action" "ok" "$evidence"
      ;;
    restart)
      [[ -n "$node" ]] || { write_ack "$request_id" "$action" "failed" "" "missing owner_kind_node"; LAST_REQUEST_ID="$request_id"; return 0; }
      docker exec "$node" systemctl start kubelet
      kc uncordon "$node" || true
      rm -f "$FAULTED_NODE_FILE" "$CORDONED_NODE_FILE"
      write_ack "$request_id" "$action" "ok" "started kubelet and uncordoned $node"
      ;;
    done)
      write_ack "$request_id" "$action" "ok" "done"
      ;;
    *)
      write_ack "$request_id" "$action" "failed" "" "unknown action"
      ;;
  esac
  LAST_REQUEST_ID="$request_id"
}

log "host controller waiting for runner (15m test + margin)"
DEADLINE=$((SECONDS + 1080))
RUNNER_PHASE=""
while (( SECONDS < DEADLINE )); do
  RUNNER_PHASE="$(kc_ns get pod robustness-runner -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  handle_host_request || true
  case "$RUNNER_PHASE" in
    Succeeded|Failed) break ;;
  esac
  sleep 1
done

kc_ns logs pod/robustness-runner | tee "$ARTIFACTS/robustness.test.log" >/dev/null || true
cp "$ARTIFACTS/robustness.test.log" "$ARTIFACTS/runner-stdout.log" 2>/dev/null || true

if [[ "$RUNNER_PHASE" != "Succeeded" ]]; then
  die "runner phase=$RUNNER_PHASE (log: $ARTIFACTS/robustness.test.log)"
fi

grep -E '^--- PASS: TestOwnerCrash/owner_is_leader' "$ARTIFACTS/robustness.test.log" >/dev/null \
  || die "required subtest owner_is_leader did not PASS"
grep -E '^--- PASS: TestOwnerCrash/owner_is_not_leader' "$ARTIFACTS/robustness.test.log" >/dev/null \
  || die "required subtest owner_is_not_leader did not PASS"
grep -E '^--- PASS: TestOwnerCrash ' "$ARTIFACTS/robustness.test.log" >/dev/null \
  || die "TestOwnerCrash parent did not PASS"

log "B1 robustness passed; artifacts in $ARTIFACTS"
