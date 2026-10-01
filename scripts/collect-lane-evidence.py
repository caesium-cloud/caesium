#!/usr/bin/env python3
"""Turn the G6 system-suite lane artifacts into scenario evidence fragments.

The G6 counterpart of `scripts/collect-evidence.py` (G3's early-evidence
consumer), built on the same rules: every field a fragment carries is read
back out of a file the lane actually produced, a missing or unreadable input is
reported `inconclusive` (or refused outright) and never `pass`, and the result
is validated by `scripts/check-test-evidence.py` against the committed
`test/contracts/scenarios.json`.

Subcommands, one per lane:

  core         B3 `TestCore` rows from `scripts/robustness.sh` artifacts
               (CAESIUM_ROBUSTNESS_RUN='^TestCore$').
  lifecycle    F4 standalone or F2 cluster rows from the qualification record
               `scripts/lifecycle-tests.sh` writes.
  generated    C2 native-fuzz and C3 oracle-mutation rows from
               `scripts/fuzz-tests.sh` and `scripts/validate-test-oracles.sh`.
  coverage     G2 coverage-ratchet row from `scripts/integration-coverage.sh`.
  console      D3 console cluster-recovery row from the Playwright
               `cluster-recovery` project's `d3-evidence.json`.
  performance  E4 performance-gate row from `scripts/performance.sh`'s gate.
  report       Merge fragments into one report for a named lane gate.

Dependency-free (Python 3 stdlib only), like the checker it feeds.
"""

from __future__ import annotations

import argparse
import importlib.util
import json
import re
import sys
from pathlib import Path


SCRIPTS = Path(__file__).resolve().parent
# The lanes refuse a dirty checkout; importing the G3 consumer must not leave a
# scripts/__pycache__ behind for the next lane on the same checkout to trip on.
sys.dont_write_bytecode = True
_SPEC = importlib.util.spec_from_file_location("collect_evidence", SCRIPTS / "collect-evidence.py")
BASE = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(BASE)

EvidenceError = BASE.EvidenceError
read_text = BASE.read_text
read_json = BASE.read_json

DIGEST = BASE.DIGEST
SHA = BASE.SHA


def _require_sha(candidate_sha):
    if not SHA.match(candidate_sha or ""):
        raise EvidenceError(f"candidate SHA is not a commit SHA: {candidate_sha!r}")
    return candidate_sha


def _status(outcome):
    return {"pass": "pass", "fail": "fail"}.get(outcome, "inconclusive")


def _no_fault():
    # The checker rejects an empty `kind`, so it is omitted when no fault is
    # injected rather than filled with a placeholder.
    return {"activated": False, "observations": []}


# --------------------------------------------------------------------------
# B3 fenced core suite (TestCore)
# --------------------------------------------------------------------------

CORE_IDENTITY = "robustness-core"
CORE_PARENT = "TestCore"

# Record fields that name a run whose recorder events belong to the scenario.
CORE_RUN_FIELDS = (
    "run_id", "failed_run", "rejected_run", "first_run", "second_run",
    "majority_run", "reconciled_run", "heal_run",
)


def _get(record, *path):
    value = record
    for key in path:
        if not isinstance(value, dict):
            return None
        value = value.get(key)
    return value


def _int(value):
    return value if isinstance(value, int) and not isinstance(value, bool) else None


def _num(value):
    if isinstance(value, bool):
        return None
    return value if isinstance(value, (int, float)) else None


def _nonempty(value):
    return isinstance(value, str) and bool(value.strip())


def _eq(value, expected):
    return isinstance(value, str) and value.lower() == expected


def _generation_grew(record):
    before = _int(_get(record, "lease_before", "Generation"))
    after = _int(_get(record, "lease_after", "Generation"))
    return before is not None and after is not None and after > before


def _owner_moved(record):
    before = _get(record, "lease_before", "OwnerNode")
    after = _get(record, "lease_after", "OwnerNode")
    return _nonempty(before) and _nonempty(after) and before != after


def _split_plans(section):
    """`split plan <tag> drop raft=N internal=M` lines the 2-1 split logged."""
    return [
        (tag, int(raft), int(internal))
        for tag, raft, internal in re.findall(
            r"split plan (\S+) drop raft=(\d+) internal=(\d+)", section
        )
    ]


def _minority_isolated(record, section):
    # isolateMember installs one rule per peer per direction; every rule's own
    # packet counter must have matched Raft traffic, or the split was not real.
    plans = _split_plans(section)
    wanted = _int(record.get("fault_plans"))
    return (
        wanted is not None and wanted >= 4 and len(plans) >= wanted
        and all(raft > 0 for _, raft, _ in plans)
    )


def _timeouts_possibly_committed(record):
    attempts = record.get("attempts")
    if not isinstance(attempts, list) or not attempts:
        return False
    transport = [a for a in attempts if isinstance(a, dict) and a.get("Status") == 0]
    return bool(transport) and all(a.get("Outcome") == "possibly_committed" for a in transport)


# What runQuorumLoss records for a minority mutation whose timed-out trigger
# returned no run id: still possibly committed, but never reconciled.
UNRESOLVED_IDENTITY = "unknown_possibly_committed"


def _identity_reconciled(attempt, entry):
    """A run id recorded during the fault and read back after healing."""
    return (
        _nonempty(attempt.get("RunID"))
        and entry.get("identity") != UNRESOLVED_IDENTITY
        and entry.get("present") is True
        and _nonempty(entry.get("status_after_heal"))
    )


def _reconciliation(record):
    """Each minority attempt paired with its post-heal entry, or None if malformed."""
    attempts = record.get("attempts")
    reconciled = record.get("reconciled")
    if not isinstance(attempts, list) or not isinstance(reconciled, list):
        return None
    if len(reconciled) != len(attempts) or not reconciled:
        return None
    if not all(isinstance(item, dict) for item in attempts + reconciled):
        return None
    return [(attempt, entry, _identity_reconciled(attempt, entry))
            for attempt, entry in zip(attempts, reconciled)]


def _post_heal_reconciled(record):
    # DT-QUORUM-01 reconciles by recorded identities: an unresolved identity is
    # not a reconciliation, however it is classified.
    pairs = _reconciliation(record)
    return pairs is not None and all(ok for _, _, ok in pairs)


def _reconciliation_summary(record):
    """The per-attempt identity classification carried into the fragment."""
    pairs = _reconciliation(record) or []
    identities = [
        "reconciled" if ok else (entry.get("identity") if _nonempty(entry.get("identity")) else "unreconciled")
        for _, entry, ok in pairs
    ]
    return {
        "identities": identities,
        "reconciled": sum(1 for _, _, ok in pairs if ok),
        "unresolved": sum(1 for _, _, ok in pairs if not ok),
    }


