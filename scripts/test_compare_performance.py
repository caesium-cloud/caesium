"""Fail-closed tests for scripts/compare-performance.py and the total-asset budget."""

from __future__ import annotations

import json
import os
from pathlib import Path
import runpy
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts/compare-performance.py"
SH = ROOT / "scripts/performance.sh"
MJS = ROOT / "ui/scripts/check-bundle-size.mjs"
COMPARE = runpy.run_path(str(SCRIPT))


def provenance(label="base", **overrides):
    sha = "a" * 40 if label == "base" else "b" * 40
    digest = "11" * 32 if label == "base" else "22" * 32
    body = {
        "git_sha": sha,
        "image_id": f"sha256:{digest}",
        "image_ref": f"caesiumcloud/caesium:{label}",
        "platform": "linux/arm64",
        "go_version": "go1.27.1",
        "builder_image_id": "sha256:" + "ab" * 32,
        "host_id": "perf-host-1",
        "catalog_sha256": "c" * 64,
        "settings_sha256": "d" * 64,
        "instrumented": False,
        "cli_digest": "sha256:" + "ee" * 32,
        "toolchain_id": "caesiumcloud/caesium-builder:latest@sha256:abcd",
    }
    body.update(overrides)
    return body


def around(center, n=10, spread=2.0):
    """Deterministic low-noise samples clustered around center."""
    return [center + spread * ((i % 5) - 2) / 2.0 for i in range(n)]


def side(label, **overrides):
    body = {
        "label": label,
        "provenance": provenance(label),
        "correctness": {"ok": True, "failures": []},
        "workloads": {
            "closed-baseline": {
                "phase": "warm",
                "samples": around(100.0 if label == "base" else 100.0),
            }
        },
        "benchmarks": {
            "BenchmarkOwnerApplyCompletionLinear64": {
                "ns_per_op": around(10_000.0 if label == "base" else 10_000.0)
            }
        },
        "browser": {
            "route_readiness_ms": {"/jobs": around(80.0 if label == "base" else 80.0)},
            "action_to_render_ms": around(120.0 if label == "base" else 120.0),
            "long_session_heap_bytes": around(20_000_000.0 if label == "base" else 20_000_000.0),
        },
        "bundle": {
            "largest_js_raw_bytes": 800_000,
            "total_raw_bytes": 1_800_000,
        },
        "system": {"cpu_pct": around(40.0 if label == "base" else 40.0)},
    }
    body.update(overrides)
    return body


def document(base=None, candidate=None):
    return {
        "schema_version": 1,
        "base": base if base is not None else side("base"),
        "candidate": candidate if candidate is not None else side("candidate"),
    }


def run_cli(*args, input_doc=None):
    with tempfile.TemporaryDirectory() as tmp:
        cmd = [sys.executable, str(SCRIPT), *args]
        if input_doc is not None:
            path = Path(tmp) / "in.json"
            path.write_text(json.dumps(input_doc))
            cmd.append(str(path))
        return subprocess.run(cmd, capture_output=True, text=True)


def compare_doc(doc, **kwargs):
    return COMPARE["compare"](
        doc,
        min_samples=kwargs.get("min_samples", COMPARE["DEFAULT_MIN_SAMPLES"]),
        max_cv=kwargs.get("max_cv", COMPARE["DEFAULT_MAX_CV"]),
        alpha=kwargs.get("alpha", COMPARE["ALPHA"]),
    )


