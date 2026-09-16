#!/usr/bin/env bash
# Issue #493 pod-replacement scenario.
#
# A StatefulSet pod keeps its PVC across replacement but not its pod IP, and
# dqlite records the node's advertised address inside that PVC. This lane
# installs the chart with three replicas and retained volumes, creates durable
# data, replaces every pod in turn *proving the pod IP actually changed*, and
# then asserts the StatefulSet returns to 3/3, the replaced member rejoins the
# dqlite cluster at its new address, the run recorded beforehand is still
# readable, and a new job executes.
#
# A replacement that only succeeded because the CNI happened to hand back the
# same IP proves nothing, so this script fails rather than passing in that case.
#
# Unique kind cluster + namespace from CAESIUM_REPLACEMENT_ID. Every kubectl and
# helm call uses the explicit kubeconfig; the caller's context is never touched.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
die() { log "ERROR: $*"; exit 1; }

require_cmd() { command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"; }
require_env() { [[ -n "${!1:-}" ]] || die "$1 is required"; }

require_cmd kind
require_cmd kubectl
require_cmd helm
require_cmd docker
require_cmd curl
require_cmd python3

require_env CAESIUM_REPLACEMENT_ID
require_env CAESIUM_REPLACEMENT_ARTIFACTS
require_env CAESIUM_REPLACEMENT_IMAGE
require_env CAESIUM_REPLACEMENT_KIND_IMAGE
require_env CAESIUM_REPLACEMENT_TASK_IMAGE

REPLACEMENT_ID="$CAESIUM_REPLACEMENT_ID"
if ! printf '%s' "$REPLACEMENT_ID" | grep -Eq '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$' || [[ ${#REPLACEMENT_ID} -gt 47 ]]; then
  die "CAESIUM_REPLACEMENT_ID must be a lowercase DNS-1123 name <= 47 chars, got $REPLACEMENT_ID"
fi

ARTIFACTS="$CAESIUM_REPLACEMENT_ARTIFACTS"
mkdir -p "$ARTIFACTS"
ARTIFACTS="$(cd "$ARTIFACTS" && pwd)"

SERVER_IMAGE="$CAESIUM_REPLACEMENT_IMAGE"
KIND_IMAGE="$CAESIUM_REPLACEMENT_KIND_IMAGE"
TASK_IMAGE="$CAESIUM_REPLACEMENT_TASK_IMAGE"
NAMESPACE="$REPLACEMENT_ID"
RELEASE="caesium"
DQLITE_PORT=9001
VALUES="$ROOT/helm/caesium/ci/test-values-replacement.yaml"
KUBECONFIG_PATH="$ARTIFACTS/kubeconfig"
OWNED_CLUSTER=""
PF_PID=""
PF_PORT="${CAESIUM_REPLACEMENT_PORT:-18493}"
API="http://127.0.0.1:${PF_PORT}"
JOB_ID=""
FIRST_RUN=""

unset KUBECONFIG || true
export KUBECONFIG="$KUBECONFIG_PATH"

kc() { kubectl --kubeconfig "$KUBECONFIG_PATH" "$@"; }
kcn() { kubectl --kubeconfig "$KUBECONFIG_PATH" --namespace "$NAMESPACE" "$@"; }

pf_stop() {
  if [[ -n "$PF_PID" ]] && kill -0 "$PF_PID" 2>/dev/null; then
    kill "$PF_PID" 2>/dev/null || true
    wait "$PF_PID" 2>/dev/null || true
  fi
  PF_PID=""
}

pf_start() {
  pf_stop
  kcn port-forward "service/${RELEASE}" "${PF_PORT}:8080" >"$ARTIFACTS/port-forward.log" 2>&1 &
  PF_PID=$!
  local tries=0
  until curl -sf "${API}/health" >/dev/null 2>&1; do
    tries=$((tries + 1))
    if [[ $tries -gt 90 ]]; then
      cat "$ARTIFACTS/port-forward.log" >&2 || true
      die "port-forward to ${RELEASE} never became ready"
    fi
    sleep 1
  done
}

collect() {
  [[ -f "$KUBECONFIG_PATH" ]] || return 0
  kcn get pods -o wide >"$ARTIFACTS/pods.txt" 2>/dev/null || true
  kcn describe pods >"$ARTIFACTS/describe-pods.txt" 2>/dev/null || true
  kcn get pvc >"$ARTIFACTS/pvc.txt" 2>/dev/null || true
  kcn get events --sort-by=.lastTimestamp >"$ARTIFACTS/events.txt" 2>/dev/null || true
  local ordinal
  for ordinal in 0 1 2; do
    kcn logs "${RELEASE}-${ordinal}" -c caesium --tail=400 \
      >"$ARTIFACTS/${RELEASE}-${ordinal}.log" 2>/dev/null || true
    kcn logs "${RELEASE}-${ordinal}" -c caesium --previous --tail=400 \
      >"$ARTIFACTS/${RELEASE}-${ordinal}.previous.log" 2>/dev/null || true
  done
}

cleanup() {
  local status=$?
  set +e
  pf_stop
  collect
  if [[ -n "$OWNED_CLUSTER" && "${CAESIUM_REPLACEMENT_KEEP_CLUSTER:-}" != "1" ]]; then
    log "cleanup: kind delete cluster --name $OWNED_CLUSTER"
    kind delete cluster --name "$OWNED_CLUSTER" >/dev/null 2>&1 || true
  fi
  exit "$status"
}
trap cleanup EXIT INT TERM

docker image inspect "$SERVER_IMAGE" >/dev/null || die "server image not present locally: $SERVER_IMAGE"
docker image inspect "$KIND_IMAGE" >/dev/null 2>&1 || docker pull "$KIND_IMAGE" >/dev/null
docker image inspect "$TASK_IMAGE" >/dev/null 2>&1 || docker pull "$TASK_IMAGE" >/dev/null

# The chart splits image.repository and image.tag, so publish the candidate
# under a predictable ref instead of parsing the caller's.
CHART_REPO="caesiumcloud/caesium"
CHART_TAG="replacement-${REPLACEMENT_ID}"
docker tag "$SERVER_IMAGE" "${CHART_REPO}:${CHART_TAG}"

existing="$(kind get clusters 2>/dev/null || true)"
if printf '%s\n' "$existing" | grep -qxF "$REPLACEMENT_ID"; then
  die "kind cluster $REPLACEMENT_ID already exists; refusing to claim or delete it"
fi

log "creating kind cluster $REPLACEMENT_ID ($KIND_IMAGE)"
OWNED_CLUSTER="$REPLACEMENT_ID"
kind create cluster \
  --name "$REPLACEMENT_ID" \
  --image "$KIND_IMAGE" \
  --kubeconfig "$KUBECONFIG_PATH" \
  --wait 180s

log "loading images into the cluster"
kind load docker-image --name "$REPLACEMENT_ID" "${CHART_REPO}:${CHART_TAG}"
kind load docker-image --name "$REPLACEMENT_ID" "$TASK_IMAGE"

kc create namespace "$NAMESPACE"

log "installing the chart: 3 replicas, persistence enabled"
if ! helm --kubeconfig "$KUBECONFIG_PATH" install "$RELEASE" ./helm/caesium \
  --namespace "$NAMESPACE" \
  --values "$VALUES" \
  --set "image.repository=${CHART_REPO}" \
  --set "image.tag=${CHART_TAG}" \
  --wait --timeout 480s; then
  collect
  die "helm install never became ready"
fi

api() {
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "$body" ]]; then
    curl -sS --max-time 30 -X "$method" -H 'Content-Type: application/json' -d "$body" "${API}${path}"
  else
    curl -sS --max-time 30 -X "$method" "${API}${path}"
  fi
}

# Retries the whole call and re-establishes the port-forward: replacing a pod
# can move the forward's target out from under us.
api_retry() {
  local method="$1" path="$2" body="${3:-}" out="" attempt=0
  while [[ $attempt -lt 30 ]]; do
    if out="$(api "$method" "$path" "$body" 2>/dev/null)" && [[ -n "$out" ]]; then
      printf '%s' "$out"
      return 0
    fi
    attempt=$((attempt + 1))
    sleep 2
    pf_start
  done
  return 1
}

pod_ip() { kcn get pod "$1" -o jsonpath='{.status.podIP}' 2>/dev/null; }

persisted_address() {
  kcn exec "$1" -c caesium -- cat /var/lib/caesium/dqlite/info.yaml 2>/dev/null |
    sed -n 's/^Address: *//p' | tr -d '\r'
}

# CAESIUM_REPLACEMENT_READY_TIMEOUT shortens the wait for a local reproduction
# against a build that cannot recover; CI keeps the generous default.
READY_TIMEOUT="${CAESIUM_REPLACEMENT_READY_TIMEOUT:-480}"

wait_all_ready() {
  local deadline=$((SECONDS + READY_TIMEOUT)) ready=""
  while [[ $SECONDS -lt $deadline ]]; do
    ready="$(kcn get statefulset "$RELEASE" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
    if [[ "$ready" == "3" ]]; then
      return 0
    fi
    sleep 5
  done
  collect
  die "statefulset never returned to 3/3 ready (last readyReplicas: ${ready:-0})"
}

apply_job() {
  local alias="$1" body out
  body="$(python3 - "$alias" "$TASK_IMAGE" <<'PY'
import json, sys
alias, image = sys.argv[1], sys.argv[2]
print(json.dumps({"definitions": [{
    "apiVersion": "v1",
    "kind": "Job",
    "metadata": {"alias": alias},
    "trigger": {"type": "http", "configuration": {"path": alias}},
    "steps": [{
        "name": "probe",
        "type": "task",
        "engine": "kubernetes",
        "image": image,
        "command": ["sh", "-c", "echo caesium-pod-replacement-probe"],
    }],
}]}))
PY
)"
  out="$(api_retry POST /v1/jobdefs/apply "$body")" || die "applying $alias failed"
  printf '%s' "$out" | grep -q '"applied":1' || die "applying $alias did not apply: $out"
}

job_id() {
  api_retry GET /v1/jobs | python3 -c '
import json, sys
alias = sys.argv[1]
payload = json.load(sys.stdin)
jobs = payload.get("jobs", []) if isinstance(payload, dict) else payload
for job in jobs:
    if job.get("alias") == alias:
        print(job["id"])
        break
else:
    raise SystemExit("job %s not found" % alias)
' "$1"
}

start_run() {
  api_retry POST "/v1/jobs/${1}/run" '{}' | python3 -c '
import json, sys
run = json.load(sys.stdin)
run_id = run.get("id")
if not run_id:
    raise SystemExit("no run id in response: %r" % run)
print(run_id)
'
}

run_status() {
  api_retry GET "/v1/jobs/${1}/runs/${2}" | python3 -c '
import json, sys
print(json.load(sys.stdin).get("status", ""))
'
}

await_run() {
  local job="$1" run="$2" deadline=$((SECONDS + 420)) state=""
  while [[ $SECONDS -lt $deadline ]]; do
    state="$(run_status "$job" "$run" 2>/dev/null || true)"
    case "$state" in
      succeeded) return 0 ;;
      failed | cancelled | skipped) collect; die "run $run finished $state" ;;
    esac
    sleep 5
  done
  collect
  die "run $run never finished (last status: ${state:-unknown})"
}

