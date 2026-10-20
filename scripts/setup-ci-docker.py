#!/usr/bin/env python3
"""Install and verify the native CI Docker runtime before candidate image work."""

import fcntl
import hashlib
import io
import json
import os
from pathlib import Path
import platform
import re
import shlex
import stat
import subprocess
import sys
import tarfile
import tempfile
import urllib.request
import uuid


ENGINE_PACKAGE = "5:29.9.1-1~ubuntu.26.04~resolute"
CONTAINERD_PACKAGE = "2.4.2-2~ubuntu.26.04~resolute"
CONTAINERD_VERSION = "2.3.7"
CONTAINERD_REVISION = "6f589cf13dc01a5b493911355427e05b2ff005d7"
CONTAINERD_ARCHIVES = {
    "amd64": "09a1b669ac6d21e7e24c3c33a9805b48987ba5845e8d88f039c47013a0c0025c",
    "arm64": "e08530c6c8a23c612606da5d2832aed9bed8fc7f8c1b41df0f148a701b931d69",
}
CONTAINERD_BINARIES = ("containerd", "containerd-shim-runc-v2", "containerd-stress", "ctr")
INSTALL_ROOT = Path("/opt/caesium-ci/containerd")
PROBE_IMAGE = "alpine:3.23@sha256:85fe1e81d6758c208f3e1eed4338a1997e19d4be002d4dd32d3100c9a8c010a0"
EXPECTED = {
    "ServerVersion": "29.9.1",
    "ContainerdCommit": CONTAINERD_REVISION,
    "RuncCommit": "v1.5.2-0-g29dd3dc2",
    "CgroupDriver": "cgroupfs",
    "CgroupVersion": "2",
}

# This read-only probe runs under sudo. It prints only bound process identity;
# raw process arguments and environments never enter the CI log.
PROCESS_PROBE = r'''
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys

PROC_ROOT = Path("/proc")

def daemon_pid():
    raw = subprocess.check_output(["systemctl", "show", "--property=MainPID", "--value", "containerd.service"], text=True).strip()
    if not re.fullmatch(r"[1-9][0-9]*", raw):
        raise RuntimeError("containerd has no running MainPID")
    return raw

def inspect_process(pid, expected_executable, expected_sha256):
    executable = PROC_ROOT / pid / "exe"
    observed_path = str(executable.resolve(strict=True))
    observed_hash = hashlib.sha256(executable.read_bytes()).hexdigest()
    if observed_path != expected_executable or observed_hash != expected_sha256:
        raise RuntimeError("running process executable differs from the verified upstream binary")
    return {"pid": int(pid), "executable": observed_path, "sha256": observed_hash}

def flag_value(arguments, flag):
    positions = [i for i, value in enumerate(arguments) if value == flag]
    if len(positions) != 1 or positions[0] + 1 >= len(arguments):
        return None
    return arguments[positions[0] + 1]

def inspect_shim(cid, expected_executable, expected_sha256):
    if not re.fullmatch(r"[0-9a-f]{64}", cid):
        raise RuntimeError("invalid probe container identity")
    matches = []
    for process in PROC_ROOT.iterdir():
        if not process.name.isdecimal():
            continue
        try:
            arguments = (process / "cmdline").read_bytes().rstrip(b"\0").decode().split("\0")
        except (FileNotFoundError, ProcessLookupError, UnicodeDecodeError):
            continue
        if (arguments and Path(arguments[0]).name == "containerd-shim-runc-v2"
                and flag_value(arguments, "-namespace") == "moby"
                and flag_value(arguments, "-id") == cid):
            matches.append(process.name)
    if len(matches) != 1:
        raise RuntimeError("probe container has no unique running moby shim")
    result = inspect_process(matches[0], expected_executable, expected_sha256)
    result.update(namespace="moby", container_id=cid)
    return result

def main():
    mode = sys.argv[1]
    if mode == "path":
        environment = (PROC_ROOT / daemon_pid() / "environ").read_bytes().split(b"\0")
        paths = [entry[5:].decode() for entry in environment if entry.startswith(b"PATH=")]
        if len(paths) != 1:
            raise RuntimeError("containerd has no unique PATH")
        print(json.dumps({"path": paths[0]}))
    elif mode == "daemon":
        print(json.dumps(inspect_process(daemon_pid(), sys.argv[2], sys.argv[3])))
    elif mode == "shim":
        print(json.dumps(inspect_shim(sys.argv[4], sys.argv[2], sys.argv[3])))
    else:
        raise RuntimeError("unknown process probe")

if __name__ == "__main__":
    main()
'''
REPOSITORY_ARCHITECTURES = {
    "x86_64": "amd64",
    "amd64": "amd64",
    "aarch64": "arm64",
    "arm64": "arm64",
}


