#!/usr/bin/env python3
"""Keep coverage's native stress fixture independent of product-job artifacts."""

import copy
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[1]
STRESS_IMAGE = "caesiumcloud/resource-stress:${{ env.IMAGE_TAG }}-amd64"
BUILD_COMMAND = "just tag=${{ env.IMAGE_TAG }}-amd64 build-stress"
SMOKE_LINES = [
    "set -euo pipefail",
    'date +%s >"$RUNNER_TEMP/stress-smoke.since"',
    f'bash build/stress/smoke.sh docker {STRESS_IMAGE} 2>&1 | tee "$RUNNER_TEMP/stress-smoke.log"',
]
COLLECTION_COMMAND = "just tag=${{ env.IMAGE_TAG }}-amd64 coverage-ratchets"


class CoverageStressPrerequisiteTests(unittest.TestCase):
    def setUp(self):
        workflow = yaml.safe_load((ROOT / ".github/workflows/ci.yml").read_text())
        self.job = workflow["jobs"]["coverage-ratchets"]

    def assertPrerequisite(self, job):
        self.assertEqual(job["needs"], ["changes", "builder"])
        self.assertEqual(job["runs-on"], "ubuntu-26.04")
        self.assertEqual(job["env"]["CAESIUM_SKIP_IMAGE_BUILD"], "true")
        self.assertNotIn("CAESIUM_RESOURCE_STRESS_IMAGE", job["env"])
        self.assertFalse(job.get("continue-on-error", False))
        steps = job["steps"]
        build = [i for i, step in enumerate(steps) if "build-stress" in step.get("run", "")]
        smoke = [i for i, step in enumerate(steps) if "build/stress/smoke.sh" in step.get("run", "")]
        collect = [i for i, step in enumerate(steps) if step.get("run") == COLLECTION_COMMAND]
        self.assertEqual(len(build), 1)
        self.assertEqual(len(smoke), 1)
        self.assertEqual(len(collect), 1)
        self.assertLess(build[0], smoke[0])
        self.assertLess(smoke[0], collect[0])
        prerequisite = steps[build[0]]
        self.assertEqual(prerequisite["run"], BUILD_COMMAND)
        self.assertEqual(prerequisite["env"]["CAESIUM_SKIP_IMAGE_BUILD"], "false")
        for index in (build[0], smoke[0], collect[0]):
            self.assertNotIn("if", steps[index])
            self.assertFalse(steps[index].get("continue-on-error", False))
        self.assertEqual(steps[smoke[0]].get("id"), "native_stress")
        self.assertEqual(steps[smoke[0]]["run"].splitlines(), SMOKE_LINES)
        self.assertNotIn("stress-image-test", "\n".join(step.get("run", "") for step in steps))
        collector = steps[collect[0]]
        self.assertEqual(collector.get("env", {}).get("CAESIUM_SKIP_IMAGE_BUILD", "true"), "true")
        self.assertEqual(collector.get("env", {}).get("CAESIUM_RESOURCE_STRESS_IMAGE"), STRESS_IMAGE)

    def test_coverage_builds_smokes_and_selects_exact_fixture_before_collection(self):
        self.assertPrerequisite(self.job)

    def test_missing_or_bypassed_prerequisite_is_refused(self):
        self.assertPrerequisite(self.job)
        for fault in ("latest", "missing-selection", "job-env-selection", "wrong-tag", "skip-build", "missing-build",
                      "late-build", "optional-build", "ignored-failure",
                      "collector-override", "product-dependency", "missing-smoke", "late-smoke", "early-smoke",
                      "duplicate-build", "duplicate-smoke", "optional-smoke", "ignored-smoke", "wrong-smoke-tag",
                      "smoke-without-pipefail", "smoke-with-swallowed-failure", "smoke-retry", "optional-collection",
                      "ignored-collection"):
            with self.subTest(fault=fault):
                job = copy.deepcopy(self.job)
                steps = job["steps"]
                build = next(step for step in steps if step.get("run") == BUILD_COMMAND)
                smoke = next(step for step in steps if step.get("id") == "native_stress")
                collect = next(step for step in steps if step.get("run") == COLLECTION_COMMAND)
                if fault == "latest":
                    collect["env"]["CAESIUM_RESOURCE_STRESS_IMAGE"] = "caesiumcloud/resource-stress:latest"
                elif fault == "missing-selection":
                    del collect["env"]["CAESIUM_RESOURCE_STRESS_IMAGE"]
                elif fault == "job-env-selection":
                    job["env"]["CAESIUM_RESOURCE_STRESS_IMAGE"] = collect["env"].pop("CAESIUM_RESOURCE_STRESS_IMAGE")
                elif fault == "wrong-tag":
                    build["run"] = BUILD_COMMAND.replace("-amd64", "-arm64")
                elif fault == "skip-build":
                    build["env"]["CAESIUM_SKIP_IMAGE_BUILD"] = "true"
                elif fault == "missing-build":
                    steps.remove(build)
                elif fault == "late-build":
                    steps.remove(build)
                    steps.insert(steps.index(collect) + 1, build)
                elif fault == "optional-build":
                    build["if"] = "false"
                elif fault == "ignored-failure":
                    build["continue-on-error"] = True
                elif fault == "collector-override":
                    collect["env"] = {"CAESIUM_RESOURCE_STRESS_IMAGE": "foreign:latest"}
                elif fault == "product-dependency":
                    job["needs"].append("images")
                elif fault == "missing-smoke":
                    steps.remove(smoke)
                elif fault == "late-smoke":
                    steps.remove(smoke)
                    steps.insert(steps.index(collect) + 1, smoke)
                elif fault == "early-smoke":
                    steps.remove(smoke)
                    steps.insert(steps.index(build), smoke)
                elif fault == "duplicate-build":
                    steps.insert(steps.index(build), copy.deepcopy(build))
                elif fault == "duplicate-smoke":
                    steps.insert(steps.index(smoke), copy.deepcopy(smoke))
                elif fault == "optional-smoke":
                    smoke["if"] = "false"
                elif fault == "ignored-smoke":
                    smoke["continue-on-error"] = True
                elif fault == "wrong-smoke-tag":
                    smoke["run"] = smoke["run"].replace(STRESS_IMAGE, "caesiumcloud/resource-stress:latest")
                elif fault == "smoke-without-pipefail":
                    smoke["run"] = smoke["run"].replace("set -euo pipefail", "set -eu")
                elif fault == "smoke-with-swallowed-failure":
                    smoke["run"] += " || true"
                elif fault == "smoke-retry":
                    smoke["run"] += "\n" + SMOKE_LINES[-1]
                elif fault == "optional-collection":
                    collect["if"] = "always()"
                elif fault == "ignored-collection":
                    collect["continue-on-error"] = True
                with self.assertRaises(AssertionError):
                    self.assertPrerequisite(job)

    def test_actual_split_shell_builds_once_and_smoke_failure_blocks_collection(self):
        steps = self.job["steps"]
        selected = [next(step for step in steps if step.get("run") == BUILD_COMMAND),
                    next(step for step in steps if step.get("id") == "native_stress"),
                    next(step for step in steps if step.get("run") == COLLECTION_COMMAND)]
        for exit_code in (0, 17):
            with self.subTest(exit_code=exit_code), tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary)
                calls = directory / "calls"
                just = directory / "just"
                just.write_text('#!/bin/sh\ncase "$*" in\n'
                                '  *build-stress) printf "build|%s|%s\\n" "$*" "$CAESIUM_SKIP_IMAGE_BUILD" >>"$SMOKE_CALLS"; '
                                '[ "$CAESIUM_SKIP_IMAGE_BUILD" = false ] || exit 93;;\n'
                                '  *coverage-ratchets) printf "collect|%s\\n" "$*" >>"$SMOKE_CALLS";;\n'
                                '  *) exit 94;;\nesac\n')
                just.chmod(0o755)
                smoke = directory / "build/stress/smoke.sh"
                smoke.parent.mkdir(parents=True)
                smoke.write_text('printf "smoke|%s\\n" "$*" >>"$SMOKE_CALLS"\n'
                                 'printf "observed fixture\\n"\nexit "$SMOKE_EXIT"\n')
                environment = {**os.environ, **self.job["env"], "RUNNER_TEMP": temporary,
                               "PATH": temporary + os.pathsep + os.environ["PATH"],
                               "SMOKE_CALLS": str(calls), "SMOKE_EXIT": str(exit_code)}
                for step in selected:
                    result = subprocess.run(["bash", "-c", step["run"].replace("${{ env.IMAGE_TAG }}", "harness-candidate")],
                                            cwd=directory, env={**environment, **step.get("env", {})},
                                            capture_output=True, text=True, timeout=5)
                    if result.returncode:
                        break
                self.assertEqual(result.returncode, exit_code, result.stdout + result.stderr)
                expected = ["build|tag=harness-candidate-amd64 build-stress|false",
                            "smoke|docker caesiumcloud/resource-stress:harness-candidate-amd64"]
                if exit_code == 0:
                    expected.append("collect|tag=harness-candidate-amd64 coverage-ratchets")
                self.assertEqual(calls.read_text().splitlines(), expected)
                self.assertEqual((directory / "stress-smoke.log").read_text(), "observed fixture\n")


if __name__ == "__main__":
    unittest.main()