class KnownFixturesTests(unittest.TestCase):
    def test_known_faster_reports_faster(self):
        doc = document(
            side("base", workloads={"closed-baseline": {"phase": "warm", "samples": around(100, 10, 2)}}),
            side(
                "candidate",
                workloads={"closed-baseline": {"phase": "warm", "samples": around(50, 10, 2)}},
            ),
        )
        # Drop other families so the overall verdict is driven by the fixture.
        for label in ("base", "candidate"):
            doc[label]["benchmarks"] = {}
            doc[label]["browser"] = {}
            doc[label]["bundle"] = {}
            doc[label]["system"] = {}
        report = compare_doc(doc)
        self.assertEqual(report["overall"], "faster")
        self.assertTrue(report["speed_compared"])
        self.assertEqual(report["metrics"][0]["verdict"], "faster")
        self.assertNotEqual(report["overall"], "equivalent")
        proc = run_cli(input_doc=doc)
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertEqual(json.loads(proc.stdout)["overall"], "faster")

    def test_known_slower_reports_slower(self):
        doc = document(
            side("base", workloads={"closed-baseline": {"phase": "warm", "samples": around(50, 10, 2)}}),
            side(
                "candidate",
                workloads={"closed-baseline": {"phase": "warm", "samples": around(100, 10, 2)}},
            ),
        )
        for label in ("base", "candidate"):
            doc[label]["benchmarks"] = {}
            doc[label]["browser"] = {}
            doc[label]["bundle"] = {}
            doc[label]["system"] = {}
        report = compare_doc(doc)
        self.assertEqual(report["overall"], "slower")
        self.assertEqual(report["metrics"][0]["verdict"], "slower")
        proc = run_cli(input_doc=doc)
        self.assertEqual(proc.returncode, 4, proc.stderr)

    def test_noisy_is_not_equivalent(self):
        base_s = [40, 180, 60, 200, 50, 190, 70, 170, 45, 185]
        cand_s = [50, 175, 65, 195, 55, 185, 80, 160, 48, 190]
        doc = document(
            side("base", workloads={"closed-baseline": {"phase": "warm", "samples": base_s}}),
            side("candidate", workloads={"closed-baseline": {"phase": "warm", "samples": cand_s}}),
        )
        for label in ("base", "candidate"):
            doc[label]["benchmarks"] = {}
            doc[label]["browser"] = {}
            doc[label]["bundle"] = {}
            doc[label]["system"] = {}
        report = compare_doc(doc)
        self.assertNotEqual(report["overall"], "equivalent")
        self.assertNotEqual(report["metrics"][0]["verdict"], "equivalent")
        self.assertIn(report["overall"], ("inconclusive", "no_significant_difference"))
        self.assertIn(report["metrics"][0]["verdict"], ("inconclusive", "no_significant_difference"))
        joined = " ".join(report["reasons"] + report["metrics"][0]["reasons"])
        self.assertNotIn("equivalent", joined.replace("not equivalence", "").replace("not equivalent", ""))
        self.assertTrue("noisy" in joined or "not significant" in joined or "not equivalence" in joined)

    def test_undersampled_is_inconclusive_not_equivalent(self):
        doc = document(
            side("base", workloads={"closed-baseline": {"phase": "warm", "samples": [100, 102, 99]}}),
            side("candidate", workloads={"closed-baseline": {"phase": "warm", "samples": [50, 51, 49]}}),
        )
        for label in ("base", "candidate"):
            doc[label]["benchmarks"] = {}
            doc[label]["browser"] = {}
            doc[label]["bundle"] = {}
            doc[label]["system"] = {}
        report = compare_doc(doc)
        self.assertEqual(report["overall"], "inconclusive")
        self.assertEqual(report["metrics"][0]["verdict"], "inconclusive")
        self.assertTrue(any("undersampled" in r for r in report["metrics"][0]["reasons"]))
        self.assertNotEqual(report["overall"], "faster")
        self.assertNotEqual(report["overall"], "equivalent")
        proc = run_cli(input_doc=doc)
        self.assertEqual(proc.returncode, 3, proc.stderr)


