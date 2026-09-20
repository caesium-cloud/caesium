#!/usr/bin/env python3
"""Compare base vs candidate performance evidence. Fail closed.

E3 ships the comparator, not calibrated SLOs (those are E4). This program
never treats missing data, mismatched provenance, undersampled series, or a
statistically insignificant delta as equivalence. Correctness/completion
failures abort before any speed comparison.

Input is a JSON comparison document (schema_version 1) with `base` and
`candidate` sides, each carrying provenance, correctness, and metric
families (workloads, benchmarks, browser, bundle, system).

Exit status:
  0  conclusive non-regression: overall is faster or no_significant_difference
  1  usage or schema error
  2  fail-closed: correctness failed, instrumented image, or missing required data
  3  inconclusive (mismatched environments, undersampled, noisy)
  4  conclusive slower

Stdout is the JSON report. Human summary goes to stderr.
"""

from __future__ import annotations

import argparse
import json
import math
import re
import statistics
import sys
from pathlib import Path


SCHEMA_VERSION = 1
ALPHA = 0.05
DEFAULT_MIN_SAMPLES = 5
DEFAULT_MAX_CV = 0.30

# Keep in lockstep with ui/scripts/check-bundle-size.mjs.
DEFAULT_LARGEST_JS_RAW = 1_400_000
DEFAULT_LARGEST_JS_GZIP = 430_000
DEFAULT_TOTAL_RAW = 5_000_000
DEFAULT_TOTAL_GZIP = 1_600_000

ROUTE_ASSET_EXT = {
    ".js",
    ".css",
    ".wasm",
    ".woff",
    ".woff2",
    ".ttf",
    ".otf",
    ".eot",
    ".png",
    ".jpg",
    ".jpeg",
    ".gif",
    ".svg",
    ".webp",
    ".avif",
    ".ico",
}

REQUIRED_PROVENANCE = (
    "git_sha",
    "image_id",
    "image_ref",
    "platform",
    "go_version",
    "builder_image_id",
    "host_id",
    "catalog_sha256",
    "settings_sha256",
    "instrumented",
    "cli_digest",
    "toolchain_id",
)

# Fields that must be identical for a comparison to be meaningful. git_sha
# and image_id are expected to differ (that is the comparison).
PROVENANCE_MUST_MATCH = (
    "platform",
    "go_version",
    "builder_image_id",
    "host_id",
    "catalog_sha256",
    "settings_sha256",
    "toolchain_id",
)

LOWER_IS_BETTER_HINTS = (
    "ns_per_op",
    "bytes_per_op",
    "allocs_per_op",
    "duration",
    "latency",
    "p50",
    "p99",
    "seconds",
    "ms",
    "heap",
    "memory",
    "bytes",
    "readiness",
    "render",
)
HIGHER_IS_BETTER_HINTS = ("throughput", "rate", "ops_per_sec", "succeeded_per_second")

GO_BENCH_RE = re.compile(
    r"^(Benchmark\S+?)(?:-\d+)?\s+(\d+)\s+(\d+(?:\.\d+)?)\s+ns/op"
    r"(?:\s+(\d+(?:\.\d+)?)\s+B/op)?"
    r"(?:\s+(\d+(?:\.\d+)?)\s+allocs/op)?"
)


class CompareError(Exception):
    """Input is unusable; the caller must exit 1."""


# ---------------------------------------------------------------------------
# Stats
# ---------------------------------------------------------------------------


def _sorted(xs):
    return sorted(float(x) for x in xs)


def mean(xs):
    return statistics.fmean(xs) if xs else math.nan


def stdev(xs):
    if len(xs) <= 1:
        return 0.0
    return statistics.stdev(xs)


def percentile(xs, p):
    ys = _sorted(xs)
    if not ys:
        return math.nan
    if len(ys) == 1:
        return ys[0]
    k = (len(ys) - 1) * (p / 100.0)
    lo = int(math.floor(k))
    hi = int(math.ceil(k))
    if lo == hi:
        return ys[lo]
    return ys[lo] + (ys[hi] - ys[lo]) * (k - lo)


def cv(xs):
    m = mean(xs)
    if m == 0:
        return math.inf if stdev(xs) else 0.0
    return stdev(xs) / abs(m)


