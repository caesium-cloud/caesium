#!/usr/bin/env bash
# Isolation-context lane (identity-and-access C2, design §10.1): a three-node
# distributed cluster proving that an owner carries a namespace's isolation
# context to a capable worker and never to one without the capability.
#
#   node A (owner)     serves the policy fixture; its worker withdraws the
#                      capability (it renews A's run leases and may run
#                      default-namespace tasks, never isolated ones);
#   node B (capable)   serves NO policy file, advertises isolation_v1;
#   node C (incapable) serves no policy file and withdraws the capability
#                      (CAESIUM_WORKER_ISOLATION_V1_DISABLED=true).
#
# Because B has no policy file, a secret-rule allow or deny observed on B can
# only have come from A's context. The runner drives A's API and asserts:
# TestIntegrationTestSuite/TestDistributedIsolationContextReachesCapableWorker.
#
# Invoked by `just integration-test-isolation`; every input comes from the
# environment so the recipe stays declarative.
set -euo pipefail

: "${CONTAINER_CLI:?}" "${PLATFORM:?}" "${TEST_IMAGE:?}" "${RUNNER_IMAGE:?}" "${SOCK:?}" "${REPO_DIR:?}" "${BLD_DIR:?}"

net=caesium-isolation-net
subnet=172.31.251.0/24
ip_a=172.31.251.10
ip_b=172.31.251.11
ip_c=172.31.251.12
token=integration-isolation-internal-token-0123456789
names=(caesium-iso-a caesium-iso-b caesium-iso-c)
log="$REPO_DIR/.tmp/integration-test-isolation.log"
cli_dir="$REPO_DIR/.tmp/caesium-cli"

cleanup() {
    for name in "${names[@]}"; do
        "$CONTAINER_CLI" rm -f "$name" >/dev/null 2>&1 || true
    done
    "$CONTAINER_CLI" network rm "$net" >/dev/null 2>&1 || true
}
dump_logs() {
    for name in "${names[@]}"; do
        echo "===== $name ====="
        "$CONTAINER_CLI" logs "$name" 2>&1 | tail -n 200 || true
    done
}
trap cleanup EXIT
cleanup
mkdir -p "$REPO_DIR/.tmp"

# The CLI the suite drives comes from the same test image as the servers.
rm -rf "$cli_dir"
mkdir -p "$cli_dir"
cli_ctr=$("$CONTAINER_CLI" create --platform "$PLATFORM" "$TEST_IMAGE" true)
"$CONTAINER_CLI" cp "$cli_ctr":/bin/caesium "$cli_dir/caesium"
"$CONTAINER_CLI" rm -f "$cli_ctr" >/dev/null
chmod +x "$cli_dir/caesium"

"$CONTAINER_CLI" network create --subnet "$subnet" "$net" >/dev/null

# start_node NAME IP [extra docker args...]
start_node() {
    local name=$1 ip=$2
    shift 2
    "$CONTAINER_CLI" run -d --platform "$PLATFORM" \
        --name "$name" --network "$net" --ip "$ip" \
        --privileged --user 0:0 \
        -v "$SOCK":/var/run/docker.sock \
        -e DOCKER_HOST=unix:///var/run/docker.sock \
        -e CAESIUM_LOG_LEVEL=debug \
        -e CAESIUM_EXECUTION_MODE=distributed \
        -e CAESIUM_NODE_ADDRESS="$ip:9001" \
        -e CAESIUM_DATABASE_VOTERS=3 \
        -e CAESIUM_DATABASE_STANDBYS=0 \
        -e CAESIUM_INTERNAL_WAKEUP_TOKEN="$token" \
        -e CAESIUM_INTERNAL_PORT=8443 \
        -e CAESIUM_RUN_OWNER_ENABLED=true \
        -e CAESIUM_RUN_LEASE_TTL=30s \
        -e CAESIUM_RUN_OWNER_DISPATCH_INTERVAL=500ms \
        -e CAESIUM_RUN_OWNER_DISPATCH_DEADLINE=5m \
        -e CAESIUM_WORKER_POOL_SIZE=2 \
        -e CAESIUM_WORKER_POLL_INTERVAL=500ms \
        -e CAESIUM_WORKER_RECLAIM_INTERVAL=500ms \
        -e CAESIUM_WORKER_LEASE_TTL=30s \
        "$@" \
        "$TEST_IMAGE" start >/dev/null
}