class FailClosedTests(unittest.TestCase):
    def test_missing_data_is_not_a_pass(self):
        base = side("base")
        cand = side("candidate")
        cand["workloads"] = {}
        cand["benchmarks"] = {}
        cand["browser"] = {}
        cand["bundle"] = {}
        cand["system"] = {}
        base["benchmarks"] = {}
        base["browser"] = {}
        base["bundle"] = {}
        base["system"] = {}
        report = compare_doc(document(base, cand))
        self.assertEqual(report["overall"], "fail")
        self.assertTrue(any("missing data" in r.lower() or "missing data" in m["reasons"][0] for m in report["metrics"] for r in [report["overall"]]))
        self.assertTrue(any(m["verdict"] == "fail" for m in report["metrics"]))
        proc = run_cli(input_doc=document(base, cand))
        self.assertEqual(proc.returncode, 2, proc.stderr)

    def test_empty_samples_fail_closed(self):
        doc = document(
            side("base", workloads={"closed-baseline": {"phase": "warm", "samples": []}}),
            side("candidate", workloads={"closed-baseline": {"phase": "warm", "samples": around(50)}}),
        )
        for label in ("base", "candidate"):
            doc[label]["benchmarks"] = {}
            doc[label]["browser"] = {}
            doc[label]["bundle"] = {}
            doc[label]["system"] = {}
        report = compare_doc(doc)
        self.assertEqual(report["overall"], "fail")
        self.assertFalse(any(m["verdict"] == "equivalent" for m in report["metrics"]))

    def test_mismatched_environments_are_inconclusive(self):
        cand = side("candidate")
        cand["provenance"] = provenance("candidate", host_id="perf-host-OTHER")
        for label_side in (side("base"), cand):
            label_side["workloads"] = {
                "closed-baseline": {"phase": "warm", "samples": around(100)}
            }
            label_side["benchmarks"] = {}
            label_side["browser"] = {}
            label_side["bundle"] = {}
            label_side["system"] = {}
        report = compare_doc(document(side("base", workloads={"closed-baseline": {"phase": "warm", "samples": around(100)}}, benchmarks={}, browser={}, bundle={}, system={}), cand))
        self.assertEqual(report["overall"], "inconclusive")
        self.assertTrue(any("host_id" in r for r in report["reasons"]))
        self.assertNotEqual(report["overall"], "equivalent")

    def test_missing_provenance_field_fails(self):
        base = side("base")
        del base["provenance"]["host_id"]
        report = compare_doc(document(base, side("candidate")))
        self.assertEqual(report["overall"], "fail")
        self.assertTrue(any("host_id" in r for r in report["reasons"]))
        self.assertFalse(report["speed_compared"])

    def test_instrumented_image_fails_before_speed(self):
        cand = side("candidate", provenance=provenance("candidate", instrumented=True))
        cand["workloads"] = {"closed-baseline": {"phase": "warm", "samples": around(10, 10, 1)}}
        base = side("base")
        base["workloads"] = {"closed-baseline": {"phase": "warm", "samples": around(100, 10, 1)}}
        for s in (base, cand):
            s["benchmarks"] = {}
            s["browser"] = {}
            s["bundle"] = {}
            s["system"] = {}
        report = compare_doc(document(base, cand))
        self.assertEqual(report["overall"], "fail")
        self.assertFalse(report["speed_compared"])
        self.assertTrue(any("instrumented" in r for r in report["reasons"]))

    def test_correctness_failure_aborts_before_speed(self):
        cand = side(
            "candidate",
            correctness={"ok": False, "failures": ["closed-baseline: 2 runs failed"]},
            workloads={"closed-baseline": {"phase": "warm", "samples": around(10, 10, 1)}},
        )
        base = side("base", workloads={"closed-baseline": {"phase": "warm", "samples": around(100, 10, 1)}})
        for s in (base, cand):
            s["benchmarks"] = {}
            s["browser"] = {}
            s["bundle"] = {}
            s["system"] = {}
        report = compare_doc(document(base, cand))
        self.assertEqual(report["overall"], "fail")
        self.assertFalse(report["speed_compared"])
        self.assertIn("speed not compared", report["benchstat"])
        self.assertNotEqual(report["overall"], "faster")

    def test_failed_sample_outcome_is_correctness_failure(self):
        samples = [{"value": 50, "outcome": "passed"} for _ in range(9)]
        samples.append({"value": 50, "outcome": "failed"})
        cand = side("candidate", workloads={"closed-baseline": {"phase": "warm", "samples": samples}})
        base = side("base", workloads={"closed-baseline": {"phase": "warm", "samples": around(100)}})
        for s in (base, cand):
            s["benchmarks"] = {}
            s["browser"] = {}
            s["bundle"] = {}
            s["system"] = {}
        report = compare_doc(document(base, cand))
        self.assertEqual(report["overall"], "fail")
        self.assertFalse(report["speed_compared"])

    def test_insignificant_difference_is_not_equivalent(self):
        # Heavy overlap: +1% shift inside the noise, n=10.
        base_s = around(100, 10, 8)
        cand_s = [x + 1.0 for x in base_s]
        doc = document(
            side("base", workloads={"closed-baseline": {"phase": "warm", "samples": base_s}}),
            side("candidate", workloads={"closed-baseline": {"phase": "warm", "samples": cand_s}}),
        )
        for label in ("base", "candidate"):
            doc[label]["benchmarks"] = {}
            doc[label]["browser"] = {}
            doc[label]["bundle"] = {}
            doc[label]["system"] = {}
        report = compare_doc(doc)
        self.assertNotEqual(report["overall"], "equivalent")
        self.assertNotEqual(report["metrics"][0]["verdict"], "equivalent")
        self.assertIn(report["metrics"][0]["verdict"], ("no_significant_difference", "inconclusive"))
        self.assertTrue(any("not equivalence" in r or "not equivalent" in r for r in report["metrics"][0]["reasons"] + report["reasons"]))

    def test_identical_samples_are_not_equivalent(self):
        samples = around(100)
        doc = document(
            side("base", workloads={"closed-baseline": {"phase": "warm", "samples": samples}}),
            side("candidate", workloads={"closed-baseline": {"phase": "warm", "samples": list(samples)}}),
        )
        for label in ("base", "candidate"):
            doc[label]["benchmarks"] = {}
            doc[label]["browser"] = {}
            doc[label]["bundle"] = {}
            doc[label]["system"] = {}
        report = compare_doc(doc)
        self.assertEqual(report["overall"], "no_significant_difference")
        self.assertNotEqual(report["overall"], "equivalent")

    def test_schema_mismatch_is_usage_error(self):
        proc = run_cli(input_doc={"schema_version": 99, "base": {}, "candidate": {}})
        self.assertEqual(proc.returncode, 1)
        self.assertIn("schema_version", proc.stderr)

    def test_missing_input_is_usage_error(self):
        proc = run_cli()
        self.assertEqual(proc.returncode, 1)


