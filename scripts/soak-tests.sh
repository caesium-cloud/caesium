#!/usr/bin/env bash
# F3 seeded single-host soak and exploratory qualification: host controller.
#
# Provisions an OWNED kind cluster (one control plane + three workers) with
# three persistent Caesium members from B3's harness values
# (helm/caesium/ci/test-values-robustness.yaml), runs the integration-tagged
# exploratory runner (test/robustness/exploratory_test.go, ^TestExploratory$)
# in-cluster, executes every fault the runner requests from the host, and
# writes a machine-readable record to $CAESIUM_SOAK_ARTIFACTS/soak.json.
#
#   CAESIUM_SOAK_ARTIFACTS="$(mktemp -d)" \
#   CAESIUM_SOAK_PROFILE=short \
#   CAESIUM_SOAK_SEED=12345 \
#     bash scripts/soak-tests.sh
#
# Shape mirrors scripts/robustness.sh (B1-B3: host-request ConfigMaps, kill
# evidence, image identity) and scripts/lifecycle-tests.sh (F4/F2: owned
# resources, candidate provenance, an `incomplete` record written first, a
# manifest in which a scenario with no record is blocked, every phase's exit
# code folded into the record).
#
# Inputs:
#   CAESIUM_SOAK_ARTIFACTS  (required) directory outside this checkout, or a git-ignored one inside it.
#   CAESIUM_SOAK_PROFILE    short (default) | nightly — test/robustness/workloads/<profile>.json.
#   CAESIUM_SOAK_SEED       integer from 1 to 999999999999999999 (10^18 - 1);
#                           generated in that range and recorded as generated
#                           when unset (soak-report.py owns the range). The seed
#                           reproduces the PLAN; OS scheduling is not
#                           reproducible, so the ACTUAL fault schedule is kept
#                           in fault-schedule.jsonl and soak.json.
#   CAESIUM_SOAK_DURATION   Go duration for the schedule budget (default from the
#                           workload: short 12m, nightly 30m). The mandatory pass
#                           (one episode of every family) always runs.
#   CAESIUM_SOAK_ID         kind cluster + namespace name (DNS-1123, <= 40
#                           chars); default soak-<random>. Must not exist.
#   CAESIUM_SOAK_KIND_IMAGE default kindest/node:v1.36.1 (B3's).
#   CAESIUM_SOAK_TASK_IMAGE default alpine:3.23.
#   CAESIUM_SOAK_QUEUE_MAX_DEPTH  server CAESIUM_RUN_QUEUE_MAX_DEPTH (default 5).
#   CAESIUM_SOAK_REUSE_IMAGES=1   use pre-built caesiumcloud/caesium{,-robustness}:<sha>
#                           instead of building them (provenance: supplied).
#   CAESIUM_SOAK_ALLOW_UNVERIFIED_IMAGE=1  proceed, and record it, when the
#                           candidate's provenance cannot be established (a
#                           supplied image, or a build from a dirty tree, which
#                           is tagged <sha>-dirty). Without it such a run is
#                           BLOCKED before any cluster is created.
#   CAESIUM_SOAK_KEEP_CLUSTER=1  leave the owned cluster for debugging.
#
# Candidate provenance follows the lifecycle controller: candidate_sha is this
# checkout's HEAD; the harness builds the server and runner images itself with
# `just tag=<sha> robustness-runner` from a clean tree (built-by-this-run).
#
# Ownership: the kind cluster is created only after its name was proven absent,
# and teardown deletes only that cluster. Task containers are matched on the
# kind nodes by a per-invocation ownership token (CAESIUM_SOAK_OWNER env on
# every soak step), never by name.
#
# Faults are process kills (kubelet stopped, then SIGKILL of the member's
# container through containerd) and member disk replacement (PVC + pod
# deletion). None of them is power-loss qualification.
#
# `bash scripts/soak-tests.sh --self-test` runs the hermetic helper checks only.
# `bash scripts/soak-tests.sh --print-seed` resolves CAESIUM_SOAK_SEED exactly
# as a run would (generating one when unset) and prints it: exit 0, or 3 for a
# seed a run would refuse. Nothing is created.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
REPORT="$ROOT/scripts/soak-report.py"
HOSTLOGIC="$ROOT/test/robustness/hostlogic.py"
VALUES="$ROOT/helm/caesium/ci/test-values-robustness.yaml"

log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
now_ms() { python3 -c 'import datetime;print(datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3]+"Z")'; }

if [[ "${1:-}" == "--self-test" ]]; then
  python3 "$HOSTLOGIC" self-test
  python3 -m unittest discover -s "$ROOT/scripts" -p 'test_soak_report.py'
  exit 0
fi

# Sets SEED, SEED_SOURCE (supplied|generated|invalid) and SEED_ERROR from
# CAESIUM_SOAK_SEED. Generation, validation and the recorded seed share the one
# range in soak-report.py, so a generated seed is never refused.
resolve_seed() {
  local out
  out="$(python3 "$REPORT" seed)" || return 2
  eval "$out"
}

if [[ "${1:-}" == "--print-seed" ]]; then
  resolve_seed || { log "ERROR: could not resolve CAESIUM_SOAK_SEED"; exit 2; }
  if [[ -n "$SEED_ERROR" ]]; then log "BLOCKED (inputs): $SEED_ERROR"; exit 3; fi
  printf 'seed=%s source=%s\n' "$SEED" "$SEED_SOURCE"
  exit 0
fi

for cmd in docker kind kubectl helm python3 just git; do
  command -v "$cmd" >/dev/null 2>&1 || { log "ERROR: required command not found: $cmd"; exit 2; }
