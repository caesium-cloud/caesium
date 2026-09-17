"""Generated-wrapper tests using a stub container CLI and real host kubectl.

The optional socket probe also exercises Docker when the image is available.
"""

from __future__ import annotations

import json
import os
import signal
import sys
import time
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
        self.kube_capture = self.tmp / "captured-kubeconfig"
        self.wrapper = self.tmp / "caesium"
        stub = self.bin / "docker"
        stub.write_text(
            "#!/bin/sh\n"
            'log="${CLI_WRAPPER_STUB_LOG}"\n'
            ': > "$log"\n'
            'for a in "$@"; do\n'
            '  printf "%s\\n" "$a" >> "$log"\n'
            '  case "$a" in\n'
            '    *:/caesium-kube/.kube/config:ro)\n'
            '      cp "${a%:/caesium-kube/.kube/config:ro}" "$CLI_WRAPPER_KUBE_CAPTURE" ;;\n'
            '  esac\n'
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
        tmpdir = self.tmp / "tmp"
        tmpdir.mkdir(exist_ok=True)
        env = {
            "PATH": f"{self.bin}{os.pathsep}{os.environ.get('PATH', '')}",
            "HOME": str(self.home),
            "TMPDIR": str(tmpdir),
            "CLI_WRAPPER_STUB_LOG": str(self.stub_log),
            "CLI_WRAPPER_KUBE_CAPTURE": str(self.kube_capture),
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

    def _kube_mount_src(self, args=None):
        args = args if args is not None else self._args()
        suffix = ":/caesium-kube/.kube/config:ro"
        for item in args:
            if item.endswith(suffix):
                return Path(item[: -len(suffix)])
        self.fail(f"no kubeconfig mount in {args}")

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
        mounted = self._kube_mount_src(args)
        self.assertTrue(self.kube_capture.is_file())
        self.assertFalse(mounted.parent.exists(), mounted)
        self.assertIn("KUBECONFIG=/caesium-kube/.kube/config", args)
        self.assertIn("CAESIUM_KUBERNETES_CONFIG=/caesium-kube", args)
        self.assertEqual(json.loads(self.kube_capture.read_text())["kind"], "Config")

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
        mounted = self._kube_mount_src(args)
        self.assertTrue(self.kube_capture.is_file())
        self.assertFalse(mounted.parent.exists(), mounted)
        self.assertIn("KUBECONFIG=/caesium-kube/.kube/config", args)
        self.assertIn("CAESIUM_KUBERNETES_CONFIG=/caesium-kube", args)
        self.assertEqual(json.loads(self.kube_capture.read_text())["kind"], "Config")

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
        mounted = self._kube_mount_src(args)
        self.assertTrue(self.kube_capture.is_file())
        self.assertFalse(mounted.parent.exists(), mounted)
        self.assertIn("CAESIUM_KUBERNETES_CONFIG=/caesium-kube", args)
        self.assertEqual(json.loads(self.kube_capture.read_text())["kind"], "Config")

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
        mounted = self._kube_mount_src(args)
        self.assertTrue(self.kube_capture.is_file())
        self.assertFalse(mounted.parent.exists(), mounted)
        self.assertIn("KUBECONFIG=/caesium-kube/.kube/config", args)
        self.assertIn("CAESIUM_KUBERNETES_CONFIG=/caesium-kube", args)

    def test_external_ca_file_is_flattened_into_mounted_kubeconfig(self):
        sock = self._socket("k8s.sock")
        kube_dir = self.tmp / "cluster"
        kube_dir.mkdir()
        ca = kube_dir / "ca.crt"
        user_crt = kube_dir / "user.crt"
        user_key = kube_dir / "user.key"
        ca.write_bytes(b"CA CERT\n")
        user_crt.write_bytes(b"USER CERT\n")
        user_key.write_bytes(b"USER KEY\n")
        kube = kube_dir / "config"
        kube.write_text(
            "apiVersion: v1\n"
            "kind: Config\n"
            "clusters:\n"
            "- cluster:\n"
            "    certificate-authority: ca.crt\n"
            "    server: https://127.0.0.1:1\n"
            "  name: test\n"
            "contexts:\n"
            "- context:\n"
            "    cluster: test\n"
            "    user: test\n"
            "  name: test\n"
            "current-context: test\n"
            "users:\n"
            "- name: test\n"
            "  user:\n"
            "    client-certificate: user.crt\n"
            "    client-key: user.key\n",
            encoding="utf-8",
        )
        result = self._run(
            ["dev", "--once"],
            extra={
                "CAESIUM_SOCK": str(sock),
                "KUBECONFIG": str(kube),
                "CAESIUM_CLI_SOCKET_IN_CONTAINER_WRITABLE": "1",
            },
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        mounted = self.kube_capture.read_text(encoding="utf-8")
        self.assertFalse(self._kube_mount_src().parent.exists())
        self.assertNotIn("certificate-authority:", mounted)
        self.assertNotIn("client-certificate:", mounted)
        self.assertNotIn("client-key:", mounted)
        self.assertEqual(json.loads(mounted)["clusters"][0]["cluster"]["certificate-authority-data"], "Q0EgQ0VSVAo=")
        self.assertEqual(json.loads(mounted)["users"][0]["user"]["client-certificate-data"], "VVNFUiBDRVJUCg==")
        self.assertEqual(json.loads(mounted)["users"][0]["user"]["client-key-data"], "VVNFUiBLRVkK")

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


    def _lifecycle_stub(self):
        # The barrier delays the actual config read until another invocation has
        # finished, reproducing Docker's interval between argv and bind-mount use.
        stub = self.bin / "docker"
        stub.write_text(f"#!{sys.executable}\n" + '''
import json
import os
from pathlib import Path
import signal
import sys
import time
suffix = ":/caesium-kube/.kube/config:ro"
src = Path(next(a[:-len(suffix)] for a in sys.argv[1:] if a.endswith(suffix)))
root = Path(os.environ["PROBE_ROOT"])
name = os.environ["PROBE_NAME"]
def capture():
    return {"source": str(src), "config": json.loads(src.read_text()),
            "mode": src.stat().st_mode & 0o777,
            "directory_mode": src.parent.stat().st_mode & 0o777}
def finish_signal(number, frame):
    result = capture()
    result["signal"] = number
    (root / (name + ".capture")).write_text(json.dumps(result))
    raise SystemExit(128 + number)
for number in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
    signal.signal(number, finish_signal)
(root / (name + ".ready")).write_text(str(src))
if os.environ.get("PROBE_WAIT"):
    deadline = time.monotonic() + 10
    while not (root / (name + ".release")).exists():
        if time.monotonic() > deadline:
            raise SystemExit("barrier timeout")
        time.sleep(0.01)
result = capture()
if os.environ.get("PROBE_STDIN"):
    result["stdin"] = sys.stdin.read()
(root / (name + ".capture")).write_text(json.dumps(result))
raise SystemExit(int(os.environ.get("PROBE_EXIT", "0")))
''')
        stub.chmod(0o755)

    def _lifecycle_env(self, name, **extra):
        kube = self.tmp / (name + ".config")
        kube.write_text(json.dumps({
            "apiVersion": "v1", "kind": "Config", "current-context": name,
            "clusters": [{"name": name, "cluster": {"server": "https://127.0.0.1:1"}}],
            "contexts": [{"name": name, "context": {"cluster": name, "user": name}}],
            "users": [{"name": name, "user": {"token": "synthetic-" + name}}],
        }))
        return self._env({"KUBECONFIG": str(kube), "PROBE_ROOT": str(self.tmp),
                          "PROBE_NAME": name, "DOCKER_HOST": "tcp://unused.invalid:2375",
                          **extra})

    def _wait_ready(self, process, name):
        deadline = time.monotonic() + 10
        while not (self.tmp / (name + ".ready")).exists():
            self.assertIsNone(process.poll(), "container CLI exited before barrier")
            self.assertLess(time.monotonic(), deadline, "container CLI barrier timeout")
            time.sleep(0.01)
        return Path((self.tmp / (name + ".ready")).read_text())

    def test_concurrent_invocations_keep_separate_configs_until_child_exit(self):
        self._lifecycle_stub()
        first = subprocess.Popen([str(self.wrapper), "dev", "--once"],
                                 env=self._lifecycle_env("A", PROBE_WAIT="1"),
                                 stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            first_path = self._wait_ready(first, "A")
            second = subprocess.run([str(self.wrapper), "dev", "--once"],
                                    env=self._lifecycle_env("B"), capture_output=True,
                                    text=True, timeout=10)
            self.assertEqual(second.returncode, 0, second.stderr)
            self.assertTrue(first_path.is_file())
            second_capture = json.loads((self.tmp / "B.capture").read_text())
            self.assertFalse(Path(second_capture["source"]).parent.exists())
        finally:
            (self.tmp / "A.release").touch()
            out, err = first.communicate(timeout=10)
        self.assertEqual(first.returncode, 0, err)
        first_capture = json.loads((self.tmp / "A.capture").read_text())
        self.assertNotEqual(first_capture["source"], second_capture["source"])
        for name, result in (("A", first_capture), ("B", second_capture)):
            self.assertEqual(result["config"]["current-context"], name)
            self.assertEqual(result["config"]["users"][0]["user"]["token"], "synthetic-" + name)
            self.assertEqual(result["mode"], 0o600)
            self.assertEqual(result["directory_mode"], 0o700)
            self.assertFalse(Path(result["source"]).parent.exists())

    def test_child_failure_preserves_status_and_cleans_config(self):
        self._lifecycle_stub()
        result = subprocess.run([str(self.wrapper), "--help"],
                                env=self._lifecycle_env("failure", PROBE_EXIT="37"),
                                capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 37, result.stderr)
        self.assertEqual(list((self.tmp / "tmp").iterdir()), [])

    def test_child_retains_stdin(self):
        self._lifecycle_stub()
        result = subprocess.run([str(self.wrapper), "--help"],
                                env=self._lifecycle_env("stdin", PROBE_STDIN="1"),
                                input="synthetic stdin\n", capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads((self.tmp / "stdin.capture").read_text())["stdin"],
                         "synthetic stdin\n")

    def test_signal_forwarding_keeps_config_until_child_handles_signal(self):
        self._lifecycle_stub()
        shells = ["/bin/sh"]
        if Path("/bin/dash").exists():
            shells.append("/bin/dash")
        for shell in shells:
            for sig in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
                with self.subTest(shell=shell, signal=sig):
                    name = Path(shell).name + str(sig.value)
                    child = subprocess.Popen([shell, str(self.wrapper), "dev", "--once"],
                                             env=self._lifecycle_env(name, PROBE_WAIT="1"),
                                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                    try:
                        source = self._wait_ready(child, name)
                        child.send_signal(sig)
                        out, err = child.communicate(timeout=10)
                        self.assertEqual(child.returncode, 128 + sig, err)
                        capture = json.loads((self.tmp / (name + ".capture")).read_text())
                        self.assertEqual(capture["signal"], sig)
                        self.assertEqual(capture["config"]["current-context"], name)
                        self.assertFalse(source.parent.exists())
                    finally:
                        (self.tmp / (name + ".release")).touch()
                        child.communicate(timeout=10)

    def test_flatten_failure_cleans_config_without_starting_container(self):
        kube = self.tmp / "broken.config"
        kube.write_text('apiVersion: v1\nclusters:\n- name: test\n  cluster:\n'
                        '    certificate-authority: missing.crt\n')
        result = self._run(["--help"], {"KUBECONFIG": str(kube),
                                      "DOCKER_HOST": "tcp://unused.invalid:2375"})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("missing.crt", result.stderr)
        self.assertFalse(self.stub_log.exists())
        self.assertEqual(list((self.tmp / "tmp").iterdir()), [])

    def test_yaml_comments_quoting_and_flow_fields_reach_container(self):
        (self.tmp / "ca.crt").write_bytes(b"CA CERT\n")
        (self.tmp / "client's #cert.crt").write_bytes(b"USER CERT\n")
        (self.tmp / "key with spaces").write_bytes(b"USER KEY\n")
        kube = self.tmp / "valid.config"
        kube.write_text('''apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:1
    certificate-authority: ca.crt # local CA
contexts: [{name: test, context: {cluster: test, user: test}}]
current-context: test
users:
- name: test
  user: {client-certificate: 'client''s #cert.crt', client-key: "key\\u0020with spaces"}
''')
        result = self._run(["--help"], {"KUBECONFIG": str(kube),
                                      "DOCKER_HOST": "tcp://unused.invalid:2375"})
        self.assertEqual(result.returncode, 0, result.stderr)
        config = json.loads(self.kube_capture.read_text())
        self.assertEqual(config["clusters"][0]["cluster"]["certificate-authority-data"], "Q0EgQ0VSVAo=")
        self.assertEqual(config["users"][0]["user"]["client-certificate-data"], "VVNFUiBDRVJUCg==")
        self.assertEqual(config["users"][0]["user"]["client-key-data"], "VVNFUiBLRVkK")
        self.assertFalse(self._kube_mount_src().parent.exists())


class FlattenKubeconfigTests(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="c8f-", dir="/tmp"))

    def tearDown(self):
        for child in sorted(self.tmp.rglob("*"), reverse=True):
            try:
                if child.is_file() or child.is_symlink():
                    child.unlink()
                elif child.is_dir():
                    child.rmdir()
            except OSError:
                pass
        try:
            self.tmp.rmdir()
        except OSError:
            pass

    def _flatten(self, src, dest):
        script = 'CAESIUM_CLI_WRAPPER_SOURCED=1; . "$1"; caesium_cli_flatten_kubeconfig "$2" "$3"'
        return subprocess.run(
            ["sh", "-c", script, "flatten", str(SCRIPT), str(src), str(dest)],
            capture_output=True,
            text=True,
            cwd=str(ROOT),
        )

    def test_missing_kubectl_reports_host_requirement(self):
        result = subprocess.run(
            ["/bin/sh", "-c", 'CAESIUM_CLI_WRAPPER_SOURCED=1; . "$1"; '
             'caesium_cli_flatten_kubeconfig "$2" "$3"',
             "flatten", str(SCRIPT), str(self.tmp / "config"), str(self.tmp / "flat")],
            env={"PATH": str(self.tmp)}, capture_output=True, text=True,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("kubectl is required on the host", result.stderr)
        self.assertFalse((self.tmp / "flat").exists())

    def test_relative_ca_and_client_files_become_data(self):
        kube_dir = self.tmp / "k"
        kube_dir.mkdir()
        (kube_dir / "ca.crt").write_bytes(b"CA CERT\n")
        (kube_dir / "user.crt").write_bytes(b"USER CERT\n")
        (kube_dir / "user.key").write_bytes(b"USER KEY\n")
        src = kube_dir / "config"
        src.write_text(
            "apiVersion: v1\n"
            "clusters:\n"
            "- cluster:\n"
            "    certificate-authority: ca.crt\n"
            "    server: https://127.0.0.1:1\n"
            "  name: test\n"
            "users:\n"
            "- name: test\n"
            "  user:\n"
            '    client-certificate: "user.crt"\n'
            "    client-key: user.key\n",
            encoding="utf-8",
        )
        dest = self.tmp / "flat"
        result = self._flatten(src, dest)
        self.assertEqual(result.returncode, 0, result.stderr)
        text = dest.read_text(encoding="utf-8")
        self.assertNotIn("certificate-authority:", text)
        self.assertNotIn("client-certificate:", text)
        self.assertNotIn("client-key:", text)
        self.assertEqual(json.loads(text)["clusters"][0]["cluster"]["certificate-authority-data"], "Q0EgQ0VSVAo=")
        self.assertEqual(json.loads(text)["users"][0]["user"]["client-certificate-data"], "VVNFUiBDRVJUCg==")
        self.assertEqual(json.loads(text)["users"][0]["user"]["client-key-data"], "VVNFUiBLRVkK")
        self.assertEqual(dest.stat().st_mode & 0o777, 0o600)

    def test_absolute_ca_path_is_embedded(self):
        ca = self.tmp / "elsewhere" / "ca.crt"
        ca.parent.mkdir()
        ca.write_bytes(b"ABS CA\n")
        src = self.tmp / "config"
        src.write_text(
            "apiVersion: v1\n"
            "clusters:\n"
            "- cluster:\n"
            f"    certificate-authority: {ca}\n"
            "    server: https://127.0.0.1:1\n"
            "  name: test\n",
            encoding="utf-8",
        )
        dest = self.tmp / "flat"
        result = self._flatten(src, dest)
        self.assertEqual(result.returncode, 0, result.stderr)
        text = dest.read_text(encoding="utf-8")
        self.assertNotIn(str(ca), text)
        self.assertEqual(json.loads(text)["clusters"][0]["cluster"]["certificate-authority-data"], "QUJTIENBCg==")

    def test_missing_ca_file_fails(self):
        src = self.tmp / "config"
        src.write_text(
            "apiVersion: v1\n"
            "clusters:\n"
            "- cluster:\n"
            "    certificate-authority: missing.crt\n"
            "    server: https://127.0.0.1:1\n"
            "  name: test\n",
            encoding="utf-8",
        )
        dest = self.tmp / "flat"
        result = self._flatten(src, dest)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("missing.crt", result.stderr)

    def test_existing_data_fields_are_left_alone(self):
        src = self.tmp / "config"
        src.write_text(
            "apiVersion: v1\n"
            "clusters:\n"
            "- cluster:\n"
            "    certificate-authority-data: Q0EK\n"
            "    server: https://127.0.0.1:1\n"
            "  name: test\n",
            encoding="utf-8",
        )
        dest = self.tmp / "flat"
        result = self._flatten(src, dest)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(dest.read_text())["clusters"][0]["cluster"]["certificate-authority-data"], "Q0EK")


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
