import { fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactElement } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { TaskDetailPanel } from "../TaskDetailPanel";

const { mockFetch, terminalWrite, terminalReset, fitAddonFit } = vi.hoisted(() => ({
  mockFetch: vi.fn(),
  terminalWrite: vi.fn((_chunk: string, callback?: () => void) => callback?.()),
  terminalReset: vi.fn(),
  fitAddonFit: vi.fn(),
}));

vi.mock("@/lib/auth", () => ({
  withAuthHeaders: () => ({}),
}));

vi.mock("xterm", () => ({
  Terminal: vi.fn().mockImplementation(() => ({
    loadAddon: vi.fn(),
    open: vi.fn(),
    reset: terminalReset,
    write: terminalWrite,
    dispose: vi.fn(),
  })),
}));

vi.mock("xterm-addon-fit", () => ({
  FitAddon: vi.fn().mockImplementation(() => ({
    fit: fitAddonFit,
  })),
}));

describe("TaskDetailPanel", () => {
  function renderPanel(component: ReactElement) {
    const queryClient = new QueryClient();
    return render(
      <QueryClientProvider client={queryClient}>
        <div data-testid="dag-canvas-host" className="relative">
          {component}
        </div>
      </QueryClientProvider>,
    );
  }

  beforeEach(() => {
    mockFetch.mockReset();
    terminalWrite.mockClear();
    terminalReset.mockClear();
    fitAddonFit.mockClear();
    globalThis.fetch = mockFetch as unknown as typeof fetch;
    mockFetch.mockImplementation(() => Promise.resolve(noContentLogResponse()));
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
    const host = screen.getByTestId("dag-canvas-host");

    expect(panel).toHaveStyle({ width: "680px" });
    expect(host).toHaveStyle({ paddingRight: "680px" });
    expect(host).toHaveAttribute("data-task-detail-panel-open", "true");

    fireEvent.pointerDown(handle, { clientX: 680 });
    fireEvent.pointerMove(window, { clientX: 400 });

    expect(panel).toHaveStyle({ width: "800px" });
    expect(host).toHaveStyle({ paddingRight: "800px" });

    fireEvent.pointerMove(window, { clientX: 20 });
    expect(panel).toHaveStyle({ width: "1080px" });
    expect(host).toHaveStyle({ paddingRight: "1080px" });

    fireEvent.pointerUp(window);
    fireEvent.pointerMove(window, { clientX: 500 });
    expect(panel).toHaveStyle({ width: "1080px" });
  });

  it("renders structured logs without wrapping and explains log state badges", async () => {
    mockFetch.mockImplementationOnce(() =>
      Promise.resolve(
        new Response("worker=demo-node throughput_rps=172 trace_id=abc123\n", {
          status: 200,
          headers: {
            "X-Caesium-Log-Source": "persisted",
            "X-Caesium-Log-Truncated": "true",
          },
        }),
      ),
    );

    renderPanel(
      <TaskDetailPanel
        taskId="task-logs"
        jobId="job-1"
        runId="run-1"
        onClose={() => {}}
      />,
    );

    expect(await screen.findByText("Logs ready")).toBeInTheDocument();
    expect(screen.getByTestId("log-state-primary")).toHaveAttribute(
      "title",
      "Log output has loaded and is ready to inspect.",
    );
    expect(screen.getByTestId("log-source-badge")).toHaveTextContent("Retained snapshot");
    expect(screen.getByTestId("log-source-badge")).toHaveAttribute(
      "title",
      "Persisted logs from a finished task.",
    );
    expect(screen.getByTestId("log-truncated-badge")).toHaveTextContent("Truncated output");
    expect(screen.getByTestId("log-truncated-badge")).toHaveAttribute(
      "title",
      "The retained log exceeded the storage limit, so only the available tail is shown.",
    );

    const scroller = await screen.findByTestId("task-log-structured-scroll");
    const structuredText = screen.getByTestId("task-log-structured-text");
    expect(scroller).toHaveClass("overflow-auto");
    expect(structuredText).toHaveClass("whitespace-pre");
    expect(structuredText).toHaveTextContent("worker=demo-node throughput_rps=172 trace_id=abc123");
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

function noContentLogResponse() {
  return new Response(null, {
    status: 204,
    headers: {
      "X-Caesium-Log-State": "empty",
    },
  });
}