done
python3 "$HOSTLOGIC" self-test >/dev/null || { log "ERROR: hostlogic self-test failed"; exit 2; }

: "${CAESIUM_SOAK_ARTIFACTS:?set CAESIUM_SOAK_ARTIFACTS to an absolute directory outside the checkout}"
mkdir -p "$CAESIUM_SOAK_ARTIFACTS"
ART="$(cd "$CAESIUM_SOAK_ARTIFACTS" && pwd)"
# Inside the checkout only when git ignores it (e.g. .tmp/lane-evidence), so the
# candidate tree stays clean for the provenance check.
case "$ART/" in
  "$ROOT/"*)
    git -C "$ROOT" check-ignore -q "$ART/soak.json" \
      || { log "ERROR: artifacts inside the checkout must be git-ignored ($ART)"; exit 2; }
    ;;
esac

SOAK_ID="${CAESIUM_SOAK_ID:-soak-$(python3 -c 'import secrets;print(secrets.token_hex(5))')}"
PROFILE="${CAESIUM_SOAK_PROFILE:-short}"
resolve_seed || { log "ERROR: could not resolve CAESIUM_SOAK_SEED"; exit 2; }
DURATION="${CAESIUM_SOAK_DURATION:-}"
KIND_IMAGE="${CAESIUM_SOAK_KIND_IMAGE:-kindest/node:v1.36.1}"
TASK_IMAGE_SRC="${CAESIUM_SOAK_TASK_IMAGE:-alpine:3.23}"
QUEUE_DEPTH="${CAESIUM_SOAK_QUEUE_MAX_DEPTH:-5}"
ALLOW_UNVERIFIED="${CAESIUM_SOAK_ALLOW_UNVERIFIED_IMAGE:-0}"
REUSE_IMAGES="${CAESIUM_SOAK_REUSE_IMAGES:-0}"
KEEP="${CAESIUM_SOAK_KEEP_CLUSTER:-0}"
NS="$SOAK_ID"
KUBE="$ART/kubeconfig"
LOGS="$ART/logs"
OWNER_TOKEN="$(python3 -c 'import secrets;print(secrets.token_hex(16))')"
CANDIDATE_SHA="$(git rev-parse HEAD)"

# Purge every per-run file so nothing from an earlier invocation can count.
rm -rf "$ART/records" "$ART/samples" "$ART/containers" "$LOGS"
rm -f "$ART/soak.json" "$ART/fault-schedule.jsonl" "$ART/phases.txt" "$ART/owned-cluster.txt" \
  "$ART/faulted-nodes.txt" "$ART/cordoned-nodes.txt" "$ART/candidate-identities.txt" "$ART/runner.yaml"
mkdir -p "$ART/records" "$ART/samples" "$ART/containers" "$LOGS"
: >"$ART/phases.txt"

# The placeholder is written before any input is judged, so even a refused
# invocation leaves a record that says why. It carries the seed this run uses,
# supplied or generated, with its source; an invalid supplied seed is recorded
# as null (source "invalid") and refused just below.
python3 "$REPORT" init --artifacts "$ART" --soak-id "$SOAK_ID" --candidate-sha "$CANDIDATE_SHA" \
  --seed "$SEED" --seed-source "$SEED_SOURCE" --profile "$PROFILE" --duration "$DURATION"

phase() { printf '%s=%s\n' "$1" "$2" >>"$ART/phases.txt"; }
record_set() { python3 "$REPORT" set --artifacts "$ART" "$@" >/dev/null; }
blocked() {
  local name="$1"
  shift
  phase "$name" 1
  record_set "blocked_phase=$name" "blocked_reason=$*"
  log "BLOCKED ($name): $*"
  exit 3
}

kc() { kubectl --kubeconfig "$KUBE" "$@"; }
kc_ns() { kubectl --kubeconfig "$KUBE" --namespace "$NS" "$@"; }
unset KUBECONFIG || true

OWNED=0
KIND_NODES=()
heal_faults() {
  local node
  for f in "$ART/faulted-nodes.txt" "$ART/cordoned-nodes.txt"; do
    [[ -f "$f" ]] || continue
    while IFS= read -r node; do
      [[ -n "$node" ]] || continue
      log "cleanup: restoring kubelet/uncordon on $node"
      docker exec "$node" systemctl start kubelet >/dev/null 2>&1 || true
      kc uncordon "$node" >/dev/null 2>&1 || true
    done <"$f"
  done
}

