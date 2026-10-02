import { render, screen, within } from "@testing-library/react";
import { act } from "@testing-library/react";
import { Profiler } from "react";
import { describe, expect, it, vi } from "vitest";
import { UTCClock, UTCClockProvider } from "@/components/ui/utc-clock";
import type { JobTask, TaskRun } from "@/lib/api";
import { RunTimeline } from "../RunTimeline";

function makeTask(overrides: Partial<TaskRun> & { task_id: string }): TaskRun {
  return {
    id: overrides.id ?? `run-${overrides.task_id}`,
    job_run_id: "run-1",
    atom_id: "atom-1",
    engine: "docker",
    image: "alpine:3.23",
    command: ["echo", "hi"],
    status: "succeeded",
    started_at: "2026-08-01T00:00:00.000Z",
    completed_at: "2026-08-01T00:00:01.000Z",
    created_at: "2026-08-01T00:00:00.000Z",
    updated_at: "2026-08-01T00:00:01.000Z",
    ...overrides,
  };
}

const taskDefinitions: Record<string, JobTask> = {
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
    created_at: "",
    updated_at: "",
  },
  "task-2": {
    id: "task-2",
    job_id: "job-1",
    atom_id: "atom-1",
    name: "process-file",
    node_selector: {},
    retries: 0,
    retry_delay: 0,
    retry_backoff: false,
    trigger_rule: "all_success",
    created_at: "",
    updated_at: "",
  },
};

describe("RunTimeline", () => {
  it("keeps terminal unstarted tasks anchored to recorded times and does not tick", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-08-02T00:00:00Z"));
    const tasks = [makeTask({ task_id: "task-1", status: "failed", started_at: undefined, error: "image pull failed" }),
      makeTask({ task_id: "task-2", status: "queued", started_at: undefined, completed_at: undefined })];
    let renders = 0;
    const { unmount } = render(<UTCClockProvider><UTCClock /><Profiler id="timeline" onRender={() => renders++}>
      <RunTimeline tasks={tasks} taskDefinitions={taskDefinitions} runStartedAt="2026-08-01T00:00:00Z" runStatus="failed" />
    </Profiler></UTCClockProvider>);
    expect(screen.getByText("image pull failed")).toBeInTheDocument();
    expect(screen.getByText("Did not start")).toBeInTheDocument();
    expect(screen.queryByText("Waiting on upstream")).not.toBeInTheDocument();
    const bars = screen.getAllByTestId("run-timeline-bar");
    bars.forEach(bar => expect(bar).toHaveAttribute("data-ghost", "false"));
    const positions = bars.map(bar => bar.style.left);
    const before = renders;
    act(() => { vi.advanceTimersByTime(5000); });
    expect(renders).toBe(before);
    expect(bars.map(bar => bar.style.left)).toEqual(positions);
    unmount();
    vi.useRealTimers();
  });

  it("renders a plain row for an unfanned task and keeps the run-timeline-task-row testid", () => {
    const tasks = [makeTask({ task_id: "task-1" })];

    render(
      <RunTimeline tasks={tasks} taskDefinitions={taskDefinitions} runStartedAt="2026-08-01T00:00:00.000Z" />,
    );

    const rows = screen.getAllByTestId("run-timeline-task-row");
    expect(rows).toHaveLength(1);
    expect(within(rows[0]).queryByTestId("run-timeline-group-row")).not.toBeInTheDocument();
    expect(within(rows[0]).queryByTestId("run-timeline-density-strip")).not.toBeInTheDocument();
    expect(rows[0]).toHaveTextContent("extract");
    expect(rows[0]).not.toHaveTextContent("×");
  });

  it("renders a group lane with a density strip and ×N label for a fanned task", () => {
    const tasks = [
      makeTask({
        task_id: "task-2",
        partition_count: 3,
        partition_status_counts: { succeeded: 2, running: 1 },
      }),
    ];

    render(
      <RunTimeline tasks={tasks} taskDefinitions={taskDefinitions} runStartedAt="2026-08-01T00:00:00.000Z" />,
    );

    const row = screen.getByTestId("run-timeline-task-row");
    expect(row).toHaveAttribute("data-partition-count", "3");

    const groupRow = within(row).getByTestId("run-timeline-group-row");
    expect(groupRow).toHaveTextContent("×3");

    const strip = within(groupRow).getByTestId("run-timeline-density-strip");
    const segments = within(strip).getAllByTestId("run-timeline-density-segment");
    expect(segments.map((segment) => segment.getAttribute("data-status"))).toEqual([
      "succeeded",
      "running",
    ]);
  });

  it("falls back to a plain row when a fanned task has no status breakdown yet", () => {
    const tasks = [makeTask({ task_id: "task-2", partition_count: 3 })];

    render(
      <RunTimeline tasks={tasks} taskDefinitions={taskDefinitions} runStartedAt="2026-08-01T00:00:00.000Z" />,
    );

    const row = screen.getByTestId("run-timeline-task-row");
    const groupRow = within(row).getByTestId("run-timeline-group-row");
    expect(groupRow).toHaveTextContent("×3");
    expect(within(groupRow).queryByTestId("run-timeline-density-strip")).not.toBeInTheDocument();
  });

  it("renders a group lane for a single-partition group carrying partition identity", () => {
    // Regression: an expansion that materializes exactly one instance still has
    // a partition value and its own cache identity — gating on
    // `partition_count > 1` alone silently rendered it as a plain row, so the
    // same step's lane changed shape run to run as N crossed 1.
    const tasks = [
      makeTask({ task_id: "task-2", partition_count: 1, partition_value: "alpha" }),
    ];

    render(
      <RunTimeline tasks={tasks} taskDefinitions={taskDefinitions} runStartedAt="2026-08-01T00:00:00.000Z" />,
    );

    const row = screen.getByTestId("run-timeline-task-row");
    const groupRow = within(row).getByTestId("run-timeline-group-row");
    expect(groupRow).toHaveTextContent("×1");
  });
});