class BenchstatAndBrowserTests(unittest.TestCase):
    def test_benchstat_text_reports_delta_and_refuses_tilde_as_equivalence(self):
        doc = document(
            side(
                "base",
                benchmarks={"BenchmarkOwnerApplyCompletionLinear64": {"ns_per_op": around(10_000, 10, 50)}},
                workloads={},
                browser={},
                bundle={},
                system={},
            ),
            side(
                "candidate",
                benchmarks={"BenchmarkOwnerApplyCompletionLinear64": {"ns_per_op": around(7_000, 10, 50)}},
                workloads={},
                browser={},
                bundle={},
                system={},
            ),
        )
        report = compare_doc(doc)
        self.assertEqual(report["overall"], "faster")
        self.assertIn("BenchmarkOwnerApplyCompletionLinear64", report["benchstat"])
        self.assertIn("faster", report["benchstat"])
        self.assertIn("not equivalence", report["benchstat"])

    def test_go_bench_text_parses(self):
        text = "\n".join(
            [
                "BenchmarkOwnerApplyCompletionLinear64-10  1000  12345 ns/op  400 B/op  12 allocs/op",
                "BenchmarkOwnerApplyCompletionLinear64-10  1000  12400 ns/op  400 B/op  12 allocs/op",
                "BenchmarkOwnerApplyCompletionLinear64-10  1000  12200 ns/op  400 B/op  12 allocs/op",
                "BenchmarkOwnerApplyCompletionLinear64-10  1000  12310 ns/op  400 B/op  12 allocs/op",
                "BenchmarkOwnerApplyCompletionLinear64-10  1000  12380 ns/op  400 B/op  12 allocs/op",
            ]
        )
        parsed = COMPARE["parse_go_bench_text"](text)
        self.assertEqual(len(parsed["BenchmarkOwnerApplyCompletionLinear64"]["ns_per_op"]), 5)

    def test_browser_families_compare_separately(self):
        base = side(
            "base",
            workloads={},
            benchmarks={},
            bundle={},
            system={},
            browser={
                "route_readiness_ms": {"/jobs": around(80), "/system": around(90)},
                "action_to_render_ms": around(150),
                "long_session_heap_bytes": around(20_000_000),
            },
        )
        cand = side(
            "candidate",
            workloads={},
            benchmarks={},
            bundle={},
            system={},
            browser={
                "route_readiness_ms": {"/jobs": around(40), "/system": around(45)},
                "action_to_render_ms": around(70),
                "long_session_heap_bytes": around(19_500_000, 10, 50_000),
            },
        )
        report = compare_doc(document(base, cand))
        ids = {m["id"] for m in report["metrics"]}
        self.assertIn("browser.route_readiness_ms./jobs.live", ids)
        self.assertIn("browser.action_to_render_ms.live", ids)
        self.assertIn("browser.long_session_heap_bytes.live", ids)
        self.assertEqual(report["overall"], "faster")

    def test_system_metrics_include_distributions(self):
        report = compare_doc(
            document(
                side("base", workloads={}, benchmarks={}, browser={}, bundle={}, system={"cpu_pct": around(40)}),
                side("candidate", workloads={}, benchmarks={}, browser={}, bundle={}, system={"cpu_pct": around(20)}),
            )
        )
        dist = report["metrics"][0]["base"]
        for key in ("n", "mean", "stdev", "min", "max", "p50", "p90", "p99", "cv"):
            self.assertIn(key, dist)
        self.assertEqual(dist["n"], 10)

    def test_cold_and_warm_are_not_mixed(self):
        base = side(
            "base",
            workloads={"closed-baseline": {"phase": "cold", "samples": around(100)}},
            benchmarks={},
            browser={},
            bundle={},
            system={},
        )
        cand = side(
            "candidate",
            workloads={"closed-baseline": {"phase": "warm", "samples": around(50)}},
            benchmarks={},
            browser={},
            bundle={},
            system={},
        )
        report = compare_doc(document(base, cand))
        self.assertEqual(report["metrics"][0]["verdict"], "fail")
        self.assertTrue(any("phase" in r for r in report["metrics"][0]["reasons"]))


