"""Regression checks for G2 coverage collection and the fail-closed checker.

Hermetic: synthetic coverprofiles and provenance, plus static audits of
build/Dockerfile.coverage and scripts/integration-coverage.sh. Does not start
caesium-server-test, bind host 8080, or run just integration-up / ui-e2e.
"""

from __future__ import annotations

import json
import os
from pathlib import Path
import runpy
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
CHECKER = ROOT / "scripts/check-coverage.py"
COLLECTOR = ROOT / "scripts/integration-coverage.sh"
BROWSER_JOURNEY = ROOT / "scripts/coverage-browser-journey.sh"
BROWSER_CHECKER = ROOT / "scripts/check-browser-journey.py"
DOCKERFILE = ROOT / "build/Dockerfile.coverage"
COV = runpy.run_path(str(CHECKER))

SHA = "a" * 40
MODULE = "github.com/caesium-cloud/caesium"
APPLY_CLI = f"{MODULE}/cmd/job/apply.go"
APPLY_HTTP = f"{MODULE}/api/rest/controller/jobdef/apply.go"
APPLY_WRITE = f"{MODULE}/internal/jobdef/importer.go"
EXPORT_CLI = f"{MODULE}/cmd/job/export.go"
EXPORT_HTTP = f"{MODULE}/api/rest/controller/job/manifest.go"
EXPORT_READ = f"{MODULE}/internal/jobdef/exporter.go"
OTHER = f"{MODULE}/pkg/log/log.go"


def block(file, stmts=4, count=1, sl=1):
    return f"{file}:{sl}.1,{sl + 3}.2 {stmts} {count}"


def profile(mode="set", *lines):
    return "mode: " + mode + "\n" + "\n".join(lines) + "\n"


def funcs_in(relpath):
    return COV["parse_go_funcs"]((ROOT / relpath).read_text())


def func_start(relpath, name):
    for fn in funcs_in(relpath):
        if fn["name"] == name:
            return fn["start"]
    raise AssertionError(f"no func {name} in {relpath}")


def func_block(relpath, name, stmts=4, count=1):
    sl = func_start(relpath, name) + 1
    return block(f"{MODULE}/{relpath}", stmts, count, sl=sl)


def init_blocks(*relpaths):
    lines = []
    for relpath in relpaths:
        hits = [fn for fn in funcs_in(relpath) if fn["name"] == "init"]
        if not hits:
            raise AssertionError(f"no init() in {relpath}")
        for fn in hits:
            lines.append(block(f"{MODULE}/{relpath}", 3, 1, sl=fn["start"]))
    return lines


def write_to_read_cli():
    return profile(
        "set",
        func_block("cmd/job/apply.go", "sendApplyRequest", 6, 1),
        func_block("cmd/job/export.go", "exportGet", 5, 1),
        block(OTHER, 2, 0),
    )


def write_to_read_server():
    return profile(
        "set",
        func_block("api/rest/controller/jobdef/apply.go", "Apply", 8, 1),
        func_block("internal/jobdef/importer.go", "ApplyWithOptions", 10, 1),
        func_block("api/rest/controller/job/manifest.go", "Manifest", 7, 1),
        func_block("internal/jobdef/exporter.go", "Export", 9, 1),
        block(OTHER, 2, 0),
    )


def unit_covering_path():
    return profile(
        "atomic",
        func_block("cmd/job/apply.go", "sendApplyRequest", 6, 3),
        func_block("api/rest/controller/jobdef/apply.go", "Apply", 8, 2),
        func_block("internal/jobdef/importer.go", "ApplyWithOptions", 10, 4),
        func_block("cmd/job/export.go", "exportGet", 5, 1),
        func_block("api/rest/controller/job/manifest.go", "Manifest", 7, 1),
        func_block("internal/jobdef/exporter.go", "Export", 9, 1),
    )


def provenance(source, **overrides):
    doc = {
        "schema_version": 1,
        "source": source,
        "kind": "coverprofile",
        "module": MODULE if source != "reagents" else COV["REAGENTS_MODULE"],
        "candidate_sha": SHA,
        "complete": True,
        "missing": False,
        "killed": False,
    }
    doc.update(overrides)
    return doc


def write_source(dirpath, source, text=None, prov=None):
    dirpath = Path(dirpath)
    dirpath.mkdir(parents=True, exist_ok=True)
    if text is not None:
        (dirpath / f"{source}.out").write_text(text)
    if prov is not None:
        (dirpath / f"{source}.provenance.json").write_text(json.dumps(prov))


def run_checker(profiles_dir, extra=(), env=None):
    cmd = [
        sys.executable,
        str(CHECKER),
        "--profiles-dir", str(profiles_dir),
        "--candidate-sha", SHA,
        "--repo-root", str(ROOT),
        "--report", str(Path(profiles_dir) / "report.json"),
        *extra,
    ]
    return subprocess.run(cmd, capture_output=True, text=True, env=env or os.environ.copy())


def output(result):
    return result.stdout + result.stderr


