"""Exercise connector-lane ordering and public export without real resources."""
import base64
import json
import os
from pathlib import Path
import shutil
import signal
import time
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class ConnectorLaneDriverTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='connectors-lane-probe-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / 'scripts').mkdir()
        (self.root / 'bin').mkdir()
        (self.root / 'ui').mkdir()
        shutil.copyfile(ROOT / 'scripts/connectors-test.sh', self.root / 'scripts/connectors-test.sh')
        (self.root / 'scripts/connectors-test.sh').chmod(0o755)
        (self.root / '.gitignore').write_text('artifacts/\n')
        fake = self.root / 'bin/tool'
        fake.write_text('''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
name=Path(sys.argv[0]).name
root=Path(os.environ['PROBE_ROOT'])
with (root/'artifacts/calls').open('a') as out: out.write(json.dumps([name,*sys.argv[1:]])+'\\n')
if name=='npx' and 'test' in sys.argv:
    Path(os.environ['PLAYWRIGHT_JSON_OUTPUT_NAME']).write_text(json.dumps({'stats':{'expected':10},'error':'csk_browser_private','attachments':[{'contentType':'application/json','body':'eyJrZXkiOiJjc2tfYnJvd3Nlcl9wcml2YXRlIn0='}]}))
    raise SystemExit(int(os.environ.get('BROWSER_EXIT','0')))
''')
        fake.chmod(0o755)
        for name in ('just', 'docker', 'node', 'npm', 'npx', 'kind', 'kubectl', 'helm'):
            (self.root / 'bin' / name).symlink_to('tool')
        (self.root / 'scripts/connectors-fixture.sh').write_text('''#!/bin/sh
set -eu
mkdir "$CAESIUM_CONNECTORS_ARTIFACTS"
if [ "${PROBE_BLOCK_FIXTURE:-false}" = true ]; then
    trap 'printf cleaned >"$CAESIUM_CONNECTORS_ARTIFACTS/cleaned"; exit 143' TERM
    touch "$CAESIUM_CONNECTORS_ARTIFACTS/started"
    while :; do sleep 1; done
fi
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
proof = {'candidate_sha':sha, 'server':{'origin':'source-build','expected_image_id':'','observed_image_id':server},
         'runner':{'origin':'source-build','expected_image_id':'','observed_image_id':runner}}
images = [{'Id':fixture}, {'Id':runner}]
if os.environ.get('PROBE_HANDOFF_FAILURE') == 'runner': images[1]['Id'] = 'sha256:'+'d'*64
(root/'image-provenance.json').write_text(json.dumps(proof))
(root/'images.json').write_text(json.dumps(images))
PYPHASE
CAESIUM_CONNECTORS_BROWSER_PHASE=true CAESIUM_CONNECTORS_CANDIDATE_SHA="$CANDIDATE_SHA" \\
CAESIUM_E2E_AUTH_ADMIN_KEY=csk_browser_private CAESIUM_BASE_URL=http://fixture \\
"$CAESIUM_CONNECTORS_BROWSER_HOOK"
''')
        (self.root / 'scripts/connectors-config-cluster.sh').write_text('''#!/bin/sh
set -eu
mkdir "$CAESIUM_CONNECTORS_CLUSTER_ARTIFACTS"
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