class BundleBudgetTests(unittest.TestCase):
    def test_mjs_keeps_largest_chunk_and_adds_total_route_assets(self):
        src = MJS.read_text()
        self.assertIn('process.env.BUNDLE_MAX_BYTES ?? "1400000"', src)
        self.assertIn('process.env.BUNDLE_MAX_GZIP_BYTES ?? "430000"', src)
        self.assertIn('process.env.BUNDLE_TOTAL_MAX_BYTES ?? "5000000"', src)
        self.assertIn('process.env.BUNDLE_TOTAL_MAX_GZIP_BYTES ?? "1600000"', src)
        self.assertIn("Splitting a large chunk cannot evade this", src)
        self.assertEqual(COMPARE["DEFAULT_LARGEST_JS_RAW"], 1_400_000)
        self.assertEqual(COMPARE["DEFAULT_TOTAL_RAW"], 5_000_000)

    def test_split_chunks_cannot_evade_total_budget(self):
        """Negative case: each JS file is under the largest-chunk budget, the sum is not.

        This is the documented evasion the total-route-assets check exists to
        catch. Budgets are evaluated by node ui/scripts/check-bundle-size.mjs.
        """
        if not shutil.which("node"):
            self.skipTest("node is required so gzip matches check-bundle-size.mjs")
        with tempfile.TemporaryDirectory() as tmp:
            assets = Path(tmp) / "dist" / "assets"
            assets.mkdir(parents=True)
            chunk = 1_200_000  # under 1_400_000 largest-chunk raw
            # Five under-limit chunks sum to 6 MiB, over the 5 MiB total budget.
            for name in ("chunk-a.js", "chunk-b.js", "chunk-c.js", "chunk-d.js", "chunk-e.js"):
                (assets / name).write_bytes(b"a" * chunk)
            result = COMPARE["evaluate_bundle_dir"](str(assets))
            self.assertFalse(result["ok"])
            self.assertGreater(result["total_raw_bytes"], COMPARE["DEFAULT_TOTAL_RAW"])
            self.assertLessEqual(result["largest_js_raw_bytes"], COMPARE["DEFAULT_LARGEST_JS_RAW"])
            self.assertTrue(any("Total route-asset" in e for e in result["errors"]))
            self.assertTrue(any("Splitting a large chunk cannot evade this" in e for e in result["errors"]))

            proc = subprocess.run(
                [sys.executable, str(SCRIPT), "--bundle-dir", str(assets)],
                capture_output=True,
                text=True,
            )
            self.assertEqual(proc.returncode, 2, proc.stderr)
            payload = json.loads(proc.stdout)
            self.assertFalse(payload["ok"])

    def test_under_both_budgets_passes(self):
        if not shutil.which("node"):
            self.skipTest("node is required so gzip matches check-bundle-size.mjs")
        with tempfile.TemporaryDirectory() as tmp:
            assets = Path(tmp) / "assets"
            assets.mkdir()
            (assets / "app.js").write_bytes(b"b" * 50_000)
            result = COMPARE["evaluate_bundle_dir"](str(assets))
            self.assertTrue(result["ok"], result["errors"])

    def test_mjs_negative_case_when_node_is_available(self):
        node = shutil.which("node")
        if not node:
            self.skipTest("node is not on PATH; Python twin already proves the split-evasion case")
        with tempfile.TemporaryDirectory() as tmp:
            assets = Path(tmp) / "assets"
            assets.mkdir()
            for name in ("chunk-a.js", "chunk-b.js", "chunk-c.js", "chunk-d.js", "chunk-e.js"):
                (assets / name).write_bytes(b"a" * 1_200_000)
            proc = subprocess.run(
                [node, str(MJS), "--dist", str(assets), "--json"],
                capture_output=True,
                text=True,
                cwd=tmp,
            )
            self.assertNotEqual(proc.returncode, 0, proc.stdout + proc.stderr)
            self.assertIn("Splitting a large chunk cannot evade this", proc.stderr)
            payload = json.loads(proc.stdout)
            self.assertFalse(payload["ok"])
            self.assertGreater(payload["total_raw_bytes"], payload["budgets"]["total_raw_bytes"])
            self.assertLessEqual(payload["largest_js_raw_bytes"], payload["budgets"]["largest_js_raw_bytes"])


