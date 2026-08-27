import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactElement } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { api, type PartitionInstance, type TaskRun } from "@/lib/api";
import { TaskDetailPanel } from "../TaskDetailPanel";

vi.mock("@/lib/auth", () => ({
  withAuthHeaders: () => ({}),
}));

// The default tab renders LogViewer, which drives a real xterm.js terminal.
// jsdom has no matchMedia/canvas support for it, so stub it out the same way
// the sibling TaskDetailPanel.test.tsx does — these tests only care about the
// "Details" tab's partition table.
vi.mock("xterm", () => ({
  Terminal: vi.fn().mockImplementation(function () {
    return {
      loadAddon: vi.fn(),
      open: vi.fn(),
      reset: vi.fn(),
      write: vi.fn((_chunk: string, callback?: () => void) => callback?.()),
      dispose: vi.fn(),
    };
  }),
}));

vi.mock("xterm-addon-fit", () => ({
  FitAddon: vi.fn().mockImplementation(function () {
    return { fit: vi.fn() };
  }),
}));

vi.mock("sonner", () => ({
  toast: {
    error: vi.fn(),
    success: vi.fn(),
  },
}));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      // The panel reads the WHOLE group through getAllPartitions (which pages
      // internally); getPartitions is the single-page primitive and is mocked
      // only so a stray call would be visible rather than hitting the network.
      getPartitions: vi.fn(),
      getAllPartitions: vi.fn(),
      retryPartition: vi.fn(),
    },
  };
});

const { toast } = await import("sonner");

const unstructuredRows: PartitionInstance[] = [
  {
    value: "alpha",
    index: 0,
    status: "succeeded",
    attempt: 1,
    cache_hit: true,
    duration: "1.2s",
    task_run_id: "tr-alpha",
  },
  {
    value: "bravo",
    index: 1,
    status: "failed",
    attempt: 2,
    cache_hit: false,
    duration: "800ms",
    error: "boom",
    task_run_id: "tr-bravo",
  },
  {
    value: "charlie",
    index: 2,
    status: "running",
    attempt: 1,
    cache_hit: false,
    task_run_id: "tr-charlie",
  },
];

const structuredRows: PartitionInstance[] = [
  {
    value: "alpha",
    index: 0,
    status: "succeeded",
    attempt: 1,
    cache_hit: true,
    duration: "1.2s",
    fingerprint: "sha256:abcdef1234567890",
    depends_on: [],
    task_run_id: "tr-alpha",
  },
  {
    value: "bravo",
    index: 1,
    status: "succeeded",
    attempt: 1,
    cache_hit: false,
    duration: "900ms",
    fingerprint: "sha256:0987654321fedcba",
    depends_on: ["alpha"],
    task_run_id: "tr-bravo",
  },
];

const runTask: TaskRun = {
  id: "task-fanned",
  job_run_id: "run-1",
  task_id: "task-fanned",
  atom_id: "atom-1",
  engine: "docker",
  image: "alpine:3.23",
  command: ["echo", "partition"],
  status: "succeeded",
  partition_count: 3,
  created_at: "2026-08-01T00:00:00Z",
  updated_at: "2026-08-01T00:00:00Z",
};

/** The full-group envelope api.getAllPartitions resolves to. */
function groupOf(rows: PartitionInstance[], statusCounts?: Record<string, number>) {
  return {
    partitions: rows,
    total: rows.length,
    limit: 500,
    offset: 0,
    next_offset: null,
    status_counts:
      statusCounts ??
      rows.reduce<Record<string, number>>((counts, row) => {
        counts[row.status] = (counts[row.status] ?? 0) + 1;
        return counts;
      }, {}),
  };
}

function noContentLogResponse() {
  return new Response(null, {
    status: 204,
    headers: { "X-Caesium-Log-State": "empty" },
  });
}

function renderPanel(component: ReactElement) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(<QueryClientProvider client={queryClient}>{component}</QueryClientProvider>);
}

async function openDetailsTab() {
  fireEvent.click(await screen.findByRole("button", { name: /details/i }));
}

/** Every log URL the panel requested, in order. */
function logRequestURLs(): string[] {
  return vi
    .mocked(globalThis.fetch)
    .mock.calls.map((call) => String(call[0]))
    .filter((url) => url.includes("/logs?"));
}