def _kill_proven(record):
    evidence = str(record.get("kill_evidence") or "")
    killed = re.search(r"^ctr kill (\S+)$", evidence, re.M)
    if not killed:
        return False
    listing = "\n".join(
        line for line in evidence.splitlines()
        if not line.strip().lower().startswith(("kubelet stopped", "ctr kill"))
    )
    return BASE._task_dead(killed.group(1), listing)


def _gate_arrivals(record):
    arrivals = record.get("gate_arrivals")
    if not isinstance(arrivals, list):
        return set()
    return {item.get("name") for item in arrivals if isinstance(item, dict)}


def _two_histories(record):
    histories = record.get("histories")
    return (
        isinstance(histories, list) and len(histories) == 2
        and all(_nonempty(item) for item in histories) and len(set(histories)) == 2
    )


def _internal_tls_proven(record):
    """A valid-certificate exchange on the dedicated mTLS listener happened."""
    if record.get("control_reached") is True and record.get("control_status") == 200:
        return True
    # A 401 from the internal listener is only reachable after the TLS
    # handshake accepted the runner's cluster-CA client certificate.
    return record.get("complete_status") == 401 and record.get("dispatch_status") == 401


class CoreRow:
    """One manifest row, its TestCore subtest and how its evidence is read."""

    def __init__(self, sid, subtest, observations, fault=None, instrumented_only=False,
                 samples="events", extra_flags=None, reconciles=False):
        self.sid = sid
        self.subtest = subtest
        self.observations = observations          # {observation_id: fn(record, section)}
        self.fault = fault                        # (kind, {observation_id: fn})
        self.instrumented_only = instrumented_only
        self.samples = samples
        self.extra_flags = extra_flags            # fn(record) -> dict
        self.reconciles = reconciles              # rests on post-heal identity reconciliation