def collector_test_env(art):
    """Synthetic checker input with explicit ratchet and diff provenance."""
    changed = art / "changed-paths.txt"
    changed.write_text("")
    ratchet = art / "test-ratchet.json"
    ratchet.write_text(json.dumps({
        "schema_version": 1,
        "kind": "package-diff-ratchet",
        "source": "integration",
        "packages": {f"{MODULE}/cmd/job": {"min_percent": 0}},
        "diff": {"uncovered_changed_paths_max": 0},
    }))
    env = os.environ.copy()
    env["CAESIUM_COVERAGE_ARTIFACTS"] = str(art)
    env["CAESIUM_COVERAGE_CHANGED_PATHS"] = str(changed)
    env["CAESIUM_COVERAGE_RATCHET"] = str(ratchet)
    env["CANDIDATE_SHA"] = SHA
    return env


class CoverprofileParseTests(unittest.TestCase):
    def test_parse_go_funcs_finds_apply_export_and_init(self):
        apply_funcs = {fn["name"]: fn for fn in funcs_in("cmd/job/apply.go")}
        self.assertIn("init", apply_funcs)
        self.assertIn("sendApplyRequest", apply_funcs)
        self.assertIn("RunE", apply_funcs)
        self.assertLess(apply_funcs["RunE"]["start"], apply_funcs["init"]["start"])
        self.assertGreater(apply_funcs["sendApplyRequest"]["start"], apply_funcs["init"]["end"])
        export_funcs = {fn["name"]: fn for fn in funcs_in("cmd/job/export.go")}
        self.assertIn("init", export_funcs)
        self.assertIn("exportGet", export_funcs)
        self.assertIn("RunE", export_funcs)

    def test_parse_and_package_percent(self):
        text = profile("set", block(APPLY_CLI, 4, 1), block(EXPORT_CLI, 6, 0))
        mode, blocks = COV["parse_profile_text"](text, origin="t")
        self.assertEqual(mode, "set")
        summary = COV["summarize_blocks"](blocks)
        self.assertEqual(summary["covered"], 4)
        self.assertEqual(summary["total"], 10)
        self.assertEqual(summary["percent"], 40.0)
        pkg = summary["packages"][f"{MODULE}/cmd/job"]
        self.assertEqual(pkg["covered"], 4)

    def test_empty_file_is_schema_failure_not_zero(self):
        with self.assertRaises(ValueError) as ctx:
            COV["parse_profile_text"]("", origin="empty")
        self.assertIn("empty", str(ctx.exception))

    def test_missing_mode_rejected(self):
        with self.assertRaises(ValueError):
            COV["parse_profile_text"](block(APPLY_CLI), origin="n")

    def test_merge_set_takes_max(self):
        _, a = COV["parse_profile_text"](profile("set", block(APPLY_CLI, 4, 1)))
        _, b = COV["parse_profile_text"](profile("set", block(APPLY_CLI, 4, 0), block(EXPORT_CLI, 2, 1)))
        merged = COV["merge_blocks"]("set", a, b)
        summary = COV["summarize_blocks"](merged)
        self.assertEqual(summary["covered"], 6)


class MissingAndKilledTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)

    def test_missing_server_profile_is_incomplete_not_zero(self):
        write_source(self.dir, "cli", write_to_read_cli(), provenance("cli"))
        write_source(self.dir, "server", None, provenance("server", complete=False, missing=True))
        result = run_checker(self.dir)
        self.assertEqual(result.returncode, 2, output(result))
        self.assertIn("incomplete", output(result))
        self.assertIn("not 0", result.stdout)
        report = json.loads((self.dir / "report.json").read_text())
        self.assertEqual(report["verdict"], "incomplete")
        self.assertIsNone(report["contributions"]["server"]["percent"])
        self.assertIsNone(report["contributions"]["integration"]["percent"])
        self.assertNotEqual(report["contributions"]["server"]["percent"], 0)
        self.assertEqual(report["write_to_read"]["status"], "incomplete")
        self.assertFalse(report["write_to_read"]["covered"])

    def test_killed_server_is_incomplete_even_if_a_file_exists(self):
        write_source(self.dir, "cli", write_to_read_cli(), provenance("cli"))
        write_source(
            self.dir,
            "server",
            write_to_read_server(),
            provenance("server", complete=False, killed=True, signal="SIGKILL"),
        )
        result = run_checker(self.dir)
        self.assertEqual(result.returncode, 2, output(result))
        self.assertIn("killed", output(result))
        report = json.loads((self.dir / "report.json").read_text())
        self.assertEqual(report["contributions"]["server"]["status"], "incomplete")
        self.assertIsNone(report["contributions"]["server"]["percent"])
        self.assertEqual(report["write_to_read"]["status"], "incomplete")

    def test_honest_zero_complete_profile_fails_write_to_read_not_incomplete(self):
        zero = profile("set", block(APPLY_CLI, 6, 0), block(EXPORT_CLI, 5, 0), block(OTHER, 2, 0))
        write_source(self.dir, "cli", zero, provenance("cli"))
        write_source(self.dir, "server", zero, provenance("server"))
        result = run_checker(self.dir)
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("write-to-read", output(result))
        report = json.loads((self.dir / "report.json").read_text())
        self.assertEqual(report["verdict"], "fail")
        self.assertEqual(report["contributions"]["integration"]["percent"], 0.0)
        self.assertEqual(report["write_to_read"]["status"], "fail")

    def test_strict_promotes_incomplete_to_fail(self):
        write_source(self.dir, "cli", write_to_read_cli(), provenance("cli"))
        write_source(self.dir, "server", None, provenance("server", complete=False, missing=True))
        result = run_checker(self.dir, extra=("--strict",))
        self.assertEqual(result.returncode, 1, output(result))


class WriteToReadTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)

    def test_merged_cli_and_server_covers_apply_export_path(self):
        write_source(self.dir, "cli", write_to_read_cli(), provenance("cli"))
        write_source(self.dir, "server", write_to_read_server(), provenance("server"))
        result = run_checker(self.dir)
        self.assertEqual(result.returncode, 0, output(result))
        report = json.loads((self.dir / "report.json").read_text())
        self.assertEqual(report["verdict"], "pass")
        self.assertTrue(report["write_to_read"]["covered"])
        self.assertEqual(report["write_to_read"]["id"], "jobdef-apply-export")
        self.assertEqual(report["contributions"]["cli"]["status"], "complete")
        self.assertEqual(report["contributions"]["server"]["status"], "complete")
        self.assertEqual(report["contributions"]["integration"]["status"], "complete")
        self.assertIsNotNone(report["contributions"]["integration"]["percent"])
        self.assertEqual(report["contributions"]["browser"]["status"], "incomplete")
        self.assertIsNone(report["contributions"]["browser"]["percent"])
        self.assertEqual(report["contributions"]["unit"]["status"], "incomplete")
        self.assertIsNone(report["contributions"]["unit"]["percent"])
        self.assertTrue(report["reagents_audit"]["audited"])
        self.assertFalse(report["reagents_audit"]["contaminated_root"])
        self.assertFalse(report["performance_instrumentation"])

    def test_unit_coverage_of_the_path_is_not_integration_evidence(self):
        write_source(self.dir, "unit", unit_covering_path(), provenance("unit"))
        write_source(self.dir, "cli", None, provenance("cli", complete=False, missing=True))
        write_source(self.dir, "server", None, provenance("server", complete=False, missing=True))
        result = run_checker(self.dir)
        self.assertEqual(result.returncode, 2, output(result))
        report = json.loads((self.dir / "report.json").read_text())
        self.assertEqual(report["contributions"]["unit"]["status"], "complete")
        self.assertGreater(report["contributions"]["unit"]["percent"], 0)
        self.assertFalse(report["write_to_read"]["covered"])
        self.assertEqual(report["write_to_read"]["status"], "incomplete")

    def test_cli_without_server_cannot_demonstrate_the_path(self):
        write_source(self.dir, "cli", write_to_read_cli(), provenance("cli"))
        result = run_checker(self.dir)
        self.assertEqual(result.returncode, 2, output(result))
        report = json.loads((self.dir / "report.json").read_text())
        self.assertEqual(report["write_to_read"]["status"], "incomplete")

    def test_init_only_coverage_is_not_write_to_read(self):
        init_prof = profile("set", *init_blocks("cmd/job/apply.go", "cmd/job/export.go"))
        write_source(self.dir, "cli", init_prof, provenance("cli"))
        write_source(self.dir, "server", init_prof, provenance("server"))
        result = run_checker(self.dir)
        self.assertEqual(result.returncode, 1, output(result))
        report = json.loads((self.dir / "report.json").read_text())
        self.assertEqual(report["write_to_read"]["status"], "fail")
        self.assertFalse(report["write_to_read"]["covered"])
        for item in report["write_to_read"]["evidence"]:
            self.assertFalse(item["covered"], item)

    def test_server_init_of_cli_files_does_not_satisfy_server_write_read(self):
        write_source(self.dir, "cli", write_to_read_cli(), provenance("cli"))
        write_source(
            self.dir,
            "server",
            profile("set", *init_blocks("cmd/job/apply.go", "cmd/job/export.go")),
            provenance("server"),
        )
        result = run_checker(self.dir)
        self.assertEqual(result.returncode, 1, output(result))
        report = json.loads((self.dir / "report.json").read_text())
        self.assertFalse(report["write_to_read"]["covered"])
        by_id = {item["id"]: item for item in report["write_to_read"]["evidence"]}
        self.assertFalse(by_id["server_write_http"]["covered"])
        self.assertFalse(by_id["server_read_http"]["covered"])

    def test_init_only_contract_files_are_gaps(self):
        extra = profile("set", *init_blocks("cmd/run/retry.go", "cmd/event/event.go"))
        server = write_to_read_server().rstrip() + "\n" + "\n".join(extra.splitlines()[1:]) + "\n"
        write_source(self.dir, "cli", write_to_read_cli(), provenance("cli"))
        write_source(self.dir, "server", server, provenance("server"))
        result = run_checker(self.dir)
        self.assertEqual(result.returncode, 0, output(result))
        report = json.loads((self.dir / "report.json").read_text())
        by_id = {item["id"]: item for item in report["contract_gaps"]}
        self.assertEqual(by_id["DT-RETRY-01"]["status"], "gap")
        self.assertEqual(by_id["DT-EVENT-01"]["status"], "gap")
        self.assertEqual(by_id["DT-DAG-01"]["status"], "covered")


class BrowserMergeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)
        write_source(self.dir, "cli", write_to_read_cli(), provenance("cli"))
        write_source(self.dir, "server", write_to_read_server(), provenance("server"))

    def test_missing_browser_is_incomplete_contribution_not_zero_and_does_not_fail_cli_server(self):
        result = run_checker(self.dir)
        self.assertEqual(result.returncode, 0, output(result))
        report = json.loads((self.dir / "report.json").read_text())
        self.assertEqual(report["contributions"]["browser"]["status"], "incomplete")
        self.assertIsNone(report["contributions"]["browser"]["percent"])

    def test_labelled_browser_profile_is_merged_when_present(self):
        browser = profile("set", block(f"{MODULE}/api/ui.go", 3, 1), block(APPLY_HTTP, 8, 1))
        write_source(self.dir, "browser", browser, provenance("browser", kind="gocoverdir"))
        result = run_checker(self.dir)
        self.assertEqual(result.returncode, 0, output(result))
        report = json.loads((self.dir / "report.json").read_text())
        self.assertEqual(report["contributions"]["browser"]["status"], "complete")
        self.assertIsNotNone(report["contributions"]["browser"]["percent"])
        self.assertGreater(report["contributions"]["browser"]["percent"], 0)
        self.assertEqual(report["all_surfaces"]["status"], "complete")
        self.assertGreater(report["all_surfaces"]["covered"], report["contributions"]["integration"]["covered"])

    def test_failed_chromium_journey_fails_even_with_a_complete_profile(self):
        browser = profile("set", block(f"{MODULE}/api/ui.go", 3, 1))
        write_source(
            self.dir, "browser", browser,
            provenance("browser", kind="gocoverdir", test_exit_code=1),
        )
        result = run_checker(self.dir, extra=("--require-browser",))
        self.assertEqual(result.returncode, 1, output(result))
        report = json.loads((self.dir / "report.json").read_text())
        self.assertEqual(report["verdict"], "fail")
        self.assertIn("browser-journey", output(result))

    def test_require_browser_without_profile_is_incomplete(self):
        result = run_checker(self.dir, extra=("--require-browser",))
        self.assertEqual(result.returncode, 2, output(result))
        self.assertIn("browser", output(result))

    def test_foreign_browser_module_fails_closed(self):
        write_source(
            self.dir,
            "browser",
            profile("set", block(APPLY_HTTP, 8, 1)),
            provenance("browser", module="github.com/example/other"),
        )
        result = run_checker(self.dir)
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("foreign", output(result))


class BrowserJourneyResultTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = Path(self.tmp.name) / "playwright.json"

    def check(self, first="passed", second="passed", *, retry=False, missing=False):
        specs = [{
            "title": "sidebar navigates between every primary control-plane page",
            "tests": [{"results": [{"status": first}]}],
        }]
        if not missing:
            specs.append({
                "title": "operator can pause and unpause a job from the detail page",
                "tests": [{"results": [{"status": "failed"}, {"status": second}]}]
                if retry else [{"results": [{"status": second}]}],
            })
        self.path.write_text(json.dumps({"suites": [{"specs": specs}]}))
        return subprocess.run(
            [sys.executable, str(BROWSER_CHECKER), str(self.path)],
            capture_output=True, text=True,
        )

    def test_two_first_attempt_passes(self):
        self.assertEqual(self.check().returncode, 0)

    def test_skip_retry_or_missing_journey_does_not_pass(self):
        for kwargs in ({"first": "skipped"}, {"retry": True}, {"missing": True}):
            with self.subTest(kwargs=kwargs):
                result = self.check(**kwargs)
                self.assertNotEqual(result.returncode, 0, output(result))


class ForeignAndPerformanceTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)
        write_source(self.dir, "cli", write_to_read_cli(), provenance("cli"))

    def test_candidate_sha_mismatch_is_foreign(self):
        write_source(self.dir, "server", write_to_read_server(), provenance("server", candidate_sha="b" * 40))
        result = run_checker(self.dir)
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("foreign", output(result))

    def test_performance_label_is_rejected(self):
        write_source(
            self.dir,
            "server",
            write_to_read_server(),
            provenance("server", kind="performance", label="load-test"),
        )
        result = run_checker(self.dir)
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("performance", output(result))
        report = json.loads((self.dir / "report.json").read_text())
        self.assertTrue(report["performance_instrumentation"])

    def test_performance_package_path_is_rejected(self):
        write_source(
            self.dir,
            "server",
            profile("set", block(f"{MODULE}/test/performance/load_test.go", 4, 1)),
            provenance("server"),
        )
        result = run_checker(self.dir)
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("performance", output(result))

    def test_incompatible_modes_fail_closed(self):
        write_source(self.dir, "cli", write_to_read_cli(), provenance("cli"))
        write_source(self.dir, "server", profile("atomic", block(APPLY_HTTP, 8, 1)), provenance("server"))
        result = run_checker(self.dir)
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("incompatible", output(result))


class RatchetTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)
        write_source(self.dir, "cli", write_to_read_cli(), provenance("cli"))
        write_source(self.dir, "server", write_to_read_server(), provenance("server"))

    def test_no_global_percentage_flag_and_vanity_global_floor_rejected(self):
        help_text = subprocess.run(
            [sys.executable, str(CHECKER), "--help"], capture_output=True, text=True
        ).stdout
        self.assertNotIn("min-global", help_text)
        self.assertNotIn("80", help_text)
        ratchet = self.dir / "ratchet.json"
        ratchet.write_text(json.dumps({"schema_version": 1, "global_percent": 80, "packages": {}}))
        result = run_checker(self.dir, extra=("--ratchet", str(ratchet)))
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("global percentage", output(result))

    def test_write_baseline_then_drop_fails(self):
        baseline = self.dir / "baseline.json"
        result = run_checker(self.dir, extra=("--write-baseline", str(baseline)))
        self.assertEqual(result.returncode, 0, output(result))
        doc = json.loads(baseline.read_text())
        self.assertNotIn("global_percent", doc)
        self.assertNotIn("min_global_percent", doc)
        self.assertEqual(doc["kind"], "package-diff-ratchet")
        self.assertEqual(doc["measured_candidate_sha"], SHA)
        self.assertIn(f"{MODULE}/cmd/job", doc["packages"])
        floor = doc["packages"][f"{MODULE}/cmd/job"]["min_statements_covered"]
        self.assertGreater(floor, 0)
        dropped = profile(
            "set",
            func_block("cmd/job/apply.go", "sendApplyRequest", 6, 0),
            func_block("cmd/job/export.go", "exportGet", 5, 0),
        )
        write_source(self.dir, "cli", dropped, provenance("cli"))
        result = run_checker(self.dir, extra=("--ratchet", str(baseline)))
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("ratchet", output(result))

    def test_baseline_round_trip_on_unchanged_profiles_exits_0(self):
        baseline = self.dir / "baseline.json"
        first = run_checker(self.dir, extra=("--write-baseline", str(baseline)))
        self.assertEqual(first.returncode, 0, output(first))
        self.assertTrue(baseline.is_file())
        self.assertNotIn("global_percent", json.loads(baseline.read_text()))
        second = run_checker(self.dir, extra=("--ratchet", str(baseline)))
        self.assertEqual(second.returncode, 0, output(second))

    def test_uncovered_changed_paths_are_reported_and_ratcheted(self):
        changed = self.dir / "changed.txt"
        changed.write_text("cmd/job/apply.go\nREADME.md\ncmd/job/apply_test.go\n")
        baseline = self.dir / "baseline.json"
        result = run_checker(
            self.dir,
            extra=("--changed-paths", str(changed), "--write-baseline", str(baseline)),
        )
        self.assertEqual(result.returncode, 0, output(result))
        report = json.loads((self.dir / "report.json").read_text())
        self.assertEqual(report["uncovered_changed_paths"], [])
        doc = json.loads(baseline.read_text())
        self.assertEqual(doc["diff"]["uncovered_changed_paths_max"], 0)
        write_source(
            self.dir,
            "cli",
            profile(
                "set",
                func_block("cmd/job/export.go", "exportGet", 5, 1),
                func_block("cmd/job/apply.go", "sendApplyRequest", 6, 0),
            ),
            provenance("cli"),
        )
        result = run_checker(
            self.dir,
            extra=("--changed-paths", str(changed), "--ratchet", str(baseline)),
        )
        self.assertEqual(result.returncode, 1, output(result))
        report = json.loads((self.dir / "report.json").read_text())
        self.assertIn(APPLY_CLI, report["uncovered_changed_paths"])

    def test_diff_ratchet_without_changed_paths_is_incomplete(self):
        baseline = self.dir / "ratchet.json"
        baseline.write_text(json.dumps({
            "schema_version": 1,
            "kind": "package-diff-ratchet",
            "source": "integration",
            "packages": {f"{MODULE}/cmd/job": {"min_percent": 0}},
            "diff": {"uncovered_changed_paths_max": 0},
        }))
        result = run_checker(self.dir, extra=("--ratchet", str(baseline)))
        self.assertEqual(result.returncode, 2, output(result))
        self.assertIn("no changed-paths input", output(result))


class ReagentsAuditTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)
        write_source(self.dir, "cli", write_to_read_cli(), provenance("cli"))
        write_source(self.dir, "server", write_to_read_server(), provenance("server"))

    def test_committed_reagents_module_is_audited_and_not_in_root_go_mod(self):
        issues = []
        audit = COV["audit_reagents"](str(ROOT), issues)
        self.assertTrue(audit["audited"])
        self.assertFalse(audit["contaminated_root"])
        self.assertEqual(audit["module"], COV["REAGENTS_MODULE"])
        self.assertEqual([item.message for item in issues], [])
        root_mod = (ROOT / "go.mod").read_text()
        self.assertNotIn(COV["REAGENTS_MODULE"], root_mod)

    def test_reagents_paths_in_cli_profile_are_contamination(self):
        mixed = profile("set", block(APPLY_CLI, 6, 1), block(f"{COV['REAGENTS_MODULE']}/cmd/git-source/main.go", 3, 1))
        write_source(self.dir, "cli", mixed, provenance("cli"))
        result = run_checker(self.dir)
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("contamination", output(result))

    def test_changed_reagents_without_profile_is_incomplete_not_skipped(self):
        changed = self.dir / "changed.txt"
        changed.write_text("reagents/cmd/git-source/main.go\n")
        result = run_checker(self.dir, extra=("--changed-paths", str(changed)))
        self.assertEqual(result.returncode, 2, output(result))
        self.assertIn("reagents", output(result))
        report = json.loads((self.dir / "report.json").read_text())
        self.assertEqual(report["contributions"]["reagents"]["status"], "incomplete")
        self.assertIsNone(report["contributions"]["reagents"]["percent"])

    def test_missing_repo_root_does_not_silently_skip_reagents(self):
        result = subprocess.run(
            [
                sys.executable, str(CHECKER),
                "--profiles-dir", str(self.dir),
                "--candidate-sha", SHA,
                "--report", str(self.dir / "report.json"),
            ],
            capture_output=True, text=True,
        )
        self.assertEqual(result.returncode, 2, output(result))
        self.assertIn("reagents", output(result))


