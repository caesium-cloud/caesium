"""Exercise fixture orchestration with an atomic mock engine, never Docker."""
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import tempfile
import time
import unittest

ROOT = Path(__file__).resolve().parents[1]

ENGINE = r'''#!/usr/bin/env python3
import json, os, signal, sys, time
from pathlib import Path
args=sys.argv[1:]
state=Path(os.environ['MOCK_STATE'])
with (state/'calls.jsonl').open('a') as out: out.write(json.dumps(args)+'\n')
def option(name): return args[args.index(name)+1]
def role(cid): return {'1'*64:'server','2'*64:'fixture','3'*64:'runner','4'*64:'gateoff','5'*64:'invalid','6'*64:'anchor','f'*64:'foreign'}.get(cid,'unknown')
if args[0]=='version':
 print(json.dumps({'Platform':{'Name':os.environ.get('MOCK_ENGINE','Docker Engine - Community')},'Os':'linux','Components':[{'Name':'Engine'}]}))
 if os.environ.get('MOCK_VERSION_FAIL'): sys.exit(1)
elif args[0]=='info':
 print(os.environ.get('MOCK_SECURITY_OPTIONS','["name=seccomp,profile=builtin"]'))
 if os.environ.get('MOCK_INFO_FAIL'): sys.exit(1)
elif args[:2]==['container','inspect'] or args[0]=='inspect':
 target=args[-1]
 cid=target if len(target)==64 else ('f'*64 if (state/'names'/target).exists() else '')
 if not cid: sys.exit(1)
 if '--format' in args:
  restarts=(state/'server-restarts').read_text() if (state/'server-restarts').exists() else '0'
  print({'{{.Id}}':cid,'{{.Image}}':'sha256:'+'a'*64,'{{.State.Running}}':'true','{{.State.StartedAt}}':'2026-10-15T00:00:0'+restarts+'Z'}[option('--format')])
 else:
  container_state={'Running':True,'Error':'state-credential','Health':{'Log':[{'Output':'health-credential'}]}}
  if role(cid)=='invalid':
   container_state={'Running':False,'OOMKilled':False,'ExitCode':int(os.environ.get('MOCK_INVALID_EXIT','1')),'StartedAt':'2026-10-15T00:00:00Z','FinishedAt':'2026-10-15T00:00:01Z'}
   if os.environ.get('MOCK_INVALID_MISSING_STATE'): container_state.pop('ExitCode')
  print(json.dumps([{'Id':cid,'Image':'sha256:'+'a'*64,'Config':{'Env':['CAESIUM_AUTH_MODE='+os.environ.get('MOCK_AUTH_MODE','api-key'),'CAESIUM_CONNECTORS_CONFIG_FILE=/etc/caesium/connectors/connections.yaml','CAESIUM_CONNECTORS_ENABLED='+os.environ.get('MOCK_GATE_ENABLED','true'),'CAESIUM_RUN_QUEUE_ENABLED='+os.environ.get('MOCK_QUEUE_ENABLED','true'),'CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED='+os.environ.get('MOCK_DEQUEUER_ENABLED','true'),'SAMPLE_SECRET=private','ARBITRARY=unusual-credential','CAESIUM_REGISTRY_AUTH=secret://env/ARBITRARY','CAESIUM_DATABASE_DSN=postgres://u:password@db','CAESIUM_IT_REGISTRY_CREDS=ci-user:ci-pass'],'Cmd':['--auth=argv-credential']},'Args':['argv-credential'],'State':container_state}]))
elif args[:2]==['image','inspect']:
 print(json.dumps([{'Id':args[-1],'Config':{'Env':['LABELLESS=image-credential'],'Cmd':['argv-credential'],'Labels':{'x':'label-credential'}}}]))
 if os.environ.get('MOCK_IMAGE_INSPECT_FAIL'): sys.exit(1)
elif args[0]=='create':
 name=option('--name'); reservation=state/'names'/name
 try:
  fd=os.open(reservation,os.O_CREAT|os.O_EXCL|os.O_WRONLY);os.close(fd)
 except FileExistsError: sys.exit(17)
 kind='fixture' if 'fixture-' in name else 'runner' if 'runner-' in name else 'gateoff' if 'gateoff-' in name else 'invalid' if 'invalid-startup-' in name else 'anchor' if 'network-anchor-' in name else 'server'
 cid={'server':'1','fixture':'2','runner':'3','gateoff':'4','invalid':'5','anchor':'6'}[kind]*64
 Path(option('--cidfile')).write_text(cid+'\n')
 print(cid,flush=True)
 if os.environ.get('MOCK_SIGNAL_AFTER_CREATE')==kind:
  os.killpg(os.getpgrp(),signal.SIGTERM);time.sleep(1)
elif args[0]=='start':
 cid=args[-1]
 if role(cid)=='invalid': (state/'invalid-started').touch()
 if os.environ.get('MOCK_FAIL_START')==role(cid): sys.exit(21)
 if '-a' in args:
  (state/'runner-attached').touch()
  if os.environ.get('MOCK_BLOCK_RUNNER'):
   while not (state/'release').exists(): time.sleep(.02)
  for name in os.environ.get('MOCK_SCENARIOS','Fixture').split(','):
   if name: print('    --- PASS: TestIntegrationTestSuite/TestConnectors'+name+' (0.01s)')
  if os.environ.get('MOCK_SKIPPED_SCENARIO'):
   print('    --- SKIP: TestIntegrationTestSuite/TestConnectors'+os.environ['MOCK_SKIPPED_SCENARIO']+'/nested (0.01s)')
 else: print(cid)
elif args[0]=='restart':
 if os.environ.get('MOCK_FAIL_RESTART'): sys.exit(21)
 path=state/'server-restarts'; path.write_text(str(int(path.read_text() if path.exists() else '0')+1))
elif args[0]=='wait':
 if os.environ.get('MOCK_INVALID_WAIT_FAIL'): sys.exit(21)
 print(os.environ.get('MOCK_INVALID_EXIT','1'))
elif args[0]=='logs':
 if role(args[-1])=='invalid':
  print('startup failed' if os.environ.get('MOCK_INVALID_WRONG_ERROR') else 'yaml: field inlineToken not found in type connector.fileCredentials')
  if os.environ.get('MOCK_INVALID_LOG_CANARY'): print('fixture-invalid-inline-credential-canary')
 else: print('bootstrap csk_live_fixture_bootstrap')
elif args[0]=='exec':
 if args[-1].endswith('/evidence'):
  if os.environ.get('MOCK_INVALID_MISSING_RPC_PROOF'): print('{"ready":true}')
  else: print(json.dumps([{'kind':'provider_rpc'}] if os.environ.get('MOCK_INVALID_RPC') and (state/'invalid-started').exists() else []))
 else: print('{"ready":true,"sdk":"1.49.0","cli":"1.9.1","server":"1.32.0"}')
elif args[0]=='cp': Path(args[-1]).write_text('fixture CLI')
elif args[0] in ['build','rm']: pass
else: raise SystemExit('unexpected mock engine command: '+repr(args))
'''

