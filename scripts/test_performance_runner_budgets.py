"""Per-runner E4 calibrations (distributed-testing G6 / W9-alpha).

E4's budgets and fixed baseline mean something only on the runner they were
calibrated on (budgets.fixed_baseline.must_match). The local developer runner
keeps test/performance/budgets.json + baseline.json; every other runner gets
its own pair, test/performance/budgets-<runner>.json + baseline-<runner>.json,
selected with CAESIUM_PERF_BUDGETS / CAESIUM_PERF_BASELINE. These checks keep
each pair honest: reviewed (the comparator's own validation), bound to one
runner identity, recorded under the calibrated settings, carrying the same
Q5-approved rule and series set as the local file (only numeric values and the
recorded baseline may differ), with an append-only `changes` log -- and the
workflow must point its runner at exactly that runner's pair.
"""

import json
from pathlib import Path
import runpy
import subprocess
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[1]
PERF = ROOT / "test/performance"
COMPARE = runpy.run_path(str(ROOT / "scripts/compare-performance.py"))
LOCAL_BUDGETS = PERF / "budgets.json"
# The performance lane is defined once in the reusable qualification workflow
# (distributed-testing G4), which CI and the nightly schedule both call.
WORKFLOW = yaml.safe_load((ROOT / ".github/workflows/qualification-lanes.yml").read_text())
JOB = WORKFLOW["jobs"]["performance-gate"]


def runner_pairs():
    """(budgets path, baseline path) for every per-runner calibration."""
    pairs = []
    for budgets in sorted(PERF.glob("budgets-*.json")):
        runner = budgets.name[len("budgets-"):-len(".json")]
        pairs.append((budgets, PERF / f"baseline-{runner}.json"))
    return pairs


def rule_shape(budgets):
    return {
        family: [rule["match"] for rule in body["rules"]]
        for family, body in budgets["families"].items()
    }


class RunnerCalibrationTests(unittest.TestCase):
    def test_every_runner_pair_is_reviewed_bound_and_same_rule(self):
        local = COMPARE["load_budgets"](LOCAL_BUDGETS)
        for budgets_path, baseline_path in runner_pairs():
            with self.subTest(budgets=budgets_path.name):
                budgets = COMPARE["load_budgets"](budgets_path)
                self.assertTrue(baseline_path.exists(), f"{baseline_path.name} is missing")
                baseline = json.loads(baseline_path.read_text())
                host_id = budgets["calibration"]["runner"]["host_id"]
                self.assertEqual(baseline["provenance"]["host_id"], host_id)
                self.assertEqual(budgets["fixed_baseline"]["path"],
                                 str(baseline_path.relative_to(ROOT)))
                # Q5: the decision rule, SLO set, bundle budgets and rerun
                # policy are the approved ones; only numeric margins, SLO
                # ceilings and the recorded baseline are per-runner.
                self.assertEqual(budgets["decision"], local["decision"])
                self.assertEqual(budgets["rerun_policy"], local["rerun_policy"])
                self.assertEqual(budgets["bundle"], local["bundle"])
                self.assertEqual(budgets["fixed_baseline"]["must_match"], local["fixed_baseline"]["must_match"])
                self.assertEqual(rule_shape(budgets), rule_shape(local))
                self.assertEqual([slo["id"] for slo in budgets["slos"]], [slo["id"] for slo in local["slos"]])
                # The baseline was recorded under the calibrated settings and
                # passed its target-base verdict under these budgets.
                self.assertEqual(baseline["report"]["budgets_values_sha256"],
                                 budgets["changes"][-1]["values_sha256"])
                self.assertIn(baseline["report"]["target_base"], COMPARE["PASS_VERDICTS"])
                runs = budgets["calibration"]["runs"]
                self.assertGreaterEqual(len([run for run in runs if run.get("control") == "a_a"]), 3)
                rates = budgets["calibration"]["control_rates"]
                self.assertEqual(rates["false_fail"], 0)
                self.assertEqual(rates["inconclusive"], 0)

    def test_the_local_runner_calibration_is_untouched_by_runner_pairs(self):
        local = json.loads(LOCAL_BUDGETS.read_text())
        self.assertEqual(local["calibration"]["runner"]["host_id"], "caesium-perf-m5max-dd|Darwin|arm64")
        self.assertEqual(local["fixed_baseline"]["path"], "test/performance/baseline.json")

    def test_runner_changes_logs_are_append_only_in_git_history(self):
        for budgets_path, _ in runner_pairs():
            rel = str(budgets_path.relative_to(ROOT))
            log = subprocess.run(["git", "-C", str(ROOT), "log", "--format=%H", "--", rel],
                                 capture_output=True, text=True)
            if log.returncode != 0:
                self.skipTest("git history unavailable")
            versions = []
            for sha in reversed([line for line in log.stdout.split() if line]):
                shown = subprocess.run(["git", "-C", str(ROOT), "show", f"{sha}:{rel}"],
                                       capture_output=True, text=True)
                if shown.returncode == 0:
                    versions.append(json.loads(shown.stdout))
            versions.append(json.loads(budgets_path.read_text()))
            for older, newer in zip(versions, versions[1:]):
                self.assertEqual(newer["changes"][:len(older["changes"])], older["changes"], rel)
                if COMPARE["budget_values_sha256"](older) != COMPARE["budget_values_sha256"](newer):
                    self.assertGreater(len(newer["changes"]), len(older["changes"]), rel)

    def test_the_workflow_runner_uses_its_own_calibration(self):
        env = JOB["env"]
        host_id = env["CAESIUM_PERF_HOST_ID"]
        budgets_ref = env.get("CAESIUM_PERF_BUDGETS")
        if budgets_ref is None:
            # Uncalibrated: the lane may only run advisory (the default mode).
            self.assertNotIn(host_id, {json.loads(p.read_text())["calibration"]["runner"]["host_id"]
                                       for p, _ in runner_pairs()})
            return
        budgets = COMPARE["load_budgets"](ROOT / budgets_ref)
        self.assertEqual(budgets["calibration"]["runner"]["host_id"], host_id)
        self.assertEqual(env["CAESIUM_PERF_BASELINE"], budgets["fixed_baseline"]["path"])


if __name__ == "__main__":
    unittest.main()
