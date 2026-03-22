import { fireEvent, render, screen } from "@testing-library/react";
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
  beforeEach(() => {
    window.localStorage.clear();
    Object.defineProperty(window, "innerWidth", {
      configurable: true,
      value: 1200,
      writable: true,
    });
  });

  it("resizes from the drag handle and clamps to viewport bounds", () => {
    render(
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
});
