#!/usr/bin/env bash
# Advisory real-surface connector lane. Resources remain owned by the shared
# fixture drivers; this script chooses tracked recipes and retained evidence.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# Preparation owns no provider/admin state. The supervisor gives this process
# group one TERM and waits for npm/Playwright cleanup before joining it.
prepare_browser() {
    python3 - "$ROOT/ui" "$1" "${CAESIUM_CONNECTORS_BROWSER_INSTALL_DEPS:-false}" <<'PYPREP'
import os, signal, subprocess, sys, time
from pathlib import Path
ui, logs, install_deps = Path(sys.argv[1]), Path(sys.argv[2]), sys.argv[3]
if install_deps not in ('true', 'false'):
    raise SystemExit('browser dependency option must be true or false')
if logs.is_symlink() or not logs.is_dir():
    raise SystemExit('browser preparation requires its fresh private directory')
interruption = None
child = None
signalled = False
def stop():
    global signalled
    if child is None or signalled: return
    signalled = True
    try: os.killpg(child.pid, signal.SIGTERM)
    except ProcessLookupError: pass
    except PermissionError:
        # Darwin can report EPERM for a group containing only zombies. Reap
        # our terminal leader and require fresh proof that no live member
        # remains; a live denial or inconclusive inspection still fails.
        if child.poll() is None or live_group(): raise
def interrupted(signum, _frame):
    global interruption
    if interruption is None: interruption = 128 + signum
    stop()
def live_group():
    for attempt in range(2):
        try:
            rows = subprocess.check_output(['ps', '-axo', 'pgid=,stat='], text=True)
            break
        except subprocess.CalledProcessError as error:
            # TERM can also hit the ps in this helper's group. Retry once only
            # for our recorded interruption; unrelated errors remain failures.
            if attempt or not interruption or error.returncode not in (-signal.SIGHUP, -signal.SIGINT, -signal.SIGTERM):
                raise
    # Treat observed '?' states as live; they cannot prove cleanup is done.
    members = []
    for line in rows.splitlines():
        fields = line.split()
        if (len(fields) != 2 or not fields[0].isdigit()
            or not fields[1] or fields[1][0] not in 'RSDTtZXxIWUKP?'):
            raise RuntimeError('process-group inspection is inconclusive')
        members.append((int(fields[0]), fields[1]))
    if not members: raise RuntimeError('process-group inspection is inconclusive')
    return any(pgid == child.pid and not state.startswith('Z') for pgid, state in members)
def drain_group(failure=None):
    while True:
        try:
            live = live_group()
        except Exception as error:
            if failure is None: failure = error
            live = None
        if failure is not None or interruption:
            try: stop()
            except Exception as error:
                if failure is None: failure = error
        if live is None:
            # Inspection errors stay fatal. During cleanup only, probe this
            # exact known group conservatively: EPERM is present/unknown;
            # only ESRCH proves absence when ps cannot provide an answer.
            try: os.killpg(child.pid, 0)
            except ProcessLookupError: live = False
            except OSError: live = True
            else: live = True
        if not live:
            if failure is not None: raise failure
            return
        time.sleep(.05)
for signum in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
    signal.signal(signum, interrupted)
commands = (
    ('npm.log', ['npm', 'ci', '--prefer-offline']),
    ('install.log', ['npx', 'playwright', 'install'] + (['--with-deps'] if install_deps == 'true' else []) + ['chromium']),
)
result = 0
for name, command in commands:
    if interruption: break
    result = None
    signalled = False
    try:
        with (logs / name).open('x') as log:
            # Give this helper exclusive signal ownership even if interruption
            # lands inside Popen before the new command is registered.
            child = subprocess.Popen(command, cwd=ui, stdout=log, stderr=subprocess.STDOUT,
                                     start_new_session=True, env=dict(os.environ, CI='true'))
        if interruption: stop()
        result = child.wait()
        if result: stop()
    finally:
        if child is not None:
            cleanup_failure = None
            if result is None:
                try: stop()
                except Exception as error: cleanup_failure = error
                result = child.wait()
            try: drain_group(cleanup_failure)
            finally: child = None
    if result: break
raise SystemExit(interruption or (result if result >= 0 else 128-result))
PYPREP
}
if [[ "${1:-}" == prepare-browser ]]; then
    [[ $# == 1 ]] || { echo 'usage: connectors-test.sh [browser|prepare-browser]' >&2; exit 2; }
    : "${CAESIUM_CONNECTORS_BROWSER_PREP_DIR:?private browser preparation directory required}"
    prepare_browser "$CAESIUM_CONNECTORS_BROWSER_PREP_DIR"
    exit "$?"
fi

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
    export CAESIUM_CONNECTORS_EXPECTED_UI_URL=https://temporal-ui.example.test/console
    export CAESIUM_MANUAL_TRIGGER_API_KEY=integration-test-key
    export PLAYWRIGHT_JSON_OUTPUT_NAME="$browser/playwright-results.json"
    case "${CAESIUM_CONNECTORS_PARALLEL_CLUSTER:-false}" in
        true)
            # Only the supervisor owns the pipe writer. A forged/stale file
            # cannot bypass the actual preparation exit/join and receipt hash.
            python3 - "$ROOT" <<'PYREADY'
import hashlib, json, os, re, stat, subprocess, sys
from pathlib import Path
root = Path(sys.argv[1])
try:
    fd = int(os.environ['CAESIUM_CONNECTORS_BROWSER_PREP_FD'])
    if fd <= 2 or not stat.S_ISFIFO(os.fstat(fd).st_mode): raise ValueError()
    with os.fdopen(fd, 'rb') as channel:
        digest = channel.read(66)
    if not re.fullmatch(b'[0-9a-f]{64}\n', digest): raise ValueError()
    directory = Path(os.environ['CAESIUM_CONNECTORS_BROWSER_PREP_DIR'])
    path = directory / 'status.json'
    if directory.is_symlink() or path.is_symlink() or not path.is_file() or path.stat().st_size > 4096:
        raise ValueError()
    raw = path.read_bytes()
    if hashlib.sha256(raw).hexdigest().encode() + b'\n' != digest: raise ValueError()
    receipt = json.loads(raw)
    expected = {
        'version': 1, 'invocation_id': os.environ['CAESIUM_CONNECTORS_BROWSER_PREP_INVOCATION'],
        'candidate_sha': os.environ['CAESIUM_CONNECTORS_CANDIDATE_SHA'],
        'package_lock_sha256': os.environ['CAESIUM_CONNECTORS_BROWSER_PREP_LOCK_SHA256'],
        'status': 'success', 'exit_code': 0,
    }
    if not isinstance(receipt, dict) or any(type(receipt.get(key)) is not type(value) or receipt[key] != value for key, value in expected.items()):
        raise ValueError()
    if subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=root, text=True).strip() != expected['candidate_sha']:
        raise ValueError()
    if hashlib.sha256((root / 'ui/package-lock.json').read_bytes()).hexdigest() != expected['package_lock_sha256']:
        raise ValueError()
