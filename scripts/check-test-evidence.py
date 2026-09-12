#!/usr/bin/env python3
"""Validate a scenario evidence report against the distributed-testing manifest.

The checker is fail-closed and dependency-free (Python 3 stdlib). Inconclusive
evidence is never a pass. G3+ should invoke it against the real report:

  python3 scripts/check-test-evidence.py \\
      --manifest test/contracts/scenarios.json \\
      --report "$ARTIFACTS/evidence.json" \\
      --require early --strict

Flags:
  --manifest PATH   Scenario manifest JSON (required).
  --report PATH     Evidence report JSON (required).
  --require GATE    Only require scenarios that list this gate (for example
                    early or full). Default: every scenario in the manifest.
  --strict          Treat inconclusive results as failures (exit 1). Without
                    this flag, a report that is only inconclusive exits 2.

Exit status:
  0  every required scenario passed (or used an allowed skip)
  1  failed closed: invalid schema, duplicate IDs, missing scenario,
     unexpected skip, disabled gate, wrong topology/mode/feature flags,
     wrong or missing scenario artifact identity, absent fault-activation
     evidence, failed observations, invented pass, or --strict with
     inconclusive evidence
  2  no hard failures, but at least one required scenario is inconclusive
     (missing recorder, checker timeout, insufficient samples)
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path


REQUIRED_CONTRACT_IDS = (
    "DT-ADMIT-01",
    "DT-OWNER-01",
    "DT-COMPLETE-01",
    "DT-TERMINAL-01",
    "DT-RETRY-01",
    "DT-DAG-01",
    "DT-CANCEL-01",
    "DT-EVENT-01",
    "DT-AUTH-01",
    "DT-RECOVER-01",
    "DT-QUORUM-01",
)

SCENARIO_STATUSES = {"absent", "unproven", "proven"}
REPORT_STATUSES = {"pass", "fail", "skip", "inconclusive", "timeout"}
HEX = set("0123456789abcdefABCDEF")


class Issue:
    __slots__ = ("verdict", "code", "target", "message")

    def __init__(self, verdict, code, message, target=""):
        self.verdict = verdict
        self.code = code
        self.target = target
        self.message = message

    def label(self):
        return f"{self.code}={self.target}" if self.target else self.code


def _fail(code, message, target=""):
    return Issue("fail", code, message, target)


def _inconclusive(code, message, target=""):
    return Issue("inconclusive", code, message, target)


def _is_int(value):
    return isinstance(value, int) and not isinstance(value, bool)


def _is_sha(value):
    return isinstance(value, str) and len(value) in (40, 64) and HEX.issuperset(value)


def _is_digest(value):
    if not isinstance(value, str) or not value.startswith("sha256:"):
        return False
    digest = value.split(":", 1)[1]
    return len(digest) == 64 and HEX.issuperset(digest)


def load_json(path):
    try:
        raw = Path(path).read_text()
    except OSError as err:
        raise ValueError(f"cannot read {path}: {err}") from err
    try:
        return json.loads(raw)
    except json.JSONDecodeError as err:
        raise ValueError(f"{path} is not JSON: {err}") from err


def _require(obj, name, typ, path, issues, *, nonempty=False):
    if name not in obj:
        issues.append(_fail("schema", f"{path}.{name} is required"))
        return None
    value = obj[name]
    if typ is int:
        ok = _is_int(value)
        type_name = "int"
    else:
        ok = isinstance(value, typ)
        type_name = typ.__name__
    if not ok:
        issues.append(_fail("schema", f"{path}.{name} must be {type_name}"))
        return None
    if nonempty and typ is str and not value.strip():
        issues.append(_fail("schema", f"{path}.{name} must be nonempty"))
        return None
    if nonempty and typ is list and not value:
        issues.append(_fail("schema", f"{path}.{name} must be nonempty"))
        return None
    if nonempty and typ is dict and not value:
        issues.append(_fail("schema", f"{path}.{name} must be nonempty"))
        return None
    return value


def validate_manifest(doc):
    issues = []
    if not isinstance(doc, dict):
        return [_fail("schema", "manifest must be an object")]
    version = _require(doc, "schema_version", int, "manifest", issues)
    if version is not None and version != 1:
        issues.append(_fail("schema", "manifest.schema_version must be 1"))
    contract_ids = _require(
        doc, "required_contract_ids", list, "manifest", issues, nonempty=True
    )
    scenarios = _require(doc, "scenarios", list, "manifest", issues, nonempty=True)
    if contract_ids is None or scenarios is None:
        return issues
    if any(not isinstance(item, str) for item in contract_ids):
        issues.append(_fail("schema", "manifest.required_contract_ids must be strings"))
        return issues
    seen_contracts = []
    duplicates = []
    for item in contract_ids:
        if item in seen_contracts and item not in duplicates:
            duplicates.append(item)
            issues.append(_fail(
                "duplicate-id",
                f"required contract id {item!r} is repeated",
                item,
            ))
        seen_contracts.append(item)
    missing = [item for item in REQUIRED_CONTRACT_IDS if item not in contract_ids]
    extra = [item for item in contract_ids if item not in REQUIRED_CONTRACT_IDS]
    if missing:
        issues.append(_fail(
            "schema",
            "manifest.required_contract_ids is missing " + ", ".join(missing),
        ))
    if extra:
        issues.append(_fail(
            "schema",
            "manifest.required_contract_ids has unknown ids " + ", ".join(extra),
        ))

    seen_ids = []
    covered = set()
    for index, scenario in enumerate(scenarios):
        path = f"manifest.scenarios[{index}]"
        if not isinstance(scenario, dict):
            issues.append(_fail("schema", f"{path} must be an object"))
            continue
        sid = _require(scenario, "id", str, path, issues, nonempty=True)
        if sid:
            if sid in seen_ids:
                issues.append(_fail("duplicate-id", f"scenario id {sid!r} is repeated", sid))
            else:
                seen_ids.append(sid)
        _require(scenario, "name", str, path, issues, nonempty=True)
        _require(scenario, "selector", str, path, issues, nonempty=True)
        _require(scenario, "owner_item", str, path, issues, nonempty=True)
        status = _require(scenario, "status", str, path, issues, nonempty=True)
        if status is not None and status not in SCENARIO_STATUSES:
            issues.append(_fail(
                "schema",
                f"{path}.status must be one of {sorted(SCENARIO_STATUSES)}",
                sid or "",
            ))
        gates = _require(scenario, "gates", list, path, issues)
        if gates is not None:
            for gate in gates:
                if not isinstance(gate, str) or not gate.strip():
                    issues.append(_fail("schema", f"{path}.gates must be nonempty strings", sid or ""))
                    break
        contract_ids_field = _require(scenario, "contract_ids", list, path, issues)
        category = scenario.get("category")
        if contract_ids_field is not None:
            if any(not isinstance(item, str) or not item.strip() for item in contract_ids_field):
                issues.append(_fail("schema", f"{path}.contract_ids must be nonempty strings", sid or ""))
            else:
                unknown = [item for item in contract_ids_field if item not in REQUIRED_CONTRACT_IDS]
                if unknown:
                    issues.append(_fail(
                        "schema",
                        f"{path}.contract_ids has unknown ids " + ", ".join(unknown),
                        sid or "",
                    ))
                covered.update(contract_ids_field)
            if not contract_ids_field and not (isinstance(category, str) and category.strip()):
                issues.append(_fail(
                    "schema",
                    f"{path} needs contract_ids or a nonempty category",
                    sid or "",
                ))
        if category is not None and (not isinstance(category, str) or not category.strip()):
            issues.append(_fail("schema", f"{path}.category must be a nonempty string", sid or ""))
        _validate_topology(scenario.get("topology"), f"{path}.topology", issues, sid or "")
        _validate_mode(scenario.get("mode"), f"{path}.mode", issues, sid or "")
        _validate_feature_flags(scenario.get("feature_flags"), f"{path}.feature_flags", issues, sid or "")
        _validate_observations(scenario.get("expected_observations"), f"{path}.expected_observations", issues, sid or "")
        _validate_allowed_skips(scenario.get("allowed_skips"), f"{path}.allowed_skips", issues, sid or "")
        _validate_evidence(scenario.get("evidence"), f"{path}.evidence", issues, sid or "")
        _validate_fault(scenario.get("fault_activation"), f"{path}.fault_activation", issues, sid or "")
        if "registered_by" in scenario:
            _require(scenario, "registered_by", str, path, issues, nonempty=True)
        if "notes" in scenario:
            _require(scenario, "notes", str, path, issues, nonempty=True)
    uncovered = [item for item in REQUIRED_CONTRACT_IDS if item not in covered]
    if uncovered:
        issues.append(_fail(
            "schema",
            "no scenario covers " + ", ".join(uncovered),
        ))
    return issues


def _validate_topology(value, path, issues, target):
    if not isinstance(value, dict):
        issues.append(_fail("schema", f"{path} must be an object", target))
        return
    _require(value, "kind", str, path, issues, nonempty=True)
    replicas = _require(value, "replicas", int, path, issues)
    _require(value, "persistence", bool, path, issues)
    _require(value, "engine", str, path, issues, nonempty=True)
    if replicas is not None and replicas < 1:
        issues.append(_fail("schema", f"{path}.replicas must be >= 1", target))
    for name in ("voters", "standbys", "database_shards", "control_plane_nodes", "worker_nodes"):
        if name in value and (not _is_int(value[name]) or value[name] < 0):
            issues.append(_fail("schema", f"{path}.{name} must be an int >= 0", target))


def _validate_mode(value, path, issues, target):
    if not isinstance(value, dict):
        issues.append(_fail("schema", f"{path} must be an object", target))
        return
    _require(value, "execution", str, path, issues, nonempty=True)
    _require(value, "surface", str, path, issues, nonempty=True)
    if "owner" in value:
        _require(value, "owner", str, path, issues, nonempty=True)


def _validate_feature_flags(value, path, issues, target):
    if not isinstance(value, dict):
        issues.append(_fail("schema", f"{path} must be an object", target))
        return
    for key, item in value.items():
        if not isinstance(key, str) or not key.strip() or not isinstance(item, str):
            issues.append(_fail("schema", f"{path} keys and values must be nonempty strings", target))
            return


def _validate_observations(value, path, issues, target):
    if not isinstance(value, list) or not value:
        issues.append(_fail("schema", f"{path} must be a nonempty list", target))
        return
    seen = []
    for index, item in enumerate(value):
        item_path = f"{path}[{index}]"
        if not isinstance(item, dict):
            issues.append(_fail("schema", f"{item_path} must be an object", target))
            continue
        oid = _require(item, "id", str, item_path, issues, nonempty=True)
        _require(item, "description", str, item_path, issues, nonempty=True)
        if oid:
            if oid in seen:
                issues.append(_fail("duplicate-id", f"{item_path}.id {oid!r} is repeated", target))
            else:
                seen.append(oid)


def _validate_allowed_skips(value, path, issues, target):
    if not isinstance(value, list):
        issues.append(_fail("schema", f"{path} must be a list", target))
        return
    seen = []
    for index, item in enumerate(value):
        item_path = f"{path}[{index}]"
        if not isinstance(item, dict):
            issues.append(_fail("schema", f"{item_path} must be an object", target))
            continue
        reason = _require(item, "reason", str, item_path, issues, nonempty=True)
        _require(item, "description", str, item_path, issues, nonempty=True)
        if reason:
            if reason in seen:
                issues.append(_fail("duplicate-id", f"{item_path}.reason {reason!r} is repeated", target))
            else:
                seen.append(reason)


def _validate_evidence(value, path, issues, target):
    if not isinstance(value, dict):
        issues.append(_fail("schema", f"{path} must be an object", target))
        return
    for name in ("require_candidate_sha", "require_candidate_digest", "require_recorder"):
        _require(value, name, bool, path, issues)
    _require(value, "artifact_identity", str, path, issues, nonempty=True)
    samples = _require(value, "min_samples", int, path, issues)
    if samples is not None and samples < 0:
        issues.append(_fail("schema", f"{path}.min_samples must be >= 0", target))


def _validate_fault(value, path, issues, target):
    if not isinstance(value, dict):
        issues.append(_fail("schema", f"{path} must be an object", target))
        return
    required = _require(value, "required", bool, path, issues)
    kind = _require(value, "kind", str, path, issues)
    observations = _require(value, "required_observations", list, path, issues)
    if required and (not isinstance(kind, str) or not kind.strip()):
        issues.append(_fail("schema", f"{path}.kind is required when fault activation is required", target))
    if observations is not None:
        if any(not isinstance(item, str) or not item.strip() for item in observations):
            issues.append(_fail("schema", f"{path}.required_observations must be nonempty strings", target))
        elif required and not observations:
            issues.append(_fail("schema", f"{path}.required_observations must be nonempty when required", target))


def validate_report_schema(doc):
    issues = []
    if not isinstance(doc, dict):
        return [_fail("schema", "report must be an object")]
    _require(doc, "candidate_sha", str, "report", issues, nonempty=True)
    _require(doc, "candidate_digest", str, "report", issues, nonempty=True)
    _require(doc, "gate_enabled", bool, "report", issues)
    disabled = _require(doc, "disabled_gates", list, "report", issues)
    if disabled is not None and any(not isinstance(item, str) or not item.strip() for item in disabled):
        issues.append(_fail("schema", "report.disabled_gates must be nonempty strings"))
    scenarios = _require(doc, "scenarios", list, "report", issues)
    if scenarios is None:
        return issues
    seen = []
    for index, scenario in enumerate(scenarios):
        path = f"report.scenarios[{index}]"
        if not isinstance(scenario, dict):
            issues.append(_fail("schema", f"{path} must be an object"))
            continue
        sid = _require(scenario, "id", str, path, issues, nonempty=True)
        if sid:
            if sid in seen:
                issues.append(_fail("duplicate-id", f"report scenario id {sid!r} is repeated", sid))
            else:
                seen.append(sid)
        status = _require(scenario, "status", str, path, issues, nonempty=True)
        if status is not None and status not in REPORT_STATUSES:
            issues.append(_fail(
                "schema",
                f"{path}.status must be one of {sorted(REPORT_STATUSES)}",
                sid or "",
            ))
        if "skip_reason" in scenario and scenario["skip_reason"] is not None:
            _require(scenario, "skip_reason", str, path, issues, nonempty=True)
        if "artifact_identity" in scenario:
            _require(scenario, "artifact_identity", str, path, issues, nonempty=True)
        if "candidate_sha" in scenario:
            _require(scenario, "candidate_sha", str, path, issues, nonempty=True)
        if "candidate_digest" in scenario:
            _require(scenario, "candidate_digest", str, path, issues, nonempty=True)
        require_observed_config = status in REPORT_STATUSES and status != "skip"
        if require_observed_config or "topology" in scenario:
            _validate_topology(scenario.get("topology"), f"{path}.topology", issues, sid or "")
        if require_observed_config or "mode" in scenario:
            _validate_mode(scenario.get("mode"), f"{path}.mode", issues, sid or "")
        if require_observed_config or "feature_flags" in scenario:
            _validate_feature_flags(
                scenario.get("feature_flags"), f"{path}.feature_flags", issues, sid or ""
            )
        if "observations" in scenario and not isinstance(scenario["observations"], list):
            issues.append(_fail("schema", f"{path}.observations must be a list", sid or ""))
        elif "observations" in scenario and any(not isinstance(item, str) for item in scenario["observations"]):
            issues.append(_fail("schema", f"{path}.observations must be strings", sid or ""))
        if "fault_activation" in scenario:
            fault = scenario["fault_activation"]
            if not isinstance(fault, dict):
                issues.append(_fail("schema", f"{path}.fault_activation must be an object", sid or ""))
            else:
                _require(fault, "activated", bool, f"{path}.fault_activation", issues)
                if "kind" in fault:
                    _require(fault, "kind", str, f"{path}.fault_activation", issues, nonempty=True)
                if "observations" in fault and not (
                    isinstance(fault["observations"], list)
                    and all(isinstance(item, str) and item.strip() for item in fault["observations"])
                ):
                    issues.append(_fail("schema", f"{path}.fault_activation.observations must be nonempty strings", sid or ""))
        if "recorder" in scenario:
            recorder = scenario["recorder"]
            if not isinstance(recorder, dict):
                issues.append(_fail("schema", f"{path}.recorder must be an object", sid or ""))
            else:
                _require(recorder, "present", bool, f"{path}.recorder", issues)
                if "sample_count" in recorder:
                    count = _require(recorder, "sample_count", int, f"{path}.recorder", issues)
                    if count is not None and count < 0:
                        issues.append(_fail("schema", f"{path}.recorder.sample_count must be >= 0", sid or ""))
        if "checker" in scenario:
            checker = scenario["checker"]
            if not isinstance(checker, dict):
                issues.append(_fail("schema", f"{path}.checker must be an object", sid or ""))
            else:
                _require(checker, "timed_out", bool, f"{path}.checker", issues)
    return issues


def _allowed_skip_reasons(scenario):
    return {item["reason"] for item in scenario.get("allowed_skips") or [] if isinstance(item, dict) and "reason" in item}


def _compare_observed_mapping(expected, actual, label, code, sid):
    """Fail closed when observed evidence does not satisfy manifest requirements."""
    issues = []
    if not isinstance(expected, dict):
        return issues
    if not isinstance(actual, dict):
        issues.append(_fail(
            code,
            f"scenario {sid!r} {label} is missing from evidence",
            sid,
        ))
        return issues
    mismatches = []
    for key, value in expected.items():
        if key not in actual:
            mismatches.append(f"{key} missing")
        elif actual[key] != value:
            mismatches.append(f"{key} {actual[key]!r} != {value!r}")
    if mismatches:
        issues.append(_fail(
            code,
            f"scenario {sid!r} {label} does not match the manifest: " + ", ".join(mismatches),
            sid,
        ))
    return issues


def _scenario_identity(result, field):
    """Return the scenario's own identity field; do not inherit the report header."""
    if field not in result:
        return None
    return result.get(field)