cleanup() {
  local rc=$? result cleanup_rc=0
  trap - EXIT INT TERM
  set +e
  if [[ "$OWNED" == 1 ]]; then
    heal_faults
    kc_ns logs pod/soak-runner >"$LOGS/runner-final.log" 2>&1
    kc get pods -A -o wide >"$LOGS/pods-final.txt" 2>&1
    kc_ns --request-timeout=10s get events --sort-by=.metadata.creationTimestamp -o wide >"$LOGS/events-final.txt" 2>&1
    for n in 0 1 2; do
      kc_ns --request-timeout=10s logs "caesium-$n" -c caesium --timestamps=true >"$LOGS/caesium-$n-current.log" 2>&1
      kc_ns --request-timeout=10s logs "caesium-$n" -c caesium --previous --timestamps=true >"$LOGS/caesium-$n-previous.log" 2>&1
    done
    if [[ "$KEEP" != 1 ]]; then
      if kind delete cluster --name "$SOAK_ID" >"$LOGS/kind-delete.log" 2>&1; then
        rm -f "$ART/owned-cluster.txt"
      else
        cleanup_rc=1
        log "ERROR: owned kind cluster $SOAK_ID could not be deleted; see $LOGS/kind-delete.log"
      fi
    else
      log "CAESIUM_SOAK_KEEP_CLUSTER=1; leaving owned cluster $SOAK_ID (kubeconfig $KUBE)"
    fi
    phase cleanup "$cleanup_rc"
  fi
  rm -f "$ART/internal-token.txt"
  python3 "$REPORT" finalize --artifacts "$ART"
  result="$(python3 "$REPORT" result --artifacts "$ART")"
  log "soak result=$result soak_id=$SOAK_ID seed=${SEED} profile=$PROFILE record=$ART/soak.json"
  if [[ "$result" == pass ]]; then exit 0; fi
  if [[ "$rc" -ne 0 ]]; then exit "$rc"; fi
  exit 1
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# ---------------------------------------------------------------------------
# Inputs.
# ---------------------------------------------------------------------------
[[ "$SOAK_ID" =~ ^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$ ]] || blocked inputs "invalid CAESIUM_SOAK_ID $SOAK_ID"
[[ "$PROFILE" == short || "$PROFILE" == nightly ]] || blocked inputs "CAESIUM_SOAK_PROFILE must be short or nightly"
[[ -z "$SEED_ERROR" ]] || blocked inputs "$SEED_ERROR"
if [[ -n "$DURATION" ]]; then
  [[ "$DURATION" =~ ^([0-9]+(h|m|s))+$ ]] || blocked inputs "CAESIUM_SOAK_DURATION must be a Go duration such as 30m or 1h"
fi
[[ "$QUEUE_DEPTH" =~ ^[1-9][0-9]{0,2}$ ]] || blocked inputs "CAESIUM_SOAK_QUEUE_MAX_DEPTH must be 1-999"
[[ ! -e "$KUBE" ]] || blocked inputs "$KUBE exists; refusing to adopt another cluster's kubeconfig"
BUDGET_S="$(python3 - "$PROFILE" "$DURATION" "$ROOT" <<'PY'
import json, re, sys
profile, duration, root = sys.argv[1:]
def secs(d):
    total = 0
    for n, u in re.findall(r"(\d+)(h|m|s)", d):
        total += int(n) * {"h": 3600, "m": 60, "s": 1}[u]
    return total
w = json.load(open(f"{root}/test/robustness/workloads/{profile}.json"))
print(secs(duration) if duration else secs(w["schedule_budget"]))
PY
)"
# Runner: the budget plus the mandatory pass, drain and samples; the host
# waits a little longer so it can still collect a timed-out runner's log.
RUNNER_TIMEOUT_S=$((BUDGET_S + 2400))
CONTROLLER_BUDGET_S=$((RUNNER_TIMEOUT_S + 300))
phase inputs 0
record_set "kind_image=$KIND_IMAGE" "task_image=$TASK_IMAGE_SRC" "queue_max_depth=$QUEUE_DEPTH" \
  "schedule_budget_seconds=$BUDGET_S" "runner_timeout_seconds=$RUNNER_TIMEOUT_S"
log "soak_id=$SOAK_ID profile=$PROFILE seed=$SEED budget=${BUDGET_S}s candidate=$CANDIDATE_SHA artifacts=$ART"

# ---------------------------------------------------------------------------
# Candidate provenance and images.
# ---------------------------------------------------------------------------
DIRTY="$(git status --porcelain)"
TAG="$CANDIDATE_SHA"
PROV_MODE=built-by-this-run
PROV_VERIFIED=true
PROV_OVERRIDE=false
if [[ "$REUSE_IMAGES" == 1 ]]; then
  PROV_MODE=supplied
  PROV_VERIFIED=false
elif [[ -n "$DIRTY" ]]; then
  PROV_MODE=built-from-dirty-tree
  PROV_VERIFIED=false
  TAG="${CANDIDATE_SHA}-dirty"
  printf '%s\n' "$DIRTY" >"$LOGS/dirty-tree.txt"
fi
if [[ "$PROV_VERIFIED" != true ]]; then
  if [[ "$ALLOW_UNVERIFIED" == 1 ]]; then
    PROV_OVERRIDE=true
  else
    record_set "provenance={\"mode\":\"$PROV_MODE\",\"verified\":false,\"override\":false}"
    blocked provenance "candidate provenance is $PROV_MODE; set CAESIUM_SOAK_ALLOW_UNVERIFIED_IMAGE=1 to proceed and record it"
  fi
fi
SERVER_IMAGE="caesiumcloud/caesium:$TAG"
RUNNER_IMAGE="caesiumcloud/caesium-robustness:$TAG"
phase provenance 0
if [[ "$REUSE_IMAGES" == 1 ]]; then
  if ! docker image inspect "$SERVER_IMAGE" >/dev/null 2>&1 || ! docker image inspect "$RUNNER_IMAGE" >/dev/null 2>&1; then
    blocked build "CAESIUM_SOAK_REUSE_IMAGES=1 but $SERVER_IMAGE or $RUNNER_IMAGE is absent"
  fi
else
  log "building $SERVER_IMAGE and $RUNNER_IMAGE (just tag=$TAG robustness-runner)"
  just "tag=$TAG" robustness-runner >"$LOGS/build.log" 2>&1 || blocked build "image build failed; see logs/build.log"
fi
phase build 0
SERVER_ID="$(docker image inspect --format '{{.Id}}' "$SERVER_IMAGE")"
RUNNER_ID="$(docker image inspect --format '{{.Id}}' "$RUNNER_IMAGE")"
record_set "provenance={\"mode\":\"$PROV_MODE\",\"verified\":$PROV_VERIFIED,\"override\":$PROV_OVERRIDE,\"tag\":\"$TAG\",\"server_image\":\"$SERVER_IMAGE\",\"server_image_id\":\"$SERVER_ID\",\"runner_image\":\"$RUNNER_IMAGE\",\"runner_image_id\":\"$RUNNER_ID\"}"
docker image inspect "$KIND_IMAGE" >/dev/null 2>&1 || docker pull "$KIND_IMAGE" >"$LOGS/pull-kind.log" 2>&1 \
  || blocked build "kind node image $KIND_IMAGE unavailable"
