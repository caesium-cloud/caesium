#!/usr/bin/env python3
"""Project engine inspection onto identity/state fields safe for fixture evidence."""
import json
import re
import sys
import subprocess


def fields(record, names):
    return {name: record[name] for name in names if name in record}


def project(record, kind):
    if kind == "container":
        result = fields(record, ("Id", "Name", "Image", "Created"))
        result["State"] = fields(record.get("State", {}), (
            "Status", "Running", "Paused", "Restarting", "OOMKilled", "Dead",
            "Pid", "ExitCode", "StartedAt", "FinishedAt",
        ))
        result["HostConfig"] = fields(record.get("HostConfig", {}), ("NetworkMode",))
    else:
        result = fields(record, ("Id", "Created", "RepoTags", "RepoDigests", "Architecture", "Os", "Variant", "Size"))
    config = record.get("Config") or {}
    # Even innocently named variables can contain secret://env targets, DSNs
    # or credentials. No values, argv, labels, error text or health logs survive.
    names = {entry.split("=", 1)[0] for entry in config.get("Env", [])
             if isinstance(entry, str) and "=" in entry
             and re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", entry.split("=", 1)[0])}
    result["Config"] = {"EnvNames": sorted(names)}
    return result


def main():
    kind = sys.argv[1]
    if kind not in ("container", "image"):
        raise ValueError("expected container or image")
    command = [sys.argv[2], *(["container", "inspect"] if kind == "container" else ["image", "inspect"]), *sys.argv[3:]]
    raw = subprocess.run(command, check=True, stdout=subprocess.PIPE, text=True).stdout
    records = json.loads(raw)
    if not isinstance(records, list) or not records or not all(isinstance(r, dict) for r in records):
        raise ValueError("expected nonempty inspection records")
    json.dump([project(record, kind) for record in records], sys.stdout, indent=2)


if __name__ == "__main__":
    main()
