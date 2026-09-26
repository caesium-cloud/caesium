#!/usr/bin/env python3
"""Sample survivor memory during F2 snapshot catalog writes.

The host controller runs the probe inside each surviving caesium container and
records cgroup anon/file (or cgroup v1 usage and stat), caesium RSS, and the
existing GET /metrics Go heap and GC series. Absent series are recorded. This
does not change the 1Gi limit, Raft retention, or snapshot pass rules except
to refuse a pass when the samples themselves are missing.
"""

from __future__ import annotations

import argparse
import fcntl
import json
import math
import os
import re
import signal
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path


MEMBERS = ("caesium-0", "caesium-1")
REASONS = ("cadence", "apply-error")
MEMORY_LIMIT = "1Gi"
SOURCE = "kubectl-exec"
GO_SERIES = {
    "heap_alloc_bytes": "go_memstats_heap_alloc_bytes",
    "heap_inuse_bytes": "go_memstats_heap_inuse_bytes",
    "last_gc_time_seconds": "go_memstats_last_gc_time_seconds",
}
GC_SERIES = "go_gc_duration_seconds"
METRIC_RE = re.compile(
    r"^(?P<name>[A-Za-z_:][A-Za-z0-9_:]*)"
    r"(?:\{(?P<labels>[^}]*)\})?"
    r"\s+(?P<value>\S+)(?:\s+\S+)?$"
)
LABEL_RE = re.compile(r'([A-Za-z_][A-Za-z0-9_]*)="((?:\\.|[^"\\])*)"')
STAT_RE = re.compile(r"^([A-Za-z_][A-Za-z0-9_]*)\s+(\d+)$")
RSS_RE = re.compile(r"^VmRSS:\s*(\d+)\s+kB\s*$", re.MULTILINE)