docker image inspect "$TASK_IMAGE_SRC" >/dev/null 2>&1 || docker pull "$TASK_IMAGE_SRC" >"$LOGS/pull-task.log" 2>&1 \
  || blocked build "task image $TASK_IMAGE_SRC unavailable"

# ---------------------------------------------------------------------------
# Owned kind cluster.
# ---------------------------------------------------------------------------
clusters="$(kind get clusters 2>&1)" || blocked kind_create "kind get clusters failed: $clusters"
if printf '%s\n' "$clusters" | grep -qxF "$SOAK_ID"; then
  blocked kind_create "kind cluster $SOAK_ID already exists; refusing to claim or delete it"
fi
if docker ps -a --format '{{.Names}}' | grep -qxF "${SOAK_ID}-control-plane"; then
  blocked kind_create "container ${SOAK_ID}-control-plane already exists; refusing to claim cluster $SOAK_ID"
fi
cat >"$ART/kind.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: ${SOAK_ID}
nodes:
  - role: control-plane
  - role: worker
  - role: worker
  - role: worker
EOF
# Absence was proven immediately above, so a create that fails half way is
# still this invocation's cluster and is deleted by the trap.
OWNED=1
printf '%s\n' "$SOAK_ID" >"$ART/owned-cluster.txt"
record_set "cluster=$SOAK_ID" "namespace=$NS"
log "kind create cluster $SOAK_ID ($KIND_IMAGE)"
kind create cluster --name "$SOAK_ID" --image "$KIND_IMAGE" --config "$ART/kind.yaml" \
  --kubeconfig "$KUBE" --wait 180s >"$LOGS/kind-create.log" 2>&1 || blocked kind_create "kind create failed; see logs/kind-create.log"
phase kind_create 0
while IFS= read -r node; do
  [[ -n "$node" ]] || continue
  [[ "$node" == "$SOAK_ID-"* ]] || blocked node_tools "kind returned a node outside the owned cluster: $node"
  KIND_NODES+=("$node")
