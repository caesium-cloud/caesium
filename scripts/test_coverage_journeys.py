#!/usr/bin/env python3
"""Hermetic checks of the actual owned collector functions; no daemon calls."""
import contextlib
import copy
import io
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import os
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('collector', ROOT / 'scripts/coverage-journeys.py')
b = importlib.util.module_from_spec(spec)
spec.loader.exec_module(b)
OWNER, IMAGE, INDEX, CONFIG, CID = 'a' * 40, 'sha256:' + 'b' * 64, 'sha256:' + 'c' * 64, 'sha256:' + 'd' * 64, 'e' * 64
RUN, TASK = '11111111-1111-4111-8111-111111111111', '22222222-2222-4222-8222-222222222222'

class FaultChecks(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.tmp = Path(self.temp.name)
        sleeper = patch.object(b.time, "sleep", return_value=None)
        sleeper.start()
        self.addCleanup(sleeper.stop)

    def pair(self, name):
        path = self.tmp / name
        path.mkdir()
        (path / 'covmeta.actual').write_bytes(b'actual metadata')
        (path / 'covcounters.actual').write_bytes(b'actual counter bytes')
        return path

    def collector(self):
        args = SimpleNamespace(root=str(ROOT), artifacts=str(self.tmp), raw=str(self.tmp / 'raw'),
          coverage_image=IMAGE, builder_image=IMAGE, platform='linux/arm64', candidate_sha=OWNER,
          run_id='owned', build_context='{}', image_provenance='built-by-this-run', verified='true',
          container_cli='docker', docker_socket='/owned/socket', socket_gid=0,
          backend_inputs=str(self.tmp / 'input.json'), backend_inputs_sha256='')
        return b.Collector(args)

    def info(self):
        return {'Id': CID, 'Name': '/owned', 'Image': IMAGE, 'RestartCount': 0,
          'Config': {'Labels': {b.LABEL_OWNER: OWNER, b.LABEL_RUN: 'owned', b.LABEL_LANE: 'lane'}},
          'State': {'Running': False, 'ExitCode': 0, 'OOMKilled': False}}

    def command(self, info=None, fault=None):
        state = {'removed': False, 'mutations': []}
        info = info or self.info()
        def run(*args, check=True, **kwargs):
            rc, stdout, stderr = 0, '', ''
            if args[1] == 'inspect':
                if fault == 'socket': rc, stderr = 1, 'Cannot connect to daemon'
                elif state['removed']: rc, stderr = 1, 'Error: No such container: ' + str(args[2])
                else: stdout = json.dumps([info])
            else:
                state['mutations'].append(args)
                if args[1] == 'rm':
                    if fault == 'rm': rc = 1
                    else: state['removed'] = True
                if args[1] == 'kill' and fault == 'flush': rc = 1
            result = subprocess.CompletedProcess(args, rc, stdout, stderr)
            if check and rc: raise b.JourneyError('injected command failure')
            return result
        return run, state

    def test_typed_absence_rejects_socket_foreign_and_present_inventory(self):
        for stderr in ('Cannot connect to daemon', 'Error: No such container: foreign', 'Error: No such container: owned\nextra'):
            self.assertFalse(b.missing_object(subprocess.CompletedProcess([], 1, '', stderr), 'container', 'owned'))
        self.assertFalse(b.missing_object(subprocess.CompletedProcess([], 1, '[{}]', 'Error: No such container: owned'), 'container', 'owned'))
        self.assertTrue(b.missing_object(subprocess.CompletedProcess([], 1, '', 'Error: No such container: owned'), 'container', 'owned'))

    def test_owned_sso_bridge_supports_loopback_publication_and_keeps_identity_checks(self):
        c = self.collector()
        network_id = 'f' * 64
        calls = []

        def daemon(*args, check=True, **kwargs):
            calls.append(args)
            if args == ('network', 'inspect', c.network_name):
                return subprocess.CompletedProcess(args, 1, '',
                    'Error response from daemon: network ' + c.network_name + ' not found')
            if args[:2] == ('network', 'create'):
                # Docker's internal-only bridge drops host publications. Model
                # that protocol distinction, rather than accepting any create.
                daemon.published = '--internal' not in args
                return subprocess.CompletedProcess(args, 0, network_id, '')
            if args == ('network', 'inspect', network_id):
                return subprocess.CompletedProcess(args, 0, json.dumps([{
                    'Id': network_id, 'Labels': {b.LABEL_OWNER: OWNER, b.LABEL_RUN: c.run_id}}]), '')
            if args == ('port', CID, '8080/tcp'):
                if not daemon.published:
                    raise b.JourneyError('container command failed (1): port')
                return subprocess.CompletedProcess(args, 0, '127.0.0.1:49152\n', '')
            self.fail('unexpected daemon operation: ' + repr(args))

        c.docker_run = daemon
        c.create_network()
        self.assertEqual(c.network_id, network_id)
        self.assertEqual(c.host_api_base(CID), 'http://127.0.0.1:49152')
        create = next(args for args in calls if args[:2] == ('network', 'create'))
        self.assertIn(b.LABEL_OWNER + '=' + OWNER, create)
        self.assertIn(b.LABEL_RUN + '=' + c.run_id, create)
        self.assertEqual(create[-1], c.network_name)

        for field, invalid in (('Id', 'foreign'), ('Labels', {b.LABEL_OWNER: 'foreign', b.LABEL_RUN: c.run_id})):
            c = self.collector()
            valid = {'Id': network_id, 'Labels': {b.LABEL_OWNER: OWNER, b.LABEL_RUN: c.run_id}}
            valid[field] = invalid
            c.docker_run = lambda *args, **kwargs: subprocess.CompletedProcess(args, 1, '',
                'Error response from daemon: network ' + c.network_name + ' not found')
            c.docker_stdout = lambda *args: network_id if args[:2] == ('network', 'create') else json.dumps([valid])
            with self.assertRaises(b.JourneyError):
                c.create_network()

    def test_host_api_port_refuses_absence_wildcard_multiple_and_command_failure(self):
        c = self.collector()
        for mapping in ('', '0.0.0.0:49152', '[::]:49152', '127.0.0.1:49152\n127.0.0.1:49153'):
            with self.subTest(mapping=mapping):
                c.docker_stdout = lambda *args: mapping
                with self.assertRaises(b.JourneyError):
                    c.host_api_base(CID)
        with patch.object(c, 'docker_stdout', side_effect=b.JourneyError('container command failed (1): port')):
            with self.assertRaises(b.JourneyError):
                c.host_api_base(CID)

    def test_socket_inventory_failure_cannot_remove_or_prove_absence(self):
        for action in ('absent', 'remove'):
            command, state = self.command(fault='socket')
            with self.assertRaises(b.JourneyError): b.guarded_resource(command, 'container', CID, action, OWNER, 'owned', image=IMAGE)
            self.assertEqual(state['mutations'], [])

    def test_foreign_labels_and_wrong_image_refuse_before_mutation(self):
        for bad in ('owner', 'image'):
            info = self.info()
            if bad == 'owner': info['Config']['Labels'][b.LABEL_OWNER] = 'foreign'
            else: info['Image'] = INDEX
            command, state = self.command(info)
            with self.assertRaises(b.JourneyError): b.guarded_resource(command, 'container', CID, 'remove', OWNER, 'owned', image=IMAGE)
            self.assertEqual(state['mutations'], [])

    def test_failed_rm_and_flush_with_valid_counters_cannot_be_eligible(self):
        self.assertTrue(b.raw_files(self.pair('raw')))
        for fault, action in (('rm', 'remove'), ('flush', 'stop')):
            command, state = self.command(fault=fault)
            with self.assertRaises(b.JourneyError): b.guarded_resource(command, 'container', CID, action, OWNER, 'owned', image=IMAGE)
            self.assertTrue(state['mutations'])

    def test_stop_refuses_signal_exit_restart_oom_and_running_states(self):
        for key, value in (('ExitCode', 143), ('Running', True), ('OOMKilled', True), ('RestartCount', 1)):
            info = self.info()
            if key == 'RestartCount': info[key] = value
            else: info['State'][key] = value
            command, _ = self.command(info)
            with self.assertRaises(b.JourneyError): b.guarded_resource(command, 'container', CID, 'stop', OWNER, 'owned', image=IMAGE)
        command, _ = self.command()
        self.assertEqual(b.guarded_resource(command, 'container', CID, 'stop', OWNER, 'owned', image=IMAGE)['flush_rc'], 0)

    def connector_fixture(self, health, fault=None):
        audit = self.tmp / ("audit-" + str(len(list(self.tmp.iterdir()))))
        audit.mkdir()
        info = self.info()
        info["Config"].update(Env=["TOKEN=never-persist-env"])
        info["Config"]["Labels"][b.LABEL_LANE] = "base-connectors"
        info["State"].update(Running=fault != "stopped", Error="never-persist-native-error")
        calls, polls = [], []
        logs = json.dumps({"msg": "connector config loaded", "fingerprint": "a" * 64, "level": "info", "ts": "2026-10-05T12:00:00Z",
                           "error": "csk_neverpersist", "token": "never-persist-token"}) + "\n" + json.dumps(
               {"msg": "Bearer never-persist-bearer", "jwt": "eyJheader.eyJpayload.signature",
                "cookie": "never-persist-cookie", "assertion": "never-persist-assertion"})
        def command(*args, check=True, **kwargs):
            calls.append(args)
            rc, stdout = 0, ""
            if args[1] == "inspect":
                if fault == "foreign": info["Config"]["Labels"][b.LABEL_OWNER] = "foreign"
                if fault == "image": info["Image"] = INDEX
                if fault == "identity": info["Id"] = 'f' * 64
                stdout = json.dumps([info])
            elif args[1] == "logs":
                raw = logs if fault != "no-fingerprint" else json.dumps({"msg": "not loaded: connector config loaded"})
                rc = 17 if fault == "logs-failed" else 0
                stdout = json.dumps(b.connector_log_records((("stdout", io.BytesIO(raw.encode())),)))
                if rc:
                    stdout = json.dumps({"records": [], "loaded_event": None, "counts": {"failed_log_streams_omitted": 2}})
            elif args[1] == "exec":
                self.assertEqual(args, ("container", "exec", CID, "wget", "-q", "-T", "2", "-O", "-",
                                        "http://127.0.0.1:8080/health"))
                self.assertEqual(kwargs["timeout"], 5)
                polls.append(args)
                rc, stdout = health[min(len(polls) - 1, len(health) - 1)]
            elif args[1] == "kill":
                self.assertTrue(polls, "fingerprint alone must not cause a signal")
                self.assertEqual(health[min(len(polls) - 1, len(health) - 1)], (0, '{"status":"healthy"}'))
                rc = 1 if fault == "flush" else 0
            elif args[1] == "stop":
                info["State"].update(Running=False, ExitCode=17 if fault == "exit" else 0)
                rc = 1 if fault == "stop" else 0
            else:
                self.fail("unexpected connector operation " + repr(args))
            return subprocess.CompletedProcess(args, rc, stdout, "never-persist-native-stderr")
        return command, audit, calls

    def test_connector_waits_for_complete_exact_health_before_signal_and_retains_safe_states(self):
        command, audit, calls = self.connector_fixture([(0, '{"status":"unavailable"}'), (0, '{"status":"healthy"}')])
        result = b.connector_shutdown(command, CID, OWNER, "owned", IMAGE, audit)
        self.assertEqual(result["flush_rc"], 0)
        signals = [i for i, args in enumerate(calls) if args[1] == "kill"]
        self.assertEqual(len(signals), 1)
        self.assertEqual(len([args for args in calls[:signals[0]] if args[1] == "exec"]), 2)
        raw = (audit / 'connector-diagnostics.json').read_text()
        record = json.loads(raw)
        self.assertTrue(record['complete'] and record['fingerprint'] and record['healthy'])
        self.assertEqual(record['polls'], 2)
        self.assertTrue(record['pre_stop']['State']['Running'])
        self.assertFalse(record['post_stop']['State']['Running'])
        self.assertEqual(record['refusal_category'], 'none')
        self.assertEqual(record['post_log_rc'], 0)
        for secret in ('never-persist', 'eyJheader', 'TOKEN=', 'Bearer', 'Config', 'Env', 'Error'):
            self.assertNotIn(secret, raw)
        self.assertIn('connector config loaded', raw)

    def test_connector_never_ready_stopped_and_invalid_complete_json_do_not_signal(self):
        for rc, body in ((0, '{"status":"unhealthy"}'), (0, '{"healthy":true}'),
                         (0, '{"msg":"healthy"}'), (0, '{"status":"healthy"} trailing'),
                         (0, '{"status":"healthy"'), (1, '{"status":"healthy"}')):
            with self.subTest(rc=rc, body=body):
                command, audit, calls = self.connector_fixture([(rc, body)])
                with self.assertRaises(b.JourneyError):
                    b.connector_shutdown(command, CID, OWNER, "owned", IMAGE, audit)
                self.assertFalse(any(args[1] in ('kill', 'stop') for args in calls))
                record = json.loads((audit / 'connector-diagnostics.json').read_text())
                self.assertFalse(record['complete'])
                self.assertEqual(record['refusal_category'], 'never_ready')
                self.assertEqual(record['polls'], 60)
        for fault in ('stopped', 'no-fingerprint', 'foreign', 'image', 'identity', 'logs-failed'):
            with self.subTest(fault=fault):
                command, audit, calls = self.connector_fixture([(0, '{"status":"healthy"}')], fault)
                with self.assertRaises(b.JourneyError):
                    b.connector_shutdown(command, CID, OWNER, "owned", IMAGE, audit)
                self.assertFalse(any(args[1] in ('kill', 'stop') for args in calls))
                record = json.loads((audit / 'connector-diagnostics.json').read_text())
                self.assertFalse(record['complete'])
                self.assertEqual(record['polls'], 60 if fault in ('no-fingerprint', 'logs-failed') else 1)
                if fault == 'logs-failed':
                    self.assertEqual(record['log_rc'], 17)
                    self.assertEqual(record['log_counts']['failed_log_streams_omitted'], 2)
                    self.assertFalse(record['fingerprint'])

    def test_connector_failed_stop_retains_refusal_without_native_secret_or_eligibility(self):
        for fault, category in (('flush', 'flush_failed'), ('stop', 'stop_failed'), ('exit', 'nonzero_exit')):
            with self.subTest(fault=fault):
                command, audit, _ = self.connector_fixture([(0, '{"status":"healthy"}')], fault)
                with self.assertRaises(b.JourneyError):
                    b.connector_shutdown(command, CID, OWNER, "owned", IMAGE, audit)
                raw = (audit / 'connector-diagnostics.json').read_text()
                record = json.loads(raw)
                self.assertFalse(record['complete'])
                self.assertEqual(record['refusal_category'], category)
                self.assertIn('post_stop', record)
                self.assertNotIn('never-persist', raw)
        command, audit, _ = self.connector_fixture([(0, '{"status":"healthy"}')])
        (audit / 'connector-diagnostics.json').write_text('existing evidence')
        with self.assertRaises(FileExistsError):
            b.connector_shutdown(command, CID, OWNER, "owned", IMAGE, audit)
        self.assertEqual((audit / 'connector-diagnostics.json').read_text(), 'existing evidence')

    def test_connector_unknown_errors_logs_and_output_caps_do_not_leak_or_pass(self):
        command, audit, calls = self.connector_fixture([(0, '{"status":"healthy"}')])
        def interrupted(*args, **kwargs):
            if args[1] == 'exec':
                raise RuntimeError('never-persist-native-error csk_neverpersist')
            return command(*args, **kwargs)
        with self.assertRaises(RuntimeError):
            b.connector_shutdown(interrupted, CID, OWNER, 'owned', IMAGE, audit)
        raw = (audit / 'connector-diagnostics.json').read_text()
        self.assertNotIn('never-persist', raw)
        self.assertEqual(json.loads(raw)['exception_type'], 'UnexpectedError')
        self.assertFalse(any(args[1] in ('kill', 'stop') for args in calls))
        logs = b.connector_log_records((("stderr", io.BytesIO((json.dumps({'msg': ['csk_secret'], 'caller': 'csk_secret.go:1',
            'error': 'Bearer private', 'Env': ['TOKEN=private']}) + '\nprivate raw line').encode())),))["records"]
        self.assertNotIn('private', json.dumps(logs))
        self.assertNotIn('csk_secret', json.dumps(logs))
        self.assertEqual(logs[-1]['msg'], '[unstructured log omitted]')
        def process(args, stdout, stderr, **kwargs):
            self.assertEqual(kwargs['timeout'], 10)
            stdout.write(b'x' * (2 * 1024 * 1024 + 1))
            stderr.write(b'never-persist-stderr')
            return subprocess.CompletedProcess(args, 0)
        with patch.object(b.subprocess, 'run', side_effect=process):
            with self.assertRaises(b.JourneyError):
                b.connector_command('container', 'exec', CID, timeout=10)

    def test_connector_nonlog_cap_keeps_static_reason_and_numeric_bound(self):
        command, audit, calls = self.connector_fixture([(0, '{"status":"healthy"}')])
        def oversized(*args, **kwargs):
            if args[1] == 'exec':
                raise b.ConnectorOutputLimit(2 * 1024 * 1024, 2 * 1024 * 1024 + 1)
            return command(*args, **kwargs)
        with self.assertRaises(b.ConnectorOutputLimit):
            b.connector_shutdown(oversized, CID, OWNER, 'owned', IMAGE, audit)
        record = json.loads((audit / 'connector-diagnostics.json').read_text())
        self.assertFalse(record['complete'])
        self.assertEqual(record['refusal_category'], 'nonlog_output_cap')
        self.assertEqual(record['output_limit'], 2 * 1024 * 1024)
        self.assertEqual(record['observed_output_bytes_at_least'], 2 * 1024 * 1024 + 1)
        self.assertFalse(any(args[1] in ('kill', 'stop') for args in calls))

    def actual_connector_log_process(self, program):
        run = subprocess.run
        def child(argv, **kwargs):
            self.assertEqual(argv, ['docker', 'container', 'logs', '--tail', 'all', CID])
            self.assertEqual(kwargs['timeout'], 10)
            self.assertFalse(kwargs['check'])
            # Real child OS stdout/stderr and actual unlinked temp-file spools;
            # no Docker process, application execution or live coverage claim.
            return run([sys.executable, '-c', program], **kwargs)
        with patch.object(b.subprocess, 'run', side_effect=child):
            return b.connector_command('container', 'logs', '--tail', 'all', CID, check=False, timeout=10)

    def test_connector_large_startup_keeps_loaded_proof_apart_from_last100(self):
        result = self.actual_connector_log_process("""import json,sys
print(json.dumps({'msg':'connector config loaded','fingerprint':'a'*64,'token':'never-persist-token'}))
for _ in range(1400): print(json.dumps({'msg':'never-persist-startup'+'x'*250,'error':'csk_neverpersist'}))
print(json.dumps({'msg':'server started'}),file=sys.stderr)
""")
        evidence = json.loads(result.stdout)
        self.assertEqual(result.returncode, 0)
        self.assertGreater(evidence['counts']['stdout_bytes'], 256 * 1024)
        self.assertEqual(evidence['counts']['stdout_lines'], 1401)
        self.assertEqual(evidence['counts']['stderr_lines'], 1)
        self.assertEqual(len(evidence['records']), 100)
        self.assertEqual(evidence['loaded_event']['msg'], 'connector config loaded')
        self.assertEqual(evidence['loaded_event']['stream'], 'stdout')
        self.assertEqual(evidence['loaded_event']['fingerprint'], 'a' * 64)
        self.assertFalse(any(r['msg'] == 'connector config loaded' for r in evidence['records']))
        self.assertLess(len(result.stdout.encode()), 40 * 1024)
        self.assertEqual(result.stderr, '')
        self.assertNotIn('never-persist', result.stdout)
        self.assertNotIn('csk_neverpersist', result.stdout)

    def test_connector_stderr_event_is_real_proof_only_for_successful_log_command(self):
        program = """import json,sys
print('never-persist-stdout')
print(json.dumps({'msg':'connector config loaded','fingerprint':'a'*64,'error':'never-persist-error'}),file=sys.stderr)
for _ in range(1100): print(json.dumps({'msg':'Bearer never-persist-secret'}),file=sys.stderr)
"""
        result = self.actual_connector_log_process(program)
        evidence = json.loads(result.stdout)
        self.assertEqual(evidence['loaded_event']['stream'], 'stderr')
        self.assertEqual(evidence['counts']['stderr_lines'], 1101)
        self.assertEqual(len(evidence['records']), 100)
        self.assertNotIn('never-persist', result.stdout)
        failed = self.actual_connector_log_process(program + '\nsys.exit(17)')
        self.assertEqual(failed.returncode, 17)
        failure = json.loads(failed.stdout)
        self.assertIsNone(failure['loaded_event'])
        self.assertEqual(failure['records'], [])
        self.assertEqual(failure['counts']['failed_log_streams_omitted'], 2)
        self.assertNotIn('connector config loaded', failed.stdout)
        self.assertNotIn('never-persist', failed.stdout)

    def test_connector_real_successful_logs_without_valid_fingerprint_never_signal(self):
        for channel in ('stdout', 'stderr'):
            for fingerprint in ('missing', '', 'a' * 63, 'g' * 64, 123, ['a' * 64]):
                with self.subTest(channel=channel, fingerprint=fingerprint):
                    event = {'msg': 'connector config loaded'}
                    if fingerprint != 'missing':
                        event['fingerprint'] = fingerprint
                    program = "import json,sys\nprint(" + repr(json.dumps(event)) + ",file=sys." + channel + ")"
                    result = self.actual_connector_log_process(program)
                    evidence = json.loads(result.stdout)
                    self.assertEqual(result.returncode, 0)
                    self.assertIsNone(evidence['loaded_event'])
                    command, audit, calls = self.connector_fixture([(0, '{"status":"healthy"}')])
                    def invalid_logs(*args, **kwargs):
                        if args[1] == 'logs':
                            return result
                        return command(*args, **kwargs)
                    with self.assertRaises(b.JourneyError):
                        b.connector_shutdown(invalid_logs, CID, OWNER, 'owned', IMAGE, audit)
                    record = json.loads((audit / 'connector-diagnostics.json').read_text())
                    self.assertFalse(record['complete'] or record['fingerprint'])
                    self.assertTrue(record['healthy'])
                    self.assertEqual(record['refusal_category'], 'never_ready')
                    self.assertEqual(record['polls'], 60)
                    self.assertFalse(any(args[1] in ('kill', 'stop') for args in calls))

    def test_connector_stream_line_bound_cannot_promote_oversized_suffix_or_invalid_utf8(self):
        class BoundedStream(io.BytesIO):
            def read(self, *args):
                raise AssertionError('whole-stream reads are forbidden')
            def readline(self, size=-1):
                self.assert_bound(size)
                return super().readline(size)
            def assert_bound(self, size):
                if size != 64 * 1024 + 1:
                    raise AssertionError('unbounded line read')
        data = (b'x' * (2 * 1024 * 1024) + b'{"msg":"connector config loaded"}\n'
                + b'\xff{"msg":"connector config loaded"}\n'
                + b'{"msg":"server started"}\n')
        evidence = b.connector_log_records((("stdout", BoundedStream(data)),))
        self.assertIsNone(evidence['loaded_event'])
        self.assertEqual(evidence['counts']['oversized_lines'], 1)
        self.assertEqual(evidence['counts']['invalid_utf8_lines'], 1)
        self.assertEqual(evidence['counts']['stdout_bytes'], len(data))
        self.assertEqual(evidence['counts']['stdout_lines'], 3)
        self.assertLess(len(json.dumps(evidence)), 1024)

    def test_missing_base_raws_never_merge_valid_journeys_and_originals_survive(self):
        raw = self.tmp / 'cohort'
        raw.mkdir()
        good = self.pair('journey')
        shell = '''set -eu
source "$1/scripts/coverage-journeys.sh"
ROOT="$1"; RAW="$2"; MERGE_LOG="$4"
COVERAGE_JOURNEY_CLI_DIRS=("$3"); COVERAGE_JOURNEY_SERVER_DIRS=("$3")
gocoverdir_complete() { python3 "$ROOT/scripts/coverage-journeys.py" validate-raw --directory "$1" >/dev/null 2>&1; }
merge_gocoverdirs() { echo called >> "$MERGE_LOG"; }
merge_coverage_journeys
'''
        log = self.tmp / 'merges'
        result = subprocess.run(['bash', '-c', shell, 'hermetic', str(ROOT), str(raw), str(good), str(log)], capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(log.exists())
        for name in ('cli', 'server'):
            path = raw / name; path.mkdir()
            (path / 'covmeta.actual').write_bytes(b'original meta')
            (path / 'covcounters.actual').write_bytes(b'original counters')
        before = {name: b.raw_files(raw / name) for name in ('cli', 'server')}
        result = subprocess.run(['bash', '-c', shell, 'hermetic', str(ROOT), str(raw), str(good), str(log)], capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(log.read_text().splitlines(), ['called', 'called'])
        self.assertEqual(before, {name: b.raw_files(raw / name) for name in before})

    def test_shell_failed_cleanup_retains_immutable_id(self):
        shell = '''set -eu
source "$1/scripts/coverage-journeys.sh"
ARTIFACTS="$2"; COVERAGE_JOURNEY_ACTIVE_IDS=(owned-id)
coverage_journey_resource() { return 1; }
log() { :; }
if cleanup_coverage_journeys; then exit 9; fi
[[ "${COVERAGE_JOURNEY_ACTIVE_IDS[0]}" == owned-id ]]
'''
        result = subprocess.run(['bash', '-c', shell, 'hermetic', str(ROOT), str(self.tmp)], capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.tmp / 'retained-owned-container-ids.txt').read_text().strip(), 'owned-id')

    def test_index_config_receipts_remain_distinct_and_legacy_works(self):
        for config, index in ((CONFIG, INDEX), (CONFIG, CONFIG)):
            c = self.collector()
            inputs = {'schema_version': 1, 'platform': c.platform, 'docker_socket': c.docker_socket,
              'task_image_id': config, 'task_image_ref': 'alpine:3.23', 'task_archive': '/owned/archive',
              'task_archive_sha256': 'f' * 64, 'podman_privileged_approved': True, 'task_docker_image_id': index}
            data = json.dumps(inputs).encode(); c.backend_inputs_path.write_bytes(data)
            c.backend_inputs_sha256 = hashlib.sha256(data).hexdigest()
            c.docker_stdout = lambda *args: json.dumps([{'Id': index, 'Os': 'linux', 'Architecture': 'arm64', 'RepoTags': ['alpine:3.23']}])
            checked = []
            with patch.object(b, 'backend_module', return_value=SimpleNamespace(archive_identity=lambda *args: checked.append(args))):
                c.load_backend_prerequisites()
            self.assertEqual((c.task_image_id, c.task_docker_image_id), (config, index))
            self.assertEqual(checked[0][2], config)
            c.docker_stdout = lambda *args: json.dumps([{'Id': IMAGE, 'Os': 'linux', 'Architecture': 'arm64', 'RepoTags': ['alpine:3.23']}])
            with patch.object(b, 'backend_module', return_value=SimpleNamespace(archive_identity=lambda *args: None)):
                with self.assertRaises(b.JourneyError): c.load_backend_prerequisites()

    def test_native_running_and_exact_local_cause(self):
        c = self.collector(); c.task_image_id, c.task_docker_image_id, c.task_image_ref = CONFIG, INDEX, 'alpine:3.23'
        c.shutdown_job = {'runtime_id': CID, 'run_id': RUN, 'task_id': TASK}
        info = {'Id': CID, 'Image': INDEX, 'Name': '/' + TASK + '-' + RUN, 'RestartCount': 0,
          'Config': {'Image': 'alpine:3.23', 'Cmd': ['sh', '-c', 'sleep 300; echo caesium-shutdown-owned']},
          'State': {'Running': True, 'Restarting': False, 'OOMKilled': False}}
        c.inspect_container = lambda *args: info
        self.assertEqual(c.local_task_cancel_cause(), 'task ' + TASK + ' cancelled: context canceled')
        self.assertTrue(c.native_running()['running'])
        for field, value in (('Running', False), ('OOMKilled', True), ('Restarting', True)):
            info['State'][field] = value
            with self.assertRaises(b.JourneyError): c.native_running()
            info['State'][field] = {'Running': True, 'OOMKilled': False, 'Restarting': False}[field]

    def test_cleanup_secret_failure_prevents_publication(self):
        c = self.collector()
        secret = self.tmp / 'private.env'; secret.write_text('secret')
        c.secret_files = [secret]
        with patch.object(Path, 'unlink', side_effect=OSError('injected secret removal error')):
            with self.assertRaises(b.JourneyError): c.cleanup()
        self.assertFalse(c.cleanup_complete)
        self.assertTrue(c.cleanup_errors)
        self.assertIn(secret, c.secret_files)
        self.assertTrue((self.tmp / 'cleanup-incomplete.json').is_file())

    def test_main_cannot_publish_pass_after_cleanup_failure(self):
        events = []
        class Fake:
            def execute(self): events.append('execute'); return {}
            def cleanup(self): events.append('cleanup'); raise b.JourneyError('injected cleanup failure')
            def write_provenance(self, value): events.append('published')
        with patch.object(b, 'parse_args', return_value=SimpleNamespace(mode='sso')), patch.object(b, 'Collector', return_value=Fake()), patch.object(b.signal, 'signal'), contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(b.main(), 1)
        self.assertNotIn('published', events)

    def test_completed_rows_must_predate_original_server_finish(self):
        for late in (False, True):
            c = self.collector()
            c.task_image_id, c.task_docker_image_id, c.task_image_ref = CONFIG, INDEX, 'alpine:3.23'
            c.shutdown_job = {'job_id': RUN, 'run_id': RUN, 'task_id': TASK, 'task_run_id': TASK, 'runtime_id': CID}
            c.native_runtime_absent_generations = [1]
            c.verify_runtime_absent = lambda generation: None
            c.server_generations = [{'finished_at': '2026-10-04T12:00:02Z'}]
            completed = '2026-10-04T12:00:03Z' if late else '2026-10-04T12:00:01Z'
            cause = c.local_task_cancel_cause()
            parent = {'id': RUN, 'status': 'failed', 'error': 'context canceled', 'completed_at': completed,
                      'tasks': [{'task_id': TASK, 'status': 'failed', 'error': cause}]}
            instances = {'total': 1, 'partitions': [{'task_run_id': TASK, 'runtime_id': CID, 'status': 'failed', 'error': cause, 'completed_at': completed}]}
            c.api_json = lambda _, path: instances if '/partitions' in path else parent
            if late:
                with self.assertRaises(b.JourneyError): c.verify_shutdown_job_failed('owned-server')
            else:
                c.verify_shutdown_job_failed('owned-server')
                self.assertEqual(c.shutdown_job['final_task_run_error'], cause)

    def test_public_read_never_promotes_partial_valid_json_or_cap_plus_one(self):
        class Stream:
            headers = {}
            def __init__(self, first, fault=False): self.first, self.fault = first, fault
            def read(self, count):
                if self.first is not None:
                    first, self.first = self.first, None
                    return first
                if self.fault: raise OSError('sentinel after valid JSON')
                return b''
        self.assertEqual(b.complete_response_bytes(Stream(b'{}'), 8), b'{}')
        for stream in (Stream(b'{}', True), Stream(b'x' * 9)):
            with self.assertRaises(b.JourneyError): b.complete_response_bytes(stream, 8)
        truncated = Stream(b'{}'); truncated.headers = {'Content-Length': '3'}
        with self.assertRaises(b.JourneyError): b.complete_response_bytes(truncated, 8)

    def test_interrupted_docker_command_is_joined_and_not_evidence(self):
        c = self.collector()
        process = SimpleNamespace(pid=999999, returncode=0)
        calls = []
        def communicate(timeout):
            calls.append(timeout)
            if len(calls) == 1:
                raise subprocess.TimeoutExpired("fault-injected", timeout)
            return "", ""
        process.communicate = communicate
        with patch.object(b.subprocess, "Popen", return_value=process), patch.object(b.os, "killpg") as kill:
            with self.assertRaises(b.JourneyError): c.docker_run("container", "inspect", CID, timeout=1)
        self.assertEqual(calls, [1, 5])
        kill.assert_called_once_with(process.pid, b.signal.SIGTERM)

    def test_failed_cleanup_invalidates_reusable_primary_provenance(self):
        profiles, raw, artifacts = (self.tmp / name for name in ("profiles", "raw", "artifacts"))
        profiles.mkdir(); (raw / "journeys").mkdir(parents=True); artifacts.mkdir()
        paths = [profiles / (source + ".provenance.json") for source in ("cli", "server", "integration", "browser")]
        paths += [raw / "journeys" / "manifest.json", artifacts / "backend-inputs.json"]
        for path in paths:
            path.write_text(json.dumps({"complete": True, "candidate_sha": OWNER}))
            path.chmod(0o400)
        b.invalidate_collection(profiles, raw, artifacts)
        for path in paths:
            value = json.loads(path.read_text())
            self.assertIs(value["complete"], False)
            self.assertIs(value["cleanup_complete"], False)
            self.assertEqual(value["candidate_sha"], OWNER)

    def test_shell_secret_unlink_failure_is_not_success(self):
        secret = self.tmp / "private.env"; secret.write_text("secret")
        shell = '''set -eu
source "$1/scripts/coverage-journeys.sh"
ARTIFACTS="$2"; COVERAGE_JOURNEY_SECRET_FILES=("$3")
rm() { return 1; }
log() { :; }
if cleanup_coverage_journeys; then exit 9; fi
[[ "${COVERAGE_JOURNEY_SECRET_FILES[0]}" == "$3" ]]
'''
        result = subprocess.run(["bash", "-c", shell, "hermetic", str(ROOT), str(self.tmp), str(secret)], capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(secret.exists())

    def test_saved_sso_receipt_requires_original_native_and_timestamp_evidence(self):
        raw = self.tmp / "raw"; raw.mkdir()
        processes = []
        for source, lane in (("cli", "shutdown-apply"), ("cli", "shutdown-start"), ("server", "sso-server-g1"), ("server", "sso-server-g2")):
            directory = raw / lane; directory.mkdir()
            (directory / "covmeta.actual").write_bytes(b"original metadata")
            (directory / "covcounters.actual").write_bytes(b"original counters")
            process = {"schema_version": 1, "source": source, "lane": lane, "kind": "gocoverdir", "module": b.MODULE,
                "candidate_sha": OWNER, "image_id": IMAGE, "complete": True, "missing": False, "killed": False,
                "oom_killed": False, "running": False, "restart_count": 0, "exit_code": 0,
                "container_id": (str(len(processes) + 1) * 64), "raw_dir": lane, "files": b.raw_files(directory),
                "provenance_path": lane + ".json", "flush": "process-exit", "signal": None, "stop_rc": None, "flush_rc": None}
            if source == "server":
                process.update(generation=len(processes) - 1, flush="sigusr2", signal="SIGTERM", stop_rc=0, flush_rc=0,
                    database_mount_sha256="same-db", server_environment_sha256="same-env", finished_at="2026-10-04T12:00:02Z")
            processes.append(process)
        shutdown = {"job_id": RUN, "run_id": RUN, "task_id": TASK, "task_run_id": TASK, "runtime_id": CID,
            "task_image_id": CONFIG, "initial_run_status": "running", "initial_task_status": "running",
            "native_runtime_removed": True, "verified_after_generation": 2, "final_run_status": "failed", "final_task_status": "failed",
            "final_run_error": "context canceled", "final_task_error": "task " + TASK + " cancelled: context canceled",
            "final_task_run_id": TASK, "final_task_run_status": "failed", "final_task_run_error": "task " + TASK + " cancelled: context canceled",
            "run_completed_at": "2026-10-04T12:00:01Z", "task_completed_at": "2026-10-04T12:00:01Z",
            "native_runtime_absent_after_generation": 2, "native_runtime_absent_generations": [1, 2], "replay_restart_elapsed_seconds": 1,
            "native_before_signal": {"running": True, "run_id": RUN, "task_id": TASK, "runtime_id": CID,
                "docker_image_id": INDEX, "task_config_id": CONFIG}}
        original = {"complete": True, "missing": False, "killed": False, "candidate_sha": OWNER, "image_id": IMAGE,
            "task_image_id": CONFIG, "task_docker_image_id": INDEX, "task_image_ref": "alpine:3.23",
            "server_generations": processes[2:], "cli_processes": processes[:2], "shutdown_cancellation": shutdown}
        inputs = self.tmp / "inputs.json"
        inputs.write_text(json.dumps({"task_image_id": CONFIG, "task_docker_image_id": INDEX, "task_image_ref": "alpine:3.23"}))
        digest = hashlib.sha256(inputs.read_bytes()).hexdigest(); original["backend_inputs_sha256"] = digest
        source = (ROOT / "scripts/coverage-journeys.sh").read_text()
        start = source.index('import datetime\nimport hashlib', source.index('local sso_record='))
        code = source[start:source.index('\nPY\n  then', start)]
        record = self.tmp / "record.json"
        env = {**os.environ, "SSO_RECORD_RAW_ROOT": str(raw.resolve()), "SSO_RECORD_SHA": OWNER,
               "SSO_RECORD_IMAGE_ID": IMAGE, "SSO_RECORD_INPUTS_SHA256": digest}
        for fault in (None, "plain", "native", "timestamp", "exit", "cause", "flush", "wrong-uuid", "substring"):
            value = copy.deepcopy(original)
            if fault == "plain":
                value["shutdown_cancellation"].update(final_task_error="context canceled", final_task_run_error="context canceled")
            if fault == "wrong-uuid":
                wrong = "task " + RUN + " cancelled: context canceled"
                value["shutdown_cancellation"].update(final_task_error=wrong, final_task_run_error=wrong)
            if fault == "substring":
                wrong = "prefix context canceled suffix"
                value["shutdown_cancellation"].update(final_task_error=wrong, final_task_run_error=wrong)
            if fault == "native": value["shutdown_cancellation"]["native_before_signal"]["running"] = False
            if fault == "timestamp": value["shutdown_cancellation"]["task_completed_at"] = "2026-10-04T12:00:03Z"
            if fault == "exit": value["server_generations"][0]["exit_code"] = 143
            if fault == "cause": value["shutdown_cancellation"]["final_task_error"] = "context canceled"
            if fault == "flush": value["server_generations"][0]["flush_rc"] = 1
            for process in value["cli_processes"] + value["server_generations"]:
                (raw / process["provenance_path"]).write_text(json.dumps(process))
            record.write_text(json.dumps(value))
            cli_list, server_list = self.tmp / "cli-list", self.tmp / "server-list"
            for path in (cli_list, server_list): path.unlink(missing_ok=True)
            result = subprocess.run(["python3", "-c", code, str(record), str(inputs), str(cli_list), str(server_list)],
                env=env, capture_output=True)
            if fault in (None, "plain"):
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(cli_list.read_text().splitlines(), ["shutdown-apply", "shutdown-start"])
            else:
                self.assertNotEqual(result.returncode, 0, fault)
                self.assertFalse(cli_list.exists(), fault)

    def test_live_shutdown_exact_cause_forms_must_agree_on_both_rows(self):
        formatted = "task " + TASK + " cancelled: context canceled"
        for collapsed, concrete, accepted in (
            (formatted, formatted, True), ("context canceled", "context canceled", True),
            (formatted, "context canceled", False), ("context canceled", formatted, False),
            ("task " + RUN + " cancelled: context canceled", "task " + RUN + " cancelled: context canceled", False),
            ("prefix context canceled suffix", "prefix context canceled suffix", False),
        ):
            c = self.collector()
            c.shutdown_job = {"job_id": RUN, "run_id": RUN, "task_id": TASK, "task_run_id": TASK, "runtime_id": CID}
            c.native_runtime_absent_generations = [1]
            c.verify_runtime_absent = lambda generation: None
            c.server_generations = [{"finished_at": "2026-10-04T12:00:02Z"}]
            parent = {"id": RUN, "status": "failed", "error": "context canceled", "completed_at": "2026-10-04T12:00:01Z",
                "tasks": [{"task_id": TASK, "status": "failed", "error": collapsed}]}
            instances = {"total": 1, "partitions": [{"task_run_id": TASK, "runtime_id": CID, "status": "failed",
                "error": concrete, "completed_at": "2026-10-04T12:00:01Z"}]}
            c.api_json = lambda _, path: instances if "/partitions" in path else parent
            if accepted:
                c.verify_shutdown_job_failed("server")
                self.assertEqual(c.shutdown_job["final_task_error"], concrete)
            else:
                with self.assertRaises(b.JourneyError): c.verify_shutdown_job_failed("server")

    def test_shell_untrack_last_id_and_keep_another_under_nounset(self):
        shell = '''set -eu
source "$1/scripts/coverage-journeys.sh"
coverage_journey_track_id first
coverage_journey_untrack_id first
[[ "${#COVERAGE_JOURNEY_ACTIVE_IDS[@]}" == 0 ]]
coverage_journey_track_id first
coverage_journey_track_id second
coverage_journey_untrack_id first
[[ "${#COVERAGE_JOURNEY_ACTIVE_IDS[@]}" == 1 && "${COVERAGE_JOURNEY_ACTIVE_IDS[0]}" == second ]]
coverage_journey_untrack_id second
[[ "${#COVERAGE_JOURNEY_ACTIVE_IDS[@]}" == 0 ]]
'''
        result = subprocess.run(["bash", "-c", shell, "hermetic", str(ROOT)], capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_audit_extraction_failure_keeps_ownership_and_cannot_pass(self):
        source = (ROOT / "scripts/integration-coverage.sh").read_text()
        start = source.index("extract_audit() {")
        extract = source[start:source.index("\nwrite_fixture() {", start)]
        shell = '''set -eu
source "$1/scripts/coverage-journeys.sh"
ROOT="$1"; ARTIFACTS="$2"; AUDIT="$2"; ID=owned; CANDIDATE_SHA=owner; IMAGE_ID=image; PLATFORM=linux/arm64
CONTAINER_CLI=fake_docker; FAULT="$3"
require_cmd() { :; }
die() { exit 1; }
log() { :; }
coverage_journey_resource() {
  [[ "$1" != remove || "$FAULT" == success ]] || return 1
}
fake_docker() {
  case "$1" in
    create)
      [[ " $* " == *" --name owned-audit-extract "* && " $* " == *" --label caesium.coverage.owner=owner "* && " $* " == *" --label caesium.coverage.run=owned "* && " $* " == *" --label caesium.coverage.lane=audit-extract "* ]] || return 8
      [[ "$FAULT" != create ]] || return 1
      echo immutable-id
      ;;
    cp) [[ "$FAULT" != cp ]] ;;
    *) return 9 ;;
  esac
}
trap 'printf "%s\\n" "${COVERAGE_JOURNEY_PENDING_NAMES[*]}" >"$ARTIFACTS/names"; if ((${#COVERAGE_JOURNEY_ACTIVE_IDS[@]})); then printf "%s\\n" "${COVERAGE_JOURNEY_ACTIVE_IDS[@]}" >"$ARTIFACTS/ids"; fi' EXIT
''' + extract + "\nextract_audit\nprintf PASS\n"
        for fault in ("success", "create", "cp", "remove"):
            case = self.tmp / fault; case.mkdir()
            result = subprocess.run(["bash", "-c", shell, "hermetic", str(ROOT), str(case), fault], capture_output=True)
            if fault == "success":
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, b"PASS")
            else:
                self.assertNotEqual(result.returncode, 0, fault)
                self.assertNotIn(b"PASS", result.stdout)
                self.assertIn("owned-audit-extract", (case / "names").read_text())
                if fault != "create": self.assertIn("immutable-id", (case / "ids").read_text())

    def test_network_rm_failure_retains_ledger(self):
        c = self.collector(); c.network_id, c.network_attempted = CID, True
        info = {'Id': CID, 'Labels': {b.LABEL_OWNER: OWNER, b.LABEL_RUN: 'owned'}}
        command, _ = self.command(info, fault='rm'); c.docker_run = command
        with self.assertRaises(b.JourneyError): c.cleanup()
        self.assertEqual(c.network_id, CID)
        self.assertFalse(c.cleanup_complete)

if __name__ == "__main__":
    unittest.main()
