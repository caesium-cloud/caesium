#!/usr/bin/env bash
# Membership acceptance for the retained-PVC replacement scenario. Kept
# separate so the cached-snapshot wait can be exercised without a kind cluster.

replacement_membership_complete() {
  local members="$1" old_addr="$2" new_addr="$3"
  printf '%s\n' "$members" | awk -v old="$old_addr" -v new="$new_addr" '
    NF {
      count++
      if ($2 == "voter") voters++
      if ($1 == new && $2 == "voter") replacement_voter = 1
      if ($1 == old) old_member = 1
    }
    END {
      exit !(count == 3 && voters == 3 && replacement_voter && !old_member)
    }
  '
}

# raft_members_once is supplied by the caller. A successful database readiness
# probe does not guarantee the asynchronous cluster snapshot has refreshed yet.
# The budget covers its normal 10s refresh interval and up to 60s refresh work.
replacement_wait_membership() {
  local old_addr="$1" new_addr="$2" timeout="${3:-120}" interval="${4:-2}"
  local deadline=$((SECONDS + timeout)) members=""
  local last_success="<no successful nodes response>" last_error=""

  while (( SECONDS < deadline )); do
    if members="$(raft_members_once 2>/dev/null)"; then
      last_success="$members"
      if replacement_membership_complete "$members" "$old_addr" "$new_addr"; then
        printf '%s\n' "$members"
        return 0
      fi
    else
      last_error="nodes endpoint unavailable or returned invalid JSON"
    fi
    sleep "$interval"
  done

  printf 'last successful raft membership observation: %s\n' "$last_success" >&2
  if [[ -n "$last_error" ]]; then
    printf 'latest nodes read error: %s\n' "$last_error" >&2
  fi
  return 1
}
