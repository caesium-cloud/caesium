#!/usr/bin/env bash
# Advisory real-surface connector lane. Resources remain owned by the shared
# fixture drivers; this script chooses tracked recipes and retained evidence.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if [[ "${1:-}" == browser || "${CAESIUM_CONNECTORS_BROWSER_PHASE:-false}" == true ]]; then
    : "${CAESIUM_CONNECTORS_ARTIFACTS:?fixture artifacts required}"
    : "${CAESIUM_CONNECTORS_CANDIDATE_SHA:?candidate required}"
    : "${CAESIUM_E2E_AUTH_ADMIN_KEY:?authenticated fixture required}"
    browser="$CAESIUM_CONNECTORS_ARTIFACTS/../browser"
    mkdir "$browser"
    printf '%s\n' "$CAESIUM_CONNECTORS_CANDIDATE_SHA" >"$browser/candidate-sha.txt"
    python3 - "$CAESIUM_CONNECTORS_ARTIFACTS/server.json" >"$browser/server-image-id.txt" <<'PY'
import json, sys
record, = json.load(open(sys.argv[1]))
print(record['Image'])
PY
    export CI=true PLAYWRIGHT_BASE_URL="$CAESIUM_BASE_URL"
    export PLAYWRIGHT_JSON_OUTPUT_NAME="$browser/playwright-results.json"
    cd "$ROOT/ui"
    npm ci >"$browser/npm.log" 2>&1
    npx playwright install chromium >"$browser/install.log" 2>&1
    result=0
    # Only the isolated connector project is selected. Retry zero preserves a
    # first-attempt failure; the shared evidence collector checks every case.
    npx playwright test --project=connectors --retries=0 --workers=1 \
        --reporter=line,json --output="$browser/test-results" >"$browser/test.log" 2>&1 || result=$?
    printf '%s\n' "$result" >"$browser/exit-status"
    cat "$browser/test.log"
    exit "$result"
