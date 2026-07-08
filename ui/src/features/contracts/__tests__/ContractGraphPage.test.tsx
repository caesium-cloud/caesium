import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ContractGraph } from "../ContractGraphPage";
import { api } from "@/lib/api";

vi.mock("@tanstack/react-router", () => ({
  Link: ({
    to,
    params,
    children,
    ...props
  }: {
    to: string;
    params?: Record<string, string>;
    children: React.ReactNode;
    [key: string]: unknown;
  }) => {
    const href = to === "/jobs/$jobId" && params?.jobId ? `/jobs/${params.jobId}` : to;
    return (
      <a href={href} {...props}>
        {children}
      </a>
    );
  },
  useNavigate: () => vi.fn(),
  useSearch: () => ({}),
}));

vi.mock("reactflow", () => {
  const ReactFlow = ({
    nodes = [],
    edges = [],
    nodeTypes = {},
  }: {
    nodes?: Array<Record<string, unknown>>;
    edges?: Array<Record<string, unknown>>;
    nodeTypes?: Record<string, React.ComponentType<{ data: Record<string, unknown> }>>;
  }) => (
    <div data-testid="reactflow-mock">
      {nodes.map((node) => {
        const data = (node.data ?? {}) as Record<string, unknown>;
        const Component = nodeTypes[String(node.type)];
        return Component ? (
          <Component key={String(node.id)} data={data} />
        ) : (
          <div key={String(node.id)} data-testid={`node-${String(node.id)}`}>
            {String(data.label ?? node.id)}
          </div>
        );
      })}
      {edges.map((edge) => {
        const data = (edge.data ?? {}) as Record<string, unknown>;
        const style = (edge.style ?? {}) as Record<string, unknown>;
        return (
          <div
            key={String(edge.id)}
            data-testid={String(data.testId)}
            data-edge-class={String(data.edgeClass)}
            data-edge-verdict={String(data.verdict)}
            data-stroke={String(style.stroke)}
            data-dasharray={String(style.strokeDasharray ?? "")}
          >
            {String(data.edgeClass)}
          </div>
        );
      })}
    </div>
  );

  return {
    __esModule: true,
    default: ReactFlow,
    Background: () => null,
    BaseEdge: () => null,
    Controls: () => null,
    EdgeLabelRenderer: ({ children }: { children: React.ReactNode }) => <>{children}</>,
    Handle: () => null,
    MarkerType: { ArrowClosed: "arrow-closed" },
    Position: { Left: "left", Right: "right", Top: "top", Bottom: "bottom" },
    getSmoothStepPath: () => ["M0,0L1,1", 0, 0],
  };
});

vi.mock("dagre", () => {
  class Graph {
    private nodes = new Map<string, { x: number; y: number }>();

    setDefaultEdgeLabel() {}
    setGraph() {}
    setNode(id: string) {
      this.nodes.set(id, { x: 0, y: 0 });
    }
    setEdge() {}
    node(id: string) {
      return this.nodes.get(id) ?? { x: 0, y: 0 };
    }
  }

  return {
    __esModule: true,
    default: {
      graphlib: { Graph },
      layout: () => undefined,
    },
  };
});

vi.mock("@/lib/api", () => {
  class MockApiError extends Error {
    status: number;
    kind: string;
    constructor(status: number, message: string, kind = "unknown") {
      super(message);
      this.status = status;
      this.kind = kind;
      this.name = "ApiError";
    }
  }

  return {
    ApiError: MockApiError,
    api: {
      getSystemFeatures: vi.fn(),
      getContractGraph: vi.fn(),
      getJobs: vi.fn(),
    },
  };
});

function createWrapper() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return ({ children }: { children: React.ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  );
}

function mockFeatures(contractEnabled: boolean) {
  vi.mocked(api.getSystemFeatures).mockResolvedValue({
    database_console_enabled: false,
    log_console_enabled: false,
    agent_remediation_enabled: false,
    freshness_enabled: false,
    contract_enforcement_enabled: contractEnabled,
  });
}

describe("ContractGraph", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("renders a not-found state and does not fetch the graph when disabled", async () => {
    mockFeatures(false);
    render(<ContractGraph initialDataset="" onDatasetSubmit={vi.fn()} />, { wrapper: createWrapper() });

    await expect(screen.findByTestId("not-found-state")).resolves.toBeVisible();
    expect(screen.getByText("Contracts disabled")).toBeInTheDocument();
    expect(api.getContractGraph).not.toHaveBeenCalled();
  });

  it("renders the contract empty state for a graph with no edges", async () => {
    mockFeatures(true);
    vi.mocked(api.getContractGraph).mockResolvedValue({ nodes: [], edges: [] });
    vi.mocked(api.getJobs).mockResolvedValue([]);

    render(<ContractGraph initialDataset="" onDatasetSubmit={vi.fn()} />, { wrapper: createWrapper() });

    await expect(screen.findByTestId("contracts-empty-state")).resolves.toBeVisible();
    expect(screen.getByText("No contract edges yet")).toBeInTheDocument();
    expect(screen.getByText(/declare dataset schemas/)).toBeInTheDocument();
  });

  it("renders classed edges, dataset nodes, and job links", async () => {
    mockFeatures(true);
    vi.mocked(api.getJobs).mockResolvedValue([
      { id: "producer-id", alias: "producer", trigger_id: "", labels: {}, annotations: {}, paused: false, created_at: "", updated_at: "" },
      { id: "consumer-id", alias: "consumer", trigger_id: "", labels: {}, annotations: {}, paused: false, created_at: "", updated_at: "" },
    ]);
    vi.mocked(api.getContractGraph).mockResolvedValue({
      nodes: [
        { id: "job:producer", kind: "job", alias: "producer" },
        { id: "job:consumer", kind: "job", alias: "consumer" },
        { id: "dataset:lake/customers", kind: "dataset", dataset: { namespace: "lake", name: "customers" } },
      ],
      edges: [
        {
          id: "declared:producer:consumer:lake/customers",
          from: "job:producer",
          to: "job:consumer",
          class: "declared",
          verdict: "breaking",
          dataset: { namespace: "lake", name: "customers" },
          findings: [{ verdict: "breaking", detail: "required field removed" }],
        },
      ],
    });

    render(<ContractGraph initialDataset="lake/customers" onDatasetSubmit={vi.fn()} />, { wrapper: createWrapper() });

    await expect(screen.findByTestId("contracts-graph")).resolves.toBeVisible();
    expect(screen.getByTestId("contract-legend")).toBeVisible();
    expect(screen.getByTestId("contract-node:dataset:lake/customers")).toHaveTextContent("lake/customers");
    expect(screen.getByTestId("contract-node:job:producer")).toHaveAttribute("href", "/jobs/producer-id");

    const edge = screen.getByTestId("contract-edge:declared:declared:producer:consumer:lake/customers");
    expect(edge).toHaveAttribute("data-edge-class", "declared");
    expect(edge).toHaveAttribute("data-edge-verdict", "breaking");
    expect(edge).toHaveAttribute("data-stroke", "hsl(var(--danger))");
    expect(edge).toHaveAttribute("data-dasharray", "");

    await waitFor(() => {
      expect(api.getContractGraph).toHaveBeenCalledWith({ dataset: "lake/customers" });
    });
  });
});
