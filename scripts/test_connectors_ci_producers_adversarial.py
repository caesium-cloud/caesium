"""State-transition attacks on the producer helper's final success seal.

The primary module supplies valid API/ZIP fixtures. These tests change a fresh
observation only after the helper has really downloaded, loaded and published
its authenticated artifact-ready receipt. No command reaches Docker or GitHub.
"""
import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


SPEC = importlib.util.spec_from_file_location(
    'connectors_producer_adversarial_fixtures',
    Path(__file__).with_name('test_connectors_ci_producers.py'),
)
FIXTURES = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(FIXTURES)
MOD = FIXTURES.MOD


class TransitionClient(FIXTURES.FakeClient):
    """Return valid READY, producer completion, then an independently bad seal."""

    def __init__(self, *, tail_change=None, seal_jobs=None, seal_artifacts=None,
                 seal_run=None):
        super().__init__([FIXTURES.jobs(True), FIXTURES.jobs(), FIXTURES.jobs()])
        self.tail_change = tail_change
        self.seal_jobs = seal_jobs
        self.seal_artifacts = seal_artifacts
        self.seal_run = seal_run
        self.jobs_observations = 0
        self.artifact_observations = 0
        self.api_observations = []

    def get(self, endpoint):
        self.api_observations.append(endpoint)
        result = FIXTURES.run()
        # The unqualified endpoint is intentionally the LAST, fresh run view.
        if endpoint.endswith('/runs/456') and self.seal_run:
            self.seal_run(result)
        return result

    def pages(self, endpoint, key):
        self.api_observations.append(endpoint)
        if key == 'jobs':
            self.jobs_observations += 1
            result = super().pages(endpoint, key)
            if self.jobs_observations == 2 and self.tail_change:
                self.tail_change(result)
            if self.jobs_observations >= 3 and self.seal_jobs:
                self.seal_jobs(result)
            return result
        self.artifact_observations += 1
        result = copy.deepcopy(self.artifacts)
        if self.artifact_observations >= 2 and self.seal_artifacts:
            self.seal_artifacts(result)
        return result


class ProducerFinalSealAdversarialTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='producer-final-seal-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.session = 0
        self.output = None
        self.digest = None

    def invoke(self, client):
        self.session += 1
        self.output = self.root / str(self.session)
        self.output.mkdir()
        read_fd, write_fd = os.pipe()
        try:
            with patch.object(MOD.time, 'sleep'), patch.object(
                    MOD.subprocess, 'run', side_effect=AssertionError('live command is forbidden')):
                MOD.handoff(client, FIXTURES.EXPECTED, self.output, 'e' * 32, write_fd)
        finally:
            # The helper closes its writer at READY, including on later error.
            # Our pre-READY error path still closes it so this read cannot hang.
            try:
                os.close(write_fd)
            except OSError:
                pass
            try:
                self.digest = os.read(read_fd, 66)
            finally:
                os.close(read_fd)

    def assert_ready_but_no_success(self, client):
        ready = self.output / 'ready.json'
        self.assertTrue(ready.is_file(), 'attack must occur after authentic READY')
        raw = ready.read_bytes()
        self.assertEqual(self.digest, hashlib.sha256(raw).hexdigest().encode() + b'\n')
        record = json.loads(raw)
        self.assertEqual(record['status'], 'artifact-ready')
        self.assertEqual(record['candidate_sha'], FIXTURES.C)
        self.assertEqual(record['head_sha'], FIXTURES.H)
        self.assertNotEqual(record['candidate_sha'], record['head_sha'])
        self.assertFalse((self.output / 'status.json').exists(), 'READY must never launder final failure')
        self.assertEqual(len(client.downloads), 5, 'metadata refresh cannot redownload an alternative')
        self.assertEqual(len(client.loads), 3, 'metadata refresh cannot replay image loads')

    def reject(self, client, message):
        with self.assertRaisesRegex(MOD.Invalid, message):
            self.invoke(client)
        self.assert_ready_but_no_success(client)

    def test_post_step_can_fail_while_job_remains_running_after_ready(self):
        def failed_post(rows):
            for job in rows:
                if job['name'] == 'images':
                    job.update(status='in_progress', conclusion=None, completed_at=None)
                    job['steps'][-1].update(status='completed', conclusion='failure')
        client = TransitionClient(tail_change=failed_post)
        self.reject(client, 'step did not succeed')
        self.assertEqual(client.jobs_observations, 2)

    def test_final_seal_requires_artifact_still_exists(self):
        client = TransitionClient(seal_artifacts=lambda rows: rows.pop(0))
        self.reject(client, 'missing or ambiguous')
        self.assertEqual(client.artifact_observations, 2)

    def test_final_seal_rejects_same_name_same_digest_replacement(self):
        def replaced(rows):
            rows[0]['id'] += 5000
        client = TransitionClient(seal_artifacts=replaced)
        self.reject(client, 'artifacts changed at final seal')

    def test_final_seal_rejects_digest_or_valid_created_time_drift(self):
        for key, value in [('digest', 'sha256:' + 'f' * 64),
                           ('created_at', '2026-10-22T12:00:30Z')]:
            with self.subTest(key=key):
                def changed(rows, key=key, value=value):
                    rows[0][key] = value
                self.reject(TransitionClient(seal_artifacts=changed), 'artifacts changed at final seal')

    def test_final_seal_rejects_expiration_of_the_downloaded_artifact(self):
        def expired(rows):
            rows[0]['expired'] = True
        self.reject(TransitionClient(seal_artifacts=expired), 'identity is foreign or invalid')

    def test_final_seal_does_not_substitute_prior_attempt_same_name(self):
        def stale_only(rows):
            rows[0].update(id=9000, created_at='2026-10-22T11:59:59Z')
        client = TransitionClient(seal_artifacts=stale_only)
        self.reject(client, 'missing or ambiguous')
        self.assertNotIn(9000, client.downloads)

    def test_final_seal_rejects_producer_job_id_or_start_time_drift(self):
        for name in MOD.PRODUCERS:
            for key, value in [('id', 9000), ('started_at', '2026-10-22T12:00:01Z')]:
                with self.subTest(producer=name, key=key):
                    def changed(rows, name=name, key=key, value=value):
                        next(job for job in rows if job['name'] == name)[key] = value
                    self.reject(TransitionClient(seal_jobs=changed),
                                'jobs changed at final seal|job (identity|start) changed')

    def test_final_seal_rejects_job_attempt_drift_with_stable_run_view(self):
        def changed(rows):
            next(job for job in rows if job['name'] == 'images')['run_attempt'] = 3
        self.reject(TransitionClient(seal_jobs=changed), 'job source is foreign')

    def test_final_seal_rejects_new_run_attempt_after_ready(self):
        self.reject(TransitionClient(seal_run=lambda run: run.update(run_attempt=3)),
                    'workflow identity is foreign')

    def test_final_run_cannot_replace_pr_api_head_with_checkout_candidate(self):
        # The valid initial artifact metadata uses PR H; checkout/image receipts
        # use C. Coherently changing the fresh run to C is still a foreign view.
        self.reject(TransitionClient(seal_run=lambda run: run.update(head_sha=FIXTURES.C)),
                    'workflow identity is foreign')

    def test_final_seal_rejects_changed_workflow_after_ready(self):
        self.reject(TransitionClient(seal_run=lambda run: run.update(workflow_id=790)),
                    'workflow changed at final seal')

    def test_final_seal_rejects_regression_to_pending_post_after_success(self):
        def pending_again(rows):
            job = next(job for job in rows if job['name'] == 'images')
            job.update(status='in_progress', conclusion=None, completed_at=None)
            job['steps'][-1].update(status='in_progress', conclusion=None, completed_at=None)
        self.reject(TransitionClient(seal_jobs=pending_again), 'jobs changed at final seal')

    def test_final_seal_rejects_new_regular_suffix_after_ready(self):
        for name in ('Post-cache verification', 'Complete job plus verification'):
            with self.subTest(name=name):
                def changed(rows, name=name):
                    job = next(job for job in rows if job['name'] == 'images')
                    job['steps'].append(dict(name=name, status='completed', conclusion='success',
                                             completed_at=FIXTURES.END))
                self.reject(TransitionClient(seal_jobs=changed),
                            'regular producer step follows publication barrier')

    def test_final_seal_freezes_the_published_barrier_completion_time(self):
        # A POST may complete later, but the already-published barrier's own
        # completion time is immutable READY evidence, not a moving window.
        def changed(rows):
            job = next(job for job in rows if job['name'] == 'images')
            barrier = next(step for step in job['steps']
                           if step['name'] == MOD.BARRIERS['images'])
            barrier['completed_at'] = '2026-10-22T12:02:00Z'
        self.reject(TransitionClient(seal_jobs=changed), 'publication|jobs changed|barrier')


if __name__ == '__main__':
    unittest.main()
