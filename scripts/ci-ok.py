#!/usr/bin/env python3
"""Evaluate a GitHub Actions `needs` JSON object for the merge gate.

Fails on missing, failed, cancelled, or unexpectedly skipped jobs. A skip
is allowed only when the successful changes job explicitly deselected that
job. Keep these selectors in sync with ci.yml; test_ci.py checks the wiring.

For the early-evidence lane a green job result is not sufficient evidence.
When that lane is part of the required set and the path filters selected it,
the gate re-reads the report the lane uploaded, requires it to bind to the
candidate SHA this run tested, requires every scenario the committed manifest
gates `early` to be reported `pass` with its required fault-activation
observations, and re-runs the fail-closed scenario checker over it. An absent,
unreadable, foreign or hollow report fails the gate.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
from pathlib import Path


SCRIPTS = Path(__file__).resolve().parent
CHECKER = SCRIPTS / "check-test-evidence.py"

# A job runs when any of its selectors is true. No selectors means always.
SELECTORS = {
    "changes": (),
    "ci-config": (),
    "builder": ("images",),
    "builder-arm64": ("images",),
    "images": ("images",),
    "images-arm64": ("images",),
    "reagents": ("go", "reagents", "ci"),
    "reagents-arm64": ("go", "reagents", "ci"),
    "lint": ("go", "reagents", "ci"),
    "unit-test": ("go", "reagents", "ci"),
    "unit-test-arm64": ("go", "ci"),
    "ui-test": ("ui", "go", "ci"),
    "ui-e2e": ("ui", "go", "ci"),
    "ui-e2e-auth": ("ui", "go", "ci"),
    "integration": ("go", "ci"),
    # Unconditional producer of the promoted lane: it never skips, so a helm
    # chart failure reports as itself instead of as a skipped consumer.
    "helm-lint": (),
    "early-evidence": ("go", "helm", "ci"),
}

# The promoted lane and the manifest gate its evidence must satisfy.
EVIDENCE_JOB = "early-evidence"
EVIDENCE_GATE = "early"


def gated_scenarios(manifest):
    """Manifest rows the evidence gate requires. An empty set is not a pass."""
    scenarios = manifest.get("scenarios")
    if not isinstance(scenarios, list):
        return []
    return [
        item for item in scenarios
        if isinstance(item, dict)
        and item.get("id")
        and EVIDENCE_GATE in (item.get("gates") or [])
    ]


def _fault_problem(scenario, result):
    """Return a message when required fault-activation evidence is absent."""
    fault = scenario.get("fault_activation") or {}
    if not fault.get("required"):
        return None
    observed = result.get("fault_activation")
    observed = observed if isinstance(observed, dict) else {}
    seen = observed.get("observations")
    seen = seen if isinstance(seen, list) else []
    missing = [item for item in fault.get("required_observations") or [] if item not in seen]
    if observed.get("activated") is not True:
        return f"fault {fault.get('kind')!r} not activated"
    if observed.get("kind") != fault.get("kind"):
        return f"fault kind {observed.get('kind')!r} != {fault.get('kind')!r}"
    if missing:
        return "missing fault observations " + ", ".join(missing)
    return None


def verify_evidence(report_path, manifest_path, candidate_sha):
    """Fail closed on absent, foreign or hollow early-gate evidence."""
    label = f"{EVIDENCE_JOB} evidence"
    if not report_path:
        return [f"{label}: no --evidence-report was passed to the gate"]
    if not candidate_sha:
        return [f"{label}: no --candidate-sha to bind the evidence to"]
    try:
        manifest = json.loads(Path(manifest_path).read_text())
    except (OSError, ValueError) as err:
        return [f"{label}: unreadable manifest {manifest_path}: {err}"]
    if not isinstance(manifest, dict):
        return [f"{label}: manifest {manifest_path} must be an object"]
    try:
        report = json.loads(Path(report_path).read_text())
    except FileNotFoundError:
        return [f"{label}: {report_path} is absent; the lane produced no evidence"]
    except (OSError, ValueError) as err:
        return [f"{label}: unreadable report {report_path}: {err}"]
    if not isinstance(report, dict):
        return [f"{label}: report {report_path} must be an object"]

    problems = []
    required = gated_scenarios(manifest)
    if not required:
        problems.append(f"{label}: gate {EVIDENCE_GATE!r} has no registered scenarios")
    reported_sha = report.get("candidate_sha")
    if reported_sha != candidate_sha:
        problems.append(
            f"{label}: report candidate_sha {reported_sha!r} is not the tested candidate {candidate_sha!r}"
        )
    results = {
        item["id"]: item for item in report.get("scenarios") or []
        if isinstance(item, dict) and item.get("id")
    }
    for scenario in required:
        sid = scenario["id"]
        result = results.get(sid)
        if result is None:
            problems.append(f"{label}: scenario {sid!r} is missing from the report")
            continue
        if result.get("status") != "pass":
            problems.append(f"{label}: scenario {sid!r} status {result.get('status')!r}, want 'pass'")
        fault = _fault_problem(scenario, result)
        if fault:
            problems.append(f"{label}: scenario {sid!r} {fault}")

    checked = subprocess.run(
        [
            sys.executable, str(CHECKER),
            "--manifest", str(manifest_path),
            "--report", str(report_path),
            "--require", EVIDENCE_GATE,
            "--strict",
        ],
        capture_output=True,
        text=True,
    )
    output = (checked.stdout + checked.stderr).strip()
    if output:
        print(output)
    if checked.returncode != 0:
        problems.append(f"{label}: check-test-evidence.py exited {checked.returncode}")
    return problems


def parse_args(argv):
    parser = argparse.ArgumentParser(description="Evaluate the CI merge gate.")
    parser.add_argument("jobs", nargs="*", help="required job names, matching ci.yml `needs`")
    parser.add_argument("--evidence-report", default=None, help="evidence report JSON the promoted lane uploaded")
    parser.add_argument(
        "--evidence-manifest",
        default=str(SCRIPTS.parent / "test/contracts/scenarios.json"),
        help="committed scenario manifest",
    )
    parser.add_argument("--candidate-sha", default=None, help="the commit SHA this run tested")
    return parser.parse_args(argv)


def main(argv=None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    raw = os.environ.get("NEEDS_JSON", "")
    if not raw:
        print("ci-ok: NEEDS_JSON is empty", file=sys.stderr)
        return 1
    try:
        needs = json.loads(raw)
    except json.JSONDecodeError as err:
        print(f"ci-ok: NEEDS_JSON is not JSON: {err}", file=sys.stderr)
        return 1

    if not isinstance(needs, dict):
        print("ci-ok: NEEDS_JSON must be an object", file=sys.stderr)
        return 1
    changes = needs.get("changes")
    if not isinstance(changes, dict) or changes.get("result") != "success":
        print("ci-ok: changes must succeed", file=sys.stderr)
        return 1
    outputs = changes.get("outputs")
    if not isinstance(outputs, dict) or any(
        outputs.get(key) not in ("true", "false")
        for key in ("go", "ui", "helm", "reagents", "ci", "images")
    ):
        print("ci-ok: missing or invalid change outputs", file=sys.stderr)
        return 1
    any_change = any(outputs[key] == "true" for key in ("go", "ui", "helm", "reagents", "ci"))
    if (outputs["images"] == "true") != any_change:
        print("ci-ok: inconsistent image selection", file=sys.stderr)
        return 1

    required = args.jobs
    if not required:
        print("ci-ok: no required job names passed", file=sys.stderr)
        return 1

    failed: list[str] = []
    lines: list[str] = []
    deselected: dict[str, bool] = {}
    for name in required:
        entry = needs.get(name)
        result = entry.get("result", "missing") if isinstance(entry, dict) else "missing"
        lines.append(f"  {name}: {result}")
        selectors = SELECTORS.get(name)
        skip_allowed = bool(selectors) and all(outputs[key] == "false" for key in selectors)
        deselected[name] = skip_allowed
        if selectors is None or (result != "success" and not (result == "skipped" and skip_allowed)):
            failed.append(f"{name}={result}")

    print("ci-ok job results:")
    print("\n".join(lines))

    if EVIDENCE_JOB in required:
        if deselected.get(EVIDENCE_JOB):
            print(f"ci-ok: {EVIDENCE_JOB} was deselected by the path filters; no evidence required")
        else:
            failed.extend(verify_evidence(
                args.evidence_report, args.evidence_manifest, args.candidate_sha,
            ))

    if failed:
        print("ci-ok failed: " + ", ".join(failed), file=sys.stderr)
        return 1
    print("ci-ok passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
