import json
import copy
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("lifecycle-snapshot-failure.py")


class SnapshotFailureManifestTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="caesium-f2-snapshot-diag-")
        self.addCleanup(self.tmp.cleanup)
        self.art = Path(self.tmp.name)
        (self.art / "cluster-logs").mkdir()
        self.lifecycle_id = "lifecycle-current"

    def run_manifest(self, batch=13):
        env = os.environ.copy()
        env.update({
            "LC_ART": str(self.art), "LC_ID": self.lifecycle_id,
            "LC_SNAP_BATCH": str(batch), "LC_DIAG_PODS_RC": "0",
            "LC_DIAG_EVENTS_RC": "0", "LC_DIAG_LOG0_RC": "0",
            "LC_DIAG_LOG1_RC": "1", "LC_DIAG_COPY_RC": "0",
            "LC_DIAG_PREV_LOG0_RC": "3", "LC_DIAG_PREV_LOG1_RC": "4",
        })
        subprocess.run(["python3", str(SCRIPT)], env=env, check=True, capture_output=True)
        return json.loads((self.art / f"cluster-logs/snapshot-failure-batch-{batch:02d}.json").read_text())

    def valid_disputed_write(self):
        return {
            "lifecycle_id": self.lifecycle_id, "batch": 13,
            "write_index": 99, "attempted_annotation": "006099",
            "job_id": "9aa0a5d2-1095-4e47-9d78-b6307c13f763",
            "apply_error": "Post /v1/jobdefs/apply: EOF",
            "readbacks": [
                {"pod": "caesium-0", "http_status": 200,
                 "job_id": "9aa0a5d2-1095-4e47-9d78-b6307c13f763",
                 "annotation": "006099", "matches_attempted": True},
                {"pod": "caesium-1", "error": "context deadline exceeded",
                 "job_id": "", "annotation": "", "matches_attempted": None},
            ],
        }
    def disputed_write(self, record=None):
        record = record if record is not None else self.valid_disputed_write()
        (self.art / "cluster-snapshot-disputed-batch-13.json").write_text(
            json.dumps(record) + "\n"
        )

    def test_missing_readback_keeps_observation_unavailable(self):
        result = self.run_manifest()
        self.assertEqual(result["disputed_write_readback"]["status"], "unavailable")
        self.assertEqual(result["capture_exit_codes"]["caesium-1-current-log"], 1)
        self.assertEqual(result["batch"], 13)

    def test_current_disputed_write_records_both_survivors(self):
        self.disputed_write()
        result = self.run_manifest()
        readback = result["disputed_write_readback"]
        self.assertEqual(readback["status"], "captured")
        self.assertEqual(readback["observation"]["attempted_annotation"], "006099")
        self.assertEqual(len(readback["observation"]["readbacks"]), 2)

    def test_each_top_level_identity_and_write_check_rejects_its_own_mutation(self):
        for field, value, detail in (
            ("lifecycle_id", "lifecycle-old", "another lifecycle invocation or batch"),
            ("batch", 12, "another lifecycle invocation or batch"),
            ("write_index", 0, "write index"),
            ("write_index", 501, "write index"),
            ("write_index", True, "write index"),
            ("attempted_annotation", "006098", "annotation or Apply failure"),
            ("apply_error", "", "annotation or Apply failure"),
            ("apply_error", True, "annotation or Apply failure"),
            ("job_id", "", "job identity"),
            ("job_id", None, "job identity"),
        ):
            with self.subTest(field=field, value=value):
                record = self.valid_disputed_write()
                record[field] = value
                self.disputed_write(record)
                result = self.run_manifest()
                self.assertEqual(result["disputed_write_readback"]["status"], "unavailable")
                self.assertIn(detail, result["disputed_write_readback"]["detail"])

    def test_survivor_set_checks_reject_complete_but_wrong_rows(self):
        valid = self.valid_disputed_write()
        for rows in (
            valid["readbacks"][:1],
            [valid["readbacks"][0], dict(valid["readbacks"][1], pod="caesium-2")],
            [valid["readbacks"][0], copy.deepcopy(valid["readbacks"][0])],
        ):
            with self.subTest(rows=rows):
                record = self.valid_disputed_write()
                record["readbacks"] = rows
                self.disputed_write(record)
                result = self.run_manifest()["disputed_write_readback"]
                self.assertEqual(result["status"], "unavailable")
                self.assertIn("both surviving pods", result["detail"])

    def test_each_successful_row_field_is_required(self):
        for field in ("http_status", "job_id", "annotation", "matches_attempted"):
            with self.subTest(field=field):
                record = self.valid_disputed_write()
                del record["readbacks"][0][field]
                self.disputed_write(record)
                result = self.run_manifest()["disputed_write_readback"]
                self.assertEqual(result["status"], "unavailable")
                self.assertIn({"http_status": "HTTP status", "job_id": "catalog job and annotation",
                               "annotation": "catalog job and annotation", "matches_attempted": "contradicts"}[field], result["detail"])

    def test_match_claim_must_agree_with_each_row_field(self):
        for change in ({"annotation": "006098"}, {"job_id": "foreign-job"},
                       {"error": "deadline exceeded"}, {"http_status": 503},
                       {"matches_attempted": False}, {"matches_attempted": None},
                       {"matches_attempted": 1}):
            with self.subTest(change=change):
                record = self.valid_disputed_write()
                record["readbacks"][0].update(change)
                self.disputed_write(record)
                result = self.run_manifest()["disputed_write_readback"]
                self.assertEqual(result["status"], "unavailable")
                self.assertIn("matches_attempted", result["detail"])

    def test_failed_row_cannot_claim_false_as_a_known_result(self):
        for value in (False, True):
            record = self.valid_disputed_write()
            record["readbacks"][1]["matches_attempted"] = value
            self.disputed_write(record)
            result = self.run_manifest()["disputed_write_readback"]
            self.assertEqual(result["status"], "unavailable")
            self.assertIn("unknown", result["detail"])

    def test_go_serialized_unannotated_job_is_a_valid_nonmatching_observation(self):
        record = self.valid_disputed_write()
        record["readbacks"][0].update(annotation="", matches_attempted=False)
        self.disputed_write(record)
        result = self.run_manifest()["disputed_write_readback"]
        self.assertEqual(result["status"], "captured")
        self.assertEqual(result["observation"]["readbacks"][0]["annotation"], "")

    def test_missing_survivor_keeps_other_survivor_observation(self):
        record = self.valid_disputed_write()
        record["readbacks"][1]["error"] = "survivor has no observable HTTP address"
        self.disputed_write(record)
        result = self.run_manifest()["disputed_write_readback"]
        self.assertEqual(result["status"], "captured")
        self.assertTrue(result["observation"]["readbacks"][0]["matches_attempted"])
        self.assertIsNone(result["observation"]["readbacks"][1]["matches_attempted"])

    def test_batch_zero_preserves_host_capture_with_explicit_unavailable_readback(self):
        # Execute the actual failure-branch body and capture helper with mocked
        # kubectl/controller functions, so a batch-zero guard would fail this test.
        source = SCRIPT.with_name("lifecycle-tests.sh").read_text()
        helper = source.split("    lc_capture_snapshot_write_failure() {", 1)[1].split(
            "    # Exit 0 only after", 1)[0]
        helper = "lc_capture_snapshot_write_failure() {" + helper
        failure = source.split('          LC_SNAP_REASON="catalog write phase failed at batch $LC_BATCH_TAG"', 1)[1].split(
            "          break", 1)[0]
        command = helper + '''
lc_ns() {
  printf '%s\\n' "$*" >>"$LC_ART/calls.log"
  printf 'observed output\\n'
  case "$*" in
    *'logs caesium-0'*'--previous'*) return 3;;
    *'logs caesium-1'*'--previous'*) return 4;;
  esac
}
lc_run_timed() { printf 'copy unavailable\\n' >"$2"; return 7; }
LC_BATCH=0
LC_BATCH_TAG=00
''' + failure
        env = os.environ.copy()
        env.update(LC_ART=str(self.art), LC_ID=self.lifecycle_id,
                   ROOT=str(SCRIPT.parent.parent), LC_KUBE="unused")
        subprocess.run(["bash", "-c", command], env=env, check=True, capture_output=True)
        result = json.loads((self.art / "cluster-logs/snapshot-failure-batch-00.json").read_text())
        self.assertEqual(result["phase_log"], "cluster-logs/GenerateSnapshotWrites.log")
        self.assertEqual(result["disputed_write_readback"]["status"], "unavailable")
        self.assertIn("distinct-write phase", result["disputed_write_readback"]["detail"])
        for pod, code in (("caesium-0", 3), ("caesium-1", 4)):
            self.assertEqual(result["capture_exit_codes"][pod + "-previous-log"], code)
            self.assertTrue((self.art / result["artifacts"][pod + "-previous-log"]).is_file())
        self.assertEqual(result["capture_exit_codes"]["runner-artifact-copy"], 7)
        calls = (self.art / "calls.log").read_text().splitlines()
        self.assertTrue(all("--request-timeout=10s" in call for call in calls))
        self.assertIn("--previous", calls[1])
        self.assertIn("--previous", calls[2])


if __name__ == "__main__":
    unittest.main()
