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
import sys
import os
import re
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
JOURNEY_SHELL = ROOT / 'scripts/coverage-journeys.sh'
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

    def test_manifest_writer_failure_propagates_under_or_list_caller(self):
        fake_bin = self.tmp / 'bin'
        fake_bin.mkdir()
        fake_python = fake_bin / 'python3'
        fake_python.write_text('#!/bin/sh\nexit 37\n')
        fake_python.chmod(0o755)
        script = r'''set +e
source "$1"
coverage_journey_fail() { return 1; }
coverage_journey_write_manifest_required "$2/manifest.json" "$2" || exit 23
exit 91
'''
        env = os.environ.copy()
        env['PATH'] = str(fake_bin) + os.pathsep + env.get('PATH', '')
        result = subprocess.run(['bash', '-c', script, 'manifest-failure', str(JOURNEY_SHELL), str(self.tmp)],
                                capture_output=True, text=True, env=env, timeout=10)
        self.assertEqual(result.returncode, 23, result.stderr)

    def test_backend_consumer_requires_actual_complete_gate_receipt(self):
        valid = {
            'complete': True, 'selected_complete': True,
            'gate_mode': True, 'coverage_contribution': True,
        }
        self.assertTrue(b.complete_backend_gate(valid))
        for key in valid:
            with self.subTest(key=key):
                self.assertFalse(b.complete_backend_gate(dict(valid, **{key: False})))
        self.assertFalse(b.complete_backend_gate(None))
        self.assertFalse(b.complete_backend_gate([]))

    def test_sso_entrypoint_separates_refusal_from_sanitized_unexpected_traceback(self):
        argv = [
            'coverage-journeys.py', 'sso', '--root', str(ROOT),
            '--artifacts', str(self.tmp / 'artifacts'), '--raw', str(self.tmp / 'raw'),
            '--coverage-image', IMAGE, '--builder-image', IMAGE, '--platform', 'linux/amd64',
            '--candidate-sha', OWNER, '--run-id', 'owned', '--build-context', '{}',
            '--image-provenance', 'built-by-this-run', '--docker-socket', '/owned/socket',
            '--socket-gid', '0', '--backend-inputs', '/owned/inputs',
            '--backend-inputs-sha256', 'c' * 64, '--verified', 'true',
        ]

        class FakeCollector:
            failure = None
            def __init__(self, args):
                self.args = args
            def execute(self):
                raise self.failure
            def cleanup(self):
                pass

        with patch.object(b.sys, 'argv', argv), patch.object(b, 'Collector', FakeCollector), \
             patch.object(b.signal, 'signal'):
            FakeCollector.failure = b.JourneyError('SECRET_REFUSAL')
            with patch.object(b.sys, 'stderr', new=io.StringIO()) as refusal_stderr:
                self.assertEqual(b.main(), 1)
            self.assertEqual(refusal_stderr.getvalue(),
                             'SSO live coverage journey refused: JourneyError; raw diagnostics withheld\n')

            FakeCollector.failure = RuntimeError('SECRET_UNEXPECTED')
            with patch.object(b.sys, 'stderr', new=io.StringIO()) as unexpected_stderr:
                self.assertEqual(b.main(), 1)
            diagnostic = json.loads(unexpected_stderr.getvalue())
            self.assertEqual(diagnostic['error'], 'unexpected-sso-journey-exception')
            self.assertEqual(diagnostic['exception_type'], 'builtins.RuntimeError')
            self.assertEqual(diagnostic['traceback'][-1]['function'], 'execute')
            self.assertNotIn('SECRET', unexpected_stderr.getvalue())

    def test_configurable_lane_minimums_cannot_weaken_hard_floors(self):
        script = r'''set -e
source "$1"
unset CAESIUM_DISTRIBUTED_INTEGRATION_MIN_PASS CAESIUM_OWNER_MEMORY_INTEGRATION_MIN_PASS
[[ "$(coverage_journey_minimum_passes CAESIUM_DISTRIBUTED_INTEGRATION_MIN_PASS 20)" == 20 ]]
[[ "$(CAESIUM_DISTRIBUTED_INTEGRATION_MIN_PASS=1 coverage_journey_minimum_passes CAESIUM_DISTRIBUTED_INTEGRATION_MIN_PASS 20)" == 20 ]]
[[ "$(CAESIUM_DISTRIBUTED_INTEGRATION_MIN_PASS=00027 coverage_journey_minimum_passes CAESIUM_DISTRIBUTED_INTEGRATION_MIN_PASS 20)" == 27 ]]
[[ "$(CAESIUM_OWNER_MEMORY_INTEGRATION_MIN_PASS=2 coverage_journey_minimum_passes CAESIUM_OWNER_MEMORY_INTEGRATION_MIN_PASS 14)" == 14 ]]
if CAESIUM_OWNER_MEMORY_INTEGRATION_MIN_PASS=2x coverage_journey_minimum_passes CAESIUM_OWNER_MEMORY_INTEGRATION_MIN_PASS 14; then exit 71; fi
if CAESIUM_OWNER_MEMORY_INTEGRATION_MIN_PASS=9999999999 coverage_journey_minimum_passes CAESIUM_OWNER_MEMORY_INTEGRATION_MIN_PASS 14; then exit 72; fi
'''
        result = subprocess.run(['bash', '-c', script, 'minimum-floors', str(JOURNEY_SHELL)],
                                capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_parallel_lanes_overlap_join_fail_closed_and_preserve_parent_cleanup_ids(self):
        script = r'''set -euo pipefail
source "$1"
RAW="$2/raw"
ARTIFACTS="$2/artifacts"
AUDIT="$2/audit"
EVENTS="$2/events"
DONE="$2/done"
CLEANUP_CALLS="$2/cleanup-calls"
ID=run
CANDIDATE_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
IMAGE_ID=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
mkdir -p "$RAW/journeys" "$ARTIFACTS/journeys" "$AUDIT" "$DONE"
coverage_journey_log_redacted() { cat; }
coverage_journey_fail() { return 1; }
cleanup_coverage_journeys() { return 0; }
coverage_journey_resource() { printf '%s\n' "$*" >>"$CLEANUP_CALLS"; }
parallel_lane() {
  local lane="$1" peer="$2" result="$3" identity
  case "$lane" in
    local) identity=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ;;
    auth) identity=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb ;;
    distributed) identity=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc ;;
    owner-memory) identity=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd ;;
  esac
  coverage_journey_track_pending_name "test-$lane"
  coverage_journey_track_id "$identity"
  printf '%s\n' "$lane" >>"$EVENTS"
  for _ in $(seq 1 200); do
    grep -Fxq "$peer" "$EVENTS" && break
    sleep 0.01
  done
  if ! grep -Fxq "$peer" "$EVENTS"; then touch "$DONE/$lane"; return 41; fi
  mkdir -p "$RAW/journeys/$lane/cli" "$RAW/journeys/$lane/server"
  python3 - "$RAW/journeys/$lane/provenance.json" "$lane" "$CANDIDATE_SHA" "$IMAGE_ID" <<'PY'
import json, pathlib, sys
path, lane, candidate, image = sys.argv[1:]
pathlib.Path(path).write_text(json.dumps({
  'source':'integration-journey', 'lane':lane, 'complete':True,
  'candidate_sha':candidate, 'image_id':image,
  'raw':{'cli':'cli', 'server':'server'}}))
PY
  touch "$DONE/$lane"
  [[ "$result" == pass ]]
}
lane_local=(parallel_lane local auth pass)
lane_auth=(parallel_lane auth local pass)
coverage_journey_run_parallel_pair local-auth local ${#lane_local[@]} "${lane_local[@]}" auth ${#lane_auth[@]} "${lane_auth[@]}"
lane_distributed=(parallel_lane distributed owner-memory fail)
lane_owner=(parallel_lane owner-memory distributed pass)
if coverage_journey_run_parallel_pair distributed-owner distributed ${#lane_distributed[@]} "${lane_distributed[@]}" owner-memory ${#lane_owner[@]} "${lane_owner[@]}"; then
  exit 81
else
  failure_rc=$?
fi
coverage_journey_cleanup_parallel_resources
printf 'FAIL_RC=%s\nNAMES=%s\nCLEANUP_CALLS=%s\n' \
  "$failure_rc" "${COVERAGE_JOURNEY_NAMES[*]}" "$(wc -l <"$CLEANUP_CALLS" | awk '{print $1}')"
'''
        result = subprocess.run(
            ['bash', '-c', script, 'parallel-lanes', str(JOURNEY_SHELL), str(self.tmp)],
            capture_output=True, text=True, timeout=20,
        )
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        self.assertIn('FAIL_RC=1', result.stdout)
        self.assertIn('NAMES=local auth', result.stdout)
        self.assertIn('CLEANUP_CALLS=8', result.stdout)
        self.assertEqual({path.name for path in (self.tmp / 'done').iterdir()},
                         {'local', 'auth', 'distributed', 'owner-memory'})
        self.assertEqual((self.tmp / 'audit/parallel-lane-container-ids.txt').read_text().count('\n'), 4)
        self.assertEqual((self.tmp / 'audit/parallel-lane-container-names.txt').read_text().count('\n'), 4)

    def test_parallel_cancel_retries_delayed_runner_creation_before_join_and_cleans_worker_secret(self):
        fake_bin = self.tmp / 'bin'
        fake_bin.mkdir()
        fake_cli = fake_bin / 'fake_runner'
        fake_cli.write_text('#!/bin/sh\nprintf started >"$FAKE_RUNNER_LAUNCH_FILE"\nsleep 0.25\nprintf "%s\\n" "$$" >"$FAKE_RUNNER_PID_FILE"\nexec sleep 30\n')
        fake_cli.chmod(0o755)
        script = r'''set -euo pipefail
trap 'echo "parallel cancel shell failed at line $LINENO (rc=$?)" >&2' ERR
source "$1"
ID="$(printf 'a%.0s' {1..40})"
CANDIDATE_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
IMAGE_ID=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
BUILDER_RUN_IMAGE=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
full_runner_name="$ID-journey-runner-owner-memory"
AUDIT="$2/audit"
ARTIFACTS="$2/artifacts"
SECRET="$2/worker-secret"
CALLS="$2/removal-calls"
mkdir -p "$AUDIT" "$ARTIFACTS"
coverage_journey_enable_parallel_resource_tracking
coverage_journey_resource() {
  local action="$1" kind="$2" name="$3" image="$4"
  printf '%s\t%s\t%s\t%s\n' "$action" "$kind" "$name" "$image" >>"$CALLS"
  [[ "$action $kind $name" == "remove container $ID-journey-runner-owner-memory" ]] || return 0
  local pid
  [[ -s "$FAKE_RUNNER_PID_FILE" ]] || return 0
  pid="$(cat "$FAKE_RUNNER_PID_FILE")"
  kill -TERM "$pid" 2>/dev/null || true
}
blocked_lane() {
  local runner_name="$ID-journey-runner-owner-memory"
  coverage_journey_track_pending_name "$runner_name" "$BUILDER_RUN_IMAGE"
  COVERAGE_JOURNEY_SECRET_FILES+=("$SECRET")
  printf secret >"$SECRET"
  fake_runner run --name "$runner_name"
}
coverage_journey_run_parallel_worker blocked_lane >"$2/worker.log" 2>&1 &
worker=$!
COVERAGE_JOURNEY_WORKER_PIDS+=("$worker")
for _ in $(seq 1 200); do
  [[ -s "$FAKE_RUNNER_LAUNCH_FILE" ]] && break
  sleep 0.01
done
[[ -s "$FAKE_RUNNER_LAUNCH_FILE" ]]
if coverage_journey_cancel_parallel_workers; then exit 71; else cancel_rc=$?; fi
[[ "$cancel_rc" -eq 1 ]]
[[ ! -e "$SECRET" ]]
[[ "${#COVERAGE_JOURNEY_WORKER_PIDS[@]}" -eq 0 ]]
grep -F "$full_runner_name" "$CALLS" >/dev/null
grep -F "$BUILDER_RUN_IMAGE" "$CALLS" >/dev/null
attempts="$(awk -F '\t' -v name="$full_runner_name" '$1 == "remove" && $2 == "container" && $3 == name { n++ } END { print n + 0 }' "$CALLS")"
printf 'runner_name_length=%s\nremoval_attempts=%s\n' "${#full_runner_name}" "$attempts"
'''
        env = os.environ.copy()
        env['PATH'] = str(fake_bin) + os.pathsep + env.get('PATH', '')
        env['FAKE_RUNNER_PID_FILE'] = str(self.tmp / 'fake-runner.pid')
        env['FAKE_RUNNER_LAUNCH_FILE'] = str(self.tmp / 'fake-runner-launching')
        import time
        started = time.monotonic()
        result = subprocess.run(
            ['bash', '-c', script, 'parallel-cancel', str(JOURNEY_SHELL), str(self.tmp)],
            capture_output=True, text=True, env=env, timeout=5,
        )
        elapsed = time.monotonic() - started
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout + (self.tmp / 'worker.log').read_text() + (self.tmp / 'removal-calls').read_text())
        self.assertLess(elapsed, 2.0, f'foreground runner cancellation took {elapsed:.3f}s')
        self.assertIn('runner_name_length=68', result.stdout)
        attempts = int(re.search(r'removal_attempts=(\d+)', result.stdout).group(1))
        self.assertGreaterEqual(attempts, 2, result.stdout)

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

    def test_sso_fixture_mode_is_set_in_builder_and_host_refuses_unexecutable_output(self):
        # The mocked builder deliberately does not produce an accepted helper;
        # this control only proves that the host no longer chmods its output
        # and that non-executable/symlink outputs remain ineligible.
        for output_kind in ('non-executable', 'symlink'):
            with self.subTest(output_kind=output_kind):
                c = self.collector()
                c.fixture_dir = self.tmp / ('fixture-' + output_kind)
                c.binary_path = c.fixture_dir / 'sso-idp'
                calls = []
                original_chmod = Path.chmod
                host_binary_chmods = []

                def docker_run(*args, check=True, **kwargs):
                    calls.append(args)
                    if args[:2] == ('container', 'inspect'):
                        name = args[2]
                        return subprocess.CompletedProcess(
                            args, 1, '', 'Error: No such container: ' + name)
                    if args[0] == 'run':
                        if output_kind == 'symlink':
                            target = c.fixture_dir / 'target'
                            target.write_bytes(b'not-a-compiled-fixture')
                            original_chmod(target, 0o755)
                            c.binary_path.symlink_to(target.name)
                        else:
                            c.binary_path.write_bytes(b'not-a-compiled-fixture')
                        return subprocess.CompletedProcess(args, 0, '', '')
                    self.fail('unexpected fixture builder operation ' + repr(args))

                def deny_host_binary_chmod(path, mode, **kwargs):
                    if path == c.binary_path:
                        host_binary_chmods.append(mode)
                        raise PermissionError('simulated builder-owned output')
                    return original_chmod(path, mode, **kwargs)

                c.docker_run = docker_run
                with patch.object(Path, 'chmod', deny_host_binary_chmod):
                    with self.assertRaises(b.JourneyError):
                        c.build_fixture()

                self.assertEqual(host_binary_chmods, [])
                builder_args = next(args for args in calls if args[0] == 'run')
                script = builder_args[builder_args.index('-c') + 1]
                self.assertIn('go build -tags=integration -o /fixture/sso-idp ./test/fixtures/sso-idp', script)
                self.assertIn('chmod 0755 /fixture/sso-idp; test -x /fixture/sso-idp', script)
                self.assertEqual(builder_args[-4:-1], (IMAGE, 'sh', '-c'))
                self.assertIn('--pull=never', builder_args)
                self.assertIn('--platform', builder_args)
                self.assertEqual(builder_args[builder_args.index('--platform') + 1], c.platform)

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

    def test_owned_sso_bridge_supports_loopback_publication_and_keeps_identity_checks(self):
        c = self.collector()
        network_id = 'f' * 64
        calls = []

        def daemon(*args, check=True, **kwargs):
            calls.append(args)
            if args == ('network', 'inspect', c.network_name):
                return subprocess.CompletedProcess(args, 1, '',
                    'Error response from daemon: network ' + c.network_name + ' not found')
            if args[:2] == ('network', 'create'):
                # Docker's internal-only bridge drops host publications. Model
                # that protocol distinction, rather than accepting any create.
                daemon.published = '--internal' not in args
                return subprocess.CompletedProcess(args, 0, network_id, '')
            if args == ('network', 'inspect', network_id):
                return subprocess.CompletedProcess(args, 0, json.dumps([{
                    'Id': network_id, 'Labels': {b.LABEL_OWNER: OWNER, b.LABEL_RUN: c.run_id}}]), '')
            if args == ('port', CID, '8080/tcp'):
                if not daemon.published:
                    raise b.JourneyError('container command failed (1): port')
                return subprocess.CompletedProcess(args, 0, '127.0.0.1:49152\n', '')
            self.fail('unexpected daemon operation: ' + repr(args))

        c.docker_run = daemon
        c.create_network()
        self.assertEqual(c.network_id, network_id)
        self.assertEqual(c.host_api_base(CID), 'http://127.0.0.1:49152')
        create = next(args for args in calls if args[:2] == ('network', 'create'))
        self.assertIn(b.LABEL_OWNER + '=' + OWNER, create)
        self.assertIn(b.LABEL_RUN + '=' + c.run_id, create)
        self.assertEqual(create[-1], c.network_name)

        for field, invalid in (('Id', 'foreign'), ('Labels', {b.LABEL_OWNER: 'foreign', b.LABEL_RUN: c.run_id})):
            c = self.collector()
            valid = {'Id': network_id, 'Labels': {b.LABEL_OWNER: OWNER, b.LABEL_RUN: c.run_id}}
            valid[field] = invalid
            c.docker_run = lambda *args, **kwargs: subprocess.CompletedProcess(args, 1, '',
                'Error response from daemon: network ' + c.network_name + ' not found')
            c.docker_stdout = lambda *args: network_id if args[:2] == ('network', 'create') else json.dumps([valid])
            with self.assertRaises(b.JourneyError):
                c.create_network()

    def test_host_api_port_refuses_absence_wildcard_multiple_and_command_failure(self):
        c = self.collector()
        for mapping in ('', '0.0.0.0:49152', '[::]:49152', '127.0.0.1:49152\n127.0.0.1:49153'):
            with self.subTest(mapping=mapping):
                c.docker_stdout = lambda *args: mapping
                with self.assertRaises(b.JourneyError):
                    c.host_api_base(CID)
        with patch.object(c, 'docker_stdout', side_effect=b.JourneyError('container command failed (1): port')):
            with self.assertRaises(b.JourneyError):
                c.host_api_base(CID)

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
            command, state = self.command(info)
            with self.assertRaises(b.JourneyError): b.guarded_resource(command, 'container', CID, 'stop', OWNER, 'owned', image=IMAGE)
            self.assertEqual(state['mutations'], [
                ('container', 'kill', '--signal=SIGUSR2', CID),
                ('container', 'stop', '-t', '60', CID),
            ])
        command, _ = self.command()
        result = b.guarded_resource(command, 'container', CID, 'stop', OWNER, 'owned', image=IMAGE)
        self.assertEqual(result['flush_rc'], 0)

    def connector_fixture(self, health, fault=None):
        audit = self.tmp / ("audit-" + str(len(list(self.tmp.iterdir()))))
        audit.mkdir()
        info = self.info()
        info["Config"].update(Env=["TOKEN=never-persist-env"])
        info["Config"]["Labels"][b.LABEL_LANE] = "base-connectors"
        info["State"].update(Running=fault != "stopped", Error="never-persist-native-error")
        calls, polls = [], []
        logs = json.dumps({"msg": "connector config loaded", "fingerprint": "a" * 64, "level": "info", "ts": "2026-10-05T12:00:00Z",
                           "error": "csk_neverpersist", "token": "never-persist-token"}) + "\n" + json.dumps(
               {"msg": "Bearer never-persist-bearer", "jwt": "eyJheader.eyJpayload.signature",
                "cookie": "never-persist-cookie", "assertion": "never-persist-assertion"})
        def command(*args, check=True, **kwargs):
            calls.append(args)
            rc, stdout = 0, ""
            if args[1] == "inspect":
                if fault == "foreign": info["Config"]["Labels"][b.LABEL_OWNER] = "foreign"
                if fault == "image": info["Image"] = INDEX
                if fault == "identity": info["Id"] = 'f' * 64
                stdout = json.dumps([info])
            elif args[1] == "logs":
                raw = logs if fault != "no-fingerprint" else json.dumps({"msg": "not loaded: connector config loaded"})
                rc = 17 if fault == "logs-failed" else 0
                stdout = json.dumps(b.connector_log_records((("stdout", io.BytesIO(raw.encode())),)))
                if rc:
                    stdout = json.dumps({"records": [], "loaded_event": None, "counts": {"failed_log_streams_omitted": 2}})
            elif args[1] == "exec":
                self.assertEqual(args, ("container", "exec", CID, "wget", "-q", "-T", "2", "-O", "-",
                                        "http://127.0.0.1:8080/health"))
                self.assertEqual(kwargs["timeout"], 5)
                polls.append(args)
                rc, stdout = health[min(len(polls) - 1, len(health) - 1)]
            elif args[1] == "kill":
                self.assertTrue(polls, "fingerprint alone must not cause a signal")
                self.assertEqual(health[min(len(polls) - 1, len(health) - 1)], (0, '{"status":"healthy"}'))
                rc = 1 if fault == "flush" else 0
            elif args[1] == "stop":
                info["State"].update(Running=False, ExitCode=17 if fault == "exit" else 0)
                rc = 1 if fault == "stop" else 0
            else:
                self.fail("unexpected connector operation " + repr(args))
            return subprocess.CompletedProcess(args, rc, stdout, "never-persist-native-stderr")
        return command, audit, calls

    def test_connector_waits_for_complete_exact_health_before_signal_and_retains_safe_states(self):
        command, audit, calls = self.connector_fixture([(0, '{"status":"unavailable"}'), (0, '{"status":"healthy"}')])
        result = b.connector_shutdown(command, CID, OWNER, "owned", IMAGE, audit)
        self.assertEqual(result["flush_rc"], 0)
        signals = [i for i, args in enumerate(calls) if args[1] == "kill"]
        self.assertEqual(len(signals), 1)
        self.assertEqual(len([args for args in calls[:signals[0]] if args[1] == "exec"]), 2)
        raw = (audit / 'connector-diagnostics.json').read_text()
        record = json.loads(raw)
        self.assertTrue(record['complete'] and record['fingerprint'] and record['healthy'])
        self.assertEqual(record['polls'], 2)
        self.assertTrue(record['pre_stop']['State']['Running'])
        self.assertFalse(record['post_stop']['State']['Running'])
        self.assertEqual(record['refusal_category'], 'none')
        self.assertEqual(record['post_log_rc'], 0)
        for secret in ('never-persist', 'eyJheader', 'TOKEN=', 'Bearer', 'Config', 'Env', 'Error'):
            self.assertNotIn(secret, raw)
        self.assertIn('connector config loaded', raw)

    def test_connector_never_ready_stopped_and_invalid_complete_json_do_not_signal(self):
        for rc, body in ((0, '{"status":"unhealthy"}'), (0, '{"healthy":true}'),
                         (0, '{"msg":"healthy"}'), (0, '{"status":"healthy"} trailing'),
                         (0, '{"status":"healthy"'), (1, '{"status":"healthy"}')):
            with self.subTest(rc=rc, body=body):
                command, audit, calls = self.connector_fixture([(rc, body)])
                with self.assertRaises(b.JourneyError):
                    b.connector_shutdown(command, CID, OWNER, "owned", IMAGE, audit)
                self.assertFalse(any(args[1] in ('kill', 'stop') for args in calls))
                record = json.loads((audit / 'connector-diagnostics.json').read_text())
                self.assertFalse(record['complete'])
                self.assertEqual(record['refusal_category'], 'never_ready')
                self.assertEqual(record['polls'], 60)
        for fault in ('stopped', 'no-fingerprint', 'foreign', 'image', 'identity', 'logs-failed'):
            with self.subTest(fault=fault):
                command, audit, calls = self.connector_fixture([(0, '{"status":"healthy"}')], fault)
                with self.assertRaises(b.JourneyError):
                    b.connector_shutdown(command, CID, OWNER, "owned", IMAGE, audit)
                self.assertFalse(any(args[1] in ('kill', 'stop') for args in calls))
                record = json.loads((audit / 'connector-diagnostics.json').read_text())
                self.assertFalse(record['complete'])
                self.assertEqual(record['polls'], 60 if fault in ('no-fingerprint', 'logs-failed') else 1)
                if fault == 'logs-failed':
                    self.assertEqual(record['log_rc'], 17)
                    self.assertEqual(record['log_counts']['failed_log_streams_omitted'], 2)
                    self.assertFalse(record['fingerprint'])

    def test_connector_failed_stop_retains_refusal_without_native_secret_or_eligibility(self):
        for fault, category in (('flush', 'flush_failed'), ('stop', 'stop_failed'), ('exit', 'nonzero_exit')):
            with self.subTest(fault=fault):
                command, audit, _ = self.connector_fixture([(0, '{"status":"healthy"}')], fault)
                with self.assertRaises(b.JourneyError):
                    b.connector_shutdown(command, CID, OWNER, "owned", IMAGE, audit)
                raw = (audit / 'connector-diagnostics.json').read_text()
                record = json.loads(raw)
                self.assertFalse(record['complete'])
                self.assertEqual(record['refusal_category'], category)
                self.assertIn('post_stop', record)
                self.assertNotIn('never-persist', raw)
        command, audit, _ = self.connector_fixture([(0, '{"status":"healthy"}')])
        (audit / 'connector-diagnostics.json').write_text('existing evidence')
        with self.assertRaises(FileExistsError):
            b.connector_shutdown(command, CID, OWNER, "owned", IMAGE, audit)
        self.assertEqual((audit / 'connector-diagnostics.json').read_text(), 'existing evidence')

    def test_connector_unknown_errors_logs_and_output_caps_do_not_leak_or_pass(self):
        command, audit, calls = self.connector_fixture([(0, '{"status":"healthy"}')])
        def interrupted(*args, **kwargs):
            if args[1] == 'exec':
                raise RuntimeError('never-persist-native-error csk_neverpersist')
            return command(*args, **kwargs)
        with self.assertRaises(RuntimeError):
            b.connector_shutdown(interrupted, CID, OWNER, 'owned', IMAGE, audit)
        raw = (audit / 'connector-diagnostics.json').read_text()
        self.assertNotIn('never-persist', raw)
        self.assertEqual(json.loads(raw)['exception_type'], 'UnexpectedError')
        self.assertFalse(any(args[1] in ('kill', 'stop') for args in calls))
        logs = b.connector_log_records((("stderr", io.BytesIO((json.dumps({'msg': ['csk_secret'], 'caller': 'csk_secret.go:1',
            'error': 'Bearer private', 'Env': ['TOKEN=private']}) + '\nprivate raw line').encode())),))["records"]
        self.assertNotIn('private', json.dumps(logs))
        self.assertNotIn('csk_secret', json.dumps(logs))
        self.assertEqual(logs[-1]['msg'], '[unstructured log omitted]')
        def process(args, stdout, stderr, **kwargs):
            self.assertEqual(kwargs['timeout'], 10)
            stdout.write(b'x' * (2 * 1024 * 1024 + 1))
            stderr.write(b'never-persist-stderr')
            return subprocess.CompletedProcess(args, 0)
        with patch.object(b.subprocess, 'run', side_effect=process):
            with self.assertRaises(b.JourneyError):
                b.connector_command('container', 'exec', CID, timeout=10)

    def test_connector_nonlog_cap_keeps_static_reason_and_numeric_bound(self):
        command, audit, calls = self.connector_fixture([(0, '{"status":"healthy"}')])
        def oversized(*args, **kwargs):
            if args[1] == 'exec':
                raise b.ConnectorOutputLimit(2 * 1024 * 1024, 2 * 1024 * 1024 + 1)
            return command(*args, **kwargs)
        with self.assertRaises(b.ConnectorOutputLimit):
            b.connector_shutdown(oversized, CID, OWNER, 'owned', IMAGE, audit)
        record = json.loads((audit / 'connector-diagnostics.json').read_text())
        self.assertFalse(record['complete'])
        self.assertEqual(record['refusal_category'], 'nonlog_output_cap')
        self.assertEqual(record['output_limit'], 2 * 1024 * 1024)
        self.assertEqual(record['observed_output_bytes_at_least'], 2 * 1024 * 1024 + 1)
        self.assertFalse(any(args[1] in ('kill', 'stop') for args in calls))

    def actual_connector_log_process(self, program):
        run = subprocess.run
        def child(argv, **kwargs):
            self.assertEqual(argv, ['docker', 'container', 'logs', '--tail', 'all', CID])
            self.assertEqual(kwargs['timeout'], 10)
            self.assertFalse(kwargs['check'])
            # Real child OS stdout/stderr and actual unlinked temp-file spools;
            # no Docker process, application execution or live coverage claim.
            return run([sys.executable, '-c', program], **kwargs)
        with patch.object(b.subprocess, 'run', side_effect=child):
            return b.connector_command('container', 'logs', '--tail', 'all', CID, check=False, timeout=10)

    def test_connector_large_startup_keeps_loaded_proof_apart_from_last100(self):
        result = self.actual_connector_log_process("""import json,sys
print(json.dumps({'msg':'connector config loaded','fingerprint':'a'*64,'token':'never-persist-token'}))
for _ in range(1400): print(json.dumps({'msg':'never-persist-startup'+'x'*250,'error':'csk_neverpersist'}))
print(json.dumps({'msg':'server started'}),file=sys.stderr)
""")
        evidence = json.loads(result.stdout)
        self.assertEqual(result.returncode, 0)
        self.assertGreater(evidence['counts']['stdout_bytes'], 256 * 1024)
        self.assertEqual(evidence['counts']['stdout_lines'], 1401)
        self.assertEqual(evidence['counts']['stderr_lines'], 1)
        self.assertEqual(len(evidence['records']), 100)
        self.assertEqual(evidence['loaded_event']['msg'], 'connector config loaded')
        self.assertEqual(evidence['loaded_event']['stream'], 'stdout')
        self.assertEqual(evidence['loaded_event']['fingerprint'], 'a' * 64)
        self.assertFalse(any(r['msg'] == 'connector config loaded' for r in evidence['records']))
        self.assertLess(len(result.stdout.encode()), 40 * 1024)
        self.assertEqual(result.stderr, '')
        self.assertNotIn('never-persist', result.stdout)
        self.assertNotIn('csk_neverpersist', result.stdout)

    def test_connector_stderr_event_is_real_proof_only_for_successful_log_command(self):
        program = """import json,sys
print('never-persist-stdout')
print(json.dumps({'msg':'connector config loaded','fingerprint':'a'*64,'error':'never-persist-error'}),file=sys.stderr)
for _ in range(1100): print(json.dumps({'msg':'Bearer never-persist-secret'}),file=sys.stderr)
"""
        result = self.actual_connector_log_process(program)
        evidence = json.loads(result.stdout)
        self.assertEqual(evidence['loaded_event']['stream'], 'stderr')
        self.assertEqual(evidence['counts']['stderr_lines'], 1101)
        self.assertEqual(len(evidence['records']), 100)
        self.assertNotIn('never-persist', result.stdout)
        failed = self.actual_connector_log_process(program + '\nsys.exit(17)')
        self.assertEqual(failed.returncode, 17)
        failure = json.loads(failed.stdout)
        self.assertIsNone(failure['loaded_event'])
        self.assertEqual(failure['records'], [])
        self.assertEqual(failure['counts']['failed_log_streams_omitted'], 2)
        self.assertNotIn('connector config loaded', failed.stdout)
        self.assertNotIn('never-persist', failed.stdout)

    def test_connector_real_successful_logs_without_valid_fingerprint_never_signal(self):
        for channel in ('stdout', 'stderr'):
            for fingerprint in ('missing', '', 'a' * 63, 'g' * 64, 123, ['a' * 64]):
                with self.subTest(channel=channel, fingerprint=fingerprint):
                    event = {'msg': 'connector config loaded'}
                    if fingerprint != 'missing':
                        event['fingerprint'] = fingerprint
                    program = "import json,sys\nprint(" + repr(json.dumps(event)) + ",file=sys." + channel + ")"
                    result = self.actual_connector_log_process(program)
                    evidence = json.loads(result.stdout)
                    self.assertEqual(result.returncode, 0)
                    self.assertIsNone(evidence['loaded_event'])
                    command, audit, calls = self.connector_fixture([(0, '{"status":"healthy"}')])
                    def invalid_logs(*args, **kwargs):
                        if args[1] == 'logs':
                            return result
                        return command(*args, **kwargs)
                    with self.assertRaises(b.JourneyError):
                        b.connector_shutdown(invalid_logs, CID, OWNER, 'owned', IMAGE, audit)
                    record = json.loads((audit / 'connector-diagnostics.json').read_text())
                    self.assertFalse(record['complete'] or record['fingerprint'])
                    self.assertTrue(record['healthy'])
                    self.assertEqual(record['refusal_category'], 'never_ready')
                    self.assertEqual(record['polls'], 60)
                    self.assertFalse(any(args[1] in ('kill', 'stop') for args in calls))

    def test_connector_stream_line_bound_cannot_promote_oversized_suffix_or_invalid_utf8(self):
        class BoundedStream(io.BytesIO):
            def read(self, *args):
                raise AssertionError('whole-stream reads are forbidden')
            def readline(self, size=-1):
                self.assert_bound(size)
                return super().readline(size)
            def assert_bound(self, size):
                if size != 64 * 1024 + 1:
                    raise AssertionError('unbounded line read')
        data = (b'x' * (2 * 1024 * 1024) + b'{"msg":"connector config loaded"}\n'
                + b'\xff{"msg":"connector config loaded"}\n'
                + b'{"msg":"server started"}\n')
        evidence = b.connector_log_records((("stdout", BoundedStream(data)),))
        self.assertIsNone(evidence['loaded_event'])
        self.assertEqual(evidence['counts']['oversized_lines'], 1)
        self.assertEqual(evidence['counts']['invalid_utf8_lines'], 1)
        self.assertEqual(evidence['counts']['stdout_bytes'], len(data))
        self.assertEqual(evidence['counts']['stdout_lines'], 3)
        self.assertLess(len(json.dumps(evidence)), 1024)

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
        c.shutdown_control_dir = self.tmp / 'native-shutdown-control'
        c.shutdown_control_dir.mkdir()
        c.shutdown_control_mount_source_sha256 = hashlib.sha256(str(c.shutdown_control_dir.resolve()).encode()).hexdigest()
        info = {'Id': CID, 'Image': INDEX, 'Name': '/' + TASK + '-' + RUN, 'RestartCount': 0,
          'Config': {'Image': 'alpine:3.23', 'Cmd': ['sh', '-c', c.shutdown_command()]},
          'Mounts': [{'Type': 'bind', 'Source': str(c.shutdown_control_dir.resolve()),
                      'Destination': '/caesium-shutdown-control', 'Mode': '', 'RW': False}],
          'State': {'Running': True, 'Restarting': False, 'OOMKilled': False}}
        c.inspect_container = lambda *args: info
        native = c.native_running()
        self.assertTrue(native['running'])
        self.assertEqual(native['control_mount_source_sha256'], c.shutdown_control_mount_source_sha256)
        self.assertTrue(native['control_mount_read_only'])
        for field, value in (('Running', False), ('OOMKilled', True), ('Restarting', True)):
            info['State'][field] = value
            with self.assertRaises(b.JourneyError): c.native_running()
            info['State'][field] = {'Running': True, 'OOMKilled': False, 'Restarting': False}[field]
        expected_mount = copy.deepcopy(info['Mounts'][0])
        for mutation in (
            lambda mount: mount.update(RW=True, Mode='rw'),
            lambda mount: mount.update(Source=str(self.tmp / 'foreign-control')),
            lambda mount: mount.update(Destination='/wrong-target'),
            lambda mount: mount.update(Type='volume'),
            lambda mount: mount.pop('RW'),
        ):
            info['Mounts'][0] = copy.deepcopy(expected_mount)
            mutation(info['Mounts'][0])
            with self.assertRaises(b.JourneyError): c.native_running()
        info['Mounts'][0] = expected_mount

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

    def test_same_durable_rows_are_retained_then_explicitly_resumed_to_success(self):
        c = self.collector()
        c.task_image_id, c.task_docker_image_id, c.task_image_ref = CONFIG, INDEX, 'alpine:3.23'
        c.shutdown_control_dir = self.tmp / 'shutdown-control'
        c.shutdown_control_dir.mkdir()
        c.shutdown_release_path = c.shutdown_control_dir / 'release'
        c.shutdown_control_mount_source_sha256 = hashlib.sha256(str(c.shutdown_control_dir.resolve()).encode()).hexdigest()
        c.shutdown_job = {'job_id': RUN, 'run_id': RUN, 'task_id': TASK, 'task_run_id': TASK, 'runtime_id': CID,
                          'attempt_before_signal': 1,
                          'resumption_mode': 'explicit-public-http-trigger-existing-run',
                          'resumption_path': '/hooks/coverage-shutdown-owned',
                          'control_mount_source_sha256': c.shutdown_control_mount_source_sha256}
        c.run_id = 'owned'
        c.native_runtime_absent_generations = [1]
        removed = []
        c.verify_runtime_absent = lambda generation, runtime_id=None: removed.append((generation, runtime_id))
        c.server_generations = [{'finished_at': '2026-10-04T12:00:02Z'}]
        new_runtime = 'f' * 64
        retained_run = {'id': RUN, 'status': 'running', 'tasks': [{'task_id': TASK, 'status': 'running', 'runtime_id': CID}]}
        retained_row = {'task_run_id': TASK, 'runtime_id': CID, 'status': 'running', 'attempt': 1}
        delayed_dispatch_run = copy.deepcopy(retained_run)
        delayed_dispatch_row = copy.deepcopy(retained_row)
        split_snapshot_run = copy.deepcopy(retained_run)
        running_run = {'id': RUN, 'status': 'running', 'tasks': [{'task_id': TASK, 'status': 'running', 'runtime_id': new_runtime}]}
        running_row = {'task_run_id': TASK, 'runtime_id': new_runtime, 'status': 'running', 'attempt': 1,
                       'started_at': '2026-10-07T12:00:03Z'}
        final_run = {'id': RUN, 'status': 'succeeded', 'completed_at': '2030-01-01T00:00:05Z',
                     'tasks': [{'task_id': TASK, 'status': 'succeeded', 'runtime_id': new_runtime,
                                'output': {'shutdown': 'resumed-owned'}}]}
        final_row = {'task_run_id': TASK, 'runtime_id': new_runtime, 'status': 'succeeded',
                     'attempt': 1, 'started_at': '2026-10-07T12:00:03Z', 'completed_at': '2030-01-01T00:00:05Z'}
        records = [retained_run, delayed_dispatch_run, split_snapshot_run, running_run, final_run]
        partitions = [retained_row, delayed_dispatch_row, running_row, running_row, final_row]
        def api_json(_, path):
            if '/partitions' in path:
                return {'total': 1, 'partitions': [partitions.pop(0)]}
            return records.pop(0)
        c.api_json = api_json
        order = []
        def native_running(runtime_id=None):
            self.assertTrue(c.shutdown_release_marker_absent(), 'release must remain held during native runtime proof')
            self.assertEqual(runtime_id, new_runtime)
            order.append('native-running')
            return {'runtime_id': new_runtime, 'run_id': RUN, 'task_id': TASK,
                    'docker_image_id': INDEX, 'task_config_id': CONFIG,
                    'command_sha256': hashlib.sha256(c.shutdown_command().encode()).hexdigest(),
                    'control_mount_source_sha256': c.shutdown_control_mount_source_sha256,
                    'control_mount_destination': '/caesium-shutdown-control', 'control_mount_read_only': True,
                    'running': True}
        c.native_running = native_running
        c.fire_shutdown_webhook = lambda _: {'status': 202, 'path': c.shutdown_job['resumption_path'],
                                              'http_triggers_accepted': 1, 'http_runs_started': 1,
                                              'receipt_id': '12345678-1234-4234-8234-123456789abc'}
        original_fire = c.fire_shutdown_webhook
        def fire(_):
            order.append('webhook-accepted')
            return original_fire(_)
        c.fire_shutdown_webhook = fire
        original_release = c.release_shutdown_task
        def release():
            self.assertEqual(order[-1], 'native-running')
            order.append('release')
            return original_release()
        c.release_shutdown_task = release
        c.verify_shutdown_job_explicitly_resumed('owned-server')
        self.assertEqual(order, ['webhook-accepted', 'native-running', 'release'])
        self.assertIs(c.shutdown_job['generation2_automatic_takeover'], False)
        self.assertEqual(c.shutdown_job['retained_task_run_id'], TASK)
        self.assertEqual(c.shutdown_job['retained_attempt'], 1)
        self.assertEqual(c.shutdown_job['retained_runtime_id'], CID)
        self.assertEqual(c.shutdown_job['final_task_run_id'], TASK)
        self.assertEqual(c.shutdown_job['final_attempt'], 1)
        self.assertEqual(c.shutdown_job['final_runtime_id'], new_runtime)
        self.assertEqual(removed, [(2, new_runtime)])
        self.assertEqual(c.shutdown_job['resumption_output'], {'shutdown': 'resumed-owned'})
        self.assertTrue(c.shutdown_job['release_marker_after_running_observation'])
        self.assertTrue(c.shutdown_job['resumption_runtime_absent_after_completion'])

    def test_release_marker_is_one_shot_and_uses_expected_bytes(self):
        c = self.collector()
        c.task_image_id, c.task_docker_image_id, c.task_image_ref = CONFIG, INDEX, 'alpine:3.23'
        c.run_id = 'owned'
        c.shutdown_control_dir = self.tmp / 'shutdown-control'
        c.shutdown_control_dir.mkdir()
        c.shutdown_release_path = c.shutdown_control_dir / 'release'
        c.shutdown_control_mount_source_sha256 = hashlib.sha256(str(c.shutdown_control_dir.resolve()).encode()).hexdigest()
        c.shutdown_job = {'runtime_id': CID, 'run_id': RUN, 'task_id': TASK, 'task_run_id': TASK,
                          'attempt_before_signal': 1,
                          'control_mount_source_sha256': c.shutdown_control_mount_source_sha256}
        with self.assertRaises(b.JourneyError):
            c.release_shutdown_task()
        c.server_generations = [{'finished_at': '2026-10-04T12:00:02Z'}]
        observed_at = b.datetime.datetime.now(b.datetime.timezone.utc).isoformat().replace('+00:00', 'Z')
        c.shutdown_job['resumption_webhook'] = {'status': 202}
        c.shutdown_job['resumption_runtime_running'] = {
            'runtime_id': 'f' * 64, 'task_run_id': TASK, 'attempt': 1,
            'task_started_at': '2026-10-07T12:00:03Z', 'observed_at': observed_at,
            'release_marker_absent': True, 'running': True,
            'native': {'runtime_id': 'f' * 64, 'run_id': RUN, 'task_id': TASK,
                       'docker_image_id': INDEX, 'task_config_id': CONFIG,
                       'command_sha256': hashlib.sha256(c.shutdown_command().encode()).hexdigest(),
                       'control_mount_source_sha256': c.shutdown_control_mount_source_sha256,
                       'control_mount_destination': '/caesium-shutdown-control', 'control_mount_read_only': True,
                       'running': True},
        }
        self.assertTrue(c.shutdown_release_marker_absent())
        marker = c.release_shutdown_task()
        self.assertFalse(c.shutdown_release_marker_absent())
        self.assertEqual(c.shutdown_release_path.read_bytes(), b'release-v1\n')
        self.assertEqual(marker['sha256'], hashlib.sha256(b'release-v1\n').hexdigest())
        with self.assertRaises(b.JourneyError):
            c.release_shutdown_task()

    def test_resumption_refuses_changed_ids_same_runtime_failed_or_pre_shutdown_completion(self):
        for fault in ('run-id', 'task-id', 'task-run-id', 'same-runtime', 'failed', 'early', 'retained-attempt', 'final-attempt', 'missing-attempt', 'wrong-output', 'runtime-changed-after-observation', 'split-runtime-change'):
            with self.subTest(fault=fault):
                c = self.collector()
                c.task_image_id, c.task_docker_image_id, c.task_image_ref = CONFIG, INDEX, 'alpine:3.23'
                c.shutdown_control_dir = self.tmp / ('shutdown-control-' + fault)
                c.shutdown_control_dir.mkdir()
                c.shutdown_release_path = c.shutdown_control_dir / 'release'
                c.shutdown_control_mount_source_sha256 = hashlib.sha256(str(c.shutdown_control_dir.resolve()).encode()).hexdigest()
                c.shutdown_job = {'job_id': RUN, 'run_id': RUN, 'task_id': TASK, 'task_run_id': TASK, 'runtime_id': CID,
                                  'attempt_before_signal': 1,
                                  'resumption_mode': 'explicit-public-http-trigger-existing-run',
                                  'resumption_path': '/hooks/coverage-shutdown-owned',
                                  'control_mount_source_sha256': c.shutdown_control_mount_source_sha256}
                c.run_id = 'owned'
                c.native_runtime_absent_generations = [1]
                c.verify_runtime_absent = lambda generation, runtime_id=None: None
                c.server_generations = [{'finished_at': '2026-10-04T12:00:02Z'}]
                new_runtime = CID if fault == 'same-runtime' else 'f' * 64
                retained_run = {'id': RUN, 'status': 'running', 'tasks': [{'task_id': TASK, 'status': 'running', 'runtime_id': CID}]}
                retained_row = {'task_run_id': TASK, 'runtime_id': CID, 'status': 'running',
                                'attempt': 2 if fault == 'retained-attempt' else 1}
                delayed_dispatch_run = copy.deepcopy(retained_run)
                delayed_dispatch_row = copy.deepcopy(retained_row)
                observed_runtime = new_runtime
                final_runtime = 'e' * 64 if fault == 'runtime-changed-after-observation' else new_runtime
                running_run = {'id': RUN, 'status': 'running', 'tasks': [{'task_id': TASK, 'status': 'running', 'runtime_id': observed_runtime}]}
                running_row = {'task_run_id': TASK, 'runtime_id': observed_runtime, 'status': 'running',
                               'attempt': 1, 'started_at': '2026-10-07T12:00:03Z'}
                split_snapshot_run = copy.deepcopy(retained_run)
                split_snapshot_row = copy.deepcopy(running_row)
                if fault == 'split-runtime-change':
                    split_snapshot_row['runtime_id'] = '0' * 64
                run_id = '99999999-9999-4999-8999-999999999999' if fault == 'run-id' else RUN
                task_id = '88888888-8888-4888-8888-888888888888' if fault == 'task-id' else TASK
                task_run_id = '77777777-7777-4777-8777-777777777777' if fault == 'task-run-id' else TASK
                completion = '2026-10-04T12:00:01Z' if fault == 'early' else '2030-01-01T00:00:05Z'
                status = 'failed' if fault == 'failed' else 'succeeded'
                final_run = {'id': run_id, 'status': status, 'completed_at': completion,
                             'tasks': [{'task_id': task_id, 'status': status, 'runtime_id': final_runtime,
                                        'output': {} if fault == 'wrong-output' else {'shutdown': 'resumed-owned'}}]}
                final_row = {'task_run_id': task_run_id, 'runtime_id': final_runtime, 'status': status,
                             'attempt': (2 if fault == 'final-attempt' else 1),
                             'started_at': '2026-10-07T12:00:03Z', 'completed_at': completion}
                if fault == 'missing-attempt':
                    final_row.pop('attempt')
                records, partitions = [retained_run, delayed_dispatch_run, split_snapshot_run, running_run, final_run], [retained_row, delayed_dispatch_row, split_snapshot_row, running_row, final_row]
                def api_json(_, path):
                    if '/partitions' in path:
                        return {'total': 1, 'partitions': [partitions.pop(0)]}
                    return records.pop(0)
                c.api_json = api_json
                c.native_running = lambda runtime_id=None: {'runtime_id': runtime_id, 'run_id': RUN,
                    'task_id': TASK, 'docker_image_id': INDEX, 'task_config_id': CONFIG,
                    'command_sha256': hashlib.sha256(c.shutdown_command().encode()).hexdigest(),
                    'control_mount_source_sha256': c.shutdown_control_mount_source_sha256,
                    'control_mount_destination': '/caesium-shutdown-control', 'control_mount_read_only': True,
                    'running': True}
                c.fire_shutdown_webhook = lambda _: {'status': 202, 'path': c.shutdown_job['resumption_path'],
                                                      'http_triggers_accepted': 1, 'http_runs_started': 1,
                                                      'receipt_id': '12345678-1234-4234-8234-123456789abc'}
                with self.assertRaises(b.JourneyError):
                    c.verify_shutdown_job_explicitly_resumed('owned-server')

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
        recovered_runtime = "f" * 64
        collector_run_id = "cov-distinct"
        command_collector = self.collector()
        command_collector.run_id = collector_run_id
        hold_command = command_collector.shutdown_command()
        hold_digest = hashlib.sha256(hold_command.encode()).hexdigest()
        control_mount_digest = hashlib.sha256(b'/owned/sso/shutdown-control').hexdigest()
        shutdown = {"job_id": RUN, "run_id": RUN, "task_id": TASK, "task_run_id": TASK, "runtime_id": CID,
            "task_image_id": CONFIG, "initial_run_status": "running", "initial_task_status": "running",
            "control_mount_source_sha256": control_mount_digest,
            "hold_strategy": "read-only-host-release-marker", "hold_command_sha256": hold_digest,
            "release_marker_absent_before_signal": True, "release_marker_absent_before_webhook": True,
            "attempt_before_signal": 1, "retained_attempt": 1, "final_attempt": 1,
            "native_runtime_removed": True, "verified_after_generation": 2,
            "native_runtime_absent_ids_by_generation": {"1": CID, "2": recovered_runtime},
            "resumption_runtime_absent_after_completion": True,
            "resumption_mode": "explicit-public-http-trigger-existing-run", "resumption_path": "/hooks/coverage-shutdown-" + collector_run_id,
            "retained_after_generation": 2, "retained_run_status": "running", "retained_task_status": "running",
            "generation2_automatic_takeover": False,
            "retained_task_run_id": TASK, "retained_runtime_id": CID,
            "resumption_webhook": {"status": 202, "path": "/hooks/coverage-shutdown-" + collector_run_id,
                "http_triggers_accepted": 1, "http_runs_started": 1, "receipt_id": "12345678-1234-4234-8234-123456789abc"},
            "resumption_runtime_running": {"runtime_id": recovered_runtime, "task_run_id": TASK, "attempt": 1,
                "task_started_at": "2026-10-04T12:00:03Z", "observed_at": "2026-10-04T12:00:03.500000Z",
                "release_marker_absent": True, "running": True,
                "native": {"runtime_id": recovered_runtime, "run_id": RUN, "task_id": TASK,
                    "docker_image_id": INDEX, "task_config_id": CONFIG, "command_sha256": hold_digest,
                    "control_mount_source_sha256": control_mount_digest,
                    "control_mount_destination": "/caesium-shutdown-control", "control_mount_read_only": True,
                    "running": True}},
            "release_marker_after_running_observation": True,
            "resumption_output": {"shutdown": "resumed-" + collector_run_id},
            "release_marker": {"strategy": "read-only-host-release-marker", "written": True,
                "sha256": hashlib.sha256(b"release-v1\n").hexdigest(), "written_at": "2026-10-04T12:00:04Z"},
            "final_run_status": "succeeded", "final_task_status": "succeeded", "final_task_run_id": TASK,
            "final_task_run_status": "succeeded", "final_runtime_id": recovered_runtime,
            "run_completed_at": "2026-10-04T12:00:05Z", "task_started_at": "2026-10-04T12:00:03Z",
            "task_completed_at": "2026-10-04T12:00:05Z",
            "native_runtime_absent_after_generation": 2, "native_runtime_absent_generations": [1, 2],
            "native_runtime_cleanup_absences": [{"runtime_id": CID, "absent": True},
                {"runtime_id": recovered_runtime, "absent": True}],
            "replay_restart_elapsed_seconds": 1,
            "native_before_signal": {"running": True, "run_id": RUN, "task_id": TASK, "runtime_id": CID,
                "docker_image_id": INDEX, "task_config_id": CONFIG, "command_sha256": hold_digest,
                "control_mount_source_sha256": control_mount_digest,
                "control_mount_destination": "/caesium-shutdown-control", "control_mount_read_only": True},
            "native_initial_running": {"running": True, "command_sha256": hold_digest,
                "control_mount_source_sha256": control_mount_digest,
                "control_mount_destination": "/caesium-shutdown-control", "control_mount_read_only": True}}
        original = {"complete": True, "missing": False, "killed": False, "candidate_sha": OWNER, "image_id": IMAGE,
            "collector_run_id": collector_run_id,
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
               "SSO_RECORD_IMAGE_ID": IMAGE, "SSO_RECORD_RUN_ID": collector_run_id, "SSO_RECORD_INPUTS_SHA256": digest}
        for fault in (None, "native", "timestamp", "exit", "flush", "wrong-uuid", "resumption", "same-runtime", "cleanup", "attempt", "auto-takeover", "durable-path", "early-release", "not-running-observed", "marker-before-running", "wrong-output", "wrong-absence-identity", "writable-mount"):
            value = copy.deepcopy(original)
            if fault == "wrong-uuid":
                value["shutdown_cancellation"]["final_task_run_id"] = RUN
            if fault == "native": value["shutdown_cancellation"]["native_before_signal"]["running"] = False
            if fault == "timestamp": value["shutdown_cancellation"]["task_started_at"] = "2026-10-04T12:00:01Z"
            if fault == "exit": value["server_generations"][0]["exit_code"] = 143
            if fault == "flush": value["server_generations"][0]["flush_rc"] = 1
            if fault == "resumption": value["shutdown_cancellation"]["resumption_webhook"]["http_runs_started"] = 0
            if fault == "same-runtime": value["shutdown_cancellation"]["final_runtime_id"] = CID
            if fault == "cleanup": value["shutdown_cancellation"]["native_runtime_cleanup_absences"][1]["absent"] = False
            if fault == "attempt": value["shutdown_cancellation"]["final_attempt"] = 2
            if fault == "auto-takeover": value["shutdown_cancellation"]["generation2_automatic_takeover"] = True
            if fault == "durable-path":
                value["shutdown_cancellation"]["resumption_path"] = "/hooks/coverage-shutdown-" + RUN
                value["shutdown_cancellation"]["resumption_webhook"]["path"] = "/hooks/coverage-shutdown-" + RUN
            if fault == "early-release": value["shutdown_cancellation"]["release_marker"]["written_at"] = "2026-10-04T12:00:06Z"
            if fault == "not-running-observed": value["shutdown_cancellation"]["resumption_runtime_running"]["native"]["running"] = False
            if fault == "marker-before-running": value["shutdown_cancellation"]["release_marker"]["written_at"] = "2026-10-04T12:00:03Z"
            if fault == "wrong-output": value["shutdown_cancellation"]["resumption_output"] = {"shutdown": "wrong-run"}
            if fault == "wrong-absence-identity": value["shutdown_cancellation"]["native_runtime_absent_ids_by_generation"]["2"] = CID
            if fault == "writable-mount": value["shutdown_cancellation"]["resumption_runtime_running"]["native"]["control_mount_read_only"] = False
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

    def test_shell_untrack_last_id_and_keep_another_under_nounset(self):
        shell = '''set -eu
source "$1/scripts/coverage-journeys.sh"
coverage_journey_track_id first
coverage_journey_untrack_id first
[[ "${#COVERAGE_JOURNEY_ACTIVE_IDS[@]}" == 0 ]]
coverage_journey_track_id first
coverage_journey_track_id second
coverage_journey_untrack_id first
[[ "${#COVERAGE_JOURNEY_ACTIVE_IDS[@]}" == 1 && "${COVERAGE_JOURNEY_ACTIVE_IDS[0]}" == second ]]
coverage_journey_untrack_id second
[[ "${#COVERAGE_JOURNEY_ACTIVE_IDS[@]}" == 0 ]]
'''
        result = subprocess.run(["bash", "-c", shell, "hermetic", str(ROOT)], capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_audit_extraction_failure_keeps_ownership_and_cannot_pass(self):
        source = (ROOT / "scripts/integration-coverage.sh").read_text()
        start = source.index("extract_audit() {")
        extract = source[start:source.index("\nwrite_fixture() {", start)]
        shell = '''set -eu
source "$1/scripts/coverage-journeys.sh"
ROOT="$1"; ARTIFACTS="$2"; AUDIT="$2"; ID=owned; CANDIDATE_SHA=owner; IMAGE_ID=image; PLATFORM=linux/arm64
CONTAINER_CLI=fake_docker; FAULT="$3"
require_cmd() { :; }
die() { exit 1; }
log() { :; }
coverage_journey_resource() {
  [[ "$1" != remove || "$FAULT" == success ]] || return 1
}
fake_docker() {
  case "$1" in
    create)
      [[ " $* " == *" --name owned-audit-extract "* && " $* " == *" --label caesium.coverage.owner=owner "* && " $* " == *" --label caesium.coverage.run=owned "* && " $* " == *" --label caesium.coverage.lane=audit-extract "* ]] || return 8
      [[ "$FAULT" != create ]] || return 1
      echo immutable-id
      ;;
    cp) [[ "$FAULT" != cp ]] ;;
    *) return 9 ;;
  esac
}
trap 'printf "%s\\n" "${COVERAGE_JOURNEY_PENDING_NAMES[*]}" >"$ARTIFACTS/names"; if ((${#COVERAGE_JOURNEY_ACTIVE_IDS[@]})); then printf "%s\\n" "${COVERAGE_JOURNEY_ACTIVE_IDS[@]}" >"$ARTIFACTS/ids"; fi' EXIT
''' + extract + "\nextract_audit\nprintf PASS\n"
        for fault in ("success", "create", "cp", "remove"):
            case = self.tmp / fault; case.mkdir()
            result = subprocess.run(["bash", "-c", shell, "hermetic", str(ROOT), str(case), fault], capture_output=True)
            if fault == "success":
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, b"PASS")
            else:
                self.assertNotEqual(result.returncode, 0, fault)
                self.assertNotIn(b"PASS", result.stdout)
                self.assertIn("owned-audit-extract", (case / "names").read_text())
                if fault != "create": self.assertIn("immutable-id", (case / "ids").read_text())

    def test_network_rm_failure_retains_ledger(self):
        c = self.collector(); c.network_id, c.network_attempted = CID, True
        info = {'Id': CID, 'Labels': {b.LABEL_OWNER: OWNER, b.LABEL_RUN: 'owned'}}
        command, _ = self.command(info, fault='rm'); c.docker_run = command
        with self.assertRaises(b.JourneyError): c.cleanup()
        self.assertEqual(c.network_id, CID)
        self.assertFalse(c.cleanup_complete)

