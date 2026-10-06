#!/usr/bin/env python3
"""Reduce optional smoke observations; never decide workload qualification."""
import argparse
import datetime
import json
import os
import re
import selectors
import signal
import subprocess
import time
from pathlib import Path

HASH = re.compile(r"[0-9a-f]{64}")
IMAGE = re.compile(r"(?:sha256:)?[0-9a-f]{64}")
MAX_BYTES = 65536
MAX_EVENTS = 100
QUERY_SECONDS = 3
INTERRUPTED = False


def unavailable(reason):
    return {"outcome": "unavailable", "reason": reason}


def state(text, cid, image):
    fields = text.split("|")
    if len(fields) != 8 or fields[:2] != [cid, image]:
        raise ValueError("identity")
    _, _, status, running, code, oom, memory, swap = fields
    if status not in {"created", "running", "paused", "restarting", "removing", "exited", "dead"}:
        raise ValueError("state")
    if running not in {"true", "false"} or oom not in {"true", "false"}:
        raise ValueError("state")
    if not re.fullmatch(r"[0-9]{1,3}", code) or int(code) > 255:
        raise ValueError("state")
    if any(not re.fullmatch(r"-?[0-9]{1,20}", x) or not -(2**63) <= int(x) < 2**63 for x in (memory, swap)):
        raise ValueError("state")
    return {"status": status, "running": running == "true", "exit": int(code),
            "oom": oom == "true", "memory": int(memory), "swap": int(swap)}


def read_bounded(path):
    with Path(path).open("rb") as stream:
        body = stream.read(MAX_BYTES + 1)
    if len(body) > MAX_BYTES:
        raise ValueError("size")
    return body.decode("ascii")


def journal(text, cid, image):
    lines = text.splitlines()
    if not lines or len(lines) > 100:
        raise ValueError("journal")
    transitions = []
    for poll, line in enumerate(lines):
        number, value = line.split("|", 1)
        if number != str(poll):
            raise ValueError("journal")
        reduced = state(value, cid, image)
        if transitions and transitions[-1]["state"] == reduced:
            transitions[-1]["last_poll"] = poll
        else:
            transitions.append({"first_poll": poll, "last_poll": poll, "state": reduced})
    return {"outcome": "complete", "polls": len(lines), "transitions": transitions}


def events(body, cid, window=None):
    lines = body.splitlines()
    if len(body) > MAX_BYTES or len(lines) > MAX_EVENTS:
        raise ValueError("size")
    records = []
    for line in lines:
        if len(line) > 4096:
            raise ValueError("size")
        value = json.loads(line)
        if not isinstance(value, dict) or value.get("Type") != "container":
            raise ValueError("event")
        actor = value.get("Actor")
        if not isinstance(actor, dict) or actor.get("ID") != cid or value.get("id", cid) != cid:
            raise ValueError("identity")
        action, stamp = value.get("Action"), value.get("timeNano")
        if action not in {"oom", "die", "kill"} or type(stamp) is not int or not 0 <= stamp < 2**63:
            raise ValueError("event")
        if window is not None and not window[0] <= stamp <= window[1]:
            raise ValueError("window")
        record = {"action": action, "time_nano": stamp}
        attributes = actor.get("Attributes", {})
        if not isinstance(attributes, dict):
            raise ValueError("event")
        if "exitCode" in attributes:
            code = attributes["exitCode"]
            if not isinstance(code, str) or not re.fullmatch(r"[0-9]{1,3}", code) or int(code) > 255:
                raise ValueError("event")
            record["exit"] = int(code)
        if "signal" in attributes:
            signal = attributes["signal"]
            if signal not in {"1", "2", "9", "15", "SIGHUP", "SIGINT", "SIGKILL", "SIGTERM"}:
                raise ValueError("event")
            record["signal"] = signal
        records.append(record)
    return {"outcome": "complete", "records": records}


