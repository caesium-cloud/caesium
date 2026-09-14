"""Regression checks for the CI gate, path selection and full-suite wiring."""

import base64
import copy
import io
import fnmatch
import itertools
import json
import os
from pathlib import Path
import re
import runpy
import shlex
import subprocess
import sys
import tempfile
import unittest
import zipfile

import yaml


ROOT = Path(__file__).resolve().parents[1]
WORKFLOW = yaml.safe_load((ROOT / ".github/workflows/ci.yml").read_text())
JOBS = WORKFLOW["jobs"]
SELECTORS = runpy.run_path(str(ROOT / "scripts/ci-ok.py"))["SELECTORS"]
FLAGS = ("go", "ui", "helm", "reagents", "ci")


def selected_outputs(enabled=()):
    outputs = {name: str(name in enabled).lower() for name in FLAGS}
    outputs["images"] = str(bool(enabled)).lower()
    return outputs


def results(outputs):
    needs = {}
    for name in JOBS["ci-ok"]["needs"]:
        selectors = SELECTORS[name]
        selected = not selectors or any(outputs[key] == "true" for key in selectors)
        needs[name] = {"result": "success" if selected else "skipped"}
    needs["changes"]["outputs"] = outputs
    return needs


class GateTests(unittest.TestCase):
    def gate(self, needs, expected):
        result = subprocess.run(
            [sys.executable, str(ROOT / "scripts/ci-ok.py"), *JOBS["ci-ok"]["needs"]],
            env={**os.environ, "NEEDS_JSON": json.dumps(needs)},
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, expected, result.stdout + result.stderr)

    def test_every_change_combination(self):
        for bits in itertools.product((False, True), repeat=len(FLAGS)):
            enabled = [name for name, bit in zip(FLAGS, bits) if bit]
            with self.subTest(enabled=enabled):
                self.gate(results(selected_outputs(enabled)), 0)

    def test_failed_cancelled_missing_and_unexpectedly_skipped_jobs(self):
        baseline = results(selected_outputs(FLAGS))
        for name in baseline:
            for status in ("failure", "cancelled", "skipped", "unknown", None):
                with self.subTest(job=name, status=status):
                    needs = copy.deepcopy(baseline)
                    if status is None:
                        del needs[name]
                    else:
                        needs[name]["result"] = status
                    self.gate(needs, 1)

    def test_failed_producer_cannot_hide_behind_skipped_consumers(self):
        needs = results(selected_outputs(("ui",)))
        needs["images"]["result"] = "failure"
        needs["ui-e2e"]["result"] = "skipped"
        needs["ui-e2e-auth"]["result"] = "skipped"
        self.gate(needs, 1)

    def test_invalid_inputs(self):
        for needs in (None, [], {}, {"changes": "success"}):
            self.gate(needs, 1)
        for outputs in ({}, selected_outputs() | {"images": "true"}, selected_outputs() | {"go": None}):
            needs = results(selected_outputs())
            needs["changes"]["outputs"] = outputs
            self.gate(needs, 1)


