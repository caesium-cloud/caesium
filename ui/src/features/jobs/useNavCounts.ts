import { useQueries } from "@tanstack/react-query";
import { api } from "@/lib/api";

const REFETCH_MS = 30_000;

export interface NavCounts {
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
  const results = useQueries({
    queries: [
      {
        queryKey: ["nav-counts", "jobs"],
        queryFn: api.getJobs,
        refetchInterval: REFETCH_MS,
        staleTime: REFETCH_MS / 2,
      },
      {
        queryKey: ["nav-counts", "triggers"],
        queryFn: api.getTriggers,
        refetchInterval: REFETCH_MS,
        staleTime: REFETCH_MS / 2,
      },
      {
        queryKey: ["nav-counts", "atoms"],
        queryFn: api.getAtoms,
        refetchInterval: REFETCH_MS,
        staleTime: REFETCH_MS / 2,
      },
    ],
  });

  const [jobs, triggers, atoms] = results;
  return {
    jobs: jobs.data ? jobs.data.length : null,
    triggers: triggers.data ? triggers.data.length : null,
    atoms: atoms.data ? atoms.data.length : null,
  };
}
