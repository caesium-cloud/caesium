#!/usr/bin/env python3
"""Retain same-attempt Helm/port-forward evidence, including setup failures."""

import argparse
import json
import os
from pathlib import Path
import re
import subprocess


def redact(value):
    value = re.sub(r"\bcsk_[A-Za-z0-9_-]+", "[REDACTED_API_KEY]", value)
    return re.sub(r"(?i)(Bearer\s+)[^\s\"']+", r"\1[REDACTED_BEARER]", value)


def collect(source, output):
    output.mkdir(parents=True, exist_ok=True)
    report = {
        "candidate_sha": os.environ.get("GITHUB_SHA", ""),
        "run_id": os.environ.get("GITHUB_RUN_ID", ""),
        "run_attempt": os.environ.get("GITHUB_RUN_ATTEMPT", ""),
        "shard": os.environ.get("HELM_SHARD", ""),
        "port_forward": {}, "commands": {},
    }
    for phase in ("full", "cache-identity", "deadlines"):
        name = f"port-forward-{phase}.log"
        path = source / name
        if path.is_file():
            (output / name).write_text(redact(path.read_text(errors="replace")))
            report["port_forward"][phase] = "retained"
        else:
            report["port_forward"][phase] = "not_created"

    commands = {
        "server.log": ["kubectl", "logs", "statefulset/caesium", "-c", "caesium"],
        "pods.log": ["kubectl", "get", "pods", "-A", "-o", "wide"],
        "describe.log": ["kubectl", "describe", "pods", "-l", "app.kubernetes.io/instance=caesium"],
        "events.log": ["kubectl", "get", "events", "-A", "--sort-by=.metadata.creationTimestamp"],
    }
    for name, command in commands.items():
        try:
            result = subprocess.run(command, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                    text=True, timeout=30, check=False)
            text, status = result.stdout, {"exit_code": result.returncode}
        except subprocess.TimeoutExpired as exc:
            text = exc.stdout or b""
            if isinstance(text, bytes):
                text = text.decode(errors="replace")
            text += "\nDiagnostic command timed out after 30 seconds\n"
            status = {"error": "timeout"}
        except OSError as exc:
            text, status = str(exc), {"error": type(exc).__name__}
        (output / name).write_text(redact(text))
        report["commands"][name] = status
    (output / "report.json").write_text(json.dumps(report, indent=2) + "\n")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, default=Path("/tmp"))
    parser.add_argument("--output", type=Path, default=Path(".tmp/helm-integration-diagnostics"))
    args = parser.parse_args()
    collect(args.source, args.output)