def distribution(xs):
    ys = [float(x) for x in xs]
    return {
        "n": len(ys),
        "mean": mean(ys) if ys else None,
        "stdev": stdev(ys) if ys else None,
        "min": min(ys) if ys else None,
        "max": max(ys) if ys else None,
        "p50": percentile(ys, 50) if ys else None,
        "p90": percentile(ys, 90) if ys else None,
        "p99": percentile(ys, 99) if ys else None,
        "cv": cv(ys) if ys else None,
    }


def _phi(z):
    return 0.5 * (1.0 + math.erf(z / math.sqrt(2.0)))


def mann_whitney_p(a, b):
    """Two-sided Mann-Whitney U p-value (normal approximation, tie-corrected).

    Returns (u, p_value). For n1=n2=0 this is undefined.
    """
    n1, n2 = len(a), len(b)
    if n1 == 0 or n2 == 0:
        return None, None
    combined = [(float(x), 0) for x in a] + [(float(y), 1) for y in b]
    combined.sort(key=lambda t: t[0])
    ranks = [0.0] * len(combined)
    ties_term = 0.0
    i = 0
    while i < len(combined):
        j = i + 1
        while j < len(combined) and combined[j][0] == combined[i][0]:
            j += 1
        avg = (i + 1 + j) / 2.0
        t = j - i
        if t > 1:
            ties_term += t * t * t - t
        for k in range(i, j):
            ranks[k] = avg
        i = j
    r1 = sum(rank for rank, (_, group) in zip(ranks, combined) if group == 0)
    u1 = r1 - n1 * (n1 + 1) / 2.0
    u2 = n1 * n2 - u1
    u = min(u1, u2)
    n = n1 + n2
    mean_u = n1 * n2 / 2.0
    var = n1 * n2 * (n + 1) / 12.0
    if n > 1 and ties_term:
        var -= n1 * n2 * ties_term / (12.0 * n * (n - 1))
    if var <= 0:
        # All values identical across both samples.
        return u, 1.0
    sigma = math.sqrt(var)
    z = (abs(u - mean_u) - 0.5) / sigma
    p = 2.0 * (1.0 - _phi(z))
    return u, min(1.0, max(0.0, p))


def lower_is_better(metric_id, explicit=None):
    if explicit is not None:
        return bool(explicit)
    name = metric_id.lower()
    if any(h in name for h in HIGHER_IS_BETTER_HINTS):
        return False
    if any(h in name for h in LOWER_IS_BETTER_HINTS):
        return True
    return True


# ---------------------------------------------------------------------------
# Document loading
# ---------------------------------------------------------------------------


def load_json(path):
    try:
        raw = Path(path).read_text()
    except OSError as err:
        raise CompareError(f"cannot read {path}: {err}") from err
    try:
        return json.loads(raw)
    except json.JSONDecodeError as err:
        raise CompareError(f"{path} is not JSON: {err}") from err


def coerce_samples(value):
    """Accept a list of numbers, or a list of dicts with `value` or a known key."""
    if value is None:
        return [], []
    if not isinstance(value, list):
        raise CompareError(f"samples must be a list, got {type(value).__name__}")
    samples = []
    outcomes = []
    for item in value:
        if isinstance(item, (int, float)) and not isinstance(item, bool):
            samples.append(float(item))
            outcomes.append("passed")
            continue
        if not isinstance(item, dict):
            raise CompareError(f"sample entries must be numbers or objects, got {type(item).__name__}")
        outcomes.append(str(item.get("outcome") or item.get("status") or "passed"))
        if "value" in item:
            samples.append(float(item["value"]))
        elif "ns_per_op" in item:
            samples.append(float(item["ns_per_op"]))
        elif "duration_seconds" in item:
            samples.append(float(item["duration_seconds"]))
        elif "ms" in item:
            samples.append(float(item["ms"]))
        elif "bytes" in item:
            samples.append(float(item["bytes"]))
        else:
            raise CompareError(f"sample object has no numeric value: {sorted(item)}")
    return samples, outcomes


def parse_go_bench_text(text):
    """Parse `go test -bench` output into {name: {ns_per_op: [...], ...}}."""
    benches = {}
    for line in text.splitlines():
        m = GO_BENCH_RE.match(line.strip())
        if not m:
            continue
        name, _n, ns, bop, allocs = m.groups()
        entry = benches.setdefault(
            name, {"ns_per_op": [], "bytes_per_op": [], "allocs_per_op": []}
        )
        entry["ns_per_op"].append(float(ns))
        if bop is not None:
            entry["bytes_per_op"].append(float(bop))
        if allocs is not None:
            entry["allocs_per_op"].append(float(allocs))
    return benches