# POSIX sh. Tests point CAESIUM_MEMORY_PROBE_ROOT at a fixture tree. The
# container leaves that unset and reads its own cgroup, /proc and /metrics.
PROBE_SCRIPT = r"""root="${CAESIUM_MEMORY_PROBE_ROOT:-}"
printf '%s\n' 'member-sample-format: 1'

first_readable() {
  _fr_root=$1
  shift
  for _fr_path in "$@"; do
    if [ -r "${_fr_root}${_fr_path}" ]; then
      printf '%s\n' "$_fr_path"
      return 0
    fi
  done
  return 1
}

emit_file() {
  _name=$1
  _path=$2
  printf '%s\n' "--- section ${_name} path=${_path}"
  if [ -n "$_path" ] && [ -r "${root}${_path}" ]; then
    printf '%s\n' 'status=present'
    cat "${root}${_path}" 2>/dev/null
    printf '\n'
  else
    printf '%s\n' 'status=absent'
  fi
}

cgfile=""
if [ -r "${root}/proc/1/cgroup" ]; then
  cgfile="${root}/proc/1/cgroup"
elif [ -r "${root}/proc/self/cgroup" ]; then
  cgfile="${root}/proc/self/cgroup"
fi
v2_rel=""
v1_rel=""
if [ -n "$cgfile" ]; then
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      0::*)
        v2_rel=${line#0::}
        ;;
      *)
        rest=${line#*:}
        controllers=${rest%%:*}
        path=${rest#*:}
        case ",${controllers}," in
          *,memory,*) v1_rel=$path ;;
        esac
        ;;
    esac
  done <"$cgfile"
fi

v2_current=""
v2_stat=""
set -- /sys/fs/cgroup/memory.current
if [ -n "$v2_rel" ] && [ "$v2_rel" != "/" ]; then
  set -- /sys/fs/cgroup/memory.current "/sys/fs/cgroup${v2_rel}/memory.current"
fi
if v2_current=$(first_readable "$root" "$@"); then
  case "$v2_current" in
    */memory.current) v2_stat="${v2_current%/memory.current}/memory.stat" ;;
  esac
fi
emit_file memory.current "$v2_current"
emit_file memory.stat "$v2_stat"

v1_usage=""
v1_stat=""
set -- /sys/fs/cgroup/memory/memory.usage_in_bytes /sys/fs/cgroup/memory.usage_in_bytes
if [ -n "$v1_rel" ] && [ "$v1_rel" != "/" ]; then
  set -- /sys/fs/cgroup/memory/memory.usage_in_bytes \
    /sys/fs/cgroup/memory.usage_in_bytes \
    "/sys/fs/cgroup/memory${v1_rel}/memory.usage_in_bytes" \
    "/sys/fs/cgroup${v1_rel}/memory.usage_in_bytes"
fi
if v1_usage=$(first_readable "$root" "$@"); then
  case "$v1_usage" in
    */memory.usage_in_bytes) v1_stat="${v1_usage%/memory.usage_in_bytes}/memory.stat" ;;
  esac
fi
emit_file memory.usage_in_bytes "$v1_usage"
emit_file memory.stat.v1 "$v1_stat"

pid=""
comm=""
for proc_dir in "${root}/proc/"[0-9]*; do
  [ -r "${proc_dir}/comm" ] || continue
  proc_comm=$(tr -d '\n' <"${proc_dir}/comm" 2>/dev/null || true)
  if [ "$proc_comm" = "caesium" ]; then
    proc_pid=${proc_dir##*/}
    if [ -z "$pid" ] || [ "$proc_pid" -lt "$pid" ]; then
      pid=$proc_pid
      comm=$proc_comm
    fi
  fi
done
if [ -z "$pid" ] && [ -r "${root}/proc/1/status" ]; then
  pid=1
  if [ -r "${root}/proc/1/comm" ]; then
    comm=$(tr -d '\n' <"${root}/proc/1/comm" 2>/dev/null || true)
  fi
fi

printf '%s\n' "--- section proc.pid path=/proc/${pid}/status"
if [ -n "$pid" ]; then
  printf '%s\n' 'status=present'
  printf '%s\n' "$pid"
else
  printf '%s\n' 'status=absent'
fi
printf '%s\n' "--- section proc.comm path=/proc/${pid}/comm"
if [ -n "$comm" ]; then
  printf '%s\n' 'status=present'
  printf '%s\n' "$comm"
else
  printf '%s\n' 'status=absent'
fi
printf '%s\n' "--- section proc.status path=/proc/${pid}/status"
if [ -n "$pid" ] && [ -r "${root}/proc/${pid}/status" ]; then
  printf '%s\n' 'status=present'
  cat "${root}/proc/${pid}/status" 2>/dev/null
  printf '\n'
else
  printf '%s\n' 'status=absent'
fi

metrics_url="${CAESIUM_MEMORY_PROBE_METRICS_URL:-http://127.0.0.1:8080/metrics}"
if [ -n "${CAESIUM_MEMORY_PROBE_METRICS_FILE:-}" ]; then
  printf '%s\n' "--- section metrics path=${CAESIUM_MEMORY_PROBE_METRICS_FILE}"
  if [ -r "${CAESIUM_MEMORY_PROBE_METRICS_FILE}" ]; then
    printf '%s\n' 'status=present'
    cat "${CAESIUM_MEMORY_PROBE_METRICS_FILE}"
    printf '\n'
  else
    printf '%s\n' 'status=absent'
    printf '%s\n' 'metrics file missing'
  fi
else
  printf '%s\n' "--- section metrics path=${metrics_url}"
  if command -v wget >/dev/null 2>&1; then
    metrics_tmp=$(mktemp 2>/dev/null || true)
    if [ -n "$metrics_tmp" ] && wget -q -O "$metrics_tmp" "$metrics_url" 2>"${metrics_tmp}.err"; then
      printf '%s\n' 'status=present'
      cat "$metrics_tmp"
      printf '\n'
    else
      printf '%s\n' 'status=error'
      if [ -n "$metrics_tmp" ] && [ -r "${metrics_tmp}.err" ]; then
        cat "${metrics_tmp}.err"
      else
        printf '%s\n' 'wget failed'
      fi
      printf '\n'
    fi
    if [ -n "$metrics_tmp" ]; then
      rm -f "$metrics_tmp" "${metrics_tmp}.err"
    fi
  elif command -v curl >/dev/null 2>&1; then
    metrics_tmp=$(mktemp 2>/dev/null || true)
    if [ -n "$metrics_tmp" ] && curl -fsS --max-time 5 -o "$metrics_tmp" "$metrics_url" 2>"${metrics_tmp}.err"; then
      printf '%s\n' 'status=present'
      cat "$metrics_tmp"
      printf '\n'
    else
      printf '%s\n' 'status=error'
      if [ -n "$metrics_tmp" ] && [ -r "${metrics_tmp}.err" ]; then
        cat "${metrics_tmp}.err"
      else
        printf '%s\n' 'curl failed'
      fi
      printf '\n'
    fi
    if [ -n "$metrics_tmp" ]; then
      rm -f "$metrics_tmp" "${metrics_tmp}.err"
    fi
  else
    printf '%s\n' 'status=absent'
    printf '%s\n' 'metrics client absent: no wget or curl'
  fi
fi
printf '%s\n' '--- end'
"""


