#!/usr/bin/env python3
"""Parent-scheduled backend journeys using an existing verified coverage cohort.

Default is an offline input-validation plan. --run alone allocates resources.
No builds, pulls, installs, retags, pruning, counter synthesis or profile merging.
"""
from __future__ import annotations

import argparse
import base64
import errno
import importlib.util
import hashlib
import http.client
import json
import os
from pathlib import Path, PurePosixPath
import re
import secrets
import shutil
import signal
import socket
import ssl
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.request
import uuid

MODULE = 'github.com/caesium-cloud/caesium'
IMAGE_RE = re.compile(r'sha256:[0-9a-f]{64}')
HASH_RE = re.compile(r'[0-9a-f]{64}')
LABEL_OWNER = 'caesium.coverage.owner'
LABEL_RUN = 'caesium.coverage.run'


class Refused(RuntimeError):
    pass


class WaitExpired(Refused):
    pass


class ServerExited(Refused):
    pass


def failure_category(error):
    # Exception text, native stderr and chained HTTP bodies are never evidence.
    for kind, category in ((WaitExpired, 'deadline'), (ServerExited, 'native-exit'),
                           (subprocess.TimeoutExpired, 'command-deadline'),
                           (Refused, 'guard-refusal'), (OSError, 'io-error'),
                           (ValueError, 'encoding-error'), (KeyboardInterrupt, 'interrupted'),
                           (SystemExit, 'interrupted')):
        if isinstance(error, kind):
            return category
    return 'unexpected-error'


TRANSPORT_CATEGORIES = frozenset({'refused', 'timeout', 'reset', 'dns', 'tls', 'other', 'unavailable'})


# Post-verdict observations only. Four read-only commands share6s, at most3s each,
# reuse the joined/byte-capped observer; they never decide profile eligibility.
STARTUP_MESSAGES = {
    'migrating database': 'migrating-database',
    'establishing db connection': 'establishing-db-connection',
    'database router initialized': 'database-router-initialized',
    'execution configuration': 'execution-configuration',
    'distributed worker disabled': 'distributed-worker-disabled',
    'spinning up api': 'spinning-up-api',
    'api listener started': 'api-listener-started',
}


def observation_capture(argv, env, *, logs=False, timeout=3):
    # Load lazily: an absent optional observer must not break collection/import.
    try:
        spec = importlib.util.spec_from_file_location('backend_native_observer',
                                                     Path(__file__).with_name('stress-native-diagnostics.py'))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        # Backend handlers raise. Use a flag during child ownership transfer,
        # exactly as stress main does, and restore every original handler only
        # after the observer group/direct child have been terminated/joined.
        handlers = {}
        try:
            def interrupted(_number, _frame):
                module.INTERRUPTED = True
            for number in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
                handlers[number] = signal.getsignal(number)
                signal.signal(number, interrupted)
            return module.capture(argv, env=env, merge_stderr=logs, timeout=timeout)
        finally:
            for number, handler in handlers.items():
                signal.signal(number, handler)
    except BaseException:
        return None, 'observer-unavailable'


def observation_unavailable(reason):
    allowed = {'command-unavailable', 'command-failed', 'deadline', 'oversized',
               'interrupted', 'observer-unavailable', 'identity-not-proved', 'invalid-body', 'invalid-bound'}
    return {'outcome': 'unavailable', 'reason': reason if reason in allowed else 'observer-unavailable',
            'truncated': reason == 'oversized'}


def observation_json(text):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError('duplicate')
            result[key] = value
        return result
    def invalid_constant(_value):
        raise ValueError('constant')
    return json.loads(text, object_pairs_hook=unique, parse_constant=invalid_constant)


def startup_log_projection(body):
    # Docker --timestamps framing is checked, but merged stream ordering is
    # deliberately not claimed. No original timestamp or arbitrary field leaks.
    lines = body.splitlines()
    value = {'outcome': 'complete', 'scope': 'tail-200', 'absence_is_proof': False,
             'ordering': 'not-claimed', 'lines': len(lines), 'ignored': 0,
             'truncated': False, 'milestones': []}
    if len(body) > 65536 or len(lines) > 200:
        return observation_unavailable('oversized')
    seen = set()
    for line in lines:
        if len(line) > 4096:
            value['ignored'] += 1
            value['truncated'] = True
            continue
        try:
            stamp, record = line.decode('utf-8').split(' ', 1)
            # Fixed Docker UTC timestamp shape, not an arbitrary URL/string.
            require(re.fullmatch(r'[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?Z', stamp), 'timestamp')
            record = observation_json(record)
            msg = record.get('msg') if type(record) is dict else None
            milestone = STARTUP_MESSAGES.get(msg) if type(msg) is str else None
            if milestone is None:
                value['ignored'] += 1
            else:
                seen.add(milestone)
        except (UnicodeError, ValueError, Refused, RecursionError):
            value['ignored'] += 1
    value['milestones'] = sorted(seen)
    return value


def startup_health_projection(body):
    if len(body) > 16384:
        return observation_unavailable('oversized')
    try:
        value = observation_json(body.decode('utf-8'))
        status = value.get('status') if type(value) is dict else None
        if type(status) is not str or status not in {'healthy', 'degraded', 'unavailable', 'unknown'}:
            raise ValueError('status')
        return {'outcome': 'healthy' if status == 'healthy' else 'unhealthy',
                'read': 'complete', 'qualification': 'unchanged'}
    except (UnicodeError, ValueError, RecursionError):
        return observation_unavailable('invalid-body')


def transport_category(error):
    # Exact stdlib types only: subclass attributes/strings are not evidence.
    seen = set()
    for _ in range(4):
        if id(error) in seen:
            return 'unavailable'
        seen.add(id(error))
        kind = type(error)
        if kind is urllib.error.URLError:
            values = object.__getattribute__(error, '__dict__')
            if type(values) is not dict or len(values) > 16:
                return 'unavailable'
            error = next((value for key, value in values.items()
                          if type(key) is str and key == 'reason'), None)
            continue
        if kind is TimeoutError:
            return 'timeout'
        if kind is ConnectionRefusedError:
            return 'refused'
        if any(kind is candidate for candidate in (ConnectionResetError, ConnectionAbortedError, BrokenPipeError)):
            return 'reset'
        if kind is socket.gaierror:
            return 'dns'
        if any(kind is candidate for candidate in (ssl.SSLError, ssl.SSLCertVerificationError, ssl.SSLEOFError,
                                                  ssl.SSLZeroReturnError, ssl.SSLWantReadError, ssl.SSLWantWriteError)):
            return 'tls'
        if kind is OSError:
            code = OSError.errno.__get__(error)
            if type(code) is not int or not 0 <= code <= 2147483647:
                return 'unavailable'
            return {errno.ECONNREFUSED: 'refused', errno.ETIMEDOUT: 'timeout',
                    errno.ECONNRESET: 'reset', errno.ECONNABORTED: 'reset', errno.EPIPE: 'reset'}.get(code, 'other')
        if any(kind is candidate for candidate in (http.client.HTTPException, http.client.RemoteDisconnected,
                                                  http.client.IncompleteRead, http.client.BadStatusLine,
                                                  http.client.LineTooLong, http.client.UnknownProtocol,
                                                  http.client.UnknownTransferEncoding, http.client.NotConnected,
                                                  http.client.InvalidURL, http.client.CannotSendRequest,
                                                  http.client.CannotSendHeader, http.client.ResponseNotReady)):
            return 'other'
        return 'unavailable'
    return 'unavailable'


def bounded_transport_counts(value):
    if type(value) is not dict or len(value) > len(TRANSPORT_CATEGORIES):
        return {}
    return {key: count for key, count in value.items()
            if type(key) is str and key in TRANSPORT_CATEGORIES
            and type(count) is int and 0 <= count <= 1000000}


def require(condition, message):
    if not condition:
        raise Refused(message)


def digest(path):
    with Path(path).open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def regular(path):
    path = Path(path)
    require(not path.is_symlink() and path.is_file(), 'required regular input file unavailable')
    return path.resolve()


def read_pinned(path, expected):
    path = regular(path)
    require(HASH_RE.fullmatch(expected or '') and digest(path) == expected, 'pinned input digest mismatch')
    value = json.loads(path.read_text())
    require(isinstance(value, dict), 'input must be a JSON object')
    return value


def validate_context(context):
    require(context.get('schema_version') == 1 and context.get('producer') == 'scripts/integration-coverage.sh', 'foreign producer schema')
    require(re.fullmatch(r'[a-f0-9]{40}', context.get('candidate_sha', '')), 'full candidate SHA required')
    for key in ('image_id', 'builder_image_id'):
        require(IMAGE_RE.fullmatch(context.get(key, '')), 'immutable producer image IDs required')
    require(context.get('verified') is True and context.get('image_provenance') == 'built-by-this-run', 'verified original producer required; supplied/rebuilt images refuse')
    require(re.fullmatch(r'[a-zA-Z0-9](?:[a-zA-Z0-9_.-]{0,61}[a-zA-Z0-9])?', context.get('coverage_id', '')), 'invalid producer coverage ID')
    build = context.get('build_context', {})
    require(build.get('goos') == 'linux' and build.get('goarch') in ('arm64', 'amd64') and build.get('cgo_enabled') is True and build.get('build_tags') == [] and build.get('compiler') == 'gc', 'original Linux amd64/arm64 CGO build context required')
    require(context.get('platform') == 'linux/' + build['goarch'], 'candidate platform mismatch')
    inventory_ref = context.get('source_inventory', {})
    inventory_path = Path(inventory_ref.get('path', ''))
    if not inventory_path.is_absolute():
        artifact_dir = Path(context.get('artifact_dir', ''))
        require(artifact_dir.is_absolute() and '..' not in inventory_path.parts and str(inventory_path) != '.', 'relative source inventory requires an explicit artifact directory')
        inventory_path = artifact_dir / inventory_path
    inventory = read_pinned(inventory_path, inventory_ref.get('sha256', ''))
    require(inventory.get('schema_version') == 1 and inventory.get('kind') == 'go-ast-source-inventory' and inventory.get('parser') == 'go/parser' and inventory.get('complete') is True, 'complete AST producer inventory required')
    require(inventory.get('candidate_sha') == context['candidate_sha'] and inventory.get('image_id') == context['image_id'] and inventory.get('build_context') == build and inventory.get('files') and inventory.get('packages'), 'producer inventory identity/context mismatch')
    require(context.get('container_cli') == 'docker', 'Docker producer context required')
    require(Path(context.get('repository_root', '')).is_absolute(), 'producer repository root required')
    if 'binary_sha256' in context:
        require(HASH_RE.fullmatch(context['binary_sha256']), 'invalid optional producer binary hash')
    return context


