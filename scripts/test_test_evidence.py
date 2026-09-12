"""Regression checks for the scenario evidence manifest and validator."""

import json
from pathlib import Path
import runpy
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts/check-test-evidence.py"
MANIFEST_PATH = ROOT / "test/contracts/scenarios.json"
CHECKER = runpy.run_path(str(SCRIPT))
CONTRACTS = CHECKER["REQUIRED_CONTRACT_IDS"]

SHA = "a" * 40
DIGEST = "sha256:" + "ab" * 32


def scenario(contract_id, **overrides):
    sid = f"s-{contract_id.lower()}"
    base = {
        "id": sid,
        "name": f"Scenario {contract_id}",
        "selector": f"Test{contract_id.replace('-', '_')}",
        "contract_ids": [contract_id],
        "status": "unproven",
        "owner_item": "T",
        "gates": ["early"],
        "topology": {
            "kind": "kind-helm-statefulset",
            "replicas": 3,
            "persistence": True,
            "engine": "kubernetes",
        },
        "mode": {"execution": "distributed", "owner": "enabled", "surface": "http"},
        "feature_flags": {
            "CAESIUM_EXECUTION_MODE": "distributed",
            "CAESIUM_RUN_OWNER_ENABLED": "true",
        },
        "expected_observations": [{"id": "obs_a", "description": "observed A"}],
        "allowed_skips": [],
        "evidence": {
            "require_candidate_sha": True,
            "require_candidate_digest": True,
            "require_recorder": True,
            "artifact_identity": f"art-{contract_id.lower()}",
            "min_samples": 1,
        },
        "fault_activation": {
            "required": True,
            "kind": "owner-sigkill",
            "required_observations": ["killed"],
        },
    }
    base.update(overrides)
    return base


def fixture_manifest(scenarios=None, **overrides):
    doc = {
        "schema_version": 1,
        "required_contract_ids": list(CONTRACTS),
        "scenarios": scenarios if scenarios is not None else [scenario(cid) for cid in CONTRACTS],
    }
    doc.update(overrides)
    return doc


def passing_result(sc, **overrides):
    result = {
        "id": sc["id"],
        "status": "pass",
        "artifact_identity": sc["evidence"]["artifact_identity"],
        "candidate_sha": SHA,
        "candidate_digest": DIGEST,
        "topology": dict(sc.get("topology") or {}),
        "mode": dict(sc.get("mode") or {}),
        "feature_flags": dict(sc.get("feature_flags") or {}),
        "observations": [item["id"] for item in sc["expected_observations"]],
        "recorder": {"present": True, "sample_count": max(sc["evidence"]["min_samples"], 1)},
        "checker": {"timed_out": False},
    }
    if sc["fault_activation"].get("required"):
        result["fault_activation"] = {
            "activated": True,
            "kind": sc["fault_activation"]["kind"],
            "observations": list(sc["fault_activation"]["required_observations"]),
        }
    result.update(overrides)
    return result


def passing_report(manifest, results=None, **overrides):
    report = {
        "candidate_sha": SHA,
        "candidate_digest": DIGEST,
        "gate_enabled": True,
        "disabled_gates": [],
        "scenarios": results if results is not None else [passing_result(sc) for sc in manifest["scenarios"]],
    }
    report.update(overrides)
    return report


def run_checker(manifest, report, extra=()):
    with tempfile.TemporaryDirectory() as tmp:
        man = Path(tmp) / "manifest.json"
        rep = Path(tmp) / "report.json"
        man.write_text(json.dumps(manifest) if not isinstance(manifest, str) else manifest)
        rep.write_text(json.dumps(report) if not isinstance(report, str) else report)
        return subprocess.run(
            [sys.executable, str(SCRIPT), "--manifest", str(man), "--report", str(rep), *extra],
            capture_output=True,
            text=True,
        )


def output(result):
    return result.stdout + result.stderr


