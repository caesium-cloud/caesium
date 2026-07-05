import { Link } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { GitBranch, History } from "lucide-react";
import { RelativeTime } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { EmptyState } from "@/components/ui/empty-state";
import { Skeleton } from "@/components/ui/skeleton";
import { api, type DatasetDerivation, type DatasetProducingJob } from "@/lib/api";
import { cn, shortId } from "@/lib/utils";
import { decisionLabel, displayNamespace } from "./freshness-utils";

interface DerivationsPanelProps {
  namespace: string | undefined;
  name: string | undefined;
  producingJob?: DatasetProducingJob;
}

export function DerivationsPanel({ namespace, name, producingJob }: DerivationsPanelProps) {
  const derivationsQuery = useQuery({
    queryKey: ["datasets", "derivations", namespace ?? "", name],
    queryFn: () => api.getDatasetDerivations(namespace, name!, { limit: 25 }),
    enabled: Boolean(name),
    staleTime: 30_000,
  });

  if (!name) {
    return (
      <EmptyState
        title="Select a dataset"
        subtitle="The derivation audit explains why freshness runs did or did not start."
        icon={<History className="h-12 w-12 text-text-3" />}
        className="py-10"
      />
    );
  }

  if (derivationsQuery.isLoading) {
    return (
      <div className="space-y-3">
        {Array.from({ length: 3 }).map((_, index) => (
          <Skeleton key={index} className="h-24 w-full" />
        ))}
      </div>
    );
  }

  if (derivationsQuery.error) {
    return (
      <div className="rounded-md border border-danger/30 bg-danger/5 p-4 text-sm text-danger">
        Failed to load derivations:{" "}
        {derivationsQuery.error instanceof Error ? derivationsQuery.error.message : "unknown error"}
      </div>
    );
  }

  const rows = derivationsQuery.data?.derivations ?? [];
  if (rows.length === 0) {
    return (
      <EmptyState
        title="No derivations recorded"
        subtitle="The evaluator has not appended a decision for this dataset yet."
        icon={<History className="h-12 w-12 text-text-3" />}
        className="py-10"
      />
    );
  }

  return (
    <div className="space-y-3" data-testid="dataset-derivations-panel">
      {rows.map((row) => (
        <DerivationRow
          key={row.id}
          row={row}
          selectedNamespace={namespace}
          producingJob={producingJob}
        />
      ))}
    </div>
  );
}

function DerivationRow({
  row,
  selectedNamespace,
  producingJob,
}: {
  row: DatasetDerivation;
  selectedNamespace: string | undefined;
  producingJob?: DatasetProducingJob;
}) {
  const consumed = Object.entries(row.consumed_watermarks ?? {});
  const decision = decisionLabel(row.decision);
  const isSkip = row.decision.startsWith("skipped");

  return (
    <article className="rounded-md border border-border/50 bg-obsidian/25 p-3">
      <div className="flex flex-wrap items-start justify-between gap-2">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <span className="font-mono text-xs text-text-2">{formatClock(row.created_at)}</span>
            <Badge
              variant="outline"
              className={cn(
                "text-[10px]",
                isSkip ? "border-warning/30 bg-warning/10 text-warning" : "border-success/30 bg-success/10 text-success",
              )}
            >
              {decision}
            </Badge>
          </div>
          <p className="mt-2 text-sm text-text-1">{derivationSentence(row)}</p>
        </div>
        <span className="shrink-0 text-[11px] text-text-4">
          <RelativeTime date={row.created_at} />
        </span>
      </div>

      {consumed.length > 0 ? (
        <div className="mt-3 space-y-1.5">
          <div className="text-[10px] font-semibold uppercase tracking-[0.14em] text-text-3">
            Consumed arrivals
          </div>
          <div className="flex flex-wrap gap-1.5">
            {consumed.map(([datasetName, watermark]) => (
              <Link
                key={`${datasetName}:${watermark}`}
                to="/datasets"
                search={{ status: undefined, namespace: selectedNamespace || undefined, name: datasetName }}
                className="inline-flex max-w-full items-center gap-1.5 rounded-md border border-border/60 bg-card px-2 py-1 text-[11px] text-text-2 transition-colors hover:border-cyan/40 hover:text-cyan-glow"
                title={`${displayNamespace(selectedNamespace)} / ${datasetName} @ ${watermark}`}
              >
                <GitBranch className="h-3 w-3 shrink-0" />
                <span className="truncate font-mono">{datasetName}</span>
                <span className="max-w-[9rem] truncate font-mono text-text-4">{watermark}</span>
              </Link>
            ))}
          </div>
        </div>
      ) : null}

      {row.run_id && producingJob ? (
        <div className="mt-3 text-[11px] text-text-3">
          Derived run{" "}
          <Link
            to="/jobs/$jobId/runs/$runId"
            params={{ jobId: producingJob.id, runId: row.run_id }}
            className="font-mono text-cyan-glow hover:underline"
          >
            {shortId(row.run_id)}
          </Link>
        </div>
      ) : null}
    </article>
  );
}

function derivationSentence(row: DatasetDerivation): string {
  const consumed = Object.keys(row.consumed_watermarks ?? {});
  if (row.decision === "derived") {
    if (consumed.length > 0) {
      return `${decisionTimestamp(row)} derived by ${consumed.join(", ")} advance`;
    }
    return `${decisionTimestamp(row)} derived a freshness run`;
  }
  if (row.reason) {
    return `${decisionTimestamp(row)} tick skipped - ${row.reason}`;
  }
  return `${decisionTimestamp(row)} tick skipped - ${decisionLabel(row.decision)}`;
}

function decisionTimestamp(row: DatasetDerivation): string {
  return formatClock(row.created_at);
}

function formatClock(value: string): string {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return "--:--";
  }
  return date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}
