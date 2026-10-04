#!/usr/bin/env python3
"""Hermetic refusal/provenance tests; never invokes Docker or a backend API."""
import base64
import copy
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location('coverage_backends', Path(__file__).with_name('coverage-backends.py'))
b = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(b)
IMAGE = 'sha256:' + '1' * 64
BUILDER = 'sha256:' + '2' * 64
INDEX = 'sha256:' + '3' * 64
SHA = '4' * 40


class BackendGuards(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)

    def context(self, arch='arm64'):
        build = {'goos': 'linux', 'goarch': arch, 'cgo_enabled': True, 'compiler': 'gc', 'build_tags': [], 'release_tags': [], 'tool_tags': []}
        inventory = self.root / ('inventory-' + arch + '.json')
        inventory.write_text(json.dumps({'schema_version': 1, 'kind': 'go-ast-source-inventory', 'parser': 'go/parser', 'complete': True,
                                         'candidate_sha': SHA, 'image_id': IMAGE, 'build_context': build, 'files': {'internal/atom/x.go': {}}, 'packages': {'internal/atom': ['internal/atom/x.go']}}))
        return {'schema_version': 1, 'producer': 'scripts/integration-coverage.sh', 'candidate_sha': SHA, 'image_id': IMAGE, 'builder_image_id': BUILDER,
                'coverage_id': 'owned-coverage', 'build_context': build, 'image_provenance': 'built-by-this-run', 'verified': True, 'platform': 'linux/' + arch,
                'container_cli': 'docker', 'repository_root': str(self.root), 'source_inventory': {'path': str(inventory), 'sha256': b.digest(inventory)},
                '_context_sha256': '5' * 64, '_inputs_sha256': '6' * 64}

    def archive(self, arch='arm64', tags=None, extra=None):
        config = json.dumps({'os': 'linux', 'architecture': arch, 'config': {}}).encode()
        image_id = 'sha256:' + hashlib.sha256(config).hexdigest()
        path = self.root / ('task-' + arch + '.tar')
        with tarfile.open(path, 'w') as tar:
            for name, data in [('manifest.json', json.dumps([{'Config': 'config.json', 'RepoTags': tags or ['alpine:3.23'], 'Layers': []}]).encode()), ('config.json', config)]:
                entry = tarfile.TarInfo(name)
                entry.size = len(data)
                tar.addfile(entry, io.BytesIO(data))
            if extra:
                tar.addfile(extra)
        return {'schema_version': 1, 'platform': 'linux/' + arch, 'docker_socket': '/owned/docker.sock', 'task_archive': str(path), 'task_archive_sha256': b.digest(path),
                'task_image_id': image_id, 'task_docker_image_id': INDEX, 'task_image_ref': 'alpine:3.23', 'kind_image_id': IMAGE, 'podman_service_image_id': BUILDER,
                'podman_privileged_approved': True, '_inputs_sha256': '6' * 64}

    def raw(self, name='raw'):
        raw = self.root / name
        raw.mkdir()
        (raw / 'covmeta.original').write_bytes(b'original metadata')
        (raw / 'covcounters.original.1').write_bytes(b'original counters')
        return raw

    def test_producer_inventory_is_bound_and_both_architectures_work(self):
        for arch in ('arm64', 'amd64'):
            context = self.context(arch)
            self.assertIs(b.validate_context(context), context)
            for mutate in (lambda c: c.update(verified=False), lambda c: c.update(image_provenance='supplied'),
                           lambda c: c.update(builder_image_id='latest'), lambda c: c.update(platform='linux/other'),
                           lambda c: c.update(image_id=INDEX), lambda c: c['source_inventory'].update(sha256='0' * 64)):
                foreign = copy.deepcopy(context)
                mutate(foreign)
                with self.assertRaises(b.Refused):
                    b.validate_context(foreign)

    def test_relative_inventory_is_portable_but_cannot_escape_artifact_root(self):
        context = self.context()
        inventory = Path(context['source_inventory']['path'])
        context['artifact_dir'] = str(self.root)
        context['source_inventory']['path'] = inventory.name
        b.validate_context(context)
        context['source_inventory']['path'] = '../' + inventory.name
        with self.assertRaises(b.Refused):
            b.validate_context(context)

    def test_archive_config_identity_is_distinct_from_docker_index(self):
        for arch in ('arm64', 'amd64'):
            inputs = self.archive(arch)
            validated = b.validate_inputs(inputs, ['kubernetes', 'podman'])
            self.assertNotEqual(validated['task_image_id'], validated['task_docker_image_id'])
            foreign = dict(inputs, task_image_id=INDEX)
            with self.assertRaisesRegex(b.Refused, 'config/image mismatch'):
                b.validate_inputs(foreign, ['podman'])
            with self.assertRaises(b.Refused):
                b.archive_identity(inputs['task_archive'], inputs['task_archive_sha256'], inputs['task_image_id'], inputs['task_image_ref'], 'amd64' if arch == 'arm64' else 'arm64')
        inputs = self.archive(tags=['alpine:3.23', 'foreign:latest'])
        with self.assertRaises(b.Refused):
            b.validate_inputs(inputs, ['kubernetes'])
        link = tarfile.TarInfo('unowned')
        link.type, link.linkname = tarfile.SYMTYPE, '/foreign'
        inputs = self.archive(extra=link)
        with self.assertRaises(b.Refused):
            b.validate_inputs(inputs, ['kubernetes'])

    def test_missing_podman_prerequisite_refuses_before_allocation(self):
        inputs = self.archive()
        inputs.pop('podman_service_image_id')
        with self.assertRaisesRegex(b.Refused, 'conditional prerequisite unavailable'):
            b.validate_inputs(inputs, ['kubernetes', 'podman'])
        b.validate_inputs(inputs, ['kubernetes'])  # partial prerequisite plan is explicitly allowed

    def test_daemon_inventory_errors_are_not_absence(self):
        result = lambda stderr, stdout='[]', code=1: subprocess.CompletedProcess([], code, stdout, stderr)
        self.assertTrue(b.missing_object(result('Error response from daemon: get owned: no such volume\n'), 'volume', 'owned'))
        self.assertTrue(b.missing_object(result('Error response from daemon: network owned not found'), 'network', 'owned'))
        self.assertTrue(b.missing_object(result('Error response from daemon: No such container: owned'), 'container', 'owned'))
        for error in ('dial unix /owned/docker.sock: no such file or directory', 'permission denied', 'Error response from daemon: get foreign: no such volume',
                      'Error response from daemon: get owned: no such volume\nother error'):
            self.assertFalse(b.missing_object(result(error), 'volume', 'owned'))
        self.assertFalse(b.missing_object(result('Error response from daemon: get owned: no such volume', '[{}]'), 'volume', 'owned'))

    def test_private_kubeconfig_rejects_ambient_or_foreign_authority(self):
        cert = base64.b64encode(b'private test TLS material').decode()
        config = {'kind': 'Config', 'current-context': 'kind-owned', 'clusters': [{'name': 'kind-owned', 'cluster': {'server': 'https://owned-control-plane:6443', 'certificate-authority-data': cert}}],
                  'users': [{'name': 'kind-owned', 'user': {'client-certificate-data': cert, 'client-key-data': cert}}],
                  'contexts': [{'name': 'kind-owned', 'context': {'cluster': 'kind-owned', 'user': 'kind-owned'}}]}
        b.validate_kubeconfig(config, 'owned', 'owned-control-plane')
        for mutate in (lambda c: c['clusters'][0]['cluster'].update(server='https://foreign:6443'),
                       lambda c: c['clusters'][0]['cluster'].update({'insecure-skip-tls-verify': True}),
                       lambda c: c['users'][0]['user'].update(exec={'command': 'foreign-command'}),
                       lambda c: c['users'][0]['user'].update({'client-key': '/foreign/key'}),
                       lambda c: c['clusters'].append(c['clusters'][0])):
            foreign = copy.deepcopy(config)
            mutate(foreign)
            with self.assertRaises(b.Refused):
                b.validate_kubeconfig(foreign, 'owned', 'owned-control-plane')

    def test_backend_outcome_requires_real_exit_and_terminal_task(self):
        task = {'runtime_id': 'actual-native-id', 'cache_hit': False, 'status': 'succeeded', 'exit_code': 0, 'output': {'marker': 'owned'}, 'completed_at': 'observed'}
        success = {'status': 'succeeded', 'completed_at': 'observed', 'tasks': [task, dict(task, output={})]}
        b.verify_outcome(success, 'success', 'owned')
        failure = {'status': 'failed', 'completed_at': 'observed', 'tasks': [dict(task, status='failed', exit_code=17)]}
        b.verify_outcome(failure, 'failure', 'owned')
        with self.assertRaises(b.Refused):
            b.verify_outcome(dict(failure, tasks=[dict(task, status='failed', exit_code=None)]), 'failure', 'owned')
        deadline = {'status': 'failed', 'completed_at': 'observed', 'error': 'run timed out after 15s', 'tasks': [dict(task, status='failed')]}
        b.verify_outcome(deadline, 'deadline', 'owned')
        for cause in ('task timed out after 15s', 'run timed out after 10s', 'context deadline exceeded', 'engine connection failed', 'image pull failed'):
            with self.assertRaises(b.Refused):
                b.verify_outcome(dict(deadline, error=cause), 'deadline', 'owned')
        with self.assertRaises(b.Refused):
            b.verify_outcome(dict(deadline, tasks=[dict(task, status='running', completed_at=None)]), 'deadline', 'owned')
        for bad_task in (dict(task, runtime_id=''), dict(task, cache_hit=True)):
            with self.assertRaises(b.Refused):
                b.verify_outcome(dict(success, tasks=[bad_task, task]), 'success', 'owned')

    def test_raw_pair_does_not_accept_missing_empty_or_symlink_counters(self):
        raw = self.raw()
        self.assertEqual(len(b.coverage_files(raw)), 2)
        counter = raw / 'covcounters.original.1'
        counter.write_bytes(b'')
        with self.assertRaises(b.Refused):
            b.coverage_files(raw)
        counter.unlink()
        counter.symlink_to(raw / 'covmeta.original')
        with self.assertRaises(b.Refused):
            b.coverage_files(raw)
        counter.unlink()
        with self.assertRaises(b.Refused):
            b.coverage_files(raw)

    def test_original_process_provenance_and_suite_eligibility(self):
        driver = object.__new__(b.Driver)
        driver.context, driver.inputs = self.context(), self.archive()
        driver.backend, driver.image, driver.binary_hash, driver.output, driver.processes = 'podman', IMAGE, 'a' * 64, self.root, []
        driver.report = {'cases': [{'case': case, 'status': 'pass'} for case in ('success', 'failure', 'deadline')]}
        obj = {'Id': 'b' * 64, 'Image': IMAGE, 'RestartCount': 0, 'State': {'ExitCode': 0, 'Running': False, 'OOMKilled': False}}
        for index in range(6):
            driver.record_process('cli', obj, self.raw('raw-cli-' + str(index)), ['run', 'start'])
        with self.assertRaises(b.Refused):
            driver.record_process('server', obj, self.raw('invalid-server'), ['start'], 0, None)
        driver.record_process('server', obj, self.raw('raw-server'), ['start'], 0, 0)
        self.assertTrue(all(not p['complete'] for p in driver.processes))
        driver.finalize_processes()
        for record in driver.processes:
            self.assertTrue(record['complete'])
            self.assertEqual(record['builder_image_id'], BUILDER)
            self.assertEqual(record['source_inventory_sha256'], driver.context['source_inventory']['sha256'])
            self.assertEqual(record['backend_receipts']['task_docker_image_id'], INDEX)
            self.assertEqual(record['test_suite']['passed_scenarios'], 3)
            if record['source'] == 'cli':
                self.assertEqual(record['flush'], 'process-exit')
                self.assertIsNone(record['signal'])
            else:
                self.assertEqual((record['signal'], record['flush_rc'], record['stop_rc']), ('SIGTERM', 0, 0))
        driver.report['cases'][-1]['status'] = 'fail'
        with self.assertRaises(b.Refused):
            driver.finalize_processes()

    def test_cleanup_refuses_foreign_labels_and_removes_only_inspected_id(self):
        driver = object.__new__(b.Driver)
        driver.context = {'coverage_id': 'owned-run'}
        driver.ownership_id, driver.owner = SHA, 'owned-backend'
        owned = {'Id': 'c' * 64, 'Config': {'Labels': {b.LABEL_OWNER: SHA, b.LABEL_RUN: 'owned-run', 'caesium.coverage.backend-owner': 'owned-backend'}}}
        calls = []
        driver.docker = lambda *args, **kwargs: (calls.append(args) or subprocess.CompletedProcess(args, 0, json.dumps([owned]), ''))
        driver.remove_owned('container', 'owned-name')
        self.assertEqual(calls[-1], ('container', 'rm', '-f', '-v', 'c' * 64))
        owned['Config']['Labels'][b.LABEL_OWNER] = 'foreign'
        calls.clear()
        with self.assertRaises(b.Refused):
            driver.remove_owned('container', 'owned-name')
        self.assertFalse(any('rm' in call for call in calls))

    def test_interrupted_command_joins_group_before_cleanup(self):
        driver = object.__new__(b.Driver)
        driver.env = {}
        process = unittest.mock.Mock(pid=1234)
        process.communicate.side_effect = [b.Refused('interrupted'), ('', '')]
        with patch.object(b.subprocess, 'Popen', return_value=process), patch.object(b.os, 'killpg') as kill:
            with self.assertRaises(b.Refused):
                driver.command(['never-executed'])
        kill.assert_called_once_with(1234, b.signal.SIGTERM)
        self.assertEqual(process.communicate.call_count, 2)

    def test_offline_smoke_plan_needs_no_context_or_resource_allocation(self):
        inputs = self.archive()
        path = self.root / 'backend-producer-inputs.json'
        path.write_text(json.dumps(inputs))
        output = self.root / 'never-allocated'
        argv = ['coverage-backends.py', '--smoke', '--backend', 'podman', '--inputs', str(path), '--inputs-sha256', b.digest(path), '--output', str(output)]
        with patch.object(b.sys, 'argv', argv), patch.object(b.sys, 'stdout', new=io.StringIO()) as stdout, patch.object(b, 'Driver', side_effect=AssertionError('resource allocation attempted')):
            self.assertEqual(b.main(), 0)
            plan = json.loads(stdout.getvalue())
        self.assertIsNone(plan['candidate_sha'])
        self.assertTrue(plan['smoke'])
        self.assertFalse(output.exists())

    def test_normalized_containerd_reference_retains_exact_tag(self):
        self.assertEqual(b.normalized_image_ref('alpine:3.23'), 'docker.io/library/alpine:3.23')
        self.assertEqual(b.normalized_image_ref('owned/task:version'), 'docker.io/owned/task:version')
        self.assertEqual(b.normalized_image_ref('registry.example/owned/task:v1'), 'registry.example/owned/task:v1')

    def test_partial_or_failed_backend_cannot_publish_complete_contribution(self):
        context = self.context()
        process = {'backend': 'podman', 'lane': 'owned', 'source': 'cli', 'raw_dir': 'podman/raw', 'provenance_path': 'podman/provenance'}
        report = {'complete': True, 'cleanup_errors': [], 'processes': [process] * 7}
        self.assertFalse(b.contribution(context, [report], ['podman'])['complete'])
        failed = dict(report, failed=True)
        self.assertFalse(b.contribution(context, [report, failed], ['kubernetes', 'podman'])['complete'])
        with self.assertRaises(b.Refused):
            b.fresh_directory(self.root)
        link = self.root / 'alias'
        link.symlink_to(self.root, target_is_directory=True)
        with self.assertRaises(b.Refused):
            b.fresh_directory(link / 'new')


if __name__ == '__main__':
    unittest.main()
