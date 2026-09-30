"""Hermetic checks for the F3 soak controller's host-side logic.

No docker, kind or cluster: records, samples and inventories are fixtures.
A missing record or sample is blocked, never a pass.
"""

import importlib.util
import json
import os
import re
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = Path(__file__).with_name("soak-report.py")
CONTROLLER = ROOT / "scripts/soak-tests.sh"
RUNNER = ROOT / "test/robustness/exploratory_test.go"

spec = importlib.util.spec_from_file_location("soak_report", SCRIPT)
soak = importlib.util.module_from_spec(spec)
spec.loader.exec_module(soak)

MIB = 1024 * 1024


def sample_text(rss_mib=100, fds=40, goroutines=120, heap_mib=20, cmd="/bin/caesium start", db_mib=10, log_mib=5):
    return "\n".join([
        f"DQFILE {log_mib * MIB} 0000000000000001-0000000000000100",
        f"DQFILE {8 * MIB} open-3",
        f"DQFILE {db_mib * MIB} snapshot-1-1024-123456",
        "DQFILE 60 info.yaml",
        'DBQUERY {"row_count":1,"rows":[[%d]],"truncated":false}' % (db_mib * MIB),
        f"CMDLINE {cmd}",
        f"VmRSS:\t  {rss_mib * 1024} kB",
        f"VmHWM:\t  {rss_mib * 1024 + 100} kB",
        "Threads:\t18",
        f"FDS {fds}",
        f"MEMMAX {512 * MIB}",
        f"MEMCUR {rss_mib * MIB}",
        f"go_goroutines {goroutines}",
        f"go_memstats_heap_inuse_bytes {heap_mib * MIB:.6e}",
        "go_memstats_sys_bytes 5.0e+07",
        f"process_open_fds {fds}",
    ]) + "\n"


class Fixture:
    def __init__(self, tmp: Path, soak_id="soak-t1", seed=42):
        self.art = tmp
        self.soak_id = soak_id
        self.seed = seed
        soak.main(["init", "--artifacts", str(tmp), "--soak-id", soak_id, "--candidate-sha", "a" * 40,
                   "--seed", str(seed), "--profile", "short"])
        (tmp / "records").mkdir(exist_ok=True)
        (tmp / "samples").mkdir(exist_ok=True)
        (tmp / "containers").mkdir(exist_ok=True)
        soak.main(["set", "--artifacts", str(tmp), 'provenance={"mode":"built-by-this-run","verified":true,"override":false}'])

    def phases(self, **rcs):
        (self.art / "phases.txt").write_text("".join(f"{k}={v}\n" for k, v in rcs.items()))

    def record(self, key, value):
        (self.art / "records" / f"{key}.json").write_text(json.dumps(value))

    def episode(self, index, family, status="pass", **extra):
        rec = {"key": f"episode-{index:02d}-{family}", "family": family, "index": index, "mandatory": True,
               "status": status, "soak_id": self.soak_id, "seed": self.seed, "params": {}, "observations": {}}
        rec.update(extra)
        self.record(rec["key"], rec)

    def plan(self):
        self.record("plan", {"soak_id": self.soak_id, "seed": self.seed, "plan": [
            {"index": i, "family": f, "mandatory": True} for i, f in enumerate(soak.FAMILIES)]})

    def drain(self, status="pass"):
        self.record("drain", {"key": "drain", "status": status, "soak_id": self.soak_id, "seed": self.seed,
                              "observations": {"admitted_runs": 30, "final_statuses": {"succeeded": 30}}})

    def sample(self, label, meta=None, **per_pod):
        """meta: pod -> the .pod.json document (identity) for that pod, replacing the default."""
        for pod in ("caesium-0", "caesium-1", "caesium-2"):
            kw = per_pod.get(pod, {})
            (self.art / "samples" / f"{label}--{pod}.txt").write_text(sample_text(**kw))
            ident = {"uid": pod + "-uid", "container_id": "containerd://" + pod, "restart_count": 0}
            if meta and pod in meta:
                ident = meta[pod]
            (self.art / "samples" / f"{label}--{pod}.pod.json").write_text(
                ident if isinstance(ident, str) else json.dumps(ident))
        soak.main(["sample-parse", "--dir", str(self.art / "samples"), "--label", label])

    @staticmethod
    def observed(path, doc, rc=0):
        """Write one collected observation as take_inventory does: output, then its exit code."""
        path.write_text(json.dumps(doc))
        Path(str(path) + ".rc").write_text(f"{rc}\n")

    def inventory(self, label, containers=(), pods=()):
        c = self.art / "containers"
        ps = {"containers": []}
        for cid, state, owned in containers:
            ps["containers"].append({"id": cid, "state": state, "labels": {
                "io.kubernetes.pod.namespace": "soak-t1", "io.kubernetes.container.name": "atom",
                "io.kubernetes.pod.name": "task-" + cid[:4]}})
            env = [{"key": "CAESIUM_SOAK_OWNER", "value": "tok" if owned else "other"}]
            self.observed(c / f"{label}--inspect--{cid}.json", {"status": {"id": cid}, "info": {"config": {"envs": env}}})
        ps["containers"].append({"id": "f" * 64, "state": "CONTAINER_RUNNING", "labels": {
            "io.kubernetes.pod.namespace": "soak-t1", "io.kubernetes.container.name": "caesium"}})
        for node in ("n-control-plane", "n-worker", "n-worker2", "n-worker3"):
            self.observed(c / f"{label}--ps--{node}.json", ps if node == "n-worker" else {"containers": []})
        self.observed(c / f"{label}--pods.json", {"items": list(pods)})
        self.reparse(label)

    def reparse(self, label="post-drain"):
        soak.main(["containers-parse", "--dir", str(self.art / "containers"), "--label", label, "--namespace", "soak-t1",
                   "--token", "tok", "--expect-nodes", "4"])

    def passing(self):
        self.plan()
        for i, f in enumerate(soak.FAMILIES):
            self.episode(i, f)
        self.drain()
        self.sample("baseline")
        for i in (1, 2, 3):
            self.sample(f"post-drain-{i}")
        self.inventory("baseline")
        self.inventory("post-drain")
        self.phases(inputs=0, provenance=0, build=0, kind_create=0, node_tools=0, image_load=0,
                    helm_install=0, topology=0, runner_start=0, runner=0, cleanup=0)

    def finalize(self):
        rc = soak.main(["finalize", "--artifacts", str(self.art)])
        return rc, json.loads((self.art / "soak.json").read_text())