def expand_side_metrics(side):
    """Flatten a side's families into {metric_id: {samples, outcomes, lower_is_better, phase, family}}."""
    metrics = {}

    def add(metric_id, samples_value, family, phase=None, hib=None):
        samples, outcomes = coerce_samples(samples_value)
        metrics[metric_id] = {
            "samples": samples,
            "outcomes": outcomes,
            "family": family,
            "phase": phase,
            "lower_is_better": lower_is_better(metric_id, hib),
        }

    workloads = side.get("workloads") or {}
    if not isinstance(workloads, dict):
        raise CompareError("workloads must be an object")
    for name, body in workloads.items():
        if not isinstance(body, dict):
            raise CompareError(f"workload {name} must be an object")
        phase = body.get("phase")
        if "samples" in body:
            add(f"workload.{name}.duration_seconds", body["samples"], "workload", phase)
        for key in ("duration_seconds", "p50_seconds", "p99_seconds"):
            if key in body and key != "samples":
                add(f"workload.{name}.{key}", body[key], "workload", phase)
        for metric_name, series in (body.get("metrics") or {}).items():
            add(f"workload.{name}.{metric_name}", series, "workload", phase)

    benches = side.get("benchmarks") or {}
    if isinstance(benches, str):
        benches = parse_go_bench_text(benches)
    if not isinstance(benches, dict):
        raise CompareError("benchmarks must be an object or go test -bench text")
    for name, body in benches.items():
        if isinstance(body, list) or isinstance(body, (int, float)):
            add(f"bench.{name}.ns_per_op", body, "benchmark")
            continue
        if not isinstance(body, dict):
            raise CompareError(f"benchmark {name} must be an object or sample list")
        for key in ("ns_per_op", "bytes_per_op", "allocs_per_op"):
            if key in body:
                add(f"bench.{name}.{key}", body[key], "benchmark")
        if "samples" in body:
            add(f"bench.{name}.ns_per_op", body["samples"], "benchmark")

    browser = side.get("browser") or {}
    if not isinstance(browser, dict):
        raise CompareError("browser must be an object")
    for key, body in browser.items():
        if key == "correctness":
            continue
        if (
            isinstance(body, dict)
            and "samples" not in body
            and body
            and all(isinstance(v, (list, int, float)) for v in body.values())
        ):
            for route, series in body.items():
                add(f"browser.{key}.{route}", series if isinstance(series, list) else [series], "browser")
            continue
        add(
            f"browser.{key}",
            body if not isinstance(body, dict) else body.get("samples", body),
            "browser",
        )

    bundle = side.get("bundle") or {}
    if bundle:
        if not isinstance(bundle, dict):
            raise CompareError("bundle must be an object")
        for key in (
            "largest_js_raw_bytes",
            "largest_js_gzip_bytes",
            "total_raw_bytes",
            "total_gzip_bytes",
        ):
            if key in bundle:
                raw = bundle[key]
                add(f"bundle.{key}", raw if isinstance(raw, list) else [raw], "bundle")

    system = side.get("system") or {}
    if not isinstance(system, dict):
        raise CompareError("system must be an object")
    for name, series in system.items():
        add(f"system.{name}", series if not isinstance(series, dict) else series.get("samples", series), "system")

    return metrics


# ---------------------------------------------------------------------------
# Bundle directory check (Python twin of check-bundle-size.mjs)
# ---------------------------------------------------------------------------


def iter_route_assets(dist_assets_dir):
    root = Path(dist_assets_dir)
    if not root.is_dir():
        raise CompareError(f"bundle assets dir is not a directory: {root}")
    for path in root.rglob("*"):
        if not path.is_file():
            continue
        if path.name.endswith(".map"):
            continue
        if path.suffix.lower() not in ROUTE_ASSET_EXT:
            continue
        yield path


def gzip_size(path):
    import gzip as gzip_mod

    # compresslevel=6 matches zlib's default (what node:zlib gzipSync uses).
    data = Path(path).read_bytes()
    return len(gzip_mod.compress(data, compresslevel=6))


