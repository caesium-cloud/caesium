"""Hermetic tests for the just-cli container wrapper.

These drive scripts/caesium-cli-wrapper.sh through generate + a stub
container CLI. They never call a live Docker daemon or just build/integration
recipes.
"""

from __future__ import annotations

import os
import socket
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts/caesium-cli-wrapper.sh"
JUSTFILE = ROOT / "justfile"
IMAGE = "caesiumcloud/caesium:wrapper-test"


def bind_unix_socket(path: Path) -> socket.socket:
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.exists():
        path.unlink()
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    try:
        sock.bind(str(path))
    except OSError:
        sock.close()
        raise
    return sock


class WrapperTests(unittest.TestCase):
    def setUp(self):
        # Keep paths short: macOS AF_UNIX sun_path is 104 bytes.
        self.tmp = Path(tempfile.mkdtemp(prefix="c8w-", dir="/tmp"))
        self.home = self.tmp / "home"
        self.home.mkdir()
        self.bin = self.tmp / "bin"
        self.bin.mkdir()
        self.stub_log = self.tmp / "docker.args"
        self.wrapper = self.tmp / "caesium"
        stub = self.bin / "docker"
        stub.write_text(
            "#!/bin/sh\n"
            'log="${CLI_WRAPPER_STUB_LOG}"\n'
            ': > "$log"\n'
            'for a in "$@"; do\n'
            '  printf "%s\\n" "$a" >> "$log"\n'
            "done\n"
            "exit 0\n",
            encoding="utf-8",
        )
        stub.chmod(0o755)
        self.socks: list[socket.socket] = []
        self._generate()

    def tearDown(self):
        for sock in self.socks:
            try:
                sock.close()
            except OSError:
                pass
        # tempfile dirs are left for failed-test inspection only on demand;
        # always clean up so CI does not leak sockets.
        for child in sorted(self.tmp.rglob("*"), reverse=True):
            try:
                if child.is_symlink() or child.is_file():
                    child.unlink()
                elif child.is_dir():
                    child.rmdir()
            except OSError:
                pass
        try:
            self.tmp.rmdir()
        except OSError:
            pass

    def _generate(self, podman: str = "false", container_cli: str = "docker"):
        result = subprocess.run(
            [
                "sh",
                str(SCRIPT),
                "generate",
                "--output",
                str(self.wrapper),
                "--image",
                IMAGE,
                "--container-cli",
                container_cli,
                "--podman",
                podman,
                "--tag",
                "wrapper-test",
            ],
            capture_output=True,
            text=True,
            cwd=str(ROOT),
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.wrapper.is_file())
        self.assertTrue(os.access(self.wrapper, os.X_OK))

    def _env(self, extra=None):
        env = {
            "PATH": f"{self.bin}{os.pathsep}{os.environ.get('PATH', '')}",
            "HOME": str(self.home),
            "CLI_WRAPPER_STUB_LOG": str(self.stub_log),
            "LANG": os.environ.get("LANG", "C"),
        }
        if extra:
            env.update(extra)
        return env

    def _run(self, args, extra=None):
        return subprocess.run(
            [str(self.wrapper), *args],
            capture_output=True,
            text=True,
            cwd=str(self.tmp),
            env=self._env(extra),
        )

    def _args(self):
        self.assertTrue(self.stub_log.is_file(), "stub container CLI was not invoked")
        return self.stub_log.read_text(encoding="utf-8").splitlines()

    def _socket(self, rel="run/docker.sock"):
        path = self.tmp / rel
        self.socks.append(bind_unix_socket(path))
        return path

    def test_justfile_cli_invokes_generator(self):
        text = JUSTFILE.read_text(encoding="utf-8")
        self.assertIn("scripts/caesium-cli-wrapper.sh", text)
        self.assertIn("/scripts/caesium-cli-wrapper.sh\" generate", text)
        self.assertNotIn('cat > "$out_dir/caesium" <<WRAPPER', text)

    def test_generated_wrapper_documents_runtime_and_trust_boundary(self):
        text = self.wrapper.read_text(encoding="utf-8")
        self.assertIn("DOCKER_HOST", text)
        self.assertIn("CAESIUM_SOCK", text)
        self.assertIn("/var/run/docker.sock", text)
        self.assertIn("KUBECONFIG", text)
        self.assertIn("full daemon access", text)
        self.assertIn(IMAGE, text)
        self.assertIn("caesium_cli_run", text)
        # dash rejects `for x in ; do` when no CAESIUM_* vars are set.
        self.assertIn("while IFS= read -r name", text)

    def test_missing_socket_dev_fails_before_docker_run(self):
        missing = self.tmp / "nope" / "docker.sock"
        result = self._run(
            ["dev", "--once", "--path", "job.yaml"],
            extra={"DOCKER_HOST": f"unix://{missing}"},
        )
        self.assertNotEqual(result.returncode, 0)
        err = result.stderr
        self.assertIn("docker.sock", err)
        self.assertIn("DOCKER_HOST", err)
        self.assertFalse(self.stub_log.exists())

    def test_missing_caesium_sock_mentions_override(self):
        missing = self.tmp / "missing.sock"
        result = self._run(
            ["dev", "--once"],
            extra={"CAESIUM_SOCK": str(missing)},
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(str(missing), result.stderr)
        self.assertIn("CAESIUM_SOCK", result.stderr)
        self.assertIn("DOCKER_HOST", result.stderr)

    def test_lint_without_socket_still_runs_container(self):
        missing = self.tmp / "nope" / "docker.sock"
        result = self._run(
            ["job", "lint", "--path", "jobs/"],
            extra={"DOCKER_HOST": f"unix://{missing}"},
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        args = self._args()
        self.assertEqual(args[0], "run")
        joined = "\n".join(args)
        self.assertNotIn("docker.sock", joined)
        self.assertIn("-v", args)
        work_mounts = [a for a in args if a.endswith(":/work")]
        self.assertEqual(len(work_mounts), 1, args)
        self.assertEqual(Path(work_mounts[0][: -len(":/work")]).resolve(), self.tmp.resolve())
        self.assertIn("--entrypoint", args)
        self.assertIn("/bin/caesium", args)
        self.assertIn(IMAGE, args)
        self.assertIn("job", args)
        self.assertIn("lint", args)

    def test_help_without_socket_still_runs_container(self):
        missing = self.tmp / "nope" / "docker.sock"
        result = self._run(["--help"], extra={"DOCKER_HOST": f"unix://{missing}"})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("--help", self._args())

    def test_docker_host_unix_socket_is_mounted(self):
        sock = self._socket("custom.sock")
        result = self._run(
            ["dev", "--once"],
            extra={"DOCKER_HOST": f"unix://{sock}"},
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        args = self._args()
        self.assertIn(f"{sock}:/var/run/docker.sock", args)
        self.assertIn("DOCKER_HOST=unix:///var/run/docker.sock", args)
        self.assertIn("dev", args)
        self.assertIn("--once", args)

    def test_caesium_sock_used_when_docker_host_unset(self):
        sock = self._socket("caesium.sock")
        result = self._run(["dev", "--once"], extra={"CAESIUM_SOCK": str(sock)})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f"{sock}:/var/run/docker.sock", self._args())

    def test_docker_desktop_home_socket_detected(self):
        sock_path = self.home / ".docker" / "run" / "docker.sock"
        self.socks.append(bind_unix_socket(sock_path))
        result = self._run(["dev", "--once"])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f"{sock_path}:/var/run/docker.sock", self._args())

    def test_user_owned_writable_socket_keeps_user_mapping(self):
        sock = self._socket("user.sock")
        os.chmod(sock, 0o600)
        result = self._run(
            ["dev", "--once"],
            extra={
                "CAESIUM_SOCK": str(sock),
                "CAESIUM_CLI_SOCKET_IN_CONTAINER_WRITABLE": "1",
            },
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        uid = os.getuid()
        gid = os.getgid()
        args = self._args()
        self.assertIn(f"--user={uid}:{gid}", args)
        self.assertNotIn("--user=0:0", args)

    def test_docker_desktop_vm_socket_falls_back_to_root(self):
        # Host-owned and writable, but the Linux VM presents 0:0/0660.
        sock = self._socket("desktop.sock")
        os.chmod(sock, 0o600)
        result = self._run(
            ["dev", "--once"],
            extra={
                "CAESIUM_SOCK": str(sock),
                "CAESIUM_CLI_SOCKET_IN_CONTAINER_WRITABLE": "0",
            },
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        args = self._args()
        self.assertIn("--user=0:0", args)
        uid = os.getuid()
        gid = os.getgid()
        self.assertNotIn(f"--user={uid}:{gid}", args)

    def test_unwritable_socket_falls_back_to_root(self):
        sock = self._socket("root-only.sock")
        os.chmod(sock, 0o000)
        result = self._run(["dev", "--once"], extra={"CAESIUM_SOCK": str(sock)})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("--user=0:0", self._args())

    def test_kubeconfig_env_is_mounted_for_engine_loader(self):
        sock = self._socket("k8s.sock")
        kube = self.tmp / "my kube config"
        kube.write_text("apiVersion: v1\nkind: Config\n", encoding="utf-8")
        result = self._run(
            ["dev", "--once"],
            extra={
                "CAESIUM_SOCK": str(sock),
                "KUBECONFIG": str(kube),
                "CAESIUM_CLI_SOCKET_IN_CONTAINER_WRITABLE": "1",
            },
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        args = self._args()
        self.assertIn(f"{kube}:/caesium-kube/.kube/config:ro", args)
        self.assertIn("KUBECONFIG=/caesium-kube/.kube/config", args)
        self.assertIn("CAESIUM_KUBERNETES_CONFIG=/caesium-kube", args)

    def test_default_home_kubeconfig_is_mounted(self):
        sock = self._socket("k8s.sock")
        kube = self.home / ".kube" / "config"
        kube.parent.mkdir()
        kube.write_text("apiVersion: v1\nkind: Config\n", encoding="utf-8")
        result = self._run(
            ["dev", "--once"],
            extra={
                "CAESIUM_SOCK": str(sock),
                "CAESIUM_CLI_SOCKET_IN_CONTAINER_WRITABLE": "1",
            },
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        args = self._args()
        self.assertIn(f"{kube}:/caesium-kube/.kube/config:ro", args)
        self.assertIn("KUBECONFIG=/caesium-kube/.kube/config", args)
        self.assertIn("CAESIUM_KUBERNETES_CONFIG=/caesium-kube", args)

    def test_kubernetes_config_dir_is_preferred_after_kubeconfig_unset(self):
        sock = self._socket("k8s.sock")
        cfg_dir = self.tmp / "k8s-home"
        kube = cfg_dir / ".kube" / "config"
        kube.parent.mkdir(parents=True)
        kube.write_text("apiVersion: v1\nkind: Config\n", encoding="utf-8")
        result = self._run(
            ["dev", "--once"],
            extra={
                "CAESIUM_SOCK": str(sock),
                "CAESIUM_KUBERNETES_CONFIG": str(cfg_dir),
                "CAESIUM_CLI_SOCKET_IN_CONTAINER_WRITABLE": "1",
            },
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        args = self._args()
        self.assertIn(f"{kube}:/caesium-kube/.kube/config:ro", args)
        self.assertIn("CAESIUM_KUBERNETES_CONFIG=/caesium-kube", args)

    def test_kubeconfig_layout_when_running_as_root(self):
        sock = self._socket("k8s.sock")
        os.chmod(sock, 0o000)
        kube = self.tmp / "my.kubeconfig"
        kube.write_text("apiVersion: v1\n", encoding="utf-8")
        result = self._run(
            ["dev", "--once"],
            extra={"CAESIUM_SOCK": str(sock), "KUBECONFIG": str(kube)},
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        args = self._args()
        self.assertIn("--user=0:0", args)
        self.assertIn(f"{kube}:/caesium-kube/.kube/config:ro", args)
        self.assertIn("KUBECONFIG=/caesium-kube/.kube/config", args)
        self.assertIn("CAESIUM_KUBERNETES_CONFIG=/caesium-kube", args)

    def test_check_images_requires_socket(self):
        missing = self.tmp / "nope.sock"
        result = self._run(
            ["test", "--path", "jobs/", "--check-images"],
            extra={"DOCKER_HOST": f"unix://{missing}"},
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.stub_log.exists())

    def test_test_path_without_images_does_not_require_socket(self):
        missing = self.tmp / "nope.sock"
        result = self._run(
            ["test", "--path", "jobs/"],
            extra={"DOCKER_HOST": f"unix://{missing}"},
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("test", self._args())

    def test_podman_xdg_socket_detected(self):
        self._generate(podman="true", container_cli="docker")
        runtime = self.tmp / "xdg"
        sock_path = runtime / "podman" / "podman.sock"
        self.socks.append(bind_unix_socket(sock_path))
        result = self._run(
            ["dev", "--once"],
            extra={"CAESIUM_PODMAN": "true", "XDG_RUNTIME_DIR": str(runtime)},
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        args = self._args()
        self.assertIn(f"{sock_path}:/var/run/docker.sock", args)
        self.assertIn("CAESIUM_PODMAN_URI=unix:///var/run/docker.sock", args)

    def test_dash_lint_and_missing_socket(self):
        dash = "/bin/dash"
        if not os.path.isfile(dash):
            self.skipTest("dash is not installed")
        missing = self.tmp / "nope.sock"
        env = self._env({"DOCKER_HOST": f"unix://{missing}"})
        lint = subprocess.run(
            [dash, str(self.wrapper), "job", "lint", "--path", "jobs/"],
            capture_output=True,
            text=True,
            cwd=str(self.tmp),
            env=env,
        )
        self.assertEqual(lint.returncode, 0, lint.stderr)
        self.assertIn("job", self._args())
        if self.stub_log.exists():
            self.stub_log.unlink()
        dev = subprocess.run(
            [dash, str(self.wrapper), "dev", "--once"],
            capture_output=True,
            text=True,
            cwd=str(self.tmp),
            env=env,
        )
        self.assertNotEqual(dev.returncode, 0)
        self.assertIn("DOCKER_HOST", dev.stderr)
        self.assertFalse(self.stub_log.exists())

    def test_caesium_env_forwarded(self):
        sock = self._socket("env.sock")
        result = self._run(
            ["job", "lint"],
            extra={"CAESIUM_SOCK": str(sock), "CAESIUM_AUTH_MODE": "none"},
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("CAESIUM_AUTH_MODE", self._args())


class ChooseUserFlagsTests(unittest.TestCase):
    def _choose(self, host_uid, host_gid, sock_uid, sock_gid, mode, writable):
        script = f"""
CAESIUM_CLI_WRAPPER_SOURCED=1
. "{SCRIPT}"
caesium_cli_choose_user_flags {host_uid} {host_gid} {sock_uid} {sock_gid} {mode} {writable}
"""
        result = subprocess.run(
            ["sh", "-c", script],
            capture_output=True,
            text=True,
            cwd=str(ROOT),
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout

    def test_user_owned_writable(self):
        self.assertEqual(self._choose(501, 20, 501, 20, "600", 1), "--user=501:20")

    def test_linux_docker_group(self):
        self.assertEqual(
            self._choose(1000, 1000, 0, 999, "660", 0),
            "--user=1000:1000 --group-add=999",
        )

    def test_root_only_socket(self):
        self.assertEqual(self._choose(1000, 1000, 0, 0, "600", 0), "--user=0:0")

    def test_group_writable_four_digit_mode(self):
        self.assertEqual(
            self._choose(1000, 1000, 0, 50, "0660", 0),
            "--user=1000:1000 --group-add=50",
        )


class NeedsRuntimeTests(unittest.TestCase):
    def _needs(self, *args):
        quoted = " ".join("'" + a.replace("'", "'\\''") + "'" for a in args)
        script = f"""
CAESIUM_CLI_WRAPPER_SOURCED=1
. "{SCRIPT}"
if caesium_cli_needs_runtime {quoted}; then echo yes; else echo no; fi
"""
        result = subprocess.run(
            ["sh", "-c", script],
            capture_output=True,
            text=True,
            cwd=str(ROOT),
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.strip()

    def test_commands(self):
        cases = [
            (["--help"], "no"),
            (["job", "lint", "--path", "jobs/"], "no"),
            (["job", "preview"], "no"),
            (["dev", "--once"], "yes"),
            (["dev", "--help"], "no"),
            (["reproduce", "--job-id", "x"], "yes"),
            (["test", "--path", "jobs/"], "no"),
            (["test", "--check-images"], "yes"),
            (["test", "--scenario", "./harness"], "yes"),
            (["start"], "yes"),
        ]
        for args, want in cases:
            with self.subTest(args=args):
                self.assertEqual(self._needs(*args), want)


class ModeWritableTests(unittest.TestCase):
    def _group_writable(self, mode):
        script = f"""
CAESIUM_CLI_WRAPPER_SOURCED=1
. "{SCRIPT}"
if caesium_cli_mode_group_writable {mode}; then echo yes; else echo no; fi
"""
        result = subprocess.run(
            ["sh", "-c", script],
            capture_output=True,
            text=True,
            cwd=str(ROOT),
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.strip()

    def test_modes(self):
        self.assertEqual(self._group_writable("660"), "yes")
        self.assertEqual(self._group_writable("600"), "no")
        self.assertEqual(self._group_writable("640"), "no")
        self.assertEqual(self._group_writable("666"), "yes")
        self.assertEqual(self._group_writable("0770"), "yes")


class ContainerSocketProbeTests(unittest.TestCase):
    def _probe(self, writable_env):
        script = f"""
CAESIUM_CLI_WRAPPER_SOURCED=1
. "{SCRIPT}"
CAESIUM_CLI_SOCKET_IN_CONTAINER_WRITABLE={writable_env}
if caesium_cli_container_can_write_socket /tmp/sock img docker --user=501:20; then echo yes; else echo no; fi
"""
        result = subprocess.run(
            ["sh", "-c", script],
            capture_output=True,
            text=True,
            cwd=str(ROOT),
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.strip()

    def test_env_override(self):
        self.assertEqual(self._probe("0"), "no")
        self.assertEqual(self._probe("1"), "yes")

    def test_live_docker_probe_when_image_present(self):
        if subprocess.run(["docker", "info"], capture_output=True).returncode != 0:
            self.skipTest("docker daemon not available")
        if subprocess.run(
            ["docker", "image", "inspect", "alpine:3.23"],
            capture_output=True,
        ).returncode != 0:
            self.skipTest("alpine:3.23 is not present locally")
        sock = Path("/var/run/docker.sock")
        desktop = Path.home() / ".docker/run/docker.sock"
        if not sock.exists() and desktop.exists():
            sock = desktop
        if not sock.exists():
            self.skipTest("no docker socket on this host")
        uid = os.getuid()
        gid = os.getgid()
        probe = subprocess.run(
            [
                "docker",
                "run",
                "--rm",
                "-v",
                f"{sock}:/var/run/docker.sock",
                "--user",
                f"{uid}:{gid}",
                "--entrypoint",
                "/bin/sh",
                "alpine:3.23",
                "-c",
                "test -w /var/run/docker.sock",
            ],
            capture_output=True,
            text=True,
        )
        script = f"""
CAESIUM_CLI_WRAPPER_SOURCED=1
. "{SCRIPT}"
if caesium_cli_container_can_write_socket {sock} alpine:3.23 docker --user={uid}:{gid}; then echo yes; else echo no; fi
"""
        result = subprocess.run(
            ["sh", "-c", script],
            capture_output=True,
            text=True,
            cwd=str(ROOT),
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        want = "yes" if probe.returncode == 0 else "no"
        self.assertEqual(result.stdout.strip(), want, result.stderr)


if __name__ == "__main__":
    unittest.main()