def compare_report(manifest, report, require_gate=None):
    issues = []
    if report.get("gate_enabled") is not True:
        issues.append(_fail("disabled-gate", "report.gate_enabled must be true"))
    disabled = report.get("disabled_gates") or []
    if disabled:
        issues.append(_fail(
            "disabled-gate",
            "report.disabled_gates must be empty, got " + ", ".join(disabled),
            ",".join(disabled),
        ))
    scenarios = [item for item in manifest.get("scenarios", []) if isinstance(item, dict) and item.get("id")]
    if require_gate is not None:
        if not isinstance(require_gate, str) or not require_gate.strip():
            issues.append(_fail("schema", "--require gate must be nonempty"))
            return issues
        selected = [item for item in scenarios if require_gate in (item.get("gates") or [])]
        if not selected:
            issues.append(_fail(
                "disabled-gate",
                f"gate {require_gate!r} has no registered scenarios",
                require_gate,
            ))
            return issues
        scenarios = selected
    by_id = {item["id"]: item for item in scenarios}
    reported = {}
    for item in report.get("scenarios") or []:
        if not isinstance(item, dict) or not item.get("id"):
            continue
        reported[item["id"]] = item
    for sid, scenario in by_id.items():
        if sid not in reported:
            issues.append(_fail("missing-scenario", f"scenario {sid!r} is not in the report", sid))
    for sid, result in reported.items():
        if sid not in {item["id"] for item in manifest.get("scenarios", []) if isinstance(item, dict)}:
            issues.append(_fail("missing-scenario", f"report scenario {sid!r} is not in the manifest", sid))
            continue
        if sid not in by_id:
            continue
        issues.extend(_compare_scenario(by_id[sid], result, report))
    return issues


