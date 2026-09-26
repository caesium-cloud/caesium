#!/usr/bin/env python3
"""Record a blocked F2 snapshot write with current, invocation-bound evidence."""

import json
import os
from pathlib import Path


CAPTURE_FILES = {
    "pods": "pods.json",
    "events": "events.txt",
    "caesium-0-current-log": "caesium-0.log",
    "caesium-1-current-log": "caesium-1.log",
    "caesium-0-previous-log": "caesium-0.previous.log",
    "caesium-1-previous-log": "caesium-1.previous.log",
    "runner-artifact-copy": "artifact-copy.log",
}


def build_manifest(art: Path, lifecycle_id: str, batch: int, capture_exit_codes: dict) -> dict:
    if not lifecycle_id or not 0 <= batch <= 18:
        raise ValueError("snapshot diagnostic requires a lifecycle id and batch 0..18")
    label = f"{batch:02d}"
    stem = f"snapshot-failure-batch-{label}"
    disputed_path = art / f"cluster-snapshot-disputed-batch-{label}.json"
    record = {
        "lifecycle_id": lifecycle_id,
        "batch": batch,
        "phase_log": (f"cluster-logs/GenerateSnapshotUpdateBatch-{batch}.log" if batch
                      else "cluster-logs/GenerateSnapshotWrites.log"),
        "capture_exit_codes": capture_exit_codes,
        "artifacts": {
            key: f"cluster-logs/{stem}-{suffix}" for key, suffix in CAPTURE_FILES.items()
        },
    }
    if batch == 0:
        record["disputed_write_readback"] = {
            "status": "unavailable",
            "detail": "no disputed annotation readback for distinct-write phase",
        }
        return record
    try:
        disputed = json.loads(disputed_path.read_text())
        write_index = disputed.get("write_index")
        if disputed.get("lifecycle_id") != lifecycle_id or disputed.get("batch") != batch:
            raise ValueError("disputed write belongs to another lifecycle invocation or batch")
        if type(write_index) is not int or not 1 <= write_index <= 500:
            raise ValueError("disputed write index is missing or invalid")
        expected = f"{(batch - 1) * 500 + write_index:06d}"
        if (disputed.get("attempted_annotation") != expected
                or not isinstance(disputed.get("apply_error"), str) or not disputed["apply_error"]):
            raise ValueError("disputed annotation or Apply failure is missing")
        job_id = disputed.get("job_id")
        if not isinstance(job_id, str) or not job_id:
            raise ValueError("disputed catalog job identity is missing")
        readbacks = disputed.get("readbacks")
        if not isinstance(readbacks, list) or {row.get("pod") for row in readbacks} != {
            "caesium-0", "caesium-1"
        } or len(readbacks) != 2:
            raise ValueError("readback must report both surviving pods")
        for row in readbacks:
            if not isinstance(row.get("error", ""), str):
                raise ValueError("readback error must be text")
            if row.get("error"):
                if "matches_attempted" not in row or row["matches_attempted"] is not None:
                    raise ValueError("failed readback must keep matches_attempted unknown")
                continue
            if type(row.get("http_status")) is not int:
                raise ValueError("readback has neither an HTTP status nor an error")
            if row["http_status"] == 200 and (
                not isinstance(row.get("job_id"), str)
                or not isinstance(row.get("annotation"), str)
            ):
                raise ValueError("successful readback lacks the catalog job and annotation")
            matches = (row["http_status"] == 200 and row.get("job_id") == job_id
                       and row.get("annotation") == expected)
            if type(row.get("matches_attempted")) is not bool or row["matches_attempted"] != matches:
                raise ValueError("readback matches_attempted contradicts its status, job or annotation")
        record["disputed_write_readback"] = {
            "status": "captured",
            "artifact": disputed_path.name,
            "observation": disputed,
        }
    except (OSError, ValueError, TypeError, AttributeError) as error:
        record["disputed_write_readback"] = {
            "status": "unavailable",
            "detail": str(error),
        }
    return record


def main() -> None:
    art = Path(os.environ["LC_ART"])
    batch = int(os.environ["LC_SNAP_BATCH"])
    codes = {
        "pods": int(os.environ["LC_DIAG_PODS_RC"]),
        "events": int(os.environ["LC_DIAG_EVENTS_RC"]),
        "caesium-0-current-log": int(os.environ["LC_DIAG_LOG0_RC"]),
        "caesium-1-current-log": int(os.environ["LC_DIAG_LOG1_RC"]),
        "caesium-0-previous-log": int(os.environ["LC_DIAG_PREV_LOG0_RC"]),
        "caesium-1-previous-log": int(os.environ["LC_DIAG_PREV_LOG1_RC"]),
        "runner-artifact-copy": int(os.environ["LC_DIAG_COPY_RC"]),
    }
    record = build_manifest(art, os.environ["LC_ID"], batch, codes)
    path = art / "cluster-logs" / f"snapshot-failure-batch-{batch:02d}.json"
    path.write_text(json.dumps(record, indent=2) + "\n")


if __name__ == "__main__":
    main()
