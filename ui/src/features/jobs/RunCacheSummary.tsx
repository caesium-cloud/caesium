import type { JobRun } from "@/lib/api";
import { Badge } from "@/components/ui/badge";
import { getRunCacheStats, formatCacheShare } from "./cache-utils";

interface RunCacheSummaryProps {
  run?: JobRun | null;
  compact?: boolean;
}

export function RunCacheSummary({ run, compact = false }: RunCacheSummaryProps) {
  const stats = getRunCacheStats(run);

  if (!run || stats.totalTasks === 0) {
    return null;
  }

  const className = compact ? "text-[10px] px-1.5 py-0" : undefined;

  return (
    <div className="flex flex-wrap items-center gap-2">
      <Badge variant="cached" className={className}>
        {stats.cacheHits} cached
      </Badge>
      <Badge variant="outline" className={className}>
        {stats.executedTasks} executed
      </Badge>
      <Badge variant="outline" className={className}>
        {formatCacheShare(stats)} cache share
      </Badge>
    </div>
  );
}
