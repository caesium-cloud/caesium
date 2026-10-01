# Snapshot-write phase helpers for scripts/lifecycle-tests.sh.
# Sourced, not executed. Tests source this file with lc_phase and lc_case stubs.
#
# Memory watcher cadence (#604). Each write batch runs in the background while
# lc_memory_watch_phase samples both survivors. Round 0 is taken at once and
# can land before the first catalog write, so the publisher never counts it;
# a round from 1 on is an in-write sample. After the dqlite TCP_NODELAY fix a
# 500-update batch takes about 5 s, shorter than one long-cadence wait, so the
# watcher samples on the short cadence until every survivor has the target
# number of in-write samples for the batch (asked of the publisher's own rule,
# `lifecycle-memory-sample.py covered`), then on the long cadence. Once the
# batch's done file exists no further member is sampled under an in-write
# round; a batch that ends before its first in-write round stays a gap.
# Knobs (environment, whole seconds; scripts/lifecycle-tests.sh sets them):
#   LC_MEM_FAST_INTERVAL    short cadence until coverage (default 2)
#   LC_MEM_IN_WRITE_TARGET  in-write samples per survivor per batch the short
#                           cadence aims for (default 2; the publisher needs 1)
#   LC_MEM_INTERVAL         long cadence once covered (default 15)
#   LC_MEM_SAMPLE_CAP       rounds per batch, the hard bound (default 40)

