import { render } from "@testing-library/react";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";
import { buildLineageImpactGraph, lineageEdgeTypes, lineageNodeTypes } from "../LineageGraph";
import type { ImpactDownstreamNode } from "@/lib/api";

// The registered REAL node and edge renderers are mounted, so link targets,
// title attributes, tooltips and edge endpoint labels are the ones a browser
// would see. Only reactflow's measurement and layout are replaced.
vi.mock("reactflow", () => ({
  __esModule: true,
  default: () => null,
  Background: () => null,
  BaseEdge: () => null,
  Controls: () => null,
  EdgeLabelRenderer: ({ children }: { children: ReactNode }) => <>{children}</>,
  Handle: () => null,
  MarkerType: { ArrowClosed: "arrow-closed" },
  Position: { Left: "left", Right: "right", Top: "top", Bottom: "bottom" },
  getSmoothStepPath: () => ["M0,0L1,1", 0, 0],
}));
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
vi.mock("@tanstack/react-router", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@tanstack/react-router")>()),
  getRouteApi: () => ({ useSearch: () => ({}) }),
  useNavigate: () => vi.fn(),
  Link: ({ children, ...props }: { children: ReactNode }) => <a {...props}>{children}</a>,
}));

type Renderer = (props: Record<string, unknown>) => ReactNode;

describe("lineage external placeholders leak nothing (identity-and-access F2)", () => {
  it("renders the collapsed node and its edges without any foreign alias, dataset or link", () => {
    // A well-behaved server sends only {id, kind, depth}; the renderer must not
    // depend on that, so identifying extras are fed in and must not surface.
    const downstream = [
      {
        id: "external:abc",
        kind: "external",
        depth: 0,
        job_alias: "finance-secret-alias",
        job_id: "finance-job-id",
        dataset_name: "secret.dataset",
        dataset_namespace: "finance-ns",
      },
      {
        dataset_namespace: "ol",
        dataset_name: "mkt-out",
        direction: "output",
        job_id: "job-m2",
        job_alias: "m2",
        last_seen: "2026-10-21T00:00:00Z",
        depth: 1,
      },
    ] as unknown as ImpactDownstreamNode[];
    const graph = buildLineageImpactGraph("ol", "root", downstream, new Map(), new Map(), new Set());

    const { container } = render(
      <div>
        {graph.nodes.map((node) => {
          const Node = (lineageNodeTypes as unknown as Record<string, Renderer>)[String(node.type)];
          return <Node key={node.id} id={node.id} data={node.data} />;
        })}
        {graph.edges.map((edge) => {
          const Edge = (lineageEdgeTypes as unknown as Record<string, Renderer>)[String(edge.type)];
          return <Edge key={edge.id} id={edge.id} data={edge.data} style={edge.style} sourceX={0} sourceY={0} targetX={1} targetY={1} />;
        })}
      </div>,
    );

    const external = container.querySelector('[data-testid="lineage-external-node"]');
    expect(external).not.toBeNull();
    expect(external!.textContent).toContain("Another namespace");
    expect(external!.querySelector("a")).toBeNull();

    const html = container.innerHTML;
    for (const secret of ["finance-secret-alias", "finance-job-id", "secret.dataset", "finance-ns"]) {
      expect(html, `leaked ${secret}`).not.toContain(secret);
    }
    // The visible downstream node is unaffected.
    expect(html).toContain("mkt-out");
    // Edge endpoint labels around the placeholder say so.
    const labels = Array.from(container.querySelectorAll("[data-edge-source]")).flatMap((edge) => [
      edge.getAttribute("data-edge-source"),
      edge.getAttribute("data-edge-target"),
    ]);
    expect(labels).toContain("Another namespace");
  });
});
