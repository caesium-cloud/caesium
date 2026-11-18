import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { beforeEach, expect, it, vi } from "vitest";
import { api, type JobRun, type TaskRun } from "@/lib/api";
import { apiKeyLogin, clearApiKey } from "@/lib/auth";
import { RunDetailPage } from "../RunDetailPage";

vi.mock("@tanstack/react-router", () => ({
  useParams: () => ({ jobId: "job-1", runId: "run-1" }),
  useNavigate: () => vi.fn(),
  Link: ({ children }: { children: ReactNode }) => <a>{children}</a>,
}));
vi.mock("@/hooks/useDagHeight", () => ({ useDagHeight: () => [vi.fn(), 300] }));
vi.mock("@/lib/events", () => ({ events: {
  isHealthy: () => true, subscribe: vi.fn(), unsubscribe: vi.fn(),
  subscribeConnection: vi.fn(), unsubscribeConnection: vi.fn(),
} }));
vi.mock("../JobDAG", () => ({ JobDAG: () => null }));
vi.mock("../ReceiptPanel", () => ({ ReceiptPanel: () => null }));
vi.mock("../RunTimeline", () => ({ RunTimeline: () => null }));
vi.mock("../RunProposalSummary", () => ({ RunProposalSummary: () => null }));
vi.mock("../RunPicker", () => ({ RunPicker: () => null }));
vi.mock("../JobRunNavigation", () => ({ JobRunNavigation: () => null }));

const stamp = "2026-10-16T12:00:00Z";

function task(partial: Partial<TaskRun> & Pick<TaskRun, "id" | "status">): TaskRun {
  return {
    job_run_id: "run-1",
    task_id: partial.id,
    atom_id: "atom-1",
    engine: "docker",
    image: "alpine:3.23",
    command: ["true"],
    created_at: stamp,
    updated_at: stamp,
    ...partial,
  };
}

const runningTasks = [
  task({ id: "task-ok", status: "succeeded", result: "kept" }),
  task({ id: "task-hold", status: "running" }),
];

let current: JobRun;

function show() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(<QueryClientProvider client={client}><RunDetailPage /></QueryClientProvider>);
}

async function loginWithGrants(grants: Record<string, string>) {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue({
    ok: true,
    json: async () => ({ kind: "api_key", subject: "grants-principal", role: grants["*"] ?? "", grants, namespaces: Object.keys(grants), is_scoped: false, csrf_token: "csrf" }),
  }));
  expect(await apiKeyLogin("csk_live_grants")).toBe("success");
}

beforeEach(() => {
  vi.restoreAllMocks();
  clearApiKey();
  current = {
    id: "run-1", job_id: "job-1", namespace: "marketing", status: "running", tasks: runningTasks,
    started_at: stamp, created_at: stamp, updated_at: stamp,
  };
  vi.spyOn(api, "getJob").mockResolvedValue({ id: "job-1", alias: "cancel-access", namespace: "marketing", labels: {}, annotations: {}, trigger_id: "trigger-1", paused: false, created_at: stamp, updated_at: stamp });
  vi.spyOn(api, "getJobRun").mockImplementation(async () => current);
  vi.spyOn(api, "getReceipt").mockRejectedValue(new Error("No receipt"));
  vi.spyOn(api, "getJobDAG").mockResolvedValue({ job_id: "job-1", nodes: [], edges: [] });
  vi.spyOn(api, "getJobTasks").mockResolvedValue([]);
  vi.spyOn(api, "getJobRuns").mockResolvedValue({ runs: [current], total: 1, nextOffset: null });
  vi.spyOn(api, "getAtoms").mockResolvedValue([]);
  vi.spyOn(api, "getSystemFeatures").mockResolvedValue({ agent_remediation_enabled: false, database_console_enabled: false, log_console_enabled: false, freshness_enabled: false, contract_enforcement_enabled: false });
  vi.spyOn(api, "cancelRun").mockImplementation(async (jobId, runId) => {
    expect(jobId).toBe("job-1");
    expect(runId).toBe("run-1");
    current = {
      ...current,
      status: "cancelled",
      error: "cancelled by operator",
      tasks: [
        task({ id: "task-ok", status: "succeeded", result: "kept" }),
        task({ id: "task-hold", status: "cancelled" }),
      ],
    };
    return current;
  });
});

it("confirms, cancels only this run, and shows the cancelled state with the finished task kept", async () => {
  show();
  fireEvent.click(await screen.findByTestId("run-cancel-trigger"));
  expect(await screen.findByTestId("run-cancel-dialog")).toHaveTextContent("tolerant trigger rule");
  expect(screen.getByTestId("run-cancel-dialog")).toHaveTextContent("only this run");
  fireEvent.click(screen.getByTestId("run-cancel-dismiss"));
  expect(api.cancelRun).not.toHaveBeenCalled();

  fireEvent.click(screen.getByTestId("run-cancel-trigger"));
  fireEvent.click(await screen.findByTestId("run-cancel-confirm"));
  await waitFor(() => expect(api.cancelRun).toHaveBeenCalledTimes(1));
  await waitFor(() => expect(screen.getByTestId("dag-counters")).toHaveTextContent("1 succeeded"));
  expect(screen.getByTestId("dag-counters")).toHaveTextContent("1 cancelled");
  expect(screen.queryByTestId("run-cancel-trigger")).not.toBeInTheDocument();
});

it.each([
  ["an operator in both namespaces", { marketing: "operator" }, "marketing", "marketing", true],
  ["a viewer in the run's namespace", { marketing: "viewer" }, "marketing", "marketing", false],
  ["an operator who cannot act in the run's namespace", { marketing: "operator" }, "marketing", "finance", false],
])("gates Cancel for %s", async (_label, grants, jobNs, runNs, allowed) => {
  await loginWithGrants(grants);
  vi.mocked(api.getJob).mockResolvedValue({ id: "job-1", alias: "cancel-access", namespace: jobNs, labels: {}, annotations: {}, trigger_id: "trigger-1", paused: false, created_at: stamp, updated_at: stamp });
  current = { ...current, namespace: runNs };
  show();
  const trigger = await screen.findByTestId("run-cancel-trigger");
  if (allowed) {
    expect(trigger).toBeEnabled();
  } else {
    expect(trigger).toBeDisabled();
    const reason = jobNs === runNs
      ? `Requires operator role in ${runNs}`
      : `Requires operator role in ${jobNs} and ${runNs}`;
    expect(screen.getByTestId("run-cancel-gate-reason")).toHaveTextContent(reason);
    fireEvent.click(trigger);
    expect(screen.queryByTestId("run-cancel-dialog")).not.toBeInTheDocument();
    expect(api.cancelRun).not.toHaveBeenCalled();
  }
});