# Raft membership only. /v1/system/nodes also reports historical worker
# addresses taken from task_runs.claimed_by, and a node that ran a task before
# its replacement is still recorded there; those are not cluster members.
raft_members() {
  api_retry GET /v1/system/nodes | python3 -c '
import json, sys
payload = json.load(sys.stdin)
nodes = payload.get("nodes", []) if isinstance(payload, dict) else payload
for node in nodes:
    if node.get("role") in ("voter", "standby", "spare"):
        print("%s %s" % (node.get("address"), node.get("role")))
'
}

has_member() {
  printf '%s\n' "$1" | awk -v addr="$2" '$1 == addr { found = 1 } END { exit found ? 0 : 1 }'
}

pf_start
log "cluster is up; recording the starting topology"
for ordinal in 0 1 2; do
  log "  ${RELEASE}-${ordinal} pod_ip=$(pod_ip "${RELEASE}-${ordinal}") info.yaml=$(persisted_address "${RELEASE}-${ordinal}")"
done
raft_members | tee "$ARTIFACTS/members-before.txt"

ALIAS="replacement-probe"
apply_job "$ALIAS"
JOB_ID="$(job_id "$ALIAS")"
log "job $ALIAS id=$JOB_ID"
FIRST_RUN="$(start_run "$JOB_ID")"
log "durable run recorded before any replacement: $FIRST_RUN"
await_run "$JOB_ID" "$FIRST_RUN"

