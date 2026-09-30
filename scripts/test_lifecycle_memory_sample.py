"""Hermetic cgroup and /metrics fixtures for the F2 snapshot memory probe.

No docker, kind, or live caesium process. A missing sample is an evidence gap,
not a snapshot pass, and the lifecycle chart limit stays 1Gi.
"""

import importlib.util
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = Path(__file__).with_name("lifecycle-memory-sample.py")
CONTROLLER = ROOT / "scripts/lifecycle-tests.sh"
VALUES = ROOT / "helm/caesium/ci/test-values-lifecycle.yaml"

METRICS = """\
# TYPE go_memstats_heap_alloc_bytes gauge
go_memstats_heap_alloc_bytes 12345670
# TYPE go_memstats_heap_inuse_bytes gauge
go_memstats_heap_inuse_bytes 2.5e+07
# TYPE go_memstats_last_gc_time_seconds gauge
go_memstats_last_gc_time_seconds 1700000001
# TYPE go_gc_duration_seconds summary
go_gc_duration_seconds{quantile="0"} 1e-05
go_gc_duration_seconds{quantile="1"} 0.002
go_gc_duration_seconds_sum 0.01
go_gc_duration_seconds_count 4
# TYPE process_start_time_seconds gauge
process_start_time_seconds 1700000100
"""

PARTIAL_METRICS = """\
# HELP go_memstats_heap_alloc_bytes heap bytes in use
# TYPE go_memstats_heap_alloc_bytes gauge
go_memstats_heap_alloc_bytes 4096
"""