JUST = r'''#!/usr/bin/env python3
import os, re, subprocess, sys
from pathlib import Path
state=Path(os.environ['MOCK_STATE'])
recipe='integration-up-connectors' if 'integration-up-connectors' in sys.argv else 'integration-up-agent'
if recipe not in sys.argv:
 if os.environ.get('MOCK_RACE_NAME'): (state/'names'/'caesium-server-agent-test').touch()
 raise SystemExit(0)
text=Path('justfile').read_text().split(recipe+':',1)[1].splitlines()[1:]
lines=[]
for line in text:
 if line and not line.startswith(' '): break
 lines.append(line.strip().removeprefix('@'))
values={'container_cli':os.environ['CAESIUM_CONTAINER_CLI'],'agent_it_container':'caesium-server-agent-test','connectors_it_container':'caesium-server-connectors-test','platform':'linux/amd64','port':'8080','sock':'/var/run/docker.sock','repo_dir':str(Path.cwd()),'local_image_ref':'caesiumcloud/caesium','tag':'probe','event_ingest_api_key':'fixture-event','agent_api_external_url':'http://fixture','contract_deprecation_window':'5s'}
body=re.sub(r'{{\s*(\w+)\s*}}',lambda m:values[m.group(1)],'\n'.join(lines))
raise SystemExit(subprocess.run(['sh','-ec',body]).returncode)
'''


class ConnectorFixtureOrchestrationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='ec-fixture-probe-')
        self.root = Path(self.temp.name)
        (self.root/'scripts').mkdir()
        shutil.copy(ROOT/'scripts/connectors-fixture.sh', self.root/'scripts/connectors-fixture.sh')
        shutil.copy(ROOT/'scripts/connectors-fixture-metadata.py', self.root/'scripts/connectors-fixture-metadata.py')
        shutil.copy(ROOT/'scripts/connectors-fixture-control.py', self.root/'scripts/connectors-fixture-control.py')
        shutil.copy(ROOT/'justfile', self.root/'justfile')
        # integration-up-agent seeds the auth lane's policy file from this fixture.
        (self.root/'test/testdata/policy').mkdir(parents=True)
        shutil.copy(ROOT/'test/testdata/policy/two-namespaces.yaml', self.root/'test/testdata/policy/two-namespaces.yaml')
        self.state = self.root/'mock'
        (self.state/'names').mkdir(parents=True)
        self.bin = self.root/'bin'
        self.bin.mkdir()
        for name, source in [('engine', ENGINE), ('just', JUST)]:
            path = self.bin/name
            path.write_text(source)
            path.chmod(0o755)
        self.artifacts = self.root/'artifacts'
        self.git('init', '-q')
        self.git('add', 'scripts/connectors-fixture.sh', 'justfile')
        self.git('-c', 'user.name=Fixture Probe', '-c', 'user.email=fixture@example.test',
                 '-c', 'commit.gpgsign=false', 'commit', '-qm', 'probe base')

    def tearDown(self):
        self.temp.cleanup()

    def git(self, *args):
        subprocess.run(['git', *args], cwd=self.root, check=True, capture_output=True)

    def env(self, **extra):
        env = dict(os.environ)
        env.update(PATH=str(self.bin)+os.pathsep+os.environ['PATH'], MOCK_STATE=str(self.state),
                   CAESIUM_PODMAN='false', CAESIUM_CONTAINER_CLI=str(self.bin/'engine'), CAESIUM_PLATFORM='linux/amd64',
                   CAESIUM_CONNECTORS_TAG='probe', CAESIUM_CONNECTORS_ARTIFACTS=str(self.artifacts),
                   CAESIUM_CONNECTORS_EXISTING_SERVER='false',
                   CAESIUM_API_KEY='host-private-key', CAESIUM_AUTH_ADMIN_KEY='host-private-admin')
        env.update(extra)
        return env

    def launch(self, **extra):
        # Ubuntu's dash is exercised when available; other hosts use /bin/sh.
        shell = shutil.which('dash') or '/bin/sh'
        return subprocess.Popen([shell, 'scripts/connectors-fixture.sh', 'check'],
                                cwd=self.root, env=self.env(**extra), start_new_session=True,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)

    def finish(self, process):
        stdout, stderr = process.communicate(timeout=15)
        return process.returncode, stdout, stderr

    def calls(self):
        path = self.state/'calls.jsonl'
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def removed(self):
        return [call[-1] for call in self.calls() if call[0] == 'rm']

    def select_reads(self):
        (self.root/'test'/'connectors'/'testdata').mkdir(parents=True, exist_ok=True)
        (self.root/'test'/'connectors_read_test.go').touch()
        shutil.copy(ROOT/'test/connectors/testdata/connections.yaml',
                    self.root/'test/connectors/testdata/connections.yaml')

    def test_loaded_precompiled_runner_is_reused_without_source_rebuild(self):
        result, out, err = self.finish(self.launch(
            CAESIUM_INTEGRATION_RUNNER_IMAGE='caesiumcloud/caesium-integration:candidate'))
        self.assertEqual(result, 0, out + err)
        runner = next(call for call in self.calls() if call[0] == 'create' and
                      'caesium-connectors-runner-' in call[call.index('--name')+1])
        self.assertIn('caesiumcloud/caesium-integration:candidate', runner)
        self.assertFalse(any(call[0] == 'build' and 'build/Dockerfile.integration' in call for call in self.calls()))

    def test_connector_recipe_refusal_preserves_existing_policy_and_cid(self):
        policy = self.root/'.tmp/connectors-auth-policy/policy.yaml'
        policy.parent.mkdir(parents=True)
        policy.write_text('foreign mounted policy sentinel')
        baseline = self.root/'test/testdata/policy/two-namespaces.yaml'
        baseline_before = baseline.read_bytes()
        cid = self.root/'foreign-server-cid'
        cid.write_text('foreign container identity')
        result = subprocess.run([str(self.bin/'just'), 'integration-up-connectors'], cwd=self.root,
            env=self.env(CAESIUM_CONNECTORS_SERVER_CIDFILE=str(cid)), capture_output=True, text=True, timeout=10)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('server CID file already exists', result.stderr)
        self.assertEqual(policy.read_text(), 'foreign mounted policy sentinel')
        self.assertEqual(baseline.read_bytes(), baseline_before)
        self.assertEqual(cid.read_text(), 'foreign container identity')
        self.assertFalse(self.calls())

    def select_lane(self):
        self.select_reads()
        for component in ('fixture', 'messages', 'correlation', 'operations', 'actions'):
            (self.root/'test'/f'connectors_{component}_test.go').touch()
        return {'CAESIUM_CONNECTORS_LANE': 'true',
                'MOCK_SCENARIOS': 'Fixture,Messages,Correlation,Operations,Read,Actions'}

    def test_lane_uses_tracked_connector_recipe_and_observes_flags(self):
        result, out, err = self.finish(self.launch(**self.select_lane()))
        self.assertEqual(result, 0, out + err)
        server = next(call for call in self.calls() if call[0] == 'create' and
                      call[call.index('--name')+1] == 'caesium-server-connectors-test')
        self.assertIn('CAESIUM_CONNECTORS_ENABLED=true', server)
        self.assertIn('CAESIUM_RUN_QUEUE_ENABLED=true', server)
        self.assertIn('CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED=true', server)
        observed = json.loads((self.artifacts/'server-env.json').read_text())
        self.assertEqual(observed['CAESIUM_AUTH_MODE'], 'api-key')
        self.assertNotIn('SAMPLE_SECRET', observed)

    def test_lane_missing_scenario_refuses_before_creation(self):
        lane = self.select_lane()
        (self.root/'test'/'connectors_operations_test.go').unlink()
        result, out, err = self.finish(self.launch(**lane))
        self.assertNotEqual(result, 0)
        self.assertIn('missing operations scenario', err)
        self.assertFalse(any(call[0] == 'create' for call in self.calls()))

    def test_lane_observed_gate_queue_and_auth_misconfiguration_refuse(self):
        for setting, value in (('MOCK_GATE_ENABLED', 'false'), ('MOCK_QUEUE_ENABLED', 'false'),
                               ('MOCK_DEQUEUER_ENABLED', 'false'), ('MOCK_AUTH_MODE', 'none')):
            with self.subTest(setting=setting):
                # Each probe owns a fresh resource namespace/artifact directory.
                self.tearDown(); self.setUp()
                result, out, err = self.finish(self.launch(**self.select_lane(), **{setting: value}))
                self.assertNotEqual(result, 0)
                self.assertFalse((self.state/'runner-attached').exists())
                self.assertEqual(set(self.removed()), {'1'*64, '6'*64})

    def test_lane_loaded_runner_requires_producer_identity(self):
        result, out, err = self.finish(self.launch(**self.select_lane(),
            CAESIUM_INTEGRATION_RUNNER_IMAGE='caesiumcloud/caesium-integration:candidate'))
        self.assertNotEqual(result, 0)
        self.assertIn('requires producer image identity', err)
        self.assertFalse(any(call[0] == 'create' for call in self.calls()))

    def test_lane_wrong_runner_identity_refuses_before_dispatch(self):
        result, out, err = self.finish(self.launch(**self.select_lane(),
            CAESIUM_INTEGRATION_RUNNER_IMAGE='caesiumcloud/caesium-integration:candidate',
            CAESIUM_CONNECTORS_EXPECTED_RUNNER_IMAGE_ID='sha256:'+'b'*64))
        self.assertNotEqual(result, 0)
        self.assertIn('runner image differs', err)
        self.assertFalse((self.state/'runner-attached').exists())

    def test_lane_loaded_runtime_identity_matches_producer(self):
        result, out, err = self.finish(self.launch(**self.select_lane(),
            CAESIUM_SKIP_IMAGE_BUILD='true',
            CAESIUM_CONNECTORS_EXPECTED_IMAGE_ID='sha256:'+'a'*64,
            CAESIUM_INTEGRATION_RUNNER_IMAGE='caesiumcloud/caesium-integration:candidate',
            CAESIUM_CONNECTORS_EXPECTED_RUNNER_IMAGE_ID='sha256:'+'a'*64))
        self.assertEqual(result, 0, out + err)
        provenance = json.loads((self.artifacts/'image-provenance.json').read_text())
        for role in ('server', 'runner'):
            self.assertEqual(provenance[role]['origin'], 'loaded')
            self.assertEqual(provenance[role]['expected_image_id'], provenance[role]['observed_image_id'])

    def test_lane_wrong_loaded_image_refuses_before_dispatch(self):
        result, out, err = self.finish(self.launch(**self.select_lane(),
            CAESIUM_CONNECTORS_EXPECTED_IMAGE_ID='sha256:'+'b'*64))
        self.assertNotEqual(result, 0)
        self.assertIn('differs from loaded candidate', err)
        self.assertFalse((self.state/'runner-attached').exists())

    def test_duplicate_selected_pass_refuses(self):
        result, out, err = self.finish(self.launch(MOCK_SCENARIOS='Fixture,Fixture'))
        self.assertNotEqual(result, 0)
        self.assertIn('did not pass exactly once', err)

    def wait_path(self, path, seconds=10):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            if path.exists():
                return
            time.sleep(.02)
        self.fail(f'fixture never produced {path}')

    def test_http_selection_requires_real_config_restart_and_gateoff_owned_ids(self):
        self.select_reads()
        process = self.launch(MOCK_SCENARIOS='Fixture,Read', MOCK_BLOCK_RUNNER='true')
        try:
            self.wait_path(self.state/'runner-attached')
            request = {'id': 'bcb00000-0000-4000-8000-000000000001', 'action': 'restart'}
            (self.artifacts/'control'/'request.json').write_text(json.dumps(request))
            response_path = self.artifacts/'control'/'response.json'
            self.wait_path(response_path)
            response = json.loads(response_path.read_text())
            self.assertEqual(response['id'], request['id'])
            self.assertEqual(response['status'], 'ok')
            self.assertNotEqual(response['started_before'], response['started_after'])
            self.assertEqual([c for c in self.calls() if c[0] == 'restart'], [['restart', '1'*64]])
            (self.state/'release').touch()
            code, _, error = self.finish(process)
            self.assertEqual(code, 0, error)
        finally:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGTERM)
                process.communicate(timeout=10)
        self.assertCountEqual(self.removed(), ['1'*64, '2'*64, '3'*64, '4'*64, '5'*64, '6'*64])
        self.assertEqual(self.removed()[-1], '6'*64, 'network namespace owner must be removed last')
        startup = json.loads((self.artifacts/'invalid-startup-evidence.json').read_text())
        self.assertEqual(startup['candidate_sha'], (self.artifacts/'candidate-sha.txt').read_text().strip())
        self.assertEqual(startup['container_id'], '5'*64)
        self.assertEqual(startup['exit_code'], 1)
        self.assertEqual(startup['wait_exit_code'], 1)
        self.assertEqual(startup['provider_rpc_before'], startup['provider_rpc_after'])
        self.assertTrue(startup['log_canary_absent'])
        self.assertTrue(startup['strict_parser_error_observed'])
        self.assertEqual(startup['sdk'], '1.49.0')
        self.assertNotIn('fixture-invalid-inline-credential-canary', (self.artifacts/'invalid-startup.log').read_text())
        config = (self.artifacts/'http-connections.yaml').read_text()
        self.assertIn('maxUnreferencedSnapshots: 4', config)
        self.assertIn('unreferencedSnapshotMaxAge: 10s', config)
        calls = [c for c in self.calls() if c[0] == 'create']
        fixture = next(c for c in calls if 'fixture-' in c[c.index('--name')+1])
        self.assertIn('CAESIUM_CONNECTORS_CONTROL_LISTEN=127.0.0.1:7240', fixture)
        server = next(c for c in calls if c[c.index('--name')+1] == 'caesium-server-agent-test')
        self.assertIn(str(self.artifacts)+'/http-connections.yaml:/etc/caesium/connectors/connections.yaml:ro', server)
        self.assertIn(str(self.artifacts)+':/artifacts:ro', server)
        self.assertEqual(server[server.index('--entrypoint')+1], '/artifacts/control/start-server.sh')
        wrapper = self.artifacts/'control'/'start-server.sh'
        self.assertTrue(os.access(wrapper, os.X_OK))
        self.assertIn('CAESIUM_CONNECTORS_CONFIG_PREVIOUS_FINGERPRINT', wrapper.read_text())
        anchor = next(c for c in calls if 'network-anchor-' in c[c.index('--name')+1])
        self.assertEqual(anchor[anchor.index('-p')+1], '8080:8080')
        self.assertEqual(anchor[anchor.index('--user')+1], f'{os.getuid()}:{os.getgid()}')
        self.assertIn('USER=caesium-fixture', anchor)
        self.assertIn('HOME=/tmp', anchor)
        self.assertEqual(anchor[-3:], ['busybox:1.36.1', 'sleep', '2147483647'])
        for call in calls:
            if call != anchor:
                self.assertIn('--network=container:'+'6'*64, call)
                self.assertNotIn('-p', call, 'only the stable namespace owner publishes the port')
            if any(part in call[call.index('--name')+1] for part in ('invalid-startup-', 'gateoff-')):
                self.assertEqual(call[call.index('--entrypoint')+1], '/bin/caesium')
        runner = next(c for c in calls if 'caesium-connectors-runner-' in c[c.index('--name')+1])
        self.assertIn('CAESIUM_CONNECTORS_READ_FIXTURE=true', runner)
        self.assertIn('CAESIUM_CONNECTORS_OWNED_SERVER=true', runner)
        self.assertIn('CAESIUM_CONNECTORS_GATE_OFF_URL=http://127.0.0.1:8081', runner)
        self.assertEqual(runner[-1], 'TestIntegrationTestSuite/(TestConnectorsFixture|TestConnectorsRead)')

    def test_actions_selection_requires_named_pass_and_rejects_nested_skip(self):
        self.select_reads()
        (self.root/'test'/'connectors_actions_test.go').touch()
        for extra, expected in (({'MOCK_SCENARIOS': 'Fixture,Read,Actions'}, 0),
                                ({'MOCK_SCENARIOS': 'Fixture,Read'}, 1),
                                ({'MOCK_SCENARIOS': 'Fixture,Read,Actions', 'MOCK_SKIPPED_SCENARIO': 'Actions'}, 1)):
            with self.subTest(results=extra):
                for child in (self.artifacts, self.state):
                    if child.exists(): shutil.rmtree(child)
                (self.state/'names').mkdir(parents=True)
                code, _, error = self.finish(self.launch(**extra))
                self.assertEqual(code == 0, expected == 0, error)
                runner = next(c for c in self.calls() if c[0] == 'create' and 'runner-' in c[c.index('--name')+1])
                self.assertIn('CAESIUM_CONNECTORS_ACTIONS_FIXTURE=true', runner)
                self.assertIn('TestConnectorsActions', runner[-1])

    def test_manual_browser_hook_receives_live_owned_fixture_and_failure_fails_run(self):
        self.select_reads()
        hook = self.root/'browser hook.py'
        hook.write_text("#!/usr/bin/env python3\nimport json,os\nfrom pathlib import Path\n"
                        "assert os.environ['CAESIUM_AUTH_ADMIN_KEY'] == 'csk_live_fixture_bootstrap'\n"
                        "assert os.environ['CAESIUM_E2E_AUTH_ADMIN_KEY'] == os.environ['CAESIUM_API_KEY']\n"
                        "assert os.environ['CAESIUM_CONNECTORS_FIXTURE'] == 'true'\n"
                        "assert os.environ['CAESIUM_CONNECTORS_FIXTURE_CONTAINER_ID'] == '2'*64\n"
                        "assert os.environ['CAESIUM_CONNECTORS_SERVER_CONTAINER_ID'] == '1'*64\n"
                        "assert os.environ['CAESIUM_BASE_URL'] == 'http://127.0.0.1:8080'\n"
                        "assert os.environ['CAESIUM_CONNECTORS_FIXTURE_CONTROL_URL'] == 'http://127.0.0.1:7240'\n"
                        "assert os.environ['CAESIUM_CONNECTORS_GATE_OFF_URL'] == 'http://127.0.0.1:8081'\n"
                        "assert not Path(os.environ['MOCK_STATE'],'hook-before').exists()\n"
                        "Path(os.environ['MOCK_STATE'],'hook-ran').touch()\n"
                        "raise SystemExit(int(os.environ.get('MOCK_HOOK_FAIL','0')))\n")
        hook.chmod(0o755)
        for failure in ('0', '9'):
            with self.subTest(failure=failure):
                for child in (self.artifacts, self.state):
                    if child.exists(): shutil.rmtree(child)
                (self.state/'names').mkdir(parents=True)
                code, output, error = self.finish(self.launch(MOCK_SCENARIOS='Fixture,Read', CAESIUM_CONNECTORS_BROWSER_HOOK=str(hook), MOCK_HOOK_FAIL=failure))
                self.assertEqual(code, int(failure), error)
                self.assertTrue((self.state/'hook-ran').exists())
                anchor = next(c for c in self.calls() if c[0] == 'create' and 'network-anchor-' in c[c.index('--name')+1])
                self.assertIn('127.0.0.1:7240:7240', anchor)
                self.assertIn('127.0.0.1:8081:8081', anchor)
                fixture = next(c for c in self.calls() if c[0] == 'create' and 'fixture-' in c[c.index('--name')+1])
                self.assertIn('CAESIUM_CONNECTORS_CONTROL_LISTEN=0.0.0.0:7240', fixture)
                self.assertNotIn('csk_live_fixture_bootstrap', output+error)
                self.assertCountEqual(self.removed(), ['1'*64, '2'*64, '3'*64, '4'*64, '5'*64, '6'*64])

    def test_browser_hook_is_not_a_shell_command_and_never_runs_after_missing_go_pass(self):
        self.select_reads()
        hook = self.root/'browser-hook'
        hook.write_text('#!/bin/sh\ntouch "$MOCK_STATE/hook-ran"\n')
        hook.chmod(0o755)
        code, _, _ = self.finish(self.launch(MOCK_SCENARIOS='Fixture', CAESIUM_CONNECTORS_BROWSER_HOOK=str(hook)))
        self.assertNotEqual(code, 0)
        self.assertFalse((self.state/'hook-ran').exists())
        for command in ('relative-hook', str(hook)+'; touch '+str(self.state/'injected')):
            with self.subTest(command=command):
                if self.artifacts.exists(): shutil.rmtree(self.artifacts)
                code, _, error = self.finish(self.launch(CAESIUM_CONNECTORS_BROWSER_HOOK=command))
                self.assertNotEqual(code, 0)
                self.assertIn('absolute executable file', error)
                self.assertFalse((self.state/'injected').exists())

    def test_http_qualification_refuses_borrowed_server_before_mutation(self):
        self.select_reads()
        code, _, error = self.finish(self.launch(CAESIUM_CONNECTORS_EXISTING_SERVER='true'))
        self.assertNotEqual(code, 0)
        self.assertIn('requires an owned server', error)
        self.assertFalse(any(c[0] in ('create', 'build', 'restart', 'rm') for c in self.calls()))

    def test_http_missing_or_skipped_read_scenario_cannot_qualify(self):
        self.select_reads()
        for extra in ({'MOCK_SCENARIOS': 'Fixture'},
                      {'MOCK_SCENARIOS': 'Fixture,Read', 'MOCK_SKIPPED_SCENARIO': 'Read'}):
            with self.subTest(results=extra):
                for child in (self.artifacts, self.state):
                    if child.exists(): shutil.rmtree(child)
                (self.state/'names').mkdir(parents=True)
                code, _, error = self.finish(self.launch(**extra))
                self.assertNotEqual(code, 0)
                self.assertIn('scenario', error)
                self.assertCountEqual(self.removed(), ['1'*64, '2'*64, '3'*64, '4'*64, '5'*64, '6'*64])

    def test_http_gateoff_creation_is_preserved_for_owned_cleanup_on_signal(self):
        self.select_reads()
        code, _, _ = self.finish(self.launch(MOCK_SIGNAL_AFTER_CREATE='gateoff'))
        self.assertNotEqual(code, 0)
        self.assertCountEqual(self.removed(), ['1'*64, '2'*64, '4'*64, '5'*64, '6'*64])

    def test_invalid_enabled_startup_requires_exact_failure_redaction_and_zero_rpc(self):
        self.select_reads()
        for extra in ({'MOCK_INVALID_EXIT': '0'}, {'MOCK_INVALID_EXIT': '137'},
                      {'MOCK_INVALID_WAIT_FAIL': 'true'}, {'MOCK_INVALID_MISSING_STATE': 'true'},
                      {'MOCK_INVALID_WRONG_ERROR': 'true'}, {'MOCK_INVALID_LOG_CANARY': 'true'},
                      {'MOCK_INVALID_RPC': 'true'}, {'MOCK_INVALID_MISSING_RPC_PROOF': 'true'}):
            with self.subTest(failure=extra):
                for child in (self.artifacts, self.state):
                    if child.exists(): shutil.rmtree(child)
                (self.state/'names').mkdir(parents=True)
                code, _, _ = self.finish(self.launch(**extra))
                self.assertNotEqual(code, 0)
                self.assertFalse((self.artifacts/'invalid-startup-evidence.json').exists())
                self.assertCountEqual(self.removed(), ['1'*64, '2'*64, '5'*64, '6'*64])
                self.assertEqual(self.removed()[-1], '6'*64)
                self.assertFalse(any(c[0] == 'create' and ('runner-' in c[c.index('--name')+1] or
                                                         'gateoff-' in c[c.index('--name')+1])
                                     for c in self.calls()))
                log = self.artifacts/'invalid-startup.log'
                if log.exists():
                    self.assertNotIn('fixture-invalid-inline-credential-canary', log.read_text())

    def test_invalid_enabled_startup_cleanup_uses_owned_cid_after_create_signal(self):
        self.select_reads()
        code, _, _ = self.finish(self.launch(MOCK_SIGNAL_AFTER_CREATE='invalid'))
        self.assertNotEqual(code, 0)
        self.assertCountEqual(self.removed(), ['1'*64, '2'*64, '5'*64, '6'*64])

    def test_anchor_creation_and_start_failures_cleanup_only_owned_anchor(self):
        self.select_reads()
        for extra in ({'MOCK_SIGNAL_AFTER_CREATE': 'anchor'}, {'MOCK_FAIL_START': 'anchor'}):
            with self.subTest(failure=extra):
                for child in (self.artifacts, self.state):
                    if child.exists(): shutil.rmtree(child)
                (self.state/'names').mkdir(parents=True)
                code, _, _ = self.finish(self.launch(**extra))
                self.assertNotEqual(code, 0)
                self.assertEqual(self.removed(), ['6'*64])
                self.assertFalse(any(c[0] == 'restart' for c in self.calls()))

    def test_restart_failure_returns_failure_evidence_and_never_uses_a_name(self):
        self.select_reads()
        process = self.launch(MOCK_SCENARIOS='Fixture,Read', MOCK_BLOCK_RUNNER='true', MOCK_FAIL_RESTART='true')
        try:
            self.wait_path(self.state/'runner-attached')
            request = {'id': 'bcb00000-0000-4000-8000-000000000002', 'action': 'restart'}
            (self.artifacts/'control'/'request.json').write_text(json.dumps(request))
            response_path = self.artifacts/'control'/'response.json'
            self.wait_path(response_path)
            response = json.loads(response_path.read_text())
            self.assertEqual(response['status'], 'error')
            self.assertNotIn('started_after', response)
            self.assertEqual([c for c in self.calls() if c[0] == 'restart'], [['restart', '1'*64]])
            (self.state/'release').touch()
            self.assertEqual(self.finish(process)[0], 0)
        finally:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGTERM)
                process.communicate(timeout=10)

    def test_owned_run_ignores_host_keys_records_source_and_server_and_host_user(self):
        (self.root/'justfile').write_text((self.root/'justfile').read_text()+'\n# staged probe\n')
        self.git('add', 'justfile')
        untracked = self.root/'untracked source.txt'
        untracked.write_text('source outside fixture')
        code, _, error = self.finish(self.launch())
        self.assertEqual(code, 0, error)
        calls = self.calls()
        containers = [call for call in calls if call[0] == 'create']
        self.assertEqual(len(containers), 3)
        runner = next(call for call in containers if 'caesium-connectors-runner-' in call[call.index('--name')+1])
        self.assertIn('CAESIUM_API_KEY=csk_live_fixture_bootstrap', runner)
        self.assertNotIn('host-private', json.dumps(calls))
        for call in containers[1:]:
            self.assertEqual(call[call.index('--user')+1], f'{os.getuid()}:{os.getgid()}')
            self.assertIn('USER=caesium-fixture', call)
            self.assertIn('HOME=/tmp', call)
        self.assertCountEqual(self.removed(), ['1'*64, '2'*64, '3'*64])
        self.assertIn('staged probe', (self.artifacts/'source-diff.patch').read_text())
        manifest = json.loads((self.artifacts/'source-checksums.json').read_text())
        entry = next(row for row in manifest if row['path'] == untracked.name)
        self.assertEqual(entry['sha256'], hashlib.sha256(untracked.read_bytes()).hexdigest())
        server = json.loads((self.artifacts/'server.json').read_text())[0]
        self.assertEqual(server['Id'], '1'*64)
        self.assertIn('SAMPLE_SECRET', server['Config']['EnvNames'])
        self.assertNotIn('Env', server['Config'])
        self.assertTrue((self.artifacts/'server-image.json').exists())

    def test_unsupported_engine_modes_refused_before_build_or_creation(self):
        for extra in ({'CAESIUM_PODMAN': 'true'}, {'MOCK_ENGINE': 'Podman Engine'},
                      {'MOCK_ENGINE': 'unknown'}, {'MOCK_SECURITY_OPTIONS': '["name=rootless"]'},
                      {'MOCK_SECURITY_OPTIONS': '["name=userns"]'}, {'MOCK_SECURITY_OPTIONS': 'null'},
                      {'MOCK_VERSION_FAIL': 'true'}, {'MOCK_INFO_FAIL': 'true'}):
            with self.subTest(mode=extra):
                for child in (self.artifacts, self.state):
                    if child.exists(): shutil.rmtree(child)
                (self.state/'names').mkdir(parents=True)
                code, _, error = self.finish(self.launch(**extra))
                self.assertNotEqual(code, 0)
                self.assertIn('requires rootful', error)
                self.assertTrue(all(call[0] in ('version', 'info') for call in self.calls()))

    def test_all_present_component_scenarios_are_selected_and_required(self):
        (self.root/'test').mkdir(exist_ok=True)
        for component in ('messages', 'correlation', 'operations'):
            (self.root/'test'/f'connectors_{component}_test.go').touch()
        code, _, error = self.finish(self.launch(MOCK_SCENARIOS='Fixture,Messages,Correlation,Operations'))
        self.assertEqual(code, 0, error)
        runner = next(call for call in self.calls() if call[0] == 'create' and
                      'caesium-connectors-runner-' in call[call.index('--name')+1])
        self.assertEqual(runner[-1], 'TestIntegrationTestSuite/(TestConnectorsFixture|TestConnectorsMessages|TestConnectorsCorrelation|TestConnectorsOperations)')

    def test_missing_or_skipped_component_cannot_satisfy_qualification(self):
        (self.root/'test').mkdir(exist_ok=True)
        (self.root/'test'/'connectors_operations_test.go').touch()
        for extra in ({'MOCK_SCENARIOS': 'Fixture'},
                      {'MOCK_SCENARIOS': 'Fixture,Operations', 'MOCK_SKIPPED_SCENARIO': 'Operations'},
                      {'MOCK_SCENARIOS': ''}):
            with self.subTest(results=extra):
                for child in (self.artifacts, self.state):
                    if child.exists(): shutil.rmtree(child)
                (self.state/'names').mkdir(parents=True)
                code, _, error = self.finish(self.launch(**extra))
                self.assertNotEqual(code, 0)
                self.assertIn('scenario', error)
                self.assertCountEqual(self.removed(), ['1'*64, '2'*64, '3'*64])

    def test_owned_and_borrowed_inspection_never_retain_config_values_or_free_text(self):
        for borrowed in (False, True):
            with self.subTest(borrowed=borrowed):
                for child in (self.artifacts, self.state):
                    if child.exists(): shutil.rmtree(child)
                (self.state/'names').mkdir(parents=True)
                (self.state/'names'/'borrowed-server').touch()
                extra = {'CAESIUM_CONNECTORS_EXISTING_SERVER': 'true',
                         'CAESIUM_CONNECTORS_SERVER_CONTAINER': 'borrowed-server'} if borrowed else {}
                code, _, error = self.finish(self.launch(**extra))
                self.assertEqual(code, 0, error)
                for name in ('server.json', 'server-image.json', 'images.json'):
                    raw = (self.artifacts/name).read_text()
                    for secret in ('unusual-credential', 'secret://env/', 'postgres://', 'ci-user:ci-pass',
                                   'argv-credential', 'image-credential', 'label-credential',
                                   'state-credential', 'health-credential'):
                        self.assertNotIn(secret, raw, name)
                    self.assertTrue(json.loads(raw)[0]['Id'])
                server = json.loads((self.artifacts/'server.json').read_text())[0]
                self.assertTrue(server['State']['Running'])
                self.assertIn('CAESIUM_IT_REGISTRY_CREDS', server['Config']['EnvNames'])
                if borrowed:
                    self.assertNotIn('f'*64, self.removed())

    def test_failed_inspection_cannot_publish_plausible_image_evidence(self):
        code, _, _ = self.finish(self.launch(MOCK_IMAGE_INSPECT_FAIL='true'))
        self.assertNotEqual(code, 0)
        self.assertEqual(self.removed(), ['1'*64])
        self.assertFalse((self.artifacts/'server-image.json').exists())

    def test_foreign_name_created_during_build_is_never_removed(self):
        code, _, _ = self.finish(self.launch(MOCK_RACE_NAME='true'))
        self.assertNotEqual(code, 0)
        self.assertEqual(self.removed(), [])
        self.assertTrue((self.state/'names'/'caesium-server-agent-test').exists())

    def test_start_failure_removes_only_created_id_and_preserves_server_diagnostics(self):
        code, _, _ = self.finish(self.launch(MOCK_FAIL_START='server'))
        self.assertNotEqual(code, 0)
        self.assertEqual(self.removed(), ['1'*64])
        self.assertTrue((self.artifacts/'server.json').exists())
        self.assertTrue((self.artifacts/'caesium.log').exists())

    def test_signal_immediately_after_create_uses_cid_file_without_assigned_id(self):
        code, _, _ = self.finish(self.launch(MOCK_SIGNAL_AFTER_CREATE='server'))
        self.assertNotEqual(code, 0)
        self.assertEqual(self.removed(), ['1'*64])
        self.assertTrue((self.artifacts/'caesium.log').exists())

    def test_signal_exits_collect_diagnostics_and_clean_all_created_ids(self):
        for sig in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
            with self.subTest(signal=sig):
                # Each invocation requires a fresh evidence directory and state.
                for child in (self.artifacts, self.state):
                    if child.exists(): shutil.rmtree(child)
                (self.state/'names').mkdir(parents=True)
                process = self.launch(MOCK_BLOCK_RUNNER='true')
                deadline = time.monotonic()+10
                while not (self.state/'runner-attached').exists() and time.monotonic() < deadline:
                    time.sleep(.02)
                self.assertTrue((self.state/'runner-attached').exists())
                os.killpg(process.pid, sig)
                code, _, _ = self.finish(process)
                self.assertEqual(code, 128+sig)
                self.assertCountEqual(self.removed(), ['1'*64, '2'*64, '3'*64])
                self.assertTrue((self.artifacts/'container.log').exists())
                self.assertTrue((self.artifacts/'caesium.log').exists())

    def test_nonempty_artifacts_rejected_before_any_engine_calls(self):
        for filename in ('events.jsonl', 'server-container-id', 'fixture-container-id', 'runner-container-id'):
            with self.subTest(filename=filename):
                if self.artifacts.exists(): shutil.rmtree(self.artifacts)
                self.artifacts.mkdir()
                (self.artifacts/filename).write_text('stale')
                code, _, _ = self.finish(self.launch())
                self.assertNotEqual(code, 0)
                self.assertEqual(self.calls(), [])
                self.assertEqual((self.artifacts/filename).read_text(), 'stale')

    def test_existing_server_records_identity_but_never_removes_borrowed_id(self):
        (self.state/'names'/'borrowed-server').touch()
        code, _, error = self.finish(self.launch(CAESIUM_CONNECTORS_EXISTING_SERVER='true',
                                                CAESIUM_CONNECTORS_SERVER_CONTAINER='borrowed-server'))
        self.assertEqual(code, 0, error)
        self.assertNotIn('f'*64, self.removed())
        server = json.loads((self.artifacts/'server.json').read_text())[0]
        self.assertEqual(server['Id'], 'f'*64)
        self.assertTrue((self.artifacts/'server-image.json').exists())


if __name__ == '__main__':
    unittest.main()