REPLACED_OLD_ADDR=""
REPLACED_NEW_IP=""

replace_pod() {
  local pod="$1" before_ip before_addr attempt=0 after_ip=""
  before_ip="$(pod_ip "$pod")"
  before_addr="$(persisted_address "$pod")"
  [[ -n "$before_ip" ]] || die "could not read the pod IP of $pod"
  [[ -n "$before_addr" ]] || die "could not read the persisted dqlite address of $pod"

  while [[ $attempt -lt 4 ]]; do
    attempt=$((attempt + 1))
    log "replacing $pod (attempt $attempt): pod_ip=$before_ip info.yaml=$before_addr"
    kcn delete pod "$pod" --wait=true --timeout=180s
    # Only wait for the replacement to be scheduled and given an address.
    # Whether it becomes Ready is the assertion, not the precondition — a
    # regression that crash-loops must not cost this loop a Ready timeout.
    local waited=0
    while [[ $waited -lt 120 ]]; do
      after_ip="$(pod_ip "$pod")"
      if [[ -n "$after_ip" ]]; then
        break
      fi
      waited=$((waited + 2))
      sleep 2
    done
    if [[ -n "$after_ip" && "$after_ip" != "$before_ip" ]]; then
      log "$pod came back at $after_ip (was $before_ip)"
      REPLACED_OLD_ADDR="$before_addr"
      REPLACED_NEW_IP="$after_ip"
      return 0
    fi
    log "$pod was handed the same IP (${after_ip:-none}); retrying so the address really changes"
  done

  collect
  die "the CNI kept reusing $before_ip for $pod; without an address change this scenario proves nothing"
}

