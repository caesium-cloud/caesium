# Snapshot-write phase helpers for scripts/lifecycle-tests.sh.
# Sourced, not executed. Tests source this file with lc_phase and lc_case stubs.

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

lc_memory_sample_members() {
  local reason="$1" batch="$2" round="$3" batch_tag member
  printf -v batch_tag '%02d' "$batch"
  for member in caesium-0 caesium-1; do
    lc_memory_sample_member "$member" "$reason" "$batch" "$batch_tag" "$round" || true
  done
}

# Round 0 can land before catalog writes. Later rounds are the in-write samples.
lc_memory_watch_phase() {
  local batch="$1" samples=0 i
  while [[ "$samples" -lt "$LC_MEM_SAMPLE_CAP" && ! -f "$LC_PHASE_DONE" ]]; do
    lc_memory_sample_members cadence "$batch" "$samples" || true
    samples=$((samples + 1))
    [[ -f "$LC_PHASE_DONE" ]] && break
    i=0
    while [[ "$i" -lt "$LC_MEM_INTERVAL" && ! -f "$LC_PHASE_DONE" ]]; do
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