# Exercise the sourced cleanup and its embedded Node program without a daemon.
# The bridge maps /fixture and /proc/self/fd onto private host fixtures; the
# production NOFOLLOW/fstat/fchown/fchmod calls still execute unchanged.
GIT_RESTORE_FAKE_RUNTIME = r"""#!/usr/bin/env python3
import json, os, pathlib, subprocess, sys
args = sys.argv[1:]
base = pathlib.Path(os.environ['FAKE_ARTIFACTS'])
state = base / 'native-state.json'
events = base / 'events.jsonl'
mode = os.environ['FAKE_MODE']
cid = 'e' * 64
image = 'sha256:' + 'b' * 64
with events.open('a') as stream:
    stream.write(json.dumps({'op':args[0], 'action':args[1] if args[0]=='resource' else None,
      'reference':args[3] if args[0]=='resource' else None,
      'name':args[args.index('--name')+1] if args[0]=='run' and '--name' in args else None})+'\n')
if args[0] == 'run':
    labels = [args[i+1] for i,arg in enumerate(args[:-1]) if arg=='--label']
    lane_labels = [x for x in labels if x.startswith('caesium.coverage.lane=')]
    assert len(lane_labels)==1, labels
    lane = lane_labels[0].split('=',1)[1]
    if lane=='git-prep-test':
        cid='f'*64
        assert '--rm' not in args and '--pull=never' in args and image in args
        names = [args[i+1] for i,arg in enumerate(args[:-1]) if arg=='--name']
        assert names==['run-owned-journey-git-runner'], names
        assert labels==['caesium.coverage.owner='+'a'*40,
          'caesium.coverage.run=run-owned', 'caesium.coverage.lane=git-prep-test'], labels
        assert args[1]=='-d' and args.count('--label')==3 and args.count('--name')==1
        assert args[-4:]==['sh','scripts/integration-test.sh','-test.run','exact-test-pattern']
        assert args[args.index('--network')+1]=='container:owned-server'
        assert args[args.index('-v')+1]=='/source-root:/source'
        assert '/owned/cli:/coverage-cli:ro' in args
        assert '/owned/docker.sock:/var/run/docker.sock' in args
        assert '/owned/coverage:/coverage' in args
        assert os.environ['FAKE_FIXTURE']+':/fixture:rw' in args
        assert '/raw/journeys/git-sync/evidence:/coverage-evidence:rw' in args
        assert 'CAESIUM_JOBDEF_GIT_SYNC_LANE=true' in args
        value={'Id':cid,'Image':image,'RestartCount':0,
          'State':{'Status':'exited','Running':False,'ExitCode':7 if mode=='runner-error' else 0,
           'OOMKilled':False,'FinishedAt':'2026-10-05T22:00:00Z'},
          'Name':names[0],'lane':lane,
          'labels':{'caesium.coverage.owner':'a'*40,'caesium.coverage.run':'run-owned','caesium.coverage.lane':lane},
          'log':'--- PASS: TestIntegrationTestSuite/TestJobdefGitSyncLocalRepositoryUpdatesAndPrunes\n'}
        state.write_text(json.dumps(value)); print(cid); sys.exit(0)
    required = ['--pull=never', '--name', '--read-only', '--user', '--network', '--entrypoint']
    assert all(x in args for x in required) and '--rm' not in args
    assert args[args.index('--network')+1]=='none' and args[args.index('--user')+1]=='0:0'
    assert args[args.index('--entrypoint')+1]=='node' and image in args
    for label in ['caesium.coverage.owner='+'a'*40, 'caesium.coverage.run=run-owned', 'caesium.coverage.lane=git-prep-restore']:
        assert label in args
    mount = args[args.index('-v')+1]
    assert mount == os.environ['FAKE_FIXTURE']+':/fixture:rw'
    program = args[args.index('-e')+1]
    ids = args[args.index('-e')+2:]
    assert ids[:2] == [str(os.getuid()), str(os.getgid())] and len(ids)==3 and len(ids[2])==64
    bridge = r'''
const bridgeFS = require('fs');
const realOpen = bridgeFS.openSync, realClose = bridgeFS.closeSync, realDir = bridgeFS.opendirSync;
const handles = new Map();
function translate(p) {
  if (p === '/fixture') return process.env.FAKE_FIXTURE;
  const m = /^\/proc\/self\/fd\/(\d+)(.*)$/.exec(p);
  if (!m) return p;
  if (!handles.has(Number(m[1]))) throw Error('missing held parent');
  return handles.get(Number(m[1])) + m[2];
}
bridgeFS.openSync = function(p, ...args) { const path = translate(p); const fd = realOpen(path, ...args); handles.set(fd,path); return fd; };
bridgeFS.closeSync = function(fd) { handles.delete(fd); return realClose(fd); };
bridgeFS.opendirSync = function(p, ...args) { return realDir(translate(p), ...args); };
'''
    env=os.environ.copy()
    if mode=='mount-substituted': env['FAKE_FIXTURE']=os.environ['FAKE_FOREIGN']
    result = subprocess.run(['node', '-e', bridge+'\n'+program, *ids], capture_output=True, text=True,env=env)
    value = {'Id':cid,'Image':image,'RestartCount':0,
      'State':{'Status':'exited','Running':False,'ExitCode':1 if mode=='restore-fail' else result.returncode,
       'OOMKilled':False,'FinishedAt':'2026-10-05T22:00:00Z'},
      'Name':args[args.index('--name')+1], 'lane':'git-prep-restore','log':result.stdout+result.stderr}
    state.write_text(json.dumps(value))
    if mode=='ack-fail': sys.exit(47)
    print('short-id' if mode=='malformed-ack' else cid)
elif args[0] == 'logs':
    value=json.loads(state.read_text()); assert args[1]==value['Id']
    print(value['log'],end='')
elif args[0] == 'resource':
    _,action,kind,reference,*extra=args
    if action=='absent':
        sys.exit(1 if state.exists() or mode=='pending-writer' else 0)
    if mode in ('active-writer','pending-writer'):
        sys.exit(1)
    if not state.exists(): sys.exit(1)
    value=json.loads(state.read_text())
    cid=value['Id']
    assert kind=='container' and reference in (cid,value['Name'])
    assert extra==[image,value['lane']]
    if value.get('labels'):
        assert value['labels']=={'caesium.coverage.owner':'a'*40,
          'caesium.coverage.run':'run-owned','caesium.coverage.lane':value['lane']}
    if action=='owned': print(json.dumps(value))
    elif action=='remove':
        if mode=='remove-fail': sys.exit(1)
        state.unlink(); print(json.dumps({'absent':True,'Id':cid}))
    else: sys.exit(9)
else: sys.exit(9)
"""