def run(*command, **kwargs):
    return subprocess.run(command, check=True, text=True, **kwargs)


def validate_docker_arguments(arguments):
    if not arguments or not Path(arguments[0]).is_absolute() or Path(arguments[0]).name != "dockerd":
        raise RuntimeError("Docker service does not identify a direct dockerd command")
    if any(argument == "--exec-opt" or argument.startswith("--exec-opt=") for argument in arguments[1:]):
        # Docker rejects any directive present in both flags and daemon.json,
        # including a CLI exec-opt unrelated to the cgroup driver.
        raise RuntimeError("refusing conflicting Docker CLI exec-opt configuration")
    paths = []
    for index, argument in enumerate(arguments[1:], 1):
        if argument == "--config-file":
            if index + 1 >= len(arguments):
                raise RuntimeError("Docker config-file argument is incomplete")
            paths.append(arguments[index + 1])
        elif argument.startswith("--config-file="):
            paths.append(argument.split("=", 1)[1])
    if len(paths) > 1 or (paths and paths != ["/etc/docker/daemon.json"]):
        raise RuntimeError("refusing custom or ambiguous Docker config-file arguments")


def merged_docker_config(body):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise RuntimeError("Docker daemon configuration has duplicate JSON keys")
            result[key] = value
        return result
    def invalid_constant(_value):
        raise ValueError("non-JSON constant")
    try:
        config = json.loads(body, object_pairs_hook=unique, parse_constant=invalid_constant)
    except (ValueError, UnicodeError) as error:
        raise RuntimeError("Docker daemon configuration is not valid JSON") from error
    if not isinstance(config, dict):
        raise RuntimeError("Docker daemon configuration must be an object")
    options = config.get("exec-opts", [])
    if not isinstance(options, list) or any(not isinstance(option, str) for option in options):
        raise RuntimeError("Docker daemon exec-opts must be a list of strings")
    seen, merged = set(), []
    for option in options:
        key, separator, value = option.partition("=")
        if (not separator or not re.fullmatch(r"[A-Za-z0-9_.-]+", key)
                or not value or any(ord(character) < 32 for character in value)):
            raise RuntimeError("Docker daemon exec-opt is malformed")
        if key in seen:
            raise RuntimeError("Docker daemon exec-opts contain duplicate option names")
        seen.add(key)
        if key == "native.cgroupdriver":
            if value not in ("systemd", "cgroupfs"):
                raise RuntimeError("Docker daemon cgroup driver is unsupported")
            option = "native.cgroupdriver=cgroupfs"
        merged.append(option)
    if "native.cgroupdriver" not in seen:
        merged.append("native.cgroupdriver=cgroupfs")
    config["exec-opts"] = merged
    return config


def config_fingerprint(metadata):
    return (metadata.st_dev, metadata.st_ino, metadata.st_mode, metadata.st_uid, metadata.st_gid,
            metadata.st_nlink, metadata.st_size, metadata.st_mtime_ns, metadata.st_ctime_ns)


def read_docker_config(directory_fd):
    try:
        descriptor = os.open("daemon.json", os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory_fd)
    except FileNotFoundError:
        return None, b"{}"
    with os.fdopen(descriptor, "rb") as stream:
        metadata = os.fstat(stream.fileno())
        if not stat.S_ISREG(metadata.st_mode) or metadata.st_nlink != 1:
            raise RuntimeError("Docker daemon configuration must be a regular file with one link")
        body = stream.read(1024 * 1024 + 1)
        if len(body) > 1024 * 1024 or config_fingerprint(metadata) != config_fingerprint(os.fstat(stream.fileno())):
            raise RuntimeError("Docker daemon configuration changed or exceeded its size bound")
    return metadata, body


