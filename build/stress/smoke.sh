#!/usr/bin/env bash
# Drive the real fixture image on its producer before any integration consumer
# uses it. The runtime integration scenarios separately prove Caesium's wiring.
set -euo pipefail
runtime_cli="$1"
stress_ref="$2"
ctr=""
phase="healthy"
reason="command_failed"
cleanup_attempted=false
scratch=$(mktemp -d)
diagnostic_image=""
diagnostic_since=""
diagnostic_helper="${BASH_SOURCE[0]%/*}/../../scripts/stress-native-diagnostics.py"

# Optional observations cannot change qualification or release timing. Cgroup
# counters remain unavailable: a shared Linux daemon-host identity is unproved.
native_diagnostics() {
    if ! command -v python3 >/dev/null 2>&1 || [ ! -f "$diagnostic_helper" ]; then
        echo 'stress smoke: native_diagnostics=unavailable observer_missing' >&2
        return 0
    fi
    if python3 -I "$diagnostic_helper" --runtime "$runtime_cli" --cid "$ctr" \
        --image "$diagnostic_image" --snapshot "$scratch/diagnostic-snapshot" \
        --journal "$scratch/terminal-observations" --since "$diagnostic_since" \
        >"$scratch/diagnostic-reduced" 2>/dev/null; then
        cat "$scratch/diagnostic-reduced" >&2 || true
    else
        echo 'stress smoke: native_diagnostics=unavailable observer_failed' >&2
    fi
    return 0
}

fail() {
    reason="$1"
    exit "${2:-1}"
}

# Native stderr and unknown application records never enter the diagnostic.
# The known fixture emits short lines; retain at most 20 allowlisted records.
sanitize_logs() {
    awk 'length($0) <= 256 && ($0 == "waiting for /tmp/release" ||
        $0 == "waiting for /tmp/never-released" || $0 == "completed" ||
        $0 ~ /^cgroup memory limit ([0-9]+|max|unavailable)$/ ||
        $0 ~ /^allocated [0-9]+ MiB$/ ||
        $0 ~ /^workload (terminated by signal|exited with status) [0-9]+$/ ||
        $0 == "wait for release file: context deadline exceeded" ||
        $0 == "invalid arguments: memory must be 1..1024 MiB, hold 0..5m, wait-timeout >0..5m, linger 0..1m, with no positional arguments") {
            if (kept++ < 20) print
        }' "$1" >"$scratch/safe-logs"
}

read_logs() {
    log_status=0
    "$runtime_cli" logs --tail 20 "$ctr" >"$scratch/logs" 2>&1 || log_status=$?
    [ "$log_status" -eq 0 ] || return "$log_status"
    if ! sanitize_logs "$scratch/logs"; then log_status=1; return 1; fi
    safe_logs=$(cat "$scratch/safe-logs")
}

# Read identity, status and terminal fields together; never combine separate
# inspections into a state that the native runtime did not actually report.
read_state() {
    inspect_status=0
    "$runtime_cli" inspect -f '{{.Id}}|{{.Image}}|{{.State.Status}}|{{.State.Running}}|{{.State.ExitCode}}|{{.State.OOMKilled}}|{{.HostConfig.Memory}}|{{.HostConfig.MemorySwap}}' \
        "$ctr" >"$scratch/state" 2>/dev/null || inspect_status=$?
    if [ "$inspect_status" -ne 0 ]; then
        printf 'stress smoke: cid=%s inspect_failure=command_failed rc=%s\n' "$ctr" "$inspect_status" >&2
        return "$inspect_status"
    fi
    native_state=$(cat "$scratch/state")
    state_pattern="^${ctr}\\|(sha256:)?[0-9a-f]{64}\\|(created|initialized|running|paused|restarting|removing|stopping|stopped|exited|dead)\\|(true|false)\\|[0-9]{1,3}\\|(true|false)\\|[0-9]{1,20}\\|-?[0-9]{1,20}$"
    if ! [[ "$native_state" =~ $state_pattern ]]; then
        inspect_status=1
        printf 'stress smoke: cid=%s inspect_failure=invalid_or_unbound rc=1\n' "$ctr" >&2
        return 1
    fi
    IFS='|' read -r state_id state_image state_status running exit_code oom memory swap <<<"$native_state"
}

