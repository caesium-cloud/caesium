#!/usr/bin/env python3
"""Require exact, non-skipped top-level Go integration suite passes."""

from __future__ import annotations

import argparse
import json
import pathlib
import re
import sys
import unittest


SUITE = "TestIntegrationTestSuite"
NAME_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")


def _event_pattern(name: str) -> re.Pattern[str]:
    if not NAME_RE.fullmatch(name):
        raise ValueError(f"invalid integration test name: {name!r}")
    return re.compile(
        r"^--- (PASS|SKIP): "
        + re.escape(SUITE + "/" + name)
        + r"(?:[ \t]+\([^/\r\n]*\))?[ \t]*$"
    )


def check_output(output: str, required: list[str]) -> dict[str, object]:
    if len(required) != len(set(required)):
        raise ValueError("required integration test names must be unique")
    patterns = {name: _event_pattern(name) for name in required}
    counts: dict[str, dict[str, int]] = {
        name: {"PASS": 0, "SKIP": 0} for name in required
    }
    for line in output.splitlines():
        for name, pattern in patterns.items():
            match = pattern.fullmatch(line)
            if match:
                counts[name][match.group(1)] += 1

    passed = [name for name in required if counts[name]["PASS"] == 1 and counts[name]["SKIP"] == 0]
    missing = [name for name in required if counts[name]["PASS"] == 0 and counts[name]["SKIP"] == 0]
    skipped = [name for name in required if counts[name]["SKIP"] > 0]
    duplicate = [name for name in required if counts[name]["PASS"] > 1]
    return {
        "required": list(required),
        "passed": passed,
        "missing": missing,
        "skipped": skipped,
        "duplicate": duplicate,
        "valid": not (missing or skipped or duplicate),
    }


def _run_check(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--log", required=True, type=pathlib.Path)
    parser.add_argument("--required-name", action="append", default=[])
    args = parser.parse_args(argv)
    try:
        output = args.log.read_text(encoding="utf-8", errors="replace")
        result = check_output(output, args.required_name)
    except (OSError, ValueError) as exc:
        print(f"named integration pass check failed: {exc}", file=sys.stderr)
        return 2
    print(json.dumps(result, sort_keys=True))
    if not result["valid"]:
        print("required top-level integration tests are missing, skipped, or duplicated", file=sys.stderr)
        return 1
    return 0


class NamedJourneyGuardTests(unittest.TestCase):
    def test_nested_pass_cannot_replace_missing_top_level_pass(self) -> None:
        result = check_output(
            "--- PASS: TestIntegrationTestSuite/TestRequired/subtest (0.00s)\n",
            ["TestRequired"],
        )
        self.assertEqual(result["missing"], ["TestRequired"])
        self.assertFalse(result["valid"])

    def test_exact_top_level_pass_is_accepted(self) -> None:
        result = check_output(
            "--- PASS: TestIntegrationTestSuite/TestRequired (0.01s)\n",
            ["TestRequired"],
        )
        self.assertEqual(result["passed"], ["TestRequired"])
        self.assertTrue(result["valid"])

    def test_skip_is_not_a_pass(self) -> None:
        result = check_output(
            "--- SKIP: TestIntegrationTestSuite/TestRequired (0.00s)\n",
            ["TestRequired"],
        )
        self.assertEqual(result["skipped"], ["TestRequired"])
        self.assertFalse(result["valid"])

    def test_lookalike_name_does_not_match(self) -> None:
        result = check_output(
            "--- PASS: TestIntegrationTestSuite/TestRequiredExtra (0.00s)\n",
            ["TestRequired"],
        )
        self.assertEqual(result["missing"], ["TestRequired"])
        self.assertFalse(result["valid"])

    def test_duplicate_top_level_pass_is_ambiguous(self) -> None:
        line = "--- PASS: TestIntegrationTestSuite/TestRequired (0.00s)\n"
        result = check_output(line + line, ["TestRequired"])
        self.assertEqual(result["duplicate"], ["TestRequired"])
        self.assertFalse(result["valid"])


def main() -> int:
    if len(sys.argv) == 2 and sys.argv[1] == "--self-test":
        suite = unittest.defaultTestLoader.loadTestsFromTestCase(NamedJourneyGuardTests)
        result = unittest.TextTestRunner(verbosity=2).run(suite)
        return 0 if result.wasSuccessful() else 1
    return _run_check(sys.argv[1:])


if __name__ == "__main__":
    raise SystemExit(main())
