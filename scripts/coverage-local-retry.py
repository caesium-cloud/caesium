#!/usr/bin/env python3
"""Original-image public whole-run retry with natural CLI owner drain.

No supplied profiles, database seeding, builds, pulls, or counter synthesis.
Only --run allocates resources. The parent owns the serial Docker lane.
"""
from __future__ import annotations

import argparse
import calendar
import datetime
import importlib.util
import json
import os
from pathlib import Path
import re
import signal
import sys
import time
import uuid


def module(name, filename):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(filename))
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


backend = module('local_retry_backend', 'coverage-backends.py')
common = module('local_retry_common', 'coverage-journeys.py')
require, Refused = backend.require, backend.Refused
LANE = 'local-retry'
ROLES = ('server-1', 'apply', 'start', 'retry', 'server-2')
UUID_RE = re.compile(r'[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}')


def timestamp(raw):
    match = re.fullmatch(r'(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d{1,9}))?(Z|[+-]\d\d:\d\d)', raw or '')
    require(match is not None, 'actual absolute timestamp missing')
    date = datetime.datetime.fromisoformat(match[1] + match[3].replace('Z', '+00:00'))
    require(date.year > 2000, 'zero or historical process timestamp refused')
    return calendar.timegm(date.utctimetuple()) * 1_000_000_000 + int((match[2] or '').ljust(9, '0'))


def exact_uuid(value):
    require(isinstance(value, str) and UUID_RE.fullmatch(value) and str(uuid.UUID(value)) == value, 'public durable UUID missing')
    return value


def failed_snapshot(snapshot, job_id, run_id, marker):
    require(snapshot.get('job_id') == job_id and snapshot.get('id') == run_id and snapshot.get('params') == {'case': marker}, 'durable run identity/parameters changed')
    require(snapshot.get('status') == 'failed' and snapshot.get('error') and 'cancel' not in snapshot['error'].lower(), 'run did not preserve ordinary failure')
    timestamp(snapshot.get('completed_at'))
    rows = snapshot.get('rows', {})
    require(set(rows) == {'preserved', 'failed'}, 'exact two concrete rows required')
    for name, status in (('preserved', 'succeeded'), ('failed', 'failed')):
        row = rows[name]
        exact_uuid(row.get('task_id'))
        exact_uuid(row.get('task_run_id'))
        require(row.get('status') == status and row.get('attempt') == 1 and row.get('cache_hit') is False, 'durable task status/attempt/cache policy mismatch')
        require(re.fullmatch(r'[0-9a-f]{64}', row.get('runtime_id', '')), 'concrete native runtime ID missing')
        require(timestamp(row.get('started_at')) < timestamp(row.get('completed_at')), 'task completion does not follow start')
    require(rows['preserved']['output'] == {'marker': marker}, 'successful predecessor output missing')
    require(rows['failed'].get('error') and 'cancel' not in rows['failed']['error'].lower() and rows['failed'].get('exit_code') == 17, 'failed retry lost ordinary exit17 evidence')
    require(rows['failed'].get('oom_known') is True and rows['failed'].get('oom_killed', False) is False, 'non-OOM native failure not observed')
    return rows


