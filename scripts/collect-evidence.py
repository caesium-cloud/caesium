#!/usr/bin/env python3
"""Turn real lane artifacts into a scenario evidence report.

This is the *artifact consumer* half of the early-evidence lane. It never
invents an observation: every field it emits is read out of a file the lane
actually produced (kind/Helm artifacts under `CAESIUM_ROBUSTNESS_ARTIFACTS`, a
`docker inspect` of the live integration server, a retained `/metrics` scrape,
and the Go test logs). When an input is missing or unreadable the scenario is
reported `inconclusive` — never `pass` — so `scripts/check-test-evidence.py`
fails or reports inconclusive instead of silently accepting a hollow lane.

Subcommands:

  robustness       Build fragments for the B1 three-node owner-crash scenarios
                   from `scripts/robustness.sh` artifacts.
  sql-work-budget  Build the fragment for E5's existing-suite SQL-work budget
                   from the integration server inspect + /metrics + test log.
  report           Merge fragments into one evidence report for
                   `scripts/check-test-evidence.py --report`.
  redact           Strip the lane's generated internal token and kubeconfigs
                   from an artifact directory before it is uploaded.

Dependency-free (Python 3 stdlib only), like the checker it feeds.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path


# Env names whose *values* must never reach an uploaded evidence report.
SECRET_ENV = re.compile(r"(TOKEN|KEY|SECRET|PASSWORD|PASSPHRASE|CREDENTIAL|DSN)")

OWNER_CRASH_SUBTESTS = (
    ("b1-owner-crash-leader", "owner_is_leader"),
    ("b1-owner-crash-nonleader", "owner_is_not_leader"),
)
OWNER_CRASH_IDENTITY = "robustness-owner-crash"
OWNER_CRASH_FAULT_KIND = "owner-sigkill"
OWNER_CRASH_TOPOLOGY_KIND = "kind-helm-statefulset"

SQL_BUDGET_ID = "e5-sql-work-budget"
SQL_BUDGET_IDENTITY = "integration-sql-work-budget"
SQL_BUDGET_TOPOLOGY_KIND = "integration-existing"
SQL_BUDGET_SELECTOR = "TestIntegrationTestSuite/TestStatementBudgetFixedWorkload"

DIGEST = re.compile(r"^sha256:[0-9a-f]{64}$")
SHA = re.compile(r"^[0-9a-fA-F]{40}$|^[0-9a-fA-F]{64}$")


class EvidenceError(Exception):
    """A lane input is missing or malformed; the caller must fail closed."""


def read_text(path):
    try:
        return Path(path).read_text()
    except OSError as err:
        raise EvidenceError(f"cannot read {path}: {err}") from err


def read_json(path):
    raw = read_text(path)
    try:
        return json.loads(raw)
    except json.JSONDecodeError as err:
        raise EvidenceError(f"{path} is not JSON: {err}") from err


def parse_env_lines(lines):
    """Parse KEY=VALUE lines, dropping values of secret-looking names."""
    env = {}
    for line in lines:
        line = line.strip()
        if not line or "=" not in line:
            continue
        key, _, value = line.partition("=")
        key = key.strip()
        if not key or SECRET_ENV.search(key):
            continue
        env[key] = value
    return env


def agreed_env(per_member):
    """Keys every member reports with the same value.

    Per-pod values (CAESIUM_NODE_ADDRESS) legitimately differ and are dropped;
    a cluster-wide flag that disagrees between members is not a fact about the
    cluster, so it must not be reported as one.
    """
    if not per_member:
        raise EvidenceError("no member environments were captured")
    keys = set(per_member[0])
    for env in per_member[1:]:
        keys &= set(env)
    out = {}
    for key in sorted(keys):
        values = {env[key] for env in per_member}
        if len(values) == 1:
            out[key] = values.pop()
    return out


def _fragment(candidate_sha, candidate_digest, scenarios):
    """A fragment always names the candidate commit; the digest is optional.

    Only a lane that runs the candidate *release* image can claim the report's
    candidate digest. The Docker integration lane runs the `-test` variant of
    the same build, so it records the image it actually ran instead of
    borrowing an identity it cannot demonstrate.
    """
    if not SHA.match(candidate_sha or ""):
        raise EvidenceError(f"candidate SHA is not a commit SHA: {candidate_sha!r}")
    fragment = {"candidate_sha": candidate_sha, "scenarios": scenarios}
    if candidate_digest is not None:
        if not DIGEST.match(candidate_digest):
            raise EvidenceError(f"candidate digest is not sha256:<64 hex>: {candidate_digest!r}")
        fragment["candidate_digest"] = candidate_digest
    return fragment


# --------------------------------------------------------------------------
# B1 three-node owner-crash lane
# --------------------------------------------------------------------------

def _go_outcome(log, selector):
    """Return pass/fail/None for a Go test selector in a `go test -v` log."""
    if re.search(rf"^\s*--- PASS: {re.escape(selector)}(\s|\()", log, re.M):
        return "pass"
    if re.search(rf"^\s*--- FAIL: {re.escape(selector)}(\s|\()", log, re.M):
        return "fail"
    return None


def _kind_nodes(artifacts):
    """Roles declared in the kind config this run actually created."""
    config = read_text(artifacts / "kind.yaml")
    roles = re.findall(r"^\s*-\s*role:\s*(\S+)\s*$", config, re.M)
    control = sum(1 for role in roles if role == "control-plane")
    workers = sum(1 for role in roles if role == "worker")
    if control < 1 or workers < 1:
        raise EvidenceError(f"kind.yaml declares no usable topology: {roles}")
    return control, workers


def _observed_members(artifacts):
    """Per-pod CAESIUM_* environments dumped from the running StatefulSet."""
    envs = []
    for path in sorted(artifacts.glob("caesium-*.env")):
        lines = path.read_text().splitlines()
        if not lines:
            raise EvidenceError(f"{path} is empty; pod environment was not captured")
        envs.append(parse_env_lines(lines))
    if not envs:
        raise EvidenceError("no caesium-*.env dumps in the artifact directory")
    return envs


def _persistent_claims(artifacts):
    described = read_text(artifacts / "describe-ns-pods.txt")
    return sorted(set(re.findall(r"^\s*ClaimName:\s*(data-caesium-\d+)\s*$", described, re.M)))


def robustness_topology(artifacts, env):
    control, workers = _kind_nodes(artifacts)
    members = _observed_members(artifacts)
    claims = _persistent_claims(artifacts)
    if len(claims) != len(members):
        raise EvidenceError(
            f"observed {len(members)} members but {len(claims)} bound data claims {claims}"
        )
    if "CAESIUM_KUBERNETES_NAMESPACE" not in env:
        raise EvidenceError("members do not report the Kubernetes engine namespace")

    def number(name):
        raw = env.get(name)
        if raw is None or not raw.strip().isdigit():
            raise EvidenceError(f"members do not agree on a numeric {name} ({raw!r})")
        return int(raw)

    return {
        "kind": OWNER_CRASH_TOPOLOGY_KIND,
        "replicas": len(members),
        "persistence": bool(claims),
        "engine": "kubernetes",
        "control_plane_nodes": control,
        "worker_nodes": workers,
        "voters": number("CAESIUM_DATABASE_VOTERS"),
        "standbys": number("CAESIUM_DATABASE_STANDBYS"),
        "database_shards": number("CAESIUM_DATABASE_SHARDS"),
    }


def observed_mode(env, surface):
    owner = "enabled" if env.get("CAESIUM_RUN_OWNER_ENABLED", "false") == "true" else "disabled"
    return {
        "execution": env.get("CAESIUM_EXECUTION_MODE", "local"),
        "owner": owner,
        "surface": surface,
    }


def _owner_crash_observations(record, events, parent_log):
    """Map the retained record/recorder evidence onto the manifest observation IDs.

    `runOwnerCrash` writes its record only after every assertion in the scenario
    has held, so a record plus the subtest PASS line is real evidence for the
    assertions that leave no separate field behind. Everything that *does* leave
    a field behind is checked against that field here.
    """
    observed = []
    run_id = str(record.get("run_id") or "")
    before = record.get("lease_before") or {}
    after = record.get("lease_after") or {}
    owner_address = str(record.get("owner_address") or "")
    generation_grew = (
        isinstance(before.get("Generation"), int)
        and isinstance(after.get("Generation"), int)
        and after["Generation"] > before["Generation"]
    )
    took_over = bool(after.get("OwnerNode")) and after.get("OwnerNode") != owner_address
    succeeded = str(record.get("public_status", "")).lower() == "succeeded"

    if run_id:
        observed.append("http_202_with_run_uuid")
    if run_id and succeeded:
        observed.append("run_uuid_publicly_readable_after_fault")
        observed.append("run_uuid_bound_to_same_job")
    if generation_grew and took_over:
        observed.append("authoritative_lease_generation_increased")
    if generation_grew and took_over and succeeded and after.get("RunID") == run_id:
        observed.append("surviving_owner_completed_same_run")
    if record.get("successor_starts", 0) >= 1 and record.get("successor_complete", 0) >= 1:
        observed.append("legal_successors_executed")
    if (
        record.get("block_completions", 0) >= 1
        and record.get("successor_complete", 0) >= 1
        and succeeded
    ):
        observed.append("public_state_matches_raw_effects")
    # The rejoin poll is the last gate before the record is written.
    observed.append("restarted_member_rejoined_retained_volume")
    match = re.search(r"dqlite leader=(\S+) members=(\d+)", parent_log)
    if match and int(match.group(2)) >= 3:
        observed.append("three_dqlite_voters_and_leader")
    if any(event.get("run_id") == run_id and event.get("kind") == "start" for event in events):
        observed.append("fixture_task_on_surviving_worker")
    return observed


CTR_ERROR_MARKERS = (
    "failed to dial", "connection refused", "cannot connect", "no such file or directory",
    "permission denied", "i/o timeout", "deadline exceeded", "rpc error", "unavailable",
    "transport is closing", "error response from daemon",
)


def _ctr_listing_ok(listing):
    """Same validity rule as test/robustness/kill_evidence.go.

    An error string from `ctr` is not proof of anything; only a real task
    listing (TASK plus PID or STATUS) can be read as evidence.
    """
    stripped = listing.strip()
    if not stripped:
        return False
    header = next((line for line in listing.splitlines() if line.strip()), "")
    tokens = {token.lower() for token in header.split()}
    if "task" not in tokens or not ({"pid", "status"} & tokens):
        return False
    lower = stripped.lower()
    if lower.startswith("ctr:") or "error:" in lower[:120]:
        return False
    return not any(marker in lower for marker in CTR_ERROR_MARKERS)


def _task_dead(container_id, listing):
    """Mirror taskDeadFromListing: stopped, or absent from a valid listing.

    The host controller refuses to kill unless the container is in the listing
    beforehand, so absence afterwards is death, not a missing observation.
    """
    container_id = (container_id or "").strip()
    short = container_id[:12]
    if len(short) < 8 or not _ctr_listing_ok(listing):
        return False
    for line in listing.splitlines():
        if container_id not in line and short not in line:
            continue
        lower = line.lower()
        if "running" in lower:
            return False
        return any(word in lower for word in ("stopped", "exited", "killed"))
    return True


def _owner_crash_fault(record, events, log, subtest):
    run_id = str(record.get("run_id") or "")
    evidence = str(record.get("kill_evidence") or "")
    node = str(record.get("owner_node") or "")
    observed = []

    section = log.split(f"=== RUN   TestOwnerCrash/{subtest}", 1)
    body = section[1] if len(section) > 1 else ""
    cordon = body.find("cordon ack: cordoned")
    admitted = body.find("admitted run")
    if cordon != -1 and (admitted == -1 or cordon < admitted):
        observed.append("owner_kind_node_cordoned_before_fixture")
    killed = re.search(r"^ctr kill (\S+)$", evidence, re.M)
    if killed:
        observed.append("owner_container_sigkill")
    listing = "\n".join(
        line for line in evidence.splitlines()
        if not line.strip().lower().startswith(("kubelet stopped", "ctr kill"))
    )
    # The test only reaches sink.Release after the kill evidence shows death,
    # so a retained release event plus a dead task orders the two.
    released = any(
        event.get("kind") == "release" and event.get("run_id") == run_id for event in events
    )
    if released and killed and _task_dead(killed.group(1), listing):
        observed.append("process_exit_before_sink_barrier_release")
    if node and f"kubelet stopped on {node}" in evidence:
        observed.append("kubelet_stopped_during_survivor_takeover")
    return {
        "activated": bool(observed),
        "kind": OWNER_CRASH_FAULT_KIND,
        "observations": observed,
    }


def build_robustness(artifacts, candidate_sha=None):
    artifacts = Path(artifacts)
    versions = read_json(artifacts / "versions.json")
    sha = candidate_sha or versions.get("candidate_sha")
    digest = read_text(artifacts / "candidate-digest.txt").strip()
    log = read_text(artifacts / "robustness.test.log")
    parent_log = log.split("=== RUN   TestOwnerCrash/", 1)[0]
    members = _observed_members(artifacts)
    env = agreed_env(members)
    topology = robustness_topology(artifacts, env)
    mode = observed_mode(env, "http")

    try:
        events = read_json(artifacts / "records" / "events.json")
        if not isinstance(events, list):
            raise EvidenceError("records/events.json is not a list")
    except EvidenceError:
        events = None

    scenarios = []
    for scenario_id, subtest in OWNER_CRASH_SUBTESTS:
        entry = {
            "id": scenario_id,
            "artifact_identity": OWNER_CRASH_IDENTITY,
            "candidate_sha": sha,
            "candidate_digest": digest,
            "topology": topology,
            "mode": mode,
            "feature_flags": env,
            "checker": {"timed_out": False},
        }
        outcome = _go_outcome(log, f"TestOwnerCrash/{subtest}")
        try:
            record = read_json(artifacts / "records" / f"{subtest}.json")
        except EvidenceError:
            record = None
        if record is None or events is None:
            entry["status"] = "inconclusive"
            entry["recorder"] = {"present": False, "sample_count": 0}
            entry["observations"] = []
            entry["fault_activation"] = {"activated": False, "kind": OWNER_CRASH_FAULT_KIND,
                                         "observations": []}
            scenarios.append(entry)
            continue
        run_id = str(record.get("run_id") or "")
        samples = [event for event in events if event.get("run_id") == run_id]
        entry["recorder"] = {"present": True, "sample_count": len(samples)}
        entry["observations"] = _owner_crash_observations(record, events, parent_log)
        entry["fault_activation"] = _owner_crash_fault(record, events, log, subtest)
        entry["status"] = {"pass": "pass", "fail": "fail"}.get(outcome, "inconclusive")
        scenarios.append(entry)
    return _fragment(sha, digest, scenarios)


# --------------------------------------------------------------------------
# E5 SQL-work budget on the existing integration suite
# --------------------------------------------------------------------------

def _metric_samples(text, metric):
    pattern = re.compile(rf"^{re.escape(metric)}\{{[^}}]*\}}\s+\S+$", re.M)
    return pattern.findall(text)


def build_sql_work_budget(inspect_path, metrics_path, log_path, candidate_sha, server_image_id):
    containers = read_json(inspect_path)
    if not isinstance(containers, list) or not containers:
        raise EvidenceError(f"{inspect_path} is not a docker inspect array")
    container = containers[0]
    env = parse_env_lines((container.get("Config") or {}).get("Env") or [])
    # Persistence means the database survives the container, not that some
    # unrelated anonymous volume (dind's /var/lib/docker) exists.
    database = env.get("CAESIUM_DATABASE_PATH", "/var/lib/caesium/dqlite").rstrip("/")
    data_mounts = [
        mount for mount in container.get("Mounts") or []
        if (database + "/").startswith(str(mount.get("Destination", "")).rstrip("/") + "/")
    ]
    if "CAESIUM_KUBERNETES_NAMESPACE" in env:
        engine = "kubernetes"
    elif any(key == "DOCKER_HOST" for key in env):
        engine = "docker"
    else:
        raise EvidenceError("integration server reports neither a Kubernetes namespace nor DOCKER_HOST")
    shards = env.get("CAESIUM_DATABASE_SHARDS", "")
    if not shards.isdigit():
        raise EvidenceError(f"integration server has no numeric CAESIUM_DATABASE_SHARDS ({shards!r})")

    log = read_text(log_path)
    outcome = _go_outcome(log, SQL_BUDGET_SELECTOR)
    metrics = read_text(metrics_path)
    statements = _metric_samples(metrics, "caesium_db_statements_total")
    writes = _metric_samples(metrics, "caesium_db_writes_total")

    observations = []
    if outcome == "pass":
        observations.append("workload_completed_successfully")
        if statements:
            observations.append("statement_counter_delta_within_budget")
        if writes:
            observations.append("write_counter_delta_within_budget")

    if not DIGEST.match(server_image_id or ""):
        raise EvidenceError(f"server image id is not sha256:<64 hex>: {server_image_id!r}")
    entry = {
        "id": SQL_BUDGET_ID,
        "status": {"pass": "pass", "fail": "fail"}.get(outcome, "inconclusive"),
        "artifact_identity": SQL_BUDGET_IDENTITY,
        "candidate_sha": candidate_sha,
        # Not `candidate_digest`: this lane runs the `-test` image variant, so
        # it names the artifact it ran rather than the release digest.
        "observed_server_image_id": server_image_id,
        "topology": {
            "kind": SQL_BUDGET_TOPOLOGY_KIND,
            "replicas": len(containers),
            "persistence": bool(data_mounts),
            "engine": engine,
            "database_shards": int(shards),
        },
        "mode": observed_mode(env, "http+cli"),
        # Only the product's own configuration; the base image's PATH and
        # docker-in-docker build args are not Caesium feature flags.
        "feature_flags": {k: v for k, v in env.items() if k.startswith("CAESIUM_")},
        "observations": observations,
        # No fault is injected here; the checker rejects an empty `kind`, so
        # the key is omitted rather than filled with a placeholder.
        "fault_activation": {"activated": False, "observations": []},
        "recorder": {"present": bool(statements and writes),
                     "sample_count": len(statements) + len(writes)},
        "checker": {"timed_out": False},
    }
    return _fragment(candidate_sha, None, [entry])


# --------------------------------------------------------------------------
# Report assembly and artifact redaction
# --------------------------------------------------------------------------

def build_report(fragment_paths, gate_enabled=True, disabled_gates=()):
    if not fragment_paths:
        raise EvidenceError("no evidence fragments were given")
    sha = digest = None
    scenarios = []
    seen = set()
    for path in fragment_paths:
        if not Path(path).is_file():
            raise EvidenceError(f"missing evidence fragment {path}; the lane did not produce it")
        fragment = read_json(path)
        if not isinstance(fragment, dict):
            raise EvidenceError(f"{path} is not an evidence fragment object")
        value = fragment.get("candidate_sha")
        if not isinstance(value, str) or not value.strip():
            raise EvidenceError(f"{path} has no candidate_sha")
        if sha is not None and sha != value:
            raise EvidenceError(
                f"{path} candidate_sha {value!r} does not match the other fragments ({sha!r})"
            )
        sha = value
        if "candidate_digest" in fragment:
            value = fragment["candidate_digest"]
            if not isinstance(value, str) or not value.strip():
                raise EvidenceError(f"{path} has an empty candidate_digest")
            if digest is not None and digest != value:
                raise EvidenceError(
                    f"{path} candidate_digest {value!r} does not match the other "
                    f"fragments ({digest!r})"
                )
            digest = value
        for scenario in fragment.get("scenarios") or []:
            sid = scenario.get("id")
            if not sid:
                raise EvidenceError(f"{path} has a scenario without an id")
            if sid in seen:
                raise EvidenceError(f"scenario {sid!r} appears in more than one fragment")
            seen.add(sid)
            scenarios.append(scenario)
    if not scenarios:
        raise EvidenceError("evidence fragments contain no scenarios")
    if digest is None:
        raise EvidenceError(
            "no fragment carries the candidate release digest; the lane that "
            "deploys the candidate image did not report"
        )
    return {
        "candidate_sha": sha,
        "candidate_digest": digest,
        "gate_enabled": bool(gate_enabled),
        "disabled_gates": list(disabled_gates),
        "scenarios": scenarios,
    }


REDACT_REMOVE = ("internal-token.txt", "kubeconfig", "kubeconfig-iso")


def redact_artifacts(artifacts):
    """Remove the run's generated credentials from an about-to-be-uploaded dir."""
    artifacts = Path(artifacts)
    if not artifacts.is_dir():
        raise EvidenceError(f"{artifacts} is not a directory")
    secrets = []
    token = artifacts / "internal-token.txt"
    if token.is_file():
        value = token.read_text().strip()
        if value:
            secrets.append(value)
    removed = []
    for name in REDACT_REMOVE:
        path = artifacts / name
        if path.is_file():
            path.unlink()
            removed.append(name)
    scrubbed = []
    for path in sorted(artifacts.rglob("*")):
        if not path.is_file():
            continue
        try:
            data = path.read_bytes()
        except OSError:
            continue
        replaced = data
        for secret in secrets:
            replaced = replaced.replace(secret.encode(), b"[REDACTED_INTERNAL_TOKEN]")
        if replaced != data:
            path.write_bytes(replaced)
            scrubbed.append(str(path.relative_to(artifacts)))
    return removed, scrubbed