def load_module():
    spec = importlib.util.spec_from_file_location("lifecycle_memory_sample", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class MemoryProbeFixtureTest(unittest.TestCase):
    def setUp(self):
        self.module = load_module()
        self.tmp = tempfile.TemporaryDirectory(prefix="caesium-f2-memory-")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.lifecycle_id = "lifecycle-memory"

    def write(self, relative, text):
        path = self.root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)
        return path

    def sample(self, member="caesium-0", reason="cadence", batch=13, env=None, command=None,
               timeout=5):
        capture = self.root / f"capture-{member}-{reason}-{batch}.txt"
        jsonl = self.root / "snapshot-memory-samples.jsonl"
        argv = [
            sys.executable, str(SCRIPT), "sample",
            "--member", member, "--reason", reason, "--batch", str(batch),
            "--lifecycle-id", self.lifecycle_id, "--jsonl", str(jsonl),
            "--capture", str(capture), "--artifact-root", str(self.root),
            "--timeout", str(timeout), "--timestamp", "2026-09-26T00:00:00Z",
            "--round", "1", "--container-id", "abc123", "--restart-count", "0",
            "--", *(command or ["/bin/sh", "-s"]),
        ]
        subprocess.run(argv, env=env, check=True, capture_output=True, text=True)
        return json.loads(jsonl.read_text().splitlines()[-1])

    def probe_env(self, metrics_path=None, path=None):
        env = os.environ.copy()
        env["CAESIUM_MEMORY_PROBE_ROOT"] = str(self.root)
        if metrics_path is None:
            env.pop("CAESIUM_MEMORY_PROBE_METRICS_FILE", None)
        else:
            env["CAESIUM_MEMORY_PROBE_METRICS_FILE"] = str(metrics_path)
        if path is not None:
            env["PATH"] = path
        return env

    def v2_tree(self, metrics=METRICS):
        self.write("sys/fs/cgroup/memory.current", "734000000\n")
        self.write("sys/fs/cgroup/memory.stat", "anon 700000000\nfile 20000000\nfile_mapped 99\nslab 123\n")
        self.write("proc/1/comm", "caesium\n")
        self.write("proc/1/status", "Name:\tcaesium\nVmRSS:\t    4096 kB\n")
        return self.write("metrics.txt", metrics)

    def test_cgroup_v2_probe_records_anon_file_rss_and_go_series(self):
        metrics = self.v2_tree()
        sample = self.sample(env=self.probe_env(metrics))
        self.assertEqual(sample["member"], "caesium-0")
        self.assertEqual(sample["timestamp"], "2026-09-26T00:00:00Z")
        self.assertEqual(sample["timestamp_source"], "host-utc")
        self.assertEqual(sample["source"], "kubectl-exec")
        self.assertEqual(sample["reason"], "cadence")
        self.assertEqual(sample["batch"], 13)
        self.assertEqual(sample["memory_limit"], "1Gi")
        self.assertTrue(sample["readings_ok"])
        self.assertEqual(sample["reading_gaps"], [])
        cgroup = sample["cgroup"]
        self.assertEqual(cgroup["version"], "v2")
        self.assertEqual(cgroup["source_paths"]["memory.current"], "/sys/fs/cgroup/memory.current")
        self.assertEqual(cgroup["source_paths"]["memory.stat"], "/sys/fs/cgroup/memory.stat")
        self.assertEqual(cgroup["memory_current_bytes"], 734000000)
        self.assertEqual(cgroup["anon_bytes"], 700000000)
        self.assertEqual(cgroup["file_bytes"], 20000000)
        self.assertEqual(cgroup["stat_bytes"]["file_mapped"], 99)
        self.assertEqual(cgroup["absent"], [])
        rss = sample["process_rss"]
        self.assertEqual(rss["pid"], 1)
        self.assertTrue(rss["matches_caesium"])
        self.assertEqual(rss["rss_kib"], 4096)
        self.assertEqual(rss["rss_bytes"], 4194304)
        self.assertEqual(rss["source"], "/proc/1/status")
        go = sample["go"]
        self.assertEqual(go["metrics_status"], "present")
        self.assertEqual(go["heap_alloc_bytes"], {
            "series": "go_memstats_heap_alloc_bytes", "present": True, "value": 12345670})
        self.assertEqual(go["heap_inuse_bytes"]["value"], 25000000)
        self.assertEqual(go["last_gc_time_seconds"]["series"], "go_memstats_last_gc_time_seconds")
        self.assertEqual(go["last_gc_time_seconds"]["value"], 1700000001)
        self.assertTrue(go["gc_duration_seconds"]["present"])
        self.assertEqual(go["gc_duration_seconds"]["count"], 4)
        self.assertAlmostEqual(go["gc_duration_seconds"]["sum"], 0.01)
        self.assertAlmostEqual(go["gc_duration_seconds"]["quantiles"]["0"], 1e-05)
        self.assertEqual(go["absent_series"], [])

    def test_host_cgroup_namespace_uses_proc_cgroup_path(self):
        nested = "sys/fs/cgroup/kubepods.slice/pod.slice/container.scope"
        self.write("proc/1/cgroup", "0::/kubepods.slice/pod.slice/container.scope\n")
        self.write(f"{nested}/memory.current", "42\n")
        self.write(f"{nested}/memory.stat", "anon 10\nfile 32\n")
        self.write("proc/1/comm", "caesium\n")
        self.write("proc/1/status", "VmRSS: 1 kB\n")
        metrics = self.write("metrics.txt", METRICS)
        sample = self.sample(env=self.probe_env(metrics))
        self.assertEqual(sample["cgroup"]["version"], "v2")
        self.assertEqual(sample["cgroup"]["memory_current_bytes"], 42)
        self.assertEqual(sample["cgroup"]["anon_bytes"], 10)
        self.assertEqual(sample["cgroup"]["file_bytes"], 32)
        self.assertEqual(
            sample["cgroup"]["source_paths"]["memory.current"],
            "/sys/fs/cgroup/kubepods.slice/pod.slice/container.scope/memory.current")
        self.assertTrue(sample["readings_ok"])

    def test_cgroup_v1_records_usage_and_absent_anon_file(self):
        self.write("sys/fs/cgroup/memory/memory.usage_in_bytes", "1073741824\n")
        self.write("sys/fs/cgroup/memory/memory.stat",
                   "cache 4096\nrss 8192\nrss_huge 0\nmapped_file 0\nswap 0\n")
        self.write("proc/1/comm", "caesium\n")
        self.write("proc/1/status", "Name:\tcaesium\nVmRSS:\t  2048 kB\n")
        metrics = self.write("metrics.txt", METRICS)
        sample = self.sample(member="caesium-1", batch=0, env=self.probe_env(metrics))
        cgroup = sample["cgroup"]
        self.assertEqual(cgroup["version"], "v1")
        self.assertEqual(cgroup["memory_usage_bytes"], 1073741824)
        self.assertIsNone(cgroup["memory_current_bytes"])
        self.assertIsNone(cgroup["anon_bytes"])
        self.assertIsNone(cgroup["file_bytes"])
        self.assertEqual(cgroup["v1_rss_bytes"], 8192)
        self.assertEqual(cgroup["v1_cache_bytes"], 4096)
        self.assertIn("memory.stat anon", cgroup["absent"])
        self.assertIn("memory.stat file", cgroup["absent"])
        self.assertIn("memory.current", cgroup["absent"])
        self.assertEqual(cgroup["source_paths"]["memory.usage_in_bytes"],
                         "/sys/fs/cgroup/memory/memory.usage_in_bytes")
        self.assertEqual(cgroup["source_paths"]["memory.stat"],
                         "/sys/fs/cgroup/memory/memory.stat")
        self.assertEqual(sample["process_rss"]["rss_bytes"], 2048 * 1024)
        self.assertTrue(sample["readings_ok"])
        self.assertEqual(sample["member"], "caesium-1")

    def test_missing_go_series_are_recorded_absent(self):
        metrics = self.v2_tree(PARTIAL_METRICS)
        sample = self.sample(env=self.probe_env(metrics))
        go = sample["go"]
        self.assertEqual(go["metrics_status"], "present")
        self.assertEqual(go["metrics_error"], "")
        self.assertTrue(go["heap_alloc_bytes"]["present"])
        self.assertEqual(go["heap_alloc_bytes"]["value"], 4096)
        self.assertEqual(go["heap_inuse_bytes"], {
            "series": "go_memstats_heap_inuse_bytes", "present": False})
        self.assertEqual(go["last_gc_time_seconds"], {
            "series": "go_memstats_last_gc_time_seconds", "present": False})
        self.assertEqual(go["gc_duration_seconds"], {
            "series": "go_gc_duration_seconds", "present": False})
        self.assertCountEqual(go["absent_series"], [
            "go_memstats_heap_inuse_bytes",
            "go_memstats_last_gc_time_seconds",
            "go_gc_duration_seconds",
            "process_start_time_seconds",
        ])
        self.assertTrue(sample["readings_ok"])
        self.assertEqual(sample["reading_gaps"], [])

    def test_metrics_client_absent_records_every_series_missing(self):
        self.v2_tree()
        bindir = self.root / "bin"
        bindir.mkdir()
        for name in ("cat", "tr"):
            os.symlink(shutil.which(name), bindir / name)
        sample = self.sample(env=self.probe_env(path=str(bindir)))
        go = sample["go"]
        self.assertEqual(go["metrics_status"], "absent")
        self.assertIn("no wget or curl", go["metrics_error"])
        for key in ("heap_alloc_bytes", "heap_inuse_bytes", "last_gc_time_seconds", "gc_duration_seconds"):
            self.assertFalse(go[key]["present"], key)
        self.assertIn("go_metrics", sample["reading_gaps"])
        self.assertFalse(sample["readings_ok"])
        self.assertEqual(sample["cgroup"]["anon_bytes"], 700000000)
        self.assertEqual(sample["process_rss"]["rss_bytes"], 4194304)

    def test_unreadable_capture_is_a_sample_and_not_a_pass(self):
        sample = self.sample(command=["/bin/sh", "-c", "printf 'not-a-probe\\n'; sleep 30"], timeout=1)
        self.assertEqual(sample["capture_exit_code"], 124)
        self.assertEqual((self.root / "capture-caesium-0-cadence-13.txt").read_text().count("not-a-probe"), 1)
        self.assertFalse(sample["readings_ok"])
        self.assertEqual(sample["reading_gaps"], ["cgroup", "process_rss", "go_metrics"])
        self.assertIn("probe format marker missing", sample["capture_error"])
        self.assertNotIn("member-sample-format", sample["capture_error"])
        document, code = self.module.publish(
            samples=[sample], lifecycle_id=self.lifecycle_id, batches=[13],
            apply_batch=None, evidence={"truncation_proved": True, "batch": 13})
        self.assertEqual(code, 2)
        self.assertTrue(document["truncation_proved"])
        self.assertTrue(document["memory_samples"]["gap"])
        self.assertIn("no in-write memory sample", document["memory_samples"]["gap_detail"])
        self.assertEqual(document["memory_samples"]["memory_limit"], "1Gi")

    def test_publish_requires_both_survivors_and_preserves_blocked_evidence(self):
        metrics = self.v2_tree()
        first = self.sample(member="caesium-0", batch=13, env=self.probe_env(metrics))
        evidence = {
            "lifecycle_id": self.lifecycle_id, "batch": 13,
            "disputed_write_readback": {"status": "unavailable"},
        }
        document, code = self.module.publish(
            samples=[first], lifecycle_id=self.lifecycle_id, batches=[13],
            apply_batch=13, evidence=evidence)
        self.assertEqual(code, 2)
        self.assertEqual(document["disputed_write_readback"]["status"], "unavailable")
        self.assertIn("caesium-1 batch 13 has no memory sample", document["memory_samples"]["gap_detail"])
        self.assertIn("caesium-1 batch 13 has no post-apply-error memory sample",
                      document["memory_samples"]["gap_detail"])
        second = self.sample(member="caesium-1", reason="apply-error", batch=13,
                             env=self.probe_env(metrics))
        cadence_only = dict(second, reason="cadence")
        document, code = self.module.publish(
            samples=[first, cadence_only], lifecycle_id=self.lifecycle_id, batches=[13],
            apply_batch=13, evidence=evidence)
        self.assertEqual(code, 2)
        self.assertIn("post-apply-error", document["memory_samples"]["gap_detail"])
        failed_after = dict(second, readings_ok=False, reading_gaps=["cgroup"])
        first_error = dict(first, reason="apply-error")
        document, code = self.module.publish(
            samples=[first, failed_after, second, first_error], lifecycle_id=self.lifecycle_id,
            batches=[13], apply_batch=13, evidence=evidence)
        self.assertEqual(code, 0)
        self.assertFalse(document["memory_samples"]["gap"])
        self.assertEqual(document["memory_samples"]["sample_count"], 4)
        self.assertEqual(len(document["memory_samples"]["samples"]), 4)

    def test_finish_cli_writes_gap_without_dropping_samples(self):
        jsonl = self.root / "samples.jsonl"
        jsonl.write_text(json.dumps({
            "lifecycle_id": "other", "member": "caesium-0", "batch": 0,
            "reason": "cadence", "readings_ok": True,
        }) + "\n")
        dest = self.root / "case.json"
        result = subprocess.run([
            sys.executable, str(SCRIPT), "finish",
            "--lifecycle-id", self.lifecycle_id, "--jsonl", str(jsonl),
            "--dest", str(dest), "--batches", "0", "--apply-batch", "",
            "--evidence", str(self.root / "missing.json"),
        ], capture_output=True, text=True)
        self.assertEqual(result.returncode, 3)
        document = json.loads(dest.read_text())
        self.assertEqual(document["case_evidence_error"], "evidence file missing")
        self.assertTrue(document["memory_samples"]["gap"])
        self.assertEqual(document["memory_samples"]["samples"], [])
        self.assertEqual(document["memory_limit"], "1Gi")

    def test_pre_write_round_and_empty_go_scrape_are_gaps(self):
        metrics = self.v2_tree(metrics="\n")
        early = self.sample(env=self.probe_env(metrics))
        self.assertFalse(early["readings_ok"])
        self.assertIn("go_metrics", early["reading_gaps"])
        during = dict(early, round=0, readings_ok=True, reading_gaps=[])
        document, code = self.module.publish(
            samples=[during, dict(during, member="caesium-1")],
            lifecycle_id=self.lifecycle_id, batches=[13], apply_batch=None, evidence={"ok": True})
        self.assertEqual(code, 2)
        self.assertIn("no in-write memory sample", document["memory_samples"]["gap_detail"])
        document, code = self.module.publish(
            samples=[], lifecycle_id=self.lifecycle_id, batches=[], apply_batch=None, evidence={"ok": True})
        self.assertEqual(code, 2)
        self.assertIn("no catalog-write batch", document["memory_samples"]["gap_detail"])

    def test_corrupt_pass_evidence_does_not_publish_a_pass(self):
        metrics = self.v2_tree()
        first = self.sample(member="caesium-0", batch=0, env=self.probe_env(metrics))
        second = self.sample(member="caesium-1", batch=0, env=self.probe_env(metrics))
        bad = self.root / "truncated.json"
        bad.write_text("{")
        dest = self.root / "case.json"
        jsonl = self.root / "snapshot-memory-samples.jsonl"
        result = subprocess.run([
            sys.executable, str(SCRIPT), "finish",
            "--lifecycle-id", self.lifecycle_id, "--jsonl", str(jsonl),
            "--dest", str(dest), "--batches", "0", "--evidence", str(bad),
        ], capture_output=True, text=True)
        self.assertEqual(result.returncode, 3)
        document = json.loads(dest.read_text())
        self.assertIsNone(document["case_evidence"])
        self.assertIn("case_evidence_error", document)
        self.assertTrue(first["readings_ok"] and second["readings_ok"])

    def test_controller_blocks_a_pass_and_keeps_a_failed_write_rc(self):
        art = self.root / "art"
        art.mkdir()
        (art / "cluster-logs").mkdir()
        evidence = art / "proof.json"
        evidence.write_text('{"truncation_proved": true}\n')
        script = f'''
set -euo pipefail
ROOT={ROOT}
source "$ROOT/scripts/lifecycle-snapshot-phase.sh"
lc_case() {{ printf '%s\\n' "$2" > "$LC_ART/status"; }}
lc_phase() {{ printf '%s\\n' 7 > "$LC_PHASE_DONE"; return 7; }}
lc_base() {{ printf '%s\\n' base; }}
lc_memory_sample_members() {{ :; }}
LC_ART={art}
LC_ID={self.lifecycle_id}
LC_CAND_ID=cand
LC_MEM_ACTIVE=0
LC_MEM_BATCHES=
LC_MEM_APPLY_BATCH=
LC_MEM_SEQ=0
LC_MEM_INTERVAL=0
LC_MEM_SAMPLE_CAP=1
LC_PHASE_PID=
LC_SNAP_RC=0
mkdir -p "$LC_ART/cluster-logs"
lc_run_snapshot_phase 0
[[ "$LC_SNAP_RC" == 7 ]]
: > "$LC_ART/cluster-logs/snapshot-memory-samples.jsonl"
LC_MEM_ACTIVE=1
LC_MEM_BATCHES=0
lc_snapshot_case pass "would pass" {evidence}
[[ "$(cat "$LC_ART/status")" == blocked ]]
[[ "$LC_SNAP_RC" == 1 ]]
'''
        result = subprocess.run(["bash", "-c", script], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((art / "status").read_text().strip(), "blocked")

    def test_phase_group_kill_reaps_the_runner_child(self):
        art = self.root / "group"
        art.mkdir()
        script = f'''
set -euo pipefail
ROOT={ROOT}
source "$ROOT/scripts/lifecycle-snapshot-phase.sh"
set -m
( sleep 30 & echo $! > {art}/child; wait ) &
pgid=$!
sleep 0.2
lc_stop_phase_group "$pgid"
child=$(cat {art}/child)
if kill -0 "$child" 2>/dev/null; then
  echo "child still alive" >&2
  exit 1
fi
'''
        result = subprocess.run(["bash", "-c", script], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        controller = CONTROLLER.read_text()
        self.assertIn('lc_stop_phase_group "${LC_PHASE_PID:-}"', controller)
        self.assertIn("lc_stop_phase_group \"$LC_PHASE_PID\"", controller)

    def test_lifecycle_chart_keeps_1gi_limit_and_topology(self):
        text = VALUES.read_text()
        self.assertEqual(re.findall(r"(?m)^[ \t]*memory:\s*(\S+)\s*$", text), ["128Mi", "1Gi"])
        self.assertIn("replicaCount: 3", text)
        self.assertIn('name: CAESIUM_DATABASE_SHARDS', text)
        self.assertIn('name: CAESIUM_DATABASE_VOTERS', text)
        self.assertRegex(text, r'name: CAESIUM_DATABASE_SHARDS\n\s+value: "1"')
        self.assertRegex(text, r'name: CAESIUM_DATABASE_VOTERS\n\s+value: "3"')
        controller = CONTROLLER.read_text()
        self.assertNotIn("resources.limits.memory", controller)
        self.assertNotIn("--set resources", controller)


if __name__ == "__main__":
    unittest.main()