def fresh_directory(path):
    path = Path(path).absolute()
    require(not os.path.lexists(path) and not any(parent.is_symlink() for parent in path.parents), 'fresh non-symlink output required')
    path.mkdir(parents=True)
    return path


def normalized_image_ref(reference):
    first = reference.split('/')[0]
    if '/' not in reference:
        return 'docker.io/library/' + reference
    if '.' not in first and ':' not in first and first != 'localhost':
        return 'docker.io/' + reference
    return reference


def backend_receipts(inputs, backend):
    receipt = {key: inputs[key] for key in ('platform', 'task_archive_sha256', 'task_image_id', 'task_image_ref')}
    if inputs.get('task_docker_image_id'):
        receipt['task_docker_image_id'] = inputs['task_docker_image_id']
    receipt['kind_image_id' if backend == 'kubernetes' else 'podman_service_image_id'] = inputs['kind_image_id' if backend == 'kubernetes' else 'podman_service_image_id']
    return receipt


def contribution(context, reports, selected):
    complete = selected == ['kubernetes', 'podman'] and len(reports) == 2 and all(report.get('complete') is True and not report.get('failed') and not report.get('cleanup_errors') for report in reports)
    lanes = []
    if complete:
        for report in reports:
            processes = report['processes']
            require(len(processes) == 7 and len([p for p in processes if p['source'] == 'server']) == 1 and len([p for p in processes if p['source'] == 'cli']) == 6 and all(p.get('complete') is True and not p.get('missing') and not p.get('killed') for p in processes), 'complete backend process inventory required')
            for record in report['processes']:
                lanes.append({key: record[key] for key in ('backend', 'lane', 'source', 'raw_dir', 'provenance_path')})
    require(len({(p['backend'], p['lane']) for p in lanes}) == len(lanes) and len({p['raw_dir'] for p in lanes}) == len(lanes), 'duplicate contributor lane/raw directory')
    result = {key: context[key] for key in ('candidate_sha', 'image_id', 'builder_image_id', 'build_context', 'image_provenance', 'verified')}
    result.update(schema_version=1, kind='real-backend-coverage', complete=complete, lanes=lanes)
    return result


def validate_oci_closure(archive, members, task_config_id, architecture):
    names = {member.name: member for member in members}
    if not ({'index.json', 'oci-layout'} & names.keys()):
        return  # Legacy Docker-save manifest format, not an OCI layout.
    require({'index.json', 'oci-layout'} <= names.keys(), 'incomplete OCI layout metadata')
    def metadata(name):
        member = names.get(name)
        require(member is not None and member.isfile() and member.size <= 2 * 1024 * 1024, 'missing/oversized OCI metadata')
        stream = archive.extractfile(member)
        require(stream is not None, 'OCI metadata unavailable')
        return json.loads(stream.read())
    require(metadata('oci-layout').get('imageLayoutVersion') == '1.0.0', 'unsupported OCI layout')
    root = metadata('index.json')
    require(root.get('schemaVersion') == 2 and isinstance(root.get('manifests'), list) and root['manifests'], 'invalid OCI root index')
    index_types = {'application/vnd.oci.image.index.v1+json', 'application/vnd.docker.distribution.manifest.list.v2+json'}
    manifest_types = {'application/vnd.oci.image.manifest.v1+json', 'application/vnd.docker.distribution.manifest.v2+json'}
    pending = [(descriptor, 0) for descriptor in root['manifests']]
    seen, matched_task = {}, False
    while pending:
        descriptor, depth = pending.pop()
        require(isinstance(descriptor, dict) and depth <= 32 and len(seen) < 10000, 'invalid/excessive OCI descriptor tree')
        identity, size, media = descriptor.get('digest', ''), descriptor.get('size'), descriptor.get('mediaType', '')
        require(IMAGE_RE.fullmatch(identity) and type(size) is int and size >= 0 and isinstance(media, str) and media, 'invalid OCI descriptor identity')
        if identity in seen:
            require(seen[identity] == (size, media), 'conflicting OCI descriptor identity')
            continue
        name = 'blobs/sha256/' + identity.removeprefix('sha256:')
        member = names.get(name)
        require(member is not None and member.isfile() and member.size == size, 'OCI descriptor closure incomplete')
        stream = archive.extractfile(member)
        require(stream is not None and 'sha256:' + hashlib.file_digest(stream, 'sha256').hexdigest() == identity, 'OCI descriptor content mismatch')
        seen[identity] = (size, media)
        if media in index_types:
            index = metadata(name)
            require(index.get('schemaVersion') == 2 and isinstance(index.get('manifests'), list), 'invalid OCI child index')
            pending.extend((child, depth + 1) for child in index['manifests'])
        elif media in manifest_types:
            manifest = metadata(name)
            require(manifest.get('schemaVersion') == 2 and isinstance(manifest.get('config'), dict) and isinstance(manifest.get('layers'), list), 'invalid OCI image manifest')
            pending.extend((child, depth + 1) for child in [manifest['config'], *manifest['layers']])
            platform = descriptor.get('platform', {})
            if manifest['config'].get('digest') == task_config_id and (not platform or platform.get('os') == 'linux' and platform.get('architecture') == architecture):
                matched_task = True
    require(matched_task, 'OCI index does not bind the selected task config/platform')


def archive_identity(path, expected_hash, image_id, image_ref, architecture):
    path = regular(path)
    require(digest(path) == expected_hash and HASH_RE.fullmatch(expected_hash or ''), 'task archive digest mismatch')
    require(IMAGE_RE.fullmatch(image_id or '') and re.fullmatch(r'[a-z0-9][a-z0-9./:_-]+', image_ref or '') and ':' in image_ref and not image_ref.endswith(':latest'), 'pinned task image identity/reference required')
    with tarfile.open(path) as archive:
        members = archive.getmembers()
        require(len({m.name for m in members}) == len(members), 'duplicate image archive entry')
        require(all(not m.issym() and not m.islnk() and not PurePosixPath(m.name).is_absolute() and '..' not in PurePosixPath(m.name).parts for m in members), 'unsafe image archive entry')
        manifest_stream = archive.extractfile('manifest.json')
        require(manifest_stream is not None, 'Docker task archive manifest required')
        manifest = json.load(manifest_stream)
        require(isinstance(manifest, list) and len(manifest) == 1 and manifest[0].get('RepoTags') == [image_ref], 'archive must contain only the exact task image reference')
        config_path = manifest[0].get('Config', '')
        require(config_path and config_path in {m.name for m in members}, 'archive config reference unavailable')
        config_stream = archive.extractfile(config_path)
        require(config_stream is not None, 'task archive configuration unavailable')
        config_bytes = config_stream.read()
        require('sha256:' + hashlib.sha256(config_bytes).hexdigest() == image_id, 'task archive config/image mismatch')
        config = json.loads(config_bytes)
        require(config.get('os') == 'linux' and config.get('architecture') == architecture, 'task archive must match requested Linux architecture')
        validate_oci_closure(archive, members, image_id, architecture)
    return path


def missing_object(result, kind, name):
    if result.returncode == 0 or result.stdout.strip() not in ('', '[]'):
        return False
    escaped = re.escape(name)
    patterns = {'volume': [rf'Error response from daemon: get {escaped}: no such volume'],
                'network': [rf'Error response from daemon: network {escaped} not found'],
                'container': [rf'Error response from daemon: No such container: {escaped}', rf'Error: No such container: {escaped}']}
    return any(re.fullmatch(p, result.stderr.strip()) for p in patterns.get(kind, []))


def validate_kubeconfig(config, cluster_name, control_name):
    require(config.get('kind') == 'Config', 'invalid kubeconfig kind')
    require(len(config.get('clusters', [])) == len(config.get('users', [])) == len(config.get('contexts', [])) == 1, 'kubeconfig must bind only the owned cluster')
    cluster = config['clusters'][0]
    user = config['users'][0]
    context = config['contexts'][0]
    require(cluster['name'] == 'kind-' + cluster_name and user['name'] == 'kind-' + cluster_name and context['name'] == config.get('current-context') == 'kind-' + cluster_name, 'foreign kubeconfig context')
    require(context['context'].get('cluster') == cluster['name'] and context['context'].get('user') == user['name'], 'kubeconfig binding mismatch')
    settings = cluster['cluster']
    require(settings.get('server') == 'https://' + control_name + ':6443' and not settings.get('insecure-skip-tls-verify'), 'owned internal TLS endpoint required')
    require(set(settings) <= {'server', 'certificate-authority-data'} and 'certificate-authority-data' in settings, 'embedded owned CA only')
    credentials = user['user']
    require(set(credentials) == {'client-certificate-data', 'client-key-data'}, 'embedded private credentials only; exec/token/auth-provider refuse')
    for value in [settings['certificate-authority-data'], *credentials.values()]:
        try:
            decoded = base64.b64decode(value, validate=True)
        except (ValueError, TypeError) as exc:
            raise Refused('invalid embedded TLS credential') from exc
        require(bool(decoded), 'empty TLS credential')
    return config


def coverage_files(path):
    path = Path(path)
    require(path.is_dir() and not path.is_symlink(), 'raw coverage directory unavailable')
    files = list(path.iterdir())
    require(files and all(p.stat().st_size > 0 and p.is_file() and not p.is_symlink() and p.name.startswith(('covmeta.', 'covcounters.')) for p in files), 'foreign/missing raw coverage files')
    require(any(p.name.startswith('covmeta.') for p in files) and any(p.name.startswith('covcounters.') for p in files), 'raw profile needs meta and counters')
    return {p.name: digest(p) for p in sorted(files)}


