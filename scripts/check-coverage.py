#!/usr/bin/env python3
"""Validate labelled CLI/server/browser coverage with provenance.

Fail-closed, stdlib-only. A missing or killed GOCOVERDIR profile is incomplete
evidence, never 0% coverage and never a pass. Unit, integration (CLI+server)
and browser contributions are reported separately. Package/diff floors come
from a measured baseline; there is no global percentage gate.

  python3 scripts/check-coverage.py \\
      --profiles-dir "$ARTIFACTS/profiles" \\
      --repo-root . \\
      --report "$ARTIFACTS/report.json"

Exit status:
  0  complete required contributions, write-to-read path covered, ratchets hold
  1  failed closed: schema, foreign/performance profile, contamination,
     ratchet drop, or a complete integration profile that misses write-to-read
  2  incomplete: missing/killed/empty-GOCOVERDIR profile, or a required
     reagents/browser contribution that was not collected
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path


ROOT_MODULE = "github.com/caesium-cloud/caesium"
REAGENTS_MODULE = "github.com/caesium-cloud/caesium/reagents"

SOURCES = ("unit", "cli", "server", "integration", "browser", "reagents")
INTEGRATION_PARTS = ("cli", "server")
OPTIONAL_SOURCES = ("unit", "browser", "reagents")

PROFILE_LINE = re.compile(
    r"^(?P<file>.+):(?P<sl>\d+)\.(?P<sc>\d+),(?P<el>\d+)\.(?P<ec>\d+) "
    r"(?P<stmts>\d+) (?P<count>\d+)$"
)
SHA_RE = re.compile(r"^[0-9a-fA-F]{40}$|^[0-9a-fA-F]{64}$")
HEX = set("0123456789abcdefABCDEF")

FORBIDDEN_PACKAGE_MARKERS = (
    "/test/performance",
    "/test/load",
    "/test/robustness",
)

PERFORMANCE_LABELS = ("performance", "perf", "load-test", "loadtest")

WRITE_TO_READ = {
    "id": "jobdef-apply-export",
    "description": (
        "CLI/HTTP POST /v1/jobdefs/apply persists a job; "
        "GET /v1/jobs/:id/manifest (caesium job export) reads it back"
    ),
    "write": (
        "github.com/caesium-cloud/caesium/cmd/job/apply.go",
        "github.com/caesium-cloud/caesium/api/rest/controller/jobdef/apply.go",
        "github.com/caesium-cloud/caesium/internal/jobdef/importer.go",
    ),
    "read": (
        "github.com/caesium-cloud/caesium/cmd/job/export.go",
        "github.com/caesium-cloud/caesium/api/rest/controller/job/manifest.go",
        "github.com/caesium-cloud/caesium/internal/jobdef/exporter.go",
    ),
}

# init() of cmd/job/apply.go and export.go runs on every instrumented binary
# start, including `caesium start` and `--help`. File-level hits are not a
# request-to-write-to-read proof. Each row must be covered in the named
# contribution's own profile (server-side evidence from server.out).
WRITE_TO_READ_EVIDENCE = (
    {
        "id": "cli_write",
        "source": "cli",
        "file": "github.com/caesium-cloud/caesium/cmd/job/apply.go",
        "funcs": ("sendApplyRequest", "RunE"),
        "side": "write",
    },
    {
        "id": "cli_read",
        "source": "cli",
        "file": "github.com/caesium-cloud/caesium/cmd/job/export.go",
        "funcs": ("exportGet", "RunE"),
        "side": "read",
    },
    {
        "id": "server_write_http",
        "source": "server",
        "file": "github.com/caesium-cloud/caesium/api/rest/controller/jobdef/apply.go",
        "funcs": ("Apply",),
        "side": "write",
    },
    {
        "id": "server_write_persist",
        "source": "server",
        "file": "github.com/caesium-cloud/caesium/internal/jobdef/importer.go",
        "funcs": ("Apply", "ApplyWithOptions"),
        "side": "write",
    },
    {
        "id": "server_read_http",
        "source": "server",
        "file": "github.com/caesium-cloud/caesium/api/rest/controller/job/manifest.go",
        "funcs": ("Manifest",),
        "side": "read",
    },
    {
        "id": "server_read_persist",
        "source": "server",
        "file": "github.com/caesium-cloud/caesium/internal/jobdef/exporter.go",
        "funcs": ("Export",),
        "side": "read",
    },
)

_FUNC_HEAD = re.compile(
    r"^(?:func\s+(?:\([^)]*\)\s+)?(?P<name>\w+)\s*\(|(?P<rune>RunE)\s*:\s*func\s*\()"
)
_FUNC_INDEX = {}

CONTRACT_PATHS = {
    "DT-ADMIT-01": (
        "api/rest/controller/job/run/post.go",
        "api/rest/service/run/run.go",
    ),
    "DT-OWNER-01": (
        "internal/run/owner_manager.go",
        "internal/run/owner_state.go",
    ),
    "DT-COMPLETE-01": (
        "internal/dispatch/dispatch.go",
        "internal/dispatch/internal_server.go",
    ),
    "DT-TERMINAL-01": (
        "api/rest/controller/job/run/get.go",
        "internal/run/store.go",
    ),
    "DT-RETRY-01": (
        "api/rest/controller/job/run/retry.go",
        "cmd/run/retry.go",
    ),
    "DT-DAG-01": (
        "api/rest/controller/jobdef/apply.go",
        "internal/jobdef/importer.go",
    ),
    "DT-CANCEL-01": (
        "api/rest/controller/job/queue/delete.go",
        "cmd/job/queue.go",
    ),
    "DT-EVENT-01": (
        "api/rest/controller/event/stream.go",
        "cmd/event/event.go",
    ),
    "DT-AUTH-01": (
        "api/middleware/auth.go",
        "cmd/auth/auth.go",
    ),
    "DT-RECOVER-01": (
        "internal/run/recovery.go",
        "cmd/start/start.go",
    ),
    "DT-QUORUM-01": (
        "pkg/dqlite/dqlite.go",
        "api/health.go",
    ),
}


class Issue:
    __slots__ = ("verdict", "code", "target", "message")

    def __init__(self, verdict, code, message, target=""):
        self.verdict = verdict
        self.code = code
        self.target = target
        self.message = message

    def label(self):
        return f"{self.code}={self.target}" if self.target else self.code

    def as_dict(self):
        return {
            "verdict": self.verdict,
            "code": self.code,
            "target": self.target,
            "message": self.message,
        }


def _fail(code, message, target=""):
    return Issue("fail", code, message, target)


def _incomplete(code, message, target=""):
    return Issue("incomplete", code, message, target)


def load_json(path):
    try:
        raw = Path(path).read_text()
    except OSError as err:
        raise ValueError(f"cannot read {path}: {err}") from err
    try:
        return json.loads(raw)
    except json.JSONDecodeError as err:
        raise ValueError(f"{path} is not JSON: {err}") from err


def _is_sha(value):
    return isinstance(value, str) and bool(SHA_RE.fullmatch(value))


def parse_go_funcs(text):
    """Package-level funcs and cobra RunE literals with 1-based line ranges.

    `func init()` is included so callers can exclude those blocks. Nested
    functions share the outer range; that is enough to tell init from RunE.
    """
    funcs = []
    current = None
    depth = 0
    seen_brace = False
    for lineno, line in enumerate(text.splitlines(), start=1):
        code = line.split("//", 1)[0]
        if current is None:
            match = _FUNC_HEAD.match(line.lstrip())
            if match is None:
                continue
            name = match.group("name") or match.group("rune")
            current = {"name": name, "start": lineno, "end": lineno}
            depth = code.count("{") - code.count("}")
            seen_brace = "{" in code
            if seen_brace and depth <= 0:
                funcs.append(current)
                current = None
                seen_brace = False
            continue
        depth += code.count("{") - code.count("}")
        current["end"] = lineno
        if "{" in code:
            seen_brace = True
        if seen_brace and depth <= 0:
            funcs.append(current)
            current = None
            seen_brace = False
    if current is not None:
        funcs.append(current)
    return funcs


def _repo_file(file_path, repo_root):
    if not repo_root:
        return None
    root = Path(repo_root)
    if file_path.startswith(ROOT_MODULE + "/"):
        return root / file_path[len(ROOT_MODULE) + 1:]
    if file_path == ROOT_MODULE or file_path.endswith("/caesium.go"):
        return root / "caesium.go"
    suffix = file_path.split(ROOT_MODULE + "/", 1)[-1] if ROOT_MODULE in file_path else file_path
    candidate = root / suffix
    return candidate if candidate.is_file() else None


def load_go_funcs(file_path, repo_root):
    key = (str(repo_root), file_path)
    if key in _FUNC_INDEX:
        return _FUNC_INDEX[key]
    path = _repo_file(file_path, repo_root)
    if path is None or not path.is_file():
        _FUNC_INDEX[key] = []
        return []
    funcs = parse_go_funcs(path.read_text())
    _FUNC_INDEX[key] = funcs
    return funcs


def _file_matches(profile_file, wanted):
    if profile_file == wanted:
        return True
    if profile_file.endswith("/" + wanted) or profile_file.endswith(wanted):
        return True
    if wanted.startswith(ROOT_MODULE + "/") and profile_file.endswith(wanted[len(ROOT_MODULE) + 1:]):
        return True
    return False


def _block_in_ranges(start_line, ranges):
    return any(lo <= start_line <= hi for lo, hi in ranges)


def init_ranges(file_path, repo_root):
    return [(fn["start"], fn["end"]) for fn in load_go_funcs(file_path, repo_root) if fn["name"] == "init"]


def function_is_covered(blocks, file_path, func_names, *, repo_root):
    """True when a non-init block whose start line sits in one of func_names ran."""
    if not blocks:
        return False
    funcs = [fn for fn in load_go_funcs(file_path, repo_root) if fn["name"] in func_names]
    if not funcs:
        return False
    inits = init_ranges(file_path, repo_root)
    wanted = [(fn["start"], fn["end"]) for fn in funcs]
    for (profile_file, start_line, _sc, _el, _ec), (_stmts, count) in blocks.items():
        if count <= 0 or not _file_matches(profile_file, file_path):
            continue
        if _block_in_ranges(start_line, inits):
            continue
        if _block_in_ranges(start_line, wanted):
            return True
    return False


def file_has_non_init_coverage(blocks, file_path, *, repo_root):
    if not blocks:
        return False
    inits = init_ranges(file_path, repo_root)
    for (profile_file, start_line, _sc, _el, _ec), (_stmts, count) in blocks.items():
        if count <= 0 or not _file_matches(profile_file, file_path):
            continue
        if inits and _block_in_ranges(start_line, inits):
            continue
        return True
    return False


def package_of(file_path):
    if not file_path.endswith(".go"):
        return file_path.rsplit("/", 1)[0] if "/" in file_path else file_path
    return file_path.rsplit("/", 1)[0]


def normalize_changed_path(path, module=ROOT_MODULE):
    path = path.strip().replace("\\", "/")
    if not path or path.endswith("_test.go"):
        return ""
    if not path.endswith(".go"):
        return ""
    if path.startswith(module + "/"):
        return path
    if path.startswith("./"):
        path = path[2:]
    if path.startswith("reagents/"):
        return REAGENTS_MODULE + "/" + path[len("reagents/"):]
    return module + "/" + path.lstrip("/")


def parse_profile_text(text, *, origin="profile"):
    """Parse a go coverprofile (textfmt). Returns (mode, blocks) or raises ValueError."""
    if text is None:
        raise ValueError(f"{origin} is missing")
    if isinstance(text, bytes):
        try:
            text = text.decode("utf-8")
        except UnicodeDecodeError as err:
            raise ValueError(f"{origin} is not UTF-8: {err}") from err
    if text == "":
        raise ValueError(f"{origin} is empty")
    lines = text.splitlines()
    if not lines or not lines[0].startswith("mode:"):
        raise ValueError(f"{origin} is not a go coverprofile (missing mode: line)")
    mode = lines[0].split(":", 1)[1].strip()
    if mode not in {"set", "count", "atomic"}:
        raise ValueError(f"{origin} has unknown cover mode {mode!r}")
    blocks = {}
    for lineno, line in enumerate(lines[1:], start=2):
        if not line.strip():
            continue
        match = PROFILE_LINE.match(line)
        if not match:
            raise ValueError(f"{origin}:{lineno} is not a coverprofile block")
        file_path = match.group("file")
        key = (
            file_path,
            int(match.group("sl")),
            int(match.group("sc")),
            int(match.group("el")),
            int(match.group("ec")),
        )
        stmts = int(match.group("stmts"))
        count = int(match.group("count"))
        if key in blocks:
            prev_stmts, prev_count = blocks[key]
            if prev_stmts != stmts:
                raise ValueError(f"{origin}:{lineno} restates {key[0]} with different stmt count")
            if mode == "set":
                count = max(prev_count, count)
            else:
                count += prev_count
        blocks[key] = (stmts, count)
    return mode, blocks


def parse_profile_file(path):
    path = Path(path)
    try:
        text = path.read_text()
    except OSError as err:
        raise ValueError(f"cannot read {path}: {err}") from err
    return parse_profile_text(text, origin=str(path))


def merge_blocks(mode, first, second, *, origin="merge"):
    if not first:
        return dict(second)
    if not second:
        return dict(first)
    out = dict(first)
    for key, (stmts, count) in second.items():
        if key not in out:
            out[key] = (stmts, count)
            continue
        prev_stmts, prev_count = out[key]
        if prev_stmts != stmts:
            raise ValueError(f"{origin}: incompatible statement counts for {key[0]}")
        if mode == "set":
            out[key] = (stmts, max(prev_count, count))
        else:
            out[key] = (stmts, prev_count + count)
    return out


def summarize_blocks(blocks):
    files = {}
    packages = {}
    covered = 0
    total = 0
    for (file_path, _sl, _sc, _el, _ec), (stmts, count) in blocks.items():
        hit = stmts if count > 0 else 0
        covered += hit
        total += stmts
        f = files.setdefault(file_path, {"covered": 0, "total": 0})
        f["covered"] += hit
        f["total"] += stmts
        pkg = package_of(file_path)
        p = packages.setdefault(pkg, {"covered": 0, "total": 0})
        p["covered"] += hit
        p["total"] += stmts
    for bucket in (files, packages):
        for stats in bucket.values():
            stats["percent"] = _percent(stats["covered"], stats["total"])
    return {
        "covered": covered,
        "total": total,
        "percent": _percent(covered, total),
        "files": files,
        "packages": packages,
    }


def _percent(covered, total):
    if total <= 0:
        return None
    return round(100.0 * covered / total, 1)


def file_is_covered(summary, file_path):
    files = summary.get("files") or {}
    stats = files.get(file_path)
    if stats and stats["covered"] > 0:
        return True
    suffix = file_path
    if file_path.startswith(ROOT_MODULE + "/"):
        suffix = file_path[len(ROOT_MODULE) + 1:]
    for name, stats in files.items():
        if stats["covered"] <= 0:
            continue
        if name == suffix or name.endswith("/" + suffix) or name.endswith(file_path):
            return True
    return False


def any_file_covered(summary, files):
    return any(file_is_covered(summary, path) for path in files)


def validate_provenance(prov, source, issues, *, candidate_sha=None):
    if not isinstance(prov, dict):
        issues.append(_fail("schema", f"{source} provenance must be an object", source))
        return None
    src = prov.get("source", source)
    if src != source:
        issues.append(_fail("foreign", f"{source} provenance.source is {src!r}", source))
    module = prov.get("module") or ROOT_MODULE
    if source == "reagents" and module != REAGENTS_MODULE:
        issues.append(_fail("foreign", f"reagents profile module is {module!r}", source))
    elif source != "reagents" and module == REAGENTS_MODULE:
        issues.append(_fail(
            "contamination",
            f"{source} profile is labelled with the reagents module",
            source,
        ))
    elif source != "reagents" and module != ROOT_MODULE:
        issues.append(_fail("foreign", f"{source} profile module is {module!r}", source))
    kind = str(prov.get("kind") or "")
    label = str(prov.get("label") or prov.get("lane") or "")
    for marker in PERFORMANCE_LABELS:
        if marker in kind.lower() or marker in label.lower() or marker in str(src).lower():
            issues.append(_fail(
                "performance",
                f"{source} profile is labelled as performance/load evidence",
                source,
            ))
            break
    sha = prov.get("candidate_sha")
    if sha and not _is_sha(sha):
        issues.append(_fail("schema", f"{source} candidate_sha is not a commit SHA", source))
    elif sha and candidate_sha and sha != candidate_sha:
        issues.append(_fail(
            "foreign",
            f"{source} candidate_sha {sha} does not match {candidate_sha}",
            source,
        ))
    if prov.get("verified") is False or prov.get("image_provenance") == "supplied/unverified":
        issues.append(_incomplete(
            "unverified",
            f"{source} image is supplied/unverified and is not a provenanced match of the candidate",
            source,
        ))
    return prov


def load_contribution(source, path, provenance_path, issues, *, candidate_sha=None):
    """Load one labelled contribution.

    Missing files or killed provenance yield status=incomplete with percent=None.
    They never synthesise a 0% summary.
    """
    contrib = {
        "source": source,
        "status": "absent",
        "percent": None,
        "covered": None,
        "total": None,
        "mode": None,
        "blocks": None,
        "summary": None,
        "provenance": None,
        "path": str(path) if path else None,
    }
    prov = None
    if provenance_path and Path(provenance_path).is_file():
        try:
            prov = load_json(provenance_path)
        except ValueError as err:
            issues.append(_fail("schema", str(err), source))
            contrib["status"] = "fail"
            return contrib
    elif path and Path(path).is_file():
        # A profile with no provenance cannot be bound to a candidate commit.
        contrib["status"] = "incomplete"
        if source in INTEGRATION_PARTS or source == "integration":
            issues.append(_incomplete(
                "missing",
                f"{source} provenance is missing; that is incomplete, not a default complete",
                source,
            ))
        return contrib
    if prov is not None:
        validate_provenance(prov, source, issues, candidate_sha=candidate_sha)
        contrib["provenance"] = prov

    if prov and (prov.get("missing") or not prov.get("complete", True) or prov.get("killed")):
        contrib["status"] = "incomplete"
        # Optional sources (unit/browser/reagents) stay incomplete in the report
        # without failing the CLI/server collect. Integration parts are required.
        if source in INTEGRATION_PARTS:
            if prov.get("killed") or str(prov.get("signal") or "").upper() in {"SIGKILL", "KILL", "9"}:
                issues.append(_incomplete(
                    "killed",
                    f"{source} process was killed; coverage is incomplete, not zero",
                    source,
                ))
            elif prov.get("missing"):
                issues.append(_incomplete(
                    "missing",
                    f"{source} GOCOVERDIR/profile is missing; that is incomplete, not 0%",
                    source,
                ))
            else:
                issues.append(_incomplete(
                    "incomplete",
                    f"{source} provenance marks the profile incomplete",
                    source,
                ))
        return contrib

    if not path or not Path(path).is_file():
        if source in INTEGRATION_PARTS:
            issues.append(_incomplete(
                "missing",
                f"{source} profile is missing; that is incomplete, not 0%",
                source,
            ))
            contrib["status"] = "incomplete"
        else:
            contrib["status"] = "absent"
        return contrib

    try:
        mode, blocks = parse_profile_file(path)
    except ValueError as err:
        issues.append(_fail("schema", str(err), source))
        contrib["status"] = "fail"
        return contrib

    for file_path in {key[0] for key in blocks}:
        for marker in FORBIDDEN_PACKAGE_MARKERS:
            if marker in file_path:
                issues.append(_fail(
                    "performance",
                    f"{source} profile contains forbidden package path {file_path}",
                    source,
                ))
        if source != "reagents" and file_path.startswith(REAGENTS_MODULE):
            issues.append(_fail(
                "contamination",
                f"{source} profile contains reagents paths",
                source,
            ))
        if source == "reagents" and file_path.startswith(ROOT_MODULE + "/") and not file_path.startswith(REAGENTS_MODULE):
            issues.append(_fail(
                "contamination",
                f"reagents profile contains root-module paths",
                source,
            ))

    summary = summarize_blocks(blocks)
    contrib.update({
        "status": "complete",
        "percent": summary["percent"],
        "covered": summary["covered"],
        "total": summary["total"],
        "mode": mode,
        "blocks": blocks,
        "summary": summary,
    })
    if candidate_sha:
        sha = (prov or {}).get("candidate_sha")
        if not sha:
            issues.append(_fail(
                "foreign",
                f"{source} complete profile has no candidate_sha; cannot bind to {candidate_sha}",
                source,
            ))
        elif sha != candidate_sha:
            pass  # already reported in validate_provenance
        if (prov or {}).get("verified") is False or (prov or {}).get("image_provenance") == "supplied/unverified":
            contrib["status"] = "incomplete"
    return contrib


def merge_contributions(parts, issues, *, name):
    present = [part for part in parts if part.get("blocks") is not None]
    if not present:
        return {
            "source": name,
            "status": "incomplete",
            "percent": None,
            "covered": None,
            "total": None,
            "mode": None,
            "blocks": None,
            "summary": None,
        }
    modes = {part["mode"] for part in present}
    if len(modes) > 1:
        issues.append(_fail(
            "incompatible",
            f"{name} cannot merge cover modes {sorted(modes)}",
            name,
        ))
        return {
            "source": name,
            "status": "fail",
            "percent": None,
            "covered": None,
            "total": None,
            "mode": None,
            "blocks": None,
            "summary": None,
        }
    mode = present[0]["mode"]
    blocks = {}
    try:
        for part in present:
            blocks = merge_blocks(mode, blocks, part["blocks"], origin=name)
    except ValueError as err:
        issues.append(_fail("incompatible", str(err), name))
        return {
            "source": name,
            "status": "fail",
            "percent": None,
            "covered": None,
            "total": None,
            "mode": None,
            "blocks": None,
            "summary": None,
        }
    summary = summarize_blocks(blocks)
    statuses = {part["status"] for part in parts}
    if any(part["status"] != "complete" for part in parts):
        status = "incomplete"
        if "fail" in statuses:
            status = "fail"
        # Incomplete merge still reports the observed union, but percent on the
        # contribution itself stays null so a partial dump cannot be read as a
        # measured floor.
        return {
            "source": name,
            "status": status,
            "percent": None,
            "covered": None,
            "total": None,
            "mode": mode,
            "blocks": blocks,
            "summary": summary,
            "observed_percent": summary["percent"],
        }
    return {
        "source": name,
        "status": "complete",
        "percent": summary["percent"],
        "covered": summary["covered"],
        "total": summary["total"],
        "mode": mode,
        "blocks": blocks,
        "summary": summary,
    }


def write_to_read_result(integration, cli, server, *, repo_root):
    result = {
        "id": WRITE_TO_READ["id"],
        "description": WRITE_TO_READ["description"],
        "covered": False,
        "status": "incomplete",
        "write": [],
        "read": [],
        "evidence": [],
    }
    if (
        integration.get("status") != "complete"
        or cli.get("status") != "complete"
        or server.get("status") != "complete"
        or not server.get("blocks")
        or not cli.get("blocks")
    ):
        result["status"] = "incomplete"
        return result
    if not repo_root:
        result["status"] = "incomplete"
        result["reason"] = "repo-root is required to prove function coverage (init() is not a write or a read)"
        return result
    sources = {"cli": cli, "server": server, "integration": integration}
    evidence = []
    for spec in WRITE_TO_READ_EVIDENCE:
        contrib = sources.get(spec["source"]) or {}
        covered = function_is_covered(
            contrib.get("blocks"),
            spec["file"],
            spec["funcs"],
            repo_root=repo_root,
        )
        evidence.append({
            "id": spec["id"],
            "source": spec["source"],
            "file": spec["file"],
            "funcs": list(spec["funcs"]),
            "side": spec["side"],
            "covered": covered,
        })
    result["evidence"] = evidence
    result["write"] = [item for item in evidence if item["side"] == "write"]
    result["read"] = [item for item in evidence if item["side"] == "read"]
    if all(item["covered"] for item in evidence):
        result["covered"] = True
        result["status"] = "pass"
    else:
        result["status"] = "fail"
    return result


def contract_gaps(integration, *, repo_root):
    gaps = []
    if integration.get("status") != "complete" or not integration.get("blocks"):
        return [
            {
                "id": cid,
                "status": "incomplete",
                "paths": list(paths),
            }
            for cid, paths in CONTRACT_PATHS.items()
        ]
    blocks = integration["blocks"]
    for cid, paths in CONTRACT_PATHS.items():
        full = [p if p.startswith(ROOT_MODULE) else f"{ROOT_MODULE}/{p}" for p in paths]
        covered_files = [
            path for path in full
            if file_has_non_init_coverage(blocks, path, repo_root=repo_root)
        ]
        covered = bool(full) and len(covered_files) == len(full)
        gaps.append({
            "id": cid,
            "status": "covered" if covered else "gap",
            "paths": full,
            "covered_paths": covered_files,
        })
    return gaps


def uncovered_changed_paths(integration, changed_paths):
    out = []
    reagents_changed = []
    if not changed_paths:
        return out, reagents_changed
    summary = (integration or {}).get("summary") or {"files": {}}
    files = summary.get("files") or {}
    for raw in changed_paths:
        normalized = normalize_changed_path(raw)
        if not normalized:
            continue
        if normalized.startswith(REAGENTS_MODULE):
            reagents_changed.append(normalized)
            continue
        stats = files.get(normalized)
        if stats is None or stats["covered"] == 0:
            out.append(normalized)
    return out, reagents_changed


def audit_reagents(repo_root, issues):
    audit = {
        "module": REAGENTS_MODULE,
        "audited": False,
        "go_mod": None,
        "contaminated_root": False,
    }
    if not repo_root:
        issues.append(_incomplete(
            "reagents",
            "reagents/go.mod was not audited (--repo-root missing)",
            "reagents",
        ))
        return audit
    go_mod = Path(repo_root) / "reagents" / "go.mod"
    root_mod = Path(repo_root) / "go.mod"
    if not go_mod.is_file():
        issues.append(_fail("reagents", "reagents/go.mod is missing", "reagents"))
        return audit
    text = go_mod.read_text()
    audit["go_mod"] = str(go_mod)
    audit["audited"] = True
    first = next(
        (
            line.strip()
            for line in text.splitlines()
            if line.strip() and not line.strip().startswith("//")
        ),
        "",
    )
    if first != f"module {REAGENTS_MODULE}":
        issues.append(_fail(
            "reagents",
            f"reagents/go.mod module line is {first!r}",
            "reagents",
        ))
    if root_mod.is_file():
        root_text = root_mod.read_text()
        if REAGENTS_MODULE in root_text:
            audit["contaminated_root"] = True
            issues.append(_fail(
                "contamination",
                "root go.mod references the reagents module",
                "reagents",
            ))
    return audit


def apply_ratchet(integration, ratchet, issues, *, uncovered_diff):
    applied = {"applied": False, "packages": {}, "diff": None}
    if not ratchet:
        return applied
    if not isinstance(ratchet, dict):
        issues.append(_fail("schema", "ratchet must be an object"))
        return applied
    if ratchet.get("min_global_percent") is not None or ratchet.get("global_percent") is not None:
        issues.append(_fail(
            "ratchet",
            "global percentage floors are not used; package/diff ratchets only",
        ))
    applied["applied"] = True
    if integration.get("status") != "complete" or not integration.get("summary"):
        issues.append(_incomplete(
            "ratchet",
            "cannot enforce package ratchets against incomplete integration coverage",
        ))
        return applied
    packages = integration["summary"]["packages"]
    floors = ratchet.get("packages") or {}
    for name, floor in floors.items():
        if not isinstance(floor, dict):
            issues.append(_fail("schema", f"ratchet.packages[{name}] must be an object", name))
            continue
        min_percent = floor.get("min_percent")
        min_covered = floor.get("min_statements_covered")
        stats = packages.get(name)
        observed = {
            "percent": None if stats is None else stats["percent"],
            "covered": None if stats is None else stats["covered"],
        }
        applied["packages"][name] = observed
        if stats is None:
            issues.append(_fail(
                "ratchet",
                f"package {name} is in the baseline but absent from integration coverage",
                name,
            ))
            continue
        if min_percent is not None and (stats["percent"] is None or stats["percent"] + 1e-9 < float(min_percent)):
            issues.append(_fail(
                "ratchet",
                f"package {name} integration coverage {stats['percent']} < floor {min_percent}",
                name,
            ))
        if min_covered is not None and stats["covered"] < int(min_covered):
            issues.append(_fail(
                "ratchet",
                f"package {name} covered statements {stats['covered']} < floor {min_covered}",
                name,
            ))
    diff_floor = ratchet.get("diff") or {}
    if diff_floor:
        applied["diff"] = {"uncovered_changed_paths": list(uncovered_diff)}
        max_uncovered = diff_floor.get("uncovered_changed_paths_max")
        min_percent = diff_floor.get("min_percent")
        if max_uncovered is not None and len(uncovered_diff) > int(max_uncovered):
            issues.append(_fail(
                "ratchet",
                f"{len(uncovered_diff)} uncovered changed paths exceeds floor {max_uncovered}",
                "diff",
            ))
        if min_percent is not None:
            issues.append(_fail(
                "schema",
                "ratchet.diff.min_percent is not a supported floor; use uncovered_changed_paths_max",
                "diff",
            ))
    return applied


def baseline_from_integration(integration, *, uncovered_diff=None):
    if integration.get("status") != "complete" or not integration.get("summary"):
        raise ValueError("cannot write a baseline from incomplete integration coverage")
    packages = {}
    for name, stats in sorted(integration["summary"]["packages"].items()):
        if not stats["covered"]:
            continue
        packages[name] = {
            "min_percent": stats["percent"],
            "min_statements_covered": stats["covered"],
        }
    baseline = {
        "schema_version": 1,
        "kind": "package-diff-ratchet",
        "source": "integration",
        "packages": packages,
    }
    if uncovered_diff is not None:
        baseline["diff"] = {
            "uncovered_changed_paths_max": len(uncovered_diff),
        }
    return baseline


def contribution_public(contrib):
    out = {
        "source": contrib.get("source"),
        "status": contrib.get("status"),
        "percent": contrib.get("percent"),
        "covered": contrib.get("covered"),
        "total": contrib.get("total"),
        "mode": contrib.get("mode"),
    }
    if contrib.get("provenance"):
        pub = {
            key: contrib["provenance"].get(key)
            for key in (
                "source", "kind", "module", "candidate_sha", "image_id",
                "collection", "flush", "killed", "missing", "complete",
                "exit_code", "signal",
            )
            if key in contrib["provenance"]
        }
        out["provenance"] = pub
    if contrib.get("status") == "complete" and contrib.get("summary"):
        out["package_count"] = len(contrib["summary"]["packages"])
    return out


def evaluate(contributions, *, candidate_sha=None, repo_root=None,
             changed_paths=None, ratchet=None, require_browser=False,
             require_reagents=False, require_unit=False):
    issues = []
    if candidate_sha and not _is_sha(candidate_sha):
        issues.append(_fail("schema", "candidate_sha is not a commit SHA"))

    reagents_audit = audit_reagents(repo_root, issues)

    unit = contributions.get("unit") or _absent("unit")
    cli = contributions.get("cli") or _absent("cli")
    server = contributions.get("server") or _absent("server")
    supplied_integration = contributions.get("integration")
    browser = contributions.get("browser") or _absent("browser")
    reagents = contributions.get("reagents") or _absent("reagents")

    if supplied_integration and supplied_integration.get("status") == "complete":
        integration = supplied_integration
        if cli.get("status") == "absent" and server.get("status") == "absent":
            # Pre-merged integration is acceptable only with provenance naming
            # both CLI and server. Otherwise it cannot stand in for real surfaces.
            prov = supplied_integration.get("provenance") or {}
            named = prov.get("sources") or prov.get("parts") or []
            if not (isinstance(named, list) and set(named) >= set(INTEGRATION_PARTS)):
                issues.append(_fail(
                    "schema",
                    "pre-merged integration profile must name sources [cli, server] in provenance",
                    "integration",
                ))
    else:
        integration = merge_contributions([cli, server], issues, name="integration")

    merged_with_browser = integration
    if browser.get("status") == "complete":
        merged_with_browser = merge_contributions(
            [
                {**integration, "status": integration.get("status") or "incomplete"},
                browser,
            ],
            issues,
            name="integration+browser",
        )
    elif browser.get("status") == "absent":
        # Missing browser evidence is an incomplete contribution, not 0%. It
        # does not fail the CLI/server collect unless --require-browser.
        browser["status"] = "incomplete"

    wtr = write_to_read_result(integration, cli, server, repo_root=repo_root)
    if wtr["status"] == "fail":
        issues.append(_fail(
            "write-to-read",
            "CLI/server profiles do not cover the job apply→export request-to-write-to-read functions (init() does not count; server write/read must appear in server.out)",
            WRITE_TO_READ["id"],
        ))
    elif wtr["status"] == "incomplete":
        issues.append(_incomplete(
            "write-to-read",
            "write-to-read path cannot be demonstrated without complete CLI and server profiles",
            WRITE_TO_READ["id"],
        ))

    gaps = contract_gaps(integration, repo_root=repo_root)
    uncovered, reagents_changed = uncovered_changed_paths(integration, changed_paths or [])
    if reagents_changed and reagents.get("status") != "complete":
        issues.append(_incomplete(
            "reagents",
            "reagents paths changed but no reagents coverage profile was collected",
            "reagents",
        ))
        if reagents.get("status") == "absent":
            reagents["status"] = "incomplete"

    if require_browser and browser.get("status") != "complete":
        issues.append(_incomplete(
            "browser",
            "browser contribution was required but is not complete",
            "browser",
        ))
    if require_unit and unit.get("status") != "complete":
        issues.append(_incomplete(
            "unit",
            "unit contribution was required but is not complete",
            "unit",
        ))
    if require_reagents and reagents.get("status") != "complete":
        issues.append(_incomplete(
            "reagents",
            "reagents contribution was required but is not complete",
            "reagents",
        ))
    if unit.get("status") == "absent":
        unit["status"] = "incomplete"

    ratchet_result = apply_ratchet(integration, ratchet, issues, uncovered_diff=uncovered)

    report = {
        "schema_version": 1,
        "candidate_sha": candidate_sha,
        "contributions": {
            "unit": contribution_public(unit),
            "cli": contribution_public(cli),
            "server": contribution_public(server),
            "integration": contribution_public(integration),
            "browser": contribution_public(browser),
            "reagents": contribution_public(reagents),
        },
        "write_to_read": wtr,
        "contract_gaps": gaps,
        "uncovered_changed_paths": uncovered,
        "reagents_audit": reagents_audit,
        "ratchet": {
            "applied": ratchet_result["applied"],
            "packages": ratchet_result.get("packages") or {},
            "diff": ratchet_result.get("diff"),
        },
        "performance_instrumentation": False,
    }
    if any(item.code == "performance" for item in issues):
        report["performance_instrumentation"] = True
    report["issues"] = [item.as_dict() for item in issues]
    report["verdict"] = verdict_of(issues)
    return report, issues


def _absent(source):
    return {
        "source": source,
        "status": "absent",
        "percent": None,
        "covered": None,
        "total": None,
        "mode": None,
        "blocks": None,
        "summary": None,
        "provenance": None,
    }


def verdict_of(issues):
    if any(item.verdict == "fail" for item in issues):
        return "fail"
    if any(item.verdict == "incomplete" for item in issues):
        return "incomplete"
    return "pass"


def exit_code(issues, *, strict=False):
    if strict:
        issues = [
            Issue("fail", item.code, item.message, item.target)
            if item.verdict == "incomplete" else item
            for item in issues
        ]
    if any(item.verdict == "fail" for item in issues):
        return 1
    if any(item.verdict == "incomplete" for item in issues):
        return 2
    return 0


def discover_profiles(profiles_dir):
    profiles_dir = Path(profiles_dir)
    found = {}
    for source in SOURCES:
        profile = profiles_dir / f"{source}.out"
        provenance = profiles_dir / f"{source}.provenance.json"
        if profile.is_file() or provenance.is_file():
            found[source] = {
                "profile": profile if profile.is_file() else None,
                "provenance": provenance if provenance.is_file() else None,
            }
    return found


def load_changed_paths(path):
    if not path:
        return []
    text = Path(path).read_text()
    return [line.strip() for line in text.splitlines() if line.strip() and not line.startswith("#")]


def parse_profile_flag(value):
    if "=" not in value:
        raise argparse.ArgumentTypeError("expected source=path")
    source, path = value.split("=", 1)
    source = source.strip()
    if source not in SOURCES:
        raise argparse.ArgumentTypeError(f"unknown source {source!r}")
    return source, path


def parse_args(argv=None):
    parser = argparse.ArgumentParser(
        prog="check-coverage.py",
        description="Validate labelled CLI/server/browser coverage with provenance.",
    )
    parser.add_argument("--profiles-dir", help="Directory of <source>.out and <source>.provenance.json")
    parser.add_argument(
        "--profile",
        action="append",
        default=[],
        metavar="SOURCE=PATH",
        help="Labelled coverprofile (unit, cli, server, integration, browser, reagents)",
    )
    parser.add_argument(
        "--provenance",
        action="append",
        default=[],
        metavar="SOURCE=PATH",
        help="Labelled provenance JSON for a source",
    )
    parser.add_argument("--candidate-sha", default=None)
    parser.add_argument("--repo-root", default=None, help="Repository root (reagents/go.mod audit)")
    parser.add_argument("--changed-paths", default=None, help="File listing changed paths, one per line")
    parser.add_argument("--ratchet", default=None, help="Measured package/diff floors JSON")
    parser.add_argument(
        "--write-baseline",
        default=None,
        help="Write package/diff floors from this complete integration profile",
    )
    parser.add_argument("--report", default=None, help="Write the coverage report JSON")
    parser.add_argument("--require-browser", action="store_true")
    parser.add_argument("--require-unit", action="store_true")
    parser.add_argument("--require-reagents", action="store_true")
    parser.add_argument(
        "--strict",
        action="store_true",
        help="Treat incomplete contributions as failures (exit 1)",
    )
    return parser.parse_args(argv)


def collect_contributions(args, issues_out):
    mapping = {source: {"profile": None, "provenance": None} for source in SOURCES}
    if args.profiles_dir:
        for source, found in discover_profiles(args.profiles_dir).items():
            mapping[source] = found
    for raw in args.profile:
        source, path = parse_profile_flag(raw)
        mapping[source]["profile"] = Path(path)
    for raw in args.provenance:
        source, path = parse_profile_flag(raw)
        mapping[source]["provenance"] = Path(path)

    contributions = {}
    for source, paths in mapping.items():
        if paths["profile"] is None and paths["provenance"] is None:
            continue
        contributions[source] = load_contribution(
            source,
            paths["profile"],
            paths["provenance"],
            issues_out,
            candidate_sha=args.candidate_sha,
        )
    return contributions


def main(argv=None):
    args = parse_args(argv)
    preload_issues = []
    try:
        contributions = collect_contributions(args, preload_issues)
    except argparse.ArgumentTypeError as err:
        print(f"coverage: schema: {err}", file=sys.stderr)
        return 1
    ratchet = None
    if args.ratchet:
        try:
            ratchet = load_json(args.ratchet)
        except ValueError as err:
            print(f"coverage: schema: {err}", file=sys.stderr)
            return 1
    try:
        changed = load_changed_paths(args.changed_paths) if args.changed_paths else []
    except OSError as err:
        print(f"coverage: schema: {err}", file=sys.stderr)
        return 1

    report, issues = evaluate(
        contributions,
        candidate_sha=args.candidate_sha,
        repo_root=args.repo_root,
        changed_paths=changed,
        ratchet=ratchet,
        require_browser=args.require_browser,
        require_reagents=args.require_reagents,
        require_unit=args.require_unit,
    )
    issues = preload_issues + issues
    report["issues"] = [item.as_dict() for item in issues]
    report["verdict"] = verdict_of(issues)
    if any(item.code == "performance" for item in issues):
        report["performance_instrumentation"] = True

    if args.write_baseline and report["verdict"] == "pass":
        integration = None
        integration_contrib = contributions.get("integration")
        if integration_contrib and integration_contrib.get("status") == "complete":
            integration = integration_contrib
        else:
            rebuild_issues = []
            cli = contributions.get("cli") or _absent("cli")
            server = contributions.get("server") or _absent("server")
            integration = merge_contributions([cli, server], rebuild_issues, name="integration")
        try:
            baseline = baseline_from_integration(
                integration,
                uncovered_diff=report.get("uncovered_changed_paths"),
            )
        except ValueError:
            baseline = None
        if baseline is not None:
            Path(args.write_baseline).write_text(json.dumps(baseline, indent=2) + "\n")

    if args.report:
        Path(args.report).write_text(json.dumps(report, indent=2) + "\n")

    lines = [f"  {item.label()}: {item.message}" for item in issues]
    if lines:
        print("coverage issues:")
        print("\n".join(lines))
    code = exit_code(issues, strict=args.strict)
    failed = [item.label() for item in issues if item.verdict == "fail"]
    incomplete = [item.label() for item in issues if item.verdict == "incomplete"]
    if failed:
        print("coverage failed: " + ", ".join(failed), file=sys.stderr)
    elif incomplete:
        print("coverage incomplete: " + ", ".join(incomplete), file=sys.stderr)
    else:
        print("coverage passed")
    wtr = report.get("write_to_read") or {}
    integ = report.get("contributions", {}).get("integration") or {}
    if integ.get("percent") is not None:
        print(f"coverage integration percent={integ['percent']}")
    else:
        print("coverage integration percent=null (incomplete; not 0)")
    print(f"coverage write-to-read {wtr.get('id')} status={wtr.get('status')} covered={wtr.get('covered')}")
    return code


if __name__ == "__main__":
    raise SystemExit(main())
