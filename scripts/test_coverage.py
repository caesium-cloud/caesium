"""Regression checks for G2 coverage collection and the fail-closed checker.

Hermetic: synthetic coverprofiles and provenance, plus static audits of
build/Dockerfile.coverage and scripts/integration-coverage.sh. Does not start
caesium-server-test, bind host 8080, or run just integration-up / ui-e2e.
"""

from __future__ import annotations

import json
import hashlib
import os
from pathlib import Path
import runpy
import shutil
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
IMAGE_ID = "sha256:" + "c" * 64
BUILD_CONTEXT = {"goos": "linux", "goarch": "amd64", "build_tags": [], "cgo_enabled": True,
                 "compiler": "gc", "release_tags": ["go1.1"], "tool_tags": []}
AUDITED_PACKAGES = {MODULE, f"{MODULE}/ui", f"{MODULE}/internal/atom", f"{MODULE}/internal/testfault", f"{MODULE}/api", f"{MODULE}/cmd/run", f"{MODULE}/cmd/event", f"{MODULE}/internal/models",
                    *(p.rsplit("/", 1)[0] for p in (APPLY_CLI, APPLY_HTTP, APPLY_WRITE, EXPORT_HTTP, OTHER))}
DIFF_POLICY = {
    "basis": "policy", "scope": "audited-statement-files",
    "coverage_source": "integration+browser", "uncovered_changed_paths_max": 0,
}


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
        "image_id": IMAGE_ID, "build_context": BUILD_CONTEXT,
    }
    if source == "browser":
        doc.update(kind="gocoverdir", verified=True, image_provenance="built-by-this-run",
                   test_exit_code=0, exit_code=0, stop_rc=0, oom_killed=False,
                   collection="chromium-live-console")
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
    audit = Path(profiles_dir) / "coverpkg-packages.txt"
    audit.write_text("\n".join(sorted(AUDITED_PACKAGES)) + "\n")
    cmd = [
        sys.executable,
        str(CHECKER),
        "--profiles-dir", str(profiles_dir),
        "--candidate-sha", SHA,
        "--repo-root", str(ROOT),
        "--report", str(Path(profiles_dir) / "report.json"),
        "--coverpkg-audit", str(audit), "--diff-base", "fixture-base",
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
        "diff": DIFF_POLICY,
    }))
    env = os.environ.copy()
    env["CAESIUM_COVERAGE_ARTIFACTS"] = str(art)
    env["CAESIUM_COVERAGE_CHANGED_PATHS"] = str(changed)
    env["CAESIUM_COVERAGE_RATCHET"] = str(ratchet)
    env["CANDIDATE_SHA"] = SHA
    audit = art / "audit"
    audit.mkdir(exist_ok=True)
    (audit / "coverpkg-packages.txt").write_text("\n".join(sorted(AUDITED_PACKAGES)) + "\n")
    return env


def write_source_inventory(directory, records, **changes):
    package_files = {}
    complete_files = {}
    for package in sorted(AUDITED_PACKAGES):
        rel = package.removeprefix(MODULE).lstrip("/")
        package_files[package] = []
        for source in sorted((ROOT / rel).glob("*.go")):
            if source.name.endswith("_test.go"):
                continue
            path = package + "/" + source.name
            package_files[package].append(path)
            complete_files[path] = inventory_file(str(source.relative_to(ROOT)), has_function_body=True)
    complete_files.update(records)
    inventory = {
        "schema_version": 1, "kind": "go-ast-source-inventory", "parser": "go/parser",
        "complete": True, "packages": package_files, "build_context": BUILD_CONTEXT,
        "candidate_sha": SHA, "image_id": IMAGE_ID,
        "coverpkg_sha256": hashlib.sha256(("\n".join(sorted(AUDITED_PACKAGES)) + "\n").encode()).hexdigest(),
        "files": complete_files,
    }
    inventory.update(changes)
    path = Path(directory) / "source-inventory.json"
    path.write_text(json.dumps(inventory))
    return path


