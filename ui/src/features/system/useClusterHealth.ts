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
 * Polls `/v1/system/health` (via `api.getHealth`) every 15s.
 *
 * Returns `{ state: 'unknown' }` until a response lands or if the endpoint
 * fails — callers gate the cluster footer on `state !== 'unknown'`.
 *
 * The plan's stub fallback is here intentionally: when the new
 * `/v1/system/health` shape lands (Phase 3.1), we'll widen the typing without
 * breaking the consumer surface.
 */
export function useClusterHealth(): ClusterHealth {
  const { data, isError } = useQuery({
    queryKey: ["cluster-health"],
    queryFn: api.getHealth,
    refetchInterval: REFETCH_MS,
    staleTime: REFETCH_MS / 2,
    retry: 1,
  });

  if (isError || !data) {
    return { state: "unknown", uptimeSeconds: null, raw: null };
  }

  return {
    state: classify(data.status),
    uptimeSeconds: typeof data.uptime === "number" ? data.uptime : null,
    raw: data,
  };
}
