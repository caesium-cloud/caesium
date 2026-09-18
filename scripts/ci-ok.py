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

distributed-testing G7 -- candidate identity and base freshness (Q6):

Whenever the caller supplies ANY of `--candidate-sha`/`--event-name`/
`--base-sha`/`--head-sha` (checked by is-None, i.e. "was this flag passed at
all" -- not by truthiness, so an explicit `--candidate-sha ''` cannot slip
past as though the flag were omitted), the gate also verifies:

- The full candidate identity is present and internally consistent for the
  triggering event: `--event-name`, this run's tested `--candidate-sha`, and
  for `pull_request`/`merge_group` a non-empty `--base-sha`/`--head-sha`.
  Missing identity or an unrecognized event refuses. On `merge_group`,
  `--candidate-sha` must equal `--head-sha`, per GitHub's contract that the
  tested commit IS the merge group's head. This does not (and cannot) verify
  that `github.sha` itself is the real prospective merge commit -- that is
  GitHub's own event contract, audited structurally by test_ci.py rather
  than re-derived here -- except on `pull_request`, where the tested commit
  is a synthetic merge (`refs/pull/N/merge`) and IS independently checked:
  see `verify_pull_request_candidate_parents`.
- On `pull_request`, the candidate's actual git parents (read via
  `--candidate-parents`, itself produced by `git cat-file -p` in the
  workflow -- this module stays I/O-free) must number exactly two, and the
  second must match `--head-sha`; a `pull_request.base.sha` payload field
  that disagrees with the first parent is reported, not refused, since the
  merge ref can be regenerated after the payload was recorded. The first
  parent -- not the payload field -- is what base-freshness compares.
- A base-freshness comparison between the tested base SHA and a
  `--current-base-sha` read fresh at evaluation time. On `pull_request`
  (`strict: false`, no queue -- see docs/ci.md) this is informational only:
  a stale base is logged and written to the job summary, never failed,
  because master moving during review is expected and unenforceable from a
  workflow. On `merge_group` a mismatch -- or a missing input on either
  side -- is a real integrity failure and fails closed, because the queue is
  what is supposed to guarantee the tested base is current.
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


# Event types this gate knows how to reason about. Anything else refuses:
# an unrecognized trigger cannot be assumed to carry the invariants (a real
# prospective-merge `github.sha`, a meaningful base/head pair) this gate
# depends on.
RECOGNIZED_EVENTS = ("pull_request", "merge_group", "push")

# Events that name a base/head pair a caller must supply.
_BASE_HEAD_EVENTS = ("pull_request", "merge_group")


def verify_candidate_identity(event_name, candidate_sha, base_sha, head_sha):
    """Validate the recorded identity of the commit this run tested.

    Pure and side-effect-free. Returns a list of problems; empty means the
    identity is well-formed (not that it is *correct* -- GitHub's event
    payload is trusted for the base/head values themselves).
    """
    label = "candidate identity"
    if not event_name:
        return [f"{label}: no --event-name was passed to the gate"]
    if not candidate_sha:
        return [f"{label}: no --candidate-sha was passed to the gate"]
    if event_name not in RECOGNIZED_EVENTS:
        return [f"{label}: unrecognized event {event_name!r}"]
    problems = []
    if event_name in _BASE_HEAD_EVENTS:
        if not base_sha:
            problems.append(f"{label}: {event_name} run is missing --base-sha")
        if not head_sha:
            problems.append(f"{label}: {event_name} run is missing --head-sha")
    # On merge_group, GitHub's contract is that the tested commit (github.sha)
    # IS the merge group's head commit -- unlike pull_request, where the
    # tested commit is a separate synthetic merge of base+head (see
    # verify_pull_request_candidate_parents for that case). A mismatch here
    # means the wiring handed the gate an identity that does not describe
    # what was actually checked out and tested.
    if event_name == "merge_group" and head_sha and candidate_sha != head_sha:
        problems.append(
            f"{label}: merge_group candidate sha {candidate_sha!r} does not match head sha {head_sha!r}"
        )
    return problems


