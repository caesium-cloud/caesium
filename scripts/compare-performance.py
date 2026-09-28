#!/usr/bin/env python3
"""Compare base vs candidate performance evidence. Fail closed.

This program never treats missing data, mismatched provenance, undersampled
series, or a statistically insignificant delta as equivalence.
Correctness/completion failures abort before any speed comparison.

Input is a JSON comparison document (schema_version 1) with `base` and
`candidate` sides, each carrying provenance, correctness, and metric
families (workloads, benchmarks, browser, bundle, system).

The decision rule is E4's, loaded from test/performance/budgets.json
(`--budgets`; the file must carry a reviewed `changes` entry for its current
values or the comparator refuses it). Per series: a Hodges-Lehmann ratio with
a one-sided Moses bound (non-inferiority against the series' allowed
degradation) and one-sided Mann-Whitney tests, Holm-corrected per family.
Absolute SLOs are checked on the candidate. `--baseline` adds a second
verdict against the versioned fixed baseline (cumulative regression); a
missing baseline or one from a different runner/setup is inconclusive, never
silently compared. `--attempt N` places the run in the bounded rerun policy.
See apply_budgets() and the budgets file for the full rule. E3's uncorrected
per-series verdicts stay in the report as `uncorrected_verdict` /
`uncorrected_overall`.

Exit status:
  0  pass: every series non-inferior within budget and SLOs met; overall is
     no_significant_difference, faster (state tradeoffs), or within_budget
     (a significant slowdown bounded inside its allowed degradation)
  1  usage or schema error, including unreviewed budget values
  2  fail-closed: correctness failed, instrumented image, or missing required
     data, including missing or invalid SLO evidence (below)
  3  inconclusive (rerun required) or inconclusive_unresolved (rerun budget
     exhausted, or not fixable by a rerun: mismatched environment, missing or
     mismatched fixed baseline, undersampled SLO). A strict gate blocks on both.
  4  slower (material regression vs base or fixed baseline) or slo_breach

SLO evidence fails closed. An SLO whose source was not selected for this run
(a workload, series or bundle absent from the candidate) is `not_evaluated`.
A workload that ran must carry the SLO's field in every sample: a sample
without it is missing data, and the SLO (and the run) is `fail`, exit 2,
never `not_evaluated`. Every numeric SLO observation must be a finite JSON
number; NaN, Infinity, numeric strings and booleans are invalid evidence and
also `fail`. Metric samples must be finite too (a non-finite sample is a
schema error, exit 1).

Other modes: `--record-baseline OUT` writes the fixed baseline from a run
whose target-base verdict passed; `--calibrate DOC...` summarizes same-code
A/A control runs (false-positive rates, pooled variance, cross-run drift) and
refuses any document whose two sides are not one build of one source
(A_A_IDENTITY), whatever its `control` label says; `--bundle-dir` runs the
bundle-size check.

Stdout is the JSON report. Human summary goes to stderr. Every JSON this
program writes is strict JSON: a non-finite number (an undefined delta or CV)
is written as null, never as NaN or Infinity.

Two report sections are informational only and never change a verdict or the
exit status: `multiplicity` (how many series were tested at alpha and how
many significant results chance alone predicts, uncorrected) and each
metric's `diagnostics.extreme_samples` (robust z-scores that point at the
sample to attribute). No outlier is ever removed.
"""

from __future__ import annotations

import argparse
import fnmatch
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
# Iglewicz-Hoaglin modified z-score cut-off; used only to point at samples.
EXTREME_ROBUST_Z = 3.5

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


def extreme_samples(xs, threshold=EXTREME_ROBUST_Z):
    """1-based sample positions whose modified z-score exceeds threshold.

    Informational: it names the sample to attribute (sample order is the
    recorded order, i.e. repeat order for interleaved series). It never
    removes a sample or changes a verdict. A zero MAD reports nothing.
    """
    ys = [float(x) for x in xs]
    if len(ys) < 3:
        return []
    med = statistics.median(ys)
    mad = statistics.median(abs(y - med) for y in ys)
    if mad == 0:
        return []
    out = []
    for index, y in enumerate(ys, start=1):
        z = 0.6745 * (y - med) / mad
        if abs(z) > threshold:
            out.append({"sample": index, "value": y, "robust_z": round(z, 2)})
    return out


def is_degenerate(result):
    """Both sides constant and equal: the test cannot produce a small p."""
    base, cand = result.get("base") or {}, result.get("candidate") or {}
    return (base.get("stdev") or 0) == 0 and (cand.get("stdev") or 0) == 0 and \
        base.get("mean") == cand.get("mean")


def multiplicity_summary(metric_results, alpha):
    """Informational chance-level accounting for the per-metric decisions.

    The expectations assume independent tests at exactly alpha. They are not:
    every benchmark in one sample shares a `go test` process and its host
    state, so benchmark verdicts arrive in same-direction clusters (a same-image
    A/A control produced six "faster" ns/op verdicts at once). The tie-corrected
    normal approximation is also somewhat conservative. Treat the numbers as an
    order of magnitude; they never feed a verdict or the exit status.
    """
    tested = [m for m in metric_results if m.get("p_value") is not None]
    informative = [m for m in tested if not is_degenerate(m)]
    n = len(informative)
    significant = sum(1 for m in informative if m["p_value"] < alpha)
    slower = sum(1 for m in informative if m["verdict"] == "slower")
    tail = sum(
        math.comb(n, k) * alpha ** k * (1.0 - alpha) ** (n - k)
        for k in range(significant, n + 1)
    ) if n else 1.0
    by_family = {}
    for m in informative:
        entry = by_family.setdefault(
            m.get("family") or "unknown", {"informative": 0, "significant": 0, "slower": 0, "faster": 0}
        )
        entry["informative"] += 1
        entry["significant"] += int(m["p_value"] < alpha)
        entry["slower"] += int(m["verdict"] == "slower")
        entry["faster"] += int(m["verdict"] == "faster")
    return {
        "informational_only": True,
        "alpha": alpha,
        "metrics_with_p_value": len(tested),
        "informative_metrics": n,
        "degenerate_metrics": len(tested) - n,
        "expected_significant_if_no_change": round(alpha * n, 3),
        "expected_slower_if_no_change": round(alpha / 2.0 * n, 3),
        "probability_no_slower_if_no_change": round((1.0 - alpha / 2.0) ** n, 4),
        "observed_significant": significant,
        "observed_slower": slower,
        "probability_at_least_observed_significant_if_no_change": round(min(1.0, tail), 4),
        "by_family": dict(sorted(by_family.items())),
        "note": (
            "Per-metric verdicts use alpha without correction; aggregation and "
            "multiple-comparison policy are E4 decisions (Q2/Q5). Degenerate metrics "
            "(both sides constant and equal) are excluded from the counts. The "
            "expectations assume independent tests; benchmarks sampled in one process "
            "are correlated, so their false positives cluster."
        ),
    }


def _phi(z):
    return 0.5 * (1.0 + math.erf(z / math.sqrt(2.0)))


def _mann_whitney_core(a, b):
    """Rank sums for Mann-Whitney U: (u1, u2, mean_u, sigma).

    u1 is the base-sample statistic (large when base values tend to exceed
    candidate values); sigma is tie-corrected. None when a side is empty.
    """
    n1, n2 = len(a), len(b)
    if n1 == 0 or n2 == 0:
        return None
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
    sigma = math.sqrt(var) if var > 0 else 0.0
    return u1, u2, mean_u, sigma


def mann_whitney(a, b):
    """Two-sided Mann-Whitney U (normal approximation, tie-corrected).

    Returns (u1, u2, p). u1 is the base-sample statistic (large when base
    values tend to exceed candidate values). Direction of a change must be
    taken from u1 vs u2, not from the means.
    """
    core = _mann_whitney_core(a, b)
    if core is None:
        return None, None, None
    u1, u2, mean_u, sigma = core
    if sigma <= 0:
        return u1, u2, 1.0
    u = min(u1, u2)
    z = (abs(u - mean_u) - 0.5) / sigma
    p = 2.0 * (1.0 - _phi(z))
    return u1, u2, min(1.0, max(0.0, p))


def mann_whitney_one_sided(a, b):
    """One-sided Mann-Whitney p-values (p_b_greater, p_b_less).

    p_b_greater is small when the second sample tends to exceed the first.
    Normal approximation with tie and continuity correction; all-tied data
    yields (1.0, 1.0).
    """
    core = _mann_whitney_core(a, b)
    if core is None:
        return None, None
    _u1, u2, mean_u, sigma = core
    if sigma <= 0:
        return 1.0, 1.0
    greater = 1.0 - _phi((u2 - mean_u - 0.5) / sigma)
    less = _phi((u2 - mean_u + 0.5) / sigma)
    return min(1.0, max(0.0, greater)), min(1.0, max(0.0, less))


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


def is_finite_number(value):
    """A real JSON number: not a bool, not a string, not NaN or +/-Infinity."""
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


