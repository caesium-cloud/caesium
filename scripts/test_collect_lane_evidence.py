"""Regression checks for the G6 system-suite lane evidence consumer.

Each test drives `scripts/collect-lane-evidence.py` over an artifact directory
shaped like the one its lane really leaves behind (the record shapes are copied
from live runs: W6's 12/12 TestCore pass, W8's standalone and cluster lifecycle
qualifications, W7's D3 journey, W8-γ's performance gate), then feeds the result
to the real `scripts/check-test-evidence.py` against the committed manifest.
A lane that stops producing evidence must fail closed, never silently pass.
"""

import copy
import json
from pathlib import Path
import runpy
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts/collect-lane-evidence.py"
CHECKER = ROOT / "scripts/check-test-evidence.py"
MANIFEST = ROOT / "test/contracts/scenarios.json"
EARLY = runpy.run_path(str(ROOT / "scripts/test_collect_evidence.py"))

SHA = EARLY["SHA"]
DIGEST = EARLY["DIGEST"]
IMAGE_ID = "sha256:" + "5a" * 32
BUILDER_ID = "sha256:" + "b1" * 32
COVERAGE_ID = "sha256:" + "c0" * 32

CORE_SUBTESTS = (
    "terminal_no_regress", "frozen_retry_recipe", "fan_in_predecessors", "wrong_token_internal",
    "invalid_mtls_peer", "cancel_completion_race", "cancel_post_commit_fence",
    "commit_before_response_loss", "stale_generation_complete", "worker_unreachable_bench",
    "quorum_loss_uncertain_write", "durable_event_before_delivery_crash",
)


def run(*args):
    return subprocess.run([sys.executable, str(SCRIPT), *args], capture_output=True, text=True)


QUORUM_ROWS = ("b3-quorum-loss-uncertain-write", "b3-split-heal-2-1")


def check(report, gate, manifest=None):
    with tempfile.TemporaryDirectory() as tmp:
        path = Path(tmp) / "report.json"
        path.write_text(json.dumps(report))
        manifest_path = MANIFEST
        if manifest is not None:
            manifest_path = Path(tmp) / "scenarios.json"
            manifest_path.write_text(json.dumps(manifest))
        return subprocess.run(
            [sys.executable, str(CHECKER), "--manifest", str(manifest_path), "--report", str(path),
             "--require", gate, "--strict"],
            capture_output=True, text=True,
        )


def quorum_rows_promoted():
    """The committed manifest with the quorum-loss rows proven under nightly-core,
    as they will be once TestCore records an identity for a timed-out mutation."""
    manifest = json.loads(MANIFEST.read_text())
    for row in manifest["scenarios"]:
        if row["id"] in QUORUM_ROWS:
            row["status"], row["gates"] = "proven", ["nightly-core"]
    return manifest


def core_records():
    """Record shapes TestCore writes (test/robustness/core_*.go), from a live pass."""
    stale = {
        "run_id": "run-stale", "owner_paused": "caesium-1", "held_seconds": 42.0, "past_lease": True,
        "lease_before": {"RunID": "run-stale", "OwnerNode": "10.244.3.3:9001", "Generation": 1},
        "lease_after": {"RunID": "run-stale", "OwnerNode": "10.244.1.3:9001", "Generation": 2},
        "refusal_code": "stale_generation", "complete_status": 409, "public_status": "succeeded",
        "pause_state_before": "RUNNING", "pause_state_paused": "PAUSED", "redacted": True,
    }
    kill = EARLY["kill_evidence"]("f" * 64)
    return {
        "terminal_no_regress": {"run_id": "run-terminal", "owner": "caesium-0", "public_status": "succeeded",
                                "complete_status": 409, "refusal_code": "terminal_run", "redacted": True},
        "frozen_retry_recipe": {"failed_run": "run-retry", "retry_status": 202, "after_retry": "failed",
                                "boom_starts": 2, "rejected_run": "run-rejected", "rejected_status": 409,
                                "frozen_digest": "ab" * 32, "redacted": True},
        "fan_in_predecessors": {"run_id": "run-fanin", "public_status": "succeeded", "left_complete": 1,
                                "right_complete": 1, "join_starts": 1, "redacted": True},
        "wrong_token_internal": {"run_id": "run-token", "complete_status": 401, "dispatch_status": 401,
                                 "principal": "bearer", "token_class": "wrong", "kind": "wrong-token",
                                 "target_complete": "caesium-0", "target_dispatch": "caesium-1",
                                 "state_digest": "cd" * 32, "redacted": True},
        "invalid_mtls_peer": {"run_id": "run-mtls", "kind": "invalid-cert", "handshake": "failed",
                              "error_class": "tls", "control_status": 200, "control_reached": True,
                              "state_digest": "ef" * 32, "redacted": True},
        "cancel_completion_race": {
            "first_run": "run-first", "second_run": "run-second", "first_status": "cancelled",
            "second_status": "succeeded", "completion_status": 409, "completion_code": "terminal_run",
            "gate_arrivals": [{"name": "complete"}, {"name": "replace"}],
            "persisted_scope": {"Complete": True}, "persisted_events": [{"Sequence": 70}],
            "histories": ["run-first", "run-second"], "redacted": True},
        "cancel_post_commit_fence": {
            "first_run": "run-pc1", "second_run": "run-pc2", "first_status": "cancelled",
            "second_status": "succeeded", "old_complete_status": 409, "old_complete_code": "terminal_run",
            "old_lease_absence_proven": False, "successor_starts": 0,
            "replacement_202_ack": "not_process_death", "completion_phase": "post_cancel_commit",
            "histories": ["run-pc1", "run-pc2"], "redacted": True},
        "commit_before_response_loss": {
            "client_error": "Post \"http://127.0.0.1:1/v1/jobs/x/run\": EOF", "upstream_status": 202,
            "reconciled_run": "run-loss", "final_status": "succeeded", "outcome": "possibly_committed",
            "heal_run": "run-heal", "heal_status": 202, "redacted": True},
        "stale_generation_complete": stale,
        "worker_unreachable_bench": {
            "worker": "caesium-1", "owner": "caesium-0", "network_error_rise": 2, "sent_before": 28,
            "sent_after": 38, "recovered_claim": {"claimed_by": "10.244.3.3:9001"},
            "run_ids": ["run-bench"], "pause_state_paused": "PAUSED", "pause_state_resumed": "RUNNING",
            "redacted": True},
        "quorum_loss_uncertain_write": {
            "minority": "caesium-1", "majority": "caesium-0", "majority_run": "run-majority",
            "majority_final": "succeeded", "fault_plans": 4, "timeout_is_reject": False,
            "attempts": [{"Status": 0, "Err": "deadline", "Outcome": "possibly_committed", "RunID": ""},
                         {"Status": 0, "Err": "deadline", "Outcome": "possibly_committed", "RunID": ""}],
            "reconciled": [{"identity": "unknown_possibly_committed"}, {"identity": "unknown_possibly_committed"}],
            "redacted": True},
        "durable_event_before_delivery_crash": {
            "run_id": "run-durable", "held_sequence": 272, "publisher": "caesium-2", "hook_members": 3,
            "row_while_held": {"Sequence": 272, "BusPending": True, "DispatchedAt": ""},
            "row_after_kill": {"Sequence": 272, "BusPending": False, "DispatchedAt": "2026-09-26T03:22:37Z"},
            "kill_evidence": kill, "redacted": True},
    }


