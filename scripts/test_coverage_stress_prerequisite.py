#!/usr/bin/env python3
"""Keep coverage's native stress fixture independent of product-job artifacts."""

import copy
from pathlib import Path
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[1]
STRESS_IMAGE = "caesiumcloud/resource-stress:${{ env.IMAGE_TAG }}-amd64"
STRESS_COMMAND = "just tag=${{ env.IMAGE_TAG }}-amd64 stress-image-test"
COLLECTION_COMMAND = "just tag=${{ env.IMAGE_TAG }}-amd64 coverage-ratchets"


class CoverageStressPrerequisiteTests(unittest.TestCase):
    def setUp(self):
        workflow = yaml.safe_load((ROOT / ".github/workflows/ci.yml").read_text())
        self.job = workflow["jobs"]["coverage-ratchets"]

    def assertPrerequisite(self, job):
        self.assertEqual(job["needs"], ["changes", "builder"])
        self.assertEqual(job["runs-on"], "ubuntu-24.04")
        self.assertEqual(job["env"]["CAESIUM_SKIP_IMAGE_BUILD"], "true")
        self.assertNotIn("CAESIUM_RESOURCE_STRESS_IMAGE", job["env"])
        steps = job["steps"]
        build = [i for i, step in enumerate(steps) if step.get("run") == STRESS_COMMAND]
        collect = [i for i, step in enumerate(steps) if step.get("run") == COLLECTION_COMMAND]
        self.assertEqual(len(build), 1)
        self.assertEqual(len(collect), 1)
        self.assertLess(build[0], collect[0])
        prerequisite = steps[build[0]]
        self.assertEqual(prerequisite["env"]["CAESIUM_SKIP_IMAGE_BUILD"], "false")
        self.assertNotIn("if", prerequisite)
        self.assertFalse(prerequisite.get("continue-on-error", False))
        collector = steps[collect[0]]
        self.assertEqual(collector.get("env", {}).get("CAESIUM_SKIP_IMAGE_BUILD", "true"), "true")
        self.assertEqual(collector.get("env", {}).get("CAESIUM_RESOURCE_STRESS_IMAGE"), STRESS_IMAGE)

    def test_coverage_builds_smokes_and_selects_exact_fixture_before_collection(self):
        self.assertPrerequisite(self.job)

    def test_missing_or_bypassed_prerequisite_is_refused(self):
        for fault in ("latest", "missing-selection", "job-env-selection", "wrong-tag", "skip-build", "missing-build",
                      "late-build", "optional-build", "ignored-failure",
                      "collector-override", "product-dependency"):
            with self.subTest(fault=fault):
                job = copy.deepcopy(self.job)
                steps = job["steps"]
                build = next(step for step in steps if step.get("run") == STRESS_COMMAND)
                collect = next(step for step in steps if step.get("run") == COLLECTION_COMMAND)
                if fault == "latest":
                    collect["env"]["CAESIUM_RESOURCE_STRESS_IMAGE"] = "caesiumcloud/resource-stress:latest"
                elif fault == "missing-selection":
                    del collect["env"]["CAESIUM_RESOURCE_STRESS_IMAGE"]
                elif fault == "job-env-selection":
                    job["env"]["CAESIUM_RESOURCE_STRESS_IMAGE"] = collect["env"].pop("CAESIUM_RESOURCE_STRESS_IMAGE")
                elif fault == "wrong-tag":
                    build["run"] = STRESS_COMMAND.replace("-amd64", "-arm64")
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
                with self.assertRaises(AssertionError):
                    self.assertPrerequisite(job)


if __name__ == "__main__":
    unittest.main()