def inventory_file(relpath, **changes):
    record = {"parsed": True, "build_matched": True, "has_function_body": False, "has_call": False, "has_var_initializer": False,
              "source_sha256": hashlib.sha256((ROOT / relpath).read_bytes()).hexdigest()}
    record.update(changes)
    return record


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

    def test_missing_or_wrong_browser_image_and_process_evidence_cannot_pass(self):
        for field in ("test_exit_code", "image_id", "verified", "exit_code", "stop_rc", "oom_killed"):
            with self.subTest(field=field):
                prov = provenance("browser")
                del prov[field]
                write_source(self.dir, "browser", profile("set", block(f"{MODULE}/api/ui.go", 3, 1)), prov)
                result = run_checker(self.dir, extra=("--require-browser",))
                self.assertNotEqual(result.returncode, 0, output(result))
                self.assertNotEqual(json.loads((self.dir / "report.json").read_text())["contributions"]["browser"]["status"], "complete")
        write_source(self.dir, "browser", profile("set", block(f"{MODULE}/api/ui.go", 3, 1)), provenance("browser", image_id="sha256:" + "d" * 64))
        result = run_checker(self.dir, extra=("--require-browser",))
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("browser image must equal", output(result))


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
        self.assertNotIn("diff", doc, "an empty/uncovered count is not a measured diff policy")
        doc["diff"] = DIFF_POLICY
        baseline.write_text(json.dumps(doc))
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
            "diff": DIFF_POLICY,
        }))
        result = run_checker(self.dir, extra=("--ratchet", str(baseline)))
        self.assertEqual(result.returncode, 2, output(result))
        self.assertIn("no changed-paths input", output(result))

    def test_audited_diff_credits_browser_and_reports_excluded_and_empty_inputs(self):
        changed = self.dir / "changed.txt"
        changed.write_text("api/ui.go\ninternal/models/task.go\ntest/robustness/corelogic.go\ncmd/job/apply_test.go\n")
        inventory = write_source_inventory(self.dir, {f"{MODULE}/internal/models/task.go": inventory_file("internal/models/task.go")})
        policy = self.dir / "policy.json"
        policy.write_text(json.dumps({"schema_version": 1, "kind": "package-diff-ratchet", "source": "integration",
                                      "packages": {f"{MODULE}/cmd/job": {"min_percent": 0}}, "diff": DIFF_POLICY}))
        write_source(self.dir, "server", write_to_read_server() + block(f"{MODULE}/api/ui.go", 3, 0) + "\n", provenance("server"))
        write_source(self.dir, "browser", profile("set", block(f"{MODULE}/api/ui.go", 3, 1)), provenance("browser"))
        extra = ("--changed-paths", str(changed), "--ratchet", str(policy), "--require-browser", "--source-inventory", str(inventory))
        result = run_checker(self.dir, extra=extra)
        self.assertEqual(result.returncode, 0, output(result))
        report = json.loads((self.dir / "report.json").read_text())
        self.assertEqual(report["diff_coverage"]["changed_path_count"], 4)
        self.assertEqual(report["diff_coverage"]["eligible_paths"], [f"{MODULE}/api/ui.go"])
        self.assertFalse(report["diff_coverage"]["empty_diff"])
        self.assertEqual(report["diff_coverage"]["base"], "fixture-base")
        self.assertEqual(report["uncovered_changed_paths"], [])
        write_source(self.dir, "browser", profile("set", block(f"{MODULE}/api/ui.go", 3, 0)), provenance("browser"))
        self.assertEqual(run_checker(self.dir, extra=extra).returncode, 1, "eligible uncovered statement file must fail")
        changed.write_text("")
        self.assertEqual(run_checker(self.dir, extra=extra).returncode, 0)
        empty = json.loads((self.dir / "report.json").read_text())["diff_coverage"]
        self.assertTrue(empty["empty_diff"])
        self.assertEqual(empty["eligible_path_count"], 0)

    def test_partial_coverpkg_audit_cannot_exempt_profile_packages(self):
        audit = self.dir / "partial-audit.txt"
        audit.write_text(MODULE + "\n")
        result = run_checker(self.dir, extra=("--coverpkg-audit", str(audit)))
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("missing from the coverpkg audit", output(result))

    def test_omitted_real_executable_file_remains_eligible_without_profile_blocks(self):
        changed = self.dir / "changed.txt"
        changed.write_text("cmd/job/diff.go\n")
        policy = self.dir / "policy.json"
        policy.write_text(json.dumps({"schema_version": 1, "kind": "package-diff-ratchet", "source": "integration",
                                      "packages": {f"{MODULE}/cmd/job": {"min_percent": 0}}, "diff": DIFF_POLICY}))
        write_source(self.dir, "browser", profile("set", block(OTHER, 2, 1)), provenance("browser"))
        extra = ("--changed-paths", str(changed), "--ratchet", str(policy), "--require-browser")
        write_source(self.dir, "cli", write_to_read_cli() + func_block("cmd/job/diff.go", "sendDiffRequest", 4, 0) + "\n", provenance("cli"))
        self.assertEqual(run_checker(self.dir, extra=extra).returncode, 1)
        write_source(self.dir, "cli", write_to_read_cli(), provenance("cli"))
        for inv in (None, write_source_inventory(self.dir, {f"{MODULE}/internal/models/task.go": inventory_file("internal/models/task.go")})):
            result = run_checker(self.dir, extra=extra + (("--source-inventory", str(inv)) if inv else ()))
            self.assertEqual(result.returncode, 1, output(result))
            report = json.loads((self.dir / "report.json").read_text())
            self.assertEqual(report["diff_coverage"]["eligible_paths"], [f"{MODULE}/cmd/job/diff.go"])
            self.assertEqual(report["uncovered_changed_paths"], [f"{MODULE}/cmd/job/diff.go"])
        # A forged statement-free label cannot exempt the actual function
        # source, even with a matching digest and otherwise complete manifest.
        inv = write_source_inventory(self.dir, {f"{MODULE}/cmd/job/diff.go": inventory_file("cmd/job/diff.go")})
        self.assertEqual(run_checker(self.dir, extra=extra + ("--source-inventory", str(inv))).returncode, 1)

        # A package initializer without a function body is not coverable.
        changed.write_text("internal/models/models.go\n")
        inv = write_source_inventory(self.dir, {f"{MODULE}/internal/models/models.go": inventory_file("internal/models/models.go", has_var_initializer=True)})
        result = run_checker(self.dir, extra=extra + ("--source-inventory", str(inv)))
        self.assertEqual(result.returncode, 0, output(result))
        self.assertIn(f"{MODULE}/internal/models/models.go", json.loads((self.dir / "report.json").read_text())["diff_coverage"]["statement_free_paths"])
        inv = write_source_inventory(self.dir, {f"{MODULE}/internal/models/models.go": inventory_file("internal/models/models.go")})
        self.assertEqual(run_checker(self.dir, extra=extra + ("--source-inventory", str(inv))).returncode, 0,
                         "package initializer alone has no Go coverage function body")

    def test_statement_free_exemption_requires_valid_bound_inventory(self):
        path = f"{MODULE}/internal/models/task.go"
        changed = self.dir / "changed.txt"
        changed.write_text("internal/models/task.go\n")
        policy = self.dir / "policy.json"
        policy.write_text(json.dumps({"schema_version": 1, "kind": "package-diff-ratchet", "source": "integration",
                                      "packages": {f"{MODULE}/cmd/job": {"min_percent": 0}}, "diff": DIFF_POLICY}))
        write_source(self.dir, "browser", profile("set", block(OTHER, 2, 1)), provenance("browser"))
        extra = ("--changed-paths", str(changed), "--ratchet", str(policy), "--require-browser")
        inv = write_source_inventory(self.dir, {path: inventory_file("internal/models/task.go")})
        self.assertEqual(run_checker(self.dir, extra=extra + ("--source-inventory", str(inv))).returncode, 0)
        report = json.loads((self.dir / "report.json").read_text())
        self.assertEqual(report["diff_coverage"]["statement_free_paths"], [path])
        for field, value in (("candidate_sha", "b" * 40), ("image_id", "sha256:" + "d" * 64),
                             ("coverpkg_sha256", "0" * 64), ("files", {}), ("complete", False),
                             ("packages", {})):
            with self.subTest(field=field):
                inv = write_source_inventory(self.dir, {path: inventory_file("internal/models/task.go")}, **{field: value})
                self.assertEqual(run_checker(self.dir, extra=extra + ("--source-inventory", str(inv))).returncode, 1)
        for field in ("has_function_body", "parsed"):
            changes = {field: field != "parsed"}
            inv = write_source_inventory(self.dir, {path: inventory_file("internal/models/task.go", **changes)})
            self.assertEqual(run_checker(self.dir, extra=extra + ("--source-inventory", str(inv))).returncode, 1)
        inv = write_source_inventory(self.dir, {path: inventory_file("internal/models/task.go", source_sha256="0" * 64)})
        self.assertEqual(run_checker(self.dir, extra=extra + ("--source-inventory", str(inv))).returncode, 1)
        doc = json.loads(inv.read_text())
        del doc["files"][path]
        inv.write_text(json.dumps(doc))
        self.assertEqual(run_checker(self.dir, extra=extra + ("--source-inventory", str(inv))).returncode, 1,
                         "partial record map must not authorize an exemption")
        inv = write_source_inventory(self.dir, {path: inventory_file("internal/models/task.go")})
        doc = json.loads(inv.read_text())
        omitted = f"{MODULE}/internal/models/models.go"
        del doc["files"][omitted]
        doc["packages"][f"{MODULE}/internal/models"].remove(omitted)
        inv.write_text(json.dumps(doc))
        self.assertEqual(run_checker(self.dir, extra=extra + ("--source-inventory", str(inv))).returncode, 1,
                         "consistently truncated manifest must not authorize an exemption")
        self.assertFalse(COV["statement_free_file"](path, json.loads(write_source_inventory(self.dir, {path: inventory_file("internal/models/task.go")}).read_text()), repo_root=str(self.dir)),
                         "unavailable source cannot authorize an exemption")
        inv.write_text("{invalid")
        self.assertEqual(run_checker(self.dir, extra=extra + ("--source-inventory", str(inv))).returncode, 1)
        self.assertEqual(json.loads((self.dir / "report.json").read_text())["verdict"], "fail")

    def test_actual_uncoverable_files_and_build_exclusions_are_reported(self):
        changed = self.dir / "changed.txt"
        paths = ["internal/models/models.go", "internal/atom/atom.go", "ui/embed.go",
                 "internal/testfault/testfault.go", "internal/testfault/disabled.go"]
        changed.write_text("\n".join(paths) + "\n")
        records = {f"{MODULE}/{path}": inventory_file(path) for path in paths}
        records[f"{MODULE}/internal/testfault/testfault.go"].update(has_function_body=True, build_matched=False)
        records[f"{MODULE}/internal/testfault/disabled.go"].update(has_function_body=True)
        records[f"{MODULE}/internal/models/models.go"].update(has_var_initializer=True, has_call=True)
        inv = write_source_inventory(self.dir, records)
        policy = self.dir / "policy.json"
        policy.write_text(json.dumps({"schema_version": 1, "kind": "package-diff-ratchet", "source": "integration",
                                      "packages": {f"{MODULE}/cmd/job": {"min_percent": 0}}, "diff": DIFF_POLICY}))
        write_source(self.dir, "server", write_to_read_server() + block(f"{MODULE}/internal/testfault/disabled.go", 0, 0) + "\n", provenance("server"))
        write_source(self.dir, "browser", profile("set", block(OTHER, 2, 1)), provenance("browser"))
        extra = ("--changed-paths", str(changed), "--source-inventory", str(inv), "--ratchet", str(policy), "--require-browser")
        result = run_checker(self.dir, extra=extra)
        self.assertEqual(result.returncode, 0, output(result))
        diff = json.loads((self.dir / "report.json").read_text())["diff_coverage"]
        self.assertEqual(diff["eligible_paths"], [])
        self.assertEqual(diff["zero_statement_paths"], [f"{MODULE}/internal/testfault/disabled.go"])
        self.assertEqual(diff["build_excluded_paths"], [f"{MODULE}/internal/testfault/testfault.go"])
        self.assertEqual(diff["statement_free_paths"], sorted(f"{MODULE}/{path}" for path in paths[:3]))
        # Forging both the AST and build flag cannot exempt real function code.
        changed.write_text("cmd/job/diff.go\n")
        inv = write_source_inventory(self.dir, {f"{MODULE}/cmd/job/diff.go": inventory_file("cmd/job/diff.go", build_matched=False)})
        self.assertEqual(run_checker(self.dir, extra=extra).returncode, 1)
        # Erasing executable records from an otherwise complete manifest is rejected.
        doc = json.loads(inv.read_text()); del doc["files"][f"{MODULE}/cmd/job/diff.go"]
        inv.write_text(json.dumps(doc))
        self.assertEqual(run_checker(self.dir, extra=extra).returncode, 1)

    def test_function_body_guard_distinguishes_declarations_from_literals(self):
        declaration = "package fixture\ntype Engine interface { ID() string; Callback(func(int) error) }\nvar Assets any\nvar All = Register()\ntype Handler func() struct { ID int }\n"
        self.assertFalse(COV["source_has_function_body"](declaration))
        for code in ("func Empty() {}", "var F = func() int { return 1 }", "func (r *Reader) Read() (int, error) { return 0, nil }", "func Value() struct { ID int } { return struct{ID int}{1} }"):
            with self.subTest(source=code):
                self.assertTrue(COV["source_has_function_body"]("package fixture\n" + code))
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            path = f"{MODULE}/literal.go"
            source = root / "literal.go"; source.write_text("package fixture\nvar F = func() int { return 1 }\n")
            record = {"parsed": True, "has_function_body": False, "source_sha256": hashlib.sha256(source.read_bytes()).hexdigest()}
            self.assertFalse(COV["statement_free_file"](path, {"files": {path: record}}, repo_root=root), "forged FuncLit body flag must not grant exemption")

    def test_build_context_identity_and_independent_match_are_fail_closed(self):
        path = f"{MODULE}/internal/testfault/testfault.go"
        source = (ROOT / "internal/testfault/testfault.go").read_text()
        self.assertFalse(COV["source_build_matches"](path, source, BUILD_CONTEXT))
        self.assertTrue(COV["source_build_matches"](path, "/*\n//go:build testfault\n*/\npackage p\nfunc F() {}\n", BUILD_CONTEXT),
                        "a directive inside a block comment cannot authorize forged exclusion")
        tagged = {**BUILD_CONTEXT, "build_tags": ["testfault"]}
        self.assertTrue(COV["source_build_matches"](path, source, tagged))
        for name, code, expected in [("platform_windows.go", "package p\nfunc F() {}", False),
                                     ("platform_linux_amd64.go", "package p\nfunc F() {}", True),
                                     ("tag.go", "//go:build linux && (arm64 || amd64) && !testfault\n\npackage p\n", True)]:
            self.assertEqual(COV["source_build_matches"](f"{MODULE}/{name}", code, BUILD_CONTEXT), expected)
        inv = write_source_inventory(self.dir, {path: inventory_file("internal/testfault/testfault.go", has_function_body=True, build_matched=False)}, build_context={**BUILD_CONTEXT, "goarch": "arm64"})
        result = run_checker(self.dir, extra=("--source-inventory", str(inv)))
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("build context differs", output(result))


    def test_malformed_merged_provenance_context_fails_closed(self):
        inventory = json.loads(write_source_inventory(self.dir, {}).read_text())
        for parts in (None, [], {}, {"cli": None}, {"cli": {}}):
            with self.subTest(parts=parts):
                issues = []
                result = COV["validate_source_inventory"](
                    inventory, candidate_sha=SHA,
                    image_records=[{"image_id": IMAGE_ID, "source_provenance": parts}],
                    audited_packages=AUDITED_PACKAGES, repo_root=ROOT, issues=issues)
                self.assertIsNone(result)
                self.assertTrue(any("build context differs" in issue.message for issue in issues))


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

    def test_changed_reagents_without_profile_is_reported_unmeasured_outside_root_diff(self):
        changed = self.dir / "changed.txt"
        changed.write_text("reagents/cmd/git-source/main.go\n")
        result = run_checker(self.dir, extra=("--changed-paths", str(changed)))
        self.assertEqual(result.returncode, 0, output(result))
        report = json.loads((self.dir / "report.json").read_text())
        self.assertEqual(report["contributions"]["reagents"]["status"], "unmeasured")
        self.assertIsNone(report["contributions"]["reagents"]["percent"])
        self.assertEqual(report["diff_coverage"]["separate_module"], {"paths": [f"{COV['REAGENTS_MODULE']}/cmd/git-source/main.go"], "status": "unmeasured", "in_root_diff_scope": False})

        required = run_checker(self.dir, extra=("--changed-paths", str(changed), "--require-reagents"))
        self.assertEqual(required.returncode, 2, output(required))

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


