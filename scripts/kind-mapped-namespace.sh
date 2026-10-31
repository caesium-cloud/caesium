#!/usr/bin/env bash
# The kind lane's mapped-namespace phase (identity-and-access C3).
#
# On the helm-integration-test kind cluster, after the policy-free suite:
#   - pre-create the mapped Kubernetes namespace, its service account and a
#     secret, an unmapped namespace for the RBAC negative, and the policy
#     ConfigMap (the chart creates none of them);
#   - install release `c3` in caesium-c3 from test-values-k8s.yaml +
#     test-values-k8s-mapped.yaml, pinned to the CI candidate image;
#   - serve it on 127.0.0.1:8080 through a self-healing port-forward (the
#     scenario restarts the server pod);
#   - run TestKubernetesMappedNamespace in the integration runner and require
#     it to PASS, never SKIP.
#
# Usage: scripts/kind-mapped-namespace.sh <image-tag>   (e.g. "$IMAGE_TAG-amd64")
set -euo pipefail

tag="${1:?usage: kind-mapped-namespace.sh <image-tag>}"
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
release_ns=caesium-c3
mapped_ns=caesium-mapped
# Local runs (not CI) may name another kind cluster, a compiling runner image
# and the address the runner reaches the forwarded server on.
cluster="${CAESIUM_KIND_CLUSTER:-kind}"
kind_node="$cluster-control-plane"
runner_image="${CAESIUM_MAPPED_RUNNER_IMAGE:-caesiumcloud/caesium-integration:$tag}"
server_host="${CAESIUM_MAPPED_SERVER_HOST:-127.0.0.1}"
log_dir="${CAESIUM_MAPPED_LOG_DIR:-/tmp}"
mkdir -p .tmp
scenario_log=.tmp/kind-mapped-namespace.log

kubectl create namespace "$release_ns"
kubectl create namespace "$mapped_ns"
kubectl create namespace caesium-unmapped
kubectl -n "$mapped_ns" create serviceaccount caesium-mapped-runner
kubectl -n "$mapped_ns" create secret generic c3-token --from-literal=value=c3-mapped-token
kubectl -n "$release_ns" create configmap access-policy \
  --from-file=policy.yaml=test/testdata/policy/kubernetes-mapped.yaml

if ! helm install c3 ./helm/caesium --namespace "$release_ns" \
  --values ./helm/caesium/ci/test-values-k8s.yaml \
  --values ./helm/caesium/ci/test-values-k8s-mapped.yaml \
  --set image.tag="$tag" \
  --wait --timeout 300s; then
  kubectl -n "$release_ns" get pods,pvc -o wide || true
  kubectl -n "$release_ns" describe pods || true
  kubectl -n "$release_ns" logs statefulset/c3-caesium -c caesium || true
  exit 1
fi

# The scenario deletes and crashes the server pod; keep 8080 served across both.
(
  while true; do
    kubectl -n "$release_ns" port-forward service/c3-caesium 8080:8080 >>"$log_dir/port-forward-mapped.log" 2>&1 || true
    sleep 1
  done
) &
forward_pid=$!
trap 'kill "$forward_pid" 2>/dev/null || true; pkill -f "port-forward service/c3-caesium" 2>/dev/null || true' EXIT

ready=false
for _ in $(seq 1 60); do
  if curl -sf http://127.0.0.1:8080/health >/dev/null; then ready=true; break; fi
  sleep 1
done
if [ "$ready" != true ]; then
  cat "$log_dir/port-forward-mapped.log" || true
  exit 1
fi

kind get kubeconfig --name "$cluster" > .tmp/mapped-kubeconfig
chmod 600 .tmp/mapped-kubeconfig
# The hold witness listens on IPv4 in the runner; pods reach it through the
# kind bridge's IPv4 gateway.
network_config=$(docker network inspect kind --format '{{json .IPAM.Config}}')
witness_host=$(python3 -c '
import ipaddress, json, sys
for config in json.load(sys.stdin):
    gateway = config.get("Gateway")
    if gateway and ipaddress.ip_address(gateway).version == 4:
        print(gateway)
        break
' <<< "$network_config")
if [ -z "$witness_host" ]; then
  echo "kind network has no IPv4 gateway for the hold witness: $network_config" >&2
  exit 1
fi

docker run --rm \
  -v "$root":/bld/caesium \
  -v "$root/.tmp/mapped-kubeconfig":/tmp/mapped-kubeconfig:ro \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e DOCKER_HOST=unix:///var/run/docker.sock \
  -e KUBECONFIG=/tmp/mapped-kubeconfig \
  -e CAESIUM_TEST_KUBE_MAPPED=1 \
  -e CAESIUM_TEST_ENGINE=kubernetes \
  -e CAESIUM_EXECUTION_MODE=distributed \
  -e CAESIUM_TEST_WITNESS_HOST="$witness_host" \
  -e CAESIUM_TEST_KIND_NODE="$kind_node" \
  -e CAESIUM_TEST_EXPECTED_IMAGE="caesiumcloud/caesium:$tag" \
  -e CAESIUM_CLI_PATH=/bld/caesium/.tmp/caesium-cli/caesium \
  -e CAESIUM_HOST="$server_host" \
  --network=host \
  -w /bld/caesium \
  "$runner_image" \
  sh scripts/integration-test.sh \
    -test.run '^TestIntegrationTestSuite$' \
    -testify.m '^TestKubernetesMappedNamespace$' | tee "$scenario_log"

# A missing method or a skipped subtest must not report green.
if ! grep -Eq -- '--- PASS: TestIntegrationTestSuite/TestKubernetesMappedNamespace \(' "$scenario_log"; then
  echo "Mapped-namespace scenario did not pass" >&2
  exit 1
fi
if grep -Eq -- '--- SKIP: TestIntegrationTestSuite/TestKubernetesMappedNamespace' "$scenario_log"; then
  echo "Mapped-namespace scenario skipped required coverage" >&2
  exit 1
fi
