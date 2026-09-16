import type { ClusterCheck, HealthResponse, Node, Reachability } from "@/lib/api";
import type { ClusterHealthState } from "./useClusterHealth";

/**
 * Cluster-health presentation logic, kept pure and separate from SystemPage so
 * the states that matter can be tested without a DOM.
 *
 * Issue #494: the console rendered the quorum numerator AND denominator from
 * `nodes.length` and marked the Nodes check `alwaysOk`, so a cluster running on
 * two of three replicas reported "All systems operational / Quorum 3/3". The
 * rules here keep four things apart:
 *
 *   configured membership   — how many voters the cluster is supposed to have
 *   observed liveness       — how many of them actually answered a probe
 *   quorum availability     — whether a majority answered
 *   unknown                 — liveness that was never determined, which is
 *                             never rendered as healthy
 */

export type Tone = "ok" | "warn" | "danger" | "muted";

export type QuorumViewStatus = "available" | "degraded" | "unavailable" | "unknown" | "unreported";

export interface QuorumView {
  status: QuorumViewStatus;
  /** Voters observed reachable. Null when liveness was never determined. */
  reachable: number | null;
  /** Configured voters — membership, not availability. */
  total: number;
  /** Majority needed for the cluster to serve writes. */
  required: number;
  /** "2/3", or "?/3" when liveness is unknown. */
  label: string;
  tone: Tone;
  detail: string;
}

const UNREPORTED: QuorumView = {
  status: "unreported",
  reachable: null,
  total: 0,
  required: 0,
  label: "—",
  tone: "muted",
  detail: "No raft cluster backs this deployment",
};

export function deriveQuorumView(cluster?: ClusterCheck | null): QuorumView {
  if (!cluster || !cluster.clustered || !cluster.quorum) {
    return UNREPORTED;
  }

  const { quorum } = cluster;
  const total = quorum.total_voters ?? 0;
  const reachable = quorum.reachable_voters ?? 0;
  const required = quorum.required_voters ?? 0;

  // Liveness was never observed, or could not be determined. Report it as
  // unknown rather than borrowing the membership count as the numerator.
  if (!cluster.observed || quorum.status === "unknown") {
    return {
      status: "unknown",
      reachable: null,
      total,
      required,
      label: total > 0 ? `?/${total}` : "?",
      tone: "warn",
      detail: cluster.observed
        ? "Voter liveness could not be determined"
        : "Waiting for the first liveness probe",
    };
  }

  const unreachable = quorum.unreachable_voters ?? 0;
  const unknown = quorum.unknown_voters ?? 0;
  const label = `${reachable}/${total}`;

  if (quorum.status === "unavailable") {
    return {
      status: "unavailable",
      reachable,
      total,
      required,
      label,
      tone: "danger",
      detail: `Quorum lost — ${reachable} of ${required} required voters reachable`,
    };
  }

  if (quorum.status === "degraded") {
    const missing = unreachable > 0 ? `${unreachable} unreachable` : `${unknown} unverified`;
    return {
      status: "degraded",
      reachable,
      total,
      required,
      label,
      tone: "warn",
      detail: `Serving without full redundancy — ${missing}`,
    };
  }

  return {
    status: "available",
    reachable,
    total,
    required,
    label,
    tone: "ok",
    detail: "All voters reachable",
  };
}

export interface SystemBanner {
  tone: Tone;
  /** Uppercase state badge, e.g. OPERATIONAL / DEGRADED / UNAVAILABLE. */
  badge: string;
  headline: string;
}

/**
 * Banner copy for the /system header. Only a fully observed, fully reachable
 * cluster is allowed to read "All systems operational".
 */
export function deriveSystemBanner(
  state: ClusterHealthState,
  health: HealthResponse | null,
): SystemBanner {
  if (state === "unknown" && !health) {
    return {
      tone: "danger",
      badge: "unknown",
      headline: "Health check failed — API unreachable",
    };
  }

  const quorum = deriveQuorumView(health?.checks?.cluster);

  if (state === "unavailable" || quorum.status === "unavailable") {
    return {
      tone: "danger",
      badge: "unavailable",
      headline: `Quorum lost — only ${quorum.reachable ?? 0} of ${quorum.total} voters reachable`,
    };
  }

  if (state === "incident") {
    return { tone: "danger", badge: "incident", headline: "One or more checks are failing" };
  }

  if (quorum.status === "unknown") {
    return {
      tone: "warn",
      badge: state === "operational" ? "degraded" : state,
      headline: "Cluster liveness unknown — voters have not been verified",
    };
  }

  if (state === "degraded" || quorum.status === "degraded") {
    return {
      tone: "warn",
      badge: "degraded",
      headline:
        quorum.status === "degraded"
          ? `Degraded — ${quorum.reachable} of ${quorum.total} voters reachable`
          : "System degraded — review the failing checks",
    };
  }

  return { tone: "ok", badge: "operational", headline: "All systems operational" };
}

/** A health check's dot colour. A check with no status is never green. */
export function checkTone(status?: string): Tone {
  switch (status) {
    case "healthy":
      return "ok";
    case "degraded":
      return "warn";
    case "unavailable":
      return "danger";
    default:
      return "warn";
  }
}

/** A node row's dot colour. Unknown liveness is amber, never green. */
export function reachabilityTone(reachability?: Reachability): Tone {
  switch (reachability) {
    case "reachable":
      return "ok";
    case "unreachable":
      return "danger";
    default:
      return "warn";
  }
}

export function reachabilityLabel(reachability?: Reachability): string {
  switch (reachability) {
    case "reachable":
      return "Reachable";
    case "unreachable":
      return "Unreachable";
    default:
      return "Unknown";
  }
}

/**
 * Nodes counted for the "Nodes" KPI: reachable members over total members.
 * Returns null when NO node's liveness was determined, so the caller renders
 * "?" — "0 reachable" and "not yet probed" are different claims, and neither is
 * the membership count.
 */
export function reachableNodeCount(nodes: Node[]): number | null {
  if (nodes.length === 0) return 0;
  const determined = nodes.some(
    (n) => n.reachability === "reachable" || n.reachability === "unreachable",
  );
  if (!determined) return null;
  return nodes.filter((n) => n.reachability === "reachable").length;
}