def validate_receipt(record, context, inputs, output):
    """Validate original five process raws and durable/native observations before merge."""
    require(output.is_dir() and not output.is_symlink() and not any(parent.is_symlink() for parent in output.parents), 'original raw root must be a regular owned directory')
    require(record.get('complete') is True and record.get('missing') is False and record.get('killed') is False and record.get('cleanup_errors') == [], 'incomplete or unclean local retry journey')
    require(record.get('lane') == LANE and record.get('kind') == 'real-local-retry-natural-drain', 'foreign local retry receipt')
    for key in ('candidate_sha', 'image_id', 'builder_image_id', 'build_context', 'image_provenance', 'verified'):
        require(record.get(key) == context[key], 'local retry producer identity changed')
    require(record.get('producer_context_sha256') == context['_context_sha256'] and record.get('producer_inputs_sha256') == context['_inputs_sha256'] and record.get('source_inventory_sha256') == context['source_inventory']['sha256'], 'local retry original producer bindings changed')
    require(record.get('task_image_id') == inputs['task_image_id'] and record.get('task_docker_image_id') == inputs['task_docker_image_id'] and record.get('task_archive_sha256') == inputs['task_archive_sha256'], 'task index/config/archive identities changed')
    absences = record.get('cleanup_absences', [])
    require(len(absences) == 7 and len({(a.get('kind'), a.get('name')) for a in absences}) == 7 and all(a.get('absent') is True for a in absences) and sorted(a.get('kind') for a in absences) == ['container'] * 5 + ['network', 'volume'], 'exact five processes/network/data-volume cleanup proof missing')
    require(all(a.get('name', '').startswith(record['owner'] + '-') and (a.get('kind') == 'volume' or re.fullmatch(r'[a-f0-9]{64}', a.get('identity', ''))) for a in absences), 'foreign cleanup receipt identity')
    processes = record.get('processes', [])
    require([p.get('role') for p in processes] == list(ROLES), 'exact five original process profiles required')
    seen, dirs = set(), {'cli': [], 'server': []}
    for process in processes:
        source = 'server' if process['role'].startswith('server-') else 'cli'
        require(process.get('source') == source and process.get('complete') is True and process.get('exit_code') == 0 and process.get('oom_killed') is False and process.get('killed') is False and process.get('restart_count') == 0, 'original process exit/source policy refused')
        for key in ('candidate_sha', 'image_id', 'builder_image_id', 'build_context', 'image_provenance', 'verified', 'producer_context_sha256', 'producer_inputs_sha256', 'source_inventory_sha256'):
            require(process.get(key) == record[key], 'original process escaped producer cohort')
        require(process.get('binary_sha256') == record.get('binary_sha256') and backend.HASH_RE.fullmatch(process.get('binary_sha256', '')), 'original binary identity changed')
        if context.get('binary_sha256'):
            require(process['binary_sha256'] == context['binary_sha256'], 'binary differs from producer')
        identity = process.get('container_id', '')
        require(re.fullmatch(r'[0-9a-f]{64}', identity) and identity not in seen, 'original process identity missing/duplicated')
        require(identity in {a['identity'] for a in absences if a['kind'] == 'container'}, 'process cleanup identity differs from original')
        seen.add(identity)
        require(timestamp(process.get('started_at')) < timestamp(process.get('finished_at')), 'original process lifetime missing')
        if source == 'server':
            require(process.get('flush_rc') == process.get('stop_rc') == 0 and process.get('flush') == 'sigusr2' and process.get('signal') == 'SIGTERM', 'server flush/clean shutdown missing')
        else:
            require(process.get('flush') == 'process-exit' and process.get('signal') is None and process.get('stop_rc') is None and process.get('flush_rc') is None, 'CLI did not exit naturally')
        relative = Path(process.get('raw_dir', ''))
        require(relative == Path('raw-' + process['role']), 'foreign raw path refused')
        require(backend.coverage_files(output / relative) == process.get('files'), 'original raw profile inventory changed/incomplete')
        sidecar = output / ('provenance-' + process['role'] + '.json')
        require(not sidecar.is_symlink() and json.loads(sidecar.read_text()) == process, 'process sidecar changed')
        dirs[source].append('journeys/' + LANE + '/' + relative.as_posix())
    indexed = {p['role']: p for p in processes}
    require(indexed['retry'].get('command') == ['run', 'retry', '--job-id', record['job_id'], '--run-id', record['run_id']], 'retry must be genuine no-server/no-partition CLI')
    require(timestamp(indexed['server-1']['finished_at']) < timestamp(indexed['retry']['started_at']) and timestamp(indexed['retry']['finished_at']) < timestamp(indexed['server-2']['started_at']), 'shared database generations overlapped')
    job_id, run_id = exact_uuid(record['job_id']), exact_uuid(record['run_id'])
    before = failed_snapshot(record['before'], job_id, run_id, record['marker'])
    after = failed_snapshot(record['after'], job_id, run_id, record['marker'])
    require(before['preserved'] == after['preserved'], 'succeeded predecessor was rerun or rewritten')
    old, new = before['failed'], after['failed']
    require(old['task_id'] == new['task_id'] and old['task_run_id'] == new['task_run_id'] and old['runtime_id'] != new['runtime_id'], 'same concrete failed row was not genuinely retried')
    require(timestamp(old['completed_at']) < timestamp(new['started_at']) < timestamp(new['completed_at']) <= timestamp(indexed['retry']['finished_at']), 'new attempt did not complete before natural CLI exit')
    require(timestamp(record['before']['completed_at']) < timestamp(record['after']['completed_at']) <= timestamp(indexed['retry']['finished_at']), 'run completion not owned by CLI')
    witness = record['native']
    require(witness.get('runtime_id') == new['runtime_id'] and witness.get('task_id') == new['task_id'] and witness.get('task_run_id') == new['task_run_id'] and witness.get('run_id') == run_id and witness.get('image_id') == inputs['task_docker_image_id'] and witness.get('running') is True and witness.get('cli_running') is True and witness.get('absent_before_observer') is True, 'native Running/absence identity evidence missing')
    require(timestamp(indexed['retry']['started_at']) <= witness['running_at_ns'] < witness['die_at_ns'] <= timestamp(indexed['retry']['finished_at']) and witness.get('exit_code') == 17, 'native die did not precede CLI natural exit')
    require(witness['die_at_ns'] <= witness['absent_at_ns'] < timestamp(indexed['server-2']['started_at']), 'native absence not observed before observer start')
    require(record.get('finished_log') == 'FINISHED_' + record['marker'] + ':' + record['marker'], 'public finished log/parameter evidence missing')
    return dirs


