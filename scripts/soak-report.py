#!/usr/bin/env python3
"""Host-side logic for scripts/soak-tests.sh (distributed-testing F3).

Everything the controller decides lives here so it is hermetically testable
(scripts/test_soak_report.py): parsing the runner's host requests, storing the
records the runner hands back, appending the retained ACTUAL fault schedule,
parsing per-pod resource samples and container inventories, and folding it
all into soak.json.

soak.json starts as ``result: incomplete``. A family with no record is
``blocked``; missing samples, a setup failure or an unobserved fault are
blocked or inconclusive, never a pass. A process kill is recorded as a process
kill; nothing here is power-loss qualification.
"""

from __future__ import annotations

import argparse
import datetime as _dt
import json
import os
import re
import secrets
import shlex
import sys
from pathlib import Path

FAMILIES = [
    "slow_consumers",
    "queue_overload",
    "retention",
    "repeated_failover",
    "node_replacement",
]
POST_DRAIN_CHECKS = [
    "whole_schedule_safety",
    "post_drain_resources",
    "post_drain_fds",
    "post_drain_containers",
]
# A nonzero phase makes the run blocked unless a scenario FAILED, with one
# exception: failing to delete the owned cluster is a failure of this run.
# Setup phases mean no scenario could be observed; the runner's own exit code
# is informative only, because a blocked (inconclusive) episode also fails the
# Go test, and only the records say which of the two happened.
FATAL_PHASES = {"cleanup"}

MIB = 1024 * 1024
# Stated post-drain tolerances. They are part of the record, so a reader can
# see exactly what "stabilized" and "returned to baseline" meant for this run.
TOLERANCES = {
    "rss_growth": "last post-drain native RSS <= 1.5 x baseline native RSS + 64 MiB per member, where native RSS = VmRSS - database bytes (dqlite holds the database in memory) - closed Raft segment bytes on the member (the retained log is held in memory, bounded by the snapshot threshold/trailing settings, W7-gamma)",
    "rss_stability": "post-drain VmRSS max - min <= max(24 MiB, 10% of the first post-drain sample)",
    "rss_limit": "every post-drain VmRSS < 90% of the member's cgroup memory.max",
    "heap_inuse": "last post-drain go_memstats_heap_inuse_bytes <= 2 x baseline + 32 MiB",
    "goroutines": "last post-drain go_goroutines <= baseline + max(40, 50% of baseline)",
    "fds": "last post-drain open FDs (/proc/1/fd) <= baseline + max(16, 25% of baseline)",
    "fd_stability": "post-drain open FDs max - min <= 8",
    "containers": "after drain and a grace covering kubelet's one-minute container GC, no task container or task pod carrying this run's ownership token remains (running or exited), and no task container runs at all; every kind node's container listing, the task pod listing and every task container's inspect must be observed (exit 0, parseable output), or the check is blocked",
    "process_identity": "every post-drain sample of a member carries a pod UID, container ID and restart count, identical across the post-drain series (kills and replacements before the drain are allowed); a missing identity blocks, and a change fails, because no fault is scheduled after the drain and a crash or OOM restart would reset the readings",
}

# The one supported seed range. Generation, validation and the record all use
# it: at most 18 decimal digits, inside the runner's int64
# (strconv.ParseInt(..., 10, 64) in test/robustness/exploratory_test.go).
SEED_MIN = 1
SEED_MAX = 10**18 - 1
SEED_RE = re.compile(r"[0-9]{1,18}")

KEY_RE = re.compile(r"^[a-z0-9][a-z0-9._-]{0,80}$")
METRIC_NAMES = (
    "go_goroutines",
    "go_threads",
    "go_memstats_heap_inuse_bytes",
    "go_memstats_sys_bytes",
    "process_open_fds",
    "process_resident_memory_bytes",
)


