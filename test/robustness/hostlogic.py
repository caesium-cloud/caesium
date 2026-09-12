#!/usr/bin/env python3
"""Host-side helpers for scripts/robustness.sh.

Commands read listings from stdin unless noted. Exit 0 means success / dead /
valid; exit 1 means not-dead, invalid, or mismatch.
"""

from __future__ import annotations

import io
import json
import os
import re
import sys
import tempfile
from contextlib import redirect_stdout

SHA_RE = re.compile(r"sha256:([0-9a-fA-F]{64})")
LISTING_ERROR_MARKERS = (
    "failed to dial",
    "connection refused",
    "cannot connect",
    "no such file or directory",
    "permission denied",
    "i/o timeout",
    "deadline exceeded",
    "rpc error",
    "unavailable",
    "transport is closing",
    "error response from daemon",
)


def _header_tokens(line: str) -> list[str]:
    return [tok.lower() for tok in re.split(r"\s+", line.strip()) if tok]


def listing_kind(text: str) -> str:
    raw = text or ""
    stripped = raw.strip()
    if not stripped:
        return "invalid"
    lines = [ln for ln in raw.splitlines() if ln.strip()]
    header_ok = False
    if lines:
        toks = _header_tokens(lines[0])
        header_ok = "task" in toks and ("pid" in toks or "status" in toks)
    lower = stripped.lower()
    if not header_ok:
        if lower.startswith("ctr:") or "error:" in lower[:120]:
            return "error"
        if any(m in lower for m in LISTING_ERROR_MARKERS):
            return "error"
        return "invalid"
    return "ok"


def listing_valid(text: str) -> bool:
    return listing_kind(text) == "ok"


def _cid_needles(cid: str) -> tuple[str, str]:
    cid = (cid or "").strip()
    short = cid[:12] if len(cid) >= 12 else cid
    return cid, short


def task_dead(cid: str, listing: str, seen: bool) -> tuple[bool, str]:
    cid, short = _cid_needles(cid)
    if not cid or len(short) < 8:
        return False, "empty container id"
    kind = listing_kind(listing)
    if kind != "ok":
        return False, f"listing is {kind}, not proof of death"
    matched = False
    for line in listing.splitlines():
        if cid not in line and short not in line:
            continue
        matched = True
        l = line.lower()
        if "running" in l:
            return False, "container still running"
        if any(s in l for s in ("stopped", "exited", "killed")):
            return True, "container stopped"
        return False, "container still present without stopped evidence"
    if matched:
        return False, "container still present without stopped evidence"
    if seen:
        return True, "container absent from valid listing"
    return False, "container id never appeared in ctr tasks list"


def extract_shas(*texts: str) -> list[str]:
    seen: set[str] = set()
    out: list[str] = []
    for text in texts:
        if not text:
            continue
        for m in SHA_RE.finditer(text):
            ident = "sha256:" + m.group(1).lower()
            if ident not in seen:
                seen.add(ident)
                out.append(ident)
    return out


def identities_from_inspect(data) -> list[str]:
    if isinstance(data, dict):
        items = [data]
    elif isinstance(data, list):
        items = data
    else:
        return []
    texts: list[str] = []
    for img in items:
        if not isinstance(img, dict):
            continue
        texts.append(str(img.get("Id") or ""))
        for rd in img.get("RepoDigests") or []:
            texts.append(str(rd))
    return extract_shas(*texts)


def identities_match(expected: list[str], observed: str) -> tuple[bool, str]:
    want = set(extract_shas(*expected))
    got = extract_shas(observed)
    if not want:
        return False, "no expected sha256 identities"
    if not got:
        return False, f"observed image id has no sha256: {observed!r}"
    if set(got) & want:
        return True, "ok"
    return False, f"running {sorted(got)} not in expected {sorted(want)}"


