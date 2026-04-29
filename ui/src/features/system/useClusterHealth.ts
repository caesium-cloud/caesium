import { useQuery } from "@tanstack/react-query";
import { api, type HealthResponse } from "@/lib/api";

const REFETCH_MS = 15_000;

export type ClusterHealthState = "operational" | "degraded" | "incident" | "unknown";

export interface ClusterHealth {
  state: ClusterHealthState;
  uptimeSeconds: number | null;
  raw: HealthResponse | null;
}

const KNOWN_HEALTHY = new Set(["ok", "healthy", "operational", "ready", "up"]);
const KNOWN_DEGRADED = new Set(["degraded", "warning", "warn"]);

function classify(status: string | undefined): ClusterHealthState {
  if (!status) return "unknown";
  const key = status.toLowerCase();
  if (KNOWN_HEALTHY.has(key)) return "operational";
  if (KNOWN_DEGRADED.has(key)) return "degraded";
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
 * Returns `{ state: 'unknown' }` only on genuine network failure or when no
 * response has arrived yet. Callers gate the cluster footer on
 * `state !== 'unknown'`.
 */
export function useClusterHealth(): ClusterHealth {
  const { data, isError } = useQuery({
    queryKey: ["cluster-health"],
    queryFn: api.getHealthStatus,
    refetchInterval: REFETCH_MS,
    staleTime: REFETCH_MS / 2,
    retry: 1,
  });

  if (isError || !data) {
    return { state: "unknown", uptimeSeconds: null, raw: null };
  }

  return {
    state: classify(data.status),
    uptimeSeconds: typeof data.uptime === "number" ? data.uptime / 1e9 : null,
    raw: data,
  };
}
