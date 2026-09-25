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
import hashlib
import json
import math
import os
import re
import shutil
import statistics
import subprocess
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

MJS = Path(__file__).resolve().parents[1] / "ui/scripts/check-bundle-size.mjs"

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
# and image_id are expected to differ (that is the comparison). Per-SHA
# builder image IDs also differ; go_version is what proves the toolchain.
PROVENANCE_MUST_MATCH = (
    "platform",
    "go_version",
    "host_id",
    "catalog_sha256",
    "settings_sha256",
)

BENCHMARK_HARNESS_FILES = (
    "internal/run/owner_benchmark_test.go",
    "internal/run/recovery_benchmark_test.go",
)
BENCHMARK_REQUIRED_HELPERS = ("internal/run/owner_state_test.go",)

# Exact last-component / whole-token names. Substring matches are forbidden:
# "rate" must not make error_rate/generate/migrate higher-is-better.
LOWER_IS_BETTER_NAMES = {
    "ns_per_op",
    "bytes_per_op",
    "allocs_per_op",
    "duration_seconds",
    "p50_seconds",
    "p99_seconds",
    "route_readiness_ms",
    "action_to_render_ms",
    "long_session_heap_bytes",
    "largest_js_raw_bytes",
    "largest_js_gzip_bytes",
    "total_raw_bytes",
    "total_gzip_bytes",
    "cpu_pct",
    "error_rate",
    "failure_rate",
    "drop_rate",
    "ms",
    "seconds",
    "bytes",
}
HIGHER_IS_BETTER_NAMES = {
    "throughput",
    "ops_per_sec",
    "succeeded_per_second",
}

REQUIRED_BROWSER_SERIES = (
    "browser.route_readiness_ms./jobs.live",
    "browser.route_readiness_ms./triggers.live",
    "browser.route_readiness_ms./system.live",
    "browser.route_readiness_ms./jobdefs.live",
    "browser.action_to_render_ms.live",
    "browser.long_session_heap_bytes.live",
)

BUNDLE_KEYS = (
    "largest_js_raw_bytes",
    "largest_js_gzip_bytes",
    "total_raw_bytes",
    "total_gzip_bytes",
)

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


def mann_whitney(a, b):
    """Two-sided Mann-Whitney U (normal approximation, tie-corrected).

    Returns (u1, u2, p). u1 is the base-sample statistic (large when base
    values tend to exceed candidate values). Direction of a change must be
    taken from u1 vs u2, not from the means.
    """
    n1, n2 = len(a), len(b)
    if n1 == 0 or n2 == 0:
        return None, None, None
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
    n = n1 + n2
    mean_u = n1 * n2 / 2.0
    var = n1 * n2 * (n + 1) / 12.0
    if n > 1 and ties_term:
        var -= n1 * n2 * ties_term / (12.0 * n * (n - 1))
    if var <= 0:
        return u1, u2, 1.0
    sigma = math.sqrt(var)
    u = min(u1, u2)
    z = (abs(u - mean_u) - 0.5) / sigma
    p = 2.0 * (1.0 - _phi(z))
    return u1, u2, min(1.0, max(0.0, p))


def hodges_lehmann(a, b):
    """Median of all pairwise (candidate - base) differences."""
    diffs = [float(y) - float(x) for x in a for y in b]
    return percentile(diffs, 50) if diffs else math.nan


def lower_is_better(metric_id, explicit=None):
    """Whole-token direction. Unknown names raise CompareError (never guess)."""
    if explicit is not None:
        return bool(explicit)
    for part in reversed(str(metric_id).lower().split(".")):
        token = part.strip("/")
        if token in LOWER_IS_BETTER_NAMES:
            return True
        if token in HIGHER_IS_BETTER_NAMES:
            return False
    raise CompareError(
        f"unknown metric direction for {metric_id!r}: set lower_is_better explicitly "
        "(substring hints such as 'rate' are forbidden)"
    )


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


def browser_metric_id(metric, route="", kind=""):
    parts = ["browser", metric]
    if route:
        parts.append(route)
    if kind:
        parts.append(kind)
    return ".".join(parts)


def parse_browser_series_key(key):
    raw = str(key)
    if "|" in raw:
        route, kind = raw.split("|", 1)
        return route, kind
    if raw in {"live", "synthetic"}:
        return "", raw
    return raw, "live"