def parse_capture(text: str) -> dict:
    marker = "member-sample-format: 1"
    index = text.find(marker)
    if index < 0:
        return {
            "format_error": "probe format marker missing",
            "truncated": True,
            "preamble": text[:2000],
            "sections": {},
        }
    truncated = "\n--- end\n" not in text[index:] and not text[index:].rstrip().endswith("--- end")
    parts = re.split(r"(?m)^--- section ", text[index + len(marker):])
    sections = {}
    for part in parts[1:]:
        if part.lstrip().startswith("--- end"):
            continue
        header, _, rest = part.partition("\n")
        match = re.match(r"^(?P<name>\S+)\s+path=(?P<path>.*)$", header.strip())
        if not match:
            continue
        lines = rest.splitlines()
        while lines and lines[-1].strip() in ("", "--- end"):
            ended = lines[-1].strip() == "--- end"
            lines.pop()
            if ended:
                break
        status = "absent"
        payload = lines
        if lines and lines[0].startswith("status="):
            status = lines[0].split("=", 1)[1].strip()
            payload = lines[1:]
        sections[match.group("name")] = {
            "path": match.group("path"),
            "status": status,
            "body": "\n".join(payload).strip("\n"),
        }
    return {
        "format_error": "",
        "truncated": truncated,
        "preamble": text[:index][-500:],
        "sections": sections,
    }


def _present_section(sections: dict, name: str) -> dict | None:
    section = sections.get(name)
    if not section or section.get("status") != "present":
        return None
    return section


def _parse_u64(body: str) -> int:
    text = body.strip()
    if not re.fullmatch(r"\d+", text):
        raise ValueError(f"not an integer byte count: {text[:80]!r}")
    return int(text)


def _parse_stat(body: str) -> tuple[dict, list[str]]:
    values = {}
    errors = []
    for line in body.splitlines():
        if not line.strip():
            continue
        match = STAT_RE.match(line.strip())
        if not match:
            errors.append(line[:200])
            continue
        values[match.group(1)] = int(match.group(2))
    return values, errors


