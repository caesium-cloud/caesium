import { StatusGlyph } from "@/components/ui/status-badge";
import { statusMeta } from "@/lib/status";
import type { DatasetStatus } from "@/lib/api";
import { cn } from "@/lib/utils";
import { freshnessTone } from "./freshness-utils";

interface FreshnessStatusChipProps {
  status: DatasetStatus | undefined;
  inheritedStale?: boolean;
  className?: string;
}

export function FreshnessStatusChip({
  status,
  inheritedStale = false,
  className,
}: FreshnessStatusChipProps) {
  const tone = freshnessTone(status, inheritedStale);
  return (
    <span className={cn("inline-flex items-center gap-2 whitespace-nowrap text-xs font-bold lowercase", tone.textClass, className)} data-freshness-status={status ?? "unknown"}>
      <StatusGlyph meta={statusMeta(tone.label === "fresh" ? "succeeded" : tone.label.includes("stale") ? "paused" : tone.label === "unknown" ? "unknown" : "failed")} />
      {tone.label}
    </span>
  );
}