def expand_side_metrics(side):
    """Flatten a side's families into {metric_id: {samples, outcomes, lower_is_better, phase, family}}.

    Bundle byte counts are NOT expanded: they are a deterministic budget/delta,
    never a Mann-Whitney series of n=1.
    """
    metrics = {}

    def add(metric_id, samples_value, family, phase=None, hib=None):
        samples, outcomes = coerce_samples(samples_value)
        try:
            direction = lower_is_better(metric_id, hib)
        except CompareError as err:
            metrics[metric_id] = {
                "samples": samples,
                "outcomes": outcomes,
                "family": family,
                "phase": phase,
                "lower_is_better": None,
                "direction_error": str(err),
            }
            return
        metrics[metric_id] = {
            "samples": samples,
            "outcomes": outcomes,
            "family": family,
            "phase": phase,
            "lower_is_better": direction,
        }

    workloads = side.get("workloads") or {}
    if not isinstance(workloads, dict):
        raise CompareError("workloads must be an object")
    for name, body in workloads.items():
        if not isinstance(body, dict):
            raise CompareError(f"workload {name} must be an object")
        phase = body.get("phase")
        hib = body.get("lower_is_better")
        if "samples" in body:
            add(f"workload.{name}.duration_seconds", body["samples"], "workload", phase, hib)
        for key in ("duration_seconds", "p50_seconds", "p99_seconds"):
            if key in body and key != "samples":
                add(f"workload.{name}.{key}", body[key], "workload", phase, hib)
        for metric_name, series in (body.get("metrics") or {}).items():
            series_hib = hib
            series_val = series
            if isinstance(series, dict) and "samples" in series:
                series_hib = series.get("lower_is_better", hib)
                series_val = series["samples"]
            add(f"workload.{name}.{metric_name}", series_val, "workload", phase, series_hib)

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
            for series_key, series in body.items():
                route, kind = parse_browser_series_key(series_key)
                add(
                    browser_metric_id(key, route, kind),
                    series if isinstance(series, list) else [series],
                    "browser",
                )
            continue
        kind = "live"
        if isinstance(body, dict):
            kind = body.get("kind") or "live"
            samples_value = body.get("samples", body)
            route = body.get("route") or ""
            add(browser_metric_id(key, route, kind), samples_value, "browser")
        else:
            add(browser_metric_id(key, "", "live"), body, "browser")

    system = side.get("system") or {}
    if not isinstance(system, dict):
        raise CompareError("system must be an object")
    for name, series in system.items():
        hib = None
        samples_value = series
        if isinstance(series, dict):
            hib = series.get("lower_is_better")
            samples_value = series.get("samples", series)
        add(f"system.{name}", samples_value, "system", hib=hib)

    return metrics


# ---------------------------------------------------------------------------
# Bundle directory check — always the node script so gzip matches CI.
# ---------------------------------------------------------------------------


def evaluate_bundle_dir(dist_assets_dir, env=None):
    """Run ui/scripts/check-bundle-size.mjs --json. No Python gzip twin."""
    node = shutil.which("node")
    if not node:
        raise CompareError("node is required to evaluate bundle budgets (must match check-bundle-size.mjs)")
    if not MJS.is_file():
        raise CompareError(f"missing {MJS}")
    merged = os.environ.copy()
    if env:
        merged.update(env)
    proc = subprocess.run(
        [node, str(MJS), "--dist", str(dist_assets_dir), "--json"],
        capture_output=True,
        text=True,
        env=merged,
    )
    if not proc.stdout.strip():
        raise CompareError(
            f"check-bundle-size.mjs produced no JSON (exit {proc.returncode}): {proc.stderr.strip()}"
        )
    try:
        payload = json.loads(proc.stdout)
    except json.JSONDecodeError as err:
        raise CompareError(f"check-bundle-size.mjs stdout is not JSON: {err}") from err
    payload.setdefault("ok", proc.returncode == 0)
    payload.setdefault("errors", [])
    return payload


def bundle_failures(bundle, label):
    if not isinstance(bundle, dict) or not bundle:
        return [f"{label} bundle evidence is missing"]
    failures = []
    if bundle.get("ok") is False:
        failures.extend(str(x) for x in (bundle.get("errors") or ["bundle.ok is false"]))
    elif bundle.get("errors"):
        failures.extend(str(x) for x in bundle["errors"])
    return failures


