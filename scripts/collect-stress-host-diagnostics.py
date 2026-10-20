#!/usr/bin/env python3
"""Collect bounded host observations without changing stress qualification."""

import argparse
import datetime
import json
import os
from pathlib import Path
import platform
import re
import selectors
import signal
import subprocess
import time


MAX_BYTES = 65536
MAX_RECORDS = 100
MAX_MESSAGE = 2048
PROBE_SECONDS = 3
UNITS = ("docker.service", "containerd.service", "systemd-oomd.service")
JOURNAL_FIELDS = "MESSAGE,_TRANSPORT,_SYSTEMD_UNIT,__REALTIME_TIMESTAMP"
CID_LINE = re.compile(r"^stress smoke: cid=([0-9a-f]{64})(?:[ \t]|$)")
INTERRUPTED = False


def redact(text):
    text = re.sub(r"(?i)\bBearer\s+[^\s\"'\\,;]+", "Bearer [REDACTED_BEARER]", text)
    return re.sub(r"\bcsk_[A-Za-z0-9_-]+", "[REDACTED_API_KEY]", text)


def capture(command, timeout=PROBE_SECONDS, max_bytes=MAX_BYTES):
    """Own and join an isolated read-only observer group, including descendants."""
    if not 0 < timeout <= PROBE_SECONDS or not 0 < max_bytes <= MAX_BYTES:
        return {"exit_code": None, "error": "invalid_bounds"}, b""
    if INTERRUPTED:
        return {"exit_code": None, "error": "interrupted"}, b""
    try:
        process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                   start_new_session=True)
    except OSError as error:
        return {"exit_code": None, "error": type(error).__name__}, b""
    body = bytearray()
    failure = None
    deadline = time.monotonic() + timeout
    try:
        with selectors.DefaultSelector() as selector:
            selector.register(process.stdout, selectors.EVENT_READ)
            while True:
                if INTERRUPTED:
                    failure = "interrupted"
                    break
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    failure = "timeout"
                    break
                if not selector.select(min(remaining, .05)):
                    continue
                chunk = process.stdout.read1(4096)
                if not chunk:
                    try:
                        process.wait(timeout=max(.001, deadline - time.monotonic()))
                    except subprocess.TimeoutExpired:
                        failure = "timeout"
                    break
                body.extend(chunk)
                if len(body) > max_bytes:
                    failure = "oversize"
                    break
    finally:
        # The leader can exit after a child closes its pipes. Always join the
        # entire owned group; none of these observers executes the workload.
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        try:
            process.wait(timeout=1)
        except subprocess.TimeoutExpired:
            failure = "cleanup_timeout"
        process.stdout.close()
    status = {"exit_code": process.returncode}
    if failure:
        status["error"] = failure
        return status, b""
    if process.returncode != 0:
        status["error"] = "exit_nonzero"
    return status, bytes(body)


def container_ids(smoke_log):
    with Path(smoke_log).open("rb") as stream:
        body = stream.read(MAX_BYTES + 1)
    if len(body) > MAX_BYTES:
        raise ValueError("smoke_log_oversize")
    text = body.decode("utf-8", errors="strict")
    ids = sorted({match.group(1) for line in text.splitlines() if (match := CID_LINE.match(line))})
    if not ids or len(ids) > 16:
        raise ValueError("smoke_identity_unproved")
    return ids


def reduce_journal(body, ids, source, since, until):
    lines = body.decode("utf-8", errors="strict").splitlines()
    if len(lines) > MAX_RECORDS:
        raise ValueError("journal_record_limit")
    records, rejected = [], 0
    for line in lines:
        try:
            record = json.loads(line)
            if not isinstance(record, dict) or not isinstance(record.get("MESSAGE"), str):
                raise ValueError("record")
            message = record["MESSAGE"]
            matched = [cid for cid in ids if re.search(r"(?<![A-Za-z0-9])" + cid + r"(?![A-Za-z0-9])", message)]
            if not matched:
                continue
            stamp = record.get("__REALTIME_TIMESTAMP")
            if not isinstance(stamp, str) or not re.fullmatch(r"[0-9]{1,20}", stamp):
                raise ValueError("timestamp")
            if not since * 1_000_000 <= int(stamp) <= int(until * 1_000_000):
                raise ValueError("timestamp_window")
            unit = record.get("_SYSTEMD_UNIT")
            if source == "kernel":
                if record.get("_TRANSPORT") != "kernel":
                    raise ValueError("transport")
            elif unit not in UNITS:
                raise ValueError("unit")
            # Refuse oversized messages rather than retaining a misleading
            # prefix that can omit a qualification or secret delimiter.
            if len(message.encode("utf-8")) > MAX_MESSAGE:
                raise ValueError("message_size")
            finding = "unknown"
            if source == "kernel" and re.search(r"\bconstraint=CONSTRAINT_MEMCG\b", message):
                finding = "memcg_oom"
            elif unit == "systemd-oomd.service" and message.startswith("Killed "):
                finding = "oomd_kill"
            records.append({"container_ids": matched, "timestamp_us": int(stamp),
                            "source": source, "unit": unit if source != "kernel" else None,
                            "message": redact(message), "finding": finding})
        except (ValueError, TypeError):
            rejected += 1
    return {"records": records, "rejected_records": rejected}


