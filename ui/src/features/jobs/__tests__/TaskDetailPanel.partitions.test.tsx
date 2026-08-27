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
      getPartitions: vi.fn(),
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

describe("TaskDetailPanel partition table", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.mocked(api.getPartitions).mockResolvedValue({ partitions: unstructuredRows });
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
    vi.mocked(api.getPartitions).mockResolvedValue({ partitions: structuredRows });

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

  it("refetches through getPartitions when the status filter changes", async () => {
    vi.mocked(api.getPartitions).mockImplementation((_jobId, _runId, _taskId, status) =>
      Promise.resolve({
        partitions: status ? unstructuredRows.filter((row) => row.status === status) : unstructuredRows,
      }),
    );

    renderPanel(
      <TaskDetailPanel taskId="task-fanned" jobId="job-1" runId="run-1" runTask={runTask} onClose={() => {}} />,
    );
    await openDetailsTab();

    const table = await screen.findByTestId("partition-table");
    expect(within(table).getAllByTestId("partition-row")).toHaveLength(3);

    fireEvent.change(screen.getByTestId("partition-status-filter"), { target: { value: "failed" } });

    await waitFor(() => {
      expect(api.getPartitions).toHaveBeenLastCalledWith("job-1", "run-1", "task-fanned", "failed");
    });
    await waitFor(() => {
      const filteredRows = within(table).getAllByTestId("partition-row");
      expect(filteredRows).toHaveLength(1);
      expect(filteredRows[0].textContent).toContain("bravo");
    });
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

    fireEvent.click(within(bravoRow as HTMLElement).getByRole("button", { name: /retry/i }));

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
    fireEvent.click(within(bravoRow as HTMLElement).getByRole("button", { name: /retry/i }));

    await waitFor(() => {
      expect(toast.error).toHaveBeenCalledWith("Failed to retry partition: insufficient access");
    });
  });
});
