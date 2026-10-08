"""Adversarial bounded reduction and actual read-only subprocess controls."""
import importlib.util
import errno
import json
import os
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

SCRIPT = Path(__file__).with_name("stress-native-diagnostics.py")
SPEC = importlib.util.spec_from_file_location("stress_diagnostics", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)
CID = "a" * 64
IMAGE = "sha256:" + "b" * 64
STATE = f"{CID}|{IMAGE}|exited|false|137|false|67108864|67108864"


def event(cid=CID, **changes):
    value = {"Type": "container", "Action": "die", "Actor": {"ID": cid, "Attributes": {
        "exitCode": "137", "signal": "9", "environment": "SECRET_DATA"}}, "timeNano": 123}
    value.update(changes)
    return json.dumps(value).encode()


class StressNativeDiagnosticsTests(unittest.TestCase):
    def test_events_reduce_only_exact_container_without_unknown_attributes(self):
        value = MODULE.events(event(), CID)
        self.assertEqual(value, {"outcome": "complete", "records": [
            {"action": "die", "time_nano": 123, "exit": 137, "signal": "9"}]})
        self.assertNotIn("SECRET", json.dumps(value))

    def test_foreign_ambiguous_or_untyped_events_refuse_entire_capture(self):
        for body in [event("c" * 64), event(id="c" * 64), event(Type="image"),
                     event(Action="start"), event(timeNano=True), event(timeNano=-1),
                     event(Actor={"ID": CID, "Attributes": {"signal": "SECRET"}}),
                     event(Actor={"ID": CID, "Attributes": {"exitCode": "NaN"}}),
                     b"null", b"{", b"\xff", event() + b"\n" + event("c" * 64)]:
            with self.subTest(body=body[:50]):
                with self.assertRaises((ValueError, TypeError, UnicodeError)):
                    MODULE.events(body, CID)

    def test_event_count_line_and_total_caps_refuse_partial_valid_evidence(self):
        for body in [b"\n".join([event()] * 101), event(padding="x" * 4096), b"x" * 65537]:
            with self.assertRaises(ValueError):
                MODULE.events(body, CID)

    def test_exact_event_window_refuses_stale_and_future_witnesses(self):
        self.assertEqual(MODULE.events(event(timeNano=123), CID, (123, 124))["outcome"], "complete")
        for stamp in [122, 125]:
            with self.assertRaises(ValueError):
                MODULE.events(event(timeNano=stamp), CID, (123, 124))

    def test_journal_keeps_original_coherent_transitions_and_exact_bound(self):
        text = "\n".join(f"{n}|{STATE}" for n in range(100))
        value = MODULE.journal(text, CID, IMAGE)
        self.assertEqual(value["polls"], 100)
        self.assertEqual(len(value["transitions"]), 1)
        self.assertEqual(value["transitions"][0]["last_poll"], 99)
        self.assertFalse(value["transitions"][0]["state"]["oom"])
        text = f"0|{STATE}\n1|{STATE.replace('|137|false|', '|137|true|')}"
        self.assertEqual(len(MODULE.journal(text, CID, IMAGE)["transitions"]), 2)

    def test_journal_identity_order_boolean_numeric_and_extra_rows_are_refused(self):
        for text in ["", f"1|{STATE}", f"0|{STATE}\n0|{STATE}",
                     f"0|{STATE.replace(CID, 'c' * 64)}", f"0|{STATE.replace(IMAGE, 'sha256:' + 'c' * 64)}",
                     f"0|{STATE.replace('|false|137', '|unknown|137')}",
                     f"0|{STATE.replace('|137|', '|999|')}",
                     "\n".join(f"{n}|{STATE}" for n in range(101))]:
            with self.subTest(text=text[:50]):
                with self.assertRaises(ValueError):
                    MODULE.journal(text, CID, IMAGE)

    def test_actual_subprocess_stderr_is_never_retained_and_nonzero_is_unavailable(self):
        code = "import sys;sys.stdout.buffer.write(" + repr(event()) + ");sys.stderr.write('SECRET_NATIVE')"
        body, error = MODULE.capture([sys.executable, "-c", code])
        self.assertIsNone(error)
        self.assertNotIn(b"SECRET_NATIVE", body)
        body, error = MODULE.capture([sys.executable, "-c", code + ";sys.exit(9)"])
        self.assertIsNone(body)
        self.assertEqual(error, "command-failed")

    def test_explicit_environment_merged_stderr_and_timeout_bound_preserve_defaults(self):
        code = "import os,sys;sys.stdout.write(os.environ['EXACT_OBSERVER_ENV']);sys.stderr.write('stderr-proof')"
        body, error = MODULE.capture([sys.executable, "-c", code],
                                     env=dict(os.environ, EXACT_OBSERVER_ENV="stdout-proof"),
                                     merge_stderr=True, timeout=1)
        self.assertIsNone(error)
        self.assertIn(b"stdout-proof", body)
        self.assertIn(b"stderr-proof", body)
        with mock.patch.object(MODULE.subprocess, "Popen") as spawn:
            for bound in (False, 0, -1, 4, float('nan'), float('inf'), '1'):
                self.assertEqual(MODULE.capture(['unused'], timeout=bound), (None, 'invalid-bound'))
            spawn.assert_not_called()

    def test_actual_oversized_or_timed_out_child_is_joined(self):
        original = subprocess.Popen
        children = []
        def spawn(*args, **kwargs):
            child = original(*args, **kwargs)
            children.append(child)
            return child
        for code, reason in [("import sys;sys.stdout.write('x'*70000)", "oversized"),
                             ("import time;time.sleep(5)", "deadline")]:
            with mock.patch.object(MODULE, "QUERY_SECONDS", .15), mock.patch.object(MODULE.subprocess, "Popen", spawn):
                body, error = MODULE.capture([sys.executable, "-c", code])
            self.assertIsNone(body)
            self.assertEqual(error, reason)
            self.assertIsNotNone(children[-1].poll())
            self.assertTrue(children[-1].stdout.closed)

    def test_actual_spawned_child_is_joined_when_selector_setup_fails(self):
        original = subprocess.Popen
        original_selector = MODULE.selectors.DefaultSelector
        children = []
        def spawn(*args, **kwargs):
            child = original(*args, **kwargs)
            children.append(child)
            return child
        def registration_failure():
            selector = original_selector()
            selector.register = mock.Mock(side_effect=OSError(errno.EMFILE, "SECRET_FD_ERROR"))
            return selector
        for fault in [OSError(errno.EMFILE, "SECRET_FD_ERROR"), registration_failure]:
            with self.subTest(phase="construct" if isinstance(fault, OSError) else "register"):
                try:
                    with mock.patch.object(MODULE.subprocess, "Popen", spawn), \
                         mock.patch.object(MODULE.selectors, "DefaultSelector", side_effect=fault):
                        with self.assertRaises(OSError):
                            MODULE.capture([sys.executable, "-c", "import time;time.sleep(5)"])
                    self.assertIsNotNone(children[-1].poll(), "selector failure left the owned child alive")
                    self.assertTrue(children[-1].stdout.closed, "selector failure leaked the owned read pipe")
                finally:
                    child = children[-1]
                    if child.poll() is None:
                        os.killpg(child.pid, signal.SIGKILL)
                    child.wait()
                    child.stdout.close()

    def test_successful_leader_cannot_leave_closed_pipe_descendant_alive(self):
        with tempfile.TemporaryDirectory() as temporary:
            pid_file = Path(temporary) / "descendant"
            code = ("import subprocess,sys,pathlib;"
                    "child=subprocess.Popen([sys.executable,'-c','import time;time.sleep(5)'],"
                    "stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL);"
                    "pathlib.Path(" + repr(str(pid_file)) + ").write_text(str(child.pid));"
                    "sys.stdout.buffer.write(" + repr(event()) + ")")
            body, error = MODULE.capture([sys.executable, "-c", code])
            self.assertIsNone(error)
            self.assertEqual(body, event())
            child = int(pid_file.read_text())
            try:
                deadline = time.monotonic() + 2
                while time.monotonic() < deadline:
                    try:
                        os.kill(child, 0)
                    except ProcessLookupError:
                        break
                    # An orphan may await the host's reaper after SIGKILL. It
                    # must already be dead, never a surviving observer writer.
                    status = subprocess.run(["ps", "-o", "stat=", "-p", str(child)],
                                            capture_output=True, text=True, timeout=1, check=False)
                    if status.stdout.strip().startswith("Z"):
                        break
                    time.sleep(.01)
                else:
                    self.fail("successful capture left its same-group descendant alive")
            finally:
                try:
                    os.kill(child, signal.SIGKILL)
                except ProcessLookupError:
                    pass

    def test_missing_runtime_or_unbound_snapshot_cannot_query_or_qualify(self):
        self.assertEqual(MODULE.capture(["/missing/stress-runtime"]), (None, "command-unavailable"))
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            snapshot = root / "snapshot"
            journal = root / "journal"
            snapshot.write_text(STATE.replace(IMAGE, "sha256:" + "c" * 64))
            journal.write_text("0|" + STATE)
            argv = [sys.executable, "-I", str(SCRIPT), "--runtime", "/missing/stress-runtime",
                    "--cid", CID, "--image", IMAGE, "--snapshot", str(snapshot),
                    "--journal", str(journal), "--since", str(int(time.time()))]
            result = subprocess.run(argv, capture_output=True, text=True, timeout=5, check=True)
            value = json.loads(result.stdout)
            self.assertEqual(value["events"], {"outcome": "unavailable", "reason": "identity-not-proved"})
            self.assertEqual(value["qualification"], "unchanged")
            self.assertNotIn("snapshot", value)

    def test_actual_cli_finite_query_binds_snapshot_image_and_container(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "snapshot").write_text(STATE)
            (root / "journal").write_text("0|" + STATE)
            runtime = root / "runtime"
            runtime.write_text("#!" + sys.executable + "\nimport sys,json,pathlib\n"
                               "pathlib.Path(" + repr(str(root / "argv")) + ").write_text(json.dumps(sys.argv[1:]))\n"
                               "print(json.dumps(" + repr(json.loads(event(timeNano=int(time.time()) * 10**9))) + "))\n")
            runtime.chmod(0o700)
            result = subprocess.run([sys.executable, "-I", str(SCRIPT), "--runtime", str(runtime),
                                     "--cid", CID, "--image", IMAGE, "--snapshot", str(root / "snapshot"),
                                     "--journal", str(root / "journal"), "--since", str(int(time.time()) - 1)],
                                    capture_output=True, text=True, timeout=5, check=True)
            value = json.loads(result.stdout)
            self.assertEqual(value["events"]["outcome"], "complete")
            self.assertEqual(value["image"], IMAGE)
            self.assertEqual(value["cgroup"]["outcome"], "unavailable")
            argv = json.loads((root / "argv").read_text())
            self.assertEqual(argv[0], "events")
            self.assertIn("--until", argv)
            self.assertEqual(argv.count("container=" + CID), 1)
            self.assertEqual(argv[-2:], ["--format", "{{json .}}"])
            self.assertNotIn("SECRET", result.stdout + result.stderr)

    def test_interrupted_observer_joins_its_readonly_child_before_return(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "snapshot").write_text(STATE)
            (root / "journal").write_text("0|" + STATE)
            runtime = root / "runtime"
            runtime.write_text("#!" + sys.executable + "\nimport os,time,pathlib\npathlib.Path(" +
                               repr(str(root / "pid")) + ").write_text(str(os.getpid()))\ntime.sleep(5)\n")
            runtime.chmod(0o700)
            process = subprocess.Popen([sys.executable, "-I", str(SCRIPT), "--runtime", str(runtime),
                                        "--cid", CID, "--image", IMAGE, "--snapshot", str(root / "snapshot"),
                                        "--journal", str(root / "journal"), "--since", str(int(time.time()))],
                                       stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            try:
                deadline = time.monotonic() + 2
                while not (root / "pid").exists() and time.monotonic() < deadline:
                    time.sleep(.01)
                self.assertTrue((root / "pid").exists())
                child = int((root / "pid").read_text())
                process.send_signal(signal.SIGTERM)
                stdout, stderr = process.communicate(timeout=2)
                self.assertEqual(json.loads(stdout)["reason"], "observer-interrupted")
                self.assertEqual(stderr, "")
                with self.assertRaises(ProcessLookupError):
                    os.kill(child, 0)
            finally:
                if process.poll() is None:
                    process.kill()
                process.communicate()


if __name__ == "__main__":
    unittest.main()