CORE_ROWS = (
    CoreRow("b3-terminal-no-regress", "terminal_no_regress", {
        "terminal_outcome_does_not_regress": lambda r, s: _eq(r.get("public_status"), "succeeded"),
        "second_terminal_event_suppressed": lambda r, s: (
            r.get("complete_status") == 409 and _nonempty(r.get("refusal_code"))
        ),
    }),
    CoreRow("b3-frozen-retry-recipe", "frozen_retry_recipe", {
        "retry_returns_202": lambda r, s: r.get("retry_status") == 202,
        "frozen_persisted_recipe_fields": lambda r, s: (
            (_int(r.get("boom_starts")) or 0) >= 2 and _eq(r.get("after_retry"), "failed")
            and _nonempty(r.get("frozen_digest"))
        ),
        "rejected_retry_no_state_change": lambda r, s: r.get("rejected_status") == 409,
    }),
    CoreRow("b3-fan-in-predecessors", "fan_in_predecessors", {
        "starts_match_recorded_predecessor_completions": lambda r, s: (
            (_int(r.get("left_complete")) or 0) >= 1 and (_int(r.get("right_complete")) or 0) >= 1
            and (_int(r.get("join_starts")) or 0) >= 1
        ),
        "fan_in_requires_whole_predecessor_group": lambda r, s: (
            (_int(r.get("left_complete")) or 0) >= 1 and (_int(r.get("right_complete")) or 0) >= 1
            and _eq(r.get("public_status"), "succeeded")
        ),
        # FanInStartedTooEarly over the frozen instance identities and explicit
        # edges is asserted before the record is written.
        "frozen_instance_identity": lambda r, s: (
            _nonempty(r.get("run_id")) and (_int(r.get("join_starts")) or 0) >= 1
        ),
    }),
    CoreRow("b3-wrong-token-internal", "wrong_token_internal", {
        "valid_cert_wrong_token_401": lambda r, s: (
            r.get("complete_status") == 401 and r.get("dispatch_status") == 401
        ),
        "denied_operation_no_mutation": lambda r, s: _nonempty(r.get("state_digest")),
        "principal_scope_recorded_without_secrets": lambda r, s: (
            _nonempty(r.get("principal")) and r.get("token_class") == "wrong"
            and r.get("redacted") is True
        ),
    }, fault=("wrong-internal-token", {
        "wrong_token_sent_on_internal_listener": lambda r, s: (
            r.get("token_class") == "wrong" and _nonempty(r.get("target_complete"))
            and _nonempty(r.get("target_dispatch"))
        ),
        "request_rejected_before_task_handling": lambda r, s: (
            r.get("complete_status") == 401 and r.get("dispatch_status") == 401
            and _nonempty(r.get("state_digest"))
        ),
    }), extra_flags=lambda r: {"internal_tls": "provisioned"} if _internal_tls_proven(r) else {}),
    CoreRow("b3-invalid-mtls-peer", "invalid_mtls_peer", {
        "invalid_cert_handshake_failed": lambda r, s: (
            r.get("handshake") == "failed" and r.get("error_class") == "tls"
        ),
        "denied_operation_no_mutation": lambda r, s: _nonempty(r.get("state_digest")),
        "principal_scope_recorded_without_secrets": lambda r, s: (
            _nonempty(r.get("kind")) and r.get("redacted") is True
        ),
    }, fault=("invalid-mtls-peer", {
        "invalid_client_cert_presented": lambda r, s: r.get("kind") == "invalid-cert",
        "handshake_failed_before_handler": lambda r, s: (
            r.get("handshake") == "failed" and r.get("control_reached") is True
            and r.get("control_status") == 200
        ),
    }), extra_flags=lambda r: {"internal_tls": "provisioned"} if _internal_tls_proven(r) else {}),
    CoreRow("b3-cancel-completion-race", "cancel_completion_race", {
        "old_run_tasks_cancelled": lambda r, s: _eq(r.get("first_status"), "cancelled"),
        "completion_race_history_preserved": lambda r, s: (
            _two_histories(r) and _get(r, "persisted_scope", "Complete") is True
            and isinstance(r.get("persisted_events"), list) and bool(r.get("persisted_events"))
        ),
        "replacement_202_is_not_process_death_ack": lambda r, s: (
            _two_histories(r) and _eq(r.get("second_status"), "succeeded")
        ),
    }, fault=("cancel-completion-race", {
        "cancel_or_replace_issued": lambda r, s: (
            "replace" in _gate_arrivals(r) and _nonempty(r.get("second_run"))
        ),
        "completion_path_also_observed": lambda r, s: (
            "complete" in _gate_arrivals(r) and (_int(r.get("completion_status")) or 0) > 0
        ),
    })),
    CoreRow("b3-cancel-post-commit-fence", "cancel_post_commit_fence", {
        "old_run_stays_cancelled": lambda r, s: _eq(r.get("first_status"), "cancelled"),
        "late_completion_fenced": lambda r, s: (
            (r.get("old_complete_status") == 409 and _nonempty(r.get("old_complete_code")))
            or r.get("old_lease_absence_proven") is True
        ),
        "replacement_is_a_distinct_history": lambda r, s: (
            _two_histories(r) and _eq(r.get("second_status"), "succeeded")
        ),
        "no_old_successor_after_replace": lambda r, s: r.get("successor_starts") == 0,
    }, fault=("post-cancel-completion", {
        "replacement_committed_before_completion": lambda r, s: (
            r.get("completion_phase") == "post_cancel_commit"
            and r.get("replacement_202_ack") == "not_process_death"
        ),
        "old_owner_completion_sent": lambda r, s: (_int(r.get("old_complete_status")) or 0) > 0,
    })),
    CoreRow("b3-commit-before-response-loss", "commit_before_response_loss", {
        "upstream_202_observed_before_client_loss": lambda r, s: (
            r.get("upstream_status") == 202 and _nonempty(r.get("client_error"))
        ),
        "timeout_recorded_as_possibly_committed": lambda r, s: r.get("outcome") == "possibly_committed",
        "reconciled_by_identity_on_another_member": lambda r, s: (
            _nonempty(r.get("reconciled_run")) and _nonempty(r.get("final_status"))
        ),
    }, fault=("drop-response-after-commit", {
        # The heal probe goes through the same interposer after disarm, so a
        # usable 202 there proves the drop was the interposer's doing.
        "interposer_applied_drop": lambda r, s: (
            _nonempty(r.get("client_error")) and r.get("heal_status") == 202
        ),
        "upstream_202_before_client_failure": lambda r, s: (
            r.get("upstream_status") == 202 and _nonempty(r.get("client_error"))
        ),
    })),
    CoreRow("b3-stale-generation-complete", "stale_generation_complete", {
        "stale_generation_rejected_409": lambda r, s: r.get("complete_status") == 409,
        "refusal_reason_preserved": lambda r, s: r.get("refusal_code") == "stale_generation",
        # requireNoMutation compares durable fingerprints before the record.
        "rejected_request_no_mutation": lambda r, s: (
            r.get("complete_status") == 409 and _eq(r.get("public_status"), "succeeded")
        ),
    }, fault=("stale-generation-complete", {
        "request_carries_stale_generation": lambda r, s: _generation_grew(r),
        "completion_rejected_before_commit": lambda r, s: (
            r.get("complete_status") == 409 and r.get("refusal_code") == "stale_generation"
        ),
    })),
    CoreRow("b3-owner-pause-past-lease", "stale_generation_complete", {
        "owner_held_past_lease": lambda r, s: (
            r.get("past_lease") is True and (_num(r.get("held_seconds")) or 0) > 30
        ),
        "stale_generation_rejected_409": lambda r, s: r.get("complete_status") == 409,
        "survivor_completed_same_run": lambda r, s: (
            _eq(r.get("public_status"), "succeeded") and _generation_grew(r) and _owner_moved(r)
            and _get(r, "lease_after", "RunID") == r.get("run_id")
        ),
    }, fault=("owner-pause-past-lease", {
        # The subtest cordons the owner's kind node through the host
        # controller before triggering and fails outright if that is refused.
        "owner_kind_node_cordoned_before_fixture": lambda r, s: _nonempty(r.get("owner_paused")),
        "runtime_paused": lambda r, s: (
            r.get("pause_state_before") == "RUNNING" and r.get("pause_state_paused") == "PAUSED"
        ),
        # The subtest fails if the paused owner still answers /health.
        "member_unreachable_during_pause": lambda r, s: r.get("pause_state_paused") == "PAUSED",
        "held_past_lease": lambda r, s: r.get("past_lease") is True,
    })),
    CoreRow("b3-worker-unreachable-bench", "worker_unreachable_bench", {
        "network_error_rejection_metric_rose": lambda r, s: (_num(r.get("network_error_rise")) or 0) >= 1,
        "dispatch_progress_on_reachable_peers": lambda r, s: (
            (_num(r.get("sent_after")) or 0) > (_num(r.get("sent_before")) or 0)
            and isinstance(r.get("run_ids"), list) and bool(r.get("run_ids"))
        ),
        "worker_receives_work_after_cooldown": lambda r, s: _nonempty(_get(r, "recovered_claim", "claimed_by")),
    }, fault=("worker-pause-unreachable", {
        "worker_runtime_paused": lambda r, s: r.get("pause_state_paused") == "PAUSED",
        # The subtest fails if the paused worker still answers /health.
        "worker_unreachable_during_pause": lambda r, s: (
            r.get("pause_state_paused") == "PAUSED" and r.get("pause_state_resumed") == "RUNNING"
        ),
        "rejected_network_error_rose": lambda r, s: (_num(r.get("network_error_rise")) or 0) >= 1,
    })),
    CoreRow("b3-quorum-loss-uncertain-write", "quorum_loss_uncertain_write", {
        "timeout_recorded_as_possibly_committed": lambda r, s: _timeouts_possibly_committed(r),
        "no_rejection_inferred_from_client_timeout": lambda r, s: (
            r.get("timeout_is_reject") is False and _timeouts_possibly_committed(r)
        ),
        "post_heal_identity_reconciliation": lambda r, s: _post_heal_reconciled(r),
    }, fault=("minority-isolation", {
        "two_voters_unreachable": lambda r, s: _minority_isolated(r, s),
        # Identified means the run id the fault-time attempt recorded is the
        # one read back after healing, not merely that a request was sent.
        "identified_mutation_sent_during_fault": lambda r, s: _post_heal_reconciled(r),
        "fault_healed_before_final_read": lambda r, s: _post_heal_reconciled(r),
    }), reconciles=True),
    CoreRow("b3-split-heal-2-1", "quorum_loss_uncertain_write", {
        "majority_progresses_during_split": lambda r, s: _eq(r.get("majority_final"), "succeeded"),
        "minority_write_possibly_committed": lambda r, s: _timeouts_possibly_committed(r),
        # waitMembership after heal is asserted before the record.
        "quorum_restored_after_heal": lambda r, s: _post_heal_reconciled(r),
    }, fault=("2-1-split", {
        "minority_isolated_from_both_peers": lambda r, s: _minority_isolated(r, s),
        "majority_mutation_during_fault": lambda r, s: _nonempty(r.get("majority_run")),
        "fault_healed_before_final_read": lambda r, s: _post_heal_reconciled(r),
    }), reconciles=True),
    CoreRow("b3-durable-event-before-delivery-crash", "durable_event_before_delivery_crash", {
        "durable_event_committed_before_kill": lambda r, s: (
            _get(r, "row_while_held", "BusPending") is True
            and _get(r, "row_while_held", "Sequence") == r.get("held_sequence")
        ),
        "publisher_process_killed": lambda r, s: _kill_proven(r),
        "durable_replay_after_kill": lambda r, s: (
            _get(r, "row_after_kill", "BusPending") is False
            and _nonempty(_get(r, "row_after_kill", "DispatchedAt"))
        ),
    }, fault=("kill-after-durable-event-before-publish", {
        "hook_entered_before_publish": lambda r, s: (_int(r.get("hook_members")) or 0) >= 1,
        "publisher_process_killed": lambda r, s: _kill_proven(r),
        "committed_row_observed_before_kill": lambda r, s: (
            _get(r, "row_while_held", "BusPending") is True
        ),
    }), instrumented_only=True, samples="hook_members"),
)


