#!/bin/sh
# F1 owns the Temporal/worker fixture. F2 extends this with the tracked
# authenticated connector lane; this script never invents a Caesium server.
set -eu
mode=${1:-check}
if [ "$mode" = serve ]; then
    artifacts=${CAESIUM_CONNECTORS_ARTIFACTS:-/artifacts}
    mkdir -p "$artifacts"
    temporal --version >"$artifacts/temporal-version.txt"
    temporal server start-dev --ip 127.0.0.1 --port 7233 --headless \
        --db-filename "$artifacts/temporal.db" >"$artifacts/temporal.log" 2>&1 &
    temporal_pid=$!
    connectors-fixture >"$artifacts/worker.log" 2>&1 &
    worker_pid=$!
    trap 'kill "$worker_pid" "$temporal_pid" 2>/dev/null || true; wait "$worker_pid" "$temporal_pid" 2>/dev/null || true' EXIT
    trap 'exit 143' TERM INT
    # A dead server/worker must end the container, not leave a healthy shell.
    while kill -0 "$temporal_pid" 2>/dev/null && kill -0 "$worker_pid" 2>/dev/null; do sleep 1; done
    exit 1
fi
if [ "$mode" != check ] && [ "$mode" != build ]; then
    echo "usage: connectors-fixture.sh [build|check|serve]" >&2; exit 2
fi
cd "$(dirname "$0")/.."
root=$(pwd)
cli=${CAESIUM_CONTAINER_CLI:-docker}
platform=${CAESIUM_PLATFORM:-linux/$(uname -m | sed 's/aarch64/arm64/;s/x86_64/amd64/')}
tag=${CAESIUM_CONNECTORS_TAG:-latest}
builder=${CAESIUM_CONNECTORS_BUILDER_IMAGE:-caesiumcloud/caesium-builder:$tag-full}
image=caesiumcloud/caesium-connectors-fixture:$tag
runner=caesiumcloud/caesium-connectors-runner:$tag
candidate=$(git rev-parse HEAD)
source_status=$(git status --porcelain)
source_diff=$(git diff --binary)
"$cli" build --platform "$platform" --build-arg BUILDER_IMAGE="$builder" \
    -f build/Dockerfile.connector-fixture -t "$image" .
[ "$mode" = check ] || exit 0
"$cli" build --platform "$platform" --build-arg BUILDER_IMAGE="$builder" \
    -f build/Dockerfile.integration -t "$runner" .
server=${CAESIUM_CONNECTORS_SERVER_CONTAINER:-caesium-server-agent-test}
owned_server=false
owned_fixture=false
fixture=caesium-connectors-fixture-$tag-$$
artifacts=${CAESIUM_CONNECTORS_ARTIFACTS:-$root/.tmp/connectors-fixture/$tag-$$}
mkdir -p "$artifacts/cli"
printf '%s\n' "$source_status" >"$artifacts/source-status.txt"
printf '%s\n' "$source_diff" >"$artifacts/source-diff.patch"
# Include untracked fixture source in development provenance as well.
if command -v sha256sum >/dev/null 2>&1; then
    find test/connectors -type f -exec sha256sum {} \; >"$artifacts/fixture-source-checksums.txt"
    sha256sum test/connectors_fixture_test.go build/Dockerfile.connector-fixture scripts/connectors-fixture.sh justfile >>"$artifacts/fixture-source-checksums.txt"
else
    find test/connectors -type f -exec shasum -a 256 {} \; >"$artifacts/fixture-source-checksums.txt"
    shasum -a 256 test/connectors_fixture_test.go build/Dockerfile.connector-fixture scripts/connectors-fixture.sh justfile >>"$artifacts/fixture-source-checksums.txt"
