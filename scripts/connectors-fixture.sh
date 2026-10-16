#!/bin/sh
# Manual real-provider qualification. C1 adds HTTP/restart proof on the tracked
# authenticated server; F2 owns the later broad CI/browser lane.
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
# The tracked server needs rootful Linux Docker with its default user namespace.
# Refuse unsupported mappings before builds or creation rather than producing
# subordinate-UID files (or failing later on the host-owned evidence mount).
if [ "${CAESIUM_PODMAN:-false}" = true ] || ! engine_server_json=$("$cli" version --format '{{json .Server}}') ||
    ! printf '%s\n' "$engine_server_json" | python3 -c '
import json, sys
try:
    server = json.load(sys.stdin)
    supported = (server.get("Os") == "linux" and server.get("Platform", {}).get("Name", "").startswith("Docker")
                 and any(c.get("Name") == "Engine" for c in server.get("Components", [])))
except (ValueError, TypeError, AttributeError):
    supported = False
raise SystemExit(0 if supported else 1)
'; then
    echo "connector fixture check requires rootful Linux Docker; Podman and unknown engines are unsupported" >&2
    exit 1
fi
if ! engine_security_json=$("$cli" info --format '{{json .SecurityOptions}}') ||
    ! printf '%s\n' "$engine_security_json" | python3 -c '
import json, sys
try:
    options = json.load(sys.stdin)
    supported = (isinstance(options, list) and all(isinstance(v, str) for v in options)
                 and not any(v.split(",", 1)[0] in ("name=rootless", "name=userns") for v in options))
except (ValueError, TypeError):
    supported = False
raise SystemExit(0 if supported else 1)
'; then
    echo "connector fixture check requires rootful Docker without user-namespace remapping" >&2
    exit 1
fi
mkdir "$artifacts/cli"
server_cid=$artifacts/server-container-id
fixture_cid=$artifacts/fixture-container-id
runner_cid=$artifacts/runner-container-id
gateoff_cid=$artifacts/gateoff-container-id
invalid_cid=$artifacts/invalid-startup-container-id
anchor_cid=$artifacts/network-anchor-container-id
fixture_name=caesium-connectors-fixture-$tag-$$
runner_name=caesium-connectors-runner-$tag-$$
gateoff_name=caesium-connectors-gateoff-$tag-$$
invalid_name=caesium-connectors-invalid-startup-$tag-$$
anchor_name=caesium-connectors-network-anchor-$tag-$$
server_name=${CAESIUM_CONNECTORS_SERVER_CONTAINER:-caesium-server-agent-test}
server_id=
network_id=
runner_cli_pid=
control_pid=
existing=${CAESIUM_CONNECTORS_EXISTING_SERVER:-false}
case "$existing" in true|false) ;; *) echo "existing server option must be true or false" >&2; exit 1 ;; esac
http_reads=false
if [ -f test/connectors_read_test.go ]; then
    http_reads=true
    [ "$existing" = false ] || { echo "selected connector HTTP/restart qualification requires an owned server" >&2; exit 1; }
    mkdir "$artifacts/control"
    cat >"$artifacts/control/start-server.sh" <<'SH'
#!/bin/sh
set -eu
if [ -f /artifacts/control/previous-fingerprint ]; then
    CAESIUM_CONNECTORS_CONFIG_PREVIOUS_FINGERPRINT=$(cat /artifacts/control/previous-fingerprint)
    export CAESIUM_CONNECTORS_CONFIG_PREVIOUS_FINGERPRINT
fi
exec /bin/caesium "$@"
SH
    chmod 0755 "$artifacts/control/start-server.sh"
    python3 - "$artifacts/http-connections.yaml" <<'PY'
from pathlib import Path
import sys
raw = Path('test/connectors/testdata/connections.yaml').read_text()
needle = '    enabled: true\n'
if raw.count(needle) != 1:
    raise SystemExit('real fixture configuration must have exactly one enabled connection')
limits = ('    limits:\n      rpcDeadline: 1s\n      maxUnreferencedSnapshots: 4\n'
          '      unreferencedSnapshotMaxAge: 10s\n')
