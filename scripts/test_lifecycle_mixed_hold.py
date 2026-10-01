"""Hermetic tests for F2's held mixed-version window (H3, W9-δ).

Runs the real ``lc_mixed_hold_*`` block extracted from lifecycle-tests.sh under
bash with ``lc_ns`` and ``lc_run_timed`` stubbed. No Docker, kind or cluster.
The guards exercised:

* the hold starts only from the chart default (RollingUpdate, partition 0) and
  patches exactly ``partition: 2``;
* a hold is verified only when exactly caesium-2 runs the candidate at the
  update revision while caesium-0/1 are still the seeded previous-release pods,
  and it must be the same three pods after the runner observation;
* release restores the recorded strategy byte-for-byte (failure is fatal, 1)
  and reports a rollout that did not finish separately (2).
"""

import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
CONTROLLER = ROOT / "scripts/lifecycle-tests.sh"
PREV = "caesiumcloud/caesium:v0.1.0"
CAND = "caesiumcloud/caesium:" + "a" * 40
SEEDED = {"caesium-0": "uid-0", "caesium-1": "uid-1", "caesium-2": "uid-2"}


def hold_block():
    source = CONTROLLER.read_text()
    start = source.index("  # BEGIN lc_mixed_hold_*")
    end = source.index("  # END lc_mixed_hold_*")
    return source[start:end]


def statefulset(partition=0, strategy_type="RollingUpdate", updated=1, ready=3,
                current="rev-old", update="rev-new", generation=2, observed=2):
    strategy = {"type": strategy_type}
    if strategy_type == "RollingUpdate":
        strategy["rollingUpdate"] = {"partition": partition}
    return {"metadata": {"generation": generation},
            "spec": {"replicas": 3, "updateStrategy": strategy},
            "status": {"observedGeneration": observed, "currentRevision": current,
                       "updateRevision": update, "updatedReplicas": updated, "readyReplicas": ready}}


def pod(name, uid, revision, image, ready=True):
    return {"metadata": {"name": name, "uid": uid, "labels": {"controller-revision-hash": revision}},
            "spec": {"containers": [{"name": "caesium", "image": image}]},
            "status": {"podIP": "10.0.0." + name[-1], "phase": "Running",
                       "conditions": [{"type": "Ready", "status": "True" if ready else "False"}],
                       "containerStatuses": [{"name": "caesium", "imageID": "sha256:x", "restartCount": 0}]}}


def held_pods(**overrides):
    pods = {
        "caesium-0": pod("caesium-0", SEEDED["caesium-0"], "rev-old", PREV),
        "caesium-1": pod("caesium-1", SEEDED["caesium-1"], "rev-old", PREV),
        "caesium-2": pod("caesium-2", "uid-2-new", "rev-new", CAND),
    }
    pods.update(overrides)
    return {"items": list(pods.values())}


PRELUDE = r"""
set -uo pipefail
LC_ID=lifecycle-holdtest LC_ART="$ART" LC_KUBE="$ART/kubeconfig"
LC_PREV=caesiumcloud/caesium:v0.1.0 CAESIUM_LIFECYCLE_CANDIDATE_IMAGE="$CAND"
mkdir -p "$ART/cluster-logs"
# Serves the fixture for the current stage; records every patch.
lc_ns() {
  printf 'lc_ns %s\n' "$*" >>"$ART/calls.log"
  case "$1 $2" in
    "get statefulset") cat "$ART/fixture/sts-$(cat "$ART/stage").json" ;;
    "get pods") cat "$ART/fixture/pods-$(cat "$ART/stage").json" ;;
    "patch statefulset")
      shift 2
      while [[ $# -gt 0 ]]; do
        case "$1" in
          -p) printf '%s\n' "$2" >"$ART/patch-inline.json"; shift ;;
          --patch-file) cp "$2" "$ART/patch-file.json"; shift ;;
        esac
        shift
      done
      [[ "${PATCH_RC:-0}" == 0 ]] ;;
    *) return 0 ;;
  esac
}
lc_run_timed() { printf 'rollout %s\n' "$*" >>"$ART/calls.log"; return "${ROLLOUT_RC:-0}"; }
"""


class MixedHoldTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="caesium-mixed-hold-")
        self.addCleanup(self.tmp.cleanup)
        self.art = Path(self.tmp.name)
        (self.art / "fixture").mkdir()
        (self.art / "cluster-fixture.json").write_text(json.dumps(
            {"members": [{"name": name, "uid": uid} for name, uid in SEEDED.items()]}))

    def fixture(self, stage, sts=None, pods=None):
        if sts is not None:
            (self.art / "fixture" / f"sts-{stage}.json").write_text(json.dumps(sts))
        if pods is not None:
            (self.art / "fixture" / f"pods-{stage}.json").write_text(json.dumps(pods))

    def run_block(self, body, stage, **env):
        (self.art / "stage").write_text(stage)
        script = PRELUDE + hold_block() + body
        result = subprocess.run(["bash", "-c", script], capture_output=True, text=True,
                                env={**os.environ, "ART": str(self.art), "CAND": CAND,
                                     **{k: str(v) for k, v in env.items()}})
        return result

    def record(self):
        return json.loads((self.art / "cluster-logs" / "mixed-hold.json").read_text())

    def start_hold(self):
        self.fixture("before", sts=statefulset())
        result = self.run_block('lc_mixed_hold_start; echo "rc=$?"', "before")
        self.assertIn("rc=0", result.stdout, result.stderr)

    def test_hold_patches_partition_two_and_verifies_one_upgraded_member(self):
        self.start_hold()
        self.assertEqual(json.loads((self.art / "patch-inline.json").read_text()),
                         {"spec": {"updateStrategy": {"type": "RollingUpdate", "rollingUpdate": {"partition": 2}}}})
        self.fixture("after-helm", sts=statefulset(partition=2), pods=held_pods())
        result = self.run_block('lc_mixed_hold_verify after-helm; echo "rc=$?"', "after-helm")
        self.assertIn("rc=0", result.stdout, result.stderr)
        record = self.record()
        self.assertTrue(record["held"])
        self.assertEqual(record["original_update_strategy"], {"type": "RollingUpdate", "rollingUpdate": {"partition": 0}})
        self.assertEqual(record["stages"]["after-helm"]["problems"], [])
        self.assertEqual(record["stages"]["after-helm"]["pods"]["caesium-2"]["image"], CAND)
        self.fixture("after-observation", sts=statefulset(partition=2), pods=held_pods())
        result = self.run_block('lc_mixed_hold_verify after-observation; echo "rc=$?"', "after-observation")
        self.assertIn("rc=0", result.stdout, result.stderr)
        self.assertTrue(self.record()["held"])

    def test_non_default_strategy_is_not_held(self):
        self.fixture("before", sts=statefulset(strategy_type="OnDelete"))
        result = self.run_block('lc_mixed_hold_start; echo "rc=$?"', "before")
        self.assertIn("rc=1", result.stdout)
        self.assertFalse((self.art / "patch-inline.json").exists())
        self.fixture("before", sts=statefulset(partition=1))
        result = self.run_block('lc_mixed_hold_start; echo "rc=$?"', "before")
        self.assertIn("rc=1", result.stdout)
        self.assertFalse((self.art / "patch-inline.json").exists())

    def test_unhonoured_partition_or_recreated_previous_member_is_not_a_hold(self):
        self.start_hold()
        cases = {
            "rolled-past-partition": (statefulset(partition=2, updated=3),
                                      held_pods(**{"caesium-0": pod("caesium-0", "uid-0-new", "rev-new", CAND)})),
            "previous-member-recreated": (statefulset(partition=2),
                                          held_pods(**{"caesium-1": pod("caesium-1", "uid-1-new", "rev-old", PREV)})),
            "candidate-not-ready": (statefulset(partition=2, ready=2),
                                    held_pods(**{"caesium-2": pod("caesium-2", "uid-2-new", "rev-new", CAND, ready=False)})),
            "caesium-2-not-recreated": (statefulset(partition=2),
                                        held_pods(**{"caesium-2": pod("caesium-2", SEEDED["caesium-2"], "rev-new", CAND)})),
            "partition-lost": (statefulset(partition=0), held_pods()),
        }
        for name, (sts, pods) in cases.items():
            with self.subTest(name):
                self.fixture("after-helm", sts=sts, pods=pods)
                result = self.run_block('lc_mixed_hold_verify after-helm; echo "rc=$?"', "after-helm")
                self.assertIn("rc=1", result.stdout, result.stderr)
                record = self.record()
                self.assertFalse(record["held"])
                self.assertTrue(record["stages"]["after-helm"]["problems"])

    def test_pods_must_be_unchanged_across_the_observation(self):
        self.start_hold()
        self.fixture("after-helm", sts=statefulset(partition=2), pods=held_pods())
        self.assertIn("rc=0", self.run_block('lc_mixed_hold_verify after-helm; echo "rc=$?"', "after-helm").stdout)
        replaced = held_pods(**{"caesium-2": pod("caesium-2", "uid-2-other", "rev-new", CAND)})
        self.fixture("after-observation", sts=statefulset(partition=2), pods=replaced)
        result = self.run_block('lc_mixed_hold_verify after-observation; echo "rc=$?"', "after-observation")
        self.assertIn("rc=1", result.stdout)
        record = self.record()
        self.assertFalse(record["held"])
        self.assertIn("caesium-2 changed pod between after-helm and after-observation",
                      record["stages"]["after-observation"]["problems"])

    def test_release_restores_the_recorded_strategy_and_reports_rollout(self):
        self.start_hold()
        self.fixture("release", sts=statefulset(partition=0, updated=3, current="rev-new"))
        result = self.run_block('lc_mixed_release; echo "rc=$?"', "release")
        self.assertIn("rc=0", result.stdout, result.stderr)
        self.assertEqual(json.loads((self.art / "patch-file.json").read_text()),
                         {"spec": {"updateStrategy": {"type": "RollingUpdate", "rollingUpdate": {"partition": 0}}}})
        self.assertIn("rollout status statefulset/caesium --timeout=600s", (self.art / "calls.log").read_text())
        release = self.record()["release"]
        self.assertTrue(release["strategy_restored"])
        self.assertEqual(release["rollout_status_exit"], 0)
        result = self.run_block('lc_mixed_release; echo "rc=$?"', "release", ROLLOUT_RC=1)
        self.assertIn("rc=2", result.stdout)
        self.assertEqual(self.record()["release"]["rollout_status_exit"], 1)

    def test_release_that_cannot_restore_the_strategy_is_fatal(self):
        self.start_hold()
        self.fixture("release", sts=statefulset(partition=2))
        result = self.run_block('lc_mixed_release; echo "rc=$?"', "release")
        self.assertIn("rc=1", result.stdout)
        self.assertFalse(self.record()["release"]["strategy_restored"])
        result = self.run_block('lc_mixed_release; echo "rc=$?"', "release", PATCH_RC=1)
        self.assertIn("rc=1", result.stdout)

    def test_controller_holds_before_helm_and_releases_before_the_upgrade_evidence(self):
        source = CONTROLLER.read_text()
        start = source.index("lc_mixed_hold_start || LC_HOLD_RC=$?")
        helm = source.index("helm upgrade caesium", start)
        verify = source.index("lc_mixed_hold_verify after-helm", helm)
        window = source.index('lc_phase MixedWindow "$LC_PREV_IDS" "$LC_OLD_BASE" || LC_MIXED_RC=$?', verify)
        after = source.index("lc_mixed_hold_verify after-observation", window)
        release = source.index("lc_mixed_release || LC_RELEASE_RC=$?", after)
        pods = source.index("get pods -o json >\"$LC_ART/cluster-pods-after-upgrade.json\"", release)
        self.assertLess(start, pods)
        self.assertNotIn('lc_phase MixedWindow "$LC_PREV_IDS" "$LC_OLD_BASE" &', source)
        self.assertIn("'mixed_hold_exit':int(os.environ['LC_HOLD_RC'])", source)
        # The chart is untouched: the lifecycle values still leave updateStrategy unset.
        values = (ROOT / "helm/caesium/ci/test-values-lifecycle.yaml").read_text()
        self.assertNotIn("updateStrategy", values)
        self.assertNotIn("partition", values)


if __name__ == "__main__":
    unittest.main()
