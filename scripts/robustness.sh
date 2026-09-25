#!/usr/bin/env bash
# Robustness host controller (B1 owner crash, B2 targeted faults, B3 core suite).
# Unique kind cluster + namespace from CAESIUM_ROBUSTNESS_ID. Every kubectl/helm
# call uses the explicit kubeconfig. The in-cluster test never docker-execs nodes.
#
# Optional inputs (all default to B1's behaviour, so the early-evidence lane is
# unchanged when none of them is set):
#
#   CAESIUM_ROBUSTNESS_RUN
#       -test.run pattern for the in-cluster runner. Default '^TestOwnerCrash$'.
#       The required subtests and recorder keys are derived from it, so a
#       selection that runs nothing cannot pass.
#   CAESIUM_ROBUSTNESS_INSTRUMENTED_IMAGE
#       A server image built with the `testfault` build tag. When set it is
#       deployed instead of the release image and the runner is told the
#       durable-event-before-publication control is available. When unset the
#       ordinary release image is deployed and that control is compiled out.
#   CAESIUM_ROBUSTNESS_TESTFAULT_DIR
#       Control directory inside each member's own emptyDir. Default
#       /tmp/caesium-testfault.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
HOSTLOGIC="$ROOT/test/robustness/hostlogic.py"

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
[[ -f "$HOSTLOGIC" ]] || die "missing $HOSTLOGIC"
python3 "$HOSTLOGIC" self-test >/dev/null || die "hostlogic.py self-test failed"

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
RELEASE_IMAGE="$CAESIUM_ROBUSTNESS_SERVER_IMAGE"
DEPLOY_TAG="$CANDIDATE_SHA"
CANONICAL_SERVER_IMAGE="caesiumcloud/caesium:${DEPLOY_TAG}"
NAMESPACE="$ROBUSTNESS_ID"
VALUES="$ROOT/helm/caesium/ci/test-values-robustness.yaml"
LAST_REQUEST_ID=""
PAUSED_TASK_FILE="$ARTIFACTS/paused-task"
PARTITION_FILE="$ARTIFACTS/installed-partitions"

# Test selection. Everything below derives from it so a selection that executes
# nothing can never report success.
RUN_PATTERN="${CAESIUM_ROBUSTNESS_RUN:-^TestOwnerCrash\$}"
INSTRUMENTED_IMAGE="${CAESIUM_ROBUSTNESS_INSTRUMENTED_IMAGE:-}"
TESTFAULT_DIR="${CAESIUM_ROBUSTNESS_TESTFAULT_DIR:-/tmp/caesium-testfault}"
# Marker strings that must NOT exist in a release binary. They are defined only
# in internal/testfault's build-tagged file, so a release image containing them
# would mean the test-only control had leaked into a shippable artifact.
TESTFAULT_MARKERS=(caesium-testfault-control CAESIUM_TESTFAULT_DIR bus-publish-pause.json)

case "$RUN_PATTERN" in
  *TestOwnerCrash*)
    REQUIRED_SUBTESTS=(TestOwnerCrash/owner_is_leader TestOwnerCrash/owner_is_not_leader)
    REQUIRED_PARENTS=(TestOwnerCrash)
    REQUIRED_RECORD_KEYS=(events owner_is_leader owner_is_not_leader)
    ;;
  *TestTargetedFaults*)
    REQUIRED_SUBTESTS=(
      TestTargetedFaults/event_history_correlation
      TestTargetedFaults/response_loss_possibly_committed
      TestTargetedFaults/external_pause_resume
      TestTargetedFaults/asymmetric_partition
    )
    REQUIRED_PARENTS=(TestTargetedFaults)
    REQUIRED_RECORD_KEYS=(
      fault_events
      event_history_correlation
      response_loss_possibly_committed
      external_pause_resume
      asymmetric_partition
    )
    if [[ -n "$INSTRUMENTED_IMAGE" ]]; then
      # The instrumented image is the ONLY way this subtest can run. Requiring
      # it here means a skip can never stand in for the instrumented result.
      REQUIRED_SUBTESTS+=(TestTargetedFaults/bus_publish_pause)
      REQUIRED_RECORD_KEYS+=(bus_publish_pause)
    fi
    ;;
  *TestCore*)
    REQUIRED_SUBTESTS=(
      TestCore/terminal_no_regress
      TestCore/frozen_retry_recipe
      TestCore/fan_in_predecessors
      TestCore/wrong_token_internal
      TestCore/invalid_mtls_peer
      TestCore/cancel_completion_race
      TestCore/commit_before_response_loss
      TestCore/stale_generation_complete
      TestCore/worker_unreachable_bench
      TestCore/quorum_loss_uncertain_write
    )
    REQUIRED_PARENTS=(TestCore)
    REQUIRED_RECORD_KEYS=(
      core_events
      core_topology
      terminal_no_regress
      frozen_retry_recipe
      fan_in_predecessors
      wrong_token_internal
      invalid_mtls_peer
      cancel_completion_race
      commit_before_response_loss
      stale_generation_complete
      worker_unreachable_bench
      quorum_loss_uncertain_write
    )
    if [[ -n "$INSTRUMENTED_IMAGE" ]]; then
      REQUIRED_SUBTESTS+=(TestCore/durable_event_before_delivery_crash)
      REQUIRED_RECORD_KEYS+=(durable_event_before_delivery_crash)
    fi
    ;;
  *)
    die "CAESIUM_ROBUSTNESS_RUN='$RUN_PATTERN' has no declared required subtests; refusing to run an unchecked selection"
    ;;
esac

if [[ -n "$INSTRUMENTED_IMAGE" ]]; then
  # The instrumented server replaces the release server for THIS run only. The
  # release image is still inspected and checked for the control's markers, so
  # the baseline claim is made against the same artifact the lane ships.
  SERVER_IMAGE="$INSTRUMENTED_IMAGE"
  DEPLOY_TAG="${CANDIDATE_SHA}-testfault"
  CANONICAL_SERVER_IMAGE="caesiumcloud/caesium:${DEPLOY_TAG}"
