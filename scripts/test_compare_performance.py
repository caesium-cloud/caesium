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
BENCH_FILES = (
    "internal/run/owner_benchmark_test.go",
    "internal/run/recovery_benchmark_test.go",
)
BENCH_DIGEST = "f" * 64


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
        "benchmark_source_git_sha": sha,
        "benchmark_harness_git_sha": "b" * 40,
        "benchmark_harness_sha256": BENCH_DIGEST,
        "benchmark_overlay_paths": list(BENCH_FILES) if label == "base" else [],
        "benchmark_repeats": 10,
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
                "ns_per_op": around(10_000.0 if label == "base" else 10_000.0),
                "bytes_per_op": around(400.0),
                "allocs_per_op": around(12.0, spread=0),
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
    base = base if base is not None else side("base")
    candidate = candidate if candidate is not None else side("candidate")
    benches = base.get("benchmarks") or {}
    if isinstance(benches, str):
        benches = COMPARE["parse_go_bench_text"](benches)
    names = sorted(benches)
    files = [
        {"path": path, "sha256": str(i) * 64}
        for i, path in enumerate(BENCH_FILES, start=1)
    ]
    return {
        "schema_version": 1,
        "base": base,
        "candidate": candidate,
        "benchmark_harness": {
            "schema_version": 1,
            "base_source_sha": base["provenance"].get("git_sha"),
            "candidate_source_sha": candidate["provenance"].get("git_sha"),
            "harness_source_sha": candidate["provenance"].get("git_sha"),
            "harness_sha256": BENCH_DIGEST,
            "benchmark_names": names,
            "base_overlay_paths": list(BENCH_FILES),
            "base_release_image_id": base["provenance"].get("image_id"),
            "candidate_release_image_id": candidate["provenance"].get("image_id"),
            "files": [
                {"path": path, "sha256": str(i) * 64,
                 "base_original_sha256": None, "overlaid": True}
                for i, path in enumerate(BENCH_FILES, start=1)
            ],
        },
        "benchmark_sampling": {
            "schema_version": 1,
            "repeats": 10,
            "expected_names": names,
            "source_files": files,
            "settings_sha256": "d" * 64,
            "order": [
                {"repeat": repeat, "side": label, "exit_code": 0}
                for repeat in range(1, 11)
                for label in (("base", "candidate") if repeat % 2 else ("candidate", "base"))
            ],
            "aggregate_exit": {"base": 0, "candidate": 0},
        },
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


class BenchmarkHarnessProvenanceTests(unittest.TestCase):
    @staticmethod
    def bench_document():
        doc = document()
        doc["required_families"] = ["benchmark"]
        for label in ("base", "candidate"):
            for family in ("workloads", "browser", "bundle", "system"):
                doc[label][family] = {}
        return doc

    def assert_rejected(self, doc, reason):
        report = compare_doc(doc)
        self.assertEqual(report["overall"], "fail", report)
        self.assertFalse(report["speed_compared"])
        self.assertTrue(any(reason in item for item in report["reasons"]), report["reasons"])
        proc = run_cli(input_doc=doc)
        self.assertEqual(proc.returncode, 2, proc.stderr)

    def test_matching_harness_allows_benchmark_comparison(self):
        doc = self.bench_document()
        report = compare_doc(doc)
        self.assertEqual(report["overall"], "no_significant_difference")
        self.assertTrue(report["speed_compared"])
        self.assertEqual(run_cli(input_doc=doc).returncode, 0)

    def test_missing_manifest_fails_closed(self):
        doc = self.bench_document()
        del doc["benchmark_harness"]
        self.assert_rejected(doc, "benchmark_harness")

    def test_missing_sampling_evidence_fails_closed(self):
        doc = self.bench_document()
        del doc["benchmark_sampling"]
        self.assert_rejected(doc, "benchmark_sampling")

    def test_one_of_eleven_declared_benchmarks_cannot_pass_compare_only(self):
        doc = self.bench_document()
        present = "BenchmarkOwnerApplyCompletionLinear64"
        names = sorted([present] + [f"BenchmarkRecoverMissing{i}" for i in range(10)])
        doc["benchmark_harness"]["benchmark_names"] = names
        doc["benchmark_sampling"]["expected_names"] = names
        # Both sides still contain ten ns/op, B/op, and allocs/op samples for
        # the one surviving benchmark. The other ten functions disappeared.
        self.assert_rejected(doc, "missing=")

    def test_one_missing_metric_repeat_cannot_pass(self):
        doc = self.bench_document()
        doc["candidate"]["benchmarks"]["BenchmarkOwnerApplyCompletionLinear64"]["bytes_per_op"].pop()
        self.assert_rejected(doc, "bytes_per_op has 9 samples, want 10")

    def test_benchmark_repeat_provenance_and_order_must_agree(self):
        for mutation, reason in (
            (lambda doc: doc["candidate"]["provenance"].update(benchmark_repeats=9), "benchmark_repeats"),
            (lambda doc: doc["benchmark_sampling"]["order"].pop(), "order"),
            (lambda doc: doc["benchmark_sampling"]["aggregate_exit"].update(candidate=17), "aggregate_exit"),
        ):
            with self.subTest(reason=reason):
                doc = self.bench_document()
                mutation(doc)
                self.assert_rejected(doc, reason)

    def test_different_side_harness_digest_fails_closed(self):
        doc = self.bench_document()
        doc["base"]["provenance"]["benchmark_harness_sha256"] = "0" * 64
        self.assert_rejected(doc, "base.provenance.benchmark_harness_sha256")

    def test_wrong_harness_source_and_image_identity_fail_closed(self):
        for field, reason in (
            ("harness_source_sha", "harness_source_sha"),
            ("base_release_image_id", "base_release_image_id"),
        ):
            with self.subTest(field=field):
                doc = self.bench_document()
                doc["benchmark_harness"][field] = "wrong"
                self.assert_rejected(doc, reason)

    def test_missing_file_or_false_overlay_accounting_fails_closed(self):
        for mutation, reason in (
            (lambda doc: doc["benchmark_harness"]["files"].pop(), "files"),
            (lambda doc: doc["benchmark_harness"]["base_overlay_paths"].clear(), "base_overlay_paths"),
        ):
            with self.subTest(reason=reason):
                doc = self.bench_document()
                mutation(doc)
                self.assert_rejected(doc, reason)

    def test_valid_partial_overlay_keeps_same_harness(self):
        doc = self.bench_document()
        doc["benchmark_harness"]["base_overlay_paths"] = [BENCH_FILES[0]]
        doc["benchmark_harness"]["files"][1]["overlaid"] = False
        doc["benchmark_harness"]["files"][1]["base_original_sha256"] = "2" * 64
        doc["base"]["provenance"]["benchmark_overlay_paths"] = [BENCH_FILES[0]]
        self.assertEqual(compare_doc(doc)["overall"], "no_significant_difference")


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
                benchmarks={"BenchmarkOwnerApplyCompletionLinear64": {
                    "ns_per_op": around(10_000, 10, 50),
                    "bytes_per_op": around(400, 10, 0),
                    "allocs_per_op": around(12, 10, 0),
                }},
                workloads={},
                browser={},
                bundle={},
                system={},
            ),
            side(
                "candidate",
                benchmarks={"BenchmarkOwnerApplyCompletionLinear64": {
                    "ns_per_op": around(7_000, 10, 50),
                    "bytes_per_op": around(400, 10, 0),
                    "allocs_per_op": around(12, 10, 0),
                }},
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

    def test_compare_subcommand_rejects_benchmark_without_harness(self):
        doc = BenchmarkHarnessProvenanceTests.bench_document()
        del doc["benchmark_harness"]
        with tempfile.TemporaryDirectory() as tmp:
            inp = Path(tmp) / "comparison.json"
            inp.write_text(json.dumps(doc))
            env = os.environ.copy()
            env["CAESIUM_PERF_ARTIFACTS"] = tmp
            proc = subprocess.run(
                ["bash", str(SH), "compare", str(inp)],
                capture_output=True, text=True, env=env, cwd=str(ROOT),
            )
            self.assertEqual(proc.returncode, 2, proc.stdout + proc.stderr)
            report = json.loads((Path(tmp) / "report.json").read_text())
            self.assertEqual(report["overall"], "fail")
            self.assertFalse(report["speed_compared"])

    def test_live_assembly_refuses_missing_benchmark_exit_marker(self):
        source = SH.read_text()
        marker = "python3 - <<'PY'\nimport json, os, pathlib, re\n"
        code = "import json, os, pathlib, re\n" + source.split(marker, 1)[1].split("\nPY\n", 1)[0]
        with tempfile.TemporaryDirectory() as tmp:
            art = Path(tmp)
            (art / "observations").mkdir()
            for label in ("base", "candidate"):
                (art / label).mkdir()
            doc = document()
            (art / "observations" / "benchmark-harness.json").write_text(
                json.dumps(doc["benchmark_harness"])
            )
            (art / "observations" / "benchmark-names.json").write_text(json.dumps({
                "source_files": doc["benchmark_sampling"]["source_files"],
                "benchmark_names": doc["benchmark_sampling"]["expected_names"],
            }))
            env = os.environ.copy()
            env.update({
                "ARTIFACTS": str(art), "RUN_BENCH": "1", "REPEATS": "10",
                "BASE_SHA": "a" * 40, "CANDIDATE_SHA": "b" * 40,
                "BASE_IMAGE_ID": doc["base"]["provenance"]["image_id"],
                "CANDIDATE_IMAGE_ID": doc["candidate"]["provenance"]["image_id"],
                "BENCH_HARNESS_MANIFEST": str(art / "observations" / "benchmark-harness.json"),
            })
            result = subprocess.run(
                [sys.executable, "-c", code], env=env, capture_output=True, text=True
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("benchmark exit or repeat evidence is missing", result.stderr)
            self.assertFalse((art / "comparison.json").exists())

    def test_live_assembly_records_complete_benchmark_sampling(self):
        source = SH.read_text()
        marker = "python3 - <<'PY'\nimport json, os, pathlib, re\n"
        code = "import json, os, pathlib, re\n" + source.split(marker, 1)[1].split("\nPY\n", 1)[0]
        with tempfile.TemporaryDirectory() as tmp:
            art = Path(tmp)
            (art / "observations").mkdir()
            doc = document()
            manifest = doc["benchmark_harness"]
            (art / "observations" / "benchmark-harness.json").write_text(json.dumps(manifest))
            (art / "observations" / "benchmark-names.json").write_text(json.dumps({
                "source_files": doc["benchmark_sampling"]["source_files"],
                "benchmark_names": doc["benchmark_sampling"]["expected_names"],
            }))
            order = []
            for label in ("base", "candidate"):
                directory = art / label
                directory.mkdir()
                rows = []
                for repeat in range(1, 11):
                    row = f"BenchmarkOwnerApplyCompletionLinear64-12  1  {10000 + repeat} ns/op  400 B/op  12 allocs/op\n"
                    (directory / f"bench-repeat-{repeat}.txt").write_text(row)
                    rows.append(row)
                (directory / "bench.txt").write_text("".join(rows))
                (directory / "bench.txt.exit").write_text("0\n")
                (directory / "bench.txt.repeats.tsv").write_text(
                    "".join(f"{repeat}\t0\n" for repeat in range(1, 11))
                )
            for repeat in range(1, 11):
                for label in (("base", "candidate") if repeat % 2 else ("candidate", "base")):
                    order.append(f"{repeat}\t{label}\t0\n")
            (art / "observations" / "benchmark-order.tsv").write_text("".join(order))
            env = os.environ.copy()
            env.update({
                "ARTIFACTS": str(art), "RUN_BENCH": "1", "REPEATS": "10",
                "RUN_LOAD": "0", "RUN_BROWSER": "0", "RUN_BUNDLE": "0",
                "BASE_SHA": "a" * 40, "CANDIDATE_SHA": "b" * 40,
                "BASE_IMAGE": "base:synthetic", "CANDIDATE_IMAGE": "candidate:synthetic",
                "BASE_IMAGE_ID": doc["base"]["provenance"]["image_id"],
                "CANDIDATE_IMAGE_ID": doc["candidate"]["provenance"]["image_id"],
                "BASE_CLI_DIGEST": "sha256:base-cli", "CANDIDATE_CLI_DIGEST": "sha256:candidate-cli",
                "BASE_BUILT": "built", "CANDIDATE_BUILT": "built",
                "BASE_GO_VERSION": "go1.27.1", "CANDIDATE_GO_VERSION": "go1.27.1",
                "BASE_BUILDER_ID": "sha256:builder", "CANDIDATE_BUILDER_ID": "sha256:builder",
                "BASE_TOOLCHAIN": "builder:synthetic", "CANDIDATE_TOOLCHAIN": "builder:synthetic",
                "DOCKER_PLATFORM": "linux/arm64", "HOST_ID": "synthetic-host",
                "CATALOG_SHA": "c" * 64, "SETTINGS_SHA": "d" * 64,
                "BENCH_HARNESS_MANIFEST": str(art / "observations" / "benchmark-harness.json"),
            })
            result = subprocess.run(
                [sys.executable, "-c", code], env=env, capture_output=True, text=True
            )
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            assembled = json.loads((art / "comparison.json").read_text())
            self.assertEqual(assembled["benchmark_sampling"], doc["benchmark_sampling"])
            self.assertEqual(assembled["base"]["provenance"]["benchmark_repeats"], 10)
            self.assertEqual(assembled["candidate"]["provenance"]["benchmark_repeats"], 10)
            self.assertTrue(assembled["base"]["correctness"]["ok"])
            self.assertTrue(assembled["candidate"]["correctness"]["ok"])

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
