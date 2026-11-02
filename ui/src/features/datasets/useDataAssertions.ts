import { useEffect } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "@/lib/api";
import { events } from "@/lib/events";

export function useDataAssertionsEnabled() {
  return (
    useQuery({
      queryKey: ["system-features"],
      queryFn: api.getSystemFeatures,
      staleTime: 60_000,
    }).data?.data_assertions_enabled === true
  );
}

/**
 * A run's recorded per-assertion results, passes included. Absent (never
 * requested) while the feature is off, so a deployment without the circuit
 * breaker makes no call to a route it does not serve. `live` polls while the
 * run executes, because results land as each task completes.
 */
export function useRunAssertions(
  jobId: string | undefined,
  runId: string | undefined,
  live = false,
) {
  const enabled = useDataAssertionsEnabled();
  return useQuery({
    queryKey: ["run-assertions", jobId, runId],
    queryFn: ({ signal }) => api.getRunAssertions(jobId!, runId!, signal),
    enabled: enabled && !!jobId && !!runId,
    staleTime: 10_000,
    refetchInterval: live ? 5_000 : false,
  });
}

export function useHoldInvalidation(enabled: boolean) {
  const client = useQueryClient();
  useEffect(() => {
    if (!enabled) return;
    const invalidate = () => {
      void client.invalidateQueries({ queryKey: ["datasets"] });
      void client.invalidateQueries({ queryKey: ["dataset-holds"] });
      void client.invalidateQueries({ queryKey: ["lineage"] });
    };
    const types = ["dataset_held", "dataset_released", "run_held_upstream"];
    types.forEach((type) => events.subscribe(type, invalidate));
    return () => types.forEach((type) => events.unsubscribe(type, invalidate));
  }, [client, enabled]);
}