except (KeyError, ValueError, OSError, subprocess.SubprocessError):
    raise SystemExit('browser preparation receipt is missing, foreign or unsuccessful')
PYREADY
            ;;
        false) prepare_browser "$browser" ;;
        *) echo 'parallel cluster option must be true or false' >&2; exit 1 ;;
    esac
    cd "$ROOT/ui"
    result=0
    # Only the isolated connector project is selected. Retry zero preserves a
    # first-attempt failure; the shared evidence collector checks every case.
    npx playwright test --project=connectors --retries=0 --workers=1 \
        --reporter=line,json --output="$browser/test-results" >"$browser/test.log" 2>&1 || result=$?
    printf '%s\n' "$result" >"$browser/exit-status"
    cat "$browser/test.log"
    exit "$result"
fi
[[ $# == 0 ]] || { echo 'usage: connectors-test.sh [browser|prepare-browser]' >&2; exit 2; }
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
import base64, json, math, re, sys
root = Path(sys.argv[1]); public = root / 'public'; public.mkdir(exist_ok=True)
allow = {
 'fixture': ('candidate-sha.txt', 'source-checksums.json', 'source-status.txt', 'server.json',
             'server-image.json', 'server-env.json', 'image-provenance.json', 'images.json', 'health.json', 'events.jsonl',
             'exit-status', 'fixture-evidence.json', 'messages-provider-evidence.json', 'correlation-evidence.json',
             'operations-component-evidence.json', 'read-http-evidence.json', 'read-correlation-http-evidence.json',
             'actions-http-evidence.json', 'actions-http-responses.json', 'invalid-startup-evidence.json',
             'test.log', 'caesium.log', 'worker.log', 'temporal-version.txt', 'server-version.txt'),
 'browser': ('candidate-sha.txt', 'server-image-id.txt', 'playwright-results.json', 'exit-status', 'test.log'),
 'browser-prep': ('status.json',),
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
            if group == 'browser-prep':
                record = json.loads(text)
                fields = ('version', 'invocation_id', 'candidate_sha', 'package_lock_sha256',
                          'status', 'exit_code', 'prep_pid', 'duration_seconds')
                if (record.get('version') != 1 or record.get('status') != 'success' or record.get('exit_code') != 0
                    or any(not isinstance(record.get(key), str) or not re.fullmatch('[0-9a-f]{'+str(size)+'}', record[key])
                           for key, size in (('invocation_id', 32), ('candidate_sha', 40), ('package_lock_sha256', 64)))
                    or type(record.get('prep_pid')) is not int or record['prep_pid'] <= 0
                    or type(record.get('duration_seconds')) not in (int, float)
                    or not math.isfinite(record['duration_seconds']) or record['duration_seconds'] < 0):
                    continue
                text = json.dumps({key: record[key] for key in fields}, indent=2)
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
parallel=${CAESIUM_CONNECTORS_PARALLEL_CLUSTER:-false}
case "$parallel" in true|false) ;; *) echo 'parallel cluster option must be true or false' >&2; exit 1 ;; esac
frozen_image_ids=
if [[ "$parallel" == true ]]; then
    # Only a producer-bound loaded cohort may overlap the two resource drivers.
    # The local source-build default obtains its handoff after the first phase.
    frozen_image_ids=$(python3 - "$candidate" <<'PYFREEZE'
import os, re, subprocess, sys
candidate = sys.argv[1]
if os.environ.get('CAESIUM_SKIP_IMAGE_BUILD') != 'true' or os.environ.get('CAESIUM_CONNECTORS_PREBUILT_FIXTURE') != 'true':
    raise SystemExit('parallel connector phases require all three loaded producer images')
if os.environ.get('CAESIUM_CONNECTORS_EXPECTED_FIXTURE_CANDIDATE_SHA') != candidate:
    raise SystemExit('parallel connector phases require the fixture producer candidate')
tag = os.environ.get('CAESIUM_CONNECTORS_TAG', 'latest')
roles = (
    ('server', 'caesiumcloud/caesium:' + tag + '-test', 'CAESIUM_CONNECTORS_EXPECTED_IMAGE_ID'),
    ('runner', os.environ.get('CAESIUM_INTEGRATION_RUNNER_IMAGE'), 'CAESIUM_CONNECTORS_EXPECTED_RUNNER_IMAGE_ID'),
    ('fixture', 'caesiumcloud/caesium-connectors-fixture:' + tag, 'CAESIUM_CONNECTORS_EXPECTED_FIXTURE_IMAGE_ID'),
)
ids = []
for role, image, name in roles:
    expected = os.environ.get(name, '')
    if not image or not re.fullmatch(r'sha256:[0-9a-f]{64}', expected):
        raise SystemExit('parallel connector phases lack immutable producer ' + role + ' identity')
    observed = subprocess.check_output(['docker', 'image', 'inspect', '--format', '{{.Id}}', image], text=True).strip()
    if observed != expected:
        raise SystemExit('parallel connector ' + role + ' tag differs from producer image')
    ids.append(observed)
print(*ids)
PYFREEZE
)
    read -r reuse_server_id reuse_runner_id reuse_fixture_id <<<"$frozen_image_ids"
    export CAESIUM_CONNECTORS_REUSE_SERVER_IMAGE_ID="$reuse_server_id"
    export CAESIUM_CONNECTORS_REUSE_RUNNER_IMAGE_ID="$reuse_runner_id"
    export CAESIUM_CONNECTORS_REUSE_FIXTURE_IMAGE_ID="$reuse_fixture_id"
    # Supervise independent process groups, including subprocesses such as
    # browser and engine clients. Every failure/signal joins all three groups.
    run_parallel_phases() {
        exec python3 - "$artifacts" <<'PYPAIR'
import hashlib, json, os, signal, subprocess, sys, time, uuid
from pathlib import Path
root = Path(sys.argv[1])
prep = root / 'browser-prep'; prep.mkdir(mode=0o700)
invocation = uuid.uuid4().hex
candidate = os.environ['CANDIDATE_SHA']
lock = hashlib.sha256(Path('ui/package-lock.json').read_bytes()).hexdigest()
read_fd, write_fd = os.pipe()
children = []
joined = {}
stopping = False
signal_result = None
shutdown_signal = None
signalled = set()
retired = set()
ready = False
prep_started = time.monotonic()
def stop(signum):
    global stopping, shutdown_signal
    stopping = True
    if shutdown_signal is None: shutdown_signal = signum
    for name, child in children:
        if child.pid in signalled or child.pid in retired: continue
        # Mark before delivery: repeated signals/finally cannot abort cleanup,
        # and a Popen that returns after interruption still receives one TERM.
        signalled.add(child.pid)
        try: os.killpg(child.pid, shutdown_signal)
        except ProcessLookupError: pass
        except PermissionError:
            if child.poll() is None or child.pid in live_groups(): raise
            retired.add(child.pid)
def interrupted(signum, _frame):
    global signal_result
    if signal_result is None: signal_result = 128 + signum
    stop(signal.SIGTERM)
def join(child):
    if child.pid not in joined: joined[child.pid] = child.wait()
    return joined[child.pid]
def live_groups():
    # An exited leader can leave a slow cleanup descendant in its owned group.
    # Ignore zombies, which have exited, and never signal any unrelated PGID.
    owned = {child.pid for _, child in children if child.pid not in retired}
    if not owned: return set()
    for attempt in range(2):
        try:
            rows = subprocess.check_output(['ps', '-axo', 'pgid=,stat='], text=True)
            break
        except subprocess.CalledProcessError as error:
            if attempt or signal_result is None or error.returncode not in (-signal.SIGHUP, -signal.SIGINT, -signal.SIGTERM):
                raise
    # Treat observed '?' states as live; they cannot prove cleanup is done.
    members = []
    for line in rows.splitlines():
        fields = line.split()
        if (len(fields) != 2 or not fields[0].isdigit()
            or not fields[1] or fields[1][0] not in 'RSDTtZXxIWUKP?'):
            raise RuntimeError('process-group inspection is inconclusive')
        members.append((int(fields[0]), fields[1]))
    if not members: raise RuntimeError('process-group inspection is inconclusive')
    return {pgid for pgid, state in members if pgid in owned and not state.startswith('Z')}
def publish(child):
    global ready, write_fd
    if subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip() != candidate:
        raise RuntimeError('candidate changed during browser preparation')
    if hashlib.sha256(Path('ui/package-lock.json').read_bytes()).hexdigest() != lock:
        raise RuntimeError('package lock changed during browser preparation')
    receipt = dict(version=1, invocation_id=invocation, candidate_sha=candidate,
                   package_lock_sha256=lock, status='success', exit_code=0,
                   prep_pid=child.pid, duration_seconds=round(time.monotonic()-prep_started, 6))
    raw = (json.dumps(receipt, sort_keys=True) + '\n').encode()
    temporary = prep / ('status-' + invocation + '.tmp')
    with temporary.open('xb') as output: output.write(raw)
    try: os.link(temporary, prep / 'status.json')
    finally: temporary.unlink()
    os.write(write_fd, hashlib.sha256(raw).hexdigest().encode() + b'\n')
    os.close(write_fd); write_fd = None
    ready = True
for signum in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
    signal.signal(signum, interrupted)
failure = 0
common = dict(CAESIUM_CONNECTORS_BROWSER_PREP_DIR=str(prep),
              CAESIUM_CONNECTORS_BROWSER_PREP_INVOCATION=invocation,
              CAESIUM_CONNECTORS_BROWSER_PREP_LOCK_SHA256=lock)
try:
    for name, command, extra in (
        ('prep', ['bash', 'scripts/connectors-test.sh', 'prepare-browser'], {}),
        ('fixture', ['sh', 'scripts/connectors-fixture.sh', 'check'], {
            'CAESIUM_CONNECTORS_ARTIFACTS': str(root / 'fixture'),
            'CAESIUM_CONNECTORS_BROWSER_PREP_FD': str(read_fd)}),
        ('cluster', ['bash', 'scripts/connectors-config-cluster.sh'], {
            'CAESIUM_CONNECTORS_REUSE_IMAGES': 'true', 'CAESIUM_CONNECTORS_CLUSTER_ARTIFACTS': str(root / 'cluster')}),
    ):
        if stopping: break
        env = dict(os.environ, **common, **extra)
        if name == 'prep':
            for key in ('CAESIUM_E2E_AUTH_ADMIN_KEY', 'CAESIUM_BASE_URL'):
                env.pop(key, None)
        with (root / (name + '.log')).open('w') as log:
            child = subprocess.Popen(command, env=env, stdout=log, stderr=subprocess.STDOUT,
                                     start_new_session=True, pass_fds=(read_fd,) if name == 'fixture' else ())
        children.append((name, child))
        if name == 'fixture': os.close(read_fd); read_fd = None
        if stopping: stop(signal.SIGTERM)
    while any(child.poll() is None for _, child in children) or (not ready and not stopping):
        for name, child in children:
            result = child.poll()
            if result and not failure:
                failure = result if result > 0 else 128-result
                stop(signal.SIGTERM)
        if not stopping:
            prepared = next(child for name, child in children if name == 'prep')
            if not ready and prepared.poll() == 0 and prepared.pid not in live_groups():
                join(prepared)
                retired.add(prepared.pid)
                publish(prepared)
        time.sleep(.05)
finally:
    stop(signal.SIGTERM)
    for fd in (read_fd, write_fd):
        if fd is not None: os.close(fd)
    results = [join(child) for _, child in children]
    while live_groups(): time.sleep(.05)
if signal_result: raise SystemExit(signal_result)
raise SystemExit(failure or next((value if value > 0 else 128-value for value in results if value), 0) or (0 if ready else 1))
PYPAIR
    }
    run_parallel_phases &
else
    CAESIUM_CONNECTORS_ARTIFACTS="$artifacts/fixture" run_owned sh scripts/connectors-fixture.sh check \
        >"$artifacts/fixture.log" 2>&1 &
fi
child_pid=$!
result=0
wait "$child_pid" || result=$?
child_pid=
if [[ "$result" != 0 ]]; then
    for log in fixture cluster; do [[ ! -f "$artifacts/$log.log" ]] || cat "$artifacts/$log.log"; done
    exit "$result"
fi
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
                            ('runner', 'CAESIUM_CONNECTORS_EXPECTED_RUNNER_IMAGE_ID'),
                            ('fixture', 'CAESIUM_CONNECTORS_EXPECTED_FIXTURE_IMAGE_ID')):
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
    if role == 'fixture' and value['origin'] == 'loaded' and value.get('producer_candidate_sha') != candidate:
        raise SystemExit('fixture image handoff differs from fixture producer candidate')
    ids[role] = observed
