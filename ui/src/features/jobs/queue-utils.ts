import type { RunQueueItem } from "@/lib/api";

export function formatPriority(priority: number) {
  switch (priority) {
    case 1:
      return "low";
    case 3:
      return "high";
    default:
      return "normal";
  }
}

export function formatQueueParams(params?: Record<string, string>) {
  if (!params || Object.keys(params).length === 0) {
    return "no params";
  }
  return Object.keys(params)
    .sort()
    .map((key) => `${key}=${params[key]}`)
    .join(", ");
}

/**
 * A queued run whose claim outlived its lease: the dequeuer that took it died
 * mid-drain, so it waits on the leader's reaper rather than on capacity. The
 * server sends both `stale` and `claim_state`; either one is enough.
 */
export function isStaleQueueRow(row: RunQueueItem) {
  return row.stale === true || row.claim_state === "stale";
}

export function queuePendingReason(row: RunQueueItem) {
  if (isStaleQueueRow(row)) {
    const holder = row.claimed_by ? ` by ${row.claimed_by}` : "";
    return `Claim${holder} expired — the dequeuer that took this run died mid-drain; the leader will release it`;
  }
  if (row.claim_state === "claimed") {
    return row.claimed_by ? `Starting on ${row.claimed_by}` : "Claimed by a dequeuer; starting";
  }
  const explicitReason = row.pending_reason ?? row.wait_reason ?? row.blocked_reason ?? row.reason;
  if (row.blocked === true) {
    return explicitReason ? `Blocked: ${explicitReason}` : "Blocked by scheduler policy";
  }
  if (explicitReason) {
    return explicitReason;
  }
  return `Waiting for a run slot; ${formatPriority(row.priority)} priority at position #${row.position}`;
}
