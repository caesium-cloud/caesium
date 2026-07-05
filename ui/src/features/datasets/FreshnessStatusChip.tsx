import { Badge } from "@/components/ui/badge";
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
    <Badge
      variant="outline"
      className={cn("gap-1.5 whitespace-nowrap px-2 py-0.5 text-[10px]", tone.badgeClass, className)}
      data-freshness-status={status ?? "unknown"}
    >
      <span className={cn("h-1.5 w-1.5 rounded-full", tone.dotClass)} />
      {tone.label}
    </Badge>
  );
}