def parse_cgroup(sections: dict) -> dict:
    current = _present_section(sections, "memory.current")
    usage = _present_section(sections, "memory.usage_in_bytes")
    errors = []
    absent = []
    record = {
        "version": None,
        "memory_current_bytes": None,
        "memory_usage_bytes": None,
        "anon_bytes": None,
        "file_bytes": None,
        "v1_rss_bytes": None,
        "v1_cache_bytes": None,
        "stat_bytes": {},
        "stat_present": False,
        "absent": absent,
        "errors": errors,
        "source_paths": {},
    }
    if current is not None:
        stat = _present_section(sections, "memory.stat")
        record["version"] = "v2"
        record["source_paths"]["memory.current"] = current.get("path") or ""
        try:
            record["memory_current_bytes"] = _parse_u64(current.get("body") or "")
        except ValueError as exc:
            errors.append(f"memory.current: {exc}")
        if stat is None:
            record["stat_present"] = False
            absent.extend(["memory.stat", "memory.stat anon", "memory.stat file"])
            errors.append("cgroup v2 memory.stat absent")
        else:
            record["stat_present"] = True
            record["source_paths"]["memory.stat"] = stat.get("path") or ""
            parsed, stat_errors = _parse_stat(stat.get("body") or "")
            record["stat_bytes"] = parsed
            errors.extend(f"memory.stat: {item}" for item in stat_errors)
            if "anon" in parsed:
                record["anon_bytes"] = parsed["anon"]
            else:
                absent.append("memory.stat anon")
            if "file" in parsed:
                record["file_bytes"] = parsed["file"]
            else:
                absent.append("memory.stat file")
        return record
    if usage is not None:
        stat = _present_section(sections, "memory.stat.v1")
        record["version"] = "v1"
        record["source_paths"]["memory.usage_in_bytes"] = usage.get("path") or ""
        try:
            record["memory_usage_bytes"] = _parse_u64(usage.get("body") or "")
        except ValueError as exc:
            errors.append(f"memory.usage_in_bytes: {exc}")
        absent.extend(["memory.current"])
        if stat is None:
            absent.extend(["memory.stat", "memory.stat anon", "memory.stat file"])
            errors.append("cgroup v1 memory.stat absent")
        else:
            record["stat_present"] = True
            record["source_paths"]["memory.stat"] = stat.get("path") or ""
            parsed, stat_errors = _parse_stat(stat.get("body") or "")
            record["stat_bytes"] = parsed
            errors.extend(f"memory.stat: {item}" for item in stat_errors)
            record["v1_rss_bytes"] = parsed.get("rss")
            record["v1_cache_bytes"] = parsed.get("cache")
            if "anon" in parsed:
                record["anon_bytes"] = parsed["anon"]
            else:
                absent.append("memory.stat anon")
            if "file" in parsed:
                record["file_bytes"] = parsed["file"]
            else:
                absent.append("memory.stat file")
        return record
    for name in ("memory.current", "memory.stat", "memory.usage_in_bytes", "memory.stat.v1"):
        section = sections.get(name) or {}
        if section.get("status") == "error" and section.get("body"):
            errors.append(f"{name}: {section['body'][:300]}")
    absent.extend([
        "memory.current", "memory.stat", "memory.stat anon", "memory.stat file",
        "memory.usage_in_bytes",
    ])
    errors.append("cgroup memory files absent")
    return record


def parse_rss(sections: dict) -> dict:
    pid_section = _present_section(sections, "proc.pid")
    comm_section = _present_section(sections, "proc.comm")
    status_section = _present_section(sections, "proc.status")
    comm = (comm_section or {}).get("body", "").strip()
    pid = None
    errors = []
    if pid_section is not None:
        raw_pid = (pid_section.get("body") or "").strip()
        if re.fullmatch(r"\d+", raw_pid):
            pid = int(raw_pid)
        else:
            errors.append(f"pid unreadable: {raw_pid[:80]!r}")
    rss_kib = None
    if status_section is None:
        errors.append("process status absent")
    else:
        match = RSS_RE.search(status_section.get("body") or "")
        if not match:
            errors.append("VmRSS absent")
        else:
            rss_kib = int(match.group(1))
    return {
        "pid": pid,
        "comm": comm,
        "matches_caesium": comm == "caesium",
        "rss_kib": rss_kib,
        "rss_bytes": None if rss_kib is None else rss_kib * 1024,
        "source": None if pid is None else f"/proc/{pid}/status",
        "errors": errors,
    }