class Driver(backend.Driver):
    def __init__(self, context, inputs, output):
        super().__init__(context, inputs, LANE, output)
        self.report = {key: context[key] for key in ('candidate_sha', 'image_id', 'builder_image_id', 'build_context', 'image_provenance', 'verified')}
        self.report.update(schema_version=1, source='integration-journey', lane=LANE, kind='real-local-retry-natural-drain', module=backend.MODULE, complete=False, missing=True, killed=False,
                           producer_context_sha256=context['_context_sha256'], producer_inputs_sha256=context['_inputs_sha256'], source_inventory_sha256=context['source_inventory']['sha256'],
                           task_image_id=inputs['task_image_id'], task_docker_image_id=inputs['task_docker_image_id'], task_archive_sha256=inputs['task_archive_sha256'], owner=self.owner, processes=self.processes)
        self.native_resources = []
        self.known_rows = {}
        self.allocated_ids = {}
        self.absences = {}

    def docker(self, *args, **kwargs):
        kwargs.setdefault('timeout', 30)
        if getattr(self, 'cleanup_deadline', None) is not None:
            remaining = self.cleanup_deadline - time.monotonic()
            require(remaining > 0, 'cleanup deadline requires operator reconciliation')
            kwargs['timeout'] = min(kwargs['timeout'], remaining)
        return super().docker(*args, **kwargs)

    def owned(self, kind, name):
        obj = super().owned(kind, name)
        if kind == 'container':
            require(obj.get('Image') == self.image, 'owned candidate image changed')
        canonical = obj['Name'].removeprefix('/') if kind == 'container' else obj['Name']
        identity = obj['Name'] if kind == 'volume' else obj['Id']
        if kind in ('container', 'network', 'volume'):
            self.allocated_ids[(kind, canonical)] = identity
        return obj

    def environment(self):
        return {'GOCOVERDIR': '/coverage', 'DOCKER_HOST': 'unix:///var/run/docker.sock', 'CAESIUM_AUTH_MODE': 'none',
                'CAESIUM_MANUAL_TRIGGER_API_KEY': 'integration-test-key', 'CAESIUM_DATABASE_SHARDS': '1',
                'CAESIUM_DATABASE_PATH': '/var/lib/caesium/dqlite', 'CAESIUM_NODE_ADDRESS': '127.0.0.1:9001',
                'CAESIUM_DATABASE_NODES': '', 'CAESIUM_DATABASE_BOOTSTRAP_PEERS': '', 'CAESIUM_EXECUTION_MODE': 'local',
                'CAESIUM_WORKER_ENABLED': 'false', 'CAESIUM_RUN_OWNER_ENABLED': 'false', 'CAESIUM_CACHE_ENABLED': 'false',
                'CAESIUM_FRESHNESS_ENABLED': 'false', 'CAESIUM_RESOURCE_STATS_ENABLED': 'true', 'CAESIUM_RESOURCE_STATS_SAMPLE_INTERVAL': '100ms',
                'CAESIUM_SHUTDOWN_GRACE_PERIOD': '45s', 'CAESIUM_LOG_LEVEL': 'info'}

    def create(self, role, argv, local=False):
        name = self.owner + '-' + role
        self.register('container', name)
        raw = self.output / ('raw-' + role)
        raw.mkdir(mode=0o777)
        raw.chmod(0o777)
        args = ['create', '--pull=never', '--name', name, *self.labels(), '--platform', self.inputs['platform'], '--network', self.network, '--user', '0:0',
                '--mount', 'type=bind,src=' + str(raw) + ',dst=/coverage']
        env = {'GOCOVERDIR': '/coverage'}
        if local:
            env = self.environment()
            args += ['--mount', 'type=volume,src=' + self.database + ',dst=/var/lib/caesium/dqlite',
                     '--mount', 'type=bind,src=' + self.inputs['docker_socket'] + ',dst=/var/run/docker.sock']
        if role.startswith('server-'):
            args += ['-p', '127.0.0.1::8080']
        if role == 'apply':
            args += ['--mount', 'type=bind,src=' + str(self.output / 'definitions') + ',dst=/definitions,readonly']
        for key, value in env.items():
            args += ['-e', key + '=' + value]
        identity = self.docker(*args, self.image, *argv).stdout.strip()
        obj = self.owned('container', name)
        require(obj['Id'] == identity and obj['Image'] == self.image, 'allocated candidate image/ID changed')
        return identity, raw

    def record(self, role, identity, raw, argv, final=None):
        obj = self.owned('container', identity)
        state = obj['State']
        require(obj.get('Config', {}).get('Cmd') == argv, 'actual original CLI/server argv differs from receipt')
        require(obj['Image'] == self.image and state.get('Running') is False and state.get('ExitCode') == 0 and state.get('OOMKilled') is False and obj.get('RestartCount') == 0, 'candidate original process did not exit cleanly')
        record = {key: self.report[key] for key in ('candidate_sha', 'image_id', 'builder_image_id', 'build_context', 'image_provenance', 'verified', 'producer_context_sha256', 'producer_inputs_sha256', 'source_inventory_sha256')}
        record.update(schema_version=1, source='server' if role.startswith('server-') else 'cli', kind='gocoverdir', module=backend.MODULE, lane=LANE + '-' + role, role=role,
                      complete=True, missing=False, killed=False, container_id=identity, binary_sha256=self.binary_hash, command=argv, exit_code=0, oom_killed=False, restart_count=0,
                      started_at=state['StartedAt'], finished_at=state['FinishedAt'], raw_dir=raw.name, files=backend.coverage_files(raw), flush='process-exit', signal=None, flush_rc=None, stop_rc=None)
        if final is not None:
            record.update(flush='sigusr2', signal='SIGTERM', flush_rc=final['flush_rc'], stop_rc=final['stop_rc'])
        self.processes.append(record)
        self.save('provenance-' + role + '.json', record)
        self.save('processes.json', self.processes)
        return record

    def server(self, generation):
        role = 'server-' + str(generation)
        self.server_id, self.server_raw = self.create(role, ['start'], local=True)
        self.docker('start', self.server_id)
        obj = self.owned('container', self.server_id)
        self.binary_hash = self.docker('exec', self.server_id, 'sha256sum', '/bin/caesium').stdout.split()[0]
        require(backend.HASH_RE.fullmatch(self.binary_hash) and (not self.context.get('binary_sha256') or self.context['binary_sha256'] == self.binary_hash), 'candidate binary differs from producer')
        self.report['binary_sha256'] = self.binary_hash
        bindings = obj['NetworkSettings']['Ports'].get('8080/tcp', [])
        require(len(bindings) == 1 and bindings[0]['HostIp'] == '127.0.0.1', 'owned loopback binding missing')
        self.base = 'http://127.0.0.1:' + bindings[0]['HostPort']
        def ready():
            require(self.owned('container', self.server_id)['State']['Running'], 'owned server exited before health')
            try:
                return self.http('/health').get('status') == 'healthy'
            except Refused:
                require(self.owned('container', self.server_id)['State']['Running'], 'owned server exited before health')
                return False
        self.wait(ready, 'actual healthy server', 45)
        # This auth-none main server exposes health only after signal setup
        # and native database initialization. No connector config is enabled.
        self.server_name = self.owner + '-' + role

    def stop_server(self, generation):
        diagnostics = {'schema_version': 1}
        try:
            final = common.guarded_resource(self.docker, 'container', self.server_id, 'stop', self.ownership_id, self.context['coverage_id'], image=self.image, diagnostics=diagnostics)
            self.record('server-' + str(generation), self.server_id, self.server_raw, ['start'], final)
            self.remove_owned('container', self.server_id)
        finally:
            self.save('server-' + str(generation) + '-stop.json', diagnostics)

    def cli(self, role, argv):
        identity, raw = self.create(role, argv)
        if role == 'start':
            self.report['admission_uncertain'] = True
        self.docker('start', identity)
        self.wait(lambda: not self.owned('container', identity)['State']['Running'], 'natural public CLI exit', 45)
        logs = self.docker('logs', identity, timeout=10)
        if role == 'start':
            # Admission can precede profile validation. Capture the exact
            # public ID and concrete rows before any partial-profile refusal.
            self.report['run_id'] = exact_uuid(logs.stdout.strip())
            self.snapshot(self.report['job_id'], self.report['run_id'])
            require(len(self.known_rows) == 2, 'admitted concrete task identities incomplete')
            self.report['admission_uncertain'] = False
        self.record(role, identity, raw, argv)
        # These auth-none fixture commands have no credentials. Keep stdout
        # separate from stderr, and never retain a raw Docker inspect object.
        for stream, value in (('stdout', logs.stdout), ('stderr', logs.stderr)):
            (self.output / (role + '.' + stream)).write_text(re.sub(r'csk_[A-Za-z0-9_-]+', '[REDACTED_API_KEY]', value))
        return logs.stdout.strip()

    def snapshot(self, job_id, run_id):
        run = self.http('/v1/jobs/' + job_id + '/runs/' + run_id)
        result = {key: run.get(key) for key in ('id', 'job_id', 'params', 'status', 'error', 'completed_at')}
        result['rows'] = {}
        require(len(run.get('tasks', [])) == 2, 'unexpected durable task count')
        for task in run['tasks']:
            catalog = exact_uuid(task['task_id'])
            # The run detail ID is the catalog ID; partitions supplies the
            # actual concrete TaskRun ID even for this unfanned pipeline.
            page = self.http('/v1/jobs/' + job_id + '/runs/' + run_id + '/tasks/' + catalog + '/partitions')
            require(page.get('total') == 1 and len(page.get('partitions', [])) == 1, 'unexpected partition/fanout shape')
            row = dict(page['partitions'][0])
            row['task_id'] = catalog
            row['output'] = task.get('output', {})
            # Partitions formats timestamps to seconds. Preserve the run
            # detail's full precision for the natural-exit ordering proof.
            for key in ('started_at', 'completed_at'):
                if task.get(key):
                    require(timestamp(row.get(key)) // 1_000_000_000 == timestamp(task[key]) // 1_000_000_000, 'concrete/detail timestamp projections disagree')
                    row[key] = task[key]
            result['rows'][self.task_names[catalog]] = row
            self.known_rows[catalog] = row['task_run_id']
        return result

    def native(self, identity, job_id, run_id, task_id, task_run_id):
        obj = self.inspect('container', identity)
        env = obj.get('Config', {}).get('Env', [])
        name = obj.get('Name', '').removeprefix('/')
        allowed = [task_id + '-' + run_id, task_id + '-' + run_id + '-' + task_run_id]
        require(obj.get('Id') == identity and name in allowed and obj.get('Image') == self.inputs['task_docker_image_id'] and 'COVERAGE_LOCAL_RETRY_OWNER=' + self.owner in env and 'CAESIUM_RUN_ID=' + run_id in env, 'foreign native task; mutation refused')
        require(obj['State'].get('OOMKilled') is False, 'unexpected native OOM')
        return obj

    def native_absent(self, identity):
        result = self.docker('container', 'inspect', identity, check=False, timeout=10)
        if backend.missing_object(result, 'container', identity):
            return True
        require(result.returncode == 0, 'native inventory error cannot prove absence')
        return False

    def retry(self, job_id, run_id, before):
        argv = ['run', 'retry', '--job-id', job_id, '--run-id', run_id]
        identity, raw = self.create('retry', argv, local=True)
        old = before['rows']['failed']
        since = str(int(time.time()) - 1)
        self.docker('start', identity)
        task_id, task_run_id = old['task_id'], old['task_run_id']
        names = [task_id + '-' + run_id, task_id + '-' + run_id + '-' + task_run_id]
        def running():
            require(self.owned('container', identity)['State']['Running'], 'CLI exited before native retry Running witness')
            ids = self.docker('ps', '-aq', '--no-trunc', '--filter', 'name=' + task_id + '-' + run_id, timeout=10).stdout.split()
            for native_id in ids:
                obj = self.native(native_id, job_id, run_id, task_id, task_run_id)
                if obj['Name'].removeprefix('/') in names and obj['State']['Running']:
                    require(native_id != old['runtime_id'], 'old runtime borrowed for retry')
                    self.native_resources.append((native_id, job_id, run_id, task_id, task_run_id))
                    self.save('native-resources.json', self.native_resources)
                    return {'runtime_id': native_id, 'task_id': task_id, 'task_run_id': task_run_id, 'run_id': run_id, 'image_id': obj['Image'], 'running': True, 'cli_running': True, 'running_at_ns': time.time_ns()}
            return None
        witness = self.wait(running, 'actual native retry Running', 45)
        def removed():
            if self.native_absent(witness['runtime_id']):
                witness['absent_at_ns'] = time.time_ns()
                return True
            self.native(witness['runtime_id'], job_id, run_id, task_id, task_run_id)
            return False
        self.wait(removed, 'native retry removal', 45)
        # Polling can observe removal after the CLI has exited. The original
        # die timestamp and durable completion are independently required to
        # precede FinishedAt; do not invent an earlier absence observation.
        witness['absent_before_observer'] = True
        events = self.docker('events', '--since', since, '--until', str(int(time.time()) + 1), '--filter', 'container=' + witness['runtime_id'], '--filter', 'event=die', '--format', '{{json .}}', timeout=10)
        values = [json.loads(line) for line in events.stdout.splitlines() if line.strip()]
        die = [value for value in values if value.get('Action') == 'die' and value.get('Actor', {}).get('ID') == witness['runtime_id']]
        require(len(die) == 1 and die[0]['Actor']['Attributes'].get('exitCode') == '17' and isinstance(die[0].get('timeNano'), int), 'exact native die17 event missing')
        witness.update(die_at_ns=die[0]['timeNano'], exit_code=17)
        self.wait(lambda: not self.owned('container', identity)['State']['Running'], 'CLI natural retry exit', 45)
        self.record('retry', identity, raw, argv)
        self.report['native'] = witness

    def run(self):
        candidate = self.image_check(self.image)
        require(candidate.get('Config', {}).get('Labels', {}).get('org.opencontainers.image.revision') == self.ownership_id, 'candidate revision escaped original cohort')
        self.verify_task_docker_receipt()
        self.register('network', self.network)
        self.docker('network', 'create', *self.labels(), self.network)
        self.owned('network', self.network)
        self.database = self.volume('database')
        self.server(1)
        marker = self.owner + '-natural-drain'
        definition = {'apiVersion': 'v1', 'kind': 'Job', 'metadata': {'alias': marker, 'cache': {'enabled': False}},
                      'trigger': {'type': 'cron', 'configuration': {'expression': '0 0 31 2 *'}}, 'steps': []}
        for name, command in (
            ('preserved', "echo '##caesium::output {\"marker\":\"" + marker + "\"}'"),
            ('failed', 'echo "START_' + marker + ':$CAESIUM_PARAM_CASE"; sleep 8; echo "FINISHED_' + marker + ':$CAESIUM_PARAM_CASE"; exit 17')):
            task = {'name': name, 'engine': 'docker', 'image': self.task_ref, 'command': ['sh', '-c', command], 'env': {'COVERAGE_LOCAL_RETRY_OWNER': self.owner}}
            task['next' if name == 'preserved' else 'dependsOn'] = ['failed' if name == 'preserved' else 'preserved']
            definition['steps'].append(task)
        definitions = self.output / 'definitions'
        definitions.mkdir()
        (definitions / 'job.job.yaml').write_text(json.dumps(definition, indent=2))
        self.cli('apply', ['job', 'apply', '--path', '/definitions', '--server', 'http://' + self.server_name + ':8080'])
        jobs = [job for job in self.http('/v1/jobs') if job.get('alias') == marker]
        require(len(jobs) == 1, 'public apply job identity missing/ambiguous')
        job_id = exact_uuid(jobs[0]['id'])
        names = self.http('/v1/jobs/' + job_id + '/tasks')
        self.task_names = {exact_uuid(task['id']): task['name'] for task in names}
        require(len(self.task_names) == 2 and set(self.task_names.values()) == {'preserved', 'failed'}, 'exact applied task names/catalog IDs required')
        self.report.update(job_id=job_id, marker=marker)
        run_id = exact_uuid(self.cli('start', ['run', 'start', '--job-id', job_id, '--params', 'case=' + marker, '--server', 'http://' + self.server_name + ':8080']))
        self.report.update(job_id=job_id, run_id=run_id, marker=marker)
        def terminal():
            snapshot = self.snapshot(job_id, run_id)
            return snapshot if snapshot['status'] == 'failed' and snapshot.get('completed_at') else None
        before = self.wait(terminal, 'initial public failed run', 45)
        failed_snapshot(before, job_id, run_id, marker)
        require(all(self.native_absent(row['runtime_id']) for row in before['rows'].values()), 'initial native runtime still present')
        self.report.update(job_id=job_id, run_id=run_id, marker=marker, before=before)
        self.stop_server(1)
        self.retry(job_id, run_id, before)
        self.server(2)
        after = self.snapshot(job_id, run_id)
        self.report['after'] = after
        log = self.read_log(job_id, run_id, after['rows']['failed']['task_id'])
        expected = 'FINISHED_' + marker + ':' + marker
        require(expected in log.splitlines(), 'public retry finished log/params missing')
        self.report['finished_log'] = expected
        self.stop_server(2)

    def read_log(self, job_id, run_id, task_id):
        return self.http(backend.unfanned_log_path(job_id, run_id, {'id': task_id, 'task_id': task_id}), text=True)

    def remove_owned(self, kind, name):
        super().remove_owned(kind, name)
        require(backend.missing_object(self.docker(kind, 'inspect', name, check=False, timeout=10), kind, name), 'owned resource literal absence unproved')
        for (resource_kind, canonical), identity in self.allocated_ids.items():
            if resource_kind == kind and name in (canonical, identity):
                self.absences[(kind, canonical)] = {'kind': kind, 'name': canonical, 'identity': identity, 'absent': True}

    def cleanup(self):
        self.cleanup_deadline = time.monotonic() + 120
        errors = []
        # Join/remove candidate allocators first, then inspect only exact owned
        # run/catalog names. Failure cleanup never makes their profiles eligible.
        for kind, name in reversed(self.resources):
            if kind == 'container':
                try:
                    obj = self.owned(kind, name) if not backend.missing_object(self.docker(kind, 'inspect', name, check=False, timeout=10), kind, name) else None
                    if obj is not None and obj['State'].get('Running'):
                        self.docker('stop', '--time', '45', obj['Id'], timeout=60)
                        require(self.owned(kind, obj['Id'])['State'].get('Running') is False, 'allocator cleanup not joined')
                    self.remove_owned(kind, name)
                except Exception:
                    errors.append('owned candidate cleanup unproved: ' + name)
        if self.report.get('run_id'):
            for task_id, task_run_id in self.known_rows.items():
                try:
                    identities = self.docker('ps', '-aq', '--no-trunc', '--filter', 'name=' + task_id + '-' + self.report['run_id'], timeout=10).stdout.split()
                    for identity in identities:
                        obj = self.native(identity, self.report['job_id'], self.report['run_id'], task_id, task_run_id)
                        self.docker('container', 'rm', '-f', obj['Id'])
                        require(self.native_absent(identity), 'native cleanup absence unproved')
                except Exception:
                    errors.append('owned native cleanup unproved')
        for kind, name in reversed(self.resources):
            if kind != 'container':
                try:
                    self.remove_owned(kind, name)
                except Exception:
                    errors.append('owned resource cleanup unproved: ' + name)
        try:
            backend.shutil.rmtree(self.private)
        except OSError:
            errors.append('private directory cleanup unproved')
        if self.report.get('admission_uncertain'):
            errors.append('public admission identity requires operator reconciliation')
        if getattr(self, 'uncertain_command', False):
            errors.append('interrupted command requires outcome reconciliation')
        self.report.update(cleanup_errors=errors, cleanup_absences=list(self.absences.values()), complete=False, missing=True)
        self.save('provenance.json', self.report)
        return errors


def load(args):
    context = backend.validate_context(backend.read_pinned(args.context, args.context_sha256))
    inputs = backend.validate_inputs(backend.read_pinned(args.inputs, args.inputs_sha256), [])
    require(context['platform'] == inputs['platform'] and backend.IMAGE_RE.fullmatch(inputs.get('task_docker_image_id', '')), 'matching original task Docker index/platform required')
    context.update(_context_sha256=args.context_sha256, _inputs_sha256=args.inputs_sha256)
    inputs['_inputs_sha256'] = args.inputs_sha256
    return context, inputs


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for option in ('context', 'context-sha256', 'inputs', 'inputs-sha256', 'output'):
        parser.add_argument('--' + option, required=True)
    parser.add_argument('--run', action='store_true')
    parser.add_argument('--validate', action='store_true')
    args = parser.parse_args()
    require(args.run != args.validate, 'choose exactly run or validate')
    context, inputs = load(args)  # Immutable original input bytes before allocation.
    output = Path(args.output).absolute()
    if args.validate:
        record = json.loads(backend.regular(output / 'provenance.json').read_text())
        dirs = validate_receipt(record, context, inputs, output)
        for source, values in dirs.items():
            (output / (source + '-dirs.txt')).write_text('\n'.join(values) + '\n')
        return 0
    def interrupted(signum, frame):
        raise Refused('interrupted or timed out local retry journey')
    for signum in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP, signal.SIGALRM):
        signal.signal(signum, interrupted)
    signal.alarm(300)
    driver = Driver(context, inputs, output)
    success = False
    try:
        driver.run()
        success = True
    except BaseException:
        driver.report['refusal_category'] = 'public_journey_or_process_refused'
    finally:
        signal.alarm(0)
        for signum in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
            signal.signal(signum, signal.SIG_IGN)
        errors = driver.cleanup()
    if not success or errors:
        print('Local retry journey refused; see sanitized provenance.json', file=sys.stderr)
        return 1
    driver.report.update(complete=True, missing=False)
    # Guard final publication with exact five original raws after cleanup.
    try:
        validate_receipt(driver.report, context, inputs, output)
    except BaseException:
        driver.report.update(complete=False, missing=True, refusal_category='original_profile_or_observation_refused')
        driver.save('provenance.json', driver.report)
        raise Refused('local retry final evidence refused')
    driver.save('provenance.json', driver.report)
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except Exception:
        print('Local retry guard refused (raw diagnostics withheld)', file=sys.stderr)
        sys.exit(1)