def unfanned_log_path(job_id, run_id, task):
    # Public run detail rewrites .id to the catalog task ID even for one row.
    # This fixture has no fan-out; the handler resolves its unique concrete
    # instance authoritatively. Never pass the collapsed .id as task_run_id.
    require(task.get('partition_count', 0) == 0 and not task.get('partition_value'), 'backend fixture unexpectedly fanned; exact log instance unavailable')
    task_id = str(uuid.UUID(task['task_id']))
    return '/v1/jobs/' + job_id + '/runs/' + run_id + '/logs?task_id=' + task_id


def verify_outcome(run, case, marker):
    tasks = run.get('tasks', [])
    require(tasks and all(t.get('runtime_id') and t.get('completed_at') and not t.get('cache_hit') for t in tasks), 'real uncached backend tasks required')
    if case == 'success':
        require(run.get('status') == 'succeeded' and len(tasks) == 2 and all(t.get('status') == 'succeeded' and t.get('exit_code') == 0 for t in tasks), 'success task/exit outcome mismatch')
        require(any(t.get('output', {}).get('marker') == marker for t in tasks), 'structured producer output was not persisted')
    elif case == 'failure':
        require(run.get('status') == 'failed' and len(tasks) == 1 and tasks[0].get('status') == 'failed' and tasks[0].get('exit_code') == 17, 'deliberate exit17 was not observed; connection/pull failure is insufficient')
    else:
        require(run.get('status') == 'failed' and len(tasks) == 1 and tasks[0].get('status') == 'failed' and tasks[0].get('completed_at') and run.get('error') == 'run timed out after 15s', 'run/task terminal deadline cause missing')
    require(run.get('completed_at'), 'durable terminal completion missing')