def _compare_scenario(scenario, result, report):
    issues = []
    sid = scenario["id"]
    status = result.get("status")
    skip_reason = result.get("skip_reason")
    allowed = _allowed_skip_reasons(scenario)
    if status == "skip":
        if not isinstance(skip_reason, str) or skip_reason not in allowed:
            issues.append(_fail(
                "unexpected-skip",
                f"scenario {sid!r} skipped with unexpected reason {skip_reason!r}",
                sid,
            ))
        return issues
    if skip_reason:
        issues.append(_fail(
            "unexpected-skip",
            f"scenario {sid!r} has skip_reason without status skip",
            sid,
        ))
    if status == "fail":
        issues.append(_fail("failed", f"scenario {sid!r} reported fail", sid))

    issues.extend(_compare_observed_mapping(
        scenario.get("topology"),
        result.get("topology"),
        "topology",
        "wrong-topology",
        sid,
    ))
    issues.extend(_compare_observed_mapping(
        scenario.get("mode"),
        result.get("mode"),
        "mode",
        "wrong-mode",
        sid,
    ))
    issues.extend(_compare_observed_mapping(
        scenario.get("feature_flags"),
        result.get("feature_flags"),
        "feature flags",
        "wrong-feature-flags",
        sid,
    ))

    evidence = scenario.get("evidence") or {}
    top_sha = report.get("candidate_sha")
    top_digest = report.get("candidate_digest")
    result_sha = _scenario_identity(result, "candidate_sha")
    result_digest = _scenario_identity(result, "candidate_digest")
    if evidence.get("require_candidate_sha"):
        if not isinstance(result_sha, str) or not result_sha.strip():
            issues.append(_fail(
                "wrong-artifact-identity",
                f"scenario {sid!r} candidate SHA is missing from scenario evidence",
                sid,
            ))
        elif not _is_sha(top_sha) or not _is_sha(result_sha) or result_sha != top_sha:
            issues.append(_fail(
                "wrong-artifact-identity",
                f"scenario {sid!r} candidate SHA does not match the report",
                sid,
            ))
    if evidence.get("require_candidate_digest"):
        if not isinstance(result_digest, str) or not result_digest.strip():
            issues.append(_fail(
                "wrong-artifact-identity",
                f"scenario {sid!r} candidate digest is missing from scenario evidence",
                sid,
            ))
        elif not _is_digest(top_digest) or not _is_digest(result_digest) or result_digest != top_digest:
            issues.append(_fail(
                "wrong-artifact-identity",
                f"scenario {sid!r} candidate digest does not match the report",
                sid,
            ))
    expected_identity = (evidence.get("artifact_identity") or "")
    actual_identity = result.get("artifact_identity")
    if actual_identity != expected_identity:
        issues.append(_fail(
            "wrong-artifact-identity",
            f"scenario {sid!r} artifact identity {actual_identity!r} != {expected_identity!r}",
            sid,
        ))

    fault = scenario.get("fault_activation") or {}
    observed_fault = result.get("fault_activation") if isinstance(result.get("fault_activation"), dict) else {}
    if fault.get("required"):
        observations = observed_fault.get("observations") if isinstance(observed_fault.get("observations"), list) else []
        missing = [item for item in fault.get("required_observations") or [] if item not in observations]
        if observed_fault.get("activated") is not True or observed_fault.get("kind") != fault.get("kind") or missing:
            issues.append(_fail(
                "absent-fault-activation",
                f"scenario {sid!r} is missing required {fault.get('kind')!r} fault evidence",
                sid,
            ))

    checker = result.get("checker") if isinstance(result.get("checker"), dict) else {}
    recorder = result.get("recorder") if isinstance(result.get("recorder"), dict) else {}
    timed_out = checker.get("timed_out") is True or status == "timeout"
    recorder_missing = evidence.get("require_recorder") and recorder.get("present") is not True
    sample_count = recorder.get("sample_count")
    min_samples = evidence.get("min_samples") or 0
    undersampled = (
        evidence.get("require_recorder")
        and recorder.get("present") is True
        and (not _is_int(sample_count) or sample_count < min_samples)
    )
    if timed_out:
        issues.append(_inconclusive(
            "checker-timeout",
            f"scenario {sid!r} checker timed out",
            sid,
        ))
    if recorder_missing:
        issues.append(_inconclusive(
            "missing-recorder",
            f"scenario {sid!r} recorder is absent",
            sid,
        ))
    if undersampled:
        issues.append(_inconclusive(
            "insufficient-samples",
            f"scenario {sid!r} sample_count {sample_count!r} < min_samples {min_samples}",
            sid,
        ))
    if status == "inconclusive" and not (timed_out or recorder_missing or undersampled):
        issues.append(_inconclusive(
            "insufficient-samples",
            f"scenario {sid!r} reported inconclusive",
            sid,
        ))

    expected = [item["id"] for item in scenario.get("expected_observations") or [] if isinstance(item, dict) and item.get("id")]
    observed = result.get("observations") if isinstance(result.get("observations"), list) else []
    missing_obs = [item for item in expected if item not in observed]
    if missing_obs and not (timed_out or recorder_missing or undersampled):
        issues.append(_fail(
            "failed",
            f"scenario {sid!r} missing observations " + ", ".join(missing_obs),
            sid,
        ))

    if scenario.get("status") == "absent" and status == "pass":
        issues.append(_fail(
            "failed",
            f"scenario {sid!r} is absent and cannot report pass",
            sid,
        ))
    if scenario.get("status") == "proven" and status != "pass":
        issues.append(_fail(
            "failed",
            f"scenario {sid!r} is marked proven but did not pass",
            sid,
        ))
    return issues


