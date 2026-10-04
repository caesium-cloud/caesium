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
import hashlib
import json
import os
import pathlib
import re
import secrets
import select
import signal
import shutil
import subprocess
import sys
import tempfile
import time
from typing import Any
from urllib.parse import parse_qsl, urlsplit


MODULE = "github.com/caesium-cloud/caesium"
LABEL_OWNER = "caesium.coverage.owner"
LABEL_RUN = "caesium.coverage.run"
LABEL_LANE = "caesium.coverage.lane"
KEY_RE = re.compile(r"csk_[A-Za-z0-9_-]+")
DIGEST_RE = re.compile(r"[0-9a-f]{64}")
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


class Interrupted(JourneyError):
    pass


def redact(text: str) -> str:
    return KEY_RE.sub("[REDACTED_API_KEY]", text)


class Collector:
    def __init__(self, args: argparse.Namespace) -> None:
        self.args = args
        self.root = pathlib.Path(args.root).resolve()
        self.artifacts = pathlib.Path(args.artifacts).resolve()
        self.raw = pathlib.Path(args.raw).resolve()
        self.sso_artifacts = self.artifacts
        self.fixture_dir = self.artifacts / "fixture"
        self.server_raw = self.raw / "journeys" / "sso" / "server"
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
        self.network_name = f"{self.run_id}-sso-net"
        self.network_id = ""
        self.ids: list[tuple[str, str]] = []
        self.secret_files: list[pathlib.Path] = []
        self.protocol_events: list[dict[str, Any]] = []
        self.server_generations: list[dict[str, Any]] = []
        self.server_environment_sha256 = ""
        self.helper_process: subprocess.Popen[bytes] | None = None
        self.helper_container_id = ""

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

    def docker_run(
        self,
        *arguments: str,
        check: bool = True,
        timeout: int = 180,
        capture: bool = True,
    ) -> subprocess.CompletedProcess[str]:
        command = [self.docker, *map(str, arguments)]
        try:
            result = subprocess.run(
                command,
                check=False,
                text=True,
                stdout=subprocess.PIPE if capture else None,
                stderr=subprocess.PIPE if capture else None,
                timeout=timeout,
            )
        except subprocess.TimeoutExpired as exc:
            raise JourneyError(f"container command timed out: {command[1]}") from exc
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
        if result.returncode == 0:
            raise JourneyError(f"refusing pre-existing container name {name}")

    def create_network(self) -> None:
        result = self.docker_run("network", "inspect", self.network_name, check=False)
        if result.returncode == 0:
            raise JourneyError(f"refusing pre-existing network name {self.network_name}")
        created = self.docker_stdout(
            "network",
            "create",
            "--internal",
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
        self.docker_run(
            "run",
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
            "go build -tags=integration -o /fixture/sso-idp ./test/fixtures/sso-idp",
            timeout=900,
        )
        if not self.binary_path.is_file():
            raise JourneyError("builder did not produce the SSO fixture executable")
        self.binary_path.chmod(0o755)
        if self.binary_path.stat().st_size == 0:
            raise JourneyError("SSO fixture executable is empty")

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
        return info

    def remove_owned(self, container_id: str, lane: str) -> None:
        inspected = self.docker_run("container", "inspect", container_id, check=False)
        if inspected.returncode != 0:
            self.ids = [(cid, tag) for cid, tag in self.ids if cid != container_id]
            return
        self.assert_owned(container_id, lane)
        self.docker_run("container", "rm", "-f", container_id, check=False)
        self.ids = [(cid, tag) for cid, tag in self.ids if cid != container_id]

    def start_idp(self) -> None:
        self.require_absent_container(self.idp_name)
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
        if self.docker_run("container", "inspect", self.server_name, check=False).returncode == 0:
            raise JourneyError(f"refusing pre-existing container name {self.server_name}")
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
            "--env-file",
            str(self.server_env_file),
            "-v",
            f"{self.server_raw}:/var/lib/caesium/coverage",
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
        record = {
            "generation": generation,
            "container_id": container_id,
            "image_id": final.get("Image"),
            "database_mount_sha256": hashlib.sha256(str(self.database_dir.resolve()).encode()).hexdigest(),
            "server_environment_sha256": self.server_environment_sha256,
            "flush_rc": flush_rc,
            "exit_code": state.get("ExitCode"),
            "stop_rc": stop.returncode,
            "signal": "SIGTERM",
            "oom_killed": bool(state.get("OOMKilled")),
        }
        if (
            flush_rc != 0
            or stop.returncode != 0
            or state.get("ExitCode") not in (0, 143)
            or state.get("OOMKilled") is not False
            or final.get("Image") != self.image
        ):
            raise JourneyError(f"SSO server generation {generation} did not stop cleanly")
        self.server_generations.append(record)
        self.remove_owned(container_id, lane)
        return record

    def start_journey_process(self) -> subprocess.Popen[bytes]:
        self.require_absent_container(self.journey_name)
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
        created = subprocess.run(args, check=False, text=True, capture_output=True, timeout=60)
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
        journey_deadline = time.monotonic() + 230
        event, buffer = self.read_protocol_event(process, buffer, journey_deadline)
        while event.get("event") == "check":
            self.safe_check_event(event)
            event, buffer = self.read_protocol_event(process, buffer, journey_deadline)
        self.safe_restart_event(event)
        first_id = next(cid for cid, lane in self.ids if lane == "sso-server-g1")
        self.stop_server(first_id, 1)
        second_id = self.start_server(2)
        if first_id == second_id:
            raise JourneyError("SSO server restart reused the original container identity")
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
            "fixture_binary_sha256": binary_digest,
            "public_idp_metadata_sha256": metadata_digest,
            "idp_ready": json.loads(self.idp_ready_file.read_text()),
            "auth_status": json.loads(self.auth_status_file.read_text()),
            "server_generations": self.server_generations,
            "helper_events": self.protocol_events,
            "verdict": final,
            "raw": {"server": "server"},
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
                    pass
        if self.helper_container_id:
            try:
                self.remove_owned(self.helper_container_id, "sso-journey")
            except JourneyError as exc:
                print(f"cleanup retained SSO journey container: {redact(str(exc))}", file=sys.stderr)
        for container_id, lane in list(reversed(self.ids)):
            try:
                self.remove_owned(container_id, lane)
            except JourneyError as exc:
                print(f"cleanup retained SSO container {container_id}: {redact(str(exc))}", file=sys.stderr)
        if self.network_id:
            try:
                info = json.loads(self.docker_stdout("network", "inspect", self.network_id))[0]
                labels = info.get("Labels") or {}
                if labels.get(LABEL_OWNER) == self.sha and labels.get(LABEL_RUN) == self.run_id:
                    self.docker_run("network", "rm", self.network_id, check=False)
                else:
                    print("cleanup refused SSO network with unexpected labels", file=sys.stderr)
            except (JourneyError, json.JSONDecodeError, IndexError):
                pass
        for path in self.secret_files:
            try:
                path.unlink(missing_ok=True)
            except OSError:
                pass
        if self.secret_dir is not None and self.secret_dir.exists():
            shutil.rmtree(self.secret_dir, ignore_errors=True)
        if self.database_dir is not None and self.database_dir.exists():
            shutil.rmtree(self.database_dir, ignore_errors=True)

    def execute(self) -> None:
        for path in (self.server_raw,):
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
        self.database_dir = pathlib.Path(tempfile.mkdtemp(prefix=f"caesium-sso-db-{self.run_id}-"))
        self.server_raw.mkdir(parents=True, mode=0o777)
        self.server_raw.chmod(0o777)
        self.database_dir.chmod(0o777)
        self.create_network()
        self.build_fixture()
        self.write_secrets()
        self.start_idp()
        self.await_idp()
        self.start_server(1)
        final = self.run_journey()
        self.write_provenance(final)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=("sso",))
    parser.add_argument("--root", required=True)
    parser.add_argument("--artifacts", required=True)
    parser.add_argument("--raw", required=True)
    parser.add_argument("--coverage-image", required=True)
    parser.add_argument("--builder-image", required=True)
    parser.add_argument("--platform", required=True)
    parser.add_argument("--candidate-sha", required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--build-context", required=True)
    parser.add_argument("--image-provenance", required=True)
    parser.add_argument("--verified", choices=("true", "false"), required=True)
    parser.add_argument("--container-cli", default="docker")
    args = parser.parse_args()
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
    return args


def main() -> int:
    args = parse_args()
    collector = Collector(args)

    def interrupted(signum: int, _frame: Any) -> None:
        raise Interrupted(f"received signal {signum}")

    for sig in (signal.SIGTERM, signal.SIGHUP, signal.SIGINT):
        signal.signal(sig, interrupted)
    try:
        collector.execute()
        print("SSO live coverage journey passed; see raw/journeys/sso/provenance.json")
        return 0
    except (JourneyError, OSError, ValueError, Interrupted) as exc:
        print(f"SSO live coverage journey failed: {redact(str(exc))}", file=sys.stderr)
        return 1
    finally:
        collector.cleanup()


if __name__ == "__main__":
    raise SystemExit(main())
