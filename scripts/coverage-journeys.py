#!/usr/bin/env python3
"""Collect the persistent OIDC/SAML restart journey for integration coverage.

The caller has already built and pinned the coverage and builder images. This
driver compiles the test-only IdP fixture in the builder container, then runs
the fixture and candidate server on a private Docker network. Only the
candidate server is instrumented. The journey process owns protocol state in
memory and communicates the SAML replay-store restart barrier over stdin/stdout.
"""

from __future__ import annotations

import argparse
from collections import deque
import datetime
import hashlib
import importlib.util
import json
import os
import pathlib
import re
import secrets
import select
import signal
import shutil
import stat
import subprocess
import sys
import tempfile
import time
import traceback
from typing import Any
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen
from urllib.parse import parse_qsl, urlsplit


MODULE = "github.com/caesium-cloud/caesium"
LABEL_OWNER = "caesium.coverage.owner"
LABEL_RUN = "caesium.coverage.run"
LABEL_LANE = "caesium.coverage.lane"
KEY_RE = re.compile(r"csk_[A-Za-z0-9_-]+")
DIGEST_RE = re.compile(r"[0-9a-f]{64}")
UUID_RE = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")
MAX_PROTOCOL_LINE = 64 * 1024
COUNT_KEYS = ("oidc_users", "saml_users", "oidc_sessions", "saml_sessions", "assertions")
EXPECTED_CHECKS = (
    "readiness",
    "oidc-success",
    "oidc-code-replay",
    "oidc-state-mismatch",
    "oidc-missing-cookie",
    "oidc-tampered-cookie",
    "oidc-provider-error",
    "oidc-bad_nonce",
    "oidc-bad_audience",
    "oidc-fresh-positive",
    "saml-signed-success",
    "saml-immediate-replay",
    "saml-restart-barrier",
    "saml-persistent-replay",
    "saml-fresh-positive",
    "saml-tampered",
    "saml-bad_audience",
    "saml-expired",
    "saml-relay-mismatch",
    "saml-tampered-cookie",
    "saml-missing-cookie",
    "saml-missing-response",
    *(f"{provider}-return-target-{index}" for provider in ("oidc", "saml") for index in range(5)),
)
EXPECTED_COUNTS = {
    "oidc_users": 1,
    "saml_users": 1,
    "oidc_sessions": 7,
    "saml_sessions": 7,
    "assertions": 7,
}
EXPECTED_REDIRECTS = {
    "/runs?status=mine#coverage",
    "/runs?status=failed#coverage",
    "/",
    "/runs?status=mine#absolute",
    "/jobs%20escaped?x=%2F#frag%20ment",
}


class JourneyError(RuntimeError):
    pass


def safe_exception_metadata(error: Exception) -> dict[str, Any]:
    """Keep exception class and frame coordinates; omit messages, source, and locals."""
    frames = traceback.extract_tb(error.__traceback__)[-12:]
    return {
        "exception_type": type(error).__module__ + "." + type(error).__qualname__,
        "traceback": [{"file": pathlib.Path(frame.filename).name, "function": frame.name,
                       "line": frame.lineno} for frame in frames],
    }


def unexpected_exception_record(label: str, error: Exception) -> str:
    return json.dumps({"error": label, **safe_exception_metadata(error)}, sort_keys=True)


class Interrupted(JourneyError):
    pass


def redact(text: str) -> str:
    return KEY_RE.sub("[REDACTED_API_KEY]", text)


def missing_object(result: subprocess.CompletedProcess[str], kind: str, name: str) -> bool:
    if result.returncode == 0 or result.stdout.strip() not in ("", "[]"):
        return False
    escaped = re.escape(name)
    patterns = {
        "network": [rf'Error response from daemon: network {escaped} not found'],
        "container": [rf'Error response from daemon: No such container: {escaped}', rf'Error: No such container: {escaped}'],
    }
    return any(re.fullmatch(pattern, result.stderr.strip()) for pattern in patterns.get(kind, []))


def raw_files(directory: pathlib.Path) -> dict[str, str]:
    if not directory.is_dir() or directory.is_symlink():
        raise JourneyError("original raw coverage directory is unavailable")
    files = {}
    for path in sorted(directory.iterdir()):
        if not path.is_file() or path.is_symlink() or path.stat().st_size <= 0 or not path.name.startswith(("covmeta.", "covcounters.")):
            raise JourneyError("original raw coverage contains empty or foreign files")
        files[path.name] = hashlib.sha256(path.read_bytes()).hexdigest()
    if not any(name.startswith("covmeta.") for name in files) or not any(name.startswith("covcounters.") for name in files):
        raise JourneyError("original raw coverage pair is incomplete")
    return files


def complete_backend_gate(result: Any) -> bool:
    return (isinstance(result, dict)
            and result.get("complete") is True
            and result.get("selected_complete") is True
            and result.get("gate_mode") is True
            and result.get("coverage_contribution") is True)


def invalidate_collection(profiles: pathlib.Path, raw: pathlib.Path, artifacts: pathlib.Path) -> None:
    """Keep failed cleanup from leaving reusable eligible cohort records."""
    paths = [profiles / (source + ".provenance.json") for source in ("cli", "server", "integration", "browser")]
    paths += [raw / "journeys" / "manifest.json", artifacts / "backend-inputs.json"]
    for path in paths:
        if path.is_symlink():
            raise JourneyError("refusing symlinked collection provenance")
        if not path.exists():
            continue
        value = json.loads(path.read_bytes())
        if not isinstance(value, dict):
            raise JourneyError("collection provenance is not an object")
        value.update(complete=False, cleanup_complete=False,
                     cleanup_error="owned cleanup incomplete; operator reconciliation required")
        temporary = path.with_name(path.name + ".cleanup-incomplete")
        descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, "w") as stream:
            stream.write(json.dumps(value, indent=2) + "\n")
        os.replace(temporary, path)


def complete_response_bytes(response, limit: int) -> bytes:
    try:
        body = response.read(limit + 1)
        if len(body) > limit or response.read(1):
            raise JourneyError("public response exceeded the evidence limit")
        length = response.headers.get("Content-Length")
        if length is not None and (not re.fullmatch(r"[0-9]+", length) or int(length) != len(body)):
            raise JourneyError("public response body is incomplete")
        return body
    except JourneyError:
        raise
    except Exception as exc:
        raise JourneyError("public response read failed") from exc


def backend_module():
    spec = importlib.util.spec_from_file_location("coverage_backends", pathlib.Path(__file__).with_name("coverage-backends.py"))
    if spec is None or spec.loader is None:
        raise JourneyError("backend validation helper is unavailable")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def guarded_resource(command, kind: str, name: str, action: str, owner: str, run_id: str, lane: str = "", image: str = "", diagnostics: dict[str, Any] | None = None) -> dict[str, Any]:
    result = command(kind, "inspect", name, check=False)
    if missing_object(result, kind, name):
        if action in ("absent", "remove"):
            return {"absent": True}
        raise JourneyError("owned resource is unexpectedly absent")
    if result.returncode != 0:
        raise JourneyError("resource inventory failed; operator reconciliation required")
    if action == "absent":
        raise JourneyError("refusing a pre-existing resource name")
    value = json.loads(result.stdout)
    if not isinstance(value, list) or len(value) != 1 or not isinstance(value[0], dict):
        raise JourneyError("resource inventory is ambiguous")
    info = value[0]
    identity = info.get("Id")
    labels = ((info.get("Config") or {}).get("Labels") or {}) if kind == "container" else (info.get("Labels") or {})
    if not isinstance(identity, str) or not re.fullmatch(r"[0-9a-f]{64}", identity):
        raise JourneyError("resource has no immutable identity")
    if labels.get(LABEL_OWNER) != owner or labels.get(LABEL_RUN) != run_id or lane and labels.get(LABEL_LANE) != lane:
        raise JourneyError("refusing foreign resource ownership")
    if image and info.get("Image") != image:
        raise JourneyError("owned container image differs from candidate")
    if action == "remove":
        command(kind, "rm", *(["-f"] if kind == "container" else []), identity)
        absent = command(kind, "inspect", identity, check=False)
        if not missing_object(absent, kind, identity):
            raise JourneyError("owned resource removal is unproved")
        return {"absent": True, "Id": identity}
    if action == "stop":
        if diagnostics is not None:
            diagnostics["pre_stop"] = safe_process_state(info)
        flush = command("container", "kill", "--signal=SIGUSR2", identity, check=False)
        if diagnostics is not None:
            diagnostics["flush_rc"] = flush.returncode
        time.sleep(1)
        stop = command("container", "stop", "-t", "60", identity, check=False, timeout=75)
        if diagnostics is not None:
            diagnostics["stop_rc"] = stop.returncode
        final = guarded_resource(command, kind, identity, "owned", owner, run_id, lane, image)
        final.update(flush_rc=flush.returncode, stop_rc=stop.returncode)
        state = final["State"]
        if diagnostics is not None:
            diagnostics["post_stop"] = safe_process_state(final)
            diagnostics["refusal_category"] = next((category for failed, category in (
                (flush.returncode != 0, "flush_failed"), (stop.returncode != 0, "stop_failed"),
                (state.get("ExitCode") != 0, "nonzero_exit"), (state.get("OOMKilled") is not False, "oom_state"),
                (state.get("Running") is not False, "still_running"), (final.get("RestartCount") != 0, "restarted")) if failed), "none")
        if (flush.returncode != 0 or stop.returncode != 0 or state.get("ExitCode") != 0
                or state.get("OOMKilled") is not False or state.get("Running") is not False
                or final.get("RestartCount") != 0):
            raise JourneyError("owned process flush/clean shutdown unproved")
        return final
    return {"Id": identity, "Image": info.get("Image"), "State": info.get("State") or {}, "RestartCount": info.get("RestartCount")}


def safe_process_state(info: dict[str, Any]) -> dict[str, Any]:
    """Never persist Config/Env, native Error strings or unknown inspect fields."""
    state = info.get("State") or {}
    safe = {key: state[key] for key in ("Running", "Paused", "Restarting", "OOMKilled", "Dead")
            if isinstance(state.get(key), bool)}
    if type(state.get("ExitCode")) is int:
        safe["ExitCode"] = state["ExitCode"]
    for key in ("StartedAt", "FinishedAt"):
        value = state.get(key)
        if isinstance(value, str) and re.fullmatch(r"[0-9T:.+Z-]{1,40}", value):
            safe[key] = value
    if state.get("Status") in ("created", "running", "paused", "restarting", "removing", "exited", "dead"):
        safe["Status"] = state["Status"]
    return {"Id": info["Id"], "Image": info.get("Image"),
            "RestartCount": info.get("RestartCount") if type(info.get("RestartCount")) is int else None, "State": safe}


def connector_log_records(streams) -> dict[str, Any]:
    """Filter successful log streams; iteration order is not cross-stream chronology."""
    safe_messages = {"connector config loaded", "caesium failure", "graceful shutdown failed",
                     "received shutdown signal", "server shutting down", "server started"}
    records = deque(maxlen=100)
    loaded = None
    counts = {"stdout_bytes": 0, "stderr_bytes": 0, "stdout_lines": 0, "stderr_lines": 0,
              "oversized_lines": 0, "omitted_records": 0, "invalid_utf8_lines": 0}
    for channel, stream in streams:
        stream.seek(0)
        while True:
            raw = stream.readline(64 * 1024 + 1)
            if not raw:
                break
            counts[channel + "_lines"] += 1
            counts[channel + "_bytes"] += len(raw)
            if len(raw) > 64 * 1024:
                counts["oversized_lines"] += 1
                # Drain this whole oversized line in bounded chunks. Never let
                # a valid-looking suffix become an independent loaded event.
                while not raw.endswith(b"\n"):
                    raw = stream.readline(64 * 1024 + 1)
                    counts[channel + "_bytes"] += len(raw)
                    if not raw:
                        break
                continue
            try:
                value = json.loads(raw.decode("utf-8"))
            except UnicodeDecodeError:
                counts["invalid_utf8_lines"] += 1
                value = None
            except (ValueError, RecursionError):
                value = None
            if not isinstance(value, dict):
                counts["omitted_records"] += 1
                records.append({"stream": channel, "msg": "[unstructured log omitted]"})
                continue
            message = value.get("msg")
            safe = isinstance(message, str) and message in safe_messages
            if not safe:
                counts["omitted_records"] += 1
            record = {"stream": channel, "msg": message if safe else "[message details omitted]"}
            for key, pattern in (("level", r"debug|info|warn|error|fatal|panic"),
                                 ("ts", r"[0-9T:.+Z-]{1,40}"),
                                 ("caller", r"[a-zA-Z0-9_/-]{1,140}\.go:[0-9]{1,10}")):
                if isinstance(value.get(key), str) and re.fullmatch(pattern, value[key]) and not KEY_RE.search(value[key]):
                    record[key] = value[key]
            if "error" in value:
                record["error"] = "[error details omitted]"
            if message == "connector config loaded":
                if isinstance(value.get("fingerprint"), str) and DIGEST_RE.fullmatch(value["fingerprint"]):
                    record["fingerprint"] = value["fingerprint"]
                    loaded = record.copy()
            records.append(record)
    counts["retained_records"] = len(records)
    return {"records": list(records), "loaded_event": loaded, "counts": counts}


class ConnectorOutputLimit(JourneyError):
    def __init__(self, limit: int, observed: int):
        super().__init__("connector nonlog output exceeded evidence cap")
        self.limit, self.observed = limit, observed


