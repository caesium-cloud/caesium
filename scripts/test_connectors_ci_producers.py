"""Hermetic adversarial checks of the exact-attempt CI producer handoff."""
import copy
from contextlib import chdir
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import unittest
from unittest.mock import patch
import zipfile

SPEC = importlib.util.spec_from_file_location('connectors_producers', Path(__file__).with_name('connectors-ci-producers.py'))
MOD = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MOD)
C, H = 'a' * 40, 'b' * 40
START, READY, END = '2026-10-22T12:00:00Z', '2026-10-22T12:01:00Z', '2026-10-22T12:04:00Z'
EXPECTED = dict(repository='owner/repo', repository_id=123, run_id=456, run_attempt=2,
                workflow_ref='owner/repo/.github/workflows/ci.yml@refs/pull/1/merge',
                candidate_sha=C, head_sha=H, event='pull_request')


def jobs(pending=False):
    result = []
    for index, name in enumerate(MOD.PRODUCERS):
        tail = pending and name in MOD.BARRIERS
        result.append(dict(id=100 + index, name=name, run_id=456, run_attempt=2, head_sha=H,
                           status='in_progress' if tail else 'completed', conclusion=None if tail else 'success',
                           started_at=START, completed_at=None if tail else END,
                           steps=[dict(name='Build and publish', status='completed', conclusion='success', completed_at=READY)]
                           + ([dict(name=MOD.BARRIERS[name], status='completed', conclusion='success', completed_at=READY),
                               dict(name='Post cache export', status='in_progress' if tail else 'completed',
                                    conclusion=None if tail else 'success', completed_at=None if tail else END)]
                              if name in MOD.BARRIERS else [])))
    return result


def run():
    return dict(id=456, run_attempt=2, head_sha=H, event='pull_request',
                repository=dict(id=123, full_name='owner/repo'), path='.github/workflows/ci.yml',
                workflow_id=789, status='in_progress', conclusion=None)


def zip_bytes(files):
    output = io.BytesIO()
    with zipfile.ZipFile(output, 'w') as archive:
        for name, value in files.items(): archive.writestr(name, value)
    return output.getvalue()


class FakeClient:
    def __init__(self, states=None, details=None):
        self.states = states or [jobs()]
        self.details = details
        self.polls = 0
        self.job_requests = []
        self.loads = []
        self.downloads = []
        self.api_commands = []
        self.archives, self.artifacts = {}, []
        for index, (name, (_, members)) in enumerate(MOD.ARTIFACTS.items()):
            contents = {member: b'image archive' if member.endswith('.tar.zst') else
                        ((C if member == 'candidate-sha.txt' else 'sha256:' +
                          ('a' if member in ('release.txt', 'test.txt') else 'b' if member == 'integration-runner.txt' else 'c') * 64) + '\n').encode()
                        for member in members}
            raw = zip_bytes(contents)
            ident = 1000 + index
            self.archives[ident] = raw
            self.artifacts.append(dict(id=ident, name=name, expired=False, created_at=READY,
                                       size_in_bytes=len(raw), digest='sha256:' + hashlib.sha256(raw).hexdigest(),
                                       workflow_run=dict(id=456, repository_id=123, head_sha=H)))

    def remaining(self, maximum=None): return 30
    def get(self, endpoint):
        if '/actions/jobs/' in endpoint:
            self.job_requests.append(endpoint)
            index = max(self.polls-1, 0)
            rows = self.states[min(index, len(self.states)-1)]
            details = ({row['id']: row for row in rows} if self.details is None else
                       self.details[min(index, len(self.details)-1)])
            return copy.deepcopy(details[int(endpoint.rsplit('/', 1)[1])])
        return run()
    def pages(self, endpoint, key):
        if key == 'jobs':
            value = self.states[min(self.polls, len(self.states)-1)]
            self.polls += 1
            return copy.deepcopy(value)
        return copy.deepcopy(self.artifacts)
    def command(self, argv, *, output=None, api=False):
        if api:
            self.api_commands.append(list(argv))
            ident = int(argv[-1].split('/')[-2]); self.downloads.append(ident)
            output.write(self.archives[ident])
        else:
            self.loads.append(argv)


class ProducerHandoffTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='connectors-producer-probe-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def handoff(self, client, callback=None):
        output = self.root / 'handoff'; output.mkdir()
        read, write = os.pipe()
        try:
            with patch.object(MOD.time, 'sleep', side_effect=callback):
                MOD.handoff(client, EXPECTED, output, 'e' * 32, write)
        finally:
            try: os.close(write)
            except OSError: pass
        with os.fdopen(read, 'rb') as channel: digest = channel.read()
        return output, digest

    def test_artifact_ready_precedes_post_success_but_final_receipt_waits(self):
        client = FakeClient([jobs(True), jobs(True), jobs()])
        def waiting(_):
            self.assertTrue((self.root/'handoff/ready.json').is_file())
            self.assertFalse((self.root/'handoff/status.json').exists())
        output, digest = self.handoff(client, waiting)
        ready = (output/'ready.json').read_bytes()
        self.assertEqual(digest, hashlib.sha256(ready).hexdigest().encode()+b'\n')
        self.assertEqual(json.loads(ready)['status'], 'artifact-ready')
        final = json.loads((output/'status.json').read_text())
        self.assertEqual(final['status'], 'success')
        self.assertTrue(all(job['status'] == 'completed' and job['conclusion'] == 'success' for job in final['producers'].values()))
        self.assertEqual(len(client.downloads), 5)
        self.assertEqual(len(client.loads), 3)
        self.assertTrue(all('--allow-escape-sequences' in argv for argv in client.api_commands))
        self.assertEqual(set(final['artifacts']), set(MOD.ARTIFACTS))
        self.assertEqual(final['candidate_sha'], C)
        self.assertEqual(final['head_sha'], H)

    def test_late_post_failure_remains_failed_after_authenticated_ready(self):
        late = jobs(); late[1].update(conclusion='failure')
        with self.assertRaisesRegex(MOD.Invalid, 'job did not succeed'):
            self.handoff(FakeClient([jobs(True), late]))
        self.assertTrue((self.root/'handoff/ready.json').exists())
        self.assertFalse((self.root/'handoff/status.json').exists())

    def test_pending_post_cannot_become_final_success_on_wait_expiry(self):
        def expired(_): raise MOD.Invalid('producer handoff wait expired')
        with self.assertRaisesRegex(MOD.Invalid, 'wait expired'):
            self.handoff(FakeClient([jobs(True)]), expired)
        self.assertFalse((self.root/'handoff/status.json').exists())

    def test_completed_producers_fast_path(self):
        with patch.object(MOD.time, 'sleep', side_effect=AssertionError('unexpected poll')):
            output, _ = self.handoff(FakeClient())
        self.assertTrue((output/'status.json').exists())

    def test_sparse_list_steps_use_exact_job_details_through_final_seal(self):
        for sparse in ('absent', 'empty', 'null'):
            self.setUp(); rows = jobs()
            for row in rows:
                if row['name'] in MOD.BARRIERS:
                    if sparse == 'absent': row.pop('steps')
                    else: row['steps'] = [] if sparse == 'empty' else None
            client = FakeClient([rows], [{row['id']: row for row in jobs()}])
            with self.subTest(sparse=sparse):
                output, _ = self.handoff(client)
                self.assertTrue((output/'status.json').is_file())
                self.assertEqual(client.job_requests, [
                    'repos/owner/repo/actions/jobs/101', 'repos/owner/repo/actions/jobs/102'] * 2)
                self.assertEqual(len(client.downloads), 5)

    def test_detail_can_advance_from_list_in_progress_to_completed(self):
        client = FakeClient([jobs(True)], [{row['id']: row for row in jobs()}])
        def unexpected_wait(_): raise AssertionError('current job details are already completed')
        output, _ = self.handoff(client, unexpected_wait)
        final = json.loads((output/'status.json').read_text())
        self.assertTrue(all(row['status'] == 'completed' for row in final['producers'].values()))

    def test_foreign_or_malformed_detail_cannot_replace_list_identity(self):
        mutations = [(key, value) for key, value in (
            ('id', 999), ('id', True), ('name', 'foreign'), ('name', None),
            ('run_id', 999), ('run_id', True), ('run_attempt', 1), ('run_attempt', True), ('head_sha', C))]
        for key, value in mutations:
            self.setUp(); details = {row['id']: row for row in jobs()}
            details[101][key] = value
            client = FakeClient(details=[details])
            with self.subTest(key=key, value=value), self.assertRaises(MOD.Invalid): self.handoff(client)
            self.assertEqual(client.downloads, [])
        for malformed in (None, [], 'foreign', {}, False):
            self.setUp(); details = {row['id']: row for row in jobs()}; details[101] = malformed
            client = FakeClient(details=[details])
            with self.subTest(malformed=malformed), self.assertRaises(MOD.Invalid): self.handoff(client)
            self.assertEqual(client.downloads, [])

    def test_foreign_list_source_fails_before_job_detail_request(self):
        for key, value in (('id', None), ('id', True), ('run_id', 999), ('run_attempt', 1), ('head_sha', C)):
            self.setUp(); rows = jobs(); rows[1][key] = value
            client = FakeClient([rows], [{row['id']: row for row in jobs()}])
            with self.subTest(key=key), self.assertRaises(MOD.Invalid): self.handoff(client)
            self.assertEqual(client.job_requests, [])
            self.assertEqual(client.downloads, [])

    def test_sparse_or_failed_detail_never_falls_back_to_complete_list_steps(self):
        changes = [lambda row: row.pop('steps'), lambda row: row.update(steps=None),
                   lambda row: row.update(steps={}), lambda row: row.update(steps=[None]),
                   lambda row: row.update(steps=[]), lambda row: row.update(conclusion='failure'),
                   lambda row: row['steps'][0].update(conclusion='skipped'),
                   lambda row: row['steps'][2].update(conclusion='failure'),
                   lambda row: row['steps'].append(copy.deepcopy(row['steps'][1])),
                   lambda row: row['steps'].append(dict(name='Regular step', status='queued'))]
        for change in changes:
            self.setUp(); details = {row['id']: row for row in jobs()}; change(details[101])
            client = FakeClient(details=[details])
            with self.subTest(change=change), self.assertRaises(MOD.Invalid): self.handoff(client)
            self.assertEqual(client.downloads, [])
            self.assertFalse((self.root/'handoff/ready.json').exists())

    def test_list_to_detail_cannot_regress_started_or_completed_job(self):
        for status in ('in_progress', 'completed'):
            for changed in ('status', 'started_at', 'completed_at'):
                if status == 'in_progress' and changed == 'completed_at':
                    continue
                self.setUp(); rows = jobs(status == 'in_progress')
                details = {row['id']: copy.deepcopy(row) for row in rows}
                if changed == 'status':
                    details[101].update(status='queued' if status == 'in_progress' else 'in_progress',
                                        conclusion=None, completed_at=None)
                else:
                    details[101][changed] = '2026-10-22T12:00:01Z'
                client = FakeClient([rows], [details])
                with self.subTest(status=status, changed=changed), self.assertRaises(MOD.Invalid):
                    self.handoff(client)
                self.assertEqual(client.downloads, [])

    def test_exact_detail_failure_is_not_hidden_by_successful_list(self):
        client = FakeClient(); original = client.get
        def failed(endpoint):
            if '/actions/jobs/' in endpoint:
                raise MOD.Invalid('producer API request failed')
            return original(endpoint)
        with patch.object(client, 'get', side_effect=failed), self.assertRaisesRegex(MOD.Invalid, 'API request failed'):
            self.handoff(client)
        self.assertEqual(client.downloads, [])
        self.assertFalse((self.root/'handoff/ready.json').exists())

    def test_completed_detail_cannot_regress_on_final_seal_with_stale_list(self):
        active = {row['id']: row for row in jobs(True)}
        complete = {row['id']: row for row in jobs()}
        client = FakeClient([jobs(True)], [complete, active])
        with self.assertRaisesRegex(MOD.Invalid, 'producer jobs changed at final seal'):
            self.handoff(client)
        self.assertTrue((self.root/'handoff/ready.json').is_file())
        self.assertFalse((self.root/'handoff/status.json').exists())

    def test_detail_barrier_identity_cannot_change_while_list_steps_are_empty(self):
        for changed in ('removed', 'timestamp', 'queued'):
            self.setUp(); rows = jobs(True)
            for row in rows:
                if row['name'] in MOD.BARRIERS: row['steps'] = []
            first = {row['id']: row for row in jobs(True)}
            later = copy.deepcopy(first)
            if changed == 'removed': later[101]['steps'].pop(1)
            elif changed == 'timestamp': later[101]['steps'][1]['completed_at'] = '2026-10-22T12:01:01Z'
            else: later[101]['steps'][1].update(status='queued', conclusion=None)
            client = FakeClient([rows], [first, later])
            with self.subTest(changed=changed), self.assertRaisesRegex(
                    MOD.Invalid, 'producer publication (readiness|identity) changed'):
                self.handoff(client)
            self.assertTrue((self.root/'handoff/ready.json').is_file())
            self.assertFalse((self.root/'handoff/status.json').exists())

    def test_regular_steps_must_finish_before_ready(self):
        rows = jobs(True); rows[1]['steps'][1].update(status='queued', conclusion=None)
        _, ready, finished = MOD.select_jobs(rows, EXPECTED)
        self.assertFalse(ready); self.assertFalse(finished)

    def test_failed_skipped_foreign_and_ambiguous_jobs_fail_closed(self):
        cases = []
        for conclusion in ('failure', 'cancelled', 'skipped', 'timed_out', None):
            rows = jobs(); rows[1]['conclusion'] = conclusion; cases.append(rows)
        for key, value in (('run_id', 999), ('run_attempt', 1), ('head_sha', C), ('run_attempt', True), ('status', 'unknown')):
            rows = jobs(); rows[1][key] = value; cases.append(rows)
        cases += [jobs()+[jobs()[1]]]
        for rows in cases:
            with self.subTest(rows=rows):
                with self.assertRaises(MOD.Invalid): MOD.select_jobs(rows, EXPECTED)

    def test_cold_start_waits_for_not_yet_created_fixture_job_then_succeeds(self):
        cold = [job for job in jobs(True) if job['name'] != 'connector-fixture-images']
        client = FakeClient([cold, jobs(True), jobs()])
        checks = []
        def waiting(_):
            checks.append((self.root/'handoff/ready.json').exists())
        output, _ = self.handoff(client, waiting)
        self.assertEqual(checks, [False, True])
        self.assertTrue((output/'status.json').is_file())
        self.assertEqual(len(client.downloads), 5)

    def test_never_created_producer_expires_without_downloads_or_ready(self):
        cold = [job for job in jobs() if job['name'] != 'connector-fixture-images']
        client = FakeClient([cold])
        def expired(_): raise MOD.Invalid('producer handoff wait expired')
        with self.assertRaisesRegex(MOD.Invalid, 'wait expired'): self.handoff(client, expired)
        self.assertEqual(client.downloads, [])
        self.assertFalse((self.root/'handoff/ready.json').exists())

    def test_seen_producer_cannot_disappear_or_change_before_readiness(self):
        cold = [job for job in jobs() if job['name'] != 'connector-fixture-images']
        for changed in ('missing', 'id'):
            self.setUp(); later = jobs()
            if changed == 'missing': later = [job for job in later if job['name'] != 'images']
            else: later[1]['id'] = 999
            client = FakeClient([cold, later])
            with self.subTest(changed=changed), self.assertRaises(MOD.Invalid): self.handoff(client)
            self.assertEqual(client.downloads, [])

    def test_prestart_timestamps_can_change_until_actual_execution_begins(self):
        for name in MOD.PRODUCERS:
            for status in ('queued', 'waiting', 'pending'):
                self.setUp()
                first, later = jobs(True), jobs(True)
                for rows, start in ((first, None), (later, '2026-10-22T11:59:30Z')):
                    row = next(row for row in rows if row['name'] == name)
                    row.update(status=status, conclusion=None, started_at=start, completed_at=None, steps=[])
                churn = copy.deepcopy(later)
                next(row for row in churn if row['name'] == name)['started_at'] = '2026-10-22T11:59:45Z'
                client = FakeClient([first, later, churn, jobs(True), jobs()])
                def waiting(_):
                    if client.polls <= 3:
                        self.assertEqual(client.downloads, [])
                        self.assertFalse((self.root/'handoff/ready.json').exists())
                with self.subTest(name=name, status=status):
                    output, _ = self.handoff(client, waiting)
                    final = json.loads((output/'status.json').read_text())
                    self.assertEqual(final['producers'][name]['started_at'], START)
                    self.assertEqual(len(client.downloads), 5)

    def test_started_producers_reject_start_drift_null_or_status_regression(self):
        for name in MOD.PRODUCERS:
            for key, value in (('started_at', None), ('started_at', '2026-10-22T12:00:01Z'),
                               ('status', 'queued'), ('status', 'waiting'), ('status', 'pending')):
                self.setUp(); active = jobs(True)
                next(row for row in active if row['name'] == name).update(
                    status='in_progress', conclusion=None, completed_at=None, steps=[])
                changed = copy.deepcopy(active)
                next(row for row in changed if row['name'] == name)[key] = value
                client = FakeClient([active, changed])
                with self.subTest(name=name, key=key, value=value), self.assertRaisesRegex(
                        MOD.Invalid, 'producer job (start|execution) changed'):
                    self.handoff(client)
                self.assertEqual(client.downloads, [])
                self.assertFalse((self.root/'handoff/ready.json').exists())

    def test_prestart_producer_cannot_publish_ready_even_with_completed_steps(self):
        for name in MOD.PRODUCERS:
            self.setUp(); rows = jobs(True)
            next(row for row in rows if row['name'] == name).update(status='queued', conclusion=None, completed_at=None)
            client = FakeClient([rows])
            def expired(_): raise MOD.Invalid('producer handoff wait expired')
            with self.subTest(name=name), self.assertRaisesRegex(MOD.Invalid, 'wait expired'):
                self.handoff(client, expired)
            self.assertEqual(client.downloads, [])
            self.assertFalse((self.root/'handoff/ready.json').exists())

    def test_barrier_prefix_suffix_and_time_cannot_weaken_readiness(self):
        changes = [lambda row: row['steps'][0].update(conclusion='skipped'),
                   lambda row: row['steps'].append(dict(name='Another regular step', status='queued')),
                   lambda row: row['steps'].append(copy.deepcopy(row['steps'][1])),
                   lambda row: row['steps'][1].update(completed_at='2026-10-22T11:59:59Z'),
                   lambda row: row['steps'][2].update(conclusion='failure'),
                   lambda row: row['steps'].pop(1)]
        for change in changes:
            rows = jobs(); change(rows[1])
            with self.subTest(change=change):
                with self.assertRaises(MOD.Invalid): MOD.select_jobs(rows, EXPECTED)

    def test_prior_attempt_artifact_is_ignored_only_when_current_exists(self):
        client = FakeClient(); stale = copy.deepcopy(client.artifacts[0])
        stale.update(id=999, created_at='2026-10-22T11:00:00Z')
        selected = MOD.select_artifacts([stale]+client.artifacts, {job['name']: job for job in jobs()}, EXPECTED)
        self.assertEqual(selected[stale['name']]['id'], 1000)
        with self.assertRaisesRegex(MOD.Invalid, 'missing or ambiguous'):
            MOD.select_artifacts([stale]+client.artifacts[1:], {job['name']: job for job in jobs()}, EXPECTED)

    def test_artifact_foreign_expired_ambiguous_post_window_and_digest_fail(self):
        for mutation in (lambda row: row.update(expired=True), lambda row: row.update(digest=None),
                         lambda row: row['workflow_run'].update(head_sha=C),
                         lambda row: row['workflow_run'].update(id=999),
                         lambda row: row.update(created_at=END)):
            rows = FakeClient().artifacts; mutation(rows[0])
            with self.assertRaises(MOD.Invalid): MOD.select_artifacts(rows, {j['name']: j for j in jobs()}, EXPECTED)
        rows = FakeClient().artifacts; duplicate = copy.deepcopy(rows[0]); duplicate['id'] = 999; rows.append(duplicate)
        with self.assertRaises(MOD.Invalid): MOD.select_artifacts(rows, {j['name']: j for j in jobs()}, EXPECTED)

    def test_digest_and_both_candidate_receipts_precede_any_image_load(self):
        for invalid in ('digest', 'image-ids-amd64', 'connector-fixture-image-ids-amd64'):
            self.setUp(); client = FakeClient()
            if invalid == 'digest': client.artifacts[0]['digest'] = 'sha256:'+'f'*64
            else:
                row = next(row for row in client.artifacts if row['name'] == invalid)
                raw = client.archives[row['id']]
                with zipfile.ZipFile(io.BytesIO(raw)) as archive:
                    contents = {name: archive.read(name) for name in archive.namelist()}
                contents['candidate-sha.txt'] = H.encode()
                raw = zip_bytes(contents); client.archives[row['id']] = raw
                row['digest'] = 'sha256:'+hashlib.sha256(raw).hexdigest()
            with self.subTest(invalid=invalid), self.assertRaises(MOD.Invalid): self.handoff(client)
            self.assertEqual(client.loads, [])

    def test_exact_flat_zip_members_only(self):
        for members in ([('wanted', b'ok'), ('wanted', b'again')], [('../wanted', b'bad')],
                        [('/wanted', b'bad')], [('wanted', b'ok'), ('extra', b'bad')], [('a\\wanted', b'bad')]):
            self.setUp(); archive = self.root/'archive.zip'
            with zipfile.ZipFile(archive, 'w') as output:
                for name, raw in members: output.writestr(name, raw)
            with self.assertRaises(MOD.Invalid): MOD.extract(archive, self.root/'out', ('wanted',))
        self.setUp(); archive = self.root/'archive.zip'
        with zipfile.ZipFile(archive, 'w') as output:
            member = zipfile.ZipInfo('wanted'); member.external_attr = (stat.S_IFLNK | 0o777) << 16
            output.writestr(member, 'target')
        with self.assertRaises(MOD.Invalid): MOD.extract(archive, self.root/'out', ('wanted',))

    def test_run_identity_separates_pr_head_from_actual_checkout(self):
        self.assertEqual(MOD.validate_run(run(), EXPECTED), 789)
        for key, value in (('head_sha', C), ('run_attempt', 1), ('event', 'push'), ('path', 'foreign.yml'), ('conclusion', 'failure')):
            row = run(); row[key] = value
            if key == 'conclusion': row['status'] = 'completed'
            with self.subTest(key=key), self.assertRaises(MOD.Invalid): MOD.validate_run(row, EXPECTED)

    def test_event_candidate_and_repository_binding(self):
        event = self.root/'event.json'
        payload = dict(repository=dict(id=123, full_name='owner/repo'),
                       pull_request=dict(base=dict(sha='d'*40), head=dict(sha=H), merge_commit_sha=C))
        event.write_text(json.dumps(payload))
        env = dict(GITHUB_REPOSITORY='owner/repo', GITHUB_REPOSITORY_ID='123', GITHUB_RUN_ID='456', GITHUB_RUN_ATTEMPT='2',
                   GITHUB_WORKFLOW_REF=EXPECTED['workflow_ref'], GITHUB_SHA=C, GITHUB_EVENT_NAME='pull_request',
                   GITHUB_EVENT_PATH=str(event), CAESIUM_CONNECTORS_CI_HEAD_SHA=H, CANDIDATE_SHA=C)
        with patch.object(MOD, 'candidate_parents', return_value=['d'*40, H]):
            self.assertEqual(MOD.context(env), EXPECTED)
            for key, value in (('GITHUB_SHA', H), ('CAESIUM_CONNECTORS_CI_HEAD_SHA', C), ('GITHUB_EVENT_NAME', 'pull_request_target'),
                               ('GITHUB_REPOSITORY_ID', '999'), ('GITHUB_WORKFLOW_REF', 'owner/repo/foreign.yml@master')):
                with self.subTest(key=key), self.assertRaises(MOD.Invalid): MOD.context(dict(env, **{key: value}))

    def test_complete_pagination_duplicate_ids_and_incomplete_pages(self):
        client = MOD.Client(EXPECTED, MOD.time.monotonic()+30)
        pages = [dict(total_count=101, jobs=[dict(id=i) for i in range(1, 101)]), dict(total_count=101, jobs=[dict(id=101)])]
        with patch.object(client, 'get', side_effect=pages) as get:
            self.assertEqual(len(client.pages('endpoint', 'jobs')), 101)
            self.assertIn('page=2', get.call_args.args[0])
        for last in (dict(total_count=101, jobs=[]), dict(total_count=101, jobs=[dict(id=1)])):
            with patch.object(client, 'get', side_effect=[pages[0], last]), self.assertRaises(MOD.Invalid):
                client.pages('endpoint', 'jobs')

    def test_request_limits_and_token_isolation_for_image_loader(self):
        client = MOD.Client(EXPECTED, MOD.time.monotonic()+1800)
        with patch.dict(os.environ, GH_TOKEN='private', GITHUB_TOKEN='private2', GH_DEBUG='api'), \
                patch.object(MOD.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, b'{}')) as invoke:
            client.get('repos/owner/repo/actions/runs/456')
            self.assertIn('--allow-escape-sequences', invoke.call_args.args[0])
            self.assertLessEqual(invoke.call_args.kwargs['timeout'], 30)
            self.assertEqual(invoke.call_args.kwargs['env']['GH_TOKEN'], 'private')
            self.assertNotIn('GH_DEBUG', invoke.call_args.kwargs['env'])
            client.command(['docker', 'load'])
            self.assertNotIn('GH_TOKEN', invoke.call_args.kwargs['env'])
            self.assertNotIn('GITHUB_TOKEN', invoke.call_args.kwargs['env'])
            self.assertGreater(invoke.call_args.kwargs['timeout'], 30)
        with patch.object(MOD.subprocess, 'run', side_effect=subprocess.CalledProcessError(1, ['gh'], stderr=b'private')):
            with self.assertRaisesRegex(MOD.Invalid, '^producer API request failed$'): client.get('endpoint')


