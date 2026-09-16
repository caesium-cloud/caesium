import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { describe, expect, it, vi } from "vitest";
import type { Job } from "@/lib/api";
import { api } from "@/lib/api";
import { CacheView } from "../CacheView";

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children }: { children: React.ReactNode }) => <a>{children}</a>,
}));

vi.mock("@/lib/api", () => ({
  api: {
    getJobCache: vi.fn(),
    deleteJobCache: vi.fn(),
    deleteTaskCache: vi.fn(),
  },
}));

describe("CacheView", () => {
  it("shows a future cache expiry as a countdown", async () => {
    const now = new Date();
    vi.mocked(api.getJobCache).mockResolvedValue({
      entries: [{
        hash: "cache-hash",
        task_name: "scheduled",
        result: "ok",
        run_id: "run-1",
        task_run_id: "task-run-1",
        created_at: now.toISOString(),
        expires_at: new Date(now.getTime() + 12 * 60 * 60_000 + 5_000).toISOString(),
      }],
    });

    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <CacheView jobId="job-1" job={{ id: "job-1", alias: "cache-demo" } as Job} />
      </QueryClientProvider>,
    );

    expect(await screen.findByTestId("cache-expiry")).toHaveTextContent("in 13h");
  });
});