class PerformanceShWiringTests(unittest.TestCase):
    def test_compare_subcommand_runs_the_python_comparator(self):
        doc = document(
            side(
                "base",
                workloads={"closed-baseline": {"phase": "warm", "samples": around(100, 10, 2)}},
                benchmarks={},
                browser={},
                bundle={},
                system={},
            ),
            side(
                "candidate",
                workloads={"closed-baseline": {"phase": "warm", "samples": around(50, 10, 2)}},
                benchmarks={},
                browser={},
                bundle={},
                system={},
            ),
        )
        with tempfile.TemporaryDirectory() as tmp:
            inp = Path(tmp) / "comparison.json"
            inp.write_text(json.dumps(doc))
            env = os.environ.copy()
            env["CAESIUM_PERF_ARTIFACTS"] = tmp
            proc = subprocess.run(
                ["bash", str(SH), "compare", str(inp)],
                capture_output=True,
                text=True,
                env=env,
                cwd=str(ROOT),
            )
            self.assertEqual(proc.returncode, 0, proc.stdout + proc.stderr)
            report = json.loads((Path(tmp) / "report.json").read_text())
            self.assertEqual(report["overall"], "faster")

    def test_script_refuses_instrumented_env_in_source(self):
        src = SH.read_text()
        self.assertIn("GOCOVERDIR", src)
        self.assertIn("testfault", src)
        self.assertIn("build-release", src)
        self.assertIn("interleave", src)
        self.assertIn("cold", src)
        self.assertIn("warm", src)
        self.assertNotIn("just load-test", src)
        self.assertNotIn("just integration-up", src)
        self.assertIn(">&2", src)
        self.assertIn("|| return 1", src)
        self.assertIn("if build_release", src)
        self.assertNotIn('CANDIDATE_BUILT="$(build_release', src)
        self.assertNotIn("BASE_BUILT=\"$(build_release", src)
        self.assertNotIn('run_benches "$ROOT" "$ARTIFACTS/candidate/bench.txt" || true', src)
        self.assertIn("required_families", src)
        self.assertIn("browser.exit", src)
        self.assertIn("check-bundle-size.mjs", src)
        self.assertIn("caesiumcloud/caesium-builder:${sha}", src)
        self.assertIn("Warm repetitions are interleaved", src)