describe("TaskDetailPanel partition table", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.mocked(api.getAllPartitions).mockResolvedValue(groupOf(unstructuredRows));
    globalThis.fetch = vi.fn(() => Promise.resolve(noContentLogResponse())) as unknown as typeof fetch;

    // @tanstack/react-virtual measures the scroll container via
    // offsetWidth/offsetHeight on mount; jsdom reports 0 for both, which
    // makes the virtualizer's row range empty (outerSize <= 0). Stub a real
    // viewport size so the partition rows actually render under jsdom.
    Object.defineProperty(HTMLElement.prototype, "offsetHeight", {
      configurable: true,
      value: 240,
    });
    Object.defineProperty(HTMLElement.prototype, "offsetWidth", {
      configurable: true,
      value: 640,
    });
  });

  it("renders partition rows and hides fingerprint/depends-on for an unstructured group", async () => {
    renderPanel(
      <TaskDetailPanel taskId="task-fanned" jobId="job-1" runId="run-1" runTask={runTask} onClose={() => {}} />,
    );
    await openDetailsTab();

    const table = await screen.findByTestId("partition-table");
    const rows = within(table).getAllByTestId("partition-row");
    expect(rows).toHaveLength(3);

    const rowText = rows.map((row) => row.textContent ?? "");
    expect(rowText.some((text) => text.includes("alpha"))).toBe(true);
    expect(rowText.some((text) => text.includes("bravo"))).toBe(true);
    expect(rowText.some((text) => text.includes("charlie"))).toBe(true);

    expect(within(table).queryByText("Fingerprint")).not.toBeInTheDocument();
    expect(within(table).queryByText("Depends on")).not.toBeInTheDocument();
  });

  it("shows fingerprint and depends-on columns only when a row is structured", async () => {
    vi.mocked(api.getAllPartitions).mockResolvedValue(groupOf(structuredRows));

    renderPanel(
      <TaskDetailPanel taskId="task-fanned" jobId="job-1" runId="run-1" runTask={runTask} onClose={() => {}} />,
    );
    await openDetailsTab();

    const table = await screen.findByTestId("partition-table");
    expect(within(table).getByText("Fingerprint")).toBeInTheDocument();
    expect(within(table).getByText("Depends on")).toBeInTheDocument();
    // Fingerprint cell is truncated to the first 12 characters of the value.
    expect(within(table).getByText("sha256:abcde")).toBeInTheDocument();
    const bravoRow = within(table)
      .getAllByTestId("partition-row")
      .find((row) => row.textContent?.includes("bravo"));
    expect(bravoRow?.textContent).toContain("alpha");
  });

  it("refetches through the paging reader when the status filter changes", async () => {
    vi.mocked(api.getAllPartitions).mockImplementation((_jobId, _runId, _taskId, status) =>
      Promise.resolve(
        groupOf(
          status ? unstructuredRows.filter((row) => row.status === status) : unstructuredRows,
          // status_counts always describes the WHOLE group, even under a filter.
          { succeeded: 1, failed: 1, running: 1 },
        ),
      ),
    );

    renderPanel(
      <TaskDetailPanel taskId="task-fanned" jobId="job-1" runId="run-1" runTask={runTask} onClose={() => {}} />,
    );
    await openDetailsTab();

    const table = await screen.findByTestId("partition-table");
    expect(within(table).getAllByTestId("partition-row")).toHaveLength(3);

    fireEvent.change(screen.getByTestId("partition-status-filter"), { target: { value: "failed" } });

    await waitFor(() => {
      expect(api.getAllPartitions).toHaveBeenLastCalledWith("job-1", "run-1", "task-fanned", "failed");
    });
    await waitFor(() => {
      const filteredRows = within(table).getAllByTestId("partition-row");
      expect(filteredRows).toHaveLength(1);
      expect(filteredRows[0].textContent).toContain("bravo");
    });

    // The header must keep reporting the group, not the filtered slice: a
    // filtered table that renamed itself "×1" would claim the fan-out shrank.
    expect(screen.getByTestId("partition-table-total").textContent).toContain("×3");
    expect(screen.getByTestId("partition-table-total").textContent).toContain("1 failed");
  });

  it("reports the whole group's size in the header, not one page", async () => {
    // A 1200-instance group: the table renders the rows the paging reader
    // collected, and the header states the group total.
    const many = Array.from({ length: 1200 }, (_unused, index) => ({
      value: `p${index}`,
      index,
      status: "succeeded",
      attempt: 1,
      cache_hit: false,
      task_run_id: `tr-${index}`,
    }));
    vi.mocked(api.getAllPartitions).mockResolvedValue(groupOf(many));

    renderPanel(
      <TaskDetailPanel
        taskId="task-fanned"
        jobId="job-1"
        runId="run-1"
        runTask={{ ...runTask, partition_count: 1200 }}
        onClose={() => {}}
      />,
    );
    await openDetailsTab();

    await screen.findByTestId("partition-table");
    expect(screen.getByTestId("partition-table-total").textContent).toContain("×1200");
  });

  it("retries a failed partition through the retry API", async () => {
    vi.mocked(api.retryPartition).mockResolvedValue({ retried: true, index: 1, value: "bravo" });

    renderPanel(
      <TaskDetailPanel taskId="task-fanned" jobId="job-1" runId="run-1" runTask={runTask} onClose={() => {}} />,
    );
    await openDetailsTab();

    const table = await screen.findByTestId("partition-table");
    const bravoRow = within(table).getAllByTestId("partition-row").find((row) => row.textContent?.includes("bravo"));
    expect(bravoRow).toBeDefined();

    fireEvent.click(within(bravoRow as HTMLElement).getByRole("button", { name: /^retry$/i }));

    await waitFor(() => {
      expect(api.retryPartition).toHaveBeenCalledWith("job-1", "run-1", "task-fanned", 1);
    });
    await waitFor(() => {
      expect(toast.success).toHaveBeenCalledWith("Partition retry requested");
    });
  });

  it("surfaces a retry API error instead of failing silently", async () => {
    vi.mocked(api.retryPartition).mockRejectedValue(new Error("insufficient access"));

    renderPanel(
      <TaskDetailPanel taskId="task-fanned" jobId="job-1" runId="run-1" runTask={runTask} onClose={() => {}} />,
    );
    await openDetailsTab();

    const table = await screen.findByTestId("partition-table");
    const bravoRow = within(table).getAllByTestId("partition-row").find((row) => row.textContent?.includes("bravo"));
    fireEvent.click(within(bravoRow as HTMLElement).getByRole("button", { name: /^retry$/i }));

    await waitFor(() => {
      expect(toast.error).toHaveBeenCalledWith("Failed to retry partition: insufficient access");
    });
  });

  // A fan-out that materialized exactly ONE instance is still a fan-out: it has
  // a partition value, its own TaskRun and its own log. The table used to be
  // gated on `partition_count > 1`, so that group rendered as a plain task and
  // its partition identity was unreachable from the UI.
  it("shows the partition table for a group that expanded to a single instance", async () => {
    const single: PartitionInstance[] = [
      {
        value: "only",
        index: 0,
        status: "succeeded",
        attempt: 1,
        cache_hit: false,
        duration: "300ms",
        task_run_id: "tr-only",
      },
    ];
    vi.mocked(api.getAllPartitions).mockResolvedValue(groupOf(single));

    renderPanel(
      <TaskDetailPanel
        taskId="task-fanned"
        jobId="job-1"
        runId="run-1"
        runTask={{ ...runTask, partition_count: 1, partition_value: "only" }}
        onClose={() => {}}
      />,
    );
    await openDetailsTab();

    const table = await screen.findByTestId("partition-table");
    expect(within(table).getAllByTestId("partition-row")).toHaveLength(1);
    expect(table.textContent).toContain("only");
    expect(screen.getByTestId("partition-table-total").textContent).toContain("×1");
  });

  it("hides the partition table for an unfanned task", async () => {
    renderPanel(
      <TaskDetailPanel
        taskId="task-plain"
        jobId="job-1"
        runId="run-1"
        runTask={{ ...runTask, partition_count: undefined, partition_value: undefined }}
        onClose={() => {}}
      />,
    );
    await openDetailsTab();

    await screen.findByText("Trigger Rule");
    expect(screen.queryByTestId("partition-table")).not.toBeInTheDocument();
    expect(api.getAllPartitions).not.toHaveBeenCalled();
  });
});

