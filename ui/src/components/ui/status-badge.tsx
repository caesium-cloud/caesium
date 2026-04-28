import { type CSSProperties } from "react";
import { cn } from "@/lib/utils";
import { statusMeta } from "@/lib/status";

export type StatusBadgeVariant = "filled" | "soft" | "dot";
export type StatusBadgeSize = "sm" | "md";

interface StatusBadgeProps {
  status: string;
  variant?: StatusBadgeVariant;
  size?: StatusBadgeSize;
  /** Override the rendered label; defaults to the canonical status label. */
  label?: string;
  className?: string;
}

/**
 * Status pill backed by `statusMeta`. Three visual variants:
 * - `filled` (default): tinted background + matching border
 * - `soft`: transparent background, colored text only
 * - `dot`: dot + label, no chrome
 */
export function StatusBadge({
  status,
  variant = "filled",
  size = "md",
  label,
  className,
}: StatusBadgeProps) {
  const meta = statusMeta(status);
  const text = label ?? meta.label;

  const sizing =
    size === "sm"
      ? "px-1.5 py-[2px] text-[10px]"
      : "px-2 py-[3px] text-[11px]";
  const dotSize = size === "sm" ? "h-1.5 w-1.5" : "h-2 w-2";

  if (variant === "dot") {
    return (
      <span
        className={cn(
          "inline-flex items-center gap-1.5 font-medium uppercase tracking-[0.04em]",
          size === "sm" ? "text-[10px]" : "text-[11px]",
          className,
        )}
        style={{ color: meta.fg }}
        data-status={meta.label}
      >
        <span
          aria-hidden="true"
          className={cn("inline-block rounded-full", dotSize, meta.dotClass)}
          style={{ backgroundColor: meta.fg }}
        />
        {text}
      </span>
    );
  }

  const fill: CSSProperties =
    variant === "soft"
      ? { color: meta.fg, backgroundColor: "transparent", borderColor: "transparent" }
      : { color: meta.fg, backgroundColor: meta.bg, borderColor: meta.border };

  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 rounded-full border font-semibold uppercase tracking-[0.04em] whitespace-nowrap",
        sizing,
        className,
      )}
      style={fill}
      data-status={meta.label}
      data-variant={variant}
    >
      <span
        aria-hidden="true"
        className={cn("inline-block rounded-full", dotSize, meta.dotClass)}
        style={{ backgroundColor: meta.fg }}
      />
      {text}
    </span>
  );
}
