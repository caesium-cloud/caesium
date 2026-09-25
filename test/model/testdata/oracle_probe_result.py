"""Run and classify C3 Go probes, refusing runner failures as oracle evidence."""

import json
import re
import subprocess
import sys


RUNTIME_FAILURE = re.compile(
    r"fatal error:|runtime:|panic:|newosproc|failed to create new OS thread|"
    r"resource temporarily unavailable|cannot allocate memory|out of memory|"
    r"no space left on device|signal:|SIGSEGV|SIGABRT|unexpected fault address|"
    r"stack overflow|fork/exec|error obtaining buildID|test timed out",
    re.IGNORECASE,
)
PACKAGE_SUMMARY = re.compile(
    r"(?:PASS|FAIL|exit status 1)\n|(?:ok|FAIL)\s+\S+\s+\d+(?:\.\d+)?s\n"
)


def validate(output, returncode, mode, expected, marker):
    """Return reasons a run cannot prove the expected candidate/mutant result."""
    issues = []
    tests = {}
    test_outputs = {}
    package_terminal = []
    package_outputs = []

    for number, line in enumerate(output.splitlines(), 1):
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            issues.append(f"non-JSON go test output at line {number}")
            continue
        if not isinstance(event, dict):
            issues.append(f"non-object go test event at line {number}")
            continue
        action = event.get("Action")
        name = event.get("Test")
        if action not in {"start", "run", "output", "pass", "fail", "skip", "pause", "cont"}:
            issues.append(f"unexpected go test action {action!r} at line {number}")
        if action == "output":
            text = event.get("Output", "")
            if not isinstance(text, str):
                issues.append(f"non-text go test output at line {number}")
                continue
            if RUNTIME_FAILURE.search(text):
                issues.append(f"runtime or resource failure at line {number}: {text.strip()}")
            if name:
                test_outputs.setdefault(name, []).append(text)
            else:
                package_outputs.append(text)
        elif action in {"pass", "fail", "skip"}:
            if name:
                tests.setdefault(name, []).append(action)
            else:
                package_terminal.append(action)

    wanted = "pass" if mode == "candidate" else "fail"
    if returncode != (0 if mode == "candidate" else 1):
        issues.append(f"go test exited {returncode}, expected {0 if mode == 'candidate' else 1}")
    if package_terminal != [wanted]:
        issues.append(f"package terminal events {package_terminal}, expected [{wanted}]")
    for name in expected:
        if tests.get(name) != [wanted]:
            issues.append(f"{name} terminal events {tests.get(name)}, expected [{wanted}]")
    unexpected = set(tests) - set(expected)
    if unexpected:
        issues.append(f"unexpected test terminal events: {sorted(unexpected)}")
    if mode == "mutant":
        if len(expected) != 1 or not marker:
            issues.append("mutant validation requires one named test and its assertion marker")
        elif marker not in "".join(test_outputs.get(expected[0], [])):
            issues.append(f"{expected[0]} did not emit its oracle assertion marker")
    for text in package_outputs:
        if not PACKAGE_SUMMARY.fullmatch(text):
            issues.append(f"unexpected package output: {text.strip()!r}")
    return issues


def main():
    checkout, mode, package, selector, expected_csv, label, log, deadline, marker = sys.argv[1:]
    expected = expected_csv.split(",")
    command = ["go", "test", "-json", "-count=1", "-timeout=45s", "-run", selector, package]
    try:
        result = subprocess.run(
            command, cwd=checkout, text=True, stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT, timeout=int(deadline), check=False,
        )
    except subprocess.TimeoutExpired:
        print(f"{label}: checker exceeded {deadline}s; inconclusive, refusing green", file=sys.stderr)
        return 1
    with open(log, "w", encoding="utf-8") as stream:
        stream.write(result.stdout)
    issues = validate(result.stdout, result.returncode, mode, expected, marker)
    if issues:
        print(f"{label}: invalid oracle evidence ({'; '.join(issues)}); log={log}", file=sys.stderr)
        print(result.stdout[-6000:], file=sys.stderr)
        return 1
    print(f"{label}: {'pass' if mode == 'candidate' else 'fail'} {sorted(expected)} (go exit {result.returncode})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