lc_stop_phase_group() {
  local pid="${1:-}"
  [[ -n "$pid" ]] || return 0
  # The phase subshell is its own process group (set -m). Killing the group
  # also stops kubectl exec children that would otherwise be reparented.
  kill -TERM -"$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

lc_memory_identity() {
  local member="$1"
  kubectl --kubeconfig "$LC_KUBE" --namespace "$LC_ID" --request-timeout=5s \
    get pod "$member" -o jsonpath='{.status.containerStatuses[?(@.name=="caesium")].containerID}{" "}{.status.containerStatuses[?(@.name=="caesium")].restartCount}' \
    2>/dev/null || true
}

lc_memory_sample_member() {
  local member="$1" reason="$2" batch="$3" batch_tag="$4" round="$5" capture ident container_id restart_count
  local dir="$LC_ART/cluster-logs/memory-captures"
  mkdir -p "$dir"
  LC_MEM_SEQ=$((LC_MEM_SEQ + 1))
  capture="$dir/${batch_tag}-${reason}-${member}-${LC_MEM_SEQ}.txt"
  ident="$(lc_memory_identity "$member")"
  container_id="${ident%% *}"
  restart_count="${ident#* }"
  [[ "$restart_count" == "$ident" ]] && restart_count=""
  case "$container_id" in
    containerd://*) container_id="${container_id#containerd://}" ;;
  esac
  python3 "$ROOT/scripts/lifecycle-memory-sample.py" sample \
    --member "$member" --reason "$reason" --batch "$batch" --round "$round" \
    --container-id "$container_id" --restart-count "$restart_count" \
    --lifecycle-id "$LC_ID" \
    --jsonl "$LC_ART/cluster-logs/snapshot-memory-samples.jsonl" \
    --capture "$capture" --artifact-root "$LC_ART" --timeout 12 \
    -- kubectl --kubeconfig "$LC_KUBE" --namespace "$LC_ID" \
      --request-timeout=12s exec -i "$member" -c caesium -- sh -s || true
}

# An optional fourth argument names the batch's done file: once it exists the
# remaining members are skipped, so a post-completion reading is never
# recorded under an in-write round.
lc_memory_sample_members() {
  local reason="$1" batch="$2" round="$3" until_file="${4:-}" batch_tag member
  printf -v batch_tag '%02d' "$batch"
  for member in caesium-0 caesium-1; do
    [[ -n "$until_file" && -f "$until_file" ]] && break
    lc_memory_sample_member "$member" "$reason" "$batch" "$batch_tag" "$round" || true
  done
}

# True once both survivors hold LC_MEM_IN_WRITE_TARGET in-write samples for
# the batch, by the same rule the publisher's finish applies.
lc_memory_in_write_covered() {
  python3 "$ROOT/scripts/lifecycle-memory-sample.py" covered \
    --lifecycle-id "$LC_ID" \
    --jsonl "$LC_ART/cluster-logs/snapshot-memory-samples.jsonl" \
    --batch "$1" --min "${LC_MEM_IN_WRITE_TARGET:-2}" >/dev/null 2>&1
}

# Round 0 can land before catalog writes. Later rounds are the in-write samples.
lc_memory_watch_phase() {
  local batch="$1" round=0 covered=0 interval i
  while [[ "$round" -lt "${LC_MEM_SAMPLE_CAP:-40}" && ! -f "$LC_PHASE_DONE" ]]; do
    if [[ "$round" == 0 ]]; then
      lc_memory_sample_members cadence "$batch" 0 || true
    else
      lc_memory_sample_members cadence "$batch" "$round" "$LC_PHASE_DONE" || true
    fi
    round=$((round + 1))
    [[ -f "$LC_PHASE_DONE" ]] && break
    if [[ "$covered" == 0 ]] && lc_memory_in_write_covered "$batch"; then
      covered=1
    fi
    if [[ "$covered" == 1 ]]; then
      interval="${LC_MEM_INTERVAL:-15}"
    else
      interval="${LC_MEM_FAST_INTERVAL:-2}"
    fi
    i=0
    while [[ "$i" -lt "$interval" && ! -f "$LC_PHASE_DONE" ]]; do
      sleep 1
      i=$((i + 1))
    done
  done
}

lc_run_snapshot_phase() {
  local batch="$1" batch_tag wait_rc
  printf -v batch_tag '%02d' "$batch"
  LC_MEM_ACTIVE=1
  if [[ -n "$LC_MEM_BATCHES" ]]; then
    LC_MEM_BATCHES="${LC_MEM_BATCHES},${batch}"
  else
    LC_MEM_BATCHES="$batch"
  fi
  LC_PHASE_DONE="$LC_ART/cluster-logs/snapshot-phase-${batch_tag}.rc"
  rm -f "$LC_PHASE_DONE"
  set +e
  set -m
  (
    set +e
    if [[ "$batch" == 0 ]]; then
      lc_phase GenerateSnapshotWrites "$LC_CAND_ID" "$(lc_base)"
    else
      LC_SNAPSHOT_BATCH="$batch" lc_phase GenerateSnapshotUpdateBatch "$LC_CAND_ID" "$(lc_base)"
    fi
    printf '%s\n' "$?" >"$LC_PHASE_DONE"
  ) &
  LC_PHASE_PID=$!
  set +m
  set -e
  lc_memory_watch_phase "$batch"
  set +e
  wait "$LC_PHASE_PID"
  wait_rc=$?
  set -e
  LC_PHASE_PID=""
  if [[ -f "$LC_PHASE_DONE" ]]; then
    LC_SNAP_RC="$(tr -dc '0-9' <"$LC_PHASE_DONE")"
  else
    LC_SNAP_RC=$wait_rc
  fi
  [[ "$LC_SNAP_RC" =~ ^[0-9]+$ ]] || LC_SNAP_RC=1
}

lc_snapshot_case() {
  local status="$1" detail="$2" evidence="${3:-}" finish_rc=0 published
  published="$evidence"
  if [[ "${LC_MEM_ACTIVE:-0}" == 1 ]]; then
    published="$LC_ART/cluster-logs/snapshot-memory-case.json"
    python3 "$ROOT/scripts/lifecycle-memory-sample.py" finish \
      --lifecycle-id "$LC_ID" \
      --jsonl "$LC_ART/cluster-logs/snapshot-memory-samples.jsonl" \
      --dest "$published" \
      --batches "$LC_MEM_BATCHES" \
      --apply-batch "${LC_MEM_APPLY_BATCH:-}" \
      --evidence "$evidence" || finish_rc=$?
    if [[ ! -s "$published" ]]; then
      printf '%s\n' '{"memory_samples":{"gap":true,"gap_detail":"memory sample publisher failed","samples":[],"memory_limit":"1Gi"}}' >"$published"
      finish_rc=1
    fi
    if [[ "$finish_rc" != 0 ]]; then
      detail="${detail}; memory samples are an evidence gap, not a pass"
      if [[ "$status" == pass ]]; then
        status=blocked
        LC_SNAP_RC=1
      fi
    fi
  fi
  lc_case snapshot-catch-up "$status" "$detail" "$published"
}