Path(sys.argv[1]).write_text(raw.replace(needle, needle + limits))
PY
fi
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
    if ! python3 "$root/scripts/connectors-fixture-metadata.py" container "$cli" "$1" >"$artifacts/server.inspect.tmp"; then
        rm -f "$artifacts/server.inspect.tmp"
        return 1
    fi
    mv "$artifacts/server.inspect.tmp" "$artifacts/server.json"
    server_image=$("$cli" container inspect --format '{{.Image}}' "$1") || return 1
    python3 "$root/scripts/connectors-fixture-metadata.py" image "$cli" "$server_image" >"$artifacts/server-image.tmp" || return 1
    mv "$artifacts/server-image.tmp" "$artifacts/server-image.json"
}
cleanup() {
    # EXIT cleanup also runs when an explicit signal trap exits under dash.
    # Read CID files here: interruption can happen after creation but before a
    # shell variable is assigned. Names never authorize removal.
    trap '' HUP INT TERM
    if [ -n "$control_pid" ]; then kill "$control_pid" 2>/dev/null || true; wait "$control_pid" 2>/dev/null || true; fi
    if cleanup_id=$(created_id "$runner_cid"); then
        "$cli" rm -f "$cleanup_id" >/dev/null 2>&1 || true
    fi
    if [ -n "$runner_cli_pid" ]; then kill "$runner_cli_pid" 2>/dev/null || true; fi
    if cleanup_id=$(created_id "$gateoff_cid"); then
        python3 "$root/scripts/connectors-fixture-metadata.py" container "$cli" "$cleanup_id" >"$artifacts/gateoff.json" 2>/dev/null || true
        "$cli" rm -f "$cleanup_id" >/dev/null 2>&1 || true
    fi
    if cleanup_id=$(created_id "$invalid_cid"); then
        python3 "$root/scripts/connectors-fixture-metadata.py" container "$cli" "$cleanup_id" >"$artifacts/invalid-startup-container.json" 2>/dev/null || true
        "$cli" rm -f "$cleanup_id" >/dev/null 2>&1 || true
    fi
    if cleanup_id=$(created_id "$fixture_cid"); then
        "$cli" logs "$cleanup_id" >"$artifacts/container.log" 2>&1 || true
        "$cli" rm -f "$cleanup_id" >/dev/null 2>&1 || true
    fi
    python3 "$root/scripts/connectors-fixture-metadata.py" image "$cli" "$image" "$runner" >"$artifacts/images.json" 2>/dev/null || true
    cleanup_server_id=$server_id
    if [ -z "$cleanup_server_id" ]; then cleanup_server_id=$(created_id "$server_cid") || cleanup_server_id=; fi
    if [ -n "$cleanup_server_id" ]; then
        record_server "$cleanup_server_id" || true
        "$cli" logs "$cleanup_server_id" 2>&1 | sed -E 's/csk_[[:alnum:]_-]+/[REDACTED-API-KEY]/g' >"$artifacts/caesium.log" || true
    fi
    if cleanup_id=$(created_id "$server_cid"); then
        "$cli" rm -f "$cleanup_id" >/dev/null 2>&1 || true
    fi
    # The stable namespace owner is last: every app process can restart while
    # the fixture, runner and listening ports retain their network identity.
    if cleanup_id=$(created_id "$anchor_cid"); then
        python3 "$root/scripts/connectors-fixture-metadata.py" container "$cli" "$cleanup_id" >"$artifacts/network-anchor.json" 2>/dev/null || true
        python3 "$root/scripts/connectors-fixture-metadata.py" image "$cli" busybox:1.36.1 >"$artifacts/network-anchor-image.json" 2>/dev/null || true
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
    if [ "$http_reads" = true ]; then
        export CAESIUM_CONNECTORS_SERVER_CONFIG="$artifacts/http-connections.yaml"
        export CAESIUM_CONNECTORS_SERVER_ARTIFACTS="$artifacts"
        [ ! -e "$anchor_cid" ] || { echo "network anchor CID file already exists" >&2; exit 1; }
        "$cli" create --platform "$platform" --name "$anchor_name" --cidfile "$anchor_cid" \
            -p "${CAESIUM_PORT:-8080}:8080" --user "$host_user" \
            -e USER=caesium-fixture -e HOME=/tmp busybox:1.36.1 sleep 2147483647 >"$artifacts/network-anchor-create.log"
        network_id=$(created_id "$anchor_cid")
        "$cli" start "$network_id" >"$artifacts/network-anchor-start.log"
        export CAESIUM_CONNECTORS_NETWORK_ANCHOR="$network_id"
    fi
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
[ -n "$network_id" ] || network_id=$server_id
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
    --network="container:$network_id" --user "$host_user" \
    -e USER=caesium-fixture -e HOME=/tmp -v "$artifacts:/artifacts" \
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
gateoff_url=
if [ "$http_reads" = true ]; then
    # Strict enabled startup is a separate real process, using the exact same
    # candidate image as the authenticated server. The malformed credential is
    # a synthetic canary: it must never appear in the actual startup error.
    cat >"$artifacts/invalid-enabled.yaml" <<'YAML'
version: 1
connections:
  - id: fixture
    provider: temporal
    endpoint: 127.0.0.1:7234
    scope: default
    enabled: true
    credentials:
      inlineToken: fixture-invalid-inline-credential-canary
YAML
    [ ! -e "$invalid_cid" ] || { echo "invalid-startup CID file already exists" >&2; exit 1; }
    "$cli" create --platform "$platform" --name "$invalid_name" --cidfile "$invalid_cid" \
        --network="container:$network_id" --entrypoint /bin/caesium --user "$host_user" \
        -e USER=caesium-fixture -e HOME=/tmp \
        -v "$artifacts/invalid-enabled.yaml:/etc/caesium/connectors/invalid.yaml:ro" \
        -e CAESIUM_PORT=8082 -e CAESIUM_NODE_ADDRESS=127.0.0.1:9003 \
        -e CAESIUM_AUTH_MODE=api-key -e CAESIUM_AUTH_REQUIRE_TLS=false \
        -e CAESIUM_AUTH_KEY_HASH_SECRET=agent-integration-auth-key-hash-secret-000001 \
        -e CAESIUM_CONNECTORS_ENABLED=true -e CAESIUM_CONNECTORS_CONFIG_FILE=/etc/caesium/connectors/invalid.yaml \
        "caesiumcloud/caesium:$tag-test" start >"$artifacts/invalid-startup-create.log"
    invalid_id=$(created_id "$invalid_cid")
    python3 - "$cli" "$invalid_id" "$fixture_id" "$artifacts" "$candidate" <<'PY'
import json, re, subprocess, sys
from pathlib import Path
cli, child, fixture, directory, candidate = sys.argv[1:]
artifacts = Path(directory)

def command(*args, timeout=10):
    return subprocess.run([cli, *args], check=True, stdout=subprocess.PIPE,
                          stderr=subprocess.PIPE, timeout=timeout).stdout

def rpc_count():
    events = json.loads(command('exec', fixture, 'curl', '-fsS', 'http://127.0.0.1:7240/evidence'))
    if not isinstance(events, list) or not all(isinstance(event, dict) for event in events):
        raise SystemExit('invalid startup lacks a conclusive provider RPC journal')
    return sum(event.get('kind') == 'provider_rpc' for event in events)

before = rpc_count()
try:
    command('start', child)
    waited = command('wait', child, timeout=15).decode().strip()
    records = json.loads(command('container', 'inspect', child))
    if not isinstance(records, list) or len(records) != 1 or not isinstance(records[0], dict):
        raise SystemExit('invalid startup lacks exact container state')
    record = records[0]
    state = record.get('State', {})
    raw = command('logs', child).decode(errors='replace')
except (OSError, ValueError, subprocess.SubprocessError):
    raise SystemExit('invalid enabled startup did not yield bounded process evidence') from None
canary = 'fixture-invalid-inline-credential-canary'
safe = re.sub(r'csk_[A-Za-z0-9_-]+', '[REDACTED-API-KEY]', raw.replace(canary, '[REDACTED-CANARY]'))
(artifacts / 'invalid-startup.log').write_text(safe)
after = rpc_count()
if (record.get('Id') != child or state.get('Running') is not False or state.get('OOMKilled') is not False
        or state.get('ExitCode') != 1 or waited != '1'
        or not state.get('StartedAt') or not state.get('FinishedAt')):
    raise SystemExit('invalid enabled startup must exit with application failure, not success, kill or timeout')
if 'inlineToken' not in raw or 'not found' not in raw:
    raise SystemExit('invalid enabled startup did not report the strict credential parser refusal')
if canary in raw:
    raise SystemExit('invalid enabled startup exposed its synthetic credential canary')
if before != after:
    raise SystemExit('invalid enabled startup contacted the provider before refusal')
versions = json.loads((artifacts / 'health.json').read_text())
evidence = {'candidate_sha': candidate, 'container_id': child, 'image_id': record.get('Image'),
            'exit_code': state['ExitCode'], 'wait_exit_code': int(waited), 'oom_killed': False,
            'started_at': state['StartedAt'], 'finished_at': state['FinishedAt'],
            'provider_rpc_before': before, 'provider_rpc_after': after,
            'log_canary_absent': True, 'strict_parser_error_observed': True,
            'sdk': versions.get('sdk'), 'cli': versions.get('cli'), 'server': versions.get('server')}
(artifacts / 'invalid-startup-evidence.json').write_text(json.dumps(evidence, indent=2) + '\n')
(artifacts / 'invalid-startup-evidence.json').chmod(0o600)
PY
    [ ! -e "$gateoff_cid" ] || { echo "gate-off CID file already exists" >&2; exit 1; }
    "$cli" create --platform "$platform" --name "$gateoff_name" --cidfile "$gateoff_cid" \
        --network="container:$network_id" --entrypoint /bin/caesium \
        -e CAESIUM_PORT=8081 -e CAESIUM_NODE_ADDRESS=127.0.0.1:9002 \
        -e CAESIUM_CONNECTORS_ENABLED=false -e CAESIUM_CONNECTORS_CONFIG_FILE=/missing-ignored-config.yaml \
        "caesiumcloud/caesium:$tag-test" start >"$artifacts/gateoff-create.log"
    gateoff_id=$(created_id "$gateoff_cid")
    "$cli" start "$gateoff_id" >"$artifacts/gateoff-start.log"
    ready=false
    for _attempt in $(seq 1 60); do
        if "$cli" exec "$fixture_id" curl -fsS http://127.0.0.1:8081/health/ready >/dev/null 2>&1; then ready=true; break; fi
        sleep 1
    done
    [ "$ready" = true ] || { echo "gate-off process failed local readiness" >&2; exit 1; }
    gateoff_url=http://127.0.0.1:8081
    python3 "$root/scripts/connectors-fixture-control.py" "$cli" "$server_id" "$artifacts/control" >"$artifacts/control.log" 2>&1 &
    control_pid=$!
fi
"$cli" cp "$server_id:/bin/caesium" "$artifacts/cli/caesium"
scenarios=TestConnectorsFixture
for component in messages correlation operations read; do
    if [ -f "test/connectors_${component}_test.go" ]; then
        case "$component" in
            messages) scenario=TestConnectorsMessages ;;
            correlation) scenario=TestConnectorsCorrelation ;;
            operations) scenario=TestConnectorsOperations ;;
            read) scenario=TestConnectorsRead ;;
        esac
        scenarios="$scenarios $scenario"
    fi
