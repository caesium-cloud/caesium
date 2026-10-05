"""Hermetic checks of robustness cleanup's actual per-member log collector.

Only extracted shell functions run, with a fake kubectl executable. No cluster,
Docker daemon, network, or scenario evidence is created by these controls.
"""

import importlib.util
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import time
import unittest


ROOT = Path(__file__).resolve().parents[1]
SOURCE = (ROOT / "scripts/robustness.sh").read_text()
KC_NS = SOURCE[SOURCE.index("kc_ns() {"):SOURCE.index("\nrecord_cluster() {")]
CAPTURE = SOURCE[SOURCE.index("capture_member_logs() ("):SOURCE.index("\ncleanup() {")]
CLEANUP = SOURCE[SOURCE.index("cleanup() {"):SOURCE.index("\ntrap cleanup EXIT INT TERM")]

FAKE_KUBECTL = r'''#!/usr/bin/env python3
import json, os, sys, time
from pathlib import Path
args = sys.argv[1:]
with Path(os.environ["FAKE_CALLS"]).open("a") as calls:
    calls.write(json.dumps(args) + "\n")
pod = next((arg for arg in args if arg.startswith("pod/caesium-")), None)
if pod:
    previous = "--previous=true" in args
    print("2026-10-05T13:23:00Z member startup diagnostic " + os.environ.get("FAKE_TOKEN", ""), flush=True)
    if os.environ.get("FAKE_HANG") == "1" and pod == "pod/caesium-0" and not previous:
        time.sleep(100)
        print("must never finish", flush=True)
    if os.environ.get("FAKE_PREVIOUS_ERROR") == "1" and previous:
        print("previous terminated container unavailable", file=sys.stderr)
        raise SystemExit(7)
'''


class RobustnessDiagnosticsTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="caesium-member-log-controls-")
        self.addCleanup(self.tmp.cleanup)
        self.art = Path(self.tmp.name)
        self.tools = self.art / "tools"
        self.tools.mkdir()
        kubectl = self.tools / "kubectl"
        kubectl.write_text(FAKE_KUBECTL)
        kubectl.chmod(0o755)
        self.calls = self.art / "calls.jsonl"
        self.ledger = self.art / "owned-clusters.txt"
        self.ledger.write_text("owned-robustness\n")
        self.kubeconfig = self.art / "kubeconfig"
        self.kubeconfig.write_text("private fixture config; never read by fake tool\n")
        self.env = os.environ.copy()
        self.env.update({
            "PATH": str(self.tools) + os.pathsep + self.env["PATH"],
            "FAKE_CALLS": str(self.calls),
        })
        self.setup = "\n".join((
            "set -euo pipefail",
            "ARTIFACTS=" + shlex.quote(str(self.art)),
            "KUBECONFIG_PATH=" + shlex.quote(str(self.kubeconfig)),
            "OWNED_CLUSTERS=" + shlex.quote(str(self.ledger)),
            "ROBUSTNESS_ID=owned-robustness",
            "NAMESPACE=owned-robustness",
            "CANDIDATE_SHA=diagnostic-fixture-not-real-evidence",
            KC_NS, CAPTURE,
        ))

    def run_functions(self, ending="capture_member_logs", env=None):
        supplied = self.env.copy()
        supplied.update(env or {})
        return subprocess.run(["bash", "-c", self.setup + "\n" + ending],
                              env=supplied, text=True, capture_output=True, timeout=20)

    def manifest(self):
        return json.loads((self.art / "member-logs/capture-status.json").read_text())

    def actual_calls(self):
        return [json.loads(line) for line in self.calls.read_text().splitlines()] if self.calls.exists() else []

    def test_exact_members_context_modes_timestamps_and_bounds(self):
        result = self.run_functions()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "", "raw member logs must not reach the console")
        records = self.manifest()["captures"]
        self.assertEqual([(r["pod"], r["mode"]) for r in records],
                         [(f"caesium-{i}", mode) for i in range(3) for mode in ("current", "previous")])
        self.assertEqual(len(self.actual_calls()), 6)
        for call, record in zip(self.actual_calls(), records):
            self.assertEqual(call[:4], ["--kubeconfig", str(self.kubeconfig), "--namespace", "owned-robustness"])
            self.assertEqual(call[4:], record["arguments"])
            self.assertIn("--request-timeout=10s", call)
            self.assertIn("--tail=2000", call)
            self.assertIn("--timestamps=true", call)
            self.assertNotIn("--follow", call)
            self.assertEqual(record["exit_code"], 0)
            self.assertFalse(record["timed_out"])
            self.assertIn("member startup diagnostic", (self.art / "member-logs" / record["stdout"]).read_text())
            self.assertEqual((self.art / "member-logs" / record["stderr"]).read_text(), "")
            self.assertTrue(record["started_at"])
            self.assertTrue(record["finished_at"])
            self.assertGreaterEqual(record["duration_seconds"], 0)

    def test_unavailable_previous_records_error_and_continues_all_members(self):
        result = self.run_functions(env={"FAKE_PREVIOUS_ERROR": "1"})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "")
        self.assertEqual(result.stderr, "")
        records = self.manifest()["captures"]
        self.assertEqual(len(records), 6)
        for record in records:
            self.assertEqual(record["exit_code"], 7 if record["mode"] == "previous" else 0)
            if record["mode"] == "previous":
                self.assertIn("previous terminated container unavailable", (self.art / "member-logs" / record["stderr"]).read_text())

    def test_hung_call_is_killed_recorded_and_remaining_members_captured(self):
        started = time.monotonic()
        result = self.run_functions(env={"FAKE_HANG": "1"})
        elapsed = time.monotonic() - started
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertGreaterEqual(elapsed, 10)
        self.assertLess(elapsed, 15)
        records = self.manifest()["captures"]
        self.assertEqual(len(records), 6)
        self.assertTrue(records[0]["timed_out"])
        self.assertNotEqual(records[0]["exit_code"], 0)
        self.assertIsNotNone(records[0]["exit_code"])
        self.assertTrue(all(r["exit_code"] == 0 for r in records[1:]))
        self.assertNotIn("must never finish", (self.art / "member-logs" / records[0]["stdout"]).read_text())

    def test_mismatched_namespace_or_missing_owner_refuses_foreign_calls(self):
        for setup in ("NAMESPACE=foreign", ""):
            with self.subTest(setup=setup):
                if not setup:
                    self.ledger.write_text("foreign-cluster\n")
                result = self.run_functions(ending=setup + "\ncapture_member_logs")
                self.assertEqual(result.returncode, 1)
                self.assertEqual(self.actual_calls(), [])
                manifest = self.manifest()
                self.assertIn("scope_error", manifest)
                self.assertEqual(manifest["captures"], [])

    def test_original_failure_and_cleanup_order_survive_capture_errors(self):
        ending = '''
log() { :; }
heal_installed_partitions() { printf 'heal\\n' >>"$ARTIFACTS/cleanup-order"; }
resume_paused_task() { printf 'resume\\n' >>"$ARTIFACTS/cleanup-order"; }
restart_faulted_node() { printf 'restart\\n' >>"$ARTIFACTS/cleanup-order"; }
kc() { :; }
delete_owned_clusters() {
  [[ -f "$ARTIFACTS/member-logs/capture-status.json" ]]
  printf 'delete\\n' >>"$ARTIFACTS/cleanup-order"
}
''' + CLEANUP + "\ntrap cleanup EXIT\nexit 23"
        result = self.run_functions(ending, {"FAKE_PREVIOUS_ERROR": "1"})
        self.assertEqual(result.returncode, 23, result.stderr)
        self.assertEqual((self.art / "cleanup-order").read_text().splitlines(), ["heal", "resume", "restart", "delete"])
        self.assertEqual(len(self.manifest()["captures"]), 6)

    def test_new_logs_use_existing_recursive_token_scrubbing(self):
        token = "private-diagnostic-token-for-testing"
        result = self.run_functions(env={"FAKE_TOKEN": token})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn(token, result.stdout + result.stderr)
        (self.art / "internal-token.txt").write_text(token + "\n")
        spec = importlib.util.spec_from_file_location("robustness_diagnostic_redactor", ROOT / "scripts/collect-evidence.py")
        module = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = module
        spec.loader.exec_module(module)
        removed, scrubbed = module.redact_artifacts(self.art)
        self.assertIn("internal-token.txt", removed)
        self.assertIn("kubeconfig", removed)
        self.assertEqual(len([p for p in scrubbed if p.startswith("member-logs/")]), 6)
        for path in (self.art / "member-logs").iterdir():
            self.assertNotIn(token.encode(), path.read_bytes())


if __name__ == "__main__":
    unittest.main()
