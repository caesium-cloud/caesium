/**
 * Shared threshold constants for usage-style indicators.
 *
 * `<UsageBar />` and any other component that tints a value by intensity
 * pulls from this file so we have one place to tune the boundaries.
 */
export const USAGE_THRESHOLDS = {
  /** Below this, the bar reads as healthy. */
  warn: 65,
  /** At or above this, the bar reads as critical. */
  danger: 85,
} as const;

export type UsageLevel = "ok" | "warn" | "danger";

export function usageLevel(percent: number): UsageLevel {
  if (percent >= USAGE_THRESHOLDS.danger) return "danger";
  if (percent >= USAGE_THRESHOLDS.warn) return "warn";
  return "ok";
}