def connector_shutdown(command, identity: str, owner: str, run_id: str, image: str,
                       audit: pathlib.Path) -> dict[str, Any]:
    diagnostics: dict[str, Any] = {"schema_version": 1, "complete": False, "fingerprint": False,
        "healthy": False, "polls": 0, "refusal_category": "readiness_pending", "log_records": []}
    destination = audit / "connector-diagnostics.json"
    try:
        if destination.exists() or destination.is_symlink():
            raise JourneyError("connector diagnostic path must be fresh")
        if not re.fullmatch(r"[0-9a-f]{64}", identity):
            diagnostics["refusal_category"] = "invalid_immutable_identity"
            raise JourneyError("connector requires an immutable container identity")
        for attempt in range(60):
            diagnostics["polls"] = attempt + 1
            diagnostics["refusal_category"] = "ownership_inventory_failed"
            info = guarded_resource(command, "container", identity, "owned", owner, run_id, "base-connectors", image)
            if info["Id"] != identity:
                raise JourneyError("connector inventory differs from requested immutable identity")
            diagnostics["pre_stop"] = safe_process_state(info)
            diagnostics["refusal_category"] = "log_operation_failed"
            logs = command("container", "logs", "--tail", "all", identity, check=False, timeout=10)
            diagnostics["log_rc"] = logs.returncode
            evidence = json.loads(logs.stdout)
            diagnostics["log_counts"] = evidence["counts"]
            if logs.returncode == 0:
                diagnostics["log_records"] = evidence["records"]
                event = evidence["loaded_event"]
                if event is not None:
                    diagnostics["loaded_event"] = event
                    diagnostics["fingerprint"] = True
            if info["State"].get("Running") is not True:
                diagnostics["refusal_category"] = "stopped_before_readiness"
                raise JourneyError("connector stopped before fingerprint and health readiness")
            diagnostics["refusal_category"] = "health_operation_failed"
            health = command("container", "exec", identity, "wget", "-q", "-T", "2", "-O", "-",
                             "http://127.0.0.1:8080/health", check=False, timeout=5)
            diagnostics["health_rc"] = health.returncode
            try:
                body = json.loads(health.stdout)
            except ValueError:
                body = None
            diagnostics["healthy"] = (health.returncode == 0 and isinstance(body, dict)
                                      and body.get("status") == "healthy")
            if logs.returncode == 0 and diagnostics["fingerprint"] and diagnostics["healthy"]:
                break
            time.sleep(1)
        else:
            diagnostics["refusal_category"] = "never_ready"
            raise JourneyError("connector did not reach loaded fingerprint and health readiness")
        diagnostics["refusal_category"] = "stop_operation_failed"
        final = guarded_resource(command, "container", identity, "stop", owner, run_id, "base-connectors", image,
                                 diagnostics=diagnostics)
        diagnostics["complete"] = True
        return {**safe_process_state(final), "flush_rc": final["flush_rc"], "stop_rc": final["stop_rc"]}
    except Exception as exc:
        if isinstance(exc, ConnectorOutputLimit):
            diagnostics.update(refusal_category="nonlog_output_cap", output_limit=exc.limit,
                               observed_output_bytes_at_least=exc.observed)
        diagnostics["exception_type"] = type(exc).__name__ if type(exc).__name__ in (
            "JourneyError", "ConnectorOutputLimit", "TimeoutExpired", "OSError", "ValueError", "JSONDecodeError") else "UnexpectedError"
        raise
    finally:
        # A diagnostics write failure propagates; it never converts refusal to pass.
        if "pre_stop" in diagnostics:
            try:
                guarded_resource(command, "container", identity, "owned", owner, run_id, "base-connectors", image)
                logs = command("container", "logs", "--tail", "all", identity, check=False, timeout=10)
                diagnostics["post_log_rc"] = logs.returncode
                if logs.returncode == 0:
                    evidence = json.loads(logs.stdout)
                    diagnostics["post_log_records"] = evidence["records"]
                    diagnostics["post_log_counts"] = evidence["counts"]
            except Exception:
                diagnostics["post_log_category"] = "unavailable"
        with os.fdopen(os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w") as stream:
            stream.write(json.dumps(diagnostics, indent=2) + "\n")


def connector_command(*arguments, check=True, timeout=10):
    # Successful Docker logs may transport app records on either channel. Keep
    # both private, and return only reduced structured evidence. Native errors
    # from failed log commands never supply loaded/readiness proof.
    with tempfile.TemporaryFile() as output, tempfile.TemporaryFile() as errors:
        result = subprocess.run(["docker", *arguments], stdout=output, stderr=errors, timeout=timeout, check=False)
        if arguments[:2] == ("container", "logs"):
            evidence = (connector_log_records((("stdout", output), ("stderr", errors)))
                        if result.returncode == 0 else {"records": [], "loaded_event": None,
                            "counts": {"failed_log_streams_omitted": 2}})
            raw = json.dumps(evidence)
        else:
            output.seek(0)
            data = output.read(2 * 1024 * 1024 + 1)
            if len(data) > 2 * 1024 * 1024:
                raise ConnectorOutputLimit(2 * 1024 * 1024, len(data))
            raw = data.decode("utf-8")
        if check and result.returncode:
            raise JourneyError("connector Docker command failed")
        return subprocess.CompletedProcess(arguments, result.returncode, raw, "")


def validate_backend_contribution(args: argparse.Namespace) -> None:
    def require(condition: bool, message: str) -> None:
        if not condition:
            raise JourneyError(message)

    def read_json(path: pathlib.Path, label: str) -> tuple[dict[str, Any], bytes]:
        try:
            raw = path.read_bytes()
            value = json.loads(raw)
        except (OSError, json.JSONDecodeError) as exc:
            raise JourneyError(f"backend {label} is unavailable or invalid JSON") from exc
        require(isinstance(value, dict), f"backend {label} must be a JSON object")
        return value, raw

    def digest(raw: bytes) -> str:
        return hashlib.sha256(raw).hexdigest()

    context_source = pathlib.Path(args.context)
    inputs_source = pathlib.Path(args.inputs)
    output_source = pathlib.Path(args.output)
    require(context_source.is_absolute() and inputs_source.is_absolute() and output_source.is_absolute(), "backend manifest and output paths must be absolute")
    require(context_source.is_file() and not context_source.is_symlink() and inputs_source.is_file() and not inputs_source.is_symlink(), "backend manifests must be regular non-symlink files")
    require(output_source.is_dir() and not output_source.is_symlink(), "backend output root must be a regular directory")
    context_path = context_source.resolve(strict=True)
    inputs_path = inputs_source.resolve(strict=True)
    output_root = output_source.resolve(strict=True)
    context, context_raw = read_json(context_path, "producer context")
    inputs, inputs_raw = read_json(inputs_path, "producer prerequisites")
    context_sha = digest(context_raw)
    inputs_sha = digest(inputs_raw)
    require(context_sha == args.context_sha256, "backend producer context digest changed")
    require(inputs_sha == args.inputs_sha256, "backend prerequisite digest changed")
    require(context.get("schema_version") == 1 and context.get("producer") == "scripts/integration-coverage.sh", "backend context has foreign producer identity")
    require(context.get("candidate_sha") == args.candidate_sha and context.get("image_id") == args.image_id, "backend context differs from the active candidate")
    require(context.get("builder_image_id") == args.builder_image_id and context.get("build_context") == args.build_context, "backend context differs from the pinned toolchain")
    require(context.get("image_provenance") == "built-by-this-run" and context.get("verified") is True, "backend context is not verified built-by-this-run evidence")
    require(context.get("platform") == args.platform and context.get("container_cli") == "docker", "backend context platform/container engine mismatch")
    require(inputs.get("schema_version") == 1 and inputs.get("platform") == args.platform, "backend prerequisites use another schema or platform")
    require(context.get("coverage_id") == args.run_id, "backend context belongs to another collection run")
    require(isinstance(inputs.get("docker_socket"), str) and inputs["docker_socket"] == context.get("socket_path"), "backend prerequisite socket differs from the collector socket")
    require(inputs.get("podman_privileged_approved") is True, "Podman privileged fixture approval is missing")

    inventory_ref = context.get("source_inventory")
    require(isinstance(inventory_ref, dict) and DIGEST_RE.fullmatch(inventory_ref.get("sha256", "")), "backend context source inventory binding is invalid")
    inventory_value = pathlib.PurePosixPath(str(inventory_ref.get("path", "")))
    artifact_dir = pathlib.Path(context.get("artifact_dir", ""))
    require(artifact_dir.is_absolute(), "backend context artifact directory must be absolute")
    require(not inventory_value.is_absolute() and ".." not in inventory_value.parts, "backend source inventory path escapes the artifact directory")
    inventory_path = artifact_dir / pathlib.Path(*inventory_value.parts)
    require(inventory_path.is_file() and not inventory_path.is_symlink(), "backend source inventory file is missing or unsafe")
    inventory_raw = inventory_path.read_bytes()
    require(digest(inventory_raw) == inventory_ref["sha256"], "backend source inventory digest changed")

    result, _ = read_json(output_root / "result.json", "run result")
    contribution, contribution_raw = read_json(output_root / "contribution.json", "contribution")
    require(complete_backend_gate(result),
            "backend runner did not complete both coverage backends in gate mode")
    require(result.get("candidate_sha") == args.candidate_sha and result.get("selected") == ["kubernetes", "podman"], "backend runner selected a different candidate/backend set")
    expected_top = {"schema_version", "kind", "complete", "candidate_sha", "image_id", "builder_image_id", "build_context", "image_provenance", "verified", "lanes"}
    require(set(contribution) == expected_top, "backend contribution has missing or unexpected fields")
    require(contribution.get("schema_version") == 1 and contribution.get("kind") == "real-backend-coverage" and contribution.get("complete") is True, "backend contribution is incomplete")
    for key in ("candidate_sha", "image_id", "builder_image_id", "build_context", "image_provenance", "verified"):
        require(contribution.get(key) == context.get(key), f"backend contribution {key} differs from producer context")

    reports = result.get("contributors")
    require(isinstance(reports, list) and len(reports) == 2, "backend result is missing one or more contributor reports")
    report_by_backend: dict[str, dict[str, Any]] = {}
    for report in reports:
        require(isinstance(report, dict), "backend contributor report is invalid")
        backend = report.get("backend")
        require(backend in ("kubernetes", "podman") and backend not in report_by_backend, "backend contributor identity is missing or duplicated")
        report_by_backend[backend] = report
        saved_report, _ = read_json(output_root / backend / "backend-result.json", f"{backend} report")
        require(saved_report == report, f"{backend} saved report differs from the run manifest")
        require(report.get("complete") is True and report.get("missing") is False and report.get("killed") is False, f"{backend} report is incomplete or killed")
        require(not report.get("failed") and report.get("cleanup_errors") == [], f"{backend} failed or left owned resources behind")
        require(report.get("candidate_sha") == args.candidate_sha and report.get("image_id") == args.image_id and report.get("builder_image_id") == args.builder_image_id, f"{backend} report belongs to another image/candidate")
        cases = report.get("cases")
        require(isinstance(cases, list) and [case.get("case") for case in cases] == ["success", "failure", "deadline"] and all(case.get("status") == "pass" for case in cases), f"{backend} did not pass all real task scenarios")
        require(len(report.get("processes", [])) == 7, f"{backend} process inventory is incomplete")
    require(set(report_by_backend) == {"kubernetes", "podman"}, "both real backend contributor reports are required")

    lanes = contribution.get("lanes")
    require(isinstance(lanes, list) and len(lanes) == 14, "backend contribution must contain all fourteen process profiles")
    cli_dirs: list[str] = []
    server_dirs: list[str] = []
    seen_lanes: set[str] = set()
    seen_paths: set[str] = set()
    binary_hashes: set[str] = set()
    process_counts = {"kubernetes": {"cli": 0, "server": 0}, "podman": {"cli": 0, "server": 0}}
    expected_suite = {"run_pattern": "backend-success|failure|deadline", "minimum_passes": 3, "passed_scenarios": 3, "exit_code": 0}

    def resolve_output_path(value: Any, label: str) -> tuple[pathlib.Path, str]:
        require(isinstance(value, str) and value and "\\" not in value, f"backend {label} path is invalid")
        relative = pathlib.PurePosixPath(value)
        require(not relative.is_absolute() and ".." not in relative.parts and relative.parts, f"backend {label} path escapes its evidence root")
        path = output_root.joinpath(*relative.parts)
        require(not any(parent.is_symlink() for parent in (path, *path.parents) if parent != output_root.parent), f"backend {label} path traverses a symlink")
        resolved = path.resolve(strict=True)
        require(resolved.is_relative_to(output_root), f"backend {label} path resolves outside its evidence root")
        return resolved, relative.as_posix()

    for lane_ref in lanes:
        require(isinstance(lane_ref, dict) and set(lane_ref) == {"backend", "lane", "source", "raw_dir", "provenance_path"}, "backend lane reference has missing or unexpected fields")
        backend, lane, source = lane_ref["backend"], lane_ref["lane"], lane_ref["source"]
        require(backend in report_by_backend and source in ("cli", "server"), "backend lane has an unknown backend or source")
        require(isinstance(lane, str) and lane not in seen_lanes, "backend lane identity is missing or duplicated")
        seen_lanes.add(lane)
        raw_dir, raw_rel = resolve_output_path(lane_ref["raw_dir"], "raw")
        provenance_path, provenance_rel = resolve_output_path(lane_ref["provenance_path"], "provenance")
        require(raw_rel not in seen_paths and provenance_rel not in seen_paths, "backend contribution reuses a raw/provenance path")
        seen_paths.update((raw_rel, provenance_rel))
        provenance, _ = read_json(provenance_path, "process provenance")
        report_processes = report_by_backend[backend].get("processes", [])
        matching = [record for record in report_processes if record.get("lane") == lane]
        require(len(matching) == 1, "backend lane does not match exactly one recorded process")
        process_ref = matching[0]
        require(process_ref.get("raw_dir") == raw_rel and process_ref.get("provenance_path") == provenance_rel, "backend process path differs from its persisted provenance")
        require(provenance.get("source") == source and provenance.get("backend") == backend and provenance.get("lane") == lane, "backend process provenance identity mismatch")
        for key in ("candidate_sha", "image_id", "builder_image_id", "build_context", "image_provenance", "verified"):
            require(provenance.get(key) == context.get(key), f"backend process {key} differs from producer context")
        require(provenance.get("schema_version") == 1 and provenance.get("kind") == "gocoverdir" and provenance.get("module") == MODULE, "backend process provenance schema/module mismatch")
        require(provenance.get("complete") is True and provenance.get("missing") is False and provenance.get("killed") is False, "backend process provenance is incomplete or killed")
        require(provenance.get("source_inventory_sha256") == inventory_ref["sha256"] and provenance.get("producer_context_sha256") == context_sha and provenance.get("producer_inputs_sha256") == inputs_sha, "backend process producer hashes do not match the staged evidence")
        require(provenance.get("test_suite") == expected_suite, "backend process lacks the three passing real backend scenarios")
        container_id = provenance.get("container_id")
        require(isinstance(container_id, str) and re.fullmatch(r"[0-9a-f]{64}", container_id), "backend process immutable container ID is invalid")
        require(provenance.get("oom_killed") is False, "backend process was OOM-killed")
        if source == "server":
            require(provenance.get("exit_code") == 0, "backend server did not exit after SIGTERM")
            require(provenance.get("flush") == "sigusr2" and provenance.get("signal") == "SIGTERM" and provenance.get("flush_rc") == 0 and provenance.get("stop_rc") == 0, "backend server did not flush and stop gracefully")
        else:
            require(provenance.get("exit_code") == 0, "backend CLI process did not exit cleanly")
            require(provenance.get("flush") == "process-exit" and provenance.get("signal") is None and provenance.get("flush_rc") is None and provenance.get("stop_rc") is None, "backend CLI process-exit provenance is invalid")
        receipts = provenance.get("backend_receipts")
        receipt_key = "kind_image_id" if backend == "kubernetes" else "podman_service_image_id"
        expected_receipt_fields = {"platform", "task_archive_sha256", "task_image_id", "task_image_ref", receipt_key}
        if "task_docker_image_id" in inputs:
            expected_receipt_fields.add("task_docker_image_id")
        expected_receipt_fields.update(("task_image_supplier_ref", "kind_image_supplier_ref"))
        require(isinstance(receipts, dict) and set(receipts) == expected_receipt_fields, "backend process receipt fields are incomplete or unexpected")
        for key in expected_receipt_fields:
            require(receipts.get(key) == inputs.get(key), f"backend process receipt {key} differs from staged prerequisites")
        binary_hash = provenance.get("binary_sha256")
        require(isinstance(binary_hash, str) and DIGEST_RE.fullmatch(binary_hash), "backend candidate binary digest is invalid")
        binary_hashes.add(binary_hash)

        require(raw_dir.is_dir() and not raw_dir.is_symlink(), "backend raw profile directory is unavailable")
        files: dict[str, str] = {}
        for raw_file in sorted(raw_dir.iterdir()):
            require(raw_file.is_file() and not raw_file.is_symlink() and raw_file.stat().st_size > 0 and raw_file.name.startswith(("covmeta.", "covcounters.")), "backend raw profile contains missing or foreign files")
            files[raw_file.name] = digest(raw_file.read_bytes())
        require(any(name.startswith("covmeta.") for name in files) and any(name.startswith("covcounters.") for name in files), "backend raw profile lacks Go metadata/counters")
        require(provenance.get("files") == files, "backend raw profile bytes differ from the persisted digest inventory")
        process_counts[backend][source] += 1
        if source == "cli":
            cli_dirs.append(raw_rel)
        else:
            server_dirs.append(raw_rel)

    require(process_counts == {"kubernetes": {"cli": 6, "server": 1}, "podman": {"cli": 6, "server": 1}}, "backend profile source counts do not match the required six CLI and one server per backend")
    require(len(binary_hashes) == 1, "backend contributors used different candidate binaries")
    final_path = pathlib.Path(args.final_manifest)
    cli_list = pathlib.Path(args.cli_list)
    server_list = pathlib.Path(args.server_list)
    require(all(path.is_absolute() for path in (final_path, cli_list, server_list)), "backend output manifest/path-list paths must be absolute")
    for path in (final_path, cli_list, server_list):
        require(path.parent.is_dir() and not path.parent.is_symlink(), "backend output parent must be an existing non-symlink directory")
    require(not final_path.exists() and not final_path.is_symlink(), "refusing to overwrite a backend output manifest")
    for path in (cli_list, server_list):
        require(not path.exists() and not path.is_symlink(), "refusing to overwrite backend profile path lists")
    for path, content, mode in (
        (final_path, contribution_raw, 0o400),
        (cli_list, "".join(f"{value}\n" for value in cli_dirs).encode(), 0o600),
        (server_list, "".join(f"{value}\n" for value in server_dirs).encode(), 0o600),
    ):
        descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, mode)
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(content)
        path.chmod(mode)