def _subtest_section(log, subtest):
    parts = log.split(f"=== RUN   {CORE_PARENT}/{subtest}\n", 1)
    if len(parts) < 2:
        return ""
    return parts[1].split("=== RUN   ", 1)[0]


def _run_ids(record):
    ids = {str(record[key]) for key in CORE_RUN_FIELDS if _nonempty(record.get(key))}
    runs = record.get("run_ids")
    if isinstance(runs, list):
        ids.update(str(item) for item in runs if _nonempty(item))
    return ids


def build_core(artifacts, candidate_sha):
    artifacts = Path(artifacts)
    sha = _require_sha(candidate_sha)
    versions = read_json(artifacts / "versions.json")
    if versions.get("candidate_sha") != sha:
        raise EvidenceError(
            f"versions.json candidate_sha {versions.get('candidate_sha')!r} is not {sha!r}"
        )
    digest = read_text(artifacts / "candidate-digest.txt").strip()
    log = read_text(artifacts / "robustness.test.log")
    env = BASE.agreed_env(BASE._observed_members(artifacts))
    topology = BASE.robustness_topology(artifacts, env)
    mode = BASE.observed_mode(env, "http")

    try:
        core_topology = read_json(artifacts / "records" / "core_topology.json")
    except EvidenceError:
        core_topology = {}
    # The instrumented selection is decided by what was deployed, read back
    # from the runner's own record, never assumed from the caller.
    instrumented = core_topology.get("instrumented") is True
    try:
        events = read_json(artifacts / "records" / "core_events.json")
        if not isinstance(events, list):
            raise EvidenceError("records/core_events.json is not a list")
    except EvidenceError:
        events = None
    parent = BASE._go_outcome(log, CORE_PARENT)

    scenarios = []
    for row in CORE_ROWS:
        if row.instrumented_only and not instrumented:
            # Not produced at all: a gate that requires it reports it missing.
            continue
        entry = {
            "id": row.sid,
            "artifact_identity": CORE_IDENTITY,
            "candidate_sha": sha,
            "candidate_digest": digest,
            "topology": topology,
            "mode": mode,
            "feature_flags": dict(env),
            "checker": {"timed_out": False},
        }
        outcome = BASE._go_outcome(log, f"{CORE_PARENT}/{row.subtest}")
        if parent != "pass" and outcome == "pass":
            outcome = None
        try:
            record = read_json(artifacts / "records" / f"{row.subtest}.json")
            if not isinstance(record, dict):
                record = None
        except EvidenceError:
            record = None
        if record is None or events is None:
            entry["status"] = "fail" if outcome == "fail" else "inconclusive"
            entry["recorder"] = {"present": False, "sample_count": 0}
            entry["observations"] = []
            if row.fault:
                entry["fault_activation"] = {"activated": False, "kind": row.fault[0], "observations": []}
            else:
                entry["fault_activation"] = _no_fault()
            scenarios.append(entry)
            continue
        section = _subtest_section(log, row.subtest)
        if row.samples == "hook_members":
            samples = _int(record.get("hook_members")) or 0
        else:
            ids = _run_ids(record)
            samples = sum(1 for event in events if isinstance(event, dict) and event.get("run_id") in ids)
        entry["recorder"] = {"present": True, "sample_count": samples}
        entry["observations"] = [oid for oid, check in row.observations.items() if check(record, section)]
        if row.fault:
            kind, checks = row.fault
            seen = [oid for oid, check in checks.items() if check(record, section)]
            entry["fault_activation"] = {"activated": bool(seen), "kind": kind, "observations": seen}
        else:
            entry["fault_activation"] = _no_fault()
        if row.extra_flags:
            entry["feature_flags"].update(row.extra_flags(record))
        entry["status"] = _status(outcome)
        if row.reconciles:
            summary = _reconciliation_summary(record)
            entry["reconciliation"] = summary
            if summary["unresolved"] and entry["status"] != "fail":
                # A possibly-committed write nobody read back after healing is
                # neither a pass nor a rejection.
                entry["status"] = "inconclusive"
        scenarios.append(entry)
    fragment = BASE._fragment(sha, digest, scenarios)
    fragment["instrumented"] = instrumented
    return fragment


# --------------------------------------------------------------------------
# F4 standalone / F2 cluster lifecycle qualification
# --------------------------------------------------------------------------

LIFECYCLE = {
    "standalone": {
        "id": "f4-standalone-lifecycle",
        "identity": "lifecycle-standalone-qualification",
        "record_kind": "caesium-lifecycle-qualification",
        "file": "qualification.json",
    },
    "cluster": {
        "id": "f2-cluster-lifecycle",
        "identity": "lifecycle-cluster-qualification",
        "record_kind": "caesium-cluster-lifecycle-qualification",
        "file": "cluster-qualification.json",
    },
}
CASE_OK = ("pass", "recorded-outcome")


def _env_flags_from_args(server_env):
    """`-e KEY=VALUE ...` as the standalone record stores the server env."""
    flags = {}
    for key, value in re.findall(r"(?:^|\s)-e\s+(CAESIUM_[A-Z0-9_]+)=(\S*)", server_env or ""):
        if not BASE.SECRET_ENV.search(key):
            flags[key] = value
    return flags


def _env_flags_from_manifest(text):
    """`- name: KEY\\n  value: VALUE` pairs of the live StatefulSet manifest."""
    flags = {}
    pattern = re.compile(r"-\s+name:\s*(CAESIUM_[A-Z0-9_]+)\s*\n\s+value:\s*\"?([^\"\n]*)\"?")
    for key, value in pattern.findall(text):
        if not BASE.SECRET_ENV.search(key):
            flags[key] = value.strip()
    return flags


