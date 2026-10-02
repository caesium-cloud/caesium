import type { JobRun } from "@/lib/api";

function runTime(run: JobRun): number {
  const started = Date.parse(run.started_at);
  return Number.isFinite(started) ? started : Date.parse(run.created_at);
}

/** Completion order does not determine which concurrent run is latest. */
export function mergeLatestRun(current: JobRun | undefined, incoming: JobRun): JobRun {
  if (!current) return incoming;
  if (current.id === incoming.id) return { ...current, ...incoming };
  const nextTime = runTime(incoming);
  const currentTime = runTime(current);
  return Number.isFinite(nextTime) && (!Number.isFinite(currentTime) || nextTime > currentTime) ? incoming : current;
}

/** Only the backend's explicit retry event may reopen a terminal execution. */
export function mergeRetriedRun(current: JobRun, incoming: JobRun | undefined): JobRun {
  if (incoming?.id !== current.id || incoming.status !== "running") return current;
  const nextTime = Date.parse(incoming.updated_at);
  const currentTime = Date.parse(current.updated_at);
  // A retained retry event must not reopen a newer terminal REST snapshot.
  if (Number.isFinite(nextTime) && Number.isFinite(currentTime) && nextTime < currentTime) return current;
  return { ...current, ...incoming, completed_at: incoming.completed_at, error: incoming.error };
}