it("scales a 492ms terminal run to 500ms without distorting duration or clipping endpoints", () => {
  render(<RunTimeline tasks={[makeTask({ task_id: "task-1", started_at: "2026-08-01T00:00:00.000Z", completed_at: "2026-08-01T00:00:00.492Z", status: "succeeded" })]} taskDefinitions={taskDefinitions} runStartedAt="2026-08-01T00:00:00.000Z" runStatus="succeeded" />);
  const ticks = screen.getAllByTestId("timeline-tick");
  expect(ticks.map(tick => tick.textContent)).toEqual(["0ms", "100ms", "200ms", "300ms", "400ms", "500ms"]);
  expect(ticks[0].style.transform).toBe("translateX(0%)");
  expect(ticks.at(-1)!.style.transform).toBe("translateX(-100%)");
  expect(parseFloat(screen.getByTestId("run-timeline-bar").style.width)).toBeCloseTo(98.4);
});

it("shows one shared now cursor only while the run is live", () => {
  const props = { tasks: [makeTask({ task_id: "task-1", status: "running", completed_at: undefined })], taskDefinitions, runStartedAt: "2026-08-01T00:00:00Z" };
  const { rerender } = render(<RunTimeline {...props} runStatus="running" />);
  expect(screen.getAllByTestId("run-timeline-now")).toHaveLength(1);
  expect(screen.getByTestId("run-timeline-bar")).toHaveClass("cs-live-bar");
  rerender(<RunTimeline {...props} tasks={[makeTask({ task_id: "task-1" })]} runStatus="succeeded" />);
  expect(screen.queryByTestId("run-timeline-now")).not.toBeInTheDocument();
  expect(screen.getByTestId("run-timeline-bar")).not.toHaveClass("cs-live-bar");
});
