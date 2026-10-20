#!/usr/bin/env python3
"""Install and verify the native ARM64 CI runtime before loading any images."""

import json
import platform
import subprocess


ENGINE_PACKAGE = "5:29.9.1-1~ubuntu.26.04~resolute"
CONTAINERD_PACKAGE = "2.4.2-2~ubuntu.26.04~resolute"
EXPECTED = {
    "ServerVersion": "29.9.1",
    "ContainerdCommit": "b7a25261e029d68978bbf290df0a62fc819b3c51",
    "RuncCommit": "v1.5.2-0-g29dd3dc2",
}


def run(*command, **kwargs):
    return subprocess.run(command, check=True, text=True, **kwargs)


def setup():
    release = platform.freedesktop_os_release()
    if (platform.system() != "Linux" or platform.machine() not in ("aarch64", "arm64")
            or release.get("ID") != "ubuntu" or release.get("VERSION_ID") != "26.04"
            or release.get("VERSION_CODENAME") != "resolute"):
        raise RuntimeError("ARM64 Docker setup requires the Ubuntu 26.04 ARM64 CI runner")

    # Replace only Docker's repository definitions. Keep daemon, storage and
    # cgroup configuration, and preserve package configuration during install.
    run("sudo", "install", "-m", "0755", "-d", "/etc/apt/keyrings")
    run("sudo", "curl", "-fsSL", "https://download.docker.com/linux/ubuntu/gpg",
        "-o", "/etc/apt/keyrings/docker.asc")
    run("sudo", "chmod", "a+r", "/etc/apt/keyrings/docker.asc")
    run("sudo", "rm", "-f", "/etc/apt/sources.list.d/docker.sources")
    run("sudo", "tee", "/etc/apt/sources.list.d/docker.list", input=(
        "deb [arch=arm64 signed-by=/etc/apt/keyrings/docker.asc] "
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
    run("sudo", "systemctl", "restart", "containerd.service", "docker.service")
    run("dpkg-query", "-W", "docker-ce", "docker-ce-cli", "containerd.io")
    info = json.loads(run("docker", "info", "--format", "{{json .}}", capture_output=True).stdout)
    observed = {
        "ServerVersion": info.get("ServerVersion"),
        "ContainerdCommit": (info.get("ContainerdCommit") or {}).get("ID"),
        "RuncCommit": (info.get("RuncCommit") or {}).get("ID"),
    }
    print(json.dumps({"kind": "ci-arm64-docker-runtime", "observed": observed, "expected": EXPECTED}), flush=True)
    if observed != EXPECTED:
        raise RuntimeError("running Docker runtime does not match the pinned CI packages")


if __name__ == "__main__":
    setup()
