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

/**
 * How old a health observation may be before the console stops presenting it as
 * current: three poll intervals (the console polls every 15s).
 *
 * Without this, a stalled connection leaves React Query serving the last
 * successful response indefinitely, and a cluster that was healthy when the
 * connection stalled stays green on screen forever. An observation that has
 * stopped advancing says nothing about the cluster now.
 */
export const HEALTH_OBSERVATION_MAX_AGE_MS = 45_000;

/**
 * Whether the last health response is too old to be presented as current.
 *
 * Both ages are measured using the client clock. Comparing `observed_at` with
 * the browser clock would misclassify a live cluster when the clocks differ.
 * The caller remembers when `observed_at` last changed to detect a server
 * whose background observation has stopped advancing.
 */
export function isHealthStale(
  health: HealthResponse | null | undefined,
  receivedAt: number | null,
  now: number = Date.now(),
  observedSince: number | null = receivedAt,
): boolean {
  if (!health) return false; // Not stale: absent. The caller reports that instead.
  return (receivedAt != null && now - receivedAt > HEALTH_OBSERVATION_MAX_AGE_MS) ||
    (observedSince != null && now - observedSince > HEALTH_OBSERVATION_MAX_AGE_MS);
}

/** Options shared by the derivations, so a stale observation degrades once. */
export interface DeriveOptions {
  /** The last health response is too old to be treated as current. */
  stale?: boolean;
}

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

export function deriveQuorumView(
  cluster?: ClusterCheck | null,
  options: DeriveOptions = {},
): QuorumView {
  if (!cluster || !cluster.clustered || !cluster.quorum) {
    return UNREPORTED;
  }

  const { quorum } = cluster;
  const total = quorum.total_voters ?? 0;
  const reachable = quorum.reachable_voters ?? 0;
  const required = quorum.required_voters ?? 0;

  if (options.stale) {
    return {
      status: "unknown",
      reachable: null,
      total,
      required,
      label: total > 0 ? `?/${total}` : "?",
      tone: "warn",
      detail: "Health data is stale — this observation has stopped updating",
    };
  }

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
 *
 * Node liveness is assessed alongside quorum, never folded into it: a dead
 * standby or spare cannot cost the cluster its voter majority, so quorum stays
 * `available` — but the deployment has still lost a node, and saying
 * "operational" next to an explicitly unreachable member is the same lie this
 * page was fixed for.
 */
export function deriveSystemBanner(
  state: ClusterHealthState,
  health: HealthResponse | null,
  options: DeriveOptions = {},
): SystemBanner {
  if (state === "unknown" && !health) {
    return {
      tone: "danger",
      badge: "unknown",
      headline: "Health check failed — API unreachable",
    };
  }

  if (options.stale) {
    // The last response was good, but it is old enough that it says nothing
    // about the cluster now. It must never read as operational.
    return {
      tone: "warn",
      badge: "stale",
      headline: "Health data is stale — the last observation has stopped updating",
    };
  }

  const cluster = health?.checks?.cluster;
  const quorum = deriveQuorumView(cluster);
  const nodes = cluster?.clustered ? cluster.nodes : undefined;
  const degradedBadge = state === "operational" || state === "unknown" ? "degraded" : state;

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
      badge: degradedBadge,
      headline: "Cluster liveness unknown — voters have not been verified",
    };
  }

  if (quorum.status === "degraded") {
    return {
      tone: "warn",
      badge: "degraded",
      headline: `Degraded — ${quorum.reachable} of ${quorum.total} voters reachable`,
    };
  }

  if (nodes && nodes.unreachable > 0) {
    return {
      tone: "warn",
      badge: "degraded",
      headline: `Degraded — ${nodes.reachable} of ${nodes.total} cluster nodes reachable`,
    };
  }

  if (nodes && nodes.unknown > 0) {
    return {
      tone: "warn",
      badge: degradedBadge,
      headline: `Cluster liveness unknown — ${nodes.unknown} of ${nodes.total} nodes unverified`,
    };
  }

  if (state === "degraded") {
    return { tone: "warn", badge: "degraded", headline: "System degraded — review the failing checks" };
  }

  if (state !== "operational") {
    // The server answered with something we cannot map to "all good".
    return { tone: "warn", badge: degradedBadge, headline: "Health status unknown" };
  }

  return { tone: "ok", badge: "operational", headline: "All systems operational" };
}

/** Reachable / total across every member, or null when none was determined. */
export function nodeLivenessLabel(cluster?: ClusterCheck | null): string | null {
  if (!cluster?.clustered || !cluster.nodes || cluster.nodes.total === 0) return null;
  const { nodes } = cluster;
  if (nodes.reachable === 0 && nodes.unknown === nodes.total) return `?/${nodes.total}`;
  return `${nodes.reachable}/${nodes.total}`;
}