def build_lifecycle(artifacts, mode_name, candidate_sha, expected_image_id=None):
    if mode_name not in LIFECYCLE:
        raise EvidenceError(f"unknown lifecycle mode {mode_name!r}")
    spec = LIFECYCLE[mode_name]
    artifacts = Path(artifacts)
    sha = _require_sha(candidate_sha)
    record = read_json(artifacts / spec["file"])
    if not isinstance(record, dict) or record.get("kind") != spec["record_kind"]:
        raise EvidenceError(f"{spec['file']} is not a {spec['record_kind']} record")
    if record.get("candidate_sha") != sha:
        raise EvidenceError(
            f"{spec['file']} qualifies {record.get('candidate_sha')!r}, not the tested candidate {sha!r}"
        )
    expected = record.get("expected_cases")
    cases = record.get("cases")
    if not isinstance(expected, list) or not expected or not isinstance(cases, list):
        raise EvidenceError(f"{spec['file']} carries no expected case set")
    by_name = {case.get("name"): case for case in cases if isinstance(case, dict)}
    observed = [name for name in expected if (by_name.get(name) or {}).get("status") in CASE_OK]

    if mode_name == "standalone":
        image_id = str(((record.get("images") or {}).get("candidate") or {}).get("image_id") or "")
        provenance = record.get("candidate_provenance") or {}
        flags = _env_flags_from_args(record.get("server_env"))
        topo = record.get("topology") or {}
        persisted = all(
            (by_name.get(name) or {}).get("status") == "pass"
            for name in ("assert1-candidate-healthy-and-migrated", "assert3-recorded-identities-readable")
        )
        topology = {
            "kind": "docker-standalone",
            "replicas": _int(topo.get("nodes")) or 0,
            # The candidate read the previous release's identities back off the
            # same named volume: that is what persistence means here.
            "persistence": persisted,
            "engine": "docker",
            "database_shards": _int(topo.get("database_shards")) or 0,
        }
        mode = {
            "execution": flags.get("CAESIUM_EXECUTION_MODE", "local"),
            "owner": "enabled" if flags.get("CAESIUM_RUN_OWNER_ENABLED") == "true" else "disabled",
            "surface": "http+cli",
        }
        # Provenance: the run built its own image from a clean checkout, OR it
        # ran the image this workflow's `images` job built from the same SHA and
        # the lane verified that image id before the qualification started. An
        # override without that independent binding is not evidence.
        verified = provenance.get("verified") is True and provenance.get("built_by_this_run") is True
        bound = (
            bool(expected_image_id) and provenance.get("override") is True
            and provenance.get("image_id") == expected_image_id == image_id
        )
        provenance_ok = verified or bound
    else:
        candidate = record.get("candidate_image") or {}
        image_id = str(candidate.get("image_id") or "")
        topo = record.get("topology") or {}
        try:
            flags = _env_flags_from_manifest(read_text(artifacts / "manifest-upgraded.yaml"))
        except EvidenceError:
            flags = {}
        topology = {
            "kind": "kind-helm-statefulset",
            "replicas": _int(topo.get("replicas")) or 0,
            "persistence": topo.get("persistent") is True,
            "engine": "kubernetes",
            "database_shards": _int(topo.get("database_shards")) or 0,
        }
        mode = {
            "execution": flags.get("CAESIUM_EXECUTION_MODE", "local"),
            "owner": "enabled" if flags.get("CAESIUM_RUN_OWNER_ENABLED") == "true" else "disabled",
            "surface": "http+cli",
        }
        # Cluster mode always builds and archive-verifies its own candidate.
        provenance_ok = DIGEST.match(image_id or "") is not None and candidate.get("source_docker_image_id") == image_id
    if not DIGEST.match(image_id or ""):
        raise EvidenceError(f"{spec['file']} names no candidate image id ({image_id!r})")

    result = record.get("result")
    if result == "pass" and len(observed) == len(expected):
        # A qualification of an image nothing binds to candidate_sha is not a
        # qualification of the candidate.
        status = "pass" if provenance_ok else "fail"
    elif result in ("fail", "blocked") or (by_name and any(
        (case or {}).get("status") == "fail" for case in by_name.values()
    )):
        status = "fail"
    else:
        status = "inconclusive"
    entry = {
        "id": spec["id"],
        "status": status,
        "artifact_identity": spec["identity"],
        "candidate_sha": sha,
        "candidate_digest": image_id,
        "topology": topology,
        "mode": mode,
        "feature_flags": flags,
        "observations": observed,
        "fault_activation": _no_fault(),
        "recorder": {"present": True, "sample_count": len(observed)},
        "checker": {"timed_out": False},
        "qualification": {
            "lifecycle_id": record.get("lifecycle_id"),
            "result": result,
            "provenance_bound": provenance_ok,
            "failed_cases": record.get("failed_cases") or [],
        },
    }
    return BASE._fragment(sha, image_id, [entry])


# --------------------------------------------------------------------------
# C2 native fuzzing + C3 oracle mutation validator
# --------------------------------------------------------------------------

FUZZ_ID = "c2-native-fuzz"
FUZZ_IDENTITY = "generated-native-fuzz"
ORACLE_ID = "c3-oracle-mutations"
ORACLE_IDENTITY = "generated-oracle-mutations"
ORACLE_CANDIDATE_PROBES = ("model-candidate", "history-candidate", "robustness-candidate")


def _generated_topology():
    return {"kind": "builder-container", "replicas": 1, "persistence": False, "engine": "docker"}


