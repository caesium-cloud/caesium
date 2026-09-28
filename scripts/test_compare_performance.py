"""Fail-closed tests for scripts/compare-performance.py and the total-asset budget."""

from __future__ import annotations

import hashlib
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
BENCH_HELPERS = (("internal/run/owner_state_test.go", "3" * 64),)
_bench_digest = hashlib.sha256()
for _path, _sha in [(path, str(i) * 64) for i, path in enumerate(BENCH_FILES, start=1)] + list(BENCH_HELPERS):
    _bench_digest.update(_path.encode() + b"\0" + _sha.encode() + b"\0")
BENCH_DIGEST = _bench_digest.hexdigest()


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
            "helper_files": [
                {"path": path, "sha256": sha} for path, sha in BENCH_HELPERS
            ],
        },
        "benchmark_sampling": {
            "schema_version": 1,
            "repeats": 10,
            "base_compile": {
                "exit_code": 0,
                "output_path": "observations/benchmark-base-compile.txt",
            },
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

    def test_helper_hash_is_part_of_shared_harness_identity(self):
        doc = self.bench_document()
        doc["benchmark_harness"]["helper_files"][0]["sha256"] = "4" * 64
        self.assert_rejected(doc, "harness_sha256 differs")

    def test_base_compile_failure_is_classified_as_harness_incompatibility(self):
        doc = self.bench_document()
        doc["benchmark_sampling"]["base_compile"]["exit_code"] = 1
        doc["benchmark_sampling"]["order"][0]["exit_code"] = 1
        doc["benchmark_sampling"]["aggregate_exit"]["base"] = 1
        doc["base"]["correctness"]["failures"] = ["workload failed"]
        report = compare_doc(doc)
        self.assertEqual(report["overall"], "fail")
        self.assertFalse(report["speed_compared"])
        self.assertTrue(any("benchmark harness incompatible with base" in x for x in report["reasons"]))
        self.assertTrue(any("base correctness: workload failed" in x for x in report["reasons"]))

    def test_side_file_mode_requires_and_accepts_benchmark_evidence(self):
        doc = self.bench_document()
        with tempfile.TemporaryDirectory() as tmp:
            base_path = Path(tmp) / "base.json"
            candidate_path = Path(tmp) / "candidate.json"
            evidence_path = Path(tmp) / "evidence.json"
            base_path.write_text(json.dumps(doc["base"]))
            candidate_path.write_text(json.dumps(doc["candidate"]))
            evidence_path.write_text(json.dumps(doc))
            command = [sys.executable, str(SCRIPT), "--base", str(base_path), "--candidate", str(candidate_path)]
            missing = subprocess.run(command, capture_output=True, text=True)
            self.assertEqual(missing.returncode, 1, missing.stderr)
            self.assertIn("requires --benchmark-evidence", missing.stderr)
            with_evidence = subprocess.run(
                command + ["--benchmark-evidence", str(evidence_path)], capture_output=True, text=True
            )
            self.assertEqual(with_evidence.returncode, 0, with_evidence.stderr)
            self.assertEqual(json.loads(with_evidence.stdout)["overall"], "no_significant_difference")

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
            # The synthetic host cannot match the recorded fixed baseline.
            env["CAESIUM_PERF_BASELINE"] = "none"
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
            self.assertEqual(report["decision"]["fixed_baseline"]["verdict"], "not_requested")
            self.assertTrue(report["strict_gate"]["blocking"])

            # By default the compare subcommand also judges against the
            # versioned fixed baseline; a synthetic host never matches it.
            del env["CAESIUM_PERF_BASELINE"]
            proc = subprocess.run(
                ["bash", str(SH), "compare", str(inp)],
                capture_output=True, text=True, env=env, cwd=str(ROOT),
            )
            self.assertEqual(proc.returncode, 3, proc.stdout + proc.stderr)
            report = json.loads((Path(tmp) / "report.json").read_text())
            self.assertEqual(report["overall"], "inconclusive_unresolved")
            self.assertEqual(report["decision"]["fixed_baseline"]["verdict"], "inconclusive")
            self.assertFalse(report["decision"]["fixed_baseline"]["rerun_eligible"])

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
            (art / "observations" / "benchmark-base-compile.txt").write_text("")
            (art / "observations" / "benchmark-base-compile.exit").write_text("0\n")
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
            (art / "observations" / "benchmark-base-compile.txt").write_text("")
            (art / "observations" / "benchmark-base-compile.exit").write_text("0\n")
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

            # A base compile failure remains a conclusive failure, with an
            # explicit harness reason instead of blaming base correctness.
            (art / "observations" / "benchmark-base-compile.exit").write_text("23\n")
            (art / "base" / "bench.txt.exit").write_text("23\n")
            (art / "base" / "bench.txt.repeats.tsv").write_text(
                "".join(f"{repeat}\t23\n" for repeat in range(1, 11))
            )
            failed_order = [
                f"{repeat}\t{label}\t{23 if label == 'base' else 0}\n"
                for repeat in range(1, 11)
                for label in (("base", "candidate") if repeat % 2 else ("candidate", "base"))
            ]
            (art / "observations" / "benchmark-order.tsv").write_text("".join(failed_order))
            result = subprocess.run(
                [sys.executable, "-c", code], env=env, capture_output=True, text=True
            )
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            assembled = json.loads((art / "comparison.json").read_text())
            self.assertTrue(assembled["base"]["correctness"]["ok"])
            report = compare_doc(assembled)
            self.assertEqual(report["overall"], "fail")
            self.assertTrue(any("benchmark harness incompatible with base" in x for x in report["reasons"]))
            self.assertFalse(any("base correctness: benchmarks exited" in x for x in report["reasons"]))

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


def workload_only(base_samples, cand_samples, phase="cold"):
    doc = document(
        side("base", workloads={"closed-baseline": {"phase": phase, "samples": base_samples}}),
        side("candidate", workloads={"closed-baseline": {"phase": phase, "samples": cand_samples}}),
    )
    for label in ("base", "candidate"):
        doc[label]["benchmarks"] = {}
        doc[label]["browser"] = {}
        doc[label]["bundle"] = {}
        doc[label]["system"] = {}
    return doc


class InformationalDiagnosticsTests(unittest.TestCase):
    """Reporting that names what to attribute but never changes a verdict."""

    # The recorded ec893213 cold series: one 32.5 s repeat among ~6.5 s ones.
    COLD_BASE = [6.72, 6.59, 6.37, 6.73, 6.32, 6.15, 6.54, 6.2, 6.48, 6.56]
    COLD_CANDIDATE = [6.97, 6.32, 6.46, 6.26, 6.53, 6.32, 32.54, 6.85, 6.53, 6.12]

    def test_single_stall_stays_inconclusive_and_names_its_repeat(self):
        doc = workload_only(self.COLD_BASE, self.COLD_CANDIDATE)
        report = compare_doc(doc)
        metric = report["metrics"][0]
        self.assertEqual(report["overall"], "inconclusive")
        self.assertEqual(metric["verdict"], "inconclusive")
        self.assertTrue(any("noisy" in reason for reason in metric["reasons"]))
        extremes = metric["diagnostics"]["candidate"]["extreme_samples"]
        self.assertEqual([entry["sample"] for entry in extremes], [7])
        self.assertEqual(extremes[0]["value"], 32.54)
        self.assertEqual(metric["diagnostics"]["base"]["extreme_samples"], [])
        # The flagged sample stays in the statistics.
        self.assertEqual(metric["candidate"]["max"], 32.54)
        self.assertIn("extreme candidate samples in workload.closed-baseline.duration_seconds: #7=32.54",
                      report["benchstat"])
        proc = run_cli(input_doc=doc)
        self.assertEqual(proc.returncode, 3, proc.stderr)

    def test_flagged_extreme_sample_is_not_removed_from_a_slower_verdict(self):
        cand = around(100, 10, 2)
        cand[3] = 400.0
        doc = workload_only(around(50, 10, 2), cand, phase="warm")
        report = compare_doc(doc)
        metric = report["metrics"][0]
        self.assertEqual(metric["verdict"], "slower")
        self.assertEqual(report["overall"], "slower")
        self.assertEqual([e["sample"] for e in metric["diagnostics"]["candidate"]["extreme_samples"]], [4])
        self.assertAlmostEqual(metric["candidate"]["mean"], sum(cand) / 10)
        self.assertEqual(run_cli(input_doc=doc).returncode, 4)

    def test_extreme_samples_need_spread_and_three_points(self):
        extreme = COMPARE["extreme_samples"]
        self.assertEqual(extreme([12_700_000.0] * 9 + [11_900_000.0]), [])  # zero MAD
        self.assertEqual(extreme([1.0, 100.0]), [])
        self.assertEqual([e["sample"] for e in extreme([10, 11, 10, 12, 11, 10, 11, 90])], [8])

    def test_multiplicity_counts_informative_series_only(self):
        report = compare_doc(document())
        multiplicity = report["multiplicity"]
        self.assertTrue(multiplicity["informational_only"])
        # allocs_per_op is constant and equal on both sides: excluded.
        self.assertEqual(multiplicity["degenerate_metrics"], 1)
        n = multiplicity["informative_metrics"]
        self.assertEqual(n, multiplicity["metrics_with_p_value"] - 1)
        self.assertGreater(n, 0)
        self.assertAlmostEqual(multiplicity["expected_significant_if_no_change"], round(0.05 * n, 3))
        self.assertAlmostEqual(multiplicity["expected_slower_if_no_change"], round(0.025 * n, 3))
        self.assertAlmostEqual(multiplicity["probability_no_slower_if_no_change"], round(0.975 ** n, 4))
        self.assertEqual(multiplicity["observed_significant"], 0)
        self.assertEqual(multiplicity["probability_at_least_observed_significant_if_no_change"], 1.0)
        self.assertIn("multiplicity (informational)", report["benchstat"])

    def test_multiplicity_never_gates_the_exit_status(self):
        # Every informative series is faster: improbable by chance, and still
        # exit 0 because verdicts and exit codes ignore the summary.
        doc = workload_only(around(100, 10, 2), around(50, 10, 2), phase="warm")
        report = compare_doc(doc)
        self.assertEqual(report["overall"], "faster")
        self.assertEqual(report["multiplicity"]["observed_significant"], 1)
        self.assertLess(report["multiplicity"]["probability_at_least_observed_significant_if_no_change"], 0.06)
        self.assertEqual(run_cli(input_doc=doc).returncode, 0)

    def test_multiplicity_breaks_counts_down_by_family(self):
        # Shape of the same-image A/A control: several benchmarks from the same
        # processes move together while the other families stay insignificant.
        base = side("base")
        cand = side("candidate")
        for label, body, center in (("base", base, 10_000.0), ("candidate", cand, 9_700.0)):
            body["benchmarks"] = {
                f"BenchmarkOwner{name}": {
                    "ns_per_op": around(center, 10, 40.0),
                    "bytes_per_op": around(400.0, spread=0),
                    "allocs_per_op": around(12.0, spread=0),
                }
                for name in ("A", "B", "C")
            }
        report = compare_doc(document(base, cand))
        self.assertEqual(report["overall"], "faster")
        families = report["multiplicity"]["by_family"]
        self.assertEqual(families["benchmark"], {"informative": 3, "significant": 3, "slower": 0, "faster": 3})
        self.assertEqual(families["workload"]["significant"], 0)
        self.assertEqual(families["browser"]["informative"], 3)
        self.assertEqual(report["multiplicity"]["degenerate_metrics"], 6)
        self.assertIn("correlated", report["multiplicity"]["note"])

    def test_fail_closed_report_has_no_multiplicity(self):
        doc = document()
        doc["candidate"]["correctness"] = {"ok": False, "failures": ["boom"]}
        report = compare_doc(doc)
        self.assertEqual(report["overall"], "fail")
        self.assertIsNone(report["multiplicity"])

    def test_control_marker_passes_through(self):
        doc = document()
        doc["control"] = "a_a"
        self.assertEqual(compare_doc(doc)["control"], "a_a")
        self.assertIsNone(compare_doc(document())["control"])


class PerformanceShAttributionTests(unittest.TestCase):
    """Live-run wiring for the A/A control and per-sample evidence."""

    @staticmethod
    def assembly_code():
        source = SH.read_text()
        marker = "python3 - <<'PY'\nimport json, os, pathlib, re\n"
        return "import json, os, pathlib, re\n" + source.split(marker, 1)[1].split("\nPY\n", 1)[0]

    def assemble(self, art, aa_control):
        env = os.environ.copy()
        env.update({
            "ARTIFACTS": str(art), "RUN_BENCH": "0", "REPEATS": "10",
            "RUN_LOAD": "1", "RUN_BROWSER": "0", "RUN_BUNDLE": "0",
            "AA_CONTROL": aa_control,
            "BASE_SHA": "a" * 40, "CANDIDATE_SHA": "a" * 40,
            "BASE_IMAGE": "img:a", "CANDIDATE_IMAGE": "img:a",
            "BASE_IMAGE_ID": "sha256:" + "11" * 32, "CANDIDATE_IMAGE_ID": "sha256:" + "11" * 32,
            "BASE_CLI_DIGEST": "sha256:cli", "CANDIDATE_CLI_DIGEST": "sha256:cli",
            "BASE_BUILT": "built", "CANDIDATE_BUILT": "built",
            "BASE_GO_VERSION": "go1.27.1", "CANDIDATE_GO_VERSION": "go1.27.1",
            "BASE_BUILDER_ID": "sha256:builder", "CANDIDATE_BUILDER_ID": "sha256:builder",
            "BASE_TOOLCHAIN": "builder:synthetic", "CANDIDATE_TOOLCHAIN": "builder:synthetic",
            "DOCKER_PLATFORM": "linux/arm64", "HOST_ID": "synthetic-host",
            "CATALOG_SHA": "c" * 64, "SETTINGS_SHA": "d" * 64,
            "BENCH_HARNESS_MANIFEST": str(art / "observations" / "benchmark-harness.json"),
        })
        return subprocess.run(
            [sys.executable, "-c", self.assembly_code()], env=env, capture_output=True, text=True
        )

    def test_assembly_orders_workloads_by_repeat_and_marks_a_a_control(self):
        with tempfile.TemporaryDirectory() as tmp:
            art = Path(tmp)
            (art / "observations").mkdir()
            for label in ("base", "candidate"):
                runs = art / label / "runs"
                runs.mkdir(parents=True)
                for repeat in range(1, 11):
                    stem = runs / f"closed-baseline-cold-{repeat}"
                    stem.with_suffix(".json").write_text(json.dumps({
                        "outcome": "passed", "duration_seconds": float(repeat),
                    }))
                    stem.with_suffix(".exit").write_text("0\n")
            result = self.assemble(art, "1")
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            assembled = json.loads((art / "comparison.json").read_text())
            self.assertEqual(assembled["control"], "a_a")
            samples = assembled["candidate"]["workloads"]["closed-baseline.cold"]["samples"]
            # Lexical order would put repeat 10 second.
            self.assertEqual([s["repeat"] for s in samples], list(range(1, 11)))
            self.assertEqual([s["value"] for s in samples], [float(r) for r in range(1, 11)])
            report = compare_doc(assembled)
            self.assertEqual(report["control"], "a_a")

            result = self.assemble(art, "0")
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIsNone(json.loads((art / "comparison.json").read_text())["control"])

    def test_assembly_carries_driver_slo_fields_and_run_settings(self):
        with tempfile.TemporaryDirectory() as tmp:
            art = Path(tmp)
            (art / "observations").mkdir()
            for label in ("base", "candidate"):
                runs = art / label / "runs"
                runs.mkdir(parents=True)
                for repeat in range(1, 11):
                    stem = runs / f"open-tiny-sustained-warm-{repeat}"
                    stem.with_suffix(".json").write_text(json.dumps({
                        "outcome": "passed", "duration_seconds": 50.0 + repeat,
                        "latency": {"p50_seconds": 1.5, "p99_seconds": 2.5},
                        "throughput": {"completed_per_second": 0.48, "verdict": "sustained"},
                        "backlog": {"slope_per_second": -0.01},
                        "counts": {"unreconciled": 0},
                    }))
                    stem.with_suffix(".exit").write_text("0\n")
            result = self.assemble(art, "0")
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            assembled = json.loads((art / "comparison.json").read_text())
            sample = assembled["candidate"]["workloads"]["open-tiny-sustained.warm"]["samples"][0]
            self.assertEqual(sample["value"], 51.0)
            self.assertEqual(sample["latency_p99_seconds"], 2.5)
            self.assertEqual(sample["completed_per_second"], 0.48)
            self.assertEqual(sample["sustained_verdict"], "sustained")
            self.assertEqual(sample["backlog_slope_per_second"], -0.01)
            self.assertEqual(sample["unreconciled"], 0)
            self.assertEqual(assembled["settings"]["repeats"], 10)
            self.assertEqual(assembled["settings"]["server_phase_first_side"], "base")
            self.assertTrue(assembled["measured_at"])
            # Carried fields are SLO inputs, never extra compared series.
            report = compare_doc(assembled)
            self.assertEqual([m["id"] for m in report["metrics"]
                              if m["family"] == "workload"], ["workload.open-tiny-sustained.warm.duration_seconds"])

    def test_a_a_control_reuses_the_candidate_build_only_for_one_sha_and_image(self):
        source = SH.read_text()
        start = source.index('BASE_BUILT="supplied"\nAA_CONTROL=0')
        end = source.index('if [[ -z "$BASE_WORKTREE" ]]; then')
        block = source[start:end]
        prelude = "\n".join([
            "log() { printf '%s\\n' \"$*\" >&2; }",
            "die() { printf 'DIE %s\\n' \"$*\"; exit 1; }",
            "build_release() { echo \"BUILD $1\"; return 0; }",
            "docker() { return 0; }",  # every image already present
            "git() { return 0; }",
            "mktemp() { echo /nonexistent; }",
            'ROOT=/nonexistent; BASE_WORKTREE=""; CANDIDATE_BUILT=built',
        ]) + "\n"
        cases = (
            ("a" * 40, "a" * 40, "img:x", "img:x", "AA=1 BASE_BUILT=built"),
            ("a" * 40, "b" * 40, "img:a", "img:b", "AA=0 BASE_BUILT=supplied"),
            ("a" * 40, "a" * 40, "img:base", "img:candidate", "AA=0 BASE_BUILT=supplied"),
        )
        for base_sha, cand_sha, base_img, cand_img, want in cases:
            script = prelude + (
                f"BASE_SHA={base_sha}\nCANDIDATE_SHA={cand_sha}\n"
                f"BASE_IMAGE={base_img}\nCANDIDATE_IMAGE={cand_img}\n"
            ) + block + 'echo "AA=$AA_CONTROL BASE_BUILT=$BASE_BUILT"\n'
            proc = subprocess.run(["bash", "-c", script], capture_output=True, text=True)
            self.assertIn(want, proc.stdout, proc.stdout + proc.stderr)

    def test_live_run_retains_per_sample_attribution_evidence(self):
        source = SH.read_text()
        self.assertIn('docker logs --timestamps "$name" >"$ARTIFACTS/$side/server-logs/${phase}-${idx}.log"', source)
        for call in ('stop_server "$side" cold "$r"', 'stop_server "$side" warm "$r"',
                     'stop_server "$side" browser "$r"'):
            self.assertIn(call, source)
        self.assertNotIn('stop_server "$side"\n', source)
        self.assertIn("docker events --filter type=container --filter type=image", source)
        # The events stream and container snapshot start before the first
        # benchmark sample, not only with the server phases.
        events_start = source.index("\nstart_docker_events\n")
        self.assertLess(events_start, source.index('bash "$ROOT/scripts/performance-benchmarks.sh"'))
        self.assertLess(source.index("docker-ps-start.txt"), events_start)
        self.assertIn("docker-ps-end.txt", source)
        for mark in ('timeline_mark "$phase" "$side" "$idx" start',
                     'timeline_mark "$phase" "$side" "$idx" end "$rc"'):
            self.assertIn(mark, source)
        self.assertEqual(source.count('timeline_mark browser "$side" "$r" start'), 2)
        self.assertEqual(source.count('timeline_mark browser "$side" "$r" end "$brc"'), 2)
        self.assertEqual(source.count(
            'CAESIUM_PERF_BROWSER_DIAGNOSTICS_OUT="$ARTIFACTS/$side/browser-diagnostics.jsonl"'), 2)
        self.assertEqual(source.count('CAESIUM_PERF_SIDE="$side" CAESIUM_PERF_REPEAT="$r"'), 2)
        spec = (ROOT / "ui/e2e/performance.spec.ts").read_text()
        for env_name in ("CAESIUM_PERF_BROWSER_DIAGNOSTICS_OUT", "CAESIUM_PERF_SIDE", "CAESIUM_PERF_REPEAT"):
            self.assertIn(f"process.env.{env_name}", spec)
        self.assertIn('trace: { mode: "on", screenshots: false, snapshots: false', spec)
        # Timing is read from the page clock, not from a runner stopwatch
        # around Playwright's 100/250/500 ms assertion retries.
        self.assertIn('record("action_to_render_ms", roundMs(renderMs)', spec)
        self.assertIn('clock: "click-event"', spec)
        self.assertIn('record("route_readiness_ms", roundMs(readyMs)', spec)
        self.assertNotIn('record("action_to_render_ms", renderMs)', spec)
        self.assertIn("--enable-precise-memory-info", spec)
        # The render-blocking third-party stylesheet is kept off the internet
        # in measured runs: warmed for the live first navigation, answered
        # locally where routing disables the cache.
        self.assertIn("const warmed = PERF_RUN ? await warmThirdPartyStylesheets(page, request) : [];", spec)
        self.assertIn('third_party_css: PERF_RUN ? "empty-stylesheet" : "live"', spec)
        # Readiness is stamped after the visibility check (which forces the
        # pending layout), never before it; the spec's own regression test
        # proves the cost reaches the recorded time.
        self.assertIn("probe.headings.push({ text, t: performance.now() });", spec)
        self.assertIn("probe.firsts[key] = performance.now();", spec)
        self.assertNotIn("const t = performance.now();\n    pending = false;", spec)
        self.assertIn('test("render probe: work done by the visibility check reaches the recorded time"', spec)
        # Long-session memory runs in ONE document via the real sidebar links.
        session = spec.split('test("live long-session memory', 1)[1].split("\ntest(", 1)[0]
        self.assertNotIn("page.goto(path)", session)
        self.assertEqual(session.count("page.goto("), 1)
        self.assertIn("aside nav a[href=", session)
        self.assertIn('"in-app navigation replaced the document"', session)
        self.assertIn('navigation: "in-app"', session)


# ---------------------------------------------------------------------------
# E4: budgeted decision rule, fixed baseline, SLOs, bounded reruns
# ---------------------------------------------------------------------------

BUDGETS_PATH = ROOT / "test/performance/budgets.json"
BASELINE_PATH = ROOT / "test/performance/baseline.json"


def real_budgets():
    return json.loads(BUDGETS_PATH.read_text())


def signed(budgets):
    """Re-sign a mutated budgets copy as a new reviewed change (test-only)."""
    budgets = json.loads(json.dumps(budgets))
    budgets["changes"].append({
        "version": len(budgets["changes"]) + 1,
        "date": "2026-09-27",
        "rationale": "synthetic test budgets",
        "evidence": "scripts/test_compare_performance.py",
        "reviewed_in": "test",
        "values_sha256": "",
    })
    budgets["changes"][-1]["values_sha256"] = COMPARE["budget_values_sha256"](budgets)
    return COMPARE["validate_budgets"](budgets)


def test_budgets(slos=None, max_reruns=None):
    budgets = real_budgets()
    if slos is not None:
        budgets["slos"] = slos
    if max_reruns is not None:
        budgets["rerun_policy"]["max_reruns"] = max_reruns
    return signed(budgets)


def budgeted(doc, budgets=None, baseline=None, baseline_error=None, attempt=1):
    return COMPARE["compare"](
        doc,
        budgets=budgets if budgets is not None else test_budgets(slos=[]),
        baseline=baseline,
        baseline_error=baseline_error,
        attempt=attempt,
    )


def shifted(center, ratio, n=10, cv_pct=1.0, phase=0):
    """Deterministic samples: a fixed spread pattern around center*ratio."""
    pattern = [-1.6, -1.1, -0.7, -0.3, 0.0, 0.2, 0.5, 0.8, 1.2, 1.5, -0.9, 0.9]
    out = []
    for i in range(n):
        step = pattern[(i + phase) % len(pattern)]
        out.append(center * ratio * (1.0 + cv_pct / 100.0 * step))
    return out


def bench_family_doc(base_centers, cand_ratios, cv_pct=1.0):
    """Benchmarks only: every series of one family, like one go test process."""
    base = side("base", workloads={}, browser={}, bundle={}, system={})
    cand = side("candidate", workloads={}, browser={}, bundle={}, system={})
    base["benchmarks"] = {}
    cand["benchmarks"] = {}
    for index, (name, center) in enumerate(sorted(base_centers.items())):
        base["benchmarks"][name] = {
            "ns_per_op": shifted(center, 1.0, cv_pct=cv_pct, phase=index),
            "bytes_per_op": [400.0] * 10,
            "allocs_per_op": [12.0] * 10,
        }
        cand["benchmarks"][name] = {
            "ns_per_op": shifted(center, cand_ratios.get(name, 1.0), cv_pct=cv_pct, phase=index + 3),
            "bytes_per_op": [400.0] * 10,
            "allocs_per_op": [12.0] * 10,
        }
    doc = document(base, cand)
    doc["required_families"] = ["benchmark"]
    return doc


BENCH_NAMES = {f"BenchmarkOwner{name}": 10_000.0 + 1_000.0 * i
               for i, name in enumerate("ABCDEFGHIJK")}


class BudgetFileReviewTests(unittest.TestCase):
    """Budget values change only with a visible, reviewed changes entry."""

    def test_committed_budgets_are_complete_and_reviewed(self):
        budgets = COMPARE["load_budgets"](BUDGETS_PATH)
        self.assertEqual(budgets["schema_version"], 1)
        for change in budgets["changes"]:
            for key in ("date", "rationale", "evidence", "reviewed_in", "values_sha256"):
                self.assertTrue(str(change.get(key, "")).strip(), (key, change))
        self.assertEqual(budgets["changes"][-1]["values_sha256"], COMPARE["budget_values_sha256"](budgets))
        calibration = budgets["calibration"]
        for key in ("runner", "runs", "limitations"):
            self.assertIn(key, calibration)
        self.assertIn("host_id", calibration["runner"])
        self.assertIn("Q2", json.dumps(calibration["limitations"]))
        for family in ("workload", "benchmark", "browser", "system"):
            for rule in budgets["families"][family]["rules"]:
                self.assertNotIn("PROVISIONAL", rule["rationale"])

    def test_value_change_without_a_changes_entry_is_refused(self):
        budgets = real_budgets()
        budgets["families"]["benchmark"]["rules"][0]["max_relative_degradation"] += 0.05
        with self.assertRaisesRegex(COMPARE["CompareError"], "last reviewed change"):
            COMPARE["validate_budgets"](budgets)
        # The CLI refuses it too (usage error, never a silent comparison).
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "budgets.json"
            path.write_text(json.dumps(budgets))
            proc = run_cli("--budgets", str(path), input_doc=document())
            self.assertEqual(proc.returncode, 1, proc.stderr)
            self.assertIn("last reviewed change", proc.stderr)
        # A reviewed entry with rationale, evidence and review makes it valid.
        self.assertTrue(signed(budgets))

    def test_changes_entry_needs_rationale_evidence_and_review(self):
        for key in ("rationale", "evidence", "reviewed_in"):
            budgets = signed(real_budgets())
            budgets["changes"][-1][key] = " "
            with self.assertRaisesRegex(COMPARE["CompareError"], key):
                COMPARE["validate_budgets"](budgets)

    def test_descriptions_and_calibration_notes_are_not_budget_values(self):
        budgets = real_budgets()
        budgets["description"] += " edited"
        budgets["calibration"]["note"] = "more evidence"
        COMPARE["validate_budgets"](budgets)

    def test_every_committed_budget_revision_is_logged(self):
        """Against git history: a committed value change needs a new entry."""
        log = subprocess.run(
            ["git", "-C", str(ROOT), "log", "--format=%H", "--", "test/performance/budgets.json"],
            capture_output=True, text=True,
        )
        if log.returncode != 0:
            self.skipTest("git history unavailable")
        revisions = [sha for sha in log.stdout.split() if sha]
        versions = []
        for sha in reversed(revisions):
            shown = subprocess.run(
                ["git", "-C", str(ROOT), "show", f"{sha}:test/performance/budgets.json"],
                capture_output=True, text=True,
            )
            if shown.returncode == 0:
                versions.append((sha, json.loads(shown.stdout)))
        versions.append(("working-tree", real_budgets()))
        for (_, older), (label, newer) in zip(versions, versions[1:]):
            old_digest = COMPARE["budget_values_sha256"](older)
            new_digest = COMPARE["budget_values_sha256"](newer)
            if old_digest == new_digest:
                continue
            self.assertGreater(len(newer["changes"]), len(older["changes"]),
                               f"{label} changed budget values without a new changes entry")
            self.assertEqual(newer["changes"][-1]["values_sha256"], new_digest, label)
            self.assertEqual(newer["changes"][:len(older["changes"])], older["changes"],
                             f"{label} rewrote earlier reviewed changes")

    def test_bundle_budgets_stay_in_lockstep_with_the_mjs_check(self):
        absolute = real_budgets()["bundle"]["absolute_budgets"]
        mjs = MJS.read_text()
        for key, env in (("largest_js_raw_bytes", "BUNDLE_MAX_BYTES"),
                         ("largest_js_gzip_bytes", "BUNDLE_MAX_GZIP_BYTES"),
                         ("total_raw_bytes", "BUNDLE_TOTAL_MAX_BYTES"),
                         ("total_gzip_bytes", "BUNDLE_TOTAL_MAX_GZIP_BYTES")):
            self.assertRegex(mjs, rf'process\.env\.{env} \?\? "{absolute[key]}"')
        self.assertEqual(absolute["largest_js_raw_bytes"], COMPARE["DEFAULT_LARGEST_JS_RAW"])
        self.assertEqual(absolute["largest_js_gzip_bytes"], COMPARE["DEFAULT_LARGEST_JS_GZIP"])
        self.assertEqual(absolute["total_raw_bytes"], COMPARE["DEFAULT_TOTAL_RAW"])
        self.assertEqual(absolute["total_gzip_bytes"], COMPARE["DEFAULT_TOTAL_GZIP"])
        slo_bundle = {s["bundle"]: s["max"] for s in real_budgets()["slos"] if "bundle" in s}
        self.assertEqual(slo_bundle, {k: v for k, v in absolute.items() if k in COMPARE["BUNDLE_KEYS"]})

    def test_committed_baseline_matches_the_budgets_setup(self):
        baseline = json.loads(BASELINE_PATH.read_text())
        self.assertEqual(baseline["schema_version"], 1)
        prov = baseline["provenance"]
        for field in real_budgets()["fixed_baseline"]["must_match"]:
            self.assertTrue(prov.get(field), field)
        self.assertIs(prov["instrumented"], False)
        self.assertIs(prov["built_by_this_run"], True)
        self.assertIn(baseline["report"]["target_base"], COMPARE["PASS_VERDICTS"])
        for key in ("cpu_model", "cpu_count", "memory_bytes", "os_version", "docker", "co_tenant_containers"):
            self.assertIn(key, baseline["runner"])
        self.assertEqual(baseline["runner"]["host_id"], real_budgets()["calibration"]["runner"]["host_id"])
        for metric_id, body in baseline["metrics"].items():
            self.assertEqual(body["n"], len(body["samples"]), metric_id)
            self.assertGreaterEqual(body["n"], 10, metric_id)
            for key in ("median", "mad", "iqr", "p95", "p99"):
                self.assertIn(key, body, metric_id)


class BudgetStatisticsTests(unittest.TestCase):
    def test_holm_adjustment(self):
        adjusted = COMPARE["holm_adjust"]({"a": 0.01, "b": 0.04, "c": 0.03})
        self.assertAlmostEqual(adjusted["a"], 0.03)
        self.assertAlmostEqual(adjusted["c"], 0.06)
        self.assertAlmostEqual(adjusted["b"], 0.06)  # monotone: never below c

    def test_moses_bounds_cover_the_shift(self):
        base = [float(x) for x in range(100, 110)]
        cand = [x + 5.0 for x in base]
        hl, lower, upper = COMPARE["moses_shift_bounds"](base, cand, 0.05)
        self.assertAlmostEqual(hl, 5.0)
        self.assertLess(lower, 5.0)
        self.assertGreater(upper, 5.0)
        # Too few samples to exclude a single pairwise difference.
        self.assertEqual(COMPARE["moses_shift_bounds"]([1.0, 2.0], [3.0, 4.0], 0.01)[1:], (None, None))

    def test_degradation_is_a_ratio_and_direction_aware(self):
        base = shifted(100.0, 1.0)
        cand = shifted(100.0, 1.2)
        slower = COMPARE["degradation_estimate"](base, cand, True, 0.05)
        self.assertAlmostEqual(slower["point"], 1.2, places=2)
        # The same move is an improvement for a higher-is-better series.
        faster = COMPARE["degradation_estimate"](base, cand, False, 0.05)
        self.assertAlmostEqual(faster["point"], 1 / 1.2, places=2)
        self.assertLess(faster["upper"], 1.0)

    def test_one_sided_mann_whitney(self):
        greater, less = COMPARE["mann_whitney_one_sided"](shifted(10, 1.0), shifted(10, 1.3))
        self.assertLess(greater, 0.001)
        self.assertGreater(less, 0.99)


class BudgetDecisionTests(unittest.TestCase):
    def test_a_a_like_input_is_no_significant_difference(self):
        # Same distribution on both sides, samples in a different order, in
        # every sampled family: exactly the same-code control case.
        doc = document()
        for label, phase in (("base", 0), ("candidate", 5)):
            # Per-sample driver fields, as scripts/performance.sh records them:
            # a measured workload must carry every SLO field it is judged on.
            doc[label]["workloads"] = {"closed-baseline.warm": {"phase": "warm", "samples": [
                {"value": v, "outcome": "passed", "latency_p99_seconds": 1.5}
                for v in shifted(2.2, 1.0, cv_pct=7, phase=phase)
            ]}}
            doc[label]["benchmarks"] = {
                name: {"ns_per_op": shifted(center, 1.0, cv_pct=3, phase=phase + i),
                       "bytes_per_op": [400.0] * 10, "allocs_per_op": [12.0] * 10}
                for i, (name, center) in enumerate(sorted(BENCH_NAMES.items()))
            }
            doc[label]["browser"] = {"route_readiness_ms": {"/jobs": shifted(77, 1.0, cv_pct=3, phase=phase)},
                                     "action_to_render_ms": shifted(66, 1.0, cv_pct=2, phase=phase),
                                     "long_session_heap_bytes": shifted(14e6, 1.0, cv_pct=0.7, phase=phase)}
            doc[label]["system"] = {}
        doc = document(doc["base"], doc["candidate"])
        report = budgeted(doc)
        self.assertEqual(report["overall"], "no_significant_difference", report["reasons"])
        self.assertEqual(report["reruns"]["status"], "resolved")
        self.assertIn("not equivalence", report["reasons"][0])
        proc = run_cli("--budgets", str(BUDGETS_PATH), input_doc=doc)
        self.assertEqual(proc.returncode, 0, proc.stderr)

    def test_correlated_small_shifts_do_not_fail_on_uncorrected_p_values(self):
        # The W7-beta A/A shape: six ns/op series from one process move ~2.5%
        # together, several with uncorrected p < 0.05.
        ratios = {name: 1.025 for name in list(sorted(BENCH_NAMES))[:6]}
        doc = bench_family_doc(BENCH_NAMES, ratios, cv_pct=1.5)
        report = budgeted(doc)
        family = report["decision"]["target_base"]["families"]["benchmark"]
        self.assertGreaterEqual(family["uncorrected_significant"], 1)
        self.assertIn(report["overall"], ("no_significant_difference", "within_budget"))
        self.assertNotIn("slower", family["counts"])
        self.assertEqual(COMPARE["EXIT_BY_OVERALL"][report["overall"]], 0)
        self.assertGreater(family["geomean_degradation"], 1.0)
        # The same data under E3's uncorrected rule reports it as slower.
        self.assertEqual(report["uncorrected_overall"], "slower")

    def test_one_material_regression_among_many_series_fails(self):
        ratios = {"BenchmarkOwnerC": 1.30}
        doc = bench_family_doc(BENCH_NAMES, ratios, cv_pct=1.5)
        report = budgeted(doc)
        self.assertEqual(report["overall"], "slower", report["reasons"])
        slow = [s for s in report["decision"]["target_base"]["series"] if s["verdict"] == "slower"]
        self.assertEqual([s["id"] for s in slow], ["bench.BenchmarkOwnerC.ns_per_op"])
        self.assertEqual(report["reruns"]["status"], "resolved")  # never rerun a regression
        proc = run_cli("--budgets", str(BUDGETS_PATH), input_doc=doc)
        self.assertEqual(proc.returncode, 4, proc.stderr)

    def test_known_faster_is_faster_with_tradeoffs_listed(self):
        ratios = {"BenchmarkOwnerA": 0.7, "BenchmarkOwnerB": 1.01}
        report = budgeted(bench_family_doc(BENCH_NAMES, ratios, cv_pct=1.0))
        self.assertEqual(report["overall"], "faster", report["reasons"])
        claim = report["decision"]["optimization_claim"]
        self.assertEqual(claim["improved_series"], ["bench.BenchmarkOwnerA.ns_per_op"])
        self.assertIn("bench.BenchmarkOwnerB.ns_per_op", [t["id"] for t in claim["tradeoff_candidates"]])
        self.assertIn("tradeoffs", claim["note"])

    def test_significant_slowdown_within_budget_is_reported_not_failed(self):
        doc = bench_family_doc({"BenchmarkOwnerA": 10_000.0}, {"BenchmarkOwnerA": 1.03}, cv_pct=0.5)
        report = budgeted(doc)
        self.assertEqual(report["overall"], "within_budget", report["reasons"])
        self.assertEqual(COMPARE["EXIT_BY_OVERALL"]["within_budget"], 0)

    def test_unestablished_bound_is_inconclusive_and_rerun(self):
        margin = real_budgets()["families"]["benchmark"]["rules"][0]["max_relative_degradation"]
        doc = bench_family_doc({"BenchmarkOwnerA": 10_000.0}, {"BenchmarkOwnerA": 1.0 + margin * 0.8}, cv_pct=4.5)
        report = budgeted(doc)
        series = {s["id"]: s for s in report["decision"]["target_base"]["series"]}
        self.assertEqual(series["bench.BenchmarkOwnerA.ns_per_op"]["verdict"], "inconclusive")
        self.assertEqual(report["overall"], "inconclusive", report["reasons"])
        self.assertTrue(report["reruns"]["rerun_required"])
        self.assertEqual(report["reruns"]["next_attempt"], 2)
        self.assertTrue(any("not established" in r or "noisy" in r for r in report["reasons"]))

    def test_noisy_series_is_inconclusive_until_the_rerun_budget_is_exhausted(self):
        doc = workload_only(InformationalDiagnosticsTests.COLD_BASE, InformationalDiagnosticsTests.COLD_CANDIDATE)
        budgets = test_budgets(slos=[])
        allowed = budgets["rerun_policy"]["max_reruns"] + 1
        for attempt in range(1, allowed):
            report = budgeted(doc, budgets, attempt=attempt)
            self.assertEqual(report["overall"], "inconclusive")
            self.assertEqual(report["reruns"]["status"], "rerun_required")
        report = budgeted(doc, budgets, attempt=allowed)
        self.assertEqual(report["overall"], "inconclusive_unresolved")
        self.assertEqual(report["reruns"]["status"], "exhausted")
        self.assertTrue(report["strict_gate"]["blocking"])
        self.assertTrue(any("noisy" in r for r in report["reasons"]))
        with self.assertRaisesRegex(COMPARE["CompareError"], "attempt must be between"):
            budgeted(doc, budgets, attempt=allowed + 1)
        proc = run_cli("--budgets", str(BUDGETS_PATH), "--attempt", str(allowed), input_doc=doc)
        self.assertEqual(proc.returncode, 3, proc.stderr)
        self.assertEqual(json.loads(proc.stdout)["overall"], "inconclusive_unresolved")

    def test_non_inferiority_alpha_is_split_across_allowed_attempts(self):
        budgets = test_budgets(slos=[])
        report = budgeted(document(), budgets)
        allowed = budgets["rerun_policy"]["max_reruns"] + 1
        self.assertAlmostEqual(report["decision"]["alpha_non_inferiority_per_attempt"],
                               budgets["decision"]["alpha_non_inferiority"] / allowed)

    def test_undersampled_is_inconclusive(self):
        doc = workload_only([6.4, 6.5, 6.3, 6.6], [6.4, 6.5, 6.3, 6.6])
        report = budgeted(doc)
        self.assertEqual(report["overall"], "inconclusive")
        self.assertTrue(any("undersampled" in r for r in report["reasons"]))

    def test_mismatched_host_is_unresolved_without_a_rerun(self):
        doc = document()
        doc["candidate"]["provenance"]["host_id"] = "other-host"
        report = budgeted(doc)
        self.assertEqual(report["overall"], "inconclusive_unresolved")
        self.assertEqual(report["reruns"]["status"], "unresolved_not_rerunnable")
        self.assertTrue(any("host_id differs" in r for r in report["reasons"]))

    def test_correctness_failure_aborts_before_budgets(self):
        doc = document()
        doc["candidate"]["correctness"] = {"ok": False, "failures": ["run 2 failed"]}
        report = budgeted(doc)
        self.assertEqual(report["overall"], "fail")
        self.assertFalse(report["speed_compared"])
        self.assertEqual(report["decision"]["slos"]["verdict"], "not_evaluated")
        self.assertEqual(report["reruns"]["status"], "resolved")

    def test_higher_is_better_series_degrades_when_it_drops(self):
        base = side("base", workloads={}, benchmarks={}, browser={}, bundle={},
                    system={"throughput": {"samples": shifted(50.0, 1.0), "lower_is_better": False}})
        cand = side("candidate", workloads={}, benchmarks={}, browser={}, bundle={},
                    system={"throughput": {"samples": shifted(50.0, 0.6), "lower_is_better": False}})
        report = budgeted(document(base, cand))
        self.assertEqual(report["overall"], "slower", report["reasons"])

    def test_series_without_a_rule_fails_closed(self):
        result = COMPARE["evaluate_series"](
            "mystery.series", {"samples": [1.0] * 10}, {"samples": [1.0] * 10}, None, None, 0.05)
        self.assertEqual(result["status"], "fail")
        self.assertIsNone(COMPARE["rule_for"](real_budgets(), "mystery", "mystery.series"))

    def test_bundle_growth_beyond_budget_is_slower(self):
        doc = document()
        doc["candidate"]["bundle"] = {"largest_js_raw_bytes": 800_000, "total_raw_bytes": 2_200_000}
        report = budgeted(doc)
        self.assertEqual(report["overall"], "slower")
        self.assertTrue(any("total_raw_bytes grew" in r for r in report["reasons"]))


class BudgetSloTests(unittest.TestCase):
    def test_series_slo_breach_fails_even_when_the_comparison_is_neutral(self):
        slo = {"id": "t-readiness", "series": "browser.route_readiness_ms./jobs.live", "statistic": "p90",
               "max": 50.0, "min_samples": 10, "unit": "ms", "rationale": "test"}
        report = budgeted(document(), test_budgets(slos=[slo]))
        self.assertEqual(report["decision"]["target_base"]["verdict"], "no_significant_difference")
        self.assertEqual(report["overall"], "slo_breach")
        self.assertEqual(COMPARE["EXIT_BY_OVERALL"]["slo_breach"], 4)

    def test_breach_on_a_noisy_run_is_rerun_not_terminal(self):
        slo = {"id": "t-readiness", "series": "browser.route_readiness_ms./jobs.live", "statistic": "p90",
               "max": 50.0, "min_samples": 10, "unit": "ms", "rationale": "test"}
        doc = document()
        # Host contention: the benchmark series swings far beyond its max_cv.
        noisy = [10_000.0, 30_000.0] * 5
        doc["candidate"]["benchmarks"]["BenchmarkOwnerApplyCompletionLinear64"]["ns_per_op"] = noisy
        report = budgeted(doc, test_budgets(slos=[slo]))
        self.assertEqual(report["decision"]["slos"]["verdict"], "inconclusive")
        self.assertEqual(report["overall"], "inconclusive")
        self.assertTrue(report["reruns"]["rerun_required"])
        self.assertTrue(any("noisy run" in r for r in report["reasons"]))

    def test_p90_excludes_exactly_one_stall_in_ten(self):
        values = [6.5] * 9 + [32.5]
        slo = {"id": "cold", "workload": "closed-baseline.cold", "field": "latency_p99_seconds",
               "statistic": "p90", "max": 10.0, "min_samples": 10, "rationale": "test"}
        doc = workload_only([], [])
        doc["base"]["workloads"] = {"closed-baseline.cold": {"phase": "cold", "samples": shifted(6.5, 1.0)}}
        doc["candidate"]["workloads"] = {"closed-baseline.cold": {"phase": "cold", "samples": [
            {"value": v, "outcome": "passed", "latency_p99_seconds": lat}
            for v, lat in zip(shifted(6.5, 1.0), values)
        ]}}
        report = budgeted(doc, test_budgets(slos=[slo]))
        self.assertEqual(report["decision"]["slos"]["results"][0]["verdict"], "pass")
        doc["candidate"]["workloads"]["closed-baseline.cold"]["samples"][0]["latency_p99_seconds"] = 30.0
        report = budgeted(doc, test_budgets(slos=[slo]))
        self.assertEqual(report["decision"]["slos"]["results"][0]["verdict"], "breach")
        self.assertEqual(report["overall"], "slo_breach")

    def test_floor_and_equality_slos(self):
        slos = [
            {"id": "rate", "workload": "open", "field": "completed_per_second", "statistic": "p90",
             "min": 0.4, "min_samples": 10, "rationale": "test"},
            {"id": "sustained", "workload": "open", "field": "sustained_verdict", "statistic": "all",
             "equals": "sustained", "min_samples": 10, "rationale": "test"},
        ]
        samples = [{"value": 60.0, "outcome": "passed", "completed_per_second": 0.5,
                    "sustained_verdict": "sustained"} for _ in range(10)]
        doc = document()
        doc["candidate"]["workloads"]["open"] = {"phase": "warm", "samples": samples}
        doc["base"]["workloads"]["open"] = {"phase": "warm", "samples": [dict(s) for s in samples]}
        report = budgeted(doc, test_budgets(slos=slos))
        self.assertEqual([r["verdict"] for r in report["decision"]["slos"]["results"]], ["pass", "pass"])
        samples[3]["sustained_verdict"] = "growing"
        samples[4]["completed_per_second"] = 0.1
        samples[5]["completed_per_second"] = 0.1
        report = budgeted(doc, test_budgets(slos=slos))
        self.assertEqual([r["verdict"] for r in report["decision"]["slos"]["results"]], ["breach", "breach"])

    def test_undersampled_slo_is_unresolved_and_unmeasured_slo_is_reported(self):
        slos = [
            {"id": "few", "series": "workload.closed-baseline.duration_seconds", "statistic": "p90",
             "max": 1000.0, "min_samples": 20, "rationale": "test"},
            {"id": "absent", "series": "workload.never.cold.duration_seconds", "statistic": "p90",
             "max": 1.0, "min_samples": 10, "rationale": "test"},
        ]
        report = budgeted(document(), test_budgets(slos=slos))
        self.assertEqual(report["overall"], "inconclusive_unresolved")
        self.assertEqual(report["decision"]["slos"]["not_evaluated"], ["absent"])


class FixedBaselineTests(unittest.TestCase):
    def baseline_from(self, doc):
        report = budgeted(doc)
        doc["candidate"]["provenance"]["built_by_this_run"] = True
        return COMPARE["record_baseline"](doc, report, runner={"host_id": "perf-host-1"},
                                          recorded_at="2026-09-27T00:00:00Z")

    def test_recorded_baseline_round_trips_and_exposes_cumulative_regression(self):
        reference = document()
        baseline = self.baseline_from(reference)
        self.assertEqual(baseline["provenance"]["host_id"], "perf-host-1")
        metric = baseline["metrics"]["bench.BenchmarkOwnerApplyCompletionLinear64.ns_per_op"]
        self.assertEqual(metric["n"], 10)
        self.assertEqual(len(metric["samples"]), 10)
        for key in ("median", "mad", "iqr", "p95", "p99"):
            self.assertIn(key, metric)

        report = budgeted(document(), baseline=baseline)
        self.assertEqual(report["decision"]["fixed_baseline"]["verdict"], "no_significant_difference")
        self.assertEqual(report["overall"], "no_significant_difference")
        self.assertFalse(report["strict_gate"]["blocking"])

        # Base and candidate moved together (each PR was within budget), so the
        # target-base comparison passes; the fixed baseline sees the drift.
        drifted = document()
        for label in ("base", "candidate"):
            drifted[label]["benchmarks"]["BenchmarkOwnerApplyCompletionLinear64"]["ns_per_op"] = around(13_000.0)
        report = budgeted(drifted, baseline=baseline)
        self.assertEqual(report["decision"]["target_base"]["verdict"], "no_significant_difference")
        self.assertEqual(report["decision"]["fixed_baseline"]["verdict"], "slower")
        self.assertEqual(report["overall"], "slower")

    def test_cumulative_improvement_does_not_become_an_optimization_claim(self):
        baseline = self.baseline_from(document())
        improved = document()
        for label in ("base", "candidate"):
            improved[label]["benchmarks"]["BenchmarkOwnerApplyCompletionLinear64"]["ns_per_op"] = around(7_000.0)
        report = budgeted(improved, baseline=baseline)
        self.assertEqual(report["decision"]["fixed_baseline"]["verdict"], "faster")
        self.assertEqual(report["overall"], "no_significant_difference")

    def test_missing_baseline_is_unresolved(self):
        report = budgeted(document(), baseline_error="fixed baseline is missing: /nowhere")
        self.assertEqual(report["decision"]["fixed_baseline"]["verdict"], "inconclusive")
        self.assertEqual(report["overall"], "inconclusive_unresolved")
        with tempfile.TemporaryDirectory() as tmp:
            budgets = Path(tmp) / "budgets.json"
            budgets.write_text(json.dumps(test_budgets(slos=[])))
            proc = run_cli("--budgets", str(budgets), "--baseline", str(Path(tmp) / "none.json"),
                           input_doc=document())
            self.assertEqual(proc.returncode, 3, proc.stderr)
            self.assertIn("fixed baseline is missing", proc.stdout)

    def test_baseline_from_another_host_is_refused_not_compared(self):
        baseline = self.baseline_from(document())
        baseline["provenance"]["host_id"] = "ci-runner-7"
        slower = document()
        slower["candidate"]["benchmarks"]["BenchmarkOwnerApplyCompletionLinear64"]["ns_per_op"] = around(30_000.0)
        slower["base"]["benchmarks"]["BenchmarkOwnerApplyCompletionLinear64"]["ns_per_op"] = around(30_000.0)
        report = budgeted(slower, baseline=baseline)
        fixed = report["decision"]["fixed_baseline"]
        self.assertEqual(fixed["verdict"], "inconclusive")
        self.assertNotIn("series", fixed)  # never silently compared
        self.assertTrue(any("host_id differs" in r for r in fixed["reasons"]))
        self.assertEqual(report["overall"], "inconclusive_unresolved")

    def test_new_series_needs_a_re_recorded_baseline(self):
        baseline = self.baseline_from(document())
        del baseline["metrics"]["browser.action_to_render_ms.live"]
        report = budgeted(document(), baseline=baseline)
        self.assertEqual(report["decision"]["fixed_baseline"]["verdict"], "inconclusive")
        self.assertFalse(report["decision"]["fixed_baseline"]["rerun_eligible"])
        self.assertEqual(report["overall"], "inconclusive_unresolved")

    def test_recording_refuses_a_failed_or_unverified_run(self):
        doc = document()
        doc["candidate"]["correctness"] = {"ok": False, "failures": ["boom"]}
        with self.assertRaisesRegex(COMPARE["CompareError"], "target-base verdict"):
            COMPARE["record_baseline"](doc, budgeted(doc))
        doc = document()
        with self.assertRaisesRegex(COMPARE["CompareError"], "did not build"):
            COMPARE["record_baseline"](doc, budgeted(doc))
        doc["candidate"]["provenance"]["built_by_this_run"] = True
        doc["candidate"]["provenance"]["instrumented"] = True
        with self.assertRaisesRegex(COMPARE["CompareError"], "instrumented"):
            COMPARE["record_baseline"](doc, budgeted(document()))

    def test_record_baseline_cli(self):
        doc = document()
        doc["candidate"]["provenance"]["built_by_this_run"] = True
        doc["settings"] = {"repeats": 10}
        doc["measured_at"] = "2026-09-27T12:00:00+00:00"
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "baseline.json"
            runner = Path(tmp) / "host.json"
            runner.write_text(json.dumps({"host_id": "perf-host-1", "cpu_model": "synthetic"}))
            proc = run_cli("--budgets", str(BUDGETS_PATH), "--record-baseline", str(out),
                           "--runner-json", str(runner), input_doc=doc)
            self.assertEqual(proc.returncode, 0, proc.stderr)
            recorded = json.loads(out.read_text())
            self.assertEqual(recorded["runner"]["cpu_model"], "synthetic")
            self.assertEqual(recorded["settings"], {"repeats": 10})
            self.assertEqual(recorded["recorded_at"], "2026-09-27T12:00:00+00:00")
            self.assertEqual(recorded["source"]["base_git_sha"], "a" * 40)


def a_a_document():
    """One build of one source on both sides, as performance.sh records an A/A control."""
    prov = provenance("candidate")
    doc = document(side("base", provenance=dict(prov)), side("candidate", provenance=dict(prov)))
    doc["control"] = "a_a"
    manifest = doc["benchmark_harness"]
    manifest["base_overlay_paths"] = []
    for entry in manifest["files"]:
        entry["overlaid"] = False
        entry["base_original_sha256"] = entry["sha256"]
    return doc


class CalibrationTests(unittest.TestCase):
    def a_a(self, phase):
        doc = a_a_document()
        doc["candidate"]["workloads"]["closed-baseline"]["samples"] = shifted(100.0, 1.0, cv_pct=2, phase=phase)
        return doc

    def test_calibration_summarizes_control_runs_and_cross_run_drift(self):
        budgets = test_budgets(slos=[])
        summary = COMPARE["calibrate"]([("aa1", self.a_a(1)), ("aa2", self.a_a(4)), ("aa3", self.a_a(7))], budgets)
        self.assertEqual(summary["control_rates"]["runs"], 3)
        self.assertEqual(summary["control_rates"]["false_fail"], 0.0)
        self.assertEqual(len(summary["cross_run_fixed_baseline"]["pairs"]), 6)
        series = summary["series"]["workload.closed-baseline.duration_seconds"]
        for key in ("sigma_log_median", "supported_margin", "min_samples_for_margin", "max_cv"):
            self.assertIn(key, series)
        self.assertIsNotNone(summary["pooled_uncorrected_p"]["ks_uniform_p"])

    def test_calibration_refuses_non_control_runs(self):
        with self.assertRaisesRegex(COMPARE["CompareError"], "not an A/A control"):
            COMPARE["calibrate"]([("ab", document())], test_budgets(slos=[]))

    def test_the_a_a_fixture_is_a_passing_same_build_control(self):
        doc = a_a_document()
        self.assertEqual(COMPARE["a_a_identity_issues"](doc), [])
        self.assertEqual(budgeted(doc)["overall"], "no_significant_difference")

    def test_calibration_refuses_a_mislabeled_a_b_document(self):
        # The A/B fixture (different git_sha, image and ref per side) labelled
        # a_a: compare() accepts different builds, so only the identity check
        # keeps a real product change out of the control rates and variance.
        budgets = test_budgets(slos=[])
        mislabeled = document()
        mislabeled["control"] = "a_a"
        with self.assertRaisesRegex(COMPARE["CompareError"], "provenance.git_sha differs") as caught:
            COMPARE["calibrate"]([("aa1", self.a_a(1)), ("mislabeled", mislabeled)], budgets)
        self.assertIn("mislabeled is not an A/A control run", str(caught.exception))
        self.assertIn("provenance.image_id differs", str(caught.exception))
        self.assertNotIn("aa1 is not", str(caught.exception))
        with tempfile.TemporaryDirectory() as tmp:
            budgets_path = Path(tmp) / "budgets.json"
            budgets_path.write_text(json.dumps(budgets))
            good = Path(tmp) / "aa1.json"
            good.write_text(json.dumps(self.a_a(1)))
            bad = Path(tmp) / "mislabeled.json"
            bad.write_text(json.dumps(mislabeled))
            proc = run_cli("--budgets", str(budgets_path), "--calibrate", str(good), str(bad))
            self.assertEqual(proc.returncode, 1, proc.stderr)
            self.assertEqual(proc.stdout, "")  # nothing summarized, nothing pooled
            self.assertIn("image_id differs", proc.stderr)
            proc = run_cli("--budgets", str(budgets_path), "--calibrate", str(good))
            self.assertEqual(proc.returncode, 0, proc.stderr)
            self.assertEqual(json.loads(proc.stdout)["control_rates"]["runs"], 1)

    def test_every_identity_field_must_match_and_be_present(self):
        budgets = test_budgets(slos=[])
        for field in COMPARE["A_A_IDENTITY"]:
            for value, want in (("sha256:" + "99" * 32, f"provenance.{field} differs"),
                                (None, f"provenance.{field} is missing")):
                with self.subTest(field=field, value=value):
                    doc = a_a_document()
                    doc["candidate"]["provenance"][field] = value
                    with self.assertRaisesRegex(COMPARE["CompareError"], want):
                        COMPARE["calibrate"]([("aa", doc)], budgets)

    def test_calibration_refuses_a_same_build_document_without_the_label(self):
        doc = a_a_document()
        del doc["control"]
        with self.assertRaisesRegex(COMPARE["CompareError"], "control=None, want 'a_a'"):
            COMPARE["calibrate"]([("unlabelled", doc)], test_budgets(slos=[]))


def strict_json(text):
    """Parse as standard JSON: NaN/Infinity tokens are an error."""
    def refuse(token):
        raise ValueError(f"non-standard JSON constant {token}")
    return json.loads(text, parse_constant=refuse)


def slo_workload_doc(latency=3.8, duration=3.9):
    """A closed-baseline warm run as performance.sh assembles it, workload family only."""
    doc = workload_only([], [])
    doc["required_families"] = ["workload"]
    for label in ("base", "candidate"):
        doc[label]["workloads"] = {"closed-baseline.warm": {"phase": "warm", "samples": [
            {"value": v, "outcome": "passed", "duration_seconds": v, "exit_code": 0, "repeat": i + 1,
             "latency_p99_seconds": latency + 0.01 * (i % 3)}
            for i, v in enumerate(shifted(duration, 1.0, phase=0 if label == "base" else 5))
        ]}}
    return doc


class SloEvidenceTests(unittest.TestCase):
    """PR #589 review: SLO evidence fails closed through the full comparator CLI,
    judged with the committed budgets and a matching fixed baseline."""

    def cli(self, doc):
        with tempfile.TemporaryDirectory() as tmp:
            reference = slo_workload_doc(latency=1.0)
            reference["candidate"]["provenance"]["built_by_this_run"] = True
            baseline = Path(tmp) / "baseline.json"
            recorded = run_cli("--budgets", str(BUDGETS_PATH), "--record-baseline", str(baseline),
                               input_doc=reference)
            self.assertEqual(recorded.returncode, 0, recorded.stderr)
            proc = run_cli("--budgets", str(BUDGETS_PATH), "--baseline", str(baseline), input_doc=doc)
        report = strict_json(proc.stdout)
        self.assertEqual(report["decision"]["fixed_baseline"]["verdict"], "no_significant_difference")
        self.assertTrue(report["strict_gate"]["fixed_baseline_evaluated"])
        slos = {r["id"]: r for r in report["decision"]["slos"]["results"]}
        return proc, report, slos

    def test_a_measured_workload_missing_an_slo_field_fails_closed(self):
        proc, report, slos = self.cli(slo_workload_doc(latency=1.0))
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertEqual(slos["closed-baseline-warm-latency-p99"]["verdict"], "pass")
        self.assertFalse(report["strict_gate"]["blocking"])

        # Ten warm samples over the 3.5 s ceiling breach it.
        doc = slo_workload_doc(latency=3.8)
        proc, report, slos = self.cli(doc)
        self.assertEqual(proc.returncode, 4, proc.stderr)
        self.assertEqual(report["overall"], "slo_breach")
        self.assertEqual(slos["closed-baseline-warm-latency-p99"]["verdict"], "breach")

        # One sample of the measured workload without the field is missing
        # evidence: never not_evaluated, never a pass on the other nine.
        del doc["candidate"]["workloads"]["closed-baseline.warm"]["samples"][4]["latency_p99_seconds"]
        proc, report, slos = self.cli(doc)
        self.assertEqual(proc.returncode, 2, proc.stderr)
        self.assertEqual(report["overall"], "fail")
        self.assertTrue(report["strict_gate"]["blocking"])
        latency = slos["closed-baseline-warm-latency-p99"]
        self.assertEqual(latency["verdict"], "fail")
        self.assertIn("1 of 10 samples lack latency_p99_seconds (samples [5])", latency["reasons"][0])
        self.assertNotIn("closed-baseline-warm-latency-p99", report["decision"]["slos"]["not_evaluated"])
        # The same field set to null is missing too.
        doc["candidate"]["workloads"]["closed-baseline.warm"]["samples"][4]["latency_p99_seconds"] = None
        proc, report, slos = self.cli(doc)
        self.assertEqual(proc.returncode, 2, proc.stderr)
        self.assertEqual(slos["closed-baseline-warm-latency-p99"]["verdict"], "fail")

        # A workload the run did not select is still only not_evaluated.
        for unselected in ("closed-baseline-cold-latency-p99", "open-tiny-sustained-warm-latency-p99",
                           "open-tiny-sustained-warm-sustained"):
            self.assertEqual(slos[unselected]["verdict"], "not_evaluated")
            self.assertIn(unselected, report["decision"]["slos"]["not_evaluated"])

    def test_non_finite_or_non_numeric_slo_observations_fail_closed(self):
        for bad in (float("nan"), "NaN", float("inf"), -float("inf"), "Infinity", "3.8", True):
            with self.subTest(bad=bad):
                # Every sample is otherwise well inside the ceiling, so only
                # the invalid observations can fail the run.
                doc = slo_workload_doc(latency=1.0)
                for sample in doc["candidate"]["workloads"]["closed-baseline.warm"]["samples"]:
                    sample["latency_p99_seconds"] = bad
                proc, report, slos = self.cli(doc)
                self.assertEqual(proc.returncode, 2, proc.stderr)
                self.assertEqual(report["overall"], "fail")
                self.assertTrue(report["strict_gate"]["blocking"])
                latency = slos["closed-baseline-warm-latency-p99"]
                self.assertEqual(latency["verdict"], "fail")
                self.assertNotIn("observed", latency)
                self.assertIn("10 of 10 observations are not finite JSON numbers", latency["reasons"][0])
        # A single non-finite observation fails even where p90 would drop it.
        doc = slo_workload_doc(latency=1.0)
        doc["candidate"]["workloads"]["closed-baseline.warm"]["samples"][0]["latency_p99_seconds"] = float("inf")
        proc, report, slos = self.cli(doc)
        self.assertEqual(proc.returncode, 2, proc.stderr)
        self.assertEqual(slos["closed-baseline-warm-latency-p99"]["verdict"], "fail")

    def test_non_finite_metric_samples_are_a_schema_error(self):
        for bad in (float("nan"), float("inf"), "NaN"):
            with self.subTest(bad=bad):
                with self.assertRaisesRegex(COMPARE["CompareError"], "not a finite number"):
                    COMPARE["coerce_samples"]([1.0, {"value": bad}])
        doc = slo_workload_doc(latency=1.0)
        doc["candidate"]["workloads"]["closed-baseline.warm"]["samples"][2]["value"] = float("nan")
        proc = run_cli("--budgets", str(BUDGETS_PATH), input_doc=doc)
        self.assertEqual(proc.returncode, 1, proc.stderr)
        self.assertIn("not a finite number", proc.stderr)

    def test_report_writer_never_emits_nan_or_infinity(self):
        self.assertEqual(
            strict_json(COMPARE["dump_json"]({"a": float("nan"), "b": [float("inf"), -float("inf")], "c": 1.5})),
            {"a": None, "b": [None, None], "c": 1.5},
        )
        # A zero-mean base makes delta_pct undefined (inf in memory).
        doc = document()
        doc["base"]["system"] = {"cpu_pct": [0.0] * 10}
        doc["candidate"]["system"] = {"cpu_pct": [1.0] * 10}
        with tempfile.TemporaryDirectory() as tmp:
            budgets = Path(tmp) / "budgets.json"
            budgets.write_text(json.dumps(test_budgets(slos=[])))
            proc = run_cli("--budgets", str(budgets), input_doc=doc)
        self.assertNotIn("Infinity", proc.stdout)
        self.assertNotIn("NaN", proc.stdout)
        metric = [m for m in strict_json(proc.stdout)["metrics"] if m["id"] == "system.cpu_pct"][0]
        self.assertIsNone(metric["delta_pct"])


if __name__ == "__main__":
    unittest.main()
