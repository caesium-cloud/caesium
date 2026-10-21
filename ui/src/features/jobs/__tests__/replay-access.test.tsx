import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { api, type JobRun } from "@/lib/api";
import { apiKeyLogin, checkSession, clearApiKey, getPrincipal } from "@/lib/auth";
import { RunDetailPage } from "../RunDetailPage";
import { ReplayDialog } from "../ReplayDialog";

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

const baseline: JobRun = {
  id: "run-1", job_id: "job-1", status: "succeeded", tasks: [],
  started_at: "2026-10-16T12:00:00Z", created_at: "2026-10-16T12:00:00Z", updated_at: "2026-10-16T12:00:01Z",
};

function show(children: ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(<QueryClientProvider client={client}>{children}</QueryClientProvider>);
}

async function login(role: string, kind = "api_key") {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue({
    ok: true,
    json: async () => ({ kind, subject: "namespace-principal", role, is_scoped: true, csrf_token: "csrf" }),
  }));
  if (kind === "user") expect(await checkSession()).toBe(true);
  else expect(await apiKeyLogin("csk_live_namespace")).toBe("success");
}

afterEach(() => { vi.unstubAllGlobals(); });

beforeEach(() => {
  vi.restoreAllMocks();
  clearApiKey();
  vi.spyOn(api, "getJob").mockResolvedValue({ id: "job-1", alias: "replay-access", labels: {}, annotations: {}, trigger_id: "trigger-1", paused: false, created_at: baseline.created_at, updated_at: baseline.updated_at });
  vi.spyOn(api, "getJobRun").mockResolvedValue(baseline);
  vi.spyOn(api, "getReceipt").mockRejectedValue(new Error("No receipt"));
  vi.spyOn(api, "getJobDAG").mockResolvedValue({ job_id: "job-1", nodes: [], edges: [] });
  vi.spyOn(api, "getJobTasks").mockResolvedValue([]);
  vi.spyOn(api, "getJobRuns").mockResolvedValue({ runs: [baseline], total: 1, nextOffset: null });
  vi.spyOn(api, "getAtoms").mockResolvedValue([]);
  vi.spyOn(api, "getSystemFeatures").mockResolvedValue({ agent_remediation_enabled: false, database_console_enabled: false, log_console_enabled: false, freshness_enabled: false, contract_enforcement_enabled: false });
  vi.spyOn(api, "postReplay").mockResolvedValue({ run_id: "replay-1", status: "succeeded", quarantine: true });
});

it.each(["api_key", "user"])("gates replay and its shortcut for a %s with an empty cluster role", async kind => {
  await login("", kind);
  expect(getPrincipal()).toMatchObject({ kind, role: null, canRunner: false });
  show(<RunDetailPage />);
  const trigger = await screen.findByTestId("run-replay-trigger");
  expect(trigger).toBeDisabled();
  expect(screen.getByTestId("run-replay-gate-reason")).toHaveTextContent("Requires runner role");
  fireEvent.click(trigger);
  fireEvent.keyDown(document.body, { key: "p" });
  expect(screen.queryByTestId("replay-dialog")).not.toBeInTheDocument();
  expect(api.postReplay).not.toHaveBeenCalled();
});

it.each(["auth-disabled", "runner"])("preserves the replay shortcut and submission for %s", async mode => {
  if (mode === "runner") await login("runner");
  show(<RunDetailPage />);
  expect(await screen.findByTestId("run-replay-trigger")).toBeEnabled();
  fireEvent.keyDown(document.body, { key: "p" });
  expect(await screen.findByTestId("replay-dialog")).toBeVisible();
  fireEvent.click(screen.getByTestId("replay-submit"));
  await waitFor(() => expect(api.postReplay).toHaveBeenCalledWith("job-1", "run-1", { set: {} }, expect.any(String)));
});

it("refuses a forced dialog submission after switching to an authenticated principal with no cluster grant", async () => {
  await login("runner");
  show(<ReplayDialog jobId="job-1" baselineRunId="run-1" open onOpenChange={vi.fn()} />);
  expect(await screen.findByTestId("replay-submit")).toBeEnabled();
  await act(async () => { await login(""); });
  const submit = screen.getByTestId("replay-submit");
  expect(submit).toBeDisabled();
  fireEvent.submit(submit.closest("form")!);
  expect(api.postReplay).not.toHaveBeenCalled();
  expect(screen.getByRole("alert")).toHaveTextContent("Requires runner role");
});

async function loginWithGrants(grants: Record<string, string>) {
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue({
    ok: true,
    json: async () => ({ kind: "api_key", subject: "grants-principal", role: grants["*"] ?? "", grants, namespaces: Object.keys(grants), is_scoped: false }),
  }));
  expect(await apiKeyLogin("csk_live_grants")).toBe("success");
}

function withNamespaces(jobNamespace: string, runNamespace: string) {
  vi.mocked(api.getJob).mockResolvedValue({ id: "job-1", alias: "replay-access", namespace: jobNamespace, labels: {}, annotations: {}, trigger_id: "trigger-1", paused: false, created_at: baseline.created_at, updated_at: baseline.updated_at });
  vi.mocked(api.getJobRun).mockResolvedValue({ ...baseline, namespace: runNamespace });
}

it.each([
  ["a runner in the job's namespace", { marketing: "runner" }, "marketing", "marketing", true, true],
  ["a runner in another namespace", { finance: "runner" }, "marketing", "marketing", false, false],
  ["a viewer in the job's namespace", { marketing: "viewer" }, "marketing", "marketing", false, false],
  ["a cluster runner", { "*": "runner" }, "marketing", "marketing", true, true],
  ["a runner who cannot read the run's persisted namespace", { marketing: "runner" }, "marketing", "finance", false, true],
])("gates Replay and Re-run for %s", async (_label, grants, jobNs, runNs, replayAllowed, rerunAllowed) => {
  await loginWithGrants(grants);
  withNamespaces(jobNs, runNs);
  show(<RunDetailPage />);
  const replay = await screen.findByTestId("run-replay-trigger");
  const rerun = screen.getByTestId("run-rerun-trigger");
  if (replayAllowed) expect(replay).toBeEnabled(); else expect(replay).toBeDisabled();
  if (rerunAllowed) expect(rerun).toBeEnabled(); else expect(rerun).toBeDisabled();
});
