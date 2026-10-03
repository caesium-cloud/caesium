import { cn } from "@/lib/utils";
import { USAGE_THRESHOLDS, usageLevel, type UsageLevel } from "@/lib/thresholds";

interface UsageBarProps {
  /** Value in `[0, 100]`. Out-of-range values are clamped. */
  value: number;
  /** Optional inline label rendered above the bar (e.g. `"CPU"`). */
  label?: string;
  /** Render the numeric percent next to the bar. Defaults to `true`. */
  showValue?: boolean;
  className?: string;
  /** Pixel height of the bar track. Defaults to `6`. */
  height?: number;
}

const LEVEL_FILL: Record<UsageLevel, string> = {
  ok: "hsl(var(--success))",
  warn: "hsl(var(--warning))",
  danger: "hsl(var(--danger))",
};

/**
 * Horizontal usage bar (CPU, memory, disk). Tints by `USAGE_THRESHOLDS`
 * (warn / danger) so the same color story applies everywhere.
 */
export function UsageBar({
  value,
  label,
  showValue = true,
  className,
  height = 6,
}: UsageBarProps) {
  const clamped = Math.max(0, Math.min(100, value));
  const level = usageLevel(clamped);
  const fill = LEVEL_FILL[level];

  return (
    <div className={cn("flex flex-col gap-1", className)} data-level={level}>
      {label || showValue ? (
        <div className="flex items-baseline justify-between gap-3 text-[11px]">
          {label ? <span className="text-text-3">{label}</span> : <span />}
          {showValue ? (
            <span className="tabular-nums" style={{ color: fill }}>
              {clamped.toFixed(0)}%
            </span>
          ) : null}
        </div>
      ) : null}
      <div
        role="progressbar"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={clamped}
        aria-valuetext={`${clamped.toFixed(0)} percent`}
        aria-label={label}
        className="relative w-full rounded-sm bg-obsidian"
        style={{ height }}
      >
        <div
          className="h-full rounded-sm transition-[width] duration-500"
          style={{ width: `${clamped}%`, backgroundColor: fill }}
        />
        {[USAGE_THRESHOLDS.warn, USAGE_THRESHOLDS.danger].map(threshold => <span key={threshold} aria-hidden="true" className="absolute -top-0.5 bottom-[-2px] border-l" style={{ left: `${threshold}%`, borderColor: threshold === USAGE_THRESHOLDS.warn ? "hsl(var(--gold))" : "hsl(var(--danger))" }} />)}
      </div>
    </div>
  );
}

export { USAGE_THRESHOLDS };