def evaluate_bundle_dir(
    dist_assets_dir,
    max_raw=DEFAULT_LARGEST_JS_RAW,
    max_gzip=DEFAULT_LARGEST_JS_GZIP,
    total_raw=DEFAULT_TOTAL_RAW,
    total_gzip=DEFAULT_TOTAL_GZIP,
):
    """Apply largest-chunk AND total-route-asset budgets. Fail closed."""
    files = list(iter_route_assets(dist_assets_dir))
    js_files = [p for p in files if p.suffix.lower() == ".js"]
    errors = []
    if not js_files:
        return {
            "ok": False,
            "errors": [f"No JS assets found in {dist_assets_dir}."],
            "largest_js_raw_bytes": 0,
            "largest_js_gzip_bytes": 0,
            "total_raw_bytes": 0,
            "total_gzip_bytes": 0,
            "file_count": 0,
        }
    js_stats = []
    for path in js_files:
        js_stats.append((path, path.stat().st_size, gzip_size(path)))
    largest_raw = max(js_stats, key=lambda t: t[1])
    largest_gzip = max(js_stats, key=lambda t: t[2])
    total_raw_bytes = 0
    total_gzip_bytes = 0
    for path in files:
        total_raw_bytes += path.stat().st_size
        total_gzip_bytes += gzip_size(path)
    if largest_raw[1] > max_raw:
        errors.append(
            f"Raw chunk budget exceeded: {largest_raw[0].name} is {largest_raw[1]} bytes (limit {max_raw})."
        )
    if largest_gzip[2] > max_gzip:
        errors.append(
            f"Gzip chunk budget exceeded: {largest_gzip[0].name} is {largest_gzip[2]} bytes (limit {max_gzip})."
        )
    if total_raw_bytes > total_raw:
        errors.append(
            f"Total route-asset raw budget exceeded: {total_raw_bytes} bytes across {len(files)} files "
            f"(limit {total_raw}). Splitting a large chunk cannot evade this."
        )
    if total_gzip_bytes > total_gzip:
        errors.append(
            f"Total route-asset gzip budget exceeded: {total_gzip_bytes} bytes across {len(files)} files "
            f"(limit {total_gzip}). Splitting a large chunk cannot evade this."
        )
    return {
        "ok": not errors,
        "errors": errors,
        "largest_js_raw_bytes": largest_raw[1],
        "largest_js_gzip_bytes": largest_gzip[2],
        "total_raw_bytes": total_raw_bytes,
        "total_gzip_bytes": total_gzip_bytes,
        "file_count": len(files),
        "budgets": {
            "largest_js_raw_bytes": max_raw,
            "largest_js_gzip_bytes": max_gzip,
            "total_raw_bytes": total_raw,
            "total_gzip_bytes": total_gzip,
        },
    }


# ---------------------------------------------------------------------------
# Comparison
# ---------------------------------------------------------------------------


def side_correctness(side, metrics):
    failures = []
    declared = side.get("correctness") or {}
    if declared.get("ok") is False:
        failures.extend(str(x) for x in (declared.get("failures") or ["correctness.ok is false"]))
    elif declared.get("failures"):
        failures.extend(str(x) for x in declared["failures"])
    for metric_id, body in metrics.items():
        for i, outcome in enumerate(body["outcomes"]):
            if outcome not in ("passed", "pass", "ok", "succeeded"):
                failures.append(f"{metric_id}[{i}] outcome={outcome}")
    return failures


def provenance_issues(base, candidate):
    issues = []
    missing = []
    mismatched = []
    instrumented = []
    for label, side in (("base", base), ("candidate", candidate)):
        prov = side.get("provenance")
        if not isinstance(prov, dict) or not prov:
            missing.append(f"{label}.provenance is missing")
            continue
        for field in REQUIRED_PROVENANCE:
            if field not in prov or prov[field] in (None, ""):
                missing.append(f"{label}.provenance.{field} is missing")
        if prov.get("instrumented") is True:
            instrumented.append(f"{label} image is instrumented (GOCOVERDIR/testfault coverage must not leak into performance)")
        elif str(prov.get("instrumented")).lower() in {"1", "yes", "true"}:
            instrumented.append(f"{label} image is instrumented")
    base_p = base.get("provenance") if isinstance(base.get("provenance"), dict) else {}
    cand_p = candidate.get("provenance") if isinstance(candidate.get("provenance"), dict) else {}
    for field in PROVENANCE_MUST_MATCH:
        if field not in base_p or field not in cand_p:
            continue
        if base_p.get(field) != cand_p.get(field):
            mismatched.append(
                f"provenance.{field} differs: base={base_p.get(field)!r} candidate={cand_p.get(field)!r}"
            )
    return {"missing": missing, "mismatched": mismatched, "instrumented": instrumented}