class CommittedManifestTests(unittest.TestCase):
    def test_committed_manifest_is_schema_valid_and_unproven(self):
        doc = json.loads(MANIFEST_PATH.read_text())
        issues = CHECKER["validate_manifest"](doc)
        self.assertEqual([issue.message for issue in issues], [])
        self.assertEqual(doc["required_contract_ids"], list(CONTRACTS))
        ids = [item["id"] for item in doc["scenarios"]]
        self.assertEqual(ids, sorted(set(ids), key=ids.index))
        covered = set()
        owners = set()
        for item in doc["scenarios"]:
            self.assertIn(item["status"], ("absent", "unproven"))
            self.assertNotIn(item["status"], ("proven", "pass", "passing", "passed"))
            self.assertEqual(item["gates"], [])
            covered.update(item["contract_ids"])
            owners.add(item["owner_item"])
            if item["owner_item"] in {"B1", "B2", "B3", "D3", "E5"}:
                self.assertEqual(item["status"], "absent", item["id"])
        self.assertEqual(covered, set(CONTRACTS))
        self.assertTrue({"B1", "E5"} <= owners)
        by_id = {item["id"]: item for item in doc["scenarios"]}
        self.assertEqual(by_id["b1-owner-crash-leader"]["selector"], "TestOwnerCrash/owner_is_leader")
        self.assertEqual(by_id["b1-owner-crash-nonleader"]["selector"], "TestOwnerCrash/owner_is_not_leader")
        self.assertEqual(by_id["e5-sql-work-budget"]["category"], "sql-work-budget")
        self.assertEqual(by_id["e5-sql-work-budget"]["contract_ids"], [])

    def test_committed_absent_rows_reject_invented_pass(self):
        doc = json.loads(MANIFEST_PATH.read_text())
        result = run_checker(doc, passing_report(doc))
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("cannot report pass", output(result))

    def test_require_early_on_unregistered_committed_gate_fails(self):
        doc = json.loads(MANIFEST_PATH.read_text())
        result = run_checker(doc, passing_report(doc), extra=("--require", "early"))
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("disabled-gate", output(result))


class ManifestSchemaTests(unittest.TestCase):
    def test_duplicate_scenario_ids_rejected(self):
        manifest = fixture_manifest()
        manifest["scenarios"][1]["id"] = manifest["scenarios"][0]["id"]
        result = run_checker(manifest, passing_report(fixture_manifest()))
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("duplicate-id", output(result))

    def test_duplicate_required_contract_ids_rejected(self):
        manifest = fixture_manifest()
        manifest["required_contract_ids"] = list(CONTRACTS) + [CONTRACTS[0]]
        result = run_checker(manifest, passing_report(manifest))
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("duplicate-id", output(result))
        self.assertIn(CONTRACTS[0], output(result))

    def test_missing_selector_rejected(self):
        manifest = fixture_manifest()
        del manifest["scenarios"][0]["selector"]
        result = run_checker(manifest, passing_report(fixture_manifest()))
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("schema", output(result))
        self.assertIn("selector", output(result))

    def test_unknown_and_uncovered_contract_ids_rejected(self):
        manifest = fixture_manifest()
        manifest["scenarios"][0]["contract_ids"] = ["DT-UNKNOWN-01"]
        result = run_checker(manifest, passing_report(manifest))
        self.assertEqual(result.returncode, 1, output(result))
        text = output(result)
        self.assertIn("schema", text)
        self.assertIn("DT-UNKNOWN-01", text)
        self.assertIn(CONTRACTS[0], text)

    def test_empty_contract_ids_without_category_rejected(self):
        manifest = fixture_manifest()
        extra = scenario(CONTRACTS[0], id="budget-row", contract_ids=[], category="sql-work-budget")
        extra["contract_ids"] = []
        del extra["category"]
        manifest["scenarios"].append(extra)
        result = run_checker(manifest, passing_report(manifest))
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("category", output(result))

    def test_invalid_manifest_json_rejected(self):
        result = run_checker("{", passing_report(fixture_manifest()))
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("schema", output(result))

    def test_cli_documents_flags(self):
        result = subprocess.run(
            [sys.executable, str(SCRIPT), "--help"],
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, output(result))
        for flag in ("--manifest", "--report", "--require", "--strict"):
            self.assertIn(flag, result.stdout)


