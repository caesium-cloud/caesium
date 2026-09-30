import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type HealthResponse } from "@/lib/api";
import { isHealthStale } from "./quorum";

const REFETCH_MS = 15_000;
// How often the age of the last observation is re-evaluated. A stalled refetch
// produces no render of its own, so without a tick the banner would only flip
// on the next successful response — which may never arrive.
const STALE_TICK_MS = 5_000;

export type ClusterHealthState =
  | "operational"
  | "degraded"
  | "unavailable"
  | "incident"
  | "unknown";

export interface ClusterHealth {
  state: ClusterHealthState;
  uptimeSeconds: number | null;
  raw: HealthResponse | null;
  /**
   * The last successful response is too old to be treated as current — the
   * poll is stalling, or the server's own observation has stopped advancing.
   * React Query keeps serving cached data through a pending refetch, so without
   * this a cluster that was healthy when the connection stalled would stay
   * green on screen indefinitely.
   */
  stale: boolean;
}

interface ObservedHealth {
  response: HealthResponse;
  observedAt: string | null;
  observedSince: number | null;
}

const KNOWN_HEALTHY = new Set(["ok", "healthy", "operational", "ready", "up"]);
const KNOWN_DEGRADED = new Set(["degraded", "warning", "warn"]);
// The server reports `unavailable` when the dqlite cluster has lost quorum: it
// is still answering HTTP (so the pod stays live and ready) but cannot serve
// writes. That is an outage, not a degradation.
const KNOWN_UNAVAILABLE = new Set(["unavailable", "no_quorum"]);

export function classify(status: string | undefined): ClusterHealthState {
  if (!status) return "unknown";
  const key = status.toLowerCase();
  if (KNOWN_HEALTHY.has(key)) return "operational";
  if (KNOWN_DEGRADED.has(key)) return "degraded";
  if (KNOWN_UNAVAILABLE.has(key)) return "unavailable";
  if (key === "down" || key === "incident" || key === "error" || key === "failed") {
    return "incident";
  }
  return "unknown";
}

/**
 * Polls `/health` every 15s and classifies the cluster state.
 *
 * Uses `api.getHealthStatus` (non-throwing variant) so HTTP 503 responses
 * from the server — which signal degraded or incident health — still deliver
 * a parseable JSON body. `api.getHealth` throws on any non-2xx, making those
 * states invisible and silently falling back to `unknown`.
 *
 * `data.uptime` is a Go `time.Duration` (nanoseconds); divide by 1e9 to get
 * seconds, matching the conversion in `SystemPage.tsx`.
 *
 * Returns `{ state: 'unknown' }` when the network fails, no response has
 * arrived, or the last observation expires. Callers use `raw` to distinguish
 * a missing response from an expired one.
 */
export function useClusterHealth(): ClusterHealth {
  const queryClient = useQueryClient();
  const { data, isError, dataUpdatedAt } = useQuery({
    queryKey: ["cluster-health"],
    queryFn: async (): Promise<ObservedHealth> => {
      const response = await api.getHealthStatus();
      const observedAt = response.checks?.cluster?.observed_at ?? null;
      const previous = queryClient.getQueryData<ObservedHealth>(["cluster-health"]);
      return {
        response,
        observedAt,
        // Keep the first client receipt time while the server repeats the
        // same observation. A new observation starts a fresh age window.
        observedSince: observedAt
          ? previous?.observedAt === observedAt ? previous.observedSince : Date.now()
          : null,
      };
    },
    refetchInterval: REFETCH_MS,
    staleTime: REFETCH_MS / 2,
    retry: 1,
  });

  // Staleness is a function of elapsed time, and a stalled refetch never
  // re-renders on its own, so the age is re-evaluated on a slow tick.
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), STALE_TICK_MS);
    return () => clearInterval(timer);
  }, []);

  if (isError || !data) {
    return { state: "unknown", uptimeSeconds: null, raw: null, stale: false };
  }

  const stale = isHealthStale(data.response, dataUpdatedAt || null, now, data.observedSince);

  return {
    state: stale ? "unknown" : classify(data.response.status),
    uptimeSeconds: typeof data.response.uptime === "number" ? data.response.uptime / 1e9 : null,
    raw: data.response,
    stale,
  };
}