fi
: >"$OWNED_CLUSTERS"
# Truncate append-only identity files so a reused ARTIFACTS dir cannot mix candidates.
: >"$ARTIFACTS/crictl-inspecti.json"
: >"$ARTIFACTS/ctr-image-info.txt"

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

unrecord_cluster() {
  local name="$1"
  if [[ ! -f "$OWNED_CLUSTERS" ]]; then
    return 0
  fi
  grep -vxF "$name" "$OWNED_CLUSTERS" >"$OWNED_CLUSTERS.tmp" || true
  mv "$OWNED_CLUSTERS.tmp" "$OWNED_CLUSTERS"
}

require_absent_cluster() {
  local name="$1" clusters nodes
  clusters="$(kind get clusters 2>&1)" || die "kind get clusters failed: $clusters"
  if printf '%s\n' "$clusters" | grep -qxF "$name"; then
    die "kind cluster $name already exists; refusing to claim or delete it"
  fi
  nodes="$(docker ps -a --format '{{.Names}}' 2>&1)" || die "docker ps failed: $nodes"
  if printf '%s\n' "$nodes" | grep -qxF "${name}-control-plane"; then
    die "docker container ${name}-control-plane already exists; refusing to claim cluster $name"
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

# Chain name for one partition tag. Derived from the tag exactly as
# test/robustness/faults.PartitionPlan.OpenChain does, so install and heal agree.
open_chain() {
  local tag="$1"
  printf 'CS%s' "$(printf '%s' "$tag" | tr -cd '[:alnum:]' | tr '[:lower:]' '[:upper:]' | cut -c1-18)"
}

# A reverse partition uses the forward tag plus "r". Match the trailing
# delimiter so healing one direction never consumes its sibling's rules.
tagged_rule_lines() {
  local tag="$1"
  grep -F -- "${tag}-"
}

tagged_rule_lines_self_test() {
  local own='-A FORWARD -m comment --comment rb-test-drop-9001 -j DROP'
  local reverse='-A FORWARD -m comment --comment rb-testr-drop-9001 -j DROP'
  [[ "$(printf '%s\n%s\n' "$own" "$reverse" | tagged_rule_lines rb-test)" == "$own" ]] \
    || die "tagged rule matching also selected a reverse partition"
  [[ "$(printf '%s\n%s\n' 'node rb-test' 'node rb-testr' | awk -v tag=rb-test '$2 != tag')" == 'node rb-testr' ]] \
    || die "partition journal removal also selected a reverse partition"
}
tagged_rule_lines_self_test

# Remove exactly the rules carrying this tag, and nothing else: every delete is
# built from the node's own `iptables -S` output.
remove_tagged_rules() {
  local node="$1" tag="$2" chain
  chain="$(open_chain "$tag")"
  local line spec
  while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    spec="${line#-A }"
    # xargs (not eval) so the quoted --comment argument is split correctly and
    # no rule text is ever interpreted as shell.
    printf '%s\n' "$spec" | xargs docker exec "$node" iptables -D >/dev/null 2>&1 || true
  done < <(docker exec "$node" iptables -S FORWARD 2>/dev/null | tagged_rule_lines "$tag" || true)
  while docker exec "$node" iptables -C FORWARD -j "$chain" >/dev/null 2>&1; do
    docker exec "$node" iptables -D FORWARD -j "$chain" >/dev/null 2>&1 || break
  done
  docker exec "$node" iptables -F "$chain" >/dev/null 2>&1 || true
  docker exec "$node" iptables -X "$chain" >/dev/null 2>&1 || true
}

known_node() {
  local n="${1:-}"
  [[ -n "$n" ]] || return 1
  printf '%s\n' "${KIND_NODES[@]}" | grep -qxF "$n"
}

valid_control_dir() {
  printf '%s' "${1:-}" | grep -Eq '^/tmp/[A-Za-z0-9._/-]+$'
}

# Run a short shell command inside a container's own namespaces through the
# node's container runtime. The host controller keeps all node-level access;
# the in-cluster runner never gets exec rights.
CTR_EXEC_OUT=""
ctr_exec_sh() {
  local node="$1" cid="$2" script="$3" rc=0
  CTR_EXEC_OUT="$(docker exec "$node" timeout 30 \
    ctr -n k8s.io tasks exec --exec-id "tf$(date +%s)$RANDOM" "$cid" sh -c "$script" 2>&1)" || rc=$?
  return $rc
}

valid_partition_params() {
  known_node "$p_src_node" || return 1
  known_node "$p_dst_node" || return 1
  [[ "$p_src_node" != "$p_dst_node" ]] || return 1
  printf '%s' "$p_tag" | grep -Eq '^[a-z0-9][a-z0-9-]{2,30}$' || return 1
  printf '%s' "$p_src_ip" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' || return 1
  printf '%s' "$p_dst_ip" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' || return 1
  [[ "$p_src_ip" != "$p_dst_ip" ]] || return 1
  printf '%s' "$p_drop_ports" | grep -Eq '^[0-9]+(,[0-9]+)*$' || return 1
  printf '%s' "$p_count_ports" | grep -Eq '^[0-9]+(,[0-9]+)*$' || return 1
}

install_partition() {
  local chain port
  chain="$(open_chain "$p_tag")"
  # Register for cleanup BEFORE anything is applied, so a crashed controller
  # still heals the cluster it faulted.
  printf '%s %s\n' "$p_src_node" "$p_tag" >>"$PARTITION_FILE"
  printf '%s %s\n' "$p_dst_node" "$p_tag" >>"$PARTITION_FILE"
  sync
  # Blocked direction: one DROP per partitioned port, at the top of FORWARD on
  # the node hosting the source. Each rule's own packet counter is the
  # traversal evidence for that discovered peer route.
  for port in $(printf '%s' "$p_drop_ports" | tr ',' ' '); do
    docker exec "$p_src_node" iptables -I FORWARD 1 \
      -s "$p_src_ip" -d "$p_dst_ip" -p tcp --dport "$port" \
      -m comment --comment "${p_tag}-drop-${port}" -j DROP >/dev/null 2>&1 || return 1
  done
  # Open direction: counted, never altered. A RETURN hands the packet straight
  # back to FORWARD at the rule after the jump.
  docker exec "$p_dst_node" iptables -N "$chain" >/dev/null 2>&1 \
    || docker exec "$p_dst_node" iptables -F "$chain" >/dev/null 2>&1 || return 1
  for port in $(printf '%s' "$p_count_ports" | tr ',' ' '); do
    docker exec "$p_dst_node" iptables -A "$chain" \
      -s "$p_dst_ip" -d "$p_src_ip" -p tcp --dport "$port" \
      -m comment --comment "${p_tag}-open-count-${port}" -j RETURN >/dev/null 2>&1 || return 1
  done
  docker exec "$p_dst_node" iptables -I FORWARD 1 -j "$chain" >/dev/null 2>&1 || return 1
}

# Raw per-rule packet counters from both nodes, in the two sections the runner
# parses. Only this run's tagged rules are emitted, so the evidence stays small
# and unambiguous.
partition_counter_evidence() {
  local chain
  chain="$(open_chain "$p_tag")"
  printf '=== src %s ===\n' "$p_src_node"
  docker exec "$p_src_node" iptables -L FORWARD -v -n -x 2>/dev/null | grep -F -- "$p_tag" || true
  printf '=== dst %s ===\n' "$p_dst_node"
  docker exec "$p_dst_node" iptables -L "$chain" -v -n -x 2>/dev/null | grep -F -- "$p_tag" || true
}

fail_request() {
  local rid="$1" act="$2" msg="$3"
  log "host request $act/$rid failed: $msg"
  write_ack "$rid" "$act" "failed" "" "$msg"
  LAST_REQUEST_ID="$rid"
}

# A frozen container and an installed network rule both outlive a crashed test,
# so every fault this controller applies is registered before it is applied and
# is undone here.
resume_paused_task() {
  [[ -f "$PAUSED_TASK_FILE" ]] || return 0
  local node cid
  read -r node cid <"$PAUSED_TASK_FILE" || return 0
  [[ -n "$node" && -n "$cid" ]] || return 0
  log "cleanup: resuming paused task $cid on $node"
  docker exec "$node" ctr -n k8s.io tasks resume "$cid" >/dev/null 2>&1 || true
  docker exec "$node" systemctl start kubelet >/dev/null 2>&1 || true
  rm -f "$PAUSED_TASK_FILE"
}

heal_installed_partitions() {
  [[ -f "$PARTITION_FILE" ]] || return 0
  local node tag
  while read -r node tag; do
    [[ -n "$node" && -n "$tag" ]] || continue
    log "cleanup: removing network rules $tag from $node"
    remove_tagged_rules "$node" "$tag" >/dev/null 2>&1 || true
  done <"$PARTITION_FILE"
  rm -f "$PARTITION_FILE"
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
  heal_installed_partitions
  resume_paused_task
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
docker image inspect "$RELEASE_IMAGE" >/dev/null || die "SERVER_IMAGE not present locally: $RELEASE_IMAGE"
docker image inspect "$SERVER_IMAGE" >/dev/null || die "server image not present locally: $SERVER_IMAGE"
docker image inspect "$RUNNER_IMAGE" >/dev/null || die "RUNNER_IMAGE not present locally: $RUNNER_IMAGE"
docker image inspect "$TASK_IMAGE" >/dev/null || die "TASK_IMAGE not present locally: $TASK_IMAGE"

# ---------------------------------------------------------------------------
# Release baseline: the test-only fault control must be COMPILE-TIME absent.
#
# internal/testfault's release twin has Enabled=false as an untyped constant, so
# every guarded call site is removed before linking and none of the control's
# strings can reach the binary. That is a claim about a build, so it is checked
# against the built artifact on every run rather than trusted.
# ---------------------------------------------------------------------------
extract_binary() {
  local image="$1" dest="$2" cid
  cid="$(docker create "$image" true)" || return 1
  docker cp "$cid":/bin/caesium "$dest" >/dev/null 2>&1
  local rc=$?
  docker rm -f "$cid" >/dev/null 2>&1 || true
  return $rc
}

marker_hits() {
  local file="$1" marker="$2" n
  n="$(grep -ac -- "$marker" "$file" 2>/dev/null || true)"
  printf '%s' "${n:-0}"
}

BIN_CHECK="$ARTIFACTS/.binary-under-check"
MARKER_REPORT="$ARTIFACTS/testfault-marker-scan.txt"
: >"$MARKER_REPORT"

extract_binary "$RELEASE_IMAGE" "$BIN_CHECK" || die "could not extract /bin/caesium from $RELEASE_IMAGE for the release baseline check"
for marker in "${TESTFAULT_MARKERS[@]}"; do
  hits="$(marker_hits "$BIN_CHECK" "$marker")"
  printf 'release %s marker=%s hits=%s\n' "$RELEASE_IMAGE" "$marker" "$hits" >>"$MARKER_REPORT"
  [[ "$hits" == "0" ]] \
    || die "release image $RELEASE_IMAGE contains the test-only fault-control marker '$marker' ($hits matching lines); the control must be compiled out of shippable artifacts"
done
hits="$(marker_hits "$BIN_CHECK" "testfault")"
printf 'release %s marker=%s hits=%s\n' "$RELEASE_IMAGE" "testfault" "$hits" >>"$MARKER_REPORT"
[[ "$hits" == "0" ]] || die "release image $RELEASE_IMAGE still references 'testfault' ($hits matching lines)"
rm -f "$BIN_CHECK"
log "release baseline: $RELEASE_IMAGE contains no test-only fault-control symbol or string"

if [[ -n "$INSTRUMENTED_IMAGE" ]]; then
  # Symmetric check: a build tag that silently failed to apply would make every
  # instrumented assertion vacuous.
  extract_binary "$INSTRUMENTED_IMAGE" "$BIN_CHECK" || die "could not extract /bin/caesium from $INSTRUMENTED_IMAGE"
  for marker in "${TESTFAULT_MARKERS[@]}"; do
    hits="$(marker_hits "$BIN_CHECK" "$marker")"
    printf 'instrumented %s marker=%s hits=%s\n' "$INSTRUMENTED_IMAGE" "$marker" "$hits" >>"$MARKER_REPORT"
    [[ "$hits" != "0" ]] \
      || die "instrumented image $INSTRUMENTED_IMAGE is missing the fault-control marker '$marker'; it was not built with the testfault tag"
  done
  rm -f "$BIN_CHECK"
  log "instrumented image $INSTRUMENTED_IMAGE carries the test-only fault control"
fi

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

HOST_IMAGE_ID="$(docker image inspect --format '{{.Id}}' "$SERVER_IMAGE")"
CANDIDATE_DIGEST="$HOST_IMAGE_ID"
log "candidate_digest=$CANDIDATE_DIGEST"
printf '%s\n' "$CANDIDATE_DIGEST" >"$ARTIFACTS/candidate-digest.txt"
printf '%s\n' "$HOST_IMAGE_ID" >"$ARTIFACTS/host-image-id.txt"
docker image inspect "$KIND_IMAGE" >"$ARTIFACTS/kind-image.json"
docker image inspect "$SERVER_IMAGE" >"$ARTIFACTS/server-image.json"
docker image inspect "$RUNNER_IMAGE" >"$ARTIFACTS/runner-image.json"
docker image inspect "$TASK_IMAGE" >"$ARTIFACTS/task-image.json"

if [[ "$SERVER_IMAGE" != "$CANONICAL_SERVER_IMAGE" ]]; then
  log "tagging $SERVER_IMAGE as $CANONICAL_SERVER_IMAGE for helm image.tag=$DEPLOY_TAG"
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
[[ "$ISO_NAME" != "$ROBUSTNESS_ID" ]] || die "isolation cluster name collided with $ROBUSTNESS_ID"
require_absent_cluster "$ISO_NAME"
require_absent_cluster "$ROBUSTNESS_ID"
# Record only after absence is confirmed so a pre-existing cluster is never
# owned or deleted. A create that fails mid-way is this invocation's cluster.
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
  unrecord_cluster "$ISO_NAME"
  rm -f "$ISO_KUBECONFIG"
else
  die "failed to delete isolation cluster $ISO_NAME"
fi

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
    # B2 needs external network fault controls and an external process freeze.
    # Diagnose both on every worker BEFORE anything is faulted, exactly as B1
    # does for systemctl/ctr: a missing tool must be a clear refusal, never a
    # fault that silently did nothing.
    docker exec "$node" sh -c 'command -v iptables >/dev/null' \
      || die "iptables unavailable on $node; external network fault controls cannot be applied (KIND_IMAGE=$KIND_IMAGE)"
    docker exec "$node" iptables -S FORWARD >>"$ARTIFACTS/node-tools-$node.txt" 2>&1 \
      || die "iptables cannot read the FORWARD chain on $node"
    probe_chain="CSPROBE$$"
    docker exec "$node" iptables -N "$probe_chain" >/dev/null 2>&1 || die "cannot create an iptables user chain on $node"
    if ! docker exec "$node" iptables -A "$probe_chain" -p tcp --dport 9999 -m comment --comment "caesium-robustness-probe" -j RETURN >/dev/null 2>&1; then
      docker exec "$node" iptables -X "$probe_chain" >/dev/null 2>&1 || true
      die "iptables on $node rejects a commented counting rule; partition evidence would be unobtainable"
    fi
    docker exec "$node" iptables -F "$probe_chain" >/dev/null 2>&1 || true
    docker exec "$node" iptables -X "$probe_chain" >/dev/null 2>&1 || true
    docker exec "$node" ctr -n k8s.io tasks pause --help >/dev/null 2>&1 \
      || log "note: 'ctr tasks pause --help' returned non-zero on $node; pause is still attempted and verified from the task listing"
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

imported_identities() {
  local node="$1" needle="$2"
  docker exec "$node" ctr -n k8s.io images ls | python3 "$HOSTLOGIC" ctr-image-shas "$needle"
}

WORKER_NODE=""
for n in "${KIND_NODES[@]}"; do
  if [[ "$n" != *control-plane* ]]; then
    WORKER_NODE="$n"
    break
  fi
done
[[ -n "$WORKER_NODE" ]] || die "no kind worker for imported digest"
docker exec "$WORKER_NODE" ctr -n k8s.io images ls >"$ARTIFACTS/ctr-images-ls.txt" 2>/dev/null || true
python3 "$HOSTLOGIC" ctr-image-shas "caesiumcloud/caesium:${DEPLOY_TAG}" \
  <"$ARTIFACTS/ctr-images-ls.txt" >"$ARTIFACTS/imported-digest.txt" \
  || imported_identities "$WORKER_NODE" "caesiumcloud/caesium:${DEPLOY_TAG}" >"$ARTIFACTS/imported-digest.txt"
: >"$ARTIFACTS/ctr-image-info.txt"
for ref in "caesiumcloud/caesium:${DEPLOY_TAG}" "docker.io/caesiumcloud/caesium:${DEPLOY_TAG}"; do
  docker exec "$WORKER_NODE" ctr -n k8s.io images info "$ref" >>"$ARTIFACTS/ctr-image-info.txt" 2>/dev/null || true
done
python3 - "$WORKER_NODE" "$ARTIFACTS/crictl-inspecti.json" \
  "caesiumcloud/caesium:${DEPLOY_TAG}" \
  "docker.io/caesiumcloud/caesium:${DEPLOY_TAG}" <<'PY'
import json, subprocess, sys

node, dest, *refs = sys.argv[1:]
docs = []
for ref in refs:
    try:
        raw = subprocess.check_output(
            ["docker", "exec", node, "crictl", "inspecti", ref],
            stderr=subprocess.DEVNULL,
            text=True,
        )
    except (subprocess.CalledProcessError, FileNotFoundError):
        continue
    raw = raw.strip()
    if not raw:
        continue
    try:
        parsed = json.loads(raw)
    except json.JSONDecodeError:
        continue
    if isinstance(parsed, list):
        docs.extend(parsed)
    else:
        docs.append(parsed)
with open(dest, "w", encoding="utf-8") as fh:
    json.dump(docs, fh, indent=2)
    fh.write("\n")
PY
python3 "$HOSTLOGIC" collect-identities \
  "$ARTIFACTS/server-image.json" \
  "$ARTIFACTS/imported-digest.txt" \
  "$ARTIFACTS/crictl-inspecti.json" \
  "$ARTIFACTS/host-image-id.txt" \
  >"$ARTIFACTS/candidate-identities.txt" \
  || die "failed to collect candidate image identities"
IMPORTED_DIGEST="$(head -n1 "$ARTIFACTS/imported-digest.txt")"
log "host_image_id=$HOST_IMAGE_ID imported_digest=$IMPORTED_DIGEST identities=$(tr '\n' ',' <"$ARTIFACTS/candidate-identities.txt" | sed 's/,$//')"
[[ -s "$ARTIFACTS/candidate-identities.txt" ]] || die "candidate identity set is empty"

TOKEN="$(python3 -c 'import secrets; print(secrets.token_urlsafe(48))')"
(( ${#TOKEN} >= 32 )) || die "generated internal token is shorter than 32 bytes"
printf '%s\n' "$TOKEN" >"$ARTIFACTS/internal-token.txt"

HELM_EXTRA=()
if [[ -n "$INSTRUMENTED_IMAGE" ]]; then
  # Append CAESIUM_TESTFAULT_DIR after the values file's own extraEnv entries.
  # Helm --set addresses list elements by index, so the index is counted from
  # the values file and then VERIFIED with `helm template` below: a miscount
  # would leave a null hole and must fail loudly, not deploy an inert server.
  NEXT_ENV_INDEX="$(python3 - "$VALUES" <<'PY'
import re, sys
lines = open(sys.argv[1], encoding="utf-8").read().splitlines()
count, inside, indent = 0, False, None
for line in lines:
    if re.match(r"^\s*extraEnv:\s*$", line):
        inside = True
        continue
    if not inside:
        continue
    if not line.strip():
        continue
    m = re.match(r"^(\s*)- name:", line)
    if m:
        if indent is None:
            indent = len(m.group(1))
        if len(m.group(1)) == indent:
            count += 1
        continue
    if indent is not None and len(line) - len(line.lstrip()) <= indent and not line.lstrip().startswith(("value", "-")):
        break
print(count)
PY
)"
  [[ "$NEXT_ENV_INDEX" =~ ^[0-9]+$ && "$NEXT_ENV_INDEX" -gt 0 ]] \
    || die "could not count config.extraEnv entries in $VALUES (got '$NEXT_ENV_INDEX')"
  HELM_EXTRA+=(
    --set "config.extraEnv[${NEXT_ENV_INDEX}].name=CAESIUM_TESTFAULT_DIR"
    --set-string "config.extraEnv[${NEXT_ENV_INDEX}].value=$TESTFAULT_DIR"
  )
  log "instrumented deploy: CAESIUM_TESTFAULT_DIR=$TESTFAULT_DIR at config.extraEnv[$NEXT_ENV_INDEX]"
  rendered="$(helm template caesium "$ROOT/helm/caesium" \
    --namespace "$NAMESPACE" --values "$VALUES" \
    --set image.tag="$DEPLOY_TAG" \
    --set config.extraEnv[0].name=CAESIUM_INTERNAL_WAKEUP_TOKEN \
    --set-string config.extraEnv[0].value=verify-only \
    "${HELM_EXTRA[@]}" 2>&1)" \
    || die "helm template with the testfault env failed: $rendered"
  for want in CAESIUM_INTERNAL_WAKEUP_TOKEN CAESIUM_TESTFAULT_DIR CAESIUM_EXECUTION_MODE; do
    printf '%s\n' "$rendered" | grep -q "$want" \
      || die "rendered StatefulSet is missing $want; the extraEnv index is wrong"
  done
fi

log "helm install caesium into namespace $NAMESPACE"
helm install caesium "$ROOT/helm/caesium" \
  --kubeconfig "$KUBECONFIG_PATH" \
  --namespace "$NAMESPACE" \
  --create-namespace \
  --values "$VALUES" \
  --set image.tag="$DEPLOY_TAG" \
  --set config.extraEnv[0].name=CAESIUM_INTERNAL_WAKEUP_TOKEN \
  --set-string config.extraEnv[0].value="$TOKEN" \
  "${HELM_EXTRA[@]+"${HELM_EXTRA[@]}"}" \
  --wait --timeout 240s

log "verifying three bound PVCs, distinct members, and candidate image IDs"
EXPECTED_IDENTITIES="$(paste -sd, "$ARTIFACTS/candidate-identities.txt")"
RUNNING_DIGEST="$(python3 - "$KUBECONFIG_PATH" "$NAMESPACE" "$DEPLOY_TAG,$SERVER_IMAGE" "$EXPECTED_IDENTITIES" <<'PY'
import json, re, subprocess, sys

kube, ns, tag, expected = sys.argv[1:]
# `tag` is a comma-separated list of acceptable image-name fragments. A loaded
# image can carry more than one name in containerd (the canonical
# caesiumcloud/caesium:<tag> alias plus the reference it was built under), and
# the kubelet reports whichever one it resolved. The authoritative check remains
# the imageID digest comparison below; this one only rejects an image that is
# not this candidate at all.
name_fragments = [t for t in (tag or "").split(",") if t.strip()]
want = {"sha256:" + m.group(1).lower() for m in re.finditer(r"sha256:([0-9a-fA-F]{64})", expected)}
if not want:
    raise SystemExit("expected candidate identities are empty")

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
    if not any(fragment in image for fragment in name_fragments):
        raise SystemExit(f"pod {p['metadata']['name']} image {image} does not use any candidate name {name_fragments}")
    image_id = cs.get("imageID") or ""
    got = {"sha256:" + m.group(1).lower() for m in re.finditer(r"sha256:([0-9a-fA-F]{64})", image_id)}
    if not got:
        raise SystemExit(f"pod {p['metadata']['name']} missing resolved imageID ({image_id})")
    if not (got & want):
        raise SystemExit(f"pod {p['metadata']['name']} imageID {image_id} does not match loaded candidate {sorted(want)}")
    uids.add(uid); ips.add(ip); nodes.add(node); digests.add(image_id)

if len(uids) != 3 or len(ips) != 3 or len(nodes) != 3:
    raise SystemExit(f"members not distinct uids={len(uids)} ips={len(ips)} nodes={len(nodes)}")
if len(digests) != 1:
    raise SystemExit(f"caesium pods are not running one image: {sorted(digests)}")
print(next(iter(digests)))
PY
)"
[[ "$RUNNING_DIGEST" == *sha256:* ]] || die "topology verification did not return a running digest ($RUNNING_DIGEST)"
log "running_image_id=$RUNNING_DIGEST expected_identities=$EXPECTED_IDENTITIES"
python3 "$HOSTLOGIC" image-match "$ARTIFACTS/candidate-identities.txt" "$RUNNING_DIGEST" \
  || die "running image identity does not match loaded candidate"
printf '%s\n' "$RUNNING_DIGEST" >"$ARTIFACTS/running-image-id.txt"
# Keep the expected identity set. Do not redefine the candidate as the observed running ID.
CANDIDATE_DIGEST="$EXPECTED_IDENTITIES"

for i in 0 1 2; do
  kc_ns exec "caesium-$i" -c caesium -- sh -c 'printenv | grep ^CAESIUM_ | sort' \
    >"$ARTIFACTS/caesium-$i.env" || true
done

log "deploying in-cluster runner on the control-plane node"
RUNNER_INSTRUMENTED=0
RUNNER_TIMEOUT=15m
if [[ -n "$INSTRUMENTED_IMAGE" ]]; then
  RUNNER_INSTRUMENTED=1
fi
case "$RUN_PATTERN" in
  # The fault suite freezes a member past a 30s lease and holds a partition, so
  # it needs more wall clock than the owner-crash regression.
  *TestTargetedFaults*) RUNNER_TIMEOUT=35m ;;
  # B3 composes those controls into recovery/auth/dispatch cases, including a
  # pause past the 30s lease and a 2-1 split, so it needs a longer budget.
  *TestCore*) RUNNER_TIMEOUT=70m ;;
esac
log "runner selection: -test.run '$RUN_PATTERN' -test.timeout $RUNNER_TIMEOUT instrumented=$RUNNER_INSTRUMENTED"
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
      args: ["-test.v", "-test.count=1", "-test.run", "${RUN_PATTERN}", "-test.timeout", "${RUNNER_TIMEOUT}"]
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
          value: "${CANDIDATE_DIGEST}"
        - name: CAESIUM_MANUAL_TRIGGER_API_KEY
          value: caesium-robustness-manual-key
        - name: CAESIUM_ROBUSTNESS_INSTRUMENTED
          value: "${RUNNER_INSTRUMENTED}"
        - name: CAESIUM_ROBUSTNESS_TESTFAULT_DIR
          value: "${TESTFAULT_DIR}"
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

listing_valid() {
  python3 "$HOSTLOGIC" listing-valid <<<"$1"
}

task_dead() {
  local cid="$1" listing="$2" seen="$3"
  python3 "$HOSTLOGIC" task-dead "$cid" "$seen" <<<"$listing"
}

handle_host_request() {
  local action request_id node cid pod evidence listing reqjson
  local p_node p_container_id p_target p_timeout_s p_tag p_src_node p_dst_node
  local p_src_ip p_dst_ip p_drop_ports p_count_ports p_dir p_directive
  reqjson="$(kc_ns get configmap robustness-host-request -o json 2>/dev/null || true)"
  [[ -n "$reqjson" ]] || return 0
  eval "$(REQUEST_JSON="$reqjson" python3 - <<'PY'
import json, os, shlex
cm = json.loads(os.environ["REQUEST_JSON"])
data = cm.get("data") or {}
payload = data.get("payload") or "{}"
try:
    obj = json.loads(payload)
except Exception:
    obj = {}
params = obj.get("params") or {}
if not isinstance(params, dict):
    params = {}
if not params:
    try:
        params = json.loads(data.get("params") or "{}")
    except Exception:
        params = {}
def pick(*keys):
    for k in keys:
        v = obj.get(k) or data.get(k) or ""
        if v:
            return str(v)
    return ""
print("request_id="+shlex.quote(pick("request_id")))
print("action="+shlex.quote(pick("action")))
print("node="+shlex.quote(pick("owner_kind_node")))
print("cid="+shlex.quote(pick("owner_container_id")))
print("pod="+shlex.quote(pick("owner_pod")))
# B2 action parameters. Only a fixed key set is exported, and every value is
# shell-quoted, so a malformed request can never become a host command.
for key in ("node", "container_id", "target", "timeout_s", "tag", "src_node",
            "dst_node", "src_ip", "dst_ip", "drop_ports", "count_ports",
            "dir", "directive"):
    print("p_"+key+"="+shlex.quote(str(params.get(key) or "")))
PY
)"
  [[ -n "$action" && -n "$request_id" ]] || return 0
  [[ "$request_id" != "$LAST_REQUEST_ID" ]] || return 0
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
      before=""
      before_rc=0
      before="$(docker exec "$node" ctr -n k8s.io tasks list 2>&1)" || before_rc=$?
      printf '%s\n' "$before" >"$ARTIFACTS/ctr-tasks-before-kill.txt"
      if [[ "$before_rc" -ne 0 ]] || ! listing_valid "$before"; then
        write_ack "$request_id" "$action" "failed" "$before" "ctr tasks list before kill failed (rc=$before_rc)"
        LAST_REQUEST_ID="$request_id"
        return 0
      fi
      if [[ "$before" == *"$cid"* || "$before" == *"$short_cid"* ]]; then
        seen_cid=1
      else
        write_ack "$request_id" "$action" "failed" "$before" "owner container $cid not in ctr tasks list before kill"
        LAST_REQUEST_ID="$request_id"
        return 0
      fi
      last_list_err=""
      for _try in $(seq 1 20); do
        kill_rc=0
        docker exec "$node" ctr -n k8s.io tasks kill --signal SIGKILL "$cid" >"$ARTIFACTS/ctr-kill-$cid.txt" 2>&1 || kill_rc=$?
        list_rc=0
        listing="$(docker exec "$node" ctr -n k8s.io tasks list 2>&1)" || list_rc=$?
        printf '%s\n' "$listing" >"$ARTIFACTS/ctr-tasks-after-kill.txt"
        if [[ "$list_rc" -ne 0 ]] || ! listing_valid "$listing"; then
          last_list_err="ctr tasks list after kill failed (rc=$list_rc)"
          sleep 1
          continue
        fi
        if task_dead "$cid" "$listing" "$seen_cid"; then
          killed=1
          break
        fi
        last_list_err="process still running (kill_rc=$kill_rc)"
        sleep 1
      done
      evidence="$(printf 'kubelet stopped on %s\nctr kill %s\n%s\n' "$node" "$cid" "$listing")"
      if [[ "$killed" -ne 1 ]]; then
        write_ack "$request_id" "$action" "failed" "$evidence" "${last_list_err:-process still running}"
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
    task-state)
      if ! known_node "$p_node"; then
        fail_request "$request_id" "$action" "node '$p_node' is not in cluster $ROBUSTNESS_ID"; return 0
      fi
      listing="$(docker exec "$p_node" ctr -n k8s.io tasks list 2>&1)" || true
      write_ack "$request_id" "$action" "ok" "$listing"
      ;;
    pause)
      # Freeze the whole container through the runtime's cgroup freezer, and
      # stop kubelet first so a liveness probe cannot restart the frozen
      # container and turn "resume" into "replace". Registered for cleanup
      # BEFORE it is applied.
      if ! known_node "$p_node" || [[ -z "$p_container_id" ]]; then
        fail_request "$request_id" "$action" "pause needs a known node and container id (node='$p_node')"; return 0
      fi
      printf '%s %s\n' "$p_node" "$p_container_id" >"$PAUSED_TASK_FILE"
      printf '%s\n' "$p_node" >"$FAULTED_NODE_FILE"
      sync
      if ! docker exec "$p_node" systemctl stop kubelet >/dev/null 2>&1; then
        fail_request "$request_id" "$action" "systemctl stop kubelet failed on $p_node"; return 0
      fi
      if ! docker exec "$p_node" ctr -n k8s.io tasks pause "$p_container_id" >"$ARTIFACTS/ctr-pause-$p_container_id.txt" 2>&1; then
        evidence="$(cat "$ARTIFACTS/ctr-pause-$p_container_id.txt" 2>/dev/null || true)"
        fail_request "$request_id" "$action" "ctr tasks pause failed on $p_node: $evidence"; return 0
      fi
      listing="$(docker exec "$p_node" ctr -n k8s.io tasks list 2>&1)" || true
      printf '%s\n' "$listing" >"$ARTIFACTS/ctr-tasks-paused.txt"
      write_ack "$request_id" "$action" "ok" "$listing"
      ;;
    resume)
      if ! known_node "$p_node" || [[ -z "$p_container_id" ]]; then
        fail_request "$request_id" "$action" "resume needs a known node and container id"; return 0
      fi
      docker exec "$p_node" ctr -n k8s.io tasks resume "$p_container_id" >"$ARTIFACTS/ctr-resume-$p_container_id.txt" 2>&1 || true
      listing="$(docker exec "$p_node" ctr -n k8s.io tasks list 2>&1)" || true
      printf '%s\n' "$listing" >"$ARTIFACTS/ctr-tasks-resumed.txt"
      docker exec "$p_node" systemctl start kubelet >/dev/null 2>&1 || true
      kc uncordon "$p_node" >/dev/null 2>&1 || true
      rm -f "$PAUSED_TASK_FILE" "$FAULTED_NODE_FILE"
      write_ack "$request_id" "$action" "ok" "$listing"
      ;;
    exec-probe)
      # Run the reachability probe INSIDE the member's own namespaces, so the
      # connection really originates from the partitioned peer.
      if ! known_node "$p_node" || [[ -z "$p_container_id" ]]; then
        fail_request "$request_id" "$action" "exec-probe needs a known node and container id"; return 0
      fi
      if ! printf '%s' "$p_target" | grep -Eq '^http://[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+:[0-9]+/[A-Za-z0-9/_.-]*$'; then
        fail_request "$request_id" "$action" "refusing probe target '$p_target'"; return 0
      fi
      probe_timeout="${p_timeout_s:-4}"
      printf '%s' "$probe_timeout" | grep -Eq '^[0-9]{1,3}$' || probe_timeout=4
      probe_out="$(docker exec "$p_node" timeout $((probe_timeout + 12)) \
        ctr -n k8s.io tasks exec --exec-id "probe$(date +%s%N)" "$p_container_id" \
        wget -T "$probe_timeout" -q -O - "$p_target" 2>&1)" && probe_rc=0 || probe_rc=$?
      write_ack "$request_id" "$action" "ok" "$(printf 'rc=%s\n%s' "$probe_rc" "$probe_out")"
      ;;
    partition)
      if ! valid_partition_params; then
        fail_request "$request_id" "$action" "invalid partition parameters"; return 0
      fi
      if ! install_partition; then
        fail_request "$request_id" "$action" "failed to install network rules for $p_tag"; return 0
      fi
      write_ack "$request_id" "$action" "ok" "$(partition_counter_evidence)"
      ;;
    counters)
      if ! valid_partition_params; then
        fail_request "$request_id" "$action" "invalid partition parameters"; return 0
      fi
      write_ack "$request_id" "$action" "ok" "$(partition_counter_evidence)"
      ;;
    heal)
      if ! valid_partition_params; then
        fail_request "$request_id" "$action" "invalid partition parameters"; return 0
      fi
      evidence="$(partition_counter_evidence)"
      remove_tagged_rules "$p_src_node" "$p_tag"
      remove_tagged_rules "$p_dst_node" "$p_tag"
      if [[ -f "$PARTITION_FILE" ]]; then
        awk -v tag="$p_tag" '$2 != tag' "$PARTITION_FILE" >"$PARTITION_FILE.tmp" 2>/dev/null || true
        mv "$PARTITION_FILE.tmp" "$PARTITION_FILE" 2>/dev/null || true
      fi
      remaining="$( { docker exec "$p_src_node" iptables -S 2>/dev/null; docker exec "$p_dst_node" iptables -S 2>/dev/null; } | tagged_rule_lines "$p_tag" || true)"
      if [[ -n "$remaining" ]]; then
        fail_request "$request_id" "$action" "rules tagged $p_tag survived heal: $remaining"; return 0
      fi
      write_ack "$request_id" "$action" "ok" "$(printf '%s\n=== healed ===\nno rules tagged %s remain on %s or %s\n' "$evidence" "$p_tag" "$p_src_node" "$p_dst_node")"
      ;;
    testfault-arm)
      if ! known_node "$p_node" || [[ -z "$p_container_id" ]] || ! valid_control_dir "$p_dir"; then
        fail_request "$request_id" "$action" "arm needs a known node, container id and control dir (dir='$p_dir')"; return 0
      fi
      if ! printf '%s' "$p_directive" | python3 -c 'import json,sys; json.loads(sys.stdin.read())' >/dev/null 2>&1; then
        fail_request "$request_id" "$action" "directive is not JSON"; return 0
      fi
      case "$p_directive" in
        *"'"*) fail_request "$request_id" "$action" "directive contains a quote that would break the in-container write"; return 0 ;;
      esac
      if ! ctr_exec_sh "$p_node" "$p_container_id" \
        "mkdir -p '$p_dir' && printf '%s' '$p_directive' > '$p_dir/bus-publish-pause.json' && ls -l '$p_dir'"; then
        fail_request "$request_id" "$action" "could not write the directive into $p_container_id"; return 0
      fi
      write_ack "$request_id" "$action" "ok" "$CTR_EXEC_OUT"
      ;;
    testfault-disarm)
      if ! known_node "$p_node" || [[ -z "$p_container_id" ]] || ! valid_control_dir "$p_dir"; then
        fail_request "$request_id" "$action" "disarm needs a known node, container id and control dir"; return 0
      fi
      ctr_exec_sh "$p_node" "$p_container_id" "rm -f '$p_dir/bus-publish-pause.json'; ls -a '$p_dir' 2>/dev/null || true" || true
      write_ack "$request_id" "$action" "ok" "$CTR_EXEC_OUT"
      ;;
    testfault-log)
      if ! known_node "$p_node" || [[ -z "$p_container_id" ]] || ! valid_control_dir "$p_dir"; then
        fail_request "$request_id" "$action" "log read needs a known node, container id and control dir"; return 0
      fi
      # A missing or unreadable log is reported as a FAILURE, never acked as an
      # empty read: an empty ack is indistinguishable from "this member never
      # entered the hook", which would let vanished evidence pass for a member
      # that had nothing to release.
      if ! ctr_exec_sh "$p_node" "$p_container_id" "cat '$p_dir/bus-publish-hook.log'"; then
        fail_request "$request_id" "$action" "hook log unreadable on $p_node/$p_container_id: $CTR_EXEC_OUT"; return 0
      fi
      write_ack "$request_id" "$action" "ok" "$CTR_EXEC_OUT"
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

