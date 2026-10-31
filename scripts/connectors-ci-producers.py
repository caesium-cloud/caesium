#!/usr/bin/env python3
"""Wait for this CI attempt's producers and load their exact image artifacts.

This is a foreground child of connectors-test.sh, never a workflow scheduler.
Polling reads producer state; it does not retry a failed producer or scenario.
"""
import argparse
from datetime import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import time
import zipfile


PRODUCERS = ('builder', 'images', 'connector-fixture-images', 'ui-test')
BARRIERS = {'images': 'Publish native amd64 CLI',
            'connector-fixture-images': 'Publish connector fixture image identity'}
ARTIFACTS = {
    'product-amd64': ('images', ('product-amd64.tar.zst',)),
    'integration-runner-amd64': ('images', ('integration-runner-amd64.tar.zst',)),
    'connector-fixture-amd64': ('connector-fixture-images', ('connector-fixture-amd64.tar.zst',)),
    'image-ids-amd64': ('images', ('release.txt', 'test.txt', 'integration-runner.txt', 'candidate-sha.txt')),
    'connector-fixture-image-ids-amd64': ('connector-fixture-images', ('fixture.txt', 'candidate-sha.txt')),
}
SHA = re.compile(r'[0-9a-f]{40}')
IMAGE = re.compile(r'sha256:[0-9a-f]{64}')


class Invalid(RuntimeError):
    """A sanitized failure category, without API payloads or credentials."""


def require(condition, message):
    if not condition:
        raise Invalid(message)


def timestamp(value):
    require(isinstance(value, str) and re.fullmatch(r'\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,6})?Z', value),
            'producer timestamp is invalid')
    return datetime.fromisoformat(value.replace('Z', '+00:00'))


def positive(value):
    return type(value) is int and value > 0


def matches_fields(record, values):
    return isinstance(record, dict) and all(type(record.get(key)) is type(value) and record[key] == value
                                            for key, value in values.items())


def candidate_parents(candidate):
    try:
        raw = subprocess.check_output(['git', '--no-replace-objects', 'cat-file', 'commit', candidate],
                                      stderr=subprocess.DEVNULL, timeout=30,
                                      env={key: value for key, value in os.environ.items()
                                           if key not in ('GH_TOKEN', 'GITHUB_TOKEN')})
    except (OSError, subprocess.SubprocessError):
        raise Invalid('CI candidate commit is unreadable') from None
    # Checkout's depth-one boundary hides parents from revision traversal.
    # The literal commit object retains them without fetching parent objects;
    # only headers before the message/signature body establish this identity.
    header, separator, _message = raw.partition(b'\n\n')
    require(separator, 'CI candidate commit header is invalid')
    parents = []
    for line in header.split(b'\n'):
        if line.startswith(b'parent '):
            require(re.fullmatch(b'parent [0-9a-f]{40}', line), 'CI candidate parent header is invalid')
            parents.append(line[7:].decode('ascii'))
    return parents