def sample_float(value, where="sample"):
    """float(value), refusing anything that is not a finite number."""
    try:
        number = float(value)
    except (TypeError, ValueError) as err:
        raise CompareError(f"{where} is not a number: {value!r}") from err
    if not math.isfinite(number):
        raise CompareError(f"{where} is not a finite number: {value!r}")
    return number


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
            samples.append(sample_float(item))
            outcomes.append("passed")
            continue
        if not isinstance(item, dict):
            raise CompareError(f"sample entries must be numbers or objects, got {type(item).__name__}")
        outcomes.append(str(item.get("outcome") or item.get("status") or "passed"))
        for key in ("value", "ns_per_op", "duration_seconds", "ms", "bytes"):
            if key in item:
                samples.append(sample_float(item[key], f"sample {key}"))
                break
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
        "diagnostics": {
            "base": {"extreme_samples": extreme_samples(left.get("samples") or [])},
            "candidate": {"extreme_samples": extreme_samples(right.get("samples") or [])},
        },
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


def benchstat_text(metric_results, multiplicity=None):
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
    for m in rows:
        if m["verdict"] in {"faster", "no_significant_difference"}:
            continue
        for label in ("base", "candidate"):
            extremes = ((m.get("diagnostics") or {}).get(label) or {}).get("extreme_samples") or []
            if extremes:
                shown = ", ".join(f"#{e['sample']}={e['value']:.6g} (z={e['robust_z']})" for e in extremes)
                lines.append(f"extreme {label} samples in {m['id']}: {shown}")
    if multiplicity:
        lines.append(
            "multiplicity (informational): "
            f"{multiplicity['informative_metrics']} informative metrics at alpha={multiplicity['alpha']}; "
            f"with no real change expect {multiplicity['expected_significant_if_no_change']:.2f} significant "
            f"({multiplicity['expected_slower_if_no_change']:.2f} slower), "
            f"P(no slower)={multiplicity['probability_no_slower_if_no_change']:.2f}; "
            f"observed {multiplicity['observed_significant']} significant, "
            f"{multiplicity['observed_slower']} slower"
        )
    return "\n".join(lines) + "\n"


# ---------------------------------------------------------------------------
# E4 budgeted decision rule (test/performance/budgets.json)
# ---------------------------------------------------------------------------
#
# The per-series Mann-Whitney verdicts above stay in the report as
# `uncorrected_verdict`. When budgets are supplied the decision is:
#
#   1. Correctness/completion, provenance and harness identity fail closed
#      before any speed comparison (unchanged).
#   2. Each series is matched to the first budget rule (fnmatch on its id)
#      of its family. No rule -> fail closed.
#   3. Per series: the Hodges-Lehmann location shift on log values (a ratio)
#      with a distribution-free Moses confidence bound, and one-sided
#      Mann-Whitney p-values for "worse" and "better".
#   4. Per family: Holm step-down on the one-sided p-values at
#      alpha_family = alpha / (families with an informative series). Holm is
#      valid under arbitrary dependence, so series that share a process
#      (benchmarks) cannot fail the family on one uncorrected p-value.
#   5. Per series verdict:
#        undersampled or bound not computable      -> inconclusive
#        Holm-significant worse AND point > 1+margin -> slower (material)
#        CV above the rule's max_cv                  -> inconclusive (noisy)
#        upper bound <= 1+margin (non-inferior):
#            Holm-significant better                 -> faster
#            Holm-significant worse (within margin)  -> within_budget
#            otherwise                               -> no_significant_difference
#        otherwise (bound not established)           -> inconclusive
#      Passing requires EVERY series to be non-inferior (an intersection-union
#      test, valid at the per-series level without correction). The
#      non-inferiority level is split across the allowed attempts up front,
#      so bounded reruns cannot shop for a pass.
#   6. Absolute SLOs are checked on the candidate side only.
#   7. The same rule runs against the versioned fixed baseline with its own
#      margins; a provenance mismatch there is inconclusive, never compared.
#   8. The worst verdict wins: fail > slower > slo_breach > inconclusive >
#      within_budget > faster > no_significant_difference. An inconclusive
#      result carries the bounded rerun state; exhausted or non-rerunnable
#      inconclusive evidence becomes `inconclusive_unresolved` (exit 3), which
#      a strict gate must treat as blocking.

DEFAULT_BUDGETS = Path(__file__).resolve().parents[1] / "test/performance/budgets.json"
DEFAULT_BASELINE = Path(__file__).resolve().parents[1] / "test/performance/baseline.json"
BUDGETS_SCHEMA_VERSION = 1
BASELINE_SCHEMA_VERSION = 1
BUDGETED_FAMILIES = ("workload", "benchmark", "browser", "system")
# Budget sections whose values are covered by the reviewed-changes digest.
BUDGET_VALUE_KEYS = ("decision", "families", "slos", "bundle", "rerun_policy", "fixed_baseline")
VERDICT_RANK = {
    "no_significant_difference": 1,
    "faster": 2,
    "within_budget": 3,
    "inconclusive": 4,
    "slo_breach": 5,
    "slower": 6,
    "fail": 7,
}
PASS_VERDICTS = ("no_significant_difference", "faster", "within_budget")
# Fixed-baseline evidence is comparable only on the same runner, toolchain,
# workload catalog, settings, and benchmark harness.
FIXED_BASELINE_MUST_MATCH = (
    "host_id",
    "platform",
    "go_version",
    "catalog_sha256",
    "settings_sha256",
    "benchmark_harness_sha256",
)
_NORMAL = statistics.NormalDist()