def capture(argv):
    """Cap stdout/time and join the read-only child; never retain its stderr."""
    try:
        process = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                   start_new_session=True)
    except OSError:
        return None, "command-unavailable"
    body = bytearray()
    failure = None
    deadline = time.monotonic() + QUERY_SECONDS
    with selectors.DefaultSelector() as selector:
        selector.register(process.stdout, selectors.EVENT_READ)
        try:
            while True:
                if INTERRUPTED:
                    failure = "interrupted"
                    break
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    failure = "deadline"
                    break
                if not selector.select(min(remaining, .05)):
                    continue
                chunk = process.stdout.read1(4096)
                if not chunk:
                    break
                body.extend(chunk)
                if len(body) > MAX_BYTES:
                    failure = "oversized"
                    break
            if failure is None:
                try:
                    code = process.wait(timeout=max(0.001, deadline - time.monotonic()))
                    if code != 0:
                        failure = "command-failed"
                except subprocess.TimeoutExpired:
                    failure = "deadline"
        finally:
            # A successful leader can exit while a same-group descendant has
            # closed its pipes and remains alive. Always terminate the owned
            # observer group; these processes never execute the fixture.
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait()
            process.stdout.close()
    return (None, failure) if failure else (bytes(body), None)


def diagnose(args):
    result = {"schema_version": 1, "kind": "stress-native-observation",
              "qualification": "unchanged", "journal": unavailable("not-captured"),
              "events": unavailable("identity-not-proved"),
              "cgroup": unavailable("shared-host-identity-not-proved")}
    if not HASH.fullmatch(args.cid) or not IMAGE.fullmatch(args.image):
        return result
    try:
        snapshot = state(read_bounded(args.snapshot).strip(), args.cid, args.image)
    except (OSError, UnicodeError, ValueError):
        return result
    result.update(cid=args.cid, image=args.image, snapshot=snapshot)
    try:
        result["journal"] = journal(read_bounded(args.journal), args.cid, args.image)
    except (OSError, UnicodeError, ValueError):
        result["journal"] = unavailable("invalid-or-missing")
    now = datetime.datetime.now(datetime.timezone.utc)
    until_nano = int(now.timestamp()) * 10**9 + now.microsecond * 1000
    if not re.fullmatch(r"[0-9]{1,12}", args.since) or not 0 <= now.timestamp() - int(args.since) <= 120:
        result["events"] = unavailable("window-not-proved")
        return result
    argv = [args.runtime, "events", "--since", args.since, "--until", now.isoformat(),
            "--filter", "container=" + args.cid, "--filter", "event=oom",
            "--filter", "event=die", "--filter", "event=kill", "--format", "{{json .}}"]
    result["event_window"] = {"since_seconds": int(args.since), "until_nano": until_nano}
    body, failure = capture(argv)
    if failure:
        result["events"] = unavailable(failure)
    else:
        try:
            result["events"] = events(body, args.cid, (int(args.since) * 10**9, until_nano))
        except (UnicodeError, ValueError, TypeError):
            result["events"] = unavailable("invalid-or-unbound")
    return result


def main():
    parser = argparse.ArgumentParser()
    for flag in ("runtime", "cid", "image", "snapshot", "journal", "since"):
        parser.add_argument("--" + flag, required=True)
    args = parser.parse_args()
    def interrupted(_number, _frame):
        # Do not throw while Popen is transferring child ownership. The bounded
        # reader observes the flag and joins its isolated process group first.
        global INTERRUPTED
        INTERRUPTED = True
    for number in (signal.SIGTERM, signal.SIGHUP, signal.SIGINT):
        signal.signal(number, interrupted)
    try:
        value = diagnose(args)
    except Exception:
        value = unavailable("observer-failed")
    if INTERRUPTED:
        value = unavailable("observer-interrupted")
    print(json.dumps(value, separators=(",", ":")))


if __name__ == "__main__":
    main()
