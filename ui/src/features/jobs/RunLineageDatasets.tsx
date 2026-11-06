import { useRef } from "react";
import { Link } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { api } from "@/lib/api";
import { usePrincipal } from "@/lib/auth";
import { isTerminalRunStatus } from "./cache-utils";

const POLL_MS = 3_000;
/** How long to keep refetching after the run goes terminal, for subscriber lag. */
const SETTLE_MS = 15_000;

/**
 * The lineage dataset identities a run persisted, each a one-click entry into
 * the lineage impact view pre-filled with that dataset. Renders nothing while
 * loading, on error, or when the run persisted no lineage (including every run
 * while CAESIUM_OPEN_LINEAGE_ENABLED is off).
 */
export function RunLineageDatasets({
  jobId,
  runId,
  runStatus,
}: {
  jobId: string;
  runId: string;
  runStatus?: string;
}) {
  const principal = usePrincipal();
  // Lineage is persisted asynchronously by an event subscriber, so refetch while
  // the run is live and for a short settle window after it turns terminal.
  const settleUntil = useRef<number | null>(null);
  const terminal = isTerminalRunStatus(runStatus);
  const query = useQuery({
    queryKey: ["run-datasets", jobId, runId],
    queryFn: () => api.getRunDatasets(jobId, runId),
    retry: false,
    refetchInterval: () => {
      if (!terminal) {
        settleUntil.current = null;
        return POLL_MS;
      }
      // First tick after the run went terminal opens the settle window.
      settleUntil.current ??= Date.now() + SETTLE_MS;
      return Date.now() < settleUntil.current ? POLL_MS : false;
    },
  });
  const datasets = Array.isArray(query.data?.datasets) ? query.data.datasets : [];
  if (datasets.length === 0) {
    return null;
  }
  // The impact query is a global cross-job read a job-scoped key cannot make.
  const canOpenImpact = !principal.isScoped;

  return (
    <section
      aria-label="Run lineage datasets"
      data-testid="run-lineage-datasets"
      className="rounded-lg border border-border/50 bg-surface p-4 text-xs"
    >
      <h3 className="mb-2 text-[11px] font-bold lowercase text-text-3">Lineage datasets</h3>
      <ul className="space-y-1">
        {datasets.map((dataset) => {
          const label = `${dataset.direction} · ${dataset.namespace}/${dataset.name}`;
          const key = `${dataset.direction}:${dataset.namespace}:${dataset.name}:${dataset.task_name ?? ""}`;
          return (
            <li key={key} className="flex min-w-0 items-center gap-2 text-text-2" data-testid="run-lineage-dataset">
              <span className="min-w-0 flex-1 truncate" title={label}>
                {label}
                {dataset.task_name ? <span className="text-text-3"> · {dataset.task_name}</span> : null}
              </span>
              {canOpenImpact ? (
                <Link
                  to="/lineage"
                  search={{ namespace: dataset.namespace, name: dataset.name }}
                  className="shrink-0 text-caesium-cyan hover:underline"
                  data-testid="run-lineage-impact-link"
                >
                  View impact
                </Link>
              ) : null}
            </li>
          );
        })}
      </ul>
    </section>
  );
}
