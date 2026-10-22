import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ContractGraph } from "../ContractGraphPage";
import { api } from "@/lib/api";

// Unlike ContractGraphPage.test.tsx, the host stub renders the REAL node AND
// edge components the page registers (nodeTypes / edgeTypes), so link targets,
// title attributes, tooltips and edge endpoint labels are the ones a browser
// would see. Only reactflow's own layout/measurement is replaced.
vi.mock("@tanstack/react-router", () => ({
  Link: ({ to, params, children, ...props }: { to: string; params?: Record<string, string>; children: ReactNode; [key: string]: unknown }) => (
    <a href={to === "/jobs/$jobId" && params?.jobId ? `/jobs/${params.jobId}` : to} {...props}>
      {children}
    </a>
  ),
  useNavigate: () => vi.fn(),
  useSearch: () => ({}),
}));

vi.mock("reactflow", () => {
  type Component = (props: Record<string, unknown>) => ReactNode;
  const ReactFlow = ({
    nodes = [],
    edges = [],
    nodeTypes = {},
    edgeTypes = {},
  }: {
    nodes?: Array<Record<string, unknown>>;
    edges?: Array<Record<string, unknown>>;
    nodeTypes?: Record<string, Component>;
    edgeTypes?: Record<string, Component>;
  }) => (
    <div data-testid="reactflow-real-renderers">
      {nodes.map((node) => {
        const NodeComponent = nodeTypes[String(node.type)];
        return NodeComponent ? <NodeComponent key={String(node.id)} id={node.id} data={node.data} /> : null;
      })}
      {edges.map((edge) => {
        const EdgeComponent = edgeTypes[String(edge.type)];
        return EdgeComponent ? (
          <EdgeComponent
            key={String(edge.id)}
            id={edge.id}
            data={edge.data}
            style={edge.style}
            sourceX={0}
            sourceY={0}
            targetX={1}
            targetY={1}
          />
        ) : null;
      })}
    </div>
  );
  return {
    __esModule: true,
    default: ReactFlow,
    Background: () => null,
    BaseEdge: () => null,
    Controls: () => null,
    EdgeLabelRenderer: ({ children }: { children: ReactNode }) => <>{children}</>,
    Handle: () => null,
    MarkerType: { ArrowClosed: "arrow-closed" },
    Position: { Left: "left", Right: "right", Top: "top", Bottom: "bottom" },
    getSmoothStepPath: () => ["M0,0L1,1", 0, 0],
  };
});

vi.mock("dagre", () => {
  class Graph {
    setDefaultEdgeLabel() {}
    setGraph() {}
    setNode() {}
    setEdge() {}
    node() {
      return { x: 0, y: 0 };
    }
  }
  return { __esModule: true, default: { graphlib: { Graph }, layout: () => undefined } };
});

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, api: { getSystemFeatures: vi.fn(), getContractGraph: vi.fn(), getJobs: vi.fn() } };
});

function wrapper({ children }: { children: ReactNode }) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

describe("contract graph external placeholders leak nothing", () => {
  beforeEach(() => {
    vi.mocked(api.getSystemFeatures).mockResolvedValue({ contract_enforcement_enabled: true } as never);
    vi.mocked(api.getJobs).mockResolvedValue([
      { id: "producer-id", alias: "producer", trigger_id: "", labels: {}, annotations: {}, paused: false, created_at: "", updated_at: "" },
    ]);
  });

  it("renders no foreign alias, dataset, link or tooltip even if a node carries them", async () => {
    // A well-behaved server sends only {id, kind}. The renderer must not depend
    // on that: extra identifying fields on an external node, and an edge whose
    // endpoint is missing from the node list, still show only the placeholder.
    vi.mocked(api.getContractGraph).mockResolvedValue({
      nodes: [
        { id: "job:producer", kind: "job", alias: "producer" },
        {
          id: "external:0f1e2d",
          kind: "external",
          alias: "finance-secret-alias",
          labels: { team: "finance-secret-team" },
          dataset: { namespace: "finance-ns", name: "secret.dataset" },
        },
      ],
      edges: [
        { id: "external:9a8b7c", from: "job:producer", to: "external:0f1e2d" },
        { id: "external:aaaaaa", from: "job:finance-hidden-job", to: "job:producer" },
      ],
    } as never);

    const { container } = render(<ContractGraph initialDataset="" onDatasetSubmit={vi.fn()} />, { wrapper });
    await screen.findByTestId("reactflow-real-renderers");

    const external = screen.getByTestId("contract-node:external:0f1e2d");
    expect(external).toHaveTextContent("Another namespace");
    expect(external).not.toHaveAttribute("href");
    expect(external.querySelector("a")).toBeNull();

    // The edge into the external node and the edge from an unknown endpoint.
    const edges = container.querySelectorAll("[data-edge-source]");
    expect(edges).toHaveLength(2);
    expect(edges[0].getAttribute("data-edge-target")).toBe("Another namespace");


    const everything = container.innerHTML;
    for (const secret of ["finance-secret-alias", "finance-secret-team", "secret.dataset", "finance-ns", "finance-hidden-job"]) {
      expect(everything, `leaked ${secret}`).not.toContain(secret);
    }
    // An endpoint absent from the node list is a placeholder, never its raw id.
    expect(edges[1].getAttribute("data-edge-source")).toBe("Another namespace");
    // Only the visible producer job is a link.
    expect(Array.from(container.querySelectorAll("a")).map((a) => a.getAttribute("href"))).toEqual(["/jobs/producer-id"]);
  });
});
