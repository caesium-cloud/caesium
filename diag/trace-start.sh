#!/usr/bin/env bash
# DIAG ONLY (never merged): start bpftrace in the background with whichever
# tracepoint-argument syntax and string sizing this runner's bpftrace accepts.
# usage: trace-start.sh <outdir> [verbose]
set -uo pipefail
out="$1" verbose="${2:-}"
here="$(cd "$(dirname "$0")" && pwd)"
bpftrace --version || true
for attempt in 'args. 200 full' 'args. default full' 'args. default 160' 'args. 200 160' \
               'args-> 200 full' 'args-> default 160'; do
    read -r arg strlen width <<<"$attempt"
    if [ "$width" = full ]; then sub='str(\1)'; else sub="str(\\1, $width)"; fi
    sed -e "s/ARG/$arg/g" -e "s/STR(\([^)]*\))/$sub/g" "$here/trace.bt.in" >"$out/trace.bt"
    envs=(); [ "$strlen" = default ] || envs=(BPFTRACE_MAX_STRLEN="$strlen")
    sudo env "${envs[@]}" nohup bpftrace ${verbose:+-v} "$out/trace.bt" \
        >"$out/trace.log" 2>"$out/trace.err" &
    for ((i=0; i<60; i++)); do
        if grep -q '^Attaching' "$out/trace.log" 2>/dev/null; then
            echo "bpftrace attached: $attempt"
            exit 0
        fi
        if ! sudo pgrep -x bpftrace >/dev/null; then break; fi
        sleep 1
    done
    echo "bpftrace failed: $attempt"; tail -c 1500 "$out/trace.err"
    sudo pkill -x bpftrace || true
    sleep 1
done
echo "TRACE_UNAVAILABLE"
exit 0