/**
 * A health check's dot colour. A check with no status is never green.
 *
 * Accepts both vocabularies: the health-check statuses (`healthy`) and the
 * quorum statuses (`available`), so the Quorum row can be coloured straight
 * from the quorum assessment.
 */
export function checkTone(status?: string): Tone {
  switch (status) {
    case "healthy":
    case "available":
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
 * One row of the cluster node table.
 *
 * Liveness comes from the CURRENT health observation, never from the node
 * query. `/health` is unauthenticated; `/v1/system/nodes` goes through the auth
 * middleware, whose key lookup is itself a leader-dependent database read — so
 * during a quorum loss the node query is exactly the request most likely to
 * stall while `/health` keeps updating. Rendering reachability from the stale
 * cached node array put green rows underneath an outage banner.
 */
export interface NodeRow {
  address: string;
  role?: string;
  leader: boolean;
  reachability: Reachability;
  latencyMs?: number;
  /** Supplementary, from the node query. Null when it is unavailable. */
  workersBusy: number | null;
  workersTotal: number | null;
  /** True when this row's liveness came from the live health observation. */
  livenessCurrent: boolean;
}

/**
 * Merges the current health observation with the node query by address.
 *
 * Raft members take their liveness from `checks.cluster.members`. Anything the
 * node query lists that is NOT a current member — configured seeds, historical
 * workers, or rows left over from a stalled query — is reported `unknown`,
 * because nothing current says otherwise.
 *
 * It takes the whole health response rather than just the cluster check so it
 * can tell "health says this deployment has no raft cluster" from "health is
 * not answering at all". Both leave the cluster check undefined, but only the
 * first makes the node query a trustworthy liveness source: when health is
 * down, the cached node array is of unknown age and may well predate whatever
 * took health down with it.
 */
export function mergeNodeRows(
  health: HealthResponse | null | undefined,
  nodes: Node[],
  options: DeriveOptions = {},
): NodeRow[] {
  const supplementary = new Map(nodes.map((n) => [n.address, n]));
  const rows: NodeRow[] = [];
  const seen = new Set<string>();

  // A stale observation is no better evidence of liveness than a missing one.
  const healthAvailable = health != null && !options.stale;
  const cluster = health?.checks?.cluster;
  const clustered = !!cluster?.clustered;
  const observedMembers =
    clustered && cluster?.observed && !options.stale ? (cluster.members ?? []) : [];

  // Even when the observation is stale, the member LIST is still the best
  // record of which nodes exist; only their liveness is discarded.
  if (options.stale && clustered) {
    for (const member of cluster?.members ?? []) {
      seen.add(member.address);
      const extra = supplementary.get(member.address);
      rows.push({
        address: member.address,
        role: member.role,
        leader: false,
        reachability: "unknown",
        workersBusy: extra?.workers_busy ?? null,
        workersTotal: extra?.workers_total ?? null,
        livenessCurrent: false,
      });
    }
  }
  // On a dqlite deployment the raft members are the only current liveness
  // source. Only a deployment with no raft cluster at all (an external
  // database) can take liveness from the node query, because there is nothing
  // else — and there the server reports every row unknown anyway. And nothing
  // at all is trustworthy while health itself is unavailable.
  const trustNodeQuery = healthAvailable && !clustered;
  for (const member of observedMembers) {
    const extra = supplementary.get(member.address);
    seen.add(member.address);
    rows.push({
      address: member.address,
      role: member.role,
      leader: member.leader,
      reachability: member.reachability ?? "unknown",
      latencyMs: member.latency_ms,
      workersBusy: extra?.workers_busy ?? null,
      workersTotal: extra?.workers_total ?? null,
      livenessCurrent: true,
    });
  }

  for (const node of nodes) {
    if (seen.has(node.address)) continue;
    rows.push({
      address: node.address,
      role: node.role,
      leader: trustNodeQuery ? (node.leader ?? false) : false,
      // Not a current raft member — and the payload that claimed otherwise may
      // be arbitrarily stale.
      reachability: trustNodeQuery ? (node.reachability ?? "unknown") : "unknown",
      latencyMs: trustNodeQuery ? node.latency_ms : undefined,
      workersBusy: node.workers_busy ?? null,
      workersTotal: node.workers_total ?? null,
      livenessCurrent: trustNodeQuery,
    });
  }

  rows.sort((a, b) => a.address.localeCompare(b.address));
  return rows;
}

/**
 * Nodes counted for the "Nodes" KPI: reachable members over total members.
 * Returns null when NO node's liveness was determined, so the caller renders
 * "?" — "0 reachable" and "not yet probed" are different claims, and neither is
 * the membership count.
 */
export function reachableNodeCount(rows: NodeRow[]): number | null {
  if (rows.length === 0) return 0;
  const determined = rows.some(
    (r) => r.reachability === "reachable" || r.reachability === "unreachable",
  );
  if (!determined) return null;
  return rows.filter((r) => r.reachability === "reachable").length;
}