class Driver:
    def __init__(self, context, inputs, backend, output):
        self.context, self.inputs, self.backend = context, inputs, backend
        self.ownership_id = context.get('candidate_sha', 'prereq-' + inputs['_inputs_sha256'][:40])
        self.owner = 'cb-' + self.ownership_id[:8] + '-' + secrets.token_hex(5) + '-' + ('kube' if backend == 'kubernetes' else 'podman')
        self.output = fresh_directory(output)
        self.resources, self.processes, self.witnesses = [], [], {}
        self.env = os.environ.copy()
        for key in ('DOCKER_CONTEXT', 'DOCKER_TLS_VERIFY', 'DOCKER_CERT_PATH', 'DOCKER_API_VERSION'):
            self.env.pop(key, None)
        self.env['KIND_EXPERIMENTAL_PROVIDER'] = 'docker'
        self.env['DOCKER_HOST'] = 'unix://' + inputs['docker_socket']
        require(Path(inputs['docker_socket']).is_socket(), 'supplied Docker socket unavailable; no daemon recovery attempted')
        self.private = Path(tempfile.mkdtemp(prefix=self.owner + '-'))
        self.private.chmod(0o700)
        self.image = context.get('image_id', '')
        self.architecture = inputs['platform'].split('/')[1]
        self.task_image = inputs['task_image_id']
        self.task_ref = inputs['task_image_ref']
        self.network = self.owner + '-net'
        self.server_id = ''
        self.stage = 'initialization'
        self.last_health = {'attempts': 0, 'outcome': 'not-attempted', 'status': None, 'read': 'not-attempted', 'parse': 'not-attempted'}
        self.nodes = {}
        self.service_id = ''
        self.report = {'schema_version': 1, 'source': 'integration-journey', 'lane': 'backend-' + backend,
                       'kind': 'backend-gocoverdir-contributors', 'module': MODULE, 'candidate_sha': context.get('candidate_sha'),
                       'image_id': self.image, 'builder_image_id': context.get('builder_image_id'), 'build_context': context.get('build_context'), 'backend': backend,
                       'image_provenance': context.get('image_provenance'), 'verified': context.get('verified', False), 'complete': False,
                       'missing': True, 'killed': False, 'owner': self.owner, 'processes': self.processes, 'cases': []}

    def command(self, argv, check=True, timeout=120):
        process = subprocess.Popen(argv, env=self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, start_new_session=True)
        try:
            stdout, stderr = process.communicate(timeout=timeout)
        except BaseException:
            self.uncertain_command = True
            # Join the command's whole process group before resource cleanup.
            # kind/docker children must not keep allocating after interruption.
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                process.communicate(timeout=8)
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                process.communicate(timeout=8)
            raise
        result = subprocess.CompletedProcess(argv, process.returncode, stdout, stderr)
        require(not check or result.returncode == 0, 'backend command failed: ' + argv[0] + ' (diagnostics withheld)')
        return result

    def docker(self, *args, **kwargs):
        return self.command(['docker', *args], **kwargs)

    def inspect(self, kind, name):
        return json.loads(self.docker(kind, 'inspect', name).stdout)[0]

    def labels(self):
        return ['--label', LABEL_OWNER + '=' + self.ownership_id, '--label', LABEL_RUN + '=' + self.context['coverage_id'], '--label', 'caesium.coverage.backend-owner=' + self.owner]

    def register(self, kind, name):
        result = self.docker(kind, 'inspect', name, check=False)
        require(missing_object(result, kind, name), 'could not prove unused backend resource name')
        self.resources.append((kind, name))
        self.save('allocated-resources.json', {'owner': self.owner, 'resources': self.resources})

    def owned(self, kind, name):
        obj = self.inspect(kind, name)
        labels = obj.get('Config', {}).get('Labels', {}) if kind == 'container' else obj.get('Labels', {})
        require(labels.get(LABEL_OWNER) == self.ownership_id and labels.get(LABEL_RUN) == self.context['coverage_id'] and labels.get('caesium.coverage.backend-owner') == self.owner, 'foreign backend resource; mutation refused')
        return obj

    def image_check(self, image_id):
        obj = self.inspect('image', image_id)
        require(obj['Id'] == image_id and obj['Os'] == 'linux' and obj['Architecture'] == self.architecture, 'loaded immutable image must match requested Linux architecture')
        return obj

    def verify_task_docker_receipt(self):
        if self.inputs.get('task_docker_image_id'):
            obj = self.image_check(self.inputs['task_docker_image_id'])
            require(self.task_ref in (obj.get('RepoTags') or []), 'loaded Docker task index/reference differs from pinned prerequisite')

    def save(self, name, value):
        (self.output / name).write_text(json.dumps(value, indent=2) + '\n')

    def failure_diagnostic(self, error):
        stages = {'initialization', 'image-validation', 'network-allocation', 'provisioning',
                  'server-allocation', 'server-inspection', 'server-binary', 'server-network',
                  'server-binding', 'server-health', 'server-flush', 'process-validation', 'cleanup'}
        stages.update('scenario-' + case + '-' + step for case in ('success', 'failure', 'deadline')
                      for step in ('apply', 'start', 'observe', 'logs'))
        phase = self.stage if isinstance(self.stage, str) and self.stage in stages else 'unknown-stage'
        source = self.last_health if isinstance(self.last_health, dict) else {}
        health = {}
        for key, allowed in {'outcome': {'not-attempted', 'pending', 'status-check', 'transport-error', 'read-error', 'http-error', 'guard-refused', 'parse-error', 'complete'},
                             'read': {'not-attempted', 'length-headers', 'reading-body', 'length-mismatch', 'received', 'oversized', 'complete'},
                             'parse': {'not-attempted', 'invalid', 'valid'}}.items():
            health[key] = source.get(key) if isinstance(source.get(key), str) and source.get(key) in allowed else 'unavailable'
        attempts, status = source.get('attempts'), source.get('status')
        health['attempts'] = attempts if type(attempts) is int and 0 <= attempts <= 1000000 else None
        health['status'] = status if type(status) is int and 100 <= status <= 599 else None
        category = source.get('transport_last')
        health['transport_last'] = category if type(category) is str and category in TRANSPORT_CATEGORIES else 'unavailable'
        health['transport_counts'] = bounded_transport_counts(source.get('transport_counts'))
        value = {'schema_version': 1, 'phase': phase, 'exception_category': failure_category(error),
                 'health': health, 'native': {'outcome': 'not-acknowledged'}}
        snapshot_stage, inspect_status = 'identity', None
        # The command timeout helper marks ambiguous mutation outcomes. This
        # extra read must not add such a marker or alter the original cleanup.
        had_uncertain = hasattr(self, 'uncertain_command')
        uncertain = getattr(self, 'uncertain_command', False)
        try:
            identity = self.server_id
            if identity:
                require(HASH_RE.fullmatch(identity), 'invalid diagnostic identity')
                snapshot_stage = 'inspect'
                result = self.docker('container', 'inspect', identity, check=False, timeout=10)
                inspect_status = result.returncode if type(result.returncode) is int and -255 <= result.returncode <= 255 else None
                require(result.returncode == 0 and len(result.stdout) <= 1024 * 1024, 'diagnostic inspect unavailable')
                snapshot_stage = 'decode'
                items = json.loads(result.stdout)
                require(isinstance(items, list) and len(items) == 1 and isinstance(items[0], dict), 'diagnostic inspect malformed')
                obj = items[0]
                labels = obj.get('Config', {}).get('Labels', {})
                snapshot_stage = 'ownership'
                require(obj.get('Id') == identity and obj.get('Name') == '/' + self.owner + '-server'
                        and obj.get('Image') == self.image and IMAGE_RE.fullmatch(self.image)
                        and labels.get(LABEL_OWNER) == self.ownership_id
                        and labels.get(LABEL_RUN) == self.context['coverage_id']
                        and labels.get('caesium.coverage.backend-owner') == self.owner, 'diagnostic ownership changed')
                snapshot_stage = 'state'
                state = obj.get('State', {})
                require(all(type(state.get(k)) is bool for k in ('Running', 'OOMKilled', 'Dead'))
                        and type(state.get('ExitCode')) is int and 0 <= state['ExitCode'] <= 255
                        and type(obj.get('RestartCount')) is int and 0 <= obj['RestartCount'] <= 1000000, 'diagnostic state malformed')
                reduced = {k: state[k] for k in ('Running', 'OOMKilled', 'Dead', 'ExitCode')}
                for key in ('StartedAt', 'FinishedAt'):
                    stamp = state.get(key)
                    require(isinstance(stamp, str) and re.fullmatch(r'[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?Z', stamp), 'diagnostic timestamp malformed')
                    reduced[key] = stamp
                snapshot_stage = 'port'
                ports = obj.get('NetworkSettings', {}).get('Ports', {}).get('8080/tcp')
                port = {'outcome': 'unavailable'}
                if ports:
                    require(isinstance(ports, list) and len(ports) == 1 and ports[0].get('HostIp') == '127.0.0.1'
                            and re.fullmatch(r'[0-9]{1,5}', ports[0].get('HostPort', ''))
                            and 0 < int(ports[0]['HostPort']) <= 65535, 'diagnostic port malformed')
                    port = {'outcome': 'verified-loopback', 'host_ip': '127.0.0.1', 'host_port': int(ports[0]['HostPort'])}
                value['native'] = {'outcome': 'verified-owned', 'id': identity, 'image_id': self.image,
                                   'state': reduced, 'restart_count': obj['RestartCount'], 'port': port}
        except BaseException as snapshot_error:
            value['native'] = {'outcome': 'unavailable', 'category': failure_category(snapshot_error),
                               'stage': snapshot_stage, 'inspect_status': inspect_status}
        finally:
            if had_uncertain:
                self.uncertain_command = uncertain
            elif hasattr(self, 'uncertain_command'):
                del self.uncertain_command
        if phase == 'server-health' and isinstance(error, WaitExpired):
            try:
                value['startup_observations'] = self.startup_observations(value['native'])
            except BaseException:
                value['startup_observations'] = observation_unavailable('observer-unavailable')
        self.report['failure_diagnostic'] = value
        try:
            self.save('failure-diagnostic.json', value)
            value['retention'] = 'written'
        except BaseException:
            value['retention'] = 'write-failed'
        return value

    def startup_observations(self, native):
        value = {'query_budget_seconds': 6, 'qualification': 'unchanged',
                 'logs': observation_unavailable('identity-not-proved'),
                 'internal_health': observation_unavailable('identity-not-proved')}
        if native.get('outcome') != 'verified-owned':
            return value
        identity = self.server_id
        if not (type(identity) is str and HASH_RE.fullmatch(identity)
                and type(self.image) is str and IMAGE_RE.fullmatch(self.image)
                and native.get('id') == identity and native.get('image_id') == self.image):
            return value
        deadline = time.monotonic() + 6
        def capture(argv, *, logs=False):
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return None, 'deadline'
            return observation_capture(argv, self.env, logs=logs, timeout=min(3, remaining))
        expected = '|'.join([identity, self.image, '/' + self.owner + '-server',
                             self.ownership_id, self.context['coverage_id'], self.owner])
        format_ = '{{.Id}}|{{.Image}}|{{.Name}}|' + '|'.join(
            '{{index .Config.Labels "' + key + '"}}'
            for key in (LABEL_OWNER, LABEL_RUN, 'caesium.coverage.backend-owner'))
        def bound():
            body, failure = capture(['docker', 'container', 'inspect', '--format', format_, identity])
            return failure is None and body == (expected + '\n').encode('utf-8')
        # Revalidate before EACH query. Reads use the pinned Docker environment;
        # they bypass command() so ambiguous reads never mutate cleanup policy.
        for name, command, project in (
            ('logs', ['docker', 'logs', '--timestamps', '--tail', '200', identity], startup_log_projection),
            ('internal_health', ['docker', 'exec', identity, '/bin/busybox', 'timeout', '-s', 'KILL', '2',
                                 '/usr/bin/wget', '--no-proxy', '--max-redirect=0',
                                 '--timeout=1', '--tries=1', '-q', '-O', '-',
                                 'http://127.0.0.1:8080/health'], startup_health_projection)):
            try:
                if not bound():
                    continue
                body, failure = capture(command, logs=name == 'logs')
                value[name] = observation_unavailable(failure) if failure else project(body)
            except BaseException:
                value[name] = observation_unavailable('observer-unavailable')
        return value

    def wait(self, fn, label, timeout=90):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            value = fn()
            if value:
                return value
            time.sleep(.2)
        raise WaitExpired('deadline waiting for ' + label)

    def http(self, path, payload=None, text=False):
        health = path == '/health' and getattr(self, 'stage', '') == 'server-health'
        if health:
            self.last_health.update(attempts=self.last_health['attempts'] + 1, outcome='pending', status=None, read='not-attempted', parse='not-attempted')
        req = urllib.request.Request(self.base + path, None if payload is None else json.dumps(payload).encode(), headers={'Content-Type': 'application/json'})
        try:
            with urllib.request.urlopen(req, timeout=12) as response:
                if health:
                    self.last_health.update(outcome='status-check', status=response.status if type(response.status) is int and 100 <= response.status <= 599 else None, read='length-headers')
                require(response.status in (200, 202), 'public HTTP status mismatch')
                maximum = 2 * 1024 * 1024
                lengths = response.headers.get_all('Content-Length', [])
                require(len(lengths) <= 1, 'ambiguous public response length')
                declared = lengths[0] if lengths else None
                if declared is not None:
                    require(re.fullmatch(r'[0-9]+', declared.strip()) and len(declared.strip()) <= 10, 'invalid public response length')
                    declared = int(declared)
                    require(declared <= maximum, 'public response too large')
                if health:
                    self.last_health['read'] = 'reading-body'
                body = response.read(maximum + 1)
                if health:
                    self.last_health['read'] = 'length-mismatch' if declared is not None and len(body) != declared else 'received'
                require(declared is None or len(body) == declared, 'public response body incomplete')
        except (urllib.error.URLError, OSError, http.client.HTTPException) as exc:
            if health:
                self.last_health['outcome'] = 'transport-error' if self.last_health['status'] is None else 'read-error'
            if health and not isinstance(exc, urllib.error.HTTPError):
                category = transport_category(exc)
                counts = bounded_transport_counts(self.last_health.get('transport_counts'))
                counts[category] = min(counts.get(category, 0) + 1, 1000000)
                self.last_health.update(transport_last=category, transport_counts=counts)
            if isinstance(exc, urllib.error.HTTPError):
                if health:
                    self.last_health.update(outcome='http-error', status=exc.code if type(exc.code) is int and 100 <= exc.code <= 599 else None)
                try:
                    exc.close()
                except (OSError, http.client.HTTPException) as close_error:
                    raise Refused('public HTTP error response close failed') from close_error
            raise Refused('public backend HTTP request failed') from exc
        except Refused:
            if health:
                self.last_health['outcome'] = 'guard-refused'
            raise
        if health:
            self.last_health['read'] = 'oversized' if len(body) > maximum else 'complete'
        require(len(body) <= maximum, 'public response too large')
        try:
            value = body.decode() if text else json.loads(body)
        except (UnicodeError, ValueError) as exc:
            if health:
                self.last_health.update(outcome='parse-error', parse='invalid')
            raise Refused('public response encoding invalid') from exc
        if health:
            self.last_health.update(outcome='complete', parse='valid')
        return value

    def live_deadline_log(self, job_id, run_id, task, marker):
        # Cancellation removes the native object without a persisted raw log
        # snapshot. Read its actual public stream while Running, then close our
        # own HTTP reader; do not wait for the long task/whole stream to finish.
        started = time.monotonic()
        req = urllib.request.Request(self.base + unfanned_log_path(job_id, run_id, task))
        try:
            with urllib.request.urlopen(req, timeout=8) as response:
                require(response.status == 200 and response.headers.get('Content-Type', '').split(';')[0] == 'text/plain', 'live deadline log status/content mismatch')
                line = response.readline(64 * 1024 + 1)
        except (urllib.error.URLError, OSError, http.client.HTTPException) as exc:
            if isinstance(exc, urllib.error.HTTPError):
                try:
                    exc.close()
                except (OSError, http.client.HTTPException) as close_error:
                    raise Refused('live deadline log error response close failed') from close_error
            raise Refused('live deadline log request incomplete') from exc
        require(time.monotonic() - started <= 8 and 0 < len(line) <= 64 * 1024 and line.endswith(b'\n'), 'live deadline log prefix incomplete/oversized/late')
        try:
            text = line.decode()
        except UnicodeError as exc:
            raise Refused('live deadline log encoding invalid') from exc
        require('DEADLINE_' + marker in text, 'live deadline log marker absent')
        return text

    def volume(self, suffix):
        name = self.owner + '-' + suffix
        self.register('volume', name)
        self.docker('volume', 'create', *self.labels(), name)
        return name

    def podman_args(self):
        return ['--storage-driver=vfs', '--cgroup-manager=cgroupfs', '--events-backend=file', '--root=/var/lib/containers/storage', '--runroot=/run/containers/storage', '--tmpdir=/run/containers/tmp']

    def podman(self, *args, check=True):
        obj = self.owned('container', self.service_id)
        require(obj['State']['Running'], 'owned Podman service unavailable')
        return self.docker('exec', obj['Id'], 'podman', '--remote', '--url=unix:///run/podman/podman.sock', *args, check=check)

    def prepare_podman(self):
        image = self.image_check(self.inputs['podman_service_image_id'])
        self.verify_task_docker_receipt()
        permitted = {'/var/lib/containers', '/home/podman/.local/share/containers', '/run/containers', '/run/podman', '/home/podman'}
        require(set((image.get('Config', {}).get('Volumes') or {})) <= permitted, 'unmapped Podman declared volume would leak')
        volumes = {path: self.volume('podman-' + str(i)) for i, path in enumerate(sorted(permitted))}
        self.socket_volume = volumes['/run/podman']
        name = self.owner + '-service'
        self.register('container', name)
        args = ['run', '-d', '--pull=never', '--name', name, *self.labels(), '--privileged', '--cgroupns=private', '--network', self.network, '-e', 'HOME=/home/podman']
        for path, volume in volumes.items():
            args += ['--mount', 'type=volume,src=' + volume + ',dst=' + path]
        # Configure only the owned run volume; log_driver is a containers.conf
        # setting, not a global Podman flag. No host config or cgroup mount.
        args += ['-e', 'CONTAINERS_CONF_OVERRIDE=/run/containers/containers.conf', '--entrypoint', 'sh', image['Id'], '-c',
                 "printf '%s\\n' '[containers]' 'log_driver = \"k8s-file\"' > \"$CONTAINERS_CONF_OVERRIDE\"; exec podman \"$@\"",
                 'owned-podman-service', *self.podman_args(), 'system', 'service', '--time=0', 'unix:///run/podman/podman.sock']
        self.service_id = self.docker(*args).stdout.strip()
        require(IMAGE_RE.fullmatch(self.inputs['podman_service_image_id']) and self.owned('container', self.service_id)['Image'] == image['Id'], 'Podman service image mismatch')
        def ready():
            result = self.podman('version', '--format', 'json', check=False)
            return json.loads(result.stdout) if result.returncode == 0 else None
        versions = self.wait(ready, 'native Podman API/version', 45)
        info = json.loads(self.podman('info', '--format', 'json').stdout)
        host = info.get('host', {})
        store = info.get('store', {})
        require(host.get('arch') in ({'arm64', 'aarch64'} if self.architecture == 'arm64' else {'amd64', 'x86_64'}) and store.get('graphRoot') == '/var/lib/containers/storage' and store.get('runRoot') == '/run/containers/storage' and store.get('graphDriverName') == 'vfs', 'Podman native roots/architecture/VFS mismatch')
        self.save('podman-prerequisite.json', {'service_image_id': image['Id'], 'versions': versions,
                                              'arch': host.get('arch'), 'conmon': host.get('conmon'), 'runtime': host.get('ociRuntime'),
                                              'store': {k: store.get(k) for k in ('graphRoot', 'runRoot', 'graphDriverName')}})
        service_id = self.owned('container', self.service_id)['Id']
        self.docker('cp', str(self.inputs['task_archive']), service_id + ':/run/containers/task.tar')
        self.podman('load', '-i', '/run/containers/task.tar')
        imported = json.loads(self.podman('image', 'inspect', self.task_image).stdout)[0]
        require('sha256:' + imported['Id'].removeprefix('sha256:') == self.task_image, 'Podman task import changed config identity')
        self.task_ref = self.task_image  # exact local image, no registry fallback
        smoke_name = self.owner + '-smoke'
        result = self.podman('create', '--pull=never', '--name', smoke_name, '--label', LABEL_OWNER + '=' + self.owner,
                             self.task_image, 'sh', '-c', 'echo ' + smoke_name)
        smoke_id = result.stdout.strip()
        require(re.fullmatch('[a-f0-9]{64}', smoke_id), 'native Libpod smoke allocation identity missing')
        self.podman('start', smoke_id)
        require(self.podman('wait', smoke_id).stdout.strip() == '0', 'native Libpod smoke exit failure')
        require(smoke_name in self.podman('logs', smoke_id).stdout, 'native Libpod smoke logs unavailable')
        smoke = json.loads(self.podman('inspect', smoke_id).stdout)[0]
        require(smoke.get('Config', {}).get('Labels', {}).get(LABEL_OWNER) == self.owner and smoke.get('State', {}).get('ExitCode') == 0, 'native Libpod smoke inspect mismatch')
        self.podman('rm', smoke_id)
        require(self.native_absent(smoke_id), 'native Libpod smoke removal unproved')
        # A long native control independently proves service Stop/Remove before
        # the candidate runs its own deadline journey.
        long_id = self.podman('create', '--pull=never', '--label', LABEL_OWNER + '=' + self.owner,
                              self.task_image, 'sleep', '300').stdout.strip()
        self.podman('start', long_id)
        self.podman('stop', '--time', '1', long_id)
        self.podman('rm', long_id)
        require(self.native_absent(long_id), 'native Libpod stop/remove unproved')

    def kubectl(self, *args, check=True):
        return self.command(['kubectl', '--kubeconfig', str(self.admin_config), *args], check=check)

    def capture_nodes(self):
        for role, suffix in [('control-plane', 'control-plane'), ('worker', 'worker')]:
            name = self.cluster + '-' + suffix
            result = self.docker('container', 'inspect', name, check=False)
            if missing_object(result, 'container', name):
                continue
            require(result.returncode == 0, 'kind node inventory unavailable')
            obj = json.loads(result.stdout)[0]
            labels = obj.get('Config', {}).get('Labels', {})
            require(labels.get('io.x-k8s.kind.cluster') == self.cluster and labels.get('io.x-k8s.kind.role') == role and obj['Name'].lstrip('/') == name and obj['Image'] == self.inputs['kind_image_id'], 'foreign kind node mutation refused')
            self.nodes[name] = {'id': obj['Id'], 'role': role, 'image_digests': []}

    def node(self, name):
        node = self.nodes[name]
        obj = self.inspect('container', node['id'])
        require(obj['Id'] == node['id'] and obj['Image'] == self.inputs['kind_image_id'] and obj.get('Config', {}).get('Labels', {}).get('io.x-k8s.kind.cluster') == self.cluster and obj.get('Config', {}).get('Labels', {}).get('io.x-k8s.kind.role') == node['role'], 'kind node ownership changed')
        return obj['Id']

    def cri_image_mapping(self, node_id):
        # The CRI alias may differ from ctr's archive manifest digest. It is
        # accepted only as a relation to the exact imported immutable config,
        # with the same tag/digest mapping returned by both config and tag reads.
        mappings = []
        for reference in (self.task_image, normalized_image_ref(self.task_ref)):
            result = self.docker('exec', node_id, 'crictl', 'inspecti', reference)
            image = json.loads(result.stdout).get('status', {})
            tags, aliases = image.get('repoTags'), image.get('repoDigests')
            require(image.get('id') == self.task_image and tags == [normalized_image_ref(self.task_ref)], 'CRI image config/tag differs from verified archive')
            require(isinstance(aliases, list) and len(aliases) == 1 and isinstance(aliases[0], str) and
                    re.fullmatch(r'[a-z0-9][a-z0-9._:/-]*@sha256:[a-f0-9]{64}', aliases[0]), 'CRI imported image alias is missing or ambiguous')
            mappings.append({'config_id': image['id'], 'repo_tags': tags, 'repo_digests': aliases})
        require(mappings[0] == mappings[1], 'CRI immutable config/tag mapping disagrees')
        return mappings[0]

    def import_mapping(self, name, *, recheck=False):
        node_id = self.node(name)
        listing = self.docker('exec', node_id, 'ctr', '-n', 'k8s.io', 'images', 'list').stdout
        lines = [line.split() for line in listing.splitlines() if line.split() and line.split()[0] in {self.task_ref, normalized_image_ref(self.task_ref)}]
        require(len(lines) == 1 and len(lines[0]) > 2 and IMAGE_RE.fullmatch(lines[0][2]), 'kind task reference not imported exactly once')
        root_digest = lines[0][2]
        manifest = json.loads(self.docker('exec', node_id, 'ctr', '-n', 'k8s.io', 'content', 'get', root_digest).stdout)
        digests = [root_digest]
        if 'manifests' in manifest:
            platforms = [m for m in manifest['manifests'] if m.get('platform', {}).get('os') == 'linux' and m.get('platform', {}).get('architecture') == self.architecture]
            require(len(platforms) == 1, 'ambiguous task architecture manifest')
            digests.append(platforms[0]['digest'])
            manifest = json.loads(self.docker('exec', node_id, 'ctr', '-n', 'k8s.io', 'content', 'get', platforms[0]['digest']).stdout)
        require(manifest.get('config', {}).get('digest') == self.task_image, 'kind imported task config differs from archive')
        digests.append(self.task_image)
        mapping = self.cri_image_mapping(node_id)
        if recheck:
            require(self.nodes[name]['image_digests'] == digests and self.nodes[name].get('cri_image') == mapping, 'imported ctr/CRI task mapping changed after preparation')
        else:
            self.nodes[name]['image_digests'] = digests
            self.nodes[name]['cri_image'] = mapping
        return node_id, mapping

    def prepare_kubernetes(self):
        self.image_check(self.inputs['kind_image_id'])
        self.verify_task_docker_receipt()
        self.cluster = self.owner
        self.namespace = self.owner
        existing = self.command(['kind', 'get', 'clusters']).stdout.splitlines()
        require(self.cluster not in existing, 'existing kind cluster refused')
        for suffix in ('control-plane', 'worker'):
            result = self.docker('container', 'inspect', self.cluster + '-' + suffix, check=False)
            require(missing_object(result, 'container', self.cluster + '-' + suffix), 'existing kind node refused')
        self.admin_config = self.private / 'admin-config'
        config_path = self.private / 'kind.json'
        config_path.write_text(json.dumps({'kind': 'Cluster', 'apiVersion': 'kind.x-k8s.io/v1alpha4', 'nodes': [{'role': 'control-plane'}, {'role': 'worker'}]}))
        self.cluster_attempted = True
        self.command(['kind', 'create', 'cluster', '--name', self.cluster, '--image', self.inputs['kind_image_id'], '--config', str(config_path), '--kubeconfig', str(self.admin_config), '--wait', '180s'], timeout=240)
        self.admin_config.chmod(0o600)
        self.capture_nodes()
        require(len(self.nodes) == 2, 'exactly two owned kind nodes required')
        internal = self.command(['kind', 'get', 'kubeconfig', '--name', self.cluster, '--internal']).stdout
        internal_path = self.private / 'internal-config'
        internal_path.write_text(internal)
        internal_path.chmod(0o600)
        flattened = self.command(['kubectl', '--kubeconfig', str(internal_path), 'config', 'view', '--raw', '--flatten', '-o', 'json']).stdout
        config = validate_kubeconfig(json.loads(flattened), self.cluster, self.cluster + '-control-plane')
        config_dir = self.private / 'kube' / '.kube'
        config_dir.mkdir(parents=True, mode=0o700)
        self.kube_dir = config_dir.parent
        self.kube_dir.chmod(0o700)
        kube_file = config_dir / 'config'
        kube_file.write_text(json.dumps(config))
        kube_file.chmod(0o600)
        # Main runs root only inside its owned container, so 0600 private keys
        # remain readable without chmod/chown of foreign paths.
        namespace_file = self.private / 'namespace.json'
        namespace_file.write_text(json.dumps({'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': self.namespace, 'labels': {LABEL_OWNER: self.ownership_id, LABEL_RUN: self.context['coverage_id'], 'caesium.coverage.backend-owner': self.owner}}}))
        self.kubectl('create', '-f', str(namespace_file))
        self.namespace_uid = json.loads(self.kubectl('get', 'namespace', self.namespace, '-o', 'json').stdout)['metadata']['uid']
        self.command(['kind', 'load', 'image-archive', '--name', self.cluster, str(self.inputs['task_archive'])], timeout=120)
        for name in self.nodes:
            self.import_mapping(name)
        self.save('kubernetes-prerequisite.json', {'cluster': self.cluster, 'namespace': self.namespace, 'namespace_uid': self.namespace_uid,
                                                  'nodes': self.nodes, 'task_image_id': self.task_image, 'task_archive_sha256': self.inputs['task_archive_sha256'],
                                                  'config_tls': True, 'config_credentials': 'private embedded certificate data; not retained'})

    def start_server(self):
        self.stage = 'server-allocation'
        name = self.owner + '-server'
        self.register('container', name)
        raw = self.output / 'raw-server'
        raw.mkdir(mode=0o777)
        raw.chmod(0o777)
        database = self.volume('database')
        env = {'GOCOVERDIR': '/var/lib/caesium/coverage', 'CAESIUM_AUTH_MODE': 'none', 'CAESIUM_MANUAL_TRIGGER_API_KEY': 'integration-test-key',
               'CAESIUM_DATABASE_SHARDS': '1', 'CAESIUM_DATABASE_PATH': '/var/lib/caesium/dqlite', 'CAESIUM_NODE_ADDRESS': '127.0.0.1:9001',
               'CAESIUM_DATABASE_CONSOLE_ENABLED': 'true', 'CAESIUM_EXECUTION_MODE': 'local', 'CAESIUM_WORKER_ENABLED': 'false',
               'CAESIUM_RUN_OWNER_ENABLED': 'false', 'CAESIUM_CACHE_ENABLED': 'false', 'CAESIUM_FRESHNESS_ENABLED': 'false',
               'CAESIUM_SHUTDOWN_GRACE_PERIOD': '45s', 'CAESIUM_LOG_LEVEL': 'debug'}
        args = ['run', '-d', '--pull=never', '--name', name, *self.labels(), '--network', self.network, '--user', '0:0',
                '-p', '127.0.0.1::8080', '--mount', 'type=bind,src=' + str(raw) + ',dst=/var/lib/caesium/coverage',
                '--mount', 'type=volume,src=' + database + ',dst=/var/lib/caesium/dqlite']
        if self.backend == 'kubernetes':
            env.update(CAESIUM_KUBERNETES_CONFIG='/coverage-kube', CAESIUM_KUBERNETES_NAMESPACE=self.namespace)
            args += ['--mount', 'type=bind,src=' + str(self.kube_dir) + ',dst=/coverage-kube,readonly']
        else:
            env['CAESIUM_PODMAN_URI'] = 'unix:///run/podman/podman.sock'
            args += ['--mount', 'type=volume,src=' + self.socket_volume + ',dst=/run/podman,readonly']
        for key, value in env.items():
            args += ['-e', key + '=' + value]
        self.server_id = self.docker(*args, self.image, 'start').stdout.strip()
        self.stage = 'server-inspection'
        obj = self.owned('container', self.server_id)
        require(obj['Image'] == self.image, 'main image escaped producer cohort')
        self.stage = 'server-binary'
        binary_hash = self.docker('exec', obj['Id'], 'sha256sum', '/bin/caesium').stdout.split()[0]
        require(HASH_RE.fullmatch(binary_hash) and ('binary_sha256' not in self.context or binary_hash == self.context['binary_sha256']), 'main binary differs from original producer')
        self.binary_hash = binary_hash
        self.stage = 'server-network'
        if self.backend == 'kubernetes':
            self.docker('network', 'connect', 'kind', obj['Id'])
            obj = self.owned('container', self.server_id)
            require(obj.get('Id') == self.server_id and obj.get('Name') == '/' + name
                    and obj.get('Image') == self.image, 'post-attach backend server identity changed')
        self.stage = 'server-binding'
        ports = obj['NetworkSettings'].get('Ports')
        require(type(ports) is dict, 'backend server port bindings unavailable')
        bindings = ports.get('8080/tcp', [])
        require(type(bindings) is list and len(bindings) == 1 and type(bindings[0]) is dict
                and bindings[0].get('HostIp') == '127.0.0.1', 'unexpected backend server port binding')
        port = bindings[0].get('HostPort')
        require(type(port) is str and re.fullmatch(r'[0-9]{1,5}', port)
                and 1 <= int(port) <= 65535, 'invalid backend server host port')
        self.base = 'http://127.0.0.1:' + port
        self.server_name = name
        self._wait_for_server()
        self.server_raw = raw

    def _wait_for_server(self):
        self.stage = 'server-health'
        if not hasattr(self, 'last_health'):
            self.last_health = {'attempts': 0, 'outcome': 'not-attempted', 'status': None, 'read': 'not-attempted', 'parse': 'not-attempted'}
        def running():
            if not self.owned('container', self.server_id)['State']['Running']:
                raise ServerExited('backend server exited before health')
        def ready():
            running()
            try:
                return self.http('/health')
            except Refused:
                running()
                return None
        self.wait(ready, 'actual main readiness', 90)

    def record_process(self, source, obj, raw, command, stop_rc=None, flush_rc=None):
        state = obj['State']
        require(obj['Image'] == self.image and state.get('ExitCode') == 0 and not state.get('Running') and not state.get('OOMKilled') and not obj.get('RestartCount'), 'candidate process did not exit cleanly')
        require(re.fullmatch(r'[a-f0-9]{64}', obj.get('Id', '')), 'immutable candidate container ID missing')
        index = len(self.processes) + 1
        lane = self.backend + '-' + source + '-' + str(index)
        record = {k: self.context[k] for k in ('candidate_sha', 'image_id', 'builder_image_id', 'build_context', 'image_provenance', 'verified')}
        record.update(schema_version=1, source=source, backend=self.backend, lane=lane, kind='gocoverdir', module=MODULE, complete=False, missing=False, killed=False,
                      source_inventory_sha256=self.context['source_inventory']['sha256'], producer_context_sha256=self.context['_context_sha256'],
                      producer_inputs_sha256=self.context['_inputs_sha256'], binary_sha256=self.binary_hash, backend_receipts=backend_receipts(self.inputs, self.backend),
                      container_id=obj['Id'], exit_code=0, oom_killed=False, command=command,
                      raw_dir=self.backend + '/' + raw.name, provenance_path=self.backend + '/provenance-' + str(index) + '.json', files=coverage_files(raw))
        if source == 'server':
            require(stop_rc == flush_rc == 0, 'candidate main flush/graceful stop failed')
            record.update(stop_rc=stop_rc, flush_rc=flush_rc, signal='SIGTERM', flush='sigusr2')
        else:
            record.update(stop_rc=None, flush_rc=None, signal=None, flush='process-exit')
        self.processes.append(record)
        self.save('processes.json', self.processes)

    def finalize_processes(self):
        require([case['case'] for case in self.report['cases']] == ['success', 'failure', 'deadline'] and all(case['status'] == 'pass' for case in self.report['cases']), 'every actual backend scenario must pass before contributor eligibility')
        require(len([p for p in self.processes if p['source'] == 'server']) == 1 and len([p for p in self.processes if p['source'] == 'cli']) == 6, 'backend contributor process inventory incomplete')
        for record in self.processes:
            record.update(complete=True, test_suite={'run_pattern': 'backend-success|failure|deadline', 'minimum_passes': 3, 'passed_scenarios': 3, 'exit_code': 0})
            self.save(Path(record['provenance_path']).name, record)
        self.save('processes.json', self.processes)

    def cli(self, *args):
        index = len([p for p in self.processes if p['source'] == 'cli']) + 1
        name = self.owner + '-cli-' + str(index)
        self.register('container', name)
        raw = self.output / ('raw-cli-' + str(index))
        raw.mkdir(mode=0o777)
        raw.chmod(0o777)
        mounted = self.output / 'definitions'
        result = self.docker('create', '--pull=never', '--name', name, *self.labels(), '--network', self.network, '--user', '0:0',
                             '--mount', 'type=bind,src=' + str(raw) + ',dst=/var/lib/caesium/coverage',
                             '--mount', 'type=bind,src=' + str(mounted) + ',dst=/definitions,readonly',
                             '-e', 'GOCOVERDIR=/var/lib/caesium/coverage', self.image, *args, '--server', 'http://' + self.server_name + ':8080')
        container_id = result.stdout.strip()
        self.owned('container', container_id)
        stdout = self.docker('start', '-a', container_id, check=False, timeout=90)
        (self.output / ('cli-' + str(index) + '.stdout')).write_text(stdout.stdout)
        (self.output / ('cli-' + str(index) + '.stderr')).write_text(stdout.stderr)
        obj = self.owned('container', container_id)
        require(stdout.returncode == 0, 'public candidate CLI failed')
        self.record_process('cli', obj, raw, list(args))
        return stdout.stdout.strip()

    def native(self, run_id, task):
        runtime = task['runtime_id']
        if self.backend == 'podman':
            obj = json.loads(self.podman('inspect', runtime).stdout)[0]
            env = obj.get('Config', {}).get('Env', [])
            require('COVERAGE_BACKEND_OWNER=' + self.owner in env and run_id in obj.get('Name', '') and task['task_id'] in obj.get('Name', '') and obj.get('Id') == runtime and 'sha256:' + obj.get('Image', '').removeprefix('sha256:') == self.task_image, 'foreign or wrong-image native Libpod object')
            if obj.get('State', {}).get('Status') != 'running':
                return None
            return {'run_id': run_id, 'task_id': task['task_id'], 'runtime_id': runtime, 'native_id': obj['Id'], 'image_id': self.task_image, 'status': 'running', 'engine': 'podman'}
        result = self.kubectl('-n', self.namespace, 'get', 'pod', runtime, '-o', 'json', check=False)
        if result.returncode:
            require('NotFound' in result.stderr and runtime in result.stderr, 'native pod lookup failed')
            return None
        obj = json.loads(result.stdout)
        metadata, spec, status = obj['metadata'], obj['spec'], obj.get('status', {})
        require(metadata.get('namespace') == self.namespace and run_id in metadata['name'] and task['task_id'] in metadata['name'] and metadata['name'] == runtime and len(spec.get('containers', [])) == 1, 'foreign backend pod identity')
        container = spec['containers'][0]
        require(container.get('image') == self.task_ref and {'name': 'COVERAGE_BACKEND_OWNER', 'value': self.owner} in container.get('env', []), 'pod image/owner input mismatch')
        if status.get('phase') != 'Running' or not status.get('containerStatuses'):
            return None
        node = spec.get('nodeName')
        require(node in self.nodes, 'task scheduled outside owned kind nodes')
        require(len(status['containerStatuses']) == 1, 'ambiguous native task container status')
        pod_container = status['containerStatuses'][0]
        require(container.get('name') == 'atom' and pod_container.get('name') == 'atom' and
                pod_container.get('image') == normalized_image_ref(self.task_ref) and pod_container.get('restartCount') == 0,
                'pod container name/image/restart identity mismatch')
        if not pod_container.get('state', {}).get('running'):
            return None
        require(re.fullmatch(r'[a-f0-9]{64}', pod_container.get('containerID', '').removeprefix('containerd://')) and
                pod_container.get('containerID', '').startswith('containerd://'), 'exact containerd task container ID required')
        container_id = pod_container['containerID'].removeprefix('containerd://')
        node_id, mapping = self.import_mapping(node, recheck=True)
        image_id = pod_container.get('imageID', '')
        require(image_id == mapping['config_id'] or image_id in mapping['repo_digests'], 'pod image alias not bound to the imported CRI config')
        native = json.loads(self.docker('exec', node_id, 'crictl', 'inspect', container_id).stdout).get('status', {})
        require(native.get('id') == container_id and native.get('metadata', {}).get('name') == 'atom' and
                native.get('metadata', {}).get('attempt') == 0 and native.get('state') == 'CONTAINER_RUNNING' and
                native.get('image', {}).get('image') == normalized_image_ref(self.task_ref) and native.get('imageRef') == image_id,
                'concrete CRI container state/image identity mismatch')
        labels = native.get('labels', {})
        require(all(labels.get(key) == value for key, value in {
            'io.kubernetes.pod.uid': metadata['uid'], 'io.kubernetes.pod.name': runtime,
            'io.kubernetes.pod.namespace': self.namespace, 'io.kubernetes.container.name': 'atom',
        }.items()), 'concrete CRI container is not bound to the exact owned pod')
        namespace = json.loads(self.kubectl('get', 'namespace', self.namespace, '-o', 'json').stdout)['metadata']
        require(namespace.get('uid') == self.namespace_uid and namespace.get('labels', {}).get('caesium.coverage.backend-owner') == self.owner,
                'owned namespace changed during native witness')
        # Re-read the exact pod after native reads so a replaced container/pod
        # cannot borrow the original pod's public Running observation.
        current = json.loads(self.kubectl('-n', self.namespace, 'get', 'pod', runtime, '-o', 'json').stdout)
        require(current['metadata'].get('uid') == metadata['uid'] and current.get('spec', {}).get('nodeName') == node and
                current.get('status', {}).get('phase') == 'Running' and len(current.get('status', {}).get('containerStatuses', [])) == 1 and
                all(current['status']['containerStatuses'][0].get(key) == pod_container.get(key) for key in ('name', 'image', 'imageID', 'containerID', 'restartCount', 'state')), 'pod/container witness changed during CRI observation')
        self.import_mapping(node, recheck=True)
        return {'run_id': run_id, 'task_id': task['task_id'], 'runtime_id': runtime, 'native_id': metadata['uid'], 'node': node,
                'node_id': node_id, 'cri_container_id': container_id, 'image_id': image_id, 'config_id': mapping['config_id'],
                'cri_image': mapping, 'status': 'Running', 'engine': 'kubernetes'}

    def native_absent(self, runtime):
        if self.backend == 'podman':
            result = self.podman('container', 'exists', runtime, check=False)
            if result.returncode == 0:
                return False
            require(result.returncode == 1 and not result.stderr.strip(), 'native Libpod absence not proved (API/inventory failure)')
            require(self.podman('version', '--format', 'json').returncode == 0, 'native Libpod API unavailable after absence check')
            return True
        result = self.kubectl('-n', self.namespace, 'get', 'pod', runtime, '-o', 'json', check=False)
        if result.returncode == 0:
            return False
        require(re.fullmatch(r'Error from server \(NotFound\): pods "' + re.escape(runtime) + r'" not found', result.stderr.strip()), 'native pod absence not proved')
        namespace = json.loads(self.kubectl('get', 'namespace', self.namespace, '-o', 'json').stdout)
        require(namespace['metadata']['uid'] == self.namespace_uid and namespace['metadata']['labels'].get('caesium.coverage.backend-owner') == self.owner, 'owned namespace identity unavailable after absence')
        return True

    def manifest(self, case):
        marker = self.owner + '-' + case
        metadata = {'alias': marker, 'cache': {'enabled': False}}
        if case == 'deadline':
            metadata['runTimeout'] = '15s'
        def task(name, command):
            return {'name': name, 'engine': self.backend, 'image': self.task_ref, 'command': ['sh', '-c', command],
                    'env': {'COVERAGE_BACKEND_OWNER': self.owner}}
        if case == 'success':
            producer = task('producer', "echo 'PRODUCER_" + marker + "'; sleep 8; echo '##caesium::output {\"marker\":\"" + marker + "\"}'")
            producer['next'] = ['consumer']
            consumer = task('consumer', 'echo "CONSUMED_' + marker + ':$CAESIUM_OUTPUT_PRODUCER_MARKER"; test "$CAESIUM_OUTPUT_PRODUCER_MARKER" = "' + marker + '"; sleep 8')
            steps = [producer, consumer]
        else:
            command = 'echo FAILURE_' + marker + '; sleep 8; exit 17' if case == 'failure' else 'echo DEADLINE_' + marker + '; exec sleep 300'
            steps = [task('work', command)]
        return {'apiVersion': 'v1', 'kind': 'Job', 'metadata': metadata,
                'trigger': {'type': 'cron', 'configuration': {'expression': '0 0 31 2 *'}}, 'steps': steps}, marker

    def run_case(self, case):
        self.stage = 'scenario-' + case + '-apply'
        definition, marker = self.manifest(case)
        definitions = self.output / 'definitions'
        definitions.mkdir(exist_ok=True)
        path = definitions / case
        path.mkdir()
        (path / 'job.job.yaml').write_text(json.dumps(definition, indent=2))
        self.cli('job', 'apply', '--path', '/definitions/' + case)
        jobs = self.http('/v1/jobs')
        matches = [j for j in jobs if j.get('alias') == marker]
        require(len(matches) == 1, 'publicly applied backend job identity missing/ambiguous')
        job_id = matches[0]['id']
        self.stage = 'scenario-' + case + '-start'
        run_id = self.cli('run', 'start', '--job-id', job_id)
        require(str(uuid.UUID(run_id)) == run_id, 'public CLI start did not return an exact run identity')
        self.stage = 'scenario-' + case + '-observe'
        witnessed = {}
        live_logs = {}
        start = time.monotonic()
        def observe():
            run = self.http('/v1/jobs/' + job_id + '/runs/' + run_id)
            for task in run.get('tasks', []):
                if task.get('runtime_id') and task.get('status') == 'running' and task['runtime_id'] not in witnessed:
                    witness = self.native(run_id, task)
                    if witness:
                        witnessed[task['runtime_id']] = witness
                        if case == 'deadline':
                            live_logs[task['id']] = self.live_deadline_log(job_id, run_id, task, marker)
            return run if run.get('status') in ('succeeded', 'failed', 'cancelled', 'skipped') else None
        run = self.wait(observe, 'real backend ' + case + ' outcome', 120)
        verify_outcome(run, case, marker)
        require(all(task.get('engine') == self.backend and task.get('image') == self.task_ref for task in run['tasks']), 'durable backend/image identity mismatch')
        require(set(witnessed) == {t['runtime_id'] for t in run['tasks']}, 'each backend task must be observed natively Running before completion')
        self.stage = 'scenario-' + case + '-logs'
        logs = dict(live_logs)
        for task in run['tasks']:
            if case != 'deadline':
                logs[task['id']] = self.http(unfanned_log_path(job_id, run_id, task), text=True)
            self.wait(lambda: self.native_absent(task['runtime_id']), 'exact native backend cleanup', 30)
        combined = '\n'.join(logs.values())
        expected_log = {'success': 'CONSUMED_' + marker + ':' + marker, 'failure': 'FAILURE_' + marker, 'deadline': 'DEADLINE_' + marker}[case]
        require(expected_log in combined, 'actual backend logs/output propagation absent')
        if case == 'deadline':
            require(time.monotonic() - start >= 13, 'deadline journey failed prematurely')
        if self.backend == 'kubernetes':
            for witness in witnessed.values():
                events = json.loads(self.kubectl('-n', self.namespace, 'get', 'events', '--field-selector', 'involvedObject.uid=' + witness['native_id'], '-o', 'json').stdout)
                require(not any(e.get('reason') in ('Pulling', 'FailedPull', 'ErrImagePull', 'ImagePullBackOff') for e in events.get('items', [])), 'task image attempted a registry pull')
        result = {'case': case, 'status': 'pass', 'job_id': job_id, 'run_id': run_id, 'run_status': run['status'],
                  'task_statuses': [t['status'] for t in run['tasks']], 'exit_codes': [t.get('exit_code') for t in run['tasks']],
                  'native_running': list(witnessed.values()), 'native_absent': True, 'logs': logs, 'log_evidence': 'live-running-prefix' if case == 'deadline' else 'terminal-public-replay', 'elapsed_seconds': time.monotonic() - start}
        self.report['cases'].append(result)
        self.save(case + '.json', result)

    def stop_server(self):
        obj = self.owned('container', self.server_id)
        flush = self.docker('kill', '--signal=SIGUSR2', obj['Id'])
        stop = self.docker('stop', '--time', '60', obj['Id'], check=False, timeout=75)
        obj = self.owned('container', obj['Id'])
        logs = self.docker('logs', obj['Id'], check=False)
        # Auth is disabled, but scrub accidental key-shaped text defensively.
        (self.output / 'server.log').write_text(re.sub(r'csk_[A-Za-z0-9_-]+', '[REDACTED_API_KEY]', logs.stdout + logs.stderr))
        self.record_process('server', obj, self.server_raw, ['start'], stop.returncode, flush.returncode)

    def run(self):
        self.stage = 'image-validation'
        candidate = self.image_check(self.image)
        require(candidate.get('Config', {}).get('Labels', {}).get('org.opencontainers.image.revision') == self.ownership_id, 'loaded coverage image revision differs from producer')
        self.stage = 'network-allocation'
        self.register('network', self.network)
        self.docker('network', 'create', *self.labels(), self.network)
        self.stage = 'provisioning'
        if self.backend == 'kubernetes':
            self.prepare_kubernetes()
        else:
            self.prepare_podman()
        self.start_server()
        for case in ('success', 'failure', 'deadline'):
            self.run_case(case)
        self.stage = 'server-flush'
        self.stop_server()
        self.stage = 'process-validation'
        self.finalize_processes()
        self.report.update(complete=True, missing=False)

    def remove_owned(self, kind, name):
        result = self.docker(kind, 'inspect', name, check=False)
        if missing_object(result, kind, name):
            return
        require(result.returncode == 0, 'cleanup inventory failed; operator reconciliation required')
        obj = self.owned(kind, name)
        identity = obj['Name'] if kind == 'volume' else obj['Id']
        self.docker(kind, 'rm', *(['-f', '-v'] if kind == 'container' else []), identity)

    def cleanup(self):
        errors = []
        # Stop clients before tearing down their backend. Unexpected termination
        # invalidates evidence; cleanup cannot upgrade an incomplete process.
        for kind, name in reversed(self.resources):
            if kind == 'container':
                try:
                    self.remove_owned(kind, name)
                except Exception:
                    errors.append('owned container cleanup unavailable: ' + name)
        if getattr(self, 'cluster_attempted', False):
            try:
                self.capture_nodes()
                if getattr(self, 'namespace_uid', ''):
                    ns = json.loads(self.kubectl('get', 'namespace', self.namespace, '-o', 'json').stdout)
                    require(ns['metadata']['uid'] == self.namespace_uid and ns['metadata']['labels'].get('caesium.coverage.backend-owner') == self.owner, 'namespace cleanup ownership changed')
                    self.kubectl('delete', 'namespace', self.namespace, '--wait=true', '--timeout=30s')
            except Exception:
                errors.append('owned namespace cleanup could not be verified')
            for name in list(self.nodes):
                try:
                    self.docker('container', 'rm', '-f', '-v', self.node(name))
                except Exception:
                    errors.append('owned kind node cleanup unavailable: ' + name)
            # Never invoke kind delete/prune or delete its shared Docker network.
        for kind, name in reversed(self.resources):
            if kind != 'container':
                try:
                    self.remove_owned(kind, name)
                except Exception:
                    errors.append('owned resource cleanup unavailable: ' + name)
        try:
            shutil.rmtree(self.private)
        except OSError:
            errors.append('private credential directory cleanup unavailable')
        if getattr(self, 'uncertain_command', False):
            errors.append('interrupted command requires daemon outcome reconciliation; absence is not commit proof')
        self.report['cleanup_errors'] = errors
        if errors:
            self.report.update(complete=False, missing=True)
        self.save('backend-result.json', self.report)
        return errors