class EvidenceReportTests(unittest.TestCase):
    def test_complete_pass_exits_zero(self):
        manifest = fixture_manifest()
        result = run_checker(manifest, passing_report(manifest))
        self.assertEqual(result.returncode, 0, output(result))
        self.assertIn("test-evidence passed", result.stdout)

    def test_missing_scenario_fails(self):
        manifest = fixture_manifest()
        report = passing_report(manifest)
        report["scenarios"] = report["scenarios"][1:]
        result = run_checker(manifest, report)
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("missing-scenario", output(result))
        self.assertIn(manifest["scenarios"][0]["id"], output(result))

    def test_unexpected_skip_fails(self):
        manifest = fixture_manifest()
        report = passing_report(manifest)
        report["scenarios"][0]["status"] = "skip"
        report["scenarios"][0]["skip_reason"] = "flaky"
        result = run_checker(manifest, report)
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("unexpected-skip", output(result))

    def test_allowed_skip_passes(self):
        manifest = fixture_manifest()
        manifest["scenarios"][0]["allowed_skips"] = [{
            "reason": "engine_not_selected",
            "description": "This engine lane is not part of the report",
        }]
        report = passing_report(manifest)
        report["scenarios"][0] = {
            "id": manifest["scenarios"][0]["id"],
            "status": "skip",
            "skip_reason": "engine_not_selected",
        }
        result = run_checker(manifest, report)
        self.assertEqual(result.returncode, 0, output(result))

    def test_disabled_gate_flag_fails(self):
        manifest = fixture_manifest()
        report = passing_report(manifest, gate_enabled=False)
        result = run_checker(manifest, report)
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("disabled-gate", output(result))

    def test_disabled_gates_list_fails(self):
        manifest = fixture_manifest()
        report = passing_report(manifest, disabled_gates=["early"])
        result = run_checker(manifest, report)
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("disabled-gate", output(result))

    def test_require_unregistered_gate_fails(self):
        manifest = fixture_manifest()
        result = run_checker(manifest, passing_report(manifest), extra=("--require", "full"))
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("disabled-gate", output(result))

    def test_require_early_ignores_unselected_scenarios(self):
        manifest = fixture_manifest()
        manifest["scenarios"].append(scenario(CONTRACTS[0], id="late-full", gates=["full"]))
        selected = [item for item in manifest["scenarios"] if "early" in item["gates"]]
        report = passing_report(manifest, results=[passing_result(item) for item in selected])
        result = run_checker(manifest, report, extra=("--require", "early"))
        self.assertEqual(result.returncode, 0, output(result))

    def test_wrong_artifact_identity_fails(self):
        manifest = fixture_manifest()
        report = passing_report(manifest)
        report["scenarios"][0]["artifact_identity"] = "other-artifact"
        result = run_checker(manifest, report)
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("wrong-artifact-identity", output(result))

    def test_candidate_sha_mismatch_fails(self):
        manifest = fixture_manifest()
        report = passing_report(manifest)
        report["scenarios"][0]["candidate_sha"] = "b" * 40
        result = run_checker(manifest, report)
        self.assertEqual(result.returncode, 1, output(result))
        text = output(result)
        self.assertIn("wrong-artifact-identity", text)
        self.assertIn("does not match the report", text)

    def test_missing_scenario_identity_fails_under_strict(self):
        manifest = fixture_manifest()
        report = passing_report(manifest)
        for item in report["scenarios"]:
            del item["candidate_sha"]
            del item["candidate_digest"]
        result = run_checker(manifest, report, extra=("--strict",))
        self.assertEqual(result.returncode, 1, output(result))
        text = output(result)
        self.assertIn("wrong-artifact-identity", text)
        self.assertIn("missing from scenario evidence", text)
        self.assertIn("candidate SHA", text)
        self.assertIn("candidate digest", text)

    def test_missing_scenario_candidate_sha_fails(self):
        manifest = fixture_manifest()
        report = passing_report(manifest)
        del report["scenarios"][0]["candidate_sha"]
        result = run_checker(manifest, report)
        self.assertEqual(result.returncode, 1, output(result))
        text = output(result)
        self.assertIn("wrong-artifact-identity", text)
        self.assertIn("candidate SHA is missing from scenario evidence", text)

    def test_missing_scenario_candidate_digest_fails(self):
        manifest = fixture_manifest()
        report = passing_report(manifest)
        del report["scenarios"][0]["candidate_digest"]
        result = run_checker(manifest, report)
        self.assertEqual(result.returncode, 1, output(result))
        text = output(result)
        self.assertIn("wrong-artifact-identity", text)
        self.assertIn("candidate digest is missing from scenario evidence", text)

    def test_wrong_topology_mode_and_feature_flags_fail_under_strict(self):
        manifest = fixture_manifest()
        report = passing_report(manifest)
        for item in report["scenarios"]:
            item["topology"] = {
                "kind": "single-process",
                "replicas": 1,
                "persistence": False,
                "engine": "docker",
            }
            item["mode"] = {
                "execution": "local",
                "owner": "disabled",
                "surface": "unit",
            }
            item["feature_flags"] = {"CAESIUM_RUN_OWNER_ENABLED": "false"}
        result = run_checker(manifest, report, extra=("--strict",))
        self.assertEqual(result.returncode, 1, output(result))
        text = output(result)
        self.assertIn("wrong-topology", text)
        self.assertIn("wrong-mode", text)
        self.assertIn("wrong-feature-flags", text)
        self.assertIn("CAESIUM_RUN_OWNER_ENABLED", text)

    def test_topology_mismatch_fails(self):
        manifest = fixture_manifest()
        report = passing_report(manifest)
        report["scenarios"][0]["topology"] = {
            "kind": "single-process",
            "replicas": 1,
            "persistence": False,
            "engine": "docker",
        }
        result = run_checker(manifest, report)
        self.assertEqual(result.returncode, 1, output(result))
        text = output(result)
        self.assertIn("wrong-topology", text)
        self.assertNotIn("wrong-mode", text)
        self.assertNotIn("wrong-feature-flags", text)

    def test_mode_mismatch_fails(self):
        manifest = fixture_manifest()
        report = passing_report(manifest)
        report["scenarios"][0]["mode"] = {
            "execution": "local",
            "owner": "disabled",
            "surface": "unit",
        }
        result = run_checker(manifest, report)
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("wrong-mode", output(result))

    def test_feature_flag_mismatch_fails(self):
        manifest = fixture_manifest()
        report = passing_report(manifest)
        report["scenarios"][0]["feature_flags"] = {
            "CAESIUM_EXECUTION_MODE": "distributed",
            "CAESIUM_RUN_OWNER_ENABLED": "false",
        }
        result = run_checker(manifest, report)
        self.assertEqual(result.returncode, 1, output(result))
        text = output(result)
        self.assertIn("wrong-feature-flags", text)
        self.assertIn("CAESIUM_RUN_OWNER_ENABLED", text)

    def test_missing_observed_configuration_fails(self):
        manifest = fixture_manifest()
        report = passing_report(manifest)
        for item in report["scenarios"]:
            del item["topology"]
            del item["mode"]
            del item["feature_flags"]
        result = run_checker(manifest, report, extra=("--strict",))
        self.assertEqual(result.returncode, 1, output(result))
        text = output(result)
        self.assertIn("schema", text)
        self.assertIn("topology", text)
        self.assertIn("mode", text)
        self.assertIn("feature_flags", text)

    def test_absent_fault_activation_fails(self):
        manifest = fixture_manifest()
        report = passing_report(manifest)
        report["scenarios"][0]["fault_activation"] = {
            "activated": False,
            "kind": "owner-sigkill",
            "observations": ["killed"],
        }
        result = run_checker(manifest, report)
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("absent-fault-activation", output(result))

    def test_missing_fault_observations_fails(self):
        manifest = fixture_manifest()
        report = passing_report(manifest)
        report["scenarios"][0]["fault_activation"]["observations"] = []
        result = run_checker(manifest, report)
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("absent-fault-activation", output(result))

    def test_checker_timeout_is_inconclusive(self):
        manifest = fixture_manifest()
        report = passing_report(manifest)
        report["scenarios"][0]["status"] = "timeout"
        report["scenarios"][0]["checker"] = {"timed_out": True}
        result = run_checker(manifest, report)
        self.assertEqual(result.returncode, 2, output(result))
        self.assertIn("checker-timeout", output(result))
        self.assertIn("inconclusive", result.stderr)
        self.assertNotIn("test-evidence passed", result.stdout)

    def test_missing_recorder_is_inconclusive(self):
        manifest = fixture_manifest()
        report = passing_report(manifest)
        report["scenarios"][0]["recorder"] = {"present": False, "sample_count": 0}
        report["scenarios"][0]["observations"] = []
        result = run_checker(manifest, report)
        self.assertEqual(result.returncode, 2, output(result))
        self.assertIn("missing-recorder", output(result))
        self.assertNotIn("test-evidence failed", result.stderr)

    def test_insufficient_samples_is_inconclusive(self):
        manifest = fixture_manifest()
        report = passing_report(manifest)
        report["scenarios"][0]["recorder"] = {"present": True, "sample_count": 0}
        result = run_checker(manifest, report)
        self.assertEqual(result.returncode, 2, output(result))
        self.assertIn("insufficient-samples", output(result))

    def test_strict_maps_inconclusive_to_failure(self):
        manifest = fixture_manifest()
        report = passing_report(manifest)
        report["scenarios"][0]["checker"] = {"timed_out": True}
        result = run_checker(manifest, report, extra=("--strict",))
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("checker-timeout", output(result))
        self.assertIn("test-evidence failed", result.stderr)

    def test_fail_dominates_inconclusive(self):
        manifest = fixture_manifest()
        report = passing_report(manifest)
        report["scenarios"][0]["checker"] = {"timed_out": True}
        report["scenarios"] = report["scenarios"][:1]
        result = run_checker(manifest, report)
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("missing-scenario", output(result))
        self.assertIn("checker-timeout", output(result))

    def test_reported_fail_exits_nonzero(self):
        manifest = fixture_manifest()
        report = passing_report(manifest)
        report["scenarios"][0]["status"] = "fail"
        result = run_checker(manifest, report)
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("failed", output(result))

    def test_absent_status_cannot_pass(self):
        manifest = fixture_manifest()
        manifest["scenarios"][0]["status"] = "absent"
        result = run_checker(manifest, passing_report(manifest))
        self.assertEqual(result.returncode, 1, output(result))
        self.assertIn("cannot report pass", output(result))


if __name__ == "__main__":
    unittest.main()