done < <(kind get nodes --name "$SOAK_ID")
[[ ${#KIND_NODES[@]} -eq 4 ]] || blocked node_tools "expected 4 kind nodes, found ${#KIND_NODES[@]}"
for node in "${KIND_NODES[@]}"; do
  docker exec "$node" sh -c 'command -v systemctl >/dev/null && command -v ctr >/dev/null && command -v crictl >/dev/null' \
    || blocked node_tools "systemctl/ctr/crictl unavailable on $node; faults and container inventories would be unobservable"
done
phase node_tools 0
known_node() {
  local n="${1:-}"
  [[ -n "$n" ]] || return 1
  printf '%s\n' "${KIND_NODES[@]}" | grep -qxF "$n"
}

# Multi-arch tags and attestation lists fail kind import; flatten the task
# image to one platform without provenance (as scripts/robustness.sh does).
TASK_ARCH="$(docker image inspect --format '{{.Architecture}}' "$TASK_IMAGE_SRC")"
TASK_IMAGE="caesium-soak-task:${SOAK_ID}"
BUILDX_NO_DEFAULT_ATTESTATIONS=1 docker build --provenance=false --sbom=false --platform "linux/${TASK_ARCH}" \
  -t "$TASK_IMAGE" - >"$LOGS/task-image.log" 2>&1 <<EOF || blocked image_load "could not flatten $TASK_IMAGE_SRC"
FROM ${TASK_IMAGE_SRC}
EOF
for img in "$SERVER_IMAGE" "$RUNNER_IMAGE" "$TASK_IMAGE"; do
  log "kind load docker-image $img"
  kind load docker-image --name "$SOAK_ID" "$img" >>"$LOGS/kind-load.log" 2>&1 || blocked image_load "kind load $img failed; see logs/kind-load.log"
done
WORKER=""
for n in "${KIND_NODES[@]}"; do [[ "$n" == *control-plane* ]] || { WORKER="$n"; break; }; done
docker image inspect "$SERVER_IMAGE" >"$ART/server-image.json"
printf '%s\n' "$SERVER_ID" >"$ART/host-image-id.txt"
docker exec "$WORKER" ctr -n k8s.io images ls >"$LOGS/ctr-images-ls.txt" 2>&1 || true
python3 "$HOSTLOGIC" ctr-image-shas "caesiumcloud/caesium:${TAG}" <"$LOGS/ctr-images-ls.txt" >"$ART/imported-digest.txt" 2>/dev/null || true
docker exec "$WORKER" crictl inspecti -o json "caesiumcloud/caesium:${TAG}" >"$ART/crictl-inspecti.json" 2>/dev/null || echo '{}' >"$ART/crictl-inspecti.json"
python3 "$HOSTLOGIC" collect-identities "$ART/server-image.json" "$ART/imported-digest.txt" "$ART/crictl-inspecti.json" \
  "$ART/host-image-id.txt" >"$ART/candidate-identities.txt" || blocked image_load "could not collect candidate image identities"
[[ -s "$ART/candidate-identities.txt" ]] || blocked image_load "candidate identity set is empty"
phase image_load 0

# ---------------------------------------------------------------------------
# Three persistent members from B3's values, plus the queue settings the
# queue-overload family needs (the dequeuer is off in the base values).
# ---------------------------------------------------------------------------
INTERNAL_TOKEN="$(python3 -c 'import secrets;print(secrets.token_urlsafe(48))')"
printf '%s\n' "$INTERNAL_TOKEN" >"$ART/internal-token.txt"
NEXT_ENV="$(python3 - "$VALUES" <<'PY'
import re, sys
count, inside, indent = 0, False, None
for line in open(sys.argv[1], encoding="utf-8").read().splitlines():
    if re.match(r"^\s*extraEnv:\s*$", line):
        inside = True
        continue
    if not inside or not line.strip():
        continue
    m = re.match(r"^(\s*)- name:", line)
    if m:
        indent = len(m.group(1)) if indent is None else indent
        if len(m.group(1)) == indent:
            count += 1
        continue
    if indent is not None and len(line) - len(line.lstrip()) <= indent and not line.lstrip().startswith(("value", "-")):
        break
print(count)
PY
)"
[[ "$NEXT_ENV" =~ ^[0-9]+$ && "$NEXT_ENV" -gt 0 ]] || blocked helm_install "could not count config.extraEnv in $VALUES"
HELM_ARGS=(
  --namespace "$NS" --values "$VALUES" --set "image.tag=$TAG"
  --set "config.extraEnv[0].name=CAESIUM_INTERNAL_WAKEUP_TOKEN" --set-string "config.extraEnv[0].value=$INTERNAL_TOKEN"
  --set "config.extraEnv[$NEXT_ENV].name=CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED" --set-string "config.extraEnv[$NEXT_ENV].value=true"
  --set "config.extraEnv[$((NEXT_ENV + 1))].name=CAESIUM_RUN_QUEUE_MAX_DEPTH" --set-string "config.extraEnv[$((NEXT_ENV + 1))].value=$QUEUE_DEPTH"
)
rendered="$(helm template caesium "$ROOT/helm/caesium" "${HELM_ARGS[@]}" 2>&1)" || blocked helm_install "helm template failed: $rendered"
for want in CAESIUM_INTERNAL_WAKEUP_TOKEN CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED CAESIUM_RUN_QUEUE_MAX_DEPTH CAESIUM_EXECUTION_MODE; do
  printf '%s\n' "$rendered" | grep -q "$want" || blocked helm_install "rendered StatefulSet lacks $want; the extraEnv index is wrong"
done
log "helm install caesium into $NS"
helm install caesium "$ROOT/helm/caesium" --kubeconfig "$KUBE" --create-namespace "${HELM_ARGS[@]}" \
  --wait --timeout 300s >"$LOGS/helm-install.log" 2>&1 || blocked helm_install "helm install failed; see logs/helm-install.log"
phase helm_install 0

RUNNING_ID="$(python3 - "$KUBE" "$NS" <<'PY'
import json, subprocess, sys
kube, ns = sys.argv[1:]
def kc(*args):
    return json.loads(subprocess.check_output(["kubectl", "--kubeconfig", kube, "-n", ns, *args], text=True))
pvcs = [p for p in kc("get", "pvc", "-o", "json")["items"]
        if p["metadata"]["name"].startswith("data-caesium-") and p.get("status", {}).get("phase") == "Bound"]
if len(pvcs) != 3:
    raise SystemExit(f"expected 3 bound data PVCs, found {len(pvcs)}")
pods = [p for p in kc("get", "pods", "-l", "app.kubernetes.io/instance=caesium", "-o", "json")["items"]
        if any(c["name"] == "caesium" for c in p.get("status", {}).get("containerStatuses") or [])]
if len(pods) != 3:
    raise SystemExit(f"expected 3 caesium pods, found {len(pods)}")
nodes = {p["spec"]["nodeName"] for p in pods}
if len(nodes) != 3 or any("control-plane" in n for n in nodes):
    raise SystemExit(f"members are not on three distinct workers: {sorted(nodes)}")
ids = {next(c for c in p["status"]["containerStatuses"] if c["name"] == "caesium").get("imageID", "") for p in pods}
if len(ids) != 1 or not next(iter(ids)):
    raise SystemExit(f"members do not run one resolved image: {sorted(ids)}")
print(next(iter(ids)))
PY
)" || blocked topology "three persistent members on distinct workers were not established"
python3 "$HOSTLOGIC" image-match "$ART/candidate-identities.txt" "$RUNNING_ID" \
  || blocked topology "running image $RUNNING_ID does not match the loaded candidate"
printf '%s\n' "$RUNNING_ID" >"$ART/running-image-id.txt"
record_set "running_image_id=$RUNNING_ID"
phase topology 0

# ---------------------------------------------------------------------------
# In-cluster runner (control-plane node) and the host-request loop.
# ---------------------------------------------------------------------------
cat >"$ART/runner.yaml" <<EOF
apiVersion: v1
kind: ServiceAccount
metadata: {name: soak-runner, namespace: ${NS}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: soak-runner, namespace: ${NS}}
rules:
  - apiGroups: [""]
    resources: ["pods", "pods/log", "pods/status", "services", "endpoints", "configmaps", "persistentvolumeclaims", "events"]
    verbs: ["get", "list", "watch", "create", "update", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: soak-runner, namespace: ${NS}}
subjects: [{kind: ServiceAccount, name: soak-runner, namespace: ${NS}}]
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: soak-runner}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: robustness-host-request, namespace: ${NS}}
data: {request_id: "", action: ""}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: robustness-host-ack, namespace: ${NS}}
data: {request_id: "", action: "", status: ""}
---
apiVersion: v1
kind: Pod
metadata:
  name: soak-runner
  namespace: ${NS}
  labels: {app.kubernetes.io/name: robustness-runner}
