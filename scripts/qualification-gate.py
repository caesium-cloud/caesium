#!/usr/bin/env python3
"""distributed-testing G4: qualification lanes, the release gate, the nightly fold.

One registry (`LANES`) names every lane in
.github/workflows/qualification-lanes.yml, its dispatch token, the manifest
gate its report must pass, and its class:

- ``release-required`` -- run by the release chain on the tagged commit and
  required by `publish` (F2 cluster lifecycle, D3 console recovery, B3 core
  robustness with the instrumented server);
- ``nightly-required`` -- scheduled, failing a night when red, but not part of
  the publish chain (the long C2 fuzz campaign);
- ``advisory`` -- scheduled, its verdict recorded and reported, never required
  (E4 performance: the hosted fleet is uncalibrated; F3 soak: known-failing on
  the product findings #598-#603). An advisory lane fails a night only when it
  recorded NO verdict, i.e. its harness broke.

Subcommands (stdlib only, no network):

``lanes --validate SELECTION``
    Refuse a selection naming a lane that does not exist (a typo would
    otherwise run nothing and report green).

``release``
    The publish gate, fail-closed. Required to publish, all bound to the
    candidate SHA this run built and tested (`RELEASE_REQUIRED_JOBS`,
    `RELEASE_REQUIRED_EVIDENCE`, `CLI_ARCHES`):

    * `changes`, `ci-ok`, `images`, `images-arm64` and the `qualification`
      call succeeded (`ci-ok` itself re-validates every promoted merge lane,
      F4 among them);
    * each release-required lane's own job succeeded (the call's `results`);
    * the F4 standalone lifecycle report and the three release-required lane
      reports bind to the candidate SHA and pass their manifest gates
      strictly (ci-ok.py's `verify_evidence`, which re-runs
      check-test-evidence.py);
    * both native CLI binaries carry the smoke marker their own-architecture
      runner wrote, with a matching sha256 and native runner arch;
    * outside a dry run, the ref is a `v*` tag.

    Anything absent, foreign, failed, skipped or hollow refuses.

``summary``
    Fold one nightly run (the lanes' `results`, every downloaded report and
    the soak record) into a Markdown summary, and name the failing lanes for
    the single triage issue comment. Nightly results are broader evidence,
    never proof for an individual PR.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import runpy
import sys
from collections import namedtuple
from pathlib import Path


SCRIPTS = Path(__file__).resolve().parent
CI_OK = runpy.run_path(str(SCRIPTS / "ci-ok.py"))
verify_evidence = CI_OK["verify_evidence"]

RELEASE = "release-required"
NIGHTLY = "nightly-required"
ADVISORY = "advisory"

ALL = "all"

# The open product findings the soak fails on (F3 note, W9-epsilon).
SOAK_KNOWN_FINDINGS = ("#598", "#599", "#600", "#601", "#602", "#603")

Lane = namedtuple("Lane", "token gate role artifact title reproduce")

# job id in qualification-lanes.yml -> Lane. `artifact` is the report artifact
# name (performance-gate fans out a matrix, so its reports are
# `performance-gate-evidence-<run>`). `reproduce` is the one-command local
# reproduction on a clean checkout of the candidate (hold the host lane lock).
LANES = {
    "lifecycle-cluster": Lane(
        "cluster-lifecycle", "nightly-cluster-lifecycle", RELEASE, "lifecycle-cluster-evidence",
        "F2 persistent three-voter cluster lifecycle",
        "just lifecycle-cluster"),
    "console-recovery": Lane(
        "console-recovery", "nightly-console", RELEASE, "console-recovery-evidence",
        "D3 console cluster-recovery journey",
        "just tag=<sha> console-recovery"),
    "core-robustness-nightly": Lane(
        "core-robustness", "nightly-core", RELEASE, "core-robustness-nightly-evidence",
        "B3 fenced TestCore with the instrumented server",
        "just tag=<sha> robustness-instrumented && CAESIUM_ROBUSTNESS_INSTRUMENTED_IMAGE="
        "caesiumcloud/caesium:<sha>-testfault just tag=<sha> core-robustness nightly-core"),
    "fuzz-campaign": Lane(
        "fuzz-campaign", "generated-fuzz", NIGHTLY, "fuzz-campaign-evidence",
        "C2 native fuzz targets at the long budget",
        "CAESIUM_FUZZ_SECONDS=<fuzz-seconds> just tag=<sha> generated-fuzz"),
    "performance-gate": Lane(
        "performance", None, ADVISORY, "performance-gate-evidence",
        "E4 performance gate (advisory: the hosted fleet is uncalibrated)",
        "just performance-gate advisory"),
    "soak": Lane(
        "soak", None, ADVISORY, "soak-evidence",
        "F3 seeded single-host soak (advisory, known-failing: " + ", ".join(SOAK_KNOWN_FINDINGS) + ")",
        "CAESIUM_SOAK_SEED=<seed> just soak-tests <profile> advisory"),
}

TOKENS = {lane.token: job for job, lane in LANES.items()}

# What the release chain runs (ci.yml `qualification` passes exactly this).
RELEASE_LANES = tuple(job for job, lane in LANES.items() if lane.role == RELEASE)
RELEASE_LANE_SELECTION = ",".join(LANES[job].token for job in RELEASE_LANES)

# ci.yml jobs whose own result must be `success` before anything is published.
RELEASE_REQUIRED_JOBS = ("changes", "ci-ok", "images", "images-arm64", "qualification")

# Report artifact (one directory per artifact under --evidence-dir) -> gate.
RELEASE_REQUIRED_EVIDENCE = {
    "lifecycle-standalone": ("lifecycle-standalone-evidence", "lifecycle"),
    **{job: (LANES[job].artifact, LANES[job].gate) for job in RELEASE_LANES},
}

# Native CLI smoke gate (scripts/ci-cli-smoke.sh): binary -> runner `uname -m`.
CLI_ARCHES = {"amd64": "x86_64", "arm64": "aarch64"}

# Lanes that exist but are never required to publish, and why.
NOT_REQUIRED_TO_PUBLISH = {
    job: lane.title for job, lane in LANES.items() if lane.role != RELEASE
}

_SHA = re.compile(r"^[0-9a-f]{40}$")


def parse_selection(selection):
    """Lane tokens named by a comma-separated selection (order kept, deduplicated)."""
    tokens = []
    for raw in (selection or "").split(","):
        token = raw.strip()
        if token and token not in tokens:
            tokens.append(token)
    return tokens


def selection_problems(selection):
    tokens = parse_selection(selection)
    if not tokens:
        return ["no lanes selected"]
    known = set(TOKENS) | {ALL}
    return [f"unknown lane {token!r} (known: {', '.join(sorted(known))})"
            for token in tokens if token not in known]


def selected_jobs(selection):
    tokens = parse_selection(selection)
    if ALL in tokens:
        return list(LANES)
    return [TOKENS[token] for token in tokens if token in TOKENS]


def parse_results(raw):
    """The `results` output of qualification-lanes.yml: {job: {"result": ...}}."""
    if not raw:
        return None, "lane results are empty (the qualification call produced no `results` output)"
    try:
        value = json.loads(raw)
    except ValueError as err:
        return None, f"lane results are not JSON: {err}"
    if not isinstance(value, dict):
        return None, "lane results must be a JSON object"
    return value, None


def job_result(results, job):
    entry = results.get(job) if isinstance(results, dict) else None
    if isinstance(entry, dict) and isinstance(entry.get("result"), str):
        return entry["result"]
    return "missing"


def read_marker(path):
    fields = {}
    for line in path.read_text().splitlines():
        key, sep, value = line.partition("=")
        if sep:
            fields[key.strip()] = value.strip()
    return fields


def verify_cli(cli_dir):
    """The native CLI smoke/checksum gate, re-checked before anything publishes."""
    problems, lines = [], []
    for arch, machine in CLI_ARCHES.items():
        name = f"caesium-linux-{arch}"
        binary = Path(cli_dir) / name
        marker = Path(cli_dir) / f"{name}.smoke-ok"
        if not binary.is_file():
            problems.append(f"native CLI {arch}: {binary} is absent")
            continue
        if not marker.is_file():
            problems.append(f"native CLI {arch}: {marker.name} is absent; the {arch} build never smoke-tested it")
            continue
        try:
            fields = read_marker(marker)
        except OSError as err:
            problems.append(f"native CLI {arch}: unreadable marker: {err}")
            continue
        actual = hashlib.sha256(binary.read_bytes()).hexdigest()
        if fields.get("binary") != name:
            problems.append(f"native CLI {arch}: marker names binary {fields.get('binary')!r}, want {name!r}")
        if fields.get("sha256") != actual:
            problems.append(f"native CLI {arch}: marker sha256 {fields.get('sha256')!r} != binary sha256 {actual!r}")
        if fields.get("runner_arch") != machine:
            problems.append(
                f"native CLI {arch}: smoke ran on {fields.get('runner_arch')!r}, not natively on {machine!r}")
        lines.append(f"native CLI {arch}: sha256 {actual}, smoke on {fields.get('runner_arch')}")
    return problems, lines


def _write_step_summary(text):
    path = os.environ.get("GITHUB_STEP_SUMMARY")
    if not path:
        return
    try:
        with open(path, "a", encoding="utf-8") as handle:
            handle.write(text + "\n")
    except OSError:
        pass


def release_gate(args, needs_raw, results_raw):
    """Return (problems, report lines) for the publish gate."""
    problems, lines = [], []
    sha = args.candidate_sha or ""
    if not _SHA.match(sha):
        problems.append(f"candidate sha {sha!r} is not a full commit SHA")
    dry_run = args.dry_run == "true"
    if args.dry_run not in ("true", "false"):
        problems.append(f"--dry-run must be true or false, got {args.dry_run!r}")
    elif not dry_run and not (args.ref or "").startswith("refs/tags/v"):
        problems.append(f"ref {args.ref!r} is not a v* tag; only a tag (or a dry run) can publish")
    lines.append(f"candidate {sha} ref {args.ref} dry_run={dry_run}")

    try:
        needs = json.loads(needs_raw or "")
    except ValueError:
        needs = None
    if not isinstance(needs, dict):
        problems.append("NEEDS_JSON is missing or not a JSON object")
        needs = {}
    for job in RELEASE_REQUIRED_JOBS:
        entry = needs.get(job)
        result = entry.get("result", "missing") if isinstance(entry, dict) else "missing"
        lines.append(f"job {job}: {result}")
        if result != "success":
            problems.append(f"{job}={result}")

    results, error = parse_results(results_raw)
    if error:
        problems.append(error)
        results = {}
    for job in RELEASE_LANES:
        result = job_result(results, job)
        lines.append(f"lane {job}: {result}")
        if result != "success":
            problems.append(f"release lane {job}={result}")

    for job, (artifact, gate) in RELEASE_REQUIRED_EVIDENCE.items():
        report = Path(args.evidence_dir) / artifact / "evidence.json"
        found = verify_evidence(str(report), args.manifest, sha, job=job, gate=gate)
        lines.append(f"evidence {job} (gate {gate}): {'pass' if not found else 'REFUSED'}")
        problems.extend(found)

    cli_problems, cli_lines = verify_cli(args.cli_dir)
    problems.extend(cli_problems)
    lines.extend(cli_lines)

    for job, why in NOT_REQUIRED_TO_PUBLISH.items():
        lines.append(f"not required to publish: {job} -- {why}")
    return problems, lines


def cmd_lanes(args):
    problems = selection_problems(args.validate)
    if problems:
        for problem in problems:
            print(f"qualification lanes: {problem}", file=sys.stderr)
        return 1
    print("qualification lanes: " + ", ".join(selected_jobs(args.validate)))
    return 0


def cmd_release(args):
    problems, lines = release_gate(args, os.environ.get("NEEDS_JSON", ""),
                                   os.environ.get("LANE_RESULTS_JSON", ""))
    print("release qualification:")
    for line in lines:
        print(f"  {line}")
    verdict = "BLOCKED" if problems else "allowed"
    summary = [f"### Release qualification: publication {verdict}", ""]
    summary += [f"- {line}" for line in lines]
    if problems:
        summary += ["", "Refusals:"] + [f"- {problem}" for problem in problems]
    _write_step_summary("\n".join(summary))
    if problems:
        print("release qualification failed (publication blocked): " + "; ".join(problems), file=sys.stderr)
        return 1
    print("release qualification passed (publication allowed)")
    return 0


def _soak_verdict(record_path, sha):
    try:
        record = json.loads(Path(record_path).read_text())
    except FileNotFoundError:
        return None, f"{record_path} is absent; the soak recorded no verdict"
    except (OSError, ValueError) as err:
        return None, f"unreadable soak record {record_path}: {err}"
    if not isinstance(record, dict):
        return None, "soak record must be an object"
    if record.get("candidate_sha") != sha:
        return None, f"soak record candidate_sha {record.get('candidate_sha')!r} is not {sha!r}"
    if record.get("result") not in ("pass", "fail", "blocked"):
        return None, f"soak record result {record.get('result')!r} is not a verdict"
    return record, None


def _performance_verdicts(evidence_dir):
    verdicts = []
    for report in sorted(Path(evidence_dir).glob("performance-gate-evidence-*/evidence.json")):
        try:
            row = json.loads(report.read_text())["scenarios"][0]
        except (OSError, ValueError, KeyError, IndexError, TypeError):
            verdicts.append(f"{report.parent.name}: unreadable")
            continue
        perf = row.get("performance") or {}
        verdicts.append(f"{report.parent.name}: row {row.get('status')}, overall {perf.get('overall')}")
    return verdicts


def summarize(args, results_raw):
    """Return (markdown, failing lane list)."""
    sha = args.candidate_sha or ""
    failing = []
    rows = []
    results, error = parse_results(results_raw)
    if error:
        failing.append("results")
        rows.append(("results", "FAIL", f"{error} (qualification call: {args.qualification_result})", ""))
        results = {}
    select = job_result(results, "select") if results else "missing"
    if results and select != "success":
        failing.append("select")
        rows.append(("select", "FAIL", f"lane selection {args.lanes!r} was refused ({select})", ""))
    chosen = set(selected_jobs(args.lanes))
    for job, lane in LANES.items():
        reproduce = lane.reproduce.replace("<sha>", sha).replace("<fuzz-seconds>", args.fuzz_seconds)
        if job not in chosen:
            rows.append((job, "not selected", lane.title, ""))
            continue
        result = job_result(results, job)
        if lane.role != ADVISORY:
            if result != "success":
                failing.append(job)
                rows.append((job, "FAIL", f"{lane.title}: job {result}", reproduce))
                continue
            report = Path(args.evidence_dir) / lane.artifact / "evidence.json"
            found = verify_evidence(str(report), args.manifest, sha, job=job, gate=lane.gate)
            if found:
                failing.append(job)
                rows.append((job, "FAIL", f"{lane.title}: " + "; ".join(found), reproduce))
            else:
                rows.append((job, "pass", f"{lane.title} (gate {lane.gate})", reproduce))
            continue
        if result != "success":
            failing.append(job)
            rows.append((job, "FAIL", f"{lane.title}: no verdict recorded (job {result})", reproduce))
            continue
        if job == "soak":
            record, problem = _soak_verdict(Path(args.evidence_dir) / lane.artifact / "soak.json", sha)
            if problem:
                failing.append(job)
                rows.append((job, "FAIL", f"{lane.title}: {problem}", reproduce))
                continue
            seed = record.get("seed")
            profile = record.get("profile") or args.soak_profile
            reproduce = lane.reproduce.replace("<seed>", str(seed)).replace("<profile>", str(profile))
            detail = (f"{lane.title}: result {record.get('result')}, profile {profile}, seed {seed} "
                      f"({record.get('seed_source')}), failed gates "
                      f"{', '.join(record.get('failed_gates') or []) or 'none'}")
            rows.append((job, f"advisory: {record.get('result')}", detail, reproduce))
            continue
        verdicts = _performance_verdicts(args.evidence_dir)
        if not verdicts:
            failing.append(job)
            rows.append((job, "FAIL", f"{lane.title}: job succeeded but left no report", reproduce))
            continue
        rows.append((job, "advisory: recorded", f"{lane.title}: " + "; ".join(verdicts), reproduce))

    verdict = "FAILING" if failing else "green"
    out = [
        f"## Nightly qualification: {verdict}",
        "",
        f"Candidate `{sha}` ({args.run_url}). Lanes requested: `{args.lanes}`.",
        "Nightly results are broader evidence about this commit, never proof for an individual PR.",
        "",
        "| Lane | Verdict | Detail | Reproduce (clean checkout of the candidate, host lane lock) |",
        "| --- | --- | --- | --- |",
    ]
    for job, status, detail, reproduce in rows:
        detail = detail.replace("|", "\\|").replace("\n", " ")
        out.append(f"| `{job}` | {status} | {detail} | {('`' + reproduce + '`') if reproduce else ''} |")
    out += [
        "",
        f"Triage owner: {args.triage_owner or 'the CODEOWNERS default owners'}. A failing scheduled night adds "
        "ONE comment to the single open issue titled `Nightly qualification failures` (opened if none is open). "
        "Advisory verdicts (performance, soak) are reported here and never fail a night unless no verdict was "
        "recorded. The soak is known-failing on " + ", ".join(SOAK_KNOWN_FINDINGS) + ".",
    ]
    return "\n".join(out), failing


def codeowners_default(path):
    """Owners of the `*` rule in a CODEOWNERS file (last match wins, like GitHub)."""
    owners = ""
    try:
        text = Path(path).read_text()
    except OSError:
        return ""
    for line in text.splitlines():
        parts = line.split("#", 1)[0].split()
        if parts and parts[0] == "*":
            owners = " ".join(parts[1:])
    return owners


def cmd_summary(args):
    if not args.triage_owner and args.codeowners:
        args.triage_owner = codeowners_default(args.codeowners)
    markdown, failing = summarize(args, os.environ.get("LANE_RESULTS_JSON", ""))
    print(markdown)
    _write_step_summary(markdown)
    if args.markdown_out:
        Path(args.markdown_out).write_text(markdown + "\n")
    output = os.environ.get("GITHUB_OUTPUT")
    if output:
        with open(output, "a", encoding="utf-8") as handle:
            handle.write(f"failing={','.join(failing)}\n")
            handle.write(f"triage_owner={args.triage_owner or ''}\n")
    return 0


def parse_args(argv):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = parser.add_subparsers(dest="command", required=True)

    lanes = sub.add_parser("lanes", help="validate a lane selection")
    lanes.add_argument("--validate", required=True, metavar="SELECTION")

    release = sub.add_parser("release", help="the publish gate (NEEDS_JSON, LANE_RESULTS_JSON)")
    release.add_argument("--candidate-sha", required=True)
    release.add_argument("--ref", required=True)
    release.add_argument("--dry-run", required=True, help="true or false")
    release.add_argument("--evidence-dir", required=True, help="one directory per downloaded *-evidence artifact")
    release.add_argument("--cli-dir", required=True, help="the release-cli-* binaries and .smoke-ok markers")
    release.add_argument("--manifest", default=str(SCRIPTS.parent / "test/contracts/scenarios.json"))

    summary = sub.add_parser("summary", help="fold a nightly run (LANE_RESULTS_JSON)")
    summary.add_argument("--lanes", required=True)
    summary.add_argument("--candidate-sha", required=True)
    summary.add_argument("--evidence-dir", required=True)
    summary.add_argument("--manifest", default=str(SCRIPTS.parent / "test/contracts/scenarios.json"))
    summary.add_argument("--run-url", default="")
    summary.add_argument("--qualification-result", default="unknown")
    summary.add_argument("--fuzz-seconds", default="5m")
    summary.add_argument("--soak-profile", default="short")
    summary.add_argument("--triage-owner", default="")
    summary.add_argument("--codeowners", default="")
    summary.add_argument("--markdown-out", default="")
    return parser.parse_args(argv)


def main(argv=None):
    args = parse_args(sys.argv[1:] if argv is None else argv)
    return {"lanes": cmd_lanes, "release": cmd_release, "summary": cmd_summary}[args.command](args)


if __name__ == "__main__":
    raise SystemExit(main())