def compare_bundle(base_bundle, candidate_bundle):
    """Deterministic budget/delta. Never Mann-Whitney on n=1 byte counts."""
    result = {
        "family": "bundle",
        "verdict": "fail",
        "reasons": [],
        "base": {},
        "candidate": {},
        "delta_pct": {},
    }
    if not isinstance(base_bundle, dict) or not base_bundle:
        result["reasons"].append("missing data: base bundle")
        return result
    if not isinstance(candidate_bundle, dict) or not candidate_bundle:
        result["reasons"].append("missing data: candidate bundle")
        return result
    for key in BUNDLE_KEYS:
        if key in base_bundle:
            result["base"][key] = base_bundle[key]
        if key in candidate_bundle:
            result["candidate"][key] = candidate_bundle[key]
        if key in base_bundle and key in candidate_bundle:
            old, new = base_bundle[key], candidate_bundle[key]
            if isinstance(old, (int, float)) and isinstance(new, (int, float)) and old:
                result["delta_pct"][key] = (new - old) / abs(old) * 100.0
    reasons = []
    reasons.extend(f"base: {e}" for e in (base_bundle.get("errors") or []))
    reasons.extend(f"candidate: {e}" for e in (candidate_bundle.get("errors") or []))
    if base_bundle.get("ok") is False or candidate_bundle.get("ok") is False or reasons:
        result["reasons"] = reasons or ["bundle over budget"]
        result["verdict"] = "fail"
        return result
    result["verdict"] = "no_significant_difference"
    result["reasons"] = ["bundle is a deterministic budget/delta, not a sampled equivalence claim"]
    return result


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
    return {"missing": missing, "mismatched": mismatched, "instrumented": instrumented, "invalid": []}