spec:
  serviceAccountName: soak-runner
  restartPolicy: Never
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchExpressions: [{key: node-role.kubernetes.io/control-plane, operator: Exists}]
  tolerations:
    - {key: node-role.kubernetes.io/control-plane, operator: Exists, effect: NoSchedule}
  containers:
    - name: runner
      image: ${RUNNER_IMAGE}
      imagePullPolicy: IfNotPresent
      command: ["/bin/robustness.test"]
      args: ["-test.v", "-test.count=1", "-test.run", "^TestExploratory\$", "-test.timeout", "${RUNNER_TIMEOUT_S}s"]
      ports: [{name: recorder, containerPort: 8090}]
      env:
        - {name: POD_NAME, valueFrom: {fieldRef: {fieldPath: metadata.name}}}
        - {name: POD_NAMESPACE, valueFrom: {fieldRef: {fieldPath: metadata.namespace}}}
        - {name: POD_IP, valueFrom: {fieldRef: {fieldPath: status.podIP}}}
        - {name: CANDIDATE_SHA, value: "${CANDIDATE_SHA}"}
        - {name: CAESIUM_ROBUSTNESS_ID, value: "${SOAK_ID}"}
        - {name: CAESIUM_ROBUSTNESS_TASK_IMAGE, value: "${TASK_IMAGE}"}
        - {name: CAESIUM_ROBUSTNESS_SERVER_IMAGE, value: "${SERVER_IMAGE}"}
        - {name: CAESIUM_ROBUSTNESS_CANDIDATE_DIGEST, value: "${RUNNING_ID}"}
        - {name: CAESIUM_MANUAL_TRIGGER_API_KEY, value: caesium-robustness-manual-key}
        - {name: CAESIUM_SOAK_SEED, value: "${SEED}"}
        - {name: CAESIUM_SOAK_PROFILE, value: "${PROFILE}"}
        - {name: CAESIUM_SOAK_DURATION, value: "${DURATION}"}
        - {name: CAESIUM_SOAK_OWNER_TOKEN, value: "${OWNER_TOKEN}"}
      resources:
        requests: {cpu: 50m, memory: 128Mi}
        limits: {cpu: "1", memory: 512Mi}
EOF
kc apply -f "$ART/runner.yaml" >"$LOGS/runner-apply.log" 2>&1 || blocked runner_start "runner manifest rejected; see logs/runner-apply.log"
kc_ns wait --for=condition=Ready pod/soak-runner --timeout=180s >>"$LOGS/runner-apply.log" 2>&1 \
  || blocked runner_start "runner pod never became Ready"
phase runner_start 0

write_ack() {
  local request_id="$1" action="$2" status="$3" evidence="${4:-}" error="${5:-}"
  REQUEST_ID="$request_id" ACTION="$action" STATUS="$status" EVIDENCE="$evidence" ERROR="$error" \
    python3 -c 'import json,os;print(json.dumps({k.lower():os.environ[k] for k in ("REQUEST_ID","ACTION","STATUS","EVIDENCE","ERROR")}))' \
    >"$LOGS/host-ack.json"
  kc_ns create configmap robustness-host-ack --from-file=payload="$LOGS/host-ack.json" \
    --from-literal=request_id="$request_id" --from-literal=action="$action" --from-literal=status="$status" \
    --dry-run=client -o yaml | kc apply -f - >/dev/null
}

schedule() {
  # schedule <status> <started_at> [evidence_file] [detail]
  python3 "$REPORT" schedule-append --artifacts "$ART" --action "$r_action" --status "$1" \
    --request-id "$r_request_id" --episode "$p_episode" --family "$p_family" \
    --pod "${r_owner_pod:-$p_pod}" --node "$r_owner_kind_node" --container-id "$r_owner_container_id" \
    --run-id "$r_run_id" --requested-at "$r_requested_at" --started-at "$2" \
    --evidence-file "${3:-}" --detail "${4:-}"
}

# Runs inside each member (sh in the member container); expanded there, not
# here. Besides /proc/1 and /metrics it lists the dqlite data directory and asks
# the member for the database size: dqlite keeps the whole database in memory
# (its VFS) and holds the retained Raft log in memory too (W7-gamma), so both
# grow RSS by design as the soak retains runs and events.
SAMPLE_SCRIPT="$(cat <<'SH'
printf "CMDLINE %s\n" "$(tr "\0" " " </proc/1/cmdline)"
grep -E "^(VmRSS|VmHWM|Threads):" /proc/1/status
printf "FDS %s\n" "$(ls /proc/1/fd | wc -l)"
printf "MEMMAX %s\n" "$(cat /sys/fs/cgroup/memory.max 2>/dev/null || echo unknown)"
printf "MEMCUR %s\n" "$(cat /sys/fs/cgroup/memory.current 2>/dev/null || echo 0)"
for f in /var/lib/caesium/dqlite/*; do printf "DQFILE %s %s\n" "$(stat -c %s "$f")" "${f##*/}"; done
printf "DBQUERY %s\n" "$(wget -qO- -T 10 --header 'Content-Type: application/json' --post-data '{"sql":"SELECT page_count * page_size FROM pragma_page_count(), pragma_page_size()","limit":1}' http://127.0.0.1:8080/v1/database/query | tr -d '\n')"
wget -qO- -T 10 http://127.0.0.1:8080/metrics | grep -E "^(go_goroutines|go_threads|go_memstats_heap_inuse_bytes|go_memstats_sys_bytes|process_open_fds|process_resident_memory_bytes) "
SH
)"

