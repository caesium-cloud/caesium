import { describe, expect, it, vi } from "vitest";
import { buildLineageImpactGraph, impactNodeTarget } from "../LineageGraph";
import type { ImpactDownstreamNode } from "@/lib/api";

vi.mock("reactflow", () => ({
  __esModule: true,
  default: () => null,
  Background: () => null,
  BaseEdge: () => null,
  Controls: () => null,
  EdgeLabelRenderer: () => null,
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

describe("lineage impact run click-through", () => {
  const downstream: ImpactDownstreamNode[] = [
    {
      dataset_namespace: "ol",
      dataset_name: "with-run",
      direction: "output",
      job_id: "job-1",
      job_alias: "a",
      run_id: "run-9",
      last_seen: "2026-10-21T00:00:00Z",
      depth: 0,
    },
    {
      dataset_namespace: "ol",
      dataset_name: "without-run",
      direction: "output",
      job_id: "job-2",
      job_alias: "b",
      last_seen: "2026-10-21T00:00:00Z",
      depth: 1,
    },
  ];

  it("carries run_id onto the node and targets the run when present", () => {
    const graph = buildLineageImpactGraph("ol", "root", downstream, new Map(), new Map(), new Set());
    const withRun = graph.nodes.find((node) => node.data.name === "with-run")!;
    expect(withRun.data.runId).toBe("run-9");
    expect(impactNodeTarget(withRun.data)).toEqual({ jobId: "job-1", runId: "run-9" });
  });

  it("falls back to the job when the server named no run", () => {
    const graph = buildLineageImpactGraph("ol", "root", downstream, new Map(), new Map(), new Set());
    const withoutRun = graph.nodes.find((node) => node.data.name === "without-run")!;
    expect(withoutRun.data.runId).toBeUndefined();
    expect(impactNodeTarget(withoutRun.data)).toEqual({ jobId: "job-2" });
  });

  it("has no target for a node that names no job", () => {
    expect(impactNodeTarget({})).toBeUndefined();
  });
});
