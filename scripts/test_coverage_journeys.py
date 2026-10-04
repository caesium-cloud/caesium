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
        for fault in (None, "native", "timestamp", "exit", "cause", "flush"):
            value = copy.deepcopy(original)
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
            if fault is None:
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(cli_list.read_text().splitlines(), ["shutdown-apply", "shutdown-start"])
            else:
                self.assertNotEqual(result.returncode, 0, fault)
                self.assertFalse(cli_list.exists(), fault)

    def test_network_rm_failure_retains_ledger(self):
        c = self.collector(); c.network_id, c.network_attempted = CID, True
        info = {'Id': CID, 'Labels': {b.LABEL_OWNER: OWNER, b.LABEL_RUN: 'owned'}}
        command, _ = self.command(info, fault='rm'); c.docker_run = command
        with self.assertRaises(b.JourneyError): c.cleanup()
        self.assertEqual(c.network_id, CID)
        self.assertFalse(c.cleanup_complete)

if __name__ == "__main__":
    unittest.main()