def records_ok(cm: dict) -> tuple[bool, str]:
    data = cm.get("data") or {}
    if not isinstance(data, dict) or not data:
        return False, "robustness-records ConfigMap has no data"
    required = ("events", "owner_is_leader", "owner_is_not_leader")
    parsed: dict[str, object] = {}
    for key in required:
        raw = data.get(key)
        if not raw:
            b64 = data.get(key + "_b64")
            if b64:
                import base64

                try:
                    raw = base64.b64decode(b64).decode("utf-8")
                except Exception as exc:
                    return False, f"{key}_b64 is not readable: {exc}"
        if not raw:
            return False, f"missing recorder key {key}"
        try:
            parsed[key] = json.loads(raw)
        except Exception as exc:
            return False, f"{key} is not JSON: {exc}"
    events = parsed["events"]
    if not isinstance(events, list) or not events:
        return False, "events is not a non-empty JSON array"
    for key in ("owner_is_leader", "owner_is_not_leader"):
        obj = parsed[key]
        if not isinstance(obj, dict) or not str(obj.get("run_id") or "").strip():
            return False, f"{key} is missing run_id"
    return True, "ok"


def _read_stdin() -> str:
    return sys.stdin.read()


def _cmd_task_dead(argv: list[str]) -> int:
    if len(argv) < 2:
        print("usage: task-dead <cid> <seen>", file=sys.stderr)
        return 2
    cid, seen_s = argv[0], argv[1]
    dead, reason = task_dead(cid, _read_stdin(), seen_s == "1")
    if not dead:
        print(reason, file=sys.stderr)
        return 1
    return 0


def _cmd_listing_valid(_: list[str]) -> int:
    return 0 if listing_valid(_read_stdin()) else 1


def _cmd_collect_identities(argv: list[str]) -> int:
    texts: list[str] = []
    expected: list[str] = []
    read_stdin = False
    for arg in argv:
        if arg == "-":
            read_stdin = True
            continue
        if os.path.isfile(arg):
            with open(arg, encoding="utf-8") as fh:
                raw = fh.read()
            raw_s = raw.strip()
            if raw_s.startswith("{") or raw_s.startswith("["):
                try:
                    expected.extend(identities_from_inspect(json.loads(raw)))
                    continue
                except json.JSONDecodeError:
                    pass
            texts.append(raw)
            continue
        texts.append(arg)
    extra = _read_stdin() if read_stdin else ""
    expected.extend(extract_shas(*texts, extra))
    if not expected:
        print("no sha256 identities collected", file=sys.stderr)
        return 1
    sys.stdout.write("\n".join(expected) + "\n")
    return 0


def _cmd_image_match(argv: list[str]) -> int:
    if len(argv) < 2:
        print("usage: image-match <expected-file> <observed-image-id>", file=sys.stderr)
        return 2
    with open(argv[0], encoding="utf-8") as fh:
        expected = [ln.strip() for ln in fh if ln.strip()]
    ok, reason = identities_match(expected, argv[1])
    if not ok:
        print(reason, file=sys.stderr)
        return 1
    return 0


def _cmd_ctr_image_shas(argv: list[str]) -> int:
    if not argv:
        print("usage: ctr-image-shas <needle>", file=sys.stderr)
        return 2
    needle = argv[0]
    texts = [ln for ln in _read_stdin().splitlines() if needle in ln]
    shas = extract_shas(*texts)
    if not shas:
        print("no sha256 digest for " + needle, file=sys.stderr)
        return 1
    sys.stdout.write("\n".join(shas) + "\n")
    return 0


def _cmd_records_ok(argv: list[str]) -> int:
    if not argv:
        print("usage: records-ok <configmap.json>", file=sys.stderr)
        return 2
    with open(argv[0], encoding="utf-8") as fh:
        cm = json.load(fh)
    ok, reason = records_ok(cm)
    if not ok:
        print(reason, file=sys.stderr)
        return 1
    return 0