GIT_RESTORE_SHELL = r'''set -eu
source "$1/scripts/coverage-journeys.sh"
ARTIFACTS="$2"; ID=run-owned; CONTAINER_CLI="$3"
CANDIDATE_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
BUILDER_RUN_IMAGE=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
IMAGE_ID="$BUILDER_RUN_IMAGE"; PLATFORM=linux/arm64
COVERAGE_JOURNEY_PREP_POLL_INTERVAL=0
log() { printf '%s\n' "$*" >&2; }
coverage_journey_resource() { "$CONTAINER_CLI" resource "$@"; }
coverage_journey_register_git_dir "$4" || exit 88
case "$FAKE_MODE" in
  active-writer) COVERAGE_JOURNEY_ACTIVE_IDS=(uncertain-writer) ;;
  pending-writer) COVERAGE_JOURNEY_PREP_PENDING_NAMES=(uncertain-writer); COVERAGE_JOURNEY_PREP_PENDING_LANES=(git-prep-test) ;;
  prep-failed) COVERAGE_JOURNEY_PREP_FAILED=true ;;
  changed-root) mv "$4" "$4.old"; mkdir "$4" ;;
  root-symlink) mv "$4" "$4.old"; ln -s "$FAKE_FOREIGN" "$4" ;;
esac
cleanup_coverage_journeys
'''