def canonical_json(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"))


def json_safe(value):
    """Replace non-finite floats (an undefined delta or CV) with None."""
    if isinstance(value, float):
        return value if math.isfinite(value) else None
    if isinstance(value, dict):
        return {key: json_safe(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [json_safe(item) for item in value]
    return value


def dump_json(value):
    """Strict JSON for every document this program writes: never NaN/Infinity."""
    return json.dumps(json_safe(value), indent=2, allow_nan=False) + "\n"


def budget_values_sha256(budgets):
    """Digest of every enforced budget value (not descriptions or the log)."""
    body = {key: budgets.get(key) for key in BUDGET_VALUE_KEYS}
    return hashlib.sha256(canonical_json(body).encode()).hexdigest()


def _positive_number(value):
    return isinstance(value, (int, float)) and not isinstance(value, bool) and value > 0


def validate_budgets(budgets):
    """Raise CompareError unless budgets are complete and reviewed."""
    if not isinstance(budgets, dict):
        raise CompareError("budgets must be an object")
    if budgets.get("schema_version") != BUDGETS_SCHEMA_VERSION:
        raise CompareError(
            f"budgets schema_version={budgets.get('schema_version')!r}, want {BUDGETS_SCHEMA_VERSION}"
        )
    decision = budgets.get("decision")
    if not isinstance(decision, dict):
        raise CompareError("budgets.decision is missing")
    for key in ("alpha", "alpha_non_inferiority"):
        value = decision.get(key)
        if not _positive_number(value) or value >= 0.5:
            raise CompareError(f"budgets.decision.{key} must be in (0, 0.5)")
    families = budgets.get("families")
    if not isinstance(families, dict):
        raise CompareError("budgets.families is missing")
    for family in BUDGETED_FAMILIES:
        body = families.get(family)
        if not isinstance(body, dict) or not isinstance(body.get("rules"), list) or not body["rules"]:
            raise CompareError(f"budgets.families.{family}.rules is missing or empty")
        for index, rule in enumerate(body["rules"]):
            where = f"budgets.families.{family}.rules[{index}]"
            if not isinstance(rule, dict) or not isinstance(rule.get("match"), str) or not rule["match"]:
                raise CompareError(f"{where}.match is missing")
            for key in ("max_relative_degradation", "fixed_baseline_max_relative_degradation", "max_cv"):
                if not _positive_number(rule.get(key)):
                    raise CompareError(f"{where}.{key} must be a positive number")
            if type(rule.get("min_samples")) is not int or rule["min_samples"] < 3:
                raise CompareError(f"{where}.min_samples must be an integer >= 3")
            if not isinstance(rule.get("rationale"), str) or not rule["rationale"].strip():
                raise CompareError(f"{where}.rationale is missing")
        if body["rules"][-1]["match"] != f"{ 'bench' if family == 'benchmark' else family}.*":
            raise CompareError(f"budgets.families.{family} must end with a catch-all rule")
    slos = budgets.get("slos")
    if not isinstance(slos, list):
        raise CompareError("budgets.slos must be a list")
    seen = set()
    for index, slo in enumerate(slos):
        where = f"budgets.slos[{index}]"
        if not isinstance(slo, dict) or not isinstance(slo.get("id"), str) or slo["id"] in seen:
            raise CompareError(f"{where}.id is missing or duplicated")
        seen.add(slo["id"])
        sources = [key for key in ("series", "workload", "bundle") if key in slo]
        if len(sources) != 1:
            raise CompareError(f"{where} must name exactly one of series, workload, bundle")
        if "workload" in slo and not isinstance(slo.get("field"), str):
            raise CompareError(f"{where}.field is required with workload")
        if slo.get("statistic") not in ("p90", "max", "min", "all", "value"):
            raise CompareError(f"{where}.statistic must be p90, max, min, all or value")
        bounds = [key for key in ("max", "min", "equals") if key in slo]
        if len(bounds) != 1:
            raise CompareError(f"{where} must set exactly one of max, min, equals")
        if not isinstance(slo.get("rationale"), str) or not slo["rationale"].strip():
            raise CompareError(f"{where}.rationale is missing")
    bundle = budgets.get("bundle")
    if not isinstance(bundle, dict) or not isinstance(bundle.get("max_relative_growth"), dict):
        raise CompareError("budgets.bundle.max_relative_growth is missing")
    for key in BUNDLE_KEYS:
        growth = bundle["max_relative_growth"].get(key)
        if not isinstance(growth, dict) or not _positive_number(growth.get("target_base")) or \
                not _positive_number(growth.get("fixed_baseline")):
            raise CompareError(f"budgets.bundle.max_relative_growth.{key} needs target_base and fixed_baseline")
    policy = budgets.get("rerun_policy")
    if not isinstance(policy, dict) or type(policy.get("max_reruns")) is not int or policy["max_reruns"] < 0:
        raise CompareError("budgets.rerun_policy.max_reruns must be a non-negative integer")
    if not isinstance(policy.get("between_attempts"), list) or not policy["between_attempts"]:
        raise CompareError("budgets.rerun_policy.between_attempts must say what changes between attempts")
    fixed = budgets.get("fixed_baseline")
    if not isinstance(fixed, dict) or fixed.get("must_match") != list(FIXED_BASELINE_MUST_MATCH):
        raise CompareError(f"budgets.fixed_baseline.must_match must be {list(FIXED_BASELINE_MUST_MATCH)}")
    if not isinstance(budgets.get("calibration"), dict) or not budgets["calibration"].get("runner"):
        raise CompareError("budgets.calibration.runner is missing: no promotion before calibration evidence")
    changes = budgets.get("changes")
    if not isinstance(changes, list) or not changes:
        raise CompareError("budgets.changes must record every reviewed budget change")
    for index, change in enumerate(changes):
        where = f"budgets.changes[{index}]"
        if not isinstance(change, dict):
            raise CompareError(f"{where} must be an object")
        for key in ("date", "rationale", "evidence", "reviewed_in", "values_sha256"):
            if not isinstance(change.get(key), str) or not change[key].strip():
                raise CompareError(f"{where}.{key} is missing")
    latest = changes[-1]["values_sha256"]
    actual = budget_values_sha256(budgets)
    if latest != actual:
        raise CompareError(
            "budget values differ from the last reviewed change: add a budgets.changes entry with "
            f"rationale, evidence, reviewed_in and values_sha256={actual}"
        )
    return budgets


def load_budgets(path):
    return validate_budgets(load_json(path))


def rule_for(budgets, family, metric_id):
    body = (budgets.get("families") or {}).get(family)
    if not isinstance(body, dict):
        return None
    for rule in body.get("rules") or []:
        if fnmatch.fnmatchcase(metric_id, rule["match"]):
            return rule
    return None


def nearest_rank(xs, pct):
    ys = sorted(float(x) for x in xs)
    if not ys:
        return math.nan
    rank = max(1, math.ceil(pct / 100.0 * len(ys)))
    return ys[rank - 1]


def moses_shift_bounds(a, b, alpha):
    """Hodges-Lehmann shift of b over a with one-sided Moses bounds at alpha.

    Returns (hl, lower, upper). Each bound alone has coverage >= 1 - alpha
    (normal approximation for the order-statistic index, rounded down, so the
    interval errs wide). Bounds are None when the samples are too small to
    exclude even one pairwise difference at this alpha.
    """
    diffs = sorted(float(y) - float(x) for x in a for y in b)
    n1, n2 = len(a), len(b)
    if not diffs:
        return math.nan, None, None
    hl = percentile(diffs, 50)
    z = _NORMAL.inv_cdf(1.0 - alpha)
    count = len(diffs)
    c = math.floor(count / 2.0 - z * math.sqrt(n1 * n2 * (n1 + n2 + 1) / 12.0))
    if c < 1:
        return hl, None, None
    return hl, diffs[c - 1], diffs[count - c]


def degradation_estimate(a, b, lower_is_better, alpha):
    """Degradation ratio (candidate relative to reference; > 1 is worse).

    Positive samples use log values, so the shift is a ratio. Samples with
    zero or negative values use shifts relative to the reference median.
    Returns None when neither transform is defined.
    """
    if all(x > 0 for x in a) and all(y > 0 for y in b):
        ta = [math.log(x) for x in a]
        tb = [math.log(y) for y in b]
        to_ratio = math.exp
        transform = "log_ratio"
    else:
        med = statistics.median(a)
        if med == 0:
            return None
        ta = [x / abs(med) for x in a]
        tb = [y / abs(med) for y in b]

        def to_ratio(shift):
            return 1.0 + shift

        transform = "shift_over_reference_median"
    hl, lower, upper = moses_shift_bounds(ta, tb, alpha)
    if not lower_is_better:
        hl = -hl
        lower, upper = (None if upper is None else -upper), (None if lower is None else -lower)
    return {
        "transform": transform,
        "point": to_ratio(hl),
        "lower": None if lower is None else to_ratio(lower),
        "upper": None if upper is None else to_ratio(upper),
    }


def holm_adjust(pvalues):
    """Holm step-down adjusted p-values for {key: p}."""
    ordered = sorted(pvalues.items(), key=lambda item: (item[1], item[0]))
    m = len(ordered)
    adjusted = {}
    running = 0.0
    for index, (key, p) in enumerate(ordered):
        running = max(running, min(1.0, (m - index) * p))
        adjusted[key] = running
    return adjusted


def evaluate_series(metric_id, reference, candidate, rule, margin, alpha_ni):
    """Statistics for one series under one budget rule (no verdict yet)."""
    a = list(reference.get("samples") or [])
    b = list(candidate.get("samples") or [])
    result = {
        "id": metric_id,
        "family": candidate.get("family") or reference.get("family"),
        "phase": candidate.get("phase") or reference.get("phase"),
        "rule": None if rule is None else rule["match"],
        "max_relative_degradation": margin,
        "min_samples": None if rule is None else rule["min_samples"],
        "max_cv": None if rule is None else rule["max_cv"],
        "n_reference": len(a),
        "n_candidate": len(b),
        "cv_reference": cv(a) if a else None,
        "cv_candidate": cv(b) if b else None,
        "degradation": None,
        "p_worse": None,
        "p_better": None,
        "status": "ok",
        "reasons": [],
    }
    if rule is None:
        result["status"] = "fail"
        result["reasons"].append(f"no budget rule matches {metric_id}")
        return result
    direction = candidate.get("lower_is_better")
    if direction is None or reference.get("lower_is_better") is None:
        result["status"] = "fail"
        result["reasons"].append(
            candidate.get("direction_error") or reference.get("direction_error") or
            f"unknown metric direction for {metric_id}"
        )
        return result
    result["lower_is_better"] = direction
    if not a or not b:
        result["status"] = "fail"
        result["reasons"].append("missing data: one or both sides have no samples")
        return result
    if len(a) < rule["min_samples"] or len(b) < rule["min_samples"]:
        result["status"] = "undersampled"
        result["reasons"].append(
            f"undersampled: reference n={len(a)} candidate n={len(b)} min_samples={rule['min_samples']}"
        )
        return result
    if stdev(a) == 0 and stdev(b) == 0 and mean(a) == mean(b):
        result["status"] = "degenerate"
        result["degradation"] = {"transform": "constant", "point": 1.0, "lower": 1.0, "upper": 1.0}
        result["reasons"].append("both sides constant and equal")
        return result
    estimate = degradation_estimate(a, b, direction, alpha_ni)
    if estimate is None:
        result["status"] = "undersampled"
        result["reasons"].append("reference median is zero: no relative bound is defined")
        return result
    result["degradation"] = estimate
    if estimate["upper"] is None:
        result["status"] = "undersampled"
        result["reasons"].append(
            f"n={len(a)}+{len(b)} cannot bound the shift at one-sided alpha {alpha_ni:.4g}"
        )
        return result
    greater, less = mann_whitney_one_sided(a, b)
    worse, better = (greater, less) if direction else (less, greater)
    result["p_worse"] = worse
    result["p_better"] = better
    return result


def classify_family(results, alpha_family):
    """Holm per family, then a verdict for every series (mutates results)."""
    informative = [r for r in results if r["status"] == "ok"]
    worse = holm_adjust({r["id"]: r["p_worse"] for r in informative})
    better = holm_adjust({r["id"]: r["p_better"] for r in informative})
    for r in results:
        r["alpha_family"] = alpha_family
        if r["status"] == "fail":
            r["verdict"] = "fail"
            continue
        if r["status"] == "undersampled":
            r["verdict"] = "inconclusive"
            continue
        if r["status"] == "degenerate":
            r["verdict"] = "no_significant_difference"
            continue
        r["holm_p_worse"] = worse[r["id"]]
        r["holm_p_better"] = better[r["id"]]
        estimate = r["degradation"]
        allowed = 1.0 + r["max_relative_degradation"]
        worse_sig = r["holm_p_worse"] < alpha_family
        better_sig = r["holm_p_better"] < alpha_family
        noisy = max(r["cv_reference"], r["cv_candidate"]) > r["max_cv"]
        if worse_sig and estimate["point"] > allowed:
            r["verdict"] = "slower"
            r["reasons"].append(
                f"material regression: degradation {estimate['point']:.4f} > allowed {allowed:.4f} "
                f"with Holm p={r['holm_p_worse']:.4g} < {alpha_family:.4g}"
            )
        elif noisy:
            r["verdict"] = "inconclusive"
            r["reasons"].append(
                f"noisy: cv reference={r['cv_reference']:.3f} candidate={r['cv_candidate']:.3f} "
                f"max_cv={r['max_cv']}"
            )
        elif estimate["upper"] <= allowed:
            if better_sig:
                r["verdict"] = "faster"
                r["reasons"].append(
                    f"improvement: degradation {estimate['point']:.4f} with Holm p={r['holm_p_better']:.4g}"
                )
            elif worse_sig:
                r["verdict"] = "within_budget"
                r["reasons"].append(
                    f"significant slowdown bounded within budget: upper {estimate['upper']:.4f} <= {allowed:.4f}"
                )
            else:
                r["verdict"] = "no_significant_difference"
                r["reasons"].append(
                    f"non-inferior: upper bound {estimate['upper']:.4f} <= allowed {allowed:.4f}; "
                    "not significant after Holm"
                )
        else:
            r["verdict"] = "inconclusive"
            r["reasons"].append(
                f"non-inferiority not established: upper bound {estimate['upper']:.4f} > allowed {allowed:.4f}"
            )
    return results


def worst(verdicts):
    verdicts = list(verdicts)
    if not verdicts:
        return None
    return max(verdicts, key=lambda v: VERDICT_RANK[v])


def geometric_mean(values):
    values = [v for v in values if v and v > 0]
    if not values:
        return None
    return math.exp(sum(math.log(v) for v in values) / len(values))


def budgeted_comparison(reference_metrics, candidate_metrics, budgets, margin_key, alpha_ni, families_in_scope=None):
    """Apply the rule to every candidate series against a reference."""
    decision = budgets["decision"]
    by_family = {}
    series = []
    ids = sorted(set(reference_metrics) | set(candidate_metrics))
    for metric_id in ids:
        left = reference_metrics.get(metric_id)
        right = candidate_metrics.get(metric_id)
        family = (right or left or {}).get("family")
        if families_in_scope is not None and family not in families_in_scope:
            continue
        rule = rule_for(budgets, family, metric_id)
        margin = None if rule is None else rule[margin_key]
        if left is None or right is None:
            missing_side = "reference" if left is None else "candidate"
            result = {
                "id": metric_id,
                "family": family,
                "rule": None if rule is None else rule["match"],
                "status": "fail",
                "reasons": [f"missing data: series absent from the {missing_side}"],
            }
        elif left.get("phase") != right.get("phase"):
            result = {
                "id": metric_id,
                "family": family,
                "rule": None if rule is None else rule["match"],
                "status": "fail",
                "reasons": [f"cold/warm phase mismatch reference={left.get('phase')!r} candidate={right.get('phase')!r}"],
            }
        else:
            result = evaluate_series(metric_id, left, right, rule, margin, alpha_ni)
        by_family.setdefault(family, []).append(result)
        series.append(result)
    tested = [f for f, rs in by_family.items() if any(r["status"] == "ok" for r in rs)]
    alpha_family = decision["alpha"] / max(1, len(tested))
    families = {}
    for family, results in sorted(by_family.items(), key=lambda item: str(item[0])):
        classify_family(results, alpha_family)
        counts = {}
        for r in results:
            counts[r["verdict"]] = counts.get(r["verdict"], 0) + 1
        uncorrected = sum(
            1 for r in results
            if r.get("p_worse") is not None and min(r["p_worse"], r["p_better"]) < decision["alpha"]
        )
        families[family] = {
            "verdict": worst(r["verdict"] for r in results),
            "series": len(results),
            "counts": dict(sorted(counts.items())),
            "uncorrected_significant": uncorrected,
            "geomean_degradation": geometric_mean(
                (r.get("degradation") or {}).get("point") for r in results
            ),
        }
    verdict = worst(r["verdict"] for r in series) or "inconclusive"
    reasons = [
        f"{r['id']}: {'; '.join(r['reasons'])}"
        for r in series
        if r["verdict"] not in PASS_VERDICTS
    ]
    if not series:
        reasons = ["no comparable series"]
    return {
        "verdict": verdict,
        "reasons": reasons,
        "alpha_family": alpha_family,
        "families": families,
        "series": series,
    }


def bundle_growth(reference, candidate, budgets, key):
    """Deterministic relative bundle growth against a budget (no statistics)."""
    out = {"verdict": "no_significant_difference", "reasons": [], "growth": {}}
    limits = budgets["bundle"]["max_relative_growth"]
    if not isinstance(reference, dict) or not reference:
        return {"verdict": "fail", "reasons": ["missing data: reference bundle"], "growth": {}}
    if not isinstance(candidate, dict) or not candidate:
        return {"verdict": "fail", "reasons": ["missing data: candidate bundle"], "growth": {}}
    for name in BUNDLE_KEYS:
        old, new = reference.get(name), candidate.get(name)
        if not isinstance(old, (int, float)) or not isinstance(new, (int, float)) or old <= 0:
            continue
        growth = (new - old) / old
        allowed = limits[name][key]
        out["growth"][name] = {"reference": old, "candidate": new, "relative": growth, "allowed": allowed}
        if growth > allowed:
            out["verdict"] = "slower"
            out["reasons"].append(f"bundle {name} grew {growth:+.2%} > allowed {allowed:.2%}")
    if not out["growth"]:
        return {"verdict": "fail", "reasons": ["missing data: no comparable bundle sizes"], "growth": {}}
    return out


def _slo_values(slo, candidate_side, candidate_metrics):
    """(values, missing) for an SLO, or None only when its source was not selected.

    None means the run did not measure the source at all (an absent series,
    bundle or workload), so the SLO is not_evaluated. A workload that ran is
    evidence the SLO must judge: `missing` lists the 1-based samples that lack
    the field, and the caller fails the SLO closed on any of them rather than
    judging the rest or skipping it.
    """
    if "series" in slo:
        body = candidate_metrics.get(slo["series"])
        return None if body is None else (list(body.get("samples") or []), [])
    if "bundle" in slo:
        value = (candidate_side.get("bundle") or {}).get(slo["bundle"])
        return None if value is None else ([value], [])
    workloads = candidate_side.get("workloads") or {}
    if slo["workload"] not in workloads:
        return None
    body = workloads[slo["workload"]]
    samples = body.get("samples") if isinstance(body, dict) else None
    values = []
    missing = []
    for index, sample in enumerate(samples if isinstance(samples, list) else [], start=1):
        if not isinstance(sample, dict) or sample.get(slo["field"]) is None:
            missing.append(index)
        else:
            values.append(sample[slo["field"]])
    return values, missing


def evaluate_slos(budgets, candidate_side, candidate_metrics):
    results = []
    for slo in budgets.get("slos") or []:
        entry = {"id": slo["id"], "statistic": slo["statistic"], "verdict": "pass", "reasons": []}
        for key in ("series", "workload", "field", "bundle", "max", "min", "equals", "unit"):
            if key in slo:
                entry[key] = slo[key]
        found = _slo_values(slo, candidate_side, candidate_metrics)
        if found is None:
            entry["verdict"] = "not_evaluated"
            entry["reasons"].append("source was not measured in this run")
            results.append(entry)
            continue
        values, missing = found
        if missing:
            # The workload ran, so an absent field is missing evidence, not an
            # unselected source: fail closed instead of judging a subset.
            entry["verdict"] = "fail"
            entry["n"] = len(values) + len(missing)
            entry["reasons"].append(
                f"missing data: workload {slo['workload']} ran but {len(missing)} of {entry['n']} "
                f"samples lack {slo['field']} (samples {missing})"
            )
            results.append(entry)
            continue
        if "equals" not in slo:
            invalid = [(index, value) for index, value in enumerate(values, start=1)
                       if not is_finite_number(value)]
            if invalid:
                entry["verdict"] = "fail"
                entry["n"] = len(values)
                shown = ", ".join(f"sample {index}={value!r}" for index, value in invalid[:5])
                more = f" and {len(invalid) - 5} more" if len(invalid) > 5 else ""
                entry["reasons"].append(
                    f"invalid data: {len(invalid)} of {len(values)} observations are not finite "
                    f"JSON numbers ({shown}{more})"
                )
                results.append(entry)
                continue
        min_samples = slo.get("min_samples", 1)
        entry["n"] = len(values)
        if len(values) < min_samples:
            entry["verdict"] = "inconclusive"
            entry["reasons"].append(f"undersampled: n={len(values)} min_samples={min_samples}")
            results.append(entry)
            continue
        if "equals" in slo:
            bad = [v for v in values if v != slo["equals"]]
            entry["observed"] = sorted({str(v) for v in values})
            if bad:
                entry["verdict"] = "breach"
                entry["reasons"].append(f"{len(bad)} of {len(values)} samples != {slo['equals']!r}")
            results.append(entry)
            continue
        numbers = [float(v) for v in values]  # validated finite above
        statistic = slo["statistic"]
        if statistic == "p90":
            # The 90th-percentile worst sample: the high tail against a
            # ceiling, the low tail against a floor (nearest rank, so n=10
            # excludes exactly the single worst sample).
            if "min" in slo:
                observed = -nearest_rank([-x for x in numbers], 90)
            else:
                observed = nearest_rank(numbers, 90)
        elif statistic == "min":
            observed = min(numbers)
        elif statistic == "max":
            observed = max(numbers)
        elif "min" in slo:  # all/value against a floor: the lowest sample
            observed = min(numbers)
        else:  # all/value against a ceiling or equality: the highest sample
            observed = max(numbers)
        entry["observed"] = observed
        if "max" in slo and observed > slo["max"]:
            entry["verdict"] = "breach"
            entry["reasons"].append(f"{statistic}={observed:.6g} > max {slo['max']}")
        if "min" in slo and observed < slo["min"]:
            entry["verdict"] = "breach"
            entry["reasons"].append(f"{statistic}={observed:.6g} < min {slo['min']}")
        results.append(entry)
    verdicts = [r["verdict"] for r in results]
    if "fail" in verdicts:
        verdict = "fail"
    elif "breach" in verdicts:
        verdict = "slo_breach"
    elif "inconclusive" in verdicts:
        verdict = "inconclusive"
    else:
        verdict = "no_significant_difference"
    return {
        "verdict": verdict,
        "reasons": [f"SLO {r['id']}: {'; '.join(r['reasons'])}" for r in results if r["verdict"] in ("fail", "breach", "inconclusive")],
        "results": results,
        "not_evaluated": [r["id"] for r in results if r["verdict"] == "not_evaluated"],
    }


def baseline_side_metrics(baseline):
    metrics = {}
    for metric_id, body in (baseline.get("metrics") or {}).items():
        if not isinstance(body, dict) or not isinstance(body.get("samples"), list):
            raise CompareError(f"baseline metric {metric_id} has no samples")
        metrics[metric_id] = {
            "samples": [sample_float(x, f"baseline metric {metric_id} sample") for x in body["samples"]],
            "outcomes": ["passed"] * len(body["samples"]),
            "family": body.get("family"),
            "phase": body.get("phase"),
            "lower_is_better": body.get("lower_is_better"),
        }
    return metrics


def fixed_baseline_comparison(baseline, baseline_error, candidate, candidate_metrics, required_families, budgets, alpha_ni):
    """Cumulative-regression verdict against the versioned fixed baseline."""
    if baseline_error:
        return {
            "verdict": "inconclusive",
            "rerun_eligible": False,
            "reasons": [baseline_error],
        }
    if baseline.get("schema_version") != BASELINE_SCHEMA_VERSION:
        return {
            "verdict": "inconclusive",
            "rerun_eligible": False,
            "reasons": [f"fixed baseline schema_version={baseline.get('schema_version')!r}, want {BASELINE_SCHEMA_VERSION}"],
        }
    base_prov = baseline.get("provenance") if isinstance(baseline.get("provenance"), dict) else {}
    cand_prov = candidate.get("provenance") if isinstance(candidate.get("provenance"), dict) else {}
    mismatched = []
    for field in FIXED_BASELINE_MUST_MATCH:
        if field == "benchmark_harness_sha256" and "benchmark" not in required_families:
            continue
        if base_prov.get(field) in (None, "") or base_prov.get(field) != cand_prov.get(field):
            mismatched.append(
                f"fixed baseline provenance.{field} differs: baseline={base_prov.get(field)!r} "
                f"candidate={cand_prov.get(field)!r}"
            )
    summary = {
        "baseline_git_sha": base_prov.get("git_sha"),
        "baseline_recorded_at": baseline.get("recorded_at"),
    }
    if mismatched:
        summary.update({
            "verdict": "inconclusive",
            "rerun_eligible": False,
            "reasons": mismatched + ["refusing to compare against a baseline from a different runner or setup; re-record it with a reviewed budgets change"],
        })
        return summary
    in_scope = [f for f in required_families if f in BUDGETED_FAMILIES]
    if not in_scope:
        in_scope = sorted({m.get("family") for m in candidate_metrics.values()} & set(BUDGETED_FAMILIES))
    missing_families = [f for f in required_families if f != "bundle" and f not in (baseline.get("required_families") or [])]
    if missing_families:
        summary.update({
            "verdict": "inconclusive",
            "rerun_eligible": False,
            "reasons": [f"fixed baseline did not record required families {missing_families}"],
        })
        return summary
    try:
        reference = baseline_side_metrics(baseline)
    except CompareError as err:
        summary.update({"verdict": "inconclusive", "rerun_eligible": False, "reasons": [str(err)]})
        return summary
    result = budgeted_comparison(
        reference, candidate_metrics, budgets, "fixed_baseline_max_relative_degradation", alpha_ni, set(in_scope)
    )
    rerun_eligible = True
    for series in result["series"]:
        if series["verdict"] == "fail" and any("absent from the reference" in r for r in series["reasons"]):
            # A new series needs a re-recorded baseline, not a rerun.
            series["verdict"] = "inconclusive"
            series["reasons"].append("re-record the fixed baseline to include this series")
            rerun_eligible = False
    result["verdict"] = worst(s["verdict"] for s in result["series"]) or "inconclusive"
    result["reasons"] = [
        f"{s['id']}: {'; '.join(s['reasons'])}" for s in result["series"] if s["verdict"] not in PASS_VERDICTS
    ]
    if "bundle" in required_families:
        bundle = bundle_growth(baseline.get("bundle") or {}, candidate.get("bundle") or {}, budgets, "fixed_baseline")
        result["bundle"] = bundle
        result["verdict"] = worst([result["verdict"], bundle["verdict"]])
        result["reasons"].extend(bundle["reasons"])
    summary.update(result)
    summary["rerun_eligible"] = rerun_eligible
    return summary


def rerun_state(verdict, rerun_eligible, attempt, policy):
    allowed = policy["max_reruns"] + 1
    state = {
        "attempt": attempt,
        "max_reruns": policy["max_reruns"],
        "attempts_allowed": allowed,
        "rerun_on": "inconclusive",
        "between_attempts": policy["between_attempts"],
        "pooling": policy.get("pooling"),
    }
    if verdict != "inconclusive":
        state["status"] = "resolved"
        state["rerun_required"] = False
    elif not rerun_eligible:
        state["status"] = "unresolved_not_rerunnable"
        state["rerun_required"] = False
    elif attempt < allowed:
        state["status"] = "rerun_required"
        state["rerun_required"] = True
        state["next_attempt"] = attempt + 1
    else:
        state["status"] = "exhausted"
        state["rerun_required"] = False
    state["exhausted"] = state["status"] in ("exhausted", "unresolved_not_rerunnable")
    return state


def optimization_claim(target):
    faster = [s["id"] for s in target["series"] if s.get("verdict") == "faster"]
    if not faster:
        return None
    tradeoffs = sorted(
        (
            (s["degradation"]["point"], s["id"])
            for s in target["series"]
            if s.get("degradation") and s["degradation"]["point"] > 1.0 and s["id"] not in faster
        ),
        reverse=True,
    )
    return {
        "improved_series": faster,
        "tradeoff_candidates": [{"id": i, "degradation": p} for p, i in tradeoffs],
        "note": (
            "An optimization claim must name its tradeoffs: the series listed here moved in "
            "the worse direction (not necessarily significantly). Neutral performance is a valid result."
        ),
    }


def compare(doc, min_samples=DEFAULT_MIN_SAMPLES, max_cv=DEFAULT_MAX_CV, alpha=ALPHA,
            budgets=None, baseline=None, baseline_error=None, attempt=1):
    """Compare a document. Without budgets this is E3's uncorrected rule;
    with budgets the E4 decision rule sets `overall` (see apply_budgets)."""
    report = _compare_uncorrected(doc, min_samples, max_cv, alpha)
    if budgets is None:
        report.pop("_internal", None)
        return report
    return apply_budgets(report, budgets, baseline, baseline_error, attempt)


def _compare_uncorrected(doc, min_samples, max_cv, alpha):
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
            "multiplicity": None,
            "control": doc.get("control"),
            "provenance": prov,
            "required_families": required_families,
            "min_samples": min_samples,
            "max_cv": max_cv,
            "alpha": alpha,
            "_internal": {
                "base": base,
                "candidate": candidate,
                "base_metrics": base_metrics,
                "candidate_metrics": cand_metrics,
                "fail_reasons": list(fail_reasons),
                "mismatched": list(prov["mismatched"]),
            },
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
                    "diagnostics": None,
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
                    "diagnostics": None,
                }
            )
            continue
        metric_results.append(compare_metric(metric_id, left, right, min_samples, max_cv, alpha))
        speed_compared = True

    overall, reasons = rollup(metric_results, fail_reasons, inconclusive_reasons)
    if overall == "equivalent":
        overall = "no_significant_difference"
        reasons.append("equivalent is forbidden; coerced to no_significant_difference")
    multiplicity = multiplicity_summary(metric_results, alpha)
    report = {
        "schema_version": SCHEMA_VERSION,
        "overall": overall,
        "reasons": reasons,
        "speed_compared": speed_compared,
        "metrics": metric_results,
        "bundle": bundle_result,
        "benchstat": benchstat_text(metric_results, multiplicity),
        "multiplicity": multiplicity,
        "control": doc.get("control"),
        "provenance": prov,
        "required_families": required_families,
        "min_samples": min_samples,
        "max_cv": max_cv,
        "alpha": alpha,
        "_internal": {
            "base": base,
            "candidate": candidate,
            "base_metrics": base_metrics,
            "candidate_metrics": cand_metrics,
            "fail_reasons": [],
            "mismatched": list(prov["mismatched"]),
        },
    }
    return report


def apply_budgets(report, budgets, baseline, baseline_error, attempt):
    """Replace the uncorrected overall verdict with the budgeted decision."""
    internal = report.pop("_internal")
    base = internal["base"]
    candidate = internal["candidate"]
    base_metrics = internal["base_metrics"]
    cand_metrics = internal["candidate_metrics"]
    policy = budgets["rerun_policy"]
    allowed = policy["max_reruns"] + 1
    if type(attempt) is not int or attempt < 1 or attempt > allowed:
        raise CompareError(
            f"attempt must be between 1 and {allowed} (rerun_policy.max_reruns={policy['max_reruns']})"
        )
    decision_cfg = budgets["decision"]
    alpha_ni = decision_cfg["alpha_non_inferiority"] / allowed
    required = report["required_families"]
    uncorrected_overall = report["overall"]

    if internal["fail_reasons"]:
        target = {
            "verdict": "fail",
            "reasons": list(report["reasons"]),
            "families": {},
            "series": [],
        }
        slos = {
            "verdict": "not_evaluated",
            "reasons": ["correctness/provenance failed closed"],
            "results": [],
            "not_evaluated": [],
        }
        fixed = {"verdict": "not_evaluated", "reasons": ["correctness/provenance failed closed"]}
    else:
        target = budgeted_comparison(base_metrics, cand_metrics, budgets, "max_relative_degradation", alpha_ni)
        target["rerun_eligible"] = True
        if base.get("bundle") or candidate.get("bundle") or "bundle" in required:
            bundle = bundle_growth(base.get("bundle") or {}, candidate.get("bundle") or {}, budgets, "target_base")
            target["bundle"] = bundle
            target["verdict"] = worst([target["verdict"], bundle["verdict"]])
            target["reasons"].extend(bundle["reasons"])
        if internal["mismatched"]:
            # Different environments: no series verdict is meaningful, and a
            # rerun on the same mismatched pair cannot fix it.
            target["verdict"] = "fail" if target["verdict"] == "fail" else "inconclusive"
            target["reasons"] = list(internal["mismatched"]) + target["reasons"]
            target["rerun_eligible"] = False
        slos = evaluate_slos(budgets, candidate, cand_metrics)
        noisy = [s["id"] for s in target.get("series") or []
                 if any(reason.startswith("noisy:") for reason in s.get("reasons") or [])]
        if slos["verdict"] == "slo_breach" and noisy:
            # The same run shows host contention (series over their max_cv),
            # so an absolute-ceiling breach is not attributable to the
            # candidate yet: rerun instead of failing on runner noise. A
            # breach on a quiet run is terminal.
            slos["verdict"] = "inconclusive"
            slos["rerun_eligible"] = True
            slos["reasons"] = [
                f"{reason} (on a noisy run: {', '.join(noisy)}; rerun before treating it as a breach)"
                for reason in slos["reasons"]
            ]
        if baseline is None and baseline_error is None:
            fixed = {
                "verdict": "not_requested",
                "reasons": ["no --baseline supplied; the strict gate requires one"],
            }
        else:
            fixed = fixed_baseline_comparison(
                baseline, baseline_error, candidate, cand_metrics, required, budgets, alpha_ni
            )

    parts = [("target_base", target), ("slos", slos)]
    # A passing fixed-baseline verdict never upgrades the overall result (an
    # optimization claim is about this change against its base); any other
    # fixed-baseline verdict can only make it worse.
    if fixed["verdict"] not in ("not_requested", "not_evaluated") + PASS_VERDICTS:
        parts.append(("fixed_baseline", fixed))
    overall = worst(part["verdict"] for _, part in parts if part["verdict"] in VERDICT_RANK)
    rerun_eligible = True
    for name, part in parts:
        if part["verdict"] != "inconclusive":
            continue
        if not part.get("rerun_eligible", False):
            # An undersampled SLO or a mismatched environment is not fixed by
            # repeating the same run.
            rerun_eligible = False
    reruns = rerun_state(overall, rerun_eligible, attempt, policy)
    if overall == "inconclusive" and reruns["exhausted"]:
        overall = "inconclusive_unresolved"
    reasons = []
    for name, part in parts:
        if part["verdict"] not in PASS_VERDICTS:
            reasons.extend(f"{name}: {reason}" for reason in part.get("reasons") or [])
    if overall in PASS_VERDICTS and not reasons:
        reasons = {
            "no_significant_difference": [
                "every series is non-inferior within its budget and no change is significant after Holm; "
                "this is a bounded non-inferiority result, not equivalence"
            ],
            "faster": [
                "every series is non-inferior; some improve significantly after Holm (state the tradeoffs)"
            ],
            "within_budget": ["a significant slowdown is bounded within its allowed degradation"],
        }[overall]
    if reruns["status"] == "rerun_required":
        reasons.append(
            f"inconclusive on attempt {attempt} of {allowed}: rerun required "
            f"({'; '.join(policy['between_attempts'])})"
        )
    elif overall == "inconclusive_unresolved":
        reasons.append(
            f"inconclusive_unresolved after attempt {attempt} of {allowed} ({reruns['status']}); "
            "a strict gate must block"
        )

    series_by_id = {s["id"]: s for s in target.get("series") or []}
    for metric in report["metrics"]:
        metric["uncorrected_verdict"] = metric["verdict"]
        budgeted = series_by_id.get(metric["id"])
        if budgeted is not None:
            metric["verdict"] = budgeted["verdict"]
            metric["budget"] = {
                key: budgeted.get(key)
                for key in (
                    "rule", "max_relative_degradation", "min_samples", "max_cv", "degradation",
                    "p_worse", "p_better", "holm_p_worse", "holm_p_better", "alpha_family", "reasons",
                )
            }
    report["uncorrected_overall"] = uncorrected_overall
    report["overall"] = overall
    report["reasons"] = reasons
    report["decision"] = {
        "rule": "budgeted",
        "budgets_values_sha256": budget_values_sha256(budgets),
        "budgets_version": budgets["changes"][-1].get("version"),
        "attempt": attempt,
        "alpha": decision_cfg["alpha"],
        "alpha_non_inferiority_total": decision_cfg["alpha_non_inferiority"],
        "alpha_non_inferiority_per_attempt": alpha_ni,
        "target_base": target,
        "fixed_baseline": fixed,
        "slos": slos,
        "optimization_claim": optimization_claim(target) if target.get("series") else None,
    }
    report["reruns"] = reruns
    report["strict_gate"] = {
        "blocking": EXIT_BY_OVERALL.get(overall, 2) != 0 or fixed["verdict"] == "not_requested",
        "fixed_baseline_evaluated": fixed["verdict"] not in ("not_requested", "not_evaluated"),
        "note": (
            "inconclusive and inconclusive_unresolved block the strict gate; so does a run "
            "without the fixed-baseline comparison"
        ),
    }
    if report["speed_compared"]:
        report["benchstat"] = benchstat_text(report["metrics"], report["multiplicity"]) + decision_text(report)
    return report


def decision_text(report):
    decision = report["decision"]
    lines = [
        f"budgeted decision (attempt {decision['attempt']}/{report['reruns']['attempts_allowed']}): "
        f"target_base={decision['target_base']['verdict']} "
        f"fixed_baseline={decision['fixed_baseline']['verdict']} slos={decision['slos']['verdict']} "
        f"-> {report['overall']}",
    ]
    for family, body in (decision["target_base"].get("families") or {}).items():
        geo = body.get("geomean_degradation")
        geo_s = "n/a" if geo is None else f"{geo:.4f}"
        lines.append(
            f"  {family}: {body['verdict']} series={body['series']} counts={body['counts']} "
            f"uncorrected_significant={body['uncorrected_significant']} geomean_degradation={geo_s}"
        )
    return "\n".join(lines) + "\n"


EXIT_BY_OVERALL = {
    "faster": 0,
    "no_significant_difference": 0,
    "within_budget": 0,
    "fail": 2,
    "inconclusive": 3,
    "inconclusive_unresolved": 3,
    "slower": 4,
    "slo_breach": 4,
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


def ks_uniform_p(pvalues):
    """Asymptotic Kolmogorov-Smirnov p-value for uniformity on [0, 1]."""
    xs = sorted(float(p) for p in pvalues)
    n = len(xs)
    if n == 0:
        return None
    d = max(max((i + 1) / n - x, x - i / n) for i, x in enumerate(xs))
    lam = (math.sqrt(n) + 0.12 + 0.11 / math.sqrt(n)) * d
    total = 0.0
    for k in range(1, 101):
        term = 2.0 * (-1) ** (k - 1) * math.exp(-2.0 * k * k * lam * lam)
        total += term
        if abs(term) < 1e-12:
            break
    return min(1.0, max(0.0, total))


# Provenance proving that both sides of an A/A control are one build of one
# source. compare() deliberately lets these differ (that is what an A/B is),
# so the `control` label alone never establishes the same-code premise.
A_A_IDENTITY = (
    "git_sha",
    "image_id",
    "image_ref",
    "cli_digest",
    "builder_image_id",
    "toolchain_id",
    "go_version",
)


def a_a_identity_issues(doc):
    """Why a document is not a same-build A/A control (empty when it is)."""
    if not isinstance(doc, dict):
        return ["comparison document must be an object"]
    issues = []
    if doc.get("control") != "a_a":
        issues.append(f"control={doc.get('control')!r}, want 'a_a'")
    provs = {}
    for label in ("base", "candidate"):
        side = doc.get(label)
        prov = side.get("provenance") if isinstance(side, dict) else None
        provs[label] = prov if isinstance(prov, dict) else {}
    base, cand = provs["base"], provs["candidate"]
    for field in A_A_IDENTITY:
        left, right = base.get(field), cand.get(field)
        if left in (None, "") or right in (None, ""):
            issues.append(f"provenance.{field} is missing (base={left!r} candidate={right!r})")
        elif left != right:
            issues.append(f"provenance.{field} differs: base={left!r} candidate={right!r}")
    return issues


def calibrate(docs, budgets, confidence_z=2.878):
    """Summarize same-code control runs under the budgets (E4 evidence).

    `docs` is a list of (label, comparison document). Every document must be
    an A/A control: `control: a_a` AND one build of one source on both sides
    (A_A_IDENTITY). Any document that is not is refused before anything is
    pooled, so an A/B can never be counted as runner noise. The summary
    reports each run's budgeted verdict, the observed control false-positive
    rates, pooled per-series variance, the smallest sample size and margin the
    variance supports, and cross-run drift: every ordered pair of different
    runs' candidate sides compared under the fixed-baseline margins, which is
    what a non-interleaved fixed baseline sees.
    """
    allowed = budgets["rerun_policy"]["max_reruns"] + 1
    alpha_ni = budgets["decision"]["alpha_non_inferiority"] / allowed
    z_ni = _NORMAL.inv_cdf(1.0 - alpha_ni)
    runs = []
    pooled_p = []
    per_series = {}
    candidates = []
    refused = []
    for label, doc in docs:
        issues = a_a_identity_issues(doc)
        if issues:
            refused.append(f"{label} is not an A/A control run: {'; '.join(issues)}")
    if refused:
        raise CompareError(
            "refusing calibration evidence (an A/A control measures one build of one source on "
            "both sides): " + " | ".join(refused)
        )
    for label, doc in docs:
        report = compare(doc, budgets=budgets)
        target = report["decision"]["target_base"]
        informative = [
            m for m in report["metrics"]
            if m.get("p_value") is not None and not is_degenerate(m)
        ]
        pooled_p.extend(m["p_value"] for m in informative)
        runs.append({
            "label": label,
            "overall": report["overall"],
            "target_base": target["verdict"],
            "slos": report["decision"]["slos"]["verdict"],
            "uncorrected_overall": report["uncorrected_overall"],
            "series": len(target.get("series") or []),
            "families": {
                family: {
                    "verdict": body["verdict"],
                    "counts": body["counts"],
                    "uncorrected_significant": body["uncorrected_significant"],
                    "geomean_degradation": body["geomean_degradation"],
                }
                for family, body in (target.get("families") or {}).items()
            },
            "uncorrected_significant": sum(1 for m in informative if m["p_value"] < budgets["decision"]["alpha"]),
            "min_uncorrected_p": min((m["p_value"] for m in informative), default=None),
            "non_pass_series": [
                {"id": s["id"], "verdict": s["verdict"], "reasons": s["reasons"]}
                for s in target.get("series") or []
                if s["verdict"] not in PASS_VERDICTS
            ],
            "slo_results": [
                {"id": s["id"], "verdict": s["verdict"], "observed": s.get("observed")}
                for s in report["decision"]["slos"]["results"]
            ],
        })
        for s in target.get("series") or []:
            entry = per_series.setdefault(s["id"], {
                "family": s.get("family"), "rule": s.get("rule"),
                "margin": s.get("max_relative_degradation"), "cv": [], "upper": [], "point": [],
                "sigma_log": [],
            })
            for key in ("cv_reference", "cv_candidate"):
                if s.get(key) is not None:
                    entry["cv"].append(s[key])
            if s.get("degradation") and s["degradation"].get("upper") is not None:
                entry["upper"].append(s["degradation"]["upper"])
                entry["point"].append(s["degradation"]["point"])
        base_metrics = expand_side_metrics(doc["base"])
        cand_metrics = expand_side_metrics(doc["candidate"])
        for metric_id, body in cand_metrics.items():
            entry = per_series.get(metric_id)
            if entry is None:
                continue
            for side_metrics in (base_metrics, cand_metrics):
                samples = (side_metrics.get(metric_id) or {}).get("samples") or []
                if len(samples) > 2 and all(x > 0 for x in samples):
                    logs = [math.log(x) for x in samples]
                    med = statistics.median(logs)
                    mad = statistics.median(abs(x - med) for x in logs)
                    entry["sigma_log"].append({"sd": stdev(logs), "mad_sigma": 1.4826 * mad, "n": len(samples)})
        candidates.append((label, doc))

    series_summary = {}
    for metric_id, entry in sorted(per_series.items()):
        sds = sorted(s["sd"] for s in entry["sigma_log"])
        robust = sorted(s["mad_sigma"] for s in entry["sigma_log"])
        ns = [s["n"] for s in entry["sigma_log"]]
        summary = {
            "family": entry["family"],
            "rule": entry["rule"],
            "margin": entry["margin"],
            "runs": len(entry["upper"]),
            "max_cv": max(entry["cv"]) if entry["cv"] else None,
            "median_cv": statistics.median(entry["cv"]) if entry["cv"] else None,
            "max_upper": max(entry["upper"]) if entry["upper"] else None,
            "points": [round(p, 5) for p in entry["point"]],
        }
        if sds and ns:
            # Pooled log-scale sigma: the median side, and the worst side.
            sigma = statistics.median(sds)
            sigma_hi = sds[-1]
            n = min(ns)
            se = 1.05 * sigma * math.sqrt(2.0 / n)
            summary.update({
                "sigma_log_median": sigma,
                "sigma_log_max": sigma_hi,
                "sigma_log_robust_median": statistics.median(robust) if robust else None,
                "n": n,
                "expected_null_upper": math.exp(z_ni * se),
                # Margin the median variance supports with the A/A upper bound
                # exceeding it at most ~0.2% of the time per series.
                "supported_margin": math.exp((z_ni + confidence_z) * se) - 1.0,
                # Smallest n at which the expected A/A upper bound uses at most
                # half of the budgeted margin.
                "min_samples_for_margin": (
                    None if not entry["margin"] or sigma == 0 else
                    max(3, math.ceil(2.0 * (2.0 * z_ni * 1.05 * sigma / math.log1p(entry["margin"])) ** 2))
                ),
            })
        series_summary[metric_id] = summary

    pairs = []
    for i, (label_a, doc_a) in enumerate(candidates):
        for j, (label_b, doc_b) in enumerate(candidates):
            if i == j:
                continue
            reference = expand_side_metrics(doc_a["candidate"])
            candidate = expand_side_metrics(doc_b["candidate"])
            result = budgeted_comparison(
                reference, candidate, budgets, "fixed_baseline_max_relative_degradation", alpha_ni
            )
            worst_upper = max(
                ((s["degradation"]["upper"], s["id"]) for s in result["series"]
                 if s.get("degradation") and s["degradation"].get("upper") is not None),
                default=(None, None),
            )
            pairs.append({
                "reference": label_a,
                "candidate": label_b,
                "verdict": result["verdict"],
                "worst_upper": worst_upper[0],
                "worst_upper_series": worst_upper[1],
                "non_pass_series": [
                    {"id": s["id"], "verdict": s["verdict"], "reasons": s["reasons"]}
                    for s in result["series"] if s["verdict"] not in PASS_VERDICTS
                ],
            })

    def rate(items, predicate):
        return None if not items else sum(1 for item in items if predicate(item)) / len(items)

    return {
        "schema_version": 1,
        "budgets_values_sha256": budget_values_sha256(budgets),
        "alpha_non_inferiority_per_attempt": alpha_ni,
        "runs": runs,
        "control_rates": {
            "runs": len(runs),
            "false_fail": rate(runs, lambda r: r["target_base"] in ("slower", "fail")),
            "inconclusive": rate(runs, lambda r: r["target_base"] == "inconclusive"),
            "pass": rate(runs, lambda r: r["target_base"] in PASS_VERDICTS),
            "uncorrected_any_significant": rate(runs, lambda r: r["uncorrected_significant"] > 0),
            "uncorrected_overall_not_nsd": rate(runs, lambda r: r["uncorrected_overall"] != "no_significant_difference"),
        },
        "pooled_uncorrected_p": {
            "count": len(pooled_p),
            "below_alpha": sum(1 for p in pooled_p if p < budgets["decision"]["alpha"]),
            "ks_uniform_p": ks_uniform_p(pooled_p),
            "note": "two-sided uncorrected Mann-Whitney p-values of informative series; tie/continuity correction makes them conservative",
        },
        "cross_run_fixed_baseline": {
            "pairs": pairs,
            "false_fail": rate(pairs, lambda p: p["verdict"] in ("slower", "fail")),
            "inconclusive": rate(pairs, lambda p: p["verdict"] == "inconclusive"),
            "pass": rate(pairs, lambda p: p["verdict"] in PASS_VERDICTS),
        },
        "series": series_summary,
    }


def summary_stats(samples):
    ys = sorted(float(x) for x in samples)
    med = statistics.median(ys)
    return {
        "n": len(ys),
        "median": med,
        "mad": statistics.median(abs(y - med) for y in ys),
        "iqr": percentile(ys, 75) - percentile(ys, 25),
        "p95": nearest_rank(ys, 95),
        "p99": nearest_rank(ys, 99),
        "mean": mean(ys),
        "cv": cv(ys),
        "min": ys[0],
        "max": ys[-1],
    }


def record_baseline(doc, report, runner=None, settings=None, recorded_at=None, source=None):
    """Build baseline.json from a passing run's candidate side.

    Refuses a run whose correctness, provenance or target-base verdict did
    not pass, an instrumented image, or an image this run did not build.
    """
    decision = report.get("decision") or {}
    target = decision.get("target_base") or {}
    if report.get("overall") == "fail" or target.get("verdict") not in PASS_VERDICTS:
        raise CompareError(
            f"refusing to record a fixed baseline from a run whose target-base verdict is "
            f"{target.get('verdict')!r} (overall {report.get('overall')!r})"
        )
    candidate = doc["candidate"]
    prov = candidate.get("provenance") or {}
    if prov.get("instrumented") is not False:
        raise CompareError("refusing to record a fixed baseline from an instrumented image")
    if prov.get("built_by_this_run") is not True:
        raise CompareError("refusing to record a fixed baseline from an image this run did not build")
    required = list(doc.get("required_families") or [])
    metrics = {}
    for metric_id, body in sorted(expand_side_metrics(candidate).items()):
        if body.get("family") not in BUDGETED_FAMILIES or not body.get("samples"):
            continue
        entry = {
            "family": body["family"],
            "phase": body.get("phase"),
            "lower_is_better": body.get("lower_is_better"),
        }
        entry.update(summary_stats(body["samples"]))
        entry["samples"] = list(body["samples"])
        metrics[metric_id] = entry
    fields = REQUIRED_PROVENANCE + ("built_by_this_run", "benchmark_harness_sha256", "benchmark_repeats")
    return {
        "schema_version": BASELINE_SCHEMA_VERSION,
        "description": (
            "Versioned FIXED performance baseline (E4). The comparator repeats its budgeted "
            "rule against these samples to expose cumulative regression that a target-base "
            "comparison cannot see. It is comparable only on the recorded runner and setup "
            "(budgets.fixed_baseline.must_match); anything else is inconclusive. Re-record it "
            "only with a reviewed budgets change."
        ),
        "recorded_at": recorded_at or doc.get("measured_at"),
        "source": source or {},
        "provenance": {field: prov.get(field) for field in fields if field in prov},
        "runner": runner or {},
        "settings": settings or doc.get("settings") or {},
        "required_families": required,
        "report": {
            "overall": report.get("overall"),
            "target_base": target.get("verdict"),
            "budgets_values_sha256": decision.get("budgets_values_sha256"),
        },
        "metrics": metrics,
        "bundle": {key: (candidate.get("bundle") or {}).get(key) for key in BUNDLE_KEYS
                   if key in (candidate.get("bundle") or {})},
    }


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("input", nargs="?", help="comparison document JSON")
    parser.add_argument("--input", dest="input_flag", help="comparison document JSON")
    parser.add_argument("--base", help="base side JSON (used with --candidate)")
    parser.add_argument("--candidate", help="candidate side JSON (used with --base)")
    parser.add_argument("--benchmark-evidence", help="comparison JSON carrying the shared benchmark harness and sampling manifest for --base/--candidate")
    parser.add_argument("--output", "-o", help="write the JSON report to this path as well as stdout")
    parser.add_argument("--min-samples", type=int, default=DEFAULT_MIN_SAMPLES,
                        help="uncorrected (E3) per-series rule only; budgets set the decision")
    parser.add_argument("--max-cv", type=float, default=DEFAULT_MAX_CV,
                        help="uncorrected (E3) per-series rule only; budgets set the decision")
    parser.add_argument("--alpha", type=float, default=ALPHA,
                        help="uncorrected (E3) per-series rule only; budgets set the decision")
    parser.add_argument("--budgets", default=str(DEFAULT_BUDGETS),
                        help="budgets.json that sets the decision rule (default: %(default)s)")
    parser.add_argument("--baseline",
                        help="fixed baseline JSON for the cumulative-regression verdict; a missing file is inconclusive")
    parser.add_argument("--attempt", type=int, default=1,
                        help="1-based attempt number under budgets.rerun_policy (default 1)")
    parser.add_argument("--record-baseline", metavar="OUT",
                        help="write a fixed baseline from this run's candidate side (target-base verdict must pass)")
    parser.add_argument("--runner-json", help="runner identity JSON recorded into --record-baseline")
    parser.add_argument("--settings-json", help="CAESIUM_PERF_* settings JSON recorded into --record-baseline")
    parser.add_argument("--recorded-at", help="timestamp recorded into --record-baseline")
    parser.add_argument("--calibrate", nargs="+", metavar="COMPARISON",
                        help="summarize same-code A/A control comparison documents under the budgets (E4 evidence)")
    parser.add_argument(
        "--bundle-dir",
        help="evaluate a dist/assets directory against largest-chunk AND total-route-asset budgets",
    )
    args = parser.parse_args(argv)

    if args.bundle_dir:
        result = evaluate_bundle_dir(args.bundle_dir)
        sys.stdout.write(dump_json(result))
        return 0 if result["ok"] else 2

    if args.calibrate:
        try:
            budgets = load_budgets(args.budgets)
            summary = calibrate([(path, load_json(path)) for path in args.calibrate], budgets)
        except CompareError as err:
            sys.stderr.write(f"error: {err}\n")
            return 1
        encoded = dump_json(summary)
        sys.stdout.write(encoded)
        if args.output:
            Path(args.output).write_text(encoded)
        return 0

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
        budgets = load_budgets(args.budgets)
        baseline = None
        baseline_error = None
        if args.baseline and not args.record_baseline:
            if not Path(args.baseline).is_file():
                baseline_error = f"fixed baseline is missing: {args.baseline}"
            else:
                try:
                    baseline = load_json(args.baseline)
                except CompareError as err:
                    baseline_error = f"fixed baseline is unreadable: {err}"
                if baseline is not None and not isinstance(baseline, dict):
                    baseline, baseline_error = None, "fixed baseline must be an object"
        report = compare(
            doc, min_samples=args.min_samples, max_cv=args.max_cv, alpha=args.alpha,
            budgets=budgets, baseline=baseline, baseline_error=baseline_error, attempt=args.attempt,
        )
        if args.record_baseline:
            runner = load_json(args.runner_json) if args.runner_json else None
            settings = load_json(args.settings_json) if args.settings_json else None
            source = {
                "comparison_sha256": hashlib.sha256(canonical_json(doc).encode()).hexdigest(),
                "base_git_sha": ((doc.get("base") or {}).get("provenance") or {}).get("git_sha"),
                "control": doc.get("control"),
            }
            recorded = record_baseline(doc, report, runner, settings, args.recorded_at, source)
            Path(args.record_baseline).write_text(dump_json(recorded))
            sys.stderr.write(
                f"recorded fixed baseline {args.record_baseline}: {len(recorded['metrics'])} series from "
                f"{recorded['provenance'].get('git_sha')}\n"
            )
            return 0
    except CompareError as err:
        sys.stderr.write(f"error: {err}\n")
        return 1

    encoded = dump_json(report)
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
