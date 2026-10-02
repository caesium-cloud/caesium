import type { AtomLogoProps } from "@/components/brand/atom-logo";
import type { ClusterHealth } from "./useClusterHealth";

export function clusterAtomProps(health: ClusterHealth): Pick<AtomLogoProps, "voters" | "quorum"> {
  const cluster = health.raw?.checks?.cluster;
  if (health.raw && !cluster?.clustered) return { voters: [], quorum: "unreported" };
  const unknown = health.stale || !cluster || !cluster.observed || cluster.quorum.status === "unknown";
  return {
    voters: (cluster?.members ?? []).filter((member) => member.role.toLowerCase() === "voter").map((member) => ({
      id: String(member.id ?? member.address), leader: member.leader,
      reachable: unknown || member.reachability === "unknown" ? null : member.reachability === "reachable",
    })),
    quorum: unknown ? "unknown" : cluster.quorum.status === "unavailable" ? "lost" : "ok",
  };
}