snapshot() {
    [ -n "$ctr" ] || return 0
    if read_state; then
        printf '%s\n' "$native_state" >"$scratch/diagnostic-snapshot" || true
        printf 'stress smoke: cid=%s image=%s status=%s running=%s exit=%s oom=%s memory=%s swap=%s\n' \
            "$state_id" "$state_image" "$state_status" "$running" "$exit_code" "$oom" "$memory" "$swap" >&2
    else
        rm -f "$scratch/diagnostic-snapshot" || true
        printf 'stress smoke: cid=%s inspect_unavailable rc=%s\n' "$ctr" "$inspect_status" >&2
    fi
    if read_logs; then
        printf 'stress smoke: cid=%s fixture_logs_begin\n' "$ctr" >&2
        cat "$scratch/safe-logs" >&2
        printf 'stress smoke: cid=%s fixture_logs_end\n' "$ctr" >&2
    else
        printf 'stress smoke: cid=%s logs_unavailable rc=%s\n' "$ctr" "$log_status" >&2
    fi
    native_diagnostics
}

cleanup() {
    [ -n "$ctr" ] && [ "$cleanup_attempted" = false ] || return 0
    # Preserve safe evidence before any removal, including cleanup refusal.
    snapshot
    cleanup_attempted=true
    cleanup_status=0
    "$runtime_cli" rm -f "$ctr" > /dev/null 2>&1 || cleanup_status=$?
    if [ "$cleanup_status" -ne 0 ]; then
        printf 'stress smoke: phase=cleanup cid=%s removal_failed rc=%s\n' "$ctr" "$cleanup_status" >&2
        return "$cleanup_status"
    fi
    ctr=""
}

finish() {
    result=$?
    trap - EXIT
    if [ "$result" -ne 0 ]; then
        printf 'stress smoke: phase=%s reason=%s rc=%s\n' "$phase" "$reason" "$result" >&2
    fi
    cleanup_status=0
    cleanup || cleanup_status=$?
    # A failed cleanup cannot qualify an otherwise successful smoke. Preserve
    # the original failure when both observation and cleanup failed.
    if [ "$result" -eq 0 ]; then result=$cleanup_status; fi
    rm -rf "$scratch"
    exit "$result"
}
trap finish EXIT

foreground() {
    command_status=0
    "$runtime_cli" run --rm "$stress_ref" "$@" >"$scratch/foreground" 2>&1 || command_status=$?
    sanitize_logs "$scratch/foreground" || fail log_sanitization_failed
    cat "$scratch/safe-logs"
}

# Memory flags belong before the image, unlike fixture arguments below.
command_status=0
"$runtime_cli" run --rm --memory=64m --memory-swap=64m "$stress_ref" --memory-mib 16 --hold 2s \
    >"$scratch/foreground" 2>&1 || command_status=$?
sanitize_logs "$scratch/foreground" || fail log_sanitization_failed
cat "$scratch/safe-logs"
[ "$command_status" -eq 0 ] || fail healthy_run_failed "$command_status"
grep -Fx 'cgroup memory limit 67108864' "$scratch/safe-logs" >/dev/null || fail healthy_limit_missing
grep -Fx 'allocated 16 MiB' "$scratch/safe-logs" >/dev/null || fail healthy_allocation_missing
grep -Fx 'completed' "$scratch/safe-logs" >/dev/null || fail healthy_completion_missing

# Upload the release without an exec process/monitor in the constrained cgroup.
# Build it before starting the waiter so preparation uses none of its wait budget.
phase="prepare_release"
command_status=0
: >"$scratch/release" && chmod 0644 "$scratch/release" && \
    tar -cf "$scratch/release.tar" -C "$scratch" release \
    > /dev/null 2>&1 || command_status=$?
[ "$command_status" -eq 0 ] || fail release_archive_prepare_failed "$command_status"