def context(env):
    names = ('GITHUB_REPOSITORY', 'GITHUB_REPOSITORY_ID', 'GITHUB_RUN_ID', 'GITHUB_RUN_ATTEMPT',
             'GITHUB_WORKFLOW_REF', 'GITHUB_SHA', 'GITHUB_EVENT_NAME', 'GITHUB_EVENT_PATH',
             'CAESIUM_CONNECTORS_CI_HEAD_SHA', 'CANDIDATE_SHA')
    require(all(env.get(name) for name in names), 'CI producer context is incomplete')
    repo = env['GITHUB_REPOSITORY']
    require(re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', repo), 'CI repository is invalid')
    workflow_ref = env['GITHUB_WORKFLOW_REF']
    require(workflow_ref.startswith(repo + '/.github/workflows/ci.yml@'), 'CI workflow is foreign')
    ids = {}
    for key in ('GITHUB_REPOSITORY_ID', 'GITHUB_RUN_ID', 'GITHUB_RUN_ATTEMPT'):
        require(re.fullmatch(r'[1-9][0-9]*', env[key]), 'CI identity is invalid')
        ids[key] = int(env[key])
    candidate, head = env['CANDIDATE_SHA'], env['CAESIUM_CONNECTORS_CI_HEAD_SHA']
    require(type(candidate) is str and type(head) is str and SHA.fullmatch(candidate) and SHA.fullmatch(head)
            and env['GITHUB_SHA'] == candidate,
            'CI candidate identity is invalid')
    try:
        event = json.loads(Path(env['GITHUB_EVENT_PATH']).read_text())
    except (OSError, ValueError):
        raise Invalid('CI event is unreadable') from None
    require(isinstance(event, dict) and event.get('repository', {}).get('full_name') == repo
            and event['repository'].get('id') == ids['GITHUB_REPOSITORY_ID'], 'CI event repository is foreign')
    kind = env['GITHUB_EVENT_NAME']
    if kind == 'pull_request':
        pr = event.get('pull_request') or {}
        require(isinstance(pr, dict), 'CI pull request source is foreign')
        base, source = pr.get('base'), pr.get('head')
        require(isinstance(base, dict) and isinstance(source, dict)
                and type(base.get('sha')) is str and SHA.fullmatch(base['sha']) and source.get('sha') == head,
                'CI pull request source is foreign')
        # GitHub can regenerate the merge ref after recording the payload:
        # its base.sha and merge_commit_sha can lag the actual candidate.
        # As in ci-ok.py, the literal first parent is the tested base; the
        # second must still bind this exact candidate to the event's head.
        parents = candidate_parents(candidate)
        require(len(parents) == 2 and parents[0] != parents[1] and parents[1] == head,
                'CI pull request candidate parents are foreign')
    elif kind == 'push':
        require(event.get('after') == candidate and head == candidate, 'CI push source is foreign')
    elif kind == 'merge_group':
        require(event.get('merge_group', {}).get('head_sha') == candidate and head == candidate,
                'CI merge group source is foreign')
    elif kind == 'workflow_dispatch':
        require(head == candidate, 'CI dispatch source is foreign')
    else:
        raise Invalid('CI event is unsupported')
    return dict(repository=repo, repository_id=ids['GITHUB_REPOSITORY_ID'],
                run_id=ids['GITHUB_RUN_ID'], run_attempt=ids['GITHUB_RUN_ATTEMPT'],
                workflow_ref=workflow_ref, candidate_sha=candidate, head_sha=head, event=kind)


class Client:
    def __init__(self, identity, deadline):
        self.identity, self.deadline = identity, deadline

    def remaining(self, maximum=None):
        seconds = self.deadline - time.monotonic()
        require(seconds > 0, 'producer handoff wait expired')
        return min(seconds, maximum) if maximum else seconds

    def command(self, argv, *, output=None, api=False):
        # Credentials belong only to gh, never docker/zstd or their descendants.
        env = dict(os.environ)
        env.pop('GH_DEBUG', None)
        if not api:
            env.pop('GH_TOKEN', None)
            env.pop('GITHUB_TOKEN', None)
        try:
            result = subprocess.run(argv, stdout=output or subprocess.PIPE, stderr=subprocess.PIPE,
                                    env=env, timeout=self.remaining(30 if api and output is None else None), check=True)
        except (OSError, subprocess.SubprocessError):
            raise Invalid('producer API request failed' if api else 'producer image load failed') from None
        return result.stdout

    def get(self, endpoint):
        raw = self.command(['gh', 'api', '--allow-escape-sequences', '--hostname', 'github.com', endpoint], api=True)
        try:
            return json.loads(raw)
        except (ValueError, UnicodeError):
            raise Invalid('producer API response is invalid') from None

    def pages(self, endpoint, key):
        rows, ids = [], set()
        for page in range(1, 1001):
            data = self.get(endpoint + ('&' if '?' in endpoint else '?') + f'per_page=100&page={page}')
            require(isinstance(data, dict) and type(data.get('total_count')) is int and data['total_count'] >= 0
                    and isinstance(data.get(key), list), 'producer API pagination is invalid')
            batch = data[key]
            require(len(batch) <= 100, 'producer API pagination is invalid')
            for row in batch:
                require(isinstance(row, dict) and positive(row.get('id')) and row['id'] not in ids,
                        'producer API IDs are ambiguous')
                ids.add(row['id']); rows.append(row)
            if len(batch) < 100:
                require(len(rows) == data['total_count'], 'producer API pagination is incomplete')
                return rows
        raise Invalid('producer API pagination is excessive')


def validate_run(run, expected):
    require(matches_fields(run, dict(id=expected['run_id'], run_attempt=expected['run_attempt'],
                            head_sha=expected['head_sha'], event=expected['event']))
            and run.get('repository', {}).get('id') == expected['repository_id']
            and run['repository'].get('full_name') == expected['repository']
            and run.get('path') == '.github/workflows/ci.yml' and positive(run.get('workflow_id')),
            'producer workflow identity is foreign')
    require(run.get('status') in ('queued', 'in_progress', 'completed', 'waiting', 'pending'),
            'producer workflow status is invalid')
    require(run.get('status') != 'completed' or run.get('conclusion') == 'success',
            'producer workflow did not succeed')
    return run['workflow_id']


def validate_job(job, expected):
    require(isinstance(job, dict) and positive(job.get('id'))
            and matches_fields(job, {key: expected[key] for key in ('run_id', 'run_attempt', 'head_sha')}),
            'producer job source is foreign')
    status = job.get('status')
    if status == 'completed':
        require(job.get('conclusion') == 'success', 'producer job did not succeed: ' + str(job.get('name')))
        start, end = timestamp(job.get('started_at')), timestamp(job.get('completed_at'))
        require(start <= end, 'producer job time window is invalid')
    else:
        require(status in ('queued', 'in_progress', 'waiting', 'pending'), 'producer job status is invalid')
    return status


def read_jobs(client, expected):
    prefix = f"repos/{expected['repository']}/actions"
    rows = client.pages(prefix + f"/runs/{expected['run_id']}/attempts/{expected['run_attempt']}/jobs", 'jobs')
    for name in BARRIERS:
        indices = [index for index, row in enumerate(rows) if row.get('name') == name]
        require(len(indices) <= 1, 'producer job is ambiguous: ' + name)
        if not indices:
            continue
        index = indices[0]
        listed = rows[index]
        status = validate_job(listed, expected)
        # The jobs-list API can omit steps even after completion. Read the
        # exact job on every poll and final seal; never infer publication from
        # an empty list or carry old steps into a newer observation.
        detail = client.get(prefix + f"/jobs/{listed['id']}")
        require(matches_fields(detail, dict(id=listed['id'], name=name)),
                'producer job detail identity is foreign')
        current = validate_job(detail, expected)
        if status in ('in_progress', 'completed'):
            require(current in ('in_progress', 'completed'), 'producer job execution changed')
            require(detail.get('started_at') == listed.get('started_at'), 'producer job start changed')
        if status == 'completed':
            require(current == 'completed' and detail.get('completed_at') == listed.get('completed_at'),
                    'completed producer job changed')
        rows[index] = detail
    return select_jobs(rows, expected)


def select_jobs(rows, expected):
    selected, ready, finished = {}, True, True
    for name in PRODUCERS:
        matches = [row for row in rows if row.get('name') == name]
        require(len(matches) <= 1, 'producer job is ambiguous: ' + name)
        if not matches:
            # needs[builder] jobs are not created by GitHub until their
            # prerequisite completes. Missing-before-first-observation waits;
            # the handoff's pinned observation rejects later disappearance.
            ready, finished = False, False
            continue
        job = matches[0]
        status = validate_job(job, expected)
        if status != 'completed':
            finished = False
            if status != 'in_progress':
                ready = False
        if name not in BARRIERS:
            ready = ready and status == 'completed'
        else:
            steps = job.get('steps')
            require(isinstance(steps, list) and all(isinstance(step, dict) for step in steps),
                    'producer publication steps are invalid')
            # A successful publication barrier is the LAST regular step in the
            # tracked workflow. Only action POSTs and job finalization follow.
            matches = [i for i, step in enumerate(steps) if step.get('name') == BARRIERS[name]]
            require(len(matches) <= 1, 'producer publication barrier is ambiguous')
            for step in steps:
                if step.get('status') == 'completed':
                    require(step.get('conclusion') in ('success', 'skipped'), 'producer step did not succeed')
                else:
                    require(step.get('status') in ('queued', 'in_progress', 'pending'), 'producer step status is invalid')
            if not matches:
                require(status != 'completed', 'producer publication barrier is missing')
                ready = False
            else:
                index = matches[0]
                require(all(isinstance(step.get('name'), str) and
                            (step['name'].startswith('Post ') or step['name'] == 'Complete job')
                            for step in steps[index+1:]), 'regular producer step follows publication barrier')
                if steps[index].get('status') != 'completed':
                    ready = False
                else:
                    require(all(step.get('status') == 'completed' and step.get('conclusion') == 'success'
                                for step in steps[:index+1]), 'producer publication prefix did not succeed')
                    require(timestamp(job.get('started_at')) <= timestamp(steps[index].get('completed_at')),
                            'producer publication time window is invalid')
                    if status == 'completed':
                        require(timestamp(steps[index]['completed_at']) <= timestamp(job['completed_at']),
                                'producer publication exceeds job window')
        selected[name] = job
    return selected, ready, finished


def select_artifacts(rows, jobs, expected):
    selected = {}
    for name, (producer, _) in ARTIFACTS.items():
        barrier = next(step for step in jobs[producer]['steps'] if step['name'] == BARRIERS[producer])
        start, end = timestamp(jobs[producer]['started_at']), timestamp(barrier['completed_at'])
        current = []
        for row in rows:
            if row.get('name') != name:
                continue
            created = timestamp(row.get('created_at'))
            if created < start:
                continue  # A prior attempt never substitutes for this attempt.
            require(created <= end, 'producer artifact is outside its job window')
            current.append(row)
        require(len(current) == 1, 'producer artifact is missing or ambiguous: ' + name)
        row = current[0]
        run = row.get('workflow_run') or {}
        require(row.get('expired') is False and run.get('id') == expected['run_id']
                and run.get('head_sha') == expected['head_sha'] and run.get('repository_id') == expected['repository_id']
                and type(row.get('size_in_bytes')) is int and row['size_in_bytes'] > 0
                and isinstance(row.get('digest'), str) and IMAGE.fullmatch(row['digest']),
                'producer artifact identity is foreign or invalid')
        selected[name] = row
    return selected


def extract(archive, directory, names):
    directory.mkdir(mode=0o700)
    try:
        with zipfile.ZipFile(archive) as source:
            members = source.infolist()
            require(len(members) == len(names) and {member.filename for member in members} == set(names),
                    'producer artifact members are invalid')
            for member in members:
                mode = member.external_attr >> 16
                require(member.filename in names and not member.is_dir() and not member.flag_bits & 1
                        and not stat.S_ISLNK(mode) and (not stat.S_IFMT(mode) or stat.S_ISREG(mode)),
                        'producer artifact member is unsafe')
                if not member.filename.endswith('.tar.zst'):
                    require(member.file_size <= 128, 'producer receipt is excessive')
                else:
                    require(0 < member.file_size <= 16 * 1024**3, 'producer image archive is excessive or empty')
                with source.open(member) as incoming, (directory / member.filename).open('xb') as output:
                    while block := incoming.read(1024 * 1024):
                        output.write(block)
    except (OSError, zipfile.BadZipFile, RuntimeError) as error:
        if isinstance(error, Invalid): raise
        raise Invalid('producer artifact ZIP is invalid') from None


def write_record(output, name, record):
    raw = (json.dumps(record, sort_keys=True) + '\n').encode()
    temporary = output / (name + '.tmp')
    with temporary.open('xb') as target: target.write(raw)
    os.link(temporary, output / name); temporary.unlink()
    return hashlib.sha256(raw).hexdigest().encode() + b'\n'


def producer_proof(jobs):
    proof = {}
    for name, job in jobs.items():
        proof[name] = {key: job.get(key) for key in ('id', 'started_at', 'completed_at', 'status', 'conclusion')}
        if name in BARRIERS:
            proof[name]['barrier_completed_at'] = next(step['completed_at'] for step in job['steps']
                                                       if step['name'] == BARRIERS[name])
    return proof


def handoff(client, expected, output, invocation, ready_fd):
    prefix = f"repos/{expected['repository']}/actions/runs/{expected['run_id']}"
    workflow_id = None
    seen_ids, seen_started, seen_ended, seen_barriers = {}, {}, {}, {}
    def observe(rows):
        require(set(seen_ids) <= set(rows), 'previously observed producer job disappeared')
        for name, job in rows.items():
            require(seen_ids.get(name, job['id']) == job['id'], 'producer job identity changed')
            seen_ids[name] = job['id']
            if name in seen_started:
                require(job['status'] in ('in_progress', 'completed'), 'producer job execution changed')
                require(job.get('started_at') == seen_started[name], 'producer job start changed')
            elif job['status'] in ('in_progress', 'completed'):
                # A queued row can already expose started_at. Freeze the
                # execution window only once the job actually starts; its
                # identity remains pinned from the first queued observation.
                timestamp(job.get('started_at')); seen_started[name] = job['started_at']
            if name in seen_ended:
                require(job['status'] == 'completed' and job['conclusion'] == 'success'
                        and job['completed_at'] == seen_ended[name], 'completed producer job changed')
            elif job['status'] == 'completed':
                seen_ended[name] = job['completed_at']
            if name in BARRIERS:
                barrier = next((step for step in job['steps'] if step.get('name') == BARRIERS[name]), None)
                if name in seen_barriers:
                    require(barrier is not None and barrier.get('status') == 'completed' and barrier.get('conclusion') == 'success'
                            and barrier.get('completed_at') == seen_barriers[name], 'producer publication identity changed')
                elif barrier is not None and barrier.get('status') == 'completed' and barrier.get('conclusion') == 'success':
                    seen_barriers[name] = barrier['completed_at']
    while True:
        run = client.get(prefix + f"/attempts/{expected['run_attempt']}")
        observed = validate_run(run, expected)
        require(workflow_id in (None, observed), 'producer workflow changed')
        workflow_id = observed
        jobs, ready, finished = read_jobs(client, expected)
        observe(jobs)
        if ready:
            break
        time.sleep(min(5, client.remaining()))
    artifacts = select_artifacts(client.pages(prefix + '/artifacts', 'artifacts'), jobs, expected)
    for name, (_, names) in ARTIFACTS.items():
        row = artifacts[name]
        archive = output / (str(row['id']) + '.zip')
        with archive.open('xb') as target:
            client.command(['gh', 'api', '--allow-escape-sequences', '--hostname', 'github.com',
                            f"repos/{expected['repository']}/actions/artifacts/{row['id']}/zip"], output=target, api=True)
        with archive.open('rb') as source:
            digest = hashlib.file_digest(source, 'sha256').hexdigest()
        require('sha256:' + digest == row['digest'], 'producer artifact digest differs')
        extract(archive, output / name, names)
        archive.unlink()
    images = {}
    for role, name, file in (('server', 'image-ids-amd64', 'test.txt'),
                             ('runner', 'image-ids-amd64', 'integration-runner.txt'),
                             ('fixture', 'connector-fixture-image-ids-amd64', 'fixture.txt')):
        value = (output / name / file).read_text().strip()
        require(IMAGE.fullmatch(value), 'producer image receipt is invalid')
        images[role] = value
    require(IMAGE.fullmatch((output / 'image-ids-amd64/release.txt').read_text().strip()),
            'producer release receipt is invalid')
    require((output / 'connector-fixture-image-ids-amd64/candidate-sha.txt').read_text().strip() == expected['candidate_sha'],
            'fixture producer candidate is foreign')
    require((output / 'image-ids-amd64/candidate-sha.txt').read_text().strip() == expected['candidate_sha'],
            'product producer candidate is foreign')
    for name in ('product-amd64', 'integration-runner-amd64', 'connector-fixture-amd64'):
        client.command(['bash', '-o', 'pipefail', '-c', 'zstd -d -c "$1" | docker load', '--',
                        str(output / name / (name + '.tar.zst'))])
    # API metadata proves producer identity/status, while receipts and the
    # parent's actual-tag freeze bind checkout C and immutable runtime IDs.
    record = dict(version=1, invocation_id=invocation, status='artifact-ready',
                  **expected, workflow_id=workflow_id, images=images,
                  producers=producer_proof(jobs),
                  artifacts={name: {key: row[key] for key in ('id', 'digest', 'created_at')}
                             for name, row in artifacts.items()})
    digest = write_record(output, 'ready.json', record)
    os.write(ready_fd, digest); os.close(ready_fd)
    # Publication permits overlap with action cache exports, never a successful
    # final verdict. Stay owned/alive until the complete producer jobs succeed.
    while not finished:
        time.sleep(min(5, client.remaining()))
        require(validate_run(client.get(prefix + f"/attempts/{expected['run_attempt']}"), expected) == workflow_id,
                'producer workflow changed')
        jobs, ready, finished = read_jobs(client, expected)
        require(ready, 'producer publication readiness changed')
        observe(jobs)
    # Seal READY against fresh metadata. A deleted/replaced artifact, expired
    # retention, new attempt or changed producer cannot qualify the final lane.
    require(validate_run(client.get(prefix), expected) == workflow_id, 'producer workflow changed at final seal')
    jobs, ready, finished = read_jobs(client, expected)
    require(ready and finished, 'producer jobs changed at final seal')
    observe(jobs)
    sealed = select_artifacts(client.pages(prefix + '/artifacts', 'artifacts'), jobs, expected)
    require({name: tuple(row[key] for key in ('id', 'digest', 'created_at')) for name, row in sealed.items()} ==
            {name: tuple(row[key] for key in ('id', 'digest', 'created_at')) for name, row in artifacts.items()},
            'producer artifacts changed at final seal')
    record.update(status='success', exit_code=0,
                  producers=producer_proof(jobs))
    write_record(output, 'status.json', record)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', required=True, type=Path)
    parser.add_argument('--invocation', required=True)
    parser.add_argument('--ready-fd', required=True, type=int)
    args = parser.parse_args()
    require(re.fullmatch(r'[0-9a-f]{32}', args.invocation), 'producer invocation is invalid')
    require(not args.output.is_symlink() and args.output.is_dir() and not any(args.output.iterdir()),
            'producer handoff directory is not fresh')
    require(args.ready_fd > 2 and stat.S_ISFIFO(os.fstat(args.ready_fd).st_mode), 'producer readiness channel is invalid')
    os.umask(0o077)
    expected = context(os.environ)
    try:
        checkout = subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True, stderr=subprocess.DEVNULL, timeout=30,
                                           env={key: value for key, value in os.environ.items()
                                                if key not in ('GH_TOKEN', 'GITHUB_TOKEN')}).strip()
    except (OSError, subprocess.SubprocessError):
        raise Invalid('producer checkout is unreadable') from None
    require(checkout == expected['candidate_sha'],
            'producer checkout differs from candidate')
    # Thirty minutes matches the existing producer job cap, not the separate
    # 900-second promotion criterion. The owning job still caps total time.
    handoff(Client(expected, time.monotonic() + 1800), expected, args.output, args.invocation, args.ready_fd)


if __name__ == '__main__':
    try:
        main()
    except Invalid as error:
        raise SystemExit(str(error)) from None
