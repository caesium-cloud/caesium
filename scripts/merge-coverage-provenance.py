#!/usr/bin/env python3
"""Validate original raw CLI/server provenance before coverage conversion."""

import argparse
import json
import re
import sys
from pathlib import Path


MODULE = "github.com/caesium-cloud/caesium"


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--profiles-dir", required=True)
    parser.add_argument("--candidate-sha", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    if not re.fullmatch(r"[0-9a-f]{40}|[0-9a-f]{64}", args.candidate_sha):
        print("candidate SHA is invalid", file=sys.stderr)
        return 1
    originals = {}
    for source in ("cli", "server"):
        path = Path(args.profiles_dir) / f"{source}.provenance.json"
        try:
            prov = json.loads(path.read_text())
        except FileNotFoundError:
            print(f"{source} original provenance is missing; raw files cannot prove identity", file=sys.stderr)
            return 2
        except (OSError, json.JSONDecodeError) as err:
            print(f"{source} original provenance cannot be read: {err}", file=sys.stderr)
            return 1
        if not isinstance(prov, dict) or any((
            prov.get("schema_version") != 1,
            prov.get("source") != source,
            prov.get("kind") != "gocoverdir",
            prov.get("module") != MODULE,
            prov.get("candidate_sha") != args.candidate_sha,
        )):
            print(f"{source} original provenance has foreign/schema identity: {prov}", file=sys.stderr)
            return 1
        if any((
            prov.get("complete") is not True,
            prov.get("missing") is not False,
            prov.get("killed") is not False,
            prov.get("verified") is not True,
            prov.get("image_provenance") != "built-by-this-run",
            str(prov.get("signal") or "").upper() in {"SIGKILL", "KILL", "9"},
            prov.get("oom_killed") is True,
            prov.get("exit_code") not in ((0,) if source == "cli" else (0, 143)),
            source == "server" and prov.get("stop_rc") != 0,
            source == "server" and prov.get("signal") != "SIGTERM",
            source == "server" and prov.get("oom_killed") is not False,
        )):
            print(f"{source} original provenance is incomplete, killed, or unverified: {prov}", file=sys.stderr)
            return 2
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", str(prov.get("image_id") or "")):
            print(f"{source} original image identity is missing or invalid", file=sys.stderr)
            return 1
        originals[source] = prov
    image_ids = {prov["image_id"] for prov in originals.values()}
    if len(image_ids) != 1:
        print("CLI and server raw profiles were collected from different images", file=sys.stderr)
        return 1
    merged = {
        "schema_version": 1,
        "source": "integration",
        "kind": "gocoverdir",
        "module": MODULE,
        "candidate_sha": args.candidate_sha,
        "image_id": image_ids.pop(),
        "image_provenance": "built-by-this-run",
        "verified": True,
        "complete": True,
        "missing": False,
        "killed": False,
        "sources": ["cli", "server"],
        "collection": "covdata-merge",
        "source_provenance": originals,
    }
    Path(args.output).write_text(json.dumps(merged, indent=2) + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