def collect(smoke_log, since, now=None):
    report = {
        "schema_version": 1, "kind": "stress-host-observation", "qualification": "unchanged",
        "candidate_sha": redact(os.environ.get("GITHUB_SHA", "")[:128]),
        "run_id": redact(os.environ.get("GITHUB_RUN_ID", "")[:128]),
        "run_attempt": redact(os.environ.get("GITHUB_RUN_ATTEMPT", "")[:128]),
        "observer_uid": os.geteuid(),
        "classification": "unknown", "container_ids": [], "probes": {}, "records": [],
        "host": {"hostname": platform.node(), "system": platform.system(),
                 "architecture": platform.machine(), "kernel": platform.release()},
    }
    # The observer must own every probe process, including journal readers.
    # Nested sudo would let an unprivileged parent lose permission to kill its
    # root descendants when a bounded read times out.
    if report["observer_uid"] != 0:
        report["availability"] = "unavailable"
        report["error"] = "observer_requires_root"
        return report
    until = time.time() if now is None else now
    if not re.fullmatch(r"[0-9]{1,12}", str(since)) or not 0 <= until - int(since) <= 120:
        report["error"] = "time_window_unproved"
        return report
    since = int(since)
    try:
        ids = container_ids(smoke_log)
    except UnicodeError:
        report["error"] = "invalid_smoke_encoding"
        return report
    except (OSError, ValueError) as error:
        report["error"] = str(error) if isinstance(error, ValueError) else type(error).__name__
        return report
    report["container_ids"] = ids
    # journalctl filters MESSAGE before JSON serialization and our byte bound.
    # Only validated literal CIDs enter this PCRE2 pattern. Keep its token
    # boundaries aligned with the independent reducer and disable smart case.
    message_pattern = r"(?<![A-Za-z0-9])(?:" + "|".join(re.escape(cid) for cid in ids) + r")(?![A-Za-z0-9])"
    until_text = datetime.datetime.fromtimestamp(until, datetime.timezone.utc).strftime("%Y-%m-%d %H:%M:%S.%f UTC")
    report["window"] = {"since_seconds": since, "until": until_text}
    for name, options in (("kernel", ["-k"]),
                          ("units", [argument for unit in UNITS for argument in ("-u", unit)])):
        command = ["journalctl", "--no-pager", "--output=json",
                   "--output-fields=" + JOURNAL_FIELDS,
                   "--grep", message_pattern, "--case-sensitive=yes",
                   "--since", "@" + str(since), "--until", until_text, *options]
        status, body = capture(command)
        report["probes"][name] = status
        if status.get("error"):
            continue
        try:
            reduced = reduce_journal(body, ids, name, since, until)
            report["records"].extend(reduced["records"])
            status["rejected_records"] = reduced["rejected_records"]
        except (UnicodeError, ValueError):
            status["error"] = "invalid_journal"
    status, body = capture(["docker", "info", "--format", "{{json .}}"])
    report["probes"]["daemon_identity"] = status
    if not status.get("error"):
        try:
            info = json.loads(body)
            if not isinstance(info, dict):
                raise ValueError("daemon_identity")
            report["daemon"] = {key: redact(value) for key in (
                "ID", "Name", "Architecture", "KernelVersion", "OperatingSystem", "ServerVersion", "CgroupDriver", "CgroupVersion")
                if isinstance(value := info.get(key), str) and len(value) <= 128}
            for component in ("ContainerdCommit", "RuncCommit"):
                commit = info.get(component)
                if isinstance(commit, dict) and isinstance(commit.get("ID"), str) and len(commit["ID"]) <= 128:
                    report["daemon"][component] = {"ID": redact(commit["ID"])}
            security = info.get("SecurityOptions")
            if isinstance(security, list) and len(security) <= 16:
                report["daemon"]["SecurityOptions"] = [redact(option) for option in security
                                                       if isinstance(option, str) and len(option) <= 256]
        except (UnicodeError, ValueError, TypeError):
            status["error"] = "invalid_daemon_identity"
    status, body = capture(["systemctl", "is-active", "systemd-oomd.service"])
    report["probes"]["oomd_active"] = status
    active = body.decode("utf-8", errors="replace").strip()
    if active in ("active", "inactive", "failed", "activating", "deactivating", "unknown"):
        report["oomd_active_state"] = active
    findings = sorted({record["finding"] for record in report["records"] if record["finding"] != "unknown"})
    if findings:
        report["classification"] = "+".join(findings)
    return report


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--smoke-log", required=True)
    parser.add_argument("--since", required=True)
    args = parser.parse_args()
    def interrupted(_number, _frame):
        global INTERRUPTED
        INTERRUPTED = True
    for number in (signal.SIGTERM, signal.SIGHUP, signal.SIGINT):
        signal.signal(number, interrupted)
    print(json.dumps(collect(args.smoke_log, args.since), separators=(",", ":")))


if __name__ == "__main__":
    main()
