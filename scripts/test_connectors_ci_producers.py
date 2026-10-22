"""Hermetic adversarial checks of the exact-attempt CI producer handoff."""
import copy
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
    def __init__(self, states=None):
        self.states = states or [jobs()]
        self.polls = 0
        self.loads = []
        self.downloads = []
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
    def get(self, endpoint): return run()
    def pages(self, endpoint, key):
        if key == 'jobs':
            value = self.states[min(self.polls, len(self.states)-1)]
            self.polls += 1
            return copy.deepcopy(value)
        return copy.deepcopy(self.artifacts)
    def command(self, argv, *, output=None, api=False):
        if api:
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
        payload = dict(repository=dict(id=123, full_name='owner/repo'), pull_request=dict(head=dict(sha=H), merge_commit_sha=C))
        event.write_text(json.dumps(payload))
        env = dict(GITHUB_REPOSITORY='owner/repo', GITHUB_REPOSITORY_ID='123', GITHUB_RUN_ID='456', GITHUB_RUN_ATTEMPT='2',
                   GITHUB_WORKFLOW_REF=EXPECTED['workflow_ref'], GITHUB_SHA=C, GITHUB_EVENT_NAME='pull_request',
                   GITHUB_EVENT_PATH=str(event), CAESIUM_CONNECTORS_CI_HEAD_SHA=H, CANDIDATE_SHA=C)
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
            self.assertLessEqual(invoke.call_args.kwargs['timeout'], 30)
            self.assertEqual(invoke.call_args.kwargs['env']['GH_TOKEN'], 'private')
            self.assertNotIn('GH_DEBUG', invoke.call_args.kwargs['env'])
            client.command(['docker', 'load'])
            self.assertNotIn('GH_TOKEN', invoke.call_args.kwargs['env'])
            self.assertNotIn('GITHUB_TOKEN', invoke.call_args.kwargs['env'])
            self.assertGreater(invoke.call_args.kwargs['timeout'], 30)
        with patch.object(MOD.subprocess, 'run', side_effect=subprocess.CalledProcessError(1, ['gh'], stderr=b'private')):
            with self.assertRaisesRegex(MOD.Invalid, '^producer API request failed$'): client.get('endpoint')


if __name__ == '__main__':
    unittest.main()