class ReviewFixTests(unittest.TestCase):
    def _speed_sides(self, base_samples, cand_samples, **cand_prov):
        base = side(
            "base",
            workloads={"closed-baseline": {"phase": "warm", "samples": base_samples}},
            benchmarks={},
            browser={},
            bundle={},
            system={},
        )
        cand = side(
            "candidate",
            provenance=provenance("candidate", **cand_prov),
            workloads={"closed-baseline": {"phase": "warm", "samples": cand_samples}},
            benchmarks={},
            browser={},
            bundle={},
            system={},
        )
        return document(base, cand)

    def test_faster_does_not_hide_inconclusive_metrics_or_host_mismatch(self):
        base = side(
            "base",
            provenance=provenance("base", host_id="hostA"),
            workloads={
                "fast": {"phase": "warm", "samples": around(100, 10, 2)},
                "tiny": {"phase": "warm", "samples": [100, 101]},
                "noisy": {"phase": "warm", "samples": [10, 400, 12, 380, 11, 390, 13, 410, 9, 420]},
            },
            benchmarks={},
            browser={},
            bundle={},
            system={},
        )
        cand = side(
            "candidate",
            provenance=provenance("candidate", host_id="hostB"),
            workloads={
                "fast": {"phase": "warm", "samples": around(50, 10, 2)},
                "tiny": {"phase": "warm", "samples": [40, 41]},
                "noisy": {"phase": "warm", "samples": [12, 390, 15, 370, 14, 400, 11, 405, 10, 415]},
            },
            benchmarks={},
            browser={},
            bundle={},
            system={},
        )
        doc = document(base, cand)
        report = compare_doc(doc)
        self.assertEqual(report["overall"], "inconclusive")
        proc = run_cli(input_doc=doc)
        self.assertNotEqual(proc.returncode, 0, proc.stderr)
        self.assertEqual(proc.returncode, 3, proc.stderr)

    def test_outlier_mean_does_not_report_faster(self):
        doc = self._speed_sides([10] * 7 + [1000], [20] * 8)
        report = compare_doc(doc)
        self.assertNotEqual(report["overall"], "faster")
        self.assertNotEqual(report["metrics"][0]["verdict"], "faster")
        proc = run_cli(input_doc=doc)
        self.assertNotEqual(proc.returncode, 0)

    def test_significant_equal_means_are_not_nsd(self):
        # Identical values except a rank-visible swap that keeps the mean equal
        # is hard; equal samples that are significant cannot happen, but equal
        # means with a significant rank shift must not fall through to nsd.
        base_s = [1, 1, 1, 1, 1, 1, 1, 1001]
        cand_s = [2, 2, 2, 2, 2, 2, 2, 994]
        self.assertAlmostEqual(sum(base_s) / 8, sum(cand_s) / 8)
        doc = self._speed_sides(base_s, cand_s)
        report = compare_doc(doc)
        self.assertNotEqual(report["metrics"][0]["verdict"], "no_significant_difference")
        self.assertNotEqual(report["overall"], "no_significant_difference")

    def test_required_benchmark_family_with_failed_text_is_not_faster(self):
        base = side(
            "base",
            workloads={"closed-baseline": {"phase": "warm", "samples": around(100, 10, 2)}},
            benchmarks="",
            browser={},
            bundle={},
            system={},
        )
        cand = side(
            "candidate",
            workloads={"closed-baseline": {"phase": "warm", "samples": around(50, 10, 2)}},
            benchmarks="FAIL build failed",
            browser={},
            bundle={},
            system={},
        )
        doc = document(base, cand)
        doc["required_families"] = ["workload", "benchmark"]
        report = compare_doc(doc)
        self.assertNotEqual(report["overall"], "faster")
        self.assertEqual(report["overall"], "fail")
        proc = run_cli(input_doc=doc)
        self.assertEqual(proc.returncode, 2, proc.stderr)

    def test_mismatched_go_versions_are_inconclusive(self):
        doc = self._speed_sides(around(100), around(50), go_version="go1.26.0")
        report = compare_doc(doc)
        self.assertEqual(report["overall"], "inconclusive")
        self.assertTrue(any("go_version" in r for r in report["reasons"]))
        proc = run_cli(input_doc=doc)
        self.assertEqual(proc.returncode, 3, proc.stderr)

    def test_missing_required_bundle_does_not_drop_the_family(self):
        base = side("base", workloads={}, benchmarks={}, browser={}, bundle={}, system={})
        cand = side("candidate", workloads={}, benchmarks={}, browser={}, bundle={}, system={})
        doc = document(base, cand)
        doc["required_families"] = ["bundle"]
        report = compare_doc(doc)
        self.assertEqual(report["overall"], "fail")
        self.assertTrue(any("bundle" in r for r in report["reasons"]))

    def test_bundle_budget_errors_are_correctness_failures(self):
        base = side(
            "base",
            workloads={},
            benchmarks={},
            browser={},
            system={},
            bundle={"ok": True, "errors": [], "largest_js_raw_bytes": 100, "total_raw_bytes": 100},
        )
        cand = side(
            "candidate",
            workloads={},
            benchmarks={},
            browser={},
            system={},
            bundle={
                "ok": False,
                "errors": ["Total route-asset raw budget exceeded"],
                "largest_js_raw_bytes": 100,
                "total_raw_bytes": 9_000_000,
            },
        )
        doc = document(base, cand)
        doc["required_families"] = ["bundle"]
        report = compare_doc(doc)
        self.assertEqual(report["overall"], "fail")
        self.assertFalse(report["speed_compared"])
        self.assertTrue(any("budget" in r.lower() or "bundle" in r.lower() for r in report["reasons"]))

    def test_partial_browser_routes_do_not_pass(self):
        base = side(
            "base",
            workloads={},
            benchmarks={},
            bundle={},
            system={},
            browser={"route_readiness_ms": {"/jobs|live": around(80), "/triggers|live": around(90)}},
        )
        cand = side(
            "candidate",
            workloads={},
            benchmarks={},
            bundle={},
            system={},
            browser={"route_readiness_ms": {"/jobs|live": around(40), "/triggers|live": around(45)}},
        )
        doc = document(base, cand)
        doc["required_families"] = ["browser"]
        report = compare_doc(doc)
        self.assertNotEqual(report["overall"], "faster")
        self.assertEqual(report["overall"], "fail")
        self.assertTrue(any("required browser series missing" in r for r in report["reasons"]))

    def test_error_rate_is_lower_is_better(self):
        base = side(
            "base",
            workloads={},
            benchmarks={},
            browser={},
            bundle={},
            system={"error_rate": around(0.10, 10, 0.01)},
        )
        cand = side(
            "candidate",
            workloads={},
            benchmarks={},
            browser={},
            bundle={},
            system={"error_rate": around(0.40, 10, 0.01)},
        )
        report = compare_doc(document(base, cand))
        self.assertEqual(report["metrics"][0]["verdict"], "slower")
        self.assertTrue(report["metrics"][0]["lower_is_better"])

    def test_unknown_metric_name_fails_not_higher_is_better(self):
        base = side(
            "base",
            workloads={},
            benchmarks={},
            browser={},
            bundle={},
            system={"generate": around(10)},
        )
        cand = side(
            "candidate",
            workloads={},
            benchmarks={},
            browser={},
            bundle={},
            system={"generate": around(5)},
        )
        report = compare_doc(document(base, cand))
        self.assertEqual(report["overall"], "fail")
        self.assertEqual(report["metrics"][0]["verdict"], "fail")
        self.assertTrue(any("unknown metric direction" in r for r in report["metrics"][0]["reasons"]))

    def test_build_release_failed_just_does_not_echo_built(self):
        script = r"""
log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2; }
build_release() {
  local sha="$1" dest="$2" src="$3"
  log "building release image $dest from $src at $sha (just tag=$sha build-release)"
  (
    cd "$src"
    CAESIUM_SKIP_IMAGE_BUILD=false just tag="$sha" build-release
  ) >&2 || return 1
  return 0
}
if build_release a b .; then echo built; else echo failed-status; fi
"""
        with tempfile.TemporaryDirectory() as tmp:
            fake_just = Path(tmp) / "just"
            fake_just.write_text("#!/bin/sh\necho boom >&2\nexit 1\n")
            fake_just.chmod(0o755)
            env = os.environ.copy()
            env["PATH"] = tmp + os.pathsep + env.get("PATH", "")
            proc = subprocess.run(["bash", "-c", script], capture_output=True, text=True, env=env, cwd=tmp)
            self.assertIn("failed-status", proc.stdout)
            self.assertNotIn("built", proc.stdout.split())


if __name__ == "__main__":
    unittest.main()