fi
[[ $# == 0 ]] || { echo 'usage: connectors-test.sh [browser]' >&2; exit 2; }
for command in git python3 just docker node npm kind kubectl helm; do
    command -v "$command" >/dev/null || { echo "missing command: $command" >&2; exit 1; }
done
[[ "${CAESIUM_PODMAN:-false}" == false && "${CAESIUM_CONTAINER_CLI:-docker}" == docker ]] || {
    echo 'connector lane requires rootful Docker' >&2; exit 1;
}
candidate="$(git rev-parse HEAD)"
[[ "${CANDIDATE_SHA:-$candidate}" == "$candidate" ]] || { echo 'candidate does not match HEAD' >&2; exit 1; }
[[ -z "$(git status --porcelain)" ]] || { echo 'connector lane requires a clean candidate' >&2; exit 1; }
artifacts="${CAESIUM_CONNECTORS_ARTIFACTS:-$ROOT/.tmp/lane-evidence/connectors}"
[[ "$artifacts" == /* ]] || { echo 'artifact directory must be absolute' >&2; exit 1; }
python3 - "$artifacts" <<'PY'
from pathlib import Path
import sys
p = Path(sys.argv[1])
if p.is_symlink() or (p.exists() and (not p.is_dir() or any(p.iterdir()))):
    raise SystemExit('refusing nonempty or invalid artifact directory')
p.mkdir(parents=True)
PY
# Each resource driver owns a fresh process group. Forward interruption only
# to that group, including its browser/engine clients, then await its cleanup.
child_pid=
run_owned() {
    exec python3 - "$@" <<'PYCHILD'
import os, signal, subprocess, sys
child = None
pending = []
def forward(signum, _frame):
    if child is None:
        pending.append(signum)
    else:
        try: os.killpg(child.pid, signum)
        except ProcessLookupError: pass
for signum in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
    signal.signal(signum, forward)
child = subprocess.Popen(sys.argv[1:], start_new_session=True)
for signum in pending: forward(signum, None)
result = child.wait()
raise SystemExit(result if result >= 0 else 128-result)
PYCHILD
}
interrupted() {
    signum=$1
    result=$2
    trap '' HUP INT TERM
    if [[ -n "$child_pid" ]]; then
        kill -"$signum" "$child_pid" 2>/dev/null || true
        wait "$child_pid" || true
        child_pid=
    fi
    exit "$result"
}
trap 'interrupted HUP 129' HUP
trap 'interrupted INT 130' INT
trap 'interrupted TERM 143' TERM
# Raw local browser diagnostics and cluster credentials stay private. Export
# only selected machine proof/logs, with API-key redaction, to CI's public path.
finish() {
    result=$?
    trap - EXIT
    printf '%s\n' "$result" >"$artifacts/exit-status"
    python3 - "$artifacts" <<'PY'
from pathlib import Path
import base64, json, re, sys
root = Path(sys.argv[1]); public = root / 'public'; public.mkdir(exist_ok=True)
allow = {
 'fixture': ('candidate-sha.txt', 'source-checksums.json', 'source-status.txt', 'server.json',
             'server-image.json', 'server-env.json', 'image-provenance.json', 'images.json', 'health.json', 'events.jsonl',
             'exit-status', 'fixture-evidence.json', 'messages-provider-evidence.json', 'correlation-evidence.json',
             'operations-component-evidence.json', 'read-http-evidence.json', 'read-correlation-http-evidence.json',
             'actions-http-evidence.json', 'actions-http-responses.json', 'invalid-startup-evidence.json',
             'test.log', 'caesium.log', 'worker.log', 'temporal-version.txt', 'server-version.txt'),
 'browser': ('candidate-sha.txt', 'server-image-id.txt', 'playwright-results.json', 'exit-status', 'test.log'),
 'cluster': ('candidate-sha.txt', 'source-checksums.json', 'source-status.txt', 'config-cluster-evidence.json',
             'kind-node-ids.json', 'kind-nodes.txt', 'server-image-id.txt', 'candidate-identities.txt',
             'loaded-candidate-cri.json', 'candidate-seed-identities.txt', 'imported-server-digests.txt', 'reuse-image-identities.json',
             'fixture-health.json', 'statefulset.json', 'connector-config-test.log', 'events.jsonl', 'exit-status'),
}
for group, names in allow.items():
    for name in names:
        source = root / group / name
        if source.is_file() and not source.is_symlink():
            target = public / group / name; target.parent.mkdir(exist_ok=True)
            text = source.read_text(errors='replace')
            if group == 'cluster' and name == 'statefulset.json':
                raw = json.loads(text)
                spec = raw['spec']; pod = spec['template']['spec']; container = pod['containers'][0]
                flags = {'CAESIUM_CONNECTORS_ENABLED', 'CAESIUM_AUTH_MODE', 'CAESIUM_RUN_QUEUE_ENABLED',
                         'CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED', 'CAESIUM_CONNECTORS_CONFIG_FILE',
                         'CAESIUM_EXECUTION_MODE', 'CAESIUM_RUN_OWNER_ENABLED', 'CAESIUM_DATABASE_SHARDS',
                         'CAESIUM_DATABASE_VOTERS', 'CAESIUM_DATABASE_STANDBYS'}
                text = json.dumps({'spec': {'replicas': spec['replicas'],
                    'volumeClaimTemplates': spec.get('volumeClaimTemplates', []),
                    'template': {'spec': {'containers': [{'image': container['image'],
                        'env': [item for item in container['env'] if item['name'] in flags]}]}}}}, indent=2)
            if group == 'browser' and name == 'playwright-results.json':
                report = json.loads(text)
                def scrub_body(value):
                    if isinstance(value, dict):
                        if value.get('contentType') == 'application/json' and isinstance(value.get('body'), str):
                            raw = base64.b64decode(value['body'], validate=True).decode()
                            safe = re.sub(r'csk_[A-Za-z0-9_-]+', '[REDACTED-API-KEY]', raw)
                            value['body'] = base64.b64encode(safe.encode()).decode()
                        for child in value.values(): scrub_body(child)
                    elif isinstance(value, list):
                        for child in value: scrub_body(child)
                scrub_body(report)
                text = json.dumps(report, indent=2)
            target.write_text(re.sub(r'csk_[A-Za-z0-9_-]+', '[REDACTED-API-KEY]', text))
PY
    exit "$result"
}
trap finish EXIT
export CANDIDATE_SHA="$candidate"
export CAESIUM_CONNECTORS_LANE=true
export CAESIUM_CONNECTORS_EXISTING_SERVER=false
export CAESIUM_CONNECTORS_SERVER_CONTAINER=caesium-server-connectors-test
export CAESIUM_CONNECTORS_BROWSER_HOOK="$ROOT/scripts/connectors-test.sh"
CAESIUM_CONNECTORS_ARTIFACTS="$artifacts/fixture" run_owned sh scripts/connectors-fixture.sh check \
    >"$artifacts/fixture.log" 2>&1 &
child_pid=$!
result=0
wait "$child_pid" || result=$?
child_pid=
[[ "$result" == 0 ]] || { cat "$artifacts/fixture.log"; exit "$result"; }
# Freeze the tested images at the handoff. Tags are selectors, so a coherent
# replacement of every tag must still fail against the first phase's receipts.
image_ids=$(python3 - "$artifacts/fixture" "$candidate" <<'PYHANDOFF'
import json, os, re, sys
from pathlib import Path
root, candidate = Path(sys.argv[1]), sys.argv[2]
proof = json.loads((root / 'image-provenance.json').read_text())
images = json.loads((root / 'images.json').read_text())
if proof.get('candidate_sha') != candidate or not isinstance(images, list) or len(images) != 2:
    raise SystemExit('fixture image handoff lacks candidate-bound identities')
ids = {}
for role, producer_name in (('server', 'CAESIUM_CONNECTORS_EXPECTED_IMAGE_ID'),
                            ('runner', 'CAESIUM_CONNECTORS_EXPECTED_RUNNER_IMAGE_ID')):
    value = proof.get(role) or {}
    observed, expected = value.get('observed_image_id'), value.get('expected_image_id')
    if not isinstance(observed, str) or not re.fullmatch(r'sha256:[0-9a-f]{64}', observed):
        raise SystemExit('fixture image handoff lacks immutable ' + role + ' identity')
    if value.get('origin') not in ('loaded', 'source-build'):
        raise SystemExit('fixture image handoff lacks ' + role + ' origin')
    if (value['origin'] == 'loaded' or expected) and expected != observed:
        raise SystemExit('fixture image handoff differs from expected ' + role + ' identity')
    if os.environ.get(producer_name) and os.environ[producer_name] != observed:
        raise SystemExit('fixture image handoff differs from producer ' + role + ' identity')
    ids[role] = observed
fixture = images[0].get('Id')
if not isinstance(fixture, str) or not re.fullmatch(r'sha256:[0-9a-f]{64}', fixture):
    raise SystemExit('fixture image handoff lacks immutable provider identity')
if images[1].get('Id') != ids['runner']:
    raise SystemExit('fixture runner runtime and image identities differ')
print(ids['server'], ids['runner'], fixture)
PYHANDOFF
)
read -r reuse_server_id reuse_runner_id reuse_fixture_id <<<"$image_ids"
CAESIUM_CONNECTORS_REUSE_IMAGES=true CAESIUM_CONNECTORS_CLUSTER_ARTIFACTS="$artifacts/cluster" \
    CAESIUM_CONNECTORS_REUSE_SERVER_IMAGE_ID="$reuse_server_id" \
    CAESIUM_CONNECTORS_REUSE_RUNNER_IMAGE_ID="$reuse_runner_id" \
    CAESIUM_CONNECTORS_REUSE_FIXTURE_IMAGE_ID="$reuse_fixture_id" \
    run_owned bash scripts/connectors-config-cluster.sh >"$artifacts/cluster.log" 2>&1 &
child_pid=$!
result=0
wait "$child_pid" || result=$?
child_pid=
[[ "$result" == 0 ]] || { cat "$artifacts/cluster.log"; exit "$result"; }
[[ "$(git rev-parse HEAD)" == "$candidate" && -z "$(git status --porcelain)" ]] || {
    echo 'candidate changed during connector qualification' >&2; exit 1;
}
python3 scripts/collect-lane-evidence.py connectors --artifacts "$artifacts" \
    --candidate-sha "$candidate" --out "$artifacts/evidence.json"
python3 scripts/check-test-evidence.py --manifest test/contracts/scenarios.json \
    --report "$artifacts/evidence.json" --require connectors --strict
printf 'Connector lane evidence: %s\n' "$artifacts/evidence.json"