def _self_test() -> int:
    failures: list[str] = []

    def check(name: str, cond: bool) -> None:
        if not cond:
            failures.append(name)

    listing = "TASK                                PID      STATUS\n"
    running = listing + "abcdef1234567890deadbeefcafebabe  1  RUNNING\n"
    stopped = listing + "abcdef1234567890deadbeefcafebabe  1  STOPPED\n"
    cid = "abcdef1234567890deadbeefcafebabe"
    check("valid header", listing_valid(listing))
    check("empty invalid", not listing_valid(""))
    check(
        "dial error is not valid",
        not listing_valid("ctr: failed to dial containerd: connection refused"),
    )
    dead, _ = task_dead(cid, "ctr: failed to dial containerd: connection refused", True)
    check("dial error is not death", not dead)
    dead, _ = task_dead(cid, running, True)
    check("running is not death", not dead)
    dead, _ = task_dead(cid, stopped, True)
    check("stopped is death", dead)
    dead, _ = task_dead(cid, listing, True)
    check("absent from valid listing is death", dead)
    dead, _ = task_dead(cid, listing, False)
    check("absent without seen is not death", not dead)

    inspect = {
        "Id": "sha256:" + ("a" * 64),
        "RepoDigests": ["caesiumcloud/caesium@sha256:" + ("b" * 64)],
    }
    ids = identities_from_inspect(inspect)
    check("inspect config id", ("sha256:" + ("a" * 64)) in ids)
    check("inspect manifest id", ("sha256:" + ("b" * 64)) in ids)
    ok, _ = identities_match(ids, "containerd://sha256:" + ("a" * 64))
    check("running config maps", ok)
    ok, _ = identities_match(ids, "docker-pullable://x@sha256:" + ("b" * 64))
    check("running manifest maps", ok)
    ok, _ = identities_match(ids, "sha256:" + ("c" * 64))
    check("unknown running fails", not ok)

    cm = {
        "data": {
            "events": json.dumps([{"kind": "probe"}]),
            "owner_is_leader": json.dumps({"run_id": "r1"}),
            "owner_is_not_leader": json.dumps({"run_id": "r2"}),
        }
    }
    ok, _ = records_ok(cm)
    check("records ok", ok)
    ok, _ = records_ok({"data": {}})
    check("empty records fail", not ok)

    inspect_doc = {
        "Id": "sha256:" + ("a" * 64),
        "RepoDigests": ["caesiumcloud/caesium@sha256:" + ("b" * 64)],
    }
    with tempfile.TemporaryDirectory() as td:
        inspect_path = os.path.join(td, "inspect.json")
        extra_path = os.path.join(td, "extra.txt")
        with open(inspect_path, "w", encoding="utf-8") as fh:
            json.dump(inspect_doc, fh)
        with open(extra_path, "w", encoding="utf-8") as fh:
            fh.write("sha256:" + ("b" * 64) + "\n")
        buf = io.StringIO()
        with redirect_stdout(buf):
            rc = main(["collect-identities", inspect_path, extra_path])
        out = buf.getvalue()
        check("collect-identities rc", rc == 0)
        check("collect-identities config", ("sha256:" + ("a" * 64)) in out)
        check("collect-identities manifest", ("sha256:" + ("b" * 64)) in out)

    if failures:
        print("self-test failed: " + ", ".join(failures), file=sys.stderr)
        return 1
    print("hostlogic self-test ok")
    return 0


def main(argv: list[str]) -> int:
    if not argv:
        print(
            "usage: hostlogic.py task-dead|listing-valid|collect-identities|"
            "image-match|ctr-image-shas|records-ok|self-test",
            file=sys.stderr,
        )
        return 2
    cmd, rest = argv[0], argv[1:]
    if cmd == "task-dead":
        return _cmd_task_dead(rest)
    if cmd == "listing-valid":
        return _cmd_listing_valid(rest)
    if cmd == "collect-identities":
        return _cmd_collect_identities(rest)
    if cmd == "image-match":
        return _cmd_image_match(rest)
    if cmd == "ctr-image-shas":
        return _cmd_ctr_image_shas(rest)
    if cmd == "records-ok":
        return _cmd_records_ok(rest)
    if cmd == "self-test":
        return _self_test()
    print("unknown command: " + cmd, file=sys.stderr)
    return 2


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