def validate_inputs(inputs, selected):
    require(inputs.get('schema_version') == 1, 'backend input schema required')
    inputs = dict(inputs)
    require(inputs.get('platform') in ('linux/amd64', 'linux/arm64'), 'explicit supported input platform required')
    if 'task_docker_image_id' in inputs:
        require(IMAGE_RE.fullmatch(inputs['task_docker_image_id']), 'invalid separate task Docker index identity')
    require(Path(inputs.get('docker_socket', '')).is_absolute(), 'explicit absolute Docker Unix socket required')
    inputs['task_archive'] = str(archive_identity(inputs.get('task_archive', ''), inputs.get('task_archive_sha256', ''), inputs.get('task_image_id', ''), inputs.get('task_image_ref', ''), inputs['platform'].split('/')[1]))
    if 'kubernetes' in selected:
        require(IMAGE_RE.fullmatch(inputs.get('kind_image_id', '')), 'preloaded immutable matching kind node image required')
    if 'podman' in selected:
        require(IMAGE_RE.fullmatch(inputs.get('podman_service_image_id', '')) and inputs.get('podman_privileged_approved') is True, 'Podman conditional prerequisite unavailable: root-owned preloaded matching service image and privileged fixture approval required')
    return inputs


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--context')
    parser.add_argument('--context-sha256')
    parser.add_argument('--inputs', required=True)
    parser.add_argument('--inputs-sha256', required=True)
    parser.add_argument('--output', required=True)
    parser.add_argument('--backend', choices=('kubernetes', 'podman', 'both'), default='both')
    parser.add_argument('--run', action='store_true')
    parser.add_argument('--smoke', action='store_true', help='with --run: native backend prerequisite only, no candidate or coverage contribution')
    args = parser.parse_args()
    selected = ['kubernetes', 'podman'] if args.backend == 'both' else [args.backend]
    inputs = validate_inputs(read_pinned(args.inputs, args.inputs_sha256), selected)
    inputs['_inputs_sha256'] = args.inputs_sha256
    if args.smoke:
        require(not args.context and not args.context_sha256, 'native smoke must not consume/fabricate a coverage producer context')
        context = {'coverage_id': 'native-smoke-' + secrets.token_hex(6)}
    else:
        require(args.context and args.context_sha256, 'pinned coverage producer context required')
        context = validate_context(read_pinned(args.context, args.context_sha256))
        require(inputs['platform'] == context['platform'], 'backend prerequisite platform differs from producer')
        context['_context_sha256'] = args.context_sha256
        context['_inputs_sha256'] = args.inputs_sha256
    if not args.run:
        print(json.dumps({'mode': 'offline plan; no Docker/network/resources', 'selected': selected, 'candidate_sha': context.get('candidate_sha'), 'image_id': context.get('image_id'), 'smoke': args.smoke, 'platform': inputs['platform'], 'requires': 'parent serial runtime; loaded images/native smoke/private kind TLS'}, indent=2))
        return 0
    output = fresh_directory(args.output)
    def interrupted(signum, frame):
        raise Refused('interrupted backend journey')
    for signum in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(signum, interrupted)
    records = []
    try:
        for backend in selected:
            driver = Driver(context, inputs, backend, output / backend)
            try:
                if args.smoke:
                    driver.stage = 'provisioning'
                    driver.register('network', driver.network)
                    driver.docker('network', 'create', *driver.labels(), driver.network)
                    driver.prepare_kubernetes() if backend == 'kubernetes' else driver.prepare_podman()
                    driver.report.update(smoke_complete=True)
                else:
                    driver.run()
            except BaseException as error:
                driver.report.update(complete=False, missing=True, failed=True)
                driver.failure_diagnostic(error)
                raise
            finally:
                for signum in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
                    signal.signal(signum, signal.SIG_IGN)
                driver.stage = 'cleanup'
                cleanup_errors = driver.cleanup()
                for signum in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
                    signal.signal(signum, interrupted)
                records.append(driver.report)
            if cleanup_errors:
                driver.failure_diagnostic(Refused('backend cleanup incomplete'))
            require(not cleanup_errors, 'backend cleanup incomplete')
    except BaseException:
        result = {'complete': False, 'selected_complete': False, 'candidate_sha': context.get('candidate_sha'), 'selected': selected, 'contributors': records}
        (output / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
        (output / ('smoke-result.json' if args.smoke else 'contribution.json')).write_text(json.dumps({'kind': 'native-backend-prerequisite-smoke', 'complete': len(records) == len(selected) and all(r.get('smoke_complete') and not r.get('failed') and not r.get('cleanup_errors') for r in records), 'coverage_contribution': False, 'platform': inputs['platform'], 'receipts': records} if args.smoke else contribution(context, records, selected), indent=2) + '\n')
        raise
    result = {'complete': selected == ['kubernetes', 'podman'], 'selected_complete': True, 'coverage_contribution': not args.smoke, 'candidate_sha': context.get('candidate_sha'), 'selected': selected, 'contributors': records}
    (output / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
    (output / ('smoke-result.json' if args.smoke else 'contribution.json')).write_text(json.dumps({'kind': 'native-backend-prerequisite-smoke', 'complete': len(records) == len(selected) and all(r.get('smoke_complete') and not r.get('failed') and not r.get('cleanup_errors') for r in records), 'coverage_contribution': False, 'platform': inputs['platform'], 'receipts': records} if args.smoke else contribution(context, records, selected), indent=2) + '\n')
    return 0


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except Exception:
        print('backend qualification refused/incomplete; no coverage PASS claimed', file=sys.stderr)
        raise SystemExit(1)
