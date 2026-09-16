"""Regression checks for the early-evidence artifact consumer.

These drive `scripts/collect-evidence.py` over artifact directories shaped
exactly like the ones `scripts/robustness.sh` and `just
integration-test-sql-budget` leave behind, then feed the result to the real
`scripts/check-test-evidence.py` against the committed manifest. A lane that
stops producing evidence must fail closed, never silently pass.
"""

import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts/collect-evidence.py"
CHECKER = ROOT / "scripts/check-test-evidence.py"
MANIFEST = ROOT / "test/contracts/scenarios.json"

SHA = "b" * 40
DIGEST = "sha256:" + "cd" * 32
TEST_IMAGE_ID = "sha256:" + "ef" * 32
TOKEN = "robustness-internal-token-value-not-a-real-secret"
NODE = "rb-fixture-worker"

CLUSTER_ENV = {
    "CAESIUM_DATABASE_CONSOLE_ENABLED": "true",
    "CAESIUM_DATABASE_PATH": "/var/lib/caesium/dqlite",
    "CAESIUM_DATABASE_SHARDS": "1",
    "CAESIUM_DATABASE_STANDBYS": "0",
    "CAESIUM_DATABASE_TYPE": "internal",
    "CAESIUM_DATABASE_VOTERS": "3",
    "CAESIUM_EXECUTION_MODE": "distributed",
    "CAESIUM_INTERNAL_WAKEUP_TOKEN": TOKEN,
    "CAESIUM_KUBERNETES_NAMESPACE": "rb-fixture",
    "CAESIUM_LOG_LEVEL": "debug",
    "CAESIUM_MANUAL_TRIGGER_API_KEY": "caesium-robustness-manual-key",
    "CAESIUM_RUN_LEASE_TTL": "30s",
    "CAESIUM_RUN_OWNER_DISPATCH_INTERVAL": "1s",
    "CAESIUM_RUN_OWNER_ENABLED": "true",
    "CAESIUM_RUN_OWNER_IN_MEMORY": "true",
    "CAESIUM_WORKER_LEASE_TTL": "30s",
    "CAESIUM_WORKER_POOL_SIZE": "2",
    "CAESIUM_WORKER_RECLAIM_INTERVAL": "1s",
}

SERVER_ENV = [
    "DOCKER_HOST=unix:///var/run/docker.sock",
    "CAESIUM_MANUAL_TRIGGER_API_KEY=integration-test-key",
    "CAESIUM_DATABASE_SHARDS=4",
    "CAESIUM_CACHE_ENABLED=true",
    "CAESIUM_CACHE_PIN_DIGESTS=true",
    "CAESIUM_CONTRACT_ENFORCEMENT=fail",
]

SQL_SELECTOR = "TestIntegrationTestSuite/TestStatementBudgetFixedWorkload"


def run(*args):
    return subprocess.run([sys.executable, str(SCRIPT), *args], capture_output=True, text=True)


def kill_evidence(container, node=NODE, outcome="stopped"):
    """Shapes `scripts/robustness.sh` actually writes into the record."""
    header = "TASK                PID     STATUS\n"
    rows = "0000000000000000    342     RUNNING\n"
    if outcome == "stopped":
        listing = header + rows + f"{container}    1215    STOPPED\n"
    elif outcome == "running":
        listing = header + rows + f"{container}    1215    RUNNING\n"
    elif outcome == "absent":
        # containerd drops the task entirely; B1 treats that as death because
        # the host controller proved the container was listed before the kill.
        listing = header + rows
    elif outcome == "error":
        listing = "ctr: failed to dial: connection refused\n"
    else:
        raise ValueError(outcome)
    return f"kubelet stopped on {node}\nctr kill {container}\n{listing}"