class CollectorMergeProvenanceTests(unittest.TestCase):
    """Drive real merge-mode shell wiring with a deterministic covdata stand-in."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.art = Path(self.tmp.name)
        self.profiles = self.art / "profiles"
        self.profiles.mkdir()
        self.originals = {}
        for source in ("cli", "server"):
            raw = self.art / "raw" / source
            raw.mkdir(parents=True)
            (raw / "covmeta.fake").write_text("meta")
            (raw / "covcounters.fake").write_text("counters")
            self.originals[source] = provenance(
                source, kind="gocoverdir", verified=True,
                image_provenance="built-by-this-run", image_id="sha256:" + "c" * 64,
                exit_code=0, stop_rc=0, signal="SIGTERM", oom_killed=False,
            )
            write_source(self.profiles, source, prov=self.originals[source])
        (self.art / "fake-cli.out").write_text(write_to_read_cli())
        (self.art / "fake-server.out").write_text(write_to_read_server())
        (self.art / "fake-browser.out").write_text(profile("set", block(f"{MODULE}/api/ui.go", 3, 1)))
        fake = self.art / "fake-container"
        fake.write_text('''#!/usr/bin/env python3
import os, pathlib, shutil, sys
root = pathlib.Path(os.environ["FAKE_COV_ROOT"])
with (root / "container-calls").open("a") as log:
    log.write(" ".join(sys.argv[1:]) + "\\n")
args = sys.argv[1:]
mounts = {}
for i, value in enumerate(args):
    if value == "-v":
        host, inside, *rest = args[i + 1].split(":")
        mounts[inside] = pathlib.Path(host)
if "textfmt" in args:
    out = next(value[3:] for value in args if value.startswith("-o="))
    dest = mounts["/out"] / pathlib.Path(out).name
    source = mounts["/in"].name
    if source == "integration":
        cli = (root / "fake-cli.out").read_text().splitlines()
        server = (root / "fake-server.out").read_text().splitlines()
        dest.write_text("\\n".join(cli + server[1:]) + "\\n")
    else:
        shutil.copyfile(root / ("fake-" + source + ".out"), dest)
elif "merge" in args:
    (mounts["/out"] / "covmeta.fake").write_text("meta")
    (mounts["/out"] / "covcounters.fake").write_text("counters")
''')
        fake.chmod(0o755)
        self.env = collector_test_env(self.art)
        self.env["CAESIUM_CONTAINER_CLI"] = str(fake)
        self.env["FAKE_COV_ROOT"] = str(self.art)
        write_source(self.profiles, "browser", profile("set", block(f"{MODULE}/api/ui.go", 3, 1)), provenance("browser"))

    def merge(self):
        return subprocess.run(
            ["bash", str(COLLECTOR), "merge"],
            capture_output=True, text=True, env=self.env, cwd=str(ROOT),
        )

    def test_complete_originals_are_preserved_and_named_in_merged_provenance(self):
        originals = {
            source: (self.profiles / f"{source}.provenance.json").read_bytes()
            for source in ("cli", "server")
        }
        result = self.merge()
        self.assertEqual(result.returncode, 0, output(result))
        for source, content in originals.items():
            self.assertEqual((self.profiles / f"{source}.provenance.json").read_bytes(), content)
        merged = json.loads((self.profiles / "integration.provenance.json").read_text())
        self.assertEqual(merged["source_provenance"], self.originals)
        self.assertEqual(merged["image_id"], self.originals["cli"]["image_id"])

    def test_foreign_killed_unverified_or_missing_originals_never_convert_or_pass(self):
        cases = {
            "foreign": {"candidate_sha": "b" * 40},
            "killed": {"complete": False, "killed": True, "signal": "SIGKILL"},
            "unverified": {"verified": False, "image_provenance": "supplied/unverified"},
            "review_repro": {"candidate_sha": "b" * 40, "complete": False, "killed": True, "verified": False, "image_provenance": "supplied/unverified"},
            "missing": None,
        }
        for case, changes in cases.items():
            with self.subTest(case=case):
                source_path = self.profiles / "cli.provenance.json"
                if changes is None:
                    source_path.unlink(missing_ok=True)
                    original = None
                else:
                    source_path.write_text(json.dumps({**self.originals["cli"], **changes}))
                    original = source_path.read_bytes()
                write_source(self.profiles, "cli", write_to_read_cli())
                write_source(self.profiles, "server", write_to_read_server())
                write_source(self.profiles, "integration", write_to_read_server(), provenance("integration"))
                result = self.merge()
                self.assertNotEqual(result.returncode, 0, output(result))
                report = json.loads((self.art / "report.json").read_text())
                self.assertNotEqual(report["verdict"], "pass")
                self.assertFalse((self.art / "container-calls").exists(), "conversion must follow provenance validation")
                self.assertFalse((self.profiles / "integration.provenance.json").exists())
                self.assertFalse((self.profiles / "integration.out").exists())
                self.assertFalse((self.profiles / "cli.out").exists())
                self.assertFalse((self.profiles / "server.out").exists())
                if original is None:
                    self.assertFalse(source_path.exists())
                else:
                    self.assertEqual(source_path.read_bytes(), original)

    def test_killed_browser_stays_incomplete_on_check_and_merge_without_opt_in(self):
        write_source(self.profiles, "cli", write_to_read_cli(), self.originals["cli"])
        write_source(self.profiles, "server", write_to_read_server(), self.originals["server"])
        write_source(self.profiles, "browser", prov=provenance("browser", complete=False, killed=True, exit_code=137, oom_killed=True))
        for mode in ("check", "merge"):
            with self.subTest(mode=mode):
                result = subprocess.run(["bash", str(COLLECTOR), mode], capture_output=True, text=True, env=self.env, cwd=str(ROOT))
                self.assertEqual(result.returncode, 2, output(result))
                report = json.loads((self.art / "report.json").read_text())
                self.assertEqual(report["verdict"], "incomplete")
                self.assertTrue(report["browser_required"])
                self.assertFalse((self.art / "ratchet.json").exists())

    def test_external_browser_raw_missing_or_empty_cannot_reuse_stale_counters(self):
        raw = self.art / "raw/browser"
        raw.mkdir()
        (raw / "covmeta.stale").write_text("stale")
        (raw / "covcounters.stale").write_text("stale")
        prov = self.art / "external-browser.provenance.json"
        prov.write_text(json.dumps(provenance("browser")))
        self.env["CAESIUM_COVERAGE_BROWSER_PROVENANCE"] = str(prov)
        empty = self.art / "empty"
        empty.mkdir()
        for source in (self.art / "missing", empty):
            with self.subTest(source=source.name):
                self.env["CAESIUM_COVERAGE_BROWSER_DIR"] = str(source)
                result = subprocess.run(["bash", str(COLLECTOR), "check"], capture_output=True, text=True, env=self.env, cwd=str(ROOT))
                self.assertEqual(result.returncode, 1, output(result))
                self.assertNotEqual(json.loads((self.art / "report.json").read_text())["verdict"], "pass")
        supplied = self.art / "supplied"
        supplied.mkdir()
        (supplied / "covmeta.current").write_text("current")
        (supplied / "covcounters.current").write_text("current")
        self.env["CAESIUM_COVERAGE_BROWSER_DIR"] = str(supplied)
        self.assertEqual(self.merge().returncode, 0)
        self.assertEqual({p.name for p in raw.iterdir()}, {"covmeta.current", "covcounters.current"})

    def test_external_browser_copy_failure_cannot_reuse_stale_evidence(self):
        for source, text in (("cli", write_to_read_cli()), ("server", write_to_read_server())):
            write_source(self.profiles, source, text, self.originals[source])
        raw = self.art / "raw/browser"; raw.mkdir()
        (raw / "covmeta.stale").write_text("stale")
        (raw / "covcounters.stale").write_text("stale")
        supplied = self.art / "supplied"; supplied.mkdir()
        (supplied / "covmeta.current").write_text("current")
        (supplied / "covcounters.current").write_text("current")
        prov = self.art / "external.json"; prov.write_text(json.dumps(provenance("browser")))
        fakebin = self.art / "bin"; fakebin.mkdir()
        copier = fakebin / "cp"; copier.write_text("#!/bin/sh\nexit 17\n"); copier.chmod(0o755)
        self.env.update(CAESIUM_COVERAGE_BROWSER_DIR=str(supplied), CAESIUM_COVERAGE_BROWSER_PROVENANCE=str(prov),
                        PATH=str(fakebin) + os.pathsep + self.env["PATH"])
        result = subprocess.run(["bash", str(COLLECTOR), "check"], capture_output=True, text=True, env=self.env, cwd=ROOT)
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("cannot copy external browser", output(result))
        self.assertEqual(json.loads((self.art / "report.json").read_text())["verdict"], "incomplete")
        self.assertFalse((self.art / "ratchet.json").exists())
        self.assertEqual({p.name for p in raw.iterdir()}, {"covmeta.stale", "covcounters.stale"})
        self.assertEqual(list((self.art / "raw").glob("browser-import.*")), [])


class CollectorCollectTests(unittest.TestCase):
    """Exercise collector lifecycle decisions without launching containers/browsers."""

    def setUp(self):
        CollectorMergeProvenanceTests.setUp(self)
        self.bin = self.art / "bin"
        self.bin.mkdir()
        self.env.update(CAESIUM_COVERAGE_ID="cov-test", CAESIUM_COVERAGE_KEEP="1")
        self.env["PATH"] = str(self.bin) + os.pathsep + self.env["PATH"]
        self.env["FAKE_BROWSER_CHECKER"] = str(BROWSER_CHECKER)
        self.env["FAKE_BROWSER_JOURNEY"] = str(BROWSER_JOURNEY)
        write_source_inventory(self.art, {})
        git = self.bin / "git"
        git.write_text('''#!/usr/bin/env python3
import sys
args=sys.argv[1:]
if "status" in args: pass
elif "--git-dir" in args: print(".git")
else: print("a" * 40)
''')
        sleeper = self.bin / "sleep"
        sleeper.write_text("#!/bin/sh\nexit 0\n")
        bash = self.bin / "bash"
        bash.write_text('''#!/usr/bin/env python3
import os,sys,json,pathlib,subprocess
if len(sys.argv)>1 and sys.argv[1]==os.environ["FAKE_BROWSER_JOURNEY"]:
    failed=os.environ.get("FAKE_SCENARIO")=="journey-failed"
    report=pathlib.Path(sys.argv[3]);report.write_text(json.dumps({"suites":[{"specs":[
      {"title":"sidebar navigates between every primary control-plane page","tests":[{"results":[{"status":"failed" if failed else "passed"}]}]},
      {"title":"operator can pause and unpause a job from the detail page","tests":[{"results":[{"status":"passed"}]}]}
    ]}]}))
    raise SystemExit(subprocess.run([sys.executable,os.environ["FAKE_BROWSER_CHECKER"],str(report)]).returncode)
os.execv("/bin/bash",["bash"]+sys.argv[1:])
''')
        for path in (git, sleeper, bash):
            path.chmod(0o755)
        container = self.art / "fake-container"
        with container.open("a") as stream:
            stream.write('''else:
    name=next((args[i+1] for i,v in enumerate(args) if v=="--name"),"")
    names_path=root/"container-names.json"
    names=json.loads(names_path.read_text()) if names_path.exists() else []
    if args[:2]==["rm","-f"]:
        names=[n for n in names if n not in args[2:]]
    elif args and args[0]=="build": pass
    elif args[:2]==["image","inspect"]:
        if "--format" in args:
            fmt=args[args.index("--format")+1]
            if "revision" in fmt: print("a"*40)
            elif ".Os" in fmt: print("linux")
            elif ".Architecture" in fmt: print("amd64")
            elif "builder" in args[-1]: print("sha256:"+("d" if "@" in args[-1] and os.environ.get("FAKE_SCENARIO")=="builder-mismatch" else "b")*64)
            elif args[-1]=="caesiumcloud/caesium-coverage:latest" and (root/"retagged").exists(): print("sha256:"+"d"*64)
            else: print("sha256:"+"c"*64)
    elif args and args[0]=="create": print("audit-export")
    elif args and args[0]=="port": print("127.0.0.1:12345")
    elif args and args[0]=="inspect":
        killed=args[-1].endswith("-browser") and os.environ.get("FAKE_SCENARIO")=="killed"
        print(json.dumps([{"State":{"ExitCode":137 if killed else 0,"OOMKilled":killed}}]))
    elif args and args[0]=="run":
        if name and os.environ.get("FAKE_SCENARIO")=="retag":
            (root/"retagged").write_text("other candidate now owns the tag")
        if mounts.get("/source"):
            if "-i" not in args: raise SystemExit("inventory stdin was not attached")
            shutil.copyfile(root/"source-inventory.json",mounts["/audit"]/"source-inventory.json")
        if name:
            if name in names: raise SystemExit("container name already exists: "+name)
            names.append(name)
        if "wget" in args: print("healthy")
        for inside in ("/coverage","/var/lib/caesium/coverage"):
            if inside in mounts:
                (mounts[inside]/"covmeta.fake").write_text("meta")
                (mounts[inside]/"covcounters.fake").write_text("counters")
    names_path.write_text(json.dumps(names))
''')
        # The stand-in only emulates CLI responses; it cannot launch a real engine.
        code = container.read_text().replace("import os, pathlib, shutil, sys", "import os, pathlib, shutil, sys, json")
        code = code.replace('args = sys.argv[1:]', 'args = sys.argv[1:]\nwith (root / "container-args.jsonl").open("a") as log: log.write(json.dumps(args) + "\\n")')
        container.write_text(code)
        (self.art / "container-names.json").write_text(json.dumps(["cov-test-browser"]))

    def collect(self, scenario):
        self.env["FAKE_SCENARIO"] = scenario
        return subprocess.run(["bash", str(COLLECTOR), "collect"], capture_output=True, text=True,
                              env=self.env, cwd=str(ROOT), timeout=30)

    def test_clean_collect_requires_browser_and_replaces_kept_browser(self):
        result = self.collect("clean")
        self.assertEqual(result.returncode, 0, output(result))
        report = json.loads((self.art / "report.json").read_text())
        self.assertEqual(report["verdict"], "pass")
        self.assertTrue(report["browser_required"])
        browser = json.loads((self.profiles / "browser.provenance.json").read_text())
        self.assertTrue(browser["complete"])
        self.assertEqual(browser["test_exit_code"], 0)
        self.assertEqual(browser["image_id"], IMAGE_ID)
        self.assertIn("cov-test-browser", result.stdout.split("KEEP=1;")[-1])


    def test_retag_between_collection_launches_cannot_change_measured_image(self):
        result = self.collect("retag")
        self.assertEqual(result.returncode, 0, output(result))
        self.assertTrue((self.art / "retagged").exists(), "fake engine must change tag after first process starts")
        calls = [json.loads(line) for line in (self.art / "container-args.jsonl").read_text().splitlines()]
        builds = [args for args in calls if args[0] == "build"]
        self.assertEqual(len(builds), 1)
        self.assertIn("BUILDER_IMAGE=caesiumcloud/caesium-builder:latest@sha256:" + "b" * 64, builds[0])
        launches = [args for args in calls if args[0] in ("run", "create")]
        self.assertGreater(len(launches), 7)
        for args in launches:
            self.assertNotIn("caesiumcloud/caesium-coverage:latest", args)
            self.assertNotIn("caesiumcloud/caesium-builder:latest", args)
            self.assertTrue(IMAGE_ID in args or "sha256:" + "b" * 64 in args, args)
        for source in ("cli", "server", "integration", "browser"):
            self.assertEqual(json.loads((self.profiles / f"{source}.provenance.json").read_text())["image_id"], IMAGE_ID)

    def test_builder_canonical_reference_must_be_named_and_match_pinned_id(self):
        for builder in ("sha256:" + "b" * 64, "b" * 64, "[invalid]", "builder:latest@@sha256:" + "b" * 64):
            with self.subTest(builder=builder):
                self.env["CAESIUM_BUILDER_IMAGE"] = builder
                result = self.collect("clean")
                self.assertEqual(result.returncode, 1, output(result))
                self.assertIn("valid named reference", output(result))
        self.env.pop("CAESIUM_BUILDER_IMAGE")
        result = self.collect("builder-mismatch")
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("differs from the resolved builder image", output(result))
        calls = [json.loads(line) for line in (self.art / "container-args.jsonl").read_text().splitlines()]
        self.assertFalse(any(args[0] in ("build", "run", "create") for args in calls),
                         "malformed/mismatched builder references must fail before build or collection")
        self.env["CAESIUM_BUILDER_IMAGE"] = "caesiumcloud/caesium-builder:latest@sha256:" + "e" * 64
        result = self.collect("clean")
        self.assertEqual(result.returncode, 0, output(result))
        calls = [json.loads(line) for line in (self.art / "container-args.jsonl").read_text().splitlines()]
        builds = [args for args in calls if args[0] == "build"]
        self.assertIn("BUILDER_IMAGE=caesiumcloud/caesium-builder:latest@sha256:" + "b" * 64, builds[0])

    def test_failed_journey_or_killed_server_cannot_collect_or_recheck_success(self):
        for scenario in ("journey-failed", "killed"):
            with self.subTest(scenario=scenario):
                result = self.collect(scenario)
                self.assertNotEqual(result.returncode, 0, output(result))
                browser = json.loads((self.profiles / "browser.provenance.json").read_text())
                self.assertFalse(browser["complete"])
                self.assertEqual(browser["killed"], scenario == "killed")
                for mode in ("check", "merge"):
                    checked = subprocess.run(["bash", str(COLLECTOR), mode], capture_output=True, text=True,
                                             env=self.env, cwd=str(ROOT), timeout=30)
                    self.assertNotEqual(checked.returncode, 0, output(checked))
                    self.assertNotEqual(json.loads((self.art / "report.json").read_text())["verdict"], "pass")
                self.assertFalse((self.art / "ratchet.json").exists())


class DockerfileAndCollectorTests(unittest.TestCase):
    @unittest.skipUnless(shutil.which("go"), "Go is required for the standalone AST inventory fixture")
    def test_inventory_parses_clean_source_without_resolving_ui_embeds(self):
        # Run only the standard-library inventory helper, never compile product
        # source. A clean checkout's missing ui/dist must not prevent AST proof.
        helper = COLLECTOR.read_text().split("cat >/tmp/coverage-source-inventory.go <<'GO'\n", 1)[1].split("\nGO\n", 1)[0]
        with tempfile.TemporaryDirectory(prefix="coverage-inventory.") as tmp:
            root = Path(tmp).resolve()
            source, audit = root / "source", root / "audit"
            (source / "ui").mkdir(parents=True)
            audit.mkdir()
            embed = source / "ui/embed.go"
            embed.write_text('package ui\nimport "embed"\n//go:embed all:dist\nvar Assets embed.FS\n')
            types = source / "ui/types.go"
            types.write_text("package ui\ntype Declaration interface { ID() string; Callback(func(int) error) }\nvar Uninitialised int\nvar Registered = len([]int{1})\n")
            functions = source / "ui/functions.go"
            functions.write_text("package ui\nfunc Value() int { return 1 }\nvar Initialized = Value()\nvar Literal = func() int { return 2 }\n")
            (source / "ui/excluded.go").write_text("//go:build testfault\n\npackage ui\nfunc Fault() { println(1) }\n")
            (source / "ui/platform_windows.go").write_text("package ui\nfunc Platform() { println(1) }\n")
            (source / "ui/empty.go").write_text("package ui\nfunc Empty() {}\n")
            (source / "ui/ignored_test.go").write_text("not valid Go source")
            package = MODULE + "/ui"
            manifest = audit / "coverpkg-packages.txt"
            manifest.write_text(package + "\n")
            executable = root / "inventory.go"
            executable.write_text(helper.replace('"/source', '"' + str(source)).replace('"/audit', '"' + str(audit)))
            env = os.environ.copy()
            env.update(GOTOOLCHAIN="local", GOPROXY="off", GOWORK="off", GO111MODULE="off",
                       INVENTORY_SHA=SHA, INVENTORY_IMAGE_ID=IMAGE_ID, INVENTORY_GOOS="linux", INVENTORY_GOARCH="amd64")
            def inventory_run():
                return subprocess.run(["go", "run", str(executable)], cwd=root, env=env,
                                      text=True, capture_output=True, timeout=60)
            result = inventory_run()
            self.assertEqual(result.returncode, 0, output(result))
            doc = json.loads((audit / "source-inventory.json").read_text())
            self.assertEqual(doc["packages"][package], [package + "/embed.go", package + "/empty.go", package + "/excluded.go", package + "/functions.go", package + "/platform_windows.go", package + "/types.go"])
            self.assertFalse(doc["files"][package + "/embed.go"]["has_var_initializer"])
            for field in ("has_function_body", "has_call", "has_var_initializer"):
                self.assertTrue(doc["files"][package + "/functions.go"][field])
            self.assertFalse(doc["files"][package + "/types.go"]["has_function_body"])
            self.assertTrue(doc["files"][package + "/types.go"]["has_var_initializer"])
            self.assertTrue(doc["files"][package + "/types.go"]["has_call"])
            self.assertTrue(doc["files"][package + "/empty.go"]["has_function_body"])
            self.assertFalse(doc["files"][package + "/excluded.go"]["build_matched"])
            self.assertFalse(doc["files"][package + "/platform_windows.go"]["build_matched"])
            self.assertTrue(doc["files"][package + "/functions.go"]["build_matched"])
            self.assertEqual(doc["build_context"]["goos"], "linux")
            self.assertEqual(doc["files"][package + "/embed.go"]["source_sha256"], hashlib.sha256(embed.read_bytes()).hexdigest())
            self.assertFalse((source / "ui/dist").exists())
            for bad in (MODULE + "/missing", MODULE + "/../escape", "example.invalid/foreign"):
                with self.subTest(package=bad):
                    manifest.write_text(bad + "\n")
                    self.assertNotEqual(inventory_run().returncode, 0)
            manifest.write_text(package + "\n")
            types.write_text("package ui\ntype Broken struct {")
            result = inventory_run()
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("types.go", result.stderr, "parser diagnostics must remain visible")

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
        for script in (COLLECTOR, BROWSER_JOURNEY):
            with self.subTest(script=script.name):
                result = subprocess.run(["bash", "-n", str(script)], capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_browser_journey_rejects_invalid_url_or_missing_report_before_npm(self):
        for args in (("https://example.com", "/tmp/unused.json"), ("http://127.0.0.1:12345",)):
            result = subprocess.run(["bash", str(BROWSER_JOURNEY), *args], capture_output=True, text=True)
            self.assertEqual(result.returncode, 1, output(result))
            self.assertNotIn("npm", output(result))

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
        write_source(profiles, "browser", profile("set", block(f"{MODULE}/api/ui.go", 3, 1)), provenance("browser"))
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
