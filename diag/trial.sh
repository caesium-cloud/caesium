#!/usr/bin/env bash
# DIAG ONLY (never merged): one terminal-phase OOM trial that mirrors
# build/stress/smoke.sh's terminal phase and poll budget exactly.
# usage: trial.sh <runtime> <image> <single|linger> <index> <outdir> <release.tar>
set -uo pipefail
rt="$1" image="$2" mode="$3" index="$4" out="$5" release="$6"
args=(--memory-mib 128 --wait-file /tmp/release --wait-timeout 10s --hold 2s)
[ "$mode" = linger ] && args+=(--linger 2s)
since=$(date +%s)
cid=$("$rt" run -d --memory=64m --memory-swap=64m "$image" "${args[@]}" 2>/dev/null) || {
    echo "RESULT $index $mode run_failed - - - - -"; exit 0; }
ready=false
for ((p=0; p<100; p++)); do
    if "$rt" logs "$cid" 2>&1 | grep -qx 'waiting for /tmp/release'; then ready=true; break; fi
    sleep 0.05
done
if [ "$ready" != true ]; then
    echo "RESULT $index $mode barrier_missing $cid - - - -"; "$rt" rm -f "$cid" >/dev/null 2>&1; exit 0
fi
"$rt" cp - "$cid:/tmp" <"$release" >/dev/null 2>&1 || {
    echo "RESULT $index $mode cp_failed $cid - - - -"; "$rt" rm -f "$cid" >/dev/null 2>&1; exit 0; }
outcome=unconfirmed first_exit=- first_oom_running=- code=- status=-
journal=""
for ((poll=0; poll<100; poll++)); do
    state=$("$rt" inspect -f '{{.State.Status}}|{{.State.Running}}|{{.State.ExitCode}}|{{.State.OOMKilled}}' "$cid" 2>/dev/null) || {
        outcome=inspect_failed; break; }
    IFS='|' read -r status running code oom <<<"$state"
    journal+="$poll|$state"$'\n'
    [ "$running" = true ] && [ "$oom" = true ] && [ "$first_oom_running" = - ] && first_oom_running=$poll
    if [ "$running" = false ] && [ "$status" != stopped ] && [ "$status" != stopping ]; then
        [ "$first_exit" = - ] && first_exit=$poll
        if [ "$status" != exited ]; then outcome=wrong_status; break; fi
        if [ "$code" != 137 ]; then outcome=wrong_exit; break; fi
        if [ "$oom" = true ]; then outcome=ok; break; fi
    fi
    sleep 0.1
done
echo "RESULT $index $mode $outcome $cid $first_exit $first_oom_running $code $status"
if [ "$outcome" != ok ]; then
    dir="$out/fail-$index-$mode-${cid:0:12}"
    mkdir -p "$dir"
    printf '%s' "$journal" >"$dir/journal"
    "$rt" logs "$cid" >"$dir/logs" 2>&1 || true
    "$rt" inspect "$cid" >"$dir/inspect.json" 2>&1 || true
    if [ "$rt" = docker ]; then
        timeout 5 docker events --since "$since" --until "$(date +%s)" --filter "container=$cid" \
            --format '{{.TimeNano}} {{.Action}} {{json .Actor.Attributes}}' >"$dir/events" 2>&1 || true
    else
        timeout 5 podman events --since "$since" --until "$(date +%s)" --filter "container=$cid" \
            --stream=false >"$dir/events" 2>&1 || true
    fi
    sudo dmesg --time-format iso | grep -A1 "$cid" >"$dir/dmesg" 2>&1 || true
    sudo journalctl --since "@$since" --no-pager -o short-precise | grep "${cid:0:12}" >"$dir/journal-system" 2>&1 || true
fi
"$rt" rm -f "$cid" >/dev/null 2>&1 || echo "RMFAIL $cid"