fi
cleanup() {
    if [ "$owned_fixture" = true ]; then
        "$cli" logs "$fixture" >"$artifacts/container.log" 2>&1 || true
        "$cli" rm -f "$fixture" >/dev/null 2>&1 || true
    fi
    "$cli" image inspect "$image" "$runner" >"$artifacts/images.json" 2>/dev/null || true
    if [ "$owned_server" = true ]; then
        "$cli" logs "$server" 2>&1 | sed -E 's/csk_[[:alnum:]_-]+/[REDACTED-API-KEY]/g' >"$artifacts/caesium.log" || true
        "$cli" rm -f "$server" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT
if [ "${CAESIUM_CONNECTORS_EXISTING_SERVER:-false}" != true ]; then
    if "$cli" container inspect "$server" >/dev/null 2>&1; then
        echo "refusing to replace existing Caesium container $server" >&2; exit 1
    fi
    [ "$server" = caesium-server-agent-test ] || { echo "tracked integration-up-agent uses caesium-server-agent-test" >&2; exit 1; }
    owned_server=true
    just tag="$tag" integration-up-agent
else
    "$cli" container inspect "$server" >"$artifacts/server.json"
fi
# The shipped auth lane emits its bootstrap key once. Capture it in memory,
# following integration-test-agent; never print it or persist unredacted logs.
admin_key="${CAESIUM_AUTH_ADMIN_KEY:-${CAESIUM_API_KEY:-}}"
for _attempt in $(seq 1 60); do
    if [ -n "$admin_key" ]; then break; fi
    admin_key=$("$cli" logs "$server" 2>&1 | awk '/csk_/ {for (i=1;i<=NF;i++) if ($i ~ /^csk_/) {print $i;exit}}' | tr -d '\r')
    sleep 1
done
[ -n "$admin_key" ] || { echo "tracked auth server bootstrap key did not appear" >&2; exit 1; }
"$cli" run -d --platform "$platform" --name "$fixture" --network="container:$server" \
    -v "$artifacts:/artifacts" \
    -e CAESIUM_CONNECTORS_CANDIDATE_SHA="$candidate" \
    -e CAESIUM_CONNECTORS_RUNNER_KEY \
    -e CAESIUM_MANUAL_TRIGGER_API_KEY=integration-test-key "$image" >"$artifacts/fixture-container-id"
owned_fixture=true
ready=false
for _attempt in $(seq 1 120); do
    if "$cli" exec "$fixture" curl -fsS http://127.0.0.1:7240/health >"$artifacts/health.json" 2>/dev/null; then ready=true; break; fi
    if [ "$("$cli" inspect --format '{{.State.Running}}' "$fixture")" != true ]; then break; fi
    sleep 1
done
[ "$ready" = true ] || { echo "fixture/server/real worker failed readiness" >&2; exit 1; }
"$cli" cp "$server:/bin/caesium" "$artifacts/cli/caesium"
printf '%s\n' "$candidate" >"$artifacts/candidate-sha.txt"
selection=TestIntegrationTestSuite/TestConnectorsFixture
if [ -f test/connectors_messages_test.go ]; then
    selection='TestIntegrationTestSuite/(TestConnectorsFixture|TestConnectorsMessages)'
else
    echo "B2 adapter qualification not included: message scenario source is absent"
fi
"$cli" run --rm --platform "$platform" --network="container:$server" \
    -v "$root:/bld/caesium" -v "$artifacts:/artifacts" \
    -e CAESIUM_CLI_PATH=/artifacts/cli/caesium \
    -e CAESIUM_CONNECTORS_FIXTURE=true \
    -e CAESIUM_CONNECTORS_ARTIFACTS=/artifacts \
    -e CAESIUM_CONNECTORS_CANDIDATE_SHA="$candidate" \
    -e CAESIUM_API_KEY="$admin_key" -e CAESIUM_AUTH_ADMIN_KEY="$admin_key" \
    -e CAESIUM_MANUAL_TRIGGER_API_KEY=integration-test-key \
    -w /bld/caesium "$runner" \
    sh scripts/integration-test.sh -test.run "$selection" \
    >"$artifacts/test.log" 2>&1 || { cat "$artifacts/test.log"; exit 1; }
cat "$artifacts/test.log"
# Selected F1 never accepts an off-lane skip or a zero-match test command.
if ! grep -q -- '--- PASS: TestIntegrationTestSuite/TestConnectorsFixture ' "$artifacts/test.log"; then
    echo "fixture scenario did not pass" >&2; exit 1
fi
if [ -f test/connectors_messages_test.go ] && ! grep -q -- '--- PASS: TestIntegrationTestSuite/TestConnectorsMessages ' "$artifacts/test.log"; then
    echo "selected B2 adapter scenario did not pass" >&2; exit 1
fi
if grep -qE -- '--- SKIP: .*TestConnectors(Fixture|Messages)' "$artifacts/test.log"; then
    echo "selected fixture scenario skipped" >&2; exit 1
fi
printf 'Connector fixture evidence: %s\n' "$artifacts"