CONTROLLER_BUDGET=1080
case "$RUNNER_TIMEOUT" in
  35m) CONTROLLER_BUDGET=2400 ;;
  70m) CONTROLLER_BUDGET=4800 ;;
esac
log "host controller waiting for runner ($RUNNER_TIMEOUT test + margin, budget ${CONTROLLER_BUDGET}s)"
DEADLINE=$((SECONDS + CONTROLLER_BUDGET))
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

# Go indents subtest PASS lines; BSD grep also treats a leading --- pattern as options.
pass_line() {
  grep -e "--- PASS: $1" "$ARTIFACTS/robustness.test.log" >/dev/null
}
for name in "${REQUIRED_SUBTESTS[@]}"; do
  pass_line "$name" || die "required subtest $name did not PASS"
done
for name in "${REQUIRED_PARENTS[@]}"; do
  pass_line "$name " || die "$name parent did not PASS"
done
# A skipped subtest is never a result. The instrumented selection requires
# bus_publish_pause above, so its skip branch cannot substitute for it.
if grep -e "--- SKIP" "$ARTIFACTS/robustness.test.log" >/dev/null 2>&1; then
  log "note: the runner reported skipped subtests:"
  grep -e "--- SKIP" "$ARTIFACTS/robustness.test.log" | tee -a "$ARTIFACTS/skipped-subtests.txt"
  for name in "${REQUIRED_SUBTESTS[@]}"; do
    if grep -e "--- SKIP: $name" "$ARTIFACTS/robustness.test.log" >/dev/null 2>&1; then
      die "required subtest $name was SKIPPED"
    fi
  done
