#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT/scripts/helm-pod-replacement-membership.sh"

OLD='10.0.0.2:9001'
NEW='10.0.0.9:9001'
STALE=$'10.0.0.1:9001 voter\n10.0.0.2:9001 voter\n10.0.0.3:9001 voter'
FRESH=$'10.0.0.1:9001 voter\n10.0.0.9:9001 voter\n10.0.0.3:9001 voter'
WRONG_ROLE=$'10.0.0.1:9001 voter\n10.0.0.9:9001 spare\n10.0.0.3:9001 voter'

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# The first successful nodes response can be the pre-replacement snapshot. A
# later snapshot must satisfy the full address, role, and count contract.
printf '0' >"$tmp/calls"
raft_members_once() {
  local calls
  calls="$(cat "$tmp/calls")"
  printf '%s' "$((calls + 1))" >"$tmp/calls"
  if (( calls == 0 )); then
    printf '%s\n' "$STALE"
  else
    printf '%s\n' "$FRESH"
  fi
}
actual="$(replacement_wait_membership "$OLD" "$NEW" 2 0.05)"
[[ "$actual" == "$FRESH" ]]
[[ "$(cat "$tmp/calls")" -ge 2 ]]

# A transient endpoint/JSON failure is not evidence of recovery; wait for a
# later complete snapshot instead.
printf '0' >"$tmp/calls"
raft_members_once() {
  local calls
  calls="$(cat "$tmp/calls")"
  printf '%s' "$((calls + 1))" >"$tmp/calls"
  if (( calls == 0 )); then
    return 1
  fi
  printf '%s\n' "$FRESH"
}
actual="$(replacement_wait_membership "$OLD" "$NEW" 2 0.05)"
[[ "$actual" == "$FRESH" ]]
[[ "$(cat "$tmp/calls")" -ge 2 ]]

# Persistent stale membership, including the old address, cannot be accepted.
raft_members_once() { printf '%s\n' "$STALE"; }
if replacement_wait_membership "$OLD" "$NEW" 1 0.05 >"$tmp/accepted" 2>"$tmp/last"; then
  echo 'accepted stale membership' >&2
  exit 1
fi
[[ ! -s "$tmp/accepted" ]]
grep -qF "$OLD voter" "$tmp/last"

# A later read error must not overwrite the last successful stale snapshot in
# timeout diagnostics.
printf '0' >"$tmp/calls"
raft_members_once() {
  local calls
  calls="$(cat "$tmp/calls")"
  printf '%s' "$((calls + 1))" >"$tmp/calls"
  if (( calls == 0 )); then
    printf '%s\n' "$STALE"
  else
    return 1
  fi
}
if replacement_wait_membership "$OLD" "$NEW" 1 0.05 >"$tmp/accepted" 2>"$tmp/last"; then
  echo 'accepted stale membership followed by read errors' >&2
  exit 1
fi
grep -qF "$OLD voter" "$tmp/last"
grep -qF 'latest nodes read error:' "$tmp/last"

# A replacement present as a spare is still a failed voter repair.
raft_members_once() { printf '%s\n' "$WRONG_ROLE"; }
if replacement_wait_membership "$OLD" "$NEW" 1 0.05 >"$tmp/accepted" 2>"$tmp/last"; then
  echo 'accepted replacement with wrong role' >&2
  exit 1
fi
[[ ! -s "$tmp/accepted" ]]
grep -qF "$NEW spare" "$tmp/last"

echo 'replacement membership snapshot contract passed'
