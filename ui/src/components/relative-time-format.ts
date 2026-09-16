/**
 * Formats a wall-clock instant relative to `now`. Past labels deliberately keep
 * the compact form used throughout the console. Future labels are opt-in so a
 * small server/browser clock skew does not make a historical event appear to
 * be scheduled in the future.
 */
export function formatRelativeTime(date: string, now: number, future = false): string {
  const time = new Date(date).getTime();
  if (!Number.isFinite(time)) return "Unknown time";

  const diff = now - time;
  const isFuture = future && diff < 0;
  if (!future && diff < 0) return "just now";

  const elapsed = Math.abs(diff);
  const round = isFuture ? Math.ceil : Math.floor;

  // A sub-second boundary is indistinguishable to an operator and prevents a
  // scheduled fire from flickering between a past and future label.
  if (elapsed < 1000) return "just now";

  const suffix = isFuture ? "" : " ago";
  const prefix = isFuture ? "in " : "";
  if (elapsed < 60_000) return `${prefix}${round(elapsed / 1000)}s${suffix}`;

  if (elapsed < 60 * 60_000) return `${prefix}${round(elapsed / 60_000)}m${suffix}`;

  if (elapsed < 24 * 60 * 60_000) return `${prefix}${round(elapsed / (60 * 60_000))}h${suffix}`;

  return `${prefix}${round(elapsed / (24 * 60 * 60_000))}d${suffix}`;
}