def reconcile_minority_identities(records):
    """The minority attempts carry a recorded run id each, read back after heal.

    The live W6 record above never got that far: both timed-out triggers
    returned no run id, so runQuorumLoss wrote `unknown_possibly_committed`.
    """
    record = records["quorum_loss_uncertain_write"]
    for index, attempt in enumerate(record["attempts"]):
        attempt["RunID"] = f"run-minority-{index}"
    record["reconciled"] = [
        {"deadline": deadline, "outcome": "possibly_committed", "status": 0,
         "present": True, "status_after_heal": "succeeded"}
        for deadline in ("5s", "15s")
    ]


def core_events():
    runs = ["run-terminal", "run-retry", "run-rejected", "run-fanin", "run-token", "run-mtls", "run-first",
            "run-second", "run-pc1", "run-pc2", "run-loss", "run-heal", "run-stale", "run-bench",
            "run-majority"]
    return [{"kind": "start", "run_id": run_id, "step": "block"} for run_id in runs]


def write_core_artifacts(root, *, instrumented=True, outcomes=None, drop=(), mutate=None):
    art = EARLY["write_robustness_artifacts"](root)
    for name in ("owner_is_leader.json", "owner_is_not_leader.json", "events.json"):
        (art / "records" / name).unlink()
    outcomes = outcomes or {}
    log = "=== RUN   TestCore\n    core_test.go:60: dqlite leader=10.244.1.3:9001 members=3\n"
    for subtest in CORE_SUBTESTS:
        log += f"=== RUN   TestCore/{subtest}\n"
        if subtest == "quorum_loss_uncertain_write":
            for tag in ("p0", "p0r", "p1", "p1r"):
                log += f"    core_recovery.go:617: split plan {tag} drop raft=5 internal=0\n"
    parent = "FAIL" if "FAIL" in outcomes.values() else "PASS"
    log += f"--- {parent}: TestCore (299.68s)\n"
    for subtest in CORE_SUBTESTS:
        if subtest == "durable_event_before_delivery_crash" and not instrumented:
            log += f"    --- SKIP: TestCore/{subtest} (0.00s)\n"
            continue
        log += f"    --- {outcomes.get(subtest, 'PASS')}: TestCore/{subtest} (10.00s)\n"
    (art / "robustness.test.log").write_text(log)
    records = core_records()
    if mutate:
        mutate(records)
    (art / "records" / "core_topology.json").write_text(json.dumps({"instrumented": instrumented}))
    (art / "records" / "core_events.json").write_text(json.dumps(core_events()))
    for subtest, record in records.items():
        if subtest in drop:
            continue
        if subtest == "durable_event_before_delivery_crash" and not instrumented:
            continue
        (art / "records" / f"{subtest}.json").write_text(json.dumps(record))
    return art