# A waiting process must not touch the large allocation before the harness has
# set its limit. Its OOM is the kernel verdict, never a fixture-chosen exit 137.
#
# --linger allocates in a child of the container's init and keeps that init
# alive 2s after the child dies. A runtime learns of an OOM kill only by
# reading the cgroup's memory.events, whose change notification the kernel
# defers up to ~10ms after an earlier one. When the victim is the container's
# only process the cgroup empties at once, and the host may remove it before
# that read: the runtime then records exit 137 without OOMKilled (see the
# stress fixture section of docs/ci.md). A surviving init keeps the cgroup and
# its kill record readable, so only the kernel's verdict decides this phase.
phase="allocate_waiter"
diagnostic_since=$(date +%s 2>/dev/null) || diagnostic_since=""
command_status=0
ctr=$("$runtime_cli" run -d --memory=64m --memory-swap=64m "$stress_ref" \
    --memory-mib 128 --wait-file /tmp/release --wait-timeout 10s --hold 2s --linger 2s 2>/dev/null) || command_status=$?
if [ "$command_status" -ne 0 ]; then
    ctr=""
    echo 'stress smoke: reconciliation_required=true unconfirmed_waiter' >&2
    fail waiter_run_failed "$command_status"
fi
if ! [[ "$ctr" =~ ^[0-9a-f]{64}$ ]]; then
    ctr="" # Refuse an unknown acknowledgement; never remove an arbitrary ID.
    echo 'stress smoke: reconciliation_required=true unconfirmed_waiter' >&2
    fail waiter_identity_invalid
fi
phase="release_barrier"
ready=false
for ((poll=0; poll<50; poll++)); do
    read_logs || fail barrier_logs_failed "$log_status"
    if [[ "$safe_logs" == *"waiting for /tmp/release"* ]]; then ready=true; break; fi
    sleep 0.1
done
[ "$ready" = true ] || fail barrier_marker_missing
read_state || fail barrier_inspect_failed "$inspect_status"
diagnostic_image="$state_image"
[ "$running" = true ] || fail waiter_not_running
[ "$memory" = 67108864 ] && [ "$swap" = 67108864 ] || fail barrier_memory_mismatch
read_logs || fail barrier_logs_failed "$log_status"
if [[ "$safe_logs" == *"allocated "* ]]; then fail allocated_before_release; fi
phase="release"
command_status=0
"$runtime_cli" cp - "$ctr:/tmp" <"$scratch/release.tar" > /dev/null 2>&1 || command_status=$?
[ "$command_status" -eq 0 ] || fail release_archive_failed "$command_status"
phase="terminal"
terminal=false
for ((poll=0; poll<100; poll++)); do
    read_state || fail terminal_inspect_failed "$inspect_status"
    printf '%s|%s\n' "$poll" "$native_state" >>"$scratch/terminal-observations" || true
    # Podman reports stopped before cleanup and stopping during shutdown.
    # Neither qualifies: wait within the same budget for a strict exited record.
    if [ "$running" = false ] && [ "$state_status" != stopped ] && [ "$state_status" != stopping ]; then
        [ "$state_status" = exited ] || fail wrong_terminal_status
        [ "$exit_code" = 137 ] || fail wrong_terminal_exit
        if [ "$oom" = true ]; then
            [ "$memory" = 67108864 ] && [ "$swap" = 67108864 ] || fail terminal_memory_mismatch
            terminal=true
            break
        fi
    fi
    sleep 0.1
done
[ "$terminal" = true ] || fail terminal_oom_unconfirmed
# The init's record corroborates that the kernel killed the workload child,
# not the init, and that the init outlived it. Only OOMKilled qualifies.
read_logs || fail terminal_logs_failed "$log_status"
grep -Fx 'workload terminated by signal 9' "$scratch/safe-logs" >/dev/null || fail terminal_supervisor_record_missing
phase="cleanup"
cleanup || fail cleanup_failed "$cleanup_status"

# Reject an accidentally unbounded workload before any allocation.
phase="allocation_bound"
foreground --memory-mib 1025 --hold 0s
[ "$command_status" -eq 2 ] || fail allocation_bound_wrong_exit
phase="wait_bound"
foreground --memory-mib 16 --wait-file /tmp/never-released --wait-timeout 100ms
[ "$command_status" -eq 1 ] || fail wait_bound_wrong_exit
echo 'stress image: resident allocation, release barrier, real OOM, and bounds passed'