class GitFixtureOwnershipChecks(unittest.TestCase):
    def exercise(self, mode, unsafe=None):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            artifacts = root / 'artifacts'; (artifacts / 'journeys').mkdir(parents=True)
            fixture = artifacts / 'journeys/git-source-fixture-run-owned.abc123'; fixture.mkdir()
            child = fixture / '.git'; child.mkdir(); (child / 'HEAD').write_text('fixture')
            child.chmod(0o500); (child / 'HEAD').chmod(0o400)
            foreign = root / 'foreign'; foreign.mkdir(); target = foreign / 'untouched'; target.write_text('foreign')
            before = (target.stat().st_uid, target.stat().st_gid, target.stat().st_mode)
            if unsafe=='symlink': (fixture / 'foreign-link').symlink_to(foreign, target_is_directory=True)
            if unsafe=='hardlink': os.link(target, fixture / 'foreign-hardlink')
            if unsafe=='fifo': os.mkfifo(fixture / 'foreign-fifo')
            if unsafe=='depth':
                deep = fixture
                for _ in range(33): deep /= 'child'; deep.mkdir()
            runtime = root / 'fake-runtime'; runtime.write_text(GIT_RESTORE_FAKE_RUNTIME); runtime.chmod(0o700)
            env = os.environ.copy(); env.update(FAKE_ARTIFACTS=str(artifacts), FAKE_FIXTURE=str(fixture), FAKE_MODE=mode, FAKE_FOREIGN=str(foreign))
            shell = GIT_RESTORE_SHELL
            if mode.startswith('runner-'):
                text = (ROOT / 'scripts/coverage-journeys.sh').read_text()
                marker = text.index('# Keep the runner payload separate from lifecycle identity.')
                begin = text.index('      runner_payload_args=(',marker)
                branch = text[begin:text.index('      passes=',begin)]
                writer = '''run_writer() {
local mode=git-sync lane=git-sync test_rc=125 pattern=exact-test-pattern
local runner_name="${ID}-journey-runner-${lane}" server_id=owned-server
local cli_dir=/owned/cli cli_raw=/owned/coverage SOCK=/owned/docker.sock
local ROOT=/source-root lane_dir=/raw/journeys/git-sync
local COVERAGE_GIT_FIXTURE_ROOT="$FAKE_FIXTURE"
local COVERAGE_GIT_SOURCES_JSON='[{"url":"git://owned:9418/coverage.git"}]'
local CAESIUM_EVENT_INGEST_API_KEY=integration-test-key
local runner_log="$ARTIFACTS/journeys/test.log"
local -a runner_args=() runner_payload_args=()
''' + branch + '''
[[ "${runner_args[0]}" == run && " ${runner_args[*]} " == *" --rm "* ]] || exit 88
[[ " ${runner_args[*]} " == *" --name ${ID}-journey-runner-git-sync "* ]] || exit 89
[[ " ${runner_args[*]} " == *" caesium.coverage.lane=git-sync-runner "* ]] || exit 90
[[ "${runner_args[*]}" != *git-prep-test* ]] || exit 91
name_count=0; label_count=0
for arg in "${runner_args[@]}"; do
  [[ "$arg" == --name ]] && ((name_count+=1))
  [[ "$arg" == --label ]] && ((label_count+=1))
done
[[ "$name_count" == 1 && "$label_count" == 3 ]] || exit 92
if [[ "$FAKE_MODE" == runner-error ]]; then [[ "$test_rc" == 7 ]]; else [[ "$test_rc" == 0 ]]; fi
}
run_writer || exit 87
'''
                shell = shell.replace('cleanup_coverage_journeys\n',writer+'cleanup_coverage_journeys\n')
            result = subprocess.run(['/bin/bash','-c',shell,'hermetic',str(ROOT),str(artifacts),str(runtime),str(fixture)], env=env, capture_output=True, timeout=20)
            events = [json.loads(x) for x in (artifacts / 'events.jsonl').read_text().splitlines()] if (artifacts / 'events.jsonl').exists() else []
            if mode in ('success','runner-success') and unsafe is None:
                self.assertEqual(result.returncode,0,result.stderr)
                self.assertFalse(fixture.exists())
                expected=['resource','run','resource','resource','logs','resource']
                self.assertEqual([x['op'] for x in events],expected * (2 if mode=='runner-success' else 1))
                if mode=='runner-success':
                    self.assertEqual(events[1]['name'],'run-owned-journey-git-runner')
                    self.assertEqual(events[7]['name'],'run-owned-journey-git-restore-0')
                    self.assertEqual(events[5]['reference'],'f'*64,
                                     'test writer must be removed by the exact immutable ID')
                    self.assertEqual(events[5]['action'],'remove')
                    self.assertEqual(events[11]['reference'],'e'*64,
                                     'fixture restore helper must also be removed by exact ID')
                self.assertFalse((artifacts / 'native-state.json').exists())
                self.assertIn('ownership-restored',(artifacts / 'journeys/git-restore-run-owned-0.log').read_text())
            else:
                self.assertNotEqual(result.returncode,0,(mode,unsafe,result.stdout,result.stderr))
                self.assertTrue(fixture.exists())
                self.assertEqual((target.stat().st_uid,target.stat().st_gid,target.stat().st_mode),before)
                self.assertTrue((artifacts / 'retained-owned-git-fixture-paths.txt').exists())
                self.assertIn('operator reconciliation required',result.stderr.decode())
                if mode in ('active-writer','pending-writer','prep-failed','changed-root','root-symlink'):
                    self.assertFalse(any(x['op']=='run' for x in events))
                if unsafe:
                    self.assertEqual((child / 'HEAD').stat().st_mode & 0o777,0o400)
                if mode=='remove-fail':
                    self.assertTrue((artifacts / 'native-state.json').exists())
                    self.assertIn('e'*64,(artifacts / 'retained-owned-preparation-container-ids.txt').read_text())
            # Remove only harness-owned files after the refusal assertions.
            child.chmod(0o700) if child.exists() else None

    def test_real_embedded_restore_program_then_checked_removal(self):
        self.exercise('success')

    def test_uncertain_or_failed_writers_prevent_restoration(self):
        for mode in ('active-writer','pending-writer','prep-failed'):
            with self.subTest(mode=mode): self.exercise(mode)

    def test_changed_root_and_root_symlink_refuse_before_allocation(self):
        for mode in ('changed-root','root-symlink'):
            with self.subTest(mode=mode): self.exercise(mode)

    def test_unsafe_descendants_refuse_before_any_permission_change(self):
        for unsafe in ('symlink','hardlink','fifo','depth'):
            with self.subTest(unsafe=unsafe): self.exercise('success',unsafe)

    def test_git_test_writer_is_joined_before_restore_and_error_stays_refused(self):
        self.exercise('runner-success')
        self.exercise('runner-error')

    def test_shared_runner_payload_preserves_all_non_git_lane_arguments(self):
        source = (ROOT / 'scripts/coverage-journeys.sh').read_text()
        marker = source.index('# Keep the runner payload separate from lifecycle identity.')
        begin = source.index('      runner_payload_args=(', marker)
        runner_start = source.index('      runner_args=(', begin)
        end = source.index('      if [[ "$mode" == "git-sync" ]]; then', runner_start)
        production = source[begin:end]
        wrapper = r'''set -euo pipefail
run_mode() {
local mode="$1" lane="$1" test_rc=0 pattern=exact-test-pattern
local runner_name="${ID}-journey-runner-${lane}" server_id=owned-server
local cli_dir=/owned/cli cli_raw=/owned/coverage SOCK=/owned/docker.sock
local ROOT=/source-root lane_dir=/raw/journeys/${lane} auth_env=/owned/auth.env
local CAESIUM_EVENT_INGEST_API_KEY=integration-test-key
local -a runner_args=() runner_payload_args=()
''' + production + r'''
  [[ "${runner_args[0]}" == run && " ${runner_args[*]} " == *" --pull=never "* && " ${runner_args[*]} " == *" --rm "* ]]
  [[ " ${runner_args[*]} " == *" --platform linux/arm64 "* && " ${runner_args[*]} " == *" --name owned-journey-runner-${mode} "* ]]
  [[ " ${runner_args[*]} " == *" --label caesium.coverage.owner=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa "* ]]
  [[ " ${runner_args[*]} " == *" --label caesium.coverage.run=owned "* ]]
  [[ " ${runner_args[*]} " == *" --label caesium.coverage.lane=${mode}-runner "* ]]
  [[ " ${runner_payload_args[*]} " == *" -v /source-root:/source "* ]]
  [[ " ${runner_payload_args[*]} " == *" -v /owned/cli:/coverage-cli:ro "* ]]
  [[ " ${runner_payload_args[*]} " == *" -v /owned/docker.sock:/var/run/docker.sock "* ]]
  [[ " ${runner_payload_args[*]} " == *" -v /owned/coverage:/coverage "* ]]
  [[ " ${runner_payload_args[*]} " == *" --network container:owned-server "* ]]
  [[ " ${runner_payload_args[*]} " == *" -w /source "* ]]
  [[ " ${runner_payload_args[*]} " == *" sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb sh scripts/integration-test.sh -test.run exact-test-pattern "* ]]
  if [[ "$mode" == auth ]]; then
    [[ " ${runner_payload_args[*]} " == *" --env-file /owned/auth.env "* ]]
    [[ " ${runner_payload_args[*]} " == *" CAESIUM_AGENT_AUTH_LANE=true "* ]]
    [[ " ${runner_payload_args[*]} " == *" CAESIUM_AUTH_MODE=api-key "* ]]
  elif [[ "$mode" == distributed || "$mode" == owner-memory ]]; then
    [[ " ${runner_payload_args[*]} " == *" CAESIUM_EXECUTION_MODE=distributed "* ]]
    if [[ "$mode" == owner-memory ]]; then
      [[ " ${runner_payload_args[*]} " == *" CAESIUM_RUN_OWNER_IN_MEMORY=true "* ]]
    else
      [[ " ${runner_payload_args[*]} " != *CAESIUM_RUN_OWNER_IN_MEMORY* ]]
    fi
  else
    [[ " ${runner_payload_args[*]} " != *CAESIUM_EXECUTION_MODE* ]]
    [[ " ${runner_payload_args[*]} " != *CAESIUM_AGENT_AUTH_LANE* ]]
  fi
}
ID=owned CANDIDATE_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa PLATFORM=linux/arm64
BUILDER_RUN_IMAGE=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
CAESIUM_MAINTENANCE_JOURNEYS_REQUIRED=true
for mode in local auth distributed owner-memory; do run_mode "$mode"; done
'''
        result=subprocess.run(['/bin/bash','-c',wrapper],capture_output=True,text=True,timeout=10)
        self.assertEqual(result.returncode,0,result.stderr+result.stdout)

    def test_git_sync_lane_uses_prep_owned_identity_and_removes_exact_id(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            artifacts = root / 'artifacts'; (artifacts / 'journeys').mkdir(parents=True)
            raw = root / 'raw'; raw.mkdir()
            fake_bin = root / 'bin'; fake_bin.mkdir()
            prep_id = 'f' * 64
            server_id = 'a' * 64
            image = 'sha256:' + 'b' * 64
            state = root / 'prep-state.json'
            events = root / 'events.jsonl'
            runtime = fake_bin / 'runtime'
            runtime.write_text(r'''#!/usr/bin/env python3
import json, os, pathlib, sys
args=sys.argv[1:]
events=pathlib.Path(os.environ['FAKE_EVENTS'])
def record(value):
    with events.open('a') as out: out.write(json.dumps(value)+'\n')
if args[0]=='run' and '-d' in args:
    name=args[args.index('--name')+1]
    labels=[args[i+1] for i,x in enumerate(args[:-1]) if x=='--label']
    record({'op':'run','name':name,'labels':labels,'args':args})
    if 'git-prep-test' in labels[-1]:
        assert args[1]=='-d' and '--rm' not in args, args
        assert name=='owned-journey-git-runner', name
        assert labels==['caesium.coverage.owner='+'a'*40,'caesium.coverage.run=owned','caesium.coverage.lane=git-prep-test'], labels
        assert args.count('--name')==1 and args.count('--label')==3, args
        assert '-v' in args and os.environ['FAKE_FIXTURE']+':/fixture:rw' in args
        assert '-e' in args and 'CAESIUM_JOBDEF_GIT_SYNC_LANE=true' in args
        state={'Id':'f'*64,'Image':os.environ['FAKE_IMAGE'],'RestartCount':0,
          'Config':{'Labels':{'caesium.coverage.owner':'a'*40,'caesium.coverage.run':'owned','caesium.coverage.lane':'git-prep-test'}},
          'State':{'Status':'exited','Running':False,'ExitCode':0,'OOMKilled':False,'FinishedAt':'2026-10-08T00:00:00Z'}}
        pathlib.Path(os.environ['FAKE_STATE']).write_text(json.dumps(state))
        print('f'*64)
    else:
        print('a'*64)
elif args[0]=='run' and 'wget' in args:
    print('healthy')
elif args[0]=='inspect' and args[1]=='-f':
    print(os.environ['FAKE_SERVER_IMAGE'])
elif args[0]=='logs':
    print('--- PASS: TestIntegrationTestSuite/TestJobdefGitSyncLocalRepositoryUpdatesAndPrunes')
else:
    raise SystemExit('unexpected fake runtime command: '+repr(args))
''')
            runtime.chmod(0o700)
            real_python = sys.executable
            fake_python = fake_bin / 'python3'
            fake_python.write_text('#!/bin/sh\nif [ "$1" = "' + str(ROOT / 'scripts/test_coverage_named_journeys.py') + '" ]; then\n  if [ "${2:-}" = validate-git-receipt ]; then printf \'{"valid":true}\\n\'; fi\n  exit 0\nfi\nexec ' + real_python + ' "$@"\n')
            fake_python.chmod(0o700)
            harness = r'''set -euo pipefail
trap 'rc=$?; printf "ERR rc=%s line=%s cmd=%s\\n" "$rc" "$LINENO" "$BASH_COMMAND" >&2' ERR
source "$1/scripts/coverage-journeys.sh"
ROOT="$1"; ARTIFACTS="$2"; RAW="$3"; CONTAINER_CLI="$4"
ID=owned; CANDIDATE_SHA=$(printf 'a%.0s' {1..40})
IMAGE_ID="sha256:$(printf 'c%.0s' {1..64})"; BUILDER_RUN_IMAGE="sha256:$(printf 'b%.0s' {1..64})"
PLATFORM=linux/amd64; NETWORK=owned-network; SOCK=/owned/docker.sock; SOCK_GID=0
BUILD_CONTEXT='{}'; IMAGE_PROVENANCE=built-by-this-run; IMAGE_VERIFIED=true
COVERAGE_JOURNEY_PREP_POLL_INTERVAL=0
COVERAGE_JOURNEY_SERVER_ENV=(CAESIUM_TEST_FAKE=true)
log() { printf '%s\n' "$*" >&2; }
coverage_journey_server_env() { COVERAGE_JOURNEY_SERVER_ENV=(CAESIUM_TEST_FAKE=true); }
coverage_journey_prepare_git_sync() {
  local lane_dir="$1"
  COVERAGE_GIT_FIXTURE_ROOT="$FAKE_FIXTURE"
  COVERAGE_GIT_SOURCES_JSON='[{"url":"git://owned:9418/coverage.git"}]'
  mkdir -p "$lane_dir/evidence"
  printf '{"fixture":"owned"}\n' >"$lane_dir/evidence/git-sync.json"
}
coverage_journey_resource() {
  local action="$1" kind="$2" ref="$3" image="${4:-}" lane="${5:-}"
  case "$action:$kind:$ref" in
    absent:container:*) return 0 ;;
    owned:container:owned-journey-git-runner|owned:container:$(printf 'f%.0s' {1..64})) cat "$FAKE_STATE" ;;
    stop:container:$(printf 'a%.0s' {1..64}))
      printf '{"flush_rc":0,"stop_rc":0,"State":{"ExitCode":0,"OOMKilled":false,"FinishedAt":"2026-10-08T00:00:01Z"}}\n' ;;
    remove:container:*)
      printf '{"op":"remove","reference":"%s","lane":"%s"}\n' "$ref" "$lane" >>"$FAKE_EVENTS"
      [[ "$ref" == $(printf 'f%.0s' {1..64}) || "$ref" == $(printf 'a%.0s' {1..64}) ]] ;;
    *) printf 'unexpected resource %s %s %s %s\n' "$action" "$kind" "$ref" "$lane" >&2; return 1 ;;
  esac
}
gocoverdir_complete() { return 0; }
coverage_journey_build_named_args() { COVERAGE_JOURNEY_NAMED_ARGS=(--log "$1"); }
coverage_journey_write_record() {
  [[ "$4" == 0 && "$5" -ge 1 && "${10}" == true && "${17}" == true && "${19}" == true ]]
  printf '{"test_rc":%s,"passes":%s,"complete":%s}\n' "$4" "$5" "${10}" >"$RAW/journeys/git-sync/record.json"
}
coverage_journey_run_lane git-sync git-sync exact-test-pattern 1 /owned/cli
[[ -s "$RAW/journeys/git-sync/record.json" ]]
'''
            env = os.environ.copy()
            env.update(PATH=str(fake_bin) + os.pathsep + env.get('PATH',''),
                       FAKE_STATE=str(state), FAKE_EVENTS=str(events), FAKE_FIXTURE=str(root/'fixture'),
                       FAKE_IMAGE=image, FAKE_SERVER_IMAGE='sha256:'+'c'*64)
            (root/'fixture').mkdir()
            completed = subprocess.run(['/bin/bash','-c',harness,'git-sync-lane',str(ROOT),str(artifacts),str(raw),str(runtime)],
                                       env=env,capture_output=True,text=True,timeout=20)
            self.assertEqual(completed.returncode,0,completed.stderr+completed.stdout)
            operations=[json.loads(line) for line in events.read_text().splitlines()]
            prep=[item for item in operations if item.get('op')=='run' and item['name']=='owned-journey-git-runner']
            self.assertEqual(len(prep),1)
            self.assertEqual(prep[0]['labels'],[
                'caesium.coverage.owner='+'a'*40,
                'caesium.coverage.run=owned',
                'caesium.coverage.lane=git-prep-test',
            ])
            self.assertNotIn('--rm',prep[0]['args'])
            self.assertEqual([item for item in operations if item.get('op')=='remove'],[
                {'op':'remove','reference':prep_id,'lane':'git-prep-test'},
                {'op':'remove','reference':server_id,'lane':''},
            ])

    def test_restore_failure_and_lost_acknowledgement_never_clean_fixture(self):
        for mode in ('restore-fail','ack-fail','malformed-ack','remove-fail','mount-substituted'):
            with self.subTest(mode=mode): self.exercise(mode)


if __name__ == "__main__":
    unittest.main()