class Collector:
    def __init__(self, args: argparse.Namespace) -> None:
        self.args = args
        self.root = pathlib.Path(args.root).resolve()
        self.artifacts = pathlib.Path(args.artifacts).resolve()
        self.raw = pathlib.Path(args.raw).resolve()
        self.sso_artifacts = self.artifacts
        self.fixture_dir = self.artifacts / "fixture"
        self.server_raw_root = self.raw / "journeys" / "sso"
        self.database_dir: pathlib.Path | None = None
        self.secret_dir: pathlib.Path | None = None
        self.image = args.coverage_image
        self.builder = args.builder_image
        self.platform = args.platform
        self.sha = args.candidate_sha
        self.run_id = args.run_id
        self.build_context = json.loads(args.build_context)
        self.image_provenance = args.image_provenance
        self.verified = args.verified == "true"
        self.docker = args.container_cli
        self.docker_socket = args.docker_socket
        self.socket_gid = args.socket_gid
        self.docker_env = os.environ.copy()
        for name in ("DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_API_VERSION"):
            self.docker_env.pop(name, None)
        self.docker_env["DOCKER_HOST"] = "unix://" + self.docker_socket
        self.backend_inputs_path = pathlib.Path(args.backend_inputs)
        self.backend_inputs_sha256 = args.backend_inputs_sha256
        self.backend_inputs: dict[str, Any] = {}
        self.task_image_id = ""
        self.task_docker_image_id = ""
        self.task_image_ref = ""
        self.network_name = f"{self.run_id}-sso-net"
        self.network_id = ""
        self.network_attempted = False
        self.ids: list[tuple[str, str]] = []
        self.secret_files: list[pathlib.Path] = []
        self.protocol_events: list[dict[str, Any]] = []
        self.server_generations: list[dict[str, Any]] = []
        self.cli_processes: list[dict[str, Any]] = []
        self.server_environment_sha256 = ""
        self.shutdown_job: dict[str, Any] | None = None
        self.shutdown_control_dir: pathlib.Path | None = None
        self.shutdown_control_mount_source_sha256 = ""
        self.shutdown_release_path: pathlib.Path | None = None
        self.native_runtime_absent_generations: list[int] = []
        self.bootstrap_key = ""
        self.api_env_file: pathlib.Path | None = None
        self.helper_process: subprocess.Popen[bytes] | None = None
        self.helper_container_id = ""
        self.pending_containers: dict[str, str] = {}
        self.cleanup_errors: list[str] = []
        self.cleanup_complete = False

        self.idp_alias = f"{self.run_id}-sso-idp"
        self.app_alias = f"{self.run_id}-sso-app"
        self.idp_name = f"{self.run_id}-sso-idp"
        self.server_name = f"{self.run_id}-sso-server"
        self.journey_name = f"{self.run_id}-sso-journey"
        self.issuer = f"http://{self.idp_alias}:8090"
        self.sp_base = f"http://{self.app_alias}:8080"
        self.idp_env_file = pathlib.Path()
        self.server_env_file = pathlib.Path()
        self.protocol_log = self.sso_artifacts / "events.jsonl"
        self.auth_status_file = self.sso_artifacts / "auth-status.json"
        self.idp_ready_file = self.sso_artifacts / "idp-ready.json"
        self.binary_path = self.fixture_dir / "sso-idp"
        self.metadata_path = self.fixture_dir / "idp.xml"
        self.job_path = self.artifacts / "shutdown.job.yaml"

    def docker_run(
        self,
        *arguments: str,
        check: bool = True,
        timeout: int = 180,
        capture: bool = True,
    ) -> subprocess.CompletedProcess[str]:
        command = [self.docker, *map(str, arguments)]
        process = subprocess.Popen(command, text=True, stdout=subprocess.PIPE if capture else None,
                                   stderr=subprocess.PIPE if capture else None, env=self.docker_env, start_new_session=True)
        try:
            stdout, stderr = process.communicate(timeout=timeout)
        except BaseException as exc:
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                process.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                process.communicate(timeout=5)
            raise JourneyError("container command interrupted; operator reconciliation required") from exc
        result = subprocess.CompletedProcess(command, process.returncode, stdout or "", stderr or "")
        if check and result.returncode != 0:
            # Do not echo arbitrary daemon output: inspect/log errors can carry
            # startup environment, and application logs may contain protocol
            # payloads. The caller receives the command and exit status only.
            raise JourneyError(f"container command failed ({result.returncode}): {command[1]}")
        return result

    def docker_stdout(self, *arguments: str, timeout: int = 180) -> str:
        result = self.docker_run(*arguments, timeout=timeout)
        return result.stdout.strip()

    def require_absent_container(self, name: str) -> None:
        result = self.docker_run("container", "inspect", name, check=False)
        if not missing_object(result, "container", name):
            raise JourneyError("container name is not provably free")

    def create_network(self) -> None:
        result = self.docker_run("network", "inspect", self.network_name, check=False)
        if not missing_object(result, "network", self.network_name):
            raise JourneyError("SSO network name is not provably free")
        self.network_attempted = True
        created = self.docker_stdout(
            "network",
            "create",
            "--label",
            f"{LABEL_OWNER}={self.sha}",
            "--label",
            f"{LABEL_RUN}={self.run_id}",
            self.network_name,
        )
        if not created:
            raise JourneyError("created SSO network has no immutable ID")
        self.network_id = created
        info = json.loads(self.docker_stdout("network", "inspect", self.network_id))[0]
        inspected_id = str(info.get("Id") or "")
        labels = info.get("Labels") or {}
        if labels.get(LABEL_OWNER) != self.sha or labels.get(LABEL_RUN) != self.run_id:
            raise JourneyError("created SSO network has unexpected ownership labels")
        if not inspected_id or inspected_id != self.network_id:
            raise JourneyError("created SSO network has no immutable ID")

    def build_fixture(self) -> None:
        if self.fixture_dir.exists() or self.fixture_dir.is_symlink():
            raise JourneyError(f"refusing pre-existing fixture artifact path {self.fixture_dir}")
        self.fixture_dir.mkdir(mode=0o700, parents=False)
        self.fixture_dir.chmod(0o777)
        build_name = self.run_id + "-sso-fixture-build"
        self.require_absent_container(build_name)
        self.pending_containers[build_name] = "sso-fixture-build"
        self.docker_run(
            "run",
            "--name", build_name,
            *self.container_labels("sso-fixture-build"),
            "--pull=never",
            "--rm",
            "--platform",
            self.platform,
            "-v",
            f"{self.root}:/source:ro",
            "-v",
            f"{self.fixture_dir}:/fixture",
            "-w",
            "/source",
            "-e",
            "GOTOOLCHAIN=local",
            "-e",
            "GOPROXY=off",
            "-e",
            "GOFLAGS=-buildvcs=false",
            self.builder,
            "sh",
            "-c",
            "set -eu; go build -tags=integration -o /fixture/sso-idp ./test/fixtures/sso-idp; "
            "chmod 0755 /fixture/sso-idp; test -x /fixture/sso-idp",
            timeout=900,
        )
        try:
            binary = self.binary_path.lstat()
        except OSError as exc:
            raise JourneyError("builder did not produce the SSO fixture executable") from exc
        if not stat.S_ISREG(binary.st_mode) or binary.st_size == 0 or not binary.st_mode & 0o111:
            raise JourneyError("builder did not produce a nonempty regular executable SSO fixture")

    def write_secrets(self) -> None:
        client_secret = secrets.token_urlsafe(48)
        hash_secret = secrets.token_urlsafe(48)
        self.idp_env_file.write_text(f"SSO_FIXTURE_CLIENT_SECRET={client_secret}\n")
        self.server_env_file.write_text(
            "\n".join(
                (
                    f"CAESIUM_AUTH_KEY_HASH_SECRET={hash_secret}",
                    f"CAESIUM_AUTH_OIDC_CLIENT_SECRET={client_secret}",
                    "",
                )
            )
        )
        self.idp_env_file.chmod(0o600)
        self.server_env_file.chmod(0o600)
        self.secret_files.extend((self.idp_env_file, self.server_env_file))

    def container_labels(self, lane: str, generation: int | None = None) -> list[str]:
        labels = [
            "--label",
            f"{LABEL_OWNER}={self.sha}",
            "--label",
            f"{LABEL_RUN}={self.run_id}",
            "--label",
            f"{LABEL_LANE}={lane}",
        ]
        if generation is not None:
            labels += ["--label", f"caesium.coverage.generation={generation}"]
        return labels

    def inspect_container(self, container_id: str) -> dict[str, Any]:
        raw = self.docker_stdout("container", "inspect", container_id)
        value = json.loads(raw)
        if not isinstance(value, list) or len(value) != 1 or not isinstance(value[0], dict):
            raise JourneyError(f"container inspect returned an invalid record for {container_id}")
        return value[0]

    def assert_owned(self, container_id: str, lane: str) -> dict[str, Any]:
        info = self.inspect_container(container_id)
        labels = (info.get("Config") or {}).get("Labels") or {}
        if labels.get(LABEL_OWNER) != self.sha or labels.get(LABEL_RUN) != self.run_id:
            raise JourneyError(f"refusing foreign container {container_id}")
        if labels.get(LABEL_LANE) != lane:
            raise JourneyError(f"container {container_id} has unexpected lane label")
        if info.get("Id") != container_id and info.get("Name", "").lstrip("/") != container_id:
            raise JourneyError("owned resource immutable identity changed")
        return info

    def remove_owned(self, container_id: str, lane: str) -> None:
        expected_image = self.builder if lane in ("sso-idp", "sso-journey", "sso-fixture-build") else self.image
        guarded_resource(self.docker_run, "container", container_id, "remove", self.sha, self.run_id, lane, expected_image)
        self.ids = [(cid, tag) for cid, tag in self.ids if cid != container_id]
        self.pending_containers = {name: tag for name, tag in self.pending_containers.items() if tag != lane}

    def start_idp(self) -> None:
        self.require_absent_container(self.idp_name)
        self.pending_containers[self.idp_name] = "sso-idp"
        result = self.docker_run(
            "run",
            "-d",
            "--pull=never",
            "--platform",
            self.platform,
            "--name",
            self.idp_name,
            *self.container_labels("sso-idp"),
            "--network",
            self.network_name,
            "--network-alias",
            self.idp_alias,
            "--env-file",
            str(self.idp_env_file),
            "-v",
            f"{self.fixture_dir}:/fixture",
            "--entrypoint",
            "/fixture/sso-idp",
            self.builder,
            "serve",
            "--listen",
            ":8090",
            "--issuer",
            self.issuer,
            "--sp-base",
            self.sp_base,
            "--metadata-file",
            "/fixture/idp.xml",
        )
        container_id = result.stdout.strip()
        if not container_id:
            raise JourneyError("SSO fixture server did not return a container ID")
        self.ids.append((container_id, "sso-idp"))
        info = self.assert_owned(container_id, "sso-idp")
        if info.get("Image") != self.builder:
            # Docker inspect stores the immutable image ID after resolving the
            # supplied builder ID; require that it is the resolved image.
            raise JourneyError("SSO fixture server did not use the pinned builder image")

    def candidate_get(self, url: str, *, network: str | None = None, attempts: int = 1) -> str:
        for _ in range(attempts):
            args = [
                "run",
                "--pull=never",
                "--rm",
                "--platform",
                self.platform,
            ]
            if network:
                args += ["--network", network]
            args += ["--entrypoint", "wget", self.image, "-q", "-O", "-", url]
            result = self.docker_run(*args, check=False, timeout=30)
            if result.returncode == 0:
                return result.stdout
            time.sleep(1)
        raise JourneyError(f"candidate image could not read {url} after {attempts} attempts")

    def await_idp(self) -> None:
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            try:
                body = self.candidate_get(f"{self.issuer}/ready", network=self.network_name)
                record = json.loads(body)
                if isinstance(record, dict) and (
                    record.get("ready") is True
                    and record.get("version") == 1
                    and record.get("issuer") == self.issuer
                    and record.get("sp_base") == self.sp_base
                    and set(record) == {"ready", "version", "issuer", "sp_base"}
                    and self.metadata_path.is_file()
                ):
                    self.idp_ready_file.write_text(json.dumps(record, indent=2) + "\n")
                    return
            except (JourneyError, json.JSONDecodeError):
                pass
        raise JourneyError("SSO fixture server did not expose a matching ready record")

    def server_environment(self) -> dict[str, str]:
        client_secret = self._secret_from_file("CAESIUM_AUTH_OIDC_CLIENT_SECRET", self.server_env_file)
        hash_secret = self._secret_from_file("CAESIUM_AUTH_KEY_HASH_SECRET", self.server_env_file)
        return {
            "CAESIUM_AUTH_MODE": "api-key",
            "CAESIUM_AUTH_REQUIRE_TLS": "false",
            "CAESIUM_AUTH_KEY_HASH_SECRET": hash_secret,
            "CAESIUM_AUTH_PUBLIC_BASE_URL": self.sp_base,
            "CAESIUM_AUTH_ROLE_MAPPING": "coverage-admins=admin;coverage-readers=viewer",
            "CAESIUM_AUTH_DEFAULT_ROLE": "",
            "CAESIUM_AUTH_OIDC_ENABLED": "true",
            "CAESIUM_AUTH_OIDC_ISSUER_URL": self.issuer,
            "CAESIUM_AUTH_OIDC_CLIENT_ID": "coverage-caesium",
            "CAESIUM_AUTH_OIDC_CLIENT_SECRET": client_secret,
            "CAESIUM_AUTH_OIDC_SCOPES": "openid profile email groups",
            "CAESIUM_AUTH_OIDC_GROUPS_CLAIM": "groups",
            "CAESIUM_AUTH_SAML_ENABLED": "true",
            "CAESIUM_AUTH_SAML_IDP_METADATA_FILE": "/fixture/idp.xml",
            "CAESIUM_DATABASE_CONSOLE_ENABLED": "true",
            "CAESIUM_DATABASE_PATH": "/var/lib/caesium/dqlite",
            "DOCKER_HOST": "unix:///var/run/docker.sock",
            "CAESIUM_LOG_LEVEL": "info",
            "GOCOVERDIR": "/var/lib/caesium/coverage",
        }

    @staticmethod
    def _secret_from_file(name: str, path: pathlib.Path) -> str:
        for line in path.read_text().splitlines():
            key, separator, value = line.partition("=")
            if separator and key == name:
                return value
        raise JourneyError(f"temporary secret file is missing {name}")

    def start_server(self, generation: int) -> str:
        lane = f"sso-server-g{generation}"
        server_raw = self.server_raw_root / f"server-g{generation}"
        if server_raw.exists() or server_raw.is_symlink():
            raise JourneyError(f"refusing pre-existing SSO server profile path {server_raw}")
        server_raw.mkdir(parents=True, mode=0o777)
        server_raw.chmod(0o777)
        self.require_absent_container(self.server_name)
        self.pending_containers[self.server_name] = lane
        environment = self.server_environment()
        self.server_environment_sha256 = hashlib.sha256(
            json.dumps(environment, sort_keys=True, separators=(",", ":")).encode()
        ).hexdigest()
        self.server_env_file.write_text("".join(f"{name}={value}\n" for name, value in environment.items()))
        self.server_env_file.chmod(0o600)
        server_args = [
            "run",
            "-d",
            "--pull=never",
            "--platform",
            self.platform,
            "--name",
            self.server_name,
            *self.container_labels(lane, generation),
            "--network",
            self.network_name,
            "--network-alias",
            self.app_alias,
            "--user",
            "10001:10001",
            "--group-add",
            str(self.socket_gid),
            "--env-file",
            str(self.server_env_file),
            "-p",
            "127.0.0.1::8080",
            "-v",
            f"{self.docker_socket}:/var/run/docker.sock",
            "-v",
            f"{server_raw}:/var/lib/caesium/coverage",
            "-v",
            f"{self.database_dir}:/var/lib/caesium/dqlite",
            "-v",
            f"{self.metadata_path}:/fixture/idp.xml:ro",
            self.image,
            "start",
        ]
        result = self.docker_run(*server_args)
        container_id = result.stdout.strip()
        if not container_id:
            raise JourneyError(f"SSO server generation {generation} has no container ID")
        self.ids.append((container_id, lane))
        info = self.assert_owned(container_id, lane)
        if info.get("Image") != self.image:
            raise JourneyError(f"SSO server generation {generation} differs from the pinned coverage image")
        self.await_server(container_id)
        return container_id

    def load_backend_prerequisites(self) -> None:
        if not self.backend_inputs_path.is_absolute() or self.backend_inputs_path.is_symlink() or not self.backend_inputs_path.is_file():
            raise JourneyError("SSO shutdown journey requires a regular staged backend prerequisite file")
        raw = self.backend_inputs_path.read_bytes()
        if hashlib.sha256(raw).hexdigest() != self.backend_inputs_sha256:
            raise JourneyError("SSO backend prerequisite digest changed")
        try:
            inputs = json.loads(raw)
        except (json.JSONDecodeError, UnicodeDecodeError) as exc:
            raise JourneyError("SSO backend prerequisites are invalid JSON") from exc
        if not isinstance(inputs, dict) or inputs.get("schema_version") != 1:
            raise JourneyError("SSO backend prerequisite schema is unsupported")
        if inputs.get("platform") != self.platform or inputs.get("docker_socket") != self.docker_socket:
            raise JourneyError("SSO backend prerequisite platform or Docker socket differs from the producer")
        task_image_id = inputs.get("task_image_id")
        task_image_ref = inputs.get("task_image_ref")
        if not isinstance(task_image_id, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", task_image_id):
            raise JourneyError("SSO task image must have an immutable config identity")
        if not isinstance(task_image_ref, str) or not re.fullmatch(r"[a-z0-9][a-z0-9./:_-]+", task_image_ref) or task_image_ref.endswith(":latest"):
            raise JourneyError("SSO task image must use the exact pinned non-latest reference")
        if not isinstance(inputs.get("task_archive_sha256"), str) or not DIGEST_RE.fullmatch(inputs["task_archive_sha256"]):
            raise JourneyError("SSO task image archive digest is invalid")
        if inputs.get("podman_privileged_approved") is not True:
            raise JourneyError("SSO shutdown journey requires the staged privileged-fixture approval")
        backend_module().archive_identity(inputs.get("task_archive", ""), inputs["task_archive_sha256"], task_image_id, task_image_ref, self.platform.split("/")[1])
        docker_identity = inputs.get("task_docker_image_id", task_image_id)
        if not isinstance(docker_identity, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", docker_identity):
            raise JourneyError("loaded Docker task receipt is invalid")
        actual = json.loads(self.docker_stdout("image", "inspect", task_image_ref))[0]
        if actual.get("Id") != docker_identity or actual.get("Os") != "linux" or actual.get("Architecture") != self.platform.split("/")[1] or task_image_ref not in (actual.get("RepoTags") or []):
            raise JourneyError("preloaded Docker task image differs from its pinned Docker/platform receipt")
        self.task_docker_image_id = docker_identity
        self.backend_inputs = inputs
        self.task_image_id = task_image_id
        self.task_image_ref = task_image_ref

    def bootstrap_admin_key(self, container_id: str) -> None:
        logs = self.docker_stdout("container", "logs", container_id)
        candidates = re.findall(r"(?m)^\s*(csk_[A-Za-z0-9_-]+)\s*$", logs)
        del logs
        if len(candidates) != 1:
            raise JourneyError("fresh SSO database did not emit exactly one bootstrap API key")
        self.bootstrap_key = candidates[0]
        self.api_env_file = self.secret_dir / "candidate-cli.env"
        self.api_env_file.write_text(f"CAESIUM_API_KEY={self.bootstrap_key}\n")
        self.api_env_file.chmod(0o600)
        self.secret_files.append(self.api_env_file)

    def host_api_base(self, container_id: str) -> str:
        mapping = self.docker_stdout("port", container_id, "8080/tcp")
        match = re.fullmatch(r"127\.0\.0\.1:(\d+)", mapping.strip())
        if not match:
            raise JourneyError("SSO server did not expose its API on a loopback-only ephemeral port")
        return f"http://127.0.0.1:{match.group(1)}"

    def api_json(self, container_id: str, path: str) -> Any:
        if not self.bootstrap_key:
            raise JourneyError("SSO candidate API key is unavailable")
        request = Request(
            self.host_api_base(container_id) + path,
            headers={"Authorization": "Bearer " + self.bootstrap_key, "Accept": "application/json"},
        )
        try:
            with urlopen(request, timeout=10) as response:
                if response.status != 200:
                    raise JourneyError("SSO public API returned an unexpected status")
                body = complete_response_bytes(response, 2 * 1024 * 1024)
        except (HTTPError, URLError, TimeoutError) as exc:
            raise JourneyError("SSO public API request failed") from exc
        if len(body) > 2 * 1024 * 1024:
            raise JourneyError("SSO public API response exceeded the evidence limit")
        try:
            return json.loads(body)
        except (json.JSONDecodeError, UnicodeDecodeError) as exc:
            raise JourneyError("SSO public API returned invalid JSON") from exc

    def candidate_cli(self, lane: str, *args: str, timeout: int = 60) -> str:
        if self.api_env_file is None:
            raise JourneyError("SSO candidate CLI credential file is unavailable")
        raw_dir = self.raw / "journeys" / "sso" / lane
        if raw_dir.exists() or raw_dir.is_symlink():
            raise JourneyError(f"refusing pre-existing SSO candidate CLI profile path {raw_dir}")
        raw_dir.mkdir(parents=True, mode=0o777)
        raw_dir.chmod(0o777)
        container_name = f"{self.run_id}-sso-cli-{lane}"
        self.require_absent_container(container_name)
        self.pending_containers[container_name] = f"sso-cli-{lane}"
        result = self.docker_run(
            "run",
            "--pull=never",
            "--platform",
            self.platform,
            "--name",
            container_name,
            *self.container_labels(f"sso-cli-{lane}"),
            "--network",
            self.network_name,
            "--env-file",
            str(self.api_env_file),
            "-e",
            "GOCOVERDIR=/coverage",
            "-v",
            f"{raw_dir}:/coverage",
            "-v",
            f"{self.job_path}:/coverage-shutdown.job.yaml:ro",
            "--entrypoint",
            "/bin/caesium",
            self.image,
            *args,
            check=False,
            timeout=timeout,
        )
        info = self.assert_owned(container_name, f"sso-cli-{lane}")
        container_id = str(info.get("Id") or "")
        if not re.fullmatch(r"[0-9a-f]{64}", container_id) or info.get("Image") != self.image:
            raise JourneyError("SSO candidate CLI process did not use the immutable candidate image")
        state = info.get("State") or {}
        raw_files: dict[str, str] = {}
        for path in sorted(raw_dir.iterdir()):
            if not path.is_file() or path.is_symlink() or path.stat().st_size == 0 or not path.name.startswith(("covmeta.", "covcounters.")):
                raise JourneyError("SSO candidate CLI profile contains missing or foreign files")
            raw_files[path.name] = hashlib.sha256(path.read_bytes()).hexdigest()
        has_meta = any(name.startswith("covmeta.") for name in raw_files)
        has_counters = any(name.startswith("covcounters.") for name in raw_files)
        relative_raw = raw_dir.relative_to(self.raw).as_posix()
        record = {
            "schema_version": 1,
            "source": "cli",
            "lane": lane,
            "kind": "gocoverdir",
            "module": MODULE,
            "candidate_sha": self.sha,
            "image_id": self.image,
            "builder_image_id": self.builder,
            "build_context": self.build_context,
            "image_provenance": self.image_provenance,
            "verified": self.verified,
            "container_id": container_id,
            "complete": result.returncode == 0 and state.get("ExitCode") == 0 and state.get("OOMKilled") is False and state.get("Running") is False and info.get("RestartCount") == 0 and has_meta and has_counters,
            "running": state.get("Running"),
            "restart_count": info.get("RestartCount"),
            "missing": not (has_meta and has_counters),
            "killed": bool(state.get("OOMKilled")) or result.returncode >= 128,
            "exit_code": state.get("ExitCode"),
            "oom_killed": bool(state.get("OOMKilled")),
            "flush": "process-exit",
            "signal": None,
            "stop_rc": None,
            "flush_rc": None,
            "files": raw_files,
            "operation": args[0] if args else "",
            "raw_dir": relative_raw,
        }
        provenance_dir = self.raw / "journeys" / "sso" / "process-provenance"
        provenance_dir.mkdir(parents=True, exist_ok=True)
        provenance_path = provenance_dir / f"{lane}.json"
        if provenance_path.exists() or provenance_path.is_symlink():
            raise JourneyError("refusing pre-existing SSO CLI process provenance path")
        record["provenance_path"] = provenance_path.relative_to(self.raw).as_posix()
        provenance_path.write_text(json.dumps(record, indent=2) + "\n")
        self.cli_processes.append(record)
        self.ids.append((container_id, f"sso-cli-{lane}"))
        self.remove_owned(container_id, f"sso-cli-{lane}")
        if not record["complete"]:
            raise JourneyError(f"SSO candidate CLI {lane} failed or produced incomplete process-exit coverage")
        return result.stdout.strip()

    def start_shutdown_job(self, server_id: str) -> dict[str, Any]:
        alias = f"coverage-shutdown-{self.run_id}"
        command = self.shutdown_command()
        self.shutdown_control_dir = self.sso_artifacts / "shutdown-control"
        self.shutdown_release_path = self.shutdown_control_dir / "release"
        if self.shutdown_control_dir.exists() or self.shutdown_control_dir.is_symlink():
            raise JourneyError("refusing pre-existing shutdown control directory")
        self.shutdown_control_dir.mkdir(mode=0o755)
        self.shutdown_control_dir.chmod(0o755)
        self.shutdown_control_mount_source_sha256 = hashlib.sha256(
            str(self.shutdown_control_dir.resolve(strict=True)).encode()
        ).hexdigest()
        self.job_path = self.sso_artifacts / "shutdown.job.yaml"
        self.job_path.write_text(
            "\n".join(
                (
                    "apiVersion: v1",
                    "kind: Job",
                    "metadata:",
                    f"  alias: {alias}",
                    "trigger:",
                    "  type: http",
                    "  configuration:",
                    f"    path: {json.dumps('/hooks/coverage-shutdown-' + self.run_id)}",
                    "steps:",
                    "  - name: cancel_probe",
                    f"    image: {json.dumps(self.task_image_ref)}",
                    "    engine: docker",
                    "    mounts:",
                    "      - type: bind",
                    f"        source: {json.dumps(str(self.shutdown_control_dir))}",
                    "        target: /caesium-shutdown-control",
                    "        readOnly: true",
                    f"    command: [{json.dumps('sh')}, {json.dumps('-c')}, {json.dumps(command)}]",
                    "",
                )
            )
        )
        self.candidate_cli("shutdown-apply", "job", "apply", "--path", "/coverage-shutdown.job.yaml", "--server", self.sp_base)
        jobs = self.api_json(server_id, "/v1/jobs")
        if not isinstance(jobs, list):
            raise JourneyError("SSO job list response is not a JSON array")
        matches = [job for job in jobs if isinstance(job, dict) and job.get("alias") == alias]
        if len(matches) != 1:
            raise JourneyError("SSO public job list does not contain the unique shutdown job")
        job_id = matches[0].get("id")
        if not isinstance(job_id, str) or not UUID_RE.fullmatch(job_id):
            raise JourneyError("SSO shutdown job has no public UUID identity")
        run_id = self.candidate_cli("shutdown-start", "run", "start", "--job-id", job_id, "--server", self.sp_base, timeout=90)
        if not UUID_RE.fullmatch(run_id):
            raise JourneyError("SSO candidate CLI did not return the shutdown run UUID")
        path = f"/v1/jobs/{job_id}/runs/{run_id}"
        deadline = time.monotonic() + 90
        observed: dict[str, Any] | None = None
        while time.monotonic() < deadline:
            run_record = self.api_json(server_id, path)
            tasks = run_record.get("tasks") if isinstance(run_record, dict) else None
            if not isinstance(tasks, list) or len(tasks) != 1:
                time.sleep(0.25)
                continue
            task = tasks[0]
            if not isinstance(task, dict):
                raise JourneyError("SSO shutdown run contains an invalid task record")
            if run_record.get("status") == "running" and task.get("status") == "running":
                runtime_id = task.get("runtime_id")
                task_id = task.get("task_id")
                if not isinstance(runtime_id, str) or not re.fullmatch(r"[0-9a-f]{64}", runtime_id):
                    raise JourneyError("SSO shutdown task did not expose an immutable runtime ID")
                if not isinstance(task_id, str) or not UUID_RE.fullmatch(task_id):
                    raise JourneyError("SSO shutdown task public identity is invalid")
                if task.get("engine") != "docker" or task.get("image") != self.task_image_ref:
                    raise JourneyError("SSO shutdown task used a different backend or image")
                instances = self.api_json(server_id, f"/v1/jobs/{job_id}/runs/{run_id}/tasks/{task_id}/partitions?limit=2")
                rows = instances.get("partitions") if isinstance(instances, dict) else None
                if instances.get("total") != 1 or not isinstance(rows, list) or len(rows) != 1 or not isinstance(rows[0], dict):
                    raise JourneyError("SSO shutdown task does not have exactly one concrete public TaskRun instance")
                instance = rows[0]
                task_run_id = instance.get("task_run_id")
                attempt = instance.get("attempt")
                if (
                    not isinstance(task_run_id, str)
                    or not UUID_RE.fullmatch(task_run_id)
                    or not isinstance(attempt, int)
                    or isinstance(attempt, bool)
                    or attempt < 1
                    or instance.get("status") != "running"
                    or instance.get("runtime_id") != runtime_id
                    or instance.get("completed_at")
                ):
                    raise JourneyError("SSO public instance surface did not identify the exact running native TaskRun")
                observed = {
                    "job_id": job_id,
                    "job_alias": alias,
                    "run_id": run_id,
                    "task_id": task_id,
                    "task_run_id": task_run_id,
                    "run_detail_task_catalog_id": task_id,
                    "attempt_before_signal": attempt,
                    "runtime_id": runtime_id,
                    "task_image_id": self.task_image_id,
                    "task_image_ref": self.task_image_ref,
                    "server_generation": 1,
                    "initial_run_status": "running",
                    "initial_task_status": "running",
                    "hold_strategy": "read-only-host-release-marker",
                    "hold_command_sha256": hashlib.sha256(command.encode()).hexdigest(),
                    "resumption_mode": "explicit-public-http-trigger-existing-run",
                    "resumption_path": "/hooks/coverage-shutdown-" + self.run_id,
                    "control_mount_source_sha256": self.shutdown_control_mount_source_sha256,
                }
                break
            if run_record.get("status") in ("failed", "cancelled", "skipped", "succeeded"):
                raise JourneyError("SSO shutdown job became terminal before the server restart")
            time.sleep(0.25)
        if observed is None:
            raise JourneyError("SSO public run surface never observed the shutdown task running")
        self.shutdown_job = observed
        self.shutdown_job["native_initial_running"] = self.native_running()
        return observed

    def shutdown_command(self) -> str:
        return (f"while [ ! -e /caesium-shutdown-control/release ]; do sleep 0.1; done; "
                f"echo '##caesium::output {{\"shutdown\":\"resumed-{self.run_id}\"}}'")

    def shutdown_release_marker_absent(self) -> bool:
        path = self.shutdown_release_path
        directory = self.shutdown_control_dir
        if path is None or directory is None:
            raise JourneyError("shutdown release marker path is unavailable")
        if directory.is_symlink() or not directory.is_dir() or path.parent != directory:
            raise JourneyError("shutdown release marker directory identity is unsafe")
        if path.is_symlink() or path.exists():
            return False
        return True

    def release_shutdown_task(self) -> dict[str, Any]:
        proof = self.shutdown_job.get("resumption_runtime_running") if isinstance(self.shutdown_job, dict) else None
        webhook = self.shutdown_job.get("resumption_webhook") if isinstance(self.shutdown_job, dict) else None
        native_proof = proof.get("native") if isinstance(proof, dict) else None
        expected_command_sha = hashlib.sha256(self.shutdown_command().encode()).hexdigest()
        if (not isinstance(webhook, dict) or webhook.get("status") != 202
                or not isinstance(proof, dict) or proof.get("release_marker_absent") is not True
                or proof.get("running") is not True
                or proof.get("runtime_id") == self.shutdown_job.get("runtime_id")
                or proof.get("task_run_id") != self.shutdown_job.get("task_run_id")
                or proof.get("attempt") != self.shutdown_job.get("attempt_before_signal")
                or not isinstance(native_proof, dict)
                or native_proof.get("runtime_id") != proof.get("runtime_id")
                or native_proof.get("run_id") != self.shutdown_job.get("run_id")
                or native_proof.get("task_id") != self.shutdown_job.get("task_id")
                or native_proof.get("docker_image_id") != self.task_docker_image_id
                or native_proof.get("task_config_id") != self.task_image_id
                or native_proof.get("command_sha256") != expected_command_sha
                or not isinstance(self.shutdown_job.get("control_mount_source_sha256"), str)
                or not re.fullmatch(r"[0-9a-f]{64}", self.shutdown_job["control_mount_source_sha256"])
                or native_proof.get("control_mount_source_sha256") != self.shutdown_control_mount_source_sha256
                or native_proof.get("control_mount_source_sha256") != self.shutdown_job.get("control_mount_source_sha256")
                or native_proof.get("control_mount_destination") != "/caesium-shutdown-control"
                or native_proof.get("control_mount_read_only") is not True
                or native_proof.get("running") is not True):
            raise JourneyError("shutdown task cannot be released before the accepted webhook and replacement runtime proof")
        server_generations = getattr(self, "server_generations", [])
        if not server_generations:
            raise JourneyError("shutdown task release lacks generation-1 shutdown evidence")
        original_finish = self.timestamp(server_generations[0].get("finished_at"))
        task_started = self.timestamp(proof.get("task_started_at"))
        observed = self.timestamp(proof.get("observed_at"))
        if task_started <= original_finish or task_started > observed:
            raise JourneyError("shutdown task release proof has invalid replacement runtime timing")
        if not self.shutdown_release_marker_absent():
            raise JourneyError("shutdown task release marker already exists")
        assert self.shutdown_release_path is not None
        payload = b"release-v1\n"
        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
        try:
            descriptor = os.open(self.shutdown_release_path, flags, 0o644)
            with os.fdopen(descriptor, "wb") as stream:
                stream.write(payload)
                stream.flush()
                os.fsync(stream.fileno())
        except OSError as exc:
            raise JourneyError("shutdown task release marker could not be written") from exc
        released_at = datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z")
        return {
            "strategy": "read-only-host-release-marker",
            "written": True,
            "sha256": hashlib.sha256(payload).hexdigest(),
            "written_at": released_at,
        }

    def native_running(self, runtime_id: str | None = None) -> dict[str, Any]:
        if self.shutdown_job is None:
            raise JourneyError("shutdown task identity is unavailable")
        if (self.shutdown_control_dir is None or self.shutdown_control_dir.is_symlink()
                or not self.shutdown_control_dir.is_dir()):
            raise JourneyError("shutdown control mount source is unavailable")
        runtime = runtime_id or self.shutdown_job["runtime_id"]
        info = self.inspect_container(runtime)
        config, state = info.get("Config") or {}, info.get("State") or {}
        command = self.shutdown_command()
        expected_name = self.shutdown_job["task_id"] + "-" + self.shutdown_job["run_id"]
        mounts = info.get("Mounts")
        matching_mounts = [mount for mount in mounts if isinstance(mount, dict)
                           and mount.get("Destination") == "/caesium-shutdown-control"] if isinstance(mounts, list) else []
        expected_source = str(self.shutdown_control_dir.resolve(strict=True))
        source_digest = hashlib.sha256(expected_source.encode()).hexdigest()
        if (info.get("Id") != runtime or info.get("Image") != self.task_docker_image_id
                or config.get("Image") != self.task_image_ref
                or config.get("Cmd") != ["sh", "-c", command]
                or info.get("Name", "").lstrip("/") != expected_name
                or state.get("Running") is not True or state.get("Restarting") is not False
                or state.get("OOMKilled") is not False or info.get("RestartCount") != 0
                or len(matching_mounts) != 1
                or matching_mounts[0].get("Type") != "bind"
                or matching_mounts[0].get("Source") != expected_source
                or matching_mounts[0].get("RW") is not False
                or source_digest != self.shutdown_control_mount_source_sha256):
            raise JourneyError("shutdown native runtime is not the exact live owned fixture")
        return {"runtime_id": runtime, "run_id": self.shutdown_job["run_id"], "task_id": self.shutdown_job["task_id"],
                "docker_image_id": self.task_docker_image_id, "task_config_id": self.task_image_id,
                "command_sha256": hashlib.sha256(command.encode()).hexdigest(), "running": True,
                "control_mount_source_sha256": source_digest,
                "control_mount_destination": "/caesium-shutdown-control", "control_mount_read_only": True}

    @staticmethod
    def timestamp(value: Any) -> datetime.datetime:
        if not isinstance(value, str) or not value:
            raise JourneyError("process/terminal timestamp is unavailable")
        try:
            result = datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
        except ValueError as exc:
            raise JourneyError("process/terminal timestamp is invalid") from exc
        if result.tzinfo is None or result.year <= 1:
            raise JourneyError("process/terminal timestamp is not authoritative")
        return result

    def verify_shutdown_job_running(self, server_id: str) -> None:
        if self.shutdown_job is None:
            raise JourneyError("SSO shutdown run evidence is unavailable")
        record = self.api_json(server_id, f"/v1/jobs/{self.shutdown_job['job_id']}/runs/{self.shutdown_job['run_id']}")
        tasks = record.get("tasks") if isinstance(record, dict) else None
        if (
            record.get("id") != self.shutdown_job["run_id"]
            or record.get("status") != "running"
            or not isinstance(tasks, list)
            or len(tasks) != 1
            or not isinstance(tasks[0], dict)
            or tasks[0].get("task_id") != self.shutdown_job["task_id"]
            or tasks[0].get("runtime_id") != self.shutdown_job["runtime_id"]
            or tasks[0].get("status") != "running"
        ):
            raise JourneyError("SSO shutdown run/task stopped being the exact active runtime before SIGTERM")
        instances = self.api_json(
            server_id,
            f"/v1/jobs/{self.shutdown_job['job_id']}/runs/{self.shutdown_job['run_id']}/tasks/{self.shutdown_job['task_id']}/partitions?limit=2",
        )
        rows = instances.get("partitions") if isinstance(instances, dict) else None
        if (
            instances.get("total") != 1
            or not isinstance(rows, list)
            or len(rows) != 1
            or not isinstance(rows[0], dict)
            or rows[0].get("task_run_id") != self.shutdown_job["task_run_id"]
            or rows[0].get("attempt") != self.shutdown_job["attempt_before_signal"]
            or rows[0].get("runtime_id") != self.shutdown_job["runtime_id"]
            or rows[0].get("status") != "running"
            or rows[0].get("completed_at")
        ):
            raise JourneyError("SSO concrete TaskRun identity/status changed before SIGTERM")
        self.shutdown_job["native_before_signal"] = self.native_running()
        if not self.shutdown_release_marker_absent():
            raise JourneyError("shutdown task release marker existed before generation-1 SIGTERM")
        self.shutdown_job["release_marker_absent_before_signal"] = True

    def verify_runtime_absent(self, generation: int, runtime_id: str | None = None) -> None:
        if self.shutdown_job is None:
            raise JourneyError("SSO runtime cleanup evidence is unavailable")
        is_original_runtime = runtime_id is None
        runtime_id = runtime_id or self.shutdown_job["runtime_id"]
        result = self.docker_run(
            "container", "ls", "-a", "--no-trunc", "--filter", f"id={runtime_id}", "--format", "{{.ID}}", check=False
        )
        if result.returncode != 0:
            raise JourneyError("Docker could not verify shutdown runtime cleanup")
        found = [line for line in result.stdout.splitlines() if line]
        if found:
            if found != [runtime_id]:
                raise JourneyError("Docker runtime lookup returned an unexpected identity")
            info = self.inspect_container(runtime_id)
            config = info.get("Config") or {}
            command = config.get("Cmd") or []
            state = info.get("State") or {}
            owned_runtime = (
                info.get("Id") == runtime_id
                and info.get("Image") == self.task_docker_image_id
                and config.get("Image") == self.task_image_ref
                and info.get("Name") == "/" + self.shutdown_job["task_id"] + "-" + self.shutdown_job["run_id"]
                and command == ["sh", "-c", self.shutdown_command()]
            )
            if not owned_runtime:
                raise JourneyError("Docker runtime identity could not be proven as this shutdown task")
            if state.get("Running"):
                raise JourneyError("shutdown task container is still running after server termination")
            self.docker_run("container", "rm", "-f", runtime_id)
            raise JourneyError("shutdown task runtime stopped but was not removed by the server")
        if is_original_runtime:
            self.shutdown_job["native_runtime_removed"] = True
        else:
            self.shutdown_job.setdefault("resumption_runtime_removed_ids", []).append(runtime_id)
        self.shutdown_job.setdefault("native_runtime_absent_ids_by_generation", {})[str(generation)] = runtime_id
        if generation not in self.native_runtime_absent_generations:
            self.native_runtime_absent_generations.append(generation)

    def verify_shutdown_task_resumption_running(self, server_id: str) -> dict[str, Any]:
        """Hold the one-shot release until Gen2 is executing the original TaskRun."""
        if self.shutdown_job is None or self.shutdown_job.get("resumption_webhook", {}).get("status") != 202:
            raise JourneyError("explicit shutdown resumption webhook was not accepted")
        original_finish = self.timestamp(self.server_generations[0].get("finished_at"))
        path = f"/v1/jobs/{self.shutdown_job['job_id']}/runs/{self.shutdown_job['run_id']}"
        deadline = time.monotonic() + 90
        pending_replacement_runtime: str | None = None
        while time.monotonic() < deadline:
            if not self.shutdown_release_marker_absent():
                raise JourneyError("shutdown release marker appeared before replacement runtime observation")
            record = self.api_json(server_id, path)
            tasks = record.get("tasks") if isinstance(record, dict) else None
            if not isinstance(record, dict) or record.get("id") != self.shutdown_job["run_id"]:
                raise JourneyError("run identity changed before replacement runtime observation")
            if not isinstance(tasks, list) or len(tasks) != 1 or not isinstance(tasks[0], dict):
                raise JourneyError("replacement runtime observation lacks the exact task catalog row")
            task = tasks[0]
            instances = self.api_json(
                server_id,
                f"/v1/jobs/{self.shutdown_job['job_id']}/runs/{self.shutdown_job['run_id']}/tasks/{self.shutdown_job['task_id']}/partitions?limit=2",
            )
            rows = instances.get("partitions") if isinstance(instances, dict) else None
            if (not isinstance(instances, dict) or instances.get("total") != 1 or not isinstance(rows, list)
                    or len(rows) != 1 or not isinstance(rows[0], dict)):
                raise JourneyError("replacement runtime observation lacks the exact TaskRun row")
            instance = rows[0]
            if (task.get("task_id") != self.shutdown_job["task_id"]
                    or instance.get("task_run_id") != self.shutdown_job["task_run_id"]
                    or instance.get("attempt") != self.shutdown_job["attempt_before_signal"]):
                raise JourneyError("explicit resumption changed the original task or TaskRun identity")
            if record.get("status") == "failed" or task.get("status") == "failed" or instance.get("status") == "failed":
                raise JourneyError("explicitly resumed shutdown run/task failed before release")
            if record.get("status") == "running" and task.get("status") == "running" and instance.get("status") == "running":
                task_runtime = task.get("runtime_id")
                row_runtime = instance.get("runtime_id")
                if task_runtime != row_runtime:
                    coherent_transition = (
                        isinstance(task_runtime, str) and re.fullmatch(r"[0-9a-f]{64}", task_runtime)
                        and isinstance(row_runtime, str) and re.fullmatch(r"[0-9a-f]{64}", row_runtime)
                        and self.shutdown_job["runtime_id"] in (task_runtime, row_runtime)
                        and task_runtime != row_runtime
                        and not record.get("completed_at") and not task.get("completed_at")
                        and not instance.get("completed_at")
                        and record.get("error") in (None, "") and task.get("error") in (None, "")
                        and instance.get("error") in (None, "") and task.get("output") in (None, {})
                    )
                    if coherent_transition:
                        # Run detail and partitions are separate reads. Allow
                        # only a split snapshot straddling the known old→new
                        # runtime update; the next poll must be coherent.
                        replacement_runtime = row_runtime if task_runtime == self.shutdown_job["runtime_id"] else task_runtime
                        if (pending_replacement_runtime is not None
                                and pending_replacement_runtime != replacement_runtime):
                            raise JourneyError("split runtime snapshot changed its replacement identity")
                        pending_replacement_runtime = replacement_runtime
                        time.sleep(0.25)
                        continue
                    raise JourneyError("run detail and TaskRun runtime identities diverged outside the owned transition")
                runtime = instance.get("runtime_id")
                if (not isinstance(runtime, str) or not re.fullmatch(r"[0-9a-f]{64}", runtime)
                        or task.get("runtime_id") != runtime
                        or record.get("completed_at") or task.get("completed_at") or instance.get("completed_at")
                        or record.get("error") not in (None, "") or task.get("error") not in (None, "")
                        or instance.get("error") not in (None, "")
                        or task.get("output") not in (None, {})):
                    raise JourneyError("resumption state is not the exact active original TaskRun")
                if runtime == self.shutdown_job["runtime_id"]:
                    # The explicit trigger can be accepted before its local
                    # dispatcher has reset/restarted the retained row. Permit
                    # only that exact unchanged running identity while the
                    # marker remains absent, then keep polling for a new runtime.
                    if (record.get("id") != self.shutdown_job["run_id"]
                            or task.get("task_id") != self.shutdown_job["task_id"]
                            or instance.get("task_run_id") != self.shutdown_job["task_run_id"]
                            or instance.get("attempt") != self.shutdown_job["attempt_before_signal"]):
                        raise JourneyError("pre-dispatch retained runtime changed the original durable identity")
                    time.sleep(0.25)
                    continue
                if pending_replacement_runtime is not None and runtime != pending_replacement_runtime:
                    raise JourneyError("coherent run detail changed the split-snapshot replacement identity")
                pending_replacement_runtime = runtime
                started_at = self.timestamp(instance.get("started_at"))
                observed_at = datetime.datetime.now(datetime.timezone.utc)
                if started_at <= original_finish or started_at > observed_at:
                    raise JourneyError("replacement TaskRun start time is not after shutdown and before observation")
                native = self.native_running(runtime)
                if (native.get("runtime_id") != runtime or native.get("running") is not True
                        or native.get("run_id") != self.shutdown_job["run_id"]
                        or native.get("task_id") != self.shutdown_job["task_id"]
                        or native.get("docker_image_id") != self.task_docker_image_id
                        or native.get("task_config_id") != self.task_image_id
                        or native.get("command_sha256") != hashlib.sha256(self.shutdown_command().encode()).hexdigest()
                        or native.get("control_mount_source_sha256") != self.shutdown_job.get("control_mount_source_sha256")
                        or native.get("control_mount_destination") != "/caesium-shutdown-control"
                        or native.get("control_mount_read_only") is not True):
                    raise JourneyError("replacement native runtime is not positively observed running")
                if not self.shutdown_release_marker_absent():
                    raise JourneyError("shutdown release marker appeared during replacement runtime observation")
                receipt = {
                    "runtime_id": runtime,
                    "task_run_id": instance["task_run_id"],
                    "attempt": instance["attempt"],
                    "task_started_at": instance["started_at"],
                    "observed_at": observed_at.isoformat().replace("+00:00", "Z"),
                    "release_marker_absent": True,
                    "running": True,
                    "native": native,
                }
                self.shutdown_job["resumption_runtime_running"] = receipt
                return receipt
            if record.get("status") in ("succeeded", "cancelled", "skipped") or task.get("status") in ("succeeded", "cancelled", "skipped") or instance.get("status") in ("succeeded", "cancelled", "skipped"):
                raise JourneyError("resumed shutdown TaskRun terminated before the release marker")
            time.sleep(0.25)
        raise JourneyError("Gen2 did not start the original TaskRun on a new native runtime before timeout")

    def verify_shutdown_job_explicitly_resumed(self, server_id: str) -> None:
        if self.shutdown_job is None or 1 not in self.native_runtime_absent_generations:
            raise JourneyError("shutdown resumption was not preceded by generation-1 native runtime cleanup")
        self.verify_shutdown_job_retained(server_id)
        self.shutdown_job["resumption_webhook"] = self.fire_shutdown_webhook(server_id)
        resumption_running = self.verify_shutdown_task_resumption_running(server_id)
        self.shutdown_job["release_marker"] = self.release_shutdown_task()
        path = f"/v1/jobs/{self.shutdown_job['job_id']}/runs/{self.shutdown_job['run_id']}"
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            record = self.api_json(server_id, path)
            tasks = record.get("tasks") if isinstance(record, dict) else None
            if not isinstance(record, dict) or record.get("id") != self.shutdown_job["run_id"]:
                raise JourneyError("run identity changed across server restart")
            if not isinstance(tasks, list) or len(tasks) != 1 or not isinstance(tasks[0], dict):
                raise JourneyError("same-database restart did not expose the exact shutdown task")
            task = tasks[0]
            if task.get("task_id") != self.shutdown_job["task_id"]:
                raise JourneyError("task catalog identity changed across server restart")
            instances = self.api_json(
                server_id,
                f"/v1/jobs/{self.shutdown_job['job_id']}/runs/{self.shutdown_job['run_id']}/tasks/{self.shutdown_job['task_id']}/partitions?limit=2",
            )
            rows = instances.get("partitions") if isinstance(instances, dict) else None
            if (
                not isinstance(instances, dict)
                or
                instances.get("total") != 1
                or not isinstance(rows, list)
                or len(rows) != 1
                or not isinstance(rows[0], dict)
                or rows[0].get("task_run_id") != self.shutdown_job["task_run_id"]
                or not isinstance(rows[0].get("attempt"), int)
                or isinstance(rows[0].get("attempt"), bool)
                or rows[0].get("attempt") != self.shutdown_job["attempt_before_signal"]
            ):
                raise JourneyError("same-database restart did not preserve the concrete TaskRun identity and attempt")
            instance = rows[0]
            attempt = instance.get("attempt")
            if not isinstance(attempt, int) or isinstance(attempt, bool) or attempt != self.shutdown_job["attempt_before_signal"]:
                raise JourneyError("explicit local resumption changed the original TaskRun attempt")
            if record.get("status") == "failed" or task.get("status") == "failed" or instance.get("status") == "failed":
                raise JourneyError("explicitly resumed shutdown run/task failed instead of completing")
            if record.get("status") == "succeeded" and task.get("status") == "succeeded" and instance.get("status") == "succeeded":
                runtime_id = instance.get("runtime_id")
                if not isinstance(runtime_id, str) or not re.fullmatch(r"[0-9a-f]{64}", runtime_id) or runtime_id == self.shutdown_job["runtime_id"]:
                    raise JourneyError("explicit webhook resumption did not execute the same TaskRun with a new native runtime identity")
                if runtime_id != resumption_running.get("runtime_id"):
                    raise JourneyError("completed TaskRun runtime differs from the positively observed replacement runtime")
                if (task.get("runtime_id") != runtime_id or record.get("error") not in (None, "")
                        or task.get("error") not in (None, "") or instance.get("error") not in (None, "")
                        or not record.get("completed_at") or not instance.get("completed_at") or not instance.get("started_at")
                        or task.get("output") != {"shutdown": "resumed-" + self.run_id}):
                    raise JourneyError("explicitly resumed run/task lacks matching native identity or timestamps")
                original_finish = self.timestamp(self.server_generations[0].get("finished_at"))
                task_started = self.timestamp(instance["started_at"])
                run_completed = self.timestamp(record["completed_at"])
                task_completed = self.timestamp(instance["completed_at"])
                runtime_observed = self.timestamp(resumption_running["observed_at"])
                observed_task_started = self.timestamp(resumption_running["task_started_at"])
                marker_written = self.timestamp(self.shutdown_job["release_marker"]["written_at"])
                if (task_started <= original_finish or run_completed <= max(original_finish, marker_written)
                        or task_completed <= max(task_started, marker_written)
                        or task_started < observed_task_started
                        or marker_written <= runtime_observed):
                    raise JourneyError("same TaskRun did not execute and complete after generation-1 shutdown and explicit webhook resumption")
                self.verify_runtime_absent(2, runtime_id)
                self.shutdown_job.update(
                    final_run_status=record["status"],
                    final_task_status=task["status"],
                    final_task_run_id=instance["task_run_id"],
                    final_attempt=attempt,
                    final_task_run_status=instance["status"],
                    final_runtime_id=runtime_id,
                    run_completed_at=record["completed_at"],
                    task_started_at=instance["started_at"],
                    task_completed_at=instance["completed_at"],
                    release_marker_written_at=self.shutdown_job["release_marker"]["written_at"],
                    release_marker_after_running_observation=True,
                    resumption_runtime_absent_after_completion=True,
                    resumption_output=task["output"],
                    verified_after_generation=2,
                )
                return
            if record.get("status") in ("succeeded", "cancelled", "skipped") or task.get("status") in ("succeeded", "cancelled", "skipped") or instance.get("status") in ("succeeded", "cancelled", "skipped"):
                raise JourneyError("persisted shutdown run/task has an unexpected terminal status")
            time.sleep(0.25)
        raise JourneyError("explicit webhook resumption did not complete the original shutdown run/task")

    def verify_shutdown_job_retained(self, server_id: str) -> None:
        """Prove Gen2 retained the original active rows before explicit HTTP resumption."""
        if self.shutdown_job is None:
            raise JourneyError("shutdown job identity is unavailable after restart")
        record = self.api_json(
            server_id, f"/v1/jobs/{self.shutdown_job['job_id']}/runs/{self.shutdown_job['run_id']}")
        tasks = record.get("tasks") if isinstance(record, dict) else None
        if (not isinstance(record, dict) or record.get("id") != self.shutdown_job["run_id"]
                or record.get("status") != "running" or not isinstance(tasks, list) or len(tasks) != 1
                or not isinstance(tasks[0], dict) or tasks[0].get("task_id") != self.shutdown_job["task_id"]
                or tasks[0].get("status") != "running" or tasks[0].get("runtime_id") != self.shutdown_job["runtime_id"]):
            raise JourneyError("generation 2 did not retain the exact durable running run/task before resumption")
        instances = self.api_json(
            server_id,
            f"/v1/jobs/{self.shutdown_job['job_id']}/runs/{self.shutdown_job['run_id']}/tasks/{self.shutdown_job['task_id']}/partitions?limit=2",
        )
        rows = instances.get("partitions") if isinstance(instances, dict) else None
        if (not isinstance(instances, dict) or instances.get("total") != 1 or not isinstance(rows, list)
                or len(rows) != 1 or not isinstance(rows[0], dict)
                or rows[0].get("task_run_id") != self.shutdown_job["task_run_id"]
                or not isinstance(rows[0].get("attempt"), int)
                or isinstance(rows[0].get("attempt"), bool)
                or rows[0].get("attempt") != self.shutdown_job["attempt_before_signal"]
                or rows[0].get("status") != "running"
                or rows[0].get("runtime_id") != self.shutdown_job["runtime_id"]
                or rows[0].get("completed_at")):
            raise JourneyError("generation 2 did not retain the exact original active TaskRun row")
        if not self.shutdown_release_marker_absent():
            raise JourneyError("shutdown task release marker existed before the explicit generation-2 webhook")
        self.shutdown_job.update(
            retained_after_generation=2,
            generation2_automatic_takeover=False,
            release_marker_absent_before_webhook=True,
            retained_run_status=record["status"],
            retained_task_status=tasks[0]["status"],
            retained_task_run_id=rows[0]["task_run_id"],
            retained_attempt=rows[0]["attempt"],
            retained_runtime_id=rows[0]["runtime_id"],
        )

    def fire_shutdown_webhook(self, server_id: str) -> dict[str, Any]:
        if self.shutdown_job is None:
            raise JourneyError("shutdown job identity is unavailable for public resumption")
        expected_path = "/hooks/coverage-shutdown-" + self.run_id
        if self.shutdown_job.get("resumption_path") != expected_path:
            raise JourneyError("shutdown resumption path differs from the owned HTTP trigger")
        request = Request(
            self.host_api_base(server_id) + expected_path,
            data=b"{}",
            headers={"Content-Type": "application/json", "Accept": "application/json"},
            method="POST",
        )
        try:
            with urlopen(request, timeout=10) as response:
                if response.status != 202:
                    raise JourneyError("public shutdown resumption trigger was not accepted")
                payload = complete_response_bytes(response, 64 * 1024)
        except (HTTPError, URLError, TimeoutError) as exc:
            raise JourneyError("public shutdown resumption trigger request failed") from exc
        try:
            receipt = json.loads(payload)
        except (json.JSONDecodeError, UnicodeDecodeError) as exc:
            raise JourneyError("public shutdown resumption trigger returned invalid JSON") from exc
        if (
            not isinstance(receipt, dict)
            or receipt.get("path") != expected_path.removeprefix("/hooks/")
            or receipt.get("http_triggers_accepted") != 1
            or receipt.get("http_runs_started") != 1
            or not isinstance(receipt.get("receipt_id"), str)
            or not UUID_RE.fullmatch(receipt["receipt_id"])
        ):
            raise JourneyError("public webhook receipt did not admit exactly one existing-run resumption")
        return {"status": 202, "path": expected_path, "http_triggers_accepted": 1,
                "http_runs_started": 1, "receipt_id": receipt["receipt_id"]}

    def await_server(self, container_id: str) -> None:
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            try:
                health = json.loads(self.candidate_get(f"{self.sp_base}/health", network=self.network_name))
                if health.get("status") == "healthy" or health.get("healthy") is True:
                    break
            except (JourneyError, json.JSONDecodeError):
                pass
            info = self.inspect_container(container_id)
            if not (info.get("State") or {}).get("Running"):
                raise JourneyError("SSO server exited before becoming healthy")
            time.sleep(1)
        else:
            raise JourneyError("SSO server did not become healthy")

        status = json.loads(self.candidate_get(f"{self.sp_base}/auth/status", network=self.network_name))
        if not isinstance(status, dict):
            raise JourneyError("actual /auth/status returned an invalid record")
        methods = status.get("methods") or []
        enabled = {item.get("type"): item for item in methods if isinstance(item, dict)}
        if not status.get("enabled") or not {"oidc", "saml"}.issubset(enabled):
            raise JourneyError("actual /auth/status does not expose both OIDC and SAML")
        expected = {
            "oidc": "/auth/sso/oidc/login",
            "saml": "/auth/sso/saml/login",
        }
        for kind, path in expected.items():
            if enabled[kind].get("loginUrl") != path:
                raise JourneyError(f"actual /auth/status has an unexpected {kind} login URL")
        self.auth_status_file.write_text(json.dumps(status, indent=2) + "\n")

    def stop_server(self, container_id: str, generation: int) -> dict[str, Any]:
        lane = f"sso-server-g{generation}"
        info = self.assert_owned(container_id, lane)
        if info.get("Image") != self.image:
            raise JourneyError(f"refusing to stop foreign image in generation {generation}")
        flush_rc = self.docker_run(
            "container", "kill", "--signal=SIGUSR2", container_id, check=False
        ).returncode
        time.sleep(1)
        stop = self.docker_run("container", "stop", "-t", "60", container_id, check=False)
        final = self.inspect_container(container_id)
        state = final.get("State") or {}
        raw_dir = self.server_raw_root / f"server-g{generation}"
        files: dict[str, str] = {}
        for path in sorted(raw_dir.iterdir()):
            if not path.is_file() or path.is_symlink() or path.stat().st_size == 0 or not path.name.startswith(("covmeta.", "covcounters.")):
                raise JourneyError(f"SSO server generation {generation} profile contains missing or foreign files")
            files[path.name] = hashlib.sha256(path.read_bytes()).hexdigest()
        has_meta = any(name.startswith("covmeta.") for name in files)
        has_counters = any(name.startswith("covcounters.") for name in files)
        record = {
            "generation": generation,
            "container_id": container_id,
            "image_id": final.get("Image"),
            "complete": has_meta and has_counters,
            "missing": not (has_meta and has_counters),
            "killed": bool(state.get("OOMKilled")) or state.get("ExitCode") == 137,
            "database_mount_sha256": hashlib.sha256(str(self.database_dir.resolve()).encode()).hexdigest(),
            "server_environment_sha256": self.server_environment_sha256,
            "raw_dir": raw_dir.relative_to(self.raw).as_posix(),
            "files": files,
            "flush_rc": flush_rc,
            "exit_code": state.get("ExitCode"),
            "finished_at": state.get("FinishedAt"),
            "running": state.get("Running"),
            "restart_count": final.get("RestartCount"),
            "stop_rc": stop.returncode,
            "signal": "SIGTERM",
            "oom_killed": bool(state.get("OOMKilled")),
        }
        self.timestamp(record["finished_at"])
        if final.get("RestartCount") != 0 or state.get("Running") is not False:
            raise JourneyError("SSO server process did not finish exactly once")
        if (
            flush_rc != 0
            or stop.returncode != 0
            or state.get("ExitCode") != 0
            or state.get("OOMKilled") is not False
            or final.get("Image") != self.image
            or not record["complete"]
        ):
            raise JourneyError(f"SSO server generation {generation} did not stop cleanly")
        provenance_dir = self.raw / "journeys" / "sso" / "process-provenance"
        provenance_dir.mkdir(parents=True, exist_ok=True)
        provenance_path = provenance_dir / f"server-generation-{generation}.json"
        if provenance_path.exists() or provenance_path.is_symlink():
            raise JourneyError("refusing pre-existing SSO server process provenance path")
        record.update(
            schema_version=1,
            source="server",
            lane=f"sso-server-g{generation}",
            kind="gocoverdir",
            module=MODULE,
            candidate_sha=self.sha,
            builder_image_id=self.builder,
            build_context=self.build_context,
            image_provenance=self.image_provenance,
            verified=self.verified,
            oom_killed=bool(state.get("OOMKilled")),
            flush="sigusr2",
            signal="SIGTERM",
            stop_rc=stop.returncode,
            provenance_path=provenance_path.relative_to(self.raw).as_posix(),
        )
        provenance_path.write_text(json.dumps(record, indent=2) + "\n")
        self.server_generations.append(record)
        self.remove_owned(container_id, lane)
        return record

    def start_journey_process(self) -> subprocess.Popen[bytes]:
        self.require_absent_container(self.journey_name)
        self.pending_containers[self.journey_name] = "sso-journey"
        args = [
            self.docker,
            "create",
            "--pull=never",
            "-i",
            "--platform",
            self.platform,
            "--name",
            self.journey_name,
            *self.container_labels("sso-journey"),
            "--network",
            self.network_name,
            "-v",
            f"{self.binary_path}:/fixture/sso-idp:ro",
            "--entrypoint",
            "/fixture/sso-idp",
            self.builder,
            "journey",
            "--base",
            self.sp_base,
            "--idp",
            self.issuer,
            "--candidate-sha",
            self.sha,
            "--timeout",
            "4m",
        ]
        created = self.docker_run(*args[1:], check=False, timeout=60)
        if created.returncode != 0 or not created.stdout.strip():
            raise JourneyError("could not create SSO journey helper container")
        self.helper_container_id = created.stdout.strip()
        self.ids.append((self.helper_container_id, "sso-journey"))
        info = self.assert_owned(self.helper_container_id, "sso-journey")
        if info.get("Image") != self.builder:
            raise JourneyError("SSO journey helper did not use the pinned builder image")
        try:
            self.helper_process = subprocess.Popen(
                [self.docker, "start", "-ai", self.helper_container_id],
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                env=self.docker_env,
                bufsize=0,
                start_new_session=True,
            )
        except OSError as exc:
            raise JourneyError("could not attach to SSO journey helper") from exc
        return self.helper_process

    def read_protocol_event(
        self,
        process: subprocess.Popen[bytes],
        buffer: bytearray,
        deadline: float,
    ) -> tuple[dict[str, Any], bytearray]:
        fd = process.stdout.fileno() if process.stdout else -1
        while b"\n" not in buffer:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise JourneyError("SSO journey helper protocol timed out")
            ready, _, _ = select.select([fd], [], [], min(remaining, 0.5))
            if not ready:
                if process.poll() is not None:
                    raise JourneyError("SSO journey helper exited before its next protocol event")
                continue
            chunk = os.read(fd, 4096)
            if not chunk:
                raise JourneyError("SSO journey helper closed its protocol stream")
            buffer.extend(chunk)
            if len(buffer) > MAX_PROTOCOL_LINE:
                raise JourneyError("SSO journey helper exceeded the protocol line limit")
        line, _, rest = buffer.partition(b"\n")
        if not line or len(line) > MAX_PROTOCOL_LINE:
            raise JourneyError("SSO journey helper emitted an empty or oversized protocol line")
        try:
            value = json.loads(line)
        except json.JSONDecodeError as exc:
            raise JourneyError("SSO journey helper emitted a non-JSON protocol line") from exc
        if not isinstance(value, dict):
            raise JourneyError("SSO journey helper event must be a JSON object")
        return value, bytearray(rest)

    @staticmethod
    def _safe_evidence(value: Any) -> dict[str, Any]:
        allowed = {"name", "status_code", "redirect", "role", "row_counts"}
        if not isinstance(value, dict) or "name" not in value or not set(value).issubset(allowed):
            raise JourneyError("SSO evidence has missing or unexpected fields")
        name = value["name"]
        if not isinstance(name, str) or not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}", name):
            raise JourneyError("SSO evidence name is invalid")

        safe: dict[str, Any] = {"name": name}
        if "status_code" in value:
            status_code = value["status_code"]
            if not isinstance(status_code, int) or isinstance(status_code, bool) or not 100 <= status_code <= 599:
                raise JourneyError("SSO evidence status code is invalid")
            safe["status_code"] = status_code
        if "redirect" in value:
            redirect = value["redirect"]
            if not isinstance(redirect, str) or not redirect or len(redirect) > 4096 or KEY_RE.search(redirect):
                raise JourneyError("SSO evidence redirect is invalid")
            if redirect not in EXPECTED_REDIRECTS:
                raise JourneyError("SSO evidence redirect is outside the fixture's return-destination catalog")
            parsed = urlsplit(redirect)
            if parsed.scheme not in ("", "http", "https") or parsed.username or parsed.password:
                raise JourneyError("SSO evidence redirect is not a safe URL")
            sensitive = {"state", "code", "token", "access_token", "id_token", "nonce", "assertion", "samlresponse", "relaystate", "cookie", "secret", "csrf"}
            if any(key.lower() in sensitive for key, _ in parse_qsl(parsed.query, keep_blank_values=True)):
                raise JourneyError("SSO evidence redirect contains protocol state")
            safe["redirect"] = redirect
        if "role" in value:
            role = value["role"]
            if role not in ("admin", "viewer"):
                raise JourneyError("SSO evidence role is not an expected application role")
            safe["role"] = role
        if "row_counts" in value:
            counts = value["row_counts"]
            expected = {"oidc_users", "saml_users", "oidc_sessions", "saml_sessions", "assertions"}
            if not isinstance(counts, dict) or set(counts) != expected or any(
                not isinstance(count, int) or isinstance(count, bool) or count < 0
                for count in counts.values()
            ):
                raise JourneyError("SSO evidence row counts are incomplete or invalid")
            safe["row_counts"] = {name: counts[name] for name in sorted(expected)}
        return safe

    def safe_check_event(self, event: dict[str, Any]) -> dict[str, Any]:
        if set(event) != {"event", "name", "status", "evidence"}:
            raise JourneyError("SSO progress event contains unexpected fields")
        name = event.get("name")
        if event.get("event") != "check" or event.get("status") != "pass":
            raise JourneyError("SSO journey reported a failed or unknown progress event")
        if not isinstance(name, str) or not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}", name):
            raise JourneyError("SSO progress event name is invalid or sensitive")
        evidence = self._safe_evidence(event.get("evidence"))
        if evidence["name"] != name:
            raise JourneyError("SSO progress event and evidence names differ")
        safe = {"event": "check", "name": name, "status": "pass", "evidence": evidence}
        self.protocol_events.append(safe)
        return safe

    def safe_restart_event(self, event: dict[str, Any]) -> dict[str, str]:
        if set(event) != {"event", "phase", "assertion_digest"}:
            raise JourneyError("SSO restart barrier contains unexpected fields")
        if event.get("event") != "restart_required" or event.get("phase") != "saml-persisted":
            raise JourneyError("SSO journey did not request the persistent SAML restart barrier")
        digest = event.get("assertion_digest")
        if not isinstance(digest, str) or not DIGEST_RE.fullmatch(digest):
            raise JourneyError("SSO restart barrier assertion digest is invalid")
        safe = {"event": "restart_required", "phase": "saml-persisted", "assertion_digest": digest}
        self.protocol_events.append(safe)
        return safe

    @staticmethod
    def expected_evidence_records() -> list[dict[str, Any]]:
        counts = {name: 0 for name in COUNT_KEYS}
        records: list[dict[str, Any]] = []

        def row_counts() -> dict[str, int]:
            return dict(counts)

        def successful(name: str, redirect: str) -> None:
            records.append({
                "name": name,
                "status_code": 302,
                "redirect": redirect,
                "role": "admin",
                "row_counts": row_counts(),
            })

        def refused(name: str) -> None:
            records.append({"name": name, "status_code": 401, "row_counts": row_counts()})

        records.append({"name": "readiness"})
        counts["oidc_users"] = 1
        counts["oidc_sessions"] = 1
        successful("oidc-success", "/runs?status=mine#coverage")
        refused("oidc-code-replay")
        for name in (
            "oidc-state-mismatch",
            "oidc-missing-cookie",
            "oidc-tampered-cookie",
            "oidc-provider-error",
            "oidc-bad_nonce",
            "oidc-bad_audience",
        ):
            refused(name)
        counts["oidc_sessions"] += 1
        successful("oidc-fresh-positive", "/")

        counts["saml_users"] = 1
        counts["saml_sessions"] = 1
        counts["assertions"] = 1
        successful("saml-signed-success", "/runs?status=failed#coverage")
        refused("saml-immediate-replay")
        records.append({"name": "saml-restart-barrier"})
        refused("saml-persistent-replay")
        counts["saml_sessions"] += 1
        counts["assertions"] += 1
        successful("saml-fresh-positive", "/")
        for name in (
            "saml-tampered",
            "saml-bad_audience",
            "saml-expired",
            "saml-relay-mismatch",
            "saml-tampered-cookie",
            "saml-missing-cookie",
            "saml-missing-response",
        ):
            refused(name)

        for index, redirect in enumerate((
            "/runs?status=mine#absolute",
            "/jobs%20escaped?x=%2F#frag%20ment",
            "/",
            "/",
            "/",
        )):
            counts["oidc_sessions"] += 1
            successful(f"oidc-return-target-{index}", redirect)
        for index, redirect in enumerate((
            "/runs?status=mine#absolute",
            "/jobs%20escaped?x=%2F#frag%20ment",
            "/",
            "/",
            "/",
        )):
            counts["saml_sessions"] += 1
            counts["assertions"] += 1
            successful(f"saml-return-target-{index}", redirect)
        return records

    @classmethod
    def safe_final_event(cls, event: dict[str, Any], candidate_sha: str) -> dict[str, Any]:
        allowed = {"event", "status", "candidate_sha", "checks", "counts", "evidence"}
        if set(event) != allowed:
            raise JourneyError("SSO final verdict contains missing or unexpected fields")
        if event.get("event") != "complete" or event.get("status") != "pass":
            raise JourneyError("SSO helper did not report a passing final verdict")
        if event.get("candidate_sha") != candidate_sha:
            raise JourneyError("SSO helper verdict belongs to a different candidate")
        checks = event.get("checks")
        counts = event.get("counts")
        if not isinstance(checks, list) or not checks or not all(isinstance(v, str) for v in checks):
            raise JourneyError("SSO helper checks must be a nonempty list of sanitized labels")
        if not isinstance(counts, dict) or set(counts) != set(COUNT_KEYS) or any(
            not isinstance(key, str) or not isinstance(value, int) or isinstance(value, bool) or value < 0
            for key, value in counts.items()
        ):
            raise JourneyError("SSO helper counts are incomplete or invalid")
        if tuple(checks) != EXPECTED_CHECKS:
            raise JourneyError("SSO helper did not pass the complete ordered check catalog")
        if counts != EXPECTED_COUNTS:
            raise JourneyError("SSO helper final identity/session/assertion counts are unexpected")
        evidence = event.get("evidence")
        if not isinstance(evidence, list) or not evidence:
            raise JourneyError("SSO final verdict must include sanitized evidence records")
        safe_evidence = [cls._safe_evidence(item) for item in evidence]
        if [item["name"] for item in safe_evidence] != list(EXPECTED_CHECKS):
            raise JourneyError("SSO final evidence does not match the complete ordered check catalog")
        if safe_evidence != cls.expected_evidence_records():
            raise JourneyError("SSO evidence does not match the expected HTTP outcomes and row counts")
        return {
            "event": "complete",
            "status": "pass",
            "candidate_sha": candidate_sha,
            "checks": checks,
            "counts": {name: counts[name] for name in COUNT_KEYS},
            "evidence": safe_evidence,
        }

    def send_restarted(self, process: subprocess.Popen[bytes]) -> None:
        if process.stdin is None:
            raise JourneyError("SSO journey helper stdin is unavailable")
        process.stdin.write(b'{"event":"server_restarted","generation":2}\n')
        process.stdin.flush()

    def run_journey(self) -> dict[str, Any]:
        process = self.start_journey_process()
        buffer = bytearray()
        journey_deadline = time.monotonic() + 240
        event, buffer = self.read_protocol_event(process, buffer, journey_deadline)
        while event.get("event") == "check":
            self.safe_check_event(event)
            event, buffer = self.read_protocol_event(process, buffer, journey_deadline)
        self.safe_restart_event(event)
        barrier_started = time.monotonic()
        first_id = next(cid for cid, lane in self.ids if lane == "sso-server-g1")
        self.verify_shutdown_job_running(first_id)
        self.stop_server(first_id, 1)
        self.verify_runtime_absent(1)
        second_id = self.start_server(2)
        if first_id == second_id:
            raise JourneyError("SSO server restart reused the original container identity")
        self.verify_shutdown_job_explicitly_resumed(second_id)
        replay_restart_elapsed = time.monotonic() - barrier_started
        if replay_restart_elapsed >= 55:
            raise JourneyError("SSO server shutdown/restart exceeded the existing replay-age budget")
        assert self.shutdown_job is not None
        self.shutdown_job["native_runtime_absent_after_generation"] = 2
        self.shutdown_job["native_runtime_absent_generations"] = list(self.native_runtime_absent_generations)
        self.shutdown_job["replay_restart_elapsed_seconds"] = round(replay_restart_elapsed, 3)
        self.send_restarted(process)

        event, buffer = self.read_protocol_event(process, buffer, journey_deadline)
        while event.get("event") == "check":
            self.safe_check_event(event)
            event, buffer = self.read_protocol_event(process, buffer, journey_deadline)
        final = self.safe_final_event(event, self.sha)
        if buffer.strip():
            raise JourneyError("SSO journey helper emitted trailing protocol data")
        try:
            rc = process.wait(timeout=20)
        except subprocess.TimeoutExpired as exc:
            raise JourneyError("SSO journey helper did not exit after its final verdict") from exc
        if rc != 0:
            raise JourneyError(f"SSO journey helper exited with status {rc}")
        if process.stdout:
            extra = process.stdout.read()
            if extra.strip():
                raise JourneyError("SSO journey helper emitted data after its final verdict")
        self.protocol_events.append(final)
        progress = [item["name"] for item in self.protocol_events if item.get("event") == "check"]
        if tuple(progress) != EXPECTED_CHECKS:
            raise JourneyError("SSO progress stream does not contain the complete ordered check catalog")
        barriers = [item for item in self.protocol_events if item.get("event") == "restart_required"]
        if len(barriers) != 1:
            raise JourneyError("SSO progress stream did not contain exactly one persistent-replay restart barrier")
        progress_evidence = [item["evidence"] for item in self.protocol_events if item.get("event") == "check"]
        if progress_evidence != final["evidence"]:
            raise JourneyError("SSO progress evidence differs from the final sanitized evidence")
        self.stop_server(second_id, 2)
        return final

    def write_provenance(self, final: dict[str, Any]) -> None:
        binary_digest = hashlib.sha256(self.binary_path.read_bytes()).hexdigest()
        metadata_digest = hashlib.sha256(self.metadata_path.read_bytes()).hexdigest()
        record = {
            "schema_version": 1,
            "source": "integration-journey",
            "lane": "sso",
            "collector_run_id": self.run_id,
            "kind": "persistent-sso-restart-gocoverdir",
            "module": MODULE,
            "candidate_sha": self.sha,
            "image_id": self.image,
            "builder_image_id": self.builder,
            "build_context": self.build_context,
            "image_provenance": self.image_provenance,
            "verified": self.verified,
            "complete": True,
            "missing": False,
            "killed": False,
            "backend_inputs_sha256": self.backend_inputs_sha256,
            "task_image_id": self.task_image_id,
            "task_docker_image_id": self.task_docker_image_id,
            "task_image_ref": self.task_image_ref,
            "cleanup_errors": [],
            "fixture_binary_sha256": binary_digest,
            "public_idp_metadata_sha256": metadata_digest,
            "idp_ready": json.loads(self.idp_ready_file.read_text()),
            "auth_status": json.loads(self.auth_status_file.read_text()),
            "server_generations": self.server_generations,
            "shutdown_cancellation": self.shutdown_job,
            "cli_processes": self.cli_processes,
            "helper_events": self.protocol_events,
            "verdict": final,
            "raw": {
                "server_generations": [item["raw_dir"] for item in self.server_generations],
                "cli_processes": [item["raw_dir"] for item in self.cli_processes],
            },
            "collection": "oidc-saml-http-persistent-replay-restart",
        }
        (self.sso_artifacts / "provenance.json").write_text(json.dumps(record, indent=2) + "\n")
        (self.raw / "journeys" / "sso" / "provenance.json").write_text(
            json.dumps(record, indent=2) + "\n"
        )
        with self.protocol_log.open("w") as stream:
            for event in self.protocol_events:
                stream.write(json.dumps(event, sort_keys=True) + "\n")

    def cleanup(self) -> None:
        if self.cleanup_complete:
            return
        errors = []
        process = self.helper_process
        if process is not None and process.poll() is None:
            try:
                os.killpg(process.pid, signal.SIGTERM)
                process.wait(timeout=10)
            except (OSError, subprocess.TimeoutExpired):
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait(timeout=5)
                except (OSError, subprocess.TimeoutExpired):
                    errors.append("helper process join failed")
        for name, lane in list(self.pending_containers.items()):
            try:
                result = self.docker_run("container", "inspect", name, check=False)
                if missing_object(result, "container", name):
                    self.pending_containers.pop(name, None)
                    continue
                info = self.assert_owned(name, lane)
                identity = info.get("Id")
                if (identity, lane) not in self.ids:
                    self.ids.append((identity, lane))
            except (JourneyError, ValueError):
                errors.append("pending owned allocation inventory failed")
        for container_id, lane in list(reversed(self.ids)):
            try:
                self.remove_owned(container_id, lane)
            except (JourneyError, ValueError):
                errors.append("owned container removal unproved")
        if self.shutdown_job is not None:
            try:
                runtimes = [self.shutdown_job["runtime_id"]]
                recovered = self.shutdown_job.get("final_runtime_id")
                if isinstance(recovered, str) and recovered not in runtimes:
                    runtimes.append(recovered)
                absences = []
                for runtime in runtimes:
                    listed = self.docker_run("container", "ls", "-a", "--no-trunc", "--filter", "id=" + runtime, "--format", "{{.ID}}", check=False)
                    if listed.returncode != 0:
                        raise JourneyError("native task inventory failed")
                    if listed.stdout.strip():
                        if listed.stdout.strip() != runtime:
                            raise JourneyError("native task inventory ambiguous")
                        info = self.inspect_container(runtime)
                        config = info.get("Config") or {}
                        expected_name = self.shutdown_job["task_id"] + "-" + self.shutdown_job["run_id"]
                        if (info.get("Id") != runtime or info.get("Image") != self.task_docker_image_id
                                or config.get("Image") != self.task_image_ref
                                or info.get("Name", "").lstrip("/") != expected_name
                                or config.get("Cmd") != ["sh", "-c", self.shutdown_command()]):
                            raise JourneyError("native task cleanup ownership unproved")
                        self.docker_run("container", "rm", "-f", runtime)
                        missing = self.docker_run("container", "inspect", runtime, check=False)
                        if not missing_object(missing, "container", runtime):
                            raise JourneyError("native task cleanup unproved")
                    absences.append({"runtime_id": runtime, "absent": True})
                self.shutdown_job["native_runtime_cleanup_absences"] = absences
            except (JourneyError, ValueError):
                errors.append("owned task cleanup unproved")
        if self.network_attempted:
            try:
                guarded_resource(self.docker_run, "network", self.network_id or self.network_name, "remove", self.sha, self.run_id)
                self.network_id = ""
                self.network_attempted = False
            except (JourneyError, ValueError):
                errors.append("owned network cleanup unproved")
        for path in list(self.secret_files):
            try:
                path.unlink(missing_ok=True)
                self.secret_files.remove(path)
            except OSError:
                errors.append("private secret removal failed")
        self.bootstrap_key = ""
        for attribute in ("secret_dir", "database_dir"):
            directory = getattr(self, attribute)
            if directory is not None:
                try:
                    if directory.exists():
                        shutil.rmtree(directory)
                    setattr(self, attribute, None)
                except OSError:
                    errors.append("private temporary directory cleanup failed")
        self.cleanup_errors = errors
        if errors:
            self.sso_artifacts.mkdir(parents=True, exist_ok=True)
            (self.sso_artifacts / "cleanup-incomplete.json").write_text(json.dumps({"complete": False, "cleanup_errors": errors,
                "retained_container_ids": [identity for identity, _ in self.ids], "network_id": self.network_id,
                "pending_names": list(self.pending_containers), "pending_network": self.network_name if self.network_attempted else None}) + "\n")
            raise JourneyError("SSO cleanup incomplete; operator reconciliation required")
        self.cleanup_complete = True

    def execute(self) -> dict[str, Any]:
        for path in (
            self.server_raw_root / "server-g1",
            self.server_raw_root / "server-g2",
            self.server_raw_root / "process-provenance",
        ):
            if path.exists() or path.is_symlink():
                raise JourneyError(f"refusing pre-existing SSO artifact path {path}")
        self.artifacts.mkdir(parents=True, exist_ok=True)
        self.sso_artifacts = pathlib.Path(tempfile.mkdtemp(prefix=f"sso-{self.run_id}-", dir=self.artifacts))
        self.sso_artifacts.chmod(0o700)
        self.fixture_dir = self.sso_artifacts / "fixture"
        self.secret_dir = pathlib.Path(tempfile.mkdtemp(prefix=f"caesium-sso-secrets-{self.run_id}-"))
        self.secret_dir.chmod(0o700)
        self.idp_env_file = self.secret_dir / "idp.env"
        self.server_env_file = self.secret_dir / "server.env"
        self.protocol_log = self.sso_artifacts / "events.jsonl"
        self.auth_status_file = self.sso_artifacts / "auth-status.json"
        self.idp_ready_file = self.sso_artifacts / "idp-ready.json"
        self.binary_path = self.fixture_dir / "sso-idp"
        self.metadata_path = self.fixture_dir / "idp.xml"
        self.job_path = self.sso_artifacts / "shutdown.job.yaml"
        self.database_dir = pathlib.Path(tempfile.mkdtemp(prefix=f"caesium-sso-db-{self.run_id}-"))
        self.database_dir.chmod(0o777)
        self.load_backend_prerequisites()
        self.create_network()
        self.build_fixture()
        self.write_secrets()
        self.start_idp()
        self.await_idp()
        first_id = self.start_server(1)
        self.bootstrap_admin_key(first_id)
        self.start_shutdown_job(first_id)
        final = self.run_journey()
        return final


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    modes = parser.add_subparsers(dest="mode", required=True)
    sso = modes.add_parser("sso")
    for name in ("root", "artifacts", "raw", "coverage-image", "builder-image", "platform", "candidate-sha", "run-id", "build-context", "image-provenance"):
        sso.add_argument("--" + name, required=True)
    sso.add_argument("--docker-socket", required=True)
    sso.add_argument("--socket-gid", required=True, type=int)
    sso.add_argument("--backend-inputs", required=True)
    sso.add_argument("--backend-inputs-sha256", required=True)
    sso.add_argument("--verified", choices=("true", "false"), required=True)
    sso.add_argument("--container-cli", default="docker")

    backends = modes.add_parser("validate-backends")
    for name in ("context", "context-sha256", "inputs", "inputs-sha256", "output", "final-manifest", "cli-list", "server-list", "candidate-sha", "image-id", "builder-image-id", "platform", "run-id", "build-context"):
        backends.add_argument("--" + name, required=True)

    invalidate = modes.add_parser("invalidate")
    for name in ("profiles", "raw", "artifacts"):
        invalidate.add_argument("--" + name, required=True)
    raw = modes.add_parser("validate-raw")
    raw.add_argument("--directory", required=True)
    connector = modes.add_parser("connector")
    for name in ("name", "owner", "run-id", "image", "audit"):
        connector.add_argument("--" + name, required=True)
    resource = modes.add_parser("resource")
    for name in ("kind", "name", "action", "owner", "run-id"):
        resource.add_argument("--" + name, required=True)
    resource.add_argument("--lane", default="")
    resource.add_argument("--image", default="")
    inputs = modes.add_parser("validate-inputs")
    inputs.add_argument("--inputs", required=True)
    inputs.add_argument("--sha256", required=True)
    args = parser.parse_args()
    if args.mode in ("resource", "connector", "validate-raw", "validate-inputs", "invalidate"):
        return args
    if args.mode == "sso":
        if not re.fullmatch(r"[0-9a-f]{40}", args.candidate_sha):
            parser.error("candidate SHA must be a full lowercase Git SHA")
        if not re.fullmatch(r"[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?", args.run_id):
            parser.error("run ID must be a lowercase Docker-safe identifier")
        for name in ("coverage_image", "builder_image"):
            if not re.fullmatch(r"sha256:[0-9a-f]{64}", getattr(args, name)):
                parser.error(f"{name.replace('_', ' ')} must be an immutable sha256 image ID")
        try:
            context = json.loads(args.build_context)
        except json.JSONDecodeError as exc:
            parser.error(f"build context must be JSON: {exc}")
        if not isinstance(context, dict):
            parser.error("build context must be a JSON object")
        if args.image_provenance != "built-by-this-run" or args.verified != "true":
            parser.error("SSO live journey requires a verified image built by this collection")
        if not pathlib.Path(args.docker_socket).is_absolute():
            parser.error("SSO Docker socket path must be absolute")
        if args.socket_gid < 0:
            parser.error("SSO Docker socket group ID must be nonnegative")
        if args.platform not in ("linux/amd64", "linux/arm64"):
            parser.error("SSO journey requires a supported Linux platform")
        if args.container_cli != "docker":
            parser.error("SSO journey requires the Docker CLI bound to the staged socket")
        if not pathlib.Path(args.backend_inputs).is_absolute():
            parser.error("SSO backend prerequisite path must be absolute")
        if not DIGEST_RE.fullmatch(args.backend_inputs_sha256):
            parser.error("SSO backend prerequisite digest must be a lowercase SHA-256 value")
    else:
        for name in ("candidate_sha",):
            if not re.fullmatch(r"[0-9a-f]{40}", getattr(args, name)):
                parser.error("candidate SHA must be a full lowercase Git SHA")
        for name in ("context_sha256", "inputs_sha256"):
            if not DIGEST_RE.fullmatch(getattr(args, name)):
                parser.error(f"{name.replace('_', ' ')} must be a lowercase SHA-256 digest")
        for name in ("image_id", "builder_image_id"):
            if not re.fullmatch(r"sha256:[0-9a-f]{64}", getattr(args, name)):
                parser.error(f"{name.replace('_', ' ')} must be an immutable sha256 image ID")
        if not re.fullmatch(r"[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?", args.run_id):
            parser.error("run ID must be a lowercase Docker-safe identifier")
        if args.platform not in ("linux/amd64", "linux/arm64"):
            parser.error("backend platform must be a supported Linux architecture")
        try:
            context = json.loads(args.build_context)
        except json.JSONDecodeError as exc:
            parser.error(f"build context must be JSON: {exc}")
        if not isinstance(context, dict):
            parser.error("build context must be a JSON object")
        args.build_context = context
    return args


