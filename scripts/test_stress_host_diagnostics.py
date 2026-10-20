"""Fail-closed reductions and real bounded observer subprocess regressions."""

import importlib.util
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

import yaml


SCRIPT = Path(__file__).with_name("collect-stress-host-diagnostics.py")
SPEC = importlib.util.spec_from_file_location("stress_host_diagnostics", SCRIPT)
DIAGNOSTICS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(DIAGNOSTICS)
CID = "a" * 64
FOREIGN = "b" * 64
SINCE = 1_000
UNTIL = 1_010


def journal(message, *, source="kernel", unit=None, stamp=1_005_000_000):
    record = {"MESSAGE": message, "__REALTIME_TIMESTAMP": str(stamp)}
    if source == "kernel":
        record["_TRANSPORT"] = "kernel"
    if unit:
        record["_SYSTEMD_UNIT"] = unit
    return json.dumps(record)


class CaptureTests(unittest.TestCase):
    def test_real_success_and_literal_failure_exit_status(self):
        status, body = DIAGNOSTICS.capture([sys.executable, "-c", "print('observed')"])
        self.assertEqual(status, {"exit_code": 0})
        self.assertEqual(body, b"observed\n")
        status, body = DIAGNOSTICS.capture([sys.executable, "-c", "import sys; print('partial'); sys.exit(7)"])
        self.assertEqual(status, {"exit_code": 7, "error": "exit_nonzero"})
        self.assertEqual(body, b"partial\n")

    def test_real_timeout_and_oversize_discard_partial_bytes(self):
        for code, options, expected in [
                ("import time; print('csk_partial', flush=True); time.sleep(60)", {"timeout": .05}, "timeout"),
                ("print('x' * 1024)", {"max_bytes": 128}, "oversize")]:
            with self.subTest(expected=expected):
                started = time.monotonic()
                status, body = DIAGNOSTICS.capture([sys.executable, "-c", code], **options)
                self.assertEqual(status["error"], expected)
                self.assertIsInstance(status["exit_code"], int)
                self.assertEqual(body, b"")
                self.assertLess(time.monotonic() - started, 2)

    def test_successful_leader_cannot_leave_a_detached_pipe_child_running(self):
        with tempfile.TemporaryDirectory() as tmp:
            pid_file = Path(tmp) / "child.pid"
            code = (
                "import pathlib, subprocess, sys; "
                "child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'], "
                "stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL); "
                f"pathlib.Path({str(pid_file)!r}).write_text(str(child.pid)); print('complete')"
            )
            status, body = DIAGNOSTICS.capture([sys.executable, "-c", code])
            self.assertEqual(status, {"exit_code": 0})
            self.assertEqual(body, b"complete\n")
            pid = int(pid_file.read_text())
            try:
                deadline = time.monotonic() + 1
                while True:
                    process = subprocess.run(["ps", "-o", "stat=", "-p", str(pid)],
                                             capture_output=True, text=True, timeout=1)
                    state = process.stdout.strip()
                    if not state or state.startswith("Z"):
                        break
                    if time.monotonic() >= deadline:
                        self.fail("owned observer descendant remained running after leader exit")
                    time.sleep(.01)
            finally:
                try:
                    os.kill(pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass


class ReducerTests(unittest.TestCase):
    def setUp(self):
        privileged = mock.patch.object(DIAGNOSTICS.os, "geteuid", return_value=0)
        privileged.start()
        self.addCleanup(privileged.stop)

    def reduce(self, messages, source="kernel"):
        return DIAGNOSTICS.reduce_journal("\n".join(messages).encode(), [CID], source, SINCE, UNTIL)

    def test_only_same_container_message_can_prove_memcg_oom(self):
        result = self.reduce([
            journal(f"oom-kill:constraint=CONSTRAINT_MEMCG,task_memcg=/system.slice/docker-{CID}.scope"),
            journal(f"oom-kill:constraint=CONSTRAINT_MEMCG,task_memcg=/docker/{FOREIGN}"),
            journal(f"oom-kill:constraint=CONSTRAINT_MEMCG,task_memcg=x{CID}z"),
            json.dumps({"MESSAGE": "foreign event", "container_id": CID}),
        ])
        self.assertEqual(len(result["records"]), 1)
        self.assertEqual(result["records"][0]["finding"], "memcg_oom")

    def test_exit_137_kill_from_docker_and_failed_oomd_kill_remain_unknown(self):
        messages = [journal(f"container {CID} exited 137", source="units", unit="docker.service"),
                    journal(f"Killed {CID}", source="units", unit="containerd.service"),
                    journal(f"Failed to kill {CID}", source="units", unit="systemd-oomd.service")]
        result = self.reduce(messages, "units")
        self.assertEqual([record["finding"] for record in result["records"]], ["unknown"] * 3)
        positive = self.reduce([journal(f"Killed /docker/{CID} due to memory pressure",
                                       source="units", unit="systemd-oomd.service")], "units")
        self.assertEqual(positive["records"][0]["finding"], "oomd_kill")

    def test_malformed_foreign_transport_and_out_of_window_records_are_rejected(self):
        result = self.reduce(["{", journal(f"constraint=CONSTRAINT_MEMCG {CID}", stamp=999_000_000),
                              journal(f"constraint=CONSTRAINT_MEMCG {CID}", source="units", unit="docker.service")])
        self.assertEqual(result, {"records": [], "rejected_records": 3})

    def test_matching_records_redact_credentials_without_retaining_other_fields(self):
        raw = json.loads(journal(f"constraint=CONSTRAINT_MEMCG {CID} csk_fixture-secret Bearer bearer-secret"))
        raw["SECRET_ENV"] = "unrelated-private-value"
        result = self.reduce([json.dumps(raw)])
        encoded = json.dumps(result)
        for secret in ("csk_fixture-secret", "bearer-secret", "unrelated-private-value", "SECRET_ENV"):
            self.assertNotIn(secret, encoded)
        self.assertIn("[REDACTED_API_KEY]", encoded)
        self.assertIn("[REDACTED_BEARER]", encoded)

    def test_strict_smoke_identity_and_time_window_fail_before_commands(self):
        with tempfile.TemporaryDirectory() as tmp:
            smoke = Path(tmp) / "smoke.log"
            smoke.write_text(f"untrusted prefix stress smoke: cid={CID}\nstress smoke: cid={CID}f\n")
            with mock.patch.object(DIAGNOSTICS, "capture") as capture:
                report = DIAGNOSTICS.collect(smoke, str(SINCE), now=UNTIL)
                self.assertEqual(report["error"], "smoke_identity_unproved")
                self.assertEqual(report["classification"], "unknown")
                capture.assert_not_called()
            smoke.write_text(f"stress smoke: cid={CID} status=exited\n")
            for since in ("not-a-time", str(UNTIL + 1), str(UNTIL - 121)):
                with self.subTest(since=since), mock.patch.object(DIAGNOSTICS, "capture") as capture:
                    report = DIAGNOSTICS.collect(smoke, since, now=UNTIL)
                    self.assertEqual(report["error"], "time_window_unproved")
                    capture.assert_not_called()

    def test_non_root_observer_is_explicitly_unavailable_before_any_probe(self):
        with mock.patch.object(DIAGNOSTICS.os, "geteuid", return_value=1001), \
                mock.patch.object(DIAGNOSTICS, "capture") as capture:
            report = DIAGNOSTICS.collect("not-read-without-ownership", str(SINCE), now=UNTIL)
        self.assertEqual(report["observer_uid"], 1001)
        self.assertEqual(report["availability"], "unavailable")
        self.assertEqual(report["error"], "observer_requires_root")
        self.assertEqual(report["classification"], "unknown")
        self.assertEqual(report["qualification"], "unchanged")
        capture.assert_not_called()

    def test_real_controlled_commands_retain_probe_failure_and_bound_candidate_evidence(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            smoke = directory / "smoke.log"
            smoke.write_text(f"stress smoke: cid={CID} status=exited\n")
            kernel = journal(f"oom-kill:constraint=CONSTRAINT_MEMCG,task_memcg=/docker/{CID} csk_kernel-secret")
            journalctl = directory / "journalctl"
            journalctl.write_text(f"#!{sys.executable}\nimport sys\n"
                                  f"print({kernel!r}) if '-k' in sys.argv else None\n"
                                  "sys.exit(0 if '-k' in sys.argv else 7)\n")
            identity = {"ID": "daemon-id", "Name": "daemon-host", "CgroupDriver": "systemd",
                        "CgroupVersion": "2", "KernelVersion": "observed-kernel",
                        "OperatingSystem": "Ubuntu 26.04", "ServerVersion": "29.9.1",
                        "ContainerdCommit": {"ID": "containerd-commit", "Secret": "must-not-save"},
                        "RuncCommit": {"ID": "runc-commit"},
                        "SecurityOptions": ["name=seccomp,profile=builtin", "csk_security-secret"],
                        "Secret": "must-not-save"}
            docker = directory / "docker"
            docker.write_text(f"#!{sys.executable}\nprint({json.dumps(identity)!r})\n")
            systemctl = directory / "systemctl"
            systemctl.write_text(f"#!{sys.executable}\nimport sys\nprint('inactive')\nsys.exit(3)\n")
            for command in (journalctl, docker, systemctl):
                command.chmod(0o755)
            # Only controlled executables are reachable. A reintroduced nested
            # sudo cannot read the actual host journal under this test.
            with mock.patch.dict(os.environ, {"PATH": tmp,
                                             "GITHUB_SHA": "c" * 40, "GITHUB_RUN_ID": "123",
                                             "GITHUB_RUN_ATTEMPT": "2", "PRIVATE_ENV": "must-not-save"}):
                report = DIAGNOSTICS.collect(smoke, str(SINCE), now=UNTIL)
            self.assertEqual(report["candidate_sha"], "c" * 40)
            self.assertEqual(report["run_id"], "123")
            self.assertEqual(report["run_attempt"], "2")
            self.assertEqual(report["observer_uid"], 0)
            self.assertEqual(report["classification"], "memcg_oom")
            self.assertEqual(report["qualification"], "unchanged")
            self.assertEqual(report["probes"]["units"], {"exit_code": 7, "error": "exit_nonzero"})
            self.assertEqual(report["probes"]["oomd_active"], {"exit_code": 3, "error": "exit_nonzero"})
            self.assertEqual(report["oomd_active_state"], "inactive")
            expected = {**identity, "ContainerdCommit": {"ID": "containerd-commit"},
                        "SecurityOptions": ["name=seccomp,profile=builtin", "[REDACTED_API_KEY]"]}
            del expected["Secret"]
            self.assertEqual(report["daemon"], expected)
            self.assertNotIn("shared_host_proved", report)
            self.assertNotIn("must-not-save", json.dumps(report))
            self.assertNotIn("csk_kernel-secret", json.dumps(report))
            self.assertNotIn("csk_security-secret", json.dumps(report))


class WorkflowOwnershipTests(unittest.TestCase):
    def test_every_producer_coverage_and_composite_observer_owns_privileged_probes(self):
        root = SCRIPT.parents[1]
        prefix = ("sudo -n --preserve-env=GITHUB_SHA,GITHUB_RUN_ID,GITHUB_RUN_ATTEMPT "
                  "python3 scripts/collect-stress-host-diagnostics.py ")
        workflow = yaml.safe_load((root / ".github/workflows/ci.yml").read_text())
        for job in ("images", "images-arm64", "coverage-ratchets"):
            with self.subTest(job=job):
                steps = [step for step in workflow["jobs"][job]["steps"]
                         if "collect-stress-host-diagnostics.py" in step.get("run", "")]
                self.assertEqual(len(steps), 1)
                self.assertTrue(steps[0]["run"].startswith(prefix))
        composite = yaml.safe_load((root / ".github/actions/run-integration/action.yml").read_text())
        steps = [step for step in composite["runs"]["steps"]
                 if "collect-stress-host-diagnostics.py" in step.get("run", "")]
        self.assertEqual(len(steps), 1)
        self.assertTrue(steps[0]["run"].startswith(prefix))


if __name__ == "__main__":
    unittest.main()
