/**
 * Shared helpers for rendering fan-out (partition) status breakdowns.
 *
 * Both the DAG node badge/strip (`TaskNode`) and the run-timeline group lane
 * (`RunTimeline`) render the same `partition_status_counts` shape as a row of
 * proportional segments; this module is the one place that decides ordering
 * and fraction math so the two surfaces cannot silently disagree.
 */

/** Canonical left-to-right display order for partition status segments. */
export const FANOUT_STATUS_ORDER = [
  "succeeded",
  "cached",
  "failed",
  "skipped",
  "running",
  "pending",
] as const;

export interface FanoutStatusSegment {
  status: string;
  count: number;
  /** Fraction of the total, in `[0, 1]`. */
  fraction: number;
}

/**
 * Turn a `{status: count}` map into an ordered list of segments summing to 1.
 * Returns `[]` for an absent/empty/all-zero map so callers can gate rendering
 * on `.length > 0` alone.
 */
export function fanoutStatusSegments(
  counts: Record<string, number> | null | undefined,
): FanoutStatusSegment[] {
  if (!counts) return [];

  const total = Object.values(counts).reduce((sum, n) => sum + (n > 0 ? n : 0), 0);
  if (total <= 0) return [];

  const known = new Set<string>(FANOUT_STATUS_ORDER);
  const orderedStatuses = [
    ...FANOUT_STATUS_ORDER.filter((status) => (counts[status] ?? 0) > 0),
    ...Object.keys(counts).filter((status) => !known.has(status) && (counts[status] ?? 0) > 0),
  ];

  return orderedStatuses.map((status) => ({
    status,
    count: counts[status],
    fraction: counts[status] / total,
  }));
}