def _fuzz_entry(fuzz_dir, sha):
    fuzz_dir = Path(fuzz_dir)
    try:
        summary = read_text(fuzz_dir / "summary.txt")
    except EvidenceError:
        summary = None
    entry = {
        "id": FUZZ_ID,
        "artifact_identity": FUZZ_IDENTITY,
        "candidate_sha": sha,
        "topology": _generated_topology(),
        "mode": {"execution": "hermetic", "surface": "go-test"},
        "feature_flags": {},
        "fault_activation": _no_fault(),
        "checker": {"timed_out": False},
    }
    if not summary:
        entry.update(status="inconclusive", observations=[],
                     recorder={"present": False, "sample_count": 0})
        return entry
    declared_lines = re.findall(r"^\./(\S+): declared=\[([^\]]*)\] discovered=\[([^\]]*)\]", summary, re.M)
    declared = []
    drift = bool(re.search(r"^FAIL: (declared fuzz target|undeclared fuzz target|repo-wide sweep)", summary, re.M))
    for _, want, have in declared_lines:
        want_set, have_set = set(want.split()), set(have.split())
        declared.extend(sorted(want_set))
        if want_set != have_set:
            drift = True
    explored = {}
    for target, execs, baseline in re.findall(
        r"^OK: (Fuzz\w+) \S+ \d+s, execs=(\d+) \(baseline=(\d+)\)", summary, re.M
    ):
        if int(execs) > int(baseline):
            explored[target] = int(execs)
    crashed = bool(re.search(r"^CRASH: ", summary, re.M))
    cpus = {int(cpu) for cpu, passed, failed in re.findall(
        r"^OK: -cpu=(\d+) pass=(\d+) fail=(\d+)$", summary, re.M
    ) if int(passed) > 0 and int(failed) == 0}
    result_ok = bool(re.search(r"^RESULT: all declared targets explored", summary, re.M))
    result_failed = bool(re.search(r"^RESULT: FAILED", summary, re.M))
    corpus = fuzz_dir / "corpus"

    def exported(target):
        # fuzz-tests.sh exports the package's committed testdata/fuzz/<target>
        # (when the package has one) and the run's own explored corpus as
        # <target>-cache; either, non-empty, is a preserved corpus.
        return any(
            (corpus / name).is_dir() and any((corpus / name).iterdir())
            for name in (target, f"{target}-cache")
        )

    preserved = bool(declared) and all(exported(target) for target in declared)

    observations = []
    if declared and not drift:
        observations.append("declared_targets_match_discovered")
    if declared and all(target in explored for target in declared):
        observations.append("every_target_explored_beyond_baseline")
    if declared and not crashed:
        observations.append("no_fuzz_crashers")
    if {1, 2, 4} <= cpus:
        observations.append("concurrency_matrix_passed")
    if preserved:
        observations.append("corpus_preserved")
    status = "pass" if result_ok and not result_failed else ("fail" if result_failed else "inconclusive")
    entry.update(status=status, observations=observations,
                 recorder={"present": True, "sample_count": len(explored)})
    return entry


def _oracle_entry(oracle_log, sha):
    entry = {
        "id": ORACLE_ID,
        "artifact_identity": ORACLE_IDENTITY,
        "candidate_sha": sha,
        "topology": _generated_topology(),
        "mode": {"execution": "hermetic", "surface": "go-test"},
        "feature_flags": {},
        "fault_activation": _no_fault(),
        "checker": {"timed_out": False},
    }
    try:
        log = read_text(oracle_log)
    except EvidenceError:
        log = None
    if not log:
        entry.update(status="inconclusive", observations=[],
                     recorder={"present": False, "sample_count": 0})
        return entry
    named = re.search(r"^oracle candidate=([0-9a-f]{40})$", log, re.M)
    if not named or named.group(1) != sha:
        raise EvidenceError(
            f"oracle log names candidate {named.group(1) if named else None!r}, not {sha!r}"
        )
    candidates_pass = all(
        re.search(rf"^{re.escape(label)}: pass \[", log, re.M) for label in ORACLE_CANDIDATE_PROBES
    )
    mutations = re.findall(
        r"^mutation=(\S+) bad_sha=([0-9a-f]{40}) patch=(\S+) patch_blob=([0-9a-f]{40})$", log, re.M
    )
    rejected = [name for name, _, _, _ in mutations
                if re.search(rf"^{re.escape(name)}: fail \[", log, re.M)]
    completed = bool(re.search(r"^oracle validation PASS: ", log, re.M))

    observations = []
    if candidates_pass:
        observations.append("candidate_probes_accept_the_candidate")
    if mutations and len(rejected) == len(mutations):
        observations.append("known_bad_mutations_rejected")
    if mutations:
        observations.append("mutations_recorded_with_known_bad_sha")
    if completed:
        observations.append("validator_completed")
    status = "pass" if completed and candidates_pass and mutations and len(rejected) == len(mutations) else "fail"
    entry.update(status=status, observations=observations,
                 recorder={"present": True, "sample_count": len(rejected)})
    return entry


def build_generated(fuzz_dir, oracle_log, candidate_sha, builder_image_id):
    """C2 and/or C3 rows: each row is emitted only for the input its lane ran."""
    sha = _require_sha(candidate_sha)
    if not DIGEST.match(builder_image_id or ""):
        raise EvidenceError(f"builder image id is not sha256:<64 hex>: {builder_image_id!r}")
    if not fuzz_dir and not oracle_log:
        raise EvidenceError("generated needs --fuzz-dir and/or --oracle-log")
    scenarios = []
    if fuzz_dir:
        recorded = read_text(Path(fuzz_dir) / "candidate-sha.txt").strip()
        if recorded != sha:
            raise EvidenceError(f"fuzz artifacts were produced for {recorded!r}, not {sha!r}")
        scenarios.append(_fuzz_entry(fuzz_dir, sha))
    if oracle_log:
        scenarios.append(_oracle_entry(oracle_log, sha))
    for entry in scenarios:
        entry["observed_builder_image_id"] = builder_image_id
    return BASE._fragment(sha, builder_image_id, scenarios)


# --------------------------------------------------------------------------
# G2 coverage ratchets
# --------------------------------------------------------------------------

COVERAGE_ID = "g2-coverage-ratchets"
COVERAGE_IDENTITY = "integration-coverage-ratchets"
COVERAGE_SURFACES = ("cli", "server", "integration", "browser")


def build_coverage(report_path, candidate_sha):
    sha = _require_sha(candidate_sha)
    report = read_json(report_path)
    if not isinstance(report, dict):
        raise EvidenceError(f"{report_path} is not a coverage report object")
    if report.get("candidate_sha") != sha:
        raise EvidenceError(
            f"coverage report measures {report.get('candidate_sha')!r}, not {sha!r}"
        )
    contributions = report.get("contributions") or {}
    complete = [
        name for name in COVERAGE_SURFACES
        if (contributions.get(name) or {}).get("status") == "complete"
    ]
    image_ids = {
        str(((contributions.get(name) or {}).get("provenance") or {}).get("image_id") or "")
        for name in ("cli", "server")
    }
    image_ids.discard("")
    image_id = image_ids.pop() if len(image_ids) == 1 else ""
    if not DIGEST.match(image_id):
        raise EvidenceError(f"coverage report names no single coverage image id ({sorted(image_ids)})")
    diff = report.get("diff_coverage") or {}
    ratchet = report.get("ratchet") or {}
    write_to_read = report.get("write_to_read") or {}

    observations = []
    if report.get("verdict") == "pass":
        observations.append("coverage_verdict_pass")
    if len(complete) == len(COVERAGE_SURFACES):
        observations.append("profiles_complete_with_provenance")
    if ratchet.get("applied") is True:
        observations.append("package_ratchet_applied")
    if write_to_read.get("status") == "pass" and write_to_read.get("covered") is True:
        observations.append("write_to_read_path_covered")
    if isinstance(diff, dict) and diff and report.get("uncovered_changed_paths") == []:
        observations.append("diff_ratchet_within_policy")
    if report.get("performance_instrumentation") is False:
        observations.append("performance_instrumentation_absent")
    verdict = report.get("verdict")
    status = {"pass": "pass", "fail": "fail"}.get(verdict, "inconclusive")
    entry = {
        "id": COVERAGE_ID,
        "status": status,
        "artifact_identity": COVERAGE_IDENTITY,
        "candidate_sha": sha,
        "observed_coverage_image_id": image_id,
        "topology": {"kind": "docker-coverage-instrumented", "replicas": 1,
                     "persistence": False, "engine": "docker"},
        "mode": {"execution": "local", "surface": "http+cli+browser"},
        "feature_flags": {},
        "observations": observations,
        "fault_activation": _no_fault(),
        "recorder": {"present": True, "sample_count": len(complete)},
        "checker": {"timed_out": False},
        "coverage": {
            "all_surfaces_percent": (report.get("all_surfaces") or {}).get("percent"),
            "uncovered_changed_paths": report.get("uncovered_changed_paths"),
            "issues": report.get("issues") or [],
        },
    }
    return BASE._fragment(sha, image_id, [entry])