fi

kc_ns get cm robustness-records -o json >"$ARTIFACTS/robustness-records.json" \
  || die "inconclusive: failed to export robustness-records ConfigMap"
kc_ns get cm robustness-records -o yaml >"$ARTIFACTS/robustness-records.yaml" \
  || die "inconclusive: failed to export robustness-records.yaml"
python3 "$HOSTLOGIC" records-ok "$ARTIFACTS/robustness-records.json" "${REQUIRED_RECORD_KEYS[@]}" \
  || die "inconclusive: exported recorder artifacts are missing or unreadable"
python3 - "$ARTIFACTS/robustness-records.json" "$ARTIFACTS" "${REQUIRED_RECORD_KEYS[@]}" <<'PY'
import json, pathlib, sys
cm = json.loads(pathlib.Path(sys.argv[1]).read_text())
out = pathlib.Path(sys.argv[2]) / "records"
out.mkdir(exist_ok=True)
data = cm.get("data") or {}
for key in sys.argv[3:]:
    raw = data.get(key)
    if not raw:
        raise SystemExit(f"missing {key}")
    json.loads(raw)
    (out / f"{key}.json").write_text(raw if raw.endswith("\n") else raw + "\n")
PY

log "B1 robustness passed; artifacts in $ARTIFACTS"