def configure_docker_cgroupfs(directory=Path("/etc/docker"), proc_root=Path("/proc")):
    if os.geteuid() != 0:
        raise RuntimeError("Docker daemon configuration requires root ownership")
    command = run("systemctl", "show", "--property=ExecStart", "--value",
                  "docker.service", capture_output=True).stdout
    arguments = re.findall(r"argv\[\]=([^;]*?)\s*;", command)
    if len(arguments) != 1 or any(character in arguments[0] for character in ("\\", "$", "%")):
        raise RuntimeError("Docker ExecStart arguments cannot be resolved safely")
    try:
        configured = shlex.split(arguments[0])
    except ValueError:
        raise RuntimeError("Docker ExecStart arguments cannot be resolved safely") from None
    validate_docker_arguments(configured)
    pid = run("systemctl", "show", "--property=MainPID", "--value",
              "docker.service", capture_output=True).stdout.strip()
    if not re.fullmatch(r"[1-9][0-9]*", pid):
        raise RuntimeError("Docker has no running MainPID")
    try:
        actual = (proc_root / pid / "cmdline").read_bytes().rstrip(b"\0").decode().split("\0")
    except UnicodeError:
        raise RuntimeError("running Docker arguments cannot be resolved safely") from None
    validate_docker_arguments(actual)
    directory.mkdir(mode=0o755, parents=False, exist_ok=True)
    directory_fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    temporary = None
    try:
        # Serialize this helper and reject observed changes or substitutions
        # before atomically publishing the updated configuration.
        fcntl.flock(directory_fd, fcntl.LOCK_EX)
        metadata, body = read_docker_config(directory_fd)
        config = merged_docker_config(body)
        if metadata is not None and config == json.loads(body):
            return False
        temporary = ".caesium-ci-" + uuid.uuid4().hex + ".json"
        descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                             0o600, dir_fd=directory_fd)
        with os.fdopen(descriptor, "w") as stream:
            os.fchmod(stream.fileno(), stat.S_IMODE(metadata.st_mode) if metadata else 0o644)
            if metadata:
                os.fchown(stream.fileno(), metadata.st_uid, metadata.st_gid)
            json.dump(config, stream, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        current, current_body = read_docker_config(directory_fd)
        if (current_body != body or (current is None) != (metadata is None)
                or (current is not None and config_fingerprint(current) != config_fingerprint(metadata))):
            raise RuntimeError("Docker daemon configuration changed before replacement")
        if metadata is None:
            # A link publishes a new file atomically without replacing a file
            # another writer may have created after the final absence check.
            os.link(temporary, "daemon.json", src_dir_fd=directory_fd, dst_dir_fd=directory_fd,
                    follow_symlinks=False)
            os.unlink(temporary, dir_fd=directory_fd)
        else:
            os.replace(temporary, "daemon.json", src_dir_fd=directory_fd, dst_dir_fd=directory_fd)
        temporary = None
        os.fsync(directory_fd)
        return True
    finally:
        if temporary is not None:
            os.unlink(temporary, dir_fd=directory_fd)
        os.close(directory_fd)


def archive_url(architecture):
    return ("https://github.com/containerd/containerd/releases/download/"
            f"v{CONTAINERD_VERSION}/containerd-{CONTAINERD_VERSION}-linux-{architecture}.tar.gz")


def download_archive(architecture):
    with urllib.request.urlopen(archive_url(architecture), timeout=30) as response:
        data = response.read(64 * 1024 * 1024 + 1)
    if (len(data) > 64 * 1024 * 1024
            or hashlib.sha256(data).hexdigest() != CONTAINERD_ARCHIVES[architecture]):
        raise RuntimeError("upstream containerd archive checksum or size differs from the pin")
    return data


def stage_archive(data, directory):
    expected = {"bin"} | {"bin/" + name for name in CONTAINERD_BINARIES}
    hashes = {}
    with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as archive:
        members = archive.getmembers()
        if len(members) != len(expected) or {member.name for member in members} != expected:
            raise RuntimeError("unexpected upstream containerd archive inventory")
        for member in members:
            if member.name == "bin":
                if not member.isdir():
                    raise RuntimeError("upstream binary directory is not a directory")
                continue
            if not member.isfile() or member.size <= 0 or member.size > 128 * 1024 * 1024:
                raise RuntimeError("upstream containerd binary is not a bounded regular file")
            binary = archive.extractfile(member).read()
            name = member.name.removeprefix("bin/")
            (directory / name).write_bytes(binary)
            hashes[name] = hashlib.sha256(binary).hexdigest()
    return hashes


def process_probe(mode, *arguments):
    return json.loads(run("sudo", "python3", "-c", PROCESS_PROBE, mode,
                          *arguments, capture_output=True, timeout=30).stdout)


def configure_containerd(architecture, data):
    binary_directory = INSTALL_ROOT / CONTAINERD_VERSION / architecture / "bin"
    # Refuse to discard custom service arguments. Existing daemon/config files
    # remain in place; the package's default command reads the same config.
    command = run("systemctl", "show", "--property=ExecStart", "--value",
                  "containerd.service", capture_output=True).stdout
    arguments = re.findall(r"argv\[\]=([^;]*?)\s*;", command)
    if len(arguments) != 1 or arguments[0].strip() not in (
            "/usr/bin/containerd", str(binary_directory / "containerd")):
        raise RuntimeError("refusing to replace custom containerd service arguments")
    previous_path = process_probe("path")["path"]
    if not re.fullmatch(r"/[A-Za-z0-9_./+-]+(?::/[A-Za-z0-9_./+-]+)*", previous_path):
        raise RuntimeError("containerd PATH cannot be preserved safely")
    search_path = ":".join([str(binary_directory)] + [part for part in previous_path.split(":")
                                                   if part != str(binary_directory)])
    run("sudo", "systemctl", "stop", "docker.service", "docker.socket", "containerd.service")
    with tempfile.TemporaryDirectory(prefix="caesium-ci-containerd-") as temporary:
        directory = Path(temporary)
        hashes = stage_archive(data, directory)
        run("sudo", "install", "-m", "0755", "-d", str(binary_directory))
        for name in CONTAINERD_BINARIES:
            run("sudo", "install", "-m", "0755", str(directory / name), str(binary_directory / name))
    run("sudo", "install", "-m", "0755", "-d", "/etc/systemd/system/containerd.service.d")
    run("sudo", "tee", "/etc/systemd/system/containerd.service.d/caesium-ci-runtime.conf", input=(
        "[Service]\nExecStart=\n"
        f"ExecStart={binary_directory / 'containerd'}\n"
        f'Environment="PATH={search_path}"\n'), stdout=subprocess.DEVNULL)
    run("sudo", "systemctl", "daemon-reload")
    run("sudo", "systemctl", "restart", "containerd.service", "docker.service")
    daemon = process_probe("daemon", str(binary_directory / "containerd"), hashes["containerd"])
    version = run(str(binary_directory / "containerd-shim-runc-v2"), "--version", capture_output=True).stdout
    if (not re.search(r"Version:\s+v?" + re.escape(CONTAINERD_VERSION) + r"\s", version)
            or not re.search(r"Revision:\s+" + CONTAINERD_REVISION + r"\s", version)):
        raise RuntimeError("upstream shim version differs from the verified release")
    return binary_directory, hashes, daemon


def owned_probe(cid, owner, name, require_running=False):
    inspected = json.loads(run("docker", "inspect", "--format", "{{json .}}", cid,
                               capture_output=True, timeout=10).stdout)
    if (inspected.get("Id") != cid or inspected.get("Name") != "/" + name
            or inspected.get("Config", {}).get("Labels", {}).get("caesium.ci.runtime.probe") != owner
            or inspected.get("Config", {}).get("Image") != PROBE_IMAGE):
        raise RuntimeError("runtime probe ownership is not confirmed")
    if require_running and inspected.get("State", {}).get("Running") is not True:
        raise RuntimeError("runtime probe is not running")


def verify_running_shim(architecture, binary_directory, hashes):
    owner = uuid.uuid4().hex
    name = "caesium-ci-runtime-" + owner
    run("docker", "pull", "--platform", "linux/" + architecture, PROBE_IMAGE, timeout=180)
    cid = run("docker", "create", "--name", name, "--label", "caesium.ci.runtime.probe=" + owner,
              "--platform", "linux/" + architecture, "--network", "none", "--read-only",
              "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--memory", "16m",
              "--pids-limit", "8", "--entrypoint", "/bin/sleep", PROBE_IMAGE, "30",
              capture_output=True, timeout=30).stdout.strip()
    if not re.fullmatch(r"[0-9a-f]{64}", cid):
        raise RuntimeError("runtime probe acknowledgement is uncertain; reconciliation required for " + name)
    try:
        owned_probe(cid, owner, name)
        started = run("docker", "start", cid, capture_output=True, timeout=30).stdout.strip()
        if started != cid:
            raise RuntimeError("runtime probe start acknowledgement differs from its container")
        owned_probe(cid, owner, name, require_running=True)
        return process_probe("shim", str(binary_directory / "containerd-shim-runc-v2"),
                             hashes["containerd-shim-runc-v2"], cid)
    finally:
        # Never clean up by an unconfirmed name or a foreign container ID.
        owned_probe(cid, owner, name)
        run("docker", "rm", "--force", cid, timeout=30)


def native_architecture():
    architecture = REPOSITORY_ARCHITECTURES.get(platform.machine())
    host_error = "Docker setup requires a native Ubuntu 26.04 amd64 or arm64 CI runner"
    if platform.system() != "Linux" or architecture is None:
        raise RuntimeError(host_error)
    release = platform.freedesktop_os_release()
    if (release.get("ID") != "ubuntu" or release.get("VERSION_ID") != "26.04"
            or release.get("VERSION_CODENAME") != "resolute"):
        raise RuntimeError(host_error)
    return architecture


def setup():
    architecture = native_architecture()

    data = download_archive(architecture)
    # Configure the selected ephemeral CI host before any install/restart. A
    # separate root mode owns the checked atomic update without exposing config
    # values in process arguments or logs.
    configured = run("sudo", "python3", str(Path(__file__).resolve()), "--configure-docker-cgroupfs",
                     capture_output=True, timeout=30)
    print(configured.stdout.strip(), flush=True)

    # Replace only Docker's repository definitions. Preserve all other daemon
    # and storage settings, and preserve package configuration during install.
    run("sudo", "install", "-m", "0755", "-d", "/etc/apt/keyrings")
    run("sudo", "curl", "-fsSL", "https://download.docker.com/linux/ubuntu/gpg",
        "-o", "/etc/apt/keyrings/docker.asc")
    run("sudo", "chmod", "a+r", "/etc/apt/keyrings/docker.asc")
    run("sudo", "rm", "-f", "/etc/apt/sources.list.d/docker.sources")
    run("sudo", "tee", "/etc/apt/sources.list.d/docker.list", input=(
        f"deb [arch={architecture} signed-by=/etc/apt/keyrings/docker.asc] "
        "https://download.docker.com/linux/ubuntu resolute stable\n"),
        stdout=subprocess.DEVNULL)
    run("sudo", "apt-get", "update", "-q")
    run("sudo", "systemctl", "stop", "docker.service", "docker.socket", "containerd.service")
    # Exact versions are intentional: a major-version check allowed an older
    # Docker daemon to remain paired with a newly updated containerd.
    run("sudo", "env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y",
        "--no-install-recommends", "-o", "Dpkg::Options::=--force-confold",
        f"docker-ce={ENGINE_PACKAGE}", f"docker-ce-cli={ENGINE_PACKAGE}",
        f"containerd.io={CONTAINERD_PACKAGE}")
    run("sudo", "systemctl", "start", "containerd.service")
    # The secure apt package retains runc 1.5.2. Select the security-patched
    # containerd LTS daemon AND shim explicitly, without overwriting apt files.
    # This is a CI compatibility candidate; native OOM qualification is separate.
    binary_directory, hashes, daemon = configure_containerd(architecture, data)
    run("dpkg-query", "-W", "docker-ce", "docker-ce-cli", "containerd.io")
    info = json.loads(run("docker", "info", "--format", "{{json .}}", capture_output=True).stdout)
    observed = {
        "ServerVersion": info.get("ServerVersion"),
        "ContainerdCommit": (info.get("ContainerdCommit") or {}).get("ID"),
        "RuncCommit": (info.get("RuncCommit") or {}).get("ID"),
        "CgroupDriver": info.get("CgroupDriver"),
        "CgroupVersion": info.get("CgroupVersion"),
    }
    print(json.dumps({"kind": "ci-docker-components", "architecture": architecture,
                      "observed": observed, "expected": EXPECTED}), flush=True)
    if observed != EXPECTED:
        raise RuntimeError("running Docker runtime does not match the pinned CI runtime")
    shim = verify_running_shim(architecture, binary_directory, hashes)
    print(json.dumps({"kind": "ci-docker-runtime", "architecture": architecture,
                      "observed": observed, "expected": EXPECTED,
                      "upstream_containerd": {"version": CONTAINERD_VERSION, "revision": CONTAINERD_REVISION,
                                              "url": archive_url(architecture),
                                              "archive_sha256": CONTAINERD_ARCHIVES[architecture],
                                              "binary_sha256": hashes},
                      "running_daemon": daemon, "running_shim": shim,
                      "runc_source_package": CONTAINERD_PACKAGE}), flush=True)


if __name__ == "__main__":
    if sys.argv[1:] == ["--configure-docker-cgroupfs"]:
        native_architecture()
        changed = configure_docker_cgroupfs()
        print(json.dumps({"kind": "ci-docker-cgroup-config", "changed": changed, "driver": "cgroupfs"}))
    elif not sys.argv[1:]:
        setup()
    else:
        raise SystemExit("unsupported CI runtime setup arguments")
