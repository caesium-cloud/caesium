#!/usr/bin/env python3
"""Evaluate a GitHub Actions `needs` JSON object for the merge gate.

Fails on missing, failed, cancelled, or unexpectedly skipped jobs. A skip
is allowed only when the successful changes job explicitly deselected that
job. Keep these selectors in sync with ci.yml; test_ci.py checks the wiring.
"""

from __future__ import annotations

import json
import os
import sys


# A job runs when any of its selectors is true. No selectors means always.
SELECTORS = {
    "changes": (),
    "ci-config": (),
    "builder": ("images",),
    "builder-arm64": ("images",),
    "images": ("images",),
    "images-arm64": ("images",),
    "reagents": ("go", "reagents", "ci"),
    "reagents-arm64": ("go", "reagents", "ci"),
    "lint": ("go", "reagents", "ci"),
    "unit-test": ("go", "reagents", "ci"),
    "unit-test-arm64": ("go", "ci"),
    "ui-test": ("ui", "go", "ci"),
    "ui-e2e": ("ui", "go", "ci"),
    "ui-e2e-auth": ("ui", "go", "ci"),
    "integration": ("go", "ci"),
}


def main() -> int:
    raw = os.environ.get("NEEDS_JSON", "")
    if not raw:
        print("ci-ok: NEEDS_JSON is empty", file=sys.stderr)
        return 1
    try:
        needs = json.loads(raw)
    except json.JSONDecodeError as err:
        print(f"ci-ok: NEEDS_JSON is not JSON: {err}", file=sys.stderr)
        return 1

    if not isinstance(needs, dict):
        print("ci-ok: NEEDS_JSON must be an object", file=sys.stderr)
        return 1
    changes = needs.get("changes")
    if not isinstance(changes, dict) or changes.get("result") != "success":
        print("ci-ok: changes must succeed", file=sys.stderr)
        return 1
    outputs = changes.get("outputs")
    if not isinstance(outputs, dict) or any(
        outputs.get(key) not in ("true", "false")
        for key in ("go", "ui", "helm", "reagents", "ci", "images")
    ):
        print("ci-ok: missing or invalid change outputs", file=sys.stderr)
        return 1
    any_change = any(outputs[key] == "true" for key in ("go", "ui", "helm", "reagents", "ci"))
    if (outputs["images"] == "true") != any_change:
        print("ci-ok: inconsistent image selection", file=sys.stderr)
        return 1

    required = sys.argv[1:]
    if not required:
        print("ci-ok: no required job names passed", file=sys.stderr)
        return 1

    failed: list[str] = []
    lines: list[str] = []
    for name in required:
        entry = needs.get(name)
        result = entry.get("result", "missing") if isinstance(entry, dict) else "missing"
        lines.append(f"  {name}: {result}")
        selectors = SELECTORS.get(name)
        skip_allowed = bool(selectors) and all(outputs[key] == "false" for key in selectors)
        if selectors is None or (result != "success" and not (result == "skipped" and skip_allowed)):
            failed.append(f"{name}={result}")

    print("ci-ok job results:")
    print("\n".join(lines))
    if failed:
        print("ci-ok failed: " + ", ".join(failed), file=sys.stderr)
        return 1
    print("ci-ok passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