def _labels(text: str | None) -> dict:
    if not text:
        return {}
    return {match.group(1): match.group(2) for match in LABEL_RE.finditer(text)}


def _json_number(value: float):
    if math.isfinite(value) and value.is_integer() and abs(value) < 2**53:
        return int(value)
    return value


def parse_metrics_text(text: str) -> dict:
    samples: dict[str, list[dict]] = {}
    for line in text.splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        match = METRIC_RE.match(stripped)
        if not match:
            continue
        try:
            value = float(match.group("value"))
        except ValueError:
            continue
        if not math.isfinite(value):
            continue
        samples.setdefault(match.group("name"), []).append({
            "labels": _labels(match.group("labels")),
            "value": value,
        })
    return samples


def _series_value(samples: dict, name: str) -> dict:
    rows = samples.get(name) or []
    unlabeled = [row for row in rows if not row["labels"]]
    chosen = unlabeled or rows
    if not chosen:
        return {"series": name, "present": False}
    if len(chosen) > 1 and len({row["value"] for row in chosen}) > 1:
        return {"series": name, "present": False, "error": "duplicate samples"}
    return {"series": name, "present": True, "value": _json_number(chosen[0]["value"])}


def parse_go_metrics(section: dict | None) -> dict:
    absent = {
        key: {"series": name, "present": False} for key, name in GO_SERIES.items()
    }
    gc = {"series": GC_SERIES, "present": False}
    if section is None or section.get("status") != "present":
        status = "absent" if section is None else section.get("status") or "absent"
        error = ""
        if section is not None:
            error = (section.get("body") or "").strip()[:500]
        if not error and status != "present":
            error = "metrics scrape absent"
        return {
            "metrics_status": status,
            "metrics_error": error,
            "metrics_path": "" if section is None else section.get("path") or "",
            **absent,
            "gc_duration_seconds": gc,
            "absent_series": [item["series"] for item in (*absent.values(), gc)],
        }
    parsed = parse_metrics_text(section.get("body") or "")
    record = {
        "metrics_status": "present",
        "metrics_error": "",
        "metrics_path": section.get("path") or "",
    }
    missing = []
    for key, name in GO_SERIES.items():
        item = _series_value(parsed, name)
        record[key] = item
        if not item["present"]:
            missing.append(name)
    count = _series_value(parsed, GC_SERIES + "_count")
    total = _series_value(parsed, GC_SERIES + "_sum")
    quantiles = {}
    for row in parsed.get(GC_SERIES) or []:
        label = row["labels"].get("quantile")
        if label is not None:
            quantiles[label] = _json_number(row["value"])
    if count["present"] or total["present"] or quantiles:
        gc = {
            "series": GC_SERIES,
            "present": True,
            "count": count.get("value") if count["present"] else None,
            "sum": total.get("value") if total["present"] else None,
        }
        if quantiles:
            gc["quantiles"] = quantiles
    else:
        missing.append(GC_SERIES)
    record["gc_duration_seconds"] = gc
    record["absent_series"] = missing
    return record


def _reading_gaps(cgroup: dict, rss: dict, go: dict, truncated: bool) -> list[str]:
    gaps = []
    if cgroup.get("version") == "v2":
        cgroup_ok = all(isinstance(cgroup.get(key), int) for key in (
            "memory_current_bytes", "anon_bytes", "file_bytes"))
    elif cgroup.get("version") == "v1":
        cgroup_ok = isinstance(cgroup.get("memory_usage_bytes"), int) and bool(cgroup.get("stat_bytes"))
    else:
        cgroup_ok = False
    if not cgroup_ok:
        gaps.append("cgroup")
    if rss.get("matches_caesium") is not True or not isinstance(rss.get("rss_bytes"), int):
        gaps.append("process_rss")
    series_present = any((go.get(key) or {}).get("present") for key in (
        "heap_alloc_bytes", "heap_inuse_bytes", "last_gc_time_seconds", "gc_duration_seconds"))
    # A finished scrape may legitimately omit a series; record that absence.
    # A truncated scrape with no Go series never reached /metrics.
    if go.get("metrics_status") != "present" or (truncated and not series_present):
        gaps.append("go_metrics")
    return gaps