def now() -> str:
    return _dt.datetime.now(_dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")


def read_json(path: Path, default=None):
    try:
        return json.loads(path.read_text())
    except (OSError, ValueError):
        return default


def write_json(path: Path, value) -> None:
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")
    tmp.replace(path)


# ---------------------------------------------------------------------------
# init
# ---------------------------------------------------------------------------


def cmd_init(a) -> int:
    art = Path(a.artifacts)
    record = {
        "kind": "caesium-soak-qualification",
        "schema": 1,
        "soak_id": a.soak_id,
        "cluster": a.soak_id,
        "candidate_sha": a.candidate_sha,
        # A generated seed is recorded as generated; an invalid supplied one
        # is null (source "invalid") and the controller refuses the run.
        "seed": int(a.seed) if a.seed else None,
        "seed_source": a.seed_source,
        "profile": a.profile,
        "duration": a.duration or "",
        "started_at": now(),
        "result": "incomplete",
        "detail": "the host controller has not completed; an aborted run never leaves a pass behind",
        "manifest": {"families": FAMILIES, "checks": POST_DRAIN_CHECKS},
        "fault_class": "process kill (container SIGKILL with kubelet stopped) and member disk replacement; not power-loss qualification",
        "tolerances": TOLERANCES,
        "phases": {},
    }
    write_json(art / "soak.json", record)
    return 0


def cmd_set(a) -> int:
    """Merge key=value (JSON-decoded when possible) into soak.json."""
    path = Path(a.artifacts) / "soak.json"
    record = read_json(path, {})
    for item in a.pairs:
        key, _, raw = item.partition("=")
        try:
            record[key] = json.loads(raw)
        except ValueError:
            record[key] = raw
    write_json(path, record)
    return 0


# ---------------------------------------------------------------------------
# seed
# ---------------------------------------------------------------------------


def seed_error(raw: str) -> str:
    return f"CAESIUM_SOAK_SEED {raw[:40]!r} must be an integer from {SEED_MIN} to {SEED_MAX}"


def validate_seed(raw: str) -> tuple[int | None, str]:
    """Return (seed, "") for a supported seed, else (None, why)."""
    if not SEED_RE.fullmatch(raw):
        return None, seed_error(raw)
    value = int(raw)
    if not SEED_MIN <= value <= SEED_MAX:
        return None, seed_error(raw)
    return value, ""


def generate_seed() -> int:
    return SEED_MIN + secrets.randbelow(SEED_MAX - SEED_MIN + 1)


def resolve_seed(raw: str | None) -> dict:
    """The seed a run uses: CAESIUM_SOAK_SEED when set, else a generated one.

    A generated seed goes through the same validator as a supplied one, so the
    controller can never refuse a seed it generated itself.
    """
    if not raw:
        seed, err = validate_seed(str(generate_seed()))
        return {"seed": seed, "source": "generated", "error": err}
    seed, err = validate_seed(raw)
    return {"seed": seed, "source": "invalid" if err else "supplied", "error": err}


def cmd_seed(a) -> int:
    """Print SEED/SEED_SOURCE/SEED_ERROR shell assignments from CAESIUM_SOAK_SEED."""
    got = resolve_seed(os.environ.get("CAESIUM_SOAK_SEED", ""))
    print(f"SEED={shlex.quote('' if got['seed'] is None else str(got['seed']))}")
    print(f"SEED_SOURCE={shlex.quote(got['source'])}")
    print(f"SEED_ERROR={shlex.quote(got['error'])}")
    return 0


# ---------------------------------------------------------------------------
# host requests
# ---------------------------------------------------------------------------

REQUEST_FIELDS = ("request_id", "action", "owner_pod", "owner_kind_node", "owner_container_id", "run_id", "requested_at")
PARAM_FIELDS = ("episode", "family", "label", "token", "pod", "key")


def parse_request(cm: dict) -> dict:
    data = cm.get("data") or {}
    try:
        payload = json.loads(data.get("payload") or "{}")
    except ValueError:
        payload = {}
    if not isinstance(payload, dict):
        payload = {}
    params = payload.get("params")
    if not isinstance(params, dict):
        try:
            params = json.loads(data.get("params") or "{}")
        except ValueError:
            params = {}
    if not isinstance(params, dict):
        params = {}
    out = {}
    for k in REQUEST_FIELDS:
        v = payload.get(k) or data.get(k) or ""
        out[k] = str(v)
    out["params"] = {str(k): str(v) for k, v in params.items()}
    return out


def cmd_parse_request(a) -> int:
    """Print shell assignments for the fixed request/parameter key set only."""
    req = parse_request(json.load(sys.stdin))
    for k in REQUEST_FIELDS:
        print(f"r_{k}={shlex.quote(req[k])}")
    for k in PARAM_FIELDS:
        print(f"p_{k}={shlex.quote(req['params'].get(k, ''))}")
    return 0


def cmd_store_record(a) -> int:
    """Store the runner's record under records/<key>.json after validation."""
    req = parse_request(json.load(sys.stdin))
    key = req["params"].get("key", "")
    if not KEY_RE.match(key):
        print(f"invalid record key {key!r}", file=sys.stderr)
        return 1
    try:
        value = json.loads(req["params"].get("payload", ""))
    except ValueError as exc:
        print(f"record {key} payload is not JSON: {exc}", file=sys.stderr)
        return 1
    records = Path(a.artifacts) / "records"
    records.mkdir(exist_ok=True)
    write_json(records / f"{key}.json", value)
    print(key)
    return 0


def cmd_schedule_append(a) -> int:
    """Append one ACTUAL fault-schedule entry (host wall clock, UTC)."""
    path = Path(a.artifacts) / "fault-schedule.jsonl"
    seq = 0
    if path.exists():
        seq = sum(1 for line in path.read_text().splitlines() if line.strip())
    entry = {
        "seq": seq,
        "action": a.action,
        "status": a.status,
        "request_id": a.request_id,
        "episode": a.episode,
        "family": a.family,
        "pod": a.pod,
        "node": a.node,
        "container_id": a.container_id,
        "run_id": a.run_id,
        "requested_at": a.requested_at,
        "started_at": a.started_at,
        "finished_at": a.finished_at or now(),
        "evidence_file": a.evidence_file,
        "detail": a.detail,
    }
    with path.open("a") as fh:
        fh.write(json.dumps(entry, sort_keys=True) + "\n")
    return 0


# ---------------------------------------------------------------------------
# samples and container inventories
# ---------------------------------------------------------------------------


def parse_sample_text(text: str) -> dict:
    """Parse one member's exec output (see soak-tests.sh sample_member)."""
    out: dict = {"errors": []}
    for line in text.splitlines():
        line = line.strip()
        if not line:
            continue
        m = re.match(r"^(VmRSS|VmHWM|Threads):\s+(\d+)(?:\s+kB)?$", line)
        if m:
            factor = 1024 if m.group(1) in ("VmRSS", "VmHWM") else 1
            out[m.group(1).lower() + ("_bytes" if factor == 1024 else "")] = int(m.group(2)) * factor
            continue
        m = re.match(r"^FDS\s+(\d+)$", line)
        if m:
            out["open_fds"] = int(m.group(1))
            continue
        m = re.match(r"^CMDLINE\s+(.*)$", line)
        if m:
            out["pid1_cmdline"] = m.group(1).strip()
            continue
        m = re.match(r"^MEMMAX\s+(\S+)$", line)
        if m:
            out["cgroup_memory_max_bytes"] = None if m.group(1) == "max" else int(m.group(1)) if m.group(1).isdigit() else None
            continue
        m = re.match(r"^MEMCUR\s+(\d+)$", line)
        if m:
            out["cgroup_memory_current_bytes"] = int(m.group(1))
            continue
        m = re.match(r"^DQFILE\s+(\d+)\s+(\S+)$", line)
        if m:
            size, name = int(m.group(1)), m.group(2)
            if re.fullmatch(r"\d+-\d+", name):
                out["closed_segment_bytes"] = out.get("closed_segment_bytes", 0) + size
                out["closed_segments"] = out.get("closed_segments", 0) + 1
            elif re.fullmatch(r"open-\d+", name):
                out["open_segment_bytes"] = out.get("open_segment_bytes", 0) + size
            elif re.fullmatch(r"snapshot-\d+-\d+-\d+", name):
                out["snapshot_bytes"] = max(out.get("snapshot_bytes", 0), size)
            out.setdefault("closed_segment_bytes", 0)
            continue
        m = re.match(r"^DBQUERY\s*(.*)$", line)
        if m:
            try:
                rows = json.loads(m.group(1)).get("rows") or []
                out["db_bytes"] = int(float(rows[0][0]))
            except (ValueError, TypeError, IndexError, AttributeError):
                out["errors"].append("database size query failed: " + m.group(1)[:200])
            continue
        parts = line.split()
        if len(parts) == 2 and parts[0] in METRIC_NAMES:
            try:
                out[parts[0]] = float(parts[1])
            except ValueError:
                out["errors"].append(f"unparseable metric line {line!r}")
    for need in ("vmrss_bytes", "open_fds", "go_goroutines", "go_memstats_heap_inuse_bytes", "db_bytes", "closed_segment_bytes"):
        if need not in out:
            out["errors"].append(f"missing {need}")
    if "caesium" not in out.get("pid1_cmdline", ""):
        out["errors"].append("PID 1 is not the caesium server; the sample would describe the wrong process")
    return out


def cmd_sample_parse(a) -> int:
    base = Path(a.dir)
    members = {}
    for raw in sorted(base.glob(f"{a.label}--*.txt")):
        pod = raw.stem.split("--", 1)[1]
        parsed = parse_sample_text(raw.read_text())
        meta = read_json(raw.with_suffix(".pod.json"), {}) or {}
        parsed.update({k: meta.get(k) for k in ("uid", "container_id", "restart_count", "node", "ip") if k in meta})
        members[pod] = parsed
    sample = {"label": a.label, "taken_at": now(), "members": members}
    write_json(base / f"{a.label}.json", sample)
    compact = {
        pod: {
            "rss_mib": round((m.get("vmrss_bytes") or 0) / MIB, 1),
            "fds": m.get("open_fds"),
            "goroutines": m.get("go_goroutines"),
            "restarts": m.get("restart_count"),
            "errors": m.get("errors"),
        }
        for pod, m in members.items()
    }
    print(json.dumps(compact, sort_keys=True))
    return 0


def is_task_container(c: dict, namespace: str) -> bool:
    labels = c.get("labels") or {}
    return labels.get("io.kubernetes.pod.namespace") == namespace and labels.get("io.kubernetes.container.name") == "atom"


def summarise_containers(ps_by_node: dict, inspect_by_id: dict, namespace: str, token: str, task_pods: list) -> dict:
    """ps_by_node: node -> `crictl ps -a -o json`; inspect_by_id: id -> `crictl inspect` JSON.

    Ownership is read only from an observed inspect: a task container whose
    inspect is absent (None) is unverified, never "unowned".
    """
    nodes = {}
    owned_running, owned_total, task_running, unverified = [], [], [], []
    for node, ps in sorted(ps_by_node.items()):
        counts = {"containers": 0, "task_containers": 0, "owned": 0, "owned_running": 0, "unowned_task": 0, "unverified": 0}
        for c in (ps or {}).get("containers", []) or []:
            counts["containers"] += 1
            if not is_task_container(c, namespace):
                continue
            labels = c.get("labels") or {}
            counts["task_containers"] += 1
            cid = c.get("id", "")
            state = c.get("state", "")
            running = state == "CONTAINER_RUNNING"
            ident = {"node": node, "id": cid[:13], "pod": labels.get("io.kubernetes.pod.name", ""), "state": state}
            if running:
                task_running.append(ident)
            inspect = inspect_by_id.get(cid)
            if inspect is None:
                counts["unverified"] += 1
                unverified.append(ident)
                continue
            env = container_env(inspect)
            if env.get("CAESIUM_SOAK_OWNER") == token:
                counts["owned"] += 1
                owned_total.append(ident)
                if running:
                    counts["owned_running"] += 1
                    owned_running.append(ident)
            else:
                counts["unowned_task"] += 1
        nodes[node] = counts
    return {
        "nodes": nodes,
        "owned_task_containers": owned_total[:50],
        "owned_running": owned_running[:50],
        "task_running": task_running[:50],
        "unverified_task_containers": unverified[:50],
        "task_pods": task_pods[:50],
        "counts": {
            "owned_task_containers": len(owned_total),
            "owned_running": len(owned_running),
            "task_running": len(task_running),
            "unverified_task_containers": len(unverified),
            "task_pods": len(task_pods),
        },
    }


def container_env_observed(inspect: dict) -> bool:
    """Whether a `crictl inspect` document carries an environment to read ownership from."""
    info = inspect.get("info") or {}
    return isinstance((info.get("config") or {}).get("envs"), list) or isinstance(
        ((info.get("runtimeSpec") or {}).get("process") or {}).get("env"), list)


def container_env(inspect: dict) -> dict:
    info = inspect.get("info") or {}
    env = {}
    for item in ((info.get("config") or {}).get("envs") or []):
        if isinstance(item, dict) and "key" in item:
            env[item["key"]] = item.get("value", "")
    for item in (((info.get("runtimeSpec") or {}).get("process") or {}).get("env") or []):
        if isinstance(item, str) and "=" in item:
            k, _, v = item.partition("=")
            env.setdefault(k, v)
    return env


def task_container_ids(ps: dict, namespace: str) -> list[str]:
    out = []
    for c in (ps or {}).get("containers", []) or []:
        if is_task_container(c, namespace):
            cid = str(c.get("id", ""))
            if CONTAINER_ID_RE.fullmatch(cid):
                out.append(cid)
    return out


CONTAINER_ID_RE = re.compile(r"[0-9a-f]{12,64}")


def load_evidence(path: Path, shape) -> tuple[dict | None, str]:
    """One collected observation: (document, "") or (None, why it is missing).

    take_inventory (soak-tests.sh) writes <file>.rc, the command's exit code,
    after the command finishes. An observation counts only when that exit code
    is 0 and the output parses to the expected shape: a failed, interrupted or
    empty collection is missing evidence, never an empty inventory.
    """
    name = path.name
    if not path.exists():
        return None, f"{name}: not collected"
    try:
        rc = Path(str(path) + ".rc").read_text().strip()
    except OSError:
        return None, f"{name}: no exit code recorded (collection failed or was interrupted)"
    if rc != "0":
        try:
            err = " ".join(Path(str(path) + ".err").read_text().split())[:200]
        except OSError:
            err = ""
        return None, f"{name}: command exited {rc or '?'}" + (f" ({err})" if err else "")
    doc = read_json(path)
    if not isinstance(doc, dict) or not shape(doc):
        return None, f"{name}: output is empty or not the expected JSON"
    return doc, ""


def cmd_task_container_ids(a) -> int:
    for cid in task_container_ids(read_json(Path(a.file), {}), a.namespace):
        print(cid)
    return 0


def cmd_containers_parse(a) -> int:
    """Summarise one inventory. Nodes are counted by SUCCESSFUL listings, and a
    failed or unparseable listing, pod list or task-container inspect is a
    recorded evidence gap that blocks post_drain_containers."""
    base = Path(a.dir)
    ps_by_node, inspect_by_id, gaps = {}, {}, []
    for f in sorted(base.glob(f"{a.label}--ps--*.json")):
        doc, why = load_evidence(f, lambda d: isinstance(d.get("containers"), list))
        if doc is None:
            gaps.append(why)
            continue
        ps_by_node[f.stem.split("--ps--", 1)[1]] = doc
    if a.expect_nodes and len(ps_by_node) != a.expect_nodes:
        gaps.insert(0, f"inventoried {len(ps_by_node)} nodes successfully, expected {a.expect_nodes}")
    for ps in ps_by_node.values():
        for c in ps["containers"]:
            if not is_task_container(c, a.namespace):
                continue
            cid = str(c.get("id", ""))
            if CONTAINER_ID_RE.fullmatch(cid):
                doc, why = load_evidence(base / f"{a.label}--inspect--{cid}.json", container_env_observed)
            else:
                doc, why = None, f"task container id {cid[:20]!r} cannot be inspected"
            inspect_by_id[cid] = doc
            if doc is None:
                gaps.append(f"ownership of task container {cid[:13]} unobservable: {why}")
    pods_doc, why = load_evidence(base / f"{a.label}--pods.json", lambda d: isinstance(d.get("items"), list))
    if pods_doc is None:
        gaps.append(f"task pod list unobservable: {why}")
        pods_doc = {}
    task_pods = []
    for p in pods_doc.get("items", []) or []:
        meta, status = p.get("metadata") or {}, p.get("status") or {}
        owned = any(
            e.get("name") == "CAESIUM_SOAK_OWNER" and e.get("value") == a.token
            for c in ((p.get("spec") or {}).get("containers") or [])
            for e in (c.get("env") or [])
        )
        task_pods.append({"pod": meta.get("name", ""), "phase": status.get("phase", ""), "owned": owned,
                          "node": (p.get("spec") or {}).get("nodeName", "")})
    summary = summarise_containers(ps_by_node, inspect_by_id, a.namespace, a.token, task_pods)
    summary.update({"label": a.label, "taken_at": now(), "nodes_inventoried": sorted(ps_by_node),
                    "evidence_gaps": gaps[:50]})
    if gaps:
        summary["error"] = "; ".join(gaps[:10]) + (f" (+{len(gaps) - 10} more)" if len(gaps) > 10 else "")
    write_json(base / f"{a.label}.json", summary)
    print(json.dumps({"counts": summary["counts"], "error": summary.get("error", "")}, sort_keys=True))
    return 0


# ---------------------------------------------------------------------------
# finalize
# ---------------------------------------------------------------------------


def judge_resources(baseline: dict | None, post: list[dict]) -> tuple[dict, dict]:
    """Return (resources check, fds check) from the baseline and post-drain samples."""
    res = {"name": "post_drain_resources", "tolerance": [TOLERANCES[k] for k in ("rss_growth", "rss_stability", "rss_limit", "heap_inuse", "goroutines", "process_identity")], "members": {}}
    fds = {"name": "post_drain_fds", "tolerance": [TOLERANCES["fds"], TOLERANCES["fd_stability"], TOLERANCES["process_identity"]], "members": {}}
    if not baseline or not baseline.get("members"):
        res.update(status="blocked", detail="no baseline resource sample")
        fds.update(status="blocked", detail="no baseline resource sample")
        return res, fds
    if len(post) < 2:
        res.update(status="blocked", detail=f"{len(post)} post-drain samples; stabilization needs at least 2")
        fds.update(status="blocked", detail=f"{len(post)} post-drain samples; stabilization needs at least 2")
        return res, fds
    res_fail, fd_fail, blocked = [], [], []
    for pod, base in sorted(baseline["members"].items()):
        series = [s["members"].get(pod) for s in post]
        if base.get("errors") or any(m is None or m.get("errors") for m in series):
            blocked.append(f"{pod}: incomplete sample ({base.get('errors')}, {[m.get('errors') if m else 'absent' for m in series]})")
            continue
        # Stabilization is only meaningful for ONE process: the post-drain
        # series must share a pod UID, container ID and restart count. The
        # baseline may differ (planned kills and replacements precede the
        # drain); the first post-drain sample is the reference.
        idents = [process_identity(m) for m in series]
        missing = [i for i, ident in enumerate(idents, 1) if ident is None]
        if missing:
            blocked.append(f"{pod}: post-drain sample(s) {missing} carry no pod UID/container ID/restart count, "
                           "so the series cannot be shown to describe one process")
            continue
        if len(set(idents)) > 1:
            seen = [{"uid": u, "container_id": c, "restart_count": r} for u, c, r in idents]
            msg = (f"{pod}: restarted during the fault-free post-drain window {seen}; "
                   "its readings describe fresh processes and cannot show stabilization")
            res["members"][pod] = {"post_drain_identities": seen}
            fds["members"][pod] = {"post_drain_identities": seen}
            res_fail.append(msg)
            fd_fail.append(msg)
            continue
        uid, container_id, restart_count = idents[0]
        rss = [m["vmrss_bytes"] for m in series]
        last = series[-1]

        def native(m):
            return m["vmrss_bytes"] - m["db_bytes"] - m["closed_segment_bytes"]

        b_rss = native(base)
        rss_bound = 1.5 * b_rss + 64 * MIB
        post_native = native(last)
        spread = max(rss) - min(rss)
        spread_bound = max(24 * MIB, 0.10 * rss[0])
        heap_bound = 2 * base["go_memstats_heap_inuse_bytes"] + 32 * MIB
        g_bound = base["go_goroutines"] + max(40, 0.5 * base["go_goroutines"])
        limit = last.get("cgroup_memory_max_bytes")
        member = {
            "baseline_rss_mib": round(base["vmrss_bytes"] / MIB, 1),
            "post_rss_mib": [round(v / MIB, 1) for v in rss],
            "baseline_native_rss_mib": round(b_rss / MIB, 1),
            "post_native_rss_mib": round(post_native / MIB, 1),
            "native_rss_bound_mib": round(rss_bound / MIB, 1),
            "baseline_db_mib": round(base["db_bytes"] / MIB, 1),
            "post_db_mib": round(last["db_bytes"] / MIB, 1),
            "baseline_raft_log_mib": round(base["closed_segment_bytes"] / MIB, 1),
            "post_raft_log_mib": round(last["closed_segment_bytes"] / MIB, 1),
            "rss_spread_mib": round(spread / MIB, 1),
            "rss_spread_bound_mib": round(spread_bound / MIB, 1),
            "baseline_heap_inuse_mib": round(base["go_memstats_heap_inuse_bytes"] / MIB, 1),
            "post_heap_inuse_mib": round(last["go_memstats_heap_inuse_bytes"] / MIB, 1),
            "baseline_goroutines": base["go_goroutines"],
            "post_goroutines": [m["go_goroutines"] for m in series],
            "memory_limit_mib": round(limit / MIB, 1) if limit else None,
            "process_restarted_since_baseline": base.get("container_id") != last.get("container_id")
            or base.get("uid") != last.get("uid"),
            "post_drain_identity": {"uid": uid, "container_id": container_id, "restart_count": restart_count},
        }
        res["members"][pod] = member
        if post_native > rss_bound:
            res_fail.append(f"{pod}: native RSS {member['post_native_rss_mib']} MiB > {member['native_rss_bound_mib']} MiB "
                            f"(VmRSS {member['post_rss_mib'][-1]} MiB, database {member['post_db_mib']} MiB, Raft log {member['post_raft_log_mib']} MiB)")
        if spread > spread_bound:
            res_fail.append(f"{pod}: RSS still moving after drain ({member['rss_spread_mib']} MiB > {member['rss_spread_bound_mib']} MiB)")
        if limit and max(rss) >= 0.9 * limit:
            res_fail.append(f"{pod}: RSS {round(max(rss) / MIB, 1)} MiB is within 10% of the {member['memory_limit_mib']} MiB limit")
        if last["go_memstats_heap_inuse_bytes"] > heap_bound:
            res_fail.append(f"{pod}: heap in use {member['post_heap_inuse_mib']} MiB > {round(heap_bound / MIB, 1)} MiB")
        if last["go_goroutines"] > g_bound:
            res_fail.append(f"{pod}: goroutines {last['go_goroutines']} > {g_bound}")
        f_series = [m["open_fds"] for m in series]
        f_bound = base["open_fds"] + max(16, 0.25 * base["open_fds"])
        fds["members"][pod] = {
            "baseline": base["open_fds"],
            "post": f_series,
            "bound": f_bound,
            "spread": max(f_series) - min(f_series),
            "process_restarted_since_baseline": member["process_restarted_since_baseline"],
        }
        if f_series[-1] > f_bound:
            fd_fail.append(f"{pod}: {f_series[-1]} open FDs > {f_bound} (baseline {base['open_fds']})")
        if max(f_series) - min(f_series) > 8:
            fd_fail.append(f"{pod}: open FDs still moving after drain {f_series}")
    for check, fails in ((res, res_fail), (fds, fd_fail)):
        if fails:
            check.update(status="fail", detail="; ".join(fails))
        elif blocked:
            check.update(status="blocked", detail="; ".join(blocked))
        elif not check["members"]:
            check.update(status="blocked", detail="no member had both a baseline and post-drain samples")
        else:
            check.update(status="pass", detail="within the stated tolerances")
    return res, fds


def process_identity(m: dict) -> tuple | None:
    """(pod UID, container ID, restart count) of a sample, or None when any is missing."""
    uid, cid, restarts = m.get("uid"), m.get("container_id"), m.get("restart_count")
    if not uid or not cid or not isinstance(restarts, int) or isinstance(restarts, bool):
        return None
    return str(uid), str(cid), restarts


def judge_containers(inv: dict | None, baseline: dict | None) -> dict:
    check = {"name": "post_drain_containers", "tolerance": TOLERANCES["containers"]}
    if not inv:
        check.update(status="blocked", detail="no post-drain container inventory")
        return check
    # A violation proven by observed evidence fails even when other evidence is
    # missing; otherwise any evidence gap blocks (never an empty inventory).
    gaps = inv.get("error", "")
    c = inv.get("counts") or {}
    owned_pods = [p for p in inv.get("task_pods", []) if p.get("owned")]
    check["inventory"] = c
    check["baseline_inventory"] = (baseline or {}).get("counts")
    fails = []
    if c.get("owned_running", 0):
        fails.append(f"{c['owned_running']} owned task containers still running after drain: {inv.get('owned_running')}")
    if c.get("task_running", 0):
        fails.append(f"{c['task_running']} task containers running after drain: {inv.get('task_running')}")
    if c.get("owned_task_containers", 0):
        fails.append(f"{c['owned_task_containers']} owned task containers retained after drain: {inv.get('owned_task_containers')}")
    if owned_pods:
        fails.append(f"{len(owned_pods)} owned task pods retained after drain: {owned_pods[:10]}")
    if fails:
        check.update(status="fail", detail="; ".join(fails) + (f"; evidence also incomplete: {gaps}" if gaps else ""))
    elif gaps:
        check.update(status="blocked", detail=gaps)
    else:
        check.update(status="pass", detail="no owned task container or pod remains; nothing task-side is running")
    return check


def load_phases(art: Path) -> dict:
    phases = {}
    path = art / "phases.txt"
    if path.exists():
        for line in path.read_text().splitlines():
            name, _, rc = line.strip().partition("=")
            if name and rc.lstrip("-").isdigit():
                phases[name] = int(rc)
    return phases


def fold(art: Path) -> dict:
    record = read_json(art / "soak.json", {}) or {}
    records_dir = art / "records"
    phases = load_phases(art)
    record["phases"] = phases
    plan = read_json(records_dir / "plan.json")
    episodes = []
    for f in sorted(records_dir.glob("episode-*.json")):
        rec = read_json(f)
        if not isinstance(rec, dict):
            episodes.append({"key": f.stem, "status": "blocked", "detail": "record unreadable"})
            continue
        if rec.get("soak_id") != record.get("soak_id") or rec.get("seed") != record.get("seed"):
            rec = dict(rec, status="blocked", detail="record belongs to another invocation (soak_id/seed mismatch)")
        episodes.append(rec)
    by_key = {e.get("key"): e for e in episodes}

    scenarios = {}
    planned_mandatory = []
    if isinstance(plan, dict):
        planned_mandatory = [p for p in plan.get("plan", []) if p.get("mandatory")]
        for p in planned_mandatory:
            key = f"episode-{int(p['index']):02d}-{p['family']}"
            if key not in by_key:
                by_key[key] = {"key": key, "family": p["family"], "index": p["index"], "mandatory": True,
                               "status": "blocked", "detail": "mandatory episode has no record"}
    all_eps = sorted(by_key.values(), key=lambda e: (e.get("index", 999), e.get("key", "")))
    for fam in FAMILIES:
        eps = [e for e in all_eps if e.get("family") == fam]
        statuses = [e.get("status") for e in eps]
        if not eps:
            st, detail = "blocked", "no episode of this family has a record"
        elif "fail" in statuses:
            st, detail = "fail", "; ".join(f"{e['key']}: {e.get('detail', '')}" for e in eps if e.get("status") == "fail")
        elif any(s != "pass" for s in statuses):
            st, detail = "blocked", "; ".join(f"{e['key']}: {e.get('detail', '')}" for e in eps if e.get("status") != "pass")
        else:
            st, detail = "pass", f"{len(eps)} episode(s) passed"
        scenarios[fam] = {"status": st, "episodes": [e.get("key") for e in eps], "detail": detail}

    drain = read_json(records_dir / "drain.json")
    if isinstance(drain, dict) and drain.get("soak_id") == record.get("soak_id"):
        obs = drain.get("observations") or {}
        scenarios["whole_schedule_safety"] = {
            "status": drain.get("status", "blocked"),
            "detail": drain.get("detail", ""),
            "admitted_runs": obs.get("admitted_runs"),
            "final_statuses": obs.get("final_statuses"),
            "schedule_seconds": obs.get("schedule_seconds"),
            "task_pods_after_grace": obs.get("task_pods_after_grace"),
        }
    else:
        scenarios["whole_schedule_safety"] = {"status": "blocked", "detail": "no drain record from this invocation"}

    samples = art / "samples"
    baseline = read_json(samples / "baseline.json")
    post = []
    for i in range(1, 50):
        s = read_json(samples / f"post-drain-{i}.json")
        if s is None:
            break
        post.append(s)
    res, fds = judge_resources(baseline, post)
    scenarios["post_drain_resources"] = res
    scenarios["post_drain_fds"] = fds
    containers = art / "containers"
    scenarios["post_drain_containers"] = judge_containers(read_json(containers / "post-drain.json"), read_json(containers / "baseline.json"))

    schedule = []
    sched_path = art / "fault-schedule.jsonl"
    if sched_path.exists():
        for line in sched_path.read_text().splitlines():
            if line.strip():
                try:
                    schedule.append(json.loads(line))
                except ValueError:
                    schedule.append({"unparseable": line[:200]})
    record["fault_schedule"] = schedule
    record["fault_schedule_file"] = "fault-schedule.jsonl"
    record["executed_episodes"] = [
        {k: e.get(k) for k in ("key", "family", "index", "mandatory", "status", "started_at", "ended_at", "duration_s", "params", "kinds", "detail")}
        for e in all_eps
    ]
    if isinstance(plan, dict):
        record["planned_episodes"] = len(plan.get("plan", []))
        record["schedule_budget"] = plan.get("schedule_budget")
        record["queue_max_depth"] = plan.get("queue_max_depth")
        record["member_removal_path"] = plan.get("member_removal_path")
    record["scenarios"] = scenarios

    failed_phases = {k: v for k, v in phases.items() if v != 0}
    statuses = {k: v.get("status") for k, v in scenarios.items()}
    reasons = []
    if any(s == "fail" for s in statuses.values()):
        result = "fail"
        reasons = [f"{k}: {scenarios[k].get('detail', '')}" for k, s in statuses.items() if s == "fail"]
    elif set(failed_phases) & FATAL_PHASES:
        result = "fail"
        reasons = [f"phase {k} exited {v}" for k, v in failed_phases.items() if k in FATAL_PHASES]
    elif failed_phases or any(s != "pass" for s in statuses.values()):
        result = "blocked"
        reasons = [f"phase {k} exited {v}" for k, v in failed_phases.items()]
        reasons += [f"{k}: {scenarios[k].get('detail', '')}" for k, s in statuses.items() if s != "pass"]
    elif record.get("provenance", {}).get("verified") is not True and not record.get("provenance", {}).get("override"):
        result = "blocked"
        reasons = ["candidate image provenance is unverified and no override was recorded"]
    else:
        result = "pass"
    if result == "pass" and "runner" not in phases:
        result, reasons = "blocked", ["the runner phase never recorded an exit code"]
    record["result"] = result
    record["failed_gates"] = sorted(k for k, s in statuses.items() if s != "pass") + sorted(failed_phases)
    record["detail"] = "; ".join(reasons)[:4000] if reasons else "every family, check and phase passed"
    record["finished_at"] = now()
    return record


def cmd_finalize(a) -> int:
    art = Path(a.artifacts)
    record = fold(art)
    write_json(art / "soak.json", record)
    print(json.dumps({"result": record["result"], "failed_gates": record["failed_gates"]}, sort_keys=True))
    return 0 if record["result"] == "pass" else 1


def cmd_result(a) -> int:
    record = read_json(Path(a.artifacts) / "soak.json", {}) or {}
    print(record.get("result", "incomplete"))
    return 0


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    sub = ap.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("init")
    for flag in ("artifacts", "soak-id", "candidate-sha", "seed", "profile"):
        p.add_argument("--" + flag, required=True)
    p.add_argument("--seed-source", default="supplied", choices=("supplied", "generated", "invalid"))
    p.add_argument("--duration", default="")
    p.set_defaults(fn=cmd_init)
    p = sub.add_parser("seed")
    p.set_defaults(fn=cmd_seed)
    p = sub.add_parser("set")
    p.add_argument("--artifacts", required=True)
    p.add_argument("pairs", nargs="+")
    p.set_defaults(fn=cmd_set)
    p = sub.add_parser("parse-request")
    p.set_defaults(fn=cmd_parse_request)
    p = sub.add_parser("store-record")
    p.add_argument("--artifacts", required=True)
    p.set_defaults(fn=cmd_store_record)
    p = sub.add_parser("schedule-append")
    p.add_argument("--artifacts", required=True)
    for flag in ("action", "status", "request-id", "episode", "family", "pod", "node", "container-id",
                 "run-id", "requested-at", "started-at", "finished-at", "evidence-file", "detail"):
        p.add_argument("--" + flag, default="")
    p.set_defaults(fn=cmd_schedule_append)
    p = sub.add_parser("sample-parse")
    p.add_argument("--dir", required=True)
    p.add_argument("--label", required=True)
    p.set_defaults(fn=cmd_sample_parse)
    p = sub.add_parser("containers-parse")
    p.add_argument("--dir", required=True)
    p.add_argument("--label", required=True)
    p.add_argument("--namespace", required=True)
    p.add_argument("--token", required=True)
    p.add_argument("--expect-nodes", type=int, default=0)
    p.set_defaults(fn=cmd_containers_parse)
    p = sub.add_parser("task-container-ids")
    p.add_argument("--file", required=True)
    p.add_argument("--namespace", required=True)
    p.set_defaults(fn=cmd_task_container_ids)
    p = sub.add_parser("finalize")
    p.add_argument("--artifacts", required=True)
    p.set_defaults(fn=cmd_finalize)
    p = sub.add_parser("result")
    p.add_argument("--artifacts", required=True)
    p.set_defaults(fn=cmd_result)
    a = ap.parse_args(argv)
    return a.fn(a)


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
