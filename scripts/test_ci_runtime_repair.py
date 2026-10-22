"""Regression checks for live runtime verification and retained Helm evidence."""

import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest import mock

import yaml


ROOT = Path(__file__).resolve().parents[1]


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, ROOT / "scripts" / filename)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


RUNTIME = load("ci_docker_runtime", "setup-ci-docker.py")
DIAGNOSTICS = load("helm_diagnostics", "collect-helm-diagnostics.py")


class NativeRuntimeTests(unittest.TestCase):
    @staticmethod
    def archive(extra=None):
        output = io.BytesIO()
        with tarfile.open(fileobj=output, mode="w:gz") as archive:
            directory = tarfile.TarInfo("bin")
            directory.type = tarfile.DIRTYPE
            archive.addfile(directory)
            for name in RUNTIME.CONTAINERD_BINARIES:
                payload = ("verified fixture " + name).encode()
                member = tarfile.TarInfo("bin/" + name)
                member.size = len(payload)
                archive.addfile(member, io.BytesIO(payload))
            if extra is not None:
                archive.addfile(extra, io.BytesIO(b"x") if extra.isfile() else None)
        return output.getvalue()

    def run_setup(self, info=None, install_fails=False, machine="aarch64", shim_fails=False,
                  cleanup_fails=False, daemon_fails=False, create_ack=None, foreign_probe=False,
                  custom_service=False, cgroup_config_fails=False, newer_preinstalled=False):
        commands = []
        self.commands = commands
        owner, probe_name = "", ""
        cid = "c" * 64
        if info is None:
            info = {
                "ServerVersion": RUNTIME.EXPECTED["ServerVersion"],
                "ContainerdCommit": {"ID": RUNTIME.EXPECTED["ContainerdCommit"]},
                "RuncCommit": {"ID": RUNTIME.EXPECTED["RuncCommit"]},
                "CgroupDriver": "cgroupfs", "CgroupVersion": "2",
            }

        def run(*command, **kwargs):
            nonlocal owner, probe_name
            commands.append((command, kwargs))
            if install_fails and "install" in command and "apt-get" in command:
                raise subprocess.CalledProcessError(100, command)
            if (newer_preinstalled and "apt-get" in command and "install" in command
                    and "--allow-downgrades" not in command):
                raise subprocess.CalledProcessError(100, command, stderr=(
                    "E: Packages were downgraded and -y was used without --allow-downgrades."))
            response = ""
            if command[:2] == ("docker", "info"):
                response = json.dumps(info)
            elif command[:2] == ("systemctl", "show"):
                extra = " --config /custom/containerd.toml" if custom_service else ""
                response = "{ path=/usr/bin/containerd ; argv[]=/usr/bin/containerd" + extra + " ; ignore_errors=no ; }"
            elif command[-1] == "--configure-docker-cgroupfs":
                if cgroup_config_fails:
                    raise subprocess.CalledProcessError(1, command)
                response = json.dumps({"kind": "ci-docker-cgroup-config", "changed": True, "driver": "cgroupfs"})
            elif command[:3] == ("sudo", "python3", "-c"):
                mode = command[4]
                if mode == "path":
                    response = json.dumps({"path": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"})
                elif (mode == "shim" and shim_fails) or (mode == "daemon" and daemon_fails):
                    raise subprocess.CalledProcessError(1, command)
                else:
                    record = {"pid": 123, "executable": command[5], "sha256": command[6]}
                    if mode == "shim":
                        record.update(namespace="moby", container_id=command[7])
                    response = json.dumps(record)
            elif command[0].endswith("/containerd-shim-runc-v2"):
                response = ("containerd-shim-runc-v2:\n  Version:  v" + RUNTIME.CONTAINERD_VERSION
                            + "\n  Revision: " + RUNTIME.CONTAINERD_REVISION + "\n")
            elif command[:2] == ("docker", "create"):
                probe_name = command[command.index("--name") + 1]
                owner = command[command.index("--label") + 1].split("=", 1)[1]
                response = cid if create_ack is None else create_ack
            elif command[:2] == ("docker", "start"):
                response = cid
            elif command[:2] == ("docker", "inspect"):
                response = json.dumps({"Id": cid, "Name": "/" + probe_name, "State": {"Running": True},
                                       "Config": {"Image": RUNTIME.PROBE_IMAGE,
                                                  "Labels": {"caesium.ci.runtime.probe": "foreign" if foreign_probe else owner}}})
            elif command[:2] == ("docker", "rm") and cleanup_fails:
                raise subprocess.CalledProcessError(1, command)
            return subprocess.CompletedProcess(command, 0, response)

        output = io.StringIO()
        self.output = output
        with mock.patch.object(RUNTIME.platform, "system", return_value="Linux"), \
                mock.patch.object(RUNTIME.platform, "machine", return_value=machine), \
                mock.patch.object(RUNTIME.platform, "freedesktop_os_release", return_value={
                    "ID": "ubuntu", "VERSION_ID": "26.04", "VERSION_CODENAME": "resolute"}), \
                mock.patch.object(RUNTIME, "download_archive", return_value=self.archive()), \
                mock.patch.object(RUNTIME, "run", side_effect=run), \
                contextlib.redirect_stdout(output):
            RUNTIME.setup()
        self.reports = [json.loads(line) for line in output.getvalue().splitlines()]
        return commands, json.loads(output.getvalue().splitlines()[-1])

    def test_newer_preinstalled_runner_packages_converge_to_exact_runtime_pins(self):
        for machine, architecture in [("x86_64", "amd64"), ("amd64", "amd64"),
                                      ("aarch64", "arm64"), ("arm64", "arm64")]:
            with self.subTest(machine=machine):
                commands, report = self.run_setup(machine=machine, newer_preinstalled=True)
                self.assertEqual(report["kind"], "ci-docker-runtime")
                self.assertEqual(report["architecture"], architecture)
                self.assertEqual(report["observed"], RUNTIME.EXPECTED)
                self.assertEqual(report["runc_source_package"], RUNTIME.CONTAINERD_PACKAGE)
                self.assertEqual(report["upstream_containerd"]["archive_sha256"],
                                 RUNTIME.CONTAINERD_ARCHIVES[architecture])
                self.assertEqual(report["running_shim"]["container_id"], "c" * 64)
                self.assertEqual([command for command, _ in commands if command[:2] == ("docker", "rm")],
                                 [("docker", "rm", "--force", "c" * 64)])

    def test_exact_packages_are_installed_before_running_daemon_verification(self):
        for machine, architecture in [("x86_64", "amd64"), ("amd64", "amd64"),
                                      ("aarch64", "arm64"), ("arm64", "arm64")]:
            with self.subTest(machine=machine):
                commands, report = self.run_setup(machine=machine)
                args = [command for command, _ in commands]
                install = next(command for command in args if "apt-get" in command and "install" in command)
                self.assertIn("docker-ce=" + RUNTIME.ENGINE_PACKAGE, install)
                self.assertIn("docker-ce-cli=" + RUNTIME.ENGINE_PACKAGE, install)
                self.assertIn("containerd.io=" + RUNTIME.CONTAINERD_PACKAGE, install)
                self.assertIn("Dpkg::Options::=--force-confold", install)
                self.assertEqual(install.count("--allow-downgrades"), 1)
                packages = [argument for argument in install[install.index("install") + 1:]
                            if not argument.startswith(("-", "Dpkg::Options::="))]
                self.assertEqual(packages, ["docker-ce=" + RUNTIME.ENGINE_PACKAGE,
                                            "docker-ce-cli=" + RUNTIME.ENGINE_PACKAGE,
                                            "containerd.io=" + RUNTIME.CONTAINERD_PACKAGE])
                for bypass in ("--force-yes", "--allow-change-held-packages",
                               "--allow-remove-essential", "--allow-unauthenticated"):
                    self.assertFalse(any(argument == bypass or argument.startswith(bypass + "=")
                                         for argument in install))
                restart = next(command for command in args if "restart" in command)
                verify = next(command for command in args if command[:2] == ("docker", "info"))
                configure = next(command for command in args if command[-1] == "--configure-docker-cgroupfs")
                first_stop = next(command for command in args if "stop" in command and "systemctl" in command)
                self.assertLess(args.index(configure), args.index(first_stop))
                self.assertLess(args.index(install), args.index(restart))
                self.assertLess(args.index(restart), args.index(verify))
                source = next(kwargs["input"] for command, kwargs in commands
                              if command[-1] == "/etc/apt/sources.list.d/docker.list")
                self.assertEqual(source, f"deb [arch={architecture} signed-by=/etc/apt/keyrings/docker.asc] "
                                 "https://download.docker.com/linux/ubuntu resolute stable\n")
                self.assertEqual(report["architecture"], architecture)
                self.assertEqual(report["observed"], RUNTIME.EXPECTED)
                for kind in ("ci-docker-components", "ci-docker-runtime"):
                    observation = next(item for item in self.reports if item["kind"] == kind)
                    self.assertEqual(observation["observed"]["CgroupDriver"], "cgroupfs")
                    self.assertEqual(observation["observed"]["CgroupVersion"], "2")
                self.assertEqual(report["upstream_containerd"]["archive_sha256"], RUNTIME.CONTAINERD_ARCHIVES[architecture])
                self.assertEqual(report["running_daemon"]["executable"],
                                 f"/opt/caesium-ci/containerd/2.3.7/{architecture}/bin/containerd")
                self.assertEqual(report["running_shim"]["container_id"], "c" * 64)
                self.assertEqual(report["running_shim"]["namespace"], "moby")
                override = next(kwargs["input"] for command, kwargs in commands
                                if command[-1].endswith("/caesium-ci-runtime.conf"))
                self.assertIn(f"ExecStart=/opt/caesium-ci/containerd/2.3.7/{architecture}/bin/containerd\n", override)
                self.assertIn(f'Environment="PATH=/opt/caesium-ci/containerd/2.3.7/{architecture}/bin:', override)
                self.assertIn(":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", override)
                probe = next(command for command in args if command[:2] == ("docker", "create"))
                for option in ("--network", "--read-only", "--cap-drop", "--security-opt", "--pids-limit"):
                    self.assertIn(option, probe)
                self.assertEqual(probe[-2:], (RUNTIME.PROBE_IMAGE, "30"))
                removed = [command for command in args if command[:2] == ("docker", "rm")]
                self.assertEqual(removed, [("docker", "rm", "--force", "c" * 64)])

    def test_successful_package_install_cannot_hide_old_or_missing_daemon_components(self):
        for machine in ("x86_64", "aarch64"):
            for field in RUNTIME.EXPECTED:
                for failure in ("outdated", "missing"):
                    with self.subTest(machine=machine, field=field, failure=failure):
                        info = {
                            "ServerVersion": RUNTIME.EXPECTED["ServerVersion"],
                            "ContainerdCommit": {"ID": RUNTIME.EXPECTED["ContainerdCommit"]},
                            "RuncCommit": {"ID": RUNTIME.EXPECTED["RuncCommit"]},
                            "CgroupDriver": "cgroupfs", "CgroupVersion": "2",
                        }
                        if failure == "missing":
                            del info[field]
                        else:
                            info[field] = {"ServerVersion": "29.4.2", "CgroupDriver": "systemd",
                                           "CgroupVersion": "1"}.get(field, {"ID": "outdated"})
                        with self.assertRaisesRegex(RuntimeError, "running Docker runtime"):
                            self.run_setup(info, machine=machine)
                        self.assertFalse(any(command[:2] in (("docker", "pull"), ("docker", "create"))
                                             for command, _ in self.commands))

    def test_install_failure_is_fatal(self):
        for machine in ("x86_64", "aarch64"):
            with self.subTest(machine=machine), self.assertRaises(subprocess.CalledProcessError):
                self.run_setup(install_fails=True, machine=machine)
            reports = [json.loads(line) for line in self.output.getvalue().splitlines()]
            self.assertFalse(any(report["kind"] in ("ci-docker-components", "ci-docker-runtime")
                                 for report in reports))
            self.assertFalse(any(command[:2] in (("docker", "pull"), ("docker", "create"))
                                 for command, _ in self.commands))

    def test_refused_docker_configuration_prevents_install_stop_and_probe(self):
        with self.assertRaises(subprocess.CalledProcessError):
            self.run_setup(cgroup_config_fails=True)
        self.assertFalse(any("apt-get" in command or "systemctl" in command or command[0] == "docker"
                             for command, _ in self.commands))

    def test_wrong_running_daemon_or_shim_and_cleanup_failure_are_fatal(self):
        for failure in ("daemon_fails", "shim_fails", "cleanup_fails"):
            with self.subTest(failure=failure), self.assertRaises(subprocess.CalledProcessError):
                self.run_setup(**{failure: True})
            removed = [command for command, _ in self.commands if command[:2] == ("docker", "rm")]
            self.assertEqual(removed, [] if failure == "daemon_fails" else [("docker", "rm", "--force", "c" * 64)])

    def test_uncertain_probe_acknowledgement_never_removes_an_unbound_container(self):
        with self.assertRaisesRegex(RuntimeError, "reconciliation required"), \
                mock.patch.object(RUNTIME, "owned_probe") as owned:
            self.run_setup(create_ack="not-a-confirmed-id")
        owned.assert_not_called()

    def test_foreign_probe_is_never_removed_and_custom_service_arguments_are_preserved(self):
        with self.assertRaisesRegex(RuntimeError, "ownership is not confirmed"):
            self.run_setup(foreign_probe=True)
        self.assertFalse(any(command[:2] == ("docker", "rm") for command, _ in self.commands))
        with self.assertRaisesRegex(RuntimeError, "custom containerd service arguments"):
            self.run_setup(custom_service=True)
        self.assertFalse(any(command[-1].endswith("caesium-ci-runtime.conf") for command, _ in self.commands))

    def test_archive_digest_is_checked_before_installing_any_package(self):
        for architecture in ("amd64", "arm64"):
            with self.subTest(architecture=architecture), \
                    mock.patch.object(RUNTIME.urllib.request, "urlopen", return_value=io.BytesIO(b"tampered")), \
                    self.assertRaisesRegex(RuntimeError, "archive checksum"):
                RUNTIME.download_archive(architecture)

    def test_archive_extraction_refuses_links_unknown_paths_and_duplicates(self):
        extras = [tarfile.TarInfo("../outside"), tarfile.TarInfo("bin/containerd")]
        link = tarfile.TarInfo("bin/foreign-link")
        link.type, link.linkname = tarfile.SYMTYPE, "/etc/shadow"
        extras.append(link)
        for extra in extras:
            with self.subTest(name=extra.name), tempfile.TemporaryDirectory() as temporary, \
                    self.assertRaisesRegex(RuntimeError, "archive inventory"):
                RUNTIME.stage_archive(self.archive(extra), Path(temporary))

    def test_read_only_probe_checks_actual_process_executable_hash_and_exact_moby_container(self):
        namespace = {"__name__": "ci_runtime_probe_test"}
        exec(RUNTIME.PROCESS_PROBE, namespace)
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            namespace["PROC_ROOT"] = root / "proc"
            process = namespace["PROC_ROOT"] / "123"
            process.mkdir(parents=True)
            executable = root / "containerd-shim-runc-v2"
            executable.write_bytes(b"approved upstream shim")
            expected_hash = hashlib.sha256(executable.read_bytes()).hexdigest()
            (process / "exe").symlink_to(executable)
            cid = "a" * 64
            (process / "cmdline").write_bytes((str(executable) + "\0-namespace\0moby\0-id\0" + cid + "\0").encode())
            record = namespace["inspect_shim"](cid, str(executable), expected_hash)
            self.assertEqual(record["pid"], 123)
            self.assertEqual(record["container_id"], cid)
            for path, digest in [(str(root / "wrong-shim"), expected_hash), (str(executable), "0" * 64)]:
                with self.subTest(path=path, digest=digest), self.assertRaisesRegex(RuntimeError, "executable differs"):
                    namespace["inspect_shim"](cid, path, digest)
            (process / "cmdline").write_bytes((str(executable) + "\0-namespace\0other\0-id\0" + cid + "\0").encode())
            with self.assertRaisesRegex(RuntimeError, "no unique running moby shim"):
                namespace["inspect_shim"](cid, str(executable), expected_hash)

    def test_non_native_host_fails_before_privileged_commands(self):
        valid_release = {"ID": "ubuntu", "VERSION_ID": "26.04", "VERSION_CODENAME": "resolute"}
        hosts = [("Darwin", "arm64", valid_release), ("Windows", "amd64", valid_release),
                 ("Linux", "riscv64", valid_release)]
        for machine in ("x86_64", "aarch64"):
            for release in ({}, {**valid_release, "ID": "debian"},
                            {**valid_release, "VERSION_ID": "24.04"},
                            {**valid_release, "VERSION_CODENAME": "noble"}):
                hosts.append(("Linux", machine, release))
        for system, machine, release in hosts:
            with self.subTest(system=system, machine=machine, release=release), \
                    mock.patch.object(RUNTIME.platform, "freedesktop_os_release", return_value=release), \
                    mock.patch.object(RUNTIME.platform, "system", return_value=system), \
                    mock.patch.object(RUNTIME.platform, "machine", return_value=machine), \
                    mock.patch.object(RUNTIME, "run") as run:
                with self.assertRaisesRegex(RuntimeError, "Ubuntu 26.04 amd64 or arm64"):
                    RUNTIME.setup()
                run.assert_not_called()


class DockerCgroupConfigTests(unittest.TestCase):
    @staticmethod
    def configure(root, configured=None, actual=None):
        configured = configured or "/usr/bin/dockerd -H fd:// --containerd=/run/containerd/containerd.sock"
        actual = actual or ["/usr/bin/dockerd", "-H", "fd://", "--containerd=/run/containerd/containerd.sock"]
        proc = root / "proc" / "123"
        proc.mkdir(parents=True, exist_ok=True)
        (proc / "cmdline").write_bytes(("\0".join(actual) + "\0").encode())
        def run(*command, **_kwargs):
            if "--property=MainPID" in command:
                response = "123\n"
            else:
                response = "{ path=/usr/bin/dockerd ; argv[]=" + configured + " ; ignore_errors=no ; }"
            return subprocess.CompletedProcess(command, 0, response)
        with mock.patch.object(RUNTIME.os, "geteuid", return_value=0), \
                mock.patch.object(RUNTIME, "run", side_effect=run):
            return RUNTIME.configure_docker_cgroupfs(root / "docker", root / "proc")

    def test_atomic_merge_preserves_options_features_and_permissions_and_is_idempotent(self):
        original = {"features": {"containerd-snapshotter": True, "unrelated": False},
                    "log-opts": {"max-size": "10m"}, "dns": ["192.0.2.1"],
                    "exec-opts": ["native.option=unchanged=value", "native.cgroupdriver=systemd"]}
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            directory = root / "docker"
            directory.mkdir()
            config = directory / "daemon.json"
            config.write_text(json.dumps(original))
            config.chmod(0o640)
            before = config.stat().st_ino
            self.assertTrue(self.configure(root))
            expected = {**original, "exec-opts": ["native.option=unchanged=value", "native.cgroupdriver=cgroupfs"]}
            self.assertEqual(json.loads(config.read_text()), expected)
            self.assertNotEqual(config.stat().st_ino, before)
            self.assertEqual(config.stat().st_mode & 0o777, 0o640)
            current = config.stat().st_ino
            self.assertFalse(self.configure(root))
            self.assertEqual(config.stat().st_ino, current)
            self.assertEqual(list(directory.iterdir()), [config])

    def test_missing_config_is_published_without_replacing_an_existing_file(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            self.assertTrue(self.configure(root))
            config = root / "docker" / "daemon.json"
            self.assertEqual(json.loads(config.read_text()), {"exec-opts": ["native.cgroupdriver=cgroupfs"]})
            self.assertEqual(config.stat().st_mode & 0o777, 0o644)

    def test_malformed_types_duplicate_keys_and_duplicate_options_are_never_overwritten(self):
        invalid = ["{", "[]", "null", '{"value":NaN}', '{"exec-opts":null}',
                   '{"exec-opts":"native.cgroupdriver=systemd"}', '{"exec-opts":[7]}',
                   '{"exec-opts":["native.cgroupdriver"]}', '{"exec-opts":["native.cgroupdriver="]}',
                   '{"exec-opts":["native.cgroupdriver=other"]}',
                   '{"exec-opts":["native.cgroupdriver=systemd","native.cgroupdriver=systemd"]}',
                   '{"exec-opts":["native.option=a","native.option=b"]}',
                   '{"exec-opts":[],"exec-opts":[]}', '{"features":{"custom":true,"custom":false}}']
        for value in invalid:
            with self.subTest(config=value), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                directory = root / "docker"
                directory.mkdir()
                config = directory / "daemon.json"
                config.write_text(value)
                with self.assertRaises(RuntimeError):
                    self.configure(root)
                self.assertEqual(config.read_text(), value)
                self.assertEqual(list(directory.iterdir()), [config])

    def test_cli_exec_opts_and_custom_config_files_are_refused_in_configured_and_running_daemon(self):
        options = [["--exec-opt", "native.cgroupdriver=systemd"],
                   ["--exec-opt=native.cgroupdriver=cgroupfs"], ["--exec-opt", "native.other=value"],
                   ["--config-file", "/custom/daemon.json"], ["--config-file="],
                   ["--config-file"], ["--config-file=/etc/docker/daemon.json"] * 2]
        for option in options:
            for source in ("configured", "actual"):
                with self.subTest(option=option, source=source), tempfile.TemporaryDirectory() as tmp:
                    root = Path(tmp)
                    arguments = ["/usr/bin/dockerd", *option]
                    kwargs = {source: " ".join(arguments) if source == "configured" else arguments}
                    with self.assertRaises(RuntimeError):
                        self.configure(root, **kwargs)
                    self.assertFalse((root / "docker").exists())
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            self.assertTrue(self.configure(root, configured="/usr/bin/dockerd --log-level debug --config-file=/etc/docker/daemon.json"))

    def test_unresolved_service_expansion_and_indirect_daemon_are_refused(self):
        for command in ["/usr/bin/dockerd $DOCKER_OPTS", "/usr/bin/dockerd %i", "/usr/bin/dockerd \\x20",
                        "/usr/bin/dockerd --log-level 'debug",
                        "/bin/sh -c dockerd", "/usr/bin/dockerd ; argv[]=/usr/bin/dockerd"]:
            with self.subTest(command=command), tempfile.TemporaryDirectory() as tmp, self.assertRaises(RuntimeError):
                self.configure(Path(tmp), configured=command)

    def test_symlink_hardlink_and_nonregular_config_are_refused_without_changing_targets(self):
        for kind in ("symlink", "hardlink", "directory", "fifo"):
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                directory = root / "docker"
                directory.mkdir()
                target = root / "sentinel.json"
                target.write_text('{"preserve":true}')
                config = directory / "daemon.json"
                if kind == "symlink":
                    config.symlink_to(target)
                elif kind == "hardlink":
                    os.link(target, config)
                elif kind == "directory":
                    config.mkdir()
                else:
                    os.mkfifo(config)
                with self.assertRaises((OSError, RuntimeError)):
                    self.configure(root)
                self.assertEqual(target.read_text(), '{"preserve":true}')

    def test_changed_config_and_racing_new_config_are_preserved(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            directory = root / "docker"
            directory.mkdir()
            config = directory / "daemon.json"
            config.write_text('{"before":true}')
            original_read = RUNTIME.read_docker_config
            reads = 0
            def read(descriptor):
                nonlocal reads
                reads += 1
                if reads == 2:
                    config.write_text('{"new_writer":true}')
                return original_read(descriptor)
            with mock.patch.object(RUNTIME, "read_docker_config", side_effect=read), \
                    self.assertRaisesRegex(RuntimeError, "changed before replacement"):
                self.configure(root)
            self.assertEqual(config.read_text(), '{"new_writer":true}')
            self.assertEqual(list(directory.iterdir()), [config])
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            config = root / "docker" / "daemon.json"
            original_link = os.link
            def publish(*args, **kwargs):
                config.write_text('{"new_writer":true}')
                return original_link(*args, **kwargs)
            with mock.patch.object(RUNTIME.os, "link", side_effect=publish), self.assertRaises(FileExistsError):
                self.configure(root)
            self.assertEqual(config.read_text(), '{"new_writer":true}')
            self.assertEqual(list(config.parent.iterdir()), [config])

    def test_unprivileged_configuration_cannot_read_services_or_write_config(self):
        with mock.patch.object(RUNTIME.os, "geteuid", return_value=1001), \
                mock.patch.object(RUNTIME, "run") as command, self.assertRaisesRegex(RuntimeError, "root ownership"):
            RUNTIME.configure_docker_cgroupfs()
        command.assert_not_called()


class HelmDiagnosticsTests(unittest.TestCase):
    def test_real_collector_retains_all_phases_and_failed_probe_without_credentials(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            source, output = root / "source", root / "output"
            source.mkdir()
            for phase in ("full", "cache-identity", "deadlines"):
                (source / f"port-forward-{phase}.log").write_text(f"{phase}: original transport error\n")
            kubectl = root / "kubectl"
            kubectl.write_text('#!/bin/sh\necho "probe $* csk_fixture-secret Bearer bearer-secret"\nexit 7\n')
            kubectl.chmod(0o755)
            result = subprocess.run(
                [sys.executable, str(ROOT / "scripts/collect-helm-diagnostics.py"),
                 "--source", str(source), "--output", str(output)],
                env={**os.environ, "PATH": tmp + os.pathsep + os.environ["PATH"],
                     "GITHUB_SHA": "candidate", "GITHUB_RUN_ID": "123",
                     "GITHUB_RUN_ATTEMPT": "2", "HELM_SHARD": "2"},
                capture_output=True, text=True,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            report = json.loads((output / "report.json").read_text())
            self.assertEqual(report["candidate_sha"], "candidate")
            self.assertEqual(report["run_attempt"], "2")
            self.assertEqual(report["shard"], "2")
            self.assertEqual(set(report["port_forward"].values()), {"retained"})
            self.assertTrue(all(probe == {"exit_code": 7} for probe in report["commands"].values()))
            for phase in report["port_forward"]:
                self.assertIn(phase + ": original transport error", (output / f"port-forward-{phase}.log").read_text())
            server = (output / "server.log").read_text()
            self.assertNotIn("csk_fixture-secret", server)
            self.assertNotIn("bearer-secret", server)
            self.assertIn("[REDACTED_API_KEY]", server)
            self.assertIn("[REDACTED_BEARER]", server)

    def test_setup_failure_and_timeout_are_explicit(self):
        for failure, expected in [(FileNotFoundError("kubectl missing"), "FileNotFoundError"),
                                  (subprocess.TimeoutExpired(["kubectl"], 30, output=b"partial evidence"), "timeout")]:
            with self.subTest(error=expected), tempfile.TemporaryDirectory() as tmp:
                output = Path(tmp) / "output"
                with mock.patch.object(DIAGNOSTICS.subprocess, "run", side_effect=failure):
                    DIAGNOSTICS.collect(Path(tmp) / "missing", output)
                report = json.loads((output / "report.json").read_text())
                self.assertEqual(set(report["port_forward"].values()), {"not_created"})
                self.assertTrue(all(probe == {"error": expected} for probe in report["commands"].values()))
                self.assertTrue((output / "server.log").read_text())


class RemediationWorkflowTests(unittest.TestCase):
    def test_native_setup_precedes_all_image_operations_and_consumer_smoke(self):
        jobs = yaml.safe_load((ROOT / ".github/workflows/ci.yml").read_text())["jobs"]
        for name, action in [("images", "bake-images"), ("images-arm64", "bake-images"),
                             ("integration", "run-integration"), ("integration-arm64", "run-integration")]:
            steps = jobs[name]["steps"]
            setups = [i for i, step in enumerate(steps) if step.get("run") == "python3 scripts/setup-ci-docker.py"]
            self.assertEqual(len(setups), 1, name)
            setup = setups[0]
            use = next(i for i, step in enumerate(steps) if step.get("uses") == "./.github/actions/" + action)
            self.assertLess(setup, use, name)
            self.assertNotIn("if", steps[setup], name)
            self.assertFalse(steps[setup].get("continue-on-error", False), name)
            if action == "run-integration":
                self.assertEqual(steps[use]["with"]["verify-native-stress"], "true", name)
        action = yaml.safe_load((ROOT / ".github/actions/run-integration/action.yml").read_text())
        inner = action["runs"]["steps"]
        product = next(i for i, step in enumerate(inner) if step.get("with", {}).get("name") == "product-${{ inputs.arch }}")
        smoke = next(i for i, step in enumerate(inner) if step.get("name") == "Verify native resource stress runtime")
        run = next(i for i, step in enumerate(inner) if step.get("name") == "Run ${{ inputs.recipe }}")
        self.assertLess(product, smoke)
        self.assertLess(smoke, run)
        self.assertIn("bash build/stress/smoke.sh docker", inner[smoke]["run"])
        self.assertNotIn("continue-on-error", inner[smoke])

    def test_coverage_pins_runtime_before_preserving_store_configuration_and_loading_images(self):
        steps = yaml.safe_load((ROOT / ".github/workflows/ci.yml").read_text())["jobs"]["coverage-ratchets"]["steps"]
        setups = [i for i, step in enumerate(steps) if step.get("run") == "python3 scripts/setup-ci-docker.py"]
        self.assertEqual(len(setups), 1)
        setup = setups[0]
        configure = next(i for i, step in enumerate(steps) if 'jq \'.features["containerd-snapshotter"] = true\'' in step.get("run", ""))
        load = next(i for i, step in enumerate(steps) if step.get("uses") == "./.github/actions/load-docker-images")
        self.assertLess(setup, configure)
        self.assertLess(configure, load)
        self.assertNotIn("if", steps[setup])
        self.assertFalse(steps[setup].get("continue-on-error", False))
        command = steps[configure]["run"]
        self.assertIn('if [ -s "$conf" ]; then current="$(sudo cat "$conf")"', command)
        self.assertIn('printf \'%s\' "$current" | jq', command)
        self.assertIn("sudo systemctl restart docker", command)
        self.assertNotIn("need=29.0", command)
        self.assertNotIn("apt-get", command)
        # Execute the workflow's real JSON transform. A replacement object here
        # would silently discard the cgroup driver before the coverage restart.
        expressions = re.findall(r"\bjq '([^']+)'", command)
        self.assertEqual(len(expressions), 1)
        config = RUNTIME.merged_docker_config(json.dumps({
            "features": {"unrelated": False}, "log-driver": "local",
            "exec-opts": ["native.cgroupdriver=systemd", "native.other=preserved"],
        }))
        transformed = subprocess.run(["jq", expressions[0]], input=json.dumps(config),
                                     capture_output=True, text=True, check=True, timeout=5)
        observed = json.loads(transformed.stdout)
        self.assertEqual(observed, {**config, "features": {"unrelated": False, "containerd-snapshotter": True}})
        self.assertIn("native.cgroupdriver=cgroupfs", observed["exec-opts"])

    def test_same_attempt_diagnostics_are_uploaded_before_cleanup_even_on_failure(self):
        steps = yaml.safe_load((ROOT / ".github/workflows/ci.yml").read_text())["jobs"]["helm-integration-test"]["steps"]
        collect = next(i for i, step in enumerate(steps) if step.get("name") == "Collect Helm integration diagnostics")
        upload = next(i for i, step in enumerate(steps) if step.get("name") == "Upload Helm integration diagnostics")
        cleanup = next(i for i, step in enumerate(steps) if step.get("name") == "Stop port-forward")
        self.assertLess(collect, upload)
        self.assertLess(upload, cleanup)
        for index in (collect, upload, cleanup):
            self.assertEqual(steps[index]["if"], "always()")
        self.assertEqual(steps[upload]["with"]["if-no-files-found"], "error")
        self.assertIn("${{ matrix.shard }}-${{ github.run_attempt }}", steps[upload]["with"]["name"])
        launches = [step["run"] for step in steps if "kubectl port-forward" in step.get("run", "")]
        self.assertEqual(len(launches), 3)
        for command, phase in zip(launches, ("full", "cache-identity", "deadlines")):
            self.assertIn(f"/tmp/port-forward-{phase}.log", command)


if __name__ == "__main__":
    unittest.main()
