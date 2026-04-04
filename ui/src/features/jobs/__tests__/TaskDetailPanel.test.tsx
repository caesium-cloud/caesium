import { fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactElement } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { TaskDetailPanel } from "../TaskDetailPanel";

vi.mock("../LogViewer", () => ({
  LogViewer: ({ sizeVersion }: { sizeVersion?: number }) => (
    <div data-testid="log-viewer" data-size-version={sizeVersion ?? ""}>
      Log viewer
    </div>
  ),
}));

describe("TaskDetailPanel", () => {
  function renderPanel(component: ReactElement) {
    const queryClient = new QueryClient();
    return render(
      <QueryClientProvider client={queryClient}>
        {component}
      </QueryClientProvider>,
    );
  }

  beforeEach(() => {
    window.localStorage.clear();
    Object.defineProperty(window, "innerWidth", {
      configurable: true,
      value: 1200,
      writable: true,
    });
  });

  it("resizes from the drag handle and clamps to viewport bounds", () => {
    renderPanel(
      <TaskDetailPanel
        taskId="task-1"
        jobId="job-1"
        runId="run-1"
        onClose={() => {}}
      />,
    );

    const panel = screen.getByTestId("task-detail-panel");
    const handle = screen.getByTestId("task-detail-panel-resize-handle");

    expect(panel).toHaveStyle({ width: "520px" });
    expect(screen.getByTestId("log-viewer")).toHaveAttribute("data-size-version", "520");

    fireEvent.pointerDown(handle, { clientX: 680 });
    fireEvent.pointerMove(window, { clientX: 400 });

    expect(panel).toHaveStyle({ width: "800px" });
    expect(screen.getByTestId("log-viewer")).toHaveAttribute("data-size-version", "800");

    fireEvent.pointerMove(window, { clientX: 20 });
    expect(panel).toHaveStyle({ width: "960px" });

    fireEvent.pointerUp(window);
    fireEvent.pointerMove(window, { clientX: 500 });
    expect(panel).toHaveStyle({ width: "960px" });
  });

  it("shows cache provenance when the task was served from cache", () => {
    renderPanel(
      <TaskDetailPanel
        taskId="task-cache"
        jobId="job-1"
        runId="run-1"
        task={{ id: "task-cache", job_id: "job-1", atom_id: "atom-1", name: "extract", node_selector: {}, retries: 0, retry_delay: 0, retry_backoff: false, trigger_rule: "all_success", created_at: "", updated_at: "" }}
        runTask={{
          id: "task-cache",
          job_run_id: "run-1",
          task_id: "task-cache",
          atom_id: "atom-1",
          engine: "docker",
          image: "alpine:3.23",
          command: ["echo", "cache"],
          status: "cached",
          cache_hit: true,
          cache_created_at: "2026-03-31T10:00:00Z",
          created_at: "2026-03-31T10:00:00Z",
          updated_at: "2026-03-31T10:00:00Z",
        }}
        onClose={() => {}}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: /details/i }));
    expect(screen.getByText("Reused successful output")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Invalidate" })).toBeInTheDocument();
  });
});