assert_recovered() {
  local pod="$1" old_addr="$2" new_ip="$3" persisted members run
  wait_all_ready

  persisted="$(persisted_address "$pod")"
  [[ "$persisted" == "${new_ip}:${DQLITE_PORT}" ]] ||
    die "$pod info.yaml records $persisted, expected ${new_ip}:${DQLITE_PORT}"

  pf_start
  members="$(raft_members)"
  printf '%s\n' "$members" >"$ARTIFACTS/members-after-${pod}.txt"
  log "dqlite membership after replacing $pod:"
  printf '%s\n' "$members" | sed 's/^/  /'

  has_member "$members" "${new_ip}:${DQLITE_PORT}" ||
    die "dqlite membership does not list $pod at its new address ${new_ip}:${DQLITE_PORT}"
  if has_member "$members" "$old_addr"; then
    die "dqlite membership still lists the replaced pod at its old address $old_addr"
  fi

  # Durable data written before any replacement is still readable ...
  [[ "$(run_status "$JOB_ID" "$FIRST_RUN")" == "succeeded" ]] ||
    die "the run recorded before the replacements ($FIRST_RUN) is no longer readable as succeeded"

  # ... and the cluster still schedules new work.
  run="$(start_run "$JOB_ID")"
  log "post-replacement run: $run"
  await_run "$JOB_ID" "$run"
}

# Sequential replacement, one pod at a time, quorum never intentionally broken.
# StatefulSets roll from the highest ordinal down, so follow the same order.
# CAESIUM_REPLACEMENT_ORDINALS narrows the sweep for a quick local reproduction;
# CI always runs the full set.
for ordinal in ${CAESIUM_REPLACEMENT_ORDINALS:-2 1 0}; do
  pod="${RELEASE}-${ordinal}"
  REPLACED_OLD_ADDR=""
  REPLACED_NEW_IP=""
  replace_pod "$pod"
  assert_recovered "$pod" "$REPLACED_OLD_ADDR" "$REPLACED_NEW_IP"
  log "$pod replaced, rejoined at a new address, and serving"
done

raft_members | tee "$ARTIFACTS/members-final.txt"
log "PASS: every replica was replaced with a new pod IP, rejoined the dqlite cluster, kept prior runs and executed new ones"