done
selection="TestIntegrationTestSuite/($(printf '%s' "$scenarios" | tr ' ' '|'))"
[ ! -e "$runner_cid" ] || { echo "runner CID file already exists" >&2; exit 1; }
"$cli" create --platform "$platform" --name "$runner_name" --cidfile "$runner_cid" \
    --network="container:$network_id" --user "$host_user" \
    -e USER=caesium-fixture -e HOME=/tmp -v "$root:/bld/caesium" -v "$artifacts:/artifacts" \
    -e CAESIUM_CLI_PATH=/artifacts/cli/caesium -e CAESIUM_CONNECTORS_FIXTURE=true \
    -e CAESIUM_CONNECTORS_READ_FIXTURE="$http_reads" -e CAESIUM_CONNECTORS_OWNED_SERVER=true \
    -e CAESIUM_CONNECTORS_GATE_OFF_URL="$gateoff_url" \
    -e CAESIUM_AUTH_KEY_HASH_SECRET=agent-integration-auth-key-hash-secret-000001 \
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
for scenario in $scenarios; do
    if ! grep -q -- "--- PASS: TestIntegrationTestSuite/$scenario " "$artifacts/test.log"; then
        echo "selected $scenario scenario did not pass" >&2; exit 1
    fi
    if grep -qE -- "--- (SKIP|FAIL): TestIntegrationTestSuite/$scenario([ /]|$)" "$artifacts/test.log"; then
        echo "selected $scenario scenario skipped or failed" >&2; exit 1
    fi
done
printf 'Connector fixture evidence: %s\n' "$artifacts"
