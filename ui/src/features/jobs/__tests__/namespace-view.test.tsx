import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, api, type Job, type JobRun } from "@/lib/api";
import { apiKeyLogin, clearApiKey } from "@/lib/auth";
import { retryUnlessDenied } from "@/features/auth/access";
import { resetNamespaceViewForTests } from "@/features/auth/namespaceView";
import { JobDetailPage } from "../JobDetailPage";
import { JobsPage } from "../JobsPage";
import { RunDetailPage } from "../RunDetailPage";

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children }: { children: ReactNode }) => <a href="#">{children}</a>,
  useNavigate: () => vi.fn(),
  useParams: () => ({ jobId: "job-1", runId: "run-1" }),
  useRouterState: ({ select }: { select: (s: { location: { pathname: string } }) => unknown }) =>
    select({ location: { pathname: "/jobs/job-1" } }),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("@/lib/events", () => ({
  events: {
    isHealthy: () => true,
    subscribeConnection: vi.fn(),
    unsubscribeConnection: vi.fn(),
    subscribe: vi.fn(),
    unsubscribe: vi.fn(),
  },
}));
vi.mock("@/hooks/useDagHeight", () => ({ useDagHeight: () => [vi.fn(), 300] }));

async function login(grants: Record<string, string>) {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({
        kind: "api_key",
        subject: "k",
        role: "",
        cluster_role: "",
        grants,
        namespaces: Object.keys(grants),
        is_scoped: false,
      }),
    }),
  );
  expect(await apiKeyLogin("csk_live_nsview")).toBe("success");
}

function show(children: ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}>{children}</QueryClientProvider>);
}

const base = {
  trigger_id: "t",
  labels: {},
  annotations: {},
  paused: false,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
  last_runs: [],
};
const never = () => new Promise<never>(() => undefined);
const denied = () => new ApiError(403, "forbidden: finance", "insufficient_access");

beforeEach(() => {
  vi.restoreAllMocks();
  clearApiKey();
  window.localStorage.clear();
  resetNamespaceViewForTests();
});
afterEach(() => vi.unstubAllGlobals());

describe("job list namespace chips and view filter", () => {
  beforeEach(() => {
    vi.spyOn(api, "getJobs").mockResolvedValue([
      { ...base, id: "m", alias: "marketing-job", namespace: "marketing" },
      { ...base, id: "f", alias: "finance-job", namespace: "finance" },
    ] as Job[]);
  });

  it("shows a chip on every row, also when the principal sees one namespace", async () => {
    await login({ marketing: "runner", finance: "viewer" });
    show(<JobsPage />);
    await screen.findAllByTestId("job-row");
    expect(screen.getAllByTestId("namespace-chip").map((chip) => chip.textContent)).toEqual(["finance", "marketing"]);
  });

  it("asks the server to narrow to the stored selection", async () => {
    window.localStorage.setItem("caesium.namespaceView", "marketing");
    await login({ marketing: "runner", finance: "viewer" });
    show(<JobsPage />);
    await screen.findAllByTestId("job-row");
    await waitFor(() => expect(api.getJobs).toHaveBeenCalledWith("marketing"));
  });

  it("requests the plain list without a selection", async () => {
    await login({ marketing: "runner", finance: "viewer" });
    show(<JobsPage />);
    await screen.findAllByTestId("job-row");
    expect(api.getJobs).toHaveBeenCalledWith(undefined);
    expect(api.getJobs).not.toHaveBeenCalledWith("marketing");
  });
});

describe("a deep link outside the grants", () => {
  function stubJobPageQueries() {
    for (const name of ["getAllJobRuns", "getJobQueue", "getJobDAG", "getJobTasks", "getAtoms", "getSystemFeatures", "getJobRuns", "getReceipt"] as const) {
      vi.spyOn(api, name).mockImplementation(never as never);
    }
  }

  it("renders InsufficientAccess on the job page, not Job not found and not the server's text", async () => {
    await login({ marketing: "runner" });
    stubJobPageQueries();
    vi.spyOn(api, "getJob").mockRejectedValue(denied());
    show(<JobDetailPage />);
    const panel = await screen.findByTestId("job-insufficient-access");
    expect(panel).toHaveTextContent("You do not have access to this job's namespace.");
    expect(panel).not.toHaveTextContent("finance");
    expect(screen.queryByText(/job not found/i)).toBeNull();
    expect(api.getJob).toHaveBeenCalledTimes(1);
  });

  it("retries other failures as before but never a denial", () => {
    expect(retryUnlessDenied(0, denied())).toBe(false);
    expect(retryUnlessDenied(0, new ApiError(404, "not found"))).toBe(true);
    expect(retryUnlessDenied(3, new ApiError(500, "boom"))).toBe(false);
  });

  it("renders InsufficientAccess on a run of a moved job: the job read passes, the run's namespace is denied", async () => {
    await login({ marketing: "runner" });
    stubJobPageQueries();
    vi.spyOn(api, "getJob").mockResolvedValue({ ...base, id: "job-1", alias: "moved", namespace: "marketing" } as Job);
    vi.spyOn(api, "getJobRun").mockRejectedValue(denied());
    show(<RunDetailPage />);
    const panel = await screen.findByTestId("job-insufficient-access");
    expect(panel).not.toHaveTextContent("finance");
    expect(screen.queryByText(/not found/i)).toBeNull();
    expect(api.getJobRun).toHaveBeenCalledTimes(1);
  });

  it("does not show InsufficientAccess for a run it can read", async () => {
    await login({ marketing: "runner" });
    stubJobPageQueries();
    vi.spyOn(api, "getJob").mockResolvedValue({ ...base, id: "job-1", alias: "ok", namespace: "marketing" } as Job);
    vi.spyOn(api, "getJobRun").mockResolvedValue({ id: "run-1", job_id: "job-1", status: "succeeded", tasks: [] } as unknown as JobRun);
    show(<RunDetailPage />);
    await waitFor(() => expect(api.getJobRun).toHaveBeenCalled());
    expect(screen.queryByTestId("job-insufficient-access")).toBeNull();
  });
});
