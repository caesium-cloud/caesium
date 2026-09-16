import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { Job } from "@/lib/api";
import { api } from "@/lib/api";
import { JobsPage } from "../JobsPage";

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children }: { children: ReactNode }) => <a href="#">{children}</a>,
  useNavigate: () => vi.fn(),
}));

vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));

vi.mock("@/lib/events", () => ({
  events: {
    isHealthy: () => true,
    subscribeConnection: vi.fn(),
    unsubscribeConnection: vi.fn(),
    subscribe: vi.fn(),
    unsubscribe: vi.fn(),
  },
}));

vi.mock("@/lib/api", async () => {
  const actual = await vi.importActual<typeof import("@/lib/api")>("@/lib/api");
  return {
    ...actual,
    api: {
      ...actual.api,
      getJobs: vi.fn(),
      triggerJob: vi.fn(),
      pauseJob: vi.fn(),
      unpauseJob: vi.fn(),
    },
  };
});

const sampleJob: Job = {
  id: "job-1",
  alias: "demo-pipeline",
  trigger_id: "trigger-1",
  labels: {},
  annotations: {},
  paused: false,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
  latest_run: {
    id: "run-1",
    job_id: "job-1",
    status: "succeeded",
    started_at: "2026-01-01T00:00:00Z",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  },
  last_runs: [{ status: "succeeded", duration: 1_000_000_000 }],
};

function renderPage() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <JobsPage />
    </QueryClientProvider>,
  );
}

describe("JobsPage accessibility", () => {
  beforeEach(() => {
    vi.mocked(api.getJobs).mockReset();
    vi.mocked(api.getJobs).mockResolvedValue([sampleJob]);
    window.history.replaceState(null, "", "/jobs");
  });

  it("gives the sort select an accessible name", async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByTestId("job-row")).toBeInTheDocument();
    });

    expect(screen.getByRole("combobox", { name: "Sort jobs" })).toBeInTheDocument();
    expect(screen.getByLabelText("Sort jobs")).toHaveAttribute("id", "jobs-sort");
  });

  it("names icon-only row actions", async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByTestId("job-row")).toBeInTheDocument();
    });

    expect(screen.getByRole("button", { name: "Trigger run" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Pause future runs" })).toBeInTheDocument();
  });
});