class CoreFragmentTests(unittest.TestCase):
    def collect(self, **kwargs):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        art = write_core_artifacts(Path(tmp.name) / "art", **kwargs)
        out = Path(tmp.name) / "core.json"
        result = run("core", "--artifacts", str(art), "--candidate-sha", SHA, "--out", str(out))
        return result, out

    def report(self, out, gate):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        path = Path(tmp.name) / "report.json"
        result = run("report", "--gate", gate, "--out", str(path), str(out))
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(path.read_text())

    def rows(self, out):
        return {item["id"]: item for item in json.loads(out.read_text())["scenarios"]}

    def test_instrumented_run_satisfies_the_core_gate(self):
        result, out = self.collect(mutate=reconcile_minority_identities)
        self.assertEqual(result.returncode, 0, result.stderr)
        rows = self.rows(out)
        self.assertEqual(len(rows), 14)
        self.assertTrue(all(row["status"] == "pass" for row in rows.values()))
        for sid in ("b3-quorum-loss-uncertain-write", "b3-split-heal-2-1"):
            self.assertEqual(rows[sid]["reconciliation"],
                             {"identities": ["reconciled", "reconciled"], "reconciled": 2, "unresolved": 0})
        quorum = rows["b3-quorum-loss-uncertain-write"]
        self.assertIn("post_heal_identity_reconciliation", quorum["observations"])
        self.assertIn("identified_mutation_sent_during_fault", quorum["fault_activation"]["observations"])
        report = self.report(out, "nightly-core")
        checked = check(report, "nightly-core")
        self.assertEqual(checked.returncode, 0, checked.stdout + checked.stderr)
        # Reconciled identities would also satisfy the gate with the quorum rows promoted.
        promoted = check(report, "nightly-core", quorum_rows_promoted())
        self.assertEqual(promoted.returncode, 0, promoted.stdout + promoted.stderr)

    def test_unknown_write_identities_stay_inconclusive_after_heal(self):
        # The unchanged fixture: both timed-out minority triggers returned no
        # run id, so nothing was reconciled by identity after healing.
        result, out = self.collect()
        self.assertEqual(result.returncode, 0, result.stderr)
        rows = self.rows(out)
        quorum, split = rows["b3-quorum-loss-uncertain-write"], rows["b3-split-heal-2-1"]
        for row in (quorum, split):
            self.assertEqual(row["status"], "inconclusive")
            self.assertEqual(row["reconciliation"], {
                "identities": ["unknown_possibly_committed", "unknown_possibly_committed"],
                "reconciled": 0, "unresolved": 2,
            })
            self.assertNotIn("fault_healed_before_final_read", row["fault_activation"]["observations"])
        # The possibly-committed classification is preserved...
        self.assertIn("timeout_recorded_as_possibly_committed", quorum["observations"])
        self.assertIn("no_rejection_inferred_from_client_timeout", quorum["observations"])
        self.assertIn("minority_write_possibly_committed", split["observations"])
        # ...but no reconciliation is claimed for it.
        self.assertNotIn("post_heal_identity_reconciliation", quorum["observations"])
        self.assertNotIn("identified_mutation_sent_during_fault", quorum["fault_activation"]["observations"])
        self.assertNotIn("quorum_restored_after_heal", split["observations"])
        others = [row for sid, row in rows.items() if sid not in QUORUM_ROWS]
        self.assertTrue(all(row["status"] == "pass" for row in others))
        report = self.report(out, "nightly-core")
        # The committed manifest does not claim the quorum rows, so the gate
        # passes on the other rows without certifying them...
        manifest = {row["id"]: row for row in json.loads(MANIFEST.read_text())["scenarios"]}
        for sid in QUORUM_ROWS:
            self.assertEqual((manifest[sid]["status"], manifest[sid]["gates"]), ("unproven", []))
        committed = check(report, "nightly-core")
        self.assertEqual(committed.returncode, 0, committed.stdout + committed.stderr)
        # ...and this fixture can never certify them once they are promoted.
        checked = check(report, "nightly-core", quorum_rows_promoted())
        self.assertEqual(checked.returncode, 1, checked.stdout + checked.stderr)
        output = checked.stdout + checked.stderr
        for sid in QUORUM_ROWS:
            self.assertIn(f"insufficient-samples={sid}", output)

    def test_one_unresolved_identity_withholds_the_reconciliation(self):
        def mutate(records):
            reconcile_minority_identities(records)
            record = records["quorum_loss_uncertain_write"]
            record["attempts"][1]["RunID"] = ""
            record["reconciled"][1] = {"deadline": "15s", "outcome": "possibly_committed", "status": 0,
                                       "identity": "unknown_possibly_committed"}
        result, out = self.collect(mutate=mutate)
        self.assertEqual(result.returncode, 0, result.stderr)
        quorum = self.rows(out)["b3-quorum-loss-uncertain-write"]
        self.assertEqual(quorum["status"], "inconclusive")
        self.assertEqual(quorum["reconciliation"]["identities"], ["reconciled", "unknown_possibly_committed"])
        self.assertNotIn("post_heal_identity_reconciliation", quorum["observations"])
        report = self.report(out, "nightly-core")
        self.assertEqual(check(report, "nightly-core", quorum_rows_promoted()).returncode, 1)

    def test_failed_quorum_subtest_stays_failed_with_unresolved_identities(self):
        result, out = self.collect(outcomes={"quorum_loss_uncertain_write": "FAIL"})
        self.assertEqual(result.returncode, 0, result.stderr)
        rows = self.rows(out)
        self.assertEqual(rows["b3-quorum-loss-uncertain-write"]["status"], "fail")
        self.assertEqual(rows["b3-split-heal-2-1"]["status"], "fail")

    def test_release_image_run_omits_the_instrumented_row(self):
        result, out = self.collect(instrumented=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("b3-durable-event-before-delivery-crash", self.rows(out))
        # The gate needs the instrumented case: a release-image run cannot pass it.
        missing = check(self.report(out, "nightly-core"), "nightly-core")
        self.assertEqual(missing.returncode, 1)
        self.assertIn("missing-scenario=b3-durable-event-before-delivery-crash", missing.stdout + missing.stderr)

    def test_failed_subtest_and_missing_record_never_pass(self):
        result, out = self.collect(outcomes={"fan_in_predecessors": "FAIL"}, drop=("terminal_no_regress",))
        self.assertEqual(result.returncode, 0, result.stderr)
        rows = self.rows(out)
        self.assertEqual(rows["b3-fan-in-predecessors"]["status"], "fail")
        self.assertEqual(rows["b3-terminal-no-regress"]["status"], "inconclusive")
        self.assertFalse(rows["b3-terminal-no-regress"]["recorder"]["present"])
        # With the parent FAIL, even a subtest PASS line is not a pass.
        self.assertTrue(all(row["status"] != "pass" for row in rows.values()))
        self.assertEqual(check(self.report(out, "nightly-core"), "nightly-core").returncode, 1)

    def test_fields_that_contradict_the_contract_drop_their_observation(self):
        def mutate(records):
            records["stale_generation_complete"]["complete_status"] = 200
            records["wrong_token_internal"]["complete_status"] = 200
            records["quorum_loss_uncertain_write"]["attempts"][0]["Outcome"] = "rejected"
        result, out = self.collect(mutate=mutate)
        self.assertEqual(result.returncode, 0, result.stderr)
        rows = self.rows(out)
        self.assertNotIn("stale_generation_rejected_409", rows["b3-stale-generation-complete"]["observations"])
        self.assertNotIn("completion_rejected_before_commit",
                         rows["b3-stale-generation-complete"]["fault_activation"]["observations"])
        self.assertNotIn("valid_cert_wrong_token_401", rows["b3-wrong-token-internal"]["observations"])
        # internal TLS is reported only when a valid-certificate exchange is proven.
        self.assertNotIn("internal_tls", rows["b3-wrong-token-internal"]["feature_flags"])
        self.assertEqual(rows["b3-invalid-mtls-peer"]["feature_flags"]["internal_tls"], "provisioned")
        self.assertNotIn("no_rejection_inferred_from_client_timeout",
                         rows["b3-quorum-loss-uncertain-write"]["observations"])
        checked = check(self.report(out, "nightly-core"), "nightly-core", quorum_rows_promoted())
        self.assertEqual(checked.returncode, 1)
        output = checked.stdout + checked.stderr
        for sid in ("b3-stale-generation-complete", "b3-wrong-token-internal", "b3-quorum-loss-uncertain-write"):
            self.assertIn(sid, output)

    def test_split_without_matched_drop_counters_is_not_fault_evidence(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        art = write_core_artifacts(Path(tmp.name) / "art")
        log = (art / "robustness.test.log").read_text().replace("drop raft=5", "drop raft=0", 1)
        (art / "robustness.test.log").write_text(log)
        out = Path(tmp.name) / "core.json"
        self.assertEqual(run("core", "--artifacts", str(art), "--candidate-sha", SHA, "--out", str(out)).returncode, 0)
        rows = self.rows(out)
        self.assertNotIn("two_voters_unreachable",
                         rows["b3-quorum-loss-uncertain-write"]["fault_activation"]["observations"])
        self.assertNotIn("minority_isolated_from_both_peers",
                         rows["b3-split-heal-2-1"]["fault_activation"]["observations"])

    def test_foreign_candidate_is_refused(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        art = write_core_artifacts(Path(tmp.name) / "art")
        result = run("core", "--artifacts", str(art), "--candidate-sha", "e" * 40,
                     "--out", str(Path(tmp.name) / "core.json"))
        self.assertEqual(result.returncode, 1)
        self.assertIn("versions.json candidate_sha", result.stderr)


def standalone_record(**overrides):
    manifest = json.loads(MANIFEST.read_text())
    row = next(item for item in manifest["scenarios"] if item["id"] == "f4-standalone-lifecycle")
    expected = [item["id"] for item in row["expected_observations"]]
    recorded = {"recorded-in-flight-run-after-upgrade", "recorded-rollback-previous-release-on-migrated-volume",
                "recorded-shard-count-change"}
    record = {
        "schema_version": 1,
        "kind": "caesium-lifecycle-qualification",
        "candidate_sha": SHA,
        "lifecycle_id": "lifecycle-fixture",
        "topology": {"nodes": 1, "database_shards": 1},
        "images": {"candidate": {"image_id": IMAGE_ID}},
        "server_env": "-e CAESIUM_LOG_LEVEL=debug -e CAESIUM_DATABASE_SHARDS=1 -e CAESIUM_RUN_QUEUE_ENABLED=true "
                      "-e CAESIUM_MANUAL_TRIGGER_API_KEY=caesium-lifecycle-manual-key",
        "candidate_provenance": {"provenance": "built-by-this-run", "built_by_this_run": True, "verified": True,
                                 "override": False, "image_id": IMAGE_ID},
        "expected_cases": expected,
        "cases": [{"name": name, "status": "recorded-outcome" if name in recorded else "pass"} for name in expected],
        "result": "pass",
    }
    record.update(overrides)
    return record


def cluster_record(**overrides):
    manifest = json.loads(MANIFEST.read_text())
    row = next(item for item in manifest["scenarios"] if item["id"] == "f2-cluster-lifecycle")
    expected = [item["id"] for item in row["expected_observations"]]
    record = {
        "kind": "caesium-cluster-lifecycle-qualification",
        "schema_version": 1,
        "candidate_sha": SHA,
        "lifecycle_id": "lifecycle-cluster-fixture",
        "topology": {"replicas": 3, "persistent": True, "database_shards": 1},
        "candidate_image": {"image_id": IMAGE_ID, "source_docker_image_id": IMAGE_ID},
        "expected_cases": expected,
        "cases": [{"name": name, "status": "recorded-outcome" if name == "rollback-recorded-outcome" else "pass"}
                  for name in expected],
        "result": "pass",
    }
    record.update(overrides)
    return record


LIVE_MANIFEST = """\
          env:
            - name: CAESIUM_EXECUTION_MODE
              value: distributed
            - name: CAESIUM_RUN_OWNER_ENABLED
              value: "true"
            - name: CAESIUM_DATABASE_VOTERS
              value: "3"
            - name: CAESIUM_DATABASE_SHARDS
              value: "1"
            - name: CAESIUM_INTERNAL_WAKEUP_TOKEN
              value: must-not-leak
"""


class LifecycleFragmentTests(unittest.TestCase):
    def collect(self, mode, record, *extra, files=None):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        art = Path(tmp.name) / "art"
        art.mkdir()
        name = "qualification.json" if mode == "standalone" else "cluster-qualification.json"
        (art / name).write_text(json.dumps(record))
        for filename, text in (files or {}).items():
            (art / filename).write_text(text)
        out = Path(tmp.name) / "lifecycle.json"
        result = run("lifecycle", "--mode", mode, "--artifacts", str(art), "--candidate-sha", SHA,
                     *extra, "--out", str(out))
        return result, out

    def gate(self, out, gate):
        fragment = json.loads(out.read_text())
        report = {"candidate_sha": fragment["candidate_sha"], "candidate_digest": fragment["candidate_digest"],
                  "gate_enabled": True, "disabled_gates": [], "scenarios": fragment["scenarios"]}
        return check(report, gate)

    def test_built_by_this_run_standalone_qualification_passes(self):
        result, out = self.collect("standalone", standalone_record())
        self.assertEqual(result.returncode, 0, result.stderr)
        row = json.loads(out.read_text())["scenarios"][0]
        self.assertEqual(row["status"], "pass")
        self.assertEqual(len(row["observations"]), 20)
        self.assertNotIn("CAESIUM_MANUAL_TRIGGER_API_KEY", row["feature_flags"])
        checked = self.gate(out, "lifecycle")
        self.assertEqual(checked.returncode, 0, checked.stdout + checked.stderr)

    def test_supplied_image_needs_its_producer_recorded_id(self):
        supplied = standalone_record(candidate_provenance={
            "provenance": "supplied/unverified", "built_by_this_run": False, "verified": False,
            "override": True, "image_id": IMAGE_ID})
        _, out = self.collect("standalone", supplied)
        self.assertEqual(json.loads(out.read_text())["scenarios"][0]["status"], "fail")
        _, out = self.collect("standalone", supplied, "--expected-image-id", "sha256:" + "99" * 32)
        self.assertEqual(json.loads(out.read_text())["scenarios"][0]["status"], "fail")
        _, out = self.collect("standalone", supplied, "--expected-image-id", IMAGE_ID)
        self.assertEqual(json.loads(out.read_text())["scenarios"][0]["status"], "pass")
        self.assertEqual(self.gate(out, "lifecycle").returncode, 0)

    def test_blocked_or_missing_cases_never_pass(self):
        record = standalone_record()
        record["cases"] = record["cases"][:-1]
        record["result"] = "fail"
        _, out = self.collect("standalone", record)
        row = json.loads(out.read_text())["scenarios"][0]
        self.assertNotEqual(row["status"], "pass")
        self.assertEqual(self.gate(out, "lifecycle").returncode, 1)
        record = standalone_record()
        record["cases"][3]["status"] = "blocked"
        record["result"] = "fail"
        _, out = self.collect("standalone", record)
        self.assertEqual(json.loads(out.read_text())["scenarios"][0]["status"], "fail")

    def test_foreign_or_wrong_kind_record_is_refused(self):
        result, _ = self.collect("standalone", standalone_record(candidate_sha="e" * 40))
        self.assertEqual(result.returncode, 1)
        self.assertIn("not the tested candidate", result.stderr)
        result, _ = self.collect("standalone", cluster_record())
        self.assertEqual(result.returncode, 1)

    def test_cluster_qualification_reads_flags_from_the_live_manifest(self):
        result, out = self.collect("cluster", cluster_record(), files={"manifest-upgraded.yaml": LIVE_MANIFEST})
        self.assertEqual(result.returncode, 0, result.stderr)
        row = json.loads(out.read_text())["scenarios"][0]
        self.assertEqual(row["mode"]["execution"], "distributed")
        self.assertNotIn("CAESIUM_INTERNAL_WAKEUP_TOKEN", row["feature_flags"])
        checked = self.gate(out, "nightly-cluster-lifecycle")
        self.assertEqual(checked.returncode, 0, checked.stdout + checked.stderr)
        # Without the live manifest the flags are unobserved and the gate fails.
        _, out = self.collect("cluster", cluster_record())
        self.assertEqual(self.gate(out, "nightly-cluster-lifecycle").returncode, 1)


FUZZ_TARGETS = {
    "pkg/jobdef/schemacompat": ["FuzzCompare"],
    "internal/jobdef/diff": ["FuzzDecodeDefinitions"],
    "internal/trigger/cron": ["FuzzExtractExpression", "FuzzExtractLocation"],
    "internal/run": ["FuzzTaskExecutionDescriptorRoundTrip", "FuzzMergeDescriptorSecretRefs",
                     "FuzzValidateCheckpointBlob", "FuzzRecoverRunStateTerminalRows"],
}


def write_fuzz(root, *, crash=None, skip_ok=(), drift=False, result="all declared targets explored", cpus=(1, 2, 4),
               corpus_suffix="-cache", no_corpus=()):
    fuzz = Path(root)
    (fuzz / "corpus").mkdir(parents=True)
    (fuzz / "candidate-sha.txt").write_text(SHA + "\n")
    lines = ["=== discovering fuzz targets ==="]
    for pkg, targets in FUZZ_TARGETS.items():
        discovered = targets + (["FuzzExtra"] if drift and pkg == "internal/run" else [])
        lines.append(f"./{pkg}: declared=[{' '.join(targets)}] discovered=[{' '.join(discovered)} ]")
    lines.append("=== bounded fuzzing (fuzztime=10s per target) ===")
    for targets in FUZZ_TARGETS.values():
        for target in targets:
            # Packages without committed testdata/fuzz export only <target>-cache.
            corpus = fuzz / "corpus" / f"{target}{corpus_suffix}"
            corpus.mkdir()
            if target not in no_corpus:
                (corpus / "seed").write_text("go test fuzz v1\n")
            lines.append(f"--- {target} ---")
            if target == crash:
                lines.append(f"CRASH: {target} failed (exit 1) after 12s")
            elif target not in skip_ok:
                lines.append(f"OK: {target} — 23s, execs=61234 (baseline=8), new_interesting_total=123")
    lines.append("=== concurrency repeat matrix ===")
    for cpu in cpus:
        lines.append(f"OK: -cpu={cpu} pass=72 fail=0")
    lines.append(f"RESULT: {result}" if result != "FAILED" else "RESULT: FAILED")
    (fuzz / "summary.txt").write_text("\n".join(lines) + "\n")
    return fuzz


MUTATIONS = ("lost-ack", "retry-drops-ack", "accept-stale-generation", "weaken-refusal-inert",
             "drop-durable-replay", "partial-fan-in", "omit-catch-up", "aggregate-effects")


def oracle_log(*, sha=SHA, escaped=(), completed=True):
    lines = [f"oracle candidate={sha}",
             "model-candidate: pass ['TestOracleRegressionLostAcknowledgedState'] (go exit 0)",
             "history-candidate: pass ['TestC3MissingReplay'] (go exit 0)",
             "robustness-candidate: pass ['TestC3FanInStartedTooEarly'] (go exit 0)"]
    for name in MUTATIONS:
        lines.append(f"mutation={name} bad_sha={'a' * 40} patch=test/model/testdata/mutations/{name}.patch "
                     f"patch_blob={'b' * 40}")
        if name not in escaped:
            lines.append(f"{name}: fail ['TestSomething'] (go exit 1)")
    if completed:
        lines.append("oracle validation PASS: candidate accepted; all eight known-bad mutations rejected by named tests")
    return "\n".join(lines) + "\n"


class GeneratedFragmentTests(unittest.TestCase):
    def collect(self, *, fuzz=None, oracle=None):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        args = []
        if fuzz is not None:
            args += ["--fuzz-dir", str(write_fuzz(Path(tmp.name) / "fuzz", **fuzz))]
        if oracle is not None:
            path = Path(tmp.name) / "oracles.log"
            path.write_text(oracle)
            args += ["--oracle-log", str(path)]
        out = Path(tmp.name) / "generated.json"
        result = run("generated", *args, "--candidate-sha", SHA, "--builder-image-id", BUILDER_ID,
                     "--out", str(out))
        return result, out

    def gate(self, out, gate):
        fragment = json.loads(out.read_text())
        report = {"candidate_sha": SHA, "candidate_digest": fragment["candidate_digest"], "gate_enabled": True,
                  "disabled_gates": [], "scenarios": fragment["scenarios"]}
        return check(report, gate)

    def test_explored_targets_and_rejected_mutations_pass_their_gates(self):
        result, out = self.collect(fuzz={})
        self.assertEqual(result.returncode, 0, result.stderr)
        rows = json.loads(out.read_text())["scenarios"]
        self.assertEqual([row["id"] for row in rows], ["c2-native-fuzz"])
        self.assertEqual(self.gate(out, "generated-fuzz").returncode, 0)
        result, out = self.collect(oracle=oracle_log())
        self.assertEqual(result.returncode, 0, result.stderr)
        rows = json.loads(out.read_text())["scenarios"]
        self.assertEqual([row["id"] for row in rows], ["c3-oracle-mutations"])
        self.assertEqual(rows[0]["recorder"]["sample_count"], 8)
        checked = self.gate(out, "generated-oracles")
        self.assertEqual(checked.returncode, 0, checked.stdout + checked.stderr)

    def test_either_exported_corpus_counts_and_an_empty_one_does_not(self):
        for suffix in ("", "-cache"):
            with self.subTest(suffix=suffix):
                _, out = self.collect(fuzz={"corpus_suffix": suffix})
                self.assertIn("corpus_preserved", json.loads(out.read_text())["scenarios"][0]["observations"])
        _, out = self.collect(fuzz={"no_corpus": ("FuzzCompare",)})
        self.assertNotIn("corpus_preserved", json.loads(out.read_text())["scenarios"][0]["observations"])
        self.assertEqual(self.gate(out, "generated-fuzz").returncode, 1)

    def test_seed_only_crashing_or_drifting_fuzz_never_passes(self):
        for kwargs in ({"skip_ok": ("FuzzCompare",)}, {"crash": "FuzzCompare", "result": "FAILED"},
                       {"drift": True, "result": "FAILED"}, {"cpus": (1, 2)}):
            with self.subTest(**{k: str(v) for k, v in kwargs.items()}):
                _, out = self.collect(fuzz=kwargs)
                self.assertEqual(self.gate(out, "generated-fuzz").returncode, 1)

    def test_escaped_mutation_or_incomplete_validator_fails(self):
        for log in (oracle_log(escaped=("lost-ack",)), oracle_log(completed=False)):
            _, out = self.collect(oracle=log)
            row = json.loads(out.read_text())["scenarios"][0]
            self.assertEqual(row["status"], "fail")
            self.assertEqual(self.gate(out, "generated-oracles").returncode, 1)

    def test_foreign_candidate_or_no_input_is_refused(self):
        result, _ = self.collect(oracle=oracle_log(sha="e" * 40))
        self.assertEqual(result.returncode, 1)
        self.assertIn("oracle log names candidate", result.stderr)
        result, _ = self.collect()
        self.assertEqual(result.returncode, 1)


def coverage_report(**overrides):
    provenance = {"image_id": COVERAGE_ID}
    report = {
        "schema_version": 1,
        "candidate_sha": SHA,
        "contributions": {
            "cli": {"status": "complete", "provenance": provenance},
            "server": {"status": "complete", "provenance": provenance},
            "integration": {"status": "complete"},
            "browser": {"status": "complete"},
        },
        "all_surfaces": {"percent": 9.0},
        "write_to_read": {"status": "pass", "covered": True},
        "uncovered_changed_paths": [],
        "diff_coverage": {"changed_path_count": 0, "empty_diff": True},
        "ratchet": {"applied": True},
        "performance_instrumentation": False,
        "issues": [],
        "verdict": "pass",
    }
    report.update(overrides)
    return report


class CoverageFragmentTests(unittest.TestCase):
    def collect(self, report):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        path = Path(tmp.name) / "report.json"
        path.write_text(json.dumps(report))
        out = Path(tmp.name) / "coverage.json"
        return run("coverage", "--report", str(path), "--candidate-sha", SHA, "--out", str(out)), out

    def gate(self, out):
        fragment = json.loads(out.read_text())
        return check({"candidate_sha": SHA, "candidate_digest": fragment["candidate_digest"], "gate_enabled": True,
                      "disabled_gates": [], "scenarios": fragment["scenarios"]}, "coverage")

    def test_passing_collection_satisfies_the_gate(self):
        result, out = self.collect(coverage_report())
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.gate(out).returncode, 0)

    def test_failed_verdict_uncovered_diff_or_incomplete_profile_fails(self):
        for overrides in ({"verdict": "fail"}, {"uncovered_changed_paths": ["internal/run/store.go"], "verdict": "fail"},
                          {"verdict": "incomplete"}):
            with self.subTest(**{k: str(v) for k, v in overrides.items()}):
                _, out = self.collect(coverage_report(**overrides))
                self.assertEqual(self.gate(out).returncode, 1)
        report = coverage_report()
        report["contributions"]["browser"]["status"] = "incomplete"
        _, out = self.collect(report)
        self.assertEqual(self.gate(out).returncode, 1)

    def test_foreign_report_is_refused(self):
        result, _ = self.collect(coverage_report(candidate_sha="e" * 40))
        self.assertEqual(result.returncode, 1)


def d3_evidence(**overrides):
    run_id = "14e62cfe-3cc6-4946-8356-efd3e87776f1"
    evidence = {
        "serverImage": f"caesiumcloud/caesium:{SHA}",
        "runId": run_id,
        "fault": {"startedAt": "t0", "recordedAt": "t1", "signals": 2,
                  "headingBefore": "running", "headingDuring": "running"},
        "serviceForwardAttempts": 1,
        "serviceForwardAt": "t2",
        "takeover": {"terminalBeforeTakeover": None},
        "durable": {"status": "succeeded"},
        "console": {
            "headingStatus": "succeeded", "reloadedHeadingStatus": "succeeded",
            "runRows": [{"id": run_id, "status": "succeeded"}],
            "reloadedRunRows": [{"id": run_id, "status": "succeeded"}],
            "reloadedRunListFirstRows": [{"id": run_id, "status": "succeeded"}],
            "logHasMarker": True, "reloadedLogHasMarker": True,
            "eventStreamAuthorized": 0, "authenticatedRunReads": 8,
        },
        "issues": [],
    }
    evidence.update(overrides)
    return evidence


class ConsoleFragmentTests(unittest.TestCase):
    def collect(self, evidence, playwright_exit=0):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        art = EARLY["write_robustness_artifacts"](Path(tmp.name) / "art")
        if evidence is not None:
            (art / "d3-evidence.json").write_text(json.dumps(evidence))
        out = Path(tmp.name) / "console.json"
        result = run("console", "--artifacts", str(art), "--candidate-sha", SHA,
                     "--playwright-exit", str(playwright_exit), "--out", str(out))
        return result, out

    def gate(self, out):
        fragment = json.loads(out.read_text())
        return check({"candidate_sha": SHA, "candidate_digest": fragment["candidate_digest"], "gate_enabled": True,
                      "disabled_gates": [], "scenarios": fragment["scenarios"]}, "nightly-console")

    def test_converged_journey_passes(self):
        result, out = self.collect(d3_evidence())
        self.assertEqual(result.returncode, 0, result.stderr)
        checked = self.gate(out)
        self.assertEqual(checked.returncode, 0, checked.stdout + checked.stderr)

    def test_issues_duplicates_or_failed_playwright_never_pass(self):
        duplicate = d3_evidence()
        duplicate["console"]["runRows"] = duplicate["console"]["runRows"] * 2
        for evidence, exit_code in ((d3_evidence(issues=["stale row"]), 0), (duplicate, 0),
                                    (d3_evidence(), 1), (None, 99)):
            with self.subTest(exit_code=exit_code):
                result, out = self.collect(evidence, exit_code)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.gate(out).returncode, 1)

    def test_foreign_server_image_is_refused(self):
        result, _ = self.collect(d3_evidence(serverImage="caesiumcloud/caesium:" + "e" * 40))
        self.assertEqual(result.returncode, 1)


def write_performance(root, *, overall="no_significant_difference", fixed="no_significant_difference",
                      blocking=False, sha=SHA):
    art = Path(root)
    attempt = art / "attempt-1"
    (attempt / "observations").mkdir(parents=True)
    (art / "gate.json").write_text(json.dumps({"final": {"attempt": 1, "exit_code": 0, "overall": overall}}))
    (attempt / "observations" / "host.json").write_text(json.dumps({"host_id": "github-hosted|ubuntu-24.04|x86_64"}))
    (attempt / "comparison.json").write_text(json.dumps({
        "candidate": {"provenance": {"git_sha": sha, "image_id": IMAGE_ID}}}))
    (attempt / "report.json").write_text(json.dumps({
        "overall": overall,
        "min_samples": 10,
        "provenance": {"missing": [], "mismatched": [], "instrumented": [], "invalid": []},
        "decision": {"target_base": {"verdict": "no_significant_difference"},
                     "fixed_baseline": {"verdict": fixed},
                     "slos": {"verdict": "no_significant_difference"}},
        "strict_gate": {"blocking": blocking},
    }))
    return art


class PerformanceFragmentTests(unittest.TestCase):
    def collect(self, **kwargs):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        art = write_performance(Path(tmp.name) / "perf", **kwargs)
        out = Path(tmp.name) / "perf.json"
        return run("performance", "--artifacts", str(art), "--candidate-sha", SHA, "--out", str(out)), out

    def test_resolved_non_blocking_gate_passes(self):
        result, out = self.collect()
        self.assertEqual(result.returncode, 0, result.stderr)
        row = json.loads(out.read_text())["scenarios"][0]
        self.assertEqual(row["status"], "pass")
        self.assertIn("fixed_baseline_non_inferior", row["observations"])

    def test_refused_fixed_baseline_is_inconclusive_and_regression_fails(self):
        _, out = self.collect(overall="inconclusive_unresolved", fixed="inconclusive", blocking=True)
        row = json.loads(out.read_text())["scenarios"][0]
        self.assertEqual(row["status"], "inconclusive")
        self.assertNotIn("fixed_baseline_non_inferior", row["observations"])
        _, out = self.collect(overall="slower", fixed="slower", blocking=True)
        self.assertEqual(json.loads(out.read_text())["scenarios"][0]["status"], "fail")

    def test_foreign_candidate_is_refused(self):
        result, _ = self.collect(sha="e" * 40)
        self.assertEqual(result.returncode, 1)


class LaneReportTests(unittest.TestCase):
    def test_report_requires_one_identity_and_real_fragments(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            a = root / "a.json"
            b = root / "b.json"
            a.write_text(json.dumps({"candidate_sha": SHA, "candidate_digest": IMAGE_ID,
                                     "scenarios": [{"id": "x"}]}))
            b.write_text(json.dumps({"candidate_sha": SHA, "candidate_digest": DIGEST,
                                     "scenarios": [{"id": "y"}]}))
            out = root / "report.json"
            self.assertEqual(run("report", "--gate", "g", "--out", str(out), str(a)).returncode, 0)
            report = json.loads(out.read_text())
            self.assertEqual((report["gate"], report["candidate_digest"]), ("g", IMAGE_ID))
            mixed = run("report", "--gate", "g", "--out", str(out), str(a), str(b))
            self.assertEqual(mixed.returncode, 1)
            self.assertIn("want exactly one", mixed.stderr)
            missing = run("report", "--gate", "g", "--out", str(out), str(root / "absent.json"))
            self.assertEqual(missing.returncode, 1)
            self.assertIn("the lane did not produce it", missing.stderr)
            duplicate = run("report", "--gate", "g", "--out", str(out), str(a), str(a))
            self.assertEqual(duplicate.returncode, 1)


class ConnectorsEvidenceTests(unittest.TestCase):
    """Synthetic artifact mutations test the consumer, never qualify Temporal."""

    @classmethod
    def setUpClass(cls):
        import hashlib
        import io
        import tarfile
        cls.candidate = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
        raw = subprocess.check_output(["git", "archive", "--format=tar", cls.candidate], cwd=ROOT)
        with tarfile.open(fileobj=io.BytesIO(raw)) as archive:
            cls.sources = [{"path": entry.name, "sha256": hashlib.sha256(
                entry.linkname.encode() if entry.issym() else archive.extractfile(entry).read()).hexdigest()}
                for entry in archive if entry.isfile() or entry.issym()]
        module = runpy.run_path(str(SCRIPT))
        cls.go_names = module["CONNECTORS_GO"]
        cls.browser_names = module["CONNECTORS_BROWSER"]
        cls.pins = module["CONNECTORS_PINS"]
        cls.flags = dict(module["CONNECTORS_FLAGS"], CAESIUM_CONNECTORS_CONFIG_FILE="/fixture/connections.yaml")

    def write(self, path, value):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(value) if not isinstance(value, str) else value)

    def setUp(self):
        import base64
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.out = self.root / "evidence.json"
        self.fixture, self.browser, self.cluster = (self.root / name for name in ("fixture", "browser", "cluster"))
        for path in (self.fixture, self.cluster):
            self.write(path / "candidate-sha.txt", self.candidate)
            self.write(path / "source-checksums.json", self.sources)
        startup = dict(self.pins, kind="fixture_started", candidate=self.candidate)
        self.events = [startup,
                       {"kind": "update_rpc", "reply_dropped": True, "rejected": False, "rpc_code": "OK", "update_id": "accepted", "response_run_id": "run"},
                       {"kind": "update_rpc", "reply_dropped": True, "rejected": True, "rpc_code": "OK", "update_id": "rejected"},
                       {"kind": "proxy_interruption"},
                       {"kind": "approval_effect", "run_id": "run", "actor": {"operation_id": "op"}}]
        self.receipt = {"id": "op", "external_update_id": "accepted", "state": "completed",
                        "actor": {"operation_id": "op", "stable_id": "principal"}}
        self.write(self.fixture / "events.jsonl", "\n".join(json.dumps(event) for event in self.events))
        self.write(self.fixture / "health.json", dict(self.pins, ready=True))
        self.write(self.fixture / "server-env.json", self.flags)
        self.write(self.fixture / "server-image.json", [{"Id": IMAGE_ID}])
        self.write(self.fixture / "images.json", [{"Id": DIGEST}, {"Id": BUILDER_ID}])
        self.write(self.fixture / "image-provenance.json", {"candidate_sha": self.candidate,
            "server": {"origin": "loaded", "expected_image_id": IMAGE_ID, "observed_image_id": IMAGE_ID},
            "runner": {"origin": "loaded", "expected_image_id": BUILDER_ID, "observed_image_id": BUILDER_ID}})
        self.write(self.fixture / "server.json", [{"Id": "container", "Image": IMAGE_ID,
            "State": {"Running": True, "OOMKilled": False}, "Config": {"EnvNames": list(self.flags)}}])
        self.write(self.fixture / "test.log", "\n".join(f"=== RUN   {name}\n--- PASS: {name} (1.00s)" for name in self.go_names))
        self.write(self.fixture / "exit-status", "0")
        for filename in ("fixture-evidence.json", "messages-provider-evidence.json", "correlation-evidence.json",
                         "operations-component-evidence.json", "read-http-evidence.json", "read-correlation-http-evidence.json",
                         "actions-http-evidence.json", "actions-http-responses.json"):
            self.write(self.fixture / filename, {"candidate_sha": self.candidate, "events": self.events})
        for filename in ("operations-component-evidence.json", "actions-http-evidence.json"):
            self.mutate(self.fixture / filename, lambda doc: doc.update(receipts=[self.receipt], restart=[
                {"status": "ok", "started_before": "2026-10-20T10:00:00Z", "started_after": "2026-10-20T10:01:00Z"}]))
        self.mutate(self.fixture / "actions-http-responses.json", lambda doc: doc.update(responses=[
            {"method": "POST", "path": "/v1/connectors/fixture/executions/id/actions/approve_publication",
             "status": 201, "body": {"operation": self.receipt}}]))
        self.mutate(self.fixture / "correlation-evidence.json", lambda doc: doc.update(
            activity_retry_activated=True, queued={"outcome": "queued"}, promoted_run_id="local-run"))
        self.mutate(self.fixture / "read-http-evidence.json", lambda doc: doc.update(
            unchanged_snapshot_writes=0, unchanged_identity_updates=0, closed_workflow_query_rpc_delta=0,
            external_workflows=[{"run_id": "external-run"}]))
        # Actions retain only workflow-specific events. The monitoring case
        # supplies its own named capture with events observed after arming the
        # browser outage; earlier Go-phase proxy events cannot satisfy it.
        capture = {"candidate_sha": self.candidate, "receipts": [self.receipt], "events": [
            event for event in self.events if event["kind"] not in ("proxy_interruption", "fault_armed")]}
        specs = [{"title": title, "ok": True, "tests": [{"projectName": "connectors", "expectedStatus": "passed",
                  "status": "expected", "results": [{"status": "passed", "retry": 0, "errors": [], "attachments": [{
                      "contentType": "application/json", "body": base64.b64encode(json.dumps(capture).encode()).decode()}]}]}]}
                 for title in self.browser_names]
        outage = {"kind": "connector_browser_outage", "candidate_sha": self.candidate,
                  "workflow_id": "browser-workflow", "run_id": "browser-run",
                  "observed_run_id": "browser-run", "availability": "Stale observation",
                  "observed_at": "2026-10-20T10:00:00Z",
                  "events": [{"kind": "fault_armed", "outage": True}, {"kind": "proxy_interruption"}]}
        monitoring = next(spec for spec in specs if spec["title"].startswith("viewer filters and inspects"))
        monitoring["tests"][0]["results"][0]["attachments"].append({"name": "Connector provider outage",
            "contentType": "application/json", "body": base64.b64encode(json.dumps(outage).encode()).decode()})
        self.write(self.browser / "playwright-results.json", {"suites": [{"specs": specs}], "errors": [],
            "stats": {"expected": 10, "unexpected": 0, "flaky": 0, "skipped": 0}})
        self.write(self.browser / "candidate-sha.txt", self.candidate)
        self.write(self.browser / "server-image-id.txt", IMAGE_ID)
        self.write(self.browser / "exit-status", "0")
        self.write(self.cluster / "events.jsonl", json.dumps(startup))
        self.write(self.cluster / "fixture-health.json", dict(self.pins, ready=True))
        self.write(self.cluster / "server-image-id.txt", IMAGE_ID)
        self.write(self.cluster / "reuse-image-identities.json", {"candidate_sha": self.candidate,
            **{role: {"expected_image_id": value, "observed_image_id": value}
               for role, value in (("server", IMAGE_ID), ("runner", BUILDER_ID), ("fixture", DIGEST))}})
        self.write(self.cluster / "candidate-identities.txt", IMAGE_ID + "," + DIGEST)
        self.write(self.cluster / "candidate-seed-identities.txt", IMAGE_ID + "\n" + DIGEST)
        self.write(self.cluster / "imported-server-digests.txt", DIGEST)
        self.write(self.cluster / "loaded-candidate-cri.json", {"status": {"id": IMAGE_ID,
            "repoTags": ["docker.io/caesiumcloud/caesium:test"], "repoDigests": ["caesiumcloud/caesium@" + DIGEST]}})
        self.write(self.cluster / "kind-node-ids.json", {"owned-control-plane": "c" * 64})
        self.write(self.cluster / "statefulset.json", {"spec": {"replicas": 3, "volumeClaimTemplates": [{"metadata": {"name": "data"}}],
            "template": {"spec": {"containers": [{"image": "caesiumcloud/caesium:test", "env": [
                {"name": key, "value": value} for key, value in dict(self.flags, CAESIUM_EXECUTION_MODE="distributed").items()]}]}}}})
        members = [{"name": "caesium-" + str(i), "uid": "uid" + str(i), "ip": "10.0.0." + str(i),
                    "node": "owned-control-plane", "pvc": "pvc" + str(i), "volume": "vol" + str(i),
                    "image_id": "containerd://" + IMAGE_ID, "container_id": "container" + str(i)} for i in range(3)]
        def refusal(count):
            return {"ready_status": 200, "connector_statuses": [503] * 5, "provider_rpc_before": count,
                    "provider_rpc_after": count, "connectors_enabled": True, "connectors_ready": False}
        steps = [{"phase": "matching", "fingerprint": "old", "member_fingerprints": ["old", "old"], "members": ["caesium-0", "caesium-1"],
                  "provider_rpc_before": 0, "provider_rpc_count": 2},
                 {"phase": "mismatch_refused", "member": "caesium-1", "uid": "new-uid1", "startup_provider_rpc_before": 2,
                  "provider_rpc_count": 2, "refusal": refusal(2)},
                 {"phase": "guarded_transition_and_stale_node", "fingerprint": "new", "active_member": "caesium-1", "stale_member": "caesium-0",
                  "provider_rpc_count": 3, "refusal": refusal(3), "observed_run_id": "run", "observed_coordinates": {"workflow_id": "external", "run_id": "run"}},
                 {"phase": "immutable_repoint_refused", "member": "caesium-1", "uid": "new-uid2", "fingerprint": "new",
                  "startup_provider_rpc_before": 4, "provider_rpc_count": 4, "refusal": refusal(4)}]
        self.write(self.cluster / "config-cluster-evidence.json", {"candidate_sha": self.candidate,
            "candidate_image_digest": IMAGE_ID + "," + DIGEST, "initial_members": members, "steps": steps,
            "workflow": {"workflow_id": "external", "run_id": "run"}})
        self.write(self.cluster / "connector-config-test.log", "=== RUN   TestConnectorsConfigCluster\n--- PASS: TestConnectorsConfigCluster (1.00s)\n")
        self.write(self.cluster / "exit-status", "0")

    def mutate(self, path, action):
        doc = json.loads(path.read_text())
        action(doc)
        self.write(path, doc)

    def collect(self):
        return run("connectors", "--artifacts", str(self.root), "--candidate-sha", self.candidate, "--out", str(self.out))

    def assert_fails_closed(self):
        result = self.collect()
        if result.returncode == 0:
            checked = check(json.loads(self.out.read_text()), "connectors")
            self.assertEqual(checked.returncode, 1, checked.stdout + checked.stderr)
        else:
            self.assertEqual(result.returncode, 1, result.stderr)

    def test_complete_candidate_bound_artifacts_pass_shared_strict_checker(self):
        result = self.collect()
        self.assertEqual(result.returncode, 0, result.stderr)
        report = json.loads(self.out.read_text())
        checked = check(report, "connectors")
        self.assertEqual(checked.returncode, 0, checked.stdout + checked.stderr)
        self.assertEqual(report["candidate_digest"], IMAGE_ID)
        self.assertNotIn("candidate_digest", report["scenarios"][2])
        self.assertEqual(report["scenarios"][2]["observed_nodes"], ["owned-control-plane"])

    def test_missing_hollow_foreign_and_dirty_source_artifacts_fail_closed(self):
        for filename, value in (("fixture/actions-http-evidence.json", None),
                                ("fixture/health.json", {}),
                                ("fixture/read-http-evidence.json", {"candidate_sha": SHA}),
                                ("fixture/source-checksums.json", []),
                                ("cluster/source-checksums.json", self.sources[:-1])):
            with self.subTest(filename=filename):
                path = self.root / filename
                previous = path.read_text()
                if value is None: path.unlink()
                else: self.write(path, value)
                self.assert_fails_closed()
                path.write_text(previous)

    def test_wrong_sha_server_runner_and_browser_image_bindings_fail_closed(self):
        for path, mutate in (
            (self.fixture / "candidate-sha.txt", None),
            (self.fixture / "image-provenance.json", lambda doc: doc["runner"].update(expected_image_id=DIGEST)),
            (self.fixture / "server.json", lambda doc: doc[0].update(Image=DIGEST)),
            (self.browser / "server-image-id.txt", None),
            (self.cluster / "reuse-image-identities.json", lambda doc: doc["runner"].update(
                expected_image_id=DIGEST, observed_image_id=DIGEST)),
            (self.cluster / "reuse-image-identities.json", lambda doc: doc["fixture"].update(
                expected_image_id=BUILDER_ID, observed_image_id=BUILDER_ID)),
            (self.cluster / "loaded-candidate-cri.json", lambda doc: doc["status"].update(id=COVERAGE_ID))):
            with self.subTest(path=path.name):
                previous = path.read_text()
                if mutate is None: path.write_text(SHA)
                else: self.mutate(path, mutate)
                self.assert_fails_closed()
                path.write_text(previous)

    def test_coherently_foreign_cluster_image_fails_fixture_binding(self):
        # Change every cluster identity consistently, leaving the real fixture
        # candidate intact. Internal CRI/seed/member agreement is insufficient.
        foreign = "sha256:" + "f1" * 32
        for name in ("server-image-id.txt", "candidate-identities.txt", "candidate-seed-identities.txt",
                     "imported-server-digests.txt", "loaded-candidate-cri.json", "config-cluster-evidence.json"):
            path = self.cluster / name
            path.write_text(path.read_text().replace(IMAGE_ID, foreign))
        self.assert_fails_closed()

    def test_browser_outage_requires_its_own_fresh_target_bound_capture(self):
        import base64
        path = self.browser / "playwright-results.json"
        original = path.read_text()
        def outage_attachment(doc):
            spec = next(spec for spec in doc["suites"][0]["specs"]
                        if spec["title"].startswith("viewer filters and inspects"))
            return spec["tests"][0]["results"][0]["attachments"][-1]
        for change in (lambda capture: capture.update(kind="old_go_fault"),
                       lambda capture: capture.update(candidate_sha=SHA),
                       lambda capture: capture.update(observed_run_id="foreign-run"),
                       lambda capture: capture.update(availability="live"),
                       lambda capture: capture.update(events=[{"kind": "proxy_interruption"}]),
                       lambda capture: capture.update(events=[{"kind": "fault_armed", "outage": True}])):
            doc = json.loads(original)
            attachment = outage_attachment(doc)
            capture = json.loads(base64.b64decode(attachment["body"]))
            change(capture)
            attachment["body"] = base64.b64encode(json.dumps(capture).encode()).decode()
            self.write(path, doc)
            self.assert_fails_closed()
        # A global event in an action capture does not replace monitoring proof.
        doc = json.loads(original)
        outage_attachment(doc).clear()
        first = doc["suites"][0]["specs"][0]["tests"][0]["results"][0]["attachments"][0]
        capture = json.loads(base64.b64decode(first["body"]))
        capture["events"].extend([{"kind": "fault_armed", "outage": True}, {"kind": "proxy_interruption"}])
        first["body"] = base64.b64encode(json.dumps(capture).encode()).decode()
        self.write(path, doc)
        self.assert_fails_closed()
        path.write_text(original)

    def test_missing_duplicate_skipped_and_failed_named_go_scenarios_fail(self):
        path = self.fixture / "test.log"
        original = path.read_text()
        for log in ("", original.replace("--- PASS:", "--- SKIP:", 1),
                    original + "\n--- FAIL: " + self.go_names[0] + " (0s)\n",
                    original + "\n=== RUN   " + self.go_names[0] + "\n--- PASS: " + self.go_names[0] + " (0s)\n"):
            path.write_text(log)
            self.assert_fails_closed()
        path.write_text(original)

    def test_gate_off_queue_off_and_fixture_failure_fail(self):
        for key in ("CAESIUM_CONNECTORS_ENABLED", "CAESIUM_RUN_QUEUE_ENABLED", "CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED"):
            self.mutate(self.fixture / "server-env.json", lambda doc: doc.update({key: "false"}))
            self.assert_fails_closed()
            self.write(self.fixture / "server-env.json", self.flags)
        self.write(self.fixture / "exit-status", "1")
        self.assert_fails_closed()

    def test_failed_first_browser_attempt_wrong_project_zero_and_duplicate_fail(self):
        path = self.browser / "playwright-results.json"
        original = path.read_text()
        mutations = [lambda doc: doc["suites"][0]["specs"][0]["tests"][0]["results"].insert(0, {"status": "failed", "retry": 0}),
                     lambda doc: doc["suites"][0]["specs"][0]["tests"][0]["results"][0].update(retry=1),
                     lambda doc: doc["suites"][0]["specs"][0]["tests"][0].update(projectName="default"),
                     lambda doc: doc.update(suites=[]),
                     lambda doc: doc["suites"][0]["specs"].append(copy.deepcopy(doc["suites"][0]["specs"][0]))]
        for action in mutations:
            self.mutate(path, action)
            self.assert_fails_closed()
            path.write_text(original)

    def test_absent_fault_journal_or_duplicate_effect_never_passes(self):
        original = (self.fixture / "events.jsonl").read_text()
        self.write(self.fixture / "events.jsonl", json.dumps(self.events[0]))
        self.assert_fails_closed()
        self.write(self.fixture / "events.jsonl", original)
        self.mutate(self.fixture / "actions-http-evidence.json", lambda doc: doc["events"].append(copy.deepcopy(self.events[-1])))
        self.assert_fails_closed()

    def test_cluster_labels_without_observed_faults_and_wrong_topology_fail(self):
        path = self.cluster / "config-cluster-evidence.json"
        original = path.read_text()
        mutations = [lambda doc: doc["steps"][1].pop("refusal"),
                     lambda doc: doc["steps"][2]["refusal"].update(provider_rpc_after=99),
                     lambda doc: doc["steps"][2].update(observed_run_id="retargeted"),
                     lambda doc: doc["steps"][3].update(fingerprint="invented-epoch"),
                     lambda doc: doc["initial_members"][1].update(pvc="pvc0"),
                     lambda doc: doc["initial_members"][1].update(node="unobserved-node"),
                     lambda doc: doc["initial_members"][1].update(image_id=COVERAGE_ID)]
        for action in mutations:
            self.mutate(path, action)
            self.assert_fails_closed()
            path.write_text(original)


if __name__ == "__main__":
    unittest.main()
