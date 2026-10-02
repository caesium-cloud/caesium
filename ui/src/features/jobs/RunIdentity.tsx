import { IdChip } from "@/components/ui/id-chip";
import { StatusBadge } from "@/components/ui/status-badge";
import type { JobRun } from "@/lib/api";
import { formatUTCTimestamp } from "@/lib/utils";

export function RunIdentity({ run, label = "Run" }: { run: JobRun; label?: string }) {
  return <div data-testid="run-identity" className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-2 text-xs">
    <span className="font-bold text-text-2">{label}</span>
    <IdChip value={run.id} label="run id" />
    <StatusBadge status={run.status} size="sm" />
    <span className="text-text-3">{formatUTCTimestamp(run.started_at, "Unknown time")}</span>
  </div>;
}
