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
        fake_bin = self.root / "bin"
        fake_bin.mkdir()
        fake_docker = fake_bin / "docker"
        fake_docker.write_text(
            "#!/usr/bin/env python3\n"
            "import json, os, pathlib, sys\n"
            "args = sys.argv[1:]\n"
            "source = args[args.index('-v') + 1].split(':/bld/caesium')[0]\n"
            "side = 'candidate' if source == os.environ['FAKE_CANDIDATE'] else 'base'\n"
            "log = pathlib.Path(os.environ['FAKE_DOCKER_LOG'])\n"
            "prior = [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []\n"
            "repeat = 1 + sum(row['side'] == side for row in prior)\n"
            "with log.open('a') as out:\n"
            "    out.write(json.dumps({'side': side, 'repeat': repeat, 'args': args}) + '\\n')\n"
            "if os.environ.get('FAKE_DOCKER_FAIL') == f'{side}:{repeat}':\n"
            "    print('synthetic benchmark failure')\n"
            "    sys.exit(17)\n"
            "print(f'BenchmarkOwnerFake-10  1000  {1000 + repeat} ns/op  400 B/op  12 allocs/op')\n"
        )
        fake_docker.chmod(0o755)
        self.log = self.root / "docker-calls.jsonl"
        self.env = os.environ.copy()
        self.env["PATH"] = str(fake_bin) + os.pathsep + self.env.get("PATH", "")
        self.env["FAKE_CANDIDATE"] = str(self.candidate)
        self.env["FAKE_DOCKER_LOG"] = str(self.log)

    def run_pair(self, *, fail=None):
        env = self.env.copy()
        if fail:
            env["FAKE_DOCKER_FAIL"] = fail
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
            samples = PARSE_BENCH(prefix.read_text())["BenchmarkOwnerFake"]["ns_per_op"]
            self.assertEqual(len(samples), 4)
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
        samples = PARSE_BENCH(candidate.read_text())["BenchmarkOwnerFake"]["ns_per_op"]
        self.assertEqual(len(samples), 3)


if __name__ == "__main__":
    unittest.main()
