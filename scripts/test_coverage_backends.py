#!/usr/bin/env python3
"""Hermetic guard/loopback HTTP tests; never invokes Docker or a real backend."""
import base64
import copy
from contextlib import contextmanager
import http.client
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import socketserver
import tarfile
import tempfile
import threading
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

    def test_oci_archive_requires_complete_digest_bound_descriptor_closure(self):
        def archive(omit=None, corrupt=None, wrong_platform=False):
            blobs = {}
            def descriptor(value, media):
                data = json.dumps(value).encode() if isinstance(value, dict) else value
                identity = 'sha256:' + hashlib.sha256(data).hexdigest()
                name = 'blobs/sha256/' + identity.removeprefix('sha256:')
                blobs[name] = data
                return {'mediaType': media, 'digest': identity, 'size': len(data)}
            config = descriptor({'os': 'linux', 'architecture': 'arm64'}, 'application/vnd.oci.image.config.v1+json')
            layer = descriptor(b'original task layer', 'application/vnd.oci.image.layer.v1.tar')
            manifest = descriptor({'schemaVersion': 2, 'config': config, 'layers': [layer]}, 'application/vnd.oci.image.manifest.v1+json')
            manifest['platform'] = {'os': 'linux', 'architecture': 'amd64' if wrong_platform else 'arm64'}
            index = descriptor({'schemaVersion': 2, 'manifests': [manifest]}, 'application/vnd.oci.image.index.v1+json')
            blobs['index.json'] = json.dumps({'schemaVersion': 2, 'manifests': [index]}).encode()
            blobs['oci-layout'] = b'{"imageLayoutVersion":"1.0.0"}'
            config_path = 'blobs/sha256/' + config['digest'].removeprefix('sha256:')
            blobs['manifest.json'] = json.dumps([{'Config': config_path, 'RepoTags': ['alpine:3.23'], 'Layers': []}]).encode()
            if omit == 'layer': blobs.pop('blobs/sha256/' + layer['digest'].removeprefix('sha256:'))
            if omit == 'index': blobs.pop('blobs/sha256/' + index['digest'].removeprefix('sha256:'))
            if corrupt == 'layer': blobs['blobs/sha256/' + layer['digest'].removeprefix('sha256:')] = b'corrupted task data'
            if corrupt == 'same-size': blobs['blobs/sha256/' + layer['digest'].removeprefix('sha256:')] = b'x' * layer['size']
            path = self.root / 'oci-task.tar'
            with tarfile.open(path, 'w') as tar:
                for name, data in blobs.items():
                    entry = tarfile.TarInfo(name)
                    entry.size = len(data)
                    tar.addfile(entry, io.BytesIO(data))
            return path, b.digest(path), config['digest']
        path, archive_hash, config_id = archive()
        b.archive_identity(path, archive_hash, config_id, 'alpine:3.23', 'arm64')
        for args in ({'omit': 'layer'}, {'omit': 'index'}, {'corrupt': 'layer'}, {'corrupt': 'same-size'}, {'wrong_platform': True}):
            path, archive_hash, config_id = archive(**args)
            with self.assertRaises(b.Refused):
                b.archive_identity(path, archive_hash, config_id, 'alpine:3.23', 'arm64')

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
        for bad_task in (dict(task, runtime_id=''), dict(task, cache_hit=True), dict(task, completed_at=None)):
            with self.assertRaises(b.Refused):
                b.verify_outcome(dict(success, tasks=[bad_task, task]), 'success', 'owned')

    def test_collapsed_public_task_id_is_not_a_concrete_log_selector(self):
        catalog_id = '11111111-1111-4111-8111-111111111111'
        actual_instance_id = '22222222-2222-4222-8222-222222222222'
        # Store collapseFanOutGroups exposes head.ID=TaskID, not concrete PK.
        public = {'id': catalog_id, 'task_id': catalog_id, 'runtime_id': actual_instance_id, 'partition_count': 0}
        path = b.unfanned_log_path('job', 'run', public)
        self.assertTrue(path.endswith('/logs?task_id=' + catalog_id))
        self.assertNotIn('task_run_id=', path)
        self.assertNotIn(actual_instance_id, path)
        for unexpected in (dict(public, partition_count=2), dict(public, partition_value='a')):
            with self.assertRaises(b.Refused):
                b.unfanned_log_path('job', 'run', unexpected)

    def test_deadline_log_reads_only_live_bounded_prefix_and_closes_reader(self):
        driver = object.__new__(b.Driver)
        driver.base = 'http://owned-server'
        task = {'id': '11111111-1111-4111-8111-111111111111', 'task_id': '11111111-1111-4111-8111-111111111111', 'partition_count': 0}
        class Stream:
            status = 200
            headers = {'Content-Type': 'text/plain; charset=utf-8'}
            def __init__(self, line): self.line, self.closed = line, False
            def __enter__(self): return self
            def __exit__(self, *args): self.closed = True
            def readline(self, maximum): return self.line[:maximum]
            def read(self, *args): raise AssertionError('whole live stream must not be drained')
        stream = Stream(b'DEADLINE_owned\n')
        with patch.object(b.urllib.request, 'urlopen', return_value=stream) as request:
            self.assertEqual(driver.live_deadline_log('job', 'run', task, 'owned'), 'DEADLINE_owned\n')
        self.assertTrue(stream.closed)
        self.assertEqual(request.call_args.kwargs['timeout'], 8)
        self.assertNotIn('task_run_id', request.call_args.args[0].full_url)
        for line, status, content in [(b'', 204, 'text/plain'), (b'DEADLINE_owned\n', 200, 'application/json'),
                                      (b'DEADLINE_owned', 200, 'text/plain'), (b'other\n', 200, 'text/plain'),
                                      (b'DEADLINE_owned' + b'x' * 65536 + b'\n', 200, 'text/plain')]:
            bad = Stream(line)
            bad.status, bad.headers = status, {'Content-Type': content}
            with patch.object(b.urllib.request, 'urlopen', return_value=bad):
                with self.assertRaises(b.Refused): driver.live_deadline_log('job', 'run', task, 'owned')
            self.assertTrue(bad.closed)
        with patch.object(b.urllib.request, 'urlopen', return_value=Stream(b'DEADLINE_owned\n')), patch.object(b.time, 'monotonic', side_effect=[0, 9]):
            with self.assertRaises(b.Refused): driver.live_deadline_log('job', 'run', task, 'owned')

    def test_native_running_witness_binds_exact_run_task_and_runtime(self):
        driver = object.__new__(b.Driver)
        driver.backend, driver.owner, driver.task_image = 'podman', 'owned', IMAGE
        task = {'runtime_id': 'a' * 64, 'task_id': 'catalog-task'}
        native = {'Id': task['runtime_id'], 'Name': 'catalog-task-run-id', 'Image': IMAGE.removeprefix('sha256:'),
                  'Config': {'Env': ['COVERAGE_BACKEND_OWNER=owned']}, 'State': {'Status': 'running'}}
        driver.podman = lambda *args: subprocess.CompletedProcess(args, 0, json.dumps([native]), '')
        witness = driver.native('run-id', task)
        self.assertEqual((witness['run_id'], witness['task_id'], witness['runtime_id']), ('run-id', 'catalog-task', 'a' * 64))
        native['Name'] = 'another-task-run-id'
        with self.assertRaises(b.Refused): driver.native('run-id', task)
        native['Name'], native['Id'] = 'catalog-task-run-id', 'b' * 64
        with self.assertRaises(b.Refused): driver.native('run-id', task)

    def kube_identity_fixture(self, mutate_before_import=None):
        # Captured Docker29/kind alias shape: the CRI repository alias is not
        # ctr's manifest digest, but both CRI reads identify the archive config.
        driver = object.__new__(b.Driver)
        driver.backend, driver.owner, driver.cluster, driver.namespace = 'kubernetes', 'owned', 'owned', 'owned'
        driver.namespace_uid = '12345678-1234-4234-8234-123456789012'
        driver.task_ref, driver.task_image, driver.architecture = 'alpine:3.23', IMAGE, 'arm64'
        driver.inputs = {'kind_image_id': BUILDER}
        node_name, node_id = 'owned-worker', 'c' * 64
        driver.nodes = {node_name: {'id': node_id, 'role': 'worker', 'image_digests': []}}
        task = {'runtime_id': 'task-run-runtime', 'task_id': 'task'}
        alias = 'docker.io/library/import-2026-10-04@sha256:' + 'b' * 64
        image = {'id': IMAGE, 'repoTags': ['docker.io/library/alpine:3.23'], 'repoDigests': [alias]}
        native_id = 'd' * 64
        pod = {'metadata': {'uid': '22345678-1234-4234-8234-123456789012', 'namespace': 'owned', 'name': task['runtime_id']},
               'spec': {'nodeName': node_name, 'containers': [{'name': 'atom', 'image': 'alpine:3.23',
                        'env': [{'name': 'COVERAGE_BACKEND_OWNER', 'value': 'owned'}]}]},
               'status': {'phase': 'Running', 'containerStatuses': [{'name': 'atom', 'image': 'docker.io/library/alpine:3.23',
                          'imageID': alias, 'containerID': 'containerd://' + native_id, 'restartCount': 0,
                          'state': {'running': {'startedAt': '2026-10-04T20:00:22Z'}}}]}}
        native = {'id': native_id, 'metadata': {'name': 'atom', 'attempt': 0}, 'state': 'CONTAINER_RUNNING',
                  'image': {'image': 'docker.io/library/alpine:3.23'}, 'imageRef': alias,
                  'labels': {'io.kubernetes.pod.uid': pod['metadata']['uid'], 'io.kubernetes.pod.name': task['runtime_id'],
                             'io.kubernetes.pod.namespace': 'owned', 'io.kubernetes.container.name': 'atom'}}
        node = {'Id': node_id, 'Image': BUILDER, 'Config': {'Labels': {'io.x-k8s.kind.cluster': 'owned', 'io.x-k8s.kind.role': 'worker'}}}
        state = {'image': image, 'native': native, 'pod': pod, 'node': node, 'unavailable': False,
                 'config_image': image, 'pod_reads': 0, 'replacement': None}
        def docker(*args):
            self.assertEqual(args[:2], ('exec', node_id))
            if args[2:4] == ('crictl', 'inspecti'):
                if state['unavailable']:
                    raise b.Refused('CRI unavailable')
                self.assertIn(args[4], (IMAGE, 'docker.io/library/alpine:3.23'))
                value = state['config_image'] if args[4] == IMAGE else state['image']
                return subprocess.CompletedProcess(args, 0, json.dumps({'status': value}), '')
            if args[2:4] == ('crictl', 'inspect'):
                self.assertEqual(args[4], native_id)
                return subprocess.CompletedProcess(args, 0, json.dumps({'status': state['native']}), '')
            if args[2:] == ('ctr', '-n', 'k8s.io', 'images', 'list'):
                return subprocess.CompletedProcess(args, 0, 'docker.io/library/alpine:3.23 manifest ' + INDEX, '')
            self.assertEqual(args[2:], ('ctr', '-n', 'k8s.io', 'content', 'get', INDEX))
            return subprocess.CompletedProcess(args, 0, json.dumps({'config': {'digest': IMAGE}}), '')
        def kubectl(*args, **kwargs):
            if args[:2] == ('get', 'namespace'):
                value = {'metadata': {'uid': driver.namespace_uid, 'labels': {'caesium.coverage.backend-owner': 'owned'}}}
            else:
                self.assertEqual(args, ('-n', 'owned', 'get', 'pod', task['runtime_id'], '-o', 'json'))
                state['pod_reads'] += 1
                value = state['replacement'] if state['replacement'] is not None and state['pod_reads'] > 1 else state['pod']
            return subprocess.CompletedProcess(args, 0, json.dumps(value), '')
        driver.docker, driver.kubectl = docker, kubectl
        driver.inspect = lambda kind, identity: state['node']
        if mutate_before_import:
            mutate_before_import(state)
        driver.import_mapping(node_name)
        return driver, task, state

    def test_kube_actual_import_alias_binds_config_tag_node_and_container(self):
        driver, task, state = self.kube_identity_fixture()
        witness = driver.native('run', task)
        self.assertNotIn(witness['image_id'].split('@')[1], driver.nodes['owned-worker']['image_digests'])
        self.assertEqual(witness['config_id'], IMAGE)
        self.assertEqual(witness['cri_container_id'], state['native']['id'])
        self.assertEqual(witness['native_id'], state['pod']['metadata']['uid'])
        self.assertEqual(witness['node_id'], driver.nodes['owned-worker']['id'])
        # Direct config references remain exact immutable identity, never a
        # suffix/substring match for an arbitrary reported digest.
        state['pod']['status']['containerStatuses'][0]['imageID'] = IMAGE
        state['native']['imageRef'] = IMAGE
        self.assertEqual(driver.native('run', task)['image_id'], IMAGE)

    def test_kube_preparation_refuses_wrong_or_unavailable_cri_mapping(self):
        for mutation in (
            lambda st: st['image'].update(id=INDEX),
            lambda st: st.update(config_image=dict(st['image'], repoTags=['foreign:tag'])),
            lambda st: st['image'].update(repoDigests=[]),
            lambda st: st['image'].update(repoDigests=['not-a-digest']),
            lambda st: st.update(unavailable=True),
        ):
            with self.assertRaises(b.Refused):
                self.kube_identity_fixture(mutation)

    def test_kube_wrong_config_unrelated_alias_or_changed_mapping_refuses(self):
        mutations = [
            lambda st: st['image'].update(id=INDEX),
            lambda st: st['pod']['status']['containerStatuses'][0].update(imageID='foreign@' + INDEX),
            lambda st: st['image'].update(repoDigests=['foreign@' + INDEX]),
            lambda st: st['image'].update(repoTags=['docker.io/library/foreign:3.23']),
            lambda st: st['image'].update(repoTags=['docker.io/library/alpine:3.23', 'docker.io/library/alpine:other']),
            lambda st: st['image'].update(repoDigests=st['image']['repoDigests'] * 2),
            lambda st: st.update(config_image=dict(st['image'], id=INDEX)),
            lambda st: st.update(unavailable=True),
        ]
        for index, mutate in enumerate(mutations):
            with self.subTest(mutation=index):
                driver, task, state = self.kube_identity_fixture()
                mutate(state)
                with self.assertRaises(b.Refused):
                    driver.native('run', task)

    def test_kube_concrete_native_or_pod_identity_mismatch_refuses(self):
        mutations = [
            lambda st: st['node'].update(Id='e' * 64),
            lambda st: st['native'].update(id='e' * 64),
            lambda st: st['native'].update(state='CONTAINER_EXITED'),
            lambda st: st['native']['metadata'].update(name='foreign'),
            lambda st: st['native']['metadata'].update(attempt=1),
            lambda st: st['native'].update(imageRef='foreign@' + INDEX),
            lambda st: st['native']['labels'].update({'io.kubernetes.pod.uid': 'foreign'}),
            lambda st: st['native']['labels'].update({'io.kubernetes.pod.namespace': 'foreign'}),
            lambda st: st['native']['labels'].update({'io.kubernetes.pod.name': 'foreign'}),
            lambda st: st['native']['labels'].pop('io.kubernetes.container.name'),
            lambda st: st['pod']['status']['containerStatuses'][0].update(containerID='docker://' + 'd' * 64),
            lambda st: st['pod']['status']['containerStatuses'].append(copy.deepcopy(st['pod']['status']['containerStatuses'][0])),
        ]
        for index, mutate in enumerate(mutations):
            with self.subTest(mutation=index):
                driver, task, state = self.kube_identity_fixture()
                mutate(state)
                with self.assertRaises(b.Refused):
                    driver.native('run', task)
        driver, task, state = self.kube_identity_fixture()
        state['replacement'] = copy.deepcopy(state['pod'])
        state['replacement']['metadata']['uid'] = 'foreign'
        with self.assertRaises(b.Refused):
            driver.native('run', task)

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