def compare_metric(metric_id, left, right, min_samples, max_cv, alpha):
    result = {
        "id": metric_id,
        "family": left.get("family") or right.get("family"),
        "phase": left.get("phase") or right.get("phase"),
        "verdict": "inconclusive",
        "reasons": [],
        "base": distribution(left.get("samples") or []),
        "candidate": distribution(right.get("samples") or []),
        "delta_pct": None,
        "p_value": None,
        "u_statistic": None,
        "lower_is_better": left.get("lower_is_better", True),
    }
    a = list(left.get("samples") or [])
    b = list(right.get("samples") or [])
    if not a or not b:
        result["reasons"].append("missing data: one or both sides have no samples")
        result["verdict"] = "fail"
        return result
    if len(a) < min_samples or len(b) < min_samples:
        result["reasons"].append(
            f"undersampled: base n={len(a)} candidate n={len(b)} min_samples={min_samples}"
        )
        result["verdict"] = "inconclusive"
        return result

    u, p = mann_whitney_p(a, b)
    result["u_statistic"] = u
    result["p_value"] = p
    base_mean = mean(a)
    cand_mean = mean(b)
    if base_mean == 0:
        result["delta_pct"] = None if cand_mean == 0 else math.inf
    else:
        result["delta_pct"] = (cand_mean - base_mean) / abs(base_mean) * 100.0

    hib = result["lower_is_better"]
    # Direction from means; MWU only decides significance.
    if hib:
        faster = cand_mean < base_mean
        slower = cand_mean > base_mean
    else:
        faster = cand_mean > base_mean
        slower = cand_mean < base_mean

    noisy = cv(a) > max_cv or cv(b) > max_cv
    significant = p is not None and p < alpha

    if significant and faster:
        result["verdict"] = "faster"
        result["reasons"].append(f"candidate mean is lower-is-better={hib} with p={p:.4g} < {alpha}")
        return result
    if significant and slower:
        result["verdict"] = "slower"
        result["reasons"].append(f"candidate mean is worse with p={p:.4g} < {alpha}")
        return result

    # Not significant. Never call this equivalent: insignificance is not
    # proof the two distributions match within a bound (that is E4).
    if noisy:
        result["verdict"] = "inconclusive"
        result["reasons"].append(
            f"noisy: base cv={cv(a):.3f} candidate cv={cv(b):.3f} max_cv={max_cv}; "
            "an insignificant noisy delta is not equivalence"
        )
        return result
    result["verdict"] = "no_significant_difference"
    result["reasons"].append(
        f"difference is not significant (p={p:.4g} >= {alpha}); this is not equivalence"
    )
    return result


def rollup(metric_results, fail_reasons, inconclusive_reasons):
    if fail_reasons:
        return "fail", fail_reasons
    verdicts = [m["verdict"] for m in metric_results]
    if "fail" in verdicts:
        return "fail", [m["id"] + ": " + "; ".join(m["reasons"]) for m in metric_results if m["verdict"] == "fail"]
    if "slower" in verdicts:
        return "slower", [m["id"] for m in metric_results if m["verdict"] == "slower"]
    if inconclusive_reasons and not metric_results:
        return "inconclusive", inconclusive_reasons
    if "faster" in verdicts:
        extra = inconclusive_reasons[:]
        extra.extend(m["id"] for m in metric_results if m["verdict"] == "faster")
        return "faster", extra
    if all(v == "no_significant_difference" for v in verdicts) and verdicts and not inconclusive_reasons:
        return "no_significant_difference", [
            "every comparable metric is insignificant; this is not equivalence"
        ]
    reasons = list(inconclusive_reasons)
    reasons.extend(
        m["id"] + ": " + "; ".join(m["reasons"])
        for m in metric_results
        if m["verdict"] == "inconclusive"
    )
    if not reasons:
        reasons = ["no comparable metrics"]
    return "inconclusive", reasons