describe("TaskDetailPanel fan-out log selection", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.mocked(api.getAllPartitions).mockResolvedValue(groupOf(unstructuredRows));
    globalThis.fetch = vi.fn(() => Promise.resolve(noContentLogResponse())) as unknown as typeof fetch;
    Object.defineProperty(HTMLElement.prototype, "offsetHeight", { configurable: true, value: 240 });
    Object.defineProperty(HTMLElement.prototype, "offsetWidth", { configurable: true, value: 640 });
  });

  // The backend answers 400 for a fanned task whose log request names no
  // instance, because task_id alone maps to N containers. The panel therefore
  // has to pick one, and the useful default is the failure the operator opened
  // the panel to read.
  it("defaults a fanned task's Logs tab to the first failed instance", async () => {
    renderPanel(
      <TaskDetailPanel taskId="task-fanned" jobId="job-1" runId="run-1" runTask={runTask} onClose={() => {}} />,
    );

    await screen.findByTestId("log-partition-select");
    await waitFor(() => {
      expect(logRequestURLs().some((url) => url.includes("task_run_id=tr-bravo"))).toBe(true);
    });
    expect((screen.getByTestId("log-partition-select") as HTMLSelectElement).value).toBe("tr-bravo");
    // Never the ambiguous request: task_id with no selector is the 400.
    expect(logRequestURLs().every((url) => url.includes("task_run_id="))).toBe(true);
  });

  it("falls back to instance 0 when nothing failed", async () => {
    const allGreen = unstructuredRows.map((row) => ({ ...row, status: "succeeded", error: undefined }));
    vi.mocked(api.getAllPartitions).mockResolvedValue(groupOf(allGreen));

    renderPanel(
      <TaskDetailPanel taskId="task-fanned" jobId="job-1" runId="run-1" runTask={runTask} onClose={() => {}} />,
    );

    await screen.findByTestId("log-partition-select");
    await waitFor(() => {
      expect(logRequestURLs().some((url) => url.includes("task_run_id=tr-alpha"))).toBe(true);
    });
  });

  it("re-requests the log when a different partition is selected", async () => {
    renderPanel(
      <TaskDetailPanel taskId="task-fanned" jobId="job-1" runId="run-1" runTask={runTask} onClose={() => {}} />,
    );

    const select = await screen.findByTestId("log-partition-select");
    await waitFor(() => {
      expect(logRequestURLs().some((url) => url.includes("task_run_id=tr-bravo"))).toBe(true);
    });

    fireEvent.change(select, { target: { value: "tr-charlie" } });

    await waitFor(() => {
      expect(logRequestURLs().some((url) => url.includes("task_run_id=tr-charlie"))).toBe(true);
    });
  });

  // Without a per-row action the table exposed only Retry, so a SUCCEEDED
  // instance's log had no route at all from the partition table.
  it("opens one instance's log from its row's Logs action", async () => {
    renderPanel(
      <TaskDetailPanel taskId="task-fanned" jobId="job-1" runId="run-1" runTask={runTask} onClose={() => {}} />,
    );
    await openDetailsTab();

    const table = await screen.findByTestId("partition-table");
    const charlieRow = within(table)
      .getAllByTestId("partition-row")
      .find((row) => row.textContent?.includes("charlie"));
    expect(charlieRow).toBeDefined();

    fireEvent.click(within(charlieRow as HTMLElement).getByTestId("partition-logs-button"));

    // The action switches to the Logs tab AND selects that instance.
    const select = (await screen.findByTestId("log-partition-select")) as HTMLSelectElement;
    expect(select.value).toBe("tr-charlie");
    await waitFor(() => {
      expect(logRequestURLs().some((url) => url.includes("task_run_id=tr-charlie"))).toBe(true);
    });
  });

  it("every row exposes a Logs action, not only failed ones", async () => {
    renderPanel(
      <TaskDetailPanel taskId="task-fanned" jobId="job-1" runId="run-1" runTask={runTask} onClose={() => {}} />,
    );
    await openDetailsTab();

    const table = await screen.findByTestId("partition-table");
    expect(within(table).getAllByTestId("partition-logs-button")).toHaveLength(3);
    // Retry stays scoped to the terminal failure.
    expect(within(table).getAllByTestId("partition-retry-button")).toHaveLength(1);
  });

  it("requests an unfanned task's log with no instance selector", async () => {
    renderPanel(
      <TaskDetailPanel
        taskId="task-plain"
        jobId="job-1"
        runId="run-1"
        runTask={{ ...runTask, partition_count: undefined, partition_value: undefined }}
        onClose={() => {}}
      />,
    );

    await waitFor(() => {
      expect(logRequestURLs()).toHaveLength(1);
    });
    expect(logRequestURLs()[0]).toContain("task_id=task-plain");
    expect(logRequestURLs()[0]).not.toContain("task_run_id=");
    expect(screen.queryByTestId("log-partition-select")).not.toBeInTheDocument();
  });
});
