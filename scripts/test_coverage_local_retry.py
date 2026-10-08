#!/usr/bin/env python3
"""Portable guard controls, never runtime or eligible coverage evidence."""
import copy
import ast
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('local_retry', ROOT / 'scripts/coverage-local-retry.py')
b = importlib.util.module_from_spec(spec)
spec.loader.exec_module(b)
JOB = '11111111-1111-4111-8111-111111111111'
RUN = '22222222-2222-4222-8222-222222222222'
TASKS = ['33333333-3333-4333-8333-333333333333', '44444444-4444-4444-8444-444444444444']
ROWS = ['55555555-5555-4555-8555-555555555555', '66666666-6666-4666-8666-666666666666']
IMAGE, INDEX, CONFIG = ('sha256:' + value * 64 for value in ('a', 'b', 'c'))


def date(second):
    return '2026-10-05T12:00:%02dZ' % second


class ReceiptControls(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.output = Path(self.temp.name).resolve()
        self.context = {'candidate_sha': 'd' * 40, 'image_id': IMAGE, 'builder_image_id': 'sha256:' + 'e' * 64,
                        'build_context': {'goos': 'linux', 'goarch': 'arm64', 'cgo_enabled': True, 'build_tags': [], 'compiler': 'gc'},
                        'image_provenance': 'built-by-this-run', 'verified': True, '_context_sha256': 'f' * 64, '_inputs_sha256': '0' * 64,
                        'source_inventory': {'sha256': '1' * 64}}
        self.inputs = {'task_image_id': CONFIG, 'task_docker_image_id': INDEX, 'task_archive_sha256': '2' * 64}
        self.record = {key: self.context[key] for key in ('candidate_sha', 'image_id', 'builder_image_id', 'build_context', 'image_provenance', 'verified')}
        self.record.update(lane='local-retry', kind='real-local-retry-natural-drain', complete=True, missing=False, killed=False, cleanup_errors=[],
                           producer_context_sha256=self.context['_context_sha256'], producer_inputs_sha256=self.context['_inputs_sha256'], source_inventory_sha256=self.context['source_inventory']['sha256'],
                           binary_sha256='3' * 64, task_image_id=CONFIG, task_docker_image_id=INDEX, task_archive_sha256='2' * 64,
                           job_id=JOB, run_id=RUN, marker='owned', owner='owned', processes=[],
                           cleanup_absences=[{'kind': 'container', 'name': 'owned-' + role, 'identity': str(i + 4) * 64, 'absent': True} for i, role in enumerate(b.ROLES)] +
                           [{'kind': 'network', 'name': 'owned-net', 'identity': 'c' * 64, 'absent': True}, {'kind': 'volume', 'name': 'owned-db', 'identity': 'owned-db', 'absent': True}])
        for i, (role, begin, end) in enumerate((('server-1', 1, 10), ('apply', 2, 3), ('start', 4, 5), ('retry', 11, 21), ('server-2', 22, 25))):
            raw = self.output / ('raw-' + role)
            raw.mkdir()
            (raw / 'covmeta.fixture').write_bytes(b'test-only metadata control')
            (raw / 'covcounters.fixture').write_bytes(b'test-only counter control')
            process = {key: self.record[key] for key in ('candidate_sha', 'image_id', 'builder_image_id', 'build_context', 'image_provenance', 'verified', 'producer_context_sha256', 'producer_inputs_sha256', 'source_inventory_sha256')}
            server = role.startswith('server-')
            process.update(role=role, source='server' if server else 'cli', complete=True, missing=False, killed=False, container_id=str(i + 4) * 64,
                           binary_sha256='3' * 64, command=['start'] if server else [], exit_code=0, oom_killed=False, restart_count=0,
                           started_at=date(begin), finished_at=date(end), raw_dir=raw.name, files=b.backend.coverage_files(raw),
                           flush='sigusr2' if server else 'process-exit', signal='SIGTERM' if server else None, flush_rc=0 if server else None, stop_rc=0 if server else None)
            if role == 'retry':
                process['command'] = ['run', 'retry', '--job-id', JOB, '--run-id', RUN]
            self.record['processes'].append(process)
        rows = {}
        for i, name in enumerate(('preserved', 'failed')):
            rows[name] = {'task_id': TASKS[i], 'task_run_id': ROWS[i], 'status': 'succeeded' if i == 0 else 'failed', 'attempt': 1, 'cache_hit': False,
                          'runtime_id': ('9' if i == 0 else 'a') * 64, 'started_at': date(6), 'completed_at': date(8),
                          'output': {'marker': 'owned'} if i == 0 else {}, 'error': '' if i == 0 else 'exit17', 'exit_code': 0 if i == 0 else 17, 'oom_known': True, 'oom_killed': False}
        before = {'id': RUN, 'job_id': JOB, 'params': {'case': 'owned'}, 'status': 'failed', 'error': 'ordinary failure', 'completed_at': date(9), 'rows': rows}
        after = copy.deepcopy(before)
        after['rows']['failed'].update(runtime_id='b' * 64, started_at=date(13), completed_at=date(18))
        after['completed_at'] = date(20)
        self.record.update(before=before, after=after, finished_log='FINISHED_owned:owned',
                           native={'runtime_id': 'b' * 64, 'task_id': TASKS[1], 'task_run_id': ROWS[1], 'run_id': RUN, 'image_id': INDEX, 'running': True, 'cli_running': True,
                                   'absent_before_observer': True, 'running_at_ns': b.timestamp(date(14)), 'die_at_ns': b.timestamp(date(17)), 'absent_at_ns': b.timestamp(date(19)), 'exit_code': 17})
        self.sidecars()

    def sidecars(self):
        for p in self.record['processes']:
            (self.output / ('provenance-' + p['role'] + '.json')).write_text(json.dumps(p))

    def validate(self):
        return b.validate_receipt(self.record, self.context, self.inputs, self.output)

    def test_exact_five_original_processes_and_bytes(self):
        original = {p: p.read_bytes() for p in self.output.rglob('cov*')}
        result = self.validate()
        self.assertEqual(len(result['cli']), 3)
        self.assertEqual(len(result['server']), 2)
        self.assertEqual(original, {p: p.read_bytes() for p in original})
        # An absence observed after natural exit is honestly acceptable only
        # when die+durable completion preceded exit and observation preceded g2.
        self.record['native']['absent_at_ns'] = b.timestamp(date(21)) + 1
        self.validate()

    def test_actual_record_finish_order_preserves_five_originals(self):
        d = object.__new__(b.Driver)
        d.output, d.image = self.output, IMAGE
        d.binary_hash = self.record['binary_sha256']
        d.report = self.record
        d.processes = []
        originals = {p['role']: p for p in self.record['processes']}
        for role in ('apply', 'start', 'server-1', 'retry', 'server-2'):
            original = originals[role]
            obj = {'Id': original['container_id'], 'Image': IMAGE, 'RestartCount': 0,
                   'Config': {'Cmd': original['command']},
                   'State': {'ExitCode': 0, 'Running': False, 'OOMKilled': False,
                             'StartedAt': original['started_at'], 'FinishedAt': original['finished_at']}}
            d.owned = lambda *args, obj=obj: obj
            d.record(role, obj['Id'], self.output / original['raw_dir'], original['command'],
                     {'flush_rc': 0, 'stop_rc': 0} if role.startswith('server-') else None)
        self.record['processes'] = d.processes
        original_bytes = {(self.output / ('provenance-' + p['role'] + '.json')):
                          (self.output / ('provenance-' + p['role'] + '.json')).read_bytes() for p in d.processes}
        lists = self.validate()
        self.assertEqual([p['role'] for p in self.record['processes']], ['apply', 'start', 'server-1', 'retry', 'server-2'])
        self.assertEqual(len(lists['cli']), 3)
        self.assertEqual(len(lists['server']), 2)
        self.assertEqual(original_bytes, {path: path.read_bytes() for path in original_bytes})
        self.record['processes'][-1] = self.record['processes'][0]
        with self.assertRaises(b.Refused):
            self.validate()

    def test_public_partition_omits_known_false_oom_but_refuses_unknown_or_true(self):
        # partitionRow uses omitempty on oom_killed, so an observed false
        # value is absent in real public JSON; oom_known must still be true.
        for snapshot in ('before', 'after'):
            self.record[snapshot]['rows']['failed'].pop('oom_killed')
        self.validate()
        for value in (True, None, 'false'):
            with self.subTest(oom_killed=value):
                self.record['after']['rows']['failed']['oom_killed'] = value
                with self.assertRaises(b.Refused):
                    self.validate()
        self.record['after']['rows']['failed'].pop('oom_killed')
        for value in (False, None):
            with self.subTest(oom_known=value):
                self.record['after']['rows']['failed']['oom_known'] = value
                with self.assertRaises(b.Refused):
                    self.validate()
        self.record['after']['rows']['failed'].pop('oom_known')
        with self.assertRaises(b.Refused):
            self.validate()

    def test_partial_failed_stale_or_foreign_process_cannot_merge(self):
        original = copy.deepcopy(self.record)
        changes = [
            lambda r: r.update(complete=False), lambda r: r.update(cleanup_errors=['rm failed']),
            lambda r: r['cleanup_absences'].pop(), lambda r: r['cleanup_absences'][0].update(absent=False),
            lambda r: r.update(image_id=INDEX), lambda r: r.update(producer_context_sha256='x' * 64),
            lambda r: r['processes'].pop(), lambda r: r['processes'][3].update(exit_code=143),
            lambda r: r['processes'][3].update(signal='SIGTERM'), lambda r: r['processes'][0].update(flush_rc=1),
            lambda r: r['processes'][4].update(oom_killed=True), lambda r: r['processes'][3].update(restart_count=1),
            lambda r: r['processes'][3].update(container_id=r['processes'][1]['container_id']),
            lambda r: r['processes'][3]['command'].extend(['--server', 'http://server']),
            lambda r: r['processes'][3].update(raw_dir='../old'),
            lambda r: r['processes'][0].update(finished_at=date(12)),
            lambda r: r['processes'][4].update(started_at=date(20)),
        ]
        for change in changes:
            with self.subTest(change=changes.index(change)):
                self.record = copy.deepcopy(original)
                change(self.record)
                self.sidecars()
                with self.assertRaises(b.Refused):
                    self.validate()
        self.record = original
        self.sidecars()
        raw = self.output / 'raw-retry' / 'covcounters.fixture'
        raw.unlink()
        with self.assertRaises(b.Refused):
            self.validate()
        raw.write_bytes(b'stale different bytes')
        with self.assertRaises(b.Refused):
            self.validate()

    def test_old_attempt_cancelled_or_late_write_is_not_natural_drain(self):
        original = copy.deepcopy(self.record)
        changes = [
            lambda r: r['after']['params'].update(case='different'),
            lambda r: r['after']['rows']['preserved'].update(runtime_id='c' * 64),
            lambda r: r['after']['rows']['failed'].update(runtime_id=r['before']['rows']['failed']['runtime_id']),
            lambda r: r['after']['rows']['failed'].update(task_run_id=ROWS[0]),
            lambda r: r['after']['rows']['failed'].update(completed_at=date(23)),
            lambda r: r['after']['rows']['failed'].update(error='context canceled'),
            lambda r: r['after'].update(status='cancelled'),
            lambda r: r['native'].update(running=False), lambda r: r['native'].update(cli_running=False),
            lambda r: r['native'].update(image_id=CONFIG), lambda r: r['native'].update(runtime_id='c' * 64),
            lambda r: r['native'].update(die_at_ns=b.timestamp(date(23))),
            lambda r: r['native'].update(exit_code=0), lambda r: r['native'].update(absent_before_observer=False),
            lambda r: r['native'].update(absent_at_ns=b.timestamp(date(24))),
            lambda r: r.update(finished_log='unrelated substring'),
        ]
        for change in changes:
            with self.subTest(change=changes.index(change)):
                self.record = copy.deepcopy(original)
                change(self.record)
                with self.assertRaises(b.Refused):
                    self.validate()

    def test_seventh_manifest_lane_is_mandatory(self):
        source = (ROOT / 'scripts/coverage-journeys.sh').read_text()
        marker = next(line for line in source.splitlines()
                      if line.startswith('  python3 - "$destination" "$journeys_dir" <<'))
        script = source.split(marker + '\n', 1)[1].split('\nPY', 1)[0]
        env = dict(os.environ, JOURNEY_MANIFEST_SHA='d' * 40, JOURNEY_MANIFEST_IMAGE_ID=IMAGE, JOURNEY_MANIFEST_BUILD_CONTEXT='{}',
                   JOURNEY_MANIFEST_PROVENANCE='built-by-this-run', JOURNEY_MANIFEST_VERIFIED='true', JOURNEY_MANIFEST_BACKEND_SHA256='f' * 64, JOURNEY_MANIFEST_BACKEND_PATH='bound.json')
        for lane in ('local', 'auth', 'distributed', 'owner-memory', 'git-sync', 'sso', 'local-retry'):
            path = self.output / lane
            path.mkdir()
            (path / 'provenance.json').write_text(json.dumps({'complete': True, 'candidate_sha': 'd' * 40, 'image_id': IMAGE}))
        result = subprocess.run([sys.executable, '-c', script, str(self.output / 'manifest.json'), str(self.output)], env=env, capture_output=True, timeout=5)
        self.assertEqual(result.returncode, 0, result.stderr)
        manifest = json.loads((self.output / 'manifest.json').read_text())
        self.assertTrue(manifest['complete'])
        self.assertEqual(len(manifest['lanes']), 7)
        (self.output / 'local-retry' / 'provenance.json').unlink()
        refused = subprocess.run([sys.executable, '-c', script, str(self.output / 'refused.json'), str(self.output)], env=env, capture_output=True, timeout=5)
        self.assertNotEqual(refused.returncode, 0)
        self.assertFalse((self.output / 'refused.json').exists())


class ResourceControls(unittest.TestCase):
    def driver(self):
        d = object.__new__(b.Driver)
        d.ownership_id = 'd' * 40
        d.owner = 'owned'
        d.context = {'coverage_id': 'run'}
        d.image = IMAGE
        d.allocated_ids = {}
        d.absences = {}
        d.inputs = {'task_docker_image_id': INDEX}
        return d

    def test_failure_diagnostics_are_bounded_static_and_cannot_claim_completion(self):
        records = [{'role': role} for role in b.ROLES]
        positive = b.failure_diagnostic(b.Refused('public retry finished log/params missing'), 'observer_finished_log', records)
        self.assertEqual(positive['guard'], 'public retry finished log/params missing')
        self.assertEqual(positive['phase'], 'observer_finished_log')
        self.assertFalse(positive['complete'])
        for error in (RuntimeError('Config.Env secret-value csk_live_' + 'x' * 43), b.Refused('untrusted body ' + 'x' * 10000), KeyError('raw native Error')):
            value = b.failure_diagnostic(error, 'untrusted ' + 'x' * 10000, records * 100)
            serialized = json.dumps(value)
            self.assertLess(len(serialized), 400)
            self.assertNotIn('Config.Env', serialized)
            self.assertNotIn('csk_live_', serialized)
            self.assertNotIn('secret-value', serialized)
            self.assertEqual(value['guard'], 'details_omitted')
            self.assertEqual(value['phase'], 'phase_unavailable')
            self.assertFalse(value['complete'])
        timeout = b.failure_diagnostic(TimeoutError('raw socket error'), 'natural_retry', [])
        self.assertEqual(timeout['category'], 'bounded_timeout')

    def test_entrypoint_keeps_unexpected_traceback_metadata_but_withholds_messages(self):
        tree = ast.parse(Path(b.__file__).read_text())
        entrypoint = ast.Module(body=[tree.body[-1]], type_ignores=[])

        def fail_unexpectedly():
            raise RuntimeError('SECRET_NATIVE_TOKEN')

        namespace = dict(vars(b), __name__='__main__', main=fail_unexpectedly)
        with patch.object(b.sys, 'stderr', new=io.StringIO()) as stderr:
            with self.assertRaises(SystemExit) as raised:
                exec(compile(entrypoint, b.__file__, 'exec'), namespace)
        self.assertEqual(raised.exception.code, 1)
        diagnostic = json.loads(stderr.getvalue())
        self.assertEqual(diagnostic['error'], 'unexpected-local-retry-exception')
        self.assertEqual(diagnostic['exception_type'], 'builtins.RuntimeError')
        self.assertEqual(diagnostic['traceback'][-1]['function'], 'fail_unexpectedly')
        self.assertNotIn('SECRET', stderr.getvalue())

        def refuse_deliberately():
            raise b.Refused('SECRET_REFUSAL')

        namespace['main'] = refuse_deliberately
        with patch.object(b.sys, 'stderr', new=io.StringIO()) as stderr:
            with self.assertRaises(SystemExit) as raised:
                exec(compile(entrypoint, b.__file__, 'exec'), namespace)
        self.assertEqual(raised.exception.code, 1)
        self.assertEqual(stderr.getvalue(),
                         'Local retry guard refused: Refused (raw diagnostics withheld)\n')

    def test_driver_run_unexpected_exception_is_recorded_by_actual_main_path(self):
        saved = {}

        class FakeDriver:
            def __init__(self, context, inputs, output):
                self.phase = 'observer_finished_log'
                self.processes = []
                self.report = {}

            def run(self):
                raise KeyError('SECRET_NATIVE_BODY')

            def cleanup(self):
                return []

            def save(self, name, value):
                saved[name] = copy.deepcopy(value)

        with tempfile.TemporaryDirectory() as output_dir:
            argv = ['coverage-local-retry', '--context', 'ctx', '--context-sha256', 'a' * 64,
                    '--inputs', 'inputs', '--inputs-sha256', 'b' * 64, '--output', output_dir, '--run']
            with patch.object(b.sys, 'argv', argv), \
                 patch.object(b, 'load', return_value=({}, {})), \
                 patch.object(b, 'Driver', FakeDriver), \
                 patch.object(b.signal, 'alarm'), patch.object(b.signal, 'signal'), \
                 patch.object(b.sys, 'stderr', new=io.StringIO()) as stderr:
                self.assertEqual(b.main(), 1)
        diagnostic = saved['failure-diagnostic.json']
        self.assertEqual(diagnostic['category'], 'unexpected_exception')
        self.assertEqual(diagnostic['exception_type'], 'builtins.KeyError')
        self.assertEqual(diagnostic['traceback'][-1]['function'], 'run')
        self.assertNotIn('SECRET_NATIVE_BODY', json.dumps(diagnostic) + stderr.getvalue())
        self.assertIn('Local retry journey refused', stderr.getvalue())

    def test_finished_log_exact_payload_with_native_timestamp_framing(self):
        expected = 'FINISHED_owned-natural-drain:owned-natural-drain'
        for log in (expected, expected + '\n', '2026-10-05T19:39:01.205028466Z ' + expected,
                    'START_other\n2026-10-05T19:39:01Z ' + expected + '\n',
                    '2026-10-05T19:39:01.2+01:30 ' + expected):
            with self.subTest(log=log):
                self.assertTrue(b.finished_log_matches(log, expected))
        for log in ('other ' + expected, expected + ':suffix', 'prefix' + expected,
                    '2026-10-05T19:39:01.205028466Z  ' + expected,
                    '2026-13-05T19:39:01Z ' + expected,
                    '2026-10-05T25:39:01Z ' + expected,
                    '2026-10-05T19:39:01.1234567890Z ' + expected,
                    '2026-10-05T19:39:01+01:99 ' + expected,
                    '2026-10-05T19:39:01+24:00 ' + expected,
                    '2026-10-05T19:39:01Z FINISHED_other:other',
                    '2026-10-05T19:39:01Z ' + expected + ' extra'):
            with self.subTest(log=log):
                self.assertFalse(b.finished_log_matches(log, expected))

    def test_removal_poll_validates_one_inspect_or_literal_absence(self):
        d = self.driver()
        identity = 'a' * 64
        obj = {'Id': identity, 'Image': INDEX, 'Name': '/' + TASKS[1] + '-' + RUN + '-' + ROWS[1],
               'Config': {'Env': ['COVERAGE_LOCAL_RETRY_OWNER=owned', 'CAESIUM_RUN_ID=' + RUN]},
               'State': {'Running': False, 'OOMKilled': False}}
        calls = []
        # If removal occurs after these returned bytes, a second read fails.
        # The poll must validate the captured object without another inspect.
        def docker(*args, **kwargs):
            calls.append((args, kwargs))
            self.assertEqual(len(calls), 1)
            return subprocess.CompletedProcess(args, 0, json.dumps([obj]), '')
        d.docker = docker
        d.inspect = lambda *args: self.fail('second inspect reintroduces removal race')
        self.assertFalse(d.native_removed(identity, JOB, RUN, TASKS[1], ROWS[1]))
        self.assertEqual(calls[0][0], ('container', 'inspect', identity))
        self.assertFalse(calls[0][1]['check'])
        for result in (subprocess.CompletedProcess([], 1, '[]', 'Error response from daemon: No such container: ' + identity),):
            d.docker = lambda *args, **kwargs: result
            self.assertTrue(d.native_removed(identity, JOB, RUN, TASKS[1], ROWS[1]))
        for result in (subprocess.CompletedProcess([], 1, '', 'permission denied'),
                       subprocess.CompletedProcess([], 1, '[]', 'Error response from daemon: No such container: foreign'),
                       subprocess.CompletedProcess([], 0, '[]', ''),
                       subprocess.CompletedProcess([], 0, '{}', ''),
                       subprocess.CompletedProcess([], 0, '[not JSON', '')):
            with self.subTest(result=result), self.assertRaises((b.Refused, ValueError)):
                d.docker = lambda *args, **kwargs: result
                d.native_removed(identity, JOB, RUN, TASKS[1], ROWS[1])
        obj['Id'] = 'b' * 64
        d.docker = lambda *args, **kwargs: subprocess.CompletedProcess(args, 0, json.dumps([obj]), '')
        with self.assertRaises(b.Refused):
            d.native_removed(identity, JOB, RUN, TASKS[1], ROWS[1])

    def test_native_guard_checks_exact_ids_owner_env_name_and_image(self):
        d = self.driver()
        obj = {'Id': 'a' * 64, 'Image': INDEX, 'Name': '/' + TASKS[1] + '-' + RUN + '-' + ROWS[1],
               'Config': {'Env': ['COVERAGE_LOCAL_RETRY_OWNER=owned', 'CAESIUM_RUN_ID=' + RUN]}, 'State': {'Running': True, 'OOMKilled': False}}
        d.inspect = lambda *args: obj
        d.native('a' * 64, JOB, RUN, TASKS[1], ROWS[1])
        for key, value in (('Id', 'c' * 64), ('Image', CONFIG), ('Name', '/foreign'), ('Config', {'Env': []}), ('State', {'OOMKilled': True})):
            with self.subTest(key=key):
                saved = obj[key]
                obj[key] = value
                with self.assertRaises(b.Refused):
                    d.native('a' * 64, JOB, RUN, TASKS[1], ROWS[1])
                obj[key] = saved

    def test_foreign_resource_and_remove_failure_refuse_without_claimed_absence(self):
        d = self.driver()
        obj = {'Id': 'a' * 64, 'Name': '/name', 'Image': IMAGE, 'Config': {'Labels': {b.backend.LABEL_OWNER: d.ownership_id, b.backend.LABEL_RUN: 'run', 'caesium.coverage.backend-owner': 'owned'}}}
        calls = []
        def docker(*args, **kwargs):
            calls.append(args)
            if args[1] == 'inspect':
                return subprocess.CompletedProcess(args, 0, json.dumps([obj]), '')
            raise b.Refused('rm failed')
        d.docker = docker
        obj['Config']['Labels'][b.backend.LABEL_OWNER] = 'foreign'
        with self.assertRaises(b.Refused):
            d.remove_owned('container', 'name')
        self.assertFalse(any(c[1] == 'rm' for c in calls))
        obj['Config']['Labels'][b.backend.LABEL_OWNER] = d.ownership_id
        with self.assertRaises(b.Refused):
            d.remove_owned('container', 'name')
        self.assertTrue(any(c[1] == 'rm' for c in calls))

    def test_cleanup_error_retains_refusal_not_complete(self):
        with tempfile.TemporaryDirectory() as directory:
            d = self.driver()
            d.output = Path(directory)
            d.private = d.output / 'private'
            d.private.mkdir()
            d.resources = [('container', 'owned-cli')]
            d.known_rows = {}
            d.report = {'complete': True}
            d.remove_owned = lambda *args: (_ for _ in ()).throw(b.Refused('rm failure'))
            self.assertTrue(d.cleanup())
            record = json.loads((d.output / 'provenance.json').read_text())
            self.assertFalse(record['complete'])
            self.assertTrue(record['missing'])
            self.assertEqual(d.resources, [('container', 'owned-cli')])

    def test_start_captures_admitted_identity_before_profile_refusal(self):
        d = self.driver()
        d.report = {'job_id': JOB}
        d.known_rows = {}
        d.create = lambda *args, **kwargs: ('a' * 64, Path('raw'))
        d.docker = lambda *args, **kwargs: subprocess.CompletedProcess(args, 0, RUN + '\n', '')
        d.wait = lambda *args: True
        def snapshot(job_id, run_id):
            self.assertEqual((job_id, run_id), (JOB, RUN))
            d.known_rows = dict(zip(TASKS, ROWS))
        d.snapshot = snapshot
        d.record = lambda *args: (_ for _ in ()).throw(b.Refused('partial raw profile'))
        with self.assertRaises(b.Refused):
            d.cli('start', ['run', 'start'])
        self.assertEqual(d.report['run_id'], RUN)
        self.assertFalse(d.report['admission_uncertain'])
        self.assertEqual(d.known_rows, dict(zip(TASKS, ROWS)))

    def test_failed_identity_read_retains_admission_reconciliation(self):
        d = self.driver()
        d.report = {'job_id': JOB}
        d.create = lambda *args, **kwargs: ('a' * 64, Path('raw'))
        d.docker = lambda *args, **kwargs: subprocess.CompletedProcess(args, 0, RUN + '\n', '')
        d.wait = lambda *args: True
        d.snapshot = lambda *args: (_ for _ in ()).throw(b.Refused('HTTP read failure'))
        with self.assertRaises(b.Refused):
            d.cli('start', ['run', 'start'])
        self.assertEqual(d.report['run_id'], RUN)
        self.assertTrue(d.report['admission_uncertain'])

    def test_actual_flush_failure_retains_reduced_refusal_and_no_profile(self):
        d = self.driver()
        d.server_id = 'a' * 64
        saved = {}
        d.save = lambda name, value: saved.update({name: copy.deepcopy(value)})
        d.record = lambda *args: self.fail('failed flush cannot record eligible process')
        running = True
        obj = {'Id': d.server_id, 'Image': IMAGE, 'Config': {'Labels': {b.backend.LABEL_OWNER: d.ownership_id, b.backend.LABEL_RUN: 'run'}}, 'RestartCount': 0}
        def docker(*args, **kwargs):
            nonlocal running
            if args[1] == 'inspect':
                obj['State'] = {'Running': running, 'ExitCode': 0, 'OOMKilled': False}
                return subprocess.CompletedProcess(args, 0, json.dumps([obj]), '')
            if args[1] == 'kill':
                return subprocess.CompletedProcess(args, 1, '', 'withheld')
            if args[1] == 'stop':
                running = False
                return subprocess.CompletedProcess(args, 0, '', '')
            self.fail('unexpected mutation')
        d.docker = docker
        with patch.object(b.common.time, 'sleep', return_value=None), self.assertRaises(b.common.JourneyError):
            d.stop_server(1)
        diagnostic = saved['server-1-stop.json']
        self.assertEqual(diagnostic['flush_rc'], 1)
        self.assertEqual(diagnostic['refusal_category'], 'flush_failed')
        self.assertNotIn('Config', diagnostic['pre_stop'])

    def test_actual_retry_discovery_requests_full_native_ids(self):
        d = self.driver()
        d.create = lambda *args, **kwargs: ('a' * 64, Path('raw'))
        d.docker = lambda *args, **kwargs: calls.append(args) or subprocess.CompletedProcess(args, 0, 'b' * 64 if args[0] == 'ps' else '', '')
        d.owned = lambda *args: {'State': {'Running': True}}
        d.native = lambda *args: {'Name': '/' + TASKS[1] + '-' + RUN + '-' + ROWS[1], 'State': {'Running': True}, 'Image': INDEX}
        d.native_resources = []
        d.save = lambda *args: None
        calls = []
        def wait(fn, label, timeout):
            if label == 'actual native retry Running':
                value = fn()
                self.assertEqual(value['runtime_id'], 'b' * 64)
                raise b.Refused('stop test after actual discovery')
            raise AssertionError('unexpected wait')
        d.wait = wait
        before = {'rows': {'failed': {'task_id': TASKS[1], 'task_run_id': ROWS[1], 'runtime_id': 'c' * 64}}}
        with self.assertRaises(b.Refused):
            d.retry(JOB, RUN, before)
        ps = [args for args in calls if args[0] == 'ps']
        self.assertEqual(len(ps), 1)
        self.assertIn('--no-trunc', ps[0])

    def test_actual_log_read_uses_registered_public_route(self):
        d = self.driver()
        calls = []
        d.http = lambda path, **kwargs: calls.append((path, kwargs)) or 'finished'
        self.assertEqual(d.read_log(JOB, RUN, TASKS[1]), 'finished')
        self.assertEqual(calls, [('/v1/jobs/' + JOB + '/runs/' + RUN + '/logs?task_id=' + TASKS[1], {'text': True})])

    def test_actual_snapshot_uses_concrete_row_and_full_precision(self):
        d = self.driver()
        d.task_names = {TASKS[0]: 'preserved', TASKS[1]: 'failed'}
        d.known_rows = {}
        tasks = [{'task_id': TASKS[i], 'id': TASKS[i], 'output': {'marker': 'owned'} if i == 0 else {},
                  'started_at': '2026-10-05T12:00:06.123456789Z', 'completed_at': '2026-10-05T12:00:08.987654321Z'} for i in range(2)]
        calls = []
        def http(path):
            calls.append(path)
            if path.endswith('/partitions'):
                index = TASKS.index(path.split('/')[-2])
                return {'total': 1, 'partitions': [{'task_run_id': ROWS[index], 'started_at': date(6), 'completed_at': date(8)}]}
            return {'id': RUN, 'job_id': JOB, 'tasks': tasks}
        d.http = http
        observed = d.snapshot(JOB, RUN)
        self.assertEqual(observed['rows']['failed']['task_run_id'], ROWS[1])
        self.assertEqual(observed['rows']['failed']['completed_at'], tasks[1]['completed_at'])
        self.assertEqual(d.known_rows, dict(zip(TASKS, ROWS)))
        self.assertTrue(all('/tasks/' + task_id + '/partitions' in calls[i + 1] for i, task_id in enumerate(TASKS)))

    def test_allocation_args_retry_omits_server_and_uses_explicit_same_volume(self):
        with tempfile.TemporaryDirectory() as directory:
            d = self.driver()
            d.output = Path(directory)
            d.network = 'owned-net'
            d.database = 'owned-db'
            d.inputs.update(platform='linux/arm64', docker_socket='/owned/docker.sock')
            d.register = lambda *args: None
            d.owned = lambda *args: {'Id': 'a' * 64, 'Image': IMAGE}
            calls = []
            d.docker = lambda *args, **kwargs: calls.append(args) or subprocess.CompletedProcess(args, 0, 'a' * 64, '')
            argv = ['run', 'retry', '--job-id', JOB, '--run-id', RUN]
            d.create('retry', argv, local=True)
            actual = calls[0]
            self.assertEqual(list(actual[-len(argv):]), argv)
            self.assertNotIn('--server', actual)
            self.assertIn('type=volume,src=owned-db,dst=/var/lib/caesium/dqlite', actual)
            self.assertIn('CAESIUM_NODE_ADDRESS=127.0.0.1:9001', actual)
            self.assertIn('CAESIUM_WORKER_ENABLED=false', actual)
            self.assertIn('CAESIUM_RUN_OWNER_ENABLED=false', actual)


if __name__ == '__main__':
    unittest.main()
