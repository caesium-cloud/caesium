import { render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { type ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { ClusterCheck, ClusterMember, HealthResponse, Node, Reachability } from "@/lib/api";
import { api } from "@/lib/api";
import { SystemPage } from "../SystemPage";

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, to }: { children: ReactNode; to?: string }) => <a href={to ?? "#"}>{children}</a>,
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

vi.mock("@/lib/api", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api")>("@/lib/api");
  return {
    ...actual,
    api: {
      getSystemNodes: vi.fn(),
      getSystemFeatures: vi.fn(),
      getHealthStatus: vi.fn(),
      pruneCache: vi.fn(),
    },
  };
});

const mocked = vi.mocked(api);

function member(address: string, reachability: Reachability, leader = false): ClusterMember {
  return { address, role: "voter", leader, reachability, latency_ms: 2 };
}

function node(address: string, reachability: Reachability, leader = false): Node {
  return { address, arch: "arm64", role: "voter", leader, reachability, workers_busy: 0, workers_total: 4 };
}

function cluster(overrides: Partial<ClusterCheck> = {}): ClusterCheck {
  return {
    status: "healthy",
    clustered: true,
    observed: true,
    members: [
      member("10.244.0.8:9001", "reachable", true),
      member("10.244.0.9:9001", "reachable"),
      member("10.244.0.10:9001", "reachable"),
    ],
    quorum: {
      status: "available",
      total_voters: 3,
      reachable_voters: 3,
      unreachable_voters: 0,
      unknown_voters: 0,
      required_voters: 2,
      available: true,
      degraded: false,
      leader_address: "10.244.0.8:9001",
    },
    ...overrides,
  };
}

function health(clusterCheck: ClusterCheck, status = "healthy"): HealthResponse {
  return {
    status,
    uptime: 60_000_000_000,
    checks: {
      database: { status: "healthy", latency_ms: 1 },
      active_runs: { status: "healthy", count: 0 },
      triggers: { status: "healthy", count: 2 },
      nodes: { status: clusterCheck.status, count: clusterCheck.quorum.reachable_voters },
      cluster: clusterCheck,
    },
  };
}

function show() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <SystemPage />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  mocked.getSystemFeatures.mockResolvedValue({
    database_console_enabled: false,
    log_console_enabled: false,
    agent_remediation_enabled: false,
    freshness_enabled: false,
    contract_enforcement_enabled: false,
  });
  mocked.pruneCache.mockResolvedValue({ pruned: 0 });
});

