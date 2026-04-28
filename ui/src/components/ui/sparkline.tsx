import { useEffect, useState } from "react";
import { cn } from "@/lib/utils";
import { statusMeta } from "@/lib/status";

export interface RunSummary {
  status: string;
  /** Run duration in seconds. May be missing for in-flight runs. */
  duration?: number | null;
}

interface SparklineProps {
  /** Run history, oldest → newest. The component renders newest on the right. */
  runs: RunSummary[];
  width?: number;
  height?: number;
  className?: string;
}

/**
 * Compact run-history sparkline. Each bar's height encodes duration
 * relative to the max in the window; bar color = `statusMeta(status).fg`.
 *
 * Lazy-renders a single frame after mount so it never blocks initial paint.
 * Pass an empty array to render the placeholder dash.
 */
export function Sparkline({
  runs,
  width = 90,
  height = 22,
  className,
}: SparklineProps) {
  const [ready, setReady] = useState(false);
  useEffect(() => {
    const id = requestAnimationFrame(() => setReady(true));
    return () => cancelAnimationFrame(id);
  }, []);

  if (!runs || runs.length === 0) {
    return (
      <span
        aria-label="no runs"
        className={cn("inline-flex items-center text-[11px] text-text-4", className)}
      >
        —
      </span>
    );
  }

  if (!ready) {
    return (
      <span
        aria-hidden="true"
        className={cn("inline-block", className)}
        style={{ width, height }}
      />
    );
  }

  const max = Math.max(...runs.map((r) => r.duration ?? 0), 60);
  const count = runs.length;
  const gap = 2;
  const barWidth = Math.max(1, (width - (count - 1) * gap) / count);

  return (
    <svg
      width={width}
      height={height}
      role="img"
      aria-label={`${count} recent runs`}
      className={cn("block", className)}
    >
      {runs.map((run, idx) => {
        const meta = statusMeta(run.status);
        const isRunning = meta.label === "running";
        const value = isRunning ? height * 0.6 : Math.max(3, ((run.duration ?? 0) / max) * height);
        return (
          <rect
            key={idx}
            x={idx * (barWidth + gap)}
            y={height - value}
            width={barWidth}
            height={value}
            rx={1.5}
            fill={meta.fg}
            opacity={isRunning ? 0.95 : 0.85}
          >
            {isRunning ? (
              <animate
                attributeName="opacity"
                values="0.6;1;0.6"
                dur="1.4s"
                repeatCount="indefinite"
              />
            ) : null}
          </rect>
        );
      })}
    </svg>
  );
}
