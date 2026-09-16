#!/usr/bin/env bash
# Drive the real fixture image on its producer before any integration consumer
# uses it. The runtime integration scenarios separately prove Caesium's wiring.
set -euo pipefail
runtime_cli="$1"
stress_ref="$2"
ctr=""
cleanup() {
    if [ -n "$ctr" ]; then
        "$runtime_cli" rm -f "$ctr" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT

"$runtime_cli" run --rm --memory=64m --memory-swap=64m "$stress_ref" --memory-mib 16 --hold 2s

# A waiting process must not touch the large allocation before the harness has
# set its limit. Its OOM is the kernel verdict, never a fixture-chosen exit 137.
ctr=$("$runtime_cli" run -d --memory=64m --memory-swap=64m "$stress_ref" \
    --memory-mib 128 --wait-file /tmp/release --wait-timeout 10s --hold 2s)
for _ in $(seq 1 50); do
    if "$runtime_cli" logs "$ctr" | grep -q '^waiting for /tmp/release$'; then
        break
    fi
    sleep 0.1
done
"$runtime_cli" logs "$ctr" | grep -q '^waiting for /tmp/release$'
test "$("$runtime_cli" inspect -f '{{.State.Running}}' "$ctr")" = true
if "$runtime_cli" logs "$ctr" | grep -q '^allocated '; then
    echo 'stress fixture allocated before release' >&2
    exit 1
fi
"$runtime_cli" exec "$ctr" touch /tmp/release
for _ in $(seq 1 100); do
    if [ "$("$runtime_cli" inspect -f '{{.State.Running}}' "$ctr")" = false ]; then
        break
    fi
    sleep 0.1
done
test "$("$runtime_cli" inspect -f '{{.State.Running}}' "$ctr")" = false
test "$("$runtime_cli" inspect -f '{{.State.ExitCode}}' "$ctr")" = 137
test "$("$runtime_cli" inspect -f '{{.State.OOMKilled}}' "$ctr")" = true
cleanup
ctr=""

# Reject an accidentally unbounded workload before any allocation.
rc=0
"$runtime_cli" run --rm "$stress_ref" --memory-mib 1025 --hold 0s || rc=$?
test "$rc" = 2
rc=0
"$runtime_cli" run --rm "$stress_ref" --memory-mib 16 --wait-file /tmp/never-released --wait-timeout 100ms || rc=$?
test "$rc" = 1
echo 'stress image: resident allocation, release barrier, real OOM, and bounds passed'
