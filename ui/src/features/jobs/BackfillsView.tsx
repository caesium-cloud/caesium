import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { XCircle } from "lucide-react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { RelativeTime } from "@/components/relative-time";
import { api, type Backfill } from "@/lib/api";
import { shortId } from "@/lib/utils";

interface BackfillsViewProps {
  jobId: string;
}

function renderBackfillStatus(backfill: Backfill) {
  if (backfill.status === "running" && backfill.cancel_requested_at) {
    return (
      <Badge variant="outline" className="border-amber-500/40 text-amber-300">
        Cancelling
      </Badge>
    );
  }

  const { status } = backfill;
  switch (status) {
    case "running":
      return <Badge variant="running">Running</Badge>;
    case "succeeded":
      return <Badge variant="success">Succeeded</Badge>;
    case "failed":
      return <Badge variant="destructive">Failed</Badge>;
    case "cancelled":
      return <Badge variant="outline">Cancelled</Badge>;
  }
}

function formatDateRange(start: string, end: string): string {
  const fmt = (s: string) =>
    new Date(s).toLocaleString(undefined, {
      dateStyle: "medium",
      timeStyle: "short",
    });
  return `${fmt(start)} → ${fmt(end)}`;
}

function BackfillRow({ jobId, backfill }: { jobId: string; backfill: Backfill }) {
  const queryClient = useQueryClient();

  const cancelMutation = useMutation({
    mutationFn: () => api.cancelBackfill(jobId, backfill.id),
    onSuccess: () => {
      toast.success("Backfill cancellation requested");
      queryClient.invalidateQueries({ queryKey: ["job", jobId, "backfills"] });
    },
    onError: (err: Error) => {
      toast.error(`Failed to cancel backfill: ${err.message}`);
    },
  });

  const isRunning = backfill.status === "running";
  const isCancelling = isRunning && Boolean(backfill.cancel_requested_at);
  const progressPct =
    backfill.total_runs > 0
      ? Math.round((backfill.completed_runs / backfill.total_runs) * 100)
      : 0;

  return (
    <div className="flex items-center justify-between gap-3 p-4">
      <div className="min-w-0 flex-1 space-y-1.5">
        <div className="font-medium text-sm truncate">
          {formatDateRange(backfill.start, backfill.end)}
        </div>
        <div className="text-xs text-muted-foreground">
          <RelativeTime date={backfill.created_at} />
          {" · "}
          <span className="font-mono">{shortId(backfill.id)}</span>
          {" · "}
          reprocess: <span className="font-mono">{backfill.reprocess}</span>
          {" · "}
          max concurrent: <span className="font-mono">{backfill.max_concurrent}</span>
        </div>

        {/* Progress — shown for running and terminal states */}
        {backfill.total_runs > 0 && (
          <div className="space-y-1">
            {isRunning && (
              <div className="h-1.5 w-full max-w-xs rounded-full bg-muted overflow-hidden">
                <div
                  className="h-full rounded-full bg-primary transition-all duration-500"
                  style={{ width: `${progressPct}%` }}
                />
              </div>
            )}
            <div className="text-xs font-mono text-muted-foreground">
              {backfill.completed_runs}/{backfill.total_runs} completed
              {backfill.failed_runs > 0 && (
                <span className="text-destructive"> · {backfill.failed_runs} failed</span>
              )}
              {isCancelling && <span> · cancellation requested</span>}
            </div>
          </div>
        )}

        {/* Empty backfill (total_runs = 0, terminal) */}
        {backfill.total_runs === 0 && !isRunning && (
          <div className="text-xs font-mono text-muted-foreground">0 runs — all dates skipped</div>
        )}
      </div>

      <div className="flex shrink-0 items-center gap-2">
        {renderBackfillStatus(backfill)}
        {isRunning && (
          <Button
            variant="outline"
            size="sm"
            className="h-7 px-2 text-xs"
            onClick={() => cancelMutation.mutate()}
            disabled={cancelMutation.isPending || isCancelling}
          >
            <XCircle className="mr-1 h-3.5 w-3.5" />
            {isCancelling ? "Cancelling" : "Cancel"}
          </Button>
        )}
      </div>
    </div>
  );
}

export function BackfillsView({ jobId }: BackfillsViewProps) {
  const { data: backfills, isLoading } = useQuery({
    queryKey: ["job", jobId, "backfills"],
    queryFn: () => api.getBackfills(jobId),
    refetchInterval: (query) => {
      const data = query.state.data;
      if (!data) return false;
      return data.some((b) => b.status === "running") ? 3000 : false;
    },
  });

  const sorted = backfills
    ? [...backfills].sort(
        (a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime(),
      )
    : [];

  if (isLoading) {
    return <div className="p-8 text-center text-muted-foreground text-sm">Loading…</div>;
  }

  return (
    <div className="rounded-md border bg-card divide-y">
      {sorted.length === 0 ? (
        <div className="p-8 text-center text-muted-foreground text-sm">
          No backfills found for this job.
        </div>
      ) : (
        sorted.map((b) => <BackfillRow key={b.id} jobId={jobId} backfill={b} />)
      )}
    </div>
  );
}
