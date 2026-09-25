"""No-Docker checks for paired Go benchmark sampling and exit records."""

import json
import os
import runpy
import subprocess
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).resolve().parent / "performance-benchmarks.sh"
PARSE_BENCH = runpy.run_path(str(SCRIPT.parent / "compare-performance.py"))["parse_go_bench_text"]


class BenchmarkOrderTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="caesium-bench-order-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.artifacts = self.root / "artifacts"
        self.candidate = self.root / "candidate"
        self.base = self.root / "base"
        self.candidate.mkdir()
        self.base.mkdir()
        for relpath, name in (
            ("internal/run/owner_benchmark_test.go", "BenchmarkOwnerFake"),
            ("internal/run/recovery_benchmark_test.go", "BenchmarkRecoverFake"),
        ):
            for source in (self.candidate, self.base):
                path = source / relpath
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(
                    f"package run\nimport \"testing\"\nfunc {name}(b *testing.B) {{}}\n"
                )
        fake_bin = self.root / "bin"
        fake_bin.mkdir()
        fake_docker = fake_bin / "docker"
        fake_docker.write_text(
            "#!/usr/bin/env python3\n"
            "import json, os, pathlib, sys\n"
            "args = sys.argv[1:]\n"
            "source = args[args.index('-v') + 1].split(':/bld/caesium')[0]\n"
            "side = 'candidate' if source == os.environ['FAKE_CANDIDATE'] else 'base'\n"
            "if 'go test -c' in args[-1]:\n"
            "    if os.environ.get('FAKE_DOCKER_PREFLIGHT_FAIL') == '1':\n"
            "        print('candidate benchmark references a missing base API')\n"
            "        sys.exit(23)\n"
            "    print('base benchmark harness compiles')\n"
            "    sys.exit(0)\n"
            "log = pathlib.Path(os.environ['FAKE_DOCKER_LOG'])\n"
            "prior = [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []\n"
            "repeat = 1 + sum(row['side'] == side for row in prior)\n"
            "with log.open('a') as out:\n"
            "    out.write(json.dumps({'side': side, 'repeat': repeat, 'args': args}) + '\\n')\n"
            "if os.environ.get('FAKE_DOCKER_FAIL') == f'{side}:{repeat}':\n"
            "    print('synthetic benchmark failure')\n"
            "    sys.exit(17)\n"
            "bad = os.environ.get('FAKE_DOCKER_BAD') == f'{side}:{repeat}'\n"
            "kind = os.environ.get('FAKE_DOCKER_BAD_KIND') if bad else None\n"
            "def row(name):\n"
            "    if kind == 'missing_memory' and name == 'BenchmarkOwnerFake':\n"
            "        print(f'{name}-10  1000  {1000 + repeat} ns/op')\n"
            "        return\n"
            "    print(f'{name}-10  1000  {1000 + repeat} ns/op  400 B/op  12 allocs/op')\n"
            "row('BenchmarkOwnerFake')\n"
            "if kind == 'duplicate': row('BenchmarkOwnerFake')\n"
            "if kind != 'missing': row('BenchmarkRecoverFake')\n"
            "if kind == 'extra': row('BenchmarkOwnerUnexpected')\n"
        )
        fake_docker.chmod(0o755)
        self.log = self.root / "docker-calls.jsonl"
        self.env = os.environ.copy()
        self.env["PATH"] = str(fake_bin) + os.pathsep + self.env.get("PATH", "")
        self.env["FAKE_CANDIDATE"] = str(self.candidate)
        self.env["FAKE_DOCKER_LOG"] = str(self.log)

    def run_pair(self, *, fail=None, bad=None, bad_kind=None, preflight_fail=False):
        env = self.env.copy()
        if fail:
            env["FAKE_DOCKER_FAIL"] = fail
        if bad:
            env["FAKE_DOCKER_BAD"] = bad
            env["FAKE_DOCKER_BAD_KIND"] = bad_kind
        if preflight_fail:
            env["FAKE_DOCKER_PREFLIGHT_FAIL"] = "1"
        return subprocess.run(
            ["bash", str(SCRIPT), "4", "linux/arm64", str(self.candidate),
             str(self.base), "builder:candidate", "builder:base", str(self.artifacts)],
            env=env, capture_output=True, text=True, check=False,
        )

    def calls(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()]

    def test_one_sample_per_side_per_repeat_alternates_first_side(self):
        result = self.run_pair()
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = self.calls()
        self.assertEqual(
            [(row["side"], row["repeat"]) for row in calls],
            [("base", 1), ("candidate", 1), ("candidate", 2), ("base", 2),
             ("base", 3), ("candidate", 3), ("candidate", 4), ("base", 4)],
        )
        self.assertEqual(
            (self.artifacts / "observations" / "benchmark-order.tsv").read_text(),
            "1\tbase\t0\n1\tcandidate\t0\n2\tcandidate\t0\n2\tbase\t0\n"
            "3\tbase\t0\n3\tcandidate\t0\n4\tcandidate\t0\n4\tbase\t0\n",
        )
        for row in calls:
            args = row["args"]
            self.assertIn("-count=1", args[-1])
            self.assertIn("-bench='^Benchmark(Owner|Recover)'", args[-1])
            self.assertEqual(args[args.index("--platform") + 1], "linux/arm64")
            self.assertEqual(args[-4], "builder:" + row["side"])
        for side in ("base", "candidate"):
            prefix = self.artifacts / side / "bench.txt"
            parsed = PARSE_BENCH(prefix.read_text())
            self.assertEqual(set(parsed), {"BenchmarkOwnerFake", "BenchmarkRecoverFake"})
            for benchmark in parsed.values():
                self.assertEqual(len(benchmark["ns_per_op"]), 4)
            self.assertEqual(Path(str(prefix) + ".exit").read_text(), "0\n")
            self.assertEqual(
                Path(str(prefix) + ".repeats.tsv").read_text(),
                "1\t0\n2\t0\n3\t0\n4\t0\n",
            )

    def test_failed_sample_is_recorded_and_later_repeats_still_run(self):
        result = self.run_pair(fail="candidate:2")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(self.calls()), 8)
        candidate = self.artifacts / "candidate" / "bench.txt"
        base = self.artifacts / "base" / "bench.txt"
        self.assertEqual(Path(str(candidate) + ".exit").read_text(), "17\n")
        self.assertEqual(Path(str(base) + ".exit").read_text(), "0\n")
        self.assertEqual(
            Path(str(candidate) + ".repeats.tsv").read_text(),
            "1\t0\n2\t17\n3\t0\n4\t0\n",
        )
        order = (self.artifacts / "observations" / "benchmark-order.tsv").read_text()
        self.assertIn("2\tcandidate\t17\n", order)
        self.assertIn("synthetic benchmark failure", candidate.read_text())
        for benchmark in PARSE_BENCH(candidate.read_text()).values():
            self.assertEqual(len(benchmark["ns_per_op"]), 3)

    def test_base_compile_failure_is_recorded_before_paired_samples(self):
        result = self.run_pair(preflight_fail=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("benchmark harness incompatible with base", result.stderr)
        self.assertEqual((self.artifacts / "observations/benchmark-base-compile.exit").read_text(), "23\n")
        self.assertIn("missing base API", (self.artifacts / "observations/benchmark-base-compile.txt").read_text())
        self.assertEqual(len(self.calls()), 8)

    def test_zero_exit_missing_benchmark_row_fails_closed(self):
        result = self.run_pair(bad="candidate:2", bad_kind="missing")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(self.calls()), 8)
        candidate = self.artifacts / "candidate" / "bench.txt"
        self.assertEqual(Path(str(candidate) + ".exit").read_text(), "65\n")
        self.assertIn("2\t65\n", Path(str(candidate) + ".repeats.tsv").read_text())
        self.assertIn("missing=['BenchmarkRecoverFake']", candidate.read_text())
        self.assertEqual(
            len(PARSE_BENCH(candidate.read_text())["BenchmarkRecoverFake"]["ns_per_op"]), 3
        )

    def test_zero_exit_duplicate_benchmark_row_fails_closed(self):
        result = self.run_pair(bad="base:3", bad_kind="duplicate")
        self.assertEqual(result.returncode, 0, result.stderr)
        base = self.artifacts / "base" / "bench.txt"
        self.assertEqual(Path(str(base) + ".exit").read_text(), "65\n")
        self.assertIn("duplicate=['BenchmarkOwnerFake']", base.read_text())

    def test_zero_exit_extra_benchmark_row_fails_closed(self):
        result = self.run_pair(bad="candidate:1", bad_kind="extra")
        self.assertEqual(result.returncode, 0, result.stderr)
        candidate = self.artifacts / "candidate" / "bench.txt"
        self.assertEqual(Path(str(candidate) + ".exit").read_text(), "65\n")
        self.assertIn("extra=['BenchmarkOwnerUnexpected']", candidate.read_text())

    def test_zero_exit_missing_benchmem_metrics_fails_closed(self):
        result = self.run_pair(bad="candidate:2", bad_kind="missing_memory")
        self.assertEqual(result.returncode, 0, result.stderr)
        candidate = self.artifacts / "candidate" / "bench.txt"
        self.assertEqual(Path(str(candidate) + ".exit").read_text(), "65\n")
        self.assertIn("benchmark row lacks -benchmem metrics", candidate.read_text())

    def test_benchmark_harness_mismatch_stops_before_measurement(self):
        path = self.base / "internal/run/owner_benchmark_test.go"
        path.write_text(path.read_text().replace("BenchmarkOwnerFake", "BenchmarkOwnerDifferent"))
        result = self.run_pair()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("benchmark harness differs between sides", result.stderr)
        self.assertFalse(self.log.exists())


if __name__ == "__main__":
    unittest.main()
