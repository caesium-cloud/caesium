#!/usr/bin/env python3
"""Restart only the fixture invocation's immutable owned server on request."""
import json
from pathlib import Path
import re
import subprocess
import sys
import time


def main():
    cli, server_id, directory = sys.argv[1:]
    if not re.fullmatch(r"[0-9a-f]{64}", server_id):
        raise SystemExit("fixture control requires an immutable owned container ID")
    control = Path(directory)
    seen = set()
    while True:
        try:
            request = json.loads((control / "request.json").read_text())
        except (OSError, ValueError):
            time.sleep(0.05)
            continue
        identity = request.get("id")
        if identity in seen:
            time.sleep(0.05)
            continue
        if not isinstance(identity, str) or not re.fullmatch(r"[0-9a-f-]{36}", identity):
            raise SystemExit("invalid fixture control request identity")
        seen.add(identity)
        response = {"id": identity, "action": request.get("action"), "status": "error"}
        if set(request) == {"id", "action"} and request["action"] == "restart":
            try:
                def started():
                    return subprocess.check_output(
                        [cli, "container", "inspect", "--format", "{{.State.StartedAt}}", server_id],
                        text=True, stderr=subprocess.DEVNULL, timeout=10).strip()
                response["started_before"] = started()
                subprocess.run([cli, "restart", server_id], check=True, stdout=subprocess.DEVNULL,
                               stderr=subprocess.DEVNULL, timeout=60)
                response["started_after"] = started()
                if response["started_before"] and response["started_before"] != response["started_after"]:
                    response["status"] = "ok"
            except (subprocess.SubprocessError, OSError):
                pass
        # No engine command output, environment value or arbitrary error text
        # enters the evidence. The test requires its exact request id and a new
        # observed process start time before it can continue.
        (control / "response.tmp").write_text(json.dumps(response))
        (control / "response.tmp").chmod(0o600)
        (control / "response.tmp").replace(control / "response.json")
        with (control / "events.jsonl").open("a") as output:
            output.write(json.dumps(response) + "\n")


if __name__ == "__main__":
    main()