def build_sample(*, member: str, timestamp: str, reason: str, batch: int, lifecycle_id: str,
                 capture_text: str, capture_exit_code: int, capture_artifact: str,
                 source: str = SOURCE, timestamp_source: str = "host-utc") -> dict:
    parsed = parse_capture(capture_text)
    sections = parsed["sections"]
    cgroup = parse_cgroup(sections)
    rss = parse_rss(sections)
    metrics_section = sections.get("metrics")
    go_metrics = parse_go_metrics(metrics_section)
    gaps = (["cgroup", "process_rss", "go_metrics"] if parsed["format_error"]
            else _reading_gaps(cgroup, rss, go_metrics, bool(parsed["truncated"])))
    capture_error = parsed["format_error"]
    if capture_error:
        excerpt = (capture_text or "")[:500]
        if excerpt:
            capture_error = f"{capture_error}: {excerpt}"
    elif parsed["truncated"]:
        capture_error = "probe output truncated before end marker"
    return {
        "member": member,
        "timestamp": timestamp,
        "timestamp_source": timestamp_source,
        "source": source,
        "reason": reason,
        "batch": batch,
        "lifecycle_id": lifecycle_id,
        "memory_limit": MEMORY_LIMIT,
        "capture_exit_code": capture_exit_code,
        "capture_artifact": capture_artifact,
        "capture_truncated": bool(parsed["truncated"] or parsed["format_error"]),
        "capture_error": capture_error,
        "readings_ok": not gaps,
        "reading_gaps": gaps,
        "cgroup": cgroup,
        "process_rss": rss,
        "go": go_metrics,
    }


