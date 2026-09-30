#!/usr/bin/env python3
"""Require both live Chromium coverage journeys to pass on their first attempt."""

import json
import sys
from pathlib import Path


EXPECTED = {
    "sidebar navigates between every primary control-plane page",
    "operator can pause and unpause a job from the detail page",
}


def validate(doc):
    observed = {title: [] for title in EXPECTED}

    def visit(suite):
        for spec in suite.get("specs", []):
            title = spec.get("title")
            if title in observed:
                observed[title].extend(
                    [result.get("status") for result in test.get("results", [])]
                    for test in spec.get("tests", [])
                )
        for child in suite.get("suites", []):
            visit(child)

    for suite in doc.get("suites", []):
        visit(suite)
    if any(results != [["passed"]] for results in observed.values()):
        raise ValueError(f"browser journeys were not both first-attempt passes: {observed}")
    return observed


def main():
    if len(sys.argv) != 2:
        raise SystemExit("usage: check-browser-journey.py <playwright-results.json>")
    try:
        validate(json.loads(Path(sys.argv[1]).read_text()))
    except (OSError, json.JSONDecodeError, ValueError) as err:
        raise SystemExit(str(err)) from err
    print("browser journeys: 2 first-attempt passes")


if __name__ == "__main__":
    main()
