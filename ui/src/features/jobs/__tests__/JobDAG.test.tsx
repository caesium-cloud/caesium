import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { JobDAG } from "../JobDAG";
import type { Atom, JobDAGResponse, TaskRun } from "@/lib/api";

vi.mock("reactflow", () => {
  const ReactFlow = ({
    nodes = [],
    edges = [],
    onNodeClick,
  }: {
    nodes?: Array<Record<string, unknown>>;
    edges?: Array<Record<string, unknown>>;
    onNodeClick?: (event: React.MouseEvent, node: Record<string, unknown>) => void;
  }) => (
    <div data-testid="reactflow-mock">
      {nodes.map((node) => {
        const id = String(node.id);
        const data = (node.data ?? {}) as Record<string, unknown>;
        return (
          <button
            key={id}
            type="button"
            data-testid={`node-${id}`}
            data-node-type={String(node.type)}
            data-status={String(data.status ?? "")}
            data-selected={String(Boolean(data.isSelected))}
            onClick={(event) => onNodeClick?.(event, node)}
          >
            {String(data.label ?? id)}
          </button>
        );
      })}
      {edges.map((edge) => {
        const source = String(edge.source);
        const target = String(edge.target);
        const data = (edge.data ?? {}) as Record<string, unknown>;
        const style = (edge.style ?? {}) as Record<string, unknown>;
        return (
          <button
            key={String(edge.id)}
            type="button"
            data-testid={`edge-${source}-${target}`}
            data-output-count={String(data.outputCount ?? "")}
            data-contract-defined={String(Boolean(data.contractDefined))}
            data-animated={String(Boolean(edge.animated))}
            data-stroke={String(style.stroke ?? "")}
            data-dasharray={String(style.strokeDasharray ?? "")}
            onClick={() => (data.onOpenDetails as (() => void) | undefined)?.()}
          />
        );
      })}
    </div>
  );

  return {
    __esModule: true,
    default: ReactFlow,
    Background: () => null,
    Controls: () => null,
    MarkerType: { ArrowClosed: "arrow-closed" },
    Position: { Left: "left", Right: "right", Top: "top", Bottom: "bottom" },
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

const atoms: Record<string, Atom> = {
  "atom-1": {
    id: "atom-1",
    engine: "docker",
    image: "busybox:1.36",
    command: "echo one",
    spec: {},
    created_at: "2026-03-24T00:00:00Z",
    updated_at: "2026-03-24T00:00:00Z",
  },
  "atom-2": {
    id: "atom-2",
    engine: "docker",
    image: "busybox:1.36",
    command: "echo two",
    spec: {},
    created_at: "2026-03-24T00:00:00Z",
    updated_at: "2026-03-24T00:00:00Z",
  },
};

describe("JobDAG", () => {
  it("maps branch nodes and normalizes completed task status", () => {
    const dag: JobDAGResponse = {
      job_id: "job-1",
      nodes: [
        { id: "task-1", atom_id: "atom-1", output_schema: { type: "object" } },
        { id: "branch-1", atom_id: "atom-2", type: "branch" },
      ],
      edges: [],
    };

    render(
      <JobDAG
        dag={dag}
        atoms={atoms}
        taskStatus={{ "task-1": "completed", "branch-1": "running" }}
        selectedTaskId="task-1"
      />,
    );

    expect(screen.getByTestId("node-task-1")).toHaveAttribute("data-node-type", "task");
    expect(screen.getByTestId("node-task-1")).toHaveAttribute("data-status", "succeeded");
    expect(screen.getByTestId("node-task-1")).toHaveAttribute("data-selected", "true");
    expect(screen.getByTestId("node-branch-1")).toHaveAttribute("data-node-type", "branch");
    expect(screen.getByTestId("node-branch-1")).toHaveAttribute("data-status", "running");
  });

  it("forwards node clicks to the consumer", () => {
    const onNodeClick = vi.fn();
    const dag: JobDAGResponse = {
      job_id: "job-1",
      nodes: [{ id: "task-1", atom_id: "atom-1" }],
      edges: [],
    };

    render(<JobDAG dag={dag} atoms={atoms} onNodeClick={onNodeClick} />);

    fireEvent.click(screen.getByTestId("node-task-1"));
    expect(onNodeClick).toHaveBeenCalledWith("task-1");
  });

  it("marks output-bearing contract edges and skipped branch paths", () => {
    const dag: JobDAGResponse = {
      job_id: "job-1",
      nodes: [
        { id: "task-1", atom_id: "atom-1" },
        { id: "task-2", atom_id: "atom-2" },
      ],
      edges: [{ from: "task-1", to: "task-2", contract_defined: true }],
    };

    const taskRunData: Record<string, TaskRun> = {
      "task-1": {
        id: "run-task-1",
        job_run_id: "run-1",
        task_id: "task-1",
        atom_id: "atom-1",
        engine: "docker",
        image: "busybox:1.36",
        command: ["echo", "one"],
        status: "succeeded",
        output: { rows: "10", status: "ok" },
        created_at: "2026-03-24T00:00:00Z",
        updated_at: "2026-03-24T00:00:00Z",
      },
    };

    render(
      <JobDAG
        dag={dag}
        atoms={atoms}
        taskMetadata={{
          "task-1": { status: "succeeded" },
          "task-2": { status: "skipped" },
        }}
        taskRunData={taskRunData}
      />,
    );

    expect(screen.getByTestId("edge-task-1-task-2")).toHaveAttribute("data-output-count", "2");
    expect(screen.getByTestId("edge-task-1-task-2")).toHaveAttribute("data-contract-defined", "true");
    expect(screen.getByTestId("edge-task-1-task-2")).toHaveAttribute("data-stroke", "#64748b");
    expect(screen.getByTestId("edge-task-1-task-2")).toHaveAttribute("data-dasharray", "6 3");
    expect(screen.getByTestId("edge-task-1-task-2")).toHaveAttribute("data-animated", "false");
  });

  it("opens edge details with run outputs and contract information", () => {
    const dag: JobDAGResponse = {
      job_id: "job-1",
      nodes: [
        { id: "task-1", atom_id: "atom-1", output_schema: { type: "object", properties: { rows: { type: "integer" } } } },
        { id: "task-2", atom_id: "atom-2" },
      ],
      edges: [{ from: "task-1", to: "task-2", contract_defined: true }],
    };

    const taskRunData: Record<string, TaskRun> = {
      "task-1": {
        id: "run-task-1",
        job_run_id: "run-1",
        task_id: "task-1",
        atom_id: "atom-1",
        engine: "docker",
        image: "busybox:1.36",
        command: ["echo", "one"],
        status: "succeeded",
        output: { rows: "10" },
        created_at: "2026-03-24T00:00:00Z",
        updated_at: "2026-03-24T00:00:00Z",
      },
    };

    render(
      <JobDAG
        dag={dag}
        atoms={atoms}
        taskRunData={taskRunData}
        taskDefinitions={{
          "task-1": {
            id: "task-1",
            job_id: "job-1",
            atom_id: "atom-1",
            name: "extract",
            node_selector: {},
            retries: 0,
            retry_delay: 0,
            retry_backoff: false,
            trigger_rule: "all_success",
            output_schema: { type: "object", properties: { rows: { type: "integer" } } },
            created_at: "",
            updated_at: "",
          },
          "task-2": {
            id: "task-2",
            job_id: "job-1",
            atom_id: "atom-2",
            name: "transform",
            node_selector: {},
            retries: 0,
            retry_delay: 0,
            retry_backoff: false,
            trigger_rule: "all_success",
            input_schema: { extract: { type: "object", properties: { rows: { type: "integer" } }, required: ["rows"] } },
            created_at: "",
            updated_at: "",
          },
        }}
      />,
    );

    fireEvent.click(screen.getByTestId("edge-task-1-task-2"));

    expect(screen.getByText("extract → transform")).toBeInTheDocument();
    expect(screen.getByText("Observed outputs for this run")).toBeInTheDocument();
    expect(screen.getByText("rows:")).toBeInTheDocument();
    expect(screen.getByText("10")).toBeInTheDocument();
    expect(screen.getByText("Consumer requirements declared")).toBeInTheDocument();
  });
});