def verify_pull_request_candidate_parents(candidate_parents, payload_base_sha, payload_head_sha):
    """Cross-check a pull_request candidate's ACTUAL git parents against the
    event payload's base/head fields.

    Pure and side-effect-free: `candidate_parents` is a list of parent SHAs
    the caller already read (e.g. via `git cat-file -p` on the checked-out
    `github.sha`, which works even at the default fetch-depth 1, since a
    commit object's own parent lines are never truncated by a shallow
    fetch -- only the parent *objects* are absent). On `pull_request`,
    `github.sha` is GitHub's synthetic merge of base+head
    (`refs/pull/N/merge`), not a value trusted at face value from the event
    payload, so this checks what was actually tested rather than what the
    payload merely claims: GitHub's contract is exactly two parents,
    `[base, head]`.

    Returns `(problems, resolved_base_sha, disagreement_message)`.
    `resolved_base_sha` -- the first parent, when there are exactly two --
    is what freshness should compare, since the payload's `base.sha` can lag
    the merge ref (GitHub regenerates that ref as the base branch moves, but
    does not always regenerate the payload alongside it). A parent count
    other than two, or a second parent that disagrees with the payload head,
    is a hard refusal: the checked-out tree is not what the payload claims.
    A first-parent/payload-base disagreement is NOT refused -- merely
    reported -- because it can be an ordinary side effect of that lag rather
    than a wiring defect.
    """
    label = "candidate parents"
    if len(candidate_parents) != 2:
        return (
            [f"{label}: expected exactly 2 (base, head) for a pull_request merge ref, got {len(candidate_parents)}"],
            payload_base_sha,
            None,
        )
    parent_base, parent_head = candidate_parents
    problems = []
    if payload_head_sha and parent_head != payload_head_sha:
        problems.append(
            f"{label}: second parent {parent_head!r} does not match the payload head sha {payload_head_sha!r}"
        )
    disagreement = None
    if payload_base_sha and parent_base != payload_base_sha:
        disagreement = (
            f"{label}: first parent {parent_base!r} disagrees with the payload base sha "
            f"{payload_base_sha!r} (the merge ref may have been regenerated since the payload was recorded)"
        )
    return problems, parent_base, disagreement


def verify_base_freshness(event_name, base_sha, current_base_sha):
    """Compare the tested base SHA against a freshly-read current base SHA.

    Pure and side-effect-free: `current_base_sha` is supplied by the caller,
    never read here. Returns `(problems, message)`. `problems` is non-empty
    only for a `merge_group` run, where the queue is supposed to guarantee
    the tested base is current, so a mismatch -- or either SHA being absent
    -- is a real integrity failure. On any other event (including
    `pull_request`, where `strict: false` and no queue exist -- see Q6 in
    docs/exec-plans/active/distributed-testing.md) the same comparison is
    informational only: `message` reports it, `problems` never fails on it,
    because a PR run going stale while review continues is expected and is
    not something a workflow can enforce; only a branch-protection setting
    (`strict: true`) or a merge queue can.
    """
    if event_name not in _BASE_HEAD_EVENTS:
        return [], None
    label = "base freshness"
    strict = event_name == "merge_group"
    if not base_sha:
        message = f"{label}: no tested base sha recorded for this {event_name} run"
        return ([message] if strict else []), message
    if not current_base_sha:
        message = f"{label}: no --current-base-sha supplied to compare against"
        return ([message] if strict else []), message
    if base_sha != current_base_sha:
        message = (
            f"{label}: tested base {base_sha} does not match current master tip "
            f"{current_base_sha}"
        )
        return ([message] if strict else []), message
    return [], f"{label}: tested base {base_sha} matches current master tip"


