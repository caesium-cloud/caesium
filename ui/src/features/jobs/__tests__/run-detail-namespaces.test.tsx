import { fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { api, type JobRun } from "@/lib/api";
import { apiKeyLogin, clearApiKey } from "@/lib/auth";
import { RunDetailPage } from "../RunDetailPage";

const panel = vi.hoisted(() => ({ props: null as null | Record<string, unknown> }));

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
vi.mock("../JobDAG", () => ({
  JobDAG: ({ onNodeClick }: { onNodeClick?: (id: string) => void }) => (
    <button data-testid="pick-task" onClick={() => onNodeClick?.("task-1")}>pick</button>
  ),
}));
vi.mock("../TaskDetailPanel", () => ({
  TaskDetailPanel: (props: Record<string, unknown>) => {
    panel.props = props;
    return <div data-testid="task-panel" />;
  },
}));
vi.mock("../ReceiptPanel", () => ({ ReceiptPanel: () => null }));
vi.mock("../RunTimeline", () => ({ RunTimeline: () => null }));
vi.mock("../RunProposalSummary", () => ({ RunProposalSummary: () => null }));
vi.mock("../RunPicker", () => ({ RunPicker: () => null }));
vi.mock("../JobRunNavigation", () => ({ JobRunNavigation: () => null }));

const run: JobRun = {
  id: "run-1", job_id: "job-1", namespace: "marketing", status: "succeeded", tasks: [],
  started_at: "2026-10-16T12:00:00Z", created_at: "2026-10-16T12:00:00Z", updated_at: "2026-10-16T12:00:01Z",
};

beforeEach(() => {
  vi.restoreAllMocks();
  clearApiKey();
  panel.props = null;
  vi.spyOn(api, "getJob").mockResolvedValue({ id: "job-1", alias: "a", namespace: "marketing", labels: {}, annotations: {}, trigger_id: "t", paused: false, created_at: run.created_at, updated_at: run.updated_at });
  vi.spyOn(api, "getJobRun").mockResolvedValue(run);
  vi.spyOn(api, "getReceipt").mockRejectedValue(new Error("No receipt"));
  vi.spyOn(api, "getJobDAG").mockResolvedValue({ job_id: "job-1", nodes: [{ id: "task-1" }], edges: [] } as never);
  vi.spyOn(api, "getJobTasks").mockResolvedValue([]);
  vi.spyOn(api, "getJobRuns").mockResolvedValue({ runs: [run], total: 1, nextOffset: null });
  vi.spyOn(api, "getAtoms").mockResolvedValue([]);
  vi.spyOn(api, "getSystemFeatures").mockResolvedValue({ agent_remediation_enabled: false, database_console_enabled: false, log_console_enabled: false, freshness_enabled: false, contract_enforcement_enabled: false });
});
afterEach(() => vi.unstubAllGlobals());

it("hands the job's and the run's namespaces to the task panel so confined principals keep partition retry and cache invalidation", async () => {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue({
    ok: true,
    json: async () => ({ kind: "api_key", subject: "k", role: "", grants: { marketing: "operator" }, namespaces: ["marketing"], is_scoped: false }),
  }));
  expect(await apiKeyLogin("csk_live_ns")).toBe("success");
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}><RunDetailPage /></QueryClientProvider>);

  fireEvent.click(await screen.findByTestId("pick-task"));
  await screen.findByTestId("task-panel");
  expect(panel.props).toMatchObject({ jobNamespace: "marketing", runNamespace: "marketing" });
});