# --------------------------------------------------------------------------
# D3 console cluster-recovery journey
# --------------------------------------------------------------------------

CONSOLE_ID = "d3-console-cluster-recovery"
CONSOLE_IDENTITY = "ui-e2e-cluster-recovery"
CONSOLE_FAULT_KIND = "owner-sigkill"


def build_console(artifacts, candidate_sha, playwright_exit):
    artifacts = Path(artifacts)
    sha = _require_sha(candidate_sha)
    digest = read_text(artifacts / "candidate-digest.txt").strip()
    env = BASE.agreed_env(BASE._observed_members(artifacts))
    topology = BASE.robustness_topology(artifacts, env)
    mode = BASE.observed_mode(env, "browser")
    entry = {
        "id": CONSOLE_ID,
        "artifact_identity": CONSOLE_IDENTITY,
        "candidate_sha": sha,
        "candidate_digest": digest,
        "topology": topology,
        "mode": mode,
        "feature_flags": env,
        "checker": {"timed_out": False},
    }
    try:
        evidence = read_json(artifacts / "d3-evidence.json")
        if not isinstance(evidence, dict):
            evidence = None
    except EvidenceError:
        evidence = None
    if evidence is None:
        entry.update(status="fail" if playwright_exit not in (None, 0) else "inconclusive",
                     observations=[], recorder={"present": False, "sample_count": 0},
                     fault_activation={"activated": False, "kind": CONSOLE_FAULT_KIND, "observations": []})
        return BASE._fragment(sha, digest, [entry])
    server_image = str(evidence.get("serverImage") or "")
    if not server_image.endswith(":" + sha):
        raise EvidenceError(f"d3-evidence serverImage {server_image!r} is not the candidate {sha!r}")

    run_id = evidence.get("runId")
    fault = evidence.get("fault") or {}
    console = evidence.get("console") or {}
    durable = evidence.get("durable") or {}
    takeover = evidence.get("takeover") or {}
    rows = [row for key in ("runRows", "reloadedRunRows", "reloadedRunListFirstRows")
            for row in (console.get(key) or []) if isinstance(row, dict)]
    durable_status = durable.get("status")
    observations = []
    if _nonempty(fault.get("recordedAt")) and fault.get("headingBefore") == "running" \
            and (_int(fault.get("signals")) or 0) >= 1:
        observations.append("browser_observed_fault_while_connected")
    if durable_status == "succeeded" and console.get("headingStatus") == durable_status \
            and console.get("reloadedHeadingStatus") == durable_status \
            and console.get("logHasMarker") is True and console.get("reloadedLogHasMarker") is True:
        observations.append("ui_converges_on_durable_outcome")
    if evidence.get("issues") == [] and rows and all(
        row.get("id") == run_id and row.get("status") == durable_status for row in rows
    ) and len(console.get("runRows") or []) == 1 and takeover.get("terminalBeforeTakeover") is None:
        observations.append("no_duplicate_or_false_terminal_success")

    fault_seen = []
    if _nonempty(fault.get("startedAt")) and fault.get("headingDuring") == "running":
        fault_seen.append("owner_fault_while_browser_connected")
    if (_int(evidence.get("serviceForwardAttempts")) or 0) >= 1 and _nonempty(evidence.get("serviceForwardAt")) \
            and (_int(console.get("authenticatedRunReads")) or 0) + (_int(console.get("eventStreamAuthorized")) or 0) > 0:
        fault_seen.append("ui_reconnect_through_supported_entry")
    status = "pass" if playwright_exit == 0 and evidence.get("issues") == [] else "fail"
    entry.update(
        status=status,
        observations=observations,
        recorder={"present": True, "sample_count": len(rows)},
        fault_activation={"activated": bool(fault_seen), "kind": CONSOLE_FAULT_KIND,
                          "observations": fault_seen},
    )
    return BASE._fragment(sha, digest, [entry])


# --------------------------------------------------------------------------
# E4 performance gate
# --------------------------------------------------------------------------

PERF_ID = "e4-performance-gate"
PERF_IDENTITY = "performance-gate"
PERF_PASS = {"no_significant_difference", "within_budget", "faster", "pass"}
PERF_FAIL = {"slower", "fail"}


