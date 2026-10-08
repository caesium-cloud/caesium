"""Selector contract checks for integration-hermetic-tests.py."""
from contextlib import redirect_stderr, redirect_stdout
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).with_name("integration-hermetic-tests.py")
SPEC = importlib.util.spec_from_file_location("integration_hermetic_tests", SCRIPT)
runner = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runner)

SHUTDOWN_RESULT_TESTS = {
    "TestGracefulShutdownResultAcceptsSingleExecutionAndFencedRecovery",
    "TestGracefulShutdownResultRefusesIdentityStatusAndFenceDrift",
    "TestGracefulShutdownResultRequiresUniquePairedBoundEffects",
}


class HermeticIntegrationSelectorTests(unittest.TestCase):
    def test_all_original_requirements_and_shutdown_validators_are_selected(self):
        expected = [name for names in runner.TESTS.values() for name in names]
        self.assertEqual(len(expected), 38)
        self.assertEqual(len(set(expected)), 38)
        self.assertEqual(
            {package: len(names) for package, names in runner.TESTS.items()},
            {
                "./test/robustness": 10,
                "./test/lifecycle": 8,
                "./test/robustness/cluster": 15,
                "./test/performance": 5,
            },
        )
        self.assertTrue(SHUTDOWN_RESULT_TESTS <= set(runner.TESTS["./test/robustness"]))

        commands = []

        def fake_go_test(command, **kwargs):
            commands.append(command)
            package = command[-1]
            events = [
                json.dumps({"Action": "pass", "Test": name})
                for name in runner.TESTS[package]
            ]
            return subprocess.CompletedProcess(command, 0, "\n".join(events) + "\n", "")

        with patch.object(runner.subprocess, "run", side_effect=fake_go_test), \
                redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
            self.assertEqual(runner.main(), 0)

        self.assertEqual(len(commands), len(runner.TESTS))
        robustness = next(command for command in commands if command[-1] == "./test/robustness")
        self.assertEqual(
            robustness[:7],
            ["go", "test", "-json", "-race", "-count=1", "-tags=integration", "-timeout=3m"],
        )
        selector = robustness[robustness.index("-run") + 1]
        self.assertEqual(selector, "^(" + "|".join(runner.TESTS["./test/robustness"]) + ")$")
        for name in SHUTDOWN_RESULT_TESTS:
            self.assertIn(name, selector)

    def test_missing_shutdown_validator_pass_fails_closed(self):
        missing = "TestGracefulShutdownResultRequiresUniquePairedBoundEffects"
        commands = []

        def fake_go_test(command, **kwargs):
            commands.append(command)
            package = command[-1]
            passed = [name for name in runner.TESTS[package] if name != missing]
            events = [json.dumps({"Action": "pass", "Test": name}) for name in passed]
            return subprocess.CompletedProcess(command, 0, "\n".join(events) + "\n", "")

        stderr = io.StringIO()
        with patch.object(runner.subprocess, "run", side_effect=fake_go_test), \
                redirect_stdout(io.StringIO()), redirect_stderr(stderr):
            self.assertEqual(runner.main(), 1)
        self.assertEqual(len(commands), 1)
        self.assertIn(missing, stderr.getvalue())


if __name__ == "__main__":
    unittest.main()
