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


def check(report, gate):
    with tempfile.TemporaryDirectory() as tmp:
        path = Path(tmp) / "report.json"
        path.write_text(json.dumps(report))
        return subprocess.run(
            [sys.executable, str(CHECKER), "--manifest", str(MANIFEST), "--report", str(path),
             "--require", gate, "--strict"],
            capture_output=True, text=True,
        )


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
        result, out = self.collect()
        self.assertEqual(result.returncode, 0, result.stderr)
        rows = self.rows(out)
        self.assertEqual(len(rows), 14)
        self.assertTrue(all(row["status"] == "pass" for row in rows.values()))
        checked = check(self.report(out, "nightly-core"), "nightly-core")
        self.assertEqual(checked.returncode, 0, checked.stdout + checked.stderr)

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
        checked = check(self.report(out, "nightly-core"), "nightly-core")
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


if __name__ == "__main__":
    unittest.main()