def subtest_record(subtest, run_id, container):
    return {
        "subtest": subtest,
        "run_id": run_id,
        "owner_pod": "caesium-0",
        "owner_node": NODE,
        "owner_address": "10.244.1.3:9001",
        "leader_before": "caesium-0",
        "lease_before": {"RunID": run_id, "OwnerNode": "10.244.1.3:9001", "Generation": 1},
        "lease_after": {"RunID": run_id, "OwnerNode": "10.244.2.3:9001", "Generation": 2},
        "recovery_seconds": 34.1,
        "block_starts": 2,
        "block_completions": 2,
        "successor_starts": 1,
        "successor_complete": 1,
        "public_status": "succeeded",
        "kill_evidence": kill_evidence(container),
    }


def subtest_log(subtest, run_id, container):
    return (
        f"=== RUN   TestOwnerCrash/{subtest}\n"
        f"    owner_crash_test.go:167: cordon ack: cordoned {NODE}\n"
        f"    owner_crash_test.go:185: admitted run {run_id} via caesium-0 body={{\"id\":\"{run_id}\"}}\n"
        f"    owner_crash_test.go:287: kill evidence accepted for {container} on {NODE}\n"
    )


def events_for(run_id):
    return [
        {"kind": "start", "at": "2026-09-12T16:06:18Z", "run_id": run_id,
         "step": "block", "nonce": "n1"},
        {"kind": "release", "at": "2026-09-12T16:06:20Z", "run_id": run_id},
        {"kind": "complete", "at": "2026-09-12T16:06:21Z", "run_id": run_id,
         "step": "block", "nonce": "n1"},
        {"kind": "start", "at": "2026-09-12T16:06:22Z", "run_id": run_id,
         "step": "successor", "nonce": "n2"},
        {"kind": "complete", "at": "2026-09-12T16:06:23Z", "run_id": run_id,
         "step": "successor", "nonce": "n2"},
    ]


def write_robustness_artifacts(root, *, members=3, workers=3, claims=3,
                               leader_outcome="PASS", nonleader_outcome="PASS",
                               drop_records=(), kill_outcome="stopped", env=None):
    """Mirror a real `scripts/robustness.sh` artifact directory."""
    art = Path(root)
    (art / "records").mkdir(parents=True, exist_ok=True)
    (art / "versions.json").write_text(json.dumps({
        "candidate_sha": SHA,
        "robustness_id": "rb-fixture",
        "kind_image": "kindest/node:v1.36.1",
        "server_image": f"caesiumcloud/caesium:{SHA}",
        "runner_image": f"caesiumcloud/caesium-robustness:{SHA}",
        "task_image": "alpine:3.23",
    }))
    (art / "candidate-digest.txt").write_text(DIGEST + "\n")
    (art / "internal-token.txt").write_text(TOKEN + "\n")
    (art / "kubeconfig").write_text("apiVersion: v1\nusers: [{user: {token: " + TOKEN + "}}]\n")
    nodes = ["  - role: control-plane"] + ["  - role: worker"] * workers
    (art / "kind.yaml").write_text("kind: Cluster\nnodes:\n" + "\n".join(nodes) + "\n")
    per_pod = dict(env or CLUSTER_ENV)
    for index in range(members):
        lines = [f"{key}={value}" for key, value in sorted(per_pod.items())]
        lines.append(f"CAESIUM_NODE_ADDRESS=10.244.{index + 1}.3:9001")
        (art / f"caesium-{index}.env").write_text("\n".join(lines) + "\n")
    described = "".join(
        f"Name:             caesium-{index}\n    ClaimName:  data-caesium-{index}\n"
        for index in range(claims)
    )
    (art / "describe-ns-pods.txt").write_text(described)

    leader_run, nonleader_run = "run-leader-uuid", "run-nonleader-uuid"
    leader_cid, nonleader_cid = "a" * 64, "b" * 64
    log = "=== RUN   TestOwnerCrash\n    owner_crash_test.go:56: dqlite leader=10.244.1.3:9001 members=3\n"
    log += subtest_log("owner_is_leader", leader_run, leader_cid)
    log += subtest_log("owner_is_not_leader", nonleader_run, nonleader_cid)
    log += (
        "--- PASS: TestOwnerCrash (122.37s)\n"
        f"    --- {leader_outcome}: TestOwnerCrash/owner_is_leader (62.23s)\n"
        f"    --- {nonleader_outcome}: TestOwnerCrash/owner_is_not_leader (57.59s)\n"
    )
    (art / "robustness.test.log").write_text(log)

    events = events_for(leader_run) + events_for(nonleader_run)
    (art / "records" / "events.json").write_text(json.dumps(events))
    for subtest, run_id, cid in (("owner_is_leader", leader_run, leader_cid),
                                 ("owner_is_not_leader", nonleader_run, nonleader_cid)):
        if subtest in drop_records:
            continue
        record = subtest_record(subtest, run_id, cid)
        record["kill_evidence"] = kill_evidence(cid, outcome=kill_outcome)
        (art / "records" / f"{subtest}.json").write_text(json.dumps(record))
    return art


