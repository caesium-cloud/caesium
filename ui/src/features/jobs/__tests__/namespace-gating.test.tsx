import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, type Backfill, type Job } from "@/lib/api";
import { apiKeyLogin, clearApiKey } from "@/lib/auth";
import { BackfillsView } from "../BackfillsView";
import { CacheView } from "../CacheView";
import { JobsPage } from "../JobsPage";

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children }: { children: ReactNode }) => <a href="#">{children}</a>,
  useNavigate: () => vi.fn(),
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

/** Sign in through the real whoami parser with the given grants. */
async function loginWithGrants(grants: Record<string, string>, clusterRole = "") {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({
        kind: "api_key",
        subject: "grants-principal",
        role: clusterRole,
        cluster_role: clusterRole,
        grants,
        namespaces: Object.keys(grants),
        is_scoped: false,
      }),
    }),
  );
  expect(await apiKeyLogin("csk_live_grants")).toBe("success");
}

function show(children: ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}>{children}</QueryClientProvider>);
}

const baseJob = {
  trigger_id: "t",
  labels: {},
  annotations: {},
  paused: false,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
  last_runs: [],
};

beforeEach(() => {
  vi.restoreAllMocks();
  clearApiKey();
});
afterEach(() => {
  vi.unstubAllGlobals();
});

describe("JobsPage trigger and pause gating", () => {
  beforeEach(() => {
    vi.spyOn(api, "getJobs").mockResolvedValue([
      { ...baseJob, id: "job-m", alias: "marketing-job", namespace: "marketing" },
      { ...baseJob, id: "job-f", alias: "finance-job", namespace: "finance" },
    ] as Job[]);
  });

  async function rowButtons(alias: string) {
    const rows = await screen.findAllByTestId("job-row");
    const row = rows.find((r) => r.textContent?.includes(alias));
    expect(row).toBeDefined();
    return {
      trigger: row!.querySelector<HTMLButtonElement>('button[aria-label="Trigger run"]')!,
      pause: row!.querySelector<HTMLButtonElement>('button[aria-label="Pause future runs"]')!,
    };
  }

  it("a runner in marketing triggers marketing but cannot pause or touch finance", async () => {
    await loginWithGrants({ marketing: "runner" });
    show(<JobsPage />);
    const marketing = await rowButtons("marketing-job");
    expect(marketing.trigger).toBeEnabled();
    expect(marketing.pause).toBeDisabled();
    expect(marketing.pause.title).toBe("Requires operator role in marketing");
    const finance = await rowButtons("finance-job");
    expect(finance.trigger).toBeDisabled();
    expect(finance.trigger.title).toBe("Requires runner role in finance");
  });

  it("an operator in marketing pauses marketing only", async () => {
    await loginWithGrants({ marketing: "operator" });
    show(<JobsPage />);
    expect((await rowButtons("marketing-job")).pause).toBeEnabled();
    expect((await rowButtons("finance-job")).pause).toBeDisabled();
  });

  it("a cluster viewer can neither trigger nor pause anywhere", async () => {
    await loginWithGrants({ "*": "viewer" }, "viewer");
    show(<JobsPage />);
    for (const alias of ["marketing-job", "finance-job"]) {
      const buttons = await rowButtons(alias);
      expect(buttons.trigger).toBeDisabled();
      expect(buttons.pause).toBeDisabled();
    }
  });

  it("a cluster operator keeps every action (wildcard grant)", async () => {
    await loginWithGrants({ "*": "operator" }, "operator");
    show(<JobsPage />);
    for (const alias of ["marketing-job", "finance-job"]) {
      const buttons = await rowButtons(alias);
      expect(buttons.trigger).toBeEnabled();
      expect(buttons.pause).toBeEnabled();
    }
  });

  it("auth disabled offers every action", async () => {
    show(<JobsPage />);
    const buttons = await rowButtons("finance-job");
    expect(buttons.trigger).toBeEnabled();
    expect(buttons.pause).toBeEnabled();
  });
});

describe("CacheView invalidation gating", () => {
  beforeEach(() => {
    vi.spyOn(api, "getJobCache").mockResolvedValue({
      entries: [
        {
          hash: "h",
          task_name: "t1",
          result: "ok",
          run_id: "run-1",
          task_run_id: "tr-1",
          created_at: new Date().toISOString(),
        },
      ],
    });
  });

  const job = { id: "job-1", alias: "c", namespace: "marketing" } as Job;

  it.each([
    ["runner", false],
    ["operator", true],
  ])("a %s in the job's namespace may invalidate: %s", async (role, allowed) => {
    await loginWithGrants({ marketing: role });
    show(<CacheView jobId="job-1" job={job} taskPoliciesAvailable={false} />);
    const all = await screen.findByRole("button", { name: "Invalidate All" });
    const task = await screen.findByRole("button", { name: "Invalidate Task" });
    if (allowed) {
      expect(all).toBeEnabled();
      expect(task).toBeEnabled();
    } else {
      expect(all).toBeDisabled();
      expect(task).toBeDisabled();
      expect(task.title).toBe("Requires operator role in marketing");
    }
  });

  it("an operator elsewhere may not invalidate", async () => {
    await loginWithGrants({ finance: "operator" });
    show(<CacheView jobId="job-1" job={job} taskPoliciesAvailable={false} />);
    expect(await screen.findByRole("button", { name: "Invalidate All" })).toBeDisabled();
  });
});

describe("BackfillsView cancel gating", () => {
  const running = (namespace?: string): Backfill => ({
    id: "bf-1",
    job_id: "job-1",
    namespace,
    status: "running",
    start: "2026-01-01T00:00:00Z",
    end: "2026-01-02T00:00:00Z",
    max_concurrent: 1,
    reprocess: "none",
    total_runs: 2,
    completed_runs: 0,
    failed_runs: 0,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  });

  it("needs the operator role in the job's AND the backfill's namespace", async () => {
    await loginWithGrants({ marketing: "operator" });
    vi.spyOn(api, "getBackfills").mockResolvedValue([running("finance")]);
    show(<BackfillsView jobId="job-1" jobNamespace="marketing" />);
    const cancel = await screen.findByRole("button", { name: /cancel/i });
    expect(cancel).toBeDisabled();
    expect(cancel.title).toBe("Requires operator role in finance");
  });

  it("allows an operator holding both namespaces", async () => {
    await loginWithGrants({ marketing: "operator" });
    vi.spyOn(api, "getBackfills").mockResolvedValue([running("marketing")]);
    show(<BackfillsView jobId="job-1" jobNamespace="marketing" />);
    await waitFor(async () => expect(await screen.findByRole("button", { name: /cancel/i })).toBeEnabled());
  });

  it("denies a runner", async () => {
    await loginWithGrants({ marketing: "runner" });
    vi.spyOn(api, "getBackfills").mockResolvedValue([running("marketing")]);
    show(<BackfillsView jobId="job-1" jobNamespace="marketing" />);
    expect(await screen.findByRole("button", { name: /cancel/i })).toBeDisabled();
  });
});