class WorkflowTests(unittest.TestCase):
    def test_ci_config_discovers_all_validator_tests(self):
        commands = [line.strip() for step in JOBS["ci-config"]["steps"]
                    for line in step.get("run", "").splitlines()
                    if "unittest discover" in line]
        self.assertEqual(commands, ["python3 -m unittest discover -s scripts -p 'test_*.py' -v"])
        # Execute the workflow's selector on an additional validator module. A
        # narrowed pattern would silently miss its failure and return success.
        with tempfile.TemporaryDirectory() as tmp:
            (Path(tmp) / "test_extra_validator.py").write_text(
                "import unittest\n"
                "class ExtraValidator(unittest.TestCase):\n"
                "    def test_failure(self):\n"
                "        self.fail('extra validator was discovered')\n"
            )
            command = shlex.split(commands[0])
            command[0] = sys.executable
            command[command.index("-s") + 1] = tmp
            result = subprocess.run(command, capture_output=True, text=True)
            self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
            self.assertIn("extra validator was discovered", result.stderr)
            self.assertIn("Ran 1 test", result.stderr)

    def test_browser_diagnostics_survive_setup_and_test_failures(self):
        for name, container in (("ui-e2e", "caesium-server"),
                                ("ui-e2e-auth", "caesium-server-auth")):
            with self.subTest(job=name):
                steps = JOBS[name]["steps"]
                collect = next(step for step in steps if step.get("name") == "Collect browser diagnostics")
                upload = next(step for step in steps if step.get("name") == "Upload browser diagnostics")
                cleanup = steps[-1]
                browser = next(step for step in steps if step.get("id") == "playwright")
                self.assertLess(steps.index(browser), steps.index(collect))
                self.assertLess(steps.index(collect), steps.index(upload))
                self.assertLess(steps.index(upload), steps.index(cleanup))
                for step in (collect, upload, cleanup):
                    self.assertEqual(step["if"], "always()")
                for step in (browser, collect, upload):
                    self.assertFalse(step.get("continue-on-error", False))
                self.assertEqual(upload["uses"], "actions/upload-artifact@v7")
                self.assertEqual(upload["with"]["if-no-files-found"], "error")
                self.assertEqual(upload["with"]["path"], "ui/ci-artifacts/")
                self.assertIn(name, upload["with"]["name"])
                # Run the real collection script with a failed setup and a
                # missing server, then with a failed test and retained logs.
                for setup_failed in (False, True):
                    with self.subTest(setup_failed=setup_failed), tempfile.TemporaryDirectory() as tmp:
                        root = Path(tmp)
                        docker = root / "docker"
                        docker.write_text(
                            '#!/bin/sh\n'
                            'if [ "$1" = logs ]; then\n'
                            '  echo "server diagnostic for $2 bootstrap csk_fixture-secret_123"\n'
                            f'  exit {1 if setup_failed else 0}\n'
                            'fi\necho "container status"\n'
                        )
                        docker.chmod(0o755)
                        trace = io.BytesIO()
                        with zipfile.ZipFile(trace, "w", zipfile.ZIP_DEFLATED) as archive:
                            archive.writestr("0-trace.network", json.dumps({
                                "headers": {"Authorization": "Bearer csk_trace-secret_456"},
                            }))
                            archive.writestr("0-trace.trace", '{"type":"context-options"}\n')
                            archive.writestr("resources/page.html", "<p>csk_snapshot-secret_789</p>")
                            archive.writestr("resources/image.png", b"\x89PNG\r\n\x1a\n")
                        (root / "ui/test-results").mkdir(parents=True)
                        (root / "ui/test-results/trace.zip").write_bytes(trace.getvalue())
                        (root / "ui/playwright-report").mkdir()
                        (root / "ui/playwright-report/index.html").write_text(
                            '<template id="playwrightReportBase64">data:application/zip;base64,'
                            + base64.b64encode(trace.getvalue()).decode() + '</template>'
                        )
                        (root / "ui/playwright-results.json").write_text(
                            '{"error":"csk_json-secret_123","status":"flaky"}'
                        )
                        outcomes = {
                            "server": {"outcome": "failure" if setup_failed else "success",
                                       "conclusion": "failure" if setup_failed else "success",
                                       "outputs": {"sensitive": "must not appear"}},
                            "playwright": {"outcome": "skipped" if setup_failed else "failure",
                                           "conclusion": "skipped" if setup_failed else "failure"},
                        }
                        result = subprocess.run(
                            ["bash", "-e", "-o", "pipefail", "-c", collect["run"]], cwd=tmp,
                            env={**os.environ, "PATH": tmp + os.pathsep + os.environ["PATH"],
                                 "STEP_RESULTS": json.dumps(outcomes), "CANDIDATE_SHA": "candidate",
                                 "GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "2", "GITHUB_JOB": name},
                            capture_output=True, text=True,
                        )
                        self.assertEqual(result.returncode, int(setup_failed), result.stderr)
                        artifacts = root / "ui/ci-artifacts"
                        reports = artifacts / "ci-diagnostics"
                        report = json.loads((reports / "outcomes.json").read_text())
                        self.assertEqual(report["candidate_sha"], "candidate")
                        self.assertEqual(report["steps"]["playwright"]["outcome"],
                                         "skipped" if setup_failed else "failure")
                        self.assertNotIn("must not appear", json.dumps(report))
                        server_log = (reports / "server.log").read_text()
                        self.assertIn(container, server_log)
                        self.assertIn("[REDACTED_API_KEY]", server_log)
                        self.assertNotIn("csk_fixture-secret_123", server_log)
                        self.assertIn("container status", (reports / "containers.log").read_text())
                        if setup_failed:
                            self.assertIn("::error::Could not collect", result.stdout)
                        # Both the standalone trace and HTML's embedded ZIP
                        # retain readable entries while removing credentials.
                        html = (artifacts / "playwright-report/index.html").read_text()
                        embedded = re.search(r"base64,([A-Za-z0-9+/=]+)", html)[1]
                        for data in ((artifacts / "test-results/trace.zip").read_bytes(),
                                     base64.b64decode(embedded)):
                            with zipfile.ZipFile(io.BytesIO(data)) as archive:
                                self.assertIsNone(archive.testzip())
                                self.assertEqual(archive.read("resources/image.png"), b"\x89PNG\r\n\x1a\n")
                                for entry in archive.infolist():
                                    self.assertNotIn(b"csk_", archive.read(entry))
                                headers = json.loads(archive.read("0-trace.network"))["headers"]
                                self.assertEqual(headers["Authorization"], "Bearer [REDACTED_API_KEY]")
                        structured = json.loads((artifacts / "playwright-results.json").read_text())
                        self.assertEqual(structured, {"error": "[REDACTED_API_KEY]", "status": "flaky"})

    def test_malformed_browser_archive_never_uploads_raw_credentials(self):
        for name in ("ui-e2e", "ui-e2e-auth"):
            collect = next(step for step in JOBS[name]["steps"]
                           if step.get("name") == "Collect browser diagnostics")
            with self.subTest(job=name), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                docker = root / "docker"
                docker.write_text('#!/bin/sh\necho "diagnostic csk_server_secret"\n')
                docker.chmod(0o755)
                (root / "ui/test-results").mkdir(parents=True)
                (root / "ui/test-results/trace.zip").write_bytes(b"invalid zip csk_trace_secret")
                result = subprocess.run(
                    ["bash", "-e", "-o", "pipefail", "-c", collect["run"]], cwd=tmp,
                    env={**os.environ, "PATH": tmp + os.pathsep + os.environ["PATH"],
                         "STEP_RESULTS": "{}", "CANDIDATE_SHA": "candidate",
                         "GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "2", "GITHUB_JOB": name},
                    capture_output=True, text=True,
                )
                self.assertNotEqual(result.returncode, 0)
                artifacts = root / "ui/ci-artifacts"
                self.assertTrue((artifacts / "ci-diagnostics/outcomes.json").is_file())
                self.assertFalse((artifacts / "test-results/trace.zip").exists())
                for path in artifacts.rglob("*"):
                    if path.is_file():
                        self.assertNotIn(b"csk_", path.read_bytes())

    def test_gate_selectors_match_actual_job_conditions(self):
        for name, selectors in SELECTORS.items():
            with self.subTest(job=name):
                condition = JOBS[name].get("if", "")
                actual = re.findall(r"needs.changes.outputs.(\w+) == 'true'", condition)
                self.assertEqual(set(actual), set(selectors))
                if selectors:
                    self.assertEqual(condition, " || ".join(
                        f"needs.changes.outputs.{key} == 'true'" for key in selectors
                    ))
        gate = JOBS["ci-ok"]
        command = gate["steps"][-1]["run"].replace("\\\n", " ").split()
        self.assertEqual(command[:2], ["python3", "scripts/ci-ok.py"])
        self.assertEqual(command[2:], gate["needs"])
        for name in gate["needs"]:
            self.assertTrue(set(JOBS[name].get("needs", [])) <= set(gate["needs"]))

    def test_required_legacy_checks_always_report(self):
        for name in ("build-and-integration-test", "build-and-integration-test-agent-auth"):
            self.assertIn(name, JOBS)
            self.assertEqual(JOBS[name]["if"], "always()")
            self.assertEqual(JOBS[name]["needs"], ["changes", "images", "integration"])

    def test_full_suite_shards_cover_all_indexes(self):
        for name in ("integration", "integration-arm64"):
            job = JOBS[name]
            self.assertFalse(job["strategy"]["fail-fast"])
            entries = job["strategy"]["matrix"]["include"]
            shards = [entry for entry in entries if entry["id"] == "docker"]
            self.assertEqual(sorted(entry["shard"] for entry in shards), [1, 2, 3])
            self.assertTrue(all(entry["recipe"] == "integration-test" for entry in shards))
            inputs = job["steps"][-1]["with"]
            self.assertEqual(inputs["shard-index"], "${{ matrix.shard }}")
            self.assertEqual(inputs["shard-count"], "${{ matrix.id == 'docker' && '3' || '' }}")
        for name in ("helm-integration-test", "podman-integration-test"):
            job = JOBS[name]
            self.assertFalse(job["strategy"]["fail-fast"])
            self.assertEqual(job["strategy"]["matrix"]["shard"], [1, 2, 3])
            runs = [step["run"] for step in job["steps"] if "sh scripts/integration-test.sh" in step.get("run", "")]
            self.assertEqual(len(runs), 1)
            self.assertNotIn("-run", runs[0])
            self.assertIn("caesiumcloud/caesium-integration:", runs[0])
            self.assertIn("CAESIUM_TEST_SHARD_INDEX=${{ matrix.shard }}", runs[0])
            self.assertIn("CAESIUM_TEST_SHARD_COUNT=3", runs[0])

    def test_fixture_and_build_paths_select_tests(self):
        filters = yaml.safe_load(next(
            step["with"]["filters"] for step in JOBS["changes"]["steps"] if step.get("id") == "filter"
        ))
        # These filters use only literals and ** globs, for which fnmatch also
        # recognizes the concrete (non-root) fixture paths below.
        for path, group in (
            ("test/definitions/job_one.yaml", "go"),
            ("pkg/jobdef/testdata/schema.json", "go"),
            ("docs/examples/minimal.job.yaml", "go"),
            ("docs/examples-k8s/minimal.job.yaml", "go"),
            (".dockerignore", "go"),
            ("reagents/go.mod", "reagents"),
            ("scripts/test_ci.py", "ci"),
        ):
            self.assertTrue(any(fnmatch.fnmatchcase(path, rule) for rule in filters[group]), path)
        for path in ("docs/ci.md", "README.md"):
            self.assertFalse(any(fnmatch.fnmatchcase(path, rule) for rules in filters.values() for rule in rules))

    def test_downloaded_artifacts_have_producers(self):
        saved = set()
        downloaded = set()
        for job in JOBS.values():
            for step in job.get("steps", []):
                if step.get("uses") == "./.github/actions/save-docker-images":
                    saved.add(step["with"]["name"])
                if step.get("uses") == "./.github/actions/load-docker-images":
                    downloaded.add(step["with"]["name"])
        self.assertTrue(downloaded <= saved, downloaded - saved)
        for arch in ("amd64", "arm64"):
            self.assertIn(f"product-{arch}", saved)
            self.assertIn(f"builder-{arch}", saved)
            self.assertIn(f"reagents-{arch}", saved)
            self.assertIn(f"integration-runner-{arch}", saved)

    def test_integration_consumers_do_not_download_compilers(self):
        action = yaml.safe_load((ROOT / ".github/actions/run-integration/action.yml").read_text())
        steps = action["runs"]["steps"]
        self.assertIn("CAESIUM_INTEGRATION_RUNNER_IMAGE", steps[-1]["env"])
        groups = [steps, JOBS["helm-integration-test"]["steps"], JOBS["podman-integration-test"]["steps"]]
        for steps in groups:
            loads = [step["with"]["name"] for step in steps if step.get("uses") == "./.github/actions/load-docker-images"]
            self.assertTrue(any(name.startswith("integration-runner-") for name in loads))
            self.assertFalse(any(name.startswith("builder-") for name in loads))


def change_filters():
    return yaml.safe_load(next(
        step["with"]["filters"] for step in JOBS["changes"]["steps"] if step.get("id") == "filter"
    ))


def selected_groups(path):
    return {group for group, rules in change_filters().items()
            if any(fnmatch.fnmatchcase(path, rule) for rule in rules)}


def job_selectors(name):
    return set(re.findall(r"needs\.changes\.outputs\.(\w+) == 'true'", JOBS[name].get("if", "")))


class EarlyEvidenceLaneTests(unittest.TestCase):
    """The first persistent three-node lane: selection, wiring and fail-closed."""

    LANE = "early-evidence"
    MANIFEST = json.loads((ROOT / "test/contracts/scenarios.json").read_text())
    EARLY = [item for item in MANIFEST["scenarios"] if "early" in item["gates"]]

    def test_lane_selects_code_fixture_chart_dependency_build_and_workflow_changes(self):
        selectors = job_selectors(self.LANE)
        self.assertEqual(selectors, {"go", "helm", "ci"})
        for path, reason in (
            ("internal/run/store.go", "code"),
            ("test/robustness/owner_crash_test.go", "lane test code"),
            ("test/contracts/scenarios.json", "scenario manifest"),
            ("test/definitions/job_one.yaml", "fixtures"),
            ("pkg/jobdef/testdata/schema.json", "fixtures"),
            ("helm/caesium/ci/test-values-robustness.yaml", "chart values"),
            ("helm/caesium/templates/statefulset.yaml", "chart"),
            ("go.mod", "dependency"),
            ("go.sum", "dependency"),
            ("build/Dockerfile.robustness", "image"),
            ("build/ci.docker-bake.hcl", "build graph"),
            ("justfile", "recipe"),
            ("scripts/robustness.sh", "lane runner"),
            ("scripts/collect-evidence.py", "artifact consumer"),
            (".github/workflows/ci.yml", "workflow"),
            (".github/actions/run-integration/action.yml", "workflow"),
        ):
            with self.subTest(path=path, reason=reason):
                self.assertTrue(selected_groups(path) & selectors, path)
        # Documentation-only changes must not spend a kind cluster.
        for path in ("docs/ci.md", "README.md", "docs/exec-plans/active/distributed-testing.md"):
            with self.subTest(path=path):
                self.assertFalse(selected_groups(path) & selectors, path)

    def test_lane_is_not_promoted_into_the_merge_gate(self):
        self.assertIn(self.LANE, JOBS)
        self.assertNotIn(self.LANE, JOBS["ci-ok"]["needs"])
        self.assertNotIn(self.LANE, SELECTORS)
        for name in ("build-and-integration-test", "build-and-integration-test-agent-auth"):
            self.assertNotIn(self.LANE, JOBS[name]["needs"])

    def test_required_check_jobs_are_preserved(self):
        for name in ("lint", "unit-test", "unit-test-arm64", "ui-test", "ui-e2e",
                     "ui-e2e-auth", "build-and-integration-test",
                     "build-and-integration-test-agent-auth"):
            self.assertIn(name, JOBS, name)

    def test_existing_engine_and_architecture_suites_are_preserved(self):
        recipes = {entry["recipe"] for name in ("integration", "integration-extra", "integration-arm64")
                   for entry in JOBS[name]["strategy"]["matrix"]["include"]}
        self.assertTrue({
            "integration-test",
            "integration-test-agent",
            "integration-test-distributed",
            "integration-test-owner-memory",
            "integration-test-infra",
        } <= recipes, recipes)
        for name in ("helm-integration-test", "podman-integration-test"):
            self.assertEqual(JOBS[name]["strategy"]["matrix"]["shard"], [1, 2, 3])

    def test_runner_image_compiles_the_subpackage_with_integration_tags(self):
        dockerfile = (ROOT / "build/Dockerfile.robustness").read_text()
        self.assertIn("go test -tags=integration -c ./test/robustness", dockerfile)
        bake = (ROOT / "build/ci.docker-bake.hcl").read_text()
        self.assertIn('dockerfile = "build/Dockerfile.robustness"', bake)
        self.assertIn('caesium-release = "target:release"', bake)
        images = JOBS["images"]["steps"]
        bake_step = next(step for step in images if step.get("uses") == "./.github/actions/bake-images")
        self.assertIn("robustness", bake_step["with"]["targets"].split())
        saved = [step["with"]["name"] for step in images
                 if step.get("uses") == "./.github/actions/save-docker-images"]
        self.assertIn("robustness-amd64", saved)

    def test_lane_consumes_prebuilt_images_and_never_a_compiler(self):
        action = yaml.safe_load((ROOT / ".github/actions/run-integration/action.yml").read_text())
        loads = [step["with"]["name"] for step in action["runs"]["steps"]
                 if step.get("uses") == "./.github/actions/load-docker-images"]
        self.assertIn("robustness-${{ inputs.arch }}", loads)
        steps = JOBS[self.LANE]["steps"]
        runner = next(step for step in steps if step.get("uses") == "./.github/actions/run-integration")
        self.assertEqual(runner["with"]["load-robustness"], "true")
        self.assertEqual(runner["with"]["recipe"], "integration-test-sql-budget")
        self.assertEqual(JOBS[self.LANE]["env"]["CAESIUM_SKIP_IMAGE_BUILD"], "true")
        # A `-amd64` image tag can never stand in for a real commit SHA.
        self.assertEqual(JOBS[self.LANE]["env"]["CANDIDATE_SHA"], "${{ github.sha }}")
        self.assertFalse([step for step in steps
                          if step.get("uses") == "./.github/actions/load-docker-images"
                          and step["with"]["name"].startswith("builder-")])

    def test_lane_runs_both_scenarios_then_validates_and_uploads_evidence(self):
        steps = JOBS[self.LANE]["steps"]
        recipes = [step["run"].split()[-1] for step in steps
                   if step.get("run", "").startswith("just ")]
        self.assertEqual(recipes, ["robustness-test", "check-evidence"])
        self.assertTrue(any(step.get("uses", "").startswith("azure/setup-helm") for step in steps))
        self.assertTrue(any("kind-linux-amd64" in step.get("run", "") for step in steps))
        upload = steps[-1]
        self.assertEqual(upload["uses"], "actions/upload-artifact@v7")
        self.assertEqual(upload["if"], "always()")
        self.assertEqual(upload["with"]["if-no-files-found"], "error")

    def test_registered_selectors_name_real_tests(self):
        by_id = {item["id"]: item for item in self.EARLY}
        self.assertEqual(sorted(by_id), [
            "b1-owner-crash-leader", "b1-owner-crash-nonleader", "e5-sql-work-budget",
        ])
        owner_crash = (ROOT / "test/robustness/owner_crash_test.go").read_text()
        runner = (ROOT / "scripts/robustness.sh").read_text()
        self.assertIn('"-test.run", "^TestOwnerCrash$"', runner)
        for sid in ("b1-owner-crash-leader", "b1-owner-crash-nonleader"):
            subtest = by_id[sid]["selector"].split("/", 1)[1]
            self.assertIn(f't.Run("{subtest}"', owner_crash)
            self.assertIn(f"pass_line 'TestOwnerCrash/{subtest}'", runner)
        # Bare method names match nothing: the suite prefix is load-bearing.
        selector = by_id["e5-sql-work-budget"]["selector"]
        self.assertTrue(selector.startswith("TestIntegrationTestSuite/"), selector)
        method = selector.split("/", 1)[1]
        self.assertIn(f") {method}()", (ROOT / "test/statement_budget_test.go").read_text())
        justfile = (ROOT / "justfile").read_text()
        self.assertIn(f'env("CAESIUM_SQL_BUDGET_RUN", "{selector}")', justfile)

    def test_evidence_check_fails_on_a_missing_artifact_or_scenario(self):
        recipe = re.search(r"\ncheck-evidence:\n(.*?)\n\n", (ROOT / "justfile").read_text(), re.S)
        self.assertIsNotNone(recipe)
        body = recipe.group(1)
        self.assertIn("scripts/collect-evidence.py report", body)
        self.assertIn("--require early", body)
        self.assertIn("--strict", body)
        manifest = ROOT / "test/contracts/scenarios.json"
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            report = root / "evidence.json"
            # 1. A lane that never wrote its fragment fails the merge step.
            missing = subprocess.run(
                [sys.executable, str(ROOT / "scripts/collect-evidence.py"), "report",
                 "--out", str(report), str(root / "sql-budget.json")],
                capture_output=True, text=True,
            )
            self.assertEqual(missing.returncode, 1, missing.stdout + missing.stderr)
            self.assertIn("missing evidence fragment", missing.stderr)
            # 2. A report that omits a registered scenario fails the checker.
            report.write_text(json.dumps({
                "candidate_sha": "a" * 40,
                "candidate_digest": "sha256:" + "ab" * 32,
                "gate_enabled": True,
                "disabled_gates": [],
                "scenarios": [],
            }))
            checked = subprocess.run(
                [sys.executable, str(ROOT / "scripts/check-test-evidence.py"),
                 "--manifest", str(manifest), "--report", str(report),
                 "--require", "early", "--strict"],
                capture_output=True, text=True,
            )
            self.assertEqual(checked.returncode, 1, checked.stdout + checked.stderr)
            for scenario in self.EARLY:
                self.assertIn(f"missing-scenario={scenario['id']}",
                              checked.stdout + checked.stderr)


class IntegrationRunnerTests(unittest.TestCase):
    def test_binary_receives_package_cwd_filters_and_exit_status(self):
        with tempfile.TemporaryDirectory() as tmp:
            binary = Path(tmp) / "fake-test-binary"
            binary.write_text('#!/bin/sh\npwd\nprintf "%s\\n" "$@"\nexit 17\n')
            binary.chmod(0o755)
            result = subprocess.run(
                ["sh", str(ROOT / "scripts/integration-test.sh"), "-test.run", "Suite/(TestA|TestB)", "-test.timeout=20m"],
                cwd=tmp,
                env={**os.environ, "CAESIUM_INTEGRATION_TEST_BINARY": str(binary)},
                capture_output=True, text=True,
            )
            self.assertEqual(result.returncode, 17)
            self.assertEqual(result.stdout.splitlines(), [
                str(ROOT / "test"), "-test.count=1", "-test.timeout=30m", "-test.v",
                "-test.run", "Suite/(TestA|TestB)", "-test.timeout=20m",
            ])

    def test_missing_binary_fails_without_compiling(self):
        result = subprocess.run(
            ["sh", str(ROOT / "scripts/integration-test.sh")],
            env={**os.environ, "CAESIUM_INTEGRATION_TEST_BINARY": "/missing/precompiled-test"},
            capture_output=True, text=True,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Precompiled integration test binary is missing", result.stderr)


if __name__ == "__main__":
    unittest.main()