def write_sql_artifacts(root, *, outcome="PASS", mounts=None, metrics=True, env=None):
    out = Path(root)
    out.mkdir(parents=True, exist_ok=True)
    (out / "server-inspect.json").write_text(json.dumps([{
        "Id": "container-id",
        "Config": {"Env": list(env if env is not None else SERVER_ENV)},
        "Mounts": mounts if mounts is not None else [
            {"Type": "bind", "Destination": "/var/run/docker.sock"},
        ],
    }]))
    (out / "statement-budget.log").write_text(
        "=== RUN   TestIntegrationTestSuite\n"
        f"    --- {outcome}: {SQL_SELECTOR} (12.00s)\n"
        "PASS\n"
    )
    lines = []
    if metrics:
        for category in ("task_run_insert", "event_insert"):
            lines.append(f'caesium_db_statements_total{{category="{category}"}} 42')
            lines.append(f'caesium_db_writes_total{{category="{category}"}} 17')
    (out / "metrics.prom").write_text("\n".join(lines) + "\n")
    return out


class RobustnessFragmentTests(unittest.TestCase):
    def collect(self, **kwargs):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        art = write_robustness_artifacts(Path(tmp.name) / "artifacts", **kwargs)
        out = Path(tmp.name) / "fragment.json"
        result = run("robustness", "--artifacts", str(art), "--out", str(out))
        return result, out

    def test_real_artifacts_yield_every_manifest_observation(self):
        result, out = self.collect()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        fragment = json.loads(out.read_text())
        self.assertEqual(fragment["candidate_sha"], SHA)
        self.assertEqual(fragment["candidate_digest"], DIGEST)
        manifest = {item["id"]: item for item in json.loads(MANIFEST.read_text())["scenarios"]}
        self.assertEqual(len(fragment["scenarios"]), 2)
        for scenario in fragment["scenarios"]:
            expected = manifest[scenario["id"]]
            self.assertEqual(scenario["status"], "pass")
            self.assertEqual(scenario["topology"]["replicas"], 3)
            self.assertEqual(scenario["topology"]["worker_nodes"], 3)
            self.assertEqual(scenario["topology"]["control_plane_nodes"], 1)
            self.assertTrue(scenario["topology"]["persistence"])
            self.assertEqual(scenario["topology"]["engine"], "kubernetes")
            self.assertEqual(
                [item["id"] for item in expected["expected_observations"]],
                scenario["observations"],
            )
            self.assertEqual(
                expected["fault_activation"]["required_observations"],
                scenario["fault_activation"]["observations"],
            )
            self.assertTrue(scenario["recorder"]["present"])
            self.assertGreaterEqual(scenario["recorder"]["sample_count"], 1)

    def test_secret_environment_values_never_reach_the_fragment(self):
        _, out = self.collect()
        blob = out.read_text()
        self.assertNotIn(TOKEN, blob)
        self.assertNotIn("caesium-robustness-manual-key", blob)
        flags = json.loads(blob)["scenarios"][0]["feature_flags"]
        self.assertNotIn("CAESIUM_INTERNAL_WAKEUP_TOKEN", flags)
        self.assertEqual(flags["CAESIUM_EXECUTION_MODE"], "distributed")
        # Per-pod values are not cluster facts and must be dropped.
        self.assertNotIn("CAESIUM_NODE_ADDRESS", flags)

    def test_failed_subtest_is_reported_as_failure(self):
        _, out = self.collect(leader_outcome="FAIL")
        scenarios = {item["id"]: item for item in json.loads(out.read_text())["scenarios"]}
        self.assertEqual(scenarios["b1-owner-crash-leader"]["status"], "fail")
        self.assertEqual(scenarios["b1-owner-crash-nonleader"]["status"], "pass")

    def test_missing_record_is_inconclusive_not_pass(self):
        _, out = self.collect(drop_records=("owner_is_leader",))
        scenarios = {item["id"]: item for item in json.loads(out.read_text())["scenarios"]}
        leader = scenarios["b1-owner-crash-leader"]
        self.assertEqual(leader["status"], "inconclusive")
        self.assertEqual(leader["observations"], [])
        self.assertFalse(leader["recorder"]["present"])

    def test_kill_evidence_shapes_decide_the_fault_observation(self):
        # Mirrors test/robustness/kill_evidence.go: a task absent from a valid
        # listing is dead; a running task or an unreadable listing is not.
        for outcome, expected in (("stopped", True), ("absent", True),
                                  ("running", False), ("error", False)):
            with self.subTest(outcome=outcome):
                _, out = self.collect(kill_outcome=outcome)
                fault = json.loads(out.read_text())["scenarios"][0]["fault_activation"]
                self.assertEqual(
                    "process_exit_before_sink_barrier_release" in fault["observations"],
                    expected,
                    fault,
                )

    def test_member_and_claim_disagreement_fails_closed(self):
        result, _ = self.collect(claims=2)
        self.assertEqual(result.returncode, 1)
        self.assertIn("bound data claims", result.stderr)

    def test_empty_pod_environment_fails_closed(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        art = write_robustness_artifacts(Path(tmp.name) / "artifacts")
        (art / "caesium-1.env").write_text("")
        result = run("robustness", "--artifacts", str(art),
                     "--out", str(Path(tmp.name) / "fragment.json"))
        self.assertEqual(result.returncode, 1)
        self.assertIn("pod environment was not captured", result.stderr)

    def test_missing_artifact_directory_fails_closed(self):
        with tempfile.TemporaryDirectory() as tmp:
            result = run("robustness", "--artifacts", str(Path(tmp) / "absent"),
                         "--out", str(Path(tmp) / "fragment.json"))
            self.assertEqual(result.returncode, 1)
            self.assertIn("cannot read", result.stderr)

    def test_wrong_topology_is_rejected_by_the_checker(self):
        # Two replicas cannot satisfy a manifest row that demands three.
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        art = write_robustness_artifacts(Path(tmp.name) / "artifacts", members=2, claims=2)
        fragment = Path(tmp.name) / "fragment.json"
        self.assertEqual(run("robustness", "--artifacts", str(art),
                             "--out", str(fragment)).returncode, 0)
        report = Path(tmp.name) / "report.json"
        sql = write_sql_fragment(Path(tmp.name))
        self.assertEqual(run("report", "--out", str(report), str(fragment), str(sql)).returncode, 0)
        checked = check(report)
        self.assertEqual(checked.returncode, 1, checked.stdout + checked.stderr)
        self.assertIn("wrong-topology", checked.stdout + checked.stderr)


def write_sql_fragment(root, **kwargs):
    out = write_sql_artifacts(Path(root) / "sql", **kwargs)
    fragment = Path(root) / "sql-fragment.json"
    result = run(
        "sql-work-budget",
        "--inspect", str(out / "server-inspect.json"),
        "--metrics", str(out / "metrics.prom"),
        "--log", str(out / "statement-budget.log"),
        "--candidate-sha", SHA,
        "--server-image-id", TEST_IMAGE_ID,
        "--out", str(fragment),
    )
    if result.returncode != 0:
        raise AssertionError(result.stdout + result.stderr)
    return fragment


def check(report, *extra):
    return subprocess.run(
        [sys.executable, str(CHECKER), "--manifest", str(MANIFEST),
         "--report", str(report), "--require", "early", "--strict", *extra],
        capture_output=True, text=True,
    )


class SQLWorkBudgetFragmentTests(unittest.TestCase):
    def test_observed_server_shapes_the_scenario(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        fragment = json.loads(write_sql_fragment(Path(tmp.name)).read_text())
        self.assertNotIn("candidate_digest", fragment)
        scenario = fragment["scenarios"][0]
        self.assertEqual(scenario["status"], "pass")
        self.assertEqual(scenario["observed_server_image_id"], TEST_IMAGE_ID)
        self.assertEqual(scenario["topology"], {
            "kind": "integration-existing",
            "replicas": 1,
            "persistence": False,
            "engine": "docker",
            "database_shards": 4,
        })
        self.assertEqual(scenario["mode"],
                         {"execution": "local", "owner": "disabled", "surface": "http+cli"})
        self.assertEqual(sorted(scenario["observations"]), [
            "statement_counter_delta_within_budget",
            "workload_completed_successfully",
            "write_counter_delta_within_budget",
        ])
        self.assertEqual(scenario["recorder"], {"present": True, "sample_count": 4})
        self.assertNotIn("integration-test-key", json.dumps(fragment))

    def test_failed_budget_reports_failure_without_observations(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        fragment = json.loads(write_sql_fragment(Path(tmp.name), outcome="FAIL").read_text())
        scenario = fragment["scenarios"][0]
        self.assertEqual(scenario["status"], "fail")
        self.assertEqual(scenario["observations"], [])

    def test_missing_counters_leave_the_recorder_absent(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        fragment = json.loads(write_sql_fragment(Path(tmp.name), metrics=False).read_text())
        scenario = fragment["scenarios"][0]
        self.assertFalse(scenario["recorder"]["present"])
        self.assertEqual(scenario["observations"], ["workload_completed_successfully"])

    def test_only_a_database_mount_counts_as_persistence(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        # dind's anonymous /var/lib/docker volume is not Caesium persistence.
        unrelated = json.loads(write_sql_fragment(Path(tmp.name), mounts=[
            {"Type": "bind", "Destination": "/var/run/docker.sock"},
            {"Type": "volume", "Destination": "/var/lib/docker"},
        ]).read_text())
        self.assertFalse(unrelated["scenarios"][0]["topology"]["persistence"])
        persistent = json.loads(write_sql_fragment(Path(tmp.name), mounts=[
            {"Type": "volume", "Destination": "/var/lib/caesium"},
        ]).read_text())
        self.assertTrue(persistent["scenarios"][0]["topology"]["persistence"])

    def test_base_image_environment_is_not_reported_as_a_feature_flag(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        fragment = json.loads(write_sql_fragment(
            Path(tmp.name),
            env=SERVER_ENV + ["PATH=/usr/bin", "DOCKER_VERSION=29.2.0"],
        ).read_text())
        flags = fragment["scenarios"][0]["feature_flags"]
        self.assertNotIn("PATH", flags)
        self.assertNotIn("DOCKER_VERSION", flags)
        self.assertEqual(flags["CAESIUM_DATABASE_SHARDS"], "4")

    def test_unidentifiable_engine_fails_closed(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        out = write_sql_artifacts(Path(tmp.name) / "sql", env=["CAESIUM_DATABASE_SHARDS=4"])
        result = run(
            "sql-work-budget",
            "--inspect", str(out / "server-inspect.json"),
            "--metrics", str(out / "metrics.prom"),
            "--log", str(out / "statement-budget.log"),
            "--candidate-sha", SHA,
            "--server-image-id", TEST_IMAGE_ID,
            "--out", str(Path(tmp.name) / "fragment.json"),
        )
        self.assertEqual(result.returncode, 1)
        self.assertIn("neither a Kubernetes namespace nor DOCKER_HOST", result.stderr)


class ReportTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        art = write_robustness_artifacts(self.root / "artifacts")
        self.robustness = self.root / "robustness.json"
        self.assertEqual(run("robustness", "--artifacts", str(art),
                             "--out", str(self.robustness)).returncode, 0)
        self.sql = write_sql_fragment(self.root)
        self.report = self.root / "report.json"

    def build(self, *fragments, extra=()):
        return run("report", "--out", str(self.report), *extra,
                   *(str(path) for path in (fragments or (self.sql, self.robustness))))

    def test_merged_report_satisfies_the_committed_early_gate(self):
        self.assertEqual(self.build().returncode, 0)
        report = json.loads(self.report.read_text())
        self.assertEqual(report["candidate_sha"], SHA)
        self.assertEqual(report["candidate_digest"], DIGEST)
        self.assertTrue(report["gate_enabled"])
        self.assertEqual(report["disabled_gates"], [])
        result = check(self.report)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_missing_fragment_file_fails_closed(self):
        result = self.build(self.root / "never-written.json", self.robustness)
        self.assertEqual(result.returncode, 1)
        self.assertIn("missing evidence fragment", result.stderr)

    def test_missing_scenario_fails_the_gate(self):
        self.assertEqual(self.build(self.robustness).returncode, 0)
        result = check(self.report)
        self.assertEqual(result.returncode, 1)
        self.assertIn("missing-scenario=e5-sql-work-budget", result.stdout + result.stderr)

    def test_fragment_without_the_release_digest_fails_closed(self):
        result = self.build(self.sql)
        self.assertEqual(result.returncode, 1)
        self.assertIn("candidate release digest", result.stderr)

    def test_disagreeing_candidate_identity_fails_closed(self):
        other = json.loads(self.sql.read_text())
        other["candidate_sha"] = "d" * 40
        forged = self.root / "forged.json"
        forged.write_text(json.dumps(other))
        result = self.build(forged, self.robustness)
        self.assertEqual(result.returncode, 1)
        self.assertIn("does not match the other fragments", result.stderr)

    def test_duplicate_scenario_across_fragments_fails_closed(self):
        copy = self.root / "copy.json"
        copy.write_text(self.robustness.read_text())
        result = self.build(self.robustness, copy)
        self.assertEqual(result.returncode, 1)
        self.assertIn("more than one fragment", result.stderr)

    def test_disabled_gate_is_rejected_by_the_checker(self):
        self.assertEqual(self.build(extra=("--disabled-gate", "early")).returncode, 0)
        result = check(self.report)
        self.assertEqual(result.returncode, 1)
        self.assertIn("disabled-gate", result.stdout + result.stderr)


class RedactionTests(unittest.TestCase):
    def test_generated_credentials_are_removed_before_upload(self):
        with tempfile.TemporaryDirectory() as tmp:
            art = write_robustness_artifacts(Path(tmp) / "artifacts")
            result = run("redact", "--artifacts", str(art))
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertFalse((art / "internal-token.txt").exists())
            self.assertFalse((art / "kubeconfig").exists())
            for path in art.rglob("*"):
                if path.is_file():
                    self.assertNotIn(TOKEN.encode(), path.read_bytes(), path)
            self.assertIn("[REDACTED_INTERNAL_TOKEN]", (art / "caesium-0.env").read_text())

    def test_redacted_directory_still_yields_a_complete_fragment(self):
        with tempfile.TemporaryDirectory() as tmp:
            art = write_robustness_artifacts(Path(tmp) / "artifacts")
            self.assertEqual(run("redact", "--artifacts", str(art)).returncode, 0)
            out = Path(tmp) / "fragment.json"
            self.assertEqual(run("robustness", "--artifacts", str(art),
                                 "--out", str(out)).returncode, 0)
            scenarios = json.loads(out.read_text())["scenarios"]
            self.assertEqual([item["status"] for item in scenarios], ["pass", "pass"])

    def test_missing_directory_fails_closed(self):
        with tempfile.TemporaryDirectory() as tmp:
            result = run("redact", "--artifacts", str(Path(tmp) / "absent"))
            self.assertEqual(result.returncode, 1)
            self.assertIn("is not a directory", result.stderr)


if __name__ == "__main__":
    unittest.main()