@contextmanager
def loopback_http(responses):
    class Handler(socketserver.BaseRequestHandler):
        def handle(self):
            self.request.settimeout(2)
            self.request.recv(65536)
            self.server.requests += 1
            if not self.server.responses:
                raise AssertionError('unexpected loopback request')
            response = self.server.responses.pop(0)
            if response:
                try:
                    self.request.sendall(response)
                except ConnectionError:
                    pass  # The bounded reader may close its own oversized stream.
    server = socketserver.TCPServer(('127.0.0.1', 0), Handler)
    server.responses, server.requests = list(responses), 0
    server.base = 'http://127.0.0.1:' + str(server.server_address[1])
    thread = threading.Thread(target=server.serve_forever, kwargs={'poll_interval': .01}, daemon=True)
    thread.start()
    try:
        opener = b.urllib.request.build_opener(b.urllib.request.ProxyHandler({}))
        with patch.object(b.urllib.request, 'urlopen', side_effect=opener.open):
            yield server
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)
        if thread.is_alive():
            raise AssertionError('owned loopback listener did not stop')


def finite_response(body, length=None, status='200 OK'):
    length = len(body) if length is None else length
    return ('HTTP/1.1 ' + status + '\r\nContent-Length: ' + str(length) + '\r\nConnection: close\r\n\r\n').encode() + body