wait_healthy() {
    local ip=$1
    for _ in $(seq 1 120); do
        if "$CONTAINER_CLI" run --rm --network "$net" --entrypoint sh "$RUNNER_IMAGE" -c "wget -q -O /dev/null http://$ip:8080/health" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    echo "node $ip never became healthy" >&2
    return 1
}

start_node caesium-iso-a "$ip_a" \
    -v "$REPO_DIR/test/testdata/policy":/etc/caesium/policy-fixture:ro \
    -e CAESIUM_AUTH_POLICY_FILE=/etc/caesium/policy-fixture/two-namespaces.yaml \
    -e CAESIUM_WORKER_ISOLATION_V1_DISABLED=true
wait_healthy "$ip_a" || { dump_logs; exit 1; }
start_node caesium-iso-b "$ip_b" \
    -e CAESIUM_DATABASE_NODES="$ip_a:9001" \
    -e MARKETING_IT_TOKEN=it-marketing-token \
    -e ISOLATION_IT_DENIED_TOKEN=it-denied-token
# Join one node at a time: concurrent first-start migrations race on the
# shared catalog ("database is locked").
wait_healthy "$ip_b" || { dump_logs; exit 1; }
start_node caesium-iso-c "$ip_c" \
    -e CAESIUM_DATABASE_NODES="$ip_a:9001" \
    -e CAESIUM_WORKER_ISOLATION_V1_DISABLED=true \
    -e MARKETING_IT_TOKEN=it-marketing-token \
    -e ISOLATION_IT_DENIED_TOKEN=it-denied-token
wait_healthy "$ip_c" || { dump_logs; exit 1; }

rm -f "$log"
set +e
"$CONTAINER_CLI" run --rm --platform "$PLATFORM" \
    --network "$net" \
    -v "$REPO_DIR":"$BLD_DIR" \
    -v "$SOCK":/var/run/docker.sock \
    -e DOCKER_HOST=unix:///var/run/docker.sock \
    -e CAESIUM_HOST="$ip_a" \
    -e CAESIUM_CLI_PATH="$BLD_DIR/.tmp/caesium-cli/caesium" \
    -e CAESIUM_EXECUTION_MODE=distributed \
    -e CAESIUM_IT_POLICY_FIXTURE=two-namespaces \
    -e CAESIUM_ISOLATION_LANE=1 \
    -e CAESIUM_ISOLATION_CAPABLE_NODE="$ip_b:9001" \
    -e CAESIUM_ISOLATION_INCAPABLE_NODE="$ip_c:9001" \
    -e CAESIUM_ISOLATION_OWNER_NODE="$ip_a:9001" \
    -w "$BLD_DIR" \
    "$RUNNER_IMAGE" \
    sh -c 'sh scripts/integration-test.sh -test.run "TestIntegrationTestSuite/TestDistributedIsolationContextReachesCapableWorker$"' 2>&1 | tee "$log"
rc=${PIPESTATUS[0]}
set -e
if [ "$rc" -ne 0 ]; then
    dump_logs
    exit "$rc"
fi
if ! grep -qE '^[[:space:]]*--- PASS: TestIntegrationTestSuite/TestDistributedIsolationContextReachesCapableWorker' "$log"; then
    echo "isolation lane: the scenario did not PASS (skipped or absent)" >&2
    dump_logs
    exit 1
fi
echo "isolation lane passed"