def _write(out, payload):
    Path(out).parent.mkdir(parents=True, exist_ok=True)
    Path(out).write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n")


def parse_args(argv=None):
    parser = argparse.ArgumentParser(prog="collect-evidence.py", description=__doc__.splitlines()[0])
    sub = parser.add_subparsers(dest="command", required=True)

    rob = sub.add_parser("robustness", help="Fragments for the B1 owner-crash scenarios")
    rob.add_argument("--artifacts", required=True)
    rob.add_argument("--candidate-sha", default=None)
    rob.add_argument("--out", required=True)

    sql = sub.add_parser("sql-work-budget", help="Fragment for E5's SQL-work budget")
    sql.add_argument("--inspect", required=True, help="docker inspect <server> output")
    sql.add_argument("--metrics", required=True, help="Retained /metrics scrape")
    sql.add_argument("--log", required=True, help="go test -v output for the focused run")
    sql.add_argument("--candidate-sha", required=True)
    sql.add_argument("--server-image-id", required=True,
                     help="Image ID of the server container this lane actually ran")
    sql.add_argument("--out", required=True)

    rep = sub.add_parser("report", help="Merge fragments into one evidence report")
    rep.add_argument("--out", required=True)
    rep.add_argument("--gate-disabled", action="store_true")
    rep.add_argument("--disabled-gate", action="append", default=[])
    rep.add_argument("fragments", nargs="+")

    red = sub.add_parser("redact", help="Remove generated credentials from an artifact dir")
    red.add_argument("--artifacts", required=True)
    return parser.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    try:
        if args.command == "robustness":
            _write(args.out, build_robustness(args.artifacts, args.candidate_sha))
        elif args.command == "sql-work-budget":
            _write(args.out, build_sql_work_budget(
                args.inspect, args.metrics, args.log,
                args.candidate_sha, args.server_image_id,
            ))
        elif args.command == "report":
            _write(args.out, build_report(
                args.fragments,
                gate_enabled=not args.gate_disabled,
                disabled_gates=args.disabled_gate,
            ))
        elif args.command == "redact":
            removed, scrubbed = redact_artifacts(args.artifacts)
            print(f"collect-evidence: removed {removed or 'nothing'}; "
                  f"scrubbed {len(scrubbed)} file(s)")
            return 0
    except EvidenceError as err:
        print(f"collect-evidence: {err}", file=sys.stderr)
        return 1
    print(f"collect-evidence: wrote {args.out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