class BackendHTTPGuards(unittest.TestCase):
    def driver(self, base):
        driver = object.__new__(b.Driver)
        driver.base, driver.server_id = base, 'owned-server-id'
        return driver

    def test_actual_disconnect_and_bad_protocol_become_refused(self):
        for response in (b'', b'not an HTTP response\r\n\r\n'):
            with self.subTest(response=response), loopback_http([response]) as server:
                with self.assertRaises(b.Refused) as raised:
                    self.driver(server.base).http('/health')
                self.assertIsInstance(raised.exception.__cause__, http.client.HTTPException)
                self.assertEqual(server.requests, 1)

    def test_actual_disconnect_is_retried_only_during_owned_readiness(self):
        with loopback_http([b'', finite_response(b'{"healthy":true}')]) as server:
            driver = self.driver(server.base)
            with patch.object(driver, 'owned', return_value={'State': {'Running': True}}, create=True) as owned:
                driver._wait_for_server()
            self.assertEqual(server.requests, 2)
            self.assertEqual(server.responses, [])
            owned.assert_called_with('container', 'owned-server-id')
        # A finite non-readiness request propagates instead of retrying.
        with loopback_http([b'', finite_response(b'{"healthy":true}')]) as server:
            with self.assertRaises(b.Refused): self.driver(server.base).http('/v1/jobs')
            self.assertEqual(server.requests, 1)
            self.assertEqual(len(server.responses), 1)

    def test_readiness_refuses_exited_owner_and_keeps_its_deadline(self):
        with loopback_http([b'', finite_response(b'{"healthy":true}')]) as server:
            driver = self.driver(server.base)
            with patch.object(driver, 'owned', side_effect=[{'State': {'Running': True}}, {'State': {'Running': False}}], create=True):
                with self.assertRaisesRegex(b.Refused, 'exited before health'):
                    driver._wait_for_server()
            self.assertEqual(server.requests, 1)
        driver = self.driver('http://unused-loopback')
        with patch.object(driver, 'owned', return_value={'State': {'Running': True}}, create=True), \
             patch.object(driver, 'http', side_effect=b.Refused('disconnect')), \
             patch.object(b.time, 'monotonic', side_effect=[0, 0, 91]), patch.object(b.time, 'sleep'):
            with self.assertRaisesRegex(b.Refused, 'deadline waiting for actual main readiness'):
                driver._wait_for_server()

    def test_finite_valid_json_prefix_cannot_hide_declared_truncation(self):
        body = b'{"healthy":true}'
        chunked = b'HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n' + format(len(body), 'x').encode() + b'\r\n' + body + b'\r\n'
        ambiguous = ('HTTP/1.1 200 OK\r\nContent-Length: ' + str(len(body)) + '\r\nContent-Length: 86\r\n\r\n').encode() + body
        for response in (finite_response(body, len(body) + 64), chunked, ambiguous):
            with self.subTest(chunked=response is chunked), loopback_http([response]) as server:
                with self.assertRaises(b.Refused): self.driver(server.base).http('/health')
        with loopback_http([finite_response(b'complete text', 86)]) as server:
            with self.assertRaisesRegex(b.Refused, 'body incomplete'):
                self.driver(server.base).http('/logs', text=True)

    def test_finite_cap_encoding_and_status_fail_closed(self):
        maximum = 2 * 1024 * 1024
        responses = [finite_response(b'{}', maximum + 1),
                     b'HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n' + b'x' * (maximum + 1),
                     finite_response(b'not JSON'), finite_response(b'\xff'), finite_response(b'{}', status='503 Unavailable')]
        for response in responses:
            with self.subTest(prefix=response[:65]), loopback_http([response]) as server:
                with self.assertRaises(b.Refused): self.driver(server.base).http('/health')
        with loopback_http([finite_response(b'\xff')]) as server:
            with self.assertRaises(b.Refused): self.driver(server.base).http('/logs', text=True)

    def test_complete_finite_json_and_text_remain_valid(self):
        with loopback_http([finite_response(b'{"healthy":true}'), finite_response('actual text \u2713'.encode())]) as server:
            driver = self.driver(server.base)
            self.assertEqual(driver.http('/health'), {'healthy': True})
            self.assertEqual(driver.http('/logs', text=True), 'actual text \u2713')

    def test_live_log_transport_refuses_but_complete_prefix_stays_partial(self):
        task_id = '11111111-1111-4111-8111-111111111111'
        task = {'id': task_id, 'task_id': task_id, 'partition_count': 0}
        for error in (http.client.RemoteDisconnected('closed'), http.client.IncompleteRead(b'partial'), ConnectionResetError('reset')):
            with patch.object(b.urllib.request, 'urlopen', side_effect=error):
                with self.assertRaises(b.Refused):
                    self.driver('http://unused-loopback').live_deadline_log('job', 'run', task, 'owned')
        # The real streaming path is intentionally prefix-only: no full-body length requirement.
        response = b'HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 100000\r\n\r\nDEADLINE_owned\n'
        with loopback_http([response]) as server:
            self.assertEqual(self.driver(server.base).live_deadline_log('job', 'run', task, 'owned'), 'DEADLINE_owned\n')


if __name__ == '__main__':
    unittest.main()