class SoakReportTest(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.fx = Fixture(Path(self._tmp.name))

    def tearDown(self):
        self._tmp.cleanup()

    def test_init_is_incomplete_and_bare_finalize_is_blocked(self):
        doc = json.loads((self.fx.art / "soak.json").read_text())
        self.assertEqual(doc["result"], "incomplete")
        self.assertEqual(doc["manifest"]["families"], soak.FAMILIES)
        rc, doc = self.fx.finalize()
        self.assertEqual(rc, 1)
        self.assertEqual(doc["result"], "blocked")
        for fam in soak.FAMILIES + soak.POST_DRAIN_CHECKS:
            self.assertEqual(doc["scenarios"][fam]["status"], "blocked", fam)

    def test_complete_passing_run(self):
        self.fx.passing()
        rc, doc = self.fx.finalize()
        self.assertEqual((rc, doc["result"]), (0, "pass"), doc["detail"])
        self.assertEqual(doc["failed_gates"], [])
        self.assertIn("not power-loss", doc["fault_class"])

    def test_failed_episode_fails_the_run(self):
        self.fx.passing()
        self.fx.episode(1, soak.FAMILIES[1], status="fail", detail="dequeue order wrong")
        rc, doc = self.fx.finalize()
        self.assertEqual(doc["result"], "fail")
        self.assertIn("dequeue order wrong", doc["detail"])

    def test_missing_mandatory_episode_is_blocked(self):
        self.fx.passing()
        (self.fx.art / "records" / "episode-03-repeated_failover.json").unlink()
        rc, doc = self.fx.finalize()
        self.assertEqual(doc["result"], "blocked")
        self.assertEqual(doc["scenarios"]["repeated_failover"]["status"], "blocked")

    def test_record_from_another_invocation_does_not_count(self):
        self.fx.passing()
        self.fx.record("episode-02-retention", {"key": "episode-02-retention", "family": "retention", "index": 2,
                                                "status": "pass", "soak_id": "soak-other", "seed": 42})
        rc, doc = self.fx.finalize()
        self.assertEqual(doc["scenarios"]["retention"]["status"], "blocked")

    def test_blocked_setup_phase_is_blocked_and_cleanup_failure_is_fail(self):
        self.fx.passing()
        self.fx.phases(inputs=0, provenance=0, build=0, kind_create=1, runner=0, cleanup=0)
        self.assertEqual(self.fx.finalize()[1]["result"], "blocked")
        self.fx.phases(inputs=0, provenance=0, build=0, kind_create=0, runner=0, cleanup=1)
        self.assertEqual(self.fx.finalize()[1]["result"], "fail")

    def test_runner_exit_alone_is_blocked_not_pass(self):
        self.fx.passing()
        self.fx.phases(inputs=0, runner=1, cleanup=0)
        rc, doc = self.fx.finalize()
        self.assertEqual(doc["result"], "blocked")
        self.fx.phases(inputs=0, cleanup=0)
        self.assertEqual(self.fx.finalize()[1]["result"], "blocked")

    def test_unverified_provenance_needs_a_recorded_override(self):
        self.fx.passing()
        soak.main(["set", "--artifacts", str(self.fx.art), 'provenance={"mode":"supplied","verified":false,"override":false}'])
        self.assertEqual(self.fx.finalize()[1]["result"], "blocked")
        soak.main(["set", "--artifacts", str(self.fx.art), 'provenance={"mode":"supplied","verified":false,"override":true}'])
        self.assertEqual(self.fx.finalize()[1]["result"], "pass")

    def test_retained_database_and_raft_log_are_not_a_leak(self):
        self.fx.passing()
        for i in (1, 2, 3):
            self.fx.sample(f"post-drain-{i}", **{"caesium-1": {"rss_mib": 260, "db_mib": 60, "log_mib": 90}})
        doc = self.fx.finalize()[1]
        self.assertEqual(doc["scenarios"]["post_drain_resources"]["status"], "pass", doc["scenarios"]["post_drain_resources"]["detail"])
        m = doc["scenarios"]["post_drain_resources"]["members"]["caesium-1"]
        self.assertEqual(m["post_native_rss_mib"], 110.0)

    def test_missing_database_size_is_blocked(self):
        self.fx.passing()
        p = self.fx.art / "samples" / "post-drain-3--caesium-0.txt"
        p.write_text("\n".join(l for l in p.read_text().splitlines() if not l.startswith("DBQUERY")) + "\n")
        soak.main(["sample-parse", "--dir", str(self.fx.art / "samples"), "--label", "post-drain-3"])
        self.assertEqual(self.fx.finalize()[1]["scenarios"]["post_drain_resources"]["status"], "blocked")

    def test_rss_growth_and_instability_fail(self):
        self.fx.passing()
        self.fx.sample("post-drain-3", **{"caesium-1": {"rss_mib": 300}})
        rc, doc = self.fx.finalize()
        self.assertEqual(doc["scenarios"]["post_drain_resources"]["status"], "fail")
        self.assertIn("caesium-1", doc["scenarios"]["post_drain_resources"]["detail"])

    def test_fd_leak_fails(self):
        self.fx.passing()
        for i in (1, 2, 3):
            self.fx.sample(f"post-drain-{i}", **{"caesium-2": {"fds": 90}})
        rc, doc = self.fx.finalize()
        self.assertEqual(doc["scenarios"]["post_drain_fds"]["status"], "fail")
        self.assertEqual(doc["scenarios"]["post_drain_resources"]["status"], "pass")

    def test_goroutine_leak_fails(self):
        self.fx.passing()
        for i in (1, 2, 3):
            self.fx.sample(f"post-drain-{i}", **{"caesium-0": {"goroutines": 400}})
        self.assertEqual(self.fx.finalize()[1]["scenarios"]["post_drain_resources"]["status"], "fail")

    def test_single_post_drain_sample_is_blocked(self):
        self.fx.passing()
        for i in (2, 3):
            for f in (self.fx.art / "samples").glob(f"post-drain-{i}*"):
                f.unlink()
        doc = self.fx.finalize()[1]
        self.assertEqual(doc["scenarios"]["post_drain_resources"]["status"], "blocked")
        self.assertEqual(doc["result"], "blocked")

    def test_wrong_pid1_is_blocked(self):
        self.fx.passing()
        self.fx.sample("baseline", **{"caesium-0": {"cmd": "/bin/sh -ec"}})
        self.assertEqual(self.fx.finalize()[1]["scenarios"]["post_drain_resources"]["status"], "blocked")

    def test_container_leaks_fail(self):
        self.fx.passing()
        self.fx.inventory("post-drain", containers=[("a" * 64, "CONTAINER_EXITED", True)])
        doc = self.fx.finalize()[1]
        self.assertEqual(doc["scenarios"]["post_drain_containers"]["status"], "fail")
        self.fx.inventory("post-drain", containers=[("b" * 64, "CONTAINER_RUNNING", False)])
        self.assertEqual(self.fx.finalize()[1]["scenarios"]["post_drain_containers"]["status"], "fail")
        self.fx.inventory("post-drain", containers=[("c" * 64, "CONTAINER_EXITED", False)])
        self.assertEqual(self.fx.finalize()[1]["scenarios"]["post_drain_containers"]["status"], "pass")
        owned_pod = {"metadata": {"name": "t"}, "status": {"phase": "Succeeded"},
                     "spec": {"containers": [{"env": [{"name": "CAESIUM_SOAK_OWNER", "value": "tok"}]}]}}
        self.fx.inventory("post-drain", pods=[owned_pod])
        self.assertEqual(self.fx.finalize()[1]["scenarios"]["post_drain_containers"]["status"], "fail")

    def test_partial_inventory_is_blocked(self):
        self.fx.passing()
        (self.fx.art / "containers" / "post-drain--ps--n-worker3.json").unlink()
        self.fx.inventory  # the parse below re-reads only what exists
        soak.main(["containers-parse", "--dir", str(self.fx.art / "containers"), "--label", "post-drain",
                   "--namespace", "soak-t1", "--token", "tok", "--expect-nodes", "4"])
        self.assertEqual(self.fx.finalize()[1]["scenarios"]["post_drain_containers"]["status"], "blocked")

    # -- PR #597 review: missing inventory evidence blocks --------------------

    def test_emptied_container_listings_and_pod_list_are_blocked(self):
        """Reviewer's case: all four post-drain ps files and the pod list emptied."""
        self.fx.passing()
        c = self.fx.art / "containers"
        for f in list(c.glob("post-drain--ps--*.json")) + [c / "post-drain--pods.json"]:
            f.write_text("")
        self.fx.reparse()
        rc, doc = self.fx.finalize()
        self.assertEqual((rc, doc["result"]), (1, "blocked"))
        check = doc["scenarios"]["post_drain_containers"]
        self.assertEqual(check["status"], "blocked", check["detail"])
        self.assertIn("inventoried 0 nodes successfully, expected 4", check["detail"])

    def test_emptied_pod_list_alone_is_blocked(self):
        self.fx.passing()
        (self.fx.art / "containers" / "post-drain--pods.json").write_text("")
        self.fx.reparse()
        rc, doc = self.fx.finalize()
        self.assertEqual((rc, doc["result"]), (1, "blocked"))
        self.assertIn("task pod list unobservable", doc["scenarios"]["post_drain_containers"]["detail"])

    def test_failed_or_unfinished_collection_is_blocked(self):
        self.fx.passing()
        ps = self.fx.art / "containers" / "post-drain--ps--n-worker2.json"
        Path(str(ps) + ".rc").write_text("1\n")
        Path(str(ps) + ".err").write_text("crictl: connection refused\n")
        self.fx.reparse()
        check = self.fx.finalize()[1]["scenarios"]["post_drain_containers"]
        self.assertEqual(check["status"], "blocked")
        self.assertIn("command exited 1 (crictl: connection refused)", check["detail"])
        Path(str(ps) + ".rc").unlink()
        self.fx.reparse()
        check = self.fx.finalize()[1]["scenarios"]["post_drain_containers"]
        self.assertEqual(check["status"], "blocked")
        self.assertIn("no exit code recorded", check["detail"])

    def test_owned_exited_container_with_emptied_inspect_is_blocked(self):
        """Reviewer's case: an exited owned task container whose inspect is empty."""
        self.fx.passing()
        for cid, owned in (("a" * 64, True), ("c" * 64, False)):
            self.fx.inventory("post-drain", containers=[(cid, "CONTAINER_EXITED", owned)])
            (self.fx.art / "containers" / f"post-drain--inspect--{cid}.json").write_text("")
            self.fx.reparse()
            rc, doc = self.fx.finalize()
            check = doc["scenarios"]["post_drain_containers"]
            self.assertEqual((rc, check["status"]), (1, "blocked"), check["detail"])
            self.assertIn(f"ownership of task container {cid[:13]} unobservable", check["detail"])
            self.assertEqual(check["inventory"]["unverified_task_containers"], 1)

    def test_running_task_container_fails_even_without_its_inspect(self):
        self.fx.passing()
        self.fx.inventory("post-drain", containers=[("d" * 64, "CONTAINER_RUNNING", True)])
        (self.fx.art / "containers" / f"post-drain--inspect--{'d' * 64}.json").unlink()
        self.fx.reparse()
        check = self.fx.finalize()[1]["scenarios"]["post_drain_containers"]
        self.assertEqual(check["status"], "fail")
        self.assertIn("evidence also incomplete", check["detail"])

    # -- PR #597 review: one process across the post-drain series -------------

    def test_post_drain_restarts_are_not_certified(self):
        """Reviewer's case: three distinct container IDs with restart counts 1, 2, 3."""
        self.fx.passing()
        for i in (1, 2, 3):
            self.fx.sample(f"post-drain-{i}", meta={"caesium-1": {
                "uid": "caesium-1-uid", "container_id": f"containerd://fresh-{i}", "restart_count": i}})
        rc, doc = self.fx.finalize()
        self.assertEqual((rc, doc["result"]), (1, "fail"))
        for name in ("post_drain_resources", "post_drain_fds"):
            check = doc["scenarios"][name]
            self.assertEqual(check["status"], "fail", name)
            self.assertIn("caesium-1: restarted during the fault-free post-drain window", check["detail"])

    def test_missing_post_drain_identity_is_blocked(self):
        for broken in ({}, {"uid": "caesium-0-uid", "container_id": "containerd://caesium-0", "restart_count": None}, ""):
            with self.subTest(broken=broken):
                self.fx.passing()
                self.fx.sample("post-drain-2", meta={"caesium-0": broken})
                rc, doc = self.fx.finalize()
                self.assertEqual((rc, doc["result"]), (1, "blocked"))
                for name in ("post_drain_resources", "post_drain_fds"):
                    self.assertEqual(doc["scenarios"][name]["status"], "blocked", name)
                    self.assertIn("caesium-0: post-drain sample(s) [2] carry no pod UID", doc["scenarios"][name]["detail"])

    def test_replacement_before_the_drain_is_allowed(self):
        self.fx.passing()
        fresh = {"uid": "caesium-2-uid-new", "container_id": "containerd://replaced", "restart_count": 1}
        for i in (1, 2, 3):
            self.fx.sample(f"post-drain-{i}", meta={"caesium-2": fresh})
        rc, doc = self.fx.finalize()
        self.assertEqual((rc, doc["result"]), (0, "pass"), doc["detail"])
        m = doc["scenarios"]["post_drain_resources"]["members"]["caesium-2"]
        self.assertTrue(m["process_restarted_since_baseline"])
        self.assertEqual(m["post_drain_identity"]["container_id"], "containerd://replaced")

    def test_fault_schedule_is_retained_in_order(self):
        self.fx.passing()
        for action in ("cordon", "kill", "restart"):
            soak.main(["schedule-append", "--artifacts", str(self.fx.art), "--action", action, "--status", "ok",
                       "--episode", "episode-03-repeated_failover", "--pod", "caesium-1", "--node", "n-worker",
                       "--started-at", "2026-09-30T00:00:00.000Z"])
        doc = self.fx.finalize()[1]
        self.assertEqual([(e["seq"], e["action"]) for e in doc["fault_schedule"]], [(0, "cordon"), (1, "kill"), (2, "restart")])
        self.assertTrue(all(e["finished_at"].endswith("Z") for e in doc["fault_schedule"]))


class SeedTest(unittest.TestCase):
    """PR #597 review: generation, validation and the record share one range."""

    def test_generated_seed_boundaries_pass_the_validator(self):
        span = soak.SEED_MAX - soak.SEED_MIN + 1
        for draw, want in ((0, soak.SEED_MIN), (span - 1, soak.SEED_MAX)):
            with mock.patch.object(soak.secrets, "randbelow", return_value=draw) as rb:
                got = soak.resolve_seed("")
            rb.assert_called_once_with(span)
            self.assertEqual(got, {"seed": want, "source": "generated", "error": ""})
        self.assertEqual((soak.SEED_MIN, soak.SEED_MAX), (1, 10**18 - 1))
        # randbits(62) drew this before the fix; it is outside the range.
        self.assertIsNone(soak.validate_seed("1152921504606846976")[0])

    def test_supplied_seed_range(self):
        for raw, want in (("1", 1), ("999999999999999999", 10**18 - 1), ("007", 7), ("314159", 314159)):
            self.assertEqual(soak.resolve_seed(raw), {"seed": want, "source": "supplied", "error": ""}, raw)
        for raw in ("0", "1000000000000000000", "-5", "abc", " 42", "4611686018427387903"):
            got = soak.resolve_seed(raw)
            self.assertEqual((got["seed"], got["source"]), (None, "invalid"), raw)
            self.assertIn("must be an integer from 1 to 999999999999999999", got["error"])

    def test_placeholder_records_the_generated_seed(self):
        with tempfile.TemporaryDirectory() as tmp:
            soak.main(["init", "--artifacts", tmp, "--soak-id", "s", "--candidate-sha", "a" * 40,
                       "--seed", str(soak.SEED_MAX), "--seed-source", "generated", "--profile", "short"])
            doc = json.loads((Path(tmp) / "soak.json").read_text())
            self.assertEqual((doc["seed"], doc["seed_source"], doc["result"]), (soak.SEED_MAX, "generated", "incomplete"))
            soak.main(["init", "--artifacts", tmp, "--soak-id", "s", "--candidate-sha", "a" * 40,
                       "--seed", "", "--seed-source", "invalid", "--profile", "short"])
            doc = json.loads((Path(tmp) / "soak.json").read_text())
            self.assertEqual((doc["seed"], doc["seed_source"]), (None, "invalid"))

    def _print_seed(self, value=None):
        env = {k: v for k, v in os.environ.items() if k != "CAESIUM_SOAK_SEED"}
        if value is not None:
            env["CAESIUM_SOAK_SEED"] = value
        return subprocess.run(["bash", str(CONTROLLER), "--print-seed"], env=env, capture_output=True, text=True)

    def test_controller_unset_seed_path_is_accepted(self):
        """The controller's real seed path: unset (or empty) generates an accepted seed."""
        for value in (None, "", None, None, None, None):
            r = self._print_seed(value)
            self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
            m = re.fullmatch(r"seed=(\d+) source=generated\n", r.stdout)
            self.assertIsNotNone(m, r.stdout)
            self.assertTrue(soak.SEED_MIN <= int(m.group(1)) <= soak.SEED_MAX)
        r = self._print_seed("999999999999999999")
        self.assertEqual((r.returncode, r.stdout), (0, "seed=999999999999999999 source=supplied\n"))
        r = self._print_seed("1000000000000000000")
        self.assertEqual(r.returncode, 3, r.stdout + r.stderr)
        self.assertIn("must be an integer from 1 to 999999999999999999", r.stdout)

    def test_controller_uses_only_the_shared_seed_logic(self):
        text = CONTROLLER.read_text()
        self.assertNotIn("randbits", text)
        self.assertNotIn("SEED_RECORDED", text)
        self.assertIn('--seed "$SEED" --seed-source "$SEED_SOURCE"', text)
        self.assertIn('[[ -z "$SEED_ERROR" ]] || blocked inputs "$SEED_ERROR"', text)


class RequestHandlingTest(unittest.TestCase):
    def _cm(self, params, **fields):
        data = {"request_id": "r1", "action": "record"}
        data.update(fields)
        data["params"] = json.dumps(params)
        return {"data": data}

    def test_parse_request_quotes_everything(self):
        cm = self._cm({"episode": "e; rm -rf /", "label": "$(touch /tmp/x)"}, owner_pod="caesium-0'")
        out = subprocess.run([sys.executable, str(SCRIPT), "parse-request"], input=json.dumps(cm),
                             capture_output=True, text=True, check=True).stdout
        env = subprocess.run(["bash", "-c", out + '\nprintf "%s|%s|%s" "$p_episode" "$p_label" "$r_owner_pod"'],
                             capture_output=True, text=True, check=True).stdout
        self.assertEqual(env, "e; rm -rf /|$(touch /tmp/x)|caesium-0'")
        self.assertNotIn("p_payload", out, "only the fixed parameter set is exported to the shell")

    def test_store_record_validates_key_and_payload(self):
        with tempfile.TemporaryDirectory() as tmp:
            good = self._cm({"key": "episode-01-retention", "payload": json.dumps({"status": "pass"})})
            r = subprocess.run([sys.executable, str(SCRIPT), "store-record", "--artifacts", tmp],
                               input=json.dumps(good), capture_output=True, text=True)
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertEqual(json.loads((Path(tmp) / "records/episode-01-retention.json").read_text()), {"status": "pass"})
            for bad in ({"key": "../escape", "payload": "{}"}, {"key": "ok", "payload": "not json"}):
                r = subprocess.run([sys.executable, str(SCRIPT), "store-record", "--artifacts", tmp],
                                   input=json.dumps(self._cm(bad)), capture_output=True, text=True)
                self.assertNotEqual(r.returncode, 0, bad)
            self.assertFalse((Path(tmp).parent / "escape.json").exists())

    def test_parse_sample_text(self):
        got = soak.parse_sample_text(sample_text(rss_mib=123, fds=51, goroutines=77, heap_mib=9))
        self.assertEqual(got["vmrss_bytes"], 123 * 1024 * 1024)
        self.assertEqual(got["open_fds"], 51)
        self.assertEqual(got["go_goroutines"], 77)
        self.assertEqual(got["threads"], 18)
        self.assertEqual(got["cgroup_memory_max_bytes"], 512 * MIB)
        self.assertEqual(got["db_bytes"], 10 * MIB)
        self.assertEqual(got["closed_segment_bytes"], 5 * MIB)
        self.assertEqual(got["closed_segments"], 1)
        self.assertEqual(got["snapshot_bytes"], 10 * MIB)
        self.assertEqual(got["errors"], [])
        self.assertIn("missing open_fds", soak.parse_sample_text("CMDLINE /bin/caesium start\n")["errors"])


class ControllerShapeTest(unittest.TestCase):
    def test_controller_parses(self):
        subprocess.run(["bash", "-n", str(CONTROLLER)], check=True)

    def test_inventory_commands_record_their_exit_code(self):
        text = CONTROLLER.read_text()
        body = text[text.index("take_inventory() {"):]
        body = body[:body.index("\n}\n")]
        for cmd in ("crictl ps -a -o json", "crictl inspect", "get pods -l cloud.caesium -o json"):
            line = next(l for l in body.splitlines() if cmd in l)
            self.assertRegex(line.strip(), r"^collect ", f"{cmd} must run through collect (exit code sidecar)")
        self.assertIn('printf \'%s\\n\' "$rc" >"$out.rc"', text)

    def test_controller_owns_and_deletes_only_its_cluster(self):
        text = CONTROLLER.read_text()
        self.assertIn('kind delete cluster --name "$SOAK_ID"', text)
        self.assertEqual(len(re.findall(r"kind delete cluster", text)), 1)
        self.assertIn("already exists; refusing to claim", text)
        self.assertIn("trap cleanup EXIT", text)
        self.assertNotIn("git stash", text)

    def test_no_process_kill_is_labelled_power_loss(self):
        for path in (CONTROLLER, SCRIPT, RUNNER):
            for n, line in enumerate(path.read_text().splitlines(), 1):
                low = line.lower()
                if "power" in low and "loss" in low:
                    self.assertRegex(low, r"\b(not|never|none|no|nothing)\b", f"{path.name}:{n} labels something power loss: {line.strip()}")


if __name__ == "__main__":
    unittest.main()
