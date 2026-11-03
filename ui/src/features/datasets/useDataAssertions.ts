import { useEffect, useRef } from "react";
import { useQuery, useQueryClient, type QueryClient } from "@tanstack/react-query";
import { api, type TaskRun } from "@/lib/api";
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
 * Makes a run's recorded results be read again from now. A read already in
 * flight started before the change that called for this one, so it is
 * cancelled first: invalidating alone reuses an in-flight read when the query
 * holds no data yet (query-core's cancelRefetch applies only once data exists),
 * and that older response would clear the stale mark and stay on screen.
 */
export async function invalidateRunAssertions(
  client: QueryClient,
  jobId: string | undefined,
  runId: string | undefined,
) {
  const queryKey = ["run-assertions", jobId, runId];
  await client.cancelQueries({ queryKey });
  await client.invalidateQueries({ queryKey });
}

/**
 * What a run's tasks say about whose evidence is on screen: the status, attempt
 * and timestamps an attempt moves, per task, plus the group mix of a fan-out.
 * A retried attempt moves started_at and completed_at even when it ends in the
 * status the previous one did. Lease renewals and log writes move none of it.
 */
export function taskEvidenceSignature(tasks: TaskRun[]): string {
  return JSON.stringify(
    tasks.map((t) => [
      t.id,
      t.task_id,
      t.status,
      t.attempt ?? null,
      t.started_at ?? null,
      t.completed_at ?? null,
      t.cache_hit ?? false,
      t.partition_status_counts ?? null,
    ]),
  );
}

/**
 * Keeps a run's recorded results in step with the run the page is showing, in
 * one place and however the change arrived (an event, a poll, a retry by
 * another client while the stream is down): when the tasks' evidence changes
 * from one read to the next, the results are read again. It does not fire on
 * the first read of a run, and an unchanged poll costs nothing. Results are
 * written before a task completes, so the completion this observes is never
 * ahead of them.
 */
export function useSyncRunAssertions(
  jobId: string | undefined,
  runId: string | undefined,
  tasks: TaskRun[] | undefined,
) {
  const client = useQueryClient();
  const seen = useRef<{ run: string; signature: string } | undefined>(undefined);
  const signature = tasks ? taskEvidenceSignature(tasks) : undefined;
  useEffect(() => {
    if (signature === undefined) return;
    const run = `${jobId}/${runId}`;
    const previous = seen.current;
    seen.current = { run, signature };
    if (previous && previous.run === run && previous.signature !== signature) {
      void invalidateRunAssertions(client, jobId, runId);
    }
  }, [client, jobId, runId, signature]);
}

/**
 * A run's recorded per-assertion results, passes included. Absent (never
 * requested) while the feature is off, so a deployment without the circuit
 * breaker makes no call to a route it does not serve. `live` polls while the
 * run executes, because results land as each task completes;
 * useSyncRunAssertions covers every other change to the run.
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
