/**
 * Formats a wall-clock instant relative to `now`. Past labels deliberately keep
 * the compact form used throughout the console; future labels are directional
 * so scheduled work and cache expiry do not look immediately due.
 */
export function formatRelativeTime(date: string, now: number): string {
  const time = new Date(date).getTime();
  if (!Number.isFinite(time)) return "Unknown time";

  const diff = now - time;
  const seconds = Math.floor(Math.abs(diff) / 1000);

  // A sub-second boundary is indistinguishable to an operator and prevents a
  // scheduled fire from flickering between a past and future label.
  if (seconds === 0) return "just now";

  const suffix = diff < 0 ? "" : " ago";
  const prefix = diff < 0 ? "in " : "";
  if (seconds < 60) return `${prefix}${seconds}s${suffix}`;

  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${prefix}${minutes}m${suffix}`;

  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${prefix}${hours}h${suffix}`;

  const days = Math.floor(hours / 24);
  return `${prefix}${days}d${suffix}`;
}