class PullRequestCheckoutTests(unittest.TestCase):
    """Use literal Git objects in a real depth-one checkout, without fetching."""

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='connectors-pr-checkout-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source, self.checkout = self.root/'source.git', self.root/'checkout'
        self.git_env = dict(os.environ, GIT_AUTHOR_NAME='Probe', GIT_AUTHOR_EMAIL='probe@example.test',
                            GIT_COMMITTER_NAME='Probe', GIT_COMMITTER_EMAIL='probe@example.test',
                            GIT_AUTHOR_DATE='2026-10-22T00:00:00Z', GIT_COMMITTER_DATE='2026-10-22T00:00:00Z')
        self.git('init', '--bare', '-q', str(self.source), cwd=self.root)
        self.tree = self.git('mktree', input=b'')
        self.base = self.git('commit-tree', self.tree, input=b'base\n')
        self.head = self.git('commit-tree', self.tree, '-p', self.base, input=b'head\n')
        self.candidate = self.git('commit-tree', self.tree, '-p', self.base, '-p', self.head, input=b'merge\n')
        self.git('update-ref', 'refs/heads/master', self.candidate)
        self.git('symbolic-ref', 'HEAD', 'refs/heads/master')
        self.git('-c', 'protocol.file.allow=always', 'clone', '-q', '--depth=1', '--no-local',
                 self.source.as_uri(), str(self.checkout), cwd=self.root)
        self.payload = dict(repository=dict(id=123, full_name='owner/repo'),
                            pull_request=dict(base=dict(sha=self.base), head=dict(sha=self.head), merge_commit_sha=None))
        self.event = self.root/'event.json'
        self.env = dict(GITHUB_REPOSITORY='owner/repo', GITHUB_REPOSITORY_ID='123', GITHUB_RUN_ID='456',
                        GITHUB_RUN_ATTEMPT='2', GITHUB_WORKFLOW_REF=EXPECTED['workflow_ref'],
                        GITHUB_SHA=self.candidate, GITHUB_EVENT_NAME='pull_request', GITHUB_EVENT_PATH=str(self.event),
                        CAESIUM_CONNECTORS_CI_HEAD_SHA=self.head, CANDIDATE_SHA=self.candidate)

    def git(self, *arguments, cwd=None, input=None):
        result = subprocess.run(['git', '-c', 'core.hooksPath=/dev/null', '-c', 'commit.gpgsign=false', *arguments],
                                cwd=cwd or self.source, env=self.git_env, input=input, capture_output=True, check=True)
        return result.stdout.decode().strip()

    def context(self, payload=None, env=None):
        self.event.write_text(json.dumps(self.payload if payload is None else payload))
        with chdir(self.checkout):
            return MOD.context(self.env if env is None else env)

    def store_candidate(self, parents, message='candidate'):
        raw = ('tree '+self.tree+'\n' + ''.join('parent '+parent+'\n' for parent in parents)
               + 'author Probe <probe@example.test> 1792627200 +0000\n'
               + 'committer Probe <probe@example.test> 1792627200 +0000\n\n'+message+'\n').encode()
        return self.git('hash-object', '-t', 'commit', '-w', '--stdin', cwd=self.checkout, input=raw)

    def run_main(self, env, *, valid=False):
        self.event.write_text(json.dumps(self.payload))
        output = self.root/('main-'+str(len(list(self.root.glob('main-*'))))); output.mkdir()
        read, write = os.pipe()
        old_mask = os.umask(0o077)
        try:
            argv = ['connectors-ci-producers.py', '--output', str(output), '--invocation', 'e'*32, '--ready-fd', str(write)]
            with patch.dict(os.environ, env), patch.object(MOD.sys, 'argv', argv), \
                    patch.object(MOD, 'Client') as client, patch.object(MOD, 'handoff') as handoff, chdir(self.checkout):
                if valid:
                    MOD.main()
                    self.assertEqual(handoff.call_args.args[1]['candidate_sha'], env['CANDIDATE_SHA'])
                    self.assertEqual(handoff.call_args.args[1]['head_sha'], self.head)
                else:
                    with self.assertRaises(MOD.Invalid): MOD.main()
                    client.assert_not_called(); handoff.assert_not_called()
        finally:
            os.umask(old_mask)
            os.close(read); os.close(write)

    def test_real_shallow_checkout_accepts_null_stale_or_absent_payload_merge_sha(self):
        self.assertEqual((self.checkout/'.git/shallow').read_text().strip(), self.candidate)
        self.assertEqual(self.git('show', '-s', '--format=%P', 'HEAD', cwd=self.checkout), '')
        for parent in (self.base, self.head):
            absent = subprocess.run(['git', 'cat-file', '-e', parent], cwd=self.checkout, capture_output=True)
            self.assertNotEqual(absent.returncode, 0, 'parent objects must be absent from depth-one fixture')
        for merge in (None, 'f'*40, 'absent'):
            payload = copy.deepcopy(self.payload)
            if merge == 'absent': payload['pull_request'].pop('merge_commit_sha')
            else: payload['pull_request']['merge_commit_sha'] = merge
            with self.subTest(merge=merge):
                record = self.context(payload)
                self.assertEqual(record, dict(EXPECTED, candidate_sha=self.candidate, head_sha=self.head))
        self.run_main(self.env, valid=True)

    def test_pr_event_base_is_typed_and_head_matches_literal_parent(self):
        for section in ('base', 'head'):
            for value in (None, False, 42, 'f'*39, 'f'*41, 'F'*40, 'f'*40):
                if section == 'base' and value == 'f'*40:
                    continue  # A valid payload base can lag the literal merge ref.
                payload = copy.deepcopy(self.payload); payload['pull_request'][section]['sha'] = value
                with self.subTest(section=section, value=value), self.assertRaises(MOD.Invalid): self.context(payload)
        payload = copy.deepcopy(self.payload); payload['pull_request'].pop('base')
        with self.assertRaises(MOD.Invalid): self.context(payload)
        payload = copy.deepcopy(self.payload); payload['pull_request'].pop('head')
        with self.assertRaises(MOD.Invalid): self.context(payload)

    def test_ci_candidate_and_head_context_reject_missing_malformed_or_foreign_sha(self):
        for key in ('CANDIDATE_SHA', 'CAESIUM_CONNECTORS_CI_HEAD_SHA'):
            for value in (None, False, 42, 'f'*39, 'f'*41, 'F'*40, 'f'*40):
                env = dict(self.env, **{key: value})
                if key == 'CANDIDATE_SHA': env['GITHUB_SHA'] = value
                with self.subTest(key=key, value=value), self.assertRaises(MOD.Invalid): self.context(env=env)

    def test_regenerated_shallow_merge_accepts_advanced_base_with_same_head(self):
        advanced = self.git('commit-tree', self.tree, '-p', self.base, input=b'advanced base\n')
        candidate = self.git('commit-tree', self.tree, '-p', advanced, '-p', self.head, input=b'regenerated merge\n')
        self.git('update-ref', 'refs/heads/master', candidate)
        self.checkout = self.root/'advanced-checkout'
        self.git('-c', 'protocol.file.allow=always', 'clone', '-q', '--depth=1', '--no-local',
                 self.source.as_uri(), str(self.checkout), cwd=self.root)
        self.assertEqual((self.checkout/'.git/shallow').read_text().strip(), candidate)
        for parent in (self.base, advanced, self.head):
            absent = subprocess.run(['git', 'cat-file', '-e', parent], cwd=self.checkout, capture_output=True)
            self.assertNotEqual(absent.returncode, 0)
        self.env.update(CANDIDATE_SHA=candidate, GITHUB_SHA=candidate)
        self.assertNotEqual(self.payload['pull_request']['base']['sha'], advanced)
        self.assertEqual(self.context(), dict(EXPECTED, candidate_sha=candidate, head_sha=self.head))
        self.run_main(self.env, valid=True)
        payload = copy.deepcopy(self.payload); payload['pull_request']['head']['sha'] = advanced
        with self.assertRaises(MOD.Invalid): self.context(payload)

    def test_literal_commit_rejects_missing_reordered_extra_duplicate_or_foreign_head_parents(self):
        for parents in ([], [self.base], [self.head, self.base], [self.base, self.head, 'f'*40],
                        [self.base, self.head, self.head], [self.base, 'f'*40], [self.head, self.head]):
            candidate = self.store_candidate(parents)
            self.git('update-ref', 'HEAD', candidate, cwd=self.checkout)
            env = dict(self.env, CANDIDATE_SHA=candidate, GITHUB_SHA=candidate)
            with self.subTest(parents=parents), self.assertRaises(MOD.Invalid): self.context(env=env)

    def test_parent_lines_in_commit_message_do_not_change_header_identity(self):
        candidate = self.store_candidate([self.base, self.head], message='message\nparent '+'f'*40)
        self.git('update-ref', 'HEAD', candidate, cwd=self.checkout)
        env = dict(self.env, CANDIDATE_SHA=candidate, GITHUB_SHA=candidate)
        self.assertEqual(self.context(env=env)['candidate_sha'], candidate)

    def test_replace_ref_cannot_substitute_for_literal_candidate_parents(self):
        candidate = self.store_candidate([self.base, 'f'*40])
        self.git('update-ref', 'HEAD', candidate, cwd=self.checkout)
        self.git('update-ref', 'refs/replace/'+candidate, self.candidate, cwd=self.checkout)
        self.assertIn('parent '+self.head, self.git('cat-file', 'commit', candidate, cwd=self.checkout))
        env = dict(self.env, CANDIDATE_SHA=candidate, GITHUB_SHA=candidate)
        with self.assertRaises(MOD.Invalid): self.context(env=env)

    def test_missing_candidate_or_actual_head_rejects_before_api_or_load(self):
        self.run_main(dict(self.env, CANDIDATE_SHA='f'*40, GITHUB_SHA='f'*40))
        wrong_checkout = self.store_candidate([self.base, self.head], message='other valid merge')
        self.run_main(dict(self.env, CANDIDATE_SHA=wrong_checkout, GITHUB_SHA=wrong_checkout))
        self.git('symbolic-ref', 'HEAD', 'refs/heads/missing', cwd=self.checkout)
        self.run_main(self.env)


if __name__ == '__main__':
    unittest.main()
