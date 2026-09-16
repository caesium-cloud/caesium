import { describe, expect, it } from "vitest";
import type { ClusterCheck, HealthResponse, Node, Quorum, QuorumStatus } from "@/lib/api";
import {
  checkTone,
  deriveQuorumView,
  deriveSystemBanner,
  reachabilityTone,
  reachableNodeCount,
} from "../quorum";
import { classify } from "../useClusterHealth";

function quorum(overrides: Partial<Quorum> & { status: QuorumStatus }): Quorum {
  return {
    total_voters: 3,
    reachable_voters: 3,
    unreachable_voters: 0,
    unknown_voters: 0,
    required_voters: 2,
    available: true,
    degraded: false,
    leader_address: "10.244.0.8:9001",
    ...overrides,
  };
}

function clusterCheck(overrides: Partial<ClusterCheck> = {}): ClusterCheck {
  return {
    status: "healthy",
    clustered: true,
    observed: true,
    members: [],
    quorum: quorum({ status: "available" }),
    nodes: { status: "available", total: 3, reachable: 3, unreachable: 0, unknown: 0 },
    ...overrides,
  };
}

function health(cluster: ClusterCheck | undefined, status = "healthy"): HealthResponse {
  return { status, uptime: 1_000_000_000, checks: { cluster } };
}

describe("deriveQuorumView", () => {
  it("reports every voter reachable as available", () => {
    const view = deriveQuorumView(clusterCheck());

    expect(view.status).toBe("available");
    expect(view.label).toBe("3/3");
    expect(view.tone).toBe("ok");
  });

  // Issue #494: two of three replicas serving reported "Quorum 3/3".
  it("reports a crashed replica as degraded with a 2/3 numerator", () => {
    const view = deriveQuorumView(
      clusterCheck({
        status: "degraded",
        quorum: quorum({
          status: "degraded",
          reachable_voters: 2,
          unreachable_voters: 1,
          available: true,
          degraded: true,
        }),
      }),
    );

    expect(view.status).toBe("degraded");
    expect(view.label).toBe("2/3");
    expect(view.reachable).toBe(2);
    expect(view.total).toBe(3);
    expect(view.tone).toBe("warn");
    expect(view.detail).toContain("1 unreachable");
  });

  it("reports a lost quorum as an outage", () => {
    const view = deriveQuorumView(
      clusterCheck({
        status: "unavailable",
        quorum: quorum({
          status: "unavailable",
          reachable_voters: 1,
          unreachable_voters: 2,
          available: false,
          degraded: true,
        }),
      }),
    );

    expect(view.status).toBe("unavailable");
    expect(view.label).toBe("1/3");
    expect(view.tone).toBe("danger");
    expect(view.detail).toContain("Quorum lost");
  });

  it("never renders unknown liveness as healthy", () => {
    const view = deriveQuorumView(
      clusterCheck({
        status: "unknown",
        quorum: quorum({
          status: "unknown",
          reachable_voters: 0,
          unknown_voters: 3,
          available: false,
          degraded: true,
        }),
      }),
    );

    expect(view.status).toBe("unknown");
    expect(view.tone).not.toBe("ok");
    // The membership count must never stand in for the reachable count.
    expect(view.label).toBe("?/3");
    expect(view.reachable).toBeNull();
  });

  it("never renders an unobserved cluster as healthy", () => {
    const view = deriveQuorumView(clusterCheck({ observed: false }));

    expect(view.status).toBe("unknown");
    expect(view.tone).toBe("warn");
    expect(view.detail).toContain("first liveness probe");
  });

  it("reports nothing for a deployment with no raft cluster", () => {
    expect(deriveQuorumView(undefined).status).toBe("unreported");
    expect(deriveQuorumView(clusterCheck({ clustered: false })).status).toBe("unreported");
    expect(deriveQuorumView(undefined).tone).toBe("muted");
  });
});

