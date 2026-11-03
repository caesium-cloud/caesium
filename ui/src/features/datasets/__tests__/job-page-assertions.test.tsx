import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { api, type JobRun, type RunAssertions, type TaskRun } from "@/lib/api";
import { DataAssertionsPanel } from "../DataAssertionsPanel";
import { JobDetailPage } from "@/features/jobs/JobDetailPage";

// The job page mounts the same task panel as the run page, and reads the
// featured run through its own query. These tests drive the whole page and
// read what the panel's assertions section shows as that run changes.

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children }: { children: ReactNode }) => <a href="#">{children}</a>,
  useNavigate: () => vi.fn(),
  useParams: () => ({ jobId: "job-1" }),
  useRouterState: ({ select }: { select: (s: { location: { pathname: string } }) => unknown }) =>
    select({ location: { pathname: "/jobs/job-1" } }),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("@/lib/auth", () => ({
  usePrincipal: () => ({ subject: "viewer", role: "viewer", isScoped: false }),
  withAuthHeaders: () => ({}),
}));
vi.mock("@/lib/events", () => ({
  events: {
    isHealthy: () => true,
    subscribe: vi.fn(),
    unsubscribe: vi.fn(),
    subscribeConnection: vi.fn(),
    unsubscribeConnection: vi.fn(),
  },
}));
vi.mock("@/hooks/useDagHeight", () => ({ useDagHeight: () => [vi.fn(), 300] }));
vi.mock("@/features/jobs/JobDAG", () => ({
  JobDAG: ({ onNodeClick }: { onNodeClick?: (id: string) => void }) => (
    <button data-testid="pick-task" onClick={() => onNodeClick?.("task-1")}>
      pick
    </button>
  ),
}));
// The real panel is large; what matters here is the assertions section it
// mounts, with the props the job page hands it.
vi.mock("@/features/jobs/TaskDetailPanel", () => ({
  TaskDetailPanel: (props: {
    runTask?: TaskRun;
    jobId: string;
    runId: string;
    runStatus?: string;
  }) =>
    props.runTask ? (
      <DataAssertionsPanel
        task={props.runTask}
        jobId={props.jobId}
        runId={props.runId}
        live={props.runStatus === "running"}
      />
    ) : null,
}));

const finished = "2026-11-02T10:00:05Z";

function runOf(patch: Partial<JobRun>, task: Partial<TaskRun> = {}): JobRun {
  return {
    id: "run-1",
    job_id: "job-1",
    status: "succeeded",
    started_at: "2026-11-02T10:00:00Z",
    created_at: "2026-11-02T10:00:00Z",
    updated_at: finished,
    tasks: [
      {
        id: "tr-1",
        task_id: "task-1",
        job_run_id: "run-1",
        status: "succeeded",
        attempt: 1,
        started_at: "2026-11-02T10:00:00Z",
        completed_at: finished,
        ...task,
      } as TaskRun,
    ],
    ...patch,
  } as JobRun;
}

function resultsOf(metric: string): RunAssertions {
  return {
    run_id: "run-1",
    job_id: "job-1",
    tasks: [
      {
        task_id: "task-1",
        fanned: false,
        instance_count: 1,
        verdict_counts: { passed: 1 },
        instances: [
          {
            task_run_id: "tr-1",
            status: "succeeded",
            attempt: 1,
            cache_hit: false,
            verdict: "passed",
            results: [{ dataset: "d", metric, assertion: "min", outcome: "passed" }],
          },
        ],
      },
    ],
  };
}

const none: RunAssertions = { run_id: "run-1", job_id: "job-1", tasks: [] };
const runKey = ["job", "job-1", "runs", "run-1"];

function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <JobDetailPage />
    </QueryClientProvider>,
  );
  return client;
}

beforeEach(() => {
  vi.restoreAllMocks();
  const run = runOf({ status: "running" }, { status: "running", completed_at: undefined });
  vi.spyOn(api, "getJob").mockResolvedValue({
    id: "job-1",
    alias: "a",
    namespace: "marketing",
    labels: {},
    annotations: {},
    trigger_id: "t",
    paused: false,
    created_at: run.created_at,
    updated_at: run.updated_at,
  } as Awaited<ReturnType<typeof api.getJob>>);
  vi.spyOn(api, "getJobRun").mockResolvedValue(run);
  vi.spyOn(api, "getAllJobRuns").mockResolvedValue({
    runs: [run],
    total: 1,
    truncated: false,
  } as Awaited<ReturnType<typeof api.getAllJobRuns>>);
  vi.spyOn(api, "getJobQueue").mockResolvedValue([]);
  vi.spyOn(api, "getReceipt").mockRejectedValue(new Error("No receipt"));
  vi.spyOn(api, "getJobDAG").mockResolvedValue({
    job_id: "job-1",
    nodes: [{ id: "task-1" }],
    edges: [],
  } as never);
  vi.spyOn(api, "getJobTasks").mockResolvedValue([]);
  vi.spyOn(api, "getJobRuns").mockResolvedValue({ runs: [run], total: 1, nextOffset: null });
  vi.spyOn(api, "getAtoms").mockResolvedValue([]);
  vi.spyOn(api, "getSystemFeatures").mockResolvedValue({
    data_assertions_enabled: true,
  } as Awaited<ReturnType<typeof api.getSystemFeatures>>);
});

async function openTask() {
  fireEvent.click(await screen.findByTestId("pick-task"));
}

describe("the job page's task panel", () => {
  it("reads the results again when the run completes between two polls", async () => {
    const read = vi
      .spyOn(api, "getRunAssertions")
      .mockResolvedValueOnce(none) // the final task had not recorded yet
      .mockResolvedValue(resultsOf("rowCount"));
    const client = mount();
    await openTask();
    await waitFor(() => expect(read).toHaveBeenCalledTimes(1));
    expect(screen.queryByTestId("data-assertions-panel")).toBeNull();

    // The next run poll (or event) shows the task finished and the run done.
    act(() => {
      client.setQueryData(runKey, runOf({ status: "succeeded" }));
    });
    await waitFor(() => expect(read).toHaveBeenCalledTimes(2));
    expect(await screen.findByText(/rowCount\.min: Passed/)).toBeTruthy();
  });

  it("reads the results again when a retry finished between two polls", async () => {
    const done = runOf({ status: "succeeded" });
    vi.spyOn(api, "getJobRun").mockResolvedValue(done);
    vi.spyOn(api, "getAllJobRuns").mockResolvedValue({
      runs: [done],
      total: 1,
      truncated: false,
    } as Awaited<ReturnType<typeof api.getAllJobRuns>>);
    const read = vi
      .spyOn(api, "getRunAssertions")
      .mockResolvedValueOnce(resultsOf("firstAttempt"))
      .mockResolvedValue(resultsOf("secondAttempt"));
    const client = mount();
    await openTask();
    expect(await screen.findByText(/firstAttempt\.min: Passed/)).toBeTruthy();

    // Same status as before, new attempt's evidence; nothing was live.
    act(() => {
      client.setQueryData(
        runKey,
        runOf(
          { status: "succeeded" },
          {
            attempt: 2,
            started_at: "2026-11-02T10:00:20Z",
            completed_at: "2026-11-02T10:00:25Z",
          },
        ),
      );
    });
    await waitFor(() => expect(read).toHaveBeenCalledTimes(2));
    expect(await screen.findByText(/secondAttempt\.min: Passed/)).toBeTruthy();
    expect(screen.queryByText(/firstAttempt/)).toBeNull();
  });
});
