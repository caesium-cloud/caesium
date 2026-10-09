"""Exercise the actual smoke script with a hermetic native-runtime contract."""
import os
import json
import shlex
import shutil
import sys
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
        [[ "$*" == *" --memory-mib 128 --wait-file /tmp/release --wait-timeout 10s --hold 2s --linger 2s" ]] || exit 12
        archives=("$TMPDIR"/*/release.tar)
        [[ "${#archives[@]}" == 1 && -s "${archives[0]}" ]] || exit 12
        if [ "$SCENARIO" = allocate_fail ]; then echo SECRET_NATIVE >&2; exit 7; fi
        if [ "$SCENARIO" = allocate_fail_with_output ]; then echo "$cid"; echo SECRET_NATIVE >&2; exit 7; fi
        if [ "$SCENARIO" = invalid_ack ]; then echo unknown; else echo "$cid"; fi
    elif [[ " $* " == *" --memory-mib 1025 "* ]]; then
        echo 'invalid arguments: memory must be 1..1024 MiB, hold 0..5m, wait-timeout >0..5m, linger 0..1m, with no positional arguments'
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
        case "$SCENARIO" in
            healthy_hollow) : ;;
            healthy_wrong_limit) printf 'cgroup memory limit 134217728\nallocated 16 MiB\ncompleted\n' ;;
            healthy_wrong_allocation) printf 'cgroup memory limit 67108864\nallocated 15 MiB\ncompleted\n' ;;
            healthy_no_completion) printf 'cgroup memory limit 67108864\nallocated 16 MiB\n' ;;
            *) printf 'cgroup memory limit 67108864\nallocated 16 MiB\ncompleted\n' ;;
        esac
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
    if [ -f "$FIXTURE/released" ]; then
        case "$SCENARIO" in
            supervisor_record_missing) : ;;
            supervisor_record_clean_exit) echo 'workload exited with status 0' ;;
            supervisor_record_sigterm) echo 'workload terminated by signal 15' ;;
            supervisor_record_spoofed) echo 'workload terminated by signal 9 SECRET_TAIL' ;;
            terminal_logs_fail) echo SECRET_NATIVE >&2; exit 7 ;;
            *) echo 'workload terminated by signal 9' ;;
        esac
    fi
    if [ "$SCENARIO" = unknown_logs ]; then
        echo 'SECRET_APPLICATION token=private' >&2
        printf '%300s\n' oversize
    fi
    ;;
inspect)
    [[ "${!#}" = "$cid" ]] || exit 12
    if [ "$SCENARIO" = inspect_fail ]; then echo SECRET_NATIVE >&2; exit 9; fi
    native_status=running; running=true; code=0; oom=false; memory=67108864; swap=67108864
    if [ -f "$FIXTURE/released" ]; then
        n=0; if [ -f "$FIXTURE/state_count" ]; then read -r n < "$FIXTURE/state_count"; fi
        n=$((n+1)); printf '%s\n' "$n" > "$FIXTURE/state_count"
        native_status=exited; running=false; code=137; oom=true
        case "$SCENARIO" in
            podman_stopped_then_exited) if [ "$n" -eq 1 ]; then native_status=stopped; fi ;;
            podman_stopping_then_stopped_then_exited)
                if [ "$n" -eq 1 ]; then native_status=stopping; code=0; oom=false
                elif [ "$n" -eq 2 ]; then native_status=stopped; fi ;;
            podman_stopped_forever) native_status=stopped ;;
            podman_stopping_forever) native_status=stopping; code=0; oom=false ;;
            podman_stopped_no_oom)
                if [ "$n" -eq 1 ]; then native_status=stopped; else oom=false; fi ;;
            podman_stopped_wrong_exit)
                if [ "$n" -eq 1 ]; then native_status=stopped; else code=2; oom=false; fi ;;
            podman_stopped_then_inspect_fail|podman_stopped_then_inspect_fail_rc1|podman_stopped_then_malformed|podman_stopped_then_foreign_cid)
                if [ "$n" -eq 1 ]; then native_status=stopped
                elif [ "$n" -eq 2 ]; then
                    case "$SCENARIO" in
                        podman_stopped_then_inspect_fail) echo SECRET_NATIVE >&2; exit 9 ;;
                        podman_stopped_then_inspect_fail_rc1) echo SECRET_NATIVE >&2; exit 1 ;;
                        podman_stopped_then_malformed) code=SECRET_EXIT ;;
                        podman_stopped_then_foreign_cid) cid=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc ;;
                    esac
                fi ;;
            podman_initialized) native_status=initialized ;;
            delayed_oom) if [ "$n" -lt 6 ]; then oom=false; fi ;;
            no_oom|logs_fail_cleanup_fail) oom=false ;;
            late_oom_observation) if [ "$n" -le 100 ]; then oom=false; fi ;;
            wrong_exit) code=2; oom=false ;;
            wrong_status) native_status=dead ;;
            running_forever) native_status=running; running=true; code=0; oom=false ;;
            terminal_inspect_fail) echo SECRET_NATIVE >&2; exit 9 ;;
            terminal_memory_mismatch) memory=134217728 ;;
            terminal_swap_mismatch) swap=-1 ;;
        esac
    fi
    case "$SCENARIO" in
        malformed) echo "$cid|false|137|true"; exit 0 ;;
        wrong_identity) cid=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc ;;
        malformed_boolean) oom=maybe ;;
        malformed_exit) code=NaN ;;
        malformed_memory) memory=SECRET_MEMORY ;;
        unknown_status) native_status=unknown ;;
        barrier_memory_mismatch) memory=0 ;;
        barrier_swap_mismatch) swap=134217728 ;;
    esac
    # The real guard must request one coherent record, not scalar inspections.
    [ "$3" = '{{.Id}}|{{.Image}}|{{.State.Status}}|{{.State.Running}}|{{.State.ExitCode}}|{{.State.OOMKilled}}|{{.HostConfig.Memory}}|{{.HostConfig.MemorySwap}}' ] || exit 12
    printf '%s|%s|%s|%s|%s|%s|%s|%s\n' "$cid" "$image" "$native_status" "$running" "$code" "$oom" "$memory" "$swap"
    ;;
cp)
    [ "$*" = "cp - $cid:/tmp" ] || exit 12
    "$TEST_PYTHON" -c '
import io, sys, tarfile
body = sys.stdin.buffer.read(65537)
assert len(body) <= 65536
with tarfile.open(fileobj=io.BytesIO(body), mode="r:") as archive:
    members = archive.getmembers()
    assert len(members) == 1
    member = members[0]
    assert member.name == "release" and member.isfile() and member.size == 0
    assert member.mode == 0o644 and archive.extractfile(member).read() == b""
' || exit 12
    : > "$FIXTURE/archive_checked"
    if [ "$SCENARIO" = cp_fail ]; then echo SECRET_NATIVE >&2; exit 8; fi
    : > "$FIXTURE/released"
    ;;
rm)
    [ "$*" = "rm -f $cid" ] || exit 12
    if [ "$SCENARIO" = cleanup_fail ] || [ "$SCENARIO" = logs_fail_cleanup_fail ]; then
        echo SECRET_NATIVE >&2; exit 6
    fi
    ;;
events)
    [[ " $* " == *" --filter container=$cid "* ]] || exit 12
    [[ " $* " == *" --filter event=oom --filter event=die --filter event=kill "* ]] || exit 12
    [ "${!#}" = '{{json .}}' ] || exit 12
    if [ "$SCENARIO" = event_failure ]; then echo SECRET_NATIVE >&2; exit 9; fi
    if [ "$SCENARIO" = event_malformed ]; then echo SECRET_EVENT; exit 0; fi
    stamp="$3"
    printf '{"Type":"container","Action":"oom","Actor":{"ID":"%s","Attributes":{"SECRET_ENV":"private"}},"timeNano":%s}\n' "$cid" "$((stamp*1000000000))"
    ;;
*) exit 12 ;;
esac
'''


class StressSmokeTests(unittest.TestCase):
    def run_case(self, scenario, observer=True):
        with tempfile.TemporaryDirectory() as temporary:
            fixture = Path(temporary)
            (fixture / "scratch").mkdir()
            cli = fixture / "runtime"
            cli.write_text(FAKE)
            cli.chmod(0o700)
            sleep = fixture / "sleep"
            sleep.write_text("#!/usr/bin/env bash\nexit 0\n")
            sleep.chmod(0o700)
            mktemp = fixture / "mktemp"
            mktemp.write_text("#!/bin/sh\nexec " + shlex.quote(shutil.which("mktemp")) +
                              ' -d "$FIXTURE/scratch/owned.XXXXXX"\n')
            mktemp.chmod(0o700)
            if scenario == "archive_fail":
                tar = fixture / "tar"
                tar.write_text("#!/bin/sh\necho SECRET_ARCHIVE >&2\nexit 8\n")
                tar.chmod(0o700)
            if not observer:
                for command in ["bash", "awk", "cat", "grep", "rm", "date", "chmod", "tar"]:
                    (fixture / command).symlink_to(shutil.which(command))
            env = dict(os.environ, FIXTURE=str(fixture), SCENARIO=scenario,
                       TEST_PYTHON=sys.executable,
                       TMPDIR=str(fixture / "scratch"), PATH=str(fixture) + (os.pathsep + os.environ["PATH"] if observer else ""))
            result = subprocess.run(["/bin/bash", str(ROOT / "build/stress/smoke.sh"), str(cli), "fixture:pinned"],
                                    env=env, capture_output=True, text=True, timeout=10, check=False)
            journal = (fixture / "journal").read_text().splitlines()
            if any(line.startswith("cp ") for line in journal):
                self.assertTrue((fixture / "archive_checked").exists(), "release archive was not validated")
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
                self.assertIn("\nworkload terminated by signal 9\n", result.stderr)
                self.assertEqual(sum(line == "rm -f " + CID for line in journal), 1)
                self.assertIn("--memory-mib 1025 --hold 0s", journal[-2])
                self.assertIn("--wait-timeout 100ms", journal[-1])

    def test_supervisor_record_must_show_the_workload_child_was_killed(self):
        # The runtime's OOM triple is necessary but the lingering init's record
        # must also show the kernel killed its workload child, not the init.
        for scenario in ["supervisor_record_missing", "supervisor_record_clean_exit",
                         "supervisor_record_sigterm", "supervisor_record_spoofed"]:
            with self.subTest(scenario=scenario):
                result, journal, _ = self.assert_refused(scenario, "terminal_supervisor_record_missing")
                self.assertIn("phase=terminal", result.stderr)
                self.assertIn("status=exited running=false exit=137 oom=true", result.stderr)
                self.assertEqual(sum(line == "cp - " + CID + ":/tmp" for line in journal), 1)
                self.assertEqual(sum(line == "rm -f " + CID for line in journal), 1)
                self.assertNotIn("--memory-mib 1025 --hold 0s", "\n".join(journal))
        result, journal, _ = self.assert_refused("terminal_logs_fail", "terminal_logs_failed", 7)
        self.assertIn("logs_unavailable rc=7", result.stderr)
        self.assertEqual(sum(line == "rm -f " + CID for line in journal), 1)

    def test_terminal_nonoom_or_running_cannot_qualify_at_poll_bound(self):
        for scenario in ["no_oom", "running_forever"]:
            with self.subTest(scenario=scenario):
                result, _, polls = self.assert_refused(scenario, "terminal_oom_unconfirmed")
                self.assertGreaterEqual(polls, 100)
                self.assertIn("phase=terminal", result.stderr)

    def test_podman_cleanup_transitions_require_a_later_strict_exited_record(self):
        for scenario, statuses in [
                ("podman_stopped_then_exited", ["stopped", "exited"]),
                ("podman_stopping_then_stopped_then_exited", ["stopping", "stopped", "exited"])]:
            with self.subTest(scenario=scenario):
                result, journal, polls = self.run_case(scenario)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn(PASS, result.stdout)
                self.assertEqual(polls, len(statuses) + 1)  # Qualifying reads plus cleanup snapshot.
                value = next(json.loads(line) for line in result.stderr.splitlines()
                             if line.startswith('{"schema_version":'))
                self.assertEqual(value["journal"]["outcome"], "complete")
                self.assertEqual(value["journal"]["polls"], len(statuses))
                self.assertEqual([row["state"]["status"] for row in value["journal"]["transitions"]], statuses)
                terminal = value["journal"]["transitions"][-1]["state"]
                self.assertEqual(terminal, {"status": "exited", "running": False, "exit": 137,
                                            "oom": True, "memory": 67108864, "swap": 67108864})
                self.assertEqual(value["qualification"], "unchanged")
                self.assertEqual(sum(line == "cp - " + CID + ":/tmp" for line in journal), 1)
                self.assertEqual(sum(line == "rm -f " + CID for line in journal), 1)

    def test_podman_transitions_and_intermediate_oom_cannot_qualify_at_poll_bound(self):
        for scenario in ["podman_stopped_forever", "podman_stopping_forever", "podman_stopped_no_oom"]:
            with self.subTest(scenario=scenario):
                result, _, polls = self.assert_refused(scenario, "terminal_oom_unconfirmed")
                self.assertEqual(polls, 101)  # No early success, even stopped/137/OOMtrue.
                value = next(json.loads(line) for line in result.stderr.splitlines()
                             if line.startswith('{"schema_version":'))
                self.assertEqual(value["journal"]["outcome"], "complete")
                self.assertEqual(value["journal"]["polls"], 100)
                self.assertEqual(value["qualification"], "unchanged")

    def test_podman_progression_cannot_mask_a_later_native_or_record_failure(self):
        for scenario, reason, status, diagnostic in [
                ("podman_stopped_then_inspect_fail", "terminal_inspect_failed", 9, "command_failed"),
                ("podman_stopped_then_inspect_fail_rc1", "terminal_inspect_failed", 1, "command_failed"),
                ("podman_stopped_then_malformed", "terminal_inspect_failed", 1, "invalid_or_unbound"),
                ("podman_stopped_then_foreign_cid", "terminal_inspect_failed", 1, "invalid_or_unbound"),
                ("podman_stopped_wrong_exit", "wrong_terminal_exit", 1, None)]:
            with self.subTest(scenario=scenario):
                result, journal, polls = self.assert_refused(scenario, reason, status)
                self.assertEqual(polls, 3)  # First transition, immediate refusal, diagnostic snapshot.
                self.assertEqual(sum(line == "rm -f " + CID for line in journal), 1)
                if diagnostic:
                    self.assertIn("inspect_failure=" + diagnostic + " rc=" + str(status), result.stderr)
                    # A later valid diagnostic snapshot cannot erase or qualify the failed read.
                    self.assertIn("status=exited running=false exit=137 oom=true", result.stderr)

    def test_podman_initialized_is_recognized_but_not_a_terminal_success(self):
        result, _, polls = self.assert_refused("podman_initialized", "wrong_terminal_status")
        self.assertEqual(polls, 2)
        self.assertIn("status=initialized running=false exit=137 oom=true", result.stderr)

    def test_other_terminal_exit_refused_without_waiting_for_oom(self):
        result, _, polls = self.assert_refused("wrong_exit", "wrong_terminal_exit")
        self.assertIn("exit=2 oom=false", result.stderr)
        self.assertEqual(polls, 2)  # One qualifying attempt plus failure snapshot.

    def test_terminal_status_must_be_exited(self):
        result, _, _ = self.assert_refused("wrong_status", "wrong_terminal_status")
        self.assertIn("status=dead running=false exit=137 oom=true", result.stderr)

    def test_healthy_exit_zero_requires_actual_limit_allocation_and_completion(self):
        for scenario, reason in [("healthy_hollow", "healthy_limit_missing"),
                                 ("healthy_wrong_limit", "healthy_limit_missing"),
                                 ("healthy_wrong_allocation", "healthy_allocation_missing"),
                                 ("healthy_no_completion", "healthy_completion_missing")]:
            with self.subTest(scenario=scenario):
                _, journal, _ = self.assert_refused(scenario, reason)
                self.assertEqual(len(journal), 1, "hollow healthy control admitted OOM workload")

    def test_barrier_and_terminal_require_exact_memory_and_swap(self):
        for phase in ["barrier", "terminal"]:
            for resource in ["memory", "swap"]:
                with self.subTest(phase=phase, resource=resource):
                    result, journal, _ = self.assert_refused(phase + "_" + resource + "_mismatch",
                                                              phase + "_memory_mismatch")
                    self.assertIn("cid=" + CID, result.stderr)
                    if phase == "barrier":
                        self.assertNotIn("cp - " + CID + ":/tmp", journal)
                    else:
                        self.assertIn("cp - " + CID + ":/tmp", journal)
                        self.assertIn("status=exited running=false exit=137 oom=true", result.stderr)

    def test_failed_or_malformed_native_inspection_is_not_evidence(self):
        for scenario, rc in [("inspect_fail", 9), ("terminal_inspect_fail", 9),
                             ("malformed", 1), ("wrong_identity", 1),
                             ("malformed_boolean", 1), ("malformed_exit", 1), ("malformed_memory", 1),
                             ("unknown_status", 1)]:
            with self.subTest(scenario=scenario):
                reason = "terminal_inspect_failed" if scenario == "terminal_inspect_fail" else "barrier_inspect_failed"
                result, journal, _ = self.assert_refused(scenario, reason, rc)
                self.assertIn("inspect_unavailable", result.stderr)
                diagnostic = "command_failed" if scenario in {"inspect_fail", "terminal_inspect_fail"} else "invalid_or_unbound"
                self.assertIn("inspect_failure=" + diagnostic + " rc=" + str(rc), result.stderr)
                self.assertEqual(sum(line == "rm -f " + CID for line in journal), 1)

    def test_no_allocation_before_release_including_second_log_read(self):
        for scenario in ["early_allocation", "late_allocation"]:
            with self.subTest(scenario=scenario):
                result, journal, _ = self.assert_refused(scenario, "allocated_before_release")
                self.assertNotIn("cp - " + CID + ":/tmp", journal)
                self.assertIn("allocated 128 MiB", result.stderr)

    def test_log_producer_failure_with_valid_wait_marker_is_refused(self):
        for scenario in ["logs_fail", "logs_fail_after_ready"]:
            with self.subTest(scenario=scenario):
                result, journal, _ = self.assert_refused(scenario, "barrier_logs_failed", 7)
                self.assertNotIn("cp - " + CID + ":/tmp", journal)
                self.assertIn("logs_unavailable rc=7", result.stderr)
                self.assertNotIn("fixture_logs_begin", result.stderr)

    def test_unknown_or_oversized_records_are_not_persisted(self):
        result, _, _ = self.run_case("unknown_logs")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("fixture_logs_begin", result.stderr)
        self.assertNotIn("oversize", result.stderr)
        self.assertLess(len(result.stderr), 65536)

    def test_missing_marker_and_release_failure_keep_static_diagnostics(self):
        self.assert_refused("no_marker", "barrier_marker_missing")
        result, _, _ = self.assert_refused("cp_fail", "release_archive_failed", 8)
        self.assertIn("phase=release", result.stderr)

    def test_archive_release_targets_only_owned_waiter_without_exec(self):
        result, journal, _ = self.run_case("immediate")
        self.assertEqual(result.returncode, 0, result.stderr)
        copy = journal.index("cp - " + CID + ":/tmp")
        self.assertTrue(journal[copy - 1].startswith("logs "))
        self.assertEqual(sum(line.startswith("cp ") for line in journal), 1)
        self.assertFalse(any(line.startswith("exec ") for line in journal))
        for scenario, reason in [("wrong_identity", "barrier_inspect_failed"),
                                 ("invalid_ack", "waiter_identity_invalid")]:
            with self.subTest(scenario=scenario):
                _, refused, _ = self.assert_refused(scenario, reason)
                self.assertFalse(any(line.startswith("cp ") for line in refused))

    def test_archive_preparation_failure_never_starts_waiter(self):
        result, journal, _ = self.assert_refused("archive_fail", "release_archive_prepare_failed", 8)
        self.assertIn("phase=prepare_release", result.stderr)
        self.assertEqual(len(journal), 1)

    def test_cleanup_failure_cannot_print_pass(self):
        result, journal, _ = self.assert_refused("cleanup_fail", "cleanup_failed", 6)
        self.assertIn("removal_failed rc=6", result.stderr)
        self.assertEqual(sum(line == "rm -f " + CID for line in journal), 1)
        self.assertNotIn("--memory-mib 1025 --hold 0s", "\n".join(journal))

    def test_original_failure_survives_cleanup_refusal_with_evidence_before_rm(self):
        result, journal, _ = self.assert_refused("logs_fail_cleanup_fail", "terminal_oom_unconfirmed")
        self.assertIn("removal_failed rc=6", result.stderr)
        remove = journal.index("rm -f " + CID)
        self.assertTrue(journal[remove - 3].startswith("inspect "))
        self.assertTrue(journal[remove - 2].startswith("logs "))
        self.assertTrue(journal[remove - 1].startswith("events "))
        self.assertEqual(sum(line == "rm -f " + CID for line in journal), 1)

    def test_optional_events_cannot_overrule_original_nonoom_failure(self):
        for scenario in ["no_oom", "late_oom_observation"]:
            with self.subTest(scenario=scenario):
                result, _, _ = self.assert_refused(scenario, "terminal_oom_unconfirmed")
                value = next(json.loads(line) for line in result.stderr.splitlines() if line.startswith('{"schema_version":'))
                self.assertEqual(value["journal"]["polls"], 100)
                self.assertEqual(len(value["journal"]["transitions"]), 1)
                self.assertFalse(value["journal"]["transitions"][0]["state"]["oom"])
                self.assertEqual(value["snapshot"]["oom"], scenario == "late_oom_observation")
                self.assertEqual(value["events"]["records"][0]["action"], "oom")
                self.assertEqual(value["qualification"], "unchanged")

    def test_optional_capture_failure_does_not_change_healthy_qualification(self):
        for scenario, reason in [("event_failure", "command-failed"), ("event_malformed", "invalid-or-unbound")]:
            with self.subTest(scenario=scenario):
                result, _, _ = self.run_case(scenario)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn(PASS, result.stdout)
                value = next(json.loads(line) for line in result.stderr.splitlines() if line.startswith('{"schema_version":'))
                self.assertEqual(value["events"], {"outcome": "unavailable", "reason": reason})

    def test_missing_optional_python_preserves_original_success_and_refusal(self):
        for scenario, status in [("immediate", 0), ("no_oom", 1)]:
            with self.subTest(scenario=scenario):
                result, journal, _ = self.run_case(scenario, observer=False)
                self.assertEqual(result.returncode, status, result.stderr)
                self.assertEqual(PASS in result.stdout, status == 0)
                self.assertIn("native_diagnostics=unavailable observer_missing", result.stderr)
                self.assertFalse(any(line.startswith("events ") for line in journal))

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
