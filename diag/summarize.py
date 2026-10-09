#!/usr/bin/env python3
"""DIAG ONLY (never merged): reduce trial results and bpftrace timelines."""
import collections
import json
import re
import sys
from pathlib import Path

out = Path(sys.argv[1])
label = sys.argv[2]
results = [line.split() for line in (out / "results").read_text().splitlines() if line.startswith("RESULT ")]
counts = collections.Counter((r[2], r[3]) for r in results)
oom_while_running = collections.Counter(r[2] for r in results if r[3] == "ok" and r[6] != "-")
summary = {"label": label, "trials": collections.Counter(r[2] for r in results),
           "outcomes": {f"{m}:{o}": c for (m, o), c in sorted(counts.items())},
           "ok_with_oom_seen_while_running": oom_while_running}

trace = out / "trace.log"
timeline = collections.defaultdict(list)
victims = []
if trace.exists():
    for line in trace.read_text(errors="replace").splitlines():
        if not line.startswith("T "):
            continue
        parts = line.split(" ", 3)
        stamp, kind, rest = int(parts[1]), parts[2], parts[3] if len(parts) > 3 else ""
        if kind == "victim":
            victims.append((stamp, rest))
            continue
        match = re.search(r"(?:docker-|libpod-)([0-9a-f]{64})\.scope", line)
        if match:
            timeline[match.group(1)].append((stamp, kind, rest))


def describe(cid):
    events = sorted(timeline.get(cid, []))
    if not events:
        return None
    first, last = events[0][0], events[-1][0]
    rows = [(s, k, r) for s, k, r in events]
    rows += [(s, "victim", r) for s, r in victims if first <= s <= last]
    rows.sort()
    empty = next((s for s, k, r in rows if k.startswith("populated=0")), None)
    rmdir = next(((s, r) for s, k, r in rows if k == "rmdir"), None)
    victim = next((s for s, k, r in rows if k == "victim"), None)
    reads = [(s, r) for s, k, r in rows if k == "open" and "memory.events" in r]
    after = [(s, r) for s, r in reads if victim is not None and s >= victim]
    ms = lambda a, b: None if a is None or b is None else round((a - b) / 1e6, 3)
    return {
        "victim_to_empty_ms": ms(empty, victim),
        "empty_to_rmdir_ms": ms(rmdir[0] if rmdir else None, empty),
        "rmdir_by": re.search(r"by=(\S+)", rmdir[1]).group(1) if rmdir else None,
        "runtime_reads_after_kill": [{"ms_after_kill": ms(s, victim), "ret": int(re.search(r"ret=(-?\d+)", r).group(1)),
                                      "by": re.search(r"by=(\S+)", r).group(1)} for s, r in after],
        "rows": [f"{round((s - first) / 1e6, 3):>9} ms {k} {r}" for s, k, r in rows],
    }


details = []
for r in results:
    mode, outcome, cid = r[2], r[3], r[4]
    if len(cid) != 64:
        continue
    d = describe(cid)
    if d is None:
        continue
    d.update(mode=mode, outcome=outcome, cid=cid)
    details.append(d)

agg = collections.defaultdict(lambda: collections.Counter())
for d in details:
    key = f"{d['mode']}:{d['outcome']}"
    ok_reads = [x for x in d["runtime_reads_after_kill"] if x["ret"] >= 0]
    failed_reads = [x for x in d["runtime_reads_after_kill"] if x["ret"] < 0]
    agg[key]["traced"] += 1
    agg[key]["successful_runtime_read_after_kill"] += bool(ok_reads)
    agg[key]["only_failed_runtime_reads_after_kill"] += bool(failed_reads and not ok_reads)
    agg[key]["no_runtime_read_after_kill"] += not d["runtime_reads_after_kill"]
    agg[key][f"rmdir_by={d['rmdir_by']}"] += 1
summary["trace"] = {k: dict(v) for k, v in agg.items()}
print(json.dumps(summary, indent=1, default=dict))
(out / "summary.json").write_text(json.dumps(summary, indent=1, default=dict))
with (out / "timelines.txt").open("w") as stream:
    for d in sorted(details, key=lambda d: (d["outcome"] == "ok", d["mode"])):
        if d["outcome"] == "ok" and stream.tell() > 2_000_000:
            continue
        stream.write(f"== {d['mode']} {d['outcome']} {d['cid']} victim->empty={d['victim_to_empty_ms']}ms "
                     f"empty->rmdir={d['empty_to_rmdir_ms']}ms rmdir_by={d['rmdir_by']} reads={d['runtime_reads_after_kill']}\n")
        stream.write("\n".join(d["rows"]) + "\n")