describe("deriveSystemBanner", () => {
  it("is operational only when every voter answered", () => {
    const banner = deriveSystemBanner("operational", health(clusterCheck()));

    expect(banner.tone).toBe("ok");
    expect(banner.badge).toBe("operational");
    expect(banner.headline).toBe("All systems operational");
  });

  it("never says operational while a voter is unreachable", () => {
    const degraded = clusterCheck({
      status: "degraded",
      quorum: quorum({
        status: "degraded",
        reachable_voters: 2,
        unreachable_voters: 1,
        available: true,
        degraded: true,
      }),
    });

    const banner = deriveSystemBanner("degraded", health(degraded, "degraded"));

    expect(banner.tone).toBe("warn");
    expect(banner.badge).toBe("degraded");
    expect(banner.headline).toBe("Degraded — 2 of 3 voters reachable");
  });

  it("flags a lost quorum as an outage even if the API still answers", () => {
    const lost = clusterCheck({
      status: "unavailable",
      quorum: quorum({
        status: "unavailable",
        reachable_voters: 1,
        unreachable_voters: 2,
        available: false,
        degraded: true,
      }),
    });

    const banner = deriveSystemBanner("unavailable", health(lost, "unavailable"));

    expect(banner.tone).toBe("danger");
    expect(banner.badge).toBe("unavailable");
    expect(banner.headline).toContain("Quorum lost");
  });

  // Review P2: a crashed standby never enters the voter arithmetic, so quorum
  // stays available — the banner must still not read "All systems operational".
  it("never says operational while a non-voter is unreachable", () => {
    const standbyDown = clusterCheck({
      status: "degraded",
      quorum: quorum({ status: "available" }),
      nodes: { status: "degraded", total: 4, reachable: 3, unreachable: 1, unknown: 0 },
    });

    const banner = deriveSystemBanner("degraded", health(standbyDown, "degraded"));

    expect(banner.tone).toBe("warn");
    expect(banner.badge).toBe("degraded");
    expect(banner.headline).toBe("Degraded — 3 of 4 cluster nodes reachable");
    // Quorum itself is still correctly reported as a full voter majority.
    expect(deriveQuorumView(standbyDown).label).toBe("3/3");
    expect(deriveQuorumView(standbyDown).status).toBe("available");
  });

  it("never says operational while a non-voter is unverified", () => {
    const standbyUnknown = clusterCheck({
      status: "unknown",
      quorum: quorum({ status: "available" }),
      nodes: { status: "unknown", total: 4, reachable: 3, unreachable: 0, unknown: 1 },
    });

    const banner = deriveSystemBanner("unknown", health(standbyUnknown, "unknown"));

    expect(banner.tone).toBe("warn");
    expect(banner.headline).toContain("1 of 4 nodes unverified");
  });

  it("never falls through to operational on an unmapped server status", () => {
    const banner = deriveSystemBanner("unknown", health(clusterCheck(), "something-new"));

    expect(banner.tone).not.toBe("ok");
    expect(banner.headline).not.toBe("All systems operational");
  });

  it("never says operational when liveness is unknown", () => {
    const unknown = clusterCheck({ observed: false, status: "unknown" });

    const banner = deriveSystemBanner("operational", health(unknown));

    expect(banner.tone).toBe("warn");
    expect(banner.badge).toBe("degraded");
    expect(banner.headline).toContain("liveness unknown");
  });

  it("keeps the API-unreachable message when no response arrived", () => {
    const banner = deriveSystemBanner("unknown", null);

    expect(banner.tone).toBe("danger");
    expect(banner.headline).toContain("API unreachable");
  });
});

describe("classify", () => {
  it.each([
    ["healthy", "operational"],
    ["degraded", "degraded"],
    ["unavailable", "unavailable"],
    ["failed", "incident"],
    [undefined, "unknown"],
    ["something-new", "unknown"],
  ])("maps %s to %s", (status, expected) => {
    expect(classify(status as string | undefined)).toBe(expected);
  });
});

describe("check and node tones", () => {
  it("never colours a status-less check green", () => {
    expect(checkTone(undefined)).not.toBe("ok");
    expect(checkTone("")).not.toBe("ok");
    expect(checkTone("healthy")).toBe("ok");
    expect(checkTone("degraded")).toBe("warn");
    expect(checkTone("unavailable")).toBe("danger");
  });

  it("never colours unknown node liveness green", () => {
    expect(reachabilityTone("reachable")).toBe("ok");
    expect(reachabilityTone("unreachable")).toBe("danger");
    expect(reachabilityTone("unknown")).toBe("warn");
    expect(reachabilityTone(undefined)).toBe("warn");
  });
});

describe("reachableNodeCount", () => {
  const node = (address: string, reachability?: Node["reachability"]): Node => ({
    address,
    arch: "arm64",
    reachability,
    workers_busy: 0,
    workers_total: 1,
  });

  it("counts only nodes observed reachable", () => {
    expect(
      reachableNodeCount([
        node("a:9001", "reachable"),
        node("b:9001", "reachable"),
        node("c:9001", "unreachable"),
      ]),
    ).toBe(2);
  });

  it("returns null when no node reports liveness, so the caller renders ?", () => {
    expect(reachableNodeCount([node("a:9001"), node("b:9001")])).toBeNull();
    expect(reachableNodeCount([node("a:9001", "unknown"), node("b:9001", "unknown")])).toBeNull();
  });

  it("counts what is known even when some nodes are unverified", () => {
    expect(reachableNodeCount([node("a:9001", "reachable"), node("b:9001", "unknown")])).toBe(1);
  });

  it("returns zero for an empty cluster", () => {
    expect(reachableNodeCount([])).toBe(0);
  });
});
