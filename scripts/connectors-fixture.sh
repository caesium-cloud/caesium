#!/bin/sh
# F1 owns the Temporal/worker fixture. F2 extends this with the tracked
# authenticated connector lane; this script never invents a Caesium server.
set -eu
mode=${1:-check}
if [ "$mode" = serve ]; then
    artifacts=${CAESIUM_CONNECTORS_ARTIFACTS:-/artifacts}
    temporal --version >"$artifacts/temporal-version.txt"
    temporal server start-dev --ip 127.0.0.1 --port 7233 --headless \
        --db-filename "$artifacts/temporal.db" >"$artifacts/temporal.log" 2>&1 &
    temporal_pid=$!
    connectors-fixture >"$artifacts/worker.log" 2>&1 &
    worker_pid=$!
    trap 'kill "$worker_pid" "$temporal_pid" 2>/dev/null || true; wait "$worker_pid" "$temporal_pid" 2>/dev/null || true' EXIT
    trap 'exit 129' HUP
    trap 'exit 130' INT
    trap 'exit 143' TERM
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
if [ "$mode" = build ]; then
    exec "$cli" build --platform "$platform" --build-arg BUILDER_IMAGE="$builder" \
        -f build/Dockerfile.connector-fixture -t "$image" .
fi
artifacts=${CAESIUM_CONNECTORS_ARTIFACTS:-$root/.tmp/connectors-fixture/$tag-$$}
case "$artifacts" in /*) ;; *) echo "artifact directory must be absolute" >&2; exit 1 ;; esac
# Stale journals or CID files must never satisfy this candidate's assertions or
# confer ownership of a resource created by an earlier invocation.
python3 - "$artifacts" <<'PY'
from pathlib import Path
import sys
path = Path(sys.argv[1])
if path.is_symlink() or (path.exists() and (not path.is_dir() or any(path.iterdir()))):
    raise SystemExit("refusing nonempty or invalid artifact directory")
path.mkdir(parents=True, exist_ok=True)
PY
mkdir "$artifacts/cli"
server_cid=$artifacts/server-container-id
fixture_cid=$artifacts/fixture-container-id
runner_cid=$artifacts/runner-container-id
fixture_name=caesium-connectors-fixture-$tag-$$
runner_name=caesium-connectors-runner-$tag-$$
server_name=${CAESIUM_CONNECTORS_SERVER_CONTAINER:-caesium-server-agent-test}
server_id=
runner_cli_pid=
existing=${CAESIUM_CONNECTORS_EXISTING_SERVER:-false}
case "$existing" in true|false) ;; *) echo "existing server option must be true or false" >&2; exit 1 ;; esac
host_user=$(id -u):$(id -g)
candidate=$(git rev-parse HEAD)
git status --porcelain >"$artifacts/source-status.txt"
git diff --binary HEAD >"$artifacts/source-diff.patch"
printf '%s\n' "$candidate" >"$artifacts/candidate-sha.txt"
# All tracked/staged and untracked source identities, including paths outside
# the fixture directory. NUL-separated git paths also handle whitespace safely.
python3 - "$artifacts/source-checksums.json" <<'PY'
import hashlib, json, os, subprocess, sys
paths = set(subprocess.check_output(["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"]).split(b"\0"))
records = []
for path in sorted(paths - {b""}):
    if not os.path.lexists(path):
        records.append({"path": os.fsdecode(path), "deleted": True})
        continue
    if os.path.islink(path):
        data = os.fsencode(os.readlink(path))
        kind = "symlink"
    elif os.path.isfile(path):
        with open(path, "rb") as source:
            data = source.read()
        kind = "file"
    else:
        continue
    records.append({"path": os.fsdecode(path), "kind": kind, "sha256": hashlib.sha256(data).hexdigest()})
with open(sys.argv[1], "w") as output:
    json.dump(records, output, indent=2)
PY
created_id() {
    [ -f "$1" ] || return 1
    cid=$(cat "$1")
    [ "${#cid}" -eq 64 ] || return 1
    case "$cid" in *[!0-9a-f]*) return 1 ;; esac
    printf '%s\n' "$cid"
}
record_server() {
    if ! "$cli" container inspect "$1" | python3 -c '
import json, sys
records = json.load(sys.stdin)
for record in records:
    config = record.setdefault("Config", {})
    config["Env"] = [entry.split("=", 1)[0] + "=[REDACTED]" if any(word in entry.split("=", 1)[0] for word in ("KEY", "TOKEN", "SECRET", "PASSWORD")) else entry for entry in config.get("Env", [])]
json.dump(records, sys.stdout, indent=2)
' >"$artifacts/server.inspect.tmp"; then
        rm -f "$artifacts/server.inspect.tmp"
        return 1
    fi
    mv "$artifacts/server.inspect.tmp" "$artifacts/server.json"
    server_image=$("$cli" container inspect --format '{{.Image}}' "$1") || return 1
    "$cli" image inspect "$server_image" >"$artifacts/server-image.tmp" || return 1
    mv "$artifacts/server-image.tmp" "$artifacts/server-image.json"
}
cleanup() {
    # EXIT cleanup also runs when an explicit signal trap exits under dash.
    # Read CID files here: interruption can happen after creation but before a
    # shell variable is assigned. Names never authorize removal.
    trap '' HUP INT TERM
    if cleanup_id=$(created_id "$runner_cid"); then
        "$cli" rm -f "$cleanup_id" >/dev/null 2>&1 || true
    fi
    if [ -n "$runner_cli_pid" ]; then kill "$runner_cli_pid" 2>/dev/null || true; fi
    if cleanup_id=$(created_id "$fixture_cid"); then
        "$cli" logs "$cleanup_id" >"$artifacts/container.log" 2>&1 || true
        "$cli" rm -f "$cleanup_id" >/dev/null 2>&1 || true
    fi
    "$cli" image inspect "$image" "$runner" >"$artifacts/images.json" 2>/dev/null || true
    cleanup_server_id=$server_id
    if [ -z "$cleanup_server_id" ]; then cleanup_server_id=$(created_id "$server_cid") || cleanup_server_id=; fi
    if [ -n "$cleanup_server_id" ]; then
        record_server "$cleanup_server_id" || true
        "$cli" logs "$cleanup_server_id" 2>&1 | sed -E 's/csk_[[:alnum:]_-]+/[REDACTED-API-KEY]/g' >"$artifacts/caesium.log" || true
    fi
    if cleanup_id=$(created_id "$server_cid"); then
        "$cli" rm -f "$cleanup_id" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
"$cli" build --platform "$platform" --build-arg BUILDER_IMAGE="$builder" \
    -f build/Dockerfile.connector-fixture -t "$image" .
"$cli" build --platform "$platform" --build-arg BUILDER_IMAGE="$builder" \
    -f build/Dockerfile.integration -t "$runner" .
if [ "$existing" = false ]; then
    [ "$server_name" = caesium-server-agent-test ] || { echo "tracked integration-up-agent uses caesium-server-agent-test" >&2; exit 1; }
    # Build before the atomic create. The tracked recipe has one shared env
    # body and its CID-file mode neither removes nor starts an existing name.
    just tag="$tag" build-stress build-test build-triage-agent
    [ ! -e "$server_cid" ] || { echo "server CID file already exists" >&2; exit 1; }
    CAESIUM_SKIP_IMAGE_BUILD=true CAESIUM_CONNECTORS_SERVER_CIDFILE="$server_cid" \
        just --quiet tag="$tag" integration-up-agent >"$artifacts/server-create.log"
    server_id=$(created_id "$server_cid")
    record_server "$server_id"
    "$cli" start "$server_id" >"$artifacts/server-start.log"
else
    # Resolve the borrowed name once; subsequent reads use this immutable ID.
    server_id=$("$cli" container inspect --format '{{.Id}}' "$server_name")
    record_server "$server_id"
fi
# Host credentials belong only to an explicitly borrowed server. An owned
# server always supplies its own freshly generated bootstrap key.
admin_key=
if [ "$existing" = true ]; then admin_key=${CAESIUM_AUTH_ADMIN_KEY:-${CAESIUM_API_KEY:-}}; fi
for _attempt in $(seq 1 60); do
    if [ -n "$admin_key" ]; then break; fi
    admin_key=$("$cli" logs "$server_id" 2>&1 | awk '/csk_/ {for (i=1;i<=NF;i++) if ($i ~ /^csk_/) {print $i;exit}}' | tr -d '\r')
    [ -n "$admin_key" ] || sleep 1
done
[ -n "$admin_key" ] || { echo "tracked auth server bootstrap key did not appear" >&2; exit 1; }
[ ! -e "$fixture_cid" ] || { echo "fixture CID file already exists" >&2; exit 1; }
"$cli" create --platform "$platform" --name "$fixture_name" --cidfile "$fixture_cid" \
    --network="container:$server_id" --user "$host_user" -v "$artifacts:/artifacts" \
    -e CAESIUM_CONNECTORS_CANDIDATE_SHA="$candidate" \
    -e CAESIUM_MANUAL_TRIGGER_API_KEY=integration-test-key "$image" >"$artifacts/fixture-create.log"
fixture_id=$(created_id "$fixture_cid")
"$cli" start "$fixture_id" >"$artifacts/fixture-start.log"
ready=false
for _attempt in $(seq 1 120); do
    if "$cli" exec "$fixture_id" curl -fsS http://127.0.0.1:7240/health >"$artifacts/health.json" 2>/dev/null; then ready=true; break; fi
    if [ "$("$cli" inspect --format '{{.State.Running}}' "$fixture_id")" != true ]; then break; fi
    sleep 1
done
[ "$ready" = true ] || { echo "fixture/server/real worker failed readiness" >&2; exit 1; }
"$cli" cp "$server_id:/bin/caesium" "$artifacts/cli/caesium"
selection=TestIntegrationTestSuite/TestConnectorsFixture
if [ -f test/connectors_messages_test.go ]; then
    selection='TestIntegrationTestSuite/(TestConnectorsFixture|TestConnectorsMessages)'
else
    echo "B2 adapter qualification not included: message scenario source is absent"
fi
[ ! -e "$runner_cid" ] || { echo "runner CID file already exists" >&2; exit 1; }
"$cli" create --platform "$platform" --name "$runner_name" --cidfile "$runner_cid" \
    --network="container:$server_id" --user "$host_user" \
    -v "$root:/bld/caesium" -v "$artifacts:/artifacts" \
    -e CAESIUM_CLI_PATH=/artifacts/cli/caesium -e CAESIUM_CONNECTORS_FIXTURE=true \
    -e CAESIUM_CONNECTORS_ARTIFACTS=/artifacts -e CAESIUM_CONNECTORS_CANDIDATE_SHA="$candidate" \
    -e CAESIUM_API_KEY="$admin_key" -e CAESIUM_AUTH_ADMIN_KEY="$admin_key" \
    -e CAESIUM_MANUAL_TRIGGER_API_KEY=integration-test-key -w /bld/caesium "$runner" \
    sh scripts/integration-test.sh -test.run "$selection" >"$artifacts/runner-create.log"
runner_id=$(created_id "$runner_cid")
"$cli" start -a "$runner_id" >"$artifacts/test.log" 2>&1 &
runner_cli_pid=$!
runner_rc=0
wait "$runner_cli_pid" || runner_rc=$?
runner_cli_pid=
if [ "$runner_rc" -ne 0 ]; then cat "$artifacts/test.log"; exit "$runner_rc"; fi
cat "$artifacts/test.log"
# A selected fixture scenario cannot pass via a skip or a zero-match command.
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
