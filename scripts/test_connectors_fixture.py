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
def role(cid): return {'1'*64:'server','2'*64:'fixture','3'*64:'runner','f'*64:'foreign'}.get(cid,'unknown')
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
  print({'{{.Id}}':cid,'{{.Image}}':'sha256:'+'a'*64,'{{.State.Running}}':'true'}[option('--format')])
 else: print(json.dumps([{'Id':cid,'Image':'sha256:'+'a'*64,'Config':{'Env':['CAESIUM_AUTH_MODE=api-key','SAMPLE_SECRET=private','ARBITRARY=unusual-credential','CAESIUM_REGISTRY_AUTH=secret://env/ARBITRARY','CAESIUM_DATABASE_DSN=postgres://u:password@db','CAESIUM_IT_REGISTRY_CREDS=ci-user:ci-pass'],'Cmd':['--auth=argv-credential']},'Args':['argv-credential'],'State':{'Running':True,'Error':'state-credential','Health':{'Log':[{'Output':'health-credential'}]}}}]))
elif args[:2]==['image','inspect']:
 print(json.dumps([{'Id':args[-1],'Config':{'Env':['LABELLESS=image-credential'],'Cmd':['argv-credential'],'Labels':{'x':'label-credential'}}}]))
 if os.environ.get('MOCK_IMAGE_INSPECT_FAIL'): sys.exit(1)
elif args[0]=='create':
 name=option('--name'); reservation=state/'names'/name
 try:
  fd=os.open(reservation,os.O_CREAT|os.O_EXCL|os.O_WRONLY);os.close(fd)
 except FileExistsError: sys.exit(17)
 kind='fixture' if 'fixture-' in name else 'runner' if 'runner-' in name else 'server'
 cid={'server':'1','fixture':'2','runner':'3'}[kind]*64
 Path(option('--cidfile')).write_text(cid+'\n')
 print(cid,flush=True)
 if os.environ.get('MOCK_SIGNAL_AFTER_CREATE')==kind:
  os.killpg(os.getpgrp(),signal.SIGTERM);time.sleep(1)
elif args[0]=='start':
 cid=args[-1]
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
elif args[0]=='logs': print('bootstrap csk_live_fixture_bootstrap')
elif args[0]=='exec': print('{"ready":true}')
elif args[0]=='cp': Path(args[-1]).write_text('fixture CLI')
elif args[0] in ['build','rm']: pass
else: raise SystemExit('unexpected mock engine command: '+repr(args))
'''

JUST = r'''#!/usr/bin/env python3
import os, re, subprocess, sys
from pathlib import Path
state=Path(os.environ['MOCK_STATE'])
if 'integration-up-agent' not in sys.argv:
 if os.environ.get('MOCK_RACE_NAME'): (state/'names'/'caesium-server-agent-test').touch()
 raise SystemExit(0)
text=Path('justfile').read_text().split('integration-up-agent:',1)[1].splitlines()[1:]
lines=[]
for line in text:
 if line and not line.startswith(' '): break
 lines.append(line.strip().removeprefix('@'))
values={'container_cli':os.environ['CAESIUM_CONTAINER_CLI'],'agent_it_container':'caesium-server-agent-test','platform':'linux/amd64','port':'8080','sock':'/var/run/docker.sock','repo_dir':str(Path.cwd()),'local_image_ref':'caesiumcloud/caesium','tag':'probe','event_ingest_api_key':'fixture-event','agent_api_external_url':'http://fixture','contract_deprecation_window':'5s'}
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
        shutil.copy(ROOT/'justfile', self.root/'justfile')
        # integration-up-agent seeds the auth lane's policy file from this fixture.
        (self.root/'test/testdata/policy').mkdir(parents=True)
        shutil.copy(ROOT/'test/testdata/policy/auth-lane-reload.yaml', self.root/'test/testdata/policy/auth-lane-reload.yaml')
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