take_sample() {
  local label="$1" pod
  for pod in caesium-0 caesium-1 caesium-2; do
    kc_ns exec "$pod" -c caesium -- sh -c "$SAMPLE_SCRIPT" >"$ART/samples/$label--$pod.txt" 2>&1 || true
    kc_ns get pod "$pod" -o json 2>/dev/null | python3 -c '
import json,sys
p=json.load(sys.stdin)
cs=next((c for c in p.get("status",{}).get("containerStatuses") or [] if c["name"]=="caesium"),{})
print(json.dumps({"uid":p["metadata"]["uid"],"node":p["spec"].get("nodeName",""),"ip":p["status"].get("podIP",""),
  "container_id":cs.get("containerID",""),"restart_count":cs.get("restartCount")}))' \
      >"$ART/samples/$label--$pod.pod.json" 2>/dev/null || true
  done
  python3 "$REPORT" sample-parse --dir "$ART/samples" --label "$label"
}

# Runs one inventory command: stdout to <out>, stderr to <out>.err, and its
# exit code to <out>.rc, written last. soak-report.py counts an observation
# only when <out>.rc says 0 and <out> parses, so a failed, interrupted or empty
# collection is missing evidence, never an empty inventory.
collect() {
  local out="$1" rc=0
  shift
  rm -f "$out" "$out.rc" "$out.err"
  "$@" >"$out" 2>"$out.err" || rc=$?
  printf '%s\n' "$rc" >"$out.rc"
  return "$rc"
}

take_inventory() {
  local label="$1" node id ps
  for node in "${KIND_NODES[@]}"; do
    ps="$ART/containers/$label--ps--$node.json"
    collect "$ps" docker exec "$node" crictl ps -a -o json || continue
    for id in $(python3 "$REPORT" task-container-ids --file "$ps" --namespace "$NS"); do
      collect "$ART/containers/$label--inspect--$id.json" docker exec "$node" crictl inspect "$id" || true
    done
  done
  collect "$ART/containers/$label--pods.json" kc_ns get pods -l cloud.caesium -o json || true
  python3 "$REPORT" containers-parse --dir "$ART/containers" --label "$label" --namespace "$NS" \
    --token "$OWNER_TOKEN" --expect-nodes "${#KIND_NODES[@]}"
}

