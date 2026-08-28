import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { JobTask, TaskRun } from "@/lib/api";
import { RunProposalSummary } from "../RunProposalSummary";

function makeTask(overrides: Partial<TaskRun> & { task_id: string }): TaskRun {
  return {
    id: overrides.task_id,
    job_run_id: "run-1",
    atom_id: "atom-1",
    engine: "docker",
    image: "alpine:3.23",
    command: [],
    status: "succeeded",
    created_at: "2026-08-27T00:00:00Z",
    updated_at: "2026-08-27T00:00:00Z",
    ...overrides,
  };
}

function makeTaskDef(id: string, name: string): JobTask {
  return {
    id,
    job_id: "job-1",
    atom_id: "atom-1",
    name,
    node_selector: {},
    retries: 0,
    retry_delay: 0,
    retry_backoff: false,
    trigger_rule: "all_success",
    created_at: "2026-08-27T00:00:00Z",
    updated_at: "2026-08-27T00:00:00Z",
  };
}

describe("RunProposalSummary", () => {
  it("renders nothing when no task in the run has a proposal", () => {
    const tasks = [
      makeTask({ task_id: "t1", output: { some_output: "value" } }),
      makeTask({ task_id: "t2" }),
    ];
    const { container } = render(<RunProposalSummary tasks={tasks} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing when there are no tasks at all", () => {
    const { container } = render(<RunProposalSummary tasks={[]} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("aggregates counts across mixed kinds and still lists a generic-fallback row", () => {
    const tasks = [
      makeTask({
        task_id: "t-tf",
        output: {
          proposal_kind: "terraform.plan.v1",
          proposal_summary: JSON.stringify({ add: 2, change: 1, destroy: 0 }),
        },
      }),
      makeTask({
        task_id: "t-tf-2",
        output: {
          proposal_kind: "terraform.plan.v1",
          proposal_summary: JSON.stringify({ add: 3, change: 0, destroy: 1 }),
        },
      }),
      // Unregistered kind: falls back to the generic renderer in
      // proposal-renderers.ts, which has no counts at all.
      makeTask({
        task_id: "t-dbt",
        output: {
          proposal_kind: "dbt.compile.v1",
          proposal_summary: JSON.stringify({ models_changed: 4 }),
        },
      }),
    ];
    const taskDefinitions: Record<string, JobTask> = {
      "t-tf": makeTaskDef("t-tf", "terraform-plan"),
      "t-tf-2": makeTaskDef("t-tf-2", "terraform-plan-2"),
      "t-dbt": makeTaskDef("t-dbt", "dbt-compile"),
    };

    render(<RunProposalSummary tasks={tasks} taskDefinitions={taskDefinitions} />);

    // Aggregate totals: summed only across the structured (terraform) rows.
    const totals = screen.getAllByTestId("run-proposal-summary-total");
    expect(totals).toHaveLength(3);
    expect(totals.find((el) => el.dataset.action === "add")).toHaveTextContent("5");
    expect(totals.find((el) => el.dataset.action === "change")).toHaveTextContent("1");
    expect(totals.find((el) => el.dataset.action === "destroy")).toHaveTextContent("1");

    // Every task with a proposal gets a row, including the unregistered kind.
    const rows = screen.getAllByTestId("run-proposal-summary-row");
    expect(rows).toHaveLength(3);

    const dbtRow = rows.find((row) => row.dataset.taskId === "t-dbt");
    expect(dbtRow).toBeDefined();
    expect(dbtRow).toHaveAttribute("data-kind", "dbt.compile.v1");
    expect(dbtRow).toHaveTextContent("dbt-compile");
    // Generic-kind row has no per-action counts — blank marker, no crash.
    expect(dbtRow?.querySelector('[data-testid="run-proposal-summary-row-counts-blank"]')).not.toBeNull();
    expect(dbtRow?.querySelector('[data-testid="run-proposal-summary-row-count"]')).toBeNull();

    const tfRow = rows.find((row) => row.dataset.taskId === "t-tf");
    expect(tfRow).toHaveTextContent("terraform-plan");
    const tfCounts = tfRow?.querySelectorAll('[data-testid="run-proposal-summary-row-count"]');
    expect(tfCounts).toHaveLength(3);
  });

  it("still renders a row (with blank counts, no crash) when proposal_summary is malformed", () => {
    const tasks = [
      makeTask({
        task_id: "t-broken",
        output: {
          proposal_kind: "terraform.plan.v1",
          proposal_summary: "{not valid json",
        },
      }),
    ];

    render(<RunProposalSummary tasks={tasks} />);

    const rows = screen.getAllByTestId("run-proposal-summary-row");
    expect(rows).toHaveLength(1);
    expect(rows[0]).toHaveAttribute("data-kind", "terraform.plan.v1");
    // No structured counts could be recovered from malformed JSON.
    expect(screen.queryByTestId("run-proposal-summary-row-count")).not.toBeInTheDocument();
    expect(screen.getByTestId("run-proposal-summary-row-counts-blank")).toBeInTheDocument();
    // Malformed input never produces aggregate totals either.
    expect(screen.queryByTestId("run-proposal-summary-total")).not.toBeInTheDocument();
  });

  it("falls back to the raw task_id as the row label when no task definition is available", () => {
    const tasks = [
      makeTask({
        task_id: "t-unknown",
        output: { proposal_kind: "terraform.plan.v1", proposal_summary: JSON.stringify({ add: 1 }) },
      }),
    ];

    render(<RunProposalSummary tasks={tasks} />);

    expect(screen.getByTestId("run-proposal-summary-row")).toHaveTextContent("t-unknown");
  });

  it("calls onSelectTask with the task_id when a row is clicked", () => {
    const onSelectTask = vi.fn();
    const tasks = [
      makeTask({
        task_id: "t-click",
        output: { proposal_kind: "terraform.plan.v1", proposal_summary: JSON.stringify({ add: 1 }) },
      }),
    ];

    render(<RunProposalSummary tasks={tasks} onSelectTask={onSelectTask} />);

    fireEvent.click(screen.getByTestId("run-proposal-summary-row"));
    expect(onSelectTask).toHaveBeenCalledWith("t-click");
  });
});