def build_performance(artifacts, candidate_sha):
    artifacts = Path(artifacts)
    sha = _require_sha(candidate_sha)
    gate = read_json(artifacts / "gate.json")
    final = (gate or {}).get("final") or {}
    attempt = _int(final.get("attempt"))
    if attempt is None:
        raise EvidenceError("gate.json names no final attempt")
    attempt_dir = artifacts / f"attempt-{attempt}"
    report = read_json(attempt_dir / "report.json")
    host = read_json(attempt_dir / "observations" / "host.json")
    comparison = read_json(attempt_dir / "comparison.json")
    candidate = (comparison.get("candidate") or {}) if isinstance(comparison, dict) else {}
    provenance_side = candidate.get("provenance") or {}
    cand_sha = provenance_side.get("git_sha")
    if cand_sha != sha:
        raise EvidenceError(f"performance comparison measured candidate {cand_sha!r}, not {sha!r}")
    image_id = str(provenance_side.get("image_id") or "")
    decision = report.get("decision") or {}
    overall = str(final.get("overall") or report.get("overall") or "")
    verdicts = {
        key: str((decision.get(key) or {}).get("verdict") or "")
        for key in ("target_base", "fixed_baseline", "slos")
    }
    provenance = report.get("provenance") or {}
    observations = []
    if overall and not overall.startswith("inconclusive"):
        observations.append("gate_resolved")
    if verdicts["target_base"] in PERF_PASS:
        observations.append("target_base_non_inferior")
    if verdicts["fixed_baseline"] in PERF_PASS:
        observations.append("fixed_baseline_non_inferior")
    if verdicts["slos"] in PERF_PASS:
        observations.append("slos_pass")
    if _nonempty(host.get("host_id")):
        observations.append("runner_identity_recorded")
    if not any(provenance.get(key) for key in ("missing", "mismatched", "instrumented", "invalid")):
        observations.append("uninstrumented_provenance")
    if (report.get("strict_gate") or {}).get("blocking") is False and "gate_resolved" in observations:
        status = "pass"
    elif overall in PERF_FAIL or any(value in PERF_FAIL for value in verdicts.values()):
        status = "fail"
    else:
        status = "inconclusive"
    entry = {
        "id": PERF_ID,
        "status": status,
        "artifact_identity": PERF_IDENTITY,
        "candidate_sha": sha,
        "topology": {"kind": "docker-performance", "replicas": 1, "persistence": False, "engine": "docker"},
        "mode": {"execution": "local", "surface": "http+cli+browser"},
        "feature_flags": {},
        "observations": observations,
        "fault_activation": _no_fault(),
        "recorder": {"present": True, "sample_count": _int(report.get("min_samples")) or 0},
        "checker": {"timed_out": False},
        "performance": {
            "overall": overall,
            "exit_code": final.get("exit_code"),
            "attempt": attempt,
            "verdicts": verdicts,
            "host_id": host.get("host_id"),
            "control": report.get("control"),
            "strict_gate": report.get("strict_gate"),
        },
    }
    if not DIGEST.match(image_id):
        raise EvidenceError(f"performance comparison names no candidate image id ({image_id!r})")
    entry["observed_candidate_image_id"] = image_id
    return BASE._fragment(sha, image_id, [entry])


# --------------------------------------------------------------------------
# Report assembly
# --------------------------------------------------------------------------

def build_report(fragment_paths, gate, digest=None):
    """One report per lane gate. The lane names its own artifact identity.

    A lane that runs no release image (fuzzing, coverage) names the artifact it
    DID run as the report digest; scenario rows that require the release digest
    still fail the checker unless they carry the same one.
    """
    if not isinstance(gate, str) or not gate.strip():
        raise EvidenceError("--gate is required")
    sha = None
    scenarios = []
    seen = set()
    digests = set()
    if not fragment_paths:
        raise EvidenceError("no evidence fragments were given")
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
            raise EvidenceError(f"{path} candidate_sha {value!r} does not match the other fragments ({sha!r})")
        sha = value
        if fragment.get("candidate_digest"):
            digests.add(fragment["candidate_digest"])
        for scenario in fragment.get("scenarios") or []:
            sid = scenario.get("id") if isinstance(scenario, dict) else None
            if not sid:
                raise EvidenceError(f"{path} has a scenario without an id")
            if sid in seen:
                raise EvidenceError(f"scenario {sid!r} appears in more than one fragment")
            seen.add(sid)
            scenarios.append(scenario)
    if not scenarios:
        raise EvidenceError("evidence fragments contain no scenarios")
    if digest is None:
        if len(digests) != 1:
            raise EvidenceError(f"fragments carry {len(digests)} candidate identities {sorted(digests)}; want exactly one")
        digest = digests.pop()
    if not DIGEST.match(digest or ""):
        raise EvidenceError(f"report digest is not sha256:<64 hex>: {digest!r}")
    return {
        "candidate_sha": sha,
        "candidate_digest": digest,
        "gate": gate,
        "gate_enabled": True,
        "disabled_gates": [],
        "scenarios": scenarios,
    }


def parse_args(argv=None):
    parser = argparse.ArgumentParser(prog="collect-lane-evidence.py", description=__doc__.splitlines()[0])
    sub = parser.add_subparsers(dest="command", required=True)

    core = sub.add_parser("core", help="B3 TestCore fragment")
    core.add_argument("--artifacts", required=True)
    core.add_argument("--candidate-sha", required=True)
    core.add_argument("--out", required=True)

    life = sub.add_parser("lifecycle", help="F4/F2 lifecycle qualification fragment")
    life.add_argument("--artifacts", required=True)
    life.add_argument("--mode", required=True, choices=sorted(LIFECYCLE))
    life.add_argument("--candidate-sha", required=True)
    life.add_argument("--expected-image-id", default=None,
                      help="image id the workflow loaded for this SHA (standalone override binding)")
    life.add_argument("--out", required=True)

    gen = sub.add_parser("generated", help="C2 fuzz and/or C3 oracle fragment")
    gen.add_argument("--fuzz-dir", default=None, help="scripts/fuzz-tests.sh artifact directory (C2)")
    gen.add_argument("--oracle-log", default=None, help="scripts/validate-test-oracles.sh output (C3)")
    gen.add_argument("--candidate-sha", required=True)
    gen.add_argument("--builder-image-id", required=True)
    gen.add_argument("--out", required=True)

    cov = sub.add_parser("coverage", help="G2 coverage fragment")
    cov.add_argument("--report", required=True)
    cov.add_argument("--candidate-sha", required=True)
    cov.add_argument("--out", required=True)

    con = sub.add_parser("console", help="D3 console journey fragment")
    con.add_argument("--artifacts", required=True)
    con.add_argument("--candidate-sha", required=True)
    con.add_argument("--playwright-exit", type=int, required=True)
    con.add_argument("--out", required=True)

    perf = sub.add_parser("performance", help="E4 performance-gate fragment")
    perf.add_argument("--artifacts", required=True)
    perf.add_argument("--candidate-sha", required=True)
    perf.add_argument("--out", required=True)

    rep = sub.add_parser("report", help="Merge fragments into one lane report")
    rep.add_argument("--out", required=True)
    rep.add_argument("--gate", required=True)
    rep.add_argument("--digest", default=None)
    rep.add_argument("fragments", nargs="+")
    return parser.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    try:
        if args.command == "core":
            payload = build_core(args.artifacts, args.candidate_sha)
        elif args.command == "lifecycle":
            payload = build_lifecycle(args.artifacts, args.mode, args.candidate_sha, args.expected_image_id)
        elif args.command == "generated":
            payload = build_generated(args.fuzz_dir, args.oracle_log, args.candidate_sha, args.builder_image_id)
        elif args.command == "coverage":
            payload = build_coverage(args.report, args.candidate_sha)
        elif args.command == "console":
            payload = build_console(args.artifacts, args.candidate_sha, args.playwright_exit)
        elif args.command == "performance":
            payload = build_performance(args.artifacts, args.candidate_sha)
        else:
            payload = build_report(args.fragments, args.gate, args.digest)
    except EvidenceError as err:
        print(f"collect-lane-evidence: {err}", file=sys.stderr)
        return 1
    BASE._write(args.out, payload)
    print(f"collect-lane-evidence: wrote {args.out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