def benchmark_harness_issues(doc, base, candidate):
    """Check that measured benchmarks share one declared harness and image pair."""
    manifest = doc.get("benchmark_harness")
    if not isinstance(manifest, dict):
        return ["benchmark_harness manifest is missing or malformed"]

    issues = []
    base_p = base.get("provenance") if isinstance(base.get("provenance"), dict) else {}
    cand_p = candidate.get("provenance") if isinstance(candidate.get("provenance"), dict) else {}

    def same(body, field, expected, prefix):
        actual = body.get(field)
        if actual is None or actual == "":
            issues.append(f"{prefix}.{field} is missing")
        elif actual != expected:
            issues.append(f"{prefix}.{field} differs from the benchmark source or release image")

    if manifest.get("schema_version") != 1:
        issues.append("benchmark_harness.schema_version must be 1")
    same(manifest, "base_source_sha", base_p.get("git_sha"), "benchmark_harness")
    same(manifest, "candidate_source_sha", cand_p.get("git_sha"), "benchmark_harness")
    same(manifest, "harness_source_sha", cand_p.get("git_sha"), "benchmark_harness")
    same(manifest, "base_release_image_id", base_p.get("image_id"), "benchmark_harness")
    same(manifest, "candidate_release_image_id", cand_p.get("image_id"), "benchmark_harness")
    harness_sha = manifest.get("harness_sha256")
    if not isinstance(harness_sha, str) or not re.fullmatch(r"[0-9a-f]{64}", harness_sha):
        issues.append("benchmark_harness.harness_sha256 must be a SHA-256 digest")

    files = manifest.get("files")
    if not isinstance(files, list) or len(files) != len(BENCHMARK_HARNESS_FILES):
        issues.append("benchmark_harness.files must contain both benchmark source files")
        files = []
    paths = []
    overlaid = []
    for entry in files:
        if not isinstance(entry, dict):
            issues.append("benchmark_harness.files contains a malformed entry")
            continue
        path = entry.get("path")
        paths.append(path)
        if not isinstance(entry.get("sha256"), str) or not re.fullmatch(r"[0-9a-f]{64}", entry["sha256"]):
            issues.append(f"benchmark_harness.files[{path!r}].sha256 is missing or malformed")
        original = entry.get("base_original_sha256")
        if original is not None and (not isinstance(original, str) or not re.fullmatch(r"[0-9a-f]{64}", original)):
            issues.append(f"benchmark_harness.files[{path!r}].base_original_sha256 is malformed")
        if not isinstance(entry.get("overlaid"), bool):
            issues.append(f"benchmark_harness.files[{path!r}].overlaid is missing or malformed")
        elif entry["overlaid"]:
            overlaid.append(path)
        elif original != entry.get("sha256"):
            issues.append(f"benchmark_harness.files[{path!r}] claims no overlay but base content differs")
    if paths != list(BENCHMARK_HARNESS_FILES):
        issues.append("benchmark_harness.files paths are missing, duplicated, or out of order")
    helper_files = manifest.get("helper_files")
    if not isinstance(helper_files, list):
        issues.append("benchmark_harness.helper_files is missing or malformed")
        helper_files = []
    helper_paths = []
    for entry in helper_files:
        if not isinstance(entry, dict):
            issues.append("benchmark_harness.helper_files contains a malformed entry")
            continue
        path = entry.get("path")
        helper_paths.append(path)
        if not isinstance(path, str) or not path.startswith("internal/run/") or \
                not path.endswith("_test.go") or path in BENCHMARK_HARNESS_FILES:
            issues.append(f"benchmark_harness.helper_files has an invalid path: {path!r}")
        if not isinstance(entry.get("sha256"), str) or not re.fullmatch(r"[0-9a-f]{64}", entry["sha256"]):
            issues.append(f"benchmark_harness.helper_files[{path!r}].sha256 is missing or malformed")
    if not all(isinstance(path, str) for path in helper_paths) or \
            helper_paths != sorted(set(helper_paths)):
        issues.append("benchmark_harness.helper_files paths are duplicated or out of order")
    for path in BENCHMARK_REQUIRED_HELPERS:
        if path not in helper_paths:
            issues.append(f"benchmark_harness.helper_files lacks required helper {path}")
    digest_files = files + helper_files
    if all(isinstance(entry, dict) and isinstance(entry.get("path"), str) and
           isinstance(entry.get("sha256"), str) and re.fullmatch(r"[0-9a-f]{64}", entry["sha256"])
           for entry in digest_files):
        digest = hashlib.sha256()
        for entry in digest_files:
            digest.update(entry["path"].encode() + b"\0" + entry["sha256"].encode() + b"\0")
        if digest.hexdigest() != harness_sha:
            issues.append("benchmark_harness.harness_sha256 differs from source and helper hashes")
    overlays = manifest.get("base_overlay_paths")
    if not isinstance(overlays, list) or overlays != overlaid:
        issues.append("benchmark_harness.base_overlay_paths differs from file overlay accounting")

    for label, prov, source_sha, expected_overlay in (
        ("base", base_p, base_p.get("git_sha"), overlays),
        ("candidate", cand_p, cand_p.get("git_sha"), []),
    ):
        same(prov, "benchmark_source_git_sha", source_sha, f"{label}.provenance")
        same(prov, "benchmark_harness_git_sha", manifest.get("harness_source_sha"), f"{label}.provenance")
        same(prov, "benchmark_harness_sha256", harness_sha, f"{label}.provenance")
        same(prov, "benchmark_overlay_paths", expected_overlay, f"{label}.provenance")
    issues.extend(benchmark_sampling_issues(doc, base, candidate, manifest))
    return issues


