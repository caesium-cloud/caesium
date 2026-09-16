import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { type ReactNode } from "react";
import { beforeEach, expect, it, vi } from "vitest";
import { Sidebar } from "../Sidebar";
import { useClusterHealth } from "@/features/system/useClusterHealth";
import { api, type HealthResponse } from "@/lib/api";

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, to }: { children: ReactNode; to: string }) => <a href={to}>{children}</a>,
}));
vi.mock("@/features/jobs/useNavCounts", () => ({
  useNavCounts: () => ({ jobs: null, triggers: null, atoms: null, holds: null }),
}));
vi.mock("@/features/system/useClusterHealth", () => ({ useClusterHealth: vi.fn() }));
vi.mock("@/lib/api", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api")>("@/lib/api");
  return { ...actual, api: { ...actual.api, getSystemFeatures: vi.fn() } };
});

const health: HealthResponse = {
  status: "healthy",
  uptime: 60_000_000_000,
  checks: {
    cluster: {
      status: "healthy",
      clustered: true,
      observed: true,
      quorum: {
        status: "available",
        total_voters: 3,
        reachable_voters: 3,
        unreachable_voters: 0,
        unknown_voters: 0,
        required_voters: 2,
        available: true,
        degraded: false,
      },
      nodes: { status: "available", total: 3, reachable: 3, unreachable: 0, unknown: 0 },
      members: [],
    },
  },
};

beforeEach(() => {
  vi.mocked(api.getSystemFeatures).mockResolvedValue({
    database_console_enabled: false,
    log_console_enabled: false,
    agent_remediation_enabled: false,
    freshness_enabled: false,
    contract_enforcement_enabled: false,
  });
});

it("shows unknown status and quorum when a healthy observation expires", () => {
  vi.mocked(useClusterHealth).mockReturnValue({
    state: "unknown",
    raw: health,
    stale: true,
    uptimeSeconds: 60,
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}><Sidebar /></QueryClientProvider>);

  expect(screen.getByText("Unknown")).toBeInTheDocument();
  expect(screen.getByText("Health data is stale")).toBeInTheDocument();
  expect(screen.getByTestId("sidebar-quorum")).toHaveTextContent("?/3");
  expect(screen.queryByText("Operational")).not.toBeInTheDocument();
  expect(screen.queryByText("All systems nominal")).not.toBeInTheDocument();
});