describe("SystemPage cluster health", () => {
  it("shows a full quorum and an operational banner when every voter answers", async () => {
    mocked.getHealthStatus.mockResolvedValue(health(cluster()));
    mocked.getSystemNodes.mockResolvedValue([
      node("10.244.0.8:9001", "reachable", true),
      node("10.244.0.9:9001", "reachable"),
      node("10.244.0.10:9001", "reachable"),
    ]);

    show();

    await waitFor(() => expect(screen.getByTestId("quorum-count")).toHaveTextContent("3/3"));
    expect(screen.getByTestId("system-health-badge")).toHaveTextContent("operational");
    expect(screen.getByTestId("system-health-banner")).toHaveAttribute("data-tone", "ok");
    expect(screen.getByText("All systems operational")).toBeInTheDocument();
    expect(screen.getByTestId("system-nodes-kpi")).toHaveTextContent("3/3");
  });

  // Issue #494: three configured replicas, one crashed, still rendered
  // "All systems operational" and "Quorum 3/3".
  it("shows 2/3 and a degraded banner when a replica is unreachable", async () => {
    const degraded = cluster({
      status: "degraded",
      members: [
        member("10.244.0.8:9001", "reachable", true),
        member("10.244.0.9:9001", "reachable"),
        member("10.244.0.10:9001", "unreachable"),
      ],
      quorum: {
        status: "degraded",
        total_voters: 3,
        reachable_voters: 2,
        unreachable_voters: 1,
        unknown_voters: 0,
        required_voters: 2,
        available: true,
        degraded: true,
        leader_address: "10.244.0.8:9001",
      },
    });
    mocked.getHealthStatus.mockResolvedValue(health(degraded, "degraded"));
    mocked.getSystemNodes.mockResolvedValue([
      node("10.244.0.8:9001", "reachable", true),
      node("10.244.0.9:9001", "reachable"),
      node("10.244.0.10:9001", "unreachable"),
    ]);

    show();

    await waitFor(() => expect(screen.getByTestId("quorum-count")).toHaveTextContent("2/3"));
    expect(screen.queryByText("All systems operational")).not.toBeInTheDocument();
    expect(screen.getByTestId("system-health-badge")).toHaveTextContent("degraded");
    expect(screen.getByTestId("system-health-banner")).toHaveAttribute("data-tone", "warn");
    expect(screen.getByTestId("system-nodes-kpi")).toHaveTextContent("2/3");

    // The dead replica is still listed, flagged unreachable rather than green.
    const rows = screen.getAllByTestId("cluster-node-row");
    expect(rows).toHaveLength(3);
    const dead = rows.find((r) => r.dataset.address === "10.244.0.10:9001");
    expect(dead?.dataset.reachability).toBe("unreachable");
    expect(within(dead as HTMLElement).getByText("Unreachable")).toBeInTheDocument();

    // The Nodes health check is no longer unconditionally green.
    const nodesRow = screen
      .getAllByTestId("health-check-row")
      .find((r) => r.dataset.check === "Nodes");
    expect(nodesRow?.dataset.tone).toBe("warn");
  });

  it("shows an outage when quorum is lost", async () => {
    const lost = cluster({
      status: "unavailable",
      members: [
        member("10.244.0.8:9001", "reachable", true),
        member("10.244.0.9:9001", "unreachable"),
        member("10.244.0.10:9001", "unreachable"),
      ],
      quorum: {
        status: "unavailable",
        total_voters: 3,
        reachable_voters: 1,
        unreachable_voters: 2,
        unknown_voters: 0,
        required_voters: 2,
        available: false,
        degraded: true,
        leader_address: "",
      },
    });
    mocked.getHealthStatus.mockResolvedValue(health(lost, "unavailable"));
    mocked.getSystemNodes.mockResolvedValue([
      node("10.244.0.8:9001", "reachable", true),
      node("10.244.0.9:9001", "unreachable"),
      node("10.244.0.10:9001", "unreachable"),
    ]);

    show();

    await waitFor(() => expect(screen.getByTestId("quorum-count")).toHaveTextContent("1/3"));
    expect(screen.getByTestId("system-health-badge")).toHaveTextContent("unavailable");
    expect(screen.getByTestId("system-health-banner")).toHaveAttribute("data-tone", "danger");
    expect(screen.getByTestId("quorum-detail")).toHaveTextContent("Quorum lost");
  });

  it("never renders unobserved liveness as green", async () => {
    const unobserved = cluster({
      status: "unknown",
      observed: false,
      members: [],
      quorum: {
        status: "unknown",
        total_voters: 3,
        reachable_voters: 0,
        unreachable_voters: 0,
        unknown_voters: 3,
        required_voters: 2,
        available: false,
        degraded: false,
      },
    });
    mocked.getHealthStatus.mockResolvedValue(health(unobserved, "unknown"));
    mocked.getSystemNodes.mockResolvedValue([
      node("10.244.0.8:9001", "unknown"),
      node("10.244.0.9:9001", "unknown"),
      node("10.244.0.10:9001", "unknown"),
    ]);

    show();

    await waitFor(() => expect(screen.getByTestId("quorum-count")).toHaveTextContent("?/3"));
    expect(screen.getByTestId("system-health-banner")).not.toHaveAttribute("data-tone", "ok");
    expect(screen.queryByText("All systems operational")).not.toBeInTheDocument();
    expect(screen.getByTestId("system-nodes-kpi")).toHaveTextContent("?/3");
  });
});