def benchstat_text(metric_results):
    lines = [
        "name                                          old mean        new mean        delta",
        "-----------------------------------------------------------------------------------",
    ]
    rows = [m for m in metric_results if m["family"] in {"benchmark", "workload", "browser", "system", "bundle"}]
    if not rows:
        return "no comparable metrics\n"
    for m in rows:
        old = m["base"]["mean"]
        new = m["candidate"]["mean"]
        delta = m["delta_pct"]
        verdict = m["verdict"]
        old_s = "n/a" if old is None else f"{old:.6g}"
        new_s = "n/a" if new is None else f"{new:.6g}"
        if delta is None:
            delta_s = "~"
        elif math.isinf(delta):
            delta_s = "inf"
        else:
            delta_s = f"{delta:+.2f}%"
        if verdict == "no_significant_difference":
            delta_s = f"{delta_s}  ~"
        mark = {
            "faster": "faster",
            "slower": "slower",
            "no_significant_difference": "n.s. (not equivalent)",
            "inconclusive": "inconclusive",
            "fail": "fail",
        }.get(verdict, verdict)
        p = m["p_value"]
        p_s = "" if p is None else f"  p={p:.3g}"
        n = f"  n={m['base']['n']}+{m['candidate']['n']}"
        lines.append(f"{m['id']:<45} {old_s:>14} {new_s:>14} {delta_s:>10}  {mark}{p_s}{n}")
    lines.append("")
    lines.append("Insignificance is not equivalence. Missing/undersampled/noisy series are not a pass.")
    return "\n".join(lines) + "\n"


def compare(doc, min_samples=DEFAULT_MIN_SAMPLES, max_cv=DEFAULT_MAX_CV, alpha=ALPHA):
    if not isinstance(doc, dict):
        raise CompareError("comparison document must be an object")
    if doc.get("schema_version") != SCHEMA_VERSION:
        raise CompareError(
            f"schema_version={doc.get('schema_version')!r}, this comparator understands {SCHEMA_VERSION}"
        )
    base = doc.get("base")
    candidate = doc.get("candidate")
    if not isinstance(base, dict) or not isinstance(candidate, dict):
        raise CompareError("document must contain base and candidate objects")

    fail_reasons = []
    inconclusive_reasons = []

    prov = provenance_issues(base, candidate)
    if prov["missing"]:
        fail_reasons.extend(prov["missing"])
    if prov["instrumented"]:
        fail_reasons.extend(prov["instrumented"])
    if prov["mismatched"]:
        inconclusive_reasons.extend(prov["mismatched"])

    base_metrics = expand_side_metrics(base)
    cand_metrics = expand_side_metrics(candidate)

    base_fail = side_correctness(base, base_metrics)
    cand_fail = side_correctness(candidate, cand_metrics)
    if base_fail:
        fail_reasons.extend(f"base correctness: {x}" for x in base_fail)
    if cand_fail:
        fail_reasons.extend(f"candidate correctness: {x}" for x in cand_fail)

    ids = sorted(set(base_metrics) | set(cand_metrics))
    metric_results = []
    speed_compared = False

    if fail_reasons:
        overall, reasons = rollup([], fail_reasons, inconclusive_reasons)
        report = {
            "schema_version": SCHEMA_VERSION,
            "overall": overall,
            "reasons": reasons,
            "speed_compared": False,
            "metrics": [],
            "benchstat": "speed not compared: correctness/provenance failed closed\n",
            "provenance": prov,
            "min_samples": min_samples,
            "max_cv": max_cv,
            "alpha": alpha,
        }
        return report

    for metric_id in ids:
        left = base_metrics.get(metric_id)
        right = cand_metrics.get(metric_id)
        if left is None or right is None:
            metric_results.append(
                {
                    "id": metric_id,
                    "family": (left or right or {}).get("family"),
                    "phase": (left or right or {}).get("phase"),
                    "verdict": "fail",
                    "reasons": ["missing data: metric is not present on both sides"],
                    "base": distribution((left or {}).get("samples") or []),
                    "candidate": distribution((right or {}).get("samples") or []),
                    "delta_pct": None,
                    "p_value": None,
                    "u_statistic": None,
                    "lower_is_better": True,
                }
            )
            continue
        if left.get("phase") != right.get("phase"):
            metric_results.append(
                {
                    "id": metric_id,
                    "family": left.get("family"),
                    "phase": None,
                    "verdict": "fail",
                    "reasons": [
                        f"missing data: cold/warm phase mismatch base={left.get('phase')!r} "
                        f"candidate={right.get('phase')!r}"
                    ],
                    "base": distribution(left.get("samples") or []),
                    "candidate": distribution(right.get("samples") or []),
                    "delta_pct": None,
                    "p_value": None,
                    "u_statistic": None,
                    "lower_is_better": left.get("lower_is_better", True),
                }
            )
            continue
        metric_results.append(compare_metric(metric_id, left, right, min_samples, max_cv, alpha))
        speed_compared = True

    overall, reasons = rollup(metric_results, fail_reasons, inconclusive_reasons)
    if overall == "equivalent":
        overall = "no_significant_difference"
        reasons.append("equivalent is forbidden; coerced to no_significant_difference")
    report = {
        "schema_version": SCHEMA_VERSION,
        "overall": overall,
        "reasons": reasons,
        "speed_compared": speed_compared,
        "metrics": metric_results,
        "benchstat": benchstat_text(metric_results),
        "provenance": prov,
        "min_samples": min_samples,
        "max_cv": max_cv,
        "alpha": alpha,
    }
    return report


