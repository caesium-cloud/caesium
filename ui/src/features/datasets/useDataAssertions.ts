import { useEffect, useRef } from "react";
import { useQuery, useQueryClient, type QueryClient } from "@tanstack/react-query";
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
 * Marks a run's recorded results stale. A task's results land after its last
 * poll can have read them, and a retry replaces them, so the run page calls
 * this on every task and run event that can change them.
 */
export function invalidateRunAssertions(
  client: QueryClient,
  jobId: string | undefined,
  runId: string | undefined,
) {
  return client.invalidateQueries({ queryKey: ["run-assertions", jobId, runId] });
}

/**
 * A run's recorded per-assertion results, passes included. Absent (never
 * requested) while the feature is off, so a deployment without the circuit
 * breaker makes no call to a route it does not serve. `live` polls while the
 * run executes, because results land as each task completes; the moment it
 * turns false (the run went terminal) the results are read once more, so a
 * poll that preceded the last task's write cannot leave them missing.
 */
export function useRunAssertions(
  jobId: string | undefined,
  runId: string | undefined,
  live = false,
) {
  const enabled = useDataAssertionsEnabled();
  const active = enabled && !!jobId && !!runId;
  const query = useQuery({
    queryKey: ["run-assertions", jobId, runId],
    queryFn: ({ signal }) => api.getRunAssertions(jobId!, runId!, signal),
    enabled: active,
    staleTime: 10_000,
    refetchInterval: live ? 5_000 : false,
  });
  const { refetch } = query;
  const wasLive = useRef(live);
  useEffect(() => {
    if (wasLive.current && !live && active) void refetch();
    wasLive.current = live;
  }, [live, active, refetch]);
  return query;
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
