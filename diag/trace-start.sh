#!/usr/bin/env bash
# DIAG ONLY (never merged): start bpftrace in the background with whichever
# tracepoint-argument syntax this runner's bpftrace accepts.
# usage: trace-start.sh <outdir>
set -uo pipefail
out="$1"
here="$(cd "$(dirname "$0")" && pwd)"
bpftrace --version || true
for arg in 'args.' 'args->'; do
    sed "s/ARG/$arg/g" "$here/trace.bt.in" >"$out/trace.bt"
    sudo env BPFTRACE_STRLEN=200 BPFTRACE_MAX_STRLEN=200 nohup bpftrace "$out/trace.bt" \
        >"$out/trace.log" 2>"$out/trace.err" &
    for ((i=0; i<60; i++)); do
        if grep -q '^Attaching' "$out/trace.log" 2>/dev/null; then
            echo "bpftrace attached with ${arg} syntax"
            exit 0
        fi
        if ! sudo pgrep -x bpftrace >/dev/null; then break; fi
        sleep 1
    done
    echo "bpftrace failed with ${arg} syntax:"; cat "$out/trace.err"
    sudo pkill -x bpftrace || true
    sleep 1
done
echo "TRACE_UNAVAILABLE"
exit 0