fixture = images[0].get('Id')
if not isinstance(fixture, str) or not re.fullmatch(r'sha256:[0-9a-f]{64}', fixture):
    raise SystemExit('fixture image handoff lacks immutable provider identity')
if fixture != ids['fixture']:
    raise SystemExit('fixture provider runtime and image identities differ')
if images[1].get('Id') != ids['runner']:
    raise SystemExit('fixture runner runtime and image identities differ')
print(ids['server'], ids['runner'], fixture)
PYHANDOFF
)
read -r reuse_server_id reuse_runner_id reuse_fixture_id <<<"$image_ids"
if [[ "$parallel" == true ]]; then
    [[ "$image_ids" == "$frozen_image_ids" ]] || { echo 'runtime image handoff differs from frozen producer cohort' >&2; exit 1; }
else
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
fi
[[ "$(git rev-parse HEAD)" == "$candidate" && -z "$(git status --porcelain)" ]] || {
    echo 'candidate changed during connector qualification' >&2; exit 1;
}
python3 scripts/collect-lane-evidence.py connectors --artifacts "$artifacts" \
    --candidate-sha "$candidate" --out "$artifacts/evidence.json"
python3 scripts/check-test-evidence.py --manifest test/contracts/scenarios.json \
    --report "$artifacts/evidence.json" --require connectors --strict
printf 'Connector lane evidence: %s\n' "$artifacts/evidence.json"