def _write_step_summary(line):
    path = os.environ.get("GITHUB_STEP_SUMMARY")
    if not path:
        return
    try:
        with open(path, "a", encoding="utf-8") as handle:
            handle.write(f"- {line}\n")
    except OSError:
        pass


def parse_args(argv):
    parser = argparse.ArgumentParser(description="Evaluate the CI merge gate.")
    parser.add_argument("jobs", nargs="*", help="required job names, matching ci.yml `needs`")
    parser.add_argument("--evidence-report", default=None, help="evidence report JSON the promoted lane uploaded")
    parser.add_argument(
        "--evidence-manifest",
        default=str(SCRIPTS.parent / "test/contracts/scenarios.json"),
        help="committed scenario manifest",
    )
    # These five default to None, not "": None uniquely means "the caller
    # never passed this flag at all" (the two legacy build-and-integration-
    # test* wrapper jobs, which pass none of them). An explicit empty string
    # -- `--candidate-sha ''`, e.g. from a broken template expression -- is a
    # DIFFERENT, deliberately non-exempt case: main() must still run full
    # identity validation (which then correctly refuses on the empty value)
    # rather than silently skipping it, so gating on truthiness alone here
    # is wrong.
    parser.add_argument("--candidate-sha", default=None, help="the commit SHA this run tested")
    parser.add_argument(
        "--event-name", default=None, help="the triggering event (pull_request/merge_group/push)"
    )
    parser.add_argument(
        "--base-sha", default=None,
        help="the candidate's base SHA (pull_request.base.sha / merge_group.base_sha)",
    )
    parser.add_argument(
        "--head-sha", default=None,
        help="the candidate's head SHA (pull_request.head.sha / merge_group.head_sha)",
    )
    parser.add_argument(
        "--current-base-sha", default="",
        help="master's actual current tip, read fresh, for the base-freshness check",
    )
    parser.add_argument(
        "--candidate-parents", default="",
        help=(
            "space-separated parent SHAs of the pull_request candidate commit, read via "
            "`git cat-file -p` (pure-function input only; ci-ok.py performs no I/O)"
        ),
    )
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

    # G7: identity/freshness apply whenever the caller supplies ANY of the
    # four identity flags -- checked by is-None (flag omitted), never by
    # truthiness (which an explicit `--candidate-sha ''` would also satisfy
    # as "absent" and so wrongly skip validation instead of refusing it).
    # Only the two legacy `build-and-integration-test*` wrapper jobs pass
    # none of these flags, ever, and stay exempt.
    identity_flags = (args.candidate_sha, args.event_name, args.base_sha, args.head_sha)
    if any(value is not None for value in identity_flags):
        event_name = args.event_name or ""
        candidate_sha = args.candidate_sha or ""
        base_sha = args.base_sha or ""
        head_sha = args.head_sha or ""
        current_base_sha = args.current_base_sha or ""
        print(
            "ci-ok candidate identity: "
            f"event={event_name!r} sha={candidate_sha!r} "
            f"base={base_sha!r} head={head_sha!r}"
        )
        failed.extend(verify_candidate_identity(event_name, candidate_sha, base_sha, head_sha))

        # pull_request's tested commit is a synthetic merge of base+head
        # (refs/pull/N/merge), not the payload's base.sha at face value --
        # derive the real base from the candidate's own git parents instead.
        freshness_base_sha = base_sha
        if event_name == "pull_request":
            parents = (args.candidate_parents or "").split()
            parent_problems, resolved_base_sha, disagreement = verify_pull_request_candidate_parents(
                parents, base_sha, head_sha,
            )
            failed.extend(parent_problems)
            freshness_base_sha = resolved_base_sha
            if disagreement:
                print(disagreement)
                _write_step_summary(disagreement)

        freshness_problems, freshness_message = verify_base_freshness(
            event_name, freshness_base_sha, current_base_sha,
        )
        failed.extend(freshness_problems)
        if freshness_message:
            print(freshness_message)
            _write_step_summary(freshness_message)

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
