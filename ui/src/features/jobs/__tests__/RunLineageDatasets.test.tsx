import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "@/lib/api";
import { RunLineageDatasets } from "../RunLineageDatasets";

const principal = vi.hoisted(() => ({ value: { isScoped: false, scopeKnown: true } }));

vi.mock("@/lib/auth", () => ({ usePrincipal: () => principal.value }));
vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, to, search, ...rest }: { children: ReactNode; to: string; search?: Record<string, string>; "data-testid"?: string }) => (
    <a data-testid={rest["data-testid"]} href={`${to}?${new URLSearchParams(search).toString()}`}>{children}</a>
  ),
}));

function renderPanel(runStatus?: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <RunLineageDatasets jobId="job-1" runId="run-1" runStatus={runStatus} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.restoreAllMocks();
  principal.value = { isScoped: false, scopeKnown: true };
});

describe("RunLineageDatasets", () => {
  it("lists a run's datasets with a link into the pre-filled impact view", async () => {
    vi.spyOn(api, "getRunDatasets").mockResolvedValue({
      run_id: "run-1",
      job_id: "job-1",
      datasets: [
        { namespace: "caesium", name: "j.extract.output", direction: "output", task_name: "extract", task_run_id: "tr-1", observed_at: "2026-10-21T00:00:00Z" },
      ],
    });
    renderPanel();
    const link = await screen.findByTestId("run-lineage-impact-link");
    expect(link.getAttribute("href")).toBe("/lineage?namespace=caesium&name=j.extract.output");
    expect(screen.getByTestId("run-lineage-dataset").textContent).toContain("j.extract.output");
  });

  it("renders nothing when the run persisted no lineage", async () => {
    const spy = vi.spyOn(api, "getRunDatasets").mockResolvedValue({ run_id: "run-1", job_id: "job-1", datasets: [] });
    const { container } = renderPanel();
    await waitFor(() => expect(spy).toHaveBeenCalled());
    expect(container.querySelector("[data-testid=run-lineage-datasets]")).toBeNull();
  });

  it("lists datasets but offers no impact link to a job-scoped key", async () => {
    principal.value = { isScoped: true, scopeKnown: true };
    vi.spyOn(api, "getRunDatasets").mockResolvedValue({
      run_id: "run-1",
      job_id: "job-1",
      datasets: [{ namespace: "caesium", name: "d", direction: "input", task_run_id: "tr-1", observed_at: "2026-10-21T00:00:00Z" }],
    });
    renderPanel();
    await screen.findByTestId("run-lineage-dataset");
    expect(screen.queryByTestId("run-lineage-impact-link")).toBeNull();
  });

  it("refetches while the run is live so an initially empty list becomes populated", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const spy = vi
        .spyOn(api, "getRunDatasets")
        .mockResolvedValueOnce({ run_id: "run-1", job_id: "job-1", datasets: [] })
        .mockResolvedValue({
          run_id: "run-1",
          job_id: "job-1",
          datasets: [{ namespace: "caesium", name: "late", direction: "output", task_run_id: "tr-1", observed_at: "2026-10-21T00:00:00Z" }],
        });
      renderPanel("running");
      await waitFor(() => expect(spy).toHaveBeenCalledTimes(1));
      expect(screen.queryByTestId("run-lineage-dataset")).toBeNull();
      await vi.advanceTimersByTimeAsync(3_500);
      await screen.findByTestId("run-lineage-dataset");
      expect(spy.mock.calls.length).toBeGreaterThanOrEqual(2);
    } finally {
      vi.useRealTimers();
    }
  });
});
