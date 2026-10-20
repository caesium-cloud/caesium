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


def controlled_journal_commands(directory, kernel, units=(), behavior="normal"):
    """A real executable applying journalctl's MESSAGE/field/case filters."""
    dataset = directory / "journal.json"
    dataset.write_text(json.dumps({"kernel": kernel, "units": units, "behavior": behavior}))
    arguments = directory / "journal-arguments.jsonl"
    journalctl = directory / "journalctl"
    journalctl.write_text(f"#!{sys.executable}\n" + f"DATASET = {str(dataset)!r}\nARGUMENTS = {str(arguments)!r}\n" + r'''
import datetime
import json
import re
import sys
import time

args = sys.argv[1:]
with open(ARGUMENTS, "a") as output:
    output.write(json.dumps(args) + "\n")
with open(DATASET) as stream:
    dataset = json.load(stream)
if "-k" in args and dataset["behavior"] == "timeout":
    time.sleep(60)
if "-k" in args and dataset["behavior"] == "failure":
    sys.exit(7)
pattern = args[args.index("--grep") + 1] if "--grep" in args else None
case_sensitive = "--case-sensitive=yes" in args
selected = next((arg.split("=", 1)[1].split(",") for arg in args if arg.startswith("--output-fields=")), None)
# journalctl retains these small metadata fields even with --output-fields.
always = ("__CURSOR", "__REALTIME_TIMESTAMP", "__MONOTONIC_TIMESTAMP", "_BOOT_ID")
since = int(args[args.index("--since") + 1][1:]) * 1_000_000
until = datetime.datetime.strptime(args[args.index("--until") + 1], "%Y-%m-%d %H:%M:%S.%f UTC").replace(tzinfo=datetime.timezone.utc).timestamp() * 1_000_000
unit_names = [args[index + 1] for index, arg in enumerate(args) if arg == "-u"]
for record in dataset["kernel" if "-k" in args else "units"]:
    if "-k" in args and record.get("_TRANSPORT") != "kernel":
        continue
    if "-k" not in args and record.get("_SYSTEMD_UNIT") not in unit_names:
        continue
    stamp = record.get("__REALTIME_TIMESTAMP", "")
    if stamp.isdigit() and not since <= int(stamp) <= until:
        continue
    message = record.get("MESSAGE")
    if pattern is not None and (not isinstance(message, str) or not re.search(pattern, message, 0 if case_sensitive else re.I)):
        continue
    if selected is not None:
        record = {key: value for key, value in record.items() if key in selected or key in always}
    print(json.dumps(record), flush=True)
''')
    docker = directory / "docker"
    docker.write_text(f"#!{sys.executable}\nprint('{{}}')\n")
    systemctl = directory / "systemctl"
    systemctl.write_text(f"#!{sys.executable}\nimport sys\nprint('inactive')\nsys.exit(3)\n")
    for command in (journalctl, docker, systemctl):
        command.chmod(0o755)
    return arguments


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