def benchmark_sampling_issues(doc, base, candidate, manifest):
    """Require every declared benchmark and metric in every paired repeat."""
    sampling = doc.get("benchmark_sampling")
    if not isinstance(sampling, dict):
        return ["benchmark_sampling evidence is missing or malformed"]
    issues = []
    if sampling.get("schema_version") != 1:
        issues.append("benchmark_sampling.schema_version must be 1")
    names = manifest.get("benchmark_names")
    if not isinstance(names, list) or not names or any(
        not isinstance(name, str) or
        re.fullmatch(r"Benchmark(?:Owner|Recover)[A-Za-z0-9_]*", name) is None
        for name in names
    ) or names != sorted(set(names)):
        issues.append("benchmark_harness.benchmark_names is missing, malformed, or duplicated")
        names = []
    if sampling.get("expected_names") != names:
        issues.append("benchmark_sampling.expected_names differs from the shared harness")
    base_compile = sampling.get("base_compile")
    if not isinstance(base_compile, dict) or type(base_compile.get("exit_code")) is not int or \
            base_compile["exit_code"] < 0 or base_compile.get("output_path") != "observations/benchmark-base-compile.txt":
        issues.append("benchmark_sampling.base_compile preflight evidence is missing or malformed")
    elif base_compile["exit_code"] != 0:
        issues.append(
            f"benchmark harness incompatible with base (compile exited {base_compile['exit_code']})"
        )
    files = manifest.get("files")
    if isinstance(files, list) and all(isinstance(entry, dict) for entry in files):
        source_files = [
            {"path": entry.get("path"), "sha256": entry.get("sha256")}
            for entry in files
        ]
        if sampling.get("source_files") != source_files:
            issues.append("benchmark_sampling.source_files differs from shared harness hashes")
    repeats = sampling.get("repeats")
    if type(repeats) is not int or repeats < 1:
        issues.append("benchmark_sampling.repeats must be a positive integer")
        repeats = None
    settings = sampling.get("settings_sha256")
    if not isinstance(settings, str) or not re.fullmatch(r"[0-9a-f]{64}", settings):
        issues.append("benchmark_sampling.settings_sha256 is missing or malformed")
    for label, side in (("base", base), ("candidate", candidate)):
        prov = side.get("provenance") if isinstance(side.get("provenance"), dict) else {}
        if prov.get("settings_sha256") != settings:
            issues.append(f"{label}.provenance.settings_sha256 differs from benchmark sampling")
        if prov.get("benchmark_repeats") != repeats:
            issues.append(f"{label}.provenance.benchmark_repeats differs from benchmark sampling")

    order = sampling.get("order")
    repeat_exits = {"base": [], "candidate": []}
    if repeats is not None:
        expected_pairs = [
            (repeat, label)
            for repeat in range(1, repeats + 1)
            for label in (("base", "candidate") if repeat % 2 else ("candidate", "base"))
        ]
        if not isinstance(order, list) or len(order) != len(expected_pairs):
            issues.append("benchmark_sampling.order does not contain every paired repeat")
        else:
            for entry, (repeat, label) in zip(order, expected_pairs):
                if not isinstance(entry, dict) or entry.get("repeat") != repeat or entry.get("side") != label or \
                        type(entry.get("exit_code")) is not int or entry["exit_code"] < 0:
                    issues.append(f"benchmark_sampling.order has invalid repeat {repeat} {label}")
                    continue
                repeat_exits[label].append(entry["exit_code"])
            aggregate = sampling.get("aggregate_exit")
            if not isinstance(aggregate, dict):
                issues.append("benchmark_sampling.aggregate_exit is missing")
            else:
                for label in ("base", "candidate"):
                    if len(repeat_exits[label]) != repeats:
                        continue
                    first_failure = next((code for code in repeat_exits[label] if code), 0)
                    if aggregate.get(label) != first_failure:
                        issues.append(f"benchmark_sampling.aggregate_exit.{label} differs from repeat exits")
                    if first_failure:
                        issues.append(f"benchmark_sampling.{label} has a failed repeat")

    if not names or repeats is None:
        return issues
    expected_names = set(names)
    for label, side in (("base", base), ("candidate", candidate)):
        benchmarks = side.get("benchmarks")
        if isinstance(benchmarks, str):
            for line in benchmarks.splitlines():
                line = line.strip()
                if not line.startswith("Benchmark"):
                    continue
                match = GO_BENCH_RE.fullmatch(line)
                if match is None or match.group(4) is None or match.group(5) is None:
                    issues.append(f"{label}.benchmarks has a malformed or incomplete -benchmem row")
            benchmarks = parse_go_bench_text(benchmarks)
        if not isinstance(benchmarks, dict):
            issues.append(f"{label}.benchmarks is missing or malformed")
            continue
        observed = set(benchmarks)
        if observed != expected_names:
            issues.append(f"{label}.benchmarks differs from shared harness: missing={sorted(expected_names - observed)} extra={sorted(observed - expected_names)}")
        for name in names:
            body = benchmarks.get(name)
            if not isinstance(body, dict):
                continue
            for metric in ("ns_per_op", "bytes_per_op", "allocs_per_op"):
                try:
                    samples, _ = coerce_samples(body[metric])
                except (KeyError, CompareError, TypeError, ValueError):
                    issues.append(f"{label}.benchmarks.{name}.{metric} is missing or malformed")
                    continue
                if len(samples) != repeats:
                    issues.append(f"{label}.benchmarks.{name}.{metric} has {len(samples)} samples, want {repeats}")
    return issues


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
        "u1": None,
        "u2": None,
        "lower_is_better": left.get("lower_is_better"),
    }
    if left.get("direction_error") or right.get("direction_error"):
        result["verdict"] = "fail"
        result["reasons"].append(left.get("direction_error") or right.get("direction_error"))
        return result
    if left.get("lower_is_better") is None or right.get("lower_is_better") is None:
        result["verdict"] = "fail"
        result["reasons"].append(f"unknown metric direction for {metric_id}")
        return result
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

    u1, u2, p = mann_whitney(a, b)
    result["u1"] = u1
    result["u2"] = u2
    result["u_statistic"] = None if u1 is None or u2 is None else min(u1, u2)
    result["p_value"] = p
    base_mean = mean(a)
    cand_mean = mean(b)
    hl = hodges_lehmann(a, b)
    result["hodges_lehmann"] = hl
    if base_mean == 0:
        result["delta_pct"] = None if cand_mean == 0 else math.inf
    else:
        result["delta_pct"] = (cand_mean - base_mean) / abs(base_mean) * 100.0

    hib = result["lower_is_better"]
    # Rank direction: U1 large means base tends larger than candidate.
    if u1 is None or u2 is None:
        result["verdict"] = "fail"
        result["reasons"].append("mann-whitney is undefined")
        return result
    if hib:
        rank_faster, rank_slower = u1 > u2, u1 < u2
        hl_faster, hl_slower = hl < 0, hl > 0
    else:
        rank_faster, rank_slower = u2 > u1, u2 < u1
        hl_faster, hl_slower = hl > 0, hl < 0

    noisy = cv(a) > max_cv or cv(b) > max_cv
    significant = p is not None and p < alpha

    if significant:
        if rank_faster and (hl_faster or hl == 0):
            result["verdict"] = "faster"
            result["reasons"].append(
                f"U1={u1:.4g} U2={u2:.4g} (lower_is_better={hib}) with p={p:.4g} < {alpha}"
            )
            return result
        if rank_slower and (hl_slower or hl == 0):
            result["verdict"] = "slower"
            result["reasons"].append(
                f"U1={u1:.4g} U2={u2:.4g} (lower_is_better={hib}) with p={p:.4g} < {alpha}"
            )
            return result
        result["verdict"] = "inconclusive"
        result["reasons"].append(
            f"significant (p={p:.4g}) but rank and Hodges-Lehmann directions disagree "
            f"(U1={u1:.4g} U2={u2:.4g} HL={hl:.4g}); not faster, not equivalent"
        )
        return result

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
    """Exit 0 only when every metric is faster or no_significant_difference.

    Any inconclusive metric or provenance mismatch makes the overall result
    inconclusive, even if some other metric is faster.
    """
    if fail_reasons:
        return "fail", fail_reasons
    verdicts = [m["verdict"] for m in metric_results]
    if "fail" in verdicts:
        return "fail", [m["id"] + ": " + "; ".join(m["reasons"]) for m in metric_results if m["verdict"] == "fail"]
    if "inconclusive" in verdicts or inconclusive_reasons:
        reasons = list(inconclusive_reasons)
        reasons.extend(
            m["id"] + ": " + "; ".join(m["reasons"])
            for m in metric_results
            if m["verdict"] == "inconclusive"
        )
        if not reasons:
            reasons = ["inconclusive evidence"]
        return "inconclusive", reasons
    if "slower" in verdicts:
        return "slower", [m["id"] for m in metric_results if m["verdict"] == "slower"]
    if not verdicts:
        return "inconclusive", ["no comparable metrics"]
    if all(v in {"faster", "no_significant_difference"} for v in verdicts):
        if "faster" in verdicts:
            return "faster", [m["id"] for m in metric_results if m["verdict"] == "faster"]
        return "no_significant_difference", [
            "every comparable metric is insignificant; this is not equivalence"
        ]
    return "inconclusive", ["unrecognized metric verdicts"]


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

    required_families = list(doc.get("required_families") or [])
    prov = provenance_issues(base, candidate)
    if "benchmark" in required_families or base.get("benchmarks") or candidate.get("benchmarks"):
        prov["invalid"].extend(benchmark_harness_issues(doc, base, candidate))
    if prov["missing"]:
        fail_reasons.extend(prov["missing"])
    if prov["invalid"]:
        fail_reasons.extend(prov["invalid"])
    if prov["instrumented"]:
        fail_reasons.extend(prov["instrumented"])
    if prov["mismatched"]:
        inconclusive_reasons.extend(prov["mismatched"])

    required_browser = list(doc.get("required_browser_series") or [])
    if "browser" in required_families and not required_browser:
        required_browser = list(REQUIRED_BROWSER_SERIES)

    base_metrics = expand_side_metrics(base)
    cand_metrics = expand_side_metrics(candidate)

    base_fail = side_correctness(base, base_metrics)
    cand_fail = side_correctness(candidate, cand_metrics)
    if base.get("bundle"):
        base_fail.extend(bundle_failures(base.get("bundle"), "base"))
    elif "bundle" in required_families:
        base_fail.append("required family bundle has no evidence")
    if candidate.get("bundle"):
        cand_fail.extend(bundle_failures(candidate.get("bundle"), "candidate"))
    elif "bundle" in required_families:
        cand_fail.append("required family bundle has no evidence")
    if base_fail:
        fail_reasons.extend(f"base correctness: {x}" for x in base_fail)
    if cand_fail:
        fail_reasons.extend(f"candidate correctness: {x}" for x in cand_fail)

    for family in required_families:
        if family == "bundle":
            continue
        has = any(m.get("family") == family for m in list(base_metrics.values()) + list(cand_metrics.values()))
        if not has:
            fail_reasons.append(f"required family {family} has zero metrics")

    for key in required_browser:
        if key not in base_metrics or key not in cand_metrics:
            fail_reasons.append(f"required browser series missing: {key}")

    ids = sorted(set(base_metrics) | set(cand_metrics))
    metric_results = []
    speed_compared = False
    bundle_result = None
    if base.get("bundle") or candidate.get("bundle") or "bundle" in required_families:
        bundle_result = compare_bundle(base.get("bundle") or {}, candidate.get("bundle") or {})
        if bundle_result["verdict"] == "fail":
            fail_reasons.extend(bundle_result["reasons"])

    if fail_reasons:
        overall, reasons = rollup([], fail_reasons, inconclusive_reasons)
        report = {
            "schema_version": SCHEMA_VERSION,
            "overall": overall,
            "reasons": reasons,
            "speed_compared": False,
            "metrics": [],
            "bundle": bundle_result,
            "benchstat": "speed not compared: correctness/provenance failed closed\n",
            "provenance": prov,
            "required_families": required_families,
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
        "bundle": bundle_result,
        "benchstat": benchstat_text(metric_results),
        "provenance": prov,
        "required_families": required_families,
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


def merge_sides(base_path, candidate_path, benchmark_evidence_path=None):
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
    doc = {"schema_version": SCHEMA_VERSION, "base": base_side, "candidate": cand_side}
    if benchmark_evidence_path:
        evidence = load_json(benchmark_evidence_path)
        if not isinstance(evidence, dict) or not isinstance(evidence.get("benchmark_harness"), dict) or \
                not isinstance(evidence.get("benchmark_sampling"), dict):
            raise CompareError("--benchmark-evidence must contain benchmark_harness and benchmark_sampling")
        for key in ("benchmark_harness", "benchmark_sampling", "required_families", "required_browser_series"):
            if key in evidence:
                doc[key] = evidence[key]
    elif base_side.get("benchmarks") or cand_side.get("benchmarks"):
        raise CompareError("--base/--candidate with benchmarks requires --benchmark-evidence PATH")
    return doc


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("input", nargs="?", help="comparison document JSON")
    parser.add_argument("--input", dest="input_flag", help="comparison document JSON")
    parser.add_argument("--base", help="base side JSON (used with --candidate)")
    parser.add_argument("--candidate", help="candidate side JSON (used with --base)")
    parser.add_argument("--benchmark-evidence", help="comparison JSON carrying the shared benchmark harness and sampling manifest for --base/--candidate")
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
            doc = merge_sides(args.base, args.candidate, args.benchmark_evidence)
        else:
            if args.benchmark_evidence:
                raise CompareError("--benchmark-evidence requires --base and --candidate")
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