def main() -> int:
    args = parse_args()
    if args.mode in ("resource", "connector", "validate-raw", "validate-inputs", "invalidate"):
        try:
            if args.mode == "connector":
                print(json.dumps(connector_shutdown(connector_command, args.name, args.owner, args.run_id,
                                                    args.image, pathlib.Path(args.audit))))
            elif args.mode == "invalidate":
                invalidate_collection(pathlib.Path(args.profiles), pathlib.Path(args.raw), pathlib.Path(args.artifacts))
            elif args.mode == "validate-raw":
                print(json.dumps(raw_files(pathlib.Path(args.directory)), sort_keys=True))
            elif args.mode == "validate-inputs":
                module = backend_module()
                try:
                    value = module.validate_inputs(
                        module.read_pinned(args.inputs, args.sha256), ["kubernetes", "podman"])
                except module.Refused:
                    raise JourneyError("backend prerequisite validation refused") from None
                print(value["docker_socket"])
            else:
                if args.kind not in ("container", "network") or args.action not in ("absent", "owned", "remove", "stop"):
                    raise JourneyError("unsupported guarded resource operation")
                if args.action == "stop" and args.kind != "container":
                    raise JourneyError("only owned containers may receive shutdown signals")
                def command(*arguments, check=True, timeout=180):
                    result = subprocess.run(["docker", *arguments], text=True, capture_output=True, timeout=timeout, check=False)
                    if check and result.returncode:
                        raise JourneyError("guarded Docker operation failed")
                    return result
                print(json.dumps(guarded_resource(command, args.kind, args.name, args.action, args.owner, args.run_id, args.lane, args.image)))
            return 0
        except JourneyError:
            print("guarded coverage evidence/resource operation refused: JourneyError", file=sys.stderr)
            return 1
        except Exception as error:
            print(unexpected_exception_record("unexpected-coverage-resource-exception", error), file=sys.stderr)
            return 1
    if args.mode == "validate-backends":
        try:
            validate_backend_contribution(args)
            print("real backend coverage contribution validated")
            return 0
        except JourneyError:
            print("backend coverage contribution rejected: JourneyError", file=sys.stderr)
            return 1
        except Exception as error:
            print(unexpected_exception_record("unexpected-backend-validation-exception", error), file=sys.stderr)
            return 1
    collector = Collector(args)

    def interrupted(signum: int, _frame: Any) -> None:
        raise Interrupted(f"received signal {signum}")

    for sig in (signal.SIGTERM, signal.SIGHUP, signal.SIGINT):
        signal.signal(sig, interrupted)
    try:
        final = collector.execute()
        collector.cleanup()
        collector.write_provenance(final)
        print("SSO live coverage journey passed; see raw/journeys/sso/provenance.json")
        return 0
    except (JourneyError, OSError, ValueError, Interrupted) as exc:
        print(f"SSO live coverage journey refused: {type(exc).__name__}; raw diagnostics withheld",
              file=sys.stderr)
        return 1
    except Exception as exc:
        print(unexpected_exception_record("unexpected-sso-journey-exception", exc), file=sys.stderr)
        return 1
    finally:
        for sig in (signal.SIGTERM, signal.SIGHUP, signal.SIGINT):
            signal.signal(sig, signal.SIG_IGN)
        try:
            collector.cleanup()
        except JourneyError as exc:
            print(f"SSO cleanup refused: {type(exc).__name__}; raw diagnostics withheld",
                  file=sys.stderr)
        except Exception as exc:
            print(unexpected_exception_record("unexpected-sso-cleanup-exception", exc), file=sys.stderr)


if __name__ == "__main__":
    raise SystemExit(main())