class JournalPrefilterTests(unittest.TestCase):
    def setUp(self):
        privileged = mock.patch.object(DIAGNOSTICS.os, "geteuid", return_value=0)
        privileged.start()
        self.addCleanup(privileged.stop)

    def test_real_prefilter_preserves_owned_evidence_amid_foreign_noise_and_large_metadata(self):
        second = "c" * 64
        noise = [json.loads(journal(f"foreign {FOREIGN} " + "noise" * 300)) for _ in range(60)]
        owned = json.loads(journal(f"oom-kill:constraint=CONSTRAINT_MEMCG,task_memcg=/docker/{CID} csk_owned-secret"))
        owned["PRIVATE_METADATA"] = "private-metadata-value" * 10000
        owned["__CURSOR"] = "cursor"
        owned["__MONOTONIC_TIMESTAMP"] = "100"
        owned["_BOOT_ID"] = "boot"
        another = json.loads(journal(f"container {second} disconnected"))
        units = [json.loads(journal(f"shim {second} disconnected", source="units", unit="containerd.service"))]
        self.assertGreater(len(json.dumps(noise)), DIAGNOSTICS.MAX_BYTES)
        self.assertGreater(len(json.dumps(owned)), DIAGNOSTICS.MAX_BYTES)
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            arguments = controlled_journal_commands(directory, noise + [owned, another], units)
            smoke = directory / "smoke.log"
            smoke.write_text(f"stress smoke: cid={CID} status=exited\nstress smoke: cid={second}\n")
            with mock.patch.dict(os.environ, {"PATH": tmp}):
                report = DIAGNOSTICS.collect(smoke, str(SINCE), now=UNTIL)
            calls = [json.loads(line) for line in arguments.read_text().splitlines()]
        self.assertEqual(report["classification"], "memcg_oom")
        self.assertEqual(report["qualification"], "unchanged")
        self.assertEqual(len(report["records"]), 3)
        self.assertEqual({cid for record in report["records"] for cid in record["container_ids"]}, {CID, second})
        for probe in ("kernel", "units"):
            self.assertEqual(report["probes"][probe], {"exit_code": 0, "rejected_records": 0})
        self.assertEqual(len(calls), 2)
        for call in calls:
            self.assertIn("--output-fields=MESSAGE,_TRANSPORT,_SYSTEMD_UNIT,__REALTIME_TIMESTAMP", call)
            self.assertIn("--case-sensitive=yes", call)
            pattern = call[call.index("--grep") + 1]
            self.assertIn(CID, pattern)
            self.assertIn(second, pattern)
            self.assertNotIn(FOREIGN, pattern)
        encoded = json.dumps(report)
        for value in ("private-metadata-value", "PRIVATE_METADATA", "csk_owned-secret", "__CURSOR", "_BOOT_ID"):
            self.assertNotIn(value, encoded)
        self.assertIn("[REDACTED_API_KEY]", encoded)

    def test_real_prefilter_matches_exact_case_sensitive_message_tokens_and_ignores_injection_text(self):
        rejected = [f"x{CID}", f"{CID}x", f"0{CID}", f"{CID}0", CID.upper(), FOREIGN]
        records = [json.loads(journal(f"constraint=CONSTRAINT_MEMCG {text}")) for text in rejected]
        metadata_only = json.loads(journal("unrelated message"))
        metadata_only["CONTAINER_ID"] = CID
        records.append(metadata_only)
        valid = [f"docker-{CID}.scope", f"/docker/{CID}", f"({CID})"]
        records.extend(json.loads(journal(message)) for message in valid)
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            arguments = controlled_journal_commands(directory, records)
            smoke = directory / "smoke.log"
            # Extra smoke text is never interpolated into the PCRE expression.
            smoke.write_text(f"stress smoke: cid={CID} (?i:.*)|{FOREIGN} --output=verbose\n")
            with mock.patch.dict(os.environ, {"PATH": tmp}):
                report = DIAGNOSTICS.collect(smoke, str(SINCE), now=UNTIL)
            calls = [json.loads(line) for line in arguments.read_text().splitlines()]
        self.assertEqual(report["classification"], "unknown")
        self.assertEqual([record["message"] for record in report["records"]], valid)
        self.assertEqual(report["probes"]["kernel"]["rejected_records"], 0)
        for call in calls:
            pattern = call[call.index("--grep") + 1]
            self.assertNotIn("(?i:", pattern)
            self.assertNotIn(FOREIGN, pattern)
            self.assertNotIn("--output=verbose", pattern)

    def test_invalid_and_regex_injected_smoke_identities_never_start_commands(self):
        identities = [CID + "f", "x" + CID, CID.upper(), "(" + CID + ")", CID + "|" + FOREIGN,
                      CID[:63] + ".", CID + "(?i:.*)", CID + "$(touch should-not-exist)"]
        for identity in identities:
            with self.subTest(identity=identity), tempfile.TemporaryDirectory() as tmp:
                directory = Path(tmp)
                arguments = controlled_journal_commands(directory, [])
                smoke = directory / "smoke.log"
                smoke.write_text(f"stress smoke: cid={identity}\n")
                with mock.patch.dict(os.environ, {"PATH": tmp}):
                    report = DIAGNOSTICS.collect(smoke, str(SINCE), now=UNTIL)
                self.assertEqual(report["error"], "smoke_identity_unproved")
                self.assertEqual(report["classification"], "unknown")
                self.assertEqual(report["probes"], {})
                self.assertFalse(arguments.exists())

    def test_owned_overflow_record_limits_timeout_and_failures_still_remain_unknown(self):
        self.assertEqual(DIAGNOSTICS.MAX_BYTES, 65536)
        self.assertEqual(DIAGNOSTICS.PROBE_SECONDS, 3)
        cases = [
            ("oversize", [json.loads(journal(CID + " " + "x" * 65536))], "normal", "oversize"),
            ("record_limit", [json.loads(journal(CID))] * 101, "normal", "invalid_journal"),
            ("timeout", [], "timeout", "timeout"),
            ("failure", [], "failure", "exit_nonzero"),
            ("message_limit", [json.loads(journal(CID + " " + "x" * 2048))], "normal", None),
        ]
        for name, records, behavior, expected in cases:
            with self.subTest(case=name), tempfile.TemporaryDirectory() as tmp:
                directory = Path(tmp)
                controlled_journal_commands(directory, records, behavior=behavior)
                smoke = directory / "smoke.log"
                smoke.write_text(f"stress smoke: cid={CID}\n")
                with mock.patch.dict(os.environ, {"PATH": tmp}):
                    report = DIAGNOSTICS.collect(smoke, str(SINCE), now=UNTIL)
                self.assertEqual(report["classification"], "unknown")
                self.assertEqual(report["qualification"], "unchanged")
                self.assertEqual(report["records"], [])
                status = report["probes"]["kernel"]
                self.assertEqual(status.get("error"), expected)
                if name == "failure":
                    self.assertEqual(status["exit_code"], 7)
                elif name == "message_limit":
                    self.assertEqual(status["rejected_records"], 1)


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