def append_jsonl(path: Path, sample: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    line = json.dumps(sample, sort_keys=True) + "\n"
    with path.open("a", encoding="utf-8") as handle:
        fcntl.flock(handle.fileno(), fcntl.LOCK_EX)
        handle.write(line)
        handle.flush()
        os.fsync(handle.fileno())


def load_samples(path: Path, lifecycle_id: str) -> list[dict]:
    if not path.is_file():
        return []
    samples = []
    for line in path.read_text(encoding="utf-8").splitlines():
        if not line.strip():
            continue
        item = json.loads(line)
        if item.get("lifecycle_id") == lifecycle_id:
            samples.append(item)
    return samples


def gap_reasons(samples: list[dict], batches: list[int], apply_batch: int | None) -> list[str]:
    reasons = []
    if not batches:
        reasons.append("no catalog-write batch was sampled")
    for batch in batches:
        for member in MEMBERS:
            rows = [sample for sample in samples
                    if sample.get("member") == member and sample.get("batch") == batch]
            if not rows:
                reasons.append(f"{member} batch {batch} has no memory sample")
            elif not any(sample.get("readings_ok") for sample in rows):
                reasons.append(f"{member} batch {batch} memory readings are incomplete")
    if apply_batch is not None:
        for member in MEMBERS:
            rows = [sample for sample in samples
                    if sample.get("member") == member and sample.get("batch") == apply_batch
                    and sample.get("reason") == "apply-error"]
            if not rows:
                reasons.append(f"{member} batch {apply_batch} has no post-apply-error memory sample")
    return reasons


def memory_summary(samples: list[dict], lifecycle_id: str, batches: list[int],
                   apply_batch: int | None) -> dict:
    reasons = gap_reasons(samples, batches, apply_batch)
    counts = {}
    for member in MEMBERS:
        counts[member] = sum(1 for sample in samples if sample.get("member") == member)
    return {
        "kind": "caesium-snapshot-memory-samples",
        "lifecycle_id": lifecycle_id,
        "memory_limit": MEMORY_LIMIT,
        "artifact": "cluster-logs/snapshot-memory-samples.jsonl",
        "batches": batches,
        "apply_error_batch": apply_batch,
        "sample_count": len(samples),
        "per_member_counts": counts,
        "gap": bool(reasons),
        "gap_detail": "; ".join(reasons),
        "samples": samples,
    }


def publish(*, samples: list[dict], lifecycle_id: str, batches: list[int],
            apply_batch: int | None, evidence) -> tuple[dict, int]:
    summary = memory_summary(samples, lifecycle_id, batches, apply_batch)
    if isinstance(evidence, dict):
        document = dict(evidence)
        document["memory_samples"] = summary
    else:
        document = {
            "kind": "caesium-snapshot-catch-up-evidence",
            "lifecycle_id": lifecycle_id,
            "memory_limit": MEMORY_LIMIT,
            "case_evidence": evidence,
            "memory_samples": summary,
        }
    return document, (2 if summary["gap"] else 0)


def _signal_group(pid: int, sig: int) -> None:
    try:
        os.killpg(pid, sig)
    except ProcessLookupError:
        pass


def _merge_captured(first: bytes | None, second: bytes | None) -> bytes:
    # CPython may return the same prefix from TimeoutExpired and the follow-up
    # communicate(). Keep one copy so a killed probe is not parsed twice.
    first = first or b""
    second = second or b""
    if second.startswith(first):
        return second
    if first.endswith(second):
        return first
    return first + second


def run_bounded(argv: list[str], stdin_text: str, timeout: int) -> tuple[int, str, str]:
    process = subprocess.Popen(
        argv,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        start_new_session=True,
    )
    try:
        stdout, stderr = process.communicate(stdin_text.encode(), timeout=timeout)
        return process.returncode or 0, stdout.decode(errors="replace"), stderr.decode(errors="replace")
    except subprocess.TimeoutExpired as exc:
        _signal_group(process.pid, signal.SIGTERM)
        try:
            rest_out, rest_err = process.communicate(timeout=1)
        except subprocess.TimeoutExpired:
            _signal_group(process.pid, signal.SIGKILL)
            rest_out, rest_err = process.communicate()
        stdout = _merge_captured(exc.stdout, rest_out)
        stderr = _merge_captured(exc.stderr, rest_err)
        stderr += f"\ncommand timed out after {timeout}s\n".encode()
        return 124, stdout.decode(errors="replace"), stderr.decode(errors="replace")


def _parse_batches(text: str) -> list[int]:
    batches = []
    for item in text.split(","):
        item = item.strip()
        if not item:
            continue
        if not re.fullmatch(r"\d+", item):
            raise ValueError(f"invalid batch {item!r}")
        batch = int(item)
        if batch > 18:
            raise ValueError(f"batch {batch} is outside 0..18")
        batches.append(batch)
    return batches


def _parse_optional_batch(text: str) -> int | None:
    if text is None or text == "":
        return None
    batches = _parse_batches(str(text))
    if len(batches) != 1:
        raise ValueError("apply batch must be a single integer")
    return batches[0]


def _relative_artifact(path: Path, root: Path | None) -> str:
    if root is None:
        return path.name
    try:
        return str(path.resolve().relative_to(root.resolve()))
    except ValueError:
        return path.name


def cmd_sample(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(prog="lifecycle-memory-sample.py sample")
    parser.add_argument("--member", required=True, choices=MEMBERS)
    parser.add_argument("--reason", required=True, choices=REASONS)
    parser.add_argument("--batch", required=True, type=int)
    parser.add_argument("--lifecycle-id", required=True)
    parser.add_argument("--jsonl", required=True, type=Path)
    parser.add_argument("--capture", required=True, type=Path)
    parser.add_argument("--artifact-root", type=Path)
    parser.add_argument("--timeout", type=int, default=12)
    parser.add_argument("--source", default=SOURCE)
    parser.add_argument("--timestamp", default="")
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args(argv)
    command = args.command
    if command and command[0] == "--":
        command = command[1:]
    if not command:
        parser.error("sample requires the collection command after --")
    if not 0 <= args.batch <= 18:
        parser.error("batch must be 0..18")
    if args.timeout < 1:
        parser.error("timeout must be positive")
    timestamp = args.timestamp or datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    try:
        code, stdout, stderr = run_bounded(command, PROBE_SCRIPT, args.timeout)
    except OSError as exc:
        code, stdout, stderr = 127, "", f"command could not start: {exc}\n"
    args.capture.parent.mkdir(parents=True, exist_ok=True)
    args.capture.write_text(stdout, encoding="utf-8")
    args.capture.with_suffix(args.capture.suffix + ".err").write_text(stderr, encoding="utf-8")
    capture_text = stdout
    if "member-sample-format: 1" not in stdout and stderr:
        capture_text = stdout + ("\n" if stdout else "") + stderr
    sample = build_sample(
        member=args.member, timestamp=timestamp, reason=args.reason, batch=args.batch,
        lifecycle_id=args.lifecycle_id, capture_text=capture_text, capture_exit_code=code,
        capture_artifact=_relative_artifact(args.capture, args.artifact_root), source=args.source,
    )
    append_jsonl(args.jsonl, sample)
    return 0


def _load_evidence(path: str):
    if not path:
        return None, ""
    file = Path(path)
    if not file.is_file():
        return None, "evidence file missing"
    try:
        return json.loads(file.read_text(encoding="utf-8")), ""
    except (OSError, json.JSONDecodeError) as exc:
        return None, str(exc)


def cmd_finish(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(prog="lifecycle-memory-sample.py finish")
    parser.add_argument("--lifecycle-id", required=True)
    parser.add_argument("--jsonl", required=True, type=Path)
    parser.add_argument("--dest", required=True, type=Path)
    parser.add_argument("--batches", default="")
    parser.add_argument("--apply-batch", default="")
    parser.add_argument("--evidence", default="")
    args = parser.parse_args(argv)
    batches = _parse_batches(args.batches)
    apply_batch = _parse_optional_batch(args.apply_batch)
    samples = load_samples(args.jsonl, args.lifecycle_id)
    evidence, evidence_error = _load_evidence(args.evidence)
    document, code = publish(
        samples=samples, lifecycle_id=args.lifecycle_id, batches=batches,
        apply_batch=apply_batch, evidence=evidence,
    )
    if evidence_error:
        document["case_evidence_error"] = evidence_error
    args.dest.parent.mkdir(parents=True, exist_ok=True)
    args.dest.write_text(json.dumps(document, indent=2) + "\n", encoding="utf-8")
    return code


def main(argv: list[str]) -> int:
    if len(argv) < 2 or argv[1] in ("-h", "--help"):
        sys.stderr.write(
            "usage: lifecycle-memory-sample.py emit-probe|sample|finish\n")
        return 2
    command = argv[1]
    if command == "emit-probe":
        sys.stdout.write(PROBE_SCRIPT)
        if not PROBE_SCRIPT.endswith("\n"):
            sys.stdout.write("\n")
        return 0
    if command == "sample":
        return cmd_sample(argv[2:])
    if command == "finish":
        return cmd_finish(argv[2:])
    sys.stderr.write(f"unknown command {command}\n")
    return 2


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv))
    except (ValueError, OSError) as exc:
        sys.stderr.write(f"{exc}\n")
        sys.exit(1)