LAST_REQUEST_ID=""
# Fields of the current request, assigned by `soak-report.py parse-request`
# (a fixed, shell-quoted key set; nothing else reaches the shell).
r_request_id="" r_action="" r_owner_pod="" r_owner_kind_node="" r_owner_container_id="" r_run_id="" r_requested_at=""
# p_token and p_key are parsed for completeness; the host uses its own token
# and store-record re-reads the key from the request itself.
# shellcheck disable=SC2034
p_episode="" p_family="" p_label="" p_token="" p_pod="" p_key=""
handle_host_request() {
  local reqjson started evidence listing before rc killed seen short last_err out uid
  reqjson="$(kc_ns get configmap robustness-host-request -o json 2>/dev/null)" || return 0
  printf '%s' "$reqjson" >"$LOGS/host-request.json"
  eval "$(python3 "$REPORT" parse-request <"$LOGS/host-request.json")"
  [[ -n "$r_action" && -n "$r_request_id" ]] || return 0
  [[ "$r_request_id" != "$LAST_REQUEST_ID" ]] || return 0
  LAST_REQUEST_ID="$r_request_id"
  started="$(now_ms)"
  [[ "$r_action" == record ]] || log "host request action=$r_action id=$r_request_id episode=$p_episode pod=${r_owner_pod:-$p_pod} node=$r_owner_kind_node"
  case "$r_action" in
    record)
      if out="$(python3 "$REPORT" store-record --artifacts "$ART" <"$LOGS/host-request.json" 2>&1)"; then
        write_ack "$r_request_id" record ok "stored $out"
      else
        write_ack "$r_request_id" record failed "" "$out"
      fi
      ;;
    sample)
      [[ "$p_label" =~ ^[a-z0-9-]{1,40}$ ]] || { write_ack "$r_request_id" sample failed "" "invalid label"; return 0; }
      out="$(take_sample "$p_label" 2>&1)" || true
      write_ack "$r_request_id" sample ok "$out"
      ;;
    containers)
      [[ "$p_label" =~ ^[a-z0-9-]{1,40}$ ]] || { write_ack "$r_request_id" containers failed "" "invalid label"; return 0; }
      out="$(take_inventory "$p_label" 2>&1)" || true
      write_ack "$r_request_id" containers ok "$out"
      ;;
    cordon)
      if ! known_node "$r_owner_kind_node"; then
        write_ack "$r_request_id" cordon failed "" "node '$r_owner_kind_node' is not in cluster $SOAK_ID"; schedule failed "$started"; return 0
      fi
      printf '%s\n' "$r_owner_kind_node" >>"$ART/cordoned-nodes.txt"
      if kc cordon "$r_owner_kind_node" >/dev/null 2>&1; then
        write_ack "$r_request_id" cordon ok "cordoned $r_owner_kind_node"; schedule ok "$started"
      else
        write_ack "$r_request_id" cordon failed "" "kubectl cordon failed"; schedule failed "$started"
      fi
      ;;
    kill)
      if ! known_node "$r_owner_kind_node" || [[ -z "$r_owner_container_id" ]]; then
        write_ack "$r_request_id" kill failed "" "kill needs a known node and container id"; schedule failed "$started"; return 0
      fi
      kc_ns --request-timeout=20s logs "$r_owner_pod" -c caesium --timestamps=true \
        >"$LOGS/before-kill-$r_owner_pod-$r_request_id.log" 2>&1 || true
      # Register the heal BEFORE stopping kubelet.
      printf '%s\n' "$r_owner_kind_node" >>"$ART/faulted-nodes.txt"
      sync
      if ! docker exec "$r_owner_kind_node" systemctl stop kubelet; then
        write_ack "$r_request_id" kill failed "" "systemctl stop kubelet failed"; schedule failed "$started"; return 0
      fi
      rc=0
      before="$(docker exec "$r_owner_kind_node" ctr -n k8s.io tasks list 2>&1)" || rc=$?
      short="${r_owner_container_id:0:12}"
      if [[ "$rc" -ne 0 ]] || ! python3 "$HOSTLOGIC" listing-valid <<<"$before" || [[ "$before" != *"$short"* ]]; then
        write_ack "$r_request_id" kill failed "$before" "container $r_owner_container_id not in a valid ctr tasks list before the kill"
        schedule failed "$started" "" "container absent before kill"
        return 0
      fi
      seen=1 killed=0 last_err="" listing=""
      for _ in $(seq 1 20); do
        docker exec "$r_owner_kind_node" ctr -n k8s.io tasks kill --signal SIGKILL "$r_owner_container_id" >/dev/null 2>&1 || true
        rc=0
        listing="$(docker exec "$r_owner_kind_node" ctr -n k8s.io tasks list 2>&1)" || rc=$?
        if [[ "$rc" -ne 0 ]] || ! python3 "$HOSTLOGIC" listing-valid <<<"$listing"; then
          last_err="ctr tasks list after kill failed (rc=$rc)"; sleep 1; continue
        fi
        if python3 "$HOSTLOGIC" task-dead "$r_owner_container_id" "$seen" <<<"$listing" 2>/dev/null; then
          killed=1; break
        fi
        last_err="process still running"
        sleep 1
      done
      evidence="$(printf 'kubelet stopped on %s\nctr kill %s\n%s\n' "$r_owner_kind_node" "$r_owner_container_id" "$listing")"
      printf '%s\n' "$evidence" >"$LOGS/kill-$r_request_id.txt"
      if [[ "$killed" -ne 1 ]]; then
        write_ack "$r_request_id" kill failed "$evidence" "$last_err"; schedule failed "$started" "logs/kill-$r_request_id.txt" "$last_err"
        return 0
      fi
      write_ack "$r_request_id" kill ok "$evidence"
      schedule ok "$started" "logs/kill-$r_request_id.txt" "process kill: kubelet stopped, SIGKILL via containerd; not power loss"
      ;;
    restart)
      if ! known_node "$r_owner_kind_node"; then
        write_ack "$r_request_id" restart failed "" "unknown node"; schedule failed "$started"; return 0
      fi
      docker exec "$r_owner_kind_node" systemctl start kubelet >/dev/null 2>&1 || true
      kc uncordon "$r_owner_kind_node" >/dev/null 2>&1 || true
      for f in "$ART/faulted-nodes.txt" "$ART/cordoned-nodes.txt"; do
        [[ -f "$f" ]] || continue
        grep -vxF "$r_owner_kind_node" "$f" >"$f.tmp" || true
        mv "$f.tmp" "$f"
      done
      write_ack "$r_request_id" restart ok "started kubelet and uncordoned $r_owner_kind_node"
      schedule ok "$started" "" "heal: kubelet started, node uncordoned"
      ;;
    replace)
      if ! [[ "$p_pod" =~ ^caesium-[0-9]$ ]]; then
        write_ack "$r_request_id" replace failed "" "refusing to replace '$p_pod'"; schedule failed "$started"; return 0
      fi
      uid="$(kc_ns get pod "$p_pod" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
      kc_ns --request-timeout=20s logs "$p_pod" -c caesium --timestamps=true \
        >"$LOGS/before-replace-$p_pod-$r_request_id.log" 2>&1 || true
      if kc_ns delete pvc "data-$p_pod" --wait=false >"$LOGS/replace-$r_request_id.txt" 2>&1 \
        && kc_ns delete pod "$p_pod" --wait=false >>"$LOGS/replace-$r_request_id.txt" 2>&1; then
        evidence="deleted pvc data-$p_pod and pod $p_pod (old uid $uid); the StatefulSet recreates both on a fresh volume"
        write_ack "$r_request_id" replace ok "$evidence"
        schedule ok "$started" "logs/replace-$r_request_id.txt" "member disk replacement (PVC+pod deleted)"
      else
        write_ack "$r_request_id" replace failed "" "delete failed; see logs/replace-$r_request_id.txt"
        schedule failed "$started" "logs/replace-$r_request_id.txt"
      fi
      ;;
    done)
      write_ack "$r_request_id" "done" ok "done"
      ;;
    *)
      write_ack "$r_request_id" "$r_action" failed "" "unknown action"
      ;;
  esac
}

log "host controller serving the runner (timeout ${RUNNER_TIMEOUT_S}s, budget ${CONTROLLER_BUDGET_S}s)"
DEADLINE=$((SECONDS + CONTROLLER_BUDGET_S))
RUNNER_PHASE=""
while (( SECONDS < DEADLINE )); do
  RUNNER_PHASE="$(kc_ns get pod soak-runner -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  handle_host_request || log "host request handler error (continuing)"
  case "$RUNNER_PHASE" in Succeeded|Failed) break ;; esac
  sleep 1
done
kc_ns logs pod/soak-runner >"$LOGS/runner.log" 2>&1 || true
case "$RUNNER_PHASE" in
  Succeeded) phase runner 0 ;;
  Failed) phase runner 1 ;;
  *) phase runner 124 ;;
esac
grep -E -e '--- (PASS|FAIL|SKIP)' "$LOGS/runner.log" >"$LOGS/runner-verdicts.txt" 2>/dev/null || true
log "runner phase=$RUNNER_PHASE; $(wc -l <"$LOGS/runner-verdicts.txt" | tr -d ' ') subtest verdicts in logs/runner-verdicts.txt"
