"""Exercise connector-lane ordering and public export without real resources."""
import ast
import base64
import hashlib
import json
import os
from pathlib import Path
import shutil
import signal
import time
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]


class ConnectorLaneDriverTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='connectors-lane-probe-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / 'scripts').mkdir()
        (self.root / 'bin').mkdir()
        (self.root / 'ui').mkdir()
        (self.root / 'ui/package-lock.json').write_text('{"lockfileVersion":3}\n')
        shutil.copyfile(ROOT / 'scripts/connectors-test.sh', self.root / 'scripts/connectors-test.sh')
        (self.root / 'scripts/connectors-test.sh').chmod(0o755)
        (self.root / '.gitignore').write_text('artifacts/\n')
        fake = self.root / 'bin/tool'
        fake.write_text('''#!/usr/bin/env python3
import json, os, signal, subprocess, sys, time
from pathlib import Path
name=Path(sys.argv[0]).name
root=Path(os.environ['PROBE_ROOT'])
with (root/'artifacts/calls').open('a') as out: out.write(json.dumps([name,*sys.argv[1:]])+'\\n')
def wait_file(path):
    end=time.monotonic()+5
    while not path.exists() and time.monotonic()<end: time.sleep(.01)
    assert path.exists(), str(path)
if name=='npm':
    (root/'artifacts/prep-pid').write_text(str(os.getpid()))
    if os.environ.get('PROBE_ASSERT_PREP_CI') == 'true': assert os.environ.get('CI') == 'true'
    assert 'CAESIUM_E2E_AUTH_ADMIN_KEY' not in os.environ or os.environ.get('CAESIUM_CONNECTORS_PARALLEL_CLUSTER') != 'true'
    if os.environ.get('PROBE_PREP_WAIT_FOR_PHASES') == 'true':
        for phase in ('fixture','cluster'): wait_file(root/'artifacts/lane'/phase/'started')
    if os.environ.get('PROBE_PREP_WAIT_FOR_BROWSER') == 'true':
        wait_file(root/'artifacts/lane/browser/candidate-sha.txt')
    if os.environ.get('PROBE_PREP_DRAIN_ORPHAN') == 'true':
        subprocess.Popen([sys.executable,'-c',
            'import os,signal,time; from pathlib import Path; p=Path(os.environ["PROBE_ROOT"])/"artifacts"\\n'
            'def stop(*_):\\n signal.signal(signal.SIGTERM,signal.SIG_DFL); time.sleep(.7); (p/"prep-drain-cleaned").touch(); raise SystemExit(0)\\n'
            'signal.signal(signal.SIGTERM,stop); (p/"prep-drain-started").touch()\\n'
            'while True: time.sleep(.05)'])
        wait_file(root/'artifacts/prep-drain-started')
    if os.environ.get('PROBE_BLOCK_PREP') == 'true':
        descendant=subprocess.Popen([sys.executable,'-c',
            'import os,signal,time; from pathlib import Path; p=Path(os.environ["PROBE_ROOT"])/"artifacts"\\n'
            'def stop(*_): (p/"prep-descendant-cleaned").touch(); raise SystemExit(0)\\n'
            'signal.signal(signal.SIGTERM,stop); (p/"prep-descendant-started").touch()\\n'
            'while True: time.sleep(.05)'])
        def stop(*_):
            signal.signal(signal.SIGTERM,signal.SIG_DFL)
            descendant.wait()
            if os.environ.get('PROBE_SLOW_PREP_CLEANUP') == 'true': time.sleep(.3)
            (root/'artifacts/prep-cleaned').touch()
            raise SystemExit(143)
        signal.signal(signal.SIGTERM,stop)
        wait_file(root/'artifacts/prep-descendant-started')
        (root/'artifacts/prep-started').touch()
        while True: time.sleep(.05)
    (root/'artifacts/prep-started').touch()
    if os.environ.get('PROBE_PREP_RELEASE') == 'true': wait_file(root/'artifacts/prep-release')
    (root/'artifacts/prep-ci-complete').touch()
    if os.environ.get('PROBE_MUTATE_LOCK') == 'true':
        (root/'ui/package-lock.json').write_text('{"lockfileVersion":3,"foreign":true}\\n')
    raise SystemExit(int(os.environ.get('PROBE_PREP_EXIT','0')))
if name=='npx' and 'install' in sys.argv:
    (root/'artifacts/prep-install-complete').touch()
    if os.environ.get('PROBE_PREP_ORPHAN') == 'true':
        subprocess.Popen([sys.executable,'-c',
            'import os,time; from pathlib import Path; time.sleep(.3); (Path(os.environ["PROBE_ROOT"])/"artifacts/prep-orphan-complete").touch()'])
    raise SystemExit(int(os.environ.get('PROBE_INSTALL_EXIT','0')))
if name=='npx' and 'test' in sys.argv:
    assert (root/'artifacts/prep-install-complete').exists() or os.environ.get('PROBE_DIRECT_RECEIPT') == 'true'
    assert (root/'artifacts/lane/fixture/go-complete').exists() or os.environ.get('PROBE_DIRECT_RECEIPT') == 'true'
    if os.environ.get('PROBE_PREP_ORPHAN') == 'true': assert (root/'artifacts/prep-orphan-complete').exists()
    (root/'artifacts/browser-env.json').write_text(json.dumps({name:os.environ.get(name) for name in
        ('CAESIUM_CONNECTORS_EXPECTED_UI_URL','CAESIUM_MANUAL_TRIGGER_API_KEY')}))
    Path(os.environ['PLAYWRIGHT_JSON_OUTPUT_NAME']).write_text(json.dumps({'stats':{'expected':10},'error':'csk_browser_private','attachments':[{'contentType':'application/json','body':'eyJrZXkiOiJjc2tfYnJvd3Nlcl9wcml2YXRlIn0='}]}))
    raise SystemExit(int(os.environ.get('BROWSER_EXIT','0')))
if name=='docker' and sys.argv[1:3]==['image','inspect']:
    role = 'fixture' if 'connectors-fixture:' in sys.argv[-1] else 'runner' if 'caesium-integration:' in sys.argv[-1] else 'server'
    letter = dict(server='a', runner='b', fixture='c')[role]
    if os.environ.get('PROBE_FREEZE_WRONG_ROLE') == role: letter='d'
    print('sha256:'+letter*64)
''')
        fake.chmod(0o755)
        for name in ('just', 'docker', 'node', 'npm', 'npx', 'kind', 'kubectl', 'helm'):
            (self.root / 'bin' / name).symlink_to('tool')
        (self.root / 'scripts/connectors-fixture.sh').write_text('''#!/bin/sh
set -eu
mkdir "$CAESIUM_CONNECTORS_ARTIFACTS"
printf '%s\\n' "$$" >"$CAESIUM_CONNECTORS_ARTIFACTS/pid"
if [ "${PROBE_BLOCK_FIXTURE:-false}" = true ]; then
    trap 'printf cleaned >"$CAESIUM_CONNECTORS_ARTIFACTS/cleaned"; exit 143' TERM
    touch "$CAESIUM_CONNECTORS_ARTIFACTS/started"
    while :; do sleep 1; done
fi
touch "$CAESIUM_CONNECTORS_ARTIFACTS/started"
if [ "${PROBE_REQUIRE_OVERLAP:-false}" = true ] || [ "${PROBE_FAIL_FIXTURE:-false}" = true ]; then
    python3 -c 'import os,time; from pathlib import Path; p=Path(os.environ["CAESIUM_CONNECTORS_ARTIFACTS"]).parent/"cluster/started"; end=time.monotonic()+5
while not p.exists() and time.monotonic()<end: time.sleep(.01)
assert p.exists(), "cluster did not overlap fixture"'
fi
if [ "${PROBE_FAILURE_WAIT_PREP:-false}" = true ]; then
    python3 -c 'import os,time; from pathlib import Path; p=Path(os.environ["PROBE_ROOT"])/"artifacts/prep-started"; end=time.monotonic()+5
while not p.exists() and time.monotonic()<end: time.sleep(.01)
assert p.exists(), "preparation did not start before failure"'
fi
if [ "${PROBE_FAIL_FIXTURE:-false}" = true ]; then exit 23; fi
if [ "${PROBE_EXPORT_FAILURE:-false}" = true ]; then
    touch "$CAESIUM_CONNECTORS_ARTIFACTS/../public"
fi
printf '%s\\n' "[{\\"Image\\":\\"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\\"}]" >"$CAESIUM_CONNECTORS_ARTIFACTS/server.json"
printf '%s\\n' 'bootstrap csk_secret_should_not_escape' >"$CAESIUM_CONNECTORS_ARTIFACTS/caesium.log"
printf '%s\\n' 'private key must not upload' >"$CAESIUM_CONNECTORS_ARTIFACTS/tls.key"
python3 - <<'PYPHASE'
import json, os
from pathlib import Path
root = Path(os.environ['CAESIUM_CONNECTORS_ARTIFACTS'])
server, runner, fixture = ('sha256:'+letter*64 for letter in 'abc')
sha = os.environ['CANDIDATE_SHA']
if os.environ.get('PROBE_HANDOFF_FAILURE') == 'candidate': sha = 'd'*40
if os.environ.get('PROBE_COHERENT_FOREIGN') == 'true':
    server, runner, fixture = ('sha256:'+letter*64 for letter in 'def')
proof = {'candidate_sha':sha, 'server':{'origin':'source-build','expected_image_id':'','observed_image_id':server},
         'runner':{'origin':'source-build','expected_image_id':'','observed_image_id':runner},
         'fixture':{'origin':'source-build','expected_image_id':'','observed_image_id':fixture}}
if os.environ.get('CAESIUM_CONNECTORS_PREBUILT_FIXTURE') == 'true':
    proof['fixture'].update(origin='loaded', expected_image_id=fixture, producer_candidate_sha=sha)
images = [{'Id':fixture}, {'Id':runner}]
if os.environ.get('PROBE_HANDOFF_FAILURE') == 'runner': images[1]['Id'] = 'sha256:'+'d'*64
(root/'image-provenance.json').write_text(json.dumps(proof))
(root/'images.json').write_text(json.dumps(images))
PYPHASE
if [ "${PROBE_WAIT_GO_RELEASE:-false}" = true ]; then
    python3 -c 'import os,time; from pathlib import Path; p=Path(os.environ["PROBE_ROOT"])/"artifacts/go-release"; end=time.monotonic()+5
while not p.exists() and time.monotonic()<end: time.sleep(.01)
assert p.exists(), "Go proof was not released"'
fi
if [ "${PROBE_FAIL_GO_GATE:-false}" = true ]; then exit 27; fi
touch "$CAESIUM_CONNECTORS_ARTIFACTS/go-complete"
CAESIUM_CONNECTORS_BROWSER_PHASE=true CAESIUM_CONNECTORS_CANDIDATE_SHA="$CANDIDATE_SHA" \\
CAESIUM_E2E_AUTH_ADMIN_KEY=csk_browser_private CAESIUM_BASE_URL=http://fixture \\
"$CAESIUM_CONNECTORS_BROWSER_HOOK"
''')
        (self.root / 'scripts/connectors-config-cluster.sh').write_text('''#!/bin/sh
set -eu
mkdir "$CAESIUM_CONNECTORS_CLUSTER_ARTIFACTS"
printf '%s\\n' "$$" >"$CAESIUM_CONNECTORS_CLUSTER_ARTIFACTS/pid"
if [ "${PROBE_BLOCK_CLUSTER:-false}" = true ]; then
    python3 -c 'import os,signal,time; from pathlib import Path; p=Path(os.environ["CAESIUM_CONNECTORS_CLUSTER_ARTIFACTS"])
def stop(*_):
    (p/"descendant-cleaned").touch(); raise SystemExit(0)
signal.signal(signal.SIGTERM,stop); (p/"descendant-started").touch()
while True: time.sleep(.05)' &
    descendant=$!
    if [ "${PROBE_SLOW_CLUSTER_CLEANUP:-false}" = true ]; then
        trap 'trap - TERM; wait "$descendant"; sleep .3; printf cleaned >"$CAESIUM_CONNECTORS_CLUSTER_ARTIFACTS/cleaned"; exit 143' TERM
    else
        trap 'wait "$descendant"; printf cleaned >"$CAESIUM_CONNECTORS_CLUSTER_ARTIFACTS/cleaned"; exit 143' TERM
    fi
    while [ ! -f "$CAESIUM_CONNECTORS_CLUSTER_ARTIFACTS/descendant-started" ]; do sleep .01; done
    touch "$CAESIUM_CONNECTORS_CLUSTER_ARTIFACTS/started"
    while :; do sleep 1; done
fi
touch "$CAESIUM_CONNECTORS_CLUSTER_ARTIFACTS/started"
if [ "${PROBE_REQUIRE_OVERLAP:-false}" = true ] || [ "${PROBE_FAIL_CLUSTER:-false}" = true ]; then
    python3 -c 'import os,time; from pathlib import Path; p=Path(os.environ["CAESIUM_CONNECTORS_CLUSTER_ARTIFACTS"]).parent/"fixture/started"; end=time.monotonic()+5
while not p.exists() and time.monotonic()<end: time.sleep(.01)
assert p.exists(), "fixture did not overlap cluster"'
fi
if [ "${PROBE_FAILURE_WAIT_PREP:-false}" = true ]; then
    python3 -c 'import os,time; from pathlib import Path; p=Path(os.environ["PROBE_ROOT"])/"artifacts/prep-started"; end=time.monotonic()+5
while not p.exists() and time.monotonic()<end: time.sleep(.01)
assert p.exists(), "preparation did not start before failure"'
fi
if [ "${PROBE_FAIL_CLUSTER:-false}" = true ]; then exit 24; fi
python3 - <<'PYIDS'
import json, os
from pathlib import Path
ids = {role:os.environ['CAESIUM_CONNECTORS_REUSE_'+role.upper()+'_IMAGE_ID'] for role in ('server','runner','fixture')}
if ids != {role:'sha256:'+letter*64 for role, letter in zip(('server','runner','fixture'),'abc')}:
    raise SystemExit('cluster handoff identities changed')
(Path(os.environ['CAESIUM_CONNECTORS_CLUSTER_ARTIFACTS'])/'received-image-ids.json').write_text(json.dumps(ids))
PYIDS
printf 'cluster\\n' >>"$PROBE_ROOT/artifacts/order"
''')
        for name in ('collect-lane-evidence.py', 'check-test-evidence.py'):
            (self.root / 'scripts' / name).write_text('''import json, os, sys
from pathlib import Path
root=Path(os.environ['PROBE_ROOT'])
with (root/'artifacts/order').open('a') as out: out.write(Path(__file__).name+'\\n')
if '--out' in sys.argv: Path(sys.argv[sys.argv.index('--out')+1]).write_text('{}')
''')
        subprocess.run(['git', 'init', '-q'], cwd=self.root, check=True)
        subprocess.run(['git', 'add', '.'], cwd=self.root, check=True)
        subprocess.run(['git', '-c', 'user.name=Probe', '-c', 'user.email=probe@example.test',
                        '-c', 'commit.gpgsign=false', 'commit', '-qm', 'probe'], cwd=self.root, check=True)
        self.sha = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=self.root, text=True).strip()

    def run_lane(self, background=False, **extra):
        # Calls are outside the lane artifact path: a nonempty lane must fail.
        (self.root / 'artifacts').mkdir(exist_ok=True)
        env = dict(os.environ, PATH=str(self.root / 'bin')+os.pathsep+os.environ['PATH'],
                   PROBE_ROOT=str(self.root), CAESIUM_CONNECTORS_ARTIFACTS=str(self.root / 'artifacts/lane'),
                   CANDIDATE_SHA=self.sha, CAESIUM_CONTAINER_CLI='docker', CAESIUM_PODMAN='false')
        env.pop('CAESIUM_CONNECTORS_BROWSER_PHASE', None)
        env.update(extra)
        if background:
            return subprocess.Popen(['bash', 'scripts/connectors-test.sh'], cwd=self.root, env=env,
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, start_new_session=True)
        return subprocess.run(['bash', 'scripts/connectors-test.sh'], cwd=self.root,
                              env=env, capture_output=True, text=True, timeout=15)

    def test_browser_then_cluster_then_shared_strict_checker(self):
        result = self.run_lane()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        order = (self.root / 'artifacts/order').read_text().splitlines()
        self.assertEqual(order, ['cluster', 'collect-lane-evidence.py', 'check-test-evidence.py'])
        calls = [json.loads(line) for line in (self.root / 'artifacts/calls').read_text().splitlines()]
        browser = next(call for call in calls if call[:3] == ['npx', 'playwright', 'test'])
        self.assertIn('--project=connectors', browser)
        self.assertIn('--retries=0', browser)
        self.assertIn('--workers=1', browser)
        self.assertEqual(sum(call[:2] == ['npm', 'ci'] for call in calls), 1)
        self.assertEqual(sum(call[:3] == ['npx', 'playwright', 'install'] for call in calls), 1)
        self.assertIn(['npx', 'playwright', 'install', 'chromium'], calls)
        browser_env = json.loads((self.root / 'artifacts/browser-env.json').read_text())
        self.assertEqual(browser_env['CAESIUM_CONNECTORS_EXPECTED_UI_URL'], 'https://temporal-ui.example.test/console')
        self.assertEqual(browser_env['CAESIUM_MANUAL_TRIGGER_API_KEY'], 'integration-test-key')
        lane = self.root / 'artifacts/lane'
        self.assertEqual((lane / 'browser/candidate-sha.txt').read_text().strip(), self.sha)
        identities = json.loads((lane/'cluster/received-image-ids.json').read_text())
        self.assertEqual(identities, {role:'sha256:'+letter*64 for role, letter in zip(('server','runner','fixture'),'abc')})
        self.assertEqual((lane / 'public/fixture/caesium.log').read_text().strip(),
                         'bootstrap [REDACTED-API-KEY]')
        self.assertFalse((lane / 'public/fixture/tls.key').exists())
        public_browser = (lane / 'public/browser/playwright-results.json').read_text()
        self.assertIn('[REDACTED-API-KEY]', public_browser)
        self.assertNotIn('csk_browser_private', public_browser)
        body = json.loads(public_browser)['attachments'][0]['body']
        self.assertNotIn('csk_browser_private', base64.b64decode(body).decode())

    def test_foreign_firstphase_candidate_stops_cluster(self):
        result = self.run_lane(PROBE_HANDOFF_FAILURE='candidate')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('lacks candidate-bound identities', result.stderr)
        self.assertFalse((self.root/'artifacts/order').exists())

    def test_inconsistent_firstphase_runner_stops_cluster(self):
        result = self.run_lane(PROBE_HANDOFF_FAILURE='runner')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('runtime and image identities differ', result.stderr)
        self.assertFalse((self.root/'artifacts/order').exists())

    def test_firstphase_image_must_agree_with_loaded_producer(self):
        result = self.run_lane(CAESIUM_CONNECTORS_EXPECTED_IMAGE_ID='sha256:'+'d'*64)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('differs from producer server identity', result.stderr)
        self.assertFalse((self.root/'artifacts/order').exists())

    def parallel_options(self):
        return dict(CAESIUM_CONNECTORS_PARALLEL_CLUSTER='true', CAESIUM_SKIP_IMAGE_BUILD='true',
                    CAESIUM_CONNECTORS_PREBUILT_FIXTURE='true',
                    CAESIUM_CONNECTORS_EXPECTED_FIXTURE_CANDIDATE_SHA=self.sha,
                    CAESIUM_CONNECTORS_EXPECTED_IMAGE_ID='sha256:'+'a'*64,
                    CAESIUM_INTEGRATION_RUNNER_IMAGE='caesiumcloud/caesium-integration:candidate',
                    CAESIUM_CONNECTORS_EXPECTED_RUNNER_IMAGE_ID='sha256:'+'b'*64,
                    CAESIUM_CONNECTORS_EXPECTED_FIXTURE_IMAGE_ID='sha256:'+'c'*64)

    def test_loaded_cohort_overlaps_and_joins_both_before_strict_collection(self):
        result = self.run_lane(**self.parallel_options(), PROBE_REQUIRE_OVERLAP='true',
                               CAESIUM_CONNECTORS_BROWSER_INSTALL_DEPS='true')
        self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
        calls = [json.loads(line) for line in (self.root/'artifacts/calls').read_text().splitlines()]
        self.assertEqual(sum(call[:3] == ['docker', 'image', 'inspect'] for call in calls), 3)
        self.assertEqual(sum(call[:2] == ['npm', 'ci'] for call in calls), 1)
        self.assertEqual(sum(call[:3] == ['npx', 'playwright', 'install'] for call in calls), 1)
        self.assertIn(['npx', 'playwright', 'install', '--with-deps', 'chromium'], calls)
        self.assertEqual((self.root/'artifacts/order').read_text().splitlines(),
                         ['cluster', 'collect-lane-evidence.py', 'check-test-evidence.py'])

    def test_parallel_requires_every_loaded_identity_and_matching_candidate(self):
        for field, value in (('CAESIUM_SKIP_IMAGE_BUILD', 'false'),
                             ('CAESIUM_CONNECTORS_PREBUILT_FIXTURE', 'false'),
                             ('CAESIUM_CONNECTORS_EXPECTED_FIXTURE_CANDIDATE_SHA', 'f'*40),
                             ('CAESIUM_CONNECTORS_EXPECTED_FIXTURE_IMAGE_ID', ''),
                             ('CAESIUM_CONNECTORS_EXPECTED_RUNNER_IMAGE_ID', 'invalid')):
            with self.subTest(field=field):
                self.setUp()
                options = self.parallel_options(); options[field] = value
                result = self.run_lane(**options)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse((self.root/'artifacts/lane/fixture').exists())
                self.assertFalse((self.root/'artifacts/lane/cluster').exists())

    def test_changed_fixture_tag_refuses_before_either_parallel_driver(self):
        result = self.run_lane(**self.parallel_options(), PROBE_FREEZE_WRONG_ROLE='fixture')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('fixture tag differs from producer image', result.stderr)
        self.assertFalse((self.root/'artifacts/lane/fixture').exists())
        self.assertFalse((self.root/'artifacts/lane/cluster').exists())

    def test_coherent_retag_after_freeze_fails_late_runtime_handoff(self):
        result = self.run_lane(**self.parallel_options(), PROBE_COHERENT_FOREIGN='true')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('differs from producer server identity', result.stderr)
        self.assertTrue((self.root/'artifacts/lane/cluster/started').exists())
        self.assertEqual((self.root/'artifacts/order').read_text().splitlines(), ['cluster'])

    def wait_path(self, path, process):
        deadline = time.monotonic()+5
        while not path.exists() and process.poll() is None and time.monotonic()<deadline: time.sleep(.01)
        self.assertTrue(path.exists(), str(path))

    def calls(self):
        path = self.root/'artifacts/calls'
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def assert_no_browser(self):
        self.assertFalse(any(call[:3] == ['npx', 'playwright', 'test'] for call in self.calls()))

    def finish_process(self, process):
        """Stop only this probe's known process groups after a failed assertion."""
        if process.poll() is None:
            process.send_signal(signal.SIGTERM)
            try:
                process.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.communicate(timeout=5)
        first_error = None
        for path in (self.root/'artifacts/prep-pid',
                     self.root/'artifacts/lane/fixture/pid',
                     self.root/'artifacts/lane/cluster/pid'):
            try:
                if not path.exists():
                    continue
                group = int(path.read_text())
                if group <= 0:
                    raise ValueError('probe cleanup requires a positive recorded process group')
                try:
                    os.killpg(group, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                except PermissionError as error:
                    # Darwin rejects a group containing only zombies. Accept
                    # that denial only after fresh, conclusive no-live proof
                    # for this recorded group; '?' still means possibly live.
                    no_live = False
                    try:
                        rows = subprocess.check_output(['ps', '-axo', 'pgid=,stat='], text=True)
                        members = [line.split() for line in rows.splitlines()]
                        valid = bool(members) and all(
                            len(row) == 2 and row[0].isdigit() and row[1]
                            and row[1][0] in 'RSDTtZXxIWUKP?' for row in members)
                        no_live = valid and not any(
                            int(row[0]) == group and not row[1].startswith('Z') for row in members)
                    except (OSError, subprocess.SubprocessError):
                        pass
                    if not no_live and first_error is None:
                        first_error = error
            except (OSError, ValueError) as error:
                if first_error is None:
                    first_error = error
        if first_error is not None:
            raise first_error

    def test_parallel_preparation_overlaps_go_and_browser_waits_once(self):
        process = self.run_lane(background=True, **self.parallel_options(),
                                PROBE_PREP_WAIT_FOR_PHASES='true', PROBE_PREP_RELEASE='true',
                                PROBE_WAIT_GO_RELEASE='true',
                                CI='false', PROBE_ASSERT_PREP_CI='true',
                                CAESIUM_CONNECTORS_BROWSER_INSTALL_DEPS='true')
        try:
            self.wait_path(self.root/'artifacts/prep-started', process)
            self.assertFalse((self.root/'artifacts/lane/fixture/go-complete').exists())
            self.assert_no_browser()
            (self.root/'artifacts/go-release').touch()
            self.wait_path(self.root/'artifacts/lane/fixture/go-complete', process)
            self.assert_no_browser()
            self.assertFalse((self.root/'artifacts/prep-install-complete').exists())
            (self.root/'artifacts/prep-release').touch()
            out, err = process.communicate(timeout=10)
            self.assertEqual(process.returncode, 0, out+err)
            calls = self.calls()
            self.assertEqual(sum(call[:2] == ['npm', 'ci'] for call in calls), 1)
            self.assertEqual(sum(call[:3] == ['npx', 'playwright', 'install'] for call in calls), 1)
            tests = [call for call in calls if call[:3] == ['npx', 'playwright', 'test']]
            self.assertEqual(len(tests), 1)
            self.assertIn('--retries=0', tests[0])
            self.assertIn('--workers=1', tests[0])
            self.assertIn(['npx', 'playwright', 'install', '--with-deps', 'chromium'], calls)
            self.assertFalse(any(path.name in ('npm.log', 'install.log')
                                 for path in (self.root/'artifacts/lane/public').rglob('*')))
        finally:
            self.finish_process(process)

    def test_prepared_browser_still_waits_for_go_completion(self):
        process = self.run_lane(background=True, **self.parallel_options(), PROBE_WAIT_GO_RELEASE='true')
        try:
            self.wait_path(self.root/'artifacts/prep-install-complete', process)
            self.wait_path(self.root/'artifacts/lane/fixture/started', process)
            self.assertFalse((self.root/'artifacts/lane/fixture/go-complete').exists())
            self.assert_no_browser()
            (self.root/'artifacts/go-release').touch()
            out, err = process.communicate(timeout=10)
            self.assertEqual(process.returncode, 0, out+err)
        finally:
            self.finish_process(process)

    def test_failed_go_gate_never_runs_prepared_browser(self):
        result = self.run_lane(**self.parallel_options(), PROBE_FAIL_GO_GATE='true')
        self.assertEqual(result.returncode, 27, result.stdout+result.stderr)
        self.assert_no_browser()
        order = self.root/'artifacts/order'
        self.assertNotIn('collect-lane-evidence.py', order.read_text() if order.exists() else '')
        self.assertNotIn('check-test-evidence.py', order.read_text() if order.exists() else '')

    def test_real_six_go_pass_guards_precede_browser_hook(self):
        source = (ROOT/'scripts/connectors-fixture.sh').read_text()
        guard = source.split('# A selected fixture scenario cannot pass via a skip or a zero-match command.\n', 1)[1]
        guard = guard.split('# The optional manual browser hook', 1)[0]
        scenarios = ['TestConnectorsActions', 'TestConnectorsCorrelation', 'TestConnectorsFixture',
                     'TestConnectorsMessages', 'TestConnectorsOperations', 'TestConnectorsRead']
        artifacts = self.root/'artifacts/actual-go-guards'
        artifacts.mkdir(parents=True)
        good = ['--- PASS: TestIntegrationTestSuite/'+name+' (0.01s)' for name in scenarios]
        variants = {'good': good, 'missing': good[:-1], 'skip': good+[
            '--- SKIP: TestIntegrationTestSuite/'+scenarios[-1]+' (0.00s)'], 'fail': good+[
            '--- FAIL: TestIntegrationTestSuite/'+scenarios[-1]+' (0.00s)'], 'duplicate': good+[good[-1]]}
        for name, lines in variants.items():
            with self.subTest(name=name):
                marker = artifacts/'browser-ran'
                marker.unlink(missing_ok=True)
                (artifacts/'test.log').write_text('\n'.join(lines)+'\n')
                result = subprocess.run(['sh', '-c', 'set -eu\n'+guard+'touch "$artifacts/browser-ran"\n'],
                                        env=dict(os.environ, artifacts=str(artifacts), scenarios=' '.join(scenarios)),
                                        capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode == 0, name == 'good', result.stdout+result.stderr)
                self.assertEqual(marker.exists(), name == 'good')

    def test_success_waits_for_owned_preparation_descendant(self):
        result = self.run_lane(**self.parallel_options(), PROBE_PREP_ORPHAN='true')
        self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
        self.assertTrue((self.root/'artifacts/prep-orphan-complete').exists())
        self.assertEqual(sum(call[:3] == ['npx', 'playwright', 'test'] for call in self.calls()), 1)

    def test_preparation_failure_stops_and_joins_both_resource_groups(self):
        for field, code in (('PROBE_PREP_EXIT', 31), ('PROBE_INSTALL_EXIT', 32)):
            with self.subTest(field=field):
                self.setUp()
                result = self.run_lane(**self.parallel_options(), **{field:str(code)},
                                       PROBE_PREP_WAIT_FOR_PHASES='true',
                                       PROBE_BLOCK_FIXTURE='true', PROBE_BLOCK_CLUSTER='true')
                self.assertEqual(result.returncode, code, result.stdout+result.stderr)
                lane = self.root/'artifacts/lane'
                for marker in ('fixture/cleaned', 'cluster/cleaned', 'cluster/descendant-cleaned'):
                    self.assertTrue((lane/marker).exists(), marker)
                self.assert_no_browser()
                self.assertFalse((self.root/'artifacts/order').exists())

    def test_preparation_failure_does_not_leave_browser_waiting_for_receipt(self):
        result = self.run_lane(**self.parallel_options(), PROBE_PREP_WAIT_FOR_BROWSER='true',
                               PROBE_PREP_EXIT='31', PROBE_BLOCK_CLUSTER='true')
        self.assertEqual(result.returncode, 31, result.stdout+result.stderr)
        lane = self.root/'artifacts/lane'
        self.assertTrue((lane/'browser/candidate-sha.txt').exists(), 'real browser entry reached its barrier')
        self.assertFalse((lane/'browser/exit-status').exists())
        self.assertFalse((lane/'browser/playwright-results.json').exists())
        self.assertTrue((lane/'cluster/cleaned').exists())
        self.assert_no_browser()

    def test_changed_lock_after_launch_rejects_before_publishing_readiness(self):
        result = self.run_lane(**self.parallel_options(), PROBE_MUTATE_LOCK='true')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('package lock changed during browser preparation', result.stderr)
        self.assertFalse((self.root/'artifacts/lane/browser-prep/status.json').exists())
        self.assert_no_browser()

    def test_resource_failure_joins_slow_preparation_and_its_descendant(self):
        for failed in ('FIXTURE', 'CLUSTER'):
            with self.subTest(failed=failed):
                self.setUp()
                other = 'CLUSTER' if failed == 'FIXTURE' else 'FIXTURE'
                result = self.run_lane(**self.parallel_options(), PROBE_BLOCK_PREP='true',
                                       PROBE_SLOW_PREP_CLEANUP='true', PROBE_PREP_WAIT_FOR_PHASES='true',
                                       PROBE_FAILURE_WAIT_PREP='true',
                                       **{'PROBE_FAIL_'+failed:'true', 'PROBE_BLOCK_'+other:'true'})
                self.assertEqual(result.returncode, 23 if failed == 'FIXTURE' else 24,
                                 result.stdout+result.stderr)
                self.assertTrue((self.root/'artifacts/prep-cleaned').exists())
                self.assertTrue((self.root/'artifacts/prep-descendant-cleaned').exists())
                self.assertTrue((self.root/'artifacts/lane'/other.lower()/'cleaned').exists())
                self.assert_no_browser()
                self.assertFalse((self.root/'artifacts/order').exists())

    def test_all_parent_signals_join_three_groups_and_preserve_unrelated_process(self):
        for signum in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
            with self.subTest(signum=signum):
                self.setUp()
                unrelated = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(30)'],
                                             start_new_session=True)
                process = self.run_lane(background=True, **self.parallel_options(),
                                        PROBE_BLOCK_PREP='true', PROBE_BLOCK_FIXTURE='true',
                                        PROBE_BLOCK_CLUSTER='true', PROBE_SLOW_PREP_CLEANUP='true',
                                        PROBE_SLOW_CLUSTER_CLEANUP='true')
                try:
                    for marker in ('prep-started', 'prep-descendant-started',
                                   'lane/fixture/started', 'lane/cluster/started'):
                        self.wait_path(self.root/'artifacts'/marker, process)
                    process.send_signal(signum)
                    out, err = process.communicate(timeout=10)
                    self.assertEqual(process.returncode, 128+signum, out+err)
                    for marker in ('prep-cleaned', 'prep-descendant-cleaned', 'lane/fixture/cleaned',
                                   'lane/cluster/cleaned', 'lane/cluster/descendant-cleaned'):
                        self.assertTrue((self.root/'artifacts'/marker).exists(), marker)
                    self.assertIsNone(unrelated.poll())
                    self.assert_no_browser()
                    self.assertFalse((self.root/'artifacts/order').exists())
                finally:
                    self.finish_process(process)
                    unrelated.terminate()
                    unrelated.wait(timeout=5)

    def test_late_third_child_registration_is_cancelled_and_joined(self):
        site = self.root/'artifacts/python-startup'
        site.mkdir(parents=True)
        (site/'sitecustomize.py').write_text('''import os, signal, subprocess, time
from pathlib import Path
original = subprocess.Popen
def popen(*args, **kwargs):
    child = original(*args, **kwargs)
    command = args[0] if args else kwargs.get('args', [])
    if isinstance(command, (list, tuple)) and any(str(x).endswith('scripts/connectors-config-cluster.sh') for x in command):
        root = Path(os.environ['PROBE_ROOT'])/'artifacts'
        end = time.monotonic()+5
        markers = [root/'prep-started', root/'lane/fixture/started', root/'lane/cluster/started']
        while not all(x.exists() for x in markers) and time.monotonic()<end: time.sleep(.01)
        assert all(x.exists() for x in markers), 'third child did not start'
        (root/'late-popen-signal').touch()
        os.kill(os.getpid(), signal.SIGTERM)
    return child
subprocess.Popen = popen
''')
        result = self.run_lane(**self.parallel_options(), PYTHONPATH=str(site),
                               PROBE_BLOCK_PREP='true', PROBE_BLOCK_FIXTURE='true',
                               PROBE_BLOCK_CLUSTER='true', PROBE_SLOW_PREP_CLEANUP='true',
                               PROBE_SLOW_CLUSTER_CLEANUP='true')
        self.assertTrue((self.root/'artifacts/late-popen-signal').exists())
        self.assertEqual(result.returncode, 143, result.stdout+result.stderr)
        for marker in ('prep-cleaned', 'prep-descendant-cleaned', 'lane/fixture/cleaned',
                       'lane/cluster/cleaned', 'lane/cluster/descendant-cleaned'):
            self.assertTrue((self.root/'artifacts'/marker).exists(), marker)
        self.assert_no_browser()
        self.assertFalse((self.root/'artifacts/order').exists())

    def browser_with_receipt(self, mutation='valid'):
        """Exercise the real browser entry using only a test-owned readiness pipe."""
        artifacts = self.root/'artifacts'
        artifacts.mkdir(exist_ok=True)
        lane = artifacts/'lane'
        fixture, prep = lane/'fixture', lane/'browser-prep'
        fixture.mkdir(parents=True)
        prep.mkdir()
        (fixture/'server.json').write_text(json.dumps([{'Image':'sha256:'+'a'*64}]))
        lock = hashlib.sha256((self.root/'ui/package-lock.json').read_bytes()).hexdigest()
        invocation = '1'*32
        record = dict(version=1, invocation_id=invocation, candidate_sha=self.sha,
                      package_lock_sha256=lock, status='success', exit_code=0,
                      prep_pid=os.getpid(), duration_seconds=0.01)
        updates = {
            'stale-invocation':dict(invocation_id='2'*32),
            'foreign-candidate':dict(candidate_sha='d'*40),
            'foreign-lock':dict(package_lock_sha256='e'*64),
            'failed':dict(status='failed', exit_code=31),
            'success-nonzero':dict(exit_code=31),
            'false-exit':dict(exit_code=False),
            'false-version':dict(version=True),
            'future-version':dict(version=2),
        }
        record.update(updates.get(mutation, {}))
        raw = (json.dumps(record, sort_keys=True)+'\n').encode()
        if mutation == 'partial': raw = raw[:len(raw)//2]
        if mutation == 'malformed': raw = b'not json\n'
        target = prep/'status.json'
        if mutation != 'missing': target.write_bytes(raw)
        if mutation == 'symlink-status':
            target.rename(artifacts/'foreign-status.json')
            target.symlink_to(artifacts/'foreign-status.json')
        if mutation == 'symlink-prep':
            prep.rename(lane/'foreign-prep')
            prep.symlink_to(lane/'foreign-prep', target_is_directory=True)
        if mutation == 'changed-lock':
            (self.root/'ui/package-lock.json').write_text('{"lockfileVersion":3,"changed":true}\n')
        digest = hashlib.sha256(raw).hexdigest()
        payload = (digest+'\n').encode()
        if mutation == 'foreign-digest': payload = ('f'*64+'\n').encode()
        if mutation == 'empty-pipe': payload = b''
        if mutation == 'partial-pipe': payload = digest[:32].encode()
        if mutation == 'extra-pipe': payload += b'extra\n'
        read_fd, write_fd = os.pipe()
        try:
            os.write(write_fd, payload)
        finally:
            os.close(write_fd)
        if mutation == 'regular-file-fd':
            os.close(read_fd)
            payload_path = artifacts/'pipe-payload'
            payload_path.write_bytes(payload)
            read_fd = os.open(payload_path, os.O_RDONLY)
        env = dict(os.environ, PATH=str(self.root/'bin')+os.pathsep+os.environ['PATH'],
                   PROBE_ROOT=str(self.root), PROBE_DIRECT_RECEIPT='true',
                   CAESIUM_CONNECTORS_ARTIFACTS=str(fixture), CAESIUM_CONNECTORS_CANDIDATE_SHA=self.sha,
                   CAESIUM_E2E_AUTH_ADMIN_KEY='csk_test', CAESIUM_BASE_URL='http://fixture',
                   CAESIUM_CONNECTORS_PARALLEL_CLUSTER='true',
                   CAESIUM_CONNECTORS_BROWSER_PREP_DIR=str(prep),
                   CAESIUM_CONNECTORS_BROWSER_PREP_INVOCATION=invocation,
                   CAESIUM_CONNECTORS_BROWSER_PREP_LOCK_SHA256=lock,
                   CAESIUM_CONNECTORS_BROWSER_PREP_FD=str(read_fd))
        if mutation == 'missing-fd': env.pop('CAESIUM_CONNECTORS_BROWSER_PREP_FD')
        if mutation == 'candidate-env': env['CAESIUM_CONNECTORS_CANDIDATE_SHA'] = 'd'*40
        if mutation == 'lock-env': env['CAESIUM_CONNECTORS_BROWSER_PREP_LOCK_SHA256'] = 'e'*64
        if mutation == 'invocation-env': env['CAESIUM_CONNECTORS_BROWSER_PREP_INVOCATION'] = '2'*32
        try:
            return subprocess.run(['bash', 'scripts/connectors-test.sh', 'browser'], cwd=self.root,
                                  env=env, pass_fds=(read_fd,), capture_output=True, text=True, timeout=5)
        finally:
            os.close(read_fd)

    def test_valid_supervisor_receipt_runs_browser_without_reinstall(self):
        result = self.browser_with_receipt()
        self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
        calls = self.calls()
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0][:3], ['npx', 'playwright', 'test'])
        self.assertIn('--retries=0', calls[0])
        self.assertIn('--workers=1', calls[0])

    def test_parallel_browser_rejects_untrusted_receipt_without_fallback(self):
        for mutation in ('missing', 'stale-invocation', 'foreign-candidate', 'foreign-lock',
                         'failed', 'success-nonzero', 'false-exit', 'false-version',
                         'future-version', 'partial', 'malformed', 'symlink-status',
                         'symlink-prep', 'changed-lock', 'foreign-digest', 'empty-pipe',
                         'partial-pipe', 'extra-pipe', 'missing-fd', 'regular-file-fd',
                         'candidate-env', 'lock-env', 'invocation-env'):
            with self.subTest(mutation=mutation):
                self.setUp()
                result = self.browser_with_receipt(mutation)
                self.assertNotEqual(result.returncode, 0, result.stdout+result.stderr)
                self.assertEqual(self.calls(), [], 'invalid receipt must neither reinstall nor run tests')
                self.assertFalse((self.root/'artifacts/lane/browser/exit-status').exists())

    def test_preparation_helper_requires_no_fixture_credentials_or_runtime(self):
        for caller_ci in (None, 'false'):
            with self.subTest(caller_ci=caller_ci):
                self.setUp()
                artifacts = self.root/'artifacts'
                prep = artifacts/'standalone-prep'
                prep.mkdir(parents=True)
                env = {k:v for k,v in os.environ.items() if not k.startswith('CAESIUM_') and k != 'CI'}
                if caller_ci is not None: env['CI'] = caller_ci
                env.update(PATH=str(self.root/'bin')+os.pathsep+os.environ['PATH'], PROBE_ROOT=str(self.root),
                           PROBE_ASSERT_PREP_CI='true', CAESIUM_CONNECTORS_BROWSER_PREP_DIR=str(prep),
                           CAESIUM_CONNECTORS_BROWSER_INSTALL_DEPS='true')
                result = subprocess.run(['bash', 'scripts/connectors-test.sh', 'prepare-browser'], cwd=self.root,
                                        env=env, capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
                self.assertEqual(self.calls(), [['npm', 'ci', '--prefer-offline'],
                                              ['npx', 'playwright', 'install', '--with-deps', 'chromium']])
                self.assertFalse((prep/'status.json').exists(), 'only the supervisor may attest preparation')
                self.assertFalse((artifacts/'lane').exists())

    def prepare_with_ps_probe(self, interrupt, descendant=False, persistent=False):
        artifacts = self.root/'artifacts'
        prep = artifacts/'ps-probe-prep'
        site = artifacts/'ps-probe-startup'
        prep.mkdir(parents=True)
        site.mkdir()
        (site/'sitecustomize.py').write_text('''import os, signal, subprocess, sys
from pathlib import Path
original = subprocess.Popen
def popen(*args, **kwargs):
    command = args[0] if args else kwargs.get('args', [])
    root = Path(os.environ['PROBE_ROOT'])/'artifacts'
    helper = len(sys.argv) == 4 and sys.argv[1].endswith('/ui')
    if helper and isinstance(command, (list, tuple)) and command[0] == 'ps' and (
            not (root/'ps-probe-fired').exists() or os.environ.get('PROBE_PS_PERSISTENT') == 'true'):
        (root/'ps-probe-fired').touch()
        if os.environ.get('PROBE_PS_INTERRUPT') == 'true':
            child = original([sys.executable, '-c', 'import time; time.sleep(30)'], *args[1:], **kwargs)
            os.killpg(os.getpgrp(), signal.SIGTERM)
            return child
        return original([sys.executable, '-c', 'raise SystemExit(7)'], *args[1:], **kwargs)
    return original(*args, **kwargs)
subprocess.Popen = popen
''')
        env = dict(os.environ, PATH=str(self.root/'bin')+os.pathsep+os.environ['PATH'],
                   PROBE_ROOT=str(self.root), PYTHONPATH=str(site),
                   CAESIUM_CONNECTORS_BROWSER_PREP_DIR=str(prep))
        if interrupt: env['PROBE_PS_INTERRUPT'] = 'true'
        if interrupt or descendant: env['PROBE_PREP_DRAIN_ORPHAN'] = 'true'
        if persistent: env['PROBE_PS_PERSISTENT'] = 'true'
        process = subprocess.Popen(['bash', 'scripts/connectors-test.sh', 'prepare-browser'], cwd=self.root,
                                   env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                   text=True, start_new_session=True)
        return process

    def test_interrupted_ps_still_joins_inner_preparation_cleanup(self):
        process = self.prepare_with_ps_probe(interrupt=True)
        try:
            out, err = process.communicate(timeout=10)
            self.assertTrue((self.root/'artifacts/ps-probe-fired').exists())
            self.assertTrue((self.root/'artifacts/prep-drain-started').exists())
            self.assertIn(process.returncode, (-signal.SIGTERM, 143), out+err)
            self.assertTrue((self.root/'artifacts/prep-drain-cleaned').exists(),
                            'outer helper returned while its inner cleanup was still live: '+out+err)
            self.assertNotIn('CalledProcessError', err)
            self.assertFalse((self.root/'artifacts/prep-install-complete').exists())
        finally:
            self.finish_process(process)

    def test_unrelated_ps_error_stays_failed_without_retry_or_install(self):
        process = self.prepare_with_ps_probe(interrupt=False)
        try:
            out, err = process.communicate(timeout=10)
            self.assertTrue((self.root/'artifacts/ps-probe-fired').exists())
            self.assertNotEqual(process.returncode, 0, out+err)
            self.assertIn('CalledProcessError', err)
            self.assertFalse((self.root/'artifacts/prep-install-complete').exists())
            self.assertEqual(sum(call[:2] == ['npm', 'ci'] for call in self.calls()), 1)
        finally:
            self.finish_process(process)

    def test_inspection_error_joins_slow_owned_descendant_and_preserves_failure(self):
        for persistent in (False, True):
            with self.subTest(persistent=persistent):
                self.setUp()
                process = self.prepare_with_ps_probe(interrupt=False, descendant=True,
                                                     persistent=persistent)
                try:
                    out, err = process.communicate(timeout=10)
                    self.assertTrue((self.root/'artifacts/ps-probe-fired').exists())
                    self.assertTrue((self.root/'artifacts/prep-drain-started').exists())
                    self.assertNotEqual(process.returncode, 0, out+err)
                    self.assertIn('CalledProcessError', err)
                    self.assertIn('exit status 7', err)
                    self.assertTrue((self.root/'artifacts/prep-drain-cleaned').exists(),
                                    'inspection failure returned before known inner cleanup: '+out+err)
                    self.assertFalse((self.root/'artifacts/prep-install-complete').exists())
                    self.assert_no_browser()
                    self.assertEqual(sum(call[:2] == ['npm', 'ci'] for call in self.calls()), 1)
                finally:
                    self.finish_process(process)

    def cleanup_probe(self, groups):
        probe_root = self.root/'artifacts/cleanup-contract'
        for relative, group in zip(('artifacts/prep-pid', 'artifacts/lane/fixture/pid',
                                    'artifacts/lane/cluster/pid'), groups):
            path = probe_root/relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(str(group))
        return type('CleanupProbe', (), {'root':probe_root})()

    def test_cleanup_denial_requires_no_live_proof_and_still_attempts_other_groups(self):
        groups = [501234, 501235, 501236]
        probe = self.cleanup_probe(groups)
        completed = type('CompletedProcess', (), {'poll':lambda self:0})()
        cases = (
            ('zombie', '501234 Z\n1 S\n', False),
            ('absent', '1 S\n', False),
            ('live', '501234 S\n', True),
            ('unknown-live', '501234 ?\n', True),
            ('unknown-state', '501234 !\n', True),
            ('malformed', '501234 S extra\n', True),
            ('empty', '', True),
            ('inspection-error', subprocess.CalledProcessError(7, ['ps']), True),
        )
        for name, inspection, fails in cases:
            with self.subTest(name=name):
                calls = []
                first = PermissionError(1, 'first recorded group denial')
                def killpg(group, signum):
                    calls.append((group, signum))
                    if group == groups[0]: raise first
                with patch.object(os, 'killpg', side_effect=killpg), patch.object(
                        subprocess, 'check_output', side_effect=inspection if isinstance(inspection, Exception)
                        else None, return_value=inspection):
                    if fails:
                        with self.assertRaises(PermissionError) as raised:
                            ConnectorLaneDriverTests.finish_process(probe, completed)
                        self.assertIs(raised.exception, first)
                    else:
                        ConnectorLaneDriverTests.finish_process(probe, completed)
                self.assertEqual(calls, [(group, signal.SIGKILL) for group in groups])

    def test_cleanup_of_actual_owned_zombie_groups_does_not_reap_other_children(self):
        children = [subprocess.Popen([sys.executable, '-c', 'pass'], start_new_session=True)
                    for _ in range(3)]
        try:
            groups = {child.pid for child in children}
            deadline = time.monotonic()+5
            while time.monotonic() < deadline:
                rows = subprocess.check_output(['ps', '-axo', 'pid=,pgid=,stat='], text=True)
                zombies = {int(fields[0]) for line in rows.splitlines()
                           if len(fields := line.split()) == 3 and fields[0] == fields[1]
                           and fields[2].startswith('Z')}
                if groups <= zombies: break
                time.sleep(.01)
            else:
                self.fail('owned children did not become independently verified zombie groups')
            self.assertTrue(all(child.returncode is None for child in children))
            probe = self.cleanup_probe([child.pid for child in children])
            completed = type('CompletedProcess', (), {'poll':lambda self:0})()
            ConnectorLaneDriverTests.finish_process(probe, completed)
            self.assertTrue(all(child.returncode is None for child in children),
                            'cleanup may inspect only recorded groups, never reap arbitrary children')
        finally:
            for child in children:
                if child.poll() is None: child.kill()
                child.wait(timeout=5)

    def test_permission_error_requires_no_live_owned_group(self):
        for state in ('live', 'zombie'):
            with self.subTest(state=state):
                command = 'import time; time.sleep(30)' if state == 'live' else 'pass'
                child = subprocess.Popen([sys.executable, '-c', command], start_new_session=True)
                calls = []
                def denied(group, signum):
                    calls.append((group, signum))
                    raise PermissionError(1, 'injected delivery failure to this owned group')
                class DeniedSignalOS:
                    killpg = staticmethod(denied)
                    def __getattr__(self, name):
                        return getattr(os, name)
                namespace = self.phase_definitions('PYPAIR')
                namespace.update(os=DeniedSignalOS(), children=[('fixture', child)], joined={},
                                 stopping=False, shutdown_signal=None, signal_result=None,
                                 signalled=set(), retired=set())
                try:
                    if state == 'zombie':
                        deadline = time.monotonic()+5
                        while time.monotonic() < deadline:
                            rows = subprocess.check_output(['ps', '-axo', 'pid=,pgid=,stat='], text=True)
                            members = [line.split() for line in rows.splitlines()
                                       if len(line.split()) == 3 and line.split()[1] == str(child.pid)]
                            if members and all(row[2].startswith('Z') for row in members): break
                            time.sleep(.01)
                        else:
                            self.fail('owned child did not become an unreaped zombie')
                        self.assertIsNone(child.returncode, 'test must not pre-reap the target')
                        namespace['stop'](signal.SIGTERM)
                        self.assertIn(child.pid, namespace['retired'])
                    else:
                        self.assertIsNone(child.poll())
                        with self.assertRaises(PermissionError):
                            namespace['stop'](signal.SIGTERM)
                        self.assertNotIn(child.pid, namespace['retired'])
                        self.assertIsNone(child.poll(), 'delivery failure cannot count as completed cleanup')
                    self.assertEqual(calls, [(child.pid, signal.SIGTERM)])
                finally:
                    if child.poll() is None: child.terminate()
                    child.wait(timeout=5)

    def phase_definitions(self, marker):
        source = (self.root/'scripts/connectors-test.sh').read_text()
        phase = source.split("<<'"+marker+"'\n", 1)[1].split('\n'+marker, 1)[0]
        parsed = ast.parse(phase)
        definitions = ast.Module(body=[node for node in parsed.body
                                      if isinstance(node, (ast.Import, ast.ImportFrom, ast.FunctionDef))],
                                 type_ignores=[])
        namespace = {}
        exec(compile(definitions, '<actual connector '+marker+' definitions>', 'exec'), namespace)
        return namespace

    def process_snapshot(self, marker, rows):
        child = type('OwnedChild', (), {'pid':501234, 'returncode':0, 'poll':lambda self:0})()
        class SnapshotSubprocess:
            CalledProcessError = subprocess.CalledProcessError
            @staticmethod
            def check_output(*args, **kwargs):
                return rows
        namespace = self.phase_definitions(marker)
        namespace.update(subprocess=SnapshotSubprocess(), child=child, children=[('fixture', child)],
                         retired=set(), signal_result=None, interruption=None, stopping=False,
                         shutdown_signal=None, signalled=False if marker == 'PYPREP' else set())
        return namespace, child

    def test_question_mark_process_state_is_conservatively_live(self):
        for marker in ('PYPREP', 'PYPAIR'):
            for name, rows, live in (
                    ('owned', '501234 ?\n1 S\n', True),
                    ('unrelated', '501234 Z\n501235 ?\n', False),
                    ('mixed', '501234 ?\n501234 Z\n', True)):
                with self.subTest(marker=marker, name=name):
                    namespace, child = self.process_snapshot(marker, rows)
                    result = namespace['live_group' if marker == 'PYPREP' else 'live_groups']()
                    self.assertEqual(result, live if marker == 'PYPREP' else ({child.pid} if live else set()))
            for rows in ('', 'garbled\n', '501234 !\n', '501234\n', '-1 S\n', '501234 S extra\n'):
                with self.subTest(marker=marker, invalid=rows):
                    namespace, _ = self.process_snapshot(marker, rows)
                    with self.assertRaises(RuntimeError):
                        namespace['live_group' if marker == 'PYPREP' else 'live_groups']()

    def test_question_mark_owned_member_cannot_launder_permission_denial(self):
        for marker in ('PYPREP', 'PYPAIR'):
            with self.subTest(marker=marker):
                namespace, child = self.process_snapshot(marker, '501234 ?\n1 S\n')
                calls = []
                def denied(group, signum):
                    calls.append((group, signum))
                    raise PermissionError(1, 'injected denial with an unknown-live owned member')
                namespace['os'] = type('DeniedSignalOS', (), {'killpg':staticmethod(denied)})()
                with self.assertRaises(PermissionError):
                    if marker == 'PYPREP': namespace['stop']()
                    else: namespace['stop'](signal.SIGTERM)
                self.assertEqual(calls, [(child.pid, signal.SIGTERM)])
                self.assertNotIn(child.pid, namespace['retired'])

    def test_parallel_fixture_failure_joins_cluster_and_its_descendants(self):
        result = self.run_lane(**self.parallel_options(), PROBE_FAIL_FIXTURE='true', PROBE_BLOCK_CLUSTER='true')
        self.assertEqual(result.returncode, 23, result.stdout+result.stderr)
        cluster = self.root/'artifacts/lane/cluster'
        self.assertTrue((cluster/'cleaned').exists())
        self.assertTrue((cluster/'descendant-cleaned').exists())
        self.assertFalse((self.root/'artifacts/order').exists())

    def test_parallel_cluster_failure_joins_fixture(self):
        result = self.run_lane(**self.parallel_options(), PROBE_BLOCK_FIXTURE='true', PROBE_FAIL_CLUSTER='true')
        self.assertEqual(result.returncode, 24, result.stdout+result.stderr)
        self.assertTrue((self.root/'artifacts/lane/fixture/cleaned').exists())
        self.assertFalse((self.root/'artifacts/order').exists())

    def test_parallel_first_browser_failure_stays_failed_and_joins_cluster(self):
        result = self.run_lane(**self.parallel_options(), BROWSER_EXIT='1', PROBE_BLOCK_CLUSTER='true',
                               PROBE_REQUIRE_OVERLAP='true')
        self.assertEqual(result.returncode, 1, result.stdout+result.stderr)
        self.assertEqual((self.root/'artifacts/lane/browser/exit-status').read_text().strip(), '1')
        self.assertTrue((self.root/'artifacts/lane/cluster/cleaned').exists())
        self.assertTrue((self.root/'artifacts/lane/cluster/descendant-cleaned').exists())
        self.assertFalse((self.root/'artifacts/order').exists())

    def test_parallel_signal_joins_both_cleanup_groups(self):
        process = self.run_lane(background=True, **self.parallel_options(),
                                PROBE_BLOCK_FIXTURE='true', PROBE_BLOCK_CLUSTER='true')
        try:
            self.wait_path(self.root/'artifacts/lane/fixture/started', process)
            self.wait_path(self.root/'artifacts/lane/cluster/descendant-started', process)
            process.send_signal(signal.SIGTERM)
            out, err = process.communicate(timeout=5)
            self.assertEqual(process.returncode, 143, out+err)
            self.assertTrue((self.root/'artifacts/lane/fixture/cleaned').exists())
            self.assertTrue((self.root/'artifacts/lane/cluster/cleaned').exists())
            self.assertTrue((self.root/'artifacts/lane/cluster/descendant-cleaned').exists())
            self.assertFalse((self.root/'artifacts/order').exists())
        finally:
            if process.poll() is None: process.kill(); process.communicate(timeout=5)

    def test_parallel_signal_does_not_interrupt_slower_cluster_cleanup(self):
        process = self.run_lane(background=True, **self.parallel_options(),
                                PROBE_BLOCK_FIXTURE='true', PROBE_BLOCK_CLUSTER='true',
                                PROBE_SLOW_CLUSTER_CLEANUP='true')
        try:
            self.wait_path(self.root/'artifacts/lane/fixture/started', process)
            self.wait_path(self.root/'artifacts/lane/cluster/started', process)
            process.send_signal(signal.SIGTERM)
            out, err = process.communicate(timeout=5)
            self.assertEqual(process.returncode, 143, out+err)
            self.assertTrue((self.root/'artifacts/lane/fixture/cleaned').exists())
            self.assertTrue((self.root/'artifacts/lane/cluster/cleaned').exists())
            self.assertTrue((self.root/'artifacts/lane/cluster/descendant-cleaned').exists())
            self.assertFalse((self.root/'artifacts/order').exists())
        finally:
            if process.poll() is None: process.kill(); process.communicate(timeout=5)

    def test_parallel_hup_and_interrupt_use_graceful_cleanup_and_keep_status(self):
        for signum in (signal.SIGHUP, signal.SIGINT):
            with self.subTest(signum=signum):
                self.setUp()
                process = self.run_lane(background=True, **self.parallel_options(),
                                        PROBE_BLOCK_FIXTURE='true', PROBE_BLOCK_CLUSTER='true',
                                        PROBE_SLOW_CLUSTER_CLEANUP='true')
                try:
                    self.wait_path(self.root/'artifacts/lane/fixture/started', process)
                    self.wait_path(self.root/'artifacts/lane/cluster/started', process)
                    process.send_signal(signum)
                    out, err = process.communicate(timeout=5)
                    self.assertEqual(process.returncode, 128+signum, out+err)
                    self.assertTrue((self.root/'artifacts/lane/fixture/cleaned').exists())
                    self.assertTrue((self.root/'artifacts/lane/cluster/cleaned').exists())
                    self.assertTrue((self.root/'artifacts/lane/cluster/descendant-cleaned').exists())
                    self.assertFalse((self.root/'artifacts/order').exists())
                finally:
                    if process.poll() is None: process.kill(); process.communicate(timeout=5)

    def test_first_browser_failure_stops_cluster_and_evidence(self):
        result = self.run_lane(BROWSER_EXIT='1')
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.root / 'artifacts/order').exists())
        self.assertEqual((self.root / 'artifacts/lane/exit-status').read_text().strip(), '1')

    def test_signal_forwards_to_owned_fixture_and_waits_for_cleanup(self):
        process = self.run_lane(background=True, PROBE_BLOCK_FIXTURE='true')
        try:
            started = self.root/'artifacts/lane/fixture/started'
            deadline = time.monotonic() + 5
            while not started.exists() and process.poll() is None and time.monotonic() < deadline:
                time.sleep(.01)
            self.assertTrue(started.exists())
            process.send_signal(signal.SIGTERM)
            out, err = process.communicate(timeout=5)
            self.assertEqual(process.returncode, 143, out+err)
            self.assertTrue((started.parent/'cleaned').exists())
            self.assertFalse((self.root/'artifacts/order').exists())
        finally:
            if process.poll() is None:
                process.kill(); process.communicate(timeout=5)

    def test_public_export_failure_cannot_leave_success(self):
        result = self.run_lane(PROBE_EXPORT_FAILURE='true')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('FileExistsError', result.stderr)

    def test_wrong_candidate_refuses_before_fixture(self):
        result = self.run_lane(CANDIDATE_SHA='b'*40)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('does not match HEAD', result.stderr)
        self.assertFalse((self.root / 'artifacts/calls').exists())

    def test_dirty_candidate_refuses_before_fixture(self):
        (self.root / 'ui/foreign.txt').write_text('uncommitted')
        result = self.run_lane()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('clean candidate', result.stderr)

    def test_nonempty_artifact_directory_refuses(self):
        (self.root / 'artifacts/lane').mkdir(parents=True)
        (self.root / 'artifacts/lane/stale').write_text('old')
        result = self.run_lane()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('nonempty', result.stderr)
        self.assertFalse((self.root / 'artifacts/calls').exists())


if __name__ == '__main__':
    unittest.main()