def evaluate(manifest, report, require_gate=None, strict=False):
    issues = validate_manifest(manifest)
    if issues:
        return issues
    issues.extend(validate_report_schema(report))
    if any(item.code in {"schema", "duplicate-id"} for item in issues):
        return issues
    issues.extend(compare_report(manifest, report, require_gate=require_gate))
    if strict:
        for item in issues:
            if item.verdict == "inconclusive":
                item.verdict = "fail"
    return issues


def exit_code(issues):
    if any(item.verdict == "fail" for item in issues):
        return 1
    if any(item.verdict == "inconclusive" for item in issues):
        return 2
    return 0


def parse_args(argv=None):
    parser = argparse.ArgumentParser(
        prog="check-test-evidence.py",
        description="Validate a scenario evidence report against the A2 manifest.",
    )
    parser.add_argument("--manifest", required=True, help="Path to test/contracts/scenarios.json")
    parser.add_argument("--report", required=True, help="Path to the evidence report JSON")
    parser.add_argument(
        "--require",
        dest="require_gate",
        default=None,
        help="Only require scenarios registered for this gate (early, full, ...)",
    )
    parser.add_argument(
        "--strict",
        action="store_true",
        help="Treat inconclusive evidence as a failure (exit 1)",
    )
    return parser.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    try:
        manifest = load_json(args.manifest)
        report = load_json(args.report)
    except ValueError as err:
        print(f"test-evidence: schema: {err}", file=sys.stderr)
        return 1
    issues = evaluate(manifest, report, require_gate=args.require_gate, strict=args.strict)
    lines = [f"  {item.label()}: {item.message}" for item in issues]
    if lines:
        print("test-evidence issues:")
        print("\n".join(lines))
    code = exit_code(issues)
    failed = [item.label() for item in issues if item.verdict == "fail"]
    inconclusive = [item.label() for item in issues if item.verdict == "inconclusive"]
    if failed:
        print("test-evidence failed: " + ", ".join(failed), file=sys.stderr)
    elif inconclusive:
        print("test-evidence inconclusive: " + ", ".join(inconclusive), file=sys.stderr)
    else:
        print("test-evidence passed")
    return code


if __name__ == "__main__":
    raise SystemExit(main())