class ContractGapTests(unittest.TestCase):
    def test_apply_export_path_covers_dag_contract_and_reports_admit_gap(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        d = Path(tmp.name)
        write_source(d, "cli", write_to_read_cli(), provenance("cli"))
        write_source(d, "server", write_to_read_server(), provenance("server"))
        result = run_checker(d)
        self.assertEqual(result.returncode, 0, output(result))
        report = json.loads((d / "report.json").read_text())
        by_id = {item["id"]: item for item in report["contract_gaps"]}
        self.assertEqual(by_id["DT-DAG-01"]["status"], "covered")
        self.assertEqual(by_id["DT-ADMIT-01"]["status"], "gap")
        self.assertEqual(by_id["DT-QUORUM-01"]["status"], "gap")

    def test_incomplete_integration_does_not_invent_contract_zeros(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        d = Path(tmp.name)
        write_source(d, "cli", write_to_read_cli(), provenance("cli"))
        write_source(d, "server", None, provenance("server", complete=False, missing=True))
        result = run_checker(d)
        self.assertEqual(result.returncode, 2, output(result))
        report = json.loads((d / "report.json").read_text())
        statuses = {item["status"] for item in report["contract_gaps"]}
        self.assertEqual(statuses, {"incomplete"})


class DockerfileAndCollectorTests(unittest.TestCase):
    def test_committed_ratchet_has_measured_source_and_package_diff_floors(self):
        ratchet = json.loads((ROOT / "scripts/coverage-ratchet.json").read_text())
        self.assertTrue(COV["_is_sha"](ratchet["measured_candidate_sha"]))
        self.assertGreater(len(ratchet["packages"]), 0)
        self.assertIn("uncovered_changed_paths_max", ratchet["diff"])
        self.assertNotIn("global_percent", ratchet)

    def test_dockerfile_is_a_separate_coverage_target(self):
        text = DOCKERFILE.read_text()
        self.assertIn("go build -cover", text)
        self.assertIn("coverpkg", text)
        self.assertIn("GOCOVERDIR", text)
        self.assertIn("SIGUSR2", text)
        self.assertIn("runtime/coverage", text)
        self.assertIn("reagents/go.mod", text)
        self.assertIn("reagents packages leaked", text)
        self.assertIn("caesium-testfault-control", text)
        self.assertNotIn("-tags testfault", text)
        self.assertNotIn("-tags=testfault", text)
        self.assertNotIn("just integration-up", text)
        self.assertIn("test/performance", text)
        self.assertIn("AS coverage", text)
        self.assertIn("AS reagents-coverage", text)
        self.assertIn("caesiumcloud/caesium-coverage", text)
        self.assertNotIn("FROM performance", text.lower())
        self.assertIn("covcounters", text)
        self.assertIn("org.opencontainers.image.revision", text)
        self.assertIn("CAESIUM_REVISION", text)
        self.assertIn("must never be tagged as caesium-server-test", text)
        code = "\n".join(
            ln for ln in text.splitlines() if ln.strip() and not ln.lstrip().startswith("#")
        )
        self.assertNotIn("caesium-server-test", code)
        compile_at = text.find("AS coverage-compile")
        reagents_at = text.find("AS reagents-coverage")
        runtime_at = text.rfind("AS coverage")
        self.assertLess(compile_at, reagents_at)
        self.assertLess(reagents_at, runtime_at)

    def test_collector_script_invariants(self):
        text = COLLECTOR.read_text()
        self.assertIn("GOCOVERDIR", text)
        self.assertIn("SIGUSR2", text)
        self.assertIn("docker stop", text)
        self.assertIn("job apply", text)
        self.assertIn("job export", text)
        self.assertIn("coverage-write-read", text)
        self.assertIn("check-coverage.py", text)
        self.assertIn("build/Dockerfile.coverage", text)
        self.assertIn("reagents", text)
        self.assertIn("incomplete", text)
        self.assertIn("refusing to use caesium-server-test", text)
        self.assertNotIn("--name caesium-server-test", text)
        self.assertNotIn("--publish", text)
        self.assertNotIn(" --publish-all", text)
        self.assertIn("-p 127.0.0.1::8080", text)
        self.assertNotIn("-p 8080:8080", text)
        self.assertIn("kill --signal=SIGUSR2", text)
        self.assertIn("CAESIUM_COVERAGE_BROWSER_DIR", text)
        self.assertIn("no host port", text)
        code = "\n".join(
            ln for ln in text.splitlines() if ln.strip() and not ln.lstrip().startswith("#")
        )
        self.assertNotIn("just integration-up", code)
        self.assertNotIn("just ui-e2e", code)
        self.assertNotIn("just robustness-test", code)
        self.assertNotIn("just integration-test", code)
        self.assertNotIn("go test ./test/performance", code)
        self.assertNotIn("just performance", code)
        self.assertIn("must not be a test/performance", code)
        self.assertIn("dirty working tree", text)
        self.assertIn("org.opencontainers.image.revision", text)
        self.assertIn("CAESIUM_REVISION", text)
        self.assertIn("supplied/unverified", text)
        self.assertIn('rm -rf "$RAW/cli"', text)
        self.assertIn("*.provenance.json", text)
        self.assertIn("stop_rc", text)
        self.assertIn('exit_code" != "0" && "$exit_code" != "143"', text)

    def test_collector_bash_syntax(self):
        result = subprocess.run(["bash", "-n", str(COLLECTOR), str(BROWSER_JOURNEY)], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_checker_is_python_stdlib(self):
        first = CHECKER.read_text().splitlines()[0]
        self.assertTrue(first.startswith("#!/usr/bin/env python3"))
        self.assertNotIn("import requests", CHECKER.read_text())
        self.assertNotIn("import yaml", CHECKER.read_text())

    def test_collector_check_mode_drives_the_checker(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        art = Path(tmp.name)
        profiles = art / "profiles"
        write_source(profiles, "cli", write_to_read_cli(), provenance("cli"))
        write_source(profiles, "server", write_to_read_server(), provenance("server"))
        env = collector_test_env(art)
        result = subprocess.run(
            ["bash", str(COLLECTOR), "check"],
            capture_output=True, text=True, env=env, cwd=str(ROOT),
        )
        self.assertEqual(result.returncode, 0, output(result))
        report = json.loads((art / "report.json").read_text())
        self.assertEqual(report["verdict"], "pass")
        self.assertTrue(report["write_to_read"]["covered"])

    def test_collector_check_mode_missing_profiles_leave_incomplete_placeholder_replaced(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        art = Path(tmp.name)
        env = collector_test_env(art)
        result = subprocess.run(
            ["bash", str(COLLECTOR), "check"],
            capture_output=True, text=True, env=env, cwd=str(ROOT),
        )
        self.assertEqual(result.returncode, 2, output(result))
        report = json.loads((art / "report.json").read_text())
        self.assertEqual(report["verdict"], "incomplete")
        self.assertIsNone(report["contributions"]["cli"]["percent"])
        self.assertIsNone(report["contributions"]["server"]["percent"])
        self.assertIsNone(report["contributions"]["browser"]["percent"])

    def test_collector_check_mode_accepts_labelled_browser_profile(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        art = Path(tmp.name)
        profiles = art / "profiles"
        write_source(profiles, "cli", write_to_read_cli(), provenance("cli"))
        write_source(profiles, "server", write_to_read_server(), provenance("server"))
        browser = art / "browser.out"
        browser.write_text(profile("set", block(f"{MODULE}/api/ui.go", 3, 1)))
        browser_provenance = art / "browser.provenance.json"
        browser_provenance.write_text(json.dumps(provenance("browser", kind="gocoverdir")))
        env = collector_test_env(art)
        env["CAESIUM_COVERAGE_BROWSER_PROFILE"] = str(browser)
        env["CAESIUM_COVERAGE_BROWSER_PROVENANCE"] = str(browser_provenance)
        result = subprocess.run(
            ["bash", str(COLLECTOR), "check"],
            capture_output=True, text=True, env=env, cwd=str(ROOT),
        )
        self.assertEqual(result.returncode, 0, output(result))
        report = json.loads((art / "report.json").read_text())
        self.assertEqual(report["contributions"]["browser"]["status"], "complete")

    def test_collector_refuses_to_relabel_an_external_browser_profile(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        art = Path(tmp.name)
        browser = art / "browser.out"
        browser.write_text(profile("set", block(f"{MODULE}/api/ui.go", 3, 1)))
        env = collector_test_env(art)
        env["CAESIUM_COVERAGE_BROWSER_PROFILE"] = str(browser)
        result = subprocess.run(
            ["bash", str(COLLECTOR), "check"],
            capture_output=True, text=True, env=env, cwd=str(ROOT),
        )
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("requires CAESIUM_COVERAGE_BROWSER_PROVENANCE", output(result))

    def test_g1_wildcard_will_discover_this_module(self):
        self.assertEqual(Path(__file__).name, "test_coverage.py")


class ProvenanceBindingTests(unittest.TestCase):
    def test_missing_provenance_file_is_incomplete_not_default_complete(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        d = Path(tmp.name)
        (d / "cli.out").write_text(write_to_read_cli())
        write_source(d, "server", write_to_read_server(), provenance("server"))
        result = run_checker(d)
        self.assertEqual(result.returncode, 2, output(result))
        self.assertIn("provenance is missing", output(result))
        report = json.loads((d / "report.json").read_text())
        self.assertEqual(report["contributions"]["cli"]["status"], "incomplete")
        self.assertIsNone(report["contributions"]["cli"]["percent"])

    def test_premerged_profile_without_candidate_sha_fails_strict(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        d = Path(tmp.name)
        _, cli_blocks = COV["parse_profile_text"](write_to_read_cli())
        _, server_blocks = COV["parse_profile_text"](write_to_read_server())
        merged = COV["merge_blocks"]("set", cli_blocks, server_blocks)
        mode = "set"
        lines = ["mode: set"]
        for (file_path, sl, sc, el, ec), (stmts, count) in merged.items():
            lines.append(f"{file_path}:{sl}.{sc},{el}.{ec} {stmts} {count}")
        write_source(d, "integration", "\n".join(lines) + "\n", {"sources": ["cli", "server"]})
        result = run_checker(d, extra=("--strict",))
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("candidate_sha", output(result))
        report = json.loads((d / "report.json").read_text())
        self.assertNotEqual(report["verdict"], "pass")

    def test_bad_candidate_sha_does_not_write_baseline(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        d = Path(tmp.name)
        write_source(d, "cli", write_to_read_cli(), provenance("cli"))
        write_source(d, "server", write_to_read_server(), provenance("server"))
        baseline = d / "baseline.json"
        result = subprocess.run(
            [
                sys.executable, str(CHECKER),
                "--profiles-dir", str(d),
                "--candidate-sha", "nothex",
                "--repo-root", str(ROOT),
                "--write-baseline", str(baseline),
                "--report", str(d / "report.json"),
            ],
            capture_output=True, text=True,
        )
        self.assertEqual(result.returncode, 1, output(result))
        self.assertFalse(baseline.exists(), "failing run must not write a ratchet floor")


class EvaluateDirectTests(unittest.TestCase):
    def test_premerged_integration_requires_named_cli_and_server_sources(self):
        _, cli_blocks = COV["parse_profile_text"](write_to_read_cli())
        _, server_blocks = COV["parse_profile_text"](write_to_read_server())
        merged = COV["merge_blocks"]("set", cli_blocks, server_blocks)
        summary = COV["summarize_blocks"](merged)
        contrib = {
            "source": "integration",
            "status": "complete",
            "percent": summary["percent"],
            "covered": summary["covered"],
            "total": summary["total"],
            "mode": "set",
            "blocks": merged,
            "summary": summary,
            "provenance": {"sources": ["cli"]},
        }
        report, issues = COV["evaluate"](
            {"integration": contrib},
            candidate_sha=SHA,
            repo_root=str(ROOT),
        )
        self.assertTrue(any(item.code == "schema" for item in issues), [i.message for i in issues])

    def test_premerged_integration_without_cli_server_cannot_prove_write_to_read(self):
        _, cli_blocks = COV["parse_profile_text"](write_to_read_cli())
        _, server_blocks = COV["parse_profile_text"](write_to_read_server())
        merged = COV["merge_blocks"]("set", cli_blocks, server_blocks)
        summary = COV["summarize_blocks"](merged)
        contrib = {
            "source": "integration",
            "status": "complete",
            "percent": summary["percent"],
            "covered": summary["covered"],
            "total": summary["total"],
            "mode": "set",
            "blocks": merged,
            "summary": summary,
            "provenance": {"sources": ["cli", "server"], "candidate_sha": SHA},
        }
        report, issues = COV["evaluate"](
            {"integration": contrib},
            candidate_sha=SHA,
            repo_root=str(ROOT),
        )
        self.assertEqual(report["write_to_read"]["status"], "incomplete")
        self.assertFalse(report["write_to_read"]["covered"])


if __name__ == "__main__":
    unittest.main()
