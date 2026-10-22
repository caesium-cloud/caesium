import { describe, expect, it, vi } from "vitest";
import { buildLineageImpactGraph, externalImpactLabel } from "../LineageGraph";
import type { ImpactDownstreamNode } from "@/lib/api";

vi.mock("reactflow", () => ({
  __esModule: true,
  default: () => null,
  Background: () => null,
  BaseEdge: () => null,
  Controls: () => null,
  EdgeLabelRenderer: ({ children }: { children: React.ReactNode }) => <>{children}</>,
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

describe("buildLineageImpactGraph with external nodes (identity-and-access B5)", () => {
  const downstream: ImpactDownstreamNode[] = [
    { id: "external:abc", kind: "external", depth: 0 },
    {
      dataset_namespace: "ol",
      dataset_name: "mkt-out",
      direction: "output",
      job_id: "job-m2",
      job_alias: "m2",
      last_seen: "2026-10-21T00:00:00Z",
      depth: 1,
    },
  ];

  it("renders a collapsed node as a neutral placeholder that keeps its hop", () => {
    const graph = buildLineageImpactGraph("ol", "root", downstream, new Map(), new Map(), new Set());
    const external = graph.nodes.filter((node) => node.type === "lineageExternal");
    expect(external).toHaveLength(1);
    expect(external[0].data).toMatchObject({ kind: "external", name: externalImpactLabel, depth: 0 });
    expect(external[0].data.jobId).toBeUndefined();
    expect(JSON.stringify(graph.nodes)).not.toContain("undefined");

    const visible = graph.nodes.find((node) => node.data.name === "mkt-out");
    expect(visible).toBeDefined();
    // The visible node past the placeholder hangs off it, by hop.
    const into = graph.edges.filter((edge) => edge.target === visible!.id);
    expect(into.map((edge) => edge.source)).toEqual([external[0].id]);
  });
});
