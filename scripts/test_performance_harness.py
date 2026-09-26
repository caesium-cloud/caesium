"""No-Docker regression tests for E3's shared benchmark measurement harness."""

import hashlib
import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).resolve().parent / "performance.sh"
FILES = (
    "internal/run/owner_benchmark_test.go",
    "internal/run/recovery_benchmark_test.go",
)
HELPER = "internal/run/owner_state_test.go"
UNRELATED_TEST = "internal/run/concurrency_test.go"


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
        helper = self.candidate / HELPER
        helper.parent.mkdir(parents=True)
        helper.write_text("package run\nfunc newTopoBuilder() {}\n")
        unrelated = self.candidate / UNRELATED_TEST
        unrelated.write_text("package run\n// unrelated test at base\n")
        git(self.candidate, "add", "--", "README", HELPER, UNRELATED_TEST)
        git(self.candidate, "commit", "-qm", "base product")
        self.base_sha = git(self.candidate, "rev-parse", "HEAD")
        subprocess.run(["git", "-C", str(self.candidate), "worktree", "add", "-q", "--detach",
                        str(self.base), self.base_sha], check=True)
        self.addCleanup(lambda: subprocess.run(
            ["git", "-C", str(self.candidate), "worktree", "remove", "--force", str(self.base)],
            check=False, capture_output=True,
        ))
        self.contents = {}
        for path in FILES:
            name = "BenchmarkOwnerFake" if "owner_" in path else "BenchmarkRecoverFake"
            data = f"package run\nimport \"testing\"\nfunc {name}(b *testing.B) {{}}\n".encode()
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

    def cleanup_overlay(self, *, base=None, base_sha=None, env=None):
        return subprocess.run(
            ["bash", str(SCRIPT), "cleanup-bench-harness", str(self.candidate),
             str(self.base) if base is None else base, base_sha or self.base_sha,
             self.candidate_sha],
            capture_output=True, text=True, check=False, env=env,
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
        self.assertEqual(doc["benchmark_names"], ["BenchmarkOwnerFake", "BenchmarkRecoverFake"])
        self.assertEqual(len(doc["helper_files"]), 1)
        self.assertEqual(doc["helper_files"][0]["path"], HELPER)
        self.assertEqual(doc["helper_files"][0]["sha256"], hashlib.sha256(
            (self.candidate / HELPER).read_bytes()).hexdigest())
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

    def test_overlay_cleanup_restores_a_clean_base_checkout(self):
        self.assertEqual(self.prepare().returncode, 0)
        result = self.cleanup_overlay()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(git(self.base, "status", "--porcelain", "--untracked-files=all"), "")
        self.assertTrue(all(not (self.base / path).exists() for path in FILES))

    def test_cleanup_rejects_candidate_or_empty_base_before_mutation(self):
        uncommitted = self.candidate / FILES[0]
        uncommitted.write_bytes(uncommitted.read_bytes() + b"// unsaved edit\n")
        original = uncommitted.read_bytes()
        for base in ("", str(self.candidate), str(self.base)):
            with self.subTest(base=base):
                result = self.cleanup_overlay(base=base, base_sha="0" * 40)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(uncommitted.read_bytes(), original)

    def test_cleanup_rejects_its_own_linked_checkout_before_mutation(self):
        # A controller can itself run from a linked worktree. Swapping the
        # arguments must not make its own checkout a cleanup target, even when
        # its HEAD and overlaid files pass every other identity check.
        controller = self.base / "scripts/performance.sh"
        controller.parent.mkdir(parents=True)
        shutil.copy2(SCRIPT, controller)
        git(self.base, "add", "--", "scripts/performance.sh")
        git(self.base, "commit", "-qm", "add controller to linked checkout")
        base_sha = git(self.base, "rev-parse", "HEAD")
        prepared = self.prepare(base_sha=base_sha)
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        overlay = self.base / FILES[0]
        original = overlay.read_bytes()

        result = subprocess.run(
            ["bash", str(controller), "cleanup-bench-harness", str(self.candidate),
             str(self.base), base_sha, self.candidate_sha],
            capture_output=True, text=True, check=False,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("base path resolves to the script checkout root", result.stderr)
        self.assertEqual(overlay.read_bytes(), original)

    def test_cleanup_rejects_wrong_base_sha_and_foreign_checkout_before_mutation(self):
        self.assertEqual(self.prepare().returncode, 0)
        overlay = self.base / FILES[0]
        original = overlay.read_bytes()
        result = self.cleanup_overlay(base_sha="0" * 40)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(overlay.read_bytes(), original)
        foreign = self.root / "foreign"
        subprocess.run(["git", "clone", "-q", str(self.candidate), str(foreign)], check=True)
        result = self.cleanup_overlay(base=str(foreign), base_sha=self.candidate_sha)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("not a linked worktree", result.stderr)
        self.assertEqual(overlay.read_bytes(), original)

    def test_different_test_helper_fails_before_overlay(self):
        helper = self.candidate / HELPER
        helper.write_text(helper.read_text() + "// candidate helper drift\n")
        git(self.candidate, "add", "--", HELPER)
        git(self.candidate, "commit", "-qm", "change benchmark helper")
        self.candidate_sha = git(self.candidate, "rev-parse", "HEAD")
        result = self.prepare()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(f"test helper differs between base and candidate: {HELPER}", result.stderr)
        self.assertFalse((self.base / FILES[0]).exists())
        self.assertFalse(self.manifest.exists())

    def test_unrelated_changed_and_added_tests_do_not_block_overlay(self):
        unrelated = self.candidate / UNRELATED_TEST
        unrelated.write_text("package run\n// changed independently in candidate\n")
        added = self.candidate / "internal/run/start_idempotency_test.go"
        added.write_text("package run\n// added independently in candidate\n")
        git(self.candidate, "add", "--", UNRELATED_TEST, "internal/run/start_idempotency_test.go")
        git(self.candidate, "commit", "-qm", "change unrelated tests")
        self.candidate_sha = git(self.candidate, "rev-parse", "HEAD")

        result = self.prepare()
        self.assertEqual(result.returncode, 0, result.stderr)
        doc = json.loads(self.manifest.read_text())
        self.assertEqual(doc["helper_files"], [{
            "path": HELPER,
            "sha256": hashlib.sha256((self.candidate / HELPER).read_bytes()).hexdigest(),
        }])
        self.assertEqual(doc["base_overlay_paths"], list(FILES))
        self.assertFalse((self.base / "internal/run/start_idempotency_test.go").exists())

    def test_failed_git_status_cannot_validate_overlay_cleanup(self):
        self.assertEqual(self.prepare().returncode, 0)
        real_git = shutil.which("git")
        self.assertIsNotNone(real_git)
        fake_bin = self.root / "fake-bin"
        fake_bin.mkdir()
        fake_git = fake_bin / "git"
        fake_git.write_text(
            "#!/bin/sh\n"
            "for arg in \"$@\"; do\n"
            "  if [ \"$arg\" = status ]; then echo status-unavailable >&2; exit 1; fi\n"
            "done\n"
            f'exec "{real_git}" "$@"\n'
        )
        fake_git.chmod(0o755)
        env = os.environ.copy()
        env["PATH"] = str(fake_bin) + os.pathsep + env.get("PATH", "")
        result = self.cleanup_overlay(env=env)
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("git status", result.stderr)
        self.assertTrue(all((self.base / path).exists() for path in FILES))


if __name__ == "__main__":
    unittest.main()
