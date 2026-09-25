"""No-Docker regression tests for E3's shared benchmark measurement harness."""

import hashlib
import json
import subprocess
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).resolve().parent / "performance.sh"
FILES = (
    "internal/run/owner_benchmark_test.go",
    "internal/run/recovery_benchmark_test.go",
)


def git(root, *args):
    return subprocess.check_output(["git", "-C", str(root), *args], text=True).strip()


class BenchmarkHarnessSetupTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="caesium-bench-harness-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.candidate = self.root / "candidate"
        self.base = self.root / "base"
        self.candidate.mkdir()
        subprocess.run(["git", "init", "-q", str(self.candidate)], check=True)
        git(self.candidate, "config", "user.name", "E3 Test")
        git(self.candidate, "config", "user.email", "e3@example.invalid")
        (self.candidate / "README").write_text("product source at base\n")
        git(self.candidate, "add", "--", "README")
        git(self.candidate, "commit", "-qm", "base product")
        self.base_sha = git(self.candidate, "rev-parse", "HEAD")
        subprocess.run(["git", "clone", "-q", str(self.candidate), str(self.base)], check=True)
        git(self.base, "checkout", "-q", "--detach", self.base_sha)
        self.contents = {}
        for path in FILES:
            data = f"package run\n// E3 measurement {path}\n".encode()
            target = self.candidate / path
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
            self.contents[path] = data
        git(self.candidate, "add", "--", *FILES)
        git(self.candidate, "commit", "-qm", "add measurement harness")
        self.candidate_sha = git(self.candidate, "rev-parse", "HEAD")
        self.manifest = self.root / "artifacts" / "benchmark-harness.json"

    def prepare(self, *, candidate_sha=None, base_sha=None):
        return subprocess.run(
            [
                "bash", str(SCRIPT), "prepare-bench-harness", str(self.candidate),
                str(self.base), candidate_sha or self.candidate_sha,
                base_sha or self.base_sha, str(self.manifest),
                "sha256:base-release", "sha256:candidate-release",
            ],
            capture_output=True, text=True, check=False,
        )

    def test_base_gets_exact_candidate_harness_after_release_identity_is_pinned(self):
        result = self.prepare()
        self.assertEqual(result.returncode, 0, result.stderr)
        doc = json.loads(self.manifest.read_text())
        self.assertEqual(doc["base_source_sha"], self.base_sha)
        self.assertEqual(doc["candidate_source_sha"], self.candidate_sha)
        self.assertEqual(doc["harness_source_sha"], self.candidate_sha)
        self.assertEqual(doc["base_release_image_id"], "sha256:base-release")
        self.assertEqual(doc["candidate_release_image_id"], "sha256:candidate-release")
        self.assertEqual(doc["base_overlay_paths"], list(FILES))
        self.assertEqual(git(self.base, "rev-parse", "HEAD"), self.base_sha)
        self.assertEqual(git(self.candidate, "status", "--porcelain"), "")
        self.assertEqual(
            set(git(self.base, "status", "--porcelain", "--untracked-files=all").splitlines()),
            {f"?? {path}" for path in FILES},
        )
        for entry in doc["files"]:
            path = entry["path"]
            self.assertEqual((self.base / path).read_bytes(), self.contents[path])
            self.assertEqual(entry["sha256"], hashlib.sha256(self.contents[path]).hexdigest())
            self.assertIsNone(entry["base_original_sha256"])
            self.assertTrue(entry["overlaid"])

    def test_dirty_or_wrong_sha_source_cannot_be_labelled_clean(self):
        (self.candidate / "untracked.txt").write_text("uncommitted\n")
        result = self.prepare()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("candidate checkout is dirty", result.stderr)
        self.assertFalse(self.manifest.exists())
        (self.candidate / "untracked.txt").unlink()
        result = self.prepare(candidate_sha=self.base_sha)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("candidate checkout HEAD", result.stderr)
        self.assertFalse(self.manifest.exists())


if __name__ == "__main__":
    unittest.main()
