"""Exercise the actual smoke script with a hermetic native-runtime contract."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
CID = "a" * 64
IMAGE = "sha256:" + "b" * 64
PASS = "stress image: resident allocation, release barrier, real OOM, and bounds passed"

FAKE = r'''#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$FIXTURE/journal"
cid=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
image=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
case "$1" in
run)
    if [[ " $* " == *" -d "* ]]; then
        [[ " $* " == *" --memory=64m --memory-swap=64m "* ]] || exit 12
        [[ "$*" == *" --memory-mib 128 --wait-file /tmp/release --wait-timeout 10s --hold 2s" ]] || exit 12
        if [ "$SCENARIO" = allocate_fail ]; then echo SECRET_NATIVE >&2; exit 7; fi
        if [ "$SCENARIO" = allocate_fail_with_output ]; then echo "$cid"; echo SECRET_NATIVE >&2; exit 7; fi
        if [ "$SCENARIO" = invalid_ack ]; then echo unknown; else echo "$cid"; fi
    elif [[ " $* " == *" --memory-mib 1025 "* ]]; then
        echo 'invalid arguments: memory must be 1..1024 MiB, hold 0..5m, wait-timeout >0..5m, with no positional arguments'
        [ "$SCENARIO" != bound_accepted ] || exit 0
        exit 2
    elif [[ " $* " == *" --wait-file /tmp/never-released "* ]]; then
        echo 'waiting for /tmp/never-released'
        echo 'wait for release file: context deadline exceeded' >&2
        [ "$SCENARIO" != wait_accepted ] || exit 0
        exit 1
    else
        [[ " $* " == *" --memory=64m --memory-swap=64m "* ]] || exit 12
        [[ "$*" == *" --memory-mib 16 --hold 2s" ]] || exit 12
        printf 'cgroup memory limit 67108864\nallocated 16 MiB\ncompleted\n'
        if [ "$SCENARIO" = healthy_fail ]; then echo SECRET_NATIVE >&2; exit 5; fi
    fi
    ;;
logs)
    [[ "${!#}" = "$cid" ]] || exit 12
    n=0; if [ -f "$FIXTURE/log_count" ]; then read -r n < "$FIXTURE/log_count"; fi
    printf '%s\n' "$((n+1))" > "$FIXTURE/log_count"
    if [ "$SCENARIO" != no_marker ]; then echo 'waiting for /tmp/release'; fi
    if [ "$SCENARIO" = logs_fail ] || { [ "$SCENARIO" = logs_fail_after_ready ] && [ "$n" -ge 1 ]; }; then
        echo SECRET_NATIVE >&2; exit 7
    fi
    if [ "$SCENARIO" = early_allocation ] || { [ "$SCENARIO" = late_allocation ] && [ "$n" -ge 1 ]; }; then echo 'allocated 128 MiB'; fi
    if [ "$SCENARIO" = unknown_logs ]; then
        echo 'SECRET_APPLICATION token=private' >&2
        printf '%300s\n' oversize
    fi
    ;;
inspect)
    [[ "${!#}" = "$cid" ]] || exit 12
    if [ "$SCENARIO" = inspect_fail ]; then echo SECRET_NATIVE >&2; exit 9; fi
    native_status=running; running=true; code=0; oom=false; memory=67108864
    if [ -f "$FIXTURE/released" ]; then
        n=0; if [ -f "$FIXTURE/state_count" ]; then read -r n < "$FIXTURE/state_count"; fi
        n=$((n+1)); printf '%s\n' "$n" > "$FIXTURE/state_count"
        native_status=exited; running=false; code=137; oom=true
        case "$SCENARIO" in
            delayed_oom) if [ "$n" -lt 6 ]; then oom=false; fi ;;
            no_oom|logs_fail_cleanup_fail) oom=false ;;
            wrong_exit) code=2; oom=false ;;
            wrong_status) native_status=dead ;;
            running_forever) native_status=running; running=true; code=0; oom=false ;;
            terminal_inspect_fail) echo SECRET_NATIVE >&2; exit 9 ;;
        esac
    fi
    case "$SCENARIO" in
        malformed) echo "$cid|false|137|true"; exit 0 ;;
        wrong_identity) cid=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc ;;
        malformed_boolean) oom=maybe ;;
        malformed_exit) code=NaN ;;
        malformed_memory) memory=SECRET_MEMORY ;;
    esac
    # The real guard must request one coherent record, not scalar inspections.
    [ "$3" = '{{.Id}}|{{.Image}}|{{.State.Status}}|{{.State.Running}}|{{.State.ExitCode}}|{{.State.OOMKilled}}|{{.HostConfig.Memory}}|{{.HostConfig.MemorySwap}}' ] || exit 12
    printf '%s|%s|%s|%s|%s|%s|%s|67108864\n' "$cid" "$image" "$native_status" "$running" "$code" "$oom" "$memory"
    ;;
exec)
    [ "$*" = "exec $cid touch /tmp/release" ] || exit 12
    if [ "$SCENARIO" = exec_fail ]; then echo SECRET_NATIVE >&2; exit 8; fi
    : > "$FIXTURE/released"
    ;;
rm)
    [ "$*" = "rm -f $cid" ] || exit 12
    if [ "$SCENARIO" = cleanup_fail ] || [ "$SCENARIO" = logs_fail_cleanup_fail ]; then
        echo SECRET_NATIVE >&2; exit 6
    fi
    ;;
*) exit 12 ;;
esac
'''


class StressSmokeTests(unittest.TestCase):
    def run_case(self, scenario):
        with tempfile.TemporaryDirectory() as temporary:
            fixture = Path(temporary)
            (fixture / "scratch").mkdir()
            cli = fixture / "runtime"
            cli.write_text(FAKE)
            cli.chmod(0o700)
            sleep = fixture / "sleep"
            sleep.write_text("#!/usr/bin/env bash\nexit 0\n")
            sleep.chmod(0o700)
            env = dict(os.environ, FIXTURE=str(fixture), SCENARIO=scenario,
                       TMPDIR=str(fixture / "scratch"), PATH=str(fixture) + os.pathsep + os.environ["PATH"])
            result = subprocess.run(["/bin/bash", str(ROOT / "build/stress/smoke.sh"), str(cli), "fixture:pinned"],
                                    env=env, capture_output=True, text=True, timeout=10, check=False)
            journal = (fixture / "journal").read_text().splitlines()
            polls = int((fixture / "state_count").read_text()) if (fixture / "state_count").exists() else 0
            self.assertEqual(list((fixture / "scratch").iterdir()), [], "private raw diagnostic files survived")
        self.assertNotIn("SECRET_", result.stdout + result.stderr)
        return result, journal, polls

    def assert_refused(self, scenario, reason, status=1):
        result, journal, polls = self.run_case(scenario)
        self.assertEqual(result.returncode, status, result.stderr)
        self.assertNotIn(PASS, result.stdout)
        self.assertIn("reason=" + reason, result.stderr)
        return result, journal, polls

    def test_immediate_and_observed_delayed_oom_require_complete_triple(self):
        for scenario, minimum in [("immediate", 1), ("delayed_oom", 6)]:
            with self.subTest(scenario=scenario):
                result, journal, polls = self.run_case(scenario)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn(PASS, result.stdout)
                self.assertGreaterEqual(polls, minimum)
                self.assertIn("running=false exit=137 oom=true", result.stderr)
                self.assertIn("status=exited", result.stderr)
                self.assertIn("memory=67108864 swap=67108864", result.stderr)
                self.assertIn("cid=" + CID + " image=" + IMAGE, result.stderr)
                self.assertEqual(sum(line == "rm -f " + CID for line in journal), 1)
                self.assertIn("--memory-mib 1025 --hold 0s", journal[-2])
                self.assertIn("--wait-timeout 100ms", journal[-1])

    def test_terminal_nonoom_or_running_cannot_qualify_at_poll_bound(self):
        for scenario in ["no_oom", "running_forever"]:
            with self.subTest(scenario=scenario):
                result, _, polls = self.assert_refused(scenario, "terminal_oom_unconfirmed")
                self.assertGreaterEqual(polls, 100)
                self.assertIn("phase=terminal", result.stderr)

    def test_other_terminal_exit_refused_without_waiting_for_oom(self):
        result, _, polls = self.assert_refused("wrong_exit", "wrong_terminal_exit")
        self.assertIn("exit=2 oom=false", result.stderr)
        self.assertEqual(polls, 2)  # One qualifying attempt plus failure snapshot.

    def test_terminal_status_must_be_exited(self):
        result, _, _ = self.assert_refused("wrong_status", "wrong_terminal_status")
        self.assertIn("status=dead running=false exit=137 oom=true", result.stderr)

    def test_failed_or_malformed_native_inspection_is_not_evidence(self):
        for scenario, rc in [("inspect_fail", 9), ("terminal_inspect_fail", 9),
                             ("malformed", 1), ("wrong_identity", 1),
                             ("malformed_boolean", 1), ("malformed_exit", 1), ("malformed_memory", 1)]:
            with self.subTest(scenario=scenario):
                reason = "terminal_inspect_failed" if scenario == "terminal_inspect_fail" else "barrier_inspect_failed"
                result, journal, _ = self.assert_refused(scenario, reason, rc)
                self.assertIn("inspect_unavailable", result.stderr)
                self.assertEqual(sum(line == "rm -f " + CID for line in journal), 1)

    def test_no_allocation_before_release_including_second_log_read(self):
        for scenario in ["early_allocation", "late_allocation"]:
            with self.subTest(scenario=scenario):
                result, journal, _ = self.assert_refused(scenario, "allocated_before_release")
                self.assertNotIn("exec " + CID + " touch /tmp/release", journal)
                self.assertIn("allocated 128 MiB", result.stderr)

    def test_log_producer_failure_with_valid_wait_marker_is_refused(self):
        for scenario in ["logs_fail", "logs_fail_after_ready"]:
            with self.subTest(scenario=scenario):
                result, journal, _ = self.assert_refused(scenario, "barrier_logs_failed", 7)
                self.assertNotIn("exec " + CID + " touch /tmp/release", journal)
                self.assertIn("logs_unavailable rc=7", result.stderr)
                self.assertNotIn("fixture_logs_begin", result.stderr)

    def test_unknown_or_oversized_records_are_not_persisted(self):
        result, _, _ = self.run_case("unknown_logs")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("fixture_logs_begin", result.stderr)
        self.assertNotIn("oversize", result.stderr)
        self.assertLess(len(result.stderr), 1024)

    def test_missing_marker_and_release_failure_keep_static_diagnostics(self):
        self.assert_refused("no_marker", "barrier_marker_missing")
        result, _, _ = self.assert_refused("exec_fail", "release_exec_failed", 8)
        self.assertIn("phase=release", result.stderr)

    def test_cleanup_failure_cannot_print_pass(self):
        result, journal, _ = self.assert_refused("cleanup_fail", "cleanup_failed", 6)
        self.assertIn("removal_failed rc=6", result.stderr)
        self.assertEqual(sum(line == "rm -f " + CID for line in journal), 1)
        self.assertNotIn("--memory-mib 1025 --hold 0s", "\n".join(journal))

    def test_original_failure_survives_cleanup_refusal_with_evidence_before_rm(self):
        result, journal, _ = self.assert_refused("logs_fail_cleanup_fail", "terminal_oom_unconfirmed")
        self.assertIn("removal_failed rc=6", result.stderr)
        remove = journal.index("rm -f " + CID)
        self.assertTrue(journal[remove - 2].startswith("inspect "))
        self.assertTrue(journal[remove - 1].startswith("logs "))
        self.assertEqual(sum(line == "rm -f " + CID for line in journal), 1)

    def test_foreground_and_acknowledgement_failures_do_not_remove_unknown_id(self):
        for scenario, reason, rc in [("healthy_fail", "healthy_run_failed", 5),
                                     ("allocate_fail", "waiter_run_failed", 7),
                                     ("allocate_fail_with_output", "waiter_run_failed", 7),
                                     ("invalid_ack", "waiter_identity_invalid", 1)]:
            with self.subTest(scenario=scenario):
                result, journal, _ = self.assert_refused(scenario, reason, rc)
                self.assertFalse(any(line.startswith("rm ") for line in journal))
                if scenario != "healthy_fail":
                    self.assertIn("reconciliation_required=true", result.stderr)

    def test_argument_and_wait_bounds_still_require_exact_fixture_codes(self):
        self.assert_refused("bound_accepted", "allocation_bound_wrong_exit")
        self.assert_refused("wait_accepted", "wait_bound_wrong_exit")


if __name__ == "__main__":
    unittest.main()