EXIT_BY_OVERALL = {
    "faster": 0,
    "no_significant_difference": 0,
    "fail": 2,
    "inconclusive": 3,
    "slower": 4,
}


def merge_sides(base_path, candidate_path):
    base = load_json(base_path)
    candidate = load_json(candidate_path)
    if "base" in base and "candidate" not in base:
        base_side = base["base"]
    elif "provenance" in base or "workloads" in base or "benchmarks" in base:
        base_side = base
    else:
        raise CompareError(f"{base_path} does not look like a side or comparison document")
    if "candidate" in candidate and "base" not in candidate:
        cand_side = candidate["candidate"]
    elif "provenance" in candidate or "workloads" in candidate or "benchmarks" in candidate:
        cand_side = candidate
    else:
        raise CompareError(f"{candidate_path} does not look like a side or comparison document")
    return {"schema_version": SCHEMA_VERSION, "base": base_side, "candidate": cand_side}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("input", nargs="?", help="comparison document JSON")
    parser.add_argument("--input", dest="input_flag", help="comparison document JSON")
    parser.add_argument("--base", help="base side JSON (used with --candidate)")
    parser.add_argument("--candidate", help="candidate side JSON (used with --base)")
    parser.add_argument("--output", "-o", help="write the JSON report to this path as well as stdout")
    parser.add_argument("--min-samples", type=int, default=DEFAULT_MIN_SAMPLES)
    parser.add_argument("--max-cv", type=float, default=DEFAULT_MAX_CV)
    parser.add_argument("--alpha", type=float, default=ALPHA)
    parser.add_argument(
        "--bundle-dir",
        help="evaluate a dist/assets directory against largest-chunk AND total-route-asset budgets",
    )
    args = parser.parse_args(argv)

    if args.bundle_dir:
        result = evaluate_bundle_dir(args.bundle_dir)
        json.dump(result, sys.stdout, indent=2)
        sys.stdout.write("\n")
        return 0 if result["ok"] else 2

    try:
        if args.base or args.candidate:
            if not (args.base and args.candidate):
                raise CompareError("--base and --candidate must be supplied together")
            doc = merge_sides(args.base, args.candidate)
        else:
            path = args.input_flag or args.input
            if not path:
                raise CompareError("supply a comparison document or --base and --candidate")
            doc = load_json(path)
            if "base" not in doc or "candidate" not in doc:
                raise CompareError("comparison document must contain base and candidate")
        report = compare(
            doc, min_samples=args.min_samples, max_cv=args.max_cv, alpha=args.alpha
        )
    except CompareError as err:
        sys.stderr.write(f"error: {err}\n")
        return 1

    encoded = json.dumps(report, indent=2) + "\n"
    sys.stdout.write(encoded)
    if args.output:
        Path(args.output).write_text(encoded)
    sys.stderr.write(report["benchstat"])
    sys.stderr.write(f"overall={report['overall']} speed_compared={report['speed_compared']}\n")
    for reason in report["reasons"]:
        sys.stderr.write(f"  {reason}\n")
    return EXIT_BY_OVERALL.get(report["overall"], 2)


if __name__ == "__main__":
    sys.exit(main())
