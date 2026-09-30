"""Synthetic go test streams that pin the C3 validator's fail-closed behavior."""

import json
import unittest

from oracle_probe_result import validate


TEST = "TestC3UnaccountedExternalEffect"
MARKER = "unaccounted completion escaped per-step effect checker"


def event(action, *, test=None, output=None):
    item = {"Action": action, "Package": "example/history"}
    if test is not None:
        item["Test"] = test
    if output is not None:
        item["Output"] = output
    return json.dumps(item)


def mutant_stream(extra=()):
    lines = [
        event("start"),
        event("run", test=TEST),
        event("output", test=TEST, output=f"    oracle_probe_test.go:42: {MARKER}\n"),
        event("fail", test=TEST),
        *extra,
        event("output", output="FAIL\texample/history\t0.12s\n"),
        event("fail"),
    ]
    return "\n".join(lines)


class OracleProbeResultTests(unittest.TestCase):
    def test_candidate_success_is_accepted(self):
        good = "\n".join([
            event("start"), event("run", test=TEST), event("pass", test=TEST),
            event("output", output="PASS\n"),
            event("output", output="ok  \texample/history\t0.12s\n"),
            event("pass"),
        ])
        self.assertEqual(validate(good, 0, "candidate", [TEST], ""), [])

    def test_expected_mutant_assertion_is_accepted(self):
        self.assertEqual(validate(mutant_stream(), 1, "mutant", [TEST], MARKER), [])

    def test_newosproc_after_named_assertion_is_inconclusive(self):
        # The old validator accepted this exact shape: named fail + marker +
        # exit 1, followed by an exhausted Go runner.
        broken = mutant_stream([event("output", output="fatal error: newosproc\n")])
        self.assertTrue(validate(broken, 1, "mutant", [TEST], MARKER))

    def test_file_descriptor_exhaustion_after_assertion_is_inconclusive(self):
        broken = mutant_stream([
            event("output", test=TEST, output="open /tmp/checker: too many open files\n")
        ])
        self.assertTrue(validate(broken, 1, "mutant", [TEST], MARKER))

    def test_disk_quota_exhaustion_after_assertion_is_inconclusive(self):
        broken = mutant_stream([
            event("output", test=TEST, output="write /tmp/checker: disk quota exceeded\n")
        ])
        self.assertTrue(validate(broken, 1, "mutant", [TEST], MARKER))

    def test_other_resource_diagnostics_are_inconclusive(self):
        for diagnostic in (
            "runtime: failed to create new OS thread\n",
            "fork/exec /usr/local/go/pkg/tool/compile: resource temporarily unavailable\n",
            "signal: killed\n",
            "unexpected fault address 0x0\n",
        ):
            with self.subTest(diagnostic=diagnostic):
                broken = mutant_stream([event("output", test=TEST, output=diagnostic)])
                self.assertTrue(validate(broken, 1, "mutant", [TEST], MARKER))

    def test_unexpected_package_failure_after_marker_is_inconclusive(self):
        broken = mutant_stream([event("output", output="TestMain: setup failed\n")])
        self.assertTrue(validate(broken, 1, "mutant", [TEST], MARKER))

    def test_unexpected_test_failure_is_inconclusive(self):
        broken = mutant_stream([event("fail", test="TestOther")])
        self.assertTrue(validate(broken, 1, "mutant", [TEST], MARKER))

    def test_non_json_runner_output_is_inconclusive(self):
        broken = mutant_stream(["go: failed to initialize build cache"])
        self.assertTrue(validate(broken, 1, "mutant", [TEST], MARKER))

    def test_marker_must_belong_to_named_test(self):
        broken = mutant_stream().replace(
            event("output", test=TEST, output=f"    oracle_probe_test.go:42: {MARKER}\n"),
            event("output", output=f"{MARKER}\n"),
        )
        self.assertTrue(validate(broken, 1, "mutant", [TEST], MARKER))

    def test_missing_and_skipped_tests_fail_closed(self):
        self.assertTrue(validate(event("pass"), 0, "candidate", [TEST], ""))
        skipped = "\n".join([event("skip", test=TEST), event("pass")])
        self.assertTrue(validate(skipped, 0, "candidate", [TEST], ""))

    def test_abnormal_exit_fails_despite_assertion(self):
        self.assertTrue(validate(mutant_stream(), -9, "mutant", [TEST], MARKER))


if __name__ == "__main__":
    unittest.main()
