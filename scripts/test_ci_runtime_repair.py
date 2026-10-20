"""Regression checks for live runtime verification and retained Helm evidence."""

import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
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


RUNTIME = load("ci_arm64_runtime", "setup-ci-arm64-docker.py")
DIAGNOSTICS = load("helm_diagnostics", "collect-helm-diagnostics.py")


class NativeRuntimeTests(unittest.TestCase):
    def run_setup(self, info=None, install_fails=False):
        commands = []
        info = info or {
            "ServerVersion": RUNTIME.EXPECTED["ServerVersion"],
            "ContainerdCommit": {"ID": RUNTIME.EXPECTED["ContainerdCommit"]},
            "RuncCommit": {"ID": RUNTIME.EXPECTED["RuncCommit"]},
        }

        def run(*command, **kwargs):
            commands.append((command, kwargs))
            if install_fails and "install" in command and "apt-get" in command:
                raise subprocess.CalledProcessError(100, command)
            return subprocess.CompletedProcess(command, 0, json.dumps(info))

        with mock.patch.object(RUNTIME.platform, "system", return_value="Linux"), \
                mock.patch.object(RUNTIME.platform, "machine", return_value="aarch64"), \
                mock.patch.object(RUNTIME.platform, "freedesktop_os_release", return_value={
                    "ID": "ubuntu", "VERSION_ID": "26.04", "VERSION_CODENAME": "resolute"}), \
                mock.patch.object(RUNTIME, "run", side_effect=run), \
                contextlib.redirect_stdout(io.StringIO()):
            RUNTIME.setup()
        return commands

    def test_exact_packages_are_installed_before_running_daemon_verification(self):
        commands = self.run_setup()
        args = [command for command, _ in commands]
        install = next(command for command in args if "apt-get" in command and "install" in command)
        self.assertIn("docker-ce=" + RUNTIME.ENGINE_PACKAGE, install)
        self.assertIn("docker-ce-cli=" + RUNTIME.ENGINE_PACKAGE, install)
        self.assertIn("containerd.io=" + RUNTIME.CONTAINERD_PACKAGE, install)
        self.assertIn("Dpkg::Options::=--force-confold", install)
        self.assertNotIn("--allow-downgrades", install)
        restart = next(command for command in args if "restart" in command)
        verify = next(command for command in args if command[:2] == ("docker", "info"))
        self.assertLess(args.index(install), args.index(restart))
        self.assertLess(args.index(restart), args.index(verify))
        source = next(kwargs["input"] for command, kwargs in commands if "tee" in command)
        self.assertIn("arch=arm64 signed-by=/etc/apt/keyrings/docker.asc", source)
        self.assertIn("https://download.docker.com/linux/ubuntu resolute stable", source)

    def test_successful_package_install_cannot_hide_old_or_missing_daemon_components(self):
        for field in RUNTIME.EXPECTED:
            with self.subTest(field=field):
                info = {
                    "ServerVersion": "29.9.1",
                    "ContainerdCommit": {"ID": RUNTIME.EXPECTED["ContainerdCommit"]},
                    "RuncCommit": {"ID": RUNTIME.EXPECTED["RuncCommit"]},
                }
                info[field] = "29.4.2" if field == "ServerVersion" else {}
                with self.assertRaisesRegex(RuntimeError, "running Docker runtime"):
                    self.run_setup(info)

    def test_install_failure_is_fatal(self):
        with self.assertRaises(subprocess.CalledProcessError):
            self.run_setup(install_fails=True)

    def test_non_native_host_fails_before_privileged_commands(self):
        with mock.patch.object(RUNTIME.platform, "freedesktop_os_release", return_value={}), \
                mock.patch.object(RUNTIME.platform, "system", return_value="Darwin"), \
                mock.patch.object(RUNTIME, "run") as run:
            with self.assertRaisesRegex(RuntimeError, "Ubuntu 26.04 ARM64"):
                RUNTIME.setup()
            run.assert_not_called()


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
        for name, action in [("images-arm64", "bake-images"), ("integration-arm64", "run-integration")]:
            steps = jobs[name]["steps"]
            setup = next(i for i, step in enumerate(steps) if step.get("run") == "python3 scripts/setup-ci-arm64-docker.py")
            use = next(i for i, step in enumerate(steps) if step.get("uses") == "./.github/actions/" + action)
            self.assertLess(setup, use)
            self.assertNotIn("if", steps[setup])
            self.assertFalse(steps[setup].get("continue-on-error", False))
        self.assertEqual(steps[use]["with"]["verify-native-stress"], "true")
        action = yaml.safe_load((ROOT / ".github/actions/run-integration/action.yml").read_text())
        inner = action["runs"]["steps"]
        product = next(i for i, step in enumerate(inner) if step.get("with", {}).get("name") == "product-${{ inputs.arch }}")
        smoke = next(i for i, step in enumerate(inner) if step.get("name") == "Verify native resource stress runtime")
        run = next(i for i, step in enumerate(inner) if step.get("name") == "Run ${{ inputs.recipe }}")
        self.assertLess(product, smoke)
        self.assertLess(smoke, run)
        self.assertIn("bash build/stress/smoke.sh docker", inner[smoke]["run"])
        self.assertNotIn("continue-on-error", inner[smoke])

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
