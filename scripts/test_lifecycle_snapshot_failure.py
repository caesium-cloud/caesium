import json
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

    def run_manifest(self):
        env = os.environ.copy()
        env.update({
            "LC_ART": str(self.art), "LC_ID": self.lifecycle_id,
            "LC_SNAP_BATCH": "13", "LC_DIAG_PODS_RC": "0",
            "LC_DIAG_EVENTS_RC": "0", "LC_DIAG_LOG0_RC": "0",
            "LC_DIAG_LOG1_RC": "1", "LC_DIAG_COPY_RC": "0",
        })
        subprocess.run(["python3", str(SCRIPT)], env=env, check=True, capture_output=True)
        return json.loads((self.art / "cluster-logs/snapshot-failure-batch-13.json").read_text())

    def disputed_write(self, **changes):
        record = {
            "lifecycle_id": self.lifecycle_id, "batch": 13,
            "write_index": 99, "attempted_annotation": "006099",
            "job_id": "9aa0a5d2-1095-4e47-9d78-b6307c13f763",
            "apply_error": "Post /v1/jobdefs/apply: EOF",
            "readbacks": [
                {"pod": "caesium-0", "http_status": 200,
                 "job_id": "9aa0a5d2-1095-4e47-9d78-b6307c13f763",
                 "annotation": "006099", "matches_attempted": True},
                {"pod": "caesium-1", "error": "context deadline exceeded",
                 "matches_attempted": False},
            ],
        }
        record.update(changes)
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

    def test_stale_or_partial_readback_cannot_be_reported_captured(self):
        for change in (
            {"lifecycle_id": "lifecycle-old"},
            {"attempted_annotation": "006098"},
            {"readbacks": [{"pod": "caesium-0"}]},
            {"readbacks": [{"pod": "caesium-0", "matches_attempted": True},
                           {"pod": "caesium-1", "matches_attempted": False}]},
        ):
            with self.subTest(change=change):
                self.disputed_write(**change)
                result = self.run_manifest()
                self.assertEqual(result["disputed_write_readback"]["status"], "unavailable")


if __name__ == "__main__":
    unittest.main()
