import { useQueries, useQuery } from "@tanstack/react-query";
import { usePrincipal } from "@/lib/auth";
import { useDataAssertionsEnabled, useHoldInvalidation } from "@/features/datasets/useDataAssertions";
import { api } from "@/lib/api";

const REFETCH_MS = 30_000;

export interface NavCounts {
  datasets: number | null;
  holds: number | null;
  jobs: number | null;
  triggers: number | null;
  atoms: number | null;
}

/**
 * Aggregates the counts shown in the sidebar nav badges.
 *
 * The execution plan (§0.5) calls for a single batched query against
 * `GET /v1/jobs?count_only=true` per route. That endpoint does not exist
 * yet — until it does, we fall back to the existing list endpoints and
 * count their length client-side.
 *
 * API gap (tracked in `docs/ui-refresh-execution-plan.md` §"API gap summary"):
 *   - `GET /v1/jobs?count_only=true`
 *   - `GET /v1/jobs/summary` (status counts, used by 1.1)
 */
export function useNavCounts(): NavCounts {
  const assertionsEnabled = useDataAssertionsEnabled();
  const { data: features } = useQuery({
    queryKey: ["system-features"],
    queryFn: api.getSystemFeatures,
    staleTime: 60_000,
  });
  const principal = usePrincipal();
  useHoldInvalidation(assertionsEnabled);
  const datasetsEnabled = features?.freshness_enabled === true && !principal.isScoped;
  const datasets = useQuery({
    queryKey: ["datasets", "nav"],
    queryFn: () => api.getDatasets({ limit: 1 }),
    enabled: datasetsEnabled,
    refetchInterval: REFETCH_MS,
  });
  const holds = useQuery({
    queryKey: ["dataset-holds", "nav"],
    queryFn: () => api.getDatasetHolds({ status: "active", limit: 1 }),
    enabled: assertionsEnabled && !principal.isScoped,
    refetchInterval: REFETCH_MS,
  });
  const results = useQueries({
    queries: [
      {
        queryKey: ["jobs"],
        queryFn: api.getJobs,
        refetchInterval: REFETCH_MS,
        staleTime: REFETCH_MS / 2,
      },
      {
        queryKey: ["triggers"],
        queryFn: api.getTriggers,
        refetchInterval: REFETCH_MS,
        staleTime: REFETCH_MS / 2,
      },
      {
        queryKey: ["atoms"],
        queryFn: api.getAtoms,
        refetchInterval: REFETCH_MS,
        staleTime: REFETCH_MS / 2,
      },
    ],
  });

  const [jobs, triggers, atoms] = results;
  return {
    datasets: datasetsEnabled && !datasets.error ? datasets.data?.total ?? null : null,
    holds: assertionsEnabled && !principal.isScoped && !holds.error ? holds.data?.total ?? null : null,
    jobs: jobs.data ? jobs.data.length : null,
    triggers: triggers.data ? triggers.data.length : null,
    atoms: atoms.data ? atoms.data.length : null,
  };
}
